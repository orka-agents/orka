package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHandleResponsesStreamsRequestedMarker(t *testing.T) {
	request := httptest.NewRequest(
		http.MethodPost,
		"/responses",
		strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"Reply exactly: ORKA_WS_SUBSTRATE_OK"}`),
	)
	response := httptest.NewRecorder()

	handleResponses(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if got := response.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	body := response.Body.String()
	cursor := 0
	for _, expected := range []string{
		"event: response.created",
		"event: response.output_item.added",
		"event: response.content_part.added",
		"event: response.output_text.delta",
		`"delta":"ORKA_WS_SUBSTRATE_OK"`,
		"event: response.output_text.done",
		"event: response.content_part.done",
		"event: response.output_item.done",
		`"text":"ORKA_WS_SUBSTRATE_OK"`,
		"event: response.completed",
	} {
		offset := strings.Index(body[cursor:], expected)
		if offset < 0 {
			t.Fatalf("stream omitted %q:\n%s", expected, body)
		}
		cursor += offset + len(expected)
	}
}

func TestResponseTextEchoesScenarioMarkers(t *testing.T) {
	for body, want := range map[string]string{
		`{"input":"Reply exactly: ORKA_WS_SUBSTRATE_OK"}`:      "ORKA_WS_SUBSTRATE_OK",
		`{"input":"Reply exactly: ORKA_WS_SUSPEND_FIRST_OK"}`:  "ORKA_WS_SUSPEND_FIRST_OK",
		`{"input":"Reply exactly: ORKA_WS_SUSPEND_SECOND_OK"}`: "ORKA_WS_SUSPEND_SECOND_OK",
		`{"input":"no marker here"}`:                           "ORKA_RESPONSES_FIXTURE_OK",
		`{"input":[{"role":"user","content":"Reply exactly: ORKA_WS_LC_FIRST_OK"},` +
			`{"role":"assistant","content":"ORKA_WS_LC_FIRST_OK"},` +
			`{"role":"user","content":"Reply exactly: ORKA_WS_LC_SECOND_OK"}]}`: "ORKA_WS_LC_SECOND_OK",
	} {
		if got := responseText([]byte(body)); got != want {
			t.Fatalf("responseText(%s) = %q, want %q", body, got, want)
		}
	}
}

func TestRequestHoldParsesBoundedHoldMarkers(t *testing.T) {
	for body, want := range map[string]time.Duration{
		`{"input":"ORKA_HOLD_15S then reply ORKA_WS_LC_CANCEL_OK"}`: 15 * time.Second,
		`{"input":"ORKA_HOLD_999S"}`:                                maxHoldSeconds * time.Second,
		`{"input":"no hold requested"}`:                             0,
	} {
		if got := requestHold([]byte(body)); got != want {
			t.Fatalf("requestHold(%s) = %v, want %v", body, got, want)
		}
	}
}

func TestMarkerCountsRecordEachResolvedRequest(t *testing.T) {
	marker := "ORKA_WS_COUNTED_ONCE_OK"
	request := httptest.NewRequest(
		http.MethodPost,
		"/responses",
		strings.NewReader(`{"model":"gpt-5.5","stream":false,"input":"Reply exactly: `+marker+`"}`),
	)
	handleResponses(httptest.NewRecorder(), request)

	counts := httptest.NewRecorder()
	handleMarkerCounts(counts, httptest.NewRequest(http.MethodGet, "/fixture/marker-counts", nil))
	if counts.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", counts.Code, http.StatusOK)
	}
	var decoded map[string]uint64
	if err := json.Unmarshal(counts.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode marker counts: %v", err)
	}
	if decoded[marker] != 1 {
		t.Fatalf("marker count = %d, want exactly 1", decoded[marker])
	}
}

// A resumed session may order replayed context after the active user prompt;
// structured extraction must still answer the newest user message's marker.
func TestResponseTextPrefersNewestUserMessage(t *testing.T) {
	body := `{"input":[` +
		`{"role":"user","content":"Reply exactly: ORKA_WS_SUSPEND_FIRST_OK"},` +
		`{"role":"assistant","content":"ORKA_WS_SUSPEND_FIRST_OK"},` +
		`{"role":"user","content":"Reply exactly: ORKA_WS_SUSPEND_SECOND_OK"},` +
		`{"role":"assistant","content":"replayed context mentioning ORKA_WS_SUSPEND_FIRST_OK"}]}`
	if got := responseText([]byte(body)); got != "ORKA_WS_SUSPEND_SECOND_OK" {
		t.Fatalf("responseText = %q, want the newest user message marker", got)
	}
}

func TestHandleResponsesRejectsMissingModel(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(`{"stream":true}`))
	response := httptest.NewRecorder()

	handleResponses(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusBadRequest)
	}
}
