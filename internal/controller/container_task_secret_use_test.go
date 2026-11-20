/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

const (
	secretUseTestSecretValue = "s3cr3t-value-never-logged"
	// secretUseControllerNS differs from the Tasks' namespace so the tests
	// check the exempt ServiceAccount against the controller's namespace.
	secretUseControllerNS = "orka-controller-ns"
)

func secretUseScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1alpha1.AddToScheme, corev1.AddToScheme, admissionregistrationv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

// shippedSecretUsePolicy loads the policy and binding this repository ships.
func shippedSecretUsePolicy() (*admissionregistrationv1.ValidatingAdmissionPolicy, *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "container-task-secret-use-policy",
		"container_task_secret_use_policy.yaml"))
	if err != nil {
		panic(err)
	}
	raw = []byte(strings.NewReplacer("ORKA_NAMESPACE", secretUseControllerNS, "CONTROLLER_SA", "orka-controller").Replace(string(raw)))
	documents := strings.Split(string(raw), "\n---\n")
	policy := &admissionregistrationv1.ValidatingAdmissionPolicy{}
	binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
	if err := yaml.UnmarshalStrict([]byte(documents[0]), policy); err != nil {
		panic(err)
	}
	if err := yaml.UnmarshalStrict([]byte(documents[1]), binding); err != nil {
		panic(err)
	}
	return policy, binding
}

func secretUsePolicyObjects(actions ...admissionregistrationv1.ValidationAction) []client.Object {
	policy, binding := shippedSecretUsePolicy()
	if len(actions) > 0 {
		binding.Spec.ValidationActions = actions
	}
	return []client.Object{
		policy,
		binding,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "bot-key", Namespace: defaultNS},
			Data:       map[string][]byte{"key.pem": []byte(secretUseTestSecretValue)},
		},
	}
}

func secretUseBuilder(t *testing.T, objects ...client.Object) *JobBuilder {
	t.Helper()
	t.Setenv(directSecretMountsEnvVar, "false")
	reader := fake.NewClientBuilder().WithScheme(secretUseScheme(t)).WithObjects(objects...).Build()
	builder := setupJobBuilder()
	builder.ContainerTaskSecretUse = ContainerTaskSecretUsePolicy{Enabled: true, Reader: reader, ControllerNamespace: secretUseControllerNS}
	return builder
}

func secretUseEnv(name, secret string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: "key.pem",
	}}}
}

func secretUseContainerTask() *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nightly", Namespace: defaultNS, UID: types.UID("nightly-uid"),
			CreationTimestamp: metav1.Now(),
		},
		Spec: corev1alpha1.TaskSpec{
			Type:      corev1alpha1.TaskTypeContainer,
			Image:     testBusyboxImage,
			Command:   []string{"true"},
			SecretRef: &corev1alpha1.SecretReference{Name: "bot-key"},
			Env:       []corev1.EnvVar{secretUseEnv("BOT_KEY", "bot-key")},
		},
	}
}

func scheduledRunOf(parent *corev1alpha1.Task) *corev1alpha1.Task {
	run := parent.DeepCopy()
	run.Name = parent.Name + "-1700000000"
	run.UID = types.UID("run-uid")
	run.Spec.Schedule = ""
	run.Labels = map[string]string{
		labels.LabelParentTask:   parent.Name,
		labels.LabelScheduledRun: scheduledRunLabelValue,
	}
	run.Annotations = map[string]string{labels.AnnotationParentTaskName: parent.Name}
	isController := true
	run.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: parent.Name, UID: parent.UID, Controller: &isController,
	}}
	return run
}

func requireSecretDelivered(t *testing.T, builder *JobBuilder, task *corev1alpha1.Task) {
	t.Helper()
	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	pod := job.Spec.Template.Spec
	if !hasVolume(pod.Volumes, testTaskConfigVolume) || !hasVolumeMount(pod.Containers[0].VolumeMounts, testTaskConfigVolume) {
		t.Fatal("spec.secretRef should be mounted under /secrets/task")
	}
	env, ok := findEnvVar(pod.Containers[0].Env, "BOT_KEY")
	if !ok || env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil || env.ValueFrom.SecretKeyRef.Name != "bot-key" {
		t.Fatalf("env secretKeyRef should pass through, got %#v", env)
	}
}

