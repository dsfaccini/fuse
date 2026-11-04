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
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
)

// fskitFrameworkPath is the install name of macFUSE's MFMount framework, which
// is loaded at mount time so that builds need neither cgo nor macFUSE.
var fskitFrameworkPath = "/Library/Filesystems/macfuse.fs/Contents/Frameworks/MFMount.framework/Versions/A/MFMount"

var fskitOSProductVersion = func() (string, error) {
	return syscall.Sysctl("kern.osproductversion")
}

// callErrno converts the errno that purego.SyscallN reports. On darwin purego
// loads the 32-bit errno with a 64-bit read, so the upper half is garbage and
// must be dropped before comparing against syscall constants.
func callErrno(raw uintptr) syscall.Errno {
	return syscall.Errno(uint32(raw))
}

// fskitAPI holds the addresses of the MFMount functions the FSKit transport
// calls. The channel functions arrived in macFUSE 5.3.
type fskitAPI struct {
	channelCreate          uintptr
	mount                  uintptr
	channelCopyNextMessage uintptr
	messageGetBodySize     uintptr
	messageGetBodyBuffers  uintptr
	channelSendMessage     uintptr
	channelClose           uintptr
	release                uintptr
}

func loadFSKitAPI(path string) (*fskitAPI, error) {
	lib, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFSKitUnavailable, err)
	}
	api := &fskitAPI{}
	for _, symbol := range []struct {
		name string
		addr *uintptr
	}{
		{"MFChannelCreate", &api.channelCreate},
		{"MFMount", &api.mount},
		{"MFChannelCopyNextMessage", &api.channelCopyNextMessage},
		{"MFMessageGetBodySize", &api.messageGetBodySize},
		{"MFMessageGetBodyBuffers", &api.messageGetBodyBuffers},
		{"MFChannelSendMessage", &api.channelSendMessage},
		{"MFChannelClose", &api.channelClose},
		{"MFRelease", &api.release},
	} {
		if *symbol.addr, err = purego.Dlsym(lib, symbol.name); err != nil {
			purego.Dlclose(lib)
			return nil, fmt.Errorf("%w: %v", ErrFSKitUnavailable, err)
		}
	}
	return api, nil
}

type fskitTransport struct {
	api            *fskitAPI
	mu             sync.Mutex
	channel        uintptr
	closed         bool
	mountSucceeded bool
	operations     sync.WaitGroup
	mountDone      sync.WaitGroup
	closeOnce      sync.Once
	closeErr       error
	closeNative    func() error
	releaseNative  func()
}

func mountFSKitTransport(
	dir string,
	cfg *MountConfig,
	ready chan<- error) (connectionTransport, error) {
	if err := validateFSKitMount(dir, cfg); err != nil {
		return nil, err
	}

	mountOptions, err := syscall.BytePtrFromString(fskitMountOptions(cfg))
	if err != nil {
		return nil, fmt.Errorf("FSKit mount options: %w", err)
	}
	mountPoint, err := syscall.BytePtrFromString(dir)
	if err != nil {
		return nil, fmt.Errorf("FSKit mount point: %w", err)
	}

	api, err := loadFSKitAPI(fskitFrameworkPath)
	if err != nil {
		return nil, err
	}
	channel, _, _ := purego.SyscallN(api.channelCreate)
	if channel == 0 {
		return nil, fmt.Errorf("MFChannelCreate failed")
	}
	t := &fskitTransport{
		api:     api,
		channel: channel,
		closeNative: func() error {
			closed, _, errno := purego.SyscallN(api.channelClose, channel)
			if byte(closed) == 0 {
				return fmt.Errorf("MFChannelClose: %w", callErrno(errno))
			}
			return nil
		},
		releaseNative: func() { purego.SyscallN(api.release, channel) },
	}

	t.mountDone.Add(1)
	go func() {
		defer t.mountDone.Done()

		// Report failures through the result code instead of macFUSE dialogs.
		const quiet = 1
		result, _, _ := purego.SyscallN(
			api.mount,
			channel,
			uintptr(unsafe.Pointer(mountPoint)),
			uintptr(unsafe.Pointer(mountOptions)),
			quiet)
		var mountErr error
		if code := int32(result); code != 0 {
			mountErr = FSKitMountError{Result: code}
		}
		t.setMountResult(mountErr == nil)
		ready <- mountErr
	}()

	return t, nil
}

// fskitMountOptions returns the option string MFMount receives for cfg.
func fskitMountOptions(cfg *MountConfig) string {
	options := cfg.toMap()
	delete(options, "backend")
	return mapToOptionsString(options)
}

