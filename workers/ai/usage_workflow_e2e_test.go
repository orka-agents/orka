package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/api"
	"github.com/orka-agents/orka/internal/controller"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/harness/v2/eventjournal"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/usage"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/workers/common"
)

const (
	usageWorkflowNamespace = "usage-workflow"
	usageWorkflowRepo      = "usage-fixture/repo"
	usageWorkflowModel     = "usage-fixture-model"
)

// This workflow uses the production monitor, AI model loop, ACP journal,
// authenticated HTTP routes, Task finalizer, and on-disk SQLite store. Kubernetes,
// provider responses, GitHub, and the publisher's verified delivery are fixtures;
// no usage observations or PR outcomes are inserted directly into the store.
func TestUsageIssueToMergedPRWorkflowE2E(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "orka")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	output, err := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../cmd/cli").CombinedOutput()
	require.NoError(t, err, "%s", output)
	for _, phase := range []corev1alpha1.TaskPhase{corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed} {
		t.Run(string(phase), func(t *testing.T) { testUsageIssueWorkflow(t, phase, binary) })
	}
}

func testUsageIssueWorkflow(t *testing.T, workerPhase corev1alpha1.TaskPhase, binary string) {
	f := newUsageWorkflowFixture(t)
	t.Log("issue webhook queues implementation and establishes the work request")
	f.submitIssueWebhook(t, http.StatusCreated)
	f.reconcileMonitor(t)
	var tasks corev1alpha1.TaskList
	require.NoError(t, f.kube.List(t.Context(), &tasks, client.InNamespace(usageWorkflowNamespace)))
	require.Len(t, tasks.Items, 1)
	implementation := tasks.Items[0].DeepCopy()
	require.NotEmpty(t, implementation.UID)
	require.Equal(t, corev1alpha1.TaskTypeAgent, implementation.Spec.Type)
	f.reconcileTask(t, implementation)

	t.Log("cumulative ACP usage and delegated AI calls reach the public report")
	journalEvents := f.recordACPUsage(t, implementation)
	child, replay := f.runDelegatedWorker(t, implementation, workerPhase)
	beforePR, _ := f.report(t, "")
	require.Equal(t, 1, beforePR.Summary.WorkRequests)
	assertUsageWorkflowTotals(t, beforePR.Summary, workerPhase)
	require.Zero(t, beforePR.Summary.PRsOpened)
	require.Nil(t, beforePR.Summary.TokensPerPRMerged)

	t.Log("verified publication creates one PR and GitHub confirms current readiness")
	f.deliverImplementation(t, implementation)
	for range 3 {
		f.reconcileMonitor(t)
	}
	require.EqualValues(t, 1, f.github.creates.Load())
	opened, openedSelection := f.report(t, "")
	require.Equal(t, 1, opened.Summary.PRsOpened)
	require.Equal(t, 1, opened.Summary.PRsReady)
	require.Zero(t, opened.Summary.PRsMerged)
	require.Equal(t, 1920.0, *opened.Summary.TokensPerPROpened)
	require.Nil(t, opened.Summary.TokensPerPRMerged)
	openedWork := f.work(t, opened.Works[0].ID, openedSelection)
	require.Equal(t, opened.Summary, openedWork.Summary)
	require.Len(t, openedWork.Tasks, 2)
	require.Len(t, openedWork.PullRequests, 1)
	require.Equal(t, store.UsagePRCreated, openedWork.PullRequests[0].Origin)
	require.Equal(t, "PR_usage_177", openedWork.PullRequests[0].GitHubID)

	t.Log("API and controller replacement retain reports and reject double charging on replay")
	f.restart(t)
	f.assertReport(t, opened, openedWork, openedSelection)
	f.replayWorkerUsage(t, child, replay)
	f.appendACPEvents(t, implementation, journalEvents, true)
	f.submitIssueWebhook(t, http.StatusAccepted)
	f.reconcileMonitor(t)
	f.assertReport(t, opened, openedWork, openedSelection)
	current, _ := f.report(t, "")
	require.Equal(t, opened.Summary, current.Summary)
	require.EqualValues(t, 1, f.github.creates.Load())

	t.Log("a later merge changes fresh reports and preserves the earlier report snapshot")
	f.github.merged.Store(true)
	f.backend.refreshNow = true
	f.reconcileMonitor(t)
	merged, mergedSelection := f.report(t, "")
	require.Equal(t, 1, merged.Summary.PRsMerged)
	require.Zero(t, merged.Summary.PRsReady)
	require.Equal(t, 1920.0, *merged.Summary.TokensPerPRMerged)
	f.assertReport(t, opened, openedWork, openedSelection)
	f.assertDenied(t, opened.Works[0].ID)

	t.Log("Task finalization and cleanup erase execution data but retain usage and PR outcomes")
	f.finishWorker(t, child, workerPhase)
	f.reconcileTask(t, implementation)
	retained, retainedSelection := f.report(t, "")
	assertUsageWorkflowTotals(t, retained.Summary, workerPhase)
	require.Equal(t, 1, retained.Summary.PRsMerged)
	require.NotNil(t, retained.Summary.TokensPerPRMerged)
	require.Equal(t, 1920.0, *retained.Summary.TokensPerPRMerged)
	retainedWork := f.work(t, retained.Works[0].ID, retainedSelection)
	require.Zero(t, retained.Summary.UnfinishedWork)
	for _, task := range []*corev1alpha1.Task{child, implementation} {
		f.deleteTask(t, task)
	}
	f.restart(t)
	f.reconcileMonitor(t)
	f.assertReport(t, opened, openedWork, openedSelection)
	f.assertReport(t, retained, retainedWork, retainedSelection)
	current, _ = f.report(t, "")
	require.Equal(t, retained.Summary, current.Summary)
	require.EqualValues(t, 1, f.github.creates.Load())
	f.assertCLIReport(t, binary, retained, retainedSelection)
	// The merge snapshot is also readable independently of the final Task phase.
	got, _ := f.report(t, mergedSelection)
	require.Equal(t, merged.Summary, got.Summary)
}

