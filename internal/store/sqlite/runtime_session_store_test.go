/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sozercan/orka/internal/harness"
	"github.com/sozercan/orka/internal/store"
)

const (
	runtimeSessionTestNamespace = "runtime-ns"
	runtimeSessionTestName      = "runtime-session"
	runtimeSessionTestTask      = "runtime-task"
	runtimeSessionTestAgent     = "runtime-agent"
	runtimeSessionNamespaceA    = "runtime-ns-a"
	runtimeSessionNamespaceB    = "runtime-ns-b"
)

func TestRuntimeSessionStoreCreateGetRoundTrip(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	createdAt := time.Date(2026, 6, 24, 10, 0, 0, 123, time.FixedZone("offset", -7*60*60))
	updatedAt := createdAt.Add(time.Minute)
	session := runtimeSessionFixture("runtime-a")
	session.CreatedAt = createdAt
	session.UpdatedAt = updatedAt
	session.IdleTimeout = 5 * time.Minute
	session.MaxLifetime = 2 * time.Hour

	if err := s.CreateRuntimeSession(ctx, &session); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	if !session.CreatedAt.Equal(createdAt.UTC()) || !session.UpdatedAt.Equal(updatedAt.UTC()) {
		t.Fatalf("normalized timestamps = %s/%s, want UTC %s/%s", session.CreatedAt, session.UpdatedAt, createdAt.UTC(), updatedAt.UTC())
	}

	got, err := s.GetRuntimeSession(ctx, runtimeSessionTestNamespace, "runtime-a")
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}
	assertRuntimeSessionEqual(t, *got, session)
}

func TestRuntimeSessionStoreCreateDefaults(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	session := runtimeSessionFixture(" runtime-defaults ")
	session.Owner.Namespace = " " + runtimeSessionTestNamespace + " "
	session.Owner.SessionName = " " + runtimeSessionTestName + " "
	session.Owner.ActiveTask = " " + runtimeSessionTestTask + " "
	session.Owner.AgentName = " " + runtimeSessionTestAgent + " "
	session.Owner.Provider = " kubernetes-service "
	session.State = ""
	session.CleanupPolicy = ""

	if err := s.CreateRuntimeSession(ctx, &session); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	if session.ID != "runtime-defaults" || session.Owner.Namespace != runtimeSessionTestNamespace || session.Owner.SessionName != runtimeSessionTestName {
		t.Fatalf("normalized identity = %#v", session)
	}
	if session.State != harness.RuntimeSessionStatePending {
		t.Fatalf("state = %q, want Pending", session.State)
	}
	if session.CleanupPolicy != harness.RuntimeCleanupPolicyDelete {
		t.Fatalf("cleanup policy = %q, want delete", session.CleanupPolicy)
	}
	if session.CreatedAt.IsZero() || session.UpdatedAt.IsZero() || !session.UpdatedAt.Equal(session.CreatedAt) {
		t.Fatalf("timestamps = %s/%s, want populated equal values", session.CreatedAt, session.UpdatedAt)
	}

	got, err := s.GetRuntimeSession(ctx, runtimeSessionTestNamespace, "runtime-defaults")
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}
	assertRuntimeSessionEqual(t, *got, session)
}

func TestRuntimeSessionStoreNamespaceOwnership(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	nsA := runtimeSessionFixture("runtime-shared")
	nsA.Owner.Namespace = runtimeSessionNamespaceA
	nsA.Owner.SessionName = runtimeSessionTestName
	nsB := runtimeSessionFixture("runtime-shared")
	nsB.Owner.Namespace = runtimeSessionNamespaceB
	nsB.Owner.SessionName = "session-b"

	if err := s.CreateRuntimeSession(ctx, &nsA); err != nil {
		t.Fatalf("CreateRuntimeSession ns-a: %v", err)
	}
	if err := s.CreateRuntimeSession(ctx, &nsB); err != nil {
		t.Fatalf("CreateRuntimeSession ns-b: %v", err)
	}

	gotA, err := s.GetRuntimeSession(ctx, runtimeSessionNamespaceA, "runtime-shared")
	if err != nil {
		t.Fatalf("GetRuntimeSession ns-a: %v", err)
	}
	gotB, err := s.GetRuntimeSession(ctx, runtimeSessionNamespaceB, "runtime-shared")
	if err != nil {
		t.Fatalf("GetRuntimeSession ns-b: %v", err)
	}
	if gotA.Owner.Namespace != runtimeSessionNamespaceA || gotA.Owner.SessionName != runtimeSessionTestName {
		t.Fatalf("ns-a row = %#v", gotA)
	}
	if gotB.Owner.Namespace != runtimeSessionNamespaceB || gotB.Owner.SessionName != "session-b" {
		t.Fatalf("ns-b row = %#v", gotB)
	}
	if _, err := s.GetRuntimeSession(ctx, "ns-c", "runtime-shared"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRuntimeSession ns-c error = %v, want ErrNotFound", err)
	}
	duplicate := runtimeSessionFixture("runtime-shared")
	duplicate.Owner.Namespace = runtimeSessionNamespaceA
	if err := s.CreateRuntimeSession(ctx, &duplicate); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("CreateRuntimeSession duplicate error = %v, want ErrConflict", err)
	}
}

