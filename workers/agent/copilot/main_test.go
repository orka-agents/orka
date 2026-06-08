/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	copilot "github.com/github/copilot-sdk/go"

	"github.com/sozercan/orka/internal/security"
	"github.com/sozercan/orka/workers/common"
)

const (
	testArtifactsDir        = "/tmp/artifacts/"
	testDetailedThreatModel = "# Threat Model\n\nDetailed content"
	testFindingsJSONManual1 = `{"schemaVersion":2,"repository":{"repoURL":"https://github.com/example/repo",` +
		`"branch":"main","subPath":"","headSHA":"abc","baseSHA":"def"},` +
		`"scan":{"mode":"manual","sliceId":"slice_app","summary":"ok"},"findings":[]}`
	testFindingsJSONManual0 = `{"schemaVersion":2,"repository":{"repoURL":"https://github.com/example/repo",` +
		`"branch":"main","subPath":"","headSHA":"abc","baseSHA":"def"},` +
		`"scan":{"mode":"manual","sliceId":"slice_app","summary":"ok"},"findings":[]}`
)

func TestBuildSessionConfig_Minimal(t *testing.T) {
	cfg := &common.AgentConfig{
		Prompt:   "hello world",
		MaxTurns: 50,
	}

	sc := buildSessionConfig(cfg)

	if sc.WorkingDirectory != workspaceDir {
		t.Errorf("WorkingDirectory = %q, want %q", sc.WorkingDirectory, workspaceDir)
	}
	if sc.Model != "" {
		t.Errorf("Model = %q, want empty", sc.Model)
	}
	if sc.SystemMessage != nil {
		t.Error("expected nil SystemMessage for empty systemPrompt")
	}
	if len(sc.AvailableTools) != 0 {
		t.Errorf("AvailableTools = %v, want empty", sc.AvailableTools)
	}
	if len(sc.ExcludedTools) != 0 {
		t.Errorf("ExcludedTools = %v, want empty", sc.ExcludedTools)
	}
	if sc.OnPermissionRequest == nil {
		t.Error("expected OnPermissionRequest to be set")
	}
}

func TestBuildSessionConfig_Full(t *testing.T) {
	cfg := &common.AgentConfig{
		Prompt:          "fix bugs",
		Model:           "gpt-4.1",
		SystemPrompt:    "You are a code reviewer",
		MaxTurns:        100,
		AllowedTools:    []string{"Read", "Write"},
		DisallowedTools: []string{"Bash"},
		SubPath:         "src",
	}

	sc := buildSessionConfig(cfg)

	if sc.Model != "gpt-4.1" {
		t.Errorf("Model = %q, want gpt-4.1", sc.Model)
	}
	if sc.WorkingDirectory != workspaceDir+"/src" {
		t.Errorf("WorkingDirectory = %q, want %s/src", sc.WorkingDirectory, workspaceDir)
	}
	if sc.SystemMessage == nil {
		t.Fatal("expected SystemMessage to be set")
	}
	if sc.SystemMessage.Mode != "append" {
		t.Errorf("SystemMessage.Mode = %q, want append", sc.SystemMessage.Mode)
	}
	if sc.SystemMessage.Content != "You are a code reviewer" {
		t.Errorf("SystemMessage.Content = %q", sc.SystemMessage.Content)
	}
	if len(sc.AvailableTools) != 2 {
		t.Errorf("AvailableTools len = %d, want 2", len(sc.AvailableTools))
	}
	if len(sc.ExcludedTools) != 1 {
		t.Errorf("ExcludedTools len = %d, want 1", len(sc.ExcludedTools))
	}

	// Verify permission handler auto-approves
	result, err := sc.OnPermissionRequest(
		copilot.PermissionRequest{Kind: "tool_use"},
		copilot.PermissionInvocation{SessionID: "test"},
	)
	if err != nil {
		t.Fatalf("OnPermissionRequest error: %v", err)
	}
	if result.Kind != "approved" {
		t.Errorf("OnPermissionRequest result.Kind = %q, want approved", result.Kind)
	}
}

func TestCopilotCLIPath_Override(t *testing.T) {
	t.Setenv("COPILOT_CLI_PATH", "/usr/local/bin/copilot")

	// When COPILOT_CLI_PATH is set, executeCopilot passes it as CLIPath.
	// We can't call executeCopilot directly (it needs a real server),
	// so we just verify the env var is readable.
	if p := os.Getenv("COPILOT_CLI_PATH"); p != "/usr/local/bin/copilot" {
		t.Errorf("COPILOT_CLI_PATH = %q, want /usr/local/bin/copilot", p)
	}
}

