/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/gateway/conformance"
)

func main() {
	endpoint := flag.String("endpoint", "", "adapter base URL")
	tokenEnv := flag.String(
		"token-env", "ORKA_GATEWAY_BEARER_TOKEN",
		"environment variable containing the outbound bearer token",
	)
	timeout := flag.Duration("timeout", 15*time.Second, "per-request timeout")
	referenceFixtures := flag.Bool("reference-fixtures", false, "run optional reference-adapter fault fixtures")
	flag.Parse()
	if strings.TrimSpace(*endpoint) == "" {
		fmt.Fprintln(os.Stderr, "--endpoint is required")
		os.Exit(2)
	}
	token := strings.TrimSpace(os.Getenv(*tokenEnv))
	if token == "" {
		fmt.Fprintf(os.Stderr, "%s is required\n", *tokenEnv)
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4**timeout)
	defer cancel()
	result := conformance.Check(ctx, conformance.Target{
		BaseURL: *endpoint, AuthorizationValue: token, Timeout: *timeout, ReferenceFixtures: *referenceFixtures,
	})
	_ = writeResult(os.Stdout, result, token)
	if !result.Passed {
		os.Exit(1)
	}
}

func writeResult(writer io.Writer, result conformance.CheckResult, token string) error {
	return json.NewEncoder(writer).Encode(conformance.SanitizeCheckResult(result, token))
}
