package harness_test

import (
	"context"
	"testing"

	"github.com/sozercan/orka/internal/harness"
	"github.com/sozercan/orka/internal/harness/harnesstest"
)

func TestKubernetesServiceHarnessProviderConformance(t *testing.T) {
	harnesstest.RunHarnessConformance(t, func(t *testing.T, behavior harnesstest.FakeBehavior) (string, func()) {
		t.Helper()
		server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{Behavior: behavior})
		provider := harness.KubernetesServiceProvider{EndpointURL: server.URL(), AllowInsecureLoopback: true}
		endpoint, err := provider.Endpoint(context.Background(), harness.RuntimeSessionOwner{
			Namespace:   "default",
			SessionName: "session-a",
			Provider:    harness.ProviderKindKubernetesService,
		})
		if err != nil {
			server.Close()
			t.Fatalf("Endpoint() error = %v", err)
		}
		return endpoint, server.Close
	})
}

func TestKubernetesServiceHarnessProviderRequiresInNamespaceServiceDNS(t *testing.T) {
	provider := harness.KubernetesServiceProvider{EndpointURL: "http://harness.other.svc:8080"}
	if _, err := provider.Endpoint(context.Background(), harness.RuntimeSessionOwner{
		Namespace: "default", SessionName: "session-a", Provider: harness.ProviderKindKubernetesService,
	}); err == nil {
		t.Fatal("Endpoint() error = nil, want cross-namespace service rejection")
	}
	provider = harness.KubernetesServiceProvider{EndpointURL: "http://harness.default.svc.cluster.local:8080"}
	if _, err := provider.Endpoint(context.Background(), harness.RuntimeSessionOwner{
		Namespace: "default", SessionName: "session-a", Provider: harness.ProviderKindKubernetesService,
	}); err != nil {
		t.Fatalf("Endpoint() error = %v, want in-namespace service accepted", err)
	}
}

func TestKubernetesServiceHarnessProviderRejectsInvalidEndpoint(t *testing.T) {
	provider := harness.KubernetesServiceProvider{EndpointURL: "not-a-url"}
	if _, err := provider.Endpoint(context.Background(), harness.RuntimeSessionOwner{
		Namespace: "default", SessionName: "session-a", Provider: harness.ProviderKindKubernetesService,
	}); err == nil {
		t.Fatal("Endpoint() error = nil, want invalid endpoint error")
	}
}
