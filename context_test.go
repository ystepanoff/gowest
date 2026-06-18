package gowest

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// hijackRecorder is a minimal http.ResponseWriter that also implements
// http.Hijacker, handing back a caller-supplied net.Conn. It lets a test drive
// Accept against a controllable connection (here, one end of a net.Pipe) so the
// handshake write can be made to block.
type hijackRecorder struct {
	conn   net.Conn
	rw     *bufio.ReadWriter
	header http.Header
	status int
}

func (h *hijackRecorder) Header() http.Header {
	if h.header == nil {
		h.header = make(http.Header)
	}
	return h.header
}

func (h *hijackRecorder) Write(b []byte) (int, error) { return len(b), nil }

func (h *hijackRecorder) WriteHeader(status int) { h.status = status }

func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return h.conn, h.rw, nil
}

// pipeConn builds a *Conn over one end of a synchronous net.Pipe and returns the
// raw peer end. A net.Pipe has no internal buffering: a write blocks until the
// peer reads and a read blocks until the peer writes, which makes it ideal for
// deterministically exercising context cancellation and concurrent Close.
func pipeConn(t *testing.T) (*Conn, net.Conn) {
	t.Helper()
	c1, c2 := net.Pipe()
	t.Cleanup(func() {
		c1.Close()
		c2.Close()
	})
	conn := newConn(c1, bufio.NewReader(c1), bufio.NewWriter(c1), "", DefaultMaxMessageBytes)
	return conn, c2
}

// drain discards everything the peer sends until the connection closes, letting
// frames the Conn writes (notably the close frame) flush without a real client.
func drain(peer net.Conn) {
	go io.Copy(io.Discard, peer)
}

// TestReadContextCancelPipe verifies a Read blocked waiting for a frame unblocks
// promptly with context.Canceled when its context is cancelled.
func TestReadContextCancelPipe(t *testing.T) {
	conn, _ := pipeConn(t) // peer never writes, so Read blocks

	ctx, cancel := context.WithCancel(context.Background())
	readErr := make(chan error, 1)
	go func() {
		_, _, err := conn.Read(ctx)
		readErr <- err
	}()

	time.Sleep(50 * time.Millisecond) // let Read reach its blocking frame read
	cancel()

	select {
	case err := <-readErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Read err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not unblock on context cancellation")
	}
}

// TestWriteContextCancel verifies a Write blocked because the peer is not
// reading unblocks promptly with context.Canceled when its context is
// cancelled. The 1 MiB payload bypasses bufio's buffer so the write stalls in
// the underlying pipe.
func TestWriteContextCancel(t *testing.T) {
	conn, _ := pipeConn(t) // peer never reads, so Write blocks

	ctx, cancel := context.WithCancel(context.Background())
	writeErr := make(chan error, 1)
	go func() {
		writeErr <- conn.Write(ctx, MessageBinary, make([]byte, 1<<20))
	}()

	time.Sleep(50 * time.Millisecond) // let Write reach its blocking flush
	cancel()

	select {
	case err := <-writeErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Write err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Write did not unblock on context cancellation")
	}
}

// TestPingContextCancelPipe verifies Ping honours an already-running context's
// cancellation while it awaits a pong that never arrives.
func TestPingContextCancelPipe(t *testing.T) {
	conn, peer := pipeConn(t)
	drain(peer) // let the ping frame flush; no pong is ever sent back

	ctx, cancel := context.WithCancel(context.Background())
	pingErr := make(chan error, 1)
	go func() { pingErr <- conn.Ping(ctx) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-pingErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Ping err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ping did not honour context cancellation")
	}
}

