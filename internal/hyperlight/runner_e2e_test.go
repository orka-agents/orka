//go:build linux && hyperlight_e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package hyperlight

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHyperlightE2ERunnerColdAndWarm(t *testing.T) {
	cfg := ConfigFromEnv()
	cfg.CacheDir = ""
	cold := NewRunner(cfg)
	var stdout bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	result, err := cold.Run(ctx, Request{Runtime: "bash", Script: "echo cold", Stdout: &stdout})
	if err != nil || result.ExitCode != 0 || result.Warm || stdout.String() != "cold\n" {
		t.Fatalf("real cold guest: result=%+v output=%q error=%v", result, stdout.String(), err)
	}

	cfg.CacheDir = t.TempDir()
	warm := NewRunner(cfg)
	for range 2 {
		stdout.Reset()
		result, err = warm.Run(ctx, Request{Runtime: "bash", Script: "echo warm", Stdout: &stdout})
		if err != nil || result.ExitCode != 0 || !result.Warm || stdout.String() != "warm\n" {
			t.Fatalf("real warm guest: result=%+v output=%q error=%v", result, stdout.String(), err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(cfg.CacheDir, "snapshots"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("snapshot was not reused: entries=%d error=%v", len(entries), err)
	}

	stdout.Reset()
	result, err = warm.Run(ctx, Request{
		Runtime: "bash", Script: "while true; do echo output-budget; done",
		Stdout: &stdout, OutputBudget: 4096,
	})
	if err != nil || !result.OutputExceeded || result.ExitCode != -1 || stdout.Len() > 4096 {
		t.Fatalf("guest output budget: result=%+v bytes=%d error=%v", result, stdout.Len(), err)
	}

	cfg.DevicePaths = []string{filepath.Join(t.TempDir(), "missing-kvm")}
	result, err = NewRunner(cfg).Run(ctx, Request{Runtime: "bash", Script: "echo must-not-run"})
	if !errors.Is(err, ErrUnavailable) || result.ExitCode != -1 {
		t.Fatalf("missing device did not fail closed: result=%+v error=%v", result, err)
	}
}
