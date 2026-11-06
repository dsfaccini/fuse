package fuse

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/dsfaccini/fuse/internal/buffer"
	"golang.org/x/sys/unix"
)

// errMacFUSENotFound is returned from Mount when macFUSE's mount helper is
// missing. Make sure macFUSE is installed.
var errMacFUSENotFound = errors.New("cannot locate macFUSE")

// macfuseKernelExtensionSysctl is the sysctl the macFUSE kernel extension
// registers while it is loaded.
var macfuseKernelExtensionSysctl = "vfs.generic.macfuse.version.number"

// MacFUSEKernelExtensionLoaded reports whether the macFUSE kernel extension is
// loaded. It loads nothing. A mount through BackendMacFUSEKernelExtension
// while the extension is not loaded makes macOS load it, which shows the user
// a system prompt.
func MacFUSEKernelExtensionLoaded() bool {
	_, err := unix.SysctlRaw(macfuseKernelExtensionSysctl)
	return err == nil
}

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
// returning its device and a function that waits for the mount helper and
// returns its result. The helper finishes mounting after it hands over the
// device.
func mount(dir string, cfg *MountConfig) (*os.File, func() error, error) {
	if _, err := os.Stat(macfuseMountHelper); os.IsNotExist(err) {
		return nil, nil, errMacFUSENotFound
	}

	argv, env := convertMountArgs(cfg)
	argv = append(argv, dir)
	dev, wait, err := fusermount(macfuseMountHelper, argv, env, false, cfg.DebugLogger)
	if err != nil {
		return nil, nil, fmt.Errorf("callMount: %v", err)
	}
	return dev, wait, nil
}

func mountTransport(dir string, cfg *MountConfig) (nativeMount, error) {
	if cfg.Backend == BackendFSKit {
		return mountFSKitTransport(dir, cfg)
	}

	dev, wait, err := mount(dir, cfg)
	if err != nil {
		return nil, err
	}
	return newDeviceMount(dev, BackendMacFUSEKernelExtension, wait), nil
}