func TestRuntimeSessionStoreListFiltersAndCursor(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	fixtures := []harness.RuntimeSession{
		runtimeSessionListFixture("runtime-1", "ns-list", "alpha", runtimeSessionTestTask, runtimeSessionTestAgent, harness.ProviderKindKubernetesService, harness.RuntimeSessionStatePending, harness.RuntimeCleanupPolicyDelete, base.Add(5*time.Minute)),
		runtimeSessionListFixture("runtime-2", "ns-list", "alpha", "task-b", "agent-b", harness.ProviderKindKubernetesService, harness.RuntimeSessionStateIdle, harness.RuntimeCleanupPolicyRetain, base.Add(4*time.Minute)),
		runtimeSessionListFixture("runtime-3", "ns-list", "alpha", "", runtimeSessionTestAgent, harness.ProviderKindKubernetesService, harness.RuntimeSessionStateDeleted, harness.RuntimeCleanupPolicyDelete, base.Add(3*time.Minute)),
		runtimeSessionListFixture("runtime-4", "ns-list", "beta", "task-c", runtimeSessionTestAgent, harness.ProviderKindSidecar, harness.RuntimeSessionStateReady, harness.RuntimeCleanupPolicySuspend, base.Add(2*time.Minute)),
		runtimeSessionListFixture("runtime-5", "other-ns", "alpha", runtimeSessionTestTask, runtimeSessionTestAgent, harness.ProviderKindKubernetesService, harness.RuntimeSessionStatePending, harness.RuntimeCleanupPolicyDelete, base.Add(time.Minute)),
	}
	for i := range fixtures {
		if err := s.CreateRuntimeSession(ctx, &fixtures[i]); err != nil {
			t.Fatalf("CreateRuntimeSession %s: %v", fixtures[i].ID, err)
		}
	}

	listed, cursor, err := s.ListRuntimeSessions(ctx, harness.RuntimeSessionFilter{Namespace: "ns-list"})
	if err != nil {
		t.Fatalf("ListRuntimeSessions default: %v", err)
	}
	assertRuntimeSessionIDs(t, listed, []harness.RuntimeSessionID{"runtime-1", "runtime-2", "runtime-4"})
	if cursor != "" {
		t.Fatalf("cursor = %q, want empty", cursor)
	}

	listed, _, err = s.ListRuntimeSessions(ctx, harness.RuntimeSessionFilter{Namespace: "ns-list", IncludeDeleted: true})
	if err != nil {
		t.Fatalf("ListRuntimeSessions include deleted: %v", err)
	}
	assertRuntimeSessionIDs(t, listed, []harness.RuntimeSessionID{"runtime-1", "runtime-2", "runtime-3", "runtime-4"})

	filterAssertions := []struct {
		name   string
		filter harness.RuntimeSessionFilter
		want   []harness.RuntimeSessionID
	}{
		{name: "state", filter: harness.RuntimeSessionFilter{States: []harness.RuntimeSessionState{harness.RuntimeSessionStateDeleted}}, want: []harness.RuntimeSessionID{"runtime-3"}},
		{name: "session", filter: harness.RuntimeSessionFilter{SessionName: "beta"}, want: []harness.RuntimeSessionID{"runtime-4"}},
		{name: "active task", filter: harness.RuntimeSessionFilter{ActiveTask: "task-b"}, want: []harness.RuntimeSessionID{"runtime-2"}},
		{name: "agent", filter: harness.RuntimeSessionFilter{AgentName: runtimeSessionTestAgent}, want: []harness.RuntimeSessionID{"runtime-1", "runtime-4"}},
		{name: "provider", filter: harness.RuntimeSessionFilter{Provider: harness.ProviderKindSidecar}, want: []harness.RuntimeSessionID{"runtime-4"}},
		{name: "cleanup", filter: harness.RuntimeSessionFilter{CleanupPolicies: []harness.RuntimeCleanupPolicy{harness.RuntimeCleanupPolicyRetain}}, want: []harness.RuntimeSessionID{"runtime-2"}},
	}
	for _, tt := range filterAssertions {
		t.Run(tt.name, func(t *testing.T) {
			filter := tt.filter
			filter.Namespace = "ns-list"
			listed, _, err := s.ListRuntimeSessions(ctx, filter)
			if err != nil {
				t.Fatalf("ListRuntimeSessions: %v", err)
			}
			assertRuntimeSessionIDs(t, listed, tt.want)
		})
	}

	page1, cursor, err := s.ListRuntimeSessions(ctx, harness.RuntimeSessionFilter{Namespace: "ns-list", Limit: 2})
	if err != nil {
		t.Fatalf("ListRuntimeSessions page1: %v", err)
	}
	assertRuntimeSessionIDs(t, page1, []harness.RuntimeSessionID{"runtime-1", "runtime-2"})
	if cursor == "" {
		t.Fatal("cursor is empty, want second page cursor")
	}
	page2, next, err := s.ListRuntimeSessions(ctx, harness.RuntimeSessionFilter{Namespace: "ns-list", Limit: 2, Cursor: cursor})
	if err != nil {
		t.Fatalf("ListRuntimeSessions page2: %v", err)
	}
	assertRuntimeSessionIDs(t, page2, []harness.RuntimeSessionID{"runtime-4"})
	if next != "" {
		t.Fatalf("next cursor = %q, want empty", next)
	}
}

