package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/controller"
	gatewayruntime "github.com/orka-agents/orka/internal/gateway"
	"github.com/orka-agents/orka/internal/gateway/protocol"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func newBrokeredReplyFixture(t *testing.T) (*brokeredDataFixture, *gatewayruntime.Service) {
	t.Helper()
	f := newBrokeredDataFixture(t, tools.NewReplyInConversationTool())
	require.NoError(t, gatewayv1alpha1.AddToScheme(f.kube.Scheme()))
	task := f.task
	task.Labels = map[string]string{gatewayruntime.TaskGatewayNameLabel: "chat", gatewayruntime.TaskGatewayBindingLabel: "room", gatewayruntime.TaskGatewayEventLabel: "gev-reply"}
	task.Annotations = map[string]string{gatewayruntime.TaskGatewayEventAnnotation: "gev-reply", gatewayruntime.TaskGatewayExternalEvent: "external", gatewayruntime.TaskGatewaySession: "session", gatewayruntime.TaskGatewayNameAnnotation: "chat", gatewayruntime.TaskGatewayBindingAnnotation: "room"}
	task.Spec = corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, AgentRef: &corev1alpha1.AgentReference{Name: "agent"}, SessionRef: &corev1alpha1.SessionReference{Name: "session", ThroughMessageID: "gateway:gev-reply:user", PromptIncluded: true}, RequestedBy: &corev1alpha1.RequestedBy{Issuer: "gateway.orka.ai/" + task.Namespace + "/ns-uid/chat/gateway-uid", Subject: "sender", Groups: []string{"gateway:chat"}, Roles: []string{"gateway-sender"}}}
	task.Status = corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning}
	require.NoError(t, f.kube.Update(t.Context(), task))
	require.NoError(t, f.kube.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: task.Namespace, UID: "ns-uid"}}))
	require.NoError(t, f.kube.Create(t.Context(), &gatewayv1alpha1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: "chat", Namespace: task.Namespace, UID: "gateway-uid", Generation: 1}, Status: gatewayv1alpha1.GatewayStatus{Ready: true, ObservedGeneration: 1, ObservedCapabilities: &gatewayv1alpha1.GatewayObservedCapabilities{ContractVersion: protocol.Version, Capabilities: gatewayv1alpha1.GatewayCapabilities{InterimDelivery: true}}}}))
	now := time.Now().UTC()
	event := store.GatewayEvent{ID: "gev-reply", Namespace: task.Namespace, NamespaceUID: "ns-uid", GatewayName: "chat", GatewayUID: "gateway-uid", GatewayGeneration: 1, BindingName: "room", AgentName: "agent", ExternalEventID: "external", ProtocolVersion: protocol.Version, EventType: "text", AccountID: "account", ContextID: "room", SenderID: "sender", Text: "hello", SessionName: "session", TaskName: task.Name, ReceivedAt: now, NextAttemptAt: now, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}
	_, _, err := f.data.AdmitGatewayEvent(t.Context(), store.GatewayEventAdmission{Event: event, AppendUserMessage: true, PendingLimit: 10})
	require.NoError(t, err)
	_, err = f.data.ClaimNextGatewayEvent(t.Context(), task.Namespace, "dispatch", now, time.Minute)
	require.NoError(t, err)
	require.NoError(t, f.data.MarkGatewayEventTaskCreated(t.Context(), task.Namespace, event.ID, task.Name, string(task.UID), "dispatch", now))
	service := gatewayruntime.NewService(f.kube, f.data, f.probe, f.data, gatewayruntime.DefaultConfig())
	service.Config.InterimMessagesPerTask = 2
	registry := tools.NewRegistry()
	registry.Register(tools.NewReplyInConversationTool())
	f.broker.Executor = controller.RegistryACPMCPToolExecutor{Registry: registry, ContextFactory: func(ctx context.Context, request harnessv2.MCPBrokerCallRequest) (*tools.ToolContext, error) {
		if f.beforeData != nil {
			f.beforeData(ctx)
		}
		guard, ok := controller.ACPMCPTaskDataGuardFromContext(ctx)
		require.True(t, ok)
		return &tools.ToolContext{Namespace: task.Namespace, TaskID: task.Name, TaskUID: string(task.UID), OperationID: string(request.Metadata.OperationID), GatewayReplySender: NewBrokeredGatewayReplySender(f.kube, service, client.ObjectKeyFromObject(task), string(task.UID), guard)}, nil
	}}
	f.request.Call.Arguments = json.RawMessage(`{"content":"working privately"}`)
	replyRequestDigest(t, f)
	return f, service
}