func assertUsageWorkflowTotals(t *testing.T, summary usage.Summary, phase corev1alpha1.TaskPhase) {
	t.Helper()
	require.EqualValues(t, 1600, summary.InputTokens)
	require.EqualValues(t, 320, summary.OutputTokens)
	require.EqualValues(t, 270, summary.CachedInputTokens)
	require.EqualValues(t, 1920, summary.TotalTokens)
	if phase == corev1alpha1.TaskPhaseFailed {
		require.Equal(t, "partial", summary.Completeness)
		require.Equal(t, 1, summary.MissingMeasurements)
	} else {
		require.Equal(t, "complete", summary.Completeness)
		require.Zero(t, summary.MissingMeasurements)
	}
}

type usageWorkflowStore struct {
	*sqlite.Store
	refreshNow bool
	apiReady   atomic.Bool
}

func (s *usageWorkflowStore) HealthCheck(ctx context.Context) error {
	if err := s.Store.HealthCheck(ctx); err != nil {
		return err
	}
	s.apiReady.Store(true)
	return nil
}

func (s *usageWorkflowStore) ListUsagePullRequestLinks(
	ctx context.Context, namespace, monitorUID string, before time.Time, limit int,
) ([]store.UsagePRLink, error) {
	// Advance only the poll eligibility cutoff, avoiding a five-minute sleep.
	// Selection, GitHub verification, observation times, and storage stay real.
	if s.refreshNow {
		before = time.Now().UTC().Add(time.Second)
	}
	return s.Store.ListUsagePullRequestLinks(ctx, namespace, monitorUID, before, limit)
}

type usageWorkflowFixture struct {
	kube       client.Client
	clientset  *kubefake.Clientset
	scheme     *runtime.Scheme
	db         *sql.DB
	dbPath     string
	backend    *usageWorkflowStore
	monitor    *corev1alpha1.RepositoryMonitor
	reconciler *controller.RepositoryMonitorReconciler
	tasks      *controller.TaskReconciler
	github     *usageWorkflowGitHub
	url        string
	stopAPI    context.CancelFunc
	apiDone    chan error
	startedAt  time.Time
	users      sync.Map
	viewer     string
	outsider   string
}

