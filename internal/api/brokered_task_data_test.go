package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/controller"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	storekube "github.com/orka-agents/orka/internal/store/kube"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/tools"
)

// The Task/ancestry unit tests isolate their own policy checks. The broker
// regressions below exercise the real prompt guard and Kubernetes control store.
func allowBrokeredTaskData(ctx context.Context, access func(context.Context) error) error {
	return access(ctx)
}

func TestBrokeredDataRejectsAttemptSettledBeforeAccess(t *testing.T) {
	for _, tool := range []tools.Tool{tools.NewSendMessageTool(), tools.NewCheckMessagesTool(), tools.NewSearchTranscriptTool()} {
		t.Run(tool.Name(), func(t *testing.T) {
			fixture := newBrokeredDataFixture(t, tool)
			fixture.beforeData = func(ctx context.Context) {
				_, err := fixture.control.TransitionPromptAttemptExecution(ctx, fixture.cancellation())
				require.NoError(t, err)
			}
			response := fixture.call(t)
			require.Equal(t, http.StatusBadGateway, response.Code)
			require.Zero(t, fixture.probe.accesses, "settled attempts must not access protected data")
			incoming, err := fixture.data.GetMessages(t.Context(), fixture.task.Namespace, fixture.task.Name, "root", false)
			require.NoError(t, err)
			require.Len(t, incoming, 1, "rejected inbox reads must not consume messages")
			outgoing, err := fixture.data.GetMessages(t.Context(), fixture.task.Namespace, "peer", "root", false)
			require.NoError(t, err)
			require.Empty(t, outgoing, "rejected sends must not persist messages")
		})
	}
}

func TestBrokeredDataSerializesWithPromptSettlement(t *testing.T) {
	for _, tool := range []tools.Tool{tools.NewSendMessageTool(), tools.NewCheckMessagesTool(), tools.NewSearchTranscriptTool()} {
		t.Run(tool.Name(), func(t *testing.T) {
			fixture := newBrokeredDataFixture(t, tool)
			// A separate store instance proves this uses the durable Lease,
			// rather than only the original store's in-process semaphore.
			other, err := storekube.NewComposite(fixture.kube, "orka-system", fixture.data, storekube.WithAPIReader(fixture.kube))
			require.NoError(t, err)
			attempts := 0
			checkSettlementBlocked := func(ctx context.Context) error {
				attempts++
				bounded, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
				defer cancel()
				_, err := other.TransitionPromptAttemptExecution(bounded, fixture.cancellation())
				require.ErrorIs(t, err, context.DeadlineExceeded, "settlement must wait until data access finishes")
				return nil
			}
			fixture.probe.beforeTransaction = func(ctx context.Context) error {
				bounded, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
				defer cancel()
				// Authorization must still leave the SQLite writer available.
				require.NoError(t, fixture.data.SaveResult(bounded, fixture.task.Namespace, "unrelated", []byte("progress")))
				return checkSettlementBlocked(ctx)
			}
			fixture.probe.inTransaction = checkSettlementBlocked
			response := fixture.call(t)
			require.Equal(t, 2, attempts, "authority must remain guarded before and inside the data transaction")
			require.Equal(t, http.StatusOK, response.Code)
			require.Equal(t, 1, fixture.probe.accesses)
			_, err = other.TransitionPromptAttemptExecution(t.Context(), fixture.cancellation())
			require.NoError(t, err, "data access must release the interlock before returning")
			response = fixture.call(t)
			require.Equal(t, http.StatusForbidden, response.Code)
			require.Equal(t, 1, fixture.probe.accesses, "settled attempts must not access data or replay a send")
		})
	}
}

func TestBrokeredDataRechecksImmutableAuthority(t *testing.T) {
	for _, change := range []string{"session generation", "runtime instance", "Task identity", "epoch takeover", "tool policy", "missing epoch guard"} {
		t.Run(change, func(t *testing.T) {
			fixture := newBrokeredDataFixture(t, tools.NewSearchTranscriptTool())
			fixture.beforeData = func(ctx context.Context) {
				switch change {
				case "session generation":
					fixture.credentials.ExpectedFence.RuntimeSessionGeneration++
				case "runtime instance":
					fixture.credentials.ExpectedFence.RuntimeInstanceID = "replacement-runtime"
				case "Task identity":
					fixture.credentials.Task.UID = "replacement-task"
				case "epoch takeover":
					_, err := fixture.control.CompareAndSwapControllerEpoch(ctx, store.ControllerEpochCAS{
						ExpectedVersion: 1, ExpectedEpoch: 1, NewEpoch: 2, HolderID: "replacement-controller", RequestDigest: store.CanonicalBytesDigest([]byte("takeover")),
					})
					require.NoError(t, err)
				case "tool policy":
					fixture.credentials.RuntimeProfile.ToolPolicyDigest = store.CanonicalBytesDigest([]byte("changed policy"))
				case "missing epoch guard":
					fixture.broker.EpochMutations = nil
				}
			}
			response := fixture.call(t)
			require.Equal(t, http.StatusBadGateway, response.Code)
			require.Zero(t, fixture.probe.accesses)
		})
	}
}

