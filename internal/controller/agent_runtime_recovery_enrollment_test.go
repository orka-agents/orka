package controller

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	storekube "github.com/orka-agents/orka/internal/store/kube"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func unadmittedRecoveryWitness(t *testing.T, f *runtimeRecoveryFixture) agentRuntimeBootWitness {
	t.Helper()
	backend, err := f.r.recoveryBackend(t.Context(), f.runtime)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := f.r.agentRuntimeAuthMaterial(t.Context(), f.runtime)
	if err != nil {
		t.Fatal(err)
	}
	witness := backend.witness
	witness.Fence = f.server.Fence()
	witness.ControllerAuthUID, witness.ControllerAuthVersion = auth.controllerSecretUID, auth.controllerResourceVersion
	witness.CapabilityAuthUID, witness.CapabilityAuthVersion = auth.capabilitySecretUID, auth.capabilityResourceVersion
	return witness
}

func TestRecoveryEnrollmentPodLossBeforeRetention(t *testing.T) {
	f := newRuntimeRecoveryFixture(t)
	witness := unadmittedRecoveryWitness(t, f)
	base := f.r.Client.(client.WithWatch)
	var interrupted atomic.Bool
	f.r.Client = interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, delegate client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if pod, ok := object.(*corev1.Pod); ok && pod.UID == f.pod.UID &&
				controllerutil.ContainsFinalizer(pod, agentRuntimeRecoveryPodFinalizer) && interrupted.CompareAndSwap(false, true) {
				return errors.New("injected Pod retention failure")
			}
			return delegate.Patch(ctx, object, patch, options...)
		},
	})
	f.reconcile(t)
	if !interrupted.Load() || f.runtime.Status.Ready || f.server.Counts().SessionCreates != 0 || f.server.Counts().PromptStarts != 0 {
		t.Fatal("interruption did not keep enrollment closed")
	}
	if _, err := loadAgentRuntimeBootWitness(t.Context(), f.control, f.runtime.Namespace, f.runtime.UID, witness.Fence.SupervisorBootID); !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("unretained Pod acquired committed boot ownership: %v", err)
	}
	pod := &corev1.Pod{}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), pod); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(pod, agentRuntimeRecoveryPodFinalizer) {
		t.Fatal("failed retention changed the original Pod")
	}
	if err := f.r.Delete(t.Context(), pod, deleteCurrentObjectPreconditions(pod)...); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("original unretained Pod is not absent: %v", err)
	}
	deleteUnadmittedRecoveryRuntime(t, f)
	if retired, err := loadAgentRuntimeBootRetirement(t.Context(), f.control, witness); err != nil || retired {
		t.Fatalf("Pod absence became a retirement claim: retired=%t err=%v", retired, err)
	}
}

type interruptedBootPublicationStore struct {
	*storekube.Store
	interrupted atomic.Bool
}

func (s *interruptedBootPublicationStore) TransitionExternalEffect(ctx context.Context, request store.ExternalEffectTransition) (*store.ExternalEffect, error) {
	effect, err := s.GetExternalEffect(ctx, request.ID)
	if err != nil {
		return nil, err
	}
	if effect.Identity.Kind == agentRuntimeBootWitnessKind && s.interrupted.CompareAndSwap(false, true) {
		return nil, errors.New("injected witness publication failure")
	}
	return s.Store.TransitionExternalEffect(ctx, request)
}

func TestRecoveryEnrollmentResumesRetainedUnpublishedPod(t *testing.T) {
	f := newRuntimeRecoveryFixture(t)
	witness := unadmittedRecoveryWitness(t, f)
	fault := &interruptedBootPublicationStore{Store: f.control}
	f.r.ControlStore = fault
	f.reconcile(t)
	if !fault.interrupted.Load() || f.runtime.Status.Ready || f.server.Counts().SessionCreates != 0 || f.server.Counts().PromptStarts != 0 {
		t.Fatal("publication failure did not keep enrollment closed")
	}
	if _, err := loadAgentRuntimeBootWitness(t.Context(), f.control, f.runtime.Namespace, f.runtime.UID, witness.Fence.SupervisorBootID); !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("witness unexpectedly committed: %v", err)
	}
	pod := &corev1.Pod{}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), pod); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(pod, agentRuntimeRecoveryPodFinalizer) {
		t.Fatal("witness publication preceded recoverable Pod retention")
	}
	if err := f.r.Delete(t.Context(), pod, deleteCurrentObjectPreconditions(pod)...); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(pod), pod); err != nil || pod.DeletionTimestamp.IsZero() {
		t.Fatalf("exact retained Pod did not survive normal deletion: %v", err)
	}
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: recoveryTerminal(witness)}
	pod.Status.ContainerStatuses[0].Ready = false
	if err := f.r.Status().Update(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	f.r.ControlStore = f.control
	f.advanceEpoch(t)
	deleteUnadmittedRecoveryRuntime(t, f)
	actual, err := loadAgentRuntimeBootWitness(t.Context(), f.control, f.runtime.Namespace, f.runtime.UID, witness.Fence.SupervisorBootID)
	if err != nil || actual.ContainerID != witness.ContainerID || actual.PodUID != witness.PodUID {
		t.Fatalf("retained prepared observation was not recovered exactly: %v", err)
	}
	if retired, err := loadAgentRuntimeBootRetirement(t.Context(), f.control, witness); err != nil || !retired {
		t.Fatalf("original positive container retirement was lost: retired=%t err=%v", retired, err)
	}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("retired original Pod retained its finalizer: %v", err)
	}
}

