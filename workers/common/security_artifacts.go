package common

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sozercan/orka/internal/security"
)

const (
	findingsSchemaExample = `{"version":1,"repository":{"repo_url":"...","branch":"...","head_sha":"...",` +
		`"base_sha":"..."},"scan":{"mode":"initial|incremental|manual","commit_count":0,` +
		`"summary":"..."},"findings":[]}`
	validationSchemaExample = `{"version":1,"finding_id":"fnd_...","status":"validated|failed|skipped",` +
		`"summary":"...","validation_steps":["..."],"reproduction":"...","attack_path_analysis":"...",` +
		`"likelihood":"...","impact":"...","assumptions":["..."],"controls":["..."],` +
		`"blindspots":["..."],"evidence":[]}`
)

var (
	requiredArtifactsPattern = regexp.MustCompile(`(?m)^REQUIRED_SECURITY_ARTIFACTS:\s*(.+?)\s*$`)
	artifactHeredocStart     = regexp.MustCompile(
		`cat > ([^\n]+?\.orka-artifacts/([A-Za-z0-9._-]+)) << '?([A-Za-z0-9_]+)'?\n`,
	)
)

// SecurityArtifactFollowUp runs a focused follow-up prompt that should write
// any still-missing required artifacts into the shared artifact directory.
type SecurityArtifactFollowUp func(ctx context.Context, prompt string) (string, error)

// EnsureRequiredSecurityArtifacts verifies that required security artifacts
// exist after a runtime result, attempts direct/transcript recovery when
// possible, and optionally runs a follow-up prompt to materialize anything
// still missing.
func EnsureRequiredSecurityArtifacts(
	ctx context.Context,
	cfg *AgentConfig,
	result string,
	followUp SecurityArtifactFollowUp,
) (string, error) {
	required := requiredSecurityArtifacts(cfg)
	if len(required) == 0 {
		return result, nil
	}

	missing, err := MissingArtifacts(required)
	if err != nil {
		return result, err
	}
	if len(missing) == 0 {
		return result, nil
	}

	if recovered, recoverErr := recoverArtifactsFromDirectResult(missing, result); recoverErr != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to recover artifacts from direct result: %v\n", recoverErr)
	} else if recovered > 0 {
		fmt.Printf("Recovered %d security artifacts from direct result\n", recovered)
	}

	missing, err = MissingArtifacts(required)
	if err != nil {
		return result, err
	}
	if len(missing) == 0 {
		return result, nil
	}

	if recovered, recoverErr := recoverArtifactsFromTranscript(result); recoverErr != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to recover artifacts from transcript: %v\n", recoverErr)
	} else if recovered > 0 {
		fmt.Printf("Recovered %d security artifacts from transcript\n", recovered)
	}

	missing, err = MissingArtifacts(required)
	if err != nil {
		return result, err
	}
	if len(missing) == 0 {
		return result, nil
	}
	if followUp == nil {
		return result, fmt.Errorf("required security artifacts missing: %s", strings.Join(missing, ", "))
	}

	fmt.Fprintf(
		os.Stderr,
		"warning: missing required security artifacts after initial run: %s\n",
		strings.Join(missing, ", "),
	)

	followUpResult, err := followUp(ctx, securityArtifactsFollowUpPrompt(cfg, missing))
	if err != nil {
		return result, fmt.Errorf("artifact follow-up failed: %w", err)
	}
	trimmedFollowUp := strings.TrimSpace(followUpResult)
	if trimmedFollowUp != "" {
		if strings.TrimSpace(result) != "" {
			result = strings.TrimSpace(result) + "\n\n" + trimmedFollowUp
		} else {
			result = trimmedFollowUp
		}
	}

	if recovered, recoverErr := recoverArtifactsFromDirectResult(missing, trimmedFollowUp); recoverErr != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to recover artifacts from follow-up direct result: %v\n", recoverErr)
	} else if recovered > 0 {
		fmt.Printf("Recovered %d security artifacts from follow-up direct result\n", recovered)
	}

	missing, err = MissingArtifacts(required)
	if err != nil {
		return result, err
	}
	if len(missing) == 0 {
		fmt.Printf("Required security artifacts materialized after follow-up\n")
		return result, nil
	}

	if recovered, recoverErr := recoverArtifactsFromTranscript(trimmedFollowUp); recoverErr != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to recover artifacts from follow-up transcript: %v\n", recoverErr)
	} else if recovered > 0 {
		fmt.Printf("Recovered %d security artifacts from follow-up transcript\n", recovered)
	}

	missing, err = MissingArtifacts(required)
	if err != nil {
		return result, err
	}
	if len(missing) > 0 {
		return result, fmt.Errorf(
			"required security artifacts still missing after follow-up: %s",
			strings.Join(missing, ", "),
		)
	}

	fmt.Printf("Required security artifacts materialized after follow-up\n")
	return result, nil
}

