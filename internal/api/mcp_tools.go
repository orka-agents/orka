/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

//nolint:goconst // Keep MCP wire field names and JSON Schema keywords literal to match the protocol.
package api

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/redact"
)

const (
	mcpToolListAgents           = "list_agents"
	mcpToolGetAgent             = "get_agent"
	mcpToolCreateTask           = "create_task"
	mcpToolGetTask              = "get_task"
	mcpToolListTasks            = "list_tasks"
	mcpToolWaitForTask          = "wait_for_task"
	mcpToolCancelTask           = "cancel_task"
	mcpToolListPendingApprovals = "list_pending_approvals"
	mcpToolDecideApproval       = "decide_approval"
	mcpToolGetTaskResult        = "get_task_result"
	mcpToolListTaskArtifacts    = "list_task_artifacts"
	mcpToolGetTaskArtifact      = "get_task_artifact"

	mcpDefaultListLimit     = 20
	mcpMaxListLimit         = 50
	mcpMaxPromptBytes       = 128 << 10
	mcpMaxReasonLength      = 2000
	mcpMaxApprovalIDLength  = 256
	mcpDefaultOutputBytes   = 32 << 10
	mcpMaxOutputBytes       = 256 << 10
	mcpMaxCollectionEntries = 200
	mcpMaxStatusMessage     = 1024
	mcpMaxPromptPreview     = 2000
	mcpMaxApprovalPreview   = 4 << 10
	mcpDefaultWaitSeconds   = 25
	mcpMaxWaitSeconds       = 50
	mcpPollInterval         = 2 * time.Second
	mcpTaskPollIntervalMS   = 5000
)

const (
	mcpStatusWorking       = "working"
	mcpStatusInputRequired = "input_required"
	mcpStatusCompleted     = "completed"
	mcpStatusFailed        = "failed"
	mcpStatusCancelled     = "cancelled"
)

// mcpToolDefinition is one tools/list entry plus its executor.
type mcpToolDefinition struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
	Execution   map[string]any `json:"execution,omitempty"`
	// REST names the REST routes the tool replays, for documentation and
	// tests. It is not sent to clients.
	REST []string                                                             `json:"-"`
	run  func(m *mcpServer, call *mcpCall, args json.RawMessage) (any, error) `json:"-"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type mcpToolResult struct {
	Content           []mcpContent   `json:"content"`
	StructuredContent any            `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError,omitempty"`
	Meta              map[string]any `json:"_meta,omitempty"`
}

// mcpArgumentError is a tool input error the model can correct.
type mcpArgumentError string

func (e mcpArgumentError) Error() string { return string(e) }

func readOnlyToolAnnotations(title string) map[string]any {
	return map[string]any{"title": title, "readOnlyHint": true, "openWorldHint": false}
}

func mutatingToolAnnotations(title string, destructive, idempotent bool) map[string]any {
	return map[string]any{
		"title": title, "readOnlyHint": false, "destructiveHint": destructive,
		"idempotentHint": idempotent, "openWorldHint": false,
	}
}

func objectSchema(required []string, properties map[string]any) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

var (
	mcpNamespaceProperty = map[string]any{
		"type": "string", "maxLength": 63,
		"description": "Kubernetes namespace. Omit to use the server's default namespace for your identity.",
	}
	mcpTaskNameProperty = map[string]any{"type": "string", "maxLength": 253, "description": "Task name."}
	mcpLimitProperty    = map[string]any{
		"type": "integer", "minimum": 1, "maximum": mcpMaxListLimit, "default": mcpDefaultListLimit,
		"description": "Maximum items to return.",
	}
	mcpContinueProperty = map[string]any{
		"type": "string", "maxLength": 4096, "description": "Continue token from a previous page.",
	}
	mcpOffsetProperty = map[string]any{
		"type": "integer", "minimum": 0, "default": 0,
		"description": "Byte offset into the redacted output. Use nextOffset from a previous call to read more.",
	}
	mcpMaxBytesProperty = map[string]any{
		"type": "integer", "minimum": 1, "maximum": mcpMaxOutputBytes, "default": mcpDefaultOutputBytes,
		"description": "Maximum bytes of output to return.",
	}
)

