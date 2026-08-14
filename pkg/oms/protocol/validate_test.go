package protocol

import (
	"bytes"
	"encoding/json"
	"maps"
	"strings"
	"testing"
	"time"
)

func TestPrepareMutationCanonicalizesAndBindsDigests(t *testing.T) {
	binding := testBinding()
	request := MutationEnvelope{
		ProtocolVersion: Version, OperationID: "mop-canonical-1", Binding: binding,
		MemoryID: "mem-canonical-1", Kind: MutationKindCreate, Generation: 1,
		State: &MutationState{
			Content: "Line one\r\nLine two", Tags: []string{" Beta ", "alpha", "ALPHA"},
			Metadata: map[string]string{" Owner ": " Orka ", "zone": "west"},
		},
	}
	if err := PrepareMutation(&request); err != nil {
		t.Fatalf("PrepareMutation() error = %v", err)
	}
	if got, want := request.Binding.TenantID, DeriveTenantID(binding.ClusterID, binding.NamespaceUID); got != want {
		t.Fatalf("tenantId = %q, want %q", got, want)
	}
	if got, want := request.UpsertKey, CanonicalUpsertKey(binding, request.MemoryID); got != want {
		t.Fatalf("upsertKey = %q, want %q", got, want)
	}
	if got, want := request.State.Tags, []string{"alpha", "beta"}; !slicesEqual(got, want) {
		t.Fatalf("tags = %#v, want %#v", got, want)
	}
	if got := request.State.Metadata["owner"]; got != "Orka" {
		t.Fatalf("metadata owner = %q", got)
	}
	if request.ContentDigest != ContentDigest("Line one\r\nLine two") {
		t.Fatal("content digest did not preserve exact line-ending bytes")
	}
	if err := ValidateMutationEnvelope(&request); err != nil {
		t.Fatalf("ValidateMutationEnvelope() error = %v", err)
	}

	changed := request
	changed.State = cloneStateForTest(request.State)
	changed.State.Content = strings.ReplaceAll(changed.State.Content, "\r\n", "\n")
	changed.ContentDigest = ContentDigest(changed.State.Content)
	if digest, err := MutationDigest(&changed); err != nil || digest == request.MutationDigest {
		t.Fatalf("line-ending change did not change mutation digest: digest=%q err=%v", digest, err)
	}
}

func TestDecodeMutationEnvelopeRejectsClosedProfileViolations(t *testing.T) {
	request := testMutation(t)
	valid, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	atLimit := append(append([]byte(nil), valid...), bytes.Repeat([]byte(" "), MaxHTTPBodyBytes-len(valid))...)
	if _, err := DecodeMutationEnvelope(atLimit); err != nil {
		t.Fatalf("DecodeMutationEnvelope() rejected exact %d-byte request: %v", MaxHTTPBodyBytes, err)
	}
	tests := map[string][]byte{
		"unknown field":      append([]byte(`{"extra":true,`), valid[1:]...),
		"duplicate field":    append([]byte(`{"protocolVersion":"orka.oms.v0alpha1",`), valid[1:]...),
		"trailing JSON":      append(append([]byte(nil), valid...), []byte(` {}`)...),
		"oversized":          append(append([]byte(nil), atLimit...), ' '),
		"invalid UTF-8":      append(append([]byte(nil), valid[:len(valid)-1]...), 0xff, '}'),
		"unpaired surrogate": bytes.Replace(valid, []byte(`"hello"`), []byte(`"\ud800"`), 1),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeMutationEnvelope(body); err == nil {
				t.Fatalf("DecodeMutationEnvelope() accepted %s", name)
			}
		})
	}

	var object map[string]any
	if err := json.Unmarshal(valid, &object); err != nil {
		t.Fatal(err)
	}
	delete(object, "state")
	missingState, _ := json.Marshal(object)
	if _, err := DecodeMutationEnvelope(missingState); err == nil {
		t.Fatal("DecodeMutationEnvelope() accepted a missing state field")
	}
}

