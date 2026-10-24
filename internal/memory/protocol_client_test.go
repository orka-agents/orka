package memory

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/endpointpolicy"
	memoryv1 "github.com/orka-agents/orka/pkg/memory/v1"
)

type adapterCountingResolver struct {
	calls int
}

func (r *adapterCountingResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	r.calls++
	return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
}

type adapterRecordingDialer struct {
	addresses []string
}

func (d *adapterRecordingDialer) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	d.addresses = append(d.addresses, address)
	left, right := net.Pipe()
	_ = right.Close()
	return left, nil
}

func TestProtocolClientMutationUsesBearerAndStrictReceipt(t *testing.T) {
	binding := testProtocolBinding()
	envelope := memoryv1.MutationEnvelope{
		ProtocolVersion: memoryv1.Version,
		OperationID:     "mop-1",
		Binding:         binding,
		MemoryID:        "mem-1",
		Kind:            memoryv1.MutationKindCreate,
		Generation:      1,
		State: &memoryv1.MutationState{
			Content: "durable guidance",
			Tags:    []string{"storage"},
			Metadata: map[string]string{
				"source": "test",
			},
		},
	}
	if err := memoryv1.PrepareMutation(&envelope); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != memoryv1.PathMutations || r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var got memoryv1.MutationEnvelope
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(memoryv1.MutationReceipt{
			ProtocolVersion:   memoryv1.Version,
			Binding:           got.Binding,
			Result:            memoryv1.ResultApplied,
			OperationID:       got.OperationID,
			BindingDigest:     memoryv1.BindingDigest(got.Binding),
			AppliedGeneration: got.Generation,
			BackendVersion:    "v1",
			BackendMemoryID:   "backend-1",
			ContentDigest:     got.ContentDigest,
			MutationDigest:    got.MutationDigest,
			CompletedAt:       testProtocolTime(),
		})
	}))
	defer server.Close()

	client := &ProtocolClient{baseURL: server.URL, token: "secret", client: server.Client()}
	receipt, err := client.Mutate(context.Background(), envelope)
	if err != nil {
		t.Fatalf("Mutate() error = %v", err)
	}
	if receipt.Result != memoryv1.ResultApplied || receipt.OperationID != envelope.OperationID {
		t.Fatalf("receipt = %#v", receipt)
	}
}

func TestProtocolClientDoesNotExposeRawInvalidErrorBody(t *testing.T) {
	binding := testProtocolBinding()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`provider secret body should not escape`))
	}))
	defer server.Close()

	client := &ProtocolClient{baseURL: server.URL, token: "secret", client: server.Client()}
	_, err := client.Search(context.Background(), memoryv1.SearchRequest{ProtocolVersion: memoryv1.Version, Binding: binding, Mode: memoryv1.SearchModeKeyword, PageSize: 1})
	if err == nil {
		t.Fatal("Search() error = nil")
	}
	if strings.Contains(err.Error(), "provider secret") {
		t.Fatalf("error leaked provider body: %v", err)
	}
	adapterErr, ok := err.(*AdapterError)
	if !ok || adapterErr.Code != memoryv1.ErrorCodeInternal || !adapterErr.Retryable {
		t.Fatalf("error = %#v", err)
	}
}

func TestProtocolClientDoesNotRetainProviderErrorMessage(t *testing.T) {
	binding := testProtocolBinding()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(memoryv1.ErrorResponse{
			ProtocolVersion: memoryv1.Version,
			Binding:         &binding,
			Code:            memoryv1.ErrorCodeInternal,
			Message:         "provider tenant secret must never be stored",
			Retryable:       true,
		})
	}))
	defer server.Close()

	client := &ProtocolClient{baseURL: server.URL, token: "secret", client: server.Client()}
	_, err := client.Search(context.Background(), memoryv1.SearchRequest{ProtocolVersion: memoryv1.Version, Binding: binding, Mode: memoryv1.SearchModeKeyword, PageSize: 1})
	adapterErr, ok := err.(*AdapterError)
	if !ok {
		t.Fatalf("error = %#v", err)
	}
	if strings.Contains(err.Error(), "provider tenant") || strings.Contains(adapterErr.Message, "provider tenant") {
		t.Fatalf("provider message escaped: %#v", adapterErr)
	}
	if adapterErr.Message != "memory adapter reported an internal error" || !adapterErr.Retryable {
		t.Fatalf("adapter error = %#v", adapterErr)
	}
}

