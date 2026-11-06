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
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"unsafe"

	"github.com/dsfaccini/fuse/fuseops"
	"github.com/dsfaccini/fuse/internal/buffer"
	"github.com/dsfaccini/fuse/internal/fusekernel"
)

// paddingTestTransport is a testTransport that pads short reads, as FSKit
// does.
type paddingTestTransport struct {
	*testTransport
}

func (paddingTestTransport) padsShortReads() bool { return true }

func makeOpMessage(opcode uint32, unique, nodeid uint64, body ...[]byte) []byte {
	header := fusekernel.InHeader{Opcode: opcode, Unique: unique, Nodeid: nodeid}
	message := append([]byte(nil), structBytes(&header)...)
	for _, b := range body {
		message = append(message, b...)
	}
	binary.NativeEndian.PutUint32(message, uint32(len(message)))
	return message
}

// normalizedConfig fills in cfg's defaults, as Mount does before it creates
// the connection.
func normalizedConfig(t *testing.T, cfg MountConfig) MountConfig {
	t.Helper()
	normalized, err := cfg.normalize()
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return normalized
}

// opTestConnection completes Init over a testTransport, then drives ops
// through ReadOp and Reply as a file system server would.
type opTestConnection struct {
	t         *testing.T
	transport *testTransport
	c         *Connection
	unique    uint64
}

func newOpTestConnection(t *testing.T, padsShortReads bool) *opTestConnection {
	t.Helper()
	init := fusekernel.InitIn{Major: fusekernel.ProtoVersionMaxMajor, Minor: fusekernel.ProtoVersionMaxMinor}
	transport := &testTransport{messages: [][]byte{makeOpMessage(fusekernel.OpInit, 1, 0, structBytes(&init))}}
	var kernel connectionTransport = transport
	if padsShortReads {
		kernel = paddingTestTransport{transport}
	}
	// One page of write payload keeps each in-flight message small.
	cfg := normalizedConfig(t, MountConfig{MaxWrite: uint32(buffer.GetPageSize())})
	c, err := newConnection(cfg, nil, nil, nil, kernel)
	if err != nil {
		t.Fatalf("newConnection: %v", err)
	}
	return &opTestConnection{t: t, transport: transport, c: c, unique: 1}
}

// send queues one op and reads it as a file system server would.
func (o *opTestConnection) send(opcode uint32, nodeid uint64, body ...[]byte) (context.Context, interface{}) {
	o.t.Helper()
	o.unique++
	o.transport.messages = append(o.transport.messages, makeOpMessage(opcode, o.unique, nodeid, body...))
	ctx, op, err := o.c.ReadOp()
	if err != nil {
		o.t.Fatalf("ReadOp: %v", err)
	}
	return ctx, op
}

// reply replies to a sent op with opErr, and returns the reply's header, or
// nil for an op the kernel expects no reply to.
func (o *opTestConnection) reply(ctx context.Context, opErr error) *fusekernel.OutHeader {
	o.t.Helper()
	before := len(o.transport.written())
	if err := o.c.Reply(ctx, opErr); err != nil {
		o.t.Fatalf("Reply: %v", err)
	}
	writes := o.transport.written()
	if len(writes) == before {
		return nil
	}
	return (*fusekernel.OutHeader)(unsafe.Pointer(&writes[before][0]))
}

// do sends one op and replies with what serve returns after filling the op in.
func (o *opTestConnection) do(opcode uint32, nodeid uint64, serve func(op interface{}) error, body ...[]byte) *fusekernel.OutHeader {
	o.t.Helper()
	ctx, op := o.send(opcode, nodeid, body...)
	return o.reply(ctx, serve(op))
}

func (o *opTestConnection) lookUp(inode fuseops.InodeID, size uint64) {
	o.t.Helper()
	o.do(fusekernel.OpLookup, 1, func(op interface{}) error {
		setEntry(op, inode, size)
		return nil
	}, []byte("f\x00"))
}

// read answers a read of size bytes at offset with n bytes, and returns the
// reply's errno and data length.
func (o *opTestConnection) read(inode fuseops.InodeID, offset uint64, size uint32, n int) (syscall.Errno, int) {
	o.t.Helper()
	in := fusekernel.ReadIn{Offset: offset, Size: size}
	ctx, op := o.send(fusekernel.OpRead, uint64(inode), structBytes(&in))
	return o.answerRead(ctx, op, n)
}

func (o *opTestConnection) answerRead(ctx context.Context, op interface{}, n int) (syscall.Errno, int) {
	o.t.Helper()
	op.(*fuseops.ReadFileOp).BytesRead = n
	h := o.reply(ctx, nil)
	return syscall.Errno(-h.Error), int(h.Len) - buffer.OutMessageHeaderSize
}

func (o *opTestConnection) write(inode fuseops.InodeID, offset uint64, n int) {
	o.t.Helper()
	in := fusekernel.WriteIn{Offset: offset, Size: uint32(n)}
	o.do(fusekernel.OpWrite, uint64(inode), func(interface{}) error { return nil }, structBytes(&in), make([]byte, n))
}

func (o *opTestConnection) sendForget(inode fuseops.InodeID, n uint64) context.Context {
	o.t.Helper()
	in := fusekernel.ForgetIn{Nlookup: n}
	ctx, _ := o.send(fusekernel.OpForget, uint64(inode), structBytes(&in))
	return ctx
}

func (o *opTestConnection) forget(inode fuseops.InodeID, n uint64) {
	o.t.Helper()
	o.reply(o.sendForget(inode, n), nil)
}

func (o *opTestConnection) batchForget(inode fuseops.InodeID, n uint64) {
	o.t.Helper()
	count := fusekernel.BatchForgetCountIn{Count: 1}
	entry := fusekernel.BatchForgetEntryIn{Inode: int64(inode), Nlookup: n}
	o.do(fusekernel.OpBatchForget, 0, func(interface{}) error { return nil }, structBytes(&count), structBytes(&entry))
}

