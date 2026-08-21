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
	for _, expected := range []string{
		"event: response.created",
		"event: response.output_item.done",
		`"text":"ORKA_WS_SUBSTRATE_OK"`,
		"event: response.completed",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("stream omitted %q:\n%s", expected, body)
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