func TestRuntimeSessionStoreTransitionValidatesStateMachine(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	session := runtimeSessionFixture("runtime-transition")
	if err := s.CreateRuntimeSession(ctx, &session); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}

	transitionAt := time.Date(2026, 6, 24, 11, 0, 0, 0, time.UTC)
	updated, err := s.TransitionRuntimeSession(ctx, harness.RuntimeSessionTransition{
		Namespace: runtimeSessionTestNamespace,
		ID:        session.ID,
		From:      harness.RuntimeSessionStatePending,
		To:        harness.RuntimeSessionStateBooting,
		UpdatedAt: transitionAt,
	})
	if err != nil {
		t.Fatalf("TransitionRuntimeSession: %v", err)
	}
	if updated.State != harness.RuntimeSessionStateBooting || !updated.UpdatedAt.Equal(transitionAt) {
		t.Fatalf("updated session = %#v, want Booting at transition time", updated)
	}

	_, err = s.TransitionRuntimeSession(ctx, harness.RuntimeSessionTransition{
		Namespace: runtimeSessionTestNamespace,
		ID:        session.ID,
		From:      harness.RuntimeSessionStateBooting,
		To:        harness.RuntimeSessionStateTurnRunning,
		UpdatedAt: transitionAt.Add(time.Minute),
	})
	if !errors.Is(err, store.ErrValidation) {
		t.Fatalf("invalid TransitionRuntimeSession error = %v, want ErrValidation", err)
	}
	got, err := s.GetRuntimeSession(ctx, runtimeSessionTestNamespace, session.ID)
	if err != nil {
		t.Fatalf("GetRuntimeSession after invalid transition: %v", err)
	}
	if got.State != harness.RuntimeSessionStateBooting || !got.UpdatedAt.Equal(transitionAt) {
		t.Fatalf("session changed after invalid transition: %#v", got)
	}
}

func TestRuntimeSessionStoreTransitionUsesExpectedFromState(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	session := runtimeSessionFixture("runtime-cas")
	if err := s.CreateRuntimeSession(ctx, &session); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	if _, err := s.TransitionRuntimeSession(ctx, harness.RuntimeSessionTransition{
		Namespace: runtimeSessionTestNamespace,
		ID:        session.ID,
		From:      harness.RuntimeSessionStatePending,
		To:        harness.RuntimeSessionStateBooting,
	}); err != nil {
		t.Fatalf("initial TransitionRuntimeSession: %v", err)
	}

	_, err := s.TransitionRuntimeSession(ctx, harness.RuntimeSessionTransition{
		Namespace: runtimeSessionTestNamespace,
		ID:        session.ID,
		From:      harness.RuntimeSessionStatePending,
		To:        harness.RuntimeSessionStateFailed,
	})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale TransitionRuntimeSession error = %v, want ErrConflict", err)
	}
	got, err := s.GetRuntimeSession(ctx, runtimeSessionTestNamespace, session.ID)
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}
	if got.State != harness.RuntimeSessionStateBooting {
		t.Fatalf("state = %q, want Booting after stale transition", got.State)
	}
}

func TestRuntimeSessionStoreTransitionCanSetAndClearActiveTask(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	session := runtimeSessionFixture("runtime-active-task")
	session.State = harness.RuntimeSessionStateReady
	session.Owner.ActiveTask = ""
	if err := s.CreateRuntimeSession(ctx, &session); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}

	activeTask := runtimeSessionTestTask
	updated, err := s.TransitionRuntimeSession(ctx, harness.RuntimeSessionTransition{
		Namespace:  runtimeSessionTestNamespace,
		ID:         session.ID,
		From:       harness.RuntimeSessionStateReady,
		To:         harness.RuntimeSessionStateTurnRunning,
		ActiveTask: &activeTask,
	})
	if err != nil {
		t.Fatalf("set active task transition: %v", err)
	}
	if updated.Owner.ActiveTask != runtimeSessionTestTask {
		t.Fatalf("active task = %q, want runtime task", updated.Owner.ActiveTask)
	}

	updated, err = s.TransitionRuntimeSession(ctx, harness.RuntimeSessionTransition{
		Namespace: runtimeSessionTestNamespace,
		ID:        session.ID,
		From:      harness.RuntimeSessionStateTurnRunning,
		To:        harness.RuntimeSessionStateIdle,
	})
	if err != nil {
		t.Fatalf("preserve active task transition: %v", err)
	}
	if updated.Owner.ActiveTask != runtimeSessionTestTask {
		t.Fatalf("active task = %q, want preserved runtime task", updated.Owner.ActiveTask)
	}

	if _, err := s.TransitionRuntimeSession(ctx, harness.RuntimeSessionTransition{
		Namespace: runtimeSessionTestNamespace,
		ID:        session.ID,
		From:      harness.RuntimeSessionStateIdle,
		To:        harness.RuntimeSessionStateTurnRunning,
	}); err != nil {
		t.Fatalf("back to running transition: %v", err)
	}
	clearActiveTask := ""
	updated, err = s.TransitionRuntimeSession(ctx, harness.RuntimeSessionTransition{
		Namespace:  runtimeSessionTestNamespace,
		ID:         session.ID,
		From:       harness.RuntimeSessionStateTurnRunning,
		To:         harness.RuntimeSessionStateIdle,
		ActiveTask: &clearActiveTask,
	})
	if err != nil {
		t.Fatalf("clear active task transition: %v", err)
	}
	if updated.Owner.ActiveTask != "" {
		t.Fatalf("active task = %q, want cleared", updated.Owner.ActiveTask)
	}
}

