package fuse

import (
	"bytes"
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
	var err error
	select {
	case err = <-returned:
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
	var abandoned *AbandonedMountError
	if !errors.As(err, &abandoned) {
		t.Fatalf("MountContext error = %v, want an AbandonedMountError", err)
	}
	if err := abandoned.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
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

func TestMountContextUnmountsAMountThatCompletesAfterItGaveUp(t *testing.T) {
	f := newFakeMFMount(t)
	ctx, cancel := context.WithCancel(context.Background())
	finish := make(chan struct{})
	// INIT is answered, then the caller gives up before MFMount returns.
	f.mount = func() int32 {
		if !f.answerInit() {
			t.Error("no INIT reply")
		}
		cancel()
		<-finish
		return 0
	}
	_, err := MountWithFakeMFMount(ctx, t, f, drainServer{}, MountConfig{})
	var abandoned *AbandonedMountError
	if !errors.As(err, &abandoned) || !errors.Is(err, context.Canceled) {
		close(finish)
		t.Fatalf("MountContext error = %v, want an AbandonedMountError wrapping context.Canceled", err)
	}
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer waitCancel()
	if err := abandoned.Wait(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Wait before MFMount returned = %v, want context.DeadlineExceeded", err)
	}

	close(finish)
	if err := abandoned.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
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

func TestFSKitUnmountRetriesADiskutilFailure(t *testing.T) {
	recordUnmounts(t, func(int) error { return syscall.EPERM })
	argsFile := fakeDiskutil(t)
	// The script fails until it has run three times.
	runs := filepath.Join(t.TempDir(), "runs")
	script := "#!/bin/sh\necho run >> '" + runs + "'\n[ $(wc -l < '" + runs + "') -ge 3 ] || { echo 'Unmount failed: dissented' >&2; exit 1; }\nprintf '%s\\n' \"$@\" > '" + argsFile + "'\n"
	if err := os.WriteFile(diskutilPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	mfs := &MountedFileSystem{dir: "/mnt/x", backend: BackendFSKit, joinStatusAvailable: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := mfs.Unmount(ctx); err != nil {
		t.Fatalf("Unmount: %v", err)
	}
	if _, err := os.Stat(argsFile); err != nil {
		t.Errorf("diskutil never succeeded: %v", err)
	}

	// A diskutil that keeps failing is retried until ctx ends.
	if err := os.WriteFile(diskutilPath, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 3*unmountRetryInterval)
	defer cancel()
	if err := mfs.Unmount(ctx); !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, errDiskutilUnmount) {
		t.Errorf("Unmount error = %v, want one that wraps context.DeadlineExceeded and the diskutil failure", err)
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

func TestUnmountByPathFallsBackToDiskutilOnlyOnFSKit(t *testing.T) {
	// The mount table holds the mount point's real path, and Unmount gets one
	// through a symlink.
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(link, "mnt")
	for _, test := range []struct {
		name         string
		mounted      string
		flagsExt     uint32
		wantDiskutil bool
	}{
		{name: "FSKit", mounted: filepath.Join(real, "mnt"), flagsExt: mntExtFSKit, wantDiskutil: true},
		{name: "kernel extension", mounted: filepath.Join(real, "mnt")},
		{name: "FSKit at another mount point", mounted: filepath.Join(real, "other"), flagsExt: mntExtFSKit},
	} {
		t.Run(test.name, func(t *testing.T) {
			unmounts := recordUnmounts(t, func(int) error { return syscall.EPERM })
			argsFile := fakeDiskutil(t)
			var volume unix.Statfs_t
			copy(volume.Fstypename[:], "macfuse")
			copy(volume.Mntonname[:], test.mounted)
			volume.Flags_ext = test.flagsExt
			fakeMountTable(t, volume)
			err := Unmount(dir)
			if got := unmounts(); len(got) != 1 || got[0] != (fakeUnmount{dir, 0}) {
				t.Errorf("unmount calls = %v, want [{%s 0}]", got, dir)
			}
			args, readErr := os.ReadFile(argsFile)
			if !test.wantDiskutil {
				if !errors.Is(err, syscall.EPERM) {
					t.Errorf("Unmount error = %v, want EPERM", err)
				}
				if readErr == nil {
					t.Errorf("diskutil ran with args %q", args)
				}
				return
			}
			if err != nil {
				t.Fatalf("Unmount: %v", err)
			}
			if want := "unmount\n" + dir + "\n"; string(args) != want {
				t.Errorf("diskutil args = %q (read error: %v), want %q", args, readErr, want)
			}
		})
	}
}

// fakeMountTable makes the mount table hold only volumes until the test ends.
func fakeMountTable(t *testing.T, volumes ...unix.Statfs_t) {
	t.Helper()
	old := getfsstat
	getfsstat = func(buf []unix.Statfs_t, _ int) (int, error) {
		if buf == nil {
			return len(volumes), nil
		}
		return copy(buf, volumes), nil
	}
	t.Cleanup(func() { getfsstat = old })
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
// "<result>,<exit status>[,init|nodevice]": the helper passes back /dev/null
// as the device, with init a device that sends INIT and takes its reply, and
// with nodevice no device. It then sends result as its mount result unless
// result is "-", and exits.
const fakeMountHelper = "FUSE_TEST_FAKE_MOUNT_HELPER"

func TestFakeMacFUSEMountHelper(t *testing.T) {
	fields := strings.Split(os.Getenv(fakeMountHelper), ",")
	if len(fields) < 2 {
		t.Skip("runs only as the fake mount helper")
	}
	result, exit := fields[0], fields[1]
	switch {
	case len(fields) == 3 && fields[2] == "nodevice":
	case len(fields) == 3:
		pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
		if err != nil {
			os.Exit(105)
		}
		if err := unix.Sendmsg(3, []byte{0}, unix.UnixRights(pair[0]), nil, 0); err != nil {
			os.Exit(101)
		}
		if _, err := unix.Write(pair[1], bytes.Join(fakeInit(), nil)); err != nil {
			os.Exit(106)
		}
		if _, err := unix.Read(pair[1], make([]byte, 4096)); err != nil {
			os.Exit(107)
		}
	default:
		dev, err := os.Open(os.DevNull)
		if err != nil {
			os.Exit(100)
		}
		if err := unix.Sendmsg(3, []byte{0}, unix.UnixRights(int(dev.Fd())), nil, 0); err != nil {
			os.Exit(101)
		}
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

// useFakeMacFUSEMountHelper points macfuseMountHelper at the test binary
// running TestFakeMacFUSEMountHelper. It returns a function that returns the
// arguments of the helper's last run, one per line, or nil before its first.
func useFakeMacFUSEMountHelper(t *testing.T) func() []string {
	t.Helper()
	dir := t.TempDir()
	helper, argv := filepath.Join(dir, "mount_macfuse"), filepath.Join(dir, "argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argv + "'\nexec '" + os.Args[0] + "' -test.run='^TestFakeMacFUSEMountHelper$'\n"
	if err := os.WriteFile(helper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	old := macfuseMountHelper
	macfuseMountHelper = helper
	t.Cleanup(func() { macfuseMountHelper = old })
	return func() []string {
		data, err := os.ReadFile(argv)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	}
}

func TestKernelExtensionMountUnmountsNothingAfterTheHelperFailsPastINIT(t *testing.T) {
	useFakeMacFUSEMountHelper(t)
	t.Setenv(fakeMountHelper, "13,0,init")
	unmounts := recordUnmounts(t, nil)
	dir := t.TempDir()
	returned := make(chan error, 1)
	go func() {
		_, err := Mount(dir, drainServer{}, &MountConfig{})
		returned <- err
	}()
	select {
	case err := <-returned:
		if !errors.Is(err, syscall.EACCES) {
			t.Errorf("Mount error = %v, want the helper's EACCES", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Mount did not return within 10s of the helper's failure")
	}
	// mount(2) failed, so nothing of this mount is at dir, which may be
	// another volume.
	if got := unmounts(); len(got) != 0 {
		t.Errorf("unmount calls = %v, want none", got)
	}
}

func TestKernelExtensionMountReportsTheHelperResult(t *testing.T) {
	useFakeMacFUSEMountHelper(t)

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
		{
			name:   "no device",
			helper: "-,3,nodevice",
			check:  func(err error) bool { return err != nil && strings.Contains(err.Error(), "ReadMsgUnix") },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(fakeMountHelper, test.helper)
			unmounts := recordUnmounts(t, nil)
			dir := t.TempDir()
			// The device is /dev/null, so INIT fails.
			returned := make(chan error, 1)
			go func() {
				_, err := Mount(dir, drainServer{}, &MountConfig{})
				returned <- err
			}()
			var err error
			select {
			case err = <-returned:
			case <-time.After(10 * time.Second):
				t.Fatal("Mount did not return within 10s")
			}
			if test.check != nil {
				if !test.check(err) {
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
