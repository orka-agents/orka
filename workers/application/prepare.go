//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"golang.org/x/sys/unix"
)

func runPrepare(ctx context.Context, output io.Writer) error {
	if _, err := measureProcess(); err != nil {
		return err
	}
	if err := noTargetCredentials(); err != nil {
		return err
	}
	file, err := openProjected(pv.ApplicationInputPath)
	if err != nil {
		return err
	}
	input, readErr := pv.DecodeApplicationPreparationInput(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || input.TaskUID != os.Getenv("ORKA_APPLICATION_TASK_UID") ||
		os.Getenv("ORKA_APPLICATION_POD_UID") == "" {
		return pv.ErrBinding
	}
	if err := verifyRuntimeManifest(input.Manifest); err != nil {
		return err
	}
	operation, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	report, err := pv.PrepareApplicationSource(operation, input, pv.ApplicationWorkPath, pv.ApplicationWorkerPath)
	if err != nil {
		return err
	}
	report.PodUID = os.Getenv("ORKA_APPLICATION_POD_UID")
	if err := pv.ValidateApplicationPreparationReport(input, *report); err != nil {
		return err
	}
	content, err := json.Marshal(report)
	if err != nil || len(content)+1 > pv.MaxApplicationPreparationReportBytes || pv.ContainsCredentialMaterial(content) {
		return pv.ErrLimit
	}
	content = append(content, '\n')
	if err := operation.Err(); err != nil {
		return err
	}
	n, err := output.Write(content)
	if err != nil || n != len(content) {
		return errors.New("application preparation report transport was incomplete")
	}
	return nil
}

func runGit(args []string) error {
	if len(args) < 2 || args[0] != "/usr/bin/git" {
		return pv.ErrEvidence
	}
	if err := installFileSizeLimit(modeGit); err != nil {
		return err
	}
	if _, err := measureProcessWithLeader(false); err != nil {
		return err
	}
	if err := noTargetCredentials(); err != nil {
		return err
	}
	for _, limit := range []struct {
		resource int
		value    uint64
	}{
		{unix.RLIMIT_NOFILE, 256}, {unix.RLIMIT_FSIZE, 512 << 20},
		{unix.RLIMIT_CORE, 0}, {unix.RLIMIT_CPU, 120},
	} {
		if err := unix.Setrlimit(limit.resource, &unix.Rlimit{Cur: limit.value, Max: limit.value}); err != nil {
			return errors.New("application public-source process limit could not be installed")
		}
	}
	return unix.Exec("/usr/bin/git", args, os.Environ())
}
