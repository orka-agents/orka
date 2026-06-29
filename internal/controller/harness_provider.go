package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/harness"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/store"
)

const (
	defaultHarnessTurnTimeout = 30 * time.Minute
	harnessTurnTimeoutMessage = "harness turn did not complete before timeout"
)

func taskHarnessEndpoint(task *corev1alpha1.Task) string {
	if task == nil || task.Annotations == nil {
		return ""
	}
	if harnessTaskHasControllerTurnIdentity(task) {
		if pinned, ok := task.Annotations[labels.AnnotationHarnessEndpointPinned]; ok {
			return strings.TrimSpace(pinned)
		}
	}
	return strings.TrimSpace(task.Annotations[labels.AnnotationHarnessEndpoint])
}

func taskUsesHarnessProvider(task *corev1alpha1.Task) bool {
	return taskHarnessEndpoint(task) != ""
}

func newHarnessProviderClient(endpoint string) (*harness.Client, error) {
	if strings.TrimSpace(endpoint) == strings.TrimSpace(harnessWrapperEndpoint()) {
		return harness.NewClient(endpoint, harness.WithBearerToken(harnessWrapperAuthValue()))
	}
	return harness.NewClient(endpoint)
}

func (r *TaskReconciler) runHarnessTask(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) {
	if r.ExecutionEventStore == nil {
		return r.failTask(ctx, task, "harness task requires execution event store")
	}
	if result, handled, err := r.handleAnnotatedHarnessTerminalIfRecovered(ctx, task); handled || err != nil {
		return result, err
	}
	if result, rejected, err := r.rejectUnsupportedServiceHarnessBrokeredTools(ctx, task); rejected || err != nil {
		return result, err
	}
	endpoint := taskHarnessEndpoint(task)
	if err := r.validateHarnessServiceEndpoint(ctx, task, endpoint); err != nil {
		if harnessTaskHasControllerTurnAnnotations(task) {
			if releaseErr := r.markAnnotatedHarnessRuntimeSessionUnhealthy(ctx, task, "invalid harness provider service before start"); releaseErr != nil {
				return ctrl.Result{}, releaseErr
			}
		}
		return r.failTask(ctx, task, fmt.Sprintf("invalid harness provider service: %v", err))
	}
	provider := harness.KubernetesServiceProvider{
		EndpointURL:           endpoint,
		AllowInsecureLoopback: r.HarnessEndpointAllowInsecureLoopback,
	}
	eventSessionName := taskSessionName(task)
	sessionName := harnessProtocolSessionName(task, eventSessionName)
	runtimeSessionName := harnessRuntimeOwnerSessionName(task, eventSessionName)
	providerEndpoint, err := provider.Endpoint(ctx, harness.RuntimeSessionOwner{
		Namespace:   task.Namespace,
		SessionName: runtimeSessionName,
		ActiveTask:  task.Name,
		AgentName:   taskAgentName(task),
		Provider:    harness.ProviderKindKubernetesService,
	})
	if err != nil {
		return r.failTask(ctx, task, fmt.Sprintf("invalid harness provider: %v", err))
	}
	harnessClient, err := newHarnessProviderClient(providerEndpoint)
	if err != nil {
		return r.failTask(ctx, task, fmt.Sprintf("invalid harness client: %v", err))
	}
	if result, done, err := r.ensureHarnessHealthy(ctx, task, harnessClient); done || err != nil {
		return result, err
	}
	capabilities, err := harnessClient.Capabilities(ctx)
	if err != nil {
		if releaseErr := r.markAnnotatedHarnessRuntimeSessionUnhealthy(ctx, task, "harness capabilities check failed"); releaseErr != nil {
			return ctrl.Result{}, releaseErr
		}
		return r.failTask(ctx, task, fmt.Sprintf("harness capabilities check failed: %v", err))
	}
	if requestedMode := harnessToolExecutionMode(task); !harnessCapabilitiesSupportToolMode(capabilities, requestedMode) {
		message := fmt.Sprintf("harness does not support %s tool execution", requestedMode)
		if releaseErr := r.markAnnotatedHarnessRuntimeSessionUnhealthy(ctx, task, message); releaseErr != nil {
			return ctrl.Result{}, releaseErr
		}
		return r.failTask(ctx, task, message)
	}

	runtimeSessionID := harnessRuntimeSessionID(task, runtimeSessionName)
	claimedRuntime, err := r.claimHarnessRuntimeSession(ctx, task, runtimeSessionName)
	if err != nil {
		return r.failTask(ctx, task, fmt.Sprintf("failed to claim harness runtime session: %v", err))
	}
	if claimedRuntime != nil {
		runtimeSessionID = string(claimedRuntime.ID)
		r.recordHarnessRuntimeSessionEvent(ctx, task, claimedRuntime, "harness runtime session claimed")
	}
	turnID := harnessTurnID(task)
	correlationID := harnessCorrelationID(task)
	hadPersistedIdentity := harnessTaskHasControllerIdentity(task) &&
		strings.TrimSpace(task.Annotations[labels.AnnotationHarnessRuntimeSession]) == runtimeSessionID &&
		strings.TrimSpace(task.Annotations[labels.AnnotationHarnessTurn]) == turnID &&
		strings.TrimSpace(task.Annotations[labels.AnnotationHarnessCorrelation]) == correlationID
	request := harness.StartTurnRequest{
		Version:           harness.ProtocolVersion,
		Namespace:         task.Namespace,
		TaskName:          task.Name,
		SessionName:       sessionName,
		RuntimeSessionID:  harness.RuntimeSessionID(runtimeSessionID),
		TurnID:            harness.HarnessTurnID(turnID),
		CorrelationID:     correlationID,
		Deadline:          harnessDeadline(task),
		AuthIdentity:      harness.AuthIdentity{Subject: "system:orka-task-controller"},
		Input:             harness.TurnInput{Prompt: taskPrompt(task)},
		ToolExecutionMode: harnessToolExecutionMode(task),
		Metadata: map[string]string{
			"provider": string(harness.ProviderKindKubernetesService),
		},
	}
	started, err := r.markHarnessTaskTurnIdentity(ctx, task, runtimeSessionID, turnID, correlationID)
	if err != nil {
		if cleanupErr := r.markClaimedHarnessRuntimeSessionUnhealthy(ctx, task, claimedRuntime, "failed to persist harness turn identity"); cleanupErr != nil {
			return ctrl.Result{}, cleanupErr
		}
		return ctrl.Result{}, err
	}
	if !started {
		if cleanupErr := r.markClaimedHarnessRuntimeSessionUnhealthy(ctx, task, claimedRuntime, "task no longer startable before harness turn start"); cleanupErr != nil {
			return ctrl.Result{}, cleanupErr
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	accepted, immediateResult, done, err := r.startOrResumeHarnessTurn(
		ctx,
		task,
		harnessClient,
		request,
		claimedRuntime,
		hadPersistedIdentity,
		sessionName,
		eventSessionName,
	)
	if done || err != nil {
		return immediateResult, err
	}
	if err := r.recordTaskLifecycleEvent(ctx, task, events.ExecutionEventTypeTaskStarted, events.ExecutionEventSeverityInfo,
		"Task started with harness provider"); err != nil {
		return ctrl.Result{}, err
	}

	runner := harness.TurnRunner{
		Client:     harnessClient,
		EventStore: r.ExecutionEventStore,
		MapContext: harness.EventMapContext{
			Namespace:   task.Namespace,
			TaskName:    task.Name,
			SessionName: eventSessionName,
			AgentName:   taskAgentName(task),
		},
		TurnTimeout: harnessTurnTimeout(task),
	}
	result, err := runner.RunAccepted(ctx, request, *accepted)
	return r.handleHarnessTurnRunResult(ctx, task, harnessClient, request.TurnID, result, err)
}

func (r *TaskReconciler) handleHarnessTurnRunResult(
	ctx context.Context,
	task *corev1alpha1.Task,
	harnessClient *harness.Client,
	turnID harness.HarnessTurnID,
	result harness.TurnRunResult,
	runErr error,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	if result.Failed == nil && !result.Cancelled {
		if err := r.saveHarnessCompletedResult(ctx, task, harnessClient, turnID, result.Completed); err != nil {
			logger.Error(err, "failed to persist harness completion result", "task", task.Name)
			if isHarnessNotFound(err) || isHarnessCompletionResultUnrecoverable(err) {
				message := "harness completion output could not be recovered"
				if isHarnessNotFound(err) {
					message = "harness completion output reference was not found"
				}
				if markErr := r.markAnnotatedHarnessRuntimeSessionUnhealthy(ctx, task, message); markErr != nil {
					return ctrl.Result{}, markErr
				}
				return r.failTask(ctx, task, message)
			}
			return ctrl.Result{}, err
		}
	}
	if result.Failed != nil && (!result.Failed.Retryable || !r.shouldRetry(task)) {
		if err := r.saveHarnessFailedResult(ctx, task, harnessClient, turnID, result.Failed); err != nil {
			logger.Error(err, "failed to persist harness failure result", "task", task.Name)
			return ctrl.Result{}, err
		}
	}
	if runErr != nil {
		if result.Failed != nil || result.Cancelled {
			return r.completeHarnessTaskFromTerminal(ctx, task, nil, result.Failed, result.Cancelled)
		}
		if result.Accepted != nil {
			if isHarnessTerminalFrameMissing(runErr) {
				return r.failHarnessTerminalFrameMissing(ctx, task, runErr)
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return r.failTask(ctx, task, runErr.Error())
	}
	return r.completeHarnessTaskFromTerminal(ctx, task, result.Completed, result.Failed, result.Cancelled)
}

func (r *TaskReconciler) failHarnessTerminalFrameMissing(ctx context.Context, task *corev1alpha1.Task, err error) (ctrl.Result, error) {
	message := err.Error()
	if markErr := r.markAnnotatedHarnessRuntimeSessionUnhealthy(ctx, task, message); markErr != nil {
		return ctrl.Result{}, markErr
	}
	return r.failTask(ctx, task, message)
}

func (r *TaskReconciler) ensureHarnessHealthy(
	ctx context.Context,
	task *corev1alpha1.Task,
	harnessClient *harness.Client,
) (ctrl.Result, bool, error) {
	health, err := harnessClient.Health(ctx)
	if err != nil {
		if releaseErr := r.markAnnotatedHarnessRuntimeSessionUnhealthy(ctx, task, "harness health check failed"); releaseErr != nil {
			return ctrl.Result{}, true, releaseErr
		}
		result, failErr := r.failTask(ctx, task, fmt.Sprintf("harness health check failed: %v", err))
		return result, true, failErr
	}
	if !health.Ready || health.Status == harness.HealthStatusUnhealthy {
		if releaseErr := r.markAnnotatedHarnessRuntimeSessionUnhealthy(ctx, task, "harness is not ready"); releaseErr != nil {
			return ctrl.Result{}, true, releaseErr
		}
		result, failErr := r.failTask(ctx, task, fmt.Sprintf("harness is not ready: %s", health.Status))
		return result, true, failErr
	}
	return ctrl.Result{}, false, nil
}

func (r *TaskReconciler) startOrResumeHarnessTurn(
	ctx context.Context,
	task *corev1alpha1.Task,
	harnessClient *harness.Client,
	request harness.StartTurnRequest,
	claimedRuntime *harness.RuntimeSession,
	hadPersistedIdentity bool,
	sessionName string,
	eventSessionName string,
) (*harness.StartTurnResponse, ctrl.Result, bool, error) {
	if hadPersistedIdentity {
		if result, resumed, err := r.tryResumeUnacceptedHarnessTurn(ctx, task, harnessClient, request, sessionName, eventSessionName); resumed || err != nil {
			return nil, result, true, err
		}
	}
	accepted, err := harnessClient.StartTurn(ctx, request)
	if err != nil {
		if isAmbiguousHarnessStartError(err) {
			if harnessTurnIdentityTimedOut(task) {
				if cancelErr := r.cancelHarnessProviderTurn(ctx, task, "harness start turn did not complete before timeout"); cancelErr != nil {
					log.FromContext(ctx).Error(cancelErr, "failed to cancel timed-out ambiguous harness provider turn", "task", task.Name)
				}
				if cleanupErr := r.markClaimedHarnessRuntimeSessionUnhealthy(ctx, task, claimedRuntime, "harness start turn did not complete before timeout"); cleanupErr != nil {
					return nil, ctrl.Result{}, true, cleanupErr
				}
				result, failErr := r.failTask(ctx, task, "harness start turn did not complete before timeout")
				return nil, result, true, failErr
			}
			return nil, ctrl.Result{RequeueAfter: 5 * time.Second}, true, nil
		}
		if cleanupErr := r.markClaimedHarnessRuntimeSessionUnhealthy(ctx, task, claimedRuntime, "harness start turn failed after task turn identity persistence"); cleanupErr != nil {
			return nil, ctrl.Result{}, true, cleanupErr
		}
		result, failErr := r.failTask(ctx, task, fmt.Sprintf("harness start turn failed: %v", err))
		return nil, result, true, failErr
	}
	if err := r.markHarnessTaskTurnAccepted(ctx, task, time.Now().UTC()); err != nil {
		_, _ = harnessClient.CancelTurn(context.Background(), harness.CancelTurnRequest{
			Version:          harness.ProtocolVersion,
			Namespace:        task.Namespace,
			TaskName:         task.Name,
			SessionName:      sessionName,
			RuntimeSessionID: request.RuntimeSessionID,
			TurnID:           request.TurnID,
			CorrelationID:    request.CorrelationID,
			Reason:           "failed to persist harness turn acceptance",
		})
		if cleanupErr := r.markClaimedHarnessRuntimeSessionUnhealthy(ctx, task, claimedRuntime, "failed to persist harness turn acceptance"); cleanupErr != nil {
			return nil, ctrl.Result{}, true, cleanupErr
		}
		return nil, ctrl.Result{}, true, err
	}
	return accepted, ctrl.Result{}, false, nil
}

func isAmbiguousHarnessStartError(err error) bool {
	var clientErr harness.ClientError
	if !errors.As(err, &clientErr) {
		return true
	}
	if clientErr.StatusCode >= 500 {
		return true
	}
	if clientErr.StatusCode != 0 {
		return false
	}
	message := strings.ToLower(clientErr.Message)
	for _, deterministic := range []string{
		"unsupported version",
		"did not accept",
		"accepted runtime session",
		"accepted turn",
		"accepted correlation",
	} {
		if strings.Contains(message, deterministic) {
			return false
		}
	}
	return true
}

func (r *TaskReconciler) tryResumeUnacceptedHarnessTurn(
	ctx context.Context,
	task *corev1alpha1.Task,
	harnessClient *harness.Client,
	request harness.StartTurnRequest,
	sessionName string,
	eventSessionName string,
) (ctrl.Result, bool, error) {
	if strings.TrimSpace(request.SessionName) == "" {
		request.SessionName = sessionName
	}
	runtimeSessionID := request.RuntimeSessionID
	turnID := request.TurnID
	correlationID := request.CorrelationID
	lastFrameSeq, completed, failed, cancelled, err := r.latestHarnessFrameState(ctx, task, runtimeSessionID, turnID)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	if completed != nil || failed != nil || cancelled {
		if markErr := r.markRecoveredHarnessTurnAccepted(ctx, task); markErr != nil {
			return ctrl.Result{}, true, markErr
		}
		out, completeErr := r.handleHarnessTurnRunResult(ctx, task, harnessClient, turnID, harness.TurnRunResult{
			Completed: completed,
			Failed:    failed,
			Cancelled: cancelled,
		}, nil)
		return out, true, completeErr
	}

	if task != nil && task.Annotations != nil && strings.TrimSpace(task.Annotations[labels.AnnotationHarnessTurnStartedAt]) != "" {
		if err := r.recordHarnessTaskStartedIfMissing(ctx, task); err != nil {
			return ctrl.Result{}, true, err
		}
	}

	streamCtx, cancel := harnessTurnIdentityContext(ctx, task)
	defer cancel()
	result := harness.TurnRunResult{Accepted: &harness.StartTurnResponse{
		Version:          harness.ProtocolVersion,
		Accepted:         true,
		RuntimeSessionID: runtimeSessionID,
		TurnID:           turnID,
		CorrelationID:    correlationID,
	}}
	err = r.streamRecoveredHarnessTurn(streamCtx, task, harnessClient, eventSessionName, runtimeSessionID, turnID, correlationID, lastFrameSeq, &result)
	if isHarnessNotFound(err) && len(result.Events) == 0 {
		if lastFrameSeq == 0 && (task == nil || task.Annotations == nil || strings.TrimSpace(task.Annotations[labels.AnnotationHarnessTurnStartedAt]) == "") {
			return ctrl.Result{}, false, nil
		}
		message := "harness turn disappeared after identity was persisted"
		if markErr := r.markAnnotatedHarnessRuntimeSessionUnhealthy(ctx, task, message); markErr != nil {
			return ctrl.Result{}, true, markErr
		}
		out, failErr := r.failTask(ctx, task, message)
		return out, true, failErr
	}
	if err == nil || len(result.Events) > 0 || result.Completed != nil || result.Failed != nil || result.Cancelled {
		if markErr := r.markRecoveredHarnessTurnAccepted(ctx, task); markErr != nil {
			return ctrl.Result{}, true, markErr
		}
	}
	if err != nil {
		out, handleErr := r.handleRecoveredHarnessTurnError(ctx, task, harnessClient, request.TurnID, result, err)
		return out, true, handleErr
	}
	if harnessResumeMissingTerminal(result.Completed, result.Failed, result.Cancelled) {
		out, failErr := r.failHarnessTerminalFrameMissing(ctx, task, fmt.Errorf("harness turn ended without terminal frame"))
		return out, true, failErr
	}
	out, completeErr := r.handleHarnessTurnRunResult(ctx, task, harnessClient, request.TurnID, result, nil)
	return out, true, completeErr
}

func (r *TaskReconciler) streamRecoveredHarnessTurn(
	ctx context.Context,
	task *corev1alpha1.Task,
	harnessClient *harness.Client,
	eventSessionName string,
	runtimeSessionID harness.RuntimeSessionID,
	turnID harness.HarnessTurnID,
	correlationID string,
	lastFrameSeq int64,
	result *harness.TurnRunResult,
) error {
	return harnessClient.StreamFrames(ctx, turnID, lastFrameSeq, func(frame harness.HarnessEventFrame) error {
		if frame.RuntimeSessionID != runtimeSessionID || frame.TurnID != turnID || frame.CorrelationID != correlationID {
			return fmt.Errorf("harness frame identity does not match persisted task turn")
		}
		if frame.Seq <= lastFrameSeq {
			return fmt.Errorf("non-monotonic harness frame seq %d after %d", frame.Seq, lastFrameSeq)
		}
		lastFrameSeq = frame.Seq
		mapped, err := harness.MapFrameToExecutionEvent(frame, harness.EventMapContext{
			Namespace: task.Namespace, TaskName: task.Name, SessionName: eventSessionName, AgentName: taskAgentName(task),
		})
		if err != nil {
			return err
		}
		appended, err := r.ExecutionEventStore.AppendExecutionEvent(ctx, mapped)
		if err != nil {
			return err
		}
		result.Frames = append(result.Frames, frame)
		result.Events = append(result.Events, *appended)
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
}

func (r *TaskReconciler) markRecoveredHarnessTurnAccepted(ctx context.Context, task *corev1alpha1.Task) error {
	if markErr := r.markHarnessTaskTurnAccepted(ctx, task, time.Now().UTC()); markErr != nil {
		return markErr
	}
	return r.recordTaskLifecycleEvent(ctx, task, events.ExecutionEventTypeTaskStarted, events.ExecutionEventSeverityInfo, "Task started with harness provider")
}

func (r *TaskReconciler) handleRecoveredHarnessTurnError(
	ctx context.Context,
	task *corev1alpha1.Task,
	harnessClient *harness.Client,
	turnID harness.HarnessTurnID,
	result harness.TurnRunResult,
	err error,
) (ctrl.Result, error) {
	if result.Completed != nil || result.Failed != nil || result.Cancelled {
		return r.handleHarnessTurnRunResult(ctx, task, harnessClient, turnID, harness.TurnRunResult{
			Completed: result.Completed,
			Failed:    result.Failed,
			Cancelled: result.Cancelled,
		}, nil)
	}
	if isHarnessTerminalFrameMissing(err) || isDeterministicHarnessResumeError(ctx, err) {
		if markErr := r.markAnnotatedHarnessRuntimeSessionUnhealthy(ctx, task, err.Error()); markErr != nil {
			return ctrl.Result{}, markErr
		}
		return r.failTask(ctx, task, err.Error())
	}
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(strings.ToLower(err.Error()), "deadline") || strings.Contains(strings.ToLower(err.Error()), "timeout") {
		return r.failTimedOutHarnessProviderTurn(ctx, task)
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func isHarnessCompletionResultUnrecoverable(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "truncated without output reference")
}

func isHarnessTerminalFrameMissing(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "without terminal frame")
}

func isHarnessNotFound(err error) bool {
	var clientErr harness.ClientError
	return errors.As(err, &clientErr) && clientErr.StatusCode == http.StatusNotFound
}

func harnessProviderTurnMayBeActive(task *corev1alpha1.Task) bool {
	if task == nil {
		return false
	}
	switch task.Status.Phase {
	case "", corev1alpha1.TaskPhasePending, corev1alpha1.TaskPhaseScheduled, corev1alpha1.TaskPhaseRunning, corev1alpha1.TaskPhaseCancelled:
		return true
	default:
		return false
	}
}

func (r *TaskReconciler) cancelHarnessProviderTurn(ctx context.Context, task *corev1alpha1.Task, reason string) error {
	if !taskUsesHarnessProvider(task) || !harnessTaskHasControllerTurnIdentity(task) || !harnessProviderTurnMayBeActive(task) {
		return nil
	}
	endpointURL := taskHarnessEndpoint(task)
	if serviceName, err := harnessServiceNameFromHostFromEndpoint(endpointURL, task.Namespace); err == nil {
		service := &corev1.Service{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: serviceName}, service); err != nil {
			return err
		}
	}
	provider := harness.KubernetesServiceProvider{
		EndpointURL:           endpointURL,
		AllowInsecureLoopback: r.HarnessEndpointAllowInsecureLoopback,
	}
	sessionName := taskSessionName(task)
	if sessionName == "" {
		sessionName = task.Name
	}
	endpoint, err := provider.Endpoint(ctx, harness.RuntimeSessionOwner{
		Namespace:   task.Namespace,
		SessionName: sessionName,
		ActiveTask:  task.Name,
		AgentName:   taskAgentName(task),
		Provider:    harness.ProviderKindKubernetesService,
	})
	if err != nil {
		return err
	}
	harnessClient, err := newHarnessProviderClient(endpoint)
	if err != nil {
		return err
	}
	_, err = harnessClient.CancelTurn(ctx, harness.CancelTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        task.Namespace,
		TaskName:         task.Name,
		SessionName:      sessionName,
		RuntimeSessionID: harness.RuntimeSessionID(strings.TrimSpace(task.Annotations[labels.AnnotationHarnessRuntimeSession])),
		TurnID:           harness.HarnessTurnID(strings.TrimSpace(task.Annotations[labels.AnnotationHarnessTurn])),
		CorrelationID:    strings.TrimSpace(task.Annotations[labels.AnnotationHarnessCorrelation]),
		Reason:           reason,
	})
	if isHarnessNotFound(err) {
		return nil
	}
	return err
}

func (r *TaskReconciler) saveHarnessCompletedResult(
	ctx context.Context,
	task *corev1alpha1.Task,
	harnessClient *harness.Client,
	turnID harness.HarnessTurnID,
	completed *harness.TurnCompleted,
) error {
	if completed == nil || r.ResultStore == nil {
		return nil
	}
	var resultBytes []byte
	inlineResult := []byte(completed.Result)
	if strings.TrimSpace(completed.OutputRef) != "" {
		if existing, err := r.ResultStore.GetResult(ctx, task.Namespace, task.Name); err == nil {
			task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
			if len(existing) == 0 {
				return nil
			}
			resultBytes = existing
		} else if harnessClient != nil {
			fetched, err := harnessClient.FetchTurnOutput(ctx, turnID, completed.OutputRef)
			if err != nil {
				if completed.ResultTruncated || len(inlineResult) == 0 {
					return err
				}
			} else {
				resultBytes = fetched
			}
		} else if completed.ResultTruncated || len(inlineResult) == 0 {
			return fmt.Errorf("harness completion output reference requires harness client")
		}
	}
	if len(resultBytes) == 0 && !completed.ResultTruncated {
		resultBytes = inlineResult
	}
	if len(resultBytes) == 0 && completed.ResultTruncated {
		return fmt.Errorf("harness completion result was truncated without output reference")
	}
	if len(resultBytes) == 0 {
		return nil
	}
	if err := r.ResultStore.SaveResult(ctx, task.Namespace, task.Name, resultBytes); err != nil {
		return err
	}
	task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
	return nil
}

func (r *TaskReconciler) saveHarnessFailedResult(
	ctx context.Context,
	task *corev1alpha1.Task,
	harnessClient *harness.Client,
	turnID harness.HarnessTurnID,
	failed *harness.TurnFailed,
) error {
	if failed == nil || r.ResultStore == nil {
		return nil
	}
	var resultBytes []byte
	if strings.TrimSpace(failed.OutputRef) != "" && harnessClient == nil && task.Status.ResultRef != nil && task.Status.ResultRef.Available {
		if existing, err := r.ResultStore.GetResult(ctx, task.Namespace, task.Name); err == nil && len(existing) > 0 {
			return nil
		}
	}
	if strings.TrimSpace(failed.OutputRef) != "" && harnessClient != nil {
		fetched, err := harnessClient.FetchTurnOutput(ctx, turnID, failed.OutputRef)
		if isHarnessNotFound(err) {
			resultBytes = []byte(strings.TrimSpace(firstNonEmpty(failed.Result, failed.Message, failed.Reason, "harness failed output reference was not found")))
		} else if err != nil {
			return err
		} else {
			resultBytes = fetched
		}
	} else {
		resultBytes = []byte(failed.Result)
	}
	if len(resultBytes) == 0 {
		return nil
	}
	if err := r.ResultStore.SaveResult(ctx, task.Namespace, task.Name, resultBytes); err != nil {
		return err
	}
	task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
	return nil
}

func (r *TaskReconciler) validateHarnessServiceEndpoint(ctx context.Context, task *corev1alpha1.Task, endpoint string) error {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return err
	}
	if parsed.Scheme != controllerURLSchemeHTTP && parsed.Scheme != controllerURLSchemeHTTPS {
		return fmt.Errorf("harness endpoint scheme must be http or https")
	}
	if parsed.User != nil {
		return fmt.Errorf("harness endpoint must not include user info")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("harness endpoint must not include query or fragment")
	}
	if isLoopbackHarnessHost(parsed.Hostname()) {
		if r.HarnessEndpointAllowInsecureLoopback && strings.TrimSpace(endpoint) == strings.TrimSpace(harnessWrapperEndpoint()) {
			return nil
		}
		return fmt.Errorf("harness endpoint loopback host is not allowed")
	}
	serviceName, err := harnessServiceNameFromHost(parsed.Hostname(), task.Namespace)
	if err != nil {
		return err
	}
	service := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: serviceName}, service); err != nil {
		return err
	}
	if service.Spec.Type == corev1.ServiceTypeExternalName {
		return fmt.Errorf("ExternalName services are not supported for harness providers")
	}
	if len(service.Spec.Selector) == 0 {
		return fmt.Errorf("service %q must define a non-empty pod selector for harness providers", serviceName)
	}
	for key, value := range service.Spec.Selector {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			return fmt.Errorf("service %q must define non-empty pod selector labels for harness providers", serviceName)
		}
	}
	if service.Annotations[labels.AnnotationHarnessProvider] != string(harness.ProviderKindKubernetesService) {
		return fmt.Errorf("service %q must be annotated %s=%q", serviceName, labels.AnnotationHarnessProvider, harness.ProviderKindKubernetesService)
	}
	return nil
}

