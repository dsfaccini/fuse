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
	"maps"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

// fakeMFMount stands in for macFUSE's MFMount framework. Its functions are
// purego callbacks, so the FSKit transport makes its real purego calls, walks
// real iovecs and reads errno through libSystem, with no macFUSE and no
// mount. Its channel is a fakeKernel. One fake is active at a time.
type fakeMFMount struct {
	kernel *fakeKernel

	// mount runs as MFMount and returns its result. Nil answers INIT and
	// mounts. MFMount closes the channel itself when it fails, so a failing
	// mount hangs up the kernel.
	mount func() int32

	// unmounts records the unmount(2) calls that Mount and Unmount make while
	// the fake is active, and unmountResult answers them. Nil succeeds.
	unmounts      []fakeUnmount
	unmountResult func(dir string, flags int) error

	// sendResult turns a send of n bytes into MFChannelSendMessage's result
	// and errno. Nil reports all n bytes sent.
	sendResult func(n int) (int, syscall.Errno)

	mu                sync.Mutex
	initReply         []byte
	channelCreated    bool
	channelCloses     int
	channelInterrupts int
	channelRetains    int
	channelReleases   int
	released          chan struct{} // closed by the channel MFRelease that drops the last reference
	messages          map[uintptr]*fakeMessage
	lastMessage       uintptr
	unknownReleases   int
	pinner            runtime.Pinner
}

// fakeMessage is a live MFMessageRef.
type fakeMessage struct {
	buffers [][]byte
	iovecs  []syscall.Iovec
}

// fakeUnmount is one unmount(2) call.
type fakeUnmount struct {
	dir   string
	flags int
}

// fakeChannel is the MFChannelRef that the fake hands out.
const fakeChannel uintptr = 1

var activeFakeMFMount atomic.Pointer[fakeMFMount]

