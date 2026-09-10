/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	chattools "github.com/orka-agents/orka/internal/tools"
)

// chatMockProvider implements llm.Provider for testing.
type chatMockProvider struct {
	name      string
	responses []*llm.CompletionResponse
	callCount int
	err       error
	streamCh  chan llm.StreamChunk
	streamErr error
	complete  func(context.Context, *llm.CompletionRequest) (*llm.CompletionResponse, error)
}

func (m *chatMockProvider) Complete(ctx context.Context, req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
	if m.complete != nil {
		return m.complete(ctx, req)
	}
	if m.err != nil {
		return nil, m.err
	}
	idx := m.callCount
	m.callCount++
	if idx < len(m.responses) {
		return m.responses[idx], nil
	}
	return &llm.CompletionResponse{Content: "default response"}, nil
}

func (m *chatMockProvider) Stream(_ context.Context, _ *llm.CompletionRequest) (<-chan llm.StreamChunk, error) {
	if m.streamErr != nil {
		return nil, m.streamErr
	}
	return m.streamCh, nil
}

func (m *chatMockProvider) Name() string {
	if m.name != "" {
		return m.name
	}
	return "mock-provider"
}

func newTestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	return scheme
}

func TestChatHandlerHasRunningTasksExcludesTerminalAndScheduledPhases(t *testing.T) {
	tests := []struct {
		name   string
		phase  corev1alpha1.TaskPhase
		active bool
	}{
		{name: "uninitialized", phase: "", active: true},
		{name: "pending", phase: corev1alpha1.TaskPhasePending, active: true},
		{name: "running", phase: corev1alpha1.TaskPhaseRunning, active: true},
		{name: "finalizing", phase: corev1alpha1.TaskPhaseFinalizing, active: true},
		{name: "scheduled", phase: corev1alpha1.TaskPhaseScheduled, active: false},
		{name: "succeeded", phase: corev1alpha1.TaskPhaseSucceeded, active: false},
		{name: "failed", phase: corev1alpha1.TaskPhaseFailed, active: false},
		{name: "cancelled", phase: corev1alpha1.TaskPhaseCancelled, active: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const sessionID = "chat-session"
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "child-task",
					Namespace: "default",
					Labels:    map[string]string{labels.LabelChatSession: sessionID},
				},
				Status: corev1alpha1.TaskStatus{Phase: tt.phase},
			}
			fakeClient := fake.NewClientBuilder().WithScheme(newTestScheme()).WithRuntimeObjects(task).Build()

			require.Equal(t, tt.active, hasRunningTasks(context.Background(), fakeClient, "default", sessionID))
		})
	}
}

func newTestSessionStore(t *testing.T) store.SessionStore {
	t.Helper()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	return sqlite.NewStore(db, ":memory:")
}

func createTestChatSession(t *testing.T, sessions store.SessionStore, namespace, name string) {
	t.Helper()
	now := time.Now().UTC()
	require.NoError(t, sessions.CreateSession(context.Background(), &store.SessionRecord{
		Namespace: namespace, Name: name, SessionType: "chat", CreatedAt: now, UpdatedAt: now,
	}))
}

func newTestResultStore(t *testing.T) store.ResultStore {
	t.Helper()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	return sqlite.NewStore(db, ":memory:")
}

func newTestChatHandler(t *testing.T, c client.Client, ss store.SessionStore, rs store.ResultStore, cfg ChatConfig) *ChatHandler {
	t.Helper()
	resolver := NewProviderResolver(c, cfg)
	return NewChatHandler(c, nil, nil, cfg, "", false, ss, rs, resolver)
}

// providerCRD creates a Provider CRD + matching Secret for tests.
func providerCRD(name, namespace, providerType, model string) []runtime.Object { //nolint:unparam
	return []runtime.Object{
		&corev1alpha1.Provider{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: corev1alpha1.ProviderSpec{
				Type:         corev1alpha1.ProviderType(providerType),
				DefaultModel: model,
				SecretRef:    corev1alpha1.ProviderSecretRef{Name: name + "-secret", Key: "api-key"},
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-secret", Namespace: namespace},
			Data:       map[string][]byte{"api-key": []byte("test-key")},
		},
	}
}

// --- writeSSE ---

func TestWriteSSE(t *testing.T) {
	tests := []struct {
		name  string
		event string
		data  string
		want  string
	}{
		{
			name:  "simple event",
			event: "message",
			data:  `{"content":"hello"}`,
			want:  "event: message\ndata: {\"content\":\"hello\"}\n\n",
		},
		{
			name:  "empty data",
			event: "done",
			data:  "",
			want:  "event: done\ndata: \n\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := bufio.NewWriter(&buf)
			err := writeSSE(w, tt.event, tt.data)
			require.NoError(t, err)
			assert.Equal(t, tt.want, buf.String())
		})
	}
}

// --- hashArgs ---

func TestHashArgs(t *testing.T) {
	tests := []struct {
		name     string
		toolName string
		args     json.RawMessage
	}{
		{
			name:     "deterministic",
			toolName: "list_tasks",
			args:     json.RawMessage(`{"namespace":"default"}`),
		},
		{
			name:     "empty args",
			toolName: "list_tasks",
			args:     json.RawMessage(`{}`),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h1 := hashArgs(tt.toolName, tt.args)
			h2 := hashArgs(tt.toolName, tt.args)
			assert.Equal(t, h1, h2, "same inputs should produce same hash")
			assert.Len(t, h1, 16, "hash should be 16 hex chars")
		})
	}

	t.Run("different inputs produce different hashes", func(t *testing.T) {
		h1 := hashArgs("tool_a", json.RawMessage(`{}`))
		h2 := hashArgs("tool_b", json.RawMessage(`{}`))
		assert.NotEqual(t, h1, h2)
	})
}

// --- HandleChatConfig ---

func TestHandleChatConfig(t *testing.T) {
	ss := newTestSessionStore(t)
	rs := newTestResultStore(t)
	cfg := DefaultChatConfig()
	cfg.Provider = "test-provider"
	cfg.Model = "test-model"

	fakeClient := fake.NewClientBuilder().WithScheme(newTestScheme()).Build()
	ch := newTestChatHandler(t, fakeClient, ss, rs, cfg)

	app := fiber.New()
	app.Get("/api/v1/chat/config", ch.HandleChatConfig)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/chat/config", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, true, body["enabled"])
	assert.Equal(t, "test-provider", body["provider"])
	assert.Equal(t, "test-model", body["model"])
	assert.Equal(t, float64(20), body["maxIterations"])

	// availableTools should be a non-empty list
	tools, ok := body["availableTools"].([]any)
	require.True(t, ok)
	assert.Greater(t, len(tools), 0)
}

func TestHandleChatConfigRequiresExplicitProviderForContextTokens(t *testing.T) {
	ss := newTestSessionStore(t)
	rs := newTestResultStore(t)
	cfg := DefaultChatConfig()
	cfg.Provider = "test-provider"
	cfg.Model = "test-model"
	fakeClient := fake.NewClientBuilder().WithScheme(newTestScheme()).Build()
	ch := newTestChatHandler(t, fakeClient, ss, rs, cfg)
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	require.NoError(t, err)
	ch.contextTokenAuthorization = authz

	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{AuthType: AuthTypeContextToken, ContextToken: &ContextToken{Scopes: []string{}}})
		return c.Next()
	})
	app.Get("/api/v1/chat/config", ch.HandleChatConfig)

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/v1/chat/config", nil))
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	// The resolver refuses the implicit default for context-token callers, so
	// the config must not advertise one.
	assert.Equal(t, true, body["requireExplicitProvider"])
	assert.Equal(t, "", body["provider"])
	assert.Equal(t, "", body["model"])
}

// --- HandleCancelChat ---

