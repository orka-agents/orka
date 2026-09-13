/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package admission

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/controller/openapi/builder"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/apimachinery/pkg/util/managedfields/managedfieldstest"
	apiserveradmission "k8s.io/apiserver/pkg/admission"
	admissioncel "k8s.io/apiserver/pkg/admission/plugin/cel"
	"k8s.io/apiserver/pkg/admission/plugin/policy/validating"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/matchconditions"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/cel/environment"
	"sigs.k8s.io/yaml"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const gatewayGarbageCollectorUser = "system:serviceaccount:kube-system:generic-garbage-collector"

func TestGatewayTaskProtectionForegroundFinalization(t *testing.T) {
	policy := compileGatewayTaskProtection(t)
	for _, username := range []string{
		gatewayGarbageCollectorUser,
		"system:serviceaccount:kube-system:garbage-collector",
		"system:kube-controller-manager",
	} {
		t.Run(username, func(t *testing.T) {
			oldTask := gatewayPolicyTask()
			newTask := gatewayPolicyFinalizedTask(oldTask)
			require.True(t, policy.allows(t, apiserveradmission.Update, username, "", newTask, oldTask))
		})
	}
	t.Run("last finalizer after Orka cleanup", func(t *testing.T) {
		oldTask := gatewayPolicyTask()
		oldTask.SetFinalizers([]string{metav1.FinalizerDeleteDependents})
		newTask := oldTask.DeepCopy()
		newTask.SetFinalizers(nil)
		require.True(t, policy.allows(t, apiserveradmission.Update, gatewayGarbageCollectorUser, "", newTask, oldTask))
	})
	t.Run("optional fields absent on both objects", func(t *testing.T) {
		oldTask := gatewayPolicyTask()
		for _, field := range []string{"labels", "annotations", "ownerReferences", "managedFields"} {
			unstructured.RemoveNestedField(oldTask.Object, "metadata", field)
		}
		unstructured.RemoveNestedField(oldTask.Object, "status")
		newTask := gatewayPolicyFinalizedTask(oldTask)
		require.True(t, policy.allows(t, apiserveradmission.Update, gatewayGarbageCollectorUser, "", newTask, oldTask))
	})
}

func TestGatewayTaskProtectionForegroundFinalizationWithFieldManager(t *testing.T) {
	policy := compileGatewayTaskProtection(t)
	data, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", "core.orka.ai_tasks.yaml"))
	require.NoError(t, err)
	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.UnmarshalStrict(data, &crd))
	openapi, err := builder.BuildOpenAPIV3(&crd, corev1alpha1.GroupVersion.Version, builder.Options{})
	require.NoError(t, err)
	converter, err := managedfields.NewTypeConverter(openapi.Components.Schemas, false)
	require.NoError(t, err)
	fieldManager := managedfieldstest.NewFakeFieldManager(converter, corev1alpha1.GroupVersion.WithKind("Task"))

	t.Run("unowned foreground finalizer", func(t *testing.T) {
		oldTask := gatewayPolicyTask()
		newTask, err := fieldManager.Update(oldTask.DeepCopy(), gatewayPolicyFinalizedTask(oldTask), "kube-controller-manager")
		require.NoError(t, err)
		require.True(t, policy.allows(t, apiserveradmission.Update, gatewayGarbageCollectorUser, "", newTask.(*unstructured.Unstructured), oldTask))
	})
	for _, tc := range []struct {
		name             string
		initial          []string
		remaining        []string
		foregroundDelete bool
	}{
		{
			name:    "tracked finalizer preserves Orka and other finalizers",
			initial: []string{"orka.ai/cleanup", "foregroundDeletion", "example.org/hold"}, remaining: []string{"orka.ai/cleanup", "example.org/hold"},
		},
		{name: "last tracked finalizer", initial: []string{"foregroundDeletion"}},
		{name: "last unowned finalizer after Orka cleanup", initial: []string{"orka.ai/cleanup"}, foregroundDelete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Record ownership through Kubernetes field management. Tracking
			// only the Orka finalizer by hand misses changes to the finalizer
			// field set when GC removes a tracked value or the list itself.
			initial := gatewayPolicyTask()
			initial.SetManagedFields(nil)
			initial.SetFinalizers(tc.initial)
			empty := &unstructured.Unstructured{}
			empty.SetGroupVersionKind(corev1alpha1.GroupVersion.WithKind("Task"))
			tracked, err := fieldManager.Update(empty, initial, "manager")
			require.NoError(t, err)
			oldTask := tracked.(*unstructured.Unstructured)
			require.NotEmpty(t, oldTask.GetManagedFields())
			if tc.foregroundDelete {
				// Foreground DELETE adds its finalizer directly through storage.
				// The controller then removes its own cleanup finalizer, leaving
				// GC to remove the list whose presence the controller tracked.
				oldTask.SetFinalizers([]string{"orka.ai/cleanup", metav1.FinalizerDeleteDependents})
				withoutOrka := oldTask.DeepCopy()
				withoutOrka.SetFinalizers([]string{metav1.FinalizerDeleteDependents})
				tracked, err = fieldManager.Update(oldTask.DeepCopy(), withoutOrka, "manager")
				require.NoError(t, err)
				oldTask = tracked.(*unstructured.Unstructured)
			}
			newTask := oldTask.DeepCopy()
			newTask.SetFinalizers(tc.remaining)
			updated, err := fieldManager.Update(oldTask.DeepCopy(), newTask, "kube-controller-manager")
			require.NoError(t, err)
			newTask = updated.(*unstructured.Unstructured)
			require.NotEqual(t, oldTask.GetManagedFields(), newTask.GetManagedFields(), "the regression requires a real field-management change")
			require.Equal(t, tc.remaining, newTask.GetFinalizers())
			require.True(t, policy.allows(t, apiserveradmission.Update, gatewayGarbageCollectorUser, "", newTask, oldTask))
		})
	}
}

