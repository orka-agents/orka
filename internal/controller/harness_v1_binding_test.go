package controller

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

type harnessV1CandidateErrorReader struct {
	client.Reader
	get func(client.ObjectKey, client.Object) error
}

func (r *harnessV1CandidateErrorReader) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	options ...client.GetOption,
) error {
	if r.get != nil {
		if err := r.get(key, object); err != nil {
			return err
		}
	}
	return r.Reader.Get(ctx, key, object, options...)
}

func TestEnsureHarnessV1ExecutionBindingRequeuesTransientCandidateErrors(t *testing.T) {
	tests := []struct {
		name      string
		intercept func(client.Object) bool
	}{
		{
			name: "compatibility policy API read",
			intercept: func(object client.Object) bool {
				_, ok := object.(*corev1alpha1.AgentExecutionPolicy)
				return ok
			},
		},
		{
			name: "wrapper auth Secret API read",
			intercept: func(object client.Object) bool {
				_, ok := object.(*corev1.Secret)
				return ok
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			fixture := newHarnessV1CandidateFixture(t, ctx)
			transientErr := errors.New("temporary Kubernetes API outage")
			fixture.reconciler.APIReader = &harnessV1CandidateErrorReader{
				Reader: fixture.reconciler.Client,
				get: func(_ client.ObjectKey, object client.Object) error {
					if test.intercept(object) {
						return transientErr
					}
					return nil
				},
			}

			result, err, handled := fixture.reconciler.ensureHarnessV1ExecutionBinding(
				ctx, fixture.task.DeepCopy(), fixture.agent,
			)
			if err != nil || !handled || result.RequeueAfter != 5*time.Second {
				t.Fatalf("ensure binding = result=%#v handled=%v err=%v, want five-second requeue", result, handled, err)
			}
			current := &corev1alpha1.Task{}
			if err := fixture.reconciler.Get(ctx, client.ObjectKeyFromObject(fixture.task), current); err != nil {
				t.Fatal(err)
			}
			if current.Status.Phase == corev1alpha1.TaskPhaseFailed || current.Status.AgentExecutionBinding != nil {
				t.Fatalf("transient resolution error terminalized or bound Task: %#v", current.Status)
			}
		})
	}
}

