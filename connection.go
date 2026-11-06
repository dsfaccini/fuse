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
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"path"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/dsfaccini/fuse/fuseops"
	"github.com/dsfaccini/fuse/internal/buffer"
	"github.com/dsfaccini/fuse/internal/freelist"
	"github.com/dsfaccini/fuse/internal/fusekernel"
	"github.com/dsfaccini/fuse/internal/operror"
)

type contextKeyType uint64

var contextKey interface{} = contextKeyType(0)

// Ask the Linux kernel for larger read requests.
//
// As of 2015-03-26, the behavior in the kernel is:
//
//   - (https://tinyurl.com/2eakn5e9, https://tinyurl.com/mry9e33d) Set the
//     local variable ra_pages to be init_response->max_readahead divided by
//     the page size.
//
//   - (https://tinyurl.com/2eakn5e9, https://tinyurl.com/mbpshk8h) Set
//     backing_dev_info::ra_pages to the min of that value and what was sent in
//     the request's max_readahead field.
//
//   - (https://tinyurl.com/57hpfu4x) Use backing_dev_info::ra_pages when
//     deciding how much to read ahead.
//
//   - (https://tinyurl.com/ywhfcfte) Don't read ahead at all if that field is
//     zero.
//
// Reading a page at a time is a drag. Ask for a larger size.
const maxReadahead = 1 << 20

// Connection represents a connection to the kernel through a mount's backend.
// It is used to receive and reply to requests from the kernel.
type Connection struct {
	cfg         MountConfig
	debugLogger *log.Logger
	errorLogger *log.Logger
	wireLogger  io.Writer

	// Serializes writes to wireLogger, which Reply makes from each op's
	// goroutine.
	wireLoggerMu sync.Mutex

	// The kernel-facing transport and the protocol version used with it.
	transport connectionTransport
	protocol  fusekernel.Protocol

	mu sync.Mutex

	// A map from fuse "unique" request ID (*not* the op ID for logging used
	// above) to a function that cancel's its associated context.
	//
	// GUARDED_BY(mu)
	cancelFuncs map[uint64]func()

	// The first error that is fatal to the connection. Once it is set, ReadOp
	// returns it.
	//
	// GUARDED_BY(mu)
	fatalErr error

	// The size last reported for each inode the kernel holds, kept only when
	// the transport pads short reads. See trackReportedSizes.
	//
	// GUARDED_BY(mu)
	reportedInodes map[fuseops.InodeID]reportedInode

	// Taken only when reportedInodes is kept, and before mu. Each reply that
	// can change a size record holds the write side from recording its growth,
	// through its write, to applying its shrink, so records change in the
	// order the kernel receives the replies. Each ReadFileOp reply holds the
	// read side from its check through its write, so no size reaches the
	// kernel between the two. Forgets change records too, but write nothing,
	// and take no side. See changesReportedSizes.
	//
	// The cost: size-changing replies are serialized through their writes and
	// wait for every READ reply in flight, and once one waits, new READ replies
	// wait behind it.
	sizeReplyMu sync.RWMutex

	// Freelists, serviced by freelists.go.
	inMessages  freelist.Freelist // GUARDED_BY(mu)
	outMessages freelist.Freelist // GUARDED_BY(mu)

	// Size of the buffer to allocate for each InMessage.
	inMessageBufSize int

	// pollHackRunning is true while Mount runs the poll hack. See poll_hack.go.
	pollHackRunning atomic.Bool

	// pollHackEntryKnown is true once the kernel has looked up the poll hack's
	// entry. Only ReadOp touches it.
	pollHackEntryKnown bool
}

// State that is maintained for each in-flight op. This is stuffed into the
// context that the user uses to reply to the op.
type opState struct {
	inMsg  *buffer.InMessage
	outMsg *buffer.OutMessage
	op     interface{}
	wlog   *WireLogRecord

	// The request's header fields, copied because inMsg returns to the freelist
	// when the op is answered.
	fuseID uint64
	opCode uint32

	// Set by the op's first Reply.
	replied *atomic.Bool

	// For a ReadFileOp on a transport that pads short reads, the size recorded
	// for its inode when it arrived.
	sizeAtRead uint64
}

