package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/harness"
	"github.com/sozercan/orka/internal/harness/harnesstest"
)

func TestAgentRuntimeReconcilerMarksReadyForFakeHarness(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{RuntimeName: "fibey-agentkit", AuthToken: "x"})
	defer server.Close()

	runtime, secret := testAgentRuntimeAndSecret(server.URL())
	r := newAgentRuntimeUnitReconciler(t, runtime, secret)
	if _, err := r.Reconcile(context.Background(), reconcileRequestFor(runtime)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var updated corev1alpha1.AgentRuntime
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(runtime), &updated); err != nil {
		t.Fatalf("Get AgentRuntime: %v", err)
	}
	if !updated.Status.Ready {
		t.Fatalf("Ready = false, message=%q", updated.Status.Message)
	}
	if updated.Status.ObservedGeneration != runtime.Generation {
		t.Fatalf("ObservedGeneration = %d, want %d", updated.Status.ObservedGeneration, runtime.Generation)
	}
	if updated.Status.ObservedCapabilities == nil || updated.Status.ObservedCapabilities.RuntimeName != "fibey-agentkit" {
		t.Fatalf("ObservedCapabilities = %#v", updated.Status.ObservedCapabilities)
	}
	cond := meta.FindStatusCondition(updated.Status.Conditions, agentRuntimeReadyCondition)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("Ready condition = %#v, want true", cond)
	}
}

func TestAgentRuntimeReconcilerRevalidatesBearerAuthOnReadyRuntime(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{RuntimeName: "fibey-agentkit", AuthToken: "x"})
	defer server.Close()

	runtime, secret := testAgentRuntimeAndSecret(server.URL())
	r := newAgentRuntimeUnitReconciler(t, runtime, secret)
	if _, err := r.Reconcile(context.Background(), reconcileRequestFor(runtime)); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	var ready corev1alpha1.AgentRuntime
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(runtime), &ready); err != nil {
		t.Fatalf("Get ready AgentRuntime: %v", err)
	}
	if !ready.Status.Ready {
		t.Fatalf("Ready = false after first reconcile, message=%q", ready.Status.Message)
	}

	var changed corev1.Secret
	if err := r.Get(context.Background(), client.ObjectKey{Name: secret.Name, Namespace: secret.Namespace}, &changed); err != nil {
		t.Fatalf("Get Secret: %v", err)
	}
	changed.Data["token"] = []byte("wrong")
	if err := r.Update(context.Background(), &changed); err != nil {
		t.Fatalf("Update Secret: %v", err)
	}
	if _, err := r.Reconcile(context.Background(), reconcileRequestFor(runtime)); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	var updated corev1alpha1.AgentRuntime
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(runtime), &updated); err != nil {
		t.Fatalf("Get updated AgentRuntime: %v", err)
	}
	if updated.Status.Ready {
		t.Fatalf("Ready = true, want false after bearer auth changed")
	}
	if !strings.Contains(updated.Status.Message, "401") && !strings.Contains(updated.Status.Message, "unauthorized") {
		t.Fatalf("Message = %q, want auth failure", updated.Status.Message)
	}
}

