package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/harness"
)

const (
	harnessWrapperEndpointEnv       = "ORKA_HARNESS_WRAPPER_ENDPOINT"
	harnessWrapperAuthValueEnv      = "ORKA_HARNESS_WRAPPER_BEARER_TOKEN"
	harnessWrapperAuthValueFileEnv  = "ORKA_HARNESS_WRAPPER_BEARER_TOKEN_FILE"
	harnessWrapperTurnIDAnnotation  = "orka.ai/harness-wrapper-turn-id"
	harnessWrapperRuntimeAnnotation = "orka.ai/harness-wrapper-runtime-session-id"
	harnessWrapperCorrelationIDAnno = "orka.ai/harness-wrapper-correlation-id"
	harnessWrapperLastFrameSeqAnno  = "orka.ai/harness-wrapper-last-frame-seq"
	harnessWrapperStartedAnno       = "orka.ai/harness-wrapper-started"
	harnessWrapperPlannedAtAnno     = "orka.ai/harness-wrapper-planned-at"
	harnessWrapperMetadataAnno      = "orka.ai/harness-wrapper-metadata"
	harnessWrapperStreamPollTimeout = 2 * time.Second
	harnessWrapperPlannedTurnTTL    = 5 * time.Minute
	harnessWrapperNoTimeoutDuration = time.Hour * 24 * 365 * 100
)

func taskHasHarnessWrapperTurn(task *corev1alpha1.Task) bool {
	if task == nil || task.Annotations == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(task.Annotations[harnessWrapperStartedAnno]), "true") &&
		taskHasPlannedHarnessWrapperTurn(task)
}

func taskHasPlannedHarnessWrapperTurn(task *corev1alpha1.Task) bool {
	if task == nil || task.Annotations == nil {
		return false
	}
	return strings.TrimSpace(task.Annotations[harnessWrapperTurnIDAnnotation]) != "" &&
		strings.TrimSpace(task.Annotations[harnessWrapperRuntimeAnnotation]) != "" &&
		strings.TrimSpace(task.Annotations[harnessWrapperCorrelationIDAnno]) != ""
}

func harnessWrapperEndpoint() string {
	return strings.TrimSpace(os.Getenv(harnessWrapperEndpointEnv))
}

func harnessWrapperAuthValue() string {
	if path := strings.TrimSpace(os.Getenv(harnessWrapperAuthValueFileEnv)); path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	return strings.TrimSpace(os.Getenv(harnessWrapperAuthValueEnv))
}

