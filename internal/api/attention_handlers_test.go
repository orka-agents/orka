/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/controller"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/store/storetest"
	"github.com/orka-agents/orka/internal/taskwait"
)

// attentionFreeText marks agent- and runtime-authored text that the
// attention views must never return.
const attentionFreeText = "FREE-TEXT-MUST-NOT-LEAK"

type attentionCountingEventStore struct {
	*storetest.FakeExecutionEventStore
	mu    sync.Mutex
	reads map[string]int
}

func (s *attentionCountingEventStore) ListExecutionEvents(ctx context.Context, filter store.ExecutionEventFilter) ([]store.ExecutionEvent, error) {
	s.mu.Lock()
	s.reads[filter.StreamID]++
	s.mu.Unlock()
	return s.FakeExecutionEventStore.ListExecutionEvents(ctx, filter)
}

func (s *attentionCountingEventStore) readStreams() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	maps.Copy(out, s.reads)
	return out
}

type attentionFixture struct {
	t      *testing.T
	h      *Handlers
	app    *fiber.App
	events *attentionCountingEventStore
	clock  time.Time
	user   *UserInfo
	sarMu  sync.Mutex
	sars   []authorizationv1.ResourceAttributes
	sar    func(authorizationv1.ResourceAttributes) (bool, error)
}

type attentionFixtureOption func(*HandlersConfig)

func newAttentionFixture(t *testing.T, objects []client.Object, options ...attentionFixtureOption) *attentionFixture {
	t.Helper()
	f := &attentionFixture{t: t, clock: time.Now().UTC().Add(-2 * time.Hour)}
	f.events = &attentionCountingEventStore{
		FakeExecutionEventStore: storetest.NewFakeExecutionEventStoreWithClock(func() time.Time { return f.clock }),
		reads:                   map[string]int{},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, gatewayv1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	clientset := kubefake.NewClientset()
	clientset.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		attrs := *review.Spec.ResourceAttributes
		f.sarMu.Lock()
		f.sars = append(f.sars, attrs)
		decide := f.sar
		f.sarMu.Unlock()
		allowed := true
		var err error
		if decide != nil {
			allowed, err = decide(attrs)
		}
		if err != nil {
			return true, nil, err
		}
		review.Status.Allowed = allowed
		return true, review, nil
	})
	cfg := HandlersConfig{Client: kube, KubeClient: clientset, ExecutionEventStore: f.events}
	for _, option := range options {
		option(&cfg)
	}
	f.h = NewHandlers(cfg)
	f.user = &UserInfo{Username: "alice", AuthType: AuthTypeOIDC, Issuer: "https://issuer.example", Subject: "alice"}
	f.app = fiber.New()
	f.app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, f.user)
		return c.Next()
	})
	f.app.Get("/api/v1/attention", f.h.authorizeExternalRoute("GET /api/v1/attention"), f.h.ListAttention)
	f.app.Get("/api/v1/approvals", f.h.authorizeExternalRoute("GET /api/v1/approvals"), f.h.ListPendingApprovals)
	return f
}

func withAttentionMonitorStore(t *testing.T, f **sql.DB) attentionFixtureOption {
	return func(cfg *HandlersConfig) {
		db, err := sqlite.NewDB(":memory:")
		require.NoError(t, err)
		t.Cleanup(func() { _ = db.Close() })
		*f = db
		cfg.RepositoryMonitorStore = testStorageClient(sqlite.NewStore(db, ":memory:"))
	}
}

func (f *attentionFixture) recordedSARs() []authorizationv1.ResourceAttributes {
	f.sarMu.Lock()
	defer f.sarMu.Unlock()
	return append([]authorizationv1.ResourceAttributes(nil), f.sars...)
}

func (f *attentionFixture) get(path string) (int, string) {
	f.t.Helper()
	resp, err := f.app.Test(httptest.NewRequest(http.MethodGet, path, nil))
	require.NoError(f.t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(f.t, err)
	return resp.StatusCode, string(body)
}

func (f *attentionFixture) list(path string) AttentionResponse {
	f.t.Helper()
	status, body := f.get(path)
	require.Equal(f.t, http.StatusOK, status, body)
	require.NotContains(f.t, body, attentionFreeText)
	var response AttentionResponse
	require.NoError(f.t, json.Unmarshal([]byte(body), &response))
	return response
}

func (f *attentionFixture) appendApproval(taskName string, payload map[string]any) {
	f.t.Helper()
	content, err := json.Marshal(payload)
	require.NoError(f.t, err)
	_, err = f.events.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: taskName, TaskName: taskName,
		Type: events.ExecutionEventTypeApprovalRequested, Summary: attentionFreeText, Content: content,
	})
	require.NoError(f.t, err)
	f.clock = f.clock.Add(time.Minute)
}

