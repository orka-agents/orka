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

func acpMCPBrokerHTTPHandler(broker http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.Background()
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

func (s *Server) RegisterACPMCPBroker(broker *controller.ACPMCPBroker) error {
	if s == nil {
		return fmt.Errorf("api server is required")
	}
	return RegisterACPMCPBroker(s.app, broker)
}
