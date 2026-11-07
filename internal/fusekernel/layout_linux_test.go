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

// osLayouts are the structs that Linux lays out its own way.
func osLayouts() []layout {
	return []layout{
		// The library sends flags as 0.
		{"fuse_attr", Attr{}, 88, []field{{"Ino", "ino", 0}, {"Size", "size", 8}, {"Blocks", "blocks", 16}, {"Atime", "atime", 24}, {"Mtime", "mtime", 32}, {"Ctime", "ctime", 40}, {"AtimeNsec", "atimensec", 48}, {"MtimeNsec", "mtimensec", 52}, {"CtimeNsec", "ctimensec", 56}, {"Mode", "mode", 60}, {"Nlink", "nlink", 64}, {"Uid", "uid", 68}, {"Gid", "gid", 72}, {"Rdev", "rdev", 76}, {"Blksize", "blksize", 80}, {"padding", "flags", 84}}},
		{"fuse_entry_out", EntryOut{}, 128, []field{{"Nodeid", "nodeid", 0}, {"Generation", "generation", 8}, {"EntryValid", "entry_valid", 16}, {"AttrValid", "attr_valid", 24}, {"EntryValidNsec", "entry_valid_nsec", 32}, {"AttrValidNsec", "attr_valid_nsec", 36}, {"Attr", "attr", 40}}},
		{"fuse_attr_out", AttrOut{}, 104, []field{{"AttrValid", "attr_valid", 0}, {"AttrValidNsec", "attr_valid_nsec", 8}, {"Dummy", "dummy", 12}, {"Attr", "attr", 16}}},
		{"fuse_setattr_in", SetattrIn{}, 88, []field{{"Valid", "valid", 0}, {"Padding", "padding", 4}, {"Fh", "fh", 8}, {"Size", "size", 16}, {"LockOwner", "lock_owner", 24}, {"Atime", "atime", 32}, {"Mtime", "mtime", 40}, {"Unused2", "ctime", 48}, {"AtimeNsec", "atimensec", 56}, {"MtimeNsec", "mtimensec", 60}, {"Unused3", "ctimensec", 64}, {"Mode", "mode", 68}, {"Unused4", "unused4", 72}, {"Uid", "uid", 76}, {"Gid", "gid", 80}, {"Unused5", "unused5", 84}}},
		// Protocol 7.47 added backing_id_64 at 16. The library speaks 7.34 at
		// most.
		{"fuse_open_out", OpenOut{}, 16, []field{{"Fh", "fh", 0}, {"OpenFlags", "open_flags", 8}, {"Padding", "backing_id", 12}}},
		{"fuse_getxattr_in", GetxattrIn{}, 8, []field{{"Size", "size", 0}, {"Padding", "padding", 4}}},
		// FUSE_COMPAT_SETXATTR_IN_SIZE. The kernel sends setxattr_flags, at 8,
		// only after INIT enables FUSE_SETXATTR_EXT, which the library does
		// not.
		{"fuse_setxattr_in", SetxattrIn{}, 8, []field{{"Size", "size", 0}, {"Flags", "flags", 4}}},
	}
}
