package controller

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/harness/v2/conformance/conformancetest"
	"github.com/orka-agents/orka/internal/store"
	storekube "github.com/orka-agents/orka/internal/store/kube"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type runtimeRecoveryFixture struct {
	r       *AgentRuntimeReconciler
	runtime *corev1alpha1.AgentRuntime
	config  conformancetest.Config
	server  *conformancetest.Server
	control *storekube.Store
	fence   store.ControllerEpochFence
	pod     *corev1.Pod
	rs      *appsv1.ReplicaSet
	slice   *discoveryv1.EndpointSlice
}

func newRuntimeRecoveryFixture(t *testing.T) *runtimeRecoveryFixture {
	t.Helper()
	profile, claims, limits := testAgentRuntimeProfileClaimsAndLimits()
	config := conformancetest.Config{
		ControllerBearerToken: strings.Repeat("t", 32), OperationCapabilitySecret: bytes.Repeat([]byte("s"), 32),
		RuntimeInstanceID: "recovery-instance", SupervisorBootID: "recovery-boot-1", RuntimePoolUID: "recovery-pool",
		ControllerEpoch: 1, Profile: profile, Limits: limits, WorkspaceGovernance: claims, SupportsDrain: true,
	}
	server, err := conformancetest.NewServer(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	runtime, secret := testAgentRuntimeAndSecret(t, "http://runtime.default.svc.cluster.local:8080", config)
	runtime.Spec.Deployment.KubernetesRecovery = &corev1alpha1.AgentRuntimeKubernetesRecoverySpec{
		DeploymentName: "runtime", DeploymentUID: "deployment-uid", ContainerName: "supervisor",
	}
	deployment, rs, pod, service, slice := runtimeRecoveryObjects(t, runtime, server.URL())
	scheme := newTestScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := discoveryv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := withControllerEpochLeaseUIDs(t, fake.NewClientBuilder().WithScheme(scheme).WithObjects(runtime, secret, deployment, rs, pod, service, slice).
		WithStatusSubresource(&corev1alpha1.AgentRuntime{}, &corev1alpha1.ControllerEpoch{}, &corev1alpha1.ExternalEffect{}, &corev1alpha1.Task{}, &corev1alpha1.PromptAttempt{}, &corev1.Pod{}).Build())
	control, err := storekube.New(c, defaultNS, storekube.WithAPIReader(c))
	if err != nil {
		t.Fatal(err)
	}
	epochs := readyAgentRuntimeTestEpochManager(1)
	epoch, err := control.CompareAndSwapControllerEpoch(t.Context(), store.ControllerEpochCAS{
		NewEpoch: 1, HolderID: epochs.HolderID, RequestDigest: testControllerDigest("recovery-epoch-1"), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	f := &runtimeRecoveryFixture{runtime: runtime, config: config, server: server, control: control, pod: pod, rs: rs, slice: slice,
		fence: store.ControllerEpochFence{Name: epoch.Name, HolderID: epoch.HolderID, Epoch: epoch.Epoch}}
	f.r = &AgentRuntimeReconciler{Client: c, APIReader: c, Scheme: scheme, ControlStore: control, ControllerEpochManager: epochs}
	return f
}

func runtimeRecoveryObjects(t *testing.T, runtime *corev1alpha1.AgentRuntime, backendURL string) (*appsv1.Deployment, *appsv1.ReplicaSet, *corev1.Pod, *corev1.Service, *discoveryv1.EndpointSlice) {
	t.Helper()
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: defaultNS, Name: "runtime", UID: "deployment-uid", Annotations: map[string]string{agentRuntimeRecoveryOwnerAnnotation: string(runtime.UID)}},
		Spec: appsv1.DeploymentSpec{
			Replicas: new(int32(1)), Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "recovery"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "recovery"}},
				Spec: corev1.PodSpec{AutomountServiceAccountToken: new(false), Containers: []corev1.Container{{
					Name: "supervisor", Image: "docker.io/example/supervisor@" + testControllerDigest("image"),
					Env: []corev1.EnvVar{{Name: agentRuntimeEpochEnvironment, Value: "1"}},
				}}},
			},
		},
	}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: defaultNS, Name: "runtime-rs", UID: "replicaset-uid",
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(deployment, appsv1.SchemeGroupVersion.WithKind("Deployment"))}},
		Spec: appsv1.ReplicaSetSpec{Template: *deployment.Spec.Template.DeepCopy()},
	}
	rs.Spec.Template.Labels[appsv1.DefaultDeploymentUniqueLabelKey] = "hash-1"
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: defaultNS, Name: "runtime-pod", UID: "pod-uid",
		Labels: map[string]string{"app": "recovery"}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(rs, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))}},
		Spec: *deployment.Spec.Template.Spec.DeepCopy(), Status: corev1.PodStatus{
			PodIP: "127.0.0.1", PodIPs: []corev1.PodIP{{IP: "127.0.0.1"}}, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "supervisor", ContainerID: "containerd://container-1", ImageID: testControllerDigest("image"), Ready: true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now().UTC().Add(-time.Minute).Truncate(time.Second))}}}},
		},
	}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: defaultNS, Name: "runtime", UID: "service-uid"}, Spec: corev1.ServiceSpec{
		Selector: map[string]string{"app": "recovery"}, Ports: []corev1.ServicePort{{Port: 8080, TargetPort: intstr.FromInt32(runtimeRecoveryServerPort(t, backendURL))}},
	}}
	slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: defaultNS, Name: "runtime-endpoints", UID: "slice-uid",
		Labels: map[string]string{discoveryv1.LabelServiceName: "runtime"}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(service, corev1.SchemeGroupVersion.WithKind("Service"))}},
		AddressType: discoveryv1.AddressTypeIPv4, Ports: []discoveryv1.EndpointPort{{Port: new(runtimeRecoveryServerPort(t, backendURL))}},
		Endpoints: []discoveryv1.Endpoint{{Addresses: []string{"127.0.0.1"}, TargetRef: &corev1.ObjectReference{Kind: "Pod", Namespace: defaultNS, Name: pod.Name, UID: pod.UID}}},
	}
	return deployment, rs, pod, service, slice
}

