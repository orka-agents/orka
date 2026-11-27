/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"errors"

	"github.com/gofiber/fiber/v3"

	"github.com/orka-agents/orka/internal/store"
)

func (h *Handlers) GetTaskMemoryConsolidationReport(c fiber.Ctx) error {
	id := c.Params("id")
	namespace, err := h.resolveNamespace(c, c.Query(toolNamespaceArg, ""))
	if err != nil {
		return err
	}
	for _, scopes := range [][]string{h.contextTokenAuthorization.SessionReadScopes, h.contextTokenAuthorization.MemoryReadScopes} {
		if err := h.authorizeContextTokenAction(c, "getMemoryConsolidationReport", scopes); err != nil {
			return err
		}
	}
	task, err := h.taskAccess().loadReadable(c, "getTaskResult", namespace, id)
	if err != nil {
		return err
	}
	if !taskMatchesExpectedUID(c, task) {
		return fiber.NewError(fiber.StatusNotFound, "task not found")
	}
	if task.Spec.AI == nil || task.Spec.AI.MemoryConsolidation == nil {
		return fiber.NewError(fiber.StatusNotFound, "task is not a memory consolidation run")
	}
	if h.memoryConsolidationStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "memory consolidation is not configured")
	}
	run, err := h.memoryConsolidationStore.GetMemoryConsolidationRun(c.Context(), namespace, string(task.UID))
	if errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound, "memory consolidation run has no report")
	}
	if err != nil {
		return memoryStoreError("read memory consolidation report", "memory consolidation run", err)
	}
	if len(run.Report) == 0 {
		return fiber.NewError(fiber.StatusNotFound, "memory consolidation run has no report")
	}
	c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	return c.Send(run.Report)
}
