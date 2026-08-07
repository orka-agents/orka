/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package admission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

const coexistenceControllerUsername = "system:serviceaccount:orka-system:orka-controller-manager"

func TestAgentContractValidatorRequiresAndFreezesExplicitContract(t *testing.T) {
	t.Parallel()
	scheme := coexistenceTestScheme(t)
	validator := &AgentContractValidator{
		decoder: ctrladmission.NewDecoder(scheme),
		config: CoexistenceConfig{
			ControllerUsername:      coexistenceControllerUsername,
			ClassificationUsernames: []string{coexistenceControllerUsername},
		}.normalized(),
	}

	unclassified := coexistenceTestAgent(nil)
	response := validator.Handle(context.Background(), coexistenceRequest(
		t, admissionv1.Create, "alice", unclassified, nil, "",
	))
	require.False(t, response.Allowed)
	require.Contains(t, response.Result.Message, "explicit contractVersion")

	v1 := corev1alpha1.AgentRuntimeContractHarnessV1
	classified := coexistenceTestAgent(&v1)
	response = validator.Handle(context.Background(), coexistenceRequest(
		t, admissionv1.Update, coexistenceControllerUsername, classified, unclassified, "",
	))
	require.True(t, response.Allowed, response.Result.Message)

	changedDuringClassification := classified.DeepCopy()
	changedDuringClassification.Spec.Runtime.DefaultReasoningEffort = "high"
	response = validator.Handle(context.Background(), coexistenceRequest(
		t, admissionv1.Update, coexistenceControllerUsername, changedDuringClassification, unclassified, "",
	))
	require.False(t, response.Allowed)
	require.Contains(t, response.Result.Message, "must not change other executable fields")

	v2 := corev1alpha1.AgentRuntimeContractHarnessV2
	mutatedContract := classified.DeepCopy()
	mutatedContract.Spec.Runtime.ContractVersion = &v2
	response = validator.Handle(context.Background(), coexistenceRequest(
		t, admissionv1.Update, coexistenceControllerUsername, mutatedContract, classified, "",
	))
	require.False(t, response.Allowed)
	require.Contains(t, response.Result.Message, "immutable once explicit")

	external := unclassified.DeepCopy()
	external.Spec.Runtime.Type = ""
	external.Spec.Runtime.RuntimeRef = &corev1alpha1.AgentRuntimeReference{Name: "external"}
	for _, username := range []string{"alice", coexistenceControllerUsername} {
		response = validator.Handle(context.Background(), coexistenceRequest(
			t, admissionv1.Update, username, external, unclassified, "",
		))
		require.False(t, response.Allowed)
		require.Contains(t, response.Result.Message, "cannot switch to an external runtimeRef")

		withoutRuntime := unclassified.DeepCopy()
		withoutRuntime.Spec.Runtime = nil
		response = validator.Handle(context.Background(), coexistenceRequest(
			t, admissionv1.Update, username, withoutRuntime, unclassified, "",
		))
		require.False(t, response.Allowed)
		require.Contains(t, response.Result.Message, "remove its runtime configuration")
	}

	updatedExternal := external.DeepCopy()
	updatedExternal.Spec.Runtime.RuntimeRef.Name = "replacement"
	response = validator.Handle(context.Background(), coexistenceRequest(
		t, admissionv1.Update, "alice", updatedExternal, external, "",
	))
	require.True(t, response.Allowed, response.Result.Message)
}

func TestAgentRuntimeContractValidatorFreezesExplicitContract(t *testing.T) {
	t.Parallel()
	scheme := coexistenceTestScheme(t)
	validator := &AgentRuntimeContractValidator{
		decoder: ctrladmission.NewDecoder(scheme),
		config:  CoexistenceConfig{ControllerUsername: coexistenceControllerUsername}.normalized(),
	}
	v1 := corev1alpha1.AgentRuntimeContractHarnessV1
	v2 := corev1alpha1.AgentRuntimeContractHarnessV2
	oldRuntime := &corev1alpha1.AgentRuntime{
		ObjectMeta: metav1.ObjectMeta{Name: "external", Namespace: admissionTestNamespace},
		Spec:       corev1alpha1.AgentRuntimeRegistrySpec{ContractVersion: &v1},
	}
	newRuntime := oldRuntime.DeepCopy()
	newRuntime.Spec.ContractVersion = &v2
	response := validator.Handle(context.Background(), coexistenceRequest(
		t, admissionv1.Update, coexistenceControllerUsername, newRuntime, oldRuntime, "",
	))
	require.False(t, response.Allowed)
	require.Contains(t, response.Result.Message, "immutable once explicit")
}

