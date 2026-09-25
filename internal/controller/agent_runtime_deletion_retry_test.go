package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/harness/v2/conformance/conformancetest"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestAgentRuntimeReconcilerRetriesPoisonedDrainCleanup(t *testing.T) {
	server, reconciler, runtimeObject, _ := newAgentRuntimeDrainRetryFixture(t)
	request := reconcileRequestFor(runtimeObject)
	// Automatic drain already failed. No Task or Session cleanup controller is
	// present to send a second DELETE on behalf of this resident session.
	if _, err := reconciler.Reconcile(t.Context(), request); err == nil ||
		!strings.Contains(err.Error(), "retry poisoned AgentRuntime session cleanup") {
		t.Fatalf("failed cleanup reconcile error = %v, want retained cleanup failure", err)
	}
	assertAgentRuntimeCleanupFinalizer(t, getAgentRuntime(t, reconciler, runtimeObject))
	first := server.deleteRequests()
	if len(first) != 1 {
		t.Fatalf("first reconcile DELETE count = %d, want one", len(first))
	}

	result, err := reconciler.Reconcile(t.Context(), request)
	if err != nil || result.RequeueAfter != agentRuntimeDeleteRequeue {
		t.Fatalf("successful cleanup retry = %#v, %v", result, err)
	}
	assertAgentRuntimeCleanupFinalizer(t, getAgentRuntime(t, reconciler, runtimeObject))
	deletes := server.deleteRequests()
	if len(deletes) != 2 || deletes[0].Metadata.OperationID != deletes[1].Metadata.OperationID {
		t.Fatalf("cleanup retries = %#v, want one stable operation for the original resident", deletes)
	}
	// A second poisoned resident was also available. Reconciliation remains
	// bounded to one exact DELETE, and never interrupts active/deleting peers.
	if len(server.DrainOperationIDs()) != 0 {
		t.Fatal("cleanup retry allocated another pool drain operation")
	}
	server.mu.Lock()
	server.sessions = nil
	originalFence := server.fence
	server.fence.SupervisorBootID = "replacement-boot"
	server.mu.Unlock()
	if _, err := reconciler.Reconcile(t.Context(), request); err == nil ||
		!strings.Contains(err.Error(), "deletion status fence drifted") {
		t.Fatalf("quiescent replacement status error = %v, want exact-fence rejection", err)
	}
	assertAgentRuntimeCleanupFinalizer(t, getAgentRuntime(t, reconciler, runtimeObject))
	server.SetFence(originalFence)
	if _, err := reconciler.Reconcile(t.Context(), request); err != nil {
		t.Fatalf("later original quiescent status: %v", err)
	}
	if err := reconciler.Get(t.Context(), client.ObjectKeyFromObject(runtimeObject), &corev1alpha1.AgentRuntime{}); !apierrors.IsNotFound(err) {
		t.Fatalf("AgentRuntime after validated quiescence Get() error = %v", err)
	}
	if len(server.deleteRequests()) != 2 {
		t.Fatal("status observation replayed session cleanup")
	}
	if errors := server.ProtocolErrors(); len(errors) != 0 {
		t.Fatalf("cleanup protocol errors = %v", errors)
	}
}

func TestAgentRuntimeReconcilerDrainRetryRevalidatesAuthority(t *testing.T) {
	server, reconciler, runtimeObject, secretObject := newAgentRuntimeDrainRetryFixture(t)
	server.afterStatus = func() error {
		var secret corev1.Secret
		if err := reconciler.Get(t.Context(), client.ObjectKeyFromObject(secretObject), &secret); err != nil {
			return err
		}
		secret.Annotations["test-authority-change"] = "after-status"
		return reconciler.Update(t.Context(), &secret)
	}
	if _, err := reconciler.Reconcile(t.Context(), reconcileRequestFor(runtimeObject)); err == nil ||
		!strings.Contains(err.Error(), "authentication authority changed") {
		t.Fatalf("authority drift error = %v, want pre-mutation refusal", err)
	}
	assertAgentRuntimeCleanupFinalizer(t, getAgentRuntime(t, reconciler, runtimeObject))
	if len(server.deleteRequests()) != 0 || len(server.DrainOperationIDs()) != 0 {
		t.Fatal("changed cleanup authority reached a mutation endpoint")
	}
}

