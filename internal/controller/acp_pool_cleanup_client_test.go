package controller

import (
	"context"
	"encoding/json"
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
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestRetainedPoolCleanupOperationScope(t *testing.T) {
	for _, operation := range []string{"create_runtime_session", "start_prompt", "renew_prompt_lease", "resolve_permission", "export_workspace_delta", "drain", "unknown"} {
		for _, deleteOnly := range []bool{false, true} {
			if runtimePoolRetainedCleanupOperationAllowed(operation, deleteOnly) {
				t.Fatalf("cleanup authorized %q", operation)
			}
		}
	}
	for _, operation := range []string{"cancel_prompt", "create_workspace_delta", "finalize_runtime_session_publication"} {
		if !runtimePoolRetainedCleanupOperationAllowed(operation, false) || runtimePoolRetainedCleanupOperationAllowed(operation, true) {
			t.Fatalf("wrong validation/delete-only scope for %s", operation)
		}
	}
	if !runtimePoolRetainedCleanupOperationAllowed("delete_runtime_session", true) {
		t.Fatal("normal deletion unavailable")
	}
}

//nolint:gocyclo // Keep the exact-identity drift and takeover matrix beside its shared wire-level fixture.
func TestRetainedPoolCleanupRevalidatesAuthorityBeforeDelete(t *testing.T) {
	for _, ownerEpoch := range []int64{1, 2} {
		t.Run(fmt.Sprintf("owner-epoch-%d", ownerEpoch), func(t *testing.T) {
			for _, change := range []string{"none", "leader", "task binding", "pool UID", "boot", "pod UID", "pod address", "auth version", "status epoch", "guard takeover", "peer lease", "different outgoing session", "different outgoing task"} {
				t.Run(change, func(t *testing.T) {
					d, task, control, oldFence := validationRecoveryFixture(t, false)
					owner := control.fence
					owner.Epoch = ownerEpoch
					control.fence = owner
					setAgentRuntimeTestControllerEpoch(d.Epochs, ownerEpoch)
					if err := d.Client.Status().Update(t.Context(), task); err != nil {
						t.Fatal(err)
					}
					var deleteCalls atomic.Int32
					var changedStatus atomic.Bool
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method == http.MethodGet && r.URL.Path == harnessv2.StatusPath {
							fence := oldFence
							fence.RuntimeSessionUID = ""
							fence.RuntimeSessionGeneration = 0
							if changedStatus.Load() {
								fence.ControllerEpoch = 2
							}
							writeDispatcherJSON(w, harnessv2.StatusResponse{Protocol: harnessv2.ProtocolVersion, Fence: fence, Timestamp: time.Now().UTC(), Lifecycle: harnessv2.SupervisorLifecycleReady, Drain: harnessv2.DrainStatus{AcceptingNewSessions: true}})
							return
						}
						if r.Method == http.MethodDelete {
							if !control.inMutation.Load() {
								t.Error("mutation escaped the authoritative epoch interlock")
							}
							deleteCalls.Add(1)
							var request harnessv2.DeleteRuntimeSessionRequest
							if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
								t.Error(err)
								w.WriteHeader(400)
								return
							}
							if request.Metadata.Fence.ControllerEpoch != 1 {
								t.Error("cleanup rewrote the old runtime epoch")
							}
							writeDispatcherJSON(w, harnessv2.DeleteRuntimeSessionResponse{Protocol: harnessv2.ProtocolVersion, State: harnessv2.RuntimeSessionStateDeleted, Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh}, Tombstone: testDeleteTombstone(request, time.Now().UTC())})
							return
						}
						t.Errorf("unexpected operation %s %s", r.Method, r.URL.Path)
						w.WriteHeader(404)
					}))
					defer server.Close()
					address, err := url.Parse(server.URL)
					if err != nil {
						t.Fatal(err)
					}
					pool := &corev1alpha1.RuntimePool{ObjectMeta: metav1.ObjectMeta{Namespace: task.Namespace, Name: task.Status.Execution.RuntimePoolName, UID: "recovery-pool-uid", Generation: 1}, Spec: corev1alpha1.RuntimePoolSpec{RuntimeNamespace: "orka-runtimes", Runtime: corev1alpha1.RuntimePoolRuntimeSpec{Profile: corev1alpha1.RuntimePoolProfileSpec{Digest: task.Status.AgentExecutionBinding.RuntimeProfileDigest}}}, Status: corev1alpha1.RuntimePoolStatus{ActiveInstance: &corev1alpha1.RuntimePoolActiveInstanceStatus{PodNamespace: "orka-runtimes", PodName: "resident", PodUID: "recovery-pod", PodAddress: address.Host, RuntimeInstanceID: task.Status.Execution.RuntimeInstanceID, BootID: "recovery-boot", ControllerEpoch: 1, ProfileDigest: task.Status.AgentExecutionBinding.RuntimeProfileDigest}}}
					pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "orka-runtimes", Name: "resident", UID: "recovery-pod", Labels: map[string]string{runtimePoolUIDLabel: string(pool.UID), runtimePoolNameLabel: pool.Name, runtimePoolNamespaceLabel: pool.Namespace}}, Status: corev1.PodStatus{PodIP: address.Hostname()}}
					secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "orka-runtimes", Name: "recovery-auth-e1", UID: "recovery-auth-uid", Labels: map[string]string{runtimePoolAuthLabel: "true", runtimePoolUIDLabel: string(pool.UID)}}, Data: map[string][]byte{runtimePoolControllerTokenKey: []byte(strings.Repeat("fixture-token-", 3)), runtimePoolCapabilitySecretKey: []byte(strings.Repeat("fixture-key-", 3))}}
					for _, o := range []client.Object{pool, pod, secret} {
						if err := d.Client.Create(t.Context(), o); err != nil {
							t.Fatal(err)
						}
					}
					runtimeClient, fence, err := d.runtimePoolRetainedCleanupClient(t.Context(), task, task.UID, pool, owner, true)
					if err != nil {
						t.Fatal(err)
					}
					switch change {
					case "leader":
						control.fence.Epoch++
					case "guard takeover":
						control.guardTakeover.Store(true)
					case "peer lease":
						control.session.Lease = &store.SessionMutationLease{TaskUID: "another-task"}
					case "task binding":
						current := &corev1alpha1.Task{}
						if err := d.Client.Get(t.Context(), client.ObjectKeyFromObject(task), current); err != nil {
							t.Fatal(err)
						}
						current.Status.AgentExecutionBinding.RuntimeType = corev1alpha1.AgentRuntimeClaude
						if err := d.Client.Status().Update(t.Context(), current); err != nil {
							t.Fatal(err)
						}
					case "pool UID":
						pool.UID = "different"
						if err := d.Client.Update(t.Context(), pool); err != nil {
							t.Fatal(err)
						}
					case "boot":
						pool.Status.ActiveInstance.BootID = "different"
						if err := d.Client.Status().Update(t.Context(), pool); err != nil {
							t.Fatal(err)
						}
					case "pod UID":
						pod.UID = "different"
						if err := d.Client.Update(t.Context(), pod); err != nil {
							t.Fatal(err)
						}
					case "pod address":
						pod.Status.PodIP = "192.0.2.8"
						if err := d.Client.Status().Update(t.Context(), pod); err != nil {
							t.Fatal(err)
						}
					case "auth version":
						secret.Data[runtimePoolControllerTokenKey] = []byte(strings.Repeat("different-", 4))
						if err := d.Client.Update(t.Context(), secret); err != nil {
							t.Fatal(err)
						}
					case "status epoch":
						changedStatus.Store(true)
					}
					fence.RuntimeSessionUID = oldFence.RuntimeSessionUID
					fence.RuntimeSessionGeneration = oldFence.RuntimeSessionGeneration
					outgoingUID := task.UID
					if change == "different outgoing session" {
						fence.RuntimeSessionUID = "peer-session"
					}
					if change == "different outgoing task" {
						outgoingUID = "peer-task"
					}
					err = d.deleteRuntimeSessionForTaskUID(context.Background(), runtimeClient, harnessv2.RuntimeSessionID(runtimeSessionID(fence)), task, outgoingUID, fence, "session_deleted")
					if change == "none" {
						if err != nil || deleteCalls.Load() != 1 {
							t.Fatalf("exact cleanup: calls=%d err=%v", deleteCalls.Load(), err)
						}
					} else if err == nil || deleteCalls.Load() != 0 {
						t.Fatalf("changed authority accepted: calls=%d err=%v", deleteCalls.Load(), err)
					}
				})
			}
		})
	}
}

func TestRetainedPoolCleanupPodAddressNormalization(t *testing.T) {
	for _, test := range []struct {
		address, ip string
		want        bool
	}{
		{"127.0.0.1", "127.0.0.1", true}, {"127.0.0.1:8080", "127.0.0.1", true}, {"::1", "::1", true}, {"[::1]:8080", "::1", true},
		{"127.0.0.2:8080", "127.0.0.1", false}, {"untrusted.example:8080", "127.0.0.1", false}, {"", "127.0.0.1", false},
	} {
		if got := runtimePoolCleanupPodAddressMatches(test.address, test.ip); got != test.want {
			t.Errorf("%q matches %q: %v", test.address, test.ip, got)
		}
	}
}