// Return the current wirelog record from the context if the MountConfig
// contained a non-nil wireLogger, nil otherwise.
func GetWirelog(ctx context.Context) *WireLogRecord {
	val := ctx.Value(contextKey)
	state, ok := val.(opState)
	if ok {
		return state.wlog
	}
	return nil
}

// Create a connection wrapping the supplied kernel transport. You must
// eventually call c.close().
//
// cfg must come from MountConfig.normalize. The loggers may be nil.
func newConnection(
	cfg MountConfig,
	debugLogger *log.Logger,
	errorLogger *log.Logger,
	wireLogger io.Writer,
	transport connectionTransport) (*Connection, error) {
	c := &Connection{
		cfg:         cfg,
		debugLogger: debugLogger,
		errorLogger: errorLogger,
		wireLogger:  wireLogger,
		transport:   transport,
		cancelFuncs: make(map[uint64]func()),
	}
	if p, ok := transport.(shortReadPadder); ok && p.padsShortReads() {
		c.reportedInodes = make(map[fuseops.InodeID]reportedInode)
	}

	// Initialize.
	if err := c.Init(); err != nil {
		c.close()
		return nil, fmt.Errorf("Init: %v", err)
	}

	return c, nil
}

// Init performs the work necessary to cause the mount process to complete.
func (c *Connection) Init() error {
	c.inMessageBufSize = buffer.GetPageSize() + int(c.cfg.MaxWrite)

	// Read the init op.
	ctx, op, err := c.ReadOp()
	if err != nil {
		return fmt.Errorf("Reading init op: %v", err)
	}

	initOp, ok := op.(*initOp)
	if !ok {
		c.Reply(ctx, syscall.EPROTO)
		return fmt.Errorf("Expected *initOp, got %T", op)
	}

	// Make sure the protocol version spoken by the kernel is new enough.
	min := fusekernel.Protocol{
		fusekernel.ProtoVersionMinMajor,
		fusekernel.ProtoVersionMinMinor,
	}

	if initOp.Kernel.LT(min) {
		c.Reply(ctx, syscall.EPROTO)
		return fmt.Errorf("Version too old: %v", initOp.Kernel)
	}

	// Downgrade our protocol if necessary.
	c.protocol = fusekernel.Protocol{
		fusekernel.ProtoVersionMaxMajor,
		fusekernel.ProtoVersionMaxMinor,
	}

	if initOp.Kernel.LT(c.protocol) {
		c.protocol = initOp.Kernel
	}

	cacheSymlinks := initOp.Flags&fusekernel.InitCacheSymlinks > 0
	noOpenSupport := initOp.Flags&fusekernel.InitNoOpenSupport > 0
	noOpendirSupport := initOp.Flags&fusekernel.InitNoOpendirSupport > 0
	asyncDIO := initOp.Flags&fusekernel.InitAsyncDIO > 0
	autoInvalData := initOp.Flags&fusekernel.InitAutoInvalData > 0

	// Respond to the init op.
	initOp.Library = c.protocol
	initOp.MaxReadahead = maxReadahead
	initOp.MaxWrite = c.cfg.MaxWrite

	initOp.Flags = 0

	// Tell the kernel not to use pitifully small 4 KiB writes.
	initOp.Flags |= fusekernel.InitBigWrites

	if c.cfg.EnableAsyncReads {
		initOp.Flags |= fusekernel.InitAsyncRead
	}

	if c.cfg.EnableAsyncDIO && asyncDIO {
		initOp.Flags |= fusekernel.InitAsyncDIO
	}

	// kernel 4.20 increases the max from 32 -> 256
	initOp.Flags |= fusekernel.InitMaxPages

	// MaxPages is the maximum size, in hardware pages, of the FUSE message
	// payload. It applies to both requests and replies, and does not include
	// the extra 1 page for the FUSE header and the "args" struct.
	initOp.MaxPages = c.cfg.MaxPages

	// Enable writeback caching if the user hasn't asked us not to.
	if !c.cfg.DisableWritebackCaching {
		initOp.Flags |= fusekernel.InitWritebackCache
	}

	// Enable caching symlink targets in the kernel page cache if the user opted
	// into it (might require fixing the size field of inode attributes first):
	if c.cfg.EnableSymlinkCaching && cacheSymlinks {
		initOp.Flags |= fusekernel.InitCacheSymlinks
	}

	// Tell the kernel to treat returning -ENOSYS on OpenFile as not needing
	// OpenFile calls at all (Linux >= 3.16):
	if c.cfg.EnableNoOpenSupport && noOpenSupport {
		initOp.Flags |= fusekernel.InitNoOpenSupport
	}

	// Tell the kernel to drop a file's cached pages when its mtime changes.
	if c.cfg.EnableAutoInvalData && autoInvalData && runtime.GOOS == "linux" {
		initOp.Flags |= fusekernel.InitAutoInvalData
	}

	// Tell the kernel to treat returning -ENOSYS on OpenDir as not needing
	// OpenDir calls at all (Linux >= 5.1):
	if c.cfg.EnableNoOpendirSupport && noOpendirSupport {
		initOp.Flags |= fusekernel.InitNoOpendirSupport
	}

	// Tell the Kernel to allow sending parallel lookup and readdir operations.
	if c.cfg.EnableParallelDirOps {
		initOp.Flags |= fusekernel.InitParallelDirOps
	}

	if c.cfg.EnableAtomicTrunc {
		initOp.Flags |= fusekernel.InitAtomicTrunc
	}

	if c.cfg.EnableReaddirplus {
		// Enable Readdirplus support, allowing the kernel to use Readdirplus
		initOp.Flags |= fusekernel.InitDoReaddirplus

		if c.cfg.EnableAutoReaddirplus {
			// Enable adaptive Readdirplus, allowing the kernel to choose between Readdirplus and Readdir
			initOp.Flags |= fusekernel.InitReaddirplusAuto
		}
	}

	return c.Reply(ctx, nil)
}

