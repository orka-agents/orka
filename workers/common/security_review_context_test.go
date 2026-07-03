package common

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
)

func TestPrepareSecurityReviewContextWritesManifestAndAppendsPrompt(t *testing.T) {
	cleanupSecurityArtifactsDir(t)

	workspaceDir := t.TempDir()
	srcDir := filepath.Join(workspaceDir, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	appPath := filepath.Join(srcDir, "app.go")
	if err := os.WriteFile(appPath, []byte("package app\n\nfunc Handle() {}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	reviewSlice := store.ReviewSlice{
		SchemaVersion:  1,
		ID:             "slice_app",
		RepositoryScan: "repo",
		Source:         "test",
		Title:          "Go package app",
		Summary:        "App handlers",
		Kind:           "package",
		OwnedFiles:     []store.ReviewSliceFile{{Path: "app.go", Reason: "source"}},
		Confidence:     "high",
		Status:         "pending",
	}
	data, err := json.Marshal(reviewSlice)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	t.Setenv(security.EnvReviewSliceJSON, string(data))

	cfg := &AgentConfig{
		Prompt:  "Review this slice.",
		SubPath: "src",
	}
	if err := PrepareSecurityReviewContext(workspaceDir, cfg); err != nil {
		t.Fatalf("PrepareSecurityReviewContext() error = %v", err)
	}

	if !strings.Contains(cfg.Prompt, "Review this slice.") ||
		!strings.Contains(cfg.Prompt, "## Deterministic Review Context") ||
		!strings.Contains(cfg.Prompt, "app.go (owned)") ||
		!strings.Contains(cfg.Prompt, "func Handle") {
		t.Fatalf("cfg.Prompt = %q, want original prompt plus deterministic context", cfg.Prompt)
	}

	manifestPath := filepath.Join(artifactsDir(), security.ReviewContextArtifactName("slice_app"))
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("ReadFile(manifest) error = %v", err)
	}
	manifest, err := security.ParseReviewContextManifest(manifestData)
	if err != nil {
		t.Fatalf("ParseReviewContextManifest() error = %v", err)
	}
	if manifest.SliceID != "slice_app" || len(manifest.IncludedFiles) != 1 {
		t.Fatalf("manifest = %#v, want one included slice_app file", manifest)
	}
	if manifest.IncludedFiles[0].Path != "app.go" || manifest.IncludedFiles[0].Role != "owned" {
		t.Fatalf("included file = %#v, want app.go owned", manifest.IncludedFiles[0])
	}
}

func TestPrepareSecurityReviewContextNoopsWithoutSliceEnv(t *testing.T) {
	t.Setenv(security.EnvReviewSliceJSON, "")
	cfg := &AgentConfig{Prompt: "unchanged"}

	if err := PrepareSecurityReviewContext(t.TempDir(), cfg); err != nil {
		t.Fatalf("PrepareSecurityReviewContext() error = %v", err)
	}
	if cfg.Prompt != "unchanged" {
		t.Fatalf("cfg.Prompt = %q, want unchanged", cfg.Prompt)
	}
}
