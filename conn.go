package gowest

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
)

var wsGUID = []byte("258EAFA5-E914-47DA-95CA-C5AB0DC85B11")

func wsSecKey(key []byte) string {
	sha := sha1.New()
	sha.Write(key)
	sha.Write(wsGUID)
	return base64.StdEncoding.EncodeToString(sha.Sum(nil))
}

func tokenPresentInString(s string, t string) bool {
	tokens := strings.Fields(s)
	for _, token := range tokens {
		if t == token {
			return true
		}
	}
	return false
}

func GetConnection(w http.ResponseWriter, r *http.Request) (net.Conn, *bufio.ReadWriter, error) {
	if !tokenPresentInString(r.Header.Get("Upgrade"), "websocket") {
		return nil, nil, errors.New("Unidentified upgrade protocol")
	}
	if !tokenPresentInString(r.Header.Get("Connection"), "Upgrade") {
		return nil, nil, errors.New("Connection: Upgrade header expected")
	}
	key := []byte(r.Header.Get("Sec-Websocket-Key"))
	if key == nil {
		return nil, nil, errors.New("Sec-Websocoket-Key expected")
	}
	acceptStr := wsSecKey(key)
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("Unable to hijack HTTP connection")
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
	for _, h := range resp {
		if _, err := bufrw.WriteString(h); err != nil {
			return nil, nil, err
		}
	}
	if err := bufrw.Flush(); err != nil {
		return nil, nil, err
	}

	return conn, bufrw, nil
}

func Read(bufrw bufio.ReadWriter) ([]byte, bool) {
	var message []byte
	for {
		frame, err := wsReadFrame(bufrw)
		if err != nil {
			return nil, false
		}
		message = append(message, frame.payload...)
		if frame.isFinal {
			break
		}
	}
	return message, true
}

type wsFrame struct {
	length  uint64
	opCode  byte
	isFinal bool
	payload []byte
}

func wsReadFrame(bufrw bufio.ReadWriter) (*wsFrame, error) {
	header := make([]byte, 2, 12)
	if _, err := bufrw.Read(header); err != nil {
		return nil, err
	}
	finalBit := header[0] >> 7
	opCode := header[0] & 0xf
	maskBit := header[1] >> 7
	extra := 0
	if maskBit == 1 {
		extra += 4
	}
	size := uint64(header[1] & 0x7f)
	if size == 126 {
		extra += 2
	} else if size == 127 {
		extra += 8
	}
	if extra > 0 {
		header = header[:extra]
		if _, err := bufrw.Read(header); err != nil {
			return nil, err
		}
		if size == 126 {
			size = uint64(binary.BigEndian.Uint16(header[:2]))
			header = header[2:]
		} else if size == 127 {
			size = uint64(binary.BigEndian.Uint64(header[:8]))
			header = header[8:]
		}
	}
	var mask []byte
	if maskBit == 1 {
		mask = header
	}
	payload := make([]byte, int(size))
	if _, err := io.ReadFull(bufrw, payload); err != nil {
		return nil, err
	}
	if maskBit == 1 {
		for i := 0; i < len(payload); i++ {
			payload[i] ^= mask[i%4]
		}
	}
	frame := &wsFrame{}
	frame.length = size
	frame.opCode = opCode
	if finalBit == 1 {
		frame.isFinal = true
	} else {
		frame.isFinal = false
	}
	frame.payload = payload
	return frame, nil
}
