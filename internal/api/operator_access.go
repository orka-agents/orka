package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/operatoraccess"
)

const (
	operatorWorkspaceLinkLabel        = "acp.workspace.orka.ai/execution-workspace"
	operatorWorkspaceUIDAnnotation    = "acp.workspace.orka.ai/execution-workspace-uid"
	operatorWorkspacePoolAnnotation   = "acp.workspace.orka.ai/runtime-pool"
	operatorTaskAttachmentEpochAnnot  = "acp.workspace.orka.ai/attachment-epoch"
	operatorRuntimePoolPort           = 8080
	operatorSupervisorDialTimeout     = 10 * time.Second
	operatorSupervisorResponseTimeout = 30 * time.Second
	operatorExecOptionsReadTimeout    = 30 * time.Second
	operatorAccessTaskAction          = "operatorAccess"
	operatorAccessCapabilityHeader    = "X-Orka-Operator-Access-Capability"
)

// OperatorAccessConfig enables `orka task exec` / `orka task cp`. It is
// disabled unless the controller runs with --operator-workspace-access-enabled.
type OperatorAccessConfig struct {
	Enabled bool
	// HTTPClient reaches runtime supervisors. Nil uses a dedicated client
	// that never consults proxy environment variables.
	HTTPClient *http.Client
	// Audit receives every operator access record. Nil logs and emits a
	// Kubernetes Event on the Task.
	Audit OperatorAuditSink
}

// OperatorAuditRecord is one audit record. It never carries raw argv, file
// content, credentials, or tokens.
type OperatorAuditRecord struct {
	AuditID                  string                   `json:"auditID"`
	Phase                    string                   `json:"phase"`
	Operation                operatoraccess.Operation `json:"operation"`
	Actor                    string                   `json:"actor"`
	AuthType                 string                   `json:"authType"`
	Namespace                string                   `json:"namespace"`
	Task                     string                   `json:"task"`
	TaskUID                  string                   `json:"taskUID,omitempty"`
	RuntimeSessionUID        string                   `json:"runtimeSessionUID,omitempty"`
	RuntimeSessionGeneration int64                    `json:"runtimeSessionGeneration,omitempty"`
	Workspace                string                   `json:"workspace,omitempty"`
	Command                  string                   `json:"command,omitempty"`
	ArgvDigest               string                   `json:"argvDigest,omitempty"`
	Argc                     int                      `json:"argc,omitempty"`
	TTY                      bool                     `json:"tty,omitempty"`
	Path                     string                   `json:"path,omitempty"`
	Bytes                    int64                    `json:"bytes,omitempty"`
	ExitCode                 *int                     `json:"exitCode,omitempty"`
	Outcome                  string                   `json:"outcome,omitempty"`
	Tainted                  bool                     `json:"tainted"`
	StartedAt                time.Time                `json:"startedAt"`
	FinishedAt               *time.Time               `json:"finishedAt,omitempty"`
}

// Audit phases.
const (
	OperatorAuditStarted  = "started"
	OperatorAuditFinished = "finished"
	OperatorAuditDenied   = "denied"
)

// operatorOutcomeFailed is the audit outcome for an unclassified failure.
const operatorOutcomeFailed = "failed"

// OperatorAuditSink receives operator access audit records.
type OperatorAuditSink interface {
	Record(ctx context.Context, record OperatorAuditRecord)
}

type operatorTarget struct {
	task      *corev1alpha1.Task
	workspace *workspacev1alpha1.ExecutionWorkspace
	class     *workspacev1alpha1.ExecutionWorkspaceClass
	pool      *corev1alpha1.RuntimePool
	endpoint  string
	bearer    string
	secret    []byte
	fence     harnessv2.Fence
	sessionID harnessv2.RuntimeSessionID
	record    OperatorAuditRecord
}

func (s *Server) operatorHTTPClient() *http.Client {
	if s.config.OperatorAccess.HTTPClient != nil {
		return s.config.OperatorAccess.HTTPClient
	}
	return &http.Client{Transport: &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: operatorSupervisorDialTimeout}).DialContext,
		ResponseHeaderTimeout: operatorSupervisorResponseTimeout,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	}}
}

func (s *Server) operatorAudit(ctx context.Context, record OperatorAuditRecord) {
	sink := s.config.OperatorAccess.Audit
	if sink == nil {
		sink = defaultOperatorAuditSink{server: s}
	}
	sink.Record(ctx, record)
}

