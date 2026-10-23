/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agenthost"
)

const (
	testHostNamespace = "orka-agent-hosts"
	testUserNamespace = "team"
	testAgentImage    = "registry.example/orka/controller@sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
	testHostImage     = "registry.example/host@sha256:" + "2222222222222222222222222222222222222222222222222222222222222222"
)

type fakeIdleStatus struct {
	status agenthost.IdleStatus
	err    error
}

func (f *fakeIdleStatus) ReadIdleStatus(context.Context, string) (agenthost.IdleStatus, error) {
	return f.status, f.err
}

func agentHostScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func testTemplate(access corev1alpha1.AgentHostAccess, policy corev1alpha1.AgentHostCredentialPolicy) *corev1alpha1.AgentHostTemplate {
	return &corev1alpha1.AgentHostTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "copilotd", Namespace: testUserNamespace, UID: "template-uid"},
		Spec: corev1alpha1.AgentHostTemplateSpec{
			SecurityContext: corev1alpha1.AgentHostSecurityContext{RunAsUser: 1000, RunAsGroup: 1000},
			Containers: []corev1alpha1.AgentHostContainer{{
				Name:         corev1alpha1.AgentHostContainerName,
				Image:        testHostImage,
				VolumeMounts: []corev1alpha1.AgentHostVolumeMount{{Name: "workspace", MountPath: "/workspace"}},
			}},
			Storage:        corev1alpha1.AgentHostStorage{Size: resource.MustParse("20Gi")},
			ListenPort:     7777,
			Access:         access,
			GitHubToken:    policy,
			IdleStatusFile: "idle.json",
		},
	}
}

func testHost() *corev1alpha1.AgentHost {
	return &corev1alpha1.AgentHost{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: testUserNamespace, UID: "host-uid", Generation: 1},
		Spec: corev1alpha1.AgentHostSpec{
			TemplateRef:          corev1alpha1.AgentHostTemplateReference{Name: "copilotd"},
			GitHubTokenSecretRef: &corev1alpha1.AgentHostSecretKeyReference{Name: "alice-github", Key: "token"},
		},
	}
}

func userToken(value string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "alice-github", Namespace: testUserNamespace},
		Data:       map[string][]byte{"token": []byte(value)},
	}
}

type agentHostHarness struct {
	t          *testing.T
	client     client.Client
	reconciler *AgentHostReconciler
	idle       *fakeIdleStatus
	now        time.Time
	// podCreateErr, when set, is how the API server answers a Pod create.
	podCreateErr error
}

func newAgentHostHarness(t *testing.T, objects ...client.Object) *agentHostHarness {
	t.Helper()
	h := &agentHostHarness{t: t, idle: &fakeIdleStatus{}, now: time.Date(2026, 10, 17, 22, 0, 0, 0, time.UTC)}
	h.client = fake.NewClientBuilder().
		WithScheme(agentHostScheme(t)).
		WithStatusSubresource(&corev1alpha1.AgentHost{}).
		WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, isPod := obj.(*corev1.Pod); isPod && h.podCreateErr != nil {
					return h.podCreateErr
				}
				if obj.GetUID() == "" {
					obj.SetUID(uuid.NewUUID())
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()
	h.reconciler = &AgentHostReconciler{
		Client:        h.client,
		Scheme:        h.client.Scheme(),
		HostNamespace: testHostNamespace,
		AgentImage:    testAgentImage,
		Controller: agenthost.ControllerPeer{
			Namespace: "orka-system",
			PodLabels: map[string]string{"control-plane": "controller-manager"},
		},
		IdleStatus: h.idle,
		Now:        func() time.Time { return h.now },
	}
	return h
}

// settle reconciles until the result stops asking for an immediate retry.
func (h *agentHostHarness) settle() {
	h.t.Helper()
	for range 20 {
		result, err := h.reconciler.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Namespace: testUserNamespace, Name: "alice"},
		})
		if err != nil {
			h.t.Fatalf("reconcile: %v", err)
		}
		if result.RequeueAfter > 5*time.Second || result.RequeueAfter == 0 {
			return
		}
	}
}

func (h *agentHostHarness) host() *corev1alpha1.AgentHost {
	h.t.Helper()
	host := &corev1alpha1.AgentHost{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testUserNamespace, Name: "alice"}, host); err != nil {
		h.t.Fatal(err)
	}
	return host
}

func (h *agentHostHarness) names() agenthost.Names {
	return agenthost.ObjectNames(h.host(), testHostNamespace)
}

func (h *agentHostHarness) pod() (*corev1.Pod, bool) {
	h.t.Helper()
	pod := &corev1.Pod{}
	err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testHostNamespace, Name: h.names().Pod}, pod)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return pod, true
}

