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
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// checkInheritedSockets makes the test binary act as a mount helper that
// passes back /dev/null, and exits with status 1 when it inherited a socket
// other than fd 3, its end of the socket pair. The variable's value lists the
// sockets the parent held before it ran the helper, which the helper ignores.
const checkInheritedSockets = "FUSE_TEST_CHECK_INHERITED_SOCKETS"

// openSockets returns the open file descriptors above 3 that are sockets.
func openSockets() ([]string, error) {
	fds, err := os.ReadDir("/dev/fd")
	if err != nil {
		return nil, err
	}
	var sockets []string
	for _, entry := range fds {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil || fd <= 3 {
			continue
		}
		var st unix.Stat_t
		if unix.Fstat(fd, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFSOCK {
			sockets = append(sockets, entry.Name())
		}
	}
	return sockets, nil
}

func TestFakeMountHelperInheritsOneSocket(t *testing.T) {
	held, ok := os.LookupEnv(checkInheritedSockets)
	if !ok {
		t.Skip("runs only as the fake mount helper")
	}
	sockets, err := openSockets()
	if err != nil {
		os.Exit(100)
	}
	for _, fd := range sockets {
		if !slices.Contains(strings.Split(held, ","), fd) {
			os.Exit(1)
		}
	}
	dev, err := os.Open(os.DevNull)
	if err != nil {
		os.Exit(101)
	}
	if err := unix.Sendmsg(3, []byte{0}, unix.UnixRights(int(dev.Fd())), nil, 0); err != nil {
		os.Exit(102)
	}
	os.Exit(0)
}

// fusermount runs the Linux fusermount fallback and the macFUSE kernel
// extension's mount helper. A helper that inherits the parent's end of the
// socket pair, or a second copy of its own, keeps the parent's read of the
// pair from ending when the helper exits. The test process can hold sockets
// it inherited itself, as on CI's macOS runners, and the helper inherits those
// too, so it ignores them.
func TestMountHelperSocketsCloseOnExec(t *testing.T) {
	held, err := openSockets()
	if err != nil {
		t.Fatal(err)
	}
	dev, _, err := fusermount(os.Args[0], []string{"-test.run=^TestFakeMountHelperInheritsOneSocket$"},
		[]string{checkInheritedSockets + "=" + strings.Join(held, ",")}, true, nil)
	if err != nil {
		t.Fatalf("fusermount with a helper that fails on an inherited socket: %v", err)
	}
	dev.Close()
}
