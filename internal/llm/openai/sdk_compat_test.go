/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package openai

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

func TestResponseEventFunctionNameReadsRawEvent(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		wantName    string
		wantInvalid bool
	}{
		{name: "absent", raw: `{"type":"response.function_call_arguments.delta","item_id":"fc","delta":"{"}`},
		{name: "string", raw: `{"type":"response.function_call_arguments.done","item_id":"fc","name":"lookup","arguments":"{}"}`, wantName: "lookup"},
		{name: "empty", raw: `{"type":"response.function_call_arguments.done","item_id":"fc","name":"","arguments":"{}"}`, wantInvalid: true},
		{name: "null", raw: `{"type":"response.function_call_arguments.done","item_id":"fc","name":null,"arguments":"{}"}`, wantInvalid: true},
		{name: "number", raw: `{"type":"response.function_call_arguments.done","item_id":"fc","name":7,"arguments":"{}"}`, wantInvalid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var evt responses.ResponseStreamEventUnion
			if err := json.Unmarshal([]byte(tt.raw), &evt); err != nil {
				t.Fatalf("unmarshal event: %v", err)
			}
			name, invalid := responseEventFunctionName(evt)
			if name != tt.wantName || invalid != tt.wantInvalid {
				t.Fatalf("responseEventFunctionName() = (%q, %v), want (%q, %v)", name, invalid, tt.wantName, tt.wantInvalid)
			}
			if err := validateResponseFunctionEventMetadata(evt); (err != nil) != tt.wantInvalid {
				t.Fatalf("validateResponseFunctionEventMetadata() error = %v, want invalid %v", err, tt.wantInvalid)
			}
		})
	}
}

func TestOpenAIErrorMessageKeepsBodyAndRedactsURL(t *testing.T) {
	target, err := url.Parse("https://user:secret@gateway.example/v1/responses?api-key=secret#frag")
	if err != nil {
		t.Fatal(err)
	}
	apiErr := &openai.Error{StatusCode: http.StatusBadRequest, Request: &http.Request{Method: http.MethodPost, URL: target}}
	if err := apiErr.UnmarshalJSON([]byte(`{"code":"context_length_exceeded","message":"too long"}`)); err != nil {
		t.Fatal(err)
	}

	msg := toProviderError(apiErr).Message
	for _, want := range []string{`POST "https://gateway.example/v1/responses"`, "400 Bad Request", "context_length_exceeded", "too long"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not contain %q", msg, want)
		}
	}
	if strings.Contains(msg, "secret") || strings.Contains(msg, "frag") {
		t.Errorf("message %q leaks URL userinfo, query, or fragment", msg)
	}
}
