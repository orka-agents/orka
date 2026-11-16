// The connectors E2E fixture binary: plain HTTP for the OIDC issuer, model,
// and state; TLS for the OAuth provider and resource API.
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	connectorsfixture "github.com/orka-agents/orka/test/fixtures/connectors"
)

func main() {
	ttl, err := time.ParseDuration(envOr("FIXTURE_ACCESS_TOKEN_TTL", "90s"))
	if err != nil {
		log.Fatalf("invalid FIXTURE_ACCESS_TOKEN_TTL: %v", err)
	}
	fixture, err := connectorsfixture.New(connectorsfixture.Config{
		Issuer:          mustEnv("FIXTURE_OIDC_ISSUER"),
		Audience:        mustEnv("FIXTURE_OIDC_AUDIENCE"),
		ClientID:        mustEnv("FIXTURE_OAUTH_CLIENT_ID"),
		ClientSecret:    mustEnv("FIXTURE_OAUTH_CLIENT_SECRET"),
		ModelCredential: mustEnv("FIXTURE_MODEL_CREDENTIAL"),
		AccessTokenTTL:  ttl,
	})
	if err != nil {
		log.Fatal(err)
	}
	plain := &http.Server{Addr: envOr("FIXTURE_PLAIN_ADDR", ":8080"), Handler: fixture.PlainHandler(), ReadHeaderTimeout: 5 * time.Second}
	secure := &http.Server{Addr: envOr("FIXTURE_TLS_ADDR", ":8443"), Handler: fixture.TLSHandler(), ReadHeaderTimeout: 5 * time.Second}
	errs := make(chan error, 2)
	go func() { errs <- plain.ListenAndServe() }()
	go func() { errs <- secure.ListenAndServeTLS(mustEnv("FIXTURE_TLS_CERT"), mustEnv("FIXTURE_TLS_KEY")) }()
	log.Fatal(<-errs)
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func mustEnv(name string) string {
	value := os.Getenv(name)
	if value == "" {
		log.Fatalf("%s is required", name)
	}
	return value
}
