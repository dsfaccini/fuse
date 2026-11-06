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

package fuseutil

import (
	"encoding/binary"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/dsfaccini/fuse/fuseops"
)

// The kernel's struct fuse_direntplus, as these sources read and write it:
//   - Linux include/uapi/linux/fuse.h and parse_dirplusfile in
//     https://github.com/torvalds/linux/blob/05441d697f7f9a927264f4058cbbc6a03dfcac9d/fs/fuse/readdir.c
//   - macFUSE include/fuse_kernel.h and fuse_add_direntry_plus in
//     https://github.com/macfuse/library/blob/85f89460c4962ae94a84e25037326b22efb7f0e0/lib/fuse_lowlevel.c
//
// macFUSE's fuse_attr adds btime, btimensec and flags_darwin under __APPLE__.
// That moves FUSE_NAME_OFFSET_DIRENTPLUS from 152 to 168.
var darwin = runtime.GOOS == "darwin"

func direntPlusNameOffset() int {
	if darwin {
		return 168
	}
	return 152
}

type kernelAttr struct {
	ino, size, blocks, atime, mtime, ctime, btime uint64
	atimensec, mtimensec, ctimensec, btimensec    uint32
	mode, nlink, uid, gid, rdev, blksize          uint32
}

type kernelDirentPlus struct {
	nodeid, generation, entryValid, attrValid uint64
	entryValidNsec, attrValidNsec             uint32
	attr                                      kernelAttr
	ino, off                                  uint64
	typ                                       uint32
	name                                      string
}

// fields reads native-endian integers in struct order.
type fields struct{ b []byte }

func (f *fields) u64() uint64 {
	v := binary.NativeEndian.Uint64(f.b)
	f.b = f.b[8:]
	return v
}

func (f *fields) u32() uint32 {
	v := binary.NativeEndian.Uint32(f.b)
	f.b = f.b[4:]
	return v
}

// parseDirentPlus reads buf as parse_dirplusfile does, and fails where the
// kernel answers EIO. It also fails on padding that fuse_add_direntry_plus
// would have zeroed, and on bytes the kernel would skip.
func parseDirentPlus(t *testing.T, buf []byte) []kernelDirentPlus {
	t.Helper()
	nameOffset := direntPlusNameOffset()
	var entries []kernelDirentPlus
	for len(buf) >= nameOffset {
		f := fields{buf}
		var e kernelDirentPlus
		e.nodeid, e.generation, e.entryValid, e.attrValid = f.u64(), f.u64(), f.u64(), f.u64()
		e.entryValidNsec, e.attrValidNsec = f.u32(), f.u32()
		a := &e.attr
		a.ino, a.size, a.blocks, a.atime, a.mtime, a.ctime = f.u64(), f.u64(), f.u64(), f.u64(), f.u64(), f.u64()
		if darwin {
			a.btime = f.u64()
		}
		a.atimensec, a.mtimensec, a.ctimensec = f.u32(), f.u32(), f.u32()
		if darwin {
			a.btimensec = f.u32()
		}
		a.mode, a.nlink, a.uid, a.gid, a.rdev = f.u32(), f.u32(), f.u32(), f.u32(), f.u32()
		if darwin {
			f.u32() // flags_darwin
		}
		a.blksize = f.u32()
		f.u32() // flags
		e.ino, e.off = f.u64(), f.u64()
		namelen := int(f.u32())
		e.typ = f.u32()
		if at := len(buf) - len(f.b); at != nameOffset {
			t.Fatalf("the name starts at byte %d, want %d", at, nameOffset)
		}

		reclen := (nameOffset + namelen + 7) &^ 7
		if namelen == 0 || namelen > 1024 {
			t.Fatalf("namelen = %d; the kernel answers EIO", namelen)
		}
		if reclen > len(buf) {
			break
		}
		e.name = string(f.b[:namelen])
		for i, c := range buf[nameOffset+namelen : reclen] {
			if c != 0 {
				t.Errorf("%q: padding byte %d = %#x, want 0", e.name, i, c)
			}
		}
		entries = append(entries, e)
		buf = buf[reclen:]
	}
	if len(buf) != 0 {
		t.Errorf("the kernel skips the last %d bytes", len(buf))
	}
	return entries
}

func TestWriteDirentPlusMatchesKernelLayout(t *testing.T) {
	attrs := fuseops.InodeAttributes{
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
	wantAttr := func(ino uint64) kernelAttr {
		a := kernelAttr{
			ino: ino, size: 1000, blocks: 2,
			atime: 1, atimensec: 2, mtime: 3, mtimensec: 4, ctime: 5, ctimensec: 6,
			mode: syscall.S_IFREG | 0o644, nlink: 1, uid: 501, gid: 20,
		}
		if darwin {
			a.btime, a.btimensec = 7, 8
		}
		return a
	}

	buf := make([]byte, 4096)
	var n int
	var want []kernelDirentPlus
	for i, name := range []string{"a", "file", "seven77", "eight888", "nine99999"} {
		child := fuseops.InodeID(10 + i)
		d := DirentPlus{
			Dirent: Dirent{Offset: fuseops.DirOffset(i + 1), Inode: child, Name: name, Type: DT_File},
			Entry:  fuseops.ChildInodeEntry{Child: child, Generation: 9, Attributes: attrs},
		}
		reclen := (direntPlusNameOffset() + len(name) + 7) &^ 7
		if got := WriteDirentPlus(make([]byte, reclen-1), d); got != 0 {
			t.Errorf("%q into %d bytes: wrote %d, want 0", name, reclen-1, got)
		}
		got := WriteDirentPlus(buf[n:], d)
		if got != reclen {
			t.Errorf("%q: wrote %d bytes, want %d", name, got, reclen)
		}
		n += got
		want = append(want, kernelDirentPlus{
			nodeid: uint64(child), generation: 9, attr: wantAttr(uint64(child)),
			ino: uint64(child), off: uint64(i + 1), typ: syscall.DT_REG, name: name,
		})
	}

	got := parseDirentPlus(t, buf[:n])
	if len(got) != len(want) {
		t.Fatalf("parsed %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v\nwant %+v", i, got[i], want[i])
		}
	}
}