// mcpToolDefinitions returns the tool catalog. Every tool's authorization is
// the authorization of the REST routes it replays; see REST.
func mcpToolDefinitions() []mcpToolDefinition {
	return []mcpToolDefinition{
		{
			Name: mcpToolListAgents, Title: "List Orka Agents",
			Description: "List the Agents you can see in a namespace. Pass an Agent name to create_task to delegate work to it.",
			InputSchema: objectSchema(nil, map[string]any{
				"namespace": mcpNamespaceProperty, "limit": mcpLimitProperty, "continue": mcpContinueProperty,
			}),
			Annotations: readOnlyToolAnnotations("List Orka Agents"),
			REST:        []string{"GET /api/v1/agents"},
			run:         (*mcpServer).toolListAgents,
		},
		{
			Name: mcpToolGetAgent, Title: "Get an Orka Agent",
			Description: "Describe one Agent: its runtime, model, tools, skills, and readiness.",
			InputSchema: objectSchema([]string{"name"}, map[string]any{
				"name": map[string]any{"type": "string", "maxLength": 253, "description": "Agent name."}, "namespace": mcpNamespaceProperty,
			}),
			Annotations: readOnlyToolAnnotations("Get an Orka Agent"),
			REST:        []string{"GET /api/v1/agents/:name"},
			run:         (*mcpServer).toolGetAgent,
		},
		{
			Name: mcpToolCreateTask, Title: "Delegate a Task to an Orka Agent",
			Description: "Create a Task that runs the prompt on an Orka Agent. Returns immediately with the Task name; " +
				"use wait_for_task or get_task to follow it. Supports the MCP Tasks extension for long-running calls. " +
				"Set session to continue an existing Orka Session. The workspace object uses the Task API's spec.workspace " +
				"fields and is validated exactly like the REST API.",
			InputSchema: objectSchema([]string{"agent", "prompt"}, map[string]any{
				"agent":     map[string]any{"type": "string", "maxLength": 253, "description": "Name of the Agent to run, in the Task namespace."},
				"prompt":    map[string]any{"type": "string", "minLength": 1, "maxLength": mcpMaxPromptBytes, "description": "Instructions for the Agent."},
				"namespace": mcpNamespaceProperty,
				"name": map[string]any{
					"type": "string", "maxLength": 253,
					"description": "Optional Task name. Omit to generate one. Reusing a name fails with a conflict.",
				},
				"taskType": map[string]any{
					"type": "string", "enum": []string{string(corev1alpha1.TaskTypeAgent), string(corev1alpha1.TaskTypeAI)},
					"description": "agent for Agents with a CLI runtime, ai for native Agents. Omit to read it from the Agent, which requires get access to the Agent.",
				},
				"timeout": map[string]any{"type": "string", "maxLength": 32, "description": "Optional Task timeout, for example 30m or 2h."},
				"workspace": map[string]any{
					"type": "object",
					"description": "Optional repository workspace (Task spec.workspace), e.g. {\"gitRepo\":\"https://github.com/org/repo\",\"branch\":\"main\"}. " +
						"Credentials are referenced by Secret name only (readCredentialRef, publicationCredentialRef).",
					"properties": map[string]any{
						"intent":                   map[string]any{"type": "string", "enum": []string{"read", "write"}},
						"gitRepo":                  map[string]any{"type": "string"},
						"branch":                   map[string]any{"type": "string"},
						"ref":                      map[string]any{"type": "string"},
						"subPath":                  map[string]any{"type": "string"},
						"readCredentialRef":        map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}},
						"publicationGitRepo":       map[string]any{"type": "string"},
						"publicationCredentialRef": map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}},
						"forgeCredentialRef":       map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}}},
						"pushBranch":               map[string]any{"type": "string"},
						"prBaseBranch":             map[string]any{"type": "string"},
						"prTitle":                  map[string]any{"type": "string"},
						"prBody":                   map[string]any{"type": "string"},
						"createPR":                 map[string]any{"type": "boolean"},
					},
				},
				"session": map[string]any{
					"type":        "object",
					"description": "Optional Orka Session to continue (Task spec.sessionRef).",
					"properties": map[string]any{
						"name":   map[string]any{"type": "string", "maxLength": 253},
						"create": map[string]any{"type": "boolean", "description": "Create the Session if it does not exist."},
					},
					"required":             []string{"name"},
					"additionalProperties": false,
				},
			}),
			Annotations: mutatingToolAnnotations("Delegate a Task to an Orka Agent", false, false),
			Execution:   map[string]any{"taskSupport": "optional"},
			REST:        []string{"POST /api/v1/tasks", "GET /api/v1/agents/:name (only when taskType is omitted)"},
			run:         (*mcpServer).toolCreateTask,
		},
		{
			Name: mcpToolGetTask, Title: "Get an Orka Task",
			Description: "Show a Task's status, pending approvals, and whether its result is available.",
			InputSchema: objectSchema([]string{"name"}, map[string]any{"name": mcpTaskNameProperty, "namespace": mcpNamespaceProperty}),
			Annotations: readOnlyToolAnnotations("Get an Orka Task"),
			REST:        []string{"GET /api/v1/tasks/:id", "GET /api/v1/tasks/:id/approvals"},
			run:         (*mcpServer).toolGetTask,
		},
		{
			Name: mcpToolListTasks, Title: "List Orka Tasks",
			Description: "List Tasks you can see in a namespace, newest pages first as returned by the API.",
			InputSchema: objectSchema(nil, map[string]any{
				"namespace": mcpNamespaceProperty, "limit": mcpLimitProperty, "continue": mcpContinueProperty,
				"labelSelector": map[string]any{"type": "string", "maxLength": 1024, "description": "Kubernetes label selector, e.g. orka.ai/agent=reviewer."},
			}),
			Annotations: readOnlyToolAnnotations("List Orka Tasks"),
			REST:        []string{"GET /api/v1/tasks"},
			run:         (*mcpServer).toolListTasks,
		},
		{
			Name: mcpToolWaitForTask, Title: "Wait for an Orka Task",
			Description: fmt.Sprintf("Wait up to timeoutSeconds (max %d) for a Task to finish or to need an approval decision. "+
				"Returns the current status when the wait ends; call again to keep waiting.", mcpMaxWaitSeconds),
			InputSchema: objectSchema([]string{"name"}, map[string]any{
				"name": mcpTaskNameProperty, "namespace": mcpNamespaceProperty,
				"timeoutSeconds": map[string]any{"type": "integer", "minimum": 1, "maximum": mcpMaxWaitSeconds, "default": mcpDefaultWaitSeconds},
			}),
			Annotations: readOnlyToolAnnotations("Wait for an Orka Task"),
			REST:        []string{"GET /api/v1/tasks/:id", "GET /api/v1/tasks/:id/approvals"},
			run:         (*mcpServer).toolWaitForTask,
		},
		{
			Name: mcpToolCancelTask, Title: "Cancel an Orka Task",
			Description: "Stop a Task by deleting it, exactly like DELETE /api/v1/tasks/:id. Its result and artifacts become unavailable.",
			InputSchema: objectSchema([]string{"name"}, map[string]any{"name": mcpTaskNameProperty, "namespace": mcpNamespaceProperty}),
			Annotations: mutatingToolAnnotations("Cancel an Orka Task", true, true),
			REST:        []string{"DELETE /api/v1/tasks/:id"},
			run:         (*mcpServer).toolCancelTask,
		},
		{
			Name: mcpToolListPendingApprovals, Title: "List a Task's approval requests",
			Description: "List the approval requests of a Task. A human must decide pending requests before the Task can continue.",
			InputSchema: objectSchema([]string{"task"}, map[string]any{
				"task": mcpTaskNameProperty, "namespace": mcpNamespaceProperty,
				"includeDecided": map[string]any{"type": "boolean", "default": false, "description": "Also return decided, expired, and cancelled requests."},
			}),
			Annotations: readOnlyToolAnnotations("List a Task's approval requests"),
			REST:        []string{"GET /api/v1/tasks/:id/approvals"},
			run:         (*mcpServer).toolListPendingApprovals,
		},
		{
			Name: mcpToolDecideApproval, Title: "Approve or decline an Orka approval request",
			Description: "Record a human's approve or decline decision for a pending approval request. " +
				"Only call this after the human user has reviewed the request and explicitly told you their decision; " +
				"never decide on your own. The decision is recorded under your Orka identity.",
			InputSchema: objectSchema([]string{"task", "approvalId", "decision"}, map[string]any{
				"task": mcpTaskNameProperty, "namespace": mcpNamespaceProperty,
				"approvalId": map[string]any{"type": "string", "maxLength": mcpMaxApprovalIDLength, "description": "Approval request ID from list_pending_approvals."},
				"decision":   map[string]any{"type": "string", "enum": []string{approvalDecisionApprove, approvalDecisionDecline}},
				"reason":     map[string]any{"type": "string", "maxLength": mcpMaxReasonLength, "description": "The human's reason, recorded with the decision."},
			}),
			Annotations: mutatingToolAnnotations("Approve or decline an Orka approval request", true, true),
			REST:        []string{"POST /api/v1/tasks/:id/approvals/:approvalID/decision"},
			run:         (*mcpServer).toolDecideApproval,
		},
		{
			Name: mcpToolGetTaskResult, Title: "Read an Orka Task result",
			Description: "Read a finished Task's result text. Output is redacted for credential-like values and returned in bounded chunks.",
			InputSchema: objectSchema([]string{"name"}, map[string]any{
				"name": mcpTaskNameProperty, "namespace": mcpNamespaceProperty, "offset": mcpOffsetProperty, "maxBytes": mcpMaxBytesProperty,
			}),
			Annotations: readOnlyToolAnnotations("Read an Orka Task result"),
			REST:        []string{"GET /api/v1/tasks/:id/result"},
			run:         (*mcpServer).toolGetTaskResult,
		},
		{
			Name: mcpToolListTaskArtifacts, Title: "List an Orka Task's artifacts",
			Description: "List the files a Task uploaded as artifacts.",
			InputSchema: objectSchema([]string{"name"}, map[string]any{"name": mcpTaskNameProperty, "namespace": mcpNamespaceProperty}),
			Annotations: readOnlyToolAnnotations("List an Orka Task's artifacts"),
			REST:        []string{"GET /api/v1/tasks/:id/artifacts"},
			run:         (*mcpServer).toolListTaskArtifacts,
		},
		{
			Name: mcpToolGetTaskArtifact, Title: "Read an Orka Task artifact",
			Description: "Read a text artifact in bounded, redacted chunks. Binary artifacts must be downloaded through the REST API.",
			InputSchema: objectSchema([]string{"name", "filename"}, map[string]any{
				"name": mcpTaskNameProperty, "namespace": mcpNamespaceProperty,
				"filename": map[string]any{"type": "string", "maxLength": 255, "description": "Artifact filename from list_task_artifacts."},
				"offset":   mcpOffsetProperty, "maxBytes": mcpMaxBytesProperty,
			}),
			Annotations: readOnlyToolAnnotations("Read an Orka Task artifact"),
			REST:        []string{"GET /api/v1/tasks/:id/artifacts/:filename"},
			run:         (*mcpServer).toolGetTaskArtifact,
		},
	}
}