func TestAgentRuntimeReconcilerRechecksUnauthenticatedMutationOnReadyRuntime(t *testing.T) {
	var requireAuth bool = true
	turns := map[harness.HarnessTurnID]harness.StartTurnRequest{}
	mux := http.NewServeMux()
	mux.HandleFunc(harness.HealthPath, func(w http.ResponseWriter, r *http.Request) {
		harness.WriteJSON(w, http.StatusOK, harness.HealthResponse{
			Version:   harness.ProtocolVersion,
			Status:    harness.HealthStatusOK,
			Ready:     true,
			CheckedAt: time.Now().UTC(),
		})
	})
	mux.HandleFunc(harness.CapabilitiesPath, func(w http.ResponseWriter, r *http.Request) {
		harness.WriteJSON(w, http.StatusOK, harness.CapabilitiesResponse{
			Version:                 harness.ProtocolVersion,
			ProtocolVersion:         harness.ProtocolVersion,
			Transport:               harness.HTTPTransport,
			RuntimeName:             "fibey-agentkit",
			ProviderKind:            harness.ProviderKindKubernetesService,
			ToolExecutionModes:      []harness.ToolExecutionMode{harness.ToolExecutionModeObserved},
			SupportsCancel:          true,
			SupportsRuntimeSessions: true,
		})
	})
	mux.HandleFunc(harness.TurnsPath, func(w http.ResponseWriter, r *http.Request) {
		if requireAuth && strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") != "x" {
			harness.WriteError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		var request harness.StartTurnRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			harness.WriteError(w, http.StatusBadRequest, "invalid JSON request")
			return
		}
		if _, exists := turns[request.TurnID]; exists {
			harness.WriteError(w, http.StatusConflict, "turn already exists")
			return
		}
		turns[request.TurnID] = request
		eventsPath, _ := harness.EventStreamPath(request.TurnID)
		harness.WriteJSON(w, http.StatusAccepted, harness.StartTurnResponse{
			Version:          harness.ProtocolVersion,
			Accepted:         true,
			RuntimeSessionID: request.RuntimeSessionID,
			TurnID:           request.TurnID,
			CorrelationID:    request.CorrelationID,
			EventStreamPath:  eventsPath,
		})
	})
	mux.HandleFunc(harness.TurnsPath+"/", func(w http.ResponseWriter, r *http.Request) {
		if requireAuth && strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") != "x" {
			harness.WriteError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		turnID, resource, err := harness.ParseTurnResourcePath(r.URL.EscapedPath())
		if err != nil {
			harness.WriteError(w, http.StatusNotFound, "not found")
			return
		}
		request, ok := turns[turnID]
		if !ok {
			harness.WriteError(w, http.StatusNotFound, "turn not found")
			return
		}
		switch resource {
		case "events":
			_ = harness.WriteSSEFrame(w, harness.HarnessEventFrame{
				Version:          harness.ProtocolVersion,
				Type:             harness.FrameTurnCompleted,
				RuntimeSessionID: request.RuntimeSessionID,
				TurnID:           request.TurnID,
				CorrelationID:    request.CorrelationID,
				Seq:              1,
				Completed:        &harness.TurnCompleted{Result: "ok", FinalEventSeq: 1},
			})
			_ = harness.WriteSSEDone(w)
		case "cancel":
			harness.WriteJSON(w, http.StatusAccepted, harness.CancelTurnResponse{Version: harness.ProtocolVersion, Accepted: true, RuntimeSessionID: request.RuntimeSessionID, TurnID: request.TurnID, CorrelationID: request.CorrelationID})
		default:
			harness.WriteError(w, http.StatusNotFound, "not found")
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	runtime, secret := testAgentRuntimeAndSecret(server.URL)
	r := newAgentRuntimeUnitReconciler(t, runtime, secret)
	if _, err := r.Reconcile(context.Background(), reconcileRequestFor(runtime)); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	requireAuth = false
	if _, err := r.Reconcile(context.Background(), reconcileRequestFor(runtime)); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	var updated corev1alpha1.AgentRuntime
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(runtime), &updated); err != nil {
		t.Fatalf("Get AgentRuntime: %v", err)
	}
	if updated.Status.Ready {
		t.Fatalf("Ready = true, want false after unauthenticated StartTurn became accepted")
	}
	if !strings.Contains(updated.Status.Message, "unauthenticated start turn was accepted") {
		t.Fatalf("Message = %q, want unauthenticated start failure", updated.Status.Message)
	}
}

func TestAgentRuntimeReconcilerMarksNotReadyForBadProtocol(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{ProtocolVersion: "orka.harness.v0", AuthToken: "x"})
	defer server.Close()

	runtime, secret := testAgentRuntimeAndSecret(server.URL())
	r := newAgentRuntimeUnitReconciler(t, runtime, secret)
	if _, err := r.Reconcile(context.Background(), reconcileRequestFor(runtime)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var updated corev1alpha1.AgentRuntime
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(runtime), &updated); err != nil {
		t.Fatalf("Get AgentRuntime: %v", err)
	}
	if updated.Status.Ready {
		t.Fatalf("Ready = true, want false")
	}
	if !strings.Contains(updated.Status.Message, "unsupported protocol version") {
		t.Fatalf("Message = %q, want unsupported protocol", updated.Status.Message)
	}
}

