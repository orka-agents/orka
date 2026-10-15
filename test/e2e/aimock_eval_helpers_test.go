//go:build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"time"
)

// Capture retains only request bodies, never authentication headers. These
// helpers must not include bodies or canary values in errors or diagnostics.
type mockLLMCaptureEntry struct {
	RequestID string `json:"requestId"`
	Path      string `json:"path"`
	Method    string `json:"method"`
	Body      string `json:"body"`
	Status    int    `json:"status"`
	Complete  bool   `json:"complete"`
	Error     string `json:"error"`
}

type mockLLMCaptureState struct {
	InstanceID string                `json:"instanceId"`
	Entries    []mockLLMCaptureEntry `json:"entries"`
	Rejected   int                   `json:"rejected"`
}

type mockLLMEvalSnapshot struct {
	instanceID string
	requestIDs map[string]bool
	rejected   int
}

type mockLLMEvalRequest struct {
	capture mockLLMCaptureEntry
	journal mockLLMRequest
}

const (
	mockLLMCaptureReadTimeout  = 15 * time.Second
	mockLLMCaptureFetchTimeout = 12 * time.Second
)

func (m mockLLMInstance) captureJournal() (mockLLMCaptureState, error) {
	ctx, cancel := context.WithTimeout(context.Background(), mockLLMCaptureReadTimeout)
	defer cancel()
	return m.captureJournalWithin(ctx)
}

func (m mockLLMInstance) captureJournalWithin(ctx context.Context) (mockLLMCaptureState, error) {
	cmd := exec.CommandContext(ctx, "kubectl", "exec", "-n", m.namespace, "deployment/"+m.name, "-c", "aimock",
		"--", "node", "-e", mockLLMReadScript(m.port, "/__orka_eval/capture", mockLLMCaptureFetchTimeout))
	// Bound inherited pipe waits as well as kubectl itself. Neither failure
	// path may expose captured output through a subprocess error formatter.
	cmd.WaitDelay = time.Second
	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return mockLLMCaptureState{}, fmt.Errorf("mock request capture read expired: %w", ctx.Err())
		}
		return mockLLMCaptureState{}, fmt.Errorf("read mock request capture: %w", err)
	}
	var state mockLLMCaptureState
	if err := json.Unmarshal(output, &state); err != nil {
		return state, fmt.Errorf("decode mock request capture: %w", err)
	}
	return state, nil
}

// mockLLMReadScript streams one aimock pod-loopback endpoint to stdout. The
// whole read, including the final stdout flush, ends by the remote deadline.
func mockLLMReadScript(port int, path string, timeout time.Duration) string {
	return fmt.Sprintf(`const deadline = setTimeout(() => process.exit(1), %d);
		fetch("http://127.0.0.1:%d%s", {
			signal: AbortSignal.timeout(%d)
		}).then(async r => {
			if (!r.ok) throw Error("HTTP " + r.status);
			for await (const chunk of r.body) {
				await new Promise((resolve, reject) => {
					process.stdout.write(Buffer.from(chunk), err => err ? reject(err) : resolve());
				});
			}
		}).catch(() => process.exit(1)).finally(() => clearTimeout(deadline))`,
		timeout.Milliseconds(), port, path, timeout.Milliseconds())
}

// evalSnapshot isolates a sequential case without clearing another case's
// journal, resetting fixtures, or depending on marker-only filtering.
func (m mockLLMInstance) evalSnapshot() (mockLLMEvalSnapshot, error) {
	state, err := m.captureJournal()
	if err != nil {
		return mockLLMEvalSnapshot{}, err
	}
	if state.InstanceID == "" {
		return mockLLMEvalSnapshot{}, fmt.Errorf("mock request capture has no instance identity")
	}
	ids := make(map[string]bool, len(state.Entries))
	for _, entry := range state.Entries {
		ids[entry.RequestID] = true
	}
	return mockLLMEvalSnapshot{instanceID: state.InstanceID, requestIDs: ids, rejected: state.Rejected}, nil
}

