package gowest

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// testClient is a minimal RFC 6455 client used only by the tests. It masks the
// frames it sends, as clients must, and parses the (unmasked) frames it
// receives from the server.
type testClient struct {
	conn net.Conn
	br   *bufio.Reader
}

// dial performs a WebSocket handshake against handler and returns a client
// speaking the negotiated connection.
func dial(t *testing.T, handler http.HandlerFunc, headers map[string]string) (*testClient, *http.Response) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	host := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	req := "GET / HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n"
	for k, v := range headers {
		req += k + ": " + v + "\r\n"
	}
	req += "\r\n"

	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write handshake: %v", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read handshake response: %v", err)
	}
	return &testClient{conn: conn, br: br}, resp
}

func (c *testClient) writeFrame(opcode byte, payload []byte) error {
	var header []byte
	b0 := byte(0x80) | opcode // FIN + opcode
	header = append(header, b0)

	length := len(payload)
	switch {
	case length < 126:
		header = append(header, byte(0x80)|byte(length))
	case length < 1<<16:
		header = append(header, byte(0x80)|126)
		var ext [2]byte
		binary.BigEndian.PutUint16(ext[:], uint16(length))
		header = append(header, ext[:]...)
	default:
		header = append(header, byte(0x80)|127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(length))
		header = append(header, ext[:]...)
	}

	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	header = append(header, mask[:]...)

	masked := make([]byte, length)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i&3]
	}

	if _, err := c.conn.Write(header); err != nil {
		return err
	}
	_, err := c.conn.Write(masked)
	return err
}

func (c *testClient) readFrame() (opcode byte, payload []byte, err error) {
	var header [2]byte
	if _, err = io.ReadFull(c.br, header[:]); err != nil {
		return 0, nil, err
	}
	opcode = header[0] & 0x0f
	size := uint64(header[1] & 0x7f)
	switch size {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, err
		}
		size = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return 0, nil, err
		}
		size = binary.BigEndian.Uint64(ext[:])
	}
	payload = make([]byte, size)
	_, err = io.ReadFull(c.br, payload)
	return opcode, payload, err
}

func TestAcceptHandshake(t *testing.T) {
	_, resp := dial(t, func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(r.Context(), w, r, &AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		c.Close(StatusNormalClosure, "")
	}, nil)

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	// RFC 6455 example accept value for the sample key.
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("Sec-WebSocket-Accept = %q", got)
	}
}

func TestEcho(t *testing.T) {
	client, _ := dial(t, echoHandlerFor(t), nil)

	want := "hello, gowest"
	if err := client.writeFrame(opText, []byte(want)); err != nil {
		t.Fatalf("client write: %v", err)
	}
	opcode, payload, err := client.readFrame()
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	if opcode != opText {
		t.Fatalf("opcode = %d, want text", opcode)
	}
	if string(payload) != want {
		t.Fatalf("payload = %q, want %q", payload, want)
	}
}

func TestFragmentedMessage(t *testing.T) {
	var server *Conn
	ready := make(chan struct{})
	client, _ := dial(t, func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(r.Context(), w, r, &AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		server = c
		close(ready)
		typ, data, err := c.Read(context.Background())
		if err != nil {
			t.Errorf("Read: %v", err)
			return
		}
		c.Write(context.Background(), typ, data)
	}, nil)

	// Send "foo" + "bar" as two fragments of one text message.
	writeRawFragment(t, client, opText, false, []byte("foo"))
	writeRawFragment(t, client, opContinuation, true, []byte("bar"))

	_, payload, err := client.readFrame()
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(payload) != "foobar" {
		t.Fatalf("reassembled = %q, want foobar", payload)
	}
	<-ready
	server.Close(StatusNormalClosure, "")
}

func TestPingPong(t *testing.T) {
	client, _ := dial(t, echoHandlerFor(t), nil)

	if err := client.writeFrame(opPing, []byte("ka")); err != nil {
		t.Fatalf("ping: %v", err)
	}
	opcode, payload, err := client.readFrame()
	if err != nil {
		t.Fatalf("read pong: %v", err)
	}
	if opcode != opPong {
		t.Fatalf("opcode = %d, want pong", opcode)
	}
	if string(payload) != "ka" {
		t.Fatalf("pong payload = %q, want ka", payload)
	}
}

