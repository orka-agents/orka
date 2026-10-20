/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/controller"
	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

// These tests use the registered API routes, including TokenReview and the
// SubjectAccessReviews they request. Only Kubernetes and the external tool are
// fixtures; approval decisions and effect receipts share the real SQLite store.
type v2ApprovalAPIFixture struct {
	server       *Server
	kube         client.Client
	store        *sqlite.Store
	task         *corev1alpha1.Task
	broker       *controller.ACPMCPBroker
	call         harnessv2.MCPBrokerCallRequest
	tokens       map[string]string
	count        atomic.Int32
	tokenReviews atomic.Int32
	reviewsMu    sync.Mutex
	reviews      []authorizationv1.SubjectAccessReviewSpec
}

type v2ApprovalAPIEpochGuard struct{ sync.Mutex }

func (g *v2ApprovalAPIEpochGuard) WithControllerEpochMutation(ctx context.Context, _ store.ControllerEpochFence, fn func(context.Context) error) error {
	g.Lock()
	defer g.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(ctx)
}

func newV2ApprovalAPIFixture(t *testing.T, configure ...func(*harnessv2.MCPBrokerCallRequest)) *v2ApprovalAPIFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "approvals.db")
	db, err := sqlite.NewDB(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	f := &v2ApprovalAPIFixture{store: sqlite.NewStore(db, path), tokens: map[string]string{}}
	f.call, err = newV2ApprovalAPICall()
	require.NoError(t, err)
	for _, change := range configure {
		change(&f.call)
	}
	f.call.Metadata.RequestDigest, err = harnessv2.CanonicalRequestDigest(f.call)
	require.NoError(t, err)
	epoch, err := f.store.CompareAndSwapControllerEpoch(t.Context(), store.ControllerEpochCAS{
		ExpectedVersion: 0, ExpectedEpoch: 0, NewEpoch: 1, HolderID: "approval-api-controller",
		RequestDigest: store.CanonicalBytesDigest([]byte("approval-api-epoch")), UpdatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)
	controllerFence := store.ControllerEpochFence{Name: epoch.Name, Epoch: epoch.Epoch, HolderID: epoch.HolderID}
	metadata := f.call.Metadata
	f.task = &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: f.call.Namespace, Name: "approval-task", UID: types.UID(metadata.TaskUID)},
		Status: corev1alpha1.TaskStatus{Phase: "Running", Execution: &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStateRunning, Attempt: int32(metadata.TaskAttempt), PromptID: string(metadata.PromptID),
			RuntimeInstanceID: string(metadata.Fence.RuntimeInstanceID), RuntimeSessionUID: string(metadata.Fence.RuntimeSessionUID),
			RuntimeSessionGeneration:       int64(metadata.Fence.RuntimeSessionGeneration),
			RuntimeSessionSupervisorBootID: string(metadata.Fence.SupervisorBootID), ControllerEpoch: int64(metadata.Fence.ControllerEpoch),
		}},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, authenticationv1.AddToScheme(scheme))
	identities := make(map[string]authenticationv1.UserInfo)
	for _, role := range []string{"reader", "approval-only", "outsider", "reviewer-a", "reviewer-b"} {
		// A unique fixture credential avoids sharing the middleware's token cache
		// between tests. These values are never sent to a live API server.
		f.tokens[role] = "fixture-" + store.CanonicalBytesDigest([]byte(path+role))
		identities[f.tokens[role]] = v2ApprovalAPIUser(role)
	}
	f.kube = fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(f.task).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if review, ok := obj.(*authenticationv1.TokenReview); ok {
				f.tokenReviews.Add(1)
				user, authenticated := identities[review.Spec.Token]
				review.Status = authenticationv1.TokenReviewStatus{Authenticated: authenticated, User: user}
				return nil
			}
			return c.Create(ctx, obj, opts...)
		},
	}).Build()
	clientset := kubefake.NewClientset()
	clientset.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		f.reviewsMu.Lock()
		f.reviews = append(f.reviews, *review.Spec.DeepCopy())
		f.reviewsMu.Unlock()
		attrs := review.Spec.ResourceAttributes
		role := strings.TrimPrefix(review.Spec.User, "system:serviceaccount:default:")
		if attrs != nil && attrs.Namespace == f.task.Namespace && attrs.Name == f.task.Name &&
			attrs.Group == corev1alpha1.GroupVersion.Group && attrs.Resource == "tasks" && role != "outsider" {
			switch {
			case attrs.Verb == "get" && attrs.Subresource == "":
				review.Status.Allowed = true
			case attrs.Verb == "update" && attrs.Subresource == "approvals":
				review.Status.Allowed = role == "approval-only" || strings.HasPrefix(role, "reviewer-")
			case attrs.Verb == "patch" && attrs.Subresource == "":
				review.Status.Allowed = role == "reader" || strings.HasPrefix(role, "reviewer-")
			}
		}
		return true, review, nil
	})
	f.server = NewServer(f.kube, nil, ServerConfig{
		WatchNamespace: f.task.Namespace, EnforceNamespaceIsolation: true, APIReader: f.kube,
		Clientset: clientset, ExecutionEventStore: f.store,
	})
	f.broker = &controller.ACPMCPBroker{
		Credentials: controller.ACPMCPBrokerCredentialResolverFunc(func(ctx context.Context, _ harnessv2.MCPBrokerCallRequest) (controller.ACPMCPBrokerCredentials, error) {
			current := &corev1alpha1.Task{}
			if err := f.kube.Get(ctx, client.ObjectKeyFromObject(f.task), current); err != nil {
				return controller.ACPMCPBrokerCredentials{}, err
			}
			return controller.ACPMCPBrokerCredentials{
				ControllerBearerToken: strings.Repeat("b", 32), CapabilitySecret: bytes.Repeat([]byte("c"), 32),
				ExpectedFence: metadata.Fence, ControllerFence: controllerFence,
				RuntimeProfile: harnessv2.RuntimeProfile{
					ProviderKind: "agentkit", ToolPolicyDigest: f.call.Authorization.ToolPolicyDigest,
					ApprovalPolicyDigest:   f.call.Authorization.ApprovalPolicyDigest,
					MCPConfigurationDigest: f.call.Authorization.MCPConfigurationDigest,
				},
				Task: controller.ACPMCPAuthenticatedTask{Name: current.Name, Namespace: current.Namespace, UID: string(current.UID)},
			}, nil
		}),
		Prompts: controller.ACPMCPPromptAuthorizerFunc(func(ctx context.Context, call harnessv2.MCPBrokerCallRequest) error {
			current := &corev1alpha1.Task{}
			if err := f.kube.Get(ctx, client.ObjectKeyFromObject(f.task), current); err != nil {
				return err
			}
			execution := current.Status.Execution
			if string(current.UID) != string(call.Metadata.TaskUID) || execution == nil ||
				execution.Attempt != int32(call.Metadata.TaskAttempt) || execution.PromptID != string(call.Metadata.PromptID) ||
				execution.State != corev1alpha1.TaskExecutionStateRunning {
				return errors.New("fixture prompt is no longer active")
			}
			return nil
		}),
		Executor: controller.ACPMCPToolExecutorFunc(func(_ context.Context, call harnessv2.MCPBrokerCallRequest, _ harnessv2.MCPToolDescriptor) (json.RawMessage, error) {
			f.count.Add(1)
			if call.Call.CallID != f.call.Call.CallID || call.Call.ToolName != f.call.Call.ToolName || !bytes.Equal(call.Call.Arguments, f.call.Call.Arguments) {
				return nil, errors.New("executor received different inputs")
			}
			return json.RawMessage(`{"workOrder":"fixture-order-1","created":true}`), nil
		}),
		Effects: f.store, EpochMutations: &v2ApprovalAPIEpochGuard{}, ApprovalEvents: f.store,
		ApprovalSecrets: clientset, ApprovalWaitTimeout: 10 * time.Second, ApprovalPollInterval: 5 * time.Millisecond,
	}
	return f
}