func (h *agentHostHarness) markPodReady() {
	h.t.Helper()
	pod, ok := h.pod()
	if !ok {
		h.t.Fatal("no Pod to mark ready")
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIP = "10.0.0.7"
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := h.client.Status().Update(context.Background(), pod); err != nil {
		h.t.Fatal(err)
	}
}

func agentHostReady(host *corev1alpha1.AgentHost) *metav1.Condition {
	return meta.FindStatusCondition(host.Status.Conditions, agentHostConditionReady)
}

func TestAgentHostRelayCreatesHardenedObjects(t *testing.T) {
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired),
		testHost(), userToken("ghs_user_token"))
	h.settle()

	host := h.host()
	if host.Status.HostNamespace != testHostNamespace || host.Status.Phase != corev1alpha1.AgentHostPhaseProvisioning {
		t.Fatalf("status = %+v", host.Status)
	}
	if len(host.Status.Objects) != 5 {
		t.Fatalf("receipts = %+v, want token Secrets, PVC, NetworkPolicy and Pod", host.Status.Objects)
	}
	for _, receipt := range host.Status.Objects {
		if receipt.UID == "" || receipt.Nonce == "" {
			t.Fatalf("receipt without UID or nonce: %+v", receipt)
		}
	}

	pod, ok := h.pod()
	if !ok {
		t.Fatal("no host Pod")
	}
	assertHardenedHostPod(t, pod)

	names := h.names()
	copied := &corev1.Secret{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testHostNamespace, Name: names.GitHubTokenSecret}, copied); err != nil {
		t.Fatal(err)
	}
	if string(copied.Data["token"]) != "ghs_user_token" {
		t.Fatal("GitHub token was not copied")
	}
	policy := &networkingv1.NetworkPolicy{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testHostNamespace, Name: names.NetworkPolicy}, policy); err != nil {
		t.Fatal(err)
	}
	if len(policy.Spec.Ingress) != 1 || policy.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "orka-system" {
		t.Fatalf("ingress = %+v", policy.Spec.Ingress)
	}
	connection := &corev1.Secret{}
	err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testUserNamespace, Name: names.Connection}, connection)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("relay hosts get no connection Secret, err = %v", err)
	}

	h.markPodReady()
	h.settle()
	host = h.host()
	if host.Status.Phase != corev1alpha1.AgentHostPhaseReady || agentHostReady(host).Status != metav1.ConditionTrue {
		t.Fatalf("status after ready = %+v", host.Status)
	}
}

func assertHardenedHostPod(t *testing.T, pod *corev1.Pod) {
	t.Helper()
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever || *pod.Spec.AutomountServiceAccountToken {
		t.Fatalf("pod policy = %s automount=%v", pod.Spec.RestartPolicy, *pod.Spec.AutomountServiceAccountToken)
	}
	sidecar := pod.Spec.InitContainers[len(pod.Spec.InitContainers)-1]
	if sidecar.Name != "orka-host-agent" || sidecar.Image != testAgentImage ||
		sidecar.RestartPolicy == nil || *sidecar.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Fatalf("host agent sidecar = %+v", sidecar)
	}
	// Orka's own image stages the connection token and the host agent, so host
	// images need no tools.
	stage := pod.Spec.InitContainers[0]
	if stage.Image != testAgentImage || len(stage.Args) == 0 || stage.Args[0] != "stage" {
		t.Fatalf("staging container = %+v", stage)
	}
	hostContainer := pod.Spec.Containers[0]
	if hostContainer.LivenessProbe == nil || hostContainer.LivenessProbe.HTTPGet.Path != "/readyz" {
		t.Fatalf("host liveness = %+v", hostContainer.LivenessProbe)
	}
	if hostContainer.StartupProbe == nil || hostContainer.StartupProbe.HTTPGet.Path != "/startupz" ||
		hostContainer.ReadinessProbe == nil || hostContainer.ReadinessProbe.HTTPGet.Path != "/readyz" {
		t.Fatalf("host probes = %+v / %+v", hostContainer.StartupProbe, hostContainer.ReadinessProbe)
	}
	env := map[string]string{}
	for _, value := range hostContainer.Env {
		env[value.Name] = value.Value
	}
	if env[agenthost.HostIDEnv] != "host-uid" || env[agenthost.GitHubTokenFileEnv] == "" ||
		env[agenthost.ConnectionTokenFileEnv] == "" || env[agenthost.StatusDirEnv] == "" {
		t.Fatalf("host env = %v", env)
	}
	for _, mount := range sidecar.VolumeMounts {
		if mount.Name == "orka-github-token" {
			t.Fatal("the GitHub token must be mounted into the host container only")
		}
	}
	// The host runs the staged host agent as its init, from a mount it cannot write.
	if !slices.Contains(stage.Args, "--agent-to="+env[agenthost.HostAgentEnv]) {
		t.Fatalf("host agent staged by %v, host runs %q", stage.Args, env[agenthost.HostAgentEnv])
	}
	agentMounted := false
	for _, mount := range hostContainer.VolumeMounts {
		if mount.Name == "orka-connection-token-secret" {
			t.Fatal("the host container must not mount the connection token Secret")
		}
		if mount.Name == "orka-host-agent-bin" {
			agentMounted = mount.ReadOnly && strings.HasPrefix(env[agenthost.HostAgentEnv], mount.MountPath+"/")
		}
	}
	if !agentMounted {
		t.Fatalf("host mounts = %+v; want the host agent read-only", hostContainer.VolumeMounts)
	}
}

// The API server's refusal of a host object, here an unknown RuntimeClass,
// fails the host with its reason instead of retrying in silence, and the host
// recovers once the cause is gone.
func TestAgentHostReportsARefusedPod(t *testing.T) {
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired),
		testHost(), userToken("ghs_user_token"))
	h.podCreateErr = apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "alice",
		errors.New(`pod rejected: RuntimeClass "gvisor" not found`))
	h.settle()
	host := h.host()
	ready := agentHostReady(host)
	if host.Status.Phase != corev1alpha1.AgentHostPhaseFailed || ready.Reason != agentHostReasonObjectRejected ||
		!strings.Contains(ready.Message, `RuntimeClass "gvisor" not found`) {
		t.Fatalf("status = %+v, ready = %+v", host.Status, ready)
	}
	h.podCreateErr = nil
	h.settle()
	if _, ok := h.pod(); !ok {
		t.Fatal("the host must start once the API server accepts its Pod")
	}
}

