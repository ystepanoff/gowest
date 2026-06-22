package gowest

import (
	"bufio"
	"encoding/binary"
	"io"
)

// WebSocket opcodes as defined in RFC 6455 section 5.2.
const (
	opContinuation byte = 0x0
	opText         byte = 0x1
	opBinary       byte = 0x2
	opClose        byte = 0x8
	opPing         byte = 0x9
	opPong         byte = 0xA
)

// maxControlPayload is the largest payload a control frame may carry (RFC 6455
// section 5.5).
const maxControlPayload = 125

// frame is a single parsed WebSocket frame.
type frame struct {
	fin     bool
	opcode  byte
	payload []byte
}

// isControl reports whether opcode denotes a control frame (0x8-0xF). Control
// frames carry the high bit of the opcode nibble.
func isControl(opcode byte) bool {
	return opcode&0x8 != 0
}

// validOpcode reports whether opcode is one of the six opcodes defined by RFC
// 6455. The reserved data opcodes 0x3-0x7 and reserved control opcodes 0xB-0xF
// must be rejected.
func validOpcode(opcode byte) bool {
	switch opcode {
	case opContinuation, opText, opBinary, opClose, opPing, opPong:
		return true
	default:
		return false
	}
}

// readFrame reads, validates and unmasks a single client-to-server WebSocket
// frame from r. Because gowest is a server, every inbound frame must be masked
// (RFC 6455 section 5.1); an unmasked frame is a protocol violation. readFrame
// also rejects reserved bits, unknown opcodes and control frames that are
// fragmented or larger than 125 bytes, and caps data-frame payloads at
// maxPayload so a peer cannot announce an enormous length to exhaust memory.
//
// Protocol violations return a *ProtocolError carrying the status code to relay
// to the peer; short reads return the underlying io error unwrapped.
//
// The fixed-size header and mask are read with Peek/Discard rather than
// io.ReadFull into a stack array: passing a stack slice to io.ReadFull's
// io.Reader argument forces the array to the heap (the compiler cannot prove
// the reader does not retain it), costing an allocation per frame. Peek returns
// a slice into the bufio buffer, so the header never leaves the reader's own
// storage and the parser allocates only the payload itself.
func readFrame(r *bufio.Reader, maxPayload int64) (frame, error) {
	// Fast path for the common short frame. A masked frame whose length fits the
	// 7-bit field occupies at least six bytes on the wire (two header + four
	// mask), so a single Peek(6) fetches the whole prefix without over-blocking;
	// extended-length frames (length byte 126 or 127) fall through to readFrameBig.
	bh, err := r.Peek(6)
	if err != nil {
		// Fewer than six bytes buffered: the stream ended or errored mid-frame,
		// or this is a tiny extended-length frame still arriving. Re-resolve via
		// the slow path, which peeks exactly what each layout needs.
		return readFrameBig(r, maxPayload)
	}
	b0, b1 := bh[0], bh[1]

	if b0&0x70 != 0 {
		return frame{}, &ProtocolError{Code: StatusProtocolError, Reason: "reserved bits set"}
	}
	opcode := b0 & 0x0f
	if !validOpcode(opcode) {
		return frame{}, &ProtocolError{Code: StatusProtocolError, Reason: "reserved opcode"}
	}
	if b1&0x80 == 0 {
		return frame{}, &ProtocolError{Code: StatusProtocolError, Reason: "client frame not masked"}
	}

	size := b1 & 0x7f
	if size >= 126 {
		return readFrameBig(r, maxPayload)
	}

	fin := b0&0x80 != 0
	if isControl(opcode) {
		// A control frame must not be fragmented. Its size is at most 125 here
		// (size < 126 on this path), so the maxControlPayload limit always holds
		// and the data-message budget never applies to it.
		if !fin {
			return frame{}, &ProtocolError{Code: StatusProtocolError, Reason: "fragmented control frame"}
		}
	} else if maxPayload > 0 && int64(size) > maxPayload {
		return frame{}, &ProtocolError{Code: StatusMessageTooBig, Reason: "frame exceeds max message size"}
	}

	var mask [4]byte
	copy(mask[:], bh[2:6])
	if _, err := r.Discard(6); err != nil {
		return frame{}, err
	}

	payload := make([]byte, size)
	if err := readMaskedPayload(r, payload, mask); err != nil {
		return frame{}, err
	}

	return frame{fin: fin, opcode: opcode, payload: payload}, nil
}