func deleteUnadmittedRecoveryRuntime(t *testing.T, f *runtimeRecoveryFixture) {
	t.Helper()
	if err := f.r.Delete(t.Context(), f.runtime); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := f.r.Reconcile(t.Context(), reconcileRequestFor(f.runtime)); err != nil {
			t.Fatalf("ordinary unadmitted runtime deletion failed: %v", err)
		}
		current := &corev1alpha1.AgentRuntime{}
		if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.runtime), current); apierrors.IsNotFound(err) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.runtime), &corev1alpha1.AgentRuntime{}); !apierrors.IsNotFound(err) {
		t.Fatalf("unadmitted runtime could not finish ordinary deletion: %v", err)
	}
	if f.server.Counts().SessionCreates != 0 || f.server.Counts().PromptStarts != 0 {
		t.Fatal("recovery of interrupted enrollment admitted lifecycle work")
	}
}

func TestRecoveryEnrollmentResumesPublicationBeforeConformance(t *testing.T) {
	f := newRuntimeRecoveryFixture(t)
	witness := unadmittedRecoveryWitness(t, f)
	fault := &interruptedBootPublicationStore{Store: f.control}
	f.r.ControlStore = fault
	f.reconcile(t)
	if !fault.interrupted.Load() || f.runtime.Status.Ready || f.server.Counts().SessionCreates != 0 {
		t.Fatal("publication interruption admitted conformance")
	}
	f.r.ControlStore = f.control
	f.reconcile(t)
	if !f.runtime.Status.Ready {
		t.Fatal("retained original boot did not finish enrollment")
	}
	actual := f.witness(t)
	expectedDigest, err := runtimeWitnessDigest(witness)
	if err != nil {
		t.Fatal(err)
	}
	actualDigest, err := runtimeWitnessDigest(actual)
	if err != nil || actualDigest != expectedDigest {
		t.Fatal("publication recovery rebound the original authenticated witness")
	}
}

func TestRecoveryEnrollmentPreparedMetadataCannotRebind(t *testing.T) {
	for _, change := range []string{"json", "unknown-field", "pod-uid", "runtime-uid", "boot", "owner", "missing-body", "missing-markers", "missing-finalizer", "pod-spec"} {
		t.Run(change, func(t *testing.T) {
			f := newRuntimeRecoveryFixture(t)
			witness := unadmittedRecoveryWitness(t, f)
			fault := &interruptedBootPublicationStore{Store: f.control}
			f.r.ControlStore = fault
			f.reconcile(t)
			if !fault.interrupted.Load() || f.runtime.Status.Ready {
				t.Fatal("publication fault did not preserve pending enrollment")
			}
			f.r.ControlStore = f.control
			pod := &corev1.Pod{}
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), pod); err != nil {
				t.Fatal(err)
			}
			mutatePreparedRecoveryPod(t, pod, change)
			if err := f.r.Update(t.Context(), pod); err != nil {
				t.Fatal(err)
			}
			f.reconcile(t)
			if f.runtime.Status.Ready || f.server.Counts().SessionCreates != 0 || f.server.Counts().PromptStarts != 0 {
				t.Fatal("changed prepared observation admitted lifecycle work")
			}
			if _, err := loadAgentRuntimeBootWitness(t.Context(), f.control, f.runtime.Namespace, f.runtime.UID, witness.Fence.SupervisorBootID); !errors.Is(err, store.ErrNotReady) {
				t.Fatalf("changed prepared observation replaced its reservation: %v", err)
			}
			if err := f.r.Delete(t.Context(), f.runtime); err != nil {
				t.Fatal(err)
			}
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.runtime), f.runtime); err != nil {
				t.Fatal(err)
			}
			if _, err := f.r.finalizeKubernetesAgentRuntime(t.Context(), f.runtime); err == nil {
				t.Fatal("invalid retained metadata permitted runtime finalization")
			}
			if !controllerutil.ContainsFinalizer(f.runtime, agentRuntimeFinalizer) {
				t.Fatal("invalid prepared observation lost its original owner")
			}
		})
	}
}