func TestTaskExecutionAuthorityValidatorRequiresLiveEnabledRevision(t *testing.T) {
	t.Parallel()
	scheme := coexistenceTestScheme(t)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: admissionTestNamespace,
		UID:  types.UID("namespace-uid"),
	}}
	control := &corev1alpha1.AgentExecutionControl{
		ObjectMeta: metav1.ObjectMeta{
			Name:       corev1alpha1.AgentExecutionControlName,
			Namespace:  corev1alpha1.AgentExecutionControlNamespace,
			UID:        types.UID("control-uid"),
			Generation: 3,
		},
		Status: corev1alpha1.AgentExecutionControlStatus{
			ObservedGeneration: 3,
			Backends: &corev1alpha1.AgentExecutionBackendsStatus{
				V1: corev1alpha1.AgentExecutionBackendStatus{
					EffectiveMode: corev1alpha1.AgentExecutionEffectiveModeDisabled,
					ModeRevision:  2,
				},
				V2: corev1alpha1.AgentExecutionBackendStatus{
					EffectiveMode: corev1alpha1.AgentExecutionEffectiveModeEnabled,
					ModeRevision:  7,
				},
			},
		},
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace, control).Build()
	validator := &TaskExecutionAuthorityValidator{
		decoder: ctrladmission.NewDecoder(scheme),
		reader:  reader,
		config: CoexistenceConfig{
			ControllerUsername: coexistenceControllerUsername,
		}.normalized(),
	}
	oldTask := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name:       admissionTestTaskName,
		Namespace:  admissionTestNamespace,
		UID:        types.UID("task-uid"),
		Generation: 2,
	}}
	newTask := oldTask.DeepCopy()
	newTask.Status.AgentExecutionBinding = coexistenceTestBinding(namespace.UID, control)

	response := validator.Handle(context.Background(), coexistenceRequest(
		t, admissionv1.Update, coexistenceControllerUsername, newTask, oldTask, "status",
	))
	require.True(t, response.Allowed, response.Result.Message)

	response = validator.Handle(context.Background(), coexistenceRequest(
		t, admissionv1.Update, "alice", newTask, oldTask, "status",
	))
	require.False(t, response.Allowed)
	require.Contains(t, response.Result.Message, "only the controller identity")

	staleTask := newTask.DeepCopy()
	staleTask.Status.AgentExecutionBinding.BackendControl.ModeRevision--
	response = validator.Handle(context.Background(), coexistenceRequest(
		t, admissionv1.Update, coexistenceControllerUsername, staleTask, oldTask, "status",
	))
	require.False(t, response.Allowed)
	require.Contains(t, response.Result.Message, "live enabled backend admission revision")

	boundOld := newTask.DeepCopy()
	boundNew := boundOld.DeepCopy()
	boundNew.Spec.Env = []corev1.EnvVar{{Name: "EXECUTION_INPUT", Value: "changed"}}
	response = validator.Handle(context.Background(), coexistenceRequest(
		t, admissionv1.Update, coexistenceControllerUsername, boundNew, boundOld, "",
	))
	require.False(t, response.Allowed)
	require.Contains(t, response.Result.Message, "Task spec is immutable")
}