func runtimeRecoveryServerPort(t *testing.T, endpoint string) int32 {
	t.Helper()
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatal(err)
	}
	value, err := strconv.ParseInt(port, 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	return int32(value)
}

func (f *runtimeRecoveryFixture) reconcile(t *testing.T) {
	t.Helper()
	if _, err := f.r.Reconcile(t.Context(), reconcileRequestFor(f.runtime)); err != nil {
		t.Fatal(err)
	}
	f.runtime = new(corev1alpha1.AgentRuntime)
	if err := f.r.Get(t.Context(), client.ObjectKey{Namespace: defaultNS, Name: "runtime"}, f.runtime); err != nil {
		t.Fatal(err)
	}
}

func (f *runtimeRecoveryFixture) witness(t *testing.T) agentRuntimeBootWitness {
	t.Helper()
	w, err := loadAgentRuntimeBootWitness(t.Context(), f.control, defaultNS, f.runtime.UID, f.server.Fence().SupervisorBootID)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func (f *runtimeRecoveryFixture) advanceEpoch(t *testing.T) {
	t.Helper()
	current, err := f.control.GetControllerEpoch(t.Context(), f.fence.Name)
	if err != nil {
		t.Fatal(err)
	}
	next, err := f.control.CompareAndSwapControllerEpoch(t.Context(), store.ControllerEpochCAS{
		ExpectedEpoch: current.Epoch, ExpectedVersion: current.Version, NewEpoch: current.Epoch + 1,
		HolderID: f.fence.HolderID, RequestDigest: testControllerDigest("recovery-successor-" + strconv.FormatInt(current.Epoch, 10)), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.fence.Epoch = next.Epoch
	setAgentRuntimeTestControllerEpoch(f.r.ControllerEpochManager, next.Epoch)
}

func (f *runtimeRecoveryFixture) restartContainer(t *testing.T, retainTermination, sameContainer bool) {
	t.Helper()
	witness := f.witness(t)
	f.server.Close()
	f.config.SupervisorBootID = "recovery-boot-2"
	f.config.ControllerEpoch = uint64(f.fence.Epoch)
	server, err := conformancetest.NewServer(f.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	f.server = server
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), f.pod); err != nil {
		t.Fatal(err)
	}
	if !sameContainer {
		status := &f.pod.Status.ContainerStatuses[0]
		status.ContainerID = "containerd://container-2"
		status.RestartCount++
		status.State.Running.StartedAt = metav1.NewTime(witness.StartedAt.Add(40 * time.Second))
		if retainTermination {
			status.LastTerminationState.Terminated = recoveryTerminal(witness)
		}
		if err := f.r.Status().Update(t.Context(), f.pod); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.slice), f.slice); err != nil {
		t.Fatal(err)
	}
	f.slice.Ports[0].Port = new(runtimeRecoveryServerPort(t, f.server.URL()))
	if err := f.r.Update(t.Context(), f.slice); err != nil {
		t.Fatal(err)
	}
	f.updateServiceTargetPort(t)
}

func (f *runtimeRecoveryFixture) updateServiceTargetPort(t *testing.T) {
	t.Helper()
	service := &corev1.Service{}
	if err := f.r.Get(t.Context(), client.ObjectKey{Namespace: defaultNS, Name: "runtime"}, service); err != nil {
		t.Fatal(err)
	}
	service.Spec.Ports[0].TargetPort = intstr.FromInt32(*f.slice.Ports[0].Port)
	if err := f.r.Update(t.Context(), service); err != nil {
		t.Fatal(err)
	}
}

func recoveryTerminal(witness agentRuntimeBootWitness) *corev1.ContainerStateTerminated {
	return &corev1.ContainerStateTerminated{ContainerID: witness.ContainerID, StartedAt: witness.StartedAt,
		FinishedAt: metav1.NewTime(witness.StartedAt.Add(30 * time.Second)), ExitCode: 137, Signal: 9, Reason: "Error"}
}

func TestAgentRuntimeRecoveryRejectsMultipleBackendAddressesBeforeEnrollment(t *testing.T) {
	f := newRuntimeRecoveryFixture(t)
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), f.pod); err != nil {
		t.Fatal(err)
	}
	f.pod.Status.PodIPs = append(f.pod.Status.PodIPs, corev1.PodIP{IP: "::1"})
	if err := f.r.Status().Update(t.Context(), f.pod); err != nil {
		t.Fatal(err)
	}
	second := f.slice.DeepCopy()
	second.Name, second.UID, second.ResourceVersion = "runtime-endpoints-v6", "slice-v6-uid", ""
	second.AddressType = discoveryv1.AddressTypeIPv6
	second.Endpoints[0].Addresses = []string{"::1"}
	if err := f.r.Create(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	before := f.server.Counts()
	f.reconcile(t)
	if f.runtime.Status.Ready {
		t.Fatal("multi-address recovery backend became ready")
	}
	if _, err := loadAgentRuntimeBootWitness(t.Context(), f.control, defaultNS, f.runtime.UID, f.server.Fence().SupervisorBootID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("multi-address recovery persisted a boot witness: %v", err)
	}
	if f.server.Counts() != before {
		t.Fatal("multi-address recovery reached runtime lifecycle operations")
	}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), f.pod); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(f.pod, agentRuntimeRecoveryPodFinalizer) {
		t.Fatal("unsupported recovery backend retained its Pod")
	}
}