func TestAgentHostRequiresItsGitHubToken(t *testing.T) {
	host := testHost()
	host.Spec.GitHubTokenSecretRef = nil
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired), host)
	h.settle()
	got := h.host()
	if got.Status.Phase != corev1alpha1.AgentHostPhaseFailed || agentHostReady(got).Reason != agentHostReasonGitHubTokenRequired {
		t.Fatalf("status = %+v", got.Status)
	}
	if _, ok := h.pod(); ok {
		t.Fatal("a host without its token must not start")
	}
}

func TestAgentHostMissingTemplateFails(t *testing.T) {
	h := newAgentHostHarness(t, testHost())
	h.settle()
	if got := h.host(); agentHostReady(got).Reason != agentHostReasonTemplateNotFound {
		t.Fatalf("status = %+v", got.Status)
	}
}

func TestAgentHostNeverAdoptsForeignObjects(t *testing.T) {
	host := testHost()
	names := agenthost.ObjectNames(host, testHostNamespace)
	foreign := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: names.PVC, Namespace: testHostNamespace, UID: "foreign",
			Labels: map[string]string{agenthost.LabelHostUID: string(host.UID)}},
	}
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessStandalone, corev1alpha1.AgentHostCredentialNone),
		host, foreign)
	h.settle()
	got := h.host()
	if agentHostReady(got).Reason != agentHostReasonObjectCollision {
		t.Fatalf("status = %+v", got.Status)
	}
	if _, ok := h.pod(); ok {
		t.Fatal("no Pod may start next to a foreign volume")
	}

	// Deletion leaves the foreign object alone.
	if err := h.client.Delete(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	h.settle()
	pvc := &corev1.PersistentVolumeClaim{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testHostNamespace, Name: names.PVC}, pvc); err != nil || pvc.UID != "foreign" {
		t.Fatalf("foreign PVC was touched: %v %v", err, pvc.UID)
	}
}

func TestAgentHostAdoptsObjectFromAnInterruptedCreate(t *testing.T) {
	host := testHost()
	host.Spec.GitHubTokenSecretRef = nil
	host.Status.HostNamespace = testHostNamespace
	names := agenthost.ObjectNames(host, testHostNamespace)
	// The receipt was recorded but the controller stopped before saving the UID.
	host.Status.Objects = []corev1alpha1.AgentHostObjectReceipt{{Kind: agentHostKindSecret, Name: names.ConnectionTokenSecret, Nonce: "n1"}}
	host.Finalizers = []string{agentHostFinalizer}
	created := agenthost.ConnectionTokenSecret(host, names, "n1", []byte("token-value"))
	created.UID = "created-uid"
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessStandalone, corev1alpha1.AgentHostCredentialNone), host, created)
	h.settle()
	receipt := findAgentHostReceipt(h.host(), agentHostKindSecret, "", names.ConnectionTokenSecret)
	if receipt == nil || receipt.UID != "created-uid" {
		t.Fatalf("receipt = %+v", receipt)
	}
}

func TestAgentHostStandaloneConnectionSecret(t *testing.T) {
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessStandalone, corev1alpha1.AgentHostCredentialNone), testHost())
	h.settle()
	host := h.host()
	if host.Status.ConnectionSecretRef == nil {
		t.Fatal("standalone hosts report a connection Secret")
	}
	connection := &corev1.Secret{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testUserNamespace, Name: host.Status.ConnectionSecretRef.Name}, connection); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(connection, host) || string(connection.Data["port"]) != "7777" ||
		string(connection.Data["namespace"]) != testHostNamespace || len(connection.Data["token"]) != 2*agentHostTokenBytes {
		t.Fatalf("connection Secret = %+v", connection)
	}
	if pod, ok := h.pod(); !ok || pod.Annotations[agenthost.AnnotationGitHubToken] != "" {
		t.Fatal("standalone host without a token template must not mount a GitHub token")
	}
}

func TestAgentHostReplacesATerminatedPod(t *testing.T) {
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired),
		testHost(), userToken("ghs_user_token"))
	h.settle()
	first, _ := h.pod()
	first.Status.Phase = corev1.PodFailed
	if err := h.client.Status().Update(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if _, ok := h.pod(); ok {
		t.Fatal("a failed Pod is replaced only after the restart backoff")
	}
	host := h.host()
	if host.Status.Restarts != 1 || agentHostReady(host).Reason != agentHostReasonRestarting {
		t.Fatalf("status = %+v", host.Status)
	}

	h.now = h.now.Add(11 * time.Second)
	h.settle()
	second, ok := h.pod()
	if !ok || second.UID == first.UID {
		t.Fatalf("terminated Pod was not replaced (ok=%v)", ok)
	}
	if second.Annotations[agenthost.AnnotationNonce] == first.Annotations[agenthost.AnnotationNonce] {
		t.Fatal("a replacement Pod gets a fresh creation nonce")
	}

	// A second failure doubles the wait, and readiness resets the count.
	second.Status.Phase = corev1.PodFailed
	if err := h.client.Status().Update(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	h.settle()
	h.now = h.now.Add(11 * time.Second)
	h.settle()
	if _, ok := h.pod(); ok {
		t.Fatal("the second replacement must wait longer than the first")
	}
	h.now = h.now.Add(10 * time.Second)
	h.settle()
	h.markPodReady()
	h.settle()
	if got := h.host().Status.Restarts; got != 0 {
		t.Fatalf("restarts after the host became ready = %d", got)
	}
}

func TestAgentHostHostContainerExitRetiresThePod(t *testing.T) {
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired),
		testHost(), userToken("ghs_user_token"))
	h.settle()
	pod, _ := h.pod()
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: corev1alpha1.AgentHostContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
	}}
	if err := h.client.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if _, ok := h.pod(); ok {
		t.Fatal("a Pod whose host exited must be retired even while its sidecar runs")
	}
}

