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
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/dsfaccini/fuse/fuseops"
	"github.com/dsfaccini/fuse/internal/buffer"
	"github.com/dsfaccini/fuse/internal/fusekernel"
)

// A nil *os.PathError panics when errors.As or errors.Is unwraps it.
var nilPathErr error = (*os.PathError)(nil)

func TestKernelResponseKeepsWrappedErrno(t *testing.T) {
	tests := []struct {
		name  string
		opErr error
		want  syscall.Errno
	}{
		{"bare errno", syscall.ENOENT, syscall.ENOENT},
		{"wrapped errno", fmt.Errorf("lookup: %w", syscall.ENOENT), syscall.ENOENT},
		{"path error", &os.PathError{Op: "open", Path: "f", Err: syscall.EACCES}, syscall.EACCES},
		{"no errno", errors.New("lookup failed"), syscall.EIO},
		{"EIO before ENOENT", fmt.Errorf("lookup: %w (cleanup: %w)", syscall.EIO, syscall.ENOENT), syscall.EIO},
		{"joined EACCES and ENOENT", errors.Join(syscall.EACCES, syscall.ENOENT), syscall.EACCES},
		{"nil path error", nilPathErr, syscall.EIO},
		{"wrapped nil path error", fmt.Errorf("lookup: %w", nilPathErr), syscall.EIO},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c Connection
			var m buffer.OutMessage
			m.Reset()
			c.kernelResponse(&m, 1, &fuseops.LookUpInodeOp{}, tt.opErr)
			if got := m.OutHeader().Error; got != -int32(tt.want) {
				t.Errorf("reply error = %d, want %d", got, -int32(tt.want))
			}
		})
	}
}

func TestShouldLogErrorMatchesWrappedErrno(t *testing.T) {
	wrap := func(errno syscall.Errno) error { return fmt.Errorf("op: %w", errno) }
	tests := []struct {
		name string
		op   interface{}
		err  error
		want bool
	}{
		{"lookup ENOENT", &fuseops.LookUpInodeOp{}, wrap(syscall.ENOENT), false},
		{"getxattr ENODATA", &fuseops.GetXattrOp{}, wrap(syscall.ENODATA), false},
		{"listxattr ERANGE", &fuseops.ListXattrOp{}, wrap(syscall.ERANGE), false},
		{"getxattr ENOSYS", &fuseops.GetXattrOp{}, wrap(syscall.ENOSYS), false},
		{"unknown op ENOSYS", &unknownOp{}, wrap(syscall.ENOSYS), false},
		{"lookup EIO", &fuseops.LookUpInodeOp{}, wrap(syscall.EIO), true},
		{"lookup EIO before ENOENT", &fuseops.LookUpInodeOp{}, fmt.Errorf("lookup: %w (cleanup: %w)", syscall.EIO, syscall.ENOENT), true},
		{"lookup joined EACCES and ENOENT", &fuseops.LookUpInodeOp{}, errors.Join(syscall.EACCES, syscall.ENOENT), true},
		{"lookup no errno", &fuseops.LookUpInodeOp{}, errors.New("lookup failed"), true},
		{"lookup nil path error", &fuseops.LookUpInodeOp{}, nilPathErr, true},
		{"lookup wrapped nil path error", &fuseops.LookUpInodeOp{}, fmt.Errorf("lookup: %w", nilPathErr), true},
	}
	c := &Connection{errorLogger: log.New(io.Discard, "", 0)}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := c.shouldLogError(tt.op, tt.err); got != tt.want {
				t.Errorf("shouldLogError = %v, want %v", got, tt.want)
			}
		})
	}
}