func TestAgentRuntimeRecoveryEnrollsBeforeConformanceAndReplacesEpoch(t *testing.T) {
	f := newRuntimeRecoveryFixture(t)
	f.reconcile(t)
	if !f.runtime.Status.Ready {
		t.Fatalf("enrollment conformance failed: %s", f.runtime.Status.Message)
	}
	witness := f.witness(t)
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), f.pod); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(f.runtime, agentRuntimeFinalizer) || !controllerutil.ContainsFinalizer(f.pod, agentRuntimeRecoveryPodFinalizer) {
		t.Fatal("conformance admitted an unretained runtime or Pod")
	}
	secret := &corev1.Secret{}
	if err := f.r.Get(t.Context(), client.ObjectKey{Namespace: defaultNS, Name: recoveryBootSecretName(witness)}, secret); err != nil || secret.Immutable == nil || !*secret.Immutable {
		t.Fatalf("immutable boot authority missing: %v", err)
	}
	counts := f.server.Counts()
	if counts.SessionCreates == 0 || counts.PromptStarts == 0 {
		t.Fatal("conformance never exercised real lifecycle requests")
	}
	f.advanceEpoch(t)
	f.reconcile(t)
	if f.runtime.Status.Ready {
		t.Fatal("old epoch remained admissible")
	}
	f.reconcile(t)
	retired, err := loadAgentRuntimeBootRetirement(t.Context(), f.control, witness)
	if err != nil || !retired {
		t.Fatalf("old boot did not commit authenticated drain proof: %v", err)
	}
	deployment := &appsv1.Deployment{}
	if err := f.r.Get(t.Context(), client.ObjectKey{Namespace: defaultNS, Name: "runtime"}, deployment); err != nil {
		t.Fatal(err)
	}
	_, epoch, err := recoveryTemplateDigest(deployment.Spec.Template, "supervisor")
	if err != nil || epoch != 2 {
		t.Fatalf("epoch replacement was not applied: %d, %v", epoch, err)
	}
	if f.server.Counts() != counts {
		t.Fatal("epoch rollover replayed a Session or prompt")
	}
	// An interrupted reconcile after its exact epoch patch is idempotent.
	f.reconcile(t)
	if f.runtime.Status.Ready || f.server.Counts() != counts {
		t.Fatal("a drained old boot became admissible during replacement")
	}
}

