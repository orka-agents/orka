package common

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sozercan/orka/internal/security"
)

func TestEnsureRequiredSecurityArtifactsFollowUpWritesMissingArtifact(t *testing.T) {
	cleanupSecurityArtifactsDir(t)

	cfg := &AgentConfig{
		Prompt: "REQUIRED_SECURITY_ARTIFACTS: security-findings.json",
	}

	findings := security.FindingsArtifact{
		Version: 1,
		Repository: security.FindingsArtifactRepo{
			RepoURL: "https://github.com/sozercan/actions-test.git",
			Branch:  "main",
		},
		Scan: security.FindingsArtifactScan{
			Mode:    "initial",
			Summary: "No findings in scope",
		},
		Findings: []security.FindingsArtifactFinding{},
	}
	data, err := json.Marshal(findings)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	result, err := EnsureRequiredSecurityArtifacts(
		context.Background(),
		cfg,
		"analysis summary only",
		func(_ context.Context, prompt string) (string, error) {
			if !strings.Contains(prompt, "security-findings.json") {
				t.Fatalf("follow-up prompt = %q, want security-findings.json", prompt)
			}
			if err := WriteArtifactFile(security.ArtifactFindings, data); err != nil {
				return "", err
			}
			return "SECURITY_ARTIFACTS_WRITTEN", nil
		},
	)
	if err != nil {
		t.Fatalf("EnsureRequiredSecurityArtifacts() error = %v", err)
	}
	if !strings.Contains(result, "SECURITY_ARTIFACTS_WRITTEN") {
		t.Fatalf("result = %q, want follow-up confirmation appended", result)
	}

	saved, err := os.ReadFile(filepath.Join(artifactsDir, security.ArtifactFindings))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(saved) != string(data) {
		t.Fatalf("saved artifact = %s, want %s", string(saved), string(data))
	}
}

func TestEnsureRequiredSecurityArtifactsRecoversFromTranscript(t *testing.T) {
	cleanupSecurityArtifactsDir(t)

	cfg := &AgentConfig{
		Prompt: "REQUIRED_SECURITY_ARTIFACTS: security-findings.json",
	}

	transcript := "cat > /workspace/.orka-artifacts/security-findings.json << 'EOF'\n" +
		`{"version":1,"repository":{` +
		`"repo_url":"https://github.com/sozercan/actions-test.git",` +
		`"branch":"main","head_sha":"","base_sha":""},` +
		`"scan":{"mode":"initial","commit_count":0,"summary":"empty"},` +
		`"findings":[]}` +
		"\nEOF"

	if _, err := EnsureRequiredSecurityArtifacts(context.Background(), cfg, transcript, nil); err != nil {
		t.Fatalf("EnsureRequiredSecurityArtifacts() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(artifactsDir, security.ArtifactFindings)); err != nil {
		t.Fatalf("artifact not recovered: %v", err)
	}
}

func cleanupSecurityArtifactsDir(t *testing.T) {
	t.Helper()
	if err := os.RemoveAll(artifactsDir); err != nil {
		t.Fatalf("RemoveAll(%q) error = %v", artifactsDir, err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(artifactsDir)
	})
}
