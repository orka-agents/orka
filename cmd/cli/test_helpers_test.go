/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"io"
	"os"
	"testing"
)

func captureOutput(t *testing.T, fn func() error) (string, error) {
	t.Helper()

	oldStdout := os.Stdout
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create stdout pipe: %v", err)
	}

	os.Stdout = stdoutWriter
	defer func() {
		os.Stdout = oldStdout
		stdoutReader.Close() //nolint:errcheck
	}()

	runErr := fn()
	stdoutWriter.Close() //nolint:errcheck

	stdout, err := io.ReadAll(stdoutReader)
	if err != nil {
		t.Fatalf("failed to read captured stdout: %v", err)
	}
	return string(stdout), runErr
}