func requireSecretRefused(t *testing.T, builder *JobBuilder, task *corev1alpha1.Task, want string) {
	t.Helper()
	_, err := builder.Build(context.Background(), task, nil, nil)
	if err == nil {
		t.Fatal("Build() should refuse the Task")
	}
	if errors.Is(err, ErrContainerTaskSecretUseUnavailable) {
		t.Fatalf("a policy refusal must fail the Task, not retry: %v", err)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("Build() error = %q, want it to contain %q", err, want)
	}
	if strings.Contains(err.Error(), secretUseTestSecretValue) {
		t.Fatal("the error must never carry a Secret value")
	}
}

func TestContainerTaskSecretUseDeliversToARootTask(t *testing.T) {
	requireSecretDelivered(t, secretUseBuilder(t, secretUsePolicyObjects()...), secretUseContainerTask())
}

func TestContainerTaskSecretUseIsOffByDefault(t *testing.T) {
	t.Setenv(directSecretMountsEnvVar, "false")
	builder := setupJobBuilder()
	task := secretUseContainerTask()
	task.Spec.Env = nil
	job, err := builder.Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if hasVolume(job.Spec.Template.Spec.Volumes, testTaskConfigVolume) {
		t.Fatal("without the mode a container Task's secretRef stays undelivered")
	}
	requireSecretRefused(t, builder, secretUseContainerTask(), "valueFrom.secretKeyRef")
}

func TestContainerTaskSecretUseDoesNotChangeTheDirectSecretMountSwitch(t *testing.T) {
	t.Setenv(directSecretMountsEnvVar, "true")
	builder := setupJobBuilder()
	builder.ContainerTaskSecretUse = ContainerTaskSecretUsePolicy{Enabled: true}
	task := secretUseContainerTask()
	task.Labels = map[string]string{labels.LabelChatSession: "chat"}
	requireSecretDelivered(t, builder, task)
}

func TestContainerTaskSecretUseRequiresTheBoundPolicy(t *testing.T) {
	objects := secretUsePolicyObjects()
	requireSecretRefused(t, secretUseBuilder(t, objects[1:]...), secretUseContainerTask(), "is not installed")
	requireSecretRefused(t, secretUseBuilder(t, objects[0], objects[2]), secretUseContainerTask(), "is not bound")
	requireSecretRefused(t, secretUseBuilder(t, secretUsePolicyObjects(admissionregistrationv1.Audit)...),
		secretUseContainerTask(), "not bound with Deny")
}

func TestContainerTaskSecretUseRetriesWhenThePolicyCannotBeRead(t *testing.T) {
	t.Setenv(directSecretMountsEnvVar, "false")
	reader := fake.NewClientBuilder().WithScheme(secretUseScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return apierrors.NewServiceUnavailable("try again")
		},
	}).Build()
	builder := setupJobBuilder()
	builder.ContainerTaskSecretUse = ContainerTaskSecretUsePolicy{Enabled: true, Reader: reader}
	_, err := builder.Build(context.Background(), secretUseContainerTask(), nil, nil)
	if !errors.Is(err, ErrContainerTaskSecretUseUnavailable) {
		t.Fatalf("Build() error = %v, want a retryable error", err)
	}
}