func mutatePreparedRecoveryPod(t *testing.T, pod *corev1.Pod, change string) {
	t.Helper()
	const key = "orka.ai/agent-runtime-boot-witness"
	var witness map[string]any
	if err := json.Unmarshal([]byte(pod.Annotations[key]), &witness); err != nil {
		t.Fatal(err)
	}
	switch change {
	case "json":
		pod.Annotations[key] = "{"
		return
	case "missing-body":
		delete(pod.Annotations, key)
		return
	case "missing-markers":
		delete(pod.Annotations, key)
		delete(pod.Annotations, "orka.ai/agent-runtime-prepared-owner")
		return
	case "owner":
		pod.Annotations["orka.ai/agent-runtime-prepared-owner"] = "other-runtime"
		return
	case "missing-finalizer":
		controllerutil.RemoveFinalizer(pod, agentRuntimeRecoveryPodFinalizer)
		return
	case "pod-spec":
		pod.Spec.Containers[0].Image = "changed-executable"
		return
	case "unknown-field":
		witness["unrecognized"] = true
	case "pod-uid":
		witness["podUID"] = "replacement-pod"
	case "runtime-uid":
		witness["runtimeUID"] = "replacement-runtime"
	case "boot":
		witness["fence"].(map[string]any)["supervisorBootID"] = "replacement-boot"
	default:
		t.Fatal("unsupported prepared metadata mutation")
	}
	body, err := json.Marshal(witness)
	if err != nil {
		t.Fatal(err)
	}
	pod.Annotations[key] = string(body)
}

func TestRecoveryEnrollmentPreservesOperatorPodAnnotations(t *testing.T) {
	f := newRuntimeRecoveryFixture(t)
	pod := &corev1.Pod{}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), pod); err != nil {
		t.Fatal(err)
	}
	pod.Annotations = map[string]string{agentRuntimeRecoveryOwnerAnnotation: string(f.runtime.UID), "operator.example/identity": "original"}
	if err := f.r.Update(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	if !f.runtime.Status.Ready {
		t.Fatal("operator annotations prevented boot enrollment")
	}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatal(err)
	}
	if pod.Annotations[agentRuntimeRecoveryOwnerAnnotation] != string(f.runtime.UID) || pod.Annotations["operator.example/identity"] != "original" {
		t.Fatal("enrollment replaced operator Pod annotations")
	}
}

func TestRecoveryEnrollmentLatePodRetentionCannotOutliveRuntime(t *testing.T) {
	f := newRuntimeRecoveryFixture(t)
	base := f.r.Client.(client.WithWatch)
	var delayedPod *corev1.Pod
	var delayedPatch []byte
	f.r.Client = interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, delegate client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if pod, ok := object.(*corev1.Pod); ok && pod.UID == f.pod.UID && delayedPod == nil &&
				controllerutil.ContainsFinalizer(pod, agentRuntimeRecoveryPodFinalizer) {
				var err error
				delayedPatch, err = patch.Data(pod)
				if err != nil {
					return err
				}
				delayedPod = pod.DeepCopy()
				return context.DeadlineExceeded
			}
			return delegate.Patch(ctx, object, patch, options...)
		},
	})
	f.reconcile(t)
	if delayedPod == nil || f.runtime.Status.Ready || f.server.Counts().SessionCreates != 0 {
		t.Fatal("retention timeout did not keep conformance closed")
	}
	if err := f.r.Delete(t.Context(), f.runtime); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := f.r.Reconcile(t.Context(), reconcileRequestFor(f.runtime)); err != nil {
			t.Fatal(err)
		}
		if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.runtime), &corev1alpha1.AgentRuntime{}); apierrors.IsNotFound(err) {
			break
		}
	}
	// Model only the already-sent PATCH completing after the timed-out call.
	// Its exact optimistic resourceVersion must not be able to install an
	// unowned Pod finalizer after runtime finalization.
	lateErr := base.Patch(t.Context(), delayedPod, client.RawPatch(types.MergePatchType, delayedPatch))
	if lateErr != nil && !apierrors.IsConflict(lateErr) {
		t.Fatalf("delayed original patch = %v", lateErr)
	}
	pod := &corev1.Pod{}
	if err := base.Get(t.Context(), client.ObjectKeyFromObject(f.pod), pod); err != nil {
		t.Fatal(err)
	}
	runtimeErr := base.Get(t.Context(), client.ObjectKeyFromObject(f.runtime), &corev1alpha1.AgentRuntime{})
	if apierrors.IsNotFound(runtimeErr) && controllerutil.ContainsFinalizer(pod, agentRuntimeRecoveryPodFinalizer) {
		t.Fatal("delayed retention installed a Pod finalizer after its runtime owner was deleted")
	}
	if runtimeErr != nil && !apierrors.IsNotFound(runtimeErr) {
		t.Fatal(runtimeErr)
	}
	if f.server.Counts().SessionCreates != 0 || f.server.Counts().PromptStarts != 0 {
		t.Fatal("ambiguous enrollment admitted lifecycle work")
	}
}

