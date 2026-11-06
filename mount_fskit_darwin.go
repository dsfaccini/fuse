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
	"time"
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
	messageGetBodyBuffers  uintptr
	channelSendMessage     uintptr
	channelClose           uintptr
	channelInterrupt       uintptr
	retain                 uintptr
	release                uintptr
}

// fskitAPILoader returns the MFMount functions. Tests replace it to mount
// through a fake MFMount.
var fskitAPILoader = cacheFSKitAPI(loadFSKitAPI)

// cacheFSKitAPI returns a loader that calls load with fskitFrameworkPath until
// a load succeeds, then returns that result. A failed load is retried, so a
// running process can use macFUSE once it is installed.
func cacheFSKitAPI(load func(string) (*fskitAPI, error)) func() (*fskitAPI, error) {
	var mu sync.Mutex
	var api *fskitAPI
	return func() (*fskitAPI, error) {
		mu.Lock()
		defer mu.Unlock()
		if api == nil {
			loaded, err := load(fskitFrameworkPath)
			if err != nil {
				return nil, err
			}
			api = loaded
		}
		return api, nil
	}
}

// fskitSendVectors is how many finished sends' iovec slices a transport keeps
// for reuse. A send that finds none kept allocates one.
const fskitSendVectors = 16

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
		{"MFMessageGetBodyBuffers", &api.messageGetBodyBuffers},
		{"MFChannelSendMessage", &api.channelSendMessage},
		{"MFChannelClose", &api.channelClose},
		{"MFChannelInterrupt", &api.channelInterrupt},
		{"MFRetain", &api.retain},
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
	api *fskitAPI

	// vectors holds iovec slices that sends have finished with, so that a warm
	// transport sends without allocating and without a lock across the send.
	vectors chan []syscall.Iovec

	// mounted is closed once MFMount has returned and its result is
	// published, and mountErr then holds that result.
	mounted  chan struct{}
	mountErr error

	mu      sync.Mutex
	channel uintptr
	closed  bool
	// cancelled is set once cancel runs or MFMount fails. A read that EINTR
	// interrupts then returns io.EOF instead of retrying, so the read waiting
	// for INIT ends.
	cancelled bool
	// reading counts the reads in progress.
	reading         int
	mountSucceeded  bool
	operations      sync.WaitGroup
	closeOnce       sync.Once
	closeErr        error
	closeNative     func() error
	interruptNative func()
	releaseNative   func()
}

func mountFSKitTransport(dir string, cfg *MountConfig) (nativeMount, error) {
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

	api, err := fskitAPILoader()
	if err != nil {
		return nil, err
	}
	channel, _, _ := purego.SyscallN(api.channelCreate)
	if channel == 0 {
		return nil, fmt.Errorf("MFChannelCreate failed")
	}
	t := newFSKitTransport(api, channel)

	// The mount's reference keeps the channel alive for MFMount and for
	// setMountResult, so that Close need not wait for an MFMount that
	// MountContext gave up on.
	purego.SyscallN(api.retain, channel)
	go func() {
		defer purego.SyscallN(api.release, channel)

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
		t.setMountResult(mountErr)
	}()

	return t, nil
}

