package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestUnknownReadSessionDoesNotSkipRuntimeSettlement(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "unknown-read", UID: types.UID("unknown-read-uid")},
		Spec:       corev1alpha1.TaskSpec{SessionRef: &corev1alpha1.SessionReference{Name: "conversation"}},
		Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{
			State:             corev1alpha1.TaskExecutionStateOutcomeUnknown,
			Outcome:           corev1alpha1.TaskExecutionOutcomeOutcomeUnknown,
			RuntimeSessionUID: "resident-session", RuntimeSessionGeneration: 1,
			RuntimePoolName: "bound-pool", RuntimePoolUID: "bound-pool-uid",
			RuntimeInstanceID: "runtime-instance", RuntimeSessionSupervisorBootID: "runtime-boot",
		}},
	}
	// A continued read Session may have completed natively just as the
	// controller lost its result. It is not proven settled/retired merely
	// because it did not request publication.
	readErr := errors.New("exact runtime authority read unavailable")
	dispatcher := &ACPDispatcher{APIReader: &validationRecoveryUnavailableReader{err: readErr}}
	ready, err := dispatcher.reconcileRecoveredTaskScopedRuntimeSession(context.Background(), task, task.UID, false)
	if ready || err == nil {
		t.Fatal("unknown read Session skipped exact runtime settlement authority")
	}
}

type validationRecoveryUnavailableReader struct {
	client.Reader
	err error
}

func (r *validationRecoveryUnavailableReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	return r.err
}

func TestUnknownReadSessionRemainsCleanupPending(t *testing.T) {
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{UID: "task-uid"}, Spec: corev1alpha1.TaskSpec{SessionRef: &corev1alpha1.SessionReference{Name: "conversation"}}, Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{State: corev1alpha1.TaskExecutionStateOutcomeUnknown, RuntimeSessionUID: "resident-session", RuntimeSessionGeneration: 1, RuntimeInstanceID: "old-instance", Attempt: 1}}}
	if taskScopedRuntimeSessionCleanupComplete(task) {
		t.Fatal("unknown read Session was considered reusable without exact cleanup proof")
	}
	task.Status.Execution.State = corev1alpha1.TaskExecutionStateSucceeded
	if !taskScopedRuntimeSessionCleanupComplete(task) {
		t.Fatal("successful read Session stopped retaining its conversation process")
	}
}

type validationRecoveryStore struct {
	store.DurableControlStore
	attempt       *store.PromptAttempt
	preparation   *store.ExternalEffect
	fence         store.ControllerEpochFence
	session       *store.SessionControl
	inMutation    atomic.Bool
	guardTakeover atomic.Bool
}

func (s *validationRecoveryStore) GetPromptAttempt(_ context.Context, id string) (*store.PromptAttempt, error) {
	if s.attempt == nil || id != s.attempt.ID {
		return nil, store.ErrNotFound
	}
	return s.attempt, nil
}
func (s *validationRecoveryStore) GetExternalEffect(_ context.Context, id string) (*store.ExternalEffect, error) {
	if s.preparation == nil || id != s.preparation.ID {
		return nil, store.ErrNotFound
	}
	return s.preparation, nil
}
func (s *validationRecoveryStore) GetControllerEpochFence(context.Context, string) (store.ControllerEpochFence, error) {
	return s.fence, nil
}

