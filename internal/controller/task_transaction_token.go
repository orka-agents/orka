/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/contexttoken"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/tokenexchange"
	"github.com/orka-agents/orka/internal/transactiontoken"
)

const (
	taskTransactionTokenOwnerKind                = "Task"
	taskTokenRequestOperationKey                 = "operation"
	taskTokenRequestOperationCreateTask          = "createTask"
	taskTokenRequestOperationDelegateTask        = "delegateTask"
	taskTokenRequestOperationCreateContainerTask = "createContainerTask"
	taskTokenRequestNamespaceKey                 = "namespace"
	taskTokenRequestTaskNameKey                  = "taskName"
	taskTokenRequestTaskUIDKey                   = "taskUID"
	taskTokenRequestTransactionIDKey             = "txn"
	taskTokenRequestRotationKey                  = "rotation"
	taskTokenRequestParentTaskKey                = "parentTask"
	taskTokenRequestAgentKey                     = "agent"

	taskTokenExpiresAtSecretKey  = "token-expires-at"
	taskTokenRefreshAtSecretKey  = "token-refresh-at"
	taskTokenGenerationSecretKey = "token-generation"

	taskTokenMinimumRotationLifetime = 3 * time.Second
	taskTokenRefreshDivisor          = 3
	taskTokenTransientRetryDelay     = 5 * time.Second

	// tokenexchange deliberately replaces non-timeout transport details with
	// this fixed safe error so callers can react without exposing endpoints.
	taskTokenEndpointRequestFailed = "token endpoint request failed"
)

type taskTokenOptimisticRetryError struct {
	err error
}

func (e taskTokenOptimisticRetryError) Error() string { return e.err.Error() }
func (e taskTokenOptimisticRetryError) Unwrap() error { return e.err }

type taskTokenExchangeError struct {
	err error
}

func (e *taskTokenExchangeError) Error() string {
	return fmt.Sprintf("exchanging task-bound transaction token: %v", e.err)
}

func (e *taskTokenExchangeError) Unwrap() error { return e.err }

func taskTokenRetryable(err error) bool {
	var retry taskTokenOptimisticRetryError
	return errors.As(err, &retry)
}

func taskTokenTransientExchangeFailure(err error) bool {
	var exchangeFailure *taskTokenExchangeError
	if !errors.As(err, &exchangeFailure) || exchangeFailure.err == nil {
		return false
	}
	if errors.Is(exchangeFailure.err, context.DeadlineExceeded) {
		return true
	}
	var endpointFailure *tokenexchange.ExchangeError
	if errors.As(exchangeFailure.err, &endpointFailure) {
		if endpointFailure.StatusCode == http.StatusUnauthorized || endpointFailure.StatusCode == http.StatusForbidden {
			return false
		}
		if endpointFailure.StatusCode == http.StatusRequestTimeout ||
			endpointFailure.StatusCode == http.StatusTooManyRequests ||
			(endpointFailure.StatusCode >= http.StatusInternalServerError && endpointFailure.StatusCode < 600) {
			return true
		}
		return (endpointFailure.StatusCode == 0 || endpointFailure.StatusCode == http.StatusBadRequest) &&
			(endpointFailure.Code == "server_error" || endpointFailure.Code == "temporarily_unavailable")
	}
	var networkFailure net.Error
	if errors.As(exchangeFailure.err, &networkFailure) {
		return networkFailure.Timeout() || networkFailure.Temporary() //nolint:staticcheck // net.Error is the concrete transport retry signal here.
	}
	return exchangeFailure.err.Error() == taskTokenEndpointRequestFailed
}

func taskTokenTransientKubernetesFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) ||
		apierrors.IsTimeout(err) ||
		apierrors.IsServerTimeout(err) ||
		apierrors.IsTooManyRequests(err) ||
		apierrors.IsInternalError(err) ||
		apierrors.IsServiceUnavailable(err) ||
		apierrors.IsUnexpectedServerError(err) {
		return true
	}
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		code := int(status.Status().Code)
		if code >= http.StatusInternalServerError && code < 600 {
			return true
		}
	}
	var networkFailure net.Error
	if errors.As(err, &networkFailure) {
		if networkFailure.Timeout() || networkFailure.Temporary() { //nolint:staticcheck // net.Error is the concrete transport retry signal here.
			return true
		}
	}
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		if utilnet.IsProbableEOF(cause) {
			return true
		}
	}
	return utilnet.IsConnectionReset(err) || utilnet.IsConnectionRefused(err) || utilnet.IsHTTP2ConnectionLost(err)
}

func taskTokenTransientRetryAfter(now, expiresAt time.Time) time.Duration {
	remaining := expiresAt.Sub(now)
	if remaining <= 0 {
		return 0
	}
	if remaining <= time.Nanosecond {
		return time.Nanosecond
	}
	return max(min(taskTokenTransientRetryDelay, remaining/2), time.Nanosecond)
}

func (r *TaskReconciler) taskTokenReader() client.Reader {
	if r != nil && r.APIReader != nil {
		return r.APIReader
	}
	if r == nil {
		return nil
	}
	return r.Client
}