func TestValidateJSONUnicodeEscapesAdvancesAcrossEscapedDelimiters(t *testing.T) {
	valid := map[string][]byte{
		"escaped quote before pair":     []byte(`{"value":"quoted: \" then pair: \ud83d\ude00"}`),
		"escaped backslash before pair": []byte(`{"value":"slash: \\ then pair: \ud83d\ude00"}`),
		"escaped literal surrogate":     []byte(`{"value":"literal: \\ud800"}`),
	}
	for name, body := range valid {
		t.Run(name, func(t *testing.T) {
			if !json.Valid(body) {
				t.Fatalf("test body is not valid JSON: %s", body)
			}
			if err := validateJSONUnicodeEscapes(body); err != nil {
				t.Fatalf("validateJSONUnicodeEscapes() error = %v", err)
			}
		})
	}

	invalid := map[string][]byte{
		"escaped quote before high surrogate":     []byte(`{"value":"quoted: \" then bad: \ud800"}`),
		"escaped backslash before low surrogate":  []byte(`{"value":"slash: \\ then bad: \udc00"}`),
		"high surrogate before escaped delimiter": []byte(`{"value":"\ud83d\""}`),
	}
	for name, body := range invalid {
		t.Run(name, func(t *testing.T) {
			if err := validateJSONUnicodeEscapes(body); err == nil {
				t.Fatalf("validateJSONUnicodeEscapes() accepted %s", body)
			}
		})
	}
}

func TestValidateMutationEnvelopeRejectsDigestAndCanonicalizationTampering(t *testing.T) {
	base := testMutation(t)
	tests := map[string]func(*MutationEnvelope){
		"content":        func(value *MutationEnvelope) { value.State.Content += "tampered" },
		"content digest": func(value *MutationEnvelope) { value.ContentDigest = EmptyContentDigest() },
		"mutation digest": func(value *MutationEnvelope) {
			value.MutationDigest = strings.Repeat("a", len(value.MutationDigest))
		},
		"tag order":    func(value *MutationEnvelope) { value.State.Tags = []string{"z", "a"} },
		"metadata key": func(value *MutationEnvelope) { value.State.Metadata = map[string]string{" Owner ": "value"} },
		"upsert key":   func(value *MutationEnvelope) { value.UpsertKey += "-other" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			value := base
			value.State = cloneStateForTest(base.State)
			mutate(&value)
			if err := ValidateMutationEnvelope(&value); err == nil {
				t.Fatalf("ValidateMutationEnvelope() accepted %s tampering", name)
			}
		})
	}
}

