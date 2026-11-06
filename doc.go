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

// Package fuse enables writing and mounting user-space file systems.
//
// The primary elements of interest are:
//
//   - The fuseops package, which defines the operations that fuse might send
//     to your userspace daemon.
//
//   - The Server interface, which your daemon must implement.
//
//   - fuseutil.NewFileSystemServer, which offers a convenient way to implement
//     the Server interface.
//
//   - Mount and MountContext, which mount a Server as a file system, and
//     MountedFileSystem.Unmount, which unmounts it through its Backend.
//
// Make sure to see the examples in the sub-packages of samples/, which double
// as tests for this package: https://pkg.go.dev/github.com/dsfaccini/fuse/samples
//
// MountConfig.Backend picks how the kernel reaches the file system. On Linux
// that is the kernel's FUSE module. On macOS it is macFUSE
// (https://macfuse.github.io/), through its kernel extension by default, or
// through its FSKit backend. See FSKitAvailable for what the FSKit backend
// needs. Do note that there are several macOS-specific oddities; grep through
// the documentation for more info.
package fuse