func (f *attentionFixture) appendDecision(taskName, approvalID, eventType string) {
	f.t.Helper()
	content, err := json.Marshal(map[string]string{"approvalID": approvalID})
	require.NoError(f.t, err)
	_, err = f.events.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace: "default", StreamType: store.ExecutionEventStreamTypeTask, StreamID: taskName, TaskName: taskName,
		Type: eventType, ToolCallID: approvalID, Content: content,
	})
	require.NoError(f.t, err)
}

func attentionTask(name, phase string) *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name, UID: types.UID("uid-" + name), CreationTimestamp: metav1.NewTime(time.Now().Add(-3 * time.Hour))},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhase(phase)},
	}
}

func attentionWaitingTask(name string) *corev1alpha1.Task {
	task := attentionTask(name, string(corev1alpha1.TaskPhaseRunning))
	task.Status.Conditions = []metav1.Condition{{
		Type: controller.ConditionTypeWaitingForApproval, Status: metav1.ConditionTrue, Reason: "ApprovalPending",
		Message: "waiting for approval " + attentionFreeText, LastTransitionTime: metav1.Now(),
	}}
	return task
}

func attentionParkedTask(name, approvalID string, deadline time.Time) *corev1alpha1.Task {
	task := attentionTask(name, string(corev1alpha1.TaskPhaseSucceeded))
	task.Status.Wait = &corev1alpha1.TaskWaitStatus{
		State: corev1alpha1.TaskWaitStateWaiting, Kind: corev1alpha1.TaskWaitKindApproval, References: []string{approvalID},
		Reason: attentionFreeText, Attempt: 1, PromptID: "prompt-1", RequestedAt: metav1.Now(), Deadline: metav1.NewTime(deadline),
	}
	return task
}

func attentionBrokeredTask(name string) (*corev1alpha1.Task, *approvals.CallBinding) {
	task := attentionTask(name, string(corev1alpha1.TaskPhaseRunning))
	task.Status.Execution = &corev1alpha1.TaskExecutionStatus{
		State: corev1alpha1.TaskExecutionStateRunning, Attempt: 1, PromptID: "prompt-1",
		RuntimeInstanceID: "instance-1", RuntimeSessionUID: "rs-uid", RuntimeSessionGeneration: 2,
		RuntimeSessionSupervisorBootID: "boot-1", ControllerEpoch: 3,
	}
	task.Status.Conditions = attentionWaitingTask(name).Status.Conditions
	binding := &approvals.CallBinding{
		TaskAttempt: 1, PromptID: "prompt-1", OperationIDDigest: "sha256:op", RuntimeSessionUID: "rs-uid", RuntimeSessionGeneration: 2,
		RuntimeInstanceIDDigest: store.CanonicalBytesDigest([]byte("instance-1")),
		SupervisorBootIDDigest:  store.CanonicalBytesDigest([]byte("boot-1")), ControllerEpoch: 3, RequestDigest: "sha256:req",
	}
	return task, binding
}

func attentionKinds(items []AttentionItem) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		id := item.Kind
		if item.Task != nil {
			id += "/" + item.Task.Name
		}
		if item.Monitor != nil {
			id += "/" + item.Monitor.Name
		}
		out = append(out, id)
	}
	return out
}

