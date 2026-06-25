/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	cron "github.com/robfig/cron/v3"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	execevents "github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/tracing"
	"github.com/sozercan/orka/internal/workerenv"
	"github.com/sozercan/orka/internal/workspace"
	"github.com/sozercan/orka/internal/workspace/statusrules"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	taskTransactionTokenPendingTimeout = 2 * time.Minute
	failedMountEventStaleAfter         = 2 * time.Minute
	podLogLimitBytes                   = int64(5 << 20)
	stdoutResultLogLimitBytes          = int64(15 << 20)

	eventInvolvedObjectNameField = "involvedObject.name"
	eventReasonField             = "reason"
)

const (
	// ConditionTypeComplete indicates the task has completed
	ConditionTypeComplete = "Complete"

	// ConditionTypeJobCreated indicates a Job has been created
	ConditionTypeJobCreated = "JobCreated"

	// jobCreationVisibilityGracePeriod avoids failing a task when the controller cache
	// has not observed the Job immediately after create.
	jobCreationVisibilityGracePeriod = 30 * time.Second

	workerClusterRoleBindingRecreateInterval = 100 * time.Millisecond
	workerClusterRoleBindingRecreateTimeout  = 5 * time.Second

	scheduledRunLabelValue = "true"
	managedLabelValue      = scheduledRunLabelValue

	workerRBACReconcileFailedReason = "WorkerRBACReconcileFailed"
)

// TaskReconciler reconciles a Task object
type TaskReconciler struct {
	client.Client
	Scheme                             *runtime.Scheme
	JobBuilder                         *JobBuilder
	SessionManager                     *SessionManager
	WebhookNotifier                    *WebhookNotifier
	Recorder                           record.EventRecorder
	KubeClient                         kubernetes.Interface
	ResultStore                        store.ResultStore
	PlanStore                          store.PlanStore
	MessageStore                       store.MessageStore
	ArtifactStore                      store.ArtifactStore
	ExecutionEventStore                store.ExecutionEventStore
	EnforceNamespaceIsolation          bool
	MaxTasksPerNamespace               int32
	ExecutionWorkspaceDefaultProvider  corev1alpha1.WorkspaceProvider
	AgentSandboxEnabled                bool
	AgentSandboxConfig                 AgentSandboxConfig
	SubstrateEnabled                   bool
	SubstrateConfig                    SubstrateConfig
	SubstrateExecutorFactory           func(SubstrateConfig) (workspace.WorkspaceExecutor, error)
	AIWorkerClusterRoleName            string
	VendorWorkerClusterRoleName        string
	ContainerWorkerClusterRoleName     string
	WorkerClusterRoleBindingNamePrefix string
}

// +kubebuilder:rbac:groups=core.orka.ai,resources=tasks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.orka.ai,resources=tasks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=tasks/finalizers,verbs=update
// +kubebuilder:rbac:groups=core.orka.ai,resources=agents,verbs=get;list;watch
// +kubebuilder:rbac:groups=core.orka.ai,resources=tools,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups="",resources=pods/status,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=endpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch
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
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;roles,verbs=get;list;watch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,resourceNames=ai-worker-role;vendor-worker-role;container-worker-role,verbs=bind
// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=get;list
// +kubebuilder:rbac:groups=ate.dev,resources=actortemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxtemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxwarmpools,verbs=get;list;watch
// +kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/portforward,verbs=create
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

func canStartTaskJob(phase corev1alpha1.TaskPhase) bool {
	switch phase {
	case "", corev1alpha1.TaskPhasePending, corev1alpha1.TaskPhaseScheduled:
		return true
	default:
		return false
	}
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

	if tx := task.Spec.Transaction; tx != nil {
		values := []any{}
		if tx.ID != "" {
			values = append(values, "transactionID", tx.ID)
		}
		if tx.Profile != "" {
			values = append(values, "contextTokenProfile", tx.Profile)
		}
		if tx.RequestingWorkload != "" {
			values = append(values, "requestingWorkload", tx.RequestingWorkload)
		}
		if len(values) > 0 {
			log = log.WithValues(values...)
			ctx = logf.IntoContext(ctx, log)
		}
	}

	spanAttributes := []attribute.KeyValue{
		attribute.String("task.name", task.Name),
		attribute.String("task.namespace", task.Namespace),
		attribute.String("task.type", string(task.Spec.Type)),
	}
	if tx := task.Spec.Transaction; tx != nil {
		if tx.ID != "" {
			spanAttributes = append(spanAttributes, attribute.String("transaction.id", tx.ID))
		}
		if tx.Profile != "" {
			spanAttributes = append(spanAttributes, attribute.String("context_token.profile", tx.Profile))
		}
	}

	tracer := tracing.Tracer("orka.controller")
	ctx, span := tracer.Start(ctx, "task.reconcile",
		trace.WithAttributes(spanAttributes...),
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
		_ = r.recordTaskLifecycleEvent(
			ctx,
			task,
			execevents.ExecutionEventTypeTaskCreated,
			execevents.ExecutionEventSeverityInfo,
			"Task status initialized to Pending",
		)
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

func (r *TaskReconciler) recordTaskLifecycleEvent(
	ctx context.Context,
	task *corev1alpha1.Task,
	eventType string,
	severity string,
	summary string,
) error {
	if r == nil || r.ExecutionEventStore == nil || task == nil {
		return nil
	}
	if strings.TrimSpace(task.Namespace) == "" || strings.TrimSpace(task.Name) == "" {
		return nil
	}
	_, err := r.ExecutionEventStore.AppendExecutionEvent(ctx, &store.ExecutionEvent{
		Namespace:   task.Namespace,
		StreamType:  store.ExecutionEventStreamTypeTask,
		StreamID:    task.Name,
		TaskName:    task.Name,
		SessionName: r.executionEventSessionName(ctx, task),
		Type:        eventType,
		Severity:    severity,
		Summary:     summary,
	})
	if err != nil {
		logf.FromContext(ctx).Error(
			err,
			"failed to record task lifecycle execution event",
			"namespace", task.Namespace,
			"task", task.Name,
			"eventType", eventType,
		)
		return err
	}
	return nil
}

func (r *TaskReconciler) executionEventSessionName(ctx context.Context, task *corev1alpha1.Task) string {
	sessionName := taskSessionName(task)
	if sessionName == "" {
		return ""
	}
	if r == nil || r.SessionManager == nil || r.SessionManager.store == nil {
		return ""
	}
	if _, err := r.SessionManager.GetSession(ctx, task.Namespace, sessionName); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ""
		}
		logf.FromContext(ctx).Error(
			err,
			"failed to check session before recording task lifecycle execution event",
			"namespace", task.Namespace,
			"task", task.Name,
			"session", sessionName,
		)
		return sessionName
	}
	return sessionName
}

func taskSessionName(task *corev1alpha1.Task) string {
	if task == nil || task.Spec.SessionRef == nil {
		return ""
	}
	return strings.TrimSpace(task.Spec.SessionRef.Name)
}

func executionEventSeverityForTaskPhase(phase corev1alpha1.TaskPhase) string {
	switch phase {
	case corev1alpha1.TaskPhaseFailed:
		return execevents.ExecutionEventSeverityError
	case corev1alpha1.TaskPhaseCancelled:
		return execevents.ExecutionEventSeverityWarning
	default:
		return execevents.ExecutionEventSeverityInfo
	}
}

func (r *TaskReconciler) recordTerminalTaskLifecycleEventIfMissing(ctx context.Context, task *corev1alpha1.Task) bool {
	if r == nil || r.ExecutionEventStore == nil || task == nil {
		return true
	}
	eventType := executionEventTypeForTaskPhase(task.Status.Phase)
	if eventType == "" {
		return true
	}
	listed, err := r.ExecutionEventStore.ListExecutionEvents(ctx, store.ExecutionEventFilter{
		Namespace:  task.Namespace,
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   task.Name,
		EventTypes: []string{eventType},
		Limit:      1,
	})
	if err != nil {
		logf.FromContext(ctx).Error(
			err,
			"failed to check existing terminal task lifecycle execution event",
			"namespace", task.Namespace,
			"task", task.Name,
			"eventType", eventType,
		)
		return false
	}
	if len(listed) > 0 {
		return true
	}
	return r.recordTaskLifecycleEvent(
		ctx,
		task,
		eventType,
		executionEventSeverityForTaskPhase(task.Status.Phase),
		task.Status.Message,
	) == nil
}

func executionEventTypeForTaskPhase(phase corev1alpha1.TaskPhase) string {
	switch phase {
	case corev1alpha1.TaskPhaseSucceeded:
		return execevents.ExecutionEventTypeTaskSucceeded
	case corev1alpha1.TaskPhaseFailed:
		return execevents.ExecutionEventTypeTaskFailed
	case corev1alpha1.TaskPhaseCancelled:
		return execevents.ExecutionEventTypeTaskCancelled
	default:
		return ""
	}
}

