package fuse

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func unmount(dir string) error {
	if err := fuserunmount(dir); err != nil {
		// Return custom error for fusermount unmount error for /dev/fd/N mountpoints
		if strings.HasPrefix(dir, "/dev/fd/") {
			return fmt.Errorf("%w: %s", ErrExternallyManagedMountPoint, err)
		}
		return err
	}
	return nil
}

// unmountSyscall is umount2(2). Tests replace it to unmount nothing.
var unmountSyscall = unix.Unmount

// unmountBackend unmounts dir with umount2(2), lazily when force is set, so
// that a busy mount detaches at once. Without the privilege to, as for a
// mount fusermount made, it runs fusermount -u, with -z when force is set.
func unmountBackend(dir string, _ Backend, force bool) error {
	if strings.HasPrefix(dir, "/dev/fd/") {
		return ErrExternallyManagedMountPoint
	}
	flags := 0
	var fusermountFlags []string
	if force {
		flags = unix.MNT_DETACH
		fusermountFlags = []string{"-z"}
	}
	err := unmountSyscall(dir, flags)
	if errors.Is(err, unix.EPERM) {
		return fuserunmount(dir, fusermountFlags...)
	}
	if err != nil {
		return &os.PathError{Op: "unmount", Path: dir, Err: err}
	}
	return nil
}

// fuserunmount runs fusermount -u with flags on dir. Its error wraps
// syscall.EBUSY when fusermount reports the mount busy, as strerror words it
// in glibc ("Device or resource busy") and musl ("Resource busy").
func fuserunmount(dir string, flags ...string) error {
	fusermount, err := findFusermount()
	if err != nil {
		return err
	}
	args := append(append([]string{"-u"}, flags...), "--", dir)
	cmd := exec.Command(fusermount, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		if len(output) > 0 {
			output = bytes.TrimRight(output, "\n")
			if bytes.Contains(bytes.ToLower(output), []byte("resource busy")) {
				return fmt.Errorf("%v: %s: %w", err, output, syscall.EBUSY)
			}
			return fmt.Errorf("%v: %s", err, output)
		}

		return err
	}
	return nil
}
