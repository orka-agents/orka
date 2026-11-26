package controller

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func permissionTimeoutEnvironment(env []corev1.EnvVar) []string {
	var values []string
	for _, item := range env {
		if item.Name == "ORKA_ACP_PERMISSION_TIMEOUT" {
			values = append(values, item.Value)
		}
	}
	return values
}

func TestRuntimePoolPermissionTimeoutReachesRuntimesOnlyWhenConfigured(t *testing.T) {
	for _, test := range []struct {
		timeout time.Duration
		want    []string
	}{
		{timeout: 0},
		{timeout: 20 * time.Minute, want: []string{"20m0s"}},
	} {
		t.Run(test.timeout.String(), func(t *testing.T) {
			pool := runtimePoolTestObject(1)
			r := runtimePoolTestReconciler(t, runtimePoolTestScheme(t), nil, pool)
			r.AllowedImages = ACPRuntimeImages{
				Codex: pool.Spec.Runtime.Image, Claude: pool.Spec.Runtime.Image,
				Copilot: pool.Spec.Runtime.Image, Opencode: pool.Spec.Runtime.Image,
			}
			r.PermissionTimeout = test.timeout
			cfg, err := r.runtimePoolConfig(pool)
			if err != nil {
				t.Fatal(err)
			}
			template := r.runtimePoolPodTemplate(pool, cfg, nil, "test-auth", "test-provider")
			got := permissionTimeoutEnvironment(template.Spec.Containers[0].Env)
			if len(got) != len(test.want) || (len(got) == 1 && got[0] != test.want[0]) {
				t.Fatalf("permission timeout environment = %v, want %v", got, test.want)
			}
		})
	}
}
