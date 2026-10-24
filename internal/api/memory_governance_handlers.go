package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	memoryruntime "github.com/orka-agents/orka/internal/memory"
	"github.com/orka-agents/orka/internal/store"
)

// SearchMemories performs explicit bounded memory search.
func (h *Handlers) SearchMemories(c fiber.Ctx) error {
	if err := h.ensureMemoryStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	var request memoryruntime.SearchRequest
	if err := bindStrictMemoryJSON(c, &request); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if err := h.authorizeMemoryReadVisibility(c, "searchMemories", request.IncludeDisabled); err != nil {
		return err
	}
	response, err := h.memoryService.Search(c.Context(), namespace, request, h.memorySearchContext(c))
	if err != nil {
		return memoryServiceError(err)
	}
	return c.JSON(response)
}

func (h *Handlers) authorizeMemoryReadVisibility(c fiber.Ctx, action string, includeDisabled bool) error {
	if err := h.authorizeContextTokenAction(c, action, h.contextTokenAuthorization.MemoryReadScopes); err != nil {
		return err
	}
	if !includeDisabled {
		return nil
	}
	return h.authorizeContextTokenAction(c, action+"IncludeDisabled", h.contextTokenAuthorization.MemoryOperateScopes)
}

func (h *Handlers) memorySearchContext(c fiber.Ctx) memoryruntime.SearchContext {
	actor, _ := memoryActor(c)
	return memoryruntime.SearchContext{
		Actor: actor, RequestID: requestid.FromContext(c),
		AuthorizeRemote: func() error {
			return h.authorizeContextTokenAction(c, "searchRemoteMemories", h.contextTokenAuthorization.MemorySearchRemoteScopes)
		},
	}
}

// SetMemoryTrust performs an audited server-owned trust transition.
func (h *Handlers) SetMemoryTrust(c fiber.Ctx) error {
	if err := h.ensureMemoryStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "setMemoryTrust", h.contextTokenAuthorization.MemoryOperateScopes); err != nil {
		return err
	}
	var request memoryruntime.TrustRequest
	if err := bindStrictMemoryJSON(c, &request); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	actor, _ := memoryActor(c)
	memory, err := h.memoryService.SetMemoryTrust(c.Context(), namespace, c.Params("id"), request, memoryruntime.TrustContext{
		Actor: actor, RequestID: requestid.FromContext(c),
		AuthorizeRemote: func() error {
			if err := h.authorizeMemoryBackend(c, namespace, "get", corev1alpha1.MemoryBackendDefaultName, true); err != nil {
				return err
			}
			return h.authorizeMemoryBackend(c, namespace, "update", corev1alpha1.MemoryBackendDefaultName, true)
		},
	})
	if err != nil {
		return memoryServiceError(err)
	}
	return c.JSON(memory)
}

// ListMemoryOperations lists allowlisted durable operation summaries.
func (h *Handlers) ListMemoryOperations(c fiber.Ctx) error {
	if err := h.ensureMemoryStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "listMemoryOperations", h.contextTokenAuthorization.MemoryReadScopes); err != nil {
		return err
	}
	filter, err := parseMemoryOperationFilter(c, namespace)
	if err != nil {
		return err
	}
	operations, err := h.memoryService.ListMemoryOperations(c.Context(), namespace, filter)
	if err != nil {
		return memoryServiceError(err)
	}
	items := make([]memoryruntime.Operation, 0, len(operations))
	for _, operation := range operations {
		items = append(items, memoryruntime.OperationFromStore(operation))
	}
	next, err := encodeMemoryOperationCursor(namespace, filter, operations)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "memory operation cursor could not be encoded")
	}
	return c.JSON(ListResponse{Items: items, Metadata: ListMeta{Continue: next}})
}

// GetMemoryOperation gets an allowlisted durable operation summary.
func (h *Handlers) GetMemoryOperation(c fiber.Ctx) error {
	if err := h.ensureMemoryStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "getMemoryOperation", h.contextTokenAuthorization.MemoryReadScopes); err != nil {
		return err
	}
	operation, err := h.memoryService.GetMemoryOperation(c.Context(), namespace, c.Params("id"))
	if err != nil {
		return memoryServiceError(err)
	}
	if operation.State == store.MemoryOperationQueued || operation.State == store.MemoryOperationLeased ||
		operation.State == store.MemoryOperationDispatching || operation.State == store.MemoryOperationAmbiguous {
		c.Set("Retry-After", "2")
	}
	return c.JSON(memoryruntime.OperationFromStore(*operation))
}

