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
	Chat                      ChatConfig
	ResultStore               store.ResultStore
	SessionStore              store.SessionStore
	PlanStore                 store.PlanStore
	MessageStore              store.MessageStore
	HealthChecker             store.HealthChecker
	Clientset                 kubernetes.Interface
}

// Server is the REST API server
type Server struct {
	app              *fiber.App
	client           client.Client
	config           ServerConfig
	sessionManager   *controller.SessionManager
	handlers         *Handlers
	chatHandler      *ChatHandler
	openaiHandler    *OpenAICompatHandler
	internalHandlers *InternalHandlers
	ResultStore      store.ResultStore
	SessionStore     store.SessionStore
	PlanStore        store.PlanStore
	MessageStore     store.MessageStore
}

// NewServer creates a new API server
func NewServer(c client.Client, sessionManager *controller.SessionManager, config ServerConfig) *Server {
	app := fiber.New(fiber.Config{
		AppName:      "Orka API",
		ErrorHandler: customErrorHandler,
	})

	server := &Server{
		app:            app,
		client:         c,
		config:         config,
		sessionManager: sessionManager,
		ResultStore:    config.ResultStore,
		SessionStore:   config.SessionStore,
		PlanStore:      config.PlanStore,
		MessageStore:   config.MessageStore,
	}

	server.handlers = NewHandlers(c, sessionManager, config.WatchNamespace, config.EnforceNamespaceIsolation, config.ResultStore, config.SessionStore, config.PlanStore, config.Clientset, config.HealthChecker)
	server.chatHandler = NewChatHandler(c, sessionManager, config.Chat, config.WatchNamespace, config.EnforceNamespaceIsolation, config.SessionStore, config.ResultStore)
	server.openaiHandler = NewOpenAICompatHandler(c, config.WatchNamespace, config.EnforceNamespaceIsolation, config.Chat)
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
		AllowHeaders: []string{"Origin", "Content-Type", "Accept", "Authorization"},
	}))

	// Logging middleware
	s.app.Use(NewLoggingMiddleware())

	// Metrics middleware
	s.app.Use(NewMetricsMiddleware())
}

// setupRoutes configures the API routes
func (s *Server) setupRoutes() {
	// Health endpoints
	s.app.Get("/healthz", s.handlers.Healthz)
	s.app.Get("/readyz", s.handlers.Readyz)

	// API v1 group
	api := s.app.Group("/api/v1")

	// Auth middleware for API endpoints
	api.Use(NewAuthMiddleware(s.client))

	// Task endpoints
	api.Post("/tasks", s.handlers.CreateTask)
	api.Get("/tasks", s.handlers.ListTasks)
	api.Get("/tasks/:id", s.handlers.GetTask)
	api.Delete("/tasks/:id", s.handlers.DeleteTask)
	api.Get("/tasks/:id/logs", s.handlers.GetTaskLogs)
	api.Get("/tasks/:id/result", s.handlers.GetTaskResult)
	api.Get("/tasks/:id/plan", s.handlers.GetTaskPlan)
	api.Get("/tasks/:id/children", s.handlers.GetTaskChildren)

	// Session endpoints
	api.Get("/sessions", s.handlers.ListSessions)
	api.Get("/sessions/:id", s.handlers.GetSession)
	api.Delete("/sessions/:id", s.handlers.DeleteSession)

	// Tool endpoints
	api.Get("/tools", s.handlers.ListTools)
	api.Get("/tools/:name", s.handlers.GetTool)

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

	// Auth validation endpoint
	api.Get("/auth/validate", s.handleAuthValidate)

	// Reference endpoints (for dropdowns)
	api.Get("/secrets", s.handlers.ListSecretNames)

	// Chat endpoints
	if s.config.Chat.Enabled {
		api.Post("/chat", s.chatHandler.HandleChat)
		api.Get("/chat/config", s.chatHandler.HandleChatConfig)
		api.Delete("/chat/:sessionId", s.chatHandler.HandleCancelChat)
	}

	// OpenAI-compatible API (under /v1, separate from /api/v1)
	// This allows OpenAI-compatible clients to use Orka as a custom provider.
	oai := s.app.Group("/v1")
	oai.Use(NewAuthMiddleware(s.client))
	oai.Post("/chat/completions", s.openaiHandler.HandleChatCompletions)
	oai.Get("/models", s.openaiHandler.HandleListModels)

	// Internal API for worker communication
	if s.ResultStore != nil && s.SessionStore != nil {
		s.internalHandlers = NewInternalHandlers(s.ResultStore, s.SessionStore, s.PlanStore, s.MessageStore)
		internal := s.app.Group("/internal/v1")
		internal.Use(NewAuthMiddleware(s.client))
		internal.Post("/results/:namespace/:taskName", s.internalHandlers.SubmitResult)
		internal.Get("/sessions/:namespace/:name/transcript", s.internalHandlers.GetSessionTranscript)
		if s.PlanStore != nil {
			internal.Post("/plans/:namespace/:taskName", s.internalHandlers.SubmitPlan)
			internal.Get("/plans/:namespace/:taskName", s.internalHandlers.GetPlan)
		}
		if s.MessageStore != nil {
			internal.Post("/messages/:namespace", s.internalHandlers.SendMessage)
			internal.Get("/messages/:namespace/:taskName", s.internalHandlers.GetMessages)
		}
	}
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

// customErrorHandler handles errors returned by handlers
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
