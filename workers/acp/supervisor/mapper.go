package supervisor

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/acp"
	executionevents "github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const (
	acpUpdateToolCall          = "tool_call"
	acpUpdateToolCallUpdate    = "tool_call_update"
	acpUpdateAgentThoughtChunk = "agent_thought_chunk"
	acpContentTypeText         = "text"
	acpContentTypeDiff         = "diff"
	acpContentItemContent      = "content"
	acpJSONNull                = "null"
)

const (
	maxACPToolCallTitleBytes = 1024
	acpToolCallTitleEllipsis = "…"
)

const (
	acpAssistantPhaseCommentary  = "commentary"
	acpAssistantPhaseFinalAnswer = "final_answer"
)

func promptContentToACP(blocks []harnessv2.ContentBlock) ([]acp.ContentBlock, error) {
	result := make([]acp.ContentBlock, 0, len(blocks))
	for index, block := range blocks {
		switch block.Type {
		case harnessv2.ContentBlockText:
			result = append(result, acp.Text(block.Text))
		case harnessv2.ContentBlockResourceLink:
			name := block.Name
			if name == "" {
				name = block.URI
			}
			result = append(result, acp.ContentBlock{Type: "resource_link", URI: block.URI, Name: name, MIMEType: block.MimeType})
		case harnessv2.ContentBlockArtifactRef:
			return nil, fmt.Errorf("prompt artifact reference %d must be materialized before ACP dispatch", index)
		default:
			return nil, fmt.Errorf("unsupported prompt content type %q", block.Type)
		}
	}
	return result, nil
}

func mapACPUpdate(notification *acp.SessionNotification) (*harnessv2.UpdateEvent, string, bool, error) {
	return mapACPUpdateWithOptions(notification, acpUpdateOptions{})
}

// acpUpdateOptions selects what a prompt's controller asked to receive.
type acpUpdateOptions struct {
	// live adds assistant thoughts and the tool name, raw input and file
	// edits; a controller asks for them with the live-updates header.
	live bool
	// rawInputs holds the digest of each tool call's last sent raw input, so
	// an adapter repeating it does not resend it.
	rawInputs map[string]string
	// exact replaces credential values in text before it is bounded, so a
	// credential cut by the bound cannot leave an unmatched prefix.
	exact func(string) string
	// workspaceRoot is the session's workspace, the ACP session cwd; file
	// edits are sent relative to it.
	workspaceRoot string
}

func (o acpUpdateOptions) replace(value string) string {
	if o.exact == nil {
		return value
	}
	return o.exact(value)
}

// newRawInput returns the tool call's raw input if it is new, bounded, and a
// JSON value; otherwise nil.
func (o acpUpdateOptions) newRawInput(toolCallID string, raw json.RawMessage) json.RawMessage {
	raw = json.RawMessage(bytes.TrimSpace(raw))
	if len(raw) == 0 || string(raw) == acpJSONNull || len(raw) > harnessv2.MaxToolCallRawInputBytes || !json.Valid(raw) {
		return nil
	}
	digest := sha256.Sum256(raw)
	encoded := hex.EncodeToString(digest[:])
	if o.rawInputs != nil {
		if o.rawInputs[toolCallID] == encoded {
			return nil
		}
		o.rawInputs[toolCallID] = encoded
	}
	return append(json.RawMessage(nil), raw...)
}

// liveToolContent is a live tool content snapshot: text content and the file
// edits that fit, with whether either was left out.
type liveToolContent struct {
	blocks         []harnessv2.ContentBlock
	diffs          []harnessv2.ToolCallDiff
	contentOmitted bool
	diffsOmitted   bool
}

