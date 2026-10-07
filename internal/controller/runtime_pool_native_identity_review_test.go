package controller

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func nativeIdentityReviewFixture(t *testing.T) (*externalRuntimePoolFixture, *workspacev1alpha1.ExecutionWorkspace, corev1.Pod) {
	t.Helper()
	f := newExternalRuntimePoolFixture(t)
	f.advertiseNativeProcess(t)
	w, worker := f.materialize(t)
	a := w.Status.Allocation
	a.Identity.InstanceID = "provider-instance-sensitive"
	a.Startup.Identity = a.Identity
	a.Startup.Pod = nil
	a.Startup.Endpoint = "http://provider-route.example:80/runtime"
	a.Startup.Process = &workspacev1alpha1.NativeProcessEvidence{Namespace: "native", Name: "provider-process-sensitive", UID: a.Identity.InstanceID, Version: 1,
		Worker: workspacev1alpha1.PodReference{Namespace: worker.Namespace, Name: worker.Name, UID: worker.UID}, ChallengeSHA256: "sha256:" + strings.Repeat("a", 64)}
	if err := f.r.Status().Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	return f, w, worker
}

func nativeIdentityReviewEnv(t *testing.T, request *workspacev1alpha1.WorkloadRequest) *corev1.EnvVar {
	t.Helper()
	for i := range request.Runtime.Template.Spec.Containers[0].Env {
		variable := &request.Runtime.Template.Spec.Containers[0].Env[i]
		if variable.Name == "ORKA_ACP_POD_UID" {
			return variable
		}
	}
	t.Fatal("published native workload has no runtime identity")
	return nil
}

func TestExternalNativeRuntimeFenceKeepsProviderIdentityOutOfTaskStatus(t *testing.T) {
	f, w, _ := nativeIdentityReviewFixture(t)
	env := nativeIdentityReviewEnv(t, w.Spec.Workload)
	if env.ValueFrom != nil || !strings.HasPrefix(env.Value, "workspace:") {
		t.Fatal("native runtime did not receive a literal opaque Core identity before admission")
	}
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	if err := f.r.bindExternalRuntimeInstanceEvidence(t.Context(), &pool, w); err != nil {
		t.Fatal(err)
	}
	pod, err := f.r.attestExternalWorkspaceStartup(t.Context(), w.Spec.Workload, w.Status.Allocation.Startup)
	if err != nil || pod == nil || string(pod.UID) != env.Value {
		t.Fatalf("native startup lost the admitted opaque identity: %v", err)
	}
	probe := runtimePoolValidProbe(&pool, pod, "opaque-boot", false)
	cfg, err := f.r.runtimePoolConfig(&pool)
	if err != nil {
		t.Fatal(err)
	}
	active, err := validateRuntimePoolProbe(&pool, cfg, pod, probe, f.r.now())
	if err != nil {
		t.Fatal(err)
	}
	status := &corev1alpha1.TaskExecutionStatus{}
	applyRuntimeSessionBindingToExecution(status, &ACPRuntimeSessionBinding{RuntimeInstanceID: harnessv2.RuntimeInstanceID(active.RuntimeInstanceID), SupervisorBootID: harnessv2.SupervisorBootID(active.BootID)})
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{w.Status.Allocation.Identity.AllocationID, w.Status.Allocation.Identity.InstanceID, w.Status.Allocation.Startup.Process.Name, w.Status.Allocation.Startup.Endpoint, string(w.Status.Allocation.Startup.Process.Worker.UID)} {
		if strings.Contains(string(encoded), private) {
			t.Fatal("Task execution status contains provider-native identity")
		}
	}
	pool.Status.ActiveInstance = active
	if err := f.r.Status().Update(t.Context(), &pool); err != nil {
		t.Fatal(err)
	}
	if endpoint, err := runtimePoolWorkspaceStartupEndpoint(t.Context(), f.r.Client, &pool); err != nil || endpoint != w.Status.Allocation.Startup.Endpoint {
		t.Fatalf("opaque fence lost its exact native endpoint: %v", err)
	}
	privateEvidence, err := externalRuntimeEvidence(&pool)
	if err != nil || privateEvidence.Identity != w.Status.Allocation.Identity || privateEvidence.Pod != w.Status.Allocation.Startup.Process.Worker {
		t.Fatalf("opaque runtime replaced private provider evidence: %v", err)
	}
	probe.Status.Fence.RuntimeInstanceID = harnessv2.RuntimeInstanceID(w.Status.Allocation.Identity.InstanceID + "." + active.BootID)
	if _, err := validateRuntimePoolProbe(&pool, cfg, pod, probe, f.r.now()); err == nil {
		t.Fatal("raw provider fence satisfied the admitted opaque runtime identity")
	}
	changed := w.DeepCopy()
	changed.Status.Allocation.Identity.InstanceID = "replacement-provider-instance"
	changed.Status.Allocation.Startup.Identity = changed.Status.Allocation.Identity
	changed.Status.Allocation.Startup.Process.UID = changed.Status.Allocation.Identity.InstanceID
	if err := f.r.bindExternalRuntimeInstanceEvidence(t.Context(), &pool, changed); err == nil {
		t.Fatal("replacement native identity inherited the opaque Core fence")
	}
}