func TestContentAndBodyBoundsMatchNormativeHardCaps(t *testing.T) {
	if MaxContentBytes != 256<<10 {
		t.Fatalf("MaxContentBytes = %d, want exactly 256 KiB", MaxContentBytes)
	}
	if MaxHTTPBodyBytes != 2<<20 {
		t.Fatalf("MaxHTTPBodyBytes = %d, want exactly 2 MiB", MaxHTTPBodyBytes)
	}
	if MaxAdapterResponseBytes != 4<<20 {
		t.Fatalf("MaxAdapterResponseBytes = %d, want exactly 4 MiB", MaxAdapterResponseBytes)
	}
	request := MutationEnvelope{
		ProtocolVersion: Version, OperationID: "mop-max-content", Binding: testBinding(),
		MemoryID: "mem-max-content", Kind: MutationKindCreate, Generation: 1,
		State: &MutationState{
			Content: strings.Repeat("\n", MaxContentBytes), Tags: []string{}, Metadata: map[string]string{},
		},
	}
	if err := PrepareMutation(&request); err != nil {
		t.Fatalf("PrepareMutation(max content): %v", err)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > MaxHTTPBodyBytes {
		t.Fatalf("max-content mutation encoded to %d bytes, exceeds request cap %d", len(encoded), MaxHTTPBodyBytes)
	}
	request.State.Content += "x"
	request.MutationDigest = ""
	if err := PrepareMutation(&request); err == nil {
		t.Fatal("content above the 256 KiB hard cap was accepted")
	}
}

func TestStoreResolutionUsesPreAuthorityIdentityAndCanonicalUUID(t *testing.T) {
	binding := testBinding()
	preAuthority := StoreResolutionBinding{
		ClusterID: binding.ClusterID, NamespaceUID: binding.NamespaceUID,
		BackendUID: binding.BackendUID, TenantID: binding.TenantID,
	}
	request := StoreResolveRequest{ProtocolVersion: Version, Binding: preAuthority, StoreName: "primary.store"}
	data, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeStoreResolveRequest(data)
	if err != nil {
		t.Fatalf("DecodeStoreResolveRequest(): %v", err)
	}
	if !StoreResolutionBindingEqual(decoded.Binding, preAuthority) {
		t.Fatal("pre-authority binding changed during decode")
	}
	response := StoreResolveResponse{
		ProtocolVersion: Version, Binding: preAuthority, StoreName: request.StoreName,
		StoreUUID: "d4a8d58c-90c4-4e80-8d4e-a1fd5ca8e310",
	}
	responseData, _ := json.Marshal(response)
	if _, err := DecodeStoreResolveResponse(responseData); err != nil {
		t.Fatalf("DecodeStoreResolveResponse(): %v", err)
	}
	response.StoreUUID = "not-a-uuid"
	if err := ValidateStoreResolveResponse(&response); err == nil {
		t.Fatal("non-UUID store identity was accepted")
	}
}

func TestCapabilitiesRequireAllWritableSemanticsAndFiniteLimits(t *testing.T) {
	response := CapabilitiesResponse{
		ProtocolVersion: Version, Binding: testBinding(), AdapterName: "adapter", AdapterVersion: "v1",
		Revision: "revision-1", ExpiresAt: time.Now().Add(time.Minute),
		Capabilities: Capabilities{
			DurableIdempotency: true, IdempotencyDigestConflicts: true, CreateIfAbsent: true,
			ConditionalMutation: true, MonotonicGenerations: true, DeleteHighWatermark: true,
			DurableRoutingFence: true, OperationLookup: true, ExactGet: true, StablePagination: true,
			ExclusiveOwnership: true, KeywordSearch: true, AuditVersionVisibility: true,
		},
		Limits: CapabilityLimits{
			MaxRequestBytes: MaxHTTPBodyBytes, MaxResponseBytes: MaxAdapterResponseBytes,
			MaxContentBytes: MaxContentBytes, MaxTags: MaxTags, MaxTagBytes: MaxTagBytes,
			MaxMetadataEntries: MaxMetadataEntries, MaxMetadataKeyBytes: MaxMetadataKeyBytes,
			MaxMetadataValueBytes: MaxMetadataValueBytes, MaxQueryBytes: MaxQueryBytes,
			MaxPageSize: MaxPageSize, MaxSnapshotRecords: MaxSnapshotRecords, SnapshotTTLSeconds: 60,
		},
	}
	if err := ValidateCapabilitiesResponse(&response, time.Now()); err != nil {
		t.Fatalf("valid capabilities: %v", err)
	}
	response.Limits.MaxRequestBytes = MaxHTTPBodyBytes + 1
	if err := ValidateCapabilitiesResponse(&response, time.Now()); err == nil {
		t.Fatal("capabilities above the 2 MiB request hard cap were accepted")
	}
	response.Limits.MaxRequestBytes = MaxHTTPBodyBytes
	response.Limits.SnapshotTTLSeconds = MaxSnapshotTTLSeconds + 1
	if err := ValidateCapabilitiesResponse(&response, time.Now()); err == nil {
		t.Fatal("capabilities above the snapshot TTL hard cap were accepted")
	}
	response.Limits.SnapshotTTLSeconds = 60
	response.Capabilities.DeleteHighWatermark = false
	if err := ValidateCapabilitiesResponse(&response, time.Now()); err == nil {
		t.Fatal("capabilities without delete high-watermark were accepted")
	}
}

func TestSearchResponseRequiresFutureSnapshotForContinuation(t *testing.T) {
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		exhausted bool
		offset    time.Duration
		wantErr   bool
	}{
		{name: "expired continuation", offset: -time.Nanosecond, wantErr: true},
		{name: "expiry equal to validation time", offset: 0, wantErr: true},
		{name: "strictly future continuation", offset: time.Nanosecond},
		{name: "terminal page may report elapsed snapshot", exhausted: true, offset: -time.Minute},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := validSearchResponseForTest(now, test.exhausted)
			response.SnapshotExpiresAt = now.Add(test.offset)
			err := validateSearchResponseAt(&response, now)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateSearchResponseAt() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestDecodeSearchResponseRejectsExpiredContinuationSnapshot(t *testing.T) {
	now := time.Now().UTC()
	response := validSearchResponseForTest(now, false)
	response.SnapshotExpiresAt = now.Add(-time.Second)
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeSearchResponse(body); err == nil {
		t.Fatal("DecodeSearchResponse() accepted an expired continuation snapshot")
	}

	response.SnapshotExpiresAt = time.Now().UTC().Add(time.Hour)
	body, err = json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeSearchResponse(body); err != nil {
		t.Fatalf("DecodeSearchResponse() rejected a future continuation snapshot: %v", err)
	}
}

func TestSearchResponseContinuationExpiresAtCapsLocalDeadline(t *testing.T) {
	now := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	response := validSearchResponseForTest(now, false)
	response.SnapshotExpiresAt = now.Add(2 * time.Minute)

	tests := []struct {
		name  string
		local time.Time
		want  time.Time
	}{
		{name: "provider only", want: response.SnapshotExpiresAt},
		{name: "shorter local deadline", local: now.Add(time.Minute), want: now.Add(time.Minute)},
		{name: "equal deadline", local: response.SnapshotExpiresAt, want: response.SnapshotExpiresAt},
		{name: "provider caps longer deadline", local: now.Add(5 * time.Minute), want: response.SnapshotExpiresAt},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := response.ContinuationExpiresAt(test.local)
			if !ok {
				t.Fatal("ContinuationExpiresAt() did not expose a nonterminal continuation")
			}
			if !got.Equal(test.want) {
				t.Fatalf("ContinuationExpiresAt() = %v, want %v", got, test.want)
			}
		})
	}

	response.Exhausted = true
	response.NextPageToken = ""
	if expiry, ok := response.ContinuationExpiresAt(now.Add(time.Minute)); ok || !expiry.IsZero() {
		t.Fatalf("ContinuationExpiresAt() exposed terminal response expiry %v", expiry)
	}
}

