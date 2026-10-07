package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	acpWorkspaceProviderProfileKind = "RuntimeWorkspaceProfile"
	acpWorkspaceProviderConfigKind  = "RuntimeProviderConfig"
	suspendTestRuntimePoolName      = "acp-ws-session-0123456789abcdef"
	suspendTestSessionUID           = "session-uid-1"
)

func acpTaskSessionNameTestIndex(object client.Object) []string {
	task, ok := object.(*corev1alpha1.Task)
	if !ok || task.Spec.SessionRef == nil {
		return nil
	}
	name := strings.TrimSpace(task.Spec.SessionRef.Name)
	if name == "" {
		return nil
	}
	return []string{name}
}
func acpAdapterTestClient(t *testing.T, objects ...client.Object) client.WithWatch {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testACPWorkspaceScheme(t)).
		WithIndex(&corev1alpha1.Task{}, acpTaskSessionNameField, acpTaskSessionNameTestIndex).
		WithStatusSubresource(
			&workspacev1alpha1.ExecutionWorkspace{},
			&workspacev1alpha1.ExecutionWorkspaceProvider{},
			&corev1alpha1.RuntimePool{},
		).
		WithObjects(objects...).
		Build()
}
func acpAdapterWorkspace(t *testing.T, poolName string) *workspacev1alpha1.ExecutionWorkspace {
	t.Helper()
	workspace := &workspacev1alpha1.ExecutionWorkspace{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: acpTestNamespace, Name: "acp-ws-test", UID: types.UID("acp-ws-test-uid"), Generation: 1,
			Labels:      map[string]string{workspacev1alpha1.ProviderControllerLabel: acpWorkspaceControllerLabelValue},
			Annotations: map[string]string{acpExecutionWorkspacePoolAnnotation: poolName},
		},
		Spec: workspacev1alpha1.ExecutionWorkspaceSpec{
			Mode: workspacev1alpha1.ExecutionWorkspaceModeInteractive,
			ClassBinding: workspacev1alpha1.ImmutableObjectBinding{
				Name: acpTestClassName, UID: types.UID("acp-class-uid"), Generation: 1,
				ProfileHash: "sha256:" + strings.Repeat("a", 64),
			},
			ProviderBinding: workspacev1alpha1.ImmutableObjectBinding{
				Name: acpTestProviderName, UID: types.UID("acp-provider-uid"), Generation: 1,
			},
			Slot:         defaultWorkspaceSlotName,
			DesiredState: workspacev1alpha1.ExecutionWorkspaceDesiredReady,
			Lifecycle: workspacev1alpha1.ExecutionWorkspaceLifecycle{
				DefaultOnDetach: workspacev1alpha1.WorkspaceOnDetachDelete,
				AllowedOnDetach: []workspacev1alpha1.WorkspaceOnDetach{workspacev1alpha1.WorkspaceOnDetachDelete},
				DetachTimeout:   metav1.Duration{Duration: 120000000000},
				DeletionPolicy: workspacev1alpha1.ExecutionWorkspaceDeletionPolicy{
					ProviderResources: workspacev1alpha1.WorkspaceDeletionActionDelete,
					PersistentVolumes: workspacev1alpha1.WorkspaceDeletionActionDelete,
					Checkpoints:       workspacev1alpha1.WorkspaceDeletionActionDelete,
				},
			},
		},
	}
	markWorkspaceAdmittedForPolicyReview(workspace, workspace.Generation)
	return workspace
}

func releaseTestACPEnforcedEpoch(t *testing.T, r *TaskReconciler, namespace, name string) {
	t.Helper()
	ctx := context.Background()
	released := &workspacev1alpha1.ExecutionWorkspace{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, released); err != nil ||
		released.Spec.Attachment != nil || released.Status.AttachedEpoch == 0 {
		return
	}
	base := released.DeepCopy()
	released.Status.AttachedEpoch = 0
	if err := r.Status().Patch(ctx, released, client.MergeFrom(base)); err != nil {
		t.Fatalf("release enforced epoch: %v", err)
	}
}