func newV2ApprovalAPICall() (harnessv2.MCPBrokerCallRequest, error) {
	now := time.Now().UTC()
	descriptor := harnessv2.MCPToolDescriptor{
		Name: "create_work_order", Description: "Create a harmless fixture work order",
		InputSchema: json.RawMessage(`{"type":"object"}`), Source: harnessv2.MCPToolSourceBrokeredBuiltin,
		Effect: harnessv2.MCPToolEffectConsequential,
	}
	policy := harnessv2.MCPToolPolicy{AllowedToolNames: []string{descriptor.Name}, Tools: []harnessv2.MCPToolDescriptor{descriptor}}
	var err error
	policy.DescriptorDigest, err = harnessv2.CanonicalMCPToolDescriptorDigest(policy.Tools)
	if err != nil {
		return harnessv2.MCPBrokerCallRequest{}, err
	}
	toolDigest, err := harnessv2.CanonicalRuntimeToolPolicyDigest(policy.AllowedToolNames, nil, false)
	if err != nil {
		return harnessv2.MCPBrokerCallRequest{}, err
	}
	approvalPolicy := harnessv2.MCPApprovalPolicy{RequiredTools: []string{descriptor.Name}}
	approvalDigest, err := harnessv2.CanonicalMCPApprovalPolicyDigest(approvalPolicy)
	if err != nil {
		return harnessv2.MCPBrokerCallRequest{}, err
	}
	mcpDigest, err := harnessv2.CanonicalMCPConfigurationDigest(policy.AllowedToolNames)
	if err != nil {
		return harnessv2.MCPBrokerCallRequest{}, err
	}
	fence := harnessv2.Fence{
		RuntimeInstanceID: "approval-runtime", SupervisorBootID: "approval-boot", ControllerEpoch: 1,
		RuntimePoolUID: "approval-pool", RuntimePoolGeneration: 1, RuntimeSessionUID: "approval-session", RuntimeSessionGeneration: 1,
		RuntimeProfileDigest:       harnessv2.ProfileDigest(store.CanonicalBytesDigest([]byte("approval-profile"))),
		ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
	}
	call := harnessv2.MCPBrokerCallRequest{
		Protocol: harnessv2.ProtocolVersion, Namespace: "default", SessionState: harnessv2.RuntimeSessionStatePromptRunning,
		Metadata: harnessv2.MutationMetadata{
			Fence: fence, TaskUID: "approval-task-uid", TaskAttempt: 1, PromptID: "approval-prompt", OperationID: "approval-operation",
			RequestDigestSchemaVersion: harnessv2.RequestDigestSchemaVersion, ExpiresAt: now.Add(30 * time.Second),
		},
		Lease: harnessv2.PromptLease{Generation: 1, IssuedAt: now.Add(-time.Second), ExpiresAt: now.Add(2 * time.Minute)},
		Authorization: harnessv2.PromptMCPAuthorization{
			RuntimeSessionUID: fence.RuntimeSessionUID, SessionGeneration: fence.RuntimeSessionGeneration,
			TaskUID: "approval-task-uid", TaskAttempt: 1, PromptID: "approval-prompt", LeaseGeneration: 1,
			ToolPolicyDigest: toolDigest, ApprovalPolicyDigest: approvalDigest, MCPConfigurationDigest: mcpDigest,
			ToolPolicy: policy, ApprovalPolicy: approvalPolicy, ExpiresAt: now.Add(time.Minute),
		},
		Call: harnessv2.MCPToolCall{CallID: "approval-call", ToolName: descriptor.Name, Arguments: json.RawMessage(`{"title":"Fixture work order","priority":"low"}`)},
	}
	call.Metadata.RequestDigest, err = harnessv2.CanonicalRequestDigest(call)
	return call, err
}

