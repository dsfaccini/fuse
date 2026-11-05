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
	"runtime"
	"testing"

	"github.com/dsfaccini/fuse/fuseops"
	"github.com/dsfaccini/fuse/internal/buffer"
	"github.com/dsfaccini/fuse/internal/fusekernel"
)

func convertTestMessage(t *testing.T, c *Connection, message []byte) (interface{}, *buffer.OutMessage, error) {
	t.Helper()
	c.transport = &testTransport{messages: [][]byte{message}}
	inMsg, err := c.readMessage()
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	outMsg := c.getOutMessage()
	op, err := convertInMessage(&c.cfg, inMsg, outMsg, c.protocol)
	return op, outMsg, err
}

// FuzzConvertInMessage checks that no request body panics the parser or
// grows the reply past MaxPages.
func FuzzConvertInMessage(f *testing.F) {
	opcodes := []uint32{
		fusekernel.OpLookup, fusekernel.OpForget, fusekernel.OpGetattr, fusekernel.OpSetattr,
		fusekernel.OpReadlink, fusekernel.OpSymlink, fusekernel.OpMknod, fusekernel.OpMkdir,
		fusekernel.OpUnlink, fusekernel.OpRmdir, fusekernel.OpRename, fusekernel.OpLink,
		fusekernel.OpOpen, fusekernel.OpRead, fusekernel.OpWrite, fusekernel.OpStatfs,
		fusekernel.OpRelease, fusekernel.OpFsync, fusekernel.OpSetxattr, fusekernel.OpGetxattr,
		fusekernel.OpListxattr, fusekernel.OpRemovexattr, fusekernel.OpFlush, fusekernel.OpInit,
		fusekernel.OpOpendir, fusekernel.OpReaddir, fusekernel.OpReleasedir, fusekernel.OpFsyncdir,
		fusekernel.OpCreate, fusekernel.OpInterrupt, fusekernel.OpBatchForget, fusekernel.OpFallocate,
		fusekernel.OpReaddirplus, fusekernel.OpSyncFS,
	}
	body := append(make([]byte, 64), "name\x00target\x00"...)
	for _, opcode := range opcodes {
		for _, minor := range []uint32{8, fusekernel.ProtoVersionMinMinor, fusekernel.ProtoVersionMaxMinor} {
			f.Add(opcode, minor, body)
		}
	}

	f.Fuzz(func(t *testing.T, opcode uint32, minor uint32, body []byte) {
		c := newTestConnection(nil)
		message := testMessage(opcode, 1, body)
		if len(message) > c.inMessageBufSize {
			t.Skip("message exceeds the request buffer")
		}
		c.protocol.Minor = minor
		_, outMsg, _ := convertTestMessage(t, c, message)
		if limit := buffer.OutMessageHeaderSize + int(max(maxReplySize(&c.cfg), xattrCapFloor(), readdirCapFloor())); outMsg.Len() > limit {
			t.Errorf("reply grew to %d bytes, want at most %d", outMsg.Len(), limit)
		}
	})
}

func TestConvertInMessageSizeZeroDirReadHasNoDst(t *testing.T) {
	in := fusekernel.ReadIn{Size: 0}
	for _, opcode := range []uint32{fusekernel.OpReaddir, fusekernel.OpReaddirplus} {
		c := newTestConnection(nil)
		op, outMsg, err := convertTestMessage(t, c, testMessage(opcode, 1, structBytes(&in)))
		if err != nil {
			t.Fatalf("opcode %d: convertInMessage: %v", opcode, err)
		}
		var dst []byte
		switch op := op.(type) {
		case *fuseops.ReadDirOp:
			dst = op.Dst
		case *fuseops.ReadDirPlusOp:
			dst = op.Dst
		}
		if len(dst) != 0 || outMsg.Len() != buffer.OutMessageHeaderSize {
			t.Errorf("opcode %d: len(Dst) = %d, reply = %d bytes; want 0 and the header", opcode, len(dst), outMsg.Len())
		}
	}
}

func TestConvertInMessageWriteDataHasRequestedSize(t *testing.T) {
	c := newTestConnection(nil)
	in := fusekernel.WriteIn{Size: 1}
	op, _, err := convertTestMessage(t, c, testMessage(fusekernel.OpWrite, 1, structBytes(&in), []byte("abc")))
	if err != nil {
		t.Fatalf("convertInMessage: %v", err)
	}
	if data := op.(*fuseops.WriteFileOp).Data; string(data) != "a" {
		t.Errorf("Data = %q, want %q", data, "a")
	}
}

// A count the body cannot hold must not size an allocation.
func TestConvertInMessageBoundsBatchForgetByBody(t *testing.T) {
	in := fusekernel.BatchForgetCountIn{Count: 1 << 22}
	c := newTestConnection(&testTransport{messages: [][]byte{testMessage(fusekernel.OpBatchForget, 1, structBytes(&in))}})
	inMsg, err := c.readMessage()
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	outMsg := c.getOutMessage()

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err = convertInMessage(&c.cfg, inMsg, outMsg, c.protocol)
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Error("convertInMessage succeeded, want an error")
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
		t.Errorf("allocated %d bytes for a batch with no entries", allocated)
	}
}