func TestSearchResponseRejectsNonzeroKeywordScore(t *testing.T) {
	binding := testBinding()
	content := "keyword result"
	response := SearchResponse{
		ProtocolVersion: Version, Binding: binding, RequestedMode: SearchModeKeyword,
		ActualMode: SearchModeKeyword, Records: []MemoryRecord{{
			MemoryID: "mem-keyword-1", UpsertKey: CanonicalUpsertKey(binding, "mem-keyword-1"),
			State: RecordStateLive, Generation: 1, BackendVersion: "version-1",
			BackendMemoryID: "provider-1", ContentDigest: ContentDigest(content), Content: content,
			Tags: []string{}, Metadata: map[string]string{}, UpdatedAt: time.Now().UTC(), Score: 0.5,
		}}, Exhausted: true, SnapshotExpiresAt: time.Now().UTC().Add(time.Minute),
	}
	if err := ValidateSearchResponse(&response); err == nil {
		t.Fatal("keyword search response with nonzero score was accepted")
	}
	response.Records[0].Score = 0
	if err := ValidateSearchResponse(&response); err != nil {
		t.Fatalf("zero-score keyword response: %v", err)
	}
}

func TestValidateErrorResponseUsesClosedProfileCodes(t *testing.T) {
	closedCodes := []string{
		ErrorCodeUnauthorized,
		ErrorCodeInvalidRequest,
		ErrorCodeMethodNotAllowed,
		ErrorCodeNotFound,
		ErrorCodeInternal,
		ErrorCodeResponseTooLarge,
		ErrorCodeSearchModeUnsupported,
		ErrorCodeIdentityConflict,
		ErrorCodeRoutingFenced,
		ErrorCodePageTokenInvalid,
		ErrorCodePageTokenExpired,
		ErrorCodeSnapshotCapacity,
	}
	for _, code := range closedCodes {
		t.Run(code, func(t *testing.T) {
			response := ErrorResponse{
				ProtocolVersion: Version, Code: code, Message: "bounded error",
				Retryable: false, RetryAfterSeconds: 0,
			}
			if err := ValidateErrorResponse(&response); err != nil {
				t.Fatalf("ValidateErrorResponse(%s): %v", code, err)
			}
		})
	}

	unknown := ErrorResponse{
		ProtocolVersion: Version, Code: "OMS_FUTURE_ERROR", Message: "bounded error",
		Retryable: false, RetryAfterSeconds: 0,
	}
	if err := ValidateErrorResponse(&unknown); err == nil {
		t.Fatal("ValidateErrorResponse() accepted an unknown profile error code")
	}
	body, err := EncodeJSON(unknown)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeErrorResponse(body); err == nil {
		t.Fatal("DecodeErrorResponse() accepted an unknown profile error code")
	}
}