func TestCloseHandshake(t *testing.T) {
	readErr := make(chan error, 1)
	client, _ := dial(t, func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(r.Context(), w, r, &AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		_, _, err = c.Read(context.Background())
		readErr <- err
	}, nil)

	if err := client.writeFrame(opClose, closePayload(StatusNormalClosure, "bye")); err != nil {
		t.Fatalf("client close: %v", err)
	}
	opcode, payload, err := client.readFrame()
	if err != nil {
		t.Fatalf("read close echo: %v", err)
	}
	if opcode != opClose {
		t.Fatalf("opcode = %d, want close", opcode)
	}
	code, reason, perr := parseClosePayload(payload)
	if perr != nil {
		t.Fatalf("parse close payload: %v", perr)
	}
	if code != StatusNormalClosure || reason != "bye" {
		t.Fatalf("close = (%d, %q), want (1000, bye)", code, reason)
	}

	select {
	case err := <-readErr:
		ce, ok := err.(*CloseError)
		if !ok {
			t.Fatalf("Read error = %T, want *CloseError", err)
		}
		if ce.Code != StatusNormalClosure {
			t.Fatalf("CloseError.Code = %d, want 1000", ce.Code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server Read did not return after close")
	}
}

func TestConcurrentWrites(t *testing.T) {
	const writers = 8
	client, _ := dial(t, func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(r.Context(), w, r, &AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		var wg sync.WaitGroup
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// 200-byte payloads force multi-write frames, exposing
				// any interleaving if the mutex were absent.
				c.Write(context.Background(), MessageBinary, make([]byte, 200))
			}()
		}
		wg.Wait()
		c.Close(StatusNormalClosure, "")
	}, nil)

	// Every frame must arrive intact and well-formed.
	for i := 0; i < writers; i++ {
		opcode, payload, err := client.readFrame()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if opcode != opBinary {
			t.Fatalf("frame %d opcode = %d, want binary", i, opcode)
		}
		if len(payload) != 200 {
			t.Fatalf("frame %d len = %d, want 200", i, len(payload))
		}
	}
}

func TestMaxMessageBytes(t *testing.T) {
	client, _ := dial(t, func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(r.Context(), w, r, &AcceptOptions{
			OriginPatterns:  []string{"*"},
			MaxMessageBytes: 4,
		})
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		_, _, err = c.Read(context.Background())
		if _, ok := err.(*ProtocolError); !ok {
			t.Errorf("Read error = %v (%T), want *ProtocolError", err, err)
		}
	}, nil)

	client.writeFrame(opText, []byte("too long"))
	opcode, payload, err := client.readFrame()
	if err != nil {
		t.Fatalf("read close: %v", err)
	}
	if opcode != opClose {
		t.Fatalf("opcode = %d, want close", opcode)
	}
	if code, _, _ := parseClosePayload(payload); code != StatusMessageTooBig {
		t.Fatalf("close code = %d, want %d", code, StatusMessageTooBig)
	}
}

// TestReadFrameRejectsHugeLength guards the default size cap: a header that
// announces an enormous payload must be rejected before any allocation,
// regardless of how few bytes actually follow it.
func TestReadFrameRejectsHugeLength(t *testing.T) {
	// FIN+text, masked, 64-bit length = ~1 EiB. Only the 10-byte header is
	// present; a buggy reader would try to make([]byte, 1<<60) and crash. The
	// size guard fires before the mask key is read, so no further bytes are
	// needed.
	header := []byte{0x81, 0x80 | 127, 0x10, 0, 0, 0, 0, 0, 0, 0}
	r := bufio.NewReader(strings.NewReader(string(header)))

	_, err := readFrame(r, DefaultMaxMessageBytes)
	pe, ok := err.(*ProtocolError)
	if !ok {
		t.Fatalf("error = %v (%T), want *ProtocolError", err, err)
	}
	if pe.Code != StatusMessageTooBig {
		t.Fatalf("code = %d, want %d", pe.Code, StatusMessageTooBig)
	}
}

