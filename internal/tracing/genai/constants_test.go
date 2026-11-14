/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package genai

import "testing"

func TestProviderNamesIncludesSpecEnum(t *testing.T) {
	if len(ProviderNames) != 16 {
		t.Fatalf("len(ProviderNames) = %d, want 16", len(ProviderNames))
	}
	seen := map[string]bool{}
	for _, provider := range ProviderNames {
		if provider == "" {
			t.Fatal("empty provider enum value")
		}
		if seen[provider] {
			t.Fatalf("duplicate provider enum %q", provider)
		}
		seen[provider] = true
	}
	for _, provider := range []string{ProviderAnthropic, ProviderOpenAI, ProviderAzureOpenAI, ProviderAWSBedrock, ProviderMoonshotAI} {
		if !seen[provider] {
			t.Fatalf("missing provider enum %q", provider)
		}
	}
}

func TestNormalizeProviderName(t *testing.T) {
	tests := map[string]string{
		"openai":          ProviderOpenAI,
		"azure-openai":    ProviderAzureOpenAI,
		"azure.ai.openai": ProviderAzureOpenAI,
		"anthropic":       ProviderAnthropic,
		"bedrock":         ProviderAWSBedrock,
		"unknown-gateway": "unknown-gateway",
	}
	for input, want := range tests {
		if got := NormalizeProviderName(input); got != want {
			t.Fatalf("NormalizeProviderName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestHistogramBuckets(t *testing.T) {
	if len(OperationDurationBuckets) != 14 {
		t.Fatalf("operation duration bucket count = %d, want 14", len(OperationDurationBuckets))
	}
	if len(TokenUsageBuckets) == 0 || TokenUsageBuckets[0] != 1 {
		t.Fatalf("unexpected token usage buckets: %#v", TokenUsageBuckets)
	}
}

func TestAgentRuntimeProviderNamesTheWireAPI(t *testing.T) {
	for _, test := range []struct{ runtime, model, wantProvider, wantModel string }{
		{"claude", "claude-sonnet-4", ProviderAnthropic, "claude-sonnet-4"},
		{"codex", "gpt-5", ProviderOpenAI, "gpt-5"},
		// OpenCode's prefix selects the gateway's upstream; the wire API is
		// OpenAI chat completions and carries only the model part.
		{"opencode", "anthropic/claude-sonnet-4", ProviderOpenAI, "claude-sonnet-4"},
		{"opencode", "openai/gpt-5", ProviderOpenAI, "gpt-5"},
		{"opencode", "gpt-5", "", "gpt-5"},
	} {
		provider, model := AgentRuntimeProvider(test.runtime, test.model)
		if provider != test.wantProvider || model != test.wantModel {
			t.Errorf("AgentRuntimeProvider(%q, %q) = %q, %q; want %q, %q", test.runtime, test.model, provider, model, test.wantProvider, test.wantModel)
		}
	}
}