func TestHandleCancelChat(t *testing.T) {
	ss := newTestSessionStore(t)
	rs := newTestResultStore(t)
	cfg := DefaultChatConfig()
	fakeClient := fake.NewClientBuilder().WithScheme(newTestScheme()).Build()
	ch := newTestChatHandler(t, fakeClient, ss, rs, cfg)

	app := fiber.New()
	app.Delete("/api/v1/chat/:sessionId", ch.HandleCancelChat)

	t.Run("missing sessionId returns 400", func(t *testing.T) {
		// Fiber route won't match without the param, so we test empty string via a separate route
		app2 := fiber.New()
		app2.Delete("/api/v1/chat/", ch.HandleCancelChat)
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/chat/", nil)
		resp, err := app2.Test(req)
		require.NoError(t, err)
		// Fiber returns 404 for unmatched routes or 400 for empty param
		assert.Contains(t, []int{http.StatusBadRequest, http.StatusNotFound}, resp.StatusCode)
	})

	t.Run("session not found returns 404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/chat/nonexistent-session", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	})

	t.Run("context token without session write scope is forbidden", func(t *testing.T) {
		ctx := context.Background()
		ssAuthz := newTestSessionStore(t)
		rsAuthz := newTestResultStore(t)
		fakeClient := fake.NewClientBuilder().WithScheme(newTestScheme()).Build()
		chAuthz := newTestChatHandler(t, fakeClient, ssAuthz, rsAuthz, cfg)
		authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{
			Mode: ContextTokenAuthorizationModeEnforce,
		})
		require.NoError(t, err)
		chAuthz.contextTokenAuthorization = authz

		now := time.Now()
		err = ssAuthz.CreateSession(ctx, &store.SessionRecord{
			Namespace:   "default",
			Name:        "protected-session",
			SessionType: "chat",
			CreatedAt:   now,
			UpdatedAt:   now,
		})
		require.NoError(t, err)

		appAuthz := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
		appAuthz.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{
				AuthType: AuthTypeContextToken,
				ContextToken: &ContextToken{
					Scopes: []string{ContextTokenScopeSessionsRead},
				},
			})
			return c.Next()
		})
		appAuthz.Delete("/api/v1/chat/:sessionId", chAuthz.HandleCancelChat)

		req := httptest.NewRequest(http.MethodDelete, "/api/v1/chat/protected-session", nil)
		resp, err := appAuthz.Test(req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)

		_, err = ssAuthz.GetSession(ctx, "default", "protected-session")
		require.NoError(t, err)
	})

	t.Run("existing session is deleted", func(t *testing.T) {
		ctx := context.Background()
		now := time.Now()
		err := ss.CreateSession(ctx, &store.SessionRecord{
			Namespace:   "default",
			Name:        "test-session",
			SessionType: "chat",
			CreatedAt:   now,
			UpdatedAt:   now,
		})
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodDelete, "/api/v1/chat/test-session", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusNoContent, resp.StatusCode)

		// Verify deleted
		_, err = ss.GetSession(ctx, "default", "test-session")
		assert.True(t, errors.Is(err, store.ErrNotFound))
	})

	t.Run("active session is cancelled before deletion", func(t *testing.T) {
		active, err := beginTestActiveChat(ch, context.Background(), "active-session")
		require.NoError(t, err)
		go func() {
			<-active.cancelContext.Done()
			active.finish()
		}()

		req := httptest.NewRequest(http.MethodDelete, "/api/v1/chat/active-session", nil)
		resp, err := app.Test(req, fiber.TestConfig{Timeout: 10 * time.Second})
		require.NoError(t, err)
		assert.Equal(t, http.StatusNoContent, resp.StatusCode)
		_, err = ss.GetSession(context.Background(), "default", "active-session")
		assert.ErrorIs(t, err, store.ErrNotFound)
	})
}

// --- loadChatSession ---

func TestLoadChatSession(t *testing.T) {
	ss := newTestSessionStore(t)
	rs := newTestResultStore(t)
	cfg := DefaultChatConfig()
	fakeClient := fake.NewClientBuilder().WithScheme(newTestScheme()).Build()
	ch := newTestChatHandler(t, fakeClient, ss, rs, cfg)
	ctx := context.Background()

	t.Run("no session returns nil", func(t *testing.T) {
		msgs, err := ch.loadChatSession(ctx, "default", "nonexistent")
		// LoadTranscript may return nil,nil for nonexistent sessions
		if err != nil {
			assert.Nil(t, msgs)
		} else {
			assert.Nil(t, msgs)
		}
	})

	t.Run("empty session returns nil", func(t *testing.T) {
		now := time.Now()
		err := ss.CreateSession(ctx, &store.SessionRecord{
			Namespace:   "default",
			Name:        "empty-session",
			SessionType: "chat",
			CreatedAt:   now,
			UpdatedAt:   now,
		})
		require.NoError(t, err)

		msgs, err := ch.loadChatSession(ctx, "default", "empty-session")
		assert.NoError(t, err)
		assert.Nil(t, msgs)
	})

	t.Run("session with messages returns llm.Messages", func(t *testing.T) {
		now := time.Now()
		err := ss.CreateSession(ctx, &store.SessionRecord{
			Namespace:   "default",
			Name:        "has-messages",
			SessionType: "chat",
			CreatedAt:   now,
			UpdatedAt:   now,
		})
		require.NoError(t, err)

		err = ss.AppendMessages(ctx, "default", "has-messages", []store.SessionMessage{
			{Role: "user", Content: "hello", Timestamp: now},
			{Role: "assistant", Content: "hi there", Timestamp: now},
		})
		require.NoError(t, err)

		msgs, err := ch.loadChatSession(ctx, "default", "has-messages")
		require.NoError(t, err)
		require.Len(t, msgs, 2)
		assert.Equal(t, "user", msgs[0].Role)
		assert.Equal(t, "hello", msgs[0].Content)
		assert.Equal(t, "assistant", msgs[1].Role)
		assert.Equal(t, "hi there", msgs[1].Content)
	})
}

// --- saveChatSession ---

func TestSaveChatSession(t *testing.T) {
	ss := newTestSessionStore(t)
	rs := newTestResultStore(t)
	cfg := DefaultChatConfig()
	fakeClient := fake.NewClientBuilder().WithScheme(newTestScheme()).Build()
	ch := newTestChatHandler(t, fakeClient, ss, rs, cfg)
	ctx := context.Background()

	t.Run("rejects a missing locked session instead of recreating it", func(t *testing.T) {
		messages := []llm.Message{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi"},
		}
		buffer, err := newChatMessageBuffer(messages, 0)
		require.NoError(t, err)
		err = ch.saveChatSession(ctx, "default", "new-session", buffer.pending)
		require.Error(t, err)
		assert.ErrorIs(t, err, store.ErrNotFound)
		_, getErr := ss.GetSession(ctx, "default", "new-session")
		assert.ErrorIs(t, getErr, store.ErrNotFound)
	})

	t.Run("only appends new messages (skips persisted)", func(t *testing.T) {
		// Create a session with 2 messages already persisted
		now := time.Now()
		err := ss.CreateSession(ctx, &store.SessionRecord{
			Namespace:   "default",
			Name:        "partial-session",
			SessionType: "chat",
			CreatedAt:   now,
			UpdatedAt:   now,
		})
		require.NoError(t, err)

		err = ss.AppendMessages(ctx, "default", "partial-session", []store.SessionMessage{
			{Role: "user", Content: "old message", Timestamp: now},
		})
		require.NoError(t, err)

		messages := []llm.Message{
			{Role: "user", Content: "old message"},
			{Role: "assistant", Content: "new response"},
		}
		buffer, err := newChatMessageBuffer(messages, 1)
		require.NoError(t, err)
		err = ch.saveChatSession(ctx, "default", "partial-session", buffer.pending)
		require.NoError(t, err)
		// Retrying the same entries retains their IDs and cannot duplicate them.
		require.NoError(t, ch.saveChatSession(ctx, "default", "partial-session", buffer.pending))

		stored, err := ss.LoadTranscript(ctx, "default", "partial-session", 0)
		require.NoError(t, err)
		assert.Len(t, stored, 2) // 1 old + 1 new
	})

	t.Run("no new messages is a no-op", func(t *testing.T) {
		now := time.Now()
		require.NoError(t, ss.CreateSession(ctx, &store.SessionRecord{
			Namespace: "default", Name: "noop-session", SessionType: "chat", CreatedAt: now, UpdatedAt: now,
		}))
		messages := []llm.Message{
			{Role: "user", Content: "already saved"},
		}
		buffer, err := newChatMessageBuffer(messages, 1)
		require.NoError(t, err)
		err = ch.saveChatSession(ctx, "default", "noop-session", buffer.pending)
		require.NoError(t, err)
	})
}

