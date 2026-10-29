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
//go:build darwin && cgo && fskit

package fuse

/*
#cgo darwin LDFLAGS: -framework MFMount
#include <errno.h>
#include <stdlib.h>
#include <MFMount/MFMount.h>

static MFChannelRef fskit_channel_create(void) {
    return MFChannelCreate();
}
static MFMountResult fskit_mount(
    MFChannelRef channel,
    const char *mountPoint,
    const char *options,
    bool quiet,
    int *error
) {
    MFMountResult result = MFMount(channel, mountPoint, options, quiet);
    *error = errno;
    return result;
}
static MFMessageRef fskit_channel_copy_next_message(MFChannelRef channel, int *error) {
    MFMessageRef message = MFChannelCopyNextMessage(channel);
    *error = errno;
    return message;
}
static ssize_t fskit_message_get_body_size(MFMessageRef message, int *error) {
    ssize_t size = MFMessageGetBodySize(message);
    *error = errno;
    return size;
}
static ssize_t fskit_message_get_body_buffers(
    MFMessageRef message,
    const struct iovec **buffers,
    int *error
) {
    ssize_t count = MFMessageGetBodyBuffers(message, buffers);
    *error = errno;
    return count;
}
static ssize_t fskit_channel_send_message(
    MFChannelRef channel,
    const struct iovec *buffers,
    size_t count,
    int *error
) {
    ssize_t sent = MFChannelSendMessage(channel, buffers, count);
    *error = errno;
    return sent;
}
static bool fskit_channel_close(MFChannelRef channel, int *error) {
    bool closed = MFChannelClose(channel);
    *error = errno;
    return closed;
}
*/
import "C"

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

type fskitTransport struct {
	mu                    sync.Mutex
	stateChanged          *sync.Cond
	channel               C.MFChannelRef
	closed                bool
	activeReaders         int
	receivedNativeMessage bool
	mountFinished         bool
	mountSucceeded        bool
	operations            sync.WaitGroup
	mountDone             sync.WaitGroup
	closeMu               sync.Mutex
	closeOnce             sync.Once
	releaseOnce           sync.Once
	closeErr              error
}

func mountFSKitTransport(
	dir string,
	cfg *MountConfig,
	ready chan<- error) (connectionTransport, error) {
	if err := validateFSKitMount(dir, cfg); err != nil {
		return nil, err
	}

	channel := C.fskit_channel_create()
	if channel == nil {
		return nil, fmt.Errorf("MFChannelCreate failed")
	}
	t := &fskitTransport{channel: channel}
	t.stateChanged = sync.NewCond(&t.mu)

	options := cfg.toMap()
	delete(options, "backend")
	mountOptions := C.CString(mapToOptionsString(options))
	mountPoint := C.CString(dir)
	t.mountDone.Add(1)
	go func() {
		defer t.mountDone.Done()
		defer C.free(unsafe.Pointer(mountOptions))
		defer C.free(unsafe.Pointer(mountPoint))

		var mountCallErr C.int
		result := C.fskit_mount(t.channel, mountPoint, mountOptions, true, &mountCallErr)
		var mountErr error
		switch result {
		case C.MFMountResultSuccess:
		case C.MFMountResultUnsupporteOSVersion:
			mountErr = fmt.Errorf("MFMount does not support this macOS version")
		case C.MFMountResultHelperToolsInstallationFailed:
			mountErr = fmt.Errorf("MFMount could not install its helper tools")
		case C.MFMountResultFileSystemExtensionNotFound:
			mountErr = fmt.Errorf("MFMount file system extension was not found")
		case C.MFMountResultFileSystemExtensionRequiresApproval:
			mountErr = fmt.Errorf("MFMount file system extension requires user approval")
		case C.MFMountResultCreatingMountPointFailed:
			mountErr = fmt.Errorf("MFMount could not create the mount point")
		case C.MFMountResultUnexpectedFailure:
			mountErr = fmt.Errorf("MFMount failed: %w", syscall.Errno(mountCallErr))
		default:
			mountErr = fmt.Errorf("MFMount failed with result %d", int32(result))
		}
		t.setMountResult(mountErr == nil)
		ready <- mountErr
	}()

	return t, nil
}

