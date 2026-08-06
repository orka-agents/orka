package conformance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orka-agents/orka/pkg/oms/protocol"
)

const validTestCase = "valid"

func testCapabilityLimits() protocol.CapabilityLimits {
	return protocol.CapabilityLimits{
		MaxRequestBytes:       protocol.MaxHTTPBodyBytes,
		MaxResponseBytes:      protocol.MaxAdapterResponseBytes,
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
	}
}

func requireFixturePlan(t *testing.T, runID string, limits protocol.CapabilityLimits) fixturePlan {
	t.Helper()
	fixtures, err := newFixturePlan(runID, limits)
	if err != nil {
		t.Fatal(err)
	}
	return fixtures
}

func TestFixturePlanRespectsSmallAdvertisedStateLimits(t *testing.T) {
	limits := testCapabilityLimits()
	limits.MaxContentBytes = 17
	limits.MaxTags = 1
	limits.MaxTagBytes = 1
	limits.MaxMetadataEntries = 1
	limits.MaxMetadataKeyBytes = 1
	limits.MaxMetadataValueBytes = 1
	limits.MaxQueryBytes = 17
	limits.MaxPageSize = 1
	limits.MaxSnapshotRecords = 2

	fixtures := requireFixturePlan(t, "small-limits", limits)
	request, err := fixtures.searchRequest(DefaultBinding(), protocol.SearchModeKeyword)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Query) != 17 || request.PageSize != 1 {
		t.Fatalf("search fixture query bytes/pageSize = %d/%d, want 17/1", len(request.Query), request.PageSize)
	}

	mutation, err := fixtures.mutation(
		DefaultBinding(),
		fixtureRoleMain,
		fixtures.memoryID(fixtureRoleMain),
		protocol.MutationKindCreate,
		1,
		0,
		"",
		fixtures.state(request.Query),
	)
	if err != nil {
		t.Fatal(err)
	}
	if mutation.State == nil {
		t.Fatal("mutation fixture omitted state")
	}
	if len(mutation.State.Content) != 17 || len(mutation.State.Tags) != 1 || len(mutation.State.Metadata) != 1 {
		t.Fatalf(
			"mutation fixture content bytes/tags/metadata = %d/%d/%d, want 17/1/1",
			len(mutation.State.Content),
			len(mutation.State.Tags),
			len(mutation.State.Metadata),
		)
	}
	for _, tag := range mutation.State.Tags {
		if len(tag) > limits.MaxTagBytes {
			t.Fatalf("tag uses %d bytes, maxTagBytes=%d", len(tag), limits.MaxTagBytes)
		}
	}
	for key, value := range mutation.State.Metadata {
		if len(key) > limits.MaxMetadataKeyBytes || len(value) > limits.MaxMetadataValueBytes {
			t.Fatalf(
				"metadata key/value bytes = %d/%d, limits %d/%d",
				len(key),
				len(value),
				limits.MaxMetadataKeyBytes,
				limits.MaxMetadataValueBytes,
			)
		}
	}
}

func TestFixturePlanRejectsQueryLimitsWithoutRunIsolation(t *testing.T) {
	limits := testCapabilityLimits()
	limits.MaxContentBytes = 1
	limits.MaxQueryBytes = 1
	limits.MaxSnapshotRecords = 2
	_, err := newFixturePlan("small-query", limits)
	if err == nil || !strings.Contains(err.Error(), "run isolation") {
		t.Fatalf("newFixturePlan() error = %v, want precise run-isolation incompatibility", err)
	}
}