// Log information for an operation with the given ID. calldepth is the depth
// to use when recovering file:line information with runtime.Caller.
func (c *Connection) debugLog(
	fuseID uint64,
	calldepth int,
	format string,
	v ...interface{}) {
	if c.debugLogger == nil {
		return
	}

	// Get file:line info.
	var file string
	var line int
	var ok bool

	_, file, line, ok = runtime.Caller(calldepth)
	if !ok {
		file = "???"
	}

	fileLine := fmt.Sprintf("%v:%v", path.Base(file), line)

	// Format the actual message to be printed.
	msg := fmt.Sprintf(
		"Op 0x%08x %24s] %v",
		fuseID,
		fileLine,
		fmt.Sprintf(format, v...))

	// Print it.
	c.debugLogger.Println(msg)
}

// recordFatal records err as the connection's fatal error, unless one is
// already recorded, and returns the recorded error.
//
// LOCKS_EXCLUDED(c.mu)
func (c *Connection) recordFatal(err error) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.fatalErr == nil {
		c.fatalErr = err
	}
	return c.fatalErr
}

// fatalError returns the connection's fatal error, or nil if there is none.
//
// LOCKS_EXCLUDED(c.mu)
func (c *Connection) fatalError() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.fatalErr
}

// LOCKS_EXCLUDED(c.mu)
func (c *Connection) recordCancelFunc(
	fuseID uint64,
	f func()) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.cancelFuncs[fuseID]; ok {
		return fmt.Errorf("Already have cancel func for request %v", fuseID)
	}

	c.cancelFuncs[fuseID] = f
	return nil
}

// Set up state for an op that is about to be returned to the user, given its
// underlying fuse opcode and request ID.
//
// Return a context that should be used for the op.
//
// LOCKS_EXCLUDED(c.mu)
func (c *Connection) beginOp(
	opCode uint32,
	fuseID uint64) (context.Context, error) {
	// Start with the parent context.
	ctx := c.cfg.OpContext

	// Set up a cancellation function.
	//
	// Special case: On Darwin, osxfuse aggressively reuses "unique" request IDs.
	// This matters for Forget requests, which have no reply associated and
	// therefore have IDs that are immediately eligible for reuse. For these, we
	// should not record any state keyed on their ID.
	//
	// Cf. https://github.com/osxfuse/osxfuse/issues/208
	if opCode != fusekernel.OpForget {
		var cancel func()
		ctx, cancel = context.WithCancel(ctx)
		if err := c.recordCancelFunc(fuseID, cancel); err != nil {
			cancel()
			return nil, err
		}
	}

	return ctx, nil
}

