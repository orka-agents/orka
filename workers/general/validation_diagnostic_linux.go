//go:build linux

package main

import (
	"errors"
	"io"
	"os"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"golang.org/x/sys/unix"
)

const preparationDiagnosticDeliveryFailed = "preparation diagnostic delivery failed\n"

var errPreparationDiagnosticWrite = errors.New("preparation diagnostic delivery failed")

func finishValidationPreparation(runErr error, stderr io.Writer, termination func([]byte) error) error {
	if runErr == nil {
		return nil
	}
	record := pv.PreparationFailureDiagnostic(runErr)
	terminationErr := termination(record)
	n, stderrErr := stderr.Write(record)
	if terminationErr != nil || stderrErr != nil || n != len(record) {
		// Never replace the primary failure or print a sink's arbitrary error.
		_, _ = io.WriteString(stderr, preparationDiagnosticDeliveryFailed)
	}
	return errValidationPreparation
}

func writePreparationTermination(record []byte) error {
	// Only this container's PID1 may write its existing Kubernetes termination
	// mount. Host invocation cannot create or overwrite an unrelated file.
	if os.Getpid() != 1 || len(record) == 0 || len(record) > pv.MaxPreparationDiagnosticBytes {
		return errPreparationDiagnosticWrite
	}
	fd, err := unix.Open("/dev/termination-log", unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return errPreparationDiagnosticWrite
	}
	file := os.NewFile(uintptr(fd), "preparation-termination")
	if file == nil {
		_ = unix.Close(fd)
		return errPreparationDiagnosticWrite
	}
	return writePreparationTerminationFile(file, record)
}

func writePreparationTerminationFile(file *os.File, record []byte) error {
	if len(record) == 0 || len(record) > pv.MaxPreparationDiagnosticBytes {
		_ = file.Close()
		return errPreparationDiagnosticWrite
	}
	info, statErr := file.Stat()
	if statErr != nil || !info.Mode().IsRegular() || file.Truncate(0) != nil {
		_ = file.Close()
		return errPreparationDiagnosticWrite
	}
	n, writeErr := file.Write(record)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil || n != len(record) {
		return errPreparationDiagnosticWrite
	}
	return nil
}