func (r *TaskReconciler) reconcilePendingTaskTransactionToken(
	ctx context.Context,
	task *corev1alpha1.Task,
	now time.Time,
) (ready bool, fatal bool, err error) {
	reconcileStartedAt := time.Now()
	currentTime := func() time.Time {
		return now.Add(time.Since(reconcileStartedAt))
	}
	pendingSince, pendingErr := transactionTokenPendingSince(task)
	if pendingErr != nil {
		return false, true, pendingErr
	}
	setupDeadline := pendingSince.Add(taskTransactionTokenPendingTimeout)
	remaining := setupDeadline.Sub(currentTime())
	if remaining <= 0 {
		return false, true, errors.New("task transaction token setup deadline elapsed before reading workload token")
	}
	setupCtx, cancelSetup := context.WithTimeout(ctx, remaining)
	defer cancelSetup()
	workloadSecret, wait, err := r.pendingTaskTransactionTokenSecret(setupCtx, task)
	observedNow := currentTime()
	if !observedNow.Before(setupDeadline) {
		return false, true, errors.New("task transaction token setup deadline elapsed while reading workload token")
	}
	if err != nil {
		if taskTokenTransientKubernetesFailure(err) {
			return false, false, nil
		}
		return false, true, err
	}
	if wait {
		return false, false, nil
	}
	taskToken := strings.TrimSpace(string(workloadSecret.Data[transactiontoken.TokenSecretKey]))
	directWorkload := directTaskTokenWorkloadSecret(task, workloadSecret)
	if !directWorkload && taskToken == "" {
		// Delegated child creation starts with an empty ownerless placeholder.
		// Wait only when it is exactly bound to this child's parent identity.
		if delegatedTokenPlaceholderForTask(task, workloadSecret) {
			return false, false, nil
		}
		return false, true, errors.New("delegated task transaction token placeholder identity is invalid")
	}
	if !taskOwnsTransactionTokenSecret(task, workloadSecret) {
		return false, true, errors.New("task transaction token Secret is not owned by the pending task")
	}
	if workloadSecret.Type != corev1.SecretTypeOpaque {
		return false, true, errors.New("task transaction token Secret has an invalid type")
	}
	if !directWorkload {
		if !currentTime().Before(setupDeadline) {
			return false, true, errors.New("task transaction token setup deadline elapsed before delegated token became ready")
		}
		return taskToken != "", false, nil
	}
	authoritySecret, retry, err := r.pendingTaskTransactionRenewalAuthority(
		setupCtx, task, setupDeadline, currentTime,
	)
	if err != nil {
		return false, true, err
	}
	if retry {
		return false, false, nil
	}
	if taskToken != "" {
		return r.reconcilePendingExistingTaskTransactionToken(
			setupCtx, task, authoritySecret, workloadSecret, taskToken, setupDeadline, currentTime,
		)
	}
	return r.reconcilePendingInitialTaskTransactionToken(
		setupCtx, task, authoritySecret, workloadSecret, setupDeadline, currentTime,
	)
}

func (r *TaskReconciler) pendingTaskTransactionRenewalAuthority(
	ctx context.Context,
	task *corev1alpha1.Task,
	setupDeadline time.Time,
	currentTime func() time.Time,
) (*corev1.Secret, bool, error) {
	authoritySecret, err := r.taskTransactionRenewalAuthoritySecret(ctx, task)
	observedNow := currentTime()
	if err != nil {
		if taskTokenTransientKubernetesFailure(err) && observedNow.Before(setupDeadline) {
			// The Task has no usable or refreshable workload token until the
			// controller-only authority is validated, so keep execution blocked
			// and retry within the bounded setup window.
			return nil, true, nil
		}
		if !observedNow.Before(setupDeadline) {
			return nil, false, errors.New("task transaction token setup deadline elapsed while reading renewal authority")
		}
		return nil, false, err
	}
	if !observedNow.Before(setupDeadline) {
		return nil, false, errors.New("task transaction token setup deadline elapsed while reading renewal authority")
	}
	return authoritySecret, false, nil
}

func (r *TaskReconciler) reconcilePendingExistingTaskTransactionToken(
	ctx context.Context,
	task *corev1alpha1.Task,
	authoritySecret, workloadSecret *corev1.Secret,
	taskToken string,
	setupDeadline time.Time,
	currentTime func() time.Time,
) (bool, bool, error) {
	expiresAt, refreshAt, err := renewableTaskTokenState(workloadSecret, taskToken)
	if err != nil {
		return false, true, err
	}
	observedNow := currentTime()
	if !observedNow.Before(setupDeadline) {
		return false, true, errors.New("task transaction token setup deadline elapsed before existing token became ready")
	}
	if !expiresAt.After(observedNow) {
		return false, true, errors.New("task-bound transaction token expired before setup completed")
	}
	if observedNow.Before(refreshAt) {
		return true, false, nil
	}
	_, err = r.rotateTaskTransactionToken(ctx, task, authoritySecret, workloadSecret, observedNow)
	rotationFinishedAt := currentTime()
	if !setupDeadline.IsZero() && !rotationFinishedAt.Before(setupDeadline) {
		return false, true, errors.New("task transaction token setup deadline elapsed during rotation")
	}
	if err == nil {
		return true, false, nil
	}
	if (taskTokenTransientExchangeFailure(err) || taskTokenTransientKubernetesFailure(err) || taskTokenRetryable(err)) &&
		expiresAt.After(rotationFinishedAt) &&
		(setupDeadline.IsZero() || rotationFinishedAt.Before(setupDeadline)) {
		return false, false, nil
	}
	return false, true, err
}

