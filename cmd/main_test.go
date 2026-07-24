package main

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"

	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
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
