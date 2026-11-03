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
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"testing"
	"unsafe"

	"github.com/jacobsa/fuse/internal/buffer"
	"github.com/jacobsa/fuse/internal/fusekernel"
)

type testTransport struct {
	messages [][]byte
	reads    int
	writes   [][]byte
}

func (t *testTransport) ReadMessage(dst []byte) (int, error) {
	if t.reads == len(t.messages) {
		return 0, io.EOF
	}
	message := t.messages[t.reads]
	t.reads++
	if len(dst) < len(message) {
		return 0, fmt.Errorf("destination is too small")
	}
	return copy(dst, message), nil
}

func (t *testTransport) WriteMessage(message []byte) error {
	t.writes = append(t.writes, append([]byte(nil), message...))
	return nil
}

func (t *testTransport) WritevMessage(parts [][]byte) error {
	var message []byte
	for _, part := range parts {
		message = append(message, part...)
	}
	t.writes = append(t.writes, message)
	return nil
}

func (t *testTransport) Close() error { return nil }

func TestConnectionTransportReadsWholeMessages(t *testing.T) {
	oldFuseT := fusekernel.IsPlatformFuseT
	fusekernel.IsPlatformFuseT = false
	t.Cleanup(func() { fusekernel.IsPlatformFuseT = oldFuseT })

	messageA := makeTestInMessage(101)
	messageB := makeTestInMessage(202)
	transport := &testTransport{messages: [][]byte{messageA, messageB}}

	for i, wantUnique := range []uint64{101, 202} {
		inMessage := buffer.NewInMessage(len(messageA))
		if err := inMessage.Init(transportReader{transport: transport}); err != nil {
			t.Fatalf("Init message %d: %v", i, err)
		}
		if got := inMessage.Header().Unique; got != wantUnique {
			t.Errorf("message %d unique = %d, want %d", i, got, wantUnique)
		}
	}
	if transport.reads != 2 {
		t.Errorf("ReadMessage calls = %d, want 2", transport.reads)
	}
}

func TestConnectionTransportWritesWholeReplies(t *testing.T) {
	transport := &testTransport{}
	c := &Connection{transport: transport}

	var out buffer.OutMessage
	out.Reset()
	out.OutHeader().Unique = 77
	out.Append([]byte("payload-a"), []byte("payload-b"))
	want := append(append(append([]byte(nil), out.OutHeaderBytes()...), []byte("payload-a")...), []byte("payload-b")...)

	if err := c.writeOutMessage(&out); err != nil {
		t.Fatalf("writeOutMessage: %v", err)
	}
	if len(transport.writes) != 1 {
		t.Fatalf("WritevMessage calls = %d, want 1", len(transport.writes))
	}
	if !bytes.Equal(transport.writes[0], want) {
		t.Errorf("written reply = %x, want %x", transport.writes[0], want)
	}
}

func TestFileTransportWritesWholeMessage(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()

	transport := newFileTransport(w)
	want := []byte("reply")
	if err := transport.WriteMessage(want); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(r, got); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("read bytes = %q, want %q", got, want)
	}
}

func makeTestInMessage(unique uint64) []byte {
	message := make([]byte, unsafe.Sizeof(fusekernel.InHeader{}))
	binary.NativeEndian.PutUint32(message[0:4], uint32(len(message)))
	binary.NativeEndian.PutUint64(message[8:16], unique)
	return message
}
