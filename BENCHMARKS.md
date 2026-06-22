# Benchmarks

How to run gowest's benchmarks and the results they produce. Two suites:

- **In-package** (`bench_test.go`) — isolates gowest's own framing cost (header
  encode/decode, unmask, reassembly, per-call allocation) by serving inbound
  bytes from memory and discarding outbound bytes. This is where to read
  **allocations per `Read` and per `Write`**.
- **Cross-library** (`benchmarks/`) — one echo round trip over a real TCP
  loopback, driven by a single neutral client that talks to a gowest, a gorilla,
  a coder and a gobwas server in turn, so the only variable per row is the server
  library. Lives in its own module so the gowest library stays dependency-free.
  Compression is disabled everywhere.

Payload sizes: 32 B text, and 1 KiB / 64 KiB / 1 / 2 / 5 / 10 MiB binary.

The compared libraries are [`gorilla/websocket`](https://github.com/gorilla/websocket),
[`coder/websocket`](https://github.com/coder/websocket) and
[`gobwas/ws`](https://github.com/gobwas/ws). gobwas is driven through its
idiomatic `wsutil` helpers (the way most applications use it), not its
lower-level manual frame API.

## Running

```sh
# In-package: framing cost and allocations per Read/Write, plus concurrent writers.
go test -run '^$' -bench 'ReadFrame|ConnRead|ConnWrite|ConcurrentWrite' -benchmem -count=8 .

# Cross-library echo vs gorilla, coder and gobwas.
cd benchmarks
go test -run '^$' -bench 'BenchmarkEcho' -benchmem -count=10 .
```

[`benchstat`](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat) over repeated
runs (`-count`) was used for every comparison below; `p`-values are from
benchstat.

All numbers were produced on:

```
goos: darwin    goarch: arm64    cpu: Apple M4 Pro    go: 1.24.5
```

## Results — gowest vs gorilla vs coder vs gobwas

Echo round trip, median of 10. `sec/op` (lower is better; the others annotated
with their slowdown vs gowest):

| Payload        | gowest       | gorilla          | coder            | gobwas           |
| -------------- | ------------ | ---------------- | ---------------- | ---------------- |
| 32 B text      | **15.84 µs** | 15.97 µs (~)     | 15.85 µs (~)     | 17.20 µs (+9%)   |
| 1 KiB binary   | **15.96 µs** | 16.38 µs (+3%)   | 16.73 µs (+5%)   | 18.68 µs (+17%)  |
| 64 KiB binary  | **39.09 µs** | 69.14 µs (+77%)  | 70.31 µs (+80%)  | 73.44 µs (+88%)  |
| 1 MiB binary   | **211.3 µs** | 435.3 µs (+106%) | 445.0 µs (+111%) | 482.5 µs (+128%) |
| 2 MiB binary   | **361.2 µs** | 741.2 µs (+105%) | 728.5 µs (+102%) | 778.0 µs (+115%) |
| 5 MiB binary   | **808.1 µs** | 1577 µs (+95%)   | 1584 µs (+96%)   | 1681 µs (+108%)  |
| 10 MiB binary  | **1.706 ms** | 2.950 ms (+73%)  | 3.187 ms (+87%)  | 3.125 ms (+83%)  |

`allocs/op` (and `B/op` for gowest):

| Payload        | gowest          | gorilla        | coder          | gobwas         |
| -------------- | --------------- | -------------- | -------------- | -------------- |
| 32 B text      | **2** (34 B)    | 3 (522 B)      | 2 (514 B)      | 6 (768 B)      |
| 1 KiB binary   | **3** (1.0 KiB) | 6 (2.76 KiB)   | 5 (2.75 KiB)   | 9 (3.0 KiB)    |
| 64 KiB binary  | **3** (64 KiB)  | 21 (278 KiB)   | 18 (278 KiB)   | 22 (279 KiB)   |
| 1 MiB binary   | **3** (1.0 MiB) | 34 (5.0 MiB)   | 31 (5.0 MiB)   | 35 (5.0 MiB)   |
| 2 MiB binary   | **3** (2.0 MiB) | 37 (10.1 MiB)  | 34 (10.1 MiB)  | 38 (10.1 MiB)  |
| 5 MiB binary   | **3** (5.0 MiB) | 42 (25.3 MiB)  | 39 (25.3 MiB)  | 43 (25.3 MiB)  |
| 10 MiB binary  | **3** (10 MiB)  | 45 (49.8 MiB)  | 42 (49.8 MiB)  | 46 (49.8 MiB)  |

Small messages are dominated by ~16 µs of loopback latency, so the field ties at
32 B (gobwas aside) and gowest leads slightly at 1 KiB. From 64 KiB up gowest is
**~2× faster** than every other library, and it holds a constant **3 allocations
/ a single payload copy** at every size where the others grow to ~40 allocations
and ~5× the bytes moved. (`~` = within noise; every other row is `p≈0`, n=10.)

## Why it's fast

`Read` costs **1 allocation** (just the payload); `Write` costs **0**. A handful
of design choices get there, each justified by a memory/CPU profile; details are
in the code comments in `frame.go` and `conn.go`. No `unsafe`, no `sync.Pool`,
public API unchanged.

- **`Write` allocates nothing.** The deadline is applied without a per-call
  closure, and frame headers are emitted with `WriteByte` so the header stays in
  the `bufio` buffer instead of escaping to the heap.
- **Deadline fast path.** An unbounded `context.Background()` op skips the
  deadline syscalls (reads issue none, writes issue one); only a context with a
  deadline or cancellation takes the slower watcher path.
- **`Read` adopts the frame payload.** A single-frame message returns the buffer
  `readFrame` already allocated, with no second buffer or copy.
- **Single-pass read+unmask.** The payload is unmasked chunk by chunk as it is
  read, while each chunk is still hot in cache, rather than copied in full and
  then unmasked in a second pass. The two-pass form touches the payload twice,
  which for payloads too large to stay cached doubles the memory traffic — that
  was a measurable regression in the bandwidth-bound regime (≈5 MiB+) before the
  passes were fused.
- **Escape-free `readFrame`.** The mask is applied 8 bytes at a time via
  `encoding/binary`, the header is read with `Peek`/`Discard` (no stack-array
  escapes), and payloads `< 126` bytes take a dedicated fast path.

`go test -race ./...` passes; correctness tests cover all of the above.
