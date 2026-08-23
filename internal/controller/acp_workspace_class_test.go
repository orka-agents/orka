/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	acpworkspacev1alpha1 "github.com/orka-agents/orka/api/acp.workspace/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
)

const (
	acpTestProviderName = "acp-provider"
	acpTestConfigName   = "acp-config"
	acpTestClassName    = "acp-class"
	acpTestNamespace    = "default"
	acpTestSessionName  = "session-a"
	acpTestInfraName    = "infra"
)

func testACPWorkspaceScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := testWorkspaceScheme(t)
	if err := acpworkspacev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add acp.workspace scheme: %v", err)
	}
	return scheme
}

type acpClassFixture struct {
	class    *workspacev1alpha1.ExecutionWorkspaceClass
	provider *workspacev1alpha1.ExecutionWorkspaceProvider
	config   *acpworkspacev1alpha1.RuntimeProviderConfig
	profile  *acpworkspacev1alpha1.RuntimeWorkspaceProfile
}

func (f *acpClassFixture) objects() []client.Object {
	return []client.Object{f.class, f.provider, f.config, f.profile}
}

// pinProfileHash recomputes and pins the class profile hash exactly the way
// the class controller would, using the unstructured shape of the profile.
func (f *acpClassFixture) pinProfileHash(t *testing.T) {
	t.Helper()
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(f.profile)
	if err != nil {
		t.Fatalf("convert profile: %v", err)
	}
	u := &unstructured.Unstructured{Object: raw}
	u.SetGroupVersionKind(acpworkspacev1alpha1.GroupVersion.WithKind(acpWorkspaceProviderProfileKind))
	hash, err := acpWorkspaceClassProfileHash(f.class, f.provider, u)
	if err != nil {
		t.Fatalf("hash class profile: %v", err)
	}
	f.class.Status.ProfileHash = hash
}

func newACPClassFixture(t *testing.T, backend acpworkspacev1alpha1.RuntimeProviderBackend, mutate ...func(*acpClassFixture)) *acpClassFixture {
	t.Helper()
	fixture := &acpClassFixture{
		provider: &workspacev1alpha1.ExecutionWorkspaceProvider{
			ObjectMeta: metav1.ObjectMeta{Name: acpTestProviderName, UID: types.UID("acp-provider-uid"), Generation: 1},
			Spec: workspacev1alpha1.ExecutionWorkspaceProviderSpec{
				ControllerName: acpWorkspaceProviderControllerName,
				ParametersRef: workspacev1alpha1.TypedObjectReference{
					Group: acpworkspacev1alpha1.GroupVersion.Group, Kind: acpWorkspaceProviderConfigKind, Name: acpTestConfigName,
				},
				LifecycleState:    workspacev1alpha1.ExecutionWorkspaceProviderActive,
				RequiredContracts: []string{workspacev1alpha1.ContractVersionV1},
			},
			Status: workspacev1alpha1.ExecutionWorkspaceProviderStatus{
				ObservedGeneration: 1,
				Conditions: []metav1.Condition{{
					Type: string(workspacev1alpha1.ConditionProviderReady), Status: metav1.ConditionTrue,
					Reason: string(workspacev1alpha1.ReasonReady), ObservedGeneration: 1,
				}},
			},
		},
		config: &acpworkspacev1alpha1.RuntimeProviderConfig{
			ObjectMeta: metav1.ObjectMeta{Name: acpTestConfigName, UID: types.UID("acp-config-uid"), Generation: 1},
			Spec:       acpworkspacev1alpha1.RuntimeProviderConfigSpec{Backend: backend},
		},
		profile: &acpworkspacev1alpha1.RuntimeWorkspaceProfile{
			ObjectMeta: metav1.ObjectMeta{Namespace: acpTestNamespace, Name: "acp-profile", UID: types.UID("acp-profile-uid"), Generation: 1},
		},
	}
	if backend == acpworkspacev1alpha1.RuntimeProviderBackendSubstrate {
		fixture.profile.Spec.Substrate = &acpworkspacev1alpha1.SubstrateProfileSpec{
			TemplateRef: acpworkspacev1alpha1.SubstrateTemplateReference{Name: "infra-template", Namespace: "substrate-system"},
		}
	}
	fixture.class = &workspacev1alpha1.ExecutionWorkspaceClass{
		ObjectMeta: metav1.ObjectMeta{Namespace: acpTestNamespace, Name: acpTestClassName, UID: types.UID("acp-class-uid"), Generation: 1},
		Spec: workspacev1alpha1.ExecutionWorkspaceClassSpec{
			ProviderRef: &workspacev1alpha1.ClusterObjectReference{Name: acpTestProviderName},
			ParametersRef: &workspacev1alpha1.TypedObjectReference{
				Group: acpworkspacev1alpha1.GroupVersion.Group, Kind: acpWorkspaceProviderProfileKind, Name: "acp-profile",
			},
			Mode:               workspacev1alpha1.ExecutionWorkspaceModeInteractive,
			AllowedReuseScopes: []workspacev1alpha1.WorkspaceReuseScope{workspacev1alpha1.WorkspaceReuseScopeNone, workspacev1alpha1.WorkspaceReuseScopeSession},
			Lifecycle: workspacev1alpha1.ExecutionWorkspaceLifecycle{
				DefaultOnDetach: workspacev1alpha1.WorkspaceOnDetachDelete,
				AllowedOnDetach: []workspacev1alpha1.WorkspaceOnDetach{workspacev1alpha1.WorkspaceOnDetachDelete},
				DetachTimeout:   metav1.Duration{Duration: 2 * time.Minute},
				MaxLifetime:     &metav1.Duration{Duration: 8 * time.Hour},
				DeletionPolicy: workspacev1alpha1.ExecutionWorkspaceDeletionPolicy{
					ProviderResources: workspacev1alpha1.WorkspaceDeletionActionDelete,
					PersistentVolumes: workspacev1alpha1.WorkspaceDeletionActionDelete,
					Checkpoints:       workspacev1alpha1.WorkspaceDeletionActionDelete,
				},
			},
		},
		Status: workspacev1alpha1.ExecutionWorkspaceClassStatus{
			ObservedGeneration: 1,
			ProviderRef:        &workspacev1alpha1.ClusterObjectReference{Name: acpTestProviderName},
			Conditions: []metav1.Condition{{
				Type: string(workspacev1alpha1.ConditionClassReady), Status: metav1.ConditionTrue,
				Reason: string(workspacev1alpha1.ReasonReady), ObservedGeneration: 1,
			}},
		},
	}
	for _, m := range mutate {
		m(fixture)
	}
	fixture.pinProfileHash(t)
	return fixture
}

