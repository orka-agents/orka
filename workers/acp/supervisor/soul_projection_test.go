package supervisor

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestBuiltInSoulInstructionProjection(t *testing.T) {
	prompt := "Role instructions.\nPersona: literal $(NAME), $$, {env:NAME}, {file:missing} and 語.\n"
	paths := acp.SessionPaths{Root: "/sessions/fixture", Home: "/sessions/fixture/home", Workspace: "/sessions/fixture/workspace"}
	proxy := ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/v1"}
	for _, kind := range []string{providerKindCodex, providerKindClaude, providerKindCopilot, providerKindOpencode} {
		t.Run(kind, func(t *testing.T) {
			model := "fixture-model"
			if kind == providerKindOpencode {
				model = "openai/fixture-model"
			}
			profile, err := providerProfile(kind, model, harnessv2.WorkspaceIntentWrite, testOpenCodeModelLimits())
			if err != nil {
				t.Fatal(err)
			}
			request := testProviderProjectionRequest(t, kind, model, prompt, "", nil, nil, true)
			if kind == providerKindOpencode {
				request.Profile.ModelLimits = testOpenCodeModelLimits()
			}
			projection, err := profile.ProjectSession(request, paths, proxy)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case providerKindCodex:
				found := false
				for _, value := range projection.Environment {
					var config map[string]any
					if json.Unmarshal([]byte(value), &config) == nil && config["developer_instructions"] == prompt {
						found = true
					}
				}
				if !found {
					t.Fatal("Codex developer instructions were not delivered")
				}
			case providerKindClaude:
				if projection.NewSessionMeta["systemPrompt"] != prompt {
					t.Fatal("Claude metadata lost the prompt")
				}
			default:
				if projection.Instructions == nil || projection.Instructions.Content != prompt {
					t.Fatal("native instruction projection lost literal bytes")
				}
				if kind == providerKindOpencode {
					env, err := profile.EnvironmentForSession(request, paths, proxy)
					if err != nil {
						t.Fatal(err)
					}
					var config struct {
						Instructions []string `json:"instructions"`
					}
					if err := json.Unmarshal([]byte(env["OPENCODE_CONFIG_CONTENT"]), &config); err != nil {
						t.Fatal(err)
					}
					target := filepath.Join(paths.Home, projection.Instructions.RelativePath)
					found := false
					for _, path := range config.Instructions {
						if path == target {
							found = true
						}
					}
					if !found {
						t.Fatal("OpenCode did not declare its protected instruction file")
					}
					if strings.Contains(env["OPENCODE_CONFIG_CONTENT"], prompt) {
						t.Fatal("literal instructions entered OpenCode's substituting JSON config")
					}
				}
			}
		})
	}
}
