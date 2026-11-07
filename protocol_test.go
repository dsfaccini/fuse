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

package fuse_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/dsfaccini/fuse"
	"github.com/dsfaccini/fuse/fuseops"
	"github.com/dsfaccini/fuse/fuseutil"
	"github.com/dsfaccini/fuse/internal/fusekernel"
)

// The protocol tests send each op that artifact-fs serves, encoded as the
// kernel encodes it, to fuseutil.NewFileSystemServer, the entry point
// artifact-fs uses. They check the op the file system gets and the reply the
// kernel gets, without a mount.

// fakeKernel is the kernel end that fuse.ServeFakeKernel and
// fuse.ServeFakeFSKit return.
type fakeKernel interface {
	Send(buffers ...[]byte)
	NextReply(t testing.TB) []byte
	UnreadReplies() int
}

func TestProtocolOverKernelTransport(t *testing.T) {
	testProtocol(t, func(t *testing.T, server fuse.Server) fakeKernel {
		return fuse.ServeFakeKernel(t, server)
	})
}

const (
	reqUnique = 42
	reqUid    = 501
	reqPid    = 4242

	parent fuseops.InodeID  = 2
	child  fuseops.InodeID  = 3
	handle fuseops.HandleID = 4
)

var reqContext = fuseops.OpContext{FuseID: reqUnique, Pid: reqPid, Uid: reqUid}

type protocolCase struct {
	name    string
	request [][]byte
	// serve checks the op the file system got and fills in its response.
	serve func(t *testing.T, op any) error
	// errno is the error the reply carries, and reply is its body.
	errno syscall.Errno
	reply []byte
	// checkBody checks the body instead of reply. A cache timeout counts down
	// from the reply, so a reply that carries one differs from run to run.
	checkBody func(t *testing.T, body []byte)
	// noReply ops get no reply.
	noReply bool
}

func testProtocol(t *testing.T, serve func(*testing.T, fuse.Server) fakeKernel) {
	for _, c := range protocolCases() {
		t.Run(c.name, func(t *testing.T) {
			var k fakeKernel
			if c.noReply {
				// Registered first, so it runs after the connection ends.
				t.Cleanup(func() {
					if n := k.UnreadReplies(); n != 0 {
						t.Errorf("the connection sent %d replies, want none", n)
					}
				})
			}
			served := make(chan struct{}, 1)
			k = serve(t, fuseutil.NewFileSystemServer(&protocolFS{serve: func(op any) error {
				defer func() { served <- struct{}{} }()
				return c.serve(t, op)
			}}))
			k.Send(c.request...)
			select {
			case <-served:
			case <-time.After(5 * time.Second):
				t.Fatal("the file system got no op within 5s")
			}
			switch {
			case c.noReply:
			case c.checkBody != nil:
				c.checkBody(t, checkReplyHeader(t, k.NextReply(t), c.errno))
			default:
				checkReply(t, k.NextReply(t), c.errno, c.reply)
			}
		})
	}
}

func checkReply(t *testing.T, reply []byte, errno syscall.Errno, body []byte) {
	t.Helper()
	if got := checkReplyHeader(t, reply, errno); !bytes.Equal(got, body) {
		t.Errorf("reply body = %x, want %x", got, body)
	}
}

// checkReplyHeader checks the reply's header and returns its body.
func checkReplyHeader(t *testing.T, reply []byte, errno syscall.Errno) []byte {
	t.Helper()
	var h fusekernel.OutHeader
	if err := binary.Read(bytes.NewReader(reply), binary.NativeEndian, &h); err != nil {
		t.Fatalf("reply %x: %v", reply, err)
	}
	if int(h.Len) != len(reply) {
		t.Errorf("reply header len = %d, sent %d bytes", h.Len, len(reply))
	}
	if h.Error != -int32(errno) {
		t.Errorf("reply error = %d, want %d", h.Error, -int32(errno))
	}
	if h.Unique != reqUnique {
		t.Errorf("reply unique = %d, want %d", h.Unique, reqUnique)
	}
	return reply[binary.Size(h):]
}

// validFor reads a cache timeout, its seconds at secsAt and its nanoseconds at
// nsecAt.
func validFor(body []byte, secsAt, nsecAt int) time.Duration {
	return time.Duration(binary.NativeEndian.Uint64(body[secsAt:]))*time.Second +
		time.Duration(binary.NativeEndian.Uint32(body[nsecAt:]))
}

// checkValidFor checks a cache timeout set to ttl from now when the file
// system answered.
func checkValidFor(t *testing.T, name string, got, ttl time.Duration) {
	t.Helper()
	if got <= ttl-time.Minute || got > ttl {
		t.Errorf("%s = %v, want just under %v", name, got, ttl)
	}
}

