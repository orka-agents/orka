/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/cors"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"github.com/valyala/fasthttp"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/orka-agents/orka/internal/ahphost"
	"github.com/orka-agents/orka/internal/artifactcap"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/controller"
	"github.com/orka-agents/orka/internal/executionmode"
	gatewayruntime "github.com/orka-agents/orka/internal/gateway"
	"github.com/orka-agents/orka/internal/gateway/protocol"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/liveevents"
	"github.com/orka-agents/orka/internal/operatoraccess"
	"github.com/orka-agents/orka/internal/outboundaccess"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/taskwebhook"
)

var log = logf.Log.WithName("api-server")

type ControllerEpochFenceSource interface {
	CurrentFence(context.Context) (store.ControllerEpochFence, error)
}

type ControllerEpochReader interface {
	GetControllerEpochFence(context.Context, string) (store.ControllerEpochFence, error)
}

type ControllerEpochStoreFenceSource struct {
	epochs ControllerEpochReader
}

func NewControllerEpochStoreFenceSource(epochs ControllerEpochReader) *ControllerEpochStoreFenceSource {
	return &ControllerEpochStoreFenceSource{epochs: epochs}
}

func (s *ControllerEpochStoreFenceSource) CurrentFence(ctx context.Context) (store.ControllerEpochFence, error) {
	if s == nil || s.epochs == nil {
		return store.ControllerEpochFence{}, fmt.Errorf("controller epoch store is unavailable")
	}
	return s.epochs.GetControllerEpochFence(ctx, store.DefaultControllerEpochName)
}

type ServerConfig struct {
	Port                      int
	WatchNamespace            string
	ExecutionMode             executionmode.Mode
	EnforceNamespaceIsolation bool
	OIDC                      OIDCConfig
	ContextTokens             ContextTokenConfig
	ContextTokenAuthorization ContextTokenAuthorizationConfig
	Chat                      ChatConfig
	ResultStore               store.ResultStore
	SessionStore              store.SessionStore
	NativeSessionMaxBytes     int
	PlanStore                 store.PlanStore
	MessageStore              store.MessageStore
	ArtifactStore             store.ArtifactStore
	ArtifactReservations      artifactcap.CapabilityReservationRecorder
	AgentExecutionSnapshots   store.AgentExecutionSnapshotStore
	ExternalEffects           store.ExternalEffectIdentityReader
	MemoryStore               store.MemoryStore
	MemoryProposalStore       store.MemoryProposalStore
	MemoryConsolidationStore  store.MemoryConsolidationStore
	SecurityStore             store.SecurityStore
	RepositoryMonitorStore    store.RepositoryMonitorStore
	ExecutionEventStore       store.ExecutionEventStore
	GatewayEventStore         store.GatewayEventStore
	GatewayDeliveryStore      store.GatewayDeliveryStore
	GatewayService            *gatewayruntime.Service
	HealthChecker             store.HealthChecker
	Clientset                 kubernetes.Interface
	APIReader                 client.Reader
	TaskProvenanceProtected   bool
	ControllerEpochs          ControllerEpochFenceSource
	E2EPromptFaultEnabled     bool
	Connectors                ConnectorConfig
	ConnectorTools            ConnectorToolExecutionConfig
	GitHubSignIn              GitHubSignInConfig
	AHP                       *ahphost.Host
	AHPBackend                *AHPBackendConfig
	LiveEvents                *liveevents.Broker
	RESTConfig                *rest.Config
	OperatorAccess            OperatorAccessConfig
	SubstrateMCPProxy         SubstrateMCPProxyConfig
	DashboardEnabled          bool
	MCP                       MCPServerConfig
	AgentUseAuthorization     bool
	ProviderUseAuthorization  bool
	TaskWebhookKeys           taskwebhook.KeySource
	AgentIdentityAudit        outboundaccess.AgentIdentityAuditReader
}

type Server struct {
	ahpBackend          *ahpBackend
	app                 *fiber.App
	client              client.Client
	config              ServerConfig
	sessionManager      *controller.SessionManager
	handlers            *Handlers
	chatHandler         *ChatHandler
	openaiHandler       *OpenAICompatHandler
	anthropicHandler    *AnthropicCompatHandler
	internalHandlers    *InternalHandlers
	mcpServer           *mcpServer
	ResultStore         store.ResultStore
	SessionStore        store.SessionStore
	PlanStore           store.PlanStore
	MessageStore        store.MessageStore
	ArtifactStore       store.ArtifactStore
	MemoryStore         store.MemoryStore
	MemoryProposalStore store.MemoryProposalStore
	ExecutionEventStore store.ExecutionEventStore
	GatewayEventStore   store.GatewayEventStore

	memoryConsolidationStore store.MemoryConsolidationStore

	substrateMCPProxyWriteTimeout time.Duration
}

