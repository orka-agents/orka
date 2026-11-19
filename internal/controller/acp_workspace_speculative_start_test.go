package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/operatoraccess"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func speculativeStartTestWorkspace(mutate ...func(*workspacev1alpha1.ExecutionWorkspace)) *workspacev1alpha1.ExecutionWorkspace {
	workspace := &workspacev1alpha1.ExecutionWorkspace{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "speculative", UID: "speculative-uid", Annotations: map[string]string{
			acpWorkspaceResumeRequestedAnnotation:  time.Now().UTC().Format(time.RFC3339Nano) + " requester requester-uid",
			acpWorkspaceSpeculativeStartAnnotation: formatACPWorkspaceSpeculativeStart(time.Now().Add(time.Minute), "requester-uid"),
		}, Labels: map[string]string{}},
		Spec: workspacev1alpha1.ExecutionWorkspaceSpec{DesiredState: workspacev1alpha1.ExecutionWorkspaceDesiredReady},
	}
	for _, m := range mutate {
		m(workspace)
	}
	return workspace
}

func TestACPWorkspaceSpeculativeStartHoldsPoolOnlyWhileOpen(t *testing.T) {
	t.Parallel()
	now := time.Now()
	for _, test := range []struct {
		name   string
		mutate func(*workspacev1alpha1.ExecutionWorkspace)
		held   bool
	}{
		{name: "open start", held: true},
		{name: "expired", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Annotations[acpWorkspaceSpeculativeStartAnnotation] = formatACPWorkspaceSpeculativeStart(now.Add(-time.Second), "requester-uid")
		}},
		{name: "malformed", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Annotations[acpWorkspaceSpeculativeStartAnnotation] = "soon"
		}},
		{name: "no requester", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Annotations[acpWorkspaceSpeculativeStartAnnotation] = now.Add(time.Minute).UTC().Format(time.RFC3339Nano)
		}},
		{name: "demand fulfilled", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			delete(w.Annotations, acpWorkspaceResumeRequestedAnnotation)
		}},
		{name: "attached", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Spec.Attachment = &workspacev1alpha1.ExecutionWorkspaceAttachment{Epoch: 1}
		}},
		{name: "suspended by settlement", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
		}},
		{name: "settlement pending", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Annotations[acpWorkspaceRevocationStartedAnnotation] = "3 " + now.UTC().Format(time.RFC3339Nano)
		}},
		{name: "failed", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Status.State = workspacev1alpha1.ExecutionWorkspaceStateFailed
		}},
		{name: "quarantined", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Labels[workspacev1alpha1.QuarantinedLabel] = booleanTrueValue
		}},
		{name: "tainted", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Annotations[operatoraccess.TaintAnnotation] = "x"
		}},
		{name: "deleting", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			deleted := metav1.NewTime(now)
			w.DeletionTimestamp = &deleted
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			workspace := speculativeStartTestWorkspace()
			if test.mutate != nil {
				test.mutate(workspace)
			}
			if _, held := acpWorkspaceSpeculativeStartHoldsPool(workspace, now); held != test.held {
				t.Fatalf("held = %v, want %v", held, test.held)
			}
			if got := workspaceRuntimePoolDemandHeld(workspace, now); got != test.held && workspace.Spec.Attachment == nil {
				t.Fatalf("pool demand held = %v, want %v", got, test.held)
			}
		})
	}
}

