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
	"fmt"
	"testing"

	"github.com/dsfaccini/fuse/fuseops"
	"github.com/dsfaccini/fuse/internal/fusekernel"
)

// benchmarkRoundTrip reads request from a canned transport and replies to it
// b.N times. answer fills in the op's response.
func benchmarkRoundTrip(b *testing.B, request []byte, answer func(op interface{})) {
	c := newTestConnection(cannedTransport{message: request})
	b.ReportAllocs()
	for b.Loop() {
		ctx, op, err := c.ReadOp()
		if err != nil {
			b.Fatalf("ReadOp: %v", err)
		}
		answer(op)
		if err := c.Reply(ctx, nil); err != nil {
			b.Fatalf("Reply: %v", err)
		}
	}
}

func BenchmarkRoundTripLookUp(b *testing.B) {
	benchmarkRoundTrip(b, testMessage(fusekernel.OpLookup, 5, []byte("name\x00")), func(op interface{}) {
		op.(*fuseops.LookUpInodeOp).Entry.Child = 2
	})
}

func BenchmarkRoundTripGetattr(b *testing.B) {
	benchmarkRoundTrip(b, testMessage(fusekernel.OpGetattr, 5), func(op interface{}) {
		op.(*fuseops.GetInodeAttributesOp).Attributes.Nlink = 1
	})
}

func BenchmarkRoundTripReadDir(b *testing.B) {
	for _, size := range []uint32{4 << 10, 64 << 10} {
		in := fusekernel.ReadIn{Size: size}
		b.Run(fmt.Sprintf("%dKiB", size>>10), func(b *testing.B) {
			benchmarkRoundTrip(b, testMessage(fusekernel.OpReaddir, 5, structBytes(&in)), func(op interface{}) {
				o := op.(*fuseops.ReadDirOp)
				o.BytesRead = len(o.Dst)
			})
		})
	}
}
