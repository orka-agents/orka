package controller

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestRenewPromptLeaseLoopSeparatesStopFromRuntimeCancellation(t *testing.T) {
	for _, scenario := range []struct {
		name          string
		stopAt        string
		expired       bool
		wantCancelled bool
	}{
		{name: "stopped-authority-read", stopAt: "authority"},
		{name: "stopped-http-request", stopAt: "http"},
		{name: "stopped-after-rejection-check", stopAt: "decision"},
		{name: "stopped-with-expired-lease", stopAt: "before", expired: true},
		{name: "active-rejection", wantCancelled: true},
		{name: "active-expired-lease", expired: true, wantCancelled: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			entered := make(chan struct{})
			resume := make(chan struct{})
			resumeRenewal := sync.OnceFunc(func() { close(resume) })
			defer resumeRenewal()
			var authorityReads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if scenario.stopAt == "http" {
					_, _ = io.Copy(io.Discard, r.Body)
					close(entered)
					<-r.Context().Done()
					return
				}
				writeDispatcherJSONStatus(w, http.StatusGone, harnessv2.ErrorResponse{
					Protocol: harnessv2.ProtocolVersion, Code: harnessv2.ErrorCodeStaleFence, Message: "stale fence",
				})
			}))
			defer server.Close()
			runtimeClient, err := harnessv2.NewClient(
				server.URL, harnessv2.WithControllerBearerToken(strings.Repeat("t", 32)),
				harnessv2.WithOperationCapabilitySecret([]byte(strings.Repeat("s", 32))),
				harnessv2.WithBeforeMutation(func(ctx context.Context, _ string) error {
					authorityReads.Add(1)
					if scenario.stopAt == "authority" {
						close(entered)
						<-ctx.Done()
						return markExternalRuntimeMutationReadRetryable(ctx.Err())
					}
					return nil
				}),
			)
			if err != nil {
				t.Fatal(err)
			}
			task := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "renew-stop", UID: types.UID("99999999-9999-9999-9999-999999999998")},
				Status:     corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{Attempt: 1, PromptID: "prompt-renew-stop"}},
			}
			fence := harnessv2.Fence{
				RuntimeInstanceID: "runtime-instance", SupervisorBootID: "boot-id", ControllerEpoch: 1,
				RuntimePoolUID: "pool-uid", RuntimePoolGeneration: 1,
				RuntimeProfileDigest:       harnessv2.ProfileDigest(testControlDigestForDispatcher("renew-profile")),
				ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
				RuntimeSessionUID:          "session-renew-stop", RuntimeSessionGeneration: 1,
			}
			now := time.Now().UTC()
			lease := harnessv2.PromptLease{Generation: 1, IssuedAt: now, ExpiresAt: now.Add(12 * time.Second)}
			if scenario.expired {
				lease.IssuedAt = now.Add(-12 * time.Second)
				lease.ExpiresAt = now.Add(-time.Second)
			}
			descriptor := harnessv2.MCPToolDescriptor{
				Name: acpMCPTestToolName, Description: "renew test tool", InputSchema: json.RawMessage(`{"type":"object"}`),
				Source: harnessv2.MCPToolSourceBrokeredBuiltin, Effect: harnessv2.MCPToolEffectReadOnly,
			}
			descriptorDigest, err := harnessv2.CanonicalMCPToolDescriptorDigest([]harnessv2.MCPToolDescriptor{descriptor})
			if err != nil {
				t.Fatal(err)
			}
			toolPolicy := harnessv2.MCPToolPolicy{
				AllowedToolNames: []string{acpMCPTestToolName}, Tools: []harnessv2.MCPToolDescriptor{descriptor}, DescriptorDigest: descriptorDigest,
			}
			approvalPolicy := harnessv2.MCPApprovalPolicy{}
			toolDigest, _ := harnessv2.CanonicalRuntimeToolPolicyDigest(toolPolicy.AllowedToolNames, toolPolicy.DisallowedToolNames, toolPolicy.AllowBash)
			approvalDigest, _ := harnessv2.CanonicalMCPApprovalPolicyDigest(approvalPolicy)
			mcpDigest, _ := harnessv2.CanonicalMCPConfigurationDigest(toolPolicy.AllowedToolNames)
			authorization := harnessv2.PromptMCPAuthorization{
				RuntimeSessionUID: fence.RuntimeSessionUID, SessionGeneration: fence.RuntimeSessionGeneration,
				TaskUID: harnessv2.TaskUID(task.UID), TaskAttempt: 1, PromptID: "prompt-renew-stop",
				LeaseGeneration: lease.Generation, ToolPolicyDigest: toolDigest, ApprovalPolicyDigest: approvalDigest,
				MCPConfigurationDigest: mcpDigest, ToolPolicy: toolPolicy, ApprovalPolicy: approvalPolicy,
				ExpiresAt: now, // Renew immediately so the test controls the in-flight operation.
			}
			ctx := logf.IntoContext(t.Context(), funcr.New(func(_, message string) {
				if scenario.stopAt == "decision" && strings.Contains(message, "renewal rejected") {
					// Pause after the loop observes a live lease context, but before
					// it cancels the context also needed by result delivery.
					close(entered)
					<-resume
				}
			}, funcr.Options{}))
			runtimeCtx, cancelRuntime := context.WithCancel(ctx)
			defer cancelRuntime()
			leaseCtx, stopLease, cancelOnLeaseFailure := newPromptLeaseContext(runtimeCtx, cancelRuntime)
			defer stopLease()
			admitted := make(chan struct{})
			close(admitted)
			if scenario.stopAt == "before" {
				stopLease()
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				(&ACPDispatcher{}).renewPromptLeaseLoop(
					leaseCtx, admitted, cancelOnLeaseFailure, runtimeClient, "runtime-session-renew-stop-g1",
					task, fence, lease, authorization, harnessv2.DefaultProtocolLimits(), nil,
				)
			}()
			if scenario.stopAt == "authority" || scenario.stopAt == "http" || scenario.stopAt == "decision" {
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					t.Fatal("renewal did not reach its controlled in-flight operation")
				}
				// Stream completion stops renewal before result delivery and cleanup.
				stopLease()
				resumeRenewal()
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("renewal loop did not stop")
			}
			// Stopping cannot undo a failure that already cancelled an active runtime.
			stopLease()
			if cancelled := runtimeCtx.Err() != nil; cancelled != scenario.wantCancelled {
				t.Fatalf("runtime cancelled = %t, want %t", cancelled, scenario.wantCancelled)
			}
			wantReads := int32(1)
			if scenario.expired {
				wantReads = 0
			}
			if got := authorityReads.Load(); got != wantReads {
				t.Fatalf("authority reads = %d, want %d", got, wantReads)
			}
		})
	}
}