// setEntry fills in the ChildInodeEntry of an op whose reply carries one.
func setEntry(op interface{}, inode fuseops.InodeID, size uint64) {
	var e *fuseops.ChildInodeEntry
	switch o := op.(type) {
	case *fuseops.LookUpInodeOp:
		e = &o.Entry
	case *fuseops.MkDirOp:
		e = &o.Entry
	case *fuseops.MkNodeOp:
		e = &o.Entry
	case *fuseops.CreateFileOp:
		e = &o.Entry
	case *fuseops.CreateSymlinkOp:
		e = &o.Entry
	case *fuseops.CreateLinkOp:
		e = &o.Entry
	default:
		panic("no ChildInodeEntry")
	}
	e.Child = inode
	e.Attributes.Size = size
}

func (o *opTestConnection) readDirPlus(entries ...[]byte) {
	o.t.Helper()
	in := fusekernel.ReadIn{Size: 4096}
	o.do(fusekernel.OpReaddirplus, 1, func(op interface{}) error {
		dirplus := op.(*fuseops.ReadDirPlusOp)
		for _, e := range entries {
			dirplus.BytesRead += copy(dirplus.Dst[dirplus.BytesRead:], e)
		}
		return nil
	}, structBytes(&in))
}

func direntPlus(inode fuseops.InodeID, size uint64, name string) []byte {
	entry := fusekernel.EntryOut{Nodeid: uint64(inode), Attr: fusekernel.Attr{Ino: uint64(inode), Size: size}}
	dirent := fusekernel.Dirent{Ino: uint64(inode), Namelen: uint32(len(name))}
	b := append(append(append([]byte(nil), structBytes(&entry)...), structBytes(&dirent)[:fusekernel.DirentSize]...), name...)
	return append(b, make([]byte, (8-len(b)%8)%8)...)
}

func TestShortReadOnPaddingTransport(t *testing.T) {
	tests := []struct {
		name      string
		offset    uint64
		size      uint32
		n         int
		wantErrno syscall.Errno
		wantLen   int
	}{
		{"short of the reported size", 0, 512, 46, syscall.EIO, 0},
		{"stops at the reported size", 0, 512, 200, 0, 200},
		{"short of the read size", 0, 100, 99, syscall.EIO, 0},
		{"whole read inside the size", 0, 100, 100, 0, 100},
		{"tail stops at the reported size", 150, 100, 50, 0, 50},
		{"tail short of the reported size", 150, 100, 49, syscall.EIO, 0},
		{"at the reported size", 200, 512, 0, 0, 0},
		{"past the reported size", 300, 512, 0, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := newOpTestConnection(t, true)
			o.lookUp(2, 200)
			errno, n := o.read(2, tt.offset, tt.size, tt.n)
			if errno != tt.wantErrno || n != tt.wantLen {
				t.Errorf("read reply = errno %d with %d bytes, want errno %d with %d bytes", errno, n, tt.wantErrno, tt.wantLen)
			}
		})
	}
}

func TestShortReadWithoutReportedSize(t *testing.T) {
	o := newOpTestConnection(t, true)
	if errno, n := o.read(2, 0, 512, 46); errno != 0 || n != 46 {
		t.Errorf("read reply = errno %d with %d bytes, want 46 bytes", errno, n)
	}
}

func TestShortReadOnOtherTransports(t *testing.T) {
	if _, ok := newFileTransport(nil).(shortReadPadder); ok {
		t.Error("fileTransport pads short reads")
	}

	o := newOpTestConnection(t, false)
	if o.c.reportedInodes != nil {
		t.Error("connection records sizes on a transport that does not pad short reads")
	}
	o.lookUp(2, 200)
	if errno, n := o.read(2, 0, 512, 46); errno != 0 || n != 46 {
		t.Errorf("read reply = errno %d with %d bytes, want 46 bytes", errno, n)
	}
}

// sizeReporter is an op whose reply reports a size for inode 2.
type sizeReporter struct {
	name   string
	opcode uint32
	body   [][]byte
	lookup bool // whether the reply gives the kernel a lookup
	serve  func(size uint64) func(op interface{}) error
}

func sizeReporters() []sizeReporter {
	var (
		setattr fusekernel.SetattrIn
		mkdir   fusekernel.MkdirIn
		mknod   fusekernel.MknodIn
		create  fusekernel.CreateIn
		link    = fusekernel.LinkIn{Oldnodeid: 2}
		dirplus = fusekernel.ReadIn{Size: 4096}
	)
	entry := func(size uint64) func(op interface{}) error {
		return func(op interface{}) error {
			setEntry(op, 2, size)
			return nil
		}
	}
	return []sizeReporter{
		{"lookup", fusekernel.OpLookup, [][]byte{[]byte("f\x00")}, true, entry},
		{"mkdir", fusekernel.OpMkdir, [][]byte{structBytes(&mkdir), []byte("d\x00")}, true, entry},
		{"mknod", fusekernel.OpMknod, [][]byte{structBytes(&mknod), []byte("n\x00")}, true, entry},
		{"create", fusekernel.OpCreate, [][]byte{structBytes(&create), []byte("c\x00")}, true, entry},
		{"symlink", fusekernel.OpSymlink, [][]byte{[]byte("s\x00t\x00")}, true, entry},
		{"link", fusekernel.OpLink, [][]byte{structBytes(&link), []byte("l\x00")}, true, entry},
		{"readdirplus", fusekernel.OpReaddirplus, [][]byte{structBytes(&dirplus)}, true, func(size uint64) func(op interface{}) error {
			return func(op interface{}) error {
				o := op.(*fuseops.ReadDirPlusOp)
				o.BytesRead = copy(o.Dst, direntPlus(2, size, "f"))
				return nil
			}
		}},
		{"getattr", fusekernel.OpGetattr, nil, false, getattrReply},
		{"setattr", fusekernel.OpSetattr, [][]byte{structBytes(&setattr)}, false, func(size uint64) func(op interface{}) error {
			return func(op interface{}) error {
				op.(*fuseops.SetInodeAttributesOp).Attributes.Size = size
				return nil
			}
		}},
	}
}

