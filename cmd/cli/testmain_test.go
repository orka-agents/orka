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

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeKubectlEnv); mode != "" {
		os.Exit(fakeKubectl(mode, os.Args[1:]))
	}
	os.Exit(runWithoutAmbientCluster(m))
}

func runWithoutAmbientCluster(m *testing.M) int {
	if err := pinGoToolEnvironment(); err != nil {
		fmt.Fprintln(os.Stderr, "warning: Go tool environment not pinned; tests that run go may rebuild their caches:", err)
	}
	dir, err := os.MkdirTemp("", "orka-cli-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create isolated CLI test home:", err)
		return 1
	}
	defer os.RemoveAll(dir) //nolint:errcheck
	isolated := map[string]string{"HOME": dir, "KUBECONFIG": filepath.Join(dir, "missing-kubeconfig")}
	if runtime.GOOS == "windows" {
		isolated["USERPROFILE"] = dir
	}
	for key, value := range isolated {
		if err := os.Setenv(key, value); err != nil {
			fmt.Fprintln(os.Stderr, "isolate CLI test environment:", err)
			return 1
		}
	}
	for _, key := range []string{"KUBERNETES_SERVICE_HOST", "KUBERNETES_SERVICE_PORT", envServer, envToken, envNamespace} {
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
