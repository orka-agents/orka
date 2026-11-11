//go:build linux

package patchverification

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"syscall"
)

const MaxPreparationDiagnosticBytes = 512

const (
	preparationInternal = "internal"
	preparationExec     = "exec"
	preparationExit     = "exit"
	preparationSignal   = "signal"
)

type PreparationStage string

const (
	PreparationInputOpen       PreparationStage = "input-open"
	PreparationInputDecode     PreparationStage = "input-decode"
	PreparationIdentity        PreparationStage = "identity"
	PreparationIsolation       PreparationStage = "isolation"
	PreparationWorkspace       PreparationStage = "workspace"
	PreparationGitInit         PreparationStage = "git-init"
	PreparationGitFetch        PreparationStage = "git-fetch"
	PreparationGitImport       PreparationStage = "git-import"
	PreparationOriginalArchive PreparationStage = "original-archive"
	PreparationPatchValidate   PreparationStage = "patch-validate"
	PreparationPatchApply      PreparationStage = "patch-apply"
	PreparationPatchedArchive  PreparationStage = "patched-archive"
	PreparationDiff            PreparationStage = "diff"
	PreparationReportValidate  PreparationStage = "report-validate"
	PreparationReportEncode    PreparationStage = "report-encode"
	PreparationReportWrite     PreparationStage = "report-write"
)

type preparationDiagnostic struct {
	Version  int    `json:"version"`
	Stage    string `json:"stage"`
	Category string `json:"category"`
	Reason   string `json:"reason,omitempty"`
	ExitCode int    `json:"exitCode,omitempty"`
	Signal   int    `json:"signal,omitempty"`
	Errno    int    `json:"errno,omitempty"`
}

type preparationDiagnosticError struct {
	diagnostic preparationDiagnostic
	cause      error
}

func (e *preparationDiagnosticError) Error() string { return "source preparation failed" }
func (e *preparationDiagnosticError) Unwrap() error { return e.cause }

// PreparationFailure attaches only fixed phase metadata. Error text and
// subprocess stderr never participate in classification or rendering.
func PreparationFailure(stage PreparationStage, err error) error {
	if err == nil {
		return nil
	}
	diagnostic := classifyPreparationError(err)
	if diagnostic.Stage == "" {
		diagnostic.Stage = string(stage)
	}
	if stage == PreparationIsolation && diagnostic.Category == preparationInternal {
		diagnostic.Category = string(PreparationIsolation)
	}
	return &preparationDiagnosticError{diagnostic: sanitizePreparationDiagnostic(diagnostic), cause: err}
}

// PreparationFailureDiagnostic is a private worker diagnostic, not execution
// evidence. The emitted line is bounded even for malformed internal metadata.
func PreparationFailureDiagnostic(err error) []byte {
	diagnostic := sanitizePreparationDiagnostic(classifyPreparationError(err))
	content, marshalErr := json.Marshal(diagnostic)
	if marshalErr != nil || len(content)+1 > MaxPreparationDiagnosticBytes {
		return []byte("{\"version\":1,\"stage\":\"internal\",\"category\":\"internal\"}\n")
	}
	return append(content, '\n')
}

func classifyPreparationError(err error) preparationDiagnostic {
	var previous *preparationDiagnosticError
	if errors.As(err, &previous) && previous != nil {
		return previous.diagnostic
	}
	result := preparationDiagnostic{Category: preparationInternal}
	var isolation interface{ PreparationIsolationReason() string }
	if errors.As(err, &isolation) {
		result.Reason = isolation.PreparationIsolationReason()
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		result.Category = "timeout"
	case errors.Is(err, context.Canceled):
		result.Category = "cancelled"
	case errors.Is(err, ErrLimit), errors.Is(err, errSourceLimit):
		result.Category = "limit"
	case errors.Is(err, ErrIntegrity), errors.Is(err, errSourceIntegrity):
		result.Category = "integrity"
	case errors.Is(err, ErrBinding):
		result.Category = string(PreparationIdentity)
	case errors.Is(err, ErrEvidence), errors.Is(err, errSourceInput), errors.Is(err, errSourcePath):
		result.Category = "invalid"
	default:
		var exited *exec.ExitError
		var errno syscall.Errno
		switch {
		case errors.As(err, &exited):
			result.Category = preparationExec
			if exited != nil && exited.ProcessState != nil {
				result.Category, result.ExitCode = preparationExit, exited.ExitCode()
				if status, ok := exited.Sys().(syscall.WaitStatus); ok && status.Signaled() {
					result.Category, result.ExitCode, result.Signal = preparationSignal, 0, int(status.Signal())
				}
			}
		case errors.As(err, &errno):
			result.Category = "io"
			if allowedPreparationErrno(errno) {
				result.Errno = int(errno)
			}
		case errors.Is(err, io.ErrShortWrite), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.ErrClosedPipe):
			result.Category = "io"
		case errors.Is(err, errSourceGit):
			result.Category = preparationExec
		}
	}
	return result
}

func allowedPreparationErrno(value syscall.Errno) bool {
	switch value {
	case syscall.EACCES, syscall.EAGAIN, syscall.EDQUOT, syscall.EEXIST, syscall.EFBIG,
		syscall.EINVAL, syscall.EIO, syscall.EMFILE, syscall.ENFILE, syscall.ENOENT,
		syscall.ENOMEM, syscall.ENOSPC, syscall.ENOTDIR, syscall.EPERM, syscall.EPIPE,
		syscall.EROFS, syscall.ETIMEDOUT:
		return true
	default:
		return false
	}
}

func sanitizePreparationDiagnostic(value preparationDiagnostic) preparationDiagnostic {
	value.Version = 1
	switch PreparationStage(value.Stage) {
	case PreparationInputOpen, PreparationInputDecode, PreparationIdentity, PreparationIsolation, PreparationWorkspace,
		PreparationGitInit, PreparationGitFetch, PreparationGitImport, PreparationOriginalArchive,
		PreparationPatchValidate, PreparationPatchApply, PreparationPatchedArchive, PreparationDiff,
		PreparationReportValidate, PreparationReportEncode, PreparationReportWrite:
	default:
		value.Stage = preparationInternal
	}
	switch value.Category {
	case "invalid", string(PreparationIdentity), string(PreparationIsolation), "limit", "integrity", "timeout", "cancelled", preparationExit, preparationSignal, "io", preparationExec:
	default:
		value.Category = preparationInternal
	}
	if value.Stage != string(PreparationIsolation) || value.Category != string(PreparationIsolation) {
		value.Reason = ""
	} else {
		switch value.Reason {
		case string(PreparationIdentity), "process-status", "capabilities", "process-security", "filesystem", "credentials",
			"cgroup-memory", "cgroup-pids", "cgroup-swap", "cgroup-cpu":
		default:
			value.Reason = ""
		}
	}
	if value.Category != preparationExit || value.ExitCode < 1 || value.ExitCode > 255 {
		value.ExitCode = 0
	}
	if value.Category != preparationSignal || value.Signal < 1 || value.Signal > 64 {
		value.Signal = 0
	}
	if value.Category != "io" || !allowedPreparationErrno(syscall.Errno(value.Errno)) {
		value.Errno = 0
	}
	return value
}

func preparationCommandError(err error) error {
	// Preserve only typed numeric process/OS data before replacing the command
	// error with the existing safe sentinel. In particular, do not retain stderr.
	return &preparationDiagnosticError{diagnostic: classifyPreparationError(err), cause: errSourceGit}
}