func v2ApprovalAPIUser(role string) authenticationv1.UserInfo {
	return authenticationv1.UserInfo{
		Username: "system:serviceaccount:default:" + role, UID: role + "-uid",
		Groups: []string{"system:serviceaccounts", "system:serviceaccounts:default", "system:authenticated"},
		Extra:  map[string]authenticationv1.ExtraValue{"example.com/reviewer": {role}},
	}
}

type v2ApprovalAPIResponse struct {
	status int
	body   []byte
	err    error
}

func (f *v2ApprovalAPIFixture) request(method, path, role, body string) v2ApprovalAPIResponse {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if role != "" {
		req.Header.Set("Authorization", "Bearer "+f.tokens[role])
	}
	response, err := f.server.app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		return v2ApprovalAPIResponse{err: err}
	}
	defer response.Body.Close() //nolint:errcheck
	data, err := io.ReadAll(response.Body)
	return v2ApprovalAPIResponse{status: response.StatusCode, body: data, err: err}
}

func (f *v2ApprovalAPIFixture) list(t *testing.T) []approvals.Approval {
	t.Helper()
	response := f.request(http.MethodGet, "/api/v1/tasks/approval-task/approvals?namespace=default", "reader", "")
	require.NoError(t, response.err)
	require.Equal(t, http.StatusOK, response.status, "%s", response.body)
	var listed ListTaskApprovalsResponse
	require.NoError(t, json.Unmarshal(response.body, &listed))
	require.Equal(t, f.task.Namespace, listed.Namespace)
	require.Equal(t, f.task.Name, listed.TaskName)
	current := &corev1alpha1.Task{}
	require.NoError(t, f.kube.Get(t.Context(), client.ObjectKeyFromObject(f.task), current))
	require.Equal(t, string(current.UID), listed.TaskUID, "the list must name the Task it was filtered against, even after the name is reused")
	require.Equal(t, string(current.Status.Phase), listed.TaskPhase)
	require.Equal(t, !current.DeletionTimestamp.IsZero(), listed.TaskDeleting)
	return listed.Approvals
}

