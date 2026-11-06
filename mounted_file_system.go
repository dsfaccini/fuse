// Copyright 2015 Google Inc. All Rights Reserved.
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
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"
)

// MountedFileSystem represents the status of a mount operation, with a method
// that waits for unmounting.
type MountedFileSystem struct {
	dir     string
	backend Backend

	// The result to return from Join. Not valid until the channel is closed.
	joinStatus          error
	joinStatusAvailable chan struct{}
}

// Dir returns the directory on which the file system is mounted (or where we
// attempted to mount it.)
func (mfs *MountedFileSystem) Dir() string {
	return mfs.dir
}

// Backend returns the mechanism through which the kernel reaches the file
// system.
func (mfs *MountedFileSystem) Backend() Backend {
	return mfs.backend
}

// Unmount waits unmountRetryInterval before its first retry, and doubles the
// wait before each later one, up to unmountRetryMaxInterval.
const (
	unmountRetryInterval    = 100 * time.Millisecond
	unmountRetryMaxInterval = 2 * time.Second
)

// unmountRetryAfter starts Unmount's wait before a retry. Tests replace it.
var unmountRetryAfter = time.After

// Unmount unmounts the file system through its backend:
//
//   - Linux: umount2(2), then fusermount -u when umount2(2) fails with EPERM,
//     as it does without CAP_SYS_ADMIN;
//   - the macFUSE kernel extension: unmount(2);
//   - FSKit: unmount(2), then diskutil unmount when unmount(2) fails with
//     EPERM, as it does for a process without access to network volumes.
//
// While the file system is busy, and after any diskutil failure, Unmount
// retries with a growing wait until ctx ends, and then returns an error that wraps ctx.Err() and
// the last unmount error. Once Join has returned, the server is gone, so
// Unmount forces the unmount: a lazy unmount on Linux, MNT_FORCE on darwin,
// and diskutil unmount force on FSKit. That is the way out of the state an
// error fatal to the connection leaves.
//
// When the file system is no longer mounted, Unmount fails at once with the
// backend's error: an *os.PathError wrapping EINVAL from unmount(2) or
// umount2(2), or, without CAP_SYS_ADMIN on Linux, fusermount's error that the
// mount point is not mounted.
//
// Unmount does not wait for Join. A /dev/fd/N mount point fails with
// ErrExternallyManagedMountPoint, because its owner unmounts it.
func (mfs *MountedFileSystem) Unmount(ctx context.Context) error {
	wait := unmountRetryInterval
	for {
		var served bool
		select {
		case <-mfs.joinStatusAvailable:
			served = true
		default:
		}
		err := unmountBackend(mfs.dir, mfs.backend, served)
		if !errors.Is(err, syscall.EBUSY) && !errors.Is(err, errDiskutilUnmount) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("unmount %s: %w; last attempt: %w", mfs.dir, ctx.Err(), err)
		case <-unmountRetryAfter(wait):
		}
		wait = min(2*wait, unmountRetryMaxInterval)
	}
}

// Join blocks until a mounted file system has been unmounted. It does not
// return successfully until all ops read from the connection have been
// responded to (i.e. the file system server has finished processing all
// in-flight ops).
//
// The return value will be non-nil if anything unexpected happened while
// serving, including the error that was fatal to the connection. May be
// called multiple times.
//
// After an error that is fatal to the connection, Join returns while the file
// system may still be mounted, with every request to it failing. Call
// Unmount before you mount at the same directory again.
func (mfs *MountedFileSystem) Join(ctx context.Context) error {
	select {
	case <-mfs.joinStatusAvailable:
		return mfs.joinStatus
	case <-ctx.Done():
		return ctx.Err()
	}
}

// GetFuseContext implements the equiv. of FUSE-C fuse_get_context() and thus
// returns the UID / GID / PID associated with all FUSE requests send by the kernel.
// ctx parameter must be one of the context from the fuseops handlers (e.g.: CreateFile)
func (mfs *MountedFileSystem) GetFuseContext(ctx context.Context) (uid, gid, pid uint32, err error) {
	foo := ctx.Value(contextKey)
	state, ok := foo.(opState)
	if !ok {
		return 0, 0, 0, fmt.Errorf("GetFuseContext called with invalid context: %#v", ctx)
	}
	inMsg := state.inMsg
	header := inMsg.Header()
	return header.Uid, header.Gid, header.Pid, nil
}
