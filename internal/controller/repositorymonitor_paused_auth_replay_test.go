package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/workerenv"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type repositoryMonitorPausedAuthFixture struct {
	r            *RepositoryMonitorReconciler
	monitor      *corev1alpha1.RepositoryMonitor
	task         *corev1alpha1.Task
	paused       atomic.Bool
	edited       atomic.Bool
	scanError    atomic.Bool
	deleteError  atomic.Bool
	publications atomic.Int64
}

func newRepositoryMonitorPausedAuthFixture(t *testing.T) *repositoryMonitorPausedAuthFixture {
	t.Helper()
	db := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	monitor, secret := repositoryMonitorInventoryTestObjects("paused-auth-replay")
	enabled, requirePlan := false, false
	monitor.Spec.Targets.PullRequests.Enabled = &enabled
	monitor.Spec.Targets.Issues.Enabled = true
	monitor.Spec.Agents.Implementer = &corev1alpha1.AgentReference{Name: "implementer"}
	monitor.Spec.IssueWorkflow.Implementation.RequirePlan = &requirePlan
	configureRepositoryMonitorTestWriteCredentials(monitor)
	f := &repositoryMonitorPausedAuthFixture{monitor: monitor}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/repos/orka-agents/orka/issues/42":
			title, issueLabels := "Fix issue", []map[string]string{{"name": "bug"}}
			if f.edited.Load() {
				title = "Edited issue"
			}
			if f.paused.Load() {
				issueLabels = append(issueLabels, map[string]string{"name": "orka:pause"})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 42, "title": title, "body": "Keep scope small", "state": "open", "labels": issueLabels})
		case req.Method == http.MethodGet && req.URL.Path == "/repos/orka-agents/orka/pulls":
			_, _ = w.Write([]byte(`[]`))
		case req.Method == http.MethodPost && req.URL.Path == "/repos/orka-agents/orka/pulls":
			if f.paused.Load() {
				t.Error("paused result attempted PR publication")
			}
			f.publications.Add(1)
			_, _ = w.Write([]byte(`{"number":142,"html_url":"https://github.com/orka-agents/orka/pull/142"}`))
		case req.Method == http.MethodPost && req.URL.Path == "/repos/orka-agents/orka/issues/42/comments":
			_, _ = w.Write([]byte(`{"id":4201,"html_url":"https://github.com/orka-agents/orka/issues/42#issuecomment-4201"}`))
		case req.Method == http.MethodPatch && req.URL.Path == "/repos/orka-agents/orka/issues/comments/4201":
			_, _ = w.Write([]byte(`{"id":4201,"html_url":"https://github.com/orka-agents/orka/issues/42#issuecomment-4201"}`))
		case req.Method == http.MethodPost && req.URL.Path == "/graphql":
			_, _ = w.Write([]byte(`{"data":{"repository":{"nameWithOwner":"orka-agents/orka","pullRequest":{"id":"PR_142","number":142,"url":"https://github.com/orka-agents/orka/pull/142","state":"OPEN","mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","reviewDecision":"APPROVED","headRefOid":"head42"}}}}`))
		default:
			t.Errorf("unexpected GitHub request: %s %s", req.Method, req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}, &corev1alpha1.Task{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
				if _, ok := obj.(*corev1.Secret); ok && key.Name == "paused-runtime-auth" && f.scanError.Load() {
					return errors.New("runtime auth lookup unavailable")
				}
				return c.Get(ctx, key, obj, opts...)
			},
			Delete: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.DeleteOption) error {
				if obj.GetName() == "paused-runtime-auth" && f.deleteError.Swap(false) {
					return errors.New("runtime auth deletion unavailable")
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).Build()
	f.r = &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: db, ResultStore: db, ArtifactStore: db, GitHubAPIBaseURL: server.URL}
	command := &store.CommandEvent{ID: "paused-implement", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, Number: 42, Intent: repositoryMonitorCommandIntentImplement, Status: repositoryMonitorCommandAccepted}
	if err := db.CreateCommandEvent(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	item := f.item(t)
	if item.WorkflowPhase != repositoryMonitorIssuePhaseImplementationQueued {
		t.Fatalf("implementation was not queued: phase=%q", item.WorkflowPhase)
	}
	var task corev1alpha1.Task
	if err := cl.Get(t.Context(), types.NamespacedName{Namespace: monitor.Namespace, Name: item.LastActionTaskName}, &task); err != nil {
		t.Fatal(err)
	}
	// Model a builtin ACP Task persisted before monitor Task creation became
	// credential-free. Its immutable auth binding remains required by ingestion.
	immutable := true
	snapshot := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "paused-runtime-auth", Namespace: monitor.Namespace, UID: "uid-paused-runtime-auth", Labels: map[string]string{labels.LabelCreatedBy: repositoryMonitorTaskCreatedBy, labels.LabelRepositoryMonitor: labels.SelectorValue(monitor.Name)}, Annotations: map[string]string{repositoryMonitorIssueAnnotationRuntimeAuthTask: task.Name}}, Immutable: &immutable, Data: map[string][]byte{workerenv.OpenAIAPIKey: []byte("runtime-test-placeholder")}}
	if err := controllerutil.SetControllerReference(monitor, snapshot, scheme); err != nil {
		t.Fatal(err)
	}
	if err := cl.Create(t.Context(), snapshot); err != nil {
		t.Fatal(err)
	}
	task.Spec.SecretRef = &corev1alpha1.SecretReference{Name: snapshot.Name}
	task.Annotations[repositoryMonitorIssueAnnotationRuntimeAuthUID] = string(snapshot.UID)
	task.Annotations[repositoryMonitorIssueAnnotationRuntimeAuthFields] = workerenv.OpenAIAPIKey
	task.Annotations[repositoryMonitorIssueAnnotationRuntimeAgentGeneration] = "0"
	if err := cl.Update(t.Context(), &task); err != nil {
		t.Fatal(err)
	}
	f.task = &task
	f.paused.Store(true)
	f.inventory(t, "pause-before-completion")
	result := fmt.Appendf(nil, `{"schemaVersion":"orka.issueImplementation.v1","issueNumber":42,"snapshotDigest":%q,"status":"patch_ready","summary":"Implemented the requested change."}`, item.SnapshotDigest)
	if err := db.SaveResult(t.Context(), task.Namespace, task.Name, result); err != nil {
		t.Fatal(err)
	}
	markRepositoryMonitorTestTaskDelivered(t, t.Context(), cl, task.Name, task.Spec.Workspace.PushBranch, "head42")
	return f
}