func replyRequestDigest(t *testing.T, f *brokeredDataFixture) {
	t.Helper()
	var err error
	f.request.Metadata.RequestDigest, err = harnessv2.CanonicalRequestDigest(f.request)
	require.NoError(t, err)
}

func TestBrokeredGatewayReplyAcceptReplayAndCap(t *testing.T) {
	f, _ := newBrokeredReplyFixture(t)
	jobs := &batchv1.JobList{}
	require.NoError(t, f.kube.List(t.Context(), jobs))
	require.Empty(t, jobs.Items)
	first := f.call(t)
	require.Equal(t, http.StatusOK, first.Code, first.Body.String())
	require.NotContains(t, first.Body.String(), "working privately")
	replay := f.call(t)
	require.Equal(t, http.StatusOK, replay.Code, replay.Body.String())
	rows, err := f.data.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: f.task.Namespace})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	f.request.Metadata.OperationID = "second-operation"
	f.request.Call.CallID = "second-call"
	replyRequestDigest(t, f)
	require.Equal(t, http.StatusOK, f.call(t).Code)
	f.request.Metadata.OperationID = "third-operation"
	f.request.Call.CallID = "third-call"
	replyRequestDigest(t, f)
	rejected := f.call(t)
	require.Equal(t, http.StatusOK, rejected.Code, rejected.Body.String())
	require.Contains(t, rejected.Body.String(), "lifetime message limit")
	require.NotContains(t, rejected.Body.String(), "unknown")
	rows, err = f.data.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: f.task.Namespace})
	require.NoError(t, err)
	require.Len(t, rows, 2)
}

func TestBrokeredGatewayReplyRechecksAuthority(t *testing.T) {
	for _, change := range []string{"cancelled", "settled", "session generation", "runtime instance", "epoch", "policy", "replaced task", "inactive task", "forged origin", "delegated", "read only", "transaction"} {
		t.Run(change, func(t *testing.T) {
			f, _ := newBrokeredReplyFixture(t)
			f.beforeData = func(ctx context.Context) {
				switch change {
				case "cancelled":
					_, err := f.control.TransitionPromptAttemptExecution(ctx, f.cancellation())
					require.NoError(t, err)
				case "settled":
					transition := f.cancellation()
					transition.NewState = store.PromptExecutionSettling
					attempt, err := f.control.TransitionPromptAttemptExecution(ctx, transition)
					require.NoError(t, err)
					transition.ExpectedVersion = attempt.Version
					transition.ExpectedState = attempt.ExecutionState
					transition.NewState = store.PromptExecutionSucceeded
					transition.OperationID = "settle"
					transition.OperationDigest = store.CanonicalBytesDigest([]byte("settle"))
					_, err = f.control.TransitionPromptAttemptExecution(ctx, transition)
					require.NoError(t, err)
				case "session generation":
					f.credentials.ExpectedFence.RuntimeSessionGeneration++
				case "runtime instance":
					f.credentials.ExpectedFence.RuntimeInstanceID = "foreign"
				case "epoch":
					_, err := f.control.CompareAndSwapControllerEpoch(ctx, store.ControllerEpochCAS{ExpectedVersion: 1, ExpectedEpoch: 1, NewEpoch: 2, HolderID: "other", RequestDigest: store.CanonicalBytesDigest([]byte("takeover"))})
					require.NoError(t, err)
				case "policy":
					f.credentials.RuntimeProfile.ToolPolicyDigest = store.CanonicalBytesDigest([]byte("changed"))
				default:
					task := f.task.DeepCopy()
					switch change {
					case "replaced task":
						task.UID = "replacement"
					case "inactive task":
						task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
					case "forged origin":
						task.Spec.RequestedBy.Subject = "forged"
					case "delegated":
						task.Labels[labels.LabelParentTask] = "parent"
					case "read only":
						task.Annotations[labels.AnnotationAgentReadOnly] = "true"
					case "transaction":
						task.Spec.Transaction = &corev1alpha1.TaskTransaction{Context: map[string]string{"allowedTools": "[]"}}
					}
					require.NoError(t, f.kube.Update(ctx, task))
				}
			}
			result := f.call(t)
			if result.Code == http.StatusOK {
				var response harnessv2.MCPBrokerCallResponse
				require.NoError(t, json.Unmarshal(result.Body.Bytes(), &response))
				require.True(t, response.IsError, "known denials must be model-visible failures")
			} else {
				require.Equal(t, http.StatusBadGateway, result.Code, result.Body.String())
			}
			require.NotContains(t, result.Body.String(), "gdm-")
			rows, err := f.data.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: f.task.Namespace})
			require.NoError(t, err)
			require.Empty(t, rows)
		})
	}
}

