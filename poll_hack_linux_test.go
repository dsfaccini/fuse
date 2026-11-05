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
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/dsfaccini/fuse/fuseops"
	"github.com/dsfaccini/fuse/internal/fusekernel"
	"github.com/dsfaccini/fuse/internal/mounttest"
)

// pollCountingServer serves a root directory that holds one empty file, "f",
// and counts the FUSE_POLL requests it reads.
type pollCountingServer struct {
	polls atomic.Int32
}

func (s *pollCountingServer) ServeOps(c *Connection) {
	for {
		ctx, op, err := c.ReadOp()
		if err != nil {
			return
		}
		c.Reply(ctx, s.handle(op))
	}
}

func (s *pollCountingServer) handle(op interface{}) error {
	file := fuseops.InodeAttributes{Nlink: 1, Mode: 0444}
	switch o := op.(type) {
	case *fuseops.LookUpInodeOp:
		if o.Parent != fuseops.RootInodeID || o.Name != "f" {
			return syscall.ENOENT
		}
		o.Entry = fuseops.ChildInodeEntry{Child: 2, Attributes: file}
	case *fuseops.GetInodeAttributesOp:
		o.Attributes = file
		if o.Inode == fuseops.RootInodeID {
			o.Attributes = fuseops.InodeAttributes{Nlink: 1, Mode: os.ModeDir | 0555}
		}
	case *fuseops.OpenFileOp, *fuseops.FlushFileOp, *fuseops.ReleaseFileHandleOp,
		*fuseops.ForgetInodeOp, *fuseops.BatchForgetOp:
	case *unknownOp:
		if o.OpCode == fusekernel.OpPoll {
			s.polls.Add(1)
		}
		return syscall.ENOSYS
	default:
		return syscall.ENOSYS
	}
	return nil
}

// os.Open registers a file with the runtime's epoll, and for a FUSE file the
// kernel sends FUSE_POLL until the server answers one with ENOSYS. Mount
// answers that first FUSE_POLL itself, so the file system never reads one.
func TestMountAnswersFirstPoll(t *testing.T) {
	mounttest.SkipUnlessEnabled(t)

	dir := t.TempDir()
	server := &pollCountingServer{}
	mfs, err := Mount(dir, server, &MountConfig{})
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	defer func() {
		if err := Unmount(dir); err != nil {
			t.Errorf("Unmount: %v", err)
		}
		if err := mfs.Join(context.Background()); err != nil {
			t.Errorf("Join: %v", err)
		}
	}()

	f, err := os.Open(filepath.Join(dir, "f"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if n := server.polls.Load(); n != 0 {
		t.Errorf("the file system read %d FUSE_POLL requests, want 0", n)
	}
}
