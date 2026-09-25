package controller

import (
	"net"
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
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestAgentRuntimeRecoveryEnrollmentAndTakeoverWithAPIServer(t *testing.T) {
	f := newRuntimeRecoveryFixture(t)
	address := recoveryAPITestListenAddress(t)
	f.config.ListenAddress = net.JoinHostPort(address, "0")
	f.server.Close()
	server, err := conformancetest.NewServer(f.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	f.server = server
	c := newArchivedSessionAPIClient(t)
	if err := appsv1.AddToScheme(c.Scheme()); err != nil {
		t.Fatal(err)
	}
	if err := discoveryv1.AddToScheme(c.Scheme()); err != nil {
		t.Fatal(err)
	}
	runtime, secret := testAgentRuntimeAndSecret(t, f.runtime.Spec.Deployment.Endpoint, f.config)
	deployment, rs, pod, service, slice := runtimeRecoveryObjects(t, runtime, f.server.URL())
	pod.Status.PodIP, pod.Status.PodIPs = address, []corev1.PodIP{{IP: address}}
	slice.Endpoints[0].Addresses = []string{address}
	create := func(object client.Object) {
		t.Helper()
		object.SetUID("")
		object.SetResourceVersion("")
		object.SetGeneration(0)
		if err := c.Create(t.Context(), object); err != nil {
			t.Fatalf("create %T: %v", object, err)
		}
	}
	create(deployment)
	runtime.Spec.Deployment.KubernetesRecovery = &corev1alpha1.AgentRuntimeKubernetesRecoverySpec{
		DeploymentName: deployment.Name, DeploymentUID: string(deployment.UID), ContainerName: "supervisor",
	}
	create(runtime)
	deployment.Annotations[agentRuntimeRecoveryOwnerAnnotation] = string(runtime.UID)
	if err := c.Update(t.Context(), deployment); err != nil {
		t.Fatal(err)
	}
	create(secret)
	rs.Spec.Template = *deployment.Spec.Template.DeepCopy()
	rs.Spec.Template.Labels[appsv1.DefaultDeploymentUniqueLabelKey] = "hash-1"
	rs.Spec.Selector = &metav1.LabelSelector{MatchLabels: rs.Spec.Template.Labels}
	rs.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(deployment, appsv1.SchemeGroupVersion.WithKind("Deployment"))}
	create(rs)
	pod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(rs, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))}
	pod.Spec = *deployment.Spec.Template.Spec.DeepCopy()
	pod.Labels = rs.Spec.Template.Labels
	podStatus := *pod.Status.DeepCopy()
	pod.Status = corev1.PodStatus{}
	create(pod)
	pod.Status = podStatus
	if err := c.Status().Update(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	create(service)
	slice.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(service, corev1.SchemeGroupVersion.WithKind("Service"))}
	slice.Endpoints[0].TargetRef.UID = pod.UID
	create(slice)
	control, err := storekube.New(c, defaultNS, storekube.WithAPIReader(c))
	if err != nil {
		t.Fatal(err)
	}
	manager := readyAgentRuntimeTestEpochManager(1)
	epoch, err := control.CompareAndSwapControllerEpoch(t.Context(), store.ControllerEpochCAS{
		NewEpoch: 1, HolderID: manager.HolderID, RequestDigest: testControllerDigest("api-enrollment"), UpdatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.r = &AgentRuntimeReconciler{Client: c, APIReader: c, Scheme: c.Scheme(), ControlStore: control, ControllerEpochManager: manager}
	f.control, f.runtime, f.pod, f.rs, f.slice = control, runtime, pod, rs, slice
	f.fence = store.ControllerEpochFence{Name: epoch.Name, HolderID: epoch.HolderID, Epoch: epoch.Epoch}
	backend, err := f.r.recoveryBackend(t.Context(), runtime)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("fixture listener %s, verified pins %v", server.URL(), backend.witness.Pins)
	connection, err := net.DialTimeout("tcp", backend.witness.Pins[0], time.Second)
	if err != nil {
		t.Fatalf("dial fixture listener: %v", err)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	if !f.runtime.Status.Ready {
		t.Fatalf("real API enrollment failed: %s", f.runtime.Status.Message)
	}
	witness := f.witness(t)
	assertRuntimeRecoveryOwnershipImmutable(t, f)
	f.advanceEpoch(t)
	f.reconcile(t)
	f.reconcile(t)
	retired, err := loadAgentRuntimeBootRetirement(t.Context(), f.control, witness)
	if err != nil || !retired {
		t.Fatalf("real API retirement proof did not survive takeover: %v", err)
	}
	if _, _, nextEpoch, err := f.r.recoveryDeployment(t.Context(), f.runtime); err != nil || nextEpoch != 2 {
		t.Fatalf("real API epoch patch failed: %d, %v", nextEpoch, err)
	}
	if f.runtime.Status.Ready || strings.Contains(f.runtime.Status.Message, f.config.ControllerBearerToken) {
		t.Fatal("old runtime remained ready or exposed authentication")
	}
}

// The real EndpointSlice API rejects loopback addresses. Listen only on a
// concrete local IPv4 interface so enrollment still exercises the real HTTP
// dial pins and the API's unmodified physical identity records.
func recoveryAPITestListenAddress(t *testing.T) string {
	t.Helper()
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range addresses {
		if network, ok := address.(*net.IPNet); ok && network.IP.To4() != nil && network.IP.IsGlobalUnicast() {
			return network.IP.String()
		}
	}
	t.Skip("API-server runtime recovery test requires a local non-loopback IPv4 interface")
	return ""
}

func assertRuntimeRecoveryOwnershipImmutable(t *testing.T, f *runtimeRecoveryFixture) {
	t.Helper()
	for _, change := range []func(*corev1alpha1.AgentRuntime){
		func(runtime *corev1alpha1.AgentRuntime) { runtime.Spec.Deployment.KubernetesRecovery = nil },
		func(runtime *corev1alpha1.AgentRuntime) {
			runtime.Spec.Deployment.KubernetesRecovery.DeploymentUID = "other"
		},
		func(runtime *corev1alpha1.AgentRuntime) {
			runtime.Spec.Deployment.KubernetesRecovery.ContainerName = "other"
		},
	} {
		current := f.runtime.DeepCopy()
		change(current)
		if err := f.r.Update(t.Context(), current); !apierrors.IsInvalid(err) {
			t.Fatalf("recovery ownership transition passed API validation: %v", err)
		}
	}
}