func TestRecoveryEnrollmentPreparationDuringFinalizationRetainsOwner(t *testing.T) {
	f := newRuntimeRecoveryFixture(t)
	witness := unadmittedRecoveryWitness(t, f)
	if err := f.r.retainRecoveryRuntime(t.Context(), f.runtime, f.fence); err != nil {
		t.Fatal(err)
	}
	digest, err := runtimeWitnessDigest(witness)
	if err != nil {
		t.Fatal(err)
	}
	identity := agentRuntimeRecoveryIdentity(agentRuntimeBootWitnessKind, f.runtime.UID, f.runtime.Namespace, string(witness.Fence.SupervisorBootID))
	if _, err := f.control.ReserveExternalEffect(t.Context(), store.ReserveExternalEffectRequest{
		Identity: identity, RequestDigest: digest, Fence: f.fence, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Delete(t.Context(), f.runtime); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.runtime), f.runtime); err != nil {
		t.Fatal(err)
	}
	base := f.r.Client.(client.WithWatch)
	var effectScans atomic.Int32
	f.r.APIReader = interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, delegate client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
			if err := delegate.List(ctx, list, options...); err != nil {
				return err
			}
			if _, ok := list.(*corev1alpha1.ExternalEffectList); ok && effectScans.Add(1) == 1 {
				// The first scan has already observed the reservation. Publish
				// the original preparation before the final removal guard runs.
				preparation := identity
				preparation.Kind = agentRuntimeBootPreparationKind
				return persistAgentRuntimeRecoveryEffect(ctx, f.control, f.fence, preparation, digest, witness)
			}
			return nil
		},
	})
	if _, err := f.r.finalizeKubernetesAgentRuntime(t.Context(), f.runtime); !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("preparation appearing during finalization did not defer owner removal: %v", err)
	}
	current := &corev1alpha1.AgentRuntime{}
	if err := base.Get(t.Context(), client.ObjectKeyFromObject(f.runtime), current); err != nil {
		t.Fatal(err)
	}
	if effectScans.Load() < 3 || !controllerutil.ContainsFinalizer(current, agentRuntimeFinalizer) {
		t.Fatal("finalization did not recheck the preparation before removing its owner")
	}
	if _, err := loadAgentRuntimeBootWitness(t.Context(), f.control, f.runtime.Namespace, f.runtime.UID, witness.Fence.SupervisorBootID); !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("a preparation became admission authority: %v", err)
	}
	if f.server.Counts().SessionCreates != 0 || f.server.Counts().PromptStarts != 0 {
		t.Fatal("a concurrent preparation admitted lifecycle work")
	}
}

