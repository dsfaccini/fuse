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

static int mf_mount_errno(void) { return errno; }
*/
import "C"

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

type fskitTransport struct {
	mu         sync.Mutex
	channel    C.MFChannelRef
	closed     bool
	operations sync.WaitGroup
	closeOnce  sync.Once
	closeErr   error
}

func mountFSKitTransport(
	dir string,
	cfg *MountConfig,
	ready chan<- error) (connectionTransport, error) {
	channel := C.MFChannelCreate()
	if channel == nil {
		return nil, fmt.Errorf("MFChannelCreate failed")
	}

	t := &fskitTransport{channel: channel}
	options := cfg.toMap()
	delete(options, "backend")
	mountOptions := C.CString(mapToOptionsString(options))
	mountPoint := C.CString(dir)
	go func() {
		defer C.free(unsafe.Pointer(mountOptions))
		defer C.free(unsafe.Pointer(mountPoint))

		result := C.MFMount(channel, mountPoint, mountOptions, true)
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
			mountErr = fmt.Errorf("MFMount failed: %w", syscall.Errno(C.mf_mount_errno()))
		default:
			mountErr = fmt.Errorf("MFMount failed with result %d", int32(result))
		}
		ready <- mountErr
		if mountErr != nil {
			// Closing wakes ReadMessage if MFMount failed before the FUSE INIT
			// request arrived. Close is idempotent with connection teardown.
			_ = t.Close()
		}
	}()

	return t, nil
}

func (t *fskitTransport) ReadMessage(p []byte) (int, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return 0, io.EOF
	}
	t.operations.Add(1)
	channel := t.channel
	t.mu.Unlock()
	defer t.operations.Done()

	for {
		message := C.MFChannelCopyNextMessage(channel)
		if message == nil {
			errno := syscall.Errno(C.mf_mount_errno())
			if errno == syscall.EINTR {
				continue
			}
			if errno == syscall.ENODEV {
				return 0, io.EOF
			}
			return 0, &os.PathError{Op: "MFChannelCopyNextMessage", Path: "macFUSE", Err: errno}
		}

		n, err := copyFskitMessage(message, p)
		C.MFRelease(C.MFTypeRef(message))
		return n, err
	}
}

func copyFskitMessage(message C.MFMessageRef, dst []byte) (int, error) {
	bodySize := C.MFMessageGetBodySize(message)
	if bodySize < 0 {
		return 0, fmt.Errorf("MFMessageGetBodySize: %w", syscall.Errno(C.mf_mount_errno()))
	}
	if uint64(bodySize) > uint64(len(dst)) {
		return 0, fmt.Errorf("FUSE message has %d bytes; transport buffer holds %d", int64(bodySize), len(dst))
	}

	var bodyBuffers *C.struct_iovec
	count := C.MFMessageGetBodyBuffers(message, &bodyBuffers)
	if count < 0 {
		return 0, fmt.Errorf("MFMessageGetBodyBuffers: %w", syscall.Errno(C.mf_mount_errno()))
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

	n := C.MFChannelSendMessage(channel, (*C.struct_iovec)(vectorMemory), C.size_t(len(parts)))
	runtime.KeepAlive(parts)
	if n < 0 {
		return fmt.Errorf("MFChannelSendMessage: %w", syscall.Errno(C.mf_mount_errno()))
	}
	if int(n) != total {
		return fmt.Errorf("MFChannelSendMessage sent %d bytes; expected %d", int(n), total)
	}
	return nil
}

func (t *fskitTransport) Close() error {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closed = true
		channel := t.channel
		t.mu.Unlock()

		if !C.MFChannelClose(channel) {
			t.closeErr = fmt.Errorf("MFChannelClose: %w", syscall.Errno(C.mf_mount_errno()))
		}
		// Closing wakes blocked receives. Wait for every in-flight channel call
		// to return before releasing the channel's final owned reference.
		t.operations.Wait()
		C.MFRelease(C.MFTypeRef(channel))
	})
	return t.closeErr
}

var _ connectionTransport = (*fskitTransport)(nil)
