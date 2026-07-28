/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package utils

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigureAndValidateE2EKindTargetActivatesStateKubeconfig(t *testing.T) {
	tempDir := t.TempDir()
	kubectl := writeFakeKubectl(t, tempDir)
	targetKubeconfig := writeFakeKubeconfig(t, tempDir, "target", "kind-target", "kind-target", "identity-target")
	ambientKubeconfig := writeFakeKubeconfig(t, tempDir, "ambient", "kind-decoy", "kind-decoy", "identity-decoy")
	stateDir := writeE2EState(t, tempDir, targetKubeconfig)

	t.Setenv("KIND_CLUSTER", "target")
	t.Setenv("KUBECTL", kubectl)
	t.Setenv("KUBECONFIG", ambientKubeconfig)
	t.Setenv("E2E_CLUSTER_STATE_DIR", stateDir)
	t.Setenv("FAKE_KUBECTL_WARNING", "1")

	if err := ConfigureAndValidateE2EKindTarget(); err != nil {
		t.Fatalf("ConfigureAndValidateE2EKindTarget() error = %v", err)
	}
	expectedKubeconfig := filepath.Join(stateDir, "target.kubeconfig")
	if got := os.Getenv("KUBECONFIG"); got != expectedKubeconfig {
		t.Fatalf("KUBECONFIG = %q, want %q", got, expectedKubeconfig)
	}
}

func TestConfigureAndValidateE2EKindTargetRejectsMismatches(t *testing.T) {
	tests := []struct {
		name           string
		currentContext string
		currentCluster string
		wantError      string
	}{
		{
			name:           "context",
			currentContext: "kind-decoy",
			currentCluster: "kind-target",
			wantError:      "kubeconfig context",
		},
		{
			name:           "cluster",
			currentContext: "kind-target",
			currentCluster: "kind-decoy",
			wantError:      "kubeconfig cluster",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempDir := t.TempDir()
			kubectl := writeFakeKubectl(t, tempDir)
			kubeconfig := writeFakeKubeconfig(t, tempDir, "target", tt.currentContext, tt.currentCluster, "identity-target")
			stateDir := writeE2EState(t, tempDir, kubeconfig)

			t.Setenv("KIND_CLUSTER", "target")
			t.Setenv("KUBECTL", kubectl)
			t.Setenv("KUBECONFIG", "")
			t.Setenv("E2E_CLUSTER_STATE_DIR", stateDir)

			err := ConfigureAndValidateE2EKindTarget()
			if err == nil {
				t.Fatal("ConfigureAndValidateE2EKindTarget() succeeded unexpectedly")
			}
			if !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("error = %q, want substring %q", err, tt.wantError)
			}
		})
	}
}

func TestConfigureAndValidateE2EKindTargetRejectsMatchingAmbientKubeconfig(t *testing.T) {
	tempDir := t.TempDir()
	kubectl := writeFakeKubectl(t, tempDir)
	ambientKubeconfig := writeFakeKubeconfig(t, tempDir, "ambient", "kind-target", "kind-target", "identity-target")
	t.Setenv("KIND_CLUSTER", "target")
	t.Setenv("KUBECTL", kubectl)
	t.Setenv("KUBECONFIG", ambientKubeconfig)
	t.Setenv("E2E_CLUSTER_STATE_DIR", filepath.Join(tempDir, "missing-state"))

	err := ConfigureAndValidateE2EKindTarget()
	if err == nil {
		t.Fatal("ConfigureAndValidateE2EKindTarget() accepted matching labels without helper state")
	}
	if !strings.Contains(err.Error(), "no isolated e2e cluster state") {
		t.Fatalf("error = %q, want missing isolated state failure", err)
	}
}

func TestConfigureAndValidateE2EKindTargetRejectsReplacedKubeconfig(t *testing.T) {
	tempDir := t.TempDir()
	kubectl := writeFakeKubectl(t, tempDir)
	kubeconfig := writeFakeKubeconfig(t, tempDir, "target", "kind-target", "kind-target", "identity-target")
	stateDir := writeE2EState(t, tempDir, kubeconfig)
	writeFakeKubeconfigAtPath(
		t, filepath.Join(stateDir, "target.kubeconfig"),
		"kind-target", "kind-target", "identity-replacement",
	)

	t.Setenv("KIND_CLUSTER", "target")
	t.Setenv("KUBECTL", kubectl)
	t.Setenv("KUBECONFIG", "")
	t.Setenv("E2E_CLUSTER_STATE_DIR", stateDir)

	err := ConfigureAndValidateE2EKindTarget()
	if err == nil {
		t.Fatal("ConfigureAndValidateE2EKindTarget() accepted a replaced kubeconfig")
	}
	if !strings.Contains(err.Error(), "identity does not match helper state") {
		t.Fatalf("error = %q, want identity mismatch", err)
	}
}