func TestAgentHostHelperContainerExitRetiresThePod(t *testing.T) {
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired),
		testHost(), userToken("ghs_user_token"))
	h.settle()
	pod, _ := h.pod()
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: corev1alpha1.AgentHostContainerName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		{Name: "helper", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}},
	}
	if err := h.client.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if _, ok := h.pod(); ok {
		t.Fatal("a Pod whose template helper exited must be retired, since it can never become Ready")
	}
}

func TestAgentHostSuspendsWithoutItsTemplateOrToken(t *testing.T) {
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired),
		testHost(), userToken("ghs_user_token"))
	h.settle()
	for _, object := range []client.Object{
		&corev1alpha1.AgentHostTemplate{ObjectMeta: metav1.ObjectMeta{Name: "copilotd", Namespace: testUserNamespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "alice-github", Namespace: testUserNamespace}},
	} {
		if err := h.client.Delete(context.Background(), object); err != nil {
			t.Fatal(err)
		}
	}
	host := h.host()
	host.Spec.Suspended = true
	if err := h.client.Update(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if _, ok := h.pod(); ok {
		t.Fatal("suspension must stop the Pod even without the template or token")
	}
	if got := h.host().Status.Phase; got != corev1alpha1.AgentHostPhaseSuspended {
		t.Fatalf("phase = %s", got)
	}
}

func TestAgentHostStopsWhenItsTokenIsRevoked(t *testing.T) {
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired),
		testHost(), userToken("ghs_user_token"))
	h.settle()
	names := h.names()
	source := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "alice-github", Namespace: testUserNamespace}}
	if err := h.client.Delete(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if _, ok := h.pod(); ok {
		t.Fatal("a host whose token source is gone must stop")
	}
	copied := &corev1.Secret{}
	err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testHostNamespace, Name: names.GitHubTokenSecret}, copied)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("the copied token must be deleted: %v", err)
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testHostNamespace, Name: names.PVC}, pvc); err != nil {
		t.Fatalf("the state volume must survive a failure: %v", err)
	}
	if got := h.host(); agentHostReady(got).Reason != agentHostReasonGitHubTokenUnavailable {
		t.Fatalf("status = %+v", got.Status)
	}
}

func TestAgentHostRefusesAForgedConnectionSecret(t *testing.T) {
	host := testHost()
	names := agenthost.ObjectNames(host, testHostNamespace)
	forged := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: names.Connection, Namespace: testUserNamespace, UID: "forged",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "core.orka.ai/v1alpha1", Kind: "AgentHost", Name: host.Name, UID: host.UID, Controller: new(true),
		}},
	}}
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessStandalone, corev1alpha1.AgentHostCredentialNone),
		host, forged)
	h.settle()
	if got := h.host(); agentHostReady(got).Reason != agentHostReasonObjectCollision {
		t.Fatalf("status = %+v", got.Status)
	}
	secret := &corev1.Secret{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testUserNamespace, Name: names.Connection}, secret); err != nil {
		t.Fatal(err)
	}
	if len(secret.Data) != 0 {
		t.Fatal("the connection token was written into a Secret this host did not create")
	}
}

func TestAgentHostNeverRecoversUserNamespaceObjectsByNonce(t *testing.T) {
	host := testHost()
	host.Spec.GitHubTokenSecretRef = nil
	host.Finalizers = []string{agentHostFinalizer}
	host.Status.HostNamespace = testHostNamespace
	names := agenthost.ObjectNames(host, testHostNamespace)
	// The receipt is public in status; a user copies its nonce onto a Secret
	// before the controller records the real Secret's UID.
	host.Status.Objects = []corev1alpha1.AgentHostObjectReceipt{{
		Kind: agentHostKindConnectionSecret, Namespace: testUserNamespace, Name: names.Connection, Nonce: "public-nonce",
	}}
	planted := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: names.Connection, Namespace: testUserNamespace, UID: "planted",
		Labels:      map[string]string{agenthost.LabelHostUID: string(host.UID)},
		Annotations: map[string]string{agenthost.AnnotationNonce: "public-nonce"},
	}}
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessStandalone, corev1alpha1.AgentHostCredentialNone),
		host, planted)
	h.settle()
	if got := h.host(); agentHostReady(got).Reason != agentHostReasonObjectCollision {
		t.Fatalf("status = %+v", got.Status)
	}
	secret := &corev1.Secret{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testUserNamespace, Name: names.Connection}, secret); err != nil {
		t.Fatal(err)
	}
	if len(secret.Data) != 0 {
		t.Fatal("the connection token was written into a planted Secret")
	}
}

func TestAgentHostNamespaceChangeFailsWithoutTouchingObjects(t *testing.T) {
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired),
		testHost(), userToken("ghs_user_token"))
	h.settle()
	pod, _ := h.pod()
	h.reconciler.HostNamespace = "orka-agent-hosts-2"
	h.settle()
	if got := h.host(); agentHostReady(got).Reason != agentHostReasonHostNamespaceChanged {
		t.Fatalf("status = %+v", got.Status)
	}
	if after, ok := h.pod(); !ok || after.UID != pod.UID {
		t.Fatal("a host in the old namespace must be left alone until it is deleted")
	}
}