func TestACPWorkspaceSpeculativeStartAttachableOnlyForTheBootingRequest(t *testing.T) {
	t.Parallel()
	key := workspacev1alpha1.AllocationKey{Namespace: "tenant-a", Name: "speculative", WorkspaceUID: "speculative-uid", ProviderUID: "provider-uid"}
	previous := workspacev1alpha1.InstanceIdentity{AllocationID: "allocation", InstanceID: "previous", RequestRevision: "rev-1"}
	request := func(sequence int64) *workspacev1alpha1.WorkloadRequest {
		r := &workspacev1alpha1.WorkloadRequest{Sequence: sequence, Key: key, Revision: "rev-" + string(rune('0'+sequence))}
		if sequence > 1 {
			r.PreviousInstance = &previous
		}
		return r
	}
	for _, test := range []struct {
		name       string
		mutate     func(*workspacev1alpha1.ExecutionWorkspace)
		attachable bool
	}{
		{name: "first turn published, provider not yet reporting", attachable: true, mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Spec.Workload = request(1)
		}},
		{name: "first turn booting", attachable: true, mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Spec.Workload = request(1)
			w.Status.Allocation = &workspacev1alpha1.AllocationObservation{Sequence: 1, Key: key, State: workspacev1alpha1.AllocationPending}
		}},
		{name: "resume booting", attachable: true, mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Spec.Workload = request(2)
			w.Status.Allocation = &workspacev1alpha1.AllocationObservation{Sequence: 2, Key: key, State: workspacev1alpha1.AllocationPending,
				Identity: workspacev1alpha1.InstanceIdentity{RequestRevision: "rev-2"}}
		}},
		{name: "no speculative start", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			delete(w.Annotations, acpWorkspaceSpeculativeStartAnnotation)
			w.Spec.Workload = request(2)
			w.Status.Allocation = &workspacev1alpha1.AllocationObservation{Sequence: 2, Key: key, State: workspacev1alpha1.AllocationPending}
		}},
		{name: "retirement authorized", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Spec.Workload = request(2)
			w.Spec.Retirement = &workspacev1alpha1.WorkloadRetirement{Sequence: 2, Action: workspacev1alpha1.WorkloadRetirementSuspend}
			w.Status.Allocation = &workspacev1alpha1.AllocationObservation{Sequence: 2, Key: key, State: workspacev1alpha1.AllocationPending}
		}},
		{name: "observation of an older request", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Spec.Workload = request(2)
			w.Status.Allocation = &workspacev1alpha1.AllocationObservation{Sequence: 1, Key: key, State: workspacev1alpha1.AllocationPending}
		}},
		{name: "foreign revision", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Spec.Workload = request(2)
			w.Status.Allocation = &workspacev1alpha1.AllocationObservation{Sequence: 2, Key: key, State: workspacev1alpha1.AllocationPending,
				Identity: workspacev1alpha1.InstanceIdentity{RequestRevision: "rev-other"}}
		}},
		{name: "resume request unobserved", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Spec.Workload = request(2)
		}},
		{name: "first turn after an earlier attachment", mutate: func(w *workspacev1alpha1.ExecutionWorkspace) {
			w.Spec.Workload = request(1)
			w.Spec.AttachmentEpoch = 1
		}},
		{name: "no request", mutate: func(*workspacev1alpha1.ExecutionWorkspace) {}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			workspace := speculativeStartTestWorkspace(test.mutate)
			if got := acpWorkspaceSpeculativeStartAttachable(workspace); got != test.attachable {
				t.Fatalf("attachable = %v, want %v", got, test.attachable)
			}
		})
	}
}

// speculativeExternalPoolFixture is an unattached, suspend-capable workspace
// whose pending requester recorded a speculative start.
func speculativeExternalPoolFixture(t *testing.T, deadline time.Time) *externalRuntimePoolFixture {
	t.Helper()
	f := newExternalRuntimePoolFixture(t)
	f.suspendCapableWorkspace(t, func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Annotations[acpWorkspaceDetachActionAnnotation] = string(workspacev1alpha1.WorkspaceOnDetachSuspend)
		w.Annotations[acpWorkspaceResumeRequestedAnnotation] = time.Now().UTC().Format(time.RFC3339Nano) + " requester requester-uid"
		w.Annotations[acpWorkspaceSpeculativeStartAnnotation] = formatACPWorkspaceSpeculativeStart(deadline, "requester-uid")
	})
	return f
}

func reconcileWorkspaceDemand(t *testing.T, r *WorkspaceRuntimePoolReconciler, key client.ObjectKey) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestWorkspaceRuntimePoolReconcilerHoldsSpeculativeStartUntilDeadline(t *testing.T) {
	deadline := time.Now().Add(time.Minute)
	f := speculativeExternalPoolFixture(t, deadline)
	key := client.ObjectKeyFromObject(f.workspace)
	now := time.Now()
	demand := &WorkspaceRuntimePoolReconciler{Client: f.r.Client, APIReader: f.r.Client, Now: func() time.Time { return now }}
	result := reconcileWorkspaceDemand(t, demand, key)
	if got := runtimePoolTestGetPool(t, f.r, f.pool).Spec.DesiredReplicas; got != 1 {
		t.Fatalf("open speculative start released the pool: replicas=%d", got)
	}
	if result.RequeueAfter <= 0 || result.RequeueAfter > deadline.Sub(now)+2*time.Second {
		t.Fatalf("requeue = %s, want the deadline %s", result.RequeueAfter, deadline.Sub(now))
	}

	// A controller restart rebuilds the hold from the workspace alone.
	restarted := &WorkspaceRuntimePoolReconciler{Client: f.r.Client, APIReader: f.r.Client, Now: func() time.Time { return now }}
	reconcileWorkspaceDemand(t, restarted, key)
	if got := runtimePoolTestGetPool(t, f.r, f.pool).Spec.DesiredReplicas; got != 1 {
		t.Fatalf("restarted controller released the pool: replicas=%d", got)
	}

	// Only the deadline ends an unclaimed start.
	later := deadline.Add(time.Second)
	expired := &WorkspaceRuntimePoolReconciler{Client: f.r.Client, APIReader: f.r.Client, Now: func() time.Time { return later }}
	reconcileWorkspaceDemand(t, expired, key)
	if got := runtimePoolTestGetPool(t, f.r, f.pool).Spec.DesiredReplicas; got != 0 {
		t.Fatalf("expired speculative start kept the pool: replicas=%d", got)
	}
}