func (r *TaskReconciler) reconcilePendingInitialTaskTransactionToken(
	ctx context.Context,
	task *corev1alpha1.Task,
	authoritySecret, workloadSecret *corev1.Secret,
	setupDeadline time.Time,
	currentTime func() time.Time,
) (bool, bool, error) {
	if !currentTime().Before(setupDeadline) {
		return false, true, errors.New("task transaction token setup deadline elapsed before exchange")
	}
	_, err := r.rotateTaskTransactionToken(ctx, task, authoritySecret, workloadSecret, currentTime())
	rotationFinishedAt := currentTime()
	if !rotationFinishedAt.Before(setupDeadline) {
		return false, true, errors.New("task transaction token setup deadline elapsed during exchange")
	}
	if err == nil {
		return true, false, nil
	}
	if taskTokenTransientExchangeFailure(err) || taskTokenTransientKubernetesFailure(err) || taskTokenRetryable(err) {
		// The Task has no usable workload token yet, so keep execution blocked
		// and let the pending handler retry within its bounded setup window.
		return false, false, nil
	}
	return false, true, err
}

func (r *TaskReconciler) pendingTaskTransactionTokenSecret(
	ctx context.Context,
	task *corev1alpha1.Task,
) (*corev1.Secret, bool, error) {
	if r == nil || r.Client == nil || r.taskTokenReader() == nil || task == nil || task.Annotations == nil {
		return nil, true, nil
	}
	secretName := strings.TrimSpace(task.Annotations[labels.AnnotationTransactionTokenSecret])
	if secretName == "" {
		return nil, true, nil
	}
	secret := &corev1.Secret{}
	if err := r.taskTokenReader().Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: secretName}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("reading pending task transaction token Secret: %w", err)
	}
	return secret, false, nil
}

func directTaskTokenWorkloadSecret(task *corev1alpha1.Task, secret *corev1.Secret) bool {
	return task != nil && secret != nil && task.UID != "" &&
		secret.Labels[labels.LabelPurpose] == transactiontoken.WorkloadSecretPurpose &&
		secret.Labels[labels.LabelTaskUID] == labels.SelectorValue(string(task.UID))
}

func (r *TaskReconciler) taskTransactionRenewalAuthoritySecret(
	ctx context.Context,
	task *corev1alpha1.Task,
) (*corev1.Secret, error) {
	if r == nil || r.Client == nil || r.taskTokenReader() == nil || task == nil || task.UID == "" {
		return nil, errors.New("task transaction renewal authority cannot be resolved")
	}
	listed := &corev1.SecretList{}
	if err := r.taskTokenReader().List(ctx, listed,
		client.InNamespace(task.Namespace),
		client.MatchingLabels{
			labels.LabelPurpose: transactiontoken.AuthoritySecretPurpose,
			labels.LabelTaskUID: labels.SelectorValue(string(task.UID)),
		},
	); err != nil {
		return nil, fmt.Errorf("listing task transaction renewal authority: %w", err)
	}
	if len(listed.Items) != 1 {
		return nil, fmt.Errorf("task transaction renewal authority count is %d, want exactly one", len(listed.Items))
	}
	authority := listed.Items[0].DeepCopy()
	if !taskOwnsTransactionTokenSecret(task, authority) {
		return nil, errors.New("task transaction renewal authority is not owned by the task")
	}
	if authority.Type != corev1.SecretTypeOpaque ||
		authority.Labels[labels.LabelPurpose] != transactiontoken.AuthoritySecretPurpose ||
		authority.Labels[labels.LabelTaskUID] != labels.SelectorValue(string(task.UID)) {
		return nil, errors.New("task transaction renewal authority metadata is invalid")
	}
	if len(authority.Data) != 3 || strings.TrimSpace(string(authority.Data[transactiontoken.SubjectSecretKey])) == "" {
		return nil, errors.New("task transaction renewal authority data is invalid")
	}
	if err := transactiontoken.ValidateSubjectTokenType(string(authority.Data[transactiontoken.SubjectTokenTypeSecretKey])); err != nil {
		return nil, err
	}
	if _, err := taskTransactionTokenAuthorityRequestDetails(task, authority); err != nil {
		return nil, err
	}
	return authority, nil
}