func TestRuntimeSessionStoreDeleteRequiresDeletedState(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	session := runtimeSessionFixture("runtime-delete")
	if err := s.CreateRuntimeSession(ctx, &session); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	if err := s.PruneDeletedRuntimeSession(ctx, runtimeSessionTestNamespace, session.ID); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("PruneDeletedRuntimeSession active error = %v, want ErrValidation", err)
	}
	if _, err := s.TransitionRuntimeSession(ctx, harness.RuntimeSessionTransition{Namespace: runtimeSessionTestNamespace, ID: session.ID, From: harness.RuntimeSessionStatePending, To: harness.RuntimeSessionStateDeleting}); err != nil {
		t.Fatalf("transition to deleting: %v", err)
	}
	if _, err := s.TransitionRuntimeSession(ctx, harness.RuntimeSessionTransition{Namespace: runtimeSessionTestNamespace, ID: session.ID, From: harness.RuntimeSessionStateDeleting, To: harness.RuntimeSessionStateDeleted}); err != nil {
		t.Fatalf("transition to deleted: %v", err)
	}
	if err := s.PruneDeletedRuntimeSession(ctx, runtimeSessionTestNamespace, session.ID); err != nil {
		t.Fatalf("PruneDeletedRuntimeSession deleted: %v", err)
	}
	if _, err := s.GetRuntimeSession(ctx, runtimeSessionTestNamespace, session.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRuntimeSession after delete error = %v, want ErrNotFound", err)
	}
}