func TestContainerTaskSecretUseRefusesAgentAndAPIProvenance(t *testing.T) {
	builder := secretUseBuilder(t, secretUsePolicyObjects()...)
	cases := map[string]func(*corev1alpha1.Task){
		"chat session": func(task *corev1alpha1.Task) { task.Labels = map[string]string{labels.LabelChatSession: "s"} },
		"coordinator":  func(task *corev1alpha1.Task) { task.Labels = map[string]string{labels.LabelCoordinator: "true"} },
		"API": func(task *corev1alpha1.Task) {
			task.Annotations = map[string]string{labels.AnnotationRequestedBySource: labels.RequestedBySourceAPI}
		},
		"parent label": func(task *corev1alpha1.Task) { task.Labels = map[string]string{labels.LabelParentTask: "coordinator"} },
		"parent owner": func(task *corev1alpha1.Task) {
			task.OwnerReferences = []metav1.OwnerReference{{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task", Name: "coordinator", UID: "c"}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			task := secretUseContainerTask()
			mutate(task)
			requireSecretRefused(t, builder, task, `names Secret "bot-key"`)
		})
	}
}

func TestContainerTaskSecretUseRefusesOtherNamespacesAndConfigMaps(t *testing.T) {
	builder := secretUseBuilder(t, secretUsePolicyObjects()...)
	task := secretUseContainerTask()
	task.Spec.SecretRef.Namespace = "elsewhere"
	requireSecretRefused(t, builder, task, "only Secrets in its own namespace")

	task = secretUseContainerTask()
	task.Spec.Env = append(task.Spec.Env, corev1.EnvVar{Name: "SETTINGS", ValueFrom: &corev1.EnvVarSource{
		ConfigMapKeyRef: &corev1.ConfigMapKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "settings"}, Key: "v"},
	}})
	requireSecretRefused(t, builder, task, "valueFrom.configMapKeyRef")
}

func TestContainerTaskSecretUseNeverDeliversTheAgentSecret(t *testing.T) {
	builder := secretUseBuilder(t, secretUsePolicyObjects()...)
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: defaultNS},
		Spec:       corev1alpha1.AgentSpec{SecretRef: &corev1.LocalObjectReference{Name: testAgentSecretName}},
	}
	job, err := builder.Build(context.Background(), secretUseContainerTask(), agent, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if hasVolume(job.Spec.Template.Spec.Volumes, "agent-secrets") || hasAgentEnvFromSecret(job.Spec.Template.Spec.Containers[0].EnvFrom) {
		t.Fatal("the grant covers only the Secrets the Task names, never its Agent's")
	}
}

func TestContainerTaskSecretUseScheduledRuns(t *testing.T) {
	parent := secretUseContainerTask()
	parent.Spec.Schedule = "0 3 * * *"

	t.Run("a run of a root scheduled Task receives its Secrets", func(t *testing.T) {
		builder := secretUseBuilder(t, append(secretUsePolicyObjects(), parent.DeepCopy())...)
		requireSecretDelivered(t, builder, scheduledRunOf(parent))
	})

	t.Run("a run that names different Secrets is refused", func(t *testing.T) {
		builder := secretUseBuilder(t, append(secretUsePolicyObjects(), parent.DeepCopy())...)
		run := scheduledRunOf(parent)
		run.Spec.Env = append(run.Spec.Env, secretUseEnv("OTHER", "controller-secret"))
		requireSecretRefused(t, builder, run, "spec differs")
	})

	t.Run("a run with another image is refused", func(t *testing.T) {
		builder := secretUseBuilder(t, append(secretUsePolicyObjects(), parent.DeepCopy())...)
		run := scheduledRunOf(parent)
		run.Spec.Image = "attacker/image"
		requireSecretRefused(t, builder, run, "spec differs")
	})

	t.Run("a run re-pointed at another scheduled Task is refused", func(t *testing.T) {
		other := parent.DeepCopy()
		other.Name = "other"
		other.UID = "other-uid"
		run := scheduledRunOf(parent)
		run.OwnerReferences[0].Name = other.Name
		run.OwnerReferences[0].UID = other.UID
		requireSecretRefused(t, secretUseBuilder(t, append(secretUsePolicyObjects(), parent.DeepCopy(), other)...),
			run, "does not match the scheduled Task the controller recorded")
	})

	t.Run("a run of a scheduled Task of another type is refused", func(t *testing.T) {
		aiParent := parent.DeepCopy()
		aiParent.Spec.Type = corev1alpha1.TaskTypeAI
		requireSecretRefused(t, secretUseBuilder(t, append(secretUsePolicyObjects(), aiParent)...),
			scheduledRunOf(parent), "is not its scheduled Task")
	})

	t.Run("a run whose owner is gone is refused", func(t *testing.T) {
		requireSecretRefused(t, secretUseBuilder(t, secretUsePolicyObjects()...), scheduledRunOf(parent), "no longer exists")
	})

	t.Run("a run whose owner is not scheduled or was replaced is refused", func(t *testing.T) {
		unscheduled := parent.DeepCopy()
		unscheduled.Spec.Schedule = ""
		requireSecretRefused(t, secretUseBuilder(t, append(secretUsePolicyObjects(), unscheduled)...),
			scheduledRunOf(parent), "is not its scheduled Task")

		replaced := parent.DeepCopy()
		replaced.UID = "another-uid"
		requireSecretRefused(t, secretUseBuilder(t, append(secretUsePolicyObjects(), replaced)...),
			scheduledRunOf(parent), "is not its scheduled Task")
	})

	t.Run("a scheduled Task an agent created cannot pass Secrets on", func(t *testing.T) {
		childParent := parent.DeepCopy()
		childParent.Labels = map[string]string{labels.LabelParentTask: "coordinator"}
		requireSecretRefused(t, secretUseBuilder(t, append(secretUsePolicyObjects(), childParent)...),
			scheduledRunOf(parent), "cannot pass Secrets to its runs")
	})

	t.Run("a scheduled-run label with no owner at all is refused", func(t *testing.T) {
		run := scheduledRunOf(parent)
		run.OwnerReferences = nil
		delete(run.Labels, labels.LabelParentTask)
		delete(run.Annotations, labels.AnnotationParentTaskName)
		requireSecretRefused(t, secretUseBuilder(t, append(secretUsePolicyObjects(), parent.DeepCopy())...),
			run, "created by another Task")
	})

	t.Run("a scheduled-run label without a controller owner is refused", func(t *testing.T) {
		run := scheduledRunOf(parent)
		run.OwnerReferences[0].Controller = nil
		requireSecretRefused(t, secretUseBuilder(t, append(secretUsePolicyObjects(), parent.DeepCopy())...),
			run, "created by another Task")
	})
}

