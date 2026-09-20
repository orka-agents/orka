package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	storekube "github.com/orka-agents/orka/internal/store/kube"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type nativePoolBootFixture struct {
	r          *RuntimePoolReconciler
	pool       *corev1alpha1.RuntimePool
	pod        corev1.Pod
	supervisor *fakeRuntimePoolSupervisorClient
	control    *storekube.Store
}

func newNativePoolBootFixture(t *testing.T, crossNamespace bool) *nativePoolBootFixture {
	t.Helper()
	pool := runtimePoolTestObject(1)
	supervisor := &fakeRuntimePoolSupervisorClient{}
	r := runtimePoolTestReconciler(t, runtimePoolTestScheme(t), supervisor, pool)
	namespace := pool.Namespace
	if crossNamespace {
		namespace = "native-runtimes"
		r.RuntimeNamespace = namespace
	}
	runtimePoolReconcile(t, r, pool)
	deployment := runtimePoolTestDeployment(t, r, namespace, runtimePoolResourceName(pool.Namespace, pool.Name))
	pod := runtimePoolReadyPodForDeployment(pool, deployment, "native-boot-pod", "native-boot-pod-uid", "10.0.0.83")
	runtimePoolTestCreatePod(t, r, &pod)
	supervisor.probe = runtimePoolValidProbe(pool, &pod, "native-boot", false)
	return &nativePoolBootFixture{r: r, pool: pool, pod: pod, supervisor: supervisor, control: r.ControlStore.(*storekube.Store)}
}

func (f *nativePoolBootFixture) serve(t *testing.T) {
	t.Helper()
	runtimePoolReconcile(t, f.r, f.pool)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing || pool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionAccepting {
		t.Fatal("native fixture did not serve after enrollment")
	}
}

func (f *nativePoolBootFixture) witness(t *testing.T) runtimePoolBootWitness {
	t.Helper()
	w, err := loadRuntimePoolBootWitness(t.Context(), f.control, agentRuntimeRecoveryIdentity(runtimePoolBootWitnessKind, f.pool.UID, f.pool.Namespace, string(f.supervisor.probe.Status.Fence.RuntimeInstanceID)))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func (f *nativePoolBootFixture) task(t *testing.T, name string) *corev1alpha1.Task {
	t.Helper()
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	task := runtimePoolRetirementTask(t, &pool, name)
	if err := f.r.Create(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	return task
}

func (f *nativePoolBootFixture) terminate(t *testing.T, deletePod bool) {
	t.Helper()
	pod := &corev1.Pod{}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&f.pod), pod); err != nil {
		t.Fatal(err)
	}
	if deletePod {
		if err := f.r.Delete(t.Context(), pod); err != nil {
			t.Fatal(err)
		}
		if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&f.pod), pod); err != nil {
			t.Fatal(err)
		}
	}
	status := f.pod.Status.ContainerStatuses[0]
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ContainerID: status.ContainerID, StartedAt: status.State.Running.StartedAt,
		FinishedAt: metav1.NewTime(runtimePoolTestNow), Message: "application diagnostics must not enter proof",
	}}
	pod.Status.ContainerStatuses[0].Ready = false
	pod.Status.Phase = corev1.PodFailed
	if err := f.r.Status().Update(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
}

func (f *nativePoolBootFixture) assertNoReceipt(t *testing.T, task *corev1alpha1.Task) {
	t.Helper()
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(task), task); err != nil {
		t.Fatal(err)
	}
	if task.Status.Execution.RuntimeSessionCleanupDigest != "" {
		t.Fatal("unproven native boot acquired a cleanup receipt")
	}
}

func TestRuntimePoolBootTerminationRetiresHistoricalTurns(t *testing.T) {
	for _, crossNamespace := range []bool{false, true} {
		t.Run(fmt.Sprintf("cross_namespace_%t", crossNamespace), func(t *testing.T) {
			f := newNativePoolBootFixture(t, crossNamespace)
			f.serve(t)
			w := f.witness(t)
			tasks := []*corev1alpha1.Task{f.task(t, "old-native-turn-1"), f.task(t, "old-native-turn-2")}
			original := []*corev1alpha1.Task{tasks[0].DeepCopy(), tasks[1].DeepCopy()}
			if err := f.r.Delete(t.Context(), &f.pod); err != nil {
				t.Fatal(err)
			}
			if err := f.r.reconcileNativeRuntimePoolRetirement(t.Context(), f.pool); err != nil {
				t.Fatal(err)
			}
			for _, task := range tasks {
				f.assertNoReceipt(t, task)
			}
			f.terminate(t, false)
			runtimePoolReconcile(t, f.r, f.pool)
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&f.pod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
				t.Fatalf("retired Pod not released: %v", err)
			}
			current := runtimePoolTestGetPool(t, f.r, f.pool)
			current.Spec.Runtime.Profile.Model = runtimePoolTestNextModel
			current.Generation++
			runtimePoolTestRefreshProfileDigest(t, &current)
			if err := f.r.Update(t.Context(), &current); err != nil {
				t.Fatal(err)
			}
			// A fresh reconciler and a different current profile/active pointer
			// must consume the old boot, not contact a replacement supervisor.
			restarted := &RuntimePoolReconciler{Client: f.r.Client, APIReader: f.r.APIReader, ControlStore: f.control, Epochs: f.r.Epochs, RuntimeNamespace: f.r.RuntimeNamespace}
			if err := restarted.reconcileNativeRuntimePoolRetirement(t.Context(), f.pool); err != nil {
				t.Fatal(err)
			}
			for i, task := range tasks {
				if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(task), task); err != nil {
					t.Fatal(err)
				}
				if !runtimeSessionCleanupCompleteForUID(task, task.UID) {
					t.Fatal("exact container retirement lost a historical Task receipt")
				}
				proofTask := original[i].DeepCopy() // also works before the eager Task receipt write
				if retired, err := verifiedNativeRuntimePoolRetirement(t.Context(), f.control, proofTask, proofTask.UID); err != nil || !retired {
					t.Fatalf("read-only retirement gate: %t %v", retired, err)
				}
				task.Status.Execution.RuntimeSessionCleanupDigest = ""
				if !reflect.DeepEqual(task.Status, original[i].Status) {
					t.Fatal("retirement changed Task outcome, projection, or runtime authority")
				}
			}
			var proof agentRuntimeBootRetirement
			if _, err := readAgentRuntimeRecoveryEffect(t.Context(), f.control, w.identity(runtimePoolBootRetirementKind), &proof); err != nil {
				t.Fatal(err)
			}
			if proof.ContainerTermination == nil || proof.ContainerTermination.Message != "" || proof.ContainerTermination.Reason != "" {
				t.Fatal("retirement persisted application diagnostic text")
			}
		})
	}
}

