package controller

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// workspaceSpecWrites records every spec write of the fixture's workspace.
type workspaceSpecWrites struct {
	mu     sync.Mutex
	writes []workspacev1alpha1.ExecutionWorkspaceSpec
}

func (w *workspaceSpecWrites) record(spec workspacev1alpha1.ExecutionWorkspaceSpec) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, *spec.DeepCopy())
}

func (w *workspaceSpecWrites) all() []workspacev1alpha1.ExecutionWorkspaceSpec {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]workspacev1alpha1.ExecutionWorkspaceSpec(nil), w.writes...)
}

// recordWorkspaceSpecWrites wraps the RuntimePool controller's client so the
// test sees each workspace write it issues.
func (f *externalRuntimePoolFixture) recordWorkspaceSpecWrites() *workspaceSpecWrites {
	recorded := &workspaceSpecWrites{}
	f.r.Client = interceptor.NewClient(f.r.Client.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if workspace, ok := obj.(*workspacev1alpha1.ExecutionWorkspace); ok {
				recorded.record(workspace.Spec)
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
	return recorded
}

// stageRequesterAttachment stages the requester's attachment exactly as its
// Task does before the publication, and records the start, demand, and intent.
func (f *externalRuntimePoolFixture) stageRequesterAttachment(t *testing.T, resume bool) *corev1alpha1.Task {
	t.Helper()
	ctx := context.Background()
	if err := coordinationv1.AddToScheme(f.r.Scheme); err != nil {
		t.Fatal(err)
	}
	requester := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: f.workspace.Namespace, Name: "requester", UID: "requester-uid"}}
	if err := f.r.Create(ctx, requester); err != nil {
		t.Fatal(err)
	}
	workspace := f.currentWorkspace(t)
	workspace.CreationTimestamp = metav1.Now()
	manager := WorkspaceAttachmentManager{Client: f.r.Client, APIReader: f.r.Client, LeaseTTL: acpWorkspaceAttachmentTTL}
	attachment, err := manager.prepareAttachmentIntent(ctx, workspace, requester,
		func(*workspacev1alpha1.ExecutionWorkspace, *corev1alpha1.Task, time.Time) error { return nil })
	if err != nil {
		t.Fatalf("stage requester attachment: %v", err)
	}
	intent, err := encodeACPWorkspaceAttachmentIntent(attachment)
	if err != nil {
		t.Fatal(err)
	}
	f.patchWorkspaceAnnotations(t, func(annotations map[string]string) {
		annotations[acpWorkspaceResumeRequestedAnnotation] = time.Now().UTC().Format(time.RFC3339Nano) + " requester requester-uid"
		annotations[acpWorkspaceSpeculativeStartAnnotation] = formatACPWorkspaceSpeculativeStart(runtimePoolTestNow.Add(time.Minute), requester.UID)
		annotations[acpWorkspaceAttachmentIntentAnnotation] = intent
		annotations[acpWorkspaceDetachActionAnnotation] = string(workspacev1alpha1.WorkspaceOnDetachSuspend)
		if resume {
			annotations[acpWorkspaceResumedLineageAnnotation] = booleanTrueValue
		}
	})
	return requester
}

// suspendServedInstance drives the served first instance through a DataOnly
// suspension that the provider reports settled with retained data.
func (f *externalRuntimePoolFixture) suspendServedInstance(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	workspace, pod := f.serve(t)
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
	if err := f.r.Update(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	f.supervisor.probe = runtimePoolValidProbe(f.pool, &pod, "external-boot", true)
	retiring := f.retireExternalPoolInstance(t, &pod)
	if retiring.Spec.Retirement.Action != workspacev1alpha1.WorkloadRetirementSuspend {
		t.Fatalf("retirement = %#v, want Suspend", retiring.Spec.Retirement)
	}
	retiring.Status.State = workspacev1alpha1.ExecutionWorkspaceStateSuspended
	retiring.Status.Allocation.State = workspacev1alpha1.AllocationStopped
	retiring.Status.Allocation.Startup = nil
	retiring.Status.Allocation.RetainedData = &workspacev1alpha1.RetainedDataReference{
		ID: "tag", SourceInstance: retiring.Status.Allocation.Identity, ProofSHA256: "sha256:" + strings.Repeat("4", 64)}
	retiring.Status.ObservedGeneration = retiring.Generation
	if err := f.r.Status().Update(ctx, retiring); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Delete(ctx, &pod); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		runtimePoolReconcile(t, f.r, f.pool)
	}
	if status := runtimePoolTestGetPool(t, f.r, f.pool).Status; status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopped {
		t.Fatalf("suspended pool = %s: %s", status.Lifecycle, status.Message)
	}
}

