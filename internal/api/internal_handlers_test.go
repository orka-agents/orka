/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

	t.Run("messaging not enabled", func(t *testing.T) {
		hNoMsg := NewInternalHandlers(ss, ss, ss, nil)
		app2 := fiber.New()
		app2.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{Username: "system:serviceaccount:default:worker"})
			return c.Next()
		})
		app2.Get("/internal/v1/messages/:namespace/:taskName", hNoMsg.GetMessages)

		req := httptest.NewRequest(http.MethodGet, "/internal/v1/messages/default/worker-b?parentTask=coordinator", nil)
		resp, err := app2.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
	})

	t.Run("missing taskName", func(t *testing.T) {
		app2 := fiber.New()
		app2.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{Username: "system:serviceaccount:default:worker"})
			return c.Next()
		})
		app2.Get("/internal/v1/messages/:namespace", h.GetMessages)

		req := httptest.NewRequest(http.MethodGet, "/internal/v1/messages/default?parentTask=coordinator", nil)
		resp, err := app2.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("markRead defaults to true", func(t *testing.T) {
		// Send a fresh message for this sub-test
		err := ss.SendMessage(context.Background(), &store.Message{
			Namespace: "default", FromTask: "sender", ToTask: "reader",
			ParentTask: "coord", Content: "read me",
		})
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/internal/v1/messages/default/reader?parentTask=coord", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)

		var msgs []store.Message
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&msgs))
		require.Len(t, msgs, 1)

		// Second call should return empty because markRead defaulted to true
		req2 := httptest.NewRequest(http.MethodGet, "/internal/v1/messages/default/reader?parentTask=coord", nil)
		resp2, err := app.Test(req2)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp2.StatusCode)

		var msgs2 []store.Message
		require.NoError(t, json.NewDecoder(resp2.Body).Decode(&msgs2))
		require.Empty(t, msgs2)
	})
}

func TestSubmitResult(t *testing.T) {
	h, app, _ := setupTestInternalHandlers()
	app.Post("/internal/v1/results/:namespace/:taskName", h.SubmitResult)

	t.Run("success", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/results/default/my-task",
			bytes.NewReader([]byte(`{"output":"done"}`)))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
	})

	t.Run("missing namespace", func(t *testing.T) {
		app2 := fiber.New()
		app2.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{Username: "system:serviceaccount:default:worker"})
			return c.Next()
		})
		app2.Post("/internal/v1/results/:namespace", h.SubmitResult)

		req := httptest.NewRequest(http.MethodPost, "/internal/v1/results/default",
			bytes.NewReader([]byte(`{"output":"done"}`)))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app2.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("empty body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/results/default/my-task",
			bytes.NewReader([]byte{}))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("cross-namespace denied", func(t *testing.T) {
		app2 := fiber.New()
		app2.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{
				Username: "system:serviceaccount:other-ns:worker",
			})
			return c.Next()
		})
		app2.Post("/internal/v1/results/:namespace/:taskName", h.SubmitResult)

		req := httptest.NewRequest(http.MethodPost, "/internal/v1/results/default/my-task",
			bytes.NewReader([]byte(`{"output":"done"}`)))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app2.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
}

func TestGetSessionTranscript(t *testing.T) {
	h, app, ss := setupTestInternalHandlers()
	app.Get("/internal/v1/sessions/:namespace/:name/transcript", h.GetSessionTranscript)

	// Create a session and append messages
	err := ss.CreateSession(context.Background(), &store.SessionRecord{
		Namespace:   "default",
		Name:        "my-session",
		SessionType: "task",
	})
	require.NoError(t, err)

	err = ss.AppendMessages(context.Background(), "default", "my-session", []store.SessionMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
	})
	require.NoError(t, err)

	t.Run("success", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/internal/v1/sessions/default/my-session/transcript", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "application/x-ndjson", resp.Header.Get("Content-Type"))

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		lines := strings.Split(strings.TrimSpace(string(body)), "\n")
		require.Len(t, lines, 2)

		var msg1, msg2 store.SessionMessage
		require.NoError(t, json.Unmarshal([]byte(lines[0]), &msg1))
		require.NoError(t, json.Unmarshal([]byte(lines[1]), &msg2))
		require.Equal(t, "user", msg1.Role)
		require.Equal(t, "hello", msg1.Content)
		require.Equal(t, "assistant", msg2.Role)
		require.Equal(t, "hi there", msg2.Content)
	})

	t.Run("missing name", func(t *testing.T) {
		app2 := fiber.New()
		app2.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{Username: "system:serviceaccount:default:worker"})
			return c.Next()
		})
		app2.Get("/internal/v1/sessions/:namespace/transcript", h.GetSessionTranscript)

		req := httptest.NewRequest(http.MethodGet, "/internal/v1/sessions/default/transcript", nil)
		resp, err := app2.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("empty transcript", func(t *testing.T) {
		err := ss.CreateSession(context.Background(), &store.SessionRecord{
			Namespace:   "default",
			Name:        "empty-session",
			SessionType: "task",
		})
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodGet, "/internal/v1/sessions/default/empty-session/transcript", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)

		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Empty(t, strings.TrimSpace(string(body)))
	})
}