func TestAgentHostSuspendKeepsStateAndResumes(t *testing.T) {
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired),
		testHost(), userToken("ghs_user_token"))
	h.settle()
	host := h.host()
	host.Spec.Suspended = true
	if err := h.client.Update(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if _, ok := h.pod(); ok {
		t.Fatal("suspended host still has a Pod")
	}
	host = h.host()
	if host.Status.Phase != corev1alpha1.AgentHostPhaseSuspended ||
		meta.FindStatusCondition(host.Status.Conditions, agentHostConditionSuspended).Reason != agentHostReasonRequested {
		t.Fatalf("status = %+v", host.Status)
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testHostNamespace, Name: h.names().PVC}, pvc); err != nil {
		t.Fatalf("state volume must survive suspension: %v", err)
	}

	host.Spec.Suspended = false
	if err := h.client.Update(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if _, ok := h.pod(); !ok {
		t.Fatal("resumed host has no Pod")
	}
}

func TestAgentHostIdleSuspendAndWake(t *testing.T) {
	template := testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired)
	template.Spec.IdleSuspend = &corev1alpha1.AgentHostIdleSuspend{After: metav1.Duration{Duration: 30 * time.Minute}}
	h := newAgentHostHarness(t, template, testHost(), userToken("ghs_user_token"))
	h.settle()
	h.markPodReady()

	since := h.now.Add(-10 * time.Minute)
	h.idle.status = agenthost.IdleStatus{Idle: true, IdleSince: &since}
	h.settle()
	if _, ok := h.pod(); !ok {
		t.Fatal("a host idle for less than the window must keep running")
	}
	if got := h.host().Status.IdleSince; got == nil || !got.Time.Equal(since) {
		t.Fatalf("idleSince = %v", got)
	}

	// A stale report never suspends.
	since = h.now.Add(-2 * time.Hour)
	h.idle.status = agenthost.IdleStatus{Idle: true, IdleSince: &since, Stale: true}
	h.settle()
	if _, ok := h.pod(); !ok {
		t.Fatal("a stale idle report must not suspend the host")
	}

	h.idle.status = agenthost.IdleStatus{Idle: true, IdleSince: &since}
	h.settle()
	if _, ok := h.pod(); ok {
		t.Fatal("a host idle past the window must be suspended")
	}
	host := h.host()
	if host.Status.IdleSuspendedAt == nil || host.Status.Phase != corev1alpha1.AgentHostPhaseSuspended {
		t.Fatalf("status = %+v", host.Status)
	}

	// An older wake request does nothing; a newer one starts the host.
	before := metav1.NewTime(host.Status.IdleSuspendedAt.Add(-time.Minute))
	host.Spec.WakeRequestedAt = &before
	if err := h.client.Update(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if _, ok := h.pod(); ok {
		t.Fatal("a wake request older than the suspension must not start the host")
	}
	host = h.host()
	after := metav1.NewTime(host.Status.IdleSuspendedAt.Add(time.Minute))
	host.Spec.WakeRequestedAt = &after
	if err := h.client.Update(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if _, ok := h.pod(); !ok {
		t.Fatal("a wake request must start the host")
	}
	if got := h.host().Status.IdleSuspendedAt; got != nil {
		t.Fatalf("idleSuspendedAt = %v after wake", got)
	}
}

// A wake request dated in the future does nothing until its time, then wakes
// the host once; it never keeps defeating later idle suspensions.
func TestAgentHostFutureWakeRequestWakesOnceAtItsTime(t *testing.T) {
	template := testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired)
	template.Spec.IdleSuspend = &corev1alpha1.AgentHostIdleSuspend{After: metav1.Duration{Duration: 30 * time.Minute}}
	h := newAgentHostHarness(t, template, testHost(), userToken("ghs_user_token"))
	h.settle()
	h.markPodReady()
	suspendIdle := func() {
		t.Helper()
		since := h.now.Add(-2 * time.Hour)
		h.idle.status = agenthost.IdleStatus{Idle: true, IdleSince: &since}
		h.settle()
		if _, ok := h.pod(); ok {
			t.Fatal("a host idle past the window must be suspended")
		}
	}
	suspendIdle()

	host := h.host()
	future := metav1.NewTime(h.now.Add(24 * time.Hour))
	host.Spec.WakeRequestedAt = &future
	if err := h.client.Update(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if _, ok := h.pod(); ok {
		t.Fatal("a wake request dated in the future must not start the host yet")
	}

	h.now = h.now.Add(25 * time.Hour)
	h.settle()
	if _, ok := h.pod(); !ok {
		t.Fatal("a wake request whose time has come must start the host")
	}
	h.markPodReady()
	suspendIdle()
	h.settle()
	if _, ok := h.pod(); ok {
		t.Fatal("the same wake request must not start the host again")
	}
}

// The host's NetworkPolicy follows the controller's current namespace and Pod
// labels, so the controller can keep reading the host's status.
func TestAgentHostNetworkPolicyFollowsTheController(t *testing.T) {
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired),
		testHost(), userToken("ghs_user_token"))
	h.settle()
	h.reconciler.Controller = agenthost.ControllerPeer{
		Namespace: "orka-controller", PodLabels: map[string]string{"orka.ai/network-role": "controller"},
	}
	h.settle()
	policy := &networkingv1.NetworkPolicy{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testHostNamespace, Name: h.names().NetworkPolicy}, policy); err != nil {
		t.Fatal(err)
	}
	from := policy.Spec.Ingress[0].From[0]
	if from.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "orka-controller" ||
		from.PodSelector.MatchLabels["orka.ai/network-role"] != "controller" {
		t.Fatalf("ingress still admits the old controller: %+v", from)
	}
}

