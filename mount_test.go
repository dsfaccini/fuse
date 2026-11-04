package fuse_test

import (
	"context"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dsfaccini/fuse"
	"github.com/dsfaccini/fuse/fuseops"
	"github.com/dsfaccini/fuse/fuseutil"
	"github.com/dsfaccini/fuse/internal/mounttest"
)

////////////////////////////////////////////////////////////////////////
// minimalFS
////////////////////////////////////////////////////////////////////////

// A minimal fuseutil.FileSystem that can successfully mount but do nothing
// else.
type minimalFS struct {
	fuseutil.NotImplementedFileSystem
}

func (fs *minimalFS) StatFS(
	ctx context.Context,
	op *fuseops.StatFSOp) error {
	return nil
}

////////////////////////////////////////////////////////////////////////
// Tests
////////////////////////////////////////////////////////////////////////

func TestSuccessfulMount(t *testing.T) {
	mounttest.SkipUnlessEnabled(t)

	ctx := context.Background()

	// Set up a temporary directory.
	dir, err := ioutil.TempDir("", "mount_test")
	if err != nil {
		t.Fatalf("ioutil.TempDir: %v", err)
	}

	defer os.RemoveAll(dir)

	// Mount.
	fs := &minimalFS{}
	mfs, err := fuse.Mount(
		dir,
		fuseutil.NewFileSystemServer(fs),
		&fuse.MountConfig{})

	if err != nil {
		t.Fatalf("fuse.Mount: %v", err)
	}

	defer func() {
		if err := mfs.Join(ctx); err != nil {
			t.Errorf("Joining: %v", err)
		}
	}()

	defer fuse.Unmount(mfs.Dir())
}

func TestNonexistentMountPoint(t *testing.T) {
	ctx := context.Background()

	// Set up a temporary directory.
	dir, err := ioutil.TempDir("", "mount_test")
	if err != nil {
		t.Fatalf("ioutil.TempDir: %v", err)
	}

	defer os.RemoveAll(dir)

	// Attempt to mount into a sub-directory that doesn't exist.
	fs := &minimalFS{}
	mfs, err := fuse.Mount(
		path.Join(dir, "foo"),
		fuseutil.NewFileSystemServer(fs),
		&fuse.MountConfig{})

	if err == nil {
		fuse.Unmount(mfs.Dir())
		mfs.Join(ctx)
		t.Fatal("fuse.Mount returned nil")
	}

	const want = "no such file"
	if got := err.Error(); !strings.Contains(got, want) {
		t.Errorf("Unexpected error: %v", got)
	}
}

// The mount point does not exist, so Mount fails without mounting. It reports
// the config error only when it validates the config first.
func TestMountRejectsInvalidConfigBeforeMounting(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	const separator = "cannot contain commas or backslashes"
	for _, test := range []struct {
		name   string
		config *fuse.MountConfig
		want   string
	}{
		{name: "nil", want: "must not be nil"},
		{name: "comma in FSName", config: &fuse.MountConfig{FSName: "fs:x,allow_other"}, want: separator},
		{name: "comma in Subtype", config: &fuse.MountConfig{Subtype: "x,allow_other"}, want: separator},
		{name: "comma in VolumeName", config: &fuse.MountConfig{VolumeName: "x,allow_other"}, want: separator},
		{name: "comma in option value", config: &fuse.MountConfig{Options: map[string]string{"opt": "x,allow_other"}}, want: separator},
		{name: "comma in option key", config: &fuse.MountConfig{Options: map[string]string{"x,allow_other": ""}}, want: separator},
		{name: "comma on FSKit", config: &fuse.MountConfig{FSName: "fs:x,allow_other", Options: map[string]string{"backend": "fskit"}}, want: separator},
		{name: "backslash in FSName", config: &fuse.MountConfig{FSName: `fs:x\`}, want: separator},
		{name: "backslash in Subtype", config: &fuse.MountConfig{Subtype: `x\`}, want: separator},
		{name: "backslash in option value", config: &fuse.MountConfig{Options: map[string]string{"opt": `x\`}}, want: separator},
		{name: "backslash in option key", config: &fuse.MountConfig{Options: map[string]string{`x\`: ""}}, want: separator},
		{name: "MaxWrite above MaxPages", config: &fuse.MountConfig{MaxPages: 1, MaxWrite: 1 << 20}, want: "must be at least MaxWrite"},
	} {
		_, err := fuse.Mount(missing, nil, test.config)
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: Mount error = %v, want it to contain %q", test.name, err, test.want)
		}
	}
}
