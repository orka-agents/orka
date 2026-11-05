package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/store"
)

// Only known uncommitted storage refusals may advertise an HTTP retry. An
// uncertain write might have consumed messages or created an assigned UUID;
// a new HTTP request would use a new operation ID and could lose or duplicate it.
func TestStorageErrorRetryHintsRequireKnownUncommittedOutcome(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
	routes := map[string]struct {
		err    error
		status int
	}{
		"/outcome-unknown": {fmt.Errorf("save result: %w", store.ErrOutcomeUnknown), http.StatusConflict},
		"/not-ready":       {fmt.Errorf("save result: %w", store.ErrNotReady), http.StatusServiceUnavailable},
		"/unavailable":     {fmt.Errorf("save result: %w", store.ErrUnavailable), http.StatusServiceUnavailable},
		"/gateway-store":   {gatewayStoreHTTPError(fmt.Errorf("retry: %w", store.ErrOutcomeUnknown), "retry delivery"), http.StatusConflict},
		"/memory-create":   {memoryStoreError("create memory", "memory", store.ErrOutcomeUnknown), http.StatusConflict},
		"/proposal-create": {memoryStoreError("create proposal", "memory proposal", store.ErrOutcomeUnknown), http.StatusConflict},
		"/failure":         {errors.New("boom"), http.StatusInternalServerError},
	}
	for path, test := range routes {
		app.Get(path, func(fiber.Ctx) error { return test.err })
	}
	for path, test := range routes {
		response, err := app.Test(httptest.NewRequest(http.MethodGet, path, nil))
		require.NoError(t, err)
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		require.Equal(t, test.status, response.StatusCode, path)
		if test.status == http.StatusServiceUnavailable {
			require.Equal(t, serviceUnavailableRetryAfterSeconds, response.Header.Get(fiber.HeaderRetryAfter), path)
		} else {
			require.Empty(t, response.Header.Get(fiber.HeaderRetryAfter), path)
		}
		if test.status == http.StatusConflict {
			require.Contains(t, string(body), "outcome is unknown; do not retry this request", path)
		}
	}
}

type uncertainTaskDataAccessStore struct {
	store.TaskDataAccessStore
	calls int
}

func (s *uncertainTaskDataAccessStore) WithAuthorizedTaskDataAccess(
	context.Context, string, string,
	func(context.Context) ([]store.TaskJobIdentity, error), func(context.Context) error,
) error {
	s.calls++
	return errors.Join(store.ErrOutcomeUnknown, store.ErrTaskDataCleanupChanged)
}

func TestTaskDataUnknownOutcomeTakesPriorityOverCleanupRetry(t *testing.T) {
	backing := &uncertainTaskDataAccessStore{}
	app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
	app.Get("/namespaces/:namespace/messages", func(c fiber.Ctx) error {
		return withInternalTaskData(c, backing, "", func(context.Context) error { return nil }, func(context.Context) error { return nil })
	})
	response, err := app.Test(httptest.NewRequest(http.MethodGet, "/namespaces/tenant/messages", nil))
	require.NoError(t, err)
	defer response.Body.Close() //nolint:errcheck
	require.Equal(t, http.StatusConflict, response.StatusCode)
	require.Empty(t, response.Header.Get(fiber.HeaderRetryAfter))
	require.Equal(t, 1, backing.calls, "an uncertain operation must not be reauthorized and repeated")
}

// unacceptedEventStore refuses execution events, as the storage client does
// while the controller's fence is not open.
type unacceptedEventStore struct {
	*storageclient.Client
}

func (unacceptedEventStore) AppendExecutionEvent(context.Context, *store.ExecutionEvent) (*store.ExecutionEvent, error) {
	return nil, fmt.Errorf("append: %w", store.ErrNotReady)
}

// The worker execution event route answers a write the store has not
// accepted yet with 503, not 500, so the worker retries it.
func TestInternalSubmitExecutionEventAnswersServiceUnavailableWhileStorageIsNotReady(t *testing.T) {
	eventStore := unacceptedEventStore{Client: newInternalExecutionEventStore(t)}
	app := setupOwnedInternalExecutionEventApp(t, eventStore, "task-1", "worker-pod", "worker-pod-uid")
	resp := doJSONRequest(t, app, "/internal/v1/events/default/task/task-1", map[string]any{"type": events.ExecutionEventTypeTaskStarted})
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
}

// Memory routes map a temporary storage refusal to 503 before any other
// storage error.
func TestMemoryStoreErrorAnswersServiceUnavailableWhileStorageIsNotReady(t *testing.T) {
	err := memoryStoreError("create memory", "memory", fmt.Errorf("create: %w", store.ErrNotReady))
	fiberErr, ok := errors.AsType[*fiber.Error](err)
	require.True(t, ok)
	require.Equal(t, fiber.StatusServiceUnavailable, fiberErr.Code)
}
