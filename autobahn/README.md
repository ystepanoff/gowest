# Autobahn conformance testing

[Autobahn|Testsuite](https://github.com/crossbario/autobahn-testsuite) is the
reference test suite for WebSocket implementations. It exercises every corner of
RFC 6455 — framing, fragmentation, UTF-8 handling, close codes, ping/pong and
limits.

gowest is a **server-only** library, so the suite runs in **fuzzing-client**
mode: the Autobahn client connects to the gowest echo server in
[`server.go`](server.go) and drives every test case; the server echoes each
message back. Protocol violations are detected and reported by gowest itself
(it answers control frames, validates UTF-8 and rejects malformed frames), which
is exactly what the conformance cases check.

## Run it with Docker (recommended)

From this directory:

```sh
docker compose up --build --abort-on-container-exit
```

This builds the gowest echo server, starts the upstream Autobahn image, runs the
full case set and writes an HTML report to `report/`. When it finishes:

```sh
open report/index.html        # macOS
# or: xdg-open report/index.html
```

Tear down with `docker compose down`.

## Run it manually (without Docker)

Start the echo server (from the repository root):

```sh
go run ./autobahn            # listens on :9001
```

Then run Autobahn against it however you have it installed — for example with
the upstream image, pointing it at the host server:

```sh
docker run -it --rm \
  -v "$PWD/autobahn/fuzzingclient.json:/config/fuzzingclient.json:ro" \
  -v "$PWD/autobahn/report:/config/report" \
  --network host \
  crossbario/autobahn-testsuite:25.10.1 \
  wstest -m fuzzingclient -s /config/fuzzingclient.json
```

(Adjust the `url` in `fuzzingclient.json` to `ws://127.0.0.1:9001` when not
using the compose network.)

## Interpreting the report

`report/index.html` lists every case with a status:

- **Pass** / **Non-Strict** — conformant. Non-Strict means the implementation
  handled the case acceptably but not in the single strictest way (commonly the
  exact timing of a close); it is not a failure.
- **Fail** — a genuine conformance bug worth filing.

Performance cases (section 9, large-message and many-frame throughput) are timing
benchmarks rather than pass/fail correctness checks.

## Files

| File                   | Purpose                                              |
| ---------------------- | ---------------------------------------------------- |
| `server.go`            | gowest echo server under test                        |
| `fuzzingclient.json`   | Autobahn case selection and target server            |
| `Dockerfile`           | Builds the echo server image                         |
| `docker-compose.yml`   | Wires the server and the Autobahn client together    |
| `report/`              | Generated HTML report (git-ignored)                  |
