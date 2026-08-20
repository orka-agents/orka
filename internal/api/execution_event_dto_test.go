/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
)

func TestExecutionEventResponseDTOJSONFieldNames(t *testing.T) {
	createdAt := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)
	contextUsed, contextSize := 50, 200
	response := ExecutionEventResponse{
		ID:                "event-1",
		Namespace:         "default",
		StreamType:        events.ExecutionEventStreamTypeTask,
		StreamID:          "task-1",
		Seq:               1,
		Type:              events.ExecutionEventTypeTaskCreated,
		Severity:          events.ExecutionEventSeverityInfo,
		TaskName:          "task-1",
		SessionName:       "session-a",
		AgentName:         "codex",
		ToolName:          "file_read",
		ToolCallID:        "call-1",
		Provider:          "openai",
		Model:             "gpt-4o",
		StopReason:        "stop",
		InputTokens:       3,
		OutputTokens:      5,
		CachedInputTokens: 2,
		ContextWindowUsed: &contextUsed,
		ContextWindowSize: &contextSize,
		Summary:           "created",
		Content:           json.RawMessage(`{"ok":true}`),
		ContentText:       "hello",
		Truncation:        &events.ExecutionEventTruncation{SummaryTruncated: true, SummaryOriginalChars: 5000},
		CreatedAt:         createdAt,
	}
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	for _, key := range []string{"id", "namespace", "streamType", "streamID", "seq", "type", "severity", "taskName", "sessionName", "agentName", "toolName", "toolCallID", "provider", "model", "stopReason", "inputTokens", "outputTokens", "cachedInputTokens", "contextWindowUsed", "contextWindowSize", "summary", "content", "contentText", "truncation", "createdAt"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("response JSON missing key %q in %s", key, data)
		}
	}
}

func TestExecutionEventResponsePromotesModelTelemetryFields(t *testing.T) {
	storeEvent := store.ExecutionEvent{
		ID:         "event-1",
		Namespace:  "default",
		StreamType: events.ExecutionEventStreamTypeTask,
		StreamID:   "task-1",
		Seq:        2,
		Type:       events.ExecutionEventTypeModelRequestCompleted,
		Severity:   events.ExecutionEventSeverityInfo,
		Content: mustRawJSON(t, map[string]any{
			"provider":          "anthropic",
			"model":             "claude-sonnet-4",
			"inputTokens":       123,
			"outputTokens":      45,
			"cachedInputTokens": 20,
			"stopReason":        "end_turn",
		}),
	}
	response := NewExecutionEventResponse(storeEvent)
	if response.Provider != "anthropic" || response.Model != "claude-sonnet-4" || response.StopReason != "end_turn" {
		t.Fatalf("telemetry strings = provider:%q model:%q stop:%q", response.Provider, response.Model, response.StopReason)
	}
	if response.InputTokens != 123 || response.OutputTokens != 45 || response.CachedInputTokens != 20 {
		t.Fatalf("tokens = %d/%d cached=%d, want 123/45 cached=20", response.InputTokens, response.OutputTokens, response.CachedInputTokens)
	}
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	for _, key := range []string{`"provider":"anthropic"`, `"model":"claude-sonnet-4"`, `"inputTokens":123`, `"outputTokens":45`, `"cachedInputTokens":20`, `"stopReason":"end_turn"`} {
		if !strings.Contains(string(data), key) {
			t.Fatalf("response JSON %s missing %s", data, key)
		}
	}
}

func TestExecutionEventResponsePromotesGenAISemConvTerminalFields(t *testing.T) {
	content := mustRawJSON(t, map[string]any{
		"gen_ai.provider.name":           "openai",
		"gen_ai.request.model":           "requested-model",
		"gen_ai.response.model":          "served-model",
		"gen_ai.response.finish_reasons": []string{"stop"},
		"gen_ai.usage.input_tokens":      7,
		"gen_ai.usage.output_tokens":     11,
	})

	completed := NewExecutionEventResponse(store.ExecutionEvent{
		Type:    events.ExecutionEventTypeModelRequestCompleted,
		Content: content,
	})
	if completed.Provider != compatToolOpenAIProviderType || completed.Model != "served-model" || completed.StopReason != "stop" {
		t.Fatalf("completed telemetry = provider:%q model:%q stop:%q", completed.Provider, completed.Model, completed.StopReason)
	}
	if completed.InputTokens != 7 || completed.OutputTokens != 11 {
		t.Fatalf("completed tokens = %d/%d, want 7/11", completed.InputTokens, completed.OutputTokens)
	}

	started := NewExecutionEventResponse(store.ExecutionEvent{
		Type:    events.ExecutionEventTypeModelRequestStarted,
		Content: content,
	})
	if started.Model != "requested-model" {
		t.Fatalf("started model = %q, want requested-model", started.Model)
	}
}