func TestAcquireChatSessionFencesDeletionUntilRelease(t *testing.T) {
	ss := newTestSessionStore(t)
	ch := &ChatHandler{sessionStore: ss}
	ctx := context.Background()
	release, created, lockID, err := ch.acquireChatSession(ctx, "default", "locked-chat")
	require.NoError(t, err)
	if !created {
		t.Fatal("acquireChatSession() did not create the missing chat Session")
	}
	if lockID == "" {
		t.Fatal("acquireChatSession() returned an empty lock identity")
	}
	if err := ss.DeleteSession(ctx, "default", "locked-chat"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("DeleteSession(locked chat) error = %v, want ErrConflict", err)
	}
	release()
	release() // idempotent close for overlapping handler/stream cleanup paths.
	require.NoError(t, ss.DeleteSession(ctx, "default", "locked-chat"))
	if _, err := ss.GetSession(ctx, "default", "locked-chat"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetSession() error = %v, want ErrNotFound", err)
	}
}

func TestBeginActiveChatRejectsConcurrentRequestAndRetainsEmptySession(t *testing.T) {
	ss := newTestSessionStore(t)
	ch := &ChatHandler{sessionStore: ss, config: DefaultChatConfig(), activeChats: make(map[string]*activeChatRequest)}
	ctx := context.Background()
	active, err := beginTestActiveChat(ch, ctx, "concurrent-chat")
	require.NoError(t, err)
	if _, err := beginTestActiveChat(ch, ctx, "concurrent-chat"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("beginActiveChat(concurrent) error = %v, want ErrConflict", err)
	}
	active.finish()
	if _, err := ss.GetSession(ctx, "default", "concurrent-chat"); err != nil {
		t.Fatalf("GetSession(empty failed chat) error = %v, want retained Session", err)
	}
	require.NoError(t, ss.DeleteSession(ctx, "default", "concurrent-chat"))
}

func TestChatCancellationGateBlocksReplacementUntilDeletionCompletes(t *testing.T) {
	ss := newTestSessionStore(t)
	ch := &ChatHandler{sessionStore: ss, config: DefaultChatConfig(), activeChats: make(map[string]*activeChatRequest)}
	ctx := context.Background()
	active, err := beginTestActiveChat(ch, ctx, "cancel-gated-chat")
	require.NoError(t, err)
	request, found := ch.cancelActiveChat("default", "cancel-gated-chat")
	if !found {
		t.Fatal("cancelActiveChat() did not find active request")
	}
	active.finish()
	<-request.done
	if _, err := beginTestActiveChat(ch, ctx, "cancel-gated-chat"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("beginActiveChat(during deletion handoff) error = %v, want ErrConflict", err)
	}
	ch.clearChatDeletionGate("default", "cancel-gated-chat")
	replacement, err := beginTestActiveChat(ch, ctx, "cancel-gated-chat")
	require.NoError(t, err)
	replacement.finish()
}

// --- lookupProvider ---

func TestLookupProvider(t *testing.T) {
	scheme := newTestScheme()

	t.Run("found", func(t *testing.T) {
		objs := providerCRD("test-provider", "default", "openai", "gpt-4")
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
		ss := newTestSessionStore(t)
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		p, err := ch.resolver.LookupProvider(context.Background(), "test-provider", "default")
		require.NoError(t, err)
		assert.Equal(t, "test-provider", p.Name)
		assert.Equal(t, corev1alpha1.ProviderType("openai"), p.Spec.Type)
	})

	t.Run("not found", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		ss := newTestSessionStore(t)
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		_, err := ch.resolver.LookupProvider(context.Background(), "missing", "default")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})
}

// --- resolveAPIKey ---

func TestResolveAPIKey(t *testing.T) {
	scheme := newTestScheme()

	t.Run("resolves key from secret", func(t *testing.T) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "my-secret", Namespace: "default"},
			Data:       map[string][]byte{"api-key": []byte("sk-test-123")},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(secret).Build()
		ss := newTestSessionStore(t)
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		providerObj := &corev1alpha1.Provider{
			ObjectMeta: metav1.ObjectMeta{Name: "p1", Namespace: "default"},
			Spec: corev1alpha1.ProviderSpec{
				SecretRef: corev1alpha1.ProviderSecretRef{Name: "my-secret", Key: "api-key"},
			},
		}

		key, err := ch.resolver.ResolveAPIKey(context.Background(), providerObj)
		require.NoError(t, err)
		assert.Equal(t, "sk-test-123", key)
	})

	t.Run("default key name is api-key", func(t *testing.T) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "default-key-secret", Namespace: "default"},
			Data:       map[string][]byte{"api-key": []byte("default-key-val")},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(secret).Build()
		ss := newTestSessionStore(t)
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		providerObj := &corev1alpha1.Provider{
			ObjectMeta: metav1.ObjectMeta{Name: "p2", Namespace: "default"},
			Spec: corev1alpha1.ProviderSpec{
				SecretRef: corev1alpha1.ProviderSecretRef{Name: "default-key-secret"},
			},
		}

		key, err := ch.resolver.ResolveAPIKey(context.Background(), providerObj)
		require.NoError(t, err)
		assert.Equal(t, "default-key-val", key)
	})

	t.Run("missing secret returns error", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		ss := newTestSessionStore(t)
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		providerObj := &corev1alpha1.Provider{
			ObjectMeta: metav1.ObjectMeta{Name: "p3", Namespace: "default"},
			Spec: corev1alpha1.ProviderSpec{
				SecretRef: corev1alpha1.ProviderSecretRef{Name: "nonexistent"},
			},
		}

		_, err := ch.resolver.ResolveAPIKey(context.Background(), providerObj)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to get provider secret")
	})

	t.Run("missing key in secret returns error", func(t *testing.T) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "bad-key-secret", Namespace: "default"},
			Data:       map[string][]byte{"wrong-key": []byte("value")},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(secret).Build()
		ss := newTestSessionStore(t)
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		providerObj := &corev1alpha1.Provider{
			ObjectMeta: metav1.ObjectMeta{Name: "p4", Namespace: "default"},
			Spec: corev1alpha1.ProviderSpec{
				SecretRef: corev1alpha1.ProviderSecretRef{Name: "bad-key-secret", Key: "api-key"},
			},
		}

		_, err := ch.resolver.ResolveAPIKey(context.Background(), providerObj)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "has no key")
	})
}

// --- resolveProvider ---

func TestResolveProvider(t *testing.T) {
	scheme := newTestScheme()

	// Register a test provider factory so llm.NewProvider("test-type", ...) works
	llm.RegisterProvider("test-type", func(cfg llm.ProviderConfig) (llm.Provider, error) {
		return &chatMockProvider{name: "test-type"}, nil
	})

	t.Run("resolves from request provider", func(t *testing.T) {
		objs := providerCRD("my-provider", "default", "test-type", "test-model")
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
		ss := newTestSessionStore(t)
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		provider, model, err := ch.resolver.Resolve(context.Background(), ResolveOpts{
			ProviderName: "my-provider",
			Namespace:    "default",
		})
		require.NoError(t, err)
		assert.NotNil(t, provider)
		assert.Equal(t, "test-model", model)
	})

	t.Run("resolves from config provider", func(t *testing.T) {
		objs := providerCRD("config-provider", "default", "test-type", "config-model")
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
		ss := newTestSessionStore(t)
		rs := newTestResultStore(t)
		cfg := DefaultChatConfig()
		cfg.Provider = "config-provider"
		ch := newTestChatHandler(t, fakeClient, ss, rs, cfg)

		provider, model, err := ch.resolver.Resolve(context.Background(), ResolveOpts{
			Namespace: "default",
		})
		require.NoError(t, err)
		assert.NotNil(t, provider)
		assert.Equal(t, "config-model", model)
	})

	t.Run("request model overrides provider default", func(t *testing.T) {
		objs := providerCRD("override-provider", "default", "test-type", "default-model")
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
		ss := newTestSessionStore(t)
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		_, model, err := ch.resolver.Resolve(context.Background(), ResolveOpts{
			ProviderName: "override-provider",
			Model:        "custom-model",
			Namespace:    "default",
		})
		require.NoError(t, err)
		assert.Equal(t, "custom-model", model)
	})

	t.Run("falls back to default provider", func(t *testing.T) {
		objs := providerCRD("default", "default", "test-type", "default-model")
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
		ss := newTestSessionStore(t)
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		provider, model, err := ch.resolver.Resolve(context.Background(), ResolveOpts{
			Namespace: "default",
		})
		require.NoError(t, err)
		assert.NotNil(t, provider)
		assert.Equal(t, "default-model", model)
	})

	t.Run("no provider found returns error", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		ss := newTestSessionStore(t)
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		_, _, err := ch.resolver.Resolve(context.Background(), ResolveOpts{
			Namespace: "default",
		})
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "no provider")
	})

	t.Run("resolves from agent ref", func(t *testing.T) {
		provObjs := providerCRD("agent-provider", "default", "test-type", "agent-model")
		agent := &corev1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "my-agent", Namespace: "default"},
			Spec: corev1alpha1.AgentSpec{
				ProviderRef: &corev1alpha1.ProviderReference{Name: "agent-provider"},
				Model:       &corev1alpha1.ModelConfig{Name: "agent-specific-model"},
			},
		}
		allObjs := append(provObjs, agent)
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(allObjs...).Build()
		ss := newTestSessionStore(t)
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		provider, model, err := ch.resolver.Resolve(context.Background(), ResolveOpts{
			AgentRef:  "my-agent",
			Namespace: "default",
		})
		require.NoError(t, err)
		assert.NotNil(t, provider)
		assert.Equal(t, "agent-specific-model", model)
	})
}

