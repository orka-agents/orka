package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/harness"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/tools"
)

// BrokerHarnessTool handles POST /internal/v1/harness/tools/{namespace}/{taskName}.
// Harness runtimes use this path for brokered tool execution through Orka.
func (h *InternalHandlers) BrokerHarnessTool(c fiber.Ctx) error {
	namespace := strings.TrimSpace(c.Params("namespace"))
	taskName := strings.TrimSpace(c.Params("taskName"))
	if namespace == "" || taskName == "" {
		return fiber.NewError(fiber.StatusBadRequest, "namespace and taskName are required")
	}
	if err := h.internalCallerAuthorizer().verifyNamespace(c, namespace); err != nil {
		return err
	}
	if h.executionEventStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "execution event storage not enabled")
	}
	var req harness.ToolCallRequest
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	writerTask, err := h.verifyBrokeredToolCaller(c, namespace, taskName, req)
	if err != nil {
		return err
	}
	if err := verifyActiveBrokeredToolTurn(writerTask, req); err != nil {
		return err
	}
	taskUID := brokeredToolTaskUID(writerTask)
	cached, ok, err := h.cachedBrokeredToolResult(namespace, taskName, taskUID, req)
	if err != nil {
		return err
	}
	if ok {
		c.Set("Content-Type", "application/json")
		return c.Send(cached)
	}
	allowedTools, err := allowedBrokeredToolsForTask(writerTask)
	if err != nil {
		return err
	}
	registry := tools.NewRegistry()
	registerControllerSafeBrokeredTools(registry)
	if pending := h.reserveBrokeredToolRequest(namespace, taskName, taskUID, req); pending != nil {
		c.Set("Content-Type", "application/json")
		return c.Send(pending)
	}
	broker := harness.ToolBroker{
		Registry:              registry,
		EventStore:            h.executionEventStore,
		AllowedTools:          allowedTools,
		AllowInsecureLoopback: h.allowInsecureBrokerLoopback,
		ToolContext: &tools.ToolContext{
			Client:                    h.k8sClient,
			Namespace:                 namespace,
			Tenant:                    namespace,
			WatchNamespace:            namespace,
			EnforceNamespaceIsolation: true,
			SessionID:                 sessionNameForTask(writerTask),
			TaskID:                    taskName,
			ToolCallID:                req.ToolCallID,
			ExecutionEventStore:       h.executionEventStore,
		},
	}
	result, err := broker.Execute(c.Context(), req)
	if err != nil {
		h.forgetBrokeredToolPending(namespace, taskName, taskUID, req)
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to execute brokered tool: %v", err))
	}
	data, err := json.Marshal(result)
	if err != nil {
		h.forgetBrokeredToolPending(namespace, taskName, taskUID, req)
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to encode brokered tool result: %v", err))
	}
	h.rememberBrokeredToolResult(namespace, taskName, taskUID, req, data, shouldCacheBrokeredToolResult(result))
	c.Set("Content-Type", "application/json")
	return c.Send(data)
}

func (h *InternalHandlers) verifyBrokeredToolCaller(
	c fiber.Ctx,
	namespace string,
	taskName string,
	_ harness.ToolCallRequest,
) (*corev1alpha1.Task, error) {
	if h.k8sClient != nil {
		task := &corev1alpha1.Task{}
		if err := h.k8sClient.Get(c.Context(), types.NamespacedName{Namespace: namespace, Name: taskName}, task); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, fiber.NewError(fiber.StatusNotFound, "task not found")
			}
			return nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to get task: %v", err))
		}
		if brokeredToolCallRequiresUnsupportedHarnessProvider(task) {
			return nil, fiber.NewError(fiber.StatusForbidden, "service harness brokered tool execution requires trusted provider identity and is not supported yet")
		}
	}
	return h.internalCallerAuthorizer().verifyExecutionEventStreamWriter(c, namespace, events.ExecutionEventStreamTypeTask, taskName)
}

func brokeredToolCallRequiresUnsupportedHarnessProvider(task *corev1alpha1.Task) bool {
	if task == nil || task.Annotations == nil || strings.TrimSpace(task.Annotations[labels.AnnotationHarnessBrokeredTools]) == "" {
		return false
	}
	return strings.TrimSpace(task.Annotations[labels.AnnotationHarnessEndpoint]) != "" || task.Spec.Type == corev1alpha1.TaskTypeAgent
}

