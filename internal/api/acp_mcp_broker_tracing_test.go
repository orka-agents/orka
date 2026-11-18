package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/controller"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
	orkatracing "github.com/orka-agents/orka/internal/tracing"
	"github.com/orka-agents/orka/internal/tracing/testutil"
	"github.com/orka-agents/orka/workers/acp/supervisor"
)

func TestACPMCPBrokerHTTPDelegationPreservesTraceContext(t *testing.T) {
	previousPropagator := otel.GetTextMapPropagator()
	// A global baggage propagator must not broaden the supervisor's transport.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	t.Cleanup(func() { otel.SetTextMapPropagator(previousPropagator) })
	for _, sampled := range []bool{true, false} {
		t.Run(fmt.Sprintf("sampled=%t", sampled), func(t *testing.T) {
			spans := testutil.NewSpanHarness(t)
			type observedContext struct {
				span           trace.SpanContext
				baggageMembers int
			}
			captureContext := func(ctx context.Context) observedContext {
				return observedContext{span: trace.SpanContextFromContext(ctx), baggageMembers: baggage.FromContext(ctx).Len()}
			}
			scheme := runtime.NewScheme()
			require.NoError(t, corev1alpha1.AddToScheme(scheme))
			parent := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: "parent", Namespace: "default", UID: "parent-uid"},
				Spec:       corev1alpha1.TaskSpec{AgentRef: &corev1alpha1.AgentReference{Name: "agent"}},
			}
			agent := &corev1alpha1.Agent{
				ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: parent.Namespace},
				Spec: corev1alpha1.AgentSpec{Coordination: &corev1alpha1.CoordinationConfig{
					Enabled: true, MaxDepth: 3, AllowedAgents: []corev1alpha1.AllowedAgent{{Name: "agent"}},
				}},
			}
			toolContexts := make(chan observedContext, 1)
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(parent, agent).
				WithInterceptorFuncs(interceptor.Funcs{
					Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
						if _, ok := object.(*corev1alpha1.Task); ok {
							toolContexts <- captureContext(ctx)
						}
						return c.Create(ctx, object, opts...)
					},
				}).Build()
			delegate, err := tools.NewBrokeredDelegateTaskTool(kube, tools.BrokeredDelegateTaskTransactionExchangeConfig{})
			require.NoError(t, err)
			registry := tools.NewRegistry()
			registry.Register(delegate)
			const privatePrompt = "PRIVATE_MCP_PROMPT_CANARY"
			request, profile := newACPMCPTracingRequest(t, parent, delegate)
			request.Call.Arguments = json.RawMessage(`{"agent":"agent","prompt":"` + privatePrompt + `"}`)
			request.Metadata.RequestDigest, err = harnessv2.CanonicalRequestDigest(request)
			require.NoError(t, err)
			wantBody, err := json.Marshal(request)
			require.NoError(t, err)

			effects := newSQLiteExecutionEventStoreForTest(t)
			epoch, err := effects.CompareAndSwapControllerEpoch(t.Context(), store.ControllerEpochCAS{
				NewEpoch: 1, HolderID: "controller", RequestDigest: store.CanonicalBytesDigest([]byte("trace-epoch")),
			})
			require.NoError(t, err)
			frozenPolicy := request.Authorization.Configuration()
			credentials := controller.ACPMCPBrokerCredentials{
				// These credentials are synthetic and never leave the loopback test server.
				ControllerBearerToken: strings.Repeat("b", 32), CapabilitySecret: bytes.Repeat([]byte("c"), harnessv2.MinCapabilitySecretBytes),
				ExpectedFence: request.Metadata.Fence, RuntimeProfile: profile, ExpectedMCPConfiguration: &frozenPolicy,
				ControllerFence: store.ControllerEpochFence{Name: epoch.Name, Epoch: epoch.Epoch, HolderID: epoch.HolderID},
				Task:            controller.ACPMCPAuthenticatedTask{Name: parent.Name, Namespace: parent.Namespace, UID: string(parent.UID)},
			}
			brokerContexts := make(chan observedContext, 1)
			broker := &controller.ACPMCPBroker{
				Credentials: controller.ACPMCPBrokerCredentialResolverFunc(func(context.Context, harnessv2.MCPBrokerCallRequest) (controller.ACPMCPBrokerCredentials, error) {
					return credentials, nil
				}),
				Prompts: controller.ACPMCPPromptAuthorizerFunc(func(context.Context, harnessv2.MCPBrokerCallRequest) error { return nil }),
				Effects: effects,
				Executor: controller.RegistryACPMCPToolExecutor{Registry: registry, ContextFactory: func(ctx context.Context, call harnessv2.MCPBrokerCallRequest) (*tools.ToolContext, error) {
					task, ok := controller.ACPMCPAuthenticatedTaskFromContext(ctx)
					if !ok || task != credentials.Task {
						return nil, fmt.Errorf("broker lost authenticated Task identity")
					}
					brokerContexts <- captureContext(ctx)
					return &tools.ToolContext{
						Brokered: true, Client: kube, PolicyReader: kube, TaskID: task.Name, TaskUID: task.UID, Namespace: task.Namespace,
						SessionID: string(call.Authorization.RuntimeSessionUID), OperationID: string(call.Metadata.OperationID), ExternalEffects: effects,
					}, nil
				}},
			}
			type receivedRequest struct {
				ctx                              observedContext
				traceParent, traceState, baggage string
				body                             []byte
				bearerValid, capabilityValid     bool
			}
			received := make(chan receivedRequest, 1)
			app := fiber.New()
			app.Use(NewTracingMiddleware())
			app.Use(func(c fiber.Ctx) error {
				observed := receivedRequest{
					ctx: captureContext(c.Context()), traceParent: strings.Clone(c.Get("traceparent")), traceState: strings.Clone(c.Get("tracestate")), baggage: strings.Clone(c.Get("baggage")),
					body: bytes.Clone(c.Body()), bearerValid: c.Get("Authorization") == "Bearer "+credentials.ControllerBearerToken,
					capabilityValid: harnessv2.VerifyOperationCapability(credentials.CapabilitySecret, c.Get(harnessv2.OperationCapabilityHeader), request.Metadata, true, time.Now().UTC()) == nil,
				}
				err := c.Next()
				received <- observed
				return err
			})
			require.NoError(t, RegisterACPMCPBroker(app, broker))
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			require.NoError(t, err)
			serveErrors := make(chan error, 1)
			go func() { serveErrors <- app.Listener(listener, fiber.ListenConfig{DisableStartupMessage: true}) }()
			t.Cleanup(func() {
				require.NoError(t, app.ShutdownWithTimeout(5*time.Second))
				require.NoError(t, <-serveErrors)
			})
			brokerClient, err := supervisor.NewControllerMCPBrokerClient("http://"+listener.Addr().String(), parent.Namespace, credentials.ControllerBearerToken, credentials.CapabilitySecret)
			require.NoError(t, err)
			state, err := trace.ParseTraceState("orka=supervisor")
			require.NoError(t, err)
			var flags trace.TraceFlags
			if sampled {
				flags = trace.FlagsSampled
			}
			parentSpan := trace.NewSpanContext(trace.SpanContextConfig{
				TraceID: trace.TraceID{1}, SpanID: trace.SpanID{2}, TraceFlags: flags, TraceState: state,
			})
			const privateBaggage = "PRIVATE_MCP_BAGGAGE_CANARY"
			member, err := baggage.NewMember("private", privateBaggage)
			require.NoError(t, err)
			bag, err := baggage.New(member)
			require.NoError(t, err)
			ctx := baggage.ContextWithBaggage(trace.ContextWithSpanContext(t.Context(), parentSpan), bag)
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			response, err := brokerClient.Call(ctx, request)
			require.NoError(t, err)
			require.False(t, response.IsError)
			var delegated tools.DelegateTaskResult
			require.NoError(t, json.Unmarshal(response.Result, &delegated))
			child := &corev1alpha1.Task{}
			require.NoError(t, kube.Get(t.Context(), client.ObjectKey{Namespace: parent.Namespace, Name: delegated.TaskName}, child))

			observed := <-received
			wantHeaders := propagation.HeaderCarrier(http.Header{})
			propagation.TraceContext{}.Inject(ctx, wantHeaders)
			if observed.traceParent != wantHeaders.Get("traceparent") || observed.traceState != wantHeaders.Get("tracestate") {
				t.Error("supervisor broker client did not propagate the W3C parent and state")
			}
			if !bytes.Equal(observed.body, wantBody) || !observed.bearerValid || !observed.capabilityValid {
				t.Error("trace propagation changed the signed broker request or authentication")
			}
			apiSpan := observed.ctx.span
			brokerCtx, toolCtx := <-brokerContexts, <-toolContexts
			if !brokerCtx.span.Equal(apiSpan) {
				t.Error("registered Fiber broker route dropped the API server span context")
			}
			toolSpan := toolCtx.span
			childSpan := trace.SpanContextFromContext(orkatracing.ExtractTaskTraceContext(t.Context(), child))
			for name, span := range map[string]trace.SpanContext{"API": apiSpan, "tool": toolSpan, "child": childSpan} {
				if !span.IsValid() || span.TraceID() != parentSpan.TraceID() || span.TraceFlags() != flags || span.TraceState().String() != state.String() {
					t.Errorf("%s lost the supervisor trace identity, sampling decision, or state", name)
				}
			}
			if childSpan.SpanID() != toolSpan.SpanID() || toolSpan.SpanID() == apiSpan.SpanID() {
				t.Error("child Task trace annotation does not identify the delegate tool span")
			}
			if observed.baggage != "" || observed.ctx.baggageMembers != 0 || brokerCtx.baggageMembers != 0 || toolCtx.baggageMembers != 0 || child.Annotations[labels.AnnotationTraceBaggage] != "" {
				t.Error("baggage crossed the broker transport or reached the child Task")
			}
			ended := spans.Recorder.Ended()
			if sampled {
				api := testutil.SpanNamed(ended, "POST "+harnessv2.MCPBrokerCallPath)
				tool := testutil.SpanNamed(ended, "execute_tool delegate_task")
				require.NotNil(t, api)
				require.NotNil(t, tool)
				require.True(t, api.Parent().Equal(parentSpan.WithRemote(true)), "API span must descend from the supervisor span")
				require.True(t, tool.Parent().Equal(api.SpanContext()), "delegate tool must descend from the authenticated API/broker span")
			} else if len(ended) != 0 {
				t.Errorf("unsampled supervisor context created %d sampled broker/tool spans", len(ended))
			}
			for _, span := range ended {
				attributesAndEvents := fmt.Sprint(span.Attributes(), span.Events())
				for _, private := range []string{privatePrompt, privateBaggage, credentials.ControllerBearerToken, string(credentials.CapabilitySecret)} {
					if strings.Contains(attributesAndEvents, private) {
						t.Errorf("private broker data reached span %s", span.Name())
					}
				}
			}
		})
	}
}