func validationRecoveryFixture(t *testing.T, git bool) (*ACPDispatcher, *corev1alpha1.Task, *validationRecoveryStore, harnessv2.Fence) {
	t.Helper()
	ctx := context.Background()
	task := bindingTestTask()
	task.Spec.Workspace = nil
	task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "recovery-conversation", Create: true}
	if git {
		task.Spec.Workspace = &corev1alpha1.WorkspaceConfig{GitRepo: "https://github.com/orka-agents/orka.git", Branch: "removed-branch", Intent: corev1alpha1.WorkspaceIntentWrite}
	}
	r, snapshots := newBindingTestReconciler(t, task, bindingTestNamespace())
	if _, err, handled := r.ensureAgentExecutionBinding(ctx, task, bindingTestAgent()); err != nil || handled {
		t.Fatalf("bind: %v / %v", err, handled)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(task), task); err != nil {
		t.Fatal(err)
	}
	b := task.Status.AgentExecutionBinding
	snapshot, err := snapshots.GetAgentExecutionSnapshot(ctx, store.AgentExecutionSnapshotKey{TaskUID: string(task.UID), Digest: b.Snapshot.Digest})
	if err != nil {
		t.Fatal(err)
	}
	body, err := decodeAgentExecutionSnapshot(snapshot.Body)
	if err != nil {
		t.Fatal(err)
	}
	task.Status.Execution = &corev1alpha1.TaskExecutionStatus{State: corev1alpha1.TaskExecutionStateOutcomeUnknown, Outcome: corev1alpha1.TaskExecutionOutcomeOutcomeUnknown, Attempt: 1, PromptID: "recovery-prompt", RequestDigest: testControlDigestForDispatcher("request"), RuntimePoolName: body.PoolName, RuntimePoolUID: "recovery-pool-uid", RuntimeInstanceID: "recovery-pod.recovery-boot", RuntimeSessionUID: "recovery-session", RuntimeSessionGeneration: 1, RuntimeSessionSupervisorBootID: "recovery-boot", RuntimeSessionProfileDigest: b.RuntimeProfileDigest}
	task.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{State: corev1alpha1.TaskDeliveryStateNotRequested, Outcome: corev1alpha1.TaskDeliveryOutcomeNotRequested}
	epochs := readyAgentRuntimeTestEpochManager(1)
	fence, err := epochs.CurrentFence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	control := &validationRecoveryStore{fence: fence, session: &store.SessionControl{Namespace: task.Namespace, SessionName: task.Spec.SessionRef.Name, SessionUID: task.Status.Execution.RuntimeSessionUID}}
	id, err := promptAttemptIDFromTask(task)
	if err != nil {
		t.Fatal(err)
	}
	control.attempt = &store.PromptAttempt{ID: id, Key: store.PromptAttemptKey{Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: 1, PromptID: task.Status.Execution.PromptID}, SessionUID: task.Status.Execution.RuntimeSessionUID, SessionLeaseGeneration: 1, ExecutionState: store.PromptExecutionOutcomeUnknown, DeliveryState: store.PromptDeliveryNotRequested, RequestDigest: task.Status.Execution.RequestDigest, BindingDigest: b.BindingDigest, SnapshotDigest: b.Snapshot.Digest, RuntimeInstanceID: task.Status.Execution.RuntimeInstanceID}
	var workspace harnessv2.WorkspaceSpec
	sourceRef := ""
	if git {
		identity := store.ExternalEffectIdentity{Kind: "workspace.prepare", Namespace: task.Namespace, AggregateID: string(task.UID), OperationID: "workspace-prepare-" + task.Status.Execution.PromptID}
		prepared := publisherservice.WorkspacePrepareResponse{OperationID: identity.OperationID, RepositoryID: "github:orka-agents/orka", SourceRef: "refs/heads/removed-branch", BaselineOID: strings.Repeat("a", 40), ManifestDigest: testControlDigestForDispatcher("tree"), Artifact: harnessv2.ArtifactReference{ArtifactID: "original-workspace", Digest: testControlDigestForDispatcher("artifact"), SizeBytes: 1, MediaType: "application/tar"}}
		data, err := json.Marshal(prepared)
		if err != nil {
			t.Fatal(err)
		}
		effectID, err := identity.CanonicalID()
		if err != nil {
			t.Fatal(err)
		}
		control.preparation = &store.ExternalEffect{ID: effectID, Identity: identity, State: store.ExternalEffectSucceeded, Response: data, ResponseDigest: store.CanonicalBytesDigest(data)}
		workspace = harnessv2.WorkspaceSpec{Intent: harnessv2.WorkspaceIntentWrite, Baseline: harnessv2.WorkspaceBaseline{RepositoryIdentity: prepared.RepositoryID, Revision: prepared.BaselineOID, TreeDigest: prepared.ManifestDigest, Artifact: &prepared.Artifact}}
		sourceRef = prepared.SourceRef
	} else {
		_, workspace, err = emptyRuntimeWorkspace(task, task.Status.Execution.RuntimeSessionUID)
		if err != nil {
			t.Fatal(err)
		}
	}
	task.Status.Execution.RuntimeSessionWorkspaceDigest, err = acpRuntimeWorkspaceBindingDigest(sourceRef, workspace)
	if err != nil {
		t.Fatal(err)
	}
	runtimeFence := harnessv2.Fence{RuntimePoolUID: harnessv2.RuntimePoolUID(task.Status.Execution.RuntimePoolUID), RuntimePoolGeneration: 1, RuntimeInstanceID: harnessv2.RuntimeInstanceID(task.Status.Execution.RuntimeInstanceID), SupervisorBootID: "recovery-boot", ControllerEpoch: 1, RuntimeProfileDigest: harnessv2.ProfileDigest(b.RuntimeProfileDigest), ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion, RuntimeSessionUID: "recovery-session", RuntimeSessionGeneration: 1}
	return &ACPDispatcher{Client: r.Client, APIReader: r.Client, Store: control, Snapshots: snapshots, Epochs: epochs}, task, control, runtimeFence
}

