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

import (
	"errors"
	"fmt"
)

// ErrFSKitUnavailable means the FSKit backend cannot serve any mount on this
// machine until the user acts: installs macFUSE 5.5 or later, upgrades macOS,
// or enables macFUSE's file system extension. Test for it with errors.Is.
//
// Mount with BackendFSKit and FSKitAvailable return an error that matches it
// when the installed macFUSE is older than 5.5, or when its MFMount framework,
// or a function this package needs from it, cannot be loaded. An
// FSKitMountError with result 1 through 4 matches it too.
var ErrFSKitUnavailable = errors.New("macFUSE FSKit backend is unavailable; install macFUSE 5.5 or later")

// FSKitMountError results. They mirror MFMountResult in macFUSE's MFMount
// framework.
const (
	FSKitMountResultUnsupportedOSVersion                int32 = 1
	FSKitMountResultHelperToolsInstallationFailed       int32 = 2
	FSKitMountResultFileSystemExtensionNotFound         int32 = 3
	FSKitMountResultFileSystemExtensionRequiresApproval int32 = 4
	FSKitMountResultCreatingMountPointFailed            int32 = 5
	FSKitMountResultUnexpectedFailure                   int32 = -1
)

// FSKitMountError is returned by Mount with BackendFSKit when macFUSE rejects
// the mount, or when the running macOS is older than the macFUSE FSKit module
// the mount needs. Result holds one of the FSKitMountResult codes. Test for it
// with errors.As.
type FSKitMountError struct {
	Result int32
}

// Is reports whether e matches ErrFSKitUnavailable. Results 1 through 4 do:
// MFMount returns them before it mounts anything, and they hold for every
// mount until the user acts (https://github.com/macfuse/mount/blob/395aa1b3bb90e907c10d1cae03dcee314a2d2a8a/Mount/Bridge/MFMount.swift).
func (e FSKitMountError) Is(target error) bool {
	return target == ErrFSKitUnavailable &&
		e.Result >= FSKitMountResultUnsupportedOSVersion &&
		e.Result <= FSKitMountResultFileSystemExtensionRequiresApproval
}

func (e FSKitMountError) Error() string {
	switch e.Result {
	case FSKitMountResultUnsupportedOSVersion:
		return "FSKit mount: this macOS version is unsupported; macFUSE's FSKit backend requires macOS 26 or later, or macOS 15.4 or later with the local mount option"
	case FSKitMountResultHelperToolsInstallationFailed:
		return "FSKit mount: macFUSE could not install its helper tool; mount from the logged-in user's session and enter an administrator name and password when prompted"
	case FSKitMountResultFileSystemExtensionNotFound:
		return `FSKit mount: the macFUSE file system extension was not found; if FSKit mounts worked before, macOS may have switched the extension off after a series of mounts (https://github.com/macfuse/macfuse/issues/1232): turn "macFUSE" off and on again under System Settings > General > Login Items & Extensions > File System Extensions. Otherwise run as the logged-in user, not root, and if that does not help, restart your Mac or reinstall macFUSE`
	case FSKitMountResultFileSystemExtensionRequiresApproval:
		return `FSKit mount: the macFUSE file system extension is not enabled; enable "macFUSE" and "macFUSE (local)" under System Settings > General > Login Items & Extensions > File System Extensions, then mount again`
	case FSKitMountResultCreatingMountPointFailed:
		return "FSKit mount: the mount point could not be created; make sure the enclosing folder exists and that you have permission to create folders there"
	case FSKitMountResultUnexpectedFailure:
		return "FSKit mount: macFUSE failed unexpectedly; make sure you are using the latest macFUSE, and reinstall it if the failure persists"
	default:
		return fmt.Sprintf("FSKit mount: macFUSE failed with result %d", e.Result)
	}
}