func mcpToolByName(name string) (mcpToolDefinition, bool) {
	for _, tool := range mcpToolDefinitions() {
		if tool.Name == name {
			return tool, true
		}
	}
	return mcpToolDefinition{}, false
}

type mcpToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	// Task is the 2025-11-25 task augmentation.
	Task *struct {
		TTL *int64 `json:"ttl,omitempty"`
	} `json:"task,omitempty"`
}

func (m *mcpServer) callTool(call *mcpCall) (any, error) {
	var params mcpToolCallParams
	if err := json.Unmarshal(call.params, &params); err != nil {
		return nil, newMCPError(mcpCodeInvalidParams, "invalid tools/call params")
	}
	tool, ok := mcpToolByName(params.Name)
	if !ok {
		return nil, newMCPError(mcpCodeInvalidParams, "unknown tool")
	}
	if tool.Name == mcpToolCreateTask && (params.Task != nil || call.tasksExtension) {
		return m.createTaskAsMCPTask(call, params)
	}
	value, err := tool.run(m, call, params.Arguments)
	if err != nil {
		return mcpToolErrorResult(err)
	}
	return mcpStructuredResult(value), nil
}

func mcpStructuredResult(value any) mcpToolResult {
	data, err := json.Marshal(value)
	if err != nil {
		data = []byte(`{}`)
	}
	return mcpToolResult{Content: []mcpContent{{Type: "text", Text: string(data)}}, StructuredContent: value}
}

// mcpToolErrorResult reports a tool failure to the model. Only argument and
// REST errors carry detail; anything else is logged and reported generically.
func mcpToolErrorResult(err error) (any, error) {
	var restErr *mcpRESTError
	var argErr mcpArgumentError
	var rpcErr *mcpError
	structured := map[string]any{}
	switch {
	case errors.As(err, &rpcErr):
		return nil, err
	case errors.As(err, &restErr):
		structured["status"] = restErr.Status
		structured["message"] = restErr.Message
	case errors.As(err, &argErr):
		structured["message"] = mcpSafeText(argErr.Error(), 512)
	default:
		log.Error(err, "MCP tool failed")
		structured["message"] = "internal error"
	}
	text := fmt.Sprint(structured["message"])
	if restErr != nil {
		text = restErr.Error()
	}
	return mcpToolResult{
		Content:           []mcpContent{{Type: "text", Text: text}},
		StructuredContent: map[string]any{"error": structured},
		IsError:           true,
	}, nil
}

func decodeMCPArguments(raw json.RawMessage, target any) error {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		raw = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return mcpArgumentError("invalid arguments: " + err.Error())
	}
	return nil
}

// mcpInt accepts a JSON number or a numeric string, as model tool calls
// sometimes quote integers. Null and omission leave it unset.
type mcpInt struct {
	set   bool
	value int64
}

func (v *mcpInt) UnmarshalJSON(data []byte) error {
	text := strings.TrimSpace(string(data))
	if text == "null" {
		*v = mcpInt{}
		return nil
	}
	if unquoted, err := strconv.Unquote(text); err == nil {
		text = strings.TrimSpace(unquoted)
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return fmt.Errorf("expected an integer")
	}
	*v = mcpInt{set: true, value: value}
	return nil
}

