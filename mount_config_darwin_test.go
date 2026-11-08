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
	"slices"
	"strings"
	"testing"
	"time"
)

// The kernel extension's helper gets noappledouble unless EnableAppleDouble
// is set.
func TestKernelExtensionMountPassesNoAppleDoubleUnlessEnabled(t *testing.T) {
	argv := useFakeMacFUSEMountHelper(t)
	// The helper mounts nothing and passes back /dev/null, so INIT fails.
	t.Setenv(fakeMountHelper, "0,0")
	for _, test := range []struct {
		cfg  MountConfig
		want bool
	}{
		{cfg: MountConfig{}, want: true},
		{cfg: MountConfig{EnableAppleDouble: true}, want: false},
	} {
		recordUnmounts(t, nil)
		dir := t.TempDir()
		returned := make(chan error, 1)
		go func() {
			_, err := Mount(dir, drainServer{}, &test.cfg)
			returned <- err
		}()
		select {
		case err := <-returned:
			if err == nil || !strings.HasPrefix(err.Error(), "newConnection: ") {
				t.Errorf("EnableAppleDouble=%v: Mount error = %v, want a newConnection error", test.cfg.EnableAppleDouble, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Mount did not return within 10s")
		}
		args := argv()
		if len(args) == 0 || args[len(args)-1] != dir {
			t.Errorf("EnableAppleDouble=%v: helper arguments %q do not end with the mount point %s", test.cfg.EnableAppleDouble, args, dir)
		}
		var options []string
		for i, arg := range args {
			if arg == "-o" && i+1 < len(args) {
				options = append(options, strings.Split(args[i+1], ",")...)
			}
		}
		if got := slices.Contains(options, "noappledouble"); got != test.want {
			t.Errorf("EnableAppleDouble=%v: helper options %q include noappledouble = %v, want %v", test.cfg.EnableAppleDouble, options, got, test.want)
		}
	}
}
