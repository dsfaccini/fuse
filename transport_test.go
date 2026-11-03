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
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/dsfaccini/fuse/internal/buffer"
	"github.com/dsfaccini/fuse/internal/fusekernel"
)

type testTransport struct {
	messages [][]byte
	reads    int
	writes   [][]byte
	writeErr error
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
	if t.writeErr != nil {
		return t.writeErr
	}
	t.writes = append(t.writes, append([]byte(nil), message...))
	return nil
}

func (t *testTransport) WritevMessage(parts [][]byte) error {
	if t.writeErr != nil {
		return t.writeErr
	}
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

// The kernel takes each write(2) to /dev/fuse as one whole message, so a short
// write must fail instead of sending the rest as a second message.
func TestFileTransportWritesWholeMessage(t *testing.T) {
	var fds [2]int
	if err := syscall.Pipe(fds[:]); err != nil {
		t.Fatal(err)
	}
	r := os.NewFile(uintptr(fds[0]), "pipe-read")
	defer r.Close()
	// os.NewFile keeps a non-blocking descriptor non-blocking, even through
	// Fd, so a write into a full pipe comes back short.
	if err := syscall.SetNonblock(fds[1], true); err != nil {
		t.Fatal(err)
	}
	w := os.NewFile(uintptr(fds[1]), "pipe-write")
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

	// Fill the pipe, then free one page so the next write is short.
	fill := make([]byte, 1<<16)
	for {
		_, err := syscall.Write(fds[1], fill)
		if err == syscall.EAGAIN {
			break
		}
		if err != nil {
			t.Fatalf("filling the pipe: %v", err)
		}
	}
	if _, err := io.ReadFull(r, make([]byte, os.Getpagesize())); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}

	// os.File.Write would wait for the pipe to drain and finish the message.
	// The deadline bounds that wait.
	if err := w.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	err := transport.WriteMessage(make([]byte, 1<<20))
	if err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("WriteMessage into a nearly full pipe = %v, want a short-write error", err)
	}
}

func makeTestInMessage(unique uint64) []byte {
	message := make([]byte, unsafe.Sizeof(fusekernel.InHeader{}))
	binary.NativeEndian.PutUint32(message[0:4], uint32(len(message)))
	binary.NativeEndian.PutUint64(message[8:16], unique)
	return message
}