func (r *TaskReconciler) rotateTaskTransactionToken(
	ctx context.Context,
	task *corev1alpha1.Task,
	authoritySecret *corev1.Secret,
	workloadSecret *corev1.Secret,
	now time.Time,
) (time.Time, error) {
	persistedSubjectToken := strings.TrimSpace(string(authoritySecret.Data[transactiontoken.SubjectSecretKey]))
	if persistedSubjectToken == "" {
		return time.Time{}, errors.New("task transaction renewal authority is empty")
	}
	if task.Spec.Transaction == nil || strings.TrimSpace(task.Spec.Transaction.Scope) == "" {
		return time.Time{}, errors.New("task is missing verified transaction scope metadata")
	}
	config := r.BrokeredTransactionExchange
	if config == nil || config.Exchanger == nil || !config.TTS.Enabled() {
		return time.Time{}, errors.New("task transaction token exchange is not configured")
	}
	subjectTokenType := string(authoritySecret.Data[transactiontoken.SubjectTokenTypeSecretKey])
	if err := transactiontoken.ValidateSubjectTokenType(subjectTokenType); err != nil {
		return time.Time{}, err
	}
	generation, err := taskTokenGeneration(workloadSecret)
	if err != nil {
		return time.Time{}, err
	}
	if generation == ^uint64(0) {
		return time.Time{}, errors.New("task transaction token generation is exhausted")
	}
	previousToken := strings.TrimSpace(string(workloadSecret.Data[transactiontoken.TokenSecretKey]))
	subjectToken := persistedSubjectToken
	if generation > 0 {
		if previousToken == "" {
			return time.Time{}, errors.New("task transaction token generation has no renewable subject")
		}
		// Chain each refresh from the current task-bound token. The original
		// caller token is necessarily expiring and is valid only for the first
		// exchange; reusing it would make healthy long-running Tasks fail once
		// the caller credential expires.
		subjectToken = previousToken
		subjectTokenType = transactiontoken.SubjectTokenTypeTransactionToken
	}
	nextGeneration := generation + 1
	requestDetails, err := taskTransactionTokenAuthorityRequestDetails(task, authoritySecret)
	if err != nil {
		return time.Time{}, err
	}
	requestDetails[taskTokenRequestRotationKey] = nextGeneration
	taskToken, err := config.Exchanger.Exchange(ctx, contexttoken.ExchangeRequest{
		SubjectToken:     subjectToken,
		SubjectTokenType: subjectTokenType,
		Scope:            strings.TrimSpace(task.Spec.Transaction.Scope),
		RequestedTTL:     config.TTS.ChildTokenTTL,
		RequestDetails:   requestDetails,
	})
	if err != nil {
		return time.Time{}, &taskTokenExchangeError{err: err}
	}
	taskToken = strings.TrimSpace(taskToken)
	if taskToken == "" || taskToken == subjectToken || taskToken == previousToken {
		return time.Time{}, errors.New("transaction token exchange did not return a distinct task-bound token")
	}
	expiresAt, err := unverifiedTaskTokenExpiry(taskToken)
	if err != nil {
		return time.Time{}, fmt.Errorf("reading task-bound transaction token expiry: %w", err)
	}
	remaining := expiresAt.Sub(now)
	minimumLifetime := taskTokenMinimumRotationLifetime
	if task.Spec.Type == corev1alpha1.TaskTypeAI || task.Spec.Type == corev1alpha1.TaskTypeContainer {
		minimumLifetime = transactiontoken.MinimumProjectedTokenRemainingLifetime
	}
	if remaining < minimumLifetime {
		return time.Time{}, errors.New("task-bound transaction token lifetime is too short for safe rotation")
	}
	refreshAt := now.Add(remaining / taskTokenRefreshDivisor)
	workloadSecret.Data = map[string][]byte{
		transactiontoken.TokenSecretKey: []byte(taskToken),
		taskTokenExpiresAtSecretKey:     []byte(expiresAt.UTC().Format(time.RFC3339Nano)),
		taskTokenRefreshAtSecretKey:     []byte(refreshAt.UTC().Format(time.RFC3339Nano)),
		taskTokenGenerationSecretKey:    []byte(strconv.FormatUint(nextGeneration, 10)),
	}
	return r.persistTaskTokenRotation(ctx, task, workloadSecret, generation, nextGeneration, refreshAt)
}