func (v mcpInt) bounded(field string, minimum, maximum, fallback int64) (int64, error) {
	if !v.set {
		return fallback, nil
	}
	if v.value < minimum || v.value > maximum {
		return 0, mcpArgumentError(fmt.Sprintf("%s must be between %d and %d", field, minimum, maximum))
	}
	return v.value, nil
}

// mcpBool accepts a JSON boolean or "true"/"false".
type mcpBool struct {
	value bool
}

func (v *mcpBool) UnmarshalJSON(data []byte) error {
	text := strings.TrimSpace(string(data))
	if text == "null" {
		*v = mcpBool{}
		return nil
	}
	if unquoted, err := strconv.Unquote(text); err == nil {
		text = unquoted
	}
	value, err := strconv.ParseBool(text)
	if err != nil {
		return fmt.Errorf("expected a boolean")
	}
	v.value = value
	return nil
}

// --- Agents ---

type mcpAgentSummary struct {
	Name        string   `json:"name"`
	Namespace   string   `json:"namespace"`
	TaskType    string   `json:"taskType"`
	Runtime     string   `json:"runtime,omitempty"`
	Provider    string   `json:"provider,omitempty"`
	Model       string   `json:"model,omitempty"`
	Tools       []string `json:"tools,omitempty"`
	Skills      []string `json:"skills,omitempty"`
	Ready       bool     `json:"ready"`
	ActiveTasks int32    `json:"activeTasks"`
}

func summarizeMCPAgent(agent *corev1alpha1.Agent) mcpAgentSummary {
	summary := mcpAgentSummary{
		Name: agent.Name, Namespace: agent.Namespace, TaskType: string(corev1alpha1.TaskTypeAI),
		Ready: agent.Status.Ready, ActiveTasks: agent.Status.ActiveTasks,
	}
	if agent.Spec.Runtime != nil {
		summary.TaskType = string(corev1alpha1.TaskTypeAgent)
		summary.Runtime = string(agent.Spec.Runtime.Type)
	}
	if agent.Spec.ProviderRef != nil {
		summary.Provider = agent.Spec.ProviderRef.Name
	}
	if agent.Spec.Model != nil {
		if summary.Provider == "" {
			summary.Provider = agent.Spec.Model.Provider
		}
		summary.Model = agent.Spec.Model.Name
	}
	for i, tool := range agent.Spec.Tools {
		if i == mcpMaxListLimit {
			break
		}
		summary.Tools = append(summary.Tools, tool.Name)
	}
	for i, skill := range agent.Spec.Skills {
		if i == mcpMaxListLimit {
			break
		}
		if skill.Name != "" {
			summary.Skills = append(summary.Skills, skill.Name)
		}
	}
	return summary
}

type mcpListArgs struct {
	Namespace string `json:"namespace"`
	Limit     mcpInt `json:"limit"`
	Continue  string `json:"continue"`
}

func (a mcpListArgs) query() (url.Values, error) {
	if err := validateMCPNamespace(a.Namespace); err != nil {
		return nil, err
	}
	limit, err := a.Limit.bounded("limit", 1, mcpMaxListLimit, mcpDefaultListLimit)
	if err != nil {
		return nil, err
	}
	if len(a.Continue) > 4096 {
		return nil, mcpArgumentError("continue is too long")
	}
	query := mcpNamespaceQuery(a.Namespace)
	query.Set("limit", strconv.FormatInt(limit, 10))
	if a.Continue != "" {
		query.Set("continue", a.Continue)
	}
	return query, nil
}

func (m *mcpServer) toolListAgents(call *mcpCall, raw json.RawMessage) (any, error) {
	var args mcpListArgs
	if err := decodeMCPArguments(raw, &args); err != nil {
		return nil, err
	}
	query, err := args.query()
	if err != nil {
		return nil, err
	}
	var response struct {
		Items    []corev1alpha1.Agent `json:"items"`
		Metadata ListMeta             `json:"metadata"`
	}
	if err := m.restJSON(call.c, fiber.MethodGet, "/api/v1/agents", query, nil, &response); err != nil {
		return nil, err
	}
	agents := make([]mcpAgentSummary, 0, len(response.Items))
	for i := range response.Items {
		agents = append(agents, summarizeMCPAgent(&response.Items[i]))
	}
	return map[string]any{"agents": agents, "continue": response.Metadata.Continue}, nil
}

type mcpNamedArgs struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

func (a mcpNamedArgs) validate(field string) error {
	if err := validateMCPObjectName(field, a.Name); err != nil {
		return err
	}
	return validateMCPNamespace(a.Namespace)
}

func (m *mcpServer) toolGetAgent(call *mcpCall, raw json.RawMessage) (any, error) {
	var args mcpNamedArgs
	if err := decodeMCPArguments(raw, &args); err != nil {
		return nil, err
	}
	if err := args.validate("name"); err != nil {
		return nil, err
	}
	agent, err := m.getAgent(call.c, args.Namespace, args.Name)
	if err != nil {
		return nil, err
	}
	return summarizeMCPAgent(agent), nil
}

func (m *mcpServer) getAgent(c fiber.Ctx, namespace, name string) (*corev1alpha1.Agent, error) {
	agent := &corev1alpha1.Agent{}
	if err := m.restJSON(c, fiber.MethodGet, "/api/v1/agents/"+name, mcpNamespaceQuery(namespace), nil, agent); err != nil {
		return nil, err
	}
	return agent, nil
}

// --- Tasks ---

type mcpTaskSummary struct {
	TaskID           string              `json:"taskId"`
	Name             string              `json:"name"`
	Namespace        string              `json:"namespace"`
	Type             string              `json:"type"`
	Agent            string              `json:"agent,omitempty"`
	Phase            string              `json:"phase,omitempty"`
	Status           string              `json:"status"`
	Message          string              `json:"message,omitempty"`
	CreatedAt        string              `json:"createdAt,omitempty"`
	StartedAt        string              `json:"startedAt,omitempty"`
	CompletedAt      string              `json:"completedAt,omitempty"`
	Attempts         int32               `json:"attempts,omitempty"`
	ResultAvailable  bool                `json:"resultAvailable"`
	Session          string              `json:"session,omitempty"`
	PendingApprovals *int                `json:"pendingApprovals,omitempty"`
	Prompt           string              `json:"prompt,omitempty"`
	Plan             *mcpTaskPlanSummary `json:"plan,omitempty"`
	Delivery         *mcpDeliverySummary `json:"delivery,omitempty"`
}

