/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"

	"github.com/orka-agents/orka/internal/apierror"
	memoryruntime "github.com/orka-agents/orka/internal/memory"
	"github.com/orka-agents/orka/internal/store"
)

func (h *Handlers) ensureMemoryStore() error {
	if h.memoryService == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "memory store not configured")
	}
	return nil
}

func (h *Handlers) ensureMemoryProposalStore() error {
	if h.memoryProposalStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "memory proposal store not configured")
	}
	return nil
}

// ListMemories lists namespace-scoped memories.
func (h *Handlers) ListMemories(c fiber.Ctx) error {
	if err := h.ensureMemoryStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	filter, err := parseMemoryFilter(c, namespace)
	if err != nil {
		return err
	}
	if err := h.authorizeMemoryReadVisibility(c, "listMemories", filter.IncludeDisabled); err != nil {
		return err
	}
	page, err := h.memoryService.ListMemoriesPageWithSearchContext(c.Context(), filter, h.memorySearchContext(c))
	if err != nil {
		return memoryServiceError(err)
	}
	if !page.Paginated {
		return c.JSON(ListResponse{Items: page.Items, Metadata: ListMeta{}})
	}
	return c.JSON(memoryListResponse{
		Items: page.Items,
		Metadata: memoryListMetadata{
			Continue: page.Cursor, Exhausted: page.Exhausted, Complete: page.Complete,
		},
	})
}

type memoryListResponse struct {
	Items    []store.Memory     `json:"items"`
	Metadata memoryListMetadata `json:"metadata"`
}

type memoryListMetadata struct {
	Continue  string `json:"continue,omitempty"`
	Exhausted bool   `json:"exhausted"`
	Complete  bool   `json:"complete"`
}

func (h *Handlers) CreateMemory(c fiber.Ctx) error {
	if err := h.ensureMemoryStore(); err != nil {
		return err
	}
	var request memoryruntime.CreateRequest
	if err := bindStrictMemoryJSON(c, &request); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	namespace, err := h.resolveNamespace(c, request.Namespace)
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "createMemory", h.contextTokenAuthorization.MemoryWriteScopes); err != nil {
		return err
	}
	result, err := h.memoryService.CreateMemory(c.Context(), namespace, request, memoryMutationContext(c, "createMemory", "memory created"))
	if err != nil {
		return memoryServiceError(err)
	}
	return sendMemoryMutationResult(c, result)
}

func (h *Handlers) GetMemory(c fiber.Ctx) error {
	if err := h.ensureMemoryStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	includeDisabled := c.Query("includeDisabled", "") == queryTrue
	if err := h.authorizeMemoryReadVisibility(c, "getMemory", includeDisabled); err != nil {
		return err
	}
	memory, err := h.memoryService.GetMemoryWithVisibility(c.Context(), namespace, c.Params("id"), includeDisabled)
	if err != nil {
		return memoryServiceError(err)
	}
	return c.JSON(memory)
}

func (h *Handlers) UpdateMemory(c fiber.Ctx) error {
	if err := h.ensureMemoryStore(); err != nil {
		return err
	}
	var request memoryruntime.UpdateRequest
	if err := bindStrictMemoryJSON(c, &request); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	explicitNamespace := c.Query("namespace", "")
	if explicitNamespace != "" && request.Namespace != "" && request.Namespace != explicitNamespace {
		return fiber.NewError(fiber.StatusBadRequest, "memory namespace mismatch")
	}
	if explicitNamespace == "" {
		explicitNamespace = request.Namespace
	}
	namespace, err := h.resolveNamespace(c, explicitNamespace)
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "updateMemory", h.contextTokenAuthorization.MemoryWriteScopes); err != nil {
		return err
	}
	result, err := h.memoryService.UpdateMemory(c.Context(), namespace, c.Params("id"), request,
		memoryMutationContext(c, "updateMemory", "memory updated"))
	if err != nil {
		return memoryServiceError(err)
	}
	return sendMemoryMutationResult(c, result)
}