func TestGatewayTaskProtectionManagedFieldsRequireForegroundRemoval(t *testing.T) {
	policy := compileGatewayTaskProtection(t)
	for _, change := range []string{"added", "changed", "removed"} {
		t.Run(change, func(t *testing.T) {
			oldTask := gatewayPolicyTask()
			newTask := gatewayPolicyFinalizedTask(oldTask)
			switch change {
			case "added":
				oldTask.SetManagedFields(nil)
			case "changed":
				fields := newTask.GetManagedFields()
				fields[0].Manager = "kube-controller-manager"
				newTask.SetManagedFields(fields)
			case "removed":
				newTask.SetManagedFields(nil)
			}
			require.True(t, policy.allows(t, apiserveradmission.Update, gatewayGarbageCollectorUser, "", newTask, oldTask))
			require.False(t, policy.allows(t, apiserveradmission.Update, untrustedUsername, "", newTask, oldTask))
			newTask.SetFinalizers(oldTask.GetFinalizers())
			require.False(t, policy.allows(t, apiserveradmission.Update, gatewayGarbageCollectorUser, "", newTask, oldTask), "managedFields alone does not authorize a GC update")
		})
	}
}

func TestGatewayTaskProtectionRejectsChangesDuringFinalization(t *testing.T) {
	policy := compileGatewayTaskProtection(t)
	changes := []struct {
		name  string
		path  []string
		value any
	}{
		{"specification", []string{"spec", "image"}, "another-image"},
		{"gateway identity", []string{"spec", "requestedBy", "issuer"}, "gateway.orka.ai/another-namespace/gateway"},
		{"status", []string{"status", "phase"}, "Running"},
		{"new top-level field", []string{"other"}, "new-content"},
		{"API version", []string{"apiVersion"}, "core.orka.ai/v2"},
		{"kind", []string{"kind"}, "AnotherTask"},
		{"name", []string{"metadata", "name"}, "another-task"},
		{"generate name", []string{"metadata", "generateName"}, "another-prefix-"},
		{"namespace", []string{"metadata", "namespace"}, "another-namespace"},
		{"UID", []string{"metadata", "uid"}, "another-uid"},
		{"resource version", []string{"metadata", "resourceVersion"}, "2"},
		{"generation", []string{"metadata", "generation"}, int64(2)},
		{"creation timestamp", []string{"metadata", "creationTimestamp"}, "2026-08-02T00:00:00Z"},
		{"deletion timestamp", []string{"metadata", "deletionTimestamp"}, "2026-08-03T00:00:00Z"},
		{"deletion grace period", []string{"metadata", "deletionGracePeriodSeconds"}, int64(10)},
		{"labels", []string{"metadata", "labels"}, map[string]any{"changed": "true"}},
		{"annotations", []string{"metadata", "annotations"}, map[string]any{"changed": "true"}},
		{"owner references", []string{"metadata", "ownerReferences"}, []any{}},
		{"new metadata field", []string{"metadata", "other"}, "new-value"},
		{"Orka finalizer removed", []string{"metadata", "finalizers"}, []any{"example.org/hold"}},
		{"other finalizer removed", []string{"metadata", "finalizers"}, []any{"orka.ai/cleanup"}},
		{"all finalizers removed", []string{"metadata", "finalizers"}, []any{}},
		{"finalizer added", []string{"metadata", "finalizers"}, []any{"orka.ai/cleanup", "example.org/hold", "example.org/other"}},
		{"remaining finalizers reordered", []string{"metadata", "finalizers"}, []any{"example.org/hold", "orka.ai/cleanup"}},
		{"foreground finalizer retained", []string{"metadata", "finalizers"}, []any{"orka.ai/cleanup", "foregroundDeletion", "example.org/hold"}},
	}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			oldTask := gatewayPolicyTask()
			newTask := gatewayPolicyFinalizedTask(oldTask)
			newTask.SetManagedFields(nil)
			require.NoError(t, unstructured.SetNestedField(newTask.Object, change.value, change.path...))
			require.False(t, policy.allows(t, apiserveradmission.Update, gatewayGarbageCollectorUser, "", newTask, oldTask))
		})
	}
	for _, field := range []string{"status", "metadata.labels", "metadata.annotations", "metadata.ownerReferences", "metadata.deletionTimestamp"} {
		t.Run("remove "+field, func(t *testing.T) {
			oldTask := gatewayPolicyTask()
			newTask := gatewayPolicyFinalizedTask(oldTask)
			newTask.SetManagedFields(nil)
			unstructured.RemoveNestedField(newTask.Object, strings.Split(field, ".")...)
			require.False(t, policy.allows(t, apiserveradmission.Update, gatewayGarbageCollectorUser, "", newTask, oldTask))
		})
	}
}