func TestNewProtocolClientUsesExactPinnedResolutionWithoutDNSReplay(t *testing.T) {
	resolver := &adapterCountingResolver{}
	dialer := &adapterRecordingDialer{}
	policy := endpointpolicy.PublicHTTPSPolicy{Resolver: resolver, Dialer: dialer}
	resolution, err := policy.Resolve(context.Background(), "https://memory.example.com")
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewProtocolClient(
		policy,
		nil,
		resolution,
		"sha256:"+strings.Repeat("0", 64),
		"secret",
		memoryv1.MaxHTTPBodyBytes,
		memoryv1.MaxAdapterResponseBytes,
		time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}
	transport := client.client.Transport.(*http.Transport)
	if transport.MaxConnsPerHost != adapterHTTPMaxConnsPerHost {
		t.Fatalf("MaxConnsPerHost = %d, want %d", transport.MaxConnsPerHost, adapterHTTPMaxConnsPerHost)
	}
	connection, err := transport.DialContext(context.Background(), "tcp", "memory.example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
	if resolver.calls != 1 {
		t.Fatalf("resolver calls = %d, want only the original validation", resolver.calls)
	}
	if len(dialer.addresses) != 1 || dialer.addresses[0] != "8.8.8.8:443" {
		t.Fatalf("dialed addresses = %v", dialer.addresses)
	}
}

func testProtocolBinding() memoryv1.Binding {
	binding := memoryv1.Binding{
		ClusterID: "cluster-a", NamespaceUID: "namespace-a", BackendUID: "00000000-0000-4000-8000-000000000002",
		AuthorityEpoch: 1, RoutingEpoch: 1, StoreUUID: "00000000-0000-4000-8000-000000000003",
	}
	binding.TenantID = memoryv1.DeriveTenantID(binding.ClusterID, binding.NamespaceUID)
	return binding
}

func testProtocolTime() time.Time { return time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC) }

func testProtocolCapabilitiesResponse(binding memoryv1.Binding, maxResponseBytes int) memoryv1.CapabilitiesResponse {
	return memoryv1.CapabilitiesResponse{
		ProtocolVersion: memoryv1.Version,
		Binding:         binding,
		AdapterName:     "test-adapter",
		AdapterVersion:  "v1",
		Revision:        "revision-1",
		ExpiresAt:       time.Now().Add(time.Hour),
		Capabilities: memoryv1.Capabilities{
			DurableIdempotency:         true,
			IdempotencyDigestConflicts: true,
			CreateIfAbsent:             true,
			ConditionalMutation:        true,
			MonotonicGenerations:       true,
			DeleteHighWatermark:        true,
			DurableRoutingFence:        true,
			OperationLookup:            true,
			ExactGet:                   true,
			StablePagination:           true,
			ExclusiveOwnership:         true,
			KeywordSearch:              true,
			AuditVersionVisibility:     true,
		},
		Limits: memoryv1.CapabilityLimits{
			MaxRequestBytes:       memoryv1.MaxHTTPBodyBytes,
			MaxResponseBytes:      maxResponseBytes,
			MaxContentBytes:       memoryv1.MaxContentBytes,
			MaxTags:               memoryv1.MaxTags,
			MaxTagBytes:           memoryv1.MaxTagBytes,
			MaxMetadataEntries:    memoryv1.MaxMetadataEntries,
			MaxMetadataKeyBytes:   memoryv1.MaxMetadataKeyBytes,
			MaxMetadataValueBytes: memoryv1.MaxMetadataValueBytes,
			MaxQueryBytes:         memoryv1.MaxQueryBytes,
			MaxPageSize:           memoryv1.MaxPageSize,
			MaxSnapshotRecords:    memoryv1.MaxSnapshotRecords,
			SnapshotTTLSeconds:    60,
		},
	}
}