func (r *TaskReconciler) runHarnessWrapperTask(ctx context.Context, task *corev1alpha1.Task, agent *corev1alpha1.Agent) (ctrl.Result, error) {
	workspaceRequest, err := r.resolveExecutionWorkspaceRequest(ctx, task)
	if err != nil {
		if statusErr := r.markExecutionWorkspaceValidationFailed(ctx, task, err); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return r.failTask(ctx, task, fmt.Sprintf("failed to resolve execution workspace: %v", err))
	}
	if workspaceRequest != nil {
		return r.failTask(ctx, task, "execution workspace is not supported by harness runtime yet")
	}

	endpoint := harnessWrapperEndpoint()
	if endpoint == "" {
		return r.failTask(ctx, task, fmt.Sprintf("%s is required when agent harness runtime is enabled", harnessWrapperEndpointEnv))
	}
	if r.ExecutionEventStore == nil {
		return r.failTask(ctx, task, "execution event store is required for harness wrapper mode")
	}

	now := metav1.Now()
	attempts := task.Status.Attempts + 1
	var request harness.StartTurnRequest
	startedPlannedTurn := false
	if taskHasPlannedHarnessWrapperTurn(task) {
		if !taskHasHarnessWrapperTurn(task) {
			if harnessWrapperPlannedTurnExpired(task, now.Time) {
				return r.failTask(ctx, task, "planned harness runtime turn expired before start was confirmed")
			}
			var err error
			request, err = r.harnessWrapperStartTurnRequest(ctx, task, agent, now.Time, attempts)
			if err != nil {
				return r.failTask(ctx, task, err.Error())
			}
		} else {
			startedPlannedTurn = true
			request = r.plannedHarnessWrapperStartTurnRequest(task, agent, now.Time)
		}
		request.TurnID = harness.HarnessTurnID(strings.TrimSpace(task.Annotations[harnessWrapperTurnIDAnnotation]))
		request.RuntimeSessionID = harness.RuntimeSessionID(strings.TrimSpace(task.Annotations[harnessWrapperRuntimeAnnotation]))
		request.CorrelationID = strings.TrimSpace(task.Annotations[harnessWrapperCorrelationIDAnno])
	} else {
		var err error
		request, err = r.harnessWrapperStartTurnRequest(ctx, task, agent, now.Time, attempts)
		if err != nil {
			return r.failTask(ctx, task, err.Error())
		}
		if err := r.patchHarnessWrapperPlannedTurn(ctx, task, request); err != nil {
			return ctrl.Result{}, err
		}
	}
	if !startedPlannedTurn {
		latest := &corev1alpha1.Task{}
		if err := r.Get(ctx, ctrlclient.ObjectKey{Name: task.Name, Namespace: task.Namespace}, latest); err != nil {
			return ctrl.Result{}, err
		}
		if latest.Status.Phase != corev1alpha1.TaskPhasePending {
			task.Status = latest.Status
			return ctrl.Result{}, nil
		}
		client, err := harness.NewClient(endpoint, harness.WithBearerToken(harnessWrapperAuthValue()))
		if err != nil {
			return r.failTask(ctx, task, fmt.Sprintf("invalid harness wrapper endpoint: %v", err))
		}
		if err := r.validateHarnessWrapperCapabilities(ctx, client, request); err != nil {
			return r.failTask(ctx, task, err.Error())
		}
		if _, err := client.StartTurn(ctx, request); err != nil {
			message := err.Error()
			switch {
			case strings.Contains(message, "turn already exists"):
				// Treat a duplicate turn ID as idempotent recovery after the wrapper
				// accepted the planned turn before Running status was persisted.
			case strings.Contains(message, "maximum concurrent turns"):
				if clearErr := r.clearHarnessWrapperTurnState(ctx, task); clearErr != nil {
					return ctrl.Result{}, clearErr
				}
				if releaseErr := r.releaseHarnessWrapperSessionLock(ctx, task); releaseErr != nil {
					return ctrl.Result{}, releaseErr
				}
				return ctrl.Result{RequeueAfter: time.Second}, nil
			default:
				return r.failTask(ctx, task, events.RedactExecutionEventText(message))
			}
		}
		if err := r.patchHarnessWrapperStarted(ctx, task); err != nil {
			return ctrl.Result{}, err
		}
	}

	transitionedToRunning := false
	if err := r.updateStatusWithRetry(ctx, task, func(t *corev1alpha1.Task) {
		if t.Status.Phase != corev1alpha1.TaskPhasePending && t.Status.Phase != corev1alpha1.TaskPhaseRunning {
			return
		}
		t.Status.Phase = corev1alpha1.TaskPhaseRunning
		t.Status.StartTime = &now
		t.Status.Attempts = attempts
		t.Status.JobName = ""
		t.Status.Message = "harness wrapper turn running"
		transitionedToRunning = true
	}); err != nil {
		return ctrl.Result{}, err
	}
	if !transitionedToRunning {
		return ctrl.Result{}, nil
	}
	if err := r.recordTaskLifecycleEvent(
		ctx,
		task,
		events.ExecutionEventTypeTaskStarted,
		events.ExecutionEventSeverityInfo,
		"harness wrapper task started",
	); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 100 * time.Millisecond}, nil
}