func TestWorkspaceRuntimePoolReconcilerConfirmsStaleCacheBeforeScalingDown(t *testing.T) {
	f := speculativeExternalPoolFixture(t, time.Now().Add(time.Minute))
	key := client.ObjectKeyFromObject(f.workspace)
	// The informer cache has not yet seen the speculative start that the
	// Task controller wrote before activating the pool.
	stale := interceptor.NewClient(f.r.Client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := c.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			if workspace, ok := obj.(*workspacev1alpha1.ExecutionWorkspace); ok {
				delete(workspace.Annotations, acpWorkspaceSpeculativeStartAnnotation)
			}
			return nil
		},
	})
	demand := &WorkspaceRuntimePoolReconciler{Client: stale, APIReader: f.r.Client}
	reconcileWorkspaceDemand(t, demand, key)
	if got := runtimePoolTestGetPool(t, f.r, f.pool).Spec.DesiredReplicas; got != 1 {
		t.Fatalf("a stale cache scaled the speculative pool down: replicas=%d", got)
	}
	// Without the hold on the API server either, the pool scales down.
	f.patchWorkspaceAnnotations(t, func(annotations map[string]string) {
		delete(annotations, acpWorkspaceSpeculativeStartAnnotation)
	})
	reconcileWorkspaceDemand(t, demand, key)
	if got := runtimePoolTestGetPool(t, f.r, f.pool).Spec.DesiredReplicas; got != 0 {
		t.Fatalf("an unheld unattached pool kept replicas=%d", got)
	}
}

