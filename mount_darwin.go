package fuse

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/dsfaccini/fuse/internal/buffer"
)

// errMacFUSENotFound is returned from Mount when macFUSE's mount helper is
// missing. Make sure macFUSE is installed.
var errMacFUSENotFound = errors.New("cannot locate macFUSE")

// macfuseMountHelper mounts through the macFUSE kernel extension and passes
// back the device, like fusermount. Tests point it elsewhere.
var macfuseMountHelper = "/Library/Filesystems/macfuse.fs/Contents/Resources/mount_macfuse"

func convertMountArgs(cfg *MountConfig) ([]string, []string) {
	env := []string{
		"_FUSE_CALL_BY_LIB=",
		"_FUSE_DAEMON_PATH=" + os.Args[0],
		"_FUSE_COMMVERS=2",
	}
	argv := []string{
		"-o", cfg.toOptionsString(),
		// Tell osxfuse-kext how large our buffer is. It must split
		// writes larger than this into multiple writes.
		//
		// OSXFUSE seems to ignore InitResponse.MaxWrite, and uses
		// this instead.
		"-o", "iosize=" + strconv.FormatUint(buffer.MaxWriteSize, 10),
	}

	return argv, env
}

// Mount at the given directory through the macFUSE kernel extension,
// returning its device.
func mount(
	dir string,
	cfg *MountConfig,
	ready chan<- error) (*os.File, error) {
	if _, err := os.Stat(macfuseMountHelper); os.IsNotExist(err) {
		return nil, errMacFUSENotFound
	}

	ready <- nil
	argv, env := convertMountArgs(cfg)
	argv = append(argv, dir)
	dev, err := fusermount(macfuseMountHelper, argv, env, false, cfg.DebugLogger)
	if err != nil {
		return nil, fmt.Errorf("callMount: %v", err)
	}
	return dev, nil
}

func mountTransport(
	dir string,
	cfg *MountConfig,
	ready chan<- error) (connectionTransport, error) {
	if cfg.Options["backend"] == "fskit" {
		return mountFSKitTransport(dir, cfg, ready)
	}

	dev, err := mount(dir, cfg, ready)
	if err != nil {
		return nil, err
	}
	return newFileTransport(dev), nil
}
