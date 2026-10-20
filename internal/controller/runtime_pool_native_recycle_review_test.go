// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type nativeRecycleClosedBootstrap struct{ t *testing.T }

func (transport nativeRecycleClosedBootstrap) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.t.Helper()
	if request.Method != http.MethodGet || !strings.HasSuffix(request.URL.Path, harnessv2.CredentialBootstrapPath) {
		transport.t.Fatal("recovered supervisor received a new credential bootstrap")
	}
	// Model an already bootstrapped supervisor. Production still independently
	// attests the worker, binds its private credentials and verifies its v2 probe.
	return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: request}, nil
}

func nativeRecycleReviewFixture(t *testing.T) (*externalRuntimePoolFixture, *workspacev1alpha1.ExecutionWorkspace, corev1.Pod, corev1.Pod) {
	t.Helper()
	f, _, _, worker := nativeIngressReviewFixture(t)
	f.r.HTTPClient = &http.Client{Transport: nativeRecycleClosedBootstrap{t: t}}
	w := f.currentWorkspace(t)
	pod, err := f.r.attestExternalWorkspaceStartup(t.Context(), w.Spec.Workload, w.Status.Allocation.Startup)
	if err != nil {
		t.Fatal(err)
	}
	f.supervisor.probe = runtimePoolValidProbe(f.pool, pod, "native-recycle-boot", false)
	runtimePoolReconcile(t, f.r, f.pool)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing || pool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionAccepting ||
		pool.Status.ActiveInstance == nil || pool.Status.ActiveInstance.PodUID != string(pod.UID) || string(pod.UID) == w.Status.Allocation.Identity.InstanceID {
		t.Fatalf("native fixture did not serve the opaque runtime: %#v", pool.Status)
	}
	f.pool = pool.DeepCopy()
	return f, w, *pod, worker
}

func nativeRecycleReviewAuthSecret(t *testing.T, f *externalRuntimePoolFixture) corev1.Secret {
	t.Helper()
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	cfg, err := f.r.runtimePoolConfig(&pool)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := resolveRuntimePoolAuthSecret(t.Context(), f.r.APIReader, &pool, cfg.namespace, cfg.controllerEpoch)
	if err != nil {
		t.Fatal(err)
	}
	return *auth
}

func TestExternalNativeRuntimePublicRecyclingTriggers(t *testing.T) {
	for _, cause := range []string{"unhealthy", "in-place restart", "identity exhaustion"} {
		t.Run(cause, func(t *testing.T) {
			f, w, pod, worker := nativeRecycleReviewFixture(t)
			oldRequest := w.Spec.Workload.DeepCopy()
			oldAuth := nativeRecycleReviewAuthSecret(t, f)
			oldPool := runtimePoolTestGetPool(t, f.r, f.pool)
			switch cause {
			case "unhealthy":
				f.supervisor.probe.Status.Lifecycle = harnessv2.SupervisorLifecycleUnhealthy
			case "in-place restart":
				f.supervisor.probe = runtimePoolValidProbe(f.pool, &pod, "restarted-native-boot", false)
			case "identity exhaustion":
				f.supervisor.probe.Status.SessionIdentityCapacity.Remaining = f.supervisor.probe.Status.SessionIdentityCapacity.ExhaustionReserve
			}
			for range 6 {
				runtimePoolReconcile(t, f.r, f.pool)
				pool := runtimePoolTestGetPool(t, f.r, f.pool)
				if pool.Annotations["orka.ai/external-runtime-retirement-requested"] == booleanTrueValue {
					if pool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed {
						t.Fatal("recycling left Task admission open")
					}
					if !reflect.DeepEqual(f.currentWorkspace(t).Spec.Workload, oldRequest) || f.currentWorkspace(t).Spec.Retirement != nil {
						t.Fatal("recycling rewrote the workload or skipped provider retirement settlement")
					}
					auth := nativeRecycleReviewAuthSecret(t, f)
					if auth.UID != oldAuth.UID || !reflect.DeepEqual(auth.Data, oldAuth.Data) || pool.Annotations[runtimePoolBootstrapInstanceBindingAnnotation] != oldPool.Annotations[runtimePoolBootstrapInstanceBindingAnnotation] {
						t.Fatal("recycling rotated credentials before exact provider termination")
					}
					if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&worker), &corev1.Pod{}); err != nil {
						t.Fatalf("recycling removed the native worker: %v", err)
					}
					if cause == "identity exhaustion" && f.supervisor.drainCalls != 1 {
						t.Fatal("identity exhaustion recycled without the authenticated drain barrier")
					}
					return
				}
				if cause == "identity exhaustion" && f.supervisor.drainCalls == 1 {
					capacity := f.supervisor.probe.Status.SessionIdentityCapacity
					f.supervisor.probe = runtimePoolValidProbe(f.pool, &pod, "native-recycle-boot", true)
					f.supervisor.probe.Status.SessionIdentityCapacity = capacity
				}
			}
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			t.Fatalf("native %s never requested exact recycling: %s", cause, pool.Status.Message)
		})
	}
}

