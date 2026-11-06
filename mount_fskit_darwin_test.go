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
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/dsfaccini/fuse/internal/fusekernel"
	"github.com/ebitengine/purego"
)

func TestFSKitOSVersion(t *testing.T) {
	unsupported := FSKitMountError{Result: FSKitMountResultUnsupportedOSVersion}
	for _, test := range []struct {
		version string
		local   bool
		want    error
	}{
		{version: "15.3", local: true, want: unsupported},
		{version: "15.4", local: true},
		{version: "15.7", want: unsupported},
		{version: "26.0"},
		{version: "26.0", local: true},
		{version: "27.2"},
	} {
		err := validateFSKitOSVersion(test.version, test.local)
		if !errors.Is(err, test.want) {
			t.Errorf("validateFSKitOSVersion(%q, local=%v) error = %v, want %v", test.version, test.local, err, test.want)
		}
	}

	err := validateFSKitOSVersion("not-a-version", false)
	if err == nil || errors.As(err, new(FSKitMountError)) {
		t.Errorf("validateFSKitOSVersion(%q) error = %v, want a parse error", "not-a-version", err)
	}
}

// setFSKitTestSystem points FSKit mounts at frameworkPath and reports
// osVersion as the running macOS version until the test ends.
func setFSKitTestSystem(t *testing.T, frameworkPath string, osVersion string) {
	t.Helper()
	oldPath, oldVersion, oldLoader := fskitFrameworkPath, fskitOSProductVersion, fskitAPILoader
	fskitFrameworkPath = frameworkPath
	fskitOSProductVersion = func() (string, error) { return osVersion, nil }
	// The cached loader may hold the installed framework.
	fskitAPILoader = func() (*fskitAPI, error) { return loadFSKitAPI(frameworkPath) }
	t.Cleanup(func() { fskitFrameworkPath, fskitOSProductVersion, fskitAPILoader = oldPath, oldVersion, oldLoader })
}

func TestFSKitMountOptionsOmitOptionsFSKitDoesNotParse(t *testing.T) {
	options := fskitMountOptions(&MountConfig{Options: map[string]string{"backend": "fskit"}})
	for _, option := range strings.Split(options, ",") {
		name, _, _ := strings.Cut(option, "=")
		switch name {
		case "novncache", "noappledouble", "iosize", "backend":
			t.Errorf("FSKit mount options %q include %s", options, name)
		}
	}
}

func TestLoadFSKitAPIReportsMissingFramework(t *testing.T) {
	_, err := loadFSKitAPI(filepath.Join(t.TempDir(), "MFMount"))
	if !errors.Is(err, ErrFSKitUnavailable) {
		t.Fatalf("loadFSKitAPI error = %v, want ErrFSKitUnavailable", err)
	}
}

func TestLoadFSKitAPIResolvesInstalledFramework(t *testing.T) {
	if _, err := os.Stat(fskitFrameworkPath); err != nil {
		// CI installs macFUSE, so there a missing framework is a failure.
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatalf("macFUSE MFMount framework not installed: %v", err)
		}
		t.Skipf("macFUSE MFMount framework not installed: %v", err)
	}
	if _, err := loadFSKitAPI(fskitFrameworkPath); err != nil {
		t.Fatalf("loadFSKitAPI(%q): %v", fskitFrameworkPath, err)
	}
	// Mount loads MFMount once per process.
	first, err := fskitAPILoader()
	if err != nil {
		t.Fatalf("fskitAPILoader: %v", err)
	}
	if second, _ := fskitAPILoader(); second != first {
		t.Error("fskitAPILoader loaded MFMount again")
	}
}

