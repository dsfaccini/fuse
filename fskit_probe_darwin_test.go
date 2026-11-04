// Copyright 2026 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fuse

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/dsfaccini/fuse/fuseops"
	"github.com/dsfaccini/fuse/internal/buffer"
	"github.com/dsfaccini/fuse/internal/fusekernel"
	"github.com/ebitengine/purego"
	"golang.org/x/sys/unix"
)

// TestFSKitProbe documents the mounts and the measurements.

const (
	// probeWait bounds each wait on the volume, the server or a reader.
	probeWait = 5 * time.Second
	// probeMountWait bounds the INIT request and MFMount's return.
	probeMountWait = 30 * time.Second
	// probeMountLimit bounds the measurements on one mount.
	probeMountLimit = 3 * time.Minute
	// probeCleanupReserve is left before the go test deadline, which kills
	// the process without running cleanup.
	probeCleanupReserve = 3 * time.Minute
	// probeDaemonTimeout matches the timeout mount's daemon_timeout option.
	probeDaemonTimeout = 3 * time.Second
	// probeScratch holds any message FSKit sends, so that a WRITE larger than
	// the Connection's buffer does not end the mount.
	probeScratch = 8 << 20

	frameFromFSKit  = 'K'
	frameFromServer = 'S'
)

var errProbeTimeout = errors.New("timed out")

// probeResult is one machine-readable result line.
type probeResult struct {
	Item    string            `json:"item"`
	Mount   string            `json:"mount"`
	Result  string            `json:"result"`
	Details map[string]string `json:"details,omitempty"`
}

func reportProbe(t *testing.T, r probeResult) {
	t.Helper()
	line, err := json.Marshal(r)
	if err != nil {
		t.Errorf("encoding %s: %v", r.Item, err)
		return
	}
	t.Logf("FSKIT_PROBE %s", line)
}

type fskitProbe struct {
	t    *testing.T
	ctx  context.Context
	dirs []string

	// Findings that the results across mounts compare, by mount name.
	openNoPerm map[string]error
	maxWrite   map[string]int64
	volumes    map[string]unix.Statfs_t
}

func runFSKitProbe(t *testing.T) {
	plan := []struct {
		name      string
		configure func(*MountConfig)
		measure   func(*probeMount)
	}{
		{"base", nil, (*probeMount).measureBase},
		{"local", func(c *MountConfig) { c.Options["local"] = "" }, (*probeMount).measureLocal},
		{"options", func(c *MountConfig) {
			c.DisableDefaultPermissions = true
			c.MaxWrite = 128 << 10
			c.Options["max_write"] = "262144"
		}, (*probeMount).measureOptions},
		{"close", nil, (*probeMount).measureChannelClose},
		{"timeout", func(c *MountConfig) { c.Options["daemon_timeout"] = "3" }, (*probeMount).measureDaemonTimeout},
	}
	if len(plan) != fskitProbeMounts {
		t.Fatalf("the probe plans %d mounts, but fskitProbeMounts and the docs say %d", len(plan), fskitProbeMounts)
	}
	if lines, err := fskitMountLines(); err != nil || len(lines) > 0 {
		t.Fatalf("mount(8) lists FSKit volumes %q (error: %v); the probe mounts nothing while another FSKit volume is mounted", lines, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	t.Cleanup(stop)
	if deadline, ok := t.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline.Add(-probeCleanupReserve))
		t.Cleanup(cancel)
	}
	p := &fskitProbe{
		t:          t,
		ctx:        ctx,
		openNoPerm: map[string]error{},
		maxWrite:   map[string]int64{},
		volumes:    map[string]unix.Statfs_t{},
	}
	t.Cleanup(p.checkNoMountLeft)

	for _, step := range plan {
		t.Run(step.name, func(t *testing.T) {
			if err := ctx.Err(); err != nil {
				t.Fatalf("mount skipped: %v", err)
			}
			if m := p.mountProbe(t, step.name, step.configure); m != nil {
				step.measure(m)
				m.reportReplyBuffers()
			}
		})
	}
	p.reportAcrossMounts()
}

// checkNoMountLeft runs after every mount's own cleanup. MFMount can attach a
// volume after the probe stopped waiting for it, so each mount point is
// checked again.
func (p *fskitProbe) checkNoMountLeft() {
	var left []string
	for _, dir := range p.dirs {
		if _, mounted, err := mountedVolume(dir); mounted || err != nil {
			if _, gone := unmountVolume(dir); !gone {
				left = append(left, dir)
				continue
			}
		}
		os.Remove(dir)
	}
	lines, err := fskitMountLines()
	result := "none"
	if len(left) > 0 {
		result = strings.Join(left, ",")
		p.t.Errorf("the FSKit probe left %q mounted; run diskutil unmount force on each", left)
	}
	reportProbe(p.t, probeResult{Item: "mounts_left", Mount: "all", Result: result, Details: map[string]string{
		"mount_fskit_lines": strings.Join(lines, " | "),
		"mount_error":       errName(err),
	}})
}

func (p *fskitProbe) reportAcrossMounts() {
	report := func(item, mounts, result string, details map[string]string) {
		reportProbe(p.t, probeResult{Item: item, Mount: mounts, Result: result, Details: details})
	}

	denied := func(err error) bool { return errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) }
	base, baseOK := p.openNoPerm["base"]
	off, offOK := p.openNoPerm["options"]
	result := "unknown"
	switch {
	case !baseOK || !offOK || os.Geteuid() == 0:
	case denied(base) && off == nil:
		result = "yes"
	case denied(base) && denied(off):
		result = "always_enforced"
	case base == nil && off == nil:
		result = "no"
	}
	report("default_permissions", "base,options", result, map[string]string{
		"open_mode_0_with_option":    errName(base),
		"open_mode_0_without_option": errName(off),
	})

	const initLimit, optionLimit = 128 << 10, 256 << 10
	largest, limited := p.maxWrite["base"], p.maxWrite["options"]
	result = "unknown"
	switch {
	case largest == 0 || limited == 0:
	case largest <= initLimit:
		// FSKit's own WRITE size fits under both limits, so it tells nothing.
	case limited <= initLimit:
		result = "honors_init"
	case limited <= optionLimit:
		result = "honors_mount_option"
	default:
		result = "not_honored"
	}
	report("max_write", "base,options", result, map[string]string{
		"largest_write_base":    fmt.Sprint(largest),
		"largest_write_options": fmt.Sprint(limited),
		"init_max_write":        fmt.Sprint(initLimit),
		"mount_option":          fmt.Sprint(optionLimit),
	})

	// A volume without MNT_LOCAL is a network volume to macOS.
	plain, plainOK := p.volumes["base"]
	local, localOK := p.volumes["local"]
	result = "unknown"
	switch {
	case !plainOK || !localOK:
	case plain.Flags&unix.MNT_LOCAL != 0:
		// The volume is local without the option.
	case local.Flags&unix.MNT_LOCAL != 0:
		result = "yes"
	default:
		result = "no"
	}
	report("local_option_drops_network_volume", "base,local", result, map[string]string{
		"base_flags":  fmt.Sprintf("%#x", plain.Flags),
		"local_flags": fmt.Sprintf("%#x", local.Flags),
	})
}