func TestMemoryRecordDigestUsesCanonicalCompleteRecord(t *testing.T) {
	binding := testBinding()
	content := "canonical record"
	record := MemoryRecord{
		MemoryID: "mem-record-digest", UpsertKey: CanonicalUpsertKey(binding, "mem-record-digest"),
		State: RecordStateLive, Generation: 2, BackendVersion: "version-2", BackendMemoryID: "provider-2",
		ContentDigest: ContentDigest(content), Content: content, Tags: []string{"alpha", "beta"},
		Metadata:  map[string]string{"owner": "Orka", "zone": "west"},
		UpdatedAt: time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC),
	}
	digest, err := MemoryRecordDigest(&record, binding)
	if err != nil {
		t.Fatal(err)
	}
	reordered := record
	reordered.Metadata = map[string]string{"zone": "west", "owner": "Orka"}
	reorderedDigest, err := MemoryRecordDigest(&reordered, binding)
	if err != nil {
		t.Fatal(err)
	}
	if reorderedDigest != digest {
		t.Fatal("MemoryRecordDigest() changed when only map insertion order changed")
	}

	changed := record
	changed.BackendVersion = "version-3"
	changedDigest, err := MemoryRecordDigest(&changed, binding)
	if err != nil {
		t.Fatal(err)
	}
	if changedDigest == digest {
		t.Fatal("MemoryRecordDigest() ignored a changed record field")
	}
}

func TestSearchResponseRequiresExplicitExhaustionMarker(t *testing.T) {
	response := SearchResponse{
		ProtocolVersion: Version, Binding: testBinding(), RequestedMode: SearchModeAuto,
		ActualMode: SearchModeKeyword, Records: []MemoryRecord{}, NextPageToken: "",
		Exhausted: false, SnapshotExpiresAt: time.Now().Add(time.Minute),
	}
	if err := ValidateSearchResponse(&response); err == nil {
		t.Fatal("search response with inconsistent exhaustion marker was accepted")
	}
	response.Exhausted = true
	if err := ValidateSearchResponse(&response); err != nil {
		t.Fatalf("valid exhausted search response: %v", err)
	}
}

