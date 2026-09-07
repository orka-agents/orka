package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

func TestProductionACPMCPBrokerRejectsLateCustomToolDrift(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*corev1alpha1.Tool, string)
		drift  bool
	}{
		{"destination", func(tool *corev1alpha1.Tool, destination string) {
			tool.Spec.HTTP.URL = destination
			tool.Generation++
		}, true},
		{"parameters", func(tool *corev1alpha1.Tool, _ string) {
			tool.Spec.Parameters = &apiextensionsv1.JSON{Raw: json.RawMessage(`{"type":"object","required":["other"]}`)}
			tool.Generation++
		}, true},
		{"effect-class", func(tool *corev1alpha1.Tool, _ string) {
			tool.Spec.BrokeredToolClass = corev1alpha1.AgentRuntimeBrokeredToolClassRead
			tool.Generation++
		}, true},
		{"observed-endpoint", func(tool *corev1alpha1.Tool, destination string) {
			tool.Status.Endpoint = destination
		}, true},
		{"metadata-only", func(tool *corev1alpha1.Tool, _ string) {
			tool.Labels = map[string]string{"test-observation": "after-authorization"}
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var oldCalls, newCalls atomic.Int32
			oldTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				oldCalls.Add(1)
				writeDispatcherJSON(w, map[string]bool{"ok": true})
			}))
			defer oldTarget.Close()
			newTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				newCalls.Add(1)
				writeDispatcherJSON(w, map[string]bool{"ok": true})
			}))
			defer newTarget.Close()
			const toolName = "dedicated-drift-write"
			tool := testExternalACPCustomTool(toolName)
			tool.Spec.BrokeredToolClass = corev1alpha1.AgentRuntimeBrokeredToolClassWrite
			tool.Spec.HTTP.URL = oldTarget.URL
			policy := testAgentRuntimeMCPPolicy()
			policy.AllowedTools = []string{toolName}
			f := newExternalACPDispatchFixtureWithOptions(t, "drift-runtime", policy,
				externalACPDispatchFixtureOptions{contextTimeout: time.Minute}, tool)
			held := holdExternalPolicyPrompt(t, f)
			task, taskDone := startExternalPolicyTask(t, f, held, "original-drift", false)
			prompt := receiveAcceptedPolicyPrompt(t, f, task, held)
			request := externalPolicyMCPCall(t, prompt, toolName)
			broker := newExternalPolicyBroker(t, f)
			originalExecutor := broker.Executor
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			executionErrors := make(chan error, 2)
			var executions atomic.Int32
			// Only the scheduling barrier is a test wrapper. Authentication,
			// immutable snapshot resolution, prompt authorization, effect
			// reservation and the actual custom Tool executor are production.
			broker.Executor = ACPMCPToolExecutorFunc(func(ctx context.Context, call harnessv2.MCPBrokerCallRequest, descriptor harnessv2.MCPToolDescriptor) (result json.RawMessage, err error) {
				executions.Add(1)
				defer func() { executionErrors <- err }()
				entered <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return originalExecutor.ExecuteACPMCPTool(ctx, call, descriptor)
			})
			done := startExternalPolicyMCPCall(t, f, broker, request)
			select {
			case <-entered:
			case response := <-done:
				t.Fatalf("call failed before the executor barrier: status=%d body=%s", response.Code, response.Body.String())
			case <-f.ctx.Done():
				t.Fatal("authorized original call did not reach the executor barrier")
			}
			current := &corev1alpha1.Tool{}
			if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(tool), current); err != nil {
				t.Fatal(err)
			}
			originalUID, originalRV := current.UID, current.ResourceVersion
			test.mutate(current, newTarget.URL)
			if err := f.client.Update(f.ctx, current); err != nil {
				t.Fatal(err)
			}
			if current.UID != originalUID || current.ResourceVersion == originalRV {
				t.Fatal("Tool update did not preserve the exact original identity and advance resourceVersion")
			}
			unblock()
			response := receiveExternalPolicyMCPResponse(t, f.ctx, done)
			executeErr := <-executionErrors
			wantStatus, wantCalls, wantState := http.StatusOK, int32(1), store.ExternalEffectSucceeded
			if test.drift {
				wantStatus, wantCalls, wantState = http.StatusBadGateway, 0, store.ExternalEffectOutcomeUnknown
				if executeErr == nil || !strings.Contains(executeErr.Error(), "changed after prompt authorization") {
					t.Fatalf("late Tool drift error = %v", executeErr)
				}
			} else if executeErr != nil {
				t.Fatalf("metadata-only Tool update rejected: %v", executeErr)
			}
			if response.Code != wantStatus || oldCalls.Load() != wantCalls || newCalls.Load() != 0 || executions.Load() != 1 {
				t.Fatalf("late Tool update: status=%d old=%d new=%d executions=%d", response.Code, oldCalls.Load(), newCalls.Load(), executions.Load())
			}
			assertExternalPolicyEffect(t, f, request, wantState)
			// A repeated changed request must not send to either destination.
			// Harmless metadata changes still permit the durable replay result.
			replay := receiveExternalPolicyMCPResponse(t, f.ctx, startExternalPolicyMCPCall(t, f, broker, request))
			if test.drift {
				if replay.Code != http.StatusBadGateway {
					t.Fatalf("unknown original effect admitted a replay: status=%d", replay.Code)
				}
			} else {
				var result harnessv2.MCPBrokerCallResponse
				if replay.Code != http.StatusOK || json.Unmarshal(replay.Body.Bytes(), &result) != nil || !result.Replayed {
					t.Fatalf("unchanged descriptor replay = %d %s", replay.Code, replay.Body.String())
				}
			}
			if oldCalls.Load() != wantCalls || newCalls.Load() != 0 || executions.Load() != 1 {
				t.Fatal("repeated request executed the Tool again")
			}
			assertExternalPolicyEffect(t, f, request, wantState)
			finishExternalPolicyTask(t, f, task, held, taskDone, "original Tool result", test.drift)
		})
	}
}

func assertExternalPolicyEffect(t *testing.T, f *externalACPDispatchFixture, request harnessv2.MCPBrokerCallRequest, want store.ExternalEffectState) {
	t.Helper()
	identity := store.ExternalEffectIdentity{Kind: "acp-mcp-tool", Namespace: request.Namespace,
		AggregateID: string(request.Authorization.RuntimeSessionUID), OperationID: string(request.Metadata.OperationID)}
	id, err := identity.CanonicalID()
	if err != nil {
		t.Fatal(err)
	}
	effect, err := f.controlStore.GetExternalEffect(f.ctx, id)
	if err != nil || effect.State != want || effect.Attempts != 1 {
		t.Fatalf("original consequential effect = %#v, err=%v", effect, err)
	}
}
