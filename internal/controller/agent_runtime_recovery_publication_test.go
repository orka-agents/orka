package controller

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func interruptedRuntimeRecoveryEnrollment(t *testing.T, point string) (*runtimeRecoveryFixture, agentRuntimeBootWitness) {
	t.Helper()
	f := newRuntimeRecoveryFixture(t)
	// Retain compatibility with witness-first enrollment written by older
	// controllers; new enrollment publishes only after Pod retention.
	legacy := unadmittedRecoveryWitness(t, f)
	digest, err := runtimeWitnessDigest(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistAgentRuntimeRecoveryEffect(t.Context(), f.control, f.fence,
		agentRuntimeRecoveryIdentity(agentRuntimeBootWitnessKind, legacy.RuntimeUID, legacy.Namespace, string(legacy.Fence.SupervisorBootID)), digest, legacy); err != nil {
		t.Fatal(err)
	}
	base, ok := f.r.Client.(client.WithWatch)
	if !ok {
		t.Fatal("recovery fixture client does not support interception")
	}
	var interrupted atomic.Bool
	interruption := errors.New("injected enrollment retention interruption")
	f.r.Client = interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, delegate client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if pod, ok := object.(*corev1.Pod); point == "pod-retention" && ok && pod.UID == f.pod.UID &&
				controllerutil.ContainsFinalizer(pod, agentRuntimeRecoveryPodFinalizer) && interrupted.CompareAndSwap(false, true) {
				return interruption
			}
			return delegate.Patch(ctx, object, patch, options...)
		},
		Create: func(ctx context.Context, delegate client.WithWatch, object client.Object, options ...client.CreateOption) error {
			if secret, ok := object.(*corev1.Secret); point == "boot-auth" && ok && strings.HasPrefix(secret.Name, "agent-runtime-boot-") &&
				interrupted.CompareAndSwap(false, true) {
				return interruption
			}
			return delegate.Create(ctx, object, options...)
		},
	})
	f.reconcile(t)
	if !interrupted.Load() || f.runtime.Status.Ready || f.server.Counts().SessionCreates != 0 {
		t.Fatal("interrupted enrollment admitted conformance or missed the fault")
	}
	if !controllerutil.ContainsFinalizer(f.runtime, agentRuntimeFinalizer) {
		t.Fatal("interrupted enrollment lost its runtime owner")
	}
	witness := f.witness(t)
	if err := f.r.Get(t.Context(), client.ObjectKey{Namespace: witness.Namespace, Name: recoveryBootSecretName(witness)}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatal("fault did not leave the witnessed boot without retained authentication")
	}
	return f, witness
}

func TestAgentRuntimeRecoveryResumesRetentionAfterTakeover(t *testing.T) {
	for _, point := range []string{"pod-retention", "boot-auth"} {
		t.Run(point, func(t *testing.T) {
			f, witness := interruptedRuntimeRecoveryEnrollment(t, point)
			f.advanceEpoch(t)
			f.reconcile(t)
			f.reconcile(t)
			retired, err := loadAgentRuntimeBootRetirement(t.Context(), f.control, witness)
			if err != nil || !retired {
				t.Fatalf("successor could not retire the interrupted witnessed boot: retired=%t err=%v", retired, err)
			}
			_, _, epoch, err := f.r.recoveryDeployment(t.Context(), f.runtime)
			if err != nil || epoch != uint64(f.fence.Epoch) {
				t.Fatal("successor could not replace the retired boot's epoch")
			}
			if f.runtime.Status.Ready || f.server.Counts().SessionCreates != 0 || f.server.Counts().PromptStarts != 0 {
				t.Fatal("partial enrollment recovery admitted lifecycle work")
			}
			secret := &corev1.Secret{}
			if err := f.r.Get(t.Context(), client.ObjectKey{Namespace: witness.Namespace, Name: recoveryBootSecretName(witness)}, secret); err != nil ||
				secret.Immutable == nil || !*secret.Immutable {
				t.Fatal("successor did not retain immutable authentication for the original boot")
			}
			pod := &corev1.Pod{}
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), pod); err != nil || !controllerutil.ContainsFinalizer(pod, agentRuntimeRecoveryPodFinalizer) {
				t.Fatal("successor did not retain the witnessed Pod through replacement")
			}
		})
	}
}