func TestTaskResolutionAppendRequiresExactApplyingOperation(t *testing.T) {
	t.Parallel()
	scheme := coexistenceTestScheme(t)
	operationDigest := "sha256:" + strings.Repeat("b", 64)
	adjudication := &corev1alpha1.AgentExecutionAdjudication{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cleanup-v1",
			Namespace: admissionTestNamespace,
			UID:       types.UID("adjudication-uid"),
		},
		Spec: corev1alpha1.AgentExecutionAdjudicationSpec{
			TaskRef: corev1alpha1.AgentExecutionSubjectReference{
				Name: admissionTestTaskName,
				UID:  types.UID("task-uid"),
			},
			Action: corev1alpha1.AgentExecutionAdjudicationCleanupV1,
		},
		Status: corev1alpha1.AgentExecutionAdjudicationStatus{
			State:           corev1alpha1.AgentExecutionAdjudicationApplying,
			OperationDigest: operationDigest,
		},
	}
	ref := &corev1alpha1.AgentExecutionResolutionRef{
		AdjudicationName: adjudication.Name,
		AdjudicationUID:  adjudication.UID,
		Action:           adjudication.Spec.Action,
		OperationDigest:  operationDigest,
		AppliedAt:        metav1.Now(),
	}
	digest, err := store.CanonicalAgentExecutionResolutionRefDigest(admissionTestNamespace, ref)
	require.NoError(t, err)
	ref.ResolutionDigest = digest
	oldTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      admissionTestTaskName,
			Namespace: admissionTestNamespace,
			UID:       types.UID("task-uid"),
		},
		Status: corev1alpha1.TaskStatus{
			AgentExecutionQuarantine: &corev1alpha1.AgentExecutionQuarantine{},
		},
	}
	newTask := oldTask.DeepCopy()
	newTask.Status.AgentExecutionResolutionRef = ref

	validator := &TaskExecutionAuthorityValidator{
		decoder: ctrladmission.NewDecoder(scheme),
		reader: fake.NewClientBuilder().WithScheme(scheme).
			WithObjects(adjudication).Build(),
		config: CoexistenceConfig{
			ControllerUsername:             coexistenceControllerUsername,
			AdjudicationControllerUsername: coexistenceControllerUsername,
		}.normalized(),
	}
	response := validator.Handle(context.Background(), coexistenceRequest(
		t, admissionv1.Update, coexistenceControllerUsername, newTask, oldTask, "status",
	))
	require.True(t, response.Allowed, response.Result.Message)

	applied := adjudication.DeepCopy()
	applied.Status.State = corev1alpha1.AgentExecutionAdjudicationApplied
	applied.Status.ResolutionRefDigest = digest
	validator.reader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(applied).Build()
	response = validator.Handle(context.Background(), coexistenceRequest(
		t, admissionv1.Update, coexistenceControllerUsername, newTask, oldTask, "status",
	))
	require.False(t, response.Allowed)
	require.Contains(t, response.Result.Message, "exact Applying adjudication operation")

	tampered := newTask.DeepCopy()
	tampered.Status.AgentExecutionResolutionRef.ResolutionDigest = "sha256:" + strings.Repeat("c", 64)
	validator.reader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(adjudication).Build()
	response = validator.Handle(context.Background(), coexistenceRequest(
		t, admissionv1.Update, coexistenceControllerUsername, tampered, oldTask, "status",
	))
	require.False(t, response.Allowed)
	require.Contains(t, response.Result.Message, "canonical content")
}

func TestCoexistenceConfigNormalizesClassifiersAndAdminGroupsIndependently(t *testing.T) {
	t.Parallel()
	config := (CoexistenceConfig{
		ControllerUsername:      coexistenceControllerUsername,
		ClassificationUsernames: []string{" migration-user ", "migration-user"},
		AdminGroups:             []string{" orka-admins ", "orka-admins"},
	}).normalized()

	require.True(t, config.classifier(coexistenceControllerUsername))
	require.True(t, config.classifier("migration-user"))
	require.True(t, config.admin([]string{"system:authenticated", "orka-admins"}))
	require.Equal(t, []string{"migration-user", coexistenceControllerUsername}, config.ClassificationUsernames)
	require.Equal(t, []string{"orka-admins"}, config.AdminGroups)
}

