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
	c, err := newConnection(MountConfig{OpContext: context.Background()}, nil, nil, nil, kernel)
	if err != nil {
		t.Fatalf("newConnection: %v", err)
	}
	return &opTestConnection{t: t, transport: transport, c: c, unique: 1}
}

// do sends one op, replies with what serve returns after filling the op in,
// and returns the reply's header, or nil for an op the kernel expects no reply
// to.
func (o *opTestConnection) do(opcode uint32, nodeid uint64, serve func(op interface{}) error, body ...[]byte) *fusekernel.OutHeader {
	o.t.Helper()
	o.unique++
	o.transport.messages = append(o.transport.messages, makeOpMessage(opcode, o.unique, nodeid, body...))
	writes := len(o.transport.writes)

	ctx, op, err := o.c.ReadOp()
	if err != nil {
		o.t.Fatalf("ReadOp: %v", err)
	}
	if err := o.c.Reply(ctx, serve(op)); err != nil {
		o.t.Fatalf("Reply: %v", err)
	}

	if len(o.transport.writes) == writes {
		return nil
	}
	return (*fusekernel.OutHeader)(unsafe.Pointer(&o.transport.writes[writes][0]))
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
	h := o.do(fusekernel.OpRead, uint64(inode), func(op interface{}) error {
		op.(*fuseops.ReadFileOp).BytesRead = n
		return nil
	}, structBytes(&in))
	return syscall.Errno(-h.Error), int(h.Len) - buffer.OutMessageHeaderSize
}

func (o *opTestConnection) write(inode fuseops.InodeID, offset uint64, n int) {
	o.t.Helper()
	in := fusekernel.WriteIn{Offset: offset, Size: uint32(n)}
	o.do(fusekernel.OpWrite, uint64(inode), func(interface{}) error { return nil }, structBytes(&in), make([]byte, n))
}

func (o *opTestConnection) forget(inode fuseops.InodeID) {
	o.t.Helper()
	in := fusekernel.ForgetIn{Nlookup: 1}
	o.do(fusekernel.OpForget, uint64(inode), func(interface{}) error { return nil }, structBytes(&in))
}

func (o *opTestConnection) batchForget(inode fuseops.InodeID) {
	o.t.Helper()
	count := fusekernel.BatchForgetCountIn{Count: 1}
	entry := fusekernel.BatchForgetEntryIn{Inode: int64(inode), Nlookup: 1}
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
	if o.c.reportedSizes != nil {
		t.Error("connection records sizes on a transport that does not pad short reads")
	}
	o.lookUp(2, 200)
	if errno, n := o.read(2, 0, 512, 46); errno != 0 || n != 46 {
		t.Errorf("read reply = errno %d with %d bytes, want 46 bytes", errno, n)
	}
}

func TestReportedSizeComesFromEveryAttributeReply(t *testing.T) {
	var (
		setattr    fusekernel.SetattrIn
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
		{"getattr", fusekernel.OpGetattr, func(op interface{}) error {
			op.(*fuseops.GetInodeAttributesOp).Attributes.Size = 100
			return nil
		}, nil},
		{"setattr", fusekernel.OpSetattr, func(op interface{}) error {
			op.(*fuseops.SetInodeAttributesOp).Attributes.Size = 100
			return nil
		}, [][]byte{structBytes(&setattr)}},
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
		t.Run(tt.name, func(t *testing.T) {
			o := newOpTestConnection(t, true)
			o.lookUp(2, 200)
			o.do(tt.opcode, 2, tt.serve, tt.body...)
			if errno, n := o.read(2, 0, 512, 100); errno != 0 || n != 100 {
				t.Errorf("read reply = errno %d with %d bytes, want 100 bytes", errno, n)
			}
			if errno, _ := o.read(2, 0, 512, 99); errno != syscall.EIO {
				t.Errorf("short read reply = errno %d, want EIO", errno)
			}
		})
	}
}

func TestReportedSizeFromEveryDirentPlusEntry(t *testing.T) {
	o := newOpTestConnection(t, true)
	in := fusekernel.ReadIn{Size: 4096}
	o.do(fusekernel.OpReaddirplus, 1, func(op interface{}) error {
		dirplus := op.(*fuseops.ReadDirPlusOp)
		entries := append(direntPlus(2, 100, "nine-byte"), direntPlus(3, 300, "f")...)
		dirplus.BytesRead = copy(dirplus.Dst, entries)
		return nil
	}, structBytes(&in))

	for inode, size := range map[fuseops.InodeID]uint32{2: 100, 3: 300} {
		if errno, _ := o.read(inode, 0, 512, int(size)-1); errno != syscall.EIO {
			t.Errorf("short read of inode %d = errno %d, want EIO", inode, errno)
		}
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
	for name, forget := range map[string]func(*opTestConnection, fuseops.InodeID){
		"forget":       (*opTestConnection).forget,
		"batch forget": (*opTestConnection).batchForget,
	} {
		t.Run(name, func(t *testing.T) {
			o := newOpTestConnection(t, true)
			o.lookUp(2, 200)
			forget(o, 2)
			if errno, n := o.read(2, 0, 512, 46); errno != 0 || n != 46 {
				t.Errorf("read reply = errno %d with %d bytes, want 46 bytes", errno, n)
			}
		})
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
