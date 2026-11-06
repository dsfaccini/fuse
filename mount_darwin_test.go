package fuse

import (
	"errors"
	"path/filepath"
	"testing"
)

// Every FuseImpl mounts through macFUSE. The mount helper is missing, so
// nothing mounts.
func TestMountUsesMacFUSEForEveryFuseImpl(t *testing.T) {
	oldHelper := macfuseMountHelper
	macfuseMountHelper = filepath.Join(t.TempDir(), "mount_macfuse")
	t.Cleanup(func() { macfuseMountHelper = oldHelper })
	setFSKitTestSystem(t, filepath.Join(t.TempDir(), "MFMount"), "26.0")

	for _, impl := range []FUSEImpl{FUSEImplFuseT, FUSEImplMacFUSE} {
		if _, err := Mount(t.TempDir(), nil, &MountConfig{FuseImpl: impl}); !errors.Is(err, errMacFUSENotFound) {
			t.Errorf("Mount with FuseImpl %d = %v, want %v", impl, err, errMacFUSENotFound)
		}
	}
}

// Backend wins over Options["backend"]. The mount helper and MFMount are
// missing, so nothing mounts, whichever backend Mount picks.
func TestMountBackendWinsOverTheDeprecatedOption(t *testing.T) {
	oldHelper := macfuseMountHelper
	macfuseMountHelper = filepath.Join(t.TempDir(), "mount_macfuse")
	t.Cleanup(func() { macfuseMountHelper = oldHelper })
	setFSKitTestSystem(t, filepath.Join(t.TempDir(), "MFMount"), "26.0")

	config := &MountConfig{Backend: BackendMacFUSEKernelExtension, Options: map[string]string{"backend": "fskit"}}
	if _, err := Mount(t.TempDir(), nil, config); !errors.Is(err, errMacFUSENotFound) {
		t.Errorf("Mount = %v, want %v from the kernel extension's mount helper", err, errMacFUSENotFound)
	}
}

func TestMacFUSEKernelExtensionLoadedReadsItsSysctl(t *testing.T) {
	old := macfuseKernelExtensionSysctl
	t.Cleanup(func() { macfuseKernelExtensionSysctl = old })

	macfuseKernelExtensionSysctl = "kern.osproductversion"
	if !MacFUSEKernelExtensionLoaded() {
		t.Error("MacFUSEKernelExtensionLoaded = false with its sysctl present")
	}
	macfuseKernelExtensionSysctl = "vfs.generic.macfuse.no.such.sysctl"
	if MacFUSEKernelExtensionLoaded() {
		t.Error("MacFUSEKernelExtensionLoaded = true with its sysctl missing")
	}
}