func TestGatewayTaskProtectionFinalizationRequiresDeletingTask(t *testing.T) {
	policy := compileGatewayTaskProtection(t)
	for _, tc := range []struct {
		name   string
		mutate func(oldTask, newTask *unstructured.Unstructured)
	}{
		{"not deleting", func(oldTask, newTask *unstructured.Unstructured) {
			oldTask.SetDeletionTimestamp(nil)
			newTask.SetDeletionTimestamp(nil)
		}},
		{"deletion just requested", func(oldTask, _ *unstructured.Unstructured) {
			oldTask.SetDeletionTimestamp(nil)
		}},
		{"null deletion timestamp", func(oldTask, newTask *unstructured.Unstructured) {
			require.NoError(t, unstructured.SetNestedField(oldTask.Object, nil, "metadata", "deletionTimestamp"))
			require.NoError(t, unstructured.SetNestedField(newTask.Object, nil, "metadata", "deletionTimestamp"))
		}},
		{"foreground finalizer already absent", func(oldTask, newTask *unstructured.Unstructured) {
			oldTask.SetFinalizers(newTask.GetFinalizers())
		}},
		{"no old finalizers", func(oldTask, newTask *unstructured.Unstructured) {
			oldTask.SetFinalizers(nil)
			newTask.SetFinalizers(nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldTask := gatewayPolicyTask()
			newTask := gatewayPolicyFinalizedTask(oldTask)
			tc.mutate(oldTask, newTask)
			require.False(t, policy.allows(t, apiserveradmission.Update, gatewayGarbageCollectorUser, "", newTask, oldTask))
		})
	}
	t.Run("status subresource", func(t *testing.T) {
		oldTask := gatewayPolicyTask()
		newTask := gatewayPolicyFinalizedTask(oldTask)
		require.False(t, policy.allows(t, apiserveradmission.Update, gatewayGarbageCollectorUser, "status", newTask, oldTask))
	})
}

func TestGatewayTaskProtectionPreservesIdentityRestrictions(t *testing.T) {
	policy := compileGatewayTaskProtection(t)
	for _, username := range []string{
		untrustedUsername,
		"system:serviceaccount:tenant-a:generic-garbage-collector",
		"system:serviceaccount:kube-system:namespace-controller",
		"system:serviceaccount:kube-system:deployment-controller",
		"system:serviceaccount:another-namespace:orka-controller-manager",
		"system:serviceaccount:another-namespace:orka-ai-worker",
	} {
		t.Run(username, func(t *testing.T) {
			oldTask := gatewayPolicyTask()
			newTask := gatewayPolicyFinalizedTask(oldTask)
			require.False(t, policy.allows(t, apiserveradmission.Update, username, "", newTask, oldTask))
		})
	}
	for _, username := range []string{
		trustedControllerUser,
		trustedWorkerUser,
		"system:serviceaccount:tenant-a:orka-vendor-worker",
		"system:serviceaccount:tenant-a:orka-container-worker",
	} {
		t.Run("existing writer "+username, func(t *testing.T) {
			oldTask := gatewayPolicyTask()
			newTask := gatewayPolicyFinalizedTask(oldTask)
			require.NoError(t, unstructured.SetNestedField(newTask.Object, "new-image", "spec", "image"))
			require.True(t, policy.allows(t, apiserveradmission.Update, username, "", newTask, oldTask))
		})
	}
	for _, username := range []string{
		gatewayGarbageCollectorUser,
		"system:serviceaccount:kube-system:garbage-collector",
		"system:serviceaccount:kube-system:namespace-controller",
		"system:kube-controller-manager",
	} {
		t.Run("existing cleanup DELETE "+username, func(t *testing.T) {
			require.True(t, policy.allows(t, apiserveradmission.Delete, username, "", nil, gatewayPolicyTask()))
			require.False(t, policy.allows(t, apiserveradmission.Create, username, "", gatewayPolicyTask(), nil))
		})
	}
	t.Run("generic users cannot create or delete Gateway Tasks", func(t *testing.T) {
		require.False(t, policy.allows(t, apiserveradmission.Create, untrustedUsername, "", gatewayPolicyTask(), nil))
		require.False(t, policy.allows(t, apiserveradmission.Delete, untrustedUsername, "", nil, gatewayPolicyTask()))
	})
	t.Run("unrelated Tasks are unaffected", func(t *testing.T) {
		oldTask := gatewayPolicyTask()
		unstructured.RemoveNestedField(oldTask.Object, "spec", "requestedBy")
		newTask := gatewayPolicyFinalizedTask(oldTask)
		require.True(t, policy.allows(t, apiserveradmission.Update, untrustedUsername, "", newTask, oldTask))
	})
}

type gatewayTaskPolicyEvaluator struct {
	compiler    *admissioncel.CompositedCompiler
	conditions  admissioncel.ConditionEvaluator
	validations admissioncel.ConditionEvaluator
}

// Compile the shipped policy with the same CEL compiler and object conversion
// used by kube-apiserver, including its lazy policy variables.
func compileGatewayTaskProtection(t *testing.T) gatewayTaskPolicyEvaluator {
	t.Helper()
	documents, err := splitYAMLDocuments(filepath.Join("..", "..", "config", "admission", "gateway_task_protection.yaml"))
	require.NoError(t, err)
	require.Len(t, documents, 2)
	replacements := strings.NewReplacer(
		"ORKA_NAMESPACE", "orka-system",
		"CONTROLLER_SA", "orka-controller-manager",
		"AI_WORKER_SA", "orka-ai-worker",
		"VENDOR_WORKER_SA", "orka-vendor-worker",
		"CONTAINER_WORKER_SA", "orka-container-worker",
	)
	var policy admissionregistrationv1.ValidatingAdmissionPolicy
	require.NoError(t, yaml.UnmarshalStrict([]byte(replacements.Replace(string(documents[0]))), &policy))
	require.Equal(t, admissionregistrationv1.Fail, *policy.Spec.FailurePolicy)
	var binding admissionregistrationv1.ValidatingAdmissionPolicyBinding
	require.NoError(t, yaml.UnmarshalStrict(documents[1], &binding))
	require.Equal(t, policy.Name, binding.Spec.PolicyName)
	require.Equal(t, []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}, binding.Spec.ValidationActions)

	compiler, err := admissioncel.NewCompositedCompiler(environment.MustBaseEnvSet(environment.DefaultCompatibilityVersion()))
	require.NoError(t, err)
	options := admissioncel.OptionalVariableDeclarations{}
	for _, variable := range policy.Spec.Variables {
		result := compiler.CompileAndStoreVariable(&validating.Variable{
			Name: variable.Name, Expression: variable.Expression,
		}, options, environment.NewExpressions)
		require.Nil(t, result.Error, "variable %s: %v", variable.Name, result.Error)
	}
	conditions := make([]admissioncel.ExpressionAccessor, 0, len(policy.Spec.MatchConditions))
	for _, condition := range policy.Spec.MatchConditions {
		conditions = append(conditions, (*matchconditions.MatchCondition)(&condition))
	}
	validations := make([]admissioncel.ExpressionAccessor, 0, len(policy.Spec.Validations))
	for _, validation := range policy.Spec.Validations {
		validations = append(validations, &validating.ValidationCondition{Expression: validation.Expression})
	}
	evaluator := gatewayTaskPolicyEvaluator{
		compiler:    compiler,
		conditions:  compiler.CompileCondition(conditions, options, environment.NewExpressions),
		validations: compiler.CompileCondition(validations, options, environment.NewExpressions),
	}
	require.Empty(t, evaluator.conditions.CompilationErrors())
	require.Empty(t, evaluator.validations.CompilationErrors())
	return evaluator
}

