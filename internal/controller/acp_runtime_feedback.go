package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/runtimefeedback"
	"github.com/orka-agents/orka/internal/tools"
)

const (
	RuntimeFeedbackToolName      = "runtime_feedback"
	runtimeFeedbackContainerName = "runtime"
	runtimeFeedbackTaskLabel     = "orka.ai/runtime-feedback-task-uid"
	runtimeFeedbackExplanation   = "Container-scoped network evidence for this Task attempt. Events may include supervisor or other descendant traffic; they do not prove which tool caused a failure. Missing events do not prove success or permission. This report grants no permission, policy change, or retry. Choose only an already-approved alternative."
)

var runtimeFeedbackContainerID = regexp.MustCompile(`^(containerd|cri-o)://[a-f0-9]{64}$`)

type acpMCPExecutionContextKey struct{}

type runtimeFeedbackExecution struct {
	TaskUID     string                   `json:"taskUID"`
	TaskAttempt uint32                   `json:"taskAttempt"`
	PromptID    harnessv2.PromptID       `json:"promptID"`
	Fence       harnessv2.Fence          `json:"fence"`
	Workload    runtimefeedback.Workload `json:"workload"`
}

func (e runtimeFeedbackExecution) query() (runtimefeedback.Query, error) {
	digest, err := acpDomainDigest("runtime-feedback-execution-v1", e)
	if err != nil {
		return runtimefeedback.Query{}, err
	}
	return runtimefeedback.Query{RunID: strings.TrimPrefix(digest, "sha256:"), Workload: e.Workload}, nil
}

// runtimePoolIdentityDigest keeps Task-specific feedback isolation in the
// existing immutable pool identity. Ordinary pools retain their original key.
func runtimePoolIdentityDigest(profileDigest, image, feedbackTaskUID string) (string, error) {
	identity := map[string]string{
		acpRuntimePoolIdentityProfileDigestKey: profileDigest,
		acpRuntimePoolIdentityRuntimeImageKey:  image,
	}
	if feedbackTaskUID != "" {
		identity["runtimeFeedbackTaskUID"] = feedbackTaskUID
	}
	return acpDomainDigest("runtime-pool-identity", identity)
}

func runtimeFeedbackProviderSupported(provider string) bool {
	return provider == string(corev1alpha1.AgentRuntimeCodex) || provider == string(corev1alpha1.AgentRuntimeOpencode)
}

func runtimeFeedbackPoolMatches(pool *corev1alpha1.RuntimePool, taskUID string) bool {
	if pool == nil || taskUID == "" || pool.Labels[runtimeFeedbackTaskLabel] != taskUID ||
		pool.Spec.ExecutionWorkspace != nil || pool.Spec.Capacity == nil ||
		pool.Spec.Capacity.MaxResidentSessions != 1 || pool.Spec.Capacity.MaxRunningPrompts != 1 ||
		!runtimeFeedbackProviderSupported(pool.Spec.Runtime.Profile.ProviderKind) {
		return false
	}
	digest, err := runtimePoolIdentityDigest(pool.Spec.Runtime.Profile.Digest, pool.Spec.Runtime.Image, taskUID)
	return err == nil && pool.Name == acpRuntimePoolName(pool.Spec.Runtime.Profile.ProviderKind, harnessv2.ProfileDigest(digest))
}