type defaultOperatorAuditSink struct{ server *Server }

func (d defaultOperatorAuditSink) Record(ctx context.Context, record OperatorAuditRecord) {
	exitCode := ""
	if record.ExitCode != nil {
		exitCode = strconv.Itoa(*record.ExitCode)
	}
	log.Info("audit: operator workspace access",
		"auditID", record.AuditID, "phase", record.Phase, "operation", record.Operation,
		"actor", record.Actor, "authType", record.AuthType,
		"namespace", record.Namespace, "task", record.Task, "taskUID", record.TaskUID,
		"runtimeSessionUID", record.RuntimeSessionUID, "runtimeSessionGeneration", record.RuntimeSessionGeneration,
		"workspace", record.Workspace, "command", record.Command, "argvDigest", record.ArgvDigest, "argc", record.Argc,
		"tty", record.TTY, "path", record.Path, "bytes", record.Bytes, "exitCode", exitCode,
		"outcome", record.Outcome, "tainted", record.Tainted, "startedAt", record.StartedAt, "finishedAt", record.FinishedAt)
	clientset := d.server.config.Clientset
	if record.TaskUID == "" || kubernetesClientsetIsNil(clientset) {
		return
	}
	reason := "OperatorAccess" + strings.ToUpper(record.Phase[:1]) + record.Phase[1:]
	eventType := corev1.EventTypeNormal
	if record.Phase == OperatorAuditDenied {
		eventType = corev1.EventTypeWarning
	}
	message := fmt.Sprintf("operator %s %s by %s (audit %s)", record.Operation, record.Phase, record.Actor, record.AuditID)
	if record.Outcome != "" {
		message += ": " + record.Outcome
	}
	if record.ExitCode != nil {
		message += ", exit " + exitCode
	}
	now := metav1.NewTime(time.Now())
	eventCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := clientset.CoreV1().Events(record.Namespace).Create(eventCtx, &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{GenerateName: record.Task + ".operator-", Namespace: record.Namespace},
		InvolvedObject: corev1.ObjectReference{
			APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task",
			Namespace: record.Namespace, Name: record.Task, UID: types.UID(record.TaskUID),
		},
		Reason: reason, Message: message, Type: eventType,
		Source:         corev1.EventSource{Component: "orka-api"},
		FirstTimestamp: now, LastTimestamp: now, Count: 1,
	}, metav1.CreateOptions{})
	if err != nil {
		log.Error(err, "audit: failed to record operator access Event", "auditID", record.AuditID)
	}
}

func newOperatorAuditID() string {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "op-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return "op-" + hex.EncodeToString(raw)
}

// operatorAuditEnvelopeKey stores the request's audit envelope in fiber
// Locals so the route middleware and the handler share one audit ID.
const operatorAuditEnvelopeKey = "orka.operatorAuditEnvelope"

// operatorAuditEnvelope carries the safe actor/target identity of one
// operator request from before route authorization until a handler takes
// over its started/finished lifecycle. Any refusal before that point is
// recorded exactly once as denied.
type operatorAuditEnvelope struct {
	record   OperatorAuditRecord
	recorded bool
}

// operatorRequestAudit returns the request's audit envelope, creating it from
// the authenticated identity and the requested namespace and Task name. The
// Task UID stays empty until the Task has actually been read.
func operatorRequestAudit(c fiber.Ctx, operation operatoraccess.Operation) *operatorAuditEnvelope {
	if envelope, ok := c.Locals(operatorAuditEnvelopeKey).(*operatorAuditEnvelope); ok && envelope != nil {
		return envelope
	}
	actor, authType := operatorActor(GetUserInfo(c))
	envelope := &operatorAuditEnvelope{record: OperatorAuditRecord{
		AuditID: newOperatorAuditID(), Operation: operation, Actor: actor, AuthType: authType,
		Namespace: strings.TrimSpace(c.Query(toolNamespaceArg)), Task: c.Params("id"), StartedAt: time.Now().UTC(),
	}}
	c.Locals(operatorAuditEnvelopeKey, envelope)
	return envelope
}