func TestContainerTaskSecretUseLeavesTasksWithoutSecretsAlone(t *testing.T) {
	reader := fake.NewClientBuilder().WithScheme(secretUseScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(_ context.Context, _ client.WithWatch, key client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return apierrors.NewNotFound(schema.GroupResource{}, key.Name)
		},
	}).Build()
	t.Setenv(directSecretMountsEnvVar, "false")
	builder := setupJobBuilder()
	builder.ContainerTaskSecretUse = ContainerTaskSecretUsePolicy{Enabled: true, Reader: reader}
	task := secretUseContainerTask()
	task.Spec.SecretRef = nil
	task.Spec.Env = []corev1.EnvVar{{Name: "PLAIN", Value: "x"}}
	task.Labels = map[string]string{labels.LabelChatSession: "chat"}
	if _, err := builder.Build(context.Background(), task, nil, nil); err != nil {
		t.Fatalf("a container Task that names no Secret is unaffected: %v", err)
	}
}

func TestContainerTaskSecretUseRefusesTasksWrittenBeforeThePolicy(t *testing.T) {
	enabled := metav1.NewTime(time.Now().Add(-time.Hour))
	objects := secretUsePolicyObjects()
	for _, object := range objects[:2] {
		object.SetCreationTimestamp(enabled)
	}

	t.Run("a Task created after the policy is delivered", func(t *testing.T) {
		requireSecretDelivered(t, secretUseBuilder(t, objects...), secretUseContainerTask())
	})

	t.Run("a Task created before the policy was never checked", func(t *testing.T) {
		task := secretUseContainerTask()
		task.CreationTimestamp = metav1.NewTime(enabled.Add(-time.Minute))
		requireSecretRefused(t, secretUseBuilder(t, objects...), task, "created before")
	})

	t.Run("a Task created while API servers may still be loading the policy is refused", func(t *testing.T) {
		task := secretUseContainerTask()
		task.CreationTimestamp = metav1.NewTime(enabled.Add(containerTaskSecretUsePropagation / 2))
		requireSecretRefused(t, secretUseBuilder(t, objects...), task, "created before")
	})

	t.Run("a run of a scheduled Task created before the policy is refused", func(t *testing.T) {
		parent := secretUseContainerTask()
		parent.Spec.Schedule = "0 3 * * *"
		parent.CreationTimestamp = metav1.NewTime(enabled.Add(-time.Minute))
		run := scheduledRunOf(parent)
		run.CreationTimestamp = metav1.Now()
		requireSecretRefused(t, secretUseBuilder(t, append(objects, parent)...), run, "created before")
	})
}

