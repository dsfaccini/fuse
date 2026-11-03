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

// trackReportedSizes records the size in every inode's attributes that op's
// reply carries, extends a recorded size past the end of a write, and drops
// the sizes of forgotten inodes. It returns the error to reply with: EIO for a
// ReadFileOp reply that ends before the read or the recorded size does,
// otherwise opErr.
//
// LOCKS_EXCLUDED(c.mu)
func (c *Connection) trackReportedSizes(op interface{}, opErr error) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch o := op.(type) {
	case *fuseops.ForgetInodeOp:
		delete(c.reportedSizes, o.Inode)
		return opErr
	case *fuseops.BatchForgetOp:
		for _, e := range o.Entries {
			delete(c.reportedSizes, e.Inode)
		}
		return opErr
	}

	if opErr != nil {
		return opErr
	}

	switch o := op.(type) {
	case *fuseops.LookUpInodeOp:
		c.reportedSizes[o.Entry.Child] = o.Entry.Attributes.Size
	case *fuseops.GetInodeAttributesOp:
		c.reportedSizes[o.Inode] = o.Attributes.Size
	case *fuseops.SetInodeAttributesOp:
		c.reportedSizes[o.Inode] = o.Attributes.Size
	case *fuseops.MkDirOp:
		c.reportedSizes[o.Entry.Child] = o.Entry.Attributes.Size
	case *fuseops.MkNodeOp:
		c.reportedSizes[o.Entry.Child] = o.Entry.Attributes.Size
	case *fuseops.CreateFileOp:
		c.reportedSizes[o.Entry.Child] = o.Entry.Attributes.Size
	case *fuseops.CreateSymlinkOp:
		c.reportedSizes[o.Entry.Child] = o.Entry.Attributes.Size
	case *fuseops.CreateLinkOp:
		c.reportedSizes[o.Entry.Child] = o.Entry.Attributes.Size
	case *fuseops.ReadDirPlusOp:
		c.recordDirentPlusSizes(o.Dst[:o.BytesRead])
	case *fuseops.WriteFileOp:
		end := uint64(o.Offset) + uint64(len(o.Data))
		if size, ok := c.reportedSizes[o.Inode]; ok && end > size {
			c.reportedSizes[o.Inode] = end
		}
	case *fuseops.ReadFileOp:
		return c.checkReadLength(o)
	}
	return nil
}

// recordDirentPlusSizes records the size in each fuse_direntplus entry of a
// READDIRPLUS reply.
//
// LOCKS_REQUIRED(c.mu)
func (c *Connection) recordDirentPlusSizes(b []byte) {
	const (
		entryOutSize  = int(unsafe.Sizeof(fusekernel.EntryOut{}))
		headerSize    = entryOutSize + fusekernel.DirentSize
		sizeOffset    = unsafe.Offsetof(fusekernel.EntryOut{}.Attr) + unsafe.Offsetof(fusekernel.Attr{}.Size)
		namelenOffset = entryOutSize + int(unsafe.Offsetof(fusekernel.Dirent{}.Namelen))
		alignment     = 8
	)
	for len(b) >= headerSize {
		inode := fuseops.InodeID(binary.NativeEndian.Uint64(b))
		c.reportedSizes[inode] = binary.NativeEndian.Uint64(b[sizeOffset:])

		n := headerSize + int(binary.NativeEndian.Uint32(b[namelenOffset:]))
		n = (n + alignment - 1) &^ (alignment - 1)
		if n > len(b) {
			return
		}
		b = b[n:]
	}
}

// checkReadLength returns EIO when o's reply ends before both the end of the
// read and the size last recorded for o.Inode.
//
// LOCKS_REQUIRED(c.mu)
func (c *Connection) checkReadLength(o *fuseops.ReadFileOp) error {
	size, ok := c.reportedSizes[o.Inode]
	offset := uint64(o.Offset)
	if !ok || offset >= size {
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