func TestConfigureAndValidateE2EKindTargetRejectsNoncanonicalKubeconfigPath(t *testing.T) {
	tempDir := t.TempDir()
	kubectl := writeFakeKubectl(t, tempDir)
	targetKubeconfig := writeFakeKubeconfig(
		t, tempDir, "target", "kind-target", "kind-target", "identity-target",
	)
	stateDir := writeE2EState(t, tempDir, targetKubeconfig)
	ambientKubeconfig := writeFakeKubeconfig(
		t, tempDir, "ambient", "kind-target", "kind-target", "identity-target",
	)
	if err := os.WriteFile(filepath.Join(stateDir, "kubeconfig"), []byte(ambientKubeconfig+"\n"), 0o600); err != nil {
		t.Fatalf("replace state kubeconfig path: %v", err)
	}

	t.Setenv("KIND_CLUSTER", "target")
	t.Setenv("KUBECTL", kubectl)
	t.Setenv("KUBECONFIG", "")
	t.Setenv("E2E_CLUSTER_STATE_DIR", stateDir)

	err := ConfigureAndValidateE2EKindTarget()
	if err == nil {
		t.Fatal("ConfigureAndValidateE2EKindTarget() accepted a noncanonical kubeconfig path")
	}
	if !strings.Contains(err.Error(), "is not canonical") {
		t.Fatalf("error = %q, want canonical path failure", err)
	}
}

func TestConfigureAndValidateE2EKindTargetRequiresMatchingLeaseState(t *testing.T) {
	tempDir := t.TempDir()
	kubectl := writeFakeKubectl(t, tempDir)
	kubeconfig := writeFakeKubeconfig(
		t, tempDir, "target", "kind-target", "kind-target", "identity-target",
	)
	stateDir := writeE2EState(t, tempDir, kubeconfig)
	leaseState := filepath.Join(os.Getenv("E2E_CLUSTER_LOCK_ROOT"), "kind-target.lease", "state_dir")
	if err := os.WriteFile(leaseState, []byte(filepath.Join(tempDir, "other-state")+"\n"), 0o600); err != nil {
		t.Fatalf("replace lease state claim: %v", err)
	}
	t.Setenv("KIND_CLUSTER", "target")
	t.Setenv("KUBECTL", kubectl)
	t.Setenv("E2E_CLUSTER_STATE_DIR", stateDir)

	err := ConfigureAndValidateE2EKindTarget()
	if err == nil {
		t.Fatal("ConfigureAndValidateE2EKindTarget() accepted a lease for another state directory")
	}
	if !strings.Contains(err.Error(), "bound to a different state directory") {
		t.Fatalf("error = %q, want lease state mismatch", err)
	}
}

func TestConfigureAndValidateE2EKindTargetRequiresActiveHelperOperation(t *testing.T) {
	tempDir := t.TempDir()
	kubectl := writeFakeKubectl(t, tempDir)
	kubeconfig := writeFakeKubeconfig(
		t, tempDir, "target", "kind-target", "kind-target", "identity-target",
	)
	stateDir := writeE2EState(t, tempDir, kubeconfig)
	t.Setenv("KIND_CLUSTER", "target")
	t.Setenv("KUBECTL", kubectl)
	t.Setenv("E2E_CLUSTER_STATE_DIR", stateDir)
	t.Setenv("E2E_KIND_OPERATION_TOKEN", "wrong-token")

	err := ConfigureAndValidateE2EKindTarget()
	if err == nil {
		t.Fatal("ConfigureAndValidateE2EKindTarget() accepted a run without the active helper token")
	}
	if !strings.Contains(err.Error(), "does not match the active cluster lock") {
		t.Fatalf("error = %q, want active operation claim failure", err)
	}
}

func TestConfigureAndValidateE2EKindTargetRequiresIsolatedKubeconfig(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("KIND_CLUSTER", "target")
	t.Setenv("KUBECONFIG", "")
	t.Setenv("E2E_CLUSTER_STATE_DIR", filepath.Join(tempDir, "missing-state"))

	err := ConfigureAndValidateE2EKindTarget()
	if err == nil {
		t.Fatal("ConfigureAndValidateE2EKindTarget() succeeded unexpectedly")
	}
	if !strings.Contains(err.Error(), "no isolated e2e cluster state") {
		t.Fatalf("error = %q, want missing isolated state failure", err)
	}
}

