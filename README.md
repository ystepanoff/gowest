# gowest

**A context-first, concurrency-safe, fast WebSocket library for Go.**

[![CI](https://github.com/ystepanoff/gowest/actions/workflows/ci.yml/badge.svg)](https://github.com/ystepanoff/gowest/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/ystepanoff/gowest.svg)](https://pkg.go.dev/github.com/ystepanoff/gowest)
[![Go Report Card](https://goreportcard.com/badge/github.com/ystepanoff/gowest)](https://goreportcard.com/report/github.com/ystepanoff/gowest)
<!-- Autobahn badge placeholder: published once the suite has been run and the
     report is hosted (see autobahn/). -->
![Autobahn](https://img.shields.io/badge/autobahn-pending-lightgrey)

![GoWest](GoWest.png)

gowest is a small WebSocket library built around a single `Conn` type with a
modern, `context`-aware API and an explicit concurrency contract. It is a
**server-side** library (RFC 6455) with **no external dependencies**.

- **Context-first** — `Accept`, `Read`, `Write` and `Ping` all honour
  `context.Context` deadlines and cancellation.
- **Concurrency-safe** — many goroutines may `Write` at once; frames never
  interleave. The contract is explicit and documented below.
- **Fast** — one allocation per `Read`, zero per `Write`, and ~2× the throughput
  of gorilla/coder/gobwas on uncompressed payloads from 64 KiB up
  ([benchmarks](#performance)).
- **Correct** — transparent ping/pong and close handling, UTF-8 validation, and
  framing-protocol enforcement, with an [Autobahn](autobahn/) harness to prove
  it.
- **Dependency-free** — only the standard library.

> **Status: beta.** The API is stable and the test suite (including `-race`) is
> green, but gowest has not yet been validated in production deployments. See
> [Production readiness](#production-readiness) for the honest details.

## Installation

```bash
go get github.com/ystepanoff/gowest@latest
```

```go
import "github.com/ystepanoff/gowest"
```

Requires Go 1.19+.

## Quick start (server)

gowest is **server-only today** — there is no `Dial`/client yet (it is on the
[roadmap](#roadmap)). Upgrade an incoming HTTP request with `Accept`, then read
and write messages on the returned `*Conn`:

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
			return // *gowest.CloseError on a clean peer close
		}
		if err := c.Write(ctx, typ, data); err != nil {
			return
		}
	}
}

func main() {
	http.HandleFunc("/", handler)
	log.Println("listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
```

A runnable version lives in [`examples/echo`](examples/echo). For a client to
test against, use any browser or an existing client library (e.g.
`coder/websocket`'s `Dial`) until gowest ships its own.

- **Upgrade** — `Accept` validates the handshake, checks the origin, negotiates
  a subprotocol and returns a `*Conn`.
- **Read** — `(*Conn).Read` blocks until a complete message is available,
  handling ping/pong and close frames transparently. One goroutine at a time.
- **Write** — `(*Conn).Write` sends one message; safe to call concurrently.
- **Close** — `(*Conn).Close` performs the close handshake with a `StatusCode`
  and reason.

### Options

`AcceptOptions` controls the handshake:

| Field | Meaning |
| --- | --- |
| `OriginPatterns` | Allowed `Origin` host patterns (single `*` wildcard). Empty = same-origin only. |
| `Subprotocols` | Server-preferred subprotocols; the first match with the client is negotiated. |
| `MaxMessageBytes` | Maximum inbound message size; larger messages fail with `StatusMessageTooBig`. Unset (≤ 0) uses `DefaultMaxMessageBytes` (32 MiB), so connections are bounded by default. |
| `ReadBufferSize` / `WriteBufferSize` | Buffer sizes for the hijacked connection. |

## Concurrency guarantees

gowest follows the same one-reader / many-writer rule used by most production
WebSocket libraries:

- **`Write`** may be called concurrently from any number of goroutines. Writes
  are serialised internally with a mutex, so frames never interleave on the wire.
- **`Read`** must be called from **at most one goroutine at a time**. Reading
  from several goroutines concurrently corrupts the message stream and is a
  programming error.
- **`Close`** is safe to call concurrently with `Read`, `Write` and `Ping`, and
  is idempotent. It unblocks any in-flight operation, which then returns an error.
- **`Ping`** may be called concurrently with `Write`; pongs are observed by the
  goroutine calling `Read`.

Cancellation is implemented with `net.Conn` deadlines, not a per-call goroutine
that outlives the operation: a cancelled or timed-out `Read`/`Write` interrupts
the underlying I/O and fails the connection (a WebSocket stream cannot be safely
resumed mid-frame). The full contract is documented in the
[package docs](https://pkg.go.dev/github.com/ystepanoff/gowest).

The suite is run under the race detector in CI (`go test -race ./...`).

## Features

- [x] RFC 6455 server handshake (`Accept`) with origin checking
- [x] Text and binary messages, including fragmented messages
- [x] Automatic ping/pong replies; optional observation handlers
- [x] Close handshake with status codes and reasons
- [x] Subprotocol negotiation
- [x] Inbound message size limits (memory-exhaustion guard)
- [x] Context deadlines and cancellation on every operation
- [ ] Client dialing (`Dial`) — planned
- [ ] permessage-deflate compression — planned

## Performance

On common, non-compressed workloads gowest matches or beats `gorilla/websocket`,
`coder/websocket` and `gobwas/ws`, while allocating an order of magnitude less
per message. Numbers below are a one-message echo round trip over a TCP loopback
connection, median of 10 runs on an Apple M4 Pro (Go 1.24); a single neutral
client drives every server, so the only variable per row is the server library.

| Payload        | gowest       | gorilla          | coder            | gobwas           |
| -------------- | ------------ | ---------------- | ---------------- | ---------------- |
| 32 B text      | **15.84 µs** | 15.97 µs         | 15.85 µs         | 17.20 µs (+9%)   |
| 1 KiB binary   | **15.96 µs** | 16.38 µs (+3%)   | 16.73 µs (+5%)   | 18.68 µs (+17%)  |
| 64 KiB binary  | **39.09 µs** | 69.14 µs (+77%)  | 70.31 µs (+80%)  | 73.44 µs (+88%)  |
| 1 MiB binary   | **211.3 µs** | 435.3 µs (+106%) | 445.0 µs (+111%) | 482.5 µs (+128%) |
| 10 MiB binary  | **1.706 ms** | 2.950 ms (+73%)  | 3.187 ms (+87%)  | 3.125 ms (+83%)  |

| allocs/op      | gowest | gorilla | coder | gobwas |
| -------------- | ------ | ------- | ----- | ------ |
| 32 B text      | **2**  | 3       | 2     | 6      |
| 1 KiB binary   | **3**  | 6       | 5     | 9      |
| 1 MiB binary   | **3**  | 34      | 31    | 35     |
| 10 MiB binary  | **3**  | 45      | 42    | 46     |

`Read` costs one allocation (the payload); `Write` costs zero. gowest holds a
constant 3 allocations per echo at every size.

### Benchmark caveats

- **Read these as relative, not absolute.** They were taken on one machine
  (Apple M4 Pro, Go 1.24) over **loopback TCP**, not a real network. Your
  hardware, Go version, OS and network will differ. Reproduce them yourself:
  `cd benchmarks && go test -bench=BenchmarkEcho -benchmem -count=10 .`
- **Small messages are latency-bound.** At 32 B–1 KiB a round trip is dominated
  by ~16 µs of loopback latency, so all libraries effectively tie there; the
  framing differences only emerge from ~64 KiB up.
- **Uncompressed only.** gowest does not implement permessage-deflate, so this
  compares the uncompressed path. With compression enabled, gorilla and coder
  trade CPU for bandwidth — a different trade-off this table does not cover.
- **gobwas** is driven through its idiomatic `wsutil` helpers (how most apps use
  it), not its lower-level manual frame API, which can allocate less with more
  caller code.
- The comparison libraries live in a separate `benchmarks/` module, so the
  gowest library itself stays dependency-free.

Full methodology and per-size data: [`BENCHMARKS.md`](BENCHMARKS.md).

## Comparison

| | gowest | gorilla/websocket | coder/websocket |
| --- | --- | --- | --- |
| API style | context-first | callback/deadline | context-first |
| Server (`Accept`) | ✅ | ✅ | ✅ |
| Client (`Dial`) | ❌ (planned) | ✅ | ✅ |
| Concurrent writers | ✅ (mutex) | ❌ (one writer) | ✅ |
| permessage-deflate | ❌ (planned) | ✅ | ✅ |
| External dependencies | none | none | none |
| Uncompressed echo throughput (64 KiB+) | fastest in this set | baseline | baseline |
| Allocations per echo | constant (3) | grows with size | grows with size |
| Maintenance | beta, single-maintainer | mature, archived¹ | actively maintained |

¹ gorilla/websocket is widely deployed but its repository is in maintenance mode.

If you need a client today, or compression, or a long battle-tested track
record, choose `coder/websocket` or `gorilla/websocket`. Choose gowest when you
want a small, dependency-free, context-first **server** with low per-message
overhead.

## Conformance (Autobahn)

gowest ships an [Autobahn|Testsuite](https://github.com/crossbario/autobahn-testsuite)
harness in [`autobahn/`](autobahn/). Because gowest is server-only it runs in
fuzzing-client mode (Autobahn connects to a gowest echo server and drives every
RFC 6455 case). Run it locally with Docker:

```sh
make autobahn          # builds the server, runs the suite, writes autobahn/report/
```

The published Autobahn report and badge will be added here once the suite has
been run end-to-end and the report hosted. See [`autobahn/README.md`](autobahn/README.md).

## Production readiness

Honest status, so you can make an informed call:

**What is in place**
- Full unit/integration test suite, green under `go test -race ./...`.
- CI on every push/PR: `go vet`, `go test`, `go test -race`, and benchmark
  compilation, on Go 1.19 and latest stable.
- Explicit, tested concurrency contract and context cancellation semantics.
- Inbound size limits on by default (memory-exhaustion guard).
- Benchmarks vs three established libraries.

**What is not yet proven / missing**
- **No production track record** — not yet known to run real traffic at scale.
- **Autobahn results not yet published** — the harness exists; the run/report
  are pending (see the badge above).
- **No client** (`Dial`) and **no compression** (permessage-deflate).
- **No TLS helpers** — terminate `wss://` at your server/proxy.
- Single maintainer; expect the occasional rough edge.

**Recommendation:** suitable for hobby projects, internal tools and evaluation,
and for production **after** you run the Autobahn suite and load-test for your
workload. Do not treat it as a drop-in, battle-tested replacement for
gorilla/coder yet.

## Migrating from the legacy API

The original `GetConnection`, `Read` and `WriteString` functions remain
available but are **deprecated**. Prefer `Accept` and the `Conn` methods, which
add context support, concurrency safety, origin checks and control-frame
handling.

## Development

```sh
make help     # list targets
make check    # vet + test + race + benchmark compile (what CI runs)
make bench    # cross-library echo benchmarks
make autobahn # RFC 6455 conformance suite (requires Docker)
```

## Roadmap

- Client-side dialing (`Dial`) and a published Autobahn report.
- permessage-deflate compression.
- TLS helpers for `wss://`.
- A dedicated writer goroutine as an alternative to the write mutex.

## Contributing

Issues and pull requests are welcome — especially production feedback, Autobahn
results on your platform, and benchmark numbers from other hardware.

*Happy hacking!*