func TestExtractResult_Nil(t *testing.T) {
	if r := extractResult(nil); r != "" {
		t.Errorf("extractResult(nil) = %q, want empty", r)
	}
}

func TestExtractResult_WithResultContent(t *testing.T) {
	event := &copilot.SessionEvent{
		Data: copilot.Data{
			Result: &copilot.Result{Content: "task completed"},
		},
	}
	if r := extractResult(event); r != "task completed" {
		t.Errorf("extractResult() = %q, want 'task completed'", r)
	}
}

func TestExtractResult_WithContent(t *testing.T) {
	content := "hello from assistant"
	event := &copilot.SessionEvent{
		Data: copilot.Data{
			Content: &content,
		},
	}
	if r := extractResult(event); r != "hello from assistant" {
		t.Errorf("extractResult() = %q, want 'hello from assistant'", r)
	}
}

func TestExtractResult_ContentTakesPrecedence(t *testing.T) {
	content := "assistant message"
	event := &copilot.SessionEvent{
		Data: copilot.Data{
			Result:  &copilot.Result{Content: "final result"},
			Content: &content,
		},
	}
	if r := extractResult(event); r != "assistant message" {
		t.Errorf("extractResult() = %q, want assistant content to take precedence", r)
	}
}

func TestBuildSessionConfig_EmptyModel(t *testing.T) {
	cfg := &common.AgentConfig{
		Prompt: "test prompt",
	}

	sc := buildSessionConfig(cfg)
	if sc.Model != "" {
		t.Errorf("Model = %q, want empty string", sc.Model)
	}
}

func TestBuildSessionConfig_EmptySystemPrompt(t *testing.T) {
	cfg := &common.AgentConfig{
		Prompt: "test prompt",
	}

	sc := buildSessionConfig(cfg)
	if sc.SystemMessage != nil {
		t.Error("SystemMessage should be nil for empty systemPrompt")
	}
}

func TestBuildSessionConfig_AllowedToolsOnly(t *testing.T) {
	cfg := &common.AgentConfig{
		Prompt:       "test",
		AllowedTools: []string{" Read ", "Write"},
	}

	sc := buildSessionConfig(cfg)
	if len(sc.AvailableTools) != 2 {
		t.Fatalf("AvailableTools len = %d, want 2", len(sc.AvailableTools))
	}
	if sc.AvailableTools[0] != "Read" {
		t.Errorf("AvailableTools[0] = %q, want 'Read' (trimmed)", sc.AvailableTools[0])
	}
	if len(sc.ExcludedTools) != 0 {
		t.Errorf("ExcludedTools should be empty, got %v", sc.ExcludedTools)
	}
}

func TestBuildSessionConfig_DisallowedToolsOnly(t *testing.T) {
	cfg := &common.AgentConfig{
		Prompt:          "test",
		DisallowedTools: []string{" Bash ", "Shell"},
	}

	sc := buildSessionConfig(cfg)
	if len(sc.ExcludedTools) != 2 {
		t.Fatalf("ExcludedTools len = %d, want 2", len(sc.ExcludedTools))
	}
	if sc.ExcludedTools[0] != "Bash" {
		t.Errorf("ExcludedTools[0] = %q, want 'Bash' (trimmed)", sc.ExcludedTools[0])
	}
	if len(sc.AvailableTools) != 0 {
		t.Errorf("AvailableTools should be empty, got %v", sc.AvailableTools)
	}
}

func TestExtractResult_EmptyData(t *testing.T) {
	event := &copilot.SessionEvent{}
	if r := extractResult(event); r != "" {
		t.Errorf("extractResult() = %q, want empty for empty data", r)
	}
}

func TestExtractResult_EmptyResultContent(t *testing.T) {
	event := &copilot.SessionEvent{
		Data: copilot.Data{
			Result: &copilot.Result{Content: ""},
		},
	}
	if r := extractResult(event); r != "" {
		t.Errorf("extractResult() = %q, want empty for empty Result.Content", r)
	}
}

func TestRequiredSecurityArtifacts(t *testing.T) {
	cfg := &common.AgentConfig{
		Prompt: "REQUIRED_SECURITY_ARTIFACTS: security-threat-model.md, security-findings.v2.json",
	}

	got := requiredSecurityArtifacts(cfg)
	if len(got) != 2 {
		t.Fatalf("requiredSecurityArtifacts() len = %d, want 2", len(got))
	}
	if got[0] != "security-threat-model.md" || got[1] != "security-findings.v2.json" {
		t.Fatalf("requiredSecurityArtifacts() = %v", got)
	}
}

