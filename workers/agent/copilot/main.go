/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	copilot "github.com/github/copilot-sdk/go"

	"github.com/sozercan/orka/internal/security"
	"github.com/sozercan/orka/internal/workerenv"
	"github.com/sozercan/orka/workers/common"
)

const (
	defaultMaxTurns = 50
	workspaceDir    = "/workspace"
	defaultTimeout  = 20 * time.Minute

	toolNameBash       = "bash"
	toolNameCreateFile = "create_file"
	toolNameShell      = "shell"

	validationSchemaExample = `{"version":1,"finding_id":"fnd_...","status":"validated|failed|skipped",` +
		`"summary":"...","validation_steps":["..."],"reproduction":"...","attack_path_analysis":"...",` +
		`"likelihood":"...","impact":"...","assumptions":["..."],"controls":["..."],` +
		`"blindspots":["..."],"evidence":[]}`
)

var (
	toolCallPattern           = regexp.MustCompile(`(?s)<tool_call>(.*?)</tool_call>`)
	toolNamePattern           = regexp.MustCompile(`<tool_name>([^<]+)</tool_name>`)
	artifactCreateFilePattern = regexp.MustCompile(
		`(?s)<(?:path|file_path)>([^<]+)</(?:path|file_path)>` +
			`.*?<content>(.*?)</content>`,
	)
	shellCommandPattern  = regexp.MustCompile(`(?s)<command>(.*?)</command>`)
	artifactHeredocStart = regexp.MustCompile(
		`cat > ([^\n]+?\.orka-artifacts/([A-Za-z0-9._-]+))` +
			` << '?([A-Za-z0-9_]+)'?\n`,
	)
	requiredArtifactsPattern    = regexp.MustCompile(`(?m)^REQUIRED_SECURITY_ARTIFACTS:\s*(.+?)\s*$`)
	patchDiffArtifactPattern    = regexp.MustCompile(`^security-patch-([A-Za-z0-9._-]+)\.diff$`)
	patchSummaryArtifactPattern = regexp.MustCompile(`^security-patch-([A-Za-z0-9._-]+)\.json$`)
)

func main() {
	if err := common.RunAgent("copilot", workspaceDir, defaultMaxTurns, executeCopilot); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// buildSessionConfig constructs a Copilot SDK SessionConfig from the worker config.
func buildSessionConfig(cfg *common.AgentConfig) *copilot.SessionConfig {
	dir := workspaceDir
	if cfg.SubPath != "" {
		dir = filepath.Join(workspaceDir, cfg.SubPath)
	}

	sessionCfg := &copilot.SessionConfig{
		Model:            cfg.Model,
		WorkingDirectory: dir,
		// Auto-approve all permission requests for autonomous operation
		OnPermissionRequest: func(
			_ copilot.PermissionRequest, _ copilot.PermissionInvocation,
		) (copilot.PermissionRequestResult, error) {
			return copilot.PermissionRequestResult{Kind: "approved"}, nil
		},
	}

	if cfg.SystemPrompt != "" {
		sessionCfg.SystemMessage = &copilot.SystemMessageConfig{
			Mode:    "append",
			Content: cfg.SystemPrompt,
		}
	}

	if len(cfg.AllowedTools) > 0 {
		tools := make([]string, len(cfg.AllowedTools))
		for i, t := range cfg.AllowedTools {
			tools[i] = strings.TrimSpace(t)
		}
		sessionCfg.AvailableTools = tools
	}

	if len(cfg.DisallowedTools) > 0 {
		tools := make([]string, len(cfg.DisallowedTools))
		for i, t := range cfg.DisallowedTools {
			tools[i] = strings.TrimSpace(t)
		}
		sessionCfg.ExcludedTools = tools
	}

	return sessionCfg
}

// executeCopilot runs the Copilot SDK session and returns the result text.
func executeCopilot(ctx context.Context, cfg *common.AgentConfig) (string, error) {
	// Always apply a timeout so the Copilot SDK doesn't fall back to its
	// built-in 60-second default (SendAndWait adds one when the context
	// has no deadline).
	timeout := defaultTimeout
	if cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(cfg.TimeoutSeconds) * time.Second
	}
	execCtx, timeoutCancel := context.WithTimeout(ctx, timeout)
	defer timeoutCancel()

	// Create and start the Copilot client.
	// The SDK auto-discovers the bundled CLI via embeddedcli.Setup() (called
	// from the generated init() in zcopilot_<os>_<arch>.go), falling back to
	// "copilot" in PATH when not bundled (e.g. local development on macOS).
	opts := &copilot.ClientOptions{
		Cwd: workspaceDir,
	}
	if p := os.Getenv(workerenv.CopilotCLIPath); p != "" {
		opts.CLIPath = p
	}
	if token := os.Getenv(workerenv.GitHubToken); token != "" {
		opts.GithubToken = token
	}
	client := copilot.NewClient(opts)
	if err := client.Start(execCtx); err != nil {
		return "", fmt.Errorf("failed to start copilot client: %w", err)
	}
	defer func() {
		if err := client.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to stop copilot client: %v\n", err)
		}
	}()

	// Build session config and create session
	sessionCfg := buildSessionConfig(cfg)

	fmt.Printf("Creating Copilot session (model=%s, workspace=%s)\n",
		cfg.Model, sessionCfg.WorkingDirectory)

	session, err := client.CreateSession(execCtx, sessionCfg)
	if err != nil {
		return "", fmt.Errorf("failed to create session: %w", err)
	}

	// Send the prompt and wait for completion
	fmt.Printf("Sending prompt (maxTurns=%d, timeout=%s)\n", cfg.MaxTurns, timeout)

	response, err := session.SendAndWait(execCtx, copilot.MessageOptions{
		Prompt: cfg.Prompt,
	})
	if err != nil {
		return "", fmt.Errorf("send and wait failed: %w", err)
	}

	// Extract result text from the response event
	result := extractResult(response)
	if result == "" {
		fmt.Fprintf(os.Stderr, "warning: copilot session returned empty result\n")
	} else {
		fmt.Printf("Copilot session completed (result length=%d)\n", len(result))
	}
	result, err = materializeRequiredSecurityArtifacts(execCtx, session, cfg, result)
	if err != nil {
		return result, err
	}
	return result, nil
}