func TestAgentRuntimeReconcilerRejectsUnlabeledBearerSecret(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{AuthToken: "x"})
	defer server.Close()

	runtime, secret := testAgentRuntimeAndSecret(server.URL())
	secret.Labels = nil
	r := newAgentRuntimeUnitReconciler(t, runtime, secret)
	if _, err := r.Reconcile(context.Background(), reconcileRequestFor(runtime)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var updated corev1alpha1.AgentRuntime
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(runtime), &updated); err != nil {
		t.Fatalf("Get AgentRuntime: %v", err)
	}
	if updated.Status.Ready {
		t.Fatalf("Ready = true, want false")
	}
	if !strings.Contains(updated.Status.Message, agentRuntimeAuthUseLabel) {
		t.Fatalf("Message = %q, want missing auth-secret label", updated.Status.Message)
	}
}

func TestAgentRuntimeReconcilerReportsMissingBearerSecret(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{AuthToken: "x"})
	defer server.Close()

	runtime, _ := testAgentRuntimeAndSecret(server.URL())
	r := newAgentRuntimeUnitReconciler(t, runtime)
	if _, err := r.Reconcile(context.Background(), reconcileRequestFor(runtime)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var updated corev1alpha1.AgentRuntime
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(runtime), &updated); err != nil {
		t.Fatalf("Get AgentRuntime: %v", err)
	}
	if updated.Status.Ready {
		t.Fatalf("Ready = true, want false")
	}
	if !strings.Contains(updated.Status.Message, "not found") {
		t.Fatalf("Message = %q, want missing Secret context", updated.Status.Message)
	}
}

func TestAgentRuntimeReconcilerReportsMissingBearerSecretKey(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{AuthToken: "x"})
	defer server.Close()

	runtime, secret := testAgentRuntimeAndSecret(server.URL())
	secret.Data = map[string][]byte{"other": []byte("x")}
	r := newAgentRuntimeUnitReconciler(t, runtime, secret)
	if _, err := r.Reconcile(context.Background(), reconcileRequestFor(runtime)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var updated corev1alpha1.AgentRuntime
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(runtime), &updated); err != nil {
		t.Fatalf("Get AgentRuntime: %v", err)
	}
	if updated.Status.Ready {
		t.Fatalf("Ready = true, want false")
	}
	if !strings.Contains(updated.Status.Message, "key") || !strings.Contains(updated.Status.Message, "empty or missing") {
		t.Fatalf("Message = %q, want missing key context", updated.Status.Message)
	}
}

func TestAgentRuntimeReconcilerRequiresCapabilitySubset(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{AuthToken: "x"})
	defer server.Close()

	runtime, secret := testAgentRuntimeAndSecret(server.URL())
	runtime.Spec.Capabilities.ToolExecutionModes = []corev1alpha1.AgentRuntimeToolExecutionMode{corev1alpha1.AgentRuntimeToolExecutionMode("future")}
	r := newAgentRuntimeUnitReconciler(t, runtime, secret)
	if _, err := r.Reconcile(context.Background(), reconcileRequestFor(runtime)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var updated corev1alpha1.AgentRuntime
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(runtime), &updated); err != nil {
		t.Fatalf("Get AgentRuntime: %v", err)
	}
	if updated.Status.Ready {
		t.Fatalf("Ready = true, want false")
	}
	if !strings.Contains(updated.Status.Message, "toolExecutionMode") {
		t.Fatalf("Message = %q, want capability failure", updated.Status.Message)
	}
}

func reconcileRequestFor(obj client.Object) ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKeyFromObject(obj)}
}

func newAgentRuntimeUnitReconciler(t *testing.T, objs ...client.Object) *AgentRuntimeReconciler {
	t.Helper()
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.AgentRuntime{}).
		WithObjects(objs...).
		Build()
	return &AgentRuntimeReconciler{Client: fc, Scheme: scheme}
}