//nolint:gocyclo // Handles stream polling, event mapping, and terminal task classification in one reconcile step.
func (r *TaskReconciler) finishHarnessWrapperTask(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	endpoint := harnessWrapperEndpoint()
	if endpoint == "" {
		return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, fmt.Sprintf("%s is required when agent harness runtime is enabled", harnessWrapperEndpointEnv))
	}
	if r.ExecutionEventStore == nil {
		return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, "execution event store is required for harness wrapper mode")
	}
	turnID := harness.HarnessTurnID(strings.TrimSpace(task.Annotations[harnessWrapperTurnIDAnnotation]))
	runtimeSessionID := harness.RuntimeSessionID(strings.TrimSpace(task.Annotations[harnessWrapperRuntimeAnnotation]))
	correlationID := strings.TrimSpace(task.Annotations[harnessWrapperCorrelationIDAnno])
	if turnID == "" || runtimeSessionID == "" || correlationID == "" {
		return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, "harness wrapper turn identity is missing")
	}
	client, err := harness.NewClient(endpoint, harness.WithBearerToken(harnessWrapperAuthValue()))
	if err != nil {
		return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, fmt.Sprintf("invalid harness wrapper endpoint: %v", err))
	}
	result := harness.TurnRunResult{}
	mapCtx := harness.EventMapContext{
		Namespace:   task.Namespace,
		TaskName:    task.Name,
		SessionName: harnessWrapperSessionName(task),
		AgentName:   harnessWrapperTaskAgentName(task),
	}
	afterSeq := harnessWrapperLastFrameSeq(task)
	lastFrameSeq := afterSeq
	streamCtx, cancel := context.WithTimeout(ctx, harnessWrapperStreamPollTimeout)
	defer cancel()
	err = client.StreamFrames(streamCtx, turnID, afterSeq, func(frame harness.HarnessEventFrame) error {
		if frame.RuntimeSessionID != runtimeSessionID || frame.TurnID != turnID || frame.CorrelationID != correlationID {
			return fmt.Errorf("harness frame identity does not match running turn")
		}
		mapped, err := harness.MapFrameToExecutionEvent(frame, mapCtx)
		if err != nil {
			return err
		}
		appended, err := r.ExecutionEventStore.AppendExecutionEvent(streamCtx, mapped)
		if err != nil {
			return fmt.Errorf("append mapped harness event: %w", err)
		}
		result.Frames = append(result.Frames, frame)
		result.Events = append(result.Events, *appended)
		if frame.Seq > lastFrameSeq {
			lastFrameSeq = frame.Seq
		}
		switch frame.Type {
		case harness.FrameTurnCompleted:
			result.Completed = frame.Completed
		case harness.FrameTurnFailed:
			result.Failed = frame.Failed
		case harness.FrameTurnCancelled:
			result.Cancelled = true
		}
		return nil
	})
	terminalFrameSeen := result.Completed != nil || result.Failed != nil || result.Cancelled
	if lastFrameSeq > afterSeq && !terminalFrameSeen {
		if patchErr := r.patchHarnessWrapperLastFrameSeq(ctx, task, lastFrameSeq); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
	}
	if err != nil && result.Completed == nil && result.Failed == nil && !result.Cancelled {
		if harnessWrapperStreamErrorIsTerminal(err) {
			return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, events.RedactExecutionEventText(err.Error()))
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if result.Completed != nil && r.ResultStore != nil {
		if saveErr := r.ResultStore.SaveResult(ctx, task.Namespace, task.Name, []byte(result.Completed.Result)); saveErr != nil {
			log.Error(saveErr, "failed to save harness wrapper result")
			return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, fmt.Sprintf("failed to save harness wrapper result: %v", saveErr))
		}
		task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
	}
	if result.Cancelled {
		return r.completeTask(ctx, task, corev1alpha1.TaskPhaseCancelled, "harness wrapper turn cancelled")
	}
	if result.Failed != nil {
		if result.Failed.Retryable && r.shouldRetry(task) {
			if clearErr := r.clearHarnessWrapperTurnState(ctx, task); clearErr != nil {
				return ctrl.Result{}, clearErr
			}
			return r.retryTask(ctx, task)
		}
		message := strings.TrimSpace(result.Failed.Message)
		if message == "" {
			message = strings.TrimSpace(result.Failed.Reason)
		}
		if message == "" && err != nil {
			message = err.Error()
		}
		if message == "" {
			message = "harness wrapper turn failed"
		}
		return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, events.RedactExecutionEventText(message))
	}
	if result.Completed == nil {
		return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, "harness wrapper turn ended without result")
	}
	return r.completeTask(ctx, task, corev1alpha1.TaskPhaseSucceeded, "harness wrapper task completed successfully")
}

