package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestWorkspaceRuntimePoolReconcilerDemandAndRetirementPreserveProviderStatus(t *testing.T) {
	for _, test := range []struct {
		name         string
		desired      workspacev1alpha1.ExecutionWorkspaceDesiredState
		attached     bool
		expired      bool
		wantReplicas int32
		wantDeleting bool
	}{
		{name: "attached ready workspace", desired: workspacev1alpha1.ExecutionWorkspaceDesiredReady, attached: true, wantReplicas: 1},
		{name: "detached ready workspace", desired: workspacev1alpha1.ExecutionWorkspaceDesiredReady},
		{name: "suspending workspace", desired: workspacev1alpha1.ExecutionWorkspaceDesiredSuspended, attached: true},
		{name: "workspace deletion", desired: workspacev1alpha1.ExecutionWorkspaceDesiredDeleted, wantReplicas: 1, wantDeleting: true},
		{name: "expiry belongs to retention", desired: workspacev1alpha1.ExecutionWorkspaceDesiredReady, attached: true, expired: true, wantReplicas: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			f := newExternalRuntimePoolFixture(t)
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			pool.Finalizers = []string{runtimePoolFinalizer}
			if err := f.r.Update(ctx, &pool); err != nil {
				t.Fatal(err)
			}
			workspace := f.currentWorkspace(t)
			workspace.Spec.DesiredState = test.desired
			if test.attached {
				workspace.Spec.Attachment = &workspacev1alpha1.ExecutionWorkspaceAttachment{Epoch: 1, TaskRef: workspacev1alpha1.ObjectIdentityReference{Name: "task", UID: "task-uid"}}
			}
			if test.expired {
				workspace.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * time.Hour))
				workspace.Spec.Lifecycle.MaxLifetime = &metav1.Duration{Duration: time.Hour}
			}
			if err := f.r.Update(ctx, workspace); err != nil {
				t.Fatal(err)
			}
			originalStatus := workspace.Status.DeepCopy()
			reconciler := &WorkspaceRuntimePoolReconciler{Client: f.r.Client, APIReader: f.r.Client}
			if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}); err != nil {
				t.Fatal(err)
			}
			pool = runtimePoolTestGetPool(t, f.r, f.pool)
			if pool.Spec.DesiredReplicas != test.wantReplicas || !pool.DeletionTimestamp.IsZero() != test.wantDeleting {
				t.Fatalf("pool intent = replicas %d deleting %v, want %d/%v", pool.Spec.DesiredReplicas, !pool.DeletionTimestamp.IsZero(), test.wantReplicas, test.wantDeleting)
			}
			current := f.currentWorkspace(t)
			if !reflect.DeepEqual(&current.Status, originalStatus) {
				t.Fatal("core demand controller changed provider-owned workspace status")
			}
			if test.expired && (current.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady || current.Spec.Attachment == nil || !current.DeletionTimestamp.IsZero()) {
				t.Fatal("pool demand reconciliation took ownership of workspace lifetime expiry")
			}
		})
	}
}

func TestWorkspaceRuntimePoolReconcilerRejectsReplacedWorkspaceLink(t *testing.T) {
	ctx := context.Background()
	f := newExternalRuntimePoolFixture(t)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	pool.Spec.ExecutionWorkspace.WorkspaceRef.UID = "foreign-workspace-uid"
	if err := f.r.Update(ctx, &pool); err != nil {
		t.Fatal(err)
	}
	reconciler := &WorkspaceRuntimePoolReconciler{Client: f.r.Client, APIReader: f.r.Client}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.workspace)}); err == nil {
		t.Fatal("foreign workspace link was accepted")
	}
	got := runtimePoolTestGetPool(t, f.r, f.pool)
	if got.Spec.DesiredReplicas != 1 || !got.DeletionTimestamp.IsZero() {
		t.Fatal("rejected foreign link changed runtime pool demand")
	}
}