// auditOperatorRefusal records err as a denied attempt unless a record for
// this request was already written.
func (s *Server) auditOperatorRefusal(c fiber.Ctx, envelope *operatorAuditEnvelope, err error) {
	if err == nil || envelope == nil || envelope.recorded {
		return
	}
	envelope.recorded = true
	record := envelope.record
	record.Phase = OperatorAuditDenied
	record.Outcome = operatorFailureOutcome(err)
	s.operatorAudit(c.Context(), record)
}

// operatorRouteAudit runs before route authorization, so dedicated
// permission denials are audited like every later refusal.
func (s *Server) operatorRouteAudit(operation operatoraccess.Operation) fiber.Handler {
	return func(c fiber.Ctx) error {
		envelope := operatorRequestAudit(c, operation)
		err := c.Next()
		s.auditOperatorRefusal(c, envelope, err)
		return err
	}
}

func operatorScopeFor(operation operatoraccess.Operation) string {
	switch operation {
	case operatoraccess.OperationExec:
		return operatoraccess.ScopeExec
	case operatoraccess.OperationCopyIn:
		return operatoraccess.ScopeFilesWrite
	default:
		return operatoraccess.ScopeFilesRead
	}
}

// authorizeOperatorCaller enforces the caller-type rules on top of the route
// policy. TokenReview identities passed the dedicated SubjectAccessReview in
// the route policy. Context-token callers need the explicit operator scope and
// a namespace-bound transaction context, regardless of the context-token
// authorization mode. Every other identity type (OIDC, unauthenticated) is
// refused because it has no Kubernetes identity to review.
func authorizeOperatorCaller(ui *UserInfo, operation operatoraccess.Operation, namespace string) error {
	if ui == nil {
		return fiber.NewError(fiber.StatusUnauthorized, "authentication is required")
	}
	switch ui.AuthType {
	case AuthTypeTokenReview:
		return nil
	case AuthTypeContextToken:
		if ui.ContextToken == nil || !slices.Contains(ui.ContextToken.Scopes, operatorScopeFor(operation)) {
			return fiber.NewError(fiber.StatusForbidden,
				fmt.Sprintf("context token lacks the explicit %s scope required for operator %s", operatorScopeFor(operation), operation))
		}
		if strings.TrimSpace(ui.Namespace) == "" || ui.Namespace != namespace {
			return fiber.NewError(fiber.StatusForbidden, "context token is not bound to the Task namespace")
		}
		return nil
	default:
		return fiber.NewError(fiber.StatusForbidden,
			"operator workspace access requires a Kubernetes identity authorized by SubjectAccessReview")
	}
}

func operatorActor(ui *UserInfo) (string, string) {
	if ui == nil {
		return "", ""
	}
	actor := ui.Username
	if actor == "" {
		actor = ui.Subject
	}
	return actor, ui.AuthType
}

func operatorConflict(message string) error {
	return fiber.NewError(fiber.StatusConflict, message)
}

