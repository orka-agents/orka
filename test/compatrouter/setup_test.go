package compatrouter

import (
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const routerNamespace = "orka-router-system"

type liveEnvironment struct {
	config    *rest.Config
	kube      client.Client
	clientset kubernetes.Interface
	url       string
	tokens    map[string]string
}

func newLiveEnvironment(t *testing.T) *liveEnvironment {
	t.Helper()
	path := os.Getenv("ORKA_COMPAT_ROUTER_E2E_KUBECONFIG")
	if path == "" {
		t.Skip("run scripts/compat-router-e2e.sh with a disposable worktree-scoped cluster")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", path)
	require.NoError(t, err)
	// Require an explicit kind context; never adopt ordinary shared namespaces.
	raw, err := clientcmd.LoadFromFile(path)
	require.NoError(t, err)
	require.True(
		t,
		strings.HasPrefix(raw.CurrentContext, "kind-"),
		"deployed test requires a disposable kind cluster",
	)
	apiScheme := runtime.NewScheme()
	require.NoError(t, scheme.AddToScheme(apiScheme))
	require.NoError(t, corev1alpha1.AddToScheme(apiScheme))
	kube, err := client.New(cfg, client.Options{Scheme: apiScheme})
	require.NoError(t, err)
	clientset, err := kubernetes.NewForConfig(cfg)
	require.NoError(t, err)
	f := &liveEnvironment{
		config:    cfg,
		kube:      kube,
		clientset: clientset,
		tokens:    map[string]string{},
	}
	controllerImage := os.Getenv("ORKA_COMPAT_ROUTER_E2E_IMAGE")
	workerImage := os.Getenv("ORKA_COMPAT_ROUTER_E2E_WORKER_IMAGE")
	modelImage := os.Getenv("ORKA_COMPAT_ROUTER_E2E_MODEL_IMAGE")
	require.NotEmpty(t, controllerImage)
	require.NotEmpty(t, workerImage)
	require.NotEmpty(t, modelImage)
	for _, ns := range []string{"team-a", "team-b", "team-a-runtimes", "team-b-runtimes"} {
		f.create(
			t,
			&corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name:   ns,
					Labels: map[string]string{"orka.ai/controller-mode": "harness-v2"},
				},
			},
		)
	}
	f.installRouter(t, controllerImage)
	f.create(t, &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "compat-worker"}})
	for _, namespace := range []string{"team-a", "team-b"} {
		f.installController(t, namespace, controllerImage, workerImage)
		f.installCallers(t, namespace)
	}
	f.create(t, deployment(routerNamespace, "model", modelImage, nil, nil))
	f.create(t, service(routerNamespace, "model"))
	for _, namespace := range []string{"team-a", "team-b"} {
		f.create(
			t,
			&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "same-secret", Namespace: namespace},
				Data:       map[string][]byte{"api-key": []byte("fixture-" + namespace)},
			},
		)
		f.create(t, &corev1alpha1.Provider{
			ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: namespace},
			Spec: corev1alpha1.ProviderSpec{
				Type:         corev1alpha1.ProviderTypeOpenAI,
				BaseURL:      "http://model." + routerNamespace + ".svc:8080/v1",
				DefaultModel: namespace + "-catalog",
				SecretRef:    corev1alpha1.ProviderSecretRef{Name: "same-secret", Key: "api-key"},
			},
		})
		f.create(t, &corev1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "same-agent", Namespace: namespace},
			Spec: corev1alpha1.AgentSpec{
				ProviderRef: &corev1alpha1.ProviderReference{Name: "shared"},
				Model: &corev1alpha1.ModelConfig{
					Provider: "openai",
					Name:     namespace + "-agent",
				},
			},
		})
		f.create(t, &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: "same-task", Namespace: namespace},
			Spec: corev1alpha1.TaskSpec{
				Type:    corev1alpha1.TaskTypeContainer,
				Command: []string{"sh", "-c"},
				Args:    []string{"printf 'RESULT:" + namespace + "'"},
			},
		})
	}
	for _, key := range []client.ObjectKey{
		{Namespace: routerNamespace, Name: "orka-compat-router"}, {Namespace: routerNamespace, Name: "model"},
		{Namespace: "team-a", Name: "controller"}, {Namespace: "team-b", Name: "controller"},
	} {
		require.Eventually(t, func() bool {
			var dep appsv1.Deployment
			return f.kube.Get(t.Context(), key, &dep) == nil && dep.Status.ReadyReplicas == 1
		}, 3*time.Minute, time.Second, "deployment %s must become ready", key)
	}
	f.url = f.forwardRouter(t)
	return f
}

