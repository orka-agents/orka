package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/storageclient/storagefault"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

const consumingInboxPath = "/internal/v1/messages/default/my-task?parentTask=coordinator&markRead=true"

// inboxOutcomeApp serves the inbox route with the server's error handler over
// a storage client whose ConsumeMessages calls the returned injector controls.
// Each request runs under timeout, so a write the client keeps retrying ends
// the way a worker's request deadline ends it.
func inboxOutcomeApp(t *testing.T, timeout time.Duration) (*fiber.App, *storagefault.Injector) {
	t.Helper()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	messages, injector, err := storagefault.NewEmbeddedClient(sqlite.NewStore(db, ":memory:"))
	require.NoError(t, err)
	h, _, _ := setupTestInternalHandlers()
	h.messageStore = messages
	app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
	user := internalCallerAuthWorkerUser("my-task-pod", "my-task-pod-uid")
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, user)
		ctx, cancel := context.WithTimeout(c.Context(), timeout)
		defer cancel()
		c.SetContext(ctx)
		return c.Next()
	})
	app.Get("/internal/v1/messages/:namespace/:taskName", h.GetMessages)
	require.NoError(t, messages.SendMessage(t.Context(), &store.Message{
		Namespace: "default", FromTask: "worker-b", ToTask: "my-task", ParentTask: "coordinator", Content: "hello",
	}))
	return app, injector
}

func getInbox(t *testing.T, app *fiber.App) (*http.Response, string) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, consumingInboxPath, nil),
		fiber.TestConfig{Timeout: 30 * time.Second, FailOnTimeout: true})
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp, string(body)
}

func requireInboxContents(t *testing.T, body string, want ...string) {
	t.Helper()
	var messages []store.Message
	require.NoError(t, json.Unmarshal([]byte(body), &messages), body)
	got := make([]string, 0, len(messages))
	for _, message := range messages {
		got = append(got, message.Content)
	}
	require.Equal(t, append([]string{}, want...), got)
}

// A consuming inbox read whose ConsumeMessages committed but whose result the
// storage client could not read answers 409 without a retry hint: a new HTTP
// request carries a new operation ID, so it cannot replay the consumed batch
// and would return the inbox without it.
func TestInternalConsumingInboxAnswersUnknownOutcomeWithoutRetryHint(t *testing.T) {
	app, injector := inboxOutcomeApp(t, 10*time.Second)
	consume := storagefault.Match{Methods: []string{"ConsumeMessages"}}
	// The first attempt commits and loses its response; every later attempt,
	// including the client's replay after it resolves the operation as
	// committed, is refused, so the result cannot be read.
	refused := injector.Inject(storagefault.Fault{
		Match: consume, Action: storagefault.Fail(fmt.Errorf("%w: refused by the test", store.ErrConflict)),
	})
	lost := injector.Inject(storagefault.Fault{Match: consume, Action: storagefault.LoseResponse(), Times: 1})

	resp, body := getInbox(t, app)
	require.Equal(t, http.StatusConflict, resp.StatusCode, body)
	require.Contains(t, body, "storage write outcome is unknown; do not retry this request")
	require.Empty(t, resp.Header.Get(fiber.HeaderRetryAfter))
	require.Equal(t, 1, lost.Hits())
	require.Positive(t, refused.Hits())

	// The batch was consumed, so a retry would have lost it.
	refused.Remove()
	resp, body = getInbox(t, app)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	requireInboxContents(t, body)
}

// A consuming inbox read whose ConsumeMessages never reached the provider
// answers 503 with a retry hint once the request's deadline ends the client's
// retries: nothing was consumed, so a new request returns the same batch.
func TestInternalConsumingInboxAnswersUnavailableWithRetryHint(t *testing.T) {
	app, injector := inboxOutcomeApp(t, time.Second)
	unavailable := injector.Inject(storagefault.Fault{
		Match:  storagefault.Match{Methods: []string{"ConsumeMessages"}},
		Action: storagefault.Fail(fmt.Errorf("%w: refused by the test", store.ErrUnavailable)),
	})

	resp, body := getInbox(t, app)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, body)
	require.Equal(t, serviceUnavailableRetryAfterSeconds, resp.Header.Get(fiber.HeaderRetryAfter))
	require.NotContains(t, body, "do not retry")
	require.Positive(t, unavailable.Hits())
	require.Zero(t, injector.Counts(storagefault.Match{Methods: []string{"ConsumeMessages"}}).Delivered)

	unavailable.Remove()
	resp, body = getInbox(t, app)
	require.Equal(t, http.StatusOK, resp.StatusCode, body)
	requireInboxContents(t, body, "hello")
}
