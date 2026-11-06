// Copyright 2015 Google Inc. All Rights Reserved.
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
	"github.com/dsfaccini/fuse"
	"github.com/dsfaccini/fuse/internal/fusekernel"
	"syscall"
	"unsafe"

	"github.com/dsfaccini/fuse/fuseops"
)

type DirentType uint32

const (
	DT_Unknown   DirentType = 0
	DT_Socket    DirentType = syscall.DT_SOCK
	DT_Link      DirentType = syscall.DT_LNK
	DT_File      DirentType = syscall.DT_REG
	DT_Block     DirentType = syscall.DT_BLK
	DT_Directory DirentType = syscall.DT_DIR
	DT_Char      DirentType = syscall.DT_CHR
	DT_FIFO      DirentType = syscall.DT_FIFO
)

// A struct representing an entry within a directory file, describing a child.
// See notes on fuseops.ReadDirOp and on WriteDirent for details.
type Dirent struct {
	// The (opaque) offset within the directory file of the entry following this
	// one. See notes on fuseops.ReadDirOp.Offset for details.
	Offset fuseops.DirOffset

	// The inode of the child file or directory, and its name within the parent.
	Inode fuseops.InodeID
	Name  string

	// The type of the child. The zero value (DT_Unknown) is legal, but means
	// that the kernel will need to call GetAttr when the type is needed.
	Type DirentType
}

// A struct representing an entry along with its attributes within a directory file,
// describing a child.
// See notes on fuseops.ReadDirPlusOp and on WriteDirentPlus for details.
type DirentPlus struct {
	// The basic directory entry information (offset, inode, name, type).
	Dirent Dirent

	// Detailed information about the child inode, including its attributes
	// (like size, mode, timestamps) and cache expiration times for both the
	// attributes and the name lookup.
	Entry fuseops.ChildInodeEntry
}

// Write the supplied directory entry into the given buffer in the format
// expected in fuseops.ReadFileOp.Data, returning the number of bytes written.
// Return zero if the entry would not fit.
func WriteDirent(buf []byte, d Dirent) (n int) {
	// We want to write bytes with the layout of fuse_dirent
	// (https://tinyurl.com/4k7y2h9r) in host order. The struct must be aligned
	// according to FUSE_DIRENT_ALIGN (https://tinyurl.com/3m3ewu7h), which
	// dictates 8-byte alignment.

	const direntAlignment = 8
	const direntSize = fusekernel.DirentSize

	// Compute the number of bytes of padding we'll need to maintain alignment
	// for the next entry.
	var padLen int
	if len(d.Name)%direntAlignment != 0 {
		padLen = direntAlignment - (len(d.Name) % direntAlignment)
	}

	// Do we have enough room?
	totalLen := direntSize + len(d.Name) + padLen
	if totalLen > len(buf) {
		return n
	}

	// Write the header.
	de := fusekernel.Dirent{
		Ino:     uint64(d.Inode),
		Off:     uint64(d.Offset),
		Namelen: uint32(len(d.Name)),
		Type:    uint32(d.Type),
	}

	n += copy(buf[n:], (*[direntSize]byte)(unsafe.Pointer(&de))[:])

	// Write the name afterward.
	n += copy(buf[n:], d.Name)

	// Add any necessary padding.
	if padLen != 0 {
		var padding [direntAlignment]byte
		n += copy(buf[n:], padding[:padLen])
	}

	return n
}

// Write the supplied directory entry with attributes into the given buffer in the format
// expected in fuseops.ReadDirPlusOp.Dst returning the number of bytes written.
// Return zero if the entry would not fit.
func WriteDirentPlus(buf []byte, d DirentPlus) (n int) {
	// We want to write bytes with the layout of fuse_direntplus in host order:
	// a fuse_entry_out, a fuse_dirent, then the name. The struct must be
	// aligned according to FUSE_DIRENT_ALIGN (https://tinyurl.com/3m3ewu7h),
	// which dictates 8-byte alignment. fusekernel.EntryOut has the host's
	// size: macFUSE's fuse_attr carries btime and flags, so its
	// fuse_entry_out is 16 bytes longer than Linux's.
	const entryOutSize = unsafe.Sizeof(fusekernel.EntryOut{})
	const direntPlusAlignment = 8
	const direntPlusHeaderSize = int(entryOutSize) + fusekernel.DirentSize

	// Compute the number of bytes of padding we'll need to maintain alignment
	// for the next entry.
	var padLen int
	if pad := len(d.Dirent.Name) % direntPlusAlignment; pad != 0 {
		padLen = direntPlusAlignment - pad
	}

	// Do we have enough room?
	totalLen := direntPlusHeaderSize + len(d.Dirent.Name) + padLen
	if totalLen > len(buf) {
		return 0
	}

	entryValid, entryValidNsec := fuse.ConvertExpirationTime(d.Entry.EntryExpiration)
	attrValid, attrValidNsec := fuse.ConvertExpirationTime(d.Entry.AttributesExpiration)
	var rdev uint32
	mode := fuse.ConvertGoMode(d.Entry.Attributes.Mode)
	if mode&(syscall.S_IFCHR|syscall.S_IFBLK) != 0 {
		rdev = d.Entry.Attributes.Rdev
	}

	// Write the header.
	entryOut := fusekernel.EntryOut{
		Nodeid:         uint64(d.Entry.Child),
		Generation:     uint64(d.Entry.Generation),
		EntryValid:     entryValid,
		AttrValid:      attrValid,
		EntryValidNsec: entryValidNsec,
		AttrValidNsec:  attrValidNsec,
		Attr: fusekernel.Attr{
			Ino:  uint64(d.Entry.Child),
			Size: d.Entry.Attributes.Size,
			// round up to the nearest 512 boundary (In POSIX a "block" is a unit of 512 bytes)
			Blocks:    (d.Entry.Attributes.Size + 512 - 1) / 512,
			Atime:     uint64(d.Entry.Attributes.Atime.UnixNano() / 1e9),
			Mtime:     uint64(d.Entry.Attributes.Mtime.UnixNano() / 1e9),
			Ctime:     uint64(d.Entry.Attributes.Ctime.UnixNano() / 1e9),
			AtimeNsec: uint32(d.Entry.Attributes.Atime.UnixNano() % 1e9),
			MtimeNsec: uint32(d.Entry.Attributes.Mtime.UnixNano() % 1e9),
			CtimeNsec: uint32(d.Entry.Attributes.Ctime.UnixNano() % 1e9),
			Mode:      mode,
			Nlink:     d.Entry.Attributes.Nlink,
			Rdev:      rdev,
			Uid:       d.Entry.Attributes.Uid,
			Gid:       d.Entry.Attributes.Gid,
		},
	}
	crtime := d.Entry.Attributes.Crtime.UnixNano()
	entryOut.Attr.SetCrtime(uint64(crtime/1e9), uint32(crtime%1e9))

	dirent := fusekernel.Dirent{
		Ino:     uint64(d.Dirent.Inode),
		Off:     uint64(d.Dirent.Offset),
		Namelen: uint32(len(d.Dirent.Name)),
		Type:    uint32(d.Dirent.Type),
	}

	n += copy(buf[n:], (*[entryOutSize]byte)(unsafe.Pointer(&entryOut))[:])
	n += copy(buf[n:], (*[fusekernel.DirentSize]byte)(unsafe.Pointer(&dirent))[:])

	// Write the name afterward.
	n += copy(buf[n:], d.Dirent.Name)

	// Add any necessary padding.
	if padLen != 0 {
		var padding [direntPlusAlignment]byte
		n += copy(buf[n:], padding[:padLen])
	}

	return n
}