func (f *repositoryMonitorPausedAuthFixture) reconcile(t *testing.T) {
	t.Helper()
	if _, err := f.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: f.monitor.Namespace, Name: f.monitor.Name}}); err != nil {
		t.Fatal(err)
	}
}

func (f *repositoryMonitorPausedAuthFixture) inventory(t *testing.T, id string) {
	t.Helper()
	if err := f.r.Store.CreateMonitorRun(t.Context(), &store.MonitorRun{ID: id, MonitorNamespace: f.monitor.Namespace, MonitorName: f.monitor.Name, TargetKind: repositoryMonitorIssueKind, TargetNumber: 42, Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
}

func (f *repositoryMonitorPausedAuthFixture) item(t *testing.T) *store.MonitorItem {
	t.Helper()
	item, err := f.r.Store.GetMonitorItem(t.Context(), f.monitor.Namespace, f.monitor.Name, repositoryMonitorIssueKind, "42")
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func (f *repositoryMonitorPausedAuthFixture) assertSnapshot(t *testing.T, present bool) {
	t.Helper()
	var snapshot corev1.Secret
	err := f.r.Get(t.Context(), types.NamespacedName{Namespace: f.monitor.Namespace, Name: f.task.Spec.SecretRef.Name}, &snapshot)
	if present && err != nil || !present && !apierrors.IsNotFound(err) {
		t.Fatalf("runtime auth snapshot present=%v, lookup error=%v", present, err)
	}
}

func TestRepositoryMonitorPausedImplementationRetainsAuthForReplay(t *testing.T) {
	f := newRepositoryMonitorPausedAuthFixture(t)
	for range 2 {
		f.reconcile(t)
		item := f.item(t)
		if item.WorkflowPhase != repositoryMonitorIssuePhasePaused || item.LastVerdict != repositoryMonitorIssuePhasePatchReady || f.publications.Load() != 0 {
			t.Fatalf("paused implementation result changed: phase=%q verdict=%q publications=%d", item.WorkflowPhase, item.LastVerdict, f.publications.Load())
		}
		f.assertSnapshot(t, true)
	}
	// Retryable credential reads still fail closed, without retiring the binding.
	f.scanError.Store(true)
	if _, err := f.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: f.monitor.Namespace, Name: f.monitor.Name}}); err == nil {
		t.Fatal("credential lookup failure did not stop replay")
	}
	f.scanError.Store(false)
	f.assertSnapshot(t, true)
	f.paused.Store(false)
	originalStore := f.r.Store
	f.r.Store = pausedAuthItemWriteErrorStore{RepositoryMonitorStore: originalStore, phase: repositoryMonitorIssuePhasePROpened}
	f.inventory(t, "unpause-write-failed")
	f.assertSnapshot(t, true)
	if f.item(t).WorkflowPhase != repositoryMonitorIssuePhasePaused {
		t.Fatal("failed item persistence dropped the paused result")
	}
	f.r.Store = originalStore
	f.inventory(t, "unpause-publication")
	item := f.item(t)
	if item.WorkflowPhase != repositoryMonitorIssuePhasePROpened || item.LinkedPRNumber != 142 || f.publications.Load() != 1 {
		t.Fatalf("retained result did not publish on unpause: phase=%q linkedPR=%d publications=%d", item.WorkflowPhase, item.LinkedPRNumber, f.publications.Load())
	}
	f.assertSnapshot(t, false)
	f.reconcile(t)
	if f.publications.Load() != 1 {
		t.Fatal("settled implementation was published twice")
	}
}

