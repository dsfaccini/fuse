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
	"strings"
	"testing"
)

func TestFSKitMountErrorTellsUserWhatToDo(t *testing.T) {
	for _, test := range []struct {
		result int32
		want   string
	}{
		{result: FSKitMountResultUnsupportedOSVersion, want: "requires macOS 26 or later, or macOS 15.4 or later with the local mount option"},
		{result: FSKitMountResultHelperToolsInstallationFailed, want: "enter an administrator name and password"},
		{result: FSKitMountResultFileSystemExtensionNotFound, want: `turn "macFUSE" off and on again under System Settings > General > Login Items & Extensions > File System Extensions`},
		{result: FSKitMountResultFileSystemExtensionNotFound, want: "run as the logged-in user, not root"},
		{result: FSKitMountResultFileSystemExtensionRequiresApproval, want: `enable "macFUSE" and "macFUSE (local)" under System Settings > General > Login Items & Extensions > File System Extensions`},
		{result: FSKitMountResultCreatingMountPointFailed, want: "make sure the enclosing folder exists"},
		{result: FSKitMountResultUnexpectedFailure, want: "reinstall it"},
		{result: 42, want: "result 42"},
	} {
		if got := (FSKitMountError{Result: test.result}).Error(); !strings.Contains(got, test.want) {
			t.Errorf("FSKitMountError{Result: %d}.Error() = %q, want it to contain %q", test.result, got, test.want)
		}
	}
}

// Results 1 through 4 hold for every mount until the user acts, so a caller
// can stop retrying.
func TestFSKitMountErrorMatchesErrFSKitUnavailable(t *testing.T) {
	for _, test := range []struct {
		result int32
		want   bool
	}{
		{result: FSKitMountResultUnexpectedFailure},
		{result: 0},
		{result: FSKitMountResultUnsupportedOSVersion, want: true},
		{result: FSKitMountResultHelperToolsInstallationFailed, want: true},
		{result: FSKitMountResultFileSystemExtensionNotFound, want: true},
		{result: FSKitMountResultFileSystemExtensionRequiresApproval, want: true},
		{result: FSKitMountResultCreatingMountPointFailed},
		{result: 42},
	} {
		err := fmt.Errorf("mount (background): %w", FSKitMountError{Result: test.result})
		if got := errors.Is(err, ErrFSKitUnavailable); got != test.want {
			t.Errorf("errors.Is(FSKitMountError{Result: %d}, ErrFSKitUnavailable) = %v, want %v", test.result, got, test.want)
		}
	}
	if errors.Is(FSKitMountError{Result: FSKitMountResultUnsupportedOSVersion}, FSKitMountError{Result: FSKitMountResultFileSystemExtensionNotFound}) {
		t.Error("FSKitMountError results 1 and 3 match each other")
	}
}