func TestLatestChangeCountsSpecUpdates(t *testing.T) {
	created := metav1.NewTime(time.Now().Add(-time.Hour))
	updated := metav1.NewTime(time.Now())
	policy := &metav1.ObjectMeta{CreationTimestamp: created}
	relabelled := &metav1.ObjectMeta{UID: "relabelled", CreationTimestamp: created, Generation: 1,
		ManagedFields: []metav1.ManagedFieldsEntry{{Time: &updated}}}
	if got := latestChange(policy, relabelled); !got.Equal(created.Time) {
		t.Fatalf("latestChange() = %v, want a metadata-only update ignored", got)
	}
	respecified := &metav1.ObjectMeta{UID: "respecified", CreationTimestamp: created, Generation: 2,
		ManagedFields: []metav1.ManagedFieldsEntry{{Time: &updated}}}
	if got := latestChange(policy, respecified); !got.Equal(updated.Time) {
		t.Fatalf("latestChange() = %v, want the binding's later spec update %v", got, updated.Time)
	}
	relabelledLater := metav1.NewTime(updated.Add(time.Hour))
	respecified.ManagedFields = []metav1.ManagedFieldsEntry{{Time: &relabelledLater}}
	if got := latestChange(policy, respecified); !got.Equal(updated.Time) {
		t.Fatalf("latestChange() = %v, want a metadata-only update after a spec change ignored", got)
	}
	respecified.Generation = 3
	if got := latestChange(policy, respecified); !got.Equal(relabelledLater.Time) {
		t.Fatalf("latestChange() = %v, want a new spec change %v", got, relabelledLater.Time)
	}
	if got := latestChange(policy); !got.Equal(created.Time) {
		t.Fatalf("latestChange() = %v, want the creation time %v", got, created.Time)
	}
}

func TestContainerTaskSecretUseRequiresThePolicyToCoverTheNamespace(t *testing.T) {
	failOpen := admissionregistrationv1.Ignore
	cases := map[string]func(*admissionregistrationv1.ValidatingAdmissionPolicy, *admissionregistrationv1.ValidatingAdmissionPolicyBinding){
		"another namespace": func(_ *admissionregistrationv1.ValidatingAdmissionPolicy, b *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
			b.Spec.MatchResources = &admissionregistrationv1.MatchResources{NamespaceSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{corev1.LabelMetadataName: "elsewhere"}}}
		},
		"an object selector": func(_ *admissionregistrationv1.ValidatingAdmissionPolicy, b *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
			b.Spec.MatchResources = &admissionregistrationv1.MatchResources{ObjectSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"checked": "true"}}}
		},
		"an exclusion": func(_ *admissionregistrationv1.ValidatingAdmissionPolicy, b *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
			b.Spec.MatchResources = &admissionregistrationv1.MatchResources{ExcludeResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{}}}
		},
		"a fail-open policy": func(p *admissionregistrationv1.ValidatingAdmissionPolicy, _ *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
			p.Spec.FailurePolicy = &failOpen
		},
		"a policy that names no Secrets": func(p *admissionregistrationv1.ValidatingAdmissionPolicy, _ *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
			p.Spec.Variables[0].Expression = "[]"
		},
		"a policy that checks only updates": func(p *admissionregistrationv1.ValidatingAdmissionPolicy, _ *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
			p.Spec.MatchConditions = append(p.Spec.MatchConditions,
				admissionregistrationv1.MatchCondition{Name: "updates", Expression: "request.operation == 'UPDATE'"})
		},
		"a cluster-only rule": func(p *admissionregistrationv1.ValidatingAdmissionPolicy, _ *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
			cluster := admissionregistrationv1.ClusterScope
			p.Spec.MatchConstraints.ResourceRules[0].Scope = &cluster
		},
		"an exempt ServiceAccount in another namespace": func(p *admissionregistrationv1.ValidatingAdmissionPolicy, _ *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
			for i := range p.Spec.Variables {
				p.Spec.Variables[i].Expression = strings.ReplaceAll(p.Spec.Variables[i].Expression,
					"system:serviceaccount:"+secretUseControllerNS+":", "system:serviceaccount:elsewhere:")
			}
		},
		"a policy that never checks use": func(p *admissionregistrationv1.ValidatingAdmissionPolicy, _ *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
			p.Spec.Validations = []admissionregistrationv1.Validation{{Expression: "true"}}
		},
		"a policy that skips updates": func(p *admissionregistrationv1.ValidatingAdmissionPolicy, _ *admissionregistrationv1.ValidatingAdmissionPolicyBinding) {
			p.Spec.MatchConstraints.ResourceRules[0].Operations = []admissionregistrationv1.OperationType{admissionregistrationv1.Create}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			objects := secretUsePolicyObjects()
			mutate(objects[0].(*admissionregistrationv1.ValidatingAdmissionPolicy),
				objects[1].(*admissionregistrationv1.ValidatingAdmissionPolicyBinding))
			requireSecretRefused(t, secretUseBuilder(t, objects...), secretUseContainerTask(), "cannot receive Secrets")
		})
	}

	t.Run("a binding scoped to the Task's namespace is enough", func(t *testing.T) {
		objects := secretUsePolicyObjects()
		objects[1].(*admissionregistrationv1.ValidatingAdmissionPolicyBinding).Spec.MatchResources = &admissionregistrationv1.MatchResources{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{corev1.LabelMetadataName: defaultNS}}}
		requireSecretDelivered(t, secretUseBuilder(t, objects...), secretUseContainerTask())
	})
}