func validateFSKitMount(dir string, cfg *MountConfig) error {
	// macFUSE 5.4.0 checks OS availability before opening a channel. Match that
	// guard here so unsupported hosts never allocate an unopened native channel.
	version, err := syscall.Sysctl("kern.osproductversion")
	if err != nil {
		return fmt.Errorf("reading macOS version for FSKit: %w", err)
	}
	if err := validateFSKitOSVersion(version); err != nil {
		return err
	}

	if dir == "" {
		return fmt.Errorf("FSKit mount point must not be empty")
	}
	if cfg == nil {
		return fmt.Errorf("FSKit mount config must not be nil")
	}
	validation := Connection{cfg: *cfg}
	if err := validation.sanitizeMaxPagesAndWrite(); err != nil {
		return err
	}
	return nil
}

func validateFSKitOSVersion(version string) error {
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return fmt.Errorf("invalid macOS product version %q", version)
	}
	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])
	if majorErr != nil || minorErr != nil {
		return fmt.Errorf("invalid macOS product version %q", version)
	}
	if major < 15 || major == 15 && minor < 4 {
		return fmt.Errorf("FSKit backend requires macOS 15.4 or later; found %s", version)
	}
	return nil
}

func (t *fskitTransport) setMountResult(succeeded bool) {
	t.mu.Lock()
	t.mountFinished = true
	t.mountSucceeded = succeeded
	t.stateChanged.Broadcast()
	t.mu.Unlock()
}

func (t *fskitTransport) shouldCloseNativeChannelLocked() bool {
	if t.mountFinished {
		return t.mountSucceeded
	}
	return t.receivedNativeMessage
}

func (t *fskitTransport) ReadMessage(p []byte) (int, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return 0, io.EOF
	}
	t.operations.Add(1)
	t.activeReaders++
	channel := t.channel
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.activeReaders--
		t.stateChanged.Broadcast()
		t.mu.Unlock()
		t.operations.Done()
	}()

	for {
		var callErr C.int
		message := C.fskit_channel_copy_next_message(channel, &callErr)
		if message == nil {
			errno := syscall.Errno(callErr)
			if errno == syscall.EINTR {
				continue
			}
			if errno == syscall.ENODEV {
				return 0, io.EOF
			}
			return 0, &os.PathError{Op: "MFChannelCopyNextMessage", Path: "macFUSE", Err: errno}
		}

		t.noteNativeMessage()
		t.mu.Lock()
		closed := t.closed
		t.mu.Unlock()
		if closed {
			C.MFRelease(C.MFTypeRef(message))
			return 0, io.EOF
		}

		n, err := copyFskitMessage(message, p)
		C.MFRelease(C.MFTypeRef(message))
		return n, err
	}
}

func (t *fskitTransport) noteNativeMessage() {
	t.mu.Lock()
	t.receivedNativeMessage = true
	t.stateChanged.Broadcast()
	t.mu.Unlock()
}

func copyFskitMessage(message C.MFMessageRef, dst []byte) (int, error) {
	var callErr C.int
	bodySize := C.fskit_message_get_body_size(message, &callErr)
	if bodySize < 0 {
		return 0, fmt.Errorf("MFMessageGetBodySize: %w", syscall.Errno(callErr))
	}
	if uint64(bodySize) > uint64(len(dst)) {
		return 0, fmt.Errorf("FUSE message has %d bytes; transport buffer holds %d", int64(bodySize), len(dst))
	}

	var bodyBuffers *C.struct_iovec
	count := C.fskit_message_get_body_buffers(message, &bodyBuffers, &callErr)
	if count < 0 {
		return 0, fmt.Errorf("MFMessageGetBodyBuffers: %w", syscall.Errno(callErr))
	}
	if count == 0 || bodyBuffers == nil {
		return 0, fmt.Errorf("MFMessageGetBodyBuffers returned no body buffers")
	}

	buffers := unsafe.Slice(bodyBuffers, int(count))
	var offset int
	for _, body := range buffers {
		if uint64(body.iov_len) > uint64(len(dst)-offset) {
			return 0, fmt.Errorf("FUSE message buffers exceed reported body size %d", int64(bodySize))
		}
		length := int(body.iov_len)
		copy(dst[offset:offset+length], unsafe.Slice((*byte)(body.iov_base), length))
		offset += length
	}
	if offset != int(bodySize) {
		return 0, fmt.Errorf("FUSE message buffers contain %d bytes; header reports %d", offset, int64(bodySize))
	}
	return offset, nil
}