func TestRecoveryEnrollmentFencesDelayedOwnerRemoval(t *testing.T) {
	f := newRuntimeRecoveryFixture(t)
	witness := unadmittedRecoveryWitness(t, f)
	if err := f.r.retainRecoveryRuntime(t.Context(), f.runtime, f.fence); err != nil {
		t.Fatal(err)
	}
	digest, err := runtimeWitnessDigest(witness)
	if err != nil {
		t.Fatal(err)
	}
	preparation := agentRuntimeRecoveryIdentity(agentRuntimeBootPreparationKind, f.runtime.UID, f.runtime.Namespace, string(witness.Fence.SupervisorBootID))
	for _, kind := range []string{agentRuntimeBootWitnessKind, agentRuntimeBootPreparationKind} {
		identity := preparation
		identity.Kind = kind
		if _, err := f.control.ReserveExternalEffect(t.Context(), store.ReserveExternalEffectRequest{
			Identity: identity, RequestDigest: digest, Fence: f.fence, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.r.Delete(t.Context(), f.runtime); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.runtime), f.runtime); err != nil {
		t.Fatal(err)
	}
	base := f.r.Client.(client.WithWatch)
	var delayedOwner *corev1alpha1.AgentRuntime
	var delayedPatch []byte
	f.r.Client = interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, delegate client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if owner, ok := object.(*corev1alpha1.AgentRuntime); ok && owner.UID == f.runtime.UID && delayedOwner == nil &&
				!controllerutil.ContainsFinalizer(owner, agentRuntimeFinalizer) {
				var err error
				delayedPatch, err = patch.Data(owner)
				if err != nil {
					return err
				}
				delayedOwner = owner.DeepCopy()
				return context.DeadlineExceeded
			}
			return delegate.Patch(ctx, object, patch, options...)
		},
	})
	if _, err := f.r.finalizeKubernetesAgentRuntime(t.Context(), f.runtime); !errors.Is(err, context.DeadlineExceeded) || delayedOwner == nil {
		t.Fatalf("owner removal was not left in flight: %v", err)
	}
	// The original preparation publication, whose caller timed out, commits
	// after the owner-removal request was sent. Neither write is replayed.
	if err := persistAgentRuntimeRecoveryEffect(t.Context(), f.control, f.fence, preparation, digest, witness); err != nil {
		t.Fatal(err)
	}
	if err := f.r.resumePreparedRecoveryPods(t.Context(), f.runtime, f.fence); err != nil {
		t.Fatal(err)
	}
	if err := base.Patch(t.Context(), delayedOwner, client.RawPatch(types.MergePatchType, delayedPatch)); !apierrors.IsConflict(err) {
		t.Fatalf("late owner removal still applied after original Pod retention: %v", err)
	}
	current := &corev1alpha1.AgentRuntime{}
	if err := base.Get(t.Context(), client.ObjectKeyFromObject(f.runtime), current); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{}
	if err := base.Get(t.Context(), client.ObjectKeyFromObject(f.pod), pod); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(current, agentRuntimeFinalizer) || !controllerutil.ContainsFinalizer(pod, agentRuntimeRecoveryPodFinalizer) {
		t.Fatal("recovered boot lost its original owner or Pod retention")
	}
	if f.server.Counts().SessionCreates != 0 || f.server.Counts().PromptStarts != 0 {
		t.Fatal("preparation recovery admitted lifecycle work")
	}
}

func legacyRetainedRecoveryFixture(t *testing.T) (*runtimeRecoveryFixture, agentRuntimeBootWitness) {
	t.Helper()
	f := newRuntimeRecoveryFixture(t)
	witness := unadmittedRecoveryWitness(t, f)
	if err := f.r.retainRecoveryRuntime(t.Context(), f.runtime, f.fence); err != nil {
		t.Fatal(err)
	}
	digest, err := runtimeWitnessDigest(witness)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistAgentRuntimeRecoveryEffect(t.Context(), f.control, f.fence,
		agentRuntimeRecoveryIdentity(agentRuntimeBootWitnessKind, witness.RuntimeUID, witness.Namespace, string(witness.Fence.SupervisorBootID)), digest, witness); err != nil {
		t.Fatal(err)
	}
	// This is the exact witness-first format emitted before preparations:
	// the Pod is retained, but neither new annotation exists.
	controllerutil.AddFinalizer(f.pod, agentRuntimeRecoveryPodFinalizer)
	if err := f.r.Update(t.Context(), f.pod); err != nil {
		t.Fatal(err)
	}
	return f, witness
}

func TestRecoveryEnrollmentResumesLegacyRetainedPod(t *testing.T) {
	f, old := legacyRetainedRecoveryFixture(t)
	f.restartContainer(t, true, false)
	witness := unadmittedRecoveryWitness(t, f)
	base := f.r.Client.(client.WithWatch)
	var interrupted atomic.Bool
	f.r.Client = interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, delegate client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if pod, ok := object.(*corev1.Pod); ok && pod.UID == f.pod.UID &&
				pod.Annotations[agentRuntimePreparedWitnessAnnotation] != "" && interrupted.CompareAndSwap(false, true) {
				return errors.New("injected legacy rollover annotation failure")
			}
			return delegate.Patch(ctx, object, patch, options...)
		},
	})
	f.reconcile(t)
	if !interrupted.Load() || f.runtime.Status.Ready || f.server.Counts().SessionCreates != 0 || f.server.Counts().PromptStarts != 0 {
		t.Fatal("legacy rollover interruption did not stop enrollment before conformance")
	}
	if retired, err := loadAgentRuntimeBootRetirement(t.Context(), f.control, old); err != nil || !retired {
		t.Fatalf("rollover lacks exact old-container retirement: %v", err)
	}
	if _, err := loadAgentRuntimeBootWitness(t.Context(), f.control, f.runtime.Namespace, f.runtime.UID, witness.Fence.SupervisorBootID); !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("unpublished replacement acquired admission authority: %v", err)
	}
	f.r.Client = base
	f.reconcile(t)
	if !f.runtime.Status.Ready {
		t.Fatalf("original legacy-retained Pod did not resume enrollment: %s", f.runtime.Status.Message)
	}
	actual := f.witness(t)
	if actual.Fence != witness.Fence || actual.PodUID != witness.PodUID || actual.ContainerID != witness.ContainerID {
		t.Fatal("legacy rollover resumed a different boot or Pod")
	}
}