func getattrReply(size uint64) func(op interface{}) error {
	return func(op interface{}) error {
		op.(*fuseops.GetInodeAttributesOp).Attributes.Size = size
		return nil
	}
}

// An entry reply records the inode and gives the kernel one lookup.
func TestReportedSizeFromEveryEntryReply(t *testing.T) {
	for _, r := range sizeReporters() {
		if !r.lookup {
			continue
		}
		t.Run(r.name+" of a new inode", func(t *testing.T) {
			o := newOpTestConnection(t, true)
			o.do(r.opcode, 2, r.serve(100), r.body...)
			if errno, n := o.read(2, 0, 512, 100); errno != 0 || n != 100 {
				t.Errorf("read reply = errno %d with %d bytes, want 100 bytes", errno, n)
			}
			if errno, _ := o.read(2, 0, 512, 99); errno != syscall.EIO {
				t.Errorf("short read reply = errno %d, want EIO", errno)
			}
			o.forget(2, 1)
			if errno, n := o.read(2, 0, 512, 46); errno != 0 || n != 46 {
				t.Errorf("read after forgetting one lookup = errno %d with %d bytes, want 46 bytes", errno, n)
			}
		})
		t.Run(r.name+" shrinking a held inode", func(t *testing.T) {
			o := newOpTestConnection(t, true)
			o.lookUp(2, 200)
			o.do(r.opcode, 2, r.serve(100), r.body...)
			if errno, n := o.read(2, 0, 512, 100); errno != 0 || n != 100 {
				t.Errorf("read reply = errno %d with %d bytes, want 100 bytes", errno, n)
			}
			o.forget(2, 1)
			if errno, _ := o.read(2, 0, 512, 99); errno != syscall.EIO {
				t.Errorf("short read after forgetting one of two lookups = errno %d, want EIO", errno)
			}
		})
	}
}

// An attributes reply updates an inode the kernel holds, leaves its lookup
// count alone, and creates no record.
func TestReportedSizeFromEveryAttributesReply(t *testing.T) {
	for _, r := range sizeReporters() {
		if r.lookup {
			continue
		}
		for _, size := range []uint64{100, 300} {
			t.Run(fmt.Sprintf("%s reporting %d for a held inode", r.name, size), func(t *testing.T) {
				o := newOpTestConnection(t, true)
				o.lookUp(2, 200)
				o.lookUp(2, 200)
				o.do(r.opcode, 2, r.serve(size), r.body...)
				if errno, n := o.read(2, 0, 512, int(size)); errno != 0 || n != int(size) {
					t.Errorf("read reply = errno %d with %d bytes, want %d bytes", errno, n, size)
				}
				if errno, _ := o.read(2, 0, 512, int(size)-1); errno != syscall.EIO {
					t.Errorf("short read reply = errno %d, want EIO", errno)
				}
				o.forget(2, 1)
				if errno, _ := o.read(2, 0, 512, int(size)-1); errno != syscall.EIO {
					t.Errorf("short read after forgetting one of two lookups = errno %d, want EIO", errno)
				}
				o.forget(2, 1)
				if errno, n := o.read(2, 0, 512, 46); errno != 0 || n != 46 {
					t.Errorf("read after forgetting both lookups = errno %d with %d bytes, want 46 bytes", errno, n)
				}
			})
		}
		t.Run(r.name+" of an inode no entry reply reported", func(t *testing.T) {
			o := newOpTestConnection(t, true)
			o.do(r.opcode, 2, r.serve(200), r.body...)
			if errno, n := o.read(2, 0, 512, 46); errno != 0 || n != 46 {
				t.Errorf("read reply = errno %d with %d bytes, want 46 bytes", errno, n)
			}
		})
	}
}

// hookTransport calls before and after, once each, around the next reply it
// writes, while the Reply that writes it waits.
type hookTransport struct {
	*testTransport
	pads          bool
	mu            sync.Mutex
	before, after func()
}

func (h *hookTransport) padsShortReads() bool { return h.pads }

func (h *hookTransport) SendMessage(parts [][]byte) error {
	h.mu.Lock()
	before := h.before
	h.before = nil
	h.mu.Unlock()
	if before != nil {
		before()
	}

	err := h.testTransport.SendMessage(parts)

	h.mu.Lock()
	after := h.after
	h.after = nil
	h.mu.Unlock()
	if after != nil {
		after()
	}
	return err
}

func newHookTestConnection(t *testing.T, padsShortReads bool) (*opTestConnection, *hookTransport) {
	t.Helper()
	init := fusekernel.InitIn{Major: fusekernel.ProtoVersionMaxMajor, Minor: fusekernel.ProtoVersionMaxMinor}
	transport := &testTransport{messages: [][]byte{makeOpMessage(fusekernel.OpInit, 1, 0, structBytes(&init))}}
	hooks := &hookTransport{testTransport: transport, pads: padsShortReads}
	c, err := newConnection(normalizedConfig(t, MountConfig{}), nil, nil, nil, hooks)
	if err != nil {
		t.Fatalf("newConnection: %v", err)
	}
	return &opTestConnection{t: t, transport: transport, c: c, unique: 1}, hooks
}

// replyDuring replies to ctx from inside another reply's write. A reply that
// changes size records waits for the other to finish, because the other
// holds sizeReplyMu. Only when nothing holds it does replyDuring wait for the
// reply, so that the reply lands inside the other's window.
func (o *opTestConnection) replyDuring(ctx context.Context, wg *sync.WaitGroup) {
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		if err := o.c.Reply(ctx, nil); err != nil {
			o.t.Errorf("Reply: %v", err)
		}
	}()
	if o.c.sizeReplyMu.TryLock() {
		o.c.sizeReplyMu.Unlock()
		<-done
	}
}