func TestAttentionRanksSafeSummariesAndReadsOnlyCandidateStreams(t *testing.T) {
	now := time.Now().UTC()
	parked := attentionParkedTask("parked", "parked-approval", now.Add(time.Hour))
	brokered, binding := attentionBrokeredTask("brokered")
	worker := attentionWaitingTask("worker")
	expired := attentionWaitingTask("expired")
	decided := attentionWaitingTask("decided")
	quiet := attentionTask("quiet", string(corev1alpha1.TaskPhaseRunning)) // approval events, no condition
	completed := attentionTask("completed-terminal", string(corev1alpha1.TaskPhaseSucceeded))
	completed.Status.Conditions = worker.Status.Conditions
	unknown := attentionTask("unknown", string(corev1alpha1.TaskPhaseFailed))
	unknown.Status.CompletionTime = &metav1.Time{Time: now.Add(-time.Hour)}
	unknown.Status.Execution = &corev1alpha1.TaskExecutionStatus{State: corev1alpha1.TaskExecutionStateOutcomeUnknown, Outcome: corev1alpha1.TaskExecutionOutcomeOutcomeUnknown}
	failed := attentionTask("failed", string(corev1alpha1.TaskPhaseFailed))
	failed.Status.CompletionTime = &metav1.Time{Time: now.Add(-30 * time.Minute)}
	failed.Status.Message = attentionFreeText
	oldFailed := attentionTask("old-failed", string(corev1alpha1.TaskPhaseFailed))
	oldFailed.Status.CompletionTime = &metav1.Time{Time: now.Add(-48 * time.Hour)}
	podWaiting := attentionTask("pod-waiting", string(corev1alpha1.TaskPhaseRunning))
	podWaiting.Status.Message = "pod waiting: ErrImagePull - " + attentionFreeText
	oddPod := attentionTask("odd-pod", string(corev1alpha1.TaskPhaseRunning))
	oddPod.Status.Message = "pod waiting: " + attentionFreeText
	otherNamespace := attentionWaitingTask("elsewhere")
	otherNamespace.Namespace = "other"

	f := newAttentionFixture(t, []client.Object{parked, brokered, worker, expired, decided, quiet, completed, unknown, failed, oldFailed, podWaiting, oddPod, otherNamespace})
	parkedEvent, _, err := taskwait.ParkedApprovalRequestEvent(parked, parked.Status.Wait)
	require.NoError(t, err)
	_, err = f.events.AppendExecutionEvent(context.Background(), parkedEvent)
	require.NoError(t, err)
	f.appendApproval("brokered", map[string]any{
		"approvalID": "brokered-approval", "taskUID": "uid-brokered", "action": "Execute deploy via ops", "targetTool": "ops",
		"targetArgsPreview": map[string]string{"secret": attentionFreeText}, "riskSummary": attentionFreeText, "severity": "warning",
		"expiresAt": now.Add(2 * time.Hour).Format(time.RFC3339), "binding": binding,
	})
	f.appendApproval("worker", map[string]any{
		"approvalID": "worker-approval", "taskUID": "uid-worker", "action": "create_pr", "targetTool": "github\u0000_pr",
		"riskSummary": attentionFreeText, "severity": attentionFreeText,
	})
	f.appendApproval("expired", map[string]any{"approvalID": "expired-approval", "action": "old", "expiresAt": now.Add(-time.Minute).Format(time.RFC3339)})
	f.appendApproval("decided", map[string]any{"approvalID": "decided-approval", "action": "done"})
	f.appendDecision("decided", "decided-approval", events.ExecutionEventTypeApprovalApproved)
	f.appendApproval("quiet", map[string]any{"approvalID": "quiet-approval", "action": "hidden"})
	f.appendApproval("completed-terminal", map[string]any{"approvalID": "terminal-approval", "action": "late"})

	response := f.list("/api/v1/attention?namespace=default")
	require.Equal(t, []string{
		"approval/parked", "approval/brokered", "approval/worker",
		"outcomeUnknown/unknown", "failed/failed", "podWaiting/odd-pod", "podWaiting/pod-waiting",
	}, attentionKinds(response.Items))
	require.False(t, response.Incomplete)
	require.Equal(t, 7, response.Total)
	require.Equal(t, map[string]int{"approval": 3, "outcomeUnknown": 1, "failed": 1, "podWaiting": 2}, response.Counts)
	require.Equal(t, attentionSourceUnavailable, response.Sources.Monitors)
	require.Equal(t, attentionSourceIncluded, response.Sources.Approvals)
	require.NotNil(t, response.Since)

	parkedItem := response.Items[0].Approval
	require.Equal(t, AttentionApprovalSourceParked, parkedItem.Source)
	require.Equal(t, taskwait.ParkedApprovalAction, parkedItem.Action)
	require.Equal(t, "Agent sign-off requested", response.Items[0].Summary)
	require.NotNil(t, response.Items[0].Deadline)
	require.Equal(t, AttentionApprovalSourceBrokered, response.Items[1].Approval.Source)
	require.Equal(t, "warning", response.Items[1].Approval.Severity)
	require.Equal(t, AttentionApprovalSourceWorker, response.Items[2].Approval.Source)
	require.Equal(t, "github_pr", response.Items[2].Approval.TargetTool, "control characters are dropped")
	require.Empty(t, response.Items[2].Approval.Severity, "unknown severities are dropped")
	require.Equal(t, "ErrImagePull", response.Items[6].Reason)
	require.Equal(t, attentionPodWaitingUnknown, response.Items[5].Reason)
	for _, item := range response.Items[:3] {
		require.NotNil(t, item.Approval.CanApprove, item.ID)
		require.True(t, *item.Approval.CanApprove, item.ID)
		require.True(t, *item.Approval.CanDecline, item.ID)
	}

	reads := f.events.readStreams()
	for _, name := range []string{"parked", "brokered", "worker", "expired", "decided"} {
		require.Positive(t, reads[name], "candidate %s was not read", name)
	}
	for _, name := range []string{"quiet", "completed-terminal", "failed", "unknown", "pod-waiting", "elsewhere"} {
		require.Zero(t, reads[name], "non-candidate %s stream was read", name)
	}

	inbox := f.list("/api/v1/approvals?namespace=default&status=pending")
	require.Equal(t, []string{"approval/parked", "approval/brokered", "approval/worker"}, attentionKinds(inbox.Items))
	require.Nil(t, inbox.Since)
	require.Equal(t, attentionSourceNotRequired, inbox.Sources.Monitors)
}

