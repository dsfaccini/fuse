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
	"reflect"
	"syscall"
	"testing"
	"unsafe"

	"github.com/dsfaccini/fuse/fuseops"
	"github.com/dsfaccini/fuse/internal/fusekernel"
)

// newPollHackConnection returns a connection that has answered a fake
// kernel's INIT. The kernel hangs up once its queued requests run out.
func newPollHackConnection(t *testing.T) (*Connection, *fakeKernel) {
	t.Helper()
	cfg, err := (&MountConfig{}).normalize()
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	k := newFakeKernel()
	k.Send(fakeInit()...)
	c, err := newConnection(cfg, nil, nil, nil, k)
	if err != nil {
		t.Fatalf("newConnection: %v", err)
	}
	checkInitReply(t, k.NextReply(t))
	k.HangUp()
	return c, k
}

func nodeRequest(opcode uint32, unique, nodeid uint64, body ...[]byte) [][]byte {
	return FakeRequest(fusekernel.InHeader{Opcode: opcode, Unique: unique, Nodeid: nodeid}, body...)
}

func batchForget(unique uint64, inodes ...uint64) [][]byte {
	count := fusekernel.BatchForgetCountIn{Count: uint32(len(inodes))}
	body := NativeBytes(&count)
	for _, inode := range inodes {
		entry := fusekernel.BatchForgetEntryIn{Inode: int64(inode), Nlookup: 1}
		body = append(body, NativeBytes(&entry)...)
	}
	return nodeRequest(fusekernel.OpBatchForget, unique, 0, body)
}

func TestPollHackAnswersReservedEntry(t *testing.T) {
	hack := uint64(pollHackInode)
	var open fusekernel.OpenIn
	var getxattr fusekernel.GetxattrIn
	var flush fusekernel.FlushIn
	var release fusekernel.ReleaseIn
	var forget fusekernel.ForgetIn
	requests := [][][]byte{
		nodeRequest(fusekernel.OpLookup, 1, fuseops.RootInodeID, []byte(pollHackName+"\x00")),
		nodeRequest(fusekernel.OpGetattr, 2, hack),
		nodeRequest(fusekernel.OpOpen, 3, hack, NativeBytes(&open)),
		nodeRequest(fusekernel.OpPoll, 4, hack),
		nodeRequest(fusekernel.OpGetxattr, 5, hack, NativeBytes(&getxattr), []byte("user.x\x00")),
		nodeRequest(fusekernel.OpAccess, 6, hack),
		nodeRequest(fusekernel.OpFlush, 7, hack, NativeBytes(&flush)),
		nodeRequest(fusekernel.OpRelease, 8, hack, NativeBytes(&release)),
		nodeRequest(fusekernel.OpForget, 9, hack, NativeBytes(&forget)),
		batchForget(10, hack),
		nodeRequest(fusekernel.OpGetattr, 11, fuseops.RootInodeID),
	}
	c, k := newPollHackConnection(t)
	for _, r := range requests {
		k.Send(r...)
	}
	c.pollHackRunning.Store(true)

	_, op, err := c.ReadOp()
	if err != nil {
		t.Fatalf("ReadOp: %v", err)
	}
	if got, ok := op.(*fuseops.GetInodeAttributesOp); !ok || got.Inode != fuseops.RootInodeID {
		t.Fatalf("ReadOp = %#v, want the root's GetInodeAttributesOp", op)
	}

	// FORGET and BATCH_FORGET get no reply.
	want := []struct {
		unique uint64
		errno  syscall.Errno
	}{{1, 0}, {2, 0}, {3, 0}, {4, syscall.ENOSYS}, {5, syscall.ENODATA}, {6, syscall.EIO}, {7, 0}, {8, 0}}
	if n := k.UnreadReplies(); n != len(want) {
		t.Fatalf("replies = %d, want %d", n, len(want))
	}
	var lookUp []byte
	for i, w := range want {
		reply := k.NextReply(t)
		if i == 0 {
			lookUp = reply
		}
		unique := binary.NativeEndian.Uint64(reply[8:16])
		errno := -int32(binary.NativeEndian.Uint32(reply[4:8]))
		if unique != w.unique || errno != int32(w.errno) {
			t.Errorf("reply %d: unique %d, errno %d; want unique %d, errno %d", i, unique, errno, w.unique, w.errno)
		}
	}
	outHeaderSize := int(unsafe.Sizeof(fusekernel.OutHeader{}))
	if nodeid := binary.NativeEndian.Uint64(lookUp[outHeaderSize:]); nodeid != hack {
		t.Errorf("lookup reply nodeid = %d, want %d", nodeid, hack)
	}
}

