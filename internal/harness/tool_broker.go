package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sozercan/orka/internal/approvals"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/tools"
)

// ToolBroker executes brokered harness tool requests through Orka's central
// tool registry while preserving idempotency and execution-event observability.
type ToolBroker struct {
	Registry              *tools.Registry
	EventStore            store.ExecutionEventStore
	ToolContext           *tools.ToolContext
	AllowedTools          map[string]struct{}
	AllowInsecureLoopback bool
	Now                   func() time.Time

	mu      sync.Mutex
	results map[string]brokeredToolResultRecord
	pending map[string]string
}

type brokeredToolResultRecord struct {
	requestDigest string
	result        ToolCallResult
}

func (b *ToolBroker) Execute(ctx context.Context, req ToolCallRequest) (ToolCallResult, error) {
	if err := validateToolCallRequest(req); err != nil {
		return toolCallErrorResult(req, "invalid_request", err.Error()), nil
	}
	if len(req.Input) == 0 {
		req.Input = json.RawMessage(`{}`)
	}
	expectedKey := ToolRequestIdempotencyKey(req.RuntimeSessionID, req.TurnID, req.ToolCallID)
	if strings.TrimSpace(req.IdempotencyKey) != expectedKey {
		return toolCallErrorResult(req, "idempotency_key_mismatch", "idempotency key does not match runtime session, turn, and tool call"), nil
	}
	digest := digestToolCallRequest(req)
	if result := b.cachedOrPendingResult(req, digest); result != nil {
		return *result, nil
	}

	if b.Registry == nil {
		return b.remember(req, digest, toolCallErrorResult(req, "tool_registry_unavailable", "tool registry is not configured")), nil
	}
	if b.EventStore == nil {
		return b.remember(req, digest, toolCallErrorResult(req, "event_store_unavailable", "execution event store is not configured")), nil
	}
	if _, ok := b.AllowedTools[strings.TrimSpace(req.ToolName)]; b.AllowedTools == nil || !ok {
		result := toolCallErrorResult(req, "tool_disabled", fmt.Sprintf("tool %q is not enabled", req.ToolName))
		_ = b.appendToolEvent(ctx, req, events.ExecutionEventTypeToolCallFailed, events.ExecutionEventSeverityWarning, "brokered tool rejected", map[string]any{"errorCategory": "tool_disabled"})
		return b.remember(req, digest, result), nil
	}
	if unsafeBrokeredBuiltinTool(req.ToolName) {
		result := toolCallErrorResult(req, "tool_unsupported", fmt.Sprintf("tool %q requires a task-scoped workspace proxy before brokered execution", req.ToolName))
		_ = b.appendToolEvent(ctx, req, events.ExecutionEventTypeToolCallFailed, events.ExecutionEventSeverityWarning, "brokered tool unsupported", map[string]any{"errorCategory": "tool_unsupported"})
		return b.remember(req, digest, result), nil
	}
	if err := b.validateBrokeredToolNetworkPolicy(req); err != nil {
		result := toolCallErrorResult(req, "tool_egress_denied", err.Error())
		_ = b.appendToolEvent(ctx, req, events.ExecutionEventTypeToolCallFailed, events.ExecutionEventSeverityWarning, "brokered tool egress denied", map[string]any{"errorCategory": "egress_denied"})
		return b.remember(req, digest, result), nil
	}
	if durableConflict, err := b.durableIdempotencyConflict(ctx, req, digest); err != nil {
		return toolCallErrorResult(req, "idempotency_check_failed", err.Error()), nil
	} else if durableConflict != nil {
		return *durableConflict, nil
	}
	if req.RequiresApproval {
		approvalState, err := b.brokeredToolApprovalState(ctx, req)
		if err != nil {
			return toolCallErrorResult(req, "approval_check_failed", err.Error()), nil
		}
		switch approvalState {
		case approvals.StatusApproved:
			b.forgetPending(req.IdempotencyKey)
			// Continue to tool execution below.
		case approvals.StatusPending:
			return toolCallErrorResult(req, "approval_required", "approval is required before brokered tool execution"), nil
		case approvals.StatusDeclined, approvals.StatusExpired, approvals.StatusCancelled:
			return b.remember(req, digest, toolCallErrorResult(req, "approval_denied", "brokered tool approval is "+approvalState)), nil
		case "":
			if result := b.reservePending(req, digest); result != nil {
				return *result, nil
			}
			if err := b.appendApprovalRequest(ctx, req); err != nil {
				return b.remember(req, digest, toolCallErrorResult(req, "approval_request_failed", err.Error())), nil
			}
			return toolCallErrorResult(req, "approval_required", "approval is required before brokered tool execution"), nil
		default:
			return toolCallErrorResult(req, "approval_check_failed", "unsupported approval state "+approvalState), nil
		}
	}

	if result := b.reservePending(req, digest); result != nil {
		return *result, nil
	}
	if err := b.appendToolEvent(ctx, req, events.ExecutionEventTypeToolCallStarted, events.ExecutionEventSeverityInfo, "brokered tool call started", map[string]any{
		"toolName": req.ToolName,
	}); err != nil {
		b.forgetPending(req.IdempotencyKey)
		return toolCallErrorResult(req, "event_record_failed", "failed to persist brokered tool start event"), nil
	}
	toolCtx := ctx
	if b.ToolContext != nil {
		copy := *b.ToolContext
		copy.ToolCallID = req.ToolCallID
		if copy.Tenant == "" {
			copy.Tenant = copy.Namespace
		}
		toolCtx = tools.WithToolContext(ctx, &copy)
	}
	output, err := b.Registry.Execute(toolCtx, req.ToolName, req.Input)
	if err != nil {
		result := toolCallErrorResult(req, "tool_execution_failed", err.Error())
		if recordErr := b.appendToolEvent(ctx, req, events.ExecutionEventTypeToolCallFailed, events.ExecutionEventSeverityError, "brokered tool call failed", map[string]any{
			"errorCategory":       brokeredToolErrorCategory(err),
			"errorPreviewOmitted": true,
			"errorPreviewOmitWhy": "tool errors can contain user data, paths, URLs, or stderr",
		}); recordErr != nil {
			failure := toolCallErrorResult(req, "event_record_failed", "failed to persist brokered tool failure event")
			if fallbackErr := b.appendToolEvent(ctx, req, events.ExecutionEventTypeToolCallFailed, events.ExecutionEventSeverityError, "brokered tool failure event failed", map[string]any{
				"errorCategory": "event_record_failed",
			}); fallbackErr == nil {
				return b.remember(req, digest, failure), nil
			}
			return failure, nil
		}
		return b.remember(req, digest, result), nil
	}
	result := ToolCallResult{
		Version:          ProtocolVersion,
		RuntimeSessionID: req.RuntimeSessionID,
		TurnID:           req.TurnID,
		ToolCallID:       req.ToolCallID,
		IdempotencyKey:   req.IdempotencyKey,
		Approved:         true,
		Output:           brokeredToolOutput(output),
	}
	if err := b.appendToolEvent(ctx, req, events.ExecutionEventTypeToolCallCompleted, events.ExecutionEventSeverityInfo, "brokered tool call completed", map[string]any{
		"outputBytes": len(output),
	}); err != nil {
		failure := toolCallErrorResult(req, "event_record_failed", "failed to persist brokered tool completion event")
		if recordErr := b.appendToolEvent(ctx, req, events.ExecutionEventTypeToolCallFailed, events.ExecutionEventSeverityError, "brokered tool completion event failed", map[string]any{
			"errorCategory": "event_record_failed",
		}); recordErr == nil {
			return b.remember(req, digest, failure), nil
		}
		return failure, nil
	}
	return b.remember(req, digest, result), nil
}