func (f *v2ApprovalAPIFixture) pending(t *testing.T) approvals.Approval {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		for _, approval := range f.list(t) {
			if approval.Status == approvals.StatusPending {
				return approval
			}
		}
		select {
		case <-deadline.C:
			t.Fatal("broker did not publish a pending approval through the API")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (f *v2ApprovalAPIFixture) decide(role, id, decision string) v2ApprovalAPIResponse {
	return f.request(http.MethodPost, "/api/v1/tasks/approval-task/approvals/"+id+"/decision?namespace=default", role,
		fmt.Sprintf(`{"decision":%q,"reason":"Reviewed exact inputs"}`, decision))
}

type v2ApprovalAPIBrokerCall struct {
	done   <-chan *httptest.ResponseRecorder
	cancel context.CancelFunc
}

func (f *v2ApprovalAPIFixture) start(t *testing.T) v2ApprovalAPIBrokerCall {
	t.Helper()
	body, err := json.Marshal(f.call)
	require.NoError(t, err)
	capability, err := harnessv2.SignOperationCapability(bytes.Repeat([]byte("c"), 32), harnessv2.ClaimsForMutation(f.call.Metadata))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, harnessv2.MCPBrokerCallPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("b", 32))
	req.Header.Set(harnessv2.OperationCapabilityHeader, capability)
	done := make(chan *httptest.ResponseRecorder, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		response := httptest.NewRecorder()
		f.broker.ServeHTTP(response, req)
		done <- response
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("broker did not stop after test cleanup cancelled its call")
		}
	})
	return v2ApprovalAPIBrokerCall{done: done, cancel: cancel}
}

func (call v2ApprovalAPIBrokerCall) response(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case response := <-call.done:
		return response
	case <-time.After(5 * time.Second):
		t.Fatal("broker did not complete the original call")
		return nil
	}
}

func (call v2ApprovalAPIBrokerCall) result(t *testing.T) harnessv2.MCPBrokerCallResponse {
	t.Helper()
	response := call.response(t)
	require.Equal(t, http.StatusOK, response.Code, "%s", response.Body.Bytes())
	var result harnessv2.MCPBrokerCallResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
	return result
}

func (f *v2ApprovalAPIFixture) requireWaiting(t *testing.T, call v2ApprovalAPIBrokerCall) {
	t.Helper()
	require.Zero(t, f.count.Load(), "pending approval must not execute")
	select {
	case response := <-call.done:
		t.Fatalf("broker returned before a decision: status=%d body=%s", response.Code, response.Body.Bytes())
	default:
	}
}

func (f *v2ApprovalAPIFixture) decisionEvents(t *testing.T) []store.ExecutionEvent {
	t.Helper()
	listed, err := approvals.ListEvents(t.Context(), f.store, f.task.Namespace, f.task.Name)
	require.NoError(t, err)
	var decisions []store.ExecutionEvent
	for _, event := range listed {
		if event.Type == events.ExecutionEventTypeApprovalApproved || event.Type == events.ExecutionEventTypeApprovalDeclined {
			decisions = append(decisions, event)
		}
	}
	return decisions
}

func requireV2ApprovalAPIError(t *testing.T, result harnessv2.MCPBrokerCallResponse, code string) {
	t.Helper()
	require.True(t, result.IsError)
	var envelope struct {
		IsError bool   `json:"isError"`
		Code    string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(result.Result, &envelope))
	require.True(t, envelope.IsError)
	require.Equal(t, code, envelope.Code)
}