type brokeredDataProbe struct {
	*sqlite.Store
	accesses          int
	beforeTransaction func(context.Context) error
	inTransaction     func(context.Context) error
}

func (s *brokeredDataProbe) WithAuthorizedTaskDataTransaction(ctx context.Context, namespace, taskName string, authorize, access func(context.Context) error) error {
	return s.Store.WithAuthorizedTaskDataTransaction(ctx, namespace, taskName, func(authCtx context.Context) error {
		if err := authorize(authCtx); err != nil {
			return err
		}
		if s.beforeTransaction != nil {
			return s.beforeTransaction(authCtx)
		}
		return nil
	}, func(txCtx context.Context) error {
		if s.inTransaction != nil {
			if err := s.inTransaction(txCtx); err != nil {
				return err
			}
		}
		return access(txCtx)
	})
}

func (s *brokeredDataProbe) SendMessage(ctx context.Context, message *store.Message) error {
	s.accesses++
	return s.Store.SendMessage(ctx, message)
}

func (s *brokeredDataProbe) GetMessages(ctx context.Context, namespace, taskName, parentTask string, markRead bool) ([]store.Message, error) {
	s.accesses++
	return s.Store.GetMessages(ctx, namespace, taskName, parentTask, markRead)
}

func (s *brokeredDataProbe) SearchTranscript(ctx context.Context, filter store.TranscriptSearchFilter) ([]store.TranscriptSearchResult, error) {
	s.accesses++
	return s.Store.SearchTranscript(ctx, filter)
}

type brokeredDataFixture struct {
	task        *corev1alpha1.Task
	kube        client.WithWatch
	data        *sqlite.Store
	probe       *brokeredDataProbe
	control     *storekube.Store
	fence       store.ControllerEpochFence
	attempt     *store.PromptAttempt
	request     harnessv2.MCPBrokerCallRequest
	credentials controller.ACPMCPBrokerCredentials
	broker      *controller.ACPMCPBroker
	beforeData  func(context.Context)
}

