package supervisor

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// codexCompletedOutputNormalizer understands only the completed command/MCP shapes
// emitted by codex-acp 1.1.7, commit 307d81018f7cc0c3141ddf71c7532d38310e2cfb:
// src/CodexToolCallMapper.ts:createCommandActionEvent/createTerminalCommandEvent
// and src/CodexEventHandler.ts:completeCommandExecutionEvent/completeItemEvent. Its caller must
// additionally verify the configured Codex adapter name and pinned digest.
//
// State belongs to one prompt. Retaining bounded identity tombstones prevents
// duplicate starts or completions from authorizing output for a reused call ID.
// Provider output, command text, and raw metadata are never retained here.
type codexCompletedOutputNormalizer struct {
	calls map[string]codexCommandOutputCall
}

type codexCommandOutputCall struct {
	kind     codexCommandOutputKind
	mcpName  string
	invalid  bool
	finished bool
}

type codexCommandOutputKind uint8

const (
	codexCommandOutputUnknown codexCommandOutputKind = iota
	codexCommandOutputAction
	codexCommandOutputTerminal
	codexCommandOutputMCP
)

type codexCommandOutputEnvelope struct {
	SessionUpdate string                     `json:"sessionUpdate"`
	ToolCallID    string                     `json:"toolCallId"`
	Kind          string                     `json:"kind"`
	Status        harnessv2.ToolCallStatus   `json:"status"`
	Content       json.RawMessage            `json:"content"`
	RawInput      json.RawMessage            `json:"rawInput"`
	RawOutput     json.RawMessage            `json:"rawOutput"`
	Meta          map[string]json.RawMessage `json:"_meta"`
}

func (n *codexCompletedOutputNormalizer) normalize(notification *acp.SessionNotification, mapped *harnessv2.UpdateEvent, identity rememberedACPToolCall) {
	if notification == nil || mapped == nil || mapped.ToolCall == nil {
		return
	}
	var envelope codexCommandOutputEnvelope
	if json.Unmarshal(notification.Update, &envelope) != nil {
		return
	}
	// The generic mapper does not project rawOutput. Preserve that omission
	// even for an unsupported or untracked call; only an exact completion below
	// may replace it with an authoritative snapshot.
	if len(envelope.RawOutput) > 0 {
		mapped.ToolCall.Content = nil
		mapped.ToolCall.ContentOmitted = true
		mapped.ToolCall.ContentReplace = false
	}
	id, err := canonicalACPToolCallID(envelope.ToolCallID)
	if err != nil || id != mapped.ToolCall.ToolCallID {
		return
	}
	if envelope.SessionUpdate == acpUpdateToolCall {
		if previous, exists := n.calls[id]; exists {
			previous.invalid = true
			n.calls[id] = previous
			return
		}
		if len(n.calls) >= harnessv2.MaxRuntimeSessionTombstoneOperations {
			return
		}
		if n.calls == nil {
			n.calls = make(map[string]codexCommandOutputCall)
		}
		n.calls[id] = newCodexOutputCall(envelope, identity)
		return
	}
	if envelope.SessionUpdate != acpUpdateToolCallUpdate {
		return
	}
	call, exists := n.calls[id]
	if !exists || call.kind == codexCommandOutputUnknown {
		return
	}
	if envelope.Status != harnessv2.ToolCallStatusCompleted && envelope.Status != harnessv2.ToolCallStatusFailed {
		// The pinned adapter streams only metadata before completion. An
		// unexpected content snapshot must not later have its omission erased.
		if len(envelope.Content) > 0 || len(envelope.RawOutput) > 0 {
			call.invalid = true
			n.calls[id] = call
		}
		return
	}
	valid := !call.invalid && !call.finished && len(envelope.Content) == 0
	call.finished = true
	n.calls[id] = call
	var text string
	var ok bool
	if valid {
		if call.kind == codexCommandOutputMCP {
			if identity.codexMCP && identity.name == call.mcpName {
				text, ok = codexCompletedMCPText(envelope, call.mcpName)
			}
		} else {
			text, ok = codexCompletedCommandText(envelope, call.kind)
		}
	}
	if !ok {
		mapped.ToolCall.Content = nil
		mapped.ToolCall.ContentReplace = false
		mapped.ToolCall.ContentOmitted = true
		return
	}
	// Do not redact or truncate fragments here. The controller journal must
	// inspect this entire logical snapshot together with title/kind/history.
	// Its replacement semantics also clear the start's terminal-reference
	// omission, but only when this authoritative completion fits all bounds.
	mapped.ToolCall.Content = nil
	if text != "" {
		mapped.ToolCall.Content = []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: text}}
	}
	mapped.ToolCall.ContentReplace = true
	mapped.ToolCall.ContentOmitted = false
}