// --- wrapWithRetryAndFallback ---

func TestWrapWithRetryAndFallback(t *testing.T) {
	scheme := newTestScheme()

	// Ensure test-type provider is registered
	llm.RegisterProvider("test-type", func(cfg llm.ProviderConfig) (llm.Provider, error) {
		return &chatMockProvider{name: "test-type"}, nil
	})

	t.Run("no agent ref returns retry provider", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		ss := newTestSessionStore(t)
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		primary := &chatMockProvider{name: "primary"}
		result, err := ch.wrapWithRetryAndFallback(context.Background(), nil, primary, ChatRequest{}, "default")
		require.NoError(t, err)
		assert.NotNil(t, result)
	})

	t.Run("agent with no fallbacks returns retry provider", func(t *testing.T) {
		agent := &corev1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "no-fallback-agent", Namespace: "default"},
			Spec:       corev1alpha1.AgentSpec{},
		}
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(agent).Build()
		ss := newTestSessionStore(t)
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		primary := &chatMockProvider{name: "primary"}
		result, err := ch.wrapWithRetryAndFallback(context.Background(), nil, primary, ChatRequest{AgentRef: "no-fallback-agent"}, "default")
		require.NoError(t, err)
		assert.NotNil(t, result)
	})

	t.Run("agent with fallbacks wraps with fallback provider", func(t *testing.T) {
		provObjs := providerCRD("fallback-provider", "default", "test-type", "fb-model")
		agent := &corev1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "fb-agent", Namespace: "default"},
			Spec: corev1alpha1.AgentSpec{
				Model: &corev1alpha1.ModelConfig{
					Fallbacks: []corev1alpha1.ModelFallback{
						{ProviderRef: "fallback-provider", Model: "fb-model"},
					},
				},
			},
		}
		allObjs := append(provObjs, agent)
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(allObjs...).Build()
		ss := newTestSessionStore(t)
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		primary := &chatMockProvider{name: "primary"}
		result, err := ch.wrapWithRetryAndFallback(context.Background(), nil, primary, ChatRequest{AgentRef: "fb-agent"}, "default")
		require.NoError(t, err)
		assert.NotNil(t, result)
		// The result should be a FallbackProvider wrapping the primary
		assert.NotEqual(t, "primary", result.Name())
	})

	t.Run("context token denies unauthorized fallback provider", func(t *testing.T) {
		provObjs := providerCRD("fallback-provider", "default", "test-type", "fb-model")
		agent := &corev1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "fb-agent-denied", Namespace: "default"},
			Spec: corev1alpha1.AgentSpec{
				Model: &corev1alpha1.ModelConfig{
					Fallbacks: []corev1alpha1.ModelFallback{
						{ProviderRef: "fallback-provider", Model: "fb-model"},
					},
				},
			},
		}
		allObjs := append(provObjs, agent)
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(allObjs...).Build()
		ss := newTestSessionStore(t)
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
		require.NoError(t, err)
		ch.contextTokenAuthorization = authz

		app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
		app.Get("/test", func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{
				AuthType: AuthTypeContextToken,
				ContextToken: &ContextToken{
					Scopes: []string{ContextTokenScopeProvidersUse},
					TransactionContext: map[string]any{
						"allowedProviders": []string{"primary-provider"},
					},
				},
			})

			primary := &chatMockProvider{name: "primary"}
			_, err := ch.wrapWithRetryAndFallback(
				context.Background(),
				c,
				primary,
				ChatRequest{AgentRef: "fb-agent-denied"},
				"default",
			)
			if err != nil {
				return err
			}
			return c.SendStatus(http.StatusOK)
		})

		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
}

// --- runToolLoop ---