func TestV2ApprovalAPIApproveExecutesOriginalCallOnce(t *testing.T) {
	f := newV2ApprovalAPIFixture(t)
	f.call.Call.CallID = "private-runtime-call-id-canary"
	var err error
	f.call.Metadata.RequestDigest, err = harnessv2.CanonicalRequestDigest(f.call)
	require.NoError(t, err)
	call := f.start(t)
	pending := f.pending(t)
	f.requireWaiting(t, call)
	require.Equal(t, "not_started", pending.ExecutionOutcome)
	require.Equal(t, string(f.task.UID), pending.TaskUID)
	require.Equal(t, f.call.Call.ToolName, pending.TargetTool)
	require.Equal(t, store.CanonicalBytesDigest([]byte(f.call.Call.CallID)), pending.ToolCallID)
	require.JSONEq(t, string(f.call.Call.Arguments), string(pending.TargetArgsPreview))
	require.NotNil(t, pending.Binding)
	require.Equal(t, pending.Binding.CallIDDigest, pending.ToolCallID)
	require.Equal(t, string(f.call.Metadata.PromptID), pending.Binding.PromptID)
	require.Equal(t, f.call.Metadata.TaskAttempt, pending.Binding.TaskAttempt)
	require.NotNil(t, pending.ExpiresAt)

	response := f.decide("reviewer-a", pending.ID, "approve")
	require.NoError(t, response.err)
	require.Equal(t, http.StatusOK, response.status, "%s", response.body)
	result := call.result(t)
	require.False(t, result.IsError)
	require.False(t, result.Replayed)
	require.Equal(t, f.call.Call.CallID, result.CallID)
	require.JSONEq(t, `{"workOrder":"fixture-order-1","created":true}`, string(result.Result))
	require.EqualValues(t, 1, f.count.Load())

	// The persisted receipt supplies redelivery after a new handler is created.
	restarted := *f.broker
	f.broker = &restarted
	replay := f.start(t).result(t)
	require.True(t, replay.Replayed)
	require.JSONEq(t, string(result.Result), string(replay.Result))
	require.EqualValues(t, 1, f.count.Load())
	response = f.decide("reviewer-b", pending.ID, "approve")
	require.NoError(t, response.err)
	require.Equal(t, http.StatusOK, response.status, "%s", response.body)
	response = f.decide("reviewer-b", pending.ID, "decline")
	require.NoError(t, response.err)
	require.Equal(t, http.StatusConflict, response.status, "%s", response.body)
	listed := f.list(t)
	require.Len(t, listed, 1)
	require.Equal(t, approvals.StatusApproved, listed[0].Status)
	require.Equal(t, "succeeded", listed[0].ExecutionOutcome)
	require.Equal(t, v2ApprovalAPIUser("reviewer-a").Username, listed[0].DecisionActor)
	require.Len(t, f.decisionEvents(t), 1)
	publicEvents, err := approvals.ListEvents(t.Context(), f.store, f.task.Namespace, f.task.Name)
	require.NoError(t, err)
	encoded, err := json.Marshal(publicEvents)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), f.call.Call.CallID)
}

