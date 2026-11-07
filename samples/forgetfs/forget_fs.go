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

package forgetfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/dsfaccini/fuse"
	"github.com/dsfaccini/fuse/fuseops"
	"github.com/dsfaccini/fuse/fuseutil"
	"github.com/jacobsa/syncutil"
)

// Create a file system whose sole contents are a file named "foo" and a
// directory named "bar".
//
// The file "foo" may be opened for reading and/or writing, but reads and
// writes aren't supported. Additionally, any non-existent file or directory
// name may be created within any directory, but the resulting inode will
// appear to have been unlinked immediately.
//
// The file system maintains lookup counts for the inodes involved. It records
// a violation when the kernel forgets more lookups than an inode has, or names
// an inode the file system never issued or the kernel has forgotten. Check
// reports those violations, and each created inode the kernel still holds.
func NewFileSystem() *ForgetFS {
	// Set up the actual file system.
	impl := &fsImpl{
		inodes: map[fuseops.InodeID]*inode{
			cannedID_Root: &inode{
				attributes: fuseops.InodeAttributes{
					Nlink: 1,
					Mode:  0777 | os.ModeDir,
				},
			},
			cannedID_Foo: &inode{
				attributes: fuseops.InodeAttributes{
					Nlink: 1,
					Mode:  0777,
				},
			},
			cannedID_Bar: &inode{
				attributes: fuseops.InodeAttributes{
					Nlink: 1,
					Mode:  0777 | os.ModeDir,
				},
			},
		},
		nextInodeID: cannedID_Next,
		forgot:      make(chan struct{}),
	}

	// The root inode starts with a lookup count of one.
	impl.inodes[cannedID_Root].IncrementLookupCount()

	// The canned inodes are supposed to be stable from the user's point of view,
	// so we should allow them to be looked up at any point even if the kernel
	// has balanced its lookups with its forgets. Ensure that they never go to
	// zero until the file system is destroyed.
	impl.inodes[cannedID_Foo].IncrementLookupCount()
	impl.inodes[cannedID_Bar].IncrementLookupCount()

	// Set up the mutex.
	impl.mu = syncutil.NewInvariantMutex(impl.checkInvariants)

	// Set up a wrapper that exposes only certain methods.
	return &ForgetFS{
		impl:   impl,
		server: fuseutil.NewFileSystemServer(impl),
	}
}

////////////////////////////////////////////////////////////////////////
// ForgetFS
////////////////////////////////////////////////////////////////////////

type ForgetFS struct {
	impl   *fsImpl
	server fuse.Server
}

func (fs *ForgetFS) ServeOps(c *fuse.Connection) {
	fs.server.ServeOps(c)
}

// Check waits until the kernel has forgotten every inode that CreateFile and
// MkDir minted, or until ctx ends. Those inodes have no links and their
// entries do not stay cached, so the kernel forgets each once nothing uses it.
// Check returns the recorded violations, and the minted inodes the kernel
// still holds when ctx ends.
func (fs *ForgetFS) Check(ctx context.Context) error {
	return fs.impl.check(ctx)
}

////////////////////////////////////////////////////////////////////////
// Actual implementation
////////////////////////////////////////////////////////////////////////

const (
	cannedID_Root = fuseops.RootInodeID + iota
	cannedID_Foo
	cannedID_Bar
	cannedID_Next
)

type fsImpl struct {
	fuseutil.NotImplementedFileSystem

	/////////////////////////
	// Mutable state
	/////////////////////////

	mu syncutil.InvariantMutex

	// An index of inode by ID, for all IDs we have issued.
	//
	// GUARDED_BY(mu)
	inodes map[fuseops.InodeID]*inode

	// The next ID to issue.
	//
	// INVARIANT: For each k in inodes, k < nextInodeID
	//
	// GUARDED_BY(mu)
	nextInodeID fuseops.InodeID

	// Each violation of the lookup-count contract seen so far.
	//
	// GUARDED_BY(mu)
	violations []error

	// Closed and replaced on each forget.
	//
	// GUARDED_BY(mu)
	forgot chan struct{}
}

////////////////////////////////////////////////////////////////////////
// inode
////////////////////////////////////////////////////////////////////////

type inode struct {
	attributes fuseops.InodeAttributes

	// The current lookup count.
	lookupCount uint64

	// true if lookupCount has ever been positive.
	lookedUp bool
}

func (in *inode) Forgotten() bool {
	return in.lookedUp && in.lookupCount == 0
}

func (in *inode) IncrementLookupCount() {
	in.lookupCount++
	in.lookedUp = true
}

////////////////////////////////////////////////////////////////////////
// Helpers
////////////////////////////////////////////////////////////////////////

// LOCKS_REQUIRED(fs.mu)
func (fs *fsImpl) checkInvariants() {
	// INVARIANT: For each k in inodes, k < nextInodeID
	for k, _ := range fs.inodes {
		if !(k < fs.nextInodeID) {
			panic("Unexpectedly large inode ID")
		}
	}
}

