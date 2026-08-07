package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/orka-agents/orka/internal/tracing"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/workers/harness/cliwrapper"
)

const (
	envHarnessWrapperTLSCertFile = "ORKA_HARNESS_WRAPPER_TLS_CERT_FILE"
	envHarnessWrapperTLSKeyFile  = "ORKA_HARNESS_WRAPPER_TLS_KEY_FILE"
)

type repeatedString []string

func (r *repeatedString) String() string { return strings.Join(*r, ",") }
func (r *repeatedString) Set(value string) error {
	*r = append(*r, value)
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "copilot-turn":
			return cliwrapper.RunCopilotTurnCLI(context.Background(), os.Stdin, os.Stdout)
		case "drain":
			return runDrain(args[1:])
		case "abort-rollover":
			return runAbortRollover(args[1:])
		}
	}
	cfg, err := cliwrapper.LoadConfigFromEnvUnvalidated()
	if err != nil {
		return err
	}
	_ = os.Unsetenv(cliwrapper.EnvAuthValue)
	authValueFromEnv := cfg.AuthValue
	cfg.AuthValue = ""
	var extraArgs repeatedString
	var extraEnv repeatedString
	tlsCertFile := strings.TrimSpace(os.Getenv(envHarnessWrapperTLSCertFile))
	tlsKeyFile := strings.TrimSpace(os.Getenv(envHarnessWrapperTLSKeyFile))
	fs := flag.NewFlagSet("orka-agent-harness-wrapper", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.StringVar(&cfg.ListenAddr, "listen-addr", cfg.ListenAddr, "HTTPS listen address")
	fs.StringVar(&tlsCertFile, "tls-cert-file", tlsCertFile, "server TLS certificate file")
	fs.StringVar(&tlsKeyFile, "tls-key-file", tlsKeyFile, "server TLS private key file")
	fs.StringVar(&cfg.Runtime, "runtime", cfg.Runtime, "runtime adapter: generic, codex, claude, copilot, opencode, multi")
	fs.StringVar(&cfg.WorkDir, "workdir", cfg.WorkDir, "default command working directory")
	fs.StringVar(&cfg.Generic.Command, "command", cfg.Generic.Command, "generic adapter command path")
	fs.Var(&extraArgs, "arg", "generic adapter command argument (repeatable)")
	fs.Var(&extraEnv, "env", "generic adapter command environment entry KEY=VALUE (repeatable)")
	fs.StringVar(&cfg.Generic.PromptMode, "prompt-mode", cfg.Generic.PromptMode, "generic prompt mode: stdin, env, file")
	fs.StringVar(&cfg.Generic.PromptEnv, "prompt-env", cfg.Generic.PromptEnv, "env var used for prompt env/file modes")
	fs.StringVar(&cfg.Generic.PromptFile, "prompt-file", cfg.Generic.PromptFile, "prompt file path for prompt-mode=file")
	fs.StringVar(&cfg.Generic.ResultMode, "result-mode", cfg.Generic.ResultMode, "generic result mode: stdout, file")
	fs.StringVar(&cfg.Generic.ResultFile, "result-file", cfg.Generic.ResultFile, "result file path for result-mode=file")
	fs.Int64Var(&cfg.StdoutLimitBytes, "stdout-limit-bytes", cfg.StdoutLimitBytes, "stdout capture limit")
	fs.Int64Var(&cfg.StderrLimitBytes, "stderr-limit-bytes", cfg.StderrLimitBytes, "stderr capture limit")
	fs.DurationVar(&cfg.CancelGrace, "cancel-grace-period", cfg.CancelGrace, "SIGTERM to SIGKILL grace period")
	fs.DurationVar(&cfg.TurnRetention, "turn-retention", cfg.TurnRetention, "completed turn in-memory retention TTL")
	fs.StringVar(
		&cfg.AdmissionLedgerPath,
		"admission-ledger-path",
		cfg.AdmissionLedgerPath,
		"durable wrapper admission ledger path",
	)
	fs.StringVar(
		&cfg.LedgerGeneration,
		"ledger-generation",
		cfg.LedgerGeneration,
		"durable wrapper admission ledger generation",
	)
	fs.DurationVar(
		&cfg.LedgerRetention,
		"ledger-retention",
		cfg.LedgerRetention,
		"minimum retention for controller-settled wrapper ledger records",
	)
	fs.StringVar(&cfg.Copilot.Path, "copilot-cli-path", cfg.Copilot.Path, "Copilot CLI path for the copilot adapter")
	fs.StringVar(
		&cfg.Copilot.HelperPath,
		"copilot-helper-path",
		cfg.Copilot.HelperPath,
		"helper executable path for the copilot adapter",
	)
	fs.StringVar(&cfg.AuthValue, "bearer-token", cfg.AuthValue, "required bearer token for turn/event/cancel endpoints")
	fs.BoolVar(
		&cfg.AllowUnauthenticated,
		"allow-unauthenticated",
		cfg.AllowUnauthenticated,
		"allow unauthenticated turn/event/cancel requests (local tests only)",
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	tlsCertFile = strings.TrimSpace(tlsCertFile)
	tlsKeyFile = strings.TrimSpace(tlsKeyFile)
	if tlsCertFile == "" || tlsKeyFile == "" {
		return fmt.Errorf("TLS certificate and private key files are required")
	}
	if len(extraArgs) > 0 {
		cfg.Generic.Args = append(cfg.Generic.Args, extraArgs...)
	}
	if cfg.AuthValue == "" {
		cfg.AuthValue = authValueFromEnv
	}
	if len(extraEnv) > 0 {
		cfg.Generic.Env = append(cfg.Generic.Env, extraEnv...)
		cfg.CommandEnv = append(cfg.CommandEnv, extraEnv...)
	}
	if cfg.WorkDir != "" {
		cfg.Generic.WorkDir = cfg.WorkDir
		cfg.Codex.WorkDir = cfg.WorkDir
		cfg.Claude.WorkDir = cfg.WorkDir
		cfg.Copilot.WorkDir = cfg.WorkDir
		cfg.Opencode.WorkDir = cfg.WorkDir
	}
	telemetryEnabled := workerenv.IsTrue(os.Getenv(workerenv.EnableTelemetry))
	tracingShutdown, err := tracing.Init("orka-agent-harness-wrapper", telemetryEnabled)
	if err != nil {
		return fmt.Errorf("failed to initialize telemetry: %w", err)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if shutdownErr := tracingShutdown(shutdownCtx); shutdownErr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to shutdown telemetry: %v\n", shutdownErr)
		}
	}()

	adapter, err := cliwrapper.NewRuntimeAdapter(cfg)
	if err != nil {
		return err
	}
	server, err := cliwrapper.NewServer(cfg, adapter)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := server.Close(); closeErr != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to close wrapper admission ledger: %v\n", closeErr)
		}
	}()
	httpServer := &http.Server{
		Addr: cfg.ListenAddr, Handler: server.Handler(),
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	errCh := make(chan error, 1)
	go func() {
		fmt.Fprintf(os.Stderr, "orka agent harness wrapper listening with TLS on %s (runtime=%s)\n", cfg.ListenAddr, adapter.Name())
		if err := httpServer.ListenAndServeTLS(tlsCertFile, tlsKeyFile); err != nil && err != http.ErrServerClosed {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.CancelGrace)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}