type replySenderAfterBudget struct {
	tools.GatewayReplySender
	after func()
}

func (s replySenderAfterBudget) Budget(ctx context.Context, id string) (tools.GatewayReplyBudget, error) {
	result, err := s.GatewayReplySender.Budget(ctx, id)
	if err == nil {
		s.after()
	}
	return result, err
}

func TestBrokeredGatewayReplyReauthorizesEnqueue(t *testing.T) {
	f, _ := newBrokeredReplyFixture(t)
	executor := f.broker.Executor.(controller.RegistryACPMCPToolExecutor)
	factory := executor.ContextFactory
	executor.ContextFactory = func(ctx context.Context, request harnessv2.MCPBrokerCallRequest) (*tools.ToolContext, error) {
		tc, err := factory(ctx, request)
		if err != nil {
			return nil, err
		}
		tc.GatewayReplySender = replySenderAfterBudget{tc.GatewayReplySender, func() {
			_, err := f.control.TransitionPromptAttemptExecution(ctx, f.cancellation())
			require.NoError(t, err)
		}}
		return tc, nil
	}
	f.broker.Executor = executor
	require.Equal(t, http.StatusBadGateway, f.call(t).Code)
	rows, err := f.data.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: f.task.Namespace})
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestBrokeredGatewayReplyPrepareOutsideWriterAndUnknownAfterEnqueue(t *testing.T) {
	f, _ := newBrokeredReplyFixture(t)
	transactions := 0
	f.probe.beforeTransaction = func(ctx context.Context) error {
		bounded, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		require.NoError(t, f.data.SaveResult(bounded, f.task.Namespace, "unrelated", []byte("progress")), "live preparation must leave writer available")
		transactions++
		return nil
	}
	original := f.broker.Executor
	f.broker.Executor = controller.ACPMCPToolExecutorFunc(func(ctx context.Context, request harnessv2.MCPBrokerCallRequest, d harnessv2.MCPToolDescriptor) (json.RawMessage, error) {
		result, err := original.ExecuteACPMCPTool(ctx, request, d)
		if err != nil {
			return result, err
		}
		return nil, errors.New("private failure after durable enqueue")
	})
	response := f.call(t)
	require.Equal(t, http.StatusBadGateway, response.Code)
	require.NotContains(t, response.Body.String(), "private")
	require.Equal(t, 2, transactions)
	identity := store.ExternalEffectIdentity{Kind: "acp-mcp-tool", Namespace: f.task.Namespace, AggregateID: string(f.request.Authorization.RuntimeSessionUID), OperationID: string(f.request.Metadata.OperationID)}
	effect, err := f.control.GetExternalEffectByIdentity(t.Context(), identity)
	require.NoError(t, err)
	require.Equal(t, store.ExternalEffectOutcomeUnknown, effect.State)
	require.Equal(t, http.StatusBadGateway, f.call(t).Code)
	rows, err := f.data.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: f.task.Namespace})
	require.NoError(t, err)
	require.Len(t, rows, 1)
}

func TestBrokeredGatewayReplyConcurrentDuplicate(t *testing.T) {
	f, _ := newBrokeredReplyFixture(t)
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			response := f.call(t)
			require.True(t, response.Code == http.StatusOK || response.Code == http.StatusBadGateway, response.Body.String())
		})
	}
	wg.Wait()
	require.Equal(t, http.StatusOK, f.call(t).Code)
	rows, err := f.data.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: f.task.Namespace})
	require.NoError(t, err)
	require.Len(t, rows, 1)
}