func TestAgentRuntimeRecoveryRejectsChangedPhysicalAuthority(t *testing.T) {
	tests := []struct {
		name   string
		change func(*runtimeRecoveryFixture) client.Object
	}{
		{"Deployment consent", func(f *runtimeRecoveryFixture) client.Object {
			d, _, _, _ := f.r.recoveryDeployment(context.Background(), f.runtime)
			d.Annotations[agentRuntimeRecoveryOwnerAnnotation] = "other"
			return d
		}},
		{"ReplicaSet executable", func(f *runtimeRecoveryFixture) client.Object {
			f.rs.Spec.Template.Spec.Containers[0].Command = []string{"unexpected"}
			return f.rs
		}},
		{"Pod executable", func(f *runtimeRecoveryFixture) client.Object {
			f.pod.Spec.Containers[0].Command = []string{"unexpected"}
			return f.pod
		}},
		{"Pod namespace", func(f *runtimeRecoveryFixture) client.Object { f.pod.Spec.HostPID = true; return f.pod }},
		{"Pod sidecar", func(f *runtimeRecoveryFixture) client.Object {
			f.pod.Spec.Containers = append(f.pod.Spec.Containers, corev1.Container{Name: "sidecar"})
			return f.pod
		}},
		{"EndpointSlice Pod UID", func(f *runtimeRecoveryFixture) client.Object {
			f.slice.Endpoints[0].TargetRef.UID = "other"
			return f.slice
		}},
		{"EndpointSlice Service UID", func(f *runtimeRecoveryFixture) client.Object {
			f.slice.OwnerReferences[0].UID = "other"
			return f.slice
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newRuntimeRecoveryFixture(t)
			object := test.change(f)
			if err := f.r.Update(t.Context(), object); err != nil {
				t.Fatal(err)
			}
			f.reconcile(t)
			if f.runtime.Status.Ready || f.server.Counts().SessionCreates != 0 {
				t.Fatal("changed physical authority admitted conformance")
			}
		})
	}
}

func TestAgentRuntimeRecoveryExactContainerTermination(t *testing.T) {
	f := newRuntimeRecoveryFixture(t)
	f.reconcile(t)
	witness := f.witness(t)
	for _, lastState := range []bool{false, true} {
		pod := f.pod.DeepCopy()
		terminal := recoveryTerminal(witness)
		if lastState {
			pod.Status.ContainerStatuses[0].ContainerID = "containerd://replacement"
			pod.Status.ContainerStatuses[0].RestartCount++
			pod.Status.ContainerStatuses[0].LastTerminationState.Terminated = terminal
		} else {
			pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: terminal}
		}
		if got, err := witnessedContainerTermination(witness, pod); err != nil || !reflect.DeepEqual(got, terminal) {
			t.Fatalf("exact termination lastState=%t: %v", lastState, err)
		}
		for _, change := range []func(*corev1.Pod){
			func(p *corev1.Pod) { p.UID = "replacement" },
			func(p *corev1.Pod) {
				p.Spec.Containers[0].Env = append(p.Spec.Containers[0].Env, corev1.EnvVar{Name: "CHANGED", Value: "yes"})
			},
			func(p *corev1.Pod) { p.OwnerReferences[0].UID = "replacement" },
			func(p *corev1.Pod) {
				p.Status.ContainerStatuses[0].State.Terminated = nil
				p.Status.ContainerStatuses[0].LastTerminationState.Terminated = nil
			},
		} {
			wrong := pod.DeepCopy()
			change(wrong)
			if got, _ := witnessedContainerTermination(witness, wrong); got != nil {
				t.Fatal("mismatched or absent original termination proved retirement")
			}
		}
	}
	for _, change := range []func(*corev1.ContainerStateTerminated){
		func(p *corev1.ContainerStateTerminated) { p.ContainerID = "other" },
		func(p *corev1.ContainerStateTerminated) { p.StartedAt = metav1.NewTime(p.StartedAt.Add(time.Second)) },
		func(p *corev1.ContainerStateTerminated) { p.FinishedAt = metav1.Time{} },
	} {
		terminal := recoveryTerminal(witness)
		change(terminal)
		if validWitnessContainerTermination(witness, terminal) {
			t.Fatal("mismatched container lifetime was accepted")
		}
	}
}

