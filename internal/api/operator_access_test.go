package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gofiber/fiber/v3"
	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	cliclient "github.com/orka-agents/orka/internal/cli/client"
	"github.com/orka-agents/orka/internal/controller"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/operatoraccess"
	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
)

const (
	operatorTestNamespace = "team"
	operatorTestToken     = "operator-controller-token-0123456789abcdef"
	operatorTestDigest    = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

var operatorTestCapabilitySecret = bytes.Repeat([]byte("k"), harnessv2.MinCapabilitySecretBytes)

type operatorAuditRecorder struct {
	mu      sync.Mutex
	records []OperatorAuditRecord
}

func (r *operatorAuditRecorder) Record(_ context.Context, record OperatorAuditRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, record)
}

func (r *operatorAuditRecorder) phases() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	phases := make([]string, 0, len(r.records))
	for _, record := range r.records {
		phases = append(phases, record.Phase)
	}
	return phases
}

type operatorFixture struct {
	client     client.Client
	server     *Server
	app        *fiber.App
	audit      *operatorAuditRecorder
	supervisor *httptest.Server
	user       *UserInfo

	mu               sync.Mutex
	supervisorCalls  []string
	taintedAtRequest bool
	noCapability     bool
}

func operatorTestObjects(classAccess string) []client.Object {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "t1", Namespace: operatorTestNamespace, UID: "task-uid",
			Labels:      map[string]string{operatorWorkspaceLinkLabel: "ws1"},
			Annotations: map[string]string{operatorWorkspaceUIDAnnotation: "ws-uid", operatorTaskAttachmentEpochAnnot: "1"},
		},
		Spec: corev1alpha1.TaskSpec{Execution: &corev1alpha1.ExecutionSpec{
			Workspace: &corev1alpha1.ExecutionWorkspaceSpec{ClassRef: &corev1alpha1.WorkspaceClassReference{Name: "cls"}},
		}},
		Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{
			State: corev1alpha1.TaskExecutionStateRunning, RuntimePoolName: "pool1", RuntimePoolUID: "pool-uid",
			RuntimeSessionUID: "sess", RuntimeSessionGeneration: 2, RuntimeInstanceID: "pod-uid.boot",
		}},
	}
	classAnnotations := map[string]string{}
	if classAccess != "" {
		classAnnotations[operatoraccess.ClassAccessAnnotation] = classAccess
	}
	return []client.Object{
		task,
		&workspacev1alpha1.ExecutionWorkspaceClass{ObjectMeta: metav1.ObjectMeta{
			Name: "cls", Namespace: operatorTestNamespace, UID: "class-uid", Annotations: classAnnotations,
		}},
		&workspacev1alpha1.ExecutionWorkspace{
			ObjectMeta: metav1.ObjectMeta{
				Name: "ws1", Namespace: operatorTestNamespace, UID: "ws-uid",
				Annotations: map[string]string{operatorWorkspacePoolAnnotation: "pool1"},
			},
			Spec: workspacev1alpha1.ExecutionWorkspaceSpec{
				DesiredState:    workspacev1alpha1.ExecutionWorkspaceDesiredReady,
				ClassBinding:    workspacev1alpha1.ImmutableObjectBinding{Name: "cls", UID: "class-uid"},
				AttachmentEpoch: 1,
				Attachment: &workspacev1alpha1.ExecutionWorkspaceAttachment{
					TaskRef: workspacev1alpha1.ObjectIdentityReference{Name: "t1", UID: "task-uid"}, Epoch: 1,
				},
			},
		},
		&corev1alpha1.RuntimePool{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pool1", Namespace: operatorTestNamespace, UID: "pool-uid", Generation: 1,
				Labels:      map[string]string{operatorWorkspaceLinkLabel: "ws1"},
				Annotations: map[string]string{runtimePoolPrivateAuthBindingAPI + "4": "pool1-auth/secret-uid"},
			},
			Spec: corev1alpha1.RuntimePoolSpec{ExecutionWorkspace: &corev1alpha1.RuntimePoolExecutionWorkspaceSpec{
				WorkspaceRef: &workspacev1alpha1.ObjectIdentityReference{Name: "ws1", UID: "ws-uid"},
			}},
			Status: corev1alpha1.RuntimePoolStatus{ActiveInstance: &corev1alpha1.RuntimePoolActiveInstanceStatus{
				PodNamespace: "orka-runtime", PodName: "pool1-pod", PodAddress: "10.0.0.7", PodUID: "pod-uid",
				BootID: "boot", RuntimeInstanceID: "pod-uid.boot", ControllerEpoch: 4,
				ProfileDigest: operatorTestDigest, ProfileDigestSchemaVersion: "1",
			}},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "pool1-pod", Namespace: "orka-runtime", UID: "pod-uid"},
			Status:     corev1.PodStatus{PodIP: "10.0.0.7"},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: "pool1-auth", Namespace: "orka-runtime", UID: "secret-uid",
				Labels: map[string]string{
					runtimePoolAuthLabelAPI: runtimePoolAuthLabelValueAPI, runtimePoolUIDLabelAPI: "pool-uid",
					runtimePoolCredentialEpochLabelAPI: "4",
				},
			},
			Immutable: new(true),
			Data: map[string][]byte{
				runtimePoolControllerTokenKeyAPI: []byte(operatorTestToken), runtimePoolCapabilitySecretKeyAPI: operatorTestCapabilitySecret,
			},
		},
	}
}

