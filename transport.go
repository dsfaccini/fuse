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
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"syscall"
)

// connectionTransport moves whole FUSE messages between the connection and
// its kernel-facing endpoint.
type connectionTransport interface {
	// ReadMessage reads one whole message into p and returns its length. It
	// retries a read that EINTR interrupts, and returns io.EOF once the kernel
	// has hung up. It does not keep p after it returns.
	ReadMessage(p []byte) (int, error)

	// SendMessage sends the concatenation of parts as one whole message. parts
	// is not empty. SendMessage does not keep parts, or any part, after it
	// returns.
	SendMessage(parts [][]byte) error

	Close() error
}

// fileTransport is a FUSE device: /dev/fuse on Linux, the macFUSE kernel
// extension's device on darwin.
type fileTransport struct {
	file *os.File
}

func newFileTransport(file *os.File) connectionTransport {
	return &fileTransport{file: file}
}

func (t *fileTransport) ReadMessage(p []byte) (int, error) {
	return readDevice(t.file, p)
}

// readDevice reads one message from a FUSE device. Each read of the device
// returns one whole message.
func readDevice(dev io.Reader, p []byte) (int, error) {
	for {
		n, err := dev.Read(p)
		switch {
		// EINTR seems to happen often on OS X, cf. http://golang.org/issue/11180.
		case errors.Is(err, syscall.EINTR):
			continue
		// ENODEV means fuse has hung up. On Linux, ECONNABORTED does too:
		// fuse_dev_do_read returns it instead of ENODEV when a read races the
		// connection's abort, as an unmount does (https://github.com/torvalds/linux/blob/05441d697f7f9a927264f4058cbbc6a03dfcac9d/fs/fuse/dev.c#L1627-L1629).
		case errors.Is(err, syscall.ENODEV),
			runtime.GOOS == "linux" && errors.Is(err, syscall.ECONNABORTED):
			return 0, io.EOF
		}
		return n, err
	}
}

// SendMessage writes parts with one write(2), or one writev(2) when there are
// several. The kernel takes each call as one whole message, so a short write
// fails instead of sending the rest as a second message.
func (t *fileTransport) SendMessage(parts [][]byte) error {
	var n int
	var err error
	if len(parts) == 1 {
		// Avoid the retry loop in os.File.Write.
		n, err = syscall.Write(int(t.file.Fd()), parts[0])
	} else {
		n, err = writev(int(t.file.Fd()), parts)
	}
	if err != nil {
		return err
	}

	var total int
	for _, part := range parts {
		total += len(part)
	}
	if n != total {
		return fmt.Errorf("Wrote %d bytes; expected %d", n, total)
	}

	return nil
}

func (t *fileTransport) Close() error {
	return t.file.Close()
}

var _ connectionTransport = (*fileTransport)(nil)

// deviceMount is a mount served through a FUSE device.
type deviceMount struct {
	fileTransport
	kind Backend

	// mounted is closed once the native mount has finished, and mountErr
	// then holds its result.
	mounted  chan struct{}
	mountErr error
}

// newDeviceMount returns the mount served through dev. wait blocks until the
// native mount has finished and returns its result. Nil means dev's mount has
// finished.
func newDeviceMount(dev *os.File, kind Backend, wait func() error) *deviceMount {
	m := &deviceMount{
		fileTransport: fileTransport{file: dev},
		kind:          kind,
		mounted:       make(chan struct{}),
	}
	if wait == nil {
		close(m.mounted)
		return m
	}
	go func() {
		m.mountErr = wait()
		close(m.mounted)
	}()
	return m
}

func (m *deviceMount) backend() Backend { return m.kind }

func (m *deviceMount) waitMounted() error {
	<-m.mounted
	return m.mountErr
}

// cancel does nothing: a read of the device cannot be interrupted.
func (m *deviceMount) cancel() {}

var _ nativeMount = (*deviceMount)(nil)
