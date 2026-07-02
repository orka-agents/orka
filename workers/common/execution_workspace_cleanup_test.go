/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/internal/workspace"
)

func TestCleanupExecutionWorkspaceRetainScrubsSecretsAndReportsReused(t *testing.T) {
	t.Setenv(workspaceHandoffTokenFileEnv, "/home/worker/custom-handoff-token")
	var statuses []executionWorkspaceStatusUpdate
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var status executionWorkspaceStatusUpdate
		if err := json.NewDecoder(r.Body).Decode(&status); err != nil {
			t.Errorf("decode status: %v", err)
		}
		statuses = append(statuses, status)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	recorder := newRecordingWorkspaceExecutor()
	claim, err := recorder.Claim(context.Background(), workspace.ClaimRequest{
		Namespace:       "ns",
		CreateIfMissing: true,
		Template:        workspace.TemplateRef{Name: "template"},
		Timeout:         time.Second,
	})
	if err != nil {
		t.Fatalf("claim workspace: %v", err)
	}

	err = cleanupExecutionWorkspace(
		context.Background(),
		recorder,
		claim.Ref,
		workerenv.ExecutionWorkspaceEnv{
			CleanupPolicy:  "retain",
			ClaimTimeout:   time.Second,
			StatusEndpoint: server.URL,
		},
		true,
		true,
	)
	if err != nil {
		t.Fatalf("cleanupExecutionWorkspace returned error: %v", err)
	}

	assertOperationOrder(t, recorder.operations(), "claim", "exec", "release")
	execReqs := recorder.execRequests()
	if len(execReqs) != 1 {
		t.Fatalf("recorded %d exec requests, want scrub exec", len(execReqs))
	}
	wantScrub := []string{
		"rm",
		"-f",
		agentSandboxWorkerExecPath,
		agentSandboxSATokenExecPath,
		agentSandboxTransactionTokenExecPath,
		agentSandboxContextSubjectTokenExecPath,
		agentSandboxGitAskpassExecPath,
		workspaceHandoffTokenDefaultPath,
		"/home/worker/custom-handoff-token",
	}
	if !reflect.DeepEqual(execReqs[0].Command, wantScrub) {
		t.Fatalf("scrub command = %#v, want %#v", execReqs[0].Command, wantScrub)
	}
	releaseReqs := recorder.releaseRequests()
	if len(releaseReqs) != 1 || !releaseReqs[0].Retain {
		t.Fatalf("release requests = %#v, want retain release", releaseReqs)
	}
	if len(statuses) != 1 {
		t.Fatalf("recorded %d statuses, want 1", len(statuses))
	}
	if statuses[0].Phase != corev1alpha1.ExecutionWorkspacePhaseRetained ||
		statuses[0].Reason != corev1alpha1.ExecutionWorkspaceReasonRetained ||
		!statuses[0].Reused {
		t.Fatalf("status = %#v, want retained/reused", statuses[0])
	}
}

func TestCleanupExecutionWorkspaceSubstrateRetainUsesReleaseScrub(t *testing.T) {
	var statuses []executionWorkspaceStatusUpdate
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var status executionWorkspaceStatusUpdate
		if err := json.NewDecoder(r.Body).Decode(&status); err != nil {
			t.Errorf("decode status: %v", err)
		}
		statuses = append(statuses, status)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	recorder := newRecordingWorkspaceExecutor()
	claim, err := recorder.Claim(context.Background(), workspace.ClaimRequest{
		Namespace:       "ns",
		CreateIfMissing: true,
		Template:        workspace.TemplateRef{Name: "template"},
		Timeout:         time.Second,
	})
	if err != nil {
		t.Fatalf("claim workspace: %v", err)
	}

	err = cleanupExecutionWorkspace(
		context.Background(),
		recorder,
		claim.Ref,
		workerenv.ExecutionWorkspaceEnv{
			Provider:       string(corev1alpha1.WorkspaceProviderSubstrate),
			CleanupPolicy:  "retain",
			ClaimTimeout:   time.Second,
			StatusEndpoint: server.URL,
		},
		true,
		true,
	)
	if err != nil {
		t.Fatalf("cleanupExecutionWorkspace returned error: %v", err)
	}

	assertOperationOrder(t, recorder.operations(), "claim", "release")
	if execReqs := recorder.execRequests(); len(execReqs) != 0 {
		t.Fatalf("recorded %d exec requests, want release-time provider scrub", len(execReqs))
	}
	releaseReqs := recorder.releaseRequests()
	if len(releaseReqs) != 1 || !releaseReqs[0].Retain {
		t.Fatalf("release requests = %#v, want retain release", releaseReqs)
	}
	if len(statuses) != 1 {
		t.Fatalf("recorded %d statuses, want 1", len(statuses))
	}
	if statuses[0].Phase != corev1alpha1.ExecutionWorkspacePhaseRetained ||
		statuses[0].Reason != corev1alpha1.ExecutionWorkspaceReasonRetained ||
		!statuses[0].Reused {
		t.Fatalf("status = %#v, want retained/reused", statuses[0])
	}
}