// RetryMemoryOperation performs an audited manual retry.
func (h *Handlers) RetryMemoryOperation(c fiber.Ctx) error {
	return h.memoryOperationAction(c, false)
}

// AbandonMemoryOperation performs an audited, provider-proven abandonment.
func (h *Handlers) AbandonMemoryOperation(c fiber.Ctx) error {
	return h.memoryOperationAction(c, true)
}

func (h *Handlers) memoryOperationAction(c fiber.Ctx, abandon bool) error {
	if err := h.ensureMemoryStore(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "operateMemoryOperation", h.contextTokenAuthorization.MemoryOperateScopes); err != nil {
		return err
	}
	if err := h.authorizeMemoryBackend(c, namespace, "get", corev1alpha1.MemoryBackendDefaultName, true); err != nil {
		return err
	}
	if err := h.authorizeMemoryBackend(c, namespace, "update", corev1alpha1.MemoryBackendDefaultName, true); err != nil {
		return err
	}
	var request struct {
		Reason string `json:"reason"`
	}
	if err := bindStrictMemoryJSON(c, &request); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	actor, _ := memoryActor(c)
	var operation *store.MemoryOperation
	if abandon {
		operation, err = h.memoryService.AbandonMemoryOperation(c.Context(), namespace, c.Params("id"), actor,
			request.Reason, requestid.FromContext(c))
	} else {
		operation, err = h.memoryService.RetryMemoryOperation(c.Context(), namespace, c.Params("id"), actor,
			request.Reason, requestid.FromContext(c))
	}
	if err != nil {
		return memoryServiceError(err)
	}
	location := memoryOperationLocation(namespace, operation.ID)
	c.Set("Location", location)
	c.Set("Retry-After", "2")
	return c.Status(http.StatusAccepted).JSON(memoryruntime.OperationFromStore(*operation))
}

func memoryOperationLocation(namespace, operationID string) string {
	location := "/api/v1/memory-operations/" + url.PathEscape(operationID)
	query := url.Values{}
	query.Set("namespace", namespace)
	return location + "?" + query.Encode()
}

// SearchMemories performs internal namespace-bound search.
func (h *InternalHandlers) SearchMemories(c fiber.Ctx) error {
	namespace, err := h.internalNamespace(c)
	if err != nil {
		return err
	}
	if err := h.ensureMemoryStore(); err != nil {
		return err
	}
	var request memoryruntime.SearchRequest
	if err := bindStrictMemoryJSON(c, &request); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	requiredScopes := [][]string{h.memoryReadScopes()}
	if request.IncludeDisabled {
		requiredScopes = append(requiredScopes, h.memoryOperateScopes())
	}
	if err := h.authorizeInternalMemoryTask(c, namespace, "searchMemories", requiredScopes...); err != nil {
		return err
	}
	actor, _ := memoryActor(c)
	response, err := h.memoryService.Search(c.Context(), namespace, request, memoryruntime.SearchContext{
		Actor: actor, RequestID: requestid.FromContext(c),
		AuthorizeRemote: func() error {
			return h.authorizeInternalMemoryTask(c, namespace, "searchRemoteMemories",
				h.memoryReadScopes(), h.memorySearchRemoteScopes())
		},
	})
	if err != nil {
		return memoryServiceError(err)
	}
	return c.JSON(response)
}

// ListMemoryOperations lists internal namespace-bound operations.
func (h *InternalHandlers) ListMemoryOperations(c fiber.Ctx) error {
	if err := h.ensureMemoryStore(); err != nil {
		return err
	}
	namespace, err := h.internalNamespace(c)
	if err != nil {
		return err
	}
	if err := h.authorizeInternalMemoryTask(c, namespace, "listMemoryOperations", h.memoryReadScopes()); err != nil {
		return err
	}
	filter, err := parseMemoryOperationFilter(c, namespace)
	if err != nil {
		return err
	}
	operations, err := h.memoryService.ListMemoryOperations(c.Context(), namespace, filter)
	if err != nil {
		return memoryServiceError(err)
	}
	items := make([]memoryruntime.Operation, 0, len(operations))
	for _, operation := range operations {
		items = append(items, memoryruntime.OperationFromStore(operation))
	}
	next, err := encodeMemoryOperationCursor(namespace, filter, operations)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "memory operation cursor could not be encoded")
	}
	return c.JSON(ListResponse{Items: items, Metadata: ListMeta{Continue: next}})
}

