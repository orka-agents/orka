package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	gatewayruntime "github.com/orka-agents/orka/internal/gateway"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/storetest"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

func TestForkWorkspaceUsesIndependentSessionAndExplicitData(t *testing.T) {
	for _, selected := range []bool{false, true} {
		spec := corev1alpha1.TaskSpec{
			SessionRef: &corev1alpha1.SessionReference{Name: "source"},
			Execution: &corev1alpha1.ExecutionSpec{Workspace: &corev1alpha1.ExecutionWorkspaceSpec{
				ClassRef:    &corev1alpha1.WorkspaceClassReference{Name: "session-only"},
				ReusePolicy: corev1alpha1.WorkspaceReusePolicySession,
				OnDetach:    corev1alpha1.WorkspaceOnDetachSuspend,
				RestoreFrom: &corev1alpha1.WorkspaceCheckpointReference{Name: "source-origin"},
			}},
		}
		var ref *corev1alpha1.WorkspaceCheckpointReference
		if selected {
			ref = &corev1alpha1.WorkspaceCheckpointReference{Name: "selected", UID: "checkpoint-uid", Digest: "sha256:" + strings.Repeat("a", 64)}
		}
		require.NoError(t, applyForkExecutionCheckpoint(&spec, ref, "new-fork"))
		require.Equal(t, "new-fork", spec.SessionRef.Name)
		require.True(t, spec.SessionRef.Create)
		require.Equal(t, corev1alpha1.WorkspaceReusePolicySession, spec.Execution.Workspace.ReusePolicy)
		require.Equal(t, corev1alpha1.WorkspaceOnDetachSuspend, spec.Execution.Workspace.OnDetach)
		require.Equal(t, ref, spec.Execution.Workspace.RestoreFrom)
	}
}

func TestForkWorkspaceIdempotencyRequiresSameCheckpoint(t *testing.T) {
	checkpoint := &corev1alpha1.WorkspaceCheckpointReference{
		Name: "selected-data", UID: "checkpoint-uid", Digest: "sha256:" + strings.Repeat("a", 64),
	}
	changedName, changedUID, changedDigest := checkpoint.DeepCopy(), checkpoint.DeepCopy(), checkpoint.DeepCopy()
	changedName.Name = "other-data"
	changedUID.UID = "replacement-uid"
	changedDigest.Digest = "sha256:" + strings.Repeat("b", 64)
	for _, tc := range []struct {
		name       string
		initial    *corev1alpha1.WorkspaceCheckpointReference
		retry      *corev1alpha1.WorkspaceCheckpointReference
		wantStatus int
	}{
		{name: "same checkpoint", initial: checkpoint, retry: checkpoint.DeepCopy(), wantStatus: http.StatusOK},
		{name: "no checkpoint", wantStatus: http.StatusOK},
		{name: "changed name", initial: checkpoint, retry: changedName, wantStatus: http.StatusConflict},
		{name: "changed UID", initial: checkpoint, retry: changedUID, wantStatus: http.StatusConflict},
		{name: "changed digest", initial: checkpoint, retry: changedDigest, wantStatus: http.StatusConflict},
		{name: "removed checkpoint", initial: checkpoint, wantStatus: http.StatusConflict},
		{name: "added checkpoint", retry: checkpoint, wantStatus: http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eventStore := storetest.NewFakeExecutionEventStore()
			appendTestTaskEvent(t, eventStore, "source", events.ExecutionEventTypeTaskStarted)
			source := testTask("default", "source")
			source.Spec.Type = corev1alpha1.TaskTypeAgent
			source.Spec.Execution = &corev1alpha1.ExecutionSpec{Workspace: &corev1alpha1.ExecutionWorkspaceSpec{
				ClassRef: &corev1alpha1.WorkspaceClassReference{Name: "substrate-data"},
			}}
			h, app := setupPostP0Handlers(t, eventStore, nil, source)
			h.clientset, _ = externalToolReviews(t, func(authorizationv1.SubjectAccessReviewSpec) (runtime.Object, error) {
				return externalToolReview(true), nil
			})
			app.Use(func(c fiber.Ctx) error {
				c.Locals(UserInfoContextKey, externalToolUser())
				return c.Next()
			})
			app.Post("/api/v1/tasks/:id/fork", h.ForkTask)
			doFork := func(ref *corev1alpha1.WorkspaceCheckpointReference) *http.Response {
				t.Helper()
				afterSeq := int64(1)
				body, err := json.Marshal(ForkTaskRequest{AfterSeq: &afterSeq, ExecutionCheckpoint: ref})
				require.NoError(t, err)
				request := httptest.NewRequest(http.MethodPost, "/api/v1/tasks/source/fork?namespace=default", bytes.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Idempotency-Key", "checkpoint-retry")
				response, err := app.Test(request)
				require.NoError(t, err)
				t.Cleanup(func() { _ = response.Body.Close() })
				return response
			}
			first := doFork(tc.initial)
			require.Equal(t, http.StatusCreated, first.StatusCode)
			var created ForkTaskResponse
			require.NoError(t, json.NewDecoder(first.Body).Decode(&created))
			retry := doFork(tc.retry)
			require.Equal(t, tc.wantStatus, retry.StatusCode)
			if tc.wantStatus == http.StatusOK {
				var recovered ForkTaskResponse
				require.NoError(t, json.NewDecoder(retry.Body).Decode(&recovered))
				require.Equal(t, created.NewTaskName, recovered.NewTaskName)
			}
			stored := &corev1alpha1.Task{}
			require.NoError(t, h.client.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: created.NewTaskName}, stored))
			require.Equal(t, tc.initial, stored.Spec.Execution.Workspace.RestoreFrom)
			tasks := &corev1alpha1.TaskList{}
			require.NoError(t, h.client.List(t.Context(), tasks))
			require.Len(t, tasks.Items, 2, "retry must preserve the source and its one fork")
			forkEvents, err := eventStore.ListExecutionEvents(t.Context(), store.ExecutionEventFilter{Namespace: "default", StreamID: created.NewTaskName})
			require.NoError(t, err)
			require.Len(t, forkEvents, 1, "retry must not append fork events")
			sourceEvents, err := eventStore.ListExecutionEvents(t.Context(), store.ExecutionEventFilter{Namespace: "default", StreamID: "source"})
			require.NoError(t, err)
			require.Len(t, sourceEvents, 3, "retry must not append source events")
		})
	}
}

