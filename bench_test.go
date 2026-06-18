package gowest

import (
	"bufio"
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// These in-package benchmarks isolate gowest's framing cost from the operating
// system socket. Inbound bytes are served from an in-memory stream and outbound
// bytes are discarded, so the numbers reflect header encode/decode, the unmask
// loop and per-call allocation rather than kernel I/O. Run with -benchmem to
// see allocations per Read and per Write.

// fakeAddr is a stub net.Addr for the in-memory connection.
type fakeAddr struct{}

func (fakeAddr) Network() string { return "fake" }
func (fakeAddr) String() string  { return "fake" }

// fakeConn is an in-memory net.Conn whose reads come from rd and whose writes
// are discarded. Deadline setters are no-ops: the benchmarks drive the hot path
// with context.Background(), so no real deadline is ever needed, and skipping
// the syscall keeps the measurement on gowest's own work.
type fakeConn struct {
	rd io.Reader
}

func (c *fakeConn) Read(b []byte) (int, error)       { return c.rd.Read(b) }
func (c *fakeConn) Write(b []byte) (int, error)      { return len(b), nil }
func (c *fakeConn) Close() error                     { return nil }
func (c *fakeConn) LocalAddr() net.Addr              { return fakeAddr{} }
func (c *fakeConn) RemoteAddr() net.Addr             { return fakeAddr{} }
func (c *fakeConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }

// repeatReader yields buf over and over as an endless byte stream, so a single
// pre-built frame can feed an unbounded number of Read calls without ever
// allocating or returning io.EOF.
type repeatReader struct {
	buf []byte
	pos int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if len(r.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.buf[r.pos:])
	r.pos += n
	if r.pos == len(r.buf) {
		r.pos = 0
	}
	return n, nil
}

// newReadConn returns a Conn whose Read consumes an endless repetition of a
// single masked client frame carrying a payload of the given size.
func newReadConn(size int, typ MessageType) *Conn {
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i)
	}
	raw := buildFrame(true, 0, typ.opcode(), true, payload)
	rr := &repeatReader{buf: raw}
	fc := &fakeConn{rd: rr}
	return newConn(fc, bufio.NewReaderSize(fc, 4096), bufio.NewWriterSize(fc, 4096), "", DefaultMaxMessageBytes)
}

// newWriteConn returns a Conn whose writes are discarded, for measuring the
// send path in isolation.
func newWriteConn() *Conn {
	fc := &fakeConn{rd: &repeatReader{}}
	return newConn(fc, bufio.NewReaderSize(fc, 4096), bufio.NewWriterSize(fc, 4096), "", DefaultMaxMessageBytes)
}

// payloadSizes are the message sizes exercised across the read/write benchmarks:
// a small text control message, and binary payloads spanning the 7-bit, 16-bit
// and 64-bit frame-length encodings.
var payloadSizes = []struct {
	name string
	size int
	typ  MessageType
}{
	{"small_text_32B", 32, MessageText},
	{"medium_bin_1KiB", 1 << 10, MessageBinary},
	{"large_bin_64KiB", 64 << 10, MessageBinary},
	{"huge_bin_1MiB", 1 << 20, MessageBinary},
	{"huge_bin_2MiB", 2 << 20, MessageBinary},
	{"huge_bin_5MiB", 5 << 20, MessageBinary},
}

// BenchmarkReadFrame isolates the frame parser (header decode + unmask) from the
// message-reassembly layer in Conn.Read.
func BenchmarkReadFrame(b *testing.B) {
	for _, ps := range payloadSizes {
		b.Run(ps.name, func(b *testing.B) {
			payload := make([]byte, ps.size)
			raw := buildFrame(true, 0, ps.typ.opcode(), true, payload)
			br := bufio.NewReaderSize(&fakeConn{rd: &repeatReader{buf: raw}}, 4096)
			b.SetBytes(int64(ps.size))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := readFrame(br, DefaultMaxMessageBytes); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkConnRead measures a full (*Conn).Read: parse, unmask, reassemble and,
// for text, UTF-8 validate. This is the read hot path an application sees.
func BenchmarkConnRead(b *testing.B) {
	for _, ps := range payloadSizes {
		b.Run(ps.name, func(b *testing.B) {
			conn := newReadConn(ps.size, ps.typ)
			ctx := context.Background()
			b.SetBytes(int64(ps.size))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, _, err := conn.Read(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkConnWrite measures a full (*Conn).Write: header encode plus emitting
// header and payload to the buffered writer under the write mutex.
func BenchmarkConnWrite(b *testing.B) {
	for _, ps := range payloadSizes {
		b.Run(ps.name, func(b *testing.B) {
			conn := newWriteConn()
			payload := make([]byte, ps.size)
			ctx := context.Background()
			b.SetBytes(int64(ps.size))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := conn.Write(ctx, ps.typ, payload); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkConcurrentWrite measures write throughput when many goroutines share
// one Conn, contending for the write mutex. The payload is 1 KiB, a typical
// application message.
func BenchmarkConcurrentWrite(b *testing.B) {
	for _, writers := range []int{8, 32, 128} {
		b.Run(benchName(writers), func(b *testing.B) {
			conn := newWriteConn()
			payload := make([]byte, 1<<10)
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			b.ResetTimer()

			remaining := int64(b.N)
			done := make(chan struct{}, writers)
			for w := 0; w < writers; w++ {
				go func() {
					ctx := context.Background()
					for atomic.AddInt64(&remaining, -1) >= 0 {
						if err := conn.Write(ctx, MessageBinary, payload); err != nil {
							b.Error(err)
							break
						}
					}
					done <- struct{}{}
				}()
			}
			for w := 0; w < writers; w++ {
				<-done
			}
		})
	}
}

func benchName(writers int) string {
	switch writers {
	case 8:
		return "writers_8"
	case 32:
		return "writers_32"
	default:
		return "writers_128"
	}
}
