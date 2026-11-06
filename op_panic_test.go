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

package fuse_test

import (
	"log"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dsfaccini/fuse"
	"github.com/dsfaccini/fuse/fuseops"
	"github.com/dsfaccini/fuse/fuseutil"
	"github.com/dsfaccini/fuse/internal/fusekernel"
)

// logLines hands each entry a log.Logger writes to the test.
type logLines chan string

func (l logLines) Write(p []byte) (int, error) {
	l <- string(p)
	return len(p), nil
}

func (l logLines) next(t *testing.T) string {
	t.Helper()
	select {
	case line := <-l:
		return line
	case <-time.After(5 * time.Second):
		t.Fatal("no log line within 5s")
		return ""
	}
}

// checkPanicLogged checks that line carries the panic value and a readable
// stack that reaches the panic in this file.
func checkPanicLogged(t *testing.T, line, value string) {
	t.Helper()
	if !strings.Contains(line, value) || !strings.Contains(line, "\ngoroutine ") || !strings.Contains(line, "op_panic_test.go") {
		t.Errorf("error log = %q, want the panic %q and its stack on lines of their own", line, value)
	}
}

// checkNoMoreLogs checks that nothing else was logged.
func checkNoMoreLogs(t *testing.T, lines logLines) {
	t.Helper()
	if n := len(lines); n != 0 {
		t.Errorf("the connection logged %d more lines, want none: %q", n, <-lines)
	}
}

// servePanicking serves through a file system that answers a getattr with
// attrs and hands every other op to handle.
func servePanicking(t *testing.T, handle func(op any) error) (fakeKernel, logLines) {
	lines := make(logLines, 10)
	fs := &protocolFS{serve: func(op any) error {
		if o, ok := op.(*fuseops.GetInodeAttributesOp); ok {
			o.Attributes = attrs
			return nil
		}
		return handle(op)
	}}
	k := fuse.ServeFakeKernelWithLoggers(t, fuseutil.NewFileSystemServer(fs), log.New(lines, "", 0), nil)
	return k, lines
}

// checkStillServing checks that the connection answers a getattr.
func checkStillServing(t *testing.T, k fakeKernel) {
	t.Helper()
	k.Send(request(fusekernel.OpGetattr, child, fuse.NativeBytes(&fusekernel.GetattrIn{}))...)
	checkReply(t, k.NextReply(t), 0, fuse.NativeBytes(&fusekernel.AttrOut{Attr: kernelAttr(child)}))
}

func TestHandlerPanicIsAnsweredWithEIO(t *testing.T) {
	tests := []struct {
		name    string
		request [][]byte
		noReply bool
	}{
		{name: "lookup", request: request(fusekernel.OpLookup, parent, names("file"))},
		// ForgetInode runs on the goroutine that reads requests.
		{name: "forget", request: request(fusekernel.OpForget, child, fuse.NativeBytes(&fusekernel.ForgetIn{Nlookup: 1})), noReply: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, lines := servePanicking(t, func(any) error { panic("handler boom") })
			k.Send(tt.request...)
			if !tt.noReply {
				checkReply(t, k.NextReply(t), syscall.EIO, nil)
			}
			checkPanicLogged(t, lines.next(t), "panic: handler boom")
			checkStillServing(t, k)
			checkNoMoreLogs(t, lines)
			if n := k.UnreadReplies(); n != 0 {
				t.Errorf("the connection sent %d more replies, want none", n)
			}
		})
	}
}

func TestHandlerGoexitIsAnsweredWithEIO(t *testing.T) {
	k, lines := servePanicking(t, func(any) error {
		runtime.Goexit()
		return nil
	})
	k.Send(request(fusekernel.OpLookup, parent, names("file"))...)
	checkReply(t, k.NextReply(t), syscall.EIO, nil)
	if line := lines.next(t); !strings.Contains(line, "exited without returning") {
		t.Errorf("error log = %q, want the method's exit", line)
	}
	checkStillServing(t, k)
	checkNoMoreLogs(t, lines)
}

// With no ErrorLogger, a Callback panic goes to the DebugLogger, as a
// handler panic does.
func TestCallbackPanicGoesToTheDebugLogger(t *testing.T) {
	lines := make(logLines, 10)
	fs := &protocolFS{serve: func(op any) error {
		o := op.(*fuseops.ReadFileOp)
		o.Callback = func() { panic("callback boom") }
		return nil
	}}
	k := fuse.ServeFakeKernelWithLoggers(t, fuseutil.NewFileSystemServer(fs), nil, log.New(lines, "", 0))
	k.Send(request(fusekernel.OpRead, child, fuse.NativeBytes(&fusekernel.ReadIn{Fh: uint64(handle), Size: 16}))...)
	checkReply(t, k.NextReply(t), 0, nil)
	for {
		line := lines.next(t)
		if strings.Contains(line, "Callback: panic: callback boom") {
			checkPanicLogged(t, line, "callback boom")
			return
		}
	}
}

// A Callback runs inside Reply, after the op is answered. Answering its panic
// would be a second Reply, which is fatal to the connection.
func TestCallbackPanicKeepsTheReply(t *testing.T) {
	k, lines := servePanicking(t, func(op any) error {
		o := op.(*fuseops.ReadFileOp)
		o.BytesRead = copy(o.Dst, "hello")
		o.Callback = func() { panic("callback boom") }
		return nil
	})
	readIn := fuse.NativeBytes(&fusekernel.ReadIn{Fh: uint64(handle), Size: 1 << 16})
	k.Send(request(fusekernel.OpRead, child, readIn)...)
	checkReply(t, k.NextReply(t), 0, []byte("hello"))
	checkPanicLogged(t, lines.next(t), "callback boom")
	checkStillServing(t, k)
	checkNoMoreLogs(t, lines)
	if n := k.UnreadReplies(); n != 0 {
		t.Errorf("the connection sent %d more replies, want none", n)
	}
}

func TestBytesReadOutsideItsBufferIsAnsweredWithEIO(t *testing.T) {
	readIn := fuse.NativeBytes(&fusekernel.ReadIn{Fh: uint64(handle), Size: 16})
	tests := []struct {
		name    string
		request [][]byte
		serve   func(op any)
	}{
		{"read past Dst", request(fusekernel.OpRead, child, readIn), func(op any) {
			op.(*fuseops.ReadFileOp).BytesRead = 17
		}},
		{"read past Data", request(fusekernel.OpRead, child, readIn), func(op any) {
			o := op.(*fuseops.ReadFileOp)
			o.Data = [][]byte{[]byte("hello")}
			o.BytesRead = 6
		}},
		{"negative read", request(fusekernel.OpRead, child, readIn), func(op any) {
			op.(*fuseops.ReadFileOp).BytesRead = -1
		}},
		{"readdir past Dst", request(fusekernel.OpReaddir, child, readIn), func(op any) {
			op.(*fuseops.ReadDirOp).BytesRead = 17
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, lines := servePanicking(t, func(op any) error {
				tt.serve(op)
				return nil
			})
			k.Send(tt.request...)
			checkReply(t, k.NextReply(t), syscall.EIO, nil)
			if line := lines.next(t); !strings.Contains(line, "BytesRead is ") {
				t.Errorf("error log = %q, want the bad BytesRead", line)
			}
			checkStillServing(t, k)
			checkNoMoreLogs(t, lines)
		})
	}
}