//go:fix inline

func newOperatorFixture(t *testing.T, enabled bool, classAccess string, mutate ...func([]client.Object)) *operatorFixture {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, workspacev1alpha1.AddToScheme(scheme))
	objects := operatorTestObjects(classAccess)
	for _, fn := range mutate {
		fn(objects)
	}
	f := &operatorFixture{audit: &operatorAuditRecorder{}, user: &UserInfo{Username: "alice", AuthType: AuthTypeTokenReview}}
	f.client = fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithStatusSubresource(&corev1alpha1.Task{}, &corev1alpha1.RuntimePool{}).Build()
	f.supervisor = httptest.NewServer(http.HandlerFunc(f.serveSupervisor))
	t.Cleanup(f.supervisor.Close)
	supervisorAddress := f.supervisor.Listener.Addr().String()
	httpClient := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, supervisorAddress)
	}}}
	f.server = &Server{
		client:   f.client,
		config:   ServerConfig{APIReader: f.client, OperatorAccess: OperatorAccessConfig{Enabled: enabled, Audit: f.audit, HTTPClient: httpClient}},
		handlers: NewHandlers(HandlersConfig{Client: f.client, APIReader: f.client}),
	}
	f.app = fiber.New(fiber.Config{ErrorHandler: customErrorHandler, StreamRequestBody: true})
	f.app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, f.user)
		return c.Next()
	})
	f.app.Get("/api/v1/tasks/:id/exec", f.server.HandleOperatorExec)
	f.app.Get("/api/v1/tasks/:id/files", f.server.HandleOperatorFileDownload)
	f.app.Put("/api/v1/tasks/:id/files", f.server.HandleOperatorFileUpload)
	return f
}

func (f *operatorFixture) workspaceTainted(t *testing.T) bool {
	t.Helper()
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	require.NoError(t, f.client.Get(context.Background(), types.NamespacedName{Namespace: operatorTestNamespace, Name: "ws1"}, workspace))
	return operatoraccess.Tainted(workspace.Annotations)
}

func (f *operatorFixture) taskTainted(t *testing.T) bool {
	t.Helper()
	task := &corev1alpha1.Task{}
	require.NoError(t, f.client.Get(context.Background(), types.NamespacedName{Namespace: operatorTestNamespace, Name: "t1"}, task))
	return operatoraccess.Tainted(task.Annotations)
}

func (f *operatorFixture) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.supervisorCalls...)
}

