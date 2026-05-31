package gowest

import (
	"bufio"
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

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
// goroutine may call Read at a time, and Close may be called concurrently with
// either. See the package documentation for details.
type Conn struct {
	conn net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer

	subprotocol string
	maxMessage  int64

	// writeMu serializes writes so concurrent Write calls never interleave
	// frames on the wire.
	writeMu sync.Mutex

	// closeMu guards the close handshake state below.
	closeMu   sync.Mutex
	closeSent bool

	// closeOnce ensures the underlying connection is torn down exactly once.
	closeOnce sync.Once
	done      chan struct{}
	closeErr  error
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

// Read reads the next data message from the connection. It blocks until a
// complete message (across any number of fragment frames) is available, the
// context is cancelled, or the connection is closed.
//
// Read transparently answers ping frames with pongs and discards incoming
// pongs. If the peer sends a close frame, Read echoes it and returns a
// *CloseError.
//
// Read must not be called from more than one goroutine at a time.
func (c *Conn) Read(ctx context.Context) (MessageType, []byte, error) {
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
		case opClose:
			code, reason := parseClosePayload(f.payload)
			ce := &CloseError{Code: code, Reason: reason}
			c.replyClose(code, reason)
			c.fail(ce)
			return 0, nil, ce

		case opPing:
			if err := c.writeControl(opPong, f.payload); err != nil {
				return 0, nil, c.fail(err)
			}
			continue

		case opPong:
			continue

		case opText, opBinary:
			if fragment {
				err := &CloseError{Code: StatusProtocolError, Reason: "expected continuation frame"}
				c.replyClose(err.Code, err.Reason)
				return 0, nil, c.fail(err)
			}
			if f.opcode == opBinary {
				msgType = MessageBinary
			} else {
				msgType = MessageText
			}
			message = append(message, f.payload...)

		case opContinuation:
			if !fragment {
				err := &CloseError{Code: StatusProtocolError, Reason: "unexpected continuation frame"}
				c.replyClose(err.Code, err.Reason)
				return 0, nil, c.fail(err)
			}
			message = append(message, f.payload...)

		default:
			err := &CloseError{Code: StatusProtocolError, Reason: "unknown opcode"}
			c.replyClose(err.Code, err.Reason)
			return 0, nil, c.fail(err)
		}

		if c.maxMessage > 0 && int64(len(message)) > c.maxMessage {
			err := &CloseError{Code: StatusMessageTooBig, Reason: "message exceeds max size"}
			c.replyClose(err.Code, err.Reason)
			return 0, nil, c.fail(err)
		}

		if f.fin {
			return msgType, message, nil
		}
		fragment = true
	}
}

// Write sends a single data message of the given type to the peer. It may be
// called concurrently from multiple goroutines; writes are serialized so frames
// never interleave.
func (c *Conn) Write(ctx context.Context, typ MessageType, payload []byte) error {
	if typ != MessageText && typ != MessageBinary {
		return errors.New("gowest: invalid message type")
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	// Re-check under the lock: the connection may have failed while we waited,
	// in which case report the real close cause rather than writing onto a
	// torn-down socket and surfacing an opaque I/O error.
	select {
	case <-c.done:
		return c.closeErr
	default:
	}

	stop := c.applyDeadline(ctx, func(t time.Time) { c.conn.SetWriteDeadline(t) })
	defer stop()

	if err := writeFrame(c.bw, frame{fin: true, opcode: typ.opcode(), payload: payload}); err != nil {
		return c.fail(c.contextError(ctx, err))
	}
	return nil
}

// Close sends a close frame with the given status code and reason to the peer
// and tears down the underlying connection. It is safe to call concurrently
// with Read and Write and is idempotent: only the first call performs the
// handshake, and subsequent calls return nil.
//
// The reason must be at most 123 bytes once combined with the two-byte status
// code, per RFC 6455's 125-byte control-frame limit.
func (c *Conn) Close(code StatusCode, reason string) error {
	if len(reason) > maxControlPayload-2 {
		reason = reason[:maxControlPayload-2]
	}
	c.replyClose(code, reason)
	c.fail(&CloseError{Code: code, Reason: reason})
	return nil
}

// replyClose sends a close frame at most once for the lifetime of the
// connection. Errors are ignored because the connection is being torn down
// regardless.
func (c *Conn) replyClose(code StatusCode, reason string) {
	c.closeMu.Lock()
	if c.closeSent {
		c.closeMu.Unlock()
		return
	}
	c.closeSent = true
	c.closeMu.Unlock()

	_ = c.writeControl(opClose, closePayload(code, reason))
}

// writeControl serializes a control frame against concurrent data writes.
func (c *Conn) writeControl(opcode byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	defer c.conn.SetWriteDeadline(time.Time{})

	return writeFrame(c.bw, frame{fin: true, opcode: opcode, payload: payload})
}

// fail closes the underlying connection exactly once, recording the cause that
// in-flight and future operations should report. It always returns that cause.
func (c *Conn) fail(cause error) error {
	c.closeOnce.Do(func() {
		if cause == nil {
			cause = net.ErrClosed
		}
		c.closeErr = cause
		c.bw.Flush()
		c.conn.Close()
		close(c.done)
	})
	return c.closeErr
}

// readError maps a low-level read error onto the connection's failure cause,
// preferring the context's error when the read was interrupted by cancellation
// or a deadline. Protocol violations surfaced by the frame parser are relayed
// to the peer as a close frame before the connection is failed.
func (c *Conn) readError(ctx context.Context, err error) error {
	var ce *CloseError
	if errors.As(err, &ce) {
		c.replyClose(ce.Code, ce.Reason)
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
	go func() {
		select {
		case <-ctx.Done():
			// Interrupt the blocked network operation; the operation
			// observes the resulting error and contextError maps it back
			// to ctx.Err().
			set(time.Unix(1, 0))
		case <-stop:
		}
	}()
	return func() {
		close(stop)
		set(time.Time{})
	}
}