func verifyActiveBrokeredToolTurn(task *corev1alpha1.Task, req harness.ToolCallRequest) error {
	if task == nil || task.Annotations == nil {
		return fiber.NewError(fiber.StatusForbidden, "brokered tool execution requires an active harness turn")
	}
	if strings.TrimSpace(task.Annotations[labels.AnnotationHarnessBrokeredTools]) == "" {
		return fiber.NewError(fiber.StatusForbidden, "brokered tool execution is not enabled for this task")
	}
	runtimeSessionID := strings.TrimSpace(task.Annotations[labels.AnnotationHarnessRuntimeSession])
	turnID := strings.TrimSpace(task.Annotations[labels.AnnotationHarnessTurn])
	if runtimeSessionID == "" || turnID == "" || strings.TrimSpace(task.Annotations[labels.AnnotationHarnessTurnStartedAt]) == "" {
		return fiber.NewError(fiber.StatusForbidden, "brokered tool execution requires an active harness turn")
	}
	if runtimeSessionID != strings.TrimSpace(string(req.RuntimeSessionID)) || turnID != strings.TrimSpace(string(req.TurnID)) {
		return fiber.NewError(fiber.StatusForbidden, "brokered tool request does not match active harness turn")
	}
	return nil
}

func registerControllerSafeBrokeredTools(registry *tools.Registry) {
	if registry == nil {
		return
	}
	registry.Register(&tools.ListToolsTool{})
}

func allowedBrokeredToolsForTask(task *corev1alpha1.Task) (map[string]struct{}, error) {
	if task == nil {
		return nil, fiber.NewError(fiber.StatusNotImplemented, "brokered tool authorization requires Kubernetes task identity")
	}
	allowed := map[string]struct{}{}
	for item := range strings.SplitSeq(task.Annotations[labels.AnnotationHarnessBrokeredTools], ",") {
		name := strings.TrimSpace(item)
		if name != "" {
			allowed[name] = struct{}{}
		}
	}
	return allowed, nil
}

func brokeredToolTaskUID(task *corev1alpha1.Task) string {
	if task == nil {
		return ""
	}
	return strings.TrimSpace(string(task.UID))
}

func brokeredToolHTTPKey(namespace, taskName, taskUID string, req harness.ToolCallRequest) string {
	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" {
		return ""
	}
	uid := strings.TrimSpace(taskUID)
	if uid == "" {
		uid = "-"
	}
	return strings.TrimSpace(namespace) + "/" + strings.TrimSpace(taskName) + "/" + uid + ":" + key
}

func (h *InternalHandlers) forgetBrokeredToolPending(namespace, taskName, taskUID string, req harness.ToolCallRequest) {
	key := brokeredToolHTTPKey(namespace, taskName, taskUID, req)
	if key == "" {
		return
	}
	h.brokeredToolMu.Lock()
	defer h.brokeredToolMu.Unlock()
	delete(h.brokeredToolPending, key)
}

func (h *InternalHandlers) cachedBrokeredToolResult(namespace, taskName, taskUID string, req harness.ToolCallRequest) ([]byte, bool, error) {
	key := brokeredToolHTTPKey(namespace, taskName, taskUID, req)
	if key == "" {
		return nil, false, nil
	}
	digest := brokeredToolHTTPRequestDigest(req)
	h.brokeredToolMu.Lock()
	defer h.brokeredToolMu.Unlock()
	record, ok := h.brokeredToolResults[key]
	if !ok {
		return nil, false, nil
	}
	if record.RequestDigest != digest {
		result := harness.ToolCallResult{
			Version:          harness.ProtocolVersion,
			RuntimeSessionID: req.RuntimeSessionID,
			TurnID:           req.TurnID,
			ToolCallID:       req.ToolCallID,
			IdempotencyKey:   req.IdempotencyKey,
			Error:            &harness.ErrorInfo{Code: "idempotency_conflict", Message: "idempotency key was already used for different input"},
		}
		data, _ := json.Marshal(result)
		return data, true, nil
	}
	if len(record.Result) == 0 {
		return nil, false, nil
	}
	return append([]byte(nil), record.Result...), true, nil
}

