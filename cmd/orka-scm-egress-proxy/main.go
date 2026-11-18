/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/orka-agents/orka/internal/envutil"
)

func main() {
	listenAddress := flag.String(
		"listen-address",
		envutil.String("ORKA_SCM_EGRESS_PROXY_LISTEN_ADDRESS", defaultListenAddress),
		"HTTP proxy listen address",
	)
	allowedHostsValue := flag.String(
		"allowed-hosts",
		envutil.String("ORKA_SCM_EGRESS_PROXY_ALLOWED_HOSTS", defaultAllowedHosts),
		"Comma-separated exact lower-case SCM hostnames",
	)
	forgeAPIBaseURL := flag.String(
		"forge-api-base-url",
		envutil.String("ORKA_SCM_EGRESS_PROXY_FORGE_API_BASE_URL", defaultForgeAPIBaseURL),
		"Optional HTTPS forge API base URL whose exact hostname is allowed",
	)
	tokenFile := flag.String(
		"token-file",
		envutil.String("ORKA_SCM_EGRESS_PROXY_TOKEN_FILE", defaultTokenFile),
		"Publisher proxy-auth token file",
	)
	maxRequestHeaderBytes := flag.Int64(
		"max-request-header-bytes",
		envutil.MustInt64("ORKA_SCM_EGRESS_PROXY_MAX_REQUEST_HEADER_BYTES", defaultMaxRequestHeaderBytes),
		"Maximum request header bytes",
	)
	maxResponseHeaderBytes := flag.Int64(
		"max-response-header-bytes",
		envutil.MustInt64("ORKA_SCM_EGRESS_PROXY_MAX_RESPONSE_HEADER_BYTES", defaultMaxResponseHeader),
		"Maximum forward response header bytes",
	)
	maxRequestBytes := flag.Int64(
		"max-request-bytes",
		envutil.MustInt64("ORKA_SCM_EGRESS_PROXY_MAX_REQUEST_BYTES", defaultMaxRequestBytes),
		"Maximum forward request body bytes",
	)
	maxResponseBytes := flag.Int64(
		"max-response-bytes",
		envutil.MustInt64("ORKA_SCM_EGRESS_PROXY_MAX_RESPONSE_BYTES", defaultMaxResponseBytes),
		"Maximum forward response body bytes",
	)
	maxTunnelBytes := flag.Int64(
		"max-tunnel-bytes",
		envutil.MustInt64("ORKA_SCM_EGRESS_PROXY_MAX_TUNNEL_BYTES", defaultMaxTunnelBytes),
		"Maximum bytes in each CONNECT tunnel direction",
	)
	maxConcurrent := flag.Int(
		"max-concurrent",
		envutil.MustPositiveInt("ORKA_SCM_EGRESS_PROXY_MAX_CONCURRENT", defaultMaxConcurrent),
		"Maximum concurrent requests and tunnels",
	)
	resolutionTimeout := flag.Duration(
		"resolution-timeout",
		envutil.MustDuration("ORKA_SCM_EGRESS_PROXY_RESOLUTION_TIMEOUT", defaultResolutionTimeout),
		"Per-request DNS resolution timeout",
	)
	connectTimeout := flag.Duration(
		"connect-timeout",
		envutil.MustDuration("ORKA_SCM_EGRESS_PROXY_CONNECT_TIMEOUT", defaultConnectTimeout),
		"Per-address TCP connection timeout",
	)
	responseHeaderTimeout := flag.Duration(
		"response-header-timeout",
		envutil.MustDuration("ORKA_SCM_EGRESS_PROXY_RESPONSE_HEADER_TIMEOUT", defaultResponseHeaderTimeout),
		"Forward response header timeout",
	)
	forwardTimeout := flag.Duration(
		"forward-timeout",
		envutil.MustDuration("ORKA_SCM_EGRESS_PROXY_FORWARD_TIMEOUT", defaultForwardTimeout),
		"Maximum complete forward request lifetime",
	)
	idleTimeout := flag.Duration(
		"idle-timeout",
		envutil.MustDuration("ORKA_SCM_EGRESS_PROXY_IDLE_TIMEOUT", defaultIdleTimeout),
		"Connection and tunnel idle timeout",
	)
	tunnelTimeout := flag.Duration(
		"tunnel-timeout",
		envutil.MustDuration("ORKA_SCM_EGRESS_PROXY_TUNNEL_TIMEOUT", defaultTunnelTimeout),
		"Maximum CONNECT tunnel lifetime",
	)
	shutdownTimeout := flag.Duration(
		"shutdown-timeout",
		envutil.MustDuration("ORKA_SCM_EGRESS_PROXY_SHUTDOWN_TIMEOUT", defaultShutdownTimeout),
		"Graceful shutdown timeout",
	)
	flag.Parse()

	if err := ensureKubernetesServiceAccountTokenAbsent(defaultKubernetesTokenFile); err != nil {
		log.Fatal("SCM egress proxy must not have Kubernetes API credentials")
	}
	hosts, err := allowedHosts(*allowedHostsValue, *forgeAPIBaseURL)
	if err != nil {
		log.Fatal("invalid SCM egress host policy")
	}
	authenticator, err := loadProxyAuthenticator(strings.TrimSpace(*tokenFile))
	if err != nil {
		log.Fatal("SCM egress proxy authentication is unavailable")
	}
	proxy, err := newSCMEgressProxy(proxyConfig{
		AllowedHosts: hosts, MaxRequestHeaderBytes: *maxRequestHeaderBytes,
		MaxResponseHeader: *maxResponseHeaderBytes, MaxRequestBytes: *maxRequestBytes,
		MaxResponseBytes: *maxResponseBytes, MaxTunnelBytes: *maxTunnelBytes,
		MaxConcurrent: *maxConcurrent, ResolutionTimeout: *resolutionTimeout,
		ConnectTimeout: *connectTimeout, ResponseHeaderTimeout: *responseHeaderTimeout,
		ForwardTimeout: *forwardTimeout, IdleTimeout: *idleTimeout, TunnelTimeout: *tunnelTimeout,
	}, authenticator)
	if err != nil {
		log.Fatal("invalid SCM egress proxy configuration")
	}
	listener, err := net.Listen("tcp", strings.TrimSpace(*listenAddress))
	if err != nil {
		log.Fatal("SCM egress proxy listener is unavailable")
	}
	server := &http.Server{
		Addr: strings.TrimSpace(*listenAddress), Handler: proxy,
		ReadHeaderTimeout: min(*idleTimeout, 10*time.Second), ReadTimeout: *idleTimeout,
		WriteTimeout: *responseHeaderTimeout, IdleTimeout: *idleTimeout,
		MaxHeaderBytes: int(*maxRequestHeaderBytes),
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()
	log.Printf("SCM egress proxy listening on %s", listener.Addr())
	select {
	case err := <-serveResult:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal("SCM egress proxy stopped unexpectedly")
		}
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), *shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			log.Print("SCM egress proxy shutdown timed out")
		}
	}
}