// Clean up all state associated with an op to which the user has responded,
// given its underlying fuse opcode and request ID. This must be called before
// a response is sent to the kernel, to avoid a race where the request's ID
// might be reused by osxfuse.
//
// LOCKS_EXCLUDED(c.mu)
func (c *Connection) finishOp(
	opCode uint32,
	fuseID uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Even though the op is finished, context.WithCancel requires us to arrange
	// for the cancellation function to be invoked. We also must remove it from
	// our map.
	//
	// Special case: we don't do this for Forget requests. See the note in
	// beginOp above.
	if opCode != fusekernel.OpForget {
		cancel, ok := c.cancelFuncs[fuseID]
		if !ok {
			return fmt.Errorf("Unknown request ID in finishOp: %v", fuseID)
		}

		cancel()
		delete(c.cancelFuncs, fuseID)
	}

	return nil
}

// LOCKS_EXCLUDED(c.mu)
func (c *Connection) handleInterrupt(fuseID uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// NOTE(jacobsa): fuse.txt in the Linux kernel documentation
	// (https://tinyurl.com/2r4ajuwd) defines the kernel <-> userspace protocol
	// for interrupts.
	//
	// In particular, my reading of it is that an interrupt request cannot be
	// delivered to userspace before the original request. The part about the
	// race and EAGAIN appears to be aimed at userspace programs that
	// concurrently process requests (https://tinyurl.com/3euehwfb).
	//
	// So in this method if we can't find the ID to be interrupted, it means that
	// the request has already been replied to.
	//
	// Cf. https://github.com/osxfuse/osxfuse/issues/208
	// Cf. http://comments.gmane.org/gmane.comp.file-systems.fuse.devel/14675
	cancel, ok := c.cancelFuncs[fuseID]
	if !ok {
		return
	}

	cancel()
}

// Read the next message from the kernel. The message must later be destroyed
// using destroyInMessage.
func (c *Connection) readMessage() (*buffer.InMessage, error) {
	m := c.getInMessage()
	if err := m.Init(c.transport); err != nil {
		c.putInMessage(m)
		return nil, err
	}

	return m, nil
}

// Write a buffer.OutMessage to the kernel.
func (c *Connection) writeOutMessage(outMsg *buffer.OutMessage) error {
	return c.transport.SendMessage(outMsg.Parts())
}

