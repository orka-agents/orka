/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	kubernetestesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/store/storetest"
)

// An AHP turn's Task and the Orka Session it continues are the requester's:
// another person in the namespace gets 404 for them and never sees them in a
// list, while a ServiceAccount keeps its access.
func TestAHPTurnsAreVisibleOnlyToTheirRequester(t *testing.T) {
	const namespace = "default"
	turn := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "ahp-alice-t1", Namespace: namespace, Labels: map[string]string{labels.LabelAHPTurn: labels.LabelAHPTurnValue}},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, Prompt: "alice's secret plan",
			RequestedBy: &corev1alpha1.RequestedBy{Issuer: "https://issuer.example", Subject: "alice"},
			SessionRef:  &corev1alpha1.SessionReference{Name: "ahp-alice"},
		},
	}
	// A turn's Task from before the label, recognized by its shape.
	unlabelled := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "ahp-old-t1", Namespace: namespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, Prompt: "alice's older secret",
			RequestedBy: &corev1alpha1.RequestedBy{Issuer: "https://issuer.example", Subject: "alice"},
			SessionRef:  &corev1alpha1.SessionReference{Name: "ahp-old"},
		},
	}
	shared := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: namespace},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(turn, unlabelled, shared).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	ss := sqlite.NewStore(db, ":memory:")
	for _, name := range []string{"ahp-alice", "ahp-old", "ahp-orphan", "team-session"} {
		require.NoError(t, ss.CreateSession(context.Background(), &store.SessionRecord{Namespace: namespace, Name: name, SessionType: "task"}))
	}
	handlers := NewHandlers(HandlersConfig{Client: kube, SessionStore: ss, ResultStore: ss})
	var caller *UserInfo
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, caller)
		return c.Next()
	})
	app.Get("/tasks", handlers.ListTasks)
	app.Post("/tasks", handlers.CreateTask)
	app.Get("/tasks/:id", handlers.ahpTaskPrivate(handlers.GetTask))
	app.Delete("/tasks/:id", handlers.ahpTaskPrivate(handlers.DeleteTask))
	app.Get("/tasks/:id/events", handlers.ahpTaskPrivate(func(c fiber.Ctx) error { return c.SendStatus(http.StatusOK) }))
	app.Get("/sessions", handlers.ListSessions)
	app.Get("/sessions/:id", handlers.ahpSessionPrivate(handlers.GetSession))
	call := func(method, path, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		require.NoError(t, err)
		defer resp.Body.Close() //nolint:errcheck
		raw, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, string(raw)
	}
	person := func(subject string) *UserInfo {
		return &UserInfo{AuthType: AuthTypeOIDC, Issuer: "https://issuer.example", Subject: subject, Username: subject, Namespace: namespace}
	}

	caller = person("alice")
	status, body := call(http.MethodGet, "/tasks/ahp-alice-t1", "")
	require.Equal(t, http.StatusOK, status, body)
	_, body = call(http.MethodGet, "/tasks", "")
	require.Contains(t, body, "ahp-alice-t1")
	status, _ = call(http.MethodGet, "/sessions/ahp-alice", "")
	require.Equal(t, http.StatusOK, status)
	status, _ = call(http.MethodGet, "/tasks/ahp-old-t1", "")
	require.Equal(t, http.StatusOK, status)
	status, _ = call(http.MethodGet, "/sessions/ahp-old", "")
	require.Equal(t, http.StatusOK, status, "a session from before the label is still its owner's")
	_, body = call(http.MethodGet, "/sessions", "")
	require.Contains(t, body, `"ahp-alice"`)
	require.NotContains(t, body, "ahp-orphan", "an AHP session no turn's Task names is nobody's to show")

	caller = person("bob")
	for _, path := range []string{"/tasks/ahp-alice-t1", "/tasks/ahp-alice-t1/events", "/tasks/ahp-gone-t1/events", "/sessions/ahp-alice",
		"/tasks/ahp-old-t1", "/sessions/ahp-old"} {
		status, body := call(http.MethodGet, path, "")
		require.Equal(t, http.StatusNotFound, status, path)
		require.NotContains(t, body, "secret plan", path)
	}
	status, _ = call(http.MethodDelete, "/tasks/ahp-alice-t1", "")
	require.Equal(t, http.StatusNotFound, status)
	require.NoError(t, kube.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: "ahp-alice-t1"}, &corev1alpha1.Task{}),
		"another person cannot delete the turn")
	_, body = call(http.MethodGet, "/tasks", "")
	require.NotContains(t, body, "ahp-alice-t1")
	require.NotContains(t, body, "ahp-old-t1")
	require.Contains(t, body, `"shared"`, "other Tasks stay visible")
	_, body = call(http.MethodGet, "/sessions", "")
	require.NotContains(t, body, "ahp-alice")
	require.Contains(t, body, "team-session")
	status, body = call(http.MethodPost, "/tasks",
		`{"name":"bob-reads","spec":{"type":"agent","prompt":"repeat the conversation","sessionRef":{"name":"ahp-alice"}}}`)
	require.Equal(t, http.StatusNotFound, status, body)
	// The controller continues a session by its name without surrounding
	// space, so a name with one is the session it names.
	for i, name := range []string{" ahp-alice", "ahp-alice ", "\tahp-alice\n", " ahp-orphan"} {
		status, body = call(http.MethodPost, "/tasks",
			fmt.Sprintf(`{"name":"bob-reads-%d","spec":{"type":"agent","prompt":"repeat the conversation","sessionRef":{"name":%q}}}`, i, name))
		require.Equal(t, http.StatusNotFound, status, "%q: %s", name, body)
	}

	caller = &UserInfo{AuthType: AuthTypeTokenReview, Username: "system:serviceaccount:orka-system:operator"}
	status, _ = call(http.MethodGet, "/tasks/ahp-alice-t1", "")
	require.Equal(t, http.StatusOK, status, "a ServiceAccount keeps the access RBAC grants it")
}