func TestValidateExternalWorkspaceUpgradeNamesLegacyAllocationsAndRetainedWorkspaces(t *testing.T) {
	ctx := context.Background()
	scheme := runtimePoolTestScheme(t)
	if err := workspacev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	legacyPool := runtimePoolTestObject(0)
	legacyPool.Namespace = "a"
	legacyPool.Name = "legacy-pool"
	legacyPool.Spec.ExecutionWorkspace = &corev1alpha1.RuntimePoolExecutionWorkspaceSpec{Provider: corev1alpha1.WorkspaceProviderAgentSandbox}
	retained := &workspacev1alpha1.ExecutionWorkspace{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "retained-workspace", UID: "retained-uid", Labels: map[string]string{workspacev1alpha1.ProviderControllerLabel: acpWorkspaceControllerLabelValue}}, Status: workspacev1alpha1.ExecutionWorkspaceStatus{State: workspacev1alpha1.ExecutionWorkspaceStateSuspended}}
	resumed := retained.DeepCopy()
	resumed.Namespace = "c"
	resumed.Name = "resumed-workspace"
	resumed.UID = "resumed-uid"
	resumed.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
	resumed.Annotations = map[string]string{acpWorkspaceResumedLineageAnnotation: booleanTrueValue}
	generic := retained.DeepCopy()
	generic.Name = "generic-suspended"
	generic.UID = "generic-uid"
	generic.Labels[workspacev1alpha1.ProviderControllerLabel] = "example.workspace.orka.ai"
	ordinary := retained.DeepCopy()
	ordinary.Name = "legacy-empty"
	ordinary.UID = "empty-uid"
	ordinary.Status.State = workspacev1alpha1.ExecutionWorkspaceStateReady
	genericPool := runtimePoolTestObject(0)
	genericPool.Namespace = "d"
	genericPool.Name = "generic-pool"
	genericPool.UID = types.UID("generic-pool-uid")
	genericPool.Spec.ExecutionWorkspace = &corev1alpha1.RuntimePoolExecutionWorkspaceSpec{Provider: "example.workspace.orka.ai", WorkspaceRef: &workspacev1alpha1.ObjectIdentityReference{Name: generic.Name, UID: generic.UID}, Workload: &corev1alpha1.RuntimePoolWorkspaceWorkloadSpec{ContractVersion: workspacev1alpha1.LifecycleContractV1, ProtocolVersion: corev1alpha1.RuntimePoolProtocolHarnessV2}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(legacyPool, retained, resumed, generic, ordinary, genericPool).Build()
	err := ValidateExternalWorkspaceUpgrade(ctx, c)
	if err == nil {
		t.Fatal("legacy allocations did not block external workspace upgrade")
	}
	for _, name := range []string{"RuntimePool a/legacy-pool", "legacy retained ExecutionWorkspace b/retained-workspace", "legacy retained ExecutionWorkspace c/resumed-workspace", "legacy retained ExecutionWorkspace b/legacy-empty"} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("upgrade error did not name %q: %v", name, err)
		}
	}
	for _, name := range []string{"generic-suspended", "generic-pool"} {
		if strings.Contains(err.Error(), name) {
			t.Fatalf("upgrade gate blocked compatible or empty object %q", name)
		}
	}
	for _, object := range []client.Object{legacyPool, retained, resumed, ordinary} {
		if err := c.Delete(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	if err := ValidateExternalWorkspaceUpgrade(ctx, c); err != nil {
		t.Fatalf("upgrade remains blocked after legacy retirement: %v", err)
	}
}

func TestExternalWorkspaceDispatchGateAllowsPlainPoolsAndRejectsLegacyWorkspacePools(t *testing.T) {
	for _, workspaceBacked := range []bool{false, true} {
		name := "plain runtime pool"
		if workspaceBacked {
			name = "legacy workspace pool"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			task := bindingTestTask()
			if workspaceBacked {
				task = workspaceBindingTestTask(nil)
			}
			r, _ := newBindingTestReconciler(t, task, bindingTestNamespace())
			r.WorkspaceProviderAPIEnabled = true
			r.ACPWorkspaceDispatchEnabled = true
			plan, err := PlanACPRuntime(task, bindingTestAgent(), r.ACPRuntimeImages)
			if err != nil {
				t.Fatal(err)
			}
			if workspaceBacked {
				binding, err := resolveTestACPWorkspaceBinding(t, task, "")
				if err != nil {
					t.Fatal(err)
				}
				plan, err = applyACPWorkspaceBindingToPlan(plan, binding)
				plan.Workspace.Class.ControllerName = ""
				if err != nil {
					t.Fatal(err)
				}
			}
			pool, existing, err := r.ensureACPRuntimePool(ctx, task.Namespace, plan)
			if workspaceBacked {
				if err == nil || !strings.Contains(err.Error(), "legacy bindings are cleanup-only") {
					t.Fatalf("legacy dispatch error = %v, want migration gate", err)
				}
				pools := &corev1alpha1.RuntimePoolList{}
				if err := r.List(ctx, pools); err != nil {
					t.Fatal(err)
				}
				if len(pools.Items) != 0 {
					t.Fatal("rejected legacy dispatch created a RuntimePool")
				}
				return
			}
			if err != nil || existing || pool == nil || pool.Spec.ExecutionWorkspace != nil {
				t.Fatalf("plain RuntimePool creation = pool %#v, existing %v, error %v", pool, existing, err)
			}
		})
	}
}