func acpClassTestTask(mutate ...func(*corev1alpha1.Task)) *corev1alpha1.Task {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: acpTestNamespace, Name: "class-task", UID: types.UID("class-task-uid"), Generation: 1,
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			Execution: &corev1alpha1.ExecutionSpec{Workspace: &corev1alpha1.ExecutionWorkspaceSpec{
				ClassRef: &corev1alpha1.WorkspaceClassReference{Name: acpTestClassName},
			}},
		},
	}
	for _, m := range mutate {
		m(task)
	}
	return task
}

// admitTestACPWorkspace stands in for the workspace core controller and the
// ACP adapter: it persists the fake API-server identity, the core admission
// marker, and an adapter-observed Ready status so attachment can proceed.
func admitTestACPWorkspace(t *testing.T, r *TaskReconciler, workspace *workspacev1alpha1.ExecutionWorkspace) {
	t.Helper()
	ctx := context.Background()
	if workspace.UID == "" {
		// The fake client does not assign object UIDs on Create.
		workspace.UID = types.UID(workspace.Name + "-uid")
	}
	if workspace.CreationTimestamp.IsZero() {
		// The fake client does not stamp creation time; maxLifetime clamping
		// would otherwise treat the workspace as already expired.
		workspace.CreationTimestamp = metav1.Now()
	}
	markWorkspaceAdmittedForPolicyReview(workspace, workspace.Generation)
	if err := r.Update(ctx, workspace); err != nil {
		t.Fatalf("admit workspace: %v", err)
	}
	// The spec update zeroes the in-memory status under the fake status
	// subresource; re-apply the adapter-observed pieces before persisting.
	markWorkspaceAdmittedForPolicyReview(workspace, workspace.Generation)
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
	if err := r.Status().Update(ctx, workspace); err != nil {
		t.Fatalf("mark workspace status: %v", err)
	}
}

func acpClassTestReconciler(t *testing.T, objects ...client.Object) *TaskReconciler {
	t.Helper()
	scheme := testACPWorkspaceScheme(t)
	builder := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(
		&workspacev1alpha1.ExecutionWorkspace{},
		&workspacev1alpha1.ExecutionWorkspaceClass{},
		&workspacev1alpha1.ExecutionWorkspaceProvider{},
		&corev1alpha1.Task{},
	)
	if len(objects) > 0 {
		builder = builder.WithObjects(objects...)
	}
	c := builder.Build()
	return &TaskReconciler{
		Client: c, APIReader: c, Scheme: scheme,
		WorkspaceProviderAPIEnabled: true,
	}
}

func TestResolveACPWorkspaceClassMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		backend     acpworkspacev1alpha1.RuntimeProviderBackend
		mutate      func(*acpClassFixture)
		mutateAfter func(*acpClassFixture)
		task        *corev1alpha1.Task
		wantErr     string
		check       func(*testing.T, *acpResolvedWorkspaceClass)
	}{
		{
			name:    "agent-sandbox class resolves",
			backend: acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox,
			check: func(t *testing.T, resolved *acpResolvedWorkspaceClass) {
				if resolved.Backend != corev1alpha1.WorkspaceProviderAgentSandbox {
					t.Fatalf("backend = %s", resolved.Backend)
				}
				if resolved.SubstrateTemplateName != "" || resolved.SubstrateTemplateNamespace != "" {
					t.Fatalf("agent-sandbox class resolved a substrate template")
				}
				if resolved.Binding.UID != "acp-class-uid" || resolved.Binding.ProviderUID != "acp-provider-uid" {
					t.Fatalf("binding identity = %+v", resolved.Binding)
				}
				if resolved.Binding.MaxLifetime != "8h0m0s" || resolved.Binding.DetachTimeout != "2m0s" {
					t.Fatalf("binding lifecycle = %+v", resolved.Binding)
				}
			},
		},
		{
			name:    "substrate class resolves infrastructure template",
			backend: acpworkspacev1alpha1.RuntimeProviderBackendSubstrate,
			check: func(t *testing.T, resolved *acpResolvedWorkspaceClass) {
				if resolved.Backend != corev1alpha1.WorkspaceProviderSubstrate {
					t.Fatalf("backend = %s", resolved.Backend)
				}
				if resolved.SubstrateTemplateNamespace != "substrate-system" || resolved.SubstrateTemplateName != "infra-template" {
					t.Fatalf("substrate template = %s/%s", resolved.SubstrateTemplateNamespace, resolved.SubstrateTemplateName)
				}
			},
		},
		{
			name:    "substrate template namespace defaults to the class namespace",
			backend: acpworkspacev1alpha1.RuntimeProviderBackendSubstrate,
			mutate: func(f *acpClassFixture) {
				f.profile.Spec.Substrate.TemplateRef.Namespace = ""
			},
			check: func(t *testing.T, resolved *acpResolvedWorkspaceClass) {
				if resolved.SubstrateTemplateNamespace != acpTestNamespace {
					t.Fatalf("substrate template namespace = %s", resolved.SubstrateTemplateNamespace)
				}
			},
		},
		{
			name:    "class not ready at current generation",
			backend: acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox,
			mutateAfter: func(f *acpClassFixture) {
				f.class.Status.Conditions[0].ObservedGeneration = 0
			},
			wantErr: "not ready at its current generation",
		},
		{
			name:    "pinned profile hash drift fails closed",
			backend: acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox,
			mutateAfter: func(f *acpClassFixture) {
				f.class.Status.ProfileHash = "sha256:" + strings.Repeat("0", 64)
			},
			wantErr: "drifted from its pinned hash",
		},
		{
			name:    "foreign provider controllerName",
			backend: acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox,
			mutate: func(f *acpClassFixture) {
				f.provider.Spec.ControllerName = "someone.else/adapter"
			},
			wantErr: "is not the ACP RuntimePool adapter",
		},
		{
			name:    "draining provider rejects new workspaces",
			backend: acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox,
			mutate: func(f *acpClassFixture) {
				f.provider.Spec.LifecycleState = workspacev1alpha1.ExecutionWorkspaceProviderDraining
			},
			wantErr: "rejects new ACP workspaces",
		},
		{
			name:    "provider not ready",
			backend: acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox,
			mutateAfter: func(f *acpClassFixture) {
				f.provider.Status.Conditions[0].Status = metav1.ConditionFalse
			},
			wantErr: "is not ready",
		},
		{
			name:    "class parameters kind is not an ACP profile",
			backend: acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox,
			mutate: func(f *acpClassFixture) {
				f.class.Spec.ParametersRef.Kind = "SomethingElse"
			},
			wantErr: "is not an ACP RuntimeWorkspaceProfile",
		},
		{
			name:    "provider parameters kind is not an ACP config",
			backend: acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox,
			mutate: func(f *acpClassFixture) {
				f.provider.Spec.ParametersRef.Kind = "SomethingElse"
			},
			wantErr: "is not an ACP RuntimeProviderConfig",
		},
		{
			name:    "service mode classes are rejected",
			backend: acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox,
			mutate: func(f *acpClassFixture) {
				f.class.Spec.Mode = workspacev1alpha1.ExecutionWorkspaceModeService
			},
			wantErr: "only Interactive classes",
		},
		{
			name:    "retaining deletion policy is rejected",
			backend: acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox,
			mutate: func(f *acpClassFixture) {
				f.class.Spec.Lifecycle.DeletionPolicy.PersistentVolumes = workspacev1alpha1.WorkspaceDeletionActionRetain
			},
			wantErr: "retained workspace data is not yet supported",
		},
		{
			name:    "substrate profile without a template",
			backend: acpworkspacev1alpha1.RuntimeProviderBackendSubstrate,
			mutate: func(f *acpClassFixture) {
				f.profile.Spec.Substrate = nil
			},
			wantErr: "must name the operator-owned Substrate infrastructure ActorTemplate",
		},
		{
			name:    "agent-sandbox profile with substrate inputs",
			backend: acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox,
			mutate: func(f *acpClassFixture) {
				f.profile.Spec.Substrate = &acpworkspacev1alpha1.SubstrateProfileSpec{
					TemplateRef: acpworkspacev1alpha1.SubstrateTemplateReference{Name: acpTestInfraName},
				}
			},
			wantErr: "backend is agent-sandbox",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mutations := []func(*acpClassFixture){}
			if tt.mutate != nil {
				mutations = append(mutations, tt.mutate)
			}
			fixture := newACPClassFixture(t, tt.backend, mutations...)
			if tt.mutateAfter != nil {
				tt.mutateAfter(fixture)
			}
			task := tt.task
			if task == nil {
				task = acpClassTestTask()
			}
			r := acpClassTestReconciler(t, fixture.objects()...)
			resolved, err := r.resolveACPWorkspaceClass(context.Background(), task)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want substring %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveACPWorkspaceClass() error = %v", err)
			}
			if resolved == nil {
				t.Fatalf("resolved class is nil")
			}
			if tt.check != nil {
				tt.check(t, resolved)
			}
		})
	}
}

func TestResolveACPWorkspaceClassRequiresProviderAPI(t *testing.T) {
	t.Parallel()
	fixture := newACPClassFixture(t, acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox)
	r := acpClassTestReconciler(t, fixture.objects()...)
	r.WorkspaceProviderAPIEnabled = false
	_, err := r.resolveACPWorkspaceClass(context.Background(), acpClassTestTask())
	if err == nil || !strings.Contains(err.Error(), "requires the workspace provider API") {
		t.Fatalf("error = %v", err)
	}
}