func validSearchResponseForTest(now time.Time, exhausted bool) SearchResponse {
	nextPageToken := ""
	if !exhausted {
		nextPageToken = "oms-page-v1." + strings.Repeat("a", 32) + ".1"
	}
	return SearchResponse{
		ProtocolVersion: Version, Binding: testBinding(), RequestedMode: SearchModeAuto,
		ActualMode: SearchModeKeyword, Records: []MemoryRecord{}, NextPageToken: nextPageToken,
		Exhausted: exhausted, SnapshotExpiresAt: now.Add(time.Minute),
	}
}

func testBinding() Binding {
	binding := Binding{
		ClusterID: "cluster-1", NamespaceUID: "namespace-uid-1", BackendUID: "backend-uid-1",
		AuthorityEpoch: 3, RoutingEpoch: 7, StoreUUID: "11111111-1111-4111-8111-111111111111",
	}
	binding.TenantID = DeriveTenantID(binding.ClusterID, binding.NamespaceUID)
	return binding
}

func testMutation(t *testing.T) MutationEnvelope {
	t.Helper()
	request := MutationEnvelope{
		ProtocolVersion: Version, OperationID: "mop-test-1", Binding: testBinding(), MemoryID: "mem-test-1",
		Kind: MutationKindCreate, Generation: 1,
		State: &MutationState{Content: "hello", Tags: []string{"tag"}, Metadata: map[string]string{"owner": "orka"}},
	}
	if err := PrepareMutation(&request); err != nil {
		t.Fatalf("PrepareMutation(): %v", err)
	}
	return request
}

func cloneStateForTest(state *MutationState) *MutationState {
	metadata := make(map[string]string, len(state.Metadata))
	maps.Copy(metadata, state.Metadata)
	return &MutationState{Content: state.Content, Tags: append([]string(nil), state.Tags...), Metadata: metadata}
}

func slicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func TestDecodeSearchResponseClosesEveryRecordAndBoundsNextPageToken(t *testing.T) {
	binding := testBinding()
	now := time.Now().UTC()
	record := func(id string) MemoryRecord {
		content := "content " + id
		return MemoryRecord{
			MemoryID: id, UpsertKey: CanonicalUpsertKey(binding, id), State: RecordStateLive,
			Generation: 1, BackendVersion: "version-1", BackendMemoryID: "provider-" + id,
			ContentDigest: ContentDigest(content), Content: content,
			Tags: []string{}, Metadata: map[string]string{}, UpdatedAt: now,
		}
	}
	response := SearchResponse{
		ProtocolVersion: Version, Binding: binding, RequestedMode: SearchModeKeyword, ActualMode: SearchModeKeyword,
		Records: []MemoryRecord{record("mem-closed-1"), record("mem-closed-2")}, NextPageToken: "next", Exhausted: false,
		SnapshotExpiresAt: now.Add(time.Minute),
	}
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	records := wire["records"].([]any)
	records[1].(map[string]any)["unexpected"] = true
	tampered, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeSearchResponse(tampered); err == nil {
		t.Fatal("DecodeSearchResponse() accepted an unknown field in a later record")
	}

	response.NextPageToken = strings.Repeat("a", MaxPageTokenBytes+1)
	if err := ValidateSearchResponse(&response); err == nil {
		t.Fatal("ValidateSearchResponse() accepted an oversized nextPageToken")
	}
}

func TestEqualStringMapRequiresIdenticalKeyPresence(t *testing.T) {
	if equalStringMap(map[string]string{"left": ""}, map[string]string{"right": ""}) {
		t.Fatal("equalStringMap() treated different empty-valued keys as equal")
	}
}

