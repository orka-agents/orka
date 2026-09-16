package v2

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNativeSessionRestoreRequestIntegrity(t *testing.T) {
	request := nativeSessionTestCreateRequest(t)
	wire, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var decoded CreateRuntimeSessionRequest
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.ValidateAt(testNow); err != nil {
		t.Fatalf("restoration request did not survive JSON round trip: %v", err)
	}

	for name, mutate := range map[string]func(*CreateRuntimeSessionRequest){
		"snapshot replacement": func(r *CreateRuntimeSessionRequest) { r.NativeRestore.SnapshotID = "other-snapshot" },
		"conversation replacement": func(r *CreateRuntimeSessionRequest) {
			r.NativeRestore.Snapshot.ProviderSessionID = "other-conversation"
		},
		"payload replacement with valid data digest": func(r *CreateRuntimeSessionRequest) {
			r.NativeRestore.Snapshot.Data = []byte("replacement provider state")
			r.NativeRestore.Snapshot.DataDigest = NativeSessionDataDigest(r.NativeRestore.Snapshot.Data)
		},
		"restore removed": func(r *CreateRuntimeSessionRequest) { r.NativeRestore = nil },
	} {
		t.Run(name, func(t *testing.T) {
			r := nativeSessionTestCreateRequest(t)
			mutate(&r)
			if err := r.ValidateAt(testNow); err == nil || !strings.Contains(err.Error(), "request digest mismatch") {
				t.Fatalf("mutated sealed request validation = %v, want request digest mismatch", err)
			}
		})
	}
}

func TestNativeSessionRestoreRequestRestrictions(t *testing.T) {
	for name, mutate := range map[string]func(*CreateRuntimeSessionRequest){
		"different Session": func(r *CreateRuntimeSessionRequest) { r.NativeRestore.Snapshot.SessionUID = "other-session" },
		"different profile": func(r *CreateRuntimeSessionRequest) {
			r.NativeRestore.Snapshot.ProfileDigest = ProfileDigest(testSHA256("other-profile"))
		},
		"different provider": func(r *CreateRuntimeSessionRequest) { r.NativeRestore.Snapshot.ProviderKind = "claude" },
		"write workspace": func(r *CreateRuntimeSessionRequest) {
			r.Workspace.Intent = WorkspaceIntentWrite
			r.Profile.WorkspaceIntent = WorkspaceIntentWrite
			digest, err := CanonicalProfileDigest(r.Profile)
			if err != nil {
				t.Fatal(err)
			}
			r.Metadata.Fence.RuntimeProfileDigest = digest
			r.NativeRestore.Snapshot.ProfileDigest = digest
		},
		"transcript bootstrap": func(r *CreateRuntimeSessionRequest) { r.Bootstrap = &SessionBootstrap{} },
	} {
		t.Run(name, func(t *testing.T) {
			r := nativeSessionTestCreateRequest(t)
			mutate(&r)
			sealRequest(t, r, &r.Metadata.RequestDigest)
			if err := r.ValidateAt(testNow); err == nil || !strings.Contains(err.Error(), "native restore:") {
				t.Fatalf("authorized but incompatible restore validation = %v, want native restore rejection", err)
			}
		})
	}
}

func TestNativeSessionPayloadBoundsAndDigest(t *testing.T) {
	const sha256ABC = "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	const maxCompressedBytes = 512 << 10
	if got := NativeSessionDataDigest([]byte("abc")); got != sha256ABC {
		t.Fatalf("native payload digest = %q, want standard SHA-256 vector", got)
	}
	for _, size := range []int{0, 1, maxCompressedBytes, maxCompressedBytes + 1} {
		snapshot := nativeSessionTestCreateRequest(t).NativeRestore.Snapshot
		// Compression is validated by the provider codec; the wire contract
		// bounds and authenticates the opaque compressed bytes.
		snapshot.Data = bytes.Repeat([]byte{0x80}, size)
		snapshot.DataDigest = NativeSessionDataDigest(snapshot.Data)
		wantValid := size > 0 && size <= maxCompressedBytes
		if err := snapshot.Validate(); (err == nil) != wantValid {
			t.Fatalf("snapshot with %d bytes validation = %v, want valid %v", size, err, wantValid)
		}
	}
	snapshot := nativeSessionTestCreateRequest(t).NativeRestore.Snapshot
	snapshot.Data[0] ^= 1
	if err := snapshot.Validate(); err == nil {
		t.Fatal("modified native payload passed its data digest check")
	}
}