func (f *liveEnvironment) create(t *testing.T, object client.Object) {
	t.Helper()
	// An existing object is an error, so this test cannot overwrite another run.
	require.NoError(
		t,
		f.kube.Create(t.Context(), object),
		"create %T %s",
		object,
		client.ObjectKeyFromObject(object),
	)
}

func (f *liveEnvironment) installRouter(t *testing.T, image string) {
	t.Helper()
	file, err := os.Open("../../config/compat-router/router.yaml")
	require.NoError(t, err)
	defer func() { _ = file.Close() }()
	decoder := yaml.NewYAMLOrJSONDecoder(file, 4096)
	for {
		object := &unstructured.Unstructured{}
		err := decoder.Decode(object)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		switch object.GetKind() {
		case "ConfigMap":
			require.NoError(t, unstructured.SetNestedField(
				object.Object,
				"namespaces:\n  team-a: http://api.team-a.svc:8080\n  team-b: http://api.team-b.svc:8080\n",
				"data",
				"routes.yaml",
			))
		case "Deployment":
			containers, _, err := unstructured.NestedSlice(
				object.Object,
				"spec",
				"template",
				"spec",
				"containers",
			)
			require.NoError(t, err)
			containers[0].(map[string]any)["image"] = image
			containers[0].(map[string]any)["imagePullPolicy"] = "Never"
			require.NoError(
				t,
				unstructured.SetNestedSlice(
					object.Object,
					containers,
					"spec",
					"template",
					"spec",
					"containers",
				),
			)
		}
		f.create(t, object)
	}
}

func (f *liveEnvironment) installController(t *testing.T, namespace, image, workerImage string) {
	t.Helper()
	identity := "system:serviceaccount:" + namespace + ":controller"
	f.create(
		t,
		&corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{Name: "controller", Namespace: namespace},
		},
	)
	for _, target := range []string{namespace, namespace + "-runtimes"} {
		f.create(
			t,
			&rbacv1.Role{
				ObjectMeta: metav1.ObjectMeta{Name: "controller", Namespace: target},
				Rules: []rbacv1.PolicyRule{
					{APIGroups: []string{"*"}, Resources: []string{"*"}, Verbs: []string{"*"}},
				},
			},
		)
		f.create(t, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "controller", Namespace: target},
			RoleRef: rbacv1.RoleRef{
				APIGroup: rbacv1.GroupName,
				Kind:     "Role",
				Name:     "controller",
			},
			Subjects: []rbacv1.Subject{
				{Kind: "ServiceAccount", Namespace: namespace, Name: "controller"},
			},
		})
	}
	clusterRoleName := "compat-" + namespace
	f.create(
		t,
		&rbacv1.ClusterRole{
			ObjectMeta: metav1.ObjectMeta{Name: clusterRoleName},
			Rules: []rbacv1.PolicyRule{
				{
					APIGroups: []string{authenticationv1.GroupName},
					Resources: []string{"tokenreviews"},
					Verbs:     []string{"create"},
				},
				{
					APIGroups: []string{"authorization.k8s.io"},
					Resources: []string{"subjectaccessreviews"},
					Verbs:     []string{"create"},
				},
				{
					APIGroups:     []string{""},
					Resources:     []string{"namespaces"},
					ResourceNames: []string{namespace, namespace + "-runtimes"},
					Verbs:         []string{"get"},
				},
				// Startup caches read shared infrastructure metadata. Tenant resources
				// and every write remain covered by the namespace-scoped Roles above.
				{
					APIGroups: []string{"storage.k8s.io"},
					Resources: []string{"storageclasses"},
					Verbs:     []string{"get", "list", "watch"},
				},
				{
					APIGroups: []string{"workspace.orka.ai"},
					Resources: []string{"executionworkspaceproviders", "executionworkspaceclasses"},
					Verbs:     []string{"get", "list", "watch"},
				},
			},
		},
	)
	f.create(t, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: clusterRoleName,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     clusterRoleName,
		},
		Subjects: []rbacv1.Subject{
			{Kind: "ServiceAccount", Namespace: namespace, Name: "controller"},
		},
	})
	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	f.create(
		t,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "snapshot-key", Namespace: namespace},
			Data:       map[string][]byte{"key": key},
		},
	)
	dep := deployment(namespace, "controller", image, []string{"/manager"}, []string{
		"--leader-elect",
		"--controller-mode=harness-v2",
		"--watch-namespace=" + namespace,
		"--enforce-namespace-isolation=true",
		"--execution-mode-controller-usernames=" + identity,
		"--store-backend=sqlite",
		"--store-path=/data/orka.db",
		"--agent-execution-snapshot-key-file=/snapshot/key",
		"--controller-url=http://api." + namespace + ".svc:8080",
		"--acp-runtime-namespace=" + namespace + "-runtimes",
		"--gateway-enabled=false",
		"--general-worker-image=" + workerImage,
		"--ai-worker-cluster-role-name=compat-worker",
		"--vendor-worker-cluster-role-name=compat-worker",
		"--container-worker-cluster-role-name=compat-worker",
		"--chat-max-duration=3m",
		"--chat-max-iterations=12",
	})
	dep.Spec.Template.Spec.ServiceAccountName = "controller"
	dep.Spec.Template.Spec.SecurityContext.FSGroup = ptr.To[int64](65532)
	container := &dep.Spec.Template.Spec.Containers[0]
	container.ReadinessProbe.HTTPGet.Path = "/readyz"
	container.ReadinessProbe.HTTPGet.Port = intstr.FromInt(8081)
	container.Env = []corev1.EnvVar{
		{Name: "POD_NAMESPACE", Value: namespace},
		{Name: "ORKA_ACP_ARTIFACT_ROOT", Value: "/data/acp-artifacts"},
	}
	container.VolumeMounts = []corev1.VolumeMount{
		{Name: "data", MountPath: "/data"},
		{Name: "snapshot", MountPath: "/snapshot", ReadOnly: true},
	}
	dep.Spec.Template.Spec.Volumes = []corev1.Volume{
		{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{
			Name: "snapshot",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: "snapshot-key"},
			},
		},
	}
	f.create(t, dep)
	apiService := service(namespace, "api")
	apiService.Spec.Selector["app"] = "controller"
	f.create(t, apiService)
}

