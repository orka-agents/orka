//go:build linux

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		// Subprocess/API errors can contain source or credential-bearing bytes.
		// Only the fixed mode and failure class are exported.
		if errors.Is(err, errObserverReported) {
			os.Exit(125)
		}
		message := "application worker failed closed"
		if errors.Is(err, errOfflineBuild) {
			message = errOfflineBuild.Error()
		} else if errors.Is(err, errToolchainIdentity) {
			message = errToolchainIdentity.Error()
		}
		_, _ = fmt.Fprintln(os.Stderr, message)
		os.Exit(125)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	if len(args) == 0 {
		return pv.ErrEvidence
	}
	if args[0] == modeInstall {
		if len(args) != 3 || args[1] != "--destination" || args[2] != "/runner/worker" {
			return pv.ErrEvidence
		}
		if err := installFileSizeLimit(modeInstall); err != nil {
			return err
		}
		return installWorker()
	}
	if args[0] == modeGit {
		return runGit(args[1:])
	}
	operation, cancel, err := applicationDeadline(ctx, os.Getenv("ORKA_APPLICATION_DEADLINE"))
	if err != nil {
		return err
	}
	defer cancel()
	ctx = operation
	if args[0] == modeMarker && len(args) == 3 && args[1] == "--port" {
		port, err := strconv.Atoi(args[2])
		if err != nil {
			return pv.ErrEvidence
		}
		if err := installFileSizeLimit(modeMarker); err != nil {
			return err
		}
		return runMarker(ctx, port)
	}
	return runInputMode(ctx, args, output)
}

func runInputMode(ctx context.Context, args []string, output io.Writer) error {
	if len(args) != 3 || args[1] != "--input" || args[2] != pv.ApplicationInputPath {
		return errors.New("application worker requires the fixed read-only input path")
	}
	switch args[0] {
	case modePrepare, modeBuild, modeSubject, modeObserve, modeBoundary, modeMarker, modeFixture:
	default:
		return pv.ErrEvidence
	}
	if err := installFileSizeLimit(args[0]); err != nil {
		return err
	}
	if args[0] == modePrepare {
		return runPrepare(ctx, output)
	}
	if args[0] == modeBoundary {
		return runBoundary(ctx, output)
	}
	switch args[0] {
	case modeBuild, modeSubject, modeObserve, modeMarker, modeFixture:
	default:
		return pv.ErrEvidence
	}
	input, err := readInput()
	if err != nil {
		return err
	}
	if err := verifyRuntimeManifest(input.Manifest); err != nil {
		return err
	}
	if os.Getenv("ORKA_APPLICATION_TASK_UID") != input.TaskUID ||
		os.Getenv("ORKA_APPLICATION_POD_UID") == "" || os.Getenv("ORKA_APPLICATION_POD_NAME") == "" {
		return pv.ErrBinding
	}
	return runWorkloadMode(ctx, args[0], input, output)
}

func runWorkloadMode(ctx context.Context, mode string, input pv.ApplicationWorkerInput, output io.Writer) error {
	if mode == modeMarker {
		return runMarkerFromInput(ctx, input)
	}
	if mode == modeFixture {
		return runCatalogueFixture(ctx, input)
	}
	bounded, stop := context.WithTimeout(ctx, 10*time.Minute)
	defer stop()
	switch mode {
	case modeBuild:
		return runBuild(bounded, input, output)
	case modeSubject:
		return runSubject(bounded, input)
	case modeObserve:
		return runObserve(bounded, input, output)
	default:
		return pv.ErrEvidence
	}
}

func applicationDeadline(ctx context.Context, value string) (context.Context, context.CancelFunc, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if value == "" {
		bounded, cancel := context.WithCancel(ctx)
		return bounded, cancel, nil
	}
	deadline, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || !time.Now().Before(deadline) {
		return nil, nil, errors.New("application absolute deadline is invalid or expired")
	}
	bounded, cancel := context.WithDeadline(ctx, deadline)
	return bounded, cancel, nil
}

func readInput() (pv.ApplicationWorkerInput, error) {
	file, err := openProjected(pv.ApplicationInputPath)
	if err != nil {
		return pv.ApplicationWorkerInput{}, err
	}
	defer func() { _ = file.Close() }()
	return pv.DecodeApplicationWorkerInput(file)
}

func writeReport(output io.Writer, value any) error {
	content, err := pv.MarshalApplicationReport(value)
	if err != nil {
		return err
	}
	n, err := output.Write(content)
	if err != nil || n != len(content) {
		return errors.New("application report transport was incomplete")
	}
	return nil
}

func installWorker() error {
	source, err := openRegular(pv.ApplicationWorkerPath)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	target, err := os.OpenFile("/runner/worker", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0500)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(target, io.LimitReader(source, 128<<20))
	modeErr := target.Chmod(0555)
	closeErr := target.Close()
	return errors.Join(copyErr, modeErr, closeErr)
}