func TestV2ApprovalAPIPreservesPrivateRuntimeIdentity(t *testing.T) {
	f := newV2ApprovalAPIFixture(t, func(call *harnessv2.MCPBrokerCallRequest) {
		call.Metadata.Fence.RuntimeInstanceID = "https://runtime.example/instance?slot=1"
		call.Metadata.Fence.SupervisorBootID = "https://runtime.example/boot?slot=1"
		call.Metadata.OperationID = "https://runtime.example/operation?slot=1"
	})
	_, err := f.call.ValidateAt(time.Now().UTC())
	require.NoError(t, err, "URL-shaped runtime identifiers are valid protocol inputs")
	call := f.start(t)
	pending := f.pending(t)
	f.requireWaiting(t, call)
	public, err := json.Marshal(pending)
	require.NoError(t, err)
	require.NotContains(t, string(public), "runtime.example")
	require.Equal(t, store.CanonicalBytesDigest([]byte(f.call.Metadata.Fence.RuntimeInstanceID)), pending.Binding.RuntimeInstanceIDDigest)
	require.Equal(t, store.CanonicalBytesDigest([]byte(f.call.Metadata.Fence.SupervisorBootID)), pending.Binding.SupervisorBootIDDigest)
	require.Equal(t, store.CanonicalBytesDigest([]byte(f.call.Metadata.OperationID)), pending.Binding.OperationIDDigest)

	// Sanitization would make either replacement look like the original ID.
	// Neither replacement may authorize this review, even under the same Task.
	for _, replace := range []func(*corev1alpha1.TaskExecutionStatus){
		func(execution *corev1alpha1.TaskExecutionStatus) {
			execution.RuntimeInstanceID = "https://runtime.example/instance?slot=2"
		},
		func(execution *corev1alpha1.TaskExecutionStatus) {
			execution.RuntimeSessionSupervisorBootID = "https://runtime.example/boot?slot=2"
		},
	} {
		current := &corev1alpha1.Task{}
		require.NoError(t, f.kube.Get(t.Context(), client.ObjectKeyFromObject(f.task), current))
		original := current.Status.Execution.DeepCopy()
		replace(current.Status.Execution)
		require.NoError(t, f.kube.Status().Update(t.Context(), current))
		response := f.decide("reviewer-a", pending.ID, "approve")
		require.NoError(t, response.err)
		require.Equal(t, http.StatusConflict, response.status, "%s", response.body)
		require.Empty(t, f.decisionEvents(t))
		f.requireWaiting(t, call)
		current.Status.Execution = original
		require.NoError(t, f.kube.Status().Update(t.Context(), current))
	}

	response := f.decide("reviewer-a", pending.ID, "approve")
	require.NoError(t, response.err)
	require.Equal(t, http.StatusOK, response.status, "%s", response.body)
	result := call.result(t)
	require.False(t, result.IsError)
	require.False(t, result.Replayed)
	require.EqualValues(t, 1, f.count.Load())
	replay := f.start(t).result(t)
	require.True(t, replay.Replayed)
	require.JSONEq(t, string(result.Result), string(replay.Result))
	require.EqualValues(t, 1, f.count.Load())
	listed, err := approvals.ListEvents(t.Context(), f.store, f.task.Namespace, f.task.Name)
	require.NoError(t, err)
	public, err = json.Marshal(listed)
	require.NoError(t, err)
	require.NotContains(t, string(public), "runtime.example")
}

func TestV2ApprovalAPIURLDecodedDecisionUsesOriginalApproval(t *testing.T) {
	f := newV2ApprovalAPIFixture(t)
	call := f.start(t)
	pending := f.pending(t)
	f.requireWaiting(t, call)
	// The panel uses encodeURIComponent for the colon-delimited broker ID.
	encodedID := strings.ReplaceAll(pending.ID, ":", "%3A")
	require.NotEqual(t, pending.ID, encodedID)
	response := f.decide("reviewer-a", encodedID, "approve")
	require.NoError(t, response.err)
	require.Equal(t, http.StatusOK, response.status, "%s", response.body)
	result := call.result(t)
	require.False(t, result.IsError)
	require.EqualValues(t, 1, f.count.Load())

	// Encoded and unencoded delivery refer to the same recorded decision.
	response = f.decide("reviewer-b", pending.ID, "approve")
	require.NoError(t, response.err)
	require.Equal(t, http.StatusOK, response.status, "%s", response.body)
	require.Len(t, f.decisionEvents(t), 1)
	require.EqualValues(t, 1, f.count.Load())
	require.Equal(t, pending.ID, f.list(t)[0].ID)
}

func TestV2ApprovalAPIDeclineNeverExecutes(t *testing.T) {
	f := newV2ApprovalAPIFixture(t)
	call := f.start(t)
	pending := f.pending(t)
	f.requireWaiting(t, call)
	response := f.decide("reviewer-a", pending.ID, "decline")
	require.NoError(t, response.err)
	require.Equal(t, http.StatusOK, response.status, "%s", response.body)
	requireV2ApprovalAPIError(t, call.result(t), "approval_declined")
	requireV2ApprovalAPIError(t, f.start(t).result(t), "approval_declined")
	listed := f.list(t)
	require.Len(t, listed, 1)
	require.Equal(t, approvals.StatusDeclined, listed[0].Status)
	require.Equal(t, "not_started", listed[0].ExecutionOutcome)
	require.Equal(t, v2ApprovalAPIUser("reviewer-a").Username, listed[0].DecisionActor)
	require.Len(t, f.decisionEvents(t), 1)
	require.Zero(t, f.count.Load())
}