func testAgentRuntimeAndSecret(endpoint string) (*corev1alpha1.AgentRuntime, *corev1.Secret) {
	supportsCancel := true
	supportsSessions := true
	runtime := &corev1alpha1.AgentRuntime{
		ObjectMeta: metav1.ObjectMeta{Name: "fibey-agentkit", Namespace: "default", Generation: 1},
		Spec: corev1alpha1.AgentRuntimeRegistrySpec{
			ContractVersion: corev1alpha1.AgentRuntimeContractHarnessV1,
			Deployment: corev1alpha1.AgentRuntimeDeploymentSpec{
				Mode:     corev1alpha1.AgentRuntimeDeploymentModeExternalEndpoint,
				Endpoint: endpoint,
			},
			ClientAuth: corev1alpha1.AgentRuntimeClientAuth{BearerAuthRef: corev1alpha1.AgentRuntimeBearerAuthReference{
				Name: "fibey-agentkit-harness-token",
				Key:  "token",
			}},
			Capabilities: &corev1alpha1.AgentRuntimeCapabilitiesSpec{
				ToolExecutionModes:      []corev1alpha1.AgentRuntimeToolExecutionMode{corev1alpha1.AgentRuntimeToolExecutionModeObserved},
				SupportsCancel:          &supportsCancel,
				SupportsRuntimeSessions: &supportsSessions,
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "fibey-agentkit-harness-token", Namespace: "default", Labels: map[string]string{agentRuntimeAuthUseLabel: scheduledRunLabelValue, agentRuntimeAuthRefNameLabel: "fibey-agentkit"}},
		Data:       map[string][]byte{"token": []byte("x")},
	}
	return runtime, secret
}

func TestValidateAgentRuntimeRequiredCapabilitiesRequiresCancelAndSessions(t *testing.T) {
	runtime := &corev1alpha1.AgentRuntime{}
	err := validateAgentRuntimeRequiredCapabilities(runtime, &harness.CapabilitiesResponse{
		ToolExecutionModes:      []harness.ToolExecutionMode{harness.ToolExecutionModeObserved},
		SupportsRuntimeSessions: true,
	})
	if err == nil || !strings.Contains(err.Error(), "supportsCancel") {
		t.Fatalf("validateAgentRuntimeRequiredCapabilities() error = %v, want supportsCancel requirement", err)
	}
	err = validateAgentRuntimeRequiredCapabilities(runtime, &harness.CapabilitiesResponse{
		ToolExecutionModes: []harness.ToolExecutionMode{harness.ToolExecutionModeObserved},
		SupportsCancel:     true,
	})
	if err == nil || !strings.Contains(err.Error(), "supportsRuntimeSessions") {
		t.Fatalf("validateAgentRuntimeRequiredCapabilities() error = %v, want supportsRuntimeSessions requirement", err)
	}
}

func TestObservedCapabilitiesFromConformanceRedactsStrings(t *testing.T) {
	leaked := "sk-" + strings.Repeat("a", 20)
	got := observedCapabilitiesFromConformance(&harness.CapabilitiesResponse{
		ProtocolVersion:         harness.ProtocolVersion,
		Transport:               harness.HTTPTransport,
		RuntimeName:             "runtime " + leaked,
		RuntimeVersion:          "Authorization: Bearer " + leaked,
		ProviderKind:            harness.ProviderKindKubernetesService,
		ToolExecutionModes:      []harness.ToolExecutionMode{harness.ToolExecutionModeObserved},
		SupportsCancel:          true,
		SupportsRuntimeSessions: true,
	})
	if got == nil {
		t.Fatal("observed = nil")
	}
	if strings.Contains(got.RuntimeName, leaked) || strings.Contains(got.RuntimeVersion, leaked) {
		t.Fatalf("observed capabilities leaked secret-like values: %#v", got)
	}
}

func TestValidateAgentRuntimeRequiredCapabilitiesRequiresObservedMode(t *testing.T) {
	runtime := &corev1alpha1.AgentRuntime{}
	err := validateAgentRuntimeRequiredCapabilities(runtime, &harness.CapabilitiesResponse{
		ToolExecutionModes: []harness.ToolExecutionMode{harness.ToolExecutionModeBrokered},
	})
	if err == nil || !strings.Contains(err.Error(), "observed") {
		t.Fatalf("validateAgentRuntimeRequiredCapabilities() error = %v, want observed mode requirement", err)
	}
}

func TestObservedCapabilitiesFromConformanceMapsModes(t *testing.T) {
	got := observedCapabilitiesFromConformance(&harness.CapabilitiesResponse{
		ProtocolVersion:         harness.ProtocolVersion,
		RuntimeName:             "runtime-a",
		ToolExecutionModes:      []harness.ToolExecutionMode{harness.ToolExecutionModeObserved},
		SupportsCancel:          true,
		SupportsRuntimeSessions: true,
	})
	if got == nil || len(got.ToolExecutionModes) != 1 || got.ToolExecutionModes[0] != corev1alpha1.AgentRuntimeToolExecutionModeObserved {
		t.Fatalf("observed = %#v", got)
	}
}
