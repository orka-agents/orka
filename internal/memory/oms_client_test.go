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
	"github.com/orka-agents/orka/pkg/oms/protocol"
)

type omsCountingResolver struct {
	calls int
}

func (r *omsCountingResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	r.calls++
	return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
}

type omsRecordingDialer struct {
	addresses []string
}

func (d *omsRecordingDialer) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	d.addresses = append(d.addresses, address)
	left, right := net.Pipe()
	_ = right.Close()
	return left, nil
}

func TestOMSClientMutationUsesBearerAndStrictReceipt(t *testing.T) {
	binding := testOMSBinding()
	envelope := protocol.MutationEnvelope{
		ProtocolVersion: protocol.Version,
		OperationID:     "mop-1",
		Binding:         binding,
		MemoryID:        "mem-1",
		Kind:            protocol.MutationKindCreate,
		Generation:      1,
		State: &protocol.MutationState{
			Content: "durable guidance",
			Tags:    []string{"storage"},
			Metadata: map[string]string{
				"source": "test",
			},
		},
	}
	if err := protocol.PrepareMutation(&envelope); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != protocol.PathMutations || r.Header.Get("Authorization") != "Bearer secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var got protocol.MutationEnvelope
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protocol.MutationReceipt{
			ProtocolVersion:   protocol.Version,
			Binding:           got.Binding,
			Result:            protocol.ResultApplied,
			OperationID:       got.OperationID,
			BindingDigest:     protocol.BindingDigest(got.Binding),
			AppliedGeneration: got.Generation,
			BackendVersion:    "v1",
			BackendMemoryID:   "backend-1",
			ContentDigest:     got.ContentDigest,
			MutationDigest:    got.MutationDigest,
			CompletedAt:       testOMSTime(),
		})
	}))
	defer server.Close()

	client := &OMSClient{baseURL: server.URL, token: "secret", client: server.Client()}
	receipt, err := client.Mutate(context.Background(), envelope)
	if err != nil {
		t.Fatalf("Mutate() error = %v", err)
	}
	if receipt.Result != protocol.ResultApplied || receipt.OperationID != envelope.OperationID {
		t.Fatalf("receipt = %#v", receipt)
	}
}

func TestOMSClientDoesNotExposeRawInvalidErrorBody(t *testing.T) {
	binding := testOMSBinding()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`provider secret body should not escape`))
	}))
	defer server.Close()

	client := &OMSClient{baseURL: server.URL, token: "secret", client: server.Client()}
	_, err := client.Search(context.Background(), protocol.SearchRequest{ProtocolVersion: protocol.Version, Binding: binding, Mode: protocol.SearchModeKeyword, PageSize: 1})
	if err == nil {
		t.Fatal("Search() error = nil")
	}
	if strings.Contains(err.Error(), "provider secret") {
		t.Fatalf("error leaked provider body: %v", err)
	}
	adapterErr, ok := err.(*AdapterError)
	if !ok || adapterErr.Code != protocol.ErrorCodeInternal || !adapterErr.Retryable {
		t.Fatalf("error = %#v", err)
	}
}

func TestOMSClientDoesNotRetainProviderErrorMessage(t *testing.T) {
	binding := testOMSBinding()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(protocol.ErrorResponse{
			ProtocolVersion: protocol.Version,
			Binding:         &binding,
			Code:            protocol.ErrorCodeInternal,
			Message:         "provider tenant secret must never be stored",
			Retryable:       true,
		})
	}))
	defer server.Close()

	client := &OMSClient{baseURL: server.URL, token: "secret", client: server.Client()}
	_, err := client.Search(context.Background(), protocol.SearchRequest{ProtocolVersion: protocol.Version, Binding: binding, Mode: protocol.SearchModeKeyword, PageSize: 1})
	adapterErr, ok := err.(*AdapterError)
	if !ok {
		t.Fatalf("error = %#v", err)
	}
	if strings.Contains(err.Error(), "provider tenant") || strings.Contains(adapterErr.Message, "provider tenant") {
		t.Fatalf("provider message escaped: %#v", adapterErr)
	}
	if adapterErr.Message != "OMS adapter reported an internal error" || !adapterErr.Retryable {
		t.Fatalf("adapter error = %#v", adapterErr)
	}
}

