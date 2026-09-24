package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/publisher"
)

func TestPullRequestPresentationCapabilityIsOptInForLegacyClients(t *testing.T) {
	factory := PRReconcilerFactoryFunc(func(context.Context, string) (publisher.PullRequestReconciler, error) {
		return nil, errors.New("not used by capabilities")
	})
	fixture := newServiceFixtureWithOptions(t, factory, nil)
	// A legacy strict decoder cannot accept an additional JSON field.
	response, err := fixture.httpServer.Client().Get(fixture.httpServer.URL + CapabilitiesPath)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close() //nolint:errcheck
	var fields map[string]json.RawMessage
	if err := json.NewDecoder(response.Body).Decode(&fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["pullRequestPresentation"]; ok {
		t.Fatal("plain v1 capability responses expose a field unknown to older clients")
	}
	capabilities, err := fixture.client.Capabilities(t.Context())
	if err != nil || !capabilities.PullRequestPresentation {
		t.Fatalf("new client did not negotiate presentation support: %v", err)
	}
}

func TestPresentationCapabilityDiscoveryAcceptsOlderPublisher(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != CapabilitiesPath || r.URL.Query().Get("features") != PullRequestPresentationFeature {
			t.Error("capability discovery did not opt in to presentation")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"protocol": ProtocolVersion, "pullRequestReconciliation": true})
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{
		BaseURL: server.URL, HTTPClient: server.Client(), BearerToken: []byte(strings.Repeat("a", 32)), CapabilitySecret: []byte(strings.Repeat("b", 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err := client.Capabilities(t.Context())
	if err != nil || !capabilities.PullRequestReconciliation || capabilities.PullRequestPresentation {
		t.Fatalf("legacy publisher capability response was not preserved: %v", err)
	}
}

func TestPullRequestPresentationSurvivesMixedPublisherRollout(t *testing.T) {
	var attempts atomic.Int32
	var firstDigest string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != PullRequestReconcilePath {
			t.Error("unexpected operation during PR reconciliation")
		}
		w.Header().Set("Content-Type", "application/json")
		if attempts.Add(1) == 1 {
			firstDigest = r.Header.Get(OperationRequestDigestHeader)
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(ErrorResponse{Code: "invalid_request", Message: "pull request reconcile JSON is invalid"})
			return
		}
		if firstDigest == "" || r.Header.Get(OperationRequestDigestHeader) != firstDigest {
			t.Error("rolling-upgrade retry changed the immutable request digest")
		}
		_ = json.NewEncoder(w).Encode(PullRequestReconcileResponse{OperationID: "rolling-pr"})
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{
		BaseURL: server.URL, HTTPClient: server.Client(), BearerToken: []byte(strings.Repeat("a", 32)), CapabilitySecret: []byte(strings.Repeat("b", 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.ReconcilePullRequest(t.Context(), PullRequestReconcileRequest{
		Metadata: OperationMetadata{Namespace: "default", PublicationID: "publication", OperationID: "rolling-pr"},
		Intent:   publisher.PullRequestIntent{Title: "fix: safe rollout"},
	})
	if err != nil || response.OperationID != "rolling-pr" || attempts.Load() != 2 {
		t.Fatalf("mixed-version retry did not reconcile the original request: %v", err)
	}
}

func TestUnsupportedPresentationRetryHonorsDeadline(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(ErrorResponse{Code: "invalid_request", Message: "pull request reconcile JSON is invalid"})
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{
		BaseURL: server.URL, HTTPClient: server.Client(), BearerToken: []byte(strings.Repeat("a", 32)), CapabilitySecret: []byte(strings.Repeat("b", 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	request := PullRequestReconcileRequest{
		Metadata: OperationMetadata{Namespace: "default", PublicationID: "publication", OperationID: "rolling-pr"},
		Intent:   publisher.PullRequestIntent{Title: "fix: safe rollout"},
	}
	if _, err := client.ReconcilePullRequest(ctx, request); !errors.Is(err, context.DeadlineExceeded) || attempts.Load() != 1 {
		t.Fatalf("retry ignored the caller deadline: %v", err)
	}
	// A malformed legacy request is not mistaken for the new-field rollout gap.
	request.Intent.Title = ""
	if _, err := client.ReconcilePullRequest(t.Context(), request); err == nil || attempts.Load() != 2 {
		t.Fatal("legacy malformed request was retried")
	}
}