func newBrokeredDataFixture(t *testing.T, tool tools.Tool) *brokeredDataFixture {
	t.Helper()
	task, original, data := setupBrokeredTranscriptSearch(t)
	scheme := original.Scheme()
	require.NoError(t, coordinationv1.AddToScheme(scheme))
	var tasks corev1alpha1.TaskList
	require.NoError(t, original.List(t.Context(), &tasks))
	objects := make([]client.Object, 0, len(tasks.Items))
	for i := range tasks.Items {
		objects = append(objects, tasks.Items[i].DeepCopy())
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithStatusSubresource(
		&corev1alpha1.ControllerEpoch{}, &corev1alpha1.PromptAttempt{}, &corev1alpha1.ExternalEffect{}, &corev1alpha1.RuntimeSessionControl{},
	).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
			if object.GetUID() == "" {
				object.SetUID(types.UID("fixture-" + object.GetName()))
			}
			return c.Create(ctx, object, opts...)
		},
	}).Build()
	control, err := storekube.NewComposite(kube, "orka-system", data, storekube.WithAPIReader(kube))
	require.NoError(t, err)
	epoch, err := control.CompareAndSwapControllerEpoch(t.Context(), store.ControllerEpochCAS{
		NewEpoch: 1, HolderID: "controller", RequestDigest: store.CanonicalBytesDigest([]byte("epoch")),
	})
	require.NoError(t, err)
	fixture := &brokeredDataFixture{
		task: task, kube: kube, data: data, probe: &brokeredDataProbe{Store: data}, control: control,
		fence: store.ControllerEpochFence{Name: epoch.Name, Epoch: epoch.Epoch, HolderID: epoch.HolderID},
	}
	request, profile := brokeredDataRequest(t, task, tool)
	fixture.request = request
	fixture.credentials = controller.ACPMCPBrokerCredentials{
		ControllerBearerToken: strings.Repeat("b", 32), CapabilitySecret: bytes.Repeat([]byte("c"), 32),
		ExpectedFence: request.Metadata.Fence, RuntimeProfile: profile, ControllerFence: fixture.fence,
		Task: controller.ACPMCPAuthenticatedTask{Name: task.Name, Namespace: task.Namespace, UID: string(task.UID), ParentTaskID: "root"},
	}
	fixture.attempt, err = control.CreatePromptAttempt(t.Context(), &store.PromptAttempt{
		Key:        store.PromptAttemptKey{Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: 1, PromptID: "prompt"},
		SessionUID: string(request.Authorization.RuntimeSessionUID), RuntimeInstanceID: string(request.Metadata.Fence.RuntimeInstanceID),
		RequestDigest: store.CanonicalBytesDigest([]byte("prompt")), BindingDigest: store.CanonicalBytesDigest([]byte("binding")), SnapshotDigest: store.CanonicalBytesDigest([]byte("snapshot")),
	}, fixture.fence)
	require.NoError(t, err)
	for _, state := range []store.PromptExecutionState{store.PromptExecutionReserved, store.PromptExecutionSessionStarting, store.PromptExecutionPlanned, store.PromptExecutionSubmitting, store.PromptExecutionAccepted, store.PromptExecutionRunning} {
		fixture.attempt, err = control.TransitionPromptAttemptExecution(t.Context(), store.PromptAttemptExecutionTransition{
			ID: fixture.attempt.ID, Fence: fixture.fence, ExpectedVersion: fixture.attempt.Version, ExpectedState: fixture.attempt.ExecutionState,
			NewState: state, OperationID: string(state), OperationDigest: store.CanonicalBytesDigest([]byte(state)),
		})
		require.NoError(t, err)
	}
	require.NoError(t, data.SendMessage(t.Context(), &store.Message{
		Namespace: task.Namespace, FromTask: "peer", ToTask: "*", ParentTask: "root", Content: "original broadcast",
	}))
	registry := tools.NewRegistry()
	registry.Register(tool)
	fixture.broker = &controller.ACPMCPBroker{
		Credentials: controller.ACPMCPBrokerCredentialResolverFunc(func(context.Context, harnessv2.MCPBrokerCallRequest) (controller.ACPMCPBrokerCredentials, error) {
			return fixture.credentials, nil
		}),
		Prompts: controller.DurableACPMCPPromptAuthorizer{Attempts: control}, Effects: control, EpochMutations: control,
		Executor: controller.RegistryACPMCPToolExecutor{Registry: registry, ContextFactory: func(ctx context.Context, _ harnessv2.MCPBrokerCallRequest) (*tools.ToolContext, error) {
			if fixture.beforeData != nil {
				fixture.beforeData(ctx)
			}
			guard, ok := controller.ACPMCPTaskDataGuardFromContext(ctx)
			require.True(t, ok)
			return &tools.ToolContext{
				Brokered: true, Client: kube, Namespace: task.Namespace, TaskID: task.Name, TaskUID: string(task.UID), ParentTaskID: "root",
				MessageStore:       NewTaskMessageStore(kube, fixture.probe, client.ObjectKeyFromObject(task), string(task.UID), true, guard),
				TranscriptSearcher: NewTaskTranscriptSearcher(kube, fixture.probe, data, client.ObjectKeyFromObject(task), string(task.UID), true, guard),
			}, nil
		}},
	}
	return fixture
}

func (f *brokeredDataFixture) cancellation() store.PromptAttemptExecutionTransition {
	return store.PromptAttemptExecutionTransition{
		ID: f.attempt.ID, Fence: f.fence, ExpectedVersion: f.attempt.Version, ExpectedState: f.attempt.ExecutionState,
		NewState: store.PromptExecutionCancelled, OperationID: "cancel", OperationDigest: store.CanonicalBytesDigest([]byte("cancel")),
	}
}

func (f *brokeredDataFixture) call(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(f.request)
	require.NoError(t, err)
	capability, err := harnessv2.SignOperationCapability(f.credentials.CapabilitySecret, harnessv2.ClaimsForMutation(f.request.Metadata))
	require.NoError(t, err)
	request := httptest.NewRequest(http.MethodPost, harnessv2.MCPBrokerCallPath, bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+f.credentials.ControllerBearerToken)
	request.Header.Set(harnessv2.OperationCapabilityHeader, capability)
	response := httptest.NewRecorder()
	f.broker.ServeHTTP(response, request)
	return response
}

