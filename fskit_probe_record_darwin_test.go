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

// probeRecordingDir holds the recordings, relative to the package directory.
var probeRecordingDir = filepath.Join("testdata", "fskit")

// probeRecorder writes the message stream of one FSKit probe mount to
// testdata/fskit/<mount>.frames, for replay through a fake MFMount. It exists
// only under the fuse_record build tag.
//
// A recording is probeRecordingMagic followed by frames. Each frame is:
//
//	direction  1 byte         'K' for a message FSKit sent, 'S' for one the server sent
//	elapsed    8 bytes        little-endian int64, nanoseconds from the recording's start
//	length     4 bytes        little-endian uint32, the message's byte count
//	message    length bytes   the whole FUSE message
//
// A 'K' message is the body MFChannelCopyNextMessage returned, its body
// buffers concatenated, starting with fuse_in_header. Its elapsed time is when
// the copy returned. An 'S' message is the iovecs one successful
// MFChannelSendMessage call sent, concatenated, starting with
// fuse_out_header. Its elapsed time is just before the send. Notify messages
// are 'S' frames too.
//
// Frames are in the order the recorder appended them. An 'S' frame is
// appended after its send returns, so a later 'K' frame can come first. Sort
// by elapsed time to replay in send order. The FUSE structs inside a message
// keep the host's byte order.
type probeRecorder struct {
	mu     sync.Mutex
	file   *os.File
	w      *bufio.Writer
	start  time.Time
	err    error
	closed bool
}

// checkProbeRecording fails t unless the probe can write recordings.
func checkProbeRecording(t *testing.T) {
	if err := os.MkdirAll(probeRecordingDir, 0o755); err != nil {
		t.Fatalf("recording: %v", err)
	}
	f, err := os.CreateTemp(probeRecordingDir, "writable-")
	if err != nil {
		t.Fatalf("recording: %v", err)
	}
	f.Close()
	os.Remove(f.Name())
}

// newProbeRecorder starts the recording of mount. Its cleanup closes the
// recording, so call it before registering the mount's teardown, whose
// unmount traffic then lands in the recording.
func newProbeRecorder(t *testing.T, mount string) *probeRecorder {
	path := filepath.Join(probeRecordingDir, mount+".frames")
	r := &probeRecorder{start: time.Now()}
	if r.file, r.err = os.Create(path); r.err != nil {
		t.Fatalf("recording %s: %v", path, r.err)
	}
	r.w = bufio.NewWriter(r.file)
	_, r.err = r.w.WriteString(probeRecordingMagic)
	t.Cleanup(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.closed = true
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

// frame appends one message, stamped with at.
func (r *probeRecorder) frame(direction byte, at time.Time, parts ...[]byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil || r.closed {
		return
	}
	var length int
	for _, part := range parts {
		length += len(part)
	}
	var header [13]byte
	header[0] = direction
	binary.LittleEndian.PutUint64(header[1:], uint64(at.Sub(r.start)))
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