// ADR 0057: the RuntimePool flips a pending speculative resume back to Ready,
// publishes the next request, and installs the requester's staged attachment
// in one write, so the provider observes all three together.
func TestRuntimePoolFlipsPublishesAndAttachesASpeculativeResumeInOneWrite(t *testing.T) {
	ctx := context.Background()
	f := newExternalRuntimePoolFixture(t)
	f.suspendCapableWorkspace(t, nil)
	f.suspendServedInstance(t)
	requester := f.stageRequesterAttachment(t, true)
	if !acpWorkspaceSpeculativeResumePending(f.currentWorkspace(t), runtimePoolTestNow) {
		t.Fatal("fixture is not a pending speculative resume")
	}
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	pool.Spec.DesiredReplicas = 1
	if err := f.r.Update(ctx, &pool); err != nil {
		t.Fatal(err)
	}
	writes := f.recordWorkspaceSpecWrites()
	for range 6 {
		runtimePoolReconcile(t, f.r, f.pool)
		if f.currentWorkspace(t).Spec.Workload.Sequence == 2 {
			break
		}
	}
	resumed := f.currentWorkspace(t)
	if resumed.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady || resumed.Spec.Workload.Sequence != 2 ||
		resumed.Spec.Workload.RetainedData == nil || resumed.Spec.Retirement != nil {
		t.Fatalf("resume = %s seq %d, want Ready with the retained-data request", resumed.Spec.DesiredState, resumed.Spec.Workload.Sequence)
	}
	if resumed.Spec.Attachment == nil || resumed.Spec.Attachment.TaskRef.UID != requester.UID ||
		resumed.Spec.AttachmentEpoch != resumed.Spec.Attachment.Epoch {
		t.Fatalf("attachment = %#v, want the requester's staged attachment", resumed.Spec.Attachment)
	}
	if _, present := resumed.Annotations[acpWorkspaceAttachmentIntentAnnotation]; present {
		t.Fatal("the install kept the staged attachment")
	}
	for _, spec := range writes.all() {
		ready := spec.DesiredState == workspacev1alpha1.ExecutionWorkspaceDesiredReady
		published := spec.Workload != nil && spec.Workload.Sequence == 2
		attached := spec.Attachment != nil
		if ready != published || published != attached {
			t.Fatalf("a write split the flip, publication, and attachment: ready=%v published=%v attached=%v", ready, published, attached)
		}
	}
}

// A spare-like runtime that finishes booting right after the publication is
// admitted exactly once: the attachment arrived in the same generation, so no
// later attach invalidates the provider's observation.
func TestSpeculativeFastBootIsAdmittedOnce(t *testing.T) {
	ctx := context.Background()
	f := newExternalRuntimePoolFixture(t)
	f.suspendCapableWorkspace(t, nil)
	requester := f.stageRequesterAttachment(t, false)
	writes := f.recordWorkspaceSpecWrites()
	published := f.publish(t)
	if published.Spec.Attachment == nil || published.Spec.Attachment.TaskRef.UID != requester.UID {
		t.Fatalf("first publication attachment = %#v, want the requester's staged attachment", published.Spec.Attachment)
	}
	for _, spec := range writes.all() {
		if spec.Workload != nil && spec.Workload.Sequence == 1 {
			if spec.Attachment == nil || spec.Attachment.TaskRef.UID != requester.UID {
				t.Fatalf("first-turn workload write omitted the requester's staged attachment: %#v", spec.Attachment)
			}
		}
	}
	generation := published.Generation
	// The provider observes the single write: it acknowledges the epoch and
	// the spare runtime is Ready at once.
	materialized, pod := f.materialize(t)
	workspace := f.currentWorkspace(t)
	workspace.Status = materialized.Status
	workspace.Status.AttachedEpoch = workspace.Spec.Attachment.Epoch
	workspace.Status.State = workspacev1alpha1.ExecutionWorkspaceStateAttached
	if err := f.r.Status().Update(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	status := runtimePoolTestGetPool(t, f.r, f.pool).Status
	if status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing || status.AdmissionState != corev1alpha1.RuntimePoolAdmissionAccepting {
		t.Fatalf("fast boot did not serve: %s/%s: %s", status.Lifecycle, status.AdmissionState, status.Message)
	}
	active := status.ActiveInstance.DeepCopy()
	for range 3 {
		runtimePoolReconcile(t, f.r, f.pool)
	}
	status = runtimePoolTestGetPool(t, f.r, f.pool).Status
	if status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing || status.AdmissionState != corev1alpha1.RuntimePoolAdmissionAccepting ||
		status.ActiveInstance == nil || status.ActiveInstance.PodUID != active.PodUID || status.ActiveInstance.BootID != active.BootID {
		t.Fatalf("the admitted runtime was re-admitted: %s/%s %#v", status.Lifecycle, status.AdmissionState, status.ActiveInstance)
	}
	if current := f.currentWorkspace(t); current.Generation != generation || current.Spec.Attachment == nil {
		t.Fatal("the attachment needed a later workspace generation")
	}
	if pod.UID != types.UID(active.PodUID) {
		t.Fatal("the active instance is not the attested runtime")
	}
}