func (f *operatorFixture) serveSupervisor(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.supervisorCalls = append(f.supervisorCalls, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	if r.URL.Path == harnessv2.CapabilitiesPath {
		_ = json.NewEncoder(w).Encode(harnessv2.CapabilitiesResponse{
			RuntimeProfileDigest: operatorTestDigest, SupportsOperatorAccess: !f.noCapability && r.Header.Get(operatorAccessCapabilityHeader) == "1",
		})
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+operatorTestToken {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	_ = f.client.Get(context.Background(), types.NamespacedName{Namespace: operatorTestNamespace, Name: "ws1"}, workspace)
	f.mu.Lock()
	f.taintedAtRequest = operatoraccess.Tainted(workspace.Annotations)
	f.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/operator-files"):
		var request harnessv2.OperatorFileRequest
		if err := harnessv2.DecodeOperatorRequestHeader(r.Header.Get(harnessv2.OperatorRequestHeader), &request); err != nil ||
			harnessv2.VerifyOperationCapability(operatorTestCapabilitySecret, r.Header.Get(harnessv2.OperationCapabilityHeader), request.Metadata, true, time.Now().UTC()) != nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if request.Metadata.Fence.RuntimeSessionUID != "sess" || request.Metadata.Fence.RuntimeSessionGeneration != 2 {
			http.Error(w, "fence", http.StatusConflict)
			return
		}
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Length", "7")
			_, _ = w.Write([]byte("content"))
			return
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(harnessv2.OperatorFileResult{Protocol: harnessv2.ProtocolVersion, Bytes: int64(len(body))})
	case strings.HasSuffix(r.URL.Path, "/operator-exec"):
		var request harnessv2.OperatorExecRequest
		if err := harnessv2.DecodeOperatorRequestHeader(r.Header.Get(harnessv2.OperatorRequestHeader), &request); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{operatoraccess.WebSocketSubprotocol}})
		if err != nil {
			return
		}
		ctx := context.Background()
		_ = conn.Write(ctx, websocket.MessageBinary, operatoraccess.Frame(operatoraccess.ChannelStdout, []byte(strings.Join(request.Exec.Argv, " "))))
		_ = conn.Write(ctx, websocket.MessageBinary, operatoraccess.StatusFrame(operatoraccess.Status{ExitCode: 7, Reason: operatoraccess.ExitReasonExited}))
		_ = conn.Close(websocket.StatusNormalClosure, "")
	default:
		http.NotFound(w, r)
	}
}

func (f *operatorFixture) do(t *testing.T, method, target string, body io.Reader) (int, string) {
	t.Helper()
	request := httptest.NewRequest(method, target, body)
	response, err := f.app.Test(request, fiber.TestConfig{Timeout: 10 * time.Second})
	require.NoError(t, err)
	defer response.Body.Close() //nolint:errcheck
	payload, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(payload)
}

func TestOperatorAccessIsDisabledByDefault(t *testing.T) {
	f := newOperatorFixture(t, false, "exec")
	status, body := f.do(t, http.MethodGet, "/api/v1/tasks/t1/files?namespace=team&path=a", nil)
	require.Equal(t, http.StatusForbidden, status, body)
	require.Contains(t, body, "disabled")
	require.Empty(t, f.calls())
}

func TestOperatorAccessRequiresClassOptIn(t *testing.T) {
	f := newOperatorFixture(t, true, "")
	status, body := f.do(t, http.MethodGet, "/api/v1/tasks/t1/files?namespace=team&path=a", nil)
	require.Equal(t, http.StatusForbidden, status, body)
	require.Contains(t, body, operatoraccess.ClassAccessAnnotation)
	require.Empty(t, f.calls(), "a class without the opt-in must not reach the supervisor")
	require.Equal(t, []string{OperatorAuditDenied}, f.audit.phases())

	read := newOperatorFixture(t, true, "read")
	status, body = read.do(t, http.MethodPut, "/api/v1/tasks/t1/files?namespace=team&path=a&size=1", strings.NewReader("x"))
	require.Equal(t, http.StatusForbidden, status, body)
	require.False(t, read.workspaceTainted(t))
	require.False(t, read.taskTainted(t))
}