func (r *TaskReconciler) patchHarnessWrapperPlannedTurn(
	ctx context.Context,
	task *corev1alpha1.Task,
	request harness.StartTurnRequest,
) error {
	patch := ctrlclient.MergeFrom(task.DeepCopy())
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	task.Annotations[harnessWrapperTurnIDAnnotation] = string(request.TurnID)
	task.Annotations[harnessWrapperRuntimeAnnotation] = string(request.RuntimeSessionID)
	task.Annotations[harnessWrapperCorrelationIDAnno] = request.CorrelationID
	task.Annotations[harnessWrapperLastFrameSeqAnno] = "0"
	task.Annotations[harnessWrapperStartedAnno] = "false"
	task.Annotations[harnessWrapperPlannedAtAnno] = time.Now().UTC().Format(time.RFC3339Nano)
	plannedMetadata := make(map[string]string, len(request.Metadata))
	for key, value := range request.Metadata {
		if key == "systemPrompt" {
			continue
		}
		plannedMetadata[key] = value
	}
	metadata, err := json.Marshal(plannedMetadata)
	if err != nil {
		return err
	}
	task.Annotations[harnessWrapperMetadataAnno] = string(metadata)
	return r.Patch(ctx, task, patch)
}

func (r *TaskReconciler) patchHarnessWrapperStarted(ctx context.Context, task *corev1alpha1.Task) error {
	patch := ctrlclient.MergeFrom(task.DeepCopy())
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	task.Annotations[harnessWrapperStartedAnno] = "true"
	return r.Patch(ctx, task, patch)
}

func harnessWrapperPlannedMetadata(task *corev1alpha1.Task, runtimeName string) map[string]string {
	metadata := map[string]string{}
	if task != nil && task.Annotations != nil {
		_ = json.Unmarshal([]byte(task.Annotations[harnessWrapperMetadataAnno]), &metadata)
	}
	if metadata == nil {
		metadata = map[string]string{}
	}
	if strings.TrimSpace(metadata["runtime"]) == "" {
		metadata["runtime"] = runtimeName
	}
	if strings.TrimSpace(metadata["wrapper"]) == "" {
		metadata["wrapper"] = "cli"
	}
	return metadata
}

func harnessWrapperPlannedTurnExpired(task *corev1alpha1.Task, now time.Time) bool {
	if task == nil || task.Annotations == nil {
		return false
	}
	plannedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(task.Annotations[harnessWrapperPlannedAtAnno]))
	if err != nil {
		return false
	}
	return now.Sub(plannedAt) > harnessWrapperPlannedTurnTTL
}

func (r *TaskReconciler) validateHarnessWrapperCapabilities(
	ctx context.Context,
	client *harness.Client,
	request harness.StartTurnRequest,
) error {
	capabilities, err := client.Capabilities(ctx)
	if err != nil {
		return fmt.Errorf("read harness runtime capabilities: %w", err)
	}
	wantRuntime := strings.TrimSpace(request.Metadata["runtime"])
	if wantRuntime != "" && capabilities.RuntimeName != wantRuntime {
		return fmt.Errorf("harness runtime %q does not match task runtime %q", capabilities.RuntimeName, wantRuntime)
	}
	return nil
}

func (r *TaskReconciler) releaseHarnessWrapperSessionLock(ctx context.Context, task *corev1alpha1.Task) error {
	if task.Spec.SessionRef == nil || r.SessionManager == nil {
		return nil
	}
	return r.SessionManager.ReleaseLock(ctx, task)
}

