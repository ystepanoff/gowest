package gowest

import (
	"bufio"
	"context"
	"errors"
	"net"
	"sync"
	"time"
	"unicode/utf8"
)

// deadlineInThePast is an already-elapsed deadline used to interrupt a blocked
// net.Conn operation immediately. Any fixed time in the past works; the Unix
// epoch + 1s is well clear of the zero Time that means "no deadline".
var deadlineInThePast = time.Unix(1, 0)

// closeWriteTimeout bounds how long Close waits to put its close frame on the
// wire before giving up and tearing the connection down regardless.
const closeWriteTimeout = 5 * time.Second

// MessageType identifies the kind of a WebSocket data message.
type MessageType int

const (
	// MessageText is a UTF-8 encoded text message (opcode 0x1).
	MessageText MessageType = iota + 1
	// MessageBinary is a binary message (opcode 0x2).
	MessageBinary
)

func (t MessageType) opcode() byte {
	if t == MessageBinary {
		return opBinary
	}
	return opText
}

// Conn is a WebSocket connection.
//
// A Conn is safe for concurrent use under the contract documented on the
// package: any number of goroutines may call Write simultaneously, at most one
// goroutine may call Read at a time, and Close and Ping may be called
// concurrently with either. See the package documentation for details.
type Conn struct {
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer

	subprotocol string
	maxMessage  int64

	// writeMu serialises writes so concurrent Write calls never interleave
	// frames on the wire.
	writeMu sync.Mutex

	// closeMu guards the close handshake state below.
	closeMu   sync.Mutex
	closeSent bool

	// handlerMu guards the optional ping/pong observation handlers.
	handlerMu   sync.Mutex
	pingHandler func(payload []byte)
	pongHandler func(payload []byte)

	// pingMu guards the set of goroutines blocked in Ping awaiting a pong.
	pingMu      sync.Mutex
	pongWaiters []chan struct{}

	// closeOnce ensures the underlying connection is torn down exactly once.
	closeOnce sync.Once
	done      chan struct{}

	// causeMu guards closeErr/causeSet. The cause is recorded first-writer-wins
	// so a locally-initiated Close (or a peer close / protocol abort) attributes
	// the intended error even when an in-flight operation it evicted reaches
	// fail first with a raw timeout. closeErr is read without the lock only
	// after done is closed, which happens-after the recording write.
	causeMu  sync.Mutex
	causeSet bool
	closeErr error
}

func newConn(c net.Conn, br *bufio.Reader, bw *bufio.Writer, subprotocol string, maxMessage int64) *Conn {
	return &Conn{
		conn:        c,
		br:          br,
		bw:          bw,
		subprotocol: subprotocol,
		maxMessage:  maxMessage,
		done:        make(chan struct{}),
	}
}

// Subprotocol returns the application-level subprotocol negotiated during the
// handshake, or the empty string if none was selected.
func (c *Conn) Subprotocol() string {
	return c.subprotocol
}

// SetPingHandler registers a function invoked whenever a ping frame is
// received, after Read has already replied with the matching pong (gowest
// always answers pings automatically). The handler is purely observational and
// runs on the goroutine that called Read; it must not block. Pass nil to clear
// it. SetPingHandler may be called concurrently with Read.
func (c *Conn) SetPingHandler(h func(payload []byte)) {
	c.handlerMu.Lock()
	c.pingHandler = h
	c.handlerMu.Unlock()
}

// SetPongHandler registers a function invoked whenever a pong frame is
// received, whether solicited by Ping or sent unsolicited by the peer. It runs
// on the goroutine that called Read and must not block. Pass nil to clear it.
// SetPongHandler may be called concurrently with Read.
func (c *Conn) SetPongHandler(h func(payload []byte)) {
	c.handlerMu.Lock()
	c.pongHandler = h
	c.handlerMu.Unlock()
}