func TestRecoveryEnrollmentOwnerFenceFailureDoesNotRetainPod(t *testing.T) {
	f := newRuntimeRecoveryFixture(t)
	witness := unadmittedRecoveryWitness(t, f)
	base := f.r.Client.(client.WithWatch)
	var interrupted atomic.Bool
	f.r.Client = interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, delegate client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if owner, ok := object.(*corev1alpha1.AgentRuntime); ok && owner.UID == f.runtime.UID &&
				owner.Annotations[agentRuntimeRetentionVersionAnnotation] != "" {
				interrupted.Store(true)
				return context.DeadlineExceeded
			}
			return delegate.Patch(ctx, object, patch, options...)
		},
	})
	f.reconcile(t)
	if !interrupted.Load() || f.runtime.Status.Ready {
		t.Fatal("owner-fence timeout did not stop enrollment")
	}
	pod := &corev1.Pod{}
	if err := base.Get(t.Context(), client.ObjectKeyFromObject(f.pod), pod); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(pod, agentRuntimeRecoveryPodFinalizer) || pod.Annotations[agentRuntimePreparedWitnessAnnotation] != "" {
		t.Fatal("uncertain owner fencing installed Pod retention")
	}
	if _, err := loadAgentRuntimeBootWitness(t.Context(), f.control, f.runtime.Namespace, f.runtime.UID, witness.Fence.SupervisorBootID); !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("uncertain owner fencing published admission authority: %v", err)
	}
	if f.server.Counts().SessionCreates != 0 || f.server.Counts().PromptStarts != 0 {
		t.Fatal("uncertain owner fencing admitted lifecycle work")
	}
}

func TestRecoveryEnrollmentLegacyRetentionRequiresRetirementAndOldFormat(t *testing.T) {
	for _, state := range []string{"unretired", "new-format"} {
		t.Run(state, func(t *testing.T) {
			f, old := legacyRetainedRecoveryFixture(t)
			f.restartContainer(t, true, false)
			if state == "new-format" {
				if retired, err := f.r.observeContainerRetirement(t.Context(), old, f.fence); err != nil || !retired {
					t.Fatalf("original container retirement failed: %v", err)
				}
				digest, err := runtimeWitnessDigest(old)
				if err != nil {
					t.Fatal(err)
				}
				if err := persistAgentRuntimeRecoveryEffect(t.Context(), f.control, f.fence,
					agentRuntimeRecoveryIdentity(agentRuntimeBootPreparationKind, old.RuntimeUID, old.Namespace, string(old.Fence.SupervisorBootID)), digest, old); err != nil {
					t.Fatal(err)
				}
			}
			witness := unadmittedRecoveryWitness(t, f)
			digest, err := runtimeWitnessDigest(witness)
			if err != nil {
				t.Fatal(err)
			}
			if err := persistAgentRuntimeRecoveryEffect(t.Context(), f.control, f.fence,
				agentRuntimeRecoveryIdentity(agentRuntimeBootPreparationKind, witness.RuntimeUID, witness.Namespace, string(witness.Fence.SupervisorBootID)), digest, witness); err != nil {
				t.Fatal(err)
			}
			if err := f.r.resumePreparedRecoveryPods(t.Context(), f.runtime, f.fence); err == nil {
				t.Fatal("unproved or new-format retention was accepted as a legacy rollover")
			}
			pod := &corev1.Pod{}
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), pod); err != nil {
				t.Fatal(err)
			}
			if !controllerutil.ContainsFinalizer(pod, agentRuntimeRecoveryPodFinalizer) || pod.Annotations[agentRuntimePreparedWitnessAnnotation] != "" {
				t.Fatal("rejected legacy recovery changed original retention")
			}
			if _, err := loadAgentRuntimeBootWitness(t.Context(), f.control, f.runtime.Namespace, f.runtime.UID, witness.Fence.SupervisorBootID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("rejected legacy recovery published a replacement boot: %v", err)
			}
		})
	}
}