type agentRuntimeDrainRetryServer struct {
	*agentRuntimeDeletionTestServer
	sessions    []harnessv2.RuntimeSessionStatus
	deletes     []harnessv2.DeleteRuntimeSessionRequest
	afterStatus func() error
}

func newAgentRuntimeDrainRetryFixture(t *testing.T) (*agentRuntimeDrainRetryServer, *AgentRuntimeReconciler, *corev1alpha1.AgentRuntime, *corev1.Secret) {
	t.Helper()
	profile, claims, limits := testAgentRuntimeProfileClaimsAndLimits()
	digest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	config := conformancetest.Config{
		ControllerBearerToken: strings.Repeat("t", 32), OperationCapabilitySecret: []byte(strings.Repeat("s", 32)),
		RuntimeInstanceID: "external-runtime-instance-1", SupervisorBootID: "boot-1", RuntimePoolUID: "external-pool-1",
		Profile: profile, Limits: limits, SupportsDrain: true, WorkspaceGovernance: claims,
	}
	fence := harnessv2.Fence{
		RuntimeInstanceID: config.RuntimeInstanceID, SupervisorBootID: config.SupervisorBootID,
		ControllerEpoch: 1, RuntimePoolUID: config.RuntimePoolUID, RuntimePoolGeneration: 7,
		RuntimeProfileDigest: digest, ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
	}
	server := &agentRuntimeDrainRetryServer{agentRuntimeDeletionTestServer: &agentRuntimeDeletionTestServer{
		bearer: config.ControllerBearerToken, capability: config.OperationCapabilitySecret, fence: fence,
		drainRequested: true, drainRequestedAt: time.Now().UTC().Add(-time.Minute),
	}}
	for i, state := range []harnessv2.RuntimeSessionState{
		harnessv2.RuntimeSessionStatePromptRunning, harnessv2.RuntimeSessionStateDeleting,
		harnessv2.RuntimeSessionStateCreating, harnessv2.RuntimeSessionStateIdle,
		harnessv2.RuntimeSessionStatePoisoned, harnessv2.RuntimeSessionStatePoisoned,
	} {
		session := harnessv2.RuntimeSessionStatus{
			RuntimeSessionID:  harnessv2.RuntimeSessionID(fmt.Sprintf("opaque-resident-%d", i)),
			RuntimeSessionUID: harnessv2.RuntimeSessionUID(fmt.Sprintf("owner-%d", i)), Generation: 3,
			State: state, LastTransitionAt: time.Now().UTC(),
		}
		if state == harnessv2.RuntimeSessionStatePromptRunning {
			session.ActivePromptID = "still-running"
		}
		server.sessions = append(server.sessions, session)
	}
	server.Server = httptest.NewServer(http.HandlerFunc(server.handleRetry))
	t.Cleanup(server.Close)
	runtimeObject, secret := testAgentRuntimeAndSecret(t, server.URL, config)
	reconciler := newAgentRuntimeUnitReconciler(t, runtimeObject, secret)
	allowAgentRuntimeLoopback(t)
	seedDeletingAgentRuntime(t, reconciler, runtimeObject, secret, fence)
	return server, reconciler, runtimeObject, secret
}

