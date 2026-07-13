package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/harness/harnesstest"
)

const testAgentRuntimeClusterDomain = "cluster.local"

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

func TestAgentRuntimeReconcilerOmitsBearerFromDiscoveryProbes(t *testing.T) {
	backend := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{RuntimeName: "fibey-agentkit", AuthToken: "x"})
	defer backend.Close()
	backendURL, err := url.Parse(backend.URL())
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}

	var discoveryAuthSeen atomic.Bool
	var authenticatedProbeSeen atomic.Bool
	proxy := httputil.NewSingleHostReverseProxy(backendURL)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case harness.HealthPath, harness.CapabilitiesPath:
			if r.Header.Get("Authorization") != "" {
				discoveryAuthSeen.Store(true)
			}
		default:
			if strings.HasPrefix(r.URL.Path, harness.TurnsPath) && r.Header.Get("Authorization") == "Bearer x" {
				authenticatedProbeSeen.Store(true)
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	defer front.Close()

	runtime, secret := testAgentRuntimeAndSecret(front.URL)
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
	if discoveryAuthSeen.Load() {
		t.Fatal("health/capabilities received bearer authentication")
	}
	if !authenticatedProbeSeen.Load() {
		t.Fatal("authenticated turn probes did not receive bearer authentication")
	}
}

func TestAgentRuntimeReconcilerPinsAuthenticatedInsecureProbeToValidatedPod(t *testing.T) {
	backend := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{RuntimeName: "fibey-agentkit", AuthToken: "x"})
	defer backend.Close()
	backendURL, err := url.Parse(backend.URL())
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}

	var authenticatedHost atomic.Value
	proxy := httputil.NewSingleHostReverseProxy(backendURL)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer x" {
			authenticatedHost.Store(r.Host)
		}
		proxy.ServeHTTP(w, r)
	}))
	defer front.Close()
	frontURL, err := url.Parse(front.URL)
	if err != nil {
		t.Fatalf("parse front URL: %v", err)
	}
	port, err := strconv.Atoi(frontURL.Port())
	if err != nil {
		t.Fatalf("parse front port: %v", err)
	}
	endpoint := fmt.Sprintf("http://fibey-agentkit.default.svc.cluster.local:%d", port)
	runtime, secret := testAgentRuntimeAndSecret(endpoint)
	service, pods := agentRuntimeInsecureServiceBackends(runtime.Namespace, int32(port), "127.0.0.1")
	objects := []client.Object{runtime, secret, service, pods[0]}
	r := newAgentRuntimeUnitReconciler(t, objects...)

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
	host, _ := authenticatedHost.Load().(string)
	wantHost := strings.TrimPrefix(endpoint, "http://")
	if host != wantHost {
		t.Fatalf("authenticated probe Host = %q, want Service authority %q", host, wantHost)
	}
}

func TestResolveAgentRuntimeInsecureDialAddressDistributesStableKeys(t *testing.T) {
	service, pods := agentRuntimeInsecureServiceBackends("default", 8080, "10.0.0.41", "10.0.0.42")
	r := newAgentRuntimeUnitReconciler(t, service, pods[0], pods[1])
	endpoint := "http://fibey-agentkit.default.svc.cluster.local:8080"
	seen := map[string]struct{}{}
	for i := range 100 {
		key := fmt.Sprintf("task-%d", i)
		address, err := resolveAgentRuntimeInsecureDialAddress(
			context.Background(),
			r.apiReader(),
			"default",
			endpoint,
			corev1alpha1.AgentRuntimeTransportSecurityInsecureClusterLocalHTTP,
			key,
		)
		if err != nil {
			t.Fatalf("resolveAgentRuntimeInsecureDialAddress(%q) error = %v", key, err)
		}
		again, err := resolveAgentRuntimeInsecureDialAddress(
			context.Background(),
			r.apiReader(),
			"default",
			endpoint,
			corev1alpha1.AgentRuntimeTransportSecurityInsecureClusterLocalHTTP,
			key,
		)
		if err != nil || again != address {
			t.Fatalf("selection for %q was not stable: first=%q second=%q err=%v", key, address, again, err)
		}
		seen[address] = struct{}{}
	}
	if len(seen) != 2 {
		t.Fatalf("selected backends = %#v, want both Ready replicas", seen)
	}
}

