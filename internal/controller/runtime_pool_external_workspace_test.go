package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	workspaceprovider "github.com/orka-agents/orka-workspace/sdk"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type externalRuntimePoolFixture struct {
	r          *RuntimePoolReconciler
	pool       *corev1alpha1.RuntimePool
	workspace  *workspacev1alpha1.ExecutionWorkspace
	provider   *workspacev1alpha1.ExecutionWorkspaceProvider
	supervisor *fakeRuntimePoolSupervisorClient
	seeds      int
}

func newExternalRuntimePoolFixture(t *testing.T) *externalRuntimePoolFixture {
	t.Helper()
	pool := runtimePoolTestObject(1)
	pool.Spec.Capacity = &corev1alpha1.RuntimePoolCapacitySpec{MaxResidentSessions: 1, MaxRunningPrompts: 1}
	classBinding := workspacev1alpha1.ImmutableObjectBinding{Name: "class", UID: "class-uid", Generation: 1, ProfileHash: "sha256:" + strings.Repeat("1", 64)}
	providerBinding := workspacev1alpha1.ImmutableObjectBinding{Name: "external-provider", UID: "provider-uid", Generation: 1}
	workspace := &workspacev1alpha1.ExecutionWorkspace{ObjectMeta: metav1.ObjectMeta{Namespace: pool.Namespace, Name: "external-workspace", UID: "workspace-uid", Generation: 1,
		Labels: map[string]string{workspacev1alpha1.ProviderControllerLabel: "example.workspace.orka.ai"}, Annotations: map[string]string{acpExecutionWorkspacePoolAnnotation: pool.Name}},
		Spec: workspacev1alpha1.ExecutionWorkspaceSpec{Mode: workspacev1alpha1.ExecutionWorkspaceModeInteractive, ClassBinding: classBinding, ProviderBinding: providerBinding,
			DesiredState: workspacev1alpha1.ExecutionWorkspaceDesiredReady, Lifecycle: workspacev1alpha1.ExecutionWorkspaceLifecycle{DefaultOnDetach: workspacev1alpha1.WorkspaceOnDetachDelete,
				AllowedOnDetach: []workspacev1alpha1.WorkspaceOnDetach{workspacev1alpha1.WorkspaceOnDetachDelete}, DeletionPolicy: workspacev1alpha1.ExecutionWorkspaceDeletionPolicy{
					ProviderResources: workspacev1alpha1.WorkspaceDeletionActionDelete, PersistentVolumes: workspacev1alpha1.WorkspaceDeletionActionDelete, Checkpoints: workspacev1alpha1.WorkspaceDeletionActionDelete}},
			CoreAdmission: &workspacev1alpha1.ExecutionWorkspaceCoreAdmission{ClassBinding: classBinding, ProviderBinding: providerBinding, AdmittedGeneration: 1}},
		Status: workspacev1alpha1.ExecutionWorkspaceStatus{ObservedGeneration: 1, Conditions: []metav1.Condition{{Type: string(workspacev1alpha1.ConditionWorkspaceAdmitted), Status: metav1.ConditionTrue, Reason: "Ready", ObservedGeneration: 1}}}}
	pool.Labels[acpExecutionWorkspaceLinkLabel] = workspace.Name
	pool.Annotations = map[string]string{acpExecutionWorkspaceUIDAnnotation: string(workspace.UID)}
	pool.Spec.ExecutionWorkspace = &corev1alpha1.RuntimePoolExecutionWorkspaceSpec{Provider: "example.workspace.orka.ai", BindingDigest: "sha256:" + strings.Repeat("2", 64),
		WorkspaceRef: &workspacev1alpha1.ObjectIdentityReference{Name: workspace.Name, UID: workspace.UID}, ParametersRef: &workspacev1alpha1.TypedObjectReference{Group: "example.workspace.orka.ai", Kind: "WorkspaceProfile", Name: "profile"},
		ParametersBinding: &workspacev1alpha1.ImmutableObjectBinding{Name: "profile", UID: "profile-uid", Generation: 1, ProfileHash: "sha256:" + strings.Repeat("3", 64)},
		Workload:          &corev1alpha1.RuntimePoolWorkspaceWorkloadSpec{ContractVersion: workspacev1alpha1.LifecycleContractV1, ProtocolVersion: corev1alpha1.RuntimePoolProtocolHarnessV2, RequiredFeatures: []workspacev1alpha1.ExecutionWorkspaceFeature{workspacev1alpha1.WorkspaceFeatureACPRuntime}}}
	provider := &workspacev1alpha1.ExecutionWorkspaceProvider{ObjectMeta: metav1.ObjectMeta{Name: providerBinding.Name, UID: providerBinding.UID, Generation: 1},
		Spec: workspacev1alpha1.ExecutionWorkspaceProviderSpec{ControllerName: string(pool.Spec.ExecutionWorkspace.Provider), LifecycleState: workspacev1alpha1.ExecutionWorkspaceProviderActive},
		Status: workspacev1alpha1.ExecutionWorkspaceProviderStatus{ObservedGeneration: 1, Adapter: &workspacev1alpha1.ExecutionWorkspaceAdapterStatus{Version: "v1"},
			SupportedContracts: []string{workspacev1alpha1.LifecycleContractV1}, SupportedFeatures: []workspacev1alpha1.ExecutionWorkspaceFeature{workspacev1alpha1.WorkspaceFeatureACPRuntime, workspacev1alpha1.WorkspaceFeatureSuspend},
			Conditions: []metav1.Condition{{Type: string(workspacev1alpha1.ConditionProviderHeartbeat), Status: metav1.ConditionTrue, ObservedGeneration: 1}, {Type: string(workspacev1alpha1.ConditionProviderCompatible), Status: metav1.ConditionTrue, ObservedGeneration: 1}}}}
	scheme := runtimePoolTestScheme(t)
	if err := workspacev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	supervisor := &fakeRuntimePoolSupervisorClient{}
	fixture := &externalRuntimePoolFixture{pool: pool, workspace: workspace, provider: provider, supervisor: supervisor}
	fixture.r = runtimePoolTestReconciler(t, scheme, supervisor, pool, workspace, provider)
	fixture.r.WorkspaceCredentialSeeder = func(context.Context, string, string, []byte, harnessv2.CredentialBootstrapRequest) (bool, error) {
		fixture.seeds++
		return false, nil
	}
	return fixture
}