func TestCallErrnoDropsGarbageUpperBits(t *testing.T) {
	// MFChannelCopyNextMessage reported this raw value for ENODEV when the
	// channel closed; without truncation the transport panicked on shutdown.
	if got := callErrno(0x6f18b00000000013); got != syscall.ENODEV {
		t.Fatalf("callErrno = %v, want ENODEV", got)
	}

	libc, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW)
	if err != nil {
		t.Fatal(err)
	}
	closeFn, err := purego.Dlsym(libc, "close")
	if err != nil {
		t.Fatal(err)
	}
	badFD := ^uintptr(0) // -1
	if _, _, raw := purego.SyscallN(closeFn, badFD); callErrno(raw) != syscall.EBADF {
		t.Fatalf("close(-1) errno = %v, want EBADF", callErrno(raw))
	}
}

func TestMountReturnsTypedFSKitErrors(t *testing.T) {
	// Every case uses a missing framework, so none can reach MFMount.
	missingFramework := filepath.Join(t.TempDir(), "MFMount")
	if _, err := loadFSKitAPI(missingFramework); !errors.Is(err, ErrFSKitUnavailable) {
		t.Fatalf("loadFSKitAPI(%q) error = %v, want ErrFSKitUnavailable", missingFramework, err)
	}
	config := &MountConfig{Options: map[string]string{"backend": "fskit"}}

	setFSKitTestSystem(t, missingFramework, "15.7")
	_, err := Mount(t.TempDir(), nil, config)
	var mountErr FSKitMountError
	if !errors.As(err, &mountErr) || mountErr.Result != FSKitMountResultUnsupportedOSVersion {
		t.Errorf("Mount on macOS 15.7 error = %v, want FSKitMountError with result %d", err, FSKitMountResultUnsupportedOSVersion)
	}

	setFSKitTestSystem(t, missingFramework, "26.0")
	if _, err := Mount(t.TempDir(), nil, config); !errors.Is(err, ErrFSKitUnavailable) {
		t.Errorf("Mount without MFMount error = %v, want ErrFSKitUnavailable", err)
	}
}

func TestFSKitTransportCloseWaitsForMountPublication(t *testing.T) {
	for _, test := range []struct {
		name      string
		succeeded bool
	}{
		{name: "native mount failed"},
		{name: "native mount succeeded", succeeded: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var closes, releases atomic.Int32
			closeErr := errors.New("native close error")
			transport := &fskitTransport{
				mounted: make(chan struct{}),
				closeNative: func() error {
					closes.Add(1)
					return closeErr
				},
				interruptNative: func() {},
				releaseNative:   func() { releases.Add(1) },
			}
			// The native call has returned, but its result is not published yet.
			results := make(chan error, 2)
			for range 2 {
				go func() { results <- transport.Close() }()
			}
			select {
			case err := <-results:
				t.Fatalf("Close returned before mount publication: %v", err)
			case <-time.After(10 * time.Millisecond):
			}
			transport.mu.Lock()
			closed := transport.closed
			transport.mu.Unlock()
			if closed || closes.Load() != 0 || releases.Load() != 0 {
				t.Error("Close blocked pending INIT traffic or touched the native channel before mount publication")
			}
			var mountErr error
			if !test.succeeded {
				mountErr = FSKitMountError{Result: FSKitMountResultUnexpectedFailure}
			}
			transport.setMountResult(mountErr)
			var wantErr error
			var wantCloses int32
			if test.succeeded {
				wantErr, wantCloses = closeErr, 1
			}
			for range 2 {
				select {
				case err := <-results:
					if !errors.Is(err, wantErr) {
						t.Errorf("Close error = %v, want %v", err, wantErr)
					}
				case <-time.After(time.Second):
					t.Fatal("Close did not finish after mount publication")
				}
			}
			if closes.Load() != wantCloses || releases.Load() != 1 {
				t.Fatalf("close calls=%d release calls=%d, want %d and 1", closes.Load(), releases.Load(), wantCloses)
			}
		})
	}
}