func TestRecoveredValidationWorkspaceUsesOnlyFrozenEvidence(t *testing.T) {
	for _, git := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "git"}[git], func(t *testing.T) {
			d, task, _, _ := validationRecoveryFixture(t, git)
			// A later input edit must never retarget a pending validation or refetch a
			// branch. There is deliberately no Publisher/credential resolver here.
			task.Spec.Workspace = &corev1alpha1.WorkspaceConfig{GitRepo: "https://github.com/unrelated/repository.git", Intent: corev1alpha1.WorkspaceIntentRead, SubPath: "different"}
			baseline, workspace, err := d.recoveredValidationWorkspace(t.Context(), task, task.UID)
			if err != nil {
				t.Fatal(err)
			}
			if git && (baseline.RepositoryIdentity != "github:orka-agents/orka" || workspace.Intent != harnessv2.WorkspaceIntentWrite) {
				t.Fatalf("workspace drifted: %#v", workspace)
			}
			if !git && baseline.RepositoryIdentity != acpNoWorkspaceRevision+":recovery-session" {
				t.Fatalf("empty baseline changed: %q", baseline.RepositoryIdentity)
			}
		})
	}
}

func TestRecoveredValidationWorkspaceRejectsMissingOrChangedEvidence(t *testing.T) {
	for _, name := range []string{"missing preparation", "response digest", "effect identity", "workspace digest", "binding digest", "wrong task UID"} {
		t.Run(name, func(t *testing.T) {
			d, task, control, _ := validationRecoveryFixture(t, true)
			uid := task.UID
			switch name {
			case "missing preparation":
				control.preparation = nil
			case "response digest":
				control.preparation.ResponseDigest = testControlDigestForDispatcher("other")
			case "effect identity":
				control.preparation.Identity.AggregateID = "other"
			case "workspace digest":
				task.Status.Execution.RuntimeSessionWorkspaceDigest = testControlDigestForDispatcher("other")
			case "binding digest":
				task.Status.AgentExecutionBinding.BindingDigest = testControlDigestForDispatcher("other")
			case "wrong task UID":
				uid = "other"
			}
			if _, _, err := d.recoveredValidationWorkspace(t.Context(), task, uid); err == nil {
				t.Fatal("accepted invalid immutable workspace evidence")
			}
		})
	}
}