func TestAttentionParkedApprovalNeedsArmedWait(t *testing.T) {
	now := time.Now().UTC()
	armed := attentionParkedTask("armed", "armed-approval", now.Add(time.Hour))
	requested := attentionParkedTask("requested", "requested-approval", now.Add(time.Hour))
	requested.Status.Wait.State = corev1alpha1.TaskWaitStateRequested // turn Succeeded: decidable
	failedTurn := attentionParkedTask("failed-turn", "failed-turn-approval", now.Add(time.Hour))
	failedTurn.Status.Phase = corev1alpha1.TaskPhaseFailed
	failedTurn.Status.Wait.State = corev1alpha1.TaskWaitStateRequested
	pastDeadline := attentionParkedTask("past-deadline", "past-deadline-approval", now.Add(-time.Minute))
	resumed := attentionParkedTask("resumed", "resumed-approval", now.Add(time.Hour))
	resumed.Status.Wait.State = corev1alpha1.TaskWaitStateResumed
	deleting := attentionParkedTask("deleting", "deleting-approval", now.Add(time.Hour))
	deleting.DeletionTimestamp = &metav1.Time{Time: now}
	deleting.Finalizers = []string{"orka.ai/test"}

	f := newAttentionFixture(t, []client.Object{armed, requested, failedTurn, pastDeadline, resumed, deleting})
	for _, task := range []*corev1alpha1.Task{armed, requested, failedTurn, pastDeadline, resumed, deleting} {
		event, _, err := taskwait.ParkedApprovalRequestEvent(task, task.Status.Wait)
		require.NoError(t, err)
		_, err = f.events.AppendExecutionEvent(context.Background(), event)
		require.NoError(t, err)
	}
	response := f.list("/api/v1/approvals?namespace=default")
	require.Equal(t, []string{"approval/armed", "approval/requested"}, attentionKinds(response.Items))
	reads := f.events.readStreams()
	for _, name := range []string{"failed-turn", "resumed", "deleting"} {
		require.Zero(t, reads[name], "%s is not a candidate", name)
	}
}

func TestAttentionPaginationIsStableAndCursorIsBound(t *testing.T) {
	now := time.Now().UTC()
	objects := make([]client.Object, 0, 5)
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		task := attentionTask("failed-"+name, string(corev1alpha1.TaskPhaseFailed))
		// Two pairs share a completion time, so names break the tie.
		offset := map[string]time.Duration{"a": 3, "b": 1, "c": 1, "d": 2, "e": 2}[name]
		task.Status.CompletionTime = &metav1.Time{Time: now.Add(-offset * time.Minute)}
		objects = append(objects, task)
	}
	f := newAttentionFixture(t, objects)

	full := f.list("/api/v1/attention?namespace=default")
	require.Equal(t, []string{"failed/failed-a", "failed/failed-d", "failed/failed-e", "failed/failed-b", "failed/failed-c"}, attentionKinds(full.Items))
	require.Empty(t, full.Metadata.Continue)

	var paged []AttentionItem
	path := "/api/v1/attention?namespace=default&limit=2"
	pages := 0
	for {
		page := f.list(path)
		pages++
		require.Equal(t, 5, page.Total)
		paged = append(paged, page.Items...)
		if page.Metadata.Continue == "" {
			break
		}
		require.Len(t, page.Items, 2)
		path = "/api/v1/attention?namespace=default&limit=2&continue=" + url.QueryEscape(page.Metadata.Continue)
	}
	require.Equal(t, 3, pages)
	require.Equal(t, attentionKinds(full.Items), attentionKinds(paged))

	first := f.list("/api/v1/attention?namespace=default&limit=2")
	token := url.QueryEscape(first.Metadata.Continue)
	for _, path := range []string{
		"/api/v1/attention?namespace=default&continue=not-a-token",
		"/api/v1/attention?namespace=default&since=1h&continue=" + token,
		"/api/v1/approvals?namespace=default&continue=" + token,
		"/api/v1/attention?namespace=default&limit=0",
		"/api/v1/attention?namespace=default&limit=201",
		"/api/v1/attention?namespace=default&since=-1h",
		"/api/v1/attention?namespace=default&since=200h",
		"/api/v1/approvals?namespace=default&status=approved",
		"/api/v1/approvals?namespace=default&since=1h",
	} {
		status, body := f.get(path)
		require.Equal(t, http.StatusBadRequest, status, "%s: %s", path, body)
	}
	other := newAttentionFixture(t, nil)
	other.h.watchNamespace = ""
	status, _ := other.get("/api/v1/attention?namespace=other&continue=" + token)
	require.Equal(t, http.StatusBadRequest, status, "a cursor is bound to its namespace")

	narrow := f.list("/api/v1/attention?namespace=default&since=90s")
	require.Equal(t, []string{"failed/failed-b", "failed/failed-c"}, attentionKinds(narrow.Items))
}

