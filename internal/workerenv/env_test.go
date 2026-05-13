/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package workerenv

import "testing"

func TestAIWorkerEnvRoundTrip(t *testing.T) {
	env := AIWorkerEnv{
		BaseEnv: BaseEnv{
			TaskName:           "task-1",
			TaskNamespace:      "default",
			ResultEndpoint:     "http://controller/results/default/task-1",
			ControllerURL:      "http://controller",
			TransactionID:      "txn-123",
			TransactionProfile: "kontxt",
		},
		Provider:        "openai",
		Model:           "gpt-5",
		Prompt:          "do work",
		SystemPrompt:    "be concise",
		BaseURL:         "https://example.test/v1",
		AzureAPIVersion: "2024-10-21",
		Tools:           []string{"delegate_task", "wait_for_tasks"},
		Fallbacks: []FallbackProviderEnv{{
			Provider:        "anthropic",
			APIKey:          "secret",
			Model:           "claude",
			BaseURL:         "https://anthropic.example",
			AzureAPIVersion: "ignored",
		}},
	}

	values := map[string]string{}
	for _, envVar := range env.EnvVars() {
		values[envVar.Name] = envVar.Value
	}

	parsed := ParseAIWorkerEnv(func(name string) string { return values[name] })
	if err := parsed.ValidateRequired(); err != nil {
		t.Fatalf("ValidateRequired() returned error: %v", err)
	}
	if parsed.BaseEnv != env.BaseEnv {
		t.Fatalf("base env mismatch: got %#v, want %#v", parsed.BaseEnv, env.BaseEnv)
	}
	if parsed.Provider != env.Provider || parsed.Model != env.Model || parsed.Prompt != env.Prompt {
		t.Fatalf("AI env mismatch: got %#v, want %#v", parsed, env)
	}
	if len(parsed.Tools) != 2 || parsed.Tools[0] != "delegate_task" || parsed.Tools[1] != "wait_for_tasks" {
		t.Fatalf("tools = %#v", parsed.Tools)
	}
	if len(parsed.Fallbacks) != 1 {
		t.Fatalf("fallback count = %d, want 1", len(parsed.Fallbacks))
	}
	if parsed.Fallbacks[0] != env.Fallbacks[0] {
		t.Fatalf("fallback = %#v, want %#v", parsed.Fallbacks[0], env.Fallbacks[0])
	}
}

func TestParseFallbacksInvalidCountPreservesLegacyBehavior(t *testing.T) {
	values := map[string]string{AIFallbackCount: "not-an-int"}
	fallbacks := ParseFallbacks(func(name string) string { return values[name] })
	if len(fallbacks) != 0 {
		t.Fatalf("fallbacks = %#v, want none", fallbacks)
	}
}

func TestAIWorkerEnvValidateRequired(t *testing.T) {
	for _, tt := range []struct {
		name string
		env  AIWorkerEnv
		want string
	}{
		{name: "missing provider", env: AIWorkerEnv{Model: "m", Prompt: "p"}, want: AIProvider},
		{name: "missing model", env: AIWorkerEnv{Provider: "p", Prompt: "prompt"}, want: AIModel},
		{name: "missing prompt", env: AIWorkerEnv{Provider: "p", Model: "m"}, want: AIPrompt},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.env.ValidateRequired()
			if err == nil {
				t.Fatal("expected error")
			}
			if err.Error() != tt.want+" is required" {
				t.Fatalf("error = %q, want %q", err.Error(), tt.want+" is required")
			}
		})
	}
}

func TestCoordinationEnvRenderAndParse(t *testing.T) {
	env := CoordinationEnv{
		Enabled:                 true,
		MaxDepth:                3,
		MaxChildren:             4,
		AllowedAgents:           []string{"builder", "reviewer"},
		Depth:                   "1",
		AutonomousMode:          true,
		AutonomousIteration:     2,
		AutonomousMaxIterations: 8,
	}
	values := map[string]string{}
	for _, envVar := range env.EnvVars() {
		values[envVar.Name] = envVar.Value
	}

	parsed := ParseCoordinationEnv(func(name string) string { return values[name] })
	if !parsed.Enabled || !parsed.AutonomousMode {
		t.Fatalf("parsed flags = %#v", parsed)
	}
	if parsed.MaxDepth != 3 || parsed.MaxChildren != 4 || parsed.Depth != "1" || parsed.AutonomousIteration != 2 || parsed.AutonomousMaxIterations != 8 {
		t.Fatalf("parsed numeric/depth values = %#v", parsed)
	}
	if len(parsed.AllowedAgents) != 2 || parsed.AllowedAgents[0] != "builder" || parsed.AllowedAgents[1] != "reviewer" {
		t.Fatalf("allowed agents = %#v", parsed.AllowedAgents)
	}
}

func TestTransactionLogFields(t *testing.T) {
	if got := TransactionLogFields("", ""); got != "" {
		t.Fatalf("TransactionLogFields empty = %q, want empty", got)
	}
	got := TransactionLogFields("txn-123", "kontxt")
	want := " transactionID=txn-123 contextTokenProfile=kontxt"
	if got != want {
		t.Fatalf("TransactionLogFields() = %q, want %q", got, want)
	}
}