// The kernel has a reply's shrink before Reply applies it, and can grow the
// file again in that time. The growth must survive the shrink.
func TestShrinkKeepsLaterGrowth(t *testing.T) {
	for _, r := range sizeReporters() {
		t.Run(r.name, func(t *testing.T) {
			o, hooks := newHookTestConnection(t, true)
			o.lookUp(2, 200)
			var wg sync.WaitGroup
			hooks.after = func() {
				ctx, op := o.send(fusekernel.OpGetattr, 2)
				getattrReply(300)(op)
				o.replyDuring(ctx, &wg)
			}
			o.do(r.opcode, 2, r.serve(100), r.body...)
			wg.Wait()
			if errno, n := o.read(2, 0, 512, 299); errno != syscall.EIO {
				t.Errorf("read reply = errno %d with %d bytes, want EIO: the kernel's last size is 300", errno, n)
			}
		})
	}
}

// The kernel has a truncation before Reply applies it, and can write past
// the new size in that time.
func TestShrinkKeepsLaterWrite(t *testing.T) {
	var setattr fusekernel.SetattrIn
	o, hooks := newHookTestConnection(t, true)
	o.lookUp(2, 200)
	var wg sync.WaitGroup
	hooks.after = func() {
		in := fusekernel.WriteIn{Offset: 100, Size: 50}
		ctx, _ := o.send(fusekernel.OpWrite, 2, structBytes(&in), make([]byte, 50))
		o.replyDuring(ctx, &wg)
	}
	o.do(fusekernel.OpSetattr, 2, func(op interface{}) error {
		op.(*fuseops.SetInodeAttributesOp).Attributes.Size = 100
		return nil
	}, structBytes(&setattr))
	wg.Wait()
	if errno, n := o.read(2, 0, 512, 120); errno != syscall.EIO {
		t.Errorf("read reply = errno %d with %d bytes, want EIO: the kernel's size is 150 after its write", errno, n)
	}
}

// Two replies overlap: one is recorded first, and the other is answered
// before it is written. The record must match the reply the kernel got last.
func TestOverlappingRepliesRecordedInWriteOrder(t *testing.T) {
	o, hooks := newHookTestConnection(t, true)
	o.lookUp(2, 200)
	sizes := map[uint64]uint64{}

	first, op := o.send(fusekernel.OpGetattr, 2)
	getattrReply(300)(op)
	sizes[o.unique] = 300
	second, op := o.send(fusekernel.OpGetattr, 2)
	getattrReply(100)(op)
	sizes[o.unique] = 100

	var wg sync.WaitGroup
	hooks.before = func() { o.replyDuring(second, &wg) }
	if err := o.c.Reply(first, nil); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	wg.Wait()

	var last uint64
	for _, w := range o.transport.written() {
		if unique := (*fusekernel.OutHeader)(unsafe.Pointer(&w[0])).Unique; sizes[unique] != 0 {
			last = sizes[unique]
		}
	}
	if got := o.c.reportedSize(2); got != last {
		t.Errorf("recorded size = %d, want %d from the reply the kernel got last", got, last)
	}
}

// sizeGrowth is a reply that grows inode 2 to 300.
type sizeGrowth struct {
	name   string
	held   bool // whether inode 2 is held at 200 first
	opcode uint32
	body   [][]byte
	serve  func(op interface{}) error
}

func sizeGrowths() []sizeGrowth {
	write := fusekernel.WriteIn{Offset: 200, Size: 100}
	growths := []sizeGrowth{{"write", true, fusekernel.OpWrite, [][]byte{structBytes(&write), make([]byte, 100)}, func(interface{}) error { return nil }}}
	for _, r := range sizeReporters() {
		growths = append(growths, sizeGrowth{r.name + " of a held inode", true, r.opcode, r.body, r.serve(300)})
		if r.lookup {
			growths = append(growths, sizeGrowth{r.name + " of a new inode", false, r.opcode, r.body, r.serve(300)})
		}
	}
	return growths
}

func arrivalSize(ctx context.Context) uint64 {
	return ctx.Value(contextKey).(opState).sizeAtRead
}

// The kernel can read a file as soon as it has a reply that grows it.
func TestGrowthRecordedBeforeWrite(t *testing.T) {
	for _, g := range sizeGrowths() {
		t.Run(g.name, func(t *testing.T) {
			o, hooks := newHookTestConnection(t, true)
			if g.held {
				o.lookUp(2, 200)
			}
			// A READ reply waits for the growing reply, so only the READ
			// arrives inside its write.
			var read context.Context
			var op interface{}
			hooks.after = func() {
				in := fusekernel.ReadIn{Size: 512}
				read, op = o.send(fusekernel.OpRead, 2, structBytes(&in))
			}
			o.do(g.opcode, 2, g.serve, g.body...)
			if got := arrivalSize(read); got != 300 {
				t.Errorf("size when the read arrived = %d, want 300", got)
			}
			if errno, n := o.answerRead(read, op, 250); errno != syscall.EIO {
				t.Errorf("read reply = errno %d with %d bytes, want EIO", errno, n)
			}
		})
	}
}

// A growth answered while a READ reply is being written reaches the kernel
// after that reply.
func TestReadReplyWrittenBeforeLaterGrowth(t *testing.T) {
	o, hooks := newHookTestConnection(t, true)
	o.lookUp(2, 200)
	in := fusekernel.ReadIn{Size: 512}
	read, op := o.send(fusekernel.OpRead, 2, structBytes(&in))
	readUnique := o.unique
	getattr, attrOp := o.send(fusekernel.OpGetattr, 2)
	getattrReply(300)(attrOp)
	getattrUnique := o.unique

	var wg sync.WaitGroup
	hooks.before = func() { o.replyDuring(getattr, &wg) }
	op.(*fuseops.ReadFileOp).BytesRead = 250
	if err := o.c.Reply(read, nil); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	wg.Wait()

	grown := false
	for _, w := range o.transport.written() {
		h := (*fusekernel.OutHeader)(unsafe.Pointer(&w[0]))
		switch h.Unique {
		case getattrUnique:
			grown = true
		case readUnique:
			if n := int(h.Len) - buffer.OutMessageHeaderSize; grown && h.Error == 0 && n < 300 {
				t.Errorf("the kernel got a %d-byte READ reply after size 300", n)
			}
		}
	}
}