func TestExternalNativeRuntimeRecyclingRetainsLegacyCleanupIdentity(t *testing.T) {
	f, w, _ := nativeIdentityReviewFixture(t)
	env := nativeIdentityReviewEnv(t, w.Spec.Workload)
	*env = corev1.EnvVar{Name: env.Name, ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}}
	var err error
	w.Spec.Workload.Revision, err = workspacev1alpha1.WorkloadRevision(*w.Spec.Workload)
	if err != nil {
		t.Fatal(err)
	}
	w.Status.Allocation.Identity.RequestRevision = w.Spec.Workload.Revision
	w.Status.Allocation.Startup.Identity = w.Status.Allocation.Identity
	status := w.Status.DeepCopy()
	if err := f.r.Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	w.Status = *status
	if err := f.r.Status().Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	binding := externalRuntimeInstanceEvidence{WorkspaceUID: w.UID, Sequence: w.Spec.Workload.Sequence, Identity: w.Status.Allocation.Identity,
		Pod: w.Status.Allocation.Startup.Process.Worker, NativeProcess: true, Endpoint: w.Status.Allocation.Startup.Endpoint}
	encoded, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	pool.Annotations[externalRuntimeEvidenceAnnotation] = string(encoded)
	if err := f.r.Update(t.Context(), &pool); err != nil {
		t.Fatal(err)
	}
	frozen := w.Spec.Workload.DeepCopy()
	pod, err := f.r.attestExternalWorkspaceStartup(t.Context(), w.Spec.Workload, w.Status.Allocation.Startup)
	if err != nil || string(pod.UID) != w.Status.Allocation.Identity.InstanceID {
		t.Fatalf("legacy cleanup lost the exact admitted identity: %v", err)
	}
	if err := f.r.recycleRuntimePoolInstance(t.Context(), &pool, pod); err != nil {
		t.Fatalf("legacy exact identity could not request cleanup: %v", err)
	}
	if pool.Annotations["orka.ai/external-runtime-retirement-requested"] != booleanTrueValue || !reflect.DeepEqual(f.currentWorkspace(t).Spec.Workload, frozen) {
		t.Fatal("legacy recycling changed its immutable workload")
	}
}

func TestExternalNativeRuntimeRecyclingKeepsRetirementAndRotationFences(t *testing.T) {
	f, w, pod, worker := nativeRecycleReviewFixture(t)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	oldRequest := w.Spec.Workload.DeepCopy()
	oldAuth := nativeRecycleReviewAuthSecret(t, f)
	if err := f.r.recycleRuntimePoolInstance(t.Context(), &pool, &pod); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	runtimePoolReconcile(t, f.r, f.pool)
	if f.supervisor.drainCalls != 1 || f.currentWorkspace(t).Spec.Retirement != nil {
		t.Fatal("native recycling skipped exact authenticated drain")
	}
	f.supervisor.probe = runtimePoolValidProbe(f.pool, &pod, "foreign-boot", true)
	runtimePoolReconcile(t, f.r, f.pool)
	if f.currentWorkspace(t).Spec.Retirement != nil {
		t.Fatal("foreign boot authorized native recycling")
	}
	f.supervisor.probe = runtimePoolValidProbe(f.pool, &pod, "native-recycle-boot", true)
	runtimePoolReconcile(t, f.r, f.pool)
	w = f.currentWorkspace(t)
	if w.Spec.Retirement == nil || w.Spec.Retirement.Sequence != oldRequest.Sequence || w.Spec.Retirement.Identity != w.Status.Allocation.Identity || w.Spec.Retirement.Action != workspacev1alpha1.WorkloadRetirementStop {
		t.Fatal("drained native recycling lost its exact provider allocation")
	}
	if nativeRecycleReviewAuthSecret(t, f).UID != oldAuth.UID {
		t.Fatal("native recycling rotated credentials before terminated observation")
	}
	w.Status.Allocation.Startup = nil
	w.Status.Allocation.State = workspacev1alpha1.AllocationStopped
	w.Status.ObservedGeneration = w.Generation - 1
	if err := f.r.Status().Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	if f.currentWorkspace(t).Spec.Workload.Sequence != oldRequest.Sequence || nativeRecycleReviewAuthSecret(t, f).UID != oldAuth.UID {
		t.Fatal("stale termination observation rotated native credentials or request")
	}
	w = f.currentWorkspace(t)
	w.Status.ObservedGeneration = w.Generation
	if err := f.r.Status().Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	for range 12 {
		runtimePoolReconcile(t, f.r, f.pool)
		w = f.currentWorkspace(t)
		if w.Spec.Workload.Sequence == oldRequest.Sequence+1 {
			if w.Spec.Workload.PreviousInstance == nil || *w.Spec.Workload.PreviousInstance != w.Status.Allocation.Identity || nativeIdentityReviewEnv(t, w.Spec.Workload).Value == string(pod.UID) {
				t.Fatal("native replacement lost its predecessor or opaque identity rotation")
			}
			if nativeRecycleReviewAuthSecret(t, f).UID == oldAuth.UID {
				t.Fatal("native replacement reused consumed private credentials")
			}
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&worker), &corev1.Pod{}); err != nil {
				t.Fatalf("native replacement deleted its infrastructure worker: %v", err)
			}
			return
		}
	}
	t.Fatal("exact native stopped proof did not rotate the public request")
}