// handleDeletion handles Task cleanup when deleted
func (r *TaskReconciler) handleDeletion(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) { //nolint:unparam // Result is always nil but kept for interface consistency
	log := logf.FromContext(ctx)

	if controllerutil.ContainsFinalizer(task, labels.TaskFinalizer) {
		// Clean up result data from store
		if task.Status.ResultRef != nil && task.Status.ResultRef.Available && r.ResultStore != nil {
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

		// Clean up execution timeline events before allowing a future task with the
		// same namespace/name to expose stale history.
		if r.ExecutionEventStore != nil {
			if err := r.ExecutionEventStore.DeleteExecutionEvents(ctx, task.Namespace, store.ExecutionEventStreamTypeTask, task.Name); err != nil {
				log.Error(err, "failed to delete execution events", "task", task.Name)
				return ctrl.Result{}, err
			}
		}

		if cancelErr := r.cancelHarnessWrapperTurn(ctx, task, "task deleted"); cancelErr != nil {
			log.Error(cancelErr, "failed to cancel deleted harness runtime turn")
		}

		waitingForJob, err := r.cleanupDeletedTaskJob(ctx, task)
		if err != nil {
			log.Error(err, "failed to delete Job")
			return ctrl.Result{}, err
		}
		if waitingForJob {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		releasedPoolLeases, err := r.releaseSubstratePoolActorLeasesAfterTerminalCleanup(ctx, task)
		if err != nil {
			log.Error(err, "failed to release substrate pool actor leases")
			return ctrl.Result{}, err
		}
		if !releasedPoolLeases {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		var pruneErr error
		if err := r.pruneUnusedWorkerRBAC(ctx, task.Namespace, ""); err != nil {
			log.Error(err, "failed to prune unused worker RBAC for deleted task")
			pruneErr = err
		}

		// Release session lock if held
		if task.Spec.SessionRef != nil {
			if err := r.SessionManager.ReleaseLock(ctx, task); err != nil {
				log.Error(err, "failed to release session lock")
				// Continue with finalizer removal anyway
			}
		}
		if pruneErr != nil {
			return ctrl.Result{}, pruneErr
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

	if taskTransactionTokenPending(task) {
		return r.handleTransactionTokenPending(ctx, task)
	}

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

	if err := r.validateExecutionWorkspace(task); err != nil {
		log.Error(err, "execution workspace validation failed")
		if statusErr := r.markExecutionWorkspaceValidationFailed(ctx, task, err); statusErr != nil {
			log.Error(statusErr, "failed to update execution workspace validation status")
			return ctrl.Result{}, statusErr
		}
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

	if task.Spec.Type == corev1alpha1.TaskTypeAgent {
		if err := r.pruneUnusedWorkerRBAC(ctx, task.Namespace, ""); err != nil {
			log.Error(err, "failed to prune unused worker RBAC")
			return ctrl.Result{}, err
		}
		if reason := agentTaskJobBackendUnsupportedReason(task, agent); reason != "" {
			return r.failTask(ctx, task, reason)
		}
		if taskHasPlannedHarnessWrapperTurn(task) {
			return r.runHarnessWrapperTask(ctx, task, agent)
		}
		return r.runHarnessWrapperTask(ctx, task, agent)
	}

	return r.createTaskJob(ctx, task, agent, provider)
}

func taskTransactionTokenPending(task *corev1alpha1.Task) bool {
	if task == nil || task.Annotations == nil {
		return false
	}
	pending, err := strconv.ParseBool(task.Annotations[labels.AnnotationTransactionTokenPending])
	return err == nil && pending
}

func agentTaskJobBackendUnsupportedReason(task *corev1alpha1.Task, agent *corev1alpha1.Agent) string {
	if task == nil {
		return ""
	}
	switch {
	case task.Spec.Transaction != nil:
		return "agent CLI runtime tasks do not support transaction token delegation with the harness wrapper yet"
	case effectiveAgentResources(task, agent):
		return "agent CLI runtime tasks do not support custom Kubernetes resources with the harness wrapper yet"
	case resolveExecution(task, agent) != nil:
		return "agent CLI runtime tasks do not support execution placement with the harness wrapper yet"
	default:
		return ""
	}
}

func effectiveAgentResources(task *corev1alpha1.Task, agent *corev1alpha1.Agent) bool {
	if task != nil && (len(task.Spec.Resources.Requests) > 0 || len(task.Spec.Resources.Limits) > 0) {
		return true
	}
	return agent != nil && (len(agent.Spec.Resources.Requests) > 0 || len(agent.Spec.Resources.Limits) > 0)
}

func (r *TaskReconciler) handleTransactionTokenPending(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	now := time.Now()
	since, err := transactionTokenPendingSince(task)
	if err != nil {
		patch := client.MergeFrom(task.DeepCopy())
		if task.Annotations == nil {
			task.Annotations = map[string]string{}
		}
		task.Annotations[labels.AnnotationTransactionTokenPendingSince] = now.Format(time.RFC3339Nano)
		if updateErr := r.Patch(ctx, task, patch); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		log.Info("task is waiting for delegated transaction token setup", "pendingSinceInitialized", true)
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	elapsed := now.Sub(since)
	if elapsed >= taskTransactionTokenPendingTimeout {
		msg := fmt.Sprintf("delegated transaction token setup timed out after %s", taskTransactionTokenPendingTimeout)
		r.Recorder.Event(task, corev1.EventTypeWarning, "TransactionTokenPendingTimeout", msg)
		return r.failTask(ctx, task, msg)
	}

	requeueAfter := min(taskTransactionTokenPendingTimeout-elapsed, time.Second)
	log.Info("task is waiting for delegated transaction token setup", "pendingSince", since)
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func transactionTokenPendingSince(task *corev1alpha1.Task) (time.Time, error) {
	if task == nil || task.Annotations == nil {
		return time.Time{}, fmt.Errorf("missing transaction token pending timestamp")
	}
	value := strings.TrimSpace(task.Annotations[labels.AnnotationTransactionTokenPendingSince])
	if value == "" {
		return time.Time{}, fmt.Errorf("missing transaction token pending timestamp")
	}
	since, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid transaction token pending timestamp: %w", err)
	}
	return since, nil
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
		if strings.Contains(err.Error(), "already locked") {
			session, getErr := r.SessionManager.GetSession(ctx, task.Namespace, task.Spec.SessionRef.Name)
			if getErr != nil {
				return ctrl.Result{}, getErr, true
			}
			if session.ActiveTask == task.Name {
				return ctrl.Result{}, nil, false
			}
			if session.ActiveTask == "" {
				return ctrl.Result{RequeueAfter: time.Second}, nil, true
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil, true
		}
		log.Error(err, "failed to acquire session lock")
		if errors.Is(err, store.ErrNotFound) {
			result, failErr := r.failTask(ctx, task, err.Error())
			return result, failErr, true
		}
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

	latest := &corev1alpha1.Task{}
	if err := r.Get(ctx, types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, latest); err != nil {
		return ctrl.Result{}, err
	}
	if !canStartTaskJob(latest.Status.Phase) {
		task.Status = latest.Status
		log.Info("skipping job creation because task is no longer runnable", "phase", latest.Status.Phase)
		return ctrl.Result{}, nil
	}

	// Ensure only the worker ServiceAccount and RBAC required for this task exist in the task namespace.
	// Worker identities are no longer statically bound for every trust tier, so
	// selected-tier RBAC must exist before creating a Job that references it.
	if err := r.ensureWorkerRBAC(ctx, task); err != nil {
		log.Error(err, "failed to ensure worker RBAC")
		if r.Recorder != nil {
			r.Recorder.Eventf(task, corev1.EventTypeWarning, workerRBACReconcileFailedReason,
				"failed to ensure worker RBAC in namespace %q: %v", task.Namespace, err)
		}
		return ctrl.Result{}, err
	}

	workspaceRequest, err := r.resolveExecutionWorkspaceRequest(ctx, task)
	if err != nil {
		log.Error(err, "failed to resolve execution workspace")
		if statusErr := r.markExecutionWorkspaceValidationFailed(ctx, task, err); statusErr != nil {
			log.Error(statusErr, "failed to update execution workspace validation status")
			return ctrl.Result{}, statusErr
		}
		return r.failTask(ctx, task, fmt.Sprintf("failed to resolve execution workspace: %v", err))
	}
	reservedPoolActor := false
	if workspaceRequest != nil && workspaceRequest.PoolName != "" {
		reserved, err := r.reserveSubstratePoolActor(ctx, task, workspaceRequest)
		if err != nil {
			log.Error(err, "failed to reserve substrate pool actor")
			return ctrl.Result{}, err
		}
		if !reserved {
			log.Info("all substrate pool actors are busy",
				"pool", workspaceRequest.PoolName,
				"poolNamespace", workspaceRequest.PoolNamespace,
			)
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		reservedPoolActor = true
	}

	// Create the Job
	job, err := r.JobBuilder.BuildWithOptions(ctx, task, agent, provider, JobBuildOptions{
		ExecutionWorkspace: workspaceRequest,
	})
	if err != nil {
		log.Error(err, "failed to build Job")
		if reservedPoolActor {
			if releaseErr := r.releaseSubstratePoolActorLeases(ctx, task); releaseErr != nil {
				return ctrl.Result{}, releaseErr
			}
		}
		return r.failTask(ctx, task, fmt.Sprintf("failed to build job: %v", err))
	}

	// Set owner reference
	if err := controllerutil.SetControllerReference(task, job, r.Scheme); err != nil {
		log.Error(err, "failed to set owner reference")
		if reservedPoolActor {
			if releaseErr := r.releaseSubstratePoolActorLeases(ctx, task); releaseErr != nil {
				return ctrl.Result{}, releaseErr
			}
		}
		return ctrl.Result{}, err
	}

	// Create the Job
	if err := r.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Job already exists, update status
			task.Status.JobName = job.Name
		} else {
			log.Error(err, "failed to create Job")
			if reservedPoolActor {
				if releaseErr := r.releaseSubstratePoolActorLeases(ctx, task); releaseErr != nil {
					return ctrl.Result{}, releaseErr
				}
			}
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
	transitionedToRunning := false
	if err := r.updateStatusWithRetry(ctx, task, func(t *corev1alpha1.Task) {
		transitionedToRunning = false
		if !canStartTaskJob(t.Status.Phase) {
			return
		}
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
		transitionedToRunning = true
	}); err != nil {
		log.Error(err, "failed to update status")
		return ctrl.Result{}, err
	}
	if transitionedToRunning {
		_ = r.recordTaskLifecycleEvent(
			ctx,
			task,
			execevents.ExecutionEventTypeTaskJobCreated,
			execevents.ExecutionEventSeverityInfo,
			fmt.Sprintf("Job %s created", jobName),
		)
		_ = r.recordTaskLifecycleEvent(
			ctx,
			task,
			execevents.ExecutionEventTypeTaskStarted,
			execevents.ExecutionEventSeverityInfo,
			fmt.Sprintf("Task started with Job %s", jobName),
		)
	}

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *TaskReconciler) reserveSubstratePoolActor(
	ctx context.Context,
	task *corev1alpha1.Task,
	request *ExecutionWorkspaceRequest,
) (bool, error) {
	if task == nil || request == nil || strings.TrimSpace(request.PoolName) == "" {
		return true, nil
	}
	target := int(request.PoolTargetActors)
	if target <= 0 {
		return false, fmt.Errorf("substrate actor pool %q in namespace %q has no target actors", request.PoolName, request.PoolNamespace)
	}
	if actorID, found, err := r.substratePoolActorLeaseForTask(ctx, task); err != nil {
		return false, err
	} else if found {
		if task.Status.Attempts > 0 && taskSubstratePoolActorCleanupRequired(task) {
			return false, nil
		}
		request.ClaimName = actorID
		return true, nil
	}

	prefix := deterministicSubstratePoolActorPrefix(request.PoolNamespace, request.PoolName)
	startOrdinal := 0
	if ordinal, ok := substratePoolActorOrdinalFromID(request.ClaimName, prefix); ok {
		startOrdinal = ordinal
	}
	for offset := range target {
		ordinal := (startOrdinal + offset) % target
		actorID := deterministicSubstratePoolActorID(prefix, ordinal)
		reserved, err := r.tryReserveSubstratePoolActor(ctx, task, request.PoolNamespace, actorID)
		if err != nil {
			return false, err
		}
		if reserved {
			request.ClaimName = actorID
			return true, nil
		}
	}
	return false, nil
}

func (r *TaskReconciler) substratePoolActorLeaseForTask(
	ctx context.Context,
	task *corev1alpha1.Task,
) (string, bool, error) {
	if task == nil || task.UID == "" {
		return "", false, nil
	}
	var leases coordinationv1.LeaseList
	if err := r.List(ctx, &leases, client.MatchingLabels{
		labels.LabelPurpose:                   substratePoolActorLeasePurpose,
		substratePoolActorLeaseHolderUIDLabel: labels.SelectorValue(string(task.UID)),
	}); err != nil {
		return "", false, err
	}
	for i := range leases.Items {
		lease := &leases.Items[i]
		if !substratePoolActorLeaseHeldByTask(lease, task) {
			continue
		}
		if actorID := substratePoolActorLeaseActorID(lease); actorID != "" {
			return actorID, true, nil
		}
	}
	return "", false, nil
}

func (r *TaskReconciler) tryReserveSubstratePoolActor(
	ctx context.Context,
	task *corev1alpha1.Task,
	leaseNamespace string,
	actorID string,
) (bool, error) {
	leaseName := substratePoolActorLeaseName(actorID)
	lease := &coordinationv1.Lease{}
	key := types.NamespacedName{Namespace: leaseNamespace, Name: leaseName}
	err := r.Get(ctx, key, lease)
	if apierrors.IsNotFound(err) {
		lease = newSubstratePoolActorLease(task, leaseNamespace, leaseName, actorID)
		if err := r.Create(ctx, lease); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if substratePoolActorLeaseHeldByTask(lease, task) {
		return true, nil
	}
	busy, err := substratePoolActorLeaseHasActiveHolder(ctx, r.Client, lease)
	if err != nil || busy {
		return false, err
	}
	patch := client.MergeFromWithOptions(lease.DeepCopy(), client.MergeFromWithOptimisticLock{})
	setSubstratePoolActorLeaseHolder(lease, task, actorID)
	if err := r.Patch(ctx, lease, patch); err != nil {
		if apierrors.IsConflict(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (r *TaskReconciler) releaseSubstratePoolActorLeases(ctx context.Context, task *corev1alpha1.Task) error {
	leases, err := r.substratePoolActorLeasesForTask(ctx, task)
	if err != nil {
		return err
	}
	return r.deleteSubstratePoolActorLeasesForTask(ctx, task, leases)
}

func (r *TaskReconciler) deleteSubstratePoolActorLeasesForTask(ctx context.Context, task *corev1alpha1.Task, leases []coordinationv1.Lease) error {
	for i := range leases {
		lease := &leases[i]
		current := &coordinationv1.Lease{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: lease.Namespace, Name: lease.Name}, current); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if !substratePoolActorLeaseHeldByTask(current, task) {
			continue
		}
		if err := r.Delete(ctx, current, deleteCurrentObjectPreconditions(current)...); err != nil && !apierrors.IsNotFound(err) {
			if apierrors.IsConflict(err) {
				stillHeld, verifyErr := substrateLeaseStillMatchesAfterDeleteConflict(ctx, r.Client, current, func(latest *coordinationv1.Lease) bool {
					return substratePoolActorLeaseHeldByTask(latest, task)
				})
				if verifyErr != nil {
					return verifyErr
				}
				if !stillHeld {
					continue
				}
			}
			return err
		}
	}
	return nil
}

func (r *TaskReconciler) releaseSubstratePoolActorLeasesAfterTerminalCleanup(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	leases, err := r.substratePoolActorLeasesForTask(ctx, task)
	if err != nil {
		return false, err
	}
	if len(leases) == 0 {
		return true, nil
	}
	if !taskExecutionWorkspaceCleanupSucceeded(task) {
		logf.FromContext(ctx).Info(
			"deleting substrate pool actors before releasing terminal task leases",
			"task", task.Name,
			"namespace", task.Namespace,
			"workspacePhase", taskExecutionWorkspacePhase(task),
			"workspaceReason", taskExecutionWorkspaceReason(task),
		)
		if err := r.deleteSubstratePoolActorsForLeases(ctx, task, leases); err != nil {
			logf.FromContext(ctx).Error(err, "failed to delete substrate pool actors before releasing terminal task leases")
			return false, nil
		}
	}
	return true, r.deleteSubstratePoolActorLeasesForTask(ctx, task, leases)
}

func (r *TaskReconciler) deleteSubstratePoolActorsForLeases(ctx context.Context, task *corev1alpha1.Task, leases []coordinationv1.Lease) error {
	cfg := r.SubstrateConfig.WithDefaults()
	executorFactory := r.SubstrateExecutorFactory
	if executorFactory == nil {
		executorFactory = func(cfg SubstrateConfig) (workspace.WorkspaceExecutor, error) {
			return workspace.NewSubstrateExecutor(workspace.SubstrateConfig{
				APIEndpoint:           cfg.APIEndpoint,
				APICAFile:             cfg.APICAFile,
				APIInsecureSkipVerify: cfg.APIInsecureSkipVerify,
				RouterURL:             cfg.RouterURL,
				ActorDNSSuffix:        cfg.ActorDNSSuffix,
			})
		}
	}
	executor, err := executorFactory(cfg)
	if err != nil {
		return err
	}
	defer closeWorkspaceExecutor(ctx, executor)
	for i := range leases {
		lease := &leases[i]
		current := &coordinationv1.Lease{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: lease.Namespace, Name: lease.Name}, current); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if !substratePoolActorLeaseHeldByTask(current, task) {
			continue
		}
		actorID := substratePoolActorLeaseActorID(current)
		if actorID == "" {
			continue
		}
		if _, err := executor.Delete(ctx, workspace.DeleteRequest{
			Ref:       workspace.WorkspaceRef{Namespace: current.Namespace, ClaimName: actorID, ID: actorID},
			Reason:    "terminal pooled task actor cleanup",
			Timeout:   cfg.ClaimTimeout,
			SkipScrub: true,
		}); err != nil {
			return fmt.Errorf("deleting substrate pool actor %q: %w", actorID, err)
		}
	}
	return nil
}

func (r *TaskReconciler) substratePoolActorLeasesForTask(
	ctx context.Context,
	task *corev1alpha1.Task,
) ([]coordinationv1.Lease, error) {
	if task == nil || task.UID == "" {
		return nil, nil
	}
	var leases coordinationv1.LeaseList
	if err := r.List(ctx, &leases, client.MatchingLabels{
		labels.LabelPurpose:                   substratePoolActorLeasePurpose,
		substratePoolActorLeaseHolderUIDLabel: labels.SelectorValue(string(task.UID)),
	}); err != nil {
		return nil, err
	}
	held := make([]coordinationv1.Lease, 0, len(leases.Items))
	for i := range leases.Items {
		lease := &leases.Items[i]
		if substratePoolActorLeaseHeldByTask(lease, task) {
			held = append(held, *lease)
		}
	}
	return held, nil
}

func (r *TaskReconciler) taskHasSubstratePoolActorLeases(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	leases, err := r.substratePoolActorLeasesForTask(ctx, task)
	if err != nil {
		return false, err
	}
	return len(leases) > 0, nil
}

func taskExecutionWorkspacePhase(task *corev1alpha1.Task) corev1alpha1.ExecutionWorkspacePhase {
	if task == nil || task.Status.ExecutionWorkspace == nil {
		return ""
	}
	return task.Status.ExecutionWorkspace.Phase
}

func taskExecutionWorkspaceReason(task *corev1alpha1.Task) corev1alpha1.ExecutionWorkspaceReason {
	if task == nil || task.Status.ExecutionWorkspace == nil {
		return ""
	}
	return task.Status.ExecutionWorkspace.Reason
}

// handleRunning handles Tasks in Running phase
func (r *TaskReconciler) handleRunning(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) { //nolint:gocyclo
	log := logf.FromContext(ctx)

	// Check timeout
	if task.Spec.Timeout != nil && task.Status.StartTime != nil {
		elapsed := time.Since(task.Status.StartTime.Time)
		if elapsed > task.Spec.Timeout.Duration {
			log.Info("task timed out", "elapsed", elapsed, "timeout", task.Spec.Timeout.Duration)
			if cancelErr := r.cancelHarnessWrapperTurn(ctx, task, "task timed out"); cancelErr != nil {
				log.Error(cancelErr, "failed to cancel timed-out harness runtime turn")
			}
			return r.failTask(ctx, task, "task timed out")
		}
	}

	if task.Spec.Type == corev1alpha1.TaskTypeAgent && taskHasHarnessWrapperTurn(task) {
		return r.finishHarnessWrapperTask(ctx, task)
	}
	if task.Spec.Type == corev1alpha1.TaskTypeAgent && strings.TrimSpace(task.Status.JobName) == "" {
		return r.failTask(ctx, task, "harness runtime turn identity is missing")
	}
	if _, usesWorkerRBAC, err := r.workerServiceAccountForActiveTask(ctx, task); err != nil {
		log.Error(err, "failed to classify running task worker RBAC")
		return ctrl.Result{}, err
	} else if usesWorkerRBAC {
		if err := r.ensureWorkerRBAC(ctx, task); err != nil {
			log.Error(err, "failed to repair worker RBAC for running task")
			return ctrl.Result{}, err
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
				if child.Status.ResultRef != nil && child.Status.ResultRef.Available && r.ResultStore != nil {
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
		if task.Spec.Timeout != nil && jobFailedDueToActiveDeadline(job) {
			return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, "task timed out")
		}
		// Job failed, check retry policy
		if r.shouldRetry(task) {
			log.Info("retrying task", "attempt", task.Status.Attempts)
			return r.retryTask(ctx, task)
		}
		// Inspect terminated containers for a specific cause (OOMKilled, non-zero
		// exit code, etc.) so the coordinator that delegated this Task can read
		// fetch_task_output and adapt — e.g. recreate the Agent with more memory.
		// Falls back to the generic "job failed" if no signal is available.
		msg := r.diagnoseFailedJob(ctx, task)
		return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, msg)
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
				if msg, ok, err := r.failedMountEventMessage(ctx, pod, task.Status.StartTime.Time); err != nil {
					return ctrl.Result{}, err
				} else if ok {
					msg = fmt.Sprintf("pod stuck initializing for over 2 minutes: %s", msg)
					log.Info("failing task due to failed pod mount", "message", msg)
					return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, msg)
				}
			}
		}
	}

	// Job still running, requeue
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func podWaitingForMountInitialization(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, cs := range statuses {
			if cs.State.Waiting == nil {
				continue
			}
			switch cs.State.Waiting.Reason {
			case "ContainerCreating", "PodInitializing":
				return true
			}
		}
	}
	return false
}

func eventObservedAt(event *corev1.Event) time.Time {
	if event == nil {
		return time.Time{}
	}
	if event.Series != nil && !event.Series.LastObservedTime.IsZero() {
		return event.Series.LastObservedTime.Time
	}
	if !event.EventTime.IsZero() {
		return event.EventTime.Time
	}
	if !event.LastTimestamp.IsZero() {
		return event.LastTimestamp.Time
	}
	if !event.FirstTimestamp.IsZero() {
		return event.FirstTimestamp.Time
	}
	if !event.CreationTimestamp.IsZero() {
		return event.CreationTimestamp.Time
	}
	return time.Time{}
}

func eventInvolvedObjectNameIndex(obj client.Object) []string {
	event, ok := obj.(*corev1.Event)
	if !ok || event.InvolvedObject.Name == "" {
		return nil
	}
	return []string{event.InvolvedObject.Name}
}

func eventReasonIndex(obj client.Object) []string {
	event, ok := obj.(*corev1.Event)
	if !ok || event.Reason == "" {
		return nil
	}
	return []string{event.Reason}
}

func (r *TaskReconciler) failedMountEventMessage(ctx context.Context, pod *corev1.Pod, since time.Time) (string, bool, error) {
	if pod == nil || !podWaitingForMountInitialization(pod) {
		return "", false, nil
	}

	var events corev1.EventList
	if err := r.List(ctx, &events,
		client.InNamespace(pod.Namespace),
		client.MatchingFields{
			eventInvolvedObjectNameField: pod.Name,
			eventReasonField:             "FailedMount",
		},
	); err != nil {
		return "", false, err
	}

	now := time.Now()
	for i := range events.Items {
		event := &events.Items[i]
		if event.Reason != "FailedMount" {
			continue
		}
		ref := event.InvolvedObject
		if ref.Kind != "Pod" || ref.Name != pod.Name {
			continue
		}
		if ref.UID != "" && pod.UID != "" && ref.UID != pod.UID {
			continue
		}
		observedAt := eventObservedAt(event)
		if observedAt.IsZero() || (!since.IsZero() && observedAt.Before(since)) {
			continue
		}
		if now.Sub(observedAt) > failedMountEventStaleAfter {
			continue
		}
		message := strings.TrimSpace(event.Message)
		if message == "" {
			message = "pod volume mount failed"
		}
		return message, true, nil
	}
	return "", false, nil
}

func jobFailedDueToActiveDeadline(job *batchv1.Job) bool {
	if job == nil {
		return false
	}

	for _, condition := range job.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		if condition.Reason != batchv1.JobReasonDeadlineExceeded {
			continue
		}
		if condition.Type == batchv1.JobFailed || condition.Type == batchv1.JobFailureTarget {
			return true
		}
	}

	return false
}

// diagnoseFailedJob inspects pods belonging to a failed Task's Job and returns a
// Status.Message that is specific enough for a coordinator LLM to act on.
//
// Priority of signals:
//  1. Any container terminated with reason=OOMKilled → "job failed: container
//     OOMKilled (memory limit <X> exceeded). Recreate the agent with higher
//     resources.limits.memory or set spec.resources on the task."
//  2. Any container terminated with a non-zero exit code → "job failed:
//     container exited with code <N> (reason=<R>)".
//  3. No signal available → the generic "job failed".
//
// Pod listing failures are non-fatal — we fall back to the generic message
// rather than block task completion.
func (r *TaskReconciler) diagnoseFailedJob(ctx context.Context, task *corev1alpha1.Task) string {
	log := logf.FromContext(ctx)
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(task.Namespace),
		client.MatchingLabels{labels.LabelTask: labels.SelectorValue(task.Name)}); err != nil {
		log.V(1).Info("diagnoseFailedJob: pod list failed, using generic message", "error", err.Error())
		return "job failed"
	}

	var (
		oomMsg  string
		exitMsg string
	)
	for i := range podList.Items {
		pod := &podList.Items[i]
		if task.Status.JobName != "" && !podBelongsToJob(pod, task.Status.JobName) {
			continue
		}
		// Worker pods only have one container; iterate defensively anyway.
		for _, cs := range pod.Status.ContainerStatuses {
			term := cs.State.Terminated
			if term == nil {
				// Also check LastTerminationState — pods that crashed and restarted
				// expose the relevant terminated state there.
				term = cs.LastTerminationState.Terminated
			}
			if term == nil {
				continue
			}
			if term.Reason == "OOMKilled" || term.ExitCode == 137 {
				limit := podContainerMemoryLimit(pod, cs.Name)
				if limit == "" {
					limit = "unknown"
				}
				oomMsg = fmt.Sprintf("job failed: container OOMKilled (memory limit %s exceeded). Recreate the agent with higher resources.limits.memory or set spec.resources on the task.", limit)
				continue
			}
			if term.ExitCode != 0 && exitMsg == "" {
				reason := term.Reason
				if reason == "" {
					reason = "Error"
				}
				exitMsg = fmt.Sprintf("job failed: container exited with code %d (reason=%s)", term.ExitCode, reason)
			}
		}
	}

	if oomMsg != "" {
		return oomMsg
	}
	if exitMsg != "" {
		return exitMsg
	}
	return "job failed"
}

func podBelongsToJob(pod *corev1.Pod, jobName string) bool {
	if pod == nil || strings.TrimSpace(jobName) == "" {
		return true
	}
	hasJobIdentity := false
	if got := pod.Labels[batchv1.JobNameLabel]; got != "" {
		hasJobIdentity = true
		if got == jobName {
			return true
		}
	}
	if got := pod.Labels["job-name"]; got != "" {
		hasJobIdentity = true
		if got == jobName {
			return true
		}
	}
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "Job" {
			hasJobIdentity = true
			if owner.Name == jobName {
				return true
			}
		}
	}
	return !hasJobIdentity
}

// podContainerMemoryLimit returns the memory limit configured on the named
// container as a string ("2Gi"), or "" if not set.
func podContainerMemoryLimit(pod *corev1.Pod, containerName string) string {
	if pod == nil {
		return ""
	}
	for _, c := range pod.Spec.Containers {
		if c.Name != containerName {
			continue
		}
		if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
			return q.String()
		}
	}
	return ""
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
	terminalEventRecorded := r.recordTerminalTaskLifecycleEventIfMissing(ctx, task)
	if task.Status.Phase == corev1alpha1.TaskPhaseCancelled {
		if cancelErr := r.cancelHarnessWrapperTurn(ctx, task, "task cancelled"); cancelErr != nil {
			log.Error(cancelErr, "failed to cancel harness runtime turn for cancelled task")
		}
	}

	waitingForJob, err := r.cleanupTerminalTaskJob(ctx, task)
	if err != nil {
		log.Error(err, "failed to clean up terminal task Job")
		return ctrl.Result{}, err
	}
	if waitingForJob {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	releasedPoolLeases, err := r.releaseSubstratePoolActorLeasesAfterTerminalCleanup(ctx, task)
	if err != nil {
		log.Error(err, "failed to release substrate pool actor leases")
		return ctrl.Result{}, err
	}
	if !releasedPoolLeases {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	var pruneErr error
	if err := r.pruneUnusedWorkerRBAC(ctx, task.Namespace, ""); err != nil {
		log.Error(err, "failed to prune unused worker RBAC for terminal task")
		pruneErr = err
	}

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
	if !terminalEventRecorded {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if pruneErr != nil {
		return ctrl.Result{}, pruneErr
	}

	return ctrl.Result{}, nil
}

func (r *TaskReconciler) cleanupDeletedTaskJob(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	if task.Status.JobName == "" {
		return false, nil
	}

	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: task.Status.JobName, Namespace: task.Namespace}, job); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("getting deleted task Job %q: %w", task.Status.JobName, err)
	}

	holdsPoolActor, err := r.taskHasSubstratePoolActorLeases(ctx, task)
	if err != nil {
		return false, err
	}
	propagationPolicy := metav1.DeletePropagationBackground
	if holdsPoolActor {
		propagationPolicy = metav1.DeletePropagationForeground
	}
	if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &propagationPolicy}); err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("deleting deleted task Job %q: %w", task.Status.JobName, err)
	}
	return holdsPoolActor, nil
}

func (r *TaskReconciler) cleanupTerminalTaskJob(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	if task.Status.JobName == "" {
		return false, nil
	}

	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: task.Status.JobName, Namespace: task.Namespace}, job); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("getting terminal task Job %q: %w", task.Status.JobName, err)
	}

	deleteJob := task.Status.Phase == corev1alpha1.TaskPhaseCancelled ||
		(task.Status.Phase == corev1alpha1.TaskPhaseFailed && job.Status.Active > 0)
	if !deleteJob {
		return false, nil
	}

	holdsPoolActor, err := r.taskHasSubstratePoolActorLeases(ctx, task)
	if err != nil {
		return false, err
	}
	propagationPolicy := metav1.DeletePropagationBackground
	if holdsPoolActor {
		propagationPolicy = metav1.DeletePropagationForeground
	}
	if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &propagationPolicy}); err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("deleting terminal task Job %q: %w", task.Status.JobName, err)
	}

	return holdsPoolActor, nil
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
	terminalEventErr := r.recordTaskLifecycleEvent(
		ctx,
		task,
		executionEventTypeForTaskPhase(phase),
		executionEventSeverityForTaskPhase(phase),
		message,
	)

	// Update the Agent's LastUsed timestamp so TTL tracking works
	if task.Spec.AgentRef != nil {
		if err := r.updateAgentLastUsed(ctx, task.Namespace, task.Spec.AgentRef.Name, now); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "failed to update agent LastUsed")
		}
	}
	if terminalEventErr != nil {
		return ctrl.Result{RequeueAfter: time.Second}, nil
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
	holdsPoolActor, err := r.taskHasSubstratePoolActorLeases(ctx, task)
	if err != nil {
		return ctrl.Result{}, err
	}
	if holdsPoolActor {
		waitingForJob, err := r.cleanupRetryTaskJob(ctx, task, oldJobName)
		if err != nil {
			log.Error(err, "failed to delete old Job for pooled retry")
			return ctrl.Result{}, err
		}
		if waitingForJob {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		releasedPoolLeases, err := r.releaseSubstratePoolActorLeasesAfterTerminalCleanup(ctx, task)
		if err != nil {
			log.Error(err, "failed to release substrate pool actor leases for retry")
			return ctrl.Result{}, err
		}
		if !releasedPoolLeases {
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
	}

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
	if oldJobName != "" && !holdsPoolActor {
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

func (r *TaskReconciler) cleanupRetryTaskJob(ctx context.Context, task *corev1alpha1.Task, jobName string) (bool, error) {
	if jobName == "" {
		return false, nil
	}

	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: task.Namespace}, job); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("getting retry task Job %q: %w", jobName, err)
	}

	holdsPoolActor, err := r.taskHasSubstratePoolActorLeases(ctx, task)
	if err != nil {
		return false, err
	}
	propagationPolicy := metav1.DeletePropagationBackground
	if holdsPoolActor {
		propagationPolicy = metav1.DeletePropagationForeground
	}
	if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &propagationPolicy}); err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("deleting retry task Job %q: %w", jobName, err)
	}
	return holdsPoolActor, nil
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
	if r.ResultStore == nil {
		return nil
	}

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

	// No result yet — capture pod logs for tasks that actually created a Job.
	// Some validation failures happen before Job creation; those terminal tasks should
	// not produce noisy best-effort log collection errors for a non-existent Job.
	stdoutResult := taskUsesStdoutResult(task)
	if (task.Spec.Type != corev1alpha1.TaskTypeContainer && !stdoutResult) || r.KubeClient == nil || task.Status.JobName == "" {
		return nil
	}

	var result []byte
	if stdoutResult {
		logs, err := r.readStdoutResultPodLogs(ctx, task)
		if err != nil {
			return fmt.Errorf("reading stdout result pod logs: %w", err)
		}
		stdoutPayload, ok, decodeErr := extractStdoutTaskResult(logs)
		if decodeErr != nil {
			return decodeErr
		}
		if !ok {
			return nil
		}
		result = stdoutPayload
	} else {
		logs, err := r.readPodLogs(ctx, task)
		if err != nil {
			return fmt.Errorf("reading pod logs: %w", err)
		}
		result = []byte(logs)
	}

	if err := r.ResultStore.SaveResult(ctx, task.Namespace, task.Name, result); err != nil {
		return fmt.Errorf("saving result: %w", err)
	}

	task.Status.ResultRef = &corev1alpha1.ResultReference{
		Available: true,
	}

	return nil
}

