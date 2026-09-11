package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/sessioncontext"
	"github.com/orka-agents/orka/internal/store"
)

const (
	sessionContextRoleTool = "tool"
	sessionContextRoleUser = "user"
)

type taskSessionContextPolicy struct {
	task           *corev1alpha1.Task
	name           string
	through        string
	maxMessages    int
	promptIncluded bool
	writable       bool
	storage        store.SessionContextStore
}

// sessionContextPolicy derives all access from the authenticated current worker
// and stored Gateway ownership. Callers cannot supply a Session or read cutoff.
func (h *InternalHandlers) sessionContextPolicy(c fiber.Ctx, write bool) (*taskSessionContextPolicy, error) {
	namespace, taskName := c.Params("namespace"), c.Params("taskName")
	if namespace == "" || taskName == "" {
		return nil, fiber.NewError(fiber.StatusBadRequest, "namespace and taskName are required")
	}
	authorizer := h.internalCallerAuthorizer()
	if err := authorizer.verifyNamespace(c, namespace); err != nil {
		return nil, err
	}
	if authorizer.k8sReader == nil {
		return nil, fiber.NewError(fiber.StatusServiceUnavailable, "task context authorization is unavailable")
	}
	task := &corev1alpha1.Task{}
	if err := authorizer.k8sReader.Get(c.Context(), client.ObjectKey{Namespace: namespace, Name: taskName}, task); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fiber.NewError(fiber.StatusNotFound, "task not found")
		}
		return nil, fiber.NewError(fiber.StatusInternalServerError, "failed to load task context policy")
	}
	if err := authorizer.verifyTaskWorker(c.Context(), GetUserInfo(c), task); err != nil {
		return nil, err
	}
	storage, ok := h.sessionStore.(store.SessionContextStore)
	if !ok {
		return nil, fiber.NewError(fiber.StatusNotImplemented, "session checkpoint storage is unavailable")
	}
	policy := &taskSessionContextPolicy{task: task, storage: storage, maxMessages: 50}
	if task.Spec.SessionRef != nil {
		ref := task.Spec.SessionRef
		policy.name, policy.through = ref.Name, ref.ThroughMessageID
		policy.promptIncluded = ref.PromptIncluded
		policy.writable = ref.Append && ref.ThroughMessageID == "" && !ref.PromptIncluded
		if ref.MaxMessages > 0 {
			policy.maxMessages = min(int(ref.MaxMessages), sessioncontext.MaxBootstrapMessages)
		}
	}
	gatewayOwned := false
	if h.gatewayEventStore != nil {
		event, err := h.gatewayEventStore.GetGatewayEventForTask(c.Context(), namespace, task.Name, string(task.UID))
		switch {
		case err == nil:
			gatewayOwned = true
			policy.name = event.SessionName
			policy.through = store.GatewayUserMessageID(event.ID)
			policy.maxMessages = min(store.GatewayTranscriptMessageLimit, sessioncontext.MaxBootstrapMessages)
			policy.promptIncluded, policy.writable = true, false
		case errors.Is(err, store.ErrNotFound):
		default:
			return nil, fiber.NewError(fiber.StatusInternalServerError, "failed to load gateway context ownership")
		}
	}
	if policy.name == "" {
		return nil, fiber.NewError(fiber.StatusForbidden, "task has no authorized session")
	}
	sessionType, err := transcriptSessionType(c.Context(), h.sessionStore, namespace, policy.name)
	if err != nil {
		return nil, sessionContextHTTPError(err)
	}
	if sessionType == store.SessionTypeGateway && !gatewayOwned {
		return nil, fiber.NewError(fiber.StatusForbidden, "task does not own this gateway session")
	}
	if gatewayOwned || sessionType == store.SessionTypeGateway {
		policy.writable = false
	}
	if write && (!policy.writable || task.UID == "" || !task.DeletionTimestamp.IsZero() || isTerminalInternalTaskPhase(task.Status.Phase)) {
		return nil, fiber.NewError(fiber.StatusForbidden, "checkpoint writes require a live Task with an appending, unbounded Session")
	}
	return policy, nil
}

func (p *taskSessionContextPolicy) write() store.SessionContextWrite {
	return store.SessionContextWrite{
		Namespace: p.task.Namespace, SessionName: p.name,
		OwnerName: p.task.Name, OwnerUID: string(p.task.UID), ThroughMessageID: p.through,
	}
}

