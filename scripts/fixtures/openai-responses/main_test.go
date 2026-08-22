package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	} {
		if got := responseText([]byte(body)); got != want {
			t.Fatalf("responseText(%s) = %q, want %q", body, got, want)
		}
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