func TestResolveACPClassWorkspaceBindingPolicy(t *testing.T) {
	t.Parallel()
	fixture := newACPClassFixture(t, acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox)
	r := acpClassTestReconciler(t, fixture.objects()...)
	resolved, err := r.resolveACPWorkspaceClass(context.Background(), acpClassTestTask())
	if err != nil {
		t.Fatalf("resolve class: %v", err)
	}

	t.Run("binding freezes class identity into the digest", func(t *testing.T) {
		task := acpClassTestTask()
		binding, err := resolveACPWorkspaceBindingWithClass(task, "", false, "", resolved)
		if err != nil {
			t.Fatalf("resolve binding: %v", err)
		}
		if binding.Class == nil || binding.Class.EffectiveOnDetach != string(workspacev1alpha1.WorkspaceOnDetachDelete) {
			t.Fatalf("class binding = %+v", binding.Class)
		}
		if binding.Provider != corev1alpha1.WorkspaceProviderAgentSandbox ||
			binding.CleanupPolicy != corev1alpha1.WorkspaceCleanupPolicyDelete {
			t.Fatalf("binding = %+v", binding)
		}
		if err := validateACPWorkspaceBindingValues(binding); err != nil {
			t.Fatalf("frozen binding validation: %v", err)
		}
		legacyTask := acpClassTestTask(func(task *corev1alpha1.Task) {
			task.Spec.Execution.Workspace = &corev1alpha1.ExecutionWorkspaceSpec{Enabled: true}
		})
		legacy, err := resolveACPWorkspaceBinding(legacyTask, corev1alpha1.WorkspaceProviderAgentSandbox, false, "")
		if err != nil {
			t.Fatalf("resolve legacy binding: %v", err)
		}
		if legacy.BindingDigest == binding.BindingDigest {
			t.Fatalf("class-backed binding digest must differ from the legacy digest")
		}
	})

	t.Run("requested detach action outside the class allowlist fails", func(t *testing.T) {
		task := acpClassTestTask(func(task *corev1alpha1.Task) {
			task.Spec.Execution.Workspace.OnDetach = corev1alpha1.WorkspaceOnDetachSuspend
		})
		_, err := resolveACPWorkspaceBindingWithClass(task, "", false, "", resolved)
		if err == nil || !strings.Contains(err.Error(), "is not allowed by class") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("suspend default fails closed until cold resume exists", func(t *testing.T) {
		suspendFixture := newACPClassFixture(t, acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox, func(f *acpClassFixture) {
			f.class.Spec.Lifecycle.DefaultOnDetach = workspacev1alpha1.WorkspaceOnDetachSuspend
			f.class.Spec.Lifecycle.AllowedOnDetach = []workspacev1alpha1.WorkspaceOnDetach{
				workspacev1alpha1.WorkspaceOnDetachSuspend, workspacev1alpha1.WorkspaceOnDetachDelete,
			}
		})
		suspendReconciler := acpClassTestReconciler(t, suspendFixture.objects()...)
		suspendResolved, err := suspendReconciler.resolveACPWorkspaceClass(context.Background(), acpClassTestTask())
		if err != nil {
			t.Fatalf("resolve class: %v", err)
		}
		if _, err := resolveACPWorkspaceBindingWithClass(acpClassTestTask(), "", false, "", suspendResolved); err == nil ||
			!strings.Contains(err.Error(), "not yet executable") {
			t.Fatalf("error = %v", err)
		}
		// The Task may still pick the executable Delete action explicitly.
		deleteTask := acpClassTestTask(func(task *corev1alpha1.Task) {
			task.Spec.Execution.Workspace.OnDetach = corev1alpha1.WorkspaceOnDetachDelete
		})
		if _, err := resolveACPWorkspaceBindingWithClass(deleteTask, "", false, "", suspendResolved); err != nil {
			t.Fatalf("explicit Delete action: %v", err)
		}
	})

	t.Run("reuse scope outside the class allowlist fails", func(t *testing.T) {
		noneOnly := newACPClassFixture(t, acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox, func(f *acpClassFixture) {
			f.class.Spec.AllowedReuseScopes = []workspacev1alpha1.WorkspaceReuseScope{workspacev1alpha1.WorkspaceReuseScopeNone}
		})
		noneReconciler := acpClassTestReconciler(t, noneOnly.objects()...)
		noneResolved, err := noneReconciler.resolveACPWorkspaceClass(context.Background(), acpClassTestTask())
		if err != nil {
			t.Fatalf("resolve class: %v", err)
		}
		task := acpClassTestTask(func(task *corev1alpha1.Task) {
			task.Spec.Execution.Workspace.ReusePolicy = corev1alpha1.WorkspaceReusePolicySession
			task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: acpTestSessionName, Create: true}
		})
		if _, err := resolveACPWorkspaceBindingWithClass(task, "", false, "session-uid-1", noneResolved); err == nil ||
			!strings.Contains(err.Error(), "not allowed by class") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestRejectUnsupportedACPWorkspacePlanClassGates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		name       string
		configure  func(*TaskReconciler)
		wantReject string
	}{
		{
			name:       "workspace provider API disabled",
			configure:  func(r *TaskReconciler) { r.WorkspaceProviderAPIEnabled = false },
			wantReject: "requires the workspace provider API",
		},
		{
			name:       "agent-sandbox backend disabled",
			configure:  func(r *TaskReconciler) { r.ACPWorkspaceDispatchEnabled = true },
			wantReject: "agent-sandbox is disabled",
		},
		{
			name:       "workspace dispatch disabled",
			configure:  func(r *TaskReconciler) { r.AgentSandboxEnabled = true },
			wantReject: "dispatch is disabled",
		},
		{
			name: "class-backed dispatch admitted",
			configure: func(r *TaskReconciler) {
				r.AgentSandboxEnabled = true
				r.ACPWorkspaceDispatchEnabled = true
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fixture := newACPClassFixture(t, acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox)
			r := acpClassTestReconciler(t, fixture.objects()...)
			tt.configure(r)
			plan, rejected := r.rejectUnsupportedACPWorkspacePlan(ctx, acpClassTestTask())
			if tt.wantReject == "" {
				if rejected {
					t.Fatalf("plan rejected: %+v", plan)
				}
				return
			}
			if !rejected || !strings.Contains(plan.rejectionReason, tt.wantReject) {
				t.Fatalf("plan = %+v, want rejection containing %q", plan, tt.wantReject)
			}
			if plan.workspaceStatusError == nil {
				t.Fatalf("class-shaped rejection must project a workspace validation failure")
			}
		})
	}
}

func TestACPWorkspaceClassBindingSnapshotRoundTrip(t *testing.T) {
	t.Parallel()
	fixture := newACPClassFixture(t, acpworkspacev1alpha1.RuntimeProviderBackendSubstrate)
	r := acpClassTestReconciler(t, fixture.objects()...)
	resolved, err := r.resolveACPWorkspaceClass(context.Background(), acpClassTestTask())
	if err != nil {
		t.Fatalf("resolve class: %v", err)
	}
	binding, err := resolveACPWorkspaceBindingWithClass(acpClassTestTask(), "", false, "", resolved)
	if err != nil {
		t.Fatalf("resolve binding: %v", err)
	}
	frozen := snapshotWorkspaceClassFromBinding(binding.Class)
	rebuilt := workspaceClassBindingFromSnapshot(frozen)
	if !reflect.DeepEqual(rebuilt, binding.Class) {
		t.Fatalf("snapshot round trip changed the class binding:\n%+v\n%+v", rebuilt, binding.Class)
	}
	restored := *binding
	restored.Class = rebuilt
	if err := validateACPWorkspaceBindingValues(&restored); err != nil {
		t.Fatalf("restored binding validation: %v", err)
	}

	tampered := *binding
	tamperedClass := *binding.Class
	tamperedClass.ProfileHash = "sha256:" + strings.Repeat("1", 64)
	tampered.Class = &tamperedClass
	if err := validateACPWorkspaceBindingValues(&tampered); err == nil {
		t.Fatalf("tampered class profile hash must fail canonical validation")
	}

	suspended := *binding
	suspendedClass := *binding.Class
	suspendedClass.EffectiveOnDetach = string(workspacev1alpha1.WorkspaceOnDetachSuspend)
	suspended.Class = &suspendedClass
	if err := validateACPWorkspaceBindingValues(&suspended); err == nil ||
		!strings.Contains(err.Error(), "not executable") {
		t.Fatalf("suspend detach action must stay rejected, got %v", err)
	}
}

func TestEnsureACPClassWorkspaceLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newACPClassFixture(t, acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox)
	task := acpClassTestTask()
	r := acpClassTestReconciler(t, append(fixture.objects(), task)...)
	resolved, err := r.resolveACPWorkspaceClass(ctx, task)
	if err != nil {
		t.Fatalf("resolve class: %v", err)
	}
	binding, err := resolveACPWorkspaceBindingWithClass(task, "", false, "", resolved)
	if err != nil {
		t.Fatalf("resolve binding: %v", err)
	}
	plan := ACPRuntimePlan{PoolName: "acp-ws-agent-sandbox-0123456789abcdef", Workspace: binding}

	name, ready, err := r.ensureACPClassWorkspace(ctx, task, plan)
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if ready || name != "" {
		t.Fatalf("workspace must not be ready before core admission")
	}
	workspaceName := acpClassWorkspaceName(task, binding)
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: workspaceName}, workspace); err != nil {
		t.Fatalf("created workspace: %v", err)
	}
	if workspace.Annotations[acpExecutionWorkspacePoolAnnotation] != plan.PoolName {
		t.Fatalf("workspace pool annotation = %q", workspace.Annotations[acpExecutionWorkspacePoolAnnotation])
	}
	owned := false
	for _, owner := range workspace.OwnerReferences {
		if owner.UID == task.UID {
			owned = true
			if owner.Controller != nil && *owner.Controller {
				t.Fatalf("per-Task workspace must not be controller-owned; the ACP projection owns Task status")
			}
		}
	}
	if !owned {
		t.Fatalf("per-Task workspace must carry a Task owner reference")
	}
	if workspace.Spec.ClassBinding.ProfileHash != binding.Class.ProfileHash ||
		workspace.Spec.Lifecycle.DefaultOnDetach != workspacev1alpha1.WorkspaceOnDetachDelete {
		t.Fatalf("workspace spec = %+v", workspace.Spec)
	}

	// Admit the workspace and let the adapter-observed status catch up; the
	// second ensure attaches and links the Task.
	admitTestACPWorkspace(t, r, workspace)
	name, ready, err = r.ensureACPClassWorkspace(ctx, task, plan)
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if !ready || name != workspaceName {
		t.Fatalf("ensure = (%q, %v), want attached workspace", name, ready)
	}
	linked := &corev1alpha1.Task{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, linked); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if linked.Labels[acpExecutionWorkspaceLinkLabel] != workspaceName {
		t.Fatalf("task link label = %q", linked.Labels[acpExecutionWorkspaceLinkLabel])
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: workspaceName}, workspace); err != nil {
		t.Fatalf("read attached workspace: %v", err)
	}
	if workspace.Spec.Attachment == nil || workspace.Spec.Attachment.TaskRef.UID != task.UID || workspace.Spec.Attachment.Epoch != 1 {
		t.Fatalf("attachment = %+v", workspace.Spec.Attachment)
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: workspace.Spec.Attachment.TokenSecretRef.Name}, secret); err != nil {
		t.Fatalf("attachment secret: %v", err)
	}

	// Idempotent re-ensure keeps the same attachment.
	if _, ready, err = r.ensureACPClassWorkspace(ctx, task, plan); err != nil || !ready {
		t.Fatalf("re-ensure = (%v, %v)", ready, err)
	}

}