// The kernel answers a reply to an interrupted request with ENOENT.
func TestReplyTreatsENOENTWriteAsBenign(t *testing.T) {
	tests := []struct {
		name     string
		writeErr error
		wantErr  bool
	}{
		{"bare ENOENT", syscall.ENOENT, false},
		{"wrapped ENOENT", fmt.Errorf("MFChannelSendMessage: %w", syscall.ENOENT), false},
		{"EIO", syscall.EIO, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logged bytes.Buffer
			c := &Connection{
				cfg:              MountConfig{OpContext: context.Background()},
				errorLogger:      log.New(&logged, "", 0),
				transport:        &testTransport{messages: [][]byte{makeTestInMessage(7)}, writeErr: tt.writeErr},
				cancelFuncs:      make(map[uint64]func()),
				inMessageBufSize: buffer.GetPageSize(),
			}
			ctx, _, err := c.ReadOp()
			if err != nil {
				t.Fatalf("ReadOp: %v", err)
			}

			err = c.Reply(ctx, syscall.ENOSYS)
			if (err != nil) != tt.wantErr {
				t.Errorf("Reply = %v, want error: %v", err, tt.wantErr)
			}
			if (logged.Len() != 0) != tt.wantErr {
				t.Errorf("logged %q, want a log: %v", logged.String(), tt.wantErr)
			}
		})
	}
}

func TestSanitizeMaxPagesAndWrite(t *testing.T) {
	pageSize := uint32(buffer.GetPageSize())
	defaultMaxWrite := uint32(buffer.MaxWriteSize)
	defaultMaxPages := uint16(max(uint32(buffer.MaxReadSize), uint32(buffer.MaxWriteSize)) / pageSize)

	tests := []struct {
		name       string
		inputPages uint16
		inputWrite uint32
		wantPages  uint16
		wantWrite  uint32
		wantErr    bool
		errSubstr  string
	}{
		{
			name:       "neither given",
			inputPages: 0,
			inputWrite: 0,
			wantPages:  defaultMaxPages,
			wantWrite:  defaultMaxWrite,
			wantErr:    false,
		},
		{
			name:       "only MaxWrite given (128KB)",
			inputPages: 0,
			inputWrite: 128 * 1024,
			wantPages:  defaultMaxPages,
			wantWrite:  128 * 1024,
			wantErr:    false,
		},
		{
			name:       "only MaxWrite given (not page aligned, 129KB)",
			inputPages: 0,
			inputWrite: 129 * 1024,
			wantPages:  defaultMaxPages,
			wantWrite:  129 * 1024,
			wantErr:    false,
		},
		{
			name:       "only MaxWrite given (larger than 1MB, e.g. 2MB)",
			inputPages: 0,
			inputWrite: 2 * 1024 * 1024,
			wantPages:  uint16(2 * 1024 * 1024 / pageSize),
			wantWrite:  2 * 1024 * 1024,
			wantErr:    false,
		},
		{
			name:       "only MaxPages given (valid, e.g. 256)",
			inputPages: 256,
			inputWrite: 0,
			wantPages:  256,
			wantWrite:  256 * pageSize,
			wantErr:    false,
		},
		{
			name:       "only MaxPages given (small, e.g. 32)",
			inputPages: 32,
			inputWrite: 0,
			wantPages:  32,
			wantWrite:  32 * pageSize,
			wantErr:    false,
		},
		{
			name:       "only MaxPages given (very small, e.g. 1)",
			inputPages: 1,
			inputWrite: 0,
			wantPages:  1,
			wantWrite:  pageSize,
			wantErr:    false,
		},
		{
			name:       "both given (valid, e.g. MaxPages=32, MaxWrite=128KB)",
			inputPages: 32,
			inputWrite: 128 * 1024,
			wantPages:  32,
			wantWrite:  128 * 1024,
			wantErr:    false,
		},
		{
			name:       "both given (invalid, MaxWrite one page above MaxPages=16)",
			inputPages: 16,
			inputWrite: 17 * pageSize,
			wantPages:  16,
			wantWrite:  17 * pageSize,
			wantErr:    true,
			errSubstr:  "must be at least MaxWrite",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := MountConfig{
				MaxPages: tc.inputPages,
				MaxWrite: tc.inputWrite,
			}

			err := cfg.sanitizeMaxPagesAndWrite()

			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tc.errSubstr) {
					t.Errorf("expected error to contain %q, got: %v", tc.errSubstr, err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if cfg.MaxPages != tc.wantPages {
					t.Errorf("MaxPages = %d, want %d", cfg.MaxPages, tc.wantPages)
				}
				if cfg.MaxWrite != tc.wantWrite {
					t.Errorf("MaxWrite = %d, want %d", cfg.MaxWrite, tc.wantWrite)
				}
			}
		})
	}
}

