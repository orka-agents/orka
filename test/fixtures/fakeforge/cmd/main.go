/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/orka-agents/orka/test/fixtures/fakeforge"
)

func main() {
	token, err := os.ReadFile(requiredEnv("FAKEFORGE_TOKEN_FILE"))
	if err != nil {
		log.Fatalf("read token: %v", err)
	}
	forge, err := fakeforge.New(context.Background(), fakeforge.Config{
		Host:         requiredEnv("FAKEFORGE_HOST"),
		Root:         requiredEnv("FAKEFORGE_ROOT"),
		Token:        strings.TrimSpace(string(token)),
		Repositories: strings.Split(requiredEnv("FAKEFORGE_REPOSITORIES"), ","),
	})
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{
		Addr:              ":8443",
		Handler:           forge.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Fatal(server.ListenAndServeTLS(requiredEnv("FAKEFORGE_TLS_CERT"), requiredEnv("FAKEFORGE_TLS_KEY")))
}

func requiredEnv(name string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		log.Fatalf("%s is required", name)
	}
	return value
}