func TestRuntimePoolBootNoAbsenceBasedOrRetroactiveProof(t *testing.T) {
	f := newNativePoolBootFixture(t, false)
	f.serve(t)
	task := f.task(t, "missing-old-native-turn")
	w := f.witness(t)
	pod := &corev1.Pod{}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&f.pod), pod); err != nil {
		t.Fatal(err)
	}
	controllerutil.RemoveFinalizer(pod, runtimePoolBootPodFinalizer)
	if err := f.r.Update(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Delete(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	if err := f.r.reconcileNativeRuntimePoolRetirement(t.Context(), f.pool); err == nil {
		t.Fatal("Pod absence proved retirement")
	}
	if retired, err := verifiedNativeRuntimePoolRetirement(t.Context(), f.control, task, task.UID); err != nil || retired {
		t.Fatalf("absence retirement: %t %v", retired, err)
	}
	f.assertNoReceipt(t, task)
	// A historical unenrolled boot remains unsupported even with a complete
	// frozen Task identity. A replacement's empty status cannot supply proof.
	task.Status.Execution.RuntimeInstanceID = runtimePoolRuntimeInstanceID(w.PodUID, "unenrolled-old-boot")
	task.Status.Execution.RuntimeSessionSupervisorBootID = "unenrolled-old-boot"
	if retired, err := verifiedNativeRuntimePoolRetirement(t.Context(), f.control, task, task.UID); err != nil || retired {
		t.Fatalf("retroactive retirement: %t %v", retired, err)
	}
}

func TestRuntimePoolBootExactContainerEvidence(t *testing.T) {
	f := newNativePoolBootFixture(t, false)
	f.serve(t)
	w := f.witness(t)
	terminal := &corev1.ContainerStateTerminated{ContainerID: w.ContainerID, StartedAt: w.StartedAt, FinishedAt: metav1.NewTime(runtimePoolTestNow)}
	for _, last := range []bool{false, true} {
		pod := f.pod.DeepCopy()
		if last {
			pod.Status.ContainerStatuses[0].ContainerID = "containerd://replacement"
			pod.Status.ContainerStatuses[0].RestartCount++
			pod.Status.ContainerStatuses[0].LastTerminationState.Terminated = terminal.DeepCopy()
		} else {
			pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: terminal.DeepCopy()}
		}
		if got, err := runtimePoolWitnessPod(w, pod); err != nil || !reflect.DeepEqual(got, terminal) {
			t.Fatalf("exact lifetime last=%t: %v", last, err)
		}
	}
	for name, mutate := range map[string]func(*corev1.Pod){
		"Pod UID":        func(p *corev1.Pod) { p.UID = "replacement" },
		"namespace":      func(p *corev1.Pod) { p.Namespace = "replacement" },
		"pool UID":       func(p *corev1.Pod) { p.Labels[runtimePoolUIDLabel] = "replacement" },
		"ReplicaSet UID": func(p *corev1.Pod) { p.OwnerReferences[0].UID = "replacement" },
		"container ID":   func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.ContainerID = "other" },
		"start time": func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State.Terminated.StartedAt = metav1.NewTime(w.StartedAt.Add(time.Second))
		},
		"no finish time": func(p *corev1.Pod) { p.Status.ContainerStatuses[0].State.Terminated.FinishedAt = metav1.Time{} },
		"restart count only": func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State = corev1.ContainerState{}
			p.Status.ContainerStatuses[0].RestartCount++
		},
		"shared host PID":     func(p *corev1.Pod) { p.Spec.HostPID = true },
		"changed image":       func(p *corev1.Pod) { p.Spec.Containers[0].Image = runtimePoolTestTamperedImage },
		"ephemeral container": func(p *corev1.Pod) { p.Spec.EphemeralContainers = []corev1.EphemeralContainer{{}} },
	} {
		t.Run(name, func(t *testing.T) {
			pod := f.pod.DeepCopy()
			pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: terminal.DeepCopy()}
			mutate(pod)
			if got, _ := runtimePoolWitnessPod(w, pod); got != nil {
				t.Fatal("changed physical authority proved retirement")
			}
		})
	}
}

type failingNativePoolEffectStore struct {
	*storekube.Store
	failKind string
}