func harnessServiceNameFromHostFromEndpoint(endpoint, namespace string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return "", err
	}
	return harnessServiceNameFromHost(parsed.Hostname(), namespace)
}

func harnessServiceNameFromHost(host, namespace string) (string, error) {
	normalizedHost := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	namespace = strings.ToLower(strings.TrimSpace(namespace))
	for _, suffix := range []string{"." + namespace + ".svc.cluster.local", "." + namespace + ".svc"} {
		if before, ok := strings.CutSuffix(normalizedHost, suffix); ok {
			serviceName := before
			if serviceName == "" || strings.Contains(serviceName, ".") {
				return "", fmt.Errorf("harness endpoint host must name one in-namespace Service")
			}
			return serviceName, nil
		}
	}
	return "", fmt.Errorf("harness endpoint host must be a Service DNS name in namespace %q", namespace)
}

func (r *TaskReconciler) markHarnessTaskTurnIdentity(
	ctx context.Context,
	task *corev1alpha1.Task,
	runtimeSessionID string,
	turnID string,
	correlationID string,
) (bool, error) {
	now := metav1.Now()
	if harnessTaskHasControllerIdentity(task) && task.Annotations != nil &&
		strings.TrimSpace(task.Annotations[labels.AnnotationHarnessRuntimeSession]) == runtimeSessionID &&
		strings.TrimSpace(task.Annotations[labels.AnnotationHarnessTurn]) == turnID &&
		strings.TrimSpace(task.Annotations[labels.AnnotationHarnessCorrelation]) == correlationID {
		return true, r.repairHarnessTaskTurnIdentityStatus(ctx, task, now)
	}
	attempts := task.Status.Attempts + 1
	started := false
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &corev1alpha1.Task{}
		if err := r.Get(ctx, types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, latest); err != nil {
			return err
		}
		if !canStartTaskJob(latest.Status.Phase) {
			return nil
		}
		patch := client.MergeFrom(latest.DeepCopy())
		if latest.Annotations == nil {
			latest.Annotations = map[string]string{}
		}
		latest.Annotations[labels.AnnotationHarnessRuntimeSession] = runtimeSessionID
		latest.Annotations[labels.AnnotationHarnessTurn] = turnID
		latest.Annotations[labels.AnnotationHarnessCorrelation] = correlationID
		latest.Annotations[labels.AnnotationHarnessEndpointPinned] = taskHarnessEndpoint(task)
		latest.Annotations[labels.AnnotationHarnessReusePolicyPinned] = string(harnessReusePolicy(task))
		latest.Annotations[labels.AnnotationHarnessTurnIdentityStartedAt] = now.Format(time.RFC3339Nano)
		delete(latest.Annotations, labels.AnnotationHarnessTurnStartedAt)
		if err := r.Patch(ctx, latest, patch); err != nil {
			return err
		}
		started = true
		return nil
	}); err != nil {
		return false, err
	}
	if !started {
		return false, nil
	}
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	task.Annotations[labels.AnnotationHarnessRuntimeSession] = runtimeSessionID
	task.Annotations[labels.AnnotationHarnessTurn] = turnID
	task.Annotations[labels.AnnotationHarnessCorrelation] = correlationID
	task.Annotations[labels.AnnotationHarnessEndpointPinned] = taskHarnessEndpoint(task)
	task.Annotations[labels.AnnotationHarnessReusePolicyPinned] = string(harnessReusePolicy(task))
	task.Annotations[labels.AnnotationHarnessTurnIdentityStartedAt] = now.Format(time.RFC3339Nano)
	delete(task.Annotations, labels.AnnotationHarnessTurnStartedAt)
	if err := r.updateStatusWithRetry(ctx, task, func(t *corev1alpha1.Task) {
		t.Status.StartTime = &now
		t.Status.Attempts = attempts
		meta.SetStatusCondition(&t.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeJobCreated,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: now,
			Reason:             "HarnessProviderStarting",
			Message:            "Harness provider turn identity persisted",
		})
	}); err != nil {
		return false, err
	}
	return true, nil
}