func TestRuntimeSessionStoreValidationErrors(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	assertValidationError := func(name string, fn func() error) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			if err := fn(); !errors.Is(err, store.ErrValidation) {
				t.Fatalf("error = %v, want ErrValidation", err)
			}
		})
	}

	assertValidationError("nil create", func() error { return s.CreateRuntimeSession(ctx, nil) })
	assertValidationError("empty id", func() error {
		session := runtimeSessionFixture("")
		return s.CreateRuntimeSession(ctx, &session)
	})
	assertValidationError("empty namespace", func() error {
		session := runtimeSessionFixture("runtime-invalid")
		session.Owner.Namespace = ""
		return s.CreateRuntimeSession(ctx, &session)
	})
	assertValidationError("empty session", func() error {
		session := runtimeSessionFixture("runtime-invalid")
		session.Owner.SessionName = ""
		return s.CreateRuntimeSession(ctx, &session)
	})
	assertValidationError("empty provider", func() error {
		session := runtimeSessionFixture("runtime-invalid")
		session.Owner.Provider = ""
		return s.CreateRuntimeSession(ctx, &session)
	})
	assertValidationError("unknown state", func() error {
		session := runtimeSessionFixture("runtime-invalid")
		session.State = "Mystery"
		return s.CreateRuntimeSession(ctx, &session)
	})
	assertValidationError("unknown cleanup policy", func() error {
		session := runtimeSessionFixture("runtime-invalid")
		session.CleanupPolicy = "archive"
		return s.CreateRuntimeSession(ctx, &session)
	})
	assertValidationError("negative idle timeout", func() error {
		session := runtimeSessionFixture("runtime-invalid")
		session.IdleTimeout = -time.Second
		return s.CreateRuntimeSession(ctx, &session)
	})
	assertValidationError("negative max lifetime", func() error {
		session := runtimeSessionFixture("runtime-invalid")
		session.MaxLifetime = -time.Second
		return s.CreateRuntimeSession(ctx, &session)
	})
	assertValidationError("get empty namespace", func() error {
		_, err := s.GetRuntimeSession(ctx, "", "runtime")
		return err
	})
	assertValidationError("get empty id", func() error {
		_, err := s.GetRuntimeSession(ctx, runtimeSessionTestNamespace, "")
		return err
	})
	assertValidationError("list empty namespace", func() error {
		_, _, err := s.ListRuntimeSessions(ctx, harness.RuntimeSessionFilter{})
		return err
	})
	assertValidationError("list invalid state", func() error {
		_, _, err := s.ListRuntimeSessions(ctx, harness.RuntimeSessionFilter{Namespace: runtimeSessionTestNamespace, States: []harness.RuntimeSessionState{"Mystery"}})
		return err
	})
	assertValidationError("list invalid cleanup policy", func() error {
		_, _, err := s.ListRuntimeSessions(ctx, harness.RuntimeSessionFilter{Namespace: runtimeSessionTestNamespace, CleanupPolicies: []harness.RuntimeCleanupPolicy{"archive"}})
		return err
	})
	assertValidationError("list invalid cursor", func() error {
		_, _, err := s.ListRuntimeSessions(ctx, harness.RuntimeSessionFilter{Namespace: runtimeSessionTestNamespace, Cursor: "not-an-offset"})
		return err
	})
	assertValidationError("transition empty namespace", func() error {
		_, err := s.TransitionRuntimeSession(ctx, harness.RuntimeSessionTransition{ID: "runtime", From: harness.RuntimeSessionStatePending, To: harness.RuntimeSessionStateBooting})
		return err
	})
	assertValidationError("transition invalid state", func() error {
		_, err := s.TransitionRuntimeSession(ctx, harness.RuntimeSessionTransition{Namespace: runtimeSessionTestNamespace, ID: "runtime", From: "Mystery", To: harness.RuntimeSessionStateBooting})
		return err
	})
	assertValidationError("delete empty namespace", func() error { return s.PruneDeletedRuntimeSession(ctx, "", "runtime") })

	if _, err := s.GetRuntimeSession(ctx, runtimeSessionTestNamespace, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetRuntimeSession missing error = %v, want ErrNotFound", err)
	}
	if _, err := s.TransitionRuntimeSession(ctx, harness.RuntimeSessionTransition{Namespace: runtimeSessionTestNamespace, ID: "missing", From: harness.RuntimeSessionStatePending, To: harness.RuntimeSessionStateBooting}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("TransitionRuntimeSession missing error = %v, want ErrNotFound", err)
	}
	if err := s.PruneDeletedRuntimeSession(ctx, runtimeSessionTestNamespace, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("PruneDeletedRuntimeSession missing error = %v, want ErrNotFound", err)
	}
}

func runtimeSessionFixture(id harness.RuntimeSessionID) harness.RuntimeSession {
	return harness.RuntimeSession{
		ID: id,
		Owner: harness.RuntimeSessionOwner{
			Namespace:   runtimeSessionTestNamespace,
			SessionName: runtimeSessionTestName,
			ActiveTask:  runtimeSessionTestTask,
			AgentName:   runtimeSessionTestAgent,
			Provider:    harness.ProviderKindKubernetesService,
		},
		State:         harness.RuntimeSessionStatePending,
		CleanupPolicy: harness.RuntimeCleanupPolicyDelete,
	}
}

func runtimeSessionListFixture(
	id harness.RuntimeSessionID,
	namespace string,
	sessionName string,
	activeTask string,
	agentName string,
	provider harness.ProviderKind,
	state harness.RuntimeSessionState,
	cleanupPolicy harness.RuntimeCleanupPolicy,
	updatedAt time.Time,
) harness.RuntimeSession {
	session := runtimeSessionFixture(id)
	session.Owner.Namespace = namespace
	session.Owner.SessionName = sessionName
	session.Owner.ActiveTask = activeTask
	session.Owner.AgentName = agentName
	session.Owner.Provider = provider
	session.State = state
	session.CleanupPolicy = cleanupPolicy
	session.CreatedAt = updatedAt.Add(-time.Hour)
	session.UpdatedAt = updatedAt
	return session
}

func assertRuntimeSessionEqual(t *testing.T, got, want harness.RuntimeSession) {
	t.Helper()
	if got.ID != want.ID || got.Owner != want.Owner || got.State != want.State || got.CleanupPolicy != want.CleanupPolicy {
		t.Fatalf("session identity/state = %#v, want %#v", got, want)
	}
	if got.IdleTimeout != want.IdleTimeout || got.MaxLifetime != want.MaxLifetime {
		t.Fatalf("session durations = %s/%s, want %s/%s", got.IdleTimeout, got.MaxLifetime, want.IdleTimeout, want.MaxLifetime)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) || !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Fatalf("session times = %s/%s, want %s/%s", got.CreatedAt, got.UpdatedAt, want.CreatedAt, want.UpdatedAt)
	}
}

func assertRuntimeSessionIDs(t *testing.T, got []harness.RuntimeSession, want []harness.RuntimeSessionID) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d sessions (%#v), want %d ids (%#v)", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Fatalf("session ids = %#v, want %#v", runtimeSessionIDs(got), want)
		}
	}
}