func TestExternalWorkspaceDispatchPinsExactWorkspaceAndOpaqueParameters(t *testing.T) {
	ctx := context.Background()
	classResolver, class, provider, config := externalACPFixture(t, true)
	provider.Status.Adapter = &workspacev1alpha1.ExecutionWorkspaceAdapterStatus{Version: "v1"}
	provider.Status.Conditions = append(provider.Status.Conditions,
		metav1.Condition{Type: string(workspacev1alpha1.ConditionProviderHeartbeat), Status: metav1.ConditionTrue, ObservedGeneration: provider.Generation},
		metav1.Condition{Type: string(workspacev1alpha1.ConditionProviderCompatible), Status: metav1.ConditionTrue, ObservedGeneration: provider.Generation})
	if err := classResolver.Status().Update(ctx, provider); err != nil {
		t.Fatal(err)
	}
	profile := &unstructured.Unstructured{}
	profile.SetAPIVersion("example.workspace.orka.ai/v1alpha1")
	profile.SetKind("WorkspaceProfile")
	if err := classResolver.Get(ctx, client.ObjectKey{Namespace: class.Namespace, Name: class.Spec.ParametersRef.Name}, profile); err != nil {
		t.Fatal(err)
	}
	task := workspaceBindingTestTask(nil)
	resolved, err := classResolver.resolveACPWorkspaceClass(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := resolveACPWorkspaceBindingWithClass(task, "", resolved)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := newBindingTestReconciler(t, task, bindingTestNamespace())
	r.Client = fake.NewClientBuilder().WithScheme(r.Scheme).WithRESTMapper(classResolver.RESTMapper()).
		WithStatusSubresource(&corev1alpha1.Task{}, &corev1alpha1.RuntimePool{}, &workspacev1alpha1.ExecutionWorkspace{}, class, provider).
		WithObjects(task, bindingTestNamespace(), class, provider, config, profile).Build()
	r.APIReader = r.Client
	r.WorkspaceProviderAPIEnabled = true
	r.ACPWorkspaceDispatchEnabled = true
	plan, err := PlanACPRuntime(task, bindingTestAgent(), r.ACPRuntimeImages)
	if err != nil {
		t.Fatal(err)
	}
	plan, err = applyACPWorkspaceBindingToPlan(plan, binding)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := r.createACPClassWorkspace(ctx, task, binding, plan.PoolName, "external-workspace")
	if err != nil {
		t.Fatal(err)
	}
	workspace.UID, workspace.Generation = "workspace-uid", 1
	workspace.CreationTimestamp = metav1.Now()
	workspace.Spec.Attachment = &workspacev1alpha1.ExecutionWorkspaceAttachment{Epoch: 1, ExpiresAt: metav1.NewTime(time.Now().Add(time.Hour)), TaskRef: workspacev1alpha1.ObjectIdentityReference{Name: task.Name, UID: task.UID}}
	workspace.Spec.AttachmentEpoch = 1
	markWorkspaceAdmittedForPolicyReview(workspace, workspace.Generation)
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateAttached
	workspace.Status.AttachedEpoch = 1
	workspace.Status.Conditions = append(workspace.Status.Conditions, metav1.Condition{Type: string(workspacev1alpha1.ConditionWorkspaceAttached), Status: metav1.ConditionTrue, ObservedGeneration: 1})
	status := workspace.Status.DeepCopy()
	if err := r.Update(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	workspace.Status = *status
	if err := r.Status().Update(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	pool, existing, err := r.ensureACPRuntimePoolWithPolicy(ctx, task.Namespace, plan, workspace.Name, string(workspace.UID), string(task.UID), true, "")
	if err != nil || existing || pool == nil {
		t.Fatalf("external RuntimePool creation = pool %#v, existing %v, error %v", pool, existing, err)
	}
	frozen := pool.Spec.ExecutionWorkspace
	if frozen == nil || frozen.Provider != binding.Provider || frozen.BindingDigest != binding.BindingDigest || frozen.WorkspaceRef == nil || frozen.WorkspaceRef.Name != workspace.Name || frozen.WorkspaceRef.UID != workspace.UID {
		t.Fatalf("pool lost exact workspace binding: %#v", frozen)
	}
	if !reflect.DeepEqual(frozen.ParametersRef, binding.Class.ParametersRef) || !reflect.DeepEqual(frozen.ParametersBinding, binding.Class.ParametersBinding) {
		t.Fatal("pool lost immutable opaque profile pins")
	}
	if frozen.Workload == nil || frozen.Workload.ContractVersion != workspacev1alpha1.LifecycleContractV1 || frozen.Workload.ProtocolVersion != corev1alpha1.RuntimePoolProtocolHarnessV2 {
		t.Fatalf("pool did not publish generic workload requirements: %#v", frozen)
	}
}
