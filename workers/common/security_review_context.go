package common

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sozercan/orka/internal/security"
	"github.com/sozercan/orka/internal/store"
)

// PrepareSecurityReviewContext materializes the deterministic review context
// for repository security review tasks and appends the exact bounded excerpts to
// the agent prompt.
func PrepareSecurityReviewContext(workspaceDir string, cfg *AgentConfig) error {
	if cfg == nil {
		return nil
	}
	rawSlice := strings.TrimSpace(os.Getenv(security.EnvReviewSliceJSON))
	if rawSlice == "" {
		return nil
	}

	var reviewSlice store.ReviewSlice
	if err := json.Unmarshal([]byte(rawSlice), &reviewSlice); err != nil {
		return fmt.Errorf("parse %s: %w", security.EnvReviewSliceJSON, err)
	}
	if strings.TrimSpace(reviewSlice.ID) == "" {
		return fmt.Errorf("%s missing review slice id", security.EnvReviewSliceJSON)
	}

	root := workspaceDir
	if strings.TrimSpace(cfg.SubPath) != "" {
		root = filepath.Join(workspaceDir, cfg.SubPath)
	}
	contextPrompt, manifest, err := security.BuildReviewContext(root, reviewSlice, security.ReviewContextOptions{})
	if err != nil {
		return err
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	contextArtifact := security.ReviewContextArtifactName(reviewSlice.ID)
	cfg.securityReviewContextArtifact = contextArtifact
	cfg.securityReviewContextManifest = append([]byte(nil), manifestData...)
	if err := WriteArtifactFile(contextArtifact, manifestData); err != nil {
		return err
	}

	var prompt strings.Builder
	prompt.WriteString(strings.TrimSpace(cfg.Prompt))
	prompt.WriteString("\n\n## Deterministic Review Context\n\n")
	prompt.WriteString(
		"Orka generated this bounded context from the checked-out workspace and wrote the matching " +
			"review context manifest artifact before model execution. " +
			"Cite findings only from included file ranges.\n\n",
	)
	prompt.WriteString(contextPrompt)
	cfg.Prompt = strings.TrimSpace(prompt.String())
	return nil
}

// RestoreSecurityReviewContextArtifact rewrites the worker-generated review
// context manifest in case an agent deleted or replaced it while writing
// model-authored artifacts.
func RestoreSecurityReviewContextArtifact(cfg *AgentConfig) error {
	if cfg == nil || cfg.securityReviewContextArtifact == "" || len(cfg.securityReviewContextManifest) == 0 {
		return nil
	}
	return WriteArtifactFile(cfg.securityReviewContextArtifact, cfg.securityReviewContextManifest)
}
