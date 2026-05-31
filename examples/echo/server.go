// Command echo is a minimal WebSocket echo server built on the modern gowest
// API. It upgrades each request with Accept and echoes every message back to
// the client using the per-message Read/Write methods.
package main

import (
	"context"
	"log"
	"net/http"

	"github.com/ystepanoff/gowest"
)

func main() {
	http.HandleFunc("/", echoHandler)
	log.Println("echo server listening on :9000")
	log.Fatal(http.ListenAndServe(":9000", nil))
}

func echoHandler(w http.ResponseWriter, r *http.Request) {
	c, err := gowest.Accept(r.Context(), w, r, &gowest.AcceptOptions{
		OriginPatterns: []string{"*"},
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
			log.Println("read:", err)
			return
		}
		if err := c.Write(ctx, typ, data); err != nil {
			log.Println("write:", err)
			return
		}
	}
}