func TestResolveAgentRuntimeInsecureBackendHonorsServiceIPFamily(t *testing.T) {
	service, pods := agentRuntimeInsecureServiceBackends("default", 8080, "10.0.0.42")
	service.Spec.IPFamilies = []corev1.IPFamily{corev1.IPv6Protocol}
	pods[0].Status.PodIPs = []corev1.PodIP{{IP: "10.0.0.42"}, {IP: "fd00::42"}}
	r := newAgentRuntimeUnitReconciler(t, service, pods[0])
	endpoint := "http://fibey-agentkit.default.svc.cluster.local:8080"

	backend, err := resolveAgentRuntimeInsecureBackend(
		context.Background(),
		r.apiReader(),
		"default",
		endpoint,
		corev1alpha1.AgentRuntimeTransportSecurityInsecureClusterLocalHTTP,
		"task-dual-stack",
	)
	if err != nil {
		t.Fatalf("resolveAgentRuntimeInsecureBackend() error = %v", err)
	}
	if backend.DialAddress != "[fd00::42]:8080" {
		t.Fatalf("DialAddress = %q, want IPv6 address selected by Service family", backend.DialAddress)
	}
	validated, err := validateAgentRuntimeInsecureBackend(
		context.Background(),
		r.apiReader(),
		"default",
		endpoint,
		corev1alpha1.AgentRuntimeTransportSecurityInsecureClusterLocalHTTP,
		backend,
		true,
	)
	if err != nil {
		t.Fatalf("validateAgentRuntimeInsecureBackend() error = %v", err)
	}
	if validated != backend {
		t.Fatalf("validated backend = %#v, want %#v", validated, backend)
	}
}

func TestAgentRuntimePodTargetPortResolvesRestartableInitContainer(t *testing.T) {
	restartAlways := corev1.ContainerRestartPolicyAlways
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		Containers: []corev1.Container{{Name: "app"}},
		InitContainers: []corev1.Container{{
			Name:          "runtime-sidecar",
			RestartPolicy: &restartAlways,
			Ports: []corev1.ContainerPort{{
				Name:          "runtime-http",
				Protocol:      corev1.ProtocolTCP,
				ContainerPort: 18080,
			}},
		}},
	}}
	servicePort := &corev1.ServicePort{
		Port:       8080,
		TargetPort: intstr.FromString("runtime-http"),
	}

	port, ok := agentRuntimePodTargetPort(pod, servicePort)
	if !ok || port != 18080 {
		t.Fatalf("agentRuntimePodTargetPort() = (%d, %t), want (18080, true)", port, ok)
	}
	pod.Spec.InitContainers[0].RestartPolicy = nil
	if port, ok := agentRuntimePodTargetPort(pod, servicePort); ok {
		t.Fatalf("non-restartable init container unexpectedly resolved named port %d", port)
	}
}

func TestAgentRuntimeReconcilerRejectsRedirectingEndpoint(t *testing.T) {
	var redirectedRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectedRequests.Add(1)
		harness.WriteError(w, http.StatusInternalServerError, "redirect should not be followed")
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	runtime, secret := testAgentRuntimeAndSecret(redirector.URL)
	r := newAgentRuntimeUnitReconciler(t, runtime, secret)
	if _, err := r.Reconcile(context.Background(), reconcileRequestFor(runtime)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var updated corev1alpha1.AgentRuntime
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(runtime), &updated); err != nil {
		t.Fatalf("Get AgentRuntime: %v", err)
	}
	if updated.Status.Ready {
		t.Fatalf("Ready = true, want false for redirecting endpoint")
	}
	if !strings.Contains(updated.Status.Message, "redirects are not allowed") {
		t.Fatalf("Message = %q, want redirect rejection", updated.Status.Message)
	}
	if got := redirectedRequests.Load(); got != 0 {
		t.Fatalf("redirect target requests = %d, want 0", got)
	}
}

func TestValidateAgentRuntimeRequiredCapabilitiesChecksBrokeredProfiles(t *testing.T) {
	supportsContinuation := true
	runtime := &corev1alpha1.AgentRuntime{Spec: corev1alpha1.AgentRuntimeRegistrySpec{
		Capabilities: &corev1alpha1.AgentRuntimeCapabilitiesSpec{
			ToolExecutionModes:   []corev1alpha1.AgentRuntimeToolExecutionMode{corev1alpha1.AgentRuntimeToolExecutionModeBrokered},
			BrokeredToolClasses:  []corev1alpha1.AgentRuntimeBrokeredToolClass{corev1alpha1.AgentRuntimeBrokeredToolClassRead},
			SupportsContinuation: &supportsContinuation,
		},
	}}
	caps := &harness.CapabilitiesResponse{
		Version:                 harness.ProtocolVersion,
		ProtocolVersion:         harness.ProtocolVersion,
		Transport:               harness.HTTPTransport,
		RuntimeName:             "fibey-http-runtime",
		ProviderKind:            harness.ProviderKindRemote,
		ToolExecutionModes:      []harness.ToolExecutionMode{harness.ToolExecutionModeBrokered},
		BrokeredToolClasses:     []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
		SupportsCancel:          true,
		SupportsRuntimeSessions: true,
		SupportsContinuation:    true,
	}
	if err := validateAgentRuntimeRequiredCapabilities(runtime, caps); err != nil {
		t.Fatalf("validateAgentRuntimeRequiredCapabilities() error = %v", err)
	}
	caps.BrokeredToolClasses = nil
	if err := validateAgentRuntimeRequiredCapabilities(runtime, caps); err == nil || !strings.Contains(err.Error(), "brokeredToolClass") {
		t.Fatalf("validateAgentRuntimeRequiredCapabilities() = %v, want missing brokered class", err)
	}
}

