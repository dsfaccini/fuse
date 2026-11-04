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
	"errors"
	"math"
	"slices"
	"syscall"

	"github.com/dsfaccini/fuse/fuseops"
	"github.com/dsfaccini/fuse/internal/buffer"
	"github.com/dsfaccini/fuse/internal/fusekernel"
)

// The poll hack, ported from go-fuse
// (https://github.com/hanwen/go-fuse/blob/d54f65d20662f285443aed4b7020056f842f8bb0/fuse/poll.go).
//
// On Linux, os.Open registers every file with the runtime's epoll. For a FUSE
// file, epoll_ctl sends FUSE_POLL and waits for the answer on a thread that
// keeps its P. A server in the same process may then never get a P to answer,
// and the process deadlocks (https://github.com/jacobsa/fuse/issues/97). Once
// a server answers one FUSE_POLL with ENOSYS, the kernel stops sending them.
//
// So Mount opens and polls a reserved entry before it returns, and the
// connection answers the entry's ops itself, including that first FUSE_POLL.

// pollHackName names the reserved entry in the root directory. The connection
// answers a lookup of it only while Mount runs the hack.
const pollHackName = ".fuse-poll-hack"

// pollHackInode is the reserved entry's inode ID.
const pollHackInode = fuseops.InodeID(math.MaxUint64)

// answerPollHack answers op inline and recycles its messages when op belongs to
// the poll hack's entry, and reports whether it did. It drops the entry from a
// BATCH_FORGET that also forgets other inodes, and leaves the rest to the
// server.
//
// Only ReadOp calls it.
func (c *Connection) answerPollHack(
	inMsg *buffer.InMessage,
	outMsg *buffer.OutMessage,
	op interface{}) bool {
	if lookUp, ok := op.(*fuseops.LookUpInodeOp); ok &&
		lookUp.Parent == fuseops.RootInodeID &&
		lookUp.Name == pollHackName &&
		c.pollHackRunning.Load() {
		c.pollHackEntryKnown = true
	} else if !c.pollHackEntryKnown {
		return false
	} else if forget, ok := op.(*fuseops.BatchForgetOp); ok {
		forget.Entries = slices.DeleteFunc(forget.Entries, func(e fuseops.BatchForgetEntry) bool {
			return e.Inode == pollHackInode
		})
		if len(forget.Entries) > 0 {
			return false
		}
	} else if fuseops.InodeID(inMsg.Header().Nodeid) != pollHackInode {
		return false
	}

	defer func() {
		c.putInMessage(inMsg)
		c.putOutMessage(outMsg)
	}()

	unique := inMsg.Header().Unique
	opErr := pollHackReply(op)
	if c.kernelResponse(outMsg, unique, op, opErr) {
		return true
	}
	if c.debugLogger != nil {
		c.debugLog(unique, 1, "-> poll hack: %v", opErr)
	}
	err := c.writeOutMessage(outMsg)
	// See Reply for why ENOENT is benign.
	if err != nil && !errors.Is(err, syscall.ENOENT) && c.errorLogger != nil {
		c.errorLogger.Printf("writeMessage: %v %v", err, outMsg.OutHeaderBytes())
	}
	return true
}

// pollHackReply fills in op's response for the poll hack's entry, a reserved
// empty file, and returns the error to answer with. It answers FUSE_POLL with
// ENOSYS and other unexpected ops with EIO, because ENOSYS would switch the op
// off for the whole file system.
func pollHackReply(op interface{}) error {
	attrs := fuseops.InodeAttributes{Nlink: 1, Mode: 0644}
	switch o := op.(type) {
	case *fuseops.LookUpInodeOp:
		o.Entry = fuseops.ChildInodeEntry{Child: pollHackInode, Attributes: attrs}
	case *fuseops.GetInodeAttributesOp:
		o.Attributes = attrs
	case *fuseops.SetInodeAttributesOp:
		o.Attributes = attrs
	case *fuseops.OpenFileOp, *fuseops.FlushFileOp, *fuseops.ReleaseFileHandleOp:
	case *fuseops.GetXattrOp:
		return syscall.ENODATA
	case *unknownOp:
		if o.OpCode == fusekernel.OpPoll {
			return syscall.ENOSYS
		}
		return syscall.EIO
	default:
		return syscall.EIO
	}
	return nil
}