func taskUsesStdoutResult(task *corev1alpha1.Task) bool {
	return taskRequestsReadOnlyAgent(task)
}

func extractStdoutTaskResult(logs string) ([]byte, bool, error) {
	var payload string
	for line := range strings.SplitSeq(logs, "\n") {
		line = strings.TrimSpace(line)
		if encoded, ok := strings.CutPrefix(line, workerenv.ResultStdoutPrefix); ok {
			payload = strings.TrimSpace(encoded)
		}
	}
	if payload == "" {
		return nil, false, nil
	}
	result, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, true, fmt.Errorf("decoding stdout task result: %w", err)
	}
	return result, true, nil
}

// readPodLogs reads logs from the first pod of a task's job.
func (r *TaskReconciler) readPodLogs(ctx context.Context, task *corev1alpha1.Task) (string, error) {
	return r.readPodLogsWithOptions(ctx, task, fullPodLogOptions(), true)
}

func (r *TaskReconciler) readStdoutResultPodLogs(ctx context.Context, task *corev1alpha1.Task) (string, error) {
	return r.readPodLogsWithOptions(ctx, task, stdoutResultPodLogOptions(), false)
}

func fullPodLogOptions() corev1.PodLogOptions {
	limit := podLogLimitBytes
	return corev1.PodLogOptions{
		LimitBytes: &limit,
	}
}