func newUsageWorkflowFixture(t *testing.T) *usageWorkflowFixture {
	t.Helper()
	f := &usageWorkflowFixture{
		dbPath: filepath.Join(t.TempDir(), "usage.db"), scheme: runtime.NewScheme(),
		startedAt: time.Now().UTC().Add(-time.Minute),
		viewer:    "fixture-viewer-" + t.Name(), outsider: "fixture-outsider-" + t.Name(),
	}
	for _, add := range []func(*runtime.Scheme) error{
		corev1alpha1.AddToScheme, corev1.AddToScheme, batchv1.AddToScheme, rbacv1.AddToScheme, authenticationv1.AddToScheme,
	} {
		require.NoError(t, add(f.scheme))
	}
	f.github = newUsageWorkflowGitHub(t)
	t.Setenv("ORKA_GITHUB_WEBHOOK_SECRET", "usage-webhook-fixture")
	t.Setenv("ORKA_GITHUB_API_BASE_URL", f.github.server.URL)
	t.Setenv("ORKA_GITHUB_LABEL_TRIGGER_NAMESPACE", usageWorkflowNamespace)
	f.monitor = &corev1alpha1.RepositoryMonitor{
		ObjectMeta: metav1.ObjectMeta{
			Name: "issue-delivery", Namespace: usageWorkflowNamespace, UID: "monitor-uid",
			CreationTimestamp: metav1.NewTime(f.startedAt),
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL: "https://github.com/" + usageWorkflowRepo,
			Agents: corev1alpha1.RepositoryMonitorAgents{
				Reviewer:    &corev1alpha1.AgentReference{Name: "reviewer"},
				Implementer: &corev1alpha1.AgentReference{Name: "implementer"},
			},
			ReadCredentialRef:            &corev1.LocalObjectReference{Name: "source-read"},
			PublicationReadCredentialRef: &corev1.LocalObjectReference{Name: "publication-read"},
			PublicationCredentialRef:     &corev1.LocalObjectReference{Name: "publication-write"},
			ForgeCredentialRef:           &corev1.LocalObjectReference{Name: "forge"},
		},
	}
	f.monitor.Spec.Targets.PullRequests.Enabled = new(false)
	f.monitor.Spec.Targets.Issues.Enabled = true
	f.monitor.Spec.Triggers.GitHub.Labels.Enabled = true
	f.monitor.Spec.IssueWorkflow.Implementation.RequireApprovedPlan = new(false)
	objects := make([]client.Object, 0, 8)
	objects = append(objects,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: usageWorkflowNamespace, UID: "namespace-uid", CreationTimestamp: metav1.NewTime(f.startedAt),
		}},
		f.monitor,
	)
	for _, name := range []string{"source-read", "publication-read", "publication-write", "forge"} {
		objects = append(objects, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: usageWorkflowNamespace, UID: types.UID("uid-" + name)},
			Data:       map[string][]byte{"token": []byte(name + "-fixture")}, Immutable: new(true),
		})
	}
	for _, name := range []string{"reviewer", "implementer"} {
		runtimeType := corev1alpha1.AgentRuntimeCodex
		if name == "reviewer" {
			runtimeType = corev1alpha1.AgentRuntimeClaude
		}
		objects = append(objects, &corev1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: usageWorkflowNamespace},
			Spec:       corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{Type: runtimeType}},
		})
	}
	f.users.Store(f.viewer, usageWorkflowUser(usageWorkflowNamespace, "viewer"))
	f.users.Store(f.outsider, usageWorkflowUser("other-team", "viewer"))
	f.kube = fake.NewClientBuilder().WithScheme(f.scheme).WithObjects(objects...).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}, &corev1alpha1.Task{}, &batchv1.Job{}, &corev1.Pod{}).
		WithInterceptorFuncs(interceptor.Funcs{Create: func(
			ctx context.Context, kube client.WithWatch, obj client.Object, opts ...client.CreateOption,
		) error {
			if review, ok := obj.(*authenticationv1.TokenReview); ok {
				if user, exists := f.users.Load(review.Spec.Token); exists {
					review.Status = authenticationv1.TokenReviewStatus{Authenticated: true, User: user.(authenticationv1.UserInfo)}
				}
				return nil
			}
			if obj.GetUID() == "" {
				obj.SetUID(types.UID("uid-" + obj.GetName()))
			}
			if obj.GetCreationTimestamp().Time.IsZero() {
				obj.SetCreationTimestamp(metav1.Now())
			}
			return kube.Create(ctx, obj, opts...)
		}}).Build()
	f.clientset = kubefake.NewClientset()
	f.clientset.PrependReactor("create", "subjectaccessreviews", func(
		action k8stesting.Action,
	) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		review.Status.Allowed = review.Spec.User == usageWorkflowUser(usageWorkflowNamespace, "viewer").Username &&
			review.Spec.ResourceAttributes != nil && review.Spec.ResourceAttributes.Namespace == usageWorkflowNamespace
		return true, review, nil
	})
	t.Cleanup(func() { f.stop(t) })
	f.start(t)
	return f
}

func usageWorkflowUser(namespace, name string) authenticationv1.UserInfo {
	return authenticationv1.UserInfo{
		Username: "system:serviceaccount:" + namespace + ":" + name, UID: name + "-uid",
		Groups: []string{"system:serviceaccounts", "system:serviceaccounts:" + namespace},
	}
}

