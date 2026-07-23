/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/gateway/referenceadapter"
)

const (
	defaultReadHeaderTimeout = 5 * time.Second
	defaultReadTimeout       = 30 * time.Second
	defaultWriteTimeout      = 60 * time.Second
	defaultIdleTimeout       = 60 * time.Second
)

type serverOptions struct {
	listenAddr  string
	tlsCertFile string
	tlsKeyFile  string
}

func (o serverOptions) validate() error {
	if (o.tlsCertFile == "") != (o.tlsKeyFile == "") {
		return errors.New("--tls-cert-file and --tls-key-file must be provided together")
	}
	return nil
}

func (o serverOptions) tlsEnabled() bool {
	return o.tlsCertFile != "" && o.tlsKeyFile != ""
}

func newServer(options serverOptions, handler http.Handler) (*http.Server, error) {
	if err := options.validate(); err != nil {
		return nil, err
	}

	server := &http.Server{
		Addr:              options.listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: defaultReadHeaderTimeout,
		ReadTimeout:       defaultReadTimeout,
		WriteTimeout:      defaultWriteTimeout,
		IdleTimeout:       defaultIdleTimeout,
	}
	if options.tlsEnabled() {
		server.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return server, nil
}

func serve(server *http.Server, options serverOptions) error {
	if options.tlsEnabled() {
		return server.ListenAndServeTLS(options.tlsCertFile, options.tlsKeyFile)
	}
	return server.ListenAndServe()
}

func main() {
	options := serverOptions{}
	flag.StringVar(&options.listenAddr, "listen", ":8090", "listen address")
	flag.StringVar(&options.tlsCertFile, "tls-cert-file", "", "TLS certificate file (requires --tls-key-file)")
	flag.StringVar(&options.tlsKeyFile, "tls-key-file", "", "TLS private key file (requires --tls-cert-file)")
	flag.Parse()

	token := strings.TrimSpace(os.Getenv("ORKA_GATEWAY_BEARER_TOKEN"))
	if token == "" {
		fmt.Fprintln(os.Stderr, "ORKA_GATEWAY_BEARER_TOKEN is required")
		os.Exit(2)
	}

	server, err := newServer(options, referenceadapter.New(token).Handler())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	transport := "HTTP"
	if options.tlsEnabled() {
		transport = "HTTPS"
	}
	log.Printf("starting orka.gateway.v1 reference adapter over %s on %s", transport, options.listenAddr)
	if err := serve(server, options); err != nil {
		log.Fatal(err)
	}
}