func brokeredDataRequest(t *testing.T, task *corev1alpha1.Task, tool tools.Tool) (harnessv2.MCPBrokerCallRequest, harnessv2.RuntimeProfile) {
	t.Helper()
	now := time.Now().UTC()
	digest := store.CanonicalBytesDigest
	fence := harnessv2.Fence{
		RuntimeInstanceID: "runtime", SupervisorBootID: "boot", ControllerEpoch: 1, RuntimePoolUID: "pool", RuntimePoolGeneration: 1,
		RuntimeSessionUID: "runtime-session", RuntimeSessionGeneration: 1,
		RuntimeProfileDigest: harnessv2.ProfileDigest(digest([]byte("profile"))), ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
	}
	effect := harnessv2.MCPToolEffectConsequential
	if tool.Name() == "search_transcript" {
		effect = harnessv2.MCPToolEffectReadOnly
	}
	descriptor := harnessv2.MCPToolDescriptor{Name: tool.Name(), Description: tool.Description(), InputSchema: tool.Parameters(), Source: harnessv2.MCPToolSourceBrokeredBuiltin, Effect: effect}
	descriptorDigest, err := harnessv2.CanonicalMCPToolDescriptorDigest([]harnessv2.MCPToolDescriptor{descriptor})
	require.NoError(t, err)
	policy := harnessv2.MCPToolPolicy{AllowedToolNames: []string{tool.Name()}, Tools: []harnessv2.MCPToolDescriptor{descriptor}, DescriptorDigest: descriptorDigest}
	toolDigest, err := harnessv2.CanonicalRuntimeToolPolicyDigest(policy.AllowedToolNames, nil, false)
	require.NoError(t, err)
	approvalDigest, err := harnessv2.CanonicalMCPApprovalPolicyDigest(harnessv2.MCPApprovalPolicy{})
	require.NoError(t, err)
	mcpDigest, err := harnessv2.CanonicalMCPConfigurationDigest(policy.AllowedToolNames)
	require.NoError(t, err)
	profile := harnessv2.RuntimeProfile{
		ACPProfile: harnessv2.ACPProfileV1, AdapterDigests: map[string]string{"adapter": digest([]byte("adapter"))}, ProviderKind: "codex", Model: "model",
		AgentConfigurationDigest: digest([]byte("agent")), ToolPolicyDigest: toolDigest, ApprovalPolicyDigest: approvalDigest, MCPConfigurationDigest: mcpDigest,
		WorkspaceIntent: harnessv2.WorkspaceIntentRead, ProxyCredentialRole: "provider", ProxyCredentialScope: "model:model", ResourceClass: "standard",
	}
	metadata := harnessv2.MutationMetadata{
		Fence: fence, TaskUID: harnessv2.TaskUID(task.UID), TaskAttempt: 1, PromptID: "prompt", OperationID: "data-operation",
		RequestDigestSchemaVersion: harnessv2.RequestDigestSchemaVersion, ExpiresAt: now.Add(time.Minute),
	}
	args := map[string]string{"send_message": `{"to_task":"peer","content":"late message"}`, "check_messages": `{"mark_read":true}`, "search_transcript": `{"query":"needle"}`}
	request := harnessv2.MCPBrokerCallRequest{
		Protocol: harnessv2.ProtocolVersion, Namespace: task.Namespace, SessionState: harnessv2.RuntimeSessionStatePromptRunning, Metadata: metadata,
		Lease: harnessv2.PromptLease{Generation: 1, IssuedAt: now.Add(-time.Second), ExpiresAt: now.Add(2 * time.Minute)},
		Authorization: harnessv2.PromptMCPAuthorization{
			RuntimeSessionUID: fence.RuntimeSessionUID, SessionGeneration: fence.RuntimeSessionGeneration, TaskUID: metadata.TaskUID, TaskAttempt: metadata.TaskAttempt,
			PromptID: metadata.PromptID, LeaseGeneration: 1, ToolPolicyDigest: toolDigest, ApprovalPolicyDigest: approvalDigest, MCPConfigurationDigest: mcpDigest,
			ToolPolicy: policy, ExpiresAt: now.Add(2 * time.Minute),
		},
		Call: harnessv2.MCPToolCall{CallID: "data-call", ToolName: tool.Name(), Arguments: json.RawMessage(args[tool.Name()])},
	}
	request.Metadata.RequestDigest, err = harnessv2.CanonicalRequestDigest(request)
	require.NoError(t, err)
	return request, profile
}