func TestExternalNativeRuntimeIdentityRejectsIntentAndWorkerDrift(t *testing.T) {
	for _, change := range []string{"literal identity", "missing identity", "duplicate identity", "pool UID", "workspace UID", "sequence", "worker UID"} {
		t.Run(change, func(t *testing.T) {
			f, w, _ := nativeIdentityReviewFixture(t)
			request := w.Spec.Workload.DeepCopy()
			evidence := w.Status.Allocation.Startup.DeepCopy()
			switch change {
			case "literal identity":
				nativeIdentityReviewEnv(t, request).Value = evidence.Identity.InstanceID
			case "missing identity":
				nativeIdentityReviewEnv(t, request).Name = "OTHER_IDENTITY"
			case "duplicate identity":
				request.Runtime.Template.Spec.Containers[0].Env = append(request.Runtime.Template.Spec.Containers[0].Env, *nativeIdentityReviewEnv(t, request))
			case "pool UID":
				request.Runtime.PoolBinding.UID = "replacement-pool"
			case "workspace UID":
				request.Key.WorkspaceUID = "replacement-workspace"
			case "sequence":
				request.Sequence++
			case "worker UID":
				evidence.Process.Worker.UID = "replacement-worker"
			}
			if pod, err := f.r.attestExternalWorkspaceStartup(t.Context(), request, evidence); err == nil || pod != nil {
				t.Fatal("changed native intent/worker supplied a runtime identity")
			}
		})
	}
}

