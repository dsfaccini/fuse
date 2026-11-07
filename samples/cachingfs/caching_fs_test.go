// Copyright 2015 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cachingfs_test

import (
	"bytes"
	"io/ioutil"
	"os"
	"path"
	"syscall"
	"testing"
	"time"

	"github.com/dsfaccini/fuse/fuseutil"
	"github.com/dsfaccini/fuse/internal/mounttest"
	"github.com/dsfaccini/fuse/samples"
	"github.com/dsfaccini/fuse/samples/cachingfs"
	. "github.com/jacobsa/ogletest"
	"github.com/jacobsa/timeutil"
)

func TestCachingFS(t *testing.T) {
	mounttest.SkipUnlessEnabled(t)
	RunTests(t)
}

////////////////////////////////////////////////////////////////////////
// Boilerplate
////////////////////////////////////////////////////////////////////////

type cachingFSTest struct {
	samples.SampleTest

	fs           cachingfs.CachingFS
	initialMtime time.Time
}

var _ TearDownInterface = &cachingFSTest{}

func (t *cachingFSTest) setUp(
	ti *TestInfo,
	lookupEntryTimeout time.Duration,
	getattrTimeout time.Duration) {
	var err error

	// We assert things about whether or not mtimes are cached, but writeback
	// caching causes them to always be cached. Turn it off.
	t.MountConfig.DisableWritebackCaching = true

	// Create the file system.
	t.fs, err = cachingfs.NewCachingFS(lookupEntryTimeout, getattrTimeout)
	AssertEq(nil, err)

	t.Server = fuseutil.NewFileSystemServer(t.fs)

	// Mount it.
	t.SampleTest.SetUp(ti)

	// Set up the mtime.
	t.initialMtime = time.Date(2012, 8, 15, 22, 56, 0, 0, time.Local)
	t.fs.SetMtime(t.initialMtime)
}

func (t *cachingFSTest) statAll() (foo, dir, bar os.FileInfo) {
	var err error

	foo, err = os.Stat(path.Join(t.Dir, "foo"))
	AssertEq(nil, err)

	dir, err = os.Stat(path.Join(t.Dir, "dir"))
	AssertEq(nil, err)

	bar, err = os.Stat(path.Join(t.Dir, "dir/bar"))
	AssertEq(nil, err)

	return foo, dir, bar
}

func getInodeID(fi os.FileInfo) uint64 {
	return fi.Sys().(*syscall.Stat_t).Ino
}

////////////////////////////////////////////////////////////////////////
// Basics
////////////////////////////////////////////////////////////////////////

type BasicsTest struct {
	cachingFSTest
}

var _ SetUpInterface = &BasicsTest{}

func init() { RegisterTestSuite(&BasicsTest{}) }

func (t *BasicsTest) SetUp(ti *TestInfo) {
	const (
		lookupEntryTimeout = 0
		getattrTimeout     = 0
	)

	t.cachingFSTest.setUp(ti, lookupEntryTimeout, getattrTimeout)
}

func (t *BasicsTest) StatNonexistent() {
	names := []string{
		"blah",
		"bar",
		"dir/blah",
		"dir/dir",
		"dir/foo",
	}

	for _, n := range names {
		_, err := os.Stat(path.Join(t.Dir, n))

		AssertNe(nil, err)
		ExpectTrue(os.IsNotExist(err), "n: %s, err: %v", n, err)
	}
}

func (t *BasicsTest) StatFoo() {
	fi, err := os.Stat(path.Join(t.Dir, "foo"))
	AssertEq(nil, err)

	ExpectEq("foo", fi.Name())
	ExpectEq(cachingfs.FooSize, fi.Size())
	ExpectEq(0777, fi.Mode())
	ExpectThat(fi.ModTime(), timeutil.TimeEq(t.initialMtime))
	ExpectFalse(fi.IsDir())
	ExpectEq(t.fs.FooID(), getInodeID(fi))
	ExpectEq(1, fi.Sys().(*syscall.Stat_t).Nlink)
}

func (t *BasicsTest) StatDir() {
	fi, err := os.Stat(path.Join(t.Dir, "dir"))
	AssertEq(nil, err)

	ExpectEq("dir", fi.Name())
	ExpectEq(os.ModeDir|0777, fi.Mode())
	ExpectThat(fi.ModTime(), timeutil.TimeEq(t.initialMtime))
	ExpectTrue(fi.IsDir())
	ExpectEq(t.fs.DirID(), getInodeID(fi))
	ExpectEq(1, fi.Sys().(*syscall.Stat_t).Nlink)
}