func suspendableSubstrateFixture(t *testing.T) *acpClassFixture {
	t.Helper()
	return newACPClassFixture(t, RuntimeProviderBackendSubstrate, func(f *acpClassFixture) {
		f.provider.Status.SupportedFeatures = append(
			f.provider.Status.SupportedFeatures,
			workspacev1alpha1.WorkspaceFeatureSuspend,
		)
		f.profile.Spec.Substrate.Suspend = &SubstrateSuspendPolicy{
			Mode: SubstrateSuspendModeDataOnly,
		}
		f.class.Spec.Lifecycle.DefaultOnDetach = workspacev1alpha1.WorkspaceOnDetachSuspend
		f.class.Spec.Lifecycle.AllowedOnDetach = []workspacev1alpha1.WorkspaceOnDetach{
			workspacev1alpha1.WorkspaceOnDetachSuspend, workspacev1alpha1.WorkspaceOnDetachDelete,
		}
	})
}
func suspendableSessionTask() *corev1alpha1.Task {
	agent := bindingTestAgent()
	return acpClassTestTask(func(task *corev1alpha1.Task) {
		task.Spec.Prompt = "resume the suspended session"
		task.Spec.AgentRef = &corev1alpha1.AgentReference{Name: agent.Name}
		task.Spec.Execution.Workspace.ReusePolicy = corev1alpha1.WorkspaceReusePolicySession
		task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: acpTestSessionName, Create: true}
	})
}
func bindSuspendableSessionTaskForSettlement(
	t *testing.T,
	r *TaskReconciler,
	task *corev1alpha1.Task,
) *corev1alpha1.Task {
	t.Helper()
	ctx := context.Background()
	r.ACPRuntimeEnabled = true
	r.ACPRuntimeNamespace = acpTestRuntimeNamespace
	r.ACPRuntimeImages = ACPRuntimeImages{
		Codex: "docker.io/example/codex@sha256:" + strings.Repeat("a", 64),
	}
	current := configureAgentExecutionBindingTest(t, ctx, r, task)
	candidate, err := r.resolveAgentExecutionCandidateWithWorkspaceSessionUID(
		ctx, current, bindingTestAgent(), suspendTestSessionUID,
	)
	if err != nil {
		t.Fatalf("resolve settlement binding: %v", err)
	}
	if err := r.persistAgentExecutionSnapshot(ctx, current, candidate); err != nil {
		t.Fatalf("persist settlement snapshot: %v", err)
	}
	if _, err := r.persistAgentExecutionBinding(ctx, current, candidate); err != nil {
		t.Fatalf("persist settlement binding: %v", err)
	}
	bound := &corev1alpha1.Task{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(current), bound); err != nil {
		t.Fatalf("reload settlement-bound task: %v", err)
	}
	return bound
}

