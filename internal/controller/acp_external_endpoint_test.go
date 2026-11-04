package controller

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

// A native provider's admitted router port and path must survive Task dispatch.
// A hostname-only reconstruction would send private credentials to port 8080.
func TestExternalRuntimePoolDispatchUsesPinnedNativeRouterEndpoint(t *testing.T) {
	ctx := context.Background()
	f := newExternalRuntimePoolFixture(t)
	f.advertiseNativeProcess(t)
	w, worker := f.materialize(t)
	const endpoint = "http://native-router.example:80/actor-runtime"
	startup := w.Status.Allocation.Startup
	startup.Pod = nil
	startup.Endpoint = endpoint
	startup.Process = &workspacev1alpha1.NativeProcessEvidence{Namespace: "native", Name: "process", UID: w.Status.Allocation.Identity.InstanceID, Version: 1,
		Worker: workspacev1alpha1.PodReference{Namespace: worker.Namespace, Name: worker.Name, UID: worker.UID}, ChallengeSHA256: "sha256:" + strings.Repeat("a", 64)}
	if err := f.r.Status().Update(ctx, w); err != nil {
		t.Fatal(err)
	}
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if err := f.r.bindExternalRuntimeInstanceEvidence(ctx, &pool, w); err != nil {
		t.Fatal(err)
	}
	pod, err := f.r.attestExternalWorkspaceStartup(ctx, w.Spec.Workload, startup)
	if err != nil {
		t.Fatal(err)
	}
	probe := runtimePoolValidProbe(&pool, pod, "native-boot", false)
	cfg, err := f.r.runtimePoolConfig(&pool)
	if err != nil {
		t.Fatal(err)
	}
	active, err := validateRuntimePoolProbe(&pool, cfg, pod, probe, f.r.now())
	if err != nil {
		t.Fatal(err)
	}
	pool.Status.ActiveInstance = active
	pool.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleServing
	if err := f.r.Status().Update(ctx, &pool); err != nil {
		t.Fatal(err)
	}
	epochs := NewControllerEpochManager(nil, "controller")
	epochs.current = &store.ControllerEpoch{Name: store.DefaultControllerEpochName, Epoch: active.ControllerEpoch, HolderID: "controller"}
	close(epochs.ready)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		calls++
		if r.Host != "native-router.example:80" || (r.URL.Path != "/actor-runtime"+harnessv2.CapabilitiesPath && r.URL.Path != "/actor-runtime"+harnessv2.StatusPath) {
			t.Errorf("dispatch URL = %s%s", r.Host, r.URL.Path)
		}
		rw.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/actor-runtime"+harnessv2.StatusPath {
			if r.Header.Get("Authorization") == "" || r.Header.Get(harnessv2.OperationCapabilityHeader) == "" {
				t.Error("missing authenticated runtime request")
			}
			_ = json.NewEncoder(rw).Encode(probe.Status)
			return
		}
		probe.Capabilities.SupportsPublicationFinalization = true
		_ = json.NewEncoder(rw).Encode(probe.Capabilities)
	}))
	defer server.Close()
	original := http.DefaultTransport
	transport := original.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "native-router.example:80" {
			t.Errorf("dial address = %s, want admitted router port 80", address)
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = original; transport.CloseIdleConnections() }()
	dispatcher := &ACPDispatcher{Client: f.r.Client, APIReader: f.r.Client, Epochs: epochs}
	runtimeClient, _, _, _, err := dispatcher.runtimeClient(ctx, acpDispatchTarget{pool: &pool}, harnessv2.MCPPolicyConfiguration{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtimeClient.Status(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("capability calls = %d", calls)
	}
	for _, change := range []string{"endpoint", "worker UID", "active UID"} {
		t.Run(change, func(t *testing.T) {
			current := f.currentWorkspace(t)
			current.Status = *w.Status.DeepCopy()
			target := pool.DeepCopy()
			switch change {
			case "endpoint":
				current.Status.Allocation.Startup.Endpoint = "http://replacement.example:80"
			case "worker UID":
				current.Status.Allocation.Startup.Process.Worker.UID = "foreign"
			case "active UID":
				target.Status.ActiveInstance.PodUID = "foreign"
			}
			if err := f.r.Status().Update(ctx, current); err != nil {
				t.Fatal(err)
			}
			if _, _, _, _, err := dispatcher.runtimeClient(ctx, acpDispatchTarget{pool: target}, harnessv2.MCPPolicyConfiguration{}, true); err == nil {
				t.Fatal("changed admitted endpoint/identity reached runtime")
			}
			if calls != 2 {
				t.Fatalf("stale evidence made authenticated requests: %d", calls)
			}
		})
	}
}