func TestAgentRuntimeContractValidatorRequiresAuthorizedExplicitClassification(t *testing.T) {
	t.Parallel()
	scheme := coexistenceTestScheme(t)
	validator := &AgentRuntimeContractValidator{
		decoder: ctrladmission.NewDecoder(scheme),
		config:  CoexistenceConfig{ControllerUsername: coexistenceControllerUsername}.normalized(),
	}
	unclassified := &corev1alpha1.AgentRuntime{
		ObjectMeta: metav1.ObjectMeta{Name: "external", Namespace: admissionTestNamespace},
	}
	response := validator.Handle(context.Background(), coexistenceRequest(
		t, admissionv1.Create, "alice", unclassified, nil, "",
	))
	require.False(t, response.Allowed)
	require.Contains(t, response.Result.Message, "explicit contractVersion")

	v1 := corev1alpha1.AgentRuntimeContractHarnessV1
	classified := unclassified.DeepCopy()
	classified.Spec.ContractVersion = &v1
	response = validator.Handle(context.Background(), coexistenceRequest(
		t, admissionv1.Update, "alice", classified, unclassified, "",
	))
	require.False(t, response.Allowed)
	require.Contains(t, response.Result.Message, "authorized bridge classifier")

	response = validator.Handle(context.Background(), coexistenceRequest(
		t, admissionv1.Update, coexistenceControllerUsername, classified, unclassified, "",
	))
	require.True(t, response.Allowed, response.Result.Message)
}

func TestControlPolicyValidatorRestrictsSpecAuthorship(t *testing.T) {
	t.Parallel()
	scheme := coexistenceTestScheme(t)
	validator := &ControlPolicyValidator{
		decoder: ctrladmission.NewDecoder(scheme),
		config:  CoexistenceConfig{AdminGroups: []string{"orka-admins"}}.normalized(),
	}
	control := &corev1alpha1.AgentExecutionControl{
		ObjectMeta: metav1.ObjectMeta{Name: corev1alpha1.AgentExecutionControlName, Namespace: corev1alpha1.AgentExecutionControlNamespace},
		Spec: corev1alpha1.AgentExecutionControlSpec{Backends: corev1alpha1.AgentExecutionBackendsSpec{
			V1: corev1alpha1.AgentExecutionBackendSpec{DesiredMode: corev1alpha1.AgentExecutionModeDisabled},
			V2: corev1alpha1.AgentExecutionBackendSpec{DesiredMode: corev1alpha1.AgentExecutionModeEnabled},
		}},
	}
	response := validator.Handle(context.Background(), coexistenceRequestWithGroups(
		t, admissionv1.Create, "operator", []string{"orka-admins"}, control, nil, "",
	))
	require.True(t, response.Allowed, response.Result.Message)
	response = validator.Handle(context.Background(), coexistenceRequestWithGroups(
		t, admissionv1.Create, "tenant", []string{"system:authenticated"}, control, nil, "",
	))
	require.False(t, response.Allowed)
	require.Contains(t, response.Result.Message, "restricted to configured admin groups")

	changedControl := control.DeepCopy()
	changedControl.Spec.Backends.V1.DesiredMode = corev1alpha1.AgentExecutionModeEnabled
	response = validator.Handle(context.Background(), coexistenceRequestWithGroups(
		t, admissionv1.Update, "tenant", nil, changedControl, control, "",
	))
	require.False(t, response.Allowed)
	response = validator.Handle(context.Background(), coexistenceRequestWithGroups(
		t, admissionv1.Update, "operator", []string{"orka-admins"}, changedControl, control, "",
	))
	require.True(t, response.Allowed, response.Result.Message)

	metadataOnly := control.DeepCopy()
	metadataOnly.Labels = map[string]string{"reviewed": "true"}
	response = validator.Handle(context.Background(), coexistenceRequestWithGroups(
		t, admissionv1.Update, "tenant", nil, metadataOnly, control, "",
	))
	require.True(t, response.Allowed, response.Result.Message)
	response = validator.Handle(context.Background(), coexistenceRequestWithGroups(
		t, admissionv1.Update, coexistenceControllerUsername, nil, changedControl, control, "status",
	))
	require.True(t, response.Allowed, response.Result.Message)

	policy := &corev1alpha1.AgentExecutionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "compatibility", Namespace: admissionTestNamespace},
		Spec: corev1alpha1.AgentExecutionPolicySpec{
			RetryEligibility:        corev1alpha1.AgentExecutionRetryNone,
			NetworkIsolationProfile: corev1alpha1.AgentExecutionNetworkIsolationDefaultDeny,
		},
	}
	response = validator.Handle(context.Background(), coexistenceRequestWithGroups(
		t, admissionv1.Create, "tenant", nil, policy, nil, "",
	))
	require.False(t, response.Allowed)
	response = validator.Handle(context.Background(), coexistenceRequestWithGroups(
		t, admissionv1.Create, "operator", []string{"orka-admins"}, policy, nil, "",
	))
	require.True(t, response.Allowed, response.Result.Message)
	changedPolicy := policy.DeepCopy()
	changedPolicy.Spec.AllowNewV1Bindings = true
	response = validator.Handle(context.Background(), coexistenceRequestWithGroups(
		t, admissionv1.Update, "tenant", nil, changedPolicy, policy, "",
	))
	require.False(t, response.Allowed)
	response = validator.Handle(context.Background(), coexistenceRequestWithGroups(
		t, admissionv1.Update, "operator", []string{"orka-admins"}, changedPolicy, policy, "",
	))
	require.True(t, response.Allowed, response.Result.Message)

	response = validator.Handle(context.Background(), coexistenceRequestWithGroups(
		t, admissionv1.Delete, "tenant", nil, nil, policy, "",
	))
	require.False(t, response.Allowed)
	response = validator.Handle(context.Background(), coexistenceRequestWithGroups(
		t, admissionv1.Delete, "operator", []string{"orka-admins"}, nil, policy, "",
	))
	require.True(t, response.Allowed, response.Result.Message)
	response = validator.Handle(context.Background(), coexistenceRequestWithGroups(
		t, admissionv1.Delete, "system:serviceaccount:kube-system:namespace-controller", nil, nil, policy, "",
	))
	require.True(t, response.Allowed, response.Result.Message)
	response = validator.Handle(context.Background(), coexistenceRequestWithGroups(
		t, admissionv1.Delete, "system:serviceaccount:kube-system:namespace-controller-evil", nil, nil, policy, "",
	))
	require.False(t, response.Allowed)

	unexpected := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "unexpected", Namespace: admissionTestNamespace}}
	response = validator.Handle(context.Background(), coexistenceRequestWithGroups(
		t, admissionv1.Create, "operator", []string{"orka-admins"}, unexpected, nil, "",
	))
	require.False(t, response.Allowed)
	require.Contains(t, response.Result.Message, "unexpected kind")
}