func TestNewOMSClientUsesExactPinnedResolutionWithoutDNSReplay(t *testing.T) {
	resolver := &omsCountingResolver{}
	dialer := &omsRecordingDialer{}
	policy := endpointpolicy.PublicHTTPSPolicy{Resolver: resolver, Dialer: dialer}
	resolution, err := policy.Resolve(context.Background(), "https://memory.example.com")
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewOMSClient(
		policy,
		nil,
		resolution,
		"sha256:"+strings.Repeat("0", 64),
		"secret",
		protocol.MaxHTTPBodyBytes,
		protocol.MaxAdapterResponseBytes,
		time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}
	transport := client.client.Transport.(*http.Transport)
	if transport.MaxConnsPerHost != omsHTTPMaxConnsPerHost {
		t.Fatalf("MaxConnsPerHost = %d, want %d", transport.MaxConnsPerHost, omsHTTPMaxConnsPerHost)
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

func testOMSBinding() protocol.Binding {
	binding := protocol.Binding{
		ClusterID: "cluster-a", NamespaceUID: "namespace-a", BackendUID: "00000000-0000-4000-8000-000000000002",
		AuthorityEpoch: 1, RoutingEpoch: 1, StoreUUID: "00000000-0000-4000-8000-000000000003",
	}
	binding.TenantID = protocol.DeriveTenantID(binding.ClusterID, binding.NamespaceUID)
	return binding
}

func testOMSTime() time.Time { return time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC) }

func testOMSCapabilitiesResponse(binding protocol.Binding, maxResponseBytes int) protocol.CapabilitiesResponse {
	return protocol.CapabilitiesResponse{
		ProtocolVersion: protocol.Version,
		Binding:         binding,
		AdapterName:     "test-adapter",
		AdapterVersion:  "v1",
		Revision:        "revision-1",
		ExpiresAt:       time.Now().Add(time.Hour),
		Capabilities: protocol.Capabilities{
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
		Limits: protocol.CapabilityLimits{
			MaxRequestBytes:       protocol.MaxHTTPBodyBytes,
			MaxResponseBytes:      maxResponseBytes,
			MaxContentBytes:       protocol.MaxContentBytes,
			MaxTags:               protocol.MaxTags,
			MaxTagBytes:           protocol.MaxTagBytes,
			MaxMetadataEntries:    protocol.MaxMetadataEntries,
			MaxMetadataKeyBytes:   protocol.MaxMetadataKeyBytes,
			MaxMetadataValueBytes: protocol.MaxMetadataValueBytes,
			MaxQueryBytes:         protocol.MaxQueryBytes,
			MaxPageSize:           protocol.MaxPageSize,
			MaxSnapshotRecords:    protocol.MaxSnapshotRecords,
			SnapshotTTLSeconds:    60,
		},
	}
}

func TestOMSClientEnforcesConfiguredMaxRequestBytesBeforeEgress(t *testing.T) {
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
	client := &OMSClient{
		baseURL: server.URL, token: "secret", client: server.Client(),
		maxRequestBytes: int64(len(body) - 1), maxResponseBytes: protocol.MaxAdapterResponseBytes,
	}
	if _, err := client.post(t.Context(), "/test", payload, nil, nil, false); err == nil {
		t.Fatal("post() accepted a request above the advertised limit")
	}
	if requests.Load() != 0 {
		t.Fatalf("adapter requests = %d, want zero before oversized request egress", requests.Load())
	}
	client.maxRequestBytes = int64(protocol.MaxHTTPBodyBytes) + 1
	if got := effectiveOMSRequestLimit(client.maxRequestBytes); got != protocol.MaxHTTPBodyBytes {
		t.Fatalf("effective request limit = %d, want hard limit %d", got, protocol.MaxHTTPBodyBytes)
	}
}

func TestOMSClientEnforcesConfiguredMaxResponseBytesForEveryStatus(t *testing.T) {
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

			client := &OMSClient{
				baseURL: server.URL, token: "secret", client: server.Client(), maxResponseBytes: advertisedLimit,
			}
			_, err := client.post(context.Background(), "/test", struct{}{}, nil, nil, false)
			var adapterErr *AdapterError
			if !errors.As(err, &adapterErr) || adapterErr.Code != protocol.ErrorCodeResponseTooLarge {
				t.Fatalf("post() error = %#v, want response-too-large AdapterError", err)
			}
		})
	}
}

