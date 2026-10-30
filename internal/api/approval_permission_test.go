/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/storetest"
)

const permissionApprovalTestID = "permission-approval-1"

type permissionApprovalFixture struct {
	handlers *Handlers
	events   *storetest.FakeExecutionEventStore
	task     *corev1alpha1.Task
}

func newPermissionApprovalFixture(t *testing.T, kind string, expiresAt time.Time) *permissionApprovalFixture {
	t.Helper()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "permission-task", UID: types.UID("permission-task-uid"),
			Annotations: map[string]string{labels.AnnotationRequestedBySource: labels.RequestedBySourceAPI},
		},
		Spec: corev1alpha1.TaskSpec{RequestedBy: &corev1alpha1.RequestedBy{Issuer: "https://issuer.example", Subject: "alice"}},
		Status: corev1alpha1.TaskStatus{Phase: "Running", Execution: &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStateRunning, Attempt: 1, PromptID: "prompt-1",
			RuntimeInstanceID: "instance-1", RuntimeSessionUID: "session-1", RuntimeSessionGeneration: 1,
			RuntimeSessionSupervisorBootID: "boot-1", ControllerEpoch: 3,
		}},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
	eventStore := storetest.NewFakeExecutionEventStore()
	content, err := json.Marshal(map[string]any{
		"approvalID": permissionApprovalTestID, "kind": kind, "action": "Allow the agent to run Bash",
		"taskUID": string(task.UID), "expiresAt": expiresAt,
		"options": []approvals.Option{{OptionID: "allow", Kind: "allow_once"}, {OptionID: "reject", Kind: "reject_once"}},
		"binding": approvals.CallBinding{
			TaskAttempt: 1, PromptID: "prompt-1", RuntimeSessionUID: "session-1", RuntimeSessionGeneration: 1,
			RuntimeInstanceIDDigest: store.CanonicalBytesDigest([]byte("instance-1")),
			SupervisorBootIDDigest:  store.CanonicalBytesDigest([]byte("boot-1")), ControllerEpoch: 3,
			OperationIDDigest: store.CanonicalBytesDigest([]byte("operation")), RequestDigest: store.CanonicalBytesDigest([]byte("request")),
		},
	})
	require.NoError(t, err)
	_, err = eventStore.AppendExecutionEvent(t.Context(), &store.ExecutionEvent{
		Namespace: task.Namespace, StreamType: events.ExecutionEventStreamTypeTask, StreamID: task.Name, TaskName: task.Name,
		Type: events.ExecutionEventTypeApprovalRequested, ToolCallID: permissionApprovalTestID, Content: content,
	})
	require.NoError(t, err)
	return &permissionApprovalFixture{
		handlers: &Handlers{client: kube, executionEventStore: eventStore},
		events:   eventStore, task: task,
	}
}

func (f *permissionApprovalFixture) status(t *testing.T) string {
	t.Helper()
	listed, err := approvals.ListEvents(t.Context(), f.events, f.task.Namespace, f.task.Name)
	require.NoError(t, err)
	derived := approvals.Derive(listed, time.Time{})
	require.Len(t, derived, 1)
	return derived[0].Status
}

func oidcUser(subject string) *UserInfo {
	return &UserInfo{AuthType: AuthTypeOIDC, Issuer: "https://issuer.example", Subject: subject, Username: subject}
}

func requireFiberStatus(t *testing.T, err error, status int) {
	t.Helper()
	var fiberErr *fiber.Error
	require.True(t, errors.As(err, &fiberErr), "error %v is not an HTTP error", err)
	require.Equal(t, status, fiberErr.Code, fiberErr.Message)
}

func TestDecideTaskApprovalAsLetsOnlyTheRequesterDecide(t *testing.T) {
	expires := time.Now().UTC().Add(time.Minute)
	for _, test := range []struct {
		name    string
		ui      *UserInfo
		approve bool
		status  int
		want    string
	}{
		{name: "requester approves", ui: oidcUser("alice"), approve: true, want: approvals.StatusApproved},
		{name: "requester declines", ui: oidcUser("alice"), want: approvals.StatusDeclined},
		{name: "another person", ui: oidcUser("bob"), approve: true, status: fiber.StatusForbidden, want: approvals.StatusPending},
		{name: "same subject at another issuer", ui: &UserInfo{AuthType: AuthTypeOIDC, Issuer: "https://other.example", Subject: "alice"},
			approve: true, status: fiber.StatusForbidden, want: approvals.StatusPending},
		{name: "service account", ui: &UserInfo{AuthType: AuthTypeTokenReview, Username: "system:serviceaccount:default:alice", Subject: "alice"},
			approve: true, status: fiber.StatusForbidden, want: approvals.StatusPending},
		{name: "no identity", approve: true, status: fiber.StatusForbidden, want: approvals.StatusPending},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newPermissionApprovalFixture(t, approvals.KindPermission, expires)
			err := f.handlers.DecideTaskApprovalAs(context.Background(), test.ui, f.task.Namespace, f.task.Name, permissionApprovalTestID, test.approve)
			if test.status == 0 {
				require.NoError(t, err)
			} else {
				requireFiberStatus(t, err, test.status)
			}
			require.Equal(t, test.want, f.status(t))
		})
	}
}