// A reply that changes size records holds the write side of sizeReplyMu while
// it is written, and a READ reply the read side.
func TestRepliesHoldSizeReplyMuWhileWritten(t *testing.T) {
	type check struct {
		name   string
		opcode uint32
		body   [][]byte
		serve  func(op interface{}) error
		read   bool
	}
	var checks []check
	for _, g := range sizeGrowths() {
		if g.held {
			checks = append(checks, check{g.name, g.opcode, g.body, g.serve, false})
		}
	}
	in := fusekernel.ReadIn{Size: 512}
	checks = append(checks, check{"read", fusekernel.OpRead, [][]byte{structBytes(&in)}, func(op interface{}) error {
		op.(*fuseops.ReadFileOp).BytesRead = 200
		return nil
	}, true})

	for _, ch := range checks {
		t.Run(ch.name, func(t *testing.T) {
			o, hooks := newHookTestConnection(t, true)
			o.lookUp(2, 200)
			hooks.before = func() {
				if o.c.sizeReplyMu.TryLock() {
					o.c.sizeReplyMu.Unlock()
					t.Error("reply written without sizeReplyMu")
				}
				readable := o.c.sizeReplyMu.TryRLock()
				if readable {
					o.c.sizeReplyMu.RUnlock()
				}
				if readable != ch.read {
					t.Errorf("read side free while written = %v, want %v", readable, ch.read)
				}
			}
			o.do(ch.opcode, 2, ch.serve, ch.body...)
		})
	}
}

// parkedInReply reports whether a goroutine is blocked in frame, called from
// Connection.Reply.
func parkedInReply(frame string) bool {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, frame) && strings.Contains(g, "(*Connection).Reply") {
			return true
		}
	}
	return false
}

func waitParkedInReply(t *testing.T, frame string, done <-chan struct{}) {
	t.Helper()
	for !parkedInReply(frame) {
		select {
		case <-done:
			t.Fatalf("Reply returned without blocking in %s", frame)
		default:
		}
		runtime.Gosched()
	}
}

func replyInBackground(t *testing.T, o *opTestConnection, ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := o.c.Reply(ctx, nil); err != nil {
			t.Errorf("Reply: %v", err)
		}
	}()
	return done
}

// A READ reply checks only once it holds the read side, so a growth recorded
// and written by a size-changing reply in flight counts in its check.
func TestReadCheckWaitsForReadSide(t *testing.T) {
	o := newOpTestConnection(t, true)
	o.lookUp(2, 200)
	in := fusekernel.ReadIn{Size: 512}
	read, op := o.send(fusekernel.OpRead, 2, structBytes(&in))
	readUnique := o.unique
	op.(*fuseops.ReadFileOp).BytesRead = 250

	// Stand in for a size-changing reply that grows inode 2 to 300.
	o.c.sizeReplyMu.Lock()
	done := replyInBackground(t, o, read)
	waitParkedInReply(t, "sync.(*RWMutex).RLock", done)
	o.c.mu.Lock()
	o.c.reportedInodes[2] = reportedInode{size: 300, lookups: 1}
	o.c.mu.Unlock()
	o.c.sizeReplyMu.Unlock()
	<-done

	for _, w := range o.transport.written() {
		if h := (*fusekernel.OutHeader)(unsafe.Pointer(&w[0])); h.Unique == readUnique && syscall.Errno(-h.Error) != syscall.EIO {
			t.Errorf("READ reply after growth to 300 = errno %d, want EIO", -h.Error)
		}
	}
}

// A size-changing reply records nothing before it holds the write side.
func TestSizeChangeRecordedUnderWriteSide(t *testing.T) {
	o := newOpTestConnection(t, true)
	o.lookUp(2, 200)
	ctx, op := o.send(fusekernel.OpGetattr, 2)
	getattrReply(300)(op)

	// Stand in for a READ reply in flight.
	o.c.sizeReplyMu.RLock()
	done := replyInBackground(t, o, ctx)
	waitParkedInReply(t, "sync.(*RWMutex).Lock", done)
	if got := o.c.reportedSize(2); got != 200 {
		t.Errorf("size recorded while the reply waits for the write side = %d, want 200", got)
	}
	o.c.sizeReplyMu.RUnlock()
	<-done
	if got := o.c.reportedSize(2); got != 300 {
		t.Errorf("size recorded after the reply = %d, want 300", got)
	}
}

// heldSizeReplyMuLog fails the test when it is written while sizeReplyMu is
// free.
type heldSizeReplyMuLog struct {
	t     *testing.T
	c     *Connection
	armed bool
}

func (l *heldSizeReplyMuLog) Write(p []byte) (int, error) {
	if l.armed && l.c.sizeReplyMu.TryLock() {
		l.c.sizeReplyMu.Unlock()
		l.t.Error("sizeReplyMu free after a size-changing reply's shrink, before Reply returns")
	}
	return len(p), nil
}

