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

const (
	defaultDrainTimeout      = 30 * time.Minute
	defaultDrainPollInterval = time.Second
)

func runDrain(args []string) error {
	fs := flag.NewFlagSet("drain", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		endpoint        string
		bearerTokenFile string
		caFile          string
		nextGeneration  string
		timeout         time.Duration
		pollInterval    time.Duration
	)
	fs.StringVar(&endpoint, "endpoint", "", "wrapper HTTPS endpoint")
	fs.StringVar(&bearerTokenFile, "bearer-token-file", "", "file containing the wrapper control bearer")
	fs.StringVar(&caFile, "ca-file", "", "CA bundle used to authenticate the wrapper")
	fs.StringVar(&nextGeneration, "next-generation", "", "optional exact replacement ledger generation")
	fs.DurationVar(&timeout, "timeout", defaultDrainTimeout, "maximum close-and-drain duration")
	fs.DurationVar(&pollInterval, "poll-interval", defaultDrainPollInterval, "drain status poll interval")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse drain command: %w", err)
	}
	endpoint = strings.TrimSpace(endpoint)
	bearerTokenFile = strings.TrimSpace(bearerTokenFile)
	caFile = strings.TrimSpace(caFile)
	nextGeneration = strings.TrimSpace(nextGeneration)
	if endpoint == "" || bearerTokenFile == "" || caFile == "" {
		return fmt.Errorf("drain endpoint, bearer token file, and CA file are required")
	}
	if timeout <= 0 || pollInterval <= 0 || pollInterval > timeout {
		return fmt.Errorf("drain timeout and poll interval must be positive, with poll interval no greater than timeout")
	}
	tokenBytes, err := os.ReadFile(bearerTokenFile)
	if err != nil {
		return fmt.Errorf("read wrapper drain bearer token file: %w", err)
	}
	bearer := strings.TrimSpace(string(tokenBytes))
	if bearer == "" {
		return fmt.Errorf("wrapper drain bearer token file is empty")
	}
	controlTimeout := min(10*time.Second, timeout)
	httpClient, err := newWrapperTLSHTTPClient(endpoint, caFile)
	if err != nil {
		return fmt.Errorf("configure wrapper drain TLS: %w", err)
	}
	client, err := harness.NewClient(
		endpoint,
		harness.WithBearerToken(bearer),
		harness.WithControlTimeout(controlTimeout),
		harness.WithHTTPClient(httpClient),
	)
	if err != nil {
		return fmt.Errorf("configure wrapper drain client: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if _, err := client.CloseDurableAdmission(ctx); err != nil {
		return fmt.Errorf("close wrapper admission: %w", err)
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		status, err := client.DurableDrainStatus(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("wrapper drain timed out: %w", ctx.Err())
			}
			return fmt.Errorf("read wrapper drain status: %w", err)
		}
		if !status.AdmissionClosed {
			return fmt.Errorf("wrapper did not retain its durable admission close")
		}
		if status.Completed {
			if nextGeneration != "" {
				if _, err := client.PrepareDurableRollover(ctx, nextGeneration); err != nil {
					return fmt.Errorf("prepare wrapper ledger rollover: %w", err)
				}
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wrapper drain timed out: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