func taskTransactionTokenAuthorityRequestDetails(
	task *corev1alpha1.Task,
	authority *corev1.Secret,
) (map[string]any, error) {
	if task == nil || authority == nil {
		return nil, errors.New("task transaction token request details are unavailable")
	}
	raw := authority.Data[transactiontoken.RequestDetailsSecretKey]
	if len(raw) == 0 {
		return nil, errors.New("task transaction token request details are missing")
	}
	var details map[string]any
	if err := json.Unmarshal(raw, &details); err != nil || details == nil {
		return nil, errors.New("task transaction token request details are invalid")
	}
	requireString := func(key, expected string) bool {
		actual, ok := details[key].(string)
		return ok && actual == expected
	}
	if !requireString(taskTokenRequestNamespaceKey, task.Namespace) ||
		!requireString(taskTokenRequestTaskNameKey, task.Name) ||
		!requireString(taskTokenRequestTaskUIDKey, string(task.UID)) {
		return nil, errors.New("task transaction token request details identity is invalid")
	}
	expectedOperation := taskTokenRequestOperationCreateTask
	parentTask := labels.ParentTaskName(task.Labels, task.Annotations)
	if parentTask != "" {
		expectedOperation = taskTokenRequestOperationDelegateTask
		if task.Spec.Type == corev1alpha1.TaskTypeContainer {
			expectedOperation = taskTokenRequestOperationCreateContainerTask
		}
		if !requireString(taskTokenRequestParentTaskKey, parentTask) {
			return nil, errors.New("task transaction token request details parent identity is invalid")
		}
	}
	if !requireString(taskTokenRequestOperationKey, expectedOperation) {
		return nil, errors.New("task transaction token request details operation is invalid")
	}
	if task.Spec.Transaction != nil {
		if transactionID := strings.TrimSpace(task.Spec.Transaction.ID); transactionID != "" &&
			!requireString(taskTokenRequestTransactionIDKey, transactionID) {
			return nil, errors.New("task transaction token request details transaction is invalid")
		}
	}
	if parentTask != "" && task.Spec.AgentRef != nil {
		if agent := strings.TrimSpace(task.Spec.AgentRef.Name); agent != "" && !requireString(taskTokenRequestAgentKey, agent) {
			return nil, errors.New("task transaction token request details agent is invalid")
		}
	}
	delete(details, taskTokenRequestRotationKey)
	return details, nil
}

func taskTransactionTokenRequestDetails(task *corev1alpha1.Task, generation uint64) map[string]any {
	operation := taskTokenRequestOperationCreateTask
	details := map[string]any{
		taskTokenRequestNamespaceKey: task.Namespace,
		taskTokenRequestTaskNameKey:  task.Name,
		taskTokenRequestTaskUIDKey:   string(task.UID),
		taskTokenRequestRotationKey:  generation,
	}
	if parentTask := labels.ParentTaskName(task.Labels, task.Annotations); parentTask != "" {
		details[taskTokenRequestParentTaskKey] = parentTask
		if task.Spec.Type == corev1alpha1.TaskTypeContainer {
			operation = taskTokenRequestOperationCreateContainerTask
		} else {
			operation = taskTokenRequestOperationDelegateTask
		}
		if task.Spec.AgentRef != nil {
			if agent := strings.TrimSpace(task.Spec.AgentRef.Name); agent != "" {
				details[taskTokenRequestAgentKey] = agent
			}
		}
	}
	details[taskTokenRequestOperationKey] = operation
	if task.Spec.Transaction != nil {
		if transactionID := strings.TrimSpace(task.Spec.Transaction.ID); transactionID != "" {
			details[taskTokenRequestTransactionIDKey] = transactionID
		}
	}
	return details
}

func (r *TaskReconciler) persistTaskTokenRotation(
	ctx context.Context,
	task *corev1alpha1.Task,
	intended *corev1.Secret,
	previousGeneration, intendedGeneration uint64,
	intendedRefresh time.Time,
) (time.Time, error) {
	if err := r.Update(ctx, intended); err == nil {
		return intendedRefresh, nil
	} else if !apierrors.IsConflict(err) {
		if taskTokenTransientKubernetesFailure(err) {
			refreshAt, complete, readErr := r.confirmTaskTokenRotation(ctx, task, intended.Name, intendedGeneration)
			if readErr == nil && complete {
				return refreshAt, nil
			}
			if readErr != nil && !taskTokenTransientKubernetesFailure(readErr) {
				return time.Time{}, fmt.Errorf("confirming task-bound transaction token persistence: %w", readErr)
			}
		}
		return time.Time{}, fmt.Errorf("persisting task-bound transaction token: %w", err)
	}
	fresh, err := r.freshTaskTokenWorkloadSecret(ctx, task, intended.Name)
	if err != nil {
		return time.Time{}, taskTokenOptimisticRetryError{err: err}
	}
	if refreshAt, complete := completedTaskTokenRotation(fresh, intendedGeneration); complete {
		return refreshAt, nil
	}
	generation, err := taskTokenGeneration(fresh)
	if err != nil || generation != previousGeneration {
		return time.Time{}, taskTokenOptimisticRetryError{err: errors.New("task token rotation changed concurrently")}
	}
	fresh.Data = cloneSecretData(intended.Data)
	if err := r.Update(ctx, fresh); err == nil {
		return intendedRefresh, nil
	} else if !apierrors.IsConflict(err) {
		if taskTokenTransientKubernetesFailure(err) {
			refreshAt, complete, readErr := r.confirmTaskTokenRotation(ctx, task, intended.Name, intendedGeneration)
			if readErr == nil && complete {
				return refreshAt, nil
			}
			if readErr != nil && !taskTokenTransientKubernetesFailure(readErr) {
				return time.Time{}, fmt.Errorf("confirming retried task-bound transaction token persistence: %w", readErr)
			}
		}
		return time.Time{}, fmt.Errorf("retrying task-bound transaction token persistence: %w", err)
	}
	latest, readErr := r.freshTaskTokenWorkloadSecret(ctx, task, intended.Name)
	if readErr != nil {
		if taskTokenTransientKubernetesFailure(readErr) {
			return time.Time{}, taskTokenOptimisticRetryError{err: readErr}
		}
		return time.Time{}, fmt.Errorf("confirming task token rotation after repeated conflict: %w", readErr)
	}
	if refreshAt, complete := completedTaskTokenRotation(latest, intendedGeneration); complete {
		return refreshAt, nil
	}
	return time.Time{}, taskTokenOptimisticRetryError{err: errors.New("task token rotation conflict requires retry")}
}

