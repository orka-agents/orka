package main

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"

	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/orka-agents/orka/internal/memorybackend"
)

func TestWorkspaceCleanupAPIsInstalled(t *testing.T) {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{workspacev1alpha1.GroupVersion})
	mapper.Add(
		workspacev1alpha1.GroupVersion.WithKind("ExecutionWorkspaceProvider"),
		meta.RESTScopeRoot,
	)

	installed, err := workspaceCleanupAPIsInstalled(mapper)
	if err != nil {
		t.Fatalf("partial discovery returned error: %v", err)
	}
	if installed {
		t.Fatal("partial workspace API discovery reported installed")
	}

	mapper.Add(
		workspacev1alpha1.GroupVersion.WithKind("ExecutionWorkspace"),
		meta.RESTScopeNamespace,
	)
	installed, err = workspaceCleanupAPIsInstalled(mapper)
	if err != nil {
		t.Fatalf("provider/workspace discovery returned error: %v", err)
	}
	if installed {
		t.Fatal("cleanup discovery ignored missing class and pool APIs")
	}

	mapper.Add(
		workspacev1alpha1.GroupVersion.WithKind("ExecutionWorkspaceClass"),
		meta.RESTScopeNamespace,
	)
	mapper.Add(
		workspacev1alpha1.GroupVersion.WithKind("ExecutionWorkspacePool"),
		meta.RESTScopeNamespace,
	)
	installed, err = workspaceCleanupAPIsInstalled(mapper)
	if err != nil {
		t.Fatalf("complete discovery returned error: %v", err)
	}
	if !installed {
		t.Fatal("complete workspace API discovery reported missing")
	}
}

func TestValidateWorkspaceProviderSecurityConfig(t *testing.T) {
	if err := validateWorkspaceProviderSecurityConfig(false, false); err != nil {
		t.Fatalf("disabled API validation: %v", err)
	}
	if err := validateWorkspaceProviderSecurityConfig(true, true); err != nil {
		t.Fatalf("enabled secure API validation: %v", err)
	}
	if err := validateWorkspaceProviderSecurityConfig(true, false); err == nil {
		t.Fatal("workspace API enabled without class-use admission")
	}
}

func TestFoundationMemoryReleaseRejectsActivation(t *testing.T) {
	capabilities, err := memoryReleaseCapabilitiesForStage(memoryReleaseStageFoundation)
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.featureEpoch != memorybackend.FoundationFeatureEpoch {
		t.Fatalf(
			"foundation feature epoch = %d, want %d",
			capabilities.featureEpoch,
			memorybackend.FoundationFeatureEpoch,
		)
	}
	if capabilities.activationAllowed {
		t.Fatal("foundation release unexpectedly allows activation")
	}
	if err := validateMemoryFeatureGates(true, false, capabilities); err != nil {
		t.Fatalf("foundation staging gate rejected: %v", err)
	}
	if err := validateMemoryFeatureGates(true, true, capabilities); err == nil ||
		!strings.Contains(err.Error(), "foundation release artifact") {
		t.Fatalf("foundation activation error = %v", err)
	}
}

func TestActivationMemoryReleaseAdvertisesActivationEpoch(t *testing.T) {
	capabilities, err := memoryReleaseCapabilitiesForStage(memoryReleaseStageActivation)
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.featureEpoch != memorybackend.ActivationFeatureEpoch {
		t.Fatalf(
			"activation feature epoch = %d, want %d",
			capabilities.featureEpoch,
			memorybackend.ActivationFeatureEpoch,
		)
	}
	if !capabilities.activationAllowed {
		t.Fatal("activation release did not allow activation")
	}
	if err := validateMemoryFeatureGates(true, true, capabilities); err != nil {
		t.Fatalf("activation release gate rejected: %v", err)
	}
}

func TestMemoryReleaseRejectsUnknownStage(t *testing.T) {
	if _, err := memoryReleaseCapabilitiesForStage("operator-override"); err == nil {
		t.Fatal("unknown memory release stage was accepted")
	}
}