// TestDefaultMaxMessageBytes verifies Accept applies DefaultMaxMessageBytes
// when the caller leaves MaxMessageBytes unset, so the connection is bounded by
// default rather than unlimited.
func TestDefaultMaxMessageBytes(t *testing.T) {
	got := make(chan int64, 1)
	dial(t, func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(r.Context(), w, r, &AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		got <- c.maxMessage
		c.Close(StatusNormalClosure, "")
	}, nil)

	select {
	case m := <-got:
		if m != DefaultMaxMessageBytes {
			t.Fatalf("maxMessage = %d, want %d", m, DefaultMaxMessageBytes)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler never ran")
	}
}

// TestPipelinedFramePreserved ensures a data frame the client sends in the same
// write as the handshake request is not dropped, even when ReadBufferSize asks
// for a larger reader. This exercises the brw.Reader reuse in Accept.
func TestPipelinedFramePreserved(t *testing.T) {
	echoed := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(r.Context(), w, r, &AcceptOptions{
			OriginPatterns: []string{"*"},
			ReadBufferSize: 8192, // force the larger-reader path
		})
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		_, data, err := c.Read(context.Background())
		if err != nil {
			t.Errorf("Read: %v", err)
			return
		}
		echoed <- data
		c.Close(StatusNormalClosure, "")
	}))
	t.Cleanup(srv.Close)

	host := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	// Build the handshake request and a masked text frame, then write both in
	// a single Write so the server buffers the frame while reading the request.
	req := "GET / HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"

	payload := []byte("pipelined")
	var mask [4]byte
	rand.Read(mask[:])
	frameBytes := []byte{0x81, byte(0x80) | byte(len(payload))}
	frameBytes = append(frameBytes, mask[:]...)
	for i := range payload {
		frameBytes = append(frameBytes, payload[i]^mask[i&3])
	}

	if _, err := conn.Write(append([]byte(req), frameBytes...)); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case data := <-echoed:
		if string(data) != "pipelined" {
			t.Fatalf("server read %q, want pipelined", data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server never received the pipelined frame")
	}
}

func TestSubprotocolNegotiation(t *testing.T) {
	_, resp := dial(t, func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(r.Context(), w, r, &AcceptOptions{
			OriginPatterns: []string{"*"},
			Subprotocols:   []string{"chat", "superchat"},
		})
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		if c.Subprotocol() != "superchat" {
			t.Errorf("Subprotocol = %q, want superchat", c.Subprotocol())
		}
		c.Close(StatusNormalClosure, "")
	}, map[string]string{"Sec-WebSocket-Protocol": "superchat, json"})

	if got := resp.Header.Get("Sec-WebSocket-Protocol"); got != "superchat" {
		t.Fatalf("negotiated subprotocol = %q, want superchat", got)
	}
}

func TestOriginRejected(t *testing.T) {
	_, resp := dial(t, func(w http.ResponseWriter, r *http.Request) {
		_, err := Accept(r.Context(), w, r, &AcceptOptions{
			OriginPatterns: []string{"good.example.com"},
		})
		if err == nil {
			t.Error("Accept succeeded, want origin rejection")
		}
	}, map[string]string{"Origin": "http://evil.example.com"})

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestReadContextCancel(t *testing.T) {
	done := make(chan error, 1)
	client, _ := dial(t, func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(r.Context(), w, r, &AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()
		_, _, err = c.Read(ctx)
		done <- err
	}, nil)
	_ = client

	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("Read error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not observe context cancellation")
	}
}

// writeRawFragment sends a frame with explicit FIN/opcode so tests can build
// fragmented messages.
func writeRawFragment(t *testing.T, c *testClient, opcode byte, fin bool, payload []byte) {
	t.Helper()
	var b0 byte = opcode
	if fin {
		b0 |= 0x80
	}
	header := []byte{b0, byte(0x80) | byte(len(payload))}
	var mask [4]byte
	rand.Read(mask[:])
	header = append(header, mask[:]...)
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ mask[i&3]
	}
	if _, err := c.conn.Write(header); err != nil {
		t.Fatalf("write fragment header: %v", err)
	}
	if _, err := c.conn.Write(masked); err != nil {
		t.Fatalf("write fragment payload: %v", err)
	}
}

// echoHandlerFor returns a handler that echoes one message and keeps the
// connection open until the test tears it down.
func echoHandlerFor(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(r.Context(), w, r, &AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		for {
			typ, data, err := c.Read(context.Background())
			if err != nil {
				return
			}
			if err := c.Write(context.Background(), typ, data); err != nil {
				return
			}
		}
	}
}

// readHandlerFor returns a handler that performs a single Read and reports its
// error on the channel, then keeps serving so the connection stays alive for
// the close handshake.
func readHandlerFor(t *testing.T, readErr chan<- error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(r.Context(), w, r, &AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		_, _, err = c.Read(context.Background())
		readErr <- err
	}
}

