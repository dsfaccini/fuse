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

	for _, impl := range []FUSEImpl{FUSEImplFuseT, FUSEImplMacFUSE} {
		if _, err := Mount(t.TempDir(), nil, &MountConfig{FuseImpl: impl}); !errors.Is(err, errMacFUSENotFound) {
			t.Errorf("Mount with FuseImpl %d = %v, want %v", impl, err, errMacFUSENotFound)
		}
	}
}