func TestEnsureHarnessV1ExecutionBindingFailsPermanentPolicyViolation(t *testing.T) {
	ctx := context.Background()
	fixture := newHarnessV1CandidateFixture(t, ctx)
	policy := &corev1alpha1.AgentExecutionPolicy{}
	if err := fixture.reconciler.Get(ctx, client.ObjectKeyFromObject(fixture.policy), policy); err != nil {
		t.Fatal(err)
	}
	policy.Spec.AllowNewV1Bindings = false
	if err := fixture.reconciler.Update(ctx, policy); err != nil {
		t.Fatal(err)
	}

	_, err, handled := fixture.reconciler.ensureHarnessV1ExecutionBinding(
		ctx, fixture.task.DeepCopy(), fixture.agent,
	)
	if err != nil || !handled {
		t.Fatalf("ensure binding = handled=%v err=%v, want permanent failure", handled, err)
	}
	current := &corev1alpha1.Task{}
	if err := fixture.reconciler.Get(ctx, client.ObjectKeyFromObject(fixture.task), current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != corev1alpha1.TaskPhaseFailed ||
		!strings.Contains(current.Status.Message, "does not authorize new bindings") {
		t.Fatalf("permanent policy violation status = %#v", current.Status)
	}
}

func TestResolveHarnessV1ExecutionCandidateMarksSpecViolationPermanent(t *testing.T) {
	ctx := context.Background()
	fixture := newHarnessV1CandidateFixture(t, ctx)
	task := fixture.task.DeepCopy()
	task.Spec.Workspace = &corev1alpha1.WorkspaceConfig{}

	_, err := fixture.reconciler.resolveHarnessV1ExecutionCandidate(ctx, task, fixture.agent)
	if err == nil || !isPermanentHarnessV1CandidateError(err) {
		t.Fatalf("workspace violation error = %v, permanent=%v", err, isPermanentHarnessV1CandidateError(err))
	}
}

func TestResolveHarnessV1ExecutionCandidateRejectsCopilotPermanently(t *testing.T) {
	ctx := context.Background()
	fixture := newHarnessV1CandidateFixture(t, ctx)
	policy := &corev1alpha1.AgentExecutionPolicy{}
	if err := fixture.reconciler.Get(ctx, client.ObjectKeyFromObject(fixture.policy), policy); err != nil {
		t.Fatal(err)
	}
	policy.Spec.AllowedBuiltInRuntimeTypes = append(
		policy.Spec.AllowedBuiltInRuntimeTypes,
		corev1alpha1.AgentRuntimeCopilot,
	)
	if err := fixture.reconciler.Update(ctx, policy); err != nil {
		t.Fatal(err)
	}
	agent := fixture.agent.DeepCopy()
	agent.Spec.Runtime.Type = corev1alpha1.AgentRuntimeCopilot

	candidate, err := fixture.reconciler.resolveHarnessV1ExecutionCandidate(ctx, fixture.task, agent)
	if candidate != nil || err == nil || !isPermanentHarnessV1CandidateError(err) ||
		!strings.Contains(err.Error(), "GitHub mutation-capable credential") {
		t.Fatalf("Copilot candidate = %#v, error = %v, want permanent safe-credential rejection", candidate, err)
	}
}

func TestResolveHarnessV1TargetRequiresObservedMode(t *testing.T) {
	const (
		endpoint   = "https://runtime.example.invalid"
		runtimeKey = "token"
	)
	tests := []struct {
		name         string
		capabilities *corev1alpha1.AgentRuntimeObservedCapabilities
		wantError    bool
	}{
		{name: "missing capabilities", wantError: true},
		{
			name: "brokered only",
			capabilities: &corev1alpha1.AgentRuntimeObservedCapabilities{
				ToolExecutionModes: []corev1alpha1.AgentRuntimeToolExecutionMode{
					corev1alpha1.AgentRuntimeToolExecutionModeBrokered,
				},
			},
			wantError: true,
		},
		{
			name: "observed only",
			capabilities: &corev1alpha1.AgentRuntimeObservedCapabilities{
				ToolExecutionModes: []corev1alpha1.AgentRuntimeToolExecutionMode{
					corev1alpha1.AgentRuntimeToolExecutionModeObserved,
				},
			},
		},
		{
			name: "observed and brokered",
			capabilities: &corev1alpha1.AgentRuntimeObservedCapabilities{
				ToolExecutionModes: []corev1alpha1.AgentRuntimeToolExecutionMode{
					corev1alpha1.AgentRuntimeToolExecutionModeObserved,
					corev1alpha1.AgentRuntimeToolExecutionModeBrokered,
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtime := &corev1alpha1.AgentRuntime{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default", Name: "external-v1", UID: types.UID("external-v1-uid"), Generation: 1,
				},
				Spec: corev1alpha1.AgentRuntimeRegistrySpec{
					ContractVersion: ptr.To(corev1alpha1.AgentRuntimeContractHarnessV1),
					Deployment: corev1alpha1.AgentRuntimeDeploymentSpec{
						Mode: corev1alpha1.AgentRuntimeDeploymentModeExternalEndpoint, Endpoint: endpoint,
					},
					ClientAuth: corev1alpha1.AgentRuntimeClientAuth{
						BearerAuthRef: &corev1alpha1.AgentRuntimeBearerAuthReference{Name: "external-v1-auth", Key: runtimeKey},
					},
				},
				Status: corev1alpha1.AgentRuntimeStatus{
					Ready: true, ObservedGeneration: 1, ObservedAuthRefResourceVersion: "1",
					ObservedCapabilities: test.capabilities,
				},
			}
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: runtime.Namespace, Name: runtime.Spec.ClientAuth.BearerAuthRef.Name,
					UID: types.UID("external-v1-auth-uid"), ResourceVersion: "1",
					Labels: map[string]string{
						agentRuntimeAuthUseLabel: scheduledRunLabelValue, agentRuntimeAuthRefNameLabel: runtime.Name,
					},
					Annotations: map[string]string{agentRuntimeAuthEndpointAnnotation: endpoint},
				},
				Data: map[string][]byte{runtimeKey: []byte("runtime-auth-value")},
			}
			reconciler, _ := newBindingTestReconciler(t, runtime, secret)
			task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: runtime.Namespace}}
			agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{
				RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: runtime.Name},
			}}}

			target, err := reconciler.resolveHarnessV1Target(t.Context(), reconciler.Client, task, agent)
			if test.wantError {
				if err == nil || !strings.Contains(err.Error(), "required observed tool execution mode") {
					t.Fatalf("resolve target = %#v, error = %v, want observed-mode rejection", target, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if target.runtimeRef == nil || target.runtimeRef.Name != runtime.Name ||
				target.backend != corev1alpha1.AgentExecutionBackendExternalEndpoint {
				t.Fatalf("resolved target = %#v, want external AgentRuntime %q", target, runtime.Name)
			}
		})
	}
}