func (f *usageWorkflowFixture) start(t *testing.T) {
	t.Helper()
	var err error
	f.db, err = sqlite.NewDB(f.dbPath)
	require.NoError(t, err)
	f.backend = &usageWorkflowStore{Store: sqlite.NewStore(f.db, f.dbPath)}
	sessions := controller.NewSessionManager(f.backend)
	f.reconciler = &controller.RepositoryMonitorReconciler{
		Client: f.kube, APIReader: f.kube, Scheme: f.scheme, Store: f.backend, ResultStore: f.backend,
		ArtifactStore: f.backend, GitHubAPIBaseURL: f.github.server.URL, HTTPClient: f.github.server.Client(),
	}
	f.tasks = &controller.TaskReconciler{
		Client: f.kube, APIReader: f.kube, Scheme: f.scheme, KubeClient: f.clientset,
		ExecutionEventStore: f.backend, ResultStore: f.backend, ArtifactStore: f.backend, SessionManager: sessions,
	}
	// Start accepts a port, not a listener. Retry bind collisions, and only
	// accept readiness observed by this store instead of another local server.
	for range 5 {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		port := listener.Addr().(*net.TCPAddr).Port
		server := api.NewServer(f.kube, sessions, api.ServerConfig{
			Port: port, WatchNamespace: usageWorkflowNamespace, EnforceNamespaceIsolation: true,
			Clientset: f.clientset, APIReader: f.kube, ResultStore: f.backend, SessionStore: f.backend,
			RepositoryMonitorStore: f.backend, ExecutionEventStore: f.backend, ArtifactStore: f.backend,
			HealthChecker: f.backend,
		})
		ctx, cancel := context.WithCancel(t.Context())
		f.stopAPI, f.apiDone = cancel, make(chan error, 1)
		done := f.apiDone
		require.NoError(t, listener.Close())
		go func() { done <- server.Start(ctx) }()
		f.url = fmt.Sprintf("http://127.0.0.1:%d", port)
		var startErr error
		var stopped bool
		require.Eventually(t, func() bool {
			select {
			case startErr = <-done:
				stopped = true
				return true
			default:
			}
			response, err := (&http.Client{Timeout: time.Second}).Get(f.url + "/readyz")
			if err != nil {
				return false
			}
			_ = response.Body.Close()
			return response.StatusCode == http.StatusOK && f.backend.apiReady.Load()
		}, 5*time.Second, 10*time.Millisecond)
		if !stopped {
			return
		}
		cancel()
		f.stopAPI, f.apiDone = nil, nil
		if errors.Is(startErr, syscall.EADDRINUSE) {
			continue
		}
		t.Fatalf("API stopped before readiness: %v", startErr)
	}
	t.Fatal("API could not bind a free port after five attempts")
}

func (f *usageWorkflowFixture) stop(t *testing.T) {
	t.Helper()
	if f.stopAPI != nil {
		f.stopAPI()
		select {
		case err := <-f.apiDone:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("API did not stop")
		}
		f.stopAPI = nil
	}
	if f.db != nil {
		require.NoError(t, f.db.Close())
		f.db = nil
	}
}

func (f *usageWorkflowFixture) restart(t *testing.T) {
	t.Helper()
	f.stop(t)
	f.start(t)
}

func (f *usageWorkflowFixture) request(t *testing.T, method, path, bearer string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, f.url+path, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, data
}

func (f *usageWorkflowFixture) submitIssueWebhook(t *testing.T, want int) {
	t.Helper()
	body := []byte(fmt.Sprintf(`{"action":"labeled","label":{"name":"orka:implement"},
		"repository":{"full_name":%q,"html_url":%q,"default_branch":"main"},
		"issue":{"number":77,"state":"open","title":"Implement fixture","body":"Add the fixture change.",
		"html_url":%q,"updated_at":"2026-06-01T00:00:00Z","labels":[{"name":"orka:implement"}]},
		"sender":{"login":"fixture-user"}}`,
		usageWorkflowRepo, "https://github.com/"+usageWorkflowRepo, "https://github.com/"+usageWorkflowRepo+"/issues/77"))
	mac := hmac.New(sha256.New, []byte("usage-webhook-fixture"))
	_, _ = mac.Write(body)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, f.url+"/webhooks/github", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-GitHub-Delivery", "usage-issue-delivery")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, response.Body.Close()) }()
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, want, response.StatusCode, "%s", data)
}

func (f *usageWorkflowFixture) reconcileMonitor(t *testing.T) {
	t.Helper()
	_, err := f.reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.monitor)})
	require.NoError(t, err)
	var current corev1alpha1.RepositoryMonitor
	require.NoError(t, f.kube.Get(t.Context(), client.ObjectKeyFromObject(f.monitor), &current))
	require.NotEqual(t, "Error", current.Status.Phase, "%+v", current.Status.Conditions)
}