// Read reads the next data message from the connection. It blocks until a
// complete message (across any number of fragment frames) is available, the
// context is cancelled, or the connection is closed.
//
// Read transparently answers ping frames with pongs and discards incoming
// pongs. If the peer sends a close frame, Read echoes it and returns a
// *CloseError. A framing or UTF-8 violation by the peer fails the connection
// and returns a *ProtocolError after relaying the status code to the peer.
//
// Read must not be called from more than one goroutine at a time.
//
// Cancelling ctx mid-frame fails the connection: a partially consumed frame
// leaves the read stream at an indeterminate position, so the connection cannot
// safely be reused and subsequent operations observe the cancellation cause.
func (c *Conn) Read(ctx context.Context) (MessageType, []byte, error) {
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}

	stop := c.applyDeadline(ctx, func(t time.Time) { c.conn.SetReadDeadline(t) })
	defer stop()

	var (
		message  []byte
		msgType  MessageType
		fragment bool
	)

	for {
		f, err := readFrame(c.br, c.maxMessage)
		if err != nil {
			return 0, nil, c.readError(ctx, err)
		}

		switch f.opcode {
		case opPing:
			if err := c.writeControl(opPong, f.payload); err != nil {
				return 0, nil, c.fail(c.contextError(ctx, err))
			}
			c.invokePingHandler(f.payload)
			continue

		case opPong:
			c.notePong()
			c.invokePongHandler(f.payload)
			continue

		case opClose:
			return 0, nil, c.handleClose(f.payload)

		case opText, opBinary:
			if fragment {
				return 0, nil, c.abortProtocol(&ProtocolError{Code: StatusProtocolError, Reason: "expected continuation frame"})
			}
			if f.opcode == opBinary {
				msgType = MessageBinary
			} else {
				msgType = MessageText
			}
			message = append(message, f.payload...)

		case opContinuation:
			if !fragment {
				return 0, nil, c.abortProtocol(&ProtocolError{Code: StatusProtocolError, Reason: "unexpected continuation frame"})
			}
			message = append(message, f.payload...)

		default:
			// Unreachable: readFrame rejects reserved opcodes before we get
			// here. Kept as defence in depth.
			return 0, nil, c.abortProtocol(&ProtocolError{Code: StatusProtocolError, Reason: "unknown opcode"})
		}

		if c.maxMessage > 0 && int64(len(message)) > c.maxMessage {
			return 0, nil, c.abortProtocol(&ProtocolError{Code: StatusMessageTooBig, Reason: "message exceeds max size"})
		}

		if !f.fin {
			fragment = true
			continue
		}

		// A complete text message must be valid UTF-8 (RFC 6455 section 8.1).
		if msgType == MessageText && !utf8.Valid(message) {
			return 0, nil, c.abortProtocol(&ProtocolError{Code: StatusInvalidFramePayloadData, Reason: "invalid UTF-8 in text message"})
		}
		return msgType, message, nil
	}
}

// handleClose processes an inbound close frame: it validates the payload,
// echoes a close back to the peer and fails the connection. It returns the
// *CloseError describing the peer's close, or a *ProtocolError if the payload
// was malformed.
func (c *Conn) handleClose(payload []byte) error {
	code, reason, err := parseClosePayload(payload)
	if err != nil {
		var pe *ProtocolError
		errors.As(err, &pe)
		return c.abortProtocol(pe)
	}
	ce := &CloseError{Code: code, Reason: reason}
	// Record before replyClose evicts any in-flight writer, so concurrent
	// operations attribute the peer's CloseError rather than a raw timeout.
	c.recordCause(ce)
	// Echo the close (RFC 6455 section 5.5.1). closePayload suppresses the
	// reserved 1005 "no status" code, producing an empty close frame. The
	// reason is echoed back verbatim; it already passed parseClosePayload's
	// UTF-8 and length checks, so it fits the control-frame limit.
	c.replyClose(code, reason)
	c.fail(ce)
	return ce
}

// Write sends a single data message of the given type to the peer. It may be
// called concurrently from multiple goroutines; writes are serialised so frames
// never interleave.
func (c *Conn) Write(ctx context.Context, typ MessageType, payload []byte) error {
	if typ != MessageText && typ != MessageBinary {
		return errors.New("gowest: invalid message type")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	// Re-check under the lock: the connection may have failed while we waited,
	// in which case report the real close cause rather than writing onto a
	// torn-down socket and surfacing an opaque I/O error.
	select {
	case <-c.done:
		return c.cause()
	default:
	}

	stop := c.applyDeadline(ctx, func(t time.Time) { c.conn.SetWriteDeadline(t) })
	defer stop()

	if err := writeFrame(c.bw, frame{fin: true, opcode: typ.opcode(), payload: payload}); err != nil {
		return c.fail(c.contextError(ctx, err))
	}
	return nil
}

// Ping sends a ping frame to the peer and blocks until a pong is received, the
// context is cancelled, or the connection is closed. Because pongs are observed
// by Read, a goroutine must be calling Read concurrently for Ping to complete;
// otherwise Ping blocks until ctx expires.
//
// Ping returns nil once a pong arrives, ctx.Err() if the context is cancelled
// first, or the connection's failure cause (for example ErrClosed) if the
// connection is torn down. Any pong satisfies a pending Ping; the ping payload
// is empty.
func (c *Conn) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-c.done:
		return c.cause()
	default:
	}

	ch := make(chan struct{}, 1)
	c.pingMu.Lock()
	c.pongWaiters = append(c.pongWaiters, ch)
	c.pingMu.Unlock()

	if err := c.writePing(ctx); err != nil {
		c.removePongWaiter(ch)
		return c.fail(c.contextError(ctx, err))
	}

	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		c.removePongWaiter(ch)
		return ctx.Err()
	case <-c.done:
		c.removePongWaiter(ch)
		return c.cause()
	}
}