func (h *Handlers) DeleteMemory(c fiber.Ctx) error {
	if err := h.ensureMemoryStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "deleteMemory", h.contextTokenAuthorization.MemoryWriteScopes); err != nil {
		return err
	}
	result, err := h.memoryService.DeleteMemory(c.Context(), namespace, c.Params("id"),
		memoryMutationContext(c, "deleteMemory", "memory deleted"))
	if err != nil {
		return memoryServiceError(err)
	}
	return sendMemoryMutationResult(c, result)
}

func (h *Handlers) DisableMemory(c fiber.Ctx) error {
	return h.setMemoryDisabled(c, true)
}

// EnableMemory enables a previously disabled memory.
func (h *Handlers) EnableMemory(c fiber.Ctx) error {
	return h.setMemoryDisabled(c, false)
}

func (h *Handlers) setMemoryDisabled(c fiber.Ctx, disabled bool) error {
	if err := h.ensureMemoryStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "setMemoryDisabled", h.contextTokenAuthorization.MemoryWriteScopes); err != nil {
		return err
	}
	actor, _ := memoryActor(c)
	if err := h.memoryService.SetMemoryDisabled(c.Context(), namespace, c.Params("id"), disabled, actor, requestid.FromContext(c)); err != nil {
		return memoryServiceError(err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// ListMemoryProposals lists memory governance proposals.
func (h *Handlers) ListMemoryProposals(c fiber.Ctx) error {
	if err := h.ensureMemoryProposalStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "listMemoryProposals", h.contextTokenAuthorization.MemoryReadScopes); err != nil {
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
	return c.JSON(ListResponse{Items: proposals, Metadata: ListMeta{}})
}

// CreateMemoryProposal creates a governance proposal. It does not apply proposals automatically.
func (h *Handlers) CreateMemoryProposal(c fiber.Ctx) error {
	if err := h.ensureMemoryProposalStore(); err != nil {
		return err
	}
	var request memoryProposalCreateRequest
	if err := bindStrictMemoryJSON(c, &request); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	namespace, err := h.resolveNamespace(c, request.Namespace)
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "createMemoryProposal", h.contextTokenAuthorization.MemoryWriteScopes); err != nil {
		return err
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

// GetMemoryProposal gets a proposal by ID.
func (h *Handlers) GetMemoryProposal(c fiber.Ctx) error {
	if err := h.ensureMemoryProposalStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "getMemoryProposal", h.contextTokenAuthorization.MemoryReadScopes); err != nil {
		return err
	}
	proposal, err := h.memoryProposalStore.GetMemoryProposal(c.Context(), namespace, c.Params("id"))
	if err != nil {
		return memoryStoreError("get memory proposal", "memory proposal", err)
	}
	return c.JSON(proposal)
}

// ReviewMemoryProposal records a review decision without applying the proposal automatically.
func (h *Handlers) ReviewMemoryProposal(c fiber.Ctx) error {
	if err := h.ensureMemoryProposalStore(); err != nil {
		return err
	}
	review, err := bindMemoryProposalReview(c, c.Query("namespace", ""), c.Params("id"))
	if err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, review.Namespace)
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "reviewMemoryProposal", h.contextTokenAuthorization.MemoryOperateScopes); err != nil {
		return err
	}
	review.Namespace = namespace
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

// ArchiveMemoryProposal archives a proposal without applying it.
func (h *Handlers) ArchiveMemoryProposal(c fiber.Ctx) error {
	if err := h.ensureMemoryProposalStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "archiveMemoryProposal", h.contextTokenAuthorization.MemoryWriteScopes); err != nil {
		return err
	}
	if err := h.memoryProposalStore.ArchiveMemoryProposal(c.Context(), namespace, c.Params("id")); err != nil {
		return memoryStoreError("archive memory proposal", "memory proposal", err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

// ApplyMemoryProposal applies an accepted memory proposal into durable memory.
func (h *Handlers) ApplyMemoryProposal(c fiber.Ctx) error {
	if err := h.ensureMemoryProposalStore(); err != nil {
		return err
	}
	apply, err := bindMemoryProposalApply(c, c.Query("namespace", ""), c.Params("id"))
	if err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, apply.Namespace)
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "applyMemoryProposal", h.contextTokenAuthorization.MemoryWriteScopes); err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "applyMemoryProposal", h.contextTokenAuthorization.MemoryOperateScopes); err != nil {
		return err
	}
	actor, _ := memoryActor(c)
	if apply.AppliedBy == "" {
		apply.AppliedBy = actor
	}
	result, err := h.memoryService.ApplyMemoryProposal(c.Context(), namespace, apply.ID, apply.AppliedBy,
		memoryMutationContext(c, "applyMemoryProposal", "memory proposal applied"))
	if err != nil {
		return memoryServiceError(err)
	}
	return sendMemoryMutationResult(c, result)
}

