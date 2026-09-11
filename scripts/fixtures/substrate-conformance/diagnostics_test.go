package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/workspace"
	"github.com/stretchr/testify/require"
)

func TestCommandFailurePreservesDiagnosticsWithoutCredentials(t *testing.T) {
	cfg := workspace.SubstrateConfig{HandoffToken: "fixture-handoff-value", BootstrapToken: "fixture-bootstrap-value"}
	result := &workspace.ExecResult{
		ExitCode: 1, Stdout: "never-print-command-output", Stderr: "permission denied\n" + cfg.BootstrapToken,
	}
	err := workspace.NewError("exec", workspace.ErrorKindCommandFailed, "command failed", false,
		errors.New("rejected "+cfg.HandoffToken+" Authorization: Bearer fixture-header-value"))
	message := commandFailure("native command", cfg, result, err).Error()
	require.Contains(t, message, "kind=CommandFailed")
	require.Contains(t, message, "exit=1")
	require.Contains(t, message, `permission denied\n`)
	for _, value := range []string{cfg.HandoffToken, cfg.BootstrapToken, result.Stdout, "fixture-header-value"} {
		require.NotContains(t, message, value)
	}
	result.Stderr = strings.Repeat("x", maxCommandFailureBytes-70) + cfg.BootstrapToken + strings.Repeat("x", 100)
	message = commandFailure("native command", cfg, result, nil).Error()
	require.LessOrEqual(t, len(message), maxCommandFailureBytes+len("native command:  (truncated)"))
	require.Contains(t, message, "(truncated)")
	require.NotContains(t, message, "fixture-")
	require.Contains(t, commandFailure("native command", cfg, nil, err).Error(), "result=nil")
}

func TestVerifyCommandTimeoutRejectsUnrelatedFailures(t *testing.T) {
	commandErr := workspace.NewError("exec", workspace.ErrorKindCommandFailed, "command failed", false, nil)
	for _, tc := range []struct {
		name   string
		result *workspace.ExecResult
		err    error
		valid  bool
	}{
		{"daemon timeout", &workspace.ExecResult{ExitCode: 124}, commandErr, true},
		{"transport failure", nil, errors.New("router connection reset"), false},
		{
			"authentication failure", nil,
			workspace.NewError("exec", workspace.ErrorKindFailedPrecondition, "credential rejected", false, nil), false,
		},
		{
			"client deadline", nil,
			workspace.NewError("exec", workspace.ErrorKindTimeout, "request deadline exceeded", false, nil), false,
		},
		{"no result", nil, nil, false},
		{"successful command", &workspace.ExecResult{}, nil, false},
		{"different exit", &workspace.ExecResult{ExitCode: 1}, commandErr, false},
		{"missing command error", &workspace.ExecResult{ExitCode: 124}, nil, false},
		{"unrelated error", &workspace.ExecResult{ExitCode: 124}, errors.New("request failed"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyCommandTimeout(workspace.SubstrateConfig{}, tc.result, tc.err)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "did not return the daemon timeout result")
			}
		})
	}
}