// GetMemoryOperation gets an internal namespace-bound operation.
func (h *InternalHandlers) GetMemoryOperation(c fiber.Ctx) error {
	if err := h.ensureMemoryStore(); err != nil {
		return err
	}
	namespace, err := h.internalNamespace(c)
	if err != nil {
		return err
	}
	if err := h.authorizeInternalMemoryTask(c, namespace, "getMemoryOperation", h.memoryReadScopes()); err != nil {
		return err
	}
	operation, err := h.memoryService.GetMemoryOperation(c.Context(), namespace, c.Params("id"))
	if err != nil {
		return memoryServiceError(err)
	}
	if operation.State == store.MemoryOperationQueued || operation.State == store.MemoryOperationLeased ||
		operation.State == store.MemoryOperationDispatching || operation.State == store.MemoryOperationAmbiguous {
		c.Set("Retry-After", "2")
	}
	return c.JSON(memoryruntime.OperationFromStore(*operation))
}

const (
	defaultMemoryOperationPageLimit = 100
	maxMemoryOperationPageLimit     = 200
	maxMemoryOperationCursorBytes   = 2048
)

type memoryOperationCursor struct {
	Namespace  string                       `json:"namespace"`
	CreatedAt  time.Time                    `json:"createdAt"`
	Sequence   int64                        `json:"sequence"`
	MemoryID   string                       `json:"memoryId,omitempty"`
	ProposalID string                       `json:"proposalId,omitempty"`
	Kinds      []store.MemoryOperationKind  `json:"kinds,omitempty"`
	States     []store.MemoryOperationState `json:"states,omitempty"`
	PageLimit  int                          `json:"limit"`
}

func parseMemoryOperationFilter(c fiber.Ctx, namespace ...string) (store.MemoryOperationFilter, error) {
	limit, err := parseOptionalLimit(c.Query("limit", ""))
	if err != nil {
		return store.MemoryOperationFilter{}, err
	}
	filter := store.MemoryOperationFilter{
		MemoryID: c.Query("memoryId", ""), ProposalID: c.Query("proposalId", ""), Limit: limit,
	}
	for _, kind := range splitCSV(c.Query("kind", "")) {
		filter.Kinds = append(filter.Kinds, store.MemoryOperationKind(kind))
	}
	for _, state := range splitCSV(c.Query("state", "")) {
		filter.States = append(filter.States, store.MemoryOperationState(state))
	}
	cursorValue := strings.TrimSpace(c.Query("cursor", c.Query("continue", "")))
	beforeSequence := strings.TrimSpace(c.Query("beforeSequence", ""))
	beforeCreatedAt := strings.TrimSpace(c.Query("beforeCreatedAt", ""))
	if cursorValue != "" && (beforeSequence != "" || beforeCreatedAt != "") {
		return store.MemoryOperationFilter{}, fiber.NewError(fiber.StatusBadRequest,
			"memory operation cursor cannot be combined with beforeSequence or beforeCreatedAt")
	}
	if cursorValue != "" {
		expectedNamespace := ""
		if len(namespace) > 0 {
			expectedNamespace = namespace[0]
		}
		cursor, decodeErr := decodeMemoryOperationCursor(cursorValue)
		if decodeErr != nil || !memoryOperationCursorMatches(cursor, expectedNamespace, filter) {
			return store.MemoryOperationFilter{}, fiber.NewError(fiber.StatusBadRequest, "invalid memory operation cursor")
		}
		filter.BeforeCreatedAt = &cursor.CreatedAt
		filter.BeforeSequence = cursor.Sequence
		return filter, nil
	}
	if (beforeSequence == "") != (beforeCreatedAt == "") {
		return store.MemoryOperationFilter{}, fiber.NewError(fiber.StatusBadRequest,
			"memory operation pagination requires both beforeCreatedAt and beforeSequence")
	}
	if beforeSequence != "" {
		raw := beforeSequence
		sequence, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || sequence <= 0 {
			return store.MemoryOperationFilter{}, fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("invalid beforeSequence %q", raw))
		}
		createdAt, parseErr := time.Parse(time.RFC3339Nano, beforeCreatedAt)
		if parseErr != nil || createdAt.IsZero() {
			return store.MemoryOperationFilter{}, fiber.NewError(fiber.StatusBadRequest,
				fmt.Sprintf("invalid beforeCreatedAt %q", beforeCreatedAt))
		}
		createdAt = createdAt.UTC()
		filter.BeforeCreatedAt = &createdAt
		filter.BeforeSequence = sequence
	}
	return filter, nil
}