func writeFakeKubectl(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "kubectl")
	body := `#!/usr/bin/env bash
set -euo pipefail
if [[ "${FAKE_KUBECTL_WARNING:-0}" == "1" ]]; then
  printf 'warning: harmless test warning\n' >&2
fi
if [[ "${1:-}" != "--kubeconfig" ]]; then
  exit 91
fi
kubeconfig="$2"
shift 2
case "$*" in
  "config current-context")
    sed -n 's/^context=//p' "${kubeconfig}"
    ;;
  "config view --minify -o jsonpath={.contexts[0].context.cluster}")
    sed -n 's/^cluster=//p' "${kubeconfig}"
    ;;
  *".clusters[0].cluster.server"*)
    sed -n 's/^server=//p' "${kubeconfig}"
    sed -n 's/^ca=//p' "${kubeconfig}"
    ;;
  *)
    exit 92
    ;;
esac
`
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write fake kubectl: %v", err)
	}
	lockRoot := filepath.Join(dir, "locks")
	operationDir := filepath.Join(lockRoot, "kind-target.operation")
	if err := os.MkdirAll(operationDir, 0o700); err != nil {
		t.Fatalf("create fake operation lock: %v", err)
	}
	const operationToken = "test-operation-token"
	if err := os.WriteFile(filepath.Join(operationDir, "token"), []byte(operationToken+"\n"), 0o600); err != nil {
		t.Fatalf("write fake operation token: %v", err)
	}
	t.Setenv("E2E_KIND_TARGET_READY", "1")
	t.Setenv("E2E_KIND_EXPECTED_CONTEXT", "kind-target")
	t.Setenv("E2E_KIND_EXPECTED_CLUSTER", "kind-target")
	t.Setenv("E2E_CLUSTER_LOCK_ROOT", lockRoot)
	t.Setenv("E2E_KIND_OPERATION_TOKEN", operationToken)
	return path
}

func writeFakeKubeconfig(t *testing.T, dir, name, context, cluster, identity string) string {
	t.Helper()
	path := filepath.Join(dir, name+".kubeconfig")
	writeFakeKubeconfigAtPath(t, path, context, cluster, identity)
	return path
}

func writeFakeKubeconfigAtPath(t *testing.T, path, context, cluster, identity string) {
	t.Helper()
	body := "context=" + context + "\ncluster=" + cluster + "\nserver=https://" + identity + "\nca=ca-" + identity + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fake kubeconfig: %v", err)
	}
}

func fakeKubeconfigIdentity(t *testing.T, kubeconfig string) string {
	t.Helper()
	content, err := os.ReadFile(kubeconfig)
	if err != nil {
		t.Fatalf("read fake kubeconfig: %v", err)
	}
	var server, ca string
	for line := range strings.SplitSeq(string(content), "\n") {
		if value, ok := strings.CutPrefix(line, "server="); ok {
			server = value
		}
		if value, ok := strings.CutPrefix(line, "ca="); ok {
			ca = value
		}
	}
	if server == "" || ca == "" {
		t.Fatal("fake kubeconfig identity is incomplete")
	}
	return server + "\n" + ca
}

func writeE2EState(t *testing.T, dir, kubeconfig string) string {
	t.Helper()
	stateDir := filepath.Join(dir, "state")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	canonicalKubeconfig := filepath.Join(stateDir, "target.kubeconfig")
	content, err := os.ReadFile(kubeconfig)
	if err != nil {
		t.Fatalf("read source kubeconfig: %v", err)
	}
	if err := os.WriteFile(canonicalKubeconfig, content, 0o600); err != nil {
		t.Fatalf("write canonical kubeconfig: %v", err)
	}
	identityMaterial := fakeKubeconfigIdentity(t, canonicalKubeconfig)
	lockRoot := os.Getenv("E2E_CLUSTER_LOCK_ROOT")
	for _, claimPath := range []string{
		filepath.Join(lockRoot, "kind-target.operation", "state_dir"),
		filepath.Join(lockRoot, "kind-target.lease", "state_dir"),
	} {
		if err := os.MkdirAll(filepath.Dir(claimPath), 0o700); err != nil {
			t.Fatalf("create state claim dir: %v", err)
		}
		if err := os.WriteFile(claimPath, []byte(stateDir+"\n"), 0o600); err != nil {
			t.Fatalf("write state claim: %v", err)
		}
	}
	values := map[string]string{
		"version":     "1",
		"status":      "ready",
		"cluster":     "target",
		"context":     "kind-target",
		"kubeconfig":  canonicalKubeconfig,
		"created":     "0",
		"fingerprint": fmt.Sprintf("%x", sha256.Sum256([]byte(identityMaterial))),
	}
	for name, value := range values {
		if err := os.WriteFile(filepath.Join(stateDir, name), []byte(value+"\n"), 0o600); err != nil {
			t.Fatalf("write state %s: %v", name, err)
		}
	}
	return stateDir
}
