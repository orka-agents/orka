package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/workspace"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSubstrateMCPPoolRequiresPinnedTemplateIdentity(t *testing.T) {
	for _, scenario := range []struct {
		name, pinnedUID, message string
	}{
		{name: "unbound", message: "has not pinned"},
		{name: "replaced", pinnedUID: "original-template-uid", message: "different native ActorTemplate UID"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
			defer server.Close()
			template := corev1alpha1.WorkspaceTemplateReference{Name: "mcp-template", Namespace: "ate-demo"}
			pool := &corev1alpha1.SubstrateActorPool{
				ObjectMeta: metav1.ObjectMeta{Name: testMCPPoolName, Namespace: defaultNS, UID: "pool-uid"},
				Spec:       corev1alpha1.SubstrateActorPoolSpec{TemplateRef: template, TargetActors: 1},
				Status:     corev1alpha1.SubstrateActorPoolStatus{TemplateUID: scenario.pinnedUID, Phase: corev1alpha1.SubstrateActorPoolPhaseReady},
			}
			tool := &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: "mcp-tool", Namespace: defaultNS, UID: "tool-uid"},
				Spec: corev1alpha1.ToolSpec{Description: "MCP tool", MCP: &corev1alpha1.MCPToolServer{
					Path: testMCPPath, SubstrateActor: &corev1alpha1.SubstrateMCPActor{
						TemplateRef: template, PoolRef: &corev1alpha1.SubstrateActorPoolReference{Name: pool.Name}, Boot: true,
					},
				}},
			}
			scheme := newToolScheme()
			executor := &recordingToolWorkspaceExecutor{claimCreated: true}
			executorCreated := false
			r := &ToolReconciler{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(tool, pool).WithObjects(tool, pool).Build(),
				Scheme: scheme, HTTPClient: server.Client(), SubstrateEnabled: true,
				SubstrateConfig: SubstrateConfig{RouterURL: server.URL, ClaimTimeout: time.Second},
				SubstrateTemplateValidator: func(_ context.Context, request *ExecutionWorkspaceRequest) error {
					request.TemplateUID = "current-template-uid"
					return nil
				},
				SubstrateExecutorFactory: func(SubstrateConfig) (workspace.WorkspaceExecutor, error) {
					executorCreated = true
					return executor, nil
				},
			}
			for range 2 {
				restarted := *r
				_, err := restarted.Reconcile(t.Context(), mcpToolRequest())
				require.NoError(t, err)
			}
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(tool), tool))
			require.False(t, tool.Status.Available)
			require.Contains(t, tool.Status.Error, scenario.message)
			require.False(t, executorCreated)
			leases := &coordinationv1.LeaseList{}
			require.NoError(t, r.List(t.Context(), leases))
			require.Empty(t, leases.Items)
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pool), pool))
			require.Equal(t, scenario.pinnedUID, pool.Status.TemplateUID)

			if scenario.name == "unbound" {
				// Admission resumes once the pool controller persists its binding.
				pool.Status.TemplateUID = "current-template-uid"
				require.NoError(t, r.Status().Update(t.Context(), pool))
			} else {
				// A replacement template requires a new pool with its own binding.
				replacement := pool.DeepCopy()
				replacement.Name, replacement.UID, replacement.ResourceVersion = "replacement-pool", "replacement-pool-uid", ""
				replacement.Status = corev1alpha1.SubstrateActorPoolStatus{}
				require.NoError(t, r.Create(t.Context(), replacement))
				replacement.Status.TemplateUID = "current-template-uid"
				require.NoError(t, r.Status().Update(t.Context(), replacement))
				tool.Spec.MCP.SubstrateActor.PoolRef.Name = replacement.Name
				require.NoError(t, r.Update(t.Context(), tool))
			}
			for range 4 {
				_, err := r.Reconcile(t.Context(), mcpToolRequest())
				require.NoError(t, err)
			}
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(tool), tool))
			require.True(t, tool.Status.Available)
			require.Equal(t, "current-template-uid", executor.claimTemplate.UID)
		})
	}
}
