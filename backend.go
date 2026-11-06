// Copyright 2026 Google Inc. All Rights Reserved.
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

// Backend is the mechanism through which the kernel reaches a file system.
// Its zero value names none of them.
type Backend string

const (
	// BackendLinux is the Linux kernel's FUSE device, /dev/fuse.
	BackendLinux Backend = "linux"

	// BackendMacFUSEKernelExtension is the macFUSE kernel extension's device.
	BackendMacFUSEKernelExtension Backend = "macfuse-kernel-extension"

	// BackendFSKit is macFUSE's FSKit backend, reached through its MFMount
	// framework.
	BackendFSKit Backend = "fskit"
)
