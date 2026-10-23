package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"

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
