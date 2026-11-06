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
	"testing"

	"github.com/dsfaccini/fuse/internal/mounttest"
)

// fskitProbeMounts is the most FSKit mounts one run of TestFSKitProbe makes.
// The probe checks its plan against it before the first mount.
const fskitProbeMounts = 5

// TestFSKitProbe measures how macFUSE's FSKit backend treats the FUSE
// protocol. It serves an in-memory file system through this package's
// Connection and makes at most 5 FSKit mounts, one after another. Its
// transport does not report that FSKit pads short reads, so the Connection's
// EIO reply to a short READ stays off and the macfuse#1231 cases measure
// FSKit, not that guard:
//
//  1. base, artifact-fs's mount config. Access to the mount point, entry and
//     attr TTLs, negative entries, whether LOOKUP attributes replace a cached
//     vnode's, whether FUSE_INTERRUPT is sent, macfuse#1231 on the first read
//     of an uncached file and on a second read after the file's data shrank
//     under an unchanged size, the READ padding timing with a shrink and with
//     a growth, opening a mode 0 file, write sizes, and Join after a clean
//     unmount. macOS can keep an entry past its TTL
//     (https://github.com/macfuse/macfuse/issues/1207), so the LOOKUP
//     attributes case may report unknown. The padding timing reports which
//     size a reader gets when a LOOKUP reply that changes the file's size
//     reaches FSKit while a READ it sent earlier is unanswered: the size when
//     FSKit sent the READ, or the new size. The shrink run decides the open
//     short-read guard in https://github.com/dsfaccini/fuse/pull/5.
//  2. local, base plus the local option. Access to the mount point, and Join
//     after a clean unmount.
//  3. options, base with DisableDefaultPermissions, MaxWrite 128 KiB and the
//     mount option max_write=262144. Opening a mode 0 file and write sizes,
//     then what notify messages do.
//  4. close, base. What MFChannelClose does to a live mount.
//  5. timeout, base plus daemon_timeout=3. What a READ slower than the timeout
//     does, first with a signal to the reader and answered before the channel
//     would close, then without a signal and held until the channel should
//     close.
//
// Every mount also reports whether the volume is attached when MFMount
// returns, its mount flags, the INIT request, and the reply buffers FSKit
// attaches to READ requests. After the last mount, the probe compares mounts
// for default_permissions, max_write and the local option. It needs separate
// mounts because each option is fixed at mount time, and because notify,
// MFChannelClose and daemon_timeout can each end a mount.
//
// Each result is one log line: "FSKIT_PROBE " and a JSON object with item,
// mount, result and details. A result the run does not settle is "unknown"
// and does not fail the test. Only the two macfuse#1231 cases fail on a
// measured result: each passes while the read of a short READ reply returns
// the reported size, and fails once the read returns only the reply or fails
// with EIO. A failed mount, measurements that stop early, a server panic and a
// mount left behind also fail the test, and so does a second run in one
// process, such as -count=2.
//
// The probe runs only on David's go-ahead in his own words, because macFUSE's
// FSKit extension switches itself off after a series of mounts
// (https://github.com/macfuse/macfuse/issues/1232) and a stalled FSKit mount
// clears only on reboot. AGENTS.md has the command. The probe refuses to start
// while a macFUSE volume or a probe mount point is mounted. It starts no
// further mount after a failed mount, after one that looked wedged, or while
// an earlier one is still mounted. A mount looks wedged when its measurements
// hit their time limit, a call into its volume or channel hits its bound, a
// reader survives SIGKILL, Join stays pending after the volume left the mount
// table, MFMount never returns, or it needs a forced unmount. Each of these
// fails the test. A run can therefore make fewer than 5 mounts.
//
// No mount outlives the probe. Each mount point is a fresh directory, and its
// cleanup is registered before MFMount is called, so it runs after a failure, a
// t.Fatal or a panic in the test goroutine. Cleanup unmounts with unmount(2),
// then diskutil unmount, then the forced forms of both, and fails the test
// with the recovery command if the volume stays mounted. Every wait on the
// volume is bounded. SIGINT, SIGTERM, SIGHUP and the approach of the go test
// deadline stop the measurements early so that cleanup still runs. SIGPIPE is
// ignored, so a closed output pipe cannot end the run. After the last mount,
// the probe checks the mount table again for each of its mount points.
func TestFSKitProbe(t *testing.T) {
	mounttest.SkipUnlessFSKitProbeEnabled(t)
	runFSKitProbe(t)
}