func (f *externalRuntimePoolFixture) currentWorkspace(t *testing.T) *workspacev1alpha1.ExecutionWorkspace {
	t.Helper()
	current := &workspacev1alpha1.ExecutionWorkspace{}
	if err := f.r.Get(context.Background(), client.ObjectKeyFromObject(f.workspace), current); err != nil {
		t.Fatal(err)
	}
	return current
}

func (f *externalRuntimePoolFixture) publish(t *testing.T) *workspacev1alpha1.ExecutionWorkspace {
	t.Helper()
	for range 6 {
		runtimePoolReconcile(t, f.r, f.pool)
		current := f.currentWorkspace(t)
		if current.Spec.Workload != nil {
			return current
		}
	}
	t.Fatalf("no public workload request published: %#v", runtimePoolTestGetPool(t, f.r, f.pool).Status)
	return nil
}

func (f *externalRuntimePoolFixture) materialize(t *testing.T) (*workspacev1alpha1.ExecutionWorkspace, corev1.Pod) {
	t.Helper()
	current := f.publish(t)
	request := current.Spec.Workload
	pod := runtimePoolReadyPod(f.pool, request.Runtime.Template.Namespace, "external-runtime", "external-pod-uid", "10.0.0.71")
	pod.Labels = cloneStringMap(request.Runtime.Template.Labels)
	pod.Annotations = cloneStringMap(request.Runtime.Template.Annotations)
	pod.Spec = *request.Runtime.Template.Spec.DeepCopy()
	runtimePoolTestCreatePod(t, f.r, &pod)
	identity := workspacev1alpha1.InstanceIdentity{AllocationID: "allocation-uid", InstanceID: string(pod.UID), RequestRevision: request.Revision}
	current.Status.Allocation = &workspacev1alpha1.AllocationObservation{Sequence: request.Sequence, Key: request.Key, Identity: identity, State: workspacev1alpha1.AllocationReady,
		Startup: &workspacev1alpha1.StartupEvidence{ContractVersion: workspacev1alpha1.LifecycleContractV1, Identity: identity, Endpoint: "http://10.0.0.71:8080", Pod: &workspacev1alpha1.PodReference{Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID}}}
	current.Status.ObservedGeneration = current.Generation
	if err := f.r.Status().Update(context.Background(), current); err != nil {
		t.Fatal(err)
	}
	f.supervisor.probe = runtimePoolValidProbe(f.pool, &pod, "external-boot", false)
	return current, pod
}

func (f *externalRuntimePoolFixture) serve(t *testing.T) (*workspacev1alpha1.ExecutionWorkspace, corev1.Pod) {
	t.Helper()
	workspace, pod := f.materialize(t)
	runtimePoolReconcile(t, f.r, f.pool)
	status := runtimePoolTestGetPool(t, f.r, f.pool).Status
	if status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing || status.AdmissionState != corev1alpha1.RuntimePoolAdmissionAccepting {
		t.Fatalf("attested startup did not serve: %s/%s: %s", status.Lifecycle, status.AdmissionState, status.Message)
	}
	return workspace, pod
}

func TestExternalRuntimePoolPublishesPublicIntentAndServesAttestedInstance(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	workspace, pod := f.serve(t)
	if err := workspaceprovider.ValidateWorkspaceWorkload(workspace); err != nil {
		t.Fatal(err)
	}
	if f.seeds != 1 || f.supervisor.probeCalls != 1 {
		t.Fatalf("bootstrap/probe calls = %d/%d, want one each", f.seeds, f.supervisor.probeCalls)
	}
	if workspace.Spec.Workload.Key.WorkspaceUID != workspace.UID || workspace.Spec.Workload.Runtime.PoolBinding.UID != f.pool.UID {
		t.Fatal("public request lost its exact object bindings")
	}
	deployments := &appsv1.DeploymentList{}
	if err := f.r.List(context.Background(), deployments, client.InNamespace(f.pool.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(deployments.Items) != 0 {
		t.Fatal("core created a runtime Deployment for an external workload")
	}
	auth := runtimePoolTestPrivateAuthSecret(t, f.r, f.pool)
	serialized, err := json.Marshal(workspace.Spec.Workload)
	if err != nil {
		t.Fatal(err)
	}
	for _, container := range workspace.Spec.Workload.Runtime.Template.Spec.Containers {
		for _, env := range container.Env {
			if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
				t.Fatal("public workload refers to a private Secret")
			}
		}
	}
	for _, key := range []string{runtimePoolControllerTokenKey, runtimePoolCapabilitySecretKey, runtimePoolBootstrapSigningSeedKey} {
		if bytes.Contains(serialized, auth.Data[key]) {
			t.Fatal("private runtime credentials entered public workload")
		}
	}
	status := runtimePoolTestGetPool(t, f.r, f.pool).Status
	if status.ActiveInstance == nil || status.ActiveInstance.PodUID != string(pod.UID) || status.ActiveInstance.BootID != "external-boot" {
		t.Fatalf("active fence = %#v", status.ActiveInstance)
	}
}

func TestExternalRuntimePoolRejectsWrongUIDAndMutatedPodBeforeBootstrap(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*workspacev1alpha1.ExecutionWorkspace, *corev1.Pod)
	}{
		{name: "wrong Pod UID", mutate: func(w *workspacev1alpha1.ExecutionWorkspace, _ *corev1.Pod) {
			w.Status.Allocation.Startup.Pod.UID = "foreign"
		}},
		{name: "mutated image", mutate: func(_ *workspacev1alpha1.ExecutionWorkspace, p *corev1.Pod) {
			p.Spec.Containers[0].Image = "unapproved:latest"
		}},
		{name: "unexpected network policy label", mutate: func(_ *workspacev1alpha1.ExecutionWorkspace, p *corev1.Pod) {
			p.Labels["attacker.example/allow-egress"] = booleanTrueValue
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newExternalRuntimePoolFixture(t)
			workspace, pod := f.materialize(t)
			test.mutate(workspace, &pod)
			if pod.Labels["attacker.example/allow-egress"] == booleanTrueValue {
				policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: pod.Namespace, Name: "forged-label-egress"}, Spec: networkingv1.NetworkPolicySpec{
					PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"attacker.example/allow-egress": booleanTrueValue}},
					PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, Egress: []networkingv1.NetworkPolicyEgressRule{{}},
				}}
				if err := f.r.Create(context.Background(), policy); err != nil {
					t.Fatal(err)
				}
			}
			if err := f.r.Status().Update(context.Background(), workspace); err != nil {
				t.Fatal(err)
			}
			if err := f.r.Update(context.Background(), &pod); err != nil {
				t.Fatal(err)
			}
			runtimePoolReconcile(t, f.r, f.pool)
			status := runtimePoolTestGetPool(t, f.r, f.pool).Status
			if f.seeds != 0 || f.supervisor.probeCalls != 0 || status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed || status.ActiveInstance != nil {
				t.Fatalf("unattested Pod received admission: seeds=%d probes=%d status=%#v", f.seeds, f.supervisor.probeCalls, status)
			}
		})
	}
}

