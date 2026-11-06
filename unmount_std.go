//go:build !linux
// +build !linux

package fuse

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

func unmount(dir string) error {
	return unmountBackend(dir, BackendMacFUSEKernelExtension, false)
}

// Tests replace these to unmount nothing.
var (
	unmountSyscall = syscall.Unmount
	diskutilPath   = "/usr/sbin/diskutil"
)

// unmountBackend unmounts dir with unmount(2), with MNT_FORCE when force is
// set. On FSKit, an EPERM from unmount(2) falls back to diskutil unmount,
// which goes through DiskArbitration as macFUSE's libfuse does.
func unmountBackend(dir string, backend Backend, force bool) error {
	flags := 0
	if force {
		flags = unix.MNT_FORCE
	}
	err := unmountSyscall(dir, flags)
	if err == nil {
		return nil
	}
	if backend == BackendFSKit && errors.Is(err, syscall.EPERM) {
		return diskutilUnmount(dir, force)
	}
	return &os.PathError{Op: "unmount", Path: dir, Err: err}
}

func diskutilUnmount(dir string, force bool) error {
	// An absolute path cannot be read as diskutil's force keyword or a disk
	// identifier.
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	args := []string{"unmount"}
	if force {
		args = append(args, "force")
	}
	args = append(args, abs)
	output, err := exec.Command(diskutilPath, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s: %w: %s", errDiskutilUnmount, abs, err, bytes.TrimSpace(output))
	}
	return nil
}