// TestEnsureACPClassWorkspaceSessionContention proves attachment exclusivity
// on a genuinely shared workspace: two session-reused Tasks with the same
// immutable Session UID derive the same workspace name, so the competitor
// contends for the exact workspace the holder attached.
func TestEnsureACPClassWorkspaceSessionContention(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newACPClassFixture(t, acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox)
	sessionScoped := func(name, uid string) *corev1alpha1.Task {
		return acpClassTestTask(func(task *corev1alpha1.Task) {
			task.Name = name
			task.UID = types.UID(uid)
			task.Spec.Execution.Workspace.ReusePolicy = corev1alpha1.WorkspaceReusePolicySession
			task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: acpTestSessionName, Create: true}
		})
	}
	holder := sessionScoped("session-holder", "session-holder-uid")
	competitor := sessionScoped("session-competitor", "session-competitor-uid")
	r := acpClassTestReconciler(t, append(fixture.objects(), holder, competitor)...)
	resolved, err := r.resolveACPWorkspaceClass(ctx, holder)
	if err != nil {
		t.Fatalf("resolve class: %v", err)
	}
	holderBinding, err := resolveACPWorkspaceBindingWithClass(holder, "", false, "session-uid-1", resolved)
	if err != nil {
		t.Fatalf("resolve holder binding: %v", err)
	}
	competitorBinding, err := resolveACPWorkspaceBindingWithClass(competitor, "", false, "session-uid-1", resolved)
	if err != nil {
		t.Fatalf("resolve competitor binding: %v", err)
	}
	workspaceName := acpClassWorkspaceName(holder, holderBinding)
	if got := acpClassWorkspaceName(competitor, competitorBinding); got != workspaceName {
		t.Fatalf("session-reused Tasks must derive one workspace name, got %q and %q", workspaceName, got)
	}

	plan := ACPRuntimePlan{PoolName: "acp-ws-agent-sandbox-0123456789abcdef", Workspace: holderBinding}
	if _, _, err := r.ensureACPClassWorkspace(ctx, holder, plan); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: holder.Namespace, Name: workspaceName}, workspace); err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	admitTestACPWorkspace(t, r, workspace)
	if _, ready, err := r.ensureACPClassWorkspace(ctx, holder, plan); err != nil || !ready {
		t.Fatalf("holder attach = (%v, %v)", ready, err)
	}

	// The competitor resolves the same workspace but must not take the held
	// attachment: it either waits (not ready) or fails closed, never attaches.
	competitorPlan := ACPRuntimePlan{PoolName: plan.PoolName, Workspace: competitorBinding}
	if _, ready, err := r.ensureACPClassWorkspace(ctx, competitor, competitorPlan); err == nil && ready {
		t.Fatalf("competitor must not attach a held workspace")
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: holder.Namespace, Name: workspaceName}, workspace); err != nil {
		t.Fatalf("re-read workspace: %v", err)
	}
	if workspace.Spec.Attachment == nil || workspace.Spec.Attachment.TaskRef.UID != holder.UID {
		t.Fatalf("attachment = %+v, want held by %s", workspace.Spec.Attachment, holder.UID)
	}
}

