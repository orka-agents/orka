/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/sozercan/orka/internal/store"
)

func (h *InternalHandlers) ensureMemoryStore() error {
	if h.memoryStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "memory store not configured")
	}
	return nil
}

func (h *InternalHandlers) ensureMemoryProposalStore() error {
	if h.memoryProposalStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "memory proposal store not configured")
	}
	return nil
}

func internalNamespace(c fiber.Ctx) (string, error) {
	namespace := c.Params("namespace")
	if namespace == "" {
		return "", fiber.NewError(fiber.StatusBadRequest, "namespace is required")
	}
	if err := verifyCallerNamespace(c, namespace); err != nil {
		return "", err
	}
	return namespace, nil
}

// ListMemories lists memories for the namespace in the internal route.
func (h *InternalHandlers) ListMemories(c fiber.Ctx) error {
	namespace, err := internalNamespace(c)
	if err != nil {
		return err
	}
	if err := h.ensureMemoryStore(); err != nil {
		return err
	}
	filter, err := parseMemoryFilter(c, namespace)
	if err != nil {
		return err
	}
	memories, err := h.memoryStore.ListMemories(c.Context(), filter)
	if err != nil {
		return memoryStoreError("list memories", "memory", err)
	}
	return c.JSON(memories)
}

// CreateMemory creates a memory in the namespace in the internal route.
func (h *InternalHandlers) CreateMemory(c fiber.Ctx) error {
	namespace, err := internalNamespace(c)
	if err != nil {
		return err
	}
	if err := h.ensureMemoryStore(); err != nil {
		return err
	}
	var memory store.Memory
	if err := c.Bind().JSON(&memory); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if memory.Namespace != "" && memory.Namespace != namespace {
		return fiber.NewError(fiber.StatusBadRequest, "memory namespace mismatch")
	}
	memory.Namespace = namespace
	if strings.TrimSpace(memory.Content) == "" {
		return fiber.NewError(fiber.StatusBadRequest, "content is required")
	}
	if err := h.memoryStore.CreateMemory(c.Context(), &memory); err != nil {
		return memoryStoreError("create memory", "memory", err)
	}
	return c.Status(fiber.StatusCreated).JSON(memory)
}

// GetMemory gets a memory by ID from the namespace in the internal route.
func (h *InternalHandlers) GetMemory(c fiber.Ctx) error {
	namespace, err := internalNamespace(c)
	if err != nil {
		return err
	}
	if err := h.ensureMemoryStore(); err != nil {
		return err
	}
	memory, err := h.memoryStore.GetMemory(c.Context(), namespace, c.Params("id"))
	if err != nil {
		return memoryStoreError("get memory", "memory", err)
	}
	return c.JSON(memory)
}

// UpdateMemory updates a memory in the namespace in the internal route.
func (h *InternalHandlers) UpdateMemory(c fiber.Ctx) error {
	namespace, err := internalNamespace(c)
	if err != nil {
		return err
	}
	if err := h.ensureMemoryStore(); err != nil {
		return err
	}
	var req store.Memory
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if req.Namespace != "" && req.Namespace != namespace {
		return fiber.NewError(fiber.StatusBadRequest, "memory namespace mismatch")
	}
	memory, err := h.memoryStore.GetMemory(c.Context(), namespace, c.Params("id"))
	if err != nil {
		return memoryStoreError("get memory", "memory", err)
	}
	applyMemoryUpdate(memory, req)
	memory.Namespace = namespace
	memory.ID = c.Params("id")
	if strings.TrimSpace(memory.Content) == "" {
		return fiber.NewError(fiber.StatusBadRequest, "content is required")
	}
	if err := h.memoryStore.UpdateMemory(c.Context(), memory); err != nil {
		return memoryStoreError("update memory", "memory", err)
	}
	updated, err := h.memoryStore.GetMemory(c.Context(), namespace, c.Params("id"))
	if err != nil {
		return memoryStoreError("get memory", "memory", err)
	}
	return c.JSON(updated)
}