func TestOperatorAccessCallerTypes(t *testing.T) {
	require.Error(t, authorizeOperatorCaller(nil, operatoraccess.OperationExec, "team"))
	require.NoError(t, authorizeOperatorCaller(&UserInfo{AuthType: AuthTypeTokenReview}, operatoraccess.OperationExec, "team"))
	require.Error(t, authorizeOperatorCaller(&UserInfo{AuthType: AuthTypeOIDC, Subject: "alice"}, operatoraccess.OperationCopyOut, "team"),
		"OIDC identities have no Kubernetes identity to review")
	taskScoped := &UserInfo{AuthType: AuthTypeContextToken, Namespace: "team",
		ContextToken: &ContextToken{Scopes: []string{ContextTokenScopeTaskGet, ContextTokenScopeTaskCreate}}}
	require.Error(t, authorizeOperatorCaller(taskScoped, operatoraccess.OperationCopyOut, "team"), "task scopes never imply operator scopes")
	readScoped := &UserInfo{AuthType: AuthTypeContextToken, Namespace: "team",
		ContextToken: &ContextToken{Scopes: []string{operatoraccess.ScopeFilesRead}}}
	require.NoError(t, authorizeOperatorCaller(readScoped, operatoraccess.OperationCopyOut, "team"))
	require.Error(t, authorizeOperatorCaller(readScoped, operatoraccess.OperationCopyIn, "team"))
	require.Error(t, authorizeOperatorCaller(readScoped, operatoraccess.OperationExec, "team"))
	require.Error(t, authorizeOperatorCaller(readScoped, operatoraccess.OperationCopyOut, "other"), "context tokens are namespace-bound")
	unbound := &UserInfo{AuthType: AuthTypeContextToken, ContextToken: &ContextToken{Scopes: []string{operatoraccess.ScopeExec}}}
	require.Error(t, authorizeOperatorCaller(unbound, operatoraccess.OperationExec, "team"))
}

func TestOperatorAccessOIDCCallerRefusedBeforeEffects(t *testing.T) {
	f := newOperatorFixture(t, true, "exec")
	f.user = &UserInfo{AuthType: AuthTypeOIDC, Subject: "alice"}
	status, body := f.do(t, http.MethodPut, "/api/v1/tasks/t1/files?namespace=team&path=a&size=1", strings.NewReader("x"))
	require.Equal(t, http.StatusForbidden, status, body)
	require.Empty(t, f.calls())
	require.False(t, f.workspaceTainted(t))
}

func TestOperatorAccessRefusesSessionsTheTaskDoesNotOwn(t *testing.T) {
	cases := map[string]struct {
		mutate func([]client.Object)
		status int
	}{
		"attached to another task": {func(objects []client.Object) {
			objects[2].(*workspacev1alpha1.ExecutionWorkspace).Spec.Attachment.TaskRef.UID = "other-task"
		}, http.StatusConflict},
		"detached and since re-attached": {func(objects []client.Object) {
			workspace := objects[2].(*workspacev1alpha1.ExecutionWorkspace)
			workspace.Spec.Attachment = nil
			workspace.Spec.AttachmentEpoch = 2
		}, http.StatusConflict},
		"workspace replaced": {func(objects []client.Object) {
			objects[2].(*workspacev1alpha1.ExecutionWorkspace).UID = "new-ws-uid"
		}, http.StatusGone},
		"workspace validating for publication": {func(objects []client.Object) {
			objects[0].(*corev1alpha1.Task).Status.Execution.State = corev1alpha1.TaskExecutionStateSettling
		}, http.StatusConflict},
		"workspace suspended": {func(objects []client.Object) {
			objects[2].(*workspacev1alpha1.ExecutionWorkspace).Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredSuspended
		}, http.StatusConflict},
		"not class-backed": {func(objects []client.Object) {
			objects[0].(*corev1alpha1.Task).Spec.Execution = nil
		}, http.StatusConflict},
		"class replaced": {func(objects []client.Object) {
			objects[1].(*workspacev1alpha1.ExecutionWorkspaceClass).UID = "new-class"
		}, http.StatusForbidden},
		"runtime instance replaced": {func(objects []client.Object) {
			objects[3].(*corev1alpha1.RuntimePool).Status.ActiveInstance.RuntimeInstanceID = "other.boot"
		}, http.StatusGone},
		"pod address reused": {func(objects []client.Object) {
			objects[4].(*corev1.Pod).UID = "another-pod"
		}, http.StatusGone},
		"native-process workspace": {func(objects []client.Object) {
			objects[3].(*corev1alpha1.RuntimePool).Spec.ExecutionWorkspace.Workload = &corev1alpha1.RuntimePoolWorkspaceWorkloadSpec{
				RequiredFeatures: []workspacev1alpha1.ExecutionWorkspaceFeature{workspacev1alpha1.WorkspaceFeatureNativeProcess},
			}
		}, http.StatusNotImplemented},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newOperatorFixture(t, true, "exec", tc.mutate)
			status, body := f.do(t, http.MethodPut, "/api/v1/tasks/t1/files?namespace=team&path=a&size=1", strings.NewReader("x"))
			require.Equal(t, tc.status, status, body)
			require.False(t, f.workspaceTainted(t))
			for _, call := range f.calls() {
				require.NotContains(t, call, "operator-", "a refused request must not reach the supervisor data plane")
			}
		})
	}
}

