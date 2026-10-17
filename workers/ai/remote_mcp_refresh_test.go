package main

import (
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/workerenv"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A frozen connector definition drifting to remote must never discover in the
// worker. Classification is an upper bound on controller-owned custody.
func TestNativeRemoteMCPRejectsConnectorOverlapBeforeDiscovery(t *testing.T) {
	for _, source := range []string{"frozen dispatch", "live connection policy"} {
		t.Run(source, func(t *testing.T) {
			f := newNativeRemoteFixture(t)
			if source == "frozen dispatch" {
				t.Setenv(workerenv.ConnectorToolDigests, `{"health":"controller-frozen-digest"}`)
			} else {
				policy := &corev1alpha1.OutboundAccessPolicy{}
				if err := f.client.Get(t.Context(), client.ObjectKey{Namespace: "team", Name: "egress"}, policy); err != nil {
					t.Fatal(err)
				}
				policy.Spec.Connection = &corev1alpha1.ConnectionOutboundAccess{}
				if err := f.client.Update(t.Context(), policy); err != nil {
					t.Fatal(err)
				}
			}
			_, _, err := f.prepare(t)
			if err == nil || !strings.Contains(err.Error(), "connector-backed") {
				t.Errorf("startup error = %v, want remote/connector overlap rejection", err)
			}
			if f.requests.Load() != 0 {
				t.Fatalf("overlap sent %d MCP discovery requests", f.requests.Load())
			}
		})
	}
}

func TestNativeRemoteMCPRejectsConnectionPolicyBeforeCredentials(t *testing.T) {
	f := newNativeRemoteFixture(t)
	policy := &corev1alpha1.OutboundAccessPolicy{}
	if err := f.client.Get(t.Context(), client.ObjectKey{Namespace: "team", Name: "egress"}, policy); err != nil {
		t.Fatal(err)
	}
	policy.Spec.Connection = &corev1alpha1.ConnectionOutboundAccess{}
	if err := f.client.Update(t.Context(), policy); err != nil {
		t.Fatal(err)
	}
	reader := &nativeNoSecretReadClient{Client: f.client, t: t}
	err := bindNativeRemoteDependencies(t.Context(), reader, f.tool, "auth", nil)
	if err == nil || !strings.Contains(err.Error(), "gateway policy") {
		t.Fatalf("dependency binding error = %v, want gateway-only rejection", err)
	}
}