func (f *externalRuntimePoolFixture) patchWorkspaceAnnotations(t *testing.T, mutate func(map[string]string)) {
	t.Helper()
	workspace := f.currentWorkspace(t)
	base := workspace.DeepCopy()
	mutate(workspace.Annotations)
	if err := f.r.Patch(context.Background(), workspace, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
}

// The speculative runtime passes the same attestation, sealed bootstrap, and
// authenticated probe as any start, yet no prompt can reach it before the
// requester holds the attachment. An unclaimed start retires through the
// ordinary data-preserving path, idempotently.
func TestSpeculativeRuntimeIsAttestedAndRetiresWhenUnclaimed(t *testing.T) {
	for _, test := range []struct {
		name       string
		end        func(*testing.T, *externalRuntimePoolFixture) func() time.Time
		wantAction workspacev1alpha1.WorkloadRetirementAction
	}{
		{name: "requester rejected and settled", wantAction: workspacev1alpha1.WorkloadRetirementSuspend,
			end: func(t *testing.T, f *externalRuntimePoolFixture) func() time.Time {
				// Settlement of a requester that never attached applies its
				// frozen Suspend; that write also removes the start.
				workspace := f.currentWorkspace(t)
				base := workspace.DeepCopy()
				workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
				delete(workspace.Annotations, acpWorkspaceResumeRequestedAnnotation)
				delete(workspace.Annotations, acpWorkspaceSpeculativeStartAnnotation)
				if err := f.r.Patch(context.Background(), workspace, client.MergeFrom(base)); err != nil {
					t.Fatal(err)
				}
				return time.Now
			}},
		{name: "deadline passed while the requester lives", wantAction: workspacev1alpha1.WorkloadRetirementSuspend,
			end: func(*testing.T, *externalRuntimePoolFixture) func() time.Time {
				later := time.Now().Add(time.Hour)
				return func() time.Time { return later }
			}},
		{name: "operator taint", wantAction: workspacev1alpha1.WorkloadRetirementStop,
			end: func(t *testing.T, f *externalRuntimePoolFixture) func() time.Time {
				f.taintWorkspace(t)
				return time.Now
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := speculativeExternalPoolFixture(t, time.Now().Add(time.Minute))
			key := client.ObjectKeyFromObject(f.workspace)
			reconcileWorkspaceDemand(t, &WorkspaceRuntimePoolReconciler{Client: f.r.Client, APIReader: f.r.Client}, key)
			workspace, pod := f.serve(t)
			if f.seeds != 1 || f.supervisor.probeCalls != 1 {
				t.Fatalf("bootstrap/probe calls = %d/%d, want the full startup handshake", f.seeds, f.supervisor.probeCalls)
			}
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			if err := acpWorkspacePoolReadinessFailure(workspace, &pool, string(workspace.UID), "requester-uid", time.Now()); err == nil {
				t.Fatal("an unattached speculative runtime passed the prompt handshake")
			}

			now := test.end(t, f)
			demand := &WorkspaceRuntimePoolReconciler{Client: f.r.Client, APIReader: f.r.Client, Now: now}
			reconcileWorkspaceDemand(t, demand, key)
			if got := runtimePoolTestGetPool(t, f.r, f.pool).Spec.DesiredReplicas; got != 0 {
				t.Fatalf("unclaimed speculative runtime kept replicas=%d", got)
			}
			retired := f.retireExternalPoolInstance(t, &pod)
			if retired.Spec.Retirement.Action != test.wantAction || retired.Spec.Retirement.Sequence != workspace.Spec.Workload.Sequence {
				t.Fatalf("retirement = %#v, want %s of the exact speculative request", retired.Spec.Retirement, test.wantAction)
			}
			// Retiring again changes nothing.
			reconcileWorkspaceDemand(t, demand, key)
			runtimePoolReconcile(t, f.r, f.pool)
			if again := f.currentWorkspace(t); again.Spec.Retirement == nil || *again.Spec.Retirement != *retired.Spec.Retirement {
				t.Fatalf("retirement changed on a repeated pass: %#v", again.Spec.Retirement)
			}
		})
	}
}

// speculativeTaskFixture is a fresh session workspace materialized by its
// first Task, with a RuntimePool plan the Task may create.
type speculativeTaskFixture struct {
	r    *TaskReconciler
	task *corev1alpha1.Task
	plan ACPRuntimePlan
	key  types.NamespacedName
}

func newSpeculativeTaskFixture(t *testing.T) *speculativeTaskFixture {
	t.Helper()
	return newSpeculativeTaskFixtureWith(t, nil)
}

// newSpeculativeTaskFixtureWith lets a test adjust the reconciler and the
// frozen binding before the workspace is materialized.
func newSpeculativeTaskFixtureWith(t *testing.T, configure func(*TaskReconciler, *ACPRuntimeWorkspaceBinding)) *speculativeTaskFixture {
	t.Helper()
	return newSpeculativeTaskFixtureFor(t, suspendableSubstrateFixture(t), configure)
}

// newSpeculativeTaskFixtureFor materializes the first turn of fixture's class.
func newSpeculativeTaskFixtureFor(t *testing.T, fixture *acpClassFixture, configure func(*TaskReconciler, *ACPRuntimeWorkspaceBinding)) *speculativeTaskFixture {
	t.Helper()
	ctx := context.Background()
	// A usable provider reports its adapter, heartbeat, and compatibility.
	fixture.provider.Status.Adapter = &workspacev1alpha1.ExecutionWorkspaceAdapterStatus{Version: "v1"}
	fixture.provider.Status.Conditions = append(fixture.provider.Status.Conditions,
		metav1.Condition{Type: string(workspacev1alpha1.ConditionProviderHeartbeat), Status: metav1.ConditionTrue, ObservedGeneration: fixture.provider.Generation},
		metav1.Condition{Type: string(workspacev1alpha1.ConditionProviderCompatible), Status: metav1.ConditionTrue, ObservedGeneration: fixture.provider.Generation})
	task := suspendableSessionTask()
	r := acpClassTestReconciler(t, append(fixture.objects(), task)...)
	task = bindSuspendableSessionTaskForSettlement(t, r, task)
	resolved, err := r.resolveACPWorkspaceClass(ctx, task)
	if err != nil {
		t.Fatalf("resolve class: %v", err)
	}
	binding, err := resolveACPWorkspaceBindingWithClass(task, suspendTestSessionUID, resolved)
	if err != nil {
		t.Fatalf("resolve binding: %v", err)
	}
	base := runtimePoolTestObject(1)
	profile, err := runtimePoolHarnessProfile(base.Spec.Runtime.Profile)
	if err != nil {
		t.Fatal(err)
	}
	if configure != nil {
		configure(r, binding)
	}
	plan := ACPRuntimePlan{PoolName: acpTestSessionPoolName, Workspace: binding, Image: base.Spec.Runtime.Image,
		Digest: harnessv2.ProfileDigest(base.Spec.Runtime.Profile.Digest), Profile: profile}
	f := &speculativeTaskFixture{r: r, plan: plan, key: types.NamespacedName{Namespace: task.Namespace, Name: acpClassWorkspaceName(task, binding)}}
	// The creation pass materializes and links the workspace, then waits
	// for core admission; that is where the Task records its start.
	if _, ready, err := r.ensureACPClassWorkspace(ctx, task, plan); err != nil || ready {
		t.Fatalf("materialize = (%v, %v)", ready, err)
	}
	workspace := f.workspace(t)
	admitTestACPWorkspace(t, r, workspace)
	// The provider has observed the fresh workspace but reports no allocation.
	f.patchStatus(t, func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Status.State = workspacev1alpha1.ExecutionWorkspaceStatePending
	})
	// The fake client assigns the UID only at admission; repeat the link the
	// creation pass makes against a real API server.
	task = f.reloadTask(t, task)
	if err := r.linkTaskToACPWorkspace(ctx, task, f.workspace(t)); err != nil {
		t.Fatal(err)
	}
	f.task = f.reloadTask(t, task)
	if f.task.Annotations[acpExecutionWorkspaceUIDAnnotation] != string(f.workspace(t).UID) {
		t.Fatal("creation pass did not link the Task to the workspace incarnation")
	}
	return f
}

func (f *speculativeTaskFixture) workspace(t *testing.T) *workspacev1alpha1.ExecutionWorkspace {
	t.Helper()
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := f.r.Get(context.Background(), f.key, workspace); err != nil {
		t.Fatalf("read workspace: %v", err)
	}
	return workspace
}

func (f *speculativeTaskFixture) reloadTask(t *testing.T, task *corev1alpha1.Task) *corev1alpha1.Task {
	t.Helper()
	current := &corev1alpha1.Task{}
	if err := f.r.Get(context.Background(), client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatalf("reload task: %v", err)
	}
	return current
}

