/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/orka-agents/orka/internal/gateway/referenceadapter"
)

func main() {
	listen := flag.String("listen", ":8090", "HTTP listen address")
	flag.Parse()
	token := strings.TrimSpace(os.Getenv("ORKA_GATEWAY_BEARER_TOKEN"))
	if token == "" {
		fmt.Fprintln(os.Stderr, "ORKA_GATEWAY_BEARER_TOKEN is required")
		os.Exit(2)
	}
	server := referenceadapter.New(token)
	log.Printf("starting orka.gateway.v1 reference adapter on %s", *listen)
	// The reference adapter bind address is explicitly operator-configured.
	if err := http.ListenAndServe(*listen, server.Handler()); err != nil { //nolint:gosec
		log.Fatal(err)
	}
}
