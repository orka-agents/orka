/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/sozercan/mercan/internal/store"
)

const maxResultSize = 10 << 20 // 10MB

// InternalHandlers contains handlers for internal worker endpoints.
type InternalHandlers struct {
	resultStore  store.ResultStore
	sessionStore store.SessionStore
}

// NewInternalHandlers creates a new InternalHandlers instance.
func NewInternalHandlers(rs store.ResultStore, ss store.SessionStore) *InternalHandlers {
	return &InternalHandlers{
		resultStore:  rs,
		sessionStore: ss,
	}
}

// SubmitResult handles POST /internal/v1/results/{namespace}/{taskName}.
// Workers call this to persist task results.
func (h *InternalHandlers) SubmitResult(c fiber.Ctx) error {
	namespace := c.Params("namespace")
	taskName := c.Params("taskName")

	if namespace == "" || taskName == "" {
		return fiber.NewError(fiber.StatusBadRequest, "namespace and taskName are required")
	}

	// Verify caller namespace matches the URL namespace
	if err := verifyCallerNamespace(c, namespace); err != nil {
		return err
	}

	// Read body with size limit
	body := c.Request().BodyStream()
	if body == nil {
		// Fiber may buffer the body; fall back to c.Body()
		data := c.Body()
		if len(data) == 0 {
			return fiber.NewError(fiber.StatusBadRequest, "empty request body")
		}
		if len(data) > maxResultSize {
			return fiber.NewError(fiber.StatusRequestEntityTooLarge, "result exceeds 10MB limit")
		}
		ctx := c.Context()
		if err := h.resultStore.SaveResult(ctx, namespace, taskName, data); err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to save result: %v", err))
		}
		return c.SendStatus(fiber.StatusNoContent)
	}

	lr := io.LimitReader(body, int64(maxResultSize)+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to read request body: %v", err))
	}
	if len(data) > maxResultSize {
		return fiber.NewError(fiber.StatusRequestEntityTooLarge, "result exceeds 10MB limit")
	}
	if len(data) == 0 {
		return fiber.NewError(fiber.StatusBadRequest, "empty request body")
	}

	ctx := c.Context()
	if err := h.resultStore.SaveResult(ctx, namespace, taskName, data); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to save result: %v", err))
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// GetSessionTranscript handles GET /internal/v1/sessions/{namespace}/{name}/transcript.
// Returns the session transcript as JSONL (one JSON object per line).
func (h *InternalHandlers) GetSessionTranscript(c fiber.Ctx) error {
	namespace := c.Params("namespace")
	name := c.Params("name")

	if namespace == "" || name == "" {
		return fiber.NewError(fiber.StatusBadRequest, "namespace and name are required")
	}

	ctx := c.Context()
	messages, err := h.sessionStore.LoadTranscript(ctx, namespace, name, 0)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusNotFound, "session not found")
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to load transcript: %v", err))
	}

	c.Set("Content-Type", "application/x-ndjson")

	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	for _, msg := range messages {
		if err := enc.Encode(msg); err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to encode message: %v", err))
		}
	}

	return c.SendString(sb.String())
}

// verifyCallerNamespace checks that the authenticated caller's SA namespace
// matches the target namespace in the URL path.
func verifyCallerNamespace(c fiber.Ctx, namespace string) error {
	userInfo := GetUserInfo(c)
	if userInfo == nil {
		return fiber.NewError(fiber.StatusUnauthorized, "authentication required")
	}

	// SA usernames follow the format: system:serviceaccount:<namespace>:<name>
	parts := strings.Split(userInfo.Username, ":")
	if len(parts) == 4 && parts[0] == "system" && parts[1] == "serviceaccount" {
		if parts[2] != namespace {
			return fiber.NewError(fiber.StatusForbidden, "cross-namespace access denied")
		}
	}

	return nil
}
