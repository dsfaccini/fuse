// Copyright 2026 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package fusekernel

import (
	"reflect"
	"testing"
)

// The layout tests check each wire struct that the library reads or writes
// against the kernel's C struct:
//   - Linux include/uapi/linux/fuse.h, at
//     https://github.com/torvalds/linux/blob/05441d697f7f9a927264f4058cbbc6a03dfcac9d/include/uapi/linux/fuse.h
//   - macFUSE include/fuse_kernel.h, at
//     https://github.com/macfuse/library/blob/85f89460c4962ae94a84e25037326b22efb7f0e0/include/fuse_kernel.h
//
// The protocol tests build their expected bytes from these same structs, so
// only these tests catch a struct that differs from the kernel's.

// field pairs a Go field with the C field at its offset.
type field struct {
	goName string
	cName  string
	offset uintptr
}

// layout is the C struct that a Go struct holds. size is the C struct's size,
// or, where the library reads or writes only a prefix of it, the prefix's.
type layout struct {
	cName  string
	v      any
	size   uintptr
	fields []field
}

// commonLayouts are the structs that Linux and macFUSE lay out alike.
func commonLayouts() []layout {
	return []layout{
		{"fuse_kstatfs", Kstatfs{}, 80, []field{{"Blocks", "blocks", 0}, {"Bfree", "bfree", 8}, {"Bavail", "bavail", 16}, {"Files", "files", 24}, {"Ffree", "ffree", 32}, {"Bsize", "bsize", 40}, {"Namelen", "namelen", 44}, {"Frsize", "frsize", 48}, {"Padding", "padding", 52}, {"Spare", "spare", 56}}},
		{"fuse_forget_in", ForgetIn{}, 8, []field{{"Nlookup", "nlookup", 0}}},
		{"fuse_batch_forget_in", BatchForgetCountIn{}, 8, []field{{"Count", "count", 0}, {"dummy", "dummy", 4}}},
		{"fuse_forget_one", BatchForgetEntryIn{}, 16, []field{{"Inode", "nodeid", 0}, {"Nlookup", "nlookup", 8}}},
		{"fuse_getattr_in", GetattrIn{}, 16, []field{{"GetattrFlags", "getattr_flags", 0}, {"dummy", "dummy", 4}, {"Fh", "fh", 8}}},
		{"fuse_mknod_in", MknodIn{}, 16, []field{{"Mode", "mode", 0}, {"Rdev", "rdev", 4}, {"Umask", "umask", 8}, {"padding", "padding", 12}}},
		{"fuse_mkdir_in", MkdirIn{}, 8, []field{{"Mode", "mode", 0}, {"Umask", "umask", 4}}},
		{"fuse_rename_in", RenameIn{}, 8, []field{{"Newdir", "newdir", 0}}},
		{"fuse_link_in", LinkIn{}, 8, []field{{"Oldnodeid", "oldnodeid", 0}}},
		{"fuse_open_in", OpenIn{}, 8, []field{{"Flags", "flags", 0}, {"Unused", "open_flags", 4}}},
		{"fuse_create_in", CreateIn{}, 16, []field{{"Flags", "flags", 0}, {"Mode", "mode", 4}, {"Umask", "umask", 8}, {"padding", "open_flags", 12}}},
		{"fuse_release_in", ReleaseIn{}, 24, []field{{"Fh", "fh", 0}, {"Flags", "flags", 8}, {"ReleaseFlags", "release_flags", 12}, {"LockOwner", "lock_owner", 16}}},
		{"fuse_flush_in", FlushIn{}, 24, []field{{"Fh", "fh", 0}, {"FlushFlags", "unused", 8}, {"Padding", "padding", 12}, {"LockOwner", "lock_owner", 16}}},
		{"fuse_read_in", ReadIn{}, 40, []field{{"Fh", "fh", 0}, {"Offset", "offset", 8}, {"Size", "size", 16}, {"ReadFlags", "read_flags", 20}, {"LockOwner", "lock_owner", 24}, {"Flags", "flags", 32}, {"padding", "padding", 36}}},
		{"fuse_write_in", WriteIn{}, 40, []field{{"Fh", "fh", 0}, {"Offset", "offset", 8}, {"Size", "size", 16}, {"WriteFlags", "write_flags", 20}, {"LockOwner", "lock_owner", 24}, {"Flags", "flags", 32}, {"padding", "padding", 36}}},
		{"fuse_write_out", WriteOut{}, 8, []field{{"Size", "size", 0}, {"Padding", "padding", 4}}},
		{"fuse_statfs_out", StatfsOut{}, 80, []field{{"St", "st", 0}}},
		{"fuse_fsync_in", FsyncIn{}, 16, []field{{"Fh", "fh", 0}, {"FsyncFlags", "fsync_flags", 8}, {"Padding", "padding", 12}}},
		{"fuse_getxattr_out", GetxattrOut{}, 8, []field{{"Size", "size", 0}, {"Padding", "padding", 4}}},
		// LISTXATTR carries fuse_getxattr_in. ListxattrIn takes its size and
		// padding; macFUSE's position has no meaning for a list.
		{"fuse_getxattr_in", ListxattrIn{}, 8, []field{{"Size", "size", 0}, {"Padding", "padding", 4}}},
		{"fuse_fallocate_in", FallocateIn{}, 32, []field{{"Fh", "fh", 0}, {"Offset", "offset", 8}, {"Length", "length", 16}, {"Mode", "mode", 24}, {"Padding", "padding", 28}}},
		// Protocol 7.36 added flags2 at 16. The library speaks 7.34 at most.
		{"fuse_init_in", InitIn{}, 16, []field{{"Major", "major", 0}, {"Minor", "minor", 4}, {"MaxReadahead", "max_readahead", 8}, {"Flags", "flags", 12}}},
		// Unused spans flags2 through unused, which the library leaves zero.
		{"fuse_init_out", InitOut{}, 64, []field{{"Major", "major", 0}, {"Minor", "minor", 4}, {"MaxReadahead", "max_readahead", 8}, {"Flags", "flags", 12}, {"MaxBackground", "max_background", 16}, {"CongestionThreshold", "congestion_threshold", 18}, {"MaxWrite", "max_write", 20}, {"TimeGran", "time_gran", 24}, {"MaxPages", "max_pages", 28}, {"MapAlignment", "map_alignment", 30}, {"Unused", "flags2", 32}}},
		{"fuse_interrupt_in", InterruptIn{}, 8, []field{{"Unique", "unique", 0}}},
		// Padding spans total_extlen and padding.
		{"fuse_in_header", InHeader{}, 40, []field{{"Len", "len", 0}, {"Opcode", "opcode", 4}, {"Unique", "unique", 8}, {"Nodeid", "nodeid", 16}, {"Uid", "uid", 24}, {"Gid", "gid", 28}, {"Pid", "pid", 32}, {"Padding", "total_extlen", 36}}},
		{"fuse_out_header", OutHeader{}, 16, []field{{"Len", "len", 0}, {"Error", "error", 4}, {"Unique", "unique", 8}}},
		{"fuse_dirent", Dirent{}, DirentSize, []field{{"Ino", "ino", 0}, {"Off", "off", 8}, {"Namelen", "namelen", 16}, {"Type", "type", 20}, {"Name", "name", 24}}},
		{"fuse_notify_inval_inode_out", NotifyInvalInodeOut{}, 24, []field{{"Ino", "ino", 0}, {"Off", "off", 8}, {"Len", "len", 16}}},
		{"fuse_notify_inval_entry_out", NotifyInvalEntryOut{}, 16, []field{{"Parent", "parent", 0}, {"Namelen", "namelen", 8}, {"padding", "flags", 12}}},
		{"fuse_syncfs_in", SyncFSIn{}, 8, []field{{"Padding", "padding", 0}}},
	}
}