func TestExternalNativeRuntimeRecyclingRejectsDrift(t *testing.T) {
	for _, change := range []string{"raw allocation", "saved raw allocation", "saved opaque UID", "synthetic UID", "sequence", "request revision", "worker evidence", "worker replacement", "endpoint", "stale observation", "duplicate opaque UID", "missing saved evidence"} {
		t.Run(change, func(t *testing.T) {
			f, w, pod, worker := nativeRecycleReviewFixture(t)
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			binding, err := externalRuntimeEvidence(&pool)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "raw allocation":
				w.Status.Allocation.Identity.InstanceID = "foreign-instance"
				w.Status.Allocation.Startup.Identity = w.Status.Allocation.Identity
				w.Status.Allocation.Startup.Process.UID = w.Status.Allocation.Identity.InstanceID
			case "saved raw allocation":
				binding.Identity.AllocationID = "foreign-allocation"
			case "saved opaque UID":
				binding.RuntimeUID = "foreign-runtime"
			case "synthetic UID":
				pod.UID = binding.Pod.UID
			case "sequence":
				w.Status.Allocation.Sequence++
			case "request revision":
				w.Spec.Workload.Image = "changed:latest"
				w.Spec.Workload.Revision, err = workspacev1alpha1.WorkloadRevision(*w.Spec.Workload)
				if err != nil {
					t.Fatal(err)
				}
				w.Status.Allocation.Identity.RequestRevision = w.Spec.Workload.Revision
				w.Status.Allocation.Startup.Identity = w.Status.Allocation.Identity
			case "worker evidence":
				w.Status.Allocation.Startup.Process.Worker.UID = "foreign-worker"
			case "worker replacement":
				if err := f.r.Delete(t.Context(), &worker); err != nil {
					t.Fatal(err)
				}
				worker.UID, worker.ResourceVersion = "replacement-worker", ""
				if err := f.r.Create(t.Context(), &worker); err != nil {
					t.Fatal(err)
				}
			case "endpoint":
				w.Status.Allocation.Startup.Endpoint = "http://foreign.example:80/process"
			case "stale observation":
				w.Status.ObservedGeneration--
			case "duplicate opaque UID":
				w.Spec.Workload.Runtime.Template.Spec.Containers[0].Env = append(w.Spec.Workload.Runtime.Template.Spec.Containers[0].Env, *nativeIdentityReviewEnv(t, w.Spec.Workload))
			case "missing saved evidence":
				delete(pool.Annotations, externalRuntimeEvidenceAnnotation)
			}
			if change != "missing saved evidence" {
				encoded, err := json.Marshal(binding)
				if err != nil {
					t.Fatal(err)
				}
				pool.Annotations[externalRuntimeEvidenceAnnotation] = string(encoded)
			}
			if err := f.r.Update(t.Context(), &pool); err != nil {
				t.Fatal(err)
			}
			status := w.Status.DeepCopy()
			if err := f.r.Update(t.Context(), w); err != nil {
				t.Fatal(err)
			}
			w.Status = *status
			if err := f.r.Status().Update(t.Context(), w); err != nil {
				t.Fatal(err)
			}
			if err := f.r.recycleRuntimePoolInstance(t.Context(), &pool, &pod); err == nil {
				t.Fatal("drifted native runtime authorized recycling")
			}
			pool = runtimePoolTestGetPool(t, f.r, f.pool)
			if pool.Annotations["orka.ai/external-runtime-retirement-requested"] != "" || f.currentWorkspace(t).Spec.Retirement != nil {
				t.Fatal("rejected native runtime left retirement authority")
			}
		})
	}
}