func (t *fskitTransport) WriteMessage(message []byte) error {
	return t.WritevMessage([][]byte{message})
}

func (t *fskitTransport) WritevMessage(parts [][]byte) error {
	if len(parts) == 0 {
		return fmt.Errorf("cannot send an empty FUSE message")
	}

	var total int
	for _, part := range parts {
		total += len(part)
	}

	vectorBytes := C.size_t(len(parts)) * C.size_t(C.sizeof_struct_iovec)
	vectorMemory := C.malloc(vectorBytes)
	if vectorMemory == nil {
		return fmt.Errorf("allocating FUSE reply iovecs")
	}
	defer C.free(vectorMemory)

	vectors := unsafe.Slice((*C.struct_iovec)(vectorMemory), len(parts))
	var pinner runtime.Pinner
	defer pinner.Unpin()
	for i, part := range parts {
		vectors[i].iov_base = nil
		vectors[i].iov_len = 0
		if len(part) != 0 {
			base := unsafe.SliceData(part)
			pinner.Pin(base)
			vectors[i].iov_base = unsafe.Pointer(base)
			vectors[i].iov_len = C.size_t(len(part))
		}
	}

	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return io.EOF
	}
	t.operations.Add(1)
	channel := t.channel
	t.mu.Unlock()
	defer t.operations.Done()

	var callErr C.int
	n := C.fskit_channel_send_message(channel, (*C.struct_iovec)(vectorMemory), C.size_t(len(parts)), &callErr)
	runtime.KeepAlive(parts)
	if n < 0 {
		return fmt.Errorf("MFChannelSendMessage: %w", syscall.Errno(callErr))
	}
	if int(n) != total {
		return fmt.Errorf("MFChannelSendMessage sent %d bytes; expected %d", int(n), total)
	}
	return nil
}

func (t *fskitTransport) Close() error {
	t.closeMu.Lock()
	defer t.closeMu.Unlock()

	t.mu.Lock()
	t.closed = true
	t.stateChanged.Broadcast()
	for !t.receivedNativeMessage && !t.mountFinished {
		if t.activeReaders > 0 {
			t.stateChanged.Wait()
			continue
		}

		// MFMount has no cancellation call while setup is pending. If no
		// connection reader is active, receive here to detect open and allow
		// Close to interrupt an early connection failure safely.
		channel := t.channel
		t.operations.Add(1)
		t.mu.Unlock()
		message, err := t.receiveForClose(channel)
		t.operations.Done()
		if message != nil {
			C.MFRelease(C.MFTypeRef(message))
			t.noteNativeMessage()
			t.mu.Lock()
			continue
		}
		if err == syscall.EINTR {
			t.mu.Lock()
			continue
		}

		t.mu.Lock()
		for !t.receivedNativeMessage && !t.mountFinished {
			t.stateChanged.Wait()
		}
	}
	closeChannel := t.shouldCloseNativeChannelLocked()
	t.mu.Unlock()

	var err error
	if closeChannel {
		// macFUSE 5.4.0 resumes the channel's signal sources before making its
		// first message available. That receipt is our proof that close is safe
		// while MFMount is still running; a returned mount failure closes it
		// internally, so this adapter skips a second close in that case.
		err = t.closeChannel()
	}
	t.operations.Wait()
	t.mountDone.Wait()
	t.releaseOnce.Do(func() {
		C.MFRelease(C.MFTypeRef(t.channel))
	})
	return err
}

func (t *fskitTransport) receiveForClose(channel C.MFChannelRef) (C.MFMessageRef, syscall.Errno) {
	for {
		var callErr C.int
		message := C.fskit_channel_copy_next_message(channel, &callErr)
		if message != nil {
			return message, 0
		}
		errno := syscall.Errno(callErr)
		if errno != syscall.EINTR {
			return nil, errno
		}
	}
}

func (t *fskitTransport) closeChannel() error {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closed = true
		channel := t.channel
		t.mu.Unlock()

		var callErr C.int
		if !C.fskit_channel_close(channel, &callErr) {
			t.closeErr = fmt.Errorf("MFChannelClose: %w", syscall.Errno(callErr))
		}
	})
	return t.closeErr
}

var _ connectionTransport = (*fskitTransport)(nil)
