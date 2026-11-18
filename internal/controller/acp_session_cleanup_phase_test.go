package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestSessionCleanupPoolRejectsTakeoverDuringStatus(t *testing.T) {
	fixture := newExternalACPDispatchFixture(t)
	owner, err := fixture.epochs.CurrentFence(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	profile := harnessProfileForTest()
	profile.Model = acpTestModel
	profileDigest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	var statusCalls, deleteCalls atomic.Int32
	advanceResult := make(chan error, 1)
	runtimeFence := harnessv2.Fence{
		RuntimeInstanceID: "pool-pod.pool-boot", SupervisorBootID: "pool-boot", ControllerEpoch: uint64(owner.Epoch),
		RuntimePoolUID: "phase-pool-uid", RuntimePoolGeneration: 1, RuntimeProfileDigest: profileDigest,
		ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == harnessv2.CapabilitiesPath:
			writeDispatcherJSON(w, harnessv2.CapabilitiesResponse{
				Protocol: harnessv2.ProtocolVersion, Transport: "http+ndjson", ACPVersion: harnessv2.ACPProfileV1,
				RuntimeProfileDigest: profileDigest, ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
				AdapterDigests: profile.AdapterDigests, Limits: harnessv2.DefaultProtocolLimits(),
				Provider:            harnessv2.ProviderCapabilities{ProviderKinds: []string{profile.ProviderKind}, Models: []string{profile.Model}, SupportsCancel: true, SupportsPermissions: true, SupportsTools: true},
				WorkspaceGovernance: harnessv2.StrictWorkspaceGovernanceCapabilities(), SupportsDrain: true,
			})
		case r.Method == http.MethodGet && r.URL.Path == harnessv2.StatusPath:
			if statusCalls.Add(1) == 1 {
				current, advanceErr := fixture.controlStore.GetControllerEpoch(fixture.ctx, owner.Name)
				if advanceErr == nil {
					holder := "pool-cleanup-successor"
					_, advanceErr = fixture.controlStore.CompareAndSwapControllerEpoch(fixture.ctx, store.ControllerEpochCAS{
						Name: current.Name, ExpectedVersion: current.Version, ExpectedEpoch: current.Epoch, NewEpoch: current.Epoch + 1,
						HolderID: holder, UpdatedAt: time.Now().UTC(),
						RequestDigest: controllerEpochDigest(current.Name, holder, current.Version, current.Epoch, current.Epoch+1),
					})
				}
				advanceResult <- advanceErr
			}
			now := time.Now().UTC()
			status := harnessv2.StatusResponse{
				Protocol: harnessv2.ProtocolVersion, Fence: runtimeFence, Timestamp: now,
				Lifecycle: harnessv2.SupervisorLifecycleReady, Drain: harnessv2.DrainStatus{AcceptingNewSessions: true},
			}
			if deleteCalls.Load() == 0 {
				status.Sessions = []harnessv2.RuntimeSessionStatus{{
					RuntimeSessionID: "phase-runtime-session", RuntimeSessionUID: "phase-runtime-session-uid",
					Generation: 1, State: harnessv2.RuntimeSessionStateIdle, LastTransitionAt: now,
				}}
				status.Pressure.ResidentSessions = 1
			}
			writeDispatcherJSON(w, status)
		case r.Method == http.MethodDelete:
			deleteCalls.Add(1)
			var request harnessv2.DeleteRuntimeSessionRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode runtime DELETE: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			writeDispatcherJSON(w, harnessv2.DeleteRuntimeSessionResponse{
				Protocol: harnessv2.ProtocolVersion, State: harnessv2.RuntimeSessionStateDeleted,
				Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh},
				Tombstone:      testDeleteTombstone(request, time.Now().UTC()),
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: defaultNS, Name: "phase-pool-task", UID: types.UID("phase-pool-task-uid")},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, SessionRef: &corev1alpha1.SessionReference{Name: "phase-conversation"}},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded, Attempts: 1,
			Execution: &corev1alpha1.TaskExecutionStatus{
				State: corev1alpha1.TaskExecutionStateSucceeded, Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded,
				Attempt: 1, PromptID: "phase-pool-prompt", RequestDigest: testControlDigestForDispatcher("phase-pool-prompt"),
				RuntimePoolName: "phase-pool", RuntimePoolUID: string(runtimeFence.RuntimePoolUID), ControllerEpoch: owner.Epoch,
				RuntimeInstanceID: string(runtimeFence.RuntimeInstanceID), RuntimeSessionSupervisorBootID: string(runtimeFence.SupervisorBootID),
				RuntimeSessionProfileDigest: string(profileDigest), RuntimeSessionUID: "phase-runtime-session-uid", RuntimeSessionGeneration: 1,
			},
		},
	}
	pool := &corev1alpha1.RuntimePool{
		ObjectMeta: metav1.ObjectMeta{Namespace: defaultNS, Name: "phase-pool", UID: types.UID(runtimeFence.RuntimePoolUID), Generation: 1},
		Spec: corev1alpha1.RuntimePoolSpec{RuntimeNamespace: "orka-runtimes", Runtime: corev1alpha1.RuntimePoolRuntimeSpec{
			Image:   "docker.io/example/acp@sha256:" + strings.Repeat("a", 64),
			Profile: RuntimePoolProfileFromPlan(ACPRuntimePlan{Profile: profile, Digest: profileDigest}),
		}},
		Status: corev1alpha1.RuntimePoolStatus{ActiveInstance: &corev1alpha1.RuntimePoolActiveInstanceStatus{
			PodNamespace: "orka-runtimes", PodName: "phase-pool-pod", PodUID: "pool-pod", PodAddress: endpoint.Host,
			RuntimeInstanceID: string(runtimeFence.RuntimeInstanceID), BootID: string(runtimeFence.SupervisorBootID),
			ControllerEpoch: owner.Epoch, ProfileDigest: string(profileDigest),
		}},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "orka-runtimes", Name: fmt.Sprintf("phase-pool-auth-e%d", owner.Epoch), Labels: map[string]string{
			runtimePoolAuthLabel: "true", runtimePoolUIDLabel: string(pool.UID),
		}},
		Data: map[string][]byte{runtimePoolControllerTokenKey: []byte(strings.Repeat("t", 32)), runtimePoolCapabilitySecretKey: []byte(strings.Repeat("s", 32))},
	}
	for _, object := range []client.Object{task, pool, secret} {
		if err := fixture.client.Create(fixture.ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	ready, cleanupErr := fixture.dispatcher.reconcileRecoveredRuntimeSession(fixture.ctx, task, task.UID, true,
		&sessionRuntimeCleanupFence{controller: owner, runtimeEpoch: uint64(owner.Epoch)})
	select {
	case err := <-advanceResult:
		if err != nil {
			t.Fatalf("takeover during Status: %v", err)
		}
	default:
		t.Fatal("cleanup did not reach the takeover Status response")
	}
	if ready || !errors.Is(cleanupErr, store.ErrConflict) || !strings.Contains(cleanupErr.Error(), "lost authority") {
		t.Errorf("cleanup after takeover = ready:%v err:%v", ready, cleanupErr)
	}
	if deleteCalls.Load() != 0 {
		t.Errorf("old cleanup owner sent %d DELETE calls after takeover", deleteCalls.Load())
	}
	cached, err := fixture.epochs.CurrentFence(fixture.ctx)
	if err != nil || cached != owner {
		t.Fatalf("test changed cached rather than authoritative ownership: %#v, %v", cached, err)
	}
	current := &corev1alpha1.Task{}
	if err := fixture.client.Get(fixture.ctx, client.ObjectKeyFromObject(task), current); err != nil || current.Status.Execution.RuntimeSessionCleanupDigest != "" {
		t.Fatalf("takeover wrote a Task cleanup receipt: %v", err)
	}
}