type mcpTaskPlanSummary struct {
	Summary      string `json:"summary,omitempty"`
	ProgressPct  int    `json:"progressPct"`
	GoalComplete bool   `json:"goalComplete"`
}

type mcpDeliverySummary struct {
	State          string `json:"state,omitempty"`
	Outcome        string `json:"outcome,omitempty"`
	Reason         string `json:"reason,omitempty"`
	Branch         string `json:"branch,omitempty"`
	PullRequestURL string `json:"pullRequestURL,omitempty"`
}

// mcpTaskResponse is the GET /api/v1/tasks/:id response shape.
type mcpTaskResponse struct {
	corev1alpha1.Task `json:",inline"`
	Plan              *struct {
		Summary      string `json:"summary"`
		ProgressPct  int    `json:"progressPct"`
		GoalComplete bool   `json:"goalComplete"`
	} `json:"plan,omitempty"`
}

// mcpTaskState is one authorized read of a Task and, while it runs, its
// approval requests.
type mcpTaskState struct {
	response       mcpTaskResponse
	approvals      []approvals.Approval
	approvalsKnown bool
}

func (s mcpTaskState) pending() int {
	count := 0
	for _, approval := range s.approvals {
		if approval.Status == approvals.StatusPending {
			count++
		}
	}
	return count
}

func mcpTaskTerminal(phase corev1alpha1.TaskPhase) bool {
	switch phase {
	case corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskPhaseCancelled:
		return true
	}
	return false
}

// mcpTaskStatus maps an Orka Task lifecycle phase to an MCP task status.
func (s mcpTaskState) status() string {
	switch s.response.Status.Phase {
	case corev1alpha1.TaskPhaseSucceeded:
		return mcpStatusCompleted
	case corev1alpha1.TaskPhaseFailed:
		return mcpStatusFailed
	case corev1alpha1.TaskPhaseCancelled:
		return mcpStatusCancelled
	}
	if !s.response.DeletionTimestamp.IsZero() {
		return mcpStatusCancelled
	}
	if s.pending() > 0 {
		return mcpStatusInputRequired
	}
	return mcpStatusWorking
}

func mcpTime(value *metav1.Time) string {
	if value == nil || value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func (s mcpTaskState) summary(includePrompt bool) mcpTaskSummary {
	task := &s.response.Task
	summary := mcpTaskSummary{
		TaskID:    encodeMCPTaskID(task.Namespace, task.Name, string(task.UID)),
		Name:      task.Name,
		Namespace: task.Namespace,
		Type:      string(task.Spec.Type),
		Phase:     string(task.Status.Phase),
		Status:    s.status(),
		Message:   mcpSafeText(task.Status.Message, mcpMaxStatusMessage),
		CreatedAt: mcpTime(&task.CreationTimestamp), StartedAt: mcpTime(task.Status.StartTime),
		CompletedAt:     mcpTime(task.Status.CompletionTime),
		Attempts:        task.Status.Attempts,
		ResultAvailable: task.Status.ResultRef != nil && task.Status.ResultRef.Available,
	}
	if task.Spec.AgentRef != nil {
		summary.Agent = task.Spec.AgentRef.Name
	}
	if task.Spec.SessionRef != nil {
		summary.Session = task.Spec.SessionRef.Name
	}
	if s.approvalsKnown {
		pending := s.pending()
		summary.PendingApprovals = &pending
	}
	if includePrompt {
		prompt := task.Spec.Prompt
		if prompt == "" && task.Spec.AI != nil {
			prompt = task.Spec.AI.Prompt
		}
		summary.Prompt = mcpSafeText(prompt, mcpMaxPromptPreview)
	}
	if plan := s.response.Plan; plan != nil {
		summary.Plan = &mcpTaskPlanSummary{
			Summary: mcpSafeText(plan.Summary, mcpMaxStatusMessage), ProgressPct: plan.ProgressPct, GoalComplete: plan.GoalComplete,
		}
	}
	if delivery := task.Status.Delivery; delivery != nil {
		summary.Delivery = &mcpDeliverySummary{
			State: string(delivery.State), Outcome: string(delivery.Outcome), Reason: string(delivery.Reason), Branch: delivery.Branch,
		}
		if delivery.PRReceipt != nil {
			summary.Delivery.PullRequestURL = delivery.PRReceipt.URL
		}
	}
	return summary
}

// loadTaskState reads a Task and, unless it has finished, its approvals.
// Approvals are read through their own route, which requires the same Task
// read permission; a server without execution event storage has none.
func (m *mcpServer) loadTaskState(c fiber.Ctx, namespace, name string) (mcpTaskState, error) {
	var state mcpTaskState
	if err := m.restJSON(c, fiber.MethodGet, mcpTaskPath(name), mcpNamespaceQuery(namespace), nil, &state.response); err != nil {
		return state, err
	}
	if mcpTaskTerminal(state.response.Status.Phase) {
		return state, nil
	}
	listed, err := m.listApprovals(c, state.response.Namespace, name)
	if err != nil {
		var restErr *mcpRESTError
		if errors.As(err, &restErr) && restErr.Status == fiber.StatusNotImplemented {
			return state, nil
		}
		return state, err
	}
	if listed.TaskUID != "" && listed.TaskUID != string(state.response.UID) {
		// The Task was replaced between the two reads; report what we know.
		return state, nil
	}
	state.approvals = listed.Approvals
	state.approvalsKnown = true
	return state, nil
}

func (m *mcpServer) listApprovals(c fiber.Ctx, namespace, task string) (ListTaskApprovalsResponse, error) {
	var listed ListTaskApprovalsResponse
	err := m.restJSON(c, fiber.MethodGet, mcpTaskPath(task, "approvals"), mcpNamespaceQuery(namespace), nil, &listed)
	return listed, err
}

type mcpCreateTaskArgs struct {
	Agent     string                        `json:"agent"`
	Prompt    string                        `json:"prompt"`
	Namespace string                        `json:"namespace"`
	Name      string                        `json:"name"`
	TaskType  string                        `json:"taskType"`
	Timeout   string                        `json:"timeout"`
	Workspace *corev1alpha1.WorkspaceConfig `json:"workspace"`
	Session   *struct {
		Name   string  `json:"name"`
		Create mcpBool `json:"create"`
	} `json:"session"`
}

func generateMCPTaskName() string {
	return "mcp-" + strings.ToLower(rand.Text()[:12])
}

// createTask creates the Task through POST /api/v1/tasks. Validation beyond
// argument shape is the REST handler's and the API server's.
func (m *mcpServer) createTask(call *mcpCall, raw json.RawMessage) (mcpTaskState, error) {
	var args mcpCreateTaskArgs
	if err := decodeMCPArguments(raw, &args); err != nil {
		return mcpTaskState{}, err
	}
	if err := validateMCPObjectName("agent", args.Agent); err != nil {
		return mcpTaskState{}, err
	}
	if err := validateMCPNamespace(args.Namespace); err != nil {
		return mcpTaskState{}, err
	}
	if strings.TrimSpace(args.Prompt) == "" {
		return mcpTaskState{}, mcpArgumentError("prompt is required")
	}
	if len(args.Prompt) > mcpMaxPromptBytes {
		return mcpTaskState{}, mcpArgumentError(fmt.Sprintf("prompt must be at most %d bytes", mcpMaxPromptBytes))
	}
	if args.Name == "" {
		args.Name = generateMCPTaskName()
	} else if err := validateMCPObjectName("name", args.Name); err != nil {
		return mcpTaskState{}, err
	}
	if len(args.Timeout) > 32 {
		return mcpTaskState{}, mcpArgumentError("timeout is too long")
	}
	taskType := corev1alpha1.TaskType(args.TaskType)
	switch taskType {
	case corev1alpha1.TaskTypeAgent, corev1alpha1.TaskTypeAI:
	case "":
		agent, err := m.getAgent(call.c, args.Namespace, args.Agent)
		if err != nil {
			return mcpTaskState{}, err
		}
		taskType = corev1alpha1.TaskType(summarizeMCPAgent(agent).TaskType)
	default:
		return mcpTaskState{}, mcpArgumentError("taskType must be agent or ai")
	}
	body := map[string]any{
		"name":     args.Name,
		"type":     taskType,
		"prompt":   args.Prompt,
		"agentRef": map[string]any{"name": args.Agent},
	}
	if taskType == corev1alpha1.TaskTypeAI {
		body["ai"] = map[string]any{"prompt": args.Prompt}
	}
	if args.Namespace != "" {
		body["namespace"] = args.Namespace
	}
	if args.Timeout != "" {
		body["timeout"] = args.Timeout
	}
	if args.Workspace != nil {
		body["workspace"] = args.Workspace
	}
	if args.Session != nil {
		if err := validateMCPObjectName("session.name", args.Session.Name); err != nil {
			return mcpTaskState{}, err
		}
		body["sessionRef"] = map[string]any{"name": args.Session.Name, "create": args.Session.Create.value}
	}
	var state mcpTaskState
	if err := m.restJSON(call.c, fiber.MethodPost, "/api/v1/tasks", nil, body, &state.response.Task); err != nil {
		return mcpTaskState{}, err
	}
	return state, nil
}

func (m *mcpServer) toolCreateTask(call *mcpCall, raw json.RawMessage) (any, error) {
	state, err := m.createTask(call, raw)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"task": state.summary(false),
		"next": "Call wait_for_task with this name to follow the Task, then get_task_result to read its result.",
	}, nil
}

