/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/workers/acp/supervisor"
)

// The controller renders the variable the supervisor reads, and refuses the
// same too-short TTLs the supervisor would.
func TestRuntimeSessionIdleTTLMatchesSupervisor(t *testing.T) {
	if runtimePoolIdleSessionTTLEnv != supervisor.EnvIdleSessionTTL {
		t.Fatalf("controller renders %q, supervisor reads %q", runtimePoolIdleSessionTTLEnv, supervisor.EnvIdleSessionTTL)
	}
	if MinACPRuntimeSessionIdleTTL != supervisor.MinIdleSessionTTL {
		t.Fatalf("controller minimum %s, supervisor minimum %s", MinACPRuntimeSessionIdleTTL, supervisor.MinIdleSessionTTL)
	}
	if DefaultACPRuntimeSessionIdleTTL < MinACPRuntimeSessionIdleTTL {
		t.Fatalf("default idle TTL %s is below the minimum", DefaultACPRuntimeSessionIdleTTL)
	}
}

func runtimePoolTemplateEnv(template corev1.PodTemplateSpec, name string) (string, bool) {
	for _, env := range template.Spec.Containers[0].Env {
		if env.Name == name {
			return env.Value, true
		}
	}
	return "", false
}

// A plain pool's runtime Pod carries the idle TTL when one is configured, and
// changing it rolls the pool through its normal replacement.
func TestRuntimePoolTemplateCarriesRuntimeSessionIdleTTL(t *testing.T) {
	pool := runtimePoolTestObject(1)
	r := runtimePoolTestReconciler(t, runtimePoolTestScheme(t), nil, pool)
	cfg, err := r.runtimePoolConfig(pool)
	if err != nil {
		t.Fatal(err)
	}
	disabled := r.runtimePoolPodTemplate(pool, cfg, nil, "test-auth", "test-provider")
	if _, ok := runtimePoolTemplateEnv(disabled, runtimePoolIdleSessionTTLEnv); ok {
		t.Fatal("idle TTL rendered while disabled")
	}

	r.RuntimeSessionIdleTTL = 30 * time.Minute
	enabled := r.runtimePoolPodTemplate(pool, cfg, nil, "test-auth", "test-provider")
	if value, ok := runtimePoolTemplateEnv(enabled, runtimePoolIdleSessionTTLEnv); !ok || value != "30m0s" {
		t.Fatalf("idle TTL env = %q, %v", value, ok)
	}
	if !runtimePoolDeploymentNeedsRollout(&appsv1.Deployment{Spec: appsv1.DeploymentSpec{Template: disabled}}, enabled) {
		t.Fatal("enabling idle retirement did not roll the pool")
	}

	// A workspace-backed pool holds one dedicated session; idle retirement
	// never applies to it.
	workspacePool := pool.DeepCopy()
	workspacePool.Spec.ExecutionWorkspace = &corev1alpha1.RuntimePoolExecutionWorkspaceSpec{}
	if _, ok := runtimePoolTemplateEnv(r.runtimePoolPodTemplate(workspacePool, cfg, nil, "test-auth", "test-provider"), runtimePoolIdleSessionTTLEnv); ok {
		t.Fatal("idle TTL rendered for a workspace-backed pool")
	}
}

// Configured CPU and memory reach the runtime container, and changing them
// rolls the pool.
func TestRuntimePoolTemplateUsesConfiguredPodResources(t *testing.T) {
	pool := runtimePoolTestObject(1)
	r := runtimePoolTestReconciler(t, runtimePoolTestScheme(t), nil, pool)
	cfg, err := r.runtimePoolConfig(pool)
	if err != nil {
		t.Fatal(err)
	}
	before := r.runtimePoolPodTemplate(pool, cfg, nil, "test-auth", "test-provider")
	resources, err := NewRuntimePoolPodResources("1", "2Gi", "4", "8Gi")
	if err != nil {
		t.Fatal(err)
	}
	r.PodResources = &resources
	after := r.runtimePoolPodTemplate(pool, cfg, nil, "test-auth", "test-provider")
	container := after.Spec.Containers[0]
	if container.Resources.Requests.Cpu().String() != "1" || container.Resources.Limits.Memory().String() != "8Gi" {
		t.Fatalf("runtime container resources = %#v", container.Resources)
	}
	if !runtimePoolDeploymentNeedsRollout(&appsv1.Deployment{Spec: appsv1.DeploymentSpec{Template: before}}, after) {
		t.Fatal("changed Pod resources did not roll the pool")
	}
}

// A rate-limited prompt whose RuntimeSession the runtime retired, or is
// retiring, must requeue; one whose session is still there waits in place.
func TestRejectedPromptRuntimeSessionGone(t *testing.T) {
	digest := harnessv2.ProfileDigest("sha256:" + strings.Repeat("d", 64))
	const sessionUID = harnessv2.RuntimeSessionUID("session-idle")
	fence := harnessv2.Fence{RuntimeSessionUID: sessionUID, RuntimeSessionGeneration: 2}
	sessionID := harnessv2.RuntimeSessionID(runtimeSessionID(fence))
	newClient := func(t *testing.T, descriptor harnessv2.RuntimeSessionDescriptor, status int) *harnessv2.Client {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if status != http.StatusOK {
				w.WriteHeader(status)
				return
			}
			writeDispatcherJSON(w, dispatcherRuntimeStatusResponse(digest, descriptor))
		}))
		t.Cleanup(server.Close)
		runtimeClient, err := harnessv2.NewClient(
			server.URL, harnessv2.WithControllerBearerToken(strings.Repeat("t", 32)),
			harnessv2.WithOperationCapabilitySecret([]byte(strings.Repeat("s", 32))),
			harnessv2.WithStatusCapabilityBinding(harnessv2.StatusCapabilityBinding{RuntimeProfileDigest: digest, RuntimeInstanceID: "pod-uid.boot-id"}),
		)
		if err != nil {
			t.Fatal(err)
		}
		return runtimeClient
	}
	descriptor := func(state harnessv2.RuntimeSessionState, generation uint64) harnessv2.RuntimeSessionDescriptor {
		return harnessv2.RuntimeSessionDescriptor{
			RuntimeSessionID:  harnessv2.RuntimeSessionID(runtimeSessionID(harnessv2.Fence{RuntimeSessionUID: sessionUID, RuntimeSessionGeneration: generation})),
			RuntimeSessionUID: sessionUID, Generation: generation, State: state, LastTransitionAt: time.Now().UTC(),
		}
	}
	ctx := context.Background()
	for _, test := range []struct {
		name       string
		descriptor harnessv2.RuntimeSessionDescriptor
		status     int
		gone       bool
	}{
		{"idle session still listed", descriptor(harnessv2.RuntimeSessionStateIdle, 2), http.StatusOK, false},
		{"session retired", harnessv2.RuntimeSessionDescriptor{}, http.StatusOK, true},
		{"session being deleted", descriptor(harnessv2.RuntimeSessionStateDeleting, 2), http.StatusOK, true},
		{"newer generation listed", descriptor(harnessv2.RuntimeSessionStateIdle, 3), http.StatusOK, true},
		{"status unreadable", harnessv2.RuntimeSessionDescriptor{}, http.StatusServiceUnavailable, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := rejectedPromptRuntimeSessionGone(ctx, newClient(t, test.descriptor, test.status), sessionID, fence); got != test.gone {
				t.Fatalf("rejectedPromptRuntimeSessionGone() = %v, want %v", got, test.gone)
			}
		})
	}
}
