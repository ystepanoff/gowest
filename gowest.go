package gowest

import (
	"bufio"
	"errors"
	"net"
	"net/http"

	"github.com/ystepanoff/gowest/internal/utils"
)

// GetConnection upgrades an HTTP request to a WebSocket connection and returns
// the hijacked net.Conn and its buffered reader/writer.
//
// Deprecated: use Accept, which returns a *Conn with a context-first API, safe
// concurrent writes, origin checking and control-frame handling. GetConnection
// remains for backwards compatibility and performs no origin validation.
func GetConnection(
	w http.ResponseWriter,
	r *http.Request,
) (net.Conn, *bufio.ReadWriter, error) {
	if !utils.TokenPresentInString(r.Header.Get("Upgrade"), "websocket") {
		return nil, nil, errors.New("unidentified upgrade protocol")
	}
	if !utils.TokenPresentInString(r.Header.Get("Connection"), "Upgrade") {
		return nil, nil, errors.New("connection: Upgrade header expected")
	}
	key := []byte(r.Header.Get("Sec-Websocket-Key"))
	if key == nil {
		return nil, nil, errors.New("sec-Websocket-Key expected")
	}
	acceptStr := utils.WSSecKey(key)
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("unable to hijack HTTP connection")
	}
	conn, bufrw, err := hj.Hijack()
	if err != nil {
		return nil, nil, err
	}
	resp := []string{
		"HTTP/1.1 101 Switching Protocols\r\n",
		"Upgrade: websocket\r\n",
		"Connection: Upgrade\r\n",
		"Sec-Websocket-Accept: " + acceptStr + "\r\n\r\n",
	}
	for _, s := range resp {
		if _, err := bufrw.WriteString(s); err != nil {
			return nil, nil, err
		}
	}
	if err := bufrw.Flush(); err != nil {
		return nil, nil, err
	}

	return conn, bufrw, nil
}

// Read reads a complete (possibly fragmented) message from bufrw.
//
// Deprecated: use (*Conn).Read, which honours a context, handles control frames
// and reports message types. Read is retained for backwards compatibility.
func Read(bufrw *bufio.ReadWriter) ([]byte, error) {
	var message []byte
	for {
		f, err := readFrame(bufrw.Reader, 0)
		if err != nil {
			return nil, err
		}
		message = append(message, f.payload...)
		if f.fin {
			break
		}
	}
	return message, nil
}

// WriteString writes message as a single final text frame to bufrw.
//
// Deprecated: use (*Conn).Write, which serialises concurrent writes and accepts
// a context and message type. WriteString is retained for backwards
// compatibility.
func WriteString(bufrw *bufio.ReadWriter, message []byte) error {
	return writeFrame(bufrw.Writer, frame{fin: true, opcode: opText, payload: message})
}