func TestForkWorkspaceRejectsMalformedCheckpointBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name  string
		field string
		value string
	}{
		{name: "empty name", field: "name"},
		{name: "invalid name", field: "name", value: "invalid/name"},
		{name: "long name", field: "name", value: strings.Repeat("a", 254)},
		{name: "empty UID", field: "uid"},
		{name: "blank UID", field: "uid", value: " "},
		{name: "long UID", field: "uid", value: strings.Repeat("a", 129)},
		{name: "short digest", field: "digest", value: "sha256:abcd"},
		{name: "invalid digest", field: "digest", value: "sha256:" + strings.Repeat("z", 64)},
		{name: "uppercase digest", field: "digest", value: "sha256:" + strings.Repeat("A", 64)},
		{name: "missing digest prefix", field: "digest", value: strings.Repeat("a", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eventStore := storetest.NewFakeExecutionEventStore()
			appendTestTaskEvent(t, eventStore, "source", events.ExecutionEventTypeTaskStarted)
			source := testTask("default", "source")
			source.Spec.Execution = &corev1alpha1.ExecutionSpec{Workspace: &corev1alpha1.ExecutionWorkspaceSpec{
				ClassRef: &corev1alpha1.WorkspaceClassReference{Name: "substrate-data"},
			}}
			h, app := setupPostP0Handlers(t, eventStore, nil, source)
			app.Post("/api/v1/tasks/:id/fork", h.ForkTask)
			checkpoint := map[string]string{"name": "selected-data", "uid": "checkpoint-uid", "digest": "sha256:" + strings.Repeat("a", 64)}
			checkpoint[tc.field] = tc.value
			resp := testJSONRequest(t, app, http.MethodPost, "/api/v1/tasks/source/fork?namespace=default", map[string]any{
				"afterSeq": 1, "newTaskName": "invalid-fork", "executionCheckpoint": checkpoint,
			})
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)
			err := h.client.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: "invalid-fork"}, &corev1alpha1.Task{})
			require.True(t, apierrors.IsNotFound(err), "invalid input must not create a Task even with a client that skips CRD validation")
			forkEvents, err := eventStore.ListExecutionEvents(t.Context(), store.ExecutionEventFilter{Namespace: "default", StreamID: "invalid-fork"})
			require.NoError(t, err)
			require.Empty(t, forkEvents)
			sourceEvents, err := eventStore.ListExecutionEvents(t.Context(), store.ExecutionEventFilter{Namespace: "default", StreamID: "source"})
			require.NoError(t, err)
			require.Len(t, sourceEvents, 1, "invalid input must not append a fork event to the source")
		})
	}
}