func TestExecutionEventResponseClampsUint64UsageToPlatformInt(t *testing.T) {
	response := NewExecutionEventResponse(store.ExecutionEvent{
		Type:    events.ExecutionEventTypeModelUsageUpdated,
		Content: json.RawMessage(`{"inputTokens":18446744073709551615,"outputTokens":2,"cachedInputTokens":1}`),
	})
	if response.InputTokens != int(^uint(0)>>1) || response.OutputTokens != 2 || response.CachedInputTokens != 1 {
		t.Fatalf("usage tokens = %d/%d cached=%d", response.InputTokens, response.OutputTokens, response.CachedInputTokens)
	}
}

func TestBoundedJSONNumberIntAcceptsNativeIntBoundary(t *testing.T) {
	want := math.MaxInt - 1
	if got := boundedJSONNumberInt(json.Number(strconv.Itoa(want))); got != want {
		t.Fatalf("native int boundary = %d, want %d", got, want)
	}
}

func TestExecutionEventResponseAcceptsIntegralJSONNumberEncodings(t *testing.T) {
	response := NewExecutionEventResponse(store.ExecutionEvent{
		Type: events.ExecutionEventTypeModelUsageUpdated,
		Content: json.RawMessage(`{
			"inputTokens":123.0,
			"outputTokens":1e3,
			"cachedInputTokens":1.5,
			"contextWindowUsed":-1,
			"contextWindowSize":1e100
		}`),
	})
	if response.InputTokens != 123 || response.OutputTokens != 1000 || response.CachedInputTokens != 0 {
		t.Fatalf("integral/fractional token encodings = %d/%d/%d", response.InputTokens, response.OutputTokens, response.CachedInputTokens)
	}
	if response.ContextWindowUsed == nil || *response.ContextWindowUsed != 0 ||
		response.ContextWindowSize == nil || *response.ContextWindowSize != math.MaxInt {
		t.Fatalf("bounded context encodings = %v/%v", response.ContextWindowUsed, response.ContextWindowSize)
	}
}

func TestExecutionEventResponseBoundsExtremeJSONExponents(t *testing.T) {
	response := NewExecutionEventResponse(store.ExecutionEvent{
		Type: events.ExecutionEventTypeModelUsageUpdated,
		Content: json.RawMessage(`{
			"inputTokens":1e1000000000,
			"outputTokens":1e-1000000000,
			"cachedInputTokens":1.2300e2
		}`),
	})
	if response.InputTokens != math.MaxInt || response.OutputTokens != 0 || response.CachedInputTokens != 123 {
		t.Fatalf("extreme exponent tokens = %d/%d/%d", response.InputTokens, response.OutputTokens, response.CachedInputTokens)
	}
}

func TestExecutionEventResponsePromotesContextWindowWithoutTokenAccounting(t *testing.T) {
	response := NewExecutionEventResponse(store.ExecutionEvent{
		Type: events.ExecutionEventTypeModelContextUpdated,
		Content: mustRawJSON(t, map[string]any{
			"provider": compatToolOpenAIProviderType, "model": "gpt-5",
			"contextWindowUsed": 53_000, "contextWindowSize": 200_000,
		}),
	})
	if response.Provider != compatToolOpenAIProviderType || response.Model != "gpt-5" ||
		response.ContextWindowUsed == nil || *response.ContextWindowUsed != 53_000 ||
		response.ContextWindowSize == nil || *response.ContextWindowSize != 200_000 {
		t.Fatalf("context telemetry = %#v", response)
	}
	if response.InputTokens != 0 || response.OutputTokens != 0 || response.CachedInputTokens != 0 {
		t.Fatalf("context occupancy was promoted as token accounting: %#v", response)
	}
}

