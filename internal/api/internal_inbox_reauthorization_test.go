package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/storageclient/storageclienttest"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// cleanupBeforeReplayProvider commits the first ConsumeMessages and the first
// SendMessage it receives, runs another Task's cleanup in the namespace, and
// then loses the response. The storage client's replay of the committed
// operation therefore finds the namespace's Task data fence moved. It records
// the operation ID of every attempt.
type cleanupBeforeReplayProvider struct {
	*sqlite.Store
	afterCommit func(method string) error

	mu         sync.Mutex
	lost       map[string]bool
	operations map[string][]string
}

func (p *cleanupBeforeReplayProvider) attempt(ctx context.Context, method string, call func(context.Context) error) error {
	envelope, _ := storagev1.WriteEnvelopeFrom(ctx)
	p.mu.Lock()
	p.operations[method] = append(p.operations[method], envelope.OperationID)
	p.mu.Unlock()
	if err := call(ctx); err != nil {
		return err
	}
	p.mu.Lock()
	first := !p.lost[method]
	p.lost[method] = true
	p.mu.Unlock()
	if !first {
		return nil
	}
	if err := p.afterCommit(method); err != nil {
		return err
	}
	return storagev1.ErrOutcomeUnknown
}

func (p *cleanupBeforeReplayProvider) ConsumeMessages(ctx context.Context, request store.ConsumeMessagesRequest) ([]store.Message, error) {
	var messages []store.Message
	err := p.attempt(ctx, "ConsumeMessages", func(ctx context.Context) error {
		var err error
		messages, err = p.Store.ConsumeMessages(ctx, request)
		return err
	})
	return messages, err
}

func (p *cleanupBeforeReplayProvider) SendMessage(ctx context.Context, msg *store.Message) error {
	return p.attempt(ctx, "SendMessage", func(ctx context.Context) error {
		return p.Store.SendMessage(ctx, msg)
	})
}

// requireOneOperation requires every attempt of method to carry the same
// operation ID: the route never ran the write again as a new operation.
func (p *cleanupBeforeReplayProvider) requireOneOperation(t *testing.T, method string) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := p.operations[method]
	require.GreaterOrEqual(t, len(ids), 2, "%s was not retried after its lost response", method)
	for _, id := range ids {
		require.Equal(t, ids[0], id, "%s ran again as a new operation", method)
	}
}

type reauthorizationFixture struct {
	app      *fiber.App
	provider *cleanupBeforeReplayProvider
	backend  *sqlite.Store
}

// newReauthorizationFixture serves the worker message routes, with the
// server's error handler and live caller authorization, over a storage
// client whose provider loses the first committed response after a sibling
// Task's cleanup. extra runs in that window too.
func newReauthorizationFixture(t *testing.T, extra func(storeClient store.MessageStore) error) *reauthorizationFixture {
	t.Helper()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	backend := sqlite.NewStore(db, ":memory:")
	sibling := testStorageClient(backend)
	provider := &cleanupBeforeReplayProvider{
		Store: backend, lost: map[string]bool{}, operations: map[string][]string{},
		afterCommit: func(string) error {
			if extra != nil {
				if err := extra(sibling); err != nil {
					return err
				}
			}
			// Another Task's cleanup advances the namespace's cleanup
			// generation, which fences the worker message routes.
			return sibling.DeleteResult(context.Background(), "default", "worker-c")
		},
	}
	h, _, _ := setupTestInternalHandlers()
	h.messageStore = storageclienttest.NewEmbedded(t, provider)
	app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
	user := internalCallerAuthWorkerUser("my-task-pod", "my-task-pod-uid")
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, user)
		return c.Next()
	})
	app.Post("/internal/v1/messages/:namespace", h.SendMessage)
	app.Get("/internal/v1/messages/:namespace/:taskName", h.GetMessages)
	return &reauthorizationFixture{app: app, provider: provider, backend: backend}
}

func (f *reauthorizationFixture) do(t *testing.T, method, path string, body any) (*http.Response, string) {
	t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := f.app.Test(req, fiber.TestConfig{Timeout: 30 * time.Second, FailOnTimeout: true})
	require.NoError(t, err)
	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp, string(respBody)
}

func requireUnknownOutcomeConflict(t *testing.T, resp *http.Response, body string) {
	t.Helper()
	require.Equal(t, http.StatusConflict, resp.StatusCode, body)
	require.Contains(t, body, "do not retry")
	require.Empty(t, resp.Header.Get(fiber.HeaderRetryAfter))
}

func unreadContents(t *testing.T, backend *sqlite.Store, reader string) []string {
	t.Helper()
	messages, err := backend.ListUnreadMessages(t.Context(), "default", reader, "coordinator", 0)
	require.NoError(t, err)
	contents := make([]string, 0, len(messages))
	for _, message := range messages {
		contents = append(contents, message.Content)
	}
	return contents
}

// A consuming inbox read whose ConsumeMessages committed, lost its response,
// and could not be replayed because another Task's cleanup moved the
// namespace's Task data fence in between answers 409 without a retry hint.
// The route does not authorize again and consume a second batch: a message
// sent in that window stays unread for the next read.
func TestConsumingInboxKeepsAnUnreadableReplayUnknownAcrossCleanup(t *testing.T) {
	f := newReauthorizationFixture(t, func(sibling store.MessageStore) error {
		return sibling.SendMessage(context.Background(), &store.Message{
			Namespace: "default", FromTask: "worker-b", ToTask: "my-task", ParentTask: "coordinator", Content: "sent during the replay window",
		})
	})
	for _, content := range []string{"one", "two"} {
		require.NoError(t, testStorageClient(f.backend).SendMessage(t.Context(), &store.Message{
			Namespace: "default", FromTask: "worker-b", ToTask: "my-task", ParentTask: "coordinator", Content: content,
		}))
	}

	resp, body := f.do(t, http.MethodGet, consumingInboxPath, nil)
	requireUnknownOutcomeConflict(t, resp, body)
	require.NotContains(t, body, `"one"`, "the consumed batch is not returned")
	f.provider.requireOneOperation(t, "ConsumeMessages")
	require.Equal(t, []string{"sent during the replay window"}, unreadContents(t, f.backend, "my-task"),
		"the committed batch is consumed once, and no second consuming read ran")

	resp, body = f.do(t, http.MethodGet, consumingInboxPath, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	requireInboxContents(t, body, "sent during the replay window")
}

// A send whose SendMessage committed, lost its response, and could not be
// replayed because another Task's cleanup moved the namespace's fence answers
// 409 without a retry hint and stores the message once.
func TestSendKeepsAnUnreadableReplayUnknownAcrossCleanup(t *testing.T) {
	f := newReauthorizationFixture(t, nil)
	resp, body := f.do(t, http.MethodPost, "/internal/v1/messages/default", map[string]string{
		"fromTask": "my-task", "toTask": "worker-b", "parentTask": "coordinator", "content": "exactly once",
	})
	requireUnknownOutcomeConflict(t, resp, body)
	f.provider.requireOneOperation(t, "SendMessage")
	require.Equal(t, []string{"exactly once"}, unreadContents(t, f.backend, "worker-b"), "the message is stored once")
}