func validateFSKitMount(dir string, cfg *MountConfig) error {
	if dir == "" {
		return fmt.Errorf("FSKit mount point must not be empty")
	}

	// Check availability before allocating a native channel.
	version, err := fskitOSProductVersion()
	if err != nil {
		return fmt.Errorf("reading macOS version for FSKit: %w", err)
	}
	_, local := cfg.Options["local"]
	return validateFSKitOSVersion(version, local)
}

// validateFSKitOSVersion checks version against the macFUSE FSKit module that
// serves the mount: macOS 26 for the default module, macOS 15.4 for the module
// that serves volumes mounted with the local option.
func validateFSKitOSVersion(version string, local bool) error {
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return fmt.Errorf("invalid macOS product version %q", version)
	}
	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])
	if majorErr != nil || minorErr != nil {
		return fmt.Errorf("invalid macOS product version %q", version)
	}
	minMajor, minMinor := 26, 0
	if local {
		minMajor, minMinor = 15, 4
	}
	if major < minMajor || major == minMajor && minor < minMinor {
		return FSKitMountError{Result: FSKitMountResultUnsupportedOSVersion}
	}
	return nil
}

func (t *fskitTransport) setMountResult(succeeded bool) {
	t.mu.Lock()
	t.mountSucceeded = succeeded
	t.mu.Unlock()
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
		message, _, callErr := purego.SyscallN(t.api.channelCopyNextMessage, channel)
		if message == 0 {
			errno := callErrno(callErr)
			if errno == syscall.EINTR {
				continue
			}
			if errno == syscall.ENODEV {
				return 0, io.EOF
			}
			return 0, &os.PathError{Op: "MFChannelCopyNextMessage", Path: "macFUSE", Err: errno}
		}

		t.mu.Lock()
		closed := t.closed
		t.mu.Unlock()
		if closed {
			purego.SyscallN(t.api.release, message)
			return 0, io.EOF
		}

		n, err := t.api.copyMessage(message, p)
		purego.SyscallN(t.api.release, message)
		return n, err
	}
}

func (api *fskitAPI) copyMessage(message uintptr, dst []byte) (int, error) {
	result, _, callErr := purego.SyscallN(api.messageGetBodySize, message)
	bodySize := int(result)
	if bodySize < 0 {
		return 0, fmt.Errorf("MFMessageGetBodySize: %w", callErrno(callErr))
	}
	if bodySize > len(dst) {
		return 0, fmt.Errorf("FUSE message has %d bytes; transport buffer holds %d", bodySize, len(dst))
	}

	var bodyBuffers *syscall.Iovec
	result, _, callErr = purego.SyscallN(
		api.messageGetBodyBuffers,
		message,
		uintptr(unsafe.Pointer(&bodyBuffers)))
	count := int(result)
	if count < 0 {
		return 0, fmt.Errorf("MFMessageGetBodyBuffers: %w", callErrno(callErr))
	}
	if count == 0 || bodyBuffers == nil {
		return 0, fmt.Errorf("MFMessageGetBodyBuffers returned no body buffers")
	}

	var offset int
	for _, body := range unsafe.Slice(bodyBuffers, count) {
		if body.Len > uint64(len(dst)-offset) {
			return 0, fmt.Errorf("FUSE message buffers exceed reported body size %d", bodySize)
		}
		length := int(body.Len)
		copy(dst[offset:offset+length], unsafe.Slice(body.Base, length))
		offset += length
	}
	if offset != bodySize {
		return 0, fmt.Errorf("FUSE message buffers contain %d bytes; header reports %d", offset, bodySize)
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
	vectors := make([]syscall.Iovec, len(parts))
	var pinner runtime.Pinner
	defer pinner.Unpin()
	for i, part := range parts {
		total += len(part)
		if len(part) != 0 {
			pinner.Pin(&part[0])
			vectors[i].Base = &part[0]
			vectors[i].SetLen(len(part))
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

	result, _, callErr := purego.SyscallN(
		t.api.channelSendMessage,
		channel,
		uintptr(unsafe.Pointer(&vectors[0])),
		uintptr(len(vectors)))
	sent := int(result)
	if sent < 0 {
		return fmt.Errorf("MFChannelSendMessage: %w", callErrno(callErr))
	}
	if sent != total {
		return fmt.Errorf("MFChannelSendMessage sent %d bytes; expected %d", sent, total)
	}
	return nil
}

func (t *fskitTransport) Close() error {
	t.closeOnce.Do(func() {
		// MFMount owns channel closure on failure. Keep INIT traffic available
		// until its native result is published before claiming close ownership.
		t.mountDone.Wait()
		t.mu.Lock()
		t.closed = true
		succeeded := t.mountSucceeded
		t.mu.Unlock()
		if succeeded {
			t.closeErr = t.closeNative()
		}
		t.operations.Wait()
		t.releaseNative()
	})
	return t.closeErr
}

var _ connectionTransport = (*fskitTransport)(nil)