func TestFormatWireLogEntryStatus(t *testing.T) {
	tests := []struct {
		name  string
		opErr error
		want  int
	}{
		{"wrapped errno", fmt.Errorf("lookup: %w", syscall.ENOENT), int(syscall.ENOENT)},
		{"no errno", errors.New("lookup failed"), 0},
		{"nil path error", nilPathErr, 0},
		{"wrapped nil path error", fmt.Errorf("lookup: %w", nilPathErr), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wlog := NewWireLogRecord()
			if _, err := formatWireLogEntry(&fuseops.LookUpInodeOp{}, tt.opErr, wlog); err != nil {
				t.Fatalf("formatWireLogEntry: %v", err)
			}
			if wlog.Status != tt.want {
				t.Errorf("Status = %d, want %d", wlog.Status, tt.want)
			}
		})
	}
}

// fmt recovers from a panic in Error when it formats the error value itself.
func TestReplyDebugLogsOpError(t *testing.T) {
	tests := []struct {
		name        string
		opErr       error
		errorLogger bool
		want        string
	}{
		{"ordinary error", errors.New("lookup failed"), false, `-> Error: "lookup failed"`},
		{"nil path error", nilPathErr, false, "-> Error: <nil>"},
		{"suppressed errno joined with nil path error", errors.Join(syscall.ENOSYS, nilPathErr), true,
			"-> Error: %!q(PANIC=Error method: runtime error: invalid memory address or nil pointer dereference)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var debugged bytes.Buffer
			c := &Connection{
				cfg:              MountConfig{OpContext: context.Background()},
				debugLogger:      log.New(&debugged, "", 0),
				transport:        &testTransport{messages: [][]byte{makeTestInMessage(7)}},
				cancelFuncs:      make(map[uint64]func()),
				inMessageBufSize: buffer.GetPageSize(),
			}
			if tt.errorLogger {
				c.errorLogger = log.New(io.Discard, "", 0)
			}
			ctx, _, err := c.ReadOp()
			if err != nil {
				t.Fatalf("ReadOp: %v", err)
			}

			if err := c.Reply(ctx, tt.opErr); err != nil {
				t.Errorf("Reply: %v", err)
			}
			if !strings.HasSuffix(debugged.String(), "] "+tt.want+"\n") {
				t.Errorf("debug log = %q, want it to end with %q", debugged.String(), tt.want)
			}
		})
	}
}

// newTestConnection returns a connection over transport, configured the way
// Mount and Init leave it.
func newTestConnection(transport connectionTransport) *Connection {
	cfg, err := (&MountConfig{}).normalize()
	if err != nil {
		panic(err)
	}
	return &Connection{
		cfg:              cfg,
		transport:        transport,
		protocol:         fusekernel.Protocol{Major: fusekernel.ProtoVersionMaxMajor, Minor: fusekernel.ProtoVersionMaxMinor},
		cancelFuncs:      make(map[uint64]func()),
		inMessageBufSize: buffer.GetPageSize() + int(cfg.MaxWrite),
	}
}

// testMessage frames the body parts as one kernel request.
func testMessage(opcode uint32, unique uint64, body ...[]byte) []byte {
	header := fusekernel.InHeader{Opcode: opcode, Unique: unique}
	message := append([]byte(nil), structBytes(&header)...)
	for _, part := range body {
		message = append(message, part...)
	}
	binary.NativeEndian.PutUint32(message[0:4], uint32(len(message)))
	return message
}

// structBytes views a kernel struct as the bytes on the wire.
func structBytes[T any](v *T) []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(v)), unsafe.Sizeof(*v))
}

func replyHeader(reply []byte) fusekernel.OutHeader {
	var h fusekernel.OutHeader
	copy(structBytes(&h), reply)
	return h
}

// drainFreelists empties the connection's freelists and returns what they
// held.
func drainFreelists(c *Connection) (in []*buffer.InMessage, out []*buffer.OutMessage) {
	for p := c.inMessages.Get(); p != nil; p = c.inMessages.Get() {
		in = append(in, (*buffer.InMessage)(p))
	}
	for p := c.outMessages.Get(); p != nil; p = c.outMessages.Get() {
		out = append(out, (*buffer.OutMessage)(p))
	}
	return in, out
}

