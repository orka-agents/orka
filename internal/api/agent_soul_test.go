package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/executionmode"
)

func TestHandlers_CreateAgent_ValidatesDefaultedSoulRuntime(t *testing.T) {
	for _, mode := range []executionmode.Mode{executionmode.HarnessV1, executionmode.HarnessV2} {
		for _, runtimeType := range []corev1alpha1.AgentRuntimeType{
			"", corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeClaude,
			corev1alpha1.AgentRuntimeCopilot, corev1alpha1.AgentRuntimeOpencode,
		} {
			t.Run(string(mode)+"/"+string(runtimeType), func(t *testing.T) {
				scheme := runtime.NewScheme()
				require.NoError(t, corev1alpha1.AddToScheme(scheme))
				c := fake.NewClientBuilder().WithScheme(scheme).Build()
				h := NewHandlers(HandlersConfig{Client: c, ExecutionMode: mode})
				app := fiber.New()
				app.Post("/agents", h.CreateAgent)
				spec := corev1alpha1.AgentSpec{Soul: &corev1alpha1.SoulSource{Inline: "persona"}}
				if runtimeType != "" {
					spec.Runtime = &corev1alpha1.AgentCLIRuntime{Type: runtimeType}
				}
				body, err := json.Marshal(CreateAgentRequest{Name: "soul-agent", Namespace: "default", Spec: spec})
				require.NoError(t, err)
				req := httptest.NewRequest(http.MethodPost, "/agents", bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req)
				require.NoError(t, err)
				defer resp.Body.Close() //nolint:errcheck
				created := &corev1alpha1.Agent{}
				err = c.Get(context.Background(), types.NamespacedName{Name: "soul-agent", Namespace: "default"}, created)
				if mode == executionmode.HarnessV1 && runtimeType != "" {
					require.Equal(t, http.StatusBadRequest, resp.StatusCode)
					require.True(t, apierrors.IsNotFound(err), "unsupported soul must not be persisted")
					return
				}
				require.Equal(t, http.StatusCreated, resp.StatusCode)
				require.NoError(t, err)
				require.Equal(t, "persona", created.Spec.Soul.Inline)
				if runtimeType != "" {
					require.Equal(t, corev1alpha1.AgentRuntimeContractHarnessV2, created.BuiltInContractVersion())
				}
			})
		}
	}
}