// mapACPToolCallLiveContent keeps text content and file edits. Terminal or
// unknown content omits the whole snapshot as the non-live mapping does. A
// file edit outside the workspace root, or over the update bound, is left out
// on its own, keeping the text content.
func mapACPToolCallLiveContent(raw json.RawMessage, workspaceRoot string) (liveToolContent, error) {
	var result liveToolContent
	if len(raw) == 0 || string(raw) == acpJSONNull {
		return result, nil
	}
	var items []struct {
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
		Path    string          `json:"path"`
		OldText *string         `json:"oldText"`
		NewText *string         `json:"newText"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return result, fmt.Errorf("decode ACP tool call content: %w", err)
	}
	omitted := liveToolContent{contentOmitted: true}
	diffBytes := 0
	for index, item := range items {
		if item.Type == acpContentTypeDiff {
			relative, inside := workspaceRelativeDiffPath(workspaceRoot, item.Path)
			size := 0
			if item.NewText != nil {
				size += len(*item.NewText)
			}
			if item.OldText != nil {
				size += len(*item.OldText)
			}
			if !inside || item.NewText == nil || len(result.diffs) == harnessv2.MaxToolCallDiffs ||
				diffBytes+size > harnessv2.MaxToolCallDiffBytes {
				result.diffsOmitted = true
				continue
			}
			diff := harnessv2.ToolCallDiff{Path: relative, NewText: *item.NewText}
			if item.OldText != nil {
				old := *item.OldText
				diff.OldText = &old
			}
			if (harnessv2.ToolCallUpdate{ToolCallID: "bound", Status: harnessv2.ToolCallStatusPending,
				Diffs: []harnessv2.ToolCallDiff{diff}}).Validate() != nil {
				result.diffsOmitted = true
				continue
			}
			diffBytes += size
			result.diffs = append(result.diffs, diff)
			continue
		}
		if len(result.blocks) == harnessv2.MaxContentBlocks || item.Type != acpContentItemContent || len(item.Content) == 0 {
			return omitted, nil
		}
		var block acp.ContentBlock
		if err := json.Unmarshal(item.Content, &block); err != nil {
			return result, fmt.Errorf("decode ACP tool call content block %d: %w", index, err)
		}
		projected, ok, bounded, err := projectACPContentBlock(block)
		if err != nil {
			return result, fmt.Errorf("project ACP tool call content block %d: %w", index, err)
		}
		if bounded || !ok {
			return omitted, nil
		}
		result.blocks = append(result.blocks, projected)
	}
	return result, nil
}

// workspaceRelativeDiffPath maps an ACP diff path to a path relative to the
// session's workspace root. It reports false for an empty path, one outside
// the root, or one with ".." segments.
func workspaceRelativeDiffPath(workspaceRoot, value string) (string, bool) {
	if strings.TrimSpace(value) == "" || strings.ContainsRune(value, 0) {
		return "", false
	}
	if strings.Contains("/"+value+"/", "/../") {
		return "", false
	}
	cleaned := path.Clean(value)
	if path.IsAbs(cleaned) {
		root := path.Clean(strings.TrimSpace(workspaceRoot))
		if !path.IsAbs(root) {
			return "", false
		}
		relative, inside := strings.CutPrefix(cleaned, strings.TrimSuffix(root, "/")+"/")
		if !inside {
			return "", false
		}
		cleaned = relative
	}
	if harnessv2.ValidateWorkspaceRelativePath(cleaned) != nil {
		return "", false
	}
	return cleaned, true
}

//nolint:gocyclo // One switch maps every ACP session update kind.
func mapACPUpdateWithOptions(notification *acp.SessionNotification, options acpUpdateOptions) (*harnessv2.UpdateEvent, string, bool, error) {
	if notification == nil {
		return nil, "", false, nil
	}
	var envelope struct {
		acpToolCallIdentity
		SessionUpdate string                   `json:"sessionUpdate"`
		Content       json.RawMessage          `json:"content"`
		Kind          string                   `json:"kind"`
		Status        harnessv2.ToolCallStatus `json:"status"`
		RawInput      json.RawMessage          `json:"rawInput"`
		Entries       []harnessv2.PlanEntry    `json:"entries"`
		Used          uint64                   `json:"used"`
		Size          uint64                   `json:"size"`
	}
	if err := json.Unmarshal(notification.Update, &envelope); err != nil {
		return nil, "", false, fmt.Errorf("decode ACP session update: %w", err)
	}
	switch envelope.SessionUpdate {
	case "agent_message_chunk":
		var content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(envelope.Content, &content); err != nil {
			return nil, "", false, fmt.Errorf("decode ACP agent message content: %w", err)
		}
		if content.Type != acpContentTypeText || content.Text == "" {
			return nil, "", false, nil
		}
		return &harnessv2.UpdateEvent{
			Kind:             harnessv2.UpdateAssistantMessageChunk,
			AssistantMessage: &harnessv2.AssistantMessageChunk{Text: content.Text},
		}, content.Text, true, nil
	case acpUpdateAgentThoughtChunk:
		if !options.live {
			return nil, "", false, nil
		}
		var content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(envelope.Content, &content); err != nil {
			return nil, "", false, fmt.Errorf("decode ACP agent thought content: %w", err)
		}
		if content.Type != acpContentTypeText || content.Text == "" {
			return nil, "", false, nil
		}
		return &harnessv2.UpdateEvent{
			Kind:             harnessv2.UpdateAssistantThoughtChunk,
			AssistantThought: &harnessv2.AssistantMessageChunk{Text: content.Text},
		}, "", true, nil
	case acpUpdateToolCall, acpUpdateToolCallUpdate:
		toolCallID, err := canonicalACPToolCallID(envelope.ToolCallID)
		if err != nil {
			return nil, "", false, err
		}
		contentPresent := len(envelope.Content) > 0
		var toolContent []harnessv2.ContentBlock
		var diffs []harnessv2.ToolCallDiff
		var contentOmitted, diffsOmitted bool
		if options.live {
			var live liveToolContent
			live, err = mapACPToolCallLiveContent(envelope.Content, options.workspaceRoot)
			toolContent, diffs, contentOmitted, diffsOmitted = live.blocks, live.diffs, live.contentOmitted, live.diffsOmitted
		} else {
			toolContent, contentOmitted, err = mapACPToolCallContent(envelope.Content)
		}
		if err != nil {
			return nil, "", false, err
		}
		var name string
		var rawInput json.RawMessage
		if options.live {
			// A conflicting or oversized identity is not an error here: the
			// permission path enforces identity, and the name is display data.
			name, _ = envelope.name()
			rawInput = options.newRawInput(toolCallID, envelope.RawInput)
		}
		// ACP adapters may stream tool output through provider-specific fields
		// that harness v2 deliberately does not project. A status-less update
		// with no visible metadata would otherwise become an unbounded series of
		// synthetic in_progress events carrying identical state. A raw input is
		// visible only when it changed.
		if envelope.SessionUpdate == acpUpdateToolCallUpdate && envelope.Status == "" &&
			envelope.Title == "" && envelope.Kind == "" && !contentPresent && len(rawInput) == 0 {
			return nil, "", false, nil
		}
		status := envelope.Status
		if status == "" {
			if envelope.SessionUpdate == acpUpdateToolCall {
				status = harnessv2.ToolCallStatusPending
			} else {
				status = harnessv2.ToolCallStatusInProgress
			}
		}
		kind := harnessv2.UpdateToolCallUpdate
		if envelope.SessionUpdate == acpUpdateToolCall {
			kind = harnessv2.UpdateToolCall
		}
		return &harnessv2.UpdateEvent{
			Kind: kind,
			ToolCall: &harnessv2.ToolCallUpdate{
				ToolCallID: toolCallID, Title: boundACPToolCallTitle(options.replace(envelope.Title)), Kind: envelope.Kind, Status: status,
				Content: toolContent, ContentReplace: contentPresent && !contentOmitted, ContentOmitted: contentOmitted,
				Name: name, RawInput: rawInput, Diffs: diffs, DiffsOmitted: diffsOmitted,
			},
		}, "", true, nil
	case "plan":
		plan := &harnessv2.PlanUpdate{Entries: envelope.Entries}
		if err := plan.Validate(); err != nil {
			return nil, "", false, nil
		}
		return &harnessv2.UpdateEvent{Kind: harnessv2.UpdatePlan, Plan: plan}, "", true, nil
	case "usage_update":
		usage := &harnessv2.UsageUpdate{
			ContextWindowUsed: &envelope.Used,
			ContextWindowSize: &envelope.Size,
		}
		if err := usage.Validate(); err != nil {
			return nil, "", false, nil
		}
		return &harnessv2.UpdateEvent{Kind: harnessv2.UpdateUsage, Usage: usage}, "", true, nil
	case "user_message_chunk", "available_commands_update", "current_mode_update", "config_option_update", "session_info_update", "notice":
		return nil, "", false, nil
	default:
		return nil, "", false, nil
	}
}

func mapACPToolCallContent(raw json.RawMessage) ([]harnessv2.ContentBlock, bool, error) {
	if len(raw) == 0 || string(raw) == acpJSONNull {
		return nil, false, nil
	}
	var items []struct {
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, false, fmt.Errorf("decode ACP tool call content: %w", err)
	}
	mapped := make([]harnessv2.ContentBlock, 0, len(items))
	for index, item := range items {
		if len(mapped) == harnessv2.MaxContentBlocks {
			return nil, true, nil
		}
		if item.Type != acpContentItemContent || len(item.Content) == 0 {
			return nil, true, nil
		}
		var block acp.ContentBlock
		if err := json.Unmarshal(item.Content, &block); err != nil {
			return nil, false, fmt.Errorf("decode ACP tool call content block %d: %w", index, err)
		}
		projected, ok, bounded, err := projectACPContentBlock(block)
		if err != nil {
			return nil, false, fmt.Errorf("project ACP tool call content block %d: %w", index, err)
		}
		if bounded || !ok {
			return nil, true, nil
		}
		mapped = append(mapped, projected)
	}
	return mapped, false, nil
}

func projectACPContentBlock(block acp.ContentBlock) (harnessv2.ContentBlock, bool, bool, error) {
	var projected harnessv2.ContentBlock
	switch block.Type {
	case acpContentTypeText:
		if block.Text == "" {
			return projected, false, false, nil
		}
		projected = harnessv2.ContentBlock{Type: harnessv2.ContentBlockText, Text: block.Text}
	case "resource_link":
		if block.URI == "" {
			return projected, false, false, nil
		}
		projected = harnessv2.ContentBlock{
			Type: harnessv2.ContentBlockResourceLink, URI: block.URI, Name: block.Name, MimeType: block.MIMEType,
		}
	case "resource":
		var resource struct {
			URI      string `json:"uri"`
			MIMEType string `json:"mimeType"`
			Text     string `json:"text"`
		}
		if len(block.Resource) == 0 || json.Unmarshal(block.Resource, &resource) != nil {
			return projected, false, false, nil
		}
		if resource.Text != "" {
			projected = harnessv2.ContentBlock{Type: harnessv2.ContentBlockText, Text: resource.Text}
		} else if resource.URI != "" {
			projected = harnessv2.ContentBlock{
				Type: harnessv2.ContentBlockResourceLink, URI: resource.URI, MimeType: resource.MIMEType,
			}
		} else {
			return projected, false, false, nil
		}
	default:
		return projected, false, false, nil
	}
	if acpToolContentExceedsHarnessLimits(projected) {
		return harnessv2.ContentBlock{}, false, true, nil
	}
	if err := projected.ValidateToolOutput(); err != nil {
		return harnessv2.ContentBlock{}, false, false, err
	}
	return projected, true, false, nil
}

func acpToolContentExceedsHarnessLimits(block harnessv2.ContentBlock) bool {
	return len(block.Text) > harnessv2.MaxPromptContentBytes ||
		len(block.URI) > harnessv2.MaxResourceURIBytes ||
		len(block.Name) > harnessv2.MaxContentNameBytes ||
		len(block.MimeType) > harnessv2.MaxContentMIMETypeBytes
}

// boundACPToolContentToEventLine keeps a tool update within one event line:
// it drops the live raw input and file edits first, then the content snapshot,
// which receivers then treat as omitted.
func boundACPToolContentToEventLine(event *harnessv2.Event, maxLineBytes int) error {
	if event == nil || event.Update == nil || event.Update.ToolCall == nil {
		return nil
	}
	tool := event.Update.ToolCall
	if len(tool.Content) == 0 && len(tool.Diffs) == 0 && len(tool.RawInput) == 0 {
		return nil
	}
	if maxLineBytes <= 0 {
		return fmt.Errorf("%w: max event line bytes must be positive", harnessv2.ErrEventLineTooLarge)
	}
	fits := func() (bool, error) {
		line, err := json.Marshal(event)
		if err != nil {
			return false, fmt.Errorf("marshal projected ACP tool update: %w", err)
		}
		return len(line) <= maxLineBytes, nil
	}
	if ok, err := fits(); ok || err != nil {
		return err
	}

	original := *tool
	tool.RawInput = nil
	if len(tool.Diffs) > 0 {
		tool.Diffs, tool.DiffsOmitted = nil, true
	}
	if ok, err := fits(); ok || err != nil {
		if err != nil {
			*tool = original
		}
		return err
	}
	if len(tool.Content) > 0 {
		tool.Content = nil
		tool.ContentReplace = false
		tool.ContentOmitted = true
	}
	ok, err := fits()
	if err == nil && !ok {
		err = fmt.Errorf("%w: projected ACP tool update exceeds %d bytes without telemetry content", harnessv2.ErrEventLineTooLarge, maxLineBytes)
	}
	if err != nil {
		*tool = original
	}
	return err
}

func boundACPToolCallTitle(value string) string {
	value = executionevents.RedactExecutionEventText(value)
	if len(value) <= maxACPToolCallTitleBytes {
		return value
	}
	limit := maxACPToolCallTitleBytes - len(acpToolCallTitleEllipsis)
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit] + acpToolCallTitleEllipsis
}

func acpAssistantMessagePhase(notification *acp.SessionNotification) string {
	if notification == nil {
		return ""
	}
	var envelope struct {
		SessionUpdate string `json:"sessionUpdate"`
		Meta          struct {
			Codex struct {
				Phase string `json:"phase"`
			} `json:"codex"`
		} `json:"_meta"`
	}
	if json.Unmarshal(notification.Update, &envelope) != nil || envelope.SessionUpdate != "agent_message_chunk" {
		return ""
	}
	switch envelope.Meta.Codex.Phase {
	case acpAssistantPhaseCommentary, acpAssistantPhaseFinalAnswer:
		return envelope.Meta.Codex.Phase
	default:
		return ""
	}
}

const (
	canonicalACPToolCallIDPrefix = "tool-call-v1-sha256-"
	canonicalACPToolCallIDDomain = "orka.acp.tool-call-id.v1\x00"
)

func canonicalACPToolCallID(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("ACP tool call update omitted toolCallId")
	}
	digest := sha256.Sum256([]byte(canonicalACPToolCallIDDomain + value))
	return canonicalACPToolCallIDPrefix + hex.EncodeToString(digest[:]), nil
}

type acpToolCallIdentity struct {
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"name"`
	Title      string `json:"title"`
	Meta       struct {
		IsMCPToolCall json.RawMessage `json:"is_mcp_tool_call"`
		ClaudeCode    struct {
			ToolName string `json:"toolName"`
		} `json:"claudeCode"`
	} `json:"_meta"`
}

func (identity acpToolCallIdentity) name() (string, error) {
	name := identity.ToolName
	if metaName := identity.Meta.ClaudeCode.ToolName; metaName != "" {
		if name != "" && name != metaName {
			return "", fmt.Errorf("ACP tool call has conflicting tool identities")
		}
		name = metaName
	}
	if len(name) > 253 {
		return "", fmt.Errorf("ACP tool name exceeds the identity limit")
	}
	return name, nil
}

// rememberToolCallName retains structured identities only for this prompt.
// Claude and Codex can identify a tool in a preceding update while their
// permission request carries only the toolCallId, never an authoritative title.
func (prompt *promptState) rememberToolCallName(
	notification *acp.SessionNotification, provider string, policy harnessv2.MCPToolPolicy,
) error {
	if notification == nil {
		return nil
	}
	var call struct {
		acpToolCallIdentity
		SessionUpdate string          `json:"sessionUpdate"`
		Kind          string          `json:"kind"`
		RawInput      json.RawMessage `json:"rawInput"`
	}
	if err := json.Unmarshal(notification.Update, &call); err != nil {
		return fmt.Errorf("decode ACP tool identity: %w", err)
	}
	if call.SessionUpdate != acpUpdateToolCall && call.SessionUpdate != acpUpdateToolCallUpdate {
		return nil
	}
	name, err := call.name()
	if err != nil {
		return err
	}
	outsidePolicy := false
	if provider == providerKindCodex {
		name, err = codexMCPPermissionToolName(call.Meta.IsMCPToolCall, call.Kind, call.RawInput, name, policy)
		// The model can name any MCP server or tool, and the prompt broker
		// refuses a call outside the frozen policy. Record the call without a
		// name, so a permission request for it identifies no tool and a later
		// update cannot give it one.
		outsidePolicy = errors.Is(err, errCodexMCPToolOutsidePolicy)
		if err != nil && !outsidePolicy {
			return err
		}
	}
	if name == "" && !outsidePolicy {
		return nil
	}
	id, err := canonicalACPToolCallID(call.ToolCallID)
	if err != nil {
		return err
	}
	if previous, ok := prompt.toolCallNames[id]; ok {
		if previous != name {
			return fmt.Errorf("ACP tool call identity changed during the prompt")
		}
		return nil
	}
	if len(prompt.toolCallNames) >= harnessv2.MaxRuntimeSessionTombstoneOperations {
		return fmt.Errorf("ACP tool call identity limit exceeded")
	}
	if prompt.toolCallNames == nil {
		prompt.toolCallNames = make(map[string]string)
	}
	prompt.toolCallNames[id] = name
	return nil
}

var errCodexMCPToolOutsidePolicy = errors.New("codex MCP tool identity is outside the frozen broker policy")

// The pinned Codex adapter marks MCP updates and supplies structured server/tool
// fields instead of name. Only the configured Orka server and a frozen brokered
// descriptor can supply an identity; titles and native-tool collisions cannot.
func codexMCPPermissionToolName(marker json.RawMessage, kind string, rawInput json.RawMessage, name string, policy harnessv2.MCPToolPolicy) (string, error) {
	mcpCall, err := codexMCPToolCallMarker(marker)
	if err != nil {
		return "", err
	}
	if !mcpCall {
		if descriptor, allowed := policy.Descriptor(name); allowed && descriptor.Source.Brokered() {
			return "", fmt.Errorf("codex brokered tool identity requires an MCP marker")
		}
		return name, nil
	}
	var input struct {
		Server string `json:"server"`
		Tool   string `json:"tool"`
	}
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return "", fmt.Errorf("codex MCP tool update has invalid structured identity")
	}
	if kind != "execute" {
		return "", fmt.Errorf("codex MCP tool update has an unexpected kind")
	}
	descriptor, allowed := policy.Descriptor(input.Tool)
	if input.Server != supervisorMCPServerName || !allowed || !descriptor.Source.Brokered() {
		return "", errCodexMCPToolOutsidePolicy
	}
	if name != "" && name != descriptor.Name {
		return "", fmt.Errorf("codex MCP tool update has conflicting structured identities")
	}
	return descriptor.Name, nil
}