func TestAdjudicationValidatorRestrictsAuthorshipStatusAndDeletion(t *testing.T) {
	t.Parallel()
	scheme := coexistenceTestScheme(t)
	digest := "sha256:" + strings.Repeat("d", 64)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: admissionTestTaskName, Namespace: admissionTestNamespace,
			UID: types.UID("task-uid"), ResourceVersion: "42",
		},
		Status: corev1alpha1.TaskStatus{AgentExecutionQuarantine: &corev1alpha1.AgentExecutionQuarantine{}},
	}
	validator := &AdjudicationValidator{
		decoder: ctrladmission.NewDecoder(scheme),
		reader:  fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).Build(),
		config: (CoexistenceConfig{
			ControllerUsername:             coexistenceControllerUsername,
			AdjudicationControllerUsername: coexistenceControllerUsername,
			AdminGroups:                    []string{"orka-admins"},
		}).normalized(),
	}
	adjudication := &corev1alpha1.AgentExecutionAdjudication{
		ObjectMeta: metav1.ObjectMeta{Name: "resolve-task", Namespace: admissionTestNamespace},
		Spec: corev1alpha1.AgentExecutionAdjudicationSpec{
			TaskRef: corev1alpha1.AgentExecutionSubjectReference{Name: task.Name, UID: task.UID},
			ExpectedState: corev1alpha1.AgentExecutionExpectedSubjectState{
				SubjectResourceVersion: task.ResourceVersion, EvidenceClosureWatermark: digest,
			},
			QuarantineDigest: digest,
			Action:           corev1alpha1.AgentExecutionAdjudicationCleanupBoth,
			RequestedBy:      "operator",
		},
	}
	response := validator.Handle(context.Background(), coexistenceRequestWithGroups(
		t, admissionv1.Create, "tenant", nil, adjudication, nil, "",
	))
	require.False(t, response.Allowed)
	require.Contains(t, response.Result.Message, "configured admin groups")
	response = validator.Handle(context.Background(), coexistenceRequestWithGroups(
		t, admissionv1.Create, "operator", []string{"orka-admins"}, adjudication, nil, "",
	))
	require.True(t, response.Allowed, response.Result.Message)

	mismatchedRequester := adjudication.DeepCopy()
	mismatchedRequester.Spec.RequestedBy = "someone-else"
	response = validator.Handle(context.Background(), coexistenceRequestWithGroups(
		t, admissionv1.Create, "operator", []string{"orka-admins"}, mismatchedRequester, nil, "",
	))
	require.False(t, response.Allowed)
	require.Contains(t, response.Result.Message, "authenticated admission caller")
	for _, requestedBy := range []string{"", " operator "} {
		nonCanonical := adjudication.DeepCopy()
		nonCanonical.Spec.RequestedBy = requestedBy
		response = validator.Handle(context.Background(), coexistenceRequestWithGroups(
			t, admissionv1.Create, "operator", []string{"orka-admins"}, nonCanonical, nil, "",
		))
		require.False(t, response.Allowed)
		require.Contains(t, response.Result.Message, "nonempty authenticated admission caller")
	}

	specEdit := adjudication.DeepCopy()
	specEdit.Spec.Action = corev1alpha1.AgentExecutionAdjudicationCleanupV1
	response = validator.Handle(context.Background(), coexistenceRequestWithGroups(
		t, admissionv1.Update, "operator", []string{"orka-admins"}, specEdit, adjudication, "",
	))
	require.False(t, response.Allowed)
	require.Contains(t, response.Result.Message, "spec is immutable")

	response = validator.Handle(context.Background(), coexistenceRequestWithGroups(
		t, admissionv1.Update, coexistenceControllerUsername, nil, adjudication, adjudication, "status",
	))
	require.True(t, response.Allowed, response.Result.Message)
	response = validator.Handle(context.Background(), coexistenceRequestWithGroups(
		t, admissionv1.Update, "operator", []string{"orka-admins"}, adjudication, adjudication, "status",
	))
	require.False(t, response.Allowed)
	require.Contains(t, response.Result.Message, "only the adjudication controller")

	response = validator.Handle(context.Background(), coexistenceRequestWithGroups(
		t, admissionv1.Delete, "operator", []string{"orka-admins"}, nil, adjudication, "",
	))
	require.False(t, response.Allowed)
	require.Contains(t, response.Result.Message, "must be retained")
	terminalAdjudication := adjudication.DeepCopy()
	terminalAdjudication.Status.State = corev1alpha1.AgentExecutionAdjudicationSuperseded
	response = validator.Handle(context.Background(), coexistenceRequestWithGroups(
		t, admissionv1.Delete, "operator", []string{"orka-admins"}, nil, terminalAdjudication, "",
	))
	require.True(t, response.Allowed, response.Result.Message)
	response = validator.Handle(context.Background(), coexistenceRequestWithGroups(
		t, admissionv1.Delete, "system:serviceaccount:kube-system:namespace-controller", nil, nil, adjudication, "",
	))
	require.False(t, response.Allowed)
	require.Contains(t, response.Result.Message, "must be retained")
	orphanedValidator := *validator
	orphanedValidator.reader = fake.NewClientBuilder().WithScheme(scheme).Build()
	response = orphanedValidator.Handle(context.Background(), coexistenceRequestWithGroups(
		t, admissionv1.Delete, "system:serviceaccount:kube-system:namespace-controller", nil, nil, adjudication, "",
	))
	require.True(t, response.Allowed, response.Result.Message)
	response = validator.Handle(context.Background(), coexistenceRequestWithGroups(
		t, admissionv1.Delete, "tenant", nil, nil, adjudication, "",
	))
	require.False(t, response.Allowed)
}