func TestProtocolClientEnforcesConfiguredMaxRequestBytesBeforeEgress(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	payload := struct {
		Value string `json:"value"`
	}{Value: strings.Repeat("x", 64)}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	client := &ProtocolClient{
		baseURL: server.URL, token: "secret", client: server.Client(),
		maxRequestBytes: int64(len(body) - 1), maxResponseBytes: memoryv1.MaxAdapterResponseBytes,
	}
	if _, err := client.post(t.Context(), "/test", payload, nil, nil, false); err == nil {
		t.Fatal("post() accepted a request above the advertised limit")
	}
	if requests.Load() != 0 {
		t.Fatalf("adapter requests = %d, want zero before oversized request egress", requests.Load())
	}
	client.maxRequestBytes = int64(memoryv1.MaxHTTPBodyBytes) + 1
	if got := effectiveProtocolRequestLimit(client.maxRequestBytes); got != memoryv1.MaxHTTPBodyBytes {
		t.Fatalf("effective request limit = %d, want hard limit %d", got, memoryv1.MaxHTTPBodyBytes)
	}
}

func TestProtocolClientEnforcesConfiguredMaxResponseBytesForEveryStatus(t *testing.T) {
	const advertisedLimit = 32
	responseBody := append([]byte(`{"ok":true}`), bytes.Repeat([]byte(" "), advertisedLimit)...)

	for _, status := range []int{http.StatusOK, http.StatusConflict, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write(responseBody)
			}))
			defer server.Close()

			client := &ProtocolClient{
				baseURL: server.URL, token: "secret", client: server.Client(), maxResponseBytes: advertisedLimit,
			}
			_, err := client.post(context.Background(), "/test", struct{}{}, nil, nil, false)
			var adapterErr *AdapterError
			if !errors.As(err, &adapterErr) || adapterErr.Code != memoryv1.ErrorCodeResponseTooLarge {
				t.Fatalf("post() error = %#v, want response-too-large AdapterError", err)
			}
		})
	}
}

