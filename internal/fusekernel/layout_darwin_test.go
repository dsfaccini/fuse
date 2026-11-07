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

// osLayouts are the structs that macFUSE lays out its own way, under
// __APPLE__.
func osLayouts() []layout {
	return []layout{
		// The library sends flags as 0.
		{"fuse_attr", Attr{}, 104, []field{{"Ino", "ino", 0}, {"Size", "size", 8}, {"Blocks", "blocks", 16}, {"Atime", "atime", 24}, {"Mtime", "mtime", 32}, {"Ctime", "ctime", 40}, {"Crtime_", "btime", 48}, {"AtimeNsec", "atimensec", 56}, {"MtimeNsec", "mtimensec", 60}, {"CtimeNsec", "ctimensec", 64}, {"CrtimeNsec", "btimensec", 68}, {"Mode", "mode", 72}, {"Nlink", "nlink", 76}, {"Uid", "uid", 80}, {"Gid", "gid", 84}, {"Rdev", "rdev", 88}, {"Flags_", "flags_darwin", 92}, {"Blksize", "blksize", 96}, {"padding", "flags", 100}}},
		{"fuse_entry_out", EntryOut{}, 144, []field{{"Nodeid", "nodeid", 0}, {"Generation", "generation", 8}, {"EntryValid", "entry_valid", 16}, {"AttrValid", "attr_valid", 24}, {"EntryValidNsec", "entry_valid_nsec", 32}, {"AttrValidNsec", "attr_valid_nsec", 36}, {"Attr", "attr", 40}}},
		{"fuse_attr_out", AttrOut{}, 120, []field{{"AttrValid", "attr_valid", 0}, {"AttrValidNsec", "attr_valid_nsec", 8}, {"Dummy", "dummy", 12}, {"Attr", "attr", 16}}},
		{"fuse_setattr_in", SetattrIn{}, 128, []field{{"Valid", "valid", 0}, {"Padding", "padding", 4}, {"Fh", "fh", 8}, {"Size", "size", 16}, {"LockOwner", "lock_owner", 24}, {"Atime", "atime", 32}, {"Mtime", "mtime", 40}, {"Unused2", "ctime", 48}, {"AtimeNsec", "atimensec", 56}, {"MtimeNsec", "mtimensec", 60}, {"Unused3", "ctimensec", 64}, {"Mode", "mode", 68}, {"Unused4", "unused4", 72}, {"Uid", "uid", 76}, {"Gid", "gid", 80}, {"Unused5", "unused5", 84}, {"Bkuptime_", "bkuptime", 88}, {"Chgtime_", "ctime_darwin", 96}, {"Crtime", "btime", 104}, {"BkuptimeNsec", "bkuptimensec", 112}, {"ChgtimeNsec", "ctimensec_darwin", 116}, {"CrtimeNsec", "btimensec", 120}, {"Flags_", "flags", 124}}},
		{"fuse_open_out", OpenOut{}, 16, []field{{"Fh", "fh", 0}, {"OpenFlags", "open_flags", 8}, {"Padding", "backing_id", 12}}},
		{"fuse_getxattr_in", GetxattrIn{}, 16, []field{{"Size", "size", 0}, {"Padding", "padding", 4}, {"Position", "position", 8}, {"Padding", "padding2", 12}}},
		{"fuse_setxattr_in", SetxattrIn{}, 16, []field{{"Size", "size", 0}, {"Flags", "flags", 4}, {"Position", "position", 8}, {"Padding", "setxattr_flags", 12}}},
	}
}