func (f *speculativeTaskFixture) patch(t *testing.T, mutate func(*workspacev1alpha1.ExecutionWorkspace)) {
	t.Helper()
	workspace := f.workspace(t)
	base := workspace.DeepCopy()
	mutate(workspace)
	if err := f.r.Patch(context.Background(), workspace, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
}

func (f *speculativeTaskFixture) patchStatus(t *testing.T, mutate func(*workspacev1alpha1.ExecutionWorkspace)) {
	t.Helper()
	workspace := f.workspace(t)
	base := workspace.DeepCopy()
	mutate(workspace)
	if err := f.r.Status().Patch(context.Background(), workspace, client.MergeFrom(base)); err != nil {
		t.Fatal(err)
	}
}

func (f *speculativeTaskFixture) pool(t *testing.T) (*corev1alpha1.RuntimePool, bool) {
	t.Helper()
	pool := &corev1alpha1.RuntimePool{}
	err := f.r.Get(context.Background(), types.NamespacedName{Namespace: f.key.Namespace, Name: f.plan.PoolName}, pool)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return pool, true
}

func (f *speculativeTaskFixture) start(t *testing.T, task *corev1alpha1.Task, allowCreate bool, requiredUID types.UID) {
	t.Helper()
	f.r.startACPWorkspaceSpeculativeRuntime(context.Background(), task, f.plan, allowCreate, requiredUID)
}

func TestSpeculativeStartRecordsOnceAndActivatesThePool(t *testing.T) {
	t.Parallel()
	f := newSpeculativeTaskFixture(t)
	before := time.Now()
	f.start(t, f.task, true, "")
	workspace := f.workspace(t)
	deadline, owner, ok := parseACPWorkspaceSpeculativeStart(workspace.Annotations[acpWorkspaceSpeculativeStartAnnotation])
	if !ok || owner != string(f.task.UID) || deadline.Before(before.Add(acpWorkspaceSpeculativeStartTTL)) ||
		deadline.After(time.Now().Add(acpWorkspaceSpeculativeStartTTL)) {
		t.Fatalf("speculative start = %q, want this Task for %s", workspace.Annotations[acpWorkspaceSpeculativeStartAnnotation], acpWorkspaceSpeculativeStartTTL)
	}
	pool, exists := f.pool(t)
	if !exists || pool.Spec.DesiredReplicas != 1 || pool.Labels[acpExecutionWorkspaceLinkLabel] != workspace.Name ||
		pool.Annotations[acpExecutionWorkspaceUIDAnnotation] != string(workspace.UID) {
		t.Fatalf("speculative pool = %#v, want an active pool linked to the exact workspace", pool)
	}
	if workspace.Spec.Attachment != nil {
		t.Fatal("a speculative start attached the workspace")
	}

	// The deadline is written once and never extended, including after a
	// controller restart that loses every in-memory record.
	recorded := workspace.Annotations[acpWorkspaceSpeculativeStartAnnotation]
	restarted := &TaskReconciler{Client: f.r.Client, APIReader: f.r.APIReader, Scheme: f.r.Scheme,
		WorkspaceProviderAPIEnabled: true, WorkspaceSettlementProtected: true}
	restarted.startACPWorkspaceSpeculativeRuntime(context.Background(), f.task, f.plan, true, "")
	if got := f.workspace(t).Annotations[acpWorkspaceSpeculativeStartAnnotation]; got != recorded {
		t.Fatalf("speculative start rewritten to %q", got)
	}

	// After expiry the Task neither re-records the start nor reactivates the
	// pool: it attaches through the ordinary path.
	f.patch(t, func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Annotations[acpWorkspaceSpeculativeStartAnnotation] = formatACPWorkspaceSpeculativeStart(time.Now().Add(-time.Second), f.task.UID)
	})
	pool.Spec.DesiredReplicas = 0
	if err := f.r.Update(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	f.start(t, f.task, true, "")
	if expired, _ := f.pool(t); expired.Spec.DesiredReplicas != 0 {
		t.Fatal("an expired speculative start reactivated the pool")
	}
}

func TestSpeculativeStartRefusesIneligibleRequests(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *speculativeTaskFixture) *corev1alpha1.Task
	}{
		{name: "another Task owns the demand", mutate: func(t *testing.T, f *speculativeTaskFixture) *corev1alpha1.Task {
			f.patch(t, func(w *workspacev1alpha1.ExecutionWorkspace) {
				w.Annotations[acpWorkspaceResumeRequestedAnnotation] = time.Now().UTC().Format(time.RFC3339Nano) + " other other-uid"
			})
			return f.task
		}},
		{name: "workspace tainted", mutate: func(t *testing.T, f *speculativeTaskFixture) *corev1alpha1.Task {
			f.patch(t, func(w *workspacev1alpha1.ExecutionWorkspace) { w.Annotations[operatoraccess.TaintAnnotation] = "x" })
			return f.task
		}},
		{name: "linked Task tainted", mutate: func(t *testing.T, f *speculativeTaskFixture) *corev1alpha1.Task {
			task := f.reloadTask(t, f.task)
			task.Annotations[operatoraccess.TaintAnnotation] = "x"
			if err := f.r.Update(context.Background(), task); err != nil {
				t.Fatal(err)
			}
			return f.reloadTask(t, task)
		}},
		{name: "settlement pending", mutate: func(t *testing.T, f *speculativeTaskFixture) *corev1alpha1.Task {
			f.patch(t, func(w *workspacev1alpha1.ExecutionWorkspace) {
				w.Annotations[acpWorkspaceRevocationStartedAnnotation] = "1 " + time.Now().UTC().Format(time.RFC3339Nano)
			})
			return f.task
		}},
		{name: "suspended", mutate: func(t *testing.T, f *speculativeTaskFixture) *corev1alpha1.Task {
			f.patch(t, func(w *workspacev1alpha1.ExecutionWorkspace) {
				w.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
			})
			return f.task
		}},
		{name: "fresh workspace already attached once", mutate: func(t *testing.T, f *speculativeTaskFixture) *corev1alpha1.Task {
			f.patch(t, func(w *workspacev1alpha1.ExecutionWorkspace) { w.Spec.AttachmentEpoch = 1 })
			return f.task
		}},
		{name: "Task not linked to this incarnation", mutate: func(t *testing.T, f *speculativeTaskFixture) *corev1alpha1.Task {
			task := f.task.DeepCopy()
			task.Annotations[acpExecutionWorkspaceUIDAnnotation] = "another-incarnation"
			return task
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			f := newSpeculativeTaskFixture(t)
			task := test.mutate(t, f)
			f.start(t, task, true, "")
			if _, present := f.workspace(t).Annotations[acpWorkspaceSpeculativeStartAnnotation]; present {
				t.Fatal("ineligible request recorded a speculative start")
			}
			if _, exists := f.pool(t); exists {
				t.Fatal("ineligible request created RuntimePool demand")
			}
		})
	}
}

