package supervisor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const (
	e2eRecorderTestPoolUID    = "pool-uid"
	e2eRecorderTestSessionUID = "session-uid"
	e2eRecorderTestTaskUID    = "task-uid"
)

func TestControllerE2EPromptWriteFaultRecorderSealsRequest(t *testing.T) {
	const namespace = "default"
	controllerToken := strings.Repeat("t", 32)
	capabilitySecret := []byte(strings.Repeat("s", 32))
	responses := []bool{true, false}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathMatches := r.URL.Path == harnessv2.E2EPromptWriteAmbiguityRecordPath
		authorizationMatches := r.Header.Get("Authorization") == "Bearer "+controllerToken
		namespaceMatches := r.Header.Get(harnessv2.MCPBrokerPoolNamespaceHeader) == namespace
		poolUIDMatches := r.Header.Get(harnessv2.MCPBrokerPoolUIDHeader) == e2eRecorderTestPoolUID
		if !pathMatches || !authorizationMatches || !namespaceMatches || !poolUIDMatches {
			t.Errorf(
				"unexpected recorder request path=%q pathMatch=%t authorizationMatch=%t namespaceMatch=%t poolUIDMatch=%t",
				r.URL.Path, pathMatches, authorizationMatches, namespaceMatches, poolUIDMatches,
			)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var request harnessv2.E2EPromptWriteAmbiguityRecordRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode recorder request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if request.Namespace != namespace || request.PromptOperationID != "start-prompt-prompt-1" {
			t.Errorf("recorder request = %#v", request)
		}
		if got, err := harnessv2.CanonicalRequestDigest(request); err != nil || got != request.Metadata.RequestDigest {
			t.Errorf("recorder request digest = %q err=%v, want %q", got, err, request.Metadata.RequestDigest)
		}
		if err := harnessv2.VerifyOperationCapability(
			capabilitySecret,
			r.Header.Get(harnessv2.OperationCapabilityHeader),
			request.Metadata,
			true,
			time.Now().UTC(),
		); err != nil {
			t.Errorf("verify recorder capability: %v", err)
		}
		_ = json.NewEncoder(w).Encode(harnessv2.E2EPromptWriteAmbiguityRecordResponse{Inject: responses[requests]})
		requests++
	}))
	t.Cleanup(server.Close)

	recorder, err := NewControllerE2EPromptWriteFaultRecorder(server.URL, namespace, controllerToken, capabilitySecret)
	if err != nil {
		t.Fatal(err)
	}
	metadata := harnessv2.MutationMetadata{
		Fence: harnessv2.Fence{
			RuntimeInstanceID: "runtime-1", SupervisorBootID: "boot-1", ControllerEpoch: 1,
			RuntimePoolUID: e2eRecorderTestPoolUID, RuntimePoolGeneration: 1,
			RuntimeSessionUID: e2eRecorderTestSessionUID, RuntimeSessionGeneration: 1,
			RuntimeProfileDigest: harnessv2.ProfileDigest(testDigest("profile")), ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
		},
		TaskUID: e2eRecorderTestTaskUID, TaskAttempt: 1, PromptID: "prompt-1",
		OperationID: "start-prompt-prompt-1", RequestDigestSchemaVersion: harnessv2.RequestDigestSchemaVersion,
	}
	for i, want := range responses {
		inject, err := recorder.Consume(t.Context(), metadata)
		if err != nil || inject != want {
			t.Fatalf("Consume call %d = (%v, %v), want (%v, nil)", i+1, inject, err, want)
		}
	}
	if requests != len(responses) {
		t.Fatalf("recorder requests = %d, want %d", requests, len(responses))
	}
}
