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

	"github.com/orka-agents/orka/internal/approvals"
)

// MCP Tasks map an Orka Task to an MCP task without any MCP-side state: the
// task ID names the Task and pins its UID, and every tasks/* method reads or
// changes the Task through the same REST routes as the tools. Two wire shapes
// are served:
//
//   - the io.modelcontextprotocol/tasks extension, selected by a request whose
//     _meta client capabilities declare it (tasks/get, tasks/update,
//     tasks/cancel; input_required carries elicitation input requests);
//   - the 2025-11-25 task-augmented tools/call (params.task), with tasks/get,
//     tasks/result, and tasks/cancel.

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

// mcpTaskRPCError converts a REST failure into a JSON-RPC error for tasks/*.
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

// resolveMCPTask reads the Task named by a task ID and checks that it is the
// same Task, not a later one reusing the name.
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
	}
	return mcpTimeValue(latest)
}

func (s mcpTaskState) statusMessage() string {
	if pending := s.pending(); pending > 0 && s.status() == mcpStatusInputRequired {
		return "waiting for a human decision on an approval request; use list_pending_approvals and decide_approval"
	}
	return mcpSafeText(s.response.Status.Message, mcpMaxStatusMessage)
}

// coreTaskFields is the 2025-11-25 Task object.
func (s mcpTaskState) coreTaskFields() map[string]any {
	fields := map[string]any{
		"taskId":        encodeMCPTaskID(s.response.Namespace, s.response.Name, string(s.response.UID)),
		"status":        s.status(),
		"createdAt":     mcpTimeValue(s.response.CreationTimestamp.Time),
		"lastUpdatedAt": s.lastUpdated(),
		"ttl":           nil,
		"pollInterval":  mcpTaskPollIntervalMS,
	}
	if message := s.statusMessage(); message != "" {
		fields["statusMessage"] = message
	}
	return fields
}

// extensionTaskFields is the io.modelcontextprotocol/tasks task object.
func (s mcpTaskState) extensionTaskFields(resultType string) map[string]any {
	fields := map[string]any{
		"resultType":     resultType,
		"taskId":         encodeMCPTaskID(s.response.Namespace, s.response.Name, string(s.response.UID)),
		"status":         s.status(),
		"createdAt":      mcpTimeValue(s.response.CreationTimestamp.Time),
		"lastUpdatedAt":  s.lastUpdated(),
		"ttlMs":          nil,
		"pollIntervalMs": mcpTaskPollIntervalMS,
	}
	if message := s.statusMessage(); message != "" {
		fields["statusMessage"] = message
	}
	return fields
}

// terminalToolResult is the create_task result once the Task has finished:
// its summary and a bounded, redacted prefix of its result text.
func (m *mcpServer) terminalToolResult(c fiber.Ctx, state mcpTaskState) mcpToolResult {
	value := map[string]any{"task": state.summary(false)}
	if state.response.Status.ResultRef != nil && state.response.Status.ResultRef.Available {
		var response struct {
			Result string `json:"result"`
		}
		err := m.restJSON(c, fiber.MethodGet, mcpTaskPath(state.response.Name, "result"), mcpNamespaceQuery(state.response.Namespace), nil, &response)
		if err == nil {
			value["result"] = mcpOutputChunk(response.Result, 0, mcpDefaultOutputBytes)
		}
	}
	result := mcpStructuredResult(value)
	result.IsError = state.status() != mcpStatusCompleted
	return result
}

// mcpApprovalInputSchema is the elicitation form for one approval request.
var mcpApprovalInputSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"decision": map[string]any{
			"type": "string", "title": "Decision", "enum": []string{approvalDecisionApprove, approvalDecisionDecline},
			"enumNames": []string{"Approve", "Decline"},
		},
		"reason": map[string]any{"type": "string", "title": "Reason", "maxLength": mcpMaxReasonLength},
	},
	"required": []string{"decision"},
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
	if err != nil {
		return mcpToolErrorResult(err)
	}
	if params.Task != nil {
		return map[string]any{"task": state.coreTaskFields()}, nil
	}
	return state.extensionTaskFields("task"), nil
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
	switch state.status() {
	case mcpStatusInputRequired:
		fields["inputRequests"] = state.inputRequests()
	case mcpStatusCompleted, mcpStatusFailed, mcpStatusCancelled:
		fields["result"] = m.terminalToolResult(call.c, state)
	}
	return fields, nil
}

