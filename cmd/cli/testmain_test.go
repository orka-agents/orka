/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestMain keeps the CLI tests away from the developer's real cluster and home
// directory. A command run without --server falls back to kubeconfig service
// discovery and starts a `kubectl port-forward` that deliberately outlives the
// CLI, so a test could leave one running against a real cluster and cache it in
// the real ~/.orka. Tests that need either pass --kubeconfig or set HOME.
func TestMain(m *testing.M) {
	os.Exit(runWithoutAmbientCluster(m))
}

func runWithoutAmbientCluster(m *testing.M) int {
	// Go commands run by tests must keep using the real build and module
	// caches and Go environment file, which default to locations under HOME.
	// Only tests that run `go` depend on this, so a failure is a warning.
	if err := pinGoToolEnvironment(); err != nil {
		fmt.Fprintln(os.Stderr, "warning: Go tool environment not pinned; tests that run go may rebuild their caches:", err)
	}
	dir, err := os.MkdirTemp("", "orka-cli-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create isolated CLI test home:", err)
		return 1
	}
	defer os.RemoveAll(dir) //nolint:errcheck
	// client-go skips a missing KUBECONFIG file instead of falling back to
	// ~/.kube/config, and without the service variables it cannot fall back to
	// in-cluster configuration either, so discovery finds no cluster.
	isolated := map[string]string{"HOME": dir, "KUBECONFIG": filepath.Join(dir, "missing-kubeconfig")}
	if runtime.GOOS == "windows" {
		// os.UserHomeDir reads USERPROFILE rather than HOME on Windows.
		isolated["USERPROFILE"] = dir
	}
	for key, value := range isolated {
		if err := os.Setenv(key, value); err != nil {
			fmt.Fprintln(os.Stderr, "isolate CLI test environment:", err)
			return 1
		}
	}
	for _, key := range []string{"KUBERNETES_SERVICE_HOST", "KUBERNETES_SERVICE_PORT"} {
		if err := os.Unsetenv(key); err != nil {
			fmt.Fprintln(os.Stderr, "isolate CLI test environment:", err)
			return 1
		}
	}
	return m.Run()
}

func pinGoToolEnvironment() error {
	keys := []string{"GOCACHE", "GOENV", "GOMODCACHE", "GOPATH"}
	output, err := exec.Command("go", append([]string{"env", "-json"}, keys...)...).Output()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(exitErr.Stderr))
	}
	if err != nil {
		return err
	}
	var resolved map[string]string
	if err := json.Unmarshal(output, &resolved); err != nil {
		return err
	}
	for _, key := range keys {
		if os.Getenv(key) != "" || resolved[key] == "" {
			continue
		}
		if err := os.Setenv(key, resolved[key]); err != nil {
			return err
		}
	}
	return nil
}