func TestExecutionEventResponsePreservesZeroContextOccupancy(t *testing.T) {
	response := NewExecutionEventResponse(store.ExecutionEvent{
		Type:    events.ExecutionEventTypeModelContextUpdated,
		Content: json.RawMessage(`{"contextWindowUsed":0,"contextWindowSize":200000}`),
	})
	if response.ContextWindowUsed == nil || *response.ContextWindowUsed != 0 ||
		response.ContextWindowSize == nil || *response.ContextWindowSize != 200_000 {
		t.Fatalf("zero context telemetry = %#v", response)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	if string(body["contextWindowUsed"]) != "0" || string(body["contextWindowSize"]) != "200000" {
		t.Fatalf("encoded context telemetry = %s", encoded)
	}
}

func TestSubmitExecutionEventRequestJSONFieldNames(t *testing.T) {
	request := SubmitExecutionEventRequest{
		Type:        events.ExecutionEventTypeToolCallStarted,
		Severity:    events.ExecutionEventSeverityDebug,
		TaskName:    "task-1",
		SessionName: "session-a",
		AgentName:   "codex",
		ToolName:    "file_read",
		ToolCallID:  "call-1",
		Summary:     "reading",
		Content:     json.RawMessage(`{"path":"README.md"}`),
		ContentText: "plain",
		Truncation:  &events.ExecutionEventTruncation{SummaryTruncated: true, SummaryOriginalChars: 5000},
	}
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	for _, key := range []string{"type", "severity", "taskName", "sessionName", "agentName", "toolName", "toolCallID", "summary", "content", "contentText", "truncation"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("request JSON missing key %q in %s", key, data)
		}
	}
}

func TestExecutionEventDTOConversionExcludesInternalOnlyFields(t *testing.T) {
	storeEvent := store.ExecutionEvent{
		ID:         "event-1",
		Namespace:  "default",
		StreamType: events.ExecutionEventStreamTypeTask,
		StreamID:   "task-1",
		Seq:        1,
		Type:       events.ExecutionEventTypeTaskCreated,
		Severity:   "",
		TaskName:   "task-1",
		Summary:    "created",
		Content:    json.RawMessage(`{"ok":true}`),
		CreatedAt:  time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC),
		Internal:   map[string]any{"rawToken": "secret-internal-token"},
	}
	response := NewExecutionEventResponse(storeEvent)
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if strings.Contains(string(data), "Internal") || strings.Contains(string(data), "rawToken") || strings.Contains(string(data), "secret-internal-token") {
		t.Fatalf("NewExecutionEventResponse leaked internal fields: %s", data)
	}
	if response.Severity != events.ExecutionEventSeverityInfo {
		t.Fatalf("Severity = %q, want normalized info", response.Severity)
	}
}

func TestSubmitExecutionEventRequestJSONIgnoresUnknownFields(t *testing.T) {
	data := []byte(`{"type":"TaskCreated","severity":"warning","unknown":"ignored"}`)
	var request SubmitExecutionEventRequest
	if err := json.Unmarshal(data, &request); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if request.Type != events.ExecutionEventTypeTaskCreated || request.Severity != events.ExecutionEventSeverityWarning {
		t.Fatalf("request = %#v", request)
	}
}

func TestSubmitExecutionEventRequestToStoreEventSanitizesPayload(t *testing.T) {
	bearerValue := fakeDashToken("bearer")
	apiKey := fakeOpenAIKey()
	request := SubmitExecutionEventRequest{
		Type:        events.ExecutionEventTypeModelMessage,
		Severity:    "ERROR",
		TaskName:    "task-1",
		Summary:     "Authorization: Bearer " + bearerValue,
		Content:     mustRawJSON(t, map[string]any{"apiKey": apiKey, "safe": "ok"}),
		ContentText: strings.Repeat("x", events.MaxExecutionEventContentTextChars+10),
		Truncation:  &events.ExecutionEventTruncation{SummaryTruncated: true, SummaryOriginalChars: 9000},
	}
	storeEvent, err := request.ToStoreEvent("default", events.ExecutionEventStreamTypeTask, "task-1")
	if err != nil {
		t.Fatalf("ToStoreEvent() error = %v", err)
	}
	if storeEvent.Severity != events.ExecutionEventSeverityError {
		t.Fatalf("Severity = %q, want error", storeEvent.Severity)
	}
	if strings.Contains(storeEvent.Summary, bearerValue) || strings.Contains(string(storeEvent.Content), apiKey) {
		t.Fatalf("ToStoreEvent leaked secret fields: %#v content=%s", storeEvent, storeEvent.Content)
	}
	if storeEvent.Truncation == nil || !storeEvent.Truncation.ContentTextTruncated || !storeEvent.Truncation.SummaryTruncated {
		t.Fatalf("Truncation = %#v, want merged client and server truncation metadata", storeEvent.Truncation)
	}
	if storeEvent.Truncation.SummaryOriginalChars != 9000 {
		t.Fatalf("SummaryOriginalChars = %d, want client metadata preserved", storeEvent.Truncation.SummaryOriginalChars)
	}
}