// GetTaskSessionContext returns a bounded checkpoint and recent stored history.
func (h *InternalHandlers) GetTaskSessionContext(c fiber.Ctx) error {
	policy, err := h.sessionContextPolicy(c, false)
	if err != nil {
		return err
	}
	var messages []store.SessionMessage
	if policy.through != "" {
		messages, err = h.sessionStore.LoadTranscriptThrough(c.Context(), policy.task.Namespace, policy.name, policy.through, policy.maxMessages)
	} else {
		messages, err = h.sessionStore.LoadTranscript(c.Context(), policy.task.Namespace, policy.name, policy.maxMessages)
	}
	if err != nil {
		return sessionContextHTTPError(err)
	}
	bootstrap := sessioncontext.Bootstrap{
		SessionName: policy.name, ThroughMessageID: policy.through,
		Writable: policy.writable, PromptIncluded: policy.promptIncluded,
		Messages: []store.SessionMessage{},
	}
	for _, message := range messages {
		if message.SourceType == sessioncontext.SourceType && message.SourceRef == string(policy.task.UID) &&
			message.ID != sessioncontext.PromptMessageID(string(policy.task.UID)) {
			bootstrap.TaskHistoryExists = true
		}
	}
	if !bootstrap.TaskHistoryExists {
		bootstrap.TaskHistoryExists, err = policy.taskHistoryExists(c.Context())
		if err != nil {
			return sessionContextHTTPError(err)
		}
	}
	// Freeze checkpoint selection at the same observed tail. The terminal current
	// user message must remain separate even when Gateway admission stored it.
	checkpointBoundary := ""
	checkpointEnd := len(messages)
	if policy.promptIncluded {
		checkpointEnd--
	}
	if checkpointEnd > 0 {
		checkpointBoundary = messages[checkpointEnd-1].ID
		bootstrap.Checkpoint, err = policy.storage.LoadSessionCheckpoint(c.Context(), policy.task.Namespace, policy.name, checkpointBoundary)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return sessionContextHTTPError(err)
		}
	}
	for i, message := range messages {
		if bootstrap.Checkpoint != nil && message.Order <= bootstrap.Checkpoint.LastMessageOrder &&
			(!policy.promptIncluded || i != len(messages)-1) {
			continue
		}
		bootstrap.Messages = append(bootstrap.Messages, message)
	}
	// A suffix can start inside a tool exchange. Remove that incomplete prefix;
	// an incomplete tail is preserved so the worker can stop without replaying it.
	for len(bootstrap.Messages) > 0 && bootstrap.Messages[0].Role == sessionContextRoleTool {
		bootstrap.Messages = bootstrap.Messages[1:]
	}
	// Check the remaining exchanges before byte trimming can hide an unfinished
	// batch from a previous Task. Complete exchanges may still be omitted.
	modelMessages := make([]llm.Message, 0, len(bootstrap.Messages))
	for _, message := range bootstrap.Messages {
		modelMessage, convertErr := sessioncontext.ModelMessage(message)
		if convertErr != nil {
			return fiber.NewError(fiber.StatusUnprocessableEntity, "saved history contains invalid tool calls")
		}
		modelMessages = append(modelMessages, modelMessage)
	}
	validationBudget := llm.EstimateRequestTokens(&llm.CompletionRequest{Messages: modelMessages})
	if _, err := llm.FitMessagesKeeping(modelMessages, validationBudget, -1); err != nil {
		return fiber.NewError(fiber.StatusUnprocessableEntity, "saved history contains an incomplete exchange; inspect execution records before continuing")
	}
	for {
		data, marshalErr := json.Marshal(bootstrap)
		if marshalErr != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "failed to encode session context")
		}
		if len(data) <= sessioncontext.MaxBootstrapBytes {
			c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
			return c.Send(data)
		}
		if len(bootstrap.Messages) <= 1 {
			return fiber.NewError(fiber.StatusUnprocessableEntity, "session context cannot fit its byte allowance; the current request was not shortened")
		}
		bootstrap.Messages = bootstrap.Messages[1:]
		for len(bootstrap.Messages) > 0 && bootstrap.Messages[0].Role == sessionContextRoleTool {
			bootstrap.Messages = bootstrap.Messages[1:]
		}
	}
}

// A small recent-history limit must not hide this Task's earlier execution.
// Its first non-final model response uses sequence 1; final-only Tasks use the
// stable final ID. Read one byte only to test presence within the same boundary.
func (p *taskSessionContextPolicy) taskHistoryExists(ctx context.Context) (bool, error) {
	uid := string(p.task.UID)
	for _, id := range []string{sessioncontext.TaskMessagePrefix(uid) + "1", sessioncontext.FinalMessageID(uid)} {
		_, err := p.storage.ReadSessionHistory(ctx, store.SessionHistoryRead{
			Namespace: p.task.Namespace, SessionName: p.name, ThroughMessageID: p.through, MessageID: id, Limit: 1,
		})
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return false, err
		}
	}
	return false, nil
}