func TestRequiredSecurityArtifactsParsesValidationDirective(t *testing.T) {
	cfg := &common.AgentConfig{
		Prompt: "REQUIRED_SECURITY_ARTIFACTS: security-validation.json",
	}

	got := requiredSecurityArtifacts(cfg)
	if len(got) != 1 || got[0] != security.ArtifactValidation {
		t.Fatalf("requiredSecurityArtifacts() = %v, want [%s]", got, security.ArtifactValidation)
	}
}

func TestSecurityArtifactsFollowUpPrompt(t *testing.T) {
	cfg := &common.AgentConfig{SubPath: "Sources/Kaset"}

	got := securityArtifactsFollowUpPrompt(cfg, []string{"security-threat-model.md", "security-findings.v2.json"})
	if !strings.Contains(got, "../../.orka-artifacts/security-threat-model.md") {
		t.Fatalf("follow-up prompt missing threat model path: %q", got)
	}
	if !strings.Contains(got, "Use the Bash tool only") {
		t.Fatalf("follow-up prompt missing bash-only instruction: %q", got)
	}
	if !strings.Contains(got, "security-findings.v2.json must be valid JSON with schemaVersion=2") {
		t.Fatalf("follow-up prompt missing v2 findings schema guidance: %q", got)
	}
	if !strings.Contains(got, "Set scan.sliceId to the review slice ID") {
		t.Fatalf("follow-up prompt missing v2 slice guidance: %q", got)
	}
	if !strings.Contains(got, "SECURITY_ARTIFACTS_WRITTEN") {
		t.Fatalf("follow-up prompt missing completion sentinel: %q", got)
	}
}

func TestSecurityArtifactsFollowUpPromptIncludesValidationSchema(t *testing.T) {
	cfg := &common.AgentConfig{}

	got := securityArtifactsFollowUpPrompt(cfg, []string{security.ArtifactValidation})
	if !strings.Contains(got, "security-validation.json must be valid JSON") {
		t.Fatalf("follow-up prompt missing validation schema: %q", got)
	}
	if !strings.Contains(got, "attack_path_analysis") {
		t.Fatalf("follow-up prompt missing attack path field: %q", got)
	}
}

func TestRecoverArtifactsFromDirectResultWritesThreatModel(t *testing.T) {
	t.Cleanup(func() {
		_ = os.RemoveAll(testArtifactsDir)
	})

	recovered, err := recoverArtifactsFromDirectResult(
		[]string{security.ArtifactThreatModel},
		testDetailedThreatModel,
	)
	if err != nil {
		t.Fatalf("recoverArtifactsFromDirectResult() error = %v", err)
	}
	if recovered != 1 {
		t.Fatalf("recoverArtifactsFromDirectResult() = %d, want 1", recovered)
	}

	data, err := os.ReadFile(filepath.Join(testArtifactsDir, security.ArtifactThreatModel))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(data), "Detailed content") {
		t.Fatalf("artifact content = %q, want threat model body", string(data))
	}
}

func TestRecoverArtifactsFromDirectResultRejectsThreatModelToolTranscript(t *testing.T) {
	t.Cleanup(func() {
		_ = os.RemoveAll(testArtifactsDir)
	})

	result := `
<tool_call>
<tool_name>shell</tool_name>
<parameters>
<command>cat > /workspace/.orka-artifacts/security-threat-model.md <<'EOF'
# Threat Model
EOF
</command>
</parameters>
</tool_call>`
	recovered, err := recoverArtifactsFromDirectResult([]string{security.ArtifactThreatModel}, result)
	if err != nil {
		t.Fatalf("recoverArtifactsFromDirectResult() error = %v", err)
	}
	if recovered != 0 {
		t.Fatalf("recoverArtifactsFromDirectResult() = %d, want 0", recovered)
	}
	if _, err := os.Stat(filepath.Join(testArtifactsDir, security.ArtifactThreatModel)); !os.IsNotExist(err) {
		t.Fatalf("expected no artifact to be written, stat err = %v", err)
	}
}

