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

package fuse

import (
	"unsafe"

	"github.com/dsfaccini/fuse/internal/buffer"
)

// maxFreeMessages caps each freelist. A burst of concurrent ops allocates a
// message pair per op, and the freelists keep at most this many pairs once the
// burst ends. An in-message holds a page plus MaxWrite bytes, and an
// out-message up to MaxPages pages of scratch.
const maxFreeMessages = 16

////////////////////////////////////////////////////////////////////////
// buffer.InMessage
////////////////////////////////////////////////////////////////////////

// LOCKS_EXCLUDED(c.mu)
func (c *Connection) getInMessage() *buffer.InMessage {
	c.mu.Lock()
	x := (*buffer.InMessage)(c.inMessages.Get())
	c.mu.Unlock()

	if x == nil {
		x = buffer.NewInMessage(c.inMessageBufSize)
	}

	return x
}

// LOCKS_EXCLUDED(c.mu)
func (c *Connection) putInMessage(x *buffer.InMessage) {
	c.mu.Lock()
	c.inMessages.Put(unsafe.Pointer(x), maxFreeMessages)
	c.mu.Unlock()
}

////////////////////////////////////////////////////////////////////////
// buffer.OutMessage
////////////////////////////////////////////////////////////////////////

// LOCKS_EXCLUDED(c.mu)
func (c *Connection) getOutMessage() *buffer.OutMessage {
	c.mu.Lock()
	x := (*buffer.OutMessage)(c.outMessages.Get())
	c.mu.Unlock()

	if x == nil {
		x = buffer.NewOutMessage(int(maxReplySize(&c.cfg)))
	}

	return x
}

// LOCKS_EXCLUDED(c.mu)
func (c *Connection) putOutMessage(x *buffer.OutMessage) {
	x.Reset()
	c.mu.Lock()
	c.outMessages.Put(unsafe.Pointer(x), maxFreeMessages)
	c.mu.Unlock()
}
