/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

func TestInternalCallerAuthorizerVerifyNamespace(t *testing.T) {
	tests := []struct {
		name       string
		userInfo   *UserInfo
		wantStatus int
	}{
		{name: "missing user", wantStatus: http.StatusUnauthorized},
		{name: "explicit namespace mismatch", userInfo: &UserInfo{Namespace: "other", Username: "system:serviceaccount:other:worker"}, wantStatus: http.StatusForbidden},
		{name: "service account username mismatch", userInfo: &UserInfo{Username: "system:serviceaccount:other:worker"}, wantStatus: http.StatusForbidden},
		{name: "service account namespace match", userInfo: &UserInfo{Username: "system:serviceaccount:default:worker"}, wantStatus: http.StatusNoContent},
		{name: "non service account without namespace remains allowed", userInfo: &UserInfo{Username: "admin"}, wantStatus: http.StatusNoContent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := fiber.New()
			app.Use(func(c fiber.Ctx) error {
				if tt.userInfo != nil {
					c.Locals(UserInfoContextKey, tt.userInfo)
				}
				return c.Next()
			})
			app.Get("/check", func(c fiber.Ctx) error {
				if err := (internalCallerAuthorizer{}).verifyNamespace(c, "default"); err != nil {
					return err
				}
				return c.SendStatus(fiber.StatusNoContent)
			})
			resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/check", nil))
			require.NoError(t, err)
			require.Equal(t, tt.wantStatus, resp.StatusCode)
		})
	}
}

func TestInternalCallerAuthorizerVerifyTaskWorker(t *testing.T) {
	scheme := internalCallerAuthScheme(t)
	task := internalCallerAuthTask()
	job := internalCallerAuthJob(task, "job-a", "job-uid")
	pod := internalCallerAuthPod(task, "pod-a", "pod-uid", job)
	authorizer := internalCallerAuthorizer{k8sClient: fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, job, pod).Build()}

	err := authorizer.verifyTaskWorker(context.Background(), &UserInfo{Username: "system:serviceaccount:default:worker"}, task)
	fiberErr := &fiber.Error{}
	if !errorsAsFiber(err, &fiberErr) || fiberErr.Code != http.StatusForbidden || fiberErr.Message != "caller pod token required" {
		t.Fatalf("non-tokenreview error = %v, want caller pod token required", err)
	}

	err = authorizer.verifyTaskWorker(context.Background(), &UserInfo{AuthType: AuthTypeTokenReview, Username: "system:serviceaccount:default:worker"}, task)
	if !errorsAsFiber(err, &fiberErr) || fiberErr.Code != http.StatusForbidden || fiberErr.Message != "ServiceAccount namespace required" {
		t.Fatalf("missing namespace error = %v, want ServiceAccount namespace required", err)
	}

	err = authorizer.verifyTaskWorker(context.Background(), &UserInfo{
		AuthType:  AuthTypeTokenReview,
		Username:  "system:serviceaccount:default:worker",
		Namespace: "default",
	}, task)
	if !errorsAsFiber(err, &fiberErr) || fiberErr.Code != http.StatusForbidden || fiberErr.Message != "caller pod identity required" {
		t.Fatalf("missing pod extras error = %v, want pod identity required", err)
	}

	err = authorizer.verifyTaskWorker(context.Background(), internalCallerAuthWorkerUser("pod-a", "pod-uid"), task)
	if err != nil {
		t.Fatalf("valid worker error = %v, want nil", err)
	}
}