func NewServer(c client.Client, sessionManager *controller.SessionManager, config ServerConfig) *Server {
	config.Chat.ExecutionMode = config.ExecutionMode
	config.Chat.DashboardEnabled = config.DashboardEnabled
	agentUseAuthorization.Store(config.AgentUseAuthorization)
	providerUseAuthorization.Store(config.ProviderUseAuthorization)
	app := fiber.New(fiber.Config{
		AppName:           "Orka API",
		BodyLimit:         defaultAPIRequestBodyLimit,
		StreamRequestBody: true,
		ErrorHandler:      customErrorHandler,
	})
	redactRequestParseErrors(app)

	server := &Server{
		app:                 app,
		client:              c,
		config:              config,
		sessionManager:      sessionManager,
		ResultStore:         config.ResultStore,
		SessionStore:        config.SessionStore,
		PlanStore:           config.PlanStore,
		MessageStore:        config.MessageStore,
		ArtifactStore:       config.ArtifactStore,
		MemoryStore:         config.MemoryStore,
		MemoryProposalStore: config.MemoryProposalStore,
		ExecutionEventStore: config.ExecutionEventStore,
		GatewayEventStore:   config.GatewayEventStore,

		memoryConsolidationStore: config.MemoryConsolidationStore,
	}

	server.handlers = NewHandlers(HandlersConfig{
		Client:                    c,
		APIReader:                 config.APIReader,
		WatchNamespace:            config.WatchNamespace,
		ExecutionMode:             config.ExecutionMode,
		EnforceNamespaceIsolation: config.EnforceNamespaceIsolation,
		ContextTokenAuthorization: config.ContextTokenAuthorization,
		ResultStore:               config.ResultStore,
		SessionStore:              config.SessionStore,
		NativeSessionMaxBytes:     config.NativeSessionMaxBytes,
		SessionManager:            sessionManager,
		PlanStore:                 config.PlanStore,
		KubeClient:                config.Clientset,
		HealthChecker:             config.HealthChecker,
		ArtifactStore:             config.ArtifactStore,
		MemoryStore:               config.MemoryStore,
		MemoryProposalStore:       config.MemoryProposalStore,
		MemoryConsolidationStore:  config.MemoryConsolidationStore,
		SecurityStore:             config.SecurityStore,
		RepositoryMonitorStore:    config.RepositoryMonitorStore,
		ExecutionEventStore:       config.ExecutionEventStore,
		LiveEvents:                config.LiveEvents,
		GatewayEventStore:         config.GatewayEventStore,
		GatewayDeliveryStore:      config.GatewayDeliveryStore,
		GatewayService:            config.GatewayService,
		Connectors:                config.Connectors,
		GitHubSignIn:              config.GitHubSignIn,
		OIDCEnabled:               config.OIDC.Enabled(),
		OIDCIssuer:                config.OIDC.Issuer,
		OIDCAudience:              config.OIDC.Audience,
		OIDCScopes:                config.OIDC.Scopes,
		AHP:                       config.AHP,
		SubstrateMCPProxy:         config.SubstrateMCPProxy,
		AgentIdentityAudit:        config.AgentIdentityAudit,
	})
	if config.AHP != nil && config.AHPBackend != nil {
		backend := newAHPBackend(server.handlers, config.AHP.Store(), config.LiveEvents, *config.AHPBackend)
		backend.emitterFor = config.AHP.Emitter
		backend.recheckToken = config.AHP.Recheck
		backend.updateTerminals = config.AHP.UpdateTerminals
		backend.workspaces = &ahpWorkspaces{
			b: backend, clientset: config.Clientset, rest: config.RESTConfig,
			namespace: config.AHPBackend.WorkspaceNamespace, image: config.AHPBackend.WorkspaceImage,
		}
		if err := config.AHP.RegisterBackend(backend); err != nil {
			log.Error(err, "register the AHP session backend")
		} else {
			server.ahpBackend = backend
		}
	}
	resolver := NewProviderResolver(c, config.Chat)
	server.chatHandler = NewChatHandler(c, config.APIReader, sessionManager, config.Chat, config.WatchNamespace, config.EnforceNamespaceIsolation, config.SessionStore, config.ResultStore, resolver, config.Clientset)
	server.chatHandler.contextTokenAuthorization = config.ContextTokenAuthorization
	server.chatHandler.gatewayEventStore = config.GatewayEventStore
	server.handlers.chatTurns = server.chatHandler
	server.openaiHandler = NewOpenAICompatHandler(c, config.APIReader, config.WatchNamespace, config.EnforceNamespaceIsolation, config.Chat, resolver, config.ResultStore, config.Clientset)
	server.openaiHandler.contextTokenAuthorization = config.ContextTokenAuthorization
	server.openaiHandler.gatewayEventStore = config.GatewayEventStore
	server.substrateMCPProxyWriteTimeout = substrateMCPProxyWriteTimeout
	app.Server().HeaderReceived = server.requestConfig
	server.anthropicHandler = NewAnthropicCompatHandler(c, config.APIReader, config.WatchNamespace, config.EnforceNamespaceIsolation, config.Chat, resolver, config.ResultStore, config.Clientset)
	server.anthropicHandler.contextTokenAuthorization = config.ContextTokenAuthorization
	server.anthropicHandler.gatewayEventStore = config.GatewayEventStore
	server.setupMiddleware()
	server.setupRoutes()
	server.setupStaticFiles()
	return server
}

func requestBodyConfig(header *fasthttp.RequestHeader) fasthttp.RequestConfig {
	return requestBodyConfigWithNativeSessionLimit(header, harnessv2.DefaultMaxNativeSessionBytes)
}