func TestAgentRuntimeRecoveryEffectCrashTailAndIntegrity(t *testing.T) {
	f := newRuntimeRecoveryFixture(t)
	identity := agentRuntimeRecoveryIdentity(agentRuntimeBootWitnessKind, f.runtime.UID, defaultNS, "test-effect")
	payload := map[string]string{"identity": "original"}
	digest := testControllerDigest("request")
	if _, err := f.control.ReserveExternalEffect(t.Context(), store.ReserveExternalEffectRequest{Identity: identity, RequestDigest: digest, Fence: f.fence}); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]string
	if _, err := readAgentRuntimeRecoveryEffect(t.Context(), f.control, identity, &decoded); !errors.Is(err, store.ErrNotReady) {
		t.Fatalf("pending observation used as proof: %v", err)
	}
	f.advanceEpoch(t)
	if err := persistAgentRuntimeRecoveryEffect(t.Context(), f.control, f.fence, identity, digest, payload); err != nil {
		t.Fatalf("exact reserved observation did not resume after takeover: %v", err)
	}
	effect, err := readAgentRuntimeRecoveryEffect(t.Context(), f.control, identity, &decoded)
	if err != nil || decoded["identity"] != "original" {
		t.Fatalf("resumed effect integrity: %v", err)
	}
	before := bytes.Clone(effect.Response)
	if err := persistAgentRuntimeRecoveryEffect(t.Context(), f.control, f.fence, identity, digest, payload); err != nil {
		t.Fatal(err)
	}
	if err := persistAgentRuntimeRecoveryEffect(t.Context(), f.control, f.fence, identity, digest, map[string]string{"identity": "replacement"}); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("immutable response replacement accepted: %v", err)
	}
	effect, err = readAgentRuntimeRecoveryEffect(t.Context(), f.control, identity, &decoded)
	if err != nil || !bytes.Equal(before, effect.Response) {
		t.Fatalf("effect proof changed across replay: %v", err)
	}
}

func TestAgentRuntimeRecoveryContainerFallbackExcludesRemoteProviders(t *testing.T) {
	f := newRuntimeRecoveryFixture(t)
	f.reconcile(t)
	witness := f.witness(t)
	for _, provider := range []string{"codex", "claude", "copilot", "opencode", "agentkit", "foundry", "other"} {
		local := provider != "foundry" && provider != "other"
		if agentRuntimeLocalContainerRetirementAllowed(provider) != local {
			t.Fatalf("container retirement locality for %s is wrong", provider)
		}
	}
	witness.Spec.Capabilities.Profile.ProviderKind = "foundry"
	witness.Fence.SupervisorBootID = "foundry-boot"
	if err := f.r.persistBootRetirement(t.Context(), witness, agentRuntimeBootRetirement{Kind: "kubernetes-container-termination", ContainerTermination: recoveryTerminal(witness)}, f.fence); err != nil {
		t.Fatal(err)
	}
	if retired, err := loadAgentRuntimeBootRetirement(t.Context(), f.control, witness); retired || !errors.Is(err, store.ErrConflict) {
		t.Fatalf("Foundry remote compute inherited local termination proof: %t, %v", retired, err)
	}
}