func TestNativeSessionCaptureRequestAuthorization(t *testing.T) {
	request := nativeSessionTestCaptureRequest(t, testNow)
	if err := request.ValidateAt(testNow); err != nil {
		t.Fatal(err)
	}
	request.CaptureNativeSession = false
	if err := request.ValidateAt(testNow); err == nil || !strings.Contains(err.Error(), "request digest mismatch") {
		t.Fatalf("capture flag mutation validation = %v, want request digest mismatch", err)
	}
	request.CaptureNativeSession = true
	request.Intent = WorkspaceIntentWrite
	sealRequest(t, request, &request.Metadata.RequestDigest)
	if err := request.ValidateAt(testNow); err == nil || !strings.Contains(err.Error(), "requires read") {
		t.Fatalf("write capture validation = %v, want read intent requirement", err)
	}
}

func TestNativeSessionCaptureWireResponse(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*CreateWorkspaceDeltaRequest, *CreateWorkspaceDeltaResponse)
		wantErr bool
	}{
		{name: "authorized unchanged workspace"},
		{name: "capture unavailable permits fallback", mutate: func(_ *CreateWorkspaceDeltaRequest, r *CreateWorkspaceDeltaResponse) {
			r.NativeSession = nil
			r.NativeSessionReason = "provider_state_unavailable"
		}},
		{name: "unrequested private data", wantErr: true, mutate: func(q *CreateWorkspaceDeltaRequest, _ *CreateWorkspaceDeltaResponse) {
			q.CaptureNativeSession = false
		}},
		{name: "workspace changed", wantErr: true, mutate: func(_ *CreateWorkspaceDeltaRequest, r *CreateWorkspaceDeltaResponse) {
			r.Delta.State, r.Delta.EntryCount, r.Delta.PublicationSafe = WorkspaceDeltaReadOnlyModified, 1, false
		}},
		{name: "different Session", wantErr: true, mutate: func(_ *CreateWorkspaceDeltaRequest, r *CreateWorkspaceDeltaResponse) {
			r.NativeSession.SessionUID = "other-session"
		}},
		{name: "different profile", wantErr: true, mutate: func(_ *CreateWorkspaceDeltaRequest, r *CreateWorkspaceDeltaResponse) {
			r.NativeSession.ProfileDigest = ProfileDigest(testSHA256("other-profile"))
		}},
		{name: "corrupt native data", wantErr: true, mutate: func(_ *CreateWorkspaceDeltaRequest, r *CreateWorkspaceDeltaResponse) {
			r.NativeSession.Data[0] ^= 1
		}},
		{name: "unbounded fallback reason", wantErr: true, mutate: func(_ *CreateWorkspaceDeltaRequest, r *CreateWorkspaceDeltaResponse) {
			r.NativeSession = nil
			r.NativeSessionReason = strings.Repeat("x", 129)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			request := nativeSessionTestCaptureRequest(t, now)
			response := nativeSessionTestCaptureResponse(t, request, now)
			if test.mutate != nil {
				test.mutate(&request, &response)
			}
			sealRequest(t, request, &request.Metadata.RequestDigest)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var decoded CreateWorkspaceDeltaRequest
				clientTestDecodeMutation(t, r, &decoded, true)
				if err := decoded.ValidateAt(now); err != nil {
					t.Errorf("wire capture request validation: %v", err)
				}
				writeClientTestJSON(w, http.StatusOK, response)
			}))
			defer server.Close()
			got, err := clientTestClient(t, server.URL).CreateWorkspaceDelta(context.Background(), "runtime-session-1", request)
			if test.wantErr {
				if !errors.Is(err, ErrClientProtocol) || got != nil {
					t.Fatalf("unsafe native capture response: got result %v, error %v", got != nil, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("authorized capture response: %v", err)
			}
			if response.NativeSession != nil && (got.NativeSession == nil || !bytes.Equal(got.NativeSession.Data, response.NativeSession.Data)) {
				t.Fatal("private native payload did not survive the authenticated response")
			}
			if got.NativeSessionReason != response.NativeSessionReason {
				t.Fatal("fallback reason did not survive the response")
			}
		})
	}
}