func stdoutResultPodLogOptions() corev1.PodLogOptions {
	limit := stdoutResultLogLimitBytes
	return corev1.PodLogOptions{
		LimitBytes: &limit,
	}
}

func (r *TaskReconciler) readPodLogsWithOptions(ctx context.Context, task *corev1alpha1.Task, opts corev1.PodLogOptions, appendTruncatedMarker bool) (string, error) {
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

	pod := podList.Items[len(podList.Items)-1]
	req := r.KubeClient.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &opts)
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("streaming logs: %w", err)
	}
	defer stream.Close() //nolint:errcheck

	limit := podLogLimitBytes
	if opts.LimitBytes != nil && *opts.LimitBytes > 0 {
		limit = *opts.LimitBytes
	}
	data, err := io.ReadAll(io.LimitReader(stream, limit))
	if err != nil {
		return "", fmt.Errorf("reading logs: %w", err)
	}

	if appendTruncatedMarker && int64(len(data)) == limit {
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

// validateExecutionWorkspace validates optional durable workspace settings.
func (r *TaskReconciler) validateExecutionWorkspace(task *corev1alpha1.Task) error {
	if task.Spec.Execution == nil || task.Spec.Execution.Workspace == nil || !task.Spec.Execution.Workspace.Enabled {
		return nil
	}

	ws := task.Spec.Execution.Workspace
	provider := resolveWorkspaceProvider(ws, r.ExecutionWorkspaceDefaultProvider)

	if err := validateExecutionWorkspaceBasics(task, provider); err != nil {
		return err
	}
	if err := validateExecutionWorkspaceSubstrateOptions(ws, provider); err != nil {
		return err
	}
	if err := r.validateExecutionWorkspaceProviderConfig(ws, provider); err != nil {
		return err
	}
	return validateExecutionWorkspacePolicies(task, ws)
}

func validateExecutionWorkspaceBasics(
	task *corev1alpha1.Task,
	provider corev1alpha1.WorkspaceProvider,
) error {
	if !supportedWorkspaceProvider(provider) {
		return fmt.Errorf("unsupported execution workspace provider %q", provider)
	}
	if task.Spec.Type != corev1alpha1.TaskTypeAgent {
		return fmt.Errorf("execution workspace is only supported for type: agent tasks")
	}
	return nil
}

func validateExecutionWorkspaceSubstrateOptions(
	ws *corev1alpha1.ExecutionWorkspaceSpec,
	provider corev1alpha1.WorkspaceProvider,
) error {
	if ws.Boot && provider != corev1alpha1.WorkspaceProviderSubstrate {
		return fmt.Errorf("execution workspace boot is only supported for provider %q", corev1alpha1.WorkspaceProviderSubstrate)
	}
	if ws.PoolRef != nil && provider != corev1alpha1.WorkspaceProviderSubstrate {
		return fmt.Errorf("execution workspace poolRef is only supported for provider %q", corev1alpha1.WorkspaceProviderSubstrate)
	}
	if ws.Snapshot != nil && provider != corev1alpha1.WorkspaceProviderSubstrate {
		return fmt.Errorf("execution workspace snapshot is only supported for provider %q", corev1alpha1.WorkspaceProviderSubstrate)
	}
	if ws.Hibernation != nil && provider != corev1alpha1.WorkspaceProviderSubstrate {
		return fmt.Errorf("execution workspace hibernation is only supported for provider %q", corev1alpha1.WorkspaceProviderSubstrate)
	}
	if ws.PoolRef != nil && strings.TrimSpace(ws.PoolRef.Name) == "" {
		return fmt.Errorf("execution workspace poolRef.name is required")
	}
	if ws.Snapshot != nil {
		if strings.TrimSpace(ws.Snapshot.RestoreURI) != "" ||
			strings.TrimSpace(ws.Snapshot.CheckpointURI) != "" ||
			ws.Snapshot.CheckpointOnRelease {
			return fmt.Errorf("execution workspace snapshot restore/checkpoint is not supported yet")
		}
	}
	if ws.Hibernation != nil {
		switch ws.Hibernation.ProcessMode {
		case "", corev1alpha1.ExecutionWorkspaceProcessModeFresh:
		case corev1alpha1.ExecutionWorkspaceProcessModeResident:
			return fmt.Errorf("execution workspace hibernation processMode %q is not supported yet", ws.Hibernation.ProcessMode)
		default:
			return fmt.Errorf("unsupported execution workspace hibernation processMode %q", ws.Hibernation.ProcessMode)
		}
	}

	return nil
}

func (r *TaskReconciler) validateExecutionWorkspaceProviderConfig(
	ws *corev1alpha1.ExecutionWorkspaceSpec,
	provider corev1alpha1.WorkspaceProvider,
) error {
	switch provider {
	case corev1alpha1.WorkspaceProviderAgentSandbox:
		if !r.AgentSandboxEnabled {
			return fmt.Errorf("execution workspace provider %q requires agent sandbox to be enabled", provider)
		}
		cfg := r.AgentSandboxConfig.WithDefaults()
		if err := cfg.Validate(); err != nil {
			return err
		}
		if executionWorkspaceTemplateName(ws, cfg) == "" {
			return fmt.Errorf("execution workspace templateRef.name is required when no agent sandbox default template is configured")
		}
	case corev1alpha1.WorkspaceProviderSubstrate:
		if !r.SubstrateEnabled {
			return fmt.Errorf("execution workspace provider %q requires substrate to be enabled", provider)
		}
		cfg := r.SubstrateConfig.WithDefaults()
		if err := cfg.Validate(); err != nil {
			return err
		}
		if substrateTemplateName(ws, cfg) == "" {
			return fmt.Errorf("execution workspace templateRef.name is required when no substrate default template is configured")
		}
	}
	return nil
}

func validateExecutionWorkspacePolicies(task *corev1alpha1.Task, ws *corev1alpha1.ExecutionWorkspaceSpec) error {
	if !statusrules.IsOptionalReusePolicy(ws.ReusePolicy) {
		return fmt.Errorf("unsupported execution workspace reusePolicy %q", ws.ReusePolicy)
	}

	if !statusrules.IsOptionalCleanupPolicy(ws.CleanupPolicy) {
		return fmt.Errorf("unsupported execution workspace cleanupPolicy %q", ws.CleanupPolicy)
	}
	if ws.PoolRef != nil && ws.CleanupPolicy == corev1alpha1.WorkspaceCleanupPolicyRetain {
		return fmt.Errorf("execution workspace poolRef does not support cleanupPolicy %q until substrate workspace reset is available", ws.CleanupPolicy)
	}

	if ws.ReusePolicy == corev1alpha1.WorkspaceReusePolicySession && (task.Spec.SessionRef == nil || task.Spec.SessionRef.Name == "") {
		return fmt.Errorf("execution workspace reusePolicy %q requires spec.sessionRef.name", ws.ReusePolicy)
	}

	return nil
}

func (r *TaskReconciler) markExecutionWorkspaceValidationFailed(ctx context.Context, task *corev1alpha1.Task, validationErr error) error {
	if task == nil || task.Spec.Execution == nil || task.Spec.Execution.Workspace == nil || !task.Spec.Execution.Workspace.Enabled {
		return nil
	}

	now := metav1.Now()
	message := ""
	if validationErr != nil {
		message = validationErr.Error()
	}
	ws := task.Spec.Execution.Workspace
	provider := resolveWorkspaceProvider(ws, r.ExecutionWorkspaceDefaultProvider)
	failure := statusrules.ValidationFailure{
		Message:    message,
		ObservedAt: &now,
	}
	if supportedWorkspaceProvider(provider) {
		failure.Provider = provider
		failure.TemplateRef = r.executionWorkspaceStatusTemplateRef(task, provider)
	}
	if reusePolicy, ok := executionWorkspaceStatusReusePolicy(ws); ok {
		failure.ReusePolicy = reusePolicy
	}
	if cleanupPolicy, ok := r.executionWorkspaceStatusCleanupPolicy(ws, provider); ok {
		failure.CleanupPolicy = cleanupPolicy
	}
	status := statusrules.ValidationFailedStatus(failure)

	return r.updateStatusWithRetry(ctx, task, func(t *corev1alpha1.Task) {
		t.Status.ExecutionWorkspace = status
	})
}

func (r *TaskReconciler) executionWorkspaceStatusTemplateRef(task *corev1alpha1.Task, provider corev1alpha1.WorkspaceProvider) *corev1alpha1.WorkspaceTemplateReference {
	ws := task.Spec.Execution.Workspace
	var name string
	var namespace string
	switch provider {
	case corev1alpha1.WorkspaceProviderAgentSandbox:
		cfg := r.AgentSandboxConfig.WithDefaults()
		name = executionWorkspaceTemplateName(ws, cfg)
		namespace = executionWorkspaceTemplateNamespace(ws, task.Namespace, cfg)
	case corev1alpha1.WorkspaceProviderSubstrate:
		cfg := r.SubstrateConfig.WithDefaults()
		name = substrateTemplateName(ws, cfg)
		namespace = substrateTemplateNamespace(ws, task.Namespace, cfg)
	default:
		return nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	return &corev1alpha1.WorkspaceTemplateReference{
		Name:      name,
		Namespace: strings.TrimSpace(namespace),
	}
}

func executionWorkspaceStatusReusePolicy(ws *corev1alpha1.ExecutionWorkspaceSpec) (corev1alpha1.WorkspaceReusePolicy, bool) {
	if ws == nil || ws.ReusePolicy == "" {
		return corev1alpha1.WorkspaceReusePolicyNone, true
	}
	switch ws.ReusePolicy {
	case corev1alpha1.WorkspaceReusePolicyNone, corev1alpha1.WorkspaceReusePolicySession:
		return ws.ReusePolicy, true
	default:
		return "", false
	}
}

func (r *TaskReconciler) executionWorkspaceStatusCleanupPolicy(ws *corev1alpha1.ExecutionWorkspaceSpec, provider corev1alpha1.WorkspaceProvider) (corev1alpha1.WorkspaceCleanupPolicy, bool) {
	if ws != nil && ws.CleanupPolicy != "" {
		switch ws.CleanupPolicy {
		case corev1alpha1.WorkspaceCleanupPolicyDelete, corev1alpha1.WorkspaceCleanupPolicyRetain:
			return ws.CleanupPolicy, true
		default:
			return "", false
		}
	}
	switch provider {
	case corev1alpha1.WorkspaceProviderAgentSandbox:
		return executionWorkspaceStatusValidCleanupPolicy(r.AgentSandboxConfig.WithDefaults().CleanupPolicy)
	case corev1alpha1.WorkspaceProviderSubstrate:
		return executionWorkspaceStatusValidCleanupPolicy(r.SubstrateConfig.WithDefaults().CleanupPolicy)
	default:
		return corev1alpha1.WorkspaceCleanupPolicyDelete, true
	}
}

func executionWorkspaceStatusValidCleanupPolicy(cleanupPolicy corev1alpha1.WorkspaceCleanupPolicy) (corev1alpha1.WorkspaceCleanupPolicy, bool) {
	return statusrules.StatusCleanupPolicy(cleanupPolicy, "")
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
		switch agent.Spec.Runtime.Type {
		case corev1alpha1.AgentRuntimeCodex, corev1alpha1.AgentRuntimeClaude, corev1alpha1.AgentRuntimeCopilot:
		default:
			return fmt.Errorf("agent runtime %q does not have a harness adapter configured", agent.Spec.Runtime.Type)
		}
		if taskRequestsReadOnlyAgent(task) {
			switch agent.Spec.Runtime.Type {
			case corev1alpha1.AgentRuntimeCodex:
				return fmt.Errorf("read-only agent tasks do not support codex runtime because Codex requires shell access while model credentials are exposed")
			case corev1alpha1.AgentRuntimeCopilot:
				return fmt.Errorf("read-only agent tasks do not support copilot runtime because GitHub tokens can allow repository mutation")
			}
		}
		if agent.Spec.Execution != nil && agent.Spec.Execution.Workspace != nil && agent.Spec.Execution.Workspace.Enabled {
			return fmt.Errorf("agent %q sets spec.execution.workspace, but execution workspace requests are only supported on Task.spec.execution.workspace", agent.Name)
		}
		// Agent with runtime must not have providerRef (mutually exclusive)
		if agent.Spec.ProviderRef != nil {
			return fmt.Errorf("agent %q has both runtime and providerRef set (mutually exclusive)", agent.Name)
		}
		// Agent with runtime must not have a model provider set
		if agent.Spec.Model != nil && agent.Spec.Model.Provider != "" {
			return fmt.Errorf("agent %q has both runtime and model.provider set (mutually exclusive for agent tasks)", agent.Name)
		}
		if err := validateHarnessWrapperTaskEnv(task.Spec.Env); err != nil {
			return err
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
	childAnnotations := map[string]string{
		labels.AnnotationParentTaskName: task.Name,
	}
	if task.Annotations[labels.AnnotationDisableCoordinationToolInject] == scheduledRunLabelValue {
		childAnnotations[labels.AnnotationDisableCoordinationToolInject] = scheduledRunLabelValue
	}
	child := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      childName,
			Namespace: task.Namespace,
			Labels: map[string]string{
				labels.LabelParentTask:   labels.SelectorValue(task.Name),
				labels.LabelScheduledRun: scheduledRunLabelValue,
			},
			Annotations: childAnnotations,
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
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Event{}, eventInvolvedObjectNameField, eventInvolvedObjectNameIndex); err != nil {
		return err
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Event{}, eventReasonField, eventReasonIndex); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Task{}).
		Owns(&batchv1.Job{}).
		Owns(&corev1alpha1.Task{}).
		Named("task").
		Complete(r)
}

const (
	DefaultAIWorkerClusterRoleName        = "orka-ai-worker-role"
	DefaultVendorWorkerClusterRoleName    = "orka-vendor-worker-role"
	DefaultContainerWorkerClusterRoleName = "orka-container-worker-role"

	maxWorkerClusterRoleBindingNameLength = 253
	workerClusterRoleBindingHashLength    = 10

	managedByLabelKey         = "app.kubernetes.io/managed-by"
	managedByLabelValue       = "orka"
	orkaManagedByLabelKey     = "orka.ai/managed-by"
	workerRBACOwnedLabelKey   = "orka.ai/worker-rbac-owned"
	workerRBACOwnedLabelValue = "true"
)

type workerRBACSpec struct {
	serviceAccountName     string
	clusterRoleName        string
	clusterRoleBindingName string
}

// workerRBACSpecs binds cluster-scoped worker roles into each task namespace.
// The AI worker role is intentionally broader because code_exec's Kubernetes
// backend creates per-job ServiceAccounts and Secrets; vendor and container
// workers use separate, narrower roles so those cluster-wide capabilities are
// not shared with less-trusted task types.
func (r *TaskReconciler) workerRBACSpecs(namespace string) []workerRBACSpec {
	return []workerRBACSpec{
		{
			serviceAccountName:     AIWorkerServiceAccount,
			clusterRoleName:        workerClusterRoleName(r.AIWorkerClusterRoleName, DefaultAIWorkerClusterRoleName),
			clusterRoleBindingName: workerClusterRoleBindingName(r.WorkerClusterRoleBindingNamePrefix, "ai", namespace),
		},
		{
			serviceAccountName:     VendorWorkerServiceAccount,
			clusterRoleName:        workerClusterRoleName(r.VendorWorkerClusterRoleName, DefaultVendorWorkerClusterRoleName),
			clusterRoleBindingName: workerClusterRoleBindingName(r.WorkerClusterRoleBindingNamePrefix, "vendor", namespace),
		},
		{
			serviceAccountName:     ContainerWorkerServiceAccount,
			clusterRoleName:        workerClusterRoleName(r.ContainerWorkerClusterRoleName, DefaultContainerWorkerClusterRoleName),
			clusterRoleBindingName: workerClusterRoleBindingName(r.WorkerClusterRoleBindingNamePrefix, "container", namespace),
		},
	}
}

func workerClusterRoleName(configured, fallback string) string {
	if configured != "" {
		return configured
	}
	return fallback
}

func workerClusterRoleBindingName(prefix, tier, namespace string) string {
	if prefix == "" {
		prefix = managedByLabelValue
	}
	name := fmt.Sprintf("%s-%s-worker-%s", prefix, tier, namespace)
	if len(name) <= maxWorkerClusterRoleBindingNameLength {
		return name
	}

	sum := sha256.Sum256([]byte(name))
	suffix := hex.EncodeToString(sum[:])[:workerClusterRoleBindingHashLength]
	prefixLength := maxWorkerClusterRoleBindingNameLength - workerClusterRoleBindingHashLength - 1
	return fmt.Sprintf("%s-%s", name[:prefixLength], suffix)
}

// workerRBACSpecForTask returns the single worker identity required by the task's trust tier.
func (r *TaskReconciler) workerRBACSpecForTask(task *corev1alpha1.Task) workerRBACSpec {
	namespace := ""
	if task != nil {
		namespace = task.Namespace
	}

	for _, spec := range r.workerRBACSpecs(namespace) {
		if spec.serviceAccountName == workerServiceAccountForTask(task) {
			return spec
		}
	}

	return workerRBACSpec{
		serviceAccountName:     ContainerWorkerServiceAccount,
		clusterRoleName:        workerClusterRoleName(r.ContainerWorkerClusterRoleName, DefaultContainerWorkerClusterRoleName),
		clusterRoleBindingName: workerClusterRoleBindingName(r.WorkerClusterRoleBindingNamePrefix, "container", namespace),
	}
}

// ensureWorkerRBAC ensures the ServiceAccount and role binding required
// by this task's trust tier exist in the task namespace.
func (r *TaskReconciler) ensureWorkerRBAC(ctx context.Context, task *corev1alpha1.Task) error {
	if task == nil {
		return fmt.Errorf("task is required to ensure worker RBAC")
	}

	spec := r.workerRBACSpecForTask(task)
	if err := r.ensureWorkerServiceAccount(ctx, task.Namespace, spec.serviceAccountName); err != nil {
		return err
	}
	if r.EnforceNamespaceIsolation {
		if err := r.ensureWorkerRoleBinding(ctx, task.Namespace, spec); err != nil {
			return err
		}
		if err := r.deleteLegacyWorkerClusterRoleBinding(ctx, task.Namespace, spec); err != nil {
			return err
		}
	} else if err := r.ensureWorkerClusterRoleBinding(ctx, task.Namespace, spec); err != nil {
		return err
	}
	if err := r.pruneUnusedWorkerRBAC(ctx, task.Namespace, spec.serviceAccountName); err != nil {
		return err
	}

	return nil
}

func (r *TaskReconciler) pruneUnusedWorkerRBAC(ctx context.Context, namespace, selectedServiceAccount string) error {
	keepServiceAccounts, repairBindings, err := r.workerServiceAccountUsage(ctx, namespace)
	if err != nil {
		return err
	}
	if selectedServiceAccount != "" {
		keepServiceAccounts[selectedServiceAccount] = struct{}{}
		repairBindings[selectedServiceAccount] = struct{}{}
	}

	for _, spec := range r.workerRBACSpecs(namespace) {
		if _, ok := repairBindings[spec.serviceAccountName]; ok {
			if err := r.ensureWorkerServiceAccount(ctx, namespace, spec.serviceAccountName); err != nil {
				return err
			}
			if r.EnforceNamespaceIsolation {
				if err := r.ensureWorkerRoleBinding(ctx, namespace, spec); err != nil {
					return err
				}
				if err := r.deleteLegacyWorkerClusterRoleBinding(ctx, namespace, spec); err != nil {
					return err
				}
			} else if err := r.ensureWorkerClusterRoleBinding(ctx, namespace, spec); err != nil {
				return err
			}
			if err := r.deleteLegacyStaticWorkerClusterRoleBinding(ctx, namespace, spec); err != nil {
				return err
			}
			continue
		}
		if err := r.deleteLegacyStaticWorkerClusterRoleBinding(ctx, namespace, spec); err != nil {
			return err
		}
		if err := r.deleteManagedWorkerRoleBinding(ctx, namespace, spec); err != nil {
			return err
		}
		if err := r.deleteManagedWorkerClusterRoleBinding(ctx, namespace, spec); err != nil {
			return err
		}
		if _, ok := keepServiceAccounts[spec.serviceAccountName]; ok {
			continue
		}
		if err := r.deleteManagedWorkerServiceAccount(ctx, namespace, spec.serviceAccountName); err != nil {
			return err
		}
	}

	return nil
}

func (r *TaskReconciler) workerServiceAccountUsage(ctx context.Context, namespace string) (map[string]struct{}, map[string]struct{}, error) {
	keepServiceAccounts := map[string]struct{}{}
	repairBindings := map[string]struct{}{}

	var tasks corev1alpha1.TaskList
	if err := r.List(ctx, &tasks, client.InNamespace(namespace)); err != nil {
		return nil, nil, fmt.Errorf("listing tasks in namespace %s for worker RBAC pruning: %w", namespace, err)
	}
	for i := range tasks.Items {
		task := &tasks.Items[i]
		if !task.DeletionTimestamp.IsZero() {
			continue
		}
		if task.Status.Phase == corev1alpha1.TaskPhasePending || task.Status.Phase == corev1alpha1.TaskPhaseRunning {
			serviceAccount, usesWorkerRBAC, err := r.workerServiceAccountForActiveTask(ctx, task)
			if err != nil {
				return nil, nil, err
			}
			if !usesWorkerRBAC {
				continue
			}
			keepServiceAccounts[serviceAccount] = struct{}{}
			repairBindings[serviceAccount] = struct{}{}
		}
	}

	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(namespace)); err != nil {
		return nil, nil, fmt.Errorf("listing pods in namespace %s for worker RBAC pruning: %w", namespace, err)
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.ServiceAccountName == "" {
			continue
		}
		if pod.Status.Phase == corev1.PodPending || pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodUnknown {
			keepServiceAccounts[pod.Spec.ServiceAccountName] = struct{}{}
		}
	}

	return keepServiceAccounts, repairBindings, nil
}