func (m mockLLMInstance) evalRequestsSince(
	before mockLLMEvalSnapshot, markers ...string,
) ([]mockLLMEvalRequest, error) {
	state, err := m.captureJournal()
	if err != nil {
		return nil, err
	}
	journal, err := m.journal()
	if err != nil {
		return nil, err
	}
	return validateMockLLMEvalCapture(before, state, journal, markers)
}

// evalNoRequestsSince proves an intentionally rejected path never reached the
// model. It still rejects collector resets, lost identities and capacity failures.
func (m mockLLMInstance) evalNoRequestsSince(before mockLLMEvalSnapshot) error {
	state, err := m.captureJournal()
	if err != nil {
		return err
	}
	return validateMockLLMEvalNoRequests(before, state)
}

func validateMockLLMEvalNoRequests(before mockLLMEvalSnapshot, state mockLLMCaptureState) error {
	if err := validateMockLLMEvalCaptureIntegrity(before, state); err != nil {
		return err
	}
	if len(state.Entries) != len(before.requestIDs) {
		return fmt.Errorf("rejected path reached model inference")
	}
	return nil
}

func validateMockLLMEvalCaptureIntegrity(before mockLLMEvalSnapshot, state mockLLMCaptureState) error {
	if before.instanceID == "" || state.InstanceID != before.instanceID {
		return fmt.Errorf("mock request capture restarted or has no instance identity")
	}
	retained := make(map[string]bool, len(state.Entries))
	for _, entry := range state.Entries {
		if entry.RequestID == "" || retained[entry.RequestID] {
			return fmt.Errorf("mock request capture has missing or duplicate request identities")
		}
		retained[entry.RequestID] = true
	}
	for id := range before.requestIDs {
		if !retained[id] {
			return fmt.Errorf("mock request capture lost previously observed requests")
		}
	}
	if state.Rejected != before.rejected {
		return fmt.Errorf("mock request capture rejected requests during the eval")
	}
	return nil
}

func validateMockLLMEvalCapture(
	before mockLLMEvalSnapshot, state mockLLMCaptureState,
	journal []mockLLMRequest, markers []string,
) ([]mockLLMEvalRequest, error) {
	if err := validateMockLLMEvalCaptureIntegrity(before, state); err != nil {
		return nil, err
	}
	byID := make(map[string]mockLLMRequest, len(journal))
	for _, request := range journal {
		byID[request.correlationID()] = request
	}
	served := make(map[string]bool, len(markers))
	var requests []mockLLMEvalRequest
	for _, entry := range state.Entries {
		if before.requestIDs[entry.RequestID] {
			continue
		}
		index := len(requests)
		if entry.RequestID == "" || !entry.Complete || entry.Error != "" {
			return nil, fmt.Errorf("model request %d was not completely observed", index)
		}
		if entry.Status < 200 || entry.Status >= 300 {
			return nil, fmt.Errorf("model request %d failed with HTTP %d", index, entry.Status)
		}
		if !json.Valid([]byte(entry.Body)) {
			return nil, fmt.Errorf("model request %d was not a complete JSON body", index)
		}
		request, ok := byID[entry.RequestID]
		if !ok {
			return nil, fmt.Errorf("model request %d has no matching aimock receipt", index)
		}
		var fixture struct {
			Match struct {
				UserMessage string `json:"userMessage"`
			} `json:"match"`
		}
		if err := json.Unmarshal(request.Response.Fixture, &fixture); err != nil {
			return nil, fmt.Errorf("model request %d has no decoded fixture receipt", index)
		}
		for _, marker := range markers {
			if fixture.Match.UserMessage == marker && strings.Contains(entry.Body, marker) {
				served[marker] = true
			}
		}
		// Runtime title/summary requests may use the catch-all and have no
		// tools. Tool-enabled eval turns must be served by their script.
		var raw struct {
			Tools []json.RawMessage `json:"tools"`
		}
		_ = json.Unmarshal([]byte(entry.Body), &raw)
		if len(raw.Tools) != 0 {
			for _, marker := range markers {
				if strings.Contains(entry.Body, marker) && !slices.Contains(markers, fixture.Match.UserMessage) {
					return nil, fmt.Errorf("model request %d used an unexpected fixture", index)
				}
			}
		}
		request.RawBody = json.RawMessage(entry.Body)
		requests = append(requests, mockLLMEvalRequest{capture: entry, journal: request})
	}
	if len(requests) == 0 {
		return nil, fmt.Errorf("eval observed no model requests")
	}
	for _, marker := range markers {
		if !served[marker] {
			return nil, fmt.Errorf("eval has no intended fixture receipt for %s", marker)
		}
	}
	return requests, nil
}

