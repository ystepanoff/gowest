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
func readFrame(r *bufio.Reader, maxPayload int64) (frame, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return frame{}, err
	}

	fin := header[0]&0x80 != 0
	if header[0]&0x70 != 0 {
		return frame{}, &ProtocolError{Code: StatusProtocolError, Reason: "reserved bits set"}
	}
	opcode := header[0] & 0x0f
	if !validOpcode(opcode) {
		return frame{}, &ProtocolError{Code: StatusProtocolError, Reason: "reserved opcode"}
	}
	masked := header[1]&0x80 != 0
	if !masked {
		return frame{}, &ProtocolError{Code: StatusProtocolError, Reason: "client frame not masked"}
	}

	size := uint64(header[1] & 0x7f)
	switch size {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return frame{}, err
		}
		size = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return frame{}, err
		}
		size = binary.BigEndian.Uint64(ext[:])
	}

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

	var mask [4]byte
	if _, err := io.ReadFull(r, mask[:]); err != nil {
		return frame{}, err
	}

	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return frame{}, err
	}
	for i := range payload {
		payload[i] ^= mask[i&3]
	}

	return frame{fin: fin, opcode: opcode, payload: payload}, nil
}

// writeFrame writes a single unmasked frame to w and flushes it. Servers must
// not mask the frames they send (RFC 6455 section 5.1), so no masking key is
// emitted. Callers are responsible for honouring the 125-byte control-frame
// limit; control payloads built within this package are bounded at their source.
func writeFrame(w *bufio.Writer, f frame) error {
	var header [10]byte
	header[0] = f.opcode
	if f.fin {
		header[0] |= 0x80
	}

	n := 2
	length := len(f.payload)
	switch {
	case length < 126:
		header[1] = byte(length)
	case length < 1<<16:
		header[1] = 126
		binary.BigEndian.PutUint16(header[2:4], uint16(length))
		n = 4
	default:
		header[1] = 127
		binary.BigEndian.PutUint64(header[2:10], uint64(length))
		n = 10
	}

	if _, err := w.Write(header[:n]); err != nil {
		return err
	}
	if _, err := w.Write(f.payload); err != nil {
		return err
	}
	return w.Flush()
}
