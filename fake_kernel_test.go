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
	"log"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/dsfaccini/fuse/internal/fusekernel"
)

// fakeKernel is the kernel end of a connection that tests drive without a
// mount. Send queues a request as the body buffers FSKit hands over, and
// NextReply returns the next message the connection sent. As a
// connectionTransport it stands in for /dev/fuse, read the way fileTransport
// reads it; on darwin it is also the channel behind the fake MFMount.
type fakeKernel struct {
	mu     sync.Mutex
	cond   *sync.Cond
	reads  []fakeRead
	hungUp bool
	// interrupts counts Interrupt calls. A read fails with EINTR when it
	// changes while the read waits.
	interrupts int
	replies    chan []byte
}

// fakeRead is one read of the channel: a request's body buffers, or an errno.
type fakeRead struct {
	buffers [][]byte
	errno   syscall.Errno
}

func newFakeKernel() *fakeKernel {
	k := &fakeKernel{replies: make(chan []byte, 64)}
	k.cond = sync.NewCond(&k.mu)
	return k
}

// Send queues a request.
func (k *fakeKernel) Send(buffers ...[]byte) { k.queue(fakeRead{buffers: buffers}) }

// SendErrno makes a read fail with errno.
func (k *fakeKernel) SendErrno(errno syscall.Errno) { k.queue(fakeRead{errno: errno}) }

func (k *fakeKernel) queue(r fakeRead) {
	k.mu.Lock()
	k.reads = append(k.reads, r)
	k.cond.Broadcast()
	k.mu.Unlock()
}

// HangUp makes every read after the queued ones fail with ENODEV, as an
// unmount does.
func (k *fakeKernel) HangUp() {
	k.mu.Lock()
	k.hungUp = true
	k.cond.Broadcast()
	k.mu.Unlock()
}

// Interrupt makes the reads waiting now fail with EINTR, as
// MFChannelInterrupt does. A later read waits as usual.
func (k *fakeKernel) Interrupt() {
	k.mu.Lock()
	k.interrupts++
	k.cond.Broadcast()
	k.mu.Unlock()
}

// read blocks until a read is queued, the kernel hangs up, or Interrupt runs.
// A queued read or a hang-up wins over an interrupt.
func (k *fakeKernel) read() fakeRead {
	k.mu.Lock()
	defer k.mu.Unlock()
	interrupts := k.interrupts
	for len(k.reads) == 0 && !k.hungUp && k.interrupts == interrupts {
		k.cond.Wait()
	}
	if len(k.reads) == 0 && !k.hungUp {
		return fakeRead{errno: syscall.EINTR}
	}
	if len(k.reads) == 0 {
		return fakeRead{errno: syscall.ENODEV}
	}
	r := k.reads[0]
	k.reads = k.reads[1:]
	return r
}

// nextReply waits up to five seconds for the next message the connection
// sent.
func (k *fakeKernel) nextReply() ([]byte, bool) {
	select {
	case reply := <-k.replies:
		return reply, true
	case <-time.After(5 * time.Second):
		return nil, false
	}
}

// NextReply returns the next message the connection sent.
func (k *fakeKernel) NextReply(t testing.TB) []byte {
	t.Helper()
	reply, ok := k.nextReply()
	if !ok {
		t.Fatal("the connection sent no reply within 5s")
	}
	return reply
}

// UnreadReplies returns the number of messages the connection sent that
// NextReply has not returned.
func (k *fakeKernel) UnreadReplies() int { return len(k.replies) }

func (k *fakeKernel) ReadMessage(p []byte) (int, error) { return readDevice(k, p) }

// Read reads the kernel the way a FUSE device does: one whole message, or an
// errno.
func (k *fakeKernel) Read(p []byte) (int, error) {
	r := k.read()
	if r.errno != 0 {
		return 0, &os.PathError{Op: "read", Path: "/dev/fuse", Err: r.errno}
	}
	message := bytes.Join(r.buffers, nil)
	if len(message) > len(p) {
		return 0, fmt.Errorf("FUSE message has %d bytes; transport buffer holds %d", len(message), len(p))
	}
	return copy(p, message), nil
}