func validateToolCallRequest(req ToolCallRequest) error {
	if err := validateVersion(req.Version); err != nil {
		return err
	}
	if strings.TrimSpace(string(req.RuntimeSessionID)) == "" {
		return fmt.Errorf("runtime session id is required")
	}
	if strings.TrimSpace(string(req.TurnID)) == "" {
		return fmt.Errorf("turn id is required")
	}
	if strings.TrimSpace(req.ToolCallID) == "" {
		return fmt.Errorf("tool call id is required")
	}
	if strings.TrimSpace(req.ToolName) == "" {
		return fmt.Errorf("tool name is required")
	}
	if strings.TrimSpace(req.IdempotencyKey) == "" {
		return fmt.Errorf("idempotency key is required")
	}
	if req.ApprovalPolicyRef != nil && strings.TrimSpace(req.ApprovalPolicyRef.Name) == "" {
		return fmt.Errorf("approval policy ref name is required")
	}
	if len(req.Input) == 0 {
		return nil
	}
	return nil
}

func (b *ToolBroker) appendToolEvent(ctx context.Context, req ToolCallRequest, eventType, severity, summary string, content map[string]any) error {
	if b.EventStore == nil || b.ToolContext == nil {
		return nil
	}
	body := map[string]any{
		"runtimeSessionID": req.RuntimeSessionID,
		"turnID":           req.TurnID,
		"toolCallID":       req.ToolCallID,
		"toolName":         req.ToolName,
		"idempotencyKey":   req.IdempotencyKey,
		"requestDigest":    digestToolCallRequest(req),
		"brokered":         true,
	}
	maps.Copy(body, content)
	raw, _ := json.Marshal(body)
	if len(raw) > events.MaxExecutionEventContentJSONBytes {
		compact := map[string]any{
			"requestDigest":        digestToolCallRequest(req),
			"idempotencyKeyDigest": digestString(req.IdempotencyKey),
			"toolName":             boundedBrokeredText(req.ToolName),
			"brokered":             true,
			"contentTruncated":     true,
		}
		raw, _ = json.Marshal(compact)
	}
	_, err := b.EventStore.AppendExecutionEvent(ctx, &store.ExecutionEvent{
		Namespace:   b.ToolContext.Namespace,
		StreamType:  store.ExecutionEventStreamTypeTask,
		StreamID:    b.ToolContext.TaskID,
		TaskName:    b.ToolContext.TaskID,
		SessionName: b.ToolContext.SessionID,
		ToolName:    req.ToolName,
		ToolCallID:  req.ToolCallID,
		Type:        eventType,
		Severity:    severity,
		Summary:     summary,
		Content:     raw,
		CreatedAt:   b.now(),
	})
	return err
}