func TestAttentionRequesterOnlyApproveRule(t *testing.T) {
	task := attentionWaitingTask("delegated")
	task.Spec.RequestedBy = &corev1alpha1.RequestedBy{Issuer: "https://issuer.example", Subject: "requester"}
	task.Status.AgentExecutionBinding = &corev1alpha1.AgentExecutionBinding{ContractVersion: corev1alpha1.AgentRuntimeContractHarnessV2, Backend: corev1alpha1.AgentExecutionBackendRuntimePool, Agent: &corev1alpha1.AgentExecutionAgentRef{
		Identity: &corev1alpha1.AgentExecutionIdentityRef{ActsForRequester: true},
	}}
	plain := attentionWaitingTask("plain")
	f := newAttentionFixture(t, []client.Object{task, plain})
	f.appendApproval("delegated", map[string]any{"approvalID": "delegated-approval", "action": "write"})
	f.appendApproval("plain", map[string]any{"approvalID": "plain-approval", "action": "write"})

	capabilities := func() map[string][2]*bool {
		out := map[string][2]*bool{}
		for _, item := range f.list("/api/v1/approvals?namespace=default").Items {
			out[item.Task.Name] = [2]*bool{item.Approval.CanApprove, item.Approval.CanDecline}
		}
		return out
	}

	// Another OIDC person may decline but not approve.
	f.user = &UserInfo{Username: "bob", AuthType: AuthTypeOIDC, Issuer: "https://issuer.example", Subject: "bob"}
	got := capabilities()
	require.False(t, *got["delegated"][0])
	require.True(t, *got["delegated"][1])
	require.True(t, *got["plain"][0])

	// The requester may do both.
	f.user = &UserInfo{Username: "requester", AuthType: AuthTypeOIDC, Issuer: "https://issuer.example", Subject: "requester"}
	got = capabilities()
	require.True(t, *got["delegated"][0])
	require.True(t, *got["delegated"][1])

	// A ServiceAccount can never be the requester, even with every grant.
	f.user = &UserInfo{Username: "system:serviceaccount:default:ops", AuthType: AuthTypeTokenReview}
	got = capabilities()
	require.False(t, *got["delegated"][0])
	require.True(t, *got["delegated"][1])
	require.True(t, *got["plain"][0])
}

func TestAttentionTokenReviewAuthorization(t *testing.T) {
	task := attentionWaitingTask("waiting")
	f := newAttentionFixture(t, []client.Object{task})
	f.appendApproval("waiting", map[string]any{"approvalID": "approval-1", "action": "write"})
	f.user = &UserInfo{Username: "system:serviceaccount:default:viewer", AuthType: AuthTypeTokenReview}

	t.Run("route denial reads nothing", func(t *testing.T) {
		f.sar = func(attrs authorizationv1.ResourceAttributes) (bool, error) { return false, nil }
		for _, path := range []string{"/api/v1/attention?namespace=default", "/api/v1/approvals?namespace=default"} {
			before := f.events.readStreams()["waiting"]
			status, body := f.get(path)
			require.Equal(t, http.StatusForbidden, status, body)
			require.Equal(t, before, f.events.readStreams()["waiting"])
		}
	})

	t.Run("list without decision grants", func(t *testing.T) {
		f.sars = nil
		f.sar = func(attrs authorizationv1.ResourceAttributes) (bool, error) {
			return attrs.Resource == "tasks" && attrs.Verb == "list", nil
		}
		item := f.list("/api/v1/approvals?namespace=default").Items[0]
		require.False(t, *item.Approval.CanApprove)
		require.False(t, *item.Approval.CanDecline)
		sars := f.recordedSARs()
		require.Equal(t, authorizationv1.ResourceAttributes{Namespace: "default", Verb: "list", Group: corev1alpha1.GroupVersion.Group, Resource: "tasks"}, sars[0])
		require.Equal(t, authorizationv1.ResourceAttributes{
			Namespace: "default", Verb: "update", Group: corev1alpha1.GroupVersion.Group, Resource: "tasks", Subresource: "approvals", Name: "waiting",
		}, sars[1], "decision hints use the decision route's named check")
	})

	t.Run("review errors are unknown", func(t *testing.T) {
		f.sar = func(attrs authorizationv1.ResourceAttributes) (bool, error) {
			if attrs.Verb == "list" {
				return true, nil
			}
			return false, errors.New("authorizer unavailable")
		}
		item := f.list("/api/v1/approvals?namespace=default").Items[0]
		require.Nil(t, item.Approval.CanApprove)
		require.Nil(t, item.Approval.CanDecline)
	})

	t.Run("named grants allow both", func(t *testing.T) {
		f.sar = nil
		item := f.list("/api/v1/approvals?namespace=default").Items[0]
		require.True(t, *item.Approval.CanApprove)
		require.True(t, *item.Approval.CanDecline)
	})
}

