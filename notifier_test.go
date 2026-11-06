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
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/dsfaccini/fuse/internal/fusekernel"
)

// startedServer drains ops like drainServer, after it closes started.
type startedServer struct{ started chan struct{} }

func newStartedServer() startedServer { return startedServer{started: make(chan struct{})} }

func (s startedServer) ServeOps(c *Connection) {
	close(s.started)
	drainServer{}.ServeOps(c)
}

func (s startedServer) wait(t *testing.T) {
	t.Helper()
	select {
	case <-s.started:
	case <-time.After(5 * time.Second):
		t.Fatal("ServeOps did not start within 5s")
	}
}

// notifyWithin runs notify and fails the test when it blocks for 5s.
func notifyWithin(t *testing.T, notify func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- notify() }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the notification blocked for 5s")
		return nil
	}
}

func TestNotifierReturnsENOTCONNWhileNothingServes(t *testing.T) {
	n := NewNotifier()
	check := func(when string) {
		t.Helper()
		if err := notifyWithin(t, func() error { return n.InvalidateInode(1, 0, 0) }); !errors.Is(err, syscall.ENOTCONN) {
			t.Errorf("InvalidateInode %s = %v, want ENOTCONN", when, err)
		}
		if err := notifyWithin(t, func() error { return n.InvalidateEntry(1, "x") }); !errors.Is(err, syscall.ENOTCONN) {
			t.Errorf("InvalidateEntry %s = %v, want ENOTCONN", when, err)
		}
	}
	check("before ServeOps")
	if err := ServeCanned(NewServerWithNotifier(n, drainServer{}), nil); err != nil {
		t.Fatalf("ServeCanned: %v", err)
	}
	check("after ServeOps returned")
}

func TestNotifierSendsWhileServing(t *testing.T) {
	n := NewNotifier()
	server := newStartedServer()
	k := ServeFakeKernel(t, NewServerWithNotifier(n, server))
	server.wait(t)

	if err := notifyWithin(t, func() error { return n.InvalidateInode(7, 0, 0) }); err != nil {
		t.Fatalf("InvalidateInode: %v", err)
	}
	reply := k.NextReply(t)
	code, unique := int32(binary.NativeEndian.Uint32(reply[4:8])), binary.NativeEndian.Uint64(reply[8:16])
	if code != fusekernel.NotifyCodeInvalInode || unique != 0 {
		t.Errorf("notification code = %d, unique = %d; want an inode invalidation", code, unique)
	}
}

// notifyBlockingKernel is a fake kernel whose notification writes block until
// release is closed. Replies go through.
type notifyBlockingKernel struct {
	*fakeKernel
	sending chan struct{}
	release chan struct{}
}

func (k *notifyBlockingKernel) SendMessage(parts [][]byte) error {
	// A notification's header carries unique 0.
	if binary.NativeEndian.Uint64(parts[0][8:16]) == 0 {
		close(k.sending)
		<-k.release
	}
	return k.fakeKernel.SendMessage(parts)
}

// ServeOps returns only after the notification in progress is sent, so that
// the connection is still open for it.
func TestNotifierServerWaitsForTheNotificationInProgress(t *testing.T) {
	cfg, err := (&MountConfig{}).normalize()
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	k := &notifyBlockingKernel{fakeKernel: newFakeKernel(), sending: make(chan struct{}), release: make(chan struct{})}
	k.Send(fakeInit()...)
	c, err := newConnection(cfg, nil, nil, nil, k)
	if err != nil {
		t.Fatalf("newConnection: %v", err)
	}
	defer c.close()
	checkInitReply(t, k.NextReply(t))

	n := NewNotifier()
	server := newStartedServer()
	served := make(chan struct{})
	go func() {
		NewServerWithNotifier(n, server).ServeOps(c)
		close(served)
	}()
	server.wait(t)
	notified := make(chan error, 1)
	go func() { notified <- n.InvalidateInode(7, 0, 0) }()
	select {
	case <-k.sending:
	case <-time.After(5 * time.Second):
		t.Fatal("the notification was not sent within 5s")
	}

	// The server stops while the notification write is blocked.
	k.HangUp()
	select {
	case <-served:
		t.Fatal("ServeOps returned while a notification was being sent")
	case <-time.After(100 * time.Millisecond):
	}

	close(k.release)
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("ServeOps did not return within 5s of the notification")
	}
	if err := <-notified; err != nil {
		t.Errorf("InvalidateInode = %v, want nil", err)
	}
}
