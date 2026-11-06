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

package fuse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"runtime"
	"strings"

	"github.com/dsfaccini/fuse/internal/buffer"
)

// Optional configuration accepted by Mount.
//
// On every OS and backend, Mount rejects a config whose FSName, Subtype,
// VolumeName or Options contain a comma or a backslash. A comma separates
// mount options, and fusermount reads a backslash in fsname and subtype as an
// escape.
type MountConfig struct {
	// The context from which every op read from the connetion by the sever
	// should inherit. If nil, context.Background() will be used.
	OpContext context.Context

	// If non-empty, the name of the file system as displayed by e.g. `mount`.
	// This is important because the `umount` command requires root privileges if
	// it doesn't agree with /etc/fstab.
	FSName string

	// Mount the file system in read-only mode. File modes will appear as normal,
	// but opening a file for writing and metadata operations like chmod,
	// chtimes, etc. will fail.
	ReadOnly bool

	// A logger to use for logging errors. If nil, no error logging is
	// performed.
	//
	// It receives every error a file system returns for an op, except those
	// that are expected. The errno the kernel receives decides. It does not
	// receive:
	//
	//   - ENOSYS or EINTR from any op.
	//   - ENOENT from LookUpInode.
	//   - ENOTEMPTY from RmDir or Rename.
	//   - ENODATA or ERANGE from GetXattr or ListXattr.
	//
	// It also receives, with its stack, each panic that fuseutil recovers
	// from a FileSystem method. It receives each method that fuseutil saw
	// exit through runtime.Goexit, and each BytesRead that does not fit its
	// op's buffer. Those ops are answered with EIO.
	//
	// It receives each panic from a ReadFileOp or WriteFileOp Callback, with
	// its stack. The Callback runs after the reply is sent, so the panic is
	// only logged.
	ErrorLogger *log.Logger

	// A logger to use for logging debug information. If nil, no debug logging is
	// performed.
	DebugLogger *log.Logger

	// A logger to use for logging fuse wire requests. If nil, no wire logging is
	// performed. The library serializes its writes to it per mount. A writer
	// that several mounts share must be safe for concurrent use.
	WireLogger io.Writer

	// Linux only. OS X always behaves as if writeback caching is disabled.
	//
	// By default on Linux we allow the kernel to perform writeback caching
	// (https://tinyurl.com/3ma8ypeu):
	//
	// *   When the user calls write(2), the kernel sticks the user's data into
	//     its page cache. Only later does it call through to the file system,
	//     potentially after coalescing multiple small user writes.
	//
	// *   The file system may receive multiple write ops from the kernel
	//     concurrently if there is a lot of page cache data to flush.
	//
	// *   Write performance may be significantly improved due to the user and
	//     the kernel not waiting for serial round trips to the file system. This
	//     is especially true if the user makes tiny writes.
	//
	// *   close(2) (and anything else calling f_op->flush) causes all dirty
	//     pages to be written out before it proceeds to send a FlushFileOp
	//     (https://tinyurl.com/3ur6vmsv).
	//
	// *   Similarly, close(2) causes the kernel to send a setattr request
	//     filling in the mtime if any dirty pages were flushed, since the time
	//     at which the pages were written to the file system can't be trusted.
	//
	// *   close(2) (and anything else calling f_op->flush) writes out all dirty
	//     pages, then sends a setattr request with an appropriate mtime for
	//     those writes if there were any, and only then proceeds to send a
	//     flush.
	//
	//     Code walk:
	//
	//     *   (https://tinyurl.com/3ur6vmsv) fuse_flush calls write_inode_now
	//         before calling the file system. The latter eventually calls into
	//         __writeback_single_inode.
	//
	//     *   (https://tinyurl.com/35vtmtsz) __writeback_single_inode calls
	//         do_writepages, which writes out any dirty pages.
	//
	//     *   (https://tinyurl.com/3wv4paaf) __writeback_single_inode later
	//         calls write_inode, which calls into the superblock op struct's
	//         write_inode member. For fuse, this is fuse_write_inode
	//         (https://tinyurl.com/mrxupe98).
	//
	//     *   (https://tinyurl.com/mrxt9bta) fuse_write_inode calls
	//         fuse_flush_times.
	//
	//     *   (https://tinyurl.com/mr49cjdf) fuse_flush_times sends a setttr
	//         request for setting the inode's mtime.
	//
	// However, this brings along some caveats:
	//
	// *   The file system must handle SetInodeAttributesOp or close(2) will fail,
	//     due to the call chain into fuse_flush_times listed above.
	//
	// *   The kernel caches mtime and ctime regardless of whether the file
	//     system tells it to do so, disregarding the result of further getattr
	//     requests (https://tinyurl.com/mrxnfatv, https://tinyurl.com/27jju8n4).
	//     It appears this may be true of the file size, too. Writeback caching
	//     may therefore not be suitable for file systems where these attributes
	//     can spontaneously change for reasons the kernel doesn't observe. See
	//     https://tinyurl.com/yyprvjvs for more discussion.
	//
	// Setting DisableWritebackCaching disables this behavior. Instead the file
	// system is called one or more times for each write(2), and the user's
	// syscall doesn't return until the file system returns.
	DisableWritebackCaching bool

	// OS X only.
	//
	// Normally on OS X we mount with the novncache option
	// (https://tinyurl.com/52hz9vya), which disables entry caching in the
	// kernel. This is because macFUSE (osxfuse) does not honor the entry
	// expiration values we return to it, instead caching potentially forever
	// (https://tinyurl.com/2rr6cd3m), and it is probably better to fail to cache
	// than to cache for too long, since the latter is more likely to hide
	// consistency bugs that are difficult to detect and diagnose.
	//
	// This field disables the use of novncache, restoring entry caching. Beware:
	// the value of ChildInodeEntry.EntryExpiration is ignored by the kernel, and
	// entries will be cached for an arbitrarily long time.
	//
	// The FSKit backend never receives novncache, so this field has no effect
	// there.
	EnableVnodeCaching bool

	// OS X only.
	//
	// Normally on OS X we mount with the noappledouble option
	// (https://github.com/osxfuse/osxfuse/wiki/Mount-options), which makes
	// Apple Double files (._foo) and .DS_Store files inaccessible, even when the
	// file system serves them.
	//
	// This field omits noappledouble, so those files stay visible and Finder may
	// create them. The FSKit backend never receives noappledouble, so this
	// field has no effect there.
	EnableAppleDouble bool

	// Linux only.
	//
	// Linux 4.20 introduced caching symlink targets in the page cache:
	// https://github.com/torvalds/linux/commit/5571f1e65486be025f73fa6aa30fb03725d362a2
	//
	// This is not enabled by default because the old behavior masked a bug:
	// file systems could return any size in the inode attributes of
	// symlinks. After enabling caching, the specified size caps the symlink
	// target.
	EnableSymlinkCaching bool

	// Linux only.
	//
	// Tell the kernel to treat returning -ENOSYS on OpenFile as not needing
	// OpenFile calls at all (Linux >= 3.16):
	EnableNoOpenSupport bool

	// Linux only.
	//
	// Negotiate FUSE_AUTO_INVAL_DATA when the kernel offers it. Each read
	// through the page cache then first refreshes the file's attributes if
	// they have expired [1].
	//
	// Without writeback caching, the kernel drops a regular file's cached
	// pages whenever new attributes carry a changed size. With this field, it
	// also drops them when the mtime changed.
	//
	// This matters to a file system that keeps the page cache across opens. It
	// keeps the cache when it sets OpenFileOp.KeepPageCache. It also keeps the
	// cache when it sets EnableNoOpenSupport and answers OpenFile with ENOSYS,
	// because the kernel then keeps the cache by default [2]. A change that
	// keeps the size but changes the mtime stops being served from stale pages
	// once the attributes expire. A change that keeps both the size and the
	// mtime is still served stale.
	//
	// With writeback caching, the default unless DisableWritebackCaching is
	// set, the kernel keeps its own size and mtime for regular files. It never
	// drops their pages for a change the file system reports [3]. Reads still
	// refresh expired attributes, so each read after an expiration costs a
	// GETATTR [4] [5].
	//
	// [1]: https://github.com/torvalds/linux/blob/f66469d9905b39d2c260a15d7d6db450f4db67c1/fs/fuse/file.c#L1155-L1174
	// [2]: https://github.com/torvalds/linux/blob/f66469d9905b39d2c260a15d7d6db450f4db67c1/fs/fuse/file.c#L160
	// [3]: https://github.com/torvalds/linux/blob/f66469d9905b39d2c260a15d7d6db450f4db67c1/fs/fuse/inode.c#L304-L386
	// [4]: https://github.com/torvalds/linux/blob/f66469d9905b39d2c260a15d7d6db450f4db67c1/fs/fuse/dir.c#L1564-L1567
	// [5]: https://github.com/torvalds/linux/blob/f66469d9905b39d2c260a15d7d6db450f4db67c1/fs/fuse/dir.c#L1596-L1599
	EnableAutoInvalData bool

	// Linux only.
	//
	// Tell the kernel to treat returning -ENOSYS on OpenDir as not needing
	// OpenDir calls at all (Linux >= 5.1):
	EnableNoOpendirSupport bool

	// Disable FUSE default permissions.
	// This is useful for situations where the backing data store (e.g., S3) doesn't
	// actually utilise any form of qualifiable UNIX permissions.
	DisableDefaultPermissions bool

	// OS X only.
	//
	// The name of the mounted volume, as displayed in the Finder. If empty,
	// macFUSE picks a default name.
	VolumeName string

	// The mechanism to mount through. The zero value is the platform's
	// default: BackendLinux on Linux, BackendMacFUSEKernelExtension on darwin.
	// darwin also has BackendFSKit; see FSKitAvailable for what it needs.
	// Mount rejects a backend the platform lacks.
	//
	// Options["backend"] = "fskit" is a deprecated spelling of BackendFSKit.
	// When Backend is set too, Backend wins. Either way, Mount does not pass
	// that option on to the mount.
	Backend Backend

	// Additional key=value options to pass unadulterated to the underlying mount
	// command. See `man 8 mount`, the fuse documentation, etc. for
	// system-specific information.
	//
	// For expert use only! May invalidate other guarantees made in the
	// documentation for this package.
	Options map[string]string

	// Sets the filesystem type (third field in /etc/mtab). /etc/mtab and
	// /proc/mounts will show the filesystem type as fuse.<Subtype>.
	// If not set, /proc/mounts will show the filesystem type as fuse/fuseblk.
	Subtype string

	// Flag to enable async buffered reads
	EnableAsyncReads bool

	// Flag to enable async direct IO
	EnableAsyncDIO bool

	// Flag to enable parallel lookup and readdir operations from the
	// kernel
	// Ref: https://github.com/torvalds/linux/commit/5c672ab3f0ee0f78f7acad183f34db0f8781a200
	EnableParallelDirOps bool

	// Flag to enable atomic truncate during file open operations.
	// When enabled, application calls to open with the O_TRUNC flag will cause a FUSE OpenFile
	// op with the O_TRUNC flag set. In comparison, the default behavior is an OpenFile op
	// without O_TRUNC, followed by a SetInodeAttributes op with the target size set to 0.
	// Ref: https://github.com/torvalds/linux/commit/6ff958edbf39c014eb06b65ad25b736be08c4e63
	EnableAtomicTrunc bool

	// Flag to tell the kernel we support ReadDirPlus, which optimizes performance
	// by returning not just the directory entries (like ReadDir), but also their inode
	// attributes, thereby saving one extra Lookup request per directory entry.
	EnableReaddirplus bool

	// Flag to enable adaptive ReadDirPlus.
	// This is only effective if EnableReaddirplus is true.
	//
	// When both flags are set, the kernel may dynamically choose between issuing
	// ReaddirPlus and Readdir requests based on observed access patterns.
	// For example, `ls` (which lists filenames only) may fall back to Readdir after an initial ReaddirPlus,
	// whereas `ls -l` is more likely to continue using ReaddirPlus.
	//
	// If EnableReaddirplus is true and this flag is false, the kernel will always
	// use ReaddirPlus for directory listing.
	EnableAutoReaddirplus bool

	// UseVectoredRead is a legacy flag kept for backward compatibility. It is now a no-op.
	//
	// The term vectored read was a misnomer for this flag. Its actual meaning was that
	// the file system would allocate its own buffers for read operations.
	//
	// When this flag was disabled, the FUSE library provided a buffer
	// in ReadFileOp.Dst. This buffer utilized unused space within the
	// incoming kernel request message's buffer (InMessage.storage), which is
	// sized to hold a page plus the maximum write size. Since read requests are
	// small, there is ample room for read responses.
	//
	// Conversely, when this flag was enabled, ReadFileOp.Dst was always nil.
	// This allowed file systems to avoid memory copying overhead if the data was
	// already in memory, by requiring them to return the data as a list of slices
	// in ReadFileOp.Data.
	//
	// Currently, both the read mechanisms can coexist. The library's behavior is
	// to always provide ReadFileOp.Dst. If the file system populates ReadFileOp.Data,
	// that data will be used for a vectored read, irrespective of this flag's value.
	UseVectoredRead bool

	// The kernel limit applies on Linux only. The reply cap below applies on
	// every OS.
	//
	// If set, this specifies the maximum number of hardware pages that the
	// FUSE message payload can use. This will be passed to the kernel during the
	// FUSE_INIT phase.
	//
	// It applies to both requests (e.g., write) and replies (e.g., read).
	//
	// If you want to support larger read requests (e.g., 2 MiB) but keep the
	// default write size, you only need to increase MaxPages (e.g., to 512
	// for a 4KB page size).
	//
	// If unset, a default value based on the maximum of the default read and
	// write sizes is used (typically 256 pages, which is 1 MiB for a 4KB page size).
	//
	// If MaxPages is unset but MaxWrite is set, MaxPages will default to a
	// value large enough to support both MaxWrite and the default read size.
	//
	// If MaxPages is set but MaxWrite is unset, MaxWrite will be limited based
	// on MaxPages (i.e., MaxPages * PageSize).
	//
	// Reply cap: a READDIR, READDIRPLUS, GETXATTR or LISTXATTR request that
	// asks for a reply larger than MaxPages * PageSize gets EINVAL. For
	// GETXATTR and LISTXATTR the cap is at least 64 KiB, or 128 KiB on darwin.
	//
	// On darwin the kernel extension asks for the size of the caller's buffer.
	// It clamps READDIR and READDIRPLUS to the iosize the mount passes, 1 MiB,
	// and their cap is at least that. It does not clamp GETXATTR or LISTXATTR,
	// so one through a buffer larger than the cap gets EINVAL. Raise MaxPages
	// to serve larger xattr buffers.
	MaxPages uint16

	// If set, this specifies the maximum size in bytes of a write request
	// that the FUSE connection will support. This will be passed to the kernel
	// during the FUSE_INIT phase.
	//
	// Note that if you increase MaxWrite, you must also ensure MaxPages is
	// large enough to support it (MaxPages >= MaxWrite / PageSize).
	//
	// If unset, a default value of 1 MiB is used (or less if MaxPages is set
	// to a value smaller than 256 pages).
	MaxWrite uint32
}

