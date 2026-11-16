/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
)

func (h *InternalHandlers) internalNamespace(c fiber.Ctx) (string, error) {
	namespace := c.Params("namespace")
	if namespace == "" {
		return "", fiber.NewError(fiber.StatusBadRequest, "namespace is required")
	}
	if err := h.internalCallerAuthorizer().verifyNamespace(c, namespace); err != nil {
		return "", err
	}
	return namespace, nil
}

func (h *InternalHandlers) internalMemoryNamespace(c fiber.Ctx) (string, error) {
	namespace, err := h.internalNamespace(c)
	if err != nil {
		return "", err
	}
	if err := requireMemoryStore(h.memoryStore); err != nil {
		return "", err
	}
	return namespace, nil
}

func (h *InternalHandlers) internalMemoryProposalNamespace(c fiber.Ctx) (string, error) {
	namespace, err := h.internalNamespace(c)
	if err != nil {
		return "", err
	}
	if err := requireMemoryProposalStore(h.memoryProposalStore); err != nil {
		return "", err
	}
	return namespace, nil
}

// ListMemories lists memories for the namespace in the internal route.
func (h *InternalHandlers) ListMemories(c fiber.Ctx) error {
	namespace, err := h.internalMemoryNamespace(c)
	if err != nil {
		return err
	}
	memories, err := listNamespaceMemories(c, h.memoryStore, namespace)
	if err != nil {
		return err
	}
	// An agent's recall_memory call counts the memories it returned; a
	// prompt's memory context and operator listings do not. Any ServiceAccount
	// in the namespace can list here, so only the verified worker of an active
	// Task can mark a listing as a recall. The check reads Kubernetes, so it
	// runs with the count, after the response, on a copy of the caller.
	if c.Query(tools.MemoryListPurposeParam, "") == tools.MemoryListPurposeRecall && len(memories) > 0 {
		if caller := GetUserInfo(c); caller != nil {
			// Route parameters can share the request's buffers, which are
			// reused once the handler returns, so the count keeps a copy.
			namespace := strings.Clone(namespace)
			callerCopy := *caller
			authorizer := h.internalCallerAuthorizer()
			tools.RecordAuthorizedMemoryRecall(c.Context(), func(ctx context.Context) bool {
				task, err := authorizer.resolveTaskWorker(ctx, &callerCopy, namespace)
				return err == nil && activeInternalWorkerTask(task)
			}, h.memoryStore, namespace, memories)
		}
	}
	return c.JSON(memories)
}

// CreateMemory creates a memory in the namespace in the internal route.
func (h *InternalHandlers) CreateMemory(c fiber.Ctx) error {
	namespace, err := h.internalMemoryNamespace(c)
	if err != nil {
		return err
	}
	var memory store.Memory
	if err := c.Bind().JSON(&memory); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if memory.Namespace != "" && memory.Namespace != namespace {
		return fiber.NewError(fiber.StatusBadRequest, "memory namespace mismatch")
	}
	return createNamespaceMemory(c, h.memoryStore, namespace, memory)
}

// GetMemory gets a memory by ID from the namespace in the internal route.
func (h *InternalHandlers) GetMemory(c fiber.Ctx) error {
	namespace, err := h.internalMemoryNamespace(c)
	if err != nil {
		return err
	}
	return getNamespaceMemory(c, h.memoryStore, namespace)
}

// UpdateMemory updates a memory in the namespace in the internal route.
func (h *InternalHandlers) UpdateMemory(c fiber.Ctx) error {
	namespace, err := h.internalMemoryNamespace(c)
	if err != nil {
		return err
	}
	var req store.Memory
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if req.Namespace != "" && req.Namespace != namespace {
		return fiber.NewError(fiber.StatusBadRequest, "memory namespace mismatch")
	}
	return updateNamespaceMemory(c, h.memoryStore, namespace, req)
}

// DeleteMemory soft-deletes a memory in the namespace in the internal route.
func (h *InternalHandlers) DeleteMemory(c fiber.Ctx) error {
	namespace, err := h.internalMemoryNamespace(c)
	if err != nil {
		return err
	}
	return deleteNamespaceMemory(c, h.memoryStore, namespace)
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
	namespace, err := h.internalMemoryNamespace(c)
	if err != nil {
		return err
	}
	return setNamespaceMemoryDisabled(c, h.memoryStore, namespace, disabled)
}

// ListMemoryProposals lists memory proposals for the namespace in the internal route.
func (h *InternalHandlers) ListMemoryProposals(c fiber.Ctx) error {
	namespace, err := h.internalMemoryProposalNamespace(c)
	if err != nil {
		return err
	}
	proposals, err := listNamespaceMemoryProposals(c, h.memoryProposalStore, namespace)
	if err != nil {
		return err
	}
	return c.JSON(proposals)
}

// CreateMemoryProposal creates a memory governance proposal in the namespace in the internal route.
func (h *InternalHandlers) CreateMemoryProposal(c fiber.Ctx) error {
	namespace, err := h.internalMemoryProposalNamespace(c)
	if err != nil {
		return err
	}
	var proposal store.MemoryProposal
	if err := c.Bind().JSON(&proposal); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if proposal.Namespace != "" && proposal.Namespace != namespace {
		return fiber.NewError(fiber.StatusBadRequest, "memory proposal namespace mismatch")
	}
	return createNamespaceMemoryProposal(c, h.memoryProposalStore, namespace, proposal)
}

// GetMemoryProposal gets a memory proposal by ID from the namespace in the internal route.
func (h *InternalHandlers) GetMemoryProposal(c fiber.Ctx) error {
	namespace, err := h.internalMemoryProposalNamespace(c)
	if err != nil {
		return err
	}
	return getNamespaceMemoryProposal(c, h.memoryProposalStore, namespace)
}

// ReviewMemoryProposal records a review decision without applying the proposal automatically.
func (h *InternalHandlers) ReviewMemoryProposal(c fiber.Ctx) error {
	namespace, err := h.internalMemoryProposalNamespace(c)
	if err != nil {
		return err
	}
	review, err := bindMemoryProposalReview(c, namespace, c.Params("id"))
	if err != nil {
		return err
	}
	if review.Namespace != namespace {
		return fiber.NewError(fiber.StatusBadRequest, "memory proposal namespace mismatch")
	}
	return reviewNamespaceMemoryProposal(c, h.memoryProposalStore, review)
}

// ArchiveMemoryProposal archives a proposal in the namespace in the internal route without applying it.
func (h *InternalHandlers) ArchiveMemoryProposal(c fiber.Ctx) error {
	namespace, err := h.internalMemoryProposalNamespace(c)
	if err != nil {
		return err
	}
	return archiveNamespaceMemoryProposal(c, h.memoryProposalStore, namespace)
}

// ApplyMemoryProposal applies an accepted memory proposal into durable memory in the namespace in the internal route.
func (h *InternalHandlers) ApplyMemoryProposal(c fiber.Ctx) error {
	namespace, err := h.internalMemoryProposalNamespace(c)
	if err != nil {
		return err
	}
	apply, err := bindMemoryProposalApply(c, namespace, c.Params("id"))
	if err != nil {
		return err
	}
	if apply.Namespace != namespace {
		return fiber.NewError(fiber.StatusBadRequest, "memory proposal namespace mismatch")
	}
	return applyNamespaceMemoryProposal(c, h.memoryProposalStore, apply)
}
