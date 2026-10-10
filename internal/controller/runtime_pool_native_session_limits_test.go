package controller

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func assertNativeSessionEnvironment(t *testing.T, env []corev1.EnvVar, want int) {
	t.Helper()
	count := 0
	for _, item := range env {
		if item.Name != "ORKA_NATIVE_SESSION_MAX_BYTES" {
			continue
		}
		count++
		if item.Value != strconv.Itoa(want) || item.ValueFrom != nil {
			t.Fatalf("native session environment = %v, want %d", item, want)
		}
	}
	if count != 1 {
		t.Fatalf("native session environment occurrences = %d, want 1", count)
	}
}

func TestRuntimePoolNativeSessionLimitsAllBackends(t *testing.T) {
	for _, limit := range []int{0, 12 << 20, harnessv2.MaxNativeSessionBytes} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			want := limit
			if want == 0 {
				want = harnessv2.DefaultMaxNativeSessionBytes
			}
			for _, provider := range []string{runtimePoolProviderCodex, runtimePoolProviderClaude, runtimePoolProviderCopilot, runtimePoolProviderOpencode} {
				t.Run("deployment/"+provider, func(t *testing.T) {
					pool := runtimePoolTestObject(1)
					pool.Spec.Runtime.Profile.ProviderKind = provider
					if provider == runtimePoolProviderOpencode {
						pool.Spec.Runtime.Profile.ModelLimits = &corev1alpha1.ModelTokenLimits{Context: 128000, Output: 16000}
					}
					runtimePoolTestRefreshProfileDigest(t, pool)
					r := runtimePoolTestReconciler(t, runtimePoolTestScheme(t), nil, pool)
					r.AllowedImages = ACPRuntimeImages{
						Codex: pool.Spec.Runtime.Image, Claude: pool.Spec.Runtime.Image,
						Copilot: pool.Spec.Runtime.Image, Opencode: pool.Spec.Runtime.Image,
					}
					r.NativeSessionMaxBytes = limit
					cfg, err := r.runtimePoolConfig(pool)
					if err != nil {
						t.Fatal(err)
					}
					assertNativeSessionEnvironment(t, r.runtimePoolEnvironment(pool, cfg), want)
					template := r.runtimePoolPodTemplate(pool, cfg, nil, "test-auth", "test-provider")
					assertNativeSessionEnvironment(t, template.Spec.Containers[0].Env, want)
				})
			}

			t.Run("agent sandbox", func(t *testing.T) {
				pool := runtimePoolWorkspaceTestObject()
				r := runtimePoolTestReconciler(t, runtimePoolWorkspaceTestScheme(t), nil, pool)
				r.NativeSessionMaxBytes = limit
				runtimePoolReconcile(t, r, pool)
				template, _, _ := runtimePoolWorkspaceTestChildren(t, r, pool)
				if template == nil {
					t.Fatal("Agent Sandbox template was not created")
				}
				assertNativeSessionEnvironment(t, template.Spec.PodTemplate.Spec.Containers[0].Env, want)
			})
			t.Run("substrate", func(t *testing.T) {
				r, pool := runtimePoolSubstrateTestReconciler(t, nil, &fakeSubstrateActorControl{})
				r.NativeSessionMaxBytes = limit
				cfg, err := r.runtimePoolConfig(pool)
				if err != nil {
					t.Fatal(err)
				}
				rendered, err := r.renderSubstrateRuntimeTemplate(pool, cfg, substrateTestBaseTemplate(), substrateTestTemplateNamespace, "test-actor", "test-nonce", "test-public-key")
				if err != nil {
					t.Fatal(err)
				}
				containers, _, err := unstructured.NestedSlice(rendered.object.Object, "spec", "containers")
				if err != nil || len(containers) != 1 {
					t.Fatalf("Substrate rendered container: %v", err)
				}
				env := containers[0].(map[string]any)["env"].([]any)
				var typed []corev1.EnvVar
				for _, item := range env {
					values := item.(map[string]any)
					if values["name"] == "ORKA_NATIVE_SESSION_MAX_BYTES" {
						typed = append(typed, corev1.EnvVar{Name: values["name"].(string), Value: values["value"].(string)})
					}
				}
				assertNativeSessionEnvironment(t, typed, want)
			})
		})
	}
}

func TestRuntimePoolNativeSessionLimitRejectsBeforeResourceCreation(t *testing.T) {
	for _, limit := range []int{-1, harnessv2.MaxNativeSessionBytes + 1} {
		t.Run(strconv.Itoa(limit), func(t *testing.T) {
			pool := runtimePoolTestObject(1)
			r := runtimePoolTestReconciler(t, runtimePoolTestScheme(t), nil, pool)
			r.NativeSessionMaxBytes = limit
			created := false
			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
				Create: func(_ context.Context, _ client.WithWatch, _ client.Object, _ ...client.CreateOption) error {
					created = true
					return errors.New("unexpected resource creation")
				},
			})
			if _, err := r.runtimePoolConfig(pool); err == nil {
				t.Fatal("invalid native policy accepted")
			}
			if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}); err != nil {
				t.Fatal(err)
			}
			if created {
				t.Fatal("invalid native policy created runtime resources")
			}
			current := runtimePoolTestGetPool(t, r, pool)
			if !strings.Contains(current.Status.Message, "native session max bytes") {
				t.Fatal("invalid native policy was not reported")
			}
		})
	}
}

func TestRuntimePoolNativeSessionLimitChangesTemplateRevision(t *testing.T) {
	pool := runtimePoolTestObject(1)
	r := runtimePoolTestReconciler(t, runtimePoolTestScheme(t), nil, pool)
	cfg, err := r.runtimePoolConfig(pool)
	if err != nil {
		t.Fatal(err)
	}
	before := r.runtimePoolPodTemplate(pool, cfg, nil, "test-auth", "test-provider")
	r.NativeSessionMaxBytes = 12 << 20
	cfg, err = r.runtimePoolConfig(pool)
	if err != nil {
		t.Fatal(err)
	}
	after := r.runtimePoolPodTemplate(pool, cfg, nil, "test-auth", "test-provider")
	deployment := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Template: before}}
	if !runtimePoolDeploymentNeedsRollout(deployment, after) {
		t.Fatal("changed native policy did not trigger existing revision reconciliation")
	}
	if runtimePoolDeploymentNeedsRollout(&appsv1.Deployment{Spec: appsv1.DeploymentSpec{Template: after}}, after) {
		t.Fatal("unchanged native policy triggered a new rollout")
	}
}
