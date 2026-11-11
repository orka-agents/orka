// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	workspaceprovider "github.com/orka-agents/orka-workspace/sdk"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/workspace/daemonprotocol"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const routedTestRouterURL = "https://atenet-router.ate-system.svc:443"

// routedRecordingTransport stands in for the controller's pod-certificate
// router transport. The challenge read answers 404 (bootstrap already closed)
// so startup reaches the authenticated probe; everything else stops here.
type routedRecordingTransport struct{ requests []*http.Request }

func (t *routedRecordingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.requests = append(t.requests, request.Clone(request.Context()))
	if request.Method == http.MethodGet && request.URL.Path == harnessv2.CredentialBootstrapPath {
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: request}, nil
	}
	return nil, errors.New("fixture stops at the router")
}

func (f *externalRuntimePoolFixture) advertiseSubstrateV1Backend(t *testing.T) {
	t.Helper()
	provider := &workspacev1alpha1.ExecutionWorkspaceProvider{}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.provider), provider); err != nil {
		t.Fatal(err)
	}
	provider.Status.Backend = &workspacev1alpha1.ExecutionWorkspaceBackendStatus{Version: "b34fbee7", APIVersions: []string{"ateapi.v1.0.0"}}
	provider.Status.SupportedFeatures = append(provider.Status.SupportedFeatures, workspacev1alpha1.WorkspaceFeatureEgressAllowlist)
	if err := f.r.Update(t.Context(), provider); err != nil {
		t.Fatal(err)
	}
	// A native runtime on this backend resolves only fully qualified names.
	f.r.ControllerAPIURL = "http://orka-api.default.svc.cluster.local:8080"
	f.r.ProviderProxy.BaseURL = "http://provider-auth-proxy.orka-system.svc.cluster.local:8080"
}

// useShortServiceOrigins configures Core's origins with the short
// "<service>.<namespace>.svc" names that Pods resolve through their search path.
func (f *externalRuntimePoolFixture) useShortServiceOrigins() {
	f.r.ControllerAPIURL = "http://orka-api.default.svc:8080"
	f.r.ProviderProxy.BaseURL = "http://provider-auth-proxy.orka-system.svc:8080"
}

type routedRuntimeFixture struct {
	*externalRuntimePoolFixture
	router *routedRecordingTransport
	direct *nativeIngressReviewTransport
	worker corev1.Pod
}

func newRoutedRuntimeFixture(t *testing.T, substrateV1 bool, mutate func(*workspacev1alpha1.AllocationObservation)) *routedRuntimeFixture {
	t.Helper()
	f := newExternalRuntimePoolFixture(t)
	f.advertiseNativeProcess(t)
	if substrateV1 {
		f.advertiseSubstrateV1Backend(t)
	}
	router := &routedRecordingTransport{}
	f.r.SubstrateRouter = &RuntimePoolSubstrateRouter{URL: routedTestRouterURL, HTTPClient: &http.Client{Transport: router}}
	direct := &nativeIngressReviewTransport{}
	f.r.HTTPClient = &http.Client{Transport: direct}
	w, worker := f.materialize(t)
	a := w.Status.Allocation
	a.Identity.InstanceID = "routed-actor-uid"
	a.Startup.Identity = a.Identity
	a.Startup.Pod = nil
	a.Startup.Endpoint = routedTestRouterURL
	a.Startup.Process = &workspacev1alpha1.NativeProcessEvidence{Namespace: "orka-atespace", Name: "actor-1", UID: a.Identity.InstanceID, Version: 1, RouterTarget: "orka-atespace/actor-1",
		Worker: workspacev1alpha1.PodReference{Namespace: worker.Namespace, Name: worker.Name, UID: worker.UID}, ChallengeSHA256: "sha256:" + strings.Repeat("a", 64)}
	if mutate != nil {
		mutate(a)
	}
	if err := f.r.Status().Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	return &routedRuntimeFixture{externalRuntimePoolFixture: f, router: router, direct: direct, worker: worker}
}