func requestBodyConfigWithNativeSessionLimit(header *fasthttp.RequestHeader, nativeSessionMaxBytes int) fasthttp.RequestConfig {
	if string(header.Method()) != fiber.MethodPost {
		return fasthttp.RequestConfig{}
	}
	rawTarget := string(header.RequestURI())
	path, _, _ := strings.Cut(rawTarget, "?")
	if parsed, err := url.ParseRequestURI(rawTarget); err == nil && parsed.EscapedPath() != "" {
		path = parsed.EscapedPath()
	}
	if isGatewayIngressPath(path) {
		return fasthttp.RequestConfig{MaxRequestBodySize: protocol.MaxHTTPBodyBytes}
	}
	var nativeURI fasthttp.URI
	if nativeURI.Parse(header.Host(), header.RequestURI()) == nil && isNativeSessionImportPath(string(nativeURI.PathOriginal())) {
		return fasthttp.RequestConfig{MaxRequestBodySize: harnessv2.NativeSessionJSONLimit(nativeSessionMaxBytes), ReadTimeout: 30 * time.Second}
	}
	if strings.HasPrefix(path, "/internal/v2/acp/") {
		return fasthttp.RequestConfig{MaxRequestBodySize: 1 << 20, ReadTimeout: 30 * time.Second}
	}
	if isProviderRelayPath(path) {
		return fasthttp.RequestConfig{
			MaxRequestBodySize: providerRelayMaxRequestBytes,
			ReadTimeout:        providerRelayReadTimeout,
			WriteTimeout:       providerRelayTimeout,
		}
	}
	var mcpURI fasthttp.URI
	if mcpURI.Parse(header.Host(), header.RequestURI()) == nil && isMCPEndpointPath(string(mcpURI.PathOriginal())) {
		return fasthttp.RequestConfig{MaxRequestBodySize: mcpMaxRequestBytes, ReadTimeout: mcpRequestReadTimeout}
	}
	return fasthttp.RequestConfig{}
}

func (s *Server) requestConfig(header *fasthttp.RequestHeader) fasthttp.RequestConfig {
	if isOperatorFileUpload(header) {
		return fasthttp.RequestConfig{MaxRequestBodySize: int(operatoraccess.MaxFileBytes), ReadTimeout: 10 * time.Minute}
	}
	if isOperatorFileDownload(header) {
		return fasthttp.RequestConfig{WriteTimeout: operatorFileTransferTimeout}
	}
	config := requestBodyConfigWithNativeSessionLimit(header, s.handlers.nativeSessionMaxBytes)
	var uri fasthttp.URI
	if header.IsPost() && uri.Parse(header.Host(), header.RequestURI()) == nil &&
		bytes.EqualFold(bytes.TrimRight(uri.PathOriginal(), "/"), []byte("/openai/v1/responses")) {
		config.WriteTimeout = s.openaiHandler.config.MaxDuration + time.Second
	}
	if isSubstrateMCPProxyRoute(header) {
		config.WriteTimeout = s.substrateMCPProxyWriteTimeout
		if header.IsPost() {
			config.MaxRequestBodySize = substrateMCPProxyMaxRequestBytes
			config.ReadTimeout = substrateMCPProxyRequestReadTimeout
		}
	}
	return config
}

func isOperatorFileUpload(header *fasthttp.RequestHeader) bool {
	return string(header.Method()) == fiber.MethodPut && isOperatorFilesPath(header)
}

func isOperatorFileDownload(header *fasthttp.RequestHeader) bool {
	return string(header.Method()) == fiber.MethodGet && isOperatorFilesPath(header)
}

func isOperatorFilesPath(header *fasthttp.RequestHeader) bool {
	var uri fasthttp.URI
	if uri.Parse(header.Host(), header.RequestURI()) != nil {
		return false
	}
	parts := strings.Split(strings.Trim(string(uri.PathOriginal()), "/"), "/")
	return len(parts) == 5 && strings.EqualFold(parts[0], "api") && strings.EqualFold(parts[1], "v1") &&
		strings.EqualFold(parts[2], externalToolTaskResource) && strings.EqualFold(parts[4], "files")
}

func isGatewayIngressPath(path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	return len(parts) == 6 && strings.EqualFold(parts[0], "api") && strings.EqualFold(parts[1], "v1") &&
		strings.EqualFold(parts[2], "gateways") && strings.EqualFold(parts[5], "events")
}

