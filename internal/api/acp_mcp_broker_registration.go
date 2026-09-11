package api

import (
	"fmt"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"

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
	app.Post(harnessv2.MCPBrokerCallPath, adaptor.HTTPHandler(broker))
	return nil
}

// RegisterACPMCPBroker installs the prompt-scoped broker on this API server.
func (s *Server) RegisterACPMCPBroker(broker *controller.ACPMCPBroker) error {
	if s == nil {
		return fmt.Errorf("api server is required")
	}
	return RegisterACPMCPBroker(s.app, broker)
}
