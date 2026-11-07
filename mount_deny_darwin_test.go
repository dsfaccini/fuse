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
	"os"
	"testing"

	"github.com/dsfaccini/fuse/internal/mounttest"
)

// deniedMountDir cannot hold a file: /var/empty is empty, and only root can
// write to it.
const deniedMountDir = "/var/empty/fuse-tests-deny-mounts"

// installedMFMount is where macFUSE installs MFMount. useInstalledMFMount
// lets a test load it.
var installedMFMount = fskitFrameworkPath

// init keeps every test in this binary, package fuse_test included, from
// reaching macFUSE's real mount helper, and its real MFMount unless the FSKit
// probe's gate is set. The mounting tests run only on Linux, so no gate lets a
// test reach the mount helper. A regression in backend selection then fails
// instead of mounting.
func init() {
	if os.Getenv(mounttest.FSKitProbeEnv) != "1" {
		fskitFrameworkPath = deniedMountDir + "/MFMount"
	}
	macfuseMountHelper = deniedMountDir + "/mount_macfuse"
}

func TestMountsAreDeniedByDefault(t *testing.T) {
	if _, err := os.Stat(macfuseMountHelper); !os.IsNotExist(err) {
		t.Errorf("tests can reach %s: stat error = %v, want it missing", macfuseMountHelper, err)
	}
	if os.Getenv(mounttest.FSKitProbeEnv) == "1" {
		return
	}
	if _, err := os.Stat(fskitFrameworkPath); !os.IsNotExist(err) {
		t.Errorf("without %s=1, tests can reach %s: stat error = %v, want it missing", mounttest.FSKitProbeEnv, fskitFrameworkPath, err)
	}
}

// useInstalledMFMount lets the test load the installed MFMount until it ends,
// with a fresh loader cache. It skips the test unless macFUSE 5.5 or later is
// installed. CI installs it, so there a missing or older macFUSE fails the
// test.
func useInstalledMFMount(t *testing.T) {
	t.Helper()
	err := checkMacFUSEVersion(installedMFMount)
	if err == nil {
		_, err = os.Stat(installedMFMount)
	}
	switch {
	case err == nil:
	case os.Getenv("GITHUB_ACTIONS") == "true":
		t.Fatalf("macFUSE 5.5 or later not installed: %v", err)
	default:
		t.Skipf("macFUSE 5.5 or later not installed: %v", err)
	}
	oldPath, oldLoader := fskitFrameworkPath, fskitAPILoader
	fskitFrameworkPath, fskitAPILoader = installedMFMount, cacheFSKitAPI(loadFSKitAPI)
	t.Cleanup(func() { fskitFrameworkPath, fskitAPILoader = oldPath, oldLoader })
}
