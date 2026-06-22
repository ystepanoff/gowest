// Package benchmarks compares gowest's server-side echo throughput against
// gorilla/websocket and coder/websocket over a real TCP loopback connection.
//
// Methodology: a single, neutral, hand-rolled WebSocket client (rawClient)
// drives every server, so the only variable between rows is the server library
// under test. Each iteration is one echo round trip — the client sends a masked
// frame, the server reads and writes it back, and the client reads the echo.
// Compression is disabled everywhere so the numbers reflect the uncompressed
// framing path the task targets.
//
// It lives in its own module (see go.mod) so the gowest library itself stays
// dependency-free; the comparison libraries are only pulled in here.
package benchmarks

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	coder "github.com/coder/websocket"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	gorilla "github.com/gorilla/websocket"
	"github.com/ystepanoff/gowest"
)

// Opcodes for frames the rawClient builds.
const (
	opText   byte = 0x1
	opBinary byte = 0x2
)

// rawClient is a minimal RFC 6455 client used to drive every server identically.
// It masks the frames it sends (as a client must) and parses the unmasked frames
// it receives.
type rawClient struct {
	conn net.Conn
	br   *bufio.Reader
}

// dialRaw performs the opening handshake against host and returns a ready client.
// The Sec-WebSocket-Accept value in the response is not validated: every server
// here is a conforming implementation, and skipping the check keeps the client
// free of per-dial crypto that has nothing to do with what we are measuring.
func dialRaw(host string) (*rawClient, error) {
	conn, err := net.Dial("tcp", host)
	if err != nil {
		return nil, err
	}
	// Disable Nagle so a small echo round trip is not delayed waiting to
	// coalesce. All servers run on net/http hijacked connections, so their TCP
	// behaviour is identical; only the client end needs tuning here.
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}

	req := "GET / HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, err
	}

	br := bufio.NewReaderSize(conn, 1<<20)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, err
		}
		if line == "\r\n" {
			break
		}
	}
	return &rawClient{conn: conn, br: br}, nil
}

// buildClientFrame assembles a single final masked frame. A fixed mask key is
// used: the server XORs it straight back out regardless of key, so a constant
// key keeps client-side masking out of the per-iteration measurement while
// still exercising the server's unmask path correctly.
func buildClientFrame(opcode byte, payload []byte) []byte {
	mask := [4]byte{0xA1, 0xB2, 0xC3, 0xD4}
	n := len(payload)

	out := make([]byte, 0, n+14)
	out = append(out, 0x80|opcode)
	switch {
	case n < 126:
		out = append(out, 0x80|byte(n))
	case n < 1<<16:
		out = append(out, 0x80|126, byte(n>>8), byte(n))
	default:
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		out = append(out, 0x80|127)
		out = append(out, ext[:]...)
	}
	out = append(out, mask[:]...)
	for i := 0; i < n; i++ {
		out = append(out, payload[i]^mask[i&3])
	}
	return out
}

// readMessage reads one full server message (across fragments, though the
// servers here echo single frames) into buf, which must be large enough for the
// payload. The decoded bytes are not inspected; only that exactly the right
// count arrives.
func (c *rawClient) readMessage(buf []byte) error {
	off := 0
	for {
		var h [2]byte
		if _, err := io.ReadFull(c.br, h[:]); err != nil {
			return err
		}
		fin := h[0]&0x80 != 0
		n := int(h[1] & 0x7f)
		switch n {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(c.br, ext[:]); err != nil {
				return err
			}
			n = int(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(c.br, ext[:]); err != nil {
				return err
			}
			n = int(binary.BigEndian.Uint64(ext[:]))
		}
		if _, err := io.ReadFull(c.br, buf[off:off+n]); err != nil {
			return err
		}
		off += n
		if fin {
			return nil
		}
	}
}

// --- servers under test -----------------------------------------------------

// gowestServer echoes every message using the gowest API under test.
func gowestServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := gowest.Accept(r.Context(), w, r, &gowest.AcceptOptions{
			OriginPatterns: []string{"*"},
		})
		if err != nil {
			return
		}
		defer c.Close(gowest.StatusNormalClosure, "")
		ctx := context.Background()
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if err := c.Write(ctx, typ, data); err != nil {
				return
			}
		}
	}))
}