func (b *ToolBroker) appendApprovalRequest(ctx context.Context, req ToolCallRequest) error {
	if b.EventStore == nil || b.ToolContext == nil {
		return fmt.Errorf("approval request requires execution event store and task context")
	}
	expires := b.now().Add(time.Hour).UTC()
	approvalID := brokeredToolApprovalID(req)
	approvalContent := map[string]any{
		"approvalID":           approvalID,
		"action":               "brokered_tool",
		"tool":                 boundedBrokeredText(req.ToolName),
		"toolCallID":           boundedBrokeredText(req.ToolCallID),
		"toolCallIDDigest":     digestString(req.ToolCallID),
		"riskSummary":          fmt.Sprintf("Brokered harness tool call %q requires approval", boundedBrokeredText(req.ToolName)),
		"requestDigest":        digestToolCallRequest(req),
		"idempotencyKey":       boundedBrokeredText(req.IdempotencyKey),
		"idempotencyKeyDigest": digestString(req.IdempotencyKey),
		"expiresAt":            expires.Format(time.RFC3339),
	}
	content, _ := json.Marshal(approvalContent)
	if len(content) > events.MaxExecutionEventContentJSONBytes {
		return fmt.Errorf("brokered approval request content exceeds event content limit")
	}
	_, err := b.EventStore.AppendExecutionEvent(ctx, &store.ExecutionEvent{
		Namespace:   b.ToolContext.Namespace,
		StreamType:  store.ExecutionEventStreamTypeTask,
		StreamID:    b.ToolContext.TaskID,
		TaskName:    b.ToolContext.TaskID,
		SessionName: b.ToolContext.SessionID,
		ToolName:    req.ToolName,
		ToolCallID:  req.ToolCallID,
		Type:        events.ExecutionEventTypeApprovalRequested,
		Severity:    events.ExecutionEventSeverityWarning,
		Summary:     "brokered tool approval requested",
		Content:     content,
		CreatedAt:   b.now(),
	})
	return err
}