func TestAttentionCapabilityChecksAreBounded(t *testing.T) {
	objects := make([]client.Object, 0, 3)
	for _, name := range []string{"a", "b", "c"} {
		objects = append(objects, attentionWaitingTask("waiting-"+name))
	}
	f := newAttentionFixture(t, objects)
	f.h.attentionBounds.CapabilityTasks = 2
	for _, name := range []string{"a", "b", "c"} {
		f.appendApproval("waiting-"+name, map[string]any{"approvalID": "approval-" + name, "action": "write"})
	}
	f.user = &UserInfo{Username: "system:serviceaccount:default:ops", AuthType: AuthTypeTokenReview}
	response := f.list("/api/v1/approvals?namespace=default")
	require.Len(t, response.Items, 3)
	require.True(t, *response.Items[0].Approval.CanApprove)
	require.True(t, *response.Items[1].Approval.CanApprove)
	require.Nil(t, response.Items[2].Approval.CanApprove, "past the review budget the hint is unknown")
	require.Nil(t, response.Items[2].Approval.CanDecline)
	require.Len(t, f.recordedSARs(), 1+2*2, "one route check and two named checks per Task in budget")
}

func TestAttentionContextTokenAuthorization(t *testing.T) {
	mine := attentionWaitingTask("mine")
	theirs := attentionWaitingTask("theirs")
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	require.NoError(t, err)
	f := newAttentionFixture(t, []client.Object{mine, theirs}, func(cfg *HandlersConfig) { cfg.ContextTokenAuthorization = authz })
	f.appendApproval("mine", map[string]any{"approvalID": "mine-approval", "action": "write"})
	f.appendApproval("theirs", map[string]any{"approvalID": "theirs-approval", "action": "write"})
	token := func(scopes ...string) *UserInfo {
		return &UserInfo{AuthType: AuthTypeContextToken, Issuer: "https://tts.example", Subject: "agent", ContextToken: &ContextToken{
			Scopes: scopes, TransactionContext: map[string]any{"namespace": "default", "taskName": "mine"},
		}}
	}

	f.user = token(ContextTokenScopeTaskGet)
	status, body := f.get("/api/v1/attention?namespace=default")
	require.Equal(t, http.StatusForbidden, status, body)
	require.Empty(t, f.events.readStreams())

	f.user = token(ContextTokenScopeTaskList, ContextTokenScopeTaskGet)
	response := f.list("/api/v1/attention?namespace=default")
	require.Equal(t, []string{"approval/mine"}, attentionKinds(response.Items))
	require.False(t, *response.Items[0].Approval.CanApprove, "no Task update scope")
	require.False(t, *response.Items[0].Approval.CanDecline)
	require.Zero(t, f.events.readStreams()["theirs"], "a Task outside the token context is never read")

	f.user = token(ContextTokenScopeTaskList)
	response = f.list("/api/v1/attention?namespace=default")
	require.Empty(t, response.Items, "approval detail needs the Task read scope")
	require.Zero(t, f.events.readStreams()["theirs"])

	f.user = token(ContextTokenScopeTaskList, ContextTokenScopeTaskGet, defaultTaskUpdateScope)
	response = f.list("/api/v1/attention?namespace=default")
	require.True(t, *response.Items[0].Approval.CanApprove)
	require.True(t, *response.Items[0].Approval.CanDecline)
}

func TestAttentionGatewayOwnedTasks(t *testing.T) {
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "ns-uid"}}
	gateway := &gatewayv1alpha1.Gateway{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "chat", UID: "gw-uid"}}
	owned := attentionWaitingTask("owned")
	owned.Spec.RequestedBy = &corev1alpha1.RequestedBy{Issuer: "gateway.orka.ai/default/ns-uid/chat/gw-uid", Subject: "user"}
	f := newAttentionFixture(t, []client.Object{namespace, gateway, owned})
	f.appendApproval("owned", map[string]any{"approvalID": "owned-approval", "action": "write"})
	f.user = &UserInfo{Username: "system:serviceaccount:default:ops", AuthType: AuthTypeTokenReview}

	f.sar = func(attrs authorizationv1.ResourceAttributes) (bool, error) { return attrs.Resource != "gateways", nil }
	response := f.list("/api/v1/attention?namespace=default")
	require.Empty(t, response.Items)
	require.Zero(t, f.events.readStreams()["owned"], "an unreadable Gateway Task's stream is never read")

	f.sar = func(attrs authorizationv1.ResourceAttributes) (bool, error) {
		return attrs.Resource != "gateways" || attrs.Verb == "get", nil
	}
	response = f.list("/api/v1/attention?namespace=default")
	require.Len(t, response.Items, 1)
	require.False(t, *response.Items[0].Approval.CanApprove, "deciding needs Gateway update")
	require.False(t, *response.Items[0].Approval.CanDecline)

	f.sar = nil
	response = f.list("/api/v1/attention?namespace=default")
	require.True(t, *response.Items[0].Approval.CanDecline)
}