// probeMount is one FSKit mount of the probe's in-memory file system.
type probeMount struct {
	p       *fskitProbe
	t       *testing.T
	name    string
	dir     string
	options map[string]string
	ctx     context.Context
	cancel  context.CancelFunc
	log     *probeLog
	server  *probeServer
	holds   map[string]*heldRead
	pt      *probeTransport
	conn    *Connection
	mfs     *MountedFileSystem
}

// mountProbe mounts the probe's file system with artifact-fs's mount config,
// changed by configure. It reports a failed mount, fails t and returns nil.
func (p *fskitProbe) mountProbe(t *testing.T, name string, configure func(*MountConfig)) *probeMount {
	dir, err := os.MkdirTemp("", "fskit-probe-"+name+"-")
	if err == nil {
		dir, err = filepath.EvalSymlinks(dir)
	}
	if err != nil {
		t.Fatal(err)
	}
	p.dirs = append(p.dirs, dir)
	ctx, cancel := context.WithTimeout(p.ctx, probeMountLimit)
	m := &probeMount{
		p:      p,
		t:      t,
		name:   name,
		dir:    dir,
		ctx:    ctx,
		cancel: cancel,
		log:    &probeLog{names: map[uint64]string{}, changed: make(chan struct{})},
		holds:  map[string]*heldRead{},
	}
	m.server = m.newServer()
	// Registered before MFMount, so it runs on every path out of the test.
	t.Cleanup(m.teardown)

	cfg := MountConfig{
		OpContext:               context.Background(),
		FSName:                  "fskit-probe-" + name,
		Subtype:                 "fskit-probe",
		DisableWritebackCaching: true,
		FuseImpl:                FUSEImplMacFUSE,
		Options:                 map[string]string{"backend": "fskit"},
	}
	if configure != nil {
		configure(&cfg)
	}
	m.options = cfg.toMap()
	delete(m.options, "backend")
	var options []string
	for _, key := range slices.Sorted(maps.Keys(m.options)) {
		if value := m.options[key]; value != "" {
			key += "=" + value
		}
		options = append(options, key)
	}

	start := time.Now()
	if err := m.startFSKit(cfg); err != nil {
		m.report("mount", "failed", map[string]string{"options": strings.Join(options, ","), "error": err.Error()})
		t.Errorf("mount %s: %v", name, err)
		return nil
	}
	m.report("mount", "mounted", map[string]string{
		"options": strings.Join(options, ","),
		"ms":      fmt.Sprint(time.Since(start).Milliseconds()),
	})
	request := m.pt.init
	m.report("init", fmt.Sprintf("%d.%d", request.Major, request.Minor), map[string]string{
		"max_readahead": fmt.Sprint(request.MaxReadahead),
		"flags":         fmt.Sprintf("%#x", request.Flags),
	})
	if st, mounted, err := mountedVolume(dir); mounted {
		p.volumes[name] = st
		m.report("volume", yesNo(st.Flags&unix.MNT_LOCAL != 0), map[string]string{
			"type":  unix.ByteSliceToString(st.Fstypename[:]),
			"from":  unix.ByteSliceToString(st.Mntfromname[:]),
			"flags": fmt.Sprintf("%#x", st.Flags),
		})
	} else {
		m.report("volume", "unknown", map[string]string{"error": errName(err)})
	}
	return m
}

// startFSKit mounts the way Mount does, through a probeTransport around the
// FSKit transport, and reports whether the volume is attached when MFMount
// returns.
func (m *probeMount) startFSKit(cfg MountConfig) error {
	getReplyBuffer, err := loadReplyBufferAPI()
	if err != nil {
		return err
	}
	rec := newProbeRecorder(m.t, m.name)

	fusekernel.IsPlatformFuseT = false
	ready := make(chan error, 1)
	transport, err := mountFSKitTransport(m.dir, &cfg, ready)
	if err != nil {
		return err
	}
	native, ok := transport.(*fskitTransport)
	if !ok {
		return fmt.Errorf("mountFSKitTransport returned %T", transport)
	}
	m.pt = &probeTransport{
		native:         native,
		getReplyBuffer: getReplyBuffer,
		scratch:        make([]byte, buffer.GetPageSize()+probeScratch),
		log:            m.log,
		rec:            rec,
	}

	type mountReturn struct {
		err      error
		attached bool
		replied  bool
	}
	returned := make(chan mountReturn, 1)
	go func() {
		err := <-ready
		_, attached, _ := mountedVolume(m.dir)
		returned <- mountReturn{err, attached, m.pt.replied.Load()}
	}()

	connected := make(chan error, 1)
	go func() {
		conn, err := newConnection(cfg, nil, nil, nil, m.pt)
		m.conn = conn
		connected <- err
	}()
	select {
	case err := <-connected:
		if err != nil {
			// As in Mount, MFMount's result explains a failed INIT best.
			select {
			case r := <-returned:
				if r.err != nil {
					return fmt.Errorf("mount (background): %w", r.err)
				}
			case <-time.After(probeMountWait):
			}
			return fmt.Errorf("newConnection: %w", err)
		}
	case <-time.After(probeMountWait):
		return fmt.Errorf("no INIT request within %v", probeMountWait)
	}

	m.mfs = &MountedFileSystem{dir: m.dir, joinStatusAvailable: make(chan struct{})}
	go func() {
		m.server.ServeOps(m.conn)
		m.mfs.joinStatus = m.conn.close()
		close(m.mfs.joinStatusAvailable)
	}()

	select {
	case r := <-returned:
		m.report("attached_at_mfmount_return", yesNo(r.attached), map[string]string{
			"init_replied_first": yesNo(r.replied),
		})
		if r.err != nil {
			return fmt.Errorf("mount (background): %w", r.err)
		}
	case <-time.After(probeMountWait):
		return fmt.Errorf("MFMount did not return within %v", probeMountWait)
	}
	return nil
}

// teardown releases held READs and unmounts whatever the measurements left
// mounted.
func (m *probeMount) teardown() {
	m.cancel()
	if _, mounted, err := mountedVolume(m.dir); mounted || err != nil {
		attempts, gone := unmountVolume(m.dir)
		m.report("teardown_unmount", yesNo(gone), map[string]string{"attempts": strings.Join(attempts, ",")})
	}
	if m.mfs != nil {
		m.join(probeWait)
	}
	if _, mounted, err := mountedVolume(m.dir); mounted || err != nil {
		m.t.Errorf("the FSKit probe left %s mounted (mount table error: %v); run: diskutil unmount force %s", m.dir, err, m.dir)
		return
	}
	os.Remove(m.dir)
}

func (m *probeMount) report(item, result string, details map[string]string) {
	m.t.Helper()
	reportProbe(m.t, probeResult{Item: item, Mount: m.name, Result: result, Details: details})
}

// run runs steps in order until the mount's context ends.
func (m *probeMount) run(steps ...func()) {
	for _, step := range steps {
		if err := m.ctx.Err(); err != nil {
			m.t.Errorf("measurements stopped early: %v", err)
			return
		}
		step()
	}
}