func TestEnsureACPClassWorkspaceRejectsForeignAdoption(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newACPClassFixture(t, acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox)
	task := acpClassTestTask()
	r := acpClassTestReconciler(t, append(fixture.objects(), task)...)
	resolved, err := r.resolveACPWorkspaceClass(ctx, task)
	if err != nil {
		t.Fatalf("resolve class: %v", err)
	}
	binding, err := resolveACPWorkspaceBindingWithClass(task, "", false, "", resolved)
	if err != nil {
		t.Fatalf("resolve binding: %v", err)
	}
	name := acpClassWorkspaceName(task, binding)
	foreign := &workspacev1alpha1.ExecutionWorkspace{
		ObjectMeta: metav1.ObjectMeta{Namespace: task.Namespace, Name: name, UID: types.UID("foreign-uid")},
		Spec: workspacev1alpha1.ExecutionWorkspaceSpec{
			Mode: workspacev1alpha1.ExecutionWorkspaceModeInteractive,
			ClassBinding: workspacev1alpha1.ImmutableObjectBinding{
				Name: binding.Class.Name, UID: types.UID("other-class-uid"), Generation: 1, ProfileHash: binding.Class.ProfileHash,
			},
			ProviderBinding: workspacev1alpha1.ImmutableObjectBinding{
				Name: binding.Class.ProviderName, UID: types.UID(binding.Class.ProviderUID), Generation: 1,
			},
			Slot: binding.WorkspaceSlot, DesiredState: workspacev1alpha1.ExecutionWorkspaceDesiredReady,
			Lifecycle: fixture.class.Spec.Lifecycle,
		},
	}
	if err := r.Create(ctx, foreign); err != nil {
		t.Fatalf("create foreign workspace: %v", err)
	}
	if _, _, err := r.ensureACPClassWorkspace(ctx, task, ACPRuntimePlan{PoolName: "acp-ws-foreign-check", Workspace: binding}); err == nil ||
		!strings.Contains(err.Error(), "class binding does not match") {
		t.Fatalf("error = %v", err)
	}
}