func TestPollHackLeavesOtherRequestsToServer(t *testing.T) {
	hack := uint64(pollHackInode)
	name := []byte(pollHackName + "\x00")
	for _, test := range []struct {
		name    string
		running bool
		request [][]byte
		want    interface{}
	}{
		{
			name:    "lookup after the hack",
			request: nodeRequest(fusekernel.OpLookup, 1, fuseops.RootInodeID, name),
			want:    &fuseops.LookUpInodeOp{Parent: fuseops.RootInodeID, Name: pollHackName},
		},
		{
			name:    "lookup outside the root",
			running: true,
			request: nodeRequest(fusekernel.OpLookup, 1, 2, name),
			want:    &fuseops.LookUpInodeOp{Parent: 2, Name: pollHackName},
		},
		{
			name:    "reserved inode before its lookup",
			request: nodeRequest(fusekernel.OpGetattr, 1, hack),
			want:    &fuseops.GetInodeAttributesOp{Inode: pollHackInode},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, k := newPollHackConnection(t)
			k.Send(test.request...)
			c.pollHackRunning.Store(test.running)

			_, op, err := c.ReadOp()
			if err != nil {
				t.Fatalf("ReadOp: %v", err)
			}
			// The op context is not under test.
			switch o := op.(type) {
			case *fuseops.LookUpInodeOp:
				o.OpContext = fuseops.OpContext{}
			case *fuseops.GetInodeAttributesOp:
				o.OpContext = fuseops.OpContext{}
			}
			if !reflect.DeepEqual(op, test.want) {
				t.Errorf("ReadOp = %#v, want %#v", op, test.want)
			}
			if n := k.UnreadReplies(); n != 0 {
				t.Errorf("replies = %d, want 0", n)
			}
		})
	}
}

func TestPollHackDropsEntryFromBatchForget(t *testing.T) {
	hack := uint64(pollHackInode)
	c, k := newPollHackConnection(t)
	k.Send(batchForget(1, hack)...)
	k.Send(batchForget(2, hack, 5)...)
	c.pollHackEntryKnown = true

	_, op, err := c.ReadOp()
	if err != nil {
		t.Fatalf("ReadOp: %v", err)
	}
	forget, ok := op.(*fuseops.BatchForgetOp)
	if !ok || forget.OpContext.FuseID != 2 {
		t.Fatalf("ReadOp = %#v, want the second BatchForgetOp", op)
	}
	want := []fuseops.BatchForgetEntry{{Inode: 5, N: 1}}
	if !reflect.DeepEqual(forget.Entries, want) {
		t.Errorf("entries = %v, want %v", forget.Entries, want)
	}
}

func TestPollHackRequestUnderInFlightIDIsFatal(t *testing.T) {
	c, k := newPollHackConnection(t)
	k.Send(nodeRequest(fusekernel.OpGetattr, 7, fuseops.RootInodeID)...)
	k.Send(nodeRequest(fusekernel.OpLookup, 7, fuseops.RootInodeID, []byte(pollHackName+"\x00"))...)
	c.pollHackRunning.Store(true)

	if _, _, err := c.ReadOp(); err != nil {
		t.Fatalf("ReadOp: %v", err)
	}
	if _, _, err := c.ReadOp(); err == nil {
		t.Error("ReadOp answered a poll hack request under the ID of a request in flight")
	}
	if n := k.UnreadReplies(); n != 0 {
		t.Errorf("replies = %d, want 0", n)
	}
}
