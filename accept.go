package gowest

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ystepanoff/gowest/internal/utils"
)

// DefaultMaxMessageBytes is the inbound message size limit applied when
// AcceptOptions.MaxMessageBytes is not set to a positive value. It is large
// enough for typical messages while guarding against a peer that announces an
// unbounded frame length to exhaust memory.
const DefaultMaxMessageBytes int64 = 32 << 20 // 32 MiB

// AcceptOptions configures the server-side WebSocket handshake performed by
// Accept. A nil *AcceptOptions selects the defaults documented on each field.
type AcceptOptions struct {
	// OriginPatterns is the list of host patterns permitted in the request's
	// Origin header. Each pattern is matched case-insensitively against the
	// Origin's host and may contain a single "*" wildcard matching any run of
	// characters (for example "*.example.com").
	//
	// If empty, Accept permits requests whose Origin host equals the Host
	// header (same-origin), which is the safe default for browser clients.
	// Use a pattern of "*" to allow all origins.
	OriginPatterns []string

	// Subprotocols lists the application subprotocols the server supports, in
	// order of preference. Accept selects the first entry that the client also
	// offers in its Sec-WebSocket-Protocol header. If none match, the
	// handshake still succeeds with no subprotocol.
	Subprotocols []string

	// MaxMessageBytes caps the size of a single inbound message, and also
	// bounds the buffer Read allocates for any one frame. A message exceeding
	// it fails the connection with StatusMessageTooBig.
	//
	// Zero or negative selects DefaultMaxMessageBytes. This default exists for
	// safety: without an upper bound a peer could announce an enormous frame
	// length and exhaust memory. To allow very large messages, set this to a
	// correspondingly large value explicitly.
	MaxMessageBytes int64

	// ReadBufferSize and WriteBufferSize set the sizes of the buffered reader
	// and writer wrapping the hijacked connection. Non-positive values use the
	// bufio package defaults.
	ReadBufferSize  int
	WriteBufferSize int
}

// Accept performs the server side of the WebSocket opening handshake on an
// incoming HTTP request and returns a ready-to-use Conn.
//
// It validates the upgrade headers, enforces the configured origin policy,
// negotiates a subprotocol, hijacks the underlying TCP connection and writes
// the 101 Switching Protocols response. On any failure it writes an appropriate
// HTTP error response (when the connection has not yet been hijacked) and
// returns a non-nil error.
//
// The ctx is used only for the duration of the handshake; per-message
// deadlines are supplied to Read and Write separately.
func Accept(ctx context.Context, w http.ResponseWriter, r *http.Request, opts *AcceptOptions) (*Conn, error) {
	if opts == nil {
		opts = &AcceptOptions{}
	}

	// Honour cancellation before doing any handshake work: if the caller's
	// context is already done there is no point validating headers or hijacking.
	if err := ctx.Err(); err != nil {
		http.Error(w, "request cancelled", http.StatusServiceUnavailable)
		return nil, err
	}

	if !strings.EqualFold(r.Method, http.MethodGet) {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return nil, errors.New("gowest: handshake requires GET")
	}
	if !utils.TokenPresentInString(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "expected websocket upgrade", http.StatusBadRequest)
		return nil, errors.New("gowest: missing or invalid Upgrade header")
	}
	if !headerContainsToken(r.Header, "Connection", "upgrade") {
		http.Error(w, "expected Connection: Upgrade", http.StatusBadRequest)
		return nil, errors.New("gowest: missing Connection: Upgrade header")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, errors.New("gowest: missing Sec-WebSocket-Key header")
	}
	if v := r.Header.Get("Sec-WebSocket-Version"); v != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "unsupported websocket version", http.StatusUpgradeRequired)
		return nil, errors.New("gowest: unsupported Sec-WebSocket-Version")
	}

	if err := verifyOrigin(r, opts.OriginPatterns); err != nil {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return nil, err
	}

	subprotocol := selectSubprotocol(r, opts.Subprotocols)

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "websocket not supported", http.StatusInternalServerError)
		return nil, errors.New("gowest: ResponseWriter does not support hijacking")
	}
	netConn, brw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}

	// After hijacking we own the connection: errors can no longer be reported
	// over HTTP, so close the socket and surface the error to the caller.
	accept := utils.WSSecKey([]byte(key))

	var b strings.Builder
	b.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	b.WriteString("Upgrade: websocket\r\n")
	b.WriteString("Connection: Upgrade\r\n")
	b.WriteString("Sec-WebSocket-Accept: " + accept + "\r\n")
	if subprotocol != "" {
		b.WriteString("Sec-WebSocket-Protocol: " + subprotocol + "\r\n")
	}
	b.WriteString("\r\n")

	// Bound the handshake write by ctx: a cancellation or deadline interrupts a
	// stalled write so Accept cannot block indefinitely sending the 101
	// response. A watcher goroutine pushes a past deadline onto the hijacked
	// connection on cancellation; stopHandshake joins it before we hand the
	// connection to newConn with a clean (zero) deadline.
	// Deferred so the watcher is always joined and the deadline restored even
	// if a write panics; it is idempotent-safe to run once at function exit.
	stopHandshake := applyHandshakeDeadline(ctx, netConn)
	defer stopHandshake()

	if _, err := brw.WriteString(b.String()); err != nil {
		netConn.Close()
		return nil, handshakeError(ctx, err)
	}
	if err := brw.Flush(); err != nil {
		netConn.Close()
		return nil, handshakeError(ctx, err)
	}

	// Reuse the hijacked reader: it may already hold bytes the client
	// pipelined immediately after the handshake, which a fresh reader over
	// netConn would drop. Only grow it when a larger buffer is requested and
	// nothing has been buffered yet.
	br := brw.Reader
	if opts.ReadBufferSize > 0 && br.Buffered() == 0 {
		br = bufio.NewReaderSize(netConn, opts.ReadBufferSize)
	}
	bw := brw.Writer
	if opts.WriteBufferSize > 0 {
		bw = bufio.NewWriterSize(netConn, opts.WriteBufferSize)
	}

	maxMessage := opts.MaxMessageBytes
	if maxMessage <= 0 {
		maxMessage = DefaultMaxMessageBytes
	}

	return newConn(netConn, br, bw, subprotocol, maxMessage), nil
}