func TestExternalRuntimePoolAcceptsProviderBookkeepingLabelsWithinAdmittedNetwork(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	_, pod := f.materialize(t)
	pod.Labels["example.workspace.orka.ai/native-allocation"] = "allocation-uid"
	if err := f.r.Update(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	status := runtimePoolTestGetPool(t, f.r, f.pool).Status
	if f.seeds != 1 || f.supervisor.probeCalls != 1 || status.AdmissionState != corev1alpha1.RuntimePoolAdmissionAccepting || status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing {
		t.Fatalf("provider bookkeeping label prevented admitted startup: seeds=%d probes=%d status=%#v", f.seeds, f.supervisor.probeCalls, status)
	}
}

func TestExternalRuntimePoolProviderOutagePreservesFenceAndClosesAdmission(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	_, pod := f.serve(t)
	before := runtimePoolTestGetPool(t, f.r, f.pool).Status.ActiveInstance.DeepCopy()
	if err := f.r.Delete(context.Background(), f.provider); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	status := runtimePoolTestGetPool(t, f.r, f.pool).Status
	if status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed || !reflect.DeepEqual(status.ActiveInstance, before) {
		t.Fatalf("outage changed active fence or left admission open: %#v", status)
	}
	if f.currentWorkspace(t).Spec.Retirement != nil {
		t.Fatal("provider outage synthesized runtime termination authorization")
	}
	if err := f.r.Get(context.Background(), client.ObjectKeyFromObject(&pod), &corev1.Pod{}); err != nil {
		t.Fatalf("provider outage removed the runtime Pod: %v", err)
	}
}

func TestExternalRuntimePoolRejectsMaterializationChangedDuringAuthenticatedProbe(t *testing.T) {
	for _, evidenceChanged := range []bool{false, true} {
		name := "Pod spec changed"
		if evidenceChanged {
			name = "provider observation changed"
		}
		t.Run(name, func(t *testing.T) {
			f := newExternalRuntimePoolFixture(t)
			_, pod := f.materialize(t)
			f.supervisor.afterProbe = func() {
				f.supervisor.afterProbe = nil
				if evidenceChanged {
					workspace := f.currentWorkspace(t)
					workspace.Status.Allocation.Identity.InstanceID = "replacement-instance"
					if err := f.r.Status().Update(context.Background(), workspace); err != nil {
						t.Fatal(err)
					}
					return
				}
				current := &corev1.Pod{}
				if err := f.r.Get(context.Background(), client.ObjectKeyFromObject(&pod), current); err != nil {
					t.Fatal(err)
				}
				current.Spec.Containers[0].Image = "replaced-after-bootstrap:latest"
				if err := f.r.Update(context.Background(), current); err != nil {
					t.Fatal(err)
				}
			}
			runtimePoolReconcile(t, f.r, f.pool)
			status := runtimePoolTestGetPool(t, f.r, f.pool).Status
			if f.seeds != 1 || f.supervisor.probeCalls != 1 || status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed || status.ActiveInstance != nil {
				t.Fatalf("post-probe race received admission: seeds=%d probes=%d status=%#v", f.seeds, f.supervisor.probeCalls, status)
			}
		})
	}
}

func TestExternalRuntimePoolRequiresExactDrainBeforeRetirement(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	_, pod := f.serve(t)
	current := runtimePoolTestGetPool(t, f.r, f.pool)
	current.Spec.DesiredReplicas = 0
	if err := f.r.Update(context.Background(), &current); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	if f.supervisor.drainCalls != 0 || f.currentWorkspace(t).Spec.Retirement != nil {
		t.Fatal("retirement preceded persisted admission closure")
	}
	runtimePoolReconcile(t, f.r, f.pool)
	if f.supervisor.drainCalls != 1 || f.currentWorkspace(t).Spec.Retirement != nil {
		t.Fatal("retirement preceded acknowledged authenticated drain")
	}
	f.supervisor.probe = runtimePoolValidProbe(f.pool, &pod, "foreign-boot", true)
	runtimePoolReconcile(t, f.r, f.pool)
	if f.currentWorkspace(t).Spec.Retirement != nil {
		t.Fatal("different supervisor boot authorized retirement")
	}
	f.supervisor.probe = runtimePoolValidProbe(f.pool, &pod, "external-boot", true)
	runtimePoolReconcile(t, f.r, f.pool)
	workspace := f.currentWorkspace(t)
	retirement := workspace.Spec.Retirement
	if retirement == nil || retirement.Identity != workspace.Status.Allocation.Identity || retirement.Sequence != workspace.Spec.Workload.Sequence || retirement.Action != workspacev1alpha1.WorkloadRetirementStop {
		t.Fatalf("exact drained retirement = %#v", retirement)
	}
}

func TestExternalRuntimePoolStoppedProofClearsRetirementAndRotatesSequence(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	workspace, pod := f.serve(t)
	oldRequest := workspace.Spec.Workload.DeepCopy()
	oldAuth := runtimePoolTestPrivateAuthSecret(t, f.r, f.pool)
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	pool.Spec.DesiredReplicas = 0
	pool.Annotations["orka.ai/external-runtime-retirement-requested"] = "true"
	if err := f.r.Update(context.Background(), &pool); err != nil {
		t.Fatal(err)
	}
	workspace = f.currentWorkspace(t)
	workspace.Status.Allocation.State = workspacev1alpha1.AllocationStopped
	workspace.Status.Allocation.Startup = nil
	workspace.Status.ObservedGeneration = 0
	if err := f.r.Status().Update(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	if got := runtimePoolTestGetPool(t, f.r, f.pool); got.Annotations["orka.ai/external-runtime-retirement-requested"] != "true" || got.Status.ActiveInstance == nil {
		t.Fatal("stale stopped observation cleared the active retirement fence")
	}
	workspace = f.currentWorkspace(t)
	workspace.Status.ObservedGeneration = workspace.Generation
	if err := f.r.Status().Update(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	if got := runtimePoolTestGetPool(t, f.r, f.pool); got.Annotations["orka.ai/external-runtime-retirement-requested"] != "true" || got.Status.ActiveInstance == nil {
		t.Fatal("provider stopped assertion cleared a still-existing runtime's fence")
	}
	if err := f.r.Get(context.Background(), client.ObjectKeyFromObject(&pod), &corev1.Pod{}); err != nil {
		t.Fatalf("core removed provider Pod to satisfy stopped assertion: %v", err)
	}
	if err := f.r.Delete(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	pool = runtimePoolTestGetPool(t, f.r, f.pool)
	if pool.Annotations["orka.ai/external-runtime-retirement-requested"] != "" || pool.Status.ActiveInstance != nil || pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopped {
		t.Fatalf("current stopped proof did not clear retirement: %#v", pool.Status)
	}
	pool.Spec.DesiredReplicas = 1
	if err := f.r.Update(context.Background(), &pool); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		runtimePoolReconcile(t, f.r, f.pool)
		workspace = f.currentWorkspace(t)
		if workspace.Spec.Workload.Sequence > oldRequest.Sequence {
			break
		}
	}
	request := workspace.Spec.Workload
	if request.Sequence != oldRequest.Sequence+1 || request.PreviousInstance == nil || *request.PreviousInstance != workspace.Status.Allocation.Identity || request.Revision == oldRequest.Revision {
		t.Fatalf("replacement lost sequence or exact previous fence: %#v", request)
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	auth := runtimePoolTestPrivateAuthSecret(t, f.r, f.pool)
	if bytes.Equal(auth.Data[runtimePoolBootstrapNonceKey], oldAuth.Data[runtimePoolBootstrapNonceKey]) || bytes.Equal(auth.Data[runtimePoolControllerTokenKey], oldAuth.Data[runtimePoolControllerTokenKey]) {
		t.Fatal("cold replacement reused consumed bootstrap or private controller credentials")
	}
	if workspace.Spec.Retirement != nil {
		t.Fatal("new sequence retained the previous physical retirement authorization")
	}
}

func TestExternalRuntimePoolTerminalAssertionRequiresIndependentPodAbsence(t *testing.T) {
	for _, test := range []struct {
		name            string
		deleting        bool
		foreignIdentity bool
	}{
		{name: "stopped"},
		{name: "deleted", deleting: true},
		{name: "stopped with foreign identity", foreignIdentity: true},
		{name: "deleted with foreign identity", deleting: true, foreignIdentity: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			f := newExternalRuntimePoolFixture(t)
			workspace, pod := f.serve(t)
			originalIdentity := workspace.Status.Allocation.Identity
			sequence := workspace.Spec.Workload.Sequence
			auth := runtimePoolTestPrivateAuthSecret(t, f.r, f.pool)
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			if test.deleting {
				if err := f.r.Delete(ctx, &pool); err != nil {
					t.Fatal(err)
				}
			} else {
				pool.Annotations["orka.ai/external-runtime-retirement-requested"] = booleanTrueValue
				if err := f.r.Update(ctx, &pool); err != nil {
					t.Fatal(err)
				}
			}
			workspace.Status.Allocation.Startup = nil
			workspace.Status.Allocation.State = workspacev1alpha1.AllocationStopped
			if test.deleting {
				workspace.Status.Allocation.State = workspacev1alpha1.AllocationDeleted
				workspace.Status.Allocation.Disposition = &workspacev1alpha1.ExecutionWorkspaceDisposition{Compute: workspacev1alpha1.DispositionDeleted, AccessCredentials: workspacev1alpha1.DispositionRevoked,
					EphemeralSecrets: workspacev1alpha1.DispositionDeleted, WorkspaceData: workspacev1alpha1.DispositionDeleted, PersistentVolumes: workspacev1alpha1.DispositionDeleted,
					Checkpoints: workspacev1alpha1.DispositionDeleted, ProviderResources: workspacev1alpha1.DispositionDeleted}
			}
			if test.foreignIdentity {
				workspace.Status.Allocation.Identity.InstanceID = "foreign-instance"
			}
			if err := f.r.Status().Update(ctx, workspace); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				runtimePoolReconcile(t, f.r, f.pool)
			}
			pool = runtimePoolTestGetPool(t, f.r, f.pool)
			if !slices.Contains(pool.Finalizers, runtimePoolFinalizer) || pool.Status.ActiveInstance == nil || pool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed || f.currentWorkspace(t).Spec.Workload.Sequence != sequence {
				t.Fatalf("provider terminal assertion released a live-instance fence: %#v", pool)
			}
			if err := f.r.Get(ctx, client.ObjectKeyFromObject(&pod), &corev1.Pod{}); err != nil {
				t.Fatalf("core removed provider-owned Pod: %v", err)
			}
			keptAuth := &corev1.Secret{}
			if err := f.r.Get(ctx, client.ObjectKeyFromObject(&auth), keptAuth); err != nil || !bytes.Equal(keptAuth.Data[runtimePoolControllerTokenKey], auth.Data[runtimePoolControllerTokenKey]) {
				t.Fatalf("provider terminal assertion removed or rotated live-instance credentials: %v", err)
			}
			if err := f.r.Delete(ctx, &pod); err != nil {
				t.Fatal(err)
			}
			workspace = f.currentWorkspace(t)
			workspace.Status.Allocation.Identity = originalIdentity
			if err := f.r.Status().Update(ctx, workspace); err != nil {
				t.Fatal(err)
			}
			if !test.deleting {
				runtimePoolReconcile(t, f.r, f.pool)
				pool = runtimePoolTestGetPool(t, f.r, f.pool)
				if pool.Status.ActiveInstance != nil || pool.Annotations["orka.ai/external-runtime-retirement-requested"] != "" || pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleStopped {
					t.Fatal("independent stopped proof did not release the active fence")
				}
				return
			}
			for range 6 {
				if _, err := f.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.pool)}); err != nil {
					t.Fatal(err)
				}
				if err := f.r.Get(ctx, client.ObjectKeyFromObject(f.pool), &corev1alpha1.RuntimePool{}); apierrors.IsNotFound(err) {
					break
				}
			}
			if err := f.r.Get(ctx, client.ObjectKeyFromObject(f.pool), &corev1alpha1.RuntimePool{}); !apierrors.IsNotFound(err) {
				t.Fatalf("verified deletion did not release RuntimePool finalizer: %v", err)
			}
			if err := f.r.Get(ctx, client.ObjectKeyFromObject(&auth), &corev1.Secret{}); !apierrors.IsNotFound(err) {
				t.Fatalf("verified deletion retained core credentials: %v", err)
			}
		})
	}
}