func TestV2ApprovalAPIRequiresAuthenticatedReviewerPermissions(t *testing.T) {
	f := newV2ApprovalAPIFixture(t)
	call := f.start(t)
	pending := f.pending(t)
	for _, tc := range []struct {
		role   string
		status int
	}{
		{role: "", status: http.StatusUnauthorized},
		{role: "reader", status: http.StatusForbidden},
		{role: "approval-only", status: http.StatusForbidden},
		{role: "outsider", status: http.StatusForbidden},
	} {
		response := f.decide(tc.role, pending.ID, "approve")
		require.NoError(t, response.err)
		require.Equal(t, tc.status, response.status, "role=%q body=%s", tc.role, response.body)
		f.requireWaiting(t, call)
		require.Empty(t, f.decisionEvents(t))
	}
	response := f.request(http.MethodGet, "/api/v1/tasks/approval-task/approvals?namespace=default", "outsider", "")
	require.NoError(t, response.err)
	require.Equal(t, http.StatusForbidden, response.status)
	current := &corev1alpha1.Task{}
	require.NoError(t, f.kube.Get(t.Context(), client.ObjectKeyFromObject(f.task), current))
	require.Empty(t, current.Annotations[labels.AnnotationApprovalDecisionID])
	require.Positive(t, f.tokenReviews.Load())

	f.reviewsMu.Lock()
	reviews := append([]authorizationv1.SubjectAccessReviewSpec(nil), f.reviews...)
	f.reviewsMu.Unlock()
	for _, tc := range []struct{ role, verb, subresource string }{
		{"reader", "update", "approvals"},
		{"approval-only", "update", "approvals"},
		{"approval-only", "patch", ""},
	} {
		user := v2ApprovalAPIUser(tc.role)
		require.Contains(t, reviews, authorizationv1.SubjectAccessReviewSpec{
			User: user.Username, UID: user.UID, Groups: user.Groups,
			Extra: map[string]authorizationv1.ExtraValue{"example.com/reviewer": {tc.role}},
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: f.task.Namespace, Verb: tc.verb, Group: corev1alpha1.GroupVersion.Group,
				Resource: "tasks", Subresource: tc.subresource, Name: f.task.Name,
			},
		})
	}
	response = f.decide("reviewer-a", pending.ID, "decline")
	require.NoError(t, response.err)
	require.Equal(t, http.StatusOK, response.status, "%s", response.body)
	requireV2ApprovalAPIError(t, call.result(t), "approval_declined")
	require.Zero(t, f.count.Load())
}

type v2ApprovalAPIDecisionBarrier struct {
	store.ExecutionEventStore
	arrived chan struct{}
	release chan struct{}
}