func (r *TaskReconciler) confirmTaskTokenRotation(
	ctx context.Context,
	task *corev1alpha1.Task,
	name string,
	intendedGeneration uint64,
) (time.Time, bool, error) {
	fresh, err := r.freshTaskTokenWorkloadSecret(ctx, task, name)
	if err != nil {
		return time.Time{}, false, err
	}
	refreshAt, complete := completedTaskTokenRotation(fresh, intendedGeneration)
	return refreshAt, complete, nil
}

func (r *TaskReconciler) freshTaskTokenWorkloadSecret(
	ctx context.Context,
	task *corev1alpha1.Task,
	name string,
) (*corev1.Secret, error) {
	fresh := &corev1.Secret{}
	if err := r.taskTokenReader().Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, fresh); err != nil {
		return nil, fmt.Errorf("fresh-reading task transaction token Secret: %w", err)
	}
	if !taskOwnsTransactionTokenSecret(task, fresh) || fresh.Type != corev1.SecretTypeOpaque ||
		!directTaskTokenWorkloadSecret(task, fresh) {
		return nil, errors.New("fresh task transaction token Secret identity is invalid")
	}
	return fresh, nil
}

func completedTaskTokenRotation(secret *corev1.Secret, minimumGeneration uint64) (time.Time, bool) {
	generation, err := taskTokenGeneration(secret)
	if err != nil || generation < minimumGeneration {
		return time.Time{}, false
	}
	_, refreshAt, err := renewableTaskTokenState(secret, string(secret.Data[transactiontoken.TokenSecretKey]))
	return refreshAt, err == nil
}

func cloneSecretData(source map[string][]byte) map[string][]byte {
	cloned := make(map[string][]byte, len(source))
	for key, value := range source {
		cloned[key] = append([]byte(nil), value...)
	}
	return cloned
}

func (r *TaskReconciler) reconcileActiveTaskTransactionToken(
	ctx context.Context,
	task *corev1alpha1.Task,
	now time.Time,
) (refreshAfter time.Duration, fatal bool, err error) {
	reconcileStartedAt := time.Now()
	currentTime := func() time.Time {
		return now.Add(time.Since(reconcileStartedAt))
	}
	if task == nil || task.Spec.Transaction == nil || task.Annotations == nil ||
		(task.Spec.Type != corev1alpha1.TaskTypeAI && task.Spec.Type != corev1alpha1.TaskTypeAgent &&
			task.Spec.Type != corev1alpha1.TaskTypeContainer) {
		return 0, false, nil
	}
	secretName := strings.TrimSpace(task.Annotations[labels.AnnotationTransactionTokenSecret])
	if secretName == "" {
		return 0, false, nil
	}
	workloadSecret := &corev1.Secret{}
	if err := r.taskTokenReader().Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: secretName}, workloadSecret); err != nil {
		if apierrors.IsNotFound(err) {
			return 0, true, errors.New("task transaction token Secret is unavailable")
		}
		return 0, false, fmt.Errorf("reading active task transaction token Secret: %w", err)
	}
	if !taskOwnsTransactionTokenSecret(task, workloadSecret) {
		return 0, true, errors.New("active task transaction token Secret is not owned by the task")
	}
	if workloadSecret.Type != corev1.SecretTypeOpaque {
		return 0, true, errors.New("active task transaction token Secret has an invalid type")
	}
	if !directTaskTokenWorkloadSecret(task, workloadSecret) {
		return 0, false, nil
	}
	taskToken := strings.TrimSpace(string(workloadSecret.Data[transactiontoken.TokenSecretKey]))
	expiresAt, refreshAt, err := renewableTaskTokenState(workloadSecret, taskToken)
	if err != nil {
		return 0, true, err
	}
	observedNow := currentTime()
	if !expiresAt.After(observedNow) {
		return 0, true, errors.New("task-bound transaction token expired before rotation")
	}
	// Validate the controller-only authority on every active reconciliation,
	// even before refresh is due. Allowing task progress while that authority is
	// deleted, ambiguous, or unreadable would weaken the fail-closed boundary.
	authoritySecret, err := r.taskTransactionRenewalAuthoritySecret(ctx, task)
	observedNow = currentTime()
	if err != nil {
		if taskTokenTransientKubernetesFailure(err) {
			retryAfter := taskTokenTransientRetryAfter(observedNow, expiresAt)
			if retryAfter > 0 {
				return retryAfter, false, err
			}
		}
		return 0, true, err
	}
	if !expiresAt.After(observedNow) {
		return 0, true, errors.New("task-bound transaction token expired before rotation")
	}
	if observedNow.Before(refreshAt) {
		return refreshAt.Sub(observedNow), false, nil
	}
	nextRefresh, err := r.rotateTaskTransactionToken(ctx, task, authoritySecret, workloadSecret, observedNow)
	rotationFinishedAt := currentTime()
	if err != nil {
		if taskTokenTransientExchangeFailure(err) || taskTokenTransientKubernetesFailure(err) || taskTokenRetryable(err) {
			retryAfter := taskTokenTransientRetryAfter(rotationFinishedAt, expiresAt)
			if retryAfter > 0 {
				return retryAfter, false, err
			}
		}
		return 0, true, err
	}
	return max(nextRefresh.Sub(rotationFinishedAt), time.Nanosecond), false, nil
}