func TestValidateRoutingFenceResponseBindsMaximumToRequestedEpoch(t *testing.T) {
	binding := testBinding()
	binding.RoutingEpoch = 7
	base := RoutingFenceResponse{
		ProtocolVersion: Version, Binding: binding, BindingDigest: BindingDigest(binding), CompletedAt: time.Now().UTC(),
	}
	for _, tc := range []struct {
		name    string
		result  string
		maximum uint64
		wantErr bool
	}{
		{name: "applied below request", result: ResultApplied, maximum: 6, wantErr: true},
		{name: "applied at request", result: ResultApplied, maximum: 7},
		{name: "applied above request", result: ResultApplied, maximum: 8},
		{name: "precondition at request", result: ResultPreconditionFailed, maximum: 7, wantErr: true},
		{name: "precondition newer", result: ResultPreconditionFailed, maximum: 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := base
			response.Result = tc.result
			response.MaximumRoutingEpoch = tc.maximum
			err := ValidateRoutingFenceResponse(&response)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateRoutingFenceResponse() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestClosedJSONRejectsNullForNonNullableTypedValues(t *testing.T) {
	binding := testBinding()
	mutation := MutationEnvelope{
		ProtocolVersion: Version, OperationID: "mop-nullability-1", Binding: binding,
		MemoryID: "mem-nullability-1", Kind: MutationKindCreate, Generation: 1,
		State: &MutationState{Content: "content", Tags: []string{"tag"}, Metadata: map[string]string{"owner": "orka"}},
	}
	if err := PrepareMutation(&mutation); err != nil {
		t.Fatal(err)
	}
	mutationWire := func() map[string]any {
		body, err := EncodeJSON(mutation)
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]any
		if err := json.Unmarshal(body, &wire); err != nil {
			t.Fatal(err)
		}
		return wire
	}
	assertNullError := func(name string, wire any, decode func([]byte) error) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			body, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			if err := decode(body); err == nil || !strings.Contains(err.Error(), "must not be null") {
				t.Fatalf("decode error = %v, want explicit nullability rejection", err)
			}
		})
	}

	wire := mutationWire()
	wire["expectedBackendVersion"] = nil
	assertNullError("scalar", wire, func(body []byte) error { _, err := DecodeMutationEnvelope(body); return err })

	wire = mutationWire()
	wire["state"].(map[string]any)["tags"] = []any{nil}
	assertNullError("array item", wire, func(body []byte) error { _, err := DecodeMutationEnvelope(body); return err })

	wire = mutationWire()
	wire["state"].(map[string]any)["metadata"] = map[string]any{"owner": nil}
	assertNullError("map value", wire, func(body []byte) error { _, err := DecodeMutationEnvelope(body); return err })

	search := SearchRequest{
		ProtocolVersion: Version, Binding: binding, Mode: SearchModeKeyword, Query: "q", PageSize: 1, PageToken: "",
	}
	body, err := EncodeJSON(search)
	if err != nil {
		t.Fatal(err)
	}
	var searchWire map[string]any
	if err := json.Unmarshal(body, &searchWire); err != nil {
		t.Fatal(err)
	}
	searchWire["pageToken"] = nil
	assertNullError("optional-looking required scalar", searchWire, func(body []byte) error {
		_, err := DecodeSearchRequest(body)
		return err
	})

	deleteMutation := mutation
	deleteMutation.OperationID = "mop-nullability-delete"
	deleteMutation.Kind = MutationKindDelete
	deleteMutation.Generation = 1
	deleteMutation.ExpectedGeneration = 0
	deleteMutation.ExpectedBackendVersion = ""
	deleteMutation.State = nil
	if err := PrepareMutation(&deleteMutation); err != nil {
		t.Fatal(err)
	}
	deleteBody, err := EncodeJSON(deleteMutation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeMutationEnvelope(deleteBody); err != nil {
		t.Fatalf("nullable delete state rejected: %v", err)
	}
}