func (m *probeMount) measureBase() {
	m.run(
		m.measureRootAccess,
		func() { m.measureTTL("entry_ttl", "LOOKUP") },
		func() { m.measureTTL("attr_ttl", "GETATTR") },
		m.measureNegativeEntry,
		m.measureLookupAttributes,
		m.measureInterrupt,
		m.measure1231,
		func() { m.measurePaddingTiming("shrink", 6144) },
		func() { m.measurePaddingTiming("grow", 12288) },
		m.measurePermissions,
		m.measureWriteSizes,
		m.unmountAndJoin,
	)
}

func (m *probeMount) measureLocal() {
	m.run(m.measureRootAccess, m.unmountAndJoin)
}

func (m *probeMount) measureOptions() {
	m.run(m.measurePermissions, m.measureWriteSizes, m.measureNotify)
}

func (m *probeMount) measureChannelClose() {
	m.run(m.measureRootAccess, m.closeChannel)
}

func (m *probeMount) measureDaemonTimeout() {
	m.run(m.measureSlowRead, m.measureSignalledSlowRead)
}

func (m *probeMount) path(name string) string {
	return filepath.Join(m.dir, name)
}

func (m *probeMount) stat(name string) (int64, error) {
	return statSize(m.path(name))
}

func (m *probeMount) sleep(d time.Duration) {
	select {
	case <-time.After(d):
	case <-m.ctx.Done():
	}
}

// join waits up to d for Join and returns its result as text.
func (m *probeMount) join(d time.Duration) string {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	err := m.mfs.Join(ctx)
	switch {
	case ctx.Err() != nil && err == ctx.Err():
		return "pending"
	case err == nil:
		return "nil"
	}
	return err.Error()
}

// measureRootAccess stats the mount point. A process without the Network
// Volumes permission gets EPERM on a network volume.
func (m *probeMount) measureRootAccess() {
	_, err := statSize(m.dir)
	m.report("root_access", errName(err), nil)
}

// measureTTL stats a file whose TTL is 0 twice, and one whose TTL is 2s twice
// and again after the TTL. It counts the op each stat sends.
func (m *probeMount) measureTTL(item, op string) {
	var firstErr error
	stat := func(name string) int {
		if _, err := m.stat(name); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("stat %s: %w", name, err)
		}
		return m.log.count(op, name)
	}
	zero, timed := item+"_0", item+"_2s"
	zero1, zero2 := stat(zero), stat(zero)
	timed1, timed2 := stat(timed), stat(timed)
	m.sleep(3 * time.Second)
	timed3 := stat(timed)

	result := "no"
	switch {
	case firstErr != nil:
		result = "unknown"
	case zero2 > zero1 && timed2 == timed1 && timed3 > timed2:
		result = "yes"
	}
	m.report(item, result, map[string]string{
		"op":            op,
		"ttl_0_counts":  fmt.Sprintf("%d,%d", zero1, zero2),
		"ttl_2s_counts": fmt.Sprintf("%d,%d,%d", timed1, timed2, timed3),
		"error":         errName(firstErr),
	})
}

// measureNegativeEntry stats a name whose LOOKUP answers no child with an
// hour's entry TTL, twice, and a name whose LOOKUP fails with ENOENT, twice.
func (m *probeMount) measureNegativeEntry() {
	_, err1 := m.stat("negative")
	_, err2 := m.stat("negative")
	m.stat("missing")
	m.stat("missing")
	lookups := m.log.count("LOOKUP", "negative")

	result := "no"
	switch {
	case !errors.Is(err1, syscall.ENOENT) || !errors.Is(err2, syscall.ENOENT):
		result = "unknown"
	case lookups == 1:
		result = "yes"
	}
	m.report("negative_entry", result, map[string]string{
		"lookups":        fmt.Sprint(lookups),
		"stat_errors":    errName(err1) + "," + errName(err2),
		"enoent_lookups": fmt.Sprint(m.log.count("LOOKUP", "missing")),
	})
}

// measureLookupAttributes stats a file whose entry TTL is 0 and attr TTL an
// hour, changes its size, and stats it again. The second LOOKUP and any
// GETATTR report different sizes, so the stat shows which one FSKit used.
func (m *probeMount) measureLookupAttributes() {
	const name, lookupSize, getattrSize = "lookup_attrs", 200, 300
	n := m.server.names[name]
	first, err1 := m.stat(name)
	mark := m.log.mark()
	n.lookupSize.Store(lookupSize)
	n.getattrSize.Store(getattrSize)
	second, err2 := m.stat(name)
	lookups := len(m.log.matching(mark, isOp("LOOKUP", name)))

	result := "unknown"
	switch {
	case err1 != nil || err2 != nil || lookups == 0:
	case second == lookupSize:
		result = "yes"
	case second == first:
		result = "no"
	}
	m.report("lookup_replaces_attributes", result, map[string]string{
		"first_size":  fmt.Sprint(first),
		"second_size": fmt.Sprint(second),
		"lookups":     fmt.Sprint(lookups),
		"getattrs":    fmt.Sprint(len(m.log.matching(mark, isOp("GETATTR", name)))),
		"lookup_size": fmt.Sprint(lookupSize),
		"error":       errName(errors.Join(err1, err2)),
	})
}

// measureInterrupt holds a READ from cat, interrupts cat, and waits for
// FUSE_INTERRUPT.
func (m *probeMount) measureInterrupt() {
	const name = "interrupt"
	h := m.holds[name]
	defer h.answer(4096)
	reader, err := startReader(m.path(name))
	if err != nil {
		m.report("interrupt_sent", "unknown", map[string]string{"error": errName(err)})
		return
	}
	defer reader.stop()
	if _, ok := m.awaitRead(h); !ok {
		m.report("interrupt_sent", "unknown", map[string]string{"error": "no READ arrived"})
		return
	}
	mark := m.log.mark()
	start := time.Now()
	if err := reader.cmd.Process.Signal(syscall.SIGINT); err != nil {
		m.report("interrupt_sent", "unknown", map[string]string{"error": errName(err)})
		return
	}
	_, interrupted := m.log.waitFor(m.ctx, probeWait, mark, isOp("INTERRUPT", ""))
	exitWhileHeld := reader.wait(0)
	h.answer(4096)
	m.report("interrupt_sent", yesNo(interrupted), map[string]string{
		"interrupt_ms":    m.log.firstSince(mark, start, isOp("INTERRUPT", "")),
		"read_cancelled":  yesNo(len(m.log.matching(mark, isOp("READ_CANCELLED", name))) > 0),
		"exit_while_held": exitWhileHeld,
		"exit":            reader.wait(probeWait),
	})
}

// measure1231 reproduces macfuse#1231: a READ reply shorter than the size the
// file system reported. It fails once FSKit stops padding the reply.
func (m *probeMount) measure1231() {
	const name, reported, sent = "short_read", 8192, 100
	got, err := m.readOnce(name, reported)
	details := map[string]string{
		"reported_size": fmt.Sprint(reported),
		"reply_bytes":   fmt.Sprint(sent),
		"read_bytes":    fmt.Sprint(got),
		"error":         errName(err),
	}
	switch {
	case err != nil || got < sent:
		m.report("macfuse_1231", "unknown", details)
	case got == sent:
		m.report("macfuse_1231", "not_padded", details)
		m.t.Errorf("FSKit returned a %d-byte READ reply unpadded, so macfuse#1231 looks fixed: make this case expect no padding, and revisit the workarounds for it", sent)
	default:
		m.report("macfuse_1231", "padded", details)
	}
}

