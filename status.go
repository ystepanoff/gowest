package gowest

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unicode/utf8"
)

// StatusCode is a WebSocket close status code as defined in RFC 6455 section
// 7.4.
type StatusCode int

// WebSocket close status codes defined by RFC 6455. Codes 1005, 1006 and 1015
// are reserved: they must never be sent on the wire and are only used to
// describe a connection that closed without a status code.
const (
	StatusNormalClosure           StatusCode = 1000
	StatusGoingAway               StatusCode = 1001
	StatusProtocolError           StatusCode = 1002
	StatusUnsupportedData         StatusCode = 1003
	StatusNoStatusReceived        StatusCode = 1005
	StatusAbnormalClosure         StatusCode = 1006
	StatusInvalidFramePayloadData StatusCode = 1007
	StatusPolicyViolation         StatusCode = 1008
	StatusMessageTooBig           StatusCode = 1009
	StatusMandatoryExtension      StatusCode = 1010
	StatusInternalError           StatusCode = 1011
	StatusServiceRestart          StatusCode = 1012
	StatusTryAgainLater           StatusCode = 1013
	StatusBadGateway              StatusCode = 1014
	StatusTLSHandshake            StatusCode = 1015
)

// ErrClosed is returned by Read, Write and Ping when the connection has already
// been closed locally (for example by a prior call to Close). A connection
// closed by the peer reports a *CloseError instead. Test for it with
// errors.Is(err, ErrClosed).
var ErrClosed = errors.New("gowest: connection closed")

// ProtocolError describes a violation of the WebSocket framing protocol
// detected while reading from the peer, such as an unmasked client frame, an
// unknown opcode or an oversized control frame. The connection is failed and a
// close frame carrying Code is sent to the peer.
//
// Inspect it with errors.As. Code is the RFC 6455 status code relayed to the
// peer (most commonly StatusProtocolError or StatusInvalidFramePayloadData).
type ProtocolError struct {
	Code   StatusCode
	Reason string
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("gowest: protocol error (%d): %s", e.Code, e.Reason)
}

// CloseError is returned by (*Conn).Read when the peer sends a close frame, and
// is the cause reported by subsequent operations on a connection closed by the
// peer. Use errors.As to inspect the status code and reason.
type CloseError struct {
	Code   StatusCode
	Reason string
}

func (e *CloseError) Error() string {
	return fmt.Sprintf("gowest: connection closed with code %d: %q", e.Code, e.Reason)
}

// validProtocolCode reports whether code is a status code a peer is permitted
// to send in a close frame, per RFC 6455 section 7.4.1. The reserved codes
// 1005, 1006 and 1015 must never appear on the wire; codes in the registered
// range 1000-1014 (excluding those) and the application range 3000-4999 are
// accepted. The unregistered 1016-2999 range is rejected.
func validProtocolCode(code StatusCode) bool {
	switch code {
	case StatusNoStatusReceived, StatusAbnormalClosure, StatusTLSHandshake:
		return false
	}
	switch {
	case code >= 1000 && code <= 1014:
		return true
	case code >= 3000 && code <= 4999:
		return true
	default:
		return false
	}
}

// closePayload encodes a close status code and reason into a control frame
// payload. A zero code, or one of the reserved "no code on the wire" codes
// (1005/1006/1015), produces an empty payload, which signals "no status".
func closePayload(code StatusCode, reason string) []byte {
	switch code {
	case 0, StatusNoStatusReceived, StatusAbnormalClosure, StatusTLSHandshake:
		return nil
	}
	buf := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(buf[:2], uint16(code))
	copy(buf[2:], reason)
	return buf
}

// parseClosePayload decodes and validates a close control frame payload. An
// empty payload is reported as StatusNoStatusReceived with no error. A
// one-byte payload, an invalid status code, or a reason that is not valid
// UTF-8 is a protocol violation and returns a *ProtocolError carrying the
// status code that must be relayed to the peer (RFC 6455 sections 5.5.1 and
// 7.1.6).
func parseClosePayload(payload []byte) (StatusCode, string, error) {
	if len(payload) == 0 {
		return StatusNoStatusReceived, "", nil
	}
	if len(payload) == 1 {
		return 0, "", &ProtocolError{Code: StatusProtocolError, Reason: "invalid close payload length"}
	}
	code := StatusCode(binary.BigEndian.Uint16(payload[:2]))
	if !validProtocolCode(code) {
		return 0, "", &ProtocolError{Code: StatusProtocolError, Reason: "invalid close code"}
	}
	reason := payload[2:]
	if !utf8.Valid(reason) {
		return 0, "", &ProtocolError{Code: StatusInvalidFramePayloadData, Reason: "close reason is not valid UTF-8"}
	}
	return code, string(reason), nil
}