func (s *failingNativePoolEffectStore) TransitionExternalEffect(ctx context.Context, transition store.ExternalEffectTransition) (*store.ExternalEffect, error) {
	effect, err := s.GetExternalEffect(ctx, transition.ID)
	if err != nil {
		return nil, err
	}
	if effect.Identity.Kind == s.failKind {
		return nil, errors.New("injected native pool proof write failure")
	}
	return s.Store.TransitionExternalEffect(ctx, transition)
}

func TestRuntimePoolBootEnrollmentAndRetirementCrashReplay(t *testing.T) {
	for _, kind := range []string{runtimePoolBootWitnessKind, runtimePoolBootRetirementKind} {
		t.Run(kind, func(t *testing.T) {
			f := newNativePoolBootFixture(t, false)
			failure := &failingNativePoolEffectStore{Store: f.control, failKind: kind}
			if kind == runtimePoolBootWitnessKind {
				f.r.ControlStore = failure
				if _, err := f.r.Reconcile(t.Context(), runtimePoolBootTestRequest(f.pool)); err == nil {
					t.Fatal("witness write failure did not block admission")
				}
				current := runtimePoolTestGetPool(t, f.r, f.pool)
				if current.Status.AdmissionState == corev1alpha1.RuntimePoolAdmissionAccepting || current.Status.ActiveInstance != nil {
					t.Fatal("uncommitted witness admitted work")
				}
			} else {
				f.serve(t)
				f.r.ControlStore = failure
			}
			f.terminate(t, true)
			if kind == runtimePoolBootRetirementKind {
				if err := f.r.reconcileNativeRuntimePoolRetirement(t.Context(), f.pool); err == nil {
					t.Fatal("retirement write failure was ignored")
				}
			}
			pod := &corev1.Pod{}
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&f.pod), pod); err != nil || !controllerutil.ContainsFinalizer(pod, runtimePoolBootPodFinalizer) {
				t.Fatal("crash window lost retained Pod")
			}
			failure.failKind = ""
			if err := f.r.reconcileNativeRuntimePoolRetirement(t.Context(), f.pool); err != nil {
				t.Fatal(err)
			}
			w := f.witness(t)
			if retired, err := loadRuntimePoolBootRetirement(t.Context(), f.control, w); err != nil || !retired {
				t.Fatalf("replay failed to retire boot: %t %v", retired, err)
			}
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&f.pod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
				t.Fatalf("retained Pod was not released after replay: %v", err)
			}
		})
	}
}

func TestRuntimePoolBootPreparationLossBeforeRetentionCannotAdmit(t *testing.T) {
	f := newNativePoolBootFixture(t, false)
	f.r.Client = interceptor.NewClient(f.r.Client.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, delegate client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if pod, ok := object.(*corev1.Pod); ok && controllerutil.ContainsFinalizer(pod, runtimePoolBootPodFinalizer) {
				return errors.New("injected retention failure")
			}
			return delegate.Patch(ctx, object, patch, options...)
		},
	})
	if _, err := f.r.Reconcile(t.Context(), runtimePoolBootTestRequest(f.pool)); err == nil {
		t.Fatal("failed retention admitted a boot")
	}
	if err := f.r.Delete(t.Context(), &f.pod); err != nil {
		t.Fatal(err)
	}
	if err := f.r.reconcileNativeRuntimePoolRetirement(t.Context(), f.pool); err != nil {
		t.Fatal(err)
	}
	witnesses, err := f.r.runtimePoolBootRecords(t.Context(), f.pool, runtimePoolBootWitnessKind)
	if err != nil || len(witnesses) != 0 {
		t.Fatalf("lost unretained preparation became enrolled: %v", err)
	}
	preparations, err := f.r.runtimePoolBootRecords(t.Context(), f.pool, runtimePoolBootPreparationKind)
	if err != nil || len(preparations) != 1 {
		t.Fatal("missing immutable preparation")
	}
	if retired, err := loadRuntimePoolBootRetirement(t.Context(), f.control, preparations[0]); err != nil || retired {
		t.Fatalf("preparation loss became death proof: %t %v", retired, err)
	}
}

func TestRuntimePoolBootLeadershipLossRetainsPodAndReceipts(t *testing.T) {
	f := newNativePoolBootFixture(t, false)
	f.serve(t)
	task := f.task(t, "stale-leader-turn")
	f.terminate(t, true)
	current, err := f.control.GetControllerEpoch(t.Context(), store.DefaultControllerEpochName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.control.CompareAndSwapControllerEpoch(t.Context(), store.ControllerEpochCAS{
		Name: current.Name, ExpectedEpoch: current.Epoch, ExpectedVersion: current.Version, NewEpoch: current.Epoch + 1,
		HolderID: "replacement-controller", RequestDigest: testControllerDigest("native-pool-takeover"), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.r.reconcileNativeRuntimePoolRetirement(t.Context(), f.pool); err == nil {
		t.Fatal("stale controller committed retirement")
	}
	f.assertNoReceipt(t, task)
	pod := &corev1.Pod{}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&f.pod), pod); err != nil || !controllerutil.ContainsFinalizer(pod, runtimePoolBootPodFinalizer) {
		t.Fatal("stale controller released retained Pod")
	}
}

func TestRuntimePoolBootAdmissionRejectsProbeContainerRace(t *testing.T) {
	f := newNativePoolBootFixture(t, false)
	f.supervisor.afterProbe = func() {
		pod := &corev1.Pod{}
		if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&f.pod), pod); err != nil {
			t.Fatal(err)
		}
		pod.Status.ContainerStatuses[0].ContainerID = "containerd://replacement"
		pod.Status.ContainerStatuses[0].RestartCount++
		if err := f.r.Status().Update(t.Context(), pod); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.r.Reconcile(t.Context(), runtimePoolBootTestRequest(f.pool)); err == nil {
		t.Fatal("container changed across probe without closing admission")
	}
	current := runtimePoolTestGetPool(t, f.r, f.pool)
	if current.Status.AdmissionState == corev1alpha1.RuntimePoolAdmissionAccepting {
		t.Fatal("physical probe race admitted work")
	}
}

