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

func isControl(opcode byte) bool {
	return opcode&0x8 != 0
}

// readFrame reads and unmasks a single WebSocket frame from r. It enforces the
// structural rules RFC 6455 places on control frames and rejects reserved bits,
// returning a *CloseError with StatusProtocolError for protocol violations so
// the caller can relay the status to the peer.
func readFrame(r *bufio.Reader, maxPayload int64) (frame, error) {
	var header [2]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return frame{}, err
	}

	fin := header[0]&0x80 != 0
	if header[0]&0x70 != 0 {
		return frame{}, &CloseError{Code: StatusProtocolError, Reason: "reserved bits set"}
	}
	opcode := header[0] & 0x0f
	masked := header[1]&0x80 != 0

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
			return frame{}, &CloseError{Code: StatusProtocolError, Reason: "fragmented control frame"}
		}
		if size > maxControlPayload {
			return frame{}, &CloseError{Code: StatusProtocolError, Reason: "control frame too large"}
		}
	}
	if maxPayload > 0 && size > uint64(maxPayload) {
		return frame{}, &CloseError{Code: StatusMessageTooBig, Reason: "frame exceeds max message size"}
	}

	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(r, mask[:]); err != nil {
			return frame{}, err
		}
	}

	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return frame{}, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i&3]
		}
	}

	return frame{fin: fin, opcode: opcode, payload: payload}, nil
}

// writeFrame writes a single unmasked frame to w and flushes it. Servers must
// not mask the frames they send (RFC 6455 section 5.1), so no masking key is
// emitted.
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