func (t *BasicsTest) StatBar() {
	fi, err := os.Stat(path.Join(t.Dir, "dir/bar"))
	AssertEq(nil, err)

	ExpectEq("bar", fi.Name())
	ExpectEq(cachingfs.BarSize, fi.Size())
	ExpectEq(0777, fi.Mode())
	ExpectThat(fi.ModTime(), timeutil.TimeEq(t.initialMtime))
	ExpectFalse(fi.IsDir())
	ExpectEq(t.fs.BarID(), getInodeID(fi))
	ExpectEq(1, fi.Sys().(*syscall.Stat_t).Nlink)
}

////////////////////////////////////////////////////////////////////////
// No caching
////////////////////////////////////////////////////////////////////////

type NoCachingTest struct {
	cachingFSTest
}

var _ SetUpInterface = &NoCachingTest{}

func init() { RegisterTestSuite(&NoCachingTest{}) }

func (t *NoCachingTest) SetUp(ti *TestInfo) {
	const (
		lookupEntryTimeout = 0
		getattrTimeout     = 0
	)

	t.cachingFSTest.setUp(ti, lookupEntryTimeout, getattrTimeout)
}

func (t *NoCachingTest) StatRenumberStat() {
	t.statAll()
	t.fs.RenumberInodes()
	fooAfter, dirAfter, barAfter := t.statAll()

	// We should see the new inode IDs, because the entries should not have been
	// cached.
	ExpectEq(t.fs.FooID(), getInodeID(fooAfter))
	ExpectEq(t.fs.DirID(), getInodeID(dirAfter))
	ExpectEq(t.fs.BarID(), getInodeID(barAfter))
}

func (t *NoCachingTest) StatMtimeStat() {
	newMtime := t.initialMtime.Add(time.Second)

	t.statAll()
	t.fs.SetMtime(newMtime)
	fooAfter, dirAfter, barAfter := t.statAll()

	// We should see the new mtimes, because the attributes should not have been
	// cached.
	ExpectThat(fooAfter.ModTime(), timeutil.TimeEq(newMtime))
	ExpectThat(dirAfter.ModTime(), timeutil.TimeEq(newMtime))
	ExpectThat(barAfter.ModTime(), timeutil.TimeEq(newMtime))
}

func (t *NoCachingTest) StatRenumberMtimeStat() {
	newMtime := t.initialMtime.Add(time.Second)

	t.statAll()
	t.fs.RenumberInodes()
	t.fs.SetMtime(newMtime)
	fooAfter, dirAfter, barAfter := t.statAll()

	// We should see the new inode IDs and mtimes, because nothing should have
	// been cached.
	ExpectEq(t.fs.FooID(), getInodeID(fooAfter))
	ExpectEq(t.fs.DirID(), getInodeID(dirAfter))
	ExpectEq(t.fs.BarID(), getInodeID(barAfter))

	ExpectThat(fooAfter.ModTime(), timeutil.TimeEq(newMtime))
	ExpectThat(dirAfter.ModTime(), timeutil.TimeEq(newMtime))
	ExpectThat(barAfter.ModTime(), timeutil.TimeEq(newMtime))
}

////////////////////////////////////////////////////////////////////////
// Page cache
////////////////////////////////////////////////////////////////////////

type PageCacheTest struct {
	cachingFSTest
}

var _ SetUpInterface = &PageCacheTest{}

func init() { RegisterTestSuite(&PageCacheTest{}) }

func (t *PageCacheTest) SetUp(ti *TestInfo) {
	const (
		lookupEntryTimeout = 0
		getattrTimeout     = 0
	)

	t.cachingFSTest.setUp(ti, lookupEntryTimeout, getattrTimeout)
}

func (t *PageCacheTest) TwoFileHandles_NoKeepCache() {
	t.fs.SetKeepCache(false)

	// Open the file.
	f1, err := os.Open(path.Join(t.Dir, "foo"))
	AssertEq(nil, err)

	defer f1.Close()

	// Read its contents once.
	f1.Seek(0, 0)
	AssertEq(nil, err)

	c1, err := ioutil.ReadAll(f1)
	AssertEq(nil, err)
	AssertEq(cachingfs.FooSize, len(c1))

	// Open a second handle.
	f2, err := os.Open(path.Join(t.Dir, "foo"))
	AssertEq(nil, err)

	defer f2.Close()

	// We should see different contents if we read from that handle, due to the
	// cache being invalidated at the time of opening.
	f2.Seek(0, 0)
	AssertEq(nil, err)

	c2, err := ioutil.ReadAll(f2)
	AssertEq(nil, err)
	AssertEq(cachingfs.FooSize, len(c2))

	ExpectFalse(bytes.Equal(c1, c2))

	// Another read from the second handle should give the same result as the
	// first one from that handle.
	f2.Seek(0, 0)
	AssertEq(nil, err)

	c3, err := ioutil.ReadAll(f2)
	AssertEq(nil, err)
	AssertEq(cachingfs.FooSize, len(c3))

	ExpectTrue(bytes.Equal(c2, c3))

	// And another read from the first handle should give the same result yet
	// again.
	f1.Seek(0, 0)
	AssertEq(nil, err)

	c4, err := ioutil.ReadAll(f1)
	AssertEq(nil, err)
	AssertEq(cachingfs.FooSize, len(c4))

	ExpectTrue(bytes.Equal(c2, c4))
}