func newACPMCPTracingRequest(t *testing.T, task *corev1alpha1.Task, tool tools.Tool) (harnessv2.MCPBrokerCallRequest, harnessv2.RuntimeProfile) {
	t.Helper()
	policy := harnessv2.MCPToolPolicy{
		AllowedToolNames: []string{tool.Name()},
		Tools: []harnessv2.MCPToolDescriptor{{
			Name: tool.Name(), Description: tool.Description(), InputSchema: tool.Parameters(),
			Source: harnessv2.MCPToolSourceBrokeredBuiltin, Effect: harnessv2.MCPToolEffectConsequential,
		}},
	}
	var err error
	policy.DescriptorDigest, err = harnessv2.CanonicalMCPToolDescriptorDigest(policy.Tools)
	require.NoError(t, err)
	toolDigest, err := harnessv2.CanonicalRuntimeToolPolicyDigest(policy.AllowedToolNames, nil, false)
	require.NoError(t, err)
	approvalDigest, err := harnessv2.CanonicalMCPApprovalPolicyDigest(harnessv2.MCPApprovalPolicy{})
	require.NoError(t, err)
	mcpDigest, err := harnessv2.CanonicalMCPConfigurationDigest(policy.AllowedToolNames)
	require.NoError(t, err)
	digest := store.CanonicalBytesDigest
	profile := harnessv2.RuntimeProfile{
		ACPProfile: harnessv2.ACPProfileV1, AdapterDigests: map[string]string{"adapter": digest([]byte("adapter"))}, ProviderKind: "codex", Model: "model",
		AgentConfigurationDigest: digest([]byte("agent")), ToolPolicyDigest: toolDigest, ApprovalPolicyDigest: approvalDigest, MCPConfigurationDigest: mcpDigest,
		WorkspaceIntent: harnessv2.WorkspaceIntentRead, ProxyCredentialRole: "provider", ProxyCredentialScope: "model:model", ResourceClass: "standard",
	}
	profileDigest, err := harnessv2.CanonicalProfileDigest(profile)
	require.NoError(t, err)
	fence := harnessv2.Fence{
		RuntimeInstanceID: "runtime", SupervisorBootID: "boot", ControllerEpoch: 1, RuntimePoolUID: "pool", RuntimePoolGeneration: 1,
		RuntimeSessionUID: "runtime-session", RuntimeSessionGeneration: 1,
		RuntimeProfileDigest: profileDigest, ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
	}
	now := time.Now().UTC()
	return harnessv2.MCPBrokerCallRequest{
		Protocol: harnessv2.ProtocolVersion, Namespace: task.Namespace, SessionState: harnessv2.RuntimeSessionStatePromptRunning,
		Metadata: harnessv2.MutationMetadata{
			Fence: fence, TaskUID: harnessv2.TaskUID(task.UID), TaskAttempt: 1, PromptID: "prompt", OperationID: "delegate-operation",
			RequestDigestSchemaVersion: harnessv2.RequestDigestSchemaVersion, ExpiresAt: now.Add(time.Minute),
		},
		Lease: harnessv2.PromptLease{Generation: 1, IssuedAt: now.Add(-time.Second), ExpiresAt: now.Add(2 * time.Minute)},
		Authorization: harnessv2.PromptMCPAuthorization{
			RuntimeSessionUID: fence.RuntimeSessionUID, SessionGeneration: fence.RuntimeSessionGeneration, TaskUID: harnessv2.TaskUID(task.UID), TaskAttempt: 1,
			PromptID: "prompt", LeaseGeneration: 1, ToolPolicyDigest: toolDigest, ApprovalPolicyDigest: approvalDigest, MCPConfigurationDigest: mcpDigest,
			ToolPolicy: policy, ExpiresAt: now.Add(2 * time.Minute),
		},
		Call: harnessv2.MCPToolCall{CallID: "delegate-call", ToolName: tool.Name()},
	}, profile
}
