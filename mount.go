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
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// Server is an interface for any type that knows how to serve ops read from a
// connection.
type Server interface {
	// Read and serve ops from the supplied connection until ReadOp returns an
	// error, which is io.EOF on unmount. Do not return until all operations
	// have been responded to. Must not be called more than once.
	ServeOps(*Connection)
}

// Mount is MountContext with context.Background(), so it waits for the mount
// however long it takes.
func Mount(
	dir string,
	server Server,
	config *MountConfig) (*MountedFileSystem, error) {
	return MountContext(context.Background(), dir, server, config)
}

// MountContext attempts to mount a file system on the given directory, using
// the supplied Server to serve connection requests. It blocks until the file
// system is successfully mounted. It validates config before it mounts
// anything. When the mount fails after the native mount was made, MountContext
// force-unmounts it before it returns.
//
// ctx bounds the wait for the mount. When ctx ends first, MountContext
// returns an *AbandonedMountError that wraps ctx.Err() at once. The mount then
// ends in the background, and the Server may receive ops until it has: on
// FSKit, the connection stops reading, and a mount that macFUSE completes
// later is force-unmounted. On Linux and the macFUSE kernel extension, ctx
// does not interrupt the mount helper or the wait for the kernel's INIT; a
// mount that completes after ctx ended is force-unmounted. Call
// AbandonedMountError.Wait before you mount at dir again, because that
// force-unmount goes by path. ctx has no effect once MountContext has
// returned.
//
// On Linux, MountContext answers the mount's first FUSE_POLL before it
// returns, so the process can open files in its own mount through package os
// without deadlocking. It reserves inode ID math.MaxUint64 for that: requests
// for it never reach the server. There are two exceptions:
//
//   - A /dev/fd/N mount point skips the hack. The process can then deadlock on
//     its own mount, and inode ID math.MaxUint64 stays the file system's.
//   - When the hack fails, MountContext still returns the mount, and logs the
//     error to MountConfig.ErrorLogger, or nowhere when that is nil. Opening
//     the mount through package os can then deadlock.
//
// The hack checks permissions on the root. Unless
// MountConfig.DisableDefaultPermissions is set, that sends the server a
// GetInodeAttributes for the root, and MountContext succeeds only after the
// server answers it. The server must not make that answer wait on anything
// that happens after MountContext returns.
func MountContext(
	ctx context.Context,
	dir string,
	server Server,
	config *MountConfig) (*MountedFileSystem, error) {
	cfg, err := config.normalize()
	if err != nil {
		return nil, fmt.Errorf("mount config: %w", err)
	}

	// Sanity check: make sure the mount point exists and is a directory. This
	// saves us from some confusing errors later on OS X.
	if err := checkMountPoint(dir); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Begin the mounting process, which will continue in the background.
	if cfg.DebugLogger != nil {
		cfg.DebugLogger.Println("Beginning the mounting kickoff process")
	}
	native, err := mountTransport(dir, &cfg)
	if err != nil {
		return nil, fmt.Errorf("mount: %w", err)
	}
	if cfg.DebugLogger != nil {
		cfg.DebugLogger.Println("Completed the mounting kickoff process")
	}

	type result struct {
		mfs *MountedFileSystem
		err error
	}
	done := make(chan result, 1)
	go func() {
		mfs, err := finishMount(dir, server, &cfg, native)
		done <- result{mfs, err}
	}()
	select {
	case r := <-done:
		return r.mfs, r.err
	case <-ctx.Done():
	}

	native.cancel()
	abandoned := &AbandonedMountError{ctxErr: ctx.Err(), done: make(chan struct{})}
	go func() {
		defer close(abandoned.done)
		r := <-done
		var unmountErr *unmountFailure
		switch {
		case errors.As(r.err, &unmountErr):
			abandoned.cleanupErr = unmountErr.err
		case r.err == nil:
			// The caller gave up on this mount and cannot unmount it.
			if err := forceUnmount(dir, native.backend()); err != nil {
				abandoned.cleanupErr = fmt.Errorf("unmounting the abandoned mount: %w", err)
				return
			}
			r.mfs.Join(context.Background())
		}
	}()
	return nil, abandoned
}

// AbandonedMountError is the error MountContext returns when its context ends
// before the mount completes. The mount then ends in the background, and the
// Server may receive ops until it has. Call Wait before you mount at the same
// directory again: a mount that completes late is force-unmounted by its
// path.
type AbandonedMountError struct {
	ctxErr     error
	done       chan struct{}
	cleanupErr error
}

func (e *AbandonedMountError) Error() string {
	return "mount abandoned: " + e.ctxErr.Error()
}

// Unwrap returns the error of the context that ended, so errors.Is(err,
// context.Canceled) and errors.Is(err, context.DeadlineExceeded) hold.
func (e *AbandonedMountError) Unwrap() error { return e.ctxErr }

