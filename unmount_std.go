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
	backend := BackendMacFUSEKernelExtension
	if isFSKitVolume(dir) {
		backend = BackendFSKit
	}
	return unmountBackend(dir, backend, false)
}

// Tests replace these to unmount nothing and to fake the mount table.
var (
	unmountSyscall = syscall.Unmount
	diskutilPath   = "/usr/sbin/diskutil"
	getfsstat      = unix.Getfsstat
)

// mntExtFSKit is MNT_EXT_FSKIT from <sys/mount.h>, the bit of statfs's
// f_flags_ext that marks an FSKit volume. mount(8) shows it as "fskit".
const mntExtFSKit = 0x2

// isFSKitVolume reports whether the volume mounted at dir is an FSKit volume.
// It reads the mount table without refreshing it, because statfs(2) on dir
// calls into the volume, whose server may be gone.
func isFSKitVolume(dir string) bool {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	// The mount table holds the mount point's real path. Resolving dir itself
	// would look into the volume.
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return false
	}
	path := filepath.Join(parent, filepath.Base(abs))
	stats, err := mountTable()
	if err != nil {
		return false
	}
	for _, st := range stats {
		if unix.ByteSliceToString(st.Mntonname[:]) == path {
			return st.Flags_ext&mntExtFSKit != 0
		}
	}
	return false
}

// mountTable reads the mounted volumes without touching any of them.
func mountTable() ([]unix.Statfs_t, error) {
	n, err := getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	stats := make([]unix.Statfs_t, n+16)
	if n, err = getfsstat(stats, unix.MNT_NOWAIT); err != nil {
		return nil, err
	}
	return stats[:n], nil
}

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