// AppendTaskContextMessages commits source content before the worker reduces
// active context or starts another tool. The exact Task owner fences each save.
func (h *InternalHandlers) AppendTaskContextMessages(c fiber.Ctx) error {
	policy, err := h.sessionContextPolicy(c, true)
	if err != nil {
		return err
	}
	if len(c.Body()) > store.MaxSessionContextMessageBytes+64*1024 {
		return fiber.NewError(fiber.StatusRequestEntityTooLarge, "session source batch exceeds its byte allowance")
	}
	var messages []store.SessionMessage
	decoder := json.NewDecoder(bytes.NewReader(c.Body()))
	decoder.UseNumber()
	if err := decoder.Decode(&messages); err != nil || decoder.Decode(new(any)) != io.EOF ||
		len(messages) == 0 || len(messages) > 16 {
		return fiber.NewError(fiber.StatusBadRequest, "expected between 1 and 16 session source messages")
	}
	prefix := sessioncontext.TaskMessagePrefix(string(policy.task.UID))
	for i := range messages {
		message := &messages[i]
		if !strings.HasPrefix(message.ID, prefix) || len(message.ID) > 256 || message.Order != 0 {
			return fiber.NewError(fiber.StatusBadRequest, "source message IDs must belong to the authenticated Task; order is assigned by storage")
		}
		switch message.Role {
		case sessionContextRoleUser, chatRoleAssistant, sessionContextRoleTool:
		default:
			return fiber.NewError(fiber.StatusBadRequest, "source messages must retain a user, assistant, or tool role")
		}
		message.SourceType, message.SourceRef = sessioncontext.SourceType, string(policy.task.UID)
	}
	stored, err := policy.storage.AppendContextMessages(c.Context(), policy.write(), messages)
	if err != nil {
		return sessionContextHTTPError(err)
	}
	return c.JSON(stored)
}

// SaveTaskSessionCheckpoint only commits notes over already saved sources.
func (h *InternalHandlers) SaveTaskSessionCheckpoint(c fiber.Ctx) error {
	policy, err := h.sessionContextPolicy(c, true)
	if err != nil {
		return err
	}
	if len(c.Body()) > 32*1024 {
		return fiber.NewError(fiber.StatusRequestEntityTooLarge, "checkpoint request exceeds its byte allowance")
	}
	var checkpoint store.SessionCheckpoint
	if err := json.Unmarshal(c.Body(), &checkpoint); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid checkpoint request")
	}
	if !strings.HasPrefix(checkpoint.ID, sessioncontext.TaskMessagePrefix(string(policy.task.UID))) {
		return fiber.NewError(fiber.StatusBadRequest, "checkpoint ID must belong to the authenticated Task")
	}
	if err := policy.storage.SaveSessionCheckpoint(c.Context(), policy.write(), checkpoint); err != nil {
		return sessionContextHTTPError(err)
	}
	committed, err := policy.storage.LoadSessionCheckpoint(c.Context(), policy.task.Namespace, policy.name, checkpoint.LastMessageID)
	if err != nil {
		return sessionContextHTTPError(err)
	}
	if committed.ID != checkpoint.ID {
		return fiber.NewError(fiber.StatusConflict, "checkpoint changed before readback")
	}
	return c.JSON(committed)
}

// ReadTaskSessionHistory reads one bounded source fragment from this Task's
// allowed Session. Cross-Session search remains a separate restricted feature.
func (h *InternalHandlers) ReadTaskSessionHistory(c fiber.Ctx) error {
	policy, err := h.sessionContextPolicy(c, false)
	if err != nil {
		return err
	}
	messageID, err := url.PathUnescape(c.Params("messageID"))
	if err != nil || messageID == "" || len(messageID) > 256 {
		return fiber.NewError(fiber.StatusBadRequest, "invalid source message ID")
	}
	offset, err := strconv.Atoi(c.Query("offset", "0"))
	if err != nil || offset < 0 {
		return fiber.NewError(fiber.StatusBadRequest, "history offset must be non-negative")
	}
	limit, err := strconv.Atoi(c.Query("limit", "4096"))
	if err != nil || limit < 1 || limit > store.MaxSessionHistoryReadBytes {
		return fiber.NewError(fiber.StatusBadRequest, "history limit must be between 1 and 16384 bytes")
	}
	result, err := policy.storage.ReadSessionHistory(c.Context(), store.SessionHistoryRead{
		Namespace: policy.task.Namespace, SessionName: policy.name, ThroughMessageID: policy.through,
		MessageID: messageID, Offset: offset, Limit: limit,
	})
	if err != nil {
		return sessionContextHTTPError(err)
	}
	return c.JSON(result)
}

func sessionContextHTTPError(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return fiber.NewError(fiber.StatusNotFound, "session context source not found within the allowed history")
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrDuplicateMismatch):
		return fiber.NewError(fiber.StatusConflict, "session context changed, its owner expired, or a stable ID was reused")
	case errors.Is(err, store.ErrValidation):
		return fiber.NewError(fiber.StatusBadRequest, "invalid session context data or bounds")
	case errors.Is(err, store.ErrCapacity):
		return fiber.NewError(fiber.StatusRequestEntityTooLarge, "session context exceeds its storage allowance")
	default:
		return fiber.NewError(fiber.StatusInternalServerError, "session context persistence is unavailable")
	}
}