func (r *TaskReconciler) workerServiceAccountForActiveTask(ctx context.Context, task *corev1alpha1.Task) (string, bool, error) {
	if task == nil {
		return "", false, nil
	}
	if task.Spec.Type != corev1alpha1.TaskTypeAgent {
		return workerServiceAccountForTask(task), true, nil
	}
	if ok, err := r.agentTaskHasWorkerJob(ctx, task); err != nil {
		return "", false, err
	} else if ok {
		return workerServiceAccountForTask(task), true, nil
	}

	if task.Spec.AgentRef == nil {
		return "", false, nil
	}

	agent := &corev1alpha1.Agent{}
	agentNS := task.Spec.AgentRef.Namespace
	if agentNS == "" {
		agentNS = task.Namespace
	}
	if r.EnforceNamespaceIsolation && agentNS != task.Namespace {
		return "", false, nil
	}
	if err := r.Get(ctx, types.NamespacedName{Name: task.Spec.AgentRef.Name, Namespace: agentNS}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("getting agent %s/%s for worker RBAC liveness: %w", agentNS, task.Spec.AgentRef.Name, err)
	}
	if agent.Spec.Runtime != nil || agentTaskJobBackendUnsupportedReason(task, agent) != "" {
		return "", false, nil
	}
	return workerServiceAccountForTask(task), true, nil
}

