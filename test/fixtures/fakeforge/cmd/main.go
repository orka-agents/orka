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
	"path/filepath"
	"strings"
	"time"

	"github.com/orka-agents/orka/test/fixtures/fakeforge"
)

func main() {
	// FAKEFORGE_TOKEN_DIR holds one token-<role> file per credential role.
	tokenDir := requiredEnv("FAKEFORGE_TOKEN_DIR")
	tokens := map[string]string{}
	for _, role := range fakeforge.Roles {
		token, err := os.ReadFile(filepath.Join(tokenDir, "token-"+role))
		if err != nil {
			log.Fatalf("read %s token: %v", role, err)
		}
		tokens[role] = strings.TrimSpace(string(token))
	}
	forge, err := fakeforge.New(context.Background(), fakeforge.Config{
		Host:         requiredEnv("FAKEFORGE_HOST"),
		Root:         requiredEnv("FAKEFORGE_ROOT"),
		Tokens:       tokens,
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
