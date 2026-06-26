// Command autobahn-server is a WebSocket echo server used to validate gowest's
// RFC 6455 conformance against the Autobahn|Testsuite fuzzing client.
//
// gowest is a server-only library, so the test runs in Autobahn's
// "fuzzingclient" mode: the Autobahn client connects to this server and drives
// every protocol test case, and the server simply echoes each message back with
// the same opcode. gowest answers ping/pong and close frames, validates UTF-8 in
// text messages and rejects malformed frames automatically, which is exactly
// what the conformance cases probe.
//
// It listens on :9001 by default (override with -addr) and lives in the root
// module, so it depends only on gowest and the standard library.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"

	"github.com/ystepanoff/gowest"
)

func main() {
	addr := flag.String("addr", ":9001", "listen address for the Autobahn fuzzing client")
	flag.Parse()

	http.HandleFunc("/", echo)
	log.Printf("autobahn echo server listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func echo(w http.ResponseWriter, r *http.Request) {
	c, err := gowest.Accept(r.Context(), w, r, &gowest.AcceptOptions{
		// Autobahn connects without an Origin header, but allow any so the
		// harness can be pointed at this server from anywhere.
		OriginPatterns: []string{"*"},
		// Several conformance cases send multi-megabyte messages; raise the cap
		// well above the largest so they are echoed rather than rejected.
		MaxMessageBytes: 64 << 20,
	})
	if err != nil {
		log.Printf("accept: %v", err)
		return
	}
	defer c.Close(gowest.StatusNormalClosure, "")

	ctx := context.Background()
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			// Most Autobahn cases end by either closing cleanly (*CloseError) or
			// sending a deliberately malformed frame that gowest correctly
			// rejects (*ProtocolError). Both are expected outcomes for a
			// conformance run, so neither is logged; only a genuinely unexpected
			// error (e.g. I/O) is surfaced, so a clean run stays quiet and real
			// anomalies stand out.
			var (
				ce *gowest.CloseError
				pe *gowest.ProtocolError
			)
			if !errors.As(err, &ce) && !errors.As(err, &pe) {
				log.Printf("read: %v", err)
			}
			return
		}
		if err := c.Write(ctx, typ, data); err != nil {
			return
		}
	}
}
