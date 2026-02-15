/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"

	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/store/sqlite"
)

func setupTestInternalHandlers() (*InternalHandlers, *fiber.App, *sqlite.Store) {
	db, _ := sqlite.NewDB(":memory:")
	ss := sqlite.NewStore(db, ":memory:")
	h := NewInternalHandlers(ss, ss, ss, ss)
	app := fiber.New()

	// Inject a default UserInfo so verifyCallerNamespace passes
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{
			Username: "system:serviceaccount:default:worker",
		})
		return c.Next()
	})

	return h, app, ss
}

func TestSubmitPlan(t *testing.T) {
	h, app, _ := setupTestInternalHandlers()
	app.Post("/internal/v1/plans/:namespace/:taskName", h.SubmitPlan)

	t.Run("success", func(t *testing.T) {
		body := map[string]any{
			"summary":       "step 1 done",
			"progress_pct":  50,
			"goal_complete": false,
			"plan_document": "# Plan\n- step 1\n- step 2",
		}
		bodyBytes, _ := json.Marshal(body)

		req := httptest.NewRequest(http.MethodPost, "/internal/v1/plans/default/my-task", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
	})

	t.Run("missing taskName", func(t *testing.T) {
		body := map[string]any{"summary": "test"}
		bodyBytes, _ := json.Marshal(body)

		// Use a route that provides namespace but omits taskName
		app2 := fiber.New()
		app2.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{
				Username: "system:serviceaccount:default:worker",
			})
			return c.Next()
		})
		app2.Post("/internal/v1/plans/:namespace", h.SubmitPlan)

		req := httptest.NewRequest(http.MethodPost, "/internal/v1/plans/default", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app2.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
}

func TestGetPlan(t *testing.T) {
	h, app, ss := setupTestInternalHandlers()
	app.Get("/internal/v1/plans/:namespace/:taskName", h.GetPlan)

	// Pre-save a plan
	err := ss.SavePlan(context.Background(), "default", "my-task", &store.PlanState{
		TaskName:     "my-task",
		Namespace:    "default",
		Summary:      "halfway there",
		ProgressPct:  50,
		GoalComplete: false,
		PlanDocument: "# Plan",
	})
	require.NoError(t, err)

	t.Run("existing plan", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/internal/v1/plans/default/my-task", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var plan store.PlanState
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&plan))
		require.Equal(t, "halfway there", plan.Summary)
		require.Equal(t, 50, plan.ProgressPct)
		require.Equal(t, "# Plan", plan.PlanDocument)
	})

	t.Run("not found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/internal/v1/plans/default/nonexistent", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusNotFound, resp.StatusCode)
	})
}

func TestSendMessage(t *testing.T) {
	h, app, _ := setupTestInternalHandlers()
	app.Post("/internal/v1/messages/:namespace", h.SendMessage)

	t.Run("success", func(t *testing.T) {
		body := map[string]string{
			"fromTask":   "worker-a",
			"toTask":     "worker-b",
			"parentTask": "coordinator",
			"content":    "found a bug",
		}
		bodyBytes, _ := json.Marshal(body)

		req := httptest.NewRequest(http.MethodPost, "/internal/v1/messages/default", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
	})

	t.Run("missing required fields", func(t *testing.T) {
		body := map[string]string{
			"fromTask": "worker-a",
		}
		bodyBytes, _ := json.Marshal(body)

		req := httptest.NewRequest(http.MethodPost, "/internal/v1/messages/default", bytes.NewReader(bodyBytes))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("invalid body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/messages/default", bytes.NewReader([]byte("not json")))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
}

func TestGetMessages(t *testing.T) {
	h, app, ss := setupTestInternalHandlers()
	app.Get("/internal/v1/messages/:namespace/:taskName", h.GetMessages)

	// Pre-send a message
	err := ss.SendMessage(context.Background(), &store.Message{
		Namespace:  "default",
		FromTask:   "worker-a",
		ToTask:     "worker-b",
		ParentTask: "coordinator",
		Content:    "hello from a",
	})
	require.NoError(t, err)

	t.Run("has messages", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/internal/v1/messages/default/worker-b?parentTask=coordinator&markRead=false", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var messages []store.Message
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&messages))
		require.Len(t, messages, 1)
		require.Equal(t, "hello from a", messages[0].Content)
	})

	t.Run("no messages", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/internal/v1/messages/default/worker-c?parentTask=coordinator", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var messages []store.Message
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&messages))
		require.Empty(t, messages)
	})

	t.Run("missing parentTask", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/internal/v1/messages/default/worker-b", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
}
