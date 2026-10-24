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

	memoryv1 "github.com/orka-agents/orka/pkg/memory/v1"
)

const validTestCase = "valid"

func testCapabilityLimits() memoryv1.CapabilityLimits {
	return memoryv1.CapabilityLimits{
		MaxRequestBytes:       memoryv1.MaxHTTPBodyBytes,
		MaxResponseBytes:      memoryv1.MaxAdapterResponseBytes,
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
	}
}

func requireFixturePlan(t *testing.T, runID string, limits memoryv1.CapabilityLimits) fixturePlan {
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
	request, err := fixtures.searchRequest(DefaultBinding(), memoryv1.SearchModeKeyword)
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
		memoryv1.MutationKindCreate,
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
	request := memoryv1.SearchRequest{
		ProtocolVersion: memoryv1.Version, Binding: DefaultBinding(), Mode: memoryv1.SearchModeKeyword,
		Query: "q", PageSize: 1, PageToken: "",
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	limits := testCapabilityLimits()
	limits.MaxRequestBytes = len(body)
	client.configureLimits(limits)
	if _, _, err := client.postJSON(context.Background(), memoryv1.PathSearch, request); err != nil {
		t.Fatalf("request at advertised bound failed: %v", err)
	}

	limits.MaxRequestBytes--
	client.configureLimits(limits)
	if _, _, err := client.postJSON(context.Background(), memoryv1.PathSearch, request); err == nil {
		t.Fatal("request above advertised maxRequestBytes was sent")
	} else if !strings.Contains(err.Error(), "maxRequestBytes") {
		t.Fatalf("request bound error = %q, want maxRequestBytes context", err)
	}
	if requests != 1 {
		t.Fatalf("server received %d requests, want only the exactly bounded request", requests)
	}
}

func TestVerifyRestartCapabilitiesComparesFlagsAndLimitsButIgnoresExpiry(t *testing.T) {
	prepared := testCapabilitiesResponse(DefaultBinding())
	checkpoint := Checkpoint{
		CapabilitiesRevision: prepared.Revision,
		Capabilities:         prepared.Capabilities,
		CapabilityLimits:     prepared.Limits,
	}

	renewed := prepared
	renewed.ExpiresAt = prepared.ExpiresAt.Add(time.Hour)
	if err := verifyRestartCapabilities(&renewed, checkpoint); err != nil {
		t.Fatalf("verifyRestartCapabilities(renewed expiry): %v", err)
	}

	changedFlags := renewed
	changedFlags.Capabilities.SemanticSearch = !changedFlags.Capabilities.SemanticSearch
	if err := verifyRestartCapabilities(&changedFlags, checkpoint); err == nil {
		t.Fatal("verifyRestartCapabilities() accepted changed capability flags")
	} else if !strings.Contains(err.Error(), "flags changed") {
		t.Fatalf("capability flag error = %q, want flags changed context", err)
	}

	changedLimits := renewed
	changedLimits.Limits.MaxSnapshotRecords--
	if err := verifyRestartCapabilities(&changedLimits, checkpoint); err == nil {
		t.Fatal("verifyRestartCapabilities() accepted changed capability limits")
	} else if !strings.Contains(err.Error(), "limits changed") {
		t.Fatalf("capability limit error = %q, want limits changed context", err)
	}
}

func TestSanitizeCheckResultRedactsConfiguredValue(t *testing.T) {
	sensitiveValue := "marker-" + "value"
	result := CheckResult{
		Message: "request failed with " + sensitiveValue,
		Capabilities: &memoryv1.CapabilitiesResponse{
			ProtocolVersion: memoryv1.Version, AdapterName: "adapter-" + sensitiveValue,
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
	checkpoint := Checkpoint{CheckpointVersion: checkpointVersion, ProtocolVersion: memoryv1.Version, RunID: "test"}
	checkpoint.Binding = DefaultBinding()
	if err := ValidateCheckpoint(checkpoint); err == nil {
		t.Fatal("empty checkpoint was accepted")
	}
}

func TestSearchRejectsRequestSpecificResponseViolations(t *testing.T) {
	binding := DefaultBinding()
	now := time.Now().UTC()
	record := func(id string) memoryv1.MemoryRecord {
		content := "content " + id
		return memoryv1.MemoryRecord{
			MemoryID: id, UpsertKey: memoryv1.CanonicalUpsertKey(binding, id), State: memoryv1.RecordStateLive,
			Generation: 1, BackendVersion: "version-1", BackendMemoryID: "provider-" + id,
			ContentDigest: memoryv1.ContentDigest(content), Content: content,
			Tags: []string{}, Metadata: map[string]string{}, UpdatedAt: now,
		}
	}
	const firstToken = "memory-page-v1.0123456789abcdef0123456789abcdef.1"
	baseRequest := memoryv1.SearchRequest{
		ProtocolVersion: memoryv1.Version, Binding: binding, Mode: memoryv1.SearchModeKeyword,
		Query: "", PageSize: 1, PageToken: "",
	}
	baseResponse := memoryv1.SearchResponse{
		ProtocolVersion: memoryv1.Version, Binding: binding, RequestedMode: memoryv1.SearchModeKeyword,
		ActualMode: memoryv1.SearchModeKeyword, Records: []memoryv1.MemoryRecord{}, Exhausted: true,
		SnapshotExpiresAt: now.Add(time.Minute),
	}
	for _, tc := range []struct {
		name     string
		request  memoryv1.SearchRequest
		response memoryv1.SearchResponse
	}{
		{
			name: "requested mode mismatch", request: baseRequest,
			response: func() memoryv1.SearchResponse {
				value := baseResponse
				value.RequestedMode = memoryv1.SearchModeAuto
				return value
			}(),
		},
		{
			name: "request page size exceeded", request: baseRequest,
			response: func() memoryv1.SearchResponse {
				value := baseResponse
				value.Records = []memoryv1.MemoryRecord{record("mem-page-1"), record("mem-page-2")}
				return value
			}(),
		},
		{
			name: "binding mismatch", request: baseRequest,
			response: func() memoryv1.SearchResponse { value := baseResponse; value.Binding.RoutingEpoch++; return value }(),
		},
		{
			name:    "continuation token did not advance",
			request: func() memoryv1.SearchRequest { value := baseRequest; value.PageToken = firstToken; return value }(),
			response: func() memoryv1.SearchResponse {
				value := baseResponse
				value.Exhausted = false
				value.NextPageToken = firstToken
				return value
			}(),
		},
		{
			name: "snapshot expiry exceeds advertised TTL", request: baseRequest,
			response: func() memoryv1.SearchResponse {
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
	valid := memoryv1.ErrorResponse{
		ProtocolVersion: memoryv1.Version, Code: memoryv1.ErrorCodeInvalidRequest,
		Message: "invalid request", Retryable: false, RetryAfterSeconds: 0,
	}
	for _, tc := range []struct {
		name       string
		status     int
		response   memoryv1.ErrorResponse
		retryAfter string
		setRetry   bool
		wantErr    bool
	}{
		{name: validTestCase, status: http.StatusBadRequest, response: valid},
		{name: "wrong status", status: http.StatusConflict, response: valid, wantErr: true},
		{name: "wrong code", status: http.StatusBadRequest, response: func() memoryv1.ErrorResponse {
			value := valid
			value.Code = memoryv1.ErrorCodeIdentityConflict
			return value
		}(), wantErr: true},
		{name: "unexpected binding", status: http.StatusBadRequest, response: func() memoryv1.ErrorResponse {
			value := valid
			value.Binding = &binding
			return value
		}(), wantErr: true},
		{name: "retryable", status: http.StatusBadRequest, response: func() memoryv1.ErrorResponse {
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
				context.Background(), client, memoryv1.PathCapabilities, tc.name,
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
	storeBinding := memoryv1.StoreResolutionBinding{
		ClusterID: binding.ClusterID, NamespaceUID: binding.NamespaceUID,
		BackendUID: binding.BackendUID, TenantID: binding.TenantID,
	}
	valid := memoryv1.ErrorResponse{
		ProtocolVersion: memoryv1.Version, Code: memoryv1.ErrorCodeUnauthorized,
		Message: "unauthorized", Retryable: false, RetryAfterSeconds: 0,
	}
	for _, tc := range []struct {
		name     string
		response memoryv1.ErrorResponse
		wantErr  bool
	}{
		{name: validTestCase, response: valid},
		{name: "bound", response: func() memoryv1.ErrorResponse {
			value := valid
			value.Binding = &binding
			return value
		}(), wantErr: true},
		{name: "retryable", response: func() memoryv1.ErrorResponse {
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
		capabilities     memoryv1.Capabilities
		autoMode         string
		rejectAdvertised string
		wantErr          bool
	}{
		{
			name: "semantic and hybrid advertised",
			capabilities: memoryv1.Capabilities{
				KeywordSearch: true, SemanticSearch: true, HybridSearch: true,
			},
			autoMode: memoryv1.SearchModeHybrid,
		},
		{
			name: "semantic and hybrid unadvertised",
			capabilities: memoryv1.Capabilities{
				KeywordSearch: true,
			},
			autoMode: memoryv1.SearchModeKeyword,
		},
		{
			name: "auto selected unadvertised mode",
			capabilities: memoryv1.Capabilities{
				KeywordSearch: true,
			},
			autoMode: memoryv1.SearchModeSemantic,
			wantErr:  true,
		},
		{
			name: "advertised semantic rejected",
			capabilities: memoryv1.Capabilities{
				KeywordSearch: true, SemanticSearch: true,
			},
			autoMode: memoryv1.SearchModeKeyword, rejectAdvertised: memoryv1.SearchModeSemantic,
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expiresAt := time.Now().UTC().Add(time.Minute)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				var searchRequest memoryv1.SearchRequest
				if err := json.NewDecoder(request.Body).Decode(&searchRequest); err != nil {
					writeConformanceJSON(writer, http.StatusBadRequest, memoryv1.ErrorResponse{
						ProtocolVersion: memoryv1.Version, Code: memoryv1.ErrorCodeInvalidRequest,
						Message: "invalid", Retryable: false, RetryAfterSeconds: 0,
					})
					return
				}
				actualMode := searchRequest.Mode
				if searchRequest.Mode == memoryv1.SearchModeAuto {
					actualMode = tc.autoMode
				}
				advertised := searchModeAdvertised(tc.capabilities, searchRequest.Mode)
				if searchRequest.Mode != memoryv1.SearchModeAuto && (!advertised || searchRequest.Mode == tc.rejectAdvertised) {
					responseBinding := binding
					writeConformanceJSON(writer, http.StatusUnprocessableEntity, memoryv1.ErrorResponse{
						ProtocolVersion: memoryv1.Version, Binding: &responseBinding,
						Code: memoryv1.ErrorCodeSearchModeUnsupported, Message: "unsupported",
						Retryable: false, RetryAfterSeconds: 0,
					})
					return
				}
				writeConformanceJSON(writer, http.StatusOK, memoryv1.SearchResponse{
					ProtocolVersion: memoryv1.Version, Binding: binding,
					RequestedMode: searchRequest.Mode, ActualMode: actualMode,
					Records: []memoryv1.MemoryRecord{}, Exhausted: true,
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
	capabilities := memoryv1.Capabilities{
		KeywordSearch: true, SemanticSearch: true, HybridSearch: true,
	}
	expiresAt := time.Now().UTC().Add(time.Minute)
	tokens := map[string]string{
		memoryv1.SearchModeAuto:     "memory-page-v1.11111111111111111111111111111111.1",
		memoryv1.SearchModeSemantic: "memory-page-v1.22222222222222222222222222222222.1",
		memoryv1.SearchModeHybrid:   "memory-page-v1.33333333333333333333333333333333.1",
	}
	var mutex sync.Mutex
	activeSnapshot := false
	continuations := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var searchRequest memoryv1.SearchRequest
		if err := json.NewDecoder(request.Body).Decode(&searchRequest); err != nil {
			writeConformanceJSON(writer, http.StatusBadRequest, memoryv1.ErrorResponse{
				ProtocolVersion: memoryv1.Version, Code: memoryv1.ErrorCodeInvalidRequest,
				Message: "invalid", Retryable: false, RetryAfterSeconds: 0,
			})
			return
		}
		actualMode := searchRequest.Mode
		if actualMode == memoryv1.SearchModeAuto {
			actualMode = memoryv1.SearchModeKeyword
		}
		mutex.Lock()
		defer mutex.Unlock()
		if searchRequest.PageToken == "" {
			if activeSnapshot {
				responseBinding := binding
				writer.Header().Set("Retry-After", "1")
				writeConformanceJSON(writer, http.StatusServiceUnavailable, memoryv1.ErrorResponse{
					ProtocolVersion: memoryv1.Version, Binding: &responseBinding,
					Code: memoryv1.ErrorCodeSnapshotCapacity, Message: "capacity",
					Retryable: true, RetryAfterSeconds: 1,
				})
				return
			}
			activeSnapshot = true
			writeConformanceJSON(writer, http.StatusOK, memoryv1.SearchResponse{
				ProtocolVersion: memoryv1.Version, Binding: binding,
				RequestedMode: searchRequest.Mode, ActualMode: actualMode,
				Records: []memoryv1.MemoryRecord{}, NextPageToken: tokens[searchRequest.Mode],
				Exhausted: false, SnapshotExpiresAt: expiresAt,
			})
			return
		}
		activeSnapshot = false
		continuations++
		writeConformanceJSON(writer, http.StatusOK, memoryv1.SearchResponse{
			ProtocolVersion: memoryv1.Version, Binding: binding,
			RequestedMode: searchRequest.Mode, ActualMode: actualMode,
			Records: []memoryv1.MemoryRecord{}, Exhausted: true,
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

func TestConsumeSearchProbeEnforcesAdvertisedMaxSnapshotRecords(t *testing.T) {
	binding := DefaultBinding()
	expiresAt := time.Now().UTC().Add(time.Minute)
	records := []memoryv1.MemoryRecord{
		conformanceRecord(binding, "mem-limit-a", "limit a", 1),
		conformanceRecord(binding, "mem-limit-b", "limit b", 2),
		conformanceRecord(binding, "mem-limit-c", "limit c", 3),
	}
	var mutex sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		requests++
		mutex.Unlock()
		var searchRequest memoryv1.SearchRequest
		if err := json.NewDecoder(request.Body).Decode(&searchRequest); err != nil {
			writeConformanceJSON(writer, http.StatusBadRequest, memoryv1.ErrorResponse{
				ProtocolVersion: memoryv1.Version, Code: memoryv1.ErrorCodeInvalidRequest,
				Message: "invalid", Retryable: false, RetryAfterSeconds: 0,
			})
			return
		}
		response := memoryv1.SearchResponse{
			ProtocolVersion: memoryv1.Version, Binding: binding,
			RequestedMode: searchRequest.Mode, ActualMode: memoryv1.SearchModeKeyword,
			SnapshotExpiresAt: expiresAt,
		}
		switch searchRequest.PageToken {
		case "":
			response.Records = []memoryv1.MemoryRecord{records[0]}
			response.NextPageToken = conformancePageCursor("0", "1")
		case conformancePageCursor("0", "1"):
			response.Records = []memoryv1.MemoryRecord{records[1]}
			response.NextPageToken = conformancePageCursor("0", "2")
		case conformancePageCursor("0", "2"):
			response.Records = []memoryv1.MemoryRecord{records[2]}
			response.Exhausted = true
		default:
			writeConformanceJSON(writer, http.StatusBadRequest, memoryv1.ErrorResponse{
				ProtocolVersion: memoryv1.Version, Code: memoryv1.ErrorCodeInvalidRequest,
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
	client.configureLimits(limits)
	request, err := requireFixturePlan(t, "snapshot-limit", limits).searchRequest(binding, memoryv1.SearchModeAuto)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := consumeSearchProbe(context.Background(), client, request); err == nil {
		t.Fatal("consumeSearchProbe() accepted more than advertised maxSnapshotRecords")
	} else if !strings.Contains(err.Error(), "maxSnapshotRecords=2") {
		t.Fatalf("consumeSearchProbe() error = %q, want advertised maxSnapshotRecords context", err)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if requests != 3 {
		t.Fatalf("search request count = %d, want 3", requests)
	}
}

func TestCollectContinuationEnforcesAdvertisedMaxSnapshotRecords(t *testing.T) {
	binding := DefaultBinding()
	expiresAt := time.Now().UTC().Add(time.Minute)
	records := []memoryv1.MemoryRecord{
		conformanceRecord(binding, "mem-continuation-a", "continuation a", 1),
		conformanceRecord(binding, "mem-continuation-b", "continuation b", 2),
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var searchRequest memoryv1.SearchRequest
		if err := json.NewDecoder(request.Body).Decode(&searchRequest); err != nil {
			writeConformanceJSON(writer, http.StatusBadRequest, memoryv1.ErrorResponse{
				ProtocolVersion: memoryv1.Version, Code: memoryv1.ErrorCodeInvalidRequest,
				Message: "invalid", Retryable: false, RetryAfterSeconds: 0,
			})
			return
		}
		response := memoryv1.SearchResponse{
			ProtocolVersion: memoryv1.Version, Binding: binding,
			RequestedMode: memoryv1.SearchModeKeyword, ActualMode: memoryv1.SearchModeKeyword,
			SnapshotExpiresAt: expiresAt,
		}
		switch searchRequest.PageToken {
		case conformancePageCursor("1", "1"):
			response.Records = []memoryv1.MemoryRecord{records[0]}
			response.NextPageToken = conformancePageCursor("1", "2")
		case conformancePageCursor("1", "2"):
			response.Records = []memoryv1.MemoryRecord{records[1]}
			response.Exhausted = true
		default:
			writeConformanceJSON(writer, http.StatusBadRequest, memoryv1.ErrorResponse{
				ProtocolVersion: memoryv1.Version, Code: memoryv1.ErrorCodeInvalidRequest,
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
	client.configureLimits(limits)
	request := memoryv1.SearchRequest{
		ProtocolVersion: memoryv1.Version, Binding: binding, Mode: memoryv1.SearchModeKeyword,
		Query: "continuation", PageSize: 1, PageToken: conformancePageCursor("1", "1"),
	}
	if _, err := collectContinuation(context.Background(), client, request, expiresAt, 1); err == nil {
		t.Fatal("collectContinuation() accepted more than advertised maxSnapshotRecords")
	} else if !strings.Contains(err.Error(), "maxSnapshotRecords=2") {
		t.Fatalf("collectContinuation() error = %q, want advertised maxSnapshotRecords context", err)
	}
}

func TestCollectContinuationDoesNotRequestExpiredSnapshot(t *testing.T) {
	var mutex sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		mutex.Lock()
		requests++
		mutex.Unlock()
		writeConformanceJSON(writer, http.StatusInternalServerError, memoryv1.ErrorResponse{
			ProtocolVersion: memoryv1.Version, Code: memoryv1.ErrorCodeInternal,
			Message: "continuation should not be requested", Retryable: false, RetryAfterSeconds: 0,
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
	request := memoryv1.SearchRequest{
		ProtocolVersion: memoryv1.Version, Binding: DefaultBinding(), Mode: memoryv1.SearchModeKeyword,
		Query: "expired", PageSize: 1,
		PageToken: conformancePageCursor("2", "1"),
	}
	_, err = collectContinuation(
		context.Background(), client, request, time.Now().UTC().Add(-time.Second), 1,
	)
	if err == nil {
		t.Fatal("collectContinuation() requested an expired restart continuation")
	}
	if !strings.Contains(err.Error(), "expired before continuation verification") {
		t.Fatalf("collectContinuation() error = %q, want local expiry context", err)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if requests != 0 {
		t.Fatalf("expired continuation request count = %d, want 0", requests)
	}
}

func TestPaginationFixtureSupportsAdvertisedTwoRecordSnapshotAcrossPages(t *testing.T) {
	binding := DefaultBinding()
	expiresAt := time.Now().UTC().Add(time.Minute)
	records := []memoryv1.MemoryRecord{
		conformanceRecord(binding, "mem-page-a", "page a", 1),
		conformanceRecord(binding, "mem-page-b", "page b", 2),
	}
	token := "memory-page-v1.0123456789abcdef0123456789abcdef.1"
	var mutex sync.Mutex
	var requests []memoryv1.SearchRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var searchRequest memoryv1.SearchRequest
		if err := json.NewDecoder(request.Body).Decode(&searchRequest); err != nil {
			writeConformanceJSON(writer, http.StatusBadRequest, memoryv1.ErrorResponse{
				ProtocolVersion: memoryv1.Version, Code: memoryv1.ErrorCodeInvalidRequest,
				Message: "invalid", Retryable: false, RetryAfterSeconds: 0,
			})
			return
		}
		mutex.Lock()
		requests = append(requests, searchRequest)
		mutex.Unlock()
		if searchRequest.PageSize > 1 {
			writeConformanceJSON(writer, http.StatusBadRequest, memoryv1.ErrorResponse{
				ProtocolVersion: memoryv1.Version, Code: memoryv1.ErrorCodeInvalidRequest,
				Message: "pageSize exceeds maxPageSize", Retryable: false, RetryAfterSeconds: 0,
			})
			return
		}

		response := memoryv1.SearchResponse{
			ProtocolVersion: memoryv1.Version, Binding: binding,
			RequestedMode: memoryv1.SearchModeKeyword, ActualMode: memoryv1.SearchModeKeyword,
			SnapshotExpiresAt: expiresAt,
		}
		switch searchRequest.PageToken {
		case "":
			response.Records = []memoryv1.MemoryRecord{records[0]}
			response.NextPageToken = token
			response.Exhausted = false
		case token:
			response.Records = []memoryv1.MemoryRecord{records[1]}
			response.Exhausted = true
		default:
			writeConformanceJSON(writer, http.StatusBadRequest, memoryv1.ErrorResponse{
				ProtocolVersion: memoryv1.Version, Code: memoryv1.ErrorCodeInvalidRequest,
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
	request, err := fixtures.searchRequest(binding, memoryv1.SearchModeKeyword)
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
	records := []memoryv1.MemoryRecord{
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

	duplicate := append([]memoryv1.MemoryRecord(nil), records...)
	duplicate[len(duplicate)-1] = duplicate[0]
	if err := verifySnapshotFixture(binding, duplicate, expected); err == nil {
		t.Fatal("verifySnapshotFixture() accepted a duplicate record")
	}

	changed := append([]memoryv1.MemoryRecord(nil), records...)
	changed[1].Content = "changed proof b"
	changed[1].ContentDigest = memoryv1.ContentDigest(changed[1].Content)
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
		PaginationActualMode: memoryv1.SearchModeKeyword, PaginationSnapshotExpiry: expiresAt,
	}
	continuation := snapshotContinuation{
		Records: records, ActualMode: memoryv1.SearchModeKeyword, SnapshotExpiresAt: expiresAt,
	}
	if err := verifyContinuationProof(binding, continuation, checkpoint); err != nil {
		t.Fatalf("verifyContinuationProof(valid): %v", err)
	}
	continuation.ActualMode = memoryv1.SearchModeSemantic
	if err := verifyContinuationProof(binding, continuation, checkpoint); err == nil {
		t.Fatal("verifyContinuationProof() accepted a changed actual mode")
	}
	continuation.ActualMode = memoryv1.SearchModeKeyword
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
	token := "memory-page-v1.0123456789abcdef0123456789abcdef.1"
	first := &memoryv1.SearchResponse{
		ProtocolVersion: memoryv1.Version, Binding: binding,
		RequestedMode: memoryv1.SearchModeKeyword, ActualMode: memoryv1.SearchModeKeyword,
		Records: []memoryv1.MemoryRecord{record}, NextPageToken: token,
		Exhausted: false, SnapshotExpiresAt: expiresAt,
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeConformanceJSON(writer, http.StatusOK, memoryv1.SearchResponse{
			ProtocolVersion: memoryv1.Version, Binding: binding,
			RequestedMode: memoryv1.SearchModeKeyword, ActualMode: memoryv1.SearchModeKeyword,
			Records: []memoryv1.MemoryRecord{record}, Exhausted: true,
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
	request := memoryv1.SearchRequest{
		ProtocolVersion: memoryv1.Version, Binding: binding, Mode: memoryv1.SearchModeKeyword,
		Query: "duplicate", PageSize: 1,
	}
	if _, err := collectSnapshot(context.Background(), client, request, first); err == nil {
		t.Fatal("collectSnapshot() accepted a duplicate record across pages")
	}
}

func conformancePageCursor(snapshotDigit, offset string) string {
	return "memory-page-v1." + strings.Repeat(snapshotDigit, 32) + "." + offset
}

func conformanceRecord(binding memoryv1.Binding, memoryID, content string, minute int) memoryv1.MemoryRecord {
	return memoryv1.MemoryRecord{
		MemoryID: memoryID, UpsertKey: memoryv1.CanonicalUpsertKey(binding, memoryID),
		State: memoryv1.RecordStateLive, Generation: 1,
		BackendVersion: "version-1", BackendMemoryID: "provider-" + memoryID,
		ContentDigest: memoryv1.ContentDigest(content), Content: content,
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