func TestAgentRuntimeReconcilerMarksBrokeredOnlyRuntimeReadyWhenBrokeredConformancePasses(t *testing.T) {
	type turnState struct {
		request   harness.StartTurnRequest
		continued chan harness.ContinueTurnRequest
	}
	turns := map[harness.HarnessTurnID]*turnState{}
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
			RuntimeName:             "fibey-brokered-runtime",
			ProviderKind:            harness.ProviderKindRemote,
			ToolExecutionModes:      []harness.ToolExecutionMode{harness.ToolExecutionModeBrokered},
			BrokeredToolClasses:     []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
			SupportsCancel:          true,
			SupportsRuntimeSessions: true,
			SupportsContinuation:    true,
		})
	})
	mux.HandleFunc(harness.TurnsPath, func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") != "x" {
			harness.WriteError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		var request harness.StartTurnRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			harness.WriteError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		turns[request.TurnID] = &turnState{request: request, continued: make(chan harness.ContinueTurnRequest, 1)}
		eventsPath, _ := harness.EventStreamPath(request.TurnID)
		harness.WriteJSON(w, http.StatusAccepted, harness.StartTurnResponse{Version: harness.ProtocolVersion, Accepted: true, RuntimeSessionID: request.RuntimeSessionID, TurnID: request.TurnID, CorrelationID: request.CorrelationID, EventStreamPath: eventsPath})
	})
	mux.HandleFunc(harness.TurnsPath+"/", func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") != "x" {
			harness.WriteError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		turnID, resource, err := harness.ParseTurnResourcePath(r.URL.EscapedPath())
		if err != nil {
			harness.WriteError(w, http.StatusNotFound, "not found")
			return
		}
		turn := turns[turnID]
		if turn == nil {
			harness.WriteError(w, http.StatusNotFound, "turn not found")
			return
		}
		switch resource {
		case harness.TurnResourceEvents:
			_ = harness.WriteSSEFrame(w, harness.HarnessEventFrame{Version: harness.ProtocolVersion, Type: harness.FrameTurnStarted, RuntimeSessionID: turn.request.RuntimeSessionID, TurnID: turn.request.TurnID, CorrelationID: turn.request.CorrelationID, Seq: 1})
			_ = harness.WriteSSEFrame(w, harness.HarnessEventFrame{Version: harness.ProtocolVersion, Type: harness.FrameToolCallRequested, RuntimeSessionID: turn.request.RuntimeSessionID, TurnID: turn.request.TurnID, CorrelationID: turn.request.CorrelationID, Seq: 2, ToolName: "conformance_read", ToolCallID: "call-1", Content: json.RawMessage(`{"probe":true}`)})
			select {
			case continued := <-turn.continued:
				_ = harness.WriteSSEFrame(w, harness.HarnessEventFrame{Version: harness.ProtocolVersion, Type: harness.FrameToolResultReceived, RuntimeSessionID: turn.request.RuntimeSessionID, TurnID: turn.request.TurnID, CorrelationID: turn.request.CorrelationID, Seq: 3, ToolName: "conformance_read", ToolCallID: "call-1", Content: continued.ToolResults[0].Output})
				_ = harness.WriteSSEFrame(w, harness.HarnessEventFrame{Version: harness.ProtocolVersion, Type: harness.FrameTurnCompleted, RuntimeSessionID: turn.request.RuntimeSessionID, TurnID: turn.request.TurnID, CorrelationID: turn.request.CorrelationID, Seq: 4, Completed: &harness.TurnCompleted{Result: "ok", FinalEventSeq: 4}})
			case <-time.After(2 * time.Second):
			}
			_ = harness.WriteSSEDone(w)
		case harness.TurnResourceContinue:
			var request harness.ContinueTurnRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				harness.WriteError(w, http.StatusBadRequest, "invalid JSON")
				return
			}
			turn.continued <- request
			harness.WriteJSON(w, http.StatusAccepted, harness.ContinueTurnResponse{Version: harness.ProtocolVersion, Accepted: true, RuntimeSessionID: request.RuntimeSessionID, TurnID: request.TurnID, CorrelationID: request.CorrelationID})
		default:
			harness.WriteError(w, http.StatusNotFound, "not found")
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	runtime, secret := testAgentRuntimeAndSecret(server.URL)
	runtime.Spec.Capabilities.ToolExecutionModes = []corev1alpha1.AgentRuntimeToolExecutionMode{
		corev1alpha1.AgentRuntimeToolExecutionModeBrokered,
	}
	runtime.Spec.Capabilities.BrokeredToolClasses = []corev1alpha1.AgentRuntimeBrokeredToolClass{
		corev1alpha1.AgentRuntimeBrokeredToolClassRead,
	}
	supportsContinuation := true
	runtime.Spec.Capabilities.SupportsContinuation = &supportsContinuation
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
	if updated.Status.ObservedCapabilities == nil || len(updated.Status.ObservedCapabilities.ToolExecutionModes) != 1 || updated.Status.ObservedCapabilities.ToolExecutionModes[0] != corev1alpha1.AgentRuntimeToolExecutionModeBrokered {
		t.Fatalf("ObservedCapabilities = %#v, want brokered-only", updated.Status.ObservedCapabilities)
	}
}