// A size-changing reply holds the write side until its shrink is applied.
// Reply writes the wire log after the shrink, before it releases the lock.
func TestSizeChangeHoldsWriteSideThroughShrink(t *testing.T) {
	init := fusekernel.InitIn{Major: fusekernel.ProtoVersionMaxMajor, Minor: fusekernel.ProtoVersionMaxMinor}
	transport := &testTransport{messages: [][]byte{makeOpMessage(fusekernel.OpInit, 1, 0, structBytes(&init))}}
	wireLog := &heldSizeReplyMuLog{t: t}
	c, err := newConnection(normalizedConfig(t, MountConfig{}), nil, nil, wireLog, paddingTestTransport{transport})
	if err != nil {
		t.Fatalf("newConnection: %v", err)
	}
	wireLog.c = c
	o := &opTestConnection{t: t, transport: transport, c: c, unique: 1}
	o.lookUp(2, 200)

	wireLog.armed = true
	o.do(fusekernel.OpGetattr, 2, getattrReply(100))
	wireLog.armed = false
	if got := o.c.reportedSize(2); got != 100 {
		t.Errorf("recorded size = %d, want 100", got)
	}
}

// fuseutil answers ForgetInodeOp inline in its serve loop, so a forget must
// never wait on sizeReplyMu.
func TestForgetTakesNoSideOfSizeReplyMu(t *testing.T) {
	for name, send := range map[string]func(o *opTestConnection) context.Context{
		"forget": func(o *opTestConnection) context.Context { return o.sendForget(2, 1) },
		"batch forget": func(o *opTestConnection) context.Context {
			count := fusekernel.BatchForgetCountIn{Count: 1}
			entry := fusekernel.BatchForgetEntryIn{Inode: 2, Nlookup: 1}
			ctx, _ := o.send(fusekernel.OpBatchForget, 0, structBytes(&count), structBytes(&entry))
			return ctx
		},
	} {
		t.Run(name, func(t *testing.T) {
			o := newOpTestConnection(t, true)
			o.lookUp(2, 200)
			ctx := send(o)

			// Stand in for a size-changing reply in flight.
			o.c.sizeReplyMu.Lock()
			defer o.c.sizeReplyMu.Unlock()
			done := replyInBackground(t, o, ctx)
			for {
				select {
				case <-done:
					return
				default:
				}
				if parkedInReply("sync.(*RWMutex).Lock") || parkedInReply("sync.(*RWMutex).RLock") {
					t.Fatal("forget waits on sizeReplyMu")
				}
				runtime.Gosched()
			}
		})
	}
}

func TestOtherTransportsSkipSizeReplyMu(t *testing.T) {
	o, hooks := newHookTestConnection(t, false)
	free := func() {
		if !o.c.sizeReplyMu.TryLock() {
			t.Error("a transport that does not pad short reads took sizeReplyMu")
			return
		}
		o.c.sizeReplyMu.Unlock()
	}
	hooks.before = free
	o.lookUp(2, 200)
	hooks.before = free
	o.read(2, 0, 512, 46)
}

// Growth counts once the reply is sent, a shrink only once the kernel has it.
func TestReportedSizeWhenKernelRefusesReply(t *testing.T) {
	tests := []struct {
		name      string
		reported  uint64
		n         int
		wantErrno syscall.Errno
	}{
		{"shrink not applied", 46, 46, syscall.EIO},
		{"growth applied", 300, 250, syscall.EIO},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := newOpTestConnection(t, true)
			o.lookUp(2, 200)
			o.transport.writeErr = syscall.ENOENT
			o.do(fusekernel.OpGetattr, 2, func(op interface{}) error {
				op.(*fuseops.GetInodeAttributesOp).Attributes.Size = tt.reported
				return nil
			})
			o.transport.writeErr = nil
			if errno, _ := o.read(2, 0, 512, tt.n); errno != tt.wantErrno {
				t.Errorf("read reply = errno %d, want %d", errno, tt.wantErrno)
			}
		})
	}
}

func TestReportedSizeFromEveryDirentPlusEntry(t *testing.T) {
	o := newOpTestConnection(t, true)
	o.lookUp(2, 200)
	o.readDirPlus(direntPlus(2, 100, "nine-byte"), direntPlus(3, 300, "f"))

	for inode, size := range map[fuseops.InodeID]int{2: 100, 3: 300} {
		if errno, n := o.read(inode, 0, 512, size); errno != 0 || n != size {
			t.Errorf("read of inode %d = errno %d with %d bytes, want %d bytes", inode, errno, n, size)
		}
		if errno, _ := o.read(inode, 0, 512, size-1); errno != syscall.EIO {
			t.Errorf("short read of inode %d = errno %d, want EIO", inode, errno)
		}
	}

	// Each entry gave the kernel one lookup.
	o.forget(2, 1)
	if errno, _ := o.read(2, 0, 512, 99); errno != syscall.EIO {
		t.Errorf("short read of inode 2 after forgetting one of two lookups = errno %d, want EIO", errno)
	}
	o.forget(3, 1)
	if errno, n := o.read(3, 0, 512, 46); errno != 0 || n != 46 {
		t.Errorf("read of inode 3 after forgetting its lookup = errno %d with %d bytes, want 46 bytes", errno, n)
	}
}

// The kernel ignores "." and "..", which take no lookup (fuse_lowlevel.h,
// readdirplus).
func TestDirentPlusDotEntriesIgnored(t *testing.T) {
	o := newOpTestConnection(t, true)
	o.lookUp(2, 200)
	o.readDirPlus(direntPlus(2, 100, "."), direntPlus(2, 100, ".."), direntPlus(3, 300, ".hidden"))
	if errno, _ := o.read(2, 0, 512, 150); errno != syscall.EIO {
		t.Errorf("short read after dot entries = errno %d, want EIO", errno)
	}
	o.forget(2, 1)
	if errno, n := o.read(2, 0, 512, 46); errno != 0 || n != 46 {
		t.Errorf("read after forgetting the only lookup = errno %d with %d bytes, want 46 bytes", errno, n)
	}
	if errno, _ := o.read(3, 0, 512, 299); errno != syscall.EIO {
		t.Errorf("short read of a dot-prefixed entry = errno %d, want EIO", errno)
	}
}

