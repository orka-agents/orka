package main

import (
	"log"
	"net/http"
	"time"

	"github.com/orka-agents/orka/test/fixtures/compatmodel"
)

func main() {
	server := &http.Server{
		Addr:              ":8080",
		Handler:           http.HandlerFunc(compatmodel.Handler),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Fatal(server.ListenAndServe())
}