func TestRunToolLoop(t *testing.T) {
	scheme := newTestScheme()

	t.Run("returns content on final text response", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		ss := newTestSessionStore(t)
		createTestChatSession(t, ss, "default", "test-sess")
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		provider := &chatMockProvider{
			responses: []*llm.CompletionResponse{
				{Content: "Hello, I'm here to help!", InputTokens: 10, OutputTokens: 20},
			},
		}

		messages := []llm.Message{{Role: "user", Content: "hello"}}
		content, usage, toolCalls, err := ch.runToolLoop(
			context.Background(), provider, messages, "system prompt",
			nil, NewToolExecutor(fakeClient, nil, "default", "test-sess", "", false, 5, 60*time.Second, rs),
			"test-sess", "default", "test-model", 0.7, 4096, 0, nil,
		)
		require.NoError(t, err)
		assert.Equal(t, "Hello, I'm here to help!", content)
		assert.Equal(t, 1, usage.LLMCalls)
		assert.Equal(t, 10, usage.InputTokens)
		assert.Equal(t, 20, usage.OutputTokens)
		assert.Empty(t, toolCalls)
	})

	t.Run("handles tool calls then final response", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		ss := newTestSessionStore(t)
		createTestChatSession(t, ss, "default", "test-sess2")
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		provider := &chatMockProvider{
			responses: []*llm.CompletionResponse{
				{
					Content: "",
					ToolCalls: []llm.ToolCall{
						{
							ID:        "call-1",
							Name:      "list_tasks",
							Arguments: json.RawMessage(`{"namespace":"default"}`),
						},
					},
					InputTokens:  5,
					OutputTokens: 10,
				},
				{Content: "Done!", InputTokens: 15, OutputTokens: 25},
			},
		}

		messages := []llm.Message{{Role: "user", Content: "list tasks"}}
		exec := NewToolExecutor(fakeClient, nil, "default", "test-sess2", "", false, 5, 60*time.Second, rs)
		content, usage, toolCalls, err := ch.runToolLoop(
			context.Background(), provider, messages, "system prompt",
			exec.registry.ToLLMTools(chattools.ChatToolNames()), exec,
			"test-sess2", "default", "test-model", 0.7, 4096, 0, nil,
		)
		require.NoError(t, err)
		assert.Equal(t, "Done!", content)
		assert.Equal(t, 2, usage.LLMCalls)
		assert.Len(t, toolCalls, 1)
		assert.Equal(t, "list_tasks", toolCalls[0].Name)
	})

	t.Run("respects context cancellation", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		ss := newTestSessionStore(t)
		createTestChatSession(t, ss, "default", "test-sess3")
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		provider := &chatMockProvider{
			responses: []*llm.CompletionResponse{
				{Content: "should not reach here"},
			},
		}

		messages := []llm.Message{{Role: "user", Content: "hello"}}
		content, _, _, err := ch.runToolLoop(
			ctx, provider, messages, "system prompt",
			nil, NewToolExecutor(fakeClient, nil, "default", "test-sess3", "", false, 5, 60*time.Second, rs),
			"test-sess3", "default", "test-model", 0.7, 4096, 0, nil,
		)
		require.NoError(t, err)
		assert.Contains(t, content, "ran out of time")
	})

	t.Run("respects max iterations", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		ss := newTestSessionStore(t)
		createTestChatSession(t, ss, "default", "max-iter-sess")
		rs := newTestResultStore(t)
		cfg := DefaultChatConfig()
		cfg.MaxIterations = 1 // Very low limit
		ch := newTestChatHandler(t, fakeClient, ss, rs, cfg)

		// First call returns tool call (uses iteration 0), then iteration 1 hits max
		provider := &chatMockProvider{
			responses: []*llm.CompletionResponse{
				{
					ToolCalls: []llm.ToolCall{
						{ID: "c1", Name: "list_tasks", Arguments: json.RawMessage(`{}`)},
					},
				},
				{Content: "Summary after max iterations", InputTokens: 5, OutputTokens: 5},
			},
		}

		messages := []llm.Message{{Role: "user", Content: "do things"}}
		exec2 := NewToolExecutor(fakeClient, nil, "default", "max-iter-sess", "", false, 5, 60*time.Second, rs)
		content, usage, _, err := ch.runToolLoop(
			context.Background(), provider, messages, "system prompt",
			exec2.registry.ToLLMTools(chattools.ChatToolNames()), exec2,
			"max-iter-sess", "default", "test-model", 0.7, 4096, 0, nil,
		)
		require.NoError(t, err)
		assert.NotEmpty(t, content)
		assert.GreaterOrEqual(t, usage.LLMCalls, 1)
	})

	t.Run("LLM error returns error", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		ss := newTestSessionStore(t)
		createTestChatSession(t, ss, "default", "err-sess")
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		provider := &chatMockProvider{
			err: fmt.Errorf("LLM is down"),
		}

		messages := []llm.Message{{Role: "user", Content: "hello"}}
		_, _, _, err := ch.runToolLoop(
			context.Background(), provider, messages, "system prompt",
			nil, NewToolExecutor(fakeClient, nil, "default", "err-sess", "", false, 5, 60*time.Second, rs),
			"err-sess", "default", "test-model", 0.7, 4096, 0, nil,
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "LLM completion failed")
	})

	t.Run("emits SSE events for tool calls and final message", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		ss := newTestSessionStore(t)
		createTestChatSession(t, ss, "default", "sse-sess")
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		provider := &chatMockProvider{
			responses: []*llm.CompletionResponse{
				{
					ToolCalls: []llm.ToolCall{
						{ID: "tc1", Name: "list_tasks", Arguments: json.RawMessage(`{}`)},
					},
				},
				{Content: "All done!", InputTokens: 10, OutputTokens: 20},
			},
		}

		var sseEvents []string
		emitSSE := func(event, data string) {
			sseEvents = append(sseEvents, fmt.Sprintf("%s:%s", event, data))
		}

		messages := []llm.Message{{Role: "user", Content: "do it"}}
		exec3 := NewToolExecutor(fakeClient, nil, "default", "sse-sess", "", false, 5, 60*time.Second, rs)
		content, _, _, err := ch.runToolLoop(
			context.Background(), provider, messages, "system prompt",
			exec3.registry.ToLLMTools(chattools.ChatToolNames()), exec3,
			"sse-sess", "default", "test-model", 0.7, 4096, 0, emitSSE,
		)
		require.NoError(t, err)
		assert.Equal(t, "All done!", content)

		// Should have tool_call, tool_result, and message events
		hasToolCall := false
		hasToolResult := false
		hasMessage := false
		for _, e := range sseEvents {
			if strings.HasPrefix(e, "tool_call:") {
				hasToolCall = true
			}
			if strings.HasPrefix(e, "tool_result:") {
				hasToolResult = true
			}
			if strings.HasPrefix(e, "message:") {
				hasMessage = true
			}
		}
		assert.True(t, hasToolCall, "should have tool_call SSE event")
		assert.True(t, hasToolResult, "should have tool_result SSE event")
		assert.True(t, hasMessage, "should have message SSE event")
	})
}

func TestRunToolLoopPersistsEveryMessageAfterRepeatedReduction(t *testing.T) {
	for _, providerLimit := range []bool{false, true} {
		t.Run(fmt.Sprintf("provider-limit=%t", providerLimit), func(t *testing.T) {
			ctx := context.Background()
			ss := newTestSessionStore(t)
			createTestChatSession(t, ss, "default", "reduce-session")
			history := make([]store.SessionMessage, 0, 21)
			history = append(history, store.SessionMessage{Role: "system", Content: "Preserve the public API."})
			for i := range 20 {
				role := "user"
				if i%2 != 0 {
					role = "assistant"
				}
				history = append(history, store.SessionMessage{Role: role, Content: strings.Repeat(fmt.Sprintf("history-%d ", i), 100)})
			}
			require.NoError(t, ss.AppendMessages(ctx, "default", "reduce-session", history))
			fakeClient := fake.NewClientBuilder().WithScheme(newTestScheme()).Build()
			rs := newTestResultStore(t)
			cfg := DefaultChatConfig()
			cfg.MaxSessionSize = 200
			if providerLimit {
				cfg.MaxSessionSize = 0
			}
			ch := newTestChatHandler(t, fakeClient, ss, rs, cfg)
			messages, err := ch.loadChatSession(ctx, "default", "reduce-session")
			require.NoError(t, err)
			historyCount := len(messages)
			const request = "  Investigate the failure.\nKeep the API and this exact request unchanged.  "
			messages = append(messages, llm.Message{Role: "user", Content: request})
			calls, completed := 0, 0
			provider := &chatMockProvider{complete: func(_ context.Context, req *llm.CompletionRequest) (*llm.CompletionResponse, error) {
				calls++
				foundRequest, foundInstructions := false, false
				for _, message := range req.Messages {
					foundRequest = foundRequest || message.Role == "user" && message.Content == request
					foundInstructions = foundInstructions || message.Role == "system" && message.Content == history[0].Content
				}
				require.True(t, foundRequest, "request lost on model call %d", calls)
				require.True(t, foundInstructions, "system instructions lost on model call %d", calls)
				if providerLimit && calls%2 != 0 {
					return nil, &llm.ProviderError{StatusCode: 400, Message: "context too long"}
				}
				stored, err := ss.LoadTranscript(ctx, "default", "reduce-session", 0)
				require.NoError(t, err)
				require.Len(t, stored, historyCount+1+completed*2, "each preceding call and result must be committed")
				completed++
				if completed <= 2 {
					return &llm.CompletionResponse{
						Content: strings.Repeat(fmt.Sprintf("finding-%d ", completed), 1000),
						ToolCalls: []llm.ToolCall{{
							ID: fmt.Sprintf("call-%d", completed), Name: "list_tasks", Arguments: json.RawMessage(`{}`),
						}},
					}, nil
				}
				return &llm.CompletionResponse{Content: "finished"}, nil
			}}
			executor := NewToolExecutor(fakeClient, nil, "default", "reduce-session", "", false, 5, time.Minute, rs)
			content, _, _, err := ch.runToolLoop(ctx, provider, messages, "normal system prompt", nil, executor,
				"reduce-session", "default", "test-model", 0, 100, historyCount, nil)
			require.NoError(t, err)
			require.Equal(t, "finished", content)
			require.Equal(t, 3, completed)
			stored, err := ch.loadChatSession(ctx, "default", "reduce-session")
			require.NoError(t, err)
			require.Len(t, stored, historyCount+6)
			newMessages := stored[historyCount:]
			require.Equal(t, request, newMessages[0].Content)
			for i := range 2 {
				require.Equal(t, strings.Repeat(fmt.Sprintf("finding-%d ", i+1), 1000), newMessages[1+i*2].Content)
				require.Len(t, newMessages[1+i*2].ToolCalls, 1)
				require.Equal(t, newMessages[1+i*2].ToolCalls[0].ID, newMessages[2+i*2].ToolCallID)
				require.NotEmpty(t, newMessages[2+i*2].Content)
			}
			require.Equal(t, "finished", newMessages[5].Content)
			ids := make(map[string]bool)
			for _, message := range stored {
				require.NotEmpty(t, message.ID)
				require.False(t, ids[message.ID], "duplicate persisted message ID")
				ids[message.ID] = true
			}
		})
	}
}