// measurePaddingTiming holds a READ, answers a GETATTR that reports a new
// size, then answers the READ short. The read returns the size FSKit held
// when it sent the READ, the size it held when the reply arrived, or only the
// reply.
func (m *probeMount) measurePaddingTiming(name string, after uint64) {
	const answered = 1024
	n, h := m.server.names[name], m.holds[name]
	defer h.answer(answered)
	before := n.getattrSize.Load()
	item := "read_padding_timing_" + name
	details := map[string]string{
		"size_at_read":    fmt.Sprint(before),
		"size_in_getattr": fmt.Sprint(after),
		"reply_bytes":     fmt.Sprint(answered),
	}
	f, err := openFile(m.path(name), os.O_RDONLY)
	if err != nil {
		details["error"] = errName(err)
		m.report(item, "unknown", details)
		return
	}
	defer within(probeWait, f.Close)

	type readResult struct {
		n   int
		err error
	}
	read := make(chan readResult, 1)
	go func() {
		k, err := f.Read(make([]byte, 64<<10))
		read <- readResult{k, err}
	}()
	request, ok := m.awaitRead(h)
	if !ok {
		details["error"] = "no READ arrived"
		m.report(item, "unknown", details)
		return
	}
	mark := m.log.mark()
	n.setSize(after)
	stat := make(chan string, 1)
	go func() {
		size, err := m.stat(name)
		stat <- fmt.Sprintf("%d,%s", size, errName(err))
	}()
	_, getattr := m.log.waitFor(m.ctx, probeWait, mark, isOp("GETATTR_SENT", name))
	h.answer(answered)
	var r readResult
	select {
	case r = <-read:
	case <-time.After(2 * probeWait):
		r.err = errProbeTimeout
	}
	details["read_request_bytes"] = fmt.Sprint(request.size)
	details["getattr_while_read_pending"] = yesNo(getattr)
	details["read_bytes"] = fmt.Sprint(r.n)
	details["read_error"] = errName(r.err)
	details["stat"] = <-stat

	sendTime, arrival := min(request.size, int64(before)), min(request.size, int64(after))
	result := "unknown"
	switch {
	case !getattr || r.err != nil || sendTime == arrival:
	case r.n == answered:
		result = "not_padded"
	case int64(r.n) == sendTime:
		result = "send_time"
	case int64(r.n) == arrival:
		result = "arrival_time"
	}
	m.report(item, result, details)
}

// measurePermissions opens a mode 0 file the probe's user owns.
func (m *probeMount) measurePermissions() {
	mark := m.log.mark()
	f, err := openFile(m.path("noperm"), os.O_RDONLY)
	if err == nil {
		within(probeWait, f.Close)
	}
	result := "allowed"
	switch {
	case errors.Is(err, errProbeTimeout):
		result = "unknown"
	case err != nil:
		result = "denied"
		m.p.openNoPerm[m.name] = err
	default:
		m.p.openNoPerm[m.name] = nil
	}
	_, option := m.options["default_permissions"]
	m.report("open_mode_0", result, map[string]string{
		"default_permissions": yesNo(option),
		"error":               errName(err),
		"open_reached_server": yesNo(len(m.log.matching(mark, isOp("OPEN", "noperm"))) > 0),
		"access_ops":          fmt.Sprint(len(m.log.matching(mark, isOp("ACCESS", "noperm")))),
	})
}

// measureWriteSizes writes 4 MiB with one write(2) and records the WRITE
// sizes FSKit sends.
func (m *probeMount) measureWriteSizes() {
	const name, size = "write", 4 << 20
	f, err := openFile(m.path(name), os.O_WRONLY)
	if err == nil {
		err = within(4*probeWait, func() error {
			_, err := f.Write(make([]byte, size))
			return err
		})
		if closeErr := within(4*probeWait, f.Close); err == nil {
			err = closeErr
		}
	}
	var writes, oversized int
	var largest int64
	for _, e := range m.log.matching(0, isOp("", name)) {
		switch e.op {
		case "WRITE":
			writes++
		case "OVERSIZE":
			oversized++
		default:
			continue
		}
		largest = max(largest, e.size)
	}
	m.p.maxWrite[m.name] = largest
	m.report("write_sizes", fmt.Sprint(largest), map[string]string{
		"writes":         fmt.Sprint(writes),
		"oversized":      fmt.Sprint(oversized),
		"write_bytes":    fmt.Sprint(size),
		"init_max_write": fmt.Sprint(m.conn.cfg.MaxWrite),
		"error":          errName(err),
	})
}

// measureNotify sends an entry and then an inode invalidation for cached
// files, and checks whether FSKit drops the cache and whether the channel
// still carries a LOOKUP.
func (m *probeMount) measureNotify() {
	const entry, inode = "notify_entry", "notify_inode"
	m.stat(entry)
	m.stat(entry)
	cached := m.log.count("LOOKUP", entry)
	sendErr := within(probeWait, func() error {
		return serviceEntryInval(m.conn, fuseops.RootInodeID, entry)
	})
	m.stat(entry)
	healthy := m.healthy("health_after_entry")
	m.report("notify_inval_entry", notifyResult(cached == 1, sendErr, m.log.count("LOOKUP", entry) > cached, healthy), map[string]string{
		"send_error": errName(sendErr),
		"lookups":    fmt.Sprint(m.log.count("LOOKUP", entry)),
		"healthy":    yesNo(healthy),
	})
	if !healthy {
		m.report("notify_inval_inode", "unknown", map[string]string{"error": "the entry notification broke the channel"})
		return
	}

	m.stat(inode)
	first := m.log.count("GETATTR", inode)
	m.stat(inode)
	second := m.log.count("GETATTR", inode)
	// A negative offset invalidates only the attributes.
	sendErr = within(probeWait, func() error {
		return serviceInodeInvalidation(m.conn, m.server.names[inode].id, -1, 0)
	})
	m.stat(inode)
	healthy = m.healthy("health_after_inode")
	m.report("notify_inval_inode", notifyResult(second == first, sendErr, m.log.count("GETATTR", inode) > second, healthy), map[string]string{
		"send_error": errName(sendErr),
		"getattrs":   fmt.Sprint(m.log.count("GETATTR", inode)),
		"healthy":    yesNo(healthy),
	})
}

// notifyResult names what a notification did to a cached item.
func notifyResult(cached bool, sendErr error, invalidated, healthy bool) string {
	switch {
	case !healthy:
		return "broke_channel"
	case sendErr != nil:
		return "error"
	case !cached:
		return "unknown"
	case invalidated:
		return "honored"
	}
	return "dropped"
}

// healthy reports whether a LOOKUP for a fresh name still reaches the server.
func (m *probeMount) healthy(name string) bool {
	_, err := m.stat(name)
	return errors.Is(err, syscall.ENOENT) && m.log.count("LOOKUP", name) > 0
}