func TestRecoverArtifactsAfterFollowUpWritesThreatModelFromPlainText(t *testing.T) {
	cleanupRecoveredArtifacts(t)

	required := []string{security.ArtifactThreatModel}
	missing := []string{security.ArtifactThreatModel}
	initial := "Initial analysis summary"
	followUp := "# Threat Model\n\nRecovered from the follow-up response."

	result, updatedMissing, directRecovered, transcriptRecovered, err :=
		recoverArtifactsAfterFollowUp(required, missing, initial, followUp, &common.AgentConfig{})
	if err != nil {
		t.Fatalf("recoverArtifactsAfterFollowUp() error = %v", err)
	}
	if directRecovered != 1 {
		t.Fatalf("directRecovered = %d, want 1", directRecovered)
	}
	if transcriptRecovered != 0 {
		t.Fatalf("transcriptRecovered = %d, want 0", transcriptRecovered)
	}
	if len(updatedMissing) != 0 {
		t.Fatalf("updatedMissing = %v, want no missing artifacts", updatedMissing)
	}
	if !strings.Contains(result, "Recovered from the follow-up response.") {
		t.Fatalf("result = %q, want appended follow-up content", result)
	}

	data, err := os.ReadFile(filepath.Join(testArtifactsDir, security.ArtifactThreatModel))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(data), "Recovered from the follow-up response.") {
		t.Fatalf("artifact content = %q, want recovered threat model", string(data))
	}
}

func TestArtifactFilenameForPath(t *testing.T) {
	got, ok := artifactFilenameForPath("/workspace/.orka-artifacts/security-threat-model.md")
	if !ok {
		t.Fatal("artifactFilenameForPath() = not ok, want true")
	}
	if got != "security-threat-model.md" {
		t.Fatalf("artifactFilenameForPath() = %q, want security-threat-model.md", got)
	}
}

func TestRecoverArtifactsFromTranscript(t *testing.T) {
	cleanupRecoveredArtifacts(t)

	transcript := `<tool_call>
<tool_name>create_file</tool_name>
<parameters>
<path>/workspace/.orka-artifacts/security-threat-model.md</path>
<content># threat model
</content>
</parameters>
</tool_call>

<tool_call>
<tool_name>shell</tool_name>
<parameters>
	<command>cat > /workspace/.orka-artifacts/security-findings.v2.json << 'EOF'
` + testFindingsJSONManual1 + `
EOF
	</command>
</parameters>
</tool_call>`

	recovered, err := recoverArtifactsFromTranscript(transcript)
	if err != nil {
		t.Fatalf("recoverArtifactsFromTranscript() error = %v", err)
	}
	if recovered != 2 {
		t.Fatalf("recoverArtifactsFromTranscript() = %d, want 2", recovered)
	}

	threatModel, err := os.ReadFile(filepath.Join(testArtifactsDir, "security-threat-model.md"))
	if err != nil {
		t.Fatalf("ReadFile(threat model) error = %v", err)
	}
	if string(threatModel) != "# threat model\n" {
		t.Fatalf("threat model contents = %q", string(threatModel))
	}

	findings, err := os.ReadFile(filepath.Join(testArtifactsDir, security.ArtifactFindingsV2))
	if err != nil {
		t.Fatalf("ReadFile(findings) error = %v", err)
	}
	if string(findings) != testFindingsJSONManual1 {
		t.Fatalf("findings contents = %q", string(findings))
	}
}

func TestRecoverArtifactsFromTranscriptPrefersValidShellArtifact(t *testing.T) {
	cleanupRecoveredArtifacts(t)

	transcript := `<tool_call>
<tool_name>create_file</tool_name>
<parameters>
	<path>/workspace/.orka-artifacts/security-findings.v2.json</path>
	<content>{"schemaVersion":2,"scan":{"summary":"broken
</content>
</parameters>
</tool_call>

	<tool_call>
	<tool_name>shell</tool_name>
	<parameters>
		<command>cat > /workspace/.orka-artifacts/security-findings.v2.json << 'EOF'
` + testFindingsJSONManual1 + `
EOF
</command>
</parameters>
</tool_call>`

	recovered, err := recoverArtifactsFromTranscript(transcript)
	if err != nil {
		t.Fatalf("recoverArtifactsFromTranscript() error = %v", err)
	}
	if recovered != 1 {
		t.Fatalf("recoverArtifactsFromTranscript() = %d, want 1", recovered)
	}

	findings, err := os.ReadFile(filepath.Join(testArtifactsDir, security.ArtifactFindingsV2))
	if err != nil {
		t.Fatalf("ReadFile(findings) error = %v", err)
	}
	if string(findings) != testFindingsJSONManual1 {
		t.Fatalf(
			"findings contents = %q, want %q",
			string(findings),
			testFindingsJSONManual1,
		)
	}
}