func (r *TaskReconciler) agentTaskHasWorkerJob(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	if task == nil {
		return false, nil
	}
	jobName := strings.TrimSpace(task.Status.JobName)
	if jobName == "" {
		return false, nil
	}
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: jobName, Namespace: task.Namespace}, job); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("getting agent worker Job %s/%s for RBAC liveness: %w", task.Namespace, jobName, err)
	}
	for _, owner := range job.OwnerReferences {
		if owner.Kind == "Task" && owner.Name == task.Name && owner.UID == task.UID {
			return true, nil
		}
	}
	return false, nil
}

func (r *TaskReconciler) deleteManagedWorkerRoleBinding(ctx context.Context, namespace string, spec workerRBACSpec) error {
	log := logf.FromContext(ctx)

	rb := &rbacv1.RoleBinding{}
	err := r.Get(ctx, types.NamespacedName{Name: spec.clusterRoleBindingName, Namespace: namespace}, rb)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting stale worker RoleBinding %s/%s for pruning: %w", namespace, spec.clusterRoleBindingName, err)
	}
	if rb.Labels[managedByLabelKey] != managedByLabelValue {
		return nil
	}

	if err := r.Delete(ctx, rb); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting stale worker RoleBinding %s/%s: %w", namespace, spec.clusterRoleBindingName, err)
	}
	log.Info("Deleted stale worker RoleBinding", "namespace", namespace, "binding", spec.clusterRoleBindingName, "serviceAccount", spec.serviceAccountName)
	return nil
}