// readFrameBig handles frames whose payload length is encoded in 16 or 64 bits,
// plus any frame still arriving when the fast path's Peek(6) could not be
// satisfied. It peeks the base header, then the extended length, validating the
// announced size before consuming the mask or allocating, so a frame that lies
// about an enormous length is rejected without reading further bytes.
func readFrameBig(r *bufio.Reader, maxPayload int64) (frame, error) {
	bh, err := r.Peek(2)
	if err != nil {
		return frame{}, err
	}
	b0, b1 := bh[0], bh[1]

	fin := b0&0x80 != 0
	if b0&0x70 != 0 {
		return frame{}, &ProtocolError{Code: StatusProtocolError, Reason: "reserved bits set"}
	}
	opcode := b0 & 0x0f
	if !validOpcode(opcode) {
		return frame{}, &ProtocolError{Code: StatusProtocolError, Reason: "reserved opcode"}
	}
	if b1&0x80 == 0 {
		return frame{}, &ProtocolError{Code: StatusProtocolError, Reason: "client frame not masked"}
	}

	size := uint64(b1 & 0x7f)
	headerLen := 2
	switch size {
	case 126:
		headerLen = 4
	case 127:
		headerLen = 10
	}
	if headerLen > 2 {
		hh, err := r.Peek(headerLen)
		if err != nil {
			return frame{}, err
		}
		if size == 126 {
			size = uint64(binary.BigEndian.Uint16(hh[2:4]))
		} else {
			size = binary.BigEndian.Uint64(hh[2:10])
		}
	}

	// Validate the announced size before consuming the mask or allocating the
	// payload, so a frame that lies about its length is rejected cheaply.
	if isControl(opcode) {
		if !fin {
			return frame{}, &ProtocolError{Code: StatusProtocolError, Reason: "fragmented control frame"}
		}
		if size > maxControlPayload {
			return frame{}, &ProtocolError{Code: StatusProtocolError, Reason: "control frame too large"}
		}
	} else if maxPayload > 0 && size > uint64(maxPayload) {
		// Reject before allocating: a single data frame must not exceed the
		// message budget. Control frames are already bounded above.
		return frame{}, &ProtocolError{Code: StatusMessageTooBig, Reason: "frame exceeds max message size"}
	}

	// Peek the mask, which immediately follows the (possibly extended) header.
	mh, err := r.Peek(headerLen + 4)
	if err != nil {
		return frame{}, err
	}
	var mask [4]byte
	copy(mask[:], mh[headerLen:headerLen+4])
	if _, err := r.Discard(headerLen + 4); err != nil {
		return frame{}, err
	}

	payload := make([]byte, size)
	if err := readMaskedPayload(r, payload, mask); err != nil {
		return frame{}, err
	}

	return frame{fin: fin, opcode: opcode, payload: payload}, nil
}

// readMaskedPayload fills buf from r and unmasks it in a single pass: each chunk
// the reader returns is unmasked while it is still hot in cache, before the next
// chunk is read. Unmasking buf as a whole only after a separate io.ReadFull
// (the obvious two-pass form) touches the payload twice, which for payloads too
// large to stay cached doubles the memory traffic and shows up as a regression
// in the bandwidth-bound regime (≈5 MiB+). Fusing the two keeps a single pass.
//
// The reader (a *bufio.Reader) hands back whatever it has buffered, so chunk
// boundaries do not align to the four-byte mask period; pos tracks the running
// offset so each chunk resumes the mask at the correct phase. The io.ReadFull
// error contract is preserved: a short final read returns io.ErrUnexpectedEOF.
func readMaskedPayload(r io.Reader, buf []byte, mask [4]byte) error {
	pos := 0
	for pos < len(buf) {
		n, err := r.Read(buf[pos:])
		if n > 0 {
			maskBytesOffset(buf[pos:pos+n], mask, pos)
			pos += n
		}
		if err != nil {
			if err == io.EOF {
				if pos == len(buf) {
					return nil
				}
				return io.ErrUnexpectedEOF
			}
			return err
		}
	}
	return nil
}

