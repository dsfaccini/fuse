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

// Package mounttest gates tests that mount a real file system, so that plain
// `go test ./...` never mounts.
package mounttest

import (
	"os"
	"runtime"
	"testing"
)

// Env enables tests that mount a real file system when set to "1".
const Env = "FUSE_RUN_MOUNT_TESTS"

// SkipUnlessEnabled skips t unless FUSE_RUN_MOUNT_TESTS=1 on Linux. Every
// test that mounts calls it before anything else. The mounting tests assume
// Linux, so it skips them on every other OS.
//
// A test it lets run logs "FUSE_RUN_MOUNT_TESTS=1: mounting". CI fails each
// mounting job when no test that logged it passed.
func SkipUnlessEnabled(t testing.TB) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skipf("mounts a file system; runs only on Linux")
	}
	if os.Getenv(Env) != "1" {
		t.Skipf("mounts a file system; set %s=1 to run", Env)
	}
	t.Logf("%s=1: mounting", Env)
}

// FSKitProbeEnv enables TestFSKitProbe when set to "1". The probe mounts
// through macFUSE's FSKit backend, so FUSE_RUN_MOUNT_TESTS does not run it.
const FSKitProbeEnv = "FUSE_RUN_FSKIT_PROBE"

// SkipUnlessFSKitProbeEnabled skips t unless FUSE_RUN_FSKIT_PROBE=1. The FSKit
// probe calls it before anything else.
func SkipUnlessFSKitProbeEnabled(t testing.TB) {
	t.Helper()
	if os.Getenv(FSKitProbeEnv) != "1" {
		t.Skipf("mounts through FSKit; set %s=1 to run", FSKitProbeEnv)
	}
}
