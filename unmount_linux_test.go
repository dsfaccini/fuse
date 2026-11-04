package fuse

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func Test_umountExpectCustomError(t *testing.T) {
	t.Setenv("PATH", "") // Clear PATH to fail unmount with fusermount is not found

	err := unmount("/dev/fd/42")
	if err == nil || !errors.Is(err, ErrExternallyManagedMountPoint) {
		t.Errorf("Expected: %v, but got: %v", ErrExternallyManagedMountPoint, err)
	}
}

func Test_umountNoCustomError(t *testing.T) {
	t.Setenv("PATH", "") // Clear PATH to fail unmount with fusermount is not found

	err := unmount("/dev")
	if err == nil {
		t.Fatal("Expected error but got none.")
	}
	if errors.Is(err, ErrExternallyManagedMountPoint) {
		t.Error("Custom error was not expected.")
	}
}

func TestFuserunmountEndsOptionsBeforeDir(t *testing.T) {
	bin := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsFile + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "fusermount3"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	const dir = "-mount-point"
	if err := fuserunmount(dir); err != nil {
		t.Fatalf("fuserunmount: %v", err)
	}
	got, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if want := "-u\n--\n" + dir + "\n"; string(got) != want {
		t.Errorf("fusermount args = %q, want %q", got, want)
	}
}
