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
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/store/storetest"
	"github.com/orka-agents/orka/internal/usage"
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

// Usage outlives the Tasks of AHP turns. A person sees AHP usage only when it
// names them as its requester, also once a session of its name is someone
// else's: a name is free again once its session and Tasks are gone. Usage
// that names no requester, such as a chat's, is no person's.
func TestAHPUsageStaysTheRequestersAfterTaskCleanup(t *testing.T) {
	const namespace = "default"
	bobsTurn := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "ahp-bob-t1", Namespace: namespace, Labels: map[string]string{labels.LabelAHPTurn: labels.LabelAHPTurnValue}},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, RequestedBy: &corev1alpha1.RequestedBy{Issuer: "https://issuer.example", Subject: "bob"},
			SessionRef: &corev1alpha1.SessionReference{Name: "ahp-live"},
		},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	handlers := NewHandlers(HandlersConfig{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(bobsTurn).Build()})
	requester := func(subject string) *store.UsageRequester {
		return &store.UsageRequester{Issuer: "https://issuer.example", Subject: subject}
	}
	rows := []store.UsageTask{
		{Namespace: namespace, TaskUID: "alice-turn", SessionName: "ahp-alice", Requester: requester("alice")},
		{Namespace: namespace, TaskUID: "alice-legacy", SessionName: "ahp-alice"},
		// Two people used the same name, one after the other.
		{Namespace: namespace, TaskUID: "reused-alice", SessionName: "ahp-reused", Requester: requester("alice")},
		{Namespace: namespace, TaskUID: "reused-bob", SessionName: "ahp-reused", Requester: requester("bob")},
		{Namespace: namespace, TaskUID: "other-issuer", SessionName: "ahp-other", Requester: &store.UsageRequester{Issuer: "https://other.example", Subject: "alice"}},
		// Bob's turn now continues ahp-live; Alice used the name before.
		{Namespace: namespace, TaskUID: "alice-before-bob", SessionName: "ahp-live", Requester: requester("alice")},
		{Namespace: namespace, TaskUID: "legacy-in-bobs", SessionName: "ahp-live"},
		// A name with surrounding space is the session it names.
		{Namespace: namespace, TaskUID: "padded-legacy-in-bobs", SessionName: " ahp-live"},
		// A private fork of a turn with no session of its own.
		{Namespace: namespace, TaskUID: "alice-sessionless-fork", AHPTurn: true, Requester: requester("alice")},
		{Namespace: namespace, TaskUID: "unknown-sessionless-turn", AHPTurn: true},
		// Any Task may be named with the AHP prefix: a name alone is no turn's.
		{Namespace: namespace, TaskUID: "ordinary-ahp-name", TaskName: "ahp-report", Requester: requester("bob")},
		{Namespace: namespace, TaskUID: "ordinary-ahp-name-no-requester", TaskName: "ahp-report-2"},
		{Namespace: namespace, TaskUID: "team", SessionName: "team-session"},
	}
	// Usage with no Task, as a chat's, names no requester.
	observations := []store.UsageObservation{
		{Namespace: namespace, CounterID: "chat-in-bobs", SessionName: "ahp-live"},
		{Namespace: namespace, CounterID: "chat-in-gone", SessionName: "ahp-gone"},
		{Namespace: namespace, CounterID: "chat-in-team", SessionName: "team-session"},
	}
	hidden := func(caller *UserInfo) map[string]bool {
		data := store.UsageData{Tasks: slices.Clone(rows), Observations: slices.Clone(observations)}
		app := fiber.New()
		app.Get("/", func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, caller)
			return handlers.filterUsageTaskAccess(c, &data, time.Now())
		})
		response, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		require.Equal(t, http.StatusOK, response.StatusCode)
		return data.HiddenTasks
	}
	noOnes := map[string]bool{
		"default/alice-legacy": true, "default/other-issuer": true, "default/legacy-in-bobs": true, "default/padded-legacy-in-bobs": true,
		"default/unknown-sessionless-turn": true, "default/call-chat-in-bobs": true, "default/call-chat-in-gone": true,
	}
	with := func(keys ...string) map[string]bool {
		hidden := maps.Clone(noOnes)
		for _, key := range keys {
			hidden["default/"+key] = true
		}
		return hidden
	}
	require.Equal(t, with("reused-bob"), hidden(&UserInfo{AuthType: AuthTypeOIDC, Issuer: "https://issuer.example", Subject: "alice"}))
	require.Equal(t, with("alice-turn", "reused-alice", "alice-before-bob", "alice-sessionless-fork"),
		hidden(&UserInfo{AuthType: AuthTypeOIDC, Issuer: "https://issuer.example", Subject: "bob"}))
	require.Empty(t, hidden(&UserInfo{AuthType: AuthTypeTokenReview, Username: "system:serviceaccount:default:operator"}))
}

