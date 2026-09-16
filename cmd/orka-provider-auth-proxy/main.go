/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/orka-agents/orka/internal/envutil"
)

func main() {
	listenAddress := flag.String("listen-address", envutil.String("ORKA_PROVIDER_AUTH_PROXY_LISTEN_ADDRESS", ":8080"), "HTTP listen address")
	upstreamBaseURL := flag.String("upstream-base-url", os.Getenv("ORKA_PROVIDER_AUTH_PROXY_UPSTREAM_BASE_URL"), "Credential-injecting model gateway base URL")
	tokenFile := flag.String("token-file", envutil.String("ORKA_PROVIDER_AUTH_PROXY_TOKEN_FILE", "/var/run/secrets/orka/provider-auth/token"), "Mounted current bearer token file")
	previousTokenFile := flag.String("previous-token-file", os.Getenv("ORKA_PROVIDER_AUTH_PROXY_PREVIOUS_TOKEN_FILE"), "Optional mounted previous/overlap bearer token file")
	previousTokenValidUntilFile := flag.String("previous-token-valid-until-file", os.Getenv("ORKA_PROVIDER_AUTH_PROXY_PREVIOUS_TOKEN_VALID_UNTIL_FILE"), "Optional mounted RFC3339 expiry file for the previous/overlap token")
	tokenReloadInterval := flag.Duration("token-reload-interval", envutil.MustDuration("ORKA_PROVIDER_AUTH_PROXY_TOKEN_RELOAD_INTERVAL", defaultTokenReloadInterval), "Bearer token file reload interval")
	previousTokenOverlap := flag.Duration("previous-token-overlap", envutil.MustDuration("ORKA_PROVIDER_AUTH_PROXY_PREVIOUS_TOKEN_OVERLAP", defaultPreviousTokenOverlap), "Maximum previous/overlap token acceptance window")
	maxRequestBytes := flag.Int64("max-request-bytes", envutil.MustInt64("ORKA_PROVIDER_AUTH_PROXY_MAX_REQUEST_BYTES", defaultMaxRequestBytes), "Maximum streamed request body size")
	maxResponseBytes := flag.Int64("max-response-bytes", envutil.MustInt64("ORKA_PROVIDER_AUTH_PROXY_MAX_RESPONSE_BYTES", defaultMaxResponseBytes), "Maximum streamed response body size")
	responseHeaderTimeout := flag.Duration("response-header-timeout", envutil.MustDuration("ORKA_PROVIDER_AUTH_PROXY_RESPONSE_HEADER_TIMEOUT", defaultResponseHeaderTimeout), "Upstream response header timeout")
	maxConcurrentRequests := flag.Int("max-concurrent-requests", envutil.MustPositiveInt("ORKA_PROVIDER_AUTH_PROXY_MAX_CONCURRENT_REQUESTS", defaultMaxConcurrentRequests), "Maximum concurrent upstream requests")
	flag.Parse()

	tokens := newBearerTokenStore(time.Now)
	reloader, err := newTokenFileReloader(tokenFileReloaderConfig{
		CurrentTokenFile:            *tokenFile,
		PreviousTokenFile:           *previousTokenFile,
		PreviousTokenValidUntilFile: *previousTokenValidUntilFile,
		ReloadInterval:              *tokenReloadInterval,
		PreviousTokenOverlap:        *previousTokenOverlap,
	}, tokens)
	if err != nil {
		log.Fatalf("invalid provider auth proxy token reload configuration: %v", err)
	}
	if err := reloader.reload(); err != nil {
		log.Fatal("provider auth proxy token files are unavailable or invalid")
	}
	proxy, err := newProviderAuthProxyWithTokenStore(proxyConfig{
		UpstreamBaseURL:       *upstreamBaseURL,
		MaxRequestBytes:       *maxRequestBytes,
		MaxResponseBytes:      *maxResponseBytes,
		ResponseHeaderTimeout: *responseHeaderTimeout,
		MaxConcurrentRequests: *maxConcurrentRequests,
	}, tokens)
	if err != nil {
		log.Fatalf("invalid provider auth proxy configuration: %v", err)
	}
	listener, err := net.Listen("tcp", strings.TrimSpace(*listenAddress))
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	server := newProxyHTTPServer(strings.TrimSpace(*listenAddress), proxy)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go reloader.run(ctx, func(message string) { log.Print(message) })
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.Serve(listener)
	}()
	log.Printf("provider auth proxy listening on %s", listener.Addr())
	select {
	case err := <-serveErr:
		if err != nil && !errorsIsServerClosed(err) {
			log.Fatalf("serve provider auth proxy: %v", err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("provider auth proxy shutdown failed")
		}
		if err := <-serveErr; err != nil && !errorsIsServerClosed(err) {
			log.Printf("provider auth proxy stopped unexpectedly")
		}
	}
}

func errorsIsServerClosed(err error) bool {
	return err == http.ErrServerClosed
}
