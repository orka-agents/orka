package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/workspace"
)

func TestSubstrateMCPLegacyIdentityResumesWithoutReplacingActor(t *testing.T) {
	for _, pooled := range []bool{false, true} {
		t.Run(map[bool]string{false: "dedicated", true: "pooled"}[pooled], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
			defer server.Close()
			template := corev1alpha1.WorkspaceTemplateReference{Name: "mcp-template", Namespace: "ate-demo"}
			tool := &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: "mcp-tool", Namespace: defaultNS, UID: "upgraded-tool", Finalizers: []string{substrateMCPToolActorFinalizer}},
				Spec: corev1alpha1.ToolSpec{Description: "MCP tool", MCP: &corev1alpha1.MCPToolServer{
					Path: testMCPPath, SubstrateActor: &corev1alpha1.SubstrateMCPActor{TemplateRef: template, Boot: true},
				}},
			}
			qualified := deterministicSubstrateToolActorID(tool.Namespace, tool.Name, template.Namespace, template.Name)
			var pool *corev1alpha1.SubstrateActorPool
			if pooled {
				pool = &corev1alpha1.SubstrateActorPool{
					ObjectMeta: metav1.ObjectMeta{Name: testMCPPoolName, Namespace: tool.Namespace},
					Spec:       corev1alpha1.SubstrateActorPoolSpec{TemplateRef: template, TargetActors: 1},
					Status:     corev1alpha1.SubstrateActorPoolStatus{TemplateUID: "native-template-uid"},
				}
				tool.Spec.MCP.SubstrateActor.PoolRef = &corev1alpha1.SubstrateActorPoolReference{Name: pool.Name, Namespace: pool.Namespace}
				qualified = workspace.SubstrateActorKey(template.Namespace, deterministicSubstratePoolActorID(deterministicSubstratePoolActorPrefix(pool.Namespace, pool.Name), 0))
			}
			legacy, _, _ := strings.Cut(qualified, ".")
			tool.Annotations = map[string]string{substrateMCPToolActorIDAnno: legacy, substrateMCPToolBootedIDAnno: legacy}
			tool.Status = corev1alpha1.ToolStatus{Available: true, Endpoint: server.URL + testMCPPath, Actor: &corev1alpha1.ToolActorStatus{
				Provider: corev1alpha1.WorkspaceProviderSubstrate, ActorID: legacy, TemplateRef: &template,
				RouteHost: legacy + ".actors.resources.substrate.ate.dev", PoolRef: tool.Spec.MCP.SubstrateActor.PoolRef,
			}}
			lease := newSubstrateMCPToolActorLease(tool, tool.Namespace, legacy)
			objects := []client.Object{tool}
			if pooled {
				tool.Annotations[substrateMCPToolActorPoolNameAnno] = pool.Name
				tool.Annotations[substrateMCPToolActorPoolNamespaceAnno] = pool.Namespace
				lease = newSubstrateMCPPoolActorLease(tool, pool.Namespace, legacy, legacy)
				objects = append(objects, pool)
			}
			lease.UID = "original-lease"
			delete(lease.Annotations, "orka.ai/substrate-actor-ref")
			objects = append(objects, lease)
			failPatch := true
			scheme := newToolScheme()
			c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Tool{}).WithObjects(objects...).
				WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
					if _, isLease := object.(*coordinationv1.Lease); isLease && failPatch {
						failPatch = false
						return errors.New("injected migration write failure")
					}
					return c.Patch(ctx, object, patch, opts...)
				}}).Build()
			executor := &recordingToolWorkspaceExecutor{}
			newReconciler := func() *ToolReconciler {
				return &ToolReconciler{
					Client: c, Scheme: scheme, HTTPClient: server.Client(), SubstrateEnabled: true,
					SubstrateConfig: SubstrateConfig{RouterURL: server.URL, ClaimTimeout: time.Second},
					SubstrateTemplateValidator: func(_ context.Context, request *ExecutionWorkspaceRequest) error {
						request.TemplateUID = "native-template-uid"
						return nil
					},
					SubstrateExecutorFactory: func(SubstrateConfig) (workspace.WorkspaceExecutor, error) { return executor, nil },
				}
			}
			current := &corev1alpha1.Tool{}
			for range 12 {
				// Reconstruct the controller after every write, including the
				// injected failure between status and lease migration.
				if _, err := newReconciler().Reconcile(t.Context(), mcpToolRequest()); err != nil && !strings.Contains(err.Error(), "injected migration write failure") {
					t.Fatal(err)
				}
				if err := c.Get(t.Context(), mcpToolRequest().NamespacedName, current); err != nil {
					t.Fatal(err)
				}
				if current.Status.Available && current.Status.Actor.ActorID == qualified {
					break
				}
			}
			if failPatch || !current.Status.Available || current.Status.Actor.ActorID != qualified || executor.claimName != qualified || len(executor.deletedActorIDs) != 0 {
				t.Fatalf("migration did not preserve the live Actor: status=%+v claims=%q deletes=%v injected=%t", current.Status, executor.claimName, executor.deletedActorIDs, !failPatch)
			}
			if current.Status.Actor.RouteHost != qualified+".actors.resources.substrate.ate.dev" || current.Annotations[substrateMCPToolBootedIDAnno] != qualified {
				t.Fatal("migration did not qualify the route and boot marker")
			}
			for _, booted := range executor.waitReadyBoots {
				if booted {
					t.Fatal("identity migration replayed a boot")
				}
			}
			leases := &coordinationv1.LeaseList{}
			if err := c.List(t.Context(), leases); err != nil || len(leases.Items) != 1 || leases.Items[0].UID != lease.UID || substratePoolActorLeaseActorID(&leases.Items[0]) != qualified {
				t.Fatalf("migration replaced or duplicated the ownership lease: %+v err=%v", leases.Items, err)
			}
			assertMigratedSubstrateMCPFinalization(t, newReconciler(), current, executor, qualified)
		})
	}
}