func (f *usageWorkflowFixture) reconcileTask(t *testing.T, task *corev1alpha1.Task) {
	t.Helper()
	_, err := f.tasks.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(task)})
	require.NoError(t, err)
}

func (f *usageWorkflowFixture) recordACPUsage(t *testing.T, task *corev1alpha1.Task) []harnessv2.Event {
	t.Helper()
	recorded := make([]harnessv2.Event, 0, 4)
	for i, counts := range [][2]uint64{{1000, 200}, {1500, 300}, {1500, 300}} {
		update := harnessv2.UsageUpdate{
			InputTokens: counts[0], OutputTokens: counts[1], CachedInputTokens: new(uint64(250)),
			Reported: true, Complete: true,
		}
		event := harnessv2.Event{
			Protocol: harnessv2.ProtocolVersion, Type: harnessv2.EventUpdate,
			Identity: harnessv2.EventIdentity{
				RuntimeInstanceID: "fixture-runtime", SupervisorBootID: "fixture-boot", RuntimeSessionUID: "fixture-session",
				RuntimeSessionGeneration: 1, TaskUID: harnessv2.TaskUID(task.UID), TaskAttempt: 1, PromptID: "fixture-prompt",
				Sequence: uint64(i + 2), Timestamp: time.Now().UTC(),
				RequestDigest: harnessv2.RequestDigest("sha256:" + strings.Repeat("a", 64)),
			},
			Update: &harnessv2.UpdateEvent{Kind: harnessv2.UpdateUsage, Usage: &update},
		}
		if i == 0 {
			accepted := event
			accepted.Type, accepted.Update, accepted.Identity.Sequence = harnessv2.EventAccepted, nil, 1
			accepted.Accepted = &harnessv2.AcceptedEvent{
				AcceptedAt: accepted.Identity.Timestamp, ACPVersion: harnessv2.ACPProfileV1,
				Lease: harnessv2.PromptLease{
					Generation: 1, IssuedAt: accepted.Identity.Timestamp, ExpiresAt: accepted.Identity.Timestamp.Add(time.Minute),
				},
			}
			recorded = append(recorded, accepted)
		}
		if i == 2 {
			event.Type, event.Update = harnessv2.EventCompleted, nil
			event.Completed = &harnessv2.CompletedEvent{
				StopReason: harnessv2.ACPStopReasonEndTurn,
				Result: harnessv2.PromptResult{Usage: update,
					Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: "Implementation complete."}}},
			}
		}
		recorded = append(recorded, event)
	}
	f.appendACPEvents(t, task, recorded, false)
	return recorded
}

func (f *usageWorkflowFixture) appendACPEvents(
	t *testing.T, task *corev1alpha1.Task, recorded []harnessv2.Event, replay bool,
) {
	t.Helper()
	state, err := (eventjournal.Journal{EventStore: f.backend, MapContext: eventjournal.MapContext{
		Namespace: task.Namespace, TaskName: task.Name, StreamID: task.Name, Provider: "openai", Model: usageWorkflowModel,
	}}).Open(t.Context())
	require.NoError(t, err)
	for _, event := range recorded {
		var added bool
		switch event.Type {
		case harnessv2.EventCompleted:
			_, added, err = state.AppendTerminalUsageIfNew(t.Context(), event)
			require.NoError(t, err)
			require.Equal(t, !replay, added)
			_, added, err = state.AppendPromptLifecycleIfNew(t.Context(), event)
		case harnessv2.EventAccepted:
			_, added, err = state.AppendPromptLifecycleIfNew(t.Context(), event)
		default:
			_, added, err = state.AppendUpdateIfNew(t.Context(), event)
		}
		require.NoError(t, err)
		require.Equal(t, !replay, added)
	}
}