func TestProtocolClientResponseLimitAcceptsExactBoundAndKeepsHardMaximum(t *testing.T) {
	exactBody := []byte(`{"ok":true}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(exactBody)
	}))
	defer server.Close()

	client := &ProtocolClient{
		baseURL: server.URL, token: "secret", client: server.Client(), maxResponseBytes: int64(len(exactBody)),
	}
	if _, err := client.post(context.Background(), "/test", struct{}{}, nil, nil, false); err != nil {
		t.Fatalf("post() rejected response at advertised bound: %v", err)
	}

	client.maxResponseBytes = int64(memoryv1.MaxAdapterResponseBytes) + 1
	if got := effectiveProtocolResponseLimit(client.maxResponseBytes); got != memoryv1.MaxAdapterResponseBytes {
		t.Fatalf("effective response limit = %d, want hard limit %d", got, memoryv1.MaxAdapterResponseBytes)
	}

	hardLimitBody := bytes.Repeat([]byte(" "), memoryv1.MaxAdapterResponseBytes+1)
	hardLimitServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(hardLimitBody)
	}))
	defer hardLimitServer.Close()
	client.baseURL = hardLimitServer.URL
	client.client = hardLimitServer.Client()
	_, err := client.post(context.Background(), "/test", struct{}{}, nil, nil, false)
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Code != memoryv1.ErrorCodeResponseTooLarge {
		t.Fatalf("post() error = %#v, want hard response limit enforcement", err)
	}
}

func TestProtocolClientCapabilitiesRetroactivelyEnforcesAdvertisedMaxResponseBytes(t *testing.T) {
	binding := testProtocolBinding()
	capabilities := testProtocolCapabilitiesResponse(binding, 1)
	responseBody, err := json.Marshal(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(responseBody)
	}))
	defer server.Close()

	client := &ProtocolClient{
		baseURL: server.URL, token: "secret", client: server.Client(), maxResponseBytes: memoryv1.MaxAdapterResponseBytes,
	}
	_, err = client.Capabilities(context.Background(), memoryv1.CapabilitiesRequest{
		ProtocolVersion: memoryv1.Version,
		Binding:         binding,
	})
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Code != memoryv1.ErrorCodeResponseTooLarge {
		t.Fatalf("Capabilities() error = %#v, want retroactive response-too-large rejection", err)
	}
}

func TestProtocolClientDecodesTypedSemanticConflictBodies(t *testing.T) {
	binding := testProtocolBinding()
	now := testProtocolTime()
	mutation := memoryv1.MutationEnvelope{
		ProtocolVersion: memoryv1.Version,
		OperationID:     "mop-conflict",
		Binding:         binding,
		MemoryID:        "mem-conflict",
		Kind:            memoryv1.MutationKindCreate,
		Generation:      1,
		State: &memoryv1.MutationState{
			Content:  "conflicting guidance",
			Tags:     []string{},
			Metadata: map[string]string{},
		},
	}
	if err := memoryv1.PrepareMutation(&mutation); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		path   string
		body   any
		invoke func(*ProtocolClient) (string, error)
		want   string
	}{
		{
			name: "ownership identity conflict",
			path: memoryv1.PathOwnershipClaim,
			body: memoryv1.OwnershipClaimResponse{
				ProtocolVersion: memoryv1.Version, Binding: binding,
				Result: memoryv1.ResultIdentityConflict, BindingDigest: memoryv1.BindingDigest(binding),
				ClaimIdentity: memoryv1.AuthorityDigest(binding), MaximumRoutingEpoch: binding.RoutingEpoch,
				ClaimedAt: now,
			},
			invoke: func(client *ProtocolClient) (string, error) {
				response, err := client.ClaimOwnership(context.Background(), memoryv1.OwnershipClaimRequest{
					ProtocolVersion: memoryv1.Version, Binding: binding,
				})
				if response == nil {
					return "", err
				}
				return response.Result, err
			},
			want: memoryv1.ResultIdentityConflict,
		},
		{
			name: "routing fence precondition",
			path: memoryv1.PathRoutingFence,
			body: memoryv1.RoutingFenceResponse{
				ProtocolVersion: memoryv1.Version, Binding: binding,
				Result: memoryv1.ResultPreconditionFailed, BindingDigest: memoryv1.BindingDigest(binding),
				MaximumRoutingEpoch: binding.RoutingEpoch + 1, CompletedAt: now,
			},
			invoke: func(client *ProtocolClient) (string, error) {
				response, err := client.AdvanceRoutingFence(context.Background(), memoryv1.RoutingFenceRequest{
					ProtocolVersion: memoryv1.Version, Binding: binding,
				})
				if response == nil {
					return "", err
				}
				return response.Result, err
			},
			want: memoryv1.ResultPreconditionFailed,
		},
		{
			name: "mutation idempotency conflict",
			path: memoryv1.PathMutations,
			body: memoryv1.MutationReceipt{
				ProtocolVersion: memoryv1.Version, Binding: binding,
				Result: memoryv1.ResultIdempotencyConflict, OperationID: mutation.OperationID,
				BindingDigest: memoryv1.BindingDigest(binding), ContentDigest: mutation.ContentDigest,
				MutationDigest: mutation.MutationDigest, CompletedAt: now,
			},
			invoke: func(client *ProtocolClient) (string, error) {
				response, err := client.Mutate(context.Background(), mutation)
				if response == nil {
					return "", err
				}
				return response.Result, err
			},
			want: memoryv1.ResultIdempotencyConflict,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path != test.path {
					t.Fatalf("request path = %q, want %q", request.URL.Path, test.path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				if err := json.NewEncoder(w).Encode(test.body); err != nil {
					t.Fatal(err)
				}
			}))
			defer server.Close()

			client := &ProtocolClient{baseURL: server.URL, token: "secret", client: server.Client()}
			result, err := test.invoke(client)
			if err != nil {
				t.Fatalf("typed conflict returned error: %v", err)
			}
			if result != test.want {
				t.Fatalf("result = %q, want %q", result, test.want)
			}
		})
	}
}

func TestProtocolClientFallsBackToSanitizedGenericConflictError(t *testing.T) {
	binding := testProtocolBinding()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(memoryv1.ErrorResponse{
			ProtocolVersion: memoryv1.Version,
			Binding:         &binding,
			Code:            memoryv1.ErrorCodeIdentityConflict,
			Message:         "provider tenant secret must never escape",
		})
	}))
	defer server.Close()

	client := &ProtocolClient{baseURL: server.URL, token: "secret", client: server.Client()}
	_, err := client.ClaimOwnership(context.Background(), memoryv1.OwnershipClaimRequest{
		ProtocolVersion: memoryv1.Version, Binding: binding,
	})
	adapterErr, ok := err.(*AdapterError)
	if !ok {
		t.Fatalf("error = %#v, want *AdapterError", err)
	}
	if adapterErr.Code != memoryv1.ErrorCodeIdentityConflict ||
		strings.Contains(adapterErr.Message, "provider tenant") || strings.Contains(err.Error(), "provider tenant") {
		t.Fatalf("generic conflict was not safely normalized: %#v", adapterErr)
	}
}

func TestProtocolClientTreatsMalformedTransientResponsesAsRetryableAmbiguous(t *testing.T) {
	for _, status := range []int{
		http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"malformed":`))
			}))
			defer server.Close()

			client := &ProtocolClient{baseURL: server.URL, token: "secret", client: server.Client()}
			_, err := client.Search(context.Background(), memoryv1.SearchRequest{})
			adapterErr, ok := err.(*AdapterError)
			if !ok || !adapterErr.Retryable || adapterErr.RetryAfterSeconds != 7 {
				t.Fatalf("error = %#v, want retryable ambiguous response", err)
			}
		})
	}
}

