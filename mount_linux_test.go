package fuse

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/dsfaccini/fuse/fuseops"
	"github.com/dsfaccini/fuse/internal/mounttest"
)

func Test_parseFuseFd(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		fd, err := parseFuseFd("/dev/fd/42")
		if fd != 42 {
			t.Errorf("expected 42, got %d", fd)
		}
		if err != nil {
			t.Errorf("expected no error, got %#v", err)
		}
	})

	t.Run("negative", func(t *testing.T) {
		fd, err := parseFuseFd("/dev/fd/-42")
		if fd != -1 {
			t.Errorf("expected an invalid fd, got %d", fd)
		}
		if err == nil {
			t.Errorf("expected an error, nil")
		}
	})

	t.Run("not an int", func(t *testing.T) {
		fd, err := parseFuseFd("/dev/fd/3.14159")
		if fd != -1 {
			t.Errorf("expected an invalid fd, got %d", fd)
		}
		if err == nil {
			t.Errorf("expected an error, nil")
		}
	})
}

// rootServer serves an empty root directory. With release set, it calls held
// on the first GetInodeAttributes for the root, and answers that op once
// release is closed.
type rootServer struct {
	held    func()
	release chan struct{}
}

func (s *rootServer) ServeOps(c *Connection) {
	holding := s.release != nil
	for {
		ctx, op, err := c.ReadOp()
		if err != nil {
			return
		}
		o, ok := op.(*fuseops.GetInodeAttributesOp)
		if !ok || o.Inode != fuseops.RootInodeID {
			c.Reply(ctx, syscall.ENOSYS)
			continue
		}
		if holding {
			holding = false
			s.held()
			<-s.release
		}
		o.Attributes = fuseops.InodeAttributes{Nlink: 1, Mode: os.ModeDir | 0o555}
		c.Reply(ctx, nil)
	}
}

// mountPoint returns a new directory by the path the mount table lists.
func mountPoint(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// mountInfoEntry holds the fields of a /proc/self/mountinfo line that follow
// its "-" separator.
type mountInfoEntry struct {
	fstype, source, superOptions string
}

// mountInfo returns the /proc/self/mountinfo entry of the mount at dir, which
// must hold no character that mountinfo escapes, and whether there is one.
func mountInfo(t *testing.T, dir string) (mountInfoEntry, bool) {
	t.Helper()
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		// The mount point is the fifth field. The optional fields end at "-".
		fields := strings.Fields(line)
		sep := slices.Index(fields, "-")
		if sep < 6 || len(fields) < sep+4 || fields[4] != dir {
			continue
		}
		return mountInfoEntry{fstype: fields[sep+1], source: fields[sep+2], superOptions: fields[sep+3]}, true
	}
	return mountInfoEntry{}, false
}

// The mount table lists a mount's FSName as its source and fuse.<Subtype> as
// its type, and default_permissions unless DisableDefaultPermissions is set.
func TestMountTableListsTheConfig(t *testing.T) {
	mounttest.SkipUnlessEnabled(t)

	dir := mountPoint(t)
	mfs, err := Mount(dir, &rootServer{}, &MountConfig{FSName: "fusetest:repo", Subtype: "fusetest"})
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := mfs.Unmount(ctx); err != nil {
			t.Errorf("Unmount: %v", err)
		}
		if err := mfs.Join(ctx); err != nil {
			t.Errorf("Join: %v", err)
		}
	}()

	entry, ok := mountInfo(t, dir)
	if !ok {
		t.Fatalf("mountinfo lists no mount at %s", dir)
	}
	if entry.source != "fusetest:repo" || entry.fstype != "fuse.fusetest" {
		t.Errorf("mountinfo source and type = %q, %q, want %q, %q", entry.source, entry.fstype, "fusetest:repo", "fuse.fusetest")
	}
	if !slices.Contains(strings.Split(entry.superOptions, ","), "default_permissions") {
		t.Errorf("mountinfo super options = %q, want default_permissions", entry.superOptions)
	}
}

// When ctx ends while Mount waits on the server, MountContext returns an
// *AbandonedMountError. Once the mount completes, it is unmounted, and Wait
// returns nil.
func TestMountContextUnmountsAMountItAbandoned(t *testing.T) {
	mounttest.SkipUnlessEnabled(t)

	dir := mountPoint(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The poll hack waits on the root GETATTR, and the caller gives up while
	// the server holds it.
	server := &rootServer{held: cancel, release: make(chan struct{})}
	_, err := MountContext(ctx, dir, server, &MountConfig{})
	var abandoned *AbandonedMountError
	if !errors.As(err, &abandoned) || !errors.Is(err, context.Canceled) {
		close(server.release)
		t.Fatalf("MountContext error = %v, want an AbandonedMountError wrapping context.Canceled", err)
	}
	if _, ok := mountInfo(t, dir); !ok {
		t.Errorf("mountinfo lists no mount at %s while the server holds the root GETATTR", dir)
	}
	if err := abandoned.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Wait before the server answered = %v, want context.Canceled", err)
	}

	close(server.release)
	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Minute)
	defer waitCancel()
	if err := abandoned.Wait(waitCtx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if _, ok := mountInfo(t, dir); ok {
		t.Errorf("mountinfo still lists %s after Wait", dir)
	}
}