func TestExternalRuntimePoolCancellationBeforeReplacementStartup(t *testing.T) {
	for _, foreignPredecessor := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact predecessor", true: "foreign predecessor"}[foreignPredecessor], func(t *testing.T) {
			ctx := context.Background()
			f := newExternalRuntimePoolFixture(t)
			workspace, pod := f.serve(t)
			previous := workspace.Status.Allocation.Identity
			if err := f.r.Delete(ctx, &pod); err != nil {
				t.Fatal(err)
			}
			next := workspace.Spec.Workload.DeepCopy()
			next.Sequence++
			next.PreviousInstance = &previous
			if foreignPredecessor {
				next.PreviousInstance.InstanceID = "foreign"
			}
			next.Revision, _ = workspaceprovider.WorkloadRevision(*next)
			workspace.Spec.Workload = next
			workspace.Spec.Retirement = nil
			if err := f.r.Update(ctx, workspace); err != nil {
				t.Fatal(err)
			}
			identity := workspacev1alpha1.InstanceIdentity{AllocationID: previous.AllocationID, InstanceID: "replacement-pending", RequestRevision: next.Revision}
			workspace.Status.Allocation = &workspacev1alpha1.AllocationObservation{Sequence: next.Sequence, Key: next.Key, Identity: identity, State: workspacev1alpha1.AllocationPending}
			workspace.Status.ObservedGeneration = workspace.Generation
			if err := f.r.Status().Update(ctx, workspace); err != nil {
				t.Fatal(err)
			}
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			pool.Status.ActiveInstance = nil
			pool.Status.Capacity = corev1alpha1.RuntimePoolCapacityStatus{}
			pool.Status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
			if err := f.r.Status().Update(ctx, &pool); err != nil {
				t.Fatal(err)
			}
			if err := f.r.Delete(ctx, &pool); err != nil {
				t.Fatal(err)
			}
			runtimePoolReconcile(t, f.r, f.pool)
			workspace = f.currentWorkspace(t)
			if foreignPredecessor {
				if workspace.Spec.Retirement != nil {
					t.Fatal("foreign predecessor authorized retirement")
				}
				return
			}
			if workspace.Spec.Retirement == nil || workspace.Spec.Retirement.Sequence != next.Sequence || workspace.Spec.Retirement.Identity != identity || workspace.Spec.Retirement.Action != workspacev1alpha1.WorkloadRetirementDelete {
				t.Fatalf("replacement cancellation did not authorize exact retirement: %#v", workspace.Spec.Retirement)
			}
			workspace.Status.Allocation.State = workspacev1alpha1.AllocationDeleted
			workspace.Status.Allocation.Disposition = &workspacev1alpha1.ExecutionWorkspaceDisposition{Compute: workspacev1alpha1.DispositionDeleted, AccessCredentials: workspacev1alpha1.DispositionRevoked, EphemeralSecrets: workspacev1alpha1.DispositionDeleted, WorkspaceData: workspacev1alpha1.DispositionDeleted, PersistentVolumes: workspacev1alpha1.DispositionDeleted, Checkpoints: workspacev1alpha1.DispositionDeleted, ProviderResources: workspacev1alpha1.DispositionDeleted}
			workspace.Status.ObservedGeneration = workspace.Generation
			if err := f.r.Status().Update(ctx, workspace); err != nil {
				t.Fatal(err)
			}
			for range 6 {
				if _, err := f.r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.pool)}); err != nil {
					t.Fatal(err)
				}
				if err := f.r.Get(ctx, client.ObjectKeyFromObject(f.pool), &corev1alpha1.RuntimePool{}); apierrors.IsNotFound(err) {
					return
				}
			}
			t.Fatal("terminated unbootstrapped replacement stranded its pool finalizer")
		})
	}
}

