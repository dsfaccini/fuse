package fuse

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
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

// testBackend is the backend that tests of backend-independent unmounting use.
const testBackend = BackendLinux

// testForceUnmountFlag is the flag a forced unmount passes to umount2(2).
const testForceUnmountFlag = unix.MNT_DETACH

func TestUnmountFallsBackToFusermountWithoutPrivilege(t *testing.T) {
	for _, test := range []struct {
		name     string
		served   bool
		wantArgs string
	}{
		{name: "serving", wantArgs: "-u\n--\n/mnt/x\n"},
		{name: "after Join", served: true, wantArgs: "-u\n-z\n--\n/mnt/x\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			recordUnmounts(t, func(int) error { return unix.EPERM })
			argsFile := fakeFusermount(t, "")
			mfs := &MountedFileSystem{dir: "/mnt/x", backend: BackendLinux, joinStatusAvailable: make(chan struct{})}
			if test.served {
				close(mfs.joinStatusAvailable)
			}
			if err := mfs.Unmount(context.Background()); err != nil {
				t.Fatalf("Unmount: %v", err)
			}
			got, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != test.wantArgs {
				t.Errorf("fusermount args = %q, want %q", got, test.wantArgs)
			}
		})
	}
}

func TestFuserunmountReportsABusyMount(t *testing.T) {
	for _, message := range []string{"Device or resource busy", "Resource busy"} {
		fakeFusermount(t, "echo 'fusermount3: failed to unmount /mnt/x: "+message+"' >&2; exit 1")
		if err := fuserunmount("/mnt/x"); !errors.Is(err, syscall.EBUSY) {
			t.Errorf("fuserunmount error = %v, want one that wraps EBUSY for %q", err, message)
		}
	}
	fakeFusermount(t, "echo 'fusermount3: entry for /mnt/x not found in /etc/mtab' >&2; exit 1")
	if err := fuserunmount("/mnt/x"); err == nil || errors.Is(err, syscall.EBUSY) {
		t.Errorf("fuserunmount error = %v, want one that does not wrap EBUSY", err)
	}
}

// fakeFusermount puts a fusermount3 on PATH that records its arguments, one
// per line, in the returned file, then runs body.
func fakeFusermount(t *testing.T, body string) string {
	t.Helper()
	bin := t.TempDir()
	argsFile := filepath.Join(t.TempDir(), "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsFile + "'\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(bin, "fusermount3"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	return argsFile
}
