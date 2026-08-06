package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	orkaclient "github.com/orka-agents/orka/internal/cli/client"
)

func TestMemoryCreateInitialFailurePrintsGeneratedIdempotencyKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var receivedKey string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		receivedKey = request.Header.Get("Idempotency-Key")
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(writer).Encode(map[string]string{"message": "temporarily unavailable"})
	}))
	defer server.Close()

	root := newRootCmd()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"memory", "create", "--server", server.URL, "--content", "remember this"})
	err := root.Execute()
	if err == nil {
		t.Fatal("Execute() error = nil")
	}
	if !strings.HasPrefix(receivedKey, "mcli-") {
		t.Fatalf("generated Idempotency-Key = %q", receivedKey)
	}
	if !strings.Contains(output.String(), receivedKey) || !strings.Contains(err.Error(), receivedKey) {
		t.Fatalf("generated key missing from output=%q error=%q", output.String(), err.Error())
	}
}

func TestPrintMemoryMutationOutcomeUsesMemorySubjectAfterWait(t *testing.T) {
	tests := []struct {
		name    string
		subject string
		body    map[string]any
		want    string
	}{
		{
			name: "memory id from completed operation",
			body: map[string]any{"id": "mop-1", "memoryId": "mem-1", "state": "succeeded"},
			want: "Memory created: mem-1",
		},
		{
			name:    "known update subject",
			subject: "mem-known",
			body:    map[string]any{"id": "mop-2", "state": "succeeded"},
			want:    "Memory updated: mem-known",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cmd := newRootCmd()
			var output bytes.Buffer
			cmd.SetOut(&output)
			action := strings.Split(test.want, ":")[0]
			response := &orkaclient.JSONResponse{StatusCode: http.StatusOK, Body: test.body}
			if err := printMemoryMutationOutcome(cmd, action, test.subject, response); err != nil {
				t.Fatal(err)
			}
			if got := strings.TrimSpace(output.String()); got != test.want || strings.Contains(got, "mop-") {
				t.Fatalf("output = %q, want %q", got, test.want)
			}
		})
	}
}

func TestMemoryOperationActionsSendOnlyServerOwnedProofRequest(t *testing.T) {
	for _, action := range []string{"retry", "abandon"} {
		t.Run(action, func(t *testing.T) {
			var body map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
				}
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(writer).Encode(map[string]any{"id": "mop-1", "state": "queued"})
			}))
			defer server.Close()

			root := newRootCmd()
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			root.SetArgs([]string{"memory", "operation", action, "mop-1", "--server", server.URL,
				"--reason", "operator reviewed", "--yes"})
			if err := root.Execute(); err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if len(body) != 1 || body["reason"] != "operator reviewed" {
				t.Fatalf("request body = %#v, want only reason", body)
			}
		})
	}
}
