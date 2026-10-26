package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestExternalACPInitialAttachmentPrecedesComputeDemand(t *testing.T) {
	ctx := context.Background()
	r, _, _, _ := externalACPFixture(t, true)
	r.Scheme = r.Client.Scheme()
	task := acpClassTestTask()
	if err := r.Create(ctx, task); err != nil {
		t.Fatal(err)
	}
	resolved, err := r.resolveACPWorkspaceClass(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := resolveACPWorkspaceBindingWithClass(task, "", resolved)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := r.createACPClassWorkspace(ctx, task, binding, "pool", acpClassWorkspaceName(task, binding))
	if err != nil {
		t.Fatal(err)
	}
	workspace.UID = "workspace-uid"
	workspace.Generation = 2
	workspace.CreationTimestamp = metav1.Now()
	markWorkspaceAdmittedForPolicyReview(workspace, 2)
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStatePending
	if err := r.Update(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	if WorkspaceReusable(workspace) {
		t.Fatal("generic selection admitted a pending workspace")
	}
	manager := WorkspaceAttachmentManager{Client: r.Client, APIReader: r.Client}
	if _, err := manager.Attach(ctx, workspace, task, nil); err == nil {
		t.Fatal("generic attachment admitted a pending workspace")
	}
	plan := ACPRuntimePlan{PoolName: "pool", Workspace: binding}
	_, ready, err := r.ensureACPClassWorkspace(ctx, task, plan)
	if err != nil || ready {
		t.Fatalf("preboot attachment = ready %v, error %v", ready, err)
	}
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(workspace), current); err != nil {
		t.Fatal(err)
	}
	if current.Spec.Attachment == nil || current.Spec.Attachment.TaskRef.UID != task.UID || current.Spec.Attachment.Epoch != 1 {
		t.Fatal("initial ACP attachment was not persisted")
	}
	if current.Spec.Workload != nil || current.Status.Allocation != nil {
		t.Fatal("attachment fabricated provider compute")
	}
	if err := r.Get(ctx, client.ObjectKey{Namespace: workspace.Namespace, Name: current.Spec.Attachment.TokenSecretRef.Name}, &corev1.Secret{}); err != nil {
		t.Fatal(err)
	}
}

func TestExternalACPPendingAttachmentFailsClosed(t *testing.T) {
	ctx := context.Background()
	r, _, _, _ := externalACPFixture(t, true)
	r.Scheme = r.Client.Scheme()
	task := acpClassTestTask()
	resolved, err := r.resolveACPWorkspaceClass(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := resolveACPWorkspaceBindingWithClass(task, "", resolved)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := r.createACPClassWorkspace(ctx, task, binding, "pool", "workspace")
	if err != nil {
		t.Fatal(err)
	}
	workspace.Generation = 2
	markWorkspaceAdmittedForPolicyReview(workspace, 2)
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStatePending
	for name, mutate := range map[string]func(*workspacev1alpha1.ExecutionWorkspace){
		"stale provider observation": func(w *workspacev1alpha1.ExecutionWorkspace) { w.Status.ObservedGeneration = 1 },
		"stale admission":            func(w *workspacev1alpha1.ExecutionWorkspace) { w.Status.Conditions[0].ObservedGeneration = 1 },
		"prior attachment":           func(w *workspacev1alpha1.ExecutionWorkspace) { w.Spec.AttachmentEpoch = 1 },
		"unobserved workload":        func(w *workspacev1alpha1.ExecutionWorkspace) { w.Spec.Workload = &workspacev1alpha1.WorkloadRequest{} },
		"unbound provider allocation": func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Status.Allocation = &workspacev1alpha1.AllocationObservation{}
		},
		"quarantined":        func(w *workspacev1alpha1.ExecutionWorkspace) { w.Labels[workspacev1alpha1.QuarantinedLabel] = "true" },
		"replaced class":     func(w *workspacev1alpha1.ExecutionWorkspace) { w.Spec.ClassBinding.UID = "replacement" },
		"foreign task owner": func(w *workspacev1alpha1.ExecutionWorkspace) { w.OwnerReferences = nil },
	} {
		t.Run(name, func(t *testing.T) {
			current := workspace.DeepCopy()
			mutate(current)
			if err := validateACPClassWorkspaceAttachmentAttempt(current, task, binding, "pool", time.Now()); err == nil {
				t.Fatal("unsafe preboot attachment admitted")
			}
		})
	}
}

func externalACPFixture(t *testing.T, approved bool) (*TaskReconciler, *workspacev1alpha1.ExecutionWorkspaceClass, *workspacev1alpha1.ExecutionWorkspaceProvider, *unstructured.Unstructured) {
	t.Helper()
	legacy := newACPClassFixture(t, "agent-sandbox")
	class, provider := legacy.class, legacy.provider
	provider.Spec.ControllerName = "example.workspace.orka.ai"
	provider.Spec.ParametersRef = workspacev1alpha1.TypedObjectReference{Group: "example.workspace.orka.ai", Kind: "ProviderConfig", Name: "config"}
	provider.Spec.ServiceAccountRef = &workspacev1alpha1.ProviderServiceAccountReference{Namespace: "provider-system", Name: "adapter"}
	provider.Status.SupportedContracts = []string{workspacev1alpha1.LifecycleContractV1}
	provider.Spec.RequiredContracts = []string{workspacev1alpha1.ContractVersionV1, workspacev1alpha1.LifecycleContractV1}
	provider.Status.SupportedFeatures = []workspacev1alpha1.ExecutionWorkspaceFeature{workspacev1alpha1.WorkspaceFeatureACPRuntime, workspacev1alpha1.WorkspaceFeatureSuspend}
	class.Spec.RequiredFeatures = []workspacev1alpha1.ExecutionWorkspaceFeature{workspacev1alpha1.WorkspaceFeatureACPRuntime}
	provider.Status.PinnedParametersUID = "config-uid"
	class.Spec.ParametersRef = &workspacev1alpha1.TypedObjectReference{Group: "example.workspace.orka.ai", Kind: "WorkspaceProfile", Name: "profile"}
	class.Spec.Lifecycle.AllowedOnDetach = []workspacev1alpha1.WorkspaceOnDetach{workspacev1alpha1.WorkspaceOnDetachDelete, workspacev1alpha1.WorkspaceOnDetachSuspend}
	class.Spec.Lifecycle.DefaultOnDetach = workspacev1alpha1.WorkspaceOnDetachDelete
	config := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "example.workspace.orka.ai/v1alpha1", "kind": "ProviderConfig", "spec": map[string]any{"endpoint": "https://control.example"}}}
	config.SetName("config")
	config.SetUID("config-uid")
	config.SetGeneration(1)
	profile := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "example.workspace.orka.ai/v1alpha1", "kind": "WorkspaceProfile", "spec": map[string]any{"suspend": map[string]any{"mode": "DataOnly"}}}}
	profile.SetNamespace(class.Namespace)
	profile.SetName("profile")
	profile.SetUID("profile-uid")
	profile.SetGeneration(1)
	hash, err := externalACPClassProfileHash(class, provider, config, profile)
	if err != nil {
		t.Fatal(err)
	}
	class.Status.ProfileHash = hash
	mapper := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: "example.workspace.orka.ai", Version: "v1alpha1"}})
	mapper.Add(config.GroupVersionKind(), apimeta.RESTScopeRoot)
	mapper.Add(profile.GroupVersionKind(), apimeta.RESTScopeNamespace)
	scheme := testACPWorkspaceScheme(t)
	if err := authorizationv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).WithStatusSubresource(class, provider).
		WithObjects(class, provider, config, profile).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if sar, ok := obj.(*authorizationv1.SubjectAccessReview); ok {
			want := &authorizationv1.ResourceAttributes{Group: "workspace.orka.ai", Resource: "executionworkspaceproviders", Name: provider.Name, Verb: "provider-status"}
			if !reflect.DeepEqual(sar.Spec.ResourceAttributes, want) || sar.Spec.User != "system:serviceaccount:provider-system:adapter" || len(sar.Spec.Groups) != 3 {
				t.Fatalf("unexpected provider authorization request: %#v", sar.Spec)
			}
			sar.Status.Allowed = approved
			return nil
		}
		return c.Create(ctx, obj, opts...)
	}}).Build()
	return &TaskReconciler{Client: c, APIReader: c, WorkspaceProviderAPIEnabled: true}, class, provider, config
}