func TestAgentHostIdleStatusErrorsNeverSuspend(t *testing.T) {
	template := testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired)
	template.Spec.IdleSuspend = &corev1alpha1.AgentHostIdleSuspend{After: metav1.Duration{Duration: time.Minute}}
	h := newAgentHostHarness(t, template, testHost(), userToken("ghs_user_token"))
	h.settle()
	h.markPodReady()
	h.idle.err = errors.New("unreachable")
	h.settle()
	if _, ok := h.pod(); !ok {
		t.Fatal("an unreadable idle status must not suspend the host")
	}
}

func TestAgentHostRotatesTheGitHubTokenCopy(t *testing.T) {
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired),
		testHost(), userToken("ghs_first"))
	h.settle()
	pod, _ := h.pod()
	source := &corev1.Secret{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testUserNamespace, Name: "alice-github"}, source); err != nil {
		t.Fatal(err)
	}
	source.Data["token"] = []byte("ghs_second")
	if err := h.client.Update(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	h.settle()
	copied := &corev1.Secret{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testHostNamespace, Name: h.names().GitHubTokenSecret}, copied); err != nil {
		t.Fatal(err)
	}
	if string(copied.Data["token"]) != "ghs_second" {
		t.Fatal("the host's copy did not follow the rotated token")
	}
	if after, _ := h.pod(); after.UID != pod.UID {
		t.Fatal("rotating the token must not restart the host")
	}
}

// The daemon reads its identity from the token once, at start, so a token from
// another Secret or key needs a new Pod; a new value in the same one does not.
func TestAgentHostReplacesItsPodForAnotherTokenSecretOrKey(t *testing.T) {
	second := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "alice-github-2", Namespace: testUserNamespace},
		Data:       map[string][]byte{"token": []byte("ghs_second"), "other": []byte("ghs_third")},
	}
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired),
		testHost(), userToken("ghs_first"), second)
	h.settle()
	first, _ := h.pod()
	if got := first.Annotations[agenthost.AnnotationGitHubToken]; got != "alice-github/token" {
		t.Fatalf("token source = %q", got)
	}
	previous := first
	for _, ref := range []corev1alpha1.AgentHostSecretKeyReference{
		{Name: "alice-github-2", Key: "token"},
		{Name: "alice-github-2", Key: "other"},
	} {
		host := h.host()
		host.Spec.GitHubTokenSecretRef = &ref
		if err := h.client.Update(context.Background(), host); err != nil {
			t.Fatal(err)
		}
		h.settle()
		pod, ok := h.pod()
		if !ok || pod.UID == previous.UID {
			t.Fatalf("switching to %s/%s kept the Pod (ok=%v)", ref.Name, ref.Key, ok)
		}
		if got := pod.Annotations[agenthost.AnnotationGitHubToken]; got != ref.Name+"/"+ref.Key {
			t.Fatalf("token source = %q, want %s/%s", got, ref.Name, ref.Key)
		}
		previous = pod
	}
	if got := h.host().Status.Restarts; got != 0 {
		t.Fatalf("a credential change counted as %d restarts", got)
	}
}

// A token from another Secret reaches the host's copy only once the Pod
// started with the old one is gone: the copy is mounted into that Pod, whose
// processes could otherwise read the new token while they stop.
func TestAgentHostCopiesAnotherTokenOnlyAfterTheOldPodIsGone(t *testing.T) {
	bob := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bob-github", Namespace: testUserNamespace},
		Data:       map[string][]byte{"token": []byte("ghs_bob")},
	}
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired),
		testHost(), userToken("ghs_alice"), bob)
	h.settle()
	h.markPodReady()
	h.settle()
	old, _ := h.pod()
	// The kubelet keeps a deleted Pod until its processes have stopped.
	old.Finalizers = []string{"test.orka.ai/stopping"}
	if err := h.client.Update(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	host := h.host()
	host.Spec.GitHubTokenSecretRef = &corev1alpha1.AgentHostSecretKeyReference{Name: "bob-github", Key: "token"}
	if err := h.client.Update(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	h.settle()
	copied := func() string {
		t.Helper()
		secret := &corev1.Secret{}
		if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testHostNamespace, Name: h.names().GitHubTokenSecret}, secret); err != nil {
			t.Fatal(err)
		}
		return string(secret.Data["token"])
	}
	if got := copied(); got != "ghs_alice" {
		t.Fatalf("the copy holds %q while the old Pod is still stopping", got)
	}
	stopping, ok := h.pod()
	if !ok || stopping.UID != old.UID || stopping.DeletionTimestamp.IsZero() {
		t.Fatalf("the old Pod must be stopping (ok=%v)", ok)
	}

	stopping.Finalizers = nil
	if err := h.client.Update(context.Background(), stopping); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if got := copied(); got != "ghs_bob" {
		t.Fatalf("the copy holds %q after the old Pod stopped", got)
	}
	pod, ok := h.pod()
	if !ok || pod.UID == old.UID || pod.Annotations[agenthost.AnnotationGitHubToken] != "bob-github/token" {
		t.Fatalf("no new Pod on the new token (ok=%v)", ok)
	}
}

