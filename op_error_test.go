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
	"errors"
	"fmt"
	"log"
	"os"
	"strconv"
	"syscall"
	"testing"

	"github.com/dsfaccini/fuse"
	"github.com/dsfaccini/fuse/fuseutil"
	"github.com/dsfaccini/fuse/internal/fusekernel"
)

// The kernel gets the first errno in an op error's chain, or EIO. The
// ErrorLogger gets one line per op error, except the expected errors that
// MountConfig.ErrorLogger lists.
func TestOpErrorReplyAndLog(t *testing.T) {
	wrap := func(errno syscall.Errno) error { return fmt.Errorf("op: %w", errno) }
	// A nil *os.PathError panics when errors.As or errors.Is unwraps it.
	var nilPathErr error = (*os.PathError)(nil)

	lookup := request(fusekernel.OpLookup, parent, names("file"))
	var getxattrIn fusekernel.GetxattrIn
	getxattrIn.Size = 64
	getxattr := request(fusekernel.OpGetxattr, child, join(fuse.NativeBytes(&getxattrIn), names("user.a")))
	listxattr := request(fusekernel.OpListxattr, child, fuse.NativeBytes(&fusekernel.ListxattrIn{Size: 64}))
	var setxattrIn fusekernel.SetxattrIn
	setxattrIn.Size = 5
	setxattr := request(fusekernel.OpSetxattr, child, join(fuse.NativeBytes(&setxattrIn), names("user.a"), []byte("value")))
	removexattr := request(fusekernel.OpRemovexattr, child, names("user.a"))
	link := request(fusekernel.OpLink, parent, join(fuse.NativeBytes(&fusekernel.LinkIn{Oldnodeid: uint64(child)}), names("hard")))
	read := request(fusekernel.OpRead, child, fuse.NativeBytes(&fusekernel.ReadIn{Fh: uint64(handle), Size: 4096}))
	getattr := request(fusekernel.OpGetattr, child, fuse.NativeBytes(&fusekernel.GetattrIn{}))
	rmdir := request(fusekernel.OpRmdir, parent, names("dir"))
	rename := request(fusekernel.OpRename, parent, join(fuse.NativeBytes(&fusekernel.RenameIn{Newdir: 5}), names("old", "new")))
	unlink := request(fusekernel.OpUnlink, parent, names("file"))

	const (
		lookupLine      = "Op 0x0000002a *fuseops.LookUpInodeOp] -> Error: "
		setxattrLine    = "Op 0x0000002a *fuseops.SetXattrOp] -> Error: "
		removexattrLine = "Op 0x0000002a *fuseops.RemoveXattrOp] -> Error: "
		linkLine        = "Op 0x0000002a *fuseops.CreateLinkOp] -> Error: "
		readLine        = "Op 0x0000002a *fuseops.ReadFileOp] -> Error: "
		getattrLine     = "Op 0x0000002a *fuseops.GetInodeAttributesOp] -> Error: "
		rmdirLine       = "Op 0x0000002a *fuseops.RmDirOp] -> Error: "
		renameLine      = "Op 0x0000002a *fuseops.RenameOp] -> Error: "
		unlinkLine      = "Op 0x0000002a *fuseops.UnlinkOp] -> Error: "
	)
	tests := []struct {
		name    string
		request [][]byte
		opErr   error
		errno   syscall.Errno
		// logged is the ErrorLogger's line, or empty for none.
		logged string
	}{
		{"lookup ENOENT", lookup, syscall.ENOENT, syscall.ENOENT, ""},
		{"lookup wrapped ENOENT", lookup, wrap(syscall.ENOENT), syscall.ENOENT, ""},
		{"lookup path error", lookup, &os.PathError{Op: "open", Path: "f", Err: syscall.EACCES}, syscall.EACCES,
			lookupLine + `"open f: permission denied"`},
		{"lookup EIO", lookup, wrap(syscall.EIO), syscall.EIO, lookupLine + `"op: input/output error"`},
		{"lookup no errno", lookup, errors.New("lookup failed"), syscall.EIO, lookupLine + `"lookup failed"`},
		{"lookup EIO before ENOENT", lookup, fmt.Errorf("lookup: %w (cleanup: %w)", syscall.EIO, syscall.ENOENT), syscall.EIO,
			lookupLine + `"lookup: input/output error (cleanup: no such file or directory)"`},
		{"lookup joined EACCES and ENOENT", lookup, errors.Join(syscall.EACCES, syscall.ENOENT), syscall.EACCES,
			lookupLine + `"permission denied\nno such file or directory"`},
		{"lookup nil path error", lookup, nilPathErr, syscall.EIO, lookupLine + "<nil>"},
		{"lookup wrapped nil path error", lookup, fmt.Errorf("lookup: %w", nilPathErr), syscall.EIO, lookupLine + `"lookup: <nil>"`},
		{"getxattr ENODATA", getxattr, wrap(syscall.ENODATA), syscall.ENODATA, ""},
		{"getxattr ENOSYS", getxattr, wrap(syscall.ENOSYS), syscall.ENOSYS, ""},
		{"listxattr ERANGE", listxattr, wrap(syscall.ERANGE), syscall.ERANGE, ""},
		{"setxattr ENOSYS", setxattr, wrap(syscall.ENOSYS), syscall.ENOSYS, ""},
		{"setxattr EPERM", setxattr, wrap(syscall.EPERM), syscall.EPERM, setxattrLine + `"op: operation not permitted"`},
		{"removexattr ENOSYS", removexattr, wrap(syscall.ENOSYS), syscall.ENOSYS, ""},
		{"removexattr EIO", removexattr, wrap(syscall.EIO), syscall.EIO, removexattrLine + `"op: input/output error"`},
		{"link ENOSYS", link, wrap(syscall.ENOSYS), syscall.ENOSYS, ""},
		{"link EPERM", link, wrap(syscall.EPERM), syscall.EPERM, linkLine + `"op: operation not permitted"`},
		{"read EINTR", read, wrap(syscall.EINTR), syscall.EINTR, ""},
		{"read EIO", read, wrap(syscall.EIO), syscall.EIO, readLine + `"op: input/output error"`},
		{"getattr EINTR", getattr, wrap(syscall.EINTR), syscall.EINTR, ""},
		// ESTALE's text differs between Linux and darwin.
		{"getattr ESTALE", getattr, wrap(syscall.ESTALE), syscall.ESTALE, getattrLine + strconv.Quote("op: "+syscall.ESTALE.Error())},
		{"rmdir ENOTEMPTY", rmdir, wrap(syscall.ENOTEMPTY), syscall.ENOTEMPTY, ""},
		{"rmdir EPERM", rmdir, wrap(syscall.EPERM), syscall.EPERM, rmdirLine + `"op: operation not permitted"`},
		{"rename ENOTEMPTY", rename, wrap(syscall.ENOTEMPTY), syscall.ENOTEMPTY, ""},
		{"rename EIO", rename, wrap(syscall.EIO), syscall.EIO, renameLine + `"op: input/output error"`},
		{"unlink ENOTEMPTY", unlink, wrap(syscall.ENOTEMPTY), syscall.ENOTEMPTY, unlinkLine + `"op: directory not empty"`},
		{"unlink EIO before ENOSYS", unlink, fmt.Errorf("unlink: %w (cleanup: %w)", syscall.EIO, syscall.ENOSYS), syscall.EIO,
			unlinkLine + `"unlink: input/output error (cleanup: function not implemented)"`},
		// The library decodes no FUSE_LSEEK, and the server answers an op it
		// cannot decode with ENOSYS.
		{"undecoded opcode", request(fusekernel.OpLseek, child), nil, syscall.ENOSYS, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := make(logLines, 10)
			fs := &protocolFS{serve: func(any) error { return tt.opErr }}
			k := fuse.ServeFakeKernelWithLoggers(t, fuseutil.NewFileSystemServer(fs), log.New(lines, "", 0), nil)
			k.Send(tt.request...)
			checkReply(t, k.NextReply(t), tt.errno, nil)
			// Reply logs before it writes the reply.
			if tt.logged != "" {
				checkErrorLogged(t, lines.next(t), tt.logged+"\n")
			}
			checkNoMoreLogs(t, lines)
		})
	}
}
