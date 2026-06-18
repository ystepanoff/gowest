# gowest

A minimal, dependency-free WebSocket library for Go with a modern, context-first API.

![GoWest](GoWest.png)

## Overview
**gowest** offers a small, ergonomic API built around a single `Conn` type that is safe to use from multiple goroutines. It is a lightweight alternative to `gorilla/websocket` with:

* A context-first API — `Read`, `Write` and `Accept` all honour `context.Context` deadlines and cancellation.
* Safe concurrent writes — any number of goroutines may call `Write`; frames never interleave.
* An explicit one-reader rule — at most one goroutine calls `Read` at a time.
* Transparent control-frame handling — ping/pong and close frames are handled for you.
* Origin checking and subprotocol negotiation.
* No external dependencies.

## Features

- [x] Handshake: `Accept` performs the RFC 6455 upgrade with origin checking.
- [x] Frame Parsing: Reads and writes WebSocket frames in compliance with RFC 6455.
- [x] Binary and Text Frames: Send and receive binary or text messages, including fragments.
- [x] Ping/Pong: Pings are answered automatically; pongs are ignored.
- [x] Close Frames: Proper close handshake with status codes via `Close`.
- [x] Subprotocols: Negotiated from `AcceptOptions.Subprotocols`.
- [ ] Compression: Planned (no permessage-deflate yet).

## Concurrency contract

* `Write` may be called concurrently from multiple goroutines; writes are serialised internally.
* `Read` must be called from at most one goroutine at a time.
* `Close` is safe to call concurrently with `Read` and `Write`, and is idempotent.

## Performance

On common, non-compressed workloads gowest matches or beats both
`gorilla/websocket` and `coder/websocket`, while allocating an order of
magnitude less per message. Numbers below are a one-message echo round trip over
a TCP loopback connection, median of 6 runs on an Apple M4 Pro (Go 1.24); a
single neutral client drives all three servers, so the only variable per row is
the server library.

| Payload        | gowest        | gorilla          | coder            |
| -------------- | ------------- | ---------------- | ---------------- |
| 32 B text      | **15.93 µs**  | 16.06 µs         | 15.93 µs         |
| 1 KiB binary   | **16.28 µs**  | 16.39 µs         | 17.01 µs         |
| 64 KiB binary  | **38.83 µs**  | 69.92 µs (+80%)  | 68.49 µs (+76%)  |
| 1 MiB binary   | **217.9 µs**  | 433.6 µs (+99%)  | 433.6 µs (+99%)  |
| 5 MiB binary   | **951.7 µs**  | 1567.7 µs (+65%) | 1448.5 µs (+52%) |

| Payload (allocs/op) | gowest | gorilla | coder |
| ------------------- | ------ | ------- | ----- |
| 32 B text           | **2**  | 3       | 2     |
| 1 KiB binary        | **3**  | 6       | 5     |
| 1 MiB binary        | **3**  | 34      | 31    |
| 5 MiB binary        | **3**  | 42      | 39    |

Small messages are dominated by ~16 µs of loopback latency, so all three tie
there; gowest's framing wins show from 64 KiB upward, and it holds a constant
**3 allocations** (a single payload copy) at every size. `Read` costs one
allocation, `Write` zero.

See [`BENCHMARKS.md`](BENCHMARKS.md) for the full methodology, all payload sizes
and how to reproduce the numbers.

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

Below is a simple echo server using the modern `Accept` API.
```go
package main

import (
    "context"
    "log"
    "net/http"

    "github.com/ystepanoff/gowest"
)

func handler(w http.ResponseWriter, r *http.Request) {
    c, err := gowest.Accept(r.Context(), w, r, &gowest.AcceptOptions{
        OriginPatterns: []string{"*"}, // allow any origin; tighten in production
    })
    if err != nil {
        log.Println("accept:", err)
        return
    }
    defer c.Close(gowest.StatusInternalError, "")

    ctx := context.Background()
    for {
        typ, data, err := c.Read(ctx)
        if err != nil {
            log.Println("read:", err) // *gowest.CloseError on a clean close
            return
        }
        if err := c.Write(ctx, typ, data); err != nil {
            log.Println("write:", err)
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

A runnable version lives in [`examples/echo`](examples/echo).

* **Upgrade**: `gowest.Accept` validates the handshake, checks the origin, negotiates a subprotocol and returns a `*Conn`.
* **Read**: `(*Conn).Read` blocks until a complete message is available, handling ping/pong and close frames transparently. Call it from at most one goroutine.
* **Write**: `(*Conn).Write` sends a single message and is safe to call concurrently.
* **Close**: `(*Conn).Close` performs the close handshake with a `StatusCode` and reason.

### Options

`AcceptOptions` controls the handshake:

| Field | Meaning |
| --- | --- |
| `OriginPatterns` | Allowed Origin host patterns (supports a single `*` wildcard). Empty = same-origin only. |
| `Subprotocols` | Server-preferred subprotocols; the first match with the client is negotiated. |
| `MaxMessageBytes` | Maximum inbound message size; larger messages fail with `StatusMessageTooBig`. Unset (≤ 0) uses `DefaultMaxMessageBytes` (32 MiB), so connections are bounded by default. |
| `ReadBufferSize` / `WriteBufferSize` | Buffer sizes for the hijacked connection. |

## Migrating from the legacy API

The original `GetConnection`, `Read` and `WriteString` functions remain available but are **deprecated**. Prefer `Accept` and the `Conn` methods, which add context support, concurrency safety, origin checks and control-frame handling.

## Roadmap

* Compression: per-message deflate.
* Client-side dialing (`Dial`).
* A dedicated writer goroutine as an alternative to the write mutex.
* TLS helpers for `wss://`.

## Contributing

Feel free to suggest new features, open issues, or even pull requests!

*Happy hacking!*