// closeChannel calls MFChannelClose on the live mount.
func (m *probeMount) closeChannel() {
	mark := m.log.mark()
	m.pt.closedChannel.Store(true)
	native := m.pt.native
	closed, _, errno := purego.SyscallN(native.api.channelClose, native.channel)
	closeResult := "ok"
	if byte(closed) == 0 {
		closeResult = errName(callErrno(errno))
	}
	_, woke := m.log.waitFor(m.ctx, probeWait, mark, channelEnded)
	unmounted := waitUnmounted(m.dir, probeWait)
	_, accessErr := statSize(m.dir)
	result := "volume_stays_mounted"
	if unmounted {
		result = "unmounts"
	}
	m.report("channel_close", result, map[string]string{
		"close":       closeResult,
		"reader_woke": yesNo(woke),
		"access":      errName(accessErr),
		"join":        m.join(probeWait),
	})
}

// measureSlowRead holds a READ past daemon_timeout without signalling the
// reader.
func (m *probeMount) measureSlowRead() {
	const name, item = "slow_read", "daemon_timeout_slow_read"
	h := m.holds[name]
	defer h.answer(4096)
	f, err := openFile(m.path(name), os.O_RDONLY)
	if err != nil {
		m.report(item, "unknown", map[string]string{"error": errName(err)})
		return
	}
	defer within(probeWait, f.Close)
	read := make(chan error, 1)
	go func() {
		_, err := f.Read(make([]byte, 4096))
		read <- err
	}()
	if _, ok := m.awaitRead(h); !ok {
		m.report(item, "unknown", map[string]string{"error": "no READ arrived"})
		return
	}
	mark := m.log.mark()
	start := time.Now()
	var readErr error
	returned := true
	select {
	case readErr = <-read:
	case <-time.After(probeDaemonTimeout + probeWait):
		returned = false
	case <-m.ctx.Done():
		returned = false
	}
	readMS := time.Since(start).Milliseconds()
	h.answer(4096)
	ended := len(m.log.matching(mark, channelEnded)) > 0

	result := "unknown"
	switch {
	case ended:
		result = "disconnects"
	case returned && readErr != nil:
		result = "fails_read"
	case !returned:
		result = "no_effect"
	}
	_, mounted, _ := mountedVolume(m.dir)
	m.report(item, result, map[string]string{
		"read_returned_while_held": yesNo(returned),
		"read_ms":                  fmt.Sprint(readMS),
		"read_error":               errName(readErr),
		"interrupt_ms":             m.log.firstSince(mark, start, isOp("INTERRUPT", "")),
		"channel_end_ms":           m.log.firstSince(mark, start, channelEnded),
		"mounted":                  yesNo(mounted),
	})
}

// measureSignalledSlowRead holds a READ from cat, interrupts cat, and keeps
// ignoring the interrupt past daemon_timeout.
func (m *probeMount) measureSignalledSlowRead() {
	const name, item = "slow_read_signalled", "daemon_timeout_signalled_read"
	h := m.holds[name]
	defer h.answer(4096)
	if len(m.log.matching(0, channelEnded)) > 0 {
		m.report(item, "unknown", map[string]string{"error": "the slow read case ended the channel"})
		return
	}
	reader, err := startReader(m.path(name))
	if err != nil {
		m.report(item, "unknown", map[string]string{"error": errName(err)})
		return
	}
	defer reader.stop()
	if _, ok := m.awaitRead(h); !ok {
		m.report(item, "unknown", map[string]string{"error": "no READ arrived"})
		return
	}
	mark := m.log.mark()
	start := time.Now()
	if err := reader.cmd.Process.Signal(syscall.SIGINT); err != nil {
		m.report(item, "unknown", map[string]string{"error": errName(err)})
		return
	}
	_, ended := m.log.waitFor(m.ctx, probeDaemonTimeout+probeWait, mark, channelEnded)
	exitWhileHeld := reader.wait(0)
	h.answer(4096)

	result := "no_effect"
	switch {
	case ended:
		result = "disconnects"
	case exitWhileHeld != "running":
		result = "reader_released"
	}
	_, mounted, _ := mountedVolume(m.dir)
	m.report(item, result, map[string]string{
		"interrupt_ms":    m.log.firstSince(mark, start, isOp("INTERRUPT", "")),
		"channel_end_ms":  m.log.firstSince(mark, start, channelEnded),
		"exit_while_held": exitWhileHeld,
		"mounted":         yesNo(mounted),
	})
}

// unmountAndJoin unmounts the way a user does and reports what Join returns.
func (m *probeMount) unmountAndJoin() {
	attempts, gone := unmountVolume(m.dir)
	m.report("join_after_unmount", m.join(probeWait), map[string]string{
		"attempts":  strings.Join(attempts, ","),
		"unmounted": yesNo(gone),
	})
}

// reportReplyBuffers reports the reply buffers FSKit attached to READ
// requests.
func (m *probeMount) reportReplyBuffers() {
	var reads, attached, belowSize int
	var smallest, largest int64 = -1, 0
	for _, e := range m.log.matching(0, isOp("READ_REQUEST", "")) {
		reads++
		if e.capacity <= 0 {
			continue
		}
		attached++
		if smallest < 0 || e.capacity < smallest {
			smallest = e.capacity
		}
		largest = max(largest, e.capacity)
		if e.capacity < e.size {
			belowSize++
		}
	}
	result := "no"
	switch {
	case reads == 0:
		result = "unknown"
	case attached > 0:
		result = "yes"
	}
	m.report("read_reply_buffer", result, map[string]string{
		"reads":                   fmt.Sprint(reads),
		"with_buffer":             fmt.Sprint(attached),
		"min_capacity":            fmt.Sprint(smallest),
		"max_capacity":            fmt.Sprint(largest),
		"below_read_size":         fmt.Sprint(belowSize),
		"get_reply_buffer_errors": fmt.Sprint(len(m.log.matching(0, isOp("REPLY_BUFFER_ERROR", "")))),
	})
}

// newServer builds the probe's file system. Every mount serves the same files;
// each measurement uses its own, so that no cache state carries over.
func (m *probeMount) newServer() *probeServer {
	s := &probeServer{
		log:   m.log,
		uid:   uint32(os.Getuid()),
		gid:   uint32(os.Getgid()),
		nodes: map[fuseops.InodeID]*probeNode{},
		names: map[string]*probeNode{},
	}
	s.nodes[fuseops.RootInodeID] = &probeNode{id: fuseops.RootInodeID, name: "/", mode: os.ModeDir | 0o755}
	m.log.names[uint64(fuseops.RootInodeID)] = "/"
	file := func(name string, entryTTL, attrTTL time.Duration, size uint64) *probeNode {
		n := &probeNode{name: name, mode: 0o644, entryTTL: entryTTL, attrTTL: attrTTL}
		n.setSize(size)
		n.id = fuseops.InodeID(len(s.nodes) + 1)
		s.nodes[n.id], s.names[name], m.log.names[uint64(n.id)] = n, n, name
		return n
	}
	const hour = time.Hour

	// Spotlight skips a volume whose root holds this file.
	file(".metadata_never_index", hour, hour, 0)
	file("entry_ttl_0", 0, hour, 1)
	file("entry_ttl_2s", 2*time.Second, hour, 1)
	file("attr_ttl_0", hour, 0, 1)
	file("attr_ttl_2s", hour, 2*time.Second, 1)
	file("negative", hour, hour, 0).negative = true
	file("lookup_attrs", 0, hour, 100)
	file("short_read", hour, hour, 8192).read = func(_ context.Context, op *fuseops.ReadFileOp) error {
		serveFiller(op, 100)
		return nil
	}
	m.hold(file("interrupt", hour, hour, 4096), true)
	m.hold(file("shrink", hour, 0, 12288), false)
	m.hold(file("grow", hour, 0, 6144), false)
	file("noperm", hour, hour, 1).mode = 0
	file("write", hour, 0, 0)
	file("notify_entry", hour, hour, 1)
	file("notify_inode", hour, hour, 1)
	m.hold(file("slow_read", hour, hour, 4096), false)
	m.hold(file("slow_read_signalled", hour, hour, 4096), false)
	return s
}