// TestProtocolViolations checks that the Conn rejects malformed client frames
// over a real connection, relaying the documented status code in the close
// frame it sends back and surfacing a *ProtocolError from Read. Each case
// writes raw bytes the disciplined testClient would never produce.
func TestProtocolViolations(t *testing.T) {
	tests := []struct {
		name     string
		raw      []byte
		wantCode StatusCode
	}{
		{
			name:     "unmasked frame",
			raw:      buildFrame(true, 0, opText, false, []byte("nope")),
			wantCode: StatusProtocolError,
		},
		{
			name:     "reserved opcode",
			raw:      buildFrame(true, 0, 0x5, true, nil),
			wantCode: StatusProtocolError,
		},
		{
			name:     "reserved bit",
			raw:      buildFrame(true, 0x2, opText, true, []byte("x")),
			wantCode: StatusProtocolError,
		},
		{
			name:     "fragmented ping",
			raw:      buildFrame(false, 0, opPing, true, []byte("x")),
			wantCode: StatusProtocolError,
		},
		{
			name:     "oversized control frame",
			raw:      buildFrame(true, 0, opPing, true, make([]byte, 126)),
			wantCode: StatusProtocolError,
		},
		{
			name:     "continuation without start",
			raw:      buildFrame(true, 0, opContinuation, true, []byte("x")),
			wantCode: StatusProtocolError,
		},
		{
			name:     "invalid UTF-8 text",
			raw:      buildFrame(true, 0, opText, true, []byte{0xff, 0xfe}),
			wantCode: StatusInvalidFramePayloadData,
		},
		{
			name:     "close with bad code",
			raw:      buildFrame(true, 0, opClose, true, []byte{0x03, 0xED}), // 1005
			wantCode: StatusProtocolError,
		},
		{
			name:     "close with invalid UTF-8 reason",
			raw:      buildFrame(true, 0, opClose, true, []byte{0x03, 0xE8, 0xff}), // 1000 + bad
			wantCode: StatusInvalidFramePayloadData,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			readErr := make(chan error, 1)
			client, _ := dial(t, readHandlerFor(t, readErr), nil)

			if _, err := client.conn.Write(tt.raw); err != nil {
				t.Fatalf("write raw frame: %v", err)
			}

			opcode, payload, err := client.readFrame()
			if err != nil {
				t.Fatalf("read close: %v", err)
			}
			if opcode != opClose {
				t.Fatalf("opcode = %#x, want close", opcode)
			}
			// The relayed close payload itself is built by the library and
			// must always be well-formed.
			code, _, perr := parseClosePayload(payload)
			if perr != nil {
				t.Fatalf("relayed close payload invalid: %v", perr)
			}
			if code != tt.wantCode {
				t.Fatalf("close code = %d, want %d", code, tt.wantCode)
			}

			select {
			case err := <-readErr:
				var pe *ProtocolError
				if !errors.As(err, &pe) {
					t.Fatalf("Read error = %v (%T), want *ProtocolError", err, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("server Read did not return after violation")
			}
		})
	}
}

// TestServerFramesNotMasked verifies the server never sets the mask bit on the
// frames it sends (RFC 6455 section 5.1): a client must reject a masked frame
// from a server, so this is a hard requirement, not a preference.
func TestServerFramesNotMasked(t *testing.T) {
	client, _ := dial(t, echoHandlerFor(t), nil)

	if err := client.writeFrame(opText, []byte("hi")); err != nil {
		t.Fatalf("client write: %v", err)
	}
	// Read the echoed frame's first two header bytes directly so we can
	// inspect the mask bit, which testClient.readFrame discards.
	var header [2]byte
	if _, err := io.ReadFull(client.br, header[:]); err != nil {
		t.Fatalf("read header: %v", err)
	}
	if header[1]&0x80 != 0 {
		t.Fatal("server frame has mask bit set, want unmasked")
	}
}

// TestPingAutoPong verifies a ping is answered with a pong carrying the same
// payload, exercised through Conn.Read rather than the parser alone.
func TestPingAutoPong(t *testing.T) {
	client, _ := dial(t, echoHandlerFor(t), nil)

	if err := client.writeFrame(opPing, []byte("beacon")); err != nil {
		t.Fatalf("ping: %v", err)
	}
	opcode, payload, err := client.readFrame()
	if err != nil {
		t.Fatalf("read pong: %v", err)
	}
	if opcode != opPong {
		t.Fatalf("opcode = %#x, want pong", opcode)
	}
	if string(payload) != "beacon" {
		t.Fatalf("pong payload = %q, want beacon", payload)
	}
}

// TestPing exercises the server-side Ping(ctx): the server pings, the client
// replies with a pong, and Ping returns once Read observes it.
func TestPing(t *testing.T) {
	pingErr := make(chan error, 1)
	client, _ := dial(t, func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(r.Context(), w, r, &AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		// Read drives control-frame handling; run it so the pong is observed.
		go c.Read(context.Background())
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		pingErr <- c.Ping(ctx)
	}, nil)

	// The server's ping arrives first; reply with a matching pong.
	opcode, payload, err := client.readFrame()
	if err != nil {
		t.Fatalf("read ping: %v", err)
	}
	if opcode != opPing {
		t.Fatalf("opcode = %#x, want ping", opcode)
	}
	if err := client.writeFrame(opPong, payload); err != nil {
		t.Fatalf("write pong: %v", err)
	}

	select {
	case err := <-pingErr:
		if err != nil {
			t.Fatalf("Ping returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ping did not return after pong")
	}
}

// TestPingContextCancel verifies Ping honours its context when no pong arrives.
func TestPingContextCancel(t *testing.T) {
	pingErr := make(chan error, 1)
	dial(t, func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(r.Context(), w, r, &AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		go c.Read(context.Background())
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		pingErr <- c.Ping(ctx) // client never pongs
	}, nil)

	select {
	case err := <-pingErr:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Ping error = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ping did not honour context deadline")
	}
}

// TestPingPongHandlers verifies the optional observation handlers fire on the
// Read goroutine for inbound ping and pong frames.
func TestPingPongHandlers(t *testing.T) {
	gotPing := make(chan []byte, 1)
	gotPong := make(chan []byte, 1)
	client, _ := dial(t, func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(r.Context(), w, r, &AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		c.SetPingHandler(func(p []byte) { gotPing <- append([]byte(nil), p...) })
		c.SetPongHandler(func(p []byte) { gotPong <- append([]byte(nil), p...) })
		for {
			if _, _, err := c.Read(context.Background()); err != nil {
				return
			}
		}
	}, nil)

	if err := client.writeFrame(opPing, []byte("p1")); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if err := client.writeFrame(opPong, []byte("p2")); err != nil {
		t.Fatalf("pong: %v", err)
	}

	select {
	case p := <-gotPing:
		if string(p) != "p1" {
			t.Fatalf("ping handler payload = %q, want p1", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ping handler not invoked")
	}
	select {
	case p := <-gotPong:
		if string(p) != "p2" {
			t.Fatalf("pong handler payload = %q, want p2", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pong handler not invoked")
	}
}

// TestFragmentedWithInterleavedControl verifies a control frame may arrive in
// the middle of a fragmented data message (RFC 6455 section 5.4) without
// corrupting reassembly: the ping is answered and the fragments still combine.
func TestFragmentedWithInterleavedControl(t *testing.T) {
	client, _ := dial(t, echoHandlerFor(t), nil)

	writeRawFragment(t, client, opText, false, []byte("foo"))
	if err := client.writeFrame(opPing, []byte("mid")); err != nil {
		t.Fatalf("interleaved ping: %v", err)
	}
	// The pong for the interleaved ping comes back first.
	opcode, payload, err := client.readFrame()
	if err != nil {
		t.Fatalf("read pong: %v", err)
	}
	if opcode != opPong || string(payload) != "mid" {
		t.Fatalf("interleaved reply = (%#x, %q), want (pong, mid)", opcode, payload)
	}

	writeRawFragment(t, client, opContinuation, true, []byte("bar"))
	opcode, payload, err = client.readFrame()
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if opcode != opText || string(payload) != "foobar" {
		t.Fatalf("reassembled = (%#x, %q), want (text, foobar)", opcode, payload)
	}
}

// TestCloseUsesErrClosed verifies a locally-initiated Close makes subsequent
// operations report ErrClosed rather than a CloseError.
func TestCloseUsesErrClosed(t *testing.T) {
	writeErr := make(chan error, 1)
	dial(t, func(w http.ResponseWriter, r *http.Request) {
		c, err := Accept(r.Context(), w, r, &AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			t.Errorf("Accept: %v", err)
			return
		}
		c.Close(StatusNormalClosure, "done")
		writeErr <- c.Write(context.Background(), MessageText, []byte("after close"))
	}, nil)

	select {
	case err := <-writeErr:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("Write after Close = %v, want ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handler never reported write result")
	}
}