func TestNativeSessionRestorationResponseMatching(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*CreateRuntimeSessionRequest, *CreateRuntimeSessionResponse)
		wantErr bool
	}{
		{name: "matching session load"},
		{name: "matching session resume", mutate: func(_ *CreateRuntimeSessionRequest, r *CreateRuntimeSessionResponse) {
			r.Session.NativeRestoration.Method = "session/resume"
		}},
		{name: "reconstructed fallback may create a conversation", mutate: func(_ *CreateRuntimeSessionRequest, r *CreateRuntimeSessionResponse) {
			r.Session.NativeRestoration.Method = "reconstructed"
			r.Session.NativeRestoration.Reason = "provider_rejected_restore"
			r.Session.ProviderSessionID = "new-conversation"
		}},
		{name: "restoration omitted", wantErr: true, mutate: func(_ *CreateRuntimeSessionRequest, r *CreateRuntimeSessionResponse) {
			r.Session.NativeRestoration = nil
		}},
		{name: "different saved copy", wantErr: true, mutate: func(_ *CreateRuntimeSessionRequest, r *CreateRuntimeSessionResponse) {
			r.Session.NativeRestoration.SnapshotID = "other-snapshot"
		}},
		{name: "different restored conversation", wantErr: true, mutate: func(_ *CreateRuntimeSessionRequest, r *CreateRuntimeSessionResponse) {
			r.Session.ProviderSessionID = "other-conversation"
		}},
		{name: "unknown restoration method", wantErr: true, mutate: func(_ *CreateRuntimeSessionRequest, r *CreateRuntimeSessionResponse) {
			r.Session.NativeRestoration.Method = "session/new"
		}},
		{name: "unrequested native restoration", wantErr: true, mutate: func(q *CreateRuntimeSessionRequest, _ *CreateRuntimeSessionResponse) {
			q.NativeRestore = nil
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := nativeSessionTestCreateRequest(t)
			response := clientTestCreateSessionResponse(request, testNow, RequestClassificationFresh)
			response.Session.ProviderSessionID = request.NativeRestore.Snapshot.ProviderSessionID
			response.Session.NativeRestoration = &NativeSessionRestoration{SnapshotID: request.NativeRestore.SnapshotID, Method: "session/load"}
			if test.mutate != nil {
				test.mutate(&request, &response)
			}
			sealRequest(t, request, &request.Metadata.RequestDigest)
			if err := request.ValidateAt(testNow); err != nil {
				t.Fatal(err)
			}
			if err := response.ValidateFor(request); (err != nil) != test.wantErr {
				t.Fatalf("restoration response validation = %v, want error %v", err, test.wantErr)
			}
		})
	}
}

func TestNativeSessionStatusOptionalCreationIdentity(t *testing.T) {
	base := RuntimeSessionStatus{
		RuntimeSessionID: "runtime-session-1", RuntimeSessionUID: "session-uid-1", Generation: 1,
		State: RuntimeSessionStateIdle, LastTransitionAt: testNow,
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("legacy status without creation or restoration metadata: %v", err)
	}
	for name, mutate := range map[string]func(*RuntimeSessionStatus){
		"attempt without Task":          func(s *RuntimeSessionStatus) { s.CreationTaskAttempt = 1 },
		"Task without attempt":          func(s *RuntimeSessionStatus) { s.CreationTaskUID = "task-uid-1" },
		"unbounded provider session ID": func(s *RuntimeSessionStatus) { s.ProviderSessionID = strings.Repeat("x", 1025) },
		"native result without snapshot identity": func(s *RuntimeSessionStatus) {
			s.NativeRestoration = &NativeSessionRestoration{Method: "session/load"}
		},
		"unbounded restoration reason": func(s *RuntimeSessionStatus) {
			s.NativeRestoration = &NativeSessionRestoration{Method: "reconstructed", Reason: strings.Repeat("x", 129)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			status := base
			mutate(&status)
			if err := status.Validate(); err == nil {
				t.Fatal("malformed native session status was accepted")
			}
		})
	}
}

func TestNativeSessionStatusAuthenticatedRoundTrip(t *testing.T) {
	now := time.Now().UTC()
	response := clientTestStatus(t, now)
	response.Sessions = []RuntimeSessionStatus{{
		RuntimeSessionID: "runtime-session-1", RuntimeSessionUID: "session-uid-1", Generation: 1,
		State: RuntimeSessionStateIdle, LastTransitionAt: now,
		CreationTaskUID: "task-uid-1", CreationTaskAttempt: 2, ProviderSessionID: "saved-conversation",
		NativeRestoration: &NativeSessionRestoration{SnapshotID: "snapshot-1", Method: "session/load"},
	}}
	response.Pressure.ResidentSessions = 1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != StatusPath {
			t.Errorf("unexpected native status route: %s %s", r.Method, r.URL.Path)
		}
		clientTestRequireBearer(t, r)
		binding := StatusCapabilityBinding{RuntimeProfileDigest: response.Fence.RuntimeProfileDigest}
		if _, err := VerifyStatusCapability(clientTestCapabilitySecret, r.Header.Get(OperationCapabilityHeader), binding, time.Now().UTC()); err != nil {
			t.Errorf("native status capability: %v", err)
		}
		writeClientTestJSON(w, http.StatusOK, response)
	}))
	defer server.Close()
	got, err := clientTestClient(t, server.URL).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Sessions) != 1 {
		t.Fatalf("authenticated status returned %d sessions", len(got.Sessions))
	}
	actual, want := got.Sessions[0], response.Sessions[0]
	if actual.CreationTaskUID != want.CreationTaskUID || actual.CreationTaskAttempt != want.CreationTaskAttempt ||
		actual.ProviderSessionID != want.ProviderSessionID || actual.NativeRestoration == nil ||
		*actual.NativeRestoration != *want.NativeRestoration {
		t.Fatal("authenticated status lost creation or restoration metadata")
	}
}

