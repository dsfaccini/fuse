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
	"syscall"
)

// connectionTransport moves complete FUSE messages between the connection
// and its kernel-facing endpoint. ReadMessage must return one whole message
// per call; the legacy file transport inherits this property from /dev/fuse.
type connectionTransport interface {
	ReadMessage([]byte) (int, error)
	WriteMessage([]byte) error
	WritevMessage([][]byte) error
	Close() error
}

// shortReadPadder is implemented by a transport that delivers a READ reply
// shorter than the size it cached for the file padded to that size, with bytes
// that are not file data. The connection then replies EIO to such a read; see
// Connection.trackReportedSizes.
type shortReadPadder interface {
	padsShortReads() bool
}

type transportReader struct {
	transport connectionTransport
}

func (r transportReader) Read(p []byte) (int, error) {
	return r.transport.ReadMessage(p)
}

type fileTransport struct {
	file *os.File
}

func newFileTransport(file *os.File) connectionTransport {
	return &fileTransport{file: file}
}

func (t *fileTransport) ReadMessage(p []byte) (int, error) {
	return t.file.Read(p)
}

func (t *fileTransport) WriteMessage(p []byte) error {
	// Avoid the retry loop in os.File.Write.
	n, err := syscall.Write(int(t.file.Fd()), p)
	if err != nil {
		return err
	}
	if n != len(p) {
		return fmt.Errorf("Wrote %d bytes; expected %d", n, len(p))
	}

	return nil
}

func (t *fileTransport) WritevMessage(p [][]byte) error {
	_, err := writev(int(t.file.Fd()), p)
	return err
}

func (t *fileTransport) Close() error {
	return t.file.Close()
}

var _ io.Reader = transportReader{}
var _ connectionTransport = (*fileTransport)(nil)
