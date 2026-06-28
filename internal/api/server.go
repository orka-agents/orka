/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/cors"
	"github.com/gofiber/fiber/v3/middleware/recover"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/sozercan/orka/internal/controller"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/uiembed"
)

var log = logf.Log.WithName("api-server")

// ServerConfig holds configuration for the API server
type ServerConfig struct {
	Port                      int
	MetricsPort               int
	WatchNamespace            string
	EnforceNamespaceIsolation bool
	OIDC                      OIDCConfig
	ContextTokens             ContextTokenConfig
	ContextTokenAuthorization ContextTokenAuthorizationConfig
	Chat                      ChatConfig
	ResultStore               store.ResultStore
	SessionStore              store.SessionStore
	PlanStore                 store.PlanStore
	MessageStore              store.MessageStore
	ArtifactStore             store.ArtifactStore
	MemoryStore               store.MemoryStore
	MemoryProposalStore       store.MemoryProposalStore
	SecurityStore             store.SecurityStore
	RepositoryMonitorStore    store.RepositoryMonitorStore
	ExecutionEventStore       store.ExecutionEventStore
	HealthChecker             store.HealthChecker
	Clientset                 kubernetes.Interface
}

// Server is the REST API server
type Server struct {
	app                    *fiber.App
	client                 client.Client
	config                 ServerConfig
	sessionManager         *controller.SessionManager
	handlers               *Handlers
	chatHandler            *ChatHandler
	openaiHandler          *OpenAICompatHandler
	anthropicHandler       *AnthropicCompatHandler
	internalHandlers       *InternalHandlers
	ResultStore            store.ResultStore
	SessionStore           store.SessionStore
	PlanStore              store.PlanStore
	MessageStore           store.MessageStore
	ArtifactStore          store.ArtifactStore
	MemoryStore            store.MemoryStore
	MemoryProposalStore    store.MemoryProposalStore
	SecurityStore          store.SecurityStore
	RepositoryMonitorStore store.RepositoryMonitorStore
	ExecutionEventStore    store.ExecutionEventStore
}

// NewServer creates a new API server
func NewServer(c client.Client, sessionManager *controller.SessionManager, config ServerConfig) *Server {
	app := fiber.New(fiber.Config{
		AppName:      "Orka API",
		BodyLimit:    15 << 20, // 15MB — allows artifact uploads up to 10MB + overhead
		ErrorHandler: customErrorHandler,
	})

	server := &Server{
		app:                    app,
		client:                 c,
		config:                 config,
		sessionManager:         sessionManager,
		ResultStore:            config.ResultStore,
		SessionStore:           config.SessionStore,
		PlanStore:              config.PlanStore,
		MessageStore:           config.MessageStore,
		ArtifactStore:          config.ArtifactStore,
		MemoryStore:            config.MemoryStore,
		MemoryProposalStore:    config.MemoryProposalStore,
		SecurityStore:          config.SecurityStore,
		RepositoryMonitorStore: config.RepositoryMonitorStore,
		ExecutionEventStore:    config.ExecutionEventStore,
	}

	server.handlers = NewHandlers(HandlersConfig{
		Client:                    c,
		WatchNamespace:            config.WatchNamespace,
		EnforceNamespaceIsolation: config.EnforceNamespaceIsolation,
		ContextTokenAuthorization: config.ContextTokenAuthorization,
		ResultStore:               config.ResultStore,
		SessionStore:              config.SessionStore,
		PlanStore:                 config.PlanStore,
		KubeClient:                config.Clientset,
		HealthChecker:             config.HealthChecker,
		ArtifactStore:             config.ArtifactStore,
		MemoryStore:               config.MemoryStore,
		MemoryProposalStore:       config.MemoryProposalStore,
		SecurityStore:             config.SecurityStore,
		RepositoryMonitorStore:    config.RepositoryMonitorStore,
		ExecutionEventStore:       config.ExecutionEventStore,
	})
	resolver := NewProviderResolver(c, config.Chat)
	server.chatHandler = NewChatHandler(c, sessionManager, config.Chat, config.WatchNamespace, config.EnforceNamespaceIsolation, config.SessionStore, config.ResultStore, resolver, config.Clientset)
	server.chatHandler.contextTokenAuthorization = config.ContextTokenAuthorization
	server.openaiHandler = NewOpenAICompatHandler(c, config.WatchNamespace, config.EnforceNamespaceIsolation, config.Chat, resolver, config.ResultStore, config.Clientset)
	server.openaiHandler.contextTokenAuthorization = config.ContextTokenAuthorization
	server.anthropicHandler = NewAnthropicCompatHandler(c, config.WatchNamespace, config.EnforceNamespaceIsolation, config.Chat, resolver, config.ResultStore, config.Clientset)
	server.anthropicHandler.contextTokenAuthorization = config.ContextTokenAuthorization
	server.setupMiddleware()
	server.setupRoutes()
	server.setupStaticFiles()

	return server
}