// Create a map containing all of the key=value mount options to be given to
// the mount helper.
func (c *MountConfig) toMap() (opts map[string]string) {
	isDarwin := runtime.GOOS == "darwin"
	opts = make(map[string]string)

	// Enable permissions checking in the kernel. See the comments on
	// InodeAttributes.Mode.
	if !c.DisableDefaultPermissions {
		opts["default_permissions"] = ""
	}

	// HACK(jacobsa): Work around what appears to be a bug in systemd v219, as
	// shipped in Ubuntu 15.04, where it automatically unmounts any file system
	// that doesn't set an explicit name.
	//
	// When Ubuntu contains systemd v220, this workaround should be removed and
	// the systemd bug reopened if the problem persists.
	//
	// Cf. https://github.com/bazil/fuse/issues/89
	// Cf. https://bugs.freedesktop.org/show_bug.cgi?id=90907
	fsname := c.FSName
	if runtime.GOOS == "linux" && fsname == "" {
		fsname = "some_fuse_file_system"
	}

	// Special file system name?
	if fsname != "" {
		opts["fsname"] = fsname
	}

	subtype := c.Subtype
	if subtype != "" {
		opts["subtype"] = subtype
	}

	// Read only?
	if c.ReadOnly {
		opts["ro"] = ""
	}

	// Handle OS X options. The FSKit module parses neither novncache nor
	// noappledouble.
	fskit := c.Backend == BackendFSKit
	if isDarwin {
		if !fskit && !c.EnableVnodeCaching {
			opts["novncache"] = ""
		}

		if c.VolumeName != "" {
			// Cf. https://github.com/osxfuse/osxfuse/wiki/Mount-options#volname
			opts["volname"] = c.VolumeName
		}
	}

	// OS X: disable the use of "Apple Double" (._foo and .DS_Store) files, which
	// just add noise to debug output and can have significant cost on
	// network-based file systems.
	//
	// Cf. https://github.com/osxfuse/osxfuse/wiki/Mount-options
	if isDarwin && !fskit && !c.EnableAppleDouble {
		opts["noappledouble"] = ""
	}

	// Last but not least: other user-supplied options.
	for k, v := range c.Options {
		opts[k] = v
	}

	return opts
}