func (m *mcpServer) toolGetTask(call *mcpCall, raw json.RawMessage) (any, error) {
	var args mcpNamedArgs
	if err := decodeMCPArguments(raw, &args); err != nil {
		return nil, err
	}
	if err := args.validate("name"); err != nil {
		return nil, err
	}
	state, err := m.loadTaskState(call.c, args.Namespace, args.Name)
	if err != nil {
		return nil, err
	}
	return state.summary(true), nil
}

func (m *mcpServer) toolListTasks(call *mcpCall, raw json.RawMessage) (any, error) {
	var args struct {
		mcpListArgs
		LabelSelector string `json:"labelSelector"`
	}
	if err := decodeMCPArguments(raw, &args); err != nil {
		return nil, err
	}
	query, err := args.query()
	if err != nil {
		return nil, err
	}
	if len(args.LabelSelector) > 1024 {
		return nil, mcpArgumentError("labelSelector is too long")
	}
	if args.LabelSelector != "" {
		query.Set("labelSelector", args.LabelSelector)
	}
	var response struct {
		Items    []corev1alpha1.Task `json:"items"`
		Metadata ListMeta            `json:"metadata"`
	}
	if err := m.restJSON(call.c, fiber.MethodGet, "/api/v1/tasks", query, nil, &response); err != nil {
		return nil, err
	}
	tasks := make([]mcpTaskSummary, 0, len(response.Items))
	for i := range response.Items {
		state := mcpTaskState{response: mcpTaskResponse{Task: response.Items[i]}}
		tasks = append(tasks, state.summary(false))
	}
	return map[string]any{"tasks": tasks, "continue": response.Metadata.Continue}, nil
}

// waitForTask polls a Task until it finishes, needs input, or the wait ends.
// uid, when set, pins the Task identity so a replacement is never reported.
func (m *mcpServer) waitForTask(c fiber.Ctx, namespace, name, uid string, timeout time.Duration) (mcpTaskState, bool, error) {
	if !tryAcquire(m.waitSlots) {
		return mcpTaskState{}, false, &mcpRESTError{Status: fiber.StatusTooManyRequests, Message: "too many concurrent waits; poll get_task instead"}
	}
	defer func() { <-m.waitSlots }()
	deadline := time.Now().Add(timeout)
	for {
		state, err := m.loadTaskState(c, namespace, name)
		if err != nil {
			return state, false, err
		}
		if uid != "" && string(state.response.UID) != uid {
			return state, false, &mcpRESTError{Status: fiber.StatusNotFound, Message: "task not found"}
		}
		if status := state.status(); status != mcpStatusWorking {
			return state, false, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return state, true, nil
		}
		timer := time.NewTimer(min(mcpPollInterval, remaining))
		select {
		case <-timer.C:
		case <-c.RequestCtx().Done():
			timer.Stop()
			return state, true, nil
		}
	}
}