func TestExternalRuntimeEgressAdmitsCoreOriginsAndRejectsInexpressibleOnes(t *testing.T) {
	r := &RuntimePoolReconciler{ControllerAPIURL: "https://Orka-API.orka-system.svc.cluster.local"}
	cfg := runtimePoolConfig{providerProxy: runtimePoolProviderProxyConfig{baseURL: "http://provider-auth-proxy.orka-system.svc.cluster.local:8080"}}
	policy, err := r.externalRuntimeEgress(cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := []workspacev1alpha1.RuntimeEgressRule{
		{Protocol: workspacev1alpha1.RuntimeEgressTLSPassthrough, Host: "orka-api.orka-system.svc.cluster.local", Ports: []int32{443}},
		{Protocol: workspacev1alpha1.RuntimeEgressHTTP, Host: "provider-auth-proxy.orka-system.svc.cluster.local", Ports: []int32{8080}},
	}
	if !reflect.DeepEqual(policy.Rules, want) {
		t.Fatalf("egress rules = %#v, want %#v", policy.Rules, want)
	}
	// A cluster domain other than cluster.local is the operator's to choose.
	r.ControllerAPIURL = "https://orka-api.orka-system.svc.corp.example"
	if _, err := r.externalRuntimeEgress(cfg); err != nil {
		t.Fatalf("custom cluster domain refused: %v", err)
	}

	// One origin serving both roles yields one grant.
	r.ControllerAPIURL = "http://provider-auth-proxy.orka-system.svc.cluster.local:8080/api"
	if policy, err = r.externalRuntimeEgress(cfg); err != nil || len(policy.Rules) != 1 {
		t.Fatalf("shared origin = %#v, %v", policy, err)
	}
	for name, controllerURL := range map[string]string{
		"conflicting protocol": "https://provider-auth-proxy.orka-system.svc.cluster.local:8080",
		"IP address":           "http://10.0.0.10:8080",
		"single-label host":    "http://orka-api:8080",
		"unsupported scheme":   "ftp://orka-api.orka-system.svc.cluster.local",
	} {
		r.ControllerAPIURL = controllerURL
		if _, err := r.externalRuntimeEgress(cfg); err == nil {
			t.Errorf("%s: inexpressible controller origin was admitted", name)
		}
	}
}

func TestExternalRuntimeEgressAdmitsTelemetryCollectorOnlyWhenOptedIn(t *testing.T) {
	values := map[string]string{
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT":  "https://Traces.Otel.svc.cluster.local/v1/traces",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://metrics.otel.svc.cluster.local:4318/v1/metrics",
		"OTEL_EXPORTER_OTLP_HEADERS":          "authorization=private",
	}
	getenv := func(name string) string { return values[name] }
	cfg := runtimePoolConfig{providerProxy: runtimePoolProviderProxyConfig{baseURL: "http://provider-auth-proxy.orka-system.svc.cluster.local:8080"}}
	core := []workspacev1alpha1.RuntimeEgressRule{
		{Protocol: workspacev1alpha1.RuntimeEgressTLSPassthrough, Host: "orka-api.orka-system.svc.cluster.local", Ports: []int32{443}},
		{Protocol: workspacev1alpha1.RuntimeEgressHTTP, Host: "provider-auth-proxy.orka-system.svc.cluster.local", Ports: []int32{8080}},
	}
	for _, test := range []struct {
		name               string
		telemetry, collect bool
	}{
		{"default", false, false},
		{"telemetry without opt-in", true, false},
		{"opt-in without telemetry", false, true},
	} {
		r := &RuntimePoolReconciler{ControllerAPIURL: "https://orka-api.orka-system.svc.cluster.local", EnableTelemetry: test.telemetry, TelemetryCollectorEgress: test.collect}
		policy, err := r.externalRuntimeEgressWithEnv(cfg, getenv)
		if err != nil || !reflect.DeepEqual(policy.Rules, core) {
			t.Fatalf("%s: egress rules = %#v, %v", test.name, policy, err)
		}
	}

	r := &RuntimePoolReconciler{ControllerAPIURL: "https://orka-api.orka-system.svc.cluster.local", EnableTelemetry: true, TelemetryCollectorEgress: true}
	policy, err := r.externalRuntimeEgressWithEnv(cfg, getenv)
	if err != nil {
		t.Fatal(err)
	}
	want := []workspacev1alpha1.RuntimeEgressRule{
		{Protocol: workspacev1alpha1.RuntimeEgressHTTP, Host: "metrics.otel.svc.cluster.local", Ports: []int32{4318}},
		core[0], core[1],
		{Protocol: workspacev1alpha1.RuntimeEgressTLSPassthrough, Host: "traces.otel.svc.cluster.local", Ports: []int32{443}},
	}
	if !reflect.DeepEqual(policy.Rules, want) {
		t.Fatalf("egress rules = %#v, want %#v", policy.Rules, want)
	}

	// The opt-in fails closed: an inexpressible projected collector refuses
	// admission instead of silently dropping or widening the allowlist.
	values["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"] = "http://10.0.0.7:4318/v1/traces"
	if _, err := r.externalRuntimeEgressWithEnv(cfg, getenv); err == nil || !strings.Contains(err.Error(), "telemetry collector") {
		t.Fatalf("IP collector admitted: %v", err)
	}
	values["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"] = "https://traces.otel.svc/v1/traces"
	if _, err := r.externalRuntimeEgressWithEnv(cfg, getenv); err == nil || !strings.Contains(err.Error(), "short Kubernetes Service name") {
		t.Fatalf("short-named collector admitted: %v", err)
	}
	values["OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"] = "https://provider-auth-proxy.orka-system.svc.cluster.local:8080/v1/traces"
	if _, err := r.externalRuntimeEgressWithEnv(cfg, getenv); err == nil {
		t.Fatal("collector sharing a core origin with a conflicting protocol was admitted")
	}
	// Without a projected collector there is nothing to admit.
	clear(values)
	if policy, err = r.externalRuntimeEgressWithEnv(cfg, getenv); err != nil || !reflect.DeepEqual(policy.Rules, core) {
		t.Fatalf("no collector: egress rules = %#v, %v", policy, err)
	}
}

// A native runtime behind the Substrate egress gateway cannot resolve a short
// "<service>.<namespace>.svc" name: its resolver's search suffixes are refused
// and cluster DNS answers only the fully qualified form. Admission refuses such
// an origin and names the flag and the form to use.
func TestExternalRuntimeEgressRefusesShortServiceNames(t *testing.T) {
	const (
		controllerFQDN = "http://orka-api.orka-system.svc.cluster.local:8080"
		proxyFQDN      = "http://orka-provider-auth-proxy.orka-system.svc.cluster.local:8080"
	)
	for _, tc := range []struct {
		name, controllerURL, proxyURL, flag, host string
	}{
		{"controller URL", "http://orka-api.orka-system.svc:8080", proxyFQDN, "--controller-url", "orka-api.orka-system.svc"},
		{"controller URL in another case", "https://Orka-API.orka-system.SVC", proxyFQDN, "--controller-url", "orka-api.orka-system.svc"},
		{"provider proxy URL", controllerFQDN, "http://orka-provider-auth-proxy.orka-system.svc:8080", "--acp-provider-proxy-base-url", "orka-provider-auth-proxy.orka-system.svc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &RuntimePoolReconciler{ControllerAPIURL: tc.controllerURL}
			_, err := r.externalRuntimeEgress(runtimePoolConfig{providerProxy: runtimePoolProviderProxyConfig{baseURL: tc.proxyURL}})
			if err == nil {
				t.Fatal("a short Service name was admitted to a native egress allowlist")
			}
			for _, want := range []string{tc.flag, `"` + tc.host + `"`, `"` + tc.host + `.cluster.local"`} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not name %s", err, want)
				}
			}
		})
	}
}