// TestReadWritePingRejectCancelledContext verifies the pre-flight ctx.Err()
// check: an already-cancelled context fails the operation deterministically
// without racing the underlying I/O.
func TestReadWritePingRejectCancelledContext(t *testing.T) {
	t.Run("Read", func(t *testing.T) {
		conn, _ := pipeConn(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, err := conn.Read(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Read err = %v, want context.Canceled", err)
		}
	})
	t.Run("Write", func(t *testing.T) {
		conn, _ := pipeConn(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := conn.Write(ctx, MessageText, []byte("x")); !errors.Is(err, context.Canceled) {
			t.Fatalf("Write err = %v, want context.Canceled", err)
		}
	})
	t.Run("Ping", func(t *testing.T) {
		conn, _ := pipeConn(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := conn.Ping(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Ping err = %v, want context.Canceled", err)
		}
	})
}

// TestCloseUnblocksRead verifies a Read blocked waiting for a frame is released
// promptly by a concurrent Close, reporting ErrClosed.
func TestCloseUnblocksRead(t *testing.T) {
	conn, peer := pipeConn(t)
	drain(peer) // let Close's close frame flush

	readErr := make(chan error, 1)
	go func() {
		_, _, err := conn.Read(context.Background())
		readErr <- err
	}()

	time.Sleep(50 * time.Millisecond) // let Read block
	go conn.Close(StatusNormalClosure, "bye")

	select {
	case err := <-readErr:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("Read err = %v, want ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not unblock after concurrent Close")
	}
}

// TestCloseUnblocksWrite verifies a Write blocked because the peer is not
// reading is released promptly by a concurrent Close. Close sets a past write
// deadline before contending for the write lock, which evicts the stalled
// writer instead of deadlocking behind it.
func TestCloseUnblocksWrite(t *testing.T) {
	conn, peer := pipeConn(t) // peer not reading, so Write blocks

	writeErr := make(chan error, 1)
	go func() {
		writeErr <- conn.Write(context.Background(), MessageBinary, make([]byte, 1<<20))
	}()

	time.Sleep(50 * time.Millisecond) // let Write reach its blocking flush

	closeErr := make(chan error, 1)
	go func() { closeErr <- conn.Close(StatusNormalClosure, "bye") }()

	select {
	case err := <-writeErr:
		if err == nil {
			t.Fatal("Write returned nil, want failure after concurrent Close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Write did not unblock after concurrent Close")
	}

	// Drain so Close can flush its close frame and return without waiting out
	// the full closeWriteTimeout.
	drain(peer)
	select {
	case err := <-closeErr:
		if err != nil {
			t.Fatalf("Close returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return")
	}
}

// TestCloseUnblocksPing verifies a Ping blocked awaiting a pong is released
// promptly by a concurrent Close, reporting ErrClosed via the done channel.
func TestCloseUnblocksPing(t *testing.T) {
	conn, peer := pipeConn(t)
	drain(peer) // let the ping and close frames flush; no pong is ever sent

	pingErr := make(chan error, 1)
	go func() { pingErr <- conn.Ping(context.Background()) }()

	time.Sleep(50 * time.Millisecond) // let Ping enqueue its waiter and block
	go conn.Close(StatusNormalClosure, "bye")

	select {
	case err := <-pingErr:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("Ping err = %v, want ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ping did not unblock after concurrent Close")
	}
}

// TestAcceptCancelledMidHandshake cancels the context while Accept is writing
// the 101 response to a peer that never reads it, exercising
// applyHandshakeDeadline's watcher and handshakeError's ctx mapping.
func TestAcceptCancelledMidHandshake(t *testing.T) {
	// A pipe peer that never reads: the handshake write stalls until cancelled.
	srvConn, _ := net.Pipe()
	t.Cleanup(func() { srvConn.Close() })

	// Build a minimal upgrade request and a ResponseWriter whose Hijack hands
	// back our blocking pipe end.
	reqText := "GET / HTTP/1.1\r\n" +
		"Host: example.com\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(reqText)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	rec := &hijackRecorder{conn: srvConn, rw: bufio.NewReadWriter(
		bufio.NewReader(srvConn), bufio.NewWriter(srvConn))}

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	acceptErr := make(chan error, 1)
	go func() {
		_, err := Accept(ctx, rec, req, &AcceptOptions{OriginPatterns: []string{"*"}})
		acceptErr <- err
	}()

	select {
	case err := <-acceptErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Accept err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Accept did not unblock on mid-handshake cancellation")
	}
}

// TestWriteDeadlineRestored verifies the per-operation deadline is restored to
// "none" after an operation completes: a Write under a short timeout must not
// leave a stale, now-elapsed deadline that breaks a later unbounded Write.
func TestWriteDeadlineRestored(t *testing.T) {
	conn, peer := pipeConn(t)
	drain(peer)

	// First write installs a deadline of now+50ms via the context.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := conn.Write(ctx, MessageText, []byte("first")); err != nil {
		t.Fatalf("first Write err = %v", err)
	}

	// Sleep past that deadline. If cleanup failed to clear it, the connection's
	// write deadline is now in the past and the next write times out instantly.
	time.Sleep(100 * time.Millisecond)

	if err := conn.Write(context.Background(), MessageText, []byte("second")); err != nil {
		t.Fatalf("second Write err = %v, want nil (deadline not restored?)", err)
	}
}

// TestDeadlineNotPoisonedAcrossOperations stresses the watcher-join in
// applyDeadline: each iteration runs a deadline-bearing Write whose context is
// cancelled right around completion. The cleanup must join the watcher before
// restoring the deadline so a late "deadline in the past" cannot poison the
// following iteration. Run under -race, a missing join surfaces as a spurious
// timeout (or a data race on the deadline).
func TestDeadlineNotPoisonedAcrossOperations(t *testing.T) {
	conn, peer := pipeConn(t)
	drain(peer)

	for i := 0; i < 300; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		// Cancel concurrently with the write so the watcher may fire right as
		// the operation finishes — the exact window the join must close.
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			cancel()
		}()
		err := conn.Write(ctx, MessageText, []byte("x"))
		wg.Wait()
		// The write either completes before cancellation (nil) or is caught by
		// it (context.Canceled). Anything else means a stale deadline leaked.
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("iter %d: Write err = %v, want nil or context.Canceled", i, err)
		}
		if errors.Is(err, context.Canceled) {
			// A cancelled write fails the connection; stop the loop, the point
			// (no poisoning of a later op) only applies while the conn is live.
			return
		}
	}
}

// TestAcceptHonoursCancelledContext verifies Accept refuses an already-cancelled
// context before performing the upgrade, reporting the context error and a 503.
func TestAcceptHonoursCancelledContext(t *testing.T) {
	acceptErr := make(chan error, 1)
	_, resp := dial(t, func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := Accept(ctx, w, r, &AcceptOptions{OriginPatterns: []string{"*"}})
		acceptErr <- err
	}, nil)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	select {
	case err := <-acceptErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Accept err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Accept handler never ran")
	}
}
