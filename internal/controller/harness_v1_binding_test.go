package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

func TestLoadVerifiedHarnessV1ExecutionForRecoveryControlRevision(t *testing.T) {
	tests := []struct {
		name          string
		mutateControl func(*corev1alpha1.AgentExecutionControl, *corev1alpha1.AgentExecutionBackendControlRef)
		wantError     bool
	}{
		{
			name: "same enabled generation and revision",
			mutateControl: func(
				_ *corev1alpha1.AgentExecutionControl,
				_ *corev1alpha1.AgentExecutionBackendControlRef,
			) {
			},
		},
		{
			name: "closing transition",
			mutateControl: func(
				control *corev1alpha1.AgentExecutionControl,
				ref *corev1alpha1.AgentExecutionBackendControlRef,
			) {
				control.Generation = ref.Generation + 1
				control.Spec.Backends.V1.DesiredMode = corev1alpha1.AgentExecutionModeDisabled
				control.Status.ObservedGeneration = ref.Generation
				control.Status.Backends.V1 = corev1alpha1.AgentExecutionBackendStatus{
					EffectiveMode: corev1alpha1.AgentExecutionEffectiveModeClosing,
					ModeRevision:  ref.ModeRevision + 1,
				}
			},
		},
		{
			name: "regressed mode revision",
			mutateControl: func(
				control *corev1alpha1.AgentExecutionControl,
				ref *corev1alpha1.AgentExecutionBackendControlRef,
			) {
				control.Status.Backends.V1.ModeRevision = ref.ModeRevision - 1
			},
			wantError: true,
		},
		{
			name: "observed generation below binding generation",
			mutateControl: func(
				control *corev1alpha1.AgentExecutionControl,
				ref *corev1alpha1.AgentExecutionBackendControlRef,
			) {
				control.Status.ObservedGeneration = ref.Generation - 1
			},
			wantError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			reconciler, task := newHarnessV1RecoveryBindingFixture(t, ctx)
			binding := task.Status.AgentExecutionBinding
			if binding == nil || binding.BackendControl == nil {
				t.Fatal("fixture did not persist a harness v1 backend control binding")
			}

			updateHarnessV1RecoveryControl(t, ctx, reconciler, func(control *corev1alpha1.AgentExecutionControl) {
				test.mutateControl(control, binding.BackendControl)
			})
			_, err := reconciler.loadVerifiedHarnessV1ExecutionForRecovery(
				ctx, task, binding, false,
			)
			if test.wantError && err == nil {
				t.Fatal("recovery verification unexpectedly accepted regressed backend control")
			}
			if !test.wantError && err != nil {
				t.Fatalf("recovery verification rejected admitted work: %v", err)
			}
		})
	}
}

func newHarnessV1RecoveryBindingFixture(
	t *testing.T,
	ctx context.Context,
) (*TaskReconciler, *corev1alpha1.Task) {
	t.Helper()
	const authSecretKey = "harness-auth"
	control := bindingTestControl()
	control.Generation = 3
	control.Spec.Backends.V1.DesiredMode = corev1alpha1.AgentExecutionModeEnabled
	control.Status.ObservedGeneration = control.Generation
	control.Status.Backends.V1 = corev1alpha1.AgentExecutionBackendStatus{
		EffectiveMode: corev1alpha1.AgentExecutionEffectiveModeEnabled,
		ModeRevision:  5,
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "harness-v1-recovery", UID: types.UID("harness-v1-recovery-task-uid"), Generation: 1,
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, Prompt: "continue admitted work",
			AgentRef: &corev1alpha1.AgentReference{Name: "harness-v1-agent"},
		},
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: task.Namespace, UID: types.UID("harness-v1-recovery-namespace-uid"),
	}}
	policy := &corev1alpha1.AgentExecutionPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: task.Namespace, Name: defaultHarnessV1PolicyName,
			UID: types.UID("harness-v1-recovery-policy-uid"), Generation: 1,
		},
		Spec: corev1alpha1.AgentExecutionPolicySpec{
			AllowNewV1Bindings:         true,
			AllowedBuiltInRuntimeTypes: []corev1alpha1.AgentRuntimeType{corev1alpha1.AgentRuntimeCodex},
			RetryEligibility:           corev1alpha1.AgentExecutionRetryNone,
			ProhibitedFields: []corev1alpha1.AgentExecutionProhibitedField{
				corev1alpha1.AgentExecutionProhibitWorkspaceCredentials,
				corev1alpha1.AgentExecutionProhibitForgeCredentials,
				corev1alpha1.AgentExecutionProhibitDirectPublication,
				corev1alpha1.AgentExecutionProhibitTransactionTokens,
			},
			NetworkIsolationProfile: corev1alpha1.AgentExecutionNetworkIsolationDefaultDeny,
		},
	}
	authSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: task.Namespace, Name: "harness-v1-auth",
			UID: types.UID("harness-v1-auth-secret-uid"), ResourceVersion: "1",
		},
		Data: map[string][]byte{authSecretKey: []byte(strings.Repeat("t", 32))},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: task.Namespace, Name: task.Spec.AgentRef.Name,
			UID: types.UID("harness-v1-recovery-agent-uid"), Generation: 1,
		},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{Name: "test-model"},
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type: corev1alpha1.AgentRuntimeCodex, ContractVersion: ptr.To(corev1alpha1.AgentRuntimeContractHarnessV1),
				DefaultAllowedTools: []string{}, DefaultAllowBash: new(false),
			},
		},
	}

	reconciler, durable := newBindingTestReconciler(t, control, task, namespace, policy, authSecret)
	configureAgentExecutionBindingTestGate(
		t, ctx, durable, control, store.AgentExecutionBackendV1,
	)
	reconciler.HarnessV1Endpoint = "http://harness-v1.default.svc:8080"
	reconciler.HarnessV1AuthSecretNamespace = authSecret.Namespace
	reconciler.HarnessV1AuthSecretName = authSecret.Name
	reconciler.HarnessV1AuthSecretKey = authSecretKey

	if result, err, handled := reconciler.ensureHarnessV1ExecutionBinding(ctx, task.DeepCopy(), agent); err != nil || handled {
		t.Fatalf("establish harness v1 recovery fixture binding: result=%#v handled=%v err=%v", result, handled, err)
	}
	bound := &corev1alpha1.Task{}
	if err := reconciler.Get(ctx, client.ObjectKeyFromObject(task), bound); err != nil {
		t.Fatal(err)
	}
	return reconciler, bound
}

func updateHarnessV1RecoveryControl(
	t *testing.T,
	ctx context.Context,
	reconciler *TaskReconciler,
	mutate func(*corev1alpha1.AgentExecutionControl),
) {
	t.Helper()
	current := &corev1alpha1.AgentExecutionControl{}
	key := client.ObjectKey{
		Namespace: corev1alpha1.AgentExecutionControlNamespace,
		Name:      corev1alpha1.AgentExecutionControlName,
	}
	if err := reconciler.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	desired := current.DeepCopy()
	mutate(desired)
	desiredStatus := desired.Status
	desired.Status = current.Status
	if err := reconciler.Update(ctx, desired); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Get(ctx, key, current); err != nil {
		t.Fatal(err)
	}
	current.Status = desiredStatus
	if err := reconciler.Status().Update(ctx, current); err != nil {
		t.Fatal(err)
	}
}
