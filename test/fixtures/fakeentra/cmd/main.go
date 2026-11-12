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
	"strings"
	"time"

	"github.com/orka-agents/orka/test/fixtures/fakeentra"
)

func main() {
	listen := flag.String("listen", ":8443", "HTTPS listener")
	tenant := flag.String("tenant", "", "Entra tenant ID the fake serves")
	blueprint := flag.String("blueprint", "", "agent identity blueprint client ID")
	identities := flag.String("identities", "", "comma-separated agent identity IDs that are children of the blueprint")
	resource := flag.String("resource", "", "resource the probe accepts tokens for")
	assertionFile := flag.String("assertion-file", "", "file holding the controller credential the blueprint must present")
	lifetime := flag.Duration("token-lifetime", time.Hour, "expires_in of every issued token")
	certFile := flag.String("tls-cert", "", "serving certificate")
	keyFile := flag.String("tls-key", "", "serving key")
	flag.Parse()
	assertion, err := os.ReadFile(*assertionFile)
	if err != nil || strings.TrimSpace(string(assertion)) == "" {
		log.Print("the controller credential file is missing or empty")
		os.Exit(1)
	}
	fixture := fakeentra.New(fakeentra.Config{
		TenantID: *tenant, BlueprintClientID: *blueprint, Assertion: strings.TrimSpace(string(assertion)),
		Identities: strings.Split(*identities, ","), Resource: *resource, TokenLifetime: *lifetime,
	})
	server := &http.Server{Addr: *listen, Handler: fixture.Handler(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
	_ = server.ListenAndServeTLS(*certFile, *keyFile)
	log.Print("fake Entra stopped")
	os.Exit(1)
}