// TestEnsureACPClassWorkspaceRejectsProviderIdentityDrift proves adoption is
// fail-closed against provider identity drift: a workspace whose recorded
// provider generation, provider config UID, or backend no longer matches the
// frozen binding is rejected instead of reused.
func TestEnsureACPClassWorkspaceRejectsProviderIdentityDrift(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(*workspacev1alpha1.ExecutionWorkspace)
		wantErr string
	}{
		{
			name: "provider generation drift",
			mutate: func(workspace *workspacev1alpha1.ExecutionWorkspace) {
				workspace.Spec.ProviderBinding.Generation = 7
			},
			wantErr: "provider binding does not match",
		},
		{
			name: "provider config UID drift",
			mutate: func(workspace *workspacev1alpha1.ExecutionWorkspace) {
				workspace.Annotations[acpWorkspaceProviderConfigUIDAnnotation] = "recreated-config-uid"
			},
			wantErr: "provider config or backend does not match",
		},
		{
			name: "backend drift",
			mutate: func(workspace *workspacev1alpha1.ExecutionWorkspace) {
				workspace.Annotations[acpWorkspaceBackendAnnotation] = string(acpworkspacev1alpha1.RuntimeProviderBackendSubstrate)
			},
			wantErr: "provider config or backend does not match",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fixture := newACPClassFixture(t, acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox)
			task := acpClassTestTask()
			r := acpClassTestReconciler(t, append(fixture.objects(), task)...)
			resolved, err := r.resolveACPWorkspaceClass(ctx, task)
			if err != nil {
				t.Fatalf("resolve class: %v", err)
			}
			binding, err := resolveACPWorkspaceBindingWithClass(task, "", false, "", resolved)
			if err != nil {
				t.Fatalf("resolve binding: %v", err)
			}
			plan := ACPRuntimePlan{PoolName: "acp-ws-agent-sandbox-0123456789abcdef", Workspace: binding}
			if _, _, err := r.ensureACPClassWorkspace(ctx, task, plan); err != nil {
				t.Fatalf("materialize: %v", err)
			}
			workspace := &workspacev1alpha1.ExecutionWorkspace{}
			workspaceName := acpClassWorkspaceName(task, binding)
			if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: workspaceName}, workspace); err != nil {
				t.Fatalf("read workspace: %v", err)
			}
			tc.mutate(workspace)
			if err := r.Update(ctx, workspace); err != nil {
				t.Fatalf("drift workspace: %v", err)
			}
			if _, _, err := r.ensureACPClassWorkspace(ctx, task, plan); err == nil ||
				!strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

// TestSettleACPClassWorkspaceSkipsForeignLinkTarget proves settlement
// revalidates the mutable link label: a workspace the Task neither owns nor
// shares a Session with is skipped, never revoked or deleted.
func TestSettleACPClassWorkspaceSkipsForeignLinkTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newACPClassFixture(t, acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox)
	foreign := &workspacev1alpha1.ExecutionWorkspace{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: acpTestNamespace, Name: "acp-ws-foreign", UID: types.UID("foreign-ws-uid"),
			Labels: map[string]string{workspacev1alpha1.ProviderControllerLabel: acpWorkspaceProviderControllerName},
		},
		Spec: workspacev1alpha1.ExecutionWorkspaceSpec{
			Mode:         workspacev1alpha1.ExecutionWorkspaceModeInteractive,
			DesiredState: workspacev1alpha1.ExecutionWorkspaceDesiredReady,
			Lifecycle:    fixture.class.Spec.Lifecycle,
		},
	}
	task := acpClassTestTask(func(task *corev1alpha1.Task) {
		task.Labels = map[string]string{acpExecutionWorkspaceLinkLabel: foreign.Name}
	})
	r := acpClassTestReconciler(t, append(fixture.objects(), task, foreign)...)

	done, err := r.settleACPClassWorkspace(ctx, task)
	if err != nil || !done {
		t.Fatalf("settle = (%v, %v), want (true, nil)", done, err)
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: acpTestNamespace, Name: foreign.Name}, foreign); err != nil {
		t.Fatalf("foreign workspace must survive settlement: %v", err)
	}
	if foreign.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady {
		t.Fatalf("foreign workspace desired state = %q", foreign.Spec.DesiredState)
	}

	// A link naming a workspace without the ACP controller label is equally
	// skipped even when the Task owns it.
	unlabeled := foreign.DeepCopy()
	unlabeled.ObjectMeta = metav1.ObjectMeta{
		Namespace: acpTestNamespace, Name: "acp-ws-unlabeled", UID: types.UID("unlabeled-ws-uid"),
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: corev1alpha1.GroupVersion.String(), Kind: taskResourceKind, Name: task.Name, UID: task.UID,
		}},
	}
	unlabeled.ResourceVersion = ""
	if err := r.Create(ctx, unlabeled); err != nil {
		t.Fatalf("create unlabeled workspace: %v", err)
	}
	task.Labels[acpExecutionWorkspaceLinkLabel] = unlabeled.Name
	if done, err := r.settleACPClassWorkspace(ctx, task); err != nil || !done {
		t.Fatalf("settle unlabeled = (%v, %v), want (true, nil)", done, err)
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: acpTestNamespace, Name: unlabeled.Name}, unlabeled); err != nil {
		t.Fatalf("unlabeled workspace must survive settlement: %v", err)
	}
}

