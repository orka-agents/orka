package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const (
	fixtureTestFirstMarker  = "ORKA_WS_SUSPEND_FIRST_OK"
	fixtureTestSecondMarker = "ORKA_WS_SUSPEND_SECOND_OK"
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
		`{"input":"Reply exactly: ORKA_WS_SUSPEND_FIRST_OK"}`:  fixtureTestFirstMarker,
		`{"input":"Reply exactly: ORKA_WS_SUSPEND_SECOND_OK"}`: fixtureTestSecondMarker,
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

func TestRequestHoldUsesActiveConcatenatedPrompt(t *testing.T) {
	historyOnly := `{"input":[{"role":"user","content":"history: ORKA_HOLD_15S then reply ` +
		`ORKA_WS_SUSPEND_FIRST_OK / ORKA_WS_SUSPEND_FIRST_OK -- now: Reply exactly: ` +
		`ORKA_WS_SUSPEND_SECOND_OK"}]}`
	if got := requestHold([]byte(historyOnly)); got != 0 {
		t.Fatalf("requestHold(historyOnly) = %v, want no replayed hold", got)
	}

	activeHold := `{"input":[{"role":"user","content":"history: ORKA_HOLD_15S then reply ` +
		`ORKA_WS_SUSPEND_FIRST_OK / ORKA_WS_SUSPEND_FIRST_OK -- now: ORKA_HOLD_7S then reply ` +
		`ORKA_WS_SUSPEND_SECOND_OK"}]}`
	if got := requestHold([]byte(activeHold)); got != 7*time.Second {
		t.Fatalf("requestHold(activeHold) = %v, want 7s", got)
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
	if decoded[markerKey(marker)] != 1 {
		t.Fatalf("marker count = %d, want exactly 1", decoded[markerKey(marker)])
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
	if got := responseText([]byte(body)); got != fixtureTestSecondMarker {
		t.Fatalf("responseText = %q, want the newest user message marker", got)
	}

	// A resumed session can concatenate the bootstrap transcript and the
	// active prompt into one user message; the active prompt is last.
	concatenated := `{"input":[` +
		`{"role":"developer","content":"agent configuration"},` +
		`{"role":"user","content":"history: Reply exactly: ORKA_WS_SUSPEND_FIRST_OK / ` +
		`ORKA_WS_SUSPEND_FIRST_OK -- now: Reply exactly: ORKA_WS_SUSPEND_SECOND_OK"}]}`
	if got := responseText([]byte(concatenated)); got != fixtureTestSecondMarker {
		t.Fatalf("responseText(concatenated) = %q, want the active prompt marker", got)
	}

	withoutMarker := `{"input":[` +
		`{"role":"user","content":"Reply exactly: ORKA_WS_SUSPEND_FIRST_OK"},` +
		`{"role":"assistant","content":"ORKA_WS_SUSPEND_FIRST_OK"},` +
		`{"role":"user","content":"continue without a marker"}]}`
	if got := responseText([]byte(withoutMarker)); got != "ORKA_RESPONSES_FIXTURE_OK" {
		t.Fatalf("responseText(withoutMarker) = %q, want the generic fixture response", got)
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