func renewableTaskTokenState(secret *corev1.Secret, taskToken string) (time.Time, time.Time, error) {
	if secret == nil || strings.TrimSpace(taskToken) == "" {
		return time.Time{}, time.Time{}, errors.New("renewable task transaction token is missing")
	}
	expiresAt, err := parseTaskTokenSecretTime(secret, taskTokenExpiresAtSecretKey)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	tokenExpiresAt, err := unverifiedTaskTokenExpiry(taskToken)
	if err != nil || !tokenExpiresAt.Equal(expiresAt) {
		return time.Time{}, time.Time{}, errors.New("task transaction token expiry metadata does not match the token")
	}
	refreshAt, err := parseTaskTokenSecretTime(secret, taskTokenRefreshAtSecretKey)
	if err != nil || !refreshAt.Before(expiresAt) {
		return time.Time{}, time.Time{}, errors.New("task transaction token refresh metadata is invalid")
	}
	generation, err := taskTokenGeneration(secret)
	if err != nil || generation == 0 {
		return time.Time{}, time.Time{}, errors.New("task transaction token generation is invalid")
	}
	return expiresAt, refreshAt, nil
}

func taskTokenGeneration(secret *corev1.Secret) (uint64, error) {
	if secret == nil || len(secret.Data[taskTokenGenerationSecretKey]) == 0 {
		return 0, nil
	}
	generation, err := strconv.ParseUint(strings.TrimSpace(string(secret.Data[taskTokenGenerationSecretKey])), 10, 64)
	if err != nil {
		return 0, errors.New("task transaction token generation is invalid")
	}
	return generation, nil
}

func parseTaskTokenSecretTime(secret *corev1.Secret, key string) (time.Time, error) {
	value := strings.TrimSpace(string(secret.Data[key]))
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.IsZero() {
		return time.Time{}, fmt.Errorf("task transaction token %s is invalid", key)
	}
	return parsed.UTC(), nil
}