// LOCKS_EXCLUDED(fs.mu)
func (fs *fsImpl) check(ctx context.Context) error {
	for {
		fs.mu.Lock()
		errs := append([]error(nil), fs.violations...)
		var held []fuseops.InodeID
		for id, in := range fs.inodes {
			if id >= cannedID_Next && in.lookupCount != 0 {
				held = append(held, id)
			}
		}
		forgot := fs.forgot
		fs.mu.Unlock()

		if len(held) == 0 {
			return errors.Join(errs...)
		}

		select {
		case <-forgot:
		case <-ctx.Done():
			sort.Slice(held, func(i, j int) bool { return held[i] < held[j] })
			errs = append(errs, fmt.Errorf("%v: the kernel still holds created inodes %v", ctx.Err(), held))
			return errors.Join(errs...)
		}
	}
}

// LOCKS_REQUIRED(fs.mu)
func (fs *fsImpl) violate(format string, args ...interface{}) {
	fs.violations = append(fs.violations, fmt.Errorf(format, args...))
}

// Look up the inode, recording a violation and returning nil when the file
// system never issued it or the kernel has forgotten it.
//
// LOCKS_REQUIRED(fs.mu)
func (fs *fsImpl) findInodeByID(id fuseops.InodeID) *inode {
	in := fs.inodes[id]
	switch {
	case in == nil:
		fs.violate("unknown inode: %v", id)
		return nil

	case in.Forgotten():
		fs.violate("forgotten inode: %v", id)
		return nil
	}

	return in
}

////////////////////////////////////////////////////////////////////////
// FileSystem methods
////////////////////////////////////////////////////////////////////////

func (fs *fsImpl) StatFS(
	ctx context.Context,
	op *fuseops.StatFSOp) error {
	return nil
}

func (fs *fsImpl) LookUpInode(
	ctx context.Context,
	op *fuseops.LookUpInodeOp) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Make sure the parent exists and has not been forgotten.
	if fs.findInodeByID(op.Parent) == nil {
		return fuse.EIO
	}

	// Handle the names we support.
	var childID fuseops.InodeID
	switch {
	case op.Parent == cannedID_Root && op.Name == "foo":
		childID = cannedID_Foo

	case op.Parent == cannedID_Root && op.Name == "bar":
		childID = cannedID_Bar

	default:
		return fuse.ENOENT
	}

	// Look up the child.
	child := fs.findInodeByID(childID)
	if child == nil {
		return fuse.EIO
	}
	child.IncrementLookupCount()

	// Return an appropriate entry.
	op.Entry = fuseops.ChildInodeEntry{
		Child:      childID,
		Attributes: child.attributes,
	}

	return nil
}

func (fs *fsImpl) GetInodeAttributes(
	ctx context.Context,
	op *fuseops.GetInodeAttributesOp) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Find the inode, verifying that it has not been forgotten.
	in := fs.findInodeByID(op.Inode)
	if in == nil {
		return fuse.EIO
	}

	// Return appropriate attributes.
	op.Attributes = in.attributes

	return nil
}

func (fs *fsImpl) ForgetInode(
	ctx context.Context,
	op *fuseops.ForgetInodeOp) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Find the inode and decrement its count.
	in := fs.findInodeByID(op.Inode)
	if in == nil {
		return nil
	}
	if in.lookupCount < op.N {
		fs.violate("forget of %v lookups of inode %v, which has %v", op.N, op.Inode, in.lookupCount)
	}
	in.lookupCount -= min(op.N, in.lookupCount)

	close(fs.forgot)
	fs.forgot = make(chan struct{})

	return nil
}

func (fs *fsImpl) MkDir(
	ctx context.Context,
	op *fuseops.MkDirOp) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Make sure the parent exists and has not been forgotten.
	if fs.findInodeByID(op.Parent) == nil {
		return fuse.EIO
	}

	// Mint a child inode.
	childID := fs.nextInodeID
	fs.nextInodeID++

	child := &inode{
		attributes: fuseops.InodeAttributes{
			Nlink: 0,
			Mode:  0777 | os.ModeDir,
		},
	}

	fs.inodes[childID] = child
	child.IncrementLookupCount()

	// Return an appropriate entry.
	op.Entry = fuseops.ChildInodeEntry{
		Child:      childID,
		Attributes: child.attributes,
	}

	return nil
}

func (fs *fsImpl) CreateFile(
	ctx context.Context,
	op *fuseops.CreateFileOp) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Make sure the parent exists and has not been forgotten.
	if fs.findInodeByID(op.Parent) == nil {
		return fuse.EIO
	}

	// Mint a child inode.
	childID := fs.nextInodeID
	fs.nextInodeID++

	child := &inode{
		attributes: fuseops.InodeAttributes{
			Nlink: 0,
			Mode:  0777,
		},
	}

	fs.inodes[childID] = child
	child.IncrementLookupCount()

	// Return an appropriate entry.
	op.Entry = fuseops.ChildInodeEntry{
		Child:      childID,
		Attributes: child.attributes,
	}

	return nil
}

func (fs *fsImpl) OpenFile(
	ctx context.Context,
	op *fuseops.OpenFileOp) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Verify that the inode has not been forgotten.
	if fs.findInodeByID(op.Inode) == nil {
		return fuse.EIO
	}

	return nil
}

func (fs *fsImpl) OpenDir(
	ctx context.Context,
	op *fuseops.OpenDirOp) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Verify that the inode has not been forgotten.
	if fs.findInodeByID(op.Inode) == nil {
		return fuse.EIO
	}

	return nil
}
