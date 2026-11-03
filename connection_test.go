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
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/dsfaccini/fuse/fuseops"
	"github.com/dsfaccini/fuse/internal/buffer"
)

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
			c := &Connection{
				cfg: MountConfig{
					MaxPages: tc.inputPages,
					MaxWrite: tc.inputWrite,
				},
			}

			err := c.sanitizeMaxPagesAndWrite()

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
				if c.cfg.MaxPages != tc.wantPages {
					t.Errorf("MaxPages = %d, want %d", c.cfg.MaxPages, tc.wantPages)
				}
				if c.cfg.MaxWrite != tc.wantWrite {
					t.Errorf("MaxWrite = %d, want %d", c.cfg.MaxWrite, tc.wantWrite)
				}
			}
		})
	}
}
