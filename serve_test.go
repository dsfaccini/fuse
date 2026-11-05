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

package fuse_test

import (
	"errors"
	"testing"

	"github.com/dsfaccini/fuse"
	"github.com/dsfaccini/fuse/fuseutil"
)

func TestJoinReturnsConnectionFatalError(t *testing.T) {
	transportErr := errors.New("transport broke")
	tests := []struct {
		name    string
		readErr error
		want    error
	}{
		{"hang-up", nil, nil},
		{"transport error", transportErr, transportErr},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := fuseutil.NewFileSystemServer(&fuseutil.NotImplementedFileSystem{})
			err := fuse.ServeCanned(server, tt.readErr)
			if (err == nil) != (tt.want == nil) || !errors.Is(err, tt.want) {
				t.Errorf("Join = %v, want %v", err, tt.want)
			}
		})
	}
}