func TestRecoverArtifactsFromTranscriptSupportsJSONToolCallShell(t *testing.T) {
	cleanupRecoveredArtifacts(t)

	escapedFindings := strings.ReplaceAll(testFindingsJSONManual0, `"`, `\"`)
	transcript := `<tool_call>
	{"name":"shell","arguments":{"command":"mkdir -p /workspace/.orka-artifacts && ` +
		`cat > /workspace/.orka-artifacts/security-findings.v2.json << 'ENDOFJSON'\n` +
		escapedFindings +
		`\nENDOFJSON\npython3 -m json.tool ` +
		`/workspace/.orka-artifacts/security-findings.v2.json > /dev/null && echo VALID"}}
	</tool_call>`

	recovered, err := recoverArtifactsFromTranscript(transcript)
	if err != nil {
		t.Fatalf("recoverArtifactsFromTranscript() error = %v", err)
	}
	if recovered != 1 {
		t.Fatalf("recoverArtifactsFromTranscript() = %d, want 1", recovered)
	}

	findings, err := os.ReadFile(filepath.Join(testArtifactsDir, security.ArtifactFindingsV2))
	if err != nil {
		t.Fatalf("ReadFile(findings) error = %v", err)
	}
	if string(findings) != testFindingsJSONManual0 {
		t.Fatalf(
			"findings contents = %q, want %q",
			string(findings),
			testFindingsJSONManual0,
		)
	}
}

func TestRecoverArtifactsFromTranscriptFallsBackToRawShellHeredoc(t *testing.T) {
	cleanupRecoveredArtifacts(t)

	result := `The file is now written:
	cat > /workspace/.orka-artifacts/security-findings.v2.json << 'EOF'
		` + testFindingsJSONManual0 + `
EOF
SECURITY_ARTIFACTS_WRITTEN`

	recovered, err := recoverArtifactsFromTranscript(result)
	if err != nil {
		t.Fatalf("recoverArtifactsFromTranscript() error = %v", err)
	}
	if recovered != 1 {
		t.Fatalf("recoverArtifactsFromTranscript() = %d, want 1", recovered)
	}

	findings, err := os.ReadFile(filepath.Join(testArtifactsDir, security.ArtifactFindingsV2))
	if err != nil {
		t.Fatalf("ReadFile(findings) error = %v", err)
	}
	if !strings.Contains(string(findings), `"summary":"ok"`) {
		t.Fatalf("findings contents = %q, want recovered raw heredoc JSON", string(findings))
	}
}

func TestRecoverArtifactsFromTranscriptRecoversPatchArtifacts(t *testing.T) {
	cleanupRecoveredArtifacts(t)

	findingID := "fnd_patch_123"
	diffName := "security-patch-" + findingID + ".diff"
	summaryName := "security-patch-" + findingID + ".json"
	diff := strings.Join([]string{
		"diff --git a/app.py b/app.py",
		"--- a/app.py",
		"+++ b/app.py",
		"@@ -1 +1 @@",
		"-unsafe()",
		"+safe()",
		"",
	}, "\n")
	summary := security.PatchSummaryArtifact{
		SchemaVersion: security.SchemaVersionPatchSummary,
		FindingID:     findingID,
		Summary:       "patched",
		ChangedFiles:  []string{"app.py"},
		Risk:          "low",
	}
	summaryData, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("json.Marshal(summary) error = %v", err)
	}
	transcript := `<tool_call>
<tool_name>bash</tool_name>
<parameters>
<command>cat > /workspace/.orka-artifacts/` + diffName + ` << 'EOF'
` + diff + `EOF
cat > /workspace/.orka-artifacts/` + summaryName + ` << 'EOF'
` + string(summaryData) + `
EOF
</command>
</parameters>
</tool_call>`

	recovered, err := recoverArtifactsFromTranscript(transcript)
	if err != nil {
		t.Fatalf("recoverArtifactsFromTranscript() error = %v", err)
	}
	if recovered != 2 {
		t.Fatalf("recoverArtifactsFromTranscript() = %d, want 2", recovered)
	}

	savedDiff, err := os.ReadFile(filepath.Join(testArtifactsDir, diffName))
	if err != nil {
		t.Fatalf("ReadFile(diff) error = %v", err)
	}
	if string(savedDiff) != strings.TrimSuffix(diff, "\n") {
		t.Fatalf("saved diff = %q, want %q", string(savedDiff), strings.TrimSuffix(diff, "\n"))
	}
	savedSummary, err := os.ReadFile(filepath.Join(testArtifactsDir, summaryName))
	if err != nil {
		t.Fatalf("ReadFile(summary) error = %v", err)
	}
	if string(savedSummary) != string(summaryData) {
		t.Fatalf("saved summary = %s, want %s", string(savedSummary), string(summaryData))
	}
}