func TestRepositoryMonitorPausedImplementationAuthCleanupAfterTerminalization(t *testing.T) {
	for _, outcome := range []string{"failed", "superseded", "superseded_retry"} {
		t.Run(outcome, func(t *testing.T) {
			f := newRepositoryMonitorPausedAuthFixture(t)
			if outcome == "failed" {
				var task corev1alpha1.Task
				if err := f.r.Get(t.Context(), types.NamespacedName{Namespace: f.task.Namespace, Name: f.task.Name}, &task); err != nil {
					t.Fatal(err)
				}
				task.Status.Phase = corev1alpha1.TaskPhaseFailed
				if err := f.r.Status().Update(t.Context(), &task); err != nil {
					t.Fatal(err)
				}
				f.reconcile(t)
			} else {
				f.reconcile(t)
				f.assertSnapshot(t, true)
				f.edited.Store(true)
				f.paused.Store(false)
				if outcome == "superseded_retry" {
					originalStore := f.r.Store
					f.r.Store = pausedAuthItemWriteErrorStore{RepositoryMonitorStore: originalStore, phase: repositoryMonitorIssuePhaseDiscovered}
					f.inventory(t, "supersede-failed")
					f.assertSnapshot(t, true)
					if f.item(t).WorkflowPhase != repositoryMonitorIssuePhasePaused {
						t.Fatal("failed supersession dropped the retained result")
					}
					f.r.Store = originalStore
				}
				f.inventory(t, "supersede-result")
				f.assertSnapshot(t, true)
				f.reconcile(t)
			}
			f.assertSnapshot(t, false)
			if f.publications.Load() != 0 || f.item(t).WorkflowPhase == repositoryMonitorIssuePhasePaused {
				t.Fatal("terminal implementation retained paused work or published a PR")
			}
		})
	}
}

func TestRepositoryMonitorSupersededAuthCleanupRetriesWithRetainedTask(t *testing.T) {
	f := newRepositoryMonitorPausedAuthFixture(t)
	f.reconcile(t)
	f.edited.Store(true)
	f.paused.Store(false)
	f.deleteError.Store(true)
	f.inventory(t, "supersede-result")
	item := f.item(t)
	if item.WorkflowPhase == repositoryMonitorIssuePhasePaused || item.LastActionTaskName != "" || item.LastActionID != "" {
		t.Fatal("supersession did not durably retire the old Task identity")
	}
	f.assertSnapshot(t, true)
	if _, err := f.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: f.monitor.Namespace, Name: f.monitor.Name}}); err == nil {
		t.Fatal("transient snapshot deletion failure did not request a retry")
	}
	f.assertSnapshot(t, true)
	f.reconcile(t)
	f.assertSnapshot(t, false)
	var task corev1alpha1.Task
	if err := f.r.Get(t.Context(), types.NamespacedName{Namespace: f.task.Namespace, Name: f.task.Name}, &task); err != nil {
		t.Fatalf("cleanup removed the retained Task: %v", err)
	}
	if f.publications.Load() != 0 {
		t.Fatal("superseded implementation was published")
	}
}

type pausedAuthItemWriteErrorStore struct {
	store.RepositoryMonitorStore
	phase string
}

func (s pausedAuthItemWriteErrorStore) UpsertMonitorItem(ctx context.Context, item *store.MonitorItem) error {
	if item.WorkflowPhase == s.phase {
		return errors.New("item persistence unavailable")
	}
	return s.RepositoryMonitorStore.UpsertMonitorItem(ctx, item)
}
