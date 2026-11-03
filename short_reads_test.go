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
	"runtime"
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

func structBytes[T any](v *T) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(v)), unsafe.Sizeof(*v))
}

func makeOpMessage(opcode uint32, unique, nodeid uint64, body ...[]byte) []byte {
	header := fusekernel.InHeader{Opcode: opcode, Unique: unique, Nodeid: nodeid}
	message := append([]byte(nil), structBytes(&header)...)
	for _, b := range body {
		message = append(message, b...)
	}
	binary.NativeEndian.PutUint32(message, uint32(len(message)))
	return message
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
	cfg := MountConfig{OpContext: context.Background(), MaxWrite: uint32(buffer.GetPageSize())}
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
	writes := len(o.transport.writes)
	if err := o.c.Reply(ctx, opErr); err != nil {
		o.t.Fatalf("Reply: %v", err)
	}
	if len(o.transport.writes) == writes {
		return nil
	}
	return (*fusekernel.OutHeader)(unsafe.Pointer(&o.transport.writes[writes][0]))
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

// An entry reply records the inode and gives the kernel one lookup.
func TestReportedSizeFromEveryEntryReply(t *testing.T) {
	var (
		mkdir      fusekernel.MkdirIn
		mknod      fusekernel.MknodIn
		create     fusekernel.CreateIn
		link       = fusekernel.LinkIn{Oldnodeid: 2}
		dirplus    = fusekernel.ReadIn{Size: 4096}
		serveEntry = func(op interface{}) error {
			setEntry(op, 2, 100)
			return nil
		}
	)
	tests := []struct {
		name   string
		opcode uint32
		serve  func(op interface{}) error
		body   [][]byte
	}{
		{"lookup", fusekernel.OpLookup, serveEntry, [][]byte{[]byte("f\x00")}},
		{"mkdir", fusekernel.OpMkdir, serveEntry, [][]byte{structBytes(&mkdir), []byte("d\x00")}},
		{"mknod", fusekernel.OpMknod, serveEntry, [][]byte{structBytes(&mknod), []byte("n\x00")}},
		{"create", fusekernel.OpCreate, serveEntry, [][]byte{structBytes(&create), []byte("c\x00")}},
		{"symlink", fusekernel.OpSymlink, serveEntry, [][]byte{[]byte("s\x00t\x00")}},
		{"link", fusekernel.OpLink, serveEntry, [][]byte{structBytes(&link), []byte("l\x00")}},
		{"readdirplus", fusekernel.OpReaddirplus, func(op interface{}) error {
			o := op.(*fuseops.ReadDirPlusOp)
			o.BytesRead = copy(o.Dst, direntPlus(2, 100, "f"))
			return nil
		}, [][]byte{structBytes(&dirplus)}},
	}
	for _, tt := range tests {
		t.Run(tt.name+" of a new inode", func(t *testing.T) {
			o := newOpTestConnection(t, true)
			o.do(tt.opcode, 2, tt.serve, tt.body...)
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
		t.Run(tt.name+" shrinking a held inode", func(t *testing.T) {
			o := newOpTestConnection(t, true)
			o.lookUp(2, 200)
			o.do(tt.opcode, 2, tt.serve, tt.body...)
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

// An attributes reply updates an inode the kernel holds, gives it no lookup,
// and creates no record.
func TestReportedSizeFromEveryAttributesReply(t *testing.T) {
	var setattr fusekernel.SetattrIn
	tests := []struct {
		name   string
		opcode uint32
		serve  func(size uint64) func(op interface{}) error
		body   [][]byte
	}{
		{"getattr", fusekernel.OpGetattr, func(size uint64) func(op interface{}) error {
			return func(op interface{}) error {
				op.(*fuseops.GetInodeAttributesOp).Attributes.Size = size
				return nil
			}
		}, nil},
		{"setattr", fusekernel.OpSetattr, func(size uint64) func(op interface{}) error {
			return func(op interface{}) error {
				op.(*fuseops.SetInodeAttributesOp).Attributes.Size = size
				return nil
			}
		}, [][]byte{structBytes(&setattr)}},
	}
	for _, tt := range tests {
		t.Run(tt.name+" of a held inode", func(t *testing.T) {
			o := newOpTestConnection(t, true)
			o.lookUp(2, 200)
			o.lookUp(2, 200)
			o.do(tt.opcode, 2, tt.serve(100), tt.body...)
			if errno, n := o.read(2, 0, 512, 100); errno != 0 || n != 100 {
				t.Errorf("read reply = errno %d with %d bytes, want 100 bytes", errno, n)
			}
			if errno, _ := o.read(2, 0, 512, 99); errno != syscall.EIO {
				t.Errorf("short read reply = errno %d, want EIO", errno)
			}
			o.forget(2, 1)
			if errno, _ := o.read(2, 0, 512, 99); errno != syscall.EIO {
				t.Errorf("short read after forgetting one of two lookups = errno %d, want EIO", errno)
			}
			o.forget(2, 1)
			if errno, n := o.read(2, 0, 512, 46); errno != 0 || n != 46 {
				t.Errorf("read after forgetting both lookups = errno %d with %d bytes, want 46 bytes", errno, n)
			}
		})
		t.Run(tt.name+" of an inode no entry reply reported", func(t *testing.T) {
			o := newOpTestConnection(t, true)
			o.do(tt.opcode, 2, tt.serve(200), tt.body...)
			if errno, n := o.read(2, 0, 512, 46); errno != 0 || n != 46 {
				t.Errorf("read reply = errno %d with %d bytes, want 46 bytes", errno, n)
			}
		})
	}
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

func TestNegativeEntryNotRecorded(t *testing.T) {
	o := newOpTestConnection(t, true)
	o.lookUp(0, 200)
	o.readDirPlus(direntPlus(0, 200, "f"))
	if _, ok := o.c.reportedInodes[0]; ok {
		t.Error("connection recorded inode 0")
	}
}

func TestReportedSizeKeptOnFailedReply(t *testing.T) {
	o := newOpTestConnection(t, true)
	o.lookUp(2, 200)
	o.do(fusekernel.OpGetattr, 2, func(interface{}) error { return syscall.ENOENT })
	if errno, _ := o.read(2, 0, 512, 46); errno != syscall.EIO {
		t.Errorf("short read reply = errno %d, want EIO", errno)
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
		var lookups []context.Context
		for inode := first; inode < first+inodes; inode++ {
			ctx, op := o.send(fusekernel.OpLookup, 1, []byte("f\x00"))
			setEntry(op, inode, 200)
			lookups = append(lookups, ctx)
		}
		for inode := first; inode < first+inodes; inode++ {
			in := fusekernel.ReadIn{Size: 512}
			o.unique++
			o.transport.messages = append(o.transport.messages, makeOpMessage(fusekernel.OpRead, o.unique, uint64(inode), structBytes(&in)))
		}

		for _, ctx := range lookups {
			reply(ctx)
		}
		// ReadOp reads each read while the lookups are answered. Yielding
		// after it, without taking a lock, lets them write in between.
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