func TestMaximumContentFitsBoundedOMSRequestEncoding(t *testing.T) {
	mutation := MutationEnvelope{
		ProtocolVersion: Version, OperationID: "mop-max-html-escaped-content", Binding: testBinding(),
		MemoryID: "mem-max-html-escaped-content", Kind: MutationKindCreate, Generation: 1,
		State: &MutationState{Content: strings.Repeat("<", MaxContentBytes), Tags: []string{}, Metadata: map[string]string{}},
	}
	if err := PrepareMutation(&mutation); err != nil {
		t.Fatal(err)
	}
	htmlEscaped, err := json.Marshal(mutation)
	if err != nil {
		t.Fatal(err)
	}
	if len(htmlEscaped) <= 1<<20 || len(htmlEscaped) > MaxHTTPBodyBytes {
		t.Fatalf("encoding/json request bytes = %d, want > 1 MiB and <= %d", len(htmlEscaped), MaxHTTPBodyBytes)
	}
	if _, err := DecodeMutationEnvelope(htmlEscaped); err != nil {
		t.Fatalf("DecodeMutationEnvelope(max HTML-escaped content): %v", err)
	}
	canonical, err := EncodeJSON(mutation)
	if err != nil {
		t.Fatal(err)
	}
	if len(canonical) >= len(htmlEscaped) || len(canonical) > MaxHTTPBodyBytes {
		t.Fatalf(
			"canonical request bytes = %d, HTML-escaped = %d, limit = %d",
			len(canonical),
			len(htmlEscaped),
			MaxHTTPBodyBytes,
		)
	}
	if _, err := DecodeMutationEnvelope(canonical); err != nil {
		t.Fatalf("DecodeMutationEnvelope(max canonical content): %v", err)
	}
}

func TestClosedJSONRejectsCaseFoldedAliasesAtEveryObjectLevel(t *testing.T) {
	binding := testBinding()
	health := []byte(`{"protocolVersion":"` + Version + `","ProtocolVersion":"` + Version + `","status":"ok"}`)
	if _, err := DecodeHealthResponse(health); err == nil {
		t.Fatal("DecodeHealthResponse() accepted a case-folded top-level alias")
	}

	request := CapabilitiesRequest{ProtocolVersion: Version, Binding: binding}
	body, err := EncodeJSON(request)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	wire["binding"].(map[string]any)["ClusterId"] = binding.ClusterID
	tampered, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCapabilitiesRequest(tampered); err == nil {
		t.Fatal("DecodeCapabilitiesRequest() accepted a case-folded nested alias")
	}
}

func TestValidateMutationReceiptEnforcesResultSpecificIdentity(t *testing.T) {
	binding := testBinding()
	base := MutationReceipt{
		ProtocolVersion: Version, Binding: binding, OperationID: "mop-receipt-variant-1",
		BindingDigest: BindingDigest(binding), ContentDigest: ContentDigest("content"),
		MutationDigest: ContentDigest("mutation"), CompletedAt: time.Now().UTC(),
	}
	for _, result := range []string{
		ResultPreconditionFailed, ResultIdempotencyConflict, ResultIdentityConflict,
		ResultRetryableError, ResultNonRetryableError,
	} {
		t.Run(result, func(t *testing.T) {
			receipt := base
			receipt.Result = result
			if err := ValidateMutationReceipt(&receipt); err != nil {
				t.Fatalf("zero-identity %s receipt rejected: %v", result, err)
			}
			receipt.AppliedGeneration = 9
			receipt.BackendVersion = "version-9"
			receipt.BackendMemoryID = "provider-9"
			if err := ValidateMutationReceipt(&receipt); err == nil {
				t.Fatalf("%s receipt with durable identity was accepted", result)
			}
		})
	}

	notFound := base
	notFound.Result = ResultNotFound
	notFound.AppliedGeneration = 2
	notFound.BackendVersion = "version-delete-2"
	notFound.BackendMemoryID = "provider-delete-2"
	if err := ValidateMutationReceipt(&notFound); err == nil {
		t.Fatal("notFound receipt with a non-empty content digest was accepted")
	}
	notFound.ContentDigest = EmptyContentDigest()
	if err := ValidateMutationReceipt(&notFound); err != nil {
		t.Fatalf("valid notFound receipt rejected: %v", err)
	}
}