func assertMigratedSubstrateMCPFinalization(
	t *testing.T, r *ToolReconciler, tool *corev1alpha1.Tool, executor *recordingToolWorkspaceExecutor, qualified string,
) {
	t.Helper()
	if err := r.Delete(t.Context(), tool); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(t.Context(), mcpToolRequest()); err != nil {
		t.Fatal(err)
	}
	if len(executor.deletedActorIDs) != 1 || executor.deletedActorIDs[0] != qualified {
		t.Fatalf("finalization did not use the migrated native identity: %v", executor.deletedActorIDs)
	}
	if err := r.Get(t.Context(), mcpToolRequest().NamespacedName, &corev1alpha1.Tool{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Tool finalization did not settle: %v", err)
	}
	leases := &coordinationv1.LeaseList{}
	if err := r.List(t.Context(), leases); err != nil || len(leases.Items) != 0 {
		t.Fatalf("finalization retained a legacy lease: %+v err=%v", leases.Items, err)
	}
}

func TestSubstrateMCPLeaseAliasesPreserveOwnershipAndCollectTogether(t *testing.T) {
	for _, conflicting := range []bool{false, true} {
		tool := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "mcp-tool", Namespace: defaultNS, UID: "current-tool"}}
		qualified := deterministicSubstrateToolActorID(tool.Namespace, tool.Name, "ate-demo", "mcp-template")
		original := newSubstrateMCPToolActorLease(tool, tool.Namespace, qualified)
		alias := original.DeepCopy()
		alias.Name = qualified
		if conflicting {
			other := tool.DeepCopy()
			other.UID = "other-tool"
			setSubstrateMCPToolActorLeaseHolder(alias, other, qualified)
		}
		c := fake.NewClientBuilder().WithScheme(newToolScheme()).WithObjects(original, alias).Build()
		r := &ToolReconciler{Client: c}
		if err := r.ensureSubstrateMCPToolActorLease(t.Context(), tool, qualified); (err != nil) != conflicting {
			t.Fatalf("conflicting owner=%t: ensure lease error=%v", conflicting, err)
		}
		if err := r.deleteSubstrateMCPToolActorLease(t.Context(), tool, tool.Namespace, qualified); err != nil {
			t.Fatal(err)
		}
		leases := &coordinationv1.LeaseList{}
		if err := c.List(t.Context(), leases); err != nil {
			t.Fatal(err)
		}
		want := 0
		if conflicting {
			want = 2
		}
		if len(leases.Items) != want {
			t.Fatalf("conflicting owner=%t: remaining lease count=%d, want %d", conflicting, len(leases.Items), want)
		}
	}
}

func TestSubstrateMCPIdentityMigrationUsesRecordedTemplateForCleanup(t *testing.T) {
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "mcp-tool", Namespace: defaultNS, UID: "old-binding"},
		Status: corev1alpha1.ToolStatus{Actor: &corev1alpha1.ToolActorStatus{
			Provider: corev1alpha1.WorkspaceProviderSubstrate, ActorID: "historical-actor",
			TemplateRef: &corev1alpha1.WorkspaceTemplateReference{Name: "original", Namespace: "original-space"},
		}},
	}
	c := fake.NewClientBuilder().WithScheme(newToolScheme()).WithStatusSubresource(&corev1alpha1.Tool{}).WithObjects(tool).Build()
	r := &ToolReconciler{Client: c}
	if changed, err := r.migrateSubstrateMCPIdentities(t.Context(), tool, &ExecutionWorkspaceRequest{TemplateName: "replacement", TemplateNamespace: "other-space"}, nil); err != nil || !changed {
		t.Fatalf("historical source migration: changed=%t err=%v", changed, err)
	}
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: tool.Namespace, Name: tool.Name}, tool); err != nil {
		t.Fatal(err)
	}
	if tool.Status.Actor.ActorID != "historical-actor.original-space" {
		t.Fatalf("cleanup rebound to a new Atespace: %s", tool.Status.Actor.ActorID)
	}
}