// heldRead holds the first READ of a node until the probe answers it with a
// byte count. Later READs of the node get filler up to its GETATTR size.
type heldRead struct {
	taken   atomic.Bool
	arrived chan probeEvent
	release chan int64
}

// hold makes the first READ of n wait for the probe. With honorInterrupt, a
// FUSE_INTERRUPT for the READ answers it with EINTR.
func (m *probeMount) hold(n *probeNode, honorInterrupt bool) {
	h := &heldRead{arrived: make(chan probeEvent, 1), release: make(chan int64, 1)}
	m.holds[n.name] = h
	n.read = func(ctx context.Context, op *fuseops.ReadFileOp) error {
		if h.taken.Swap(true) {
			serveFiller(op, int64(n.getattrSize.Load()))
			return nil
		}
		h.arrived <- probeEvent{offset: op.Offset, size: op.Size}
		interrupted := ctx.Done()
		if !honorInterrupt {
			interrupted = nil
		}
		select {
		case k := <-h.release:
			serveBytes(op, k)
			return nil
		case <-interrupted:
			m.log.add(probeEvent{op: "READ_CANCELLED", name: n.name})
			return syscall.EINTR
		case <-m.ctx.Done():
			return syscall.EIO
		}
	}
}

// answer releases the held READ with n bytes. Only the first call counts.
func (h *heldRead) answer(n int64) {
	select {
	case h.release <- n:
	default:
	}
}

// awaitRead waits for the held READ of h and returns its offset and size.
func (m *probeMount) awaitRead(h *heldRead) (probeEvent, bool) {
	select {
	case e := <-h.arrived:
		return e, true
	case <-time.After(probeWait):
	case <-m.ctx.Done():
	}
	return probeEvent{}, false
}

// readOnce opens name and reads it with one read(2) of size bytes.
func (m *probeMount) readOnce(name string, size int) (int, error) {
	var n int
	err := within(probeWait, func() error {
		f, err := os.Open(m.path(name))
		if err != nil {
			return err
		}
		defer f.Close()
		n, err = f.Read(make([]byte, size))
		return err
	})
	if errors.Is(err, errProbeTimeout) {
		return 0, err
	}
	return n, err
}

type probeNode struct {
	id       fuseops.InodeID
	name     string
	mode     os.FileMode
	entryTTL time.Duration
	attrTTL  time.Duration
	// negative makes LOOKUP answer with no child, cached for entryTTL.
	negative bool
	// lookupSize and getattrSize are the sizes LOOKUP and GETATTR report.
	lookupSize  atomic.Uint64
	getattrSize atomic.Uint64
	// read answers READ. Without it, READ gets filler up to getattrSize.
	read func(context.Context, *fuseops.ReadFileOp) error
}

func (n *probeNode) setSize(size uint64) {
	n.lookupSize.Store(size)
	n.getattrSize.Store(size)
}

// probeEpoch anchors the probe's timestamps. A file's mtime follows its size,
// so that a size change also reads as a content change.
var probeEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// probeServer serves the probe's files at the root of the mount. Each op runs
// on its own goroutine, so a held READ blocks nothing else.
type probeServer struct {
	log   *probeLog
	uid   uint32
	gid   uint32
	nodes map[fuseops.InodeID]*probeNode
	names map[string]*probeNode
	ops   sync.WaitGroup
}

func (s *probeServer) ServeOps(c *Connection) {
	defer s.ops.Wait()
	// A panic here would end the process before any cleanup unmounts.
	defer func() {
		if r := recover(); r != nil {
			s.log.add(probeEvent{op: "SERVE_PANIC", name: fmt.Sprint(r)})
		}
	}()
	for {
		ctx, op, err := c.ReadOp()
		if err != nil {
			s.log.add(probeEvent{op: "SERVE_END", name: errName(err)})
			return
		}
		s.ops.Add(1)
		go s.serve(c, ctx, op)
	}
}

func (s *probeServer) serve(c *Connection, ctx context.Context, op interface{}) {
	defer s.ops.Done()
	defer func() {
		if r := recover(); r != nil {
			s.log.add(probeEvent{op: "SERVE_PANIC", name: fmt.Sprint(r)})
		}
	}()
	if err := c.Reply(ctx, s.handle(ctx, op)); err != nil {
		s.log.add(probeEvent{op: "REPLY_ERROR", name: err.Error()})
	}
	if o, ok := op.(*fuseops.GetInodeAttributesOp); ok {
		s.log.add(probeEvent{op: "GETATTR_SENT", name: s.log.names[uint64(o.Inode)]})
	}
}

func (s *probeServer) handle(ctx context.Context, op interface{}) error {
	now := time.Now()
	switch o := op.(type) {
	case *fuseops.StatFSOp:
		o.BlockSize, o.IoSize = 4096, 1<<20
		o.Blocks, o.BlocksFree, o.BlocksAvailable = 1<<20, 1<<19, 1<<19
		o.Inodes, o.InodesFree = 1<<10, 1<<9
	case *fuseops.LookUpInodeOp:
		s.log.add(probeEvent{op: "LOOKUP", name: o.Name})
		n := s.names[o.Name]
		if o.Parent != fuseops.RootInodeID || n == nil {
			return syscall.ENOENT
		}
		o.Entry.EntryExpiration = now.Add(n.entryTTL)
		if n.negative {
			return nil
		}
		o.Entry.Child = n.id
		o.Entry.Attributes = s.attributes(n, n.lookupSize.Load())
		o.Entry.AttributesExpiration = now.Add(n.attrTTL)
	case *fuseops.GetInodeAttributesOp:
		n := s.nodes[o.Inode]
		if n == nil {
			return syscall.ENOENT
		}
		s.log.add(probeEvent{op: "GETATTR", name: n.name})
		o.Attributes = s.attributes(n, n.getattrSize.Load())
		o.AttributesExpiration = now.Add(n.attrTTL)
	case *fuseops.SetInodeAttributesOp:
		n := s.nodes[o.Inode]
		if n == nil {
			return syscall.ENOENT
		}
		if o.Size != nil {
			n.setSize(*o.Size)
		}
		o.Attributes = s.attributes(n, n.getattrSize.Load())
		o.AttributesExpiration = now.Add(n.attrTTL)
	case *fuseops.OpenFileOp:
		s.log.add(probeEvent{op: "OPEN", name: s.log.names[uint64(o.Inode)]})
	case *fuseops.ReadFileOp:
		n := s.nodes[o.Inode]
		if n == nil {
			return syscall.ENOENT
		}
		s.log.add(probeEvent{op: "READ", name: n.name, offset: o.Offset, size: o.Size})
		if n.read != nil {
			return n.read(ctx, o)
		}
		serveFiller(o, int64(n.getattrSize.Load()))
	case *fuseops.WriteFileOp:
		n := s.nodes[o.Inode]
		if n == nil {
			return syscall.ENOENT
		}
		s.log.add(probeEvent{op: "WRITE", name: n.name, offset: o.Offset, size: int64(len(o.Data))})
		if end := uint64(o.Offset) + uint64(len(o.Data)); end > n.getattrSize.Load() {
			n.setSize(end)
		}
	case *fuseops.OpenDirOp, *fuseops.ReadDirOp, *fuseops.ReleaseDirHandleOp,
		*fuseops.FlushFileOp, *fuseops.ReleaseFileHandleOp, *fuseops.SyncFileOp,
		*fuseops.SyncFSOp, *fuseops.ForgetInodeOp, *fuseops.BatchForgetOp:
		// ReadDirOp answers an empty listing, which keeps indexers off the files.
	default:
		return syscall.ENOSYS
	}
	return nil
}