func (m *mcpServer) toolWaitForTask(call *mcpCall, raw json.RawMessage) (any, error) {
	var args struct {
		mcpNamedArgs
		TimeoutSeconds mcpInt `json:"timeoutSeconds"`
	}
	if err := decodeMCPArguments(raw, &args); err != nil {
		return nil, err
	}
	if err := args.validate("name"); err != nil {
		return nil, err
	}
	seconds, err := args.TimeoutSeconds.bounded("timeoutSeconds", 1, mcpMaxWaitSeconds, mcpDefaultWaitSeconds)
	if err != nil {
		return nil, err
	}
	state, timedOut, err := m.waitForTask(call.c, args.Namespace, args.Name, "", time.Duration(seconds)*time.Second)
	if err != nil {
		return nil, err
	}
	return map[string]any{"task": state.summary(false), "timedOut": timedOut}, nil
}

func (m *mcpServer) toolCancelTask(call *mcpCall, raw json.RawMessage) (any, error) {
	var args mcpNamedArgs
	if err := decodeMCPArguments(raw, &args); err != nil {
		return nil, err
	}
	if err := args.validate("name"); err != nil {
		return nil, err
	}
	if _, err := m.rest(call.c, fiber.MethodDelete, mcpTaskPath(args.Name), mcpNamespaceQuery(args.Namespace), nil); err != nil {
		return nil, err
	}
	return map[string]any{"name": args.Name, "cancelled": true, "deleted": true}, nil
}

// --- Approvals ---

type mcpApprovalSummary struct {
	ID                string          `json:"id"`
	Status            string          `json:"status"`
	Action            string          `json:"action,omitempty"`
	RiskSummary       string          `json:"riskSummary,omitempty"`
	TargetTool        string          `json:"targetTool,omitempty"`
	TargetArgsPreview json.RawMessage `json:"targetArgsPreview,omitempty"`
	Severity          string          `json:"severity,omitempty"`
	CreatedAt         string          `json:"createdAt,omitempty"`
	ExpiresAt         string          `json:"expiresAt,omitempty"`
	DecisionActor     string          `json:"decisionActor,omitempty"`
	DecisionReason    string          `json:"decisionReason,omitempty"`
	DecisionTime      string          `json:"decisionTime,omitempty"`
	ExecutionOutcome  string          `json:"executionOutcome,omitempty"`
}

func mcpTimeValue(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func mcpTimePointer(value *time.Time) string {
	if value == nil {
		return ""
	}
	return mcpTimeValue(*value)
}

func summarizeMCPApproval(approval approvals.Approval) mcpApprovalSummary {
	summary := mcpApprovalSummary{
		ID: approval.ID, Status: approval.Status,
		Action:      mcpSafeText(approval.Action, mcpMaxStatusMessage),
		RiskSummary: mcpSafeText(approval.RiskSummary, mcpMaxStatusMessage),
		TargetTool:  approval.TargetTool, Severity: approval.Severity,
		CreatedAt: mcpTimeValue(approval.CreatedAt), ExpiresAt: mcpTimePointer(approval.ExpiresAt),
		DecisionActor:    approval.DecisionActor,
		DecisionReason:   mcpSafeText(approval.DecisionReason, mcpMaxStatusMessage),
		DecisionTime:     mcpTimePointer(approval.DecisionTime),
		ExecutionOutcome: approval.ExecutionOutcome,
	}
	// The preview is already sanitized by the approval producer. Redact once
	// more and drop it if it is too large to show safely.
	if preview := approval.TargetArgsPreview; len(preview) > 0 && len(preview) <= mcpMaxApprovalPreview {
		redacted := redact.SensitiveText(string(preview))
		if json.Valid([]byte(redacted)) {
			summary.TargetArgsPreview = json.RawMessage(redacted)
		}
	}
	return summary
}

func (m *mcpServer) toolListPendingApprovals(call *mcpCall, raw json.RawMessage) (any, error) {
	var args struct {
		Task           string  `json:"task"`
		Namespace      string  `json:"namespace"`
		IncludeDecided mcpBool `json:"includeDecided"`
	}
	if err := decodeMCPArguments(raw, &args); err != nil {
		return nil, err
	}
	if err := (mcpNamedArgs{Name: args.Task, Namespace: args.Namespace}).validate("task"); err != nil {
		return nil, err
	}
	listed, err := m.listApprovals(call.c, args.Namespace, args.Task)
	if err != nil {
		return nil, err
	}
	result := make([]mcpApprovalSummary, 0, len(listed.Approvals))
	for _, approval := range listed.Approvals {
		if approval.Status == approvals.StatusPending || args.IncludeDecided.value {
			result = append(result, summarizeMCPApproval(approval))
		}
		if len(result) == mcpMaxCollectionEntries {
			break
		}
	}
	return map[string]any{
		"task": listed.TaskName, "namespace": listed.Namespace, "taskPhase": listed.TaskPhase,
		"taskDeleting": listed.TaskDeleting, "approvals": result,
	}, nil
}

func validateMCPApprovalID(id string) error {
	if strings.TrimSpace(id) == "" {
		return mcpArgumentError("approvalId is required")
	}
	// Dot segments would be normalized out of the replayed path and address a
	// different route, so they are refused rather than escaped.
	if id == "." || id == ".." || len(id) > mcpMaxApprovalIDLength || !utf8.ValidString(id) ||
		strings.ContainsFunc(id, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return mcpArgumentError("approvalId is invalid")
	}
	return nil
}

// decideApproval records a decision through the REST decision route, which
// owns every decision rule: permissions, Task and run binding, first
// decision wins, and refusal for finished or deleting Tasks.
func (m *mcpServer) decideApproval(c fiber.Ctx, namespace, task, approvalID, decision, reason string) (approvals.Approval, error) {
	var decided approvals.Approval
	path := mcpTaskPath(task, "approvals", url.PathEscape(approvalID), "decision")
	err := m.restJSON(c, fiber.MethodPost, path, mcpNamespaceQuery(namespace),
		ApprovalDecisionRequest{Decision: decision, Reason: reason}, &decided)
	return decided, err
}

func (m *mcpServer) toolDecideApproval(call *mcpCall, raw json.RawMessage) (any, error) {
	var args struct {
		Task       string `json:"task"`
		Namespace  string `json:"namespace"`
		ApprovalID string `json:"approvalId"`
		Decision   string `json:"decision"`
		Reason     string `json:"reason"`
	}
	if err := decodeMCPArguments(raw, &args); err != nil {
		return nil, err
	}
	if err := (mcpNamedArgs{Name: args.Task, Namespace: args.Namespace}).validate("task"); err != nil {
		return nil, err
	}
	if err := validateMCPApprovalID(args.ApprovalID); err != nil {
		return nil, err
	}
	if args.Decision != approvalDecisionApprove && args.Decision != approvalDecisionDecline {
		return nil, mcpArgumentError("decision must be approve or decline")
	}
	if utf8.RuneCountInString(args.Reason) > mcpMaxReasonLength {
		return nil, mcpArgumentError(fmt.Sprintf("reason must be at most %d characters", mcpMaxReasonLength))
	}
	decided, err := m.decideApproval(call.c, args.Namespace, args.Task, args.ApprovalID, args.Decision, args.Reason)
	if err != nil {
		return nil, err
	}
	return summarizeMCPApproval(decided), nil
}

// --- Results and artifacts ---

type mcpOutputArgs struct {
	mcpNamedArgs
	Offset   mcpInt `json:"offset"`
	MaxBytes mcpInt `json:"maxBytes"`
}

func (a mcpOutputArgs) window() (int64, int64, error) {
	offset, err := a.Offset.bounded("offset", 0, 1<<40, 0)
	if err != nil {
		return 0, 0, err
	}
	limit, err := a.MaxBytes.bounded("maxBytes", 1, mcpMaxOutputBytes, mcpDefaultOutputBytes)
	return offset, limit, err
}

// mcpOutputChunk redacts the whole text first, so a credential split by a
// chunk boundary is still removed, then returns one UTF-8-safe window.
func mcpOutputChunk(text string, offset, limit int64) map[string]any {
	redacted := redact.SensitiveText(text)
	total := int64(len(redacted))
	start := min(offset, total)
	for start < total && start > 0 && !utf8.RuneStart(redacted[start]) {
		start++
	}
	end := min(start+limit, total)
	for end < total && end > start && !utf8.RuneStart(redacted[end]) {
		end--
	}
	chunk := map[string]any{
		"content": redacted[start:end], "offset": start, "returnedBytes": end - start,
		"totalBytes": total, "truncated": end < total,
	}
	if end < total {
		chunk["nextOffset"] = end
	}
	return chunk
}

func (m *mcpServer) toolGetTaskResult(call *mcpCall, raw json.RawMessage) (any, error) {
	var args mcpOutputArgs
	if err := decodeMCPArguments(raw, &args); err != nil {
		return nil, err
	}
	if err := args.validate("name"); err != nil {
		return nil, err
	}
	offset, limit, err := args.window()
	if err != nil {
		return nil, err
	}
	var response struct {
		Result string `json:"result"`
	}
	if err := m.restJSON(call.c, fiber.MethodGet, mcpTaskPath(args.Name, "result"), mcpNamespaceQuery(args.Namespace), nil, &response); err != nil {
		return nil, err
	}
	chunk := mcpOutputChunk(response.Result, offset, limit)
	chunk["name"] = args.Name
	return chunk, nil
}

type mcpArtifactSummary struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType,omitempty"`
	Size        int64  `json:"size"`
	CreatedAt   string `json:"createdAt,omitempty"`
}

