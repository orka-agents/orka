//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"golang.org/x/sys/unix"
)

var errValidationPreparation = errors.New("isolated public-source preparation is unavailable")

func runValidationSourcePreparation(ctx context.Context, args []string) error {
	err := prepareValidationSource(ctx, args, os.Stdout)
	return finishValidationPreparation(err, os.Stderr, writePreparationTermination)
}

func prepareValidationSource(ctx context.Context, args []string, output io.Writer) error {
	if len(args) != 1 || args[0] != "/input/request.json" {
		return pv.PreparationFailure(pv.PreparationInputDecode, pv.ErrEvidence)
	}
	if err := validationPreparationPreflight(); err != nil {
		return pv.PreparationFailure(pv.PreparationIsolation, err)
	}
	file, err := os.Open(args[0])
	if err != nil {
		return pv.PreparationFailure(pv.PreparationInputOpen, err)
	}
	input, readErr := pv.DecodePreparationInput(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return pv.PreparationFailure(pv.PreparationInputDecode, errors.Join(readErr, closeErr))
	}
	if input.TaskUID == "" || os.Getenv("ORKA_VALIDATION_TASK_UID") != input.TaskUID {
		return pv.PreparationFailure(pv.PreparationIdentity, pv.ErrBinding)
	}
	podUID := os.Getenv("ORKA_VALIDATION_POD_UID")
	if podUID == "" || len(podUID) > 128 || strings.ContainsAny(podUID, "\r\n\x00 ") {
		return pv.PreparationFailure(pv.PreparationIdentity, pv.ErrBinding)
	}
	operation, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	report, err := pv.PrepareWorkerSource(operation, input, "/workspace", "/worker")
	if err != nil {
		return err
	}
	report.PodUID = podUID
	if err := pv.ValidatePreparationReport(input, *report); err != nil {
		return pv.PreparationFailure(pv.PreparationReportValidate, err)
	}
	return writePreparationReport(output, report)
}

func writePreparationReport(output io.Writer, report *pv.PreparationReport) error {
	content, err := json.Marshal(report)
	if err != nil {
		return pv.PreparationFailure(pv.PreparationReportEncode, err)
	}
	if len(content)+1 > pv.MaxPreparationReportBytes {
		return pv.PreparationFailure(pv.PreparationReportEncode, pv.ErrLimit)
	}
	content = append(content, '\n')
	n, err := output.Write(content)
	if err != nil {
		return pv.PreparationFailure(pv.PreparationReportWrite, err)
	}
	if n != len(content) {
		return pv.PreparationFailure(pv.PreparationReportWrite, io.ErrShortWrite)
	}
	return nil
}

func validationPreparationPreflight() error {
	return validationPreparationPolicy(true)
}

func validationPreparationPolicy(leader bool) error {
	if (leader && os.Getpid() != 1) || os.Getuid() != 1000 || os.Geteuid() != 1000 ||
		os.Getgid() != 1000 || os.Getegid() != 1000 {
		return unavailablePreparationPolicy(preparationIsolationIdentity)
	}
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return unavailablePreparationPolicy(preparationIsolationStatus)
	}
	fields := map[string]string{}
	for line := range strings.SplitSeq(string(status), "\n") {
		if key, value, ok := strings.Cut(line, ":"); ok {
			fields[key] = strings.TrimSpace(value)
		}
	}
	for _, key := range []string{"CapEff", "CapPrm", "CapBnd", "CapInh", "CapAmb"} {
		if value, err := strconv.ParseUint(fields[key], 16, 64); err != nil || value != 0 {
			return unavailablePreparationPolicy(preparationIsolationCapabilities)
		}
	}
	if fields["NoNewPrivs"] != "1" || fields["Seccomp"] != "2" {
		return unavailablePreparationPolicy(preparationIsolationProcessSecurity)
	}
	var root, work unix.Statfs_t
	if unix.Statfs("/", &root) != nil || root.Flags&unix.ST_RDONLY == 0 ||
		unix.Statfs("/workspace", &work) != nil || work.Type != unix.TMPFS_MAGIC ||
		work.Bsize <= 0 || work.Blocks > (256<<20)/uint64(work.Bsize) {
		return unavailablePreparationPolicy(preparationIsolationFilesystem)
	}
	if _, err := os.Lstat("/var/run/secrets/kubernetes.io/serviceaccount/token"); !os.IsNotExist(err) {
		return unavailablePreparationPolicy(preparationIsolationCredentials)
	}
	if err := pv.CheckWorkerCgroupBounds(); err != nil {
		return fmt.Errorf("%w: %w", errValidationPreparation, err)
	}
	return nil
}

func unavailablePreparationPolicy(reason preparationIsolationFailure) error {
	return fmt.Errorf("%w: %w", errValidationPreparation, reason)
}

func runBoundedValidationGit(args []string) error {
	if len(args) < 2 || args[0] != "/usr/bin/git" || validationPreparationPolicy(false) != nil {
		return errValidationPreparation
	}
	// Go may allocate while preparing exec arguments. Apply address-space limits
	// only after replacing it with the single-threaded native launcher.
	const launcher = "/usr/local/libexec/orka-validation-git"
	if unix.Exec(launcher, append([]string{launcher}, args...), os.Environ()) != nil {
		return errValidationPreparation
	}
	return nil
}