func TestReadOpAnswersBodyThatFailsToDecode(t *testing.T) {
	limit := uint32(maxReplySize(&newTestConnection(nil).cfg))
	oversized := fusekernel.ReadIn{Size: limit + 1}
	var oversizedXattr fusekernel.GetxattrIn
	oversizedXattr.Size = limit + 1
	oversizedList := fusekernel.ListxattrIn{Size: limit + 1}
	write := fusekernel.WriteIn{Size: math.MaxUint32}
	renameIn := fusekernel.RenameIn{Newdir: 1}
	tests := []struct {
		name   string
		opcode uint32
		body   []byte
		// Zero means no reply.
		want syscall.Errno
	}{
		{"lookup without NUL", fusekernel.OpLookup, []byte("a"), syscall.EIO},
		{"symlink with one name", fusekernel.OpSymlink, []byte("\x00"), syscall.EIO},
		{"rename with one name", fusekernel.OpRename, append(structBytes(&renameIn), "abc\x00"...), syscall.EIO},
		// Size does not fit a 32-bit int.
		{"write shorter than a huge size", fusekernel.OpWrite, append(structBytes(&write), "abc"...), syscall.EIO},
		{"readdir above MaxPages", fusekernel.OpReaddir, structBytes(&oversized), syscall.EINVAL},
		{"readdirplus above MaxPages", fusekernel.OpReaddirplus, structBytes(&oversized), syscall.EINVAL},
		{"getxattr above MaxPages", fusekernel.OpGetxattr, append(structBytes(&oversizedXattr), "user.a\x00"...), syscall.EINVAL},
		{"listxattr above MaxPages", fusekernel.OpListxattr, structBytes(&oversizedList), syscall.EINVAL},
		{"forget", fusekernel.OpForget, nil, 0},
		{"batch forget", fusekernel.OpBatchForget, nil, 0},
		{"interrupt", fusekernel.OpInterrupt, nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := &testTransport{messages: [][]byte{
				testMessage(tt.opcode, 5, tt.body),
				testMessage(fusekernel.OpGetattr, 6),
			}}
			c := newTestConnection(transport)
			inMsg := buffer.NewInMessage(c.inMessageBufSize)
			c.putInMessage(inMsg)
			outMsg := new(buffer.OutMessage)
			c.putOutMessage(outMsg)

			ctx, op, err := c.ReadOp()
			if err != nil {
				t.Fatalf("ReadOp: %v", err)
			}
			if getattr, ok := op.(*fuseops.GetInodeAttributesOp); !ok || getattr.OpContext.FuseID != 6 {
				t.Fatalf("ReadOp = %#v, want the GETATTR that follows", op)
			}
			state := ctx.Value(contextKey).(opState)
			if state.inMsg != inMsg || state.outMsg != outMsg {
				t.Errorf("the next request did not reuse the corrupt request's messages")
			}

			if tt.want == 0 {
				if len(transport.writes) != 0 {
					t.Errorf("replies = %x, want none", transport.writes)
				}
				return
			}
			if len(transport.writes) != 1 {
				t.Fatalf("replies = %d, want 1", len(transport.writes))
			}
			want := fusekernel.OutHeader{Len: uint32(buffer.OutMessageHeaderSize), Error: -int32(tt.want), Unique: 5}
			if got := replyHeader(transport.writes[0]); got != want || len(transport.writes[0]) != buffer.OutMessageHeaderSize {
				t.Errorf("reply = %+v (%d bytes), want %+v", got, len(transport.writes[0]), want)
			}
		})
	}
}

// Init needs the INIT request, so reading on past a corrupt one would wait
// for a request the kernel never sends.
func TestInitFailsOnInitThatFailsToDecode(t *testing.T) {
	valid := fusekernel.InitIn{Major: fusekernel.ProtoVersionMaxMajor, Minor: fusekernel.ProtoVersionMaxMinor}
	transport := &testTransport{messages: [][]byte{
		testMessage(fusekernel.OpInit, 1),
		testMessage(fusekernel.OpInit, 2, structBytes(&valid)),
	}}
	c := newTestConnection(transport)
	if err := c.Init(); err == nil {
		t.Fatal("Init succeeded, want an error")
	}
	if transport.reads != 1 {
		t.Errorf("reads = %d, want 1", transport.reads)
	}
	if len(transport.writes) != 1 || replyHeader(transport.writes[0]).Error != -int32(syscall.EIO) {
		t.Errorf("replies = %x, want one EIO", transport.writes)
	}
}