func TestDecideTaskApprovalAsRefusesOtherApprovalsAndClosedRequests(t *testing.T) {
	t.Run("not a permission request", func(t *testing.T) {
		f := newPermissionApprovalFixture(t, "", time.Now().UTC().Add(time.Minute))
		err := f.handlers.DecideTaskApprovalAs(context.Background(), oidcUser("alice"), f.task.Namespace, f.task.Name, permissionApprovalTestID, true)
		requireFiberStatus(t, err, fiber.StatusForbidden)
		require.Equal(t, approvals.StatusPending, f.status(t))
	})
	t.Run("expired", func(t *testing.T) {
		f := newPermissionApprovalFixture(t, approvals.KindPermission, time.Now().UTC().Add(-time.Second))
		err := f.handlers.DecideTaskApprovalAs(context.Background(), oidcUser("alice"), f.task.Namespace, f.task.Name, permissionApprovalTestID, true)
		requireFiberStatus(t, err, fiber.StatusConflict)
	})
	t.Run("prompt no longer running", func(t *testing.T) {
		f := newPermissionApprovalFixture(t, approvals.KindPermission, time.Now().UTC().Add(time.Minute))
		current := &corev1alpha1.Task{}
		require.NoError(t, f.handlers.client.Get(t.Context(), types.NamespacedName{Namespace: f.task.Namespace, Name: f.task.Name}, current))
		current.Status.Execution.PromptID = "prompt-2"
		require.NoError(t, f.handlers.client.Status().Update(t.Context(), current))
		err := f.handlers.DecideTaskApprovalAs(context.Background(), oidcUser("alice"), f.task.Namespace, f.task.Name, permissionApprovalTestID, true)
		requireFiberStatus(t, err, fiber.StatusConflict)
		require.Equal(t, approvals.StatusPending, f.status(t))
	})
	t.Run("repeated decision", func(t *testing.T) {
		f := newPermissionApprovalFixture(t, approvals.KindPermission, time.Now().UTC().Add(time.Minute))
		decide := func(approve bool) error {
			return f.handlers.DecideTaskApprovalAs(context.Background(), oidcUser("alice"), f.task.Namespace, f.task.Name, permissionApprovalTestID, approve)
		}
		require.NoError(t, decide(true))
		require.NoError(t, decide(true), "the same decision again is accepted")
		requireFiberStatus(t, decide(false), fiber.StatusConflict)
		require.Equal(t, approvals.StatusApproved, f.status(t))
	})
	t.Run("namespace isolation", func(t *testing.T) {
		f := newPermissionApprovalFixture(t, approvals.KindPermission, time.Now().UTC().Add(time.Minute))
		f.handlers.enforceNamespaceIsolation = true
		ui := oidcUser("alice")
		ui.Namespace = "elsewhere"
		err := f.handlers.DecideTaskApprovalAs(context.Background(), ui, f.task.Namespace, f.task.Name, permissionApprovalTestID, true)
		requireFiberStatus(t, err, fiber.StatusForbidden)
	})
}

func TestDecisionRouteLetsOnlyTheRequesterDecidePermissionRequests(t *testing.T) {
	f := newPermissionApprovalFixture(t, approvals.KindPermission, time.Now().UTC().Add(time.Minute))
	_, err := f.handlers.decideApproval(context.Background(), oidcUser("bob"), f.task, permissionApprovalTestID, approvalDecisionApprove, "", false)
	requireFiberStatus(t, err, fiber.StatusForbidden)
	decided, err := f.handlers.decideApproval(context.Background(), oidcUser("alice"), f.task, permissionApprovalTestID, approvalDecisionApprove, "", false)
	require.NoError(t, err)
	require.Equal(t, approvals.StatusApproved, decided.Status)
	require.Equal(t, approvals.KindPermission, decided.Kind)
	require.Len(t, decided.Options, 2)
}

func TestDecideTaskApprovalAsAppliesContextTokenScopes(t *testing.T) {
	for _, test := range []struct {
		name   string
		scopes []string
		status int
		want   string
	}{
		{name: "missing scope", scopes: []string{"orka:tasks:read"}, status: fiber.StatusForbidden, want: approvals.StatusPending},
		{name: "granted scope", scopes: []string{"orka:tasks:update"}, want: approvals.StatusApproved},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newPermissionApprovalFixture(t, approvals.KindPermission, time.Now().UTC().Add(time.Minute))
			f.handlers.contextTokenAuthorization = ContextTokenAuthorizationConfig{
				Mode: ContextTokenAuthorizationModeEnforce, TaskUpdateScopes: []string{"orka:tasks:update"},
			}
			ui := &UserInfo{
				AuthType: AuthTypeContextToken, Issuer: "https://issuer.example", Subject: "alice",
				ContextToken: &ContextToken{Issuer: "https://issuer.example", Subject: "alice", Scopes: test.scopes},
			}
			err := f.handlers.DecideTaskApprovalAs(context.Background(), ui, f.task.Namespace, f.task.Name, permissionApprovalTestID, true)
			if test.status == 0 {
				require.NoError(t, err)
			} else {
				requireFiberStatus(t, err, test.status)
			}
			require.Equal(t, test.want, f.status(t))
		})
	}
}