func TestFSKitTransportCloseDrainsOperationsBeforeRelease(t *testing.T) {
	closed := make(chan struct{})
	released := make(chan struct{})
	transport := &fskitTransport{
		mounted:        make(chan struct{}),
		mountSucceeded: true,
		closeNative:    func() error { close(closed); return nil },
		releaseNative:  func() { close(released) },
	}
	close(transport.mounted)
	transport.operations.Add(1)
	result := make(chan error, 1)
	go func() { result <- transport.Close() }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not close the successful mount's channel")
	}
	select {
	case <-released:
		t.Error("native channel released while an operation was active")
	default:
	}
	transport.operations.Done()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after its operation drained")
	}
	select {
	case <-released:
	default:
		t.Fatal("Close did not release the native channel")
	}
}

func TestFSKitTransportReadsMessages(t *testing.T) {
	f := newFakeMFMount(t)
	tr := f.transport(t)
	// FSKit hands over a WRITE's payload in a second body buffer.
	write := FakeRequest(
		fusekernel.InHeader{Opcode: fusekernel.OpWrite, Unique: 2},
		NativeBytes(&fusekernel.WriteIn{Size: 7}),
		[]byte("payload"))
	f.kernel.SendErrno(syscall.EINTR)
	f.kernel.Send(write...)
	f.kernel.SendErrno(syscall.EINVAL)
	f.kernel.Send(make([]byte, 8192))
	f.kernel.HangUp()

	dst := make([]byte, 4096)
	n, err := tr.ReadMessage(dst)
	if want := bytes.Join(write, nil); err != nil || !bytes.Equal(dst[:n], want) {
		t.Errorf("ReadMessage after EINTR = %d bytes, %v; want the %d bytes of both body buffers", n, err, len(want))
	}
	var pathErr *os.PathError
	if _, err := tr.ReadMessage(dst); !errors.As(err, &pathErr) || pathErr.Err != syscall.EINVAL {
		t.Errorf("ReadMessage on EINVAL = %v, want *os.PathError with EINVAL", err)
	}
	if _, err := tr.ReadMessage(dst); err == nil {
		t.Error("ReadMessage accepted a message larger than its buffer")
	}
	if _, err := tr.ReadMessage(dst); err != io.EOF {
		t.Errorf("ReadMessage after the hang-up = %v, want io.EOF", err)
	}
	// newFakeMFMount checks that every message was released.
}

func TestFSKitTransportSendsWholeMessages(t *testing.T) {
	f := newFakeMFMount(t)
	tr := f.transport(t)
	parts := [][]byte{[]byte("header"), {}, []byte("payload")}
	if err := tr.SendMessage(parts); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if got := string(f.kernel.NextReply(t)); got != "headerpayload" {
		t.Errorf("sent %q, want %q", got, "headerpayload")
	}

	f.sendResult = func(n int) (int, syscall.Errno) { return n - 1, 0 }
	if err := tr.SendMessage(parts); err == nil {
		t.Error("SendMessage accepted a short send")
	}
	// The kernel refuses a reply to an interrupted request with ENOENT, which
	// Reply treats as benign.
	f.sendResult = func(int) (int, syscall.Errno) { return -1, syscall.ENOENT }
	if err := tr.SendMessage(parts); !errors.Is(err, syscall.ENOENT) {
		t.Errorf("SendMessage error = %v, want ENOENT", err)
	}
}

func TestFSKitAPILoaderRetriesAFailedLoad(t *testing.T) {
	var loads int
	want := &fskitAPI{}
	loader := cacheFSKitAPI(func(string) (*fskitAPI, error) {
		loads++
		if loads == 1 {
			return nil, ErrFSKitUnavailable
		}
		return want, nil
	})
	if _, err := loader(); !errors.Is(err, ErrFSKitUnavailable) {
		t.Fatalf("first load error = %v, want ErrFSKitUnavailable", err)
	}
	for range 2 {
		if got, err := loader(); err != nil || got != want {
			t.Errorf("loader() = %p, %v; want %p, nil", got, err, want)
		}
	}
	if loads != 2 {
		t.Errorf("loads = %d, want 2: one failure, then one success reused", loads)
	}
}