func TestSubmitResultRequiresCurrentTaskWorkerWithKubernetesClient(t *testing.T) {
	scheme := internalCallerAuthScheme(t)
	task := internalCallerAuthTask()
	job := internalCallerAuthJob(task, "job-a", "job-uid")
	pod := internalCallerAuthPod(task, "pod-a", "pod-uid", job)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, job, pod).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	resultStore := sqlite.NewStore(db, ":memory:")
	h := NewInternalHandlers(resultStore, resultStore, resultStore, resultStore, resultStore, InternalHandlersConfig{Client: k8sClient})

	missingPodApp := internalCallerAuthorizationApp(h, &UserInfo{
		Username:  "system:serviceaccount:default:worker",
		Namespace: "default",
		AuthType:  AuthTypeTokenReview,
	})
	resp := internalCallerAuthorizationRequest(t, missingPodApp, http.MethodPost, "/internal/v1/results/default/task-a", []byte("result"))
	require.Equal(t, http.StatusForbidden, resp.StatusCode)

	workerApp := internalCallerAuthorizationApp(h, internalCallerAuthWorkerUser("pod-a", "pod-uid"))
	resp = internalCallerAuthorizationRequest(t, workerApp, http.MethodPost, "/internal/v1/results/default/task-a", []byte("result"))
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
}

