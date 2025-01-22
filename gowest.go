package gowest

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"

	"github.com/ystepanoff/gowest/internal/utils"
)

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
		return nil, nil, errors.New("Sec-Websocoket-Key expected")
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

func Read(bufrw *bufio.ReadWriter) ([]byte, error) {
	var message []byte
	for {
		frame, err := wsReadFrame(bufrw)
		if err != nil {
			return nil, err
		}
		message = append(message, frame.payload...)
		if frame.isFinal {
			break
		}
	}
	return message, nil
}

func WriteString(bufrw *bufio.ReadWriter, message []byte) error {
	frame := wsFrame{
		uint64(len(message)),
		1,
		true,
		message,
	}
	if err := wsWriteFrame(bufrw, frame); err != nil {
		return err
	}
	return nil
}

type wsFrame struct {
	length  uint64
	opCode  byte
	isFinal bool
	payload []byte
}

func wsReadFrame(bufrw *bufio.ReadWriter) (*wsFrame, error) {
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
	switch size {
	case 126:
		extra += 2
	case 127:
		extra += 8
	}
	if extra > 0 {
		header = header[:extra]
		if _, err := bufrw.Read(header); err != nil {
			return nil, err
		}
		switch size {
		case 126:
			size = uint64(binary.BigEndian.Uint16(header[:2]))
			header = header[2:]
		case 127:
			size = binary.BigEndian.Uint64(header[:8])
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

func wsWriteFrame(bufrw *bufio.ReadWriter, frame wsFrame) error {
	buf := make([]byte, 2)
	buf[0] |= frame.opCode
	if frame.isFinal {
		buf[0] |= 0x80
	}
	if frame.length < 126 {
		buf[1] |= byte(frame.length)
	} else if frame.length < 1<<16 {
		buf[1] |= 126
		size := make([]byte, 2)
		binary.BigEndian.PutUint16(size, uint16(frame.length))
		buf = append(buf, size...)
	} else {
		buf[1] |= 127
		size := make([]byte, 8)
		binary.BigEndian.PutUint64(size, frame.length)
		buf = append(buf, size...)
	}
	buf = append(buf, frame.payload...)
	if _, err := bufrw.Write(buf); err != nil {
		return err
	}
	if err := bufrw.Flush(); err != nil {
		return err
	}
	return nil
}