func runtimeSessionIDs(sessions []harness.RuntimeSession) []harness.RuntimeSessionID {
	ids := make([]harness.RuntimeSessionID, 0, len(sessions))
	for _, session := range sessions {
		ids = append(ids, session.ID)
	}
	return ids
}

func TestRuntimeSessionStoreCreateClaimReuseReleaseAndDelete(t *testing.T) {
	s := setupDiskStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)
	created := &harness.RuntimeSession{
		ID: "runtime-a",
		Owner: harness.RuntimeSessionOwner{
			Namespace: "default", SessionName: "session-a", Provider: harness.ProviderKindKubernetesService,
		},
		State:         harness.RuntimeSessionStateReady,
		CleanupPolicy: harness.RuntimeCleanupPolicyDelete,
		IdleTimeout:   time.Minute,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := s.CreateRuntimeSession(ctx, created); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	claimed, err := s.ClaimRuntimeSession(ctx, "default", "session-a", harness.ProviderKindKubernetesService, "", "runtime-task-a", true, now.Add(time.Second))
	if err != nil {
		t.Fatalf("ClaimRuntimeSession: %v", err)
	}
	if claimed.ID != "runtime-a" || claimed.State != harness.RuntimeSessionStateTurnRunning || claimed.Owner.ActiveTask != "runtime-task-a" {
		t.Fatalf("claimed = %#v, want reused active runtime-a", claimed)
	}
	idle, err := s.MarkRuntimeSessionIdle(ctx, "default", "runtime-a", now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("MarkRuntimeSessionIdle: %v", err)
	}
	if idle.State != harness.RuntimeSessionStateIdle || idle.Owner.ActiveTask != "" {
		t.Fatalf("idle = %#v", idle)
	}
	retained, err := s.ReleaseRuntimeSession(ctx, "default", "runtime-a", harness.RuntimeCleanupPolicyRetain, now.Add(3*time.Second))
	if err != nil {
		t.Fatalf("ReleaseRuntimeSession(retain): %v", err)
	}
	if retained.State != harness.RuntimeSessionStateRetained || retained.CleanupPolicy != harness.RuntimeCleanupPolicyRetain {
		t.Fatalf("retained = %#v", retained)
	}
	deleted, err := s.DeleteRuntimeSession(ctx, "default", "runtime-a", now.Add(4*time.Second))
	if err != nil {
		t.Fatalf("DeleteRuntimeSession: %v", err)
	}
	if deleted.State != harness.RuntimeSessionStateDeleted {
		t.Fatalf("deleted = %#v", deleted)
	}
}

func TestRuntimeSessionStoreClaimCreatesAndDeniesCrossNamespaceReuse(t *testing.T) {
	s := setupDiskStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)
	claimed, err := s.ClaimRuntimeSession(ctx, "default", "session-a", harness.ProviderKindKubernetesService, "", "runtime-task-a", true, now)
	if err != nil {
		t.Fatalf("ClaimRuntimeSession(create): %v", err)
	}
	if claimed.Owner.Namespace != "default" || claimed.Owner.SessionName != "session-a" || claimed.State != harness.RuntimeSessionStateTurnRunning {
		t.Fatalf("claimed = %#v", claimed)
	}
	if _, err := s.GetRuntimeSession(ctx, "other", claimed.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-namespace GetRuntimeSession error = %v, want ErrNotFound", err)
	}
	other, err := s.ClaimRuntimeSession(ctx, "other", "session-a", harness.ProviderKindKubernetesService, "", "task-b", true, now.Add(time.Second))
	if err != nil {
		t.Fatalf("ClaimRuntimeSession(other namespace): %v", err)
	}
	if other.ID == claimed.ID || other.Owner.Namespace != "other" {
		t.Fatalf("other = %#v claimed=%#v, want distinct namespace runtime", other, claimed)
	}
}

func TestRuntimeSessionStoreClaimWithoutReuseDoesNotClaimRetainedRuntime(t *testing.T) {
	s := setupDiskStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)
	first, err := s.ClaimRuntimeSession(ctx, "default", "session-a", harness.ProviderKindKubernetesService, "", "task-a", true, now)
	if err != nil {
		t.Fatalf("ClaimRuntimeSession(first): %v", err)
	}
	if _, err := s.ReleaseRuntimeSession(ctx, "default", first.ID, harness.RuntimeCleanupPolicyRetain, now.Add(time.Second)); err != nil {
		t.Fatalf("ReleaseRuntimeSession(retain): %v", err)
	}
	second, err := s.ClaimRuntimeSession(ctx, "default", "session-a", harness.ProviderKindKubernetesService, "", "task-b", false, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("ClaimRuntimeSession(no reuse): %v", err)
	}
	if second.ID == first.ID {
		t.Fatalf("second ID = %s, want fresh runtime when reuse not allowed", second.ID)
	}
}