// Omitted markers are valid on ID-only permission callbacks. A present Codex
// marker must be a JSON boolean on both updates and permission callbacks.
func codexMCPToolCallMarker(marker json.RawMessage) (bool, error) {
	if len(marker) == 0 {
		return false, nil
	}
	var mcpCall *bool
	if err := json.Unmarshal(marker, &mcpCall); err != nil || mcpCall == nil {
		return false, fmt.Errorf("codex MCP tool call has invalid identity marker")
	}
	return *mcpCall, nil
}

func canonicalPermissionToolName(provider string, policy harnessv2.MCPToolPolicy, name string) string {
	// Only the configured Orka server's provider alias can identify a brokered
	// tool. Display titles and another MCP server's names never grant authority.
	if provider == providerKindClaude {
		if tool, prefixed := strings.CutPrefix(name, "mcp__orka__"); prefixed {
			if descriptor, allowed := policy.Descriptor(tool); allowed && descriptor.Source.Brokered() {
				return descriptor.Name
			}
			return name
		}
	}
	for _, descriptor := range policy.Tools {
		if descriptor.Source == harnessv2.MCPToolSourceProviderNative && strings.EqualFold(name, descriptor.Name) {
			return descriptor.Name
		}
	}
	return name
}

func mapPermission(event *acp.PermissionRequestEvent, at time.Time, ttl time.Duration, provider string) (*harnessv2.PermissionRequestedEvent, error) {
	if event == nil {
		return nil, fmt.Errorf("ACP permission event is required")
	}
	var toolCall struct {
		acpToolCallIdentity
		Kind     string          `json:"kind"`
		RawInput json.RawMessage `json:"rawInput"`
	}
	if err := json.Unmarshal(event.Request.ToolCall, &toolCall); err != nil {
		return nil, fmt.Errorf("decode ACP permission tool call: %w", err)
	}
	if provider == providerKindCodex {
		if _, err := codexMCPToolCallMarker(toolCall.Meta.IsMCPToolCall); err != nil {
			return nil, err
		}
	}
	toolName, err := toolCall.name()
	if err != nil {
		return nil, err
	}
	// Copilot CLI 1.0.96 omits name on its native shell permission request.
	// Recognize its command envelope only for this provider; titles and the
	// generic execute kind alone cannot identify an authorized tool. The
	// controller and supervisor still require Bash in the frozen tool policy.
	if provider == providerKindCopilot && toolName == "" && toolCall.Kind == "execute" {
		var input struct {
			Command  string   `json:"command"`
			Commands []string `json:"commands"`
		}
		if json.Unmarshal(toolCall.RawInput, &input) == nil && strings.TrimSpace(input.Command) != "" &&
			len(input.Commands) == 1 && input.Commands[0] == input.Command {
			toolName = providerToolBash
		}
	}
	toolCallID := ""
	if strings.TrimSpace(toolCall.ToolCallID) != "" {
		var err error
		toolCallID, err = canonicalACPToolCallID(toolCall.ToolCallID)
		if err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(toolCall.Title) == "" {
		toolCall.Title = "Agent permission request"
	}
	options := make([]harnessv2.PermissionOption, 0, len(event.Request.Options))
	for _, option := range event.Request.Options {
		kind := harnessv2.PermissionOptionKind(option.Kind)
		switch kind {
		case harnessv2.PermissionOptionAllowOnce, harnessv2.PermissionOptionAllowAlways,
			harnessv2.PermissionOptionRejectOnce, harnessv2.PermissionOptionRejectAlways:
		default:
			return nil, fmt.Errorf("unsupported ACP permission option kind %q", option.Kind)
		}
		options = append(options, harnessv2.PermissionOption{OptionID: option.OptionID, Name: option.Name, Kind: kind})
	}
	return &harnessv2.PermissionRequestedEvent{
		RequestID: harnessv2.PermissionRequestID(event.RequestID), ToolCallID: toolCallID,
		ToolName: toolName, Title: toolCall.Title, Options: options, ExpiresAt: at.Add(ttl),
	}, nil
}