type chatPersistenceFailureStore struct {
	store.SessionStore
	writes            int
	failWrite         int
	commitBeforeError bool
	failure           error
}

func (s *chatPersistenceFailureStore) AppendMessages(ctx context.Context, namespace, name string, messages []store.SessionMessage) error {
	s.writes++
	if s.writes == s.failWrite {
		if s.commitBeforeError {
			if err := s.SessionStore.AppendMessages(ctx, namespace, name, messages); err != nil {
				return err
			}
		}
		return s.failure
	}
	return s.SessionStore.AppendMessages(ctx, namespace, name, messages)
}

func TestRunToolLoopSaveFailureStopsBeforeNextTool(t *testing.T) {
	for _, failWrite := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("write-%d", failWrite), func(t *testing.T) {
			ss := &chatPersistenceFailureStore{
				SessionStore: newTestSessionStore(t), failWrite: failWrite, commitBeforeError: true,
				failure: errors.New("save acknowledgement lost"),
			}
			createTestChatSession(t, ss, "default", "save-failure")
			fakeClient := fake.NewClientBuilder().WithScheme(newTestScheme()).Build()
			rs := newTestResultStore(t)
			ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())
			provider := &chatMockProvider{responses: []*llm.CompletionResponse{{ToolCalls: []llm.ToolCall{
				{ID: "one", Name: "list_tasks", Arguments: json.RawMessage(`{}`)},
				{ID: "two", Name: "list_tasks", Arguments: json.RawMessage(`{}`)},
			}}}}
			executed := 0
			emit := func(event, _ string) {
				if event == "tool_result" {
					executed++
				}
			}
			executor := NewToolExecutor(fakeClient, nil, "default", "save-failure", "", false, 5, time.Minute, rs)
			_, _, _, err := ch.runToolLoop(context.Background(), provider, []llm.Message{{Role: "user", Content: "inspect tasks"}},
				"instructions", nil, executor, "save-failure", "default", "test-model", 0, 100, 0, emit)
			require.ErrorIs(t, err, ss.failure)
			require.Equal(t, max(0, failWrite-2), executed, "a failed save must stop further tools")
			if failWrite == 1 {
				require.Zero(t, provider.callCount, "do not call the model before saving the request")
			}
			stored, err := ss.LoadTranscript(context.Background(), "default", "save-failure", 0)
			require.NoError(t, err)
			require.Len(t, stored, failWrite, "retrying an acknowledged-late commit must not duplicate messages")
		})
	}
}

func TestRunToolLoopCancellationSavesResultAndDoesNotReplayTools(t *testing.T) {
	ss := newTestSessionStore(t)
	createTestChatSession(t, ss, "default", "cancel-results")
	fakeClient := fake.NewClientBuilder().WithScheme(newTestScheme()).Build()
	rs := newTestResultStore(t)
	ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())
	provider := &chatMockProvider{responses: []*llm.CompletionResponse{{ToolCalls: []llm.ToolCall{
		{ID: "one", Name: "list_tasks", Arguments: json.RawMessage(`{}`)},
		{ID: "two", Name: "list_tasks", Arguments: json.RawMessage(`{}`)},
	}}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := 0
	emit := func(event, _ string) {
		if event == "tool_result" {
			results++
			cancel()
		}
	}
	executor := NewToolExecutor(fakeClient, nil, "default", "cancel-results", "", false, 5, time.Minute, rs)
	_, _, _, err := ch.runToolLoop(ctx, provider, []llm.Message{{Role: "user", Content: "inspect tasks"}},
		"instructions", nil, executor, "cancel-results", "default", "test-model", 0, 100, 0, emit)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, results)
	messages, err := ch.loadChatSession(context.Background(), "default", "cancel-results")
	require.NoError(t, err)
	require.Len(t, messages, 3)
	require.Equal(t, "one", messages[2].ToolCallID)
	historyCount := len(messages)
	messages = append(messages, llm.Message{Role: "user", Content: "continue"})
	_, _, _, err = ch.runToolLoop(context.Background(), provider, messages, "instructions", nil, executor,
		"cancel-results", "default", "test-model", 0, 100, historyCount, nil)
	require.ErrorContains(t, err, "incomplete tool exchange")
	require.Equal(t, 1, provider.callCount, "recovery must not ask the model to repeat unresolved actions")
}

func TestChatMessageBufferRejectsInvalidHistoryBoundary(t *testing.T) {
	for _, historyCount := range []int{-1, 2} {
		_, err := newChatMessageBuffer([]llm.Message{{Role: "user", Content: "request"}}, historyCount)
		require.Error(t, err)
	}
}

func TestCallLLMWithRetryRejectsUnfittableCurrentRequest(t *testing.T) {
	buffer, err := newChatMessageBuffer([]llm.Message{{Role: "user", Content: strings.Repeat("keep exactly", 100)}}, 0)
	require.NoError(t, err)
	provider := &chatMockProvider{}
	ch := &ChatHandler{config: ChatConfig{MaxSessionSize: 40}}
	_, err = ch.callLLMWithRetry(context.Background(), provider, buffer, "instructions", "test-model", nil, 100, 0)
	require.ErrorIs(t, err, llm.ErrRequiredContextTooLarge)
	require.Zero(t, provider.callCount)
	require.Len(t, buffer.pending, 1)
	require.Equal(t, buffer.active[0].Content, buffer.pending[0].Content)
}

func TestFlushChatMessagesRejectsStaleOwnerAndDeletedSession(t *testing.T) {
	ctx := context.Background()
	ss := newTestSessionStore(t)
	createTestChatSession(t, ss, "default", "stale-chat")
	require.NoError(t, ss.AcquireLock(ctx, "default", "stale-chat", "new-owner", "new-owner"))
	staleCtx := context.WithValue(ctx, chatSessionLockContextKey{}, chatSessionLockIdentity{ownerName: "old-owner", ownerUID: "old-owner"})
	buffer, err := newChatMessageBuffer([]llm.Message{{Role: "assistant", Content: "late result"}}, 0)
	require.NoError(t, err)
	ch := &ChatHandler{sessionStore: ss}
	require.ErrorIs(t, ch.flushChatMessages(staleCtx, "default", "stale-chat", buffer), store.ErrConflict)
	require.Len(t, buffer.pending, 1)
	stored, err := ss.LoadTranscript(ctx, "default", "stale-chat", 0)
	require.NoError(t, err)
	require.Empty(t, stored)
	require.NoError(t, ss.ReleaseLock(ctx, "default", "stale-chat", "new-owner", "new-owner"))
	require.NoError(t, ss.DeleteSession(ctx, "default", "stale-chat"))
	require.ErrorIs(t, ch.flushChatMessages(staleCtx, "default", "stale-chat", buffer), store.ErrNotFound)
	_, err = ss.GetSession(ctx, "default", "stale-chat")
	require.ErrorIs(t, err, store.ErrNotFound)
}

type chatTranscriptReadFailureStore struct {
	store.SessionStore
}

func (*chatTranscriptReadFailureStore) LoadTranscript(context.Context, string, string, int) ([]store.SessionMessage, error) {
	return nil, errors.New("transcript read failed")
}

func TestHandleChatRejectsHistoryReadFailure(t *testing.T) {
	const providerType = "chat-history-read-failure-test"
	provider := &chatMockProvider{}
	llm.RegisterProvider(providerType, func(llm.ProviderConfig) (llm.Provider, error) { return provider, nil })
	base := newTestSessionStore(t)
	createTestChatSession(t, base, "default", "read-failure")
	require.NoError(t, base.AppendMessages(context.Background(), "default", "read-failure", []store.SessionMessage{
		{Role: "user", Content: "Keep the API unchanged."},
	}))
	ss := &chatTranscriptReadFailureStore{SessionStore: base}
	objects := providerCRD("default", "default", providerType, "test-model")
	fakeClient := fake.NewClientBuilder().WithScheme(newTestScheme()).WithRuntimeObjects(objects...).Build()
	ch := newTestChatHandler(t, fakeClient, ss, newTestResultStore(t), DefaultChatConfig())
	app := fiber.New()
	app.Post("/api/v1/chat", ch.HandleChat)
	body, err := json.Marshal(ChatRequest{Message: "continue", SessionID: "read-failure"})
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/chat", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := app.Test(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusInternalServerError, response.StatusCode)
	require.Zero(t, provider.callCount)
	stored, err := base.LoadTranscript(context.Background(), "default", "read-failure", 0)
	require.NoError(t, err)
	require.Len(t, stored, 1)
	require.Equal(t, "Keep the API unchanged.", stored[0].Content)
}

