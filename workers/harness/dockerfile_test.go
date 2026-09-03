package harness

import (
	"os"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/acp"
)

func TestDockerfilePinsAgentCLIs(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	for _, required := range []string{
		"@openai/codex@" + acp.CodexCLIVersion,
		"@anthropic-ai/claude-code@" + acp.ClaudeCodeVersion,
		"opencode-ai@1.18.2",
		"command -v codex",
		"command -v claude",
		"command -v opencode",
	} {
		if !strings.Contains(content, required) {
			t.Errorf("Dockerfile is missing %q", required)
		}
	}
	for _, unpinned := range []string{
		"@openai/codex ",
		"@anthropic-ai/claude-code ",
	} {
		if strings.Contains(content, unpinned) {
			t.Errorf("Dockerfile contains unpinned CLI package %q", strings.TrimSpace(unpinned))
		}
	}
}
