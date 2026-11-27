/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package genai

const (
	SchemaURL = "gen-ai-dev/1.42.0-dev"

	InstrumentationName = "orka.gen_ai"
)

const (
	OperationChat        = "chat"
	OperationExecuteTool = "execute_tool"
	OperationInvokeAgent = "invoke_agent"
	TokenTypeInput       = "input"
	TokenTypeOutput      = "output"
	ToolTypeFunction     = "function"
	ToolTypeExtension    = "extension"
	ToolTypeDatastore    = "datastore"

	ErrorTypeOther = "_OTHER"
)

const (
	AttrOperationName               = "gen_ai.operation.name"
	AttrProviderName                = "gen_ai.provider.name"
	AttrRequestModel                = "gen_ai.request.model"
	AttrRequestMaxTokens            = "gen_ai.request.max_tokens"
	AttrRequestTemperature          = "gen_ai.request.temperature"
	AttrRequestStopSequences        = "gen_ai.request.stop_sequences"
	AttrRequestStream               = "gen_ai.request.stream"
	AttrOutputType                  = "gen_ai.output.type"
	AttrUsageInputTokens            = "gen_ai.usage.input_tokens"
	AttrUsageOutputTokens           = "gen_ai.usage.output_tokens"
	AttrUsageCacheReadInputTokens   = "gen_ai.usage.cache_read.input_tokens"
	AttrUsageCacheCreateInputTokens = "gen_ai.usage.cache_creation.input_tokens"
	AttrResponseID                  = "gen_ai.response.id"
	AttrResponseModel               = "gen_ai.response.model"
	AttrResponseFinishReasons       = "gen_ai.response.finish_reasons"
	AttrResponseTimeToFirstChunk    = "gen_ai.response.time_to_first_chunk"
	AttrConversationID              = "gen_ai.conversation.id"
	AttrInputMessages               = "gen_ai.input.messages"
	AttrOutputMessages              = "gen_ai.output.messages"
	AttrSystemInstructions          = "gen_ai.system_instructions"
	AttrToolDefinitions             = "gen_ai.tool.definitions"
	AttrTokenType                   = "gen_ai.token.type"
	AttrToolName                    = "gen_ai.tool.name"
	AttrToolCallID                  = "gen_ai.tool.call.id"
	AttrToolDescription             = "gen_ai.tool.description"
	AttrToolType                    = "gen_ai.tool.type"
	AttrToolCallArguments           = "gen_ai.tool.call.arguments"
	AttrToolCallResult              = "gen_ai.tool.call.result"
	AttrAgentID                     = "gen_ai.agent.id"
	AttrAgentName                   = "gen_ai.agent.name"
	AttrErrorType                   = "error.type"

	AttrOpenAIRequestServiceTier  = "openai.request.service_tier"
	AttrOpenAIResponseServiceTier = "openai.response.service_tier"
	AttrOpenAIResponseFingerprint = "openai.response.system_fingerprint"
	AttrOpenAIAPIType             = "openai.api.type"
)

const (
	MetricClientOperationDuration  = "gen_ai.client.operation.duration"
	MetricClientTokenUsage         = "gen_ai.client.token.usage"
	MetricClientTimeToFirstChunk   = "gen_ai.client.operation.time_to_first_chunk"
	MetricExecuteToolDuration      = "gen_ai.execute_tool.duration"
	MetricInvokeAgentDuration      = "gen_ai.invoke_agent.duration"
	EventInferenceOperationDetails = "gen_ai.client.inference.operation.details"
	UnitSeconds                    = "s"
	UnitTokens                     = "{token}"
)

const (
	ProviderAnthropic        = "anthropic"
	ProviderOpenAI           = "openai"
	ProviderAzureOpenAI      = "azure.ai.openai"
	ProviderAzureAIInference = "azure.ai.inference"
	ProviderAWSBedrock       = "aws.bedrock"
	ProviderGCPGenAI         = "gcp.gen_ai"
	ProviderGCPVertexAI      = "gcp.vertex_ai"
	ProviderGCPGemini        = "gcp.gemini"
	ProviderCohere           = "cohere"
	ProviderIBMWatsonXAI     = "ibm.watsonx.ai"
	ProviderPerplexity       = "perplexity"
	ProviderXAI              = "x_ai"
	ProviderDeepSeek         = "deepseek"
	ProviderGroq             = "groq"
	ProviderMistralAI        = "mistral_ai"
	ProviderMoonshotAI       = "moonshot_ai"
)

var ProviderNames = []string{
	ProviderAnthropic,
	ProviderOpenAI,
	ProviderAzureOpenAI,
	ProviderAzureAIInference,
	ProviderAWSBedrock,
	ProviderGCPGenAI,
	ProviderGCPVertexAI,
	ProviderGCPGemini,
	ProviderCohere,
	ProviderIBMWatsonXAI,
	ProviderPerplexity,
	ProviderXAI,
	ProviderDeepSeek,
	ProviderGroq,
	ProviderMistralAI,
	ProviderMoonshotAI,
}

var OperationDurationBuckets = []float64{
	0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64,
	1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92,
}

var TokenUsageBuckets = []float64{1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144}

var ToolDurationBuckets = []float64{0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92}

var InvokeAgentDurationBuckets = []float64{1, 2, 5, 10, 30, 60, 120, 300, 600, 1200, 1800, 3600, 7200, 14400}