func TestParseACPWorkspaceSettlementReceipt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		raw       string
		wantUID   string
		wantEpoch int64
		wantOK    bool
	}{
		{name: "legacy", raw: acpDispatcherTaskUID, wantUID: acpDispatcherTaskUID, wantOK: true},
		{name: "epoch-bound", raw: acpDispatcherTaskUID + " 7", wantUID: acpDispatcherTaskUID, wantEpoch: 7, wantOK: true},
		{name: "invalid epoch", raw: acpDispatcherTaskUID + " invalid"},
		{name: "negative epoch", raw: acpDispatcherTaskUID + " -1"},
		{name: "empty", raw: ""},
		{name: "extra field", raw: acpDispatcherTaskUID + " 7 extra"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uid, epoch, ok := parseACPWorkspaceSettlementReceipt(tt.raw)
			if uid != tt.wantUID || epoch != tt.wantEpoch || ok != tt.wantOK {
				t.Fatalf("parse %q = (%q, %d, %v), want (%q, %d, %v)",
					tt.raw, uid, epoch, ok, tt.wantUID, tt.wantEpoch, tt.wantOK)
			}
		})
	}
}
func runtimePoolTestPrivateAuthSecret(
	t *testing.T,
	r *RuntimePoolReconciler,
	pool *corev1alpha1.RuntimePool,
) corev1.Secret {
	t.Helper()
	var secrets corev1.SecretList
	if err := r.List(context.Background(), &secrets, client.InNamespace(pool.Namespace), client.MatchingLabels{
		runtimePoolAuthLabel: booleanTrueValue,
		runtimePoolUIDLabel:  string(pool.UID),
	}); err != nil {
		t.Fatalf("list private RuntimePool auth Secrets: %v", err)
	}
	if len(secrets.Items) != 1 {
		t.Fatalf("private RuntimePool auth Secret count = %d, want 1", len(secrets.Items))
	}
	return secrets.Items[0]
}
func substrateFixtureTemplateValidator(reader client.Reader) func(context.Context, *ExecutionWorkspaceRequest) error {
	return func(ctx context.Context, request *ExecutionWorkspaceRequest) error {
		if request == nil || request.TemplateName == "" {
			return nil
		}
		template := &unstructured.Unstructured{}
		template.SetGroupVersionKind(substrateActorTemplateGVK)
		if err := reader.Get(ctx, types.NamespacedName{Namespace: request.TemplateNamespace, Name: request.TemplateName}, template); err != nil {
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("substrate execution workspace ActorTemplate %q not found in namespace %q", request.TemplateName, request.TemplateNamespace)
			}
			return err
		}
		if template.GetLabels()["orka.ai/execution-workspace"] != "true" {
			return fmt.Errorf("substrate ActorTemplate %q in namespace %q missing label orka.ai/execution-workspace=true", request.TemplateName, request.TemplateNamespace)
		}
		if phase, _, _ := unstructured.NestedString(template.Object, "status", "phase"); phase != "Ready" {
			return fmt.Errorf("substrate ActorTemplate %q in namespace %q is not Ready: phase=%q", request.TemplateName, request.TemplateNamespace, phase)
		}
		return nil
	}
}
func readySubstrateActorTemplateForTest(env []any) *unstructured.Unstructured {
	daemonEnv := append([]any{substrateWorkspaceDaemonListenEnvForTest()}, env...)
	return readySubstrateActorTemplateWithContainersForTest([]any{
		map[string]any{
			"name":    "workspace",
			"command": []any{"/orka-workspace-agent"},
			"env":     daemonEnv,
		},
	})
}
func readySubstrateActorTemplateWithContainersForTest(containers []any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "ate.dev/v1alpha1",
		"kind":       "ActorTemplate",
		"metadata": map[string]any{
			"name":      "orka-codex",
			"namespace": "ate-demo",
			"labels": map[string]any{
				"orka.ai/execution-workspace": "true",
				"orka.ai/workspace-provider":  "substrate",
			},
			"annotations": map[string]any{
				"orka.ai/workspace-protocol":     "http-json-v1",
				"orka.ai/workspace-daemon-port":  "8080",
				"orka.ai/workspace-staging-root": "/app",
			},
		},
		"spec": map[string]any{
			"containers": containers,
		},
		"status": map[string]any{
			"phase": "Ready",
		},
	}}
}

var substrateActorTemplateGVK = schema.GroupVersionKind{Group: "ate.dev", Version: "v1alpha1", Kind: "ActorTemplate"}

func substrateWorkspaceDaemonListenEnvForTest() map[string]any {
	return map[string]any{
		"name":  "ORKA_WORKSPACE_AGENT_LISTEN_ADDR",
		"value": ":8080",
	}
}

// testExternalPoolWorkspaceSpec pins the generic logical pool request. Its
// physical workload is published separately after the pool receives its UID.
func testExternalPoolWorkspaceSpec(binding *ACPRuntimeWorkspaceBinding, name string, uid types.UID) *corev1alpha1.RuntimePoolExecutionWorkspaceSpec {
	return &corev1alpha1.RuntimePoolExecutionWorkspaceSpec{
		Provider: binding.Provider, BindingDigest: binding.BindingDigest,
		WorkspaceRef:  &workspacev1alpha1.ObjectIdentityReference{Name: name, UID: uid},
		ParametersRef: binding.Class.ParametersRef.DeepCopy(), ParametersBinding: binding.Class.ParametersBinding.DeepCopy(),
		RestoreFrom: acpWorkspaceWorkloadCheckpointReference(binding.RestoreFrom),
		Workload:    &corev1alpha1.RuntimePoolWorkspaceWorkloadSpec{ContractVersion: workspacev1alpha1.LifecycleContractV1, ProtocolVersion: corev1alpha1.RuntimePoolProtocolHarnessV2},
	}
}