func TestRuntimePoolBootReadOnlyGateRejectsAuthorityDrift(t *testing.T) {
	f := newNativePoolBootFixture(t, false)
	f.serve(t)
	task := f.task(t, "native-proof-authority")
	f.terminate(t, true)
	if err := f.r.reconcileNativeRuntimePoolRetirement(t.Context(), f.pool); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*corev1alpha1.Task){
		"pool UID":                   func(t *corev1alpha1.Task) { t.Status.Execution.RuntimePoolUID = "other" },
		"pool name":                  func(t *corev1alpha1.Task) { t.Status.Execution.RuntimePoolName = "other" },
		"boot":                       func(t *corev1alpha1.Task) { t.Status.Execution.RuntimeSessionSupervisorBootID = "other" },
		"profile":                    func(t *corev1alpha1.Task) { t.Status.Execution.RuntimeSessionProfileDigest = "other" },
		"binding":                    func(t *corev1alpha1.Task) { t.Status.AgentExecutionBinding.BindingDigest = "corrupt" },
		"Task UID":                   func(t *corev1alpha1.Task) { t.UID = "other" },
		"missing Session generation": func(t *corev1alpha1.Task) { t.Status.Execution.RuntimeSessionGeneration = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			changed := task.DeepCopy()
			mutate(changed)
			if retired, _ := verifiedNativeRuntimePoolRetirement(t.Context(), f.control, changed, changed.UID); retired {
				t.Fatal("drifted Task obtained runtime retirement authority")
			}
		})
	}
	var effects corev1alpha1.ExternalEffectList
	if err := f.r.List(t.Context(), &effects, client.InNamespace(f.pool.Namespace)); err != nil {
		t.Fatal(err)
	}
	for i := range effects.Items {
		effect := &effects.Items[i]
		if effect.Spec.Kind != runtimePoolBootWitnessKind {
			continue
		}
		effect.Status.Response.Raw = []byte("{}")
		if err := f.r.Status().Update(t.Context(), effect); err != nil {
			t.Fatal(err)
		}
	}
	if retired, err := verifiedNativeRuntimePoolRetirement(t.Context(), f.control, task, task.UID); retired || err == nil {
		t.Fatal("corrupt witness authorized retirement")
	}
}

func TestRuntimePoolBootProviderWorkspacesRemainExcluded(t *testing.T) {
	pool := runtimePoolTestObject(1)
	pool.Spec.ExecutionWorkspace = &corev1alpha1.RuntimePoolExecutionWorkspaceSpec{}
	r := &RuntimePoolReconciler{}
	if err := r.reconcileNativeRuntimePoolRetirement(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	if w, err := r.observeNativeRuntimePoolBoot(t.Context(), pool, runtimePoolConfig{}, nil); err != nil || w != nil {
		t.Fatal("workspace provider entered native boot enrollment")
	}
}

func runtimePoolBootTestRequest(pool *corev1alpha1.RuntimePool) ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pool)}
}

func TestRuntimePoolBootRetirementRunsDuringDeletionAndCleanupOnly(t *testing.T) {
	for _, deletingPool := range []bool{false, true} {
		t.Run(fmt.Sprintf("deleting_pool_%t", deletingPool), func(t *testing.T) {
			f := newNativePoolBootFixture(t, true)
			f.serve(t)
			task := f.task(t, "delete-pool-turn")
			if deletingPool {
				if err := f.r.Delete(t.Context(), f.pool); err != nil {
					t.Fatal(err)
				}
				runtimePoolReconcile(t, f.r, f.pool)
			} else {
				f.r.CleanupOnly = true
				if err := f.r.Delete(t.Context(), &f.pod); err != nil {
					t.Fatal(err)
				}
			}
			f.assertNoReceipt(t, task)
			// Retirement must not require admission configuration or a healthy runtime.
			f.r.ProviderProxy = RuntimePoolProviderProxyConfig{}
			f.supervisor.probeErr = errors.New("old runtime is gone")
			f.terminate(t, false)
			runtimePoolReconcile(t, f.r, f.pool)
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(task), task); err != nil {
				t.Fatal(err)
			}
			if !runtimeSessionCleanupCompleteForUID(task, task.UID) {
				t.Fatal("cleanup-only lifecycle lost exact retirement proof")
			}
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&f.pod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
				t.Fatalf("cleanup lifecycle did not release terminated Pod: %v", err)
			}
			if deletingPool {
				runtimePoolReconcile(t, f.r, f.pool)
				if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pool), &corev1alpha1.RuntimePool{}); !apierrors.IsNotFound(err) {
					t.Fatalf("new retention deadlocked RuntimePool finalization: %v", err)
				}
			}
		})
	}
}

