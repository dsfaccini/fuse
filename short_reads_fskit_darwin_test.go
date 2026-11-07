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
	"syscall"
	"testing"

	"github.com/dsfaccini/fuse"
	"github.com/dsfaccini/fuse/fuseops"
	"github.com/dsfaccini/fuse/fuseutil"
	"github.com/dsfaccini/fuse/internal/fusekernel"
)

// The size tracker parses the entries fuseutil.WriteDirentPlus writes,
// through Mount, the FSKit transport and the fake MFMount.
func TestReadDirPlusThroughFakeFSKitRecordsWrittenEntries(t *testing.T) {
	const other fuseops.InodeID = 9
	served := make(chan struct{}, 4)
	k := fuse.ServeFakeFSKit(t, fuseutil.NewFileSystemServer(&protocolFS{serve: func(op any) error {
		defer func() { served <- struct{}{} }()
		switch o := op.(type) {
		case *fuseops.ReadDirPlusOp:
			for i, e := range []struct {
				inode fuseops.InodeID
				name  string
				size  uint64
			}{{parent, ".", 4096}, {1, "..", 4096}, {child, "file", 200}, {other, "a-longer-name-xyz", 300}} {
				o.BytesRead += fuseutil.WriteDirentPlus(o.Dst[o.BytesRead:], fuseutil.DirentPlus{
					Dirent: fuseutil.Dirent{Offset: fuseops.DirOffset(i + 1), Inode: e.inode, Name: e.name, Type: fuseutil.DT_File},
					Entry:  fuseops.ChildInodeEntry{Child: e.inode, Attributes: fuseops.InodeAttributes{Size: e.size, Nlink: 1, Mode: 0o644}},
				})
			}
		case *fuseops.ReadFileOp:
			o.BytesRead = copy(o.Dst, make([]byte, 46))
		}
		return nil
	}}))
	k.Send(request(fusekernel.OpReaddirplus, parent, fuse.NativeBytes(&fusekernel.ReadIn{Fh: uint64(handle), Size: 4096}))...)
	<-served
	k.NextReply(t)
	for _, inode := range []fuseops.InodeID{child, other} {
		k.Send(request(fusekernel.OpRead, inode, fuse.NativeBytes(&fusekernel.ReadIn{Fh: uint64(handle), Size: 512}))...)
		<-served
		checkReply(t, k.NextReply(t), syscall.EIO, nil)
	}
	// "." takes no lookup, so parent has no record and its short read passes.
	k.Send(request(fusekernel.OpRead, parent, fuse.NativeBytes(&fusekernel.ReadIn{Fh: uint64(handle), Size: 512}))...)
	<-served
	checkReply(t, k.NextReply(t), 0, make([]byte, 46))
}
