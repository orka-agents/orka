package v2

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func testNativeSnapshot(fence Fence) NativeSessionSnapshot {
	data := []byte("private encoded SessionKit bundle")
	return NativeSessionSnapshot{
		Data: data, DataDigest: testSHA256(string(data)), ProviderSessionID: "01970b26-25ad-71ef-bd22-9374ec0b741b",
		ProviderKind: "codex", ProviderVersion: "0.160.0", RuntimeSessionUID: fence.RuntimeSessionUID,
		RuntimeProfileDigest: fence.RuntimeProfileDigest, WorkingDirectory: "/source/work",
	}
}

func TestNativeRestoreRequiresCurrentFenceAndExcludesBootstrap(t *testing.T) {
	request := clientTestCreateSessionRequest(t, testNow, "native-create")
	request.NativeRestore = &NativeSessionRestore{Snapshot: testNativeSnapshot(request.Metadata.Fence)}
	sealRequest(t, request, &request.Metadata.RequestDigest)
	if err := request.ValidateAt(testNow); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*CreateRuntimeSessionRequest){
		func(r *CreateRuntimeSessionRequest) { r.NativeRestore.Snapshot.RuntimeSessionUID = "foreign-owner" },
		func(r *CreateRuntimeSessionRequest) {
			r.NativeRestore.Snapshot.RuntimeProfileDigest = ProfileDigest(testSHA256("old-policy"))
		},
		func(r *CreateRuntimeSessionRequest) { r.NativeRestore.Snapshot.ProviderKind = "claude" },
		func(r *CreateRuntimeSessionRequest) { r.NativeRestore.Snapshot.Data = []byte("tampered") },
		func(r *CreateRuntimeSessionRequest) {
			r.NativeRestore.Snapshot.ProviderSessionID = "provider-opaque-id"
		},
		func(r *CreateRuntimeSessionRequest) { r.Bootstrap = &SessionBootstrap{} },
	} {
		changed := request
		restore := *request.NativeRestore
		changed.NativeRestore = &restore
		mutate(&changed)
		sealRequest(t, changed, &changed.Metadata.RequestDigest)
		if err := changed.ValidateAt(testNow); err == nil {
			t.Fatal("unsafe native restore validated")
		}
	}
	oversized := testNativeSnapshot(request.Metadata.Fence)
	oversized.Data = make([]byte, MaxNativeSessionBytes+1)
	if err := oversized.Validate(); err == nil {
		t.Fatal("oversized native restore validated")
	}
	oversizedRequest := request
	oversizedRequest.NativeRestore = &NativeSessionRestore{Snapshot: oversized}
	if _, err := CanonicalRequestDigest(oversizedRequest); err == nil {
		t.Fatal("oversized native restore digest accepted")
	}
	changed := request
	restore := *request.NativeRestore
	changed.NativeRestore = &restore
	changed.NativeRestore.Snapshot.Data = []byte("different valid bundle")
	changed.NativeRestore.Snapshot.DataDigest = testSHA256(string(changed.NativeRestore.Snapshot.Data))
	if err := changed.ValidateAt(testNow); err == nil {
		t.Fatal("native data was not bound by the create digest")
	}
}

func TestClientCaptureNativeSessionAuthenticatesAndValidatesOwner(t *testing.T) {
	now := time.Now().UTC()
	create := clientTestCreateSessionRequest(t, now, "create")
	request := CaptureNativeSessionRequest{Protocol: ProtocolVersion, Metadata: clientTestMetadata(t, now, "capture", false)}
	sealRequest(t, request, &request.Metadata.RequestDigest)
	response := CaptureNativeSessionResponse{Protocol: ProtocolVersion, Classification: Classification{Class: RequestClassificationFresh},
		Session: clientTestCreateSessionResponse(create, now, RequestClassificationFresh).Session, Snapshot: testNativeSnapshot(request.Metadata.Fence)}
	response.Session.ProviderSessionID = response.Snapshot.ProviderSessionID
	response.Session.State = RuntimeSessionStatePoisoned
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost || r.URL.Path != "/v2/runtime-sessions/runtime-session-1/native-session" {
			t.Errorf("unexpected native capture route %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer "+clientTestBearer {
			t.Error("native capture omitted controller authentication")
		}
		if err := VerifyOperationCapability(clientTestCapabilitySecret, r.Header.Get(OperationCapabilityHeader), request.Metadata, true, time.Now().UTC()); err != nil {
			t.Errorf("native capture authorization: %v", err)
		}
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, WithControllerBearerToken(clientTestBearer), WithOperationCapabilitySecret(clientTestCapabilitySecret))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.CaptureNativeSession(context.Background(), create.RuntimeSessionID, request); err != nil {
		t.Fatal(err)
	}
	response.Snapshot.RuntimeSessionUID = "foreign-owner"
	if _, err := client.CaptureNativeSession(context.Background(), create.RuntimeSessionID, request); err == nil {
		t.Fatal("native capture accepted a foreign snapshot owner")
	}
}