func TestAgentRuntimeRecoveryDeletesInterruptedEnrollment(t *testing.T) {
	for _, point := range []string{"pod-retention", "boot-auth"} {
		t.Run(point, func(t *testing.T) {
			f, witness := interruptedRuntimeRecoveryEnrollment(t, point)
			f.advanceEpoch(t)
			if err := f.r.Delete(t.Context(), f.runtime); err != nil {
				t.Fatal(err)
			}
			for range 5 {
				if _, err := f.r.Reconcile(t.Context(), reconcileRequestFor(f.runtime)); err != nil {
					t.Fatalf("interrupted enrollment could not finish deletion: %v", err)
				}
				current := &corev1alpha1.AgentRuntime{}
				if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.runtime), current); apierrors.IsNotFound(err) {
					break
				} else if err != nil {
					t.Fatal(err)
				}
			}
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.runtime), &corev1alpha1.AgentRuntime{}); !apierrors.IsNotFound(err) {
				t.Fatal("interrupted enrollment retained its AgentRuntime finalizer")
			}
			retired, err := loadAgentRuntimeBootRetirement(t.Context(), f.control, witness)
			if err != nil || !retired {
				t.Fatal("partial enrollment deletion lacked original-boot retirement proof")
			}
			pod := &corev1.Pod{}
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), pod); err != nil || controllerutil.ContainsFinalizer(pod, agentRuntimeRecoveryPodFinalizer) {
				t.Fatal("partial enrollment deletion orphaned Pod retention")
			}
			if err := f.r.Get(t.Context(), client.ObjectKey{Namespace: witness.Namespace, Name: recoveryBootSecretName(witness)}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
				t.Fatal("partial enrollment deletion orphaned retained boot authentication")
			}
			if f.server.Counts().SessionCreates != 0 || f.server.Counts().PromptStarts != 0 {
				t.Fatal("partial enrollment deletion admitted lifecycle work")
			}
		})
	}
}

func TestAgentRuntimeRecoveryRetentionCannotRebindWitness(t *testing.T) {
	for _, change := range []string{"auth-version", "auth-uid", "container"} {
		t.Run(change, func(t *testing.T) {
			f, witness := interruptedRuntimeRecoveryEnrollment(t, "boot-auth")
			if change == "container" {
				pod := &corev1.Pod{}
				if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), pod); err != nil {
					t.Fatal(err)
				}
				pod.Status.ContainerStatuses[0].ContainerID = "containerd://replacement"
				pod.Status.ContainerStatuses[0].RestartCount++
				if err := f.r.Status().Update(t.Context(), pod); err != nil {
					t.Fatal(err)
				}
			} else {
				secret := &corev1.Secret{}
				key := client.ObjectKey{Namespace: f.runtime.Namespace, Name: f.runtime.Spec.ClientAuth.ControllerBearerTokenSecretRef.Name}
				if err := f.r.Get(t.Context(), key, secret); err != nil {
					t.Fatal(err)
				}
				if change == "auth-uid" {
					if err := f.r.Delete(t.Context(), secret); err != nil {
						t.Fatal(err)
					}
					secret.UID, secret.ResourceVersion = types.UID("replacement-auth-uid"), ""
					if err := f.r.Create(t.Context(), secret); err != nil {
						t.Fatal(err)
					}
				} else {
					if secret.Labels == nil {
						secret.Labels = map[string]string{}
					}
					secret.Labels["fixture-change"] = "new-version"
					if err := f.r.Update(t.Context(), secret); err != nil {
						t.Fatal(err)
					}
				}
			}
			f.advanceEpoch(t)
			f.reconcile(t)
			if err := f.r.Get(t.Context(), client.ObjectKey{Namespace: witness.Namespace, Name: recoveryBootSecretName(witness)}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
				t.Fatal("changed authority was retained as the original boot's authentication")
			}
			retired, err := loadAgentRuntimeBootRetirement(t.Context(), f.control, witness)
			if err != nil || retired || f.runtime.Status.Ready || f.server.Counts().SessionCreates != 0 || f.server.Counts().PromptStarts != 0 {
				t.Fatal("changed authority retired or admitted the original boot")
			}
			if !controllerutil.ContainsFinalizer(f.runtime, agentRuntimeFinalizer) {
				t.Fatal("unresolved original boot lost its durable owner")
			}
		})
	}
}