func TestAgentRuntimeReconcilerRequiresBrokeredReadConformanceWhenCapabilityRequested(t *testing.T) {
	turns := map[harness.HarnessTurnID]harness.StartTurnRequest{}
	mux := http.NewServeMux()
	mux.HandleFunc(harness.HealthPath, func(w http.ResponseWriter, r *http.Request) {
		harness.WriteJSON(w, http.StatusOK, harness.HealthResponse{Version: harness.ProtocolVersion, Status: harness.HealthStatusOK, Ready: true, CheckedAt: time.Now().UTC()})
	})
	mux.HandleFunc(harness.CapabilitiesPath, func(w http.ResponseWriter, r *http.Request) {
		harness.WriteJSON(w, http.StatusOK, harness.CapabilitiesResponse{
			Version:                 harness.ProtocolVersion,
			ProtocolVersion:         harness.ProtocolVersion,
			Transport:               harness.HTTPTransport,
			RuntimeName:             "fibey-runtime",
			ProviderKind:            harness.ProviderKindRemote,
			ToolExecutionModes:      []harness.ToolExecutionMode{harness.ToolExecutionModeObserved, harness.ToolExecutionModeBrokered},
			BrokeredToolClasses:     []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
			SupportsCancel:          true,
			SupportsRuntimeSessions: true,
			SupportsContinuation:    true,
		})
	})
	mux.HandleFunc(harness.TurnsPath, func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") != "x" {
			harness.WriteError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		var request harness.StartTurnRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			harness.WriteError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if _, exists := turns[request.TurnID]; exists {
			harness.WriteError(w, http.StatusConflict, "turn already exists")
			return
		}
		turns[request.TurnID] = request
		eventsPath, _ := harness.EventStreamPath(request.TurnID)
		harness.WriteJSON(w, http.StatusAccepted, harness.StartTurnResponse{Version: harness.ProtocolVersion, Accepted: true, RuntimeSessionID: request.RuntimeSessionID, TurnID: request.TurnID, CorrelationID: request.CorrelationID, EventStreamPath: eventsPath})
	})
	mux.HandleFunc(harness.TurnsPath+"/", func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") != "x" {
			harness.WriteError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		turnID, resource, err := harness.ParseTurnResourcePath(r.URL.EscapedPath())
		if err != nil {
			harness.WriteError(w, http.StatusNotFound, "not found")
			return
		}
		request := turns[turnID]
		switch resource {
		case harness.TurnResourceEvents:
			_ = harness.WriteSSEFrame(w, harness.HarnessEventFrame{Version: harness.ProtocolVersion, Type: harness.FrameTurnStarted, RuntimeSessionID: request.RuntimeSessionID, TurnID: request.TurnID, CorrelationID: request.CorrelationID, Seq: 1})
			if request.ToolExecutionMode == harness.ToolExecutionModeBrokered {
				_ = harness.WriteSSEFrame(w, harness.HarnessEventFrame{Version: harness.ProtocolVersion, Type: harness.FrameToolCallRequested, RuntimeSessionID: request.RuntimeSessionID, TurnID: request.TurnID, CorrelationID: request.CorrelationID, Seq: 2, ToolName: "read_incident", ToolCallID: "call-1", Content: json.RawMessage(`{"probe":true}`)})
				return
			}
			_ = harness.WriteSSEFrame(w, harness.HarnessEventFrame{Version: harness.ProtocolVersion, Type: harness.FrameTurnCompleted, RuntimeSessionID: request.RuntimeSessionID, TurnID: request.TurnID, CorrelationID: request.CorrelationID, Seq: 2, Completed: &harness.TurnCompleted{Result: "ok", FinalEventSeq: 2}})
			_ = harness.WriteSSEDone(w)
		case harness.TurnResourceCancel:
			harness.WriteJSON(w, http.StatusAccepted, harness.CancelTurnResponse{Version: harness.ProtocolVersion, Accepted: true, RuntimeSessionID: request.RuntimeSessionID, TurnID: request.TurnID, CorrelationID: request.CorrelationID})
		default:
			harness.WriteError(w, http.StatusNotFound, "not found")
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	runtime, secret := testAgentRuntimeAndSecret(server.URL)
	runtime.Spec.Capabilities.ToolExecutionModes = []corev1alpha1.AgentRuntimeToolExecutionMode{corev1alpha1.AgentRuntimeToolExecutionModeObserved, corev1alpha1.AgentRuntimeToolExecutionModeBrokered}
	runtime.Spec.Capabilities.BrokeredToolClasses = []corev1alpha1.AgentRuntimeBrokeredToolClass{corev1alpha1.AgentRuntimeBrokeredToolClassRead}
	supportsContinuation := true
	runtime.Spec.Capabilities.SupportsContinuation = &supportsContinuation
	r := newAgentRuntimeUnitReconciler(t, runtime, secret)
	if _, err := r.Reconcile(context.Background(), reconcileRequestFor(runtime)); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var updated corev1alpha1.AgentRuntime
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(runtime), &updated); err != nil {
		t.Fatalf("Get AgentRuntime: %v", err)
	}
	if updated.Status.Ready {
		t.Fatalf("Ready = true, want false when brokered read continue path is broken")
	}
	if !strings.Contains(updated.Status.Message, "brokered") {
		t.Fatalf("Message = %q, want brokered conformance failure", updated.Status.Message)
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
	requireAuth := true
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

func TestAgentRuntimeReconcilerRejectsBearerSecretWithoutEndpointBinding(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{AuthToken: "x"})
	defer server.Close()

	runtime, secret := testAgentRuntimeAndSecret(server.URL())
	secret.Annotations = nil
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
	if !strings.Contains(updated.Status.Message, agentRuntimeAuthEndpointAnnotation) {
		t.Fatalf("Message = %q, want missing endpoint binding annotation", updated.Status.Message)
	}
}

func TestAgentRuntimeReconcilerRejectsBearerSecretEndpointMismatch(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{AuthToken: "x"})
	defer server.Close()

	runtime, secret := testAgentRuntimeAndSecret(server.URL())
	secret.Annotations[agentRuntimeAuthEndpointAnnotation] = "http://different-runtime.default.svc.cluster.local:8080"
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
	if !strings.Contains(updated.Status.Message, "annotated for endpoint") {
		t.Fatalf("Message = %q, want endpoint mismatch", updated.Status.Message)
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
	previousAllowLoopback := agentRuntimeAllowInsecureLoopbackForTests
	previousClusterDomain := agentRuntimeClusterDomainForTests
	agentRuntimeAllowInsecureLoopbackForTests = true
	if previousClusterDomain == "" {
		agentRuntimeClusterDomainForTests = testAgentRuntimeClusterDomain
	}
	t.Cleanup(func() {
		agentRuntimeAllowInsecureLoopbackForTests = previousAllowLoopback
		agentRuntimeClusterDomainForTests = previousClusterDomain
	})
	scheme := newTestScheme()
	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.AgentRuntime{}).
		WithObjects(objs...).
		Build()
	return &AgentRuntimeReconciler{Client: fc, APIReader: fc, Scheme: scheme}
}

