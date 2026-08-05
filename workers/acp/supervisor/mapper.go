package supervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const acpUpdateToolCall = "tool_call"

func promptContentToACP(blocks []harnessv2.ContentBlock) ([]acp.ContentBlock, error) {
	result := make([]acp.ContentBlock, 0, len(blocks))
	for index, block := range blocks {
		switch block.Type {
		case harnessv2.ContentBlockText:
			result = append(result, acp.Text(block.Text))
		case harnessv2.ContentBlockResourceLink:
			result = append(result, acp.ContentBlock{Type: "resource_link", URI: block.URI, Name: block.Name, MIMEType: block.MimeType})
		case harnessv2.ContentBlockArtifactRef:
			return nil, fmt.Errorf("prompt artifact reference %d must be materialized before ACP dispatch", index)
		default:
			return nil, fmt.Errorf("unsupported prompt content type %q", block.Type)
		}
	}
	return result, nil
}

func mapACPUpdate(notification *acp.SessionNotification) (*harnessv2.UpdateEvent, string, bool, error) {
	if notification == nil {
		return nil, "", false, nil
	}
	var envelope struct {
		SessionUpdate string                   `json:"sessionUpdate"`
		Content       json.RawMessage          `json:"content"`
		ToolCallID    string                   `json:"toolCallId"`
		Title         string                   `json:"title"`
		Kind          string                   `json:"kind"`
		Status        harnessv2.ToolCallStatus `json:"status"`
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
		if content.Type != "text" || content.Text == "" {
			return nil, "", false, nil
		}
		if strings.TrimSpace(content.Text) == "" {
			return nil, content.Text, false, nil
		}
		return &harnessv2.UpdateEvent{
			Kind:             harnessv2.UpdateAssistantMessageChunk,
			AssistantMessage: &harnessv2.AssistantMessageChunk{Text: content.Text},
		}, content.Text, true, nil
	case acpUpdateToolCall, "tool_call_update":
		toolCallID, err := canonicalACPToolCallID(envelope.ToolCallID)
		if err != nil {
			return nil, "", false, err
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
				ToolCallID: toolCallID, Title: envelope.Title, Kind: envelope.Kind, Status: status,
			},
		}, "", true, nil
	case "plan":
		if len(envelope.Entries) == 0 {
			return nil, "", false, nil
		}
		return &harnessv2.UpdateEvent{Kind: harnessv2.UpdatePlan, Plan: &harnessv2.PlanUpdate{Entries: envelope.Entries}}, "", true, nil
	case "usage_update":
		return &harnessv2.UpdateEvent{Kind: harnessv2.UpdateUsage, Usage: &harnessv2.UsageUpdate{InputTokens: envelope.Used}}, "", true, nil
	case "agent_thought_chunk", "user_message_chunk", "available_commands_update", "current_mode_update", "config_option_update", "session_info_update":
		return nil, "", false, nil
	default:
		return nil, "", false, nil
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

func mapPermission(event *acp.PermissionRequestEvent, at time.Time, ttl time.Duration) (*harnessv2.PermissionRequestedEvent, error) {
	if event == nil {
		return nil, fmt.Errorf("ACP permission event is required")
	}
	var toolCall struct {
		ToolCallID string `json:"toolCallId"`
		ToolName   string `json:"name"`
		Title      string `json:"title"`
	}
	if err := json.Unmarshal(event.Request.ToolCall, &toolCall); err != nil {
		return nil, fmt.Errorf("decode ACP permission tool call: %w", err)
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
		ToolName: toolCall.ToolName, Title: toolCall.Title, Options: options, ExpiresAt: at.Add(ttl),
	}, nil
}
