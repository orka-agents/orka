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

	"github.com/orka-agents/orka/internal/acp"
	"github.com/orka-agents/orka/workers/acp/supervisor"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	if _, err := acp.HardenSupervisorProcess(); err != nil {
		logger.Error("failed to harden ACP supervisor", "error", err)
		os.Exit(1)
	}
	if supervisor.CredentialBootstrapConfigured() {
		// Provider-hosted supervisors boot credential-free (their workload
		// template is provider-visible and may be golden-snapshotted) and wait
		// for the controller to seed the pool credentials.
		logger.Info("awaiting controller credential bootstrap")
		bootstrapCtx, cancelBootstrap := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		seeded, err := supervisor.AwaitCredentialBootstrap(bootstrapCtx)
		cancelBootstrap()
		if err != nil {
			logger.Error("credential bootstrap failed", "error", err)
			os.Exit(1)
		}
		for name, value := range map[string]string{
			supervisor.EnvControllerTokenBootstrap:  seeded.ControllerToken,
			supervisor.EnvCapabilitySecretBootstrap: seeded.CapabilitySecret,
			supervisor.EnvProviderTokenBootstrap:    seeded.ProviderToken,
		} {
			if err := os.Setenv(name, value); err != nil {
				logger.Error("stage bootstrapped credential", "error", err)
				os.Exit(1)
			}
		}
		logger.Info("credential bootstrap complete")
	}
	cfg, err := supervisor.LoadConfigFromEnv()
	if err != nil {
		logger.Error("invalid ACP supervisor configuration", "error", err)
		os.Exit(1)
	}
	runtimeServer, err := supervisor.New(cfg)
	cfg.ProviderProxy.UpstreamBearerToken = ""
	if err != nil {
		logger.Error("create ACP supervisor", "error", err)
		os.Exit(1)
	}
	httpServer := &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           runtimeServer.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Control requests carry small bounded JSON bodies; a full-request
		// read deadline stops an untrusted Pod-local peer from holding
		// connections open by dripping chunked bodies. Response streaming
		// (prompt events) is unaffected by the read deadline.
		ReadTimeout:    30 * time.Second,
		IdleTimeout:    2 * time.Minute,
		MaxHeaderBytes: 32 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	shutdownResult := make(chan error, 1)
	go func() {
		<-ctx.Done()
		runtimeServer.BeginDrain("process_shutdown")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		shutdownErr := httpServer.Shutdown(shutdownCtx)
		if shutdownErr != nil {
			shutdownErr = errors.Join(shutdownErr, httpServer.Close())
		}
		shutdownResult <- shutdownErr
	}()

	logger.Info(
		"ACP supervisor listening", "address", cfg.ListenAddress, "provider", cfg.Provider.Kind,
		"runtimeInstanceID", cfg.Fence.RuntimeInstanceID,
	)
	serveErr := httpServer.ListenAndServe()
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	var shutdownErr error
	if ctx.Err() != nil {
		shutdownErr = <-shutdownResult
	} else if serveErr != nil {
		runtimeServer.BeginDrain("http_serve_failed")
		shutdownErr = httpServer.Close()
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cleanupErr := runtimeServer.Close(cleanupCtx)
	if err := errors.Join(serveErr, shutdownErr, cleanupErr); err != nil {
		logger.Error("ACP supervisor stopped with incomplete cleanup", "error", err)
		os.Exit(1)
	}
}