func testAgentRuntimeAndSecret(endpoint string) (*corev1alpha1.AgentRuntime, *corev1.Secret) {
	supportsCancel := true
	supportsSessions := true
	runtime := &corev1alpha1.AgentRuntime{
		ObjectMeta: metav1.ObjectMeta{Name: "fibey-agentkit", Namespace: "default", Generation: 1},
		Spec: corev1alpha1.AgentRuntimeRegistrySpec{
			ContractVersion: corev1alpha1.AgentRuntimeContractHarnessV1,
			Deployment: corev1alpha1.AgentRuntimeDeploymentSpec{
				Mode:              corev1alpha1.AgentRuntimeDeploymentModeExternalEndpoint,
				Endpoint:          endpoint,
				TransportSecurity: testAgentRuntimeTransportSecurity(endpoint),
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
		ObjectMeta: metav1.ObjectMeta{Name: "fibey-agentkit-harness-token", Namespace: "default", Labels: map[string]string{agentRuntimeAuthUseLabel: scheduledRunLabelValue, agentRuntimeAuthRefNameLabel: "fibey-agentkit"}, Annotations: map[string]string{agentRuntimeAuthEndpointAnnotation: endpoint}},
		Data:       map[string][]byte{"token": []byte("x")},
	}
	return runtime, secret
}

func agentRuntimeInsecureServiceBackends(namespace string, port int32, podIPs ...string) (*corev1.Service, []*corev1.Pod) {
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "fibey-agentkit", Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "fibey-agentkit"},
			Ports: []corev1.ServicePort{{
				Name:     "http",
				Protocol: corev1.ProtocolTCP,
				Port:     port,
			}},
		},
	}
	pods := make([]*corev1.Pod, 0, len(podIPs))
	for i, podIP := range podIPs {
		pods = append(pods, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("fibey-agentkit-%d", i),
				Namespace: namespace,
				UID:       types.UID(fmt.Sprintf("fibey-agentkit-%d-uid", i)),
				Labels:    map[string]string{"app": "fibey-agentkit"},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "runtime", Image: "runtime:test"}}},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				PodIP: podIP,
				Conditions: []corev1.PodCondition{{
					Type:   corev1.PodReady,
					Status: corev1.ConditionTrue,
				}},
			},
		})
	}
	return service, pods
}