func (r *TaskReconciler) deleteLegacyStaticWorkerClusterRoleBinding(ctx context.Context, namespace string, spec workerRBACSpec) error {
	tier := workerTrustTierForServiceAccount(spec.serviceAccountName)
	if tier == "" {
		return nil
	}
	prefix := r.WorkerClusterRoleBindingNamePrefix
	if prefix == "" {
		prefix = managedByLabelValue
	}
	for _, name := range []string{
		fmt.Sprintf("%s-%s-worker-rolebinding", managedByLabelValue, tier),
		fmt.Sprintf("%s-%s-worker-rolebinding", prefix, tier),
		fmt.Sprintf("%s-worker-rolebinding", tier),
	} {
		if name == spec.clusterRoleBindingName {
			continue
		}
		if err := r.deleteLegacyWorkerClusterRoleBindingByName(ctx, namespace, spec, name); err != nil {
			return err
		}
	}
	return nil
}

func workerTrustTierForServiceAccount(serviceAccountName string) string {
	switch serviceAccountName {
	case AIWorkerServiceAccount:
		return "ai"
	case VendorWorkerServiceAccount:
		return "vendor"
	case ContainerWorkerServiceAccount:
		return "container"
	default:
		return ""
	}
}

func (r *TaskReconciler) deleteLegacyWorkerClusterRoleBindingByName(ctx context.Context, namespace string, spec workerRBACSpec, name string) error {
	log := logf.FromContext(ctx)
	legacy := &rbacv1.ClusterRoleBinding{}
	err := r.Get(ctx, types.NamespacedName{Name: name}, legacy)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting legacy static worker ClusterRoleBinding %s: %w", name, err)
	}

	subjectNamespaces := r.legacyStaticWorkerClusterRoleBindingSubjectNamespaces(legacy, spec)
	if len(subjectNamespaces) == 0 {
		return nil
	}
	for _, subjectNamespace := range subjectNamespaces {
		if err := r.ensureLegacyStaticWorkerReplacement(ctx, subjectNamespace, spec); err != nil {
			return err
		}
	}

	if err := r.Delete(ctx, legacy); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting legacy static worker ClusterRoleBinding %s: %w", name, err)
	}
	log.Info("Deleted legacy static worker ClusterRoleBinding", "namespace", namespace, "binding", name, "serviceAccount", spec.serviceAccountName, "subjectNamespaces", subjectNamespaces)
	return nil
}

func (r *TaskReconciler) legacyStaticWorkerClusterRoleBindingSubjectNamespaces(crb *rbacv1.ClusterRoleBinding, spec workerRBACSpec) []string {
	if crb == nil {
		return nil
	}
	if crb.RoleRef.APIGroup != rbacv1.GroupName || crb.RoleRef.Kind != "ClusterRole" || crb.RoleRef.Name != spec.clusterRoleName {
		return nil
	}
	tier := workerTrustTierForServiceAccount(spec.serviceAccountName)
	namespaces := make([]string, 0, len(crb.Subjects))
	seen := map[string]struct{}{}
	for _, subject := range crb.Subjects {
		if subject.Kind != rbacv1.ServiceAccountKind || subject.Name != spec.serviceAccountName {
			continue
		}
		if crb.Labels[managedByLabelKey] == managedByLabelValue && crb.Name == workerClusterRoleBindingName(r.WorkerClusterRoleBindingNamePrefix, tier, subject.Namespace) {
			continue
		}
		if _, ok := seen[subject.Namespace]; ok {
			continue
		}
		seen[subject.Namespace] = struct{}{}
		namespaces = append(namespaces, subject.Namespace)
	}
	return namespaces
}

func (r *TaskReconciler) ensureLegacyStaticWorkerReplacement(ctx context.Context, namespace string, spec workerRBACSpec) error {
	if namespace == "" {
		return nil
	}
	keepServiceAccounts, _, err := r.workerServiceAccountUsage(ctx, namespace)
	if err != nil {
		return fmt.Errorf("checking legacy static worker ClusterRoleBinding subject namespace %s: %w", namespace, err)
	}
	if _, ok := keepServiceAccounts[spec.serviceAccountName]; !ok {
		return nil
	}
	replacementSpec, ok := r.workerRBACSpecForServiceAccount(namespace, spec.serviceAccountName)
	if !ok {
		return nil
	}
	if err := r.ensureWorkerServiceAccount(ctx, namespace, replacementSpec.serviceAccountName); err != nil {
		return err
	}
	if r.EnforceNamespaceIsolation {
		if err := r.ensureWorkerRoleBinding(ctx, namespace, replacementSpec); err != nil {
			return err
		}
		return r.deleteLegacyWorkerClusterRoleBinding(ctx, namespace, replacementSpec)
	}
	return r.ensureWorkerClusterRoleBinding(ctx, namespace, replacementSpec)
}

func (r *TaskReconciler) workerRBACSpecForServiceAccount(namespace, serviceAccountName string) (workerRBACSpec, bool) {
	for _, spec := range r.workerRBACSpecs(namespace) {
		if spec.serviceAccountName == serviceAccountName {
			return spec, true
		}
	}
	return workerRBACSpec{}, false
}

func (r *TaskReconciler) deleteManagedWorkerClusterRoleBinding(ctx context.Context, namespace string, spec workerRBACSpec) error {
	log := logf.FromContext(ctx)

	crb := &rbacv1.ClusterRoleBinding{}
	err := r.Get(ctx, types.NamespacedName{Name: spec.clusterRoleBindingName}, crb)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting stale worker ClusterRoleBinding %s for pruning: %w", spec.clusterRoleBindingName, err)
	}
	if crb.Labels[managedByLabelKey] != managedByLabelValue {
		return nil
	}

	if err := r.Delete(ctx, crb); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting stale worker ClusterRoleBinding %s for namespace %s: %w", spec.clusterRoleBindingName, namespace, err)
	}
	log.Info("Deleted stale worker ClusterRoleBinding", "namespace", namespace, "binding", spec.clusterRoleBindingName, "serviceAccount", spec.serviceAccountName)
	return nil
}

func (r *TaskReconciler) deleteManagedWorkerServiceAccount(ctx context.Context, namespace, name string) error {
	log := logf.FromContext(ctx)

	sa := &corev1.ServiceAccount{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, sa)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting stale worker ServiceAccount %s/%s for pruning: %w", namespace, name, err)
	}
	if sa.Labels[orkaManagedByLabelKey] != managedByLabelValue {
		return nil
	}
	if !workerServiceAccountOwnedByOrka(sa) {
		return nil
	}

	if err := r.Delete(ctx, sa); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting stale worker ServiceAccount %s/%s: %w", namespace, name, err)
	}
	log.Info("Deleted stale worker ServiceAccount", "namespace", namespace, "serviceAccount", name)
	return nil
}

func workerServiceAccountOwnedByOrka(sa *corev1.ServiceAccount) bool {
	if sa == nil || sa.Labels[orkaManagedByLabelKey] != managedByLabelValue {
		return false
	}
	if sa.Labels[workerRBACOwnedLabelKey] == workerRBACOwnedLabelValue {
		return true
	}
	return false
}

func (r *TaskReconciler) ensureWorkerServiceAccount(ctx context.Context, namespace, name string) error {
	log := logf.FromContext(ctx)

	sa := &corev1.ServiceAccount{}
	err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, sa)
	if apierrors.IsNotFound(err) {
		sa = &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels: map[string]string{
					orkaManagedByLabelKey:   managedByLabelValue,
					workerRBACOwnedLabelKey: workerRBACOwnedLabelValue,
				},
			},
		}
		if err := r.Create(ctx, sa); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("creating worker ServiceAccount %s/%s: %w", namespace, name, err)
			}
			if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, sa); err != nil {
				return fmt.Errorf("getting worker ServiceAccount %s/%s after create conflict: %w", namespace, name, err)
			}
		} else {
			log.Info("Created worker ServiceAccount", "namespace", namespace, "serviceAccount", name)
			return nil
		}
	} else if err != nil {
		return fmt.Errorf("getting worker ServiceAccount %s/%s: %w", namespace, name, err)
	}

	if sa.Labels == nil {
		sa.Labels = map[string]string{}
	}
	if sa.Labels[orkaManagedByLabelKey] != managedByLabelValue {
		sa.Labels[orkaManagedByLabelKey] = managedByLabelValue
		if err := r.Update(ctx, sa); err != nil {
			return fmt.Errorf("updating worker ServiceAccount %s/%s labels: %w", namespace, name, err)
		}
		log.Info("Updated worker ServiceAccount", "namespace", namespace, "serviceAccount", name)
	}

	return nil
}

