package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/store"
)

// A storage write that was not accepted for now, or whose outcome is
// unknown, is neither a success nor a definite failure: the API answers 503
// with a retry hint.
func TestTemporaryStorageErrorsAnswerServiceUnavailable(t *testing.T) {
	app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
	routes := map[string]error{
		"/outcome-unknown": fmt.Errorf("save result: %w", store.ErrOutcomeUnknown),
		"/not-ready":       fmt.Errorf("save result: %w", store.ErrNotReady),
		"/unavailable":     fmt.Errorf("save result: %w", store.ErrUnavailable),
		"/gateway-store":   gatewayStoreHTTPError(fmt.Errorf("retry: %w", store.ErrOutcomeUnknown), "retry delivery"),
		"/failure":         errors.New("boom"),
	}
	for path, err := range routes {
		app.Get(path, func(fiber.Ctx) error { return err })
	}
	for path := range routes {
		response, err := app.Test(httptest.NewRequest(http.MethodGet, path, nil))
		require.NoError(t, err)
		_ = response.Body.Close()
		if path == "/failure" {
			require.Equal(t, http.StatusInternalServerError, response.StatusCode)
			require.Empty(t, response.Header.Get(fiber.HeaderRetryAfter))
			continue
		}
		require.Equal(t, http.StatusServiceUnavailable, response.StatusCode, path)
		require.Equal(t, serviceUnavailableRetryAfterSeconds, response.Header.Get(fiber.HeaderRetryAfter), path)
	}
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
	err := memoryStoreError("create memory", "memory", fmt.Errorf("create: %w", store.ErrOutcomeUnknown))
	fiberErr, ok := errors.AsType[*fiber.Error](err)
	require.True(t, ok)
	require.Equal(t, fiber.StatusServiceUnavailable, fiberErr.Code)
}
