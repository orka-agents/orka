package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

func TestAgentExecutionClassificationGateRequiresExactSeal(t *testing.T) {
	control := classifiedReadinessControl()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(control).Build()
	gate := &AgentExecutionClassificationGate{APIReader: reader}
	if err := gate.Check(context.Background()); err != nil {
		t.Fatalf("sealed gate rejected: %v", err)
	}

	stale := control.DeepCopy()
	stale.Status.Classification.ControlGeneration--
	reader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(stale).Build()
	if err := (&AgentExecutionClassificationGate{APIReader: reader}).Check(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale gate error = %v", err)
	}

	open := control.DeepCopy()
	open.Status.Classification.State = corev1alpha1.AgentExecutionClassificationOpen
	reader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(open).Build()
	if err := (&AgentExecutionClassificationGate{APIReader: reader}).Check(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "not Sealed") {
		t.Fatalf("Open gate error = %v", err)
	}
}

func TestAgentExecutionClassificationReconcilerWritesDispositionsLineageAndSeal(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 6, 2, 0, 0, 0, time.UTC)
	control := &corev1alpha1.AgentExecutionControl{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: corev1alpha1.AgentExecutionControlNamespace,
			Name:      corev1alpha1.AgentExecutionControlName,
			UID:       "control-uid", Generation: 1,
		},
	}
	deletionTime := metav1.NewTime(now.Add(-time.Minute))
	deleting := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "deleting", UID: "deleting-uid", Generation: 1,
			Finalizers: []string{labels.TaskFinalizer}, DeletionTimestamp: &deletionTime,
		},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseFinalizing},
	}
	mixed := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "mixed", UID: "mixed-uid", Generation: 1,
			Finalizers: []string{labels.TaskFinalizer},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{
			Phase:          corev1alpha1.TaskPhaseRunning,
			HarnessRuntime: &corev1alpha1.HarnessRuntimeStatus{RuntimeName: "codex"},
			Execution:      &corev1alpha1.TaskExecutionStatus{State: corev1alpha1.TaskExecutionStateRunning},
		},
	}
	bound := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "bound", UID: "bound-uid", Generation: 1,
			Finalizers: []string{labels.TaskFinalizer},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:       corev1alpha1.TaskTypeAgent,
			SessionRef: &corev1alpha1.SessionReference{Name: "chat"},
		},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhasePending,
			AgentExecutionBinding: &corev1alpha1.AgentExecutionBinding{
				SchemaVersion: 1, Mode: corev1alpha1.AgentExecutionBindingModeExecute,
				ContractVersion:      corev1alpha1.AgentRuntimeContractHarnessV2,
				Backend:              corev1alpha1.AgentExecutionBackendRuntimePool,
				Provenance:           corev1alpha1.AgentExecutionProvenanceLegacyAdopted,
				BindingDigest:        "sha256:" + strings.Repeat("1", 64),
				RuntimeType:          corev1alpha1.AgentRuntimeCodex,
				RuntimeProfileDigest: "sha256:" + strings.Repeat("2", 64),
				Snapshot: corev1alpha1.AgentExecutionSnapshotRef{
					ID:     "bound-uid/sha256:" + strings.Repeat("3", 64),
					Digest: "sha256:" + strings.Repeat("3", 64), SchemaVersion: 1,
				},
			},
		},
	}
	session := &corev1alpha1.RuntimeSessionControl{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "session-control", UID: "session-control-uid"},
		Spec:       corev1alpha1.RuntimeSessionControlSpec{SessionName: "chat", SessionUID: "session-uid"},
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "namespace-uid"}}

	client := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(
			&corev1alpha1.AgentExecutionControl{}, &corev1alpha1.Task{}, &corev1alpha1.RuntimeSessionControl{},
		).
		WithObjects(control, deleting, mixed, bound, session, namespace).Build()
	reconciler := &AgentExecutionClassificationReconciler{
		Client: client, APIReader: client, Interval: time.Nanosecond,
		StabilityDelay: time.Second, Now: func() time.Time { return now },
	}
	request := ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: corev1alpha1.AgentExecutionControlNamespace,
		Name:      corev1alpha1.AgentExecutionControlName,
	}}
	for range 4 {
		if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
	}

	gotDeleting := &corev1alpha1.Task{}
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "deleting"}, gotDeleting); err != nil {
		t.Fatal(err)
	}
	if gotDeleting.Status.AgentExecutionNoExecution == nil ||
		gotDeleting.Status.AgentExecutionNoExecution.State != corev1alpha1.AgentExecutionNoExecutionUnbound {
		t.Fatalf("deleting disposition = %#v", gotDeleting.Status.AgentExecutionNoExecution)
	}
	gotMixed := &corev1alpha1.Task{}
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "mixed"}, gotMixed); err != nil {
		t.Fatal(err)
	}
	if gotMixed.Status.AgentExecutionQuarantine == nil ||
		gotMixed.Status.AgentExecutionQuarantine.Reason != corev1alpha1.AgentExecutionQuarantineMixedEvidence {
		t.Fatalf("mixed disposition = %#v", gotMixed.Status.AgentExecutionQuarantine)
	}
	gotSession := &corev1alpha1.RuntimeSessionControl{}
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "session-control"}, gotSession); err != nil {
		t.Fatal(err)
	}
	if gotSession.Status.Lineage == nil ||
		gotSession.Status.Lineage.ContractVersion != corev1alpha1.AgentRuntimeContractHarnessV2 ||
		gotSession.Status.Lineage.Provenance != corev1alpha1.RuntimeSessionLineageLegacyAdopted {
		t.Fatalf("Session lineage = %#v", gotSession.Status.Lineage)
	}

	now = now.Add(2 * time.Second)
	for range 2 {
		if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatalf("stable Reconcile() error = %v", err)
		}
	}
	gotControl := &corev1alpha1.AgentExecutionControl{}
	if err := client.Get(context.Background(), request.NamespacedName, gotControl); err != nil {
		t.Fatal(err)
	}
	if !sealedAgentExecutionClassification(gotControl) {
		t.Fatalf("classification status = %#v, want exact Sealed marker", gotControl.Status.Classification)
	}
}