func TestAgentRuntimeRecoveryReplacementRequiresOriginalContainerTermination(t *testing.T) {
	for _, test := range []struct {
		name                                  string
		termination, sameContainer, wantReady bool
	}{
		{name: "exact previous-container termination", termination: true, wantReady: true},
		{name: "missing termination"},
		{name: "same-container supervisor replacement", sameContainer: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRuntimeRecoveryFixture(t)
			f.reconcile(t)
			old := f.witness(t)
			f.restartContainer(t, test.termination, test.sameContainer)
			f.reconcile(t)
			if f.runtime.Status.Ready != test.wantReady {
				t.Fatalf("replacement Ready=%t: %s", f.runtime.Status.Ready, f.runtime.Status.Message)
			}
			if test.wantReady {
				if retired, err := loadAgentRuntimeBootRetirement(t.Context(), f.control, old); err != nil || !retired {
					t.Fatalf("original retirement proof missing: %v", err)
				}
				if f.witness(t).Fence.SupervisorBootID == old.Fence.SupervisorBootID || f.server.Counts().SessionCreates != 2 {
					t.Fatal("replacement did not enroll and independently conform")
				}
				f.reconcile(t)
				if !f.runtime.Status.Ready || f.server.Counts().SessionCreates != 2 {
					t.Fatalf("unchanged replacement reprobed or lost readiness: %s", f.runtime.Status.Message)
				}
			} else if f.server.Counts().SessionCreates != 0 {
				t.Fatal("replacement idle status admitted an unproved boot")
			}
		})
	}
}

func TestAgentRuntimeRecoveryResumesWitnessBeforeRetention(t *testing.T) {
	f := newRuntimeRecoveryFixture(t)
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
	digest, err := runtimeWitnessDigest(witness)
	if err != nil {
		t.Fatal(err)
	}
	identity := agentRuntimeRecoveryIdentity(agentRuntimeBootWitnessKind, f.runtime.UID, defaultNS, string(witness.Fence.SupervisorBootID))
	if _, err := f.control.ReserveExternalEffect(t.Context(), store.ReserveExternalEffectRequest{Identity: identity, RequestDigest: digest, Fence: f.fence}); err != nil {
		t.Fatal(err)
	}
	// No metadata retention or mutable conformance status was saved. The same
	// observation must complete, install retention, and only then run probes.
	f.reconcile(t)
	if !f.runtime.Status.Ready || f.server.Counts().SessionCreates != 2 {
		t.Fatalf("interrupted enrollment did not finish: %s", f.runtime.Status.Message)
	}
	if !reflect.DeepEqual(f.witness(t), witness) {
		t.Fatal("enrollment replaced the reserved physical observation")
	}
}

func TestAgentRuntimeRecoveryDeletionRetainsEvidenceUntilExactBootRetires(t *testing.T) {
	f := newRuntimeRecoveryFixture(t)
	f.reconcile(t)
	witness := f.witness(t)
	if err := f.r.Delete(t.Context(), f.runtime); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.runtime), f.runtime); err != nil {
		t.Fatal(err)
	}
	if result, err := f.r.finalizeKubernetesAgentRuntime(t.Context(), f.runtime); err != nil || result.RequeueAfter == 0 {
		t.Fatalf("first cleanup did not request an exact boot drain: %v", err)
	}
	pod := &corev1.Pod{}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), pod); err != nil || !controllerutil.ContainsFinalizer(pod, agentRuntimeRecoveryPodFinalizer) {
		t.Fatalf("Pod retention released before durable retirement: %v", err)
	}
	if retired, err := loadAgentRuntimeBootRetirement(t.Context(), f.control, witness); err != nil || retired {
		t.Fatalf("a drain request was treated as completed proof: %t, %v", retired, err)
	}
	for range 5 {
		if _, err := f.r.finalizeKubernetesAgentRuntime(t.Context(), f.runtime); err != nil {
			t.Fatal(err)
		}
		if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.runtime), f.runtime); apierrors.IsNotFound(err) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.runtime), &corev1alpha1.AgentRuntime{}); !apierrors.IsNotFound(err) {
		t.Fatalf("retired managed registration was not finalized: %v", err)
	}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), pod); err != nil || controllerutil.ContainsFinalizer(pod, agentRuntimeRecoveryPodFinalizer) {
		t.Fatalf("retired Pod retention remained: %v", err)
	}
	secretKey := client.ObjectKey{Namespace: defaultNS, Name: recoveryBootSecretName(witness)}
	if err := f.r.Get(t.Context(), secretKey, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("retired boot authentication remained: %v", err)
	}
	if retired, err := loadAgentRuntimeBootRetirement(t.Context(), f.control, witness); err != nil || !retired {
		t.Fatalf("finalization removed immutable cleanup evidence: %v", err)
	}
}