func TestExternalRuntimeNetworkShapeFollowsProviderAndStaysFrozen(t *testing.T) {
	v1 := &workspacev1alpha1.ExecutionWorkspaceProvider{Status: workspacev1alpha1.ExecutionWorkspaceProviderStatus{
		SupportedFeatures: []workspacev1alpha1.ExecutionWorkspaceFeature{workspacev1alpha1.WorkspaceFeatureEgressAllowlist},
	}}
	// A backend version string alone never selects the egress shape.
	legacy := &workspacev1alpha1.ExecutionWorkspaceProvider{Status: workspacev1alpha1.ExecutionWorkspaceProviderStatus{
		Backend: &workspacev1alpha1.ExecutionWorkspaceBackendStatus{APIVersions: []string{"ateapi.v1.0.0"}},
	}}
	for _, test := range []struct {
		name     string
		provider *workspacev1alpha1.ExecutionWorkspaceProvider
		native   bool
		want     bool
	}{
		{"v1 native", v1, true, true},
		{"v1 Pod-backed", v1, false, false},
		{"legacy native", legacy, true, false},
		{"no backend native", &workspacev1alpha1.ExecutionWorkspaceProvider{}, true, false},
	} {
		got, err := externalRuntimeUsesEgressAllowlist(test.provider, test.native, nil)
		if err != nil || got != test.want {
			t.Errorf("%s: egress = %v, %v; want %v", test.name, got, err, test.want)
		}
	}
	egress := &workspacev1alpha1.WorkloadRequest{Runtime: &workspacev1alpha1.RuntimeWorkload{Egress: &workspacev1alpha1.RuntimeEgressPolicy{}}}
	network := &workspacev1alpha1.WorkloadRequest{Runtime: &workspacev1alpha1.RuntimeWorkload{NetworkPolicy: &networkingv1.NetworkPolicySpec{}}}
	if got, err := externalRuntimeUsesEgressAllowlist(v1, true, egress); err != nil || !got {
		t.Fatalf("same-shape successor refused: %v", err)
	}
	if _, err := externalRuntimeUsesEgressAllowlist(v1, true, network); err == nil {
		t.Fatal("successor silently replaced an admitted NetworkPolicy with an egress allowlist")
	}
	if _, err := externalRuntimeUsesEgressAllowlist(legacy, true, egress); err == nil {
		t.Fatal("successor silently replaced an admitted egress allowlist with a NetworkPolicy")
	}
	if _, err := externalRuntimeUsesEgressAllowlist(v1, true, &workspacev1alpha1.WorkloadRequest{Runtime: &workspacev1alpha1.RuntimeWorkload{}}); err == nil {
		t.Fatal("predecessor without an admitted network shape was accepted")
	}
}