// selectSubprotocol returns the first server-preferred subprotocol that the
// client also offered, or the empty string if there is no overlap.
func selectSubprotocol(r *http.Request, supported []string) string {
	if len(supported) == 0 {
		return ""
	}
	offered := make(map[string]struct{})
	for _, h := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(h, ",") {
			if p = strings.TrimSpace(p); p != "" {
				offered[p] = struct{}{}
			}
		}
	}
	for _, p := range supported {
		if _, ok := offered[p]; ok {
			return p
		}
	}
	return ""
}

// verifyOrigin enforces the configured origin policy. Requests without an
// Origin header (non-browser clients) are always allowed.
func verifyOrigin(r *http.Request, patterns []string) error {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return nil
	}
	u, err := url.Parse(origin)
	if err != nil {
		return errors.New("gowest: malformed Origin header")
	}
	host := u.Host

	if len(patterns) == 0 {
		if strings.EqualFold(host, r.Host) {
			return nil
		}
		return errors.New("gowest: request origin not allowed by same-origin policy")
	}
	for _, pattern := range patterns {
		if matchOriginPattern(pattern, host) {
			return nil
		}
	}
	return errors.New("gowest: request origin not allowed")
}

// matchOriginPattern matches host against a pattern that may contain a single
// "*" wildcard, case-insensitively.
func matchOriginPattern(pattern, host string) bool {
	pattern = strings.ToLower(pattern)
	host = strings.ToLower(host)
	if pattern == "*" {
		return true
	}
	star := strings.IndexByte(pattern, '*')
	if star < 0 {
		return pattern == host
	}
	prefix, suffix := pattern[:star], pattern[star+1:]
	return len(host) >= len(prefix)+len(suffix) &&
		strings.HasPrefix(host, prefix) &&
		strings.HasSuffix(host, suffix)
}

// applyHandshakeDeadline bounds the handshake response write by ctx using the
// hijacked connection's deadline, mirroring (*Conn).applyDeadline. It returns a
// stop function that joins the watcher goroutine and restores a zero deadline,
// so the connection handed to newConn carries no leftover deadline and no
// goroutine leaks.
func applyHandshakeDeadline(ctx context.Context, conn net.Conn) func() {
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetWriteDeadline(deadline)
	} else {
		conn.SetWriteDeadline(time.Time{})
	}

	if ctx.Done() == nil {
		return func() { conn.SetWriteDeadline(time.Time{}) }
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			conn.SetWriteDeadline(deadlineInThePast)
		case <-stop:
		}
	}()
	return func() {
		close(stop)
		<-done
		conn.SetWriteDeadline(time.Time{})
	}
}

// handshakeError prefers ctx.Err() over the opaque timeout a cancellation
// triggers, so a caller that cancelled the handshake sees the context cause.
func handshakeError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		var ne net.Error
		if errors.Is(err, net.ErrClosed) || (errors.As(err, &ne) && ne.Timeout()) {
			return ctxErr
		}
	}
	return err
}

// headerContainsToken reports whether the named header holds the given token in
// any comma-separated position, case-insensitively.
func headerContainsToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}
