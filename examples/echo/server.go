package main

import (
	"fmt"
	"net/http"
)

func main() {
	http.HandleFunc("/", wsHandler)
	http.ListenAndServe(":9000", nil)
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Println(r.Header)
	fmt.Println(w, "Hello, World!")
}
