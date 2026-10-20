package controller

import (
	"testing"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The old hash is a private decode compatibility boundary. Removing served
// provider APIs must not rewrite immutable execution snapshots from before upgrade.
func TestLegacyWorkspaceClassProfileHashIsStable(t *testing.T) {
	f := newACPClassFixture(t, RuntimeProviderBackendAgentSandbox)
	f.class.Spec.RequiredFeatures = nil
	f.provider.Spec.ControllerName = acpWorkspaceProviderControllerName
	f.provider.Spec.ServiceAccountRef = nil
	f.provider.Spec.RequiredContracts = []string{workspacev1alpha1.ContractVersionV1}
	f.class.Spec.ParametersRef.Group = "acp.workspace.orka.ai"
	f.provider.Spec.ParametersRef.Group = "acp.workspace.orka.ai"
	f.profile.SetGroupVersionKind(schema.GroupVersionKind{Group: "acp.workspace.orka.ai", Version: "v1alpha1", Kind: "RuntimeWorkspaceProfile"})
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(f.profile)
	if err != nil {
		t.Fatal(err)
	}
	profile := &unstructured.Unstructured{Object: raw}
	got, err := acpWorkspaceClassProfileHash(f.class, f.provider, profile)
	if err != nil {
		t.Fatal(err)
	}
	const want = "sha256:0da2d1bb9f4d2438de47495e77a001f41c21dc8a57d3ecd42d781a66b5a06ca3"
	if got != want {
		t.Fatalf("legacy class profile hash = %s, want %s", got, want)
	}
}