func TestBrokeredGatewayReplyAtomicLimitAfterPreflight(t *testing.T) {
	f, service := newBrokeredReplyFixture(t)
	service.Config.InterimMessagesPerTask = 1
	executor := f.broker.Executor.(controller.RegistryACPMCPToolExecutor)
	factory := executor.ContextFactory
	executor.ContextFactory = func(ctx context.Context, request harnessv2.MCPBrokerCallRequest) (*tools.ToolContext, error) {
		tc, err := factory(ctx, request)
		if err != nil {
			return nil, err
		}
		sender := tc.GatewayReplySender
		tc.GatewayReplySender = replySenderAfterBudget{sender, func() {
			_, err := sender.Enqueue(ctx, "competing-call", "other progress")
			require.NoError(t, err)
		}}
		return tc, nil
	}
	f.broker.Executor = executor
	response := f.call(t)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.Contains(t, response.Body.String(), "lifetime message limit")
	rows, err := f.data.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: f.task.Namespace})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "other progress", rows[0].Text)
}

func TestBrokeredGatewayReplyReconstructedContextAndConflict(t *testing.T) {
	f, service := newBrokeredReplyFixture(t)
	executor := f.broker.Executor.(controller.RegistryACPMCPToolExecutor)
	factory := executor.ContextFactory
	var captured *tools.ToolContext
	executor.ContextFactory = func(ctx context.Context, request harnessv2.MCPBrokerCallRequest) (*tools.ToolContext, error) {
		tc, err := factory(ctx, request)
		captured = tc
		return tc, err
	}
	f.broker.Executor = executor
	require.Equal(t, http.StatusOK, f.call(t).Code)
	require.NotNil(t, captured)
	service.Config.InterimMessagesPerTask = 1
	// A new ToolContext with the same sealed operation must recover the gateway
	// receipt at exhaustion, independently of the external-effect cached result.
	tc := *captured
	result, err := tools.NewReplyInConversationTool().Execute(tools.WithToolContext(t.Context(), &tc), f.request.Call.Arguments)
	require.NoError(t, err)
	require.Contains(t, result, `"created":false`)
	_, err = tools.NewReplyInConversationTool().Execute(tools.WithToolContext(t.Context(), &tc), []byte(`{"content":"changed private text"}`))
	require.ErrorContains(t, err, "conflicts")
	require.NotContains(t, err.Error(), "private")
	tc.OperationID = "fresh-call"
	_, err = tools.NewReplyInConversationTool().Execute(tools.WithToolContext(t.Context(), &tc), f.request.Call.Arguments)
	require.ErrorContains(t, err, "lifetime message limit")
	rows, err := f.data.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: f.task.Namespace})
	require.NoError(t, err)
	require.Len(t, rows, 1)
}

func TestBrokeredGatewayReplyContentRejectionsAreModelVisible(t *testing.T) {
	for _, content := range []string{`{"content":""}`, `{"content":"x","target":"other"}`, `{"content":"` + strings.Repeat("x", 16385) + `"}`} {
		t.Run(content[:min(len(content), 24)], func(t *testing.T) {
			f, _ := newBrokeredReplyFixture(t)
			f.request.Call.Arguments = json.RawMessage(content)
			replyRequestDigest(t, f)
			response := f.call(t)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), "16384 UTF-8 bytes")
			rows, err := f.data.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: f.task.Namespace})
			require.NoError(t, err)
			require.Empty(t, rows)
		})
	}
}

func TestBrokeredGatewayReplyUnsupportedIsModelVisible(t *testing.T) {
	for _, unsupported := range []bool{false, true} {
		t.Run(map[bool]string{false: "unavailable", true: "unsupported"}[unsupported], func(t *testing.T) {
			f, _ := newBrokeredReplyFixture(t)
			g := &gatewayv1alpha1.Gateway{}
			require.NoError(t, f.kube.Get(t.Context(), client.ObjectKey{Namespace: f.task.Namespace, Name: "chat"}, g))
			message := "temporarily unavailable"
			if unsupported {
				g.Status.ObservedCapabilities.Capabilities.InterimDelivery = false
				message = "does not support interim delivery"
			} else {
				g.Status.Ready = false
			}
			require.NoError(t, f.kube.Update(t.Context(), g))
			response := f.call(t)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), message)
			require.NotContains(t, response.Body.String(), "unknown")
		})
	}
}
