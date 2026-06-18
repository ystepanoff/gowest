# Benchmarks

How to run gowest's benchmarks and the results they produce. Two suites:

- **In-package** (`bench_test.go`) — isolates gowest's own framing cost (header
  encode/decode, unmask, reassembly, per-call allocation) by serving inbound
  bytes from memory and discarding outbound bytes. This is where to read
  **allocations per `Read` and per `Write`**.
- **Cross-library** (`benchmarks/`) — one echo round trip over a real TCP
  loopback, driven by a single neutral client that talks to a gowest, a gorilla
  and a coder server in turn, so the only variable per row is the server
  library. Lives in its own module so the gowest library stays
  dependency-free. Compression is disabled everywhere.

Payload sizes: 32 B text, and 1 KiB / 64 KiB / 1 / 2 / 5 MiB binary.

## Running

```sh
# In-package: framing cost and allocations per Read/Write, plus concurrent writers.
go test -run '^$' -bench 'ReadFrame|ConnRead|ConnWrite|ConcurrentWrite' -benchmem -count=6 .

# Cross-library echo vs gorilla and coder.
cd benchmarks
go test -run '^$' -bench 'BenchmarkEcho' -benchmem -count=6 .
```

`-count=6` with [`benchstat`](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat)
was used for every comparison below; `p`-values are from benchstat.

All numbers were produced on:

```
goos: darwin    goarch: arm64    cpu: Apple M4 Pro    go: 1.24.5
```

## Results — gowest vs gorilla vs coder

Echo round trip, median of 6. `sec/op` (lower is better; gorilla/coder annotated
with their slowdown vs gowest):

| Payload        | gowest        | gorilla          | coder            |
| -------------- | ------------- | ---------------- | ---------------- |
| 32 B text      | **15.93 µs**  | 16.06 µs (~)     | 15.93 µs (~)     |
| 1 KiB binary   | **16.28 µs**  | 16.39 µs (~)     | 17.01 µs (+5%)   |
| 64 KiB binary  | **38.83 µs**  | 69.92 µs (+80%)  | 68.49 µs (+76%)  |
| 1 MiB binary   | **217.9 µs**  | 433.6 µs (+99%)  | 433.6 µs (+99%)  |
| 2 MiB binary   | **393.9 µs**  | 759.1 µs (+93%)  | 721.4 µs (+83%)  |
| 5 MiB binary   | **951.7 µs**  | 1567.7 µs (+65%) | 1448.5 µs (+52%) |

`allocs/op` (and `B/op` for gowest):

| Payload        | gowest      | gorilla         | coder           |
| -------------- | ----------- | --------------- | --------------- |
| 32 B text      | **2** (34 B)| 3   (522 B)     | 2   (514 B)     |
| 1 KiB binary   | **3** (1.0 KiB) | 6 (2.76 KiB) | 5 (2.75 KiB)   |
| 64 KiB binary  | **3** (64 KiB)  | 21 (278 KiB)| 18 (278 KiB)   |
| 1 MiB binary   | **3** (1.0 MiB) | 34 (5.0 MiB)| 31 (5.0 MiB)   |
| 2 MiB binary   | **3** (2.0 MiB) | 37 (10.1 MiB)| 34 (10.1 MiB) |
| 5 MiB binary   | **3** (5.0 MiB) | 42 (25.3 MiB)| 39 (25.3 MiB) |

Small messages are dominated by ~16 µs of loopback latency, so all three tie
there. gowest's framing wins show from 64 KiB up (1.5×–2× faster), and it holds a
constant **3 allocations / one payload copy** at every size where gorilla and
coder grow to ~40 allocations and ~5× the bytes. (`~` = within noise; every other
row is `p=0.002`, n=6.)

## Why it's fast

`Read` costs **1 allocation** (just the payload); `Write` costs **0**. Four
design choices get there, each justified by a memory/CPU profile; details are in
the code comments in `frame.go` and `conn.go`. No `unsafe`, no `sync.Pool`,
public API unchanged.

- **`Write` allocates nothing.** The deadline is applied without a per-call
  closure, and frame headers are emitted with `WriteByte` so the header stays in
  the `bufio` buffer instead of escaping to the heap.
- **Deadline fast path.** An unbounded `context.Background()` op skips the
  deadline syscalls (reads issue none, writes issue one); only a context with a
  deadline or cancellation takes the slower watcher path.
- **`Read` adopts the frame payload.** A single-frame message returns the buffer
  `readFrame` already allocated, with no second buffer or copy.
- **Escape-free `readFrame`.** The mask is applied 8 bytes at a time via
  `encoding/binary`, the header is read with `Peek`/`Discard` (no stack-array
  escapes), and payloads `< 126` bytes take a dedicated fast path.

`go test -race ./...` passes; correctness tests cover all of the above.