func TestWireStructsMatchTheKernel(t *testing.T) {
	if DirentSize != 24 {
		t.Errorf("DirentSize = %d, want sizeof(struct fuse_dirent), 24", DirentSize)
	}
	for _, l := range append(commonLayouts(), osLayouts()...) {
		typ := reflect.TypeOf(l.v)
		t.Run(typ.Name(), func(t *testing.T) {
			got := flatten(typ, 0)
			if len(got) != len(l.fields) {
				t.Fatalf("%s has %d fields, want %d to hold %s", typ.Name(), len(got), len(l.fields), l.cName)
			}
			for i, want := range l.fields {
				f := got[i]
				if f.Name != want.goName || f.Offset != want.offset {
					t.Errorf("field %d is %s at %d, want %s at %d, %s's %s", i, f.Name, f.Offset, want.goName, want.offset, l.cName, want.cName)
				}
				// Each field ends where the next C field, or the struct, begins.
				end := l.size
				if i+1 < len(l.fields) {
					end = l.fields[i+1].offset
				}
				if f.Offset+f.Type.Size() != end {
					t.Errorf("%s.%s ends at %d, want %d", typ.Name(), f.Name, f.Offset+f.Type.Size(), end)
				}
			}
			// A trailing zero-size field, such as Dirent.Name, pads the Go
			// size, so the code uses a constant such as DirentSize instead.
			if last := got[len(got)-1]; last.Type.Size() != 0 && typ.Size() != l.size {
				t.Errorf("sizeof %s = %d, want %d, as %s", typ.Name(), typ.Size(), l.size, l.cName)
			}
		})
	}
}

// flatten returns typ's fields, with each embedded struct's fields in its
// place, at their offsets from base.
func flatten(typ reflect.Type, base uintptr) []reflect.StructField {
	var fields []reflect.StructField
	for i := range typ.NumField() {
		f := typ.Field(i)
		f.Offset += base
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			fields = append(fields, flatten(f.Type, f.Offset)...)
			continue
		}
		fields = append(fields, f)
	}
	return fields
}