func TestReadOpErrorIsFatal(t *testing.T) {
	transportErr := errors.New("transport broke")
	wrongLen := testMessage(fusekernel.OpGetattr, 5)
	binary.NativeEndian.PutUint32(wrongLen[0:4], uint32(len(wrongLen)+1))
	tests := []struct {
		name     string
		messages [][]byte
		readErr  error
		// The number of ops ReadOp returns before the error.
		ops int
	}{
		{"short message", [][]byte{make([]byte, 10)}, nil, 0},
		{"header length mismatch", [][]byte{wrongLen}, nil, 0},
		{"transport error", nil, transportErr, 0},
		{"duplicate request ID", [][]byte{testMessage(fusekernel.OpGetattr, 5), testMessage(fusekernel.OpGetattr, 5)}, nil, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := &testTransport{messages: tt.messages, readErr: tt.readErr}
			c := newTestConnection(transport)
			for range tt.ops {
				if _, _, err := c.ReadOp(); err != nil {
					t.Fatalf("ReadOp: %v", err)
				}
			}

			_, _, err := c.ReadOp()
			if err == nil || err == io.EOF {
				t.Fatalf("ReadOp = %v, want a fatal error", err)
			}
			if tt.readErr != nil && !errors.Is(err, tt.readErr) {
				t.Errorf("ReadOp = %v, want it to wrap %v", err, tt.readErr)
			}
			reads := transport.reads
			if _, _, again := c.ReadOp(); again != err {
				t.Errorf("second ReadOp = %v, want %v", again, err)
			}
			if transport.reads != reads {
				t.Errorf("second ReadOp read %d more messages, want 0", transport.reads-reads)
			}
			if len(transport.writes) != 0 {
				t.Errorf("replies = %x, want none", transport.writes)
			}
			if got := c.fatalError(); got != err {
				t.Errorf("fatalError = %v, want %v", got, err)
			}
		})
	}
}

func TestSecondReplyIsFatal(t *testing.T) {
	read := fusekernel.ReadIn{Size: 1}
	transport := &testTransport{messages: [][]byte{
		testMessage(fusekernel.OpRead, 5, structBytes(&read)),
		testMessage(fusekernel.OpGetattr, 6),
	}}
	c := newTestConnection(transport)
	ctx, op, err := c.ReadOp()
	if err != nil {
		t.Fatalf("ReadOp: %v", err)
	}
	callbacks := 0
	op.(*fuseops.ReadFileOp).Callback = func() { callbacks++ }
	if err := c.Reply(ctx, syscall.ENOSYS); err != nil {
		t.Fatalf("Reply: %v", err)
	}

	err = c.Reply(ctx, syscall.ENOSYS)
	if err == nil {
		t.Fatal("second Reply succeeded, want an error")
	}
	if len(transport.writes) != 1 {
		t.Errorf("replies = %d, want 1", len(transport.writes))
	}
	if callbacks != 1 {
		t.Errorf("Callback ran %d times, want 1", callbacks)
	}
	// The first Reply recycled the messages. Recycling them again would hand
	// one buffer to two requests.
	if in, out := drainFreelists(c); len(in) != 1 || len(out) != 1 {
		t.Errorf("freelists hold %d in and %d out messages, want 1 and 1", len(in), len(out))
	}
	if _, _, readErr := c.ReadOp(); readErr != err {
		t.Errorf("ReadOp = %v, want %v", readErr, err)
	}
	if transport.reads != 1 {
		t.Errorf("reads = %d, want 1", transport.reads)
	}
}