// DeleteMemory soft-deletes a memory in the namespace in the internal route.
func (h *InternalHandlers) DeleteMemory(c fiber.Ctx) error {
	namespace, err := internalNamespace(c)
	if err != nil {
		return err
	}
	if err := h.ensureMemoryStore(); err != nil {
		return err
	}
	if err := h.memoryStore.DeleteMemory(c.Context(), namespace, c.Params("id")); err != nil {
		return memoryStoreError("delete memory", "memory", err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// DisableMemory disables a memory for recall in the namespace in the internal route.
func (h *InternalHandlers) DisableMemory(c fiber.Ctx) error {
	return h.setMemoryDisabled(c, true)
}

// EnableMemory enables a memory for recall in the namespace in the internal route.
func (h *InternalHandlers) EnableMemory(c fiber.Ctx) error {
	return h.setMemoryDisabled(c, false)
}

func (h *InternalHandlers) setMemoryDisabled(c fiber.Ctx, disabled bool) error {
	namespace, err := internalNamespace(c)
	if err != nil {
		return err
	}
	if err := h.ensureMemoryStore(); err != nil {
		return err
	}
	if err := h.memoryStore.SetMemoryDisabled(c.Context(), namespace, c.Params("id"), disabled); err != nil {
		return memoryStoreError("update memory", "memory", err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// ListMemoryProposals lists memory proposals for the namespace in the internal route.
func (h *InternalHandlers) ListMemoryProposals(c fiber.Ctx) error {
	namespace, err := internalNamespace(c)
	if err != nil {
		return err
	}
	if err := h.ensureMemoryProposalStore(); err != nil {
		return err
	}
	filter, err := parseMemoryProposalFilter(c, namespace)
	if err != nil {
		return err
	}
	proposals, err := h.memoryProposalStore.ListMemoryProposals(c.Context(), filter)
	if err != nil {
		return memoryStoreError("list memory proposals", "memory proposal", err)
	}
	return c.JSON(proposals)
}

// CreateMemoryProposal creates a memory governance proposal in the namespace in the internal route.
func (h *InternalHandlers) CreateMemoryProposal(c fiber.Ctx) error {
	namespace, err := internalNamespace(c)
	if err != nil {
		return err
	}
	if err := h.ensureMemoryProposalStore(); err != nil {
		return err
	}
	var proposal store.MemoryProposal
	if err := c.Bind().JSON(&proposal); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if proposal.Namespace != "" && proposal.Namespace != namespace {
		return fiber.NewError(fiber.StatusBadRequest, "memory proposal namespace mismatch")
	}
	proposal.Namespace = namespace
	if strings.TrimSpace(proposal.Title) == "" {
		return fiber.NewError(fiber.StatusBadRequest, "title is required")
	}
	if err := h.memoryProposalStore.CreateMemoryProposal(c.Context(), &proposal); err != nil {
		return memoryStoreError("create memory proposal", "memory proposal", err)
	}
	return c.Status(fiber.StatusCreated).JSON(proposal)
}

// GetMemoryProposal gets a memory proposal by ID from the namespace in the internal route.
func (h *InternalHandlers) GetMemoryProposal(c fiber.Ctx) error {
	namespace, err := internalNamespace(c)
	if err != nil {
		return err
	}
	if err := h.ensureMemoryProposalStore(); err != nil {
		return err
	}
	proposal, err := h.memoryProposalStore.GetMemoryProposal(c.Context(), namespace, c.Params("id"))
	if err != nil {
		return memoryStoreError("get memory proposal", "memory proposal", err)
	}
	return c.JSON(proposal)
}

// ReviewMemoryProposal records a review decision without applying the proposal automatically.
func (h *InternalHandlers) ReviewMemoryProposal(c fiber.Ctx) error {
	namespace, err := internalNamespace(c)
	if err != nil {
		return err
	}
	if err := h.ensureMemoryProposalStore(); err != nil {
		return err
	}
	review, err := bindMemoryProposalReview(c, namespace, c.Params("id"))
	if err != nil {
		return err
	}
	if review.Namespace != namespace {
		return fiber.NewError(fiber.StatusBadRequest, "memory proposal namespace mismatch")
	}
	if review.Reviewer == "" {
		if ui := GetUserInfo(c); ui != nil {
			review.Reviewer = ui.Username
		}
	}
	if err := h.memoryProposalStore.ReviewMemoryProposal(c.Context(), review); err != nil {
		return memoryStoreError("review memory proposal", "memory proposal", err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// ArchiveMemoryProposal archives a proposal in the namespace in the internal route without applying it.
func (h *InternalHandlers) ArchiveMemoryProposal(c fiber.Ctx) error {
	namespace, err := internalNamespace(c)
	if err != nil {
		return err
	}
	if err := h.ensureMemoryProposalStore(); err != nil {
		return err
	}
	if err := h.memoryProposalStore.ArchiveMemoryProposal(c.Context(), namespace, c.Params("id")); err != nil {
		return memoryStoreError("archive memory proposal", "memory proposal", err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}