func TestRuntimeSessionStoreClaimSeparatesAgentOwners(t *testing.T) {
	s := setupDiskStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)
	first, err := s.ClaimRuntimeSession(ctx, "default", "session-a", harness.ProviderKindKubernetesService, "agent-a", "task-a", true, now)
	if err != nil {
		t.Fatalf("ClaimRuntimeSession(agent-a): %v", err)
	}
	if _, err := s.ReleaseRuntimeSession(ctx, "default", first.ID, harness.RuntimeCleanupPolicyRetain, now.Add(time.Second)); err != nil {
		t.Fatalf("ReleaseRuntimeSession(agent-a): %v", err)
	}
	second, err := s.ClaimRuntimeSession(ctx, "default", "session-a", harness.ProviderKindKubernetesService, "agent-b", "task-b", true, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("ClaimRuntimeSession(agent-b): %v", err)
	}
	if second.ID == first.ID || second.Owner.AgentName != "agent-b" {
		t.Fatalf("second = %#v first=%#v, want distinct runtime for different agent", second, first)
	}
	reused, err := s.ClaimRuntimeSession(ctx, "default", "session-a", harness.ProviderKindKubernetesService, "agent-a", "task-c", true, now.Add(3*time.Second))
	if err != nil {
		t.Fatalf("ClaimRuntimeSession(agent-a reuse): %v", err)
	}
	if reused.ID != first.ID || reused.Owner.AgentName != "agent-a" {
		t.Fatalf("reused = %#v first=%#v, want agent-a runtime reused only by agent-a", reused, first)
	}
}

func TestRuntimeSessionStoreIdleCleanupDeletesExpiredIdleSessions(t *testing.T) {
	s := setupDiskStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)
	for _, session := range []harness.RuntimeSession{
		{
			ID:    "runtime-expired",
			Owner: harness.RuntimeSessionOwner{Namespace: "default", SessionName: "session-a", Provider: harness.ProviderKindKubernetesService},
			State: harness.RuntimeSessionStateIdle, CleanupPolicy: harness.RuntimeCleanupPolicyDelete,
			IdleTimeout: time.Minute, CreatedAt: now.Add(-2 * time.Minute), UpdatedAt: now.Add(-2 * time.Minute),
		},
		{
			ID:    "runtime-fresh",
			Owner: harness.RuntimeSessionOwner{Namespace: "default", SessionName: "session-b", Provider: harness.ProviderKindKubernetesService},
			State: harness.RuntimeSessionStateIdle, CleanupPolicy: harness.RuntimeCleanupPolicyDelete,
			IdleTimeout: time.Hour, CreatedAt: now, UpdatedAt: now,
		},
	} {
		if err := s.CreateRuntimeSession(ctx, &session); err != nil {
			t.Fatalf("CreateRuntimeSession(%s): %v", session.ID, err)
		}
	}
	deleted, err := s.CleanupIdleRuntimeSessions(ctx, now)
	if err != nil {
		t.Fatalf("CleanupIdleRuntimeSessions: %v", err)
	}
	if len(deleted) != 1 || deleted[0].ID != "runtime-expired" || deleted[0].State != harness.RuntimeSessionStateDeleted {
		t.Fatalf("deleted = %#v, want expired deleted", deleted)
	}
	fresh, err := s.GetRuntimeSession(ctx, "default", "runtime-fresh")
	if err != nil {
		t.Fatalf("GetRuntimeSession(fresh): %v", err)
	}
	if fresh.State != harness.RuntimeSessionStateIdle {
		t.Fatalf("fresh = %#v, want idle", fresh)
	}
}

func TestRuntimeSessionStoreIdleCleanupDeletesExpiredRetainedSessions(t *testing.T) {
	s := setupDiskStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)
	session := &harness.RuntimeSession{
		ID:    "runtime-retained-expired",
		Owner: harness.RuntimeSessionOwner{Namespace: "default", SessionName: "session-a", Provider: harness.ProviderKindKubernetesService},
		State: harness.RuntimeSessionStateRetained, CleanupPolicy: harness.RuntimeCleanupPolicyRetain,
		IdleTimeout: time.Minute, CreatedAt: now.Add(-2 * time.Minute), UpdatedAt: now.Add(-2 * time.Minute),
	}
	if err := s.CreateRuntimeSession(ctx, session); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	deleted, err := s.CleanupIdleRuntimeSessions(ctx, now)
	if err != nil {
		t.Fatalf("CleanupIdleRuntimeSessions: %v", err)
	}
	if len(deleted) != 1 || deleted[0].ID != session.ID || deleted[0].State != harness.RuntimeSessionStateDeleted {
		t.Fatalf("deleted = %#v, want retained runtime deleted after idle timeout", deleted)
	}
}

func TestRuntimeSessionStoreClaimSetsDefaultIdleTimeout(t *testing.T) {
	s := setupDiskStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)
	claimed, err := s.ClaimRuntimeSession(ctx, "default", "session-a", harness.ProviderKindKubernetesService, "", "task-a", true, now)
	if err != nil {
		t.Fatalf("ClaimRuntimeSession: %v", err)
	}
	if claimed.IdleTimeout != harness.DefaultRuntimeSessionIdleTimeout {
		t.Fatalf("IdleTimeout = %s, want %s", claimed.IdleTimeout, harness.DefaultRuntimeSessionIdleTimeout)
	}
}