// TestSettleACPClassWorkspaceQuarantinesPastDetachTimeout proves the frozen
// detachTimeout bounds settlement: when the adapter never releases the revoked
// epoch, the workspace is quarantined fail-closed and the Task releases.
func TestSettleACPClassWorkspaceQuarantinesPastDetachTimeout(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newACPClassFixture(t, acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox)
	task := acpClassTestTask()
	r := acpClassTestReconciler(t, append(fixture.objects(), task)...)
	resolved, err := r.resolveACPWorkspaceClass(ctx, task)
	if err != nil {
		t.Fatalf("resolve class: %v", err)
	}
	binding, err := resolveACPWorkspaceBindingWithClass(task, "", false, "", resolved)
	if err != nil {
		t.Fatalf("resolve binding: %v", err)
	}
	plan := ACPRuntimePlan{PoolName: "acp-ws-agent-sandbox-0123456789abcdef", Workspace: binding}
	if _, _, err := r.ensureACPClassWorkspace(ctx, task, plan); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	workspaceName := acpClassWorkspaceName(task, binding)
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: workspaceName}, workspace); err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	admitTestACPWorkspace(t, r, workspace)
	if _, ready, err := r.ensureACPClassWorkspace(ctx, task, plan); err != nil || !ready {
		t.Fatalf("attach = (%v, %v)", ready, err)
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, task); err != nil {
		t.Fatalf("reload task: %v", err)
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: workspaceName}, workspace); err != nil {
		t.Fatalf("read attached workspace: %v", err)
	}
	workspace.Status.AttachedEpoch = workspace.Spec.Attachment.Epoch
	if err := r.Status().Update(ctx, workspace); err != nil {
		t.Fatalf("enforce epoch: %v", err)
	}

	// First settle revokes and stamps the revocation start; the adapter still
	// enforces the epoch and the deadline has not passed, so it waits.
	done, err := r.settleACPClassWorkspace(ctx, task)
	if err != nil || done {
		t.Fatalf("settle while enforced = (%v, %v), want (false, nil)", done, err)
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: workspaceName}, workspace); err != nil {
		t.Fatalf("read revoked workspace: %v", err)
	}
	if workspace.Annotations[acpWorkspaceRevocationStartedAnnotation] == "" {
		t.Fatalf("revocation start must be stamped")
	}

	// Backdate the stamp past the frozen detachTimeout: settlement quarantines
	// the workspace and reports done so the Task finalizer releases.
	base := workspace.DeepCopy()
	expired := time.Now().UTC().Add(-workspace.Spec.Lifecycle.DetachTimeout.Duration - time.Minute)
	workspace.Annotations[acpWorkspaceRevocationStartedAnnotation] = expired.Format(time.RFC3339Nano)
	if err := r.Patch(ctx, workspace, client.MergeFrom(base)); err != nil {
		t.Fatalf("backdate revocation: %v", err)
	}
	done, err = r.settleACPClassWorkspace(ctx, task)
	if err != nil || !done {
		t.Fatalf("settle past deadline = (%v, %v), want (true, nil)", done, err)
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: workspaceName}, workspace); err != nil {
		t.Fatalf("quarantined workspace must survive: %v", err)
	}
	if workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined {
		t.Fatalf("desired state = %q, want Quarantined", workspace.Spec.DesiredState)
	}
}

func TestSettleACPClassWorkspaceRevokesAndDeletes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fixture := newACPClassFixture(t, acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox)
	task := acpClassTestTask()
	r := acpClassTestReconciler(t, append(fixture.objects(), task)...)
	resolved, err := r.resolveACPWorkspaceClass(ctx, task)
	if err != nil {
		t.Fatalf("resolve class: %v", err)
	}
	binding, err := resolveACPWorkspaceBindingWithClass(task, "", false, "", resolved)
	if err != nil {
		t.Fatalf("resolve binding: %v", err)
	}
	plan := ACPRuntimePlan{PoolName: "acp-ws-agent-sandbox-0123456789abcdef", Workspace: binding}
	if _, _, err := r.ensureACPClassWorkspace(ctx, task, plan); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	workspaceName := acpClassWorkspaceName(task, binding)
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: workspaceName}, workspace); err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	admitTestACPWorkspace(t, r, workspace)
	if _, ready, err := r.ensureACPClassWorkspace(ctx, task, plan); err != nil || !ready {
		t.Fatalf("attach = (%v, %v)", ready, err)
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: task.Name}, task); err != nil {
		t.Fatalf("reload task: %v", err)
	}

	// Simulate the adapter enforcing the epoch: settlement must not finalize
	// or delete while the data plane still reports the attachment.
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: workspaceName}, workspace); err != nil {
		t.Fatalf("read attached workspace: %v", err)
	}
	workspace.Status.AttachedEpoch = workspace.Spec.Attachment.Epoch
	if err := r.Status().Update(ctx, workspace); err != nil {
		t.Fatalf("enforce epoch: %v", err)
	}
	done, err := r.settleACPClassWorkspace(ctx, task)
	if err != nil {
		t.Fatalf("settle while enforced: %v", err)
	}
	if done {
		t.Fatalf("settlement must wait for the adapter to release the epoch")
	}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: workspaceName}, workspace); err != nil {
		t.Fatalf("workspace must still exist while the epoch is enforced: %v", err)
	}
	if workspace.Spec.Attachment != nil {
		t.Fatalf("revocation must clear attachment intent")
	}

	// Adapter releases the epoch; settlement finalizes and deletes.
	workspace.Status.AttachedEpoch = 0
	if err := r.Status().Update(ctx, workspace); err != nil {
		t.Fatalf("release epoch: %v", err)
	}
	done, err = r.settleACPClassWorkspace(ctx, task)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	if !done {
		t.Fatalf("settlement must complete after the epoch is released")
	}
	err = r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: workspaceName}, workspace)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("workspace must be deleted, got %v", err)
	}

	// Settlement is idempotent after deletion.
	if done, err := r.settleACPClassWorkspace(ctx, task); err != nil || !done {
		t.Fatalf("repeat settle = (%v, %v)", done, err)
	}
}