// resolveRuntimeFeedbackExecution performs Kubernetes reads only, allowing the
// broker to call it under its existing prompt/epoch data-access interlock.
func resolveRuntimeFeedbackExecution(ctx context.Context, reader client.Reader, task *corev1alpha1.Task, fence harnessv2.Fence) (runtimeFeedbackExecution, *corev1alpha1.RuntimePool, error) {
	if reader == nil || task == nil || task.Status.Execution == nil || task.Spec.SessionRef != nil ||
		(task.Spec.Execution != nil && task.Spec.Execution.Workspace != nil) {
		return runtimeFeedbackExecution{}, nil, errors.New("runtime feedback requires a fresh dedicated native execution")
	}
	execution := task.Status.Execution
	if execution.Attempt < 1 || execution.PromptID == "" || execution.RuntimeSessionUID != string(fence.RuntimeSessionUID) ||
		execution.RuntimeSessionGeneration != int64(fence.RuntimeSessionGeneration) || execution.RuntimeInstanceID != string(fence.RuntimeInstanceID) ||
		execution.ControllerEpoch != int64(fence.ControllerEpoch) {
		return runtimeFeedbackExecution{}, nil, errors.New("runtime feedback execution fence is stale")
	}
	pool := &corev1alpha1.RuntimePool{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: execution.RuntimePoolName}, pool); err != nil {
		return runtimeFeedbackExecution{}, nil, errors.New("runtime feedback pool is unavailable")
	}
	if !runtimeFeedbackPoolMatches(pool, string(task.UID)) || !pool.DeletionTimestamp.IsZero() ||
		string(pool.UID) != execution.RuntimePoolUID || string(pool.UID) != string(fence.RuntimePoolUID) || uint64(pool.Generation) != fence.RuntimePoolGeneration {
		return runtimeFeedbackExecution{}, nil, errors.New("runtime feedback pool is not dedicated to this execution")
	}
	active := pool.Status.ActiveInstance
	if active == nil || active.PodUID == "" || active.PodName == "" || active.PodNamespace == "" ||
		active.ProtocolVersion != corev1alpha1.RuntimePoolProtocolHarnessV2 || active.ProfileDigest != string(fence.RuntimeProfileDigest) ||
		active.ControllerEpoch != int64(fence.ControllerEpoch) || active.RuntimeInstanceID != string(fence.RuntimeInstanceID) || active.BootID != string(fence.SupervisorBootID) {
		return runtimeFeedbackExecution{}, nil, errors.New("runtime feedback active instance is stale")
	}
	workload, err := resolveRuntimeFeedbackWorkload(ctx, reader, active)
	if err != nil {
		return runtimeFeedbackExecution{}, nil, err
	}
	return runtimeFeedbackExecution{
		TaskUID: string(task.UID), TaskAttempt: uint32(execution.Attempt), PromptID: harnessv2.PromptID(execution.PromptID), Fence: fence, Workload: workload,
	}, pool, nil
}

func resolveRuntimeFeedbackWorkload(ctx context.Context, reader client.Reader, active *corev1alpha1.RuntimePoolActiveInstanceStatus) (runtimefeedback.Workload, error) {
	pod := &corev1.Pod{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: active.PodNamespace, Name: active.PodName}, pod); err != nil {
		return runtimefeedback.Workload{}, errors.New("runtime feedback worker Pod is unavailable")
	}
	if string(pod.UID) != active.PodUID || !pod.DeletionTimestamp.IsZero() || pod.Status.Phase != corev1.PodRunning || pod.Spec.NodeName == "" ||
		len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Name != runtimeFeedbackContainerName || len(pod.Spec.EphemeralContainers) != 0 || len(pod.Status.ContainerStatuses) != 1 ||
		active.RuntimeInstanceID != runtimePoolRuntimeInstanceID(pod.UID, harnessv2.SupervisorBootID(active.BootID)) {
		return runtimefeedback.Workload{}, errors.New("runtime feedback requires one exact running application container")
	}
	container := pod.Status.ContainerStatuses[0]
	if container.Name != runtimeFeedbackContainerName || container.State.Running == nil || !container.Ready || container.RestartCount < 0 || !runtimeFeedbackContainerID.MatchString(container.ContainerID) {
		return runtimefeedback.Workload{}, errors.New("runtime feedback worker container identity is unavailable")
	}
	return runtimefeedback.Workload{Namespace: pod.Namespace, PodName: pod.Name, PodUID: string(pod.UID), ContainerName: container.Name, ContainerID: container.ContainerID, RestartCount: container.RestartCount, Node: pod.Spec.NodeName}, nil
}

func feedbackRuntimeClient(ctx context.Context, reader client.Reader, pool *corev1alpha1.RuntimePool) (*harnessv2.Client, error) {
	bearer, capability, err := runtimePoolACPMCPAuthMaterial(ctx, reader, pool)
	if err != nil {
		return nil, errors.New("runtime feedback supervisor authority is unavailable")
	}
	return harnessv2.NewClient(exactPodEndpoint(pool.Status.ActiveInstance.PodAddress),
		harnessv2.WithControllerBearerToken(bearer), harnessv2.WithOperationCapabilitySecret(capability),
		harnessv2.WithStatusCapabilityBinding(harnessv2.StatusCapabilityBinding{
			RuntimeProfileDigest: harnessv2.ProfileDigest(pool.Spec.Runtime.Profile.Digest),
			RuntimeInstanceID:    harnessv2.RuntimeInstanceID(pool.Status.ActiveInstance.RuntimeInstanceID),
		}))
}