func (t *PageCacheTest) TwoFileHandles_KeepCache() {
	t.fs.SetKeepCache(true)

	// Open the file.
	f1, err := os.Open(path.Join(t.Dir, "foo"))
	AssertEq(nil, err)

	defer f1.Close()

	// Read its contents once.
	f1.Seek(0, 0)
	AssertEq(nil, err)

	c1, err := ioutil.ReadAll(f1)
	AssertEq(nil, err)
	AssertEq(cachingfs.FooSize, len(c1))

	// Open a second handle.
	f2, err := os.Open(path.Join(t.Dir, "foo"))
	AssertEq(nil, err)

	defer f2.Close()

	// We should see the same contents when we read via the second handle.
	f2.Seek(0, 0)
	AssertEq(nil, err)

	c2, err := ioutil.ReadAll(f2)
	AssertEq(nil, err)
	AssertEq(cachingfs.FooSize, len(c2))

	ExpectTrue(bytes.Equal(c1, c2))

	// Ditto if we read again from the first.
	f1.Seek(0, 0)
	AssertEq(nil, err)

	c3, err := ioutil.ReadAll(f1)
	AssertEq(nil, err)
	AssertEq(cachingfs.FooSize, len(c3))

	ExpectTrue(bytes.Equal(c1, c3))
}

////////////////////////////////////////////////////////////////////////
// Dir cache
////////////////////////////////////////////////////////////////////////

type DirCacheTest struct {
	cachingFSTest
}

var _ SetUpInterface = &DirCacheTest{}

func init() { RegisterTestSuite(&DirCacheTest{}) }

func (t *DirCacheTest) SetUp(ti *TestInfo) {
	const (
		lookupEntryTimeout = 0
		getattrTimeout     = 0
	)

	t.cachingFSTest.setUp(ti, lookupEntryTimeout, getattrTimeout)
}

func (t *DirCacheTest) CacheDirAndKeepDirCache() {
	t.fs.SetCacheDir(true)
	t.fs.SetKeepDirCache(true)

	// First read, kernel will cache the dir response.
	f, err := os.Open(path.Join(t.Dir, "dir"))
	defer f.Close() // Make sure to close specially required in failure scenario.
	AssertEq(nil, err)

	names1, err := f.Readdirnames(-1)
	AssertEq(nil, err)
	AssertEq(2, len(names1))

	err = f.Close()
	AssertEq(nil, err)

	// Second read, will be served from cache.
	f, err = os.Open(path.Join(t.Dir, "dir"))
	AssertEq(nil, err)

	names2, err := f.Readdirnames(-1)
	AssertEq(nil, err)
	AssertEq(2, len(names2))

	err = f.Close()
	AssertEq(nil, err)

	AssertEq(names1[0], names2[0])
	AssertEq(names1[1], names2[1])
}

func (t *DirCacheTest) NoCacheDirAndKeepDirCache() {
	t.fs.SetCacheDir(false)
	t.fs.SetKeepDirCache(true)

	// First read, no caching since NoCacheDir.
	f, err := os.Open(path.Join(t.Dir, "dir"))
	defer f.Close() // Make sure to close specially required in failure scenario.
	AssertEq(nil, err)

	names1, err := f.Readdirnames(-1)
	AssertEq(nil, err)
	AssertEq(2, len(names1))

	err = f.Close()
	AssertEq(nil, err)

	// Second read, will be served from filesystem, hence different name.
	f, err = os.Open(path.Join(t.Dir, "dir"))
	AssertEq(nil, err)

	names2, err := f.Readdirnames(-1)
	AssertEq(nil, err)
	AssertEq(2, len(names2))

	err = f.Close()
	AssertEq(nil, err)

	AssertNe(names1[0], names2[0])
	AssertNe(names1[1], names2[1])
}

func (t *DirCacheTest) CacheDirAndNoKeepDirCache() {
	t.fs.SetCacheDir(true)
	t.fs.SetKeepDirCache(false)

	// First read, kernel will cache the dir response.
	f, err := os.Open(path.Join(t.Dir, "dir"))
	defer f.Close() // Make sure to close specially required in failure scenario.
	AssertEq(nil, err)

	names1, err := f.Readdirnames(-1)
	AssertEq(nil, err)
	AssertEq(2, len(names1))

	err = f.Close()
	AssertEq(nil, err)

	// Second read, cached response will be invalidated since NoKeepDirCache.
	// Hence, different names.
	f, err = os.Open(path.Join(t.Dir, "dir"))
	AssertEq(nil, err)

	names2, err := f.Readdirnames(-1)
	AssertEq(nil, err)
	AssertEq(2, len(names2))

	err = f.Close()
	AssertEq(nil, err)

	AssertNe(names1[0], names2[0])
	AssertNe(names1[1], names2[1])
}