func TestRuntimePoolBootAuthenticatedDrainRequiresExactQuiescence(t *testing.T) {
	f := newNativePoolBootFixture(t, false)
	f.serve(t)
	task := f.task(t, "drain-native-turn")
	current := runtimePoolTestGetPool(t, f.r, f.pool)
	status := runtimePoolValidProbe(f.pool, &f.pod, "native-boot", true).Status
	incomplete := status
	incomplete.Pressure.LiveDescendants = 1
	if err := f.r.recordNativeRuntimePoolDrain(t.Context(), &current, current.Status.ActiveInstance, incomplete); err == nil {
		t.Fatal("live process count proved quiescence")
	}
	f.assertNoReceipt(t, task)
	pod := &corev1.Pod{}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&f.pod), pod); err != nil || !controllerutil.ContainsFinalizer(pod, runtimePoolBootPodFinalizer) {
		t.Fatal("incomplete drain released finalizer")
	}
	if err := f.r.recordNativeRuntimePoolDrain(t.Context(), &current, current.Status.ActiveInstance, status); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&f.pod), pod); err != nil || controllerutil.ContainsFinalizer(pod, runtimePoolBootPodFinalizer) {
		t.Fatal("authenticated drain did not release retention")
	}
	if retired, err := verifiedNativeRuntimePoolRetirement(t.Context(), f.control, task, task.UID); err != nil || !retired {
		t.Fatalf("authenticated native retirement: %t %v", retired, err)
	}
	// Retirement is permanent even if a later status purports to accept work.
	f.supervisor.probe.Status = runtimePoolValidProbe(f.pool, &f.pod, "native-boot", false).Status
	if _, err := f.r.Reconcile(t.Context(), runtimePoolBootTestRequest(f.pool)); err == nil {
		t.Fatal("retired boot became accepting again")
	}
}

func TestRuntimePoolBootMissingWitnessAndPostAdmissionEnrollmentStayClosed(t *testing.T) {
	for _, admissionStarted := range []bool{false, true} {
		t.Run(fmt.Sprintf("admitted_%t", admissionStarted), func(t *testing.T) {
			f := newNativePoolBootFixture(t, false)
			if admissionStarted {
				f.supervisor.probe.Status.Pressure.LiveDescendants = 1
			} else {
				f.serve(t)
				var effects corev1alpha1.ExternalEffectList
				if err := f.r.List(t.Context(), &effects, client.InNamespace(f.pool.Namespace)); err != nil {
					t.Fatal(err)
				}
				for i := range effects.Items {
					if effects.Items[i].Spec.Kind == runtimePoolBootWitnessKind || effects.Items[i].Spec.Kind == runtimePoolBootPreparationKind {
						if err := f.r.Delete(t.Context(), &effects.Items[i]); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			if _, err := f.r.Reconcile(t.Context(), runtimePoolBootTestRequest(f.pool)); err == nil {
				t.Fatal("missing/pre-admitted boot evidence did not fail closed")
			}
			current := runtimePoolTestGetPool(t, f.r, f.pool)
			if current.Status.AdmissionState == corev1alpha1.RuntimePoolAdmissionAccepting {
				t.Fatal("invalid enrollment left new admission open")
			}
		})
	}
}

func TestRuntimePoolBootReceiptAndFinalizerWriteFailuresAreReplayable(t *testing.T) {
	for _, failReceipt := range []bool{false, true} {
		t.Run(fmt.Sprintf("receipt_%t", failReceipt), func(t *testing.T) {
			f := newNativePoolBootFixture(t, false)
			f.serve(t)
			task := f.task(t, "replay-native-receipt")
			f.terminate(t, true)
			fail := true
			f.r.Client = interceptor.NewClient(f.r.Client.(client.WithWatch), interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, delegate client.Client, sub string, object client.Object, options ...client.SubResourceUpdateOption) error {
					if _, ok := object.(*corev1alpha1.Task); ok && failReceipt && fail {
						return errors.New("injected receipt write failure")
					}
					return delegate.SubResource(sub).Update(ctx, object, options...)
				},
				Patch: func(ctx context.Context, delegate client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
					if pod, ok := object.(*corev1.Pod); ok && !controllerutil.ContainsFinalizer(pod, runtimePoolBootPodFinalizer) && !failReceipt && fail {
						return errors.New("injected Pod release failure")
					}
					return delegate.Patch(ctx, object, patch, options...)
				},
			})
			if err := f.r.reconcileNativeRuntimePoolRetirement(t.Context(), f.pool); err == nil {
				t.Fatal("injected retirement-tail failure not observed")
			}
			if retired, err := verifiedNativeRuntimePoolRetirement(t.Context(), f.control, task, task.UID); err != nil || !retired {
				t.Fatalf("write failure lost committed retirement: %t %v", retired, err)
			}
			pod := &corev1.Pod{}
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&f.pod), pod); err != nil || !controllerutil.ContainsFinalizer(pod, runtimePoolBootPodFinalizer) {
				t.Fatal("write failure lost retained Pod")
			}
			fail = false
			if err := f.r.reconcileNativeRuntimePoolRetirement(t.Context(), f.pool); err != nil {
				t.Fatal(err)
			}
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(task), task); err != nil {
				t.Fatal(err)
			}
			if !runtimeSessionCleanupCompleteForUID(task, task.UID) {
				t.Fatal("replay did not persist receipt")
			}
		})
	}
}