func TestCleanupExecutionWorkspaceSubstratePoolRetainDeletesAndReportsDelete(t *testing.T) {
	var statuses []executionWorkspaceStatusUpdate
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var status executionWorkspaceStatusUpdate
		if err := json.NewDecoder(r.Body).Decode(&status); err != nil {
			t.Errorf("decode status: %v", err)
		}
		statuses = append(statuses, status)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	recorder := newRecordingWorkspaceExecutor()
	claim, err := recorder.Claim(context.Background(), workspace.ClaimRequest{
		Namespace:       "ns",
		CreateIfMissing: true,
		Template:        workspace.TemplateRef{Name: "template"},
		Timeout:         time.Second,
	})
	if err != nil {
		t.Fatalf("claim workspace: %v", err)
	}

	err = cleanupExecutionWorkspace(
		context.Background(),
		recorder,
		claim.Ref,
		workerenv.ExecutionWorkspaceEnv{
			Provider:       string(corev1alpha1.WorkspaceProviderSubstrate),
			CleanupPolicy:  "retain",
			PoolName:       "codex-pool",
			ClaimTimeout:   time.Second,
			StatusEndpoint: server.URL,
		},
		true,
		true,
	)
	if err != nil {
		t.Fatalf("cleanupExecutionWorkspace returned error: %v", err)
	}

	assertOperationOrder(t, recorder.operations(), "claim", "delete")
	if releaseReqs := recorder.releaseRequests(); len(releaseReqs) != 0 {
		t.Fatalf("release requests = %#v, want none for pooled workspace cleanup", releaseReqs)
	}
	deleteReqs := recorder.deleteRequests()
	if len(deleteReqs) != 1 {
		t.Fatalf("delete requests = %#v, want one delete request", deleteReqs)
	}
	if len(statuses) != 1 {
		t.Fatalf("recorded %d statuses, want 1", len(statuses))
	}
	if statuses[0].Phase != corev1alpha1.ExecutionWorkspacePhaseDeleted ||
		statuses[0].Reason != corev1alpha1.ExecutionWorkspaceReasonDeleted ||
		statuses[0].CleanupPolicy != corev1alpha1.WorkspaceCleanupPolicyDelete ||
		!statuses[0].Reused {
		t.Fatalf("status = %#v, want deleted/delete/reused", statuses[0])
	}
}