func (b *ToolBroker) durableIdempotencyConflict(ctx context.Context, req ToolCallRequest, digest string) (*ToolCallResult, error) {
	if b.EventStore == nil || b.ToolContext == nil {
		return nil, nil
	}
	listed, err := b.listBrokeredToolEventsForCall(ctx, req, []string{
		events.ExecutionEventTypeToolCallStarted,
		events.ExecutionEventTypeToolCallCompleted,
		events.ExecutionEventTypeToolCallFailed,
		events.ExecutionEventTypeApprovalRequested,
	})
	if err != nil {
		return nil, err
	}
	foundStarted := false
	for _, event := range listed {
		var content map[string]any
		if json.Unmarshal(event.Content, &content) != nil || !brokeredToolEventMatchesIdempotency(content, req.IdempotencyKey) {
			continue
		}
		if stringContentField(content, "requestDigest") != digest {
			result := toolCallErrorResult(req, "idempotency_conflict", "idempotency key was already used for different input")
			return &result, nil
		}
		switch event.Type {
		case events.ExecutionEventTypeToolCallCompleted:
			result := toolCallErrorResult(req, "idempotency_already_processed", "brokered tool call was already processed")
			return &result, nil
		case events.ExecutionEventTypeToolCallFailed:
			result := toolCallErrorResult(req, "idempotency_already_processed", "brokered tool call was already processed")
			return &result, nil
		case events.ExecutionEventTypeToolCallStarted:
			foundStarted = true
		}
	}
	if foundStarted {
		result := toolCallErrorResult(req, "idempotency_in_progress", "brokered tool call is already in progress")
		return &result, nil
	}
	return nil, nil
}

func unsafeBrokeredBuiltinTool(toolName string) bool {
	switch strings.TrimSpace(toolName) {
	case "file_read", "file_write", "code_exec", "web_fetch", "web_search":
		return true
	default:
		return false
	}
}

