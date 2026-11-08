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
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/dsfaccini/fuse/internal/mounttest"
	"github.com/ebitengine/purego"
)

// TestMFChannelContract checks the fake MFMount against the channel calls of
// the installed MFMount framework, on a channel that MFMount never mounts.
// Only MFMount reaches macFUSE's mount service
// (https://github.com/macfuse/mount/blob/395aa1b3bb90e907c10d1cae03dcee314a2d2a8a/Mount/Bridge/MFMount.swift#L87-L107);
// the channel functions act on the channel object alone
// (https://github.com/macfuse/mount/blob/395aa1b3bb90e907c10d1cae03dcee314a2d2a8a/Mount/Channel/Channel.swift).
func TestMFChannelContract(t *testing.T) {
	t.Run("fake", func(t *testing.T) {
		newFakeMFMount(t)
		checkMFChannelContract(t, fakeMFMountAPI())
	})
	t.Run("MFMount", func(t *testing.T) {
		mounttest.SkipUnlessMFChannelEnabled(t)
		useInstalledMFMount(t)
		api, err := fskitAPILoader()
		if err != nil {
			t.Fatalf("loading MFMount: %v", err)
		}
		checkMFChannelContract(t, api)
	})
}

// checkMFChannelContract runs the channel calls in order on one new channel.
// The channel keeps its MFChannelCreate reference: MFMount traps on the
// release of the last reference to a channel it never opened, with SIGTRAP
// (https://github.com/dsfaccini/fuse/pull/32).
func checkMFChannelContract(t *testing.T, api *fskitAPI) {
	channel, _, _ := purego.SyscallN(api.channelCreate)
	if channel == 0 {
		t.Fatal("MFChannelCreate returned NULL")
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			purego.SyscallN(api.channelClose, channel)
		}
	})

	if ref, _, _ := purego.SyscallN(api.retain, channel); ref != channel {
		t.Errorf("MFRetain = %#x, want its argument %#x", ref, channel)
	}
	purego.SyscallN(api.release, channel)

	if ok, _, errno := purego.SyscallN(api.channelInterrupt, channel); byte(ok) == 0 {
		t.Errorf("MFChannelInterrupt with no read waiting failed: %v", callErrno(errno))
	}

	// An interrupt reaches only a read that already waits, so interrupt until
	// the read returns.
	read := startChannelRead(api, channel)
	interrupt := func() { purego.SyscallN(api.channelInterrupt, channel) }
	if r, ok := awaitChannelRead(read, interrupt); !ok {
		t.Fatal("MFChannelInterrupt did not end a waiting MFChannelCopyNextMessage within 5s")
	} else if r.message != 0 || r.errno != syscall.EINTR {
		t.Errorf("interrupted MFChannelCopyNextMessage = %#x, %v; want NULL, EINTR", r.message, r.errno)
	}

	read = startChannelRead(api, channel)
	ok, _, errno := purego.SyscallN(api.channelClose, channel)
	closed = true
	if byte(ok) == 0 {
		t.Errorf("MFChannelClose failed: %v", callErrno(errno))
	}
	if r, ok := awaitChannelRead(read, func() {}); !ok {
		t.Fatal("MFChannelClose did not end a waiting MFChannelCopyNextMessage within 5s")
	} else if r.message != 0 || r.errno != syscall.ENODEV {
		t.Errorf("MFChannelCopyNextMessage ended by MFChannelClose = %#x, %v; want NULL, ENODEV", r.message, r.errno)
	}

	if message, _, errno := purego.SyscallN(api.channelCopyNextMessage, channel); message != 0 || callErrno(errno) != syscall.ENODEV {
		t.Errorf("MFChannelCopyNextMessage after MFChannelClose = %#x, %v; want NULL, ENODEV", message, callErrno(errno))
	}
	payload := []byte("message")
	var pinner runtime.Pinner
	defer pinner.Unpin()
	pinner.Pin(&payload[0])
	vector := syscall.Iovec{Base: &payload[0]}
	vector.SetLen(len(payload))
	if sent, _, errno := purego.SyscallN(api.channelSendMessage, channel, uintptr(unsafe.Pointer(&vector)), 1); int(sent) != -1 || callErrno(errno) != syscall.ENODEV {
		t.Errorf("MFChannelSendMessage after MFChannelClose = %d, %v; want -1, ENODEV", int(sent), callErrno(errno))
	}
	if ok, _, errno := purego.SyscallN(api.channelInterrupt, channel); byte(ok) != 0 || callErrno(errno) != syscall.ENODEV {
		t.Errorf("MFChannelInterrupt after MFChannelClose = %v, %v; want false, ENODEV", byte(ok) != 0, callErrno(errno))
	}
}

// channelRead is what one MFChannelCopyNextMessage returned.
type channelRead struct {
	message uintptr
	errno   syscall.Errno
}

func startChannelRead(api *fskitAPI, channel uintptr) <-chan channelRead {
	read := make(chan channelRead, 1)
	go func() {
		message, _, errno := purego.SyscallN(api.channelCopyNextMessage, channel)
		if message != 0 {
			purego.SyscallN(api.release, message)
		}
		read <- channelRead{message, callErrno(errno)}
	}()
	return read
}

// awaitChannelRead calls poke every millisecond until read delivers, for up
// to 5s.
func awaitChannelRead(read <-chan channelRead, poke func()) (channelRead, bool) {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		poke()
		select {
		case r := <-read:
			return r, true
		case <-time.After(time.Millisecond):
		}
	}
	return channelRead{}, false
}