// resolveOperatorTarget authorizes the caller and resolves the exact live
// RuntimeSession that belongs to the requested Task, failing closed at every
// identity link: Task -> linked ExecutionWorkspace incarnation -> class
// opt-in -> RuntimePool -> active Pod -> supervisor capability.
//
//nolint:gocyclo // Every link of the identity chain is verified in one auditable place.
func (s *Server) resolveOperatorTarget(c fiber.Ctx, operation operatoraccess.Operation) (*operatorTarget, error) {
	// Every refusal below is audited as denied by the caller with whatever
	// identity was established; the Task UID is recorded only once read.
	envelope := operatorRequestAudit(c, operation)
	target := &operatorTarget{record: envelope.record}
	defer func() { envelope.record = target.record }()
	deny := func(err error) (*operatorTarget, error) { return nil, err }
	if !s.config.OperatorAccess.Enabled {
		return deny(fiber.NewError(fiber.StatusForbidden,
			"operator workspace access is disabled on this Orka installation (--operator-workspace-access-enabled)"))
	}
	namespace, err := s.handlers.resolveNamespace(c, c.Query(toolNamespaceArg, ""))
	if err != nil {
		return deny(err)
	}
	target.record.Namespace = namespace
	ui := GetUserInfo(c)
	if err := authorizeOperatorCaller(ui, operation, namespace); err != nil {
		return deny(err)
	}
	loaded, err := s.handlers.taskAccess().loadReadable(c, operatorAccessTaskAction, namespace, c.Params("id"))
	if err != nil {
		return deny(err)
	}
	ctx := c.Context()
	reader := s.authorizationReader()
	task := &corev1alpha1.Task{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(loaded), task); err != nil || task.UID != loaded.UID {
		return deny(fiber.NewError(fiber.StatusNotFound, "task not found"))
	}
	target.task = task
	target.record.Namespace, target.record.Task, target.record.TaskUID = task.Namespace, task.Name, string(task.UID)
	if !task.DeletionTimestamp.IsZero() {
		return deny(operatorConflict("task is being deleted"))
	}
	if task.Spec.Execution == nil || task.Spec.Execution.Workspace == nil || task.Spec.Execution.Workspace.ClassRef == nil {
		return deny(operatorConflict("operator access requires a Task that runs in a class-backed execution workspace"))
	}
	execution := task.Status.Execution
	if execution == nil || strings.TrimSpace(execution.RuntimePoolName) == "" || execution.RuntimePoolUID == "" ||
		execution.RuntimeSessionUID == "" || execution.RuntimeSessionGeneration <= 0 || execution.RuntimeInstanceID == "" {
		return deny(operatorConflict("task has no live runtime session"))
	}
	if execution.State == corev1alpha1.TaskExecutionStateSettling {
		return deny(operatorConflict("task workspace is being validated for publication; operator access is refused"))
	}
	target.record.RuntimeSessionUID = execution.RuntimeSessionUID
	target.record.RuntimeSessionGeneration = execution.RuntimeSessionGeneration

	workspaceName := strings.TrimSpace(task.Labels[operatorWorkspaceLinkLabel])
	workspaceUID := strings.TrimSpace(task.Annotations[operatorWorkspaceUIDAnnotation])
	if workspaceName == "" || workspaceUID == "" {
		return deny(operatorConflict("task is not linked to an execution workspace"))
	}
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: workspaceName}, workspace); err != nil {
		if apierrors.IsNotFound(err) {
			return deny(fiber.NewError(fiber.StatusGone, "task execution workspace no longer exists"))
		}
		return nil, fiber.NewError(fiber.StatusServiceUnavailable, "execution workspace is unavailable")
	}
	target.workspace = workspace
	target.record.Workspace = workspace.Name
	if string(workspace.UID) != workspaceUID {
		return deny(fiber.NewError(fiber.StatusGone, "task execution workspace was replaced"))
	}
	if !workspace.DeletionTimestamp.IsZero() || workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady {
		return deny(operatorConflict("execution workspace is not running"))
	}
	if !operatorWorkspaceOwnedByTask(workspace, task) {
		return deny(operatorConflict("execution workspace is attached to another Task"))
	}
	if strings.TrimSpace(workspace.Annotations[operatorWorkspacePoolAnnotation]) != execution.RuntimePoolName {
		return deny(operatorConflict("execution workspace is not linked to the Task runtime pool"))
	}

	class := &workspacev1alpha1.ExecutionWorkspaceClass{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: workspace.Spec.ClassBinding.Name}, class); err != nil {
		if apierrors.IsNotFound(err) {
			return deny(fiber.NewError(fiber.StatusForbidden, "execution workspace class no longer exists"))
		}
		return nil, fiber.NewError(fiber.StatusServiceUnavailable, "execution workspace class is unavailable")
	}
	if class.UID != workspace.Spec.ClassBinding.UID {
		return deny(fiber.NewError(fiber.StatusForbidden, "execution workspace class was replaced"))
	}
	target.class = class
	if !operatoraccess.ParseClassAccess(class.Annotations).Allowed(operation) {
		return deny(fiber.NewError(fiber.StatusForbidden, fmt.Sprintf(
			"ExecutionWorkspaceClass %s does not allow operator %s; an administrator must set the %s annotation",
			class.Name, operation, operatoraccess.ClassAccessAnnotation)))
	}

	pool := &corev1alpha1.RuntimePool{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: execution.RuntimePoolName}, pool); err != nil {
		if apierrors.IsNotFound(err) {
			return deny(fiber.NewError(fiber.StatusGone, "task runtime pool no longer exists"))
		}
		return nil, fiber.NewError(fiber.StatusServiceUnavailable, "runtime pool is unavailable")
	}
	target.pool = pool
	if string(pool.UID) != execution.RuntimePoolUID || pool.Labels[operatorWorkspaceLinkLabel] != workspace.Name ||
		pool.Spec.ExecutionWorkspace == nil ||
		(pool.Spec.ExecutionWorkspace.WorkspaceRef != nil && pool.Spec.ExecutionWorkspace.WorkspaceRef.UID != workspace.UID) {
		return deny(fiber.NewError(fiber.StatusGone, "task runtime pool was replaced"))
	}
	if workload := pool.Spec.ExecutionWorkspace.Workload; workload != nil &&
		slices.Contains(workload.RequiredFeatures, workspacev1alpha1.WorkspaceFeatureNativeProcess) {
		return deny(fiber.NewError(fiber.StatusNotImplemented,
			"operator access is not supported for native-process (Substrate) execution workspaces"))
	}
	active := pool.Status.ActiveInstance
	if active == nil || active.RuntimeInstanceID != execution.RuntimeInstanceID || active.PodAddress == "" {
		return deny(fiber.NewError(fiber.StatusGone, "task runtime instance is no longer active"))
	}
	if epochs := s.config.ControllerEpochs; epochs != nil {
		current, err := epochs.CurrentFence(ctx)
		if err != nil {
			return nil, fiber.NewError(fiber.StatusServiceUnavailable, "controller epoch is unavailable")
		}
		if current.Epoch != active.ControllerEpoch {
			return deny(fiber.NewError(fiber.StatusGone, "task runtime instance is bound to a stale controller epoch"))
		}
	}
	pod := &corev1.Pod{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: active.PodNamespace, Name: active.PodName}, pod); err != nil {
		return deny(fiber.NewError(fiber.StatusGone, "task runtime Pod is unavailable"))
	}
	if string(pod.UID) != active.PodUID || pod.Status.PodIP != active.PodAddress || !pod.DeletionTimestamp.IsZero() {
		return deny(fiber.NewError(fiber.StatusGone, "task runtime Pod was replaced"))
	}
	secret, err := resolveArtifactRuntimePoolAuthSecret(ctx, reader, pool)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusServiceUnavailable, "runtime pool credentials are unavailable")
	}
	target.bearer = strings.TrimSpace(string(secret.Data[runtimePoolControllerTokenKeyAPI]))
	target.secret = append([]byte(nil), secret.Data[runtimePoolCapabilitySecretKeyAPI]...)
	if target.bearer == "" || len(target.secret) < harnessv2.MinCapabilitySecretBytes {
		return nil, fiber.NewError(fiber.StatusServiceUnavailable, "runtime pool credentials are incomplete")
	}
	schema, err := strconv.ParseUint(active.ProfileDigestSchemaVersion, 10, 32)
	if err != nil {
		return deny(fiber.NewError(fiber.StatusGone, "task runtime instance profile is invalid"))
	}
	target.fence = harnessv2.Fence{
		RuntimeInstanceID:          harnessv2.RuntimeInstanceID(active.RuntimeInstanceID),
		SupervisorBootID:           harnessv2.SupervisorBootID(active.BootID),
		ControllerEpoch:            uint64(active.ControllerEpoch),
		RuntimePoolUID:             harnessv2.RuntimePoolUID(pool.UID),
		RuntimePoolGeneration:      uint64(pool.Generation),
		RuntimeSessionUID:          harnessv2.RuntimeSessionUID(execution.RuntimeSessionUID),
		RuntimeSessionGeneration:   uint64(execution.RuntimeSessionGeneration),
		RuntimeProfileDigest:       harnessv2.ProfileDigest(active.ProfileDigest),
		ProfileDigestSchemaVersion: uint32(schema),
	}
	target.sessionID = harnessv2.RuntimeSessionID(
		"runtime-" + execution.RuntimeSessionUID + "-g" + strconv.FormatInt(execution.RuntimeSessionGeneration, 10))
	target.endpoint = "http://" + net.JoinHostPort(active.PodAddress, strconv.Itoa(operatorRuntimePoolPort))
	if err := s.verifyOperatorSupervisor(ctx, target); err != nil {
		return deny(err)
	}
	return target, nil
}

