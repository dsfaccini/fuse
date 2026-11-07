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
	"context"
	"log"
	"syscall"
	"testing"
	"time"

	"github.com/dsfaccini/fuse"
	"github.com/dsfaccini/fuse/fuseops"
	"github.com/dsfaccini/fuse/fuseutil"
	"github.com/dsfaccini/fuse/internal/fusekernel"
)

// heldReadFS holds each read until the read's context ends, then answers
// EINTR.
type heldReadFS struct {
	fuseutil.NotImplementedFileSystem
	held    chan struct{}
	release chan struct{}
}

func (fs *heldReadFS) ReadFile(ctx context.Context, op *fuseops.ReadFileOp) error {
	close(fs.held)
	select {
	case <-ctx.Done():
		return syscall.EINTR
	case <-fs.release:
		return syscall.EIO
	}
}

// An op's context ends when the kernel interrupts its request, and when
// MountConfig.OpContext ends.
func TestCancelledOpIsAnsweredWithEINTR(t *testing.T) {
	tests := []struct {
		name   string
		cancel func(k fakeKernel, cancelOpContext context.CancelFunc)
	}{
		// Linux sends FUSE_INTERRUPT with the request's unique plus
		// FUSE_INT_REQ_BIT.
		{"interrupt", func(k fakeKernel, _ context.CancelFunc) {
			k.Send(fuse.FakeRequest(fusekernel.InHeader{Opcode: fusekernel.OpInterrupt, Unique: reqUnique | 1},
				fuse.NativeBytes(&fusekernel.InterruptIn{Unique: reqUnique}))...)
		}},
		{"OpContext", func(_ fakeKernel, cancelOpContext context.CancelFunc) { cancelOpContext() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opContext, cancelOpContext := context.WithCancel(context.Background())
			defer cancelOpContext()
			lines := make(logLines, 10)
			fs := &heldReadFS{held: make(chan struct{}), release: make(chan struct{})}
			k := fuse.ServeFakeKernelWithConfig(t, fuseutil.NewFileSystemServer(fs), &fuse.MountConfig{
				OpContext:   opContext,
				ErrorLogger: log.New(lines, "", 0),
			})
			// Registered last, so it runs before the hang-up waits for the read.
			t.Cleanup(func() { close(fs.release) })

			k.Send(request(fusekernel.OpRead, child, fuse.NativeBytes(&fusekernel.ReadIn{Fh: uint64(handle), Size: 4096}))...)
			select {
			case <-fs.held:
			case <-time.After(5 * time.Second):
				t.Fatal("the file system got no read within 5s")
			}
			tt.cancel(k, cancelOpContext)
			checkReply(t, k.NextReply(t), syscall.EINTR, nil)
			checkNoMoreLogs(t, lines)
		})
	}
}