func verifyFeedbackRuntime(ctx context.Context, runtimeClient *harnessv2.Client, execution runtimeFeedbackExecution, running bool) (time.Time, error) {
	statusCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	status, err := runtimeClient.Status(statusCtx)
	if err != nil {
		return time.Time{}, errors.New("runtime feedback supervisor status is unavailable")
	}
	if harnessv2.CompareFence(execution.Fence, status.Fence, false) != harnessv2.FenceMatch || len(status.Sessions) != 1 {
		return time.Time{}, errors.New("runtime feedback supervisor is stale or shared")
	}
	session := status.Sessions[0]
	if session.RuntimeSessionUID != execution.Fence.RuntimeSessionUID || session.Generation != execution.Fence.RuntimeSessionGeneration {
		return time.Time{}, errors.New("runtime feedback session is stale")
	}
	if !running {
		if len(status.ActivePrompts) != 0 || !session.State.CanAdmitPrompt() {
			return time.Time{}, errors.New("runtime feedback session is not idle")
		}
		return time.Time{}, nil
	}
	if len(status.ActivePrompts) != 1 || session.State != harnessv2.RuntimeSessionStatePromptRunning || session.ActivePromptID != execution.PromptID {
		return time.Time{}, errors.New("runtime feedback prompt is not active")
	}
	prompt := status.ActivePrompts[0]
	if string(prompt.TaskUID) != execution.TaskUID || prompt.TaskAttempt != execution.TaskAttempt || prompt.PromptID != execution.PromptID ||
		prompt.RuntimeSessionUID != execution.Fence.RuntimeSessionUID || prompt.SessionGeneration != execution.Fence.RuntimeSessionGeneration {
		return time.Time{}, errors.New("runtime feedback active prompt belongs to another execution")
	}
	if prompt.StartedAt.IsZero() {
		return time.Time{}, errors.New("runtime feedback prompt start is unavailable")
	}
	return prompt.StartedAt, nil
}

// startRuntimeFeedback is called after the native RuntimeSession exists and
// before prompt submission. GKR unavailability is diagnostic unavailability;
// an invalid isolation/fence is a dispatch failure, never a shared capture.
func (d *ACPDispatcher) startRuntimeFeedback(ctx context.Context, task *corev1alpha1.Task, fence harnessv2.Fence, runtimeClient *harnessv2.Client, policy harnessv2.MCPPolicyConfiguration) (func(string), error) {
	if !policy.ToolPolicy.Allows(RuntimeFeedbackToolName) {
		return func(string) {}, nil
	}
	if d.RuntimeFeedback == nil {
		return nil, errors.New("runtime feedback is not configured")
	}
	reader := d.APIReader
	if reader == nil {
		reader = d.Client
	}
	execution, _, err := resolveRuntimeFeedbackExecution(ctx, reader, task, fence)
	if err != nil {
		return nil, err
	}
	if _, err := verifyFeedbackRuntime(ctx, runtimeClient, execution, false); err != nil {
		return nil, err
	}
	query, err := execution.query()
	if err != nil {
		return nil, err
	}
	if err := d.RuntimeFeedback.Register(ctx, query); err != nil {
		logf.FromContext(ctx).Info("runtime feedback capture is unavailable", "task", task.Name)
	}
	// Complete even after an uncertain registration response. Exact binding is
	// idempotent; a lost controller instead relies on GKR's bounded capture TTL.
	return func(reason string) {
		completeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if err := d.RuntimeFeedback.Complete(completeCtx, query, reason); err != nil {
			logf.FromContext(ctx).Info("runtime feedback capture completion is unavailable; bounded expiry remains active", "task", task.Name)
		}
	}, nil
}

// A runtime can cancel a prompt without cancelling the controller's context.
// Capture completion must retain that terminal outcome as well as local aborts.
func runtimeFeedbackCompletionReason(ctx context.Context, terminal *harnessv2.Event) string {
	if terminal == nil || terminal.Type == harnessv2.EventCancelled || ctx.Err() != nil {
		return runtimefeedback.Cancelled
	}
	return runtimefeedback.Completed
}

type runtimeFeedbackResult struct {
	Status      string                   `json:"status,omitempty"`
	Execution   runtimeFeedbackExecution `json:"execution"`
	Report      *runtimefeedback.Report  `json:"report,omitempty"`
	Explanation string                   `json:"explanation"`
}

type runtimeFeedbackTool struct {
	reader  client.Reader
	service runtimefeedback.Service
}

func RegisterRuntimeFeedbackTool(registry *tools.Registry, reader client.Reader, service runtimefeedback.Service) error {
	if registry == nil || reader == nil || service == nil {
		return errors.New("runtime feedback tool dependencies are incomplete")
	}
	registry.Register(&runtimeFeedbackTool{reader: reader, service: service})
	return nil
}

