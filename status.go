package gowest

import (
	"encoding/binary"
	"fmt"
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
	StatusTLSHandshake            StatusCode = 1015
)

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

// closePayload encodes a close status code and reason into a control frame
// payload. A zero code produces an empty payload, which signals "no status".
func closePayload(code StatusCode, reason string) []byte {
	if code == 0 {
		return nil
	}
	buf := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(buf[:2], uint16(code))
	copy(buf[2:], reason)
	return buf
}

// parseClosePayload decodes a close control frame payload. A payload shorter
// than two bytes is reported as StatusNoStatusReceived.
func parseClosePayload(payload []byte) (StatusCode, string) {
	if len(payload) < 2 {
		return StatusNoStatusReceived, ""
	}
	return StatusCode(binary.BigEndian.Uint16(payload[:2])), string(payload[2:])
}
