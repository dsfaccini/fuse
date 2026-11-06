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
	"runtime"
	"strings"
	"testing"
)

func TestMountConfigBackend(t *testing.T) {
	darwin := runtime.GOOS == "darwin"
	for _, test := range []struct {
		name    string
		cfg     MountConfig
		want    Backend
		wantErr bool
	}{
		{name: "zero value"},
		{name: "Linux", cfg: MountConfig{Backend: BackendLinux}, want: BackendLinux, wantErr: darwin},
		{name: "kernel extension", cfg: MountConfig{Backend: BackendMacFUSEKernelExtension}, want: BackendMacFUSEKernelExtension, wantErr: !darwin},
		{name: "FSKit", cfg: MountConfig{Backend: BackendFSKit}, want: BackendFSKit, wantErr: !darwin},
		{name: "unknown", cfg: MountConfig{Backend: "kext"}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := test.cfg.normalize()
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "is not available on "+runtime.GOOS) {
					t.Fatalf("normalize error = %v, want the backend refused", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalize: %v", err)
			}
			if cfg.Backend != test.want {
				t.Errorf("Backend = %q, want %q", cfg.Backend, test.want)
			}
		})
	}
}