func (s *probeServer) attributes(n *probeNode, size uint64) fuseops.InodeAttributes {
	nlink := uint32(1)
	if n.mode.IsDir() {
		nlink = 2
	}
	mtime := probeEpoch.Add(time.Duration(size) * time.Second)
	return fuseops.InodeAttributes{
		Size:   size,
		Nlink:  nlink,
		Mode:   n.mode,
		Atime:  mtime,
		Mtime:  mtime,
		Ctime:  mtime,
		Crtime: probeEpoch,
		Uid:    s.uid,
		Gid:    s.gid,
	}
}

// serveBytes answers op with n filler bytes, at most the READ's size.
func serveBytes(op *fuseops.ReadFileOp, n int64) {
	payload := bytes.Repeat([]byte{'p'}, int(max(0, min(n, op.Size))))
	if op.Dst != nil {
		op.BytesRead = copy(op.Dst, payload)
		return
	}
	op.Data = [][]byte{payload}
	op.BytesRead = len(payload)
}

// serveFiller answers op with filler up to the end of a size-byte file.
func serveFiller(op *fuseops.ReadFileOp, size int64) {
	serveBytes(op, size-op.Offset)
}

// probeTransport wraps the FSKit transport of one probe mount. ReadMessage
// mirrors fskitTransport.ReadMessage and adds what the probe measures: the
// reply buffer FSKit attaches to each request, the requests the Connection
// handles without the server, and requests larger than the Connection's
// buffer, which it answers with EIO itself so that the mount survives them.
type probeTransport struct {
	native         *fskitTransport
	getReplyBuffer uintptr
	scratch        []byte
	log            *probeLog
	rec            *probeRecorder
	// init is the INIT request. It is set before the Connection sees it.
	init fusekernel.InitIn
	// replied turns true at the server's first message, the INIT reply.
	replied atomic.Bool
	// closedChannel is set when the probe calls MFChannelClose itself.
	closedChannel atomic.Bool
}

// loadReplyBufferAPI resolves MFMessageGetReplyBuffer, which the transport
// does not use.
func loadReplyBufferAPI() (uintptr, error) {
	lib, err := purego.Dlopen(fskitFrameworkPath, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrFSKitUnavailable, err)
	}
	return purego.Dlsym(lib, "MFMessageGetReplyBuffer")
}

func (pt *probeTransport) ReadMessage(p []byte) (int, error) {
	native := pt.native
	native.mu.Lock()
	if native.closed {
		native.mu.Unlock()
		return 0, io.EOF
	}
	native.operations.Add(1)
	channel := native.channel
	native.mu.Unlock()
	defer native.operations.Done()

	for {
		message, _, callErr := purego.SyscallN(native.api.channelCopyNextMessage, channel)
		if message == 0 {
			errno := callErrno(callErr)
			switch errno {
			case syscall.EINTR:
				continue
			case syscall.ENODEV:
				pt.log.add(probeEvent{op: "EOF"})
				return 0, io.EOF
			}
			pt.log.add(probeEvent{op: "CHANNEL_ERROR", name: errName(errno)})
			return 0, &os.PathError{Op: "MFChannelCopyNextMessage", Path: "macFUSE", Err: errno}
		}

		native.mu.Lock()
		closed := native.closed
		native.mu.Unlock()
		if closed {
			purego.SyscallN(native.api.release, message)
			return 0, io.EOF
		}

		var replyBuffer uintptr
		capacity, _, _ := purego.SyscallN(pt.getReplyBuffer, message, uintptr(unsafe.Pointer(&replyBuffer)))
		n, err := native.api.copyMessage(message, pt.scratch)
		purego.SyscallN(native.api.release, message)
		if err != nil {
			return 0, err
		}
		msg := pt.scratch[:n]
		pt.rec.frame(frameFromFSKit, msg)
		if n < fusekernel.InHeaderSize {
			return copy(p, msg), nil
		}
		pt.inspect(msg, int64(int(capacity)))
		if n <= len(p) {
			return copy(p, msg), nil
		}

		// The Connection cannot hold msg, so fail it here and keep the mount.
		size := int64(n)
		if binary.NativeEndian.Uint32(msg[4:8]) == fusekernel.OpWrite && n >= fusekernel.InHeaderSize+20 {
			// fuse_write_in: fh, offset, size.
			size = int64(binary.NativeEndian.Uint32(msg[fusekernel.InHeaderSize+16:]))
		}
		pt.log.add(probeEvent{op: "OVERSIZE", name: pt.log.names[binary.NativeEndian.Uint64(msg[16:24])], size: size})
		if err := pt.WriteMessage(errorReply(binary.NativeEndian.Uint64(msg[8:16]), syscall.EIO)); err != nil {
			return 0, err
		}
	}
}

// inspect logs what the probe measures about one request.
func (pt *probeTransport) inspect(msg []byte, replyCapacity int64) {
	opcode := binary.NativeEndian.Uint32(msg[4:8])
	name := pt.log.names[binary.NativeEndian.Uint64(msg[16:24])]
	body := msg[fusekernel.InHeaderSize:]
	if replyCapacity < 0 {
		pt.log.add(probeEvent{op: "REPLY_BUFFER_ERROR", name: name})
	}
	switch opcode {
	case fusekernel.OpInit:
		if len(body) >= fusekernel.InitInSize {
			pt.init = fusekernel.InitIn{
				Major:        binary.NativeEndian.Uint32(body[0:4]),
				Minor:        binary.NativeEndian.Uint32(body[4:8]),
				MaxReadahead: binary.NativeEndian.Uint32(body[8:12]),
				Flags:        binary.NativeEndian.Uint32(body[12:16]),
			}
		}
	case fusekernel.OpInterrupt:
		pt.log.add(probeEvent{op: "INTERRUPT"})
	case fusekernel.OpAccess:
		pt.log.add(probeEvent{op: "ACCESS", name: name})
	case fusekernel.OpRead:
		// fuse_read_in: fh, offset, size.
		if len(body) >= 20 {
			size := int64(binary.NativeEndian.Uint32(body[16:20]))
			pt.log.add(probeEvent{op: "READ_REQUEST", name: name, size: size, capacity: replyCapacity})
		}
	}
}

func (pt *probeTransport) WriteMessage(msg []byte) error {
	return pt.WritevMessage([][]byte{msg})
}

