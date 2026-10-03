package controller

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The fixture was captured before extraction at Orka commit
// 1cd16c88b43410e0ea46463f8a39b473d8e5250e. Its capture source and matching SDK
// regression live in orka-workspace/testdata/baseline/orka-1cd16c88b.
func TestResolvedClassProfileHashMatchesSharedModuleBaseline(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("testdata", "workspace-class-profile-hashes.json"))
	if err != nil {
		t.Fatal(err)
	}
	var captured struct {
		Baseline string `json:"baseline"`
		Fixtures []struct {
			Name           string                                       `json:"name"`
			Class          workspacev1alpha1.ExecutionWorkspaceClass    `json:"class"`
			Provider       workspacev1alpha1.ExecutionWorkspaceProvider `json:"provider"`
			ProviderConfig unstructured.Unstructured                    `json:"providerConfig"`
			Pool           *workspacev1alpha1.ExecutionWorkspacePool    `json:"pool,omitempty"`
			Parameters     unstructured.Unstructured                    `json:"parameters"`
			Hash           string                                       `json:"hash"`
		} `json:"fixtures"`
	}
	if err := json.Unmarshal(data, &captured); err != nil {
		t.Fatal(err)
	}
	if captured.Baseline != "1cd16c88b43410e0ea46463f8a39b473d8e5250e" || len(captured.Fixtures) != 6 {
		t.Fatal("unexpected baseline or missing class-profile fixtures")
	}
	for _, fixture := range captured.Fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			scheme := runtime.NewScheme()
			if err := workspacev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			gvk := fixture.Parameters.GroupVersionKind()
			mapper := apimeta.NewDefaultRESTMapper([]schema.GroupVersion{gvk.GroupVersion()})
			mapper.Add(gvk, apimeta.RESTScopeNamespace)
			objects := []client.Object{&fixture.Provider, &fixture.Class, &fixture.ProviderConfig, &fixture.Parameters}
			if fixture.Pool != nil {
				objects = append(objects, fixture.Pool)
			}
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			reconciler := &ExecutionWorkspaceClassReconciler{Client: kube, APIReader: kube, RESTMapper: mapper}
			got, err := reconciler.resolvedClassProfileHash(context.Background(), &fixture.Class)
			if err != nil {
				t.Fatal(err)
			}
			if got != fixture.Hash {
				t.Fatalf("resolvedClassProfileHash = %s, want baseline %s", got, fixture.Hash)
			}
		})
	}
}
