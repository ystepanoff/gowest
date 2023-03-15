package gowest

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
)

var wsGUID = []byte("258EAFA5-E914-47DA-95CA-C5AB0DC85B11")

func wsSecKey(key []byte) string {
	sha := sha1.New()
	sha.Write(key)
	sha.Write(wsGUID)
	return base64.StdEncoding.EncodeToString(sha.Sum(nil))
}

func wsUpgrade(w http.ResponseWriter, r *http.Request) (net.Conn, *bufio.ReadWriter, error) {
	if r.Header.Get("Upgrade") != "websocket" {
		return nil, nil, errors.New("Unidentified upgrade protocol")
	}
	if r.Header.Get("Connection") != "Upgrade" {
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
	defer conn.Close()

	bufrw.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	bufrw.WriteString("Upgrade: websocket\r\n")
	bufrw.WriteString("Connection: Upgrade\r\n")
	bufrw.WriteString("Sec-Websocket-Accept: " + acceptStr + "\r\n\r\n")
	bufrw.Flush()

	return conn, bufrw, nil
}