func (*runtimeFeedbackTool) Name() string { return RuntimeFeedbackToolName }
func (*runtimeFeedbackTool) Description() string {
	return "Read recent Gatekeeper Runtime network-denial evidence for your current isolated execution after a native network operation fails. Takes no arguments. Evidence is container-scoped, can be incomplete or unavailable, and never grants permissions, policy changes, or retries. Choose only already-approved alternatives."
}
func (*runtimeFeedbackTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
}

func (t *runtimeFeedbackTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var parameters struct{}
	decoder := json.NewDecoder(bytes.NewReader(args))
	decoder.DisallowUnknownFields()
	if len(bytes.TrimSpace(args)) == 0 || bytes.TrimSpace(args)[0] != '{' || decoder.Decode(&parameters) != nil {
		return "", errors.New("runtime_feedback accepts an empty object only")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return "", errors.New("runtime_feedback accepts one empty object only")
	}
	request, ok := ctx.Value(acpMCPExecutionContextKey{}).(harnessv2.MCPBrokerCallRequest)
	authenticated, authenticatedOK := ACPMCPAuthenticatedTaskFromContext(ctx)
	guard, guardOK := ACPMCPTaskDataGuardFromContext(ctx)
	if !ok || !authenticatedOK || !guardOK || authenticated.Namespace != request.Namespace || authenticated.UID != string(request.Metadata.TaskUID) {
		return "", errors.New("runtime feedback requires authenticated ACP v2 prompt authority")
	}
	resolve := func() (runtimeFeedbackExecution, *corev1alpha1.RuntimePool, error) {
		var execution runtimeFeedbackExecution
		var pool *corev1alpha1.RuntimePool
		err := guard(ctx, func(guardCtx context.Context) error {
			task := &corev1alpha1.Task{}
			if err := t.reader.Get(guardCtx, client.ObjectKey{Namespace: authenticated.Namespace, Name: authenticated.Name}, task); err != nil || string(task.UID) != authenticated.UID {
				return errors.New("runtime feedback Task is unavailable")
			}
			var err error
			execution, pool, err = resolveRuntimeFeedbackExecution(guardCtx, t.reader, task, request.Metadata.Fence)
			if err == nil && (execution.TaskAttempt != request.Metadata.TaskAttempt || execution.PromptID != request.Metadata.PromptID) {
				err = errors.New("runtime feedback Task attempt changed")
			}
			return err
		})
		return execution, pool, err
	}
	execution, pool, err := resolve()
	if err != nil {
		return "", err
	}
	runtimeClient, err := feedbackRuntimeClient(ctx, t.reader, pool)
	if err != nil {
		return "", err
	}
	startedAt, err := verifyFeedbackRuntime(ctx, runtimeClient, execution, true)
	if err != nil {
		return "", err
	}
	query, err := execution.query()
	if err != nil {
		return "", err
	}
	report, reportErr := t.service.Report(ctx, query)
	if reportErr == nil {
		reportErr = report.Validate(query, time.Now().UTC())
	}
	// Admission retries do not renew GKR's frozen capture. A run that ended
	// before this prompt started cannot diagnose it, including a registration
	// finalized by a previous proven-unsent dispatch. Historical partial
	// evidence remains useful when capture overlapped the admitted prompt.
	if reportErr == nil && report.Capture != nil &&
		(!report.Capture.ExpiresAt.After(startedAt) ||
			(report.Capture.EndedAt != nil && !report.Capture.EndedAt.After(startedAt))) {
		reportErr = errors.New("runtime feedback capture ended before prompt admission")
	}
	currentStartedAt, err := verifyFeedbackRuntime(ctx, runtimeClient, execution, true)
	if err != nil {
		return "", err
	}
	if !currentStartedAt.Equal(startedAt) {
		return "", errors.New("runtime feedback prompt changed during diagnosis")
	}
	current, _, err := resolve()
	if err != nil || current != execution {
		return "", errors.New("runtime feedback execution changed during diagnosis")
	}
	if reportErr != nil {
		return runtimeFeedbackJSON(runtimeFeedbackResult{Status: runtimefeedback.Unavailable, Execution: execution, Explanation: runtimeFeedbackExplanation})
	}
	return runtimeFeedbackJSON(runtimeFeedbackResult{Execution: execution, Report: &report, Explanation: runtimeFeedbackExplanation})
}

func runtimeFeedbackJSON(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode runtime feedback result: %w", err)
	}
	return string(data), nil
}
