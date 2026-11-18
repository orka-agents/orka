/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
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
	if c.Query(tools.MemoryListPurposeParam, "") != tools.MemoryListPurposeRecall || len(memories) == 0 {
		return c.JSON(memories)
	}
	encoded, err := json.Marshal(memories)
	if err != nil {
		return err
	}
	// An agent's recall_memory call counts the memories it returned; a
	// prompt's memory context and operator listings do not. A response larger
	// than the tool reads is not counted, since the agent never gets all of
	// it. Any ServiceAccount in the namespace can list here, so only the
	// verified worker of an active Task can mark a listing as a recall. The
	// check reads Kubernetes, so it runs with the count, after the response,
	// on a copy of the caller.
	if caller := GetUserInfo(c); caller != nil && len(encoded) <= tools.MemoryRecallResultLimit {
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
	c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	return c.Send(encoded)
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
	if err := h.refuseInternalConsolidationProposal(c, namespace, review.ID); err != nil {
		return err
	}
	return reviewNamespaceMemoryProposal(c, h.memoryProposalStore, review)
}

// refuseInternalConsolidationProposal keeps consolidation proposals off the
// internal review and apply routes. Those routes check only the namespace,
// and a consolidation proposal disables or deletes the memories it targets,
// so it is reviewed and applied through the API, which authorizes the person
// doing it against each target.
func (h *InternalHandlers) refuseInternalConsolidationProposal(c fiber.Ctx, namespace, id string) error {
	// The store trims the ID it reviews or applies, so the check reads the
	// same proposal.
	proposal, err := h.memoryProposalStore.GetMemoryProposal(c.Context(), strings.TrimSpace(namespace), strings.TrimSpace(id))
	if err != nil {
		return memoryStoreError("get memory proposal", "memory proposal", err)
	}
	if store.IsMemoryConsolidationProposalType(strings.ToLower(strings.TrimSpace(proposal.Type))) {
		return fiber.NewError(fiber.StatusForbidden, "consolidation proposals are reviewed and applied through the API")
	}
	return nil
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
	if err := h.refuseInternalConsolidationProposal(c, namespace, apply.ID); err != nil {
		return err
	}
	return applyNamespaceMemoryProposal(c, h.memoryProposalStore, apply)
}