func (k *fakeKernel) SendMessage(parts [][]byte) error {
	k.replies <- bytes.Join(parts, nil)
	return nil
}

func (k *fakeKernel) Close() error {
	k.HangUp()
	return nil
}

// NativeBytes returns a copy of v's bytes in the host's layout, the layout of
// the kernel's FUSE structs.
func NativeBytes[T any](v *T) []byte {
	return bytes.Clone(unsafe.Slice((*byte)(unsafe.Pointer(v)), unsafe.Sizeof(*v)))
}

// FakeRequest returns a request as the body buffers FSKit hands over: the
// header joined with body[0], then the rest of body. It sets h.Len.
func FakeRequest(h fusekernel.InHeader, body ...[]byte) [][]byte {
	h.Len = uint32(fusekernel.InHeaderSize)
	for _, b := range body {
		h.Len += uint32(len(b))
	}
	buffers := [][]byte{NativeBytes(&h)}
	if len(body) > 0 {
		buffers[0] = append(buffers[0], body[0]...)
		buffers = append(buffers, body[1:]...)
	}
	return buffers
}

// fakeInit is the INIT request a fake kernel sends first.
func fakeInit() [][]byte {
	return FakeRequest(
		fusekernel.InHeader{Opcode: fusekernel.OpInit, Unique: 1},
		NativeBytes(&fusekernel.InitIn{Major: 7, Minor: 19, MaxReadahead: 1 << 20}))
}

// checkInitReply checks that reply answers fakeInit successfully with
// protocol 7.19.
func checkInitReply(t testing.TB, reply []byte) {
	t.Helper()
	const outHeaderSize = int(unsafe.Sizeof(fusekernel.OutHeader{}))
	if len(reply) < outHeaderSize+8 {
		t.Fatalf("INIT reply has %d bytes", len(reply))
	}
	if got := binary.NativeEndian.Uint32(reply[0:4]); int(got) != len(reply) {
		t.Errorf("INIT reply header len = %d, sent %d bytes", got, len(reply))
	}
	if got := int32(binary.NativeEndian.Uint32(reply[4:8])); got != 0 {
		t.Errorf("INIT reply error = %d, want 0", got)
	}
	if got := binary.NativeEndian.Uint64(reply[8:16]); got != 1 {
		t.Errorf("INIT reply unique = %d, want 1", got)
	}
	out := reply[outHeaderSize:]
	if major, minor := binary.NativeEndian.Uint32(out[0:4]), binary.NativeEndian.Uint32(out[4:8]); major != 7 || minor != 19 {
		t.Errorf("INIT reply protocol = %d.%d, want 7.19", major, minor)
	}
}

// ServeFakeKernel answers INIT from a fake kernel, then serves the kernel's
// requests with server until the test ends. Package fuse_test uses it.
func ServeFakeKernel(t *testing.T, server Server) *fakeKernel {
	t.Helper()
	return ServeFakeKernelWithLoggers(t, server, nil, nil)
}

// ServeFakeKernelWithLoggers is ServeFakeKernel with the connection's error
// and debug loggers.
func ServeFakeKernelWithLoggers(t *testing.T, server Server, errorLogger, debugLogger *log.Logger) *fakeKernel {
	t.Helper()
	cfg, err := (&MountConfig{}).normalize()
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	k := newFakeKernel()
	k.Send(fakeInit()...)
	c, err := newConnection(cfg, debugLogger, errorLogger, nil, k)
	if err != nil {
		t.Fatalf("newConnection: %v", err)
	}
	checkInitReply(t, k.NextReply(t))
	done := make(chan struct{})
	go func() {
		server.ServeOps(c)
		c.close()
		close(done)
	}()
	t.Cleanup(func() {
		k.HangUp()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("ServeOps did not return within 5s of the hang-up")
		}
	})
	return k
}