func TestAttentionRejectsWrongNamespace(t *testing.T) {
	f := newAttentionFixture(t, []client.Object{attentionWaitingTask("waiting")}, func(cfg *HandlersConfig) {
		cfg.EnforceNamespaceIsolation = true
	})
	f.user = &UserInfo{Username: "alice", AuthType: AuthTypeOIDC, Namespace: "team-a"}
	for _, path := range []string{"/api/v1/attention?namespace=default", "/api/v1/approvals?namespace=default"} {
		status, body := f.get(path)
		require.Equal(t, http.StatusForbidden, status, body)
	}
	require.Empty(t, f.events.readStreams())

	watched := newAttentionFixture(t, nil, func(cfg *HandlersConfig) { cfg.WatchNamespace = "default" })
	status, _ := watched.get("/api/v1/attention?namespace=other")
	require.Equal(t, http.StatusForbidden, status)
}

func TestAttentionReportsIncompleteScans(t *testing.T) {
	objects := make([]client.Object, 0, 3)
	for _, name := range []string{"a", "b", "c"} {
		objects = append(objects, attentionWaitingTask("waiting-"+name))
	}
	f := newAttentionFixture(t, objects)
	for _, name := range []string{"a", "b", "c"} {
		f.appendApproval("waiting-"+name, map[string]any{"approvalID": "approval-" + name, "action": "write"})
	}

	complete := f.list("/api/v1/approvals?namespace=default")
	require.False(t, complete.Incomplete)
	require.Len(t, complete.Items, 3)

	f.h.attentionBounds = attentionBounds{ApprovalCandidates: 2}
	response := f.list("/api/v1/approvals?namespace=default")
	require.True(t, response.Incomplete)
	require.Equal(t, []string{attentionIncompleteApprovalCandidateLimit}, response.IncompleteReasons)
	require.Len(t, response.Items, 2)

	f.h.attentionBounds = attentionBounds{TaskScan: 1}
	response = f.list("/api/v1/approvals?namespace=default")
	require.Equal(t, []string{attentionIncompleteTaskScanLimit}, response.IncompleteReasons)
	require.Len(t, response.Items, 1)

	f.appendApproval("waiting-a", map[string]any{"approvalID": "approval-a2", "action": "write"})
	f.h.attentionBounds = attentionBounds{ApprovalEvents: 1}
	response = f.list("/api/v1/approvals?namespace=default")
	require.Equal(t, []string{attentionIncompleteApprovalEventLimit}, response.IncompleteReasons)
	require.Equal(t, []string{"approval/waiting-b", "approval/waiting-c"}, attentionKinds(response.Items),
		"a stream past its bound is left out, never derived from a prefix")
}

func TestAttentionHonorsCancellation(t *testing.T) {
	f := newAttentionFixture(t, []client.Object{attentionWaitingTask("waiting")})
	f.appendApproval("waiting", map[string]any{"approvalID": "approval-1", "action": "write"})
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		c.SetContext(ctx)
		c.Locals(UserInfoContextKey, f.user)
		return c.Next()
	})
	app.Get("/api/v1/attention", f.h.ListAttention)
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/v1/attention?namespace=default", nil))
	require.NoError(t, err)
	require.NotEqual(t, http.StatusOK, resp.StatusCode)
	require.Empty(t, f.events.readStreams())
}