func TestExternalNativeRuntimeColdSequenceRotatesOpaqueIdentity(t *testing.T) {
	f, w, _ := nativeIdentityReviewFixture(t)
	oldUID := nativeIdentityReviewEnv(t, w.Spec.Workload).Value
	oldIdentity := w.Status.Allocation.Identity
	w.Status.Allocation.State = workspacev1alpha1.AllocationStopped
	w.Status.Allocation.Startup = nil
	if err := f.r.Status().Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	cfg, err := f.r.runtimePoolConfig(f.pool)
	if err != nil {
		t.Fatal(err)
	}
	auth, provider, err := f.r.ensureRuntimePoolSecrets(t.Context(), f.pool, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.r.publishExternalWorkspaceWorkload(t.Context(), f.pool, cfg, w, auth, provider); err != nil {
		t.Fatal(err)
	}
	next := f.currentWorkspace(t).Spec.Workload
	if next.Sequence != 2 || next.PreviousInstance == nil || *next.PreviousInstance != oldIdentity || nativeIdentityReviewEnv(t, next).Value == oldUID {
		t.Fatal("cold sequence did not rotate opaque identity while retaining the exact retired provider fence")
	}
	if revision, err := workspacev1alpha1.WorkloadRevision(*next); err != nil || revision != next.Revision {
		t.Fatalf("new opaque identity was not frozen before workload revision: %v", err)
	}
}

func TestExternalNativeLegacyIdentityIsCleanupOnly(t *testing.T) {
	f, w, worker := nativeIdentityReviewFixture(t)
	identityEnv := nativeIdentityReviewEnv(t, w.Spec.Workload)
	*identityEnv = corev1.EnvVar{Name: identityEnv.Name, ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}}
	var err error
	w.Spec.Workload.Revision, err = workspacev1alpha1.WorkloadRevision(*w.Spec.Workload)
	if err != nil {
		t.Fatal(err)
	}
	w.Status.Allocation.Identity.RequestRevision = w.Spec.Workload.Revision
	w.Status.Allocation.Startup.Identity = w.Status.Allocation.Identity
	legacyStatus := w.Status.DeepCopy()
	if err := f.r.Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	w.Status = *legacyStatus
	if err := f.r.Status().Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	frozen := w.Spec.Workload.DeepCopy()
	pool := runtimePoolTestGetPool(t, f.r, f.pool)
	// Simulate persisted evidence from the preceding release, without runtimeUID.
	oldEvidence := externalRuntimeInstanceEvidence{WorkspaceUID: w.UID, Sequence: w.Spec.Workload.Sequence, Identity: w.Status.Allocation.Identity, Pod: w.Status.Allocation.Startup.Process.Worker, NativeProcess: true, Endpoint: w.Status.Allocation.Startup.Endpoint}
	encoded, err := json.Marshal(oldEvidence)
	if err != nil {
		t.Fatal(err)
	}
	pool.Annotations[externalRuntimeEvidenceAnnotation] = string(encoded)
	if err := f.r.Update(t.Context(), &pool); err != nil {
		t.Fatal(err)
	}
	pod, err := f.r.attestExternalWorkspaceStartup(t.Context(), w.Spec.Workload, w.Status.Allocation.Startup)
	if err != nil || string(pod.UID) != w.Status.Allocation.Identity.InstanceID {
		t.Fatalf("legacy cleanup lost its admitted exact runtime identity: %v", err)
	}
	f.supervisor.probe = runtimePoolValidProbe(&pool, pod, "legacy-boot", false)
	cfg, err := f.r.runtimePoolConfig(&pool)
	if err != nil {
		t.Fatal(err)
	}
	pool.Status.ActiveInstance, err = validateRuntimePoolProbe(&pool, cfg, pod, f.supervisor.probe, f.r.now())
	if err != nil {
		t.Fatal(err)
	}
	pool.Status.Lifecycle = corev1alpha1.RuntimePoolLifecycleServing
	pool.Status.AdmissionState = corev1alpha1.RuntimePoolAdmissionAccepting
	if err := f.r.Status().Update(t.Context(), &pool); err != nil {
		t.Fatal(err)
	}
	if _, err := runtimePoolWorkspaceStartupEndpoint(t.Context(), f.r.Client, &pool); err == nil {
		t.Fatal("legacy native identity admitted Task dispatch")
	}
	assertNativeLegacyCleanupRecovery(t, f, &pool, pod)
	runtimePoolReconcile(t, f.r, f.pool)
	pool = runtimePoolTestGetPool(t, f.r, f.pool)
	if pool.Status.AdmissionState != corev1alpha1.RuntimePoolAdmissionClosed || !reflect.DeepEqual(f.currentWorkspace(t).Spec.Workload, frozen) {
		t.Fatal("legacy admission remained open or immutable workload was rewritten")
	}
	if err := f.r.Delete(t.Context(), &pool); err != nil {
		t.Fatal(err)
	}
	runtimePoolReconcile(t, f.r, f.pool)
	if f.supervisor.drainCalls != 1 || f.currentWorkspace(t).Spec.Retirement != nil {
		current := runtimePoolTestGetPool(t, f.r, f.pool)
		t.Fatalf("legacy cleanup has drainCalls=%d lifecycle=%s message=%s", f.supervisor.drainCalls, current.Status.Lifecycle, current.Status.Message)
	}
	f.supervisor.probe = runtimePoolValidProbe(&pool, pod, "foreign-boot", true)
	runtimePoolReconcile(t, f.r, f.pool)
	if f.currentWorkspace(t).Spec.Retirement != nil {
		t.Fatal("legacy cleanup accepted a replacement supervisor fence")
	}
	f.supervisor.probe = runtimePoolValidProbe(&pool, pod, "legacy-boot", true)
	runtimePoolReconcile(t, f.r, f.pool)
	w = f.currentWorkspace(t)
	if w.Spec.Retirement == nil || w.Spec.Retirement.Identity != w.Status.Allocation.Identity || w.Spec.Retirement.Action != workspacev1alpha1.WorkloadRetirementDelete {
		t.Fatal("legacy cleanup did not publish exact provider retirement")
	}
	w.Status.Allocation.Startup = nil
	w.Status.Allocation.State = workspacev1alpha1.AllocationDeleted
	w.Status.Allocation.Disposition = &workspacev1alpha1.ExecutionWorkspaceDisposition{Compute: workspacev1alpha1.DispositionDeleted, ProviderResources: workspacev1alpha1.DispositionDeleted, AccessCredentials: workspacev1alpha1.DispositionRevoked, EphemeralSecrets: workspacev1alpha1.DispositionDeleted, WorkspaceData: workspacev1alpha1.DispositionDeleted, PersistentVolumes: workspacev1alpha1.DispositionDeleted, Checkpoints: workspacev1alpha1.DispositionDeleted}
	w.Status.ObservedGeneration = w.Generation
	if err := f.r.Status().Update(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		runtimePoolReconcile(t, f.r, f.pool)
		if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pool), &corev1alpha1.RuntimePool{}); apierrors.IsNotFound(err) {
			if !reflect.DeepEqual(f.currentWorkspace(t).Spec.Workload, frozen) {
				t.Fatal("legacy finalization rewrote its immutable request")
			}
			if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(&worker), &corev1.Pod{}); err != nil {
				t.Fatal("Core deleted provider infrastructure during legacy cleanup")
			}
			secrets := &corev1.SecretList{}
			if err := f.r.List(t.Context(), secrets, client.MatchingLabels{runtimePoolUIDLabel: string(pool.UID)}); err != nil || len(secrets.Items) != 0 {
				t.Fatalf("legacy finalization retained Core runtime credentials: %v", err)
			}
			return
		}
	}
	t.Fatal("exact retired legacy native pool retained its finalizer")
}