func TestContractClientEnforcesAdvertisedMaxRequestBytesBeforeSending(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		writeConformanceJSON(writer, http.StatusOK, map[string]bool{"ok": true})
	}))
	t.Cleanup(server.Close)
	client, err := newContractClient(Target{
		BaseURL: server.URL, AuthorizationValue: testAuthorizationToken,
		HTTPClient: server.Client(), InsecureLoopbackOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := protocol.SearchRequest{
		ProtocolVersion: protocol.Version, Binding: DefaultBinding(), Mode: protocol.SearchModeKeyword,
		Query: "q", PageSize: 1, PageToken: "",
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	limits := testCapabilityLimits()
	limits.MaxRequestBytes = len(body)
	client.configureLimits(limits)
	if _, _, err := client.postJSON(context.Background(), protocol.PathSearch, request); err != nil {
		t.Fatalf("request at advertised bound failed: %v", err)
	}

	limits.MaxRequestBytes--
	client.configureLimits(limits)
	if _, _, err := client.postJSON(context.Background(), protocol.PathSearch, request); err == nil {
		t.Fatal("request above advertised maxRequestBytes was sent")
	} else if !strings.Contains(err.Error(), "maxRequestBytes") {
		t.Fatalf("request bound error = %q, want maxRequestBytes context", err)
	}
	if requests != 1 {
		t.Fatalf("server received %d requests, want only the exactly bounded request", requests)
	}
}

func TestSanitizeCheckResultRedactsConfiguredValue(t *testing.T) {
	sensitiveValue := "marker-" + "value"
	result := CheckResult{
		Message: "request failed with " + sensitiveValue,
		Capabilities: &protocol.CapabilitiesResponse{
			ProtocolVersion: protocol.Version, AdapterName: "adapter-" + sensitiveValue,
			AdapterVersion: sensitiveValue, Revision: "revision-" + sensitiveValue,
		},
	}
	sanitized := SanitizeCheckResult(result, sensitiveValue)
	data := sanitized.Message + sanitized.Capabilities.AdapterName +
		sanitized.Capabilities.AdapterVersion + sanitized.Capabilities.Revision
	if strings.Contains(data, sensitiveValue) {
		t.Fatalf("sanitized result leaked configured value: %s", data)
	}
	if !strings.Contains(data, redactedValue) {
		t.Fatalf("sanitized result omitted redaction marker: %s", data)
	}
}

func TestValidateCheckpointRejectsCrossAuthorityState(t *testing.T) {
	checkpoint := Checkpoint{CheckpointVersion: checkpointVersion, ProtocolVersion: protocol.Version, RunID: "test"}
	checkpoint.Binding = DefaultBinding()
	if err := ValidateCheckpoint(checkpoint); err == nil {
		t.Fatal("empty checkpoint was accepted")
	}
}

func TestSearchRejectsRequestSpecificResponseViolations(t *testing.T) {
	binding := DefaultBinding()
	now := time.Now().UTC()
	record := func(id string) protocol.MemoryRecord {
		content := "content " + id
		return protocol.MemoryRecord{
			MemoryID: id, UpsertKey: protocol.CanonicalUpsertKey(binding, id), State: protocol.RecordStateLive,
			Generation: 1, BackendVersion: "version-1", BackendMemoryID: "provider-" + id,
			ContentDigest: protocol.ContentDigest(content), Content: content,
			Tags: []string{}, Metadata: map[string]string{}, UpdatedAt: now,
		}
	}
	const firstToken = "oms-page-v1.0123456789abcdef0123456789abcdef.1"
	baseRequest := protocol.SearchRequest{
		ProtocolVersion: protocol.Version, Binding: binding, Mode: protocol.SearchModeKeyword,
		Query: "", PageSize: 1, PageToken: "",
	}
	baseResponse := protocol.SearchResponse{
		ProtocolVersion: protocol.Version, Binding: binding, RequestedMode: protocol.SearchModeKeyword,
		ActualMode: protocol.SearchModeKeyword, Records: []protocol.MemoryRecord{}, Exhausted: true,
		SnapshotExpiresAt: now.Add(time.Minute),
	}
	for _, tc := range []struct {
		name     string
		request  protocol.SearchRequest
		response protocol.SearchResponse
	}{
		{
			name: "requested mode mismatch", request: baseRequest,
			response: func() protocol.SearchResponse {
				value := baseResponse
				value.RequestedMode = protocol.SearchModeAuto
				return value
			}(),
		},
		{
			name: "request page size exceeded", request: baseRequest,
			response: func() protocol.SearchResponse {
				value := baseResponse
				value.Records = []protocol.MemoryRecord{record("mem-page-1"), record("mem-page-2")}
				return value
			}(),
		},
		{
			name: "binding mismatch", request: baseRequest,
			response: func() protocol.SearchResponse { value := baseResponse; value.Binding.RoutingEpoch++; return value }(),
		},
		{
			name:    "continuation token did not advance",
			request: func() protocol.SearchRequest { value := baseRequest; value.PageToken = firstToken; return value }(),
			response: func() protocol.SearchResponse {
				value := baseResponse
				value.Exhausted = false
				value.NextPageToken = firstToken
				return value
			}(),
		},
		{
			name: "snapshot expiry exceeds advertised TTL", request: baseRequest,
			response: func() protocol.SearchResponse {
				value := baseResponse
				value.SnapshotExpiresAt = now.Add(2 * time.Minute)
				return value
			}(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", jsonMediaType)
				writer.Header().Set("Cache-Control", "no-store")
				_ = json.NewEncoder(writer).Encode(tc.response)
			}))
			t.Cleanup(server.Close)
			client, err := newContractClient(Target{
				BaseURL:              server.URL,
				AuthorizationValue:   testAuthorizationToken,
				HTTPClient:           server.Client(),
				InsecureLoopbackOnly: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			client.limits = testCapabilityLimits()
			client.limitsConfigured = true
			if _, err := search(context.Background(), client, tc.request); err == nil {
				t.Fatalf("search() accepted %s", tc.name)
			}
		})
	}
}

func TestExpectCodecRejectionRequiresExactInvalidRequestVariant(t *testing.T) {
	binding := DefaultBinding()
	valid := protocol.ErrorResponse{
		ProtocolVersion: protocol.Version, Code: protocol.ErrorCodeInvalidRequest,
		Message: "invalid request", Retryable: false, RetryAfterSeconds: 0,
	}
	for _, tc := range []struct {
		name       string
		status     int
		response   protocol.ErrorResponse
		retryAfter string
		setRetry   bool
		wantErr    bool
	}{
		{name: validTestCase, status: http.StatusBadRequest, response: valid},
		{name: "wrong status", status: http.StatusConflict, response: valid, wantErr: true},
		{name: "wrong code", status: http.StatusBadRequest, response: func() protocol.ErrorResponse {
			value := valid
			value.Code = protocol.ErrorCodeIdentityConflict
			return value
		}(), wantErr: true},
		{name: "unexpected binding", status: http.StatusBadRequest, response: func() protocol.ErrorResponse {
			value := valid
			value.Binding = &binding
			return value
		}(), wantErr: true},
		{name: "retryable", status: http.StatusBadRequest, response: func() protocol.ErrorResponse {
			value := valid
			value.Retryable = true
			value.RetryAfterSeconds = 1
			return value
		}(), wantErr: true},
		{
			name: "retry header", status: http.StatusBadRequest, response: valid,
			retryAfter: "1", setRetry: true, wantErr: true,
		},
		{name: "empty retry header", status: http.StatusBadRequest, response: valid, setRetry: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				if tc.setRetry {
					writer.Header().Set("Retry-After", tc.retryAfter)
				}
				writeConformanceJSON(writer, tc.status, tc.response)
			}))
			t.Cleanup(server.Close)
			client, err := newContractClient(Target{
				BaseURL: server.URL, AuthorizationValue: testAuthorizationToken,
				HTTPClient: server.Client(), InsecureLoopbackOnly: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			err = expectCodecRejection(
				context.Background(), client, protocol.PathCapabilities, tc.name,
				http.StatusBadRequest, []byte(`{"unknown":true}`),
			)
			if (err != nil) != tc.wantErr {
				t.Fatalf("expectCodecRejection() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestVerifyAuthenticationRequiresExactUnauthorizedVariant(t *testing.T) {
	binding := DefaultBinding()
	storeBinding := protocol.StoreResolutionBinding{
		ClusterID: binding.ClusterID, NamespaceUID: binding.NamespaceUID,
		BackendUID: binding.BackendUID, TenantID: binding.TenantID,
	}
	valid := protocol.ErrorResponse{
		ProtocolVersion: protocol.Version, Code: protocol.ErrorCodeUnauthorized,
		Message: "unauthorized", Retryable: false, RetryAfterSeconds: 0,
	}
	for _, tc := range []struct {
		name     string
		response protocol.ErrorResponse
		wantErr  bool
	}{
		{name: validTestCase, response: valid},
		{name: "bound", response: func() protocol.ErrorResponse {
			value := valid
			value.Binding = &binding
			return value
		}(), wantErr: true},
		{name: "retryable", response: func() protocol.ErrorResponse {
			value := valid
			value.Retryable = true
			value.RetryAfterSeconds = 1
			return value
		}(), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writeConformanceJSON(writer, http.StatusUnauthorized, tc.response)
			}))
			t.Cleanup(server.Close)
			client, err := newContractClient(Target{
				BaseURL: server.URL, AuthorizationValue: testAuthorizationToken,
				HTTPClient: server.Client(), InsecureLoopbackOnly: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			fixtures := requireFixturePlan(t, "authentication", testCapabilityLimits())
			err = verifyAuthentication(
				context.Background(), client, storeBinding, "conformance-store", binding, fixtures,
			)
			if (err != nil) != tc.wantErr {
				t.Fatalf("verifyAuthentication() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestVerifySearchModeCapabilitiesFollowsAdvertisement(t *testing.T) {
	binding := DefaultBinding()
	for _, tc := range []struct {
		name             string
		capabilities     protocol.Capabilities
		autoMode         string
		rejectAdvertised string
		wantErr          bool
	}{
		{
			name: "semantic and hybrid advertised",
			capabilities: protocol.Capabilities{
				KeywordSearch: true, SemanticSearch: true, HybridSearch: true,
			},
			autoMode: protocol.SearchModeHybrid,
		},
		{
			name: "semantic and hybrid unadvertised",
			capabilities: protocol.Capabilities{
				KeywordSearch: true,
			},
			autoMode: protocol.SearchModeKeyword,
		},
		{
			name: "auto selected unadvertised mode",
			capabilities: protocol.Capabilities{
				KeywordSearch: true,
			},
			autoMode: protocol.SearchModeSemantic,
			wantErr:  true,
		},
		{
			name: "advertised semantic rejected",
			capabilities: protocol.Capabilities{
				KeywordSearch: true, SemanticSearch: true,
			},
			autoMode: protocol.SearchModeKeyword, rejectAdvertised: protocol.SearchModeSemantic,
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expiresAt := time.Now().UTC().Add(time.Minute)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var searchRequest protocol.SearchRequest
				if err := json.NewDecoder(request.Body).Decode(&searchRequest); err != nil {
					writeConformanceJSON(writer, http.StatusBadRequest, protocol.ErrorResponse{
						ProtocolVersion: protocol.Version, Code: protocol.ErrorCodeInvalidRequest,
						Message: "invalid", Retryable: false, RetryAfterSeconds: 0,
					})
					return
				}
				actualMode := searchRequest.Mode
				if searchRequest.Mode == protocol.SearchModeAuto {
					actualMode = tc.autoMode
				}
				advertised := searchModeAdvertised(tc.capabilities, searchRequest.Mode)
				if searchRequest.Mode != protocol.SearchModeAuto && (!advertised || searchRequest.Mode == tc.rejectAdvertised) {
					responseBinding := binding
					writeConformanceJSON(writer, http.StatusUnprocessableEntity, protocol.ErrorResponse{
						ProtocolVersion: protocol.Version, Binding: &responseBinding,
						Code: protocol.ErrorCodeSearchModeUnsupported, Message: "unsupported",
						Retryable: false, RetryAfterSeconds: 0,
					})
					return
				}
				writeConformanceJSON(writer, http.StatusOK, protocol.SearchResponse{
					ProtocolVersion: protocol.Version, Binding: binding,
					RequestedMode: searchRequest.Mode, ActualMode: actualMode,
					Records: []protocol.MemoryRecord{}, Exhausted: true,
					SnapshotExpiresAt: expiresAt,
				})
			}))
			t.Cleanup(server.Close)
			client, err := newContractClient(Target{
				BaseURL: server.URL, AuthorizationValue: testAuthorizationToken,
				HTTPClient: server.Client(), InsecureLoopbackOnly: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			fixtures := requireFixturePlan(t, "search-modes", testCapabilityLimits())
			err = verifySearchModeCapabilities(context.Background(), client, binding, tc.capabilities, fixtures)
			if (err != nil) != tc.wantErr {
				t.Fatalf("verifySearchModeCapabilities() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestVerifySearchModeCapabilitiesConsumesEveryContinuation(t *testing.T) {
	binding := DefaultBinding()
	capabilities := protocol.Capabilities{
		KeywordSearch: true, SemanticSearch: true, HybridSearch: true,
	}
	expiresAt := time.Now().UTC().Add(time.Minute)
	tokens := map[string]string{
		protocol.SearchModeAuto:     "oms-page-v1.11111111111111111111111111111111.1",
		protocol.SearchModeSemantic: "oms-page-v1.22222222222222222222222222222222.1",
		protocol.SearchModeHybrid:   "oms-page-v1.33333333333333333333333333333333.1",
	}
	var mutex sync.Mutex
	activeSnapshot := false
	continuations := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var searchRequest protocol.SearchRequest
		if err := json.NewDecoder(request.Body).Decode(&searchRequest); err != nil {
			writeConformanceJSON(writer, http.StatusBadRequest, protocol.ErrorResponse{
				ProtocolVersion: protocol.Version, Code: protocol.ErrorCodeInvalidRequest,
				Message: "invalid", Retryable: false, RetryAfterSeconds: 0,
			})
			return
		}
		actualMode := searchRequest.Mode
		if actualMode == protocol.SearchModeAuto {
			actualMode = protocol.SearchModeKeyword
		}
		mutex.Lock()
		defer mutex.Unlock()
		if searchRequest.PageToken == "" {
			if activeSnapshot {
				responseBinding := binding
				writer.Header().Set("Retry-After", "1")
				writeConformanceJSON(writer, http.StatusServiceUnavailable, protocol.ErrorResponse{
					ProtocolVersion: protocol.Version, Binding: &responseBinding,
					Code: protocol.ErrorCodeSnapshotCapacity, Message: "capacity",
					Retryable: true, RetryAfterSeconds: 1,
				})
				return
			}
			activeSnapshot = true
			writeConformanceJSON(writer, http.StatusOK, protocol.SearchResponse{
				ProtocolVersion: protocol.Version, Binding: binding,
				RequestedMode: searchRequest.Mode, ActualMode: actualMode,
				Records: []protocol.MemoryRecord{}, NextPageToken: tokens[searchRequest.Mode],
				Exhausted: false, SnapshotExpiresAt: expiresAt,
			})
			return
		}
		activeSnapshot = false
		continuations++
		writeConformanceJSON(writer, http.StatusOK, protocol.SearchResponse{
			ProtocolVersion: protocol.Version, Binding: binding,
			RequestedMode: searchRequest.Mode, ActualMode: actualMode,
			Records: []protocol.MemoryRecord{}, Exhausted: true,
			SnapshotExpiresAt: expiresAt,
		})
	}))
	t.Cleanup(server.Close)
	client, err := newContractClient(Target{
		BaseURL: server.URL, AuthorizationValue: testAuthorizationToken,
		HTTPClient: server.Client(), InsecureLoopbackOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	limits := testCapabilityLimits()
	limits.MaxPageSize = 1
	fixtures := requireFixturePlan(t, "search-continuations", limits)
	if err := verifySearchModeCapabilities(context.Background(), client, binding, capabilities, fixtures); err != nil {
		t.Fatalf("verifySearchModeCapabilities(): %v", err)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if activeSnapshot || continuations != 3 {
		t.Fatalf("activeSnapshot=%v continuations=%d, want false and 3", activeSnapshot, continuations)
	}
}

func TestPaginationFixtureSupportsAdvertisedTwoRecordSnapshotAcrossPages(t *testing.T) {
	binding := DefaultBinding()
	expiresAt := time.Now().UTC().Add(time.Minute)
	records := []protocol.MemoryRecord{
		conformanceRecord(binding, "mem-page-a", "page a", 1),
		conformanceRecord(binding, "mem-page-b", "page b", 2),
	}
	token := "oms-page-v1.0123456789abcdef0123456789abcdef.1"
	var mutex sync.Mutex
	var requests []protocol.SearchRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var searchRequest protocol.SearchRequest
		if err := json.NewDecoder(request.Body).Decode(&searchRequest); err != nil {
			writeConformanceJSON(writer, http.StatusBadRequest, protocol.ErrorResponse{
				ProtocolVersion: protocol.Version, Code: protocol.ErrorCodeInvalidRequest,
				Message: "invalid", Retryable: false, RetryAfterSeconds: 0,
			})
			return
		}
		mutex.Lock()
		requests = append(requests, searchRequest)
		mutex.Unlock()
		if searchRequest.PageSize > 1 {
			writeConformanceJSON(writer, http.StatusBadRequest, protocol.ErrorResponse{
				ProtocolVersion: protocol.Version, Code: protocol.ErrorCodeInvalidRequest,
				Message: "pageSize exceeds maxPageSize", Retryable: false, RetryAfterSeconds: 0,
			})
			return
		}

		response := protocol.SearchResponse{
			ProtocolVersion: protocol.Version, Binding: binding,
			RequestedMode: protocol.SearchModeKeyword, ActualMode: protocol.SearchModeKeyword,
			SnapshotExpiresAt: expiresAt,
		}
		switch searchRequest.PageToken {
		case "":
			response.Records = []protocol.MemoryRecord{records[0]}
			response.NextPageToken = token
			response.Exhausted = false
		case token:
			response.Records = []protocol.MemoryRecord{records[1]}
			response.Exhausted = true
		default:
			writeConformanceJSON(writer, http.StatusBadRequest, protocol.ErrorResponse{
				ProtocolVersion: protocol.Version, Code: protocol.ErrorCodeInvalidRequest,
				Message: "unexpected page token", Retryable: false, RetryAfterSeconds: 0,
			})
			return
		}
		writeConformanceJSON(writer, http.StatusOK, response)
	}))
	t.Cleanup(server.Close)
	client, err := newContractClient(Target{
		BaseURL: server.URL, AuthorizationValue: testAuthorizationToken,
		HTTPClient: server.Client(), InsecureLoopbackOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	limits := testCapabilityLimits()
	limits.MaxPageSize = 1
	limits.MaxSnapshotRecords = 2
	fixtures := requireFixturePlan(t, "pagination", limits)
	client.configureLimits(limits)
	request, err := fixtures.searchRequest(binding, protocol.SearchModeKeyword)
	if err != nil {
		t.Fatal(err)
	}
	first, err := search(context.Background(), client, request)
	if err != nil {
		t.Fatalf("search(): %v", err)
	}
	if first.Exhausted {
		t.Fatal("pagination fixture exhausted on the first page")
	}
	snapshot, err := collectSnapshot(context.Background(), client, request, first)
	if err != nil {
		t.Fatalf("collectSnapshot(): %v", err)
	}
	if len(snapshot) != len(records) {
		t.Fatalf("collectSnapshot() returned %d records, want %d", len(snapshot), len(records))
	}

	mutex.Lock()
	defer mutex.Unlock()
	if len(requests) != len(records) {
		t.Fatalf("search request count = %d, want %d", len(requests), len(records))
	}
	for i := range requests {
		if requests[i].PageSize != 1 {
			t.Fatalf("request %d pageSize = %d, want 1", i, requests[i].PageSize)
		}
	}
}

func TestSnapshotProofRejectsDuplicatesAndChangedRecords(t *testing.T) {
	binding := DefaultBinding()
	records := []protocol.MemoryRecord{
		conformanceRecord(binding, "mem-proof-a", "proof a", 1),
		conformanceRecord(binding, "mem-proof-b", "proof b", 2),
		conformanceRecord(binding, "mem-proof-c", "proof c", 3),
	}
	expected, err := recordDigestByKey(binding, records)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifySnapshotFixture(binding, records, expected); err != nil {
		t.Fatalf("verifySnapshotFixture(valid): %v", err)
	}

	duplicate := append([]protocol.MemoryRecord(nil), records...)
	duplicate[len(duplicate)-1] = duplicate[0]
	if err := verifySnapshotFixture(binding, duplicate, expected); err == nil {
		t.Fatal("verifySnapshotFixture() accepted a duplicate record")
	}

	changed := append([]protocol.MemoryRecord(nil), records...)
	changed[1].Content = "changed proof b"
	changed[1].ContentDigest = protocol.ContentDigest(changed[1].Content)
	if err := verifySnapshotFixture(binding, changed, expected); err == nil {
		t.Fatal("verifySnapshotFixture() accepted changed record content")
	}

	keys, digests, err := recordProofs(binding, records)
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().UTC().Add(time.Minute)
	checkpoint := Checkpoint{
		PaginationExpectedKeys: keys, PaginationExpectedDigests: digests,
		PaginationActualMode: protocol.SearchModeKeyword, PaginationSnapshotExpiry: expiresAt,
	}
	continuation := snapshotContinuation{
		Records: records, ActualMode: protocol.SearchModeKeyword, SnapshotExpiresAt: expiresAt,
	}
	if err := verifyContinuationProof(binding, continuation, checkpoint); err != nil {
		t.Fatalf("verifyContinuationProof(valid): %v", err)
	}
	continuation.ActualMode = protocol.SearchModeSemantic
	if err := verifyContinuationProof(binding, continuation, checkpoint); err == nil {
		t.Fatal("verifyContinuationProof() accepted a changed actual mode")
	}
	continuation.ActualMode = protocol.SearchModeKeyword
	continuation.SnapshotExpiresAt = expiresAt.Add(time.Second)
	if err := verifyContinuationProof(binding, continuation, checkpoint); err == nil {
		t.Fatal("verifyContinuationProof() accepted a changed snapshot expiry")
	}
	continuation.SnapshotExpiresAt = expiresAt
	continuation.Records = changed
	if err := verifyContinuationProof(binding, continuation, checkpoint); err == nil {
		t.Fatal("verifyContinuationProof() accepted changed record digests")
	}
}

func TestCollectSnapshotRejectsDuplicateRecordsAcrossPages(t *testing.T) {
	binding := DefaultBinding()
	record := conformanceRecord(binding, "mem-duplicate-page", "duplicate page", 1)
	expiresAt := time.Now().UTC().Add(time.Minute)
	token := "oms-page-v1.0123456789abcdef0123456789abcdef.1"
	first := &protocol.SearchResponse{
		ProtocolVersion: protocol.Version, Binding: binding,
		RequestedMode: protocol.SearchModeKeyword, ActualMode: protocol.SearchModeKeyword,
		Records: []protocol.MemoryRecord{record}, NextPageToken: token,
		Exhausted: false, SnapshotExpiresAt: expiresAt,
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeConformanceJSON(writer, http.StatusOK, protocol.SearchResponse{
			ProtocolVersion: protocol.Version, Binding: binding,
			RequestedMode: protocol.SearchModeKeyword, ActualMode: protocol.SearchModeKeyword,
			Records: []protocol.MemoryRecord{record}, Exhausted: true,
			SnapshotExpiresAt: expiresAt,
		})
	}))
	t.Cleanup(server.Close)
	client, err := newContractClient(Target{
		BaseURL: server.URL, AuthorizationValue: testAuthorizationToken,
		HTTPClient: server.Client(), InsecureLoopbackOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := protocol.SearchRequest{
		ProtocolVersion: protocol.Version, Binding: binding, Mode: protocol.SearchModeKeyword,
		Query: "duplicate", PageSize: 1,
	}
	if _, err := collectSnapshot(context.Background(), client, request, first); err == nil {
		t.Fatal("collectSnapshot() accepted a duplicate record across pages")
	}
}

func conformanceRecord(binding protocol.Binding, memoryID, content string, minute int) protocol.MemoryRecord {
	return protocol.MemoryRecord{
		MemoryID: memoryID, UpsertKey: protocol.CanonicalUpsertKey(binding, memoryID),
		State: protocol.RecordStateLive, Generation: 1,
		BackendVersion: "version-1", BackendMemoryID: "provider-" + memoryID,
		ContentDigest: protocol.ContentDigest(content), Content: content,
		Tags: []string{conformanceTag}, Metadata: map[string]string{"suite": "oms"},
		UpdatedAt: time.Date(2026, time.July, 30, 12, minute, 0, 0, time.UTC),
	}
}

func writeConformanceJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", jsonMediaType)
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