// bootSpeculativeWorkload stands in for the RuntimePool controller and the
// provider: the speculative request is published and still booting.
func (f *speculativeTaskFixture) bootSpeculativeWorkload(t *testing.T) {
	t.Helper()
	workspace := f.workspace(t)
	key := workspacev1alpha1.AllocationKey{Namespace: workspace.Namespace, Name: workspace.Name, WorkspaceUID: workspace.UID, ProviderUID: workspace.Spec.ProviderBinding.UID}
	f.patch(t, func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Spec.Workload = &workspacev1alpha1.WorkloadRequest{Sequence: 1, Key: key, Revision: speculativeTestRevision}
	})
	workspace = f.workspace(t)
	admitTestACPWorkspace(t, f.r, workspace)
	f.patchStatus(t, func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Status.State = workspacev1alpha1.ExecutionWorkspaceStatePending
		w.Status.Allocation = &workspacev1alpha1.AllocationObservation{Sequence: 1, Key: key, State: workspacev1alpha1.AllocationPending}
	})
}

func TestSpeculativeStartIsClaimedByExactlyOneAttachment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSpeculativeTaskFixture(t)
	f.start(t, f.task, true, "")
	f.bootSpeculativeWorkload(t)

	// A concurrent continuation of the same Session reaches the booting
	// workspace first. It may claim it: the runtime is Session-scoped, cold
	// booted, and unreachable until the post-attach handshake.
	rival := f.task.DeepCopy()
	rival.ObjectMeta = metav1.ObjectMeta{Namespace: f.task.Namespace, Name: "rival-continuation", UID: "rival-continuation-uid"}
	rival.Status = corev1alpha1.TaskStatus{}
	if err := f.r.Create(ctx, rival); err != nil {
		t.Fatal(err)
	}
	if _, ready := attachTestACPWorkspace(t, f.r, rival, f.plan, f.key.Name); !ready {
		t.Fatal("rival continuation did not attach the booting speculative workspace")
	}
	claimed := f.workspace(t)
	if claimed.Spec.Attachment == nil || claimed.Spec.Attachment.TaskRef.UID != rival.UID {
		t.Fatalf("attachment = %#v, want the rival", claimed.Spec.Attachment)
	}
	if _, present := claimed.Annotations[acpWorkspaceSpeculativeStartAnnotation]; present {
		t.Fatal("the attachment did not claim the speculative start")
	}
	if claimed.Spec.Workload == nil || claimed.Spec.Workload.Sequence != 1 {
		t.Fatal("claiming the speculative start replaced its request")
	}

	// The requester can neither attach nor re-record a start now.
	if err := validateACPClassWorkspaceAttachmentAttempt(claimed, f.task, f.plan.Workspace, f.plan.PoolName, time.Now()); !errors.Is(err, ErrWorkspaceAttachmentLocked) {
		t.Fatalf("requester attach error = %v, want the one-writer lock", err)
	}
	if _, ready, err := f.r.ensureACPClassWorkspace(ctx, f.task, f.plan); err != nil || ready {
		t.Fatalf("requester ensure = (%v, %v), want queued behind the rival", ready, err)
	}
	f.start(t, f.task, true, "")
	if _, present := f.workspace(t).Annotations[acpWorkspaceSpeculativeStartAnnotation]; present {
		t.Fatal("requester re-recorded a start on an attached workspace")
	}
}