// writePing puts a ping frame on the wire, honouring ctx for both its deadline
// and cancellation. Unlike the bounded control writes used during teardown, the
// ping write tracks the caller's context so a cancelled Ping unblocks promptly.
func (c *Conn) writePing(ctx context.Context) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	select {
	case <-c.done:
		return c.cause()
	default:
	}

	stop := c.applyDeadline(ctx, func(t time.Time) { c.conn.SetWriteDeadline(t) })
	defer stop()

	return writeFrame(c.bw, frame{fin: true, opcode: opPing})
}

// notePong wakes every goroutine currently blocked in Ping. Any pong is treated
// as satisfying all outstanding pings, which is sufficient for liveness checks
// and avoids fragile payload correlation across coalesced pongs.
func (c *Conn) notePong() {
	c.pingMu.Lock()
	for _, ch := range c.pongWaiters {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	c.pongWaiters = nil
	c.pingMu.Unlock()
}

// removePongWaiter discards a waiter that gave up before a pong arrived.
func (c *Conn) removePongWaiter(ch chan struct{}) {
	c.pingMu.Lock()
	for i, w := range c.pongWaiters {
		if w == ch {
			c.pongWaiters = append(c.pongWaiters[:i], c.pongWaiters[i+1:]...)
			break
		}
	}
	c.pingMu.Unlock()
}

// Close sends a close frame with the given status code and reason to the peer
// and tears down the underlying connection. It is safe to call concurrently
// with Read, Write and Ping and is idempotent: only the first call performs the
// handshake, and subsequent calls return nil.
//
// After Close, in-flight and future Read, Write and Ping calls observe
// ErrClosed (a peer-initiated close is reported as a *CloseError instead).
//
// Close is bounded: it spends at most closeWriteTimeout trying to put the close
// frame on the wire before tearing the connection down regardless, so it cannot
// block indefinitely on an unresponsive peer.
//
// The reason must be at most 123 bytes once combined with the two-byte status
// code, per RFC 6455's 125-byte control-frame limit; longer reasons are
// truncated.
func (c *Conn) Close(code StatusCode, reason string) error {
	// Record the intended cause before evicting any in-flight writer in
	// replyClose, so concurrent operations attribute ErrClosed rather than the
	// raw timeout their eviction produces.
	c.recordCause(ErrClosed)
	c.replyClose(code, reason)
	c.fail(ErrClosed)
	return nil
}

// replyClose sends a close frame at most once for the lifetime of the
// connection. The reason is truncated to fit the 125-byte control-frame limit.
// Errors are ignored because the connection is being torn down regardless.
func (c *Conn) replyClose(code StatusCode, reason string) {
	c.closeMu.Lock()
	if c.closeSent {
		c.closeMu.Unlock()
		return
	}
	c.closeSent = true
	c.closeMu.Unlock()

	if len(reason) > maxControlPayload-2 {
		reason = reason[:maxControlPayload-2]
	}
	_ = c.writeControl(opClose, closePayload(code, reason))
}

// writeControl serialises a control frame against concurrent data writes,
// bounded by closeWriteTimeout.
//
// Before contending for writeMu it sets a past write deadline, which evicts any
// data Write currently blocked in a send syscall so this control frame (used
// for the close handshake) cannot deadlock behind an unresponsive peer. A
// blocked writer holds writeMu and cannot run its own deadline cleanup until it
// returns, so nothing competes to clear the past deadline and eviction is
// prompt. Once it holds the lock it installs the real deadline for its own
// write, bounding the whole operation by closeWriteTimeout. The evicted writer
// observes a timeout and, because the cause was recorded before teardown,
// reports the intended close cause.
func (c *Conn) writeControl(opcode byte, payload []byte) error {
	c.conn.SetWriteDeadline(deadlineInThePast)

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.conn.SetWriteDeadline(time.Now().Add(closeWriteTimeout))
	defer c.conn.SetWriteDeadline(time.Time{})

	return writeFrame(c.bw, frame{fin: true, opcode: opcode, payload: payload})
}

// invokePingHandler calls the registered ping handler, if any, with a snapshot
// taken under the handler lock so a concurrent SetPingHandler is safe.
func (c *Conn) invokePingHandler(payload []byte) {
	c.handlerMu.Lock()
	h := c.pingHandler
	c.handlerMu.Unlock()
	if h != nil {
		h(payload)
	}
}

// invokePongHandler mirrors invokePingHandler for pong frames.
func (c *Conn) invokePongHandler(payload []byte) {
	c.handlerMu.Lock()
	h := c.pongHandler
	c.handlerMu.Unlock()
	if h != nil {
		h(payload)
	}
}

// abortProtocol relays a protocol violation to the peer as a close frame
// carrying its status code, fails the connection, and returns the violation as
// the cause future operations will report.
func (c *Conn) abortProtocol(pe *ProtocolError) error {
	// Record before replyClose evicts any in-flight writer, so it attributes
	// the protocol error rather than the timeout the eviction produces.
	c.recordCause(pe)
	c.replyClose(pe.Code, pe.Reason)
	c.fail(pe)
	return pe
}

// fail closes the underlying connection exactly once, recording the cause that
// in-flight and future operations should report. It always returns that cause.
//
// The cause is recorded first-writer-wins: callers that initiated the teardown
// (Close, abortProtocol, handleClose) record their intended cause before
// evicting in-flight operations, so a victim that reaches fail first with a raw
// timeout does not overwrite it.
func (c *Conn) fail(cause error) error {
	c.closeOnce.Do(func() {
		c.recordCause(cause)
		c.bw.Flush()
		c.conn.Close()
		close(c.done)
	})
	return c.cause()
}

// recordCause stores the failure cause the first time it is called with a
// non-nil error; later calls are ignored. A nil cause defaults to ErrClosed.
func (c *Conn) recordCause(cause error) {
	if cause == nil {
		cause = ErrClosed
	}
	c.causeMu.Lock()
	if !c.causeSet {
		c.causeSet = true
		c.closeErr = cause
	}
	c.causeMu.Unlock()
}

// cause returns the recorded failure cause, or ErrClosed if none was recorded
// (which cannot happen once done is closed, but keeps the accessor total).
func (c *Conn) cause() error {
	c.causeMu.Lock()
	defer c.causeMu.Unlock()
	if !c.causeSet {
		return ErrClosed
	}
	return c.closeErr
}

// readError maps a low-level read error onto the connection's failure cause,
// preferring the context's error when the read was interrupted by cancellation
// or a deadline. Protocol violations surfaced by the frame parser are relayed
// to the peer as a close frame before the connection is failed.
func (c *Conn) readError(ctx context.Context, err error) error {
	var pe *ProtocolError
	if errors.As(err, &pe) {
		return c.abortProtocol(pe)
	}
	return c.fail(c.contextError(ctx, err))
}

// contextError prefers ctx.Err() over a network timeout/closed error, so a
// cancelled or timed-out operation reports the context cause rather than the
// opaque deadline error it triggered.
func (c *Conn) contextError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		var ne net.Error
		if errors.Is(err, net.ErrClosed) || (errors.As(err, &ne) && ne.Timeout()) {
			return ctxErr
		}
	}
	return err
}

// applyDeadline wires a context's cancellation and deadline onto a net.Conn
// deadline setter. It returns a cleanup function that must be called when the
// operation completes.
//
// When ctx can be cancelled, a watcher goroutine sets a deadline in the past on
// cancellation, which unblocks the in-flight network call; the operation then
// observes the resulting timeout error and contextError maps it back to
// ctx.Err(). The cleanup function joins this watcher before restoring the
// zero (no-op) deadline, so the past deadline can never outlive the operation
// and poison a subsequent one, and the goroutine can never leak.
func (c *Conn) applyDeadline(ctx context.Context, set func(time.Time)) func() {
	if deadline, ok := ctx.Deadline(); ok {
		set(deadline)
	} else {
		set(time.Time{})
	}

	if ctx.Done() == nil {
		return func() { set(time.Time{}) }
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			// Interrupt the blocked network operation; the operation
			// observes the resulting error and contextError maps it back
			// to ctx.Err().
			set(deadlineInThePast)
		case <-stop:
		}
	}()
	return func() {
		// Join the watcher first: once done is closed the watcher has
		// returned, so no further set() call can be in flight. Only then is
		// it safe to restore the zero deadline for the next operation.
		close(stop)
		<-done
		set(time.Time{})
	}
}