// extractResult extracts the text content from a session event response.
func extractResult(event *copilot.SessionEvent) string {
	if event == nil {
		return ""
	}

	// SendAndWait returns the final assistant message event. Prefer the direct
	// assistant content before falling back to lower-level result payloads.
	if event.Data.Content != nil && *event.Data.Content != "" {
		return *event.Data.Content
	}
	if event.Data.SummaryContent != nil && *event.Data.SummaryContent != "" {
		return *event.Data.SummaryContent
	}
	if event.Data.Result != nil {
		if event.Data.Result.DetailedContent != nil && *event.Data.Result.DetailedContent != "" {
			return *event.Data.Result.DetailedContent
		}
		if event.Data.Result.Content != "" {
			return event.Data.Result.Content
		}
	}

	return ""
}

func materializeRequiredSecurityArtifacts(
	ctx context.Context,
	session *copilot.Session,
	cfg *common.AgentConfig,
	result string,
) (string, error) {
	if err := common.RestoreSecurityReviewContextArtifact(cfg); err != nil {
		return result, err
	}
	required := requiredSecurityArtifacts(cfg)
	if len(required) == 0 {
		return result, nil
	}

	missing, err := common.MissingArtifacts(required)
	if err != nil {
		return result, err
	}
	if len(missing) == 0 {
		return result, nil
	}
	if recovered, recoverErr := recoverArtifactsFromDirectResult(missing, result); recoverErr != nil {
		fmt.Fprintf(
			os.Stderr,
			"warning: failed to recover artifacts from direct result: %v\n",
			recoverErr,
		)
	} else if recovered > 0 {
		fmt.Printf("Recovered %d security artifacts from direct result\n", recovered)
		if err := common.RestoreSecurityReviewContextArtifact(cfg); err != nil {
			return result, err
		}
		missing, err = common.MissingArtifacts(required)
		if err != nil {
			return result, err
		}
		if len(missing) == 0 {
			return result, nil
		}
	}
	if recovered, recoverErr := recoverArtifactsFromTranscript(result); recoverErr != nil {
		fmt.Fprintf(
			os.Stderr,
			"warning: failed to recover artifacts from transcript: %v\n",
			recoverErr,
		)
	} else if recovered > 0 {
		fmt.Printf("Recovered %d security artifacts from transcript\n", recovered)
		if err := common.RestoreSecurityReviewContextArtifact(cfg); err != nil {
			return result, err
		}
		missing, err = common.MissingArtifacts(required)
		if err != nil {
			return result, err
		}
		if len(missing) == 0 {
			return result, nil
		}
	}

	fmt.Fprintf(
		os.Stderr,
		"warning: missing required security artifacts after initial session: %s\n",
		strings.Join(missing, ", "),
	)

	response, err := session.SendAndWait(ctx, copilot.MessageOptions{
		Prompt: securityArtifactsFollowUpPrompt(cfg, missing),
	})
	if err != nil {
		return result, fmt.Errorf("artifact follow-up failed: %w", err)
	}

	if followUp := strings.TrimSpace(extractResult(response)); followUp != "" {
		var directRecovered int
		var transcriptRecovered int
		result, missing, directRecovered, transcriptRecovered, err =
			recoverArtifactsAfterFollowUp(required, missing, result, followUp, cfg)
		if err != nil {
			return result, err
		}
		if directRecovered > 0 {
			fmt.Printf("Recovered %d security artifacts from follow-up direct result\n", directRecovered)
		}
		if transcriptRecovered > 0 {
			fmt.Printf("Recovered %d security artifacts from follow-up transcript\n", transcriptRecovered)
		}
		if len(missing) == 0 {
			fmt.Printf("Required security artifacts materialized after follow-up\n")
			return result, nil
		}
	} else {
		if err := common.RestoreSecurityReviewContextArtifact(cfg); err != nil {
			return result, err
		}
		missing, err = common.MissingArtifacts(required)
		if err != nil {
			return result, err
		}
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

func recoverArtifactsAfterFollowUp(
	required []string,
	missing []string,
	result string,
	followUp string,
	cfg *common.AgentConfig,
) (string, []string, int, int, error) {
	trimmedFollowUp := strings.TrimSpace(followUp)
	if trimmedFollowUp == "" {
		if err := common.RestoreSecurityReviewContextArtifact(cfg); err != nil {
			return result, nil, 0, 0, err
		}
		updatedMissing, err := common.MissingArtifacts(required)
		return result, updatedMissing, 0, 0, err
	}

	if strings.TrimSpace(result) != "" {
		result = strings.TrimSpace(result) + "\n\n" + trimmedFollowUp
	} else {
		result = trimmedFollowUp
	}

	directRecovered, err := recoverArtifactsFromDirectResult(missing, trimmedFollowUp)
	if err != nil {
		return result, nil, 0, 0, fmt.Errorf("failed to recover artifacts from follow-up direct result: %w", err)
	}
	if err := common.RestoreSecurityReviewContextArtifact(cfg); err != nil {
		return result, nil, directRecovered, 0, err
	}

	updatedMissing, err := common.MissingArtifacts(required)
	if err != nil {
		return result, nil, directRecovered, 0, err
	}
	if len(updatedMissing) == 0 {
		return result, updatedMissing, directRecovered, 0, nil
	}

	transcriptRecovered, err := recoverArtifactsFromTranscript(trimmedFollowUp)
	if err != nil {
		return result, nil, directRecovered, 0, fmt.Errorf("failed to recover artifacts from follow-up transcript: %w", err)
	}
	if err := common.RestoreSecurityReviewContextArtifact(cfg); err != nil {
		return result, nil, directRecovered, transcriptRecovered, err
	}

	updatedMissing, err = common.MissingArtifacts(required)
	if err != nil {
		return result, nil, directRecovered, transcriptRecovered, err
	}
	return result, updatedMissing, directRecovered, transcriptRecovered, nil
}

func requiredSecurityArtifacts(cfg *common.AgentConfig) []string {
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
	return nil
}

func securityArtifactsFollowUpPrompt(cfg *common.AgentConfig, missing []string) string {
	artifactDir := security.ArtifactWorkspacePath(cfg.SubPath)

	var prompt strings.Builder
	prompt.WriteString("Before responding, finish the task by writing the missing required security artifacts.\n")
	fmt.Fprintf(&prompt, "Write them under %s/.\n", artifactDir)
	prompt.WriteString("Missing files:\n")
	for _, name := range missing {
		fmt.Fprintf(&prompt, "- %s/%s\n", artifactDir, name)
	}
	prompt.WriteString("Do not inspect more repository files unless absolutely necessary.\n")
	prompt.WriteString("Reuse the analysis already completed in this session.\n")
	prompt.WriteString("Use the Bash tool only for this step. Do not use create_file or edit.\n")
	prompt.WriteString("Write the files with shell redirection or heredocs so they are definitely persisted on disk.\n")
	for _, name := range missing {
		switch name {
		case security.ArtifactThreatModel:
			prompt.WriteString("security-threat-model.md must be non-empty markdown grounded in the repository.\n")
		case security.ArtifactFindingsV2:
			prompt.WriteString(
				"security-findings.v2.json must be valid JSON with schemaVersion=2, " +
					"repository, scan, and findings fields.\n",
			)
			prompt.WriteString(
				"Set scan.sliceId to the review slice ID and use an empty findings array " +
					"when there are no supported findings.\n",
			)
		case security.ArtifactValidation:
			prompt.WriteString("security-validation.json must be valid JSON with this shape:\n")
			prompt.WriteString(validationSchemaExample + "\n")
		}
	}
	prompt.WriteString("After the files are written, reply with only: SECURITY_ARTIFACTS_WRITTEN\n")
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
	if err := common.WriteArtifactFile(missing[0], []byte(trimmed)); err != nil {
		return 0, err
	}
	return 1, nil
}

func recoverArtifactsFromTranscript(result string) (int, error) {
	candidates := recoveredArtifactCandidates(result)
	recovered := 0
	for _, candidate := range candidates {
		if err := common.WriteArtifactFile(candidate.filename, candidate.data); err != nil {
			return recovered, err
		}
		recovered++
	}
	return recovered, nil
}

func artifactFilenameForPath(p string) (string, bool) {
	cleaned := strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if !strings.Contains(cleaned, security.ArtifactWorkspaceDir+"/") {
		return "", false
	}

	filename := path.Base(cleaned)
	if filename == "." || filename == ".." || strings.ContainsAny(filename, `/\`) {
		return "", false
	}
	return filename, true
}

type artifactCandidate struct {
	filename string
	data     []byte
	source   string
	order    int
}

type jsonToolCallArguments struct {
	Command  string `json:"command"`
	Path     string `json:"path"`
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

type jsonToolCall struct {
	Name      string                `json:"name"`
	Arguments jsonToolCallArguments `json:"arguments"`
}

func recoveredArtifactCandidates(result string) []artifactCandidate {
	selected := map[string]artifactCandidate{}
	order := 0

	for _, loc := range toolCallPattern.FindAllStringSubmatchIndex(result, -1) {
		block := result[loc[2]:loc[3]]
		nameMatch := toolNamePattern.FindStringSubmatch(block)
		if len(nameMatch) >= 2 {
			switch strings.TrimSpace(nameMatch[1]) {
			case toolNameCreateFile:
				match := artifactCreateFilePattern.FindStringSubmatch(block)
				if len(match) < 3 {
					continue
				}
				if candidate, ok := newArtifactCandidate(
					match[1],
					[]byte(match[2]),
					toolNameCreateFile,
					order,
				); ok {
					selectArtifactCandidate(selected, candidate)
					order++
				}
			case toolNameShell, toolNameBash:
				match := shellCommandPattern.FindStringSubmatch(block)
				if len(match) < 2 {
					continue
				}
				for _, candidate := range artifactCandidatesFromShellCommand(match[1], order) {
					selectArtifactCandidate(selected, candidate)
					order++
				}
			}
			continue
		}

		if call, ok := parseJSONToolCall(block); ok {
			switch strings.TrimSpace(call.Name) {
			case toolNameCreateFile:
				fullPath := strings.TrimSpace(call.Arguments.Path)
				if fullPath == "" {
					fullPath = strings.TrimSpace(call.Arguments.FilePath)
				}
				if candidate, ok := newArtifactCandidate(
					fullPath,
					[]byte(call.Arguments.Content),
					toolNameCreateFile,
					order,
				); ok {
					selectArtifactCandidate(selected, candidate)
					order++
				}
			case toolNameShell, toolNameBash:
				for _, candidate := range artifactCandidatesFromShellCommand(call.Arguments.Command, order) {
					selectArtifactCandidate(selected, candidate)
					order++
				}
			}
		}
	}

	for _, candidate := range artifactCandidatesFromShellCommand(result, order) {
		selectArtifactCandidate(selected, candidate)
		order++
	}

	candidates := make([]artifactCandidate, 0, len(selected))
	for _, candidate := range selected {
		candidates = append(candidates, candidate)
	}
	return candidates
}

func parseJSONToolCall(block string) (jsonToolCall, bool) {
	trimmed := strings.TrimSpace(block)
	if !strings.HasPrefix(trimmed, "{") {
		return jsonToolCall{}, false
	}

	var call jsonToolCall
	if err := json.Unmarshal([]byte(trimmed), &call); err != nil {
		return jsonToolCall{}, false
	}
	if strings.TrimSpace(call.Name) == "" {
		return jsonToolCall{}, false
	}
	return call, true
}

func artifactCandidatesFromShellCommand(command string, baseOrder int) []artifactCandidate {
	candidates := []artifactCandidate{}
	order := baseOrder

	for _, loc := range artifactHeredocStart.FindAllStringSubmatchIndex(command, -1) {
		fullPath := command[loc[2]:loc[3]]
		delimiter := command[loc[6]:loc[7]]
		contentStart := loc[1]
		contentEnd := strings.Index(command[contentStart:], "\n"+delimiter)
		if contentEnd < 0 {
			continue
		}

		content := command[contentStart : contentStart+contentEnd]
		if candidate, ok := newArtifactCandidate(fullPath, []byte(content), "shell", order); ok {
			candidates = append(candidates, candidate)
			order++
		}
	}

	return candidates
}

func newArtifactCandidate(artifactPath string, data []byte, source string, order int) (artifactCandidate, bool) {
	filename, ok := artifactFilenameForPath(artifactPath)
	if !ok {
		return artifactCandidate{}, false
	}
	if !validArtifactCandidate(filename, data) {
		return artifactCandidate{}, false
	}
	return artifactCandidate{filename: filename, data: data, source: source, order: order}, true
}

func validArtifactCandidate(filename string, data []byte) bool {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return false
	}

	switch filename {
	case security.ArtifactFindingsV2:
		_, err := security.ParseFindingsV2Artifact([]byte(trimmed))
		return err == nil
	case security.ArtifactValidation:
		var artifact security.ValidationArtifact
		return json.Unmarshal([]byte(trimmed), &artifact) == nil
	case security.ArtifactThreatModel:
		return trimmed != "" && !looksLikeToolTranscript(trimmed)
	default:
		if strings.HasPrefix(filename, "security-review-context-") && strings.HasSuffix(filename, ".json") {
			_, err := security.ParseReviewContextManifest([]byte(trimmed))
			return err == nil
		}
		if patchDiffArtifactPattern.MatchString(filename) {
			return strings.Contains(trimmed, "diff --git ")
		}
		if match := patchSummaryArtifactPattern.FindStringSubmatch(filename); len(match) == 2 {
			var artifact security.PatchSummaryArtifact
			if err := json.Unmarshal([]byte(trimmed), &artifact); err != nil {
				return false
			}
			return artifact.SchemaVersion == security.SchemaVersionPatchSummary &&
				strings.TrimSpace(artifact.FindingID) == match[1]
		}
		return false
	}
}

func looksLikeToolTranscript(text string) bool {
	for _, marker := range []string{
		"<tool_call>",
		"</tool_call>",
		"<tool_name>",
		"</tool_name>",
		"<parameters>",
		"</parameters>",
		"<command>",
		"</command>",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func selectArtifactCandidate(selected map[string]artifactCandidate, candidate artifactCandidate) {
	current, exists := selected[candidate.filename]
	if !exists || preferArtifactCandidate(current, candidate) {
		selected[candidate.filename] = candidate
	}
}

func preferArtifactCandidate(current, next artifactCandidate) bool {
	currentPriority := artifactCandidateSourcePriority(current.source)
	nextPriority := artifactCandidateSourcePriority(next.source)
	if nextPriority != currentPriority {
		return nextPriority > currentPriority
	}
	return next.order > current.order
}

func artifactCandidateSourcePriority(source string) int {
	switch source {
	case toolNameShell, toolNameBash:
		return 2
	case toolNameCreateFile:
		return 1
	default:
		return 0
	}
}