func TestExternalRuntimePoolPublishesEgressAllowlistForSubstrateV1Native(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	f.advertiseNativeProcess(t)
	f.advertiseSubstrateV1Backend(t)
	w := f.publish(t)
	if err := workspaceprovider.ValidateWorkspaceWorkload(w); err != nil {
		t.Fatal(err)
	}
	runtime := w.Spec.Workload.Runtime
	if runtime.NetworkPolicy != nil || runtime.Egress == nil || runtime.BootstrapPort != workspacev1alpha1.NativeRouterTargetPort {
		t.Fatalf("Substrate v1 native request network = %#v / %#v, port %d", runtime.NetworkPolicy, runtime.Egress, runtime.BootstrapPort)
	}
	want := []workspacev1alpha1.RuntimeEgressRule{
		{Protocol: workspacev1alpha1.RuntimeEgressHTTP, Host: "orka-api.default.svc.cluster.local", Ports: []int32{8080}},
		{Protocol: workspacev1alpha1.RuntimeEgressHTTP, Host: "provider-auth-proxy.orka-system.svc.cluster.local", Ports: []int32{8080}},
	}
	if !reflect.DeepEqual(runtime.Egress.Rules, want) {
		t.Fatalf("admitted egress = %#v, want provider proxy and controller API origins", runtime.Egress.Rules)
	}
}

// Admission fails closed: the pool reports the misconfiguration and publishes
// no request, rather than starting a runtime that can reach nothing.
func TestExternalRuntimePoolRefusesShortServiceNamesForSubstrateV1Native(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	f.advertiseNativeProcess(t)
	f.advertiseSubstrateV1Backend(t)
	f.useShortServiceOrigins()
	for range 6 {
		runtimePoolReconcile(t, f.r, f.pool)
	}
	if f.currentWorkspace(t).Spec.Workload != nil {
		t.Fatal("a workload request was published with a short Service name in its egress allowlist")
	}
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleDegraded || !strings.Contains(pool.Status.Message, "set --acp-provider-proxy-base-url to the fully qualified name") {
		t.Fatalf("pool status = %s %q, want Degraded naming the flag to fix", pool.Status.Lifecycle, pool.Status.Message)
	}
}

func TestExternalRuntimePoolKeepsNetworkPolicyForPodBackedSubstrateV1Provider(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	f.advertiseSubstrateV1Backend(t)
	// Pods resolve short Service names through their search path.
	f.useShortServiceOrigins()
	runtime := f.publish(t).Spec.Workload.Runtime
	if runtime.Egress != nil || runtime.NetworkPolicy == nil || len(runtime.NetworkPolicy.Ingress) == 0 {
		t.Fatal("Pod-backed request lost its admitted NetworkPolicy")
	}
}