// A file system can write any Namelen. One past the reply ends the entries.
func TestDirentPlusNamelenPastReply(t *testing.T) {
	huge := direntPlus(3, 300, "f")
	binary.NativeEndian.PutUint32(huge[unsafe.Sizeof(fusekernel.EntryOut{})+unsafe.Offsetof(fusekernel.Dirent{}.Namelen):], math.MaxUint32)
	o := newOpTestConnection(t, true)
	o.readDirPlus(direntPlus(2, 100, "f"), huge)
	if errno, _ := o.read(2, 0, 512, 99); errno != syscall.EIO {
		t.Errorf("short read of the entry before = errno %d, want EIO", errno)
	}
	if errno, n := o.read(3, 0, 512, 46); errno != 0 || n != 46 {
		t.Errorf("read of the entry with a Namelen past the reply = errno %d with %d bytes, want 46 bytes", errno, n)
	}
}

// Reply checks BytesRead before the size tracker reads it, so a READDIRPLUS
// reply past its buffer is answered with EIO and records nothing.
func TestBytesReadPastBufferRecordsNothing(t *testing.T) {
	o := newOpTestConnection(t, true)
	in := fusekernel.ReadIn{Size: 4096}
	h := o.do(fusekernel.OpReaddirplus, 1, func(op interface{}) error {
		d := op.(*fuseops.ReadDirPlusOp)
		copy(d.Dst, direntPlus(2, 300, "f"))
		d.BytesRead = len(d.Dst) + 1
		return nil
	}, structBytes(&in))
	if syscall.Errno(-h.Error) != syscall.EIO {
		t.Errorf("readdirplus reply errno = %d, want EIO", -h.Error)
	}
	if errno, n := o.read(2, 0, 512, 46); errno != 0 || n != 46 {
		t.Errorf("read = errno %d with %d bytes, want 46 bytes: the failed readdirplus recorded a size", errno, n)
	}
}

// A file system may replace Dst with a shorter slice. A BytesRead past it is
// answered with EIO on every transport, and records nothing.
func TestBytesReadPastReplacedDst(t *testing.T) {
	entry := direntPlus(2, 300, "f")
	for _, opcode := range []uint32{fusekernel.OpReaddir, fusekernel.OpReaddirplus} {
		for _, pads := range []bool{false, true} {
			t.Run(fmt.Sprintf("opcode %d, pads short reads %v", opcode, pads), func(t *testing.T) {
				o := newOpTestConnection(t, pads)
				in := fusekernel.ReadIn{Size: 4096}
				h := o.do(opcode, 1, func(op interface{}) error {
					d, ok := op.(*fuseops.ReadDirOp)
					if !ok {
						d = &op.(*fuseops.ReadDirPlusOp).ReadDirOp
					}
					copy(d.Dst, entry)
					d.Dst = d.Dst[:8]
					d.BytesRead = len(entry)
					return nil
				}, structBytes(&in))
				if syscall.Errno(-h.Error) != syscall.EIO {
					t.Errorf("reply errno = %d, want EIO", -h.Error)
				}
				if errno, n := o.read(2, 0, 512, 46); errno != 0 || n != 46 {
					t.Errorf("read = errno %d with %d bytes, want 46 bytes: the failed reply recorded a size", errno, n)
				}
			})
		}
	}
}

// Callbacks run once Reply has released sizeReplyMu.
func TestCallbacksRunWithSizeReplyMuFree(t *testing.T) {
	o := newOpTestConnection(t, true)
	o.lookUp(2, 200)
	called := 0
	callback := func() {
		called++
		if !o.c.sizeReplyMu.TryLock() {
			t.Error("a callback ran while a reply held sizeReplyMu")
			return
		}
		o.c.sizeReplyMu.Unlock()
	}

	write := fusekernel.WriteIn{Offset: 0, Size: 10}
	o.do(fusekernel.OpWrite, 2, func(op interface{}) error {
		op.(*fuseops.WriteFileOp).Callback = callback
		return nil
	}, structBytes(&write), make([]byte, 10))
	read := fusekernel.ReadIn{Size: 512}
	o.do(fusekernel.OpRead, 2, func(op interface{}) error {
		r := op.(*fuseops.ReadFileOp)
		r.BytesRead = 200
		r.Callback = callback
		return nil
	}, structBytes(&read))
	if called != 2 {
		t.Errorf("callbacks run = %d, want 2", called)
	}
}

func TestNegativeEntryNotRecorded(t *testing.T) {
	o := newOpTestConnection(t, true)
	o.lookUp(0, 200)
	o.readDirPlus(direntPlus(0, 200, "f"))
	if _, ok := o.c.reportedInodes[0]; ok {
		t.Error("connection recorded inode 0")
	}
}

// The kernel takes no attributes from an error reply.
func TestReportedSizeUnchangedByFailedReply(t *testing.T) {
	for _, r := range sizeReporters() {
		for _, size := range []uint64{0, 300} {
			t.Run(fmt.Sprintf("%s reporting %d", r.name, size), func(t *testing.T) {
				o := newOpTestConnection(t, true)
				o.lookUp(2, 200)
				serve := r.serve(size)
				o.do(r.opcode, 2, func(op interface{}) error {
					serve(op)
					return syscall.EIO
				}, r.body...)
				if errno, _ := o.read(2, 0, 512, 199); errno != syscall.EIO {
					t.Errorf("short read reply = errno %d, want EIO", errno)
				}
				if errno, n := o.read(2, 0, 512, 200); errno != 0 || n != 200 {
					t.Errorf("read reply = errno %d with %d bytes, want 200 bytes", errno, n)
				}
				o.forget(2, 1)
				if errno, n := o.read(2, 0, 512, 46); errno != 0 || n != 46 {
					t.Errorf("read after forgetting the only lookup = errno %d with %d bytes, want 46 bytes", errno, n)
				}
			})
		}
	}
}