func TestRecoverAbandonedValidationPreservesOutcomeAndDoesNotPublish(t *testing.T) {
	for _, scenario := range []string{"read", "write-empty", "write-prepared"} {
		t.Run(scenario, func(t *testing.T) {
			d, task, control, fence := validationRecoveryFixture(t, scenario != "read")
			baseline, _, baselineErr := d.recoveredValidationWorkspace(t.Context(), task, task.UID)
			if baselineErr != nil {
				t.Fatal(baselineErr)
			}
			calls := []string{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == harnessv2.StatusPath {
					statusFence := fence
					statusFence.RuntimeSessionUID = ""
					statusFence.RuntimeSessionGeneration = 0
					writeDispatcherJSON(w, harnessv2.StatusResponse{Protocol: harnessv2.ProtocolVersion, Fence: statusFence, Timestamp: time.Now().UTC(), Lifecycle: harnessv2.SupervisorLifecycleReady, Drain: harnessv2.DrainStatus{AcceptingNewSessions: true}})
					return
				}
				if !control.inMutation.Load() {
					t.Error("validation mutation escaped the current-owner interlock")
				}
				calls = append(calls, r.Method+" "+r.URL.Path)
				if strings.HasSuffix(r.URL.Path, "/cancel") {
					var request harnessv2.CancelPromptRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					if request.Metadata.TaskUID != harnessv2.TaskUID(task.UID) || request.Metadata.PromptID != harnessv2.PromptID(task.Status.Execution.PromptID) {
						t.Error("cancel lost exact Task/prompt authority")
					}
					writeDispatcherJSON(w, harnessv2.CancelPromptResponse{Protocol: harnessv2.ProtocolVersion, Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationSettled, Phase: harnessv2.OperationPhaseSettled, TerminalEvent: harnessv2.EventCompleted}, BarrierState: harnessv2.CancellationBarrierSettled, SettlementProven: true, Settlement: harnessv2.PromptSettlement{TerminalEvent: harnessv2.EventCompleted, Outcome: harnessv2.PromptOutcomeSucceeded, StopReason: harnessv2.ACPStopReasonEndTurn, SettledAt: time.Now().UTC()}})
					return
				}
				if strings.Contains(r.URL.Path, "/workspace-deltas/") {
					var request harnessv2.CreateWorkspaceDeltaRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					delta := harnessv2.WorkspaceDeltaDescriptor{DeltaID: request.DeltaID, RuntimeSessionUID: request.Metadata.Fence.RuntimeSessionUID, SessionGeneration: request.Metadata.Fence.RuntimeSessionGeneration, State: harnessv2.WorkspaceDeltaNoChange, Intent: request.Intent, VerifiedBaseline: request.VerifiedBaseline, NoFollowVerified: true, PublicationSafe: true, FrozenAt: time.Now().UTC()}
					if scenario == "write-prepared" {
						delta.State = harnessv2.WorkspaceDeltaPrepared
						delta.EntryCount = 1
						delta.ChangedFileCount = 1
						delta.ManifestDigest = testControlDigestForDispatcher("delta-manifest")
						delta.Artifact = &harnessv2.ArtifactReference{ArtifactID: "unexported-delta", Digest: testControlDigestForDispatcher("delta-artifact"), SizeBytes: 1, MediaType: "application/tar"}
					}
					writeDispatcherJSON(w, harnessv2.CreateWorkspaceDeltaResponse{Protocol: harnessv2.ProtocolVersion, Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh}, Delta: delta})
					return
				}
				if strings.HasSuffix(r.URL.Path, "/publication-finalization") {
					var request harnessv2.FinalizeRuntimeSessionPublicationRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					if scenario != "write-prepared" || request.TerminalState != harnessv2.PublicationTerminalDeliveryConflict {
						t.Error("abandonment claimed successful publication")
					}
					now := time.Now().UTC()
					writeDispatcherJSON(w, harnessv2.FinalizeRuntimeSessionPublicationResponse{
						Protocol: harnessv2.ProtocolVersion, Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh},
						Session:      harnessv2.RuntimeSessionDescriptor{RuntimeSessionID: harnessv2.RuntimeSessionID(runtimeSessionID(request.Metadata.Fence)), RuntimeSessionUID: request.Metadata.Fence.RuntimeSessionUID, Generation: request.Metadata.Fence.RuntimeSessionGeneration, RuntimeInstanceID: request.Metadata.Fence.RuntimeInstanceID, SupervisorBootID: request.Metadata.Fence.SupervisorBootID, RuntimeProfileDigest: request.Metadata.Fence.RuntimeProfileDigest, State: harnessv2.RuntimeSessionStateFinalizing, ProviderSessionID: "fixture-provider", WorkspaceBaseline: baseline, CreatedAt: now.Add(-time.Minute), LastTransitionAt: now},
						Finalization: harnessv2.PublicationFinalizationReceipt{WorkspaceDeltaID: request.WorkspaceDeltaID, PublicationID: request.PublicationID, PublicationGeneration: request.PublicationGeneration, PublicationVersion: request.PublicationVersion, TerminalState: request.TerminalState, TerminalReceiptDigest: request.TerminalReceiptDigest, AppliedAt: now},
					})
					return
				}
				t.Errorf("unexpected mutation %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}))
			defer server.Close()
			pool := validationRecoveryPoolForServer(t, d, task, server.URL)
			if err := d.recoverAbandonedWorkspaceValidation(t.Context(), task, task.UID, pool); err != nil {
				t.Fatal(err)
			}
			wantCalls := 2
			if scenario == "write-prepared" {
				wantCalls = 3
			}
			if len(calls) != wantCalls {
				t.Fatalf("calls=%v", calls)
			}
			if task.Status.Execution.State != corev1alpha1.TaskExecutionStateOutcomeUnknown || control.attempt.ExecutionState != store.PromptExecutionOutcomeUnknown || task.Status.Execution.RuntimeSessionCleanupDigest != "" {
				t.Fatal("validation forged success or runtime retirement proof")
			}
		})
	}
}