func TestSpeculativeStartAttachmentWaitsForAdmissionAndProviderObservation(t *testing.T) {
	t.Parallel()
	f := newSpeculativeTaskFixture(t)
	f.start(t, f.task, true, "")
	f.bootSpeculativeWorkload(t)
	workspace := f.workspace(t)
	if err := validateACPClassWorkspaceAttachmentAttempt(workspace, f.task, f.plan.Workspace, f.plan.PoolName, time.Now()); err != nil {
		t.Fatalf("booting speculative workspace refused its requester: %v", err)
	}
	unobserved := workspace.DeepCopy()
	unobserved.Status.ObservedGeneration--
	if err := validateACPClassWorkspaceAttachmentAttempt(unobserved, f.task, f.plan.Workspace, f.plan.PoolName, time.Now()); err == nil {
		t.Fatal("attached before the provider observed the current generation")
	}
	quarantined := workspace.DeepCopy()
	quarantined.Status.Conditions = append(quarantined.Status.Conditions, metav1.Condition{
		Type: string(workspacev1alpha1.ConditionWorkspaceQuarantined), Status: metav1.ConditionTrue, ObservedGeneration: workspace.Generation})
	if err := validateACPClassWorkspaceAttachmentAttempt(quarantined, f.task, f.plan.Workspace, f.plan.PoolName, time.Now()); err == nil {
		t.Fatal("attached a quarantined speculative workspace")
	}
	tainted := workspace.DeepCopy()
	tainted.Annotations[operatoraccess.TaintAnnotation] = "x"
	if err := validateACPClassWorkspaceAttachmentAttempt(tainted, f.task, f.plan.Workspace, f.plan.PoolName, time.Now()); err == nil ||
		!strings.Contains(err.Error(), operatoraccess.ErrTainted.Error()) {
		t.Fatalf("tainted speculative workspace attach error = %v", err)
	}
}

// A Task cancelled after its speculative start settles exactly like a
// requester that never attached: an empty first-turn workspace is deleted,
// and its linked pool is torn down with it.
func TestCancelledFirstTurnAfterSpeculativeStartDeletesTheWorkspace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSpeculativeTaskFixture(t)
	f.start(t, f.task, true, "")
	f.bootSpeculativeWorkload(t)
	f.patch(t, func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Finalizers = append(w.Finalizers, executionWorkspaceFinalizer)
	})
	task := f.reloadTask(t, f.task)
	if done, err := f.r.settleACPClassWorkspace(ctx, task); err != nil || !done {
		t.Fatalf("settle = (%v, %v), want completion", done, err)
	}
	deleted := f.workspace(t)
	if deleted.DeletionTimestamp.IsZero() {
		t.Fatal("settlement kept the empty speculative workspace")
	}
	demand := &WorkspaceRuntimePoolReconciler{Client: f.r.Client, APIReader: f.r.APIReader}
	if _, err := demand.Reconcile(ctx, ctrl.Request{NamespacedName: f.key}); err != nil {
		t.Fatal(err)
	}
	if pool, exists := f.pool(t); exists && pool.DeletionTimestamp.IsZero() {
		t.Fatal("the speculative pool outlived its deleted workspace")
	}
}

// suspendAfterFirstTurn stands in for the first Task's executed, committed
// turn and its data-only suspension, leaving the stopped pool in place.
func (f *speculativeTaskFixture) suspendAfterFirstTurn(t *testing.T) *corev1alpha1.RuntimePool {
	t.Helper()
	ctx := context.Background()
	f.start(t, f.task, true, "")
	pool, exists := f.pool(t)
	if !exists {
		t.Fatal("first turn created no pool")
	}
	pool.Spec.DesiredReplicas = 0
	if err := f.r.Update(ctx, pool); err != nil {
		t.Fatal(err)
	}
	workspace := f.workspace(t)
	key := workspacev1alpha1.AllocationKey{Namespace: workspace.Namespace, Name: workspace.Name, WorkspaceUID: workspace.UID, ProviderUID: workspace.Spec.ProviderBinding.UID}
	identity := workspacev1alpha1.InstanceIdentity{AllocationID: "allocation", InstanceID: "first-instance", RequestRevision: "first-rev"}
	f.patch(t, func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
		w.Spec.AttachmentEpoch = 1
		w.Spec.Workload = &workspacev1alpha1.WorkloadRequest{Sequence: 1, Key: key, Revision: "first-rev"}
		w.Spec.Retirement = &workspacev1alpha1.WorkloadRetirement{Sequence: 1, Identity: identity, Action: workspacev1alpha1.WorkloadRetirementSuspend}
		w.Annotations[acpWorkspaceDurableSessionCommittedAnnotation] = "1"
		w.Annotations[acpWorkspaceLastSettledTaskAnnotation] = formatACPWorkspaceSettlementReceipt(string(f.task.UID), 1)
		delete(w.Annotations, acpWorkspaceResumeRequestedAnnotation)
		delete(w.Annotations, acpWorkspaceSpeculativeStartAnnotation)
	})
	workspace = f.workspace(t)
	admitTestACPWorkspace(t, f.r, workspace)
	f.patchStatus(t, func(w *workspacev1alpha1.ExecutionWorkspace) {
		w.Status.State = workspacev1alpha1.ExecutionWorkspaceStateSuspended
		w.Status.Allocation = &workspacev1alpha1.AllocationObservation{Sequence: 1, Key: key, Identity: identity, State: workspacev1alpha1.AllocationStopped,
			RetainedData: &workspacev1alpha1.RetainedDataReference{ID: "tag", SourceInstance: identity, ProofSHA256: "sha256:" + strings.Repeat("d", 64)}}
	})
	settled := f.reloadTask(t, f.task)
	settled.Annotations[acpTaskWorkspaceSettledAnnotation] = booleanTrueValue
	if err := f.r.Update(ctx, settled); err != nil {
		t.Fatal(err)
	}
	return pool
}