// operatorWorkspaceOwnedByTask accepts the Task that holds the attachment, or
// the Task that held the most recent attachment of a detached (retained)
// workspace. A workspace attached to, or since attached by, another Task is
// never reachable through an older Task.
func operatorWorkspaceOwnedByTask(workspace *workspacev1alpha1.ExecutionWorkspace, task *corev1alpha1.Task) bool {
	if attachment := workspace.Spec.Attachment; attachment != nil {
		return attachment.TaskRef.UID == task.UID
	}
	epoch, err := strconv.ParseInt(strings.TrimSpace(task.Annotations[operatorTaskAttachmentEpochAnnot]), 10, 64)
	return err == nil && epoch > 0 && epoch == workspace.Spec.AttachmentEpoch
}

// operatorWorkspaceStillTargeted reports whether workspace is still the exact
// incarnation, attachment, and attachment epoch the request was authorized
// against. A handoff to a continuation advances the attachment epoch, so a
// request authorized for the old Task can never act on its successor's
// workspace even when the successor reuses the same RuntimeSession.
func operatorWorkspaceStillTargeted(workspace *workspacev1alpha1.ExecutionWorkspace, target *operatorTarget) error {
	switch {
	case workspace.UID != target.workspace.UID || !workspace.DeletionTimestamp.IsZero():
		return fiber.NewError(fiber.StatusGone, "task execution workspace was replaced")
	case workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady:
		return operatorConflict("execution workspace is not running")
	case workspace.Spec.AttachmentEpoch != target.workspace.Spec.AttachmentEpoch || !operatorWorkspaceOwnedByTask(workspace, target.task):
		return operatorConflict("execution workspace attachment changed; operator access is refused")
	}
	return nil
}

