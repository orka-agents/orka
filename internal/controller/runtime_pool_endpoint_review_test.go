package controller

import (
	"context"
	"strings"
	"testing"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type endpointRecordingSupervisor struct {
	*fakeRuntimePoolSupervisorClient
	endpoints []string
}

func TestExternalRuntimePoolNativeEndpointRequiresExactCoreBinding(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	f.advertiseNativeProcess(t)
	w, worker := f.materialize(t)
	a := w.Status.Allocation
	a.Identity.InstanceID = "native-process-uid"
	a.Startup.Identity = a.Identity
	a.Startup.Pod = nil
	a.Startup.Endpoint = "http://native-router.example:80/process/route"
	a.Startup.Process = &workspacev1alpha1.NativeProcessEvidence{Namespace: "native", Name: "process", UID: a.Identity.InstanceID, Version: 1,
		Worker: workspacev1alpha1.PodReference{Namespace: worker.Namespace, Name: worker.Name, UID: worker.UID}, ChallengeSHA256: "sha256:" + strings.Repeat("a", 64)}
	if err := f.r.Status().Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	pod, err := f.r.attestExternalWorkspaceStartup(t.Context(), w.Spec.Workload, a.Startup)
	if err != nil {
		t.Fatal(err)
	}
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if runtimePoolInstanceEndpoint(&pool, pod) != "" {
		t.Fatal("unbound native process supplied an authenticated endpoint")
	}
	if err := f.r.bindExternalRuntimeInstanceEvidence(t.Context(), &pool, w); err != nil {
		t.Fatal(err)
	}
	pod.Annotations[externalRuntimeEndpointAnnotation] = "https://foreign.example/runtime"
	if got := runtimePoolInstanceEndpoint(&pool, pod); got != a.Startup.Endpoint {
		t.Fatalf("native endpoint lost the exact Core binding: %q", got)
	}
	pool.Status.ActiveInstance = &corev1alpha1.RuntimePoolActiveInstanceStatus{PodUID: string(pod.UID), PodName: pod.Name, PodNamespace: pod.Namespace, PodAddress: pod.Status.PodIP,
		BootID: "native-boot", RuntimeInstanceID: runtimePoolRuntimeInstanceID(pod.UID, harnessv2.SupervisorBootID("native-boot")), ProfileDigest: pool.Spec.Runtime.Profile.Digest}
	if got, err := runtimePoolWorkspaceStartupEndpoint(t.Context(), f.r.Client, &pool); err != nil || got != a.Startup.Endpoint {
		t.Fatalf("exact native dispatch route rejected: %q, %v", got, err)
	}
	pod.UID = "replacement-process"
	if runtimePoolInstanceEndpoint(&pool, pod) != "" {
		t.Fatal("replacement process inherited the authenticated native endpoint")
	}
	pool.Spec.ExecutionWorkspace.WorkspaceRef.UID = "replacement-workspace"
	if _, err := runtimePoolWorkspaceStartupEndpoint(t.Context(), f.r.Client, &pool); err == nil {
		t.Fatal("replacement workspace inherited native dispatch authority")
	}
}

func (s *endpointRecordingSupervisor) Probe(ctx context.Context, endpoint, token string, secret []byte) (RuntimePoolProbeResult, error) {
	s.endpoints = append(s.endpoints, endpoint)
	return s.fakeRuntimePoolSupervisorClient.Probe(ctx, endpoint, token, secret)
}

func TestRuntimePoolPodAnnotationCannotRedirectAuthenticatedProbe(t *testing.T) {
	for _, external := range []bool{false, true} {
		name := "Deployment"
		if external {
			name = "external Pod"
		}
		t.Run(name, func(t *testing.T) {
			var r *RuntimePoolReconciler
			var pool *corev1alpha1.RuntimePool
			supervisor := &endpointRecordingSupervisor{fakeRuntimePoolSupervisorClient: &fakeRuntimePoolSupervisorClient{}}
			if external {
				f := newExternalRuntimePoolFixture(t)
				_, pod := f.materialize(t)
				pod.Annotations = cloneStringMap(pod.Annotations)
				if pod.Annotations == nil {
					pod.Annotations = map[string]string{}
				}
				pod.Annotations[externalRuntimeEndpointAnnotation] = "https://foreign.example/runtime"
				if err := f.r.Update(t.Context(), &pod); err != nil {
					t.Fatal(err)
				}
				r, pool = f.r, f.pool
				supervisor.fakeRuntimePoolSupervisorClient = f.supervisor
			} else {
				pool = runtimePoolTestObject(1)
				r = runtimePoolTestReconciler(t, runtimePoolTestScheme(t), supervisor, pool)
				runtimePoolReconcile(t, r, pool)
				deployment := &appsv1.Deployment{}
				if err := r.Get(t.Context(), client.ObjectKey{Namespace: pool.Namespace, Name: runtimePoolResourceName(pool.Namespace, pool.Name)}, deployment); err != nil {
					t.Fatal(err)
				}
				pod := runtimePoolReadyPodForDeployment(pool, deployment, "runtime", "runtime-uid", "10.0.0.71")
				if pod.Annotations == nil {
					pod.Annotations = map[string]string{}
				}
				pod.Annotations[externalRuntimeEndpointAnnotation] = "https://foreign.example/runtime"
				runtimePoolTestCreatePod(t, r, &pod)
				supervisor.probe = runtimePoolValidProbe(pool, &pod, "boot", false)
			}
			r.SupervisorClient = supervisor
			runtimePoolReconcile(t, r, pool)
			if len(supervisor.endpoints) != 1 || supervisor.endpoints[0] != "http://10.0.0.71:8080" {
				t.Fatalf("authenticated probe used %v instead of the attested Pod address", supervisor.endpoints)
			}
			if current := runtimePoolTestGetPool(t, r, pool); current.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing {
				t.Fatalf("exact Pod did not become Serving: %s", current.Status.Message)
			}
		})
	}
}