func TestSecondReplyAfterMessagesAreReused(t *testing.T) {
	transport := &testTransport{messages: [][]byte{
		testMessage(fusekernel.OpGetattr, 5),
		testMessage(fusekernel.OpGetattr, 6),
	}}
	c := newTestConnection(transport)
	ctx5, _, err := c.ReadOp()
	if err != nil {
		t.Fatalf("ReadOp: %v", err)
	}
	if err := c.Reply(ctx5, syscall.ENOSYS); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	ctx6, _, err := c.ReadOp()
	if err != nil {
		t.Fatalf("ReadOp: %v", err)
	}
	if ctx5.Value(contextKey).(opState).inMsg != ctx6.Value(contextKey).(opState).inMsg {
		t.Fatal("request 6 did not reuse request 5's InMessage")
	}

	if err := c.Reply(ctx5, syscall.ENOSYS); err == nil {
		t.Error("second Reply to request 5 succeeded, want an error")
	}
	if len(transport.writes) != 1 {
		t.Errorf("replies = %d, want 1", len(transport.writes))
	}
	if in, out := drainFreelists(c); len(in) != 0 || len(out) != 0 {
		t.Errorf("freelists hold request 6's messages: %d in, %d out", len(in), len(out))
	}
}

// The macFUSE kext reuses a request ID once its reply is written
// (osxfuse#208).
func TestSecondReplyAfterRequestIDIsReused(t *testing.T) {
	transport := &testTransport{messages: [][]byte{
		testMessage(fusekernel.OpGetattr, 5),
		testMessage(fusekernel.OpGetattr, 5),
	}}
	c := newTestConnection(transport)
	old, _, err := c.ReadOp()
	if err != nil {
		t.Fatalf("ReadOp: %v", err)
	}
	if err := c.Reply(old, syscall.ENOSYS); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	cur, _, err := c.ReadOp()
	if err != nil {
		t.Fatalf("ReadOp: %v", err)
	}

	if err := c.Reply(old, syscall.ENOSYS); err == nil {
		t.Error("second Reply to the old request succeeded, want an error")
	}
	if len(transport.writes) != 1 {
		t.Errorf("replies = %d, want 1", len(transport.writes))
	}
	if cur.Err() != nil {
		t.Errorf("the request in flight under ID 5 was cancelled: %v", cur.Err())
	}
	if in, out := drainFreelists(c); len(in) != 0 || len(out) != 0 {
		t.Errorf("freelists hold the in-flight request's messages: %d in, %d out", len(in), len(out))
	}
}

// finishOp keeps no state for FORGET, so only the op itself can tell that it
// was answered.
func TestSecondReplyToForgetIsFatal(t *testing.T) {
	forget := fusekernel.ForgetIn{Nlookup: 1}
	c := newTestConnection(&testTransport{messages: [][]byte{testMessage(fusekernel.OpForget, 5, structBytes(&forget))}})
	ctx, _, err := c.ReadOp()
	if err != nil {
		t.Fatalf("ReadOp: %v", err)
	}
	if err := c.Reply(ctx, nil); err != nil {
		t.Fatalf("Reply: %v", err)
	}

	if err := c.Reply(ctx, nil); err == nil {
		t.Error("second Reply succeeded, want an error")
	}
	if in, out := drainFreelists(c); len(in) != 1 || len(out) != 1 {
		t.Errorf("freelists hold %d in and %d out messages, want 1 and 1", len(in), len(out))
	}
}

// An answer under that ID would answer the request in flight.
func TestReadOpCorruptBodyWithInFlightIDIsFatal(t *testing.T) {
	transport := &testTransport{messages: [][]byte{
		testMessage(fusekernel.OpGetattr, 5),
		testMessage(fusekernel.OpLookup, 5, []byte("a")),
		testMessage(fusekernel.OpGetattr, 6),
	}}
	c := newTestConnection(transport)
	if _, _, err := c.ReadOp(); err != nil {
		t.Fatalf("ReadOp: %v", err)
	}

	_, op, err := c.ReadOp()
	if err == nil || err == io.EOF {
		t.Fatalf("ReadOp = %T, %v; want a fatal error", op, err)
	}
	if len(transport.writes) != 0 {
		t.Errorf("replies = %x, want none", transport.writes)
	}
	if transport.reads != 2 {
		t.Errorf("reads = %d, want 2", transport.reads)
	}
}

// blockingTransport blocks each read until the test sends it a message.
type blockingTransport struct {
	reading  chan struct{}
	messages chan []byte
}

func (t *blockingTransport) ReadMessage(dst []byte) (int, error) {
	t.reading <- struct{}{}
	return copy(dst, <-t.messages), nil
}

