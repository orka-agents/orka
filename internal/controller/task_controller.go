/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"time"

	cron "github.com/robfig/cron/v3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/tracing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	// ConditionTypeComplete indicates the task has completed
	ConditionTypeComplete = "Complete"

	// ConditionTypeJobCreated indicates a Job has been created
	ConditionTypeJobCreated = "JobCreated"

	// jobCreationVisibilityGracePeriod avoids failing a task when the controller cache
	// has not observed the Job immediately after create.
	jobCreationVisibilityGracePeriod = 30 * time.Second

	scheduledRunLabelValue = "true"
)

// TaskReconciler reconciles a Task object
type TaskReconciler struct {
	client.Client
	Scheme                    *runtime.Scheme
	JobBuilder                *JobBuilder
	SessionManager            *SessionManager
	WebhookNotifier           *WebhookNotifier
	Recorder                  record.EventRecorder
	KubeClient                kubernetes.Interface
	ResultStore               store.ResultStore
	PlanStore                 store.PlanStore
	MessageStore              store.MessageStore
	ArtifactStore             store.ArtifactStore
	EnforceNamespaceIsolation bool
	MaxTasksPerNamespace      int32
}

// +kubebuilder:rbac:groups=core.orka.ai,resources=tasks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.orka.ai,resources=tasks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=tasks/finalizers,verbs=update
// +kubebuilder:rbac:groups=core.orka.ai,resources=agents,verbs=get;list;watch
// +kubebuilder:rbac:groups=core.orka.ai,resources=tools,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups="",resources=pods/status,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=endpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=replicationcontrollers,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;replicasets;statefulsets;daemonsets,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings;roles;rolebindings,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=get;list
// +kubebuilder:rbac:groups=metrics.k8s.io,resources=pods;nodes,verbs=get;list

// updateStatusWithRetry updates the task status with retry on conflict.
// It re-fetches the task on conflict, applies the mutate function, and retries.
func (r *TaskReconciler) updateStatusWithRetry(ctx context.Context, task *corev1alpha1.Task, mutate func(*corev1alpha1.Task)) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		// On retry, re-fetch the latest version
		if err := r.Get(ctx, types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, task); err != nil {
			return err
		}
		mutate(task)
		return r.Status().Update(ctx, task)
	})
}