func TestExternalRoutedStartupUsesOnlyConfiguredRouterWithTarget(t *testing.T) {
	f := newRoutedRuntimeFixture(t, true, nil)
	w := f.currentWorkspace(t)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if err := f.r.bindExternalRuntimeInstanceEvidence(t.Context(), &pool, w); err != nil {
		t.Fatal(err)
	}
	attested, err := f.r.attestExternalWorkspaceStartup(t.Context(), w.Spec.Workload, w.Status.Allocation.Startup)
	if err != nil {
		t.Fatal(err)
	}
	f.supervisor.probe = runtimePoolValidProbe(&pool, attested, "routed-boot", false)
	for range 3 {
		runtimePoolReconcile(t, f.r, f.pool)
	}
	pool = runtimePoolTestGetPool(t, f.r, f.pool)
	if pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing {
		t.Fatalf("routed runtime did not serve: %s: %s", pool.Status.Lifecycle, pool.Status.Message)
	}
	if f.direct.calls != 0 || len(f.router.requests) == 0 {
		t.Fatalf("direct=%d routed=%d; want only routed calls", f.direct.calls, len(f.router.requests))
	}
	for _, request := range f.router.requests {
		if request.URL.Host != "atenet-router.ate-system.svc:443" || request.URL.Path != harnessv2.CredentialBootstrapPath ||
			request.Header.Get(daemonprotocol.TargetActorHeader) != "orka-atespace/actor-1" {
			t.Fatalf("routed bootstrap request = %s %s target %q", request.Method, request.URL, request.Header.Get(daemonprotocol.TargetActorHeader))
		}
	}
	evidence, err := externalRuntimeEvidence(&pool)
	if err != nil || evidence == nil || evidence.RouterTarget != "orka-atespace/actor-1" || evidence.Endpoint != routedTestRouterURL {
		t.Fatalf("bound evidence = %#v, %v", evidence, err)
	}
	// v1 has no direct worker ingress: Core creates no worker grants.
	if _, err := externalSavedIngressTargets(&pool); err != nil || pool.Annotations[externalProcessIngressTargetsAnnotation] != "" {
		t.Fatal("routed startup froze worker ingress targets")
	}
	policies := &networkingv1.NetworkPolicyList{}
	if err := f.r.List(t.Context(), policies); err != nil {
		t.Fatal(err)
	}
	for _, policy := range policies.Items {
		if strings.HasSuffix(policy.Name, "external-provider-ingress") || strings.HasSuffix(policy.Name, "external-controller-ingress") {
			t.Fatalf("routed startup created worker ingress policy %s", policy.Name)
		}
	}
	assertRoutedPoolTransports(t, f, &pool, w)
}

// Dispatcher, probe, drain and cleanup paths reuse the bound router target.
func assertRoutedPoolTransports(t *testing.T, f *routedRuntimeFixture, pool *corev1alpha1.RuntimePool, w *workspacev1alpha1.ExecutionWorkspace) {
	t.Helper()
	endpoint, err := runtimePoolWorkspaceStartupEndpoint(t.Context(), f.r.Client, pool)
	if err != nil || endpoint != routedTestRouterURL {
		t.Fatalf("dispatch endpoint = %q, %v", endpoint, err)
	}
	f.router.requests = nil
	routed, err := externalRoutedPoolHTTPClient(f.r.SubstrateRouter, pool, 0)
	if err != nil || routed == nil {
		t.Fatalf("dispatch transport = %v, %v", routed, err)
	}
	if harnessv2.TransportSupportsHTTPTrace(routed.Transport) != harnessv2.TransportSupportsHTTPTrace(f.r.SubstrateRouter.HTTPClient.Transport) {
		t.Fatal("routing wrapper changed request trace reliability")
	}
	f.r.SupervisorClient = nil
	_, _ = f.r.supervisorClientForPool(pool).Probe(t.Context(), endpoint, "token", []byte(strings.Repeat("s", 32)))
	if len(f.router.requests) != 1 || f.router.requests[0].URL.Path != harnessv2.CapabilitiesPath || f.router.requests[0].Header.Get(daemonprotocol.TargetActorHeader) != "orka-atespace/actor-1" {
		t.Fatalf("pool probe did not use the routed target: %d requests", len(f.router.requests))
	}
	if f.direct.calls != 0 {
		t.Fatal("pool probe dialed the router URL without the routed transport")
	}
	// Retirement drain derives the same route from exact startup evidence.
	f.router.requests = nil
	_, _ = f.r.externalStartupSupervisorClient(pool, w.Status.Allocation.Startup).Probe(t.Context(), endpoint, "token", []byte(strings.Repeat("s", 32)))
	for _, request := range f.router.requests {
		if request.Header.Get(daemonprotocol.TargetActorHeader) != "orka-atespace/actor-1" {
			t.Fatal("retirement drain lost the routed target")
		}
	}
	if len(f.router.requests) == 0 || f.direct.calls != 0 {
		t.Fatal("retirement drain did not use the routed transport")
	}

	// Egress-shaped cleanup has no frozen worker targets to resolve.
	cfg, err := f.r.runtimePoolConfigForDeletion(pool)
	if err != nil {
		t.Fatal(err)
	}
	cfg.namespace = w.Spec.Workload.Runtime.Template.Namespace
	if _, err := f.r.deleteExternalCoreNetworkPolicies(t.Context(), pool, cfg, f.currentWorkspace(t)); err != nil {
		t.Fatalf("egress-shaped cleanup failed: %v", err)
	}
}