func (r *TaskReconciler) ensureWorkerRoleBinding(ctx context.Context, namespace string, spec workerRBACSpec) error {
	log := logf.FromContext(ctx)
	desired := workerRoleBinding(namespace, spec)

	rb := &rbacv1.RoleBinding{}
	err := r.Get(ctx, types.NamespacedName{Name: spec.clusterRoleBindingName, Namespace: namespace}, rb)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("creating worker RoleBinding %s/%s: %w", namespace, spec.clusterRoleBindingName, err)
			}
			if err := r.Get(ctx, types.NamespacedName{Name: spec.clusterRoleBindingName, Namespace: namespace}, rb); err != nil {
				return fmt.Errorf("getting worker RoleBinding %s/%s after create conflict: %w", namespace, spec.clusterRoleBindingName, err)
			}
		} else {
			log.Info("Created worker RoleBinding", "namespace", namespace, "binding", spec.clusterRoleBindingName, "serviceAccount", spec.serviceAccountName, "clusterRole", spec.clusterRoleName)
			return nil
		}
	} else if err != nil {
		return fmt.Errorf("getting worker RoleBinding %s/%s: %w", namespace, spec.clusterRoleBindingName, err)
	}

	if rb.RoleRef != desired.RoleRef {
		recreated, err := r.recreateWorkerRoleBinding(ctx, namespace, spec, rb, desired)
		if err != nil {
			return err
		}
		rb = recreated
	}

	changed := false
	if rb.Labels == nil {
		rb.Labels = map[string]string{}
	}
	if rb.Labels[managedByLabelKey] != managedByLabelValue {
		rb.Labels[managedByLabelKey] = managedByLabelValue
		changed = true
	}
	if !subjectsEqual(rb.Subjects, desired.Subjects) {
		rb.Subjects = desired.Subjects
		changed = true
	}

	if changed {
		if err := r.Update(ctx, rb); err != nil {
			return fmt.Errorf("updating worker RoleBinding %s/%s: %w", namespace, spec.clusterRoleBindingName, err)
		}
		log.Info("Updated worker RoleBinding", "namespace", namespace, "binding", spec.clusterRoleBindingName, "serviceAccount", spec.serviceAccountName, "clusterRole", spec.clusterRoleName)
	}

	return nil
}

func (r *TaskReconciler) recreateWorkerRoleBinding(ctx context.Context, namespace string, spec workerRBACSpec, current, desired *rbacv1.RoleBinding) (*rbacv1.RoleBinding, error) {
	log := logf.FromContext(ctx)
	log.Info("Recreating worker RoleBinding with stale RoleRef", "namespace", namespace, "binding", spec.clusterRoleBindingName, "currentKind", current.RoleRef.Kind, "currentName", current.RoleRef.Name, "desiredKind", desired.RoleRef.Kind, "desiredName", desired.RoleRef.Name)

	if err := r.Delete(ctx, current); err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("deleting worker RoleBinding %s/%s with stale RoleRef %s/%s: %w", namespace, spec.clusterRoleBindingName, current.RoleRef.Kind, current.RoleRef.Name, err)
	}

	var recreated *rbacv1.RoleBinding
	err := wait.PollUntilContextTimeout(ctx, workerClusterRoleBindingRecreateInterval, workerClusterRoleBindingRecreateTimeout, true, func(ctx context.Context) (bool, error) {
		latest := &rbacv1.RoleBinding{}
		err := r.Get(ctx, types.NamespacedName{Name: spec.clusterRoleBindingName, Namespace: namespace}, latest)
		if err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("getting worker RoleBinding %s/%s while waiting for stale RoleRef deletion: %w", namespace, spec.clusterRoleBindingName, err)
		}

		if err == nil {
			if latest.RoleRef == desired.RoleRef {
				recreated = latest
				return true, nil
			}

			// The API server may still be serving the stale object while deletion is
			// propagating, or another actor may have recreated it with the stale
			// immutable RoleRef. Keep deleting/retrying until the name is available.
			if err := r.Delete(ctx, latest); err != nil && !apierrors.IsNotFound(err) {
				return false, fmt.Errorf("deleting worker RoleBinding %s/%s with stale RoleRef %s/%s during retry: %w", namespace, spec.clusterRoleBindingName, latest.RoleRef.Kind, latest.RoleRef.Name, err)
			}
			return false, nil
		}

		create := desired.DeepCopy()
		if err := r.Create(ctx, create); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return false, nil
			}
			return false, fmt.Errorf("recreating worker RoleBinding %s/%s with RoleRef %s/%s: %w", namespace, spec.clusterRoleBindingName, desired.RoleRef.Kind, desired.RoleRef.Name, err)
		}

		recreated = create
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("recreating worker RoleBinding %s/%s after stale RoleRef %s/%s: %w", namespace, spec.clusterRoleBindingName, current.RoleRef.Kind, current.RoleRef.Name, err)
	}

	log.Info("Recreated worker RoleBinding", "namespace", namespace, "binding", spec.clusterRoleBindingName, "serviceAccount", spec.serviceAccountName, "clusterRole", spec.clusterRoleName)
	return recreated, nil
}

func (r *TaskReconciler) deleteLegacyWorkerClusterRoleBinding(ctx context.Context, namespace string, spec workerRBACSpec) error {
	log := logf.FromContext(ctx)
	legacy := &rbacv1.ClusterRoleBinding{}
	err := r.Get(ctx, types.NamespacedName{Name: spec.clusterRoleBindingName}, legacy)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting legacy worker ClusterRoleBinding %s: %w", spec.clusterRoleBindingName, err)
	}

	desiredLegacy := workerClusterRoleBinding(namespace, spec)
	managed := legacy.Labels[managedByLabelKey] == managedByLabelValue
	bindsWorkerServiceAccount := len(desiredLegacy.Subjects) == 1 && subjectsContain(legacy.Subjects, desiredLegacy.Subjects[0])
	if !managed && !bindsWorkerServiceAccount {
		log.Info("Skipping unmanaged legacy worker ClusterRoleBinding", "namespace", namespace, "binding", spec.clusterRoleBindingName)
		return nil
	}

	if err := r.Delete(ctx, legacy); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting legacy worker ClusterRoleBinding %s: %w", spec.clusterRoleBindingName, err)
	}
	log.Info("Deleted legacy worker ClusterRoleBinding", "namespace", namespace, "binding", spec.clusterRoleBindingName, "serviceAccount", spec.serviceAccountName, "clusterRole", spec.clusterRoleName)
	return nil
}

func (r *TaskReconciler) ensureWorkerClusterRoleBinding(ctx context.Context, namespace string, spec workerRBACSpec) error {
	log := logf.FromContext(ctx)
	desired := workerClusterRoleBinding(namespace, spec)

	crb := &rbacv1.ClusterRoleBinding{}
	err := r.Get(ctx, types.NamespacedName{Name: spec.clusterRoleBindingName}, crb)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("creating worker ClusterRoleBinding %s: %w", spec.clusterRoleBindingName, err)
			}
			if err := r.Get(ctx, types.NamespacedName{Name: spec.clusterRoleBindingName}, crb); err != nil {
				return fmt.Errorf("getting worker ClusterRoleBinding %s after create conflict: %w", spec.clusterRoleBindingName, err)
			}
		} else {
			log.Info("Created worker ClusterRoleBinding", "namespace", namespace, "binding", spec.clusterRoleBindingName, "serviceAccount", spec.serviceAccountName, "clusterRole", spec.clusterRoleName)
			return nil
		}
	} else if err != nil {
		return fmt.Errorf("getting worker ClusterRoleBinding %s: %w", spec.clusterRoleBindingName, err)
	}

	if crb.RoleRef != desired.RoleRef {
		recreated, err := r.recreateWorkerClusterRoleBinding(ctx, namespace, spec, crb, desired)
		if err != nil {
			return err
		}
		crb = recreated
	}

	changed := false
	if crb.Labels == nil {
		crb.Labels = map[string]string{}
	}
	if crb.Labels[managedByLabelKey] != managedByLabelValue {
		crb.Labels[managedByLabelKey] = managedByLabelValue
		changed = true
	}
	if !subjectsEqual(crb.Subjects, desired.Subjects) {
		crb.Subjects = desired.Subjects
		changed = true
	}

	if changed {
		if err := r.Update(ctx, crb); err != nil {
			return fmt.Errorf("updating worker ClusterRoleBinding %s: %w", spec.clusterRoleBindingName, err)
		}
		log.Info("Updated worker ClusterRoleBinding", "namespace", namespace, "binding", spec.clusterRoleBindingName, "serviceAccount", spec.serviceAccountName, "clusterRole", spec.clusterRoleName)
	}

	return nil
}

func (r *TaskReconciler) recreateWorkerClusterRoleBinding(ctx context.Context, namespace string, spec workerRBACSpec, current, desired *rbacv1.ClusterRoleBinding) (*rbacv1.ClusterRoleBinding, error) {
	log := logf.FromContext(ctx)
	log.Info("Recreating worker ClusterRoleBinding with stale RoleRef", "namespace", namespace, "binding", spec.clusterRoleBindingName, "currentKind", current.RoleRef.Kind, "currentName", current.RoleRef.Name, "desiredKind", desired.RoleRef.Kind, "desiredName", desired.RoleRef.Name)

	if err := r.Delete(ctx, current); err != nil && !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("deleting worker ClusterRoleBinding %s with stale RoleRef %s/%s: %w", spec.clusterRoleBindingName, current.RoleRef.Kind, current.RoleRef.Name, err)
	}

	var recreated *rbacv1.ClusterRoleBinding
	err := wait.PollUntilContextTimeout(ctx, workerClusterRoleBindingRecreateInterval, workerClusterRoleBindingRecreateTimeout, true, func(ctx context.Context) (bool, error) {
		latest := &rbacv1.ClusterRoleBinding{}
		err := r.Get(ctx, types.NamespacedName{Name: spec.clusterRoleBindingName}, latest)
		if err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("getting worker ClusterRoleBinding %s while waiting for stale RoleRef deletion: %w", spec.clusterRoleBindingName, err)
		}

		if err == nil {
			if latest.RoleRef == desired.RoleRef {
				recreated = latest
				return true, nil
			}

			// The API server may still be serving the stale object while deletion is
			// propagating, or another actor may have recreated it with the stale
			// immutable RoleRef. Keep deleting/retrying until the name is available.
			if err := r.Delete(ctx, latest); err != nil && !apierrors.IsNotFound(err) {
				return false, fmt.Errorf("deleting worker ClusterRoleBinding %s with stale RoleRef %s/%s during retry: %w", spec.clusterRoleBindingName, latest.RoleRef.Kind, latest.RoleRef.Name, err)
			}
			return false, nil
		}

		create := desired.DeepCopy()
		if err := r.Create(ctx, create); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return false, nil
			}
			return false, fmt.Errorf("recreating worker ClusterRoleBinding %s with RoleRef %s/%s: %w", spec.clusterRoleBindingName, desired.RoleRef.Kind, desired.RoleRef.Name, err)
		}

		recreated = create
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("recreating worker ClusterRoleBinding %s after stale RoleRef %s/%s: %w", spec.clusterRoleBindingName, current.RoleRef.Kind, current.RoleRef.Name, err)
	}

	log.Info("Recreated worker ClusterRoleBinding", "namespace", namespace, "binding", spec.clusterRoleBindingName, "serviceAccount", spec.serviceAccountName, "clusterRole", spec.clusterRoleName)
	return recreated, nil
}

func workerRoleBinding(namespace string, spec workerRBACSpec) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.clusterRoleBindingName,
			Namespace: namespace,
			Labels: map[string]string{
				managedByLabelKey: managedByLabelValue,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     spec.clusterRoleName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      rbacv1.ServiceAccountKind,
				Name:      spec.serviceAccountName,
				Namespace: namespace,
			},
		},
	}
}

func workerClusterRoleBinding(namespace string, spec workerRBACSpec) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: spec.clusterRoleBindingName,
			Labels: map[string]string{
				managedByLabelKey: managedByLabelValue,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     spec.clusterRoleName,
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      rbacv1.ServiceAccountKind,
				Name:      spec.serviceAccountName,
				Namespace: namespace,
			},
		},
	}
}

func subjectsContain(subjects []rbacv1.Subject, want rbacv1.Subject) bool {
	return slices.Contains(subjects, want)
}

// subjectsEqual is intentionally order-sensitive; desired worker bindings
// currently contain exactly one subject.
func subjectsEqual(a, b []rbacv1.Subject) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
