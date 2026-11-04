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

package buffer

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/dsfaccini/fuse/internal/fusekernel"
)

func TestInMessageReadSingleRejectsBadLength(t *testing.T) {
	oldFuseT := fusekernel.IsPlatformFuseT
	fusekernel.IsPlatformFuseT = true
	t.Cleanup(func() { fusekernel.IsPlatformFuseT = oldFuseT })

	for _, length := range []uint32{2, 1 << 30} {
		message := make([]byte, 64)
		binary.NativeEndian.PutUint32(message, length)
		if err := NewInMessage(len(message)).Init(bytes.NewReader(message)); err == nil {
			t.Errorf("Init with header length %d succeeded, want an error", length)
		}
	}
}