func childTaskStatusesEqual(a, b []corev1alpha1.ChildTaskStatus) bool {
	if len(a) != len(b) {
		return false
	}
	return slices.EqualFunc(a, b, func(left, right corev1alpha1.ChildTaskStatus) bool {
		return left.Name == right.Name &&
			left.Agent == right.Agent &&
			left.Phase == right.Phase &&
			left.Result == right.Result
	})
}

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

	tracer := tracing.Tracer("orka.controller")
	ctx, span := tracer.Start(ctx, "task.reconcile",
		trace.WithAttributes(
			attribute.String("task.name", task.Name),
			attribute.String("task.namespace", task.Namespace),
			attribute.String("task.type", string(task.Spec.Type)),
		),
	)
	defer span.End()

	// Handle deletion with finalizer
	if !task.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, task)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(task, labels.TaskFinalizer) {
		controllerutil.AddFinalizer(task, labels.TaskFinalizer)
		if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
			if err := r.Get(ctx, types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, task); err != nil {
				return err
			}
			controllerutil.AddFinalizer(task, labels.TaskFinalizer)
			return r.Update(ctx, task)
		}); err != nil {
			log.Error(err, "failed to add finalizer")
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	// Initialize status if empty
	if task.Status.Phase == "" {
		if err := r.updateStatusWithRetry(ctx, task, func(t *corev1alpha1.Task) {
			t.Status.Phase = corev1alpha1.TaskPhasePending
		}); err != nil {
			log.Error(err, "failed to update initial status")
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	// Handle based on current phase
	switch task.Status.Phase {
	case corev1alpha1.TaskPhasePending:
		return r.handlePending(ctx, task)
	case corev1alpha1.TaskPhaseScheduled:
		return r.handleScheduled(ctx, task)
	case corev1alpha1.TaskPhaseRunning:
		return r.handleRunning(ctx, task)
	case corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskPhaseCancelled:
		return r.handleCompleted(ctx, task)
	}

	return ctrl.Result{}, nil
}

// handleDeletion handles Task cleanup when deleted
func (r *TaskReconciler) handleDeletion(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) { //nolint:unparam // Result is always nil but kept for interface consistency
	log := logf.FromContext(ctx)

	if controllerutil.ContainsFinalizer(task, labels.TaskFinalizer) {
		// Clean up result data from store
		if task.Status.ResultRef != nil && task.Status.ResultRef.Available {
			if err := r.ResultStore.DeleteResult(ctx, task.Namespace, task.Name); err != nil {
				log.Error(err, "failed to delete result from store", "task", task.Name)
				// Continue with finalizer removal anyway
			}
		}

		// Clean up artifacts
		if r.ArtifactStore != nil {
			if err := r.ArtifactStore.DeleteArtifacts(ctx, task.Namespace, task.Name); err != nil {
				log.Error(err, "failed to delete artifacts", "task", task.Name)
			}
		}

		// Clean up plan state if any
		if r.PlanStore != nil {
			if err := r.PlanStore.DeletePlan(ctx, task.Namespace, task.Name); err != nil {
				log.Error(err, "failed to delete plan state", "task", task.Name)
				// Continue with finalizer removal anyway
			}
		}

		// Clean up inter-agent messages
		if r.MessageStore != nil {
			if err := r.MessageStore.DeleteTaskMessages(ctx, task.Namespace, task.Name); err != nil {
				log.Error(err, "failed to delete task messages", "task", task.Name)
			}
			// If this is a coordinator, clean up all children's messages
			if err := r.MessageStore.DeleteParentMessages(ctx, task.Namespace, task.Name); err != nil {
				log.Error(err, "failed to delete parent messages", "task", task.Name)
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
		controllerutil.RemoveFinalizer(task, labels.TaskFinalizer)
		if err := r.Update(ctx, task); err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}
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
		return r.handleScheduledTask(ctx, task)
	}

	// Check session lock if session is referenced
	if task.Spec.SessionRef != nil {
		if result, err, locked := r.acquireSessionLock(ctx, task); locked {
			return result, err
		}
	}

	// Enforce per-namespace task limit
	if r.MaxTasksPerNamespace > 0 {
		var namespaceTasks corev1alpha1.TaskList
		if err := r.List(ctx, &namespaceTasks, client.InNamespace(task.Namespace)); err != nil {
			log.Error(err, "failed to list namespace tasks for limit check")
			return ctrl.Result{}, err
		}
		active := int32(0)
		for _, t := range namespaceTasks.Items {
			if t.Name != task.Name && (t.Status.Phase == corev1alpha1.TaskPhasePending || t.Status.Phase == corev1alpha1.TaskPhaseRunning) {
				active++
			}
		}
		if active >= r.MaxTasksPerNamespace {
			log.Info("namespace task limit reached, requeueing",
				"namespace", task.Namespace,
				"active", active,
				"limit", r.MaxTasksPerNamespace,
			)
			r.Recorder.Eventf(task, corev1.EventTypeNormal, "NamespaceTaskLimitReached",
				"namespace %q has %d active tasks (limit: %d), requeueing", task.Namespace, active, r.MaxTasksPerNamespace)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
	}

	// Resolve agent if referenced
	agent, err := r.resolveAgent(ctx, task)
	if err != nil {
		log.Error(err, "failed to resolve agent")
		return r.failTask(ctx, task, err.Error())
	}

	// Validate task-agent compatibility
	if err := r.validateTaskAgentCompatibility(task, agent); err != nil {
		log.Error(err, "task-agent compatibility validation failed")
		return r.failTask(ctx, task, err.Error())
	}

	// Validate coordination constraints for child tasks
	if result, err, done := r.validateCoordinationConstraints(ctx, task); done {
		return result, err
	}

	// Resolve provider if referenced
	provider, err := r.resolveProvider(ctx, task, agent)
	if err != nil {
		log.Error(err, "failed to resolve provider")
		return r.failTask(ctx, task, err.Error())
	}

	return r.createTaskJob(ctx, task, agent, provider)
}

// handleScheduledTask handles transition to Scheduled phase for cron-scheduled tasks.
func (r *TaskReconciler) handleScheduledTask(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	sched, err := parser.Parse(task.Spec.Schedule)
	if err != nil {
		return r.failTask(ctx, task, fmt.Sprintf("invalid cron expression: %v", err))
	}

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

// acquireSessionLock checks and acquires a session lock. Returns (result, err, locked)
// where locked=true means the caller should return the result/err immediately.
func (r *TaskReconciler) acquireSessionLock(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error, bool) {
	log := logf.FromContext(ctx)

	locked, err := r.SessionManager.IsLocked(ctx, task)
	if err != nil {
		log.Error(err, "failed to check session lock")
		return ctrl.Result{}, err, true
	}
	if locked {
		log.Info("session is locked, waiting", "session", task.Spec.SessionRef.Name)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil, true
	}

	if err := r.SessionManager.AcquireLock(ctx, task); err != nil {
		log.Error(err, "failed to acquire session lock")
		return ctrl.Result{}, err, true
	}
	return ctrl.Result{}, nil, false
}

// resolveAgent fetches the Agent referenced by the task, if any.
func (r *TaskReconciler) resolveAgent(ctx context.Context, task *corev1alpha1.Task) (*corev1alpha1.Agent, error) {
	if task.Spec.AgentRef == nil {
		return nil, nil
	}
	agent := &corev1alpha1.Agent{}
	agentNS := task.Spec.AgentRef.Namespace
	if agentNS == "" {
		agentNS = task.Namespace
	}
	if r.EnforceNamespaceIsolation && agentNS != task.Namespace {
		r.Recorder.Eventf(task, corev1.EventTypeWarning, "NamespaceIsolationViolation",
			"cross-namespace agent reference not allowed: agent %q is in namespace %q", task.Spec.AgentRef.Name, agentNS)
		return nil, fmt.Errorf("cross-namespace agent reference not allowed when namespace isolation is enforced: agent %q in namespace %q, task in %q", task.Spec.AgentRef.Name, agentNS, task.Namespace)
	}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      task.Spec.AgentRef.Name,
		Namespace: agentNS,
	}, agent); err != nil {
		return nil, fmt.Errorf("failed to get agent: %v", err)
	}
	return agent, nil
}

// validateCoordinationConstraints validates depth, allowed agents, and concurrency for child tasks.
// Returns (result, err, done) where done=true means the caller should return the result/err.
func (r *TaskReconciler) validateCoordinationConstraints(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error, bool) {
	depthStr, ok := task.Annotations[labels.AnnotationCoordinationDepth]
	if !ok {
		return ctrl.Result{}, nil, false
	}

	log := logf.FromContext(ctx)
	parentName := labels.ParentTaskName(task.Labels, task.Annotations)
	depthInt, _ := strconv.Atoi(depthStr)

	// Look up parent task to find its agent's coordination config
	parentTask := &corev1alpha1.Task{}
	if err := r.Get(ctx, types.NamespacedName{Name: parentName, Namespace: task.Namespace}, parentTask); err != nil {
		log.Error(err, "failed to get parent task")
		result, err := r.failTask(ctx, task, fmt.Sprintf("failed to get parent task: %v", err))
		return result, err, true
	}

	parentAgent := &corev1alpha1.Agent{}
	if parentTask.Spec.AgentRef != nil {
		agentNS := parentTask.Spec.AgentRef.Namespace
		if agentNS == "" {
			agentNS = task.Namespace
		}
		if err := r.Get(ctx, types.NamespacedName{Name: parentTask.Spec.AgentRef.Name, Namespace: agentNS}, parentAgent); err != nil {
			log.Error(err, "failed to get parent agent")
			result, err := r.failTask(ctx, task, fmt.Sprintf("failed to get parent agent: %v", err))
			return result, err, true
		}
	}

	coord := parentAgent.Spec.Coordination
	if coord == nil || !coord.Enabled {
		result, err := r.failTask(ctx, task, "parent agent does not have coordination enabled")
		return result, err, true
	}

	// Enforce maxDepth
	if coord.MaxDepth > 0 && int32(depthInt) > coord.MaxDepth {
		result, err := r.failTask(ctx, task, fmt.Sprintf("coordination depth %d exceeds max %d", depthInt, coord.MaxDepth))
		return result, err, true
	}

	// Enforce allowedAgents
	if task.Spec.AgentRef != nil {
		allowed := false
		for _, a := range coord.AllowedAgents {
			if a.Name == task.Spec.AgentRef.Name {
				allowed = true
				break
			}
		}
		// Allow agents dynamically created by the parent task via create_agent tool
		if !allowed {
			childAgent := &corev1alpha1.Agent{}
			agentNS := task.Spec.AgentRef.Namespace
			if agentNS == "" {
				agentNS = task.Namespace
			}
			if err := r.Get(ctx, types.NamespacedName{Name: task.Spec.AgentRef.Name, Namespace: agentNS}, childAgent); err == nil {
				if childAgent.Labels[labels.LabelCreatedBy] == "create_agent" && labels.ParentTaskName(childAgent.Labels, childAgent.Annotations) == parentName {
					allowed = true
				}
			}
		}
		if !allowed {
			result, err := r.failTask(ctx, task, fmt.Sprintf("agent %q not in parent's allowedAgents", task.Spec.AgentRef.Name))
			return result, err, true
		}
	}

	// Enforce maxConcurrentChildren (requeue if at limit)
	if coord.MaxConcurrentChildren > 0 {
		var siblings corev1alpha1.TaskList
		if err := r.List(ctx, &siblings, client.InNamespace(task.Namespace),
			client.MatchingLabels{labels.LabelParentTask: labels.SelectorValue(parentName)}); err != nil {
			log.Error(err, "failed to list sibling tasks")
			return ctrl.Result{}, err, true
		}
		active := int32(0)
		for _, s := range siblings.Items {
			if s.Name != task.Name && (s.Status.Phase == corev1alpha1.TaskPhasePending || s.Status.Phase == corev1alpha1.TaskPhaseRunning) {
				active++
			}
		}
		if active >= coord.MaxConcurrentChildren {
			log.Info("coordination concurrency limit reached", "active", active, "max", coord.MaxConcurrentChildren)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil, true
		}
	}

	return ctrl.Result{}, nil, false
}

// resolveProvider fetches the Provider referenced by the task or agent, if any.
func (r *TaskReconciler) resolveProvider(ctx context.Context, task *corev1alpha1.Task, agent *corev1alpha1.Agent) (*corev1alpha1.Provider, error) {
	providerRef := r.resolveProviderRef(task, agent)
	if providerRef == nil {
		return nil, nil
	}
	provider := &corev1alpha1.Provider{}
	providerNS := providerRef.Namespace
	if providerNS == "" {
		providerNS = task.Namespace
	}
	if r.EnforceNamespaceIsolation && providerNS != task.Namespace {
		r.Recorder.Eventf(task, corev1.EventTypeWarning, "NamespaceIsolationViolation",
			"cross-namespace provider reference not allowed: provider %q is in namespace %q", providerRef.Name, providerNS)
		return nil, fmt.Errorf("cross-namespace provider reference not allowed when namespace isolation is enforced: provider %q in namespace %q, task in %q", providerRef.Name, providerNS, task.Namespace)
	}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      providerRef.Name,
		Namespace: providerNS,
	}, provider); err != nil {
		return nil, fmt.Errorf("failed to get provider: %v", err)
	}
	if !provider.Status.Ready {
		return nil, fmt.Errorf("provider %s is not ready: %s", providerRef.Name, provider.Status.Message)
	}
	return provider, nil
}

// createTaskJob builds the Job, sets owner reference, creates it, and updates the task status.
func (r *TaskReconciler) createTaskJob(ctx context.Context, task *corev1alpha1.Task, agent *corev1alpha1.Agent, provider *corev1alpha1.Provider) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Ensure worker ServiceAccount and RBAC exist in the task namespace
	if err := r.ensureWorkerRBAC(ctx, task.Namespace); err != nil {
		log.Error(err, "failed to ensure worker RBAC")
		// Non-fatal: continue with job creation, it may still work
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

	if s := trace.SpanFromContext(ctx); s.IsRecording() {
		s.AddEvent("phase.transition", trace.WithAttributes(
			attribute.String("task.phase", string(corev1alpha1.TaskPhaseRunning)),
		))
	}

	attempts := task.Status.Attempts
	jobName := task.Status.JobName
	if err := r.updateStatusWithRetry(ctx, task, func(t *corev1alpha1.Task) {
		t.Status.Phase = corev1alpha1.TaskPhaseRunning
		t.Status.StartTime = &now
		t.Status.Attempts = attempts
		t.Status.JobName = jobName
		meta.SetStatusCondition(&t.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeJobCreated,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: now,
			Reason:             "JobCreated",
			Message:            fmt.Sprintf("Job %s created", job.Name),
		})
	}); err != nil {
		log.Error(err, "failed to update status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// handleRunning handles Tasks in Running phase
func (r *TaskReconciler) handleRunning(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) { //nolint:gocyclo
	log := logf.FromContext(ctx)

	// Check timeout
	if task.Spec.Timeout != nil && task.Status.StartTime != nil {
		elapsed := time.Since(task.Status.StartTime.Time)
		if elapsed > task.Spec.Timeout.Duration {
			log.Info("task timed out", "elapsed", elapsed, "timeout", task.Spec.Timeout.Duration)
			return r.failTask(ctx, task, "task timed out")
		}
	}

	// Populate ChildTaskStatus for coordinator tasks
	if _, isChild := task.Labels[labels.LabelParentTask]; !isChild {
		var children corev1alpha1.TaskList
		if err := r.List(ctx, &children, client.InNamespace(task.Namespace),
			client.MatchingLabels{labels.LabelParentTask: labels.SelectorValue(task.Name)}); err == nil {
			slices.SortFunc(children.Items, func(a, b corev1alpha1.Task) int {
				switch {
				case a.Name < b.Name:
					return -1
				case a.Name > b.Name:
					return 1
				default:
					return 0
				}
			})

			childStatuses := make([]corev1alpha1.ChildTaskStatus, 0, len(children.Items))
			for _, child := range children.Items {
				phase := child.Status.Phase
				if phase == "" {
					phase = corev1alpha1.TaskPhasePending
				}
				cs := corev1alpha1.ChildTaskStatus{
					Name:  child.Name,
					Phase: phase,
				}
				if child.Spec.AgentRef != nil {
					cs.Agent = child.Spec.AgentRef.Name
				}
				if child.Status.ResultRef != nil && child.Status.ResultRef.Available {
					result, err := r.ResultStore.GetResult(ctx, child.Namespace, child.Name)
					if err != nil {
						log.Error(err, "failed to get child task result", "child", child.Name)
						cs.Result = "(result fetch error)"
					} else {
						cs.Result = string(result)
						if len(cs.Result) > 4096 {
							cs.Result = cs.Result[:4096] + "\n[truncated]"
						}
					}
				}
				childStatuses = append(childStatuses, cs)
			}
			if !childTaskStatusesEqual(task.Status.ChildTasks, childStatuses) {
				childStatusesCopy := append([]corev1alpha1.ChildTaskStatus(nil), childStatuses...)
				if err := r.updateStatusWithRetry(ctx, task, func(t *corev1alpha1.Task) {
					t.Status.ChildTasks = childStatusesCopy
				}); err != nil {
					log.Error(err, "failed to update child task status")
				}
			}
		}
	}

	// Get the Job
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      task.Status.JobName,
		Namespace: task.Namespace,
	}, job); err != nil {
		if apierrors.IsNotFound(err) {
			if r.isWithinJobCreationVisibilityGracePeriod(task) {
				log.Info("job not found shortly after creation, waiting for cache visibility",
					"job", task.Status.JobName,
					"startTime", task.Status.StartTime,
				)
				return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
			}
			if r.shouldRetry(task) {
				log.Info("job not found while task still has retry budget, scheduling retry", "attempt", task.Status.Attempts)
				return r.retryTask(ctx, task)
			}
			log.Info("Job not found, task may have been cleaned up")
			return r.failTask(ctx, task, "job not found")
		}
		log.Error(err, "failed to get Job")
		return ctrl.Result{}, err
	}

	// Check Job status
	if job.Status.Succeeded > 0 {
		// Check if this is an autonomous task that should continue iterating
		if r.isAutonomousTask(ctx, task) {
			return r.handleAutonomousIteration(ctx, task)
		}
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

	// Check for pods stuck in Pending/ContainerCreating with unrecoverable errors
	// (e.g., missing secrets, missing configmaps, image pull errors)
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(task.Namespace),
		client.MatchingLabels{labels.LabelTask: labels.SelectorValue(task.Name)}); err == nil {
		for i := range podList.Items {
			pod := &podList.Items[i]
			if pod.Status.Phase != corev1.PodPending {
				continue
			}
			// Check waiting container statuses for unrecoverable errors
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.State.Waiting != nil {
					reason := cs.State.Waiting.Reason
					if reason == "CreateContainerConfigError" || reason == "ErrImageNeverPull" {
						msg := fmt.Sprintf("pod stuck: %s - %s", reason, cs.State.Waiting.Message)
						log.Info("failing task due to unrecoverable pod error", "reason", reason)
						return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, msg)
					}
				}
			}
			// Check pod conditions for unschedulable
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse && cond.Reason == "Unschedulable" {
					msg := fmt.Sprintf("pod unschedulable: %s", cond.Message)
					log.Info("failing task due to unschedulable pod", "message", cond.Message)
					return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, msg)
				}
			}
			// Check events for volume mount failures (pod stays in ContainerCreating)
			if task.Status.StartTime != nil && time.Since(task.Status.StartTime.Time) > 2*time.Minute {
				for _, cs := range pod.Status.ContainerStatuses {
					if cs.State.Waiting != nil && cs.State.Waiting.Reason == "ContainerCreating" {
						msg := "pod stuck in ContainerCreating for over 2 minutes (possible missing secret/volume)"
						log.Info("failing task due to extended ContainerCreating state")
						return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, msg)
					}
				}
			}
		}
	}

	// Job still running, requeue
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *TaskReconciler) isWithinJobCreationVisibilityGracePeriod(task *corev1alpha1.Task) bool {
	if task == nil || task.Status.JobName == "" || task.Status.StartTime == nil {
		return false
	}
	return time.Since(task.Status.StartTime.Time) < jobCreationVisibilityGracePeriod
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

	if err := r.enforceParentScheduledTaskHistory(ctx, task); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *TaskReconciler) enforceParentScheduledTaskHistory(ctx context.Context, task *corev1alpha1.Task) error {
	if task.Labels[labels.LabelScheduledRun] != scheduledRunLabelValue {
		return nil
	}

	parentName := labels.ParentTaskName(task.Labels, task.Annotations)
	if parentName == "" {
		return nil
	}

	parent := &corev1alpha1.Task{}
	if err := r.Get(ctx, client.ObjectKey{Name: parentName, Namespace: task.Namespace}, parent); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting parent scheduled task %q: %w", parentName, err)
	}

	if err := r.enforceHistoryLimits(ctx, parent); err != nil {
		return fmt.Errorf("enforcing history limits for parent task %q: %w", parentName, err)
	}

	return nil
}

