/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/taskwait"
)

// A write turn publishes through the dispatcher and parks. Its continuation
// then dispatches through the same dispatcher: Session planning loads the
// published commit as the verified baseline, and workspace preparation and
// publication must start from it instead of failing on the parked Task's
// pre-publication expectedRemoteSHA.
//
//nolint:gocyclo // One end-to-end flow: publish, park, resume, and dispatch the continuation.
func TestParkedPublishedWriteTurnContinuationDispatchesFromTheSessionBaseline(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	baselineOID := strings.Repeat("1", 40)
	treeOID := strings.Repeat("2", 40)
	commitOID := strings.Repeat("3", 40)
	bundleDigest := "sha256:" + strings.Repeat("4", 64)

	taskUID := types.UID("13131313-1313-1313-1313-131313131313")
	promptID := "prompt-" + string(taskUID) + "-1"
	workspace := &corev1alpha1.WorkspaceConfig{
		Intent: corev1alpha1.WorkspaceIntentWrite, GitRepo: "https://github.com/orka-agents/orka.git", Ref: defaultACPSourceBranch,
		ReadCredentialRef:            &corev1alpha1.WorkspaceCredentialReference{Name: "workspace-credential"},
		PublicationGitRepo:           "https://github.com/orka-agents/orka.git",
		PublicationReadCredentialRef: &corev1alpha1.WorkspaceCredentialReference{Name: "workspace-credential"},
		PublicationCredentialRef:     &corev1alpha1.WorkspaceCredentialReference{Name: "workspace-credential"},
		ForgeCredentialRef:           &corev1alpha1.WorkspaceCredentialReference{Name: "workspace-credential"},
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "parked-write", UID: taskUID, Labels: map[string]string{acpRuntimeTaskPoolLabel: "pool"}},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, Prompt: "update the workspace, then wait", AgentRuntime: &corev1alpha1.AgentRuntimeSpec{},
			AgentRef:   &corev1alpha1.AgentReference{Name: "agent"},
			Timeout:    &metav1.Duration{Duration: time.Hour},
			SessionRef: &corev1alpha1.SessionReference{Name: "parked-write-session", Create: true, Append: true},
			Workspace:  workspace,
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending, Attempts: 1, Execution: &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStateQueued, Attempt: 1, PromptID: promptID, RuntimePoolName: "pool", RuntimePoolUID: "pool-uid",
			RequestDigest: testControlDigestForDispatcher("parked-write-request"), ControllerEpoch: 1,
		}},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "agent", UID: types.UID("agent-uid"), Generation: 1},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{Name: acpTestModel},
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type: corev1alpha1.AgentRuntimeCodex, ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2),
			},
		},
	}
	images := ACPRuntimeImages{Codex: "docker.io/example/acp@sha256:" + strings.Repeat("a", 64)}
	plan := frozenACPDispatcherPlanForTest(t, task, agent, images)
	task.Labels[acpRuntimeTaskPoolLabel] = plan.PoolName
	task.Status.Execution.RuntimePoolName = plan.PoolName

	var creationPending atomic.Bool
	runtimeServer := newDispatcherWriteRuntimeServer(t, plan.Profile, plan.Digest, &creationPending,
		func(harnessv2.FinalizeRuntimeSessionPublicationRequest) {}, func(harnessv2.DeleteRuntimeSessionRequest) {})
	defer runtimeServer.Close()
	parsed, err := url.Parse(runtimeServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	pool := &corev1alpha1.RuntimePool{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: plan.PoolName, UID: types.UID("pool-uid"), Generation: 1},
		Spec: corev1alpha1.RuntimePoolSpec{
			RuntimeNamespace: "orka-runtimes", Runtime: corev1alpha1.RuntimePoolRuntimeSpec{
				Image: plan.Image, Profile: RuntimePoolProfileFromPlan(plan),
			}, DesiredReplicas: 1,
		},
		Status: corev1alpha1.RuntimePoolStatus{
			Lifecycle: corev1alpha1.RuntimePoolLifecycleServing, AdmissionState: corev1alpha1.RuntimePoolAdmissionAccepting,
			ActiveInstance: &corev1alpha1.RuntimePoolActiveInstanceStatus{
				PodNamespace: "orka-runtimes", PodName: "runtime-pod", PodAddress: parsed.Host,
				PodUID: "pod-uid", BootID: "boot-id", RuntimeInstanceID: "pod-uid.boot-id", ControllerEpoch: 1,
				ProtocolVersion: corev1alpha1.RuntimePoolProtocolHarnessV2, ProfileDigest: string(plan.Digest),
				ProfileDigestSchemaVersion: strconv.FormatUint(uint64(harnessv2.ProfileDigestSchemaVersion), 10),
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "orka-runtimes", Name: "pool-auth-e1", Labels: map[string]string{
			runtimePoolAuthLabel: "true", runtimePoolUIDLabel: string(pool.UID),
		}},
		Data: map[string][]byte{runtimePoolControllerTokenKey: []byte(strings.Repeat("t", 32)), runtimePoolCapabilitySecretKey: []byte(strings.Repeat("s", 32))},
	}
	workspaceCredential := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "workspace-credential", UID: types.UID("workspace-credential-uid"), ResourceVersion: "1"},
		Data:       map[string][]byte{defaultACPWorkspaceCredentialKey: []byte("test-token")},
	}
	setCredentialVersions := func(execution *corev1alpha1.TaskExecutionStatus) {
		execution.ReadCredentialResourceVersion = workspaceCredential.ResourceVersion
		execution.PublicationReadCredentialResourceVersion = workspaceCredential.ResourceVersion
		execution.PublicationCredentialResourceVersion = workspaceCredential.ResourceVersion
		execution.ForgeCredentialResourceVersion = workspaceCredential.ResourceVersion
	}
	setCredentialVersions(task.Status.Execution)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}, &corev1alpha1.RuntimePool{}).
		WithObjects(task, pool, secret, workspaceCredential, agent).Build()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "parked-write.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck
	controlStore := sealedStore(t, sqlite.NewStore(db, "test"))
	epochs := NewControllerEpochManager(controlStore, "controller-parked-write")
	epochCtx, cancelEpoch := context.WithCancel(context.Background())
	epochDone := make(chan error, 1)
	go func() { epochDone <- epochs.Start(epochCtx) }()
	defer func() {
		cancelEpoch()
		<-epochDone
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	fence, err := epochs.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	createAttempt := func(bound *corev1alpha1.Task) {
		t.Helper()
		key := store.PromptAttemptKey{
			Namespace: bound.Namespace, TaskUID: string(bound.UID), Attempt: int64(bound.Status.Execution.Attempt), PromptID: bound.Status.Execution.PromptID,
		}
		attemptID, err := key.CanonicalID()
		if err != nil {
			t.Fatal(err)
		}
		bindings := make([]store.PromptCredentialBinding, 0, 4)
		for _, role := range []store.PromptCredentialRole{
			store.PromptCredentialSourceRead, store.PromptCredentialTargetRead, store.PromptCredentialTargetWrite, store.PromptCredentialForge,
		} {
			bindings = append(bindings, store.PromptCredentialBinding{
				Role: role, Namespace: bound.Namespace, SecretName: workspaceCredential.Name, SecretKey: defaultACPWorkspaceCredentialKey,
				SecretUID: string(workspaceCredential.UID), ResourceVersion: workspaceCredential.ResourceVersion,
			})
		}
		if _, err := controlStore.CreatePromptAttempt(ctx, boundPromptAttemptForTest(&store.PromptAttempt{
			ID: attemptID, Key: key, RequestDigest: bound.Status.Execution.RequestDigest,
			BindingDigest: bound.Status.AgentExecutionBinding.BindingDigest, SnapshotDigest: bound.Status.AgentExecutionBinding.Snapshot.Digest,
			CredentialBindings: bindings, ExecutionState: store.PromptExecutionQueued, DeliveryState: store.PromptDeliveryNotRequested,
		}), fence); err != nil {
			t.Fatal(err)
		}
	}
	continuity, err := NewACPSessionContinuity(ACPSessionContinuityConfig{
		SessionControls: controlStore, Transcripts: controlStore, Publications: controlStore, BranchClaims: controlStore,
	})
	if err != nil {
		t.Fatal(err)
	}
	var resolvedOID atomic.Value
	resolvedOID.Store(baselineOID)
	publisherServer := newDispatcherPublisherServer(t, treeOID, commitOID, bundleDigest, dispatcherPublisherServerOptions{
		resolvedBaselineOID: func() string { return resolvedOID.Load().(string) },
	})
	defer publisherServer.Close()
	publisherClient, err := publisherservice.NewClient(publisherservice.ClientConfig{
		BaseURL: publisherServer.URL, BearerToken: []byte(strings.Repeat("b", 32)),
		CapabilitySecret: []byte(strings.Repeat("c", 32)), HTTPClient: publisherServer.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := &ACPDispatcher{
		Client: kubeClient, APIReader: kubeClient, Store: controlStore, ResultStore: controlStore, Epochs: epochs,
		Snapshots: controlStore, Sessions: continuity, Publisher: publisherClient,
		ArtifactCapabilitySecret: []byte(strings.Repeat("d", 32)), ArtifactReservations: acceptingArtifactReservations{},
	}

	// Turn 1 publishes the branch through the dispatcher.
	bound := prepareBoundACPDispatcherTaskForTest(t, ctx, kubeClient, scheme, controlStore, task, agent, images)
	createAttempt(bound)
	dispatchQueuedTask(ctx, t, dispatcher, bound.DeepCopy())
	parked := &corev1alpha1.Task{}
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(task), parked); err != nil {
		t.Fatal(err)
	}
	if parked.Status.Phase != corev1alpha1.TaskPhaseSucceeded || parked.Status.Delivery == nil ||
		parked.Status.Delivery.Outcome != corev1alpha1.TaskDeliveryOutcomeVerifiedExact || parked.Status.Delivery.VerifiedRemoteSHA != commitOID {
		t.Fatalf("the write turn must publish %s, got status=%#v delivery=%#v", commitOID, parked.Status, parked.Status.Delivery)
	}
	// The turn requested its own precondition before publishing, and parked.
	parked.Spec.Workspace.ExpectedRemoteSHA = baselineOID
	parked.Status.Wait = &corev1alpha1.TaskWaitStatus{
		State: corev1alpha1.TaskWaitStateResuming, Kind: corev1alpha1.TaskWaitKindTimer,
		Attempt: parked.Status.Execution.Attempt, PromptID: parked.Status.Execution.PromptID,
		ContinuationTaskName: taskwait.ContinuationTaskName(parked), Message: "The requested backoff period elapsed.",
	}

	// The continuation the wait controller creates for it, linked as resumed.
	continuation := buildTaskContinuation(parked, parked.Status.Wait.ContinuationTaskName, 30*time.Minute, time.Now().Add(30*time.Minute))
	continuation.UID = types.UID("14141414-1414-1414-1414-141414141414")
	continuation.Finalizers = nil
	continuation.Labels[acpRuntimeTaskPoolLabel] = plan.PoolName
	continuationPromptID := "prompt-" + string(continuation.UID) + "-1"
	continuation.Status = corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending, Attempts: 1, Execution: &corev1alpha1.TaskExecutionStatus{
		State: corev1alpha1.TaskExecutionStateQueued, Attempt: 1, PromptID: continuationPromptID, RuntimePoolName: plan.PoolName, RuntimePoolUID: "pool-uid",
		RequestDigest: testControlDigestForDispatcher("continuation-request"), ControllerEpoch: 1,
	}}
	setCredentialVersions(continuation.Status.Execution)
	created := continuation.DeepCopy()
	if err := kubeClient.Create(ctx, created); err != nil {
		t.Fatal(err)
	}
	created.Status = continuation.Status
	if err := kubeClient.Status().Update(ctx, created); err != nil {
		t.Fatal(err)
	}
	continuation = created
	// The branch now sits at the published commit.
	resolvedOID.Store(commitOID)
	boundContinuation := prepareBoundACPDispatcherTaskForTest(t, ctx, kubeClient, scheme, controlStore, continuation, agent, images)
	createAttempt(boundContinuation)
	dispatchQueuedTask(ctx, t, dispatcher, boundContinuation.DeepCopy())
	resumed := &corev1alpha1.Task{}
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(continuation), resumed); err != nil {
		t.Fatal(err)
	}
	if resumed.Status.Execution == nil || resumed.Status.Execution.Reason == "WorkspaceUnsupported" {
		t.Fatalf("the continuation must pass workspace preparation, got %#v", resumed.Status.Execution)
	}
	if resumed.Status.Phase != corev1alpha1.TaskPhaseSucceeded || resumed.Status.Delivery == nil ||
		resumed.Status.Delivery.Outcome != corev1alpha1.TaskDeliveryOutcomeVerifiedExact || resumed.Status.Delivery.StartingSHA != commitOID {
		t.Fatalf("the continuation must publish from the published commit, got phase=%s delivery=%#v", resumed.Status.Phase, resumed.Status.Delivery)
	}
}
