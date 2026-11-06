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

// unmountBackend unmounts dir, lazily when force is set, so that a busy mount
// detaches at once.
func unmountBackend(dir string, _ Backend, force bool) error {
	if !force {
		return unmount(dir)
	}
	if strings.HasPrefix(dir, "/dev/fd/") {
		return ErrExternallyManagedMountPoint
	}
	// Mount mounts directly when it may, as root does.
	err := unix.Unmount(dir, unix.MNT_DETACH)
	if !errors.Is(err, unix.EPERM) {
		if err != nil {
			return &os.PathError{Op: "unmount", Path: dir, Err: err}
		}
		return nil
	}
	return fuserunmount(dir, "-z")
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