func (s *agentRuntimeDrainRetryServer) handleRetry(w http.ResponseWriter, request *http.Request) {
	switch {
	case request.Method == http.MethodGet && request.URL.Path == harnessv2.StatusPath:
		// Reuse the existing fixture's authenticated status implementation,
		// replacing only the observed resident cleanup state.
		response := httptest.NewRecorder()
		s.handleStatus(response, request)
		if response.Code != http.StatusOK {
			w.WriteHeader(response.Code)
			_, _ = w.Write(response.Body.Bytes())
			return
		}
		var status harnessv2.StatusResponse
		if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
			s.protocolErrorf("decode fixture status: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		s.mu.Lock()
		status.Sessions = slices.Clone(s.sessions)
		afterStatus := s.afterStatus
		s.mu.Unlock()
		status.Lifecycle = harnessv2.SupervisorLifecycleTerminating
		status.Drain.Reason = "drain_session_cleanup_unproven"
		status.Pressure = harnessv2.PressureMetadata{ResidentSessions: uint32(len(status.Sessions))}
		for _, session := range status.Sessions {
			if session.ActivePromptID != "" {
				status.ActivePrompts = append(status.ActivePrompts, harnessv2.ActivePromptStatus{
					RuntimeSessionUID: session.RuntimeSessionUID, SessionGeneration: session.Generation,
					TaskUID: "active-task", TaskAttempt: 1, PromptID: session.ActivePromptID,
					LeaseExpiresAt: time.Now().Add(time.Minute), FrameSequence: 1, StartedAt: session.LastTransitionAt,
				})
			}
		}
		status.Pressure.ActivePrompts = uint32(len(status.ActivePrompts))
		if afterStatus != nil {
			if err := afterStatus(); err != nil {
				s.protocolErrorf("update test authority: %v", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		writeAgentRuntimeDeletionTestJSON(w, http.StatusOK, status)
	case request.Method == http.MethodDelete:
		s.handleRetryDelete(w, request)
	default:
		s.handle(w, request)
	}
}

func (s *agentRuntimeDrainRetryServer) handleRetryDelete(w http.ResponseWriter, request *http.Request) {
	if !s.authorizeBearer(w, request) {
		return
	}
	var deletion harnessv2.DeleteRuntimeSessionRequest
	if err := json.NewDecoder(request.Body).Decode(&deletion); err != nil {
		s.protocolErrorf("decode session DELETE: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	if err := deletion.ValidateAt(now); err != nil {
		s.protocolErrorf("validate session DELETE: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if err := harnessv2.VerifyOperationCapability(s.capability, request.Header.Get(harnessv2.OperationCapabilityHeader), deletion.Metadata, true, now); err != nil {
		s.protocolErrorf("verify session DELETE capability: %v", err)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	expected := s.fence
	expected.RuntimeSessionUID, expected.RuntimeSessionGeneration = "owner-4", 3
	if request.URL.Path != "/v2/runtime-sessions/opaque-resident-4" || deletion.Metadata.Fence != expected ||
		deletion.Metadata.TaskUID != "" || deletion.Metadata.TaskAttempt != 0 || deletion.Metadata.PromptID != "" ||
		deletion.Reason != "agent_runtime_deletion" {
		s.protocolErrors = append(s.protocolErrors, "DELETE did not preserve the exact poisoned resident's authority")
		writeAgentRuntimeDeletionTestError(w, http.StatusBadRequest, harnessv2.ErrorCodeInvalidRequest, "unexpected session cleanup", false)
		return
	}
	s.deletes = append(s.deletes, deletion)
	if len(s.deletes) == 1 {
		writeAgentRuntimeDeletionTestError(w, http.StatusInternalServerError, harnessv2.ErrorCodeSessionPoisoned, "broker cleanup is still unproven", false)
		return
	}
	writeAgentRuntimeDeletionTestJSON(w, http.StatusOK, harnessv2.DeleteRuntimeSessionResponse{
		Protocol: harnessv2.ProtocolVersion, Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh},
		State: harnessv2.RuntimeSessionStateDeleted,
		Tombstone: harnessv2.RuntimeSessionTombstone{
			RuntimeSessionUID: expected.RuntimeSessionUID, RuntimeSessionGeneration: expected.RuntimeSessionGeneration,
			RuntimeProfileDigest: expected.RuntimeProfileDigest, DeletedAt: now,
			Operations: []harnessv2.OperationRecord{{
				OperationID: deletion.Metadata.OperationID, RequestDigest: deletion.Metadata.RequestDigest,
				Phase: harnessv2.OperationPhaseDeleted, RecordedAt: now,
			}},
		},
	})
}

func (s *agentRuntimeDrainRetryServer) deleteRequests() []harnessv2.DeleteRuntimeSessionRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.deletes)
}
