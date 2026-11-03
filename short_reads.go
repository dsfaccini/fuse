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
	"encoding/binary"
	"fmt"
	"syscall"
	"unsafe"

	"github.com/dsfaccini/fuse/fuseops"
	"github.com/dsfaccini/fuse/internal/fusekernel"
)

// reportedInode is what the connection keeps for an inode the kernel holds:
// the size last reported for it, and its lookup count, which each entry reply
// raises by one and each forget lowers by its N.
type reportedInode struct {
	size    uint64
	lookups uint64
}

// trackReportedSizes records the size in every inode's attributes that the
// reply to state.op carries, extends a recorded size past the end of a write,
// and counts the lookups entry replies give the kernel, dropping an inode once
// forgets bring its count to zero. It returns the error to reply with: EIO for
// a ReadFileOp reply that checkReadLength rejects, otherwise opErr.
//
// LOCKS_EXCLUDED(c.mu)
func (c *Connection) trackReportedSizes(state opState, opErr error) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch o := state.op.(type) {
	case *fuseops.ForgetInodeOp:
		c.forgetLookups(o.Inode, o.N)
		return opErr
	case *fuseops.BatchForgetOp:
		for _, e := range o.Entries {
			c.forgetLookups(e.Inode, e.N)
		}
		return opErr
	}

	if opErr != nil {
		return opErr
	}

	switch o := state.op.(type) {
	case *fuseops.LookUpInodeOp:
		c.recordEntry(o.Entry.Child, o.Entry.Attributes.Size)
	case *fuseops.GetInodeAttributesOp:
		c.recordSize(o.Inode, o.Attributes.Size)
	case *fuseops.SetInodeAttributesOp:
		c.recordSize(o.Inode, o.Attributes.Size)
	case *fuseops.MkDirOp:
		c.recordEntry(o.Entry.Child, o.Entry.Attributes.Size)
	case *fuseops.MkNodeOp:
		c.recordEntry(o.Entry.Child, o.Entry.Attributes.Size)
	case *fuseops.CreateFileOp:
		c.recordEntry(o.Entry.Child, o.Entry.Attributes.Size)
	case *fuseops.CreateSymlinkOp:
		c.recordEntry(o.Entry.Child, o.Entry.Attributes.Size)
	case *fuseops.CreateLinkOp:
		c.recordEntry(o.Entry.Child, o.Entry.Attributes.Size)
	case *fuseops.ReadDirPlusOp:
		c.recordDirentPlusEntries(o.Dst[:o.BytesRead])
	case *fuseops.WriteFileOp:
		end := uint64(o.Offset) + uint64(len(o.Data))
		if end > c.reportedInodes[o.Inode].size {
			c.recordSize(o.Inode, end)
		}
	case *fuseops.ReadFileOp:
		return c.checkReadLength(o, state.sizeAtRead)
	}
	return nil
}

// recordEntry records the size an entry reply reported for inode, and the
// lookup the reply gives the kernel. Inode 0 is a negative entry, which gives
// none.
//
// LOCKS_REQUIRED(c.mu)
func (c *Connection) recordEntry(inode fuseops.InodeID, size uint64) {
	if inode == 0 {
		return
	}
	c.reportedInodes[inode] = reportedInode{size: size, lookups: c.reportedInodes[inode].lookups + 1}
}

// recordSize records size for an inode the kernel holds.
//
// LOCKS_REQUIRED(c.mu)
func (c *Connection) recordSize(inode fuseops.InodeID, size uint64) {
	if r, ok := c.reportedInodes[inode]; ok {
		r.size = size
		c.reportedInodes[inode] = r
	}
}

// LOCKS_REQUIRED(c.mu)
func (c *Connection) forgetLookups(inode fuseops.InodeID, n uint64) {
	r, ok := c.reportedInodes[inode]
	switch {
	case !ok:
	case r.lookups <= n:
		delete(c.reportedInodes, inode)
	default:
		r.lookups -= n
		c.reportedInodes[inode] = r
	}
}

// recordDirentPlusEntries records each fuse_direntplus entry of a READDIRPLUS
// reply as an entry reply. Like the kernel, it skips "." and "..", and stops
// at an entry that does not fit.
//
// LOCKS_REQUIRED(c.mu)
func (c *Connection) recordDirentPlusEntries(b []byte) {
	const (
		entryOutSize  = int(unsafe.Sizeof(fusekernel.EntryOut{}))
		headerSize    = entryOutSize + fusekernel.DirentSize
		sizeOffset    = unsafe.Offsetof(fusekernel.EntryOut{}.Attr) + unsafe.Offsetof(fusekernel.Attr{}.Size)
		namelenOffset = entryOutSize + int(unsafe.Offsetof(fusekernel.Dirent{}.Namelen))
		alignment     = 8
	)
	for len(b) >= headerSize {
		namelen := int(binary.NativeEndian.Uint32(b[namelenOffset:]))
		n := (headerSize + namelen + alignment - 1) &^ (alignment - 1)
		if n > len(b) {
			return
		}

		if name := string(b[headerSize : headerSize+namelen]); name != "." && name != ".." {
			inode := fuseops.InodeID(binary.NativeEndian.Uint64(b))
			c.recordEntry(inode, binary.NativeEndian.Uint64(b[sizeOffset:]))
		}
		b = b[n:]
	}
}

// checkReadLength returns EIO when o's reply ends before both the end of the
// read and the inode's size: the larger of sizeAtRead, the size recorded when
// the read arrived, and the size recorded now.
//
// LOCKS_REQUIRED(c.mu)
func (c *Connection) checkReadLength(o *fuseops.ReadFileOp, sizeAtRead uint64) error {
	size := max(sizeAtRead, c.reportedInodes[o.Inode].size)
	offset := uint64(o.Offset)
	if offset >= size {
		return nil
	}

	want := min(uint64(o.Size), size-offset)
	if uint64(o.BytesRead) >= want {
		return nil
	}
	return fmt.Errorf(
		"read of inode %d at offset %d returned %d bytes, want %d for size %d: %w",
		o.Inode, o.Offset, o.BytesRead, want, size, syscall.EIO)
}

// reportedSize returns the size recorded for inode, or 0 when none is.
//
// LOCKS_EXCLUDED(c.mu)
func (c *Connection) reportedSize(inode fuseops.InodeID) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reportedInodes[inode].size
}