func (p gatewayTaskPolicyEvaluator) allows(t *testing.T, operation apiserveradmission.Operation, username, subresource string, object, oldObject *unstructured.Unstructured) bool {
	t.Helper()
	kind := corev1alpha1.GroupVersion.WithKind("Task")
	resource := corev1alpha1.GroupVersion.WithResource("tasks")
	var newRuntimeObject, oldRuntimeObject runtime.Object
	if object != nil {
		newRuntimeObject = object
	}
	if oldObject != nil {
		oldRuntimeObject = oldObject
	}
	attributes := apiserveradmission.NewAttributesRecord(newRuntimeObject, oldRuntimeObject, kind, admissionTestNamespace, admissionTestTaskName,
		resource, subresource, operation, nil, false, &user.DefaultInfo{Name: username})
	versioned := &apiserveradmission.VersionedAttributes{
		Attributes: attributes, VersionedKind: kind, VersionedObject: newRuntimeObject, VersionedOldObject: oldRuntimeObject,
	}
	request := admissioncel.CreateAdmissionRequest(attributes, metav1.GroupVersionResource(resource), metav1.GroupVersionKind(kind))
	ctx := p.compiler.CreateContext(context.Background())
	conditions, budget, err := p.conditions.ForInput(ctx, versioned, request, admissioncel.OptionalVariableBindings{}, nil, celconfig.RuntimeCELCostBudget)
	require.NoError(t, err)
	require.NotEmpty(t, conditions)
	for _, condition := range conditions {
		require.NoError(t, condition.Error)
		if condition.EvalResult.Value() == false {
			return true
		}
	}
	validations, _, err := p.validations.ForInput(ctx, versioned, request, admissioncel.OptionalVariableBindings{}, nil, budget)
	require.NoError(t, err)
	require.NotEmpty(t, validations)
	for _, validation := range validations {
		require.NoError(t, validation.Error)
		if validation.EvalResult.Value() != true {
			return false
		}
	}
	return true
}

