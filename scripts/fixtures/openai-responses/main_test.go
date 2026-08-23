package main

import (
	"context"
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

func TestMarkerCountsRecordEachResolvedRequest(t *testing.T) {
	marker := "ORKA_WS_COUNTED_ONCE_OK"
	// The package-level counters survive across -count=N repetitions; reset
	// this test's marker state so repeated runs assert the same exact-one.
	markerCounts.Delete(markerKey(marker))
	markerHistory.Delete(markerKey(marker))
	markerDisconnects.Delete(markerKey(marker))
	t.Cleanup(func() {
		markerCounts.Delete(markerKey(marker))
		markerHistory.Delete(markerKey(marker))
		markerDisconnects.Delete(markerKey(marker))
	})
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
}

// A stale hold marker replayed in the session history must never delay a
// later prompt: hold resolution is scoped to the newest user message.
func TestRequestHoldIgnoresReplayedHistoryMarkers(t *testing.T) {
	replayed := `{"input":[` +
		`{"role":"user","content":"ORKA_HOLD_90S Reply exactly: ORKA_WS_LC_RESTART_OK"},` +
		`{"role":"assistant","content":"ORKA_WS_LC_RESTART_OK"},` +
		`{"role":"user","content":"Reply exactly: ORKA_WS_LC_REPLACED_OK"}]}`
	if got := requestHold([]byte(replayed)); got != 0 {
		t.Fatalf("requestHold(replayed history) = %v, want no hold", got)
	}
	current := `{"input":[` +
		`{"role":"user","content":"Reply exactly: ORKA_WS_LC_FIRST_OK"},` +
		`{"role":"assistant","content":"ORKA_WS_LC_FIRST_OK"},` +
		`{"role":"user","content":"ORKA_HOLD_15S Reply exactly: ORKA_WS_LC_CANCEL_OK"}]}`
	if got := requestHold([]byte(current)); got != 15*time.Second {
		t.Fatalf("requestHold(current turn) = %v, want 15s", got)
	}
}

// Continuations must be provably history-bearing, and a cancelled held
// request must observably close the provider stream.
func TestMarkerObservationsRecordHistoryAndDisconnects(t *testing.T) {
	marker := "ORKA_WS_OBSERVED_OK"
	markerCounts.Delete(markerKey(marker))
	markerHistory.Delete(markerKey(marker))
	markerDisconnects.Delete(markerKey(marker))
	t.Cleanup(func() {
		markerCounts.Delete(markerKey(marker))
		markerHistory.Delete(markerKey(marker))
		markerDisconnects.Delete(markerKey(marker))
	})

	fresh := httptest.NewRequest(http.MethodPost, "/responses",
		strings.NewReader(`{"model":"gpt-5.5","stream":false,`+
			`"input":[{"role":"user","content":"Reply exactly: `+marker+`"}]}`))
	handleResponses(httptest.NewRecorder(), fresh)
	continuation := httptest.NewRequest(http.MethodPost, "/responses",
		strings.NewReader(`{"model":"gpt-5.5","stream":false,"input":[`+
			`{"role":"user","content":"earlier turn"},`+
			`{"role":"assistant","content":"earlier answer"},`+
			`{"role":"user","content":"Reply exactly: `+marker+`"}]}`))
	handleResponses(httptest.NewRecorder(), continuation)

	// A recreated session concatenates the bootstrap transcript and the
	// active prompt into one user message; the replayed markers still count
	// as history.
	if !requestCarriesHistory([]byte(`{"input":[{"role":"user","content":` +
		`"history: Reply exactly: ORKA_WS_LC_FIRST_OK / ORKA_WS_LC_FIRST_OK -- now: Reply exactly: ` + marker + `"}]}`)) {
		t.Fatal("a concatenated bootstrap transcript must count as replayed history")
	}
	if requestCarriesHistory([]byte(`{"input":[{"role":"user","content":"Reply exactly: ` + marker + `"}]}`)) {
		t.Fatal("a bare fresh prompt must not count as replayed history")
	}

	// A held request whose client disconnects before the hold elapses is
	// recorded as a disconnect for its marker.
	ctx, cancel := context.WithCancel(context.Background())
	held := httptest.NewRequest(http.MethodPost, "/responses",
		strings.NewReader(`{"model":"gpt-5.5","stream":false,`+
			`"input":[{"role":"user","content":"ORKA_HOLD_60S Reply exactly: `+marker+`"}]}`)).
		WithContext(ctx)
	done := make(chan struct{})
	go func() {
		handleResponses(httptest.NewRecorder(), held)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("held request did not end after the client disconnect")
	}

	observations := httptest.NewRecorder()
	handleMarkerObservations(observations, httptest.NewRequest(http.MethodGet, "/fixture/marker-observations", nil))
	var decoded map[string]struct {
		SawHistory  bool   `json:"sawHistory"`
		Disconnects uint64 `json:"disconnects"`
	}
	if err := json.Unmarshal(observations.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode marker observations: %v", err)
	}
	if !decoded[markerKey(marker)].SawHistory {
		t.Fatal("continuation request with an assistant turn must record sawHistory")
	}
	if decoded[markerKey(marker)].Disconnects != 1 {
		t.Fatalf("disconnects = %d, want exactly the cancelled held request", decoded[markerKey(marker)].Disconnects)
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