func TestOMSClientResponseLimitAcceptsExactBoundAndKeepsHardMaximum(t *testing.T) {
	exactBody := []byte(`{"ok":true}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(exactBody)
	}))
	defer server.Close()

	client := &OMSClient{
		baseURL: server.URL, token: "secret", client: server.Client(), maxResponseBytes: int64(len(exactBody)),
	}
	if _, err := client.post(context.Background(), "/test", struct{}{}, nil, nil, false); err != nil {
		t.Fatalf("post() rejected response at advertised bound: %v", err)
	}

	client.maxResponseBytes = int64(protocol.MaxAdapterResponseBytes) + 1
	if got := effectiveOMSResponseLimit(client.maxResponseBytes); got != protocol.MaxAdapterResponseBytes {
		t.Fatalf("effective response limit = %d, want hard limit %d", got, protocol.MaxAdapterResponseBytes)
	}

	hardLimitBody := bytes.Repeat([]byte(" "), protocol.MaxAdapterResponseBytes+1)
	hardLimitServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(hardLimitBody)
	}))
	defer hardLimitServer.Close()
	client.baseURL = hardLimitServer.URL
	client.client = hardLimitServer.Client()
	_, err := client.post(context.Background(), "/test", struct{}{}, nil, nil, false)
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Code != protocol.ErrorCodeResponseTooLarge {
		t.Fatalf("post() error = %#v, want hard response limit enforcement", err)
	}
}

func TestOMSClientCapabilitiesRetroactivelyEnforcesAdvertisedMaxResponseBytes(t *testing.T) {
	binding := testOMSBinding()
	capabilities := testOMSCapabilitiesResponse(binding, 1)
	responseBody, err := json.Marshal(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(responseBody)
	}))
	defer server.Close()

	client := &OMSClient{
		baseURL: server.URL, token: "secret", client: server.Client(), maxResponseBytes: protocol.MaxAdapterResponseBytes,
	}
	_, err = client.Capabilities(context.Background(), protocol.CapabilitiesRequest{
		ProtocolVersion: protocol.Version,
		Binding:         binding,
	})
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Code != protocol.ErrorCodeResponseTooLarge {
		t.Fatalf("Capabilities() error = %#v, want retroactive response-too-large rejection", err)
	}
}

func TestOMSClientDecodesTypedSemanticConflictBodies(t *testing.T) {
	binding := testOMSBinding()
	now := testOMSTime()
	mutation := protocol.MutationEnvelope{
		ProtocolVersion: protocol.Version,
		OperationID:     "mop-conflict",
		Binding:         binding,
		MemoryID:        "mem-conflict",
		Kind:            protocol.MutationKindCreate,
		Generation:      1,
		State: &protocol.MutationState{
			Content:  "conflicting guidance",
			Tags:     []string{},
			Metadata: map[string]string{},
		},
	}
	if err := protocol.PrepareMutation(&mutation); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		path   string
		body   any
		invoke func(*OMSClient) (string, error)
		want   string
	}{
		{
			name: "ownership identity conflict",
			path: protocol.PathOwnershipClaim,
			body: protocol.OwnershipClaimResponse{
				ProtocolVersion: protocol.Version, Binding: binding,
				Result: protocol.ResultIdentityConflict, BindingDigest: protocol.BindingDigest(binding),
				ClaimIdentity: protocol.AuthorityDigest(binding), MaximumRoutingEpoch: binding.RoutingEpoch,
				ClaimedAt: now,
			},
			invoke: func(client *OMSClient) (string, error) {
				response, err := client.ClaimOwnership(context.Background(), protocol.OwnershipClaimRequest{
					ProtocolVersion: protocol.Version, Binding: binding,
				})
				if response == nil {
					return "", err
				}
				return response.Result, err
			},
			want: protocol.ResultIdentityConflict,
		},
		{
			name: "routing fence precondition",
			path: protocol.PathRoutingFence,
			body: protocol.RoutingFenceResponse{
				ProtocolVersion: protocol.Version, Binding: binding,
				Result: protocol.ResultPreconditionFailed, BindingDigest: protocol.BindingDigest(binding),
				MaximumRoutingEpoch: binding.RoutingEpoch + 1, CompletedAt: now,
			},
			invoke: func(client *OMSClient) (string, error) {
				response, err := client.AdvanceRoutingFence(context.Background(), protocol.RoutingFenceRequest{
					ProtocolVersion: protocol.Version, Binding: binding,
				})
				if response == nil {
					return "", err
				}
				return response.Result, err
			},
			want: protocol.ResultPreconditionFailed,
		},
		{
			name: "mutation idempotency conflict",
			path: protocol.PathMutations,
			body: protocol.MutationReceipt{
				ProtocolVersion: protocol.Version, Binding: binding,
				Result: protocol.ResultIdempotencyConflict, OperationID: mutation.OperationID,
				BindingDigest: protocol.BindingDigest(binding), ContentDigest: mutation.ContentDigest,
				MutationDigest: mutation.MutationDigest, CompletedAt: now,
			},
			invoke: func(client *OMSClient) (string, error) {
				response, err := client.Mutate(context.Background(), mutation)
				if response == nil {
					return "", err
				}
				return response.Result, err
			},
			want: protocol.ResultIdempotencyConflict,
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

			client := &OMSClient{baseURL: server.URL, token: "secret", client: server.Client()}
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

func TestOMSClientFallsBackToSanitizedGenericConflictError(t *testing.T) {
	binding := testOMSBinding()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(protocol.ErrorResponse{
			ProtocolVersion: protocol.Version,
			Binding:         &binding,
			Code:            protocol.ErrorCodeIdentityConflict,
			Message:         "provider tenant secret must never escape",
		})
	}))
	defer server.Close()

	client := &OMSClient{baseURL: server.URL, token: "secret", client: server.Client()}
	_, err := client.ClaimOwnership(context.Background(), protocol.OwnershipClaimRequest{
		ProtocolVersion: protocol.Version, Binding: binding,
	})
	adapterErr, ok := err.(*AdapterError)
	if !ok {
		t.Fatalf("error = %#v, want *AdapterError", err)
	}
	if adapterErr.Code != protocol.ErrorCodeIdentityConflict ||
		strings.Contains(adapterErr.Message, "provider tenant") || strings.Contains(err.Error(), "provider tenant") {
		t.Fatalf("generic conflict was not safely normalized: %#v", adapterErr)
	}
}

func TestOMSClientTreatsMalformedTransientResponsesAsRetryableAmbiguous(t *testing.T) {
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

			client := &OMSClient{baseURL: server.URL, token: "secret", client: server.Client()}
			_, err := client.Search(context.Background(), protocol.SearchRequest{})
			adapterErr, ok := err.(*AdapterError)
			if !ok || !adapterErr.Retryable || adapterErr.RetryAfterSeconds != 7 {
				t.Fatalf("error = %#v, want retryable ambiguous response", err)
			}
		})
	}
}

func TestOMSClientRequiresParsedApplicationJSONForEveryStatus(t *testing.T) {
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
			client := &OMSClient{baseURL: server.URL, token: "secret", client: server.Client()}
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

func TestOMSClientTreatsMalformedSuccessfulMutationAsRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("committed but malformed"))
	}))
	defer server.Close()
	client := &OMSClient{baseURL: server.URL, token: "secret", client: server.Client()}
	_, err := client.Mutate(context.Background(), protocol.MutationEnvelope{})
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) || !adapterErr.Retryable {
		t.Fatalf("Mutate() error = %#v, want retryable malformed acknowledgement", err)
	}
}

func TestOMSClientTreatsMalformedMutationAcknowledgementAsAmbiguousAtEveryStatus(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusConflict, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"malformed":`))
			}))
			defer server.Close()
			client := &OMSClient{baseURL: server.URL, token: "secret", client: server.Client()}
			_, err := client.Mutate(context.Background(), protocol.MutationEnvelope{})
			var adapterErr *AdapterError
			if !errors.As(err, &adapterErr) || !adapterErr.Retryable {
				t.Fatalf("Mutate() error = %#v, want ambiguous retryable acknowledgement", err)
			}
		})
	}
}