// ReadOp consumes the next op from the kernel, returning the op and a context
// that should be used for work related to the op. It returns io.EOF if the
// kernel has closed the connection.
//
// ReadOp answers the poll hack's requests itself and reads on. See Mount.
//
// When a request's body fails to decode, ReadOp answers the request itself and
// reads on. The answer is EINVAL for a reply size above the cap, and EIO
// otherwise. The cap is MaxPages pages. For xattrs it is at least 64 KiB, or
// 128 KiB on darwin. On darwin, for directory reads it is at least the iosize
// the mount passes.
//
// Any other error is fatal to the connection, and ReadOp returns it from then
// on. Fatal errors are a failed read, a malformed message, a request ID
// already in flight, a second Reply to one op, and an INIT that fails to
// decode. A fatal error does not unmount the file system.
//
// If err == nil, the user is responsible for later calling c.Reply with the
// returned context.
//
// This function delivers ops in exactly the order the backend delivers them.
// It must not be called multiple times concurrently.
//
// LOCKS_EXCLUDED(c.mu)
func (c *Connection) ReadOp() (_ context.Context, op interface{}, _ error) {
	// Keep going until we find a request we know how to convert.
	for {
		if err := c.fatalError(); err != nil {
			return nil, nil, err
		}

		// Read the next message from the kernel.
		inMsg, err := c.readMessage()
		if err == io.EOF {
			return nil, nil, err
		}
		if err != nil {
			return nil, nil, c.recordFatal(fmt.Errorf("readMessage: %w", err))
		}

		// A Reply may have recorded a fatal error during the read.
		if err := c.fatalError(); err != nil {
			c.putInMessage(inMsg)
			return nil, nil, err
		}

		// Convert the message to an op.
		outMsg := c.getOutMessage()
		op, err = convertInMessage(&c.cfg, inMsg, outMsg, c.protocol)
		if err != nil {
			err = fmt.Errorf("convertInMessage: %w", err)
			opCode := inMsg.Header().Opcode
			if dupErr := c.rejectMessage(inMsg, outMsg, err); dupErr != nil {
				return nil, nil, c.recordFatal(dupErr)
			}
			// Init waits for this request, and the kernel sends it only once.
			if opCode == fusekernel.OpInit {
				return nil, nil, c.recordFatal(err)
			}
			continue
		}

		// Choose an ID for this operation for the purposes of logging, and log it.
		if c.debugLogger != nil {
			c.debugLog(inMsg.Header().Unique, 1, "<- %s", describeRequest(op))
		}

		// Special case: handle interrupt requests inline.
		if interruptOp, ok := op.(*interruptOp); ok {
			c.handleInterrupt(interruptOp.FuseID)
			c.putInMessage(inMsg)
			c.putOutMessage(outMsg)
			continue
		}

		// Special case: answer the poll hack's requests inline.
		answered, err := c.answerPollHack(inMsg, outMsg, op)
		if err != nil {
			return nil, nil, c.recordFatal(err)
		}
		if answered {
			continue
		}

		// Set up a context that remembers information about this op.
		ctx, err := c.beginOp(inMsg.Header().Opcode, inMsg.Header().Unique)
		if err != nil {
			// A reply would answer the request in flight under this ID.
			c.putInMessage(inMsg)
			c.putOutMessage(outMsg)
			return nil, nil, c.recordFatal(err)
		}
		var wlog *WireLogRecord
		if c.wireLogger != nil {
			wlog = NewWireLogRecord()
		}
		h := inMsg.Header()
		state := opState{inMsg: inMsg, outMsg: outMsg, op: op, wlog: wlog, fuseID: h.Unique, opCode: h.Opcode, replied: new(atomic.Bool)}
		if c.reportedInodes != nil {
			if read, ok := op.(*fuseops.ReadFileOp); ok {
				state.sizeAtRead = c.reportedSize(read.Inode)
			}
		}
		ctx = context.WithValue(ctx, contextKey, state)

		// Return the op to the user.
		return ctx, op, nil
	}
}

// rejectMessage answers a request whose body failed to decode with the errno
// in err, or EIO, and recycles its messages. The kernel expects no reply to
// FORGET, BATCH_FORGET and INTERRUPT. rejectMessage sends nothing and returns
// an error when another request with the same ID is in flight.
//
// LOCKS_EXCLUDED(c.mu)
func (c *Connection) rejectMessage(
	inMsg *buffer.InMessage,
	outMsg *buffer.OutMessage,
	err error) error {
	defer func() {
		c.putInMessage(inMsg)
		c.putOutMessage(outMsg)
	}()

	h := inMsg.Header()
	if c.errorLogger != nil {
		c.errorLogger.Printf("Op 0x%08x opcode %d] -> Error: %q", h.Unique, h.Opcode, err)
	}

	switch h.Opcode {
	case fusekernel.OpForget, fusekernel.OpBatchForget, fusekernel.OpInterrupt:
		return nil
	}

	if err := c.checkNotInFlight(h.Unique); err != nil {
		return err
	}

	c.kernelResponse(outMsg, h.Unique, nil, err)
	writeErr := c.writeOutMessage(outMsg)
	// See Reply for why ENOENT is benign.
	if writeErr != nil && !errors.Is(writeErr, syscall.ENOENT) && c.errorLogger != nil {
		c.errorLogger.Printf("writeMessage: %v %v", writeErr, outMsg.OutHeaderBytes())
	}
	return nil
}

// checkNotInFlight returns an error when the request with ID fuseID is in
// flight, because a reply under that ID would answer it.
//
// LOCKS_EXCLUDED(c.mu)
func (c *Connection) checkNotInFlight(fuseID uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.cancelFuncs[fuseID]; ok {
		return fmt.Errorf("Already have cancel func for request %v", fuseID)
	}
	return nil
}