// Wait blocks until the abandoned mount has ended: it failed, or it completed
// and has been force-unmounted and its Server has stopped. It returns the
// error from unmounting it, if any, or ctx.Err() when ctx ends first. Once
// Wait returns an unmount error, the mount and its Server may remain.
func (e *AbandonedMountError) Wait(ctx context.Context) error {
	select {
	case <-e.done:
		return e.cleanupErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// unmountFailure wraps the error from force-unmounting a mount that Mount
// will not return.
type unmountFailure struct{ err error }

func (e *unmountFailure) Error() string { return e.err.Error() }
func (e *unmountFailure) Unwrap() error { return e.err }

// finishMount reads INIT from native, starts serving, and waits for the native
// mount's result.
func finishMount(
	dir string,
	server Server,
	cfg *MountConfig,
	native nativeMount) (*MountedFileSystem, error) {
	if cfg.DebugLogger != nil {
		cfg.DebugLogger.Println("Creating a connection object")
	}
	// Create a Connection object wrapping the transport.
	connection, err := newConnection(
		*cfg,
		cfg.DebugLogger,
		cfg.ErrorLogger,
		cfg.WireLogger,
		native)
	if err != nil {
		if mountErr := native.waitMounted(); mountErr != nil {
			return nil, fmt.Errorf("mount (background): %w", mountErr)
		}
		// The native mount exists, but nothing serves it.
		err = fmt.Errorf("newConnection: %v", err)
		if unmountErr := forceUnmount(dir, native.backend()); unmountErr != nil {
			err = errors.Join(err, &unmountFailure{fmt.Errorf("unmounting the unserved mount: %w", unmountErr)})
		}
		return nil, err
	}
	if cfg.DebugLogger != nil {
		cfg.DebugLogger.Println("Successfully created the connection")
	}

	mfs := &MountedFileSystem{
		dir:                 dir,
		backend:             native.backend(),
		joinStatusAvailable: make(chan struct{}),
	}

	// Serve the connection in the background. When done, set the join status.
	go mfs.serve(server, connection)

	if cfg.DebugLogger != nil {
		cfg.DebugLogger.Println("Waiting for mounting process to complete")
	}

	// Wait for the mount process to complete.
	if err := native.waitMounted(); err != nil {
		err = fmt.Errorf("mount (background): %w", err)
		if native.servedMountOutlivesFailure() {
			// INIT came through this mount, so it exists. Unmounting it hangs
			// up the connection.
			if unmountErr := forceUnmount(dir, native.backend()); unmountErr != nil {
				return nil, errors.Join(err, &unmountFailure{fmt.Errorf("unmounting the failed mount: %w", unmountErr)})
			}
		}
		// The server stops once the connection hangs up.
		mfs.Join(context.Background())
		return nil, err
	}

	// Without the hack, the mount works as it did, so a failure is only logged.
	if err := runPollHack(dir, connection); err != nil && cfg.ErrorLogger != nil {
		cfg.ErrorLogger.Printf("poll hack: %v; opening this mount through package os may deadlock", err)
	}

	return mfs, nil
}

// forceUnmount removes a mount that Mount made but will not return. A
// /dev/fd/N mount point belongs to whoever opened the device, so it stays.
func forceUnmount(dir string, backend Backend) error {
	if strings.HasPrefix(dir, "/dev/fd/") {
		return nil
	}
	return unmountBackend(dir, backend, true)
}

// nativeMount is the kernel-facing side of a mount that mountTransport
// started. It owns the native mount's result.
type nativeMount interface {
	connectionTransport

	// backend returns the mechanism the mount goes through.
	backend() Backend

	// waitMounted blocks until the native mount has finished, and returns its
	// result. It may be called more than once, and after Close. FSKit and the
	// kernel extension finish mounting only after INIT is answered, so Mount
	// calls it once the connection has read INIT, or has failed to.
	waitMounted() error

	// cancel makes a read waiting for the kernel, and every later read, return
	// io.EOF, where the backend can interrupt one. MountContext calls it when
	// its context ends.
	cancel()

	// servedMountOutlivesFailure reports whether a mount that answered INIT
	// still exists when the native mount then reports failure. Otherwise the
	// failure hangs up the connection.
	servedMountOutlivesFailure() bool
}

// serve runs server until ServeOps returns, then publishes the join status:
// the connection's fatal error, if any, joined with the error from closing it.
func (mfs *MountedFileSystem) serve(server Server, c *Connection) {
	server.ServeOps(c)
	mfs.joinStatus = errors.Join(c.fatalError(), c.close())
	close(mfs.joinStatusAvailable)
}

func checkMountPoint(dir string) error {
	if strings.HasPrefix(dir, "/dev/fd") {
		return nil
	}

	fi, err := os.Stat(dir)
	switch {
	case os.IsNotExist(err):
		return err

	case err != nil:
		return fmt.Errorf("Statting mount point: %v", err)

	case !fi.IsDir():
		return fmt.Errorf("Mount point %s is not a directory", dir)
	}

	return nil
}

// fusermount runs binary, a mount helper, and returns the device it passes
// back. With wait set, the helper has exited by then. Otherwise fusermount
// also returns a function that waits for the helper's mount result; see
// mountHelperResult.
func fusermount(binary string, argv []string, additionalEnv []string, wait bool, debugLogger *log.Logger) (_ *os.File, waitHelper func() error, err error) {
	if debugLogger != nil {
		debugLogger.Println("Creating a socket pair")
	}
	// Create a socket pair.
	fds, err := cloexecSocketpair()
	if err != nil {
		return nil, nil, fmt.Errorf("Socketpair: %v", err)
	}

	if debugLogger != nil {
		debugLogger.Println("Creating files to wrap the sockets")
	}
	// Wrap the sockets into os.File objects that we will pass off to fusermount.
	writeFile := os.NewFile(uintptr(fds[0]), "fusermount-child-writes")

	readFile := os.NewFile(uintptr(fds[1]), "fusermount-parent-reads")
	defer readFile.Close()

	if debugLogger != nil {
		debugLogger.Println("Starting fusermount/os mount")
	}
	// Start fusermount or mount_macfuse.
	cmd := exec.Command(binary, argv...)
	cmd.Env = append(os.Environ(), "_FUSE_COMMFD=3")
	cmd.Env = append(cmd.Env, additionalEnv...)
	cmd.ExtraFiles = []*os.File{writeFile}
	cmd.Stderr = os.Stderr

	// Run the command.
	if wait {
		err = cmd.Run()
	} else {
		err = cmd.Start()
	}
	// Only the helper may hold the other end, so that the reads below end
	// when it exits.
	writeFile.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("running %v: %v", binary, err)
	}
	if !wait {
		defer func() {
			if err != nil {
				// Reap the helper.
				go cmd.Wait()
			}
		}()
	}

	if debugLogger != nil {
		debugLogger.Println("Wrapping socket pair in a connection")
	}
	// Wrap the socket file in a connection.
	c, err := net.FileConn(readFile)
	if err != nil {
		return nil, nil, fmt.Errorf("FileConn: %v", err)
	}
	// Without wait, the helper sends its mount result on the socket later.
	keepConn := false
	defer func() {
		if !keepConn {
			c.Close()
		}
	}()

	if debugLogger != nil {
		debugLogger.Println("Checking that we have a unix domain socket")
	}
	// We expect to have a Unix domain socket.
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return nil, nil, fmt.Errorf("Expected UnixConn, got %T", c)
	}

	if debugLogger != nil {
		debugLogger.Println("Read a message from socket")
	}
	// Read a message.
	// Read only the byte that carries the descriptor, so that the mount
	// result the helper sends next stays on the socket.
	buf := make([]byte, 1)
	oob := make([]byte, 32) // expect 24 bytes
	_, oobn, _, _, err := uc.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, nil, fmt.Errorf("ReadMsgUnix: %v", err)
	}

	// Parse the message.
	scms, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, nil, fmt.Errorf("ParseSocketControlMessage: %v", err)
	}

	// We expect one message.
	if len(scms) != 1 {
		return nil, nil, fmt.Errorf("expected 1 SocketControlMessage; got scms = %#v", scms)
	}

	scm := scms[0]

	if debugLogger != nil {
		debugLogger.Println("Successfully read the socket message.")
	}

	// Pull out the FD returned by fusermount
	gotFds, err := syscall.ParseUnixRights(&scm)
	if err != nil {
		return nil, nil, fmt.Errorf("syscall.ParseUnixRights: %v", err)
	}

	if len(gotFds) != 1 {
		return nil, nil, fmt.Errorf("wanted 1 fd; got %#v", gotFds)
	}

	if debugLogger != nil {
		debugLogger.Println("Converting FD into os.File")
	}
	// Turn the FD into an os.File.
	if !wait {
		keepConn = true
		waitHelper = func() error {
			defer c.Close()
			return mountHelperResult(binary, uc, cmd)
		}
	}
	return os.NewFile(uintptr(gotFds[0]), "/dev/fuse"), waitHelper, nil
}

