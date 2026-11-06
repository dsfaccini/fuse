// Copyright 2015 Google Inc. All Rights Reserved.
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
	"fmt"
	"unsafe"

	"github.com/dsfaccini/fuse/internal/fusekernel"
)

// OutMessageHeaderSize is the size of the leading header in every
// properly-constructed OutMessage. Reset brings the message back to this size.
const OutMessageHeaderSize = int(unsafe.Sizeof(fusekernel.OutHeader{}))

// minScratch is the smallest scratch buffer Grow allocates, so that the
// replies of most ops fit the first one.
const minScratch = 512

// OutMessage provides a mechanism for constructing a single contiguous fuse
// message from multiple segments, where the first segment is always a
// fusekernel.OutHeader message.
//
// Must be initialized with Reset. A zero OutMessage keeps no scratch buffer;
// use NewOutMessage for one that does.
type OutMessage struct {
	header fusekernel.OutHeader
	Sglist [][]byte

	// headerOnly backs Parts for a message that is only its header.
	headerOnly [1][]byte

	// sglist backs Sglist until the message has more segments than it holds.
	sglist [4][]byte

	// Grow carves segments from scratch[used:]. scratch survives Reset and
	// never grows past scratchCap bytes.
	scratch    []byte
	used       int
	scratchCap int
}

// NewOutMessage returns a reset message whose Grow reuses up to scratchCap
// bytes of storage across Reset.
func NewOutMessage(scratchCap int) *OutMessage {
	m := &OutMessage{scratchCap: scratchCap}
	m.Reset()
	return m
}

// Reset resets m so that it's ready to be used again. Afterward, the contents
// are solely a zeroed fusekernel.OutHeader struct. Segments that Grow returned
// before Reset may be returned again after it.
func (m *OutMessage) Reset() {
	m.header = fusekernel.OutHeader{}
	m.Sglist = nil
	m.used = 0
	// Drop the references to appended slices, which belong to the caller.
	clear(m.sglist[:])
}

// OutHeader returns a pointer to the header at the start of the message.
func (m *OutMessage) OutHeader() *fusekernel.OutHeader {
	return &m.header
}

// Grow adds a new buffer of <n> bytes to the message, returning a pointer to
// the start of the new segment, which is guaranteed to be zeroed. Grow(0)
// adds nothing and returns nil.
func (m *OutMessage) Grow(n int) unsafe.Pointer {
	if n == 0 {
		return nil
	}
	if n > len(m.scratch)-m.used && n <= m.scratchCap {
		// Earlier segments keep the old scratch alive until Reset.
		m.scratch = make([]byte, min(max(n, 2*len(m.scratch), minScratch), m.scratchCap))
		m.used = 0
	}
	var b []byte
	if n <= len(m.scratch)-m.used {
		b = m.scratch[m.used : m.used+n : m.used+n]
		clear(b)
		m.used += n
	} else {
		b = make([]byte, n)
	}
	m.Append(b)
	return unsafe.Pointer(&b[0])
}

// ShrinkTo shrinks m to the given size. It panics if the size is greater than
// Len() or less than OutMessageHeaderSize.
func (m *OutMessage) ShrinkTo(n int) {
	if n < OutMessageHeaderSize || n > m.Len() {
		panic(fmt.Sprintf(
			"ShrinkTo(%d) out of range (current Len: %d)",
			n,
			m.Len()))
	}
	if n == OutMessageHeaderSize {
		m.Sglist = nil
	} else {
		i := 1
		n -= OutMessageHeaderSize
		for len(m.Sglist) > i && n >= len(m.Sglist[i]) {
			n -= len(m.Sglist[i])
			i++
		}
		if n > 0 {
			m.Sglist[i] = m.Sglist[i][0:n]
			i++
		}
		m.Sglist = m.Sglist[0:i]
	}
}

// Append is equivalent to growing by len(src), then copying src over the new
// segment. Int panics if there is not enough room available.
func (m *OutMessage) Append(src ...[]byte) {
	if m.Sglist == nil {
		// First element of Sglist is pre-filled with a pointer to the header
		// to allow sending it with a single writev() call without copying the
		// slice again
		m.Sglist = append(m.sglist[:0], m.OutHeaderBytes())
	}
	m.Sglist = append(m.Sglist, src...)
	return
}

// AppendString is like Append, but accepts string input.
func (m *OutMessage) AppendString(src string) {
	m.Append([]byte(src))
	return
}

// Len returns the current size of the message, including the leading header.
func (m *OutMessage) Len() int {
	if m.Sglist == nil {
		return OutMessageHeaderSize
	}
	// First element of Sglist is the header, so we don't need to count it here
	r := 0
	for _, b := range m.Sglist {
		r += len(b)
	}
	return r
}

// OutHeaderBytes returns a byte slice containing the current header.
func (m *OutMessage) OutHeaderBytes() []byte {
	return unsafe.Slice((*byte)(unsafe.Pointer(&m.header)), OutMessageHeaderSize)
}

// Parts returns the message's segments, header first, without allocating.
func (m *OutMessage) Parts() [][]byte {
	if m.Sglist != nil {
		return m.Sglist
	}
	m.headerOnly[0] = m.OutHeaderBytes()
	return m.headerOnly[:]
}
