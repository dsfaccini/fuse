package fuse

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/dsfaccini/fuse/internal/fusekernel"
	"golang.org/x/sys/unix"
)

// testBackend is the backend that tests of backend-independent unmounting use.
const testBackend = BackendMacFUSEKernelExtension

// testForceUnmountFlag is the flag a forced unmount passes to unmount(2).
const testForceUnmountFlag = unix.MNT_FORCE

// waitUntil fails the test unless cond holds within 5s.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestMountReportsTheFSKitBackend(t *testing.T) {
	f := newFakeMFMount(t)
	mfs, err := MountWithFakeMFMount(context.Background(), t, f, drainServer{}, MountConfig{})
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	t.Cleanup(func() {
		f.kernel.HangUp()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		mfs.Join(ctx)
	})
	if got := mfs.Backend(); got != BackendFSKit {
		t.Errorf("Backend() = %q, want %q", got, BackendFSKit)
	}
}

func TestMountForceUnmountsAnUnservedFSKitMount(t *testing.T) {
	f := newFakeMFMount(t)
	f.mount = func() int32 {
		// INIT fails because the first request is not INIT, and MFMount then
		// succeeds.
		f.kernel.Send(FakeRequest(
			fusekernel.InHeader{Opcode: fusekernel.OpGetattr, Unique: 2, Nodeid: 1},
			NativeBytes(&fusekernel.GetattrIn{}))...)
		if _, ok := f.kernel.nextReply(); !ok {
			t.Error("the connection did not answer the request it read instead of INIT")
		}
		return 0
	}
	_, err := MountWithFakeMFMount(context.Background(), t, f, drainServer{}, MountConfig{})
	if err == nil || !strings.HasPrefix(err.Error(), "newConnection: ") {
		t.Fatalf("Mount error = %v, want a newConnection error", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.unmounts) != 1 || f.unmounts[0].flags != unix.MNT_FORCE {
		t.Errorf("unmount calls = %v, want one with MNT_FORCE", f.unmounts)
	}
	if f.channelCloses != 1 {
		t.Errorf("MFChannelClose calls = %d, want 1", f.channelCloses)
	}
}

func TestMountContextGivesUpOnAStalledFSKitMount(t *testing.T) {
	f := newFakeMFMount(t)
	finish := make(chan struct{})
	// MFMount stalls and never sends INIT.
	f.mount = func() int32 {
		<-finish
		return 0
	}
	// The read for INIT starts to wait only after the first interrupt, which
	// therefore misses it.
	var firstRead sync.Once
	f.beforeRead = func() {
		firstRead.Do(func() {
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
				f.mu.Lock()
				interrupted := f.channelInterrupts > 0
				f.mu.Unlock()
				if interrupted {
					return
				}
			}
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	returned := make(chan error, 1)
	go func() {
		_, err := MountWithFakeMFMount(ctx, t, f, drainServer{}, MountConfig{})
		returned <- err
	}()
	select {
	case err := <-returned:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("MountContext error = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(5 * time.Second):
		close(finish)
		t.Fatal("MountContext did not return within 5s of its deadline")
	}

	// The read waiting for INIT ends, and the transport releases its channel
	// reference while MFMount still holds its own.
	waitUntil(t, "the transport releases the channel", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.channelInterrupts > 0 && f.channelReleases == 1
	})

	// MFMount succeeds late, so the mount is closed and force-unmounted.
	close(finish)
	waitUntil(t, "the late mount is unmounted", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.unmounts) > 0
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.unmounts) != 1 || f.unmounts[0].flags != unix.MNT_FORCE {
		t.Errorf("unmount calls = %v, want one with MNT_FORCE", f.unmounts)
	}
	if f.channelCloses != 1 {
		t.Errorf("MFChannelClose calls = %d, want 1", f.channelCloses)
	}
}

func TestFSKitMountFailureEndsTheINITReadWithoutAChannelClose(t *testing.T) {
	f := newFakeMFMount(t)
	// MFMount fails before INIT and leaves the channel open.
	f.mount = func() int32 { return FSKitMountResultUnexpectedFailure }
	returned := make(chan error, 1)
	go func() {
		_, err := MountWithFakeMFMount(context.Background(), t, f, drainServer{}, MountConfig{})
		returned <- err
	}()
	select {
	case err := <-returned:
		var mountErr FSKitMountError
		if !errors.As(err, &mountErr) || mountErr.Result != FSKitMountResultUnexpectedFailure {
			t.Errorf("Mount error = %v, want FSKitMountError with result %d", err, FSKitMountResultUnexpectedFailure)
		}
	case <-time.After(5 * time.Second):
		// Unblock the read so that the fake's cleanup can finish.
		f.kernel.HangUp()
		t.Fatal("Mount did not return within 5s of MFMount's failure")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.channelCloses != 0 {
		t.Errorf("MFChannelClose calls = %d, want 0 after a failed mount", f.channelCloses)
	}
}

func TestFSKitUnmountFallsBackToDiskutil(t *testing.T) {
	for _, test := range []struct {
		name      string
		served    bool
		wantFlags int
		wantArgs  []string
	}{
		{name: "serving", wantArgs: []string{"unmount"}},
		{name: "after Join", served: true, wantFlags: unix.MNT_FORCE, wantArgs: []string{"unmount", "force"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			unmounts := recordUnmounts(t, func(int) error { return syscall.EPERM })
			argsFile := fakeDiskutil(t)
			// A relative mount point reaches diskutil as an absolute path.
			mfs := &MountedFileSystem{dir: "force", backend: BackendFSKit, joinStatusAvailable: make(chan struct{})}
			if test.served {
				close(mfs.joinStatusAvailable)
			}
			if err := mfs.Unmount(context.Background()); err != nil {
				t.Fatalf("Unmount: %v", err)
			}
			if got := unmounts(); len(got) != 1 || got[0].flags != test.wantFlags {
				t.Errorf("unmount calls = %v, want one with flags %d", got, test.wantFlags)
			}
			abs, err := filepath.Abs("force")
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatalf("diskutil did not run: %v", err)
			}
			if want := strings.Join(append(test.wantArgs, abs), "\n") + "\n"; string(got) != want {
				t.Errorf("diskutil args = %q, want %q", got, want)
			}
		})
	}
}

func TestKernelExtensionUnmountKeepsEPERM(t *testing.T) {
	recordUnmounts(t, func(int) error { return syscall.EPERM })
	argsFile := fakeDiskutil(t)
	mfs := &MountedFileSystem{dir: "/mnt/x", backend: BackendMacFUSEKernelExtension, joinStatusAvailable: make(chan struct{})}
	if err := mfs.Unmount(context.Background()); !errors.Is(err, syscall.EPERM) {
		t.Errorf("Unmount error = %v, want EPERM", err)
	}
	if _, err := os.Stat(argsFile); err == nil {
		t.Error("diskutil ran for a kernel extension mount")
	}
}

// fakeDiskutil points diskutilPath at a script that records its arguments,
// one per line, in the returned file.
func fakeDiskutil(t *testing.T) string {
	t.Helper()
	argsFile := filepath.Join(t.TempDir(), "args")
	script := filepath.Join(t.TempDir(), "diskutil")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+argsFile+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := diskutilPath
	diskutilPath = script
	t.Cleanup(func() { diskutilPath = old })
	return argsFile
}

// fakeMountHelper makes the test binary act as mount_macfuse. Its value is
// "<result>,<exit status>": the helper passes back /dev/null as the device,
// sends result as its mount result unless result is "-", and exits.
const fakeMountHelper = "FUSE_TEST_FAKE_MOUNT_HELPER"

func TestFakeMacFUSEMountHelper(t *testing.T) {
	result, exit, ok := strings.Cut(os.Getenv(fakeMountHelper), ",")
	if !ok {
		t.Skip("runs only as the fake mount helper")
	}
	dev, err := os.Open(os.DevNull)
	if err != nil {
		os.Exit(100)
	}
	if err := unix.Sendmsg(3, []byte{0}, unix.UnixRights(int(dev.Fd())), nil, 0); err != nil {
		os.Exit(101)
	}
	if result != "-" {
		status, err := strconv.Atoi(result)
		if err != nil {
			os.Exit(102)
		}
		value := int32(status)
		if _, err := unix.Write(3, NativeBytes(&value)); err != nil {
			os.Exit(103)
		}
	}
	code, err := strconv.Atoi(exit)
	if err != nil {
		os.Exit(104)
	}
	os.Exit(code)
}

func TestKernelExtensionMountReportsTheHelperResult(t *testing.T) {
	helper := filepath.Join(t.TempDir(), "mount_macfuse")
	script := "#!/bin/sh\nexec '" + os.Args[0] + "' -test.run='^TestFakeMacFUSEMountHelper$'\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := macfuseMountHelper
	macfuseMountHelper = helper
	t.Cleanup(func() { macfuseMountHelper = old })

	for _, test := range []struct {
		name   string
		helper string
		// check reports whether Mount's error is the helper's failure. Nil
		// means the helper mounted.
		check func(error) bool
	}{
		{
			name:   "mount(2) fails",
			helper: "13,0",
			check:  func(err error) bool { return errors.Is(err, syscall.EACCES) },
		},
		{
			name:   "unknown failure",
			helper: "-1,0",
			check:  func(err error) bool { return err != nil && strings.Contains(err.Error(), "status -1") },
		},
		{
			name:   "no result, exit status",
			helper: "-,3",
			check: func(err error) bool {
				var exitErr *exec.ExitError
				return errors.As(err, &exitErr) && exitErr.ExitCode() == 3
			},
		},
		// The result wins over the exit status.
		{name: "mounted", helper: "0,3"},
		{name: "mounted, no result", helper: "-,0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(fakeMountHelper, test.helper)
			unmounts := recordUnmounts(t, nil)
			dir := t.TempDir()
			// The device is /dev/null, so INIT fails.
			_, err := Mount(dir, drainServer{}, &MountConfig{})
			if test.check != nil {
				if !test.check(err) || !strings.HasPrefix(err.Error(), "mount (background): ") {
					t.Errorf("Mount error = %v, want the helper's failure", err)
				}
				if got := unmounts(); len(got) != 0 {
					t.Errorf("unmount calls = %v, want none after the helper failed", got)
				}
				return
			}
			if err == nil || !strings.HasPrefix(err.Error(), "newConnection: ") {
				t.Errorf("Mount error = %v, want a newConnection error", err)
			}
			want := fakeUnmount{dir, unix.MNT_FORCE}
			if got := unmounts(); len(got) != 1 || got[0] != want {
				t.Errorf("unmount calls = %v, want [%v]", got, want)
			}
		})
	}
}