func (t *DirCacheTest) NoCacheDirAndNoKeepDirCache() {
	t.fs.SetCacheDir(false)
	t.fs.SetKeepDirCache(false)

	// First read, no caching since NoCacheDir.
	f, err := os.Open(path.Join(t.Dir, "dir"))
	defer f.Close() // Make sure to close specially required in failure scenario.
	AssertEq(nil, err)

	names1, err := f.Readdirnames(-1)
	AssertEq(nil, err)
	AssertEq(2, len(names1))

	err = f.Close()
	AssertEq(nil, err)

	// Second read, will be served from filesystem.
	// Since NoCacheDir also NoKeepDirCache. Hence, different names.
	f, err = os.Open(path.Join(t.Dir, "dir"))
	AssertEq(nil, err)

	names2, err := f.Readdirnames(-1)
	AssertEq(nil, err)
	AssertEq(2, len(names2))

	err = f.Close()
	AssertEq(nil, err)

	AssertNe(names1[0], names2[0])
	AssertNe(names1[1], names2[1])
}

func (t *DirCacheTest) CacheDirWithChangingKeepDirCache() {
	t.fs.SetCacheDir(true)
	t.fs.SetKeepDirCache(false)

	// First read, kernel will cache the dir response.
	f, err := os.Open(path.Join(t.Dir, "dir"))
	defer f.Close() // Make sure to close specially required in failure scenario.
	AssertEq(nil, err)

	names1, err := f.Readdirnames(-1)
	AssertEq(nil, err)
	AssertEq(2, len(names1))

	err = f.Close()
	AssertEq(nil, err)

	// Cached response will not be invalidated. Hence, served from kernel cache.
	t.fs.SetKeepDirCache(true)
	// Second read, will be served from cache
	f, err = os.Open(path.Join(t.Dir, "dir"))
	AssertEq(nil, err)

	names2, err := f.Readdirnames(-1)
	AssertEq(nil, err)
	AssertEq(2, len(names2))

	err = f.Close()
	AssertEq(nil, err)

	AssertEq(names1[0], names2[0])
	AssertEq(names1[1], names2[1])

	// Kernel has cached but invalidated due to NoKeepDirCache.
	// Hence, third read will be served from filesystem.
	t.fs.SetKeepDirCache(false)

	// Third read, will be served from filesystem. So, names will be different.
	f, err = os.Open(path.Join(t.Dir, "dir"))
	AssertEq(nil, err)

	names3, err := f.Readdirnames(-1)
	AssertEq(nil, err)
	AssertEq(2, len(names3))

	err = f.Close()
	AssertEq(nil, err)

	AssertNe(names2[0], names3[0])
	AssertNe(names2[1], names3[1])
}

func (t *DirCacheTest) ChangingCacheDirWithKeepDirCache() {
	t.fs.SetCacheDir(true)
	t.fs.SetKeepDirCache(true)

	// First read, kernel will cache the dir response.
	f, err := os.Open(path.Join(t.Dir, "dir"))
	defer f.Close() // Make sure to close specially required in failure scenario.
	AssertEq(nil, err)

	names1, err := f.Readdirnames(-1)
	AssertEq(nil, err)
	AssertEq(2, len(names1))

	err = f.Close()
	AssertEq(nil, err)

	// Cached response will not be invalidated, but also not be served from cache.
	// Since NoCacheDir so names will be different.
	t.fs.SetCacheDir(false)
	// Second read, will be served from filesystem.
	f, err = os.Open(path.Join(t.Dir, "dir"))
	AssertEq(nil, err)

	names2, err := f.Readdirnames(-1)
	AssertEq(nil, err)
	AssertEq(2, len(names2))

	err = f.Close()
	AssertEq(nil, err)

	AssertNe(names1[0], names2[0])
	AssertNe(names1[1], names2[1])

	// Third read will be served from cache.
	// But first read response is cached, since KeepDirCache.
	t.fs.SetCacheDir(true)

	// Third read, will be served from filesystem. So, names will be different.
	f, err = os.Open(path.Join(t.Dir, "dir"))
	AssertEq(nil, err)

	names3, err := f.Readdirnames(-1)
	AssertEq(nil, err)
	AssertEq(2, len(names3))

	err = f.Close()
	AssertEq(nil, err)

	AssertEq(names1[0], names3[0])
	AssertEq(names1[1], names3[1])
}
