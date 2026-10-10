/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestRuntimePoolPodTemplateAddsSandboxExecToStandardPools(t *testing.T) {
	pool := runtimePoolTestObject(1)
	r := runtimePoolTestReconciler(t, runtimePoolTestScheme(t), nil, pool)
	cfg, err := r.runtimePoolConfig(pool)
	if err != nil {
		t.Fatalf("runtimePoolConfig: %v", err)
	}
	selector := map[string]string{runtimePoolKeyLabel: cfg.labels[runtimePoolKeyLabel]}
	plain := r.runtimePoolPodTemplate(pool, cfg, selector, "auth", "provider")

	r.SandboxExec = &HyperlightPodConfig{BundleImage: "example.com/hyperlight-bundle:test", DeviceGID: 990}
	template := r.runtimePoolPodTemplate(pool, cfg, selector, "auth", "provider")
	assertRuntimePoolEnvironment(t, r, pool, template.Spec.Containers[0].Env)
	container := template.Spec.Containers[0]
	env := codeExecEnv(container)
	if env[runtimePoolSandboxExecEnv] != "true" || env[hyperlightDeviceGIDEnv] != "990" {
		t.Fatalf("supervisor env = %v, want sandbox_exec enabled with the device group", env)
	}
	if got := container.Resources.Limits[DefaultHyperlightDeviceResource]; got.Cmp(resource.MustParse("1")) != 0 {
		t.Fatalf("device limit = %v, want 1", got)
	}
	if !slices.Contains(template.Spec.SecurityContext.SupplementalGroups, int64(990)) {
		t.Fatalf("supplementalGroups = %v, want the device group", template.Spec.SecurityContext.SupplementalGroups)
	}
	if !slices.ContainsFunc(template.Spec.InitContainers, func(c corev1.Container) bool { return c.Name == hyperlightBundleInit }) {
		t.Fatal("the Hyperlight bundle is not copied into the runtime Pod")
	}
	if template.Annotations[runtimePoolTemplateRevisionAnnotation] == plain.Annotations[runtimePoolTemplateRevisionAnnotation] {
		t.Fatal("enabling sandbox_exec did not change the template revision, so running Pods would not be replaced")
	}

	workspacePool := runtimePoolTestObject(1)
	workspacePool.Spec.ExecutionWorkspace = &corev1alpha1.RuntimePoolExecutionWorkspaceSpec{}
	workspaceTemplate := r.runtimePoolPodTemplate(workspacePool, cfg, selector, "auth", "provider")
	if _, ok := codeExecEnv(workspaceTemplate.Spec.Containers[0])[runtimePoolSandboxExecEnv]; ok {
		t.Fatal("a workspace-backed pool, which has no hypervisor, got sandbox_exec")
	}
	if _, ok := workspaceTemplate.Spec.Containers[0].Resources.Limits[DefaultHyperlightDeviceResource]; ok {
		t.Fatal("a workspace-backed pool requested the hypervisor device")
	}
}