func unverifiedTaskTokenExpiry(token string) (time.Time, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, errors.New("task transaction token is not a compact JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, errors.New("task transaction token payload is invalid")
	}
	var claims struct {
		Expiration json.Number `json:"exp"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.UseNumber()
	if decoder.Decode(&claims) != nil || claims.Expiration == "" {
		return time.Time{}, errors.New("task transaction token expiration is missing")
	}
	seconds, err := claims.Expiration.Int64()
	if err != nil || seconds <= 0 {
		return time.Time{}, errors.New("task transaction token expiration is invalid")
	}
	return time.Unix(seconds, 0).UTC(), nil
}

func (r *TaskReconciler) handleWithTaskTransactionTokenRefresh(
	ctx context.Context,
	task *corev1alpha1.Task,
	handler func(context.Context, *corev1alpha1.Task) (ctrl.Result, error),
) (ctrl.Result, error) {
	refreshAfter, fatal, refreshErr := r.reconcileActiveTaskTransactionToken(ctx, task, time.Now())
	if refreshErr != nil {
		if !fatal {
			if refreshAfter > 0 {
				logf.FromContext(ctx).Info("task-scoped transaction token refresh failed transiently; retrying before expiry", "retryAfter", refreshAfter)
				return ctrl.Result{RequeueAfter: refreshAfter}, nil
			}
			return ctrl.Result{}, refreshErr
		}
		logf.FromContext(ctx).Info("task-scoped transaction token refresh failed; failing closed")
		if cleanupErr := r.cleanupOwnedTaskTransactionTokenSecret(ctx, task); cleanupErr != nil {
			return ctrl.Result{}, cleanupErr
		}
		if r.Recorder != nil {
			r.Recorder.Event(task, corev1.EventTypeWarning, "TransactionTokenRefreshFailed", "task-scoped transaction token refresh failed")
		}
		return r.failTask(ctx, task, "task-scoped transaction token refresh failed")
	}
	result, err := handler(ctx, task)
	if err != nil || refreshAfter <= 0 {
		return result, err
	}
	if result.RequeueAfter <= 0 || refreshAfter < result.RequeueAfter {
		result.RequeueAfter = refreshAfter
	}
	return result, nil
}

func taskOwnsTransactionTokenSecret(task *corev1alpha1.Task, secret *corev1.Secret) bool {
	if task == nil || secret == nil || task.UID == "" {
		return false
	}
	for _, owner := range secret.OwnerReferences {
		if owner.APIVersion == corev1alpha1.GroupVersion.String() && owner.Kind == taskTransactionTokenOwnerKind &&
			owner.Name == task.Name && owner.UID == task.UID {
			return true
		}
	}
	return false
}

func (r *TaskReconciler) clearPendingTaskTransactionToken(ctx context.Context, task *corev1alpha1.Task) error {
	patch := client.MergeFrom(task.DeepCopy())
	delete(task.Annotations, labels.AnnotationTransactionTokenPending)
	delete(task.Annotations, labels.AnnotationTransactionTokenPendingSince)
	delete(task.Annotations, transactiontoken.ParentUIDAnnotation)
	delete(task.Annotations, transactiontoken.ParentNamespaceAnnotation)
	delete(task.Annotations, transactiontoken.PlaceholderUIDAnnotation)
	if err := r.Patch(ctx, task, patch); err != nil {
		return fmt.Errorf("clearing task transaction token pending state: %w", err)
	}
	return nil
}

func delegatedTokenPlaceholderForTask(task *corev1alpha1.Task, secret *corev1.Secret) bool {
	if task == nil || secret == nil || secret.Type != corev1.SecretTypeOpaque || len(secret.Data) != 0 ||
		len(secret.OwnerReferences) != 0 || task.Annotations == nil {
		return false
	}
	parentName := labels.ParentTaskName(task.Labels, task.Annotations)
	parentUID := strings.TrimSpace(task.Annotations[transactiontoken.ParentUIDAnnotation])
	parentNamespace := strings.TrimSpace(task.Annotations[transactiontoken.ParentNamespaceAnnotation])
	placeholderUID := strings.TrimSpace(task.Annotations[transactiontoken.PlaceholderUIDAnnotation])
	if parentName == "" || parentUID == "" || parentNamespace == "" || placeholderUID == "" ||
		string(secret.UID) != placeholderUID {
		return false
	}
	expectedLabels := map[string]string{
		labels.LabelPurpose:    transactiontoken.PlaceholderSecretPurpose,
		labels.LabelParentTask: labels.SelectorValue(parentName),
		labels.LabelTaskUID:    labels.SelectorValue(parentUID),
	}
	expectedAnnotations := map[string]string{
		labels.AnnotationParentTaskName:            parentName,
		transactiontoken.ParentUIDAnnotation:       parentUID,
		transactiontoken.ParentNamespaceAnnotation: parentNamespace,
	}
	return maps.Equal(secret.Labels, expectedLabels) && maps.Equal(secret.Annotations, expectedAnnotations)
}

func (r *TaskReconciler) deleteTaskTransactionTokenSecret(ctx context.Context, secret *corev1.Secret) error {
	if secret == nil {
		return nil
	}
	if secret.UID == "" {
		return errors.New("task transaction token Secret UID is unavailable")
	}
	uid := secret.UID
	return r.Delete(ctx, secret, client.Preconditions{UID: &uid})
}

func (r *TaskReconciler) cleanupOwnedTaskTransactionTokenSecret(ctx context.Context, task *corev1alpha1.Task) error {
	if r == nil || r.Client == nil || task == nil {
		return nil
	}
	var cleanupErrors []error
	if task.Annotations != nil {
		secretName := strings.TrimSpace(task.Annotations[labels.AnnotationTransactionTokenSecret])
		if secretName != "" {
			workloadSecret := &corev1.Secret{}
			if err := r.taskTokenReader().Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: secretName}, workloadSecret); err != nil {
				if !apierrors.IsNotFound(err) {
					cleanupErrors = append(cleanupErrors, fmt.Errorf("reading task transaction token Secret for cleanup: %w", err))
				}
			} else if taskOwnsTransactionTokenSecret(task, workloadSecret) || delegatedTokenPlaceholderForTask(task, workloadSecret) {
				if err := r.deleteTaskTransactionTokenSecret(ctx, workloadSecret); err != nil && !apierrors.IsNotFound(err) {
					cleanupErrors = append(cleanupErrors, fmt.Errorf("deleting task transaction token Secret: %w", err))
				}
			}
		}
	}
	if task.UID != "" {
		listed := &corev1.SecretList{}
		if err := r.taskTokenReader().List(ctx, listed,
			client.InNamespace(task.Namespace),
			client.MatchingLabels{
				labels.LabelPurpose: transactiontoken.AuthoritySecretPurpose,
				labels.LabelTaskUID: labels.SelectorValue(string(task.UID)),
			},
		); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("listing task transaction renewal authority for cleanup: %w", err))
		} else {
			for i := range listed.Items {
				authority := &listed.Items[i]
				if !taskOwnsTransactionTokenSecret(task, authority) {
					continue
				}
				if err := r.deleteTaskTransactionTokenSecret(ctx, authority); err != nil && !apierrors.IsNotFound(err) {
					cleanupErrors = append(cleanupErrors, fmt.Errorf("deleting task transaction renewal authority: %w", err))
				}
			}
		}
	}
	return errors.Join(cleanupErrors...)
}