func TestExternalRoutedStartupRejectsUnpinnedOrMismatchedEvidence(t *testing.T) {
	for _, test := range []struct {
		name        string
		substrateV1 bool
		mutate      func(*workspacev1alpha1.AllocationObservation)
		configure   func(*RuntimePoolReconciler)
	}{
		{name: "foreign router authority", substrateV1: true, mutate: func(a *workspacev1alpha1.AllocationObservation) {
			a.Startup.Endpoint = "https://attacker-router.example:443"
		}},
		{name: "router unconfigured", substrateV1: true, configure: func(r *RuntimePoolReconciler) { r.SubstrateRouter = nil }},
		{name: "target names another process", substrateV1: true, mutate: func(a *workspacev1alpha1.AllocationObservation) {
			a.Startup.Process.RouterTarget = "orka-atespace/actor-2"
		}},
		{name: "direct evidence for egress request", substrateV1: true, mutate: func(a *workspacev1alpha1.AllocationObservation) {
			a.Startup.Process.RouterTarget = ""
			a.Startup.Endpoint = "http://native-router.example:80/process"
		}},
		{name: "routed evidence for NetworkPolicy request", substrateV1: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRoutedRuntimeFixture(t, test.substrateV1, test.mutate)
			if test.configure != nil {
				test.configure(f.r)
			}
			for range 2 {
				_, _ = f.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.pool)})
			}
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			if pool.Status.Lifecycle == corev1alpha1.RuntimePoolLifecycleServing || pool.Status.AdmissionState == corev1alpha1.RuntimePoolAdmissionAccepting {
				t.Fatal("mismatched routed evidence was admitted")
			}
			if len(f.router.requests) != 0 || f.direct.calls != 0 || f.supervisor.probeCalls != 0 {
				t.Fatalf("mismatched evidence obtained credential/probe calls: routed=%d direct=%d probe=%d", len(f.router.requests), f.direct.calls, f.supervisor.probeCalls)
			}
			if evidence, err := externalRuntimeEvidence(&pool); err != nil || (evidence != nil && evidence.RouterTarget != "") {
				t.Fatalf("mismatched routed evidence was bound: %#v, %v", evidence, err)
			}
		})
	}
}