func (b *ToolBroker) validateBrokeredToolNetworkPolicy(req ToolCallRequest) error {
	if strings.TrimSpace(req.ToolName) != "web_fetch" {
		return nil
	}
	var input struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(req.Input, &input); err != nil {
		return fmt.Errorf("web_fetch input URL is required")
	}
	parsed, err := url.Parse(strings.TrimSpace(input.URL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("web_fetch URL must be absolute")
	}
	if parsed.User != nil {
		return fmt.Errorf("web_fetch URL must not include user info")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("web_fetch URL scheme must be http or https")
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if b.AllowInsecureLoopback && isLoopbackBrokerHost(host) {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return fmt.Errorf("web_fetch URL host is not allowed for brokered controller-side execution")
		}
		return nil
	}
	if host == localhostName || strings.HasSuffix(host, "."+localhostName) || strings.HasSuffix(host, ".local") ||
		strings.Contains(host, ".svc") || strings.HasSuffix(host, ".cluster.local") {
		return fmt.Errorf("web_fetch URL host is not allowed for brokered controller-side execution")
	}
	return nil
}

func isLoopbackBrokerHost(host string) bool {
	if host == localhostName || strings.HasSuffix(host, "."+localhostName) {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func (b *ToolBroker) brokeredToolApprovalState(ctx context.Context, req ToolCallRequest) (string, error) {
	if b.EventStore == nil || b.ToolContext == nil {
		return "", fmt.Errorf("approval check requires execution event store and task context")
	}
	listed, err := b.listBrokeredToolEventsForCall(ctx, req, []string{
		events.ExecutionEventTypeApprovalRequested,
		events.ExecutionEventTypeApprovalApproved,
		events.ExecutionEventTypeApprovalDeclined,
		events.ExecutionEventTypeApprovalExpired,
		events.ExecutionEventTypeApprovalCancelled,
	})
	if err != nil {
		return "", err
	}
	approvalID := brokeredToolApprovalID(req)
	decisionEvents, err := b.listBrokeredToolEventsForToolCall(ctx, approvalID, []string{
		events.ExecutionEventTypeApprovalApproved,
		events.ExecutionEventTypeApprovalDeclined,
		events.ExecutionEventTypeApprovalExpired,
		events.ExecutionEventTypeApprovalCancelled,
	})
	if err != nil {
		return "", err
	}
	listed = appendUniqueBrokeredToolEvents(listed, decisionEvents)
	requestDigest := digestToolCallRequest(req)
	requestFound := false
	for _, event := range listed {
		if event.Type != events.ExecutionEventTypeApprovalRequested {
			continue
		}
		var content map[string]any
		if json.Unmarshal(event.Content, &content) == nil &&
			stringContentField(content, "approvalID") == approvalID &&
			stringContentField(content, "requestDigest") == requestDigest {
			requestFound = true
			break
		}
	}
	if !requestFound {
		return "", nil
	}
	for _, approval := range approvals.Derive(listed, time.Time{}) {
		if approval.ID == approvalID {
			return approval.Status, nil
		}
	}
	return approvals.StatusPending, nil
}

func (b *ToolBroker) listBrokeredToolEventsForCall(ctx context.Context, req ToolCallRequest, eventTypes []string) ([]store.ExecutionEvent, error) {
	return b.listBrokeredToolEventsForToolCall(ctx, req.ToolCallID, eventTypes)
}

func (b *ToolBroker) listBrokeredToolEventsForToolCall(ctx context.Context, toolCallID string, eventTypes []string) ([]store.ExecutionEvent, error) {
	out := []store.ExecutionEvent{}
	var after int64
	for {
		batch, err := b.EventStore.ListExecutionEvents(ctx, store.ExecutionEventFilter{
			Namespace:  b.ToolContext.Namespace,
			StreamType: store.ExecutionEventStreamTypeTask,
			StreamID:   b.ToolContext.TaskID,
			EventTypes: eventTypes,
			ToolCallID: toolCallID,
			AfterSeq:   after,
			Limit:      store.MaxExecutionEventLimit,
		})
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			return out, nil
		}
		out = append(out, batch...)
		after = batch[len(batch)-1].Seq
		if len(batch) < store.MaxExecutionEventLimit {
			return out, nil
		}
	}
}

func appendUniqueBrokeredToolEvents(base []store.ExecutionEvent, extra []store.ExecutionEvent) []store.ExecutionEvent {
	seen := make(map[string]struct{}, len(base)+len(extra))
	for _, event := range base {
		seen[brokeredToolEventKey(event)] = struct{}{}
	}
	for _, event := range extra {
		key := brokeredToolEventKey(event)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		base = append(base, event)
	}
	return base
}

func brokeredToolEventKey(event store.ExecutionEvent) string {
	if strings.TrimSpace(event.ID) != "" {
		return event.ID
	}
	return fmt.Sprintf("%s/%s/%s/%d/%s/%s", event.Namespace, event.StreamType, event.StreamID, event.Seq, event.Type, event.ToolCallID)
}

func (b *ToolBroker) cachedOrPendingResult(req ToolCallRequest, digest string) *ToolCallResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.results != nil {
		if record, ok := b.results[req.IdempotencyKey]; ok {
			if record.requestDigest == digest {
				result := cloneToolCallResult(record.result)
				return &result
			}
			result := toolCallErrorResult(record.resultIdentity(), "idempotency_conflict", "idempotency key was already used for different input")
			return &result
		}
	}
	pendingDigest, pending := b.pending[req.IdempotencyKey]
	if !pending {
		return nil
	}
	if pendingDigest != digest {
		result := toolCallErrorResult(req, "idempotency_conflict", "idempotency key was already used for different input")
		return &result
	}
	if req.RequiresApproval {
		return nil
	}
	result := toolCallErrorResult(req, "idempotency_in_progress", "brokered tool call is already in progress")
	return &result
}

func (b *ToolBroker) reservePending(req ToolCallRequest, digest string) *ToolCallResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pending == nil {
		b.pending = map[string]string{}
	}
	if pendingDigest, ok := b.pending[req.IdempotencyKey]; ok {
		if pendingDigest != digest {
			result := toolCallErrorResult(req, "idempotency_conflict", "idempotency key was already used for different input")
			return &result
		}
		code := "idempotency_in_progress"
		message := "brokered tool call is already in progress"
		if req.RequiresApproval {
			code = "approval_required"
			message = "approval is required before brokered tool execution"
		}
		result := toolCallErrorResult(req, code, message)
		return &result
	}
	b.pending[req.IdempotencyKey] = digest
	return nil
}