// taskResult serves the 2025-11-25 tasks/result: it waits, bounded, for the
// Task to finish and returns the create_task tool result.
func (m *mcpServer) taskResult(call *mcpCall) (any, error) {
	_, ref, err := decodeMCPTaskParams(call.params)
	if err != nil {
		return nil, err
	}
	state, _, err := m.waitForTerminal(call.c, ref)
	if err != nil {
		return nil, err
	}
	if !mcpTaskTerminal(state.response.Status.Phase) && state.status() != mcpStatusCancelled {
		return nil, newMCPError(mcpCodeServerBusy, "task has not finished; poll tasks/get and request the result again")
	}
	result := m.terminalToolResult(call.c, state)
	result.Meta = map[string]any{mcpMetaRelatedTask: map[string]any{"taskId": encodeMCPTaskID(ref.Namespace, ref.Name, ref.UID)}}
	return result, nil
}

func (m *mcpServer) waitForTerminal(c fiber.Ctx, ref mcpTaskRef) (mcpTaskState, bool, error) {
	deadline := time.Now().Add(mcpMaxWaitSeconds * time.Second)
	for {
		state, timedOut, err := m.waitForTask(c, ref.Namespace, ref.Name, ref.UID, time.Until(deadline))
		if err != nil {
			return state, false, mcpTaskRPCError(err)
		}
		if timedOut || state.status() != mcpStatusInputRequired {
			return state, timedOut, nil
		}
		// An input_required Task is not finished; keep waiting for the
		// decision until the bound.
		if time.Until(deadline) <= mcpPollInterval {
			return state, true, nil
		}
		timer := time.NewTimer(mcpPollInterval)
		select {
		case <-timer.C:
		case <-c.RequestCtx().Done():
			timer.Stop()
			return state, true, nil
		}
	}
}

// updateTask serves the extension's tasks/update: each input response
// answers one pending approval request and is recorded through the REST
// decision route under the caller's identity.
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
	pending := map[string]bool{}
	for _, approval := range state.approvals {
		if approval.Status == approvals.StatusPending {
			pending[approval.ID] = true
		}
	}
	type decision struct{ id, decision, reason string }
	var decisions []decision
	for id, raw := range params.InputResponses {
		if !pending[id] {
			return nil, newMCPError(mcpCodeInvalidParams, "input response does not match a pending approval request")
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
		switch response.Action {
		case "accept":
			if response.Content.Decision != approvalDecisionApprove && response.Content.Decision != approvalDecisionDecline {
				return nil, newMCPError(mcpCodeInvalidParams, "accepted input must choose approve or decline")
			}
			decisions = append(decisions, decision{id, response.Content.Decision, response.Content.Reason})
		case "decline":
			// The person explicitly refused the request.
			decisions = append(decisions, decision{id, approvalDecisionDecline, response.Content.Reason})
		case "cancel":
			// Dismissed without a choice: the request stays pending.
		default:
			return nil, newMCPError(mcpCodeInvalidParams, "unsupported elicitation action")
		}
	}
	for _, d := range decisions {
		if _, err := m.decideApproval(call.c, ref.Namespace, ref.Name, d.id, d.decision, d.reason); err != nil {
			return nil, mcpTaskRPCError(err)
		}
	}
	return map[string]any{"resultType": "complete"}, nil
}

// cancelTask deletes the Task through DELETE /api/v1/tasks/:id, the only
// public cancellation route.
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
	if _, err := m.rest(call.c, fiber.MethodDelete, mcpTaskPath(ref.Name), mcpNamespaceQuery(ref.Namespace), nil); err != nil {
		return nil, mcpTaskRPCError(err)
	}
	if call.tasksExtension {
		return map[string]any{"resultType": "complete"}, nil
	}
	fields := state.coreTaskFields()
	fields["status"] = mcpStatusCancelled
	fields["statusMessage"] = "cancelled by deleting the Task"
	fields["lastUpdatedAt"] = mcpTimeValue(time.Now())
	return fields, nil
}