// invalidateUnmappedOutput retains omission state for a known call when the
// generic mapper suppresses an otherwise invisible provider-specific output.
// It cannot create call identity, authorize content, or emit a public event.
func (n *codexCompletedOutputNormalizer) invalidateUnmappedOutput(notification *acp.SessionNotification) {
	if notification == nil {
		return
	}
	var envelope codexCommandOutputEnvelope
	if json.Unmarshal(notification.Update, &envelope) != nil ||
		envelope.SessionUpdate != acpUpdateToolCallUpdate ||
		(len(envelope.Content) == 0 && len(envelope.RawOutput) == 0) {
		return
	}
	id, err := canonicalACPToolCallID(envelope.ToolCallID)
	if err != nil {
		return
	}
	if call, exists := n.calls[id]; exists {
		call.invalid = true
		n.calls[id] = call
	}
}

func newCodexOutputCall(envelope codexCommandOutputEnvelope, identity rememberedACPToolCall) codexCommandOutputCall {
	call := codexCommandOutputCall{kind: codexCommandStartKind(envelope)}
	if codexMCPOutputStart(envelope) {
		call.kind = codexCommandOutputMCP
		call.mcpName = identity.name
		call.invalid = !identity.codexMCP || identity.name == "" ||
			!codexMCPOutputInputMatches(envelope.RawInput, identity.name) ||
			envelope.Status != harnessv2.ToolCallStatusInProgress || envelope.Kind != acpToolKindExecute ||
			len(envelope.Content) != 0 || len(envelope.RawOutput) != 0 || len(envelope.Meta) != 1
	}
	return call
}

func codexCommandStartKind(envelope codexCommandOutputEnvelope) codexCommandOutputKind {
	if envelope.Status != harnessv2.ToolCallStatusInProgress || len(envelope.RawOutput) != 0 {
		return codexCommandOutputUnknown
	}
	switch envelope.Kind {
	case acpToolKindRead, "search":
		// Other pinned read/search tools (image view, web search and fuzzy
		// search) carry content or rawInput. Command actions carry neither.
		if len(envelope.Content) == 0 && len(envelope.RawInput) == 0 && len(envelope.Meta) == 0 {
			return codexCommandOutputAction
		}
	case acpToolKindExecute:
		var content []struct {
			Type       string `json:"type"`
			TerminalID string `json:"terminalId"`
		}
		var terminal struct {
			TerminalID string `json:"terminal_id"`
		}
		if json.Unmarshal(envelope.Content, &content) == nil && len(content) == 1 &&
			content[0].Type == "terminal" && content[0].TerminalID == envelope.ToolCallID &&
			json.Unmarshal(envelope.Meta["terminal_info"], &terminal) == nil && terminal.TerminalID == envelope.ToolCallID {
			return codexCommandOutputTerminal
		}
	}
	return codexCommandOutputUnknown
}

func codexCompletedCommandText(envelope codexCommandOutputEnvelope, kind codexCommandOutputKind) (string, bool) {
	var output map[string]json.RawMessage
	if json.Unmarshal(envelope.RawOutput, &output) != nil {
		return "", false
	}
	// Unknown output fields could describe truncation or a different tool's
	// result. Only this exact completed command contract is supported.
	for key := range output {
		if key != "formatted_output" && key != "exit_code" {
			return "", false
		}
	}
	var formatted string
	raw := output["formatted_output"]
	if len(raw) == 0 || string(raw) == acpJSONNull || json.Unmarshal(raw, &formatted) != nil {
		return "", false
	}
	exit, ok := codexCommandExitCode(output["exit_code"])
	if !ok {
		return "", false
	}
	if kind == codexCommandOutputTerminal {
		var terminal struct {
			TerminalID string          `json:"terminal_id"`
			ExitCode   json.RawMessage `json:"exit_code"`
			Signal     json.RawMessage `json:"signal"`
		}
		if len(envelope.Meta) != 1 || json.Unmarshal(envelope.Meta["terminal_exit"], &terminal) != nil || terminal.TerminalID != envelope.ToolCallID ||
			string(terminal.Signal) != acpJSONNull {
			return "", false
		}
		terminalExit, valid := codexCommandExitCode(terminal.ExitCode)
		if !valid || terminalExit != exit {
			return "", false
		}
	} else if len(envelope.Meta) != 0 {
		return "", false
	}
	if len(formatted) > harnessv2.MaxPromptContentBytes {
		return "", false
	}
	// Fixed labels would mask raw field boundaries from the journal's
	// cross-field redaction. Preserve the exact completed output, including
	// an explicitly empty snapshot, and retain the adapter's terminal status.
	// Exit codes are checked for consistency only, never inferred or rendered.
	return formatted, true
}

func codexCommandExitCode(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	if strings.TrimSpace(string(raw)) == acpJSONNull {
		return "unknown", true
	}
	var exit int32
	if json.Unmarshal(raw, &exit) != nil {
		return "", false
	}
	return strconv.FormatInt(int64(exit), 10), true
}