func (r *TaskReconciler) clearHarnessWrapperTurnState(ctx context.Context, task *corev1alpha1.Task) error {
	patch := ctrlclient.MergeFrom(task.DeepCopy())
	if task.Annotations != nil {
		delete(task.Annotations, harnessWrapperTurnIDAnnotation)
		delete(task.Annotations, harnessWrapperRuntimeAnnotation)
		delete(task.Annotations, harnessWrapperCorrelationIDAnno)
		delete(task.Annotations, harnessWrapperLastFrameSeqAnno)
		delete(task.Annotations, harnessWrapperStartedAnno)
		delete(task.Annotations, harnessWrapperPlannedAtAnno)
		delete(task.Annotations, harnessWrapperMetadataAnno)
	}
	return r.Patch(ctx, task, patch)
}

func harnessWrapperStreamErrorIsTerminal(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	for _, marker := range []string{"(401)", "(403)", "(404)", "(410)", "turn not found", "unauthorized"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func (r *TaskReconciler) cancelHarnessWrapperTurn(ctx context.Context, task *corev1alpha1.Task, reason string) error {
	if !taskHasHarnessWrapperTurn(task) {
		return nil
	}
	endpoint := harnessWrapperEndpoint()
	if endpoint == "" {
		return nil
	}
	client, err := harness.NewClient(endpoint, harness.WithBearerToken(harnessWrapperAuthValue()))
	if err != nil {
		return err
	}
	_, err = client.CancelTurn(ctx, harness.CancelTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        task.Namespace,
		TaskName:         task.Name,
		SessionName:      harnessWrapperSessionName(task),
		RuntimeSessionID: harness.RuntimeSessionID(strings.TrimSpace(task.Annotations[harnessWrapperRuntimeAnnotation])),
		TurnID:           harness.HarnessTurnID(strings.TrimSpace(task.Annotations[harnessWrapperTurnIDAnnotation])),
		CorrelationID:    strings.TrimSpace(task.Annotations[harnessWrapperCorrelationIDAnno]),
		Reason:           reason,
	})
	return err
}

func (r *TaskReconciler) plannedHarnessWrapperStartTurnRequest(
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
	now time.Time,
) harness.StartTurnRequest {
	deadline := now.Add(harnessWrapperNoTimeoutDuration)
	if task.Spec.Timeout != nil {
		deadline = now.Add(task.Spec.Timeout.Duration)
	}
	runtimeName := "generic"
	if agent != nil && agent.Spec.Runtime != nil {
		runtimeName = string(agent.Spec.Runtime.Type)
	}
	metadata := harnessWrapperPlannedMetadata(task, runtimeName)
	prompt := task.Spec.Prompt
	if prompt == "" && task.Spec.AI != nil {
		prompt = task.Spec.AI.Prompt
	}
	return harness.StartTurnRequest{
		Version:           harness.ProtocolVersion,
		Namespace:         task.Namespace,
		TaskName:          task.Name,
		SessionName:       harnessWrapperSessionName(task),
		RuntimeSessionID:  harness.RuntimeSessionID(strings.TrimSpace(task.Annotations[harnessWrapperRuntimeAnnotation])),
		TurnID:            harness.HarnessTurnID(strings.TrimSpace(task.Annotations[harnessWrapperTurnIDAnnotation])),
		CorrelationID:     strings.TrimSpace(task.Annotations[harnessWrapperCorrelationIDAnno]),
		Deadline:          deadline.UTC(),
		AuthIdentity:      harness.AuthIdentity{Subject: "task:" + task.Namespace + "/" + task.Name},
		Input:             harness.TurnInput{Prompt: prompt},
		ToolExecutionMode: harness.ToolExecutionModeObserved,
		Metadata:          metadata,
	}
}

func (r *TaskReconciler) harnessWrapperStartTurnRequest(
	ctx context.Context,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
	now time.Time,
	attempts int32,
) (harness.StartTurnRequest, error) {
	deadline := now.Add(harnessWrapperNoTimeoutDuration)
	if task.Spec.Timeout != nil {
		deadline = now.Add(task.Spec.Timeout.Duration)
	}
	runtimeName := "generic"
	if agent != nil && agent.Spec.Runtime != nil {
		runtimeName = string(agent.Spec.Runtime.Type)
	}
	turnID := harnessWrapperTurnID(task, attempts)
	correlationID := string(task.UID)
	if strings.TrimSpace(correlationID) == "" {
		correlationID = task.Namespace + "/" + task.Name
	}
	prompt := task.Spec.Prompt
	if prompt == "" && task.Spec.AI != nil {
		prompt = task.Spec.AI.Prompt
	}
	metadata, err := r.harnessWrapperTurnMetadata(ctx, task, agent, runtimeName)
	if err != nil {
		return harness.StartTurnRequest{}, err
	}
	return harness.StartTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        task.Namespace,
		TaskName:         task.Name,
		SessionName:      harnessWrapperSessionName(task),
		RuntimeSessionID: harness.RuntimeSessionID(task.Namespace + ":" + harnessWrapperSessionName(task) + ":" + runtimeName),
		TurnID:           turnID,
		CorrelationID:    correlationID,
		Deadline:         deadline.UTC(),
		AuthIdentity: harness.AuthIdentity{
			Subject: "task:" + task.Namespace + "/" + task.Name,
		},
		Input:             harness.TurnInput{Prompt: prompt},
		ToolExecutionMode: harness.ToolExecutionModeObserved,
		Metadata:          metadata,
	}, nil
}