// completeTask marks a task as completed
func (r *TaskReconciler) completeTask(ctx context.Context, task *corev1alpha1.Task, phase corev1alpha1.TaskPhase, message string) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	now := metav1.Now()
	task.Status.Phase = phase
	task.Status.CompletionTime = &now
	task.Status.Message = message

	if s := trace.SpanFromContext(ctx); s.IsRecording() {
		s.AddEvent("phase.transition", trace.WithAttributes(
			attribute.String("task.phase", string(phase)),
		))
	}

	// Collect result from Job output
	if err := r.collectResult(ctx, task); err != nil {
		log.Error(err, "failed to collect result")
		// Continue anyway, result collection is best-effort
	}

	// Update session if configured
	if task.Spec.SessionRef != nil && task.Spec.SessionRef.Append {
		if err := r.SessionManager.AppendMessages(ctx, task, r.ResultStore); err != nil {
			log.Error(err, "failed to append session messages")
			// Continue anyway
		}
	}
	// Release session lock regardless of Append setting
	if task.Spec.SessionRef != nil {
		if err := r.SessionManager.ReleaseLock(ctx, task); err != nil {
			log.Error(err, "failed to release session lock")
		}
	}

	// Clean up plan state on completion (best-effort)
	if r.PlanStore != nil {
		if err := r.PlanStore.DeletePlan(ctx, task.Namespace, task.Name); err != nil {
			log.Error(err, "failed to delete plan state on completion")
		}
	}

	conditionStatus := metav1.ConditionTrue
	reason := "TaskSucceeded"
	switch phase {
	case corev1alpha1.TaskPhaseFailed:
		conditionStatus = metav1.ConditionFalse
		reason = "TaskFailed"
	case corev1alpha1.TaskPhaseCancelled:
		conditionStatus = metav1.ConditionFalse
		reason = "TaskCancelled"
	}

	resultRef := task.Status.ResultRef
	if err := r.updateStatusWithRetry(ctx, task, func(t *corev1alpha1.Task) {
		t.Status.Phase = phase
		t.Status.CompletionTime = &now
		t.Status.Message = message
		t.Status.ResultRef = resultRef
		meta.SetStatusCondition(&t.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeComplete,
			Status:             conditionStatus,
			LastTransitionTime: now,
			Reason:             reason,
			Message:            message,
		})
	}); err != nil {
		log.Error(err, "failed to update completion status")
		return ctrl.Result{}, err
	}

	// Update the Agent's LastUsed timestamp so TTL tracking works
	if task.Spec.AgentRef != nil {
		if err := r.updateAgentLastUsed(ctx, task.Namespace, task.Spec.AgentRef.Name, now); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "failed to update agent LastUsed")
		}
	}

	return ctrl.Result{RequeueAfter: time.Second}, nil
}

