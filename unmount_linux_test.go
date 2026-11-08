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

// On Linux, Unmount(dir) unmounts through fusermount -u alone.
func TestUnmountByPathRunsFusermount(t *testing.T) {
	for _, test := range []struct {
		name string
		dir  string
		// body runs in the fake fusermount3.
		body string
		// want is an error that Unmount's error wraps. nil means success.
		want error
	}{
		{name: "unmounts", dir: "/mnt/x"},
		{
			name: "busy",
			dir:  "/mnt/x",
			body: "echo 'fusermount3: failed to unmount /mnt/x: Device or resource busy' >&2; exit 1",
			want: syscall.EBUSY,
		},
		{
			name: "/dev/fd/N",
			dir:  "/dev/fd/42",
			body: "echo 'fusermount3: entry for /dev/fd/42 not found in /etc/mtab' >&2; exit 1",
			want: ErrExternallyManagedMountPoint,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			argsFile := fakeFusermount(t, test.body)
			err := Unmount(test.dir)
			if test.want == nil && err != nil {
				t.Errorf("Unmount: %v", err)
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Errorf("Unmount error = %v, want one that wraps %v", err, test.want)
			}
			if test.want != ErrExternallyManagedMountPoint && errors.Is(err, ErrExternallyManagedMountPoint) {
				t.Errorf("Unmount error = %v, want one that does not wrap ErrExternallyManagedMountPoint", err)
			}
			got, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			if want := "-u\n--\n" + test.dir + "\n"; string(got) != want {
				t.Errorf("fusermount args = %q, want %q", got, want)
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