func testAgentRuntimeTransportSecurity(endpoint string) corev1alpha1.AgentRuntimeTransportSecurity {
	if strings.HasPrefix(strings.TrimSpace(endpoint), "http://") {
		return corev1alpha1.AgentRuntimeTransportSecurityInsecureClusterLocalHTTP
	}
	return corev1alpha1.AgentRuntimeTransportSecurityTLS
}

func TestValidateAgentRuntimeSpecEnforcesTransportSecuritySchemePairing(t *testing.T) {
	runtime, _ := testAgentRuntimeAndSecret("http://runtime.default.svc.cluster.local:8080")

	runtime.Spec.Deployment.TransportSecurity = ""
	if err := validateAgentRuntimeSpec(runtime); err == nil || !strings.Contains(err.Error(), `transportSecurity is "tls"`) {
		t.Fatalf("validateAgentRuntimeSpec(default tls with http) = %v, want https requirement", err)
	}

	runtime.Spec.Deployment.TransportSecurity = corev1alpha1.AgentRuntimeTransportSecurityInsecureClusterLocalHTTP
	if err := validateAgentRuntimeSpec(runtime); err != nil {
		t.Fatalf("validateAgentRuntimeSpec(insecure http) error = %v", err)
	}

	runtime.Spec.Deployment.Endpoint = "https://runtime.example.com"
	if err := validateAgentRuntimeSpec(runtime); err == nil || !strings.Contains(err.Error(), "must use http") {
		t.Fatalf("validateAgentRuntimeSpec(insecure https) = %v, want http requirement", err)
	}

	runtime.Spec.Deployment.TransportSecurity = ""
	if err := validateAgentRuntimeSpec(runtime); err != nil {
		t.Fatalf("validateAgentRuntimeSpec(default tls with https) error = %v", err)
	}

	runtime.Spec.Deployment.Endpoint = "https://user:pass@runtime.example.com"
	if err := validateAgentRuntimeSpec(runtime); err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("validateAgentRuntimeSpec(credentials) = %v, want credentials rejection", err)
	}
}