// A Pod whose UID was never recorded, which the cache has not seen yet, still
// keeps a token from another source out of the copy until it is gone.
func TestAgentHostConfirmsTheOldPodIsGoneWithoutTheCache(t *testing.T) {
	bob := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bob-github", Namespace: testUserNamespace},
		Data:       map[string][]byte{"token": []byte("ghs_bob")},
	}
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired),
		testHost(), userToken("ghs_alice"), bob)
	h.settle()
	old, _ := h.pod()
	old.Finalizers = []string{"test.orka.ai/stopping"}
	if err := h.client.Update(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	// The controller stopped between creating the Pod and recording its UID.
	host := h.host()
	findAgentHostReceipt(host, agentHostKindPod, "", h.names().Pod).UID = ""
	if err := h.client.Status().Update(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	host = h.host()
	host.Spec.GitHubTokenSecretRef = &corev1alpha1.AgentHostSecretKeyReference{Name: "bob-github", Key: "token"}
	if err := h.client.Update(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	h.reconciler.APIReader = h.client
	h.reconciler.Client = interceptor.NewClient(h.client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, isPod := obj.(*corev1.Pod); isPod {
				return apierrors.NewNotFound(corev1.Resource("pods"), key.Name)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	if _, err := h.reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: testUserNamespace, Name: "alice"},
	}); err != nil {
		t.Fatal(err)
	}
	copied := &corev1.Secret{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testHostNamespace, Name: h.names().GitHubTokenSecret}, copied); err != nil {
		t.Fatal(err)
	}
	if got := string(copied.Data["token"]); got != "ghs_alice" {
		t.Fatalf("the copy holds %q while the old Pod is still stopping", got)
	}
	if stopping, ok := h.pod(); !ok || stopping.DeletionTimestamp.IsZero() {
		t.Fatalf("the old Pod must be stopping (ok=%v)", ok)
	}
}

const testNewAgentImage = "registry.example/orka/controller@sha256:" + "3333333333333333333333333333333333333333333333333333333333333333"

func agentUpdatePending(host *corev1alpha1.AgentHost) *metav1.Condition {
	return meta.FindStatusCondition(host.Status.Conditions, agentHostConditionAgentUpdatePending)
}

// A new host agent, such as after an upgrade, reaches a running host when it
// reports idle, so no running turn is lost to the update.
func TestAgentHostMovesToANewHostAgentWhenIdle(t *testing.T) {
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired),
		testHost(), userToken("ghs_user_token"))
	h.settle()
	h.markPodReady()
	h.settle()
	old, _ := h.pod()
	if agentUpdatePending(h.host()) != nil {
		t.Fatal("a host on the current agent has no update pending")
	}

	h.reconciler.AgentImage = testNewAgentImage
	h.idle.status = agenthost.IdleStatus{Idle: false}
	h.settle()
	if pod, _ := h.pod(); pod.UID != old.UID {
		t.Fatal("a busy host must keep its Pod")
	}
	pending := agentUpdatePending(h.host())
	if pending == nil || pending.Status != metav1.ConditionTrue || !pending.LastTransitionTime.Time.Equal(h.now) {
		t.Fatalf("update pending = %+v", pending)
	}

	since := h.now.Add(-time.Hour)
	h.idle.status = agenthost.IdleStatus{Idle: true, IdleSince: &since, Stale: true}
	h.settle()
	if pod, _ := h.pod(); pod.UID != old.UID {
		t.Fatal("a stale idle report must not replace the Pod")
	}

	h.idle.status = agenthost.IdleStatus{Idle: true, IdleSince: &since}
	h.settle()
	pod, ok := h.pod()
	if !ok || pod.UID == old.UID || agenthost.PodAgentImage(pod) != testNewAgentImage {
		t.Fatalf("an idle host must move to the new agent (ok=%v)", ok)
	}
	h.markPodReady()
	h.settle()
	if agentUpdatePending(h.host()) != nil {
		t.Fatal("the update is done")
	}
	if got := h.host().Status.Restarts; got != 0 {
		t.Fatalf("an agent update counted as %d restarts", got)
	}
}

// A host that never reports idle, or has no idle status, still moves to a new
// host agent within a day.
func TestAgentHostMovesToANewHostAgentWithinADay(t *testing.T) {
	template := testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired)
	template.Spec.IdleStatusFile = ""
	h := newAgentHostHarness(t, template, testHost(), userToken("ghs_user_token"))
	h.settle()
	h.markPodReady()
	h.settle()
	old, _ := h.pod()

	h.reconciler.AgentImage = testNewAgentImage
	h.settle()
	h.now = h.now.Add(23 * time.Hour)
	h.settle()
	if pod, _ := h.pod(); pod.UID != old.UID {
		t.Fatal("the host moved before its deadline")
	}
	h.now = h.now.Add(2 * time.Hour)
	h.settle()
	pod, ok := h.pod()
	if !ok || pod.UID == old.UID || agenthost.PodAgentImage(pod) != testNewAgentImage {
		t.Fatalf("a host past its deadline must move to the new agent (ok=%v)", ok)
	}
}

