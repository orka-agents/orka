/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStageCopiesAndReplacesTheTokenAndTheAgent(t *testing.T) {
	from := writeFile(t, "token", testToken)
	dir := t.TempDir()
	tokenTo, agentTo := filepath.Join(dir, "token"), filepath.Join(dir, "orka-host-agent")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		// The second run replaces files the first left behind.
		if err := stage([]string{"--token-from", from, "--token-to", tokenTo, "--agent-to", agentTo}); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(tokenTo)
		if err != nil || string(got) != testToken {
			t.Fatalf("staged token = %q, %v", got, err)
		}
		info, err := os.Stat(tokenTo)
		if err != nil || info.Mode().Perm()&0o222 != 0 {
			t.Fatalf("staged token mode = %v, %v; want read-only", info.Mode(), err)
		}
		got, err = os.ReadFile(agentTo)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("staged agent differs from this binary: %v", err)
		}
		info, err = os.Stat(agentTo)
		if err != nil || info.Mode().Perm() != 0o555 {
			t.Fatalf("staged agent mode = %v, %v; want 0555", info.Mode(), err)
		}
	}
}

func TestStageRefusesAMissingOrEmptyToken(t *testing.T) {
	dir := t.TempDir()
	tokenTo, agentTo := filepath.Join(dir, "token"), filepath.Join(dir, "orka-host-agent")
	for name, args := range map[string][]string{
		"no flags":     nil,
		"no agent":     {"--token-from", writeFile(t, "token", testToken), "--token-to", tokenTo},
		"missing file": {"--token-from", filepath.Join(t.TempDir(), "missing"), "--token-to", tokenTo, "--agent-to", agentTo},
		"empty token":  {"--token-from", writeFile(t, "token", " \n"), "--token-to", tokenTo, "--agent-to", agentTo},
		"oversized": {
			"--token-from", writeFile(t, "token", strings.Repeat("a", maxTokenBytes+1)),
			"--token-to", tokenTo, "--agent-to", agentTo,
		},
	} {
		err := stage(args)
		if err == nil {
			t.Errorf("%s: staged", name)
		} else if strings.Contains(err.Error(), testToken) {
			t.Errorf("%s: error leaked the token: %v", name, err)
		}
	}
	for _, path := range []string{tokenTo, agentTo} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("a refused stage wrote %s: %v", path, err)
		}
	}
}