func TestExternalACPClassResolutionAndSnapshotPins(t *testing.T) {
	r, class, _, _ := externalACPFixture(t, true)
	resolved, err := r.resolveACPWorkspaceClass(context.Background(), acpClassTestTask())
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Binding.ParametersBinding.UID != "profile-uid" || resolved.Binding.ProviderConfigBinding.UID != "config-uid" || resolved.Binding.ProfileHash != class.Status.ProfileHash {
		t.Fatalf("missing immutable pins: %#v", resolved.Binding)
	}
	binding, err := resolveACPWorkspaceBindingWithClass(acpClassTestTask(), "", resolved)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateACPWorkspaceBindingValues(binding); err != nil {
		t.Fatal(err)
	}
	restored := workspaceClassBindingFromSnapshot(snapshotWorkspaceClassFromBinding(binding.Class))
	if !reflect.DeepEqual(restored, binding.Class) {
		t.Fatalf("snapshot lost external binding: %#v", restored)
	}
	restored.ParametersBinding.ProfileHash = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	drift := *binding
	drift.Class = restored
	if err := validateACPWorkspaceBindingValues(&drift); err == nil {
		t.Fatal("mutated frozen profile pin was accepted")
	}
}

func TestExternalACPClassReadinessUsesOpaqueProfileAndProviderAuthority(t *testing.T) {
	for _, approved := range []bool{false, true} {
		r, class, _, _ := externalACPFixture(t, approved)
		reconciler := &ExecutionWorkspaceClassReconciler{Client: r.Client, APIReader: r.APIReader, RESTMapper: r.RESTMapper()}
		_, reason, _, err := reconciler.resolveClassProvider(context.Background(), class)
		if err != nil {
			t.Fatal(err)
		}
		if (reason == string(workspacev1alpha1.ReasonReady)) != approved {
			t.Fatalf("class readiness reason = %s, provider approved %v", reason, approved)
		}
		if approved {
			hash, err := reconciler.resolvedClassProfileHash(context.Background(), class)
			if err != nil {
				t.Fatal(err)
			}
			if hash != class.Status.ProfileHash {
				t.Fatalf("class controller hash = %s, Task hash = %s", hash, class.Status.ProfileHash)
			}
		}
	}
}

