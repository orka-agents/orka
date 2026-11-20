/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/orka-agents/orka/internal/agentidentity"
)

// coordinationHandler serves discovery for Leases and records deletes of the
// Agent use enforcement marker, answering them as an absent Lease.
func coordinationHandler(deletes *atomic.Int32) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"kind":"APIVersions","versions":["v1"],"serverAddressByClientCIDRs":[]}`)
	})
	mux.HandleFunc("/apis", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"kind":"APIGroupList","apiVersion":"v1","groups":[{"name":"coordination.k8s.io",`+
			`"versions":[{"groupVersion":"coordination.k8s.io/v1","version":"v1"}],`+
			`"preferredVersion":{"groupVersion":"coordination.k8s.io/v1","version":"v1"}}]}`)
	})
	mux.HandleFunc("/api/v1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"kind":"APIResourceList","groupVersion":"v1","resources":[]}`)
	})
	mux.HandleFunc("/apis/coordination.k8s.io/v1", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, `{"kind":"APIResourceList","groupVersion":"coordination.k8s.io/v1","resources":[`+
			`{"name":"leases","singularName":"lease","namespaced":true,"kind":"Lease","verbs":["get","delete"]}]}`)
	})
	mux.HandleFunc("/apis/coordination.k8s.io/v1/namespaces/orka-system/leases/"+agentidentity.EnforcementLeaseName,
		func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodDelete {
				http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
				return
			}
			deletes.Add(1)
			statusHandler(http.StatusNotFound, "NotFound").ServeHTTP(w, r)
		})
	return mux
}

// newEnforcementTestManager builds a manager only for registering runnables;
// it has its own discovery server.
func newEnforcementTestManager(t *testing.T) ctrl.Manager {
	t.Helper()
	discoveryAPI := httptest.NewServer(discoveryHandler())
	t.Cleanup(discoveryAPI.Close)
	mgr, err := newTestManager(t, &rest.Config{Host: discoveryAPI.URL})
	if err != nil {
		t.Fatal(err)
	}
	return mgr
}

// With Agent use authorization off, ending the enforcement period runs before
// the manager starts. A connection error just after a node restart is retried
// within the startup budget instead of failing startup.
func TestEstablishAgentUseEnforcementPeriodRetriesTransientConnectErrors(t *testing.T) {
	var deletes atomic.Int32
	api := httptest.NewServer(coordinationHandler(&deletes))
	t.Cleanup(api.Close)
	mgr := newEnforcementTestManager(t)

	unrouted := closedAddress(t)
	const unreachableDials = 2
	var dials atomic.Int32
	config := &rest.Config{Host: api.URL, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		if dials.Add(1) <= unreachableDials {
			address = unrouted
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}}
	retry := testStartupRetry(logr.Discard(), 10*time.Second, time.Millisecond, 5*time.Millisecond)
	identity := &agentidentity.Config{}
	err := establishAgentUseEnforcementPeriod(t.Context(), retry, mgr, config, "orka-system", nil, identity)
	if err != nil {
		t.Fatalf("establishAgentUseEnforcementPeriod: %v", err)
	}
	if got := deletes.Load(); got != 1 {
		t.Fatalf("enforcement marker deletes = %d, want 1 once the API server is reachable", got)
	}
	if identity.Enforcement == nil {
		t.Fatal("the enforcement period was not recorded")
	}
}

// A refused connection that outlasts the startup budget still fails startup.
func TestEstablishAgentUseEnforcementPeriodFailsAfterTheBudget(t *testing.T) {
	var deletes atomic.Int32
	api := httptest.NewServer(coordinationHandler(&deletes))
	t.Cleanup(api.Close)
	mgr := newEnforcementTestManager(t)
	unrouted := closedAddress(t)
	config := &rest.Config{Host: api.URL, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, unrouted)
	}}
	retry := testStartupRetry(logr.Discard(), 50*time.Millisecond, time.Millisecond, 5*time.Millisecond)
	err := establishAgentUseEnforcementPeriod(t.Context(), retry, mgr, config, "orka-system", nil, &agentidentity.Config{})
	if err == nil || !isTransientConnectError(err) {
		t.Fatalf("establishAgentUseEnforcementPeriod = %v, want the connection error after the budget", err)
	}
	if got := deletes.Load(); got != 0 {
		t.Fatalf("enforcement marker deletes = %d, want 0", got)
	}
}