func gatewayPolicyTask() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": corev1alpha1.GroupVersion.String(),
		"kind":       "Task",
		"metadata": map[string]any{
			"name":                       admissionTestTaskName,
			"namespace":                  admissionTestNamespace,
			"uid":                        "gateway-task-uid",
			"resourceVersion":            "1",
			"generation":                 int64(1),
			"creationTimestamp":          "2026-08-01T00:00:00Z",
			"deletionTimestamp":          "2026-08-02T00:00:00Z",
			"deletionGracePeriodSeconds": int64(0),
			"labels":                     map[string]any{"gateway.orka.ai/gateway": "gateway"},
			"annotations":                map[string]any{"example.org/preserved": "value"},
			"finalizers":                 []any{"orka.ai/cleanup", "foregroundDeletion", "example.org/hold"},
			"ownerReferences": []any{map[string]any{
				"apiVersion": "gateway.orka.ai/v1alpha1", "kind": "Gateway", "name": "gateway", "uid": "gateway-uid",
			}},
			"managedFields": []any{map[string]any{
				"manager": "orka", "operation": "Update", "apiVersion": corev1alpha1.GroupVersion.String(), "fieldsType": "FieldsV1",
				"fieldsV1": map[string]any{"f:metadata": map[string]any{"f:finalizers": map[string]any{"v:\"orka.ai/cleanup\"": map[string]any{}}}},
			}},
		},
		"spec": map[string]any{
			"type": "container", "image": "busybox",
			"requestedBy": map[string]any{"issuer": "gateway.orka.ai/tenant-a/gateway", "subject": "user"},
		},
		"status": map[string]any{"phase": "Succeeded"},
	}}
}

func gatewayPolicyFinalizedTask(oldTask *unstructured.Unstructured) *unstructured.Unstructured {
	newTask := oldTask.DeepCopy()
	newTask.SetFinalizers([]string{"orka.ai/cleanup", "example.org/hold"})
	return newTask
}
