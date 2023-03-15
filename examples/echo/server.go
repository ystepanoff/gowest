package main

import (
	"fmt"
	"github.com/ystepanoff/gowest"
	"net/http"
)

func main() {
	http.HandleFunc("/", wsHandler)
	http.ListenAndServe(":9000", nil)
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Println("connection attempt")
	conn, bufrw, err := gowest.GetConnection(w, r)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer conn.Close()
	for {
		msg, ok := gowest.Read(*bufrw)
		if !ok {
			break
		}
		fmt.Println(string(msg))
	}
}