func (r brokeredToolResultRecord) resultIdentity() ToolCallRequest {
	return ToolCallRequest{Version: ProtocolVersion, RuntimeSessionID: r.result.RuntimeSessionID, TurnID: r.result.TurnID, ToolCallID: r.result.ToolCallID, IdempotencyKey: r.result.IdempotencyKey}
}

func (b *ToolBroker) forgetPending(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.pending, key)
}

func (b *ToolBroker) remember(req ToolCallRequest, digest string, result ToolCallResult) ToolCallResult {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.results == nil {
		b.results = map[string]brokeredToolResultRecord{}
	}
	delete(b.pending, req.IdempotencyKey)
	copy := cloneToolCallResult(result)
	b.results[req.IdempotencyKey] = brokeredToolResultRecord{requestDigest: digest, result: copy}
	return cloneToolCallResult(copy)
}

func brokeredToolEventMatchesIdempotency(content map[string]any, idempotencyKey string) bool {
	if idempotencyKey == "" {
		return false
	}
	if raw := stringContentField(content, "idempotencyKey"); raw != "" && raw == idempotencyKey {
		return true
	}
	return stringContentField(content, "idempotencyKeyDigest") == digestString(idempotencyKey)
}

func boundedBrokeredText(value string) string {
	const maxChars = 1024
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= maxChars {
		return string(runes)
	}
	return string(runes[:maxChars]) + "...[truncated]"
}

func digestString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func brokeredToolErrorCategory(err error) string {
	if err == nil {
		return "none"
	}
	normalized := strings.ToLower(err.Error())
	switch {
	case strings.Contains(normalized, "not found"):
		return "not_found"
	case strings.Contains(normalized, "access denied"), strings.Contains(normalized, "permission"):
		return "permission"
	case strings.Contains(normalized, "timeout"), strings.Contains(normalized, "timed out"):
		return "timeout"
	default:
		return "execution_error"
	}
}

func (b *ToolBroker) now() time.Time {
	if b.Now != nil {
		return b.Now().UTC()
	}
	return time.Now().UTC()
}

func toolCallErrorResult(req ToolCallRequest, code, message string) ToolCallResult {
	return ToolCallResult{Version: ProtocolVersion, RuntimeSessionID: req.RuntimeSessionID, TurnID: req.TurnID, ToolCallID: req.ToolCallID, IdempotencyKey: req.IdempotencyKey, Error: &ErrorInfo{Code: code, Message: events.RedactExecutionEventText(message)}}
}

func brokeredToolApprovalID(req ToolCallRequest) string {
	return "broker-" + digestToolCallRequest(req)[:20]
}

func stringContentField(content map[string]any, key string) string {
	value, _ := content[key].(string)
	return strings.TrimSpace(value)
}

func brokeredToolOutput(output string) json.RawMessage {
	trimmed := strings.TrimSpace(output)
	if json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	encoded, _ := json.Marshal(output)
	return encoded
}

func digestToolCallRequest(req ToolCallRequest) string {
	body := map[string]any{
		"runtimeSessionID": req.RuntimeSessionID,
		"turnID":           req.TurnID,
		"toolCallID":       req.ToolCallID,
		"toolName":         req.ToolName,
		"input":            req.Input,
		"requiresApproval": req.RequiresApproval,
	}
	data, _ := json.Marshal(body)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func cloneToolCallResult(result ToolCallResult) ToolCallResult {
	if result.Output != nil {
		result.Output = append(json.RawMessage(nil), result.Output...)
	}
	if result.Error != nil {
		errCopy := *result.Error
		result.Error = &errCopy
	}
	return result
}