func (m *mcpServer) toolListTaskArtifacts(call *mcpCall, raw json.RawMessage) (any, error) {
	var args mcpNamedArgs
	if err := decodeMCPArguments(raw, &args); err != nil {
		return nil, err
	}
	if err := args.validate("name"); err != nil {
		return nil, err
	}
	var response struct {
		Artifacts []struct {
			Filename    string    `json:"filename"`
			ContentType string    `json:"contentType"`
			Size        int64     `json:"size"`
			CreatedAt   time.Time `json:"createdAt"`
		} `json:"artifacts"`
	}
	if err := m.restJSON(call.c, fiber.MethodGet, mcpTaskPath(args.Name, "artifacts"), mcpNamespaceQuery(args.Namespace), nil, &response); err != nil {
		return nil, err
	}
	artifacts := make([]mcpArtifactSummary, 0, min(len(response.Artifacts), mcpMaxCollectionEntries))
	for _, artifact := range response.Artifacts {
		if len(artifacts) == mcpMaxCollectionEntries {
			break
		}
		artifacts = append(artifacts, mcpArtifactSummary{
			Filename: artifact.Filename, ContentType: artifact.ContentType, Size: artifact.Size, CreatedAt: mcpTimeValue(artifact.CreatedAt),
		})
	}
	return map[string]any{
		"name": args.Name, "artifacts": artifacts, "truncated": len(response.Artifacts) > len(artifacts),
	}, nil
}

func validateMCPArtifactFilename(filename string) error {
	if filename == "" {
		return mcpArgumentError("filename is required")
	}
	// The REST route matches the raw path segment, so only names that need
	// no escaping can be addressed through it.
	if len(filename) > 255 || filename == "." || filename == ".." || url.PathEscape(filename) != filename {
		return mcpArgumentError("filename must be a plain artifact name; download other artifacts through the REST API")
	}
	return nil
}

func (m *mcpServer) toolGetTaskArtifact(call *mcpCall, raw json.RawMessage) (any, error) {
	var args struct {
		mcpOutputArgs
		Filename string `json:"filename"`
	}
	if err := decodeMCPArguments(raw, &args); err != nil {
		return nil, err
	}
	if err := args.validate("name"); err != nil {
		return nil, err
	}
	if err := validateMCPArtifactFilename(args.Filename); err != nil {
		return nil, err
	}
	offset, limit, err := args.window()
	if err != nil {
		return nil, err
	}
	response, err := m.rest(call.c, fiber.MethodGet, mcpTaskPath(args.Name, "artifacts", args.Filename), mcpNamespaceQuery(args.Namespace), nil)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(response.body) || bytes.IndexByte(response.body, 0) >= 0 {
		return nil, mcpArgumentError(fmt.Sprintf("artifact %q is binary (%d bytes); download it through the REST API", args.Filename, len(response.body)))
	}
	chunk := mcpOutputChunk(string(response.body), offset, limit)
	chunk["name"] = args.Name
	chunk["filename"] = args.Filename
	return chunk, nil
}
