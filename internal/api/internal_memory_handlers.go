/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	memoryruntime "github.com/orka-agents/orka/internal/memory"
)

func (h *InternalHandlers) ensureMemoryStore() error {
	if h.memoryService == nil {
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

func (h *InternalHandlers) internalNamespace(c fiber.Ctx) (string, error) {
	namespace := c.Params("namespace")
	if namespace == "" {
		return "", fiber.NewError(fiber.StatusBadRequest, "namespace is required")
	}
	if err := h.internalCallerAuthorizer().verifyMemoryNamespace(c, namespace); err != nil {
		return "", err
	}
	return namespace, nil
}

// ListMemories lists memories for the namespace in the internal route.
func (h *InternalHandlers) ListMemories(c fiber.Ctx) error {
	namespace, err := h.internalNamespace(c)
	if err != nil {
		return err
	}
	if err := h.ensureMemoryStore(); err != nil {
		return err
	}
	if err := h.authorizeInternalMemoryTask(c, namespace, "listMemories", h.memoryReadScopes()); err != nil {
		return err
	}
	filter, err := parseMemoryFilter(c, namespace)
	if err != nil {
		return err
	}
	if filter.IncludeDisabled {
		if err := h.authorizeInternalMemoryTask(c, namespace, "includeDisabledMemories",
			h.memoryReadScopes(), h.memoryOperateScopes()); err != nil {
			return err
		}
	}
	actor, _ := memoryActor(c)
	memories, err := h.memoryService.ListMemoriesWithSearchContext(c.Context(), filter, memoryruntime.SearchContext{
		Actor: actor, RequestID: requestid.FromContext(c),
		AuthorizeRemote: func() error {
			return h.authorizeInternalMemoryTask(c, namespace, "searchRemoteMemories",
				h.memoryReadScopes(), h.memorySearchRemoteScopes())
		},
	})
	if err != nil {
		return memoryServiceError(err)
	}
	if c.Query("recordRecall", "") == queryTrue {
		ids := make([]string, 0, len(memories))
		for _, memory := range memories {
			ids = append(ids, memory.ID)
		}
		if err := h.memoryService.MarkMemoriesRecalled(c.Context(), namespace, ids); err != nil {
			return memoryServiceError(err)
		}
	}
	return c.JSON(memories)
}

// CreateMemory creates a memory in the namespace in the internal route.
func (h *InternalHandlers) CreateMemory(c fiber.Ctx) error {
	namespace, err := h.internalNamespace(c)
	if err != nil {
		return err
	}
	if err := h.ensureMemoryStore(); err != nil {
		return err
	}
	if err := h.authorizeInternalMemoryTask(c, namespace, "createMemory", h.memoryWriteScopes()); err != nil {
		return err
	}
	var request memoryruntime.CreateRequest
	if err := bindStrictMemoryJSON(c, &request); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if request.Namespace != "" && request.Namespace != namespace {
		return fiber.NewError(fiber.StatusBadRequest, "memory namespace mismatch")
	}
	result, err := h.memoryService.CreateMemory(c.Context(), namespace, request,
		internalMemoryMutationContext(c, namespace, "internalCreateMemory", "memory created by worker"))
	if err != nil {
		return memoryServiceError(err)
	}
	return sendInternalMemoryMutationResult(c, namespace, result)
}

// GetMemory gets a memory by ID from the namespace in the internal route.
func (h *InternalHandlers) GetMemory(c fiber.Ctx) error {
	namespace, err := h.internalNamespace(c)
	if err != nil {
		return err
	}
	if err := h.ensureMemoryStore(); err != nil {
		return err
	}
	if err := h.authorizeInternalMemoryTask(c, namespace, "getMemory", h.memoryReadScopes()); err != nil {
		return err
	}
	includeDisabled := c.Query("includeDisabled", "") == queryTrue
	if includeDisabled {
		if err := h.authorizeInternalMemoryTask(c, namespace, "getMemoryIncludeDisabled",
			h.memoryReadScopes(), h.memoryOperateScopes()); err != nil {
			return err
		}
	}
	memory, err := h.memoryService.GetMemoryWithVisibility(c.Context(), namespace, c.Params("id"), includeDisabled)
	if err != nil {
		return memoryServiceError(err)
	}
	return c.JSON(memory)
}

// UpdateMemory updates a memory in the namespace in the internal route.
func (h *InternalHandlers) UpdateMemory(c fiber.Ctx) error {
	namespace, err := h.internalNamespace(c)
	if err != nil {
		return err
	}
	if err := h.ensureMemoryStore(); err != nil {
		return err
	}
	if err := h.authorizeInternalMemoryTask(c, namespace, "updateMemory", h.memoryWriteScopes()); err != nil {
		return err
	}
	var request memoryruntime.UpdateRequest
	if err := bindStrictMemoryJSON(c, &request); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if request.Namespace != "" && request.Namespace != namespace {
		return fiber.NewError(fiber.StatusBadRequest, "memory namespace mismatch")
	}
	result, err := h.memoryService.UpdateMemory(c.Context(), namespace, c.Params("id"), request,
		internalMemoryMutationContext(c, namespace, "internalUpdateMemory", "memory updated by worker"))
	if err != nil {
		return memoryServiceError(err)
	}
	return sendInternalMemoryMutationResult(c, namespace, result)
}

// DeleteMemory installs a local tombstone and queues remote deletion when needed.
func (h *InternalHandlers) DeleteMemory(c fiber.Ctx) error {
	namespace, err := h.internalNamespace(c)
	if err != nil {
		return err
	}
	if err := h.ensureMemoryStore(); err != nil {
		return err
	}
	if err := h.authorizeInternalMemoryTask(c, namespace, "deleteMemory", h.memoryWriteScopes()); err != nil {
		return err
	}
	result, err := h.memoryService.DeleteMemory(c.Context(), namespace, c.Params("id"),
		internalMemoryMutationContext(c, namespace, "internalDeleteMemory", "memory deleted by worker"))
	if err != nil {
		return memoryServiceError(err)
	}
	return sendInternalMemoryMutationResult(c, namespace, result)
}

// DisableMemory disables a memory for recall in the namespace in the internal route.
func (h *InternalHandlers) DisableMemory(c fiber.Ctx) error { return h.setMemoryDisabled(c, true) }

// EnableMemory enables a memory for recall in the namespace in the internal route.
func (h *InternalHandlers) EnableMemory(c fiber.Ctx) error { return h.setMemoryDisabled(c, false) }

func (h *InternalHandlers) setMemoryDisabled(c fiber.Ctx, disabled bool) error {
	namespace, err := h.internalNamespace(c)
	if err != nil {
		return err
	}
	if err := h.ensureMemoryStore(); err != nil {
		return err
	}
	if err := h.authorizeInternalMemoryTask(c, namespace, "setMemoryDisabled", h.memoryWriteScopes()); err != nil {
		return err
	}
	actor, _ := memoryActor(c)
	if err := h.memoryService.SetMemoryDisabled(c.Context(), namespace, c.Params("id"), disabled, actor, requestid.FromContext(c)); err != nil {
		return memoryServiceError(err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// ListMemoryProposals lists memory proposals for the namespace in the internal route.
func (h *InternalHandlers) ListMemoryProposals(c fiber.Ctx) error {
	namespace, err := h.internalNamespace(c)
	if err != nil {
		return err
	}
	if err := h.ensureMemoryProposalStore(); err != nil {
		return err
	}
	if err := h.authorizeInternalMemoryTask(c, namespace, "listMemoryProposals", h.memoryReadScopes()); err != nil {
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
	namespace, err := h.internalNamespace(c)
	if err != nil {
		return err
	}
	if err := h.ensureMemoryProposalStore(); err != nil {
		return err
	}
	if err := h.authorizeInternalMemoryTask(c, namespace, "createMemoryProposal", h.memoryWriteScopes()); err != nil {
		return err
	}
	var request memoryProposalCreateRequest
	if err := bindStrictMemoryJSON(c, &request); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if request.Namespace != "" && request.Namespace != namespace {
		return fiber.NewError(fiber.StatusBadRequest, "memory proposal namespace mismatch")
	}
	proposal := request.toStore(namespace)
	if strings.TrimSpace(proposal.Title) == "" {
		return fiber.NewError(fiber.StatusBadRequest, "title is required")
	}
	if err := h.memoryProposalStore.CreateMemoryProposal(c.Context(), proposal); err != nil {
		return memoryStoreError("create memory proposal", "memory proposal", err)
	}
	return c.Status(fiber.StatusCreated).JSON(proposal)
}

// GetMemoryProposal gets a memory proposal by ID from the namespace in the internal route.
func (h *InternalHandlers) GetMemoryProposal(c fiber.Ctx) error {
	namespace, err := h.internalNamespace(c)
	if err != nil {
		return err
	}
	if err := h.ensureMemoryProposalStore(); err != nil {
		return err
	}
	if err := h.authorizeInternalMemoryTask(c, namespace, "getMemoryProposal", h.memoryReadScopes()); err != nil {
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
	namespace, err := h.internalNamespace(c)
	if err != nil {
		return err
	}
	if err := h.ensureMemoryProposalStore(); err != nil {
		return err
	}
	if err := h.authorizeInternalMemoryTask(c, namespace, "reviewMemoryProposal", h.memoryOperateScopes()); err != nil {
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
		if actor, _ := memoryActor(c); actor != "" {
			review.Reviewer = actor
		}
	}
	if err := h.memoryProposalStore.ReviewMemoryProposal(c.Context(), review); err != nil {
		return memoryStoreError("review memory proposal", "memory proposal", err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// ArchiveMemoryProposal archives a proposal in the namespace in the internal route without applying it.
func (h *InternalHandlers) ArchiveMemoryProposal(c fiber.Ctx) error {
	namespace, err := h.internalNamespace(c)
	if err != nil {
		return err
	}
	if err := h.ensureMemoryProposalStore(); err != nil {
		return err
	}
	if err := h.authorizeInternalMemoryTask(c, namespace, "archiveMemoryProposal", h.memoryWriteScopes()); err != nil {
		return err
	}
	if err := h.memoryProposalStore.ArchiveMemoryProposal(c.Context(), namespace, c.Params("id")); err != nil {
		return memoryStoreError("archive memory proposal", "memory proposal", err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// ApplyMemoryProposal applies an accepted memory proposal into durable memory in the namespace in the internal route.
func (h *InternalHandlers) ApplyMemoryProposal(c fiber.Ctx) error {
	namespace, err := h.internalNamespace(c)
	if err != nil {
		return err
	}
	if err := h.ensureMemoryProposalStore(); err != nil {
		return err
	}
	if err := h.authorizeInternalMemoryTask(c, namespace, "applyMemoryProposal",
		h.memoryWriteScopes(), h.memoryOperateScopes()); err != nil {
		return err
	}
	apply, err := bindMemoryProposalApply(c, namespace, c.Params("id"))
	if err != nil {
		return err
	}
	if apply.Namespace != namespace {
		return fiber.NewError(fiber.StatusBadRequest, "memory proposal namespace mismatch")
	}
	actor, _ := memoryActor(c)
	if apply.AppliedBy == "" {
		apply.AppliedBy = actor
	}
	result, err := h.memoryService.ApplyMemoryProposal(c.Context(), namespace, apply.ID, apply.AppliedBy,
		internalMemoryMutationContext(c, namespace, "internalApplyMemoryProposal", "memory proposal applied by worker"))
	if err != nil {
		return memoryServiceError(err)
	}
	return sendInternalMemoryMutationResult(c, namespace, result)
}

func (h *InternalHandlers) authorizeInternalMemoryTask(
	c fiber.Ctx,
	namespace, action string,
	requiredScopeGroups ...[]string,
) error {
	if !h.contextTokenAuthorization.Enabled() {
		return nil
	}
	authorizationFailure := func(token *ContextToken, message string) error {
		if token == nil {
			token = &ContextToken{}
		}
		return handleContextTokenAuthorizationFailures(
			h.contextTokenAuthorization,
			token,
			action,
			[]string{message},
		)
	}
	userInfo := GetUserInfo(c)
	if userInfo == nil || userInfo.ContextToken == nil {
		return authorizationFailure(nil, "task-scoped Txn-Token is required")
	}
	token := userInfo.ContextToken
	for _, required := range requiredScopeGroups {
		if !hasAnyScope(token.Scopes, required) {
			return authorizationFailure(token, "missing one of required scopes "+strings.Join(required, ","))
		}
	}
	tokenNamespace, ok := contextString(token.TransactionContext, "namespace")
	if !ok || tokenNamespace != namespace {
		return authorizationFailure(token, "namespace does not match the task-scoped Txn-Token")
	}
	taskName, ok := contextString(token.TransactionContext, "taskName")
	if !ok {
		if taskRef, hasTask := contextString(token.TransactionContext, "task"); hasTask {
			if after, ok0 := strings.CutPrefix(taskRef, namespace+"/"); ok0 {
				taskName = after
			} else if !strings.Contains(taskRef, "/") {
				taskName = taskRef
			}
		}
	}
	if strings.TrimSpace(taskName) == "" {
		return authorizationFailure(token, "task name is missing from the Txn-Token context")
	}
	var task *corev1alpha1.Task
	if current, ok := c.Locals(internalMemoryTaskLocalKey).(*corev1alpha1.Task); ok {
		task = current
	} else {
		reader := h.apiReader
		if reader == nil {
			reader = h.k8sClient
		}
		if reader == nil {
			return authorizationFailure(token, "task identity could not be verified")
		}
		task = &corev1alpha1.Task{}
		if err := reader.Get(c.Context(), types.NamespacedName{Namespace: namespace, Name: taskName}, task); err != nil {
			return authorizationFailure(token, "task identity could not be verified")
		}
	}
	if task.Namespace != namespace || task.Name != taskName || !task.DeletionTimestamp.IsZero() ||
		isTerminalInternalTaskPhase(task.Status.Phase) {
		return authorizationFailure(token, "task identity is not active")
	}
	tokenTaskUID, ok := contextString(token.TransactionContext, "taskUID")
	if !ok || tokenTaskUID != string(task.UID) {
		return authorizationFailure(token, "task UID does not match the active task")
	}
	return nil
}

func (h *InternalHandlers) memoryReadScopes() []string {
	if len(h.contextTokenAuthorization.MemoryReadScopes) > 0 {
		return h.contextTokenAuthorization.MemoryReadScopes
	}
	return []string{ContextTokenScopeMemoryRead}
}

func (h *InternalHandlers) memoryWriteScopes() []string {
	if len(h.contextTokenAuthorization.MemoryWriteScopes) > 0 {
		return h.contextTokenAuthorization.MemoryWriteScopes
	}
	return []string{ContextTokenScopeMemoryWrite}
}

func (h *InternalHandlers) memoryOperateScopes() []string {
	if len(h.contextTokenAuthorization.MemoryOperateScopes) > 0 {
		return h.contextTokenAuthorization.MemoryOperateScopes
	}
	return []string{ContextTokenScopeMemoryOperate}
}

func (h *InternalHandlers) memorySearchRemoteScopes() []string {
	if len(h.contextTokenAuthorization.MemorySearchRemoteScopes) > 0 {
		return h.contextTokenAuthorization.MemorySearchRemoteScopes
	}
	return []string{ContextTokenScopeMemorySearchRemote}
}

func sendInternalMemoryMutationResult(c fiber.Ctx, namespace string, result *memoryruntime.MutationResult) error {
	if result == nil {
		return fiber.NewError(fiber.StatusInternalServerError, "memory service returned no result")
	}
	if result.Operation != nil {
		result.Location = "/internal/v1/memory-operations/" + namespace + "/" + result.Operation.ID
	}
	if result.Location != "" {
		c.Set("Location", result.Location)
	}
	if result.RetryAfter > 0 {
		seconds := max(1, int(result.RetryAfter.Round(time.Second)/time.Second))
		c.Set("Retry-After", strconv.Itoa(seconds))
	}
	if result.StatusCode == fiber.StatusNoContent {
		return c.SendStatus(fiber.StatusNoContent)
	}
	if result.StatusCode == fiber.StatusAccepted {
		return c.Status(fiber.StatusAccepted).JSON(result.Operation)
	}
	return c.Status(result.StatusCode).JSON(result.Memory)
}