func TestProtocolClientRequiresParsedApplicationJSONForEveryStatus(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		contentType string
		semantic    bool
		wantErr     bool
	}{
		{name: "success with parameters", status: http.StatusOK, contentType: "application/json; charset=utf-8"},
		{name: "conflict with parameters", status: http.StatusConflict, contentType: "application/json; charset=utf-8", semantic: true},
		{name: "success json sequence rejected", status: http.StatusOK, contentType: "application/json-seq", wantErr: true},
		{name: "conflict malformed media type rejected", status: http.StatusConflict, contentType: "application/json; charset", semantic: true, wantErr: true},
		{name: "error missing content type rejected", status: http.StatusServiceUnavailable, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.contentType != "" {
					w.Header().Set("Content-Type", test.contentType)
				}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()
			client := &ProtocolClient{baseURL: server.URL, token: "secret", client: server.Client()}
			var semantic func([]byte) bool
			if test.semantic {
				semantic = func([]byte) bool { return true }
			}
			_, err := client.post(context.Background(), "/test", map[string]string{"ok": "true"}, semantic, nil, false)
			if (err != nil) != test.wantErr {
				t.Fatalf("post() error = %v, wantErr=%v", err, test.wantErr)
			}
		})
	}
}

func TestProtocolClientTreatsMalformedSuccessfulMutationAsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("committed but malformed"))
	}))
	defer server.Close()
	client := &ProtocolClient{baseURL: server.URL, token: "secret", client: server.Client()}
	_, err := client.Mutate(context.Background(), memoryv1.MutationEnvelope{})
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) || !adapterErr.Retryable {
		t.Fatalf("Mutate() error = %#v, want retryable malformed acknowledgement", err)
	}
}

