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
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/dsfaccini/fuse/internal/buffer"
	"github.com/dsfaccini/fuse/internal/fusekernel"
)

// A recording of an FSKit probe mount is probeRecordingMagic followed by
// frames. Each frame is:
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
const probeRecordingMagic = "FSKREC01"

// probeRecordingDir holds the recordings, relative to the package directory.
var probeRecordingDir = filepath.Join("testdata", "fskit")

// replayRecording is a recording of one mount of TestFSKitProbe's replay
// plan, on macFUSE 5.5.0. The recorder never writes this name: the owner
// reviews a recording before it replaces this one.
var replayRecording = filepath.Join(probeRecordingDir, "replay-macfuse-5.5.0.frames")

// probeFrame is one frame of a recording.
type probeFrame struct {
	direction byte
	elapsed   time.Duration
	message   []byte
}

// unique returns the request ID in the frame's FUSE header.
func (f probeFrame) unique() uint64 { return binary.NativeEndian.Uint64(f.message[8:16]) }

// opcode returns the opcode in a 'K' frame's FUSE header.
func (f probeFrame) opcode() uint32 { return binary.NativeEndian.Uint32(f.message[4:8]) }

// readProbeRecording returns the frames of the recording at path, sorted by
// elapsed time. It checks that each message is a whole FUSE message.
func readProbeRecording(path string) ([]probeFrame, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	rest, ok := bytes.CutPrefix(data, []byte(probeRecordingMagic))
	if !ok {
		return nil, fmt.Errorf("%s does not start with %s", path, probeRecordingMagic)
	}
	var frames []probeFrame
	for len(rest) > 0 {
		if len(rest) < 13 {
			return nil, fmt.Errorf("%s: frame %d: header truncated", path, len(frames))
		}
		length := binary.LittleEndian.Uint32(rest[9:13])
		if uint64(len(rest)-13) < uint64(length) {
			return nil, fmt.Errorf("%s: frame %d: message truncated", path, len(frames))
		}
		frame := probeFrame{
			direction: rest[0],
			elapsed:   time.Duration(binary.LittleEndian.Uint64(rest[1:9])),
			message:   rest[13 : 13+length],
		}
		rest = rest[13+length:]
		header := fusekernel.InHeaderSize
		switch frame.direction {
		case frameFromFSKit:
		case frameFromServer:
			header = buffer.OutMessageHeaderSize
		default:
			return nil, fmt.Errorf("%s: frame %d: direction %q", path, len(frames), frame.direction)
		}
		if len(frame.message) < header || binary.NativeEndian.Uint32(frame.message[0:4]) != length {
			return nil, fmt.Errorf("%s: frame %d: %d bytes, not one whole FUSE message", path, len(frames), length)
		}
		frames = append(frames, frame)
	}
	slices.SortStableFunc(frames, func(a, b probeFrame) int { return cmp.Compare(a.elapsed, b.elapsed) })
	return frames, nil
}

// fskitBodyBuffers splits a recorded request into the body buffers FSKit
// hands over: one, or two for a WRITE, whose payload has a buffer of its own.
func fskitBodyBuffers(request []byte) [][]byte {
	// fuse_write_in: fh, offset, size.
	if binary.NativeEndian.Uint32(request[4:8]) != fusekernel.OpWrite || len(request) < fusekernel.InHeaderSize+20 {
		return [][]byte{request}
	}
	payload := int(binary.NativeEndian.Uint32(request[fusekernel.InHeaderSize+16:]))
	split := len(request) - payload
	if payload == 0 || split < fusekernel.InHeaderSize+20 {
		return [][]byte{request}
	}
	return [][]byte{request[:split], request[split:]}
}