func (pt *probeTransport) WritevMessage(parts [][]byte) error {
	pt.replied.Store(true)
	if err := pt.native.WritevMessage(parts); err != nil {
		return err
	}
	pt.rec.frame(frameFromServer, parts...)
	return nil
}

func (pt *probeTransport) Close() error {
	if pt.closedChannel.Load() {
		// MFChannelClose must not run twice on one channel.
		pt.native.setMountResult(false)
	}
	return pt.native.Close()
}

var _ connectionTransport = (*probeTransport)(nil)

// errorReply builds the reply that fails request unique with errno.
func errorReply(unique uint64, errno syscall.Errno) []byte {
	reply := make([]byte, buffer.OutMessageHeaderSize)
	negative := -int32(errno)
	binary.NativeEndian.PutUint32(reply[0:4], uint32(len(reply)))
	binary.NativeEndian.PutUint32(reply[4:8], uint32(negative))
	binary.NativeEndian.PutUint64(reply[8:16], unique)
	return reply
}

type probeEvent struct {
	op       string
	name     string
	offset   int64
	size     int64
	capacity int64
	at       time.Time
}

// probeLog is the ordered record of what one mount's server and transport
// saw.
type probeLog struct {
	// names maps node IDs to file names. It is complete before the mount.
	names map[uint64]string

	mu      sync.Mutex
	events  []probeEvent
	changed chan struct{}
}

func (l *probeLog) add(e probeEvent) {
	e.at = time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
	close(l.changed)
	l.changed = make(chan struct{})
}

// mark returns the log's current position, for matching and waitFor.
func (l *probeLog) mark() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.events)
}

// matching returns the events after mark that match accepts.
func (l *probeLog) matching(mark int, match func(probeEvent) bool) []probeEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	var found []probeEvent
	for _, e := range l.events[mark:] {
		if match(e) {
			found = append(found, e)
		}
	}
	return found
}

func (l *probeLog) count(op, name string) int {
	return len(l.matching(0, isOp(op, name)))
}

// waitFor waits up to d for an event after mark that match accepts.
func (l *probeLog) waitFor(ctx context.Context, d time.Duration, mark int, match func(probeEvent) bool) (probeEvent, bool) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		l.mu.Lock()
		for _, e := range l.events[mark:] {
			if match(e) {
				l.mu.Unlock()
				return e, true
			}
		}
		changed := l.changed
		l.mu.Unlock()
		select {
		case <-changed:
		case <-timer.C:
			return probeEvent{}, false
		case <-ctx.Done():
			return probeEvent{}, false
		}
	}
}

// firstSince returns the milliseconds from start to the first event after
// mark that match accepts, or "none".
func (l *probeLog) firstSince(mark int, start time.Time, match func(probeEvent) bool) string {
	found := l.matching(mark, match)
	if len(found) == 0 {
		return "none"
	}
	return fmt.Sprint(found[0].at.Sub(start).Milliseconds())
}

// isOp matches events with op and name. An empty op or name matches any.
func isOp(op, name string) func(probeEvent) bool {
	return func(e probeEvent) bool {
		return (op == "" || e.op == op) && (name == "" || e.name == name)
	}
}

func channelEnded(e probeEvent) bool {
	return e.op == "EOF" || e.op == "CHANNEL_ERROR"
}

// probeReader is cat reading one file in its own process, so that the probe
// can signal a read without signalling the test.
type probeReader struct {
	cmd    *exec.Cmd
	exited chan struct{}
	err    error
}

func startReader(path string) (*probeReader, error) {
	r := &probeReader{cmd: exec.Command("/bin/cat", path), exited: make(chan struct{})}
	if err := r.cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		r.err = r.cmd.Wait()
		close(r.exited)
	}()
	return r, nil
}

// wait waits up to d for the reader to exit and describes its exit, or
// returns "running".
func (r *probeReader) wait(d time.Duration) string {
	select {
	case <-r.exited:
		return errText(r.err)
	default:
	}
	select {
	case <-r.exited:
		return errText(r.err)
	case <-time.After(d):
		return "running"
	}
}

func (r *probeReader) stop() {
	if r.wait(0) == "running" {
		r.cmd.Process.Kill()
		r.wait(probeWait)
	}
}

// within runs f on its own goroutine and waits up to d for it. A call into a
// wedged FSKit volume can block for good, so the probe never makes one
// unbounded. After errProbeTimeout, f may still be running, so the caller must
// not read what f writes.
func within(d time.Duration, f func() error) error {
	done := make(chan error, 1)
	go func() { done <- f() }()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return errProbeTimeout
	}
}

func statSize(path string) (int64, error) {
	var size int64
	err := within(probeWait, func() error {
		info, err := os.Stat(path)
		if err == nil {
			size = info.Size()
		}
		return err
	})
	if err != nil {
		return 0, err
	}
	return size, nil
}

func openFile(path string, flag int) (*os.File, error) {
	var f *os.File
	err := within(probeWait, func() (err error) {
		f, err = os.OpenFile(path, flag, 0)
		return err
	})
	if err != nil {
		return nil, err
	}
	return f, nil
}

// mountedVolume finds the volume mounted at dir in the mount table, without
// touching the volume.
func mountedVolume(dir string) (unix.Statfs_t, bool, error) {
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return unix.Statfs_t{}, false, err
	}
	stats := make([]unix.Statfs_t, n+16)
	if n, err = unix.Getfsstat(stats, unix.MNT_NOWAIT); err != nil {
		return unix.Statfs_t{}, false, err
	}
	for _, st := range stats[:n] {
		if unix.ByteSliceToString(st.Mntonname[:]) == dir {
			return st, true, nil
		}
	}
	return unix.Statfs_t{}, false, nil
}

func waitUnmounted(dir string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if _, mounted, err := mountedVolume(dir); err == nil && !mounted {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// unmountVolume unmounts the volume at dir, escalating from unmount(2) to
// diskutil and then to the forced form of each. It returns each attempt's
// result and whether the volume is gone.
func unmountVolume(dir string) ([]string, bool) {
	var attempts []string
	for _, step := range []struct {
		name string
		run  func() error
	}{
		{"unmount", func() error { return unix.Unmount(dir, 0) }},
		{"diskutil_unmount", func() error { return diskutil("unmount", dir) }},
		{"unmount_force", func() error { return unix.Unmount(dir, unix.MNT_FORCE) }},
		{"diskutil_unmount_force", func() error { return diskutil("unmount", "force", dir) }},
	} {
		err := within(2*probeWait, step.run)
		attempts = append(attempts, step.name+"="+errName(err))
		if waitUnmounted(dir, probeWait) {
			return attempts, true
		}
	}
	return attempts, false
}

func diskutil(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*probeWait)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/diskutil", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("diskutil %s: %w: %s", strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

// fskitMountLines returns the lines of mount(8) that mention FSKit, which is
// the check to run by hand after the probe.
func fskitMountLines() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeWait)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/sbin/mount").Output()
	var lines []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(strings.ToLower(line), "fskit") {
			lines = append(lines, line)
		}
	}
	return lines, err
}

// errName names err by its errno when it has one.
func errName(err error) string {
	if err == nil {
		return "none"
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		if name := unix.ErrnoName(errno); name != "" {
			return name
		}
	}
	return err.Error()
}

func errText(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