// A session's usage counter spans its turns, and the report shows the first
// total it sees as the session's earlier usage. A turn hidden from a person
// must not reach them through a visible turn that shares its counter.
func TestAHPUsageCounterDoesNotShowAHiddenTurn(t *testing.T) {
	const namespace = "default"
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	handlers := NewHandlers(HandlersConfig{Client: fake.NewClientBuilder().WithScheme(scheme).Build()})
	start := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	tokens := func(n int64) *int64 { return &n }
	turn := func(uid string, requester *store.UsageRequester, at time.Time) store.UsageTask {
		return store.UsageTask{Namespace: namespace, NamespaceUID: "namespace-uid", TaskUID: uid, TaskName: uid, SessionName: "ahp-shared",
			Phase: "Succeeded", Runtime: "agent", StartedAt: at, Requester: requester}
	}
	for name, earlier := range map[string]*store.UsageRequester{
		"another person's": {Issuer: "https://issuer.example", Subject: "bob"},
		"no one's":         nil,
	} {
		t.Run(name, func(t *testing.T) {
			data := store.UsageData{
				Tasks: []store.UsageTask{turn("earlier-turn", earlier, start),
					turn("alices-turn", &store.UsageRequester{Issuer: "https://issuer.example", Subject: "alice"}, start.Add(time.Minute))},
				Observations: []store.UsageObservation{
					// The earlier turn reports no counts; Alice's turn then
					// reports the counter's total, the earlier tokens included.
					{ID: "earlier", Namespace: namespace, NamespaceUID: "namespace-uid", TaskUID: "earlier-turn", SessionName: "ahp-shared",
						CounterID: "session", Scope: store.UsageScopeSession, Source: store.UsageSourceAgent, Status: store.UsageStatusCompleted,
						ObservedAt: start.Add(30 * time.Second)},
					{ID: "alice", Namespace: namespace, NamespaceUID: "namespace-uid", TaskUID: "alices-turn", SessionName: "ahp-shared",
						CounterID: "session", Scope: store.UsageScopeSession, Source: store.UsageSourceAgent, Status: store.UsageStatusCompleted,
						ObservedAt: start.Add(90 * time.Second), InputTokens: tokens(4242), OutputTokens: tokens(77)},
				},
			}
			filter := store.UsageFilter{Namespaces: []string{namespace}, NamespaceUIDs: map[string]string{namespace: "namespace-uid"},
				From: start.Add(-time.Hour), Until: start.Add(time.Hour), AsOf: start.Add(time.Hour)}
			app := fiber.New()
			app.Get("/", func(c fiber.Ctx) error {
				c.Locals(UserInfoContextKey, &UserInfo{AuthType: AuthTypeOIDC, Issuer: "https://issuer.example", Subject: "alice"})
				return handlers.filterUsageTaskAccess(c, &data, filter.AsOf)
			})
			response, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			require.Equal(t, http.StatusOK, response.StatusCode)
			report, err := usage.Build(data, filter)
			require.NoError(t, err)
			encoded, err := json.Marshal(report)
			require.NoError(t, err)
			require.Contains(t, string(encoded), "alices-turn")
			require.NotContains(t, string(encoded), "earlier-turn")
			require.NotContains(t, string(encoded), "4242")
		})
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

// Whose AHP usage is follows from the usage alone, so a report looks up no
// session owners and lists no Tasks.
func TestAHPUsageLooksUpNoSessionOwners(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	var lists atomic.Int32
	kube := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			lists.Add(1)
			return c.List(ctx, list, opts...)
		},
	}).Build()
	handlers := NewHandlers(HandlersConfig{Client: kube})
	alice := &store.UsageRequester{Issuer: "https://issuer.example", Subject: "alice"}
	data := store.UsageData{
		Tasks: []store.UsageTask{
			{Namespace: "default", TaskUID: "one", SessionName: "ahp-gone", Requester: alice},
			{Namespace: "default", TaskUID: "two", SessionName: "ahp-other"},
		},
		Observations: []store.UsageObservation{{Namespace: "default", CounterID: "chat", SessionName: "ahp-other"}},
	}
	app := fiber.New()
	app.Get("/", func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{AuthType: AuthTypeOIDC, Issuer: "https://issuer.example", Subject: "bob"})
		return handlers.filterUsageTaskAccess(c, &data, time.Now())
	})
	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, map[string]bool{"default/one": true, "default/two": true, "default/call-chat": true}, data.HiddenTasks)
	require.Zero(t, lists.Load())
}