// Skip errors that happen as a matter of course, since they spook users.
func (c *Connection) shouldLogError(
	op interface{},
	err error) bool {
	// We don't log non-errors.
	if err == nil {
		return false
	}

	// We can't log if there's nothing to log to.
	if c.errorLogger == nil {
		return false
	}

	// Judge the errno that kernelResponse sends, not any errno in the chain.
	// MountConfig.ErrorLogger lists the errnos skipped here.
	errno, _ := operror.Errno(err)

	switch errno {
	case syscall.ENOSYS, syscall.EINTR:
		// ENOSYS means the file system does not implement the op. EINTR follows
		// the kernel's interrupt.
		return false
	}

	switch op.(type) {
	case *fuseops.LookUpInodeOp:
		// It is totally normal for the kernel to ask to look up an inode by name
		// and find the name doesn't exist. For example, this happens when linking
		// a new file.
		if errno == syscall.ENOENT {
			return false
		}
	case *fuseops.RmDirOp, *fuseops.RenameOp:
		if errno == syscall.ENOTEMPTY {
			return false
		}
	case *fuseops.GetXattrOp, *fuseops.ListXattrOp:
		if errno == syscall.ENODATA || errno == syscall.ERANGE {
			return false
		}
	}

	return true
}

// Reply replies to an op previously read using ReadOp, with the supplied error
// (or nil if successful). The context must be the context returned by ReadOp.
//
// The kernel receives the first syscall.Errno in opErr's chain, or EIO when
// the chain has none. On FSKit, a short ReadFileOp reply becomes EIO; see
// fuseops.ReadFileOp.BytesRead. A reply the kernel refuses with ENOENT,
// because the request was interrupted, is not an error. A second Reply to the
// same op is fatal to the connection.
//
// LOCKS_EXCLUDED(c.mu)
func (c *Connection) Reply(ctx context.Context, opErr error) error {
	// Extract the state we stuffed in earlier.
	var key interface{} = contextKey
	foo := ctx.Value(key)
	state, ok := foo.(opState)
	if !ok {
		panic(fmt.Sprintf("Reply called with invalid context: %#v", ctx))
	}

	op := state.op
	inMsg := state.inMsg
	outMsg := state.outMsg
	fuseID := state.fuseID

	// After the first Reply, the op's messages and even its request ID may
	// belong to another request.
	if !state.replied.CompareAndSwap(false, true) {
		err := fmt.Errorf("Reply called twice for request %v", fuseID)
		c.recordFatal(err)
		return err
	}

	// Clean up state for this op.
	if err := c.finishOp(state.opCode, fuseID); err != nil {
		c.recordFatal(err)
		return err
	}

	// Deferred before sizeReplyMu is taken below, so the callback runs after
	// the lock is released.
	defer func() {
		// Invoke any callbacks set by the FUSE server after the response to the kernel is
		// complete and before the inMessage and outMessage memory buffers have been freed.
		c.runCallback(fuseID, op)

		// Make sure we destroy the messages when we're done.
		c.putInMessage(inMsg)
		c.putOutMessage(outMsg)
	}()

	if opErr == nil {
		opErr = checkBytesRead(outMsg, op)
	}

	// Record growth before the reply reaches the kernel, which may act on it.
	// A shrink waits until the kernel has the reply.
	if c.reportedInodes != nil {
		if changesReportedSizes(op) {
			c.sizeReplyMu.Lock()
			defer c.sizeReplyMu.Unlock()
		} else if _, ok := op.(*fuseops.ReadFileOp); ok {
			c.sizeReplyMu.RLock()
			defer c.sizeReplyMu.RUnlock()
		}
		opErr = c.trackReportedSizes(state, opErr)
	}

	if opErr == nil {
		c.debugLog(fuseID, 1, "-> %s", describeResponse(op))
	} else {
		c.logOpError(fuseID, op, "Error", opErr)
	}

	// Send the reply to the kernel, if one is required.
	noResponse := c.kernelResponse(outMsg, fuseID, op, opErr)

	if !noResponse {
		err := c.writeOutMessage(outMsg)
		// The kernel answers a reply to an interrupted request with ENOENT.
		// libfuse treats that as benign, and so does Reply.
		if err != nil && !errors.Is(err, syscall.ENOENT) {
			writeErrMsg := fmt.Sprintf("writeMessage: %v %v", err, outMsg.OutHeaderBytes())
			if c.errorLogger != nil {
				c.errorLogger.Print(writeErrMsg)
			}
			return errors.New(writeErrMsg)
		}
		outMsg.Sglist = nil
		if err == nil && c.reportedInodes != nil {
			c.shrinkReportedSizes(op, opErr)
		}
	}

	if c.wireLogger != nil {
		entry, err := formatWireLogEntry(op, opErr, state.wlog)
		if err == nil {
			c.wireLoggerMu.Lock()
			c.wireLogger.Write(entry)
			c.wireLoggerMu.Unlock()
		}
	}

	return nil
}

