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
		msg, err := gowest.Read(bufrw)
		if err != nil {
			fmt.Println(err)
			break
		}
		fmt.Println(string(msg))
		if err := gowest.WriteString(bufrw, msg); err != nil {
			fmt.Println(err)
		}
	}
}
