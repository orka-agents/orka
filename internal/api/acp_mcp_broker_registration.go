package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"
	"go.opentelemetry.io/otel/trace"

	"github.com/orka-agents/orka/internal/controller"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// RegisterACPMCPBroker installs the controller-side prompt MCP broker without
// coupling broker construction to Server or cmd/main.go. Callers construct the
// broker with the current durable stores and controller clients, then register
// it before starting the Fiber app.
func RegisterACPMCPBroker(app *fiber.App, broker *controller.ACPMCPBroker) error {
	if app == nil {
		return fmt.Errorf("fiber app is required")
	}
	if err := broker.Validate(); err != nil {
		return err
	}
	app.Post(harnessv2.MCPBrokerCallPath, adaptor.HTTPHandlerWithContext(acpMCPBrokerHTTPHandler(broker)))
	return nil
}

// acpMCPBrokerHTTPHandler runs the broker on a context the handler owns.
//
// fasthttpadaptor passes the pooled *fasthttp.RequestCtx as r.Context(), and
// fasthttp forbids using it after the handler returns. That context is neither
// a context.WithCancel context nor an AfterFunc implementer, so every
// WithCancel, WithDeadline or WithTimeout derived from it starts a
// standard-library goroutine that calls its Done method whenever that goroutine
// is scheduled, possibly after the handler has returned, the RequestCtx has been
// reused, or the server has shut down. The broker therefore receives only the
// API server span, and server shutdown is forwarded through a watcher that holds
// just the Done channel and is joined before the handler returns.
func acpMCPBrokerHTTPHandler(broker http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.Background()
		// The adaptor stores Fiber's context separately. Copy only its server
		// span so broker and tool spans keep their parent.
		if local, ok := adaptor.LocalContextFromHTTPRequest(r); ok {
			ctx = trace.ContextWithSpanContext(ctx, trace.SpanContextFromContext(local))
		}
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		if serverDone := r.Context().Done(); serverDone != nil {
			handlerDone := make(chan struct{})
			watcherDone := make(chan struct{})
			go func() {
				defer close(watcherDone)
				select {
				case <-serverDone:
					cancel()
				case <-handlerDone:
				}
			}()
			defer func() {
				close(handlerDone)
				<-watcherDone
			}()
		}
		broker.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RegisterACPMCPBroker installs the prompt-scoped broker on this API server.
func (s *Server) RegisterACPMCPBroker(broker *controller.ACPMCPBroker) error {
	if s == nil {
		return fmt.Errorf("api server is required")
	}
	return RegisterACPMCPBroker(s.app, broker)
}