func TestOperatorAccessRequiresSupervisorSupport(t *testing.T) {
	f := newOperatorFixture(t, true, "exec")
	f.noCapability = true
	status, body := f.do(t, http.MethodGet, "/api/v1/tasks/t1/files?namespace=team&path=a", nil)
	require.Equal(t, http.StatusNotImplemented, status, body)
}

func TestOperatorCopyOutDoesNotTaint(t *testing.T) {
	f := newOperatorFixture(t, true, "read")
	status, body := f.do(t, http.MethodGet, "/api/v1/tasks/t1/files?namespace=team&path=/src/main.go", nil)
	require.Equal(t, http.StatusOK, status, body)
	require.Equal(t, "content", body)
	require.False(t, f.workspaceTainted(t))
	require.False(t, f.taskTainted(t))
	require.Eventually(t, func() bool {
		phases := f.audit.phases()
		return len(phases) == 2 && phases[0] == OperatorAuditStarted && phases[1] == OperatorAuditFinished
	}, 5*time.Second, 10*time.Millisecond)
	f.audit.mu.Lock()
	defer f.audit.mu.Unlock()
	require.Equal(t, "src/main.go", f.audit.records[1].Path)
	require.Equal(t, int64(7), f.audit.records[1].Bytes)
	require.Equal(t, "alice", f.audit.records[1].Actor)
	require.Equal(t, "sess", f.audit.records[1].RuntimeSessionUID)
}

func TestOperatorCopyInTaintsBeforeWriting(t *testing.T) {
	f := newOperatorFixture(t, true, "exec")
	status, body := f.do(t, http.MethodPut, "/api/v1/tasks/t1/files?namespace=team&path=notes.txt&size=5", strings.NewReader("hello"))
	require.Equal(t, http.StatusOK, status, body)
	require.True(t, f.workspaceTainted(t))
	require.True(t, f.taskTainted(t))
	f.mu.Lock()
	require.True(t, f.taintedAtRequest, "the taint must be durable before the supervisor receives the write")
	f.mu.Unlock()
	require.Equal(t, []string{OperatorAuditStarted, OperatorAuditFinished}, f.audit.phases())
}

func TestOperatorCopyRejectsTraversalBeforeEffects(t *testing.T) {
	for _, path := range []string{"../etc/passwd", "a/../../b", "a%00b", "", "/"} {
		f := newOperatorFixture(t, true, "exec")
		status, body := f.do(t, http.MethodPut, "/api/v1/tasks/t1/files?namespace=team&size=1&path="+path, strings.NewReader("x"))
		require.Equal(t, http.StatusBadRequest, status, "%s: %s", path, body)
		require.False(t, f.workspaceTainted(t), path)
		for _, call := range f.calls() {
			require.NotContains(t, call, "operator-files", path)
		}
		status, body = f.do(t, http.MethodGet, "/api/v1/tasks/t1/files?namespace=team&path="+path, nil)
		require.Equal(t, http.StatusBadRequest, status, "%s: %s", path, body)
	}
}

