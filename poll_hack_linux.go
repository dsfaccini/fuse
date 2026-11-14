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
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

// testHookPollHackFileOpen, when set, runs while runPollHack holds its file
// open.
var testHookPollHackFileOpen func()

// runPollHack has the kernel send the first FUSE_POLL on c's mount at dir, from
// syscalls that release their P, so that c answers it. See poll_hack.go.
// Ported from go-fuse
// (https://github.com/hanwen/go-fuse/blob/d54f65d20662f285443aed4b7020056f842f8bb0/fuse/poll_unix.go).
func runPollHack(dir string, c *Connection) error {
	// A /dev/fd/N mount point does not name the mount.
	if _, err := parseFuseFd(dir); err == nil {
		return nil
	}

	c.pollHackRunning.Store(true)
	defer c.pollHackRunning.Store(false)

	// No fork may start while the file is open. Since Linux 7.3, and Ubuntu's
	// 7.0.0-41.41, a vfork child's exec closes its copy of each close-on-exec
	// fd before it releases the parent ("exec: do_close_on_exec() before
	// taking exec_update_lock"). Closing a copy of this fd sends FLUSH, which
	// only c answers, while the forking thread holds its P with signals
	// blocked. Under GOMAXPROCS 1, a stop-the-world in the fork window, or
	// every P forking, nothing answers, and the process hangs. Every Go fork
	// takes syscall.ForkLock for writing, so holding it for reading keeps them
	// out. Answering the open forks nothing: c answers the entry's requests
	// in ReadOp, and the server answers only a GETATTR of the root.
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()

	fd, err := unix.Open(filepath.Join(dir, pollHackName), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if testHookPollHackFileOpen != nil {
		testHookPollHackFileOpen()
	}

	// The result does not matter: c answers ENOSYS, and the kernel stops
	// sending FUSE_POLL.
	unix.Poll([]unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN | unix.POLLPRI | unix.POLLOUT}}, 0)
	return nil
}
