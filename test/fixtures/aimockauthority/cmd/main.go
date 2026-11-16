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
	observerListen := flag.String("observer-listen", "127.0.0.1:8081", "loopback-only host observer listener")
	issuer := flag.String("issuer", "", "canonical, in-cluster fixture issuer URL")
	flag.Parse()
	fixture, err := aimockauthority.New(*issuer)
	if err != nil {
		log.Print("authority fixture initialization failed")
		os.Exit(1)
	}
	stopped := make(chan struct{}, 2)
	for _, server := range []*http.Server{
		{Addr: *listen, Handler: fixture.Handler()},
		{Addr: *observerListen, Handler: fixture.ObserverHandler()},
	} {
		server.ReadHeaderTimeout, server.ReadTimeout, server.WriteTimeout = 5*time.Second, 30*time.Second, 30*time.Second
		go func() {
			_ = server.ListenAndServe()
			stopped <- struct{}{}
		}()
	}
	<-stopped
	log.Print("authority fixture stopped")
	os.Exit(1)
}
