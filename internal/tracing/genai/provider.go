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