func (r *TaskReconciler) repairHarnessTaskTurnIdentityStatus(ctx context.Context, task *corev1alpha1.Task, startedAt metav1.Time) error {
	return r.updateStatusWithRetry(ctx, task, func(t *corev1alpha1.Task) {
		if t.Status.StartTime == nil {
			t.Status.StartTime = &startedAt
		}
		if t.Status.Attempts < 1 {
			t.Status.Attempts = 1
		}
		meta.SetStatusCondition(&t.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeJobCreated,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: startedAt,
			Reason:             "HarnessProviderStarting",
			Message:            "Harness provider turn identity persisted",
		})
	})
}

func (r *TaskReconciler) markHarnessTaskTurnAccepted(ctx context.Context, task *corev1alpha1.Task, acceptedAt time.Time) error {
	if acceptedAt.IsZero() {
		acceptedAt = time.Now().UTC()
	}
	acceptedAt = acceptedAt.UTC()
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &corev1alpha1.Task{}
		if err := r.Get(ctx, types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, latest); err != nil {
			return err
		}
		patch := client.MergeFrom(latest.DeepCopy())
		if latest.Annotations == nil {
			latest.Annotations = map[string]string{}
		}
		latest.Annotations[labels.AnnotationHarnessTurnStartedAt] = acceptedAt.Format(time.RFC3339Nano)
		delete(latest.Annotations, labels.AnnotationHarnessTurnIdentityStartedAt)
		return r.Patch(ctx, latest, patch)
	}); err != nil {
		return err
	}
	if err := r.updateStatusWithRetry(ctx, task, func(t *corev1alpha1.Task) {
		if t.Status.Phase == "" || t.Status.Phase == corev1alpha1.TaskPhasePending || t.Status.Phase == corev1alpha1.TaskPhaseScheduled {
			t.Status.Phase = corev1alpha1.TaskPhaseRunning
		}
		if t.Status.StartTime == nil {
			t.Status.StartTime = &metav1.Time{Time: acceptedAt}
		}
		meta.SetStatusCondition(&t.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeJobCreated,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Time{Time: acceptedAt},
			Reason:             "HarnessProviderStarted",
			Message:            "Harness provider turn accepted",
		})
	}); err != nil {
		_ = r.clearHarnessTaskTurnAccepted(ctx, task)
		return err
	}
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	task.Annotations[labels.AnnotationHarnessTurnStartedAt] = acceptedAt.Format(time.RFC3339Nano)
	delete(task.Annotations, labels.AnnotationHarnessTurnIdentityStartedAt)
	if task.Status.Phase == "" || task.Status.Phase == corev1alpha1.TaskPhasePending || task.Status.Phase == corev1alpha1.TaskPhaseScheduled {
		task.Status.Phase = corev1alpha1.TaskPhaseRunning
	}
	if task.Status.StartTime == nil {
		task.Status.StartTime = &metav1.Time{Time: acceptedAt}
	}
	meta.SetStatusCondition(&task.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeJobCreated,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Time{Time: acceptedAt},
		Reason:             "HarnessProviderStarted",
		Message:            "Harness provider turn accepted",
	})
	return nil
}