// revalidateOperatorTarget re-reads the Task, its workspace attachment, and
// the class opt-in immediately before anything is forwarded to the runtime.
// The target was resolved before the exec request arrived, which a client
// may delay; the class opt-in may have been withdrawn or the workspace handed
// to a continuation in the meantime.
func (s *Server) revalidateOperatorTarget(ctx context.Context, target *operatorTarget, operation operatoraccess.Operation) error {
	reader := s.authorizationReader()
	task := &corev1alpha1.Task{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(target.task), task); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusGone, "task no longer exists")
		}
		return fiber.NewError(fiber.StatusServiceUnavailable, "task is unavailable")
	}
	execution, resolved := task.Status.Execution, target.task.Status.Execution
	switch {
	case task.UID != target.task.UID:
		return fiber.NewError(fiber.StatusGone, "task was replaced")
	case !task.DeletionTimestamp.IsZero():
		return operatorConflict("task is being deleted")
	case execution == nil || resolved == nil || execution.RuntimeSessionUID != resolved.RuntimeSessionUID ||
		execution.RuntimeSessionGeneration != resolved.RuntimeSessionGeneration || execution.RuntimeInstanceID != resolved.RuntimeInstanceID:
		return fiber.NewError(fiber.StatusGone, "task runtime session changed")
	case execution.State == corev1alpha1.TaskExecutionStateSettling:
		return operatorConflict("task workspace is being validated for publication; operator access is refused")
	}
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(target.workspace), workspace); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusGone, "task execution workspace no longer exists")
		}
		return fiber.NewError(fiber.StatusServiceUnavailable, "execution workspace is unavailable")
	}
	if err := operatorWorkspaceStillTargeted(workspace, target); err != nil {
		return err
	}
	class := &workspacev1alpha1.ExecutionWorkspaceClass{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(target.class), class); err != nil {
		if apierrors.IsNotFound(err) {
			return fiber.NewError(fiber.StatusForbidden, "execution workspace class no longer exists")
		}
		return fiber.NewError(fiber.StatusServiceUnavailable, "execution workspace class is unavailable")
	}
	if class.UID != target.class.UID {
		return fiber.NewError(fiber.StatusForbidden, "execution workspace class was replaced")
	}
	if !operatoraccess.ParseClassAccess(class.Annotations).Allowed(operation) {
		return fiber.NewError(fiber.StatusForbidden, fmt.Sprintf(
			"ExecutionWorkspaceClass %s no longer allows operator %s", class.Name, operation))
	}
	return nil
}

func (s *Server) verifyOperatorSupervisor(ctx context.Context, target *operatorTarget) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.endpoint+harnessv2.CapabilitiesPath, nil)
	if err != nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "runtime supervisor is unavailable")
	}
	request.Header.Set(operatorAccessCapabilityHeader, "1")
	request.Header.Set(harnessv2.NativeSessionLimitsHeader, "1")
	response, err := s.operatorHTTPClient().Do(request)
	if err != nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "runtime supervisor is unreachable")
	}
	defer response.Body.Close() //nolint:errcheck
	var capabilities harnessv2.CapabilitiesResponse
	if response.StatusCode != http.StatusOK ||
		json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&capabilities) != nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "runtime supervisor capabilities are unavailable")
	}
	if string(capabilities.RuntimeProfileDigest) != string(target.fence.RuntimeProfileDigest) {
		return fiber.NewError(fiber.StatusGone, "runtime supervisor profile does not match the Task runtime")
	}
	if !capabilities.SupportsOperatorAccess {
		return fiber.NewError(fiber.StatusNotImplemented, "runtime supervisor does not support operator access; upgrade the runtime image")
	}
	return nil
}