func testAdmittedNativeDispatchWorkspace(t *testing.T, binding *ACPRuntimeWorkspaceBinding, pool *corev1alpha1.RuntimePool, endpoint string, bootstrapPort int32) *workspacev1alpha1.ExecutionWorkspace {
	t.Helper()
	class := binding.Class
	cb := workspacev1alpha1.ImmutableObjectBinding{Name: class.Name, UID: types.UID(class.UID), Generation: class.Generation, ProfileHash: class.ProfileHash}
	pb := workspacev1alpha1.ImmutableObjectBinding{Name: class.ProviderName, UID: types.UID(class.ProviderUID), Generation: class.ProviderGeneration}
	ref := pool.Spec.ExecutionWorkspace.WorkspaceRef
	w := &workspacev1alpha1.ExecutionWorkspace{
		ObjectMeta: metav1.ObjectMeta{Namespace: pool.Namespace, Name: ref.Name, UID: ref.UID, Generation: 1,
			Labels: map[string]string{workspacev1alpha1.ProviderControllerLabel: class.ControllerName}, Annotations: map[string]string{acpExecutionWorkspacePoolAnnotation: pool.Name}},
		Spec: workspacev1alpha1.ExecutionWorkspaceSpec{Mode: workspacev1alpha1.ExecutionWorkspaceModeInteractive, ClassBinding: cb, ProviderBinding: pb, DesiredState: workspacev1alpha1.ExecutionWorkspaceDesiredReady,
			CoreAdmission: &workspacev1alpha1.ExecutionWorkspaceCoreAdmission{ClassBinding: cb, ProviderBinding: pb, AdmittedGeneration: 1}},
		Status: workspacev1alpha1.ExecutionWorkspaceStatus{ObservedGeneration: 1, Conditions: []metav1.Condition{{Type: string(workspacev1alpha1.ConditionWorkspaceAdmitted), Status: metav1.ConditionTrue, Reason: "Ready", ObservedGeneration: 1}}},
	}
	request := &workspacev1alpha1.WorkloadRequest{
		Sequence: 1, Key: workspacev1alpha1.AllocationKey{Namespace: w.Namespace, Name: w.Name, WorkspaceUID: w.UID, ProviderUID: pb.UID}, Image: pool.Spec.Runtime.Image,
		ParametersRef: class.ParametersRef.DeepCopy(), ParametersBinding: class.ParametersBinding.DeepCopy(),
		Runtime: &workspacev1alpha1.RuntimeWorkload{BootstrapPort: bootstrapPort, PoolBinding: workspacev1alpha1.ImmutableObjectBinding{Name: pool.Name, UID: pool.UID, Generation: pool.Generation, ProfileHash: pool.Spec.Runtime.Profile.Digest}, ClassBinding: cb,
			RequiredFeatures: []workspacev1alpha1.ExecutionWorkspaceFeature{workspacev1alpha1.WorkspaceFeatureNativeProcess},
			Protocol:         harnessv2.ProtocolVersion, ContainerName: "runtime", Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Namespace: pool.Spec.RuntimeNamespace, Annotations: map[string]string{}}, Spec: corev1.PodSpec{AutomountServiceAccountToken: new(false), Containers: []corev1.Container{{Name: "runtime", Image: pool.Spec.Runtime.Image}}}}},
	}
	request.Runtime.Template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "ORKA_ACP_POD_UID", Value: string(externalNativeRuntimeUID(request))}}
	var err error
	request.Revision, err = workspacev1alpha1.WorkloadRevision(*request)
	if err != nil {
		t.Fatal(err)
	}
	identity := workspacev1alpha1.InstanceIdentity{AllocationID: "deadline-allocation", InstanceID: "pod-uid", RequestRevision: request.Revision}
	w.Spec.Workload = request
	w.Status.Allocation = &workspacev1alpha1.AllocationObservation{Sequence: request.Sequence, Key: request.Key, Identity: identity, State: workspacev1alpha1.AllocationReady,
		Startup: &workspacev1alpha1.StartupEvidence{ContractVersion: workspacev1alpha1.LifecycleContractV1, Identity: identity, Endpoint: endpoint,
			Process: &workspacev1alpha1.NativeProcessEvidence{Namespace: pool.Spec.RuntimeNamespace, Name: "runtime-pod", UID: "pod-uid", Version: 1, Worker: workspacev1alpha1.PodReference{Namespace: pool.Spec.RuntimeNamespace, Name: "deadline-worker", UID: "deadline-worker-uid"}, ChallengeSHA256: "sha256:" + strings.Repeat("a", 64)}}}
	if err := workspacev1alpha1.ValidateStartup(*request, *w.Status.Allocation); err != nil {
		t.Fatal(err)
	}
	return w
}