func mapToOptionsString(opts map[string]string) string {
	var components []string
	for k, v := range opts {
		component := k
		if v != "" {
			component = fmt.Sprintf("%s=%s", k, v)
		}

		components = append(components, component)
	}

	return strings.Join(components, ",")
}

// Create an options string suitable for passing to the mount helper.
func (c *MountConfig) toOptionsString() string {
	return mapToOptionsString(c.toMap())
}

// normalize returns a copy of c with its defaults filled in, or an error when
// the kernel or the mount helper cannot take c.
func (c *MountConfig) normalize() (MountConfig, error) {
	if c == nil {
		return MountConfig{}, errors.New("MountConfig must not be nil")
	}
	cfg := *c
	if cfg.Options["backend"] == "fskit" {
		if cfg.Backend == "" {
			cfg.Backend = BackendFSKit
		}
		cfg.Options = maps.Clone(cfg.Options)
		delete(cfg.Options, "backend")
	}
	if cfg.OpContext == nil {
		cfg.OpContext = context.Background()
	}
	if err := cfg.sanitizeMaxPagesAndWrite(); err != nil {
		return MountConfig{}, err
	}
	// toMap passes VolumeName only on darwin. Check it on every OS so that one
	// rule holds.
	if err := checkMountOption("volname", cfg.VolumeName); err != nil {
		return MountConfig{}, err
	}
	for k, v := range cfg.toMap() {
		if err := checkMountOption(k, v); err != nil {
			return MountConfig{}, err
		}
	}
	if err := checkBackend(cfg.Backend); err != nil {
		return MountConfig{}, err
	}
	return cfg, nil
}

