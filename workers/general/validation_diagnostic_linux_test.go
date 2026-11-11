//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/stretchr/testify/require"
)

type failingPreparationWriter struct {
	err   error
	short bool
}

func (w failingPreparationWriter) Write(content []byte) (int, error) {
	if w.short {
		return len(content) - 1, nil
	}
	return 0, w.err
}

func TestPreparationFailureWritesSameBoundedRecordAndKeepsGenericFailure(t *testing.T) {
	var stderr, termination bytes.Buffer
	cause := pv.PreparationFailure(pv.PreparationWorkspace, syscall.ENOSPC)
	err := finishValidationPreparation(cause, &stderr, func(content []byte) error {
		_, err := termination.Write(content)
		return err
	})
	require.ErrorIs(t, err, errValidationPreparation)
	require.Equal(t, errValidationPreparation.Error(), err.Error())
	require.Equal(t, 1, generalWorkerExitCode(err))
	require.Equal(t, stderr.Bytes(), termination.Bytes())
	require.LessOrEqual(t, stderr.Len(), pv.MaxPreparationDiagnosticBytes)
	require.JSONEq(t, `{"version":1,"stage":"workspace","category":"io","errno":28}`, stderr.String())
	// This is the field already retained by the existing Pod-status collector.
	status := map[string]any{"state": map[string]any{"terminated": map[string]any{
		"exitCode": 1, "message": termination.String(),
	}}}
	content, marshalErr := json.Marshal(status)
	require.NoError(t, marshalErr)
	var restored map[string]any
	require.NoError(t, json.Unmarshal(content, &restored))
	require.Equal(t, termination.String(), restored["state"].(map[string]any)["terminated"].(map[string]any)["message"])
}

func TestPreparationDiagnosticWriteFailuresStaySafe(t *testing.T) {
	const private = "DO-NOT-EMIT-secret-source-patch-url-path"
	primary := pv.PreparationFailure(pv.PreparationGitFetch, errors.New(private))
	for _, sinkFailure := range []error{syscall.ENOSPC, syscall.EPIPE, errors.New(private)} {
		var stderr bytes.Buffer
		err := finishValidationPreparation(primary, &stderr, func([]byte) error { return sinkFailure })
		require.Equal(t, errValidationPreparation, err)
		require.Contains(t, stderr.String(), preparationDiagnosticDeliveryFailed)
		require.NotContains(t, stderr.String(), private)
		lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
		require.Len(t, lines, 2)
		require.LessOrEqual(t, len(lines[0])+1, pv.MaxPreparationDiagnosticBytes)
	}
	var termination []byte
	writers := []io.Writer{failingPreparationWriter{err: syscall.EPIPE}, failingPreparationWriter{short: true}}
	for _, writer := range writers {
		err := finishValidationPreparation(primary, writer, func(content []byte) error {
			termination = append([]byte(nil), content...)
			return nil
		})
		require.Equal(t, errValidationPreparation, err)
		require.NotEmpty(t, termination)
		require.NotContains(t, string(termination), private)
	}
}

func TestPreparationSuccessOutputIsUnchangedAndDiagnosticFree(t *testing.T) {
	report := &pv.PreparationReport{
		Version: pv.PreparationProtocolVersion, TaskUID: "synthetic-task", PodUID: "synthetic-pod",
	}
	expected, err := json.Marshal(report)
	require.NoError(t, err)
	expected = append(expected, '\n')
	var stdout, stderr bytes.Buffer
	require.NoError(t, writePreparationReport(&stdout, report))
	require.Equal(t, expected, stdout.Bytes())
	require.NoError(t, finishValidationPreparation(nil, &stderr, func([]byte) error {
		t.Fatal("success must not write a failure termination record")
		return nil
	}))
	require.Zero(t, stderr.Len())
}

func TestPreparationReportOutputClassifiesTypedWriteFailures(t *testing.T) {
	for _, test := range []struct {
		writer io.Writer
		errno  int
	}{
		{failingPreparationWriter{err: syscall.ENOSPC}, int(syscall.ENOSPC)},
		{failingPreparationWriter{err: syscall.EPIPE}, int(syscall.EPIPE)},
		{failingPreparationWriter{short: true}, 0},
	} {
		err := writePreparationReport(test.writer, &pv.PreparationReport{})
		require.Error(t, err)
		var diagnostic map[string]any
		require.NoError(t, json.Unmarshal(pv.PreparationFailureDiagnostic(err), &diagnostic))
		require.Equal(t, "report-write", diagnostic["stage"])
		require.Equal(t, "io", diagnostic["category"])
		if test.errno != 0 {
			require.Equal(t, float64(test.errno), diagnostic["errno"])
		}
		var stderr bytes.Buffer
		returned := finishValidationPreparation(err, &stderr, func([]byte) error { return nil })
		require.Equal(t, errValidationPreparation, returned)
	}
	err := prepareValidationSource(context.Background(), []string{"invalid"}, io.Discard)
	var diagnostic map[string]any
	require.NoError(t, json.Unmarshal(pv.PreparationFailureDiagnostic(err), &diagnostic))
	require.Equal(t, "input-decode", diagnostic["stage"])
	require.Equal(t, "invalid", diagnostic["category"])
}

func TestPreparationTerminationFileContainsOnlyTheBoundedRecord(t *testing.T) {
	name := filepath.Join(t.TempDir(), "termination")
	require.NoError(t, os.WriteFile(name, []byte(strings.Repeat("old-private-data", 100)), 0600))
	file, err := os.OpenFile(name, os.O_WRONLY, 0)
	require.NoError(t, err)
	record := pv.PreparationFailureDiagnostic(pv.PreparationFailure(pv.PreparationReportWrite, syscall.EPIPE))
	require.NoError(t, writePreparationTerminationFile(file, record))
	actual, err := os.ReadFile(name)
	require.NoError(t, err)
	require.Equal(t, record, actual)
	file, err = os.OpenFile(name, os.O_WRONLY, 0)
	require.NoError(t, err)
	oversized := bytes.Repeat([]byte("x"), pv.MaxPreparationDiagnosticBytes+1)
	require.ErrorIs(t, writePreparationTerminationFile(file, oversized), errPreparationDiagnosticWrite)
	actual, err = os.ReadFile(name)
	require.NoError(t, err)
	require.Equal(t, record, actual, "oversized diagnostics must not overwrite the termination file")
	directory, err := os.Open(t.TempDir())
	require.NoError(t, err)
	require.ErrorIs(t, writePreparationTerminationFile(directory, record), errPreparationDiagnosticWrite)
}