func TestAgentRuntimeEndpointPolicyRequiresExplicitSameNamespaceServiceOptIn(t *testing.T) {
	runtime, secret := testAgentRuntimeAndSecret("http://runtime.example.com")
	r := newAgentRuntimeUnitReconciler(t, runtime, secret)
	if err := r.validateAgentRuntimeEndpointPolicy(context.Background(), runtime); err == nil || !strings.Contains(err.Error(), "same-namespace") {
		t.Fatalf("validateAgentRuntimeEndpointPolicy(external http with opt-in) = %v, want same-namespace Service requirement", err)
	}

	runtime.Spec.Deployment.TransportSecurity = ""
	if err := r.validateAgentRuntimeEndpointPolicy(context.Background(), runtime); err == nil || !strings.Contains(err.Error(), `transportSecurity is "tls"`) {
		t.Fatalf("validateAgentRuntimeEndpointPolicy(http without opt-in) = %v, want tls default rejection", err)
	}
	runtime.Spec.Deployment.TransportSecurity = corev1alpha1.AgentRuntimeTransportSecurityInsecureClusterLocalHTTP

	runtime.Spec.Deployment.Endpoint = "http://127.0.0.1:8080"
	agentRuntimeAllowInsecureLoopbackForTests = false
	if err := r.validateAgentRuntimeEndpointPolicy(context.Background(), runtime); err == nil || !strings.Contains(err.Error(), "same-namespace") {
		t.Fatalf("validateAgentRuntimeEndpointPolicy(loopback) = %v, want same-namespace Service requirement", err)
	}
	agentRuntimeAllowInsecureLoopbackForTests = true

	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "default"}, Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "runtime"}}}
	r = newAgentRuntimeUnitReconciler(t, runtime, secret, service)
	runtime.Spec.Deployment.Endpoint = "http://runtime.default.svc.cluster.local:8080"
	if err := r.validateAgentRuntimeEndpointPolicy(context.Background(), runtime); err != nil {
		t.Fatalf("validateAgentRuntimeEndpointPolicy(cluster-local) error = %v", err)
	}
	runtime.Spec.Deployment.Endpoint = "http://runtime:8080"
	if err := r.validateAgentRuntimeEndpointPolicy(context.Background(), runtime); err == nil || !strings.Contains(err.Error(), "same-namespace") {
		t.Fatalf("validateAgentRuntimeEndpointPolicy(short-service) = %v, want qualified same-namespace Service requirement", err)
	}
	runtime.Spec.Deployment.Endpoint = "http://runtime.default:8080"
	if err := r.validateAgentRuntimeEndpointPolicy(context.Background(), runtime); err == nil || !strings.Contains(err.Error(), "same-namespace") {
		t.Fatalf("validateAgentRuntimeEndpointPolicy(service-namespace) = %v, want rooted Service FQDN requirement", err)
	}
	runtime.Spec.Deployment.Endpoint = "http://runtime.default.svc:8080"
	if err := r.validateAgentRuntimeEndpointPolicy(context.Background(), runtime); err == nil || !strings.Contains(err.Error(), "same-namespace") {
		t.Fatalf("validateAgentRuntimeEndpointPolicy(partial Service DNS) = %v, want rooted Service FQDN requirement", err)
	}

	selectorlessService := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "selectorless-runtime", Namespace: "default"}}
	externalNameService := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "external-runtime", Namespace: "default"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "runtime.example.com"},
	}
	r = newAgentRuntimeUnitReconciler(t, runtime, secret, service, selectorlessService, externalNameService)
	runtime.Spec.Deployment.Endpoint = "http://selectorless-runtime.default.svc.cluster.local:8080"
	if err := r.validateAgentRuntimeEndpointPolicy(context.Background(), runtime); err == nil || !strings.Contains(err.Error(), "non-empty selector") {
		t.Fatalf("validateAgentRuntimeEndpointPolicy(selectorless service) = %v, want selector requirement", err)
	}
	runtime.Spec.Deployment.Endpoint = "http://external-runtime.default.svc.cluster.local:8080"
	if err := r.validateAgentRuntimeEndpointPolicy(context.Background(), runtime); err == nil || !strings.Contains(err.Error(), "ExternalName") {
		t.Fatalf("validateAgentRuntimeEndpointPolicy(externalName) = %v, want ExternalName rejection", err)
	}
	runtime.Spec.Deployment.Endpoint = "http://missing.default.svc.cluster.local:8080"
	if err := r.validateAgentRuntimeEndpointPolicy(context.Background(), runtime); err == nil || !strings.Contains(err.Error(), "to exist") {
		t.Fatalf("validateAgentRuntimeEndpointPolicy(missing service) = %v, want Service existence requirement", err)
	}
	runtime.Spec.Deployment.Endpoint = "http://runtime.dev:8080"
	if err := r.validateAgentRuntimeEndpointPolicy(context.Background(), runtime); err == nil || !strings.Contains(err.Error(), "same-namespace") {
		t.Fatalf("validateAgentRuntimeEndpointPolicy(other namespace) = %v, want same-namespace rejection", err)
	}
	runtime.Spec.Deployment.Endpoint = "http://runtime.svc.attacker.com"
	if err := r.validateAgentRuntimeEndpointPolicy(context.Background(), runtime); err == nil || !strings.Contains(err.Error(), "same-namespace") {
		t.Fatalf("validateAgentRuntimeEndpointPolicy(svc-looking external) = %v, want same-namespace rejection", err)
	}
	runtime.Spec.Deployment.Endpoint = "http://10.0.0.5:8080"
	if err := r.validateAgentRuntimeEndpointPolicy(context.Background(), runtime); err == nil || !strings.Contains(err.Error(), "same-namespace") {
		t.Fatalf("validateAgentRuntimeEndpointPolicy(private IP) = %v, want same-namespace rejection", err)
	}
}

func TestAgentRuntimeRequestEndpointPreservesAuthorityForInsecureServiceDNS(t *testing.T) {
	previousClusterDomain := agentRuntimeClusterDomainForTests
	agentRuntimeClusterDomainForTests = testAgentRuntimeClusterDomain
	t.Cleanup(func() { agentRuntimeClusterDomainForTests = previousClusterDomain })
	got, security, err := agentRuntimeRequestEndpoint(
		"http://runtime.default.svc.cluster.local:8080/harness",
		corev1alpha1.AgentRuntimeTransportSecurityInsecureClusterLocalHTTP,
	)
	if err != nil {
		t.Fatalf("agentRuntimeRequestEndpoint() error = %v", err)
	}
	if security != corev1alpha1.AgentRuntimeTransportSecurityInsecureClusterLocalHTTP {
		t.Fatalf("security = %q, want insecure mode", security)
	}
	if got != "http://runtime.default.svc.cluster.local:8080/harness" {
		t.Fatalf("request endpoint = %q, want configured HTTP authority preserved", got)
	}
}

func TestAgentRuntimeEndpointPolicyUsesConfiguredClusterDomain(t *testing.T) {
	previousClusterDomain := agentRuntimeClusterDomainForTests
	agentRuntimeClusterDomainForTests = "corp.internal"
	t.Cleanup(func() { agentRuntimeClusterDomainForTests = previousClusterDomain })
	runtime, secret := testAgentRuntimeAndSecret("http://runtime.default.svc.corp.internal:8080")
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "runtime", Namespace: "default"},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": "runtime"}},
	}
	r := newAgentRuntimeUnitReconciler(t, runtime, secret, service)
	if err := r.validateAgentRuntimeEndpointPolicy(context.Background(), runtime); err != nil {
		t.Fatalf("validateAgentRuntimeEndpointPolicy(custom cluster domain) error = %v", err)
	}
}