func TestRuntimeSessionStoreIdleCleanupSkipsRuntimeClaimedAfterSelection(t *testing.T) {
	s := setupDiskStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)
	candidate := &harness.RuntimeSession{
		ID:    "runtime-race",
		Owner: harness.RuntimeSessionOwner{Namespace: "default", SessionName: "session-a", Provider: harness.ProviderKindKubernetesService},
		State: harness.RuntimeSessionStateRetained, CleanupPolicy: harness.RuntimeCleanupPolicyRetain,
		IdleTimeout: time.Minute, CreatedAt: now.Add(-2 * time.Minute), UpdatedAt: now.Add(-2 * time.Minute),
	}
	if err := s.CreateRuntimeSession(ctx, candidate); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	claimed, err := s.ClaimRuntimeSession(ctx, "default", "session-a", harness.ProviderKindKubernetesService, "", "task-active", true, now)
	if err != nil {
		t.Fatalf("ClaimRuntimeSession: %v", err)
	}
	deleted, ok, err := s.deleteExpiredInactiveRuntimeSession(ctx, *candidate, now.Add(time.Second))
	if err != nil {
		t.Fatalf("deleteExpiredInactiveRuntimeSession: %v", err)
	}
	if ok || deleted != nil {
		t.Fatalf("deleted=%#v ok=%v, want cleanup skip after claim", deleted, ok)
	}
	current, err := s.GetRuntimeSession(ctx, "default", claimed.ID)
	if err != nil {
		t.Fatalf("GetRuntimeSession: %v", err)
	}
	if current.State != harness.RuntimeSessionStateTurnRunning || current.Owner.ActiveTask != "task-active" {
		t.Fatalf("current=%#v, want active runtime preserved", current)
	}
}

func TestRuntimeSessionStoreInvalidTransitionRejected(t *testing.T) {
	s := setupDiskStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)
	session := &harness.RuntimeSession{
		ID:    "runtime-pending",
		Owner: harness.RuntimeSessionOwner{Namespace: "default", SessionName: "session-a", Provider: harness.ProviderKindKubernetesService},
		State: harness.RuntimeSessionStatePending, CleanupPolicy: harness.RuntimeCleanupPolicyDelete,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateRuntimeSession(ctx, session); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	if _, err := s.MarkRuntimeSessionIdle(ctx, "default", "runtime-pending", now.Add(time.Second)); err == nil {
		t.Fatal("MarkRuntimeSessionIdle() error = nil, want invalid transition")
	}
}

func TestRuntimeSessionStoreClaimReplacesFailedRuntime(t *testing.T) {
	s := setupDiskStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)
	failed := &harness.RuntimeSession{
		ID:    "runtime-failed",
		Owner: harness.RuntimeSessionOwner{Namespace: "default", SessionName: "session-a", Provider: harness.ProviderKindKubernetesService},
		State: harness.RuntimeSessionStateFailed, CleanupPolicy: harness.RuntimeCleanupPolicyDelete,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateRuntimeSession(ctx, failed); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	claimed, err := s.ClaimRuntimeSession(ctx, "default", "session-a", harness.ProviderKindKubernetesService, "", "runtime-task-new", true, now.Add(time.Second))
	if err != nil {
		t.Fatalf("ClaimRuntimeSession: %v", err)
	}
	if claimed.ID == failed.ID || claimed.State != harness.RuntimeSessionStateTurnRunning {
		t.Fatalf("claimed = %#v, want replacement TurnRunning runtime", claimed)
	}
}

func TestRuntimeSessionStoreClaimReplacesUnhealthyRuntime(t *testing.T) {
	s := setupDiskStore(t)
	ctx := context.Background()
	now := time.Date(2026, 6, 13, 10, 0, 0, 0, time.UTC)
	unhealthy := &harness.RuntimeSession{
		ID:    "runtime-unhealthy",
		Owner: harness.RuntimeSessionOwner{Namespace: "default", SessionName: "session-a", Provider: harness.ProviderKindKubernetesService},
		State: harness.RuntimeSessionStateUnhealthy, CleanupPolicy: harness.RuntimeCleanupPolicyDelete,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateRuntimeSession(ctx, unhealthy); err != nil {
		t.Fatalf("CreateRuntimeSession: %v", err)
	}
	claimed, err := s.ClaimRuntimeSession(ctx, "default", "session-a", harness.ProviderKindKubernetesService, "", "runtime-task-new", true, now.Add(time.Second))
	if err != nil {
		t.Fatalf("ClaimRuntimeSession: %v", err)
	}
	if claimed.ID == unhealthy.ID || claimed.State != harness.RuntimeSessionStateTurnRunning {
		t.Fatalf("claimed = %#v, want replacement TurnRunning runtime", claimed)
	}
}