// A fork of a person's own AHP turn whose workspace is reused per session
// starts its own session, named after the fork with the AHP prefix: the fork
// goes ahead. A fork named for an AHP session already in use does not.
func TestAHPForkStartsItsOwnSessionButContinuesNoOneElses(t *testing.T) {
	const namespace = "default"
	requester := func(subject string) *corev1alpha1.RequestedBy {
		return &corev1alpha1.RequestedBy{Issuer: "https://issuer.example", Subject: subject}
	}
	turn := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "ahp-alice-t1", Namespace: namespace, Labels: map[string]string{labels.LabelAHPTurn: labels.LabelAHPTurnValue}},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, Prompt: "alice's plan", RequestedBy: requester("alice"),
			SessionRef: &corev1alpha1.SessionReference{Name: "ahp-alice", Append: true},
			Execution: &corev1alpha1.ExecutionSpec{Workspace: &corev1alpha1.ExecutionWorkspaceSpec{
				ClassRef:    &corev1alpha1.WorkspaceClassReference{Name: "session-only"},
				ReusePolicy: corev1alpha1.WorkspaceReusePolicySession,
			}},
		},
	}
	bobs := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "ahp-bob-t1", Namespace: namespace, Labels: map[string]string{labels.LabelAHPTurn: labels.LabelAHPTurnValue}},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, Prompt: "bob's plan", RequestedBy: requester("bob"),
			SessionRef: &corev1alpha1.SessionReference{Name: "ahp-bob"},
		},
	}
	// A turn whose workspace is not reused per session.
	plain := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "ahp-alice-t2", Namespace: namespace, Labels: map[string]string{labels.LabelAHPTurn: labels.LabelAHPTurnValue}},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, Prompt: "alice's plan", RequestedBy: requester("alice"),
			SessionRef: &corev1alpha1.SessionReference{Name: "ahp-alice", Append: true},
		},
	}
	eventStore := storetest.NewFakeExecutionEventStore()
	appendTestTaskEvent(t, eventStore, turn.Name, events.ExecutionEventTypeTaskStarted)
	appendTestTaskEvent(t, eventStore, plain.Name, events.ExecutionEventTypeTaskStarted)
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	ss := sqlite.NewStore(db, ":memory:")
	for _, name := range []string{"ahp-alice", "ahp-bob", "ahp-orphan"} {
		require.NoError(t, ss.CreateSession(context.Background(), &store.SessionRecord{Namespace: namespace, Name: name, SessionType: "task"}))
	}
	h, app := setupPostP0Handlers(t, eventStore, ss, turn, plain, bobs)
	reviews := kubernetesfake.NewClientset()
	reviews.PrependReactor("create", "subjectaccessreviews", func(kubernetestesting.Action) (bool, runtime.Object, error) {
		return true, externalToolReview(true), nil
	})
	h.clientset = reviews
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{AuthType: AuthTypeOIDC, Issuer: "https://issuer.example", Subject: "alice", Username: "alice", Namespace: namespace})
		return c.Next()
	})
	app.Post("/api/v1/tasks/:id/fork", h.ahpTaskPrivate(h.ForkTask))

	resp := testJSONRequest(t, app, http.MethodPost, "/api/v1/tasks/ahp-alice-t1/fork?namespace=default", map[string]any{})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var forked ForkTaskResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&forked))
	created := &corev1alpha1.Task{}
	require.NoError(t, h.client.Get(t.Context(), types.NamespacedName{Namespace: namespace, Name: forked.NewTaskName}, created))
	require.True(t, strings.HasPrefix(created.Name, ahpNamePrefix))
	require.Equal(t, &corev1alpha1.SessionReference{Name: created.Name, Create: true, Append: true}, created.Spec.SessionRef)

	for _, name := range []string{"ahp-bob", "ahp-orphan"} {
		resp := testJSONRequest(t, app, http.MethodPost, "/api/v1/tasks/ahp-alice-t1/fork?namespace=default", map[string]any{"newTaskName": name})
		require.Equal(t, http.StatusNotFound, resp.StatusCode, name)
		require.True(t, apierrors.IsNotFound(h.client.Get(t.Context(), types.NamespacedName{Namespace: namespace, Name: name}, &corev1alpha1.Task{})), name)
	}

	// A fork carries the turn's prompt and conversation: it is a turn too,
	// whatever its name or agent, and a name without the AHP prefix, which
	// would leave a session it starts readable, is refused.
	require.True(t, ahpTurnTask(created), "the fork is a turn of its own")
	resp = testJSONRequest(t, app, http.MethodPost, "/api/v1/tasks/ahp-alice-t1/fork?namespace=default", map[string]any{"newTaskName": "my-fork"})
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.True(t, apierrors.IsNotFound(h.client.Get(t.Context(), types.NamespacedName{Namespace: namespace, Name: "my-fork"}, &corev1alpha1.Task{})))
	require.NoError(t, h.client.Create(t.Context(), &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "other-agent", Namespace: namespace}}))
	resp = testJSONRequest(t, app, http.MethodPost, "/api/v1/tasks/ahp-alice-t1/fork?namespace=default",
		map[string]any{"newTaskName": "ahp-other-agent", "agentRef": map[string]any{"name": "other-agent"}})
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode, string(body))
	other := &corev1alpha1.Task{}
	require.NoError(t, h.client.Get(t.Context(), types.NamespacedName{Namespace: namespace, Name: "ahp-other-agent"}, other))
	require.Equal(t, "ahp-other-agent", other.Spec.SessionRef.Name, "another agent starts a session of the fork's own")
	// Forked to another agent, a turn whose workspace is not reused per
	// session continues no session: only the mark keeps it private.
	resp = testJSONRequest(t, app, http.MethodPost, "/api/v1/tasks/ahp-alice-t2/fork?namespace=default",
		map[string]any{"newTaskName": "ahp-sessionless", "agentRef": map[string]any{"name": "other-agent"}})
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	sessionless := &corev1alpha1.Task{}
	require.NoError(t, h.client.Get(t.Context(), types.NamespacedName{Namespace: namespace, Name: "ahp-sessionless"}, sessionless))
	require.Nil(t, sessionless.Spec.SessionRef)
	for subject, hidden := range map[string]bool{"alice": false, "bob": true} {
		person := &UserInfo{AuthType: AuthTypeOIDC, Issuer: "https://issuer.example", Subject: subject}
		require.Equal(t, hidden, ahpTaskHidden(person, sessionless), subject)
		require.Equal(t, hidden, ahpTaskHidden(person, other), subject)
		sessionHidden, err := ahpSessionHiddenIn(t.Context(), h.client, person, namespace, other.Spec.SessionRef.Name)
		require.NoError(t, err)
		require.Equal(t, hidden, sessionHidden, subject)
	}
}