// mountHelperResult reads the mount result that a macFUSE mount helper sends
// on its socket once mount(2) returns, as macFUSE's libfuse does, and reaps
// the helper. With _FUSE_COMMVERS=2 the result is a native-endian int32: 0
// for success, an errno, or -1 for an unknown failure. A helper that sends no
// result reports through its exit status.
func mountHelperResult(helper string, conn io.Reader, cmd *exec.Cmd) error {
	var status int32
	readErr := binary.Read(conn, binary.NativeEndian, &status)
	waitErr := cmd.Wait()
	switch {
	case readErr == nil && status == 0:
		return nil
	case readErr == nil && status > 0:
		return fmt.Errorf("%s: mount(2): %w", helper, syscall.Errno(status))
	case readErr == nil:
		return fmt.Errorf("%s: mount failed with status %d", helper, status)
	case waitErr != nil:
		return fmt.Errorf("%s: %w", helper, waitErr)
	}
	return nil
}

// cloexecSocketpair returns a connected pair of Unix stream sockets that a
// child process inherits only when it is passed one explicitly. It holds
// syscall.ForkLock so that no fork in between inherits them.
func cloexecSocketpair() ([2]int, error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return fds, err
	}
	syscall.CloseOnExec(fds[0])
	syscall.CloseOnExec(fds[1])
	return fds, nil
}