// setupMiddleware configures middleware for the server
func (s *Server) setupMiddleware() {
	// Recovery middleware
	s.app.Use(recover.New())

	// Request ID middleware
	s.app.Use(requestid.New())

	// Tracing middleware (after request ID so spans include it)
	s.app.Use(NewTracingMiddleware())

	// CORS middleware
	allowedOrigins := os.Getenv("ORKA_CORS_ALLOWED_ORIGINS")
	if allowedOrigins == "" {
		allowedOrigins = "*"
	}
	origins := strings.Split(allowedOrigins, ",")
	for i, o := range origins {
		origins[i] = strings.TrimSpace(o)
	}
	s.app.Use(cors.New(cors.Config{
		AllowOrigins: origins,
		AllowMethods: []string{"GET", "POST", "PUT", "DELETE", "PATCH", "OPTIONS"},
		AllowHeaders: allowedCORSHeaders(s.config.ContextTokens),
	}))

	// Logging middleware
	s.app.Use(NewLoggingMiddleware())

	// Metrics middleware
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

// setupRoutes configures the API routes
func (s *Server) setupRoutes() {
	// Health endpoints
	s.app.Get("/healthz", s.handlers.Healthz)
	s.app.Get("/readyz", s.handlers.Readyz)

	// GitHub webhooks use HMAC verification instead of Kubernetes/OIDC bearer auth.
	s.app.Post("/webhooks/github", s.handlers.HandleGitHubWebhook)

	externalAuth := NewAuthMiddleware(s.client, AuthConfig{OIDC: s.config.OIDC, ContextTokens: s.config.ContextTokens})

	// API v1 group
	api := s.app.Group("/api/v1")

	// Auth middleware for API endpoints
	api.Use(externalAuth)

	// Task endpoints
	api.Post("/tasks", s.handlers.CreateTask)
	api.Get("/tasks", s.handlers.ListTasks)
	api.Get("/tasks/:id", s.handlers.GetTask)
	api.Delete("/tasks/:id", s.handlers.DeleteTask)
	api.Get("/tasks/:id/logs", s.handlers.GetTaskLogs)
	api.Get("/tasks/:id/events", s.handlers.ListTaskEvents)
	api.Get("/tasks/:id/stream", s.handlers.StreamTaskEvents)
	api.Get("/tasks/:id/trace", s.handlers.GetTaskTrace)
	api.Get("/tasks/:id/approvals", s.handlers.ListTaskApprovals)
	api.Post("/tasks/:id/approvals/:approvalID/decision", s.handlers.DecideTaskApproval)
	api.Post("/tasks/:id/fork", s.handlers.ForkTask)
	api.Get("/tasks/:id/result", s.handlers.GetTaskResult)
	api.Get("/tasks/:id/plan", s.handlers.GetTaskPlan)
	api.Get("/tasks/:id/children", s.handlers.GetTaskChildren)
	api.Get("/tasks/:id/artifacts", s.handlers.ListTaskArtifacts)
	api.Get("/tasks/:id/artifacts/:filename", s.handlers.DownloadTaskArtifact)

	// Session endpoints
	api.Get("/sessions", s.handlers.ListSessions)
	api.Get("/sessions/:id", s.handlers.GetSession)
	api.Get("/sessions/:id/events", s.handlers.ListSessionEvents)
	api.Get("/sessions/:id/stream", s.handlers.StreamSessionEvents)
	api.Delete("/sessions/:id", s.handlers.DeleteSession)

	// Memory endpoints
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

	// Provider endpoints
	api.Get("/providers", s.handlers.ListProviders)
	api.Post("/providers", s.handlers.CreateProvider)
	api.Get("/providers/:name", s.handlers.GetProvider)
	api.Put("/providers/:name", s.handlers.UpdateProvider)
	api.Delete("/providers/:name", s.handlers.DeleteProvider)

	// Tool endpoints
	api.Get("/tools", s.handlers.ListTools)
	api.Post("/tools", s.handlers.CreateTool)
	api.Get("/tools/:name", s.handlers.GetTool)
	api.Put("/tools/:name", s.handlers.UpdateTool)
	api.Delete("/tools/:name", s.handlers.DeleteTool)

	// Agent endpoints
	api.Post("/agents", s.handlers.CreateAgent)
	api.Get("/agents", s.handlers.ListAgents)
	api.Get("/agents/:name", s.handlers.GetAgent)
	api.Put("/agents/:name", s.handlers.UpdateAgent)
	api.Delete("/agents/:name", s.handlers.DeleteAgent)

	// Skills endpoints
	api.Post("/skills", s.handlers.CreateSkill)
	api.Get("/skills", s.handlers.ListSkills)
	api.Get("/skills/:name", s.handlers.GetSkill)
	api.Get("/skills/:name/content", s.handlers.GetSkillContent)
	api.Put("/skills/:name", s.handlers.UpdateSkill)
	api.Delete("/skills/:name", s.handlers.DeleteSkill)

	// Security endpoints
	api.Post("/security/repositories", s.handlers.CreateRepositoryScan)
	api.Get("/security/repositories", s.handlers.ListRepositoryScans)
	api.Get("/security/repositories/:name", s.handlers.GetRepositoryScan)
	api.Put("/security/repositories/:name", s.handlers.UpdateRepositoryScan)
	api.Delete("/security/repositories/:name", s.handlers.DeleteRepositoryScan)
	api.Get("/security/repositories/:name/threat-model", s.handlers.GetThreatModel)
	api.Put("/security/repositories/:name/threat-model", s.handlers.UpdateThreatModel)
	api.Get("/security/repositories/:name/scans", s.handlers.ListSecurityScanRuns)
	api.Post("/security/repositories/:name/scans", s.handlers.CreateManualSecurityScan)
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

	// Repository monitor endpoints
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
	api.Get("/monitors/events", s.handlers.ListRepositoryMonitorEvents)

	// Substrate actor-pool endpoints
	api.Get("/substrate-actor-pools", s.handlers.ListSubstrateActorPools)
	api.Post("/substrate-actor-pools", s.handlers.CreateSubstrateActorPool)
	api.Get("/substrate-actor-pools/:name", s.handlers.GetSubstrateActorPool)
	api.Put("/substrate-actor-pools/:name", s.handlers.UpdateSubstrateActorPool)
	api.Delete("/substrate-actor-pools/:name", s.handlers.DeleteSubstrateActorPool)

	// Auth validation endpoints
	api.Get("/auth/validate", s.handleAuthValidate)
	api.Get("/auth/whoami", s.handleAuthWhoAmI)

	// Reference endpoints (for dropdowns)
	api.Get("/secrets", s.handlers.ListSecretNames)

	// Chat endpoints
	if s.config.Chat.Enabled {
		api.Post("/chat", s.chatHandler.HandleChat)
		api.Get("/chat/config", s.chatHandler.HandleChatConfig)
		api.Delete("/chat/:sessionId", s.chatHandler.HandleCancelChat)
	}

	// OpenAI-compatible API (under /openai/v1, separate from /api/v1)
	// This allows OpenAI-compatible clients to use Orka as a custom provider.
	oai := s.app.Group("/openai/v1")
	oai.Use(externalAuth)
	oai.Post("/chat/completions", s.openaiHandler.HandleChatCompletions)
	oai.Get("/models", s.openaiHandler.HandleListModels)

	// Anthropic-compatible API
	anthropic := s.app.Group("/anthropic/v1")
	anthropic.Use(externalAuth)
	anthropic.Post("/messages", s.anthropicHandler.HandleMessages)
	anthropic.Get("/models", s.anthropicHandler.HandleListModels)

	// Internal API for worker communication
	if s.hasInternalStores() {
		s.internalHandlers = NewInternalHandlers(
			s.ResultStore,
			s.SessionStore,
			s.PlanStore,
			s.MessageStore,
			s.ArtifactStore,
			InternalHandlersConfig{
				Client:              s.client,
				MemoryStore:         s.MemoryStore,
				MemoryProposalStore: s.MemoryProposalStore,
				ExecutionEventStore: s.ExecutionEventStore,
			},
		)
		internal := s.app.Group("/internal/v1")
		internal.Use(NewAuthMiddleware(s.client))
		internal.Post("/results/:namespace/:taskName", s.internalHandlers.SubmitResult)
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
		s.ExecutionEventStore != nil
}

// Start starts the API server
func (s *Server) Start(ctx context.Context) error {
	addr := fmt.Sprintf(":%d", s.config.Port)
	log.Info("starting API server", "address", addr)

	// Start server in a goroutine
	errCh := make(chan error, 1)
	go func() {
		if err := s.app.Listen(addr); err != nil {
			errCh <- err
		}
	}()

	// Wait for shutdown signal or error
	select {
	case <-ctx.Done():
		log.Info("shutting down API server")
		return s.app.Shutdown()
	case err := <-errCh:
		return err
	}
}

// customErrorHandler handles errors returned by handlers and produces a
// consistent JSON envelope for all main API endpoints.
//
// Error response format contract:
//
//   - Main API (/api/...): {"error": {"code": <HTTP status>, "message": "..."}}
//     Handlers return fiber.NewError(code, msg) which is caught here.
//
//   - OpenAI-compatible proxy (/v1/...): follows the OpenAI error spec:
//     {"error": {"message": "...", "type": "...", "param": ..., "code": ...}}
//     These endpoints format errors directly and do NOT use this handler.
//
//   - Anthropic-compatible proxy (/anthropic/...): follows the Anthropic error spec:
//     {"type": "error", "error": {"type": "...", "message": "..."}}
//     These endpoints format errors directly and do NOT use this handler.
//
//   - Chat tool results (internal): {"success": false, "error": "...", "errorType": "...", "suggestion": "..."}
//     This is an internal format between the tool executor and the chat loop,
//     embedded in LLM messages. It is NOT a user-facing API error response.
func customErrorHandler(c fiber.Ctx, err error) error {
	code := fiber.StatusInternalServerError
	message := "Internal Server Error"

	if e, ok := err.(*fiber.Error); ok {
		code = e.Code
		message = e.Message
	}

	// For 404s on non-API paths, serve the SPA index.html
	if code == fiber.StatusNotFound {
		path := c.Path()
		isAPI := len(path) >= 4 && path[:4] == "/api"
		if !isAPI && path != "/healthz" && path != "/readyz" {
			distFS, fsErr := uiembed.FS()
			if fsErr == nil {
				data, readErr := fs.ReadFile(distFS, "index.html")
				if readErr == nil {
					c.Set("Content-Type", "text/html; charset=utf-8")
					return c.Status(fiber.StatusOK).Send(data)
				}
			}
		}
	}

	return c.Status(code).JSON(fiber.Map{
		"error": fiber.Map{
			"code":    code,
			"message": message,
		},
	})
}