// checkModelContentCanaries checks every captured request, including auxiliary
// requests without the case marker. Decode strings too, so JSON escaping cannot
// turn the check into a false negative. Errors identify roles, never values.
func checkModelContentCanaries(requests []mockLLMEvalRequest, canaries map[string]string) error {
	roles := make([]string, 0, len(canaries))
	for role, canary := range canaries {
		if canary == "" {
			return fmt.Errorf("empty credential canary for role %s", role)
		}
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for index, request := range requests {
		var body any
		if err := json.Unmarshal([]byte(request.capture.Body), &body); err != nil {
			return fmt.Errorf("model request %d is not a complete JSON body", index)
		}
		for _, role := range roles {
			if strings.Contains(request.capture.Body, canaries[role]) || jsonContainsCanary(body, canaries[role]) {
				return fmt.Errorf("credential canary exposed in model request %d (role %s)", index, role)
			}
		}
	}
	if len(requests) == 0 {
		return fmt.Errorf("cannot check credential containment without model requests")
	}
	return nil
}

func jsonContainsCanary(value any, canary string) bool {
	switch value := value.(type) {
	case string:
		return strings.Contains(value, canary)
	case []any:
		for _, item := range value {
			if jsonContainsCanary(item, canary) {
				return true
			}
		}
	case map[string]any:
		for key, item := range value {
			if strings.Contains(key, canary) || jsonContainsCanary(item, canary) {
				return true
			}
		}
	}
	return false
}

// evalMessages recovers tool exchanges from oversized raw Chat, Responses,
// and Anthropic requests. Unknown truncated shapes cannot manufacture a
// successful tool assertion. Small requests use aimock's canonical messages.
func (r mockLLMRequest) evalMessages() []mockLLMMessage {
	if !r.Body.Truncated {
		return r.Body.Messages
	}
	var raw struct {
		Messages []mockLLMMessage `json:"messages"`
		Input    []struct {
			Type      string          `json:"type"`
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Output    json.RawMessage `json:"output"`
		} `json:"input"`
	}
	if json.Unmarshal(r.RawBody, &raw) != nil {
		return nil
	}
	var messages []mockLLMMessage
	for _, message := range raw.Messages {
		var blocks []struct {
			Type      string          `json:"type"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(message.Content, &blocks)
		for _, block := range blocks {
			switch block.Type {
			case "tool_use":
				message.ToolCalls = append(message.ToolCalls, evalToolCall(block.ID, block.Name, block.Input))
			case "tool_result":
				messages = append(messages, mockLLMMessage{Role: "tool", ToolCallID: block.ToolUseID, Content: block.Content})
			}
		}
		messages = append(messages, message)
	}
	for _, input := range raw.Input {
		switch input.Type {
		case "function_call":
			messages = append(messages, mockLLMMessage{
				Role:      "assistant",
				ToolCalls: []mockLLMToolCall{evalToolCall(input.CallID, input.Name, input.Arguments)},
			})
		case "function_call_output":
			messages = append(messages, mockLLMMessage{Role: "tool", ToolCallID: input.CallID, Content: input.Output})
		default:
			if input.Role != "" {
				messages = append(messages, mockLLMMessage{Role: input.Role, Content: input.Content})
			}
		}
	}
	return messages
}

func evalToolCall(id, name string, arguments json.RawMessage) mockLLMToolCall {
	call := mockLLMToolCall{ID: id}
	call.Function.Name = name
	if json.Unmarshal(arguments, &call.Function.Arguments) != nil {
		call.Function.Arguments = string(arguments)
	}
	return call
}

func (r mockLLMRequest) correlationID() string {
	if r.Headers.RequestID != "" {
		return r.Headers.RequestID
	}
	return r.RequestID
}