func (r *TaskReconciler) updateAgentLastUsed(ctx context.Context, namespace, name string, at metav1.Time) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		agent := &corev1alpha1.Agent{}
		if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, agent); err != nil {
			return err
		}
		agent.Status.LastUsed = &at
		return r.Status().Update(ctx, agent)
	})
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
	// Attempts counts the initial run plus completed retries, while MaxRetries
	// is configured as the number of additional retry attempts. Retry while the
	// current execution count is still within that additional retry budget.
	return task.Status.Attempts <= task.Spec.RetryPolicy.MaxRetries
}

// retryTask creates a new Job for a retry attempt
func (r *TaskReconciler) retryTask(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// Calculate backoff delay
	delay := r.calculateRetryDelay(task)
	oldJobName := task.Status.JobName

	// Reset to pending for retry before deleting the old Job so a transient
	// NotFound from asynchronous Job deletion does not fail the task.
	if err := r.updateStatusWithRetry(ctx, task, func(t *corev1alpha1.Task) {
		t.Status.Phase = corev1alpha1.TaskPhasePending
		t.Status.JobName = ""
		t.Status.Message = ""
		t.Status.CompletionTime = nil
		t.Status.ResultRef = nil
	}); err != nil {
		log.Error(err, "failed to update status for retry")
		return ctrl.Result{}, err
	}

	// Delete the old Job after clearing the running status.
	if oldJobName != "" {
		job := &batchv1.Job{}
		err := r.Get(ctx, types.NamespacedName{
			Name:      oldJobName,
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
	maxDelay := 5 * time.Minute
	delay := initialDelay
	for i := int32(1); i < task.Status.Attempts; i++ {
		delay = time.Duration(float64(delay) * multiplier)
		// Guard against overflow (negative) and cap early
		if delay <= 0 || delay > maxDelay {
			delay = maxDelay
			break
		}
	}

	if delay > maxDelay {
		delay = maxDelay
	}

	return delay
}

// collectResult collects the task result from the Job's output
func (r *TaskReconciler) collectResult(ctx context.Context, task *corev1alpha1.Task) error {
	// Check if result already exists in store (written by worker via HTTP)
	_, err := r.ResultStore.GetResult(ctx, task.Namespace, task.Name)
	if err == nil {
		// Result already exists (written by worker)
		task.Status.ResultRef = &corev1alpha1.ResultReference{
			Available: true,
		}
		return nil
	}

	if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	// No result yet — capture pod logs for container tasks
	if task.Spec.Type != corev1alpha1.TaskTypeContainer || r.KubeClient == nil {
		return nil
	}

	logs, err := r.readPodLogs(ctx, task)
	if err != nil {
		return fmt.Errorf("reading pod logs: %w", err)
	}

	if err := r.ResultStore.SaveResult(ctx, task.Namespace, task.Name, []byte(logs)); err != nil {
		return fmt.Errorf("saving result: %w", err)
	}

	task.Status.ResultRef = &corev1alpha1.ResultReference{
		Available: true,
	}

	return nil
}

// readPodLogs reads logs from the first pod of a task's job.
func (r *TaskReconciler) readPodLogs(ctx context.Context, task *corev1alpha1.Task) (string, error) {
	podList := &corev1.PodList{}
	if err := r.List(ctx, podList,
		client.InNamespace(task.Namespace),
		client.MatchingLabels{"job-name": task.Status.JobName},
	); err != nil {
		return "", fmt.Errorf("listing pods: %w", err)
	}

	if len(podList.Items) == 0 {
		return "", fmt.Errorf("no pods found for job %s", task.Status.JobName)
	}

	const maxLogBytes = int64(5 << 20) // 5MB

	pod := podList.Items[len(podList.Items)-1]
	req := r.KubeClient.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		LimitBytes: ptr.To(maxLogBytes),
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("streaming logs: %w", err)
	}
	defer stream.Close() //nolint:errcheck

	data, err := io.ReadAll(io.LimitReader(stream, maxLogBytes))
	if err != nil {
		return "", fmt.Errorf("reading logs: %w", err)
	}

	if int64(len(data)) == maxLogBytes {
		data = append(data, "\n[truncated]"...)
	}

	return string(data), nil
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
		scheduledTime = sched.Next(task.Status.LastScheduleTime.In(loc))
	} else {
		scheduledTime = sched.Next(task.CreationTimestamp.In(loc))
	}

	// Not yet time
	if now.Before(scheduledTime) {
		nextSchedule := metav1.NewTime(scheduledTime)
		if task.Status.NextScheduleTime == nil || !task.Status.NextScheduleTime.Equal(&nextSchedule) {
			nextScheduleCopy := nextSchedule
			_ = r.updateStatusWithRetry(ctx, task, func(t *corev1alpha1.Task) {
				t.Status.NextScheduleTime = &nextScheduleCopy
			})
		}
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
		nextScheduleCopy := nextSchedule
		_ = r.updateStatusWithRetry(ctx, task, func(t *corev1alpha1.Task) {
			t.Status.NextScheduleTime = &nextScheduleCopy
		})
		return ctrl.Result{RequeueAfter: time.Until(next)}, nil
	}

	// Check concurrency policy
	if task.Spec.ConcurrencyPolicy == corev1alpha1.ForbidConcurrent || task.Spec.ConcurrencyPolicy == "" {
		var childList corev1alpha1.TaskList
		if err := r.List(ctx, &childList, client.InNamespace(task.Namespace), client.MatchingLabels{
			labels.LabelParentTask: labels.SelectorValue(task.Name),
		}); err != nil {
			return ctrl.Result{}, fmt.Errorf("listing child tasks: %w", err)
		}
		for i := range childList.Items {
			if childList.Items[i].Status.Phase == corev1alpha1.TaskPhasePending ||
				childList.Items[i].Status.Phase == corev1alpha1.TaskPhaseRunning {
				log.Info("Concurrency policy Forbid: active child task exists, skipping", "activeChild", childList.Items[i].Name)
				next := sched.Next(now)
				nextSchedule := metav1.NewTime(next)
				nextScheduleCopy := nextSchedule
				_ = r.updateStatusWithRetry(ctx, task, func(t *corev1alpha1.Task) {
					t.Status.NextScheduleTime = &nextScheduleCopy
				})
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
				labels.LabelParentTask:   labels.SelectorValue(task.Name),
				labels.LabelScheduledRun: scheduledRunLabelValue,
			},
			Annotations: map[string]string{
				labels.AnnotationParentTaskName: task.Name,
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
	nowTimeCopy := nowTime
	nextScheduleCopy := nextSchedule
	if err := r.updateStatusWithRetry(ctx, task, func(t *corev1alpha1.Task) {
		t.Status.LastScheduleTime = &nowTimeCopy
		t.Status.NextScheduleTime = &nextScheduleCopy
	}); err != nil {
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
		labels.LabelParentTask: labels.SelectorValue(task.Name),
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
		slices.SortFunc(tasks, func(a, b *corev1alpha1.Task) int {
			return a.CreationTimestamp.Compare(b.CreationTimestamp.Time)
		})
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
	r.Recorder = mgr.GetEventRecorderFor("task-controller") //nolint:staticcheck
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Task{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1alpha1.Task{}).
		Named("task").
		Complete(r)
}

const (
	workerServiceAccountName = "orka-worker"
	workerClusterRoleName    = "orka-orka-worker-role"
)

// ensureWorkerRBAC ensures the orka-worker ServiceAccount and ClusterRoleBinding
// exist in the given namespace so that task jobs have the correct permissions.
func (r *TaskReconciler) ensureWorkerRBAC(ctx context.Context, namespace string) error {
	log := logf.FromContext(ctx)

	// Ensure ServiceAccount exists
	sa := &corev1.ServiceAccount{}
	err := r.Get(ctx, types.NamespacedName{Name: workerServiceAccountName, Namespace: namespace}, sa)
	if apierrors.IsNotFound(err) {
		sa = &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:      workerServiceAccountName,
				Namespace: namespace,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "orka",
				},
			},
		}
		if err := r.Create(ctx, sa); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating ServiceAccount: %w", err)
		}
		log.Info("Created worker ServiceAccount", "namespace", namespace)
	} else if err != nil {
		return fmt.Errorf("getting ServiceAccount: %w", err)
	}

	// Ensure ClusterRoleBinding includes this namespace
	bindingName := fmt.Sprintf("orka-worker-%s", namespace)
	crb := &rbacv1.ClusterRoleBinding{}
	err = r.Get(ctx, types.NamespacedName{Name: bindingName}, crb)
	if apierrors.IsNotFound(err) {
		crb = &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: bindingName,
				Labels: map[string]string{
					"app.kubernetes.io/managed-by": "orka",
				},
			},
			RoleRef: rbacv1.RoleRef{
				APIGroup: "rbac.authorization.k8s.io",
				Kind:     "ClusterRole",
				Name:     workerClusterRoleName,
			},
			Subjects: []rbacv1.Subject{
				{
					Kind:      "ServiceAccount",
					Name:      workerServiceAccountName,
					Namespace: namespace,
				},
			},
		}
		if err := r.Create(ctx, crb); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating ClusterRoleBinding: %w", err)
		}
		log.Info("Created worker ClusterRoleBinding", "namespace", namespace, "binding", bindingName)
	} else if err != nil {
		return fmt.Errorf("getting ClusterRoleBinding: %w", err)
	}

	return nil
}