type memoryProposalCreateRequest struct {
	Namespace   string `json:"namespace,omitempty"`
	TaskName    string `json:"taskName,omitempty"`
	AgentName   string `json:"agentName,omitempty"`
	Type        string `json:"type,omitempty"`
	SkillName   string `json:"skillName,omitempty"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Content     string `json:"content,omitempty"`
	Patch       string `json:"patch,omitempty"`
}

func (r memoryProposalCreateRequest) toStore(namespace string) *store.MemoryProposal {
	return &store.MemoryProposal{
		Namespace: namespace, TaskName: r.TaskName, AgentName: r.AgentName, Type: r.Type,
		SkillName: r.SkillName, Title: r.Title, Description: r.Description, Content: r.Content, Patch: r.Patch,
	}
}

func parseMemoryFilter(c fiber.Ctx, namespace string) (store.MemoryFilter, error) {
	limit, err := parseOptionalLimit(c.Query("limit", ""))
	if err != nil {
		return store.MemoryFilter{}, err
	}
	query := c.Query("query", "")
	if query == "" {
		query = c.Query("q", "")
	}
	return store.MemoryFilter{
		Namespace:       namespace,
		Query:           query,
		SessionName:     c.Query("sessionName", ""),
		AgentName:       c.Query("agentName", ""),
		TaskName:        c.Query("taskName", ""),
		ParentTask:      c.Query("parentTask", ""),
		Source:          c.Query("source", ""),
		Tags:            splitCSV(c.Query("tags", "")),
		IDs:             splitCSV(c.Query("ids", "")),
		Trust:           parseMemoryTrust(c.Query("trust", "")),
		IncludeDisabled: c.Query("includeDisabled", "") == queryTrue,
		IncludeDeleted:  c.Query("includeDeleted", "") == queryTrue,
		Limit:           limit,
		Cursor:          strings.TrimSpace(c.Query("continue", c.Query("cursor", ""))),
	}, nil
}

func parseMemoryTrust(raw string) []store.MemoryTrust {
	values := splitCSV(raw)
	result := make([]store.MemoryTrust, 0, len(values))
	for _, value := range values {
		result = append(result, store.MemoryTrust(strings.ToLower(value)))
	}
	return result
}

func parseMemoryProposalFilter(c fiber.Ctx, namespace string) (store.MemoryProposalFilter, error) {
	limit, err := parseOptionalLimit(c.Query("limit", ""))
	if err != nil {
		return store.MemoryProposalFilter{}, err
	}
	query := c.Query("query", "")
	if query == "" {
		query = c.Query("q", "")
	}
	return store.MemoryProposalFilter{
		Namespace: namespace,
		TaskName:  c.Query("taskName", ""),
		AgentName: c.Query("agentName", ""),
		Type:      c.Query("type", ""),
		Status:    c.Query("status", ""),
		Query:     query,
		Limit:     limit,
	}, nil
}

func parseOptionalLimit(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 0 {
		return 0, fiber.NewError(fiber.StatusBadRequest, "invalid limit")
	}
	return limit, nil
}

func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func bindMemoryProposalReview(c fiber.Ctx, fallbackNamespace, id string) (store.MemoryProposalReview, error) {
	var req struct {
		Namespace  string `json:"namespace"`
		Status     string `json:"status"`
		Reviewer   string `json:"reviewer"`
		ReviewNote string `json:"reviewNote"`
	}
	if err := c.Bind().JSON(&req); err != nil {
		return store.MemoryProposalReview{}, fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if req.Namespace == "" {
		req.Namespace = fallbackNamespace
	}
	if strings.TrimSpace(req.Status) == "" {
		return store.MemoryProposalReview{}, fiber.NewError(fiber.StatusBadRequest, "status is required")
	}
	return store.MemoryProposalReview{
		Namespace:  req.Namespace,
		ID:         id,
		Status:     req.Status,
		Reviewer:   req.Reviewer,
		ReviewNote: req.ReviewNote,
	}, nil
}

func bindMemoryProposalApply(c fiber.Ctx, fallbackNamespace, id string) (store.MemoryProposalApply, error) {
	var req struct {
		Namespace string `json:"namespace"`
		AppliedBy string `json:"appliedBy"`
	}
	if strings.TrimSpace(string(c.Body())) != "" {
		if err := c.Bind().JSON(&req); err != nil {
			return store.MemoryProposalApply{}, fiber.NewError(fiber.StatusBadRequest, "invalid request body")
		}
	}
	if req.Namespace == "" {
		req.Namespace = fallbackNamespace
	}
	return store.MemoryProposalApply{
		Namespace: req.Namespace,
		ID:        id,
		AppliedBy: req.AppliedBy,
	}, nil
}

func bindStrictMemoryJSON(c fiber.Ctx, target any) error {
	body := bytes.TrimSpace(c.Body())
	if len(body) == 0 {
		return fmt.Errorf("request body is required")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("trailing JSON is not allowed")
		}
		return err
	}
	return nil
}

func memoryActor(c fiber.Ctx) (actor, principal string) {
	if user := GetUserInfo(c); user != nil {
		actor = strings.TrimSpace(user.Username)
		principal = actor
		if user.UID != "" {
			principal += "#" + user.UID
		} else if user.Subject != "" {
			principal += "#" + user.Subject
		}
	}
	if actor == "" {
		actor = "authenticated-memory-caller"
	}
	if principal == "" {
		principal = actor
	}
	return actor, principal
}

func memoryMutationContext(c fiber.Ctx, route, reason string) memoryruntime.MutationContext {
	actor, principal := memoryActor(c)
	return memoryruntime.MutationContext{
		Principal: principal, Actor: actor, IdempotencyKey: strings.TrimSpace(c.Get("Idempotency-Key")),
		Route: route, RequestID: requestid.FromContext(c), Reason: reason,
		LocationBase: "/api/v1/memory-operations/",
	}
}

func internalMemoryMutationContext(c fiber.Ctx, namespace, route, reason string) memoryruntime.MutationContext {
	context := memoryMutationContext(c, route, reason)
	context.LocationBase = "/internal/v1/memory-operations/" + namespace + "/"
	return context
}

func sendMemoryMutationResult(c fiber.Ctx, result *memoryruntime.MutationResult) error {
	if result == nil {
		return fiber.NewError(fiber.StatusInternalServerError, "memory service returned no result")
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

func memoryServiceError(err error) error {
	if err == nil {
		return nil
	}
	var structured *apierror.Error
	if errors.As(err, &structured) {
		return fmt.Errorf("%w: %w", fiber.NewError(structured.Status, structured.Error()), err)
	}
	return memoryStoreError("perform memory operation", "memory", err)
}

func memoryStoreError(action, resource string, err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound, fmt.Sprintf("%s not found", resource))
	}
	if errors.Is(err, store.ErrConflict) {
		return fiber.NewError(fiber.StatusConflict, err.Error())
	}
	if isStoreValidationError(err) {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to %s: %v", action, err))
}

func isStoreValidationError(err error) bool {
	return errors.Is(err, store.ErrValidation)
}