func assertNativeLegacyCleanupRecovery(t *testing.T, f *externalRuntimePoolFixture, pool *corev1alpha1.RuntimePool, pod *corev1.Pod) {
	t.Helper()
	if observed, err := runtimePoolWorkspaceCleanupPod(t.Context(), f.r.Client, pool); err != nil || observed.UID != pod.UID {
		t.Fatalf("planned cleanup rejected its exact legacy native Pod fence: %v", err)
	}
	probe := runtimePoolValidProbe(pool, pod, pool.Status.ActiveInstance.BootID, false)
	probe.Capabilities.SupportsPublicationFinalization = true
	probe.Status.Timestamp = time.Now().UTC()
	auth := runtimePoolTestPrivateAuthSecret(t, f.r, f.pool)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		rw.Header().Set("Content-Type", "application/json")
		if request.Method != http.MethodGet {
			t.Error("legacy recovery attempted runtime mutation or creation")
		}
		switch request.URL.Path {
		case "/runtime" + harnessv2.CapabilitiesPath:
			_ = json.NewEncoder(rw).Encode(probe.Capabilities)
		case "/runtime" + harnessv2.StatusPath:
			if request.Header.Get("Authorization") != "Bearer "+string(auth.Data[runtimePoolControllerTokenKey]) {
				t.Error("legacy recovery lost exact private authentication")
			}
			_, err := harnessv2.VerifyStatusCapability(auth.Data[runtimePoolCapabilitySecretKey], request.Header.Get(harnessv2.OperationCapabilityHeader), harnessv2.StatusCapabilityBinding{RuntimeProfileDigest: probe.Status.Fence.RuntimeProfileDigest, RuntimeInstanceID: probe.Status.Fence.RuntimeInstanceID}, time.Now().UTC())
			if err != nil {
				t.Error("legacy recovery status capability lost its exact runtime fence")
			}
			_ = json.NewEncoder(rw).Encode(probe.Status)
		default:
			t.Error("legacy recovery changed the exact admitted endpoint")
			rw.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	original := http.DefaultTransport
	transport := original.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "provider-route.example:80" {
			t.Error("legacy recovery did not dial its frozen provider endpoint")
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	http.DefaultTransport = transport
	defer func() { http.DefaultTransport = original; transport.CloseIdleConnections() }()
	epochs := NewControllerEpochManager(nil, "controller")
	epochs.current = &store.ControllerEpoch{Name: store.DefaultControllerEpochName, Epoch: pool.Status.ActiveInstance.ControllerEpoch, HolderID: "controller"}
	close(epochs.ready)
	dispatcher := &ACPDispatcher{Client: f.r.Client, APIReader: f.r.Client, Epochs: epochs}
	if _, _, _, _, err := dispatcher.runtimeClient(t.Context(), acpDispatchTarget{pool: pool}, harnessv2.MCPPolicyConfiguration{}, true); err == nil || calls.Load() != 0 {
		t.Fatal("fresh admission authenticated or exposed a legacy provider fence")
	}
	task := runtimePoolRetirementTask(t, pool, "existing-legacy-task")
	if err := f.r.Create(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	originalStatus := task.Status.DeepCopy()
	ready, err := dispatcher.reconcileRecoveredRuntimeSession(t.Context(), task, task.UID, false, nil)
	if err != nil || !ready || calls.Load() != 2 {
		t.Fatalf("existing legacy cleanup recovery failed: ready=%t calls=%d err=%v", ready, calls.Load(), err)
	}
	if !reflect.DeepEqual(task.Status, *originalStatus) {
		t.Fatal("cleanup recovery restamped Task runtime identity")
	}
	changed := pool.DeepCopy()
	changed.Status.ActiveInstance.PodUID = "replacement-runtime"
	if _, err := runtimePoolWorkspaceCleanupPod(t.Context(), f.r.Client, changed); err == nil {
		t.Fatal("cleanup inherited authority from a replacement native runtime")
	}
}