var gorillaUpgrader = gorilla.Upgrader{
	CheckOrigin:       func(*http.Request) bool { return true },
	EnableCompression: false,
	ReadBufferSize:    4096,
	WriteBufferSize:   4096,
}

// gorillaServer echoes using gorilla/websocket's ReadMessage/WriteMessage.
func gorillaServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := gorillaUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			mt, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			if err := c.WriteMessage(mt, data); err != nil {
				return
			}
		}
	}))
}

// coderServer echoes using coder/websocket with compression disabled.
func coderServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := coder.Accept(w, r, &coder.AcceptOptions{
			OriginPatterns:  []string{"*"},
			CompressionMode: coder.CompressionDisabled,
		})
		if err != nil {
			return
		}
		defer c.Close(coder.StatusNormalClosure, "")
		// Raise above the largest benchmarked payload (5 MiB); coder's default
		// read limit (32 KiB) would otherwise reject the bigger echoes. gowest
		// (DefaultMaxMessageBytes, 32 MiB) and gorilla (no limit) already admit
		// them, so this keeps the three servers comparable.
		c.SetReadLimit(16 << 20)
		ctx := context.Background()
		for {
			typ, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if err := c.Write(ctx, typ, data); err != nil {
				return
			}
		}
	}))
}

// gobwasServer echoes using gobwas/ws. gobwas is deliberately low-level: it has
// no Conn type, so the handshake (ws.UpgradeHTTP, which hijacks the net/http
// connection) and the read/echo/write loop are driven directly with the wsutil
// helpers. wsutil.ReadClientData unmasks and returns the payload; the op code is
// echoed back with wsutil.WriteServerMessage. Unlike the other servers gobwas
// reads and writes straight on the net.Conn with no bufio layer, which is its
// idiomatic usage.
func gobwasServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, _, err := ws.UpgradeHTTP(r, w)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			payload, op, err := wsutil.ReadClientData(conn)
			if err != nil {
				return
			}
			if err := wsutil.WriteServerMessage(conn, op, payload); err != nil {
				return
			}
		}
	}))
}

// --- benchmark ---------------------------------------------------------------

func benchmarkEcho(b *testing.B, factory func() *httptest.Server, size int, opcode byte) {
	srv := factory()
	defer srv.Close()

	host := strings.TrimPrefix(srv.URL, "http://")
	cl, err := dialRaw(host)
	if err != nil {
		b.Fatalf("dial: %v", err)
	}
	defer cl.conn.Close()

	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i)
	}
	frame := buildClientFrame(opcode, payload)
	readBuf := make([]byte, size)

	// Warm up one round trip so the handshake and first-write costs do not land
	// inside the timed region.
	if _, err := cl.conn.Write(frame); err != nil {
		b.Fatalf("warmup write: %v", err)
	}
	if err := cl.readMessage(readBuf); err != nil {
		b.Fatalf("warmup read: %v", err)
	}

	b.SetBytes(int64(size))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := cl.conn.Write(frame); err != nil {
			b.Fatalf("write: %v", err)
		}
		if err := cl.readMessage(readBuf); err != nil {
			b.Fatalf("read: %v", err)
		}
	}
	b.StopTimer()
}

func BenchmarkEcho(b *testing.B) {
	libs := []struct {
		name    string
		factory func() *httptest.Server
	}{
		{"gowest", gowestServer},
		{"gorilla", gorillaServer},
		{"coder", coderServer},
		{"gobwas", gobwasServer},
	}
	sizes := []struct {
		name   string
		size   int
		opcode byte
	}{
		{"small_text_32B", 32, opText},
		{"medium_bin_1KiB", 1 << 10, opBinary},
		{"large_bin_64KiB", 64 << 10, opBinary},
		{"huge_bin_1MiB", 1 << 20, opBinary},
		{"huge_bin_2MiB", 2 << 20, opBinary},
		{"huge_bin_5MiB", 5 << 20, opBinary},
		{"huge_bin_10MiB", 10 << 20, opBinary},
	}
	for _, lib := range libs {
		for _, s := range sizes {
			b.Run(lib.name+"/"+s.name, func(b *testing.B) {
				benchmarkEcho(b, lib.factory, s.size, s.opcode)
			})
		}
	}
}