// --- HandleChat ---

func TestHandleChat(t *testing.T) {
	scheme := newTestScheme()

	// Ensure test-type provider is registered
	llm.RegisterProvider("test-type", func(cfg llm.ProviderConfig) (llm.Provider, error) {
		return &chatMockProvider{
			name: "test-type",
			responses: []*llm.CompletionResponse{
				{Content: "Hello from chat!", InputTokens: 10, OutputTokens: 20},
			},
		}, nil
	})

	t.Run("empty message returns 400", func(t *testing.T) {
		objs := providerCRD("default", "default", "test-type", "test-model")
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
		ss := newTestSessionStore(t)
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		app := fiber.New()
		app.Post("/api/v1/chat", ch.HandleChat)

		body, _ := json.Marshal(ChatRequest{Message: ""})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("invalid body returns 400", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		ss := newTestSessionStore(t)
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		app := fiber.New()
		app.Post("/api/v1/chat", ch.HandleChat)

		req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", strings.NewReader("{invalid json"))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("blocked namespace returns 403", func(t *testing.T) {
		objs := providerCRD("default", "default", "test-type", "test-model")
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
		ss := newTestSessionStore(t)
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		app := fiber.New()
		app.Post("/api/v1/chat", ch.HandleChat)

		body, _ := json.Marshal(ChatRequest{Message: "hello", Namespace: "kube-system"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("namespace mismatch with watch namespace returns 403", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		ss := newTestSessionStore(t)
		rs := newTestResultStore(t)
		cfg := DefaultChatConfig()
		ch := NewChatHandler(fakeClient, nil, nil, cfg, "restricted-ns", false, ss, rs, NewProviderResolver(fakeClient, cfg))

		app := fiber.New()
		app.Post("/api/v1/chat", ch.HandleChat)

		body, _ := json.Marshal(ChatRequest{Message: "hello", Namespace: "other-ns"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	})

	t.Run("too many concurrent requests returns 429", func(t *testing.T) {
		objs := providerCRD("default", "default", "test-type", "test-model")
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
		ss := newTestSessionStore(t)
		rs := newTestResultStore(t)
		cfg := DefaultChatConfig()
		cfg.MaxConcurrent = 0 // zero capacity semaphore
		ch := newTestChatHandler(t, fakeClient, ss, rs, cfg)

		app := fiber.New()
		app.Post("/api/v1/chat", ch.HandleChat)

		body, _ := json.Marshal(ChatRequest{Message: "hello"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	})

	t.Run("JSON mode returns ChatResponse", func(t *testing.T) {
		objs := providerCRD("default", "default", "test-type", "test-model")
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
		ss := newTestSessionStore(t)
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		app := fiber.New()
		app.Post("/api/v1/chat", ch.HandleChat)

		body, _ := json.Marshal(ChatRequest{Message: "hello"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		resp, err := app.Test(req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var chatResp ChatResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&chatResp))
		assert.NotEmpty(t, chatResp.SessionID)
		assert.NotEmpty(t, chatResp.Message)
	})

	t.Run("SSE mode returns event stream", func(t *testing.T) {
		objs := providerCRD("default", "default", "test-type", "test-model")
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
		ss := newTestSessionStore(t)
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		app := fiber.New()
		app.Post("/api/v1/chat", ch.HandleChat)

		body, _ := json.Marshal(ChatRequest{Message: "hello"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		// No Accept: application/json → defaults to SSE
		resp, err := app.Test(req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

		// Parse SSE events
		scanner := bufio.NewScanner(resp.Body)
		var events []string
		for scanner.Scan() {
			line := scanner.Text()
			if after, ok := strings.CutPrefix(line, "event: "); ok {
				events = append(events, after)
			}
		}

		// Should have at least status and done events
		assert.Contains(t, events, "status")
		assert.Contains(t, events, "done")
	})

	t.Run("no provider returns 400", func(t *testing.T) {
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		ss := newTestSessionStore(t)
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		app := fiber.New()
		app.Post("/api/v1/chat", ch.HandleChat)

		body, _ := json.Marshal(ChatRequest{Message: "hello"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		resp, err := app.Test(req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("with session ID reuses session", func(t *testing.T) {
		objs := providerCRD("default", "default", "test-type", "test-model")
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
		ss := newTestSessionStore(t)
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		app := fiber.New()
		app.Post("/api/v1/chat", ch.HandleChat)

		body, _ := json.Marshal(ChatRequest{Message: "hello", SessionID: "my-session-123"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		resp, err := app.Test(req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.StatusCode)

		var chatResp ChatResponse
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&chatResp))
		assert.Equal(t, "my-session-123", chatResp.SessionID)
	})

	t.Run("SSE streaming mode", func(t *testing.T) {
		objs := providerCRD("default", "default", "test-type", "test-model")
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
		ss := newTestSessionStore(t)
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		app := fiber.New()
		app.Post("/api/v1/chat", ch.HandleChat)

		body, _ := json.Marshal(ChatRequest{Message: "hello"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		resp, err := app.Test(req)
		require.NoError(t, err)
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("provider not found returns error", func(t *testing.T) {
		// No provider CRDs registered, just a bare client
		fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
		ss := newTestSessionStore(t)
		rs := newTestResultStore(t)
		ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())

		app := fiber.New()
		app.Post("/api/v1/chat", ch.HandleChat)

		body, _ := json.Marshal(ChatRequest{Message: "hello"})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		resp, err := app.Test(req)
		require.NoError(t, err)
		// Provider resolution failure may return 400 or 500
		assert.True(t, resp.StatusCode >= 400, "expected error status code, got %d", resp.StatusCode)
	})
}

func TestLoadChatSessionRejectsGatewaySession(t *testing.T) {
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ss := sqlite.NewStore(db, ":memory:")
	require.NoError(t, ss.CreateSession(context.Background(), &store.SessionRecord{
		Namespace: "default", Name: "gateway-private", SessionType: store.SessionTypeGateway,
	}))
	require.NoError(t, ss.AppendMessages(context.Background(), "default", "gateway-private", []store.SessionMessage{{
		Role: "user", Content: "private gateway content",
	}}))

	ch := &ChatHandler{sessionStore: ss}
	_, err = ch.loadChatSession(context.Background(), "default", "gateway-private")
	require.ErrorIs(t, err, store.ErrGatewayOwnedSession)
}

func TestHandleChatHidesGatewaySessionBeforeProviderInvocation(t *testing.T) {
	const providerType = "gateway-session-boundary-test"
	mock := &chatMockProvider{name: providerType}
	llm.RegisterProvider(providerType, func(llm.ProviderConfig) (llm.Provider, error) {
		return mock, nil
	})
	objects := providerCRD(testDefaultNamespace, testDefaultNamespace, providerType, "test-model")
	fakeClient := fake.NewClientBuilder().WithScheme(newTestScheme()).WithRuntimeObjects(objects...).Build()
	ss := newTestSessionStore(t)
	rs := newTestResultStore(t)
	require.NoError(t, ss.CreateSession(context.Background(), &store.SessionRecord{
		Namespace: testDefaultNamespace, Name: "gateway-http-private", SessionType: store.SessionTypeGateway,
	}))
	cfg := DefaultChatConfig()
	cfg.Provider = testDefaultNamespace
	ch := newTestChatHandler(t, fakeClient, ss, rs, cfg)
	app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
	app.Post("/api/v1/chat", ch.HandleChat)
	body, err := json.Marshal(ChatRequest{
		Message: "reveal history", SessionID: "gateway-http-private", Namespace: testDefaultNamespace,
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	require.Zero(t, mock.callCount)
}

func TestHandleCancelChatHidesGatewaySession(t *testing.T) {
	ss := newTestSessionStore(t)
	rs := newTestResultStore(t)
	fakeClient := fake.NewClientBuilder().WithScheme(newTestScheme()).Build()
	ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())
	require.NoError(t, ss.CreateSession(context.Background(), &store.SessionRecord{
		Namespace: "default", Name: "gateway-cancel-private", SessionType: store.SessionTypeGateway,
	}))
	app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
	app.Delete("/api/v1/chat/:sessionId", ch.HandleCancelChat)
	resp, err := app.Test(httptest.NewRequest(http.MethodDelete, "/api/v1/chat/gateway-cancel-private", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
	_, err = ss.GetSession(context.Background(), "default", "gateway-cancel-private")
	require.NoError(t, err)
}

// --- Tests: loadChatSession with tool calls ---

func TestLoadChatSession_WithToolCalls(t *testing.T) {
	ss := newTestSessionStore(t)
	rs := newTestResultStore(t)
	cfg := DefaultChatConfig()
	fakeClient := fake.NewClientBuilder().WithScheme(newTestScheme()).Build()
	ch := newTestChatHandler(t, fakeClient, ss, rs, cfg)
	ctx := context.Background()

	now := time.Now()
	err := ss.CreateSession(ctx, &store.SessionRecord{
		Namespace:   "default",
		Name:        "tc-session",
		SessionType: "chat",
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	require.NoError(t, err)

	toolCallsData := []map[string]any{
		{"id": "call_1", "name": "search", "arguments": `{"q":"test"}`},
	}

	err = ss.AppendMessages(ctx, "default", "tc-session", []store.SessionMessage{
		{Role: "user", Content: "use search", Timestamp: now},
		{Role: "assistant", Content: "", ToolCalls: toolCallsData, Timestamp: now},
		{Role: "tool", Content: "search results", ToolCallID: "call_1", Name: "search", Timestamp: now},
	})
	require.NoError(t, err)

	msgs, err := ch.loadChatSession(ctx, "default", "tc-session")
	require.NoError(t, err)
	require.Len(t, msgs, 3)
	assert.Equal(t, "user", msgs[0].Role)
	assert.Equal(t, "assistant", msgs[1].Role)
	assert.Equal(t, "tool", msgs[2].Role)
	assert.Equal(t, "call_1", msgs[2].ToolCallID)
	assert.Equal(t, "search", msgs[2].Name)
}

func TestChatHandler_ContextTokenAuthorizationRejectsDisallowedModel(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	objs := providerCRD("default", "default", "openai", "gpt-4")
	fakeClient := fake.NewClientBuilder().WithScheme(newTestScheme()).WithRuntimeObjects(objs...).Build()
	ss := newTestSessionStore(t)
	rs := newTestResultStore(t)
	ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	require.NoError(t, err)
	ch.contextTokenAuthorization = authz

	app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
	app.Use(NewAuthMiddleware(fakeClient, AuthConfig{ContextTokens: ctxTokenConfig}))
	app.Post("/api/v1/chat", ch.HandleChat)

	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeProvidersUse + " " + ContextTokenScopeToolsUse,
		"tctx": map[string]any{
			"allowedModels": []string{"gpt-3.5-turbo"},
		},
	})
	body, _ := json.Marshal(ChatRequest{Message: "hello", Provider: "default", Model: "gpt-4"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", bytes.NewReader(body))
	req.Header.Set(TransactionTokenHeaderName, token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestChatHandler_ContextTokenAuthorizationRejectsDisallowedAgentRef(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	fakeClient := fake.NewClientBuilder().WithScheme(newTestScheme()).Build()
	ss := newTestSessionStore(t)
	rs := newTestResultStore(t)
	ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	require.NoError(t, err)
	ch.contextTokenAuthorization = authz

	app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
	app.Use(NewAuthMiddleware(fakeClient, AuthConfig{ContextTokens: ctxTokenConfig}))
	app.Post("/api/v1/chat", ch.HandleChat)

	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeProvidersUse,
		"tctx": map[string]any{
			"allowedAgents": []string{"allowed-agent"},
		},
	})
	body, _ := json.Marshal(ChatRequest{Message: "hello", AgentRef: "other-agent"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", bytes.NewReader(body))
	req.Header.Set(TransactionTokenHeaderName, token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestChatHandler_ContextTokenAuthorizationRejectsMissingAgentRefWhenTokenRequiresAgent(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	fakeClient := fake.NewClientBuilder().WithScheme(newTestScheme()).Build()
	ss := newTestSessionStore(t)
	rs := newTestResultStore(t)
	ch := newTestChatHandler(t, fakeClient, ss, rs, DefaultChatConfig())
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	require.NoError(t, err)
	ch.contextTokenAuthorization = authz

	app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
	app.Use(NewAuthMiddleware(fakeClient, AuthConfig{ContextTokens: ctxTokenConfig}))
	app.Post("/api/v1/chat", ch.HandleChat)

	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeProvidersUse,
		"tctx": map[string]any{
			"allowedAgents": []string{"allowed-agent"},
		},
	})
	body, _ := json.Marshal(ChatRequest{Message: "hello"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat", bytes.NewReader(body))
	req.Header.Set(TransactionTokenHeaderName, token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestPreAcquisitionCancellationPreservesDeletionGate(t *testing.T) {
	base := newTestSessionStore(t)
	blocking := &cancelableCreateSessionStore{SessionStore: base, started: make(chan struct{})}
	ch := &ChatHandler{sessionStore: blocking, config: DefaultChatConfig(), activeChats: make(map[string]*activeChatRequest)}
	ctx := context.Background()
	result := make(chan error, 1)
	go func() {
		_, err := beginTestActiveChat(ch, ctx, "pre-acquire-cancel")
		result <- err
	}()
	select {
	case <-blocking.started:
	case <-time.After(5 * time.Second):
		t.Fatal("chat acquisition did not reach CreateSession")
	}
	active, err := ch.cancelAndWaitForActiveChat(ctx, "default", "pre-acquire-cancel")
	require.NoError(t, err)
	if !active {
		t.Fatal("cancelAndWaitForActiveChat() did not observe reserved acquisition gate")
	}
	if err := <-result; err == nil {
		t.Fatal("beginActiveChat() unexpectedly succeeded after cancellation")
	}
	if _, err := beginTestActiveChat(ch, ctx, "pre-acquire-cancel"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("beginActiveChat(before DELETE completion) error = %v, want ErrConflict", err)
	}
	ch.clearChatDeletionGate("default", "pre-acquire-cancel")
}

type cancelableCreateSessionStore struct {
	store.SessionStore
	started chan struct{}
	once    sync.Once
}

func (s *cancelableCreateSessionStore) CreateSession(ctx context.Context, _ *store.SessionRecord) error {
	s.once.Do(func() { close(s.started) })
	<-ctx.Done()
	return ctx.Err()
}

func TestAbandonedCancellationWaiterDoesNotLeakGate(t *testing.T) {
	ss := newTestSessionStore(t)
	ch := &ChatHandler{sessionStore: ss, config: DefaultChatConfig(), activeChats: make(map[string]*activeChatRequest)}
	active, err := beginTestActiveChat(ch, context.Background(), "abandoned-cancel")
	require.NoError(t, err)
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	found, err := ch.cancelAndWaitForActiveChat(cancelCtx, "default", "abandoned-cancel")
	if !found || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelAndWaitForActiveChat() = found:%t err:%v, want true,context.Canceled", found, err)
	}
	active.finish()
	replacement, err := beginTestActiveChat(ch, context.Background(), "abandoned-cancel")
	require.NoError(t, err)
	replacement.finish()
}

func beginTestActiveChat(ch *ChatHandler, ctx context.Context, sessionID string) (*activeChatHandle, error) {
	reservation, err := ch.reserveActiveChat(defaultNamespace, sessionID)
	if err != nil {
		return nil, err
	}
	active, err := ch.activateReservedChat(ctx, reservation, defaultNamespace, sessionID)
	if err != nil {
		ch.finishActiveChatReservation(reservation, nil)
		return nil, err
	}
	return active, nil
}
