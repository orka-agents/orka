package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/workers/common"
)

const analysisMaxInvestigationToolCalls = 20

func (g *analysisLoopGuard) selectToolCalls(calls []llm.ToolCall) []llm.ToolCall {
	if !g.validationRequired || len(calls) <= 1 {
		return calls
	}
	for _, call := range calls {
		if !g.timelineVerified && g.isTimelineTool(call.Name) {
			return []llm.ToolCall{call}
		}
	}
	for _, call := range calls {
		if g.isValidationTool(call.Name) {
			return []llm.ToolCall{call}
		}
	}
	return calls[:1]
}

func (g *analysisLoopGuard) beginToolCall(name string) error {
	if !g.validationRequired || g.isFinalizationTool(name) {
		return nil
	}
	if g.investigationToolCalls >= analysisMaxInvestigationToolCalls {
		return fmt.Errorf("analysis investigation tool-call budget exhausted")
	}
	g.investigationToolCalls++
	return nil
}

func (g *analysisLoopGuard) cachedToolResult(
	name string,
	args json.RawMessage,
	tool *corev1alpha1.Tool,
) (string, bool) {
	if !cacheIdenticalToolCalls(tool, g.isFinalizationTool(name)) {
		return "", false
	}
	result, ok := g.toolResults[toolCallFingerprint(name, args)]
	return result, ok
}

func (g *analysisLoopGuard) rememberToolResult(
	name string,
	args json.RawMessage,
	result string,
	tool *corev1alpha1.Tool,
) {
	if !cacheIdenticalToolCalls(tool, g.isFinalizationTool(name)) {
		return
	}
	g.toolResults[toolCallFingerprint(name, args)] = result
}

func toolCallFingerprint(name string, args json.RawMessage) string {
	canonical := bytes.TrimSpace(args)
	var value any
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.UseNumber()
	if json.Valid(canonical) && decoder.Decode(&value) == nil {
		if data, err := json.Marshal(value); err == nil {
			canonical = data
		}
	}
	sum := sha256.Sum256(append([]byte(strings.TrimSpace(name)+"\x00"), canonical...))
	return hex.EncodeToString(sum[:])
}

func recordSkippedAnalysisToolCalls(
	recorder common.EventRecorder,
	original, selected []llm.ToolCall,
) {
	selectedIDs := make(map[string]bool, len(selected))
	for _, call := range selected {
		selectedIDs[call.ID] = true
	}
	for _, call := range original {
		if selectedIDs[call.ID] {
			continue
		}
		common.RecordEventWithTimeout(
			recorder,
			events.ExecutionEventTypeToolCallSkipped,
			modelLoopEventTimeout,
			common.WithEventSeverity(events.ExecutionEventSeverityWarning),
			common.WithEventToolName(call.Name),
			common.WithEventToolCallID(call.ID),
			common.WithEventSummary("parallel tool call skipped by analysis policy"),
		)
	}
}