func TestVerifyCallerNamespace(t *testing.T) {
	h, _, _ := setupTestInternalHandlers()

	t.Run("no auth returns unauthorized", func(t *testing.T) {
		app := fiber.New()
		// No UserInfo middleware — simulates unauthenticated request
		app.Post("/internal/v1/results/:namespace/:taskName", h.SubmitResult)

		req := httptest.NewRequest(http.MethodPost, "/internal/v1/results/default/my-task",
			bytes.NewReader([]byte(`{"output":"done"}`)))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("non-SA user passes through", func(t *testing.T) {
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{Username: "admin"})
			return c.Next()
		})
		app.Post("/internal/v1/results/:namespace/:taskName", h.SubmitResult)

		req := httptest.NewRequest(http.MethodPost, "/internal/v1/results/default/my-task",
			bytes.NewReader([]byte(`{"output":"done"}`)))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusNoContent, resp.StatusCode)
	})

	t.Run("cross-namespace SA denied", func(t *testing.T) {
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{
				Username: "system:serviceaccount:kube-system:controller",
			})
			return c.Next()
		})
		app.Post("/internal/v1/plans/:namespace/:taskName", h.SubmitPlan)

		body, _ := json.Marshal(map[string]any{"summary": "test"})
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/plans/default/my-task",
			bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
}

func TestSubmitPlanAdditional(t *testing.T) {
	h, _, _ := setupTestInternalHandlers()

	t.Run("invalid json body", func(t *testing.T) {
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{Username: "system:serviceaccount:default:worker"})
			return c.Next()
		})
		app.Post("/internal/v1/plans/:namespace/:taskName", h.SubmitPlan)

		req := httptest.NewRequest(http.MethodPost, "/internal/v1/plans/default/my-task",
			bytes.NewReader([]byte("not json")))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("cross-namespace denied", func(t *testing.T) {
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{
				Username: "system:serviceaccount:other-ns:worker",
			})
			return c.Next()
		})
		app.Post("/internal/v1/plans/:namespace/:taskName", h.SubmitPlan)

		body, _ := json.Marshal(map[string]any{"summary": "test"})
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/plans/default/my-task",
			bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
}

func TestSendMessageAdditional(t *testing.T) {
	_, _, ss := setupTestInternalHandlers()

	t.Run("messaging not enabled", func(t *testing.T) {
		hNoMsg := NewInternalHandlers(ss, ss, ss, nil)
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{Username: "system:serviceaccount:default:worker"})
			return c.Next()
		})
		app.Post("/internal/v1/messages/:namespace", hNoMsg.SendMessage)

		body, _ := json.Marshal(map[string]string{
			"fromTask": "a", "toTask": "b", "parentTask": "c", "content": "hi",
		})
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/messages/default",
			bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
	})

	t.Run("missing namespace", func(t *testing.T) {
		h := NewInternalHandlers(ss, ss, ss, ss)
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{Username: "system:serviceaccount:default:worker"})
			return c.Next()
		})
		// Route without :namespace param to simulate empty namespace
		app.Post("/internal/v1/messages/", h.SendMessage)

		body, _ := json.Marshal(map[string]string{
			"fromTask": "a", "toTask": "b", "parentTask": "c", "content": "hi",
		})
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/messages/",
			bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})

	t.Run("cross-namespace denied", func(t *testing.T) {
		h := NewInternalHandlers(ss, ss, ss, ss)
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{
				Username: "system:serviceaccount:other-ns:worker",
			})
			return c.Next()
		})
		app.Post("/internal/v1/messages/:namespace", h.SendMessage)

		body, _ := json.Marshal(map[string]string{
			"fromTask": "a", "toTask": "b", "parentTask": "c", "content": "hi",
		})
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/messages/default",
			bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
	})
}

func TestGetPlanAdditional(t *testing.T) {
	h, _, _ := setupTestInternalHandlers()

	t.Run("missing taskName", func(t *testing.T) {
		app := fiber.New()
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, &UserInfo{Username: "system:serviceaccount:default:worker"})
			return c.Next()
		})
		app.Get("/internal/v1/plans/:namespace", h.GetPlan)

		req := httptest.NewRequest(http.MethodGet, "/internal/v1/plans/default", nil)
		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	})
}