// checkBackend rejects a backend that this platform lacks.
func checkBackend(b Backend) error {
	switch {
	case b == "",
		runtime.GOOS == "linux" && b == BackendLinux,
		runtime.GOOS == "darwin" && (b == BackendMacFUSEKernelExtension || b == BackendFSKit):
		return nil
	}
	return fmt.Errorf("backend %q is not available on %s", b, runtime.GOOS)
}

// checkMountOption rejects an option that would end or escape early on some
// backend.
func checkMountOption(k, v string) error {
	if strings.ContainsAny(k+v, `,\`) {
		return fmt.Errorf("mount options cannot contain commas or backslashes: %q=%q", k, v)
	}
	return nil
}

func (c *MountConfig) sanitizeMaxPagesAndWrite() error {
	pageSize := uint32(buffer.GetPageSize())

	// If neither are given, set them both to the default based on MaxReadSize & MaxWriteSize.
	if c.MaxPages == 0 && c.MaxWrite == 0 {
		c.MaxWrite = buffer.MaxWriteSize
		maxPayload := max(uint32(buffer.MaxReadSize), uint32(buffer.MaxWriteSize))
		c.MaxPages = uint16(maxPayload / pageSize)
	} else if c.MaxPages != 0 && c.MaxWrite == 0 {
		// If only MaxPages is given, set MaxWrite based on MaxPages.
		c.MaxWrite = uint32(c.MaxPages) * pageSize
	} else if c.MaxPages == 0 && c.MaxWrite != 0 {
		// If only MaxWrite is given, MaxPages defaults to max(MaxWrite, MaxReadSize).
		maxPayload := max(c.MaxWrite, uint32(buffer.MaxReadSize))
		c.MaxPages = uint16((maxPayload + pageSize - 1) / pageSize)
	}

	// Validate: MaxPages needs to be >= MaxWrite (in bytes)
	if uint32(c.MaxPages)*pageSize < c.MaxWrite {
		return fmt.Errorf("MaxPages (%d pages, %d bytes) must be at least MaxWrite (%d bytes)",
			c.MaxPages, uint32(c.MaxPages)*pageSize, c.MaxWrite)
	}

	return nil
}
