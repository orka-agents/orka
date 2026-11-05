/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Command orka-host-agent runs beside an agent host in its Pod. It answers the
// host container's probes by checking the host's loopback Agent Host Protocol
// listener, and serves the host's idle status to the Orka controller. Run as
// "orka-host-agent stage", it stages the host's connection token and a copy of
// itself for the host; run as "orka-host-agent init -- command", it is the
// host container's init process.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	logger := log.New(os.Stderr, "orka-host-agent: ", log.LstdFlags|log.LUTC)
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "stage":
			if err := stage(os.Args[2:]); err != nil {
				logger.Fatalf("stage: %v", err)
			}
			return
		case "init":
			code, err := runInit(os.Args[2:])
			if err != nil {
				logger.Fatalf("init: %v", err)
			}
			os.Exit(code)
		}
	}
	address := flag.String("address", ":7790", "address to serve probes and status on")
	listenPort := flag.Int("listen-port", 0, "loopback port of the host's Agent Host Protocol listener")
	tokenFile := flag.String("token-file", "", "file holding the host's connection token")
	statusFile := flag.String("status-file", "", "the host's idle status file; empty when the host reports none")
	flag.Parse()

	if *listenPort < 1 || *listenPort > 65535 || *tokenFile == "" {
		logger.Fatal("--listen-port and --token-file are required")
	}

	agent := &agent{
		listenPort:       *listenPort,
		tokenFile:        *tokenFile,
		statusFile:       *statusFile,
		dialTimeout:      2 * time.Second,
		handshakeTimeout: 8 * time.Second,
		handshake:        ahpHandshake,
		now:              time.Now,
		logger:           logger,
	}
	server := &http.Server{
		Addr:              *address,
		Handler:           agent.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	logger.Printf("serving on %s for host port %d", *address, *listenPort)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatalf("serve: %v", err)
	}
}