func TestExternalRuntimePoolNativeTerminationDoesNotRequireWorkerDeletion(t *testing.T) {
	ctx := context.Background()
	f := newExternalRuntimePoolFixture(t)
	f.advertiseNativeProcess(t)
	workspace, worker := f.materialize(t)
	workspace.Status.Allocation.Startup.Pod = nil
	workspace.Status.Allocation.Startup.Process = &workspacev1alpha1.NativeProcessEvidence{Namespace: "native", Name: "process", UID: workspace.Status.Allocation.Identity.InstanceID, Version: 1, Worker: workspacev1alpha1.PodReference{Namespace: worker.Namespace, Name: worker.Name, UID: worker.UID}, ChallengeSHA256: "sha256:" + strings.Repeat("a", 64)}
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if err := f.r.bindExternalRuntimeInstanceEvidence(ctx, &pool, workspace); err != nil {
		t.Fatal(err)
	}
	workspace.Status.Allocation.Startup = nil
	workspace.Status.Allocation.State = workspacev1alpha1.AllocationStopped
	if stopped, err := f.r.externalObservedInstanceTerminated(ctx, &pool, workspace); err != nil || stopped {
		t.Fatalf("unauthorized process assertion released credentials: %v/%v", stopped, err)
	}
	workspace.Spec.Retirement = &workspacev1alpha1.WorkloadRetirement{Sequence: workspace.Spec.Workload.Sequence, Identity: workspace.Status.Allocation.Identity, Action: workspacev1alpha1.WorkloadRetirementStop}
	if stopped, err := f.r.externalObservedInstanceTerminated(ctx, &pool, workspace); err != nil || !stopped {
		t.Fatalf("exact authorized native termination kept worker as runtime fence: %v/%v", stopped, err)
	}
	if err := f.r.Get(ctx, client.ObjectKeyFromObject(&worker), &corev1.Pod{}); err != nil {
		t.Fatalf("core removed infrastructure worker: %v", err)
	}
	workspace.Status.Allocation.Identity.InstanceID = "replacement"
	if stopped, _ := f.r.externalObservedInstanceTerminated(ctx, &pool, workspace); stopped {
		t.Fatal("changed native process identity was accepted")
	}
}