func TestExternalRouterTransportPinsOriginAndTarget(t *testing.T) {
	var targets []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targets = append(targets, r.Header.Get(daemonprotocol.TargetActorHeader))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	router := &RuntimePoolSubstrateRouter{URL: server.URL + "/", HTTPClient: server.Client()}
	routed, err := router.routedHTTPClient(server.URL, "atespace/actor", runtimePoolProbeTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if !harnessv2.TransportSupportsHTTPTrace(routed.Transport) {
		t.Fatal("routing wrapper hid exact request trace evidence")
	}
	response, err := routed.Get(server.URL + harnessv2.HealthPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if len(targets) != 1 || targets[0] != "atespace/actor" {
		t.Fatalf("router saw targets %q", targets)
	}
	if _, err := routed.Get("https://elsewhere.example" + harnessv2.HealthPath); err == nil || len(targets) != 1 {
		t.Fatal("routed client carried a request to a non-router origin")
	}
	for name, build := range map[string]func() (*http.Client, error){
		"foreign endpoint": func() (*http.Client, error) {
			return router.routedHTTPClient("https://other.example:443", "atespace/actor", 0)
		},
		"cleartext endpoint": func() (*http.Client, error) {
			return router.routedHTTPClient(strings.Replace(server.URL, "https://", "http://", 1), "atespace/actor", 0)
		},
		"malformed target": func() (*http.Client, error) { return router.routedHTTPClient(server.URL, "atespace/actor/extra", 0) },
		"unconfigured": func() (*http.Client, error) {
			return (*RuntimePoolSubstrateRouter)(nil).routedHTTPClient(server.URL, "atespace/actor", 0)
		},
	} {
		if _, err := build(); err == nil {
			t.Errorf("%s: routed client was built", name)
		}
	}
	// Default HTTPS port and trailing slash normalize; nothing else does.
	pinned := &RuntimePoolSubstrateRouter{URL: "https://atenet-router.ate-system.svc", HTTPClient: server.Client()}
	if _, err := pinned.pinnedEndpoint("https://ATENET-router.ate-system.svc:443/"); err != nil {
		t.Fatalf("equivalent router URL refused: %v", err)
	}
	if _, err := pinned.pinnedEndpoint("https://atenet-router.ate-system.svc:8443"); err == nil {
		t.Fatal("router URL with a different port was accepted")
	}
	if supervisor := (&RuntimePoolReconciler{}).externalStartupSupervisorClient(nil, &workspacev1alpha1.StartupEvidence{Endpoint: "https://other.example", Process: &workspacev1alpha1.NativeProcessEvidence{RouterTarget: "a/b"}}); supervisor == nil {
		t.Fatal("no supervisor client")
	} else if _, err := supervisor.Probe(t.Context(), "https://other.example", "token", nil); !errors.Is(err, errExternalRouterUnconfigured) {
		t.Fatalf("unconfigured routed probe = %v", err)
	}
}

// Retirement drain sends the pool's controller bearer. Provider startup
// evidence that changed after binding must not redirect it to another Actor,
// even when it keeps the bound instance identity.
func TestExternalRetirementDrainRequiresBoundStartupEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*workspacev1alpha1.StartupEvidence)
	}{
		{name: "router target names another Actor", mutate: func(e *workspacev1alpha1.StartupEvidence) {
			e.Process.Name, e.Process.RouterTarget = "actor-2", "orka-atespace/actor-2"
		}},
		{name: "endpoint differs from the bound endpoint", mutate: func(e *workspacev1alpha1.StartupEvidence) {
			e.Endpoint = "https://atenet-router.ate-system.svc"
		}},
		{name: "worker differs from the bound worker", mutate: func(e *workspacev1alpha1.StartupEvidence) {
			e.Process.Worker.UID = "other-worker-uid"
		}},
		{name: "routing removed", mutate: func(e *workspacev1alpha1.StartupEvidence) {
			e.Process.RouterTarget = ""
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRoutedRuntimeFixture(t, true, nil)
			w := f.currentWorkspace(t)
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			if err := f.r.bindExternalRuntimeInstanceEvidence(t.Context(), &pool, w); err != nil {
				t.Fatal(err)
			}
			f.r.SupervisorClient = nil
			changed := w.Status.Allocation.Startup.DeepCopy()
			test.mutate(changed)
			supervisor := f.r.externalStartupSupervisorClient(&pool, changed)
			if _, err := supervisor.Probe(t.Context(), changed.Endpoint, "token", []byte(strings.Repeat("s", 32))); !errors.Is(err, workspaceprovider.ErrStaleIdentity) {
				t.Fatalf("probe with changed evidence = %v, want stale identity", err)
			}
			if err := supervisor.RequestDrain(t.Context(), changed.Endpoint, "token", []byte(strings.Repeat("s", 32)), harnessv2.StatusResponse{}, "workspace_retirement"); !errors.Is(err, workspaceprovider.ErrStaleIdentity) {
				t.Fatalf("drain with changed evidence = %v, want stale identity", err)
			}
			if len(f.router.requests) != 0 || f.direct.calls != 0 {
				t.Fatalf("changed evidence carried controller credentials: routed=%d direct=%d", len(f.router.requests), f.direct.calls)
			}
			// The exact bound evidence still reaches its own Actor.
			_, _ = f.r.externalStartupSupervisorClient(&pool, w.Status.Allocation.Startup).Probe(t.Context(), routedTestRouterURL, "token", []byte(strings.Repeat("s", 32)))
			if len(f.router.requests) == 0 || f.router.requests[0].Header.Get(daemonprotocol.TargetActorHeader) != "orka-atespace/actor-1" {
				t.Fatal("bound evidence did not reach its routed target")
			}
		})
	}
}