func (r *TaskReconciler) clearHarnessTaskTurnAccepted(ctx context.Context, task *corev1alpha1.Task) error {
	if task == nil {
		return nil
	}
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &corev1alpha1.Task{}
		if err := r.Get(ctx, types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, latest); err != nil {
			return err
		}
		if latest.Annotations == nil || strings.TrimSpace(latest.Annotations[labels.AnnotationHarnessTurnStartedAt]) == "" {
			return nil
		}
		patch := client.MergeFrom(latest.DeepCopy())
		delete(latest.Annotations, labels.AnnotationHarnessTurnStartedAt)
		return r.Patch(ctx, latest, patch)
	})
}

func harnessTaskHasControllerIdentity(task *corev1alpha1.Task) bool {
	if task == nil {
		return false
	}
	condition := meta.FindStatusCondition(task.Status.Conditions, ConditionTypeJobCreated)
	if condition == nil || condition.Status != metav1.ConditionTrue {
		return false
	}
	switch condition.Reason {
	case "HarnessProviderStarting", "HarnessProviderStarted":
		return true
	default:
		return false
	}
}

func harnessTaskHasControllerTurnAnnotations(task *corev1alpha1.Task) bool {
	if task == nil || task.Annotations == nil {
		return false
	}
	return strings.TrimSpace(task.Annotations[labels.AnnotationHarnessTurn]) != "" &&
		strings.TrimSpace(task.Annotations[labels.AnnotationHarnessRuntimeSession]) != "" &&
		strings.TrimSpace(task.Annotations[labels.AnnotationHarnessCorrelation]) != ""
}