func TestInternalTaskSessionAndMessageAuthorization(t *testing.T) { //nolint:gocyclo // Security matrix stays together to prove one boundary.
	t.Setenv("POD_NAMESPACE", "orka-system")
	t.Setenv(harnessWrapperServiceAccountEnv, "agent-harness-wrapper")

	scheme := internalCallerAuthScheme(t)
	coordinatorA := internalCallerAuthTaskObject("coordinator-a", "coordinator-a-uid", "", "", "")
	coordinatorB := internalCallerAuthTaskObject("coordinator-b", "coordinator-b-uid", "", "", "")
	taskA := internalCallerAuthTaskObject("task-a", "task-a-uid", "task-a-job", "coordinator-a", "session-a")
	peerA := internalCallerAuthTaskObject("peer-a", "peer-a-uid", "peer-a-job", "coordinator-a", "session-peer-a")
	taskB := internalCallerAuthTaskObject("task-b", "task-b-uid", "task-b-job", "coordinator-b", "session-b")

	jobA := internalCallerAuthJob(taskA, "task-a-job", "task-a-job-uid")
	peerJob := internalCallerAuthJob(peerA, "peer-a-job", "peer-a-job-uid")
	jobB := internalCallerAuthJob(taskB, "task-b-job", "task-b-job-uid")
	oldJobA := internalCallerAuthJob(taskA, "task-a-old-job", "task-a-old-job-uid")
	podA := internalCallerAuthPod(taskA, "task-a-pod", "task-a-pod-uid", jobA)
	peerPod := internalCallerAuthPod(peerA, "peer-a-pod", "peer-a-pod-uid", peerJob)
	podB := internalCallerAuthPod(taskB, "task-b-pod", "task-b-pod-uid", jobB)
	oldPodA := internalCallerAuthPod(taskA, "task-a-old-pod", "task-a-old-pod-uid", oldJobA)
	failedPodA := internalCallerAuthPod(taskA, "task-a-failed-pod", "task-a-failed-pod-uid", jobA)
	failedPodA.Status.Phase = corev1.PodFailed

	harnessTask := internalCallerAuthTaskObject("harness-task", "harness-task-uid", "", "coordinator-a", "harness-session")
	harnessTask.Spec.Type = corev1alpha1.TaskTypeAgent
	harnessTask.Status.Phase = corev1alpha1.TaskPhaseRunning
	harnessTask.Annotations[harnessWrapperStartedAnnotation] = "true"
	harnessTask.Annotations[harnessWrapperTurnIDAnnotation] = "turn-1"
	harnessTask.Annotations[harnessWrapperRuntimeAnnotation] = "runtime-1"
	deletingHarnessTask := harnessTask.DeepCopy()
	deletingHarnessTask.Name = "deleting-harness-task"
	deletingHarnessTask.UID = types.UID("deleting-harness-task-uid")
	deletingHarnessTask.Finalizers = []string{labels.TaskFinalizer}
	deletionTime := metav1.NewTime(time.Now().UTC())
	deletingHarnessTask.DeletionTimestamp = &deletionTime
	completedHarnessTask := harnessTask.DeepCopy()
	completedHarnessTask.Name = "completed-harness-task"
	completedHarnessTask.UID = types.UID("completed-harness-task-uid")
	completedHarnessTask.Status.Phase = corev1alpha1.TaskPhaseSucceeded

	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		coordinatorA, coordinatorB, taskA, peerA, taskB, harnessTask, deletingHarnessTask, completedHarnessTask,
		jobA, peerJob, jobB, oldJobA,
		podA, peerPod, podB, oldPodA, failedPodA,
	).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	dataStore := sqlite.NewStore(db, ":memory:")
	h := NewInternalHandlers(dataStore, dataStore, dataStore, dataStore, dataStore, InternalHandlersConfig{Client: k8sClient})

	require.NoError(t, dataStore.SaveResult(context.Background(), "default", "task-a", []byte("original-result")))
	require.NoError(t, dataStore.SavePlan(context.Background(), "default", "task-a", &store.PlanState{
		Namespace: "default",
		TaskName:  "task-a",
		Summary:   "original-plan",
	}))
	for _, sessionName := range []string{"session-a", "session-peer-a", "session-b", "harness-session"} {
		require.NoError(t, dataStore.CreateSession(context.Background(), &store.SessionRecord{
			Namespace:   "default",
			Name:        sessionName,
			SessionType: "task",
		}))
		require.NoError(t, dataStore.AppendMessages(context.Background(), "default", sessionName, []store.SessionMessage{{
			Role:      "assistant",
			Content:   "secret from " + sessionName,
			Timestamp: time.Now().UTC(),
		}}))
	}
	require.NoError(t, dataStore.SendMessage(context.Background(), &store.Message{
		Namespace:  "default",
		FromTask:   "peer-a",
		ToTask:     "task-a",
		ParentTask: "coordinator-a",
		Content:    "message for task-a",
	}))

	currentWorker := internalCallerAuthorizationApp(h, internalCallerAuthWorkerUser("task-a-pod", "task-a-pod-uid"))
	unrelatedWorker := internalCallerAuthorizationApp(h, internalCallerAuthWorkerUser("task-b-pod", "task-b-pod-uid"))
	staleWorker := internalCallerAuthorizationApp(h, internalCallerAuthWorkerUser("task-a-old-pod", "task-a-old-pod-uid"))
	failedCurrentJobPod := internalCallerAuthorizationApp(h, internalCallerAuthWorkerUser("task-a-failed-pod", "task-a-failed-pod-uid"))
	nonServiceAccount := internalCallerAuthorizationApp(h, &UserInfo{
		Username:  "admin",
		Namespace: "default",
		AuthType:  AuthTypeOIDC,
	})
	missingNamespace := internalCallerAuthorizationApp(h, &UserInfo{
		Username: "system:serviceaccount:default:worker",
		AuthType: AuthTypeTokenReview,
		Extra: map[string]authenticationv1.ExtraValue{
			"authentication.kubernetes.io/pod-name": {"task-a-pod"},
			"authentication.kubernetes.io/pod-uid":  {"task-a-pod-uid"},
		},
	})
	harnessWrapper := internalCallerAuthorizationApp(h, &UserInfo{
		Username:  "system:serviceaccount:orka-system:agent-harness-wrapper",
		Namespace: "orka-system",
		AuthType:  AuthTypeTokenReview,
	})

	t.Run("non ServiceAccount identity cannot overwrite result", func(t *testing.T) {
		resp := internalCallerAuthorizationRequest(t, nonServiceAccount, http.MethodPost, "/internal/v1/results/default/task-a", []byte("attacker-result"))
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
	t.Run("TokenReview identity requires non-empty matching namespace", func(t *testing.T) {
		resp := internalCallerAuthorizationRequest(t, missingNamespace, http.MethodPost, "/internal/v1/results/default/task-a", []byte("missing-namespace-result"))
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
	t.Run("unrelated same-namespace worker cannot overwrite result", func(t *testing.T) {
		resp := internalCallerAuthorizationRequest(t, unrelatedWorker, http.MethodPost, "/internal/v1/results/default/task-a", []byte("cross-task-result"))
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
	t.Run("stale pod cannot overwrite result", func(t *testing.T) {
		resp := internalCallerAuthorizationRequest(t, staleWorker, http.MethodPost, "/internal/v1/results/default/task-a", []byte("stale-result"))
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
	t.Run("failed pod from current Job cannot overwrite result", func(t *testing.T) {
		resp := internalCallerAuthorizationRequest(t, failedCurrentJobPod, http.MethodPost, "/internal/v1/results/default/task-a", []byte("failed-pod-result"))
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
	result, err := dataStore.GetResult(context.Background(), "default", "task-a")
	require.NoError(t, err)
	require.Equal(t, []byte("original-result"), result)

	t.Run("unrelated worker cannot read or overwrite plan", func(t *testing.T) {
		planBody, marshalErr := json.Marshal(map[string]any{"summary": "cross-task-plan"})
		require.NoError(t, marshalErr)
		writeResp := internalCallerAuthorizationRequest(t, unrelatedWorker, http.MethodPost, "/internal/v1/plans/default/task-a", planBody)
		require.Equal(t, http.StatusForbidden, writeResp.StatusCode)
		readResp := internalCallerAuthorizationRequest(t, unrelatedWorker, http.MethodGet, "/internal/v1/plans/default/task-a", nil)
		require.Equal(t, http.StatusForbidden, readResp.StatusCode)
	})
	plan, err := dataStore.GetPlan(context.Background(), "default", "task-a")
	require.NoError(t, err)
	require.Equal(t, "original-plan", plan.Summary)

	t.Run("unrelated worker cannot read another task transcript", func(t *testing.T) {
		resp := internalCallerAuthorizationRequest(t, unrelatedWorker, http.MethodGet, "/internal/v1/sessions/default/session-a/transcript", nil)
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		searchResp := internalCallerAuthorizationRequest(t, unrelatedWorker, http.MethodGet, "/internal/v1/sessions/default/search?query=secret&sessionName=session-a", nil)
		require.Equal(t, http.StatusForbidden, searchResp.StatusCode)
	})

	t.Run("transcript search limits only authorized sessions", func(t *testing.T) {
		now := time.Now().UTC()
		require.NoError(t, dataStore.AppendMessages(context.Background(), "default", "session-a", []store.SessionMessage{{
			Role: "assistant", Content: "authorization needle", Timestamp: now.Add(-time.Hour),
		}}))
		for i := range 12 {
			require.NoError(t, dataStore.AppendMessages(context.Background(), "default", "session-b", []store.SessionMessage{{
				Role: "assistant", Content: "authorization needle", Timestamp: now.Add(time.Duration(i) * time.Second),
			}}))
		}
		resp := internalCallerAuthorizationRequest(t, currentWorker, http.MethodGet, "/internal/v1/sessions/default/search?query=authorization%20needle&limit=1", nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var results []store.TranscriptSearchResult
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&results))
		require.Len(t, results, 1)
		require.Equal(t, "session-a", results[0].SessionName)
	})

	t.Run("message sender recipient and inbox stay inside coordination scope", func(t *testing.T) {
		spoofBody, marshalErr := json.Marshal(map[string]string{
			"fromTask": "task-a", "toTask": "peer-a", "parentTask": "coordinator-a", "content": "spoofed",
		})
		require.NoError(t, marshalErr)
		spoofResp := internalCallerAuthorizationRequest(t, unrelatedWorker, http.MethodPost, "/internal/v1/messages/default", spoofBody)
		require.Equal(t, http.StatusForbidden, spoofResp.StatusCode)

		crossTreeBody, marshalErr := json.Marshal(map[string]string{
			"fromTask": "task-a", "toTask": "task-b", "parentTask": "coordinator-a", "content": "cross-tree",
		})
		require.NoError(t, marshalErr)
		crossTreeResp := internalCallerAuthorizationRequest(t, currentWorker, http.MethodPost, "/internal/v1/messages/default", crossTreeBody)
		require.Equal(t, http.StatusForbidden, crossTreeResp.StatusCode)

		readResp := internalCallerAuthorizationRequest(t, unrelatedWorker, http.MethodGet, "/internal/v1/messages/default/task-a?parentTask=coordinator-a&markRead=false", nil)
		require.Equal(t, http.StatusForbidden, readResp.StatusCode)
	})

	t.Run("current worker retains task session and sibling coordination access", func(t *testing.T) {
		resultResp := internalCallerAuthorizationRequest(t, currentWorker, http.MethodPost, "/internal/v1/results/default/task-a", []byte("current-result"))
		require.Equal(t, http.StatusNoContent, resultResp.StatusCode)

		planBody, marshalErr := json.Marshal(map[string]any{"summary": "current-plan"})
		require.NoError(t, marshalErr)
		planResp := internalCallerAuthorizationRequest(t, currentWorker, http.MethodPost, "/internal/v1/plans/default/task-a", planBody)
		require.Equal(t, http.StatusNoContent, planResp.StatusCode)
		getPlanResp := internalCallerAuthorizationRequest(t, currentWorker, http.MethodGet, "/internal/v1/plans/default/task-a", nil)
		require.Equal(t, http.StatusOK, getPlanResp.StatusCode)

		transcriptResp := internalCallerAuthorizationRequest(t, currentWorker, http.MethodGet, "/internal/v1/sessions/default/session-a/transcript", nil)
		require.Equal(t, http.StatusOK, transcriptResp.StatusCode)
		searchResp := internalCallerAuthorizationRequest(t, currentWorker, http.MethodGet, "/internal/v1/sessions/default/search?query=secret&sessionName=session-a", nil)
		require.Equal(t, http.StatusOK, searchResp.StatusCode)

		messageBody, marshalErr := json.Marshal(map[string]string{
			"fromTask": "task-a", "toTask": "peer-a", "parentTask": "coordinator-a", "content": "valid sibling message",
		})
		require.NoError(t, marshalErr)
		messageResp := internalCallerAuthorizationRequest(t, currentWorker, http.MethodPost, "/internal/v1/messages/default", messageBody)
		require.Equal(t, http.StatusNoContent, messageResp.StatusCode)
		inboxResp := internalCallerAuthorizationRequest(t, currentWorker, http.MethodGet, "/internal/v1/messages/default/task-a?parentTask=coordinator-a&markRead=false", nil)
		require.Equal(t, http.StatusOK, inboxResp.StatusCode)
	})

	t.Run("valid harness wrapper retains annotated task access", func(t *testing.T) {
		resultResp := internalCallerAuthorizationRequest(t, harnessWrapper, http.MethodPost, "/internal/v1/results/default/harness-task", []byte("harness-result"))
		require.Equal(t, http.StatusNoContent, resultResp.StatusCode)

		planBody, marshalErr := json.Marshal(map[string]any{"summary": "harness-plan"})
		require.NoError(t, marshalErr)
		planResp := internalCallerAuthorizationRequest(t, harnessWrapper, http.MethodPost, "/internal/v1/plans/default/harness-task", planBody)
		require.Equal(t, http.StatusNoContent, planResp.StatusCode)
		transcriptResp := internalCallerAuthorizationRequest(t, harnessWrapper, http.MethodGet, "/internal/v1/sessions/default/harness-session/transcript", nil)
		require.Equal(t, http.StatusOK, transcriptResp.StatusCode)
		searchResp := internalCallerAuthorizationRequest(t, harnessWrapper, http.MethodGet, "/internal/v1/sessions/default/search?query=secret&excludeSessionName=harness-session", nil)
		require.Equal(t, http.StatusOK, searchResp.StatusCode)
		siblingSearchResp := internalCallerAuthorizationRequest(t, harnessWrapper, http.MethodGet, "/internal/v1/sessions/default/search?query=secret&sessionName=session-peer-a&excludeSessionName=harness-session", nil)
		require.Equal(t, http.StatusOK, siblingSearchResp.StatusCode)
	})

	t.Run("deleting harness task cannot write after finalizer cleanup starts", func(t *testing.T) {
		resultResp := internalCallerAuthorizationRequest(t, harnessWrapper, http.MethodPost, "/internal/v1/results/default/deleting-harness-task", []byte("late-result"))
		require.Equal(t, http.StatusForbidden, resultResp.StatusCode)
		messageBody, marshalErr := json.Marshal(map[string]string{
			"fromTask": "deleting-harness-task", "toTask": "*", "parentTask": "coordinator-a", "content": "late-message",
		})
		require.NoError(t, marshalErr)
		messageResp := internalCallerAuthorizationRequest(t, harnessWrapper, http.MethodPost, "/internal/v1/messages/default", messageBody)
		require.Equal(t, http.StatusForbidden, messageResp.StatusCode)
	})

	t.Run("completed harness task cannot overwrite terminal data", func(t *testing.T) {
		resp := internalCallerAuthorizationRequest(t, harnessWrapper, http.MethodPost, "/internal/v1/results/default/completed-harness-task", []byte("late-result"))
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
}

func TestInternalCoordinationAuthorizationRejectsRecreatedParentUID(t *testing.T) {
	scheme := internalCallerAuthScheme(t)
	replacementParent := internalCallerAuthTaskObject("reused-parent", "reused-parent-new-uid", "", "", "")
	staleChild := internalCallerAuthTaskObject("stale-child", "stale-child-uid", "stale-child-job", "reused-parent", "stale-session")
	staleChild.OwnerReferences[0].UID = types.UID("reused-parent-old-uid")
	freshChild := internalCallerAuthTaskObject("fresh-child", "fresh-child-uid", "fresh-child-job", "reused-parent", "fresh-session")
	freshChild.OwnerReferences[0].UID = replacementParent.UID
	staleJob := internalCallerAuthJob(staleChild, "stale-child-job", "stale-child-job-uid")
	stalePod := internalCallerAuthPod(staleChild, "stale-child-pod", "stale-child-pod-uid", staleJob)
	freshJob := internalCallerAuthJob(freshChild, "fresh-child-job", "fresh-child-job-uid")
	freshPod := internalCallerAuthPod(freshChild, "fresh-child-pod", "fresh-child-pod-uid", freshJob)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		replacementParent, staleChild, freshChild, staleJob, stalePod, freshJob, freshPod,
	).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	dataStore := sqlite.NewStore(db, ":memory:")
	for _, sessionName := range []string{"stale-session", "fresh-session"} {
		require.NoError(t, dataStore.CreateSession(context.Background(), &store.SessionRecord{
			Namespace: "default", Name: sessionName, SessionType: "task",
		}))
	}
	h := NewInternalHandlers(dataStore, dataStore, dataStore, dataStore, dataStore, InternalHandlersConfig{Client: k8sClient})
	app := internalCallerAuthorizationApp(h, internalCallerAuthWorkerUser("stale-child-pod", "stale-child-pod-uid"))

	transcriptResp := internalCallerAuthorizationRequest(t, app, http.MethodGet, "/internal/v1/sessions/default/fresh-session/transcript", nil)
	require.Equal(t, http.StatusForbidden, transcriptResp.StatusCode)
	messageBody, err := json.Marshal(map[string]string{
		"fromTask": "stale-child", "toTask": "fresh-child", "parentTask": "reused-parent", "content": "cross-generation",
	})
	require.NoError(t, err)
	messageResp := internalCallerAuthorizationRequest(t, app, http.MethodPost, "/internal/v1/messages/default", messageBody)
	require.Equal(t, http.StatusForbidden, messageResp.StatusCode)
}

func TestHarnessSessionAuthorizationFailsClosedWithoutClient(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "orka-system")
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	dataStore := sqlite.NewStore(db, ":memory:")
	h := NewInternalHandlers(dataStore, dataStore, dataStore, dataStore, dataStore)
	app := internalCallerAuthorizationApp(h, &UserInfo{
		Username:  "system:serviceaccount:orka-system:agent-harness-wrapper",
		Namespace: "orka-system",
		AuthType:  AuthTypeTokenReview,
	})

	resp := internalCallerAuthorizationRequest(t, app, http.MethodGet, "/internal/v1/sessions/default/session/transcript", nil)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestHarnessSessionAuthorizationRejectsAmbiguousTaskAndSessionHint(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "orka-system")
	scheme := internalCallerAuthScheme(t)
	parent := internalCallerAuthTaskObject("parent", "parent-uid", "", "", "")
	taskNameMatch := internalCallerAuthTaskObject("collision", "task-name-match-uid", "", "parent", "session-one")
	sessionNameMatch := internalCallerAuthTaskObject("other-task", "session-name-match-uid", "", "parent", "collision")
	for _, task := range []*corev1alpha1.Task{taskNameMatch, sessionNameMatch} {
		task.Spec.Type = corev1alpha1.TaskTypeAgent
		task.Annotations[harnessWrapperStartedAnnotation] = "true"
		task.Annotations[harnessWrapperTurnIDAnnotation] = "turn-" + task.Name
		task.Annotations[harnessWrapperRuntimeAnnotation] = "runtime-" + task.Name
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(parent, taskNameMatch, sessionNameMatch).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	dataStore := sqlite.NewStore(db, ":memory:")
	h := NewInternalHandlers(dataStore, dataStore, dataStore, dataStore, dataStore, InternalHandlersConfig{Client: k8sClient})
	app := internalCallerAuthorizationApp(h, &UserInfo{
		Username:  "system:serviceaccount:orka-system:agent-harness-wrapper",
		Namespace: "orka-system",
		AuthType:  AuthTypeTokenReview,
	})

	resp := internalCallerAuthorizationRequest(t, app, http.MethodGet, "/internal/v1/sessions/default/search?query=needle&excludeSessionName=collision", nil)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestNestedCoordinatorCanUseOwnChildMessageScope(t *testing.T) {
	scheme := internalCallerAuthScheme(t)
	root := internalCallerAuthTaskObject("root", "root-uid", "", "", "")
	nested := internalCallerAuthTaskObject("nested", "nested-uid", "nested-job", "root", "")
	child := internalCallerAuthTaskObject("child", "child-uid", "", "nested", "")
	nestedJob := internalCallerAuthJob(nested, "nested-job", "nested-job-uid")
	nestedPod := internalCallerAuthPod(nested, "nested-pod", "nested-pod-uid", nestedJob)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(root, nested, child, nestedJob, nestedPod).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	dataStore := sqlite.NewStore(db, ":memory:")
	require.NoError(t, dataStore.SendMessage(context.Background(), &store.Message{
		Namespace: "default", FromTask: "child", ToTask: "nested", ParentTask: "nested", Content: "child update",
	}))
	h := NewInternalHandlers(dataStore, dataStore, dataStore, dataStore, dataStore, InternalHandlersConfig{Client: k8sClient})
	app := internalCallerAuthorizationApp(h, internalCallerAuthWorkerUser("nested-pod", "nested-pod-uid"))
	body, err := json.Marshal(map[string]string{
		"fromTask": "nested", "toTask": "child", "parentTask": "nested", "content": "parent reply",
	})
	require.NoError(t, err)

	sendResp := internalCallerAuthorizationRequest(t, app, http.MethodPost, "/internal/v1/messages/default", body)
	require.Equal(t, http.StatusNoContent, sendResp.StatusCode)
	inboxResp := internalCallerAuthorizationRequest(t, app, http.MethodGet, "/internal/v1/messages/default/nested?parentTask=nested&markRead=false", nil)
	require.Equal(t, http.StatusOK, inboxResp.StatusCode)
	var messages []store.Message
	require.NoError(t, json.NewDecoder(inboxResp.Body).Decode(&messages))
	require.Len(t, messages, 1)
	require.Equal(t, "child update", messages[0].Content)
}

func internalCallerAuthorizationApp(h *InternalHandlers, userInfo *UserInfo) *fiber.App {
	app := fiber.New()
	if userInfo != nil {
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, userInfo)
			return c.Next()
		})
	}
	app.Post("/internal/v1/results/:namespace/:taskName", h.SubmitResult)
	app.Post("/internal/v1/plans/:namespace/:taskName", h.SubmitPlan)
	app.Get("/internal/v1/plans/:namespace/:taskName", h.GetPlan)
	app.Get("/internal/v1/sessions/:namespace/search", h.SearchTranscript)
	app.Get("/internal/v1/sessions/:namespace/:name/transcript", h.GetSessionTranscript)
	app.Post("/internal/v1/messages/:namespace", h.SendMessage)
	app.Get("/internal/v1/messages/:namespace/:taskName", h.GetMessages)
	return app
}

func internalCallerAuthorizationRequest(t *testing.T, app *fiber.App, method, target string, body []byte) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, target, bytes.NewReader(body))
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 30 * time.Second, FailOnTimeout: true})
	require.NoError(t, err)
	return resp
}

func internalCallerAuthTaskObject(name, uid, jobName, parentTask, sessionName string) *corev1alpha1.Task {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "default",
			UID:         types.UID(uid),
			Labels:      map[string]string{},
			Annotations: map[string]string{},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
		Status: corev1alpha1.TaskStatus{
			JobName: jobName,
			Phase:   corev1alpha1.TaskPhaseRunning,
		},
	}
	if parentTask != "" {
		task.Labels[labels.LabelParentTask] = labels.SelectorValue(parentTask)
		task.Annotations[labels.AnnotationParentTaskName] = parentTask
		task.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: corev1alpha1.GroupVersion.String(),
			Kind:       "Task", //nolint:goconst // Kubernetes owner kind.
			Name:       parentTask,
			UID:        types.UID(parentTask + "-uid"),
		}}
	}
	if sessionName != "" {
		task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: sessionName}
	}
	return task
}

func internalCallerAuthScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, batchv1.AddToScheme(scheme))
	return scheme
}

func internalCallerAuthTask() *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task-a", Namespace: "default", UID: types.UID("task-uid")},
		Status:     corev1alpha1.TaskStatus{JobName: "job-a"},
	}
}

func internalCallerAuthJob(task *corev1alpha1.Task, name, uid string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: task.Namespace,
			UID:       types.UID(uid),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(),
				Kind:       "Task", //nolint:goconst // Kubernetes owner kind.
				Name:       task.Name,
				UID:        task.UID,
			}},
		},
	}
}

func internalCallerAuthPod(task *corev1alpha1.Task, name, uid string, job *batchv1.Job) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: task.Namespace,
			UID:       types.UID(uid),
			Labels:    map[string]string{labels.LabelTask: labels.SelectorValue(task.Name)},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: batchv1.SchemeGroupVersion.String(),
				Kind:       "Job", //nolint:goconst // Kubernetes owner kind.
				Name:       job.Name,
				UID:        job.UID,
			}},
		},
	}
}

func internalCallerAuthWorkerUser(podName, podUID string) *UserInfo {
	return &UserInfo{
		Username:  "system:serviceaccount:default:worker",
		Namespace: "default",
		AuthType:  AuthTypeTokenReview,
		Extra: map[string]authenticationv1.ExtraValue{
			"authentication.kubernetes.io/pod-name": {podName},
			"authentication.kubernetes.io/pod-uid":  {podUID},
		},
	}
}

func errorsAsFiber(err error, target **fiber.Error) bool {
	return errors.As(err, target)
}
