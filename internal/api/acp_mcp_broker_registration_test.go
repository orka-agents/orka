package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"

	"github.com/orka-agents/orka/internal/controller"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

func TestRegisterACPMCPBrokerValidatesDependenciesAndInstallsRoute(t *testing.T) {
	app := fiber.New()
	if err := RegisterACPMCPBroker(app, &controller.ACPMCPBroker{}); err == nil {
		t.Fatal("invalid broker registration unexpectedly succeeded")
	}
	broker := &controller.ACPMCPBroker{
		Credentials: controller.ACPMCPBrokerCredentialResolverFunc(func(context.Context, harnessv2.MCPBrokerCallRequest) (controller.ACPMCPBrokerCredentials, error) {
			return controller.ACPMCPBrokerCredentials{}, store.ErrNotFound
		}),
		Prompts: controller.ACPMCPPromptAuthorizerFunc(func(context.Context, harnessv2.MCPBrokerCallRequest) error { return nil }),
		Executor: controller.ACPMCPToolExecutorFunc(func(context.Context, harnessv2.MCPBrokerCallRequest, harnessv2.MCPToolDescriptor) (json.RawMessage, error) {
			return json.RawMessage(`{"ok":true}`), nil
		}),
		Effects: inertExternalEffectStore{},
	}
	if err := RegisterACPMCPBroker(app, broker); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, harnessv2.MCPBrokerCallPath, strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := app.Test(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode == http.StatusNotFound {
		t.Fatal("registered MCP broker route returned 404")
	}
}

type inertExternalEffectStore struct{}

func (inertExternalEffectStore) ReserveExternalEffect(context.Context, store.ReserveExternalEffectRequest) (*store.ExternalEffect, error) {
	return nil, store.ErrNotFound
}
func (inertExternalEffectStore) GetExternalEffect(context.Context, string) (*store.ExternalEffect, error) {
	return nil, store.ErrNotFound
}
func (inertExternalEffectStore) TransitionExternalEffect(context.Context, store.ExternalEffectTransition) (*store.ExternalEffect, error) {
	return nil, store.ErrNotFound
}