// A turn of an AHP session from outside the report's period, such as an
// operator's from before it, may have used the session's counter too: the
// report credits nothing on that counter to the person's turns.
func TestAHPUsageCountsHiddenTurnsOutsideTheReport(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	ss := sqlite.NewStore(db, ":memory:")
	start := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	alice := &store.UsageRequester{Issuer: "https://issuer.example", Subject: "alice"}
	for _, task := range []store.UsageTask{
		{Namespace: "default", TaskUID: "operator", TaskName: "operator", SessionName: " ahp-alice\t", Phase: "Succeeded", StartedAt: start.Add(-48 * time.Hour)},
		// Padded with space SQL's TRIM does not know.
		{Namespace: "default", TaskUID: "operator-2", TaskName: "operator-2", SessionName: "\u00a0ahp-alice\u2003", Phase: "Succeeded",
			StartedAt: start.Add(-47 * time.Hour)},
		// Holding the name without being it.
		{Namespace: "default", TaskUID: "other", TaskName: "other", SessionName: "ahp-alice-2", Phase: "Succeeded", StartedAt: start.Add(-46 * time.Hour)},
		{Namespace: "default", TaskUID: "alice", TaskName: "alice", SessionName: "ahp-alice", Phase: "Succeeded", StartedAt: start, Requester: alice},
		{Namespace: "default", TaskUID: "elsewhere", TaskName: "elsewhere", SessionName: "ahp-elsewhere", Phase: "Succeeded", StartedAt: start.Add(-48 * time.Hour)},
	} {
		require.NoError(t, ss.RegisterUsageTask(ctx, task))
	}
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	handlers := NewHandlers(HandlersConfig{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), ExecutionEventStore: ss})
	tokens := int64(250)
	data := store.UsageData{
		Tasks: []store.UsageTask{{Namespace: "default", TaskUID: "alice", TaskName: "alice", SessionName: "ahp-alice", Phase: "Succeeded",
			StartedAt: start, Requester: alice}},
		Observations: []store.UsageObservation{{ID: "alice", Namespace: "default", TaskUID: "alice", SessionName: "ahp-alice", CounterID: "session",
			Scope: store.UsageScopeSession, Source: store.UsageSourceAgent, Status: store.UsageStatusCompleted, InputTokens: &tokens,
			ObservedAt: start.Add(time.Minute)}},
	}
	app := fiber.New()
	app.Get("/", func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{AuthType: AuthTypeOIDC, Issuer: "https://issuer.example", Subject: "alice"})
		return handlers.filterUsageTaskAccess(c, &data, start.Add(time.Hour))
	})
	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, map[string]bool{"default/operator": true, "default/operator-2": true}, data.HiddenTasks,
		"the earlier turns join the report, hidden")
	filter := store.UsageFilter{Namespaces: []string{"default"}, From: start.Add(-time.Hour), Until: start.Add(time.Hour), AsOf: start.Add(time.Hour)}
	report, err := usage.Build(data, filter)
	require.NoError(t, err)
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), `"inputTokens":250`)
	require.NotContains(t, string(encoded), "operator")
}

