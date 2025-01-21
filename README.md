# gowest

A lightweight Go WebSocket library that offers fine-grained control over the WebSocket handshake and frame parsing.

![GoWest](GoWest.png)

## Overview
**gowest** is a simple Go library that provides a low-level interface for creating and handling WebSocket connections. Rather than relying on a higher-level library, **gowest** aims to give you control over:

* The initial WebSocket handshake and headers
* Reading and writing raw WebSocket frames
* Handling the basic life cycle of a WebSocket connection

## Features

- [x] Handshake: Manually handles WebSocket upgrade, including necessary headers.
- [x] Frame Parsing: Reads and writes WebSocket frames in compliance with RFC 6455.
- [x] Binary and Text Frames: Currently supports sending/receiving binary or text data.
- [ ] Ping/Pong: Planned
- [ ] Close Frames: Planned
- [ ] Subprotocols: Planned
- [ ] Compression: Planned

## Installation
```bash
go get github.com/ystepanoff/gowest@latest
```

Then import it in your Go code:
```go
import (
    "github.com/ystepanoff/gowest"
)
```

## Basic usage

Below is a simple HTTP server that uses gowest to upgrade connections to WebSocket.
```go
package main

import (
    "fmt"
    "log"
    "net/http"

    "github.com/ystepanoff/gowest"
)

func handler(w http.ResponseWriter, r *http.Request) {
    conn, bufrw, err := gowest.GetConnection(w, r)
    if err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }
    defer conn.Close()

    // Continuously read messages from the client
    for {
        msg, err := gowest.Read(bufrw)
        if err != nil {
            log.Println("Error reading message:", err)
            return
        }
        fmt.Printf("Message received: %s\n", msg)

        // Echo the message back
        err = gowest.WriteString(bufrw, []byte("Echo: "+string(msg)))
        if err != nil {
            log.Println("Error writing message:", err)
            return
        }
    }
}

func main() {
    http.HandleFunc("/", handler)
    log.Println("Server listening on :8080")
    log.Fatal(http.ListenAndServe(":8080", nil))
}
```

* **Upgrade connection**: `gowest.GetConnection` upgrades the connection to a WebSocket and returns the hijacked `net.Conn` and a buffered reader/writer.
* **Read messages**: `gowest.Read` blocks until a full message is received (including fragmented frames).
* **Write messages**: `gowest.WriteString` sends a single text frame back to the client.

## Roadmap

* Ping/Pong support: Respond to pings and send pings for keep-alive.
* Close frames: Proper handling of WebSocket close frames and status codes.
* Subprotocol negotiation: Inspect Sec-WebSocket-Protocol header and pick a subprotocol if desired.
* Compression: Per-message deflate or other compression mechanisms.
* Error/Logging improvements: More detailed errors, built-in logging hooks, etc.
* TLS support: Helper methods to run over HTTPS/TLS (wss://).

## Contributing

Feel free to suggest new features, open issues, or even pull requests!

*Happy hacking!*