func TestOperatorExecEndToEndThroughTheAPI(t *testing.T) {
	f := newOperatorFixture(t, true, "exec")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = f.app.Listener(listener, fiber.ListenConfig{DisableStartupMessage: true}) }()
	t.Cleanup(func() { _ = f.app.Shutdown() })

	// Plain HTTP (no upgrade) is refused with a clear error.
	response, err := http.Get("http://" + listener.Addr().String() + "/api/v1/tasks/t1/exec?namespace=team")
	require.NoError(t, err)
	_ = response.Body.Close()
	require.Equal(t, http.StatusUpgradeRequired, response.StatusCode)
	require.False(t, f.workspaceTainted(t))

	cli := cliclient.NewWithNamespace("http://"+listener.Addr().String(), "token", operatorTestNamespace)
	var stdout bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	status, err := cli.OperatorExec(ctx, "t1",
		operatoraccess.ExecOptions{Argv: []string{"printenv", "SECRET=hunter2"}},
		cliclient.OperatorExecStreams{Stdout: &stdout})
	require.NoError(t, err)
	require.Equal(t, 7, status.ExitCode)
	require.Equal(t, "printenv SECRET=hunter2", stdout.String())
	require.True(t, f.workspaceTainted(t))
	require.True(t, f.taskTainted(t))
	require.Eventually(t, func() bool { return len(f.audit.phases()) == 2 }, 5*time.Second, 10*time.Millisecond)
	f.audit.mu.Lock()
	defer f.audit.mu.Unlock()
	finished := f.audit.records[1]
	require.Equal(t, OperatorAuditFinished, finished.Phase)
	require.Equal(t, "printenv", finished.Command)
	require.Equal(t, 2, finished.Argc)
	require.Equal(t, operatoraccess.ArgvDigest([]string{"printenv", "SECRET=hunter2"}), finished.ArgvDigest)
	require.NotNil(t, finished.ExitCode)
	require.Equal(t, 7, *finished.ExitCode)
	raw, _ := json.Marshal(f.audit.records)
	require.NotContains(t, string(raw), "hunter2", "audit records must not carry raw argv")
}

func TestOperatorExecClassReadOnlyRefusesExec(t *testing.T) {
	f := newOperatorFixture(t, true, "read")
	request := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/t1/exec?namespace=team", nil)
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "websocket")
	response, err := f.app.Test(request)
	require.NoError(t, err)
	defer response.Body.Close() //nolint:errcheck
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.False(t, f.workspaceTainted(t))
}

func TestPublicationCredentialRefusedForOperatorTaintedTask(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	for _, tainted := range []bool{false, true} {
		task := &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "settled", UID: "settled-task-uid", Annotations: map[string]string{}},
			Spec: corev1alpha1.TaskSpec{Workspace: &corev1alpha1.WorkspaceConfig{
				Intent: corev1alpha1.WorkspaceIntentWrite, PublicationCredentialRef: &corev1alpha1.WorkspaceCredentialReference{Name: "git-publish"},
			}},
			Status: corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{
				State: corev1alpha1.TaskExecutionStateSucceeded, Attempt: 1, PromptID: "prompt", PublicationCredentialResourceVersion: "11",
			}},
		}
		if tainted {
			task.Annotations[operatoraccess.TaintAnnotation] = "x"
		}
		publicationID := controller.ACPPublicationIDForTask(task)
		publication := &corev1alpha1.Publication{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "publication"},
			Spec:       corev1alpha1.PublicationSpec{ID: publicationID, TaskUID: string(task.UID)},
			Status:     corev1alpha1.PublicationStatus{State: corev1alpha1.PublicationControlState("Publishing")},
		}
		kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, publication).Build()
		server := &Server{client: kubeClient, config: ServerConfig{APIReader: kubeClient}}
		_, _, _, _, err := server.authorizePublicationCredential(context.Background(), publisherservice.CredentialMaterialRequest{
			ParentOperation: publisherservice.OperationPublicationPublish,
			Metadata: publisherservice.OperationMetadata{
				Namespace: "default", PublicationID: publicationID, OperationID: controller.ACPPublicationOperationID("publish", task),
			},
			Reference: publisherservice.CredentialReference{Name: "git-publish", Kind: publisherservice.CredentialHTTPExtraHeader},
		})
		if tainted {
			require.ErrorContains(t, err, "operator-tainted")
		} else {
			require.NoError(t, err)
		}
	}
}