func TestExternalACPClassResolutionRejectsUnapprovedAndIncompatibleProviders(t *testing.T) {
	for _, test := range []struct {
		name     string
		approved bool
		mutate   func(*workspacev1alpha1.ExecutionWorkspaceProvider, *unstructured.Unstructured)
	}{
		{name: "unapproved ServiceAccount"},
		{name: "missing lifecycle", approved: true, mutate: func(p *workspacev1alpha1.ExecutionWorkspaceProvider, _ *unstructured.Unstructured) {
			p.Status.SupportedContracts = nil
		}},
		{name: "missing ACP workload capability", approved: true, mutate: func(p *workspacev1alpha1.ExecutionWorkspaceProvider, _ *unstructured.Unstructured) {
			p.Status.SupportedFeatures = nil
		}},
		{name: "replaced config", approved: true, mutate: func(_ *workspacev1alpha1.ExecutionWorkspaceProvider, c *unstructured.Unstructured) {
			c.SetUID("replacement")
		}},
		{name: "changed config spec", approved: true, mutate: func(_ *workspacev1alpha1.ExecutionWorkspaceProvider, c *unstructured.Unstructured) {
			c.Object["spec"] = map[string]any{"endpoint": "https://replacement.example"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, _, p, c := externalACPFixture(t, test.approved)
			if test.mutate != nil {
				test.mutate(p, c)
				if err := r.Client.Status().Update(context.Background(), p); err != nil {
					t.Fatal(err)
				}
				if err := r.Update(context.Background(), c); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := r.resolveACPWorkspaceClass(context.Background(), acpClassTestTask()); err == nil {
				t.Fatal("invalid provider was accepted")
			}
		})
	}
}

func TestExternalACPDrainingContinuationIncludesColdResume(t *testing.T) {
	for _, test := range []struct {
		name       string
		existing   bool
		state      workspacev1alpha1.ExecutionWorkspaceState
		foreignUID bool
		disabled   bool
		outage     bool
		wantOK     bool
	}{
		{name: "new identity", state: workspacev1alpha1.ExecutionWorkspaceStateReady},
		{name: "running continuation", existing: true, state: workspacev1alpha1.ExecutionWorkspaceStateReady, wantOK: true},
		{name: "cold resume", existing: true, state: workspacev1alpha1.ExecutionWorkspaceStateSuspended, wantOK: true},
		{name: "wrong provider installation", existing: true, state: workspacev1alpha1.ExecutionWorkspaceStateSuspended, foreignUID: true},
		{name: "disabled continuation", existing: true, state: workspacev1alpha1.ExecutionWorkspaceStateReady, disabled: true},
		{name: "adapter outage", existing: true, state: workspacev1alpha1.ExecutionWorkspaceStateSuspended, outage: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			r, class, provider, _ := externalACPFixture(t, true)
			task := acpClassTestTask(func(task *corev1alpha1.Task) {
				task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: acpTestSessionName}
				task.Spec.Execution.Workspace.ReusePolicy = corev1alpha1.WorkspaceReusePolicySession
			})
			provider.Spec.LifecycleState = workspacev1alpha1.ExecutionWorkspaceProviderDraining
			if test.disabled {
				provider.Spec.LifecycleState = workspacev1alpha1.ExecutionWorkspaceProviderDisabled
			}
			if err := r.Update(ctx, provider); err != nil {
				t.Fatal(err)
			}
			provider.Status.Conditions = []metav1.Condition{
				{Type: string(workspacev1alpha1.ConditionProviderReady), Status: metav1.ConditionFalse, Reason: string(workspacev1alpha1.ReasonProviderDraining), ObservedGeneration: provider.Generation},
				{Type: string(workspacev1alpha1.ConditionProviderHeartbeat), Status: metav1.ConditionTrue, Reason: "Ready", ObservedGeneration: provider.Generation},
				{Type: string(workspacev1alpha1.ConditionProviderCompatible), Status: metav1.ConditionTrue, Reason: "Ready", ObservedGeneration: provider.Generation},
			}
			if test.outage {
				provider.Status.Conditions[1].Status = metav1.ConditionFalse
			}
			if err := r.Client.Status().Update(ctx, provider); err != nil {
				t.Fatal(err)
			}
			class.Status.Conditions[0].Status = metav1.ConditionFalse
			class.Status.Conditions[0].Reason = string(workspacev1alpha1.ReasonProviderDraining)
			if err := r.Client.Status().Update(ctx, class); err != nil {
				t.Fatal(err)
			}
			if test.existing {
				probe := &ACPRuntimeWorkspaceBinding{ReusePolicy: corev1alpha1.WorkspaceReusePolicySession, SessionUID: "session-uid", WorkspaceSlot: defaultWorkspaceSlotName, Class: &ACPWorkspaceClassBinding{UID: string(class.UID)}}
				workspace := &workspacev1alpha1.ExecutionWorkspace{ObjectMeta: metav1.ObjectMeta{Namespace: class.Namespace, Name: acpClassWorkspaceName(task, probe), UID: "workspace-uid", Generation: 1,
					Labels: map[string]string{workspacev1alpha1.ProviderControllerLabel: provider.Spec.ControllerName}, Annotations: map[string]string{acpExecutionWorkspacePoolAnnotation: "pool"}},
					Spec: workspacev1alpha1.ExecutionWorkspaceSpec{Mode: class.Spec.Mode, ClassBinding: workspacev1alpha1.ImmutableObjectBinding{Name: class.Name, UID: class.UID, Generation: class.Generation, ProfileHash: class.Status.ProfileHash},
						ProviderBinding: workspacev1alpha1.ImmutableObjectBinding{Name: provider.Name, UID: provider.UID, Generation: provider.Generation}, SessionRef: &workspacev1alpha1.ObjectIdentityReference{Name: acpTestSessionName, UID: "session-uid"}, Slot: defaultWorkspaceSlotName, DesiredState: workspacev1alpha1.ExecutionWorkspaceDesiredReady}, Status: workspacev1alpha1.ExecutionWorkspaceStatus{State: test.state}}
				if test.foreignUID {
					workspace.Spec.ProviderBinding.UID = "foreign"
				}
				workspace.Spec.CoreAdmission = &workspacev1alpha1.ExecutionWorkspaceCoreAdmission{ClassBinding: workspace.Spec.ClassBinding, ProviderBinding: workspace.Spec.ProviderBinding, AdmittedGeneration: 1}
				pool := &corev1alpha1.RuntimePool{ObjectMeta: metav1.ObjectMeta{Namespace: class.Namespace, Name: "pool", UID: types.UID("pool-uid")}, Spec: corev1alpha1.RuntimePoolSpec{ExecutionWorkspace: &corev1alpha1.RuntimePoolExecutionWorkspaceSpec{
					Provider: corev1alpha1.WorkspaceProvider(provider.Spec.ControllerName), BindingDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", WorkspaceRef: &workspacev1alpha1.ObjectIdentityReference{Name: workspace.Name, UID: workspace.UID}}}}
				if err := r.Create(ctx, workspace); err != nil {
					t.Fatal(err)
				}
				if err := r.Create(ctx, pool); err != nil {
					t.Fatal(err)
				}
			}
			_, err := r.resolveACPWorkspaceClassWithSessionUID(ctx, task, "session-uid")
			if (err == nil) != test.wantOK {
				t.Fatalf("resolve error = %v, want success %v", err, test.wantOK)
			}
		})
	}
}

func TestExternalACPPoolPlanRequiresExactContractAndParameters(t *testing.T) {
	r, _, _, _ := externalACPFixture(t, true)
	resolved, err := r.resolveACPWorkspaceClass(context.Background(), acpClassTestTask())
	if err != nil {
		t.Fatal(err)
	}
	binding, err := resolveACPWorkspaceBindingWithClass(acpClassTestTask(), "", resolved)
	if err != nil {
		t.Fatal(err)
	}
	plan := ACPRuntimePlan{Workspace: binding}
	for _, test := range []struct {
		name   string
		mutate func(*corev1alpha1.RuntimePoolExecutionWorkspaceSpec)
		want   bool
	}{
		{name: "exact binding", want: true},
		{name: "missing workload", mutate: func(s *corev1alpha1.RuntimePoolExecutionWorkspaceSpec) { s.Workload = nil }},
		{name: "different lifecycle", mutate: func(s *corev1alpha1.RuntimePoolExecutionWorkspaceSpec) { s.Workload.ContractVersion = "future" }},
		{name: "different protocol", mutate: func(s *corev1alpha1.RuntimePoolExecutionWorkspaceSpec) { s.Workload.ProtocolVersion = "future" }},
		{name: "replaced profile", mutate: func(s *corev1alpha1.RuntimePoolExecutionWorkspaceSpec) { s.ParametersBinding.UID = "replacement" }},
		{name: "unrequested checkpoint", mutate: func(s *corev1alpha1.RuntimePoolExecutionWorkspaceSpec) {
			s.RestoreFrom = &workspacev1alpha1.WorkloadCheckpointReference{Name: "saved-data", UID: "checkpoint-uid", Digest: "sha256:" + strings.Repeat("a", 64)}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := &corev1alpha1.RuntimePoolExecutionWorkspaceSpec{Provider: binding.Provider, BindingDigest: binding.BindingDigest,
				WorkspaceRef: &workspacev1alpha1.ObjectIdentityReference{Name: "workspace", UID: "workspace-uid"}, ParametersRef: binding.Class.ParametersRef.DeepCopy(), ParametersBinding: binding.Class.ParametersBinding.DeepCopy(),
				Workload: &corev1alpha1.RuntimePoolWorkspaceWorkloadSpec{ContractVersion: workspacev1alpha1.LifecycleContractV1, ProtocolVersion: corev1alpha1.RuntimePoolProtocolHarnessV2}}
			if test.mutate != nil {
				test.mutate(spec)
			}
			pool := &corev1alpha1.RuntimePool{Spec: corev1alpha1.RuntimePoolSpec{ExecutionWorkspace: spec}}
			if got := acpRuntimePoolWorkspaceMatchesPlan(pool, plan); got != test.want {
				t.Fatalf("pool plan matches = %v, want %v", got, test.want)
			}
		})
	}
}

func TestExternalACPCheckpointRestoreRequiresBothProviderCapabilities(t *testing.T) {
	for _, missing := range []workspacev1alpha1.ExecutionWorkspaceFeature{"", workspacev1alpha1.WorkspaceFeatureCheckpoint, workspacev1alpha1.WorkspaceFeatureRestore} {
		t.Run("missing_"+string(missing), func(t *testing.T) {
			r, _, provider, _ := externalACPFixture(t, true)
			for _, feature := range []workspacev1alpha1.ExecutionWorkspaceFeature{workspacev1alpha1.WorkspaceFeatureCheckpoint, workspacev1alpha1.WorkspaceFeatureRestore} {
				if feature != missing {
					provider.Status.SupportedFeatures = append(provider.Status.SupportedFeatures, feature)
				}
			}
			if err := r.Status().Update(context.Background(), provider); err != nil {
				t.Fatal(err)
			}
			task := acpClassTestTask()
			task.Spec.Execution.Workspace.RestoreFrom = &corev1alpha1.WorkspaceCheckpointReference{Name: "saved-data", UID: "checkpoint-uid", Digest: "sha256:" + strings.Repeat("a", 64)}
			resolved, err := r.resolveACPWorkspaceClass(context.Background(), task)
			if missing != "" {
				if err == nil {
					t.Fatalf("restore accepted without %s", missing)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			binding, err := resolveACPWorkspaceBindingWithClass(task, "", resolved)
			if err != nil {
				t.Fatal(err)
			}
			if binding.RestoreFrom == nil || binding.RestoreFrom.UID != "checkpoint-uid" {
				t.Fatal("external class resolution lost the frozen checkpoint selection")
			}
		})
	}
}

func TestExternalACPCheckpointRestoreRequiresCurrentReadyExactClass(t *testing.T) {
	for _, test := range []struct {
		name     string
		mutate   func(*workspacev1alpha1.ExecutionWorkspaceCheckpoint)
		missing  bool
		deleting bool
		want     bool
	}{
		{name: "retained checkpoint after source deletion", want: true},
		{name: "missing checkpoint", missing: true},
		{name: "replaced UID", mutate: func(c *workspacev1alpha1.ExecutionWorkspaceCheckpoint) { c.UID = "replacement" }},
		{name: "different digest", mutate: func(c *workspacev1alpha1.ExecutionWorkspaceCheckpoint) {
			c.Status.Digest = "sha256:" + strings.Repeat("b", 64)
		}},
		{name: "pending phase", mutate: func(c *workspacev1alpha1.ExecutionWorkspaceCheckpoint) { c.Status.Phase = "Pending" }},
		{name: "missing readiness", mutate: func(c *workspacev1alpha1.ExecutionWorkspaceCheckpoint) { c.Status.Conditions = nil }},
		{name: "withdrawn readiness", mutate: func(c *workspacev1alpha1.ExecutionWorkspaceCheckpoint) {
			c.Status.Conditions[0].Status = metav1.ConditionFalse
		}},
		{name: "stale readiness", mutate: func(c *workspacev1alpha1.ExecutionWorkspaceCheckpoint) { c.Status.Conditions[0].ObservedGeneration = 0 }},
		{name: "missing class provenance", mutate: func(c *workspacev1alpha1.ExecutionWorkspaceCheckpoint) { c.Status.ClassBinding = nil }},
		{name: "replaced class", mutate: func(c *workspacev1alpha1.ExecutionWorkspaceCheckpoint) { c.Status.ClassBinding.UID = "replacement" }},
		{name: "different class revision", mutate: func(c *workspacev1alpha1.ExecutionWorkspaceCheckpoint) {
			c.Status.ClassBinding.ProfileHash = "sha256:" + strings.Repeat("b", 64)
		}},
		{name: "deleting checkpoint", deleting: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, class, _, _ := externalACPFixture(t, true)
			task := acpClassTestTask()
			resolved, err := r.resolveACPWorkspaceClass(context.Background(), task)
			if err != nil {
				t.Fatal(err)
			}
			binding, err := resolveACPWorkspaceBindingWithClass(task, "", resolved)
			if err != nil {
				t.Fatal(err)
			}
			binding.RestoreFrom = &corev1alpha1.WorkspaceCheckpointReference{Name: "saved-data", UID: "checkpoint-uid", Digest: "sha256:" + strings.Repeat("a", 64)}
			checkpoint := &workspacev1alpha1.ExecutionWorkspaceCheckpoint{ObjectMeta: metav1.ObjectMeta{Namespace: task.Namespace, Name: "saved-data", UID: "checkpoint-uid", Generation: 1},
				Spec: workspacev1alpha1.ExecutionWorkspaceCheckpointSpec{WorkspaceRef: workspacev1alpha1.ObjectIdentityReference{Name: "deleted-source", UID: "deleted-source-uid"}},
				Status: workspacev1alpha1.ExecutionWorkspaceCheckpointStatus{Phase: "Ready", Digest: binding.RestoreFrom.Digest,
					ClassBinding: &workspacev1alpha1.ImmutableObjectBinding{Name: class.Name, UID: class.UID, Generation: class.Generation, ProfileHash: class.Status.ProfileHash},
					Conditions:   []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, ObservedGeneration: 1}}}}
			if test.mutate != nil {
				test.mutate(checkpoint)
			}
			if !test.missing {
				if test.deleting {
					checkpoint.Finalizers = []string{"example.workspace.orka.ai/retained-data"}
				}
				if err := r.Create(context.Background(), checkpoint); err != nil {
					t.Fatal(err)
				}
				if test.deleting {
					if err := r.Delete(context.Background(), checkpoint); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := r.validateACPWorkspaceRestoreCheckpoint(context.Background(), task, binding); (err == nil) != test.want {
				t.Fatalf("checkpoint admission error = %v, want accepted %v", err, test.want)
			}
		})
	}
}

func TestWorkspaceClassRequiredFeaturesUsesExplicitACPContract(t *testing.T) {
	for _, test := range []struct {
		name     string
		explicit []workspacev1alpha1.ExecutionWorkspaceFeature
		suspend  bool
		want     []workspacev1alpha1.ExecutionWorkspaceFeature
	}{
		{name: "generic interactive", want: []workspacev1alpha1.ExecutionWorkspaceFeature{workspacev1alpha1.WorkspaceFeatureTLS, workspacev1alpha1.WorkspaceFeatureExec, workspacev1alpha1.WorkspaceFeatureReset}},
		{name: "ACP interactive", explicit: []workspacev1alpha1.ExecutionWorkspaceFeature{workspacev1alpha1.WorkspaceFeatureACPRuntime}, want: []workspacev1alpha1.ExecutionWorkspaceFeature{workspacev1alpha1.WorkspaceFeatureACPRuntime}},
		{name: "ACP suspension", explicit: []workspacev1alpha1.ExecutionWorkspaceFeature{workspacev1alpha1.WorkspaceFeatureACPRuntime}, suspend: true, want: []workspacev1alpha1.ExecutionWorkspaceFeature{workspacev1alpha1.WorkspaceFeatureACPRuntime, workspacev1alpha1.WorkspaceFeatureSuspend}},
		{name: "explicit generic capability retained", explicit: []workspacev1alpha1.ExecutionWorkspaceFeature{workspacev1alpha1.WorkspaceFeatureACPRuntime, workspacev1alpha1.WorkspaceFeatureExec, workspacev1alpha1.WorkspaceFeatureTLS}, want: []workspacev1alpha1.ExecutionWorkspaceFeature{workspacev1alpha1.WorkspaceFeatureACPRuntime, workspacev1alpha1.WorkspaceFeatureExec, workspacev1alpha1.WorkspaceFeatureTLS}},
	} {
		t.Run(test.name, func(t *testing.T) {
			class := &workspacev1alpha1.ExecutionWorkspaceClass{Spec: workspacev1alpha1.ExecutionWorkspaceClassSpec{Mode: workspacev1alpha1.ExecutionWorkspaceModeInteractive, RequiredFeatures: test.explicit}}
			if test.suspend {
				class.Spec.Lifecycle.AllowedOnDetach = []workspacev1alpha1.WorkspaceOnDetach{workspacev1alpha1.WorkspaceOnDetachSuspend}
			}
			if got := executionWorkspaceClassRequiredFeatures(class); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("required features = %v, want %v", got, test.want)
			}
		})
	}
}

func TestExternalACPPoolPlanPinsExactCheckpoint(t *testing.T) {
	r, _, _, _ := externalACPFixture(t, true)
	task := acpClassTestTask()
	resolved, err := r.resolveACPWorkspaceClass(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := resolveACPWorkspaceBindingWithClass(task, "", resolved)
	if err != nil {
		t.Fatal(err)
	}
	binding.RestoreFrom = &corev1alpha1.WorkspaceCheckpointReference{Name: "saved-data", UID: "checkpoint-uid", Digest: "sha256:" + strings.Repeat("a", 64)}
	for _, test := range []struct {
		name   string
		mutate func(*corev1alpha1.RuntimePoolExecutionWorkspaceSpec)
		want   bool
	}{
		{name: "exact checkpoint", want: true},
		{name: "removed checkpoint", mutate: func(s *corev1alpha1.RuntimePoolExecutionWorkspaceSpec) { s.RestoreFrom = nil }},
		{name: "different name", mutate: func(s *corev1alpha1.RuntimePoolExecutionWorkspaceSpec) { s.RestoreFrom.Name = "another" }},
		{name: "different UID", mutate: func(s *corev1alpha1.RuntimePoolExecutionWorkspaceSpec) { s.RestoreFrom.UID = "replacement" }},
		{name: "different digest", mutate: func(s *corev1alpha1.RuntimePoolExecutionWorkspaceSpec) {
			s.RestoreFrom.Digest = "sha256:" + strings.Repeat("b", 64)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := &corev1alpha1.RuntimePoolExecutionWorkspaceSpec{Provider: binding.Provider, BindingDigest: binding.BindingDigest,
				WorkspaceRef: &workspacev1alpha1.ObjectIdentityReference{Name: "workspace", UID: "workspace-uid"}, ParametersRef: binding.Class.ParametersRef.DeepCopy(), ParametersBinding: binding.Class.ParametersBinding.DeepCopy(),
				Workload: &corev1alpha1.RuntimePoolWorkspaceWorkloadSpec{ContractVersion: workspacev1alpha1.LifecycleContractV1, ProtocolVersion: corev1alpha1.RuntimePoolProtocolHarnessV2}, RestoreFrom: acpWorkspaceWorkloadCheckpointReference(binding.RestoreFrom)}
			if test.mutate != nil {
				test.mutate(spec)
			}
			pool := &corev1alpha1.RuntimePool{Spec: corev1alpha1.RuntimePoolSpec{ExecutionWorkspace: spec}}
			if got := acpRuntimePoolWorkspaceMatchesPlan(pool, ACPRuntimePlan{Workspace: binding}); got != test.want {
				t.Fatalf("checkpoint pool plan matches = %v, want %v", got, test.want)
			}
		})
	}
}