// A warm transport reuses a finished send's iovecs instead of allocating.
func TestFSKitTransportReusesSendIovecs(t *testing.T) {
	f := newFakeMFMount(t)
	tr := f.transport(t)
	parts := [][]byte{[]byte("header"), []byte("payload")}
	send := func() *syscall.Iovec {
		t.Helper()
		if err := tr.SendMessage(parts); err != nil {
			t.Fatalf("SendMessage: %v", err)
		}
		f.kernel.NextReply(t)
		select {
		case vectors := <-tr.vectors:
			tr.vectors <- vectors
			return &vectors[:1][0]
		default:
			t.Fatal("the transport kept no iovecs after a send")
			return nil
		}
	}
	if first, second := send(), send(); first != second {
		t.Error("the second send allocated new iovecs")
	}
}

// Sends reuse iovecs, so concurrent sends must not mix.
func TestFSKitTransportSendsConcurrently(t *testing.T) {
	f := newFakeMFMount(t)
	tr := f.transport(t)
	const senders, sends = 8, 50
	var wg sync.WaitGroup
	for i := range senders {
		wg.Go(func() {
			parts := [][]byte{bytes.Repeat([]byte{'a' + byte(i)}, 16), bytes.Repeat([]byte{'A' + byte(i)}, 32)}
			for range sends {
				if err := tr.SendMessage(parts); err != nil {
					t.Errorf("SendMessage: %v", err)
					return
				}
			}
		})
	}
	// Read every reply, so that no sender blocks on a full channel and Close
	// can finish.
	for range senders * sends {
		reply, ok := f.kernel.nextReply()
		if !ok {
			t.Error("the transport sent no reply within 5s")
			break
		}
		i := reply[0] - 'a'
		if want := append(bytes.Repeat([]byte{'a' + i}, 16), bytes.Repeat([]byte{'A' + i}, 32)...); !bytes.Equal(reply, want) {
			t.Errorf("sent %q, want %q", reply, want)
		}
	}
	wg.Wait()
}

// drainServer answers every op with ENOSYS until the connection ends.
type drainServer struct{}

func (drainServer) ServeOps(c *Connection) {
	for {
		ctx, _, err := c.ReadOp()
		if err != nil {
			return
		}
		c.Reply(ctx, syscall.ENOSYS)
	}
}

func TestMountWrapsFSKitMountResult(t *testing.T) {
	for _, test := range []struct {
		name      string
		afterInit bool
		result    int32
	}{
		// newConnection fails to read INIT, and Mount returns the result.
		{name: "before INIT", result: FSKitMountResultFileSystemExtensionRequiresApproval},
		// The connection is up when MFMount returns the result.
		{name: "after INIT", afterInit: true, result: FSKitMountResultUnexpectedFailure},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFakeMFMount(t)
			f.mount = func() int32 {
				if test.afterInit && !f.answerInit() {
					t.Error("no INIT reply")
				}
				f.kernel.HangUp()
				return test.result
			}
			_, err := MountWithFakeMFMount(context.Background(), t, f, drainServer{}, MountConfig{})
			// artifact-fs finds the result with errors.As to decide whether
			// to fall back to the kernel extension.
			var mountErr FSKitMountError
			if !errors.As(err, &mountErr) || mountErr.Result != test.result {
				t.Errorf("Mount error = %v, want a wrapped FSKitMountError with result %d", err, test.result)
			}
			if err == nil || !strings.HasPrefix(err.Error(), "mount (background): ") {
				t.Errorf("Mount error = %v, want the prefix %q", err, "mount (background): ")
			}

			select {
			case <-f.released:
			case <-time.After(5 * time.Second):
				t.Fatal("the transport did not release the channel within 5s")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.channelCloses != 0 {
				t.Errorf("MFChannelClose calls = %d, want 0: MFMount closes the channel when it fails", f.channelCloses)
			}
			// As in macFUSE's libfuse, a failed MFMount unmounts nothing:
			// the mount point may be another volume.
			if len(f.unmounts) != 0 {
				t.Errorf("unmount calls = %v, want none", f.unmounts)
			}
		})
	}
}
