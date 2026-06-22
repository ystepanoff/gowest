package gowest

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestMaskBytesOffset verifies the phase-aware unmask matches the naive
// per-byte reference for every starting offset phase and a range of lengths,
// including the sub-8-byte tail and the 8-byte fast-loop body. This guards the
// fused single-pass read, which unmasks chunks that begin at arbitrary,
// non-four-byte-aligned offsets.
func TestMaskBytesOffset(t *testing.T) {
	mask := [4]byte{0x11, 0x22, 0x33, 0x44}
	for offset := 0; offset < 8; offset++ {
		for n := 0; n <= 40; n++ {
			data := make([]byte, n)
			for i := range data {
				data[i] = byte(i * 7)
			}
			got := append([]byte(nil), data...)
			maskBytesOffset(got, mask, offset)

			want := append([]byte(nil), data...)
			for i := range want {
				want[i] ^= mask[(offset+i)&3]
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("offset=%d n=%d: got %x, want %x", offset, n, got, want)
			}
		}
	}
}

// chunkReader returns data in fixed-size chunks (the final chunk may be
// shorter), forcing readMaskedPayload through several non-aligned reads so the
// per-chunk mask phase is exercised. A chunk size that is not a multiple of four
// guarantees chunk boundaries fall at every mask phase.
type chunkReader struct {
	data  []byte
	chunk int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := r.chunk
	if n > len(p) {
		n = len(p)
	}
	if n > len(r.data) {
		n = len(r.data)
	}
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

// TestReadMaskedPayloadChunked confirms the fused read unmasks correctly when
// the payload arrives in many small, non-four-byte-aligned chunks, and that a
// truncated stream yields io.ErrUnexpectedEOF (io.ReadFull's contract).
func TestReadMaskedPayloadChunked(t *testing.T) {
	mask := [4]byte{0xDE, 0xAD, 0xBE, 0xEF}
	for _, chunk := range []int{1, 3, 5, 7, 13, 1024} {
		plain := make([]byte, 1000)
		for i := range plain {
			plain[i] = byte(i)
		}
		masked := append([]byte(nil), plain...)
		for i := range masked {
			masked[i] ^= mask[i&3]
		}

		buf := make([]byte, len(plain))
		if err := readMaskedPayload(&chunkReader{data: masked, chunk: chunk}, buf, mask); err != nil {
			t.Fatalf("chunk=%d: %v", chunk, err)
		}
		if !bytes.Equal(buf, plain) {
			t.Fatalf("chunk=%d: unmasked mismatch", chunk)
		}
	}

	// A short stream must report io.ErrUnexpectedEOF, not a successful read.
	buf := make([]byte, 100)
	short := make([]byte, 40)
	if err := readMaskedPayload(&chunkReader{data: short, chunk: 7}, buf, mask); err != io.ErrUnexpectedEOF {
		t.Fatalf("short read err = %v, want io.ErrUnexpectedEOF", err)
	}
}

// buildFrame assembles a raw WebSocket frame with full control over every
// header field so tests can construct frames the well-behaved testClient never
// would: unmasked client frames, reserved opcodes, fragmented control frames,
// and so on. When mask is true the payload is masked with a fixed key and the
// mask bit is set.
func buildFrame(fin bool, rsv byte, opcode byte, mask bool, payload []byte) []byte {
	var b0 byte = opcode | (rsv << 4)
	if fin {
		b0 |= 0x80
	}

	var out []byte
	out = append(out, b0)

	length := len(payload)
	var lenByte byte
	switch {
	case length < 126:
		lenByte = byte(length)
	case length < 1<<16:
		lenByte = 126
	default:
		lenByte = 127
	}
	if mask {
		lenByte |= 0x80
	}
	out = append(out, lenByte)

	switch {
	case length >= 1<<16:
		var ext [8]byte
		for i := 0; i < 8; i++ {
			ext[7-i] = byte(length >> (8 * i))
		}
		out = append(out, ext[:]...)
	case length >= 126:
		out = append(out, byte(length>>8), byte(length))
	}

	if mask {
		key := [4]byte{0x12, 0x34, 0x56, 0x78}
		out = append(out, key[:]...)
		masked := make([]byte, length)
		for i := range payload {
			masked[i] = payload[i] ^ key[i&3]
		}
		out = append(out, masked...)
	} else {
		out = append(out, payload...)
	}
	return out
}

func readerOf(b []byte) *bufio.Reader {
	return bufio.NewReader(strings.NewReader(string(b)))
}

// TestReadFrameValidation drives readFrame directly across the structural edge
// cases RFC 6455 places on a single frame.
func TestReadFrameValidation(t *testing.T) {
	const maxPayload = 16

	tests := []struct {
		name       string
		raw        []byte
		wantErr    bool
		wantCode   StatusCode // checked only when wantErr
		wantOpcode byte       // checked only when !wantErr
		wantFin    bool       // checked only when !wantErr
	}{
		{
			name:       "valid masked text",
			raw:        buildFrame(true, 0, opText, true, []byte("hi")),
			wantOpcode: opText,
			wantFin:    true,
		},
		{
			name:       "valid non-final fragment",
			raw:        buildFrame(false, 0, opText, true, []byte("hi")),
			wantOpcode: opText,
			wantFin:    false,
		},
		{
			name:     "unmasked client frame rejected",
			raw:      buildFrame(true, 0, opText, false, []byte("hi")),
			wantErr:  true,
			wantCode: StatusProtocolError,
		},
		{
			name:     "reserved bit set",
			raw:      buildFrame(true, 0x4, opText, true, []byte("hi")),
			wantErr:  true,
			wantCode: StatusProtocolError,
		},
		{
			name:     "reserved data opcode 0x3",
			raw:      buildFrame(true, 0, 0x3, true, nil),
			wantErr:  true,
			wantCode: StatusProtocolError,
		},
		{
			name:     "reserved control opcode 0xB",
			raw:      buildFrame(true, 0, 0xB, true, nil),
			wantErr:  true,
			wantCode: StatusProtocolError,
		},
		{
			name:     "fragmented control frame (ping, fin=0)",
			raw:      buildFrame(false, 0, opPing, true, []byte("x")),
			wantErr:  true,
			wantCode: StatusProtocolError,
		},
		{
			name:     "control frame payload over 125 bytes",
			raw:      buildFrame(true, 0, opPing, true, make([]byte, 126)),
			wantErr:  true,
			wantCode: StatusProtocolError,
		},
		{
			name:       "control frame at exactly 125 bytes",
			raw:        buildFrame(true, 0, opPing, true, make([]byte, 125)),
			wantOpcode: opPing,
			wantFin:    true,
		},
		{
			name:     "data frame exceeds max payload",
			raw:      buildFrame(true, 0, opBinary, true, make([]byte, maxPayload+1)),
			wantErr:  true,
			wantCode: StatusMessageTooBig,
		},
		{
			name:       "data frame at exactly max payload",
			raw:        buildFrame(true, 0, opBinary, true, make([]byte, maxPayload)),
			wantOpcode: opBinary,
			wantFin:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := readFrame(readerOf(tt.raw), maxPayload)
			if tt.wantErr {
				var pe *ProtocolError
				if !errors.As(err, &pe) {
					t.Fatalf("err = %v (%T), want *ProtocolError", err, err)
				}
				if pe.Code != tt.wantCode {
					t.Fatalf("code = %d, want %d", pe.Code, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if f.opcode != tt.wantOpcode {
				t.Fatalf("opcode = %#x, want %#x", f.opcode, tt.wantOpcode)
			}
			if f.fin != tt.wantFin {
				t.Fatalf("fin = %v, want %v", f.fin, tt.wantFin)
			}
		})
	}
}

// TestReadFrameUnmasksPayload confirms the parser reverses the client mask so
// the returned payload is the plaintext.
func TestReadFrameUnmasksPayload(t *testing.T) {
	raw := buildFrame(true, 0, opBinary, true, []byte("payload"))
	f, err := readFrame(readerOf(raw), 0)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if string(f.payload) != "payload" {
		t.Fatalf("payload = %q, want %q", f.payload, "payload")
	}
}

// TestReadFrameNoMaxPayload confirms that a non-positive maxPayload disables
// the data-frame size cap (control limits still apply).
func TestReadFrameNoMaxPayload(t *testing.T) {
	raw := buildFrame(true, 0, opBinary, true, make([]byte, 5000))
	f, err := readFrame(readerOf(raw), 0)
	if err != nil {
		t.Fatalf("readFrame: %v", err)
	}
	if len(f.payload) != 5000 {
		t.Fatalf("len = %d, want 5000", len(f.payload))
	}
}

// TestParseClosePayload covers close-frame status-code and reason decoding,
// including the protocol violations RFC 6455 requires a receiver to detect.
func TestParseClosePayload(t *testing.T) {
	tests := []struct {
		name       string
		payload    []byte
		wantCode   StatusCode
		wantReason string
		wantErr    bool
		wantErrCD  StatusCode // expected ProtocolError.Code when wantErr
	}{
		{
			name:     "empty payload is no-status",
			payload:  nil,
			wantCode: StatusNoStatusReceived,
		},
		{
			name:       "code and reason",
			payload:    closePayload(StatusNormalClosure, "bye"),
			wantCode:   StatusNormalClosure,
			wantReason: "bye",
		},
		{
			name:      "single byte is malformed",
			payload:   []byte{0x03},
			wantErr:   true,
			wantErrCD: StatusProtocolError,
		},
		{
			name:      "reserved code 1005 rejected",
			payload:   []byte{0x03, 0xED}, // 1005
			wantErr:   true,
			wantErrCD: StatusProtocolError,
		},
		{
			name:      "reserved code 1006 rejected",
			payload:   []byte{0x03, 0xEE}, // 1006
			wantErr:   true,
			wantErrCD: StatusProtocolError,
		},
		{
			name:      "unregistered code 1016 rejected",
			payload:   []byte{0x03, 0xF8}, // 1016
			wantErr:   true,
			wantErrCD: StatusProtocolError,
		},
		{
			name:      "code 2999 rejected",
			payload:   []byte{0x0B, 0xB7}, // 2999
			wantErr:   true,
			wantErrCD: StatusProtocolError,
		},
		{
			name:     "application code 3000 accepted",
			payload:  []byte{0x0B, 0xB8}, // 3000
			wantCode: 3000,
		},
		{
			name:     "application code 4999 accepted",
			payload:  []byte{0x13, 0x87}, // 4999
			wantCode: 4999,
		},
		{
			name:      "code 5000 rejected",
			payload:   []byte{0x13, 0x88}, // 5000
			wantErr:   true,
			wantErrCD: StatusProtocolError,
		},
		{
			name:      "invalid UTF-8 reason rejected",
			payload:   []byte{0x03, 0xE8, 0xff, 0xfe}, // 1000 + bad bytes
			wantErr:   true,
			wantErrCD: StatusInvalidFramePayloadData,
		},
		{
			name:     "1014 bad gateway accepted",
			payload:  []byte{0x03, 0xF6}, // 1014
			wantCode: StatusBadGateway,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, reason, err := parseClosePayload(tt.payload)
			if tt.wantErr {
				var pe *ProtocolError
				if !errors.As(err, &pe) {
					t.Fatalf("err = %v (%T), want *ProtocolError", err, err)
				}
				if pe.Code != tt.wantErrCD {
					t.Fatalf("err code = %d, want %d", pe.Code, tt.wantErrCD)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if code != tt.wantCode {
				t.Fatalf("code = %d, want %d", code, tt.wantCode)
			}
			if reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tt.wantReason)
			}
		})
	}
}

// TestClosePayloadSuppressesReservedCodes confirms reserved "no wire" codes
// produce an empty payload so they never reach the peer.
func TestClosePayloadSuppressesReservedCodes(t *testing.T) {
	for _, code := range []StatusCode{0, StatusNoStatusReceived, StatusAbnormalClosure, StatusTLSHandshake} {
		if p := closePayload(code, "reason"); p != nil {
			t.Fatalf("closePayload(%d) = %v, want nil", code, p)
		}
	}
	if p := closePayload(StatusNormalClosure, ""); len(p) != 2 {
		t.Fatalf("closePayload(1000,\"\") len = %d, want 2", len(p))
	}
}
