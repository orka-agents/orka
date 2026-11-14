/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package genai

import "strings"

// NormalizeProviderName maps Orka/provider-CRD names to the GenAI provider enum.
func NormalizeProviderName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "anthropic":
		return ProviderAnthropic
	case "openai":
		return ProviderOpenAI
	case "azure-openai", "azure.ai.openai", "azure_openai", "azureopenai":
		return ProviderAzureOpenAI
	case "azure-ai-inference", "azure.ai.inference", "azure_ai_inference":
		return ProviderAzureAIInference
	case "aws-bedrock", "aws.bedrock", "bedrock":
		return ProviderAWSBedrock
	case "gcp-genai", "gcp.gen_ai", "google-genai":
		return ProviderGCPGenAI
	case "gcp-vertex-ai", "gcp.vertex_ai", "vertex-ai", "vertexai":
		return ProviderGCPVertexAI
	case "gcp-gemini", "gcp.gemini", "gemini":
		return ProviderGCPGemini
	case "cohere":
		return ProviderCohere
	case "ibm-watsonx-ai", "ibm.watsonx.ai", "watsonx":
		return ProviderIBMWatsonXAI
	case "perplexity":
		return ProviderPerplexity
	case "x-ai", "x_ai", "xai":
		return ProviderXAI
	case "deepseek":
		return ProviderDeepSeek
	case "groq":
		return ProviderGroq
	case "mistral-ai", "mistral_ai", "mistral":
		return ProviderMistralAI
	case "moonshot-ai", "moonshot_ai", "moonshot":
		return ProviderMoonshotAI
	default:
		return strings.TrimSpace(name)
	}
}

// AgentRuntimeProvider maps an Orka ACP agent runtime and its pinned profile
// model to the GenAI provider the runtime's model API speaks, and the model
// name requested from that provider. OpenCode pins "provider/model" but always
// speaks OpenAI chat completions through the proxy, which sends only the model
// part upstream: the prefix selects the gateway's upstream, not the wire API.
func AgentRuntimeProvider(runtime, model string) (provider, requestModel string) {
	model = strings.TrimSpace(model)
	switch strings.ToLower(strings.TrimSpace(runtime)) {
	case "claude":
		return ProviderAnthropic, model
	case "codex", "copilot", "agentkit":
		return ProviderOpenAI, model
	case "foundry":
		return ProviderAzureOpenAI, model
	case "opencode":
		if providerID, upstream, ok := strings.Cut(model, "/"); ok && strings.TrimSpace(providerID) != "" && strings.TrimSpace(upstream) != "" {
			return ProviderOpenAI, strings.TrimSpace(upstream)
		}
		return "", model
	default:
		return "", model
	}
}