func TestReportedSizeDroppedOnForget(t *testing.T) {
	for name, forget := range map[string]func(*opTestConnection, fuseops.InodeID, uint64){
		"forget":       (*opTestConnection).forget,
		"batch forget": (*opTestConnection).batchForget,
		// The kernel has forgotten the inode whatever the file system returns.
		"forget the file system fails": func(o *opTestConnection, inode fuseops.InodeID, n uint64) {
			o.reply(o.sendForget(inode, n), syscall.ENOSYS)
		},
		"batch forget the file system fails": func(o *opTestConnection, inode fuseops.InodeID, n uint64) {
			count := fusekernel.BatchForgetCountIn{Count: 1}
			entry := fusekernel.BatchForgetEntryIn{Inode: int64(inode), Nlookup: n}
			o.do(fusekernel.OpBatchForget, 0, func(interface{}) error { return syscall.ENOSYS }, structBytes(&count), structBytes(&entry))
		},
	} {
		t.Run(name, func(t *testing.T) {
			o := newOpTestConnection(t, true)
			for range 3 {
				o.lookUp(2, 200)
			}
			forget(o, 2, 1)
			if errno, _ := o.read(2, 0, 512, 46); errno != syscall.EIO {
				t.Errorf("short read after forgetting one of three lookups = errno %d, want EIO", errno)
			}
			forget(o, 2, 2)
			if errno, n := o.read(2, 0, 512, 46); errno != 0 || n != 46 {
				t.Errorf("read after forgetting every lookup = errno %d with %d bytes, want 46 bytes", errno, n)
			}
		})
	}
}

// The kernel can send a forget while a lookup of the same inode is in flight.
func TestReportedSizeKeptWhenForgetCrossesLookup(t *testing.T) {
	for _, forgetFirst := range []bool{false, true} {
		o := newOpTestConnection(t, true)
		o.lookUp(2, 200)
		lookup, op := o.send(fusekernel.OpLookup, 1, []byte("f\x00"))
		setEntry(op, 2, 200)
		forget := o.sendForget(2, 1)
		if forgetFirst {
			o.reply(forget, nil)
			o.reply(lookup, nil)
		} else {
			o.reply(lookup, nil)
			o.reply(forget, nil)
		}
		if errno, _ := o.read(2, 0, 512, 46); errno != syscall.EIO {
			t.Errorf("forget replied first %v: short read = errno %d, want EIO", forgetFirst, errno)
		}
	}
}

// The check uses the larger of the sizes recorded when the read arrived and
// when it is answered.
func TestShortReadAgainstSizeWhenReadArrived(t *testing.T) {
	tests := []struct {
		name     string
		reported uint64
		n        int
	}{
		{"shrink reported while reading", 46, 46},
		{"growth reported while reading", 300, 250},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := newOpTestConnection(t, true)
			o.lookUp(2, 200)
			in := fusekernel.ReadIn{Size: 512}
			ctx, read := o.send(fusekernel.OpRead, 2, structBytes(&in))
			o.do(fusekernel.OpGetattr, 2, func(op interface{}) error {
				op.(*fuseops.GetInodeAttributesOp).Attributes.Size = tt.reported
				return nil
			})
			if errno, _ := o.answerRead(ctx, read, tt.n); errno != syscall.EIO {
				t.Errorf("read reply = errno %d, want EIO", errno)
			}
		})
	}
}

// Fails under -race when the connection's reported sizes are not locked.
func TestConcurrentOpsTrackSizes(t *testing.T) {
	const rounds, inodes = 8, 128
	o := newOpTestConnection(t, true)
	var wg sync.WaitGroup
	reply := func(ctx context.Context) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := o.c.Reply(ctx, nil); err != nil {
				t.Errorf("Reply: %v", err)
			}
		}()
	}

	for round := range rounds {
		first := fuseops.InodeID(2 + round*inodes)
		var replies []context.Context
		for inode := first; inode < first+inodes; inode++ {
			ctx, op := o.send(fusekernel.OpLookup, 1, []byte("f\x00"))
			setEntry(op, inode, 200)
			replies = append(replies, ctx)
		}
		// Forgets change records without sizeReplyMu.
		if round > 0 {
			for inode := first - inodes; inode < first; inode++ {
				replies = append(replies, o.sendForget(inode, 1))
			}
		}
		for inode := first; inode < first+inodes; inode++ {
			in := fusekernel.ReadIn{Size: 512}
			o.unique++
			o.transport.messages = append(o.transport.messages, makeOpMessage(fusekernel.OpRead, o.unique, uint64(inode), structBytes(&in)))
		}

		for _, ctx := range replies {
			reply(ctx)
		}
		// ReadOp reads each read while the lookups and forgets are answered.
		// Yielding after it, without taking a lock, lets them write in between.
		for range inodes {
			ctx, op, err := o.c.ReadOp()
			if err != nil {
				t.Fatalf("ReadOp: %v", err)
			}
			runtime.Gosched()
			op.(*fuseops.ReadFileOp).BytesRead = 200
			reply(ctx)
		}
		wg.Wait()
	}
}

func TestReportedSizeExtendedByWrite(t *testing.T) {
	o := newOpTestConnection(t, true)
	o.lookUp(2, 200)
	o.write(2, 200, 100)
	if errno, _ := o.read(2, 0, 512, 250); errno != syscall.EIO {
		t.Errorf("read short of the written end = errno %d, want EIO", errno)
	}
	if errno, n := o.read(2, 0, 512, 300); errno != 0 || n != 300 {
		t.Errorf("read to the written end = errno %d with %d bytes, want 300 bytes", errno, n)
	}

	o.write(2, 0, 10)
	if errno, _ := o.read(2, 0, 512, 250); errno != syscall.EIO {
		t.Errorf("read after a write inside the size = errno %d, want EIO", errno)
	}

	o.write(3, 0, 100)
	if errno, n := o.read(3, 0, 512, 46); errno != 0 || n != 46 {
		t.Errorf("read of an inode with no reported size = errno %d with %d bytes, want 46 bytes", errno, n)
	}
}
