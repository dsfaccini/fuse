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
// the size last reported for it, and its lookup count, which each reply that
// gives the kernel a lookup raises by one and each forget lowers by its N.
type reportedInode struct {
	size    uint64
	lookups uint64
}

// changesReportedSizes reports whether the reply to op reaches the kernel and
// can change a size record. Such a reply holds the write side of
// c.sizeReplyMu from trackReportedSizes, through its write, to
// shrinkReportedSizes. Otherwise a reply recorded and written between
// another's write and shrink would see that shrink lower its size, two
// overlapping replies could be recorded in one order and written in the
// other, and a size could reach the kernel between a ReadFileOp reply's check
// and its write.
//
// Forgets change records but write nothing, so they take no lock beyond c.mu.
// fuseutil answers ForgetInodeOp inline in its serve loop, which a forget
// waiting on sizeReplyMu would stall.
func changesReportedSizes(op interface{}) bool {
	switch op.(type) {
	case *fuseops.LookUpInodeOp, *fuseops.GetInodeAttributesOp, *fuseops.SetInodeAttributesOp,
		*fuseops.MkDirOp, *fuseops.MkNodeOp, *fuseops.CreateFileOp, *fuseops.CreateSymlinkOp,
		*fuseops.CreateLinkOp, *fuseops.ReadDirPlusOp, *fuseops.WriteFileOp:
		return true
	}
	return false
}

// trackReportedSizes runs before the reply to state.op is written. It counts
// the lookups the reply gives the kernel, creates a record for each inode an
// entry reply reports, records each size above the recorded one, and extends a
// recorded size past the end of a write. A forget lowers the lookup count by
// its N and drops the inode at zero. Sizes the reply reports below the
// recorded ones wait for shrinkReportedSizes. It returns the error to reply
// with: EIO for a ReadFileOp reply that checkReadLength rejects, otherwise
// opErr.
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
	case *fuseops.WriteFileOp:
		end := uint64(o.Offset) + uint64(len(o.Data))
		if r, ok := c.reportedInodes[o.Inode]; ok && end > r.size {
			r.size = end
			c.reportedInodes[o.Inode] = r
		}
		return nil
	case *fuseops.ReadFileOp:
		return c.checkReadLength(o, state.sizeAtRead)
	}

	forEachReportedSize(state.op, func(inode fuseops.InodeID, size uint64, lookup bool) {
		r, ok := c.reportedInodes[inode]
		if !ok && !lookup {
			return
		}
		if lookup {
			r.lookups++
		}
		if !ok || size > r.size {
			r.size = size
		}
		c.reportedInodes[inode] = r
	})
	return nil
}

// shrinkReportedSizes runs once the kernel has the reply to op. It lowers each
// recorded size the reply reported below it.
//
// LOCKS_EXCLUDED(c.mu)
func (c *Connection) shrinkReportedSizes(op interface{}, opErr error) {
	if opErr != nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	forEachReportedSize(op, func(inode fuseops.InodeID, size uint64, _ bool) {
		if r, ok := c.reportedInodes[inode]; ok && size < r.size {
			r.size = size
			c.reportedInodes[inode] = r
		}
	})
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

// forEachReportedSize calls f with each inode whose attributes the successful
// reply to op carries, the size they report, and whether the reply gives the
// kernel a lookup of the inode. Inode 0, a negative entry, is skipped.
func forEachReportedSize(op interface{}, f func(inode fuseops.InodeID, size uint64, lookup bool)) {
	entry := func(e *fuseops.ChildInodeEntry) {
		if e.Child != 0 {
			f(e.Child, e.Attributes.Size, true)
		}
	}
	switch o := op.(type) {
	case *fuseops.LookUpInodeOp:
		entry(&o.Entry)
	case *fuseops.GetInodeAttributesOp:
		f(o.Inode, o.Attributes.Size, false)
	case *fuseops.SetInodeAttributesOp:
		f(o.Inode, o.Attributes.Size, false)
	case *fuseops.MkDirOp:
		entry(&o.Entry)
	case *fuseops.MkNodeOp:
		entry(&o.Entry)
	case *fuseops.CreateFileOp:
		entry(&o.Entry)
	case *fuseops.CreateSymlinkOp:
		entry(&o.Entry)
	case *fuseops.CreateLinkOp:
		entry(&o.Entry)
	case *fuseops.ReadDirPlusOp:
		forEachDirentPlus(o.Dst[:o.BytesRead], func(inode fuseops.InodeID, size uint64) {
			if inode != 0 {
				f(inode, size, true)
			}
		})
	}
}

// forEachDirentPlus calls f with the inode and size of each fuse_direntplus
// entry of a READDIRPLUS reply. Like the kernel, it skips "." and "..", which
// give no lookup, and stops at an entry that does not fit.
func forEachDirentPlus(b []byte, f func(inode fuseops.InodeID, size uint64)) {
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
			f(fuseops.InodeID(binary.NativeEndian.Uint64(b)), binary.NativeEndian.Uint64(b[sizeOffset:]))
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