// TestReplayFSKitRecording replays a recorded FSKit mount through Mount and
// the fake MFMount, served by the probe's file system as the recorded mount
// was. Each request reaches the connection in the order FSKit sent it, and
// each recorded reply is awaited where the server sent it. A reply must carry
// the recorded status and length, and a READ reply the recorded bytes. The
// probe held some READs before it answered them; the replay answers them at
// once with the recorded byte count.
//
// A failure means the library now answers FSKit's requests differently than
// when the mount was recorded. TestFSKitProbe documents how to record it
// again.
func TestReplayFSKitRecording(t *testing.T) {
	frames, err := readProbeRecording(replayRecording)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) == 0 || frames[0].direction != frameFromFSKit || frames[0].opcode() != fusekernel.OpInit {
		t.Fatalf("%s does not start with FSKit's INIT", replayRecording)
	}
	init := frames[0]
	requests := map[uint64]probeFrame{}
	replied := map[uint64]probeFrame{}
	for _, frame := range frames {
		if frame.direction == frameFromFSKit {
			requests[frame.unique()] = frame
		} else {
			replied[frame.unique()] = frame
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	m := &probeMount{
		ctx:   ctx,
		log:   &probeLog{names: map[uint64]string{}, changed: make(chan struct{})},
		holds: map[string]*heldRead{},
	}
	m.server = m.newServer()
	for unique, reply := range replied {
		request, ok := requests[unique]
		if !ok || request.opcode() != fusekernel.OpRead {
			continue
		}
		name := m.log.names[binary.NativeEndian.Uint64(request.message[16:24])]
		if h := m.holds[name]; h != nil {
			h.answer(int64(len(reply.message) - buffer.OutMessageHeaderSize))
		}
	}

	f := newFakeMFMount(t)
	var initReply []byte
	f.mount = func() int32 {
		f.kernel.Send(fskitBodyBuffers(init.message)...)
		reply, ok := f.kernel.nextReply()
		if !ok {
			f.kernel.HangUp()
			return FSKitMountResultUnexpectedFailure
		}
		initReply = reply
		return 0
	}
	cfg := MountConfig{FSName: probeDirPrefix + "replay", Subtype: "fskit-probe", DisableWritebackCaching: true}
	mfs, err := MountWithFakeMFMount(context.Background(), t, f, m.server, cfg)
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	t.Cleanup(func() {
		f.kernel.HangUp()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := mfs.Join(ctx); err != nil {
			t.Errorf("Join: %v", err)
		}
	})
	// Registered last, so it runs first and ends any READ still held.
	t.Cleanup(cancel)

	unread := map[uint64][]byte{}
	check := func(want []byte, got []byte) {
		t.Helper()
		request := requests[binary.NativeEndian.Uint64(want[8:16])]
		status := func(reply []byte) int32 { return int32(binary.NativeEndian.Uint32(reply[4:8])) }
		switch {
		case status(got) != status(want) || len(got) != len(want):
			t.Errorf("reply to opcode %d, request %d: status %d, %d bytes; recorded status %d, %d bytes",
				request.opcode(), request.unique(), status(got), len(got), status(want), len(want))
		case request.direction == frameFromFSKit && request.opcode() == fusekernel.OpRead && !bytes.Equal(got, want):
			t.Errorf("reply to READ %d differs from the recorded bytes", request.unique())
		}
	}
	if want, ok := replied[init.unique()]; !ok {
		t.Fatal("the recording holds no INIT reply")
	} else {
		check(want.message, initReply)
	}
	for _, frame := range frames {
		unique := frame.unique()
		switch {
		case unique == init.unique():
		case frame.direction == frameFromFSKit:
			f.kernel.Send(fskitBodyBuffers(frame.message)...)
		default:
			got, ok := unread[unique]
			for !ok {
				reply := f.kernel.NextReply(t)
				replyUnique := binary.NativeEndian.Uint64(reply[8:16])
				if _, recorded := replied[replyUnique]; !recorded {
					t.Fatalf("the connection replied to request %d, which the recording holds no reply to", replyUnique)
				}
				unread[replyUnique] = reply
				got, ok = unread[unique]
			}
			delete(unread, unique)
			check(frame.message, got)
		}
	}
	if n := len(unread) + f.kernel.UnreadReplies(); n != 0 {
		t.Errorf("the connection sent %d replies the recording does not hold", n)
	}
	t.Logf("replayed %d requests and checked %d replies", len(requests), len(replied))
}