func TestRuntimePoolBootCorruptRetirementProofCannotRelease(t *testing.T) {
	f := newNativePoolBootFixture(t, false)
	f.serve(t)
	task := f.task(t, "corrupt-retirement")
	w := f.witness(t)
	digest, err := runtimePoolBootWitnessDigest(w)
	if err != nil {
		t.Fatal(err)
	}
	fence, err := f.r.runtimePoolRetirementFence(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	proof := agentRuntimeBootRetirement{SchemaVersion: 1, WitnessDigest: digest, Kind: "kubernetes-container-termination", ContainerTermination: &corev1.ContainerStateTerminated{
		ContainerID: "wrong-container", StartedAt: w.StartedAt, FinishedAt: metav1.NewTime(runtimePoolTestNow),
	}}
	if err := persistAgentRuntimeRecoveryEffect(t.Context(), f.control, fence, w.identity(runtimePoolBootRetirementKind), digest, proof); err != nil {
		t.Fatal(err)
	}
	f.terminate(t, true)
	if err := f.r.reconcileNativeRuntimePoolRetirement(t.Context(), f.pool); err == nil {
		t.Fatal("semantically corrupt proof was accepted")
	}
	if retired, err := verifiedNativeRuntimePoolRetirement(t.Context(), f.control, task, task.UID); retired || err == nil {
		t.Fatal("corrupt retirement helper accepted proof")
	}
	f.assertNoReceipt(t, task)
}

func TestRuntimePoolBootChangedEnrollmentTopologyRejected(t *testing.T) {
	for name, mutate := range map[string]func(*corev1.Pod){
		"privileged": func(p *corev1.Pod) { p.Spec.Containers[0].SecurityContext.Privileged = new(true) },
		"namespace escape": func(p *corev1.Pod) {
			p.Spec.Containers[0].SecurityContext.Capabilities.Add = append(p.Spec.Containers[0].SecurityContext.Capabilities.Add, "SYS_ADMIN")
		},
		"host volume": func(p *corev1.Pod) {
			p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}})
		},
		"profile annotation": func(p *corev1.Pod) { p.Annotations[runtimePoolProfileAnnotation] = "sha256:" + strings.Repeat("0", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newNativePoolBootFixture(t, false)
			pod := &corev1.Pod{}
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&f.pod), pod); err != nil {
				t.Fatal(err)
			}
			mutate(pod)
			if err := f.r.Update(t.Context(), pod); err != nil {
				t.Fatal(err)
			}
			_, _ = f.r.Reconcile(t.Context(), runtimePoolBootTestRequest(f.pool))
			current := runtimePoolTestGetPool(t, f.r, f.pool)
			if current.Status.AdmissionState == corev1alpha1.RuntimePoolAdmissionAccepting {
				t.Fatal("unsafe topology admitted work")
			}
		})
	}
}