func (t *blockingTransport) WriteMessage([]byte) error    { return nil }
func (t *blockingTransport) WritevMessage([][]byte) error { return nil }
func (t *blockingTransport) Close() error                 { return nil }

func TestReadOpStopsAfterFatalErrorDuringRead(t *testing.T) {
	transport := &blockingTransport{reading: make(chan struct{}, 1), messages: make(chan []byte, 1)}
	c := newTestConnection(transport)
	transport.messages <- testMessage(fusekernel.OpGetattr, 5)
	ctx, _, err := c.ReadOp()
	<-transport.reading
	if err != nil {
		t.Fatalf("ReadOp: %v", err)
	}
	if err := c.Reply(ctx, syscall.ENOSYS); err != nil {
		t.Fatalf("Reply: %v", err)
	}

	type result struct {
		op  interface{}
		err error
	}
	done := make(chan result)
	go func() {
		_, op, err := c.ReadOp()
		done <- result{op, err}
	}()
	<-transport.reading
	fatal := c.Reply(ctx, syscall.ENOSYS)
	if fatal == nil {
		t.Fatal("second Reply succeeded, want an error")
	}
	transport.messages <- testMessage(fusekernel.OpGetattr, 7)
	if r := <-done; r.op != nil || r.err != fatal {
		t.Errorf("ReadOp = %T, %v; want nil, %v", r.op, r.err, fatal)
	}
}

func TestReadOpRecyclesInterrupt(t *testing.T) {
	interrupt := fusekernel.InterruptIn{Unique: 99}
	c := newTestConnection(&testTransport{messages: [][]byte{testMessage(fusekernel.OpInterrupt, 5, structBytes(&interrupt))}})
	inMsg := buffer.NewInMessage(c.inMessageBufSize)
	c.putInMessage(inMsg)
	outMsg := new(buffer.OutMessage)
	c.putOutMessage(outMsg)

	if _, _, err := c.ReadOp(); err != io.EOF {
		t.Fatalf("ReadOp = %v, want io.EOF", err)
	}
	in, out := drainFreelists(c)
	if len(in) != 1 || in[0] != inMsg || len(out) != 1 || out[0] != outMsg {
		t.Errorf("freelists hold %d in and %d out messages, want the interrupt's two", len(in), len(out))
	}
}

// A failed answer to a body that fails to decode is logged, as Reply logs one.
func TestReadOpRejectWriteErrorIsNotFatal(t *testing.T) {
	tests := []struct {
		name     string
		writeErr error
		wantLog  bool
	}{
		{"EIO", syscall.EIO, true},
		{"ENOENT", syscall.ENOENT, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logged bytes.Buffer
			c := newTestConnection(&testTransport{
				messages: [][]byte{testMessage(fusekernel.OpLookup, 5, []byte("a")), testMessage(fusekernel.OpGetattr, 6)},
				writeErr: tt.writeErr,
			})
			c.errorLogger = log.New(&logged, "", 0)

			if _, op, err := c.ReadOp(); err != nil {
				t.Fatalf("ReadOp = %v, want the GETATTR", err)
			} else if _, ok := op.(*fuseops.GetInodeAttributesOp); !ok {
				t.Fatalf("ReadOp = %T, want the GETATTR", op)
			}
			if got := strings.Contains(logged.String(), "writeMessage"); got != tt.wantLog {
				t.Errorf("logged %q, want a writeMessage log: %v", logged.String(), tt.wantLog)
			}
		})
	}
}

