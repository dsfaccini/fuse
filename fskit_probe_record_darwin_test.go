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

//go:build fuse_record

package fuse

import (
	"bufio"
	"encoding/binary"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// probeRecordingMagic opens every recording and names its format version.
const probeRecordingMagic = "FSKREC01"

// probeRecorder writes the message stream of one FSKit probe mount to
// testdata/fskit/<mount>.frames, for replay through a fake MFMount. It exists
// only under the fuse_record build tag.
//
// A recording is probeRecordingMagic followed by frames, in the order the
// messages crossed MFMount. Each frame is:
//
//	direction  1 byte         'K' for a message FSKit sent, 'S' for one the server sent
//	elapsed    8 bytes        little-endian int64, nanoseconds since the recording began
//	length     4 bytes        little-endian uint32, the message's byte count
//	message    length bytes   the whole FUSE message
//
// A 'K' message is the body MFChannelCopyNextMessage returned, its body
// buffers concatenated, starting with fuse_in_header. An 'S' message is the
// iovecs one MFChannelSendMessage call sent, concatenated, starting with
// fuse_out_header; notify messages are 'S' frames too. The FUSE structs inside
// a message keep the host's byte order.
type probeRecorder struct {
	mu    sync.Mutex
	file  *os.File
	w     *bufio.Writer
	start time.Time
	err   error
}

func newProbeRecorder(t *testing.T, mount string) *probeRecorder {
	path := filepath.Join("testdata", "fskit", mount+".frames")
	r := &probeRecorder{start: time.Now()}
	if r.err = os.MkdirAll(filepath.Dir(path), 0o755); r.err == nil {
		r.file, r.err = os.Create(path)
	}
	if r.err != nil {
		t.Errorf("recording %s: %v", path, r.err)
		return r
	}
	r.w = bufio.NewWriter(r.file)
	_, r.err = r.w.WriteString(probeRecordingMagic)
	t.Cleanup(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.err == nil {
			r.err = r.w.Flush()
		}
		if err := r.file.Close(); r.err == nil {
			r.err = err
		}
		if r.err != nil {
			t.Errorf("recording %s: %v", path, r.err)
			return
		}
		t.Logf("recorded %s", path)
	})
	return r
}

func (r *probeRecorder) frame(direction byte, parts ...[]byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return
	}
	var length int
	for _, part := range parts {
		length += len(part)
	}
	var header [13]byte
	header[0] = direction
	binary.LittleEndian.PutUint64(header[1:], uint64(time.Since(r.start)))
	binary.LittleEndian.PutUint32(header[9:], uint32(length))
	if _, r.err = r.w.Write(header[:]); r.err != nil {
		return
	}
	for _, part := range parts {
		if _, r.err = r.w.Write(part); r.err != nil {
			return
		}
	}
}