func newFSKitTransport(api *fskitAPI, channel uintptr) *fskitTransport {
	return &fskitTransport{
		api:     api,
		vectors: make(chan []syscall.Iovec, fskitSendVectors),
		mounted: make(chan struct{}),
		channel: channel,
		closeNative: func() error {
			closed, _, errno := purego.SyscallN(api.channelClose, channel)
			if byte(closed) == 0 {
				return fmt.Errorf("MFChannelClose: %w", callErrno(errno))
			}
			return nil
		},
		interruptNative: func() { purego.SyscallN(api.channelInterrupt, channel) },
		releaseNative:   func() { purego.SyscallN(api.release, channel) },
	}
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

// setMountResult publishes MFMount's result. When Close has already run, it
// did not wait for the result, so a successful mount's channel is closed here.
// A failed mount interrupts the read waiting for INIT.
func (t *fskitTransport) setMountResult(err error) {
	t.mu.Lock()
	t.mountSucceeded = err == nil
	closeLate := t.closed && err == nil
	if err != nil {
		t.cancelled = true
	}
	t.mu.Unlock()
	if closeLate {
		t.closeNative()
	}
	if err != nil {
		t.interruptReads()
	}
	t.mountErr = err
	close(t.mounted)
}

// interruptReads interrupts the channel until no read is in progress.
// MFChannelInterrupt wakes only the reads already waiting, so a read that
// checked cancelled just before it was set needs a later interrupt.
func (t *fskitTransport) interruptReads() {
	for {
		t.interruptNative()
		t.mu.Lock()
		reading := t.reading
		t.mu.Unlock()
		if reading == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func (t *fskitTransport) backend() Backend { return BackendFSKit }

func (t *fskitTransport) waitMounted() error {
	<-t.mounted
	return t.mountErr
}

// cancel interrupts the read waiting for INIT, and makes it and every later
// read return io.EOF.
func (t *fskitTransport) cancel() {
	t.mu.Lock()
	t.cancelled = true
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.operations.Add(1)
	t.mu.Unlock()
	defer t.operations.Done()
	t.interruptReads()
}

func (t *fskitTransport) ReadMessage(p []byte) (int, error) {
	t.mu.Lock()
	if t.closed || t.cancelled {
		t.mu.Unlock()
		return 0, io.EOF
	}
	t.operations.Add(1)
	t.reading++
	channel := t.channel
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.reading--
		t.mu.Unlock()
		t.operations.Done()
	}()

	for {
		message, _, callErr := purego.SyscallN(t.api.channelCopyNextMessage, channel)
		if message == 0 {
			errno := callErrno(callErr)
			if errno == syscall.EINTR {
				// Signals interrupt reads too, so only cancel ends one.
				t.mu.Lock()
				cancelled := t.cancelled
				t.mu.Unlock()
				if cancelled {
					return 0, io.EOF
				}
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
	var bodyBuffers *syscall.Iovec
	result, _, callErr := purego.SyscallN(
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

	// InMessage.Init checks the header's length against the bytes copied.
	var offset int
	for _, body := range unsafe.Slice(bodyBuffers, count) {
		if body.Len > uint64(len(dst)-offset) {
			return 0, fmt.Errorf("FUSE message exceeds the %d-byte transport buffer", len(dst))
		}
		length := int(body.Len)
		copy(dst[offset:offset+length], unsafe.Slice(body.Base, length))
		offset += length
	}
	return offset, nil
}

func (t *fskitTransport) SendMessage(parts [][]byte) error {
	if len(parts) == 0 {
		return fmt.Errorf("cannot send an empty FUSE message")
	}

	var total int
	var vectors []syscall.Iovec
	select {
	case vectors = <-t.vectors:
	default:
	}
	defer func() {
		// Drop the references to parts before the slice is reused.
		clear(vectors)
		select {
		case t.vectors <- vectors[:0]:
		default:
		}
	}()
	var pinner runtime.Pinner
	defer pinner.Unpin()
	for _, part := range parts {
		var vector syscall.Iovec
		if len(part) != 0 {
			pinner.Pin(&part[0])
			vector.Base = &part[0]
			vector.SetLen(len(part))
		}
		vectors = append(vectors, vector)
		total += len(part)
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
		// until its native result is published before claiming close
		// ownership, unless the mount was cancelled: then MFMount may never
		// return, and setMountResult closes a mount that succeeds late.
		t.mu.Lock()
		cancelled := t.cancelled
		t.mu.Unlock()
		if !cancelled {
			<-t.mounted
		}
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

var _ nativeMount = (*fskitTransport)(nil)