//nolint:gocyclo // Collects runtime metadata from Agent and Task defaults in one place.
func (r *TaskReconciler) harnessWrapperTurnMetadata(
	ctx context.Context,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
	runtimeName string,
) (map[string]string, error) {
	metadata := map[string]string{
		"runtime": runtimeName,
		"wrapper": "cli",
	}
	if agent != nil {
		if agent.Spec.Model != nil && strings.TrimSpace(agent.Spec.Model.Name) != "" {
			metadata["model"] = strings.TrimSpace(agent.Spec.Model.Name)
		}
		if agent.Spec.SystemPrompt != nil {
			systemPrompt := strings.TrimSpace(agent.Spec.SystemPrompt.Inline)
			if systemPrompt == "" && agent.Spec.SystemPrompt.ConfigMapRef != nil {
				resolved, err := r.resolveHarnessWrapperConfigMapValue(
					ctx,
					agent.Namespace,
					agent.Spec.SystemPrompt.ConfigMapRef,
				)
				if err != nil {
					return nil, err
				}
				systemPrompt = strings.TrimSpace(resolved)
			}
			if systemPrompt != "" {
				metadata["systemPrompt"] = systemPrompt
			}
		}
	}
	if agent != nil && agent.Spec.Runtime != nil && agent.Spec.Runtime.DefaultMaxTurns != nil {
		metadata["maxTurns"] = strconv.FormatInt(int64(*agent.Spec.Runtime.DefaultMaxTurns), 10)
	}
	if task.Spec.AgentRuntime != nil && task.Spec.AgentRuntime.MaxTurns != nil {
		metadata["maxTurns"] = strconv.FormatInt(int64(*task.Spec.AgentRuntime.MaxTurns), 10)
	}
	allowedTools := []string(nil)
	if agent != nil && agent.Spec.Runtime != nil {
		allowedTools = agent.Spec.Runtime.DefaultAllowedTools
	}
	if task.Spec.AgentRuntime != nil && len(task.Spec.AgentRuntime.AllowedTools) > 0 {
		allowedTools = task.Spec.AgentRuntime.AllowedTools
	}
	if len(allowedTools) > 0 {
		metadata["allowedTools"] = strings.Join(allowedTools, ",")
	}
	if task.Spec.AgentRuntime != nil && len(task.Spec.AgentRuntime.DisallowedTools) > 0 {
		metadata["disallowedTools"] = strings.Join(task.Spec.AgentRuntime.DisallowedTools, ",")
	}
	allowBash := true
	if agent != nil && agent.Spec.Runtime != nil && agent.Spec.Runtime.DefaultAllowBash != nil {
		allowBash = *agent.Spec.Runtime.DefaultAllowBash
	}
	if task.Spec.AgentRuntime != nil && task.Spec.AgentRuntime.AllowBash != nil {
		allowBash = *task.Spec.AgentRuntime.AllowBash
	}
	metadata["allowBash"] = strconv.FormatBool(allowBash)
	if task.Spec.AgentRuntime != nil && task.Spec.AgentRuntime.Workspace != nil {
		ws := task.Spec.AgentRuntime.Workspace
		if strings.TrimSpace(ws.GitRepo) != "" {
			metadata["gitRepo"] = strings.TrimSpace(ws.GitRepo)
		}
		if strings.TrimSpace(ws.Branch) != "" {
			metadata["gitBranch"] = strings.TrimSpace(ws.Branch)
		}
		if strings.TrimSpace(ws.Ref) != "" {
			metadata["gitRef"] = strings.TrimSpace(ws.Ref)
		}
		if strings.TrimSpace(ws.SubPath) != "" {
			metadata["workspaceSubPath"] = strings.TrimSpace(ws.SubPath)
		}
	}
	return metadata, nil
}

