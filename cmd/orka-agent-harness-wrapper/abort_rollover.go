package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/harness"
)

const defaultAbortRolloverTimeout = time.Minute

func runAbortRollover(args []string) error {
	fs := flag.NewFlagSet("abort-rollover", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		endpoint           string
		bearerTokenFile    string
		expectedGeneration string
		timeout            time.Duration
	)
	fs.StringVar(&endpoint, "endpoint", "", "wrapper HTTP endpoint")
	fs.StringVar(&bearerTokenFile, "bearer-token-file", "", "file containing the wrapper control bearer")
	fs.StringVar(&expectedGeneration, "expected-generation", "", "exact live ledger generation to reopen")
	fs.DurationVar(&timeout, "timeout", defaultAbortRolloverTimeout, "maximum rollover abort duration")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse abort-rollover command: %w", err)
	}
	endpoint = strings.TrimSpace(endpoint)
	bearerTokenFile = strings.TrimSpace(bearerTokenFile)
	expectedGeneration = strings.TrimSpace(expectedGeneration)
	if endpoint == "" || bearerTokenFile == "" || expectedGeneration == "" {
		return fmt.Errorf("abort-rollover endpoint, bearer token file, and expected generation are required")
	}
	if timeout <= 0 {
		return fmt.Errorf("abort-rollover timeout must be positive")
	}
	tokenBytes, err := os.ReadFile(bearerTokenFile)
	if err != nil {
		return fmt.Errorf("read wrapper abort-rollover bearer token file: %w", err)
	}
	bearer := strings.TrimSpace(string(tokenBytes))
	if bearer == "" {
		return fmt.Errorf("wrapper abort-rollover bearer token file is empty")
	}
	client, err := harness.NewClient(
		endpoint,
		harness.WithBearerToken(bearer),
		harness.WithControlTimeout(min(10*time.Second, timeout)),
	)
	if err != nil {
		return fmt.Errorf("configure wrapper abort-rollover client: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if _, err := client.AbortDurableRollover(ctx, expectedGeneration); err != nil {
		return fmt.Errorf("abort wrapper ledger rollover: %w", err)
	}
	return nil
}