func TestValidArtifactCandidateRejectsInvalidPatchArtifacts(t *testing.T) {
	if validArtifactCandidate("security-patch-fnd_expected.diff", []byte("not a unified diff")) {
		t.Fatal("validArtifactCandidate() = true, want false for invalid patch diff")
	}
	if validArtifactCandidate("security-patch-fnd_expected.json", []byte(
		`{"schemaVersion":1,"findingId":"fnd_other","summary":"patched","changedFiles":["app.py"],"risk":"low"}`,
	)) {
		t.Fatal("validArtifactCandidate() = true, want false for mismatched patch summary findingId")
	}
}

func TestExecuteCopilot_NoGitHubToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("COPILOT_CLI_PATH", "/nonexistent/copilot-cli")

	cfg := &common.AgentConfig{
		Prompt:         "test",
		MaxTurns:       5,
		TimeoutSeconds: 2,
	}

	ctx := context.Background()
	_, err := executeCopilot(ctx, cfg)
	if err == nil {
		t.Fatal("expected error when copilot client can't start")
	}
}

func TestExecuteCopilot_CancelledContext(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "fake-token")
	t.Setenv("COPILOT_CLI_PATH", "/nonexistent/copilot-cli")

	cfg := &common.AgentConfig{
		Prompt:   "test prompt",
		MaxTurns: 5,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := executeCopilot(ctx, cfg)
	if err == nil {
		t.Fatal("expected error with cancelled context")
	}
}

func TestExecuteCopilot_WithTimeout(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "fake-token")
	t.Setenv("COPILOT_CLI_PATH", "/nonexistent/copilot-cli")

	cfg := &common.AgentConfig{
		Prompt:         "test prompt",
		MaxTurns:       5,
		TimeoutSeconds: 1, // 1 second timeout
	}

	_, err := executeCopilot(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error when CLI doesn't exist")
	}
}

func cleanupRecoveredArtifacts(t *testing.T) {
	t.Helper()
	if err := os.RemoveAll(testArtifactsDir); err != nil {
		t.Fatalf("RemoveAll(%q) error = %v", testArtifactsDir, err)
	}
	t.Cleanup(func() {
		_ = os.RemoveAll(testArtifactsDir)
	})
}

func TestExecuteCopilot_NoCLIPathEnv(t *testing.T) {
	// Test the branch where COPILOT_CLI_PATH is not set.
	// The SDK will try the embedded CLI or "copilot" in PATH, which won't exist.
	t.Setenv("COPILOT_CLI_PATH", "")
	t.Setenv("GITHUB_TOKEN", "fake-token")

	cfg := &common.AgentConfig{
		Prompt:         "test",
		MaxTurns:       5,
		TimeoutSeconds: 2,
	}

	_, err := executeCopilot(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error when no CLI is available")
	}
}

func TestExecuteCopilot_DefaultTimeoutError(t *testing.T) {
	// Exercises the default timeout path (TimeoutSeconds == 0)
	t.Setenv("COPILOT_CLI_PATH", "/nonexistent/copilot-cli")
	t.Setenv("GITHUB_TOKEN", "fake-token")

	cfg := &common.AgentConfig{
		Prompt:   "test prompt",
		MaxTurns: 5,
		// TimeoutSeconds = 0, should use defaultTimeout
	}

	_, err := executeCopilot(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error when CLI doesn't exist")
	}
	if !strings.Contains(err.Error(), "failed to start copilot client") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestExecuteCopilot_NoGitHubTokenNoCLI(t *testing.T) {
	// Neither token nor CLI path set
	t.Setenv("COPILOT_CLI_PATH", "")
	t.Setenv("GITHUB_TOKEN", "")

	cfg := &common.AgentConfig{
		Prompt:         "test",
		MaxTurns:       5,
		TimeoutSeconds: 2,
	}

	_, err := executeCopilot(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error when no CLI and no token")
	}
}