// checkOp checks that op equals want and returns it, or nil when its type
// differs. Dst points into the connection's buffers, so only its length is
// compared.
func checkOp[T any](t *testing.T, op any, want *T) *T {
	t.Helper()
	got, ok := op.(*T)
	if !ok {
		t.Errorf("file system got %T, want %T", op, want)
		return nil
	}
	if wantDst := reflect.ValueOf(want).Elem().FieldByName("Dst"); wantDst.IsValid() {
		gotDst := reflect.ValueOf(got).Elem().FieldByName("Dst")
		if gotDst.Len() != wantDst.Len() {
			t.Errorf("%T.Dst has %d bytes, want %d", got, gotDst.Len(), wantDst.Len())
		}
		wantDst.Set(gotDst)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("file system got %+v\nwant %+v", got, want)
	}
	return got
}

func request(opcode uint32, node fuseops.InodeID, body ...[]byte) [][]byte {
	return fuse.FakeRequest(fusekernel.InHeader{
		Opcode: opcode,
		Unique: reqUnique,
		Nodeid: uint64(node),
		Uid:    reqUid,
		Gid:    20,
		Pid:    reqPid,
	}, body...)
}

// join concatenates parts.
func join(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

// names encodes each name as the kernel does, followed by a NUL.
func names(names ...string) []byte {
	var b []byte
	for _, name := range names {
		b = append(append(b, name...), 0)
	}
	return b
}

func ptr[T any](v T) *T { return &v }

var attrs = fuseops.InodeAttributes{
	Size:   1000,
	Nlink:  1,
	Mode:   0o644,
	Atime:  time.Unix(1, 2),
	Mtime:  time.Unix(3, 4),
	Ctime:  time.Unix(5, 6),
	Crtime: time.Unix(7, 8),
	Uid:    501,
	Gid:    20,
}

// kernelAttr is attrs as the kernel gets them for inode.
func kernelAttr(inode fuseops.InodeID) fusekernel.Attr {
	a := fusekernel.Attr{
		Ino:       uint64(inode),
		Size:      1000,
		Blocks:    2,
		Atime:     1,
		AtimeNsec: 2,
		Mtime:     3,
		MtimeNsec: 4,
		Ctime:     5,
		CtimeNsec: 6,
		Mode:      syscall.S_IFREG | 0o644,
		Nlink:     1,
		Uid:       501,
		Gid:       20,
	}
	a.SetCrtime(7, 8)
	return a
}

var entry = fuseops.ChildInodeEntry{Child: child, Generation: 9, Attributes: attrs}

// kernelEntry is entry as the kernel gets it.
func kernelEntry() []byte {
	return kernelEntryWithMode(syscall.S_IFREG | 0o644)
}

// entryWithMode is entry with the file mode mode.
func entryWithMode(mode os.FileMode) fuseops.ChildInodeEntry {
	e := entry
	e.Attributes.Mode = mode
	return e
}

// kernelEntryWithMode is entry as the kernel gets it, with the mode kernelMode.
func kernelEntryWithMode(kernelMode uint32) []byte {
	a := kernelAttr(child)
	a.Mode = kernelMode
	return fuse.NativeBytes(&fusekernel.EntryOut{Nodeid: uint64(child), Generation: 9, Attr: a})
}

// The protocol cases cover the ops of artifact-fs's file system, in the order
// of fuseutil.FileSystem.
func protocolCases() []protocolCase {
	readIn := fuse.NativeBytes(&fusekernel.ReadIn{Fh: uint64(handle), Offset: 10, Size: 1 << 16})
	dirent := fuseutil.Dirent{Offset: 1, Inode: 5, Name: "file", Type: fuseutil.DT_File}
	direntPlus := fuseutil.DirentPlus{Dirent: dirent, Entry: entry}

	var setattrIn fusekernel.SetattrIn
	setattrIn.Valid = uint32(fusekernel.SetattrMode | fusekernel.SetattrUid | fusekernel.SetattrGid |
		fusekernel.SetattrSize | fusekernel.SetattrAtime | fusekernel.SetattrMtime | fusekernel.SetattrHandle)
	setattrIn.Fh = uint64(handle)
	setattrIn.Size = 10
	setattrIn.Mode = syscall.S_IFREG | 0o600
	setattrIn.Uid, setattrIn.Gid = 7, 8
	setattrIn.Atime, setattrIn.AtimeNsec = 1, 2
	setattrIn.Mtime, setattrIn.MtimeNsec = 3, 4

	var getxattrIn fusekernel.GetxattrIn
	getxattrIn.Size = 64
	var setxattrIn fusekernel.SetxattrIn
	setxattrIn.Size, setxattrIn.Flags = 5, 1

	return []protocolCase{
		{
			name:    "statfs",
			request: request(fusekernel.OpStatfs, parent),
			serve: func(t *testing.T, op any) error {
				if o := checkOp(t, op, &fuseops.StatFSOp{}); o != nil {
					*o = fuseops.StatFSOp{BlockSize: 4096, Blocks: 100, BlocksFree: 50, BlocksAvailable: 40, IoSize: 65536, Inodes: 10, InodesFree: 5}
				}
				return nil
			},
			reply: fuse.NativeBytes(&fusekernel.StatfsOut{St: fusekernel.Kstatfs{
				Blocks: 100, Bfree: 50, Bavail: 40, Files: 10, Ffree: 5, Bsize: 65536, Namelen: 255, Frsize: 4096,
			}}),
		},
		{
			name:    "lookup",
			request: request(fusekernel.OpLookup, parent, names("file")),
			serve: func(t *testing.T, op any) error {
				if o := checkOp(t, op, &fuseops.LookUpInodeOp{Parent: parent, Name: "file", OpContext: reqContext}); o != nil {
					o.Entry = entry
				}
				return nil
			},
			reply: kernelEntry(),
		},
		{
			name:    "lookup error",
			request: request(fusekernel.OpLookup, parent, names("missing")),
			serve: func(t *testing.T, op any) error {
				checkOp(t, op, &fuseops.LookUpInodeOp{Parent: parent, Name: "missing", OpContext: reqContext})
				return fmt.Errorf("lookup: %w", syscall.ENOENT)
			},
			errno: syscall.ENOENT,
		},
		{
			name:    "lookup with expirations",
			request: request(fusekernel.OpLookup, parent, names("file")),
			serve: func(t *testing.T, op any) error {
				if o := checkOp(t, op, &fuseops.LookUpInodeOp{Parent: parent, Name: "file", OpContext: reqContext}); o != nil {
					o.Entry = entry
					o.Entry.EntryExpiration = time.Now().Add(time.Hour)
					o.Entry.AttributesExpiration = time.Now().Add(2 * time.Hour)
				}
				return nil
			},
			// fuse_entry_out: entry_valid and attr_valid at 16 and 24, their
			// nanoseconds at 32 and 36.
			checkBody: func(t *testing.T, body []byte) {
				checkValidFor(t, "entry_valid", validFor(body, 16, 32), time.Hour)
				checkValidFor(t, "attr_valid", validFor(body, 24, 36), 2*time.Hour)
				got := bytes.Clone(body)
				clear(got[16:40])
				if want := kernelEntry(); !bytes.Equal(got, want) {
					t.Errorf("reply body without timeouts = %x, want %x", got, want)
				}
			},
		},
		{
			// A negative entry: the kernel caches that the name is missing.
			name:    "negative lookup",
			request: request(fusekernel.OpLookup, parent, names("missing")),
			serve: func(t *testing.T, op any) error {
				if o := checkOp(t, op, &fuseops.LookUpInodeOp{Parent: parent, Name: "missing", OpContext: reqContext}); o != nil {
					o.Entry = fuseops.ChildInodeEntry{EntryExpiration: time.Now().Add(time.Hour)}
				}
				return nil
			},
			checkBody: func(t *testing.T, body []byte) {
				if nodeid := binary.NativeEndian.Uint64(body); nodeid != 0 {
					t.Errorf("nodeid = %d, want 0", nodeid)
				}
				checkValidFor(t, "entry_valid", validFor(body, 16, 32), time.Hour)
			},
		},
		{
			name:    "getattr",
			request: request(fusekernel.OpGetattr, child, fuse.NativeBytes(&fusekernel.GetattrIn{})),
			serve: func(t *testing.T, op any) error {
				if o := checkOp(t, op, &fuseops.GetInodeAttributesOp{Inode: child, OpContext: reqContext}); o != nil {
					o.Attributes = attrs
				}
				return nil
			},
			reply: fuse.NativeBytes(&fusekernel.AttrOut{Attr: kernelAttr(child)}),
		},
		{
			name:    "getattr with expiration",
			request: request(fusekernel.OpGetattr, child, fuse.NativeBytes(&fusekernel.GetattrIn{})),
			serve: func(t *testing.T, op any) error {
				if o := checkOp(t, op, &fuseops.GetInodeAttributesOp{Inode: child, OpContext: reqContext}); o != nil {
					o.Attributes = attrs
					o.AttributesExpiration = time.Now().Add(time.Hour)
				}
				return nil
			},
			// fuse_attr_out: attr_valid at 0, its nanoseconds at 8.
			checkBody: func(t *testing.T, body []byte) {
				checkValidFor(t, "attr_valid", validFor(body, 0, 8), time.Hour)
				got := bytes.Clone(body)
				clear(got[0:12])
				if want := fuse.NativeBytes(&fusekernel.AttrOut{Attr: kernelAttr(child)}); !bytes.Equal(got, want) {
					t.Errorf("reply body without timeouts = %x, want %x", got, want)
				}
			},
		},
		{
			name:    "setattr",
			request: request(fusekernel.OpSetattr, child, fuse.NativeBytes(&setattrIn)),
			serve: func(t *testing.T, op any) error {
				if o := checkOp(t, op, &fuseops.SetInodeAttributesOp{
					Inode:     child,
					Handle:    ptr(handle),
					Uid:       ptr(uint32(7)),
					Gid:       ptr(uint32(8)),
					Size:      ptr(uint64(10)),
					Mode:      ptr(os.FileMode(0o600)),
					Atime:     ptr(time.Unix(1, 2)),
					Mtime:     ptr(time.Unix(3, 4)),
					OpContext: reqContext,
				}); o != nil {
					o.Attributes = attrs
				}
				return nil
			},
			reply: fuse.NativeBytes(&fusekernel.AttrOut{Attr: kernelAttr(child)}),
		},
		{
			name:    "forget",
			request: request(fusekernel.OpForget, child, fuse.NativeBytes(&fusekernel.ForgetIn{Nlookup: 2})),
			serve: func(t *testing.T, op any) error {
				checkOp(t, op, &fuseops.ForgetInodeOp{Inode: child, N: 2, OpContext: reqContext})
				return nil
			},
			noReply: true,
		},
		{
			name: "batch forget",
			request: request(fusekernel.OpBatchForget, 0,
				join(
					fuse.NativeBytes(&fusekernel.BatchForgetCountIn{Count: 2}),
					fuse.NativeBytes(&fusekernel.BatchForgetEntryIn{Inode: 3, Nlookup: 1}),
					fuse.NativeBytes(&fusekernel.BatchForgetEntryIn{Inode: 5, Nlookup: 2}))),
			serve: func(t *testing.T, op any) error {
				checkOp(t, op, &fuseops.BatchForgetOp{
					Entries:   []fuseops.BatchForgetEntry{{Inode: 3, N: 1}, {Inode: 5, N: 2}},
					OpContext: reqContext,
				})
				return nil
			},
			noReply: true,
		},
		{
			// The osxfuse kext sends S_IFDIR (fuse_vnop_mkdir in osxfuse/kext).
			// The kernel takes the file type from the reply's mode.
			name:    "mkdir",
			request: request(fusekernel.OpMkdir, parent, join(fuse.NativeBytes(&fusekernel.MkdirIn{Mode: syscall.S_IFDIR | 0o755, Umask: 0o022}), names("dir"))),
			serve: func(t *testing.T, op any) error {
				if o := checkOp(t, op, &fuseops.MkDirOp{Parent: parent, Name: "dir", Mode: os.ModeDir | 0o755, OpContext: reqContext}); o != nil {
					o.Entry = entryWithMode(os.ModeDir | 0o755)
				}
				return nil
			},
			reply: kernelEntryWithMode(syscall.S_IFDIR | 0o755),
		},
		{
			// Linux strips S_IFDIR and keeps the sticky bit (fs/fuse/dir.c
			// fuse_mkdir).
			name:    "mkdir without type bits",
			request: request(fusekernel.OpMkdir, parent, join(fuse.NativeBytes(&fusekernel.MkdirIn{Mode: syscall.S_ISVTX | 0o755, Umask: 0o022}), names("dir"))),
			serve: func(t *testing.T, op any) error {
				if o := checkOp(t, op, &fuseops.MkDirOp{Parent: parent, Name: "dir", Mode: os.ModeDir | os.ModeSticky | 0o755, OpContext: reqContext}); o != nil {
					o.Entry = entry
				}
				return nil
			},
			reply: kernelEntry(),
		},
		{
			name:    "mknod",
			request: request(fusekernel.OpMknod, parent, join(fuse.NativeBytes(&fusekernel.MknodIn{Mode: syscall.S_IFIFO | 0o644}), names("fifo"))),
			serve: func(t *testing.T, op any) error {
				if o := checkOp(t, op, &fuseops.MkNodeOp{Parent: parent, Name: "fifo", Mode: os.ModeNamedPipe | 0o644, OpContext: reqContext}); o != nil {
					o.Entry = entry
				}
				return nil
			},
			reply: kernelEntry(),
		},
		{
			name: "create",
			request: request(fusekernel.OpCreate, parent, join(
				fuse.NativeBytes(&fusekernel.CreateIn{Flags: uint32(os.O_RDWR | os.O_CREATE), Mode: syscall.S_IFREG | 0o644}),
				names("new"))),
			serve: func(t *testing.T, op any) error {
				if o := checkOp(t, op, &fuseops.CreateFileOp{
					Parent:    parent,
					Name:      "new",
					Mode:      0o644,
					OpenFlags: fusekernel.OpenFlags(os.O_RDWR | os.O_CREATE),
					OpContext: reqContext,
				}); o != nil {
					o.Entry, o.Handle = entry, handle
				}
				return nil
			},
			reply: join(kernelEntry(), fuse.NativeBytes(&fusekernel.OpenOut{Fh: uint64(handle)})),
		},
		{
			name:    "link",
			request: request(fusekernel.OpLink, parent, join(fuse.NativeBytes(&fusekernel.LinkIn{Oldnodeid: uint64(child)}), names("hard"))),
			serve: func(t *testing.T, op any) error {
				if o := checkOp(t, op, &fuseops.CreateLinkOp{Parent: parent, Name: "hard", Target: child, OpContext: reqContext}); o != nil {
					o.Entry = entry
				}
				return nil
			},
			reply: kernelEntry(),
		},
		{
			name:    "symlink",
			request: request(fusekernel.OpSymlink, parent, names("link", "target")),
			serve: func(t *testing.T, op any) error {
				if o := checkOp(t, op, &fuseops.CreateSymlinkOp{Parent: parent, Name: "link", Target: "target", OpContext: reqContext}); o != nil {
					o.Entry = entryWithMode(os.ModeSymlink | 0o777)
				}
				return nil
			},
			reply: kernelEntryWithMode(syscall.S_IFLNK | 0o777),
		},
		{
			name:    "rename",
			request: request(fusekernel.OpRename, parent, join(fuse.NativeBytes(&fusekernel.RenameIn{Newdir: 5}), names("old", "new"))),
			serve: func(t *testing.T, op any) error {
				checkOp(t, op, &fuseops.RenameOp{OldParent: parent, OldName: "old", NewParent: 5, NewName: "new", OpContext: reqContext})
				return nil
			},
		},
		{
			name:    "rmdir",
			request: request(fusekernel.OpRmdir, parent, names("dir")),
			serve: func(t *testing.T, op any) error {
				checkOp(t, op, &fuseops.RmDirOp{Parent: parent, Name: "dir", OpContext: reqContext})
				return nil
			},
		},
		{
			name:    "unlink",
			request: request(fusekernel.OpUnlink, parent, names("file")),
			serve: func(t *testing.T, op any) error {
				checkOp(t, op, &fuseops.UnlinkOp{Parent: parent, Name: "file", OpContext: reqContext})
				return nil
			},
		},
		{
			name:    "opendir",
			request: request(fusekernel.OpOpendir, child, fuse.NativeBytes(&fusekernel.OpenIn{})),
			serve: func(t *testing.T, op any) error {
				if o := checkOp(t, op, &fuseops.OpenDirOp{Inode: child, OpContext: reqContext}); o != nil {
					o.Handle, o.KeepCache = handle, true
				}
				return nil
			},
			reply: fuse.NativeBytes(&fusekernel.OpenOut{Fh: uint64(handle), OpenFlags: uint32(fusekernel.OpenKeepCache)}),
		},
		{
			name:    "readdir",
			request: request(fusekernel.OpReaddir, child, readIn),
			serve: func(t *testing.T, op any) error {
				if o := checkOp(t, op, &fuseops.ReadDirOp{Inode: child, Handle: handle, Offset: 10, Dst: make([]byte, 1<<16), OpContext: reqContext}); o != nil {
					o.BytesRead = fuseutil.WriteDirent(o.Dst, dirent)
				}
				return nil
			},
			// fuse_dirent, padded to 8 bytes.
			reply: join(
				fuse.NativeBytes(&fusekernel.Dirent{Ino: 5, Off: 1, Namelen: 4, Type: syscall.DT_REG})[:fusekernel.DirentSize],
				[]byte("file\x00\x00\x00\x00")),
		},
		{
			name:    "readdir error",
			request: request(fusekernel.OpReaddir, child, readIn),
			serve: func(t *testing.T, op any) error {
				if o := checkOp(t, op, &fuseops.ReadDirOp{Inode: child, Handle: handle, Offset: 10, Dst: make([]byte, 1<<16), OpContext: reqContext}); o != nil {
					o.BytesRead = fuseutil.WriteDirent(o.Dst, dirent)
				}
				return syscall.EIO
			},
			// macOS refuses an error reply longer than its header.
			errno: syscall.EIO,
		},
		{
			name:    "readdirplus",
			request: request(fusekernel.OpReaddirplus, child, readIn),
			serve: func(t *testing.T, op any) error {
				if o := checkOp(t, op, &fuseops.ReadDirPlusOp{ReadDirOp: fuseops.ReadDirOp{
					Inode: child, Handle: handle, Offset: 10, Dst: make([]byte, 1<<16), OpContext: reqContext,
				}}); o != nil {
					o.BytesRead = fuseutil.WriteDirentPlus(o.Dst, direntPlus)
				}
				return nil
			},
			// fuse_direntplus: fuse_entry_out, then fuse_dirent and the name,
			// padded to 8 bytes.
			reply: join(
				kernelEntry(),
				fuse.NativeBytes(&fusekernel.Dirent{Ino: 5, Off: 1, Namelen: 4, Type: syscall.DT_REG})[:fusekernel.DirentSize],
				[]byte("file\x00\x00\x00\x00")),
		},
		{
			name:    "releasedir",
			request: request(fusekernel.OpReleasedir, child, fuse.NativeBytes(&fusekernel.ReleaseIn{Fh: uint64(handle)})),
			serve: func(t *testing.T, op any) error {
				checkOp(t, op, &fuseops.ReleaseDirHandleOp{Handle: handle, OpContext: reqContext})
				return nil
			},
		},
		{
			name:    "open",
			request: request(fusekernel.OpOpen, child, fuse.NativeBytes(&fusekernel.OpenIn{Flags: uint32(os.O_WRONLY | os.O_APPEND)})),
			serve: func(t *testing.T, op any) error {
				if o := checkOp(t, op, &fuseops.OpenFileOp{Inode: child, OpenFlags: fusekernel.OpenFlags(os.O_WRONLY | os.O_APPEND), OpContext: reqContext}); o != nil {
					o.Handle, o.KeepPageCache, o.UseDirectIO = handle, true, true
				}
				return nil
			},
			reply: fuse.NativeBytes(&fusekernel.OpenOut{Fh: uint64(handle), OpenFlags: uint32(fusekernel.OpenKeepCache | fusekernel.OpenDirectIO)}),
		},
		{
			name:    "read into Dst",
			request: request(fusekernel.OpRead, child, readIn),
			serve: func(t *testing.T, op any) error {
				if o := checkOp(t, op, &fuseops.ReadFileOp{Inode: child, Handle: handle, Offset: 10, Size: 1 << 16, Dst: make([]byte, 1<<16), OpContext: reqContext}); o != nil {
					o.BytesRead = copy(o.Dst, "hello")
				}
				return nil
			},
			reply: []byte("hello"),
		},
		{
			name:    "read as Data",
			request: request(fusekernel.OpRead, child, readIn),
			serve: func(t *testing.T, op any) error {
				if o := checkOp(t, op, &fuseops.ReadFileOp{Inode: child, Handle: handle, Offset: 10, Size: 1 << 16, Dst: make([]byte, 1<<16), OpContext: reqContext}); o != nil {
					o.Data = [][]byte{[]byte("hello"), []byte(" world")}
					o.BytesRead = 11
				}
				return nil
			},
			reply: []byte("hello world"),
		},
		{
			name: "write",
			// FSKit hands over the payload in a second body buffer.
			request: request(fusekernel.OpWrite, child,
				fuse.NativeBytes(&fusekernel.WriteIn{Fh: uint64(handle), Offset: 10, Size: 7}),
				[]byte("payload")),
			serve: func(t *testing.T, op any) error {
				checkOp(t, op, &fuseops.WriteFileOp{Inode: child, Handle: handle, Offset: 10, Data: []byte("payload"), OpContext: reqContext})
				return nil
			},
			reply: fuse.NativeBytes(&fusekernel.WriteOut{Size: 7}),
		},
		{
			name: "write with bytes past Size",
			request: request(fusekernel.OpWrite, child,
				fuse.NativeBytes(&fusekernel.WriteIn{Fh: uint64(handle), Offset: 10, Size: 3}),
				[]byte("payload")),
			serve: func(t *testing.T, op any) error {
				checkOp(t, op, &fuseops.WriteFileOp{Inode: child, Handle: handle, Offset: 10, Data: []byte("pay"), OpContext: reqContext})
				return nil
			},
			reply: fuse.NativeBytes(&fusekernel.WriteOut{Size: 3}),
		},
		{
			name:    "fsync",
			request: request(fusekernel.OpFsync, child, fuse.NativeBytes(&fusekernel.FsyncIn{Fh: uint64(handle)})),
			serve: func(t *testing.T, op any) error {
				checkOp(t, op, &fuseops.SyncFileOp{Inode: child, Handle: handle, OpContext: reqContext})
				return nil
			},
		},
		{
			name:    "flush",
			request: request(fusekernel.OpFlush, child, fuse.NativeBytes(&fusekernel.FlushIn{Fh: uint64(handle)})),
			serve: func(t *testing.T, op any) error {
				checkOp(t, op, &fuseops.FlushFileOp{Inode: child, Handle: handle, OpContext: reqContext})
				return nil
			},
		},
		{
			name:    "release",
			request: request(fusekernel.OpRelease, child, fuse.NativeBytes(&fusekernel.ReleaseIn{Fh: uint64(handle)})),
			serve: func(t *testing.T, op any) error {
				checkOp(t, op, &fuseops.ReleaseFileHandleOp{Handle: handle, OpContext: reqContext})
				return nil
			},
		},
		{
			name:    "readlink",
			request: request(fusekernel.OpReadlink, child),
			serve: func(t *testing.T, op any) error {
				if o := checkOp(t, op, &fuseops.ReadSymlinkOp{Inode: child, OpContext: reqContext}); o != nil {
					o.Target = "target"
				}
				return nil
			},
			reply: []byte("target"),
		},
		{
			name:    "removexattr",
			request: request(fusekernel.OpRemovexattr, child, names("user.a")),
			serve: func(t *testing.T, op any) error {
				checkOp(t, op, &fuseops.RemoveXattrOp{Inode: child, Name: "user.a", OpContext: reqContext})
				return nil
			},
		},
		{
			name:    "getxattr",
			request: request(fusekernel.OpGetxattr, child, join(fuse.NativeBytes(&getxattrIn), names("user.a"))),
			serve: func(t *testing.T, op any) error {
				if o := checkOp(t, op, &fuseops.GetXattrOp{Inode: child, Name: "user.a", Dst: make([]byte, 64), OpContext: reqContext}); o != nil {
					o.BytesRead = copy(o.Dst, "value")
				}
				return nil
			},
			reply: []byte("value"),
		},
		{
			// A zero-size request asks for the size the list needs.
			name:    "listxattr size",
			request: request(fusekernel.OpListxattr, child, fuse.NativeBytes(&fusekernel.ListxattrIn{})),
			serve: func(t *testing.T, op any) error {
				if o := checkOp(t, op, &fuseops.ListXattrOp{Inode: child, OpContext: reqContext}); o != nil {
					o.BytesRead = 8
				}
				return nil
			},
			reply: fuse.NativeBytes(&fusekernel.GetxattrOut{Size: 8}),
		},
		{
			name:    "setxattr",
			request: request(fusekernel.OpSetxattr, child, join(fuse.NativeBytes(&setxattrIn), names("user.a"), []byte("value"))),
			serve: func(t *testing.T, op any) error {
				checkOp(t, op, &fuseops.SetXattrOp{Inode: child, Name: "user.a", Value: []byte("value"), Flags: 1, OpContext: reqContext})
				return nil
			},
		},
		{
			name:    "fallocate",
			request: request(fusekernel.OpFallocate, child, fuse.NativeBytes(&fusekernel.FallocateIn{Fh: uint64(handle), Offset: 10, Length: 20, Mode: 1})),
			serve: func(t *testing.T, op any) error {
				checkOp(t, op, &fuseops.FallocateOp{Inode: child, Handle: handle, Offset: 10, Length: 20, Mode: 1, OpContext: reqContext})
				return nil
			},
		},
		{
			name:    "syncfs",
			request: request(fusekernel.OpSyncFS, child, fuse.NativeBytes(&fusekernel.SyncFSIn{})),
			serve: func(t *testing.T, op any) error {
				checkOp(t, op, &fuseops.SyncFSOp{Inode: child, OpContext: fuseops.OpContext{Pid: reqPid}})
				return nil
			},
		},
	}
}

// forgetFS implements ForgetInode and not BatchForget, like artifact-fs.
type forgetFS struct {
	fuseutil.NotImplementedFileSystem
	forgets chan fuseops.ForgetInodeOp
}

func (fs *forgetFS) ForgetInode(_ context.Context, op *fuseops.ForgetInodeOp) error {
	fs.forgets <- *op
	return nil
}

// A file system without BatchForget gets each entry of a BATCH_FORGET as a
// ForgetInode.
func TestBatchForgetFallsBackToForgetInode(t *testing.T) {
	fs := &forgetFS{forgets: make(chan fuseops.ForgetInodeOp, 2)}
	var k fakeKernel
	// Registered first, so it runs after the connection ends.
	t.Cleanup(func() {
		if n := k.UnreadReplies(); n != 0 {
			t.Errorf("the connection sent %d replies, want none", n)
		}
	})
	k = fuse.ServeFakeKernel(t, fuseutil.NewFileSystemServer(fs))
	k.Send(request(fusekernel.OpBatchForget, 0,
		join(
			fuse.NativeBytes(&fusekernel.BatchForgetCountIn{Count: 2}),
			fuse.NativeBytes(&fusekernel.BatchForgetEntryIn{Inode: 3, Nlookup: 1}),
			fuse.NativeBytes(&fusekernel.BatchForgetEntryIn{Inode: 5, Nlookup: 2})))...)
	for _, want := range []fuseops.ForgetInodeOp{{Inode: 3, N: 1, OpContext: reqContext}, {Inode: 5, N: 2, OpContext: reqContext}} {
		select {
		case got := <-fs.forgets:
			if got != want {
				t.Errorf("ForgetInode got %+v, want %+v", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no ForgetInode of inode %d within 5s", want.Inode)
		}
	}
}

// protocolFS hands every op to serve.
type protocolFS struct {
	fuseutil.NotImplementedFileSystem
	serve func(op any) error
}

func (fs *protocolFS) StatFS(_ context.Context, op *fuseops.StatFSOp) error { return fs.serve(op) }
func (fs *protocolFS) LookUpInode(_ context.Context, op *fuseops.LookUpInodeOp) error {
	return fs.serve(op)
}
func (fs *protocolFS) GetInodeAttributes(_ context.Context, op *fuseops.GetInodeAttributesOp) error {
	return fs.serve(op)
}
func (fs *protocolFS) SetInodeAttributes(_ context.Context, op *fuseops.SetInodeAttributesOp) error {
	return fs.serve(op)
}
func (fs *protocolFS) ForgetInode(_ context.Context, op *fuseops.ForgetInodeOp) error {
	return fs.serve(op)
}
func (fs *protocolFS) BatchForget(_ context.Context, op *fuseops.BatchForgetOp) error {
	return fs.serve(op)
}
func (fs *protocolFS) MkDir(_ context.Context, op *fuseops.MkDirOp) error   { return fs.serve(op) }
func (fs *protocolFS) MkNode(_ context.Context, op *fuseops.MkNodeOp) error { return fs.serve(op) }
func (fs *protocolFS) CreateFile(_ context.Context, op *fuseops.CreateFileOp) error {
	return fs.serve(op)
}
func (fs *protocolFS) CreateLink(_ context.Context, op *fuseops.CreateLinkOp) error {
	return fs.serve(op)
}
func (fs *protocolFS) CreateSymlink(_ context.Context, op *fuseops.CreateSymlinkOp) error {
	return fs.serve(op)
}
func (fs *protocolFS) Rename(_ context.Context, op *fuseops.RenameOp) error { return fs.serve(op) }
func (fs *protocolFS) RmDir(_ context.Context, op *fuseops.RmDirOp) error   { return fs.serve(op) }
func (fs *protocolFS) Unlink(_ context.Context, op *fuseops.UnlinkOp) error { return fs.serve(op) }
func (fs *protocolFS) OpenDir(_ context.Context, op *fuseops.OpenDirOp) error {
	return fs.serve(op)
}
func (fs *protocolFS) ReadDir(_ context.Context, op *fuseops.ReadDirOp) error {
	return fs.serve(op)
}
func (fs *protocolFS) ReadDirPlus(_ context.Context, op *fuseops.ReadDirPlusOp) error {
	return fs.serve(op)
}
func (fs *protocolFS) ReleaseDirHandle(_ context.Context, op *fuseops.ReleaseDirHandleOp) error {
	return fs.serve(op)
}
func (fs *protocolFS) OpenFile(_ context.Context, op *fuseops.OpenFileOp) error {
	return fs.serve(op)
}
func (fs *protocolFS) ReadFile(_ context.Context, op *fuseops.ReadFileOp) error {
	return fs.serve(op)
}
func (fs *protocolFS) WriteFile(_ context.Context, op *fuseops.WriteFileOp) error {
	return fs.serve(op)
}
func (fs *protocolFS) SyncFile(_ context.Context, op *fuseops.SyncFileOp) error {
	return fs.serve(op)
}
func (fs *protocolFS) FlushFile(_ context.Context, op *fuseops.FlushFileOp) error {
	return fs.serve(op)
}
func (fs *protocolFS) ReleaseFileHandle(_ context.Context, op *fuseops.ReleaseFileHandleOp) error {
	return fs.serve(op)
}
func (fs *protocolFS) ReadSymlink(_ context.Context, op *fuseops.ReadSymlinkOp) error {
	return fs.serve(op)
}
func (fs *protocolFS) RemoveXattr(_ context.Context, op *fuseops.RemoveXattrOp) error {
	return fs.serve(op)
}
func (fs *protocolFS) GetXattr(_ context.Context, op *fuseops.GetXattrOp) error {
	return fs.serve(op)
}
func (fs *protocolFS) ListXattr(_ context.Context, op *fuseops.ListXattrOp) error {
	return fs.serve(op)
}
func (fs *protocolFS) SetXattr(_ context.Context, op *fuseops.SetXattrOp) error {
	return fs.serve(op)
}
func (fs *protocolFS) Fallocate(_ context.Context, op *fuseops.FallocateOp) error {
	return fs.serve(op)
}
func (fs *protocolFS) SyncFS(_ context.Context, op *fuseops.SyncFSOp) error { return fs.serve(op) }
