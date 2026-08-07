package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

func TestAgentExecutionClassificationReadinessReady(t *testing.T) {
	checker := classificationReadinessForTest(t,
		classifiedReadinessAgent(),
		classifiedReadinessTask(true),
		classifiedReadinessSession(true),
	)
	if err := checker.Check(context.Background()); err != nil {
		t.Fatalf("Check() error = %v", err)
	}
}

func TestAgentExecutionClassificationReadinessAcceptsAbsentFirstUseSessionControl(t *testing.T) {
	checker := classificationReadinessForTest(t,
		classifiedReadinessAgent(),
		classifiedReadinessTask(true),
	)
	if err := checker.Check(context.Background()); err != nil {
		t.Fatalf("Check() error = %v", err)
	}
}

func TestAgentExecutionClassificationReadinessAcceptsImmutableDispositions(t *testing.T) {
	now := metav1.NewTime(time.Date(2026, 8, 6, 1, 0, 0, 0, time.UTC))
	quarantined := classifiedReadinessTask(false)
	quarantined.Name = "quarantined"
	quarantined.Spec.SessionRef = nil
	quarantined.Status.AgentExecutionBinding = nil
	quarantined.Status.AgentExecutionQuarantine = &corev1alpha1.AgentExecutionQuarantine{
		SchemaVersion: 1, Reason: corev1alpha1.AgentExecutionQuarantineMixedEvidence,
		MigrationInventoryID: "sealed-1", V1EvidenceDigest: "sha256:" + strings.Repeat("1", 64),
		V2EvidenceDigest: "sha256:" + strings.Repeat("2", 64), RecordedAt: now,
	}
	noExecution := classifiedReadinessTask(false)
	noExecution.Name = "no-execution"
	noExecution.Spec.SessionRef = nil
	noExecution.Status.AgentExecutionBinding = nil
	noExecution.Status.AgentExecutionNoExecution = &corev1alpha1.AgentExecutionNoExecution{
		SchemaVersion: 1, State: corev1alpha1.AgentExecutionNoExecutionUnbound,
		MigrationInventoryID: "sealed-1", EvidenceDigest: "sha256:" + strings.Repeat("3", 64),
		RecordedAt: now,
	}
	checker := classificationReadinessForTest(t, classifiedReadinessAgent(), quarantined, noExecution)
	if err := checker.Check(context.Background()); err != nil {
		t.Fatalf("Check() error = %v", err)
	}
}

func TestAgentExecutionClassificationReadinessRejectsIncompleteInventory(t *testing.T) {
	tests := []struct {
		name    string
		objects []client.Object
		want    string
	}{
		{
			name: "built-in Agent",
			objects: []client.Object{&corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "unclassified", Namespace: "default"},
				Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{
					Type: corev1alpha1.AgentRuntimeCodex,
				}},
			}},
			want: "unclassified built-in runtime",
		},
		{
			name: "AgentRuntime",
			objects: []client.Object{&corev1alpha1.AgentRuntime{
				ObjectMeta: metav1.ObjectMeta{Name: "unclassified", Namespace: "default"},
			}},
			want: "has no contract classification",
		},
		{
			name: "active Task",
			objects: []client.Object{&corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: "unclassified", Namespace: "default", Finalizers: []string{labels.TaskFinalizer}},
				Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
				Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
			}},
			want: "has no binding, no-execution disposition, or quarantine",
		},
		{
			name:    "referenced Session lineage",
			objects: []client.Object{classifiedReadinessTask(true), classifiedReadinessSession(false)},
			want:    "has no lineage or immutable reconciliation block",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checker := classificationReadinessForTest(t, tt.objects...)
			err := checker.Check(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Check() error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func classificationReadinessForTest(t *testing.T, objects ...client.Object) *AgentExecutionClassificationReadiness {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objects = append(objects, classifiedReadinessControl())
	return &AgentExecutionClassificationReadiness{
		APIReader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
	}
}

func classifiedReadinessControl() *corev1alpha1.AgentExecutionControl {
	now := metav1.NewTime(time.Date(2026, 8, 6, 1, 0, 0, 0, time.UTC))
	return &corev1alpha1.AgentExecutionControl{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: corev1alpha1.AgentExecutionControlNamespace,
			Name:      corev1alpha1.AgentExecutionControlName,
			UID:       "control-uid", Generation: 3,
		},
		Status: corev1alpha1.AgentExecutionControlStatus{
			Classification: &corev1alpha1.AgentExecutionClassificationStatus{
				State:      corev1alpha1.AgentExecutionClassificationSealed,
				ControlUID: "control-uid", ControlGeneration: 3,
				InventoryID: "sealed-1", InventoryDigest: "sha256:" + strings.Repeat("f", 64),
				ObservedAt: now,
			},
		},
	}
}

func classifiedReadinessAgent() *corev1alpha1.Agent {
	return &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "classified", Namespace: "default"},
		Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{
			Type:            corev1alpha1.AgentRuntimeCodex,
			ContractVersion: ptr.To(corev1alpha1.AgentRuntimeContractHarnessV2),
		}},
	}
}

func classifiedReadinessTask(withSession bool) *corev1alpha1.Task {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "classified-task", Namespace: "default", UID: types.UID("task-uid"),
			Finalizers: []string{labels.TaskFinalizer},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhasePending,
			AgentExecutionBinding: &corev1alpha1.AgentExecutionBinding{
				SchemaVersion: 1, ContractVersion: corev1alpha1.AgentRuntimeContractHarnessV2,
			},
		},
	}
	if withSession {
		task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "chat"}
	}
	return task
}

func classifiedReadinessSession(withLineage bool) *corev1alpha1.RuntimeSessionControl {
	control := &corev1alpha1.RuntimeSessionControl{
		ObjectMeta: metav1.ObjectMeta{Name: "session-control", Namespace: "default"},
		Spec:       corev1alpha1.RuntimeSessionControlSpec{SessionName: "chat", SessionUID: "session-uid"},
	}
	if withLineage {
		control.Status.Lineage = &corev1alpha1.RuntimeSessionLineageStatus{
			NamespaceUID: types.UID("namespace-uid"), SessionUID: "session-uid",
			ContractVersion: corev1alpha1.AgentRuntimeContractHarnessV2, Generation: 1,
			RuntimeIdentity: "codex", ConfigDigest: "sha256:" + strings.Repeat("a", 64),
			Provenance:    corev1alpha1.RuntimeSessionLineageFirstUse,
			EstablishedAt: metav1.NewTime(time.Date(2026, 8, 6, 1, 0, 0, 0, time.UTC)),
		}
	}
	return control
}