func TestCleanupExecutionWorkspaceIncludesReadyTelemetryInTerminalStatus(t *testing.T) {
	var status executionWorkspaceStatusUpdate
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&status); err != nil {
			t.Errorf("decode status: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	recorder := newRecordingWorkspaceExecutor()
	claim, err := recorder.Claim(context.Background(), workspace.ClaimRequest{
		Namespace:       "ns",
		CreateIfMissing: true,
		Template:        workspace.TemplateRef{Name: "template"},
		Timeout:         time.Second,
	})
	if err != nil {
		t.Fatalf("claim workspace: %v", err)
	}

	err = cleanupExecutionWorkspaceWithOptions(
		context.Background(),
		recorder,
		claim.Ref,
		workerenv.ExecutionWorkspaceEnv{
			Provider:       string(corev1alpha1.WorkspaceProviderSubstrate),
			CleanupPolicy:  "delete",
			ClaimTimeout:   time.Second,
			StatusEndpoint: server.URL,
		},
		true,
		true,
		executionWorkspaceCleanupOptions{
			statusOptions: []executionWorkspaceStatusOption{
				withExecutionWorkspaceReadyResult(&workspace.ReadyResult{
					Placement: workspace.Placement{
						WorkerNamespace: "ate-demo",
						WorkerPool:      "codex-pool",
						WorkerPodName:   "ateom-worker-1",
						PodIP:           "10.244.0.42",
					},
					Density: workspace.Density{
						WorkerCount:         1,
						ActorCount:          3,
						RunningActorCount:   1,
						SuspendedActorCount: 2,
						ActorsPerWorker:     "3.00",
					},
					ResumeLatency: 750 * time.Millisecond,
				}),
			},
		},
	)
	if err != nil {
		t.Fatalf("cleanupExecutionWorkspaceWithOptions returned error: %v", err)
	}

	assertOperationOrder(t, recorder.operations(), "claim", "delete")
	if status.Phase != corev1alpha1.ExecutionWorkspacePhaseDeleted ||
		status.Reason != corev1alpha1.ExecutionWorkspaceReasonDeleted ||
		!status.Reused {
		t.Fatalf("status = %#v, want deleted/reused", status)
	}
	if status.Placement == nil {
		t.Fatal("placement = nil, want ready placement on terminal status")
	}
	if status.Placement.WorkerPool != "codex-pool" ||
		status.Placement.WorkerPodName != "ateom-worker-1" {
		t.Fatalf("placement = %#v, want worker placement", status.Placement)
	}
	if status.ResumeLatency == nil || status.ResumeLatency.Duration != 750*time.Millisecond {
		t.Fatalf("resume latency = %#v, want 750ms", status.ResumeLatency)
	}
	if status.Density == nil {
		t.Fatal("density = nil, want ready density on terminal status")
	}
	if status.Density.ActorCount != 3 || status.Density.WorkerCount != 1 ||
		status.Density.ActorsPerWorker != "3.00" {
		t.Fatalf("density = %#v, want actor/worker density", status.Density)
	}
}

func TestCleanupExecutionWorkspaceCanSkipTerminalStatus(t *testing.T) {
	var statusRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		statusRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	recorder := newRecordingWorkspaceExecutor()
	claim, err := recorder.Claim(context.Background(), workspace.ClaimRequest{
		Namespace:       "ns",
		CreateIfMissing: true,
		Template:        workspace.TemplateRef{Name: "template"},
		Timeout:         time.Second,
	})
	if err != nil {
		t.Fatalf("claim workspace: %v", err)
	}

	err = cleanupExecutionWorkspace(
		context.Background(),
		recorder,
		claim.Ref,
		workerenv.ExecutionWorkspaceEnv{
			CleanupPolicy:  "delete",
			ClaimTimeout:   time.Second,
			StatusEndpoint: server.URL,
		},
		false,
		false,
	)
	if err != nil {
		t.Fatalf("cleanupExecutionWorkspace returned error: %v", err)
	}

	assertOperationOrder(t, recorder.operations(), "claim", "delete")
	if got := statusRequests.Load(); got != 0 {
		t.Fatalf("status endpoint received %d requests, want 0", got)
	}
}

func TestPlanExecutionWorkspaceCleanup(t *testing.T) {
	tests := map[string]struct {
		env            workerenv.ExecutionWorkspaceEnv
		wantAction     executionWorkspaceCleanupAction
		wantPolicy     string
		wantPhase      corev1alpha1.ExecutionWorkspacePhase
		wantReason     corev1alpha1.ExecutionWorkspaceReason
		wantOpReason   string
		wantErrContext string
		wantWarn       bool
		wantPreScrub   bool
	}{
		"default delete": {
			env:            workerenv.ExecutionWorkspaceEnv{CleanupPolicy: ""},
			wantAction:     executionWorkspaceCleanupActionDelete,
			wantPolicy:     "",
			wantPhase:      corev1alpha1.ExecutionWorkspacePhaseDeleted,
			wantReason:     corev1alpha1.ExecutionWorkspaceReasonDeleted,
			wantOpReason:   executionWorkspaceCleanupReasonDelete,
			wantErrContext: "delete workspace",
		},
		"retain agent sandbox scrubs first": {
			env: workerenv.ExecutionWorkspaceEnv{
				Provider:      string(corev1alpha1.WorkspaceProviderAgentSandbox),
				CleanupPolicy: " retain ",
			},
			wantAction:     executionWorkspaceCleanupActionRetain,
			wantPolicy:     "retain",
			wantPhase:      corev1alpha1.ExecutionWorkspacePhaseRetained,
			wantReason:     corev1alpha1.ExecutionWorkspaceReasonRetained,
			wantOpReason:   executionWorkspaceCleanupReasonRetain,
			wantErrContext: "retain workspace",
			wantPreScrub:   true,
		},
		"substrate pool retain becomes delete": {
			env: workerenv.ExecutionWorkspaceEnv{
				Provider:      string(corev1alpha1.WorkspaceProviderSubstrate),
				PoolName:      "pool",
				CleanupPolicy: "retain",
			},
			wantAction:     executionWorkspaceCleanupActionDelete,
			wantPolicy:     "delete",
			wantPhase:      corev1alpha1.ExecutionWorkspacePhaseDeleted,
			wantReason:     corev1alpha1.ExecutionWorkspaceReasonDeleted,
			wantOpReason:   executionWorkspaceCleanupReasonDelete,
			wantErrContext: "delete workspace",
		},
		"unsupported retains safely": {
			env: workerenv.ExecutionWorkspaceEnv{
				Provider:      string(corev1alpha1.WorkspaceProviderAgentSandbox),
				CleanupPolicy: "archive",
			},
			wantAction:     executionWorkspaceCleanupActionRetain,
			wantPolicy:     "archive",
			wantPhase:      corev1alpha1.ExecutionWorkspacePhaseRetained,
			wantReason:     corev1alpha1.ExecutionWorkspaceReasonRetained,
			wantOpReason:   executionWorkspaceCleanupReasonUnsupported,
			wantErrContext: "retain workspace after unsupported cleanup policy",
			wantWarn:       true,
			wantPreScrub:   true,
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got := planExecutionWorkspaceCleanup(tt.env)
			if got.action != tt.wantAction ||
				got.statusEnv.CleanupPolicy != tt.wantPolicy ||
				got.statusPhase != tt.wantPhase ||
				got.statusReason != tt.wantReason ||
				got.operationReason != tt.wantOpReason ||
				got.errorContext != tt.wantErrContext ||
				got.warnUnsupported != tt.wantWarn ||
				got.preScrub != tt.wantPreScrub {
				t.Fatalf("planExecutionWorkspaceCleanup() = %#v", got)
			}
		})
	}
}