func (s *v2ApprovalAPIDecisionBarrier) AppendExecutionEvent(ctx context.Context, event *store.ExecutionEvent) (*store.ExecutionEvent, error) {
	if event.Type == events.ExecutionEventTypeApprovalApproved || event.Type == events.ExecutionEventTypeApprovalDeclined {
		select {
		case s.arrived <- struct{}{}:
		default:
		}
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.ExecutionEventStore.AppendExecutionEvent(ctx, event)
}

func TestV2ApprovalAPIConcurrentDecisionsHaveOneDurableWinner(t *testing.T) {
	for _, decisions := range [][2]string{{"approve", "approve"}, {"approve", "decline"}} {
		t.Run(strings.Join(decisions[:], "-"), func(t *testing.T) {
			f := newV2ApprovalAPIFixture(t)
			call := f.start(t)
			pending := f.pending(t)
			barrier := &v2ApprovalAPIDecisionBarrier{ExecutionEventStore: f.store, arrived: make(chan struct{}, 2), release: make(chan struct{})}
			release := sync.OnceFunc(func() { close(barrier.release) })
			t.Cleanup(release)
			f.server.handlers.executionEventStore = barrier
			done := make(chan v2ApprovalAPIResponse, 2)
			for index, decision := range decisions {
				go func() { done <- f.decide([]string{"reviewer-a", "reviewer-b"}[index], pending.ID, decision) }()
			}
			// Both handlers observed Pending and reached the real store append.
			for range decisions {
				select {
				case <-barrier.arrived:
				case <-time.After(3 * time.Second):
					t.Fatal("both decisions did not reach the append barrier")
				}
			}
			f.requireWaiting(t, call)
			release()
			statuses := make([]int, 0, 2)
			for range decisions {
				response := <-done
				require.NoError(t, response.err)
				statuses = append(statuses, response.status)
			}
			if decisions[0] == decisions[1] {
				require.Equal(t, []int{http.StatusOK, http.StatusOK}, statuses)
			} else {
				require.ElementsMatch(t, []int{http.StatusOK, http.StatusConflict}, statuses)
			}
			result := call.result(t)
			listed := f.list(t)
			require.Len(t, listed, 1)
			require.Len(t, f.decisionEvents(t), 1)
			if listed[0].Status == approvals.StatusApproved {
				require.False(t, result.IsError)
				require.JSONEq(t, `{"workOrder":"fixture-order-1","created":true}`, string(result.Result))
				require.EqualValues(t, 1, f.count.Load())
			} else {
				require.Equal(t, approvals.StatusDeclined, listed[0].Status)
				requireV2ApprovalAPIError(t, result, "approval_declined")
				require.Zero(t, f.count.Load())
			}
		})
	}
}

func TestV2ApprovalAPILateDecisionsCannotExecuteAnotherTaskRun(t *testing.T) {
	for _, scenario := range []string{"task-replaced", "attempt-replaced", "prompt-replaced"} {
		t.Run(scenario, func(t *testing.T) {
			f := newV2ApprovalAPIFixture(t)
			call := f.start(t)
			pending := f.pending(t)
			current := &corev1alpha1.Task{}
			require.NoError(t, f.kube.Get(t.Context(), client.ObjectKeyFromObject(f.task), current))
			wantStatus := http.StatusConflict
			if scenario == "task-replaced" {
				require.NoError(t, f.kube.Delete(t.Context(), current))
				current.UID = "replacement-task-uid"
				current.ResourceVersion = ""
				require.NoError(t, f.kube.Create(t.Context(), current))
				require.Empty(t, f.list(t), "a reused Task name must not expose the previous Task's approvals")
				wantStatus = http.StatusNotFound
			} else {
				if scenario == "attempt-replaced" {
					current.Status.Execution.Attempt++
				} else {
					current.Status.Execution.PromptID = "replacement-prompt"
				}
				require.NoError(t, f.kube.Status().Update(t.Context(), current))
			}
			response := f.decide("reviewer-a", pending.ID, "approve")
			require.NoError(t, response.err)
			require.Equal(t, wantStatus, response.status, "%s", response.body)
			requireV2ApprovalAPIError(t, call.result(t), "approval_stale")
			require.Empty(t, f.decisionEvents(t))
			require.Zero(t, f.count.Load())
		})
	}
}

func TestV2ApprovalAPIExpiredReviewRejectsLateDecisions(t *testing.T) {
	f := newV2ApprovalAPIFixture(t)
	f.broker.ApprovalWaitTimeout = 500 * time.Millisecond
	call := f.start(t)
	pending := f.pending(t)
	f.requireWaiting(t, call)
	requireV2ApprovalAPIError(t, call.result(t), "approval_expired")
	for _, decision := range []string{"approve", "decline"} {
		response := f.decide("reviewer-a", pending.ID, decision)
		require.NoError(t, response.err)
		require.Equal(t, http.StatusConflict, response.status, "%s", response.body)
	}
	listed := f.list(t)
	require.Len(t, listed, 1)
	require.Equal(t, approvals.StatusExpired, listed[0].Status)
	require.Equal(t, "not_started", listed[0].ExecutionOutcome)
	require.Empty(t, f.decisionEvents(t))
	require.Zero(t, f.count.Load())
}

func TestV2ApprovalAPIExpiresReviewAfterWaitingBrokerDisconnects(t *testing.T) {
	f := newV2ApprovalAPIFixture(t)
	f.broker.ApprovalWaitTimeout = 500 * time.Millisecond
	call := f.start(t)
	pending := f.pending(t)
	call.cancel()
	require.Equal(t, http.StatusServiceUnavailable, call.response(t).Code)
	// No broker is present to write an expiry event. The API must still enforce
	// the original deadline instead of accepting a decision that revives it.
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		listed := f.list(t)
		require.Len(t, listed, 1)
		if listed[0].Status == approvals.StatusExpired {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("API did not expire the disconnected broker's review")
		case <-time.After(5 * time.Millisecond):
		}
	}
	listed, err := approvals.ListEvents(t.Context(), f.store, f.task.Namespace, f.task.Name)
	require.NoError(t, err)
	require.Len(t, listed, 1, "only the original approval request should be persisted")
	require.Equal(t, events.ExecutionEventTypeApprovalRequested, listed[0].Type)
	response := f.decide("reviewer-a", pending.ID, "approve")
	require.NoError(t, response.err)
	require.Equal(t, http.StatusConflict, response.status, "%s", response.body)
	requireV2ApprovalAPIError(t, f.start(t).result(t), "approval_expired")
	require.Empty(t, f.decisionEvents(t))
	require.Zero(t, f.count.Load())
}