func TestTaskExecutionAuthorityAllowsOnlyControlLessLegacyCleanupBindings(t *testing.T) {
	t.Parallel()
	scheme := coexistenceTestScheme(t)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: admissionTestNamespace, UID: types.UID("namespace-uid"),
	}}
	control := &corev1alpha1.AgentExecutionControl{
		ObjectMeta: metav1.ObjectMeta{
			Name: corev1alpha1.AgentExecutionControlName, Namespace: corev1alpha1.AgentExecutionControlNamespace,
			UID: types.UID("control-uid"), Generation: 3,
		},
		Status: corev1alpha1.AgentExecutionControlStatus{
			ObservedGeneration: 3,
			Backends: &corev1alpha1.AgentExecutionBackendsStatus{
				V1: corev1alpha1.AgentExecutionBackendStatus{EffectiveMode: corev1alpha1.AgentExecutionEffectiveModeEnabled, ModeRevision: 2},
				V2: corev1alpha1.AgentExecutionBackendStatus{EffectiveMode: corev1alpha1.AgentExecutionEffectiveModeEnabled, ModeRevision: 7},
			},
		},
	}
	validator := &TaskExecutionAuthorityValidator{
		decoder: ctrladmission.NewDecoder(scheme),
		reader:  fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace).Build(),
		config:  CoexistenceConfig{ControllerUsername: coexistenceControllerUsername}.normalized(),
	}

	for _, deleting := range []bool{false, true} {
		name := "active"
		if deleting {
			name = "deleting"
		}
		t.Run(name, func(t *testing.T) {
			oldTask := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
				Name: admissionTestTaskName, Namespace: admissionTestNamespace,
				UID: types.UID("task-uid"), Generation: 2,
			}}
			if deleting {
				deletedAt := metav1.Now()
				oldTask.DeletionTimestamp = &deletedAt
			}
			newTask := oldTask.DeepCopy()
			binding := coexistenceTestBinding(namespace.UID, control)
			binding.Mode = corev1alpha1.AgentExecutionBindingModeCleanupOnly
			binding.Provenance = corev1alpha1.AgentExecutionProvenanceLegacyCleanupOnly
			binding.BackendControl = nil
			newTask.Status.AgentExecutionBinding = binding
			response := validator.Handle(context.Background(), coexistenceRequest(
				t, admissionv1.Update, coexistenceControllerUsername, newTask, oldTask, "status",
			))
			require.True(t, response.Allowed, response.Result.Message)
		})
	}

	oldTask := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name: admissionTestTaskName, Namespace: admissionTestNamespace,
		UID: types.UID("task-uid"), Generation: 2,
	}}
	for _, provenance := range []corev1alpha1.AgentExecutionBindingProvenance{
		corev1alpha1.AgentExecutionProvenanceNewlyBound,
		corev1alpha1.AgentExecutionProvenanceLegacyAdopted,
	} {
		newTask := oldTask.DeepCopy()
		binding := coexistenceTestBinding(namespace.UID, control)
		binding.Provenance = provenance
		binding.BackendControl = nil
		newTask.Status.AgentExecutionBinding = binding
		response := validator.Handle(context.Background(), coexistenceRequest(
			t, admissionv1.Update, coexistenceControllerUsername, newTask, oldTask, "status",
		))
		require.False(t, response.Allowed)
		require.Contains(t, response.Result.Message, "live backend-control revision")
	}

	deletingTask := oldTask.DeepCopy()
	deletedAt := metav1.Now()
	deletingTask.DeletionTimestamp = &deletedAt
	newDeletingTask := deletingTask.DeepCopy()
	newDeletingTask.Status.AgentExecutionBinding = coexistenceTestBinding(namespace.UID, control)
	response := validator.Handle(context.Background(), coexistenceRequest(
		t, admissionv1.Update, coexistenceControllerUsername, newDeletingTask, deletingTask, "status",
	))
	require.False(t, response.Allowed)
	require.Contains(t, response.Result.Message, "deleting Task")
}