// Gateway-backed pool policies are not admission intent. Their cleanup must
// not depend on the live provider-proxy credential or settings, which may
// have been removed or changed since they were written.
func TestExternalGatewayBackedPolicyCleanupNeedsNoLiveProxyConfiguration(t *testing.T) {
	f := newRoutedRuntimeFixture(t, true, nil)
	w := f.currentWorkspace(t)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if err := f.r.bindExternalRuntimeInstanceEvidence(t.Context(), &pool, w); err != nil {
		t.Fatal(err)
	}
	attested, err := f.r.attestExternalWorkspaceStartup(t.Context(), w.Spec.Workload, w.Status.Allocation.Startup)
	if err != nil {
		t.Fatal(err)
	}
	f.supervisor.probe = runtimePoolValidProbe(&pool, attested, "routed-boot", false)
	for range 3 {
		runtimePoolReconcile(t, f.r, f.pool)
	}
	pool = runtimePoolTestGetPool(t, f.r, f.pool)
	cfg, err := f.r.runtimePoolConfigForDeletion(&pool)
	if err != nil {
		t.Fatal(err)
	}
	cfg.namespace = w.Spec.Workload.Runtime.Template.Namespace
	listOwned := func() []networkingv1.NetworkPolicy {
		policies := &networkingv1.NetworkPolicyList{}
		if err := f.r.List(t.Context(), policies, client.InNamespace(cfg.namespace), client.MatchingLabels{runtimePoolUIDLabel: string(pool.UID)}); err != nil {
			t.Fatal(err)
		}
		return policies.Items
	}
	if len(listOwned()) == 0 {
		t.Fatal("fixture created no gateway-backed pool policies")
	}
	// The credential is gone and every proxy/controller input changed.
	f.r.ProviderProxy.BearerToken, f.r.ProviderProxy.BearerTokenFile = nil, ""
	f.r.ProviderProxy.Namespace, f.r.ProviderProxy.BaseURL = "moved-proxy", "http://provider-auth-proxy.moved-proxy.svc:9090"
	f.r.ProviderProxy.PodLabels = map[string]string{"app": "moved"}
	f.r.ControllerNamespace, f.r.ControllerAPIPort = "moved-core", 9443
	if _, err := f.r.ProviderProxy.normalized(); err == nil {
		t.Fatal("fixture still has a usable provider-proxy credential")
	}
	if _, err := f.r.deleteExternalCoreNetworkPolicies(t.Context(), &pool, cfg, f.currentWorkspace(t)); err != nil {
		t.Fatalf("gateway-backed cleanup depended on live configuration: %v", err)
	}
	if remaining := listOwned(); len(remaining) != 0 {
		t.Fatalf("gateway-backed cleanup left %d Core-owned policies", len(remaining))
	}
}

func TestRuntimePoolProviderProxyServiceHostAcceptsAnyClusterDomain(t *testing.T) {
	for host, want := range map[string]bool{
		"proxy.orka-system.svc":                    true,
		"proxy.orka-system.svc.cluster.local":      true,
		"proxy.orka-system.svc.corp.example":       true,
		"a.proxy.orka-system.svc":                  true,
		"orka-system.svc":                          false, // no Service name
		".orka-system.svc":                         false,
		"proxy.other.svc.cluster.local":            false, // another namespace
		"proxy.orka-system.svcx":                   false,
		"proxy.orka-system.svc.":                   false,
		"proxy.orka-system.svc..cluster.local":     false,
		"proxy.orka-system.svc.cluster_local":      false,
		"proxy.orka-system.example.com":            false,
		"proxy.orka-system.svc.cluster.local:8080": false,
	} {
		if got := runtimePoolServiceHostInNamespace(host, "orka-system"); got != want {
			t.Errorf("runtimePoolServiceHostInNamespace(%q) = %t, want %t", host, got, want)
		}
	}
}