// A hidden reporter whose Task's row retention pruned, or which reported no
// counts, cannot show how much of its session's counter it used: a person's
// turns are credited nothing on that counter. A ServiceAccount, which sees
// all AHP usage, keeps the counter's gains attributed.
func TestAHPUsageCounterWithRowlessHiddenUsage(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	handlers := NewHandlers(HandlersConfig{Client: fake.NewClientBuilder().WithScheme(scheme).Build()})
	start := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	alice := &store.UsageRequester{Issuer: "https://issuer.example", Subject: "alice"}
	count := func(n int64) *int64 { return &n }
	build := func(caller *UserInfo) string {
		t.Helper()
		turn := func(uid string, at time.Time) store.UsageTask {
			return store.UsageTask{Namespace: "default", TaskUID: uid, TaskName: uid, SessionName: "ahp-alice", Phase: "Succeeded",
				StartedAt: at, Requester: alice}
		}
		report := func(id, uid string, input *int64, at time.Duration) store.UsageObservation {
			return store.UsageObservation{ID: id, Namespace: "default", TaskUID: uid, SessionName: "ahp-alice", CounterID: "session",
				Scope: store.UsageScopeSession, Source: store.UsageSourceAgent, Status: store.UsageStatusCompleted, InputTokens: input,
				ObservedAt: start.Add(at)}
		}
		data := store.UsageData{
			Tasks: []store.UsageTask{turn("first", start), turn("second", start.Add(2*time.Minute))},
			// The pruned turn ran between Alice's, and reported no counts.
			Observations: []store.UsageObservation{report("a", "first", count(100), time.Minute),
				report("b", "pruned", nil, 90*time.Second), report("c", "second", count(250), 3*time.Minute)},
		}
		app := fiber.New()
		app.Get("/", func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, caller)
			return handlers.filterUsageTaskAccess(c, &data, start.Add(time.Hour))
		})
		response, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		require.Equal(t, http.StatusOK, response.StatusCode)
		built, err := usage.Build(data, store.UsageFilter{Namespaces: []string{"default"}, From: start.Add(-time.Hour), Until: start.Add(time.Hour),
			AsOf: start.Add(time.Hour)})
		require.NoError(t, err)
		encoded, err := json.Marshal(built)
		require.NoError(t, err)
		return string(encoded)
	}
	person := build(&UserInfo{AuthType: AuthTypeOIDC, Issuer: "https://issuer.example", Subject: "alice"})
	require.NotContains(t, person, `"inputTokens":150`, "the pruned turn's usage reaches no turn of Alice's")
	require.NotContains(t, person, "pruned")
	operator := build(&UserInfo{AuthType: AuthTypeTokenReview, Username: "system:serviceaccount:default:operator"})
	require.Contains(t, operator, `"inputTokens":150`, "a ServiceAccount's report keeps the gain attributed")
}

// limitRecordingStore records the limit each session lookup is given.
type limitRecordingStore struct {
	*sqlite.Store
	limits []int
}

func (s *limitRecordingStore) UsageTasksInSessions(ctx context.Context, namespace, namespaceUID string, sessions []string, limit int) ([]store.UsageTask, error) {
	s.limits = append(s.limits, limit)
	return s.Store.UsageTasksInSessions(ctx, namespace, namespaceUID, sessions, limit)
}

// The lookup of a report's AHP session rows has one budget across all its
// teams, as the report's own record limit does.
func TestAHPUsageSessionLookupSharesOneBudgetAcrossTeams(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	recording := &limitRecordingStore{Store: sqlite.NewStore(db, ":memory:")}
	start := time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC)
	for _, namespace := range []string{"team-a", "team-b"} {
		for i := range 3 {
			require.NoError(t, recording.RegisterUsageTask(ctx, store.UsageTask{Namespace: namespace, TaskUID: fmt.Sprint("turn-", i),
				TaskName: fmt.Sprint("turn-", i), SessionName: "ahp-shared", Phase: "Succeeded", StartedAt: start}))
		}
	}
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	handlers := NewHandlers(HandlersConfig{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), ExecutionEventStore: recording})
	data := store.UsageData{Tasks: []store.UsageTask{
		{Namespace: "team-a", TaskUID: "mine", SessionName: "ahp-shared", StartedAt: start},
		{Namespace: "team-b", TaskUID: "mine", SessionName: "ahp-shared", StartedAt: start},
	}}
	app := fiber.New()
	app.Get("/", func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{AuthType: AuthTypeOIDC, Issuer: "https://issuer.example", Subject: "alice"})
		return handlers.filterUsageTaskAccess(c, &data, start.Add(time.Hour))
	})
	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/", nil))
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, []int{usageMaxRecords, usageMaxRecords - 3}, recording.limits)
}