func TestResolveHarnessV1ExecutionCandidateFreezesRuntimeAuthOnly(t *testing.T) {
	ctx := context.Background()
	fixture := newHarnessV1CandidateFixture(t, ctx)
	credential := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: fixture.task.Namespace, Name: "runtime-auth-credentials",
			UID: types.UID("runtime-auth-credentials-uid"),
		},
		Data: map[string][]byte{"OPENAI_API_KEY": []byte("runtime-auth-secret-value")},
	}
	if err := fixture.reconciler.Create(ctx, credential); err != nil {
		t.Fatal(err)
	}
	fixture.agent.Spec.SecretRef = &corev1.LocalObjectReference{Name: credential.Name}
	task := fixture.task.DeepCopy()
	task.Annotations = map[string]string{labels.AnnotationAgentRuntimeAuthOnly: scheduledRunLabelValue}

	candidate, err := fixture.reconciler.resolveHarnessV1ExecutionCandidate(ctx, task, fixture.agent)
	if err != nil {
		t.Fatal(err)
	}
	var body agentExecutionSnapshotBody
	if err := json.Unmarshal(candidate.snapshotBody, &body); err != nil {
		t.Fatal(err)
	}
	if body.HarnessV1 == nil || !body.HarnessV1.RuntimeAuthOnly {
		t.Fatalf("frozen harness v1 metadata = %#v, want runtimeAuthOnly", body.HarnessV1)
	}
	if strings.Contains(string(candidate.snapshotBody), "runtime-auth-secret-value") {
		t.Fatal("encrypted snapshot plaintext body retained a raw provider credential")
	}

	unprotectedTask := task.DeepCopy()
	delete(unprotectedTask.Annotations, labels.AnnotationAgentRuntimeAuthOnly)
	unprotected, err := fixture.reconciler.resolveHarnessV1ExecutionCandidate(ctx, unprotectedTask, fixture.agent)
	if err != nil {
		t.Fatal(err)
	}
	if unprotected.binding.Snapshot.Digest == candidate.binding.Snapshot.Digest {
		t.Fatal("runtime-auth-only annotation did not change the immutable snapshot digest")
	}
}

func TestResolveHarnessV1CredentialRefsAllowlistsRuntimeProviderKeys(t *testing.T) {
	ctx := context.Background()
	fixture := newHarnessV1CandidateFixture(t, ctx)
	tests := []struct {
		name        string
		runtimeType corev1alpha1.AgentRuntimeType
		data        map[string][]byte
		wantKeys    []string
	}{
		{
			name: "codex", runtimeType: corev1alpha1.AgentRuntimeCodex,
			data: map[string][]byte{
				"OPENAI_API_KEY":  []byte("codex-provider-key"),
				"OPENAI_BASE_URL": []byte("https://provider.example.invalid"),
			},
			wantKeys: []string{"OPENAI_API_KEY", "OPENAI_BASE_URL"},
		},
		{
			name: "claude", runtimeType: corev1alpha1.AgentRuntimeClaude,
			data: map[string][]byte{
				"ANTHROPIC_API_KEY":  []byte("claude-provider-key"),
				"ANTHROPIC_BASE_URL": []byte("https://provider.example.invalid"),
			},
			wantKeys: []string{"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: fixture.task.Namespace, Name: "provider-" + tt.name,
					UID: types.UID("provider-" + tt.name + "-uid"),
				},
				Data: tt.data,
			}
			if err := fixture.reconciler.Create(ctx, secret); err != nil {
				t.Fatal(err)
			}
			agent := fixture.agent.DeepCopy()
			agent.Spec.Runtime.Type = tt.runtimeType
			agent.Spec.SecretRef = &corev1.LocalObjectReference{Name: secret.Name}
			refs, err := resolveHarnessV1CredentialRefs(
				ctx, fixture.reconciler.Client, agent, resolvedHarnessV1Target{},
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(refs) != 1 || !slices.Equal(refs[0].Keys, tt.wantKeys) {
				t.Fatalf("credential refs = %#v, want keys %v", refs, tt.wantKeys)
			}
		})
	}
}

func TestResolveHarnessV1CredentialRefsRejectsUnrelatedKeys(t *testing.T) {
	ctx := context.Background()
	fixture := newHarnessV1CandidateFixture(t, ctx)
	for _, prohibitedKey := range []string{"GITHUB_TOKEN", "GIT_TOKEN", "KONTXT_TXN_TOKEN"} {
		t.Run(prohibitedKey, func(t *testing.T) {
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: fixture.task.Namespace, Name: "provider-" + strings.ToLower(prohibitedKey),
					UID: types.UID("provider-" + strings.ToLower(prohibitedKey) + "-uid"),
				},
				Data: map[string][]byte{
					"OPENAI_API_KEY": []byte("provider-key"),
					prohibitedKey:    []byte("unrelated-sensitive-value"),
				},
			}
			if err := fixture.reconciler.Create(ctx, secret); err != nil {
				t.Fatal(err)
			}
			agent := fixture.agent.DeepCopy()
			agent.Spec.SecretRef = &corev1.LocalObjectReference{Name: secret.Name}
			_, err := resolveHarnessV1CredentialRefs(
				ctx, fixture.reconciler.Client, agent, resolvedHarnessV1Target{},
			)
			if err == nil || !isPermanentHarnessV1CandidateError(err) || !strings.Contains(err.Error(), prohibitedKey) {
				t.Fatalf("unrelated credential key error = %v, want permanent rejection mentioning %s", err, prohibitedKey)
			}
		})
	}
}