func (f *liveEnvironment) installCallers(t *testing.T, namespace string) {
	t.Helper()
	for _, name := range []string{"editor", "chat-only", "denied"} {
		f.create(
			t,
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}},
		)
		var rules []rbacv1.PolicyRule
		if name != "denied" {
			rules = []rbacv1.PolicyRule{
				{
					APIGroups: []string{"core.orka.ai"},
					Resources: []string{"chats"},
					Verbs:     []string{"create"},
				},
				{
					APIGroups: []string{"core.orka.ai"},
					Resources: []string{"providers"},
					Verbs:     []string{"list"},
				},
			}
		}
		if name == "editor" {
			rules = append(
				rules,
				rbacv1.PolicyRule{
					APIGroups: []string{"core.orka.ai"},
					Resources: []string{"tasks", "agents"},
					Verbs:     []string{"create", "get", "list"},
				},
			)
		}
		f.create(
			t,
			&rbacv1.Role{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
				Rules:      rules,
			},
		)
		f.create(t, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
			Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Namespace: namespace, Name: name}},
		})
		issued, err := f.clientset.CoreV1().
			ServiceAccounts(namespace).
			CreateToken(t.Context(), name, &authenticationv1.TokenRequest{}, metav1.CreateOptions{})
		require.NoError(t, err)
		f.tokens[namespace+"/"+name] = issued.Status.Token
	}
}

func deployment(namespace, name, image string, command, args []string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot: new(true),
						RunAsUser:    ptr.To[int64](65532),
					},
					Containers: []corev1.Container{
						{
							Name:            name,
							Image:           image,
							ImagePullPolicy: corev1.PullNever,
							Command:         command,
							Args:            args,
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/healthz",
										Port: intstr.FromInt(8080),
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func service(namespace, name string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				"app": name,
			},
			Ports: []corev1.ServicePort{{Port: 8080, TargetPort: intstr.FromInt(8080)}},
		},
	}
}

func (f *liveEnvironment) forwardRouter(t *testing.T) string {
	t.Helper()
	pods, err := f.clientset.CoreV1().
		Pods(routerNamespace).
		List(t.Context(), metav1.ListOptions{LabelSelector: "app.kubernetes.io/component=compat-router"})
	require.NoError(t, err)
	require.Len(t, pods.Items, 1)
	transport, upgrader, err := spdy.RoundTripperFor(f.config)
	require.NoError(t, err)
	target := f.clientset.CoreV1().
		RESTClient().
		Post().
		Resource("pods").
		Namespace(routerNamespace).
		Name(pods.Items[0].Name).
		SubResource("portforward").
		URL()
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, target)
	stop, ready := make(chan struct{}), make(chan struct{})
	forward, err := portforward.New(dialer, []string{"0:8080"}, stop, ready, io.Discard, io.Discard)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- forward.ForwardPorts() }()
	t.Cleanup(func() { close(stop) })
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("router port-forward failed: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("router port-forward did not become ready")
	}
	ports, err := forward.GetPorts()
	require.NoError(t, err)
	return fmt.Sprintf("http://127.0.0.1:%d", ports[0].Local)
}