func TestProtocolClientTreatsMalformedMutationAcknowledgementAsAmbiguousAtEveryStatus(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusConflict, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"malformed":`))
			}))
			defer server.Close()
			client := &ProtocolClient{baseURL: server.URL, token: "secret", client: server.Client()}
			_, err := client.Mutate(context.Background(), memoryv1.MutationEnvelope{})
			var adapterErr *AdapterError
			if !errors.As(err, &adapterErr) || !adapterErr.Retryable {
				t.Fatalf("Mutate() error = %#v, want ambiguous retryable acknowledgement", err)
			}
		})
	}
}

func TestProtocolClientAcceptsPreAuthenticationUnauthorizedWithoutBinding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(memoryv1.ErrorResponse{
			ProtocolVersion: memoryv1.Version,
			Binding:         nil,
			Code:            memoryv1.ErrorCodeUnauthorized,
			Message:         "unauthorized",
			Retryable:       false,
		})
	}))
	defer server.Close()

	client := &ProtocolClient{baseURL: server.URL, token: "secret", client: server.Client()}
	_, err := client.Mutate(context.Background(), memoryv1.MutationEnvelope{Binding: testProtocolBinding()})
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) {
		t.Fatalf("Mutate() error = %#v, want *AdapterError", err)
	}
	if adapterErr.StatusCode != http.StatusUnauthorized ||
		adapterErr.Code != memoryv1.ErrorCodeUnauthorized || adapterErr.Retryable {
		t.Fatalf("Mutate() error = %#v, want definitive pre-authentication unauthorized error", adapterErr)
	}
}

func TestProtocolClientRejectsOtherNilErrorBindings(t *testing.T) {
	tests := []struct {
		name   string
		status int
		code   string
	}{
		{name: "wrong status", status: http.StatusForbidden, code: memoryv1.ErrorCodeUnauthorized},
		{name: "wrong code", status: http.StatusUnauthorized, code: memoryv1.ErrorCodeInvalidRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_ = json.NewEncoder(w).Encode(memoryv1.ErrorResponse{
					ProtocolVersion: memoryv1.Version,
					Binding:         nil,
					Code:            test.code,
					Message:         "rejected",
					Retryable:       false,
				})
			}))
			defer server.Close()

			client := &ProtocolClient{baseURL: server.URL, token: "secret", client: server.Client()}
			_, err := client.Mutate(context.Background(), memoryv1.MutationEnvelope{Binding: testProtocolBinding()})
			var adapterErr *AdapterError
			if !errors.As(err, &adapterErr) || adapterErr.Code != memoryv1.ErrorCodeIdentityConflict || !adapterErr.Retryable {
				t.Fatalf("Mutate() error = %#v, want ambiguous binding mismatch", err)
			}
		})
	}
}

func TestProtocolClientRejectsMismatchedErrorBinding(t *testing.T) {
	requestBinding := testProtocolBinding()
	otherBinding := requestBinding
	otherBinding.RoutingEpoch++
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(memoryv1.ErrorResponse{
			ProtocolVersion: memoryv1.Version, Binding: &otherBinding, Code: memoryv1.ErrorCodeInvalidRequest,
			Message: "invalid", Retryable: false,
		})
	}))
	defer server.Close()
	client := &ProtocolClient{baseURL: server.URL, token: "secret", client: server.Client()}
	_, err := client.Mutate(context.Background(), memoryv1.MutationEnvelope{Binding: requestBinding})
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) || !adapterErr.Retryable {
		t.Fatalf("Mutate() error = %#v, want ambiguous binding mismatch", err)
	}
}

func TestNewProtocolClientPinsValidatedServerCertificateDigest(t *testing.T) {
	resolver := &adapterCountingResolver{}
	dialer := &adapterRecordingDialer{}
	policy := endpointpolicy.PublicHTTPSPolicy{Resolver: resolver, Dialer: dialer}
	resolution, err := policy.Resolve(context.Background(), "https://memory.example.com")
	if err != nil {
		t.Fatal(err)
	}
	state := tls.ConnectionState{
		HandshakeComplete: true,
		PeerCertificates:  []*x509.Certificate{{Raw: []byte("validated certificate")}},
	}
	digest, err := endpointpolicy.CertificateDigest(&state)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewProtocolClient(
		policy,
		nil,
		resolution,
		digest,
		"secret",
		memoryv1.MaxHTTPBodyBytes,
		memoryv1.MaxAdapterResponseBytes,
		time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}
	verify := client.client.Transport.(*http.Transport).TLSClientConfig.VerifyConnection
	if err := verify(state); err != nil {
		t.Fatalf("matching certificate rejected: %v", err)
	}
	state.PeerCertificates = []*x509.Certificate{{Raw: []byte("rotated certificate")}}
	if err := verify(state); err == nil {
		t.Fatal("rotated certificate unexpectedly matched the durable digest")
	}
}