func TestContainerTaskSecretUseScheduledRunWithCRDDefaults(t *testing.T) {
	parent := secretUseContainerTask()
	parent.Spec.Schedule = "0 3 * * *"
	parent.Spec.ConcurrencyPolicy = corev1alpha1.ForbidConcurrent
	run := scheduledRunOf(parent)
	// The API server defaults these on the run after the controller clears them.
	run.Spec.ConcurrencyPolicy = corev1alpha1.ForbidConcurrent
	deadline := int64(100)
	run.Spec.StartingDeadlineSeconds = &deadline
	requireSecretDelivered(t, secretUseBuilder(t, append(secretUsePolicyObjects(), parent.DeepCopy())...), run)
}

func TestContainerTaskSecretUseAcceptsTheShippedPolicyAsTheAPIServerStoresIt(t *testing.T) {
	objects := secretUsePolicyObjects()
	policy := objects[0].(*admissionregistrationv1.ValidatingAdmissionPolicy)
	// The API server fills in these defaults; they must not count as drift.
	policy.Spec = normalizedPolicySpec(policy.Spec)
	for i := range policy.Spec.Variables {
		policy.Spec.Variables[i].Expression = strings.ReplaceAll(policy.Spec.Variables[i].Expression,
			"system:serviceaccount:"+secretUseControllerNS+":orka-controller", "system:serviceaccount:"+secretUseControllerNS+":test-orka")
	}
	requireSecretDelivered(t, secretUseBuilder(t, objects...), secretUseContainerTask())
}

func TestEmbeddedSecretUsePolicyMatchesTheShippedOne(t *testing.T) {
	shipped, err := os.ReadFile(filepath.Join("..", "..", "config", "container-task-secret-use-policy",
		"container_task_secret_use_policy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(shipped) != string(shippedContainerTaskSecretUsePolicy) {
		t.Fatal("internal/controller/container_task_secret_use_policy.yaml must be a copy of " +
			"config/container-task-secret-use-policy/container_task_secret_use_policy.yaml")
	}
}

func TestLatestChangeWithoutManagedFieldsAfterASpecChange(t *testing.T) {
	created := metav1.NewTime(time.Now().Add(-time.Hour))
	reset := &metav1.ObjectMeta{UID: "reset-managed-fields", CreationTimestamp: created, Generation: 2}
	before := time.Now()
	if got := latestChange(reset); got.Before(before) {
		t.Fatalf("latestChange() = %v, want no earlier than when it was first seen (%v)", got, before)
	}
}