func TestSubstrateMCPLegacyFinalizationBeforeActorStatus(t *testing.T) {
	for _, mode := range []string{"dedicated", "pooled", "deleting-pool"} {
		t.Run(mode, func(t *testing.T) {
			template := corev1alpha1.WorkspaceTemplateReference{Name: "mcp-template", Namespace: "ate-demo"}
			tool := &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: "mcp-tool", Namespace: defaultNS, UID: "pending-tool", Finalizers: []string{substrateMCPToolActorFinalizer}},
				Spec: corev1alpha1.ToolSpec{Description: "MCP tool", MCP: &corev1alpha1.MCPToolServer{
					SubstrateActor: &corev1alpha1.SubstrateMCPActor{TemplateRef: template},
				}},
			}
			qualified := deterministicSubstrateToolActorID(tool.Namespace, tool.Name, template.Namespace, template.Name)
			var pool *corev1alpha1.SubstrateActorPool
			objects := []client.Object{tool}
			if mode != "dedicated" {
				pool = &corev1alpha1.SubstrateActorPool{
					ObjectMeta: metav1.ObjectMeta{Name: testMCPPoolName, Namespace: tool.Namespace, UID: "original-pool", Finalizers: []string{substrateActorPoolFinalizer}},
					Spec:       corev1alpha1.SubstrateActorPoolSpec{TemplateRef: template, TargetActors: 1},
				}
				tool.Spec.MCP.SubstrateActor.PoolRef = &corev1alpha1.SubstrateActorPoolReference{Name: pool.Name}
				qualified = workspace.SubstrateActorKey(template.Namespace, deterministicSubstratePoolActorID(deterministicSubstratePoolActorPrefix(pool.Namespace, pool.Name), 0))
				objects = append(objects, pool)
			}
			legacy, _, _ := strings.Cut(qualified, ".")
			tool.Annotations = map[string]string{substrateMCPToolActorIDAnno: legacy}
			lease := newSubstrateMCPToolActorLease(tool, tool.Namespace, legacy)
			if pool != nil {
				tool.Annotations[substrateMCPToolActorPoolNameAnno] = pool.Name
				tool.Annotations[substrateMCPToolActorPoolNamespaceAnno] = pool.Namespace
				lease = newSubstrateMCPPoolActorLease(tool, pool.Namespace, legacy, legacy)
			}
			delete(lease.Annotations, "orka.ai/substrate-actor-ref")
			objects = append(objects, lease)
			scheme := newToolScheme()
			c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Tool{}).WithObjects(objects...).Build()
			require.NoError(t, c.Delete(t.Context(), tool))
			if mode == "deleting-pool" {
				require.NoError(t, c.Delete(t.Context(), pool))
			}
			executor := &recordingToolWorkspaceExecutor{}
			deleted := false
			for range 12 {
				// Native template availability and new admission are irrelevant
				// to cleanup; reconstruct the controller between durable writes.
				r := &ToolReconciler{Client: c, Scheme: scheme,
					SubstrateTemplateValidator: func(context.Context, *ExecutionWorkspaceRequest) error {
						t.Fatal("finalization tried to validate native template admission")
						return nil
					},
					SubstrateExecutorFactory: func(SubstrateConfig) (workspace.WorkspaceExecutor, error) { return executor, nil },
				}
				_, err := r.Reconcile(t.Context(), mcpToolRequest())
				require.NoError(t, err)
				current := &corev1alpha1.Tool{}
				if err := c.Get(t.Context(), mcpToolRequest().NamespacedName, current); apierrors.IsNotFound(err) {
					deleted = true
					break
				} else {
					require.NoError(t, err)
					require.Nil(t, current.Status.Actor)
				}
			}
			require.True(t, deleted)
			require.Equal(t, []string{qualified}, executor.deletedActorIDs)
			leases := &coordinationv1.LeaseList{}
			require.NoError(t, c.List(t.Context(), leases))
			require.Empty(t, leases.Items)
		})
	}
}
