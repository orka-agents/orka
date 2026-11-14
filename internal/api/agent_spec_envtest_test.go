// Copyright (c) 2026. MIT License - see LICENSE file for details.

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/gofiber/fiber/v3"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

// Real CRD generation, status subresource and resourceVersion behavior underpin
// these deterministic read/patch interleavings. Only the scheduling is injected.
func TestAgentSpecAPIServerConcurrency(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS to run Agent spec concurrency against an API server")
	}
	data, err := os.ReadFile("../../config/crd/bases/core.orka.ai_agents.yaml")
	require.NoError(t, err)
	crd := &apiextensionsv1.CustomResourceDefinition{}
	require.NoError(t, yaml.Unmarshal(data, crd))
	environment := &envtest.Environment{CRDs: []*apiextensionsv1.CustomResourceDefinition{crd}}
	cfg, err := environment.Start()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, environment.Stop()) })
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	direct, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	require.NoError(t, err)

	for _, tc := range []struct {
		name, write string
		unchanged   bool
		wantStatus  int
	}{
		{"status-only retry", "status", false, 200},
		{"outside spec edit", "spec", false, 409},
		{"same-name replacement", "replace", false, 409},
		{"deleted before patch", "delete", false, 404},
		{"status conflicts exhausted", "churn", false, 409},
		{"unchanged spec still fenced", "spec", true, 409},
		{"unchanged spec status retry", "status", true, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			agent := conditionalAgentFixture()
			agent.Name = ""
			agent.GenerateName = "conditional-"
			agent.UID = ""
			agent.ResourceVersion = ""
			agent.Generation = 0
			require.NoError(t, direct.Create(ctx, agent))
			original := agent.DeepCopy()
			calls := 0
			conflicts := 0
			wrapped := interceptor.NewClient(direct, interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				calls++
				if calls == 1 || tc.write == "churn" {
					outside := &corev1alpha1.Agent{}
					require.NoError(t, direct.Get(ctx, client.ObjectKeyFromObject(agent), outside))
					switch tc.write {
					case "status", "churn":
						outside.Status.ActiveTasks++
						require.NoError(t, direct.Status().Update(ctx, outside))
						require.Equal(t, original.Generation, outside.Generation)
					case "spec":
						outside.Spec.Model.Name = "outside"
						require.NoError(t, direct.Update(ctx, outside))
						require.Greater(t, outside.Generation, original.Generation)
					case "replace", "delete":
						require.NoError(t, direct.Delete(ctx, outside))
						if tc.write == "replace" {
							replacement := &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: agent.Name, Namespace: agent.Namespace}, Spec: original.Spec}
							replacement.Spec.Model = replacement.Spec.Model.DeepCopy()
							replacement.Spec.Model.Name = "replacement"
							require.NoError(t, direct.Create(ctx, replacement))
							require.NotEqual(t, original.UID, replacement.UID)
						}
					}
				}
				err := c.Patch(ctx, obj, patch, opts...)
				if apierrors.IsConflict(err) {
					conflicts++
				}
				return err
			}})
			h := NewHandlers(HandlersConfig{Client: wrapped, APIReader: direct})
			app := fiber.New()
			app.Put("/agents/:name/spec", h.UpdateAgentSpec)
			spec := `{"model":{"provider":"openai","name":"desired"}}`
			if tc.unchanged {
				bytes, err := json.Marshal(original.Spec)
				require.NoError(t, err)
				spec = string(bytes)
			}
			body := fmt.Sprintf(`{"expectedUID":%q,"expectedGeneration":%d,"spec":%s}`, agent.UID, agent.Generation, spec)
			status, response := agentSpecRequest(t, app, "/agents/"+agent.Name+"/spec", body)
			require.Equal(t, tc.wantStatus, status, string(response))
			stored := &corev1alpha1.Agent{}
			err = direct.Get(ctx, client.ObjectKeyFromObject(agent), stored)
			if tc.write == "delete" {
				require.True(t, apierrors.IsNotFound(err))
				return
			}
			require.NoError(t, err)
			require.Positive(t, conflicts, "the real API server must reject the stale patch")
			switch tc.write {
			case "status":
				require.Equal(t, int32(1), stored.Status.ActiveTasks)
				if tc.unchanged {
					require.Equal(t, "original", stored.Spec.Model.Name)
					require.Equal(t, original.Generation, stored.Generation)
				} else {
					require.Equal(t, "desired", stored.Spec.Model.Name)
					require.Equal(t, original.Generation+1, stored.Generation)
				}
			case "spec":
				require.Equal(t, "outside", stored.Spec.Model.Name)
				require.Equal(t, "agent UID or generation does not match", string(response))
			case "replace":
				require.Equal(t, "replacement", stored.Spec.Model.Name)
				require.NotEqual(t, original.UID, stored.UID)
			case "churn":
				require.Equal(t, "original", stored.Spec.Model.Name)
				require.Equal(t, "agent was modified concurrently", string(response))
			}
		})
	}
}