func encodeMemoryOperationCursor(namespace string, filter store.MemoryOperationFilter, operations []store.MemoryOperation) (string, error) {
	if len(operations) < memoryOperationPageLimit(filter.Limit) || len(operations) == 0 {
		return "", nil
	}
	last := operations[len(operations)-1]
	if last.Sequence <= 0 || last.CreatedAt.IsZero() {
		return "", fmt.Errorf("operation cursor requires stable ordering fields")
	}
	cursor := memoryOperationCursor{
		Namespace: strings.TrimSpace(namespace), CreatedAt: last.CreatedAt.UTC(), Sequence: last.Sequence,
		MemoryID: strings.TrimSpace(filter.MemoryID), ProposalID: strings.TrimSpace(filter.ProposalID),
		Kinds: canonicalMemoryOperationKinds(filter.Kinds), States: canonicalMemoryOperationStates(filter.States),
		PageLimit: memoryOperationPageLimit(filter.Limit),
	}
	encoded, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeMemoryOperationCursor(value string) (memoryOperationCursor, error) {
	if value == "" || len(value) > maxMemoryOperationCursorBytes {
		return memoryOperationCursor{}, fmt.Errorf("invalid cursor length")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) > maxMemoryOperationCursorBytes {
		return memoryOperationCursor{}, fmt.Errorf("invalid cursor encoding")
	}
	var cursor memoryOperationCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil || cursor.CreatedAt.IsZero() || cursor.Sequence <= 0 ||
		cursor.PageLimit <= 0 || cursor.PageLimit > maxMemoryOperationPageLimit {
		return memoryOperationCursor{}, fmt.Errorf("invalid cursor payload")
	}
	cursor.Namespace = strings.TrimSpace(cursor.Namespace)
	cursor.MemoryID = strings.TrimSpace(cursor.MemoryID)
	cursor.ProposalID = strings.TrimSpace(cursor.ProposalID)
	cursor.CreatedAt = cursor.CreatedAt.UTC()
	cursor.Kinds = canonicalMemoryOperationKinds(cursor.Kinds)
	cursor.States = canonicalMemoryOperationStates(cursor.States)
	return cursor, nil
}

func memoryOperationCursorMatches(cursor memoryOperationCursor, namespace string, filter store.MemoryOperationFilter) bool {
	return cursor.Namespace == strings.TrimSpace(namespace) && cursor.MemoryID == strings.TrimSpace(filter.MemoryID) &&
		cursor.ProposalID == strings.TrimSpace(filter.ProposalID) && cursor.PageLimit == memoryOperationPageLimit(filter.Limit) &&
		slices.Equal(cursor.Kinds, canonicalMemoryOperationKinds(filter.Kinds)) &&
		slices.Equal(cursor.States, canonicalMemoryOperationStates(filter.States))
}

func canonicalMemoryOperationKinds(values []store.MemoryOperationKind) []store.MemoryOperationKind {
	result := slices.Clone(values)
	slices.Sort(result)
	return result
}

func canonicalMemoryOperationStates(values []store.MemoryOperationState) []store.MemoryOperationState {
	result := slices.Clone(values)
	slices.Sort(result)
	return result
}

func memoryOperationPageLimit(limit int) int {
	if limit <= 0 {
		return defaultMemoryOperationPageLimit
	}
	return min(limit, maxMemoryOperationPageLimit)
}