func (h *InternalHandlers) reserveBrokeredToolRequest(namespace, taskName, taskUID string, req harness.ToolCallRequest) []byte {
	key := brokeredToolHTTPKey(namespace, taskName, taskUID, req)
	if key == "" {
		return nil
	}
	digest := brokeredToolHTTPRequestDigest(req)
	h.brokeredToolMu.Lock()
	defer h.brokeredToolMu.Unlock()
	if h.brokeredToolPending == nil {
		h.brokeredToolPending = map[string]string{}
	}
	if pendingDigest, ok := h.brokeredToolPending[key]; ok {
		code := "idempotency_in_progress"
		message := "brokered tool call is already in progress"
		if pendingDigest != digest {
			code = "idempotency_conflict"
			message = "idempotency key was already used for different input"
		}
		result := harness.ToolCallResult{
			Version:          harness.ProtocolVersion,
			RuntimeSessionID: req.RuntimeSessionID,
			TurnID:           req.TurnID,
			ToolCallID:       req.ToolCallID,
			IdempotencyKey:   req.IdempotencyKey,
			Error:            &harness.ErrorInfo{Code: code, Message: message},
		}
		data, _ := json.Marshal(result)
		return data
	}
	if _, exists := h.brokeredToolPending[key]; !exists && len(h.brokeredToolPending) >= maxBrokeredToolHTTPCacheEntries {
		result := harness.ToolCallResult{
			Version:          harness.ProtocolVersion,
			RuntimeSessionID: req.RuntimeSessionID,
			TurnID:           req.TurnID,
			ToolCallID:       req.ToolCallID,
			IdempotencyKey:   req.IdempotencyKey,
			Error:            &harness.ErrorInfo{Code: "brokered_tool_capacity_exceeded", Message: "too many brokered tool calls are in progress"},
		}
		data, _ := json.Marshal(result)
		return data
	}
	h.brokeredToolPending[key] = digest
	return nil
}

const maxBrokeredToolHTTPCacheEntries = 128

func (h *InternalHandlers) rememberBrokeredToolResult(namespace, taskName, taskUID string, req harness.ToolCallRequest, data []byte, cacheResponse bool) {
	key := brokeredToolHTTPKey(namespace, taskName, taskUID, req)
	if key == "" {
		return
	}
	h.brokeredToolMu.Lock()
	defer h.brokeredToolMu.Unlock()
	if h.brokeredToolResults == nil {
		h.brokeredToolResults = map[string]brokeredToolHTTPRecord{}
	}
	delete(h.brokeredToolPending, key)
	if _, exists := h.brokeredToolResults[key]; !exists && len(h.brokeredToolResults) >= maxBrokeredToolHTTPCacheEntries {
		for existingKey := range h.brokeredToolResults {
			delete(h.brokeredToolResults, existingKey)
			break
		}
	}
	if existing, ok := h.brokeredToolResults[key]; ok && len(existing.Result) > 0 {
		return
	}
	var result json.RawMessage
	if cacheResponse {
		result = append(json.RawMessage(nil), data...)
	}
	h.brokeredToolResults[key] = brokeredToolHTTPRecord{RequestDigest: brokeredToolHTTPRequestDigest(req), Result: result}
}

func brokeredToolHTTPRequestDigest(req harness.ToolCallRequest) string {
	copy := map[string]any{
		"runtimeSessionID": req.RuntimeSessionID,
		"turnID":           req.TurnID,
		"toolCallID":       req.ToolCallID,
		"toolName":         req.ToolName,
		"idempotencyKey":   req.IdempotencyKey,
		"input":            req.Input,
		"requiresApproval": req.RequiresApproval,
	}
	if len(req.Metadata) > 0 {
		metadata := map[string]string{}
		maps.Copy(metadata, req.Metadata)
		copy["metadata"] = metadata
	}
	data, _ := json.Marshal(copy)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func shouldCacheBrokeredToolResult(result harness.ToolCallResult) bool {
	if result.Error == nil {
		return true
	}
	switch result.Error.Code {
	case "approval_required",
		"approval_check_failed",
		"approval_request_failed",
		"event_record_failed",
		"idempotency_check_failed":
		return false
	default:
		return true
	}
}
