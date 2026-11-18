//go:build linux

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	pv "github.com/orka-agents/orka/internal/patchverification"
)

func main() {
	os.Exit(workerMain())
}

func workerMain() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	// No flag parser diagnostics, child streams, or observer errors go to logs:
	// the controller consumes the entire container log as one bounded report.
	if len(os.Args) != 3 || os.Args[1] != "--input" || os.Args[2] != "/input/bundle.gz" {
		return 125
	}
	input, err := readInput(os.Args[2])
	if err != nil {
		return 125
	}
	check, err := pv.ValidatePodInput(input)
	if err != nil {
		return 125
	}
	podUID := os.Getenv("ORKA_VALIDATION_POD_UID")
	if !uidPattern.MatchString(podUID) {
		podUID = ""
	}
	report := newReport(input, podUID)
	runBoundInput(ctx, input, check, &report)
	if writeReport(os.Stdout, report) != nil {
		return 125
	}
	return 0
}
