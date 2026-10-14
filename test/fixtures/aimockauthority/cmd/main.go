/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/orka-agents/orka/test/fixtures/aimockauthority"
)

func main() {
	listen := flag.String("listen", ":8080", "fixture HTTP listener")
	issuer := flag.String("issuer", "", "canonical, in-cluster fixture issuer URL")
	flag.Parse()
	fixture, err := aimockauthority.New(*issuer)
	if err != nil {
		log.Print("authority fixture initialization failed")
		os.Exit(1)
	}
	server := &http.Server{Addr: *listen, Handler: fixture.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
	if server.ListenAndServe() != nil {
		log.Print("authority fixture stopped")
		os.Exit(1)
	}
}