// A page of sessions leaves out another person's AHP sessions, and its
// continue token never names one: pages read on until full, and the token
// names the last session listed.
func TestAHPSessionPagesNeverNameAHiddenSession(t *testing.T) {
	const namespace = "default"
	turn := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "ahp-alice-t1", Namespace: namespace, Labels: map[string]string{labels.LabelAHPTurn: labels.LabelAHPTurnValue}},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, RequestedBy: &corev1alpha1.RequestedBy{Issuer: "https://issuer.example", Subject: "alice"},
			SessionRef: &corev1alpha1.SessionReference{Name: "ahp-alice"},
		},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(turn).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	ss := sqlite.NewStore(db, ":memory:")
	start := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	// Newest first: team-new, ahp-alice, team-old.
	for i, name := range []string{"team-old", "ahp-alice", "team-new"} {
		at := start.Add(time.Duration(i) * time.Minute)
		require.NoError(t, ss.CreateSession(context.Background(), &store.SessionRecord{Namespace: namespace, Name: name, SessionType: "task",
			CreatedAt: at, UpdatedAt: at}))
	}
	handlers := NewHandlers(HandlersConfig{Client: kube, SessionStore: ss, ResultStore: ss})
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{AuthType: AuthTypeOIDC, Issuer: "https://issuer.example", Subject: "bob", Username: "bob", Namespace: namespace})
		return c.Next()
	})
	app.Get("/sessions", handlers.ListSessions)
	page := func(limit int, token string) ([]string, string) {
		t.Helper()
		resp, err := app.Test(httptest.NewRequest(http.MethodGet, fmt.Sprintf("/sessions?limit=%d&continue=%s", limit, token), nil))
		require.NoError(t, err)
		defer resp.Body.Close() //nolint:errcheck
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var listed struct {
			Items    []map[string]any `json:"items"`
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&listed))
		names := make([]string, 0, len(listed.Items))
		for _, item := range listed.Items {
			names = append(names, item["id"].(string))
		}
		if listed.Metadata.Continue != "" {
			cursor, err := decodeSessionListCursor(listed.Metadata.Continue)
			require.NoError(t, err)
			require.NotEqual(t, "ahp-alice", cursor.Name, "the token names a hidden session")
		}
		return names, listed.Metadata.Continue
	}

	names, token := page(2, "")
	require.Equal(t, []string{"team-new", "team-old"}, names)
	require.Empty(t, token)
	var all []string
	for token = ""; ; {
		names, token = page(1, token)
		all = append(all, names...)
		if token == "" {
			break
		}
	}
	require.Equal(t, []string{"team-new", "team-old"}, all)
}