func (r *TaskReconciler) resolveHarnessWrapperConfigMapValue(
	ctx context.Context,
	namespace string,
	ref *corev1alpha1.ConfigMapKeySelector,
) (string, error) {
	if ref == nil {
		return "", nil
	}
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, ctrlclient.ObjectKey{Name: ref.Name, Namespace: namespace}, cm); err != nil {
		return "", fmt.Errorf("resolve harness runtime system prompt ConfigMap %s/%s: %w", namespace, ref.Name, err)
	}
	value, ok := cm.Data[ref.Key]
	if !ok {
		return "", fmt.Errorf("resolve harness runtime system prompt ConfigMap %s/%s key %q: key not found", namespace, ref.Name, ref.Key)
	}
	return value, nil
}

func harnessWrapperTurnID(task *corev1alpha1.Task, attempts int32) harness.HarnessTurnID {
	identity := fmt.Sprintf("%s/%s/%s/%d", task.Namespace, task.Name, task.UID, attempts)
	sum := sha256.Sum256([]byte(identity))
	prefix := harnessWrapperTurnIDPrefix(task.Name)
	return harness.HarnessTurnID(fmt.Sprintf("%s-%s-%d", prefix, hex.EncodeToString(sum[:])[:12], attempts))
}

func harnessWrapperTurnIDPrefix(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out.WriteRune(r)
		case r == '-', r == '_', r == '.':
			out.WriteRune(r)
		default:
			out.WriteByte('-')
		}
		if out.Len() >= 40 {
			break
		}
	}
	prefix := strings.Trim(out.String(), "-_.")
	if prefix == "" {
		return "turn"
	}
	return prefix
}

func harnessWrapperSessionName(task *corev1alpha1.Task) string {
	if task != nil && task.Spec.SessionRef != nil && strings.TrimSpace(task.Spec.SessionRef.Name) != "" {
		return strings.TrimSpace(task.Spec.SessionRef.Name)
	}
	if task != nil {
		return task.Name
	}
	return "default"
}

func harnessWrapperTaskAgentName(task *corev1alpha1.Task) string {
	if task != nil && task.Spec.AgentRef != nil {
		return task.Spec.AgentRef.Name
	}
	return ""
}

func harnessWrapperLastFrameSeq(task *corev1alpha1.Task) int64 {
	if task == nil || task.Annotations == nil {
		return 0
	}
	seq, err := strconv.ParseInt(strings.TrimSpace(task.Annotations[harnessWrapperLastFrameSeqAnno]), 10, 64)
	if err != nil || seq < 0 {
		return 0
	}
	return seq
}

func (r *TaskReconciler) patchHarnessWrapperLastFrameSeq(ctx context.Context, task *corev1alpha1.Task, seq int64) error {
	patch := ctrlclient.MergeFrom(task.DeepCopy())
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	task.Annotations[harnessWrapperLastFrameSeqAnno] = strconv.FormatInt(seq, 10)
	return r.Patch(ctx, task, patch)
}