func TestNativeSessionStatusRequiresAuthentication(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	bearer := WithControllerBearerToken(clientTestBearer)
	capability := WithOperationCapabilitySecret(clientTestCapabilitySecret)
	binding := WithStatusCapabilityBinding(StatusCapabilityBinding{RuntimeProfileDigest: testFence(t).RuntimeProfileDigest})
	for name, options := range map[string][]ClientOption{
		"missing bearer":           {capability, binding},
		"missing operation secret": {bearer, binding},
		"missing profile binding":  {bearer, capability},
	} {
		t.Run(name, func(t *testing.T) {
			client, err := NewClient(server.URL, options...)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Status(context.Background()); !errors.Is(err, ErrClientConfiguration) {
				t.Fatalf("status without authentication = %v, want configuration error", err)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("native status request reached the network without complete authentication")
	}
}

func nativeSessionTestCreateRequest(t *testing.T) CreateRuntimeSessionRequest {
	t.Helper()
	request := clientTestCreateSessionRequest(t, testNow, "native-create")
	request.Profile.ProviderKind = "opencode"
	request.Profile.AdapterDigests = map[string]string{"opencode": testSHA256("opencode-adapter")}
	request.Profile.WorkspaceIntent = WorkspaceIntentRead
	request.AgentConfiguration.ProviderKind = "opencode"
	request.AgentConfiguration.ReasoningEffort = ""
	var err error
	request.Profile.AgentConfigurationDigest, err = CanonicalAgentConfigurationDigest(*request.AgentConfiguration)
	if err != nil {
		t.Fatal(err)
	}
	request.Metadata.Fence.RuntimeProfileDigest, err = CanonicalProfileDigest(request.Profile)
	if err != nil {
		t.Fatal(err)
	}
	request.Workspace.Intent = WorkspaceIntentRead
	data := []byte("opaque compressed provider state")
	request.NativeRestore = &NativeSessionRestore{
		SnapshotID: "snapshot-1",
		Snapshot: NativeSessionSnapshot{
			SessionUID: request.Metadata.Fence.RuntimeSessionUID, ProfileDigest: request.Metadata.Fence.RuntimeProfileDigest,
			ProviderKind: "opencode", ProviderVersion: "1.18.9", ProviderSessionID: "saved-conversation",
			WorkingDirectory: "/workspace", WorkspaceStateDigest: testSHA256("workspace-state"),
			Data: data, DataDigest: NativeSessionDataDigest(data),
		},
	}
	sealRequest(t, request, &request.Metadata.RequestDigest)
	if err := request.ValidateAt(testNow); err != nil {
		t.Fatalf("native create fixture: %v", err)
	}
	return request
}

func nativeSessionTestCaptureRequest(t *testing.T, now time.Time) CreateWorkspaceDeltaRequest {
	t.Helper()
	request := clientTestDeltaRequest(t, now, "native-capture")
	request.Intent, request.CaptureNativeSession = WorkspaceIntentRead, true
	request.Metadata.Fence.RuntimeProfileDigest = nativeSessionTestCreateRequest(t).Metadata.Fence.RuntimeProfileDigest
	sealRequest(t, request, &request.Metadata.RequestDigest)
	return request
}

func nativeSessionTestCaptureResponse(t *testing.T, request CreateWorkspaceDeltaRequest, now time.Time) CreateWorkspaceDeltaResponse {
	t.Helper()
	snapshot := nativeSessionTestCreateRequest(t).NativeRestore.Snapshot
	return CreateWorkspaceDeltaResponse{
		Protocol: ProtocolVersion, Classification: Classification{Class: RequestClassificationFresh},
		Delta: WorkspaceDeltaDescriptor{
			DeltaID: request.DeltaID, RuntimeSessionUID: request.Metadata.Fence.RuntimeSessionUID,
			SessionGeneration: request.Metadata.Fence.RuntimeSessionGeneration, State: WorkspaceDeltaNoChange,
			Intent: request.Intent, VerifiedBaseline: request.VerifiedBaseline,
			NoFollowVerified: true, PublicationSafe: true, FrozenAt: now,
		},
		NativeSession: &snapshot,
	}
}