func requiredSecurityArtifacts(cfg *AgentConfig) []string {
	if cfg == nil {
		return nil
	}
	match := requiredArtifactsPattern.FindStringSubmatch(cfg.Prompt)
	if len(match) >= 2 {
		raw := strings.Split(match[1], ",")
		required := make([]string, 0, len(raw))
		seen := map[string]struct{}{}
		for _, item := range raw {
			name := strings.TrimSpace(item)
			if name == "" {
				continue
			}
			if _, exists := seen[name]; exists {
				continue
			}
			seen[name] = struct{}{}
			required = append(required, name)
		}
		return required
	}
	if strings.Contains(cfg.Prompt, security.ArtifactThreatModel) &&
		strings.Contains(cfg.Prompt, security.ArtifactFindings) {
		return []string{security.ArtifactThreatModel, security.ArtifactFindings}
	}
	return nil
}

func securityArtifactsFollowUpPrompt(cfg *AgentConfig, missing []string) string {
	artifactDir := security.ArtifactWorkspacePath(cfg.SubPath)

	var prompt strings.Builder
	prompt.WriteString("Before responding, finish the task by writing the missing required security artifacts.\n")
	fmt.Fprintf(&prompt, "Write them under %s/.\n", artifactDir)
	prompt.WriteString("Missing files:\n")
	for _, name := range missing {
		fmt.Fprintf(&prompt, "- %s/%s\n", artifactDir, name)
	}
	prompt.WriteString("Do not inspect more repository files unless absolutely necessary.\n")
	prompt.WriteString("Reuse the analysis already completed in this run.\n")
	prompt.WriteString("Use shell redirection or heredocs so the files are definitely persisted on disk.\n")
	for _, name := range missing {
		switch name {
		case security.ArtifactThreatModel:
			prompt.WriteString("security-threat-model.md must be non-empty markdown grounded in the repository.\n")
		case security.ArtifactFindings:
			prompt.WriteString("security-findings.json must be valid JSON with this shape:\n")
			prompt.WriteString(findingsSchemaExample + "\n")
			prompt.WriteString(
				"Each finding object must use these keys: fingerprint, title, summary, " +
					"severity, confidence, validation_status, file_path, line, commit_sha, " +
					"root_cause, remediation, suggested_action, evidence.\n",
			)
			prompt.WriteString("If there are zero findings, write valid JSON with version=1 and an empty findings array.\n")
		case security.ArtifactValidation:
			prompt.WriteString("security-validation.json must be valid JSON with this shape:\n")
			prompt.WriteString(validationSchemaExample + "\n")
		}
	}
	prompt.WriteString("After writing the files, reply with only: SECURITY_ARTIFACTS_WRITTEN\n")
	return prompt.String()
}

func recoverArtifactsFromDirectResult(missing []string, result string) (int, error) {
	if len(missing) != 1 {
		return 0, nil
	}
	trimmed := strings.TrimSpace(result)
	if trimmed == "" || !validArtifactCandidate(missing[0], []byte(trimmed)) {
		return 0, nil
	}
	if err := WriteArtifactFile(missing[0], []byte(trimmed)); err != nil {
		return 0, err
	}
	return 1, nil
}

func recoverArtifactsFromTranscript(result string) (int, error) {
	trimmed := strings.TrimSpace(result)
	if trimmed == "" {
		return 0, nil
	}

	recovered := 0
	remaining := trimmed
	for {
		loc := artifactHeredocStart.FindStringSubmatchIndex(remaining)
		if len(loc) < 8 {
			break
		}

		fullPath := remaining[loc[2]:loc[3]]
		filename := filepath.Base(remaining[loc[4]:loc[5]])
		delimiter := remaining[loc[6]:loc[7]]
		bodyStart := loc[1]
		bodyEndMarker := "\n" + delimiter
		bodyEnd := strings.Index(remaining[bodyStart:], bodyEndMarker)
		if bodyEnd < 0 {
			remaining = remaining[loc[1]:]
			continue
		}

		body := remaining[bodyStart : bodyStart+bodyEnd]
		if strings.Contains(fullPath, security.ArtifactWorkspaceDir+"/") && validArtifactCandidate(filename, []byte(body)) {
			if err := WriteArtifactFile(filename, []byte(body)); err != nil {
				return recovered, err
			}
			recovered++
		}

		advance := bodyStart + bodyEnd + len(bodyEndMarker)
		if advance >= len(remaining) {
			break
		}
		remaining = remaining[advance:]
	}

	return recovered, nil
}

func validArtifactCandidate(filename string, data []byte) bool {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return false
	}

	switch filename {
	case security.ArtifactFindings:
		var artifact security.FindingsArtifact
		return json.Unmarshal([]byte(trimmed), &artifact) == nil
	case security.ArtifactValidation:
		var artifact security.ValidationArtifact
		return json.Unmarshal([]byte(trimmed), &artifact) == nil
	case security.ArtifactThreatModel:
		return true
	default:
		return false
	}
}