func (s *Server) setupMiddleware() {
	s.app.Use(drainUnreadRequestBody)

	s.app.Use(recover.New())

	if s.config.MCP.Enabled {
		s.app.Use(adoptMCPReplayDeadline)
	}

	s.app.Use(requestid.New())

	s.app.Use(NewTracingMiddleware())

	allowedOrigins := os.Getenv("ORKA_CORS_ALLOWED_ORIGINS")
	if allowedOrigins == "" {
		allowedOrigins = "*"
	}
	origins := strings.Split(allowedOrigins, ",")
	for i, o := range origins {
		origins[i] = strings.TrimSpace(o)
	}
	allowHeaders := allowedCORSHeaders(s.config.ContextTokens)
	if s.config.MCP.Enabled {
		allowHeaders = append(allowHeaders, mcpCORSHeaders...)
	}
	var exposeHeaders []string
	if _, _, mcpOAuth := mcpOAuthMetadata(s.config.MCP, s.config.OIDC); mcpOAuth {
		exposeHeaders = []string{fiber.HeaderWWWAuthenticate}
	}
	s.app.Use(cors.New(cors.Config{
		AllowOrigins:  origins,
		AllowMethods:  []string{"GET", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"},
		AllowHeaders:  allowHeaders,
		ExposeHeaders: exposeHeaders,
	}))

	s.app.Use(NewLoggingMiddleware())

	s.app.Use(NewMetricsMiddleware())

}

func allowedCORSHeaders(contextTokens ContextTokenConfig) []string {
	headers := []string{"Origin", "Content-Type", "Accept", AuthHeader, XAPIKeyHeader}
	addHeader := func(name string) {
		if name == "" {
			return
		}
		for _, existing := range headers {
			if strings.EqualFold(existing, name) {
				return
			}
		}
		headers = append(headers, name)
	}

	for _, profile := range contextTokens.Profiles {
		for _, header := range profile.Headers {
			addHeader(header.Name)
		}
	}
	return headers
}

func (s *Server) setupRoutes() {
	s.installACPArtifactTransport()
	s.installACPArtifactAuthorizationBroker()
	s.installACPE2EPromptWriteFaultRecorder()

	s.app.Get("/healthz", s.handlers.Healthz)
	s.app.Get("/readyz", s.handlers.Readyz)

	s.app.Get(TaskWebhookJWKSPath, s.taskWebhookJWKS)

	mcpMetadata, mcpChallenge, mcpOAuth := mcpOAuthMetadata(s.config.MCP, s.config.OIDC)
	if mcpOAuth {
		s.app.Get(mcpProtectedResourceMetadataPath, serveMCPProtectedResourceMetadata(mcpMetadata))
	}
	s.app.Post("/api/v1/gateways/:namespace/:name/events", s.handlers.HandleGatewayEvent)

	s.app.Post("/webhooks/github", s.handlers.HandleGitHubWebhook)

	s.app.Get(connectors.CallbackPath, s.handlers.ConnectionCallback)

	if s.config.AHP != nil {
		s.app.Get(ahpEndpointPath, ahpEndpoint(s.config.AHP))
	}

	s.app.Get("/api/v1/auth/config", s.handlers.GetAuthConfig)
	s.app.Post("/api/v1/auth/github", s.handlers.SignInWithGitHub)

	authConfig := AuthConfig{OIDC: s.config.OIDC, ContextTokens: s.config.ContextTokens}
	if s.config.GitHubSignIn.Enabled {
		authConfig.Sessions = s.config.GitHubSignIn.authenticateSession
	}
	externalAuth := NewAuthMiddleware(s.client, authConfig)

	api := s.externalAPIGroup("/api/v1", externalAuth)

	api.Post("/tasks", s.handlers.CreateTask)
	api.Get("/tasks", s.handlers.ListTasks)
	api.Get("/tasks/:id", s.handlers.ahpTaskPrivate(s.handlers.GetTask))
	api.Delete("/tasks/:id", s.handlers.ahpTaskPrivate(s.handlers.DeleteTask))
	api.Post("/tasks/:id/cancel", s.handlers.ahpTaskPrivate(s.handlers.CancelTask))
	api.Get("/tasks/:id/logs", s.handlers.ahpTaskPrivate(s.handlers.GetTaskLogs))
	api.Get("/tasks/:id/events", s.handlers.ahpTaskPrivate(s.handlers.ListTaskEvents))
	api.Get("/tasks/:id/stream", s.handlers.ahpTaskPrivate(s.handlers.StreamTaskEvents))
	api.Get("/tasks/:id/trace", s.handlers.ahpTaskPrivate(s.handlers.GetTaskTrace))
	api.Get("/tasks/:id/approvals", s.handlers.ahpTaskPrivate(s.handlers.ListTaskApprovals))
	api.Get("/attention", s.handlers.ListAttention)
	api.Get("/approvals", s.handlers.ListPendingApprovals)
	api.Get("/audit/identity", s.handlers.ListAgentIdentityAudit)
	api.Post("/tasks/:id/approvals/:approvalID/decision", s.handlers.ahpTaskPrivate(s.handlers.DecideTaskApproval))
	api.Post("/tasks/:id/fork", s.handlers.ahpTaskPrivate(s.handlers.ForkTask))
	api.Get("/tasks/:id/result", s.handlers.ahpTaskPrivate(s.handlers.GetTaskResult))
	api.Get("/tasks/:id/memory-consolidation-report", s.handlers.ahpTaskPrivate(s.handlers.GetTaskMemoryConsolidationReport))
	api.Get("/tasks/:id/plan", s.handlers.ahpTaskPrivate(s.handlers.GetTaskPlan))
	api.Get("/tasks/:id/children", s.handlers.ahpTaskPrivate(s.handlers.GetTaskChildren))
	api.Get("/tasks/:id/artifacts", s.handlers.ahpTaskPrivate(s.handlers.ListTaskArtifacts))
	api.Get("/tasks/:id/artifacts/:filename", s.handlers.ahpTaskPrivate(s.handlers.DownloadTaskArtifact))
	api.withPreAuthorization(s.operatorRouteAudit(operatoraccess.OperationExec)).Get("/tasks/:id/exec", s.handlers.ahpTaskPrivate(s.HandleOperatorExec))
	api.withPreAuthorization(s.operatorRouteAudit(operatoraccess.OperationCopyOut)).Get("/tasks/:id/files", s.handlers.ahpTaskPrivate(s.HandleOperatorFileDownload))
	api.withPreAuthorization(s.operatorRouteAudit(operatoraccess.OperationCopyIn)).Put("/tasks/:id/files", s.handlers.ahpTaskPrivate(s.HandleOperatorFileUpload))

	api.Get("/sessions", s.handlers.ListSessions)
	api.Get("/sessions/:id", s.handlers.ahpSessionPrivate(s.handlers.GetSession))
	api.Get("/sessions/:id/native", s.handlers.ahpSessionPrivate(s.handlers.ExportNativeSession))
	api.Post("/sessions/:id/native", s.handlers.ahpSessionPrivate(s.handlers.ImportNativeSession))
	api.Get("/sessions/:id/events", s.handlers.ahpSessionPrivate(s.handlers.ListSessionEvents))
	api.Get("/sessions/:id/stream", s.handlers.ahpSessionPrivate(s.handlers.StreamSessionEvents))
	api.Delete("/sessions/:id", s.handlers.ahpSessionPrivate(s.handlers.DeleteSession))
	api.Post("/sessions/:id/cancel", s.handlers.ahpSessionPrivate(s.handlers.CancelSession))

	api.Get("/gatewayclasses", s.handlers.ListGatewayClasses)
	api.Get("/gatewayclasses/:name", s.handlers.GetGatewayClass)
	api.Get("/gateways", s.handlers.ListGateways)
	api.Get("/gateways/:name", s.handlers.GetGateway)
	api.Get("/gatewaybindings", s.handlers.ListGatewayBindings)
	api.Get("/gatewaybindings/:name", s.handlers.GetGatewayBinding)
	api.Get("/gateway-events", s.handlers.ListGatewayEvents)
	api.Get("/gateway-events/:id", s.handlers.GetGatewayEvent)
	api.Get("/gateway-deliveries", s.handlers.ListGatewayDeliveries)
	api.Get("/gateway-deliveries/:id", s.handlers.GetGatewayDelivery)
	api.Post("/gateway-deliveries/:id/retry", s.handlers.RetryGatewayDelivery)

	api.Get("/memories", s.handlers.ListMemories)
	api.Post("/memories", s.handlers.CreateMemory)
	api.Get("/memories/:id", s.handlers.GetMemory)
	api.Put("/memories/:id", s.handlers.UpdateMemory)
	api.Delete("/memories/:id", s.handlers.DeleteMemory)
	api.Post("/memories/:id/disable", s.handlers.DisableMemory)
	api.Post("/memories/:id/enable", s.handlers.EnableMemory)
	api.Get("/memory-proposals", s.handlers.ListMemoryProposals)
	api.Post("/memory-proposals", s.handlers.CreateMemoryProposal)
	api.Get("/memory-proposals/:id", s.handlers.GetMemoryProposal)
	api.Post("/memory-proposals/:id/review", s.handlers.ReviewMemoryProposal)
	api.Post("/memory-proposals/:id/apply", s.handlers.ApplyMemoryProposal)
	api.Post("/memory-proposals/:id/archive", s.handlers.ArchiveMemoryProposal)

	api.Get("/usage", s.handlers.GetUsageReport)
	api.Get("/usage/work/:id", s.handlers.GetUsageWork)
	api.Get("/usage/other/:category", s.handlers.GetUsageOther)

	api.Get("/providers", s.handlers.ListProviders)
	api.Post("/providers", s.handlers.CreateProvider)
	api.Get("/providers/:name", s.handlers.GetProvider)
	api.Put("/providers/:name", s.handlers.UpdateProvider)
	api.Delete("/providers/:name", s.handlers.DeleteProvider)

	api.Get("/tools", s.handlers.ListTools)
	api.Post("/tools", s.handlers.CreateTool)
	api.Get("/tools/:name", s.handlers.GetTool)
	api.Put("/tools/:name", s.handlers.UpdateTool)
	api.Delete("/tools/:name", s.handlers.DeleteTool)
	api.Post("/tools/:name/mcp", s.handlers.ProxySubstrateMCPTool)
	api.Delete("/tools/:name/mcp", s.handlers.ProxySubstrateMCPTool)

	api.Get("/runtime-pools", s.handlers.ListRuntimePools)
	api.Get("/runtime-pools/:name", s.handlers.GetRuntimePool)
	api.Get("/agent-runtimes", s.handlers.ListAgentRuntimes)
	api.Post("/agent-runtimes", s.handlers.CreateAgentRuntime)
	api.Get("/agent-runtimes/:name", s.handlers.GetAgentRuntime)
	api.Put("/agent-runtimes/:name", s.handlers.UpdateAgentRuntime)
	api.Delete("/agent-runtimes/:name", s.handlers.DeleteAgentRuntime)

	api.Post("/agents", s.handlers.CreateAgent)
	api.Get("/agents", s.handlers.ListAgents)
	api.Get("/agents/:name", s.handlers.GetAgent)
	api.Get("/agents/:name/system-prompt", s.handlers.GetAgentSystemPrompt)
	api.Put("/agents/:name", s.handlers.UpdateAgent)
	api.Put("/agents/:name/spec", s.handlers.UpdateAgentSpec)
	api.Delete("/agents/:name", s.handlers.DeleteAgent)

	api.Post("/skills", s.handlers.CreateSkill)
	api.Get("/skills", s.handlers.ListSkills)
	api.Get("/skills/:name", s.handlers.GetSkill)
	api.Get("/skills/:name/content", s.handlers.GetSkillContent)
	api.Put("/skills/:name", s.handlers.UpdateSkill)
	api.Delete("/skills/:name", s.handlers.DeleteSkill)

	api.Post("/security/repositories", s.handlers.CreateRepositoryScan)
	api.Get("/security/repositories", s.handlers.ListRepositoryScans)
	api.Get("/security/repositories/:name", s.handlers.GetRepositoryScan)
	api.Put("/security/repositories/:name", s.handlers.UpdateRepositoryScan)
	api.Delete("/security/repositories/:name", s.handlers.DeleteRepositoryScan)
	api.Get("/security/repositories/:name/threat-model", s.handlers.GetThreatModel)
	api.Put("/security/repositories/:name/threat-model", s.handlers.UpdateThreatModel)
	api.Get("/security/repositories/:name/scans", s.handlers.ListSecurityScanRuns)
	api.Post("/security/repositories/:name/scans", s.handlers.CreateManualSecurityScan)
	api.Get("/security/repositories/:name/scans/:scanID/progress", s.handlers.GetSecurityScanProgress)
	api.Get("/security/repositories/:name/slices", s.handlers.ListSecurityReviewSlices)
	api.Get("/security/repositories/:name/slices/:sliceID", s.handlers.GetSecurityReviewSlice)
	api.Get("/security/repositories/:name/dropped-findings", s.handlers.ListSecurityDroppedFindings)
	api.Get("/security/repositories/:name/findings", s.handlers.ListSecurityFindings)
	api.Get("/security/findings/:id", s.handlers.GetSecurityFinding)
	api.Post("/security/findings/:id/dismiss", s.handlers.DismissSecurityFinding)
	api.Post("/security/findings/:id/reopen", s.handlers.ReopenSecurityFinding)
	api.Post("/security/findings/:id/validate", s.handlers.ValidateSecurityFinding)
	api.Post("/security/findings/:id/patch", s.handlers.GenerateSecurityPatch)
	api.Get("/security/findings/:id/patches", s.handlers.ListSecurityPatchProposals)
	api.Post("/security/findings/:id/pull-request", s.handlers.CreateSecurityPullRequest)

	api.Post("/monitors/repositories", s.handlers.CreateRepositoryMonitor)
	api.Get("/monitors/repositories", s.handlers.ListRepositoryMonitors)
	api.Get("/monitors/repositories/:name", s.handlers.GetRepositoryMonitor)
	api.Put("/monitors/repositories/:name", s.handlers.UpdateRepositoryMonitor)
	api.Delete("/monitors/repositories/:name", s.handlers.DeleteRepositoryMonitor)
	api.Post("/monitors/repositories/:name/runs", s.handlers.CreateRepositoryMonitorRun)
	api.Get("/monitors/repositories/:name/runs", s.handlers.ListRepositoryMonitorRuns)
	api.Get("/monitors/repositories/:name/items", s.handlers.ListRepositoryMonitorItems)
	api.Post("/monitors/repositories/:name/commands", s.handlers.CreateRepositoryMonitorCommandEvent)
	api.Get("/monitors/commands", s.handlers.ListRepositoryMonitorCommandEvents)
	api.Get("/monitors/commands/:id", s.handlers.GetRepositoryMonitorCommandEvent)
	api.Get("/monitors/actions", s.handlers.ListRepositoryMonitorActionRecords)
	api.Get("/monitors/actions/:id", s.handlers.GetRepositoryMonitorActionRecord)
	api.Get("/monitors/work-actions", s.handlers.ListRepositoryMonitorWorkActions)
	api.Get("/monitors/work-actions/:id", s.handlers.GetRepositoryMonitorWorkAction)
	api.Get("/monitors/implementation-jobs", s.handlers.ListRepositoryMonitorImplementationJobs)
	api.Get("/monitors/implementation-jobs/:id", s.handlers.GetRepositoryMonitorImplementationJob)
	api.Get("/monitors/implementation-jobs/:id/patch-preview", s.handlers.GetRepositoryMonitorImplementationPatchPreview)
	api.Get("/monitors/mutations", s.handlers.ListRepositoryMonitorGitHubMutations)
	api.Get("/monitors/mutations/:id", s.handlers.GetRepositoryMonitorGitHubMutation)
	api.Get("/monitors/events", s.handlers.ListRepositoryMonitorEvents)

	api.Get("/substrate-actor-pools", s.handlers.ListSubstrateActorPools)
	api.Post("/substrate-actor-pools", s.handlers.CreateSubstrateActorPool)
	api.Get("/substrate-actor-pools/:name", s.handlers.GetSubstrateActorPool)
	api.Put("/substrate-actor-pools/:name", s.handlers.UpdateSubstrateActorPool)
	api.Delete("/substrate-actor-pools/:name", s.handlers.DeleteSubstrateActorPool)

	api.Get("/auth/validate", s.handleAuthValidate)
	api.Get("/auth/whoami", s.handleAuthWhoAmI)

	api.Get("/connectors", s.handlers.ListConnectors)
	api.Get("/connections", s.handlers.ListConnections)
	api.Post("/connections", s.handlers.CreateConnection)
	api.Get("/connections/:name", s.handlers.GetConnection)
	api.Put("/connections/:name", s.handlers.UpdateConnection)
	api.Delete("/connections/:name", s.handlers.DeleteConnection)
	api.Post("/connections/:name/authorize", s.handlers.AuthorizeConnection)
	api.Post("/connections/:name/complete", s.handlers.CompleteConnection)
	api.Post("/auth/session/renew", s.handlers.RenewSession)
	api.Delete("/auth/session", s.handlers.SignOut)
	api.Get("/agent-hosts/me", s.handlers.GetOwnAgentHost)
	api.Post("/agent-hosts/me/wake", s.handlers.WakeOwnAgentHost)
	api.Delete("/agent-hosts/me", s.handlers.DeleteOwnAgentHost)
	api.Post("/agent-hosts/invites", s.handlers.CreateInvite)
	api.Get("/agent-hosts/invites/:id", s.handlers.GetInvite)

	api.Post("/ahp-tokens", s.handlers.CreateAHPToken)
	api.Get("/ahp-tokens", s.handlers.ListAHPTokens)
	api.Delete("/ahp-tokens/:id", s.handlers.RevokeAHPToken)

	api.Get("/secrets", s.handlers.ListSecretNames)

	if s.config.Chat.Enabled {
		api.Post("/chat", s.chatHandler.HandleChat)
		api.Get("/chat/config", s.chatHandler.HandleChatConfig)
		api.Delete("/chat/:sessionId", s.chatHandler.HandleCancelChat)
	}

	oai := s.externalAPIGroup("/openai/v1", externalAuth)
	oai.Post("/chat/completions", s.openaiHandler.HandleChatCompletions)
	oai.Post("/responses", s.openaiHandler.HandleResponses)
	oai.Get("/models", s.openaiHandler.HandleListModels)

	anthropic := s.externalAPIGroup("/anthropic/v1", externalAuth)
	anthropic.Post("/messages", s.anthropicHandler.HandleMessages)
	anthropic.Get("/models", s.anthropicHandler.HandleListModels)

	if s.config.MCP.Enabled {
		app := s.app
		s.mcpServer = newMCPServer(func(ctx *fasthttp.RequestCtx) { app.Server().Handler(ctx) }, s.config.MCP)
		s.mcpServer.replayHeaders = mcpReplayHeaders(s.config.ContextTokens)
		mcp := s.externalAPIGroup(mcpEndpointPath, withMCPBearerChallenge(externalAuth, mcpChallenge))
		mcp.Post("", s.mcpServer.Handle)
	}

	if s.hasInternalStores() {
		s.internalHandlers = NewInternalHandlers(
			s.ResultStore,
			s.SessionStore,
			s.PlanStore,
			s.MessageStore,
			s.ArtifactStore,
			InternalHandlersConfig{
				Client:                   s.client,
				APIReader:                s.config.APIReader,
				MemoryStore:              s.MemoryStore,
				MemoryProposalStore:      s.MemoryProposalStore,
				MemoryConsolidationStore: s.memoryConsolidationStore,
				ExecutionEventStore:      s.ExecutionEventStore,
				GatewayEventStore:        s.GatewayEventStore,
				GatewayService:           s.config.GatewayService,
				TaskProvenanceProtected:  s.config.TaskProvenanceProtected,
				ConnectorTools:           s.config.ConnectorTools,
				ProviderIdentities:       s.config.Chat.ProviderIdentities,
			},
		)
		internal := s.app.Group("/internal/v1")
		internal.Get("/tasks/:namespace/:taskName/gateway-messages/origin",
			NewAuthMiddleware(s.client, AuthConfig{ReportTokenReviewUnavailable: true}),
			s.internalHandlers.GetGatewayReplyOrigin)
		internal.Use(NewAuthMiddleware(s.client))
		internal.Post("/results/:namespace/:taskName", s.internalHandlers.SubmitResult)
		internal.Post("/tasks/:namespace/:taskName/connector-tools/:tool", s.internalHandlers.ExecuteConnectorTool)
		internal.All("/tasks/:namespace/:taskName/providers/:slot/*", s.internalHandlers.RelayProviderRequest)
		internal.Post("/tasks/:namespace/:taskName/gateway-messages", s.internalHandlers.SubmitGatewayMessage)
		internal.Post("/tasks/:namespace/:taskName/children/:child/requester-stamp", s.internalHandlers.SealChildRequesterStamp)
		internal.Post("/tasks/:namespace/:taskName/children/:child/cancel", s.internalHandlers.CancelChildTask)
		internal.Get("/tasks/:namespace/:taskName/gateway-messages/budget", s.internalHandlers.GetGatewayMessageBudget)
		internal.Post("/tasks/:namespace/:taskName/execution-workspace/status", s.internalHandlers.UpdateExecutionWorkspaceStatus)
		internal.Get("/sessions/:namespace/search", s.internalHandlers.SearchTranscript)
		internal.Get("/sessions/:namespace/:name/transcript", s.internalHandlers.GetSessionTranscript)
		internal.Post("/plans/:namespace/:taskName", s.internalHandlers.SubmitPlan)
		internal.Get("/plans/:namespace/:taskName", s.internalHandlers.GetPlan)
		internal.Post("/messages/:namespace", s.internalHandlers.SendMessage)
		internal.Get("/messages/:namespace/:taskName", s.internalHandlers.GetMessages)
		internal.Post("/events/:namespace/:streamType/:streamID", s.internalHandlers.SubmitExecutionEvent)
		internal.Post("/artifacts/:namespace/:taskName/:filename", s.internalHandlers.UploadArtifact)
		internal.Get("/memories/:namespace", s.internalHandlers.ListMemories)
		internal.Post("/memories/:namespace", s.internalHandlers.CreateMemory)
		internal.Get("/memories/:namespace/:id", s.internalHandlers.GetMemory)
		internal.Put("/memories/:namespace/:id", s.internalHandlers.UpdateMemory)
		internal.Delete("/memories/:namespace/:id", s.internalHandlers.DeleteMemory)
		internal.Post("/memories/:namespace/:id/disable", s.internalHandlers.DisableMemory)
		internal.Post("/memories/:namespace/:id/enable", s.internalHandlers.EnableMemory)
		internal.Get("/memory-proposals/:namespace", s.internalHandlers.ListMemoryProposals)
		internal.Post("/memory-proposals/:namespace", s.internalHandlers.CreateMemoryProposal)
		internal.Get("/memory-proposals/:namespace/:id", s.internalHandlers.GetMemoryProposal)
		internal.Post("/memory-proposals/:namespace/:id/review", s.internalHandlers.ReviewMemoryProposal)
		internal.Post("/memory-proposals/:namespace/:id/apply", s.internalHandlers.ApplyMemoryProposal)
		internal.Post("/memory-proposals/:namespace/:id/archive", s.internalHandlers.ArchiveMemoryProposal)
		internal.Post("/memory-consolidation/:namespace/begin", s.internalHandlers.BeginMemoryConsolidation)
		internal.Post("/memory-consolidation/:namespace/complete", s.internalHandlers.CompleteMemoryConsolidation)
	}
}

func (s *Server) hasInternalStores() bool {
	return s.ResultStore != nil ||
		s.SessionStore != nil ||
		s.PlanStore != nil ||
		s.MessageStore != nil ||
		s.ArtifactStore != nil ||
		s.MemoryStore != nil ||
		s.MemoryProposalStore != nil ||
		s.ExecutionEventStore != nil ||
		s.config.GatewayService != nil
}

func (s *Server) Start(ctx context.Context) error {
	addr := fmt.Sprintf(":%d", s.config.Port)
	log.Info("starting API server", "address", addr)

	if s.ahpBackend != nil {
		go s.ahpBackend.workspaces.collectIdle(ctx)
	}
	errCh := make(chan error, 1)
	go func() {
		if err := s.app.Listen(addr); err != nil {
			errCh <- err
		}
	}()
	select {
	case <-ctx.Done():
		log.Info("shutting down API server")
		return s.app.Shutdown()
	case err := <-errCh:
		return err
	}
}

var apiPathRoots = []string{"/api", "/openai", "/anthropic", "/internal", "/webhooks", "/.well-known", mcpEndpointPath}

func routeLookupPath(path string) string {
	folded := strings.ToLower(path)
	for len(folded) > 1 && strings.HasSuffix(folded, "/") {
		folded = folded[:len(folded)-1]
	}
	return folded
}

const staticAssetRoot = "/assets"

func spaFallbackEligible(path string) bool {
	folded := routeLookupPath(path)
	underRoot := func(root string) bool { return folded == root || strings.HasPrefix(folded, root+"/") }
	if slices.ContainsFunc(apiPathRoots, underRoot) || underRoot(staticAssetRoot) {
		return false
	}
	return folded != "/healthz" && folded != "/readyz"
}

func customErrorHandler(c fiber.Ctx, err error) error {
	code := fiber.StatusInternalServerError
	message := "Internal Server Error"

	if e, ok := err.(*fiber.Error); ok {
		code = e.Code
		message = e.Message
	} else if temporary := temporaryStorageHTTPError(err); temporary != nil {
		code = temporary.Code
		message = temporary.Message
	}
	if code == fiber.StatusServiceUnavailable && len(c.Response().Header.Peek(fiber.HeaderRetryAfter)) == 0 {
		c.Set(fiber.HeaderRetryAfter, serviceUnavailableRetryAfterSeconds)
	}
	c.Set(fiber.HeaderXContentTypeOptions, "nosniff")

	if code == fiber.StatusNotFound && dashboardServed(c) && spaFallbackEligible(c.Path()) {
		if page, ok := embeddedSPAIndexPage(); ok {
			return page.send(c)
		}
	}

	if code == fiber.StatusNotFound {
		if handled, resp := compatRouteNotFound(c); handled {
			return resp
		}
	}

	return c.Status(code).JSON(fiber.Map{
		apiFieldError: fiber.Map{
			apiFieldCode:    code,
			apiFieldMessage: message,
		},
	})
}

var unsupportedCompatRoutes = map[string]string{
	"/openai/v1/conversations": "saved conversations are not supported; use /openai/v1/responses with store:false and client-owned history",
}

func compatRouteNotFound(c fiber.Ctx) (bool, error) {
	path := routeLookupPath(c.Path())
	status := fiber.StatusNotFound

	message, unsupported := unsupportedCompatRoutes[path]
	if unsupported {
		status = fiber.StatusNotImplemented
		c.Set("X-Should-Retry", "false")
	} else {
		message = fmt.Sprintf("unknown path %s", path)
	}

	switch {
	case path == "/openai" || strings.HasPrefix(path, "/openai/"):
		return true, c.Status(status).JSON(OAIError{Error: OAIErrorDetail{
			Message: message,
			Type:    OAIErrorTypeInvalidRequest,
		}})
	case path == "/anthropic" || strings.HasPrefix(path, "/anthropic/"):
		return true, anthropicError(c, status, "not_found_error", message)
	}
	return false, nil
}