func TestRecoveryEnrollmentOwnerRemovalBeforeLatePreparation(t *testing.T) {
	f := newRuntimeRecoveryFixture(t)
	witness := unadmittedRecoveryWitness(t, f)
	if err := f.r.retainRecoveryRuntime(t.Context(), f.runtime, f.fence); err != nil {
		t.Fatal(err)
	}
	digest, err := runtimeWitnessDigest(witness)
	if err != nil {
		t.Fatal(err)
	}
	preparation := agentRuntimeRecoveryIdentity(agentRuntimeBootPreparationKind, f.runtime.UID, f.runtime.Namespace, string(witness.Fence.SupervisorBootID))
	for _, kind := range []string{agentRuntimeBootWitnessKind, agentRuntimeBootPreparationKind} {
		identity := preparation
		identity.Kind = kind
		if _, err := f.control.ReserveExternalEffect(t.Context(), store.ReserveExternalEffectRequest{
			Identity: identity, RequestDigest: digest, Fence: f.fence, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.r.Delete(t.Context(), f.runtime); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.runtime), f.runtime); err != nil {
		t.Fatal(err)
	}
	base := f.r.Client.(client.WithWatch)
	var delayedOwner *corev1alpha1.AgentRuntime
	var delayedPatch []byte
	f.r.Client = interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, delegate client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if owner, ok := object.(*corev1alpha1.AgentRuntime); ok && owner.UID == f.runtime.UID && delayedOwner == nil &&
				!controllerutil.ContainsFinalizer(owner, agentRuntimeFinalizer) {
				var err error
				delayedPatch, err = patch.Data(owner)
				if err != nil {
					return err
				}
				delayedOwner = owner.DeepCopy()
				return context.DeadlineExceeded
			}
			return delegate.Patch(ctx, object, patch, options...)
		},
	})
	if _, err := f.r.finalizeKubernetesAgentRuntime(t.Context(), f.runtime); !errors.Is(err, context.DeadlineExceeded) || delayedOwner == nil {
		t.Fatalf("owner removal was not left in flight: %v", err)
	}
	// Both original writes complete after their callers timed out. Owner
	// removal wins before any Pod retention request has been sent.
	if err := persistAgentRuntimeRecoveryEffect(t.Context(), f.control, f.fence, preparation, digest, witness); err != nil {
		t.Fatal(err)
	}
	if err := base.Patch(t.Context(), delayedOwner, client.RawPatch(types.MergePatchType, delayedPatch)); err != nil {
		t.Fatal(err)
	}
	f.r.Client = base
	for range 5 {
		if _, err := f.r.Reconcile(t.Context(), reconcileRequestFor(f.runtime)); err != nil {
			t.Fatalf("late unpublished preparation stranded Secret GC: %v", err)
		}
		if err := base.Get(t.Context(), client.ObjectKeyFromObject(f.runtime), &corev1alpha1.AgentRuntime{}); apierrors.IsNotFound(err) {
			break
		}
	}
	if err := base.Get(t.Context(), client.ObjectKeyFromObject(f.runtime), &corev1alpha1.AgentRuntime{}); !apierrors.IsNotFound(err) {
		t.Fatalf("released unadmitted runtime did not finish deletion: %v", err)
	}
	pod := &corev1.Pod{}
	if err := base.Get(t.Context(), client.ObjectKeyFromObject(f.pod), pod); err != nil {
		t.Fatal(err)
	}
	if pod.UID != witness.PodUID || controllerutil.ContainsFinalizer(pod, agentRuntimeRecoveryPodFinalizer) ||
		pod.Annotations[agentRuntimePreparedWitnessAnnotation] != "" || pod.Annotations[agentRuntimePreparedOwnerAnnotation] != "" {
		t.Fatal("late preparation retained a Pod after its owner was released")
	}
	if _, err := loadAgentRuntimeBootWitness(t.Context(), f.control, witness.Namespace, witness.RuntimeUID, witness.Fence.SupervisorBootID); !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("late preparation published admission authority: %v", err)
	}
	if retired, err := loadAgentRuntimeBootRetirement(t.Context(), f.control, witness); err != nil || retired {
		t.Fatalf("unpublished preparation became a retirement claim: retired=%t err=%v", retired, err)
	}
	if f.server.Counts().SessionCreates != 0 || f.server.Counts().PromptStarts != 0 {
		t.Fatal("released enrollment admitted lifecycle work")
	}
}