func TestRecoverAbandonedValidationRejectsChangedTerminalAuthority(t *testing.T) {
	for _, change := range []string{"successful attempt", "publication in flight", "different session", "different instance", "different boot", "different runtime profile", "future epoch", "changed snapshot"} {
		t.Run(change, func(t *testing.T) {
			d, task, control, fence := validationRecoveryFixture(t, false)
			switch change {
			case "successful attempt":
				control.attempt.ExecutionState = store.PromptExecutionSucceeded
			case "publication in flight":
				control.attempt.DeliveryState = store.PromptDeliveryPublishing
			case "different session":
				control.attempt.SessionUID = "other"
			case "different instance":
				fence.RuntimeInstanceID = "other"
			case "different boot":
				fence.SupervisorBootID = "other"
			case "different runtime profile":
				fence.RuntimeProfileDigest = harnessv2.ProfileDigest(testControlDigestForDispatcher("other"))
			case "future epoch":
				fence.ControllerEpoch = 2
			case "changed snapshot":
				control.attempt.SnapshotDigest = testControlDigestForDispatcher("other")
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(500) }))
			defer server.Close()
			runtimeClient, err := harnessv2.NewClient(server.URL, harnessv2.WithControllerBearerToken(strings.Repeat("fixture-only-", 3)), harnessv2.WithOperationCapabilitySecret(bytes.Repeat([]byte{0x46}, 32)))
			if err != nil {
				t.Fatal(err)
			}
			if err := d.finishAbandonedWorkspaceValidation(t.Context(), task, task.UID, runtimeClient, fence); err == nil || calls != 0 {
				t.Fatalf("changed authority: calls=%d err=%v", calls, err)
			}
		})
	}
}

func (s *validationRecoveryStore) GetSessionControl(context.Context, string, string) (*store.SessionControl, error) {
	if s.session == nil {
		return nil, store.ErrNotFound
	}
	return s.session, nil
}
func (s *validationRecoveryStore) WithControllerEpochMutation(ctx context.Context, fence store.ControllerEpochFence, fn func(context.Context) error) error {
	if s.guardTakeover.Load() {
		s.fence.Epoch++
	}
	if fence != s.fence {
		return store.ErrConflict
	}
	s.inMutation.Store(true)
	defer s.inMutation.Store(false)
	return fn(ctx)
}