// checkBytesRead reports a BytesRead that the op's reply buffer, or its Dst,
// cannot hold. kernelResponseForOp or the size tracker would panic on it,
// after the op is marked replied.
func checkBytesRead(m *buffer.OutMessage, op interface{}) error {
	// convertInMessage grows m by the size of the readdir and xattr buffers.
	grown := m.Len() - buffer.OutMessageHeaderSize
	var n, size int
	switch o := op.(type) {
	case *fuseops.ReadFileOp:
		n, size = o.BytesRead, len(o.Dst)
		if o.Data != nil {
			size = 0
			for _, d := range o.Data {
				size += len(d)
			}
		}
	// The reply is the grown segment, and the size tracker parses Dst, which
	// a file system may have replaced.
	case *fuseops.ReadDirOp:
		n, size = o.BytesRead, min(grown, len(o.Dst))
	case *fuseops.ReadDirPlusOp:
		n, size = o.BytesRead, min(grown, len(o.Dst))
	case *fuseops.GetXattrOp:
		if len(o.Dst) == 0 {
			// A size query: BytesRead is the size, not a length within Dst.
			return nil
		}
		n, size = o.BytesRead, grown
	case *fuseops.ListXattrOp:
		if len(o.Dst) == 0 {
			return nil
		}
		n, size = o.BytesRead, grown
	default:
		return nil
	}
	if n < 0 || n > size {
		return fmt.Errorf("%T.BytesRead is %d, outside its %d-byte buffer: %w", op, n, size, syscall.EIO)
	}
	return nil
}

// runCallback runs op's Callback. The op is already answered, so a panic in
// the callback is logged instead of leaving Reply.
func (c *Connection) runCallback(fuseID uint64, op interface{}) {
	callback := c.callbackForOp(op)
	if callback == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			c.logOpError(fuseID, op, "Callback", &operror.Panic{Value: r, Stack: debug.Stack()})
		}
	}()
	callback()
}

// logOpError logs an op's error to the error logger, or to the debug logger
// when shouldLogError skips it. An operror.Panic is logged with its stack.
func (c *Connection) logOpError(fuseID uint64, op interface{}, label string, opErr error) {
	p, isPanic := opErr.(*operror.Panic)
	switch {
	case c.shouldLogError(op, opErr) && isPanic:
		c.errorLogger.Printf("Op 0x%08x %T] -> %s: panic: %v\n%s", fuseID, op, label, p.Value, p.Stack)
	case c.shouldLogError(op, opErr):
		c.errorLogger.Printf("Op 0x%08x %T] -> %s: %q", fuseID, op, label, opErr)
	case isPanic:
		c.debugLog(fuseID, 1, "-> %s: panic: %v\n%s", label, p.Value, p.Stack)
	default:
		c.debugLog(fuseID, 1, "-> %s: %q", label, opErr)
	}
}

func (c *Connection) callbackForOp(op interface{}) func() {
	switch o := op.(type) {
	case *fuseops.ReadFileOp:
		return o.Callback
	case *fuseops.WriteFileOp:
		return o.Callback
	}
	return nil
}

// Close the connection. Must not be called until operations read from the
// connection have been responded to.
//
// The transport owns platform-specific synchronization for releasing its
// endpoint safely.
func (c *Connection) close() error {
	return c.transport.Close()
}