func TestAttentionMonitorItemsAreAuthorizedSeparately(t *testing.T) {
	var db *sql.DB
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "repo"}}
	failed := attentionTask("failed", string(corev1alpha1.TaskPhaseFailed))
	failed.Status.CompletionTime = &metav1.Time{Time: time.Now().Add(-time.Minute)}
	f := newAttentionFixture(t, []client.Object{monitor, failed}, withAttentionMonitorStore(t, &db))
	ctx := context.Background()
	for _, action := range []store.WorkAction{
		{ID: "blocked-new", MonitorNamespace: "default", MonitorName: "repo", TargetKind: "pull_request", TargetNumber: 7, DesiredAction: "review", Status: store.RepositoryMonitorWorkActionStatusBlocked, BlockedReason: "review_blocked", Error: attentionFreeText},
		{ID: "blocked-dup", MonitorNamespace: "default", MonitorName: "repo", TargetKind: "pull_request", TargetNumber: 7, DesiredAction: "repair", Status: store.RepositoryMonitorWorkActionStatusBlocked, BlockedReason: attentionFreeText},
		{ID: "blocked-old", MonitorNamespace: "default", MonitorName: "repo", TargetKind: "issue", TargetNumber: 3, DesiredAction: "triage", Status: store.RepositoryMonitorWorkActionStatusBlocked, BlockedReason: "command_blocked"},
		{ID: "blocked-orphan", MonitorNamespace: "default", MonitorName: "deleted", TargetKind: "issue", TargetNumber: 4, Status: store.RepositoryMonitorWorkActionStatusBlocked},
		{ID: "running", MonitorNamespace: "default", MonitorName: "repo", TargetKind: "issue", TargetNumber: 5, Status: store.RepositoryMonitorWorkActionStatusRunning},
	} {
		require.NoError(t, f.h.repositoryMonitorStore.CreateWorkAction(ctx, &action))
	}
	_, err := db.ExecContext(ctx, `UPDATE work_actions SET updated_at = ? WHERE id = 'blocked-dup'`, time.Now().UTC().Add(-time.Hour))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE work_actions SET updated_at = ? WHERE id = 'blocked-old'`, time.Now().UTC().Add(-48*time.Hour))
	require.NoError(t, err)

	f.user = &UserInfo{Username: "system:serviceaccount:default:ops", AuthType: AuthTypeTokenReview}
	f.sar = func(attrs authorizationv1.ResourceAttributes) (bool, error) {
		return attrs.Resource != "monitorworkactions", nil
	}
	response := f.list("/api/v1/attention?namespace=default")
	require.Equal(t, attentionSourceForbidden, response.Sources.Monitors)
	require.Equal(t, []string{"failed/failed"}, attentionKinds(response.Items))

	f.sar = nil
	response = f.list("/api/v1/attention?namespace=default")
	require.Equal(t, attentionSourceIncluded, response.Sources.Monitors)
	require.Equal(t, []string{"failed/failed", "monitorBlocked/repo"}, attentionKinds(response.Items))
	item := response.Items[1]
	require.Equal(t, "blocked-new", item.Monitor.ActionID)
	require.Equal(t, "review_blocked", item.Reason)
	require.Equal(t, "Monitor review blocked on pull_request #7", item.Summary)

	f.h.attentionBounds = attentionBounds{MonitorActions: 1}
	response = f.list("/api/v1/attention?namespace=default")
	require.Equal(t, []string{attentionIncompleteMonitorActionLimit}, response.IncompleteReasons)

	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	require.NoError(t, err)
	f.h.attentionBounds = attentionBounds{}
	f.h.contextTokenAuthorization = authz
	f.user = &UserInfo{AuthType: AuthTypeContextToken, ContextToken: &ContextToken{Scopes: []string{ContextTokenScopeTaskList, ContextTokenScopeTaskGet}}}
	response = f.list("/api/v1/attention?namespace=default")
	require.Equal(t, attentionSourceForbidden, response.Sources.Monitors, "monitor items need the monitor read scope")
	f.user.ContextToken.Scopes = append(f.user.ContextToken.Scopes, ContextTokenScopeMonitorsRead)
	response = f.list("/api/v1/attention?namespace=default")
	require.Equal(t, attentionSourceIncluded, response.Sources.Monitors)
	require.Contains(t, attentionKinds(response.Items), "monitorBlocked/repo")
}

func TestAttentionSafeTextBounds(t *testing.T) {
	require.Equal(t, "a b", attentionSafeText("  a \n\t b  "))
	require.Equal(t, "ab", attentionSafeText("a\u200bb\u0007"))
	long := attentionSafeText(strings.Repeat("x", 500))
	require.Equal(t, attentionSafeTextMaxRunes+1, len([]rune(long)))
	require.Empty(t, attentionSafeCode("Free text"))
	require.Equal(t, "run_signal_failed", attentionSafeCode("run_signal_failed"))
}

func TestAttentionWithoutEventStorage(t *testing.T) {
	failed := attentionTask("failed", string(corev1alpha1.TaskPhaseFailed))
	failed.Status.CompletionTime = &metav1.Time{Time: time.Now().Add(-time.Minute)}
	f := newAttentionFixture(t, []client.Object{failed, attentionWaitingTask("waiting")}, func(cfg *HandlersConfig) {
		cfg.ExecutionEventStore = nil
	})
	response := f.list("/api/v1/attention?namespace=default")
	require.Equal(t, attentionSourceUnavailable, response.Sources.Approvals)
	require.Equal(t, []string{"failed/failed"}, attentionKinds(response.Items))
	status, body := f.get("/api/v1/approvals?namespace=default")
	require.Equal(t, http.StatusNotImplemented, status, body)
}
