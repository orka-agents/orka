package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	setPrivateUmask()
	config, err := publisherservice.LoadConfigFromEnv()
	if err != nil {
		logger.Error("invalid workspace publisher configuration", "error", err)
		os.Exit(1)
	}
	server, err := publisherservice.New(config)
	if err != nil {
		logger.Error("create workspace publisher", "error", err)
		os.Exit(1)
	}
	// Server read/write deadlines must never undercut the configured
	// operation timeouts: a publish permitted to run longer than the write
	// deadline would otherwise be cut off mid-response while the remote push
	// still completes, leaving the controller with a spurious transport
	// failure. Size both from the largest configured operation timeout plus
	// settlement overhead.
	serverTimeout := 3 * time.Minute
	operationTimeout := max(config.PublishTimeout, config.ArtifactTimeout) + time.Minute
	if operationTimeout > serverTimeout {
		serverTimeout = operationTimeout
	}
	httpServer := &http.Server{
		Addr: config.ListenAddress, Handler: server.Handler(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: serverTimeout,
		WriteTimeout: serverTimeout, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 32 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("workspace publisher shutdown failed", "error", err)
		}
	}()
	server.LogStartup(logger)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("workspace publisher failed", "error", err)
		os.Exit(1)
	}
}