func (f *speculativeTaskFixture) continuation(t *testing.T, name string) *corev1alpha1.Task {
	t.Helper()
	continuation := suspendableSessionTask()
	continuation.Name, continuation.UID = name, types.UID(name+"-uid")
	if err := f.r.Create(context.Background(), continuation); err != nil {
		t.Fatal(err)
	}
	return bindSuspendableSessionTaskForSettlement(t, f.r, continuation)
}

func TestSpeculativeResumeStartsAtTheFlipAndRetiresWhenTheContinuationIsCancelled(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newSpeculativeTaskFixture(t)
	pool := f.suspendAfterFirstTurn(t)
	continuation := f.continuation(t, "speculative-continuation")

	// The cold-resume flip and the speculative start land in the same pass.
	if _, ready, err := f.r.ensureACPClassWorkspace(ctx, continuation, f.plan); err != nil || ready {
		t.Fatalf("resume flip = (%v, %v), want pending", ready, err)
	}
	continuation = f.reloadTask(t, continuation)
	f.start(t, continuation, false, pool.UID)
	flipped := f.workspace(t)
	if flipped.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady || flipped.Spec.Attachment != nil ||
		flipped.Annotations[acpWorkspaceResumedLineageAnnotation] != booleanTrueValue {
		t.Fatalf("resume flip = %s attached=%v, want an unattached Ready resumed lineage", flipped.Spec.DesiredState, flipped.Spec.Attachment != nil)
	}
	if _, owner, ok := parseACPWorkspaceSpeculativeStart(flipped.Annotations[acpWorkspaceSpeculativeStartAnnotation]); !ok || owner != string(continuation.UID) {
		t.Fatalf("speculative start = %q, want the continuation", flipped.Annotations[acpWorkspaceSpeculativeStartAnnotation])
	}
	if active, _ := f.pool(t); active.UID != pool.UID || active.Spec.DesiredReplicas != 1 {
		t.Fatal("the resume did not activate the existing pool")
	}
	demand := &WorkspaceRuntimePoolReconciler{Client: f.r.Client, APIReader: f.r.APIReader}
	if _, err := demand.Reconcile(ctx, ctrl.Request{NamespacedName: f.key}); err != nil {
		t.Fatal(err)
	}
	if active, _ := f.pool(t); active.Spec.DesiredReplicas != 1 {
		t.Fatal("the open speculative start was scaled down before the attachment")
	}

	// The continuation is cancelled before it attaches. Its settlement
	// applies its frozen Suspend to the never-claimed runtime.
	continuation = f.reloadTask(t, continuation)
	if done, err := f.r.settleACPClassWorkspace(ctx, continuation); err != nil || !done {
		t.Fatalf("settle cancelled continuation = (%v, %v), want completion", done, err)
	}
	resuspended := f.workspace(t)
	if resuspended.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredSuspended {
		t.Fatalf("desired state = %s, want Suspended", resuspended.Spec.DesiredState)
	}
	if _, present := resuspended.Annotations[acpWorkspaceSpeculativeStartAnnotation]; present {
		t.Fatal("suspension kept the unclaimed speculative start")
	}
	if _, err := demand.Reconcile(ctx, ctrl.Request{NamespacedName: f.key}); err != nil {
		t.Fatal(err)
	}
	if active, _ := f.pool(t); active.Spec.DesiredReplicas != 0 {
		t.Fatal("the unclaimed speculative runtime kept its pool after settlement")
	}
	// The cancelled Task can never attach the resuspended workspace, and a
	// repeated settlement is a no-op.
	if err := validateACPClassWorkspaceAttachmentAttempt(resuspended, continuation, f.plan.Workspace, f.plan.PoolName, time.Now()); err == nil {
		t.Fatal("a cancelled requester could attach after settlement")
	}
	continuation = f.reloadTask(t, continuation)
	if done, err := f.r.settleACPClassWorkspace(ctx, continuation); err != nil || !done {
		t.Fatalf("repeated settlement = (%v, %v)", done, err)
	}
	if again := f.workspace(t); again.ResourceVersion != resuspended.ResourceVersion {
		t.Fatal("a repeated settlement rewrote the workspace")
	}
}