func TestRecoverAbandonedValidationRetriesLostCancellationWithFreshIdentity(t *testing.T) {
	d, task, _, fence := validationRecoveryFixture(t, false)
	operations := map[harnessv2.OperationID]bool{}
	cancelCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			var request harnessv2.CancelPromptRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			if operations[request.Metadata.OperationID] {
				t.Error("recovery reused an operation ID with freshly sealed timing")
				w.WriteHeader(409)
				return
			}
			operations[request.Metadata.OperationID] = true
			cancelCalls++
			if cancelCalls == 1 {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"protocol":`))
				return
			}
			writeDispatcherJSON(w, harnessv2.CancelPromptResponse{Protocol: harnessv2.ProtocolVersion, Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationSettled, Phase: harnessv2.OperationPhaseSettled, TerminalEvent: harnessv2.EventCompleted}, BarrierState: harnessv2.CancellationBarrierSettled, SettlementProven: true, Settlement: harnessv2.PromptSettlement{TerminalEvent: harnessv2.EventCompleted, Outcome: harnessv2.PromptOutcomeSucceeded, StopReason: harnessv2.ACPStopReasonEndTurn, SettledAt: time.Now().UTC()}})
			return
		}
		var request harnessv2.CreateWorkspaceDeltaRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		writeDispatcherJSON(w, harnessv2.CreateWorkspaceDeltaResponse{Protocol: harnessv2.ProtocolVersion, Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh}, Delta: harnessv2.WorkspaceDeltaDescriptor{DeltaID: request.DeltaID, RuntimeSessionUID: request.Metadata.Fence.RuntimeSessionUID, SessionGeneration: request.Metadata.Fence.RuntimeSessionGeneration, State: harnessv2.WorkspaceDeltaNoChange, Intent: request.Intent, VerifiedBaseline: request.VerifiedBaseline, NoFollowVerified: true, PublicationSafe: true, FrozenAt: time.Now().UTC()}})
	}))
	defer server.Close()
	runtimeClient, err := harnessv2.NewClient(server.URL, harnessv2.WithControllerBearerToken(strings.Repeat("fixture-only-", 3)), harnessv2.WithOperationCapabilitySecret(bytes.Repeat([]byte{0x56}, 32)))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.finishAbandonedWorkspaceValidation(t.Context(), task, task.UID, runtimeClient, fence); err == nil {
		t.Fatal("lost cancellation response was accepted")
	}
	if err := d.finishAbandonedWorkspaceValidation(t.Context(), task, task.UID, runtimeClient, fence); err != nil {
		t.Fatal(err)
	}
	if cancelCalls != 2 || len(operations) != 2 {
		t.Fatalf("cancelCalls=%d identities=%d", cancelCalls, len(operations))
	}
}

func validationRecoveryPoolForServer(t *testing.T, d *ACPDispatcher, task *corev1alpha1.Task, endpoint string) *corev1alpha1.RuntimePool {
	t.Helper()
	address, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Client.Status().Update(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	pool := &corev1alpha1.RuntimePool{ObjectMeta: metav1.ObjectMeta{Namespace: task.Namespace, Name: task.Status.Execution.RuntimePoolName, UID: "recovery-pool-uid", Generation: 1}, Spec: corev1alpha1.RuntimePoolSpec{RuntimeNamespace: "orka-runtimes", Runtime: corev1alpha1.RuntimePoolRuntimeSpec{Profile: corev1alpha1.RuntimePoolProfileSpec{Digest: task.Status.AgentExecutionBinding.RuntimeProfileDigest}}}, Status: corev1alpha1.RuntimePoolStatus{ActiveInstance: &corev1alpha1.RuntimePoolActiveInstanceStatus{PodNamespace: "orka-runtimes", PodName: "resident", PodUID: "recovery-pod", PodAddress: address.Host, RuntimeInstanceID: task.Status.Execution.RuntimeInstanceID, BootID: "recovery-boot", ControllerEpoch: 1, ProfileDigest: task.Status.AgentExecutionBinding.RuntimeProfileDigest}}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "orka-runtimes", Name: "resident", UID: "recovery-pod", Labels: map[string]string{runtimePoolUIDLabel: string(pool.UID), runtimePoolNameLabel: pool.Name, runtimePoolNamespaceLabel: pool.Namespace}}, Status: corev1.PodStatus{PodIP: address.Hostname()}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "orka-runtimes", Name: "recovery-auth-e1", UID: "recovery-auth-uid", Labels: map[string]string{runtimePoolAuthLabel: "true", runtimePoolUIDLabel: string(pool.UID)}}, Data: map[string][]byte{runtimePoolControllerTokenKey: []byte(strings.Repeat("fixture-token-", 3)), runtimePoolCapabilitySecretKey: []byte(strings.Repeat("fixture-key-", 3))}}
	for _, object := range []client.Object{pool, pod, secret} {
		if err := d.Client.Create(t.Context(), object); err != nil {
			t.Fatal(err)
		}
	}
	return pool
}

func TestCurrentEpochAbandonedValidationRejectsTakeoverBeforeSend(t *testing.T) {
	d, task, control, fence := validationRecoveryFixture(t, false)
	mutations := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == harnessv2.StatusPath {
			statusFence := fence
			statusFence.RuntimeSessionUID = ""
			statusFence.RuntimeSessionGeneration = 0
			control.guardTakeover.Store(true)
			writeDispatcherJSON(w, harnessv2.StatusResponse{Protocol: harnessv2.ProtocolVersion, Fence: statusFence, Timestamp: time.Now().UTC(), Lifecycle: harnessv2.SupervisorLifecycleReady, Drain: harnessv2.DrainStatus{AcceptingNewSessions: true}})
			return
		}
		mutations++
		w.WriteHeader(500)
	}))
	defer server.Close()
	pool := validationRecoveryPoolForServer(t, d, task, server.URL)
	if err := d.recoverAbandonedWorkspaceValidation(t.Context(), task, task.UID, pool); err == nil || mutations != 0 {
		t.Fatalf("takeover validation: mutations=%d err=%v", mutations, err)
	}
}

func TestRecoveredValidationAcceptsKubernetesJSONReordering(t *testing.T) {
	d, task, control, _ := validationRecoveryFixture(t, true)
	var reordered map[string]any
	if err := json.Unmarshal(control.preparation.Response, &reordered); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(reordered)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(encoded, control.preparation.Response) {
		t.Fatal("fixture did not change producer field order")
	}
	control.preparation.Response = encoded
	if _, _, err := d.recoveredValidationWorkspace(t.Context(), task, task.UID); err != nil {
		t.Fatalf("Kubernetes reordered intact preparation JSON: %v", err)
	}
	for _, name := range []string{"changed value", "unknown field", "trailing value"} {
		t.Run(name, func(t *testing.T) {
			copyDispatcher, copyTask, copyControl, _ := validationRecoveryFixture(t, true)
			var changed map[string]any
			if err := json.Unmarshal(encoded, &changed); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "changed value":
				changed["baselineOid"] = strings.Repeat("b", 40)
			case "unknown field":
				changed["extraEvidence"] = "not in original producer schema"
			}
			body, err := json.Marshal(changed)
			if err != nil {
				t.Fatal(err)
			}
			if name == "trailing value" {
				body = append(body, []byte(" {}")...)
			}
			copyControl.preparation.Response = body
			if _, _, err := copyDispatcher.recoveredValidationWorkspace(t.Context(), copyTask, copyTask.UID); err == nil {
				t.Fatal("accepted changed preparation evidence")
			}
		})
	}
}