func coexistenceTestAgent(contract *corev1alpha1.AgentRuntimeContractVersion) *corev1alpha1.Agent {
	return &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: admissionTestNamespace},
		Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{
			Type:            corev1alpha1.AgentRuntimeCodex,
			ContractVersion: contract,
		}},
	}
}

func coexistenceTestBinding(
	namespaceUID types.UID,
	control *corev1alpha1.AgentExecutionControl,
) *corev1alpha1.AgentExecutionBinding {
	digest := "sha256:" + strings.Repeat("a", 64)
	return &corev1alpha1.AgentExecutionBinding{
		SchemaVersion:   1,
		Mode:            corev1alpha1.AgentExecutionBindingModeExecute,
		ContractVersion: corev1alpha1.AgentRuntimeContractHarnessV2,
		Backend:         corev1alpha1.AgentExecutionBackendRuntimePool,
		Provenance:      corev1alpha1.AgentExecutionProvenanceNewlyBound,
		BindingDigest:   digest,
		Task: corev1alpha1.AgentExecutionBindingTaskRef{
			NamespaceUID:        namespaceUID,
			UID:                 types.UID("task-uid"),
			BoundSpecGeneration: 2,
		},
		BackendControl: &corev1alpha1.AgentExecutionBackendControlRef{
			Name:         control.Name,
			UID:          control.UID,
			Generation:   control.Generation,
			ModeRevision: control.Status.Backends.V2.ModeRevision,
			AdmittedMode: corev1alpha1.AgentExecutionEffectiveModeEnabled,
		},
		Snapshot: corev1alpha1.AgentExecutionSnapshotRef{
			ID:            "task-uid/" + digest,
			Digest:        digest,
			SchemaVersion: 1,
		},
		BoundAt: metav1.Now(),
	}
}

func coexistenceTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	return scheme
}

func coexistenceRequest(
	t *testing.T,
	operation admissionv1.Operation,
	username string,
	object runtime.Object,
	oldObject runtime.Object,
	subresource string,
) ctrladmission.Request {
	return coexistenceRequestWithGroups(t, operation, username, nil, object, oldObject, subresource)
}

func coexistenceRequestWithGroups(
	t *testing.T,
	operation admissionv1.Operation,
	username string,
	groups []string,
	object runtime.Object,
	oldObject runtime.Object,
	subresource string,
) ctrladmission.Request {
	t.Helper()
	reference := object
	if reference == nil {
		reference = oldObject
	}
	kind := coexistenceObjectKind(t, reference)
	request := ctrladmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation:   operation,
		Namespace:   admissionTestNamespace,
		SubResource: subresource,
		Kind: metav1.GroupVersionKind{
			Group: corev1alpha1.GroupVersion.Group, Version: corev1alpha1.GroupVersion.Version, Kind: kind,
		},
		UserInfo: authenticationv1.UserInfo{
			Username: username,
			Groups:   groups,
		},
	}}
	if object != nil {
		request.Object = runtime.RawExtension{Raw: marshalCoexistenceObject(t, object)}
	}
	if oldObject != nil {
		request.OldObject = runtime.RawExtension{Raw: marshalCoexistenceObject(t, oldObject)}
	}
	return request
}

func coexistenceObjectKind(t *testing.T, object runtime.Object) string {
	t.Helper()
	switch object.(type) {
	case *corev1alpha1.Agent:
		return "Agent"
	case *corev1alpha1.AgentRuntime:
		return "AgentRuntime"
	case *corev1alpha1.Task:
		return "Task"
	case *corev1alpha1.AgentExecutionAdjudication:
		return "AgentExecutionAdjudication"
	case *corev1alpha1.AgentExecutionControl:
		return kindAgentExecutionControl
	case *corev1alpha1.AgentExecutionPolicy:
		return kindAgentExecutionPolicy
	case *corev1alpha1.RuntimeSessionControl:
		return "RuntimeSessionControl"
	default:
		t.Fatalf("unsupported coexistence admission object %T", object)
		return ""
	}
}

func marshalCoexistenceObject(t *testing.T, object runtime.Object) []byte {
	t.Helper()
	copy := object.DeepCopyObject()
	switch value := copy.(type) {
	case *corev1alpha1.Agent:
		value.TypeMeta = metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Agent"}
	case *corev1alpha1.AgentRuntime:
		value.TypeMeta = metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "AgentRuntime"}
	case *corev1alpha1.Task:
		value.TypeMeta = metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task"}
	case *corev1alpha1.AgentExecutionAdjudication:
		value.TypeMeta = metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "AgentExecutionAdjudication"}
	case *corev1alpha1.AgentExecutionControl:
		value.TypeMeta = metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: kindAgentExecutionControl}
	case *corev1alpha1.AgentExecutionPolicy:
		value.TypeMeta = metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: kindAgentExecutionPolicy}
	case *corev1alpha1.RuntimeSessionControl:
		value.TypeMeta = metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RuntimeSessionControl"}
	default:
		t.Fatalf("unsupported coexistence admission object %T", object)
	}
	data, err := json.Marshal(copy)
	require.NoError(t, err)
	return data
}