func TestForkWorkspaceGatewayCheckpointAPI(t *testing.T) {
	for _, tc := range []struct {
		name           string
		missingSession bool
		allowData      bool
	}{
		{name: "gateway session", allowData: true},
		{name: "missing gateway transcript", missingSession: true, allowData: true},
		{name: "checkpoint use denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eventStore := storetest.NewFakeExecutionEventStore()
			appendTestTaskEvent(t, eventStore, "gateway-source", events.ExecutionEventTypeTaskStarted)
			source := testTask("default", "gateway-source")
			source.Spec.Type = corev1alpha1.TaskTypeAgent
			source.Spec.RequestedBy = &corev1alpha1.RequestedBy{Issuer: "gateway.orka.ai/default/namespace-uid/chat/gateway-uid"}
			source.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "gateway-session", PromptIncluded: true}
			source.Spec.Transaction = &corev1alpha1.TaskTransaction{ID: "gateway-transaction"}
			source.Labels = map[string]string{gatewayruntime.TaskGatewayEventLabel: "gateway-event"}
			source.Annotations = map[string]string{
				gatewayruntime.TaskGatewayEventAnnotation: "gateway-event",
				gatewayruntime.TaskGatewayNameAnnotation:  "chat",
			}
			source.Spec.Execution = &corev1alpha1.ExecutionSpec{Workspace: &corev1alpha1.ExecutionWorkspaceSpec{
				ClassRef:    &corev1alpha1.WorkspaceClassReference{Name: "session-only"},
				ReusePolicy: corev1alpha1.WorkspaceReusePolicySession,
				OnDetach:    corev1alpha1.WorkspaceOnDetachSuspend,
			}}
			sessions := &postP0FakeSessionStore{records: map[string]*store.SessionRecord{}}
			if !tc.missingSession {
				sessions.records["default/gateway-session"] = &store.SessionRecord{
					Namespace: "default", Name: "gateway-session", SessionType: store.SessionTypeGateway,
				}
			}
			h, app := setupPostP0Handlers(t, eventStore, sessions, source,
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: "namespace-uid"}},
				&gatewayv1alpha1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "chat", Namespace: "default", UID: "gateway-uid"}},
			)
			checkedData := false
			h.clientset, _ = externalToolReviews(t, func(spec authorizationv1.SubjectAccessReviewSpec) (runtime.Object, error) {
				if spec.ResourceAttributes.Resource == "executionworkspacecheckpoints" {
					checkedData = true
					require.Equal(t, "use", spec.ResourceAttributes.Verb)
					require.Equal(t, "selected-data", spec.ResourceAttributes.Name)
					return externalToolReview(tc.allowData), nil
				}
				return externalToolReview(true), nil
			})
			app.Use(func(c fiber.Ctx) error {
				c.Locals(UserInfoContextKey, externalToolUser())
				return c.Next()
			})
			app.Post("/api/v1/tasks/:id/fork", h.ForkTask)
			digest := "sha256:" + strings.Repeat("a", 64)
			resp := testJSONRequest(t, app, http.MethodPost, "/api/v1/tasks/gateway-source/fork?namespace=default", map[string]any{
				"afterSeq": 1, "newTaskName": "workspace-fork",
				"executionCheckpoint": map[string]string{"name": "selected-data", "uid": "checkpoint-uid", "digest": digest},
			})
			require.True(t, checkedData, "fork must authorize the selected filesystem separately from transcript access")
			created := &corev1alpha1.Task{}
			err := h.client.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: "workspace-fork"}, created)
			if !tc.allowData {
				require.Equal(t, http.StatusForbidden, resp.StatusCode)
				require.True(t, apierrors.IsNotFound(err))
				return
			}
			require.Equal(t, http.StatusCreated, resp.StatusCode)
			require.NoError(t, err)
			require.Equal(t, &corev1alpha1.SessionReference{Name: "workspace-fork", Create: true, Append: true}, created.Spec.SessionRef)
			require.Nil(t, created.Spec.Transaction)
			_, owned := gatewayruntime.TaskIdentity(created)
			require.False(t, owned)
			require.Equal(t, digest, created.Spec.Execution.Workspace.RestoreFrom.Digest)
			forkEvents, err := eventStore.ListExecutionEvents(t.Context(), store.ExecutionEventFilter{Namespace: "default", StreamID: "workspace-fork"})
			require.NoError(t, err)
			require.Len(t, forkEvents, 1)
			require.Equal(t, "workspace-fork", forkEvents[0].SessionName)
		})
	}
}