func TestDiscoverAgentRuntimeClusterDomain(t *testing.T) {
	for _, tc := range []struct {
		name       string
		searchLine string
		want       string
	}{
		{name: "ordinary namespace", searchLine: "search orka-system.svc.corp.internal svc.corp.internal corp.internal\n", want: "corp.internal"},
		{name: "namespace named svc", searchLine: "search svc.svc.cluster.local svc.cluster.local cluster.local\n", want: "cluster.local"},
		{name: "cluster domain starts with svc", searchLine: "search orka-system.svc.svc.corp svc.svc.corp svc.corp\n", want: "svc.corp"},
		{name: "overlapping suffix candidates", searchLine: "search orka-system.svc.svc.corp svc.svc.corp svc.corp corp\n", want: ""},
		{name: "unrelated search domains", searchLine: "search corp.example example\n", want: ""},
		{name: "tuple split across directives", searchLine: "search tenant.svc.evil.example\nsearch svc.evil.example evil.example\n", want: ""},
		{name: "conflicting tuples", searchLine: "search ns.svc.cluster.local svc.cluster.local cluster.local\nsearch ns.svc.evil.example svc.evil.example evil.example\n", want: ""},
		{name: "conflicting tuples on one line", searchLine: "search ns.svc.evil.example svc.evil.example evil.example ns.svc.cluster.local svc.cluster.local cluster.local\n", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolvConf := filepath.Join(t.TempDir(), "resolv.conf")
			if err := os.WriteFile(resolvConf, []byte(tc.searchLine), 0o600); err != nil {
				t.Fatalf("write resolv.conf: %v", err)
			}
			if got := discoverAgentRuntimeClusterDomain(resolvConf); got != tc.want {
				t.Fatalf("cluster domain = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateAgentRuntimeExecutableCapabilitiesRejectsUnknownBrokeredClass(t *testing.T) {
	err := validateAgentRuntimeExecutableCapabilities(&harness.CapabilitiesResponse{
		RuntimeName:             "runtime-a",
		ToolExecutionModes:      []harness.ToolExecutionMode{harness.ToolExecutionModeBrokered},
		BrokeredToolClasses:     []harness.BrokeredToolClass{harness.BrokeredToolClass("admin")},
		SupportsRuntimeSessions: true,
		SupportsContinuation:    true,
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported brokeredToolClass") {
		t.Fatalf("validateAgentRuntimeExecutableCapabilities() error = %v, want brokered class rejection", err)
	}
}

func TestValidateAgentRuntimeExecutableCapabilitiesRequiresBrokeredContinuation(t *testing.T) {
	err := validateAgentRuntimeExecutableCapabilities(&harness.CapabilitiesResponse{
		RuntimeName:             "runtime-a",
		ToolExecutionModes:      []harness.ToolExecutionMode{harness.ToolExecutionModeObserved, harness.ToolExecutionModeBrokered},
		BrokeredToolClasses:     []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
		SupportsCancel:          true,
		SupportsRuntimeSessions: true,
	})
	if err == nil || !strings.Contains(err.Error(), "supportsContinuation") {
		t.Fatalf("validateAgentRuntimeExecutableCapabilities() error = %v, want continuation requirement", err)
	}
}

func TestValidateObservedHarnessCapabilitiesRequiresCancelAndSessions(t *testing.T) {
	err := validateObservedHarnessCapabilities(&harness.CapabilitiesResponse{
		RuntimeName:             "runtime-a",
		ToolExecutionModes:      []harness.ToolExecutionMode{harness.ToolExecutionModeObserved},
		SupportsRuntimeSessions: true,
	})
	if err == nil || !strings.Contains(err.Error(), "supportsCancel") {
		t.Fatalf("validateObservedHarnessCapabilities() error = %v, want supportsCancel requirement", err)
	}
	err = validateObservedHarnessCapabilities(&harness.CapabilitiesResponse{
		RuntimeName:        "runtime-a",
		ToolExecutionModes: []harness.ToolExecutionMode{harness.ToolExecutionModeObserved},
		SupportsCancel:     true,
	})
	if err == nil || !strings.Contains(err.Error(), "supportsRuntimeSessions") {
		t.Fatalf("validateObservedHarnessCapabilities() error = %v, want supportsRuntimeSessions requirement", err)
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

func TestValidateObservedHarnessCapabilitiesRequiresObservedMode(t *testing.T) {
	err := validateObservedHarnessCapabilities(&harness.CapabilitiesResponse{
		RuntimeName:        "runtime-a",
		ToolExecutionModes: []harness.ToolExecutionMode{harness.ToolExecutionModeBrokered},
	})
	if err == nil || !strings.Contains(err.Error(), "observed") {
		t.Fatalf("validateObservedHarnessCapabilities() error = %v, want observed mode requirement", err)
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