func harnessTaskHasControllerTurnIdentity(task *corev1alpha1.Task) bool {
	if task == nil || task.Annotations == nil || !harnessTaskHasControllerIdentity(task) {
		return false
	}
	return strings.TrimSpace(task.Annotations[labels.AnnotationHarnessTurn]) != "" &&
		strings.TrimSpace(task.Annotations[labels.AnnotationHarnessRuntimeSession]) != "" &&
		strings.TrimSpace(task.Annotations[labels.AnnotationHarnessCorrelation]) != ""
}

func harnessTaskHasControllerStarted(task *corev1alpha1.Task) bool {
	if task == nil || task.Annotations == nil {
		return false
	}
	if !harnessTaskHasControllerIdentity(task) {
		return false
	}
	return strings.TrimSpace(task.Annotations[labels.AnnotationHarnessTurn]) != "" &&
		strings.TrimSpace(task.Annotations[labels.AnnotationHarnessRuntimeSession]) != "" &&
		strings.TrimSpace(task.Annotations[labels.AnnotationHarnessCorrelation]) != "" &&
		strings.TrimSpace(task.Annotations[labels.AnnotationHarnessTurnStartedAt]) != ""
}

func (r *TaskReconciler) handlePendingHarnessTaskInProgress(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) {
	startedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(task.Annotations[labels.AnnotationHarnessTurnStartedAt]))
	if err != nil {
		return r.failTask(ctx, task, "harness task has invalid in-progress timestamp")
	}
	deadline := startedAt.Add(harnessTurnTimeout(task)).Add(time.Minute)
	if time.Now().UTC().After(deadline) {
		return r.resumeHarnessTask(ctx, task)
	}
	return r.resumeHarnessTask(ctx, task)
}

func (r *TaskReconciler) failTimedOutHarnessProviderTurn(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) {
	if cancelErr := r.cancelHarnessProviderTurn(ctx, task, harnessTurnTimeoutMessage); cancelErr != nil {
		log.FromContext(ctx).Error(cancelErr, "failed to cancel timed-out harness provider turn", "task", task.Name)
	}
	if err := r.markAnnotatedHarnessRuntimeSessionUnhealthy(ctx, task, harnessTurnTimeoutMessage); err != nil {
		return ctrl.Result{}, err
	}
	return r.failTask(ctx, task, harnessTurnTimeoutMessage)
}

func harnessTerminalNeedsClient(completed *harness.TurnCompleted) bool {
	return completed != nil && strings.TrimSpace(completed.OutputRef) != "" && (completed.ResultTruncated || strings.TrimSpace(completed.Result) == "")
}

func (r *TaskReconciler) harnessClientForTask(ctx context.Context, task *corev1alpha1.Task, eventSessionName string) (*harness.Client, error) {
	endpoint := taskHarnessEndpoint(task)
	if err := r.validateHarnessServiceEndpoint(ctx, task, endpoint); err != nil {
		return nil, err
	}
	provider := harness.KubernetesServiceProvider{EndpointURL: endpoint, AllowInsecureLoopback: r.HarnessEndpointAllowInsecureLoopback}
	runtimeSessionName := harnessRuntimeOwnerSessionName(task, eventSessionName)
	providerEndpoint, err := provider.Endpoint(ctx, harness.RuntimeSessionOwner{
		Namespace: task.Namespace, SessionName: runtimeSessionName, ActiveTask: task.Name, AgentName: taskAgentName(task), Provider: harness.ProviderKindKubernetesService,
	})
	if err != nil {
		return nil, err
	}
	return newHarnessProviderClient(providerEndpoint)
}

func (r *TaskReconciler) rejectUnsupportedServiceHarnessBrokeredTools(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, bool, error) {
	if task == nil || task.Annotations == nil || strings.TrimSpace(task.Annotations[labels.AnnotationHarnessBrokeredTools]) == "" {
		return ctrl.Result{}, false, nil
	}
	if taskHarnessEndpoint(task) == "" && task.Spec.Type != corev1alpha1.TaskTypeAgent {
		return ctrl.Result{}, false, nil
	}
	message := "service harness brokered tool execution requires trusted provider identity and is not supported yet"
	if err := r.markAnnotatedHarnessRuntimeSessionUnhealthy(ctx, task, message); err != nil {
		return ctrl.Result{}, true, err
	}
	result, err := r.failTask(ctx, task, message)
	return result, true, err
}

func (r *TaskReconciler) handleAnnotatedHarnessTerminalIfRecovered(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, bool, error) {
	if !harnessTaskHasControllerTurnAnnotations(task) {
		return ctrl.Result{}, false, nil
	}
	runtimeSessionID := harness.RuntimeSessionID(strings.TrimSpace(task.Annotations[labels.AnnotationHarnessRuntimeSession]))
	turnID := harness.HarnessTurnID(strings.TrimSpace(task.Annotations[labels.AnnotationHarnessTurn]))
	_, completed, failed, cancelled, err := r.latestHarnessFrameState(ctx, task, runtimeSessionID, turnID)
	if err != nil {
		return ctrl.Result{}, false, err
	}
	if completed == nil && failed == nil && !cancelled {
		return ctrl.Result{}, false, nil
	}
	terminalClient, handledResult, handled, clientErr := r.harnessClientForRecoveredTerminal(ctx, task, completed, failed)
	if handled || clientErr != nil {
		return handledResult, true, clientErr
	}
	result, handleErr := r.handleRecoveredHarnessTerminal(ctx, task, terminalClient, turnID, completed, failed, cancelled)
	return result, true, handleErr
}

func (r *TaskReconciler) recordHarnessTaskStartedIfMissing(ctx context.Context, task *corev1alpha1.Task) error {
	if r == nil || r.ExecutionEventStore == nil || task == nil {
		return nil
	}
	listed, err := r.ExecutionEventStore.ListExecutionEvents(ctx, store.ExecutionEventFilter{
		Namespace:  task.Namespace,
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   task.Name,
		EventTypes: []string{events.ExecutionEventTypeTaskStarted},
		Limit:      1,
	})
	if err != nil {
		return err
	}
	if len(listed) > 0 {
		return nil
	}
	return r.recordTaskLifecycleEvent(ctx, task, events.ExecutionEventTypeTaskStarted, events.ExecutionEventSeverityInfo, "Task started with harness provider")
}

func (r *TaskReconciler) harnessClientForRecoveredTerminal(
	ctx context.Context,
	task *corev1alpha1.Task,
	completed *harness.TurnCompleted,
	failed *harness.TurnFailed,
) (*harness.Client, ctrl.Result, bool, error) {
	if failed != nil {
		if failed.Retryable && r.shouldRetry(task) {
			return nil, ctrl.Result{}, false, nil
		}
		if strings.TrimSpace(failed.OutputRef) != "" {
			return r.harnessClientForRecoveredTerminalOutput(ctx, task)
		}
	}
	if harnessRecoveredCompletionOutputAlreadySaved(ctx, r.ResultStore, task, completed) {
		return nil, ctrl.Result{}, false, nil
	}
	if !harnessTerminalNeedsClient(completed) {
		return nil, ctrl.Result{}, false, nil
	}
	return r.harnessClientForRecoveredTerminalOutput(ctx, task)
}

func harnessRecoveredCompletionOutputAlreadySaved(
	ctx context.Context,
	resultStore store.ResultStore,
	task *corev1alpha1.Task,
	completed *harness.TurnCompleted,
) bool {
	if resultStore == nil || task == nil || completed == nil || strings.TrimSpace(completed.OutputRef) == "" {
		return false
	}
	existing, err := resultStore.GetResult(ctx, task.Namespace, task.Name)
	return err == nil && len(existing) > 0
}

