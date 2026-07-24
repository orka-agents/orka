package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGatewayCommandHierarchy(t *testing.T) {
	cmd := newGatewayCmd()
	paths := [][]string{
		{"list"}, {"get"}, {"class", "list"}, {"binding", "get"},
		{"events", "list"}, {"deliveries", "retry"},
	}
	for _, path := range paths {
		current := cmd
		for _, name := range path {
			found, _, err := current.Find([]string{name})
			if err != nil || found == nil || found.Name() != name {
				t.Fatalf("gateway command path %v missing: %v", path, err)
			}
			current = found
		}
	}
}

func TestGatewayEventsListBuildsFilters(t *testing.T) {
	var requestPath string
	var gotState, gotGateway, gotNamespace string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath = r.URL.String()
		gotState = r.URL.Query().Get("state")
		gotGateway = r.URL.Query().Get("gateway")
		gotNamespace = r.URL.Query().Get("namespace")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"metadata":{}}`))
	}))
	defer server.Close()

	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetArgs([]string{
		"--server", server.URL, "--namespace", "ns", "gateway", "events", "list",
		"--state", "DeadLettered", "--gateway", "chat", "-o", "json",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(requestPath, "/api/v1/gateway-events?") {
		t.Fatalf("request path = %q", requestPath)
	}
	if gotState != "DeadLettered" || gotGateway != "chat" || gotNamespace != "ns" {
		t.Fatalf(
			"filters = state:%q gateway:%q namespace:%q, want DeadLettered/chat/ns",
			gotState, gotGateway, gotNamespace,
		)
	}
}

func TestGatewayEventsListHumanOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"items":[{
				"id":"event-1",
				"namespace":"team-a",
				"state":"DeadLettered",
				"createdAt":"2026-07-18T00:00:00Z"
			}],
			"metadata":{}
		}`))
	}))
	defer server.Close()

	var output bytes.Buffer
	root := newRootCmd()
	root.SetOut(&output)
	root.SetArgs([]string{
		"--server", server.URL, "--namespace", "team-a", "gateway", "events", "list",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	for _, expected := range []string{"NAME", "NAMESPACE", "STATUS", "event-1", "team-a", "DeadLettered"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("human output = %q, want %q", output.String(), expected)
		}
	}
}

func TestGatewayDeliveryRetryForwardsNamespace(t *testing.T) {
	var gotNamespace string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotNamespace = r.URL.Query().Get("namespace")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"delivery-1","state":"Pending"}`))
	}))
	defer server.Close()

	var output bytes.Buffer
	root := newRootCmd()
	root.SetOut(&output)
	root.SetArgs([]string{
		"--server", server.URL, "--namespace", "team-a",
		"gateway", "deliveries", "retry", "delivery-1", "-o", "json",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if gotNamespace != "team-a" {
		t.Fatalf("namespace query = %q, want team-a", gotNamespace)
	}
	var result map[string]any
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("JSON output = %q: %v", output.String(), err)
	}
	if result["id"] != "delivery-1" || result["state"] != "Pending" {
		t.Fatalf("JSON output = %#v, want delivery-1 Pending", result)
	}
}