func TestExternalRuntimePoolReadySuspendRolloverPreservesRetainedLineage(t *testing.T) {
	ctx := context.Background()
	f := newExternalRuntimePoolFixture(t)
	workspace := f.currentWorkspace(t)
	workspace.Spec.Lifecycle.AllowedOnDetach = append(workspace.Spec.Lifecycle.AllowedOnDetach, workspacev1alpha1.WorkspaceOnDetachSuspend)
	workspace.Annotations[acpWorkspaceSuspendModeAnnotation] = "DataOnly"
	if err := f.r.Update(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	workspace, pod := f.serve(t)
	oldRequest := workspace.Spec.Workload.DeepCopy()
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	pool.Annotations["orka.ai/external-runtime-retirement-requested"] = booleanTrueValue
	if err := f.r.Update(ctx, &pool); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	runtimePoolReconcile(t, f.r, f.pool)
	f.supervisor.probe = runtimePoolValidProbe(f.pool, &pod, "external-boot", true)
	runtimePoolReconcile(t, f.r, f.pool)
	workspace = f.currentWorkspace(t)
	if workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady || workspace.Spec.Retirement == nil || workspace.Spec.Retirement.Action != workspacev1alpha1.WorkloadRetirementSuspend {
		t.Fatalf("Ready rollover failed to authorize DataOnly suspension: %#v", workspace.Spec.Retirement)
	}
	retained := &workspacev1alpha1.RetainedDataReference{ID: "retained-lineage", SourceInstance: workspace.Status.Allocation.Identity, ProofSHA256: "sha256:" + strings.Repeat("4", 64)}
	workspace.Status.Allocation.State = workspacev1alpha1.AllocationStopped
	workspace.Status.Allocation.Startup = nil
	workspace.Status.Allocation.RetainedData = retained.DeepCopy()
	if err := f.r.Status().Update(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Delete(ctx, &pod); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		runtimePoolReconcile(t, f.r, f.pool)
		workspace = f.currentWorkspace(t)
		if workspace.Spec.Workload.Sequence > oldRequest.Sequence {
			break
		}
	}
	request := workspace.Spec.Workload
	if request.Sequence != oldRequest.Sequence+1 || request.PreviousInstance == nil || *request.PreviousInstance != retained.SourceInstance || !reflect.DeepEqual(request.RetainedData, retained) || workspace.Spec.Retirement != nil || workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady {
		t.Fatalf("Ready replacement lost exact retained lineage: %#v", request)
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestExternalRuntimePoolPublicResourceDefaultsAndFrozenRevalidation(t *testing.T) {
	f := newExternalRuntimePoolFixture(t)
	w := f.publish(t)
	request := w.Spec.Workload
	if len(request.Resources.Requests) != 2 || len(request.Resources.Limits) != 2 ||
		!reflect.DeepEqual(request.Resources, externalRuntimePoolResourceRequirements(runtimePoolResourceClassStandard)) ||
		!reflect.DeepEqual(request.Resources, request.Runtime.Template.Spec.Containers[0].Resources) {
		t.Fatalf("external public resources differ from CPU/memory admission: %#v", request.Resources)
	}
	if err := workspaceprovider.ValidateWorkspaceWorkload(w); err != nil {
		t.Fatal(err)
	}
	scratchVolumes := 0
	for _, volume := range request.Runtime.Template.Spec.Volumes {
		if volume.EmptyDir != nil {
			scratchVolumes++
			if volume.EmptyDir.SizeLimit != nil {
				t.Fatal("external request carries a Kubernetes scratch storage quota")
			}
		}
	}
	if scratchVolumes == 0 {
		t.Fatal("external workload has no isolated scratch volumes")
	}
	cfg, err := f.r.runtimePoolConfig(f.pool)
	if err != nil {
		t.Fatal(err)
	}
	plainTemplate := f.r.runtimePoolPodTemplate(f.pool, cfg, cfg.labels, "auth", "provider")
	for _, volume := range plainTemplate.Spec.Volumes {
		if volume.EmptyDir != nil && volume.EmptyDir.SizeLimit == nil {
			t.Fatal("plain Deployment lost its scratch quota")
		}
	}
	plain := runtimePoolResourceRequirements(runtimePoolResourceClassStandard)
	if plain.Requests.StorageEphemeral().Cmp(resource.MustParse("1Gi")) != 0 || plain.Limits.StorageEphemeral().Cmp(resource.MustParse("5Gi")) != 0 {
		t.Fatal("plain Deployment storage quotas changed")
	}

	// A persisted request made by an earlier release retains every positively
	// admitted quota. New defaults must never rewrite or weaken that request.
	request.Resources = plain
	request.Runtime.Template.Spec.Containers[0].Resources = plain
	for i := range request.Runtime.Template.Spec.Volumes {
		if volume := request.Runtime.Template.Spec.Volumes[i].EmptyDir; volume != nil {
			volume.SizeLimit = new(resource.MustParse("1Gi"))
		}
	}
	request.Revision, err = workspacev1alpha1.WorkloadRevision(*request)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.r.Update(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if _, _, err := runtimePoolPodTemplateValidationTarget(&pool, request.Runtime.Template); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	got := f.currentWorkspace(t).Spec.Workload
	if !reflect.DeepEqual(got, request) {
		t.Fatal("revalidation weakened a persisted resource request")
	}
}

type unavailableExternalPodListReader struct{ client.Reader }

func (r unavailableExternalPodListReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*corev1.PodList); ok {
		return errors.New("Pod observation unavailable")
	}
	return r.Reader.List(ctx, list, opts...)
}

func TestExternalRuntimePoolRetiresLostExactPodWithoutProviderDrainWaiver(t *testing.T) {
	for _, test := range []struct {
		name                                                                               string
		removePod, deleting, wrongActiveUID, native, listUnavailable, busy, wantRetirement bool
	}{
		{name: "exact observed Pod gone", removePod: true, wantRetirement: true},
		{name: "delete exact observed Pod gone", removePod: true, deleting: true, wantRetirement: true},
		{name: "provider withdrew startup but Pod remains"},
		{name: "active UID differs", removePod: true, wrongActiveUID: true},
		{name: "native worker disappearance is not process proof", removePod: true, native: true},
		{name: "Pod list unavailable", removePod: true, listUnavailable: true},
		{name: "controller settlement pending", removePod: true, busy: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			f := newExternalRuntimePoolFixture(t)
			w, pod := f.serve(t)
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			pool.Spec.DesiredReplicas = 0
			if test.native {
				evidence, err := externalRuntimeEvidence(&pool)
				if err != nil {
					t.Fatal(err)
				}
				evidence.NativeProcess = true
				encoded, err := json.Marshal(evidence)
				if err != nil {
					t.Fatal(err)
				}
				pool.Annotations[externalRuntimeEvidenceAnnotation] = string(encoded)
			}
			if err := f.r.Update(ctx, &pool); err != nil {
				t.Fatal(err)
			}
			if test.wrongActiveUID {
				pool.Status.ActiveInstance.PodUID = "unobserved-replacement"
			}
			if test.busy {
				pool.Status.Capacity.FinalizingSessions = 1
			}
			if err := f.r.Status().Update(ctx, &pool); err != nil {
				t.Fatal(err)
			}
			if test.deleting {
				w.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredDeleted
				if err := f.r.Update(ctx, w); err != nil {
					t.Fatal(err)
				}
			}
			w.Status.Allocation.State = workspacev1alpha1.AllocationPending
			w.Status.Allocation.Startup = nil
			if err := f.r.Status().Update(ctx, w); err != nil {
				t.Fatal(err)
			}
			if test.removePod {
				if err := f.r.Delete(ctx, &pod); err != nil {
					t.Fatal(err)
				}
			}
			if test.listUnavailable {
				f.r.APIReader = unavailableExternalPodListReader{f.r.Client}
			}
			for range 2 {
				runtimePoolReconcile(t, f.r, f.pool)
			}
			got := f.currentWorkspace(t)
			if (got.Spec.Retirement != nil) != test.wantRetirement {
				t.Fatalf("retirement = %#v, want intent %v", got.Spec.Retirement, test.wantRetirement)
			}
			current := runtimePoolTestGetPool(t, f.r, f.pool)
			if current.Status.ActiveInstance == nil || current.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed {
				t.Fatal("lost runtime discarded its active fence or left admission open")
			}
			if f.supervisor.drainCalls != 0 {
				t.Fatal("provider withdrawal attempted an unverified runtime drain")
			}
			if test.deleting && test.wantRetirement && got.Spec.Retirement.Action != workspacev1alpha1.WorkloadRetirementDelete {
				t.Fatal("deletion request did not authorize exact allocation deletion")
			}
			if test.wantRetirement && (got.Spec.Retirement.Sequence != w.Spec.Workload.Sequence || got.Spec.Retirement.Identity != w.Status.Allocation.Identity) {
				t.Fatal("retirement lost its exact allocation fence")
			}
		})
	}
}

func TestExternalRuntimePoolClosesNeverBootstrappedDeletedRequest(t *testing.T) {
	for _, test := range []struct {
		name        string
		mutate      func(*corev1alpha1.RuntimePool, *workspacev1alpha1.ExecutionWorkspace)
		wantCleanup bool
	}{
		{name: "terminal no-allocation proof", wantCleanup: true},
		{name: "no provider-owned credentials ever existed", wantCleanup: true, mutate: func(_ *corev1alpha1.RuntimePool, w *workspacev1alpha1.ExecutionWorkspace) {
			w.Status.Disposition.AccessCredentials = workspacev1alpha1.DispositionNotApplicable
		}},
		{name: "prior independently observed instance", mutate: func(p *corev1alpha1.RuntimePool, _ *workspacev1alpha1.ExecutionWorkspace) {
			p.Annotations[externalRuntimeEvidenceAnnotation] = "prior-evidence"
		}},
		{name: "prior credential bootstrap", mutate: func(p *corev1alpha1.RuntimePool, w *workspacev1alpha1.ExecutionWorkspace) {
			p.Annotations[runtimePoolBootstrapInstanceBindingAnnotation] = `{"authSecretUID":"used-auth-uid","workloadUID":"used-instance-uid"}`
			w.Status.Disposition.AccessCredentials = workspacev1alpha1.DispositionNotApplicable
		}},
		{name: "active instance", mutate: func(p *corev1alpha1.RuntimePool, _ *workspacev1alpha1.ExecutionWorkspace) {
			p.Status.ActiveInstance = &corev1alpha1.RuntimePoolActiveInstanceStatus{PodUID: "live-uid"}
		}},
		{name: "controller settlement pending", mutate: func(p *corev1alpha1.RuntimePool, _ *workspacev1alpha1.ExecutionWorkspace) {
			p.Status.Capacity.FinalizingSessions = 1
		}},
		{name: "stale provider proof", mutate: func(_ *corev1alpha1.RuntimePool, w *workspacev1alpha1.ExecutionWorkspace) {
			w.Status.ObservedGeneration = 0
		}},
		{name: "compute disposition pending", mutate: func(_ *corev1alpha1.RuntimePool, w *workspacev1alpha1.ExecutionWorkspace) {
			w.Status.Disposition.Compute = workspacev1alpha1.DispositionPending
		}},
		{name: "incomplete credential disposition", mutate: func(_ *corev1alpha1.RuntimePool, w *workspacev1alpha1.ExecutionWorkspace) {
			w.Status.Disposition.AccessCredentials = workspacev1alpha1.DispositionPending
		}},
		{name: "provider still identifies compute", mutate: func(_ *corev1alpha1.RuntimePool, w *workspacev1alpha1.ExecutionWorkspace) {
			w.Status.ExternalID = "allocated-instance"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			f := newExternalRuntimePoolFixture(t)
			w := f.publish(t)
			auth := runtimePoolTestPrivateAuthSecret(t, f.r, f.pool)
			pool := runtimePoolTestGetPool(t, f.r, f.pool)
			w.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredDeleted
			w.Status.State = workspacev1alpha1.ExecutionWorkspaceStateDeleted
			w.Status.ObservedGeneration = w.Generation
			w.Status.Disposition = &workspacev1alpha1.ExecutionWorkspaceDisposition{Compute: workspacev1alpha1.DispositionDeleted, AccessCredentials: workspacev1alpha1.DispositionRevoked, EphemeralSecrets: workspacev1alpha1.DispositionDeleted, WorkspaceData: workspacev1alpha1.DispositionNotApplicable, PersistentVolumes: workspacev1alpha1.DispositionNotApplicable, Checkpoints: workspacev1alpha1.DispositionNotApplicable, ProviderResources: workspacev1alpha1.DispositionNotApplicable}
			if test.mutate != nil {
				test.mutate(&pool, w)
			}
			poolStatus := pool.Status.DeepCopy()
			if err := f.r.Update(ctx, &pool); err != nil {
				t.Fatal(err)
			}
			pool.Status = *poolStatus
			if err := f.r.Status().Update(ctx, &pool); err != nil {
				t.Fatal(err)
			}
			workspaceStatus := w.Status.DeepCopy()
			if err := f.r.Update(ctx, w); err != nil {
				t.Fatal(err)
			}
			w.Status = *workspaceStatus
			if err := f.r.Status().Update(ctx, w); err != nil {
				t.Fatal(err)
			}
			if err := f.r.Delete(ctx, &pool); err != nil {
				t.Fatal(err)
			}
			if test.wantCleanup {
				currentPool := runtimePoolTestGetPool(t, f.r, f.pool)
				currentWorkspace := f.currentWorkspace(t)
				if !externalNeverBootstrappedDeletionProven(&currentPool, currentWorkspace) {
					t.Fatalf("no-allocation proof rejected: pool=%#v workspace=%#v workload=%v disposition=%v", currentPool.Status, currentWorkspace.Status, workspaceprovider.ValidateWorkspaceWorkload(currentWorkspace), workspaceprovider.ValidateDeletedDisposition(currentWorkspace.Status.Disposition, currentWorkspace.Spec.Lifecycle.DeletionPolicy))
				}
			}
			for range 2 {
				runtimePoolReconcile(t, f.r, f.pool)
			}
			err := f.r.Get(ctx, client.ObjectKeyFromObject(&auth), &corev1.Secret{})
			if apierrors.IsNotFound(err) != test.wantCleanup {
				t.Fatalf("private credentials cleanup = %v, want %v", err, test.wantCleanup)
			}
			err = f.r.Get(ctx, client.ObjectKeyFromObject(f.pool), &corev1alpha1.RuntimePool{})
			if apierrors.IsNotFound(err) != test.wantCleanup {
				t.Fatalf("pool finalizer release = %v, want %v", err, test.wantCleanup)
			}
			if f.seeds != 0 {
				t.Fatal("no-allocation cleanup released runtime credentials")
			}
		})
	}
}

func TestExternalRuntimePoolQuarantineAuthorizesStopForSuspendCapableClass(t *testing.T) {
	ctx := context.Background()
	f := newExternalRuntimePoolFixture(t)
	workspace := f.currentWorkspace(t)
	workspace.Spec.Lifecycle.AllowedOnDetach = append(workspace.Spec.Lifecycle.AllowedOnDetach, workspacev1alpha1.WorkspaceOnDetachSuspend)
	workspace.Annotations[acpWorkspaceSuspendModeAnnotation] = "DataOnly"
	if err := f.r.Update(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	workspace, pod := f.serve(t)
	workspace.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined
	workspace.Spec.Attachment = nil
	if err := f.r.Update(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	// Every external provider acts on quarantine only through a Stop (or Delete)
	// authorization; a Suspend authorization would leave the instance running.
	for range 6 {
		runtimePoolReconcile(t, f.r, f.pool)
		f.supervisor.probe = runtimePoolValidProbe(f.pool, &pod, "external-boot", true)
		workspace = f.currentWorkspace(t)
		if workspace.Spec.Retirement != nil {
			break
		}
	}
	if workspace.Spec.Retirement == nil || workspace.Spec.Retirement.Action != workspacev1alpha1.WorkloadRetirementStop {
		t.Fatalf("quarantine did not authorize an exact-instance Stop: %#v", workspace.Spec.Retirement)
	}
	if workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined || workspace.Spec.Retirement.Identity != workspace.Status.Allocation.Identity || workspace.Spec.Retirement.Sequence != workspace.Spec.Workload.Sequence {
		t.Fatalf("quarantine retirement changed intent or lost the exact instance: %#v", workspace.Spec)
	}
	if err := workspaceprovider.ValidateWorkloadRetirement(workspace.Spec.Workload, workspace.Status.Allocation, workspace.Spec.Retirement, workspacev1alpha1.WorkloadRetirementStop); err != nil {
		t.Fatalf("provider Stop validation rejects the quarantine authorization: %v", err)
	}
}
