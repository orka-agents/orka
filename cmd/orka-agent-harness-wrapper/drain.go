package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
		endpoint            string
		bearerTokenFile     string
		caFile              string
		nextGeneration      string
		controllerEndpoint  string
		controllerTokenFile string
		timeout             time.Duration
		pollInterval        time.Duration
	)
	fs.StringVar(&endpoint, "endpoint", "", "wrapper HTTPS endpoint")
	fs.StringVar(&bearerTokenFile, "bearer-token-file", "", "file containing the wrapper control bearer")
	fs.StringVar(&caFile, "ca-file", "", "CA bundle used to authenticate the wrapper")
	fs.StringVar(&nextGeneration, "next-generation", "", "optional exact replacement ledger generation")
	fs.StringVar(&controllerEndpoint, "controller-endpoint", "", "optional controller retirement endpoint")
	fs.StringVar(
		&controllerTokenFile,
		"controller-token-file",
		"",
		"projected ServiceAccount token for controller retirement",
	)
	fs.DurationVar(&timeout, "timeout", defaultDrainTimeout, "maximum close-and-drain duration")
	fs.DurationVar(&pollInterval, "poll-interval", defaultDrainPollInterval, "drain status poll interval")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse drain command: %w", err)
	}
	endpoint = strings.TrimSpace(endpoint)
	bearerTokenFile = strings.TrimSpace(bearerTokenFile)
	caFile = strings.TrimSpace(caFile)
	nextGeneration = strings.TrimSpace(nextGeneration)
	controllerEndpoint = strings.TrimSpace(controllerEndpoint)
	controllerTokenFile = strings.TrimSpace(controllerTokenFile)
	if endpoint == "" || bearerTokenFile == "" || caFile == "" {
		return fmt.Errorf("drain endpoint, bearer token file, and CA file are required")
	}
	if timeout <= 0 || pollInterval <= 0 || pollInterval > timeout {
		return fmt.Errorf("drain timeout and poll interval must be positive, with poll interval no greater than timeout")
	}
	if (controllerEndpoint == "") != (controllerTokenFile == "") {
		return fmt.Errorf("controller retirement endpoint and token file must be configured together")
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
	if controllerEndpoint != "" {
		if err := requestControllerRetirement(ctx, controllerEndpoint, controllerTokenFile); err != nil {
			return err
		}
	}
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

func requestControllerRetirement(ctx context.Context, endpoint, tokenFile string) error {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("controller retirement endpoint must be an HTTP(S) URL without user info, query, or fragment")
	}
	tokenBytes, err := os.ReadFile(strings.TrimSpace(tokenFile))
	if err != nil {
		return fmt.Errorf("read controller retirement token file: %w", err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		return fmt.Errorf("controller retirement token file is empty")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, parsed.String(), nil)
	if err != nil {
		return fmt.Errorf("create controller retirement request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("request controller retirement: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	_, _ = io.CopyN(io.Discard, response.Body, 4096)
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf("controller retirement returned HTTP %d", response.StatusCode)
	}
	return nil
}