// isAutonomousTask checks if this task has autonomous mode enabled via its agent.
func (r *TaskReconciler) isAutonomousTask(ctx context.Context, task *corev1alpha1.Task) bool {
	if task.Spec.AgentRef == nil {
		return false
	}

	agent := &corev1alpha1.Agent{}
	agentNS := task.Namespace
	if task.Spec.AgentRef.Namespace != "" {
		agentNS = task.Spec.AgentRef.Namespace
	}
	if err := r.Get(ctx, types.NamespacedName{Name: task.Spec.AgentRef.Name, Namespace: agentNS}, agent); err != nil {
		return false
	}

	return agent.Spec.Coordination != nil && agent.Spec.Coordination.Autonomous
}

// handleAutonomousIteration handles the completion of one autonomous loop iteration.
// It saves plan state, checks termination conditions, and creates a new Job if needed.
func (r *TaskReconciler) handleAutonomousIteration(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("handling autonomous iteration", "iteration", task.Status.Iteration)

	// Collect result from this iteration (best-effort)
	if err := r.collectResult(ctx, task); err != nil {
		log.Error(err, "failed to collect iteration result")
	}

	// Check plan state for termination signals
	if r.PlanStore != nil {
		plan, err := r.PlanStore.GetPlan(ctx, task.Namespace, task.Name)
		if err == nil && plan.GoalComplete {
			log.Info("autonomous task goal complete", "iteration", task.Status.Iteration, "summary", plan.Summary)
			return r.completeTask(ctx, task, corev1alpha1.TaskPhaseSucceeded,
				fmt.Sprintf("goal complete after %d iterations: %s", task.Status.Iteration+1, plan.Summary))
		}
	}

	// Check max iterations
	if task.Spec.AgentRef == nil {
		return r.failTask(ctx, task, "autonomous task requires agentRef")
	}
	agent := &corev1alpha1.Agent{}
	agentNS := task.Namespace
	if task.Spec.AgentRef.Namespace != "" {
		agentNS = task.Spec.AgentRef.Namespace
	}
	if err := r.Get(ctx, types.NamespacedName{Name: task.Spec.AgentRef.Name, Namespace: agentNS}, agent); err != nil {
		log.Error(err, "failed to get agent for autonomous check")
		return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, "failed to resolve agent for autonomous iteration")
	}

	maxIter := agent.Spec.Coordination.MaxIterations
	if maxIter > 0 && task.Status.Iteration+1 >= maxIter {
		log.Info("autonomous task reached max iterations", "maxIterations", maxIter)
		return r.completeTask(ctx, task, corev1alpha1.TaskPhaseSucceeded,
			fmt.Sprintf("reached max iterations (%d)", maxIter))
	}

	// Check if suspended — keep task Running so it can resume when Suspend is unset
	if task.Spec.Suspend != nil && *task.Spec.Suspend {
		log.Info("autonomous task suspended, waiting for resume")
		task.Status.Message = fmt.Sprintf("autonomous task suspended at iteration %d", task.Status.Iteration)
		if err := r.Status().Update(ctx, task); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Enforce child task history limits
	if err := r.enforceHistoryLimits(ctx, task); err != nil {
		log.Error(err, "failed to enforce history limits for autonomous task")
	}

	// Delete old Job
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
				log.Error(err, "failed to delete old Job for autonomous iteration")
			}
		}
	}

	// Increment iteration and reset to Pending for next Job creation
	task.Status.Iteration++
	task.Status.Phase = corev1alpha1.TaskPhasePending
	task.Status.JobName = ""
	task.Status.Message = fmt.Sprintf("autonomous iteration %d", task.Status.Iteration)

	if err := r.Status().Update(ctx, task); err != nil {
		log.Error(err, "failed to update status for autonomous iteration")
		return ctrl.Result{}, err
	}

	log.Info("autonomous task advancing to next iteration", "nextIteration", task.Status.Iteration)
	if r.Recorder != nil {
		r.Recorder.Event(task, corev1.EventTypeNormal, "AutonomousIteration",
			fmt.Sprintf("Starting iteration %d", task.Status.Iteration))
	}

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}