func TestRecoveryEnrollmentResumesInterruptedLegacyMigration(t *testing.T) {
	f, old := legacyRetainedRecoveryFixture(t)
	base := f.r.Client.(client.WithWatch)
	var interruptions atomic.Int32
	f.r.Client = interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, delegate client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if pod, ok := object.(*corev1.Pod); ok && pod.UID == f.pod.UID && pod.Annotations[agentRuntimePreparedWitnessAnnotation] != "" {
				interruptions.Add(1)
				return context.DeadlineExceeded
			}
			return delegate.Patch(ctx, object, patch, options...)
		},
	})
	f.reconcile(t)
	if interruptions.Load() != 1 || f.runtime.Status.Ready {
		t.Fatal("legacy metadata migration did not stop before conformance")
	}
	f.restartContainer(t, true, false)
	witness := unadmittedRecoveryWitness(t, f)
	f.reconcile(t)
	if interruptions.Load() != 2 || f.runtime.Status.Ready || f.server.Counts().SessionCreates != 0 || f.server.Counts().PromptStarts != 0 {
		t.Fatal("interrupted legacy rollover did not preserve admission barriers")
	}
	if retired, err := loadAgentRuntimeBootRetirement(t.Context(), f.control, old); err != nil || !retired {
		t.Fatalf("rollover lacks exact original-container retirement: %v", err)
	}
	f.r.Client = base
	f.reconcile(t)
	if !f.runtime.Status.Ready {
		t.Fatalf("interrupted migration prevented legacy rollover recovery: %s", f.runtime.Status.Message)
	}
	actual := f.witness(t)
	if actual.Fence != witness.Fence || actual.PodUID != witness.PodUID || actual.ContainerID != witness.ContainerID {
		t.Fatal("interrupted legacy migration rebound the original replacement observation")
	}
}

func TestRecoveryEnrollmentReleasedOwnerPreservesRetentionBarriers(t *testing.T) {
	for _, state := range []string{"unretained", "retained", "owner-marker", "witness-marker", "published"} {
		t.Run(state, func(t *testing.T) {
			f := newRuntimeRecoveryFixture(t)
			witness := unadmittedRecoveryWitness(t, f)
			if err := f.r.retainRecoveryRuntime(t.Context(), f.runtime, f.fence); err != nil {
				t.Fatal(err)
			}
			digest, err := runtimeWitnessDigest(witness)
			if err != nil {
				t.Fatal(err)
			}
			preparation := agentRuntimeRecoveryIdentity(agentRuntimeBootPreparationKind, witness.RuntimeUID, witness.Namespace, string(witness.Fence.SupervisorBootID))
			if err := persistAgentRuntimeRecoveryEffect(t.Context(), f.control, f.fence, preparation, digest, witness); err != nil {
				t.Fatal(err)
			}
			if state == "published" {
				identity := preparation
				identity.Kind = agentRuntimeBootWitnessKind
				if err := persistAgentRuntimeRecoveryEffect(t.Context(), f.control, f.fence, identity, digest, witness); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.r.Delete(t.Context(), f.runtime); err != nil {
				t.Fatal(err)
			}
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.runtime), f.runtime); err != nil {
				t.Fatal(err)
			}
			base := f.runtime.DeepCopy()
			controllerutil.RemoveFinalizer(f.runtime, agentRuntimeFinalizer)
			if err := f.r.Patch(t.Context(), f.runtime, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				t.Fatal(err)
			}
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), f.pod); err != nil {
				t.Fatal(err)
			}
			switch state {
			case "retained":
				controllerutil.AddFinalizer(f.pod, agentRuntimeRecoveryPodFinalizer)
			case "owner-marker":
				f.pod.Annotations = map[string]string{agentRuntimePreparedOwnerAnnotation: string(witness.RuntimeUID)}
			case "witness-marker":
				f.pod.Annotations = map[string]string{agentRuntimePreparedWitnessAnnotation: "retained observation"}
			}
			if err := f.r.Update(t.Context(), f.pod); err != nil {
				t.Fatal(err)
			}
			err = f.r.requireSettledRecoveryPreparations(t.Context(), f.runtime)
			if state == "unretained" && err != nil {
				t.Fatalf("unpublished unretained preparation was not discharged: %v", err)
			}
			if state != "unretained" && !errors.Is(err, store.ErrNotReady) {
				t.Fatalf("released owner discarded %s recovery ownership: %v", state, err)
			}
			if retired, err := loadAgentRuntimeBootRetirement(t.Context(), f.control, witness); err != nil || retired {
				t.Fatalf("retention check claimed runtime retirement: retired=%t err=%v", retired, err)
			}
		})
	}
}