// The cap follows MaxPages, with per-OS floors for requests whose size the
// kernel does not bound by max_pages.
func TestReadOpCapFloors(t *testing.T) {
	pageSize := uint32(buffer.GetPageSize())
	// Linux's XATTR_SIZE_MAX, or the osxfuse kext's FUSE_REASONABLE_XATTRSIZE.
	xattrFloor := uint32(64 << 10)
	// The osxfuse kext asks for directory reads up to the mount's iosize.
	dirFloor := uint32(0)
	if runtime.GOOS == "darwin" {
		xattrFloor = 128 << 10
		dirFloor = buffer.MaxWriteSize
	}
	xattrLimit := max(pageSize, xattrFloor)
	dirLimit := max(pageSize, dirFloor)
	readdir := func(size uint32) []byte {
		in := fusekernel.ReadIn{Size: size}
		return structBytes(&in)
	}
	getxattr := func(size uint32) []byte {
		var in fusekernel.GetxattrIn
		in.Size = size
		return append(structBytes(&in), "user.a\x00"...)
	}
	listxattr := func(size uint32) []byte {
		in := fusekernel.ListxattrIn{Size: size}
		return structBytes(&in)
	}
	tests := []struct {
		name    string
		opcode  uint32
		body    []byte
		wantErr bool
	}{
		{"readdir at the cap", fusekernel.OpReaddir, readdir(dirLimit), false},
		{"readdir above the cap", fusekernel.OpReaddir, readdir(dirLimit + 1), true},
		{"readdirplus at the cap", fusekernel.OpReaddirplus, readdir(dirLimit), false},
		{"readdirplus above the cap", fusekernel.OpReaddirplus, readdir(dirLimit + 1), true},
		{"getxattr at the cap", fusekernel.OpGetxattr, getxattr(xattrLimit), false},
		{"getxattr above the cap", fusekernel.OpGetxattr, getxattr(xattrLimit + 1), true},
		{"listxattr at the cap", fusekernel.OpListxattr, listxattr(xattrLimit), false},
		{"listxattr above the cap", fusekernel.OpListxattr, listxattr(xattrLimit + 1), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestConnection(nil)
			c.cfg.MaxPages = 1
			_, _, err := convertTestMessage(t, c, testMessage(tt.opcode, 1, tt.body))
			if gotErr := errors.Is(err, syscall.EINVAL); gotErr != tt.wantErr || (err != nil && !gotErr) {
				t.Errorf("convertInMessage = %v, want EINVAL: %v", err, tt.wantErr)
			}
		})
	}
}

// fuse_dev_do_read fails with ECONNABORTED instead of ENODEV when a read races
// the connection's abort, as an unmount does
// (https://github.com/torvalds/linux/blob/05441d697f7f9a927264f4058cbbc6a03dfcac9d/fs/fuse/dev.c#L1627-L1629).
func TestReadOpTreatsECONNABORTEDAsEOFOnLinux(t *testing.T) {
	readErr := &os.PathError{Op: "read", Path: "/dev/fuse", Err: syscall.ECONNABORTED}
	c := newTestConnection(&testTransport{readErr: readErr})

	_, _, err := c.ReadOp()
	if runtime.GOOS != "linux" {
		if err == nil || err == io.EOF {
			t.Errorf("ReadOp = %v, want a fatal error", err)
		}
		return
	}
	if err != io.EOF {
		t.Errorf("ReadOp = %v, want io.EOF", err)
	}
	if fatal := c.fatalError(); fatal != nil {
		t.Errorf("fatalError = %v, want nil", fatal)
	}
}

// overlapWriter records whether two Writes ever ran at once.
type overlapWriter struct {
	active  atomic.Int32
	overlap atomic.Bool
	writes  atomic.Int32
}

func (w *overlapWriter) Write(p []byte) (int, error) {
	if w.active.Add(1) > 1 {
		w.overlap.Store(true)
	}
	time.Sleep(time.Millisecond)
	w.active.Add(-1)
	w.writes.Add(1)
	return len(p), nil
}

func TestReplySerializesWireLogWrites(t *testing.T) {
	const n = 16
	var messages [][]byte
	for i := range n {
		messages = append(messages, testMessage(fusekernel.OpGetattr, uint64(10+i)))
	}
	c := newTestConnection(&testTransport{messages: messages})
	w := &overlapWriter{}
	c.wireLogger = w

	var ctxs []context.Context
	for range n {
		ctx, _, err := c.ReadOp()
		if err != nil {
			t.Fatalf("ReadOp: %v", err)
		}
		ctxs = append(ctxs, ctx)
	}
	var wg sync.WaitGroup
	for _, ctx := range ctxs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Reply(ctx, nil)
		}()
	}
	wg.Wait()

	if w.overlap.Load() {
		t.Error("Reply wrote two wire log entries at once")
	}
	if got := w.writes.Load(); got != n {
		t.Errorf("wire log writes = %d, want %d", got, n)
	}
}