// Existing boots may have diagnostic history that is deliberately inadmissible
// for new container-death witnesses. Their positive authenticated drain remains
// usable without enrolling them or attaching any native retention finalizer.
func TestRuntimePoolBootLegacyDiagnosticPodDrainsWithoutEnrollment(t *testing.T) {
	f := newNativePoolBootFixture(t, false)
	deployment := runtimePoolTestDeployment(t, f.r, f.pool.Namespace, runtimePoolResourceName(f.pool.Namespace, f.pool.Name))
	delete(deployment.Spec.Template.Annotations, runtimePoolBootEnrollmentAnnotation)
	deployment.Spec.Template.Annotations[runtimePoolTemplateRevisionAnnotation] = runtimePoolPodTemplateRevision(deployment.Spec.Template)
	if err := f.r.Update(t.Context(), deployment); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&f.pod), pod); err != nil {
		t.Fatal(err)
	}
	delete(pod.Annotations, runtimePoolBootEnrollmentAnnotation)
	pod.Annotations[runtimePoolTemplateRevisionAnnotation] = deployment.Spec.Template.Annotations[runtimePoolTemplateRevisionAnnotation]
	pod.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "prior-diagnostics"}}}
	if err := f.r.Update(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	rs := &appsv1.ReplicaSet{}
	if err := f.r.Get(t.Context(), client.ObjectKey{Namespace: pod.Namespace, Name: metav1.GetControllerOf(pod).Name}, rs); err != nil {
		t.Fatal(err)
	}
	rs.Spec.Template = *deployment.Spec.Template.DeepCopy()
	if err := f.r.Update(t.Context(), rs); err != nil {
		t.Fatal(err)
	}
	if err := validateAgentRuntimeRecoveryPodSpec(pod.Spec, runtimePoolBootContainerName, runtimePoolProviderCodex); err == nil {
		t.Fatal("diagnostic topology was relaxed for witness enrollment")
	}
	// Reconstruct only the old controller's existing active observation, not a
	// new witness. The desired marked template must trigger rollout, not admission.
	current := runtimePoolTestGetPool(t, f.r, f.pool)
	cfg, err := f.r.runtimePoolConfig(&current)
	if err != nil {
		t.Fatal(err)
	}
	active, err := validateRuntimePoolProbe(&current, cfg, pod, f.supervisor.probe, runtimePoolTestNow)
	if err != nil {
		t.Fatal(err)
	}
	current.Status.ActiveInstance = active
	current.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleServing
	current.Status.AdmissionState = corev1alpha1.RuntimePoolAdmissionAccepting
	if err := f.r.Status().Update(t.Context(), &current); err != nil {
		t.Fatal(err)
	}
	task := f.task(t, "unenrolled-diagnostic-turn")
	runtimePoolReconcile(t, f.r, f.pool)
	current = runtimePoolTestGetPool(t, f.r, f.pool)
	if current.Status.AdmissionState == corev1alpha1.RuntimePoolAdmissionAccepting || f.supervisor.drainCalls != 1 {
		t.Fatal("legacy diagnostic boot did not enter cleanup-only rollout")
	}
	f.assertNoReceipt(t, task)
	f.supervisor.probe = runtimePoolValidProbe(f.pool, pod, "native-boot", true)
	runtimePoolReconcile(t, f.r, f.pool)
	runtimePoolReconcile(t, f.r, f.pool)
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(task), task); err != nil {
		t.Fatal(err)
	}
	if !runtimeSessionCleanupCompleteForUID(task, task.UID) {
		t.Fatal("authenticated legacy drain lost its exact Task receipt")
	}
	if retired, err := verifiedNativeRuntimePoolRetirement(t.Context(), f.control, task, task.UID); err != nil || retired {
		t.Fatalf("legacy drain fabricated retroactive native witness: %t %v", retired, err)
	}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(pod), pod); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(pod, runtimePoolBootPodFinalizer) {
		t.Fatal("unenrolled legacy Pod acquired a retention deadlock")
	}
	for _, kind := range []string{runtimePoolBootPreparationKind, runtimePoolBootWitnessKind} {
		records, err := f.r.runtimePoolBootRecords(t.Context(), f.pool, kind)
		if err != nil || len(records) != 0 {
			t.Fatal("diagnostic legacy Pod acquired native enrollment")
		}
	}
	if err := f.r.Delete(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	deployment = runtimePoolTestDeployment(t, f.r, f.pool.Namespace, deployment.Name)
	fresh := runtimePoolReadyPodForDeployment(f.pool, deployment, "fresh-safe-pod", "fresh-safe-pod-uid", "10.0.0.84")
	runtimePoolTestCreatePod(t, f.r, &fresh)
	f.supervisor.probe = runtimePoolValidProbe(f.pool, &fresh, "fresh-safe-boot", false)
	runtimePoolReconcile(t, f.r, f.pool)
	current = runtimePoolTestGetPool(t, f.r, f.pool)
	if current.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionAccepting || current.Status.ActiveInstance == nil || current.Status.ActiveInstance.PodUID != string(fresh.UID) {
		t.Fatal("fresh replacement did not require and complete safe enrollment")
	}
	if _, err := loadRuntimePoolBootWitness(t.Context(), f.control, agentRuntimeRecoveryIdentity(runtimePoolBootWitnessKind, f.pool.UID, f.pool.Namespace, current.Status.ActiveInstance.RuntimeInstanceID)); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimePoolBootUnmarkedPodCannotEnterNativeAdmission(t *testing.T) {
	f := newNativePoolBootFixture(t, false)
	pod := &corev1.Pod{}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&f.pod), pod); err != nil {
		t.Fatal(err)
	}
	delete(pod.Annotations, runtimePoolBootEnrollmentAnnotation)
	if err := f.r.Update(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.Reconcile(t.Context(), runtimePoolBootTestRequest(f.pool)); err == nil {
		t.Fatal("unenrolled Pod admitted against a fresh native template")
	}
	current := runtimePoolTestGetPool(t, f.r, f.pool)
	if current.Status.AdmissionState == corev1alpha1.RuntimePoolAdmissionAccepting {
		t.Fatal("missing native witness left new admission open")
	}
}

func TestRuntimePoolBootEnrollmentAcceptsAPIDefaultedTemplate(t *testing.T) {
	f := newNativePoolBootFixture(t, false)
	deployment := runtimePoolTestDeployment(t, f.r, f.pool.Namespace, runtimePoolResourceName(f.pool.Namespace, f.pool.Name))
	revision := deployment.Spec.Template.Annotations[runtimePoolTemplateRevisionAnnotation]
	deployment.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyAlways
	deployment.Spec.Template.Spec.DNSPolicy = corev1.DNSClusterFirst
	if err := f.r.Update(t.Context(), deployment); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&f.pod), pod); err != nil {
		t.Fatal(err)
	}
	pod.Spec.RestartPolicy, pod.Spec.DNSPolicy = corev1.RestartPolicyAlways, corev1.DNSClusterFirst
	if err := f.r.Update(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	rs := &appsv1.ReplicaSet{}
	if err := f.r.Get(t.Context(), client.ObjectKey{Namespace: pod.Namespace, Name: metav1.GetControllerOf(pod).Name}, rs); err != nil {
		t.Fatal(err)
	}
	rs.Spec.Template = *deployment.Spec.Template.DeepCopy()
	if err := f.r.Update(t.Context(), rs); err != nil {
		t.Fatal(err)
	}
	if runtimePoolPodTemplateRevision(deployment.Spec.Template) == revision {
		t.Fatal("fixture did not model API-server defaulting after template revision hashing")
	}
	f.serve(t)
	_ = f.witness(t)
}