func TestExecutionEventFixturesValidate(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "testdata", "execution-events", "*.json"))
	if err != nil {
		t.Fatalf("Glob fixtures: %v", err)
	}
	if len(paths) == 0 {
		t.Fatalf("no execution event fixtures found")
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile() error = %v", err)
			}
			assertFixtureContainsNoSecrets(t, string(raw))

			var response ListExecutionEventsResponse
			if err := json.Unmarshal(raw, &response); err != nil {
				t.Fatalf("Unmarshal fixture error = %v", err)
			}
			if response.StreamType != events.ExecutionEventStreamTypeTask {
				t.Fatalf("StreamType = %q, want task", response.StreamType)
			}
			if response.LatestSeq != int64(len(response.Events)) {
				t.Fatalf("LatestSeq = %d, events = %d", response.LatestSeq, len(response.Events))
			}
			var previousSeq int64
			for _, event := range response.Events {
				if event.Namespace != response.Namespace || event.StreamType != response.StreamType || event.StreamID != response.StreamID {
					t.Fatalf("event stream fields %#v do not match fixture header %#v", event, response)
				}
				if event.Seq <= previousSeq {
					t.Fatalf("seq %d is not greater than previous %d", event.Seq, previousSeq)
				}
				previousSeq = event.Seq
				if !events.IsValidExecutionEventType(event.Type) {
					t.Fatalf("fixture event has unsupported type %q", event.Type)
				}
				if !events.IsValidExecutionEventSeverity(event.Severity) {
					t.Fatalf("fixture event has unsupported severity %q", event.Severity)
				}
				if event.CreatedAt.IsZero() {
					t.Fatalf("fixture event missing createdAt: %#v", event)
				}
			}
		})
	}
}

func BenchmarkNewListExecutionEventsResponseTaskEvents(b *testing.B) {
	createdAt := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	storeEvents := make([]store.ExecutionEvent, 1024)
	for i := range storeEvents {
		storeEvents[i] = store.ExecutionEvent{
			ID:          "event-1",
			Namespace:   "default",
			StreamType:  events.ExecutionEventStreamTypeTask,
			StreamID:    "task-1",
			Seq:         int64(i + 1),
			Type:        events.ExecutionEventTypeTaskPhaseChanged,
			Severity:    events.ExecutionEventSeverityInfo,
			TaskName:    "task-1",
			SessionName: "session-a",
			AgentName:   "codex",
			Summary:     "phase changed",
			ContentText: "Running",
			CreatedAt:   createdAt,
		}
	}

	b.ReportAllocs()

	for b.Loop() {
		response := NewListExecutionEventsResponse("default", events.ExecutionEventStreamTypeTask, "task-1", 0, int64(len(storeEvents)), storeEvents)
		if len(response.Events) != len(storeEvents) {
			b.Fatalf("events = %d, want %d", len(response.Events), len(storeEvents))
		}
	}
}

func assertFixtureContainsNoSecrets(t *testing.T, raw string) {
	t.Helper()
	for _, marker := range []string{"Authorization:", "Bearer ", "Co" + "okie:", "Txn-Token", "Transaction-Token", "sk-test", "sk-ant", "github" + "_pat_", "gh" + "p_", "xox" + "b-"} {
		if strings.Contains(raw, marker) {
			t.Fatalf("fixture contains secret-like marker %q", marker)
		}
	}
}

func mustRawJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal raw JSON: %v", err)
	}
	return json.RawMessage(data)
}

func fakeDashToken(prefix string) string {
	return strings.Join([]string{prefix, "value", "for", "redaction"}, "-")
}

func fakeOpenAIKey() string {
	return strings.Join([]string{"sk", "test12345678901234567890"}, "-")
}
