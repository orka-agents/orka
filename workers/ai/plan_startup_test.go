package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLoadPlanContextRetriesStartupAuthorization(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if requests.Add(1) == 1 {
					w.WriteHeader(status)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"Summary":"prior iteration","PlanDocument":"retained plan","Iteration":2}`)
			}))
			defer server.Close()
			t.Setenv("ORKA_CONTROLLER_URL", server.URL)
			t.Setenv("ORKA_TASK_NAME", "test-task")
			t.Setenv("ORKA_TASK_NAMESPACE", "default")

			result, err := loadPlanContext(t.Context())
			require.NoError(t, err)
			require.Contains(t, result, "retained plan")
			require.EqualValues(t, 2, requests.Load())
		})
	}
}

func TestLoadPlanContextCancelsStartupAuthorizationWait(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			denied := make(chan struct{})
			var firstRequest sync.Once
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(status)
				firstRequest.Do(func() { close(denied) })
			}))
			defer server.Close()
			t.Setenv("ORKA_CONTROLLER_URL", server.URL)
			t.Setenv("ORKA_TASK_NAME", "test-task")
			t.Setenv("ORKA_TASK_NAMESPACE", "default")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			finished := make(chan error, 1)
			go func() {
				_, err := loadPlanContext(ctx)
				finished <- err
			}()
			select {
			case <-denied:
			case <-time.After(5 * time.Second):
				t.Fatal("plan request did not reach the controller")
			}
			cancel()
			select {
			case err := <-finished:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(5 * time.Second):
				t.Fatal("plan loading did not honor worker cancellation")
			}
		})
	}
}