func TestRuntimePoolBootSupersededUnretainedPreparationAllowsLifecycle(t *testing.T) {
	for _, scaleDown := range []bool{false, true} {
		t.Run(fmt.Sprintf("scale_down_%t", scaleDown), func(t *testing.T) {
			f, preparation := runtimePoolTestUnretainedPreparation(t)
			current := runtimePoolTestGetPool(t, f.r, f.pool)
			current.Generation++
			if scaleDown {
				current.Spec.DesiredReplicas = 0
			} else {
				current.Spec.Capacity = &corev1alpha1.RuntimePoolCapacitySpec{MaxResidentSessions: 2, MaxRunningPrompts: 1}
			}
			if err := f.r.Update(t.Context(), &current); err != nil {
				t.Fatal(err)
			}
			// The obsolete unadmitted Pod must not monopolize the pre-rollout replay.
			if _, err := f.r.Reconcile(t.Context(), runtimePoolBootTestRequest(f.pool)); err != nil {
				t.Fatalf("superseded unretained preparation blocked lifecycle: %v", err)
			}
			for range 2 {
				runtimePoolReconcile(t, f.r, f.pool)
			}
			if _, err := loadRuntimePoolBootWitness(t.Context(), f.control, preparation.identity(runtimePoolBootWitnessKind)); !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrNotReady) {
				t.Fatalf("obsolete preparation became a committed witness: %v", err)
			}
			if retired, err := loadRuntimePoolBootRetirement(t.Context(), f.control, preparation); err != nil || retired {
				t.Fatalf("discarding an unadmitted preparation became death proof: %t %v", retired, err)
			}
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&f.pod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
				t.Fatalf("obsolete unretained Pod was not withdrawn: %v", err)
			}
			current = runtimePoolTestGetPool(t, f.r, f.pool)
			if scaleDown {
				if current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopped || current.Status.ActiveInstance != nil {
					t.Fatal("superseded preparation blocked scale-to-zero")
				}
				return
			}
			deployment := runtimePoolTestDeployment(t, f.r, f.pod.Namespace, runtimePoolResourceName(f.pool.Namespace, f.pool.Name))
			replacement := runtimePoolReadyPodForDeployment(&current, deployment, "replacement-after-preparation", "replacement-after-preparation-uid", "10.0.0.85")
			runtimePoolTestCreatePod(t, f.r, &replacement)
			f.supervisor.probe = runtimePoolValidProbe(&current, &replacement, "replacement-after-preparation", false)
			f.serve(t)
		})
	}
}

func runtimePoolTestUnretainedPreparation(t *testing.T) (*nativePoolBootFixture, runtimePoolBootWitness) {
	t.Helper()
	f := newNativePoolBootFixture(t, false)
	base := f.r.Client
	f.r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, delegate client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if pod, ok := object.(*corev1.Pod); ok && controllerutil.ContainsFinalizer(pod, runtimePoolBootPodFinalizer) {
				return errors.New("injected failure before Pod retention")
			}
			return delegate.Patch(ctx, object, patch, options...)
		},
	})
	if _, err := f.r.Reconcile(t.Context(), runtimePoolBootTestRequest(f.pool)); err == nil {
		t.Fatal("fixture did not stop before retention")
	}
	f.r.Client = base
	preparations, err := f.r.runtimePoolBootRecords(t.Context(), f.pool, runtimePoolBootPreparationKind)
	if err != nil || len(preparations) != 1 {
		t.Fatalf("fixture lacks exact preparation: %v", err)
	}
	return f, preparations[0]
}

func TestRuntimePoolBootUnretainedPreparationWithdrawsAfterMultipleRestarts(t *testing.T) {
	f, preparation := runtimePoolTestUnretainedPreparation(t)
	current := runtimePoolTestGetPool(t, f.r, f.pool)
	generation := current.Generation
	pod := &corev1.Pod{}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&f.pod), pod); err != nil {
		t.Fatal(err)
	}
	status := &pod.Status.ContainerStatuses[0]
	status.ContainerID = "containerd://unadmitted-restart-2"
	status.RestartCount = preparation.RestartCount + 2
	status.State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{
		StartedAt: metav1.NewTime(preparation.StartedAt.Add(40 * time.Second)),
	}}
	// Only the intervening container remains in last-termination history. This
	// is deliberately NOT termination evidence for the original preparation.
	status.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{
		ContainerID: "containerd://unadmitted-restart-1",
		StartedAt:   metav1.NewTime(preparation.StartedAt.Add(20 * time.Second)),
		FinishedAt:  metav1.NewTime(preparation.StartedAt.Add(30 * time.Second)),
	}
	if err := f.r.Status().Update(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	if terminal, err := runtimePoolWitnessPod(preparation, pod); err != nil || terminal != nil {
		t.Fatalf("fixture unexpectedly proves original termination: %v", err)
	}
	if _, err := f.r.Reconcile(t.Context(), runtimePoolBootTestRequest(f.pool)); err != nil {
		t.Fatalf("unretained physical boot change blocked lifecycle at unchanged generation: %v", err)
	}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&f.pod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("exact unadmitted source was not withdrawn: %v", err)
	}
	current = runtimePoolTestGetPool(t, f.r, f.pool)
	if current.Generation != generation || current.Status.ActiveInstance != nil {
		t.Fatal("withdrawal changed pool generation or admitted the obsolete source")
	}
	if _, err := loadRuntimePoolBootWitness(t.Context(), f.control, preparation.identity(runtimePoolBootWitnessKind)); !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("withdrawal committed the obsolete witness: %v", err)
	}
	if retired, err := loadRuntimePoolBootRetirement(t.Context(), f.control, preparation); err != nil || retired {
		t.Fatalf("physical boot change became a false retirement proof: %t %v", retired, err)
	}
	deployment := runtimePoolTestDeployment(t, f.r, f.pod.Namespace, runtimePoolResourceName(f.pool.Namespace, f.pool.Name))
	replacement := runtimePoolReadyPodForDeployment(&current, deployment, "replacement-after-restarts", "replacement-after-restarts-uid", "10.0.0.86")
	runtimePoolTestCreatePod(t, f.r, &replacement)
	f.supervisor.probe = runtimePoolValidProbe(&current, &replacement, "replacement-after-restarts", false)
	f.serve(t)
}