func (f *usageWorkflowFixture) runDelegatedWorker(
	t *testing.T, parent *corev1alpha1.Task, phase corev1alpha1.TaskPhase,
) (*corev1alpha1.Task, [][]byte) {
	t.Helper()
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "delegated-ai", Namespace: usageWorkflowNamespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(parent, corev1alpha1.GroupVersion.WithKind("Task")),
			},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseRunning, JobName: "delegated-job", JobUID: "delegated-job-uid",
		},
	}
	require.NoError(t, f.kube.Create(t.Context(), child))
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: child.Status.JobName, Namespace: child.Namespace, UID: types.UID(child.Status.JobUID),
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(child, corev1alpha1.GroupVersion.WithKind("Task"))},
	}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "delegated-pod", Namespace: child.Namespace, UID: "delegated-pod-uid",
			Labels:          map[string]string{labels.LabelTask: child.Name},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(job, batchv1.SchemeGroupVersion.WithKind("Job"))}},
		Spec: corev1.PodSpec{
			ServiceAccountName: "worker",
			Containers:         []corev1.Container{{Name: "worker", Command: []string{"/worker"}, Args: []string{"--mode=ai"}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	require.NoError(t, f.kube.Create(t.Context(), job))
	require.NoError(t, f.kube.Create(t.Context(), pod))
	f.reconcileTask(t, child)
	user := usageWorkflowUser(child.Namespace, "worker")
	user.Extra = map[string]authenticationv1.ExtraValue{
		"authentication.kubernetes.io/pod-name": {pod.Name}, "authentication.kubernetes.io/pod-uid": {string(pod.UID)},
	}
	f.users.Store("usage-worker-fixture", user)
	t.Setenv(workerenv.ServiceAccountToken, "")
	bearerPath := filepath.Join(t.TempDir(), "bearer")
	require.NoError(t, os.WriteFile(bearerPath, []byte("usage-worker-fixture"), 0o600))
	transport := &usageWorkflowTransport{base: http.DefaultTransport}
	recorder := common.NewHTTPEventRecorder(common.HTTPEventRecorderConfig{
		ControllerURL: f.url, Namespace: child.Namespace, TaskName: child.Name, BearerPath: bearerPath,
		Client: &http.Client{Transport: transport, Timeout: 5 * time.Second},
	})
	var calls atomic.Int32
	missingFile := filepath.Join(t.TempDir(), "missing")
	providerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			http.Error(w, "unexpected provider request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if phase == corev1alpha1.TaskPhaseFailed {
			if call > 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"type":"error",
					"error":{"type":"invalid_request_error","message":"fixture request rejected"}}`)
				return
			}
			_, _ = fmt.Fprintf(w, `{"id":"fixture-tool","type":"message","role":"assistant","model":"usage-fixture-model",
				"content":[{"type":"tool_use","id":"read-fixture","name":"file_read","input":{"path":%q}}],"stop_reason":"tool_use",
				"usage":{"input_tokens":80,"output_tokens":20,"cache_read_input_tokens":20,"cache_creation_input_tokens":0}}`,
				missingFile)
			return
		}
		_, _ = io.WriteString(w, `{"id":"fixture-message","type":"message","role":"assistant","model":"usage-fixture-model",
			"content":[{"type":"text","text":"delegated work complete"}],"stop_reason":"end_turn",
			"usage":{"input_tokens":80,"output_tokens":20,"cache_read_input_tokens":20,"cache_creation_input_tokens":0}}`)
	}))
	defer providerServer.Close()
	provider, err := llm.NewProvider("anthropic", llm.ProviderConfig{
		APIKey: "provider-fixture", BaseURL: providerServer.URL,
	})
	require.NoError(t, err)
	t.Setenv(workerenv.TaskName, child.Name)
	t.Setenv(workerenv.TaskNamespace, child.Namespace)
	result, err := executeAgentLoopWithEvents(withWorkerUsage(t.Context(), recorder), llm.NewRetryProvider(provider),
		[]llm.Message{{Role: "user", Content: "Perform the delegated work."}}, "", usageWorkflowModel,
		modelSettings{maxTokens: 128}, buildLLMTools([]string{"file_read"}, nil), nil, nil, recorder)
	wantCalls := 1
	if phase == corev1alpha1.TaskPhaseFailed {
		wantCalls = 2
		require.ErrorContains(t, err, "fixture request rejected")
	} else {
		require.NoError(t, err)
		require.Equal(t, "delegated work complete", result)
	}
	require.EqualValues(t, wantCalls, calls.Load())
	transport.mu.Lock()
	defer transport.mu.Unlock()
	require.Len(t, transport.bodies, 2*wantCalls, "started and terminal usage must traverse HTTP for every call")
	return child, append([][]byte(nil), transport.bodies...)
}

type usageWorkflowTransport struct {
	base   http.RoundTripper
	mu     sync.Mutex
	bodies [][]byte
}

func (r *usageWorkflowTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.GetBody != nil {
		body, err := request.GetBody()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(body)
		_ = body.Close()
		if err != nil {
			return nil, err
		}
		var event struct {
			Content map[string]json.RawMessage `json:"content"`
		}
		if json.Unmarshal(data, &event) == nil && event.Content["usage"] != nil {
			r.mu.Lock()
			r.bodies = append(r.bodies, data)
			r.mu.Unlock()
		}
	}
	return r.base.RoundTrip(request)
}

func (f *usageWorkflowFixture) replayWorkerUsage(t *testing.T, child *corev1alpha1.Task, bodies [][]byte) {
	t.Helper()
	for _, body := range bodies {
		status, response := f.request(t, http.MethodPost,
			"/internal/v1/events/"+child.Namespace+"/task/"+child.Name, "usage-worker-fixture", body)
		require.Equal(t, http.StatusCreated, status, "%s", response)
	}
}

func (f *usageWorkflowFixture) deliverImplementation(t *testing.T, task *corev1alpha1.Task) {
	t.Helper()
	require.NoError(t, f.kube.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	item, err := f.backend.GetMonitorItem(t.Context(), task.Namespace, f.monitor.Name, "issue", "77")
	require.NoError(t, err)
	result, err := json.Marshal(map[string]any{
		"schemaVersion": "orka.issueImplementation.v1", "repo": usageWorkflowRepo, "issueNumber": 77,
		"snapshotDigest": item.SnapshotDigest, "status": "patch_ready", "summary": "Implemented fixture.",
	})
	require.NoError(t, err)
	data, err := common.FormatStructuredResult(&common.StructuredResult{Summary: string(result)})
	require.NoError(t, err)
	require.NoError(t, f.backend.SaveResult(t.Context(), task.Namespace, task.Name, data))
	task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	task.Status.CompletionTime = new(metav1.Now())
	task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
	task.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{
		State: corev1alpha1.TaskDeliveryStateVerifiedExact, Outcome: corev1alpha1.TaskDeliveryOutcomeVerifiedExact,
		PublicationID:         "publication-fixture",
		PublicationRepository: &corev1alpha1.RepositoryIdentity{Provider: "github", ID: "github.com/" + usageWorkflowRepo},
		Branch:                task.Spec.Workspace.PushBranch, RemoteBeforeSHA: new(task.Spec.Workspace.ExpectedRemoteSHA),
		ExpectedCommitSHA: strings.Repeat("a", 40), VerifiedRemoteSHA: strings.Repeat("a", 40),
		ArtifactDigest: "sha256:" + strings.Repeat("b", 64),
	}
	require.NoError(t, f.kube.Status().Update(t.Context(), task))
	f.reconcileTask(t, task)
}

func (f *usageWorkflowFixture) report(t *testing.T, selection string) (usage.Report, string) {
	t.Helper()
	if selection == "" {
		selection = url.Values{
			"namespace": {usageWorkflowNamespace}, "from": {f.startedAt.Format(time.RFC3339Nano)},
			"repository": {usageWorkflowRepo},
		}.Encode()
	}
	status, body := f.request(t, http.MethodGet, "/api/v1/usage?"+selection, f.viewer, nil)
	require.Equal(t, http.StatusOK, status, "%s", body)
	var report usage.Report
	require.NoError(t, json.Unmarshal(body, &report))
	params, err := url.ParseQuery(selection)
	require.NoError(t, err)
	params.Set("asOf", report.Selection.AsOf.Format(time.RFC3339Nano))
	return report, params.Encode()
}

func (f *usageWorkflowFixture) work(t *testing.T, id, selection string) usage.Work {
	t.Helper()
	status, body := f.request(t, http.MethodGet, "/api/v1/usage/work/"+id+"?"+selection, f.viewer, nil)
	require.Equal(t, http.StatusOK, status, "%s", body)
	var response struct {
		Work usage.Work `json:"work"`
	}
	require.NoError(t, json.Unmarshal(body, &response))
	return response.Work
}

func (f *usageWorkflowFixture) assertReport(t *testing.T, expected usage.Report, work usage.Work, selection string) {
	t.Helper()
	actual, _ := f.report(t, selection)
	require.Equal(t, expected.Summary, actual.Summary)
	require.Equal(t, expected.Works, actual.Works)
	require.Equal(t, work, f.work(t, work.ID, selection))
}

func (f *usageWorkflowFixture) assertDenied(t *testing.T, workID string) {
	t.Helper()
	for _, path := range []string{"/api/v1/usage", "/api/v1/usage/work/" + workID, "/api/v1/usage/other/unassociated"} {
		status, body := f.request(t, http.MethodGet, path+"?teams="+usageWorkflowNamespace, f.outsider, nil)
		require.Equal(t, http.StatusForbidden, status)
		require.NotContains(t, string(body), `"summary"`)
	}
}

func (f *usageWorkflowFixture) finishWorker(t *testing.T, task *corev1alpha1.Task, phase corev1alpha1.TaskPhase) {
	t.Helper()
	require.NoError(t, f.kube.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	task.Status.Phase, task.Status.CompletionTime = phase, new(metav1.Now())
	require.NoError(t, f.kube.Status().Update(t.Context(), task))
	// The fake Kubernetes client has no garbage collector. Remove the fixture
	// Pod after its worker exits, then let the Task controller clean up the Job.
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "delegated-pod", Namespace: task.Namespace}}
	require.NoError(t, f.kube.Delete(t.Context(), pod))
	f.reconcileTask(t, task)
}

func (f *usageWorkflowFixture) deleteTask(t *testing.T, task *corev1alpha1.Task) {
	t.Helper()
	status, body := f.request(t, http.MethodDelete, "/api/v1/tasks/"+task.Name+"?namespace="+task.Namespace, f.viewer, nil)
	require.Contains(t, []int{http.StatusOK, http.StatusAccepted, http.StatusNoContent}, status, "%s", body)
	for range 3 {
		f.reconcileTask(t, task)
	}
	require.True(t, apierrors.IsNotFound(f.kube.Get(t.Context(), client.ObjectKeyFromObject(task), &corev1alpha1.Task{})))
	journal, err := f.backend.ListExecutionEvents(t.Context(), store.ExecutionEventFilter{
		Namespace: task.Namespace, StreamType: "task", StreamID: task.Name,
	})
	require.NoError(t, err)
	require.Empty(t, journal)
}

func (f *usageWorkflowFixture) assertCLIReport(t *testing.T, binary string, expected usage.Report, selection string) {
	t.Helper()
	params, err := url.ParseQuery(selection)
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), binary,
		"--server", f.url, "--token", f.viewer, "--namespace", usageWorkflowNamespace,
		"usage", "summary", "--from", params.Get("from"), "--as-of", params.Get("asOf"),
		"--repository", usageWorkflowRepo, "-o", "json")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	var actual usage.Report
	require.NoError(t, json.Unmarshal(output, &actual))
	require.Equal(t, expected.Summary, actual.Summary)
	require.Equal(t, expected.Works, actual.Works)
}

type usageWorkflowGitHub struct {
	server  *httptest.Server
	creates atomic.Int32
	merged  atomic.Bool
	created time.Time
}

func newUsageWorkflowGitHub(t *testing.T) *usageWorkflowGitHub {
	t.Helper()
	g := &usageWorkflowGitHub{created: time.Now().UTC()}
	g.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body any
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/collaborators/fixture-user/permission"):
			body = map[string]any{"permission": "write"}
		case r.Method == http.MethodGet && r.URL.Path == "/repos/"+usageWorkflowRepo+"/issues/77":
			body = map[string]any{"number": 77, "title": "Implement fixture", "body": "Add the fixture change.", "state": "open",
				"html_url":   "https://github.com/" + usageWorkflowRepo + "/issues/77",
				"updated_at": "2026-06-01T00:00:00Z", "labels": []any{}}
		case r.Method == http.MethodGet && r.URL.Path == "/repos/"+usageWorkflowRepo+"/pulls":
			body = []any{}
		case r.Method == http.MethodPost && r.URL.Path == "/repos/"+usageWorkflowRepo+"/pulls":
			g.creates.Add(1)
			body = map[string]any{"number": 177, "html_url": "https://github.com/" + usageWorkflowRepo + "/pull/177"}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/77/comments"),
			r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/issues/comments/7701"):
			body = map[string]any{
				"id": 7701, "html_url": "https://github.com/" + usageWorkflowRepo + "/issues/77#issuecomment-7701",
			}
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			pr := map[string]any{
				"id": "PR_usage_177", "number": 177, "url": "https://github.com/" + usageWorkflowRepo + "/pull/177",
				"state": "OPEN", "createdAt": g.created, "headRefOid": strings.Repeat("a", 40), "isDraft": false,
				"mergeable": "MERGEABLE", "mergeStateStatus": "CLEAN", "reviewDecision": "APPROVED"}
			if g.merged.Load() {
				pr["state"], pr["mergedAt"] = "MERGED", time.Now().UTC()
			}
			body = map[string]any{"data": map[string]any{
				"repository": map[string]any{"nameWithOwner": usageWorkflowRepo, "pullRequest": pr},
			}}
		default:
			t.Errorf("unexpected GitHub fixture request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("write GitHub fixture: %v", err)
		}
	}))
	t.Cleanup(g.server.Close)
	return g
}
