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

// Mount hands the connection the FSKit transport itself, so the connection
// sees that it pads short reads and answers a short READ with EIO.
func TestShortReadThroughFakeFSKit(t *testing.T) {
	served := make(chan struct{}, 2)
	k := fuse.ServeFakeFSKit(t, fuseutil.NewFileSystemServer(&protocolFS{serve: func(op any) error {
		defer func() { served <- struct{}{} }()
		switch o := op.(type) {
		case *fuseops.LookUpInodeOp:
			o.Entry = fuseops.ChildInodeEntry{Child: child, Attributes: fuseops.InodeAttributes{Size: 200, Nlink: 1, Mode: 0o644}}
		case *fuseops.ReadFileOp:
			o.BytesRead = 46
		}
		return nil
	}}))
	k.Send(request(fusekernel.OpLookup, parent, names("file"))...)
	<-served
	k.NextReply(t)
	k.Send(request(fusekernel.OpRead, child, fuse.NativeBytes(&fusekernel.ReadIn{Fh: uint64(handle), Size: 512}))...)
	<-served
	checkReply(t, k.NextReply(t), syscall.EIO, nil)
}