func TestOMSClientAcceptsPreAuthenticationUnauthorizedWithoutBinding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(protocol.ErrorResponse{
			ProtocolVersion: protocol.Version,
			Binding:         nil,
			Code:            protocol.ErrorCodeUnauthorized,
			Message:         "unauthorized",
			Retryable:       false,
		})
	}))
	defer server.Close()

	client := &OMSClient{baseURL: server.URL, token: "secret", client: server.Client()}
	_, err := client.Mutate(context.Background(), protocol.MutationEnvelope{Binding: testOMSBinding()})
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) {
		t.Fatalf("Mutate() error = %#v, want *AdapterError", err)
	}
	if adapterErr.StatusCode != http.StatusUnauthorized ||
		adapterErr.Code != protocol.ErrorCodeUnauthorized || adapterErr.Retryable {
		t.Fatalf("Mutate() error = %#v, want definitive pre-authentication unauthorized error", adapterErr)
	}
}

func TestOMSClientRejectsOtherNilErrorBindings(t *testing.T) {
	tests := []struct {
		name   string
		status int
		code   string
	}{
		{name: "wrong status", status: http.StatusForbidden, code: protocol.ErrorCodeUnauthorized},
		{name: "wrong code", status: http.StatusUnauthorized, code: protocol.ErrorCodeInvalidRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_ = json.NewEncoder(w).Encode(protocol.ErrorResponse{
					ProtocolVersion: protocol.Version,
					Binding:         nil,
					Code:            test.code,
					Message:         "rejected",
					Retryable:       false,
				})
			}))
			defer server.Close()

			client := &OMSClient{baseURL: server.URL, token: "secret", client: server.Client()}
			_, err := client.Mutate(context.Background(), protocol.MutationEnvelope{Binding: testOMSBinding()})
			var adapterErr *AdapterError
			if !errors.As(err, &adapterErr) || adapterErr.Code != protocol.ErrorCodeIdentityConflict || !adapterErr.Retryable {
				t.Fatalf("Mutate() error = %#v, want ambiguous binding mismatch", err)
			}
		})
	}
}

func TestOMSClientRejectsMismatchedErrorBinding(t *testing.T) {
	requestBinding := testOMSBinding()
	otherBinding := requestBinding
	otherBinding.RoutingEpoch++
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(protocol.ErrorResponse{
			ProtocolVersion: protocol.Version, Binding: &otherBinding, Code: protocol.ErrorCodeInvalidRequest,
			Message: "invalid", Retryable: false,
		})
	}))
	defer server.Close()
	client := &OMSClient{baseURL: server.URL, token: "secret", client: server.Client()}
	_, err := client.Mutate(context.Background(), protocol.MutationEnvelope{Binding: requestBinding})
	var adapterErr *AdapterError
	if !errors.As(err, &adapterErr) || !adapterErr.Retryable {
		t.Fatalf("Mutate() error = %#v, want ambiguous binding mismatch", err)
	}
}

func TestNewOMSClientPinsValidatedServerCertificateDigest(t *testing.T) {
	resolver := &omsCountingResolver{}
	dialer := &omsRecordingDialer{}
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
	client, err := NewOMSClient(
		policy,
		nil,
		resolution,
		digest,
		"secret",
		protocol.MaxHTTPBodyBytes,
		protocol.MaxAdapterResponseBytes,
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