// The update deadline counts from when the controller first saw the new
// agent, through readiness changes, and also moves a Pod that is not ready.
func TestAgentHostAgentUpdateDeadlineSurvivesReadinessChanges(t *testing.T) {
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired),
		testHost(), userToken("ghs_user_token"))
	h.settle()
	h.markPodReady()
	h.settle()
	old, _ := h.pod()

	h.reconciler.AgentImage = testNewAgentImage
	h.idle.status = agenthost.IdleStatus{Idle: false}
	h.settle()
	first := agentUpdatePending(h.host()).LastTransitionTime

	h.now = h.now.Add(23 * time.Hour)
	pod, _ := h.pod()
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	if err := h.client.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if pending := agentUpdatePending(h.host()); pending == nil || !pending.LastTransitionTime.Equal(&first) {
		t.Fatalf("the deadline restarted while the Pod was not ready: %+v", pending)
	}
	if pod, _ := h.pod(); pod.UID != old.UID {
		t.Fatal("the host moved before its deadline")
	}
	h.markPodReady()
	h.settle()
	if pending := agentUpdatePending(h.host()); pending == nil || !pending.LastTransitionTime.Equal(&first) {
		t.Fatalf("the deadline restarted when the Pod was ready again: %+v", pending)
	}

	pod, _ = h.pod()
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	if err := h.client.Status().Update(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(2 * time.Hour)
	h.settle()
	moved, ok := h.pod()
	if !ok || moved.UID == old.UID || agenthost.PodAgentImage(moved) != testNewAgentImage {
		t.Fatalf("a Pod that is not ready past the deadline must move to the new agent (ok=%v)", ok)
	}
}

// Recreating a template under its name moves its hosts to the new template on
// the volumes they already have.
func TestAgentHostMovesToARecreatedTemplate(t *testing.T) {
	template := testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired)
	h := newAgentHostHarness(t, template, testHost(), userToken("ghs_user_token"))
	h.settle()
	h.markPodReady()
	h.settle()
	first, _ := h.pod()
	claim := &corev1.PersistentVolumeClaim{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testHostNamespace, Name: h.names().PVC}, claim); err != nil {
		t.Fatal(err)
	}

	if err := h.client.Delete(context.Background(), template); err != nil {
		t.Fatal(err)
	}
	recreated := testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired)
	recreated.UID = "template-uid-2"
	recreated.Spec.Containers[0].Image = "registry.example/host@sha256:" + strings.Repeat("3", 64)
	if err := h.client.Create(context.Background(), recreated); err != nil {
		t.Fatal(err)
	}
	h.settle()
	second, ok := h.pod()
	if !ok || second.UID == first.UID {
		t.Fatalf("the host kept the Pod from the replaced template (ok=%v)", ok)
	}
	if second.Annotations[agenthost.AnnotationTemplateUID] != "template-uid-2" ||
		second.Spec.Containers[0].Image != recreated.Spec.Containers[0].Image {
		t.Fatalf("new Pod runs template %q with image %q", second.Annotations[agenthost.AnnotationTemplateUID],
			second.Spec.Containers[0].Image)
	}
	after := &corev1.PersistentVolumeClaim{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testHostNamespace, Name: h.names().PVC}, after); err != nil {
		t.Fatal(err)
	}
	if after.UID != claim.UID {
		t.Fatal("moving to a recreated template must keep the host's volume")
	}
	if got := h.host().Status.Restarts; got != 0 {
		t.Fatalf("a template change counted as %d restarts", got)
	}
}

func TestAgentHostLostStateVolumeFailsInsteadOfRecreating(t *testing.T) {
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessStandalone, corev1alpha1.AgentHostCredentialNone), testHost())
	h.settle()
	pvc := &corev1.PersistentVolumeClaim{}
	key := types.NamespacedName{Namespace: testHostNamespace, Name: h.names().PVC}
	if err := h.client.Get(context.Background(), key, pvc); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Delete(context.Background(), pvc); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if got := h.host(); agentHostReady(got).Reason != agentHostReasonStateVolumeLost {
		t.Fatalf("status = %+v", got.Status)
	}
	if err := h.client.Get(context.Background(), key, pvc); !apierrors.IsNotFound(err) {
		t.Fatalf("a lost state volume must not be silently recreated: %v", err)
	}
}

func TestAgentHostDeletionRemovesEverythingItCreated(t *testing.T) {
	h := newAgentHostHarness(t, testTemplate(corev1alpha1.AgentHostAccessRelay, corev1alpha1.AgentHostCredentialRequired),
		testHost(), userToken("ghs_user_token"))
	h.settle()
	host := h.host()
	receipts := append([]corev1alpha1.AgentHostObjectReceipt(nil), host.Status.Objects...)
	if err := h.client.Delete(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	h.settle()
	for _, receipt := range receipts {
		object := newAgentHostObject(receipt.Kind)
		err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testHostNamespace, Name: receipt.Name}, object)
		if !apierrors.IsNotFound(err) {
			t.Fatalf("%s %s survived deletion: %v", receipt.Kind, receipt.Name, err)
		}
	}
	err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testUserNamespace, Name: "alice"}, &corev1alpha1.AgentHost{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("AgentHost kept its finalizer: %v", err)
	}
}

func TestAgentHostObjectNamesAreStableAndBounded(t *testing.T) {
	host := testHost()
	host.Name = strings.Repeat("a", 250)
	names := agenthost.ObjectNames(host, testHostNamespace)
	if len(names.Pod) > 63 || len(names.ConnectionTokenSecret) > 253 || len(names.Connection) > 253 {
		t.Fatalf("names too long: %+v", names)
	}
	if again := agenthost.ObjectNames(host, testHostNamespace); again != names {
		t.Fatal("names must be stable")
	}
	other := testHost()
	other.Name = host.Name
	other.UID = "other-uid"
	if agenthost.ObjectNames(other, testHostNamespace).Pod == names.Pod {
		t.Fatal("hosts with the same name in different namespaces must not share objects")
	}
}