// maskBytesOffset XORs b in place with the four-byte WebSocket mask, where b
// begins offset bytes into the payload. It first rotates the mask to the phase
// implied by offset, then XORs eight bytes per iteration: the rotated mask is
// repeated into an eight-byte word and applied with aligned 64-bit loads/stores
// via encoding/binary, which the compiler lowers to plain word operations. This
// is several times faster than a byte-at-a-time loop on large payloads and needs
// no unsafe: LittleEndian is used for both the load and the store, so the byte
// permutation cancels out and the result is identical on every architecture.
//
// Each eight-byte iteration consumes two full four-byte mask cycles, so the
// phase is preserved and the byte-wise tail resumes at the rotated mask[0]. The
// offset lets a payload be unmasked across several non-aligned chunks (as the
// fused read loop does) while keeping every byte XORed against the right mask
// byte: mask[(offset+i) & 3] for the i-th payload byte.
func maskBytesOffset(b []byte, mask [4]byte, offset int) {
	var rm [4]byte
	rm[0] = mask[offset&3]
	rm[1] = mask[(offset+1)&3]
	rm[2] = mask[(offset+2)&3]
	rm[3] = mask[(offset+3)&3]

	if len(b) >= 8 {
		var m [8]byte
		m[0], m[1], m[2], m[3] = rm[0], rm[1], rm[2], rm[3]
		m[4], m[5], m[6], m[7] = rm[0], rm[1], rm[2], rm[3]
		mw := binary.LittleEndian.Uint64(m[:])
		for len(b) >= 8 {
			v := binary.LittleEndian.Uint64(b)
			binary.LittleEndian.PutUint64(b, v^mw)
			b = b[8:]
		}
	}
	for i := range b {
		b[i] ^= rm[i&3]
	}
}

// writeFrame writes a single unmasked frame to w and flushes it. Servers must
// not mask the frames they send (RFC 6455 section 5.1), so no masking key is
// emitted. Callers are responsible for honouring the 125-byte control-frame
// limit; control payloads built within this package are bounded at their source.
//
// The header is emitted with WriteByte rather than assembling it in a stack
// array and calling w.Write(header[:n]): bufio.Writer.Write may forward its
// argument to the underlying io.Writer, so a stack-allocated header escapes to
// the heap and costs an allocation on every frame. WriteByte writes into the
// bufio buffer directly and never lets the header reach an interface. The
// payload is written separately, so header and payload are never concatenated
// into a fresh allocation.
func writeFrame(w *bufio.Writer, f frame) error {
	b0 := f.opcode
	if f.fin {
		b0 |= 0x80
	}
	if err := w.WriteByte(b0); err != nil {
		return err
	}

	length := len(f.payload)
	switch {
	case length < 126:
		if err := w.WriteByte(byte(length)); err != nil {
			return err
		}
	case length < 1<<16:
		if err := w.WriteByte(126); err != nil {
			return err
		}
		if err := w.WriteByte(byte(length >> 8)); err != nil {
			return err
		}
		if err := w.WriteByte(byte(length)); err != nil {
			return err
		}
	default:
		if err := w.WriteByte(127); err != nil {
			return err
		}
		for shift := 56; shift >= 0; shift -= 8 {
			if err := w.WriteByte(byte(uint64(length) >> shift)); err != nil {
				return err
			}
		}
	}

	if _, err := w.Write(f.payload); err != nil {
		return err
	}
	return w.Flush()
}