func TestHarnessV1SessionLineageDigestExcludesTurnSnapshot(t *testing.T) {
	binding := &corev1alpha1.AgentExecutionBinding{
		ContractVersion: corev1alpha1.AgentRuntimeContractHarnessV1,
		Backend:         corev1alpha1.AgentExecutionBackendHarnessWrapper,
		RuntimeType:     corev1alpha1.AgentRuntimeCodex,
		Agent: &corev1alpha1.AgentExecutionAgentRef{
			Namespace: "default", Name: "agent", UID: types.UID("agent-uid"), Generation: 1,
		},
		Policy: &corev1alpha1.AgentExecutionPolicyRef{
			Name: "compatibility", UID: types.UID("policy-uid"), Generation: 1,
			Digest: "sha256:" + strings.Repeat("a", 64),
		},
		Snapshot: corev1alpha1.AgentExecutionSnapshotRef{Digest: "sha256:" + strings.Repeat("b", 64)},
	}
	first, err := agentExecutionLineageConfigDigest(binding)
	if err != nil {
		t.Fatal(err)
	}
	secondTurn := *binding
	secondTurn.Snapshot.Digest = "sha256:" + strings.Repeat("c", 64)
	second, err := agentExecutionLineageConfigDigest(&secondTurn)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("turn-specific snapshots changed v1 Session lineage digest: %s != %s", first, second)
	}
	otherRuntime := secondTurn
	otherRuntime.RuntimeType = corev1alpha1.AgentRuntimeClaude
	third, err := agentExecutionLineageConfigDigest(&otherRuntime)
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Fatal("runtime identity change did not change v1 Session lineage digest")
	}
}

func TestValidateHarnessV1RuntimeAuthOnlyRejectsUnsupportedRoute(t *testing.T) {
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Annotations: map[string]string{labels.AnnotationAgentRuntimeAuthOnly: scheduledRunLabelValue},
	}}
	tests := []struct {
		name       string
		runtime    corev1alpha1.AgentRuntimeType
		target     resolvedHarnessV1Target
		wantReason string
	}{
		{
			name: "external endpoint", runtime: corev1alpha1.AgentRuntimeCodex,
			target: resolvedHarnessV1Target{
				backend:    corev1alpha1.AgentExecutionBackendExternalEndpoint,
				runtimeRef: &corev1alpha1.AgentRuntime{},
			},
			wantReason: "built-in wrapper",
		},
		{
			name: "unsupported built-in runtime", runtime: corev1alpha1.AgentRuntimeCopilot,
			target:     resolvedHarnessV1Target{backend: corev1alpha1.AgentExecutionBackendHarnessWrapper},
			wantReason: "does not support runtime",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			agent := &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{
				Runtime: &corev1alpha1.AgentCLIRuntime{Type: test.runtime},
			}}
			_, err := validateHarnessV1RuntimeAuthOnly(task, agent, test.target)
			if err == nil || !isPermanentHarnessV1CandidateError(err) || !strings.Contains(err.Error(), test.wantReason) {
				t.Fatalf("validate runtime-auth-only route error = %v, want permanent %q failure", err, test.wantReason)
			}
		})
	}
}

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

type harnessV1CandidateFixture struct {
	reconciler *TaskReconciler
	task       *corev1alpha1.Task
	agent      *corev1alpha1.Agent
	policy     *corev1alpha1.AgentExecutionPolicy
}

func newHarnessV1CandidateFixture(
	t *testing.T,
	ctx context.Context,
) *harnessV1CandidateFixture {
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
	return &harnessV1CandidateFixture{
		reconciler: reconciler,
		task:       task,
		agent:      agent,
		policy:     policy,
	}
}

func newHarnessV1RecoveryBindingFixture(
	t *testing.T,
	ctx context.Context,
) (*TaskReconciler, *corev1alpha1.Task) {
	t.Helper()
	fixture := newHarnessV1CandidateFixture(t, ctx)
	if result, err, handled := fixture.reconciler.ensureHarnessV1ExecutionBinding(
		ctx, fixture.task.DeepCopy(), fixture.agent,
	); err != nil || handled {
		t.Fatalf("establish harness v1 recovery fixture binding: result=%#v handled=%v err=%v", result, handled, err)
	}
	bound := &corev1alpha1.Task{}
	if err := fixture.reconciler.Get(ctx, client.ObjectKeyFromObject(fixture.task), bound); err != nil {
		t.Fatal(err)
	}
	return fixture.reconciler, bound
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
