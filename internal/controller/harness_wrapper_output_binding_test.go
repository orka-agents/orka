package controller

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
	sqlitestore "github.com/orka-agents/orka/internal/store/sqlite"
)

func TestSaveHarnessWrapperResultUsesPlannedAttemptProvenance(t *testing.T) {
	db, err := sqlitestore.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := sqlitestore.NewStore(db, ":memory:")
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name: "security-task", Namespace: "ns", UID: types.UID("task-uid"),
		Labels: map[string]string{labels.LabelCreatedBy: repositorySecurityCreatedBy},
		Annotations: map[string]string{
			harnessWrapperRuntimeAnnotation: "runtime-1", harnessWrapperTurnIDAnnotation: "turn-1",
			harnessWrapperCorrelationIDAnno: "corr-1", harnessWrapperAttemptAnno: "1",
		},
	}}
	r := &TaskReconciler{
		ResultStore: s,
		SecurityIntegrityConfig: security.IntegrityConfig{
			WorkerOutputBindingMode: security.WorkerOutputBindingAudit,
		},
	}
	if err := r.saveHarnessWrapperResult(context.Background(), task, []byte("result")); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBoundResult(context.Background(), "ns", task.Name, string(task.UID), 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Provenance.ProducerKind != store.OutputProducerControllerHarness || string(got.Data) != "result" {
		t.Fatalf("bound harness result = %#v", got)
	}
}

func TestHarnessWrapperOutputAttemptIgnoresStalePlannedAttempt(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{harnessWrapperAttemptAnno: "2"}},
		Status:     corev1alpha1.TaskStatus{Attempts: 3},
	}
	if got := harnessWrapperOutputAttempt(task); got != 3 {
		t.Fatalf("harnessWrapperOutputAttempt() = %d, want 3", got)
	}
}

func TestSaveHarnessWrapperResultUsesLegacyStoreForOrdinaryTask(t *testing.T) {
	db, err := sqlitestore.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := sqlitestore.NewStore(db, ":memory:")
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name: "ordinary-task", Namespace: "ns", UID: types.UID("ordinary-uid"),
		Annotations: map[string]string{
			harnessWrapperRuntimeAnnotation: "runtime-1", harnessWrapperTurnIDAnnotation: "turn-1",
			harnessWrapperCorrelationIDAnno: "corr-1", harnessWrapperAttemptAnno: "1",
		},
	}}
	r := &TaskReconciler{ResultStore: s}
	if err := r.saveHarnessWrapperResult(context.Background(), task, []byte("result")); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetResult(context.Background(), "ns", task.Name)
	if err != nil || string(got) != "result" {
		t.Fatalf("legacy harness result = %q, %v", got, err)
	}
}