// newFakeMFMount makes a fake MFMount active until the test ends. Then it
// checks that the channel, if created, was released once, and that every
// message was released once.
func newFakeMFMount(t *testing.T) *fakeMFMount {
	t.Helper()
	f := &fakeMFMount{
		kernel:      newFakeKernel(),
		released:    make(chan struct{}),
		messages:    map[uintptr]*fakeMessage{},
		lastMessage: 0x1000,
	}
	if !activeFakeMFMount.CompareAndSwap(nil, f) {
		t.Fatal("another fake MFMount is active; tests that use one cannot run in parallel")
	}
	// purego runs Go code after a callback returns, before it reads errno.
	// That code can start a GC, whose stop-the-world waits with a timeout and
	// leaves ETIMEDOUT in errno. Keep the GC off so the errno a callback sets
	// reaches the transport. Fake-backed tests therefore never run the
	// transport under GC.
	gcPercent := debug.SetGCPercent(-1)
	t.Cleanup(func() { debug.SetGCPercent(gcPercent) })
	oldUnmount := unmountSyscall
	unmountSyscall = func(dir string, flags int) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.unmounts = append(f.unmounts, fakeUnmount{dir, flags})
		if f.unmountResult != nil {
			return f.unmountResult(dir, flags)
		}
		return nil
	}
	t.Cleanup(func() { unmountSyscall = oldUnmount })
	t.Cleanup(func() {
		f.mu.Lock()
		created := f.channelCreated
		f.mu.Unlock()
		if created {
			select {
			case <-f.released:
			case <-time.After(5 * time.Second):
				// Leave the fake active: a native call may still arrive.
				t.Error("the transport did not release the channel within 5s")
				return
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if created && f.channelReleases != 1+f.channelRetains {
			t.Errorf("channel MFRelease calls = %d, want 1 plus %d MFRetain calls", f.channelReleases, f.channelRetains)
		}
		if len(f.messages) != 0 {
			t.Errorf("%d messages never released", len(f.messages))
		}
		if f.unknownReleases != 0 {
			t.Errorf("%d MFRelease calls on a released or unknown message", f.unknownReleases)
		}
		f.pinner.Unpin()
		activeFakeMFMount.Store(nil)
	})
	return f
}

// answerInit sends INIT and waits for the reply, as FSKit does before MFMount
// returns. It reports whether the reply came.
func (f *fakeMFMount) answerInit() bool {
	f.kernel.Send(fakeInit()...)
	reply, ok := f.kernel.nextReply()
	f.mu.Lock()
	f.initReply = reply
	f.mu.Unlock()
	return ok
}

// transport returns an FSKit transport on a new channel of the fake, with a
// successful mount, and closes it when the test ends.
func (f *fakeMFMount) transport(t *testing.T) *fskitTransport {
	t.Helper()
	api := fakeMFMountAPI()
	channel, _, _ := purego.SyscallN(api.channelCreate)
	tr := newFSKitTransport(api, channel)
	tr.setMountResult(nil)
	t.Cleanup(func() { tr.Close() })
	return tr
}

// MountWithFakeMFMount calls MountContext with the fskit backend while f
// stands in for MFMount, so it mounts nothing. The mount gate scanner exempts
// it by name.
func MountWithFakeMFMount(ctx context.Context, t *testing.T, f *fakeMFMount, server Server, cfg MountConfig) (*MountedFileSystem, error) {
	t.Helper()
	if activeFakeMFMount.Load() != f {
		t.Fatal("MountWithFakeMFMount needs the active fake MFMount")
	}
	setFSKitTestSystem(t, fskitFrameworkPath, "26.0")
	oldLoader := fskitAPILoader
	fskitAPILoader = func() (*fskitAPI, error) { return fakeMFMountAPI(), nil }
	t.Cleanup(func() { fskitAPILoader = oldLoader })
	cfg.Options = maps.Clone(cfg.Options)
	if cfg.Options == nil {
		cfg.Options = map[string]string{}
	}
	cfg.Options["backend"] = "fskit"
	return MountContext(ctx, t.TempDir(), server, &cfg)
}

// ServeFakeFSKit mounts server through Mount and a fake MFMount, and returns
// the fake's kernel. When the test ends, the kernel hangs up, as an unmount
// does, and the transport must close and release the channel once. Package
// fuse_test uses it.
func ServeFakeFSKit(t *testing.T, server Server) *fakeKernel {
	t.Helper()
	f := newFakeMFMount(t)
	mfs, err := MountWithFakeMFMount(context.Background(), t, f, server, MountConfig{})
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	checkInitReply(t, f.initReply)
	t.Cleanup(func() {
		f.kernel.HangUp()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := mfs.Join(ctx); err != nil {
			t.Errorf("Join: %v", err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.channelCloses != 1 {
			t.Errorf("MFChannelClose calls = %d, want 1", f.channelCloses)
		}
	})
	return f.kernel
}

// fakeMFMountAPI returns the fake's functions. purego allows 2000 callbacks
// per process, so they are made once and act on the active fake.
var fakeMFMountAPI = sync.OnceValue(func() *fskitAPI {
	libSystem, err := purego.Dlopen("/usr/lib/libSystem.B.dylib", purego.RTLD_NOW)
	if err != nil {
		panic(err)
	}
	errnoLocation, err := purego.Dlsym(libSystem, "__error")
	if err != nil {
		panic(err)
	}
	setErrno := func(errno syscall.Errno) {
		p, _, _ := purego.SyscallN(errnoLocation)
		storeInt32(p, int32(errno))
	}

	return &fskitAPI{
		channelCreate: purego.NewCallback(func() uintptr {
			f := activeFakeMFMount.Load()
			f.mu.Lock()
			f.channelCreated = true
			f.mu.Unlock()
			return fakeChannel
		}),
		mount: purego.NewCallback(func(channel, mountPoint, options, quiet uintptr) uintptr {
			f := activeFakeMFMount.Load()
			result := int32(0)
			switch {
			case f.mount != nil:
				result = f.mount()
			case !f.answerInit():
				f.kernel.HangUp()
				result = FSKitMountResultUnexpectedFailure
			}
			// MFMountResult is a C int.
			return uintptr(uint32(result))
		}),
		channelCopyNextMessage: purego.NewCallback(func(channel uintptr) uintptr {
			f := activeFakeMFMount.Load()
			r := f.kernel.read()
			if r.errno != 0 {
				setErrno(r.errno)
				return 0
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			f.lastMessage++
			f.messages[f.lastMessage] = &fakeMessage{buffers: r.buffers}
			return f.lastMessage
		}),
		messageGetBodyBuffers: purego.NewCallback(func(message, out uintptr) uintptr {
			f := activeFakeMFMount.Load()
			f.mu.Lock()
			defer f.mu.Unlock()
			m := f.messages[message]
			m.iovecs = make([]syscall.Iovec, len(m.buffers))
			for i, b := range m.buffers {
				if len(b) != 0 {
					f.pinner.Pin(&b[0])
					m.iovecs[i].Base = &b[0]
					m.iovecs[i].SetLen(len(b))
				}
			}
			f.pinner.Pin(&m.iovecs[0])
			storeUintptr(out, uintptr(unsafe.Pointer(&m.iovecs[0])))
			return uintptr(len(m.iovecs))
		}),
		channelSendMessage: purego.NewCallback(func(channel, iovecs, count uintptr) uintptr {
			f := activeFakeMFMount.Load()
			var message []byte
			for _, v := range iovecsAt(iovecs, count) {
				if v.Len != 0 {
					message = append(message, unsafe.Slice(v.Base, v.Len)...)
				}
			}
			sent, errno := len(message), syscall.Errno(0)
			if f.sendResult != nil {
				sent, errno = f.sendResult(len(message))
			}
			if errno != 0 {
				setErrno(errno)
				return ^uintptr(0)
			}
			if sent == len(message) {
				f.kernel.replies <- message
			}
			return uintptr(sent)
		}),
		channelInterrupt: purego.NewCallback(func(channel uintptr) uintptr {
			f := activeFakeMFMount.Load()
			f.mu.Lock()
			f.channelInterrupts++
			f.mu.Unlock()
			f.kernel.Interrupt()
			return 1
		}),
		retain: purego.NewCallback(func(ref uintptr) uintptr {
			f := activeFakeMFMount.Load()
			if ref == fakeChannel {
				f.mu.Lock()
				f.channelRetains++
				f.mu.Unlock()
			}
			return ref
		}),
		channelClose: purego.NewCallback(func(channel uintptr) uintptr {
			f := activeFakeMFMount.Load()
			f.mu.Lock()
			f.channelCloses++
			f.mu.Unlock()
			f.kernel.HangUp()
			return 1
		}),
		release: purego.NewCallback(func(ref uintptr) uintptr {
			f := activeFakeMFMount.Load()
			f.mu.Lock()
			defer f.mu.Unlock()
			switch {
			case ref == fakeChannel:
				f.channelReleases++
				if f.channelReleases == 1+f.channelRetains {
					close(f.released)
				}
			case f.messages[ref] != nil:
				delete(f.messages, ref)
			default:
				f.unknownReleases++
			}
			return 0
		}),
	}
})

// The fake writes through addresses that purego hands it as uintptr.
// checkptr, which -race enables, cannot tell that they are valid.

//go:nocheckptr
func storeInt32(p uintptr, v int32) { *(*int32)(unsafe.Pointer(p)) = v }

//go:nocheckptr
func storeUintptr(p uintptr, v uintptr) { *(*uintptr)(unsafe.Pointer(p)) = v }

//go:nocheckptr
func iovecsAt(p uintptr, n uintptr) []syscall.Iovec {
	return unsafe.Slice((*syscall.Iovec)(unsafe.Pointer(p)), n)
}
