package fuse

import (
	"context"
	"errors"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeUnmount is one unmount(2) call.
type fakeUnmount struct {
	dir   string
	flags int
}

// recordUnmounts replaces unmountSyscall until the test ends. result answers
// each call; nil succeeds. It returns the calls made so far.
func recordUnmounts(t *testing.T, result func(call int) error) func() []fakeUnmount {
	t.Helper()
	var mu sync.Mutex
	var calls []fakeUnmount
	old := unmountSyscall
	unmountSyscall = func(dir string, flags int) error {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, fakeUnmount{dir, flags})
		if result != nil {
			return result(len(calls))
		}
		return nil
	}
	t.Cleanup(func() { unmountSyscall = old })
	return func() []fakeUnmount {
		mu.Lock()
		defer mu.Unlock()
		return append([]fakeUnmount(nil), calls...)
	}
}

// cannedMount is a native mount that has succeeded, over a canned transport.
type cannedMount struct {
	testTransport
	kind Backend
}

func (m *cannedMount) backend() Backend   { return m.kind }
func (m *cannedMount) waitMounted() error { return nil }
func (m *cannedMount) cancel()            {}

func TestFailedMountForceUnmountsTheNativeMount(t *testing.T) {
	unmounts := recordUnmounts(t, nil)
	cfg, err := (&MountConfig{}).normalize()
	if err != nil {
		t.Fatal(err)
	}
	// The native mount succeeded, but the kernel hangs up before INIT.
	native := &cannedMount{kind: testBackend}
	const dir = "/mnt/unserved"
	_, err = finishMount(dir, nil, &cfg, native)
	if err == nil || !strings.HasPrefix(err.Error(), "newConnection: ") {
		t.Fatalf("finishMount error = %v, want a newConnection error", err)
	}
	want := []fakeUnmount{{dir, testForceUnmountFlag}}
	if got := unmounts(); len(got) != 1 || got[0] != want[0] {
		t.Errorf("unmount calls = %v, want %v", got, want)
	}
}

func TestFailedMountLeavesADevFdMountPoint(t *testing.T) {
	unmounts := recordUnmounts(t, nil)
	cfg, err := (&MountConfig{}).normalize()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := finishMount("/dev/fd/3", nil, &cfg, &cannedMount{kind: testBackend}); err == nil {
		t.Fatal("finishMount succeeded without INIT")
	}
	if got := unmounts(); len(got) != 0 {
		t.Errorf("unmount calls = %v, want none: the device's owner unmounts it", got)
	}
}

func TestUnmountRetriesWhileBusy(t *testing.T) {
	unmounts := recordUnmounts(t, func(call int) error {
		if call < 3 {
			return syscall.EBUSY
		}
		return nil
	})
	mfs := &MountedFileSystem{dir: "/mnt/busy", backend: testBackend, joinStatusAvailable: make(chan struct{})}
	if err := mfs.Unmount(context.Background()); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	got := unmounts()
	if len(got) != 3 {
		t.Fatalf("unmount calls = %v, want 3", got)
	}
	for _, call := range got {
		if call.flags == testForceUnmountFlag {
			t.Errorf("unmount call %v forced a mount whose server still runs", call)
		}
	}
}

func TestUnmountGivesUpWhenContextEnds(t *testing.T) {
	recordUnmounts(t, func(int) error { return syscall.EBUSY })
	mfs := &MountedFileSystem{dir: "/mnt/busy", backend: testBackend, joinStatusAvailable: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 3*unmountRetryInterval)
	defer cancel()
	err := mfs.Unmount(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, syscall.EBUSY) {
		t.Errorf("Unmount error = %v, want one that wraps context.DeadlineExceeded and EBUSY", err)
	}
}

func TestUnmountForcesOnceJoinHasReturned(t *testing.T) {
	unmounts := recordUnmounts(t, nil)
	mfs := &MountedFileSystem{dir: "/mnt/dead", backend: testBackend, joinStatusAvailable: make(chan struct{})}
	// An error fatal to the connection ended serving.
	mfs.joinStatus = errors.New("connection-fatal")
	close(mfs.joinStatusAvailable)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := mfs.Unmount(ctx); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	want := fakeUnmount{"/mnt/dead", testForceUnmountFlag}
	if got := unmounts(); len(got) != 1 || got[0] != want {
		t.Errorf("unmount calls = %v, want [%v]", got, want)
	}
}
