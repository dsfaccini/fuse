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

// checkPanicLogged checks that line holds first on a line of its own, then a
// readable stack that reaches the panic in this file.
func checkPanicLogged(t *testing.T, line, first string) {
	t.Helper()
	stack, ok := strings.CutPrefix(line, first+"\n")
	if !ok || !strings.HasPrefix(stack, "goroutine ") || !strings.Contains(stack, "op_panic_test.go") {
		t.Errorf("error log = %q, want %q and then its stack; MountConfig.ErrorLogger states the format", line, first)
	}
}

// checkErrorLogged checks the line logged for an op's error.
func checkErrorLogged(t *testing.T, line, want string) {
	t.Helper()
	if line != want {
		t.Errorf("error log = %q, want %q; MountConfig.ErrorLogger states the format", line, want)
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
		logged  string
	}{
		{name: "lookup", request: request(fusekernel.OpLookup, parent, names("file")),
			logged: "Op 0x0000002a *fuseops.LookUpInodeOp] -> Error: panic: handler boom"},
		// ForgetInode runs on the goroutine that reads requests.
		{name: "forget", request: request(fusekernel.OpForget, child, fuse.NativeBytes(&fusekernel.ForgetIn{Nlookup: 1})), noReply: true,
			logged: "Op 0x0000002a *fuseops.ForgetInodeOp] -> Error: panic: handler boom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, lines := servePanicking(t, func(any) error { panic("handler boom") })
			k.Send(tt.request...)
			if !tt.noReply {
				checkReply(t, k.NextReply(t), syscall.EIO, nil)
			}
			checkPanicLogged(t, lines.next(t), tt.logged)
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
	checkErrorLogged(t, lines.next(t), "Op 0x0000002a *fuseops.LookUpInodeOp] -> Error: \"fuseutil: the method exited without returning: input/output error\"\n")
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
		// A debug line names its caller's source line after the request ID.
		if _, after, ok := strings.Cut(line, "] -> Callback: "); ok {
			checkPanicLogged(t, after, "panic: callback boom")
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
	checkPanicLogged(t, lines.next(t), "Op 0x0000002a *fuseops.ReadFileOp] -> Callback: panic: callback boom")
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
		logged  string
	}{
		{"read past Dst", request(fusekernel.OpRead, child, readIn), func(op any) {
			op.(*fuseops.ReadFileOp).BytesRead = 17
		}, `Op 0x0000002a *fuseops.ReadFileOp] -> Error: "*fuseops.ReadFileOp.BytesRead is 17, outside its 16-byte buffer: input/output error"`},
		{"read past Data", request(fusekernel.OpRead, child, readIn), func(op any) {
			o := op.(*fuseops.ReadFileOp)
			o.Data = [][]byte{[]byte("hello")}
			o.BytesRead = 6
		}, `Op 0x0000002a *fuseops.ReadFileOp] -> Error: "*fuseops.ReadFileOp.BytesRead is 6, outside its 5-byte buffer: input/output error"`},
		{"negative read", request(fusekernel.OpRead, child, readIn), func(op any) {
			op.(*fuseops.ReadFileOp).BytesRead = -1
		}, `Op 0x0000002a *fuseops.ReadFileOp] -> Error: "*fuseops.ReadFileOp.BytesRead is -1, outside its 16-byte buffer: input/output error"`},
		{"readdir past Dst", request(fusekernel.OpReaddir, child, readIn), func(op any) {
			op.(*fuseops.ReadDirOp).BytesRead = 17
		}, `Op 0x0000002a *fuseops.ReadDirOp] -> Error: "*fuseops.ReadDirOp.BytesRead is 17, outside its 16-byte buffer: input/output error"`},
		{"readdirplus past Dst", request(fusekernel.OpReaddirplus, child, readIn), func(op any) {
			op.(*fuseops.ReadDirPlusOp).BytesRead = 17
		}, `Op 0x0000002a *fuseops.ReadDirPlusOp] -> Error: "*fuseops.ReadDirPlusOp.BytesRead is 17, outside its 16-byte buffer: input/output error"`},
		{"listxattr past Dst", request(fusekernel.OpListxattr, child, fuse.NativeBytes(&fusekernel.ListxattrIn{Size: 16})), func(op any) {
			op.(*fuseops.ListXattrOp).BytesRead = 17
		}, `Op 0x0000002a *fuseops.ListXattrOp] -> Error: "*fuseops.ListXattrOp.BytesRead is 17, outside its 16-byte buffer: input/output error"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, lines := servePanicking(t, func(op any) error {
				tt.serve(op)
				return nil
			})
			k.Send(tt.request...)
			checkReply(t, k.NextReply(t), syscall.EIO, nil)
			checkErrorLogged(t, lines.next(t), tt.logged+"\n")
			checkStillServing(t, k)
			checkNoMoreLogs(t, lines)
		})
	}
}

func TestBytesReadThatFillsItsBufferIsAnswered(t *testing.T) {
	readIn := fuse.NativeBytes(&fusekernel.ReadIn{Fh: uint64(handle), Size: 16})
	zeroIn := fuse.NativeBytes(&fusekernel.ReadIn{Fh: uint64(handle), Size: 0})
	full := []byte("0123456789abcdef")
	k, lines := servePanicking(t, func(op any) error {
		switch o := op.(type) {
		case *fuseops.ReadFileOp:
			o.BytesRead = copy(o.Dst, full)
		case *fuseops.ReadDirOp:
			o.BytesRead = len(o.Dst)
		case *fuseops.ReadDirPlusOp:
			o.BytesRead = len(o.Dst)
		}
		return nil
	})
	tests := []struct {
		name    string
		request [][]byte
		reply   []byte
	}{
		{"read", request(fusekernel.OpRead, child, readIn), full},
		{"zero-size read", request(fusekernel.OpRead, child, zeroIn), nil},
		{"readdir", request(fusekernel.OpReaddir, child, readIn), make([]byte, 16)},
		{"zero-size readdir", request(fusekernel.OpReaddir, child, zeroIn), nil},
		{"readdirplus", request(fusekernel.OpReaddirplus, child, readIn), make([]byte, 16)},
	}
	for _, tt := range tests {
		k.Send(tt.request...)
		t.Run(tt.name, func(t *testing.T) { checkReply(t, k.NextReply(t), 0, tt.reply) })
	}
	checkNoMoreLogs(t, lines)
}