func (r *TaskReconciler) harnessClientForRecoveredTerminalOutput(ctx context.Context, task *corev1alpha1.Task) (*harness.Client, ctrl.Result, bool, error) {
	harnessClient, err := r.harnessClientForTask(ctx, task, taskSessionName(task))
	if err != nil {
		message := "harness terminal output client could not be created"
		if markErr := r.markAnnotatedHarnessRuntimeSessionUnhealthy(ctx, task, message); markErr != nil {
			return nil, ctrl.Result{}, true, markErr
		}
		result, failErr := r.failTask(ctx, task, fmt.Sprintf("%s: %v", message, err))
		return nil, result, true, failErr
	}
	return harnessClient, ctrl.Result{}, false, nil
}

func (r *TaskReconciler) handleRecoveredHarnessTerminal(
	ctx context.Context,
	task *corev1alpha1.Task,
	terminalClient *harness.Client,
	turnID harness.HarnessTurnID,
	completed *harness.TurnCompleted,
	failed *harness.TurnFailed,
	cancelled bool,
) (ctrl.Result, error) {
	if err := r.recordHarnessTaskStartedIfMissing(ctx, task); err != nil {
		return ctrl.Result{}, err
	}
	return r.handleHarnessTurnRunResult(ctx, task, terminalClient, turnID, harness.TurnRunResult{
		Completed: completed,
		Failed:    failed,
		Cancelled: cancelled,
	}, nil)
}