func (target *operatorTarget) metadata(operation operatoraccess.Operation) harnessv2.MutationMetadata {
	now := time.Now().UTC()
	return harnessv2.MutationMetadata{
		Fence:       target.fence,
		OperationID: harnessv2.OperationID("operator-" + string(operation) + "-" + target.record.AuditID),
		ExpiresAt:   now.Add(harnessv2.OperatorCapabilityTTL),
	}
}

// taintOperatorTarget durably marks the exact workspace incarnation and the
// Task before any write-capable operation starts. Failure refuses the
// operation. The workspace patch is UID- and resourceVersion-preconditioned.
func (s *Server) taintOperatorTarget(ctx context.Context, target *operatorTarget) error {
	value := operatoraccess.EncodeTaint(operatoraccess.Taint{
		At: time.Now().UTC(), Operation: target.record.Operation, AuditID: target.record.AuditID, TaskUID: string(target.task.UID),
	})
	for attempt := range 3 {
		workspace := &workspacev1alpha1.ExecutionWorkspace{}
		if err := s.authorizationReader().Get(ctx, client.ObjectKeyFromObject(target.workspace), workspace); err != nil {
			return fmt.Errorf("read execution workspace: %w", err)
		}
		// The ownership check and the taint write are one optimistic-lock
		// step: an attachment handoff either lands first and refuses the
		// taint, or the taint lands first and the attachment manager, which
		// revalidates every attempt, refuses to hand a tainted workspace on.
		if err := operatorWorkspaceStillTargeted(workspace, target); err != nil {
			return err
		}
		if operatoraccess.Tainted(workspace.Annotations) {
			break
		}
		base := workspace.DeepCopy()
		if workspace.Annotations == nil {
			workspace.Annotations = map[string]string{}
		}
		workspace.Annotations[operatoraccess.TaintAnnotation] = value
		err := s.client.Patch(ctx, workspace, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		if err == nil {
			break
		}
		if !apierrors.IsConflict(err) || attempt == 2 {
			return fmt.Errorf("taint execution workspace: %w", err)
		}
	}
	for attempt := range 3 {
		task := &corev1alpha1.Task{}
		if err := s.authorizationReader().Get(ctx, client.ObjectKeyFromObject(target.task), task); err != nil {
			return fmt.Errorf("read task: %w", err)
		}
		if task.UID != target.task.UID {
			return fmt.Errorf("task was replaced")
		}
		if operatoraccess.Tainted(task.Annotations) {
			break
		}
		base := task.DeepCopy()
		if task.Annotations == nil {
			task.Annotations = map[string]string{}
		}
		task.Annotations[operatoraccess.TaintAnnotation] = value
		err := s.client.Patch(ctx, task, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		if err == nil {
			break
		}
		if !apierrors.IsConflict(err) || attempt == 2 {
			return fmt.Errorf("taint task: %w", err)
		}
	}
	target.record.Tainted = true
	return nil
}

func supervisorErrorMessage(response *http.Response) (int, string) {
	if response == nil {
		return http.StatusBadGateway, "runtime supervisor is unreachable"
	}
	var payload harnessv2.ErrorResponse
	_ = json.NewDecoder(io.LimitReader(response.Body, 16<<10)).Decode(&payload)
	message := strings.TrimSpace(payload.Message)
	if message == "" {
		message = "runtime supervisor refused the operation"
	}
	status := response.StatusCode
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	return status, "runtime supervisor: " + message
}

func operatorFinishRecord(record OperatorAuditRecord, outcome string, exitCode *int, bytes int64) OperatorAuditRecord {
	finished := time.Now().UTC()
	record.Phase = OperatorAuditFinished
	record.Outcome = outcome
	record.ExitCode = exitCode
	record.Bytes = bytes
	record.FinishedAt = &finished
	return record
}
