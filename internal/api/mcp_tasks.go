/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

//nolint:goconst // Keep MCP wire field names and JSON Schema keywords literal to match the protocol.
package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
)

const mcpTaskIDPrefix = "orka.v1."

type mcpTaskRef struct {
	Namespace string `json:"ns"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

func encodeMCPTaskID(namespace, name, uid string) string {
	if uid == "" {
		return ""
	}
	data, _ := json.Marshal(mcpTaskRef{Namespace: namespace, Name: name, UID: uid})
	return mcpTaskIDPrefix + base64.RawURLEncoding.EncodeToString(data)
}

func decodeMCPTaskID(id string) (mcpTaskRef, error) {
	invalid := newMCPError(mcpCodeInvalidParams, "invalid taskId")
	encoded, ok := strings.CutPrefix(id, mcpTaskIDPrefix)
	if !ok || len(encoded) > 1024 {
		return mcpTaskRef{}, invalid
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return mcpTaskRef{}, invalid
	}
	var ref mcpTaskRef
	if err := json.Unmarshal(data, &ref); err != nil {
		return mcpTaskRef{}, invalid
	}
	if len(validation.IsDNS1123Label(ref.Namespace)) > 0 || len(validation.IsDNS1123Subdomain(ref.Name)) > 0 ||
		ref.UID == "" || len(ref.UID) > 128 {
		return mcpTaskRef{}, invalid
	}
	return ref, nil
}

type mcpTaskParams struct {
	TaskID         string                     `json:"taskId"`
	InputResponses map[string]json.RawMessage `json:"inputResponses,omitempty"`
}

func decodeMCPTaskParams(raw json.RawMessage) (mcpTaskParams, mcpTaskRef, error) {
	var params mcpTaskParams
	if len(raw) == 0 || json.Unmarshal(raw, &params) != nil {
		return params, mcpTaskRef{}, newMCPError(mcpCodeInvalidParams, "taskId is required")
	}
	ref, err := decodeMCPTaskID(params.TaskID)
	return params, ref, err
}

func mcpTaskRPCError(err error) error {
	var restErr *mcpRESTError
	if !errors.As(err, &restErr) {
		return err
	}
	switch restErr.Status {
	case fiber.StatusUnauthorized, fiber.StatusForbidden:
		return newMCPError(mcpCodeForbidden, restErr.Message)
	case fiber.StatusNotFound, fiber.StatusGone:
		return newMCPError(mcpCodeInvalidParams, "task not found")
	case fiber.StatusBadRequest, fiber.StatusConflict:
		return newMCPError(mcpCodeInvalidParams, restErr.Message)
	case fiber.StatusTooManyRequests:
		return newMCPError(mcpCodeServerBusy, restErr.Message)
	default:
		return newMCPError(mcpCodeInternalError, restErr.Message)
	}
}

func (m *mcpServer) resolveMCPTask(c fiber.Ctx, ref mcpTaskRef) (mcpTaskState, error) {
	state, err := m.loadTaskState(c, ref.Namespace, ref.Name)
	if err != nil {
		return state, mcpTaskRPCError(err)
	}
	if string(state.response.UID) != ref.UID {
		return state, newMCPError(mcpCodeInvalidParams, "task not found")
	}
	return state, nil
}

func timePointer(value *metav1.Time) *time.Time {
	if value == nil {
		return nil
	}
	return &value.Time
}

func (s mcpTaskState) lastUpdated() string {
	task := &s.response.Task
	latest := task.CreationTimestamp.Time
	for _, value := range []*time.Time{timePointer(task.Status.StartTime), timePointer(task.Status.CompletionTime)} {
		if value != nil && value.After(latest) {
			latest = *value
		}
	}
	for _, condition := range task.Status.Conditions {
		if condition.LastTransitionTime.After(latest) {
			latest = condition.LastTransitionTime.Time
		}
	}
	for _, approval := range s.approvals {
		if approval.CreatedAt.After(latest) {
			latest = approval.CreatedAt
		}
		if approval.DecisionTime != nil && approval.DecisionTime.After(latest) {
			latest = *approval.DecisionTime
		}
	}
	return mcpTimeValue(latest)
}

func (s mcpTaskState) statusMessage() string {
	if pending := s.pending(); pending > 0 && s.status() == mcpStatusInputRequired {
		return "waiting for a human decision on an approval request; use list_pending_approvals and decide_approval"
	}
	return mcpSafeText(s.response.Status.Message, mcpMaxStatusMessage)
}

func (s mcpTaskState) coreTaskFields() map[string]any {
	fields := map[string]any{
		"taskId":          encodeMCPTaskID(s.response.Namespace, s.response.Name, string(s.response.UID)),
		"status":          s.status(),
		mcpFieldCreatedAt: mcpTimeValue(s.response.CreationTimestamp.Time),
		"lastUpdatedAt":   s.lastUpdated(),
		"ttl":             nil,
		"pollInterval":    mcpTaskPollIntervalMS,
	}
	if message := s.statusMessage(); message != "" {
		fields["statusMessage"] = message
	}
	return fields
}

func (s mcpTaskState) extensionStatus() string {
	if status := s.status(); status != mcpStatusFailed {
		return status
	}
	return mcpStatusCompleted
}

func (s mcpTaskState) extensionTaskFields(resultType string) map[string]any {
	fields := map[string]any{
		"resultType":      resultType,
		"taskId":          encodeMCPTaskID(s.response.Namespace, s.response.Name, string(s.response.UID)),
		"status":          s.extensionStatus(),
		mcpFieldCreatedAt: mcpTimeValue(s.response.CreationTimestamp.Time),
		"lastUpdatedAt":   s.lastUpdated(),
		"ttlMs":           nil,
		"pollIntervalMs":  mcpTaskPollIntervalMS,
	}
	if message := s.statusMessage(); message != "" {
		fields["statusMessage"] = message
	}
	return fields
}

func (m *mcpServer) terminalToolResult(c fiber.Ctx, state mcpTaskState) (mcpToolResult, error) {
	value := map[string]any{"task": state.summary(false)}
	if state.response.Status.ResultRef != nil && state.response.Status.ResultRef.Available {
		var response struct {
			Result string `json:"result"`
		}
		err := m.restJSON(c, fiber.MethodGet, mcpTaskPath(state.response.Name, "result"),
			mcpTaskUIDQuery(state.response.Namespace, string(state.response.UID)), nil, &response)
		var restErr *mcpRESTError
		switch {
		case err == nil:
			value["result"] = mcpOutputChunk(response.Result, 0, mcpDefaultOutputBytes)
		case errors.Is(err, errMCPResponseTooLarge):
			value["resultUnavailable"] = "the result is too large to read through MCP; read it with GET " +
				mcpTaskPath(state.response.Name, "result") + "?namespace=" + state.response.Namespace
		case errors.As(err, &restErr) && restErr.Status == fiber.StatusNotFound && restErr.Message != "task not found":
		default:
			return mcpToolResult{}, mcpTaskRPCError(err)
		}
	}
	result := mcpStructuredResult(value)
	result.IsError = state.status() != mcpStatusCompleted
	return result, nil
}

var mcpApprovalInputSchema = map[string]any{
	"type": mcpSchemaTypeObject,
	"properties": map[string]any{
		mcpFieldDecision: map[string]any{
			"type": "string", "title": "Decision", "enum": []string{approvalDecisionApprove, approvalDecisionDecline},
			"enumNames": []string{"Approve", "Decline"},
		},
		mcpFieldReason: map[string]any{"type": "string", "title": "Reason", "maxLength": mcpMaxReasonLength},
	},
	"required": []string{mcpFieldDecision},
}

func mcpApprovalInputMessage(approval approvals.Approval) string {
	var message strings.Builder
	message.WriteString("Orka Agent requests approval")
	if approval.TargetTool != "" {
		message.WriteString(" to run " + approval.TargetTool)
	}
	if action := mcpSafeText(approval.Action, 512); action != "" {
		message.WriteString(": " + action)
	}
	if risk := mcpSafeText(approval.RiskSummary, 512); risk != "" {
		message.WriteString("\nRisk: " + risk)
	}
	if preview := summarizeMCPApproval(approval).TargetArgsPreview; len(preview) > 0 {
		message.WriteString("\nInputs: " + string(preview))
	}
	return message.String()
}

func (s mcpTaskState) inputRequests() map[string]any {
	requests := map[string]any{}
	for _, approval := range s.approvals {
		if approval.Status != approvals.StatusPending {
			continue
		}
		requests[approval.ID] = map[string]any{
			"method": "elicitation/create",
			"params": map[string]any{
				"mode": "form", "message": mcpApprovalInputMessage(approval), "requestedSchema": mcpApprovalInputSchema,
			},
		}
	}
	return requests
}

func (m *mcpServer) createTaskAsMCPTask(call *mcpCall, params mcpToolCallParams) (any, error) {
	state, err := m.createTask(call, params.Arguments)
	if err == nil {
		err = m.awaitCreatedTask(call.c, state)
	}
	if err != nil {
		if params.Task != nil {
			return nil, mcpCreateTaskRPCError(err)
		}
		return mcpToolErrorResult(err)
	}
	if params.Task != nil {
		return map[string]any{"task": state.coreTaskFields()}, nil
	}
	return state.extensionTaskFields("task"), nil
}

func mcpCreateTaskRPCError(err error) error {
	var argErr mcpArgumentError
	var restErr *mcpRESTError
	switch {
	case errors.As(err, &argErr):
		return newMCPError(mcpCodeInvalidParams, mcpSafeText(argErr.Error(), 512))
	case !errors.As(err, &restErr):
		return err
	}
	switch restErr.Status {
	case fiber.StatusUnauthorized, fiber.StatusForbidden:
		return newMCPError(mcpCodeForbidden, restErr.Message)
	case fiber.StatusBadRequest, fiber.StatusNotFound, fiber.StatusConflict, fiber.StatusUnprocessableEntity:
		return newMCPError(mcpCodeInvalidParams, restErr.Message)
	case fiber.StatusTooManyRequests:
		return newMCPError(mcpCodeServerBusy, restErr.Message)
	default:
		return newMCPError(mcpCodeInternalError, restErr.Message)
	}
}

const mcpCreatedTaskWait = 5 * time.Second

func (m *mcpServer) awaitCreatedTask(c fiber.Ctx, created mcpTaskState) error {
	task := &created.response.Task
	unreadable := &mcpRESTError{Status: fiber.StatusServiceUnavailable, Message: mcpSafeText(
		"task "+task.Name+" in namespace "+task.Namespace+" was created but cannot be read yet; read it with get_task instead of creating it again", 512)}
	deadline := time.Now().Add(m.createdTaskWait)
	c.Locals(mcpReplayDeadlineKey{}, deadline)
	defer c.Locals(mcpReplayDeadlineKey{}, nil)
	for backoff := 25 * time.Millisecond; ; backoff = min(2*backoff, 500*time.Millisecond) {
		var read mcpTaskState
		err := m.restJSON(c, fiber.MethodGet, mcpTaskPath(task.Name), mcpNamespaceQuery(task.Namespace), nil, &read.response)
		var restErr *mcpRESTError
		remaining := time.Until(deadline)
		switch {
		case err == nil && read.response.UID == task.UID:
			return nil
		case remaining <= 0:
			return unreadable
		case err == nil, errors.As(err, &restErr) && restErr.Status == fiber.StatusNotFound:
		default:
			return nil
		}
		timer := time.NewTimer(min(backoff, remaining))
		select {
		case <-timer.C:
		case <-c.RequestCtx().Done():
			timer.Stop()
			return unreadable
		}
	}
}

func (m *mcpServer) getTask(call *mcpCall) (any, error) {
	_, ref, err := decodeMCPTaskParams(call.params)
	if err != nil {
		return nil, err
	}
	state, err := m.resolveMCPTask(call.c, ref)
	if err != nil {
		return nil, err
	}
	if !call.tasksExtension {
		return state.coreTaskFields(), nil
	}
	fields := state.extensionTaskFields("complete")
	switch fields["status"] {
	case mcpStatusInputRequired:
		fields["inputRequests"] = state.inputRequests()
	case mcpStatusCompleted:
		result, err := m.terminalToolResult(call.c, state)
		if err != nil {
			return nil, err
		}
		fields["result"] = result
	}
	return fields, nil
}

func (m *mcpServer) taskResult(call *mcpCall) (any, error) {
	_, ref, err := decodeMCPTaskParams(call.params)
	if err != nil {
		return nil, err
	}
	state, _, err := m.waitForTerminal(call, ref)
	if err != nil {
		return nil, err
	}
	if !mcpTaskTerminal(state.response.Status.Phase) && state.status() != mcpStatusCancelled {
		return nil, newMCPError(mcpCodeServerBusy, "task has not finished; poll tasks/get and request the result again")
	}
	result, err := m.terminalToolResult(call.c, state)
	if err != nil {
		if mcpReplayDeadlinePassed(call.c) {
			return nil, newMCPError(mcpCodeServerBusy, "task has finished but its result was not read before the wait ended; request the result again")
		}
		return nil, err
	}
	result.Meta = map[string]any{mcpMetaRelatedTask: map[string]any{"taskId": encodeMCPTaskID(ref.Namespace, ref.Name, ref.UID)}}
	return result, nil
}

func (m *mcpServer) waitForTerminal(call *mcpCall, ref mcpTaskRef) (mcpTaskState, bool, error) {
	state, timedOut, err := m.pollTask(call, ref.Namespace, ref.Name, ref.UID, m.resultWait, func(state mcpTaskState) bool {
		status := state.status()
		return status != mcpStatusWorking && status != mcpStatusInputRequired
	})
	if err != nil {
		return state, false, mcpTaskRPCError(err)
	}
	return state, timedOut, nil
}

func (m *mcpServer) updateTask(call *mcpCall) (any, error) {
	if !call.tasksExtension {
		return nil, &mcpError{Code: mcpCodeMissingCapabilities, Message: "tasks/update requires the " + mcpTasksExtension + " extension",
			Data: map[string]any{"requiredCapabilities": map[string]any{"extensions": map[string]any{mcpTasksExtension: map[string]any{}}}}}
	}
	params, ref, err := decodeMCPTaskParams(call.params)
	if err != nil {
		return nil, err
	}
	if len(params.InputResponses) == 0 {
		return nil, newMCPError(mcpCodeInvalidParams, "inputResponses is required")
	}
	if len(params.InputResponses) > mcpMaxListLimit {
		return nil, newMCPError(mcpCodeInvalidParams, "too many inputResponses")
	}
	state, err := m.resolveMCPTask(call.c, ref)
	if err != nil {
		return nil, err
	}
	statuses := map[string]string{}
	for _, approval := range state.approvals {
		statuses[approval.ID] = approval.Status
	}
	type decision struct{ id, decision, reason string }
	var decisions []decision
	for id, raw := range params.InputResponses {
		status, known := statuses[id]
		if !known || validateMCPApprovalID(id) != nil {
			return nil, newMCPError(mcpCodeInvalidParams, "input response does not match an approval request")
		}
		var response struct {
			Action  string `json:"action"`
			Content struct {
				Decision string `json:"decision"`
				Reason   string `json:"reason"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &response); err != nil {
			return nil, newMCPError(mcpCodeInvalidParams, "input response must be an elicitation result")
		}
		if utf8.RuneCountInString(response.Content.Reason) > mcpMaxReasonLength {
			return nil, newMCPError(mcpCodeInvalidParams, "reason is too long")
		}
		var choice string
		switch response.Action {
		case "accept":
			if response.Content.Decision != approvalDecisionApprove && response.Content.Decision != approvalDecisionDecline {
				return nil, newMCPError(mcpCodeInvalidParams, "accepted input must choose approve or decline")
			}
			choice = response.Content.Decision
		case "decline":
			choice = approvalDecisionDecline
		case "cancel":
			continue
		default:
			return nil, newMCPError(mcpCodeInvalidParams, "unsupported elicitation action")
		}
		if status != approvals.StatusPending && !approvalStatusMatchesDecision(status, choice) {
			return nil, newMCPError(mcpCodeInvalidParams, "input response conflicts with the approval request's recorded "+status+" status")
		}
		decisions = append(decisions, decision{id, choice, response.Content.Reason})
	}
	for _, d := range decisions {
		if _, err := m.decideApproval(call.c, ref.Namespace, ref.Name, ref.UID, d.id, d.decision, d.reason); err != nil {
			return nil, mcpTaskRPCError(err)
		}
	}
	return map[string]any{"resultType": "complete"}, nil
}

func (m *mcpServer) cancelTask(call *mcpCall) (any, error) {
	_, ref, err := decodeMCPTaskParams(call.params)
	if err != nil {
		return nil, err
	}
	state, err := m.resolveMCPTask(call.c, ref)
	if err != nil {
		return nil, err
	}
	if mcpTaskTerminal(state.response.Status.Phase) {
		return nil, newMCPError(mcpCodeInvalidParams, "task has already finished")
	}
	cancelled, err := m.requestCancellation(call.c, ref.Namespace, ref.Name, ref.UID)
	if err != nil {
		return nil, mcpTaskRPCError(err)
	}
	if call.tasksExtension {
		return map[string]any{"resultType": "complete"}, nil
	}
	fields := state.coreTaskFields()
	if corev1alpha1.TaskPhase(cancelled.Phase) == corev1alpha1.TaskPhaseCancelled {
		fields["status"] = mcpStatusCancelled
	}
	fields["statusMessage"] = "cancellation requested"
	if message := mcpSafeText(cancelled.Message, mcpMaxStatusMessage); message != "" {
		fields["statusMessage"] = "cancellation requested: " + message
	}
	fields["lastUpdatedAt"] = mcpTimeValue(time.Now())
	return fields, nil
}