func (r *TaskReconciler) resumeHarnessTask(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) {
	if r.ExecutionEventStore == nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	runtimeSessionID := harness.RuntimeSessionID(strings.TrimSpace(task.Annotations[labels.AnnotationHarnessRuntimeSession]))
	turnID := harness.HarnessTurnID(strings.TrimSpace(task.Annotations[labels.AnnotationHarnessTurn]))
	correlationID := strings.TrimSpace(task.Annotations[labels.AnnotationHarnessCorrelation])
	if runtimeSessionID == "" || turnID == "" || correlationID == "" {
		return r.failTask(ctx, task, "harness task is missing persisted turn identity")
	}
	lastFrameSeq, completed, failed, cancelled, err := r.latestHarnessFrameState(ctx, task, runtimeSessionID, turnID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if completed != nil || failed != nil || cancelled {
		terminalClient, handledResult, handled, clientErr := r.harnessClientForRecoveredTerminal(ctx, task, completed, failed)
		if handled || clientErr != nil {
			return handledResult, clientErr
		}
		return r.handleRecoveredHarnessTerminal(ctx, task, terminalClient, turnID, completed, failed, cancelled)
	}
	endpoint := taskHarnessEndpoint(task)
	if err := r.validateHarnessServiceEndpoint(ctx, task, endpoint); err != nil {
		if releaseErr := r.markAnnotatedHarnessRuntimeSessionUnhealthy(ctx, task, "invalid harness provider service during resume"); releaseErr != nil {
			return ctrl.Result{}, releaseErr
		}
		return r.failTask(ctx, task, fmt.Sprintf("invalid harness provider service: %v", err))
	}
	if err := r.recordHarnessTaskStartedIfMissing(ctx, task); err != nil {
		return ctrl.Result{}, err
	}
	provider := harness.KubernetesServiceProvider{
		EndpointURL:           endpoint,
		AllowInsecureLoopback: r.HarnessEndpointAllowInsecureLoopback,
	}
	eventSessionName := taskSessionName(task)
	runtimeSessionName := harnessRuntimeOwnerSessionName(task, eventSessionName)
	providerEndpoint, err := provider.Endpoint(ctx, harness.RuntimeSessionOwner{
		Namespace: task.Namespace, SessionName: runtimeSessionName, ActiveTask: task.Name, AgentName: taskAgentName(task), Provider: harness.ProviderKindKubernetesService,
	})
	if err != nil {
		return r.failTask(ctx, task, fmt.Sprintf("invalid harness provider: %v", err))
	}
	harnessClient, err := newHarnessProviderClient(providerEndpoint)
	if err != nil {
		return r.failTask(ctx, task, fmt.Sprintf("invalid harness client: %v", err))
	}
	resumeCtx, cancel, err := harnessResumeContext(ctx, task)
	if err != nil {
		if strings.Contains(err.Error(), harnessTurnTimeoutMessage) {
			return r.failTimedOutHarnessProviderTurn(ctx, task)
		}
		return r.failTask(ctx, task, err.Error())
	}
	defer cancel()
	err = harnessClient.StreamFrames(resumeCtx, turnID, lastFrameSeq, func(frame harness.HarnessEventFrame) error {
		if frame.RuntimeSessionID != runtimeSessionID || frame.TurnID != turnID || frame.CorrelationID != correlationID {
			return fmt.Errorf("harness frame identity does not match persisted task turn")
		}
		if frame.Seq <= lastFrameSeq {
			return fmt.Errorf("non-monotonic harness frame seq %d after %d", frame.Seq, lastFrameSeq)
		}
		lastFrameSeq = frame.Seq
		mapped, err := harness.MapFrameToExecutionEvent(frame, harness.EventMapContext{
			Namespace: task.Namespace, TaskName: task.Name, SessionName: eventSessionName, AgentName: taskAgentName(task),
		})
		if err != nil {
			return err
		}
		if _, err := r.ExecutionEventStore.AppendExecutionEvent(resumeCtx, mapped); err != nil {
			return err
		}
		switch frame.Type {
		case harness.FrameTurnCompleted:
			completed = frame.Completed
		case harness.FrameTurnFailed:
			failed = frame.Failed
		case harness.FrameTurnCancelled:
			cancelled = true
		}
		return nil
	})
	if err != nil {
		return r.handleDirectHarnessResumeError(ctx, resumeCtx, task, err)
	}
	if harnessResumeMissingTerminal(completed, failed, cancelled) {
		message := "harness turn ended without terminal frame"
		if markErr := r.markAnnotatedHarnessRuntimeSessionUnhealthy(ctx, task, message); markErr != nil {
			return ctrl.Result{}, markErr
		}
		return r.failTask(ctx, task, message)
	}
	return r.handleHarnessTurnRunResult(ctx, task, harnessClient, turnID, harness.TurnRunResult{
		Completed: completed,
		Failed:    failed,
		Cancelled: cancelled,
	}, nil)
}

func (r *TaskReconciler) handleDirectHarnessResumeError(ctx context.Context, classifyCtx context.Context, task *corev1alpha1.Task, err error) (ctrl.Result, error) {
	if isHarnessNotFound(err) {
		message := "harness turn disappeared during resume"
		if markErr := r.markAnnotatedHarnessRuntimeSessionUnhealthy(ctx, task, message); markErr != nil {
			return ctrl.Result{}, markErr
		}
		return r.failTask(ctx, task, message)
	}
	if isDeterministicHarnessResumeError(classifyCtx, err) {
		if markErr := r.markAnnotatedHarnessRuntimeSessionUnhealthy(ctx, task, err.Error()); markErr != nil {
			return ctrl.Result{}, markErr
		}
		return r.failTask(ctx, task, fmt.Sprintf("harness resume failed: %s", events.RedactExecutionEventText(err.Error())))
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func harnessResumeContext(ctx context.Context, task *corev1alpha1.Task) (context.Context, context.CancelFunc, error) {
	startedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(task.Annotations[labels.AnnotationHarnessTurnStartedAt]))
	if err != nil {
		return ctx, func() {}, fmt.Errorf("harness task has invalid in-progress timestamp")
	}
	deadline := startedAt.Add(harnessTurnTimeout(task)).Add(time.Minute)
	if time.Now().UTC().After(deadline) {
		return ctx, func() {}, fmt.Errorf("harness turn did not complete before timeout")
	}
	resumeCtx, cancel := context.WithDeadline(ctx, deadline)
	return resumeCtx, cancel, nil
}

func (r *TaskReconciler) completeHarnessTaskFromTerminal(
	ctx context.Context,
	task *corev1alpha1.Task,
	completed *harness.TurnCompleted,
	failed *harness.TurnFailed,
	cancelled bool,
) (ctrl.Result, error) {
	if err := r.finalizeHarnessRuntimeSession(ctx, task, completed, failed, cancelled); err != nil {
		return ctrl.Result{}, err
	}
	if failed != nil {
		if failed.Retryable && r.shouldRetry(task) {
			if err := r.clearHarnessTaskTurnState(ctx, task); err != nil {
				return ctrl.Result{}, err
			}
			return r.retryTask(ctx, task)
		}
		return r.failTask(ctx, task, fmt.Sprintf("harness turn failed: %s", events.RedactExecutionEventText(failed.Reason)))
	}
	if completed != nil {
		if strings.TrimSpace(completed.Result) != "" && strings.TrimSpace(completed.OutputRef) == "" && !completed.ResultTruncated && r.ResultStore != nil {
			if err := r.ResultStore.SaveResult(ctx, task.Namespace, task.Name, []byte(completed.Result)); err != nil {
				return ctrl.Result{}, err
			}
		}
		return r.completeTask(ctx, task, corev1alpha1.TaskPhaseSucceeded, "harness turn completed")
	}
	if cancelled {
		return r.completeTask(ctx, task, corev1alpha1.TaskPhaseCancelled, "harness turn cancelled")
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *TaskReconciler) markAnnotatedHarnessRuntimeSessionUnhealthy(
	ctx context.Context,
	task *corev1alpha1.Task,
	reason string,
) error {
	if r == nil || r.RuntimeSessionStore == nil || task == nil || task.Annotations == nil {
		return nil
	}
	runtimeID := harness.RuntimeSessionID(strings.TrimSpace(task.Annotations[labels.AnnotationHarnessRuntimeSession]))
	if runtimeID == "" {
		return nil
	}
	current, err := r.RuntimeSessionStore.GetRuntimeSession(ctx, task.Namespace, runtimeID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if !runtimeSessionCanBeMarkedUnhealthyForTask(current, task.Name) {
		return nil
	}
	updated, err := r.RuntimeSessionStore.MarkRuntimeSessionFailed(
		ctx,
		task.Namespace,
		runtimeID,
		harness.RuntimeSessionStateUnhealthy,
		safeRuntimeSessionStatusReason(reason),
		time.Now().UTC(),
	)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	r.recordHarnessRuntimeSessionEvent(ctx, task, updated, "harness runtime session marked unhealthy")
	return nil
}

func runtimeSessionCanBeMarkedUnhealthyForTask(session *harness.RuntimeSession, taskName string) bool {
	if session == nil {
		return false
	}
	activeTask := strings.TrimSpace(session.Owner.ActiveTask)
	if activeTask != "" && activeTask != strings.TrimSpace(taskName) {
		return false
	}
	switch session.State {
	case harness.RuntimeSessionStatePending,
		harness.RuntimeSessionStateBooting,
		harness.RuntimeSessionStateReady,
		harness.RuntimeSessionStateTurnRunning,
		harness.RuntimeSessionStateIdle,
		harness.RuntimeSessionStateUnhealthy:
		return true
	default:
		return false
	}
}

func (r *TaskReconciler) markClaimedHarnessRuntimeSessionUnhealthy(
	ctx context.Context,
	task *corev1alpha1.Task,
	session *harness.RuntimeSession,
	reason string,
) error {
	if r == nil || r.RuntimeSessionStore == nil || task == nil || session == nil {
		return nil
	}
	updated, err := r.RuntimeSessionStore.MarkRuntimeSessionFailed(
		ctx,
		task.Namespace,
		session.ID,
		harness.RuntimeSessionStateUnhealthy,
		safeRuntimeSessionStatusReason(reason),
		time.Now().UTC(),
	)
	if err != nil {
		return err
	}
	r.recordHarnessRuntimeSessionEvent(ctx, task, updated, "harness runtime session marked unhealthy")
	return nil
}

func safeRuntimeSessionStatusReason(reason string) string {
	redacted, _, _ := events.RedactAndTruncateExecutionEventText(reason, events.MaxExecutionEventSummaryChars)
	return redacted
}

func (r *TaskReconciler) claimHarnessRuntimeSession(ctx context.Context, task *corev1alpha1.Task, sessionName string) (*harness.RuntimeSession, error) {
	if r == nil || r.RuntimeSessionStore == nil {
		return nil, nil
	}
	claimed, err := r.RuntimeSessionStore.ClaimRuntimeSession(
		ctx,
		task.Namespace,
		sessionName,
		harness.ProviderKindKubernetesService,
		taskAgentName(task),
		task.Name,
		harnessReusePolicy(task) == harness.RuntimeCleanupPolicyRetain,
		time.Now().UTC(),
	)
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

func (r *TaskReconciler) finalizeHarnessRuntimeSession(
	ctx context.Context,
	task *corev1alpha1.Task,
	completed *harness.TurnCompleted,
	failed *harness.TurnFailed,
	cancelled bool,
) error {
	if r == nil || r.RuntimeSessionStore == nil || task == nil || task.Annotations == nil {
		return nil
	}
	runtimeID := harness.RuntimeSessionID(strings.TrimSpace(task.Annotations[labels.AnnotationHarnessRuntimeSession]))
	if runtimeID == "" {
		return nil
	}
	now := time.Now().UTC()
	var session *harness.RuntimeSession
	var err error
	switch {
	case failed != nil:
		session, err = r.RuntimeSessionStore.MarkRuntimeSessionFailed(ctx, task.Namespace, runtimeID, harness.RuntimeSessionStateFailed, safeRuntimeSessionStatusReason(failed.Reason), now)
	case cancelled:
		session, err = r.RuntimeSessionStore.ReleaseRuntimeSession(ctx, task.Namespace, runtimeID, harness.RuntimeCleanupPolicyDelete, now)
	case completed != nil && harnessReusePolicy(task) == harness.RuntimeCleanupPolicyRetain:
		session, err = r.RuntimeSessionStore.ReleaseRuntimeSession(ctx, task.Namespace, runtimeID, harness.RuntimeCleanupPolicyRetain, now)
	case completed != nil:
		session, err = r.RuntimeSessionStore.ReleaseRuntimeSession(ctx, task.Namespace, runtimeID, harness.RuntimeCleanupPolicyDelete, now)
	}
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}
	if session != nil {
		r.recordHarnessRuntimeSessionEvent(ctx, task, session, "harness runtime session released")
	}
	return nil
}

func (r *TaskReconciler) clearHarnessTaskTurnState(ctx context.Context, task *corev1alpha1.Task) error {
	if task == nil {
		return nil
	}
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		latest := &corev1alpha1.Task{}
		if err := r.Get(ctx, types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, latest); err != nil {
			return err
		}
		patch := client.MergeFrom(latest.DeepCopy())
		delete(latest.Annotations, labels.AnnotationHarnessRuntimeSession)
		delete(latest.Annotations, labels.AnnotationHarnessTurn)
		delete(latest.Annotations, labels.AnnotationHarnessCorrelation)
		delete(latest.Annotations, labels.AnnotationHarnessTurnStartedAt)
		return r.Patch(ctx, latest, patch)
	}); err != nil {
		return err
	}
	for _, key := range []string{
		labels.AnnotationHarnessRuntimeSession,
		labels.AnnotationHarnessTurn,
		labels.AnnotationHarnessCorrelation,
		labels.AnnotationHarnessTurnStartedAt,
		labels.AnnotationHarnessTurnIdentityStartedAt,
		labels.AnnotationHarnessEndpointPinned,
	} {
		delete(task.Annotations, key)
	}
	return nil
}

func harnessReusePolicy(task *corev1alpha1.Task) harness.RuntimeCleanupPolicy {
	if task == nil || task.Annotations == nil {
		return ""
	}
	if harnessTaskHasControllerTurnIdentity(task) {
		if pinned, ok := task.Annotations[labels.AnnotationHarnessReusePolicyPinned]; ok {
			return normalizeHarnessReusePolicy(pinned)
		}
	}
	return normalizeHarnessReusePolicy(task.Annotations[labels.AnnotationHarnessReusePolicy])
}

func normalizeHarnessReusePolicy(value string) harness.RuntimeCleanupPolicy {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case string(harness.RuntimeCleanupPolicyRetain), "reuse", "resident":
		return harness.RuntimeCleanupPolicyRetain
	default:
		return ""
	}
}

func (r *TaskReconciler) recordHarnessRuntimeSessionEvent(
	ctx context.Context,
	task *corev1alpha1.Task,
	session *harness.RuntimeSession,
	summary string,
) {
	if r == nil || r.ExecutionEventStore == nil || task == nil || session == nil {
		return
	}
	content, _ := json.Marshal(map[string]any{
		"runtimeSessionID": session.ID,
		"runtimeState":     session.State,
		"cleanupPolicy":    session.CleanupPolicy,
		"provider":         session.Owner.Provider,
	})
	if _, err := r.ExecutionEventStore.AppendExecutionEvent(ctx, &store.ExecutionEvent{
		Namespace:   task.Namespace,
		StreamType:  store.ExecutionEventStreamTypeTask,
		StreamID:    task.Name,
		TaskName:    task.Name,
		SessionName: taskSessionName(task),
		Type:        events.ExecutionEventTypeAgentRuntimeMetadata,
		Severity:    events.ExecutionEventSeverityInfo,
		Summary:     summary,
		Content:     content,
	}); err != nil {
		log.FromContext(ctx).Error(err, "failed to record harness runtime session event", "task", task.Name)
	}
}

func (r *TaskReconciler) latestHarnessFrameState(
	ctx context.Context,
	task *corev1alpha1.Task,
	runtimeSessionID harness.RuntimeSessionID,
	turnID harness.HarnessTurnID,
) (int64, *harness.TurnCompleted, *harness.TurnFailed, bool, error) {
	var latest int64
	var completed *harness.TurnCompleted
	var failed *harness.TurnFailed
	var cancelled bool
	var after int64
	for {
		listed, err := r.ExecutionEventStore.ListExecutionEvents(ctx, store.ExecutionEventFilter{
			Namespace: task.Namespace, StreamID: task.Name, AfterSeq: after, Limit: store.MaxExecutionEventLimit,
		})
		if err != nil {
			return 0, nil, nil, false, err
		}
		if len(listed) == 0 {
			break
		}
		for _, event := range listed {
			seq, terminalCompleted, terminalFailed, terminalCancelled := harnessFrameStateFromEvent(event, runtimeSessionID, turnID)
			if seq > latest {
				latest = seq
			}
			if terminalCompleted != nil {
				completed = terminalCompleted
			}
			if terminalFailed != nil {
				failed = terminalFailed
			}
			if terminalCancelled {
				cancelled = true
			}
			after = event.Seq
		}
		if len(listed) < store.MaxExecutionEventLimit {
			break
		}
	}
	return latest, completed, failed, cancelled, nil
}

func harnessResumeMissingTerminal(completed *harness.TurnCompleted, failed *harness.TurnFailed, cancelled bool) bool {
	return completed == nil && failed == nil && !cancelled
}

func harnessFrameStateFromEvent(
	event store.ExecutionEvent,
	runtimeSessionID harness.RuntimeSessionID,
	turnID harness.HarnessTurnID,
) (int64, *harness.TurnCompleted, *harness.TurnFailed, bool) {
	var content struct {
		Harness struct {
			RuntimeSessionID string `json:"runtimeSessionID"`
			TurnID           string `json:"turnID"`
			Seq              int64  `json:"seq"`
		} `json:"harness"`
		Completed *harness.TurnCompleted `json:"completed,omitempty"`
		Failed    *harness.TurnFailed    `json:"failed,omitempty"`
	}
	if len(event.Content) == 0 || json.Unmarshal(event.Content, &content) != nil {
		return 0, nil, nil, false
	}
	if content.Harness.RuntimeSessionID != string(runtimeSessionID) || content.Harness.TurnID != string(turnID) {
		return 0, nil, nil, false
	}
	return content.Harness.Seq, content.Completed, content.Failed, event.Type == events.ExecutionEventTypeAgentRuntimeCancelled
}

func isDeterministicHarnessResumeError(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	message := strings.ToLower(err.Error())
	var clientErr harness.ClientError
	if errors.As(err, &clientErr) {
		if clientErr.StatusCode != 0 {
			return false
		}
		message = strings.ToLower(clientErr.Message)
	}
	for _, marker := range []string{
		"harness frame identity",
		"decode harness frame: invalid",
		"decode harness frame: json",
		"invalid harness frame",
		"map frame",
		"mapped harness event",
		"non-monotonic",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func isLoopbackHarnessHost(host string) bool {
	normalized := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if normalized == "localhost" || strings.HasSuffix(normalized, ".localhost") {
		return true
	}
	if ip := net.ParseIP(normalized); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

func harnessCapabilitiesSupportToolMode(capabilities *harness.CapabilitiesResponse, mode harness.ToolExecutionMode) bool {
	if mode == "" {
		mode = harness.ToolExecutionModeObserved
	}
	if capabilities == nil {
		return mode == harness.ToolExecutionModeObserved
	}
	if slices.Contains(capabilities.ToolExecutionModes, mode) {
		return true
	}
	return mode == harness.ToolExecutionModeObserved && len(capabilities.ToolExecutionModes) == 0
}

func taskAgentName(task *corev1alpha1.Task) string {
	if task == nil || task.Spec.AgentRef == nil {
		return ""
	}
	return strings.TrimSpace(task.Spec.AgentRef.Name)
}

func harnessToolExecutionMode(task *corev1alpha1.Task) harness.ToolExecutionMode {
	if task != nil && task.Annotations != nil && strings.TrimSpace(task.Annotations[labels.AnnotationHarnessBrokeredTools]) != "" {
		return harness.ToolExecutionModeBrokered
	}
	return harness.ToolExecutionModeObserved
}

func taskPrompt(task *corev1alpha1.Task) string {
	if task == nil {
		return ""
	}
	if strings.TrimSpace(task.Spec.Prompt) != "" {
		return strings.TrimSpace(task.Spec.Prompt)
	}
	if task.Spec.AI != nil {
		return strings.TrimSpace(task.Spec.AI.Prompt)
	}
	return ""
}

func harnessDeadline(task *corev1alpha1.Task) time.Time {
	return time.Now().UTC().Add(harnessTurnTimeout(task))
}

func harnessTurnTimeout(task *corev1alpha1.Task) time.Duration {
	if task != nil && task.Spec.Timeout != nil && task.Spec.Timeout.Duration > 0 {
		return task.Spec.Timeout.Duration
	}
	return defaultHarnessTurnTimeout
}

func harnessProtocolSessionName(task *corev1alpha1.Task, eventSessionName string) string {
	if strings.TrimSpace(eventSessionName) != "" {
		return strings.TrimSpace(eventSessionName)
	}
	if task == nil {
		return ""
	}
	return strings.TrimSpace(task.Name)
}

func harnessRuntimeOwnerSessionName(task *corev1alpha1.Task, eventSessionName string) string {
	if strings.TrimSpace(eventSessionName) != "" {
		return strings.TrimSpace(eventSessionName)
	}
	if task == nil {
		return ""
	}
	name := strings.TrimSpace(task.Name)
	prefixed := "task/" + name
	if task.Annotations != nil && harnessTaskHasControllerTurnIdentity(task) {
		runtimeID := strings.TrimSpace(task.Annotations[labels.AnnotationHarnessRuntimeSession])
		if runtimeID != "" {
			if strings.HasPrefix(runtimeID, harnessRuntimeOwnerIDPrefix(prefixed)+"-") {
				return prefixed
			}
			// Existing in-flight no-SessionRef tasks from before the task/ prefix were
			// claimed with the protocol fallback key (task.Name). Preserve that key only
			// for controller-owned persisted turn identities.
			return name
		}
	}
	return prefixed
}

func harnessRuntimeOwnerIDPrefix(sessionName string) string {
	prefix := strings.NewReplacer("/", "-", "_", "-", ":", "-").Replace(strings.ToLower(strings.TrimSpace(sessionName)))
	prefix = strings.Trim(prefix, "-")
	if prefix == "" {
		prefix = "runtime"
	}
	if len(prefix) > 32 {
		prefix = strings.Trim(prefix[:32], "-")
	}
	return prefix
}

func harnessTurnIdentityContext(ctx context.Context, task *corev1alpha1.Task) (context.Context, context.CancelFunc) {
	if deadline, ok := harnessTurnIdentityDeadline(task); ok {
		return context.WithDeadline(ctx, deadline)
	}
	return context.WithTimeout(ctx, harnessTurnTimeout(task))
}

func harnessTurnIdentityDeadline(task *corev1alpha1.Task) (time.Time, bool) {
	if task == nil {
		return time.Time{}, false
	}
	startedAt := time.Time{}
	if task.Annotations != nil {
		if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(task.Annotations[labels.AnnotationHarnessTurnIdentityStartedAt])); err == nil {
			startedAt = parsed
		}
	}
	if startedAt.IsZero() && task.Status.StartTime != nil {
		startedAt = task.Status.StartTime.Time
	} else if startedAt.IsZero() {
		if condition := meta.FindStatusCondition(task.Status.Conditions, ConditionTypeJobCreated); condition != nil && condition.Status == metav1.ConditionTrue {
			startedAt = condition.LastTransitionTime.Time
		}
	}
	if startedAt.IsZero() && !task.CreationTimestamp.IsZero() {
		startedAt = task.CreationTimestamp.Time
	}
	if startedAt.IsZero() {
		return time.Time{}, false
	}
	return startedAt.Add(harnessTurnTimeout(task)).Add(time.Minute), true
}

func harnessTurnIdentityTimedOut(task *corev1alpha1.Task) bool {
	deadline, ok := harnessTurnIdentityDeadline(task)
	return ok && time.Now().UTC().After(deadline)
}

func harnessRuntimeSessionID(task *corev1alpha1.Task, sessionName string) string {
	if harnessTaskHasControllerIdentity(task) && task.Annotations != nil {
		if value := strings.TrimSpace(task.Annotations[labels.AnnotationHarnessRuntimeSession]); value != "" {
			return value
		}
	}
	return "runtime-" + shortHarnessDigest(task.Namespace, sessionName)
}

func harnessTurnID(task *corev1alpha1.Task) string {
	if harnessTaskHasControllerIdentity(task) && task.Annotations != nil {
		if value := strings.TrimSpace(task.Annotations[labels.AnnotationHarnessTurn]); value != "" {
			return value
		}
	}
	return "turn-" + shortHarnessDigest(task.Namespace, task.Name, string(task.UID), fmt.Sprintf("%d", task.Status.Attempts+1))
}

func harnessCorrelationID(task *corev1alpha1.Task) string {
	if harnessTaskHasControllerIdentity(task) && task.Annotations != nil {
		if value := strings.TrimSpace(task.Annotations[labels.AnnotationHarnessCorrelation]); value != "" {
			return value
		}
	}
	return "corr-" + shortHarnessDigest(task.Namespace, task.Name, string(task.UID))
}

func shortHarnessDigest(values ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return hex.EncodeToString(sum[:])[:16]
}
