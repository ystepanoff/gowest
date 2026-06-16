// Package gowest is a minimal, dependency-free WebSocket library for Go.
//
// It provides a small, context-first API built around a single Conn type that
// is safe to use from multiple goroutines under a simple, explicit contract.
//
// # Getting started
//
// Upgrade an incoming HTTP request with Accept, then read and write messages:
//
//	func handler(w http.ResponseWriter, r *http.Request) {
//		c, err := gowest.Accept(r.Context(), w, r, nil)
//		if err != nil {
//			return
//		}
//		defer c.Close(gowest.StatusInternalError, "")
//
//		for {
//			typ, data, err := c.Read(r.Context())
//			if err != nil {
//				return
//			}
//			if err := c.Write(r.Context(), typ, data); err != nil {
//				return
//			}
//		}
//	}
//
// # Concurrency contract
//
// The Conn type follows the same one-reader, many-writer rule used by most
// production WebSocket libraries:
//
//   - Write may be called concurrently from multiple goroutines. Writes are
//     serialised internally with a mutex so that frames never interleave.
//   - Read must be called from at most one goroutine at a time. Reading from
//     several goroutines concurrently is a programming error and corrupts the
//     message stream.
//   - Close is safe to call concurrently with Read and Write. It closes the
//     connection exactly once; in-flight Read and Write calls are unblocked and
//     return an error.
//
// # Context
//
// Accept, Read, Write and Ping honour the supplied context.Context. If the
// context carries a deadline it is applied to the underlying network operation
// via the connection's deadline, and if the context is cancelled the blocked
// operation is interrupted and returns the context's error. Cancellation is
// implemented with net.Conn deadlines rather than a per-call goroutine that
// outlives the operation: a short-lived watcher is always joined before the
// call returns, so no goroutine leaks and no deadline outlives its operation.
//
// A cancelled or timed-out Read or Write fails the connection, because the
// WebSocket frame stream cannot be safely resumed mid-frame. Close does not
// take a context; it bounds the close-frame write with a short internal
// deadline so it cannot block indefinitely on an unresponsive peer, and it
// unblocks any Read, Write or Ping concurrently in flight.
//
// # Control frames
//
// Read transparently handles control frames: incoming pings are answered with a
// pong, pongs are ignored, and a close frame causes Read to return a *CloseError
// after echoing the close back to the peer. Call Ping to send a ping and block
// until the peer's pong is observed by a concurrent Read. SetPingHandler and
// SetPongHandler register optional, non-blocking observers for received control
// frames; they do not replace the automatic pong reply.
//
// # Errors
//
// Read, Write and Ping return typed errors callers can match:
//
//   - ErrClosed is reported once the connection has been closed locally, for
//     example by Close. Test for it with errors.Is.
//   - A *CloseError is returned by Read when the peer sends a close frame, and
//     is the cause later operations report for a peer-initiated close.
//   - A *ProtocolError is returned when the peer violates the framing protocol
//     (an unmasked frame, an unknown opcode, an oversized or fragmented control
//     frame, a malformed close payload, or invalid UTF-8 in a text message).
//     The connection is failed and the corresponding status code is relayed to
//     the peer in a close frame. Inspect both with errors.As.
//
// # Limitations
//
// This release uses a mutex to serialise writes rather than a dedicated writer
// goroutine, does not implement permessage-deflate compression, and has no
// external dependencies. The legacy GetConnection, Read and WriteString
// functions remain available but are deprecated in favour of Accept and the
// Conn methods.
package gowest
