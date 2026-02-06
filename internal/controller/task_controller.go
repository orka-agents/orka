/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	cron "github.com/robfig/cron/v3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
)

const (
	// TaskFinalizer is the finalizer for Task cleanup
	TaskFinalizer = "mercan.ai/cleanup"

	// ConditionTypeComplete indicates the task has completed
	ConditionTypeComplete = "Complete"

	// ConditionTypeJobCreated indicates a Job has been created
	ConditionTypeJobCreated = "JobCreated"
)

// TaskReconciler reconciles a Task object
type TaskReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	JobBuilder      *JobBuilder
	SessionManager  *SessionManager
	WebhookNotifier *WebhookNotifier
	PriorityQueue   *PriorityQueue
	Recorder        record.EventRecorder
}

// +kubebuilder:rbac:groups=core.mercan.ai,resources=tasks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.mercan.ai,resources=tasks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.mercan.ai,resources=tasks/finalizers,verbs=update
// +kubebuilder:rbac:groups=core.mercan.ai,resources=agents,verbs=get;list;watch
// +kubebuilder:rbac:groups=core.mercan.ai,resources=tools,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile handles the reconciliation loop for Task resources
func (r *TaskReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Fetch the Task instance
	task := &corev1alpha1.Task{}
	if err := r.Get(ctx, req.NamespacedName, task); err != nil {
		if apierrors.IsNotFound(err) {
			// Task was deleted, nothing to do
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch Task")
		return ctrl.Result{}, err
	}

	// Handle deletion with finalizer
	if !task.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, task)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(task, TaskFinalizer) {
		controllerutil.AddFinalizer(task, TaskFinalizer)
		if err := r.Update(ctx, task); err != nil {
			log.Error(err, "failed to add finalizer")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Initialize status if empty
	if task.Status.Phase == "" {
		task.Status.Phase = corev1alpha1.TaskPhasePending
		if err := r.Status().Update(ctx, task); err != nil {
			log.Error(err, "failed to update initial status")
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Handle based on current phase
	switch task.Status.Phase {
	case corev1alpha1.TaskPhasePending:
		return r.handlePending(ctx, task)
	case corev1alpha1.TaskPhaseScheduled:
		return r.handleScheduled(ctx, task)
	case corev1alpha1.TaskPhaseRunning:
		return r.handleRunning(ctx, task)
	case corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed:
		return r.handleCompleted(ctx, task)
	}

	return ctrl.Result{}, nil
}

// handleDeletion handles Task cleanup when deleted
func (r *TaskReconciler) handleDeletion(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if controllerutil.ContainsFinalizer(task, TaskFinalizer) {
		// Clean up result ConfigMap
		if task.Status.ResultRef != nil {
			resultCM := &corev1.ConfigMap{}
			err := r.Get(ctx, types.NamespacedName{
				Name:      task.Status.ResultRef.ConfigMapName,
				Namespace: task.Namespace,
			}, resultCM)
			if err == nil {
				if err := r.Delete(ctx, resultCM); err != nil && !apierrors.IsNotFound(err) {
					log.Error(err, "failed to delete result ConfigMap")
					return ctrl.Result{}, err
				}
			}
		}

		// Release session lock if held
		if task.Spec.SessionRef != nil {
			if err := r.SessionManager.ReleaseLock(ctx, task); err != nil {
				log.Error(err, "failed to release session lock")
				// Continue with finalizer removal anyway
			}
		}

		// Clean up associated Job
		if task.Status.JobName != "" {
			job := &batchv1.Job{}
			err := r.Get(ctx, types.NamespacedName{
				Name:      task.Status.JobName,
				Namespace: task.Namespace,
			}, job)
			if err == nil {
				propagationPolicy := metav1.DeletePropagationBackground
				if err := r.Delete(ctx, job, &client.DeleteOptions{
					PropagationPolicy: &propagationPolicy,
				}); err != nil && !apierrors.IsNotFound(err) {
					log.Error(err, "failed to delete Job")
					return ctrl.Result{}, err
				}
			}
		}

		// Remove finalizer
		controllerutil.RemoveFinalizer(task, TaskFinalizer)
		if err := r.Update(ctx, task); err != nil {
			log.Error(err, "failed to remove finalizer")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// handlePending handles Tasks in Pending phase
func (r *TaskReconciler) handlePending(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// If this is a scheduled task, validate cron and transition to Scheduled phase
	if task.Spec.Schedule != "" {
		// Validate cron expression
		parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
		sched, err := parser.Parse(task.Spec.Schedule)
		if err != nil {
			task.Status.Phase = corev1alpha1.TaskPhaseFailed
			task.Status.Message = fmt.Sprintf("invalid cron expression: %v", err)
			if updateErr := r.Status().Update(ctx, task); updateErr != nil {
				return ctrl.Result{}, updateErr
			}
			return ctrl.Result{}, nil
		}

		// Calculate next schedule time
		now := time.Now()
		if task.Spec.TimeZone != nil {
			if loc, err := time.LoadLocation(*task.Spec.TimeZone); err == nil {
				now = now.In(loc)
			}
		}
		next := sched.Next(now)

		task.Status.Phase = corev1alpha1.TaskPhaseScheduled
		task.Status.NextScheduleTime = &metav1.Time{Time: next}
		task.Status.Message = fmt.Sprintf("Scheduled with cron: %s", task.Spec.Schedule)
		if err := r.Status().Update(ctx, task); err != nil {
			return ctrl.Result{}, err
		}

		log.Info("Task scheduled", "schedule", task.Spec.Schedule, "nextRun", next)
		return ctrl.Result{RequeueAfter: time.Until(next)}, nil
	}

	// Check session lock if session is referenced
	if task.Spec.SessionRef != nil {
		locked, err := r.SessionManager.IsLocked(ctx, task)
		if err != nil {
			log.Error(err, "failed to check session lock")
			return ctrl.Result{}, err
		}
		if locked {
			// Session is locked by another task, requeue
			log.Info("session is locked, waiting", "session", task.Spec.SessionRef.Name)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		// Acquire session lock
		if err := r.SessionManager.AcquireLock(ctx, task); err != nil {
			log.Error(err, "failed to acquire session lock")
			return ctrl.Result{}, err
		}
	}

	// Resolve agent if referenced
	var agent *corev1alpha1.Agent
	if task.Spec.AgentRef != nil {
		agent = &corev1alpha1.Agent{}
		agentNS := task.Spec.AgentRef.Namespace
		if agentNS == "" {
			agentNS = task.Namespace
		}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      task.Spec.AgentRef.Name,
			Namespace: agentNS,
		}, agent); err != nil {
			log.Error(err, "failed to get Agent", "agent", task.Spec.AgentRef.Name)
			return r.failTask(ctx, task, fmt.Sprintf("failed to get agent: %v", err))
		}
	}

	// Validate task-agent compatibility
	if err := r.validateTaskAgentCompatibility(task, agent); err != nil {
		log.Error(err, "task-agent compatibility validation failed")
		return r.failTask(ctx, task, err.Error())
	}

	// Resolve provider if referenced
	var provider *corev1alpha1.Provider
	providerRef := r.resolveProviderRef(task, agent)
	if providerRef != nil {
		provider = &corev1alpha1.Provider{}
		providerNS := providerRef.Namespace
		if providerNS == "" {
			providerNS = task.Namespace
		}
		if err := r.Get(ctx, types.NamespacedName{
			Name:      providerRef.Name,
			Namespace: providerNS,
		}, provider); err != nil {
			log.Error(err, "failed to get Provider", "provider", providerRef.Name)
			return r.failTask(ctx, task, fmt.Sprintf("failed to get provider: %v", err))
		}
		// Check provider is ready
		if !provider.Status.Ready {
			log.Info("provider is not ready", "provider", providerRef.Name)
			return r.failTask(ctx, task, fmt.Sprintf("provider %s is not ready: %s", providerRef.Name, provider.Status.Message))
		}
	}

	// Create the Job
	job, err := r.JobBuilder.Build(ctx, task, agent, provider)
	if err != nil {
		log.Error(err, "failed to build Job")
		return r.failTask(ctx, task, fmt.Sprintf("failed to build job: %v", err))
	}

	// Set owner reference
	if err := controllerutil.SetControllerReference(task, job, r.Scheme); err != nil {
		log.Error(err, "failed to set owner reference")
		return ctrl.Result{}, err
	}

	// Create the Job
	if err := r.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Job already exists, update status
			task.Status.JobName = job.Name
		} else {
			log.Error(err, "failed to create Job")
			return r.failTask(ctx, task, fmt.Sprintf("failed to create job: %v", err))
		}
	} else {
		task.Status.JobName = job.Name
	}

	// Update status to Running
	now := metav1.Now()
	task.Status.Phase = corev1alpha1.TaskPhaseRunning
	task.Status.StartTime = &now
	task.Status.Attempts++

	meta.SetStatusCondition(&task.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeJobCreated,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             "JobCreated",
		Message:            fmt.Sprintf("Job %s created", job.Name),
	})

	if err := r.Status().Update(ctx, task); err != nil {
		log.Error(err, "failed to update status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// handleRunning handles Tasks in Running phase
func (r *TaskReconciler) handleRunning(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Check timeout
	if task.Spec.Timeout != nil && task.Status.StartTime != nil {
		elapsed := time.Since(task.Status.StartTime.Time)
		if elapsed > task.Spec.Timeout.Duration {
			log.Info("task timed out", "elapsed", elapsed, "timeout", task.Spec.Timeout.Duration)
			return r.failTask(ctx, task, "task timed out")
		}
	}

	// Get the Job
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      task.Status.JobName,
		Namespace: task.Namespace,
	}, job); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Job not found, task may have been cleaned up")
			return r.failTask(ctx, task, "job not found")
		}
		log.Error(err, "failed to get Job")
		return ctrl.Result{}, err
	}

	// Check Job status
	if job.Status.Succeeded > 0 {
		// Job succeeded
		return r.completeTask(ctx, task, corev1alpha1.TaskPhaseSucceeded, "task completed successfully")
	}

	if job.Status.Failed > 0 {
		// Job failed, check retry policy
		if r.shouldRetry(task) {
			log.Info("retrying task", "attempt", task.Status.Attempts)
			return r.retryTask(ctx, task)
		}
		return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, "job failed")
	}

	// Job still running, requeue
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// handleCompleted handles Tasks that have completed (Succeeded or Failed)
func (r *TaskReconciler) handleCompleted(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Send webhook if configured and not already sent
	if task.Spec.WebhookURL != "" && !task.Status.WebhookDelivered {
		if err := r.WebhookNotifier.Notify(ctx, task); err != nil {
			log.Error(err, "failed to send webhook")
			// Don't fail the task, just retry webhook later
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		task.Status.WebhookDelivered = true
		if err := r.Status().Update(ctx, task); err != nil {
			log.Error(err, "failed to update webhook status")
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// completeTask marks a task as completed
func (r *TaskReconciler) completeTask(ctx context.Context, task *corev1alpha1.Task, phase corev1alpha1.TaskPhase, message string) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	now := metav1.Now()
	task.Status.Phase = phase
	task.Status.CompletionTime = &now
	task.Status.Message = message

	// Collect result from Job output
	if err := r.collectResult(ctx, task); err != nil {
		log.Error(err, "failed to collect result")
		// Continue anyway, result collection is best-effort
	}

	// Update session if configured
	if task.Spec.SessionRef != nil && task.Spec.SessionRef.Append {
		if err := r.SessionManager.AppendMessages(ctx, task); err != nil {
			log.Error(err, "failed to append session messages")
			// Continue anyway
		}
		// Release session lock
		if err := r.SessionManager.ReleaseLock(ctx, task); err != nil {
			log.Error(err, "failed to release session lock")
		}
	}

	conditionStatus := metav1.ConditionTrue
	reason := "TaskSucceeded"
	if phase == corev1alpha1.TaskPhaseFailed {
		conditionStatus = metav1.ConditionFalse
		reason = "TaskFailed"
	}

	meta.SetStatusCondition(&task.Status.Conditions, metav1.Condition{
		Type:               ConditionTypeComplete,
		Status:             conditionStatus,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
	})

	if err := r.Status().Update(ctx, task); err != nil {
		log.Error(err, "failed to update completion status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{Requeue: true}, nil
}

// failTask marks a task as failed
func (r *TaskReconciler) failTask(ctx context.Context, task *corev1alpha1.Task, message string) (ctrl.Result, error) {
	return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, message)
}

// shouldRetry checks if the task should be retried
func (r *TaskReconciler) shouldRetry(task *corev1alpha1.Task) bool {
	if task.Spec.RetryPolicy == nil {
		return false
	}
	return task.Status.Attempts < task.Spec.RetryPolicy.MaxRetries
}

// retryTask creates a new Job for a retry attempt
func (r *TaskReconciler) retryTask(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Delete the old Job
	if task.Status.JobName != "" {
		job := &batchv1.Job{}
		err := r.Get(ctx, types.NamespacedName{
			Name:      task.Status.JobName,
			Namespace: task.Namespace,
		}, job)
		if err == nil {
			propagationPolicy := metav1.DeletePropagationBackground
			if err := r.Delete(ctx, job, &client.DeleteOptions{
				PropagationPolicy: &propagationPolicy,
			}); err != nil && !apierrors.IsNotFound(err) {
				log.Error(err, "failed to delete old Job for retry")
			}
		}
	}

	// Calculate backoff delay
	delay := r.calculateRetryDelay(task)

	// Reset to pending for retry
	task.Status.Phase = corev1alpha1.TaskPhasePending
	task.Status.JobName = ""
	if err := r.Status().Update(ctx, task); err != nil {
		log.Error(err, "failed to update status for retry")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: delay}, nil
}

// calculateRetryDelay calculates the delay before retry using exponential backoff
func (r *TaskReconciler) calculateRetryDelay(task *corev1alpha1.Task) time.Duration {
	if task.Spec.RetryPolicy == nil || task.Spec.RetryPolicy.InitialDelay == nil {
		return 10 * time.Second // Default delay
	}

	initialDelay := task.Spec.RetryPolicy.InitialDelay.Duration
	multiplier := task.Spec.RetryPolicy.BackoffMultiplier
	if multiplier == 0 {
		multiplier = 2
	}

	// Calculate delay with exponential backoff
	delay := initialDelay
	for i := int32(1); i < task.Status.Attempts; i++ {
		delay = time.Duration(float64(delay) * multiplier)
	}

	// Cap at 5 minutes
	maxDelay := 5 * time.Minute
	if delay > maxDelay {
		delay = maxDelay
	}

	return delay
}

// collectResult collects the task result from the Job's output
func (r *TaskReconciler) collectResult(ctx context.Context, task *corev1alpha1.Task) error {
	// Result is stored in a ConfigMap by the worker
	resultCMName := fmt.Sprintf("%s-result", task.Name)
	resultCM := &corev1.ConfigMap{}

	err := r.Get(ctx, types.NamespacedName{
		Name:      resultCMName,
		Namespace: task.Namespace,
	}, resultCM)

	if err != nil {
		if apierrors.IsNotFound(err) {
			// No result ConfigMap yet, might be created by worker
			return nil
		}
		return err
	}

	task.Status.ResultRef = &corev1alpha1.ResultReference{
		ConfigMapName: resultCMName,
		Key:           "result",
	}

	return nil
}

// resolveProviderRef determines which provider reference to use
// Priority: Task.Spec.AI.ProviderRef > Agent.Spec.ProviderRef
func (r *TaskReconciler) resolveProviderRef(task *corev1alpha1.Task, agent *corev1alpha1.Agent) *corev1alpha1.ProviderReference {
	// Agent tasks don't use providers (CLI runtimes manage their own credentials)
	if task.Spec.Type == corev1alpha1.TaskTypeAgent {
		return nil
	}

	// Check task-level provider ref first
	if task.Spec.AI != nil && task.Spec.AI.ProviderRef != nil {
		return task.Spec.AI.ProviderRef
	}

	// Check agent-level provider ref
	if agent != nil && agent.Spec.ProviderRef != nil {
		return agent.Spec.ProviderRef
	}

	return nil
}

// validateTaskAgentCompatibility validates that the task type and agent configuration are compatible.
func (r *TaskReconciler) validateTaskAgentCompatibility(task *corev1alpha1.Task, agent *corev1alpha1.Agent) error {
	switch task.Spec.Type {
	case corev1alpha1.TaskTypeAgent:
		// Agent tasks require an agentRef
		if agent == nil {
			return fmt.Errorf("type: agent tasks require an agentRef")
		}
		// Agent must have runtime configured
		if agent.Spec.Runtime == nil {
			return fmt.Errorf("agent %q does not have a runtime configured (required for type: agent tasks)", agent.Name)
		}
		// Agent with runtime must not have providerRef (mutually exclusive)
		if agent.Spec.ProviderRef != nil {
			return fmt.Errorf("agent %q has both runtime and providerRef set (mutually exclusive)", agent.Name)
		}
		// Agent with runtime must not have a model provider set
		if agent.Spec.Model != nil && agent.Spec.Model.Provider != "" {
			return fmt.Errorf("agent %q has both runtime and model.provider set (mutually exclusive for agent tasks)", agent.Name)
		}
		// Prompt is required for agent tasks
		if task.Spec.Prompt == "" {
			return fmt.Errorf("prompt is required for type: agent tasks")
		}
	case corev1alpha1.TaskTypeAI:
		// AI tasks must not reference an agent with runtime set
		if agent != nil && agent.Spec.Runtime != nil {
			return fmt.Errorf("agent %q has runtime configured (use type: agent instead of type: ai)", agent.Name)
		}
	case corev1alpha1.TaskTypeContainer:
		// Container tasks don't use agents, no validation needed
	}
	return nil
}

// handleScheduled manages the scheduling loop for recurring tasks.
func (r *TaskReconciler) handleScheduled(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Check if suspended
	if task.Spec.Suspend != nil && *task.Spec.Suspend {
		log.Info("Task is suspended, skipping schedule check")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	// Parse schedule
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	sched, err := parser.Parse(task.Spec.Schedule)
	if err != nil {
		task.Status.Phase = corev1alpha1.TaskPhaseFailed
		task.Status.Message = fmt.Sprintf("invalid cron expression: %v", err)
		_ = r.Status().Update(ctx, task)
		return ctrl.Result{}, nil
	}

	// Determine time zone
	now := time.Now().UTC()
	loc := time.UTC
	if task.Spec.TimeZone != nil {
		if l, err := time.LoadLocation(*task.Spec.TimeZone); err == nil {
			loc = l
			now = now.In(loc)
		}
	}

	// Calculate the scheduled time for the next (or current) run
	var scheduledTime time.Time
	if task.Status.LastScheduleTime != nil {
		scheduledTime = sched.Next(task.Status.LastScheduleTime.Time.In(loc))
	} else {
		scheduledTime = sched.Next(task.CreationTimestamp.Time.In(loc))
	}

	// Not yet time
	if now.Before(scheduledTime) {
		nextSchedule := metav1.NewTime(scheduledTime)
		task.Status.NextScheduleTime = &nextSchedule
		_ = r.Status().Update(ctx, task)
		return ctrl.Result{RequeueAfter: time.Until(scheduledTime)}, nil
	}

	// Check starting deadline
	deadlineSeconds := int64(100) // default
	if task.Spec.StartingDeadlineSeconds != nil {
		deadlineSeconds = *task.Spec.StartingDeadlineSeconds
	}
	if now.Sub(scheduledTime) > time.Duration(deadlineSeconds)*time.Second {
		log.Info("Missed schedule beyond deadline, skipping", "scheduledTime", scheduledTime, "deadline", deadlineSeconds)
		r.Recorder.Eventf(task, "Warning", "MissedSchedule", "Missed scheduled run at %s (deadline %ds exceeded)", scheduledTime.Format(time.RFC3339), deadlineSeconds)
		// Advance to next schedule time
		next := sched.Next(now)
		nextSchedule := metav1.NewTime(next)
		task.Status.NextScheduleTime = &nextSchedule
		_ = r.Status().Update(ctx, task)
		return ctrl.Result{RequeueAfter: time.Until(next)}, nil
	}

	// Check concurrency policy
	if task.Spec.ConcurrencyPolicy == corev1alpha1.ForbidConcurrent || task.Spec.ConcurrencyPolicy == "" {
		var childList corev1alpha1.TaskList
		if err := r.List(ctx, &childList, client.InNamespace(task.Namespace), client.MatchingLabels{
			"mercan.ai/parent-task": task.Name,
		}); err != nil {
			return ctrl.Result{}, fmt.Errorf("listing child tasks: %w", err)
		}
		for i := range childList.Items {
			if childList.Items[i].Status.Phase == corev1alpha1.TaskPhasePending ||
				childList.Items[i].Status.Phase == corev1alpha1.TaskPhaseRunning {
				log.Info("Concurrency policy Forbid: active child task exists, skipping", "activeChild", childList.Items[i].Name)
				next := sched.Next(now)
				nextSchedule := metav1.NewTime(next)
				task.Status.NextScheduleTime = &nextSchedule
				_ = r.Status().Update(ctx, task)
				return ctrl.Result{RequeueAfter: time.Until(next)}, nil
			}
		}
	}

	// Create child task with deterministic name
	childName := fmt.Sprintf("%s-%d", task.Name, scheduledTime.Unix())
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      childName,
			Namespace: task.Namespace,
			Labels: map[string]string{
				"mercan.ai/parent-task":   task.Name,
				"mercan.ai/scheduled-run": "true",
			},
		},
		Spec: *task.Spec.DeepCopy(),
	}

	// Strip scheduling fields from child
	child.Spec.Schedule = ""
	child.Spec.TimeZone = nil
	child.Spec.ConcurrencyPolicy = ""
	child.Spec.StartingDeadlineSeconds = nil
	child.Spec.SuccessfulRunsHistoryLimit = nil
	child.Spec.FailedRunsHistoryLimit = nil
	child.Spec.Suspend = nil

	// Set owner reference
	if err := ctrl.SetControllerReference(task, child, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("setting owner reference: %w", err)
	}

	if err := r.Create(ctx, child); err != nil {
		if apierrors.IsAlreadyExists(err) {
			log.Info("Child task already exists (idempotent)", "child", childName)
		} else {
			return ctrl.Result{}, fmt.Errorf("creating child task: %w", err)
		}
	} else {
		log.Info("Created scheduled child task", "child", childName)
		r.Recorder.Eventf(task, "Normal", "ScheduledRun", "Created child task %s", childName)
	}

	// Update status
	nowTime := metav1.NewTime(scheduledTime)
	next := sched.Next(now)
	nextSchedule := metav1.NewTime(next)
	task.Status.LastScheduleTime = &nowTime
	task.Status.NextScheduleTime = &nextSchedule
	if err := r.Status().Update(ctx, task); err != nil {
		return ctrl.Result{}, err
	}

	// Enforce history limits
	if err := r.enforceHistoryLimits(ctx, task); err != nil {
		log.Error(err, "Failed to enforce history limits")
	}

	return ctrl.Result{RequeueAfter: time.Until(next)}, nil
}

// enforceHistoryLimits removes old child tasks beyond the configured limits.
func (r *TaskReconciler) enforceHistoryLimits(ctx context.Context, task *corev1alpha1.Task) error {
	var childList corev1alpha1.TaskList
	if err := r.List(ctx, &childList, client.InNamespace(task.Namespace), client.MatchingLabels{
		"mercan.ai/parent-task": task.Name,
	}); err != nil {
		return fmt.Errorf("listing child tasks: %w", err)
	}

	successLimit := int32(3)
	if task.Spec.SuccessfulRunsHistoryLimit != nil {
		successLimit = *task.Spec.SuccessfulRunsHistoryLimit
	}
	failedLimit := int32(1)
	if task.Spec.FailedRunsHistoryLimit != nil {
		failedLimit = *task.Spec.FailedRunsHistoryLimit
	}

	var succeeded, failed []*corev1alpha1.Task
	for i := range childList.Items {
		child := &childList.Items[i]
		switch child.Status.Phase {
		case corev1alpha1.TaskPhaseSucceeded:
			succeeded = append(succeeded, child)
		case corev1alpha1.TaskPhaseFailed:
			failed = append(failed, child)
		}
	}

	// Sort by creation time (oldest first) and delete excess
	sortByCreation := func(tasks []*corev1alpha1.Task) {
		for i := 0; i < len(tasks); i++ {
			for j := i + 1; j < len(tasks); j++ {
				if tasks[j].CreationTimestamp.Before(&tasks[i].CreationTimestamp) {
					tasks[i], tasks[j] = tasks[j], tasks[i]
				}
			}
		}
	}

	sortByCreation(succeeded)
	sortByCreation(failed)

	for i := 0; i < len(succeeded)-int(successLimit); i++ {
		if err := r.Delete(ctx, succeeded[i]); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting old succeeded child: %w", err)
		}
	}

	for i := 0; i < len(failed)-int(failedLimit); i++ {
		if err := r.Delete(ctx, failed[i]); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting old failed child: %w", err)
		}
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *TaskReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Recorder = mgr.GetEventRecorderFor("task-controller")
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Task{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1alpha1.Task{}).
		Named("task").
		Complete(r)
}
