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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	cron "github.com/robfig/cron/v3"
	batchv1 "k8s.io/api/batch/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerpkg "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agentidentity"
	"github.com/orka-agents/orka/internal/agentruntimepolicy"
	"github.com/orka-agents/orka/internal/aitools"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/artifactcap"
	execevents "github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/executionmode"
	gatewayruntime "github.com/orka-agents/orka/internal/gateway"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/outboundaccess"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/taskwait"
	"github.com/orka-agents/orka/internal/tools"
	"github.com/orka-agents/orka/internal/tracing"
	workerpkg "github.com/orka-agents/orka/internal/worker"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/internal/workspace/statusrules"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

const (
	taskTerminalMessage = "task is terminal"
)

const (
	taskTransactionTokenPendingTimeout            = 2 * time.Minute
	failedMountEventStaleAfter                    = 2 * time.Minute
	podLogLimitBytes                              = int64(5 << 20)
	stdoutResultLogLimitBytes                     = int64(15 << 20)
	maxResolvedApprovalsJSONForWorkerEnvBytes     = 32 * 1024
	maxRecentResolvedApprovalsForWorkerEnv        = 32
	maxResolvedApprovalWorkerEnvFieldBytes        = 512
	resolvedApprovalWorkerEnvJSONOverheadEstimate = 128
	workspaceFinalizationTimeout                  = 5 * time.Minute

	eventInvolvedObjectNameField = "involvedObject.name"
	eventReasonField             = "reason"
)

const (
	ConditionTypeComplete = "Complete"

	ConditionTypeJobCreated = "JobCreated"

	ConditionTypeWaitingForApproval = "WaitingForApproval"

	jobCreationVisibilityGracePeriod = 30 * time.Second

	workerRoleBindingRecreateInterval = 100 * time.Millisecond
	workerRoleBindingRecreateTimeout  = 5 * time.Second

	booleanTrueValue       = "true"
	scheduledRunLabelValue = booleanTrueValue
	managedLabelValue      = scheduledRunLabelValue

	workerRBACReconcileFailedReason = "WorkerRBACReconcileFailed"
	taskRetryPendingReason          = "RetryPending"
)

type TaskReconciler struct {
	client.Client
	APIReader                             client.Reader
	Scheme                                *runtime.Scheme
	JobBuilder                            *JobBuilder
	TaskProvenanceProtected               bool
	GatewayService                        *gatewayruntime.Service
	SessionManager                        *SessionManager
	WebhookNotifier                       *WebhookNotifier
	Recorder                              record.EventRecorder
	KubeClient                            kubernetes.Interface
	OutboundAccessResolver                outboundaccess.Resolver
	BrokeredTransactionExchange           *workerpkg.TransactionExchangeConfig
	ResultStore                           store.ResultStore
	PlanStore                             store.PlanStore
	MessageStore                          store.MessageStore
	ArtifactStore                         store.ArtifactStore
	ExecutionEventStore                   store.ExecutionEventStore
	DurableControlStore                   store.DurableControlStore
	AgentExecutionSnapshots               store.AgentExecutionSnapshotStore
	RepositoryValidationBindings          tools.RepositoryValidationBindingStore
	MCPRegistry                           *tools.Registry
	ACPArtifactRetirer                    artifactcap.IdentityRetirer
	ACPPublicationReclaimer               ACPPublicationReclaimer
	ControllerEpochManager                *ControllerEpochManager
	ControllerNamespace                   string
	ACPAdmissionGate                      *ACPAdmissionGate
	Mode                                  executionmode.Mode
	ACPRuntimeImages                      ACPRuntimeImages
	ACPRuntimePoolCapacity                corev1alpha1.RuntimePoolCapacitySpec
	ACPToolboxPolicy                      ACPToolboxPolicy
	Identity                              agentidentity.Config
	ACPRuntimeNamespace                   string
	EnforceNamespaceIsolation             bool
	ProviderUseAuthorization              bool
	MaxTasksPerNamespace                  int32
	WorkspaceProviderAPIEnabled           bool
	WorkspaceSettlementProtected          bool
	ACPWorkspaceDispatchEnabled           bool
	ACPWorkspaceDefaultSuspendAfterDetach time.Duration
	AIWorkerServiceAccountName            string
	VendorWorkerServiceAccountName        string
	ContainerWorkerServiceAccountName     string
	AIWorkerClusterRoleName               string
	VendorWorkerClusterRoleName           string
	ContainerWorkerClusterRoleName        string
	WorkerRoleBindingNamePrefix           string
	EnforceTransactionCredentialAuth      bool
	TransactionCredentialReadScopes       []string
	OutboundAccessTrust                   outboundaccess.TrustConfig
	trustedServiceCleanupMu               sync.RWMutex
	trustedServiceCleanupDone             bool
	WebhookDeliveryWorkers                int
	webhookDeliveries                     *taskWebhookDispatcher
	MaxConcurrentReconciles               int
}

const (
	DefaultTaskMaxConcurrentReconciles = 4
	MaxTaskMaxConcurrentReconciles     = 64
)

// +kubebuilder:rbac:groups=core.orka.ai,resources=tasks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.orka.ai,resources=tasks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=tasks/finalizers,verbs=update
// +kubebuilder:rbac:groups=core.orka.ai,resources=agents,verbs=get;list;watch
// +kubebuilder:rbac:groups=core.orka.ai,resources=agents,verbs=use
// +kubebuilder:rbac:groups=core.orka.ai,resources=providers,verbs=use
// +kubebuilder:rbac:groups=core.orka.ai,resources=tools,verbs=get;list;watch
// +kubebuilder:rbac:groups=core.orka.ai,resources=sessions,verbs=get;list;delete
// +kubebuilder:rbac:groups=core.orka.ai,resources=memories,verbs=list
// +kubebuilder:rbac:groups=core.orka.ai,resources=memoryproposals,verbs=list;create
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups="",resources=pods/status,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=get;list;watch;create;patch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments;replicasets,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;roles;rolebindings,verbs=get;list;watch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=create;update;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=create;update;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,resourceNames=ai-worker-role;vendor-worker-role;container-worker-role,verbs=bind
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=get;list;create;patch

func (r *TaskReconciler) taskMetadataReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *TaskReconciler) patchTaskFinalizer(ctx context.Context, key types.NamespacedName, present bool) error {
	_, err := r.patchTaskFinalizerReturning(ctx, key, present)
	return err
}

func (r *TaskReconciler) patchTaskFinalizerReturning(ctx context.Context, key types.NamespacedName, present bool) (*corev1alpha1.Task, error) {
	var result *corev1alpha1.Task
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &corev1alpha1.Task{}
		if err := r.taskMetadataReader().Get(ctx, key, current); err != nil {
			return err
		}
		hasFinalizer := controllerutil.ContainsFinalizer(current, labels.TaskFinalizer)
		if hasFinalizer == present {
			result = current
			return nil
		}
		base := current.DeepCopy()
		if present {
			controllerutil.AddFinalizer(current, labels.TaskFinalizer)
		} else {
			controllerutil.RemoveFinalizer(current, labels.TaskFinalizer)
		}
		if err := r.Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		result = current
		return nil
	})
	return result, err
}

func acpTaskChainsAdmission(task *corev1alpha1.Task) bool {
	return task != nil && task.Spec.Type == corev1alpha1.TaskTypeAgent && task.Spec.Schedule == "" &&
		task.DeletionTimestamp.IsZero()
}

func (r *TaskReconciler) initializePendingStatus(ctx context.Context, task *corev1alpha1.Task) error {
	initialize := func(t *corev1alpha1.Task) {
		t.Status.Phase = corev1alpha1.TaskPhasePending
		startTrackingInitialApprovalSelection(t)
	}
	candidate := task.DeepCopy()
	initialize(candidate)
	err := r.Status().Update(ctx, candidate)
	if err == nil {
		candidate.DeepCopyInto(task)
		return nil
	}
	if !apierrors.IsConflict(err) {
		return err
	}
	return r.updateStatusWithRetry(ctx, task, initialize)
}

func (r *TaskReconciler) updateStatusWithRetry(ctx context.Context, task *corev1alpha1.Task, mutate func(*corev1alpha1.Task)) error {
	return retryTaskStatusOnConflict(retry.DefaultBackoff, func() error {
		if err := r.Get(ctx, types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, task); err != nil {
			return err
		}
		previousJob := store.TaskJobIdentity{Namespace: task.Namespace, TaskUID: string(task.UID), JobUID: task.Status.JobUID}
		previousJobName := task.Status.JobName
		hadACPExecution := task.Status.Execution != nil && task.Status.Execution.ControllerEpoch > 0
		reported := taskSuccessOf(task)
		mutate(task)
		write := func(writeCtx context.Context) error {
			if taskJobAuthorityChanged(previousJob, previousJobName, task) {
				if err := revokeTaskJobAuthority(writeCtx, r.ResultStore, previousJob); err != nil {
					return err
				}
			}
			return admitTaskStatus(writeCtx, r.ControllerEpochManager, reported, task, func(ctx context.Context) error {
				return r.Status().Update(ctx, task)
			})
		}
		if hadACPExecution && taskDataAuthorityEnded(task) {
			return withACPTaskStatusGuard(ctx, r.DurableControlStore, r.ControllerEpochManager, write)
		}
		return write(ctx)
	})
}

func retryTaskStatusOnConflict(backoff wait.Backoff, write func() error) error {
	var writeErr error
	err := retry.RetryOnConflict(backoff, func() error {
		writeErr = write()
		return writeErr
	})
	if err != nil {
		return err
	}
	return writeErr
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

func taskPhaseCountsTowardConcurrency(phase corev1alpha1.TaskPhase) bool {
	switch phase {
	case corev1alpha1.TaskPhasePending, corev1alpha1.TaskPhaseRunning, corev1alpha1.TaskPhaseFinalizing:
		return true
	default:
		return false
	}
}

func (r *TaskReconciler) admitNewTask(ctx context.Context, req ctrl.Request, task *corev1alpha1.Task) (ctrl.Result, error, bool) {
	log := logf.FromContext(ctx)
	chainAdmission := acpTaskChainsAdmission(task)
	if !controllerutil.ContainsFinalizer(task, labels.TaskFinalizer) {
		patched, err := r.patchTaskFinalizerReturning(ctx, req.NamespacedName, true)
		if err != nil {
			log.Error(err, "failed to add finalizer")
			return ctrl.Result{}, err, true
		}
		if !chainAdmission || patched == nil || patched.UID != task.UID || !acpTaskChainsAdmission(patched) ||
			!controllerutil.ContainsFinalizer(patched, labels.TaskFinalizer) {
			return ctrl.Result{RequeueAfter: time.Second}, nil, true
		}
		patched.DeepCopyInto(task)
	}

	if task.Status.Phase == "" {
		var err error
		if chainAdmission {
			err = r.initializePendingStatus(ctx, task)
		} else {
			err = r.updateStatusWithRetry(ctx, task, func(t *corev1alpha1.Task) {
				t.Status.Phase = corev1alpha1.TaskPhasePending
				startTrackingInitialApprovalSelection(t)
			})
		}
		if err != nil {
			log.Error(err, "failed to update initial status")
			return ctrl.Result{}, err, true
		}
		acpPreDispatch.mark(task, acpPreDispatchStatusInitialized)
		_ = r.recordTaskLifecycleEvent(
			ctx,
			task,
			execevents.ExecutionEventTypeTaskCreated,
			execevents.ExecutionEventSeverityInfo,
			"Task status initialized to Pending",
		)
		if !chainAdmission {
			return ctrl.Result{RequeueAfter: time.Second}, nil, true
		}
		if !task.DeletionTimestamp.IsZero() {
			return ctrl.Result{RequeueAfter: time.Second}, nil, true
		}
	}
	return ctrl.Result{}, nil, false
}

func (r *TaskReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	task := &corev1alpha1.Task{}
	if err := r.Get(ctx, req.NamespacedName, task); err != nil {
		if apierrors.IsNotFound(err) {
			if cleanupErr := r.cleanupTrustedServiceReadBindingsAfterTaskRemoval(ctx, req.Namespace); cleanupErr != nil {
				return ctrl.Result{}, cleanupErr
			}
			return ctrl.Result{}, nil
		}
		log.Error(err, "unable to fetch Task")
		return ctrl.Result{}, err
	}
	if task.Status.Execution == nil && task.Spec.Schedule == "" && task.DeletionTimestamp.IsZero() &&
		!task.Status.Phase.IsTerminal() {
		acpPreDispatch.mark(task, acpPreDispatchObserved)
	}
	if err := r.retainUsageTask(ctx, task); err != nil {
		stopErr := r.stopTaskAfterUsageRetentionFailure(ctx, task, err)
		_ = r.cascadeTaskCancellation(ctx, task)
		return ctrl.Result{}, stopErr
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

	if task.Spec.Schedule == "" {
		ctx = tracing.ExtractTaskTraceContext(ctx, task)
	}
	tracer := tracing.Tracer("orka.controller")
	ctx, span := tracer.Start(ctx, "task.reconcile",
		trace.WithAttributes(spanAttributes...),
	)
	defer span.End()

	if !task.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, task)
	}

	if result, err, done := r.admitNewTask(ctx, req, task); done {
		if err != nil {
			tracing.RecordSpanError(span, err)
		}
		return result, err
	}

	stopThenCascade := func(result ctrl.Result, err error) (ctrl.Result, error) {
		_ = r.cascadeTaskCancellation(ctx, task)
		return result, err
	}

	if !task.Status.Phase.IsTerminal() {
		intent, err := r.loadDeferredTaskFinalization(ctx, task)
		if err != nil {
			return ctrl.Result{}, err
		}
		if intent != nil {
			result, err := r.completeTaskWithOutcome(ctx, task, intent.Phase, intent.OutcomePhase, intent.Message, intent.RecordOutcome)
			r.stopFinishedTaskJob(ctx, task, err)
			return stopThenCascade(result, err)
		}
	}
	if message, ok := requestedTaskCancellation(task); ok {
		return stopThenCascade(r.cancelRequestedNativeTask(ctx, task, message))
	}
	if _, ok := requestedAgentTaskCancellation(task); ok {
		return stopThenCascade(ctrl.Result{}, r.cancelRequestedAgentTask(ctx, task))
	}
	if err := r.projectACPTaskAdmissionStatus(ctx, task); err != nil {
		_ = r.cascadeTaskCancellation(ctx, task)
		return ctrl.Result{}, err
	}

	return r.reconcileTaskPhaseAndApprovalNotices(ctx, task)
}

func (r *TaskReconciler) reconcileTaskPhaseAndApprovalNotices(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) {
	approvalRequeue, err := r.prepareApprovalWebhooks(ctx, task)
	if err != nil {
		return ctrl.Result{}, err
	}
	result, err := r.reconcileTaskPhase(ctx, task)
	approvalRequeue = minPositiveDuration(approvalRequeue, r.scheduleApprovalWebhooks(task))
	if approvalRequeue > 0 && (result.RequeueAfter == 0 || approvalRequeue < result.RequeueAfter) {
		result.RequeueAfter = approvalRequeue
	}
	return result, err
}

func (r *TaskReconciler) reconcileTaskPhase(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) {
	switch task.Status.Phase {
	case corev1alpha1.TaskPhasePending:
		return r.handlePending(ctx, task)
	case corev1alpha1.TaskPhaseScheduled:
		return r.handleScheduled(ctx, task)
	case corev1alpha1.TaskPhaseRunning:
		return r.handleRunning(ctx, task)
	case corev1alpha1.TaskPhaseFinalizing:
		return r.handleFinalizing(ctx, task)
	case corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskPhaseCancelled:
		return r.handleCompletedThenCascade(ctx, task)
	}
	return ctrl.Result{}, nil
}

func (r *TaskReconciler) prepareApprovalWebhooks(ctx context.Context, task *corev1alpha1.Task) (time.Duration, error) {
	if r.WebhookNotifier == nil {
		return 0, nil
	}
	return r.WebhookNotifier.prepareApprovalNotices(ctx, r.Status(), r.ExecutionEventStore, task)
}

func (r *TaskReconciler) scheduleApprovalWebhooks(task *corev1alpha1.Task) time.Duration {
	if r.WebhookNotifier == nil {
		return 0
	}
	index := firstPendingApprovalNotice(task.Status.ApprovalWebhooks)
	if index < 0 {
		return 0
	}
	now := r.WebhookNotifier.now()
	if next := task.Status.ApprovalWebhooks.Notices[index].NextAttemptTime; next != nil && now.Before(next.Time) {
		return next.Sub(now)
	}
	r.enqueueWebhookDelivery(task, false)
	return webhookDeliveryRecheckInterval
}

func (r *TaskReconciler) enqueueWebhookDelivery(task *corev1alpha1.Task, terminal bool) {
	if r.webhookDeliveries == nil {
		logf.Log.WithName("task-webhook-delivery").Error(nil, "no webhook delivery runnable is registered; notice not sent",
			"namespace", task.Namespace, "name", task.Name)
		return
	}
	r.webhookDeliveries.enqueue(task, terminal)
}

func (r *TaskReconciler) projectACPTaskAdmissionStatus(ctx context.Context, task *corev1alpha1.Task) error {
	if err := r.projectACPExecutionWorkspaceStatus(ctx, task); err != nil {
		return err
	}
	return r.syncACPWaitingForCapacity(ctx, task)
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
	if r == nil || r.SessionManager == nil || r.SessionManager.store == nil {
		return ""
	}
	if sessionName == "" {
		return chatSessionEventAttribution(ctx, r.SessionManager.store, task)
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

type sessionTypeReader interface {
	GetSessionType(ctx context.Context, namespace, name string) (string, error)
}

func chatSessionEventAttribution(ctx context.Context, sessions store.SessionStore, task *corev1alpha1.Task) string {
	if task == nil || sessions == nil {
		return ""
	}
	sessionName := strings.TrimSpace(task.Labels[labels.LabelChatSession])
	if sessionName == "" {
		return ""
	}
	var sessionType string
	var err error
	if reader, ok := sessions.(sessionTypeReader); ok {
		sessionType, err = reader.GetSessionType(ctx, task.Namespace, sessionName)
	} else {
		var session *store.SessionRecord
		if session, err = sessions.GetSession(ctx, task.Namespace, sessionName); err == nil {
			sessionType = session.SessionType
		}
	}
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			logf.FromContext(ctx).Error(err, "failed to check chat session for execution event attribution",
				"namespace", task.Namespace, "task", task.Name, "session", sessionName)
		}
		return ""
	}
	if sessionType != store.SessionTypeChat {
		return ""
	}
	return sessionName
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

//nolint:gocyclo,unparam // Cleanup ordering is intentionally kept in one auditable flow; Result is retained for consistency.
func (r *TaskReconciler) handleDeletion(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if controllerutil.ContainsFinalizer(task, labels.TaskFinalizer) {
		if active, err := r.parkedWaitActive(ctx, task); err != nil || active {
			return ctrl.Result{RequeueAfter: time.Second}, err
		}
		prepared, err := r.prepareACPClassWorkspaceDeletion(ctx, task)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !prepared {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		ready, err := r.acpTaskDeletionReady(ctx, task)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ready {
			if !r.recordTerminalTaskLifecycleEventIfMissing(ctx, task) {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		reclaimed, err := r.reclaimACPTaskPublicationBundles(ctx, task)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !reclaimed {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		retired, err := r.retireACPArtifactIdentities(ctx, task)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !retired {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		settled, err := r.settleACPClassWorkspace(ctx, task)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !settled {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		if r.ResultStore != nil {
			if err := r.ResultStore.DeleteResult(ctx, task.Namespace, task.Name); err != nil {
				return ctrl.Result{}, fmt.Errorf("delete result for task %s/%s: %w", task.Namespace, task.Name, err)
			}
		}

		if r.ArtifactStore != nil {
			if err := r.ArtifactStore.DeleteArtifacts(ctx, store.ArtifactOwner{Namespace: task.Namespace, TaskName: task.Name, TaskUID: string(task.UID)}); err != nil {
				return ctrl.Result{}, fmt.Errorf("delete artifacts for task %s/%s: %w", task.Namespace, task.Name, err)
			}
		}

		if r.PlanStore != nil {
			if err := r.PlanStore.DeletePlan(ctx, task.Namespace, task.Name); err != nil {
				return ctrl.Result{}, fmt.Errorf("delete plan state for task %s/%s: %w", task.Namespace, task.Name, err)
			}
		}

		if r.MessageStore != nil {
			if err := r.MessageStore.DeleteTaskMessages(ctx, task.Namespace, task.Name); err != nil {
				return ctrl.Result{}, fmt.Errorf("delete messages for task %s/%s: %w", task.Namespace, task.Name, err)
			}
			if err := r.MessageStore.DeleteParentMessages(ctx, task.Namespace, task.Name); err != nil {
				return ctrl.Result{}, fmt.Errorf("delete child messages for task %s/%s: %w", task.Namespace, task.Name, err)
			}
		}

		if r.ExecutionEventStore != nil {
			if err := r.ExecutionEventStore.DeleteExecutionEvents(ctx, task.Namespace, store.ExecutionEventStreamTypeTask, task.Name); err != nil {
				log.Error(err, "failed to delete execution events", "task", task.Name)
				return ctrl.Result{}, err
			}
		}

		waitingForJob, err := r.cleanupDeletedTaskJob(ctx, task)
		if err != nil {
			log.Error(err, "failed to delete Job")
			return ctrl.Result{}, err
		}
		if waitingForJob {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}

		if task.Spec.SessionRef != nil {
			if err := r.SessionManager.ReleaseLock(ctx, task); err != nil {
				log.Error(err, "failed to release session lock")
			}
		}
		if err := r.releaseMemoryConsolidationRun(ctx, task, true); err != nil {
			return ctrl.Result{}, err
		}
		if r.ResultStore != nil {
			authority, ok := r.ResultStore.(store.TaskJobAuthorityStore)
			if !ok {
				return ctrl.Result{}, errors.New("task Job authority store unavailable during final cleanup")
			}
			if err := authority.DeleteTaskJobRevocations(ctx, task.Namespace, task.Name, string(task.UID)); err != nil {
				return ctrl.Result{}, fmt.Errorf("delete task Job revocations: %w", err)
			}
		}

		key := types.NamespacedName{Name: task.Name, Namespace: task.Namespace}
		if err := r.patchTaskFinalizer(ctx, key, false); err != nil {
			if apierrors.IsNotFound(err) {
				return ctrl.Result{}, nil
			}

			log.Error(err, "failed to remove finalizer")
			return ctrl.Result{}, err
		}
	}
	if err := r.cleanupTrustedServiceReadBindingsAfterTaskRemoval(ctx, task.Namespace); err != nil {
		log.Error(err, "failed to clean up trusted Service RBAC after Task removal")
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

//nolint:gocyclo // Pending routing keeps every mutually exclusive execution path in one auditable state machine.
func (r *TaskReconciler) handlePending(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if outcome := task.Status.ExecutionOutcome; outcome != nil {
		log.Info("refusing to replay task with immutable execution outcome", "outcome", outcome.Phase)
		return r.completeTask(ctx, task, outcome.Phase, outcome.Message)
	}
	if err := r.clearApprovalDecisionNudge(ctx, task); err != nil {
		log.Error(err, "failed to clear durable approval decision nudge")
		return ctrl.Result{}, err
	}

	if requested, err := r.requestCancelForCancelledAncestor(ctx, task); err != nil || requested {
		return ctrl.Result{RequeueAfter: time.Second}, err
	}

	if taskTransactionTokenPending(task) {
		return r.handleTransactionTokenPending(ctx, task)
	}

	if task.Spec.Schedule != "" {
		return r.handleScheduledTask(ctx, task)
	}

	if task.Status.Iteration > 0 && task.Spec.Suspend != nil && *task.Spec.Suspend {
		message := fmt.Sprintf("autonomous task suspended at iteration %d", task.Status.Iteration)
		if task.Status.Message != message {
			task.Status.Message = message
			if err := r.Status().Update(ctx, task); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if task.Spec.Type == corev1alpha1.TaskTypeAgent && task.Status.AgentExecutionBinding != nil {
		return r.handleBoundAgentTaskPending(ctx, task)
	}
	if task.Spec.Type == corev1alpha1.TaskTypeAgent && task.Status.Execution == nil {
		now := time.Now().UTC()
		if deadline, ok := r.pendingAgentTaskDeadline(ctx, task, now); ok && !now.Before(deadline) {
			return r.cancelACPTaskBeforeDurableAttempt(ctx, task, "task deadline exceeded before runtime admission")
		}
	}
	if delay := r.remainingRetryDelay(task, time.Now()); delay > 0 {
		return r.holdRetryAdmission(ctx, task, delay)
	}

	if task.Spec.SessionRef != nil && task.Spec.Type != corev1alpha1.TaskTypeAgent {
		if result, err, locked := r.acquireSessionLock(ctx, task); locked {
			return result, err
		}
	}

	if result, done, _, err := r.acquireNamespaceAdmission(ctx, task); done {
		return result, err
	}

	agent, err := r.resolveAgent(ctx, task)
	if err != nil {
		log.Error(err, "failed to resolve agent")
		return r.failTask(ctx, task, err.Error())
	}

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

	if result, err, done := r.validateCoordinationConstraints(ctx, task); done {
		return result, err
	}

	provider, err := r.resolveProvider(ctx, task, agent)
	if err != nil {
		log.Error(err, "failed to resolve provider")
		return r.failTask(ctx, task, err.Error())
	}

	if task.Spec.Type == corev1alpha1.TaskTypeAgent {
		agent = withEffectiveBuiltInContract(agent, r.Mode)
		plan := r.planAgentExecution(ctx, task, agent)
		if err := validatePlannedRuntimeRefAgentTaskRestrictions(task, agent, plan); err != nil {
			return r.rejectPlannedAgentExecution(ctx, task, rejectAgentExecutionPlan(err.Error()))
		}
		switch plan.path {
		case agentExecutionPathRejected:
			return r.rejectPlannedAgentExecution(ctx, task, plan)
		case agentExecutionPathACP, agentExecutionPathExternal:
			if result, err, handled := r.ensureAgentExecutionBinding(ctx, task, agent); handled {
				return result, err
			}
			return r.queueACPRuntimeTask(ctx, task, agent)
		default:
			return ctrl.Result{}, fmt.Errorf("unknown agent execution path %q", plan.path)
		}
	}

	return r.createTaskJob(ctx, task, agent, provider)
}

func (r *TaskReconciler) pendingAgentTaskDeadline(
	ctx context.Context,
	task *corev1alpha1.Task,
	now time.Time,
) (time.Time, bool) {
	if deadline, ok := acpTaskDeadline(task, now); ok {
		return deadline, true
	}
	if task == nil || task.Spec.Type != corev1alpha1.TaskTypeAgent || task.Status.AgentExecutionBinding != nil {
		return time.Time{}, false
	}
	deadline := taskDeadlineFromTimeout(task, now, defaultACPTaskTimeout)
	if now.Before(deadline) {
		return time.Time{}, false
	}
	agent, err := r.resolveAgent(ctx, task)
	if err != nil {
		return time.Time{}, false
	}
	path := r.planAgentExecution(ctx, task, agent).path
	if path != agentExecutionPathACP && path != agentExecutionPathExternal {
		return time.Time{}, false
	}
	return deadline, true
}

func (r *TaskReconciler) handleBoundAgentTaskPending(
	ctx context.Context,
	task *corev1alpha1.Task,
) (ctrl.Result, error) {
	binding := task.Status.AgentExecutionBinding
	if binding == nil {
		return ctrl.Result{}, errors.New("bound agent Task is missing its execution binding")
	}
	switch binding.ContractVersion {
	case corev1alpha1.AgentRuntimeContractHarnessV2:
		if task.Status.Execution == nil {
			now := time.Now().UTC()
			if deadline, ok := acpTaskDeadline(task, now); ok && !now.Before(deadline) {
				return r.cancelACPTaskBeforeDurableAttempt(ctx, task, "task deadline exceeded before runtime admission")
			}
		}
		if result, err, handled := r.ensureAgentExecutionBinding(ctx, task, nil); handled {
			return result, err
		}
		return r.queueACPRuntimeTask(ctx, task, nil)
	default:
		return r.failTask(ctx, task, fmt.Sprintf(
			"immutable execution binding has unsupported contract %q", binding.ContractVersion,
		))
	}
}

func taskTransactionTokenPending(task *corev1alpha1.Task) bool {
	if task == nil || task.Annotations == nil {
		return false
	}
	pending, err := strconv.ParseBool(task.Annotations[labels.AnnotationTransactionTokenPending])
	return err == nil && pending
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

func (r *TaskReconciler) acquireSessionLock(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error, bool) {
	log := logf.FromContext(ctx)

	locked, err := r.SessionManager.IsLocked(ctx, task)
	if err != nil {
		if errors.Is(err, store.ErrValidation) {
			result, failErr := r.failTask(ctx, task, err.Error())
			return result, failErr, true
		}
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
			if session.ActiveTask == task.Name && (session.ActiveTaskUID == "" || session.ActiveTaskUID == string(task.UID)) {
				return ctrl.Result{}, nil, false
			}
			if session.ActiveTask == "" {
				return ctrl.Result{RequeueAfter: time.Second}, nil, true
			}
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil, true
		}
		if errors.Is(err, store.ErrNotReady) {
			log.Info("gateway session ownership linkage is pending", "session", task.Spec.SessionRef.Name)
			return ctrl.Result{RequeueAfter: time.Second}, nil, true
		}
		log.Error(err, "failed to acquire session lock")
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrValidation) {
			result, failErr := r.failTask(ctx, task, err.Error())
			return result, failErr, true
		}
		return ctrl.Result{}, err, true
	}
	return ctrl.Result{}, nil, false
}

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

func (r *TaskReconciler) acquireNamespaceAdmission(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, bool, bool, error) {
	if r.MaxTasksPerNamespace <= 0 {
		return ctrl.Result{}, false, false, nil
	}
	log := logf.FromContext(ctx)
	validationTask, err := r.repositoryMonitorValidationTask(ctx, task)
	if err != nil {
		log.Error(err, "failed to verify repository validation provenance for namespace limit")
		if errors.Is(err, errRepositoryMonitorValidationConfinement) {
			result, err := r.failTask(ctx, task, err.Error())
			return result, true, false, err
		}
		return ctrl.Result{}, true, false, err
	}
	if validationTask {
		return ctrl.Result{}, false, false, nil
	}
	namespace := r.namespaceAdmissionScope(task.Namespace, r.MaxTasksPerNamespace)
	children, isChild := r.coordinationChildAdmissionScope(ctx, task)
	if isChild {
		free, err := r.taskAdmissionSlotFree(ctx, task, children)
		if err != nil {
			log.Error(err, "failed to check a coordination child slot")
			return ctrl.Result{}, true, false, err
		}
		if !free {
			if err := r.releaseTaskAdmissionSlot(ctx, task, namespace); err != nil {
				log.Error(err, "failed to release a namespace task slot")
				return ctrl.Result{}, true, false, err
			}
			log.Info("coordination concurrency limit reached", "max", children.limit)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, true, true, nil
		}
	}
	granted, err := r.acquireTaskAdmissionSlot(ctx, task, namespace)
	if err != nil {
		log.Error(err, "failed to acquire a namespace task slot")
		return ctrl.Result{}, true, false, err
	}
	if !granted {
		if isChild {
			if err := r.releaseTaskAdmissionSlot(ctx, task, children); err != nil {
				log.Error(err, "failed to release a coordination child slot")
				return ctrl.Result{}, true, false, err
			}
		}
		log.Info("namespace task limit reached, requeueing",
			"namespace", task.Namespace,
			"limit", r.MaxTasksPerNamespace,
		)
		r.Recorder.Eventf(task, corev1.EventTypeNormal, "NamespaceTaskLimitReached",
			"namespace %q has no free task slot (limit: %d), requeueing", task.Namespace, r.MaxTasksPerNamespace)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, true, true, nil
	}
	return ctrl.Result{}, false, false, nil
}

func (r *TaskReconciler) coordinationChildAdmissionScope(ctx context.Context, task *corev1alpha1.Task) (taskAdmissionScope, bool) {
	if _, ok := task.Annotations[labels.AnnotationCoordinationDepth]; !ok {
		return taskAdmissionScope{}, false
	}
	parentName := labels.ParentTaskName(task.Labels, task.Annotations)
	parentTask := &corev1alpha1.Task{}
	if parentName == "" || r.Get(ctx, types.NamespacedName{Name: parentName, Namespace: task.Namespace}, parentTask) != nil {
		return taskAdmissionScope{}, false
	}
	if pinned, ok := taskParentUIDPin(task, parentName); ok && pinned != parentTask.UID {
		return taskAdmissionScope{}, false
	}
	if parentTask.Spec.AgentRef == nil {
		return taskAdmissionScope{}, false
	}
	agentNS := parentTask.Spec.AgentRef.Namespace
	if agentNS == "" {
		agentNS = task.Namespace
	}
	parentAgent := &corev1alpha1.Agent{}
	if r.Get(ctx, types.NamespacedName{Name: parentTask.Spec.AgentRef.Name, Namespace: agentNS}, parentAgent) != nil {
		return taskAdmissionScope{}, false
	}
	coord := parentAgent.Spec.Coordination
	if coord == nil || !coord.Enabled || coord.MaxConcurrentChildren <= 0 {
		return taskAdmissionScope{}, false
	}
	return r.childrenAdmissionScope(parentTask, coord.MaxConcurrentChildren), true
}

func (r *TaskReconciler) holdRetryAdmission(ctx context.Context, task *corev1alpha1.Task, delay time.Duration) (ctrl.Result, error) {
	backoff := ctrl.Result{RequeueAfter: delay}
	if result, done, waiting, err := r.acquireNamespaceAdmission(ctx, task); done {
		if waiting {
			return backoff, nil
		}
		return result, err
	}
	if result, done, waiting, err := r.coordinationConstraints(ctx, task); done && !waiting {
		return result, err
	}
	return backoff, nil
}

func (r *TaskReconciler) validateCoordinationConstraints(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error, bool) {
	result, done, _, err := r.coordinationConstraints(ctx, task)
	return result, err, done
}

func (r *TaskReconciler) coordinationConstraints(ctx context.Context, task *corev1alpha1.Task) (result ctrl.Result, done, waiting bool, err error) {
	result, err, done = r.checkCoordinationConstraints(ctx, task, &waiting)
	return result, done, waiting, err
}

func (r *TaskReconciler) checkCoordinationConstraints(ctx context.Context, task *corev1alpha1.Task, waiting *bool) (ctrl.Result, error, bool) {
	depthStr, ok := task.Annotations[labels.AnnotationCoordinationDepth]
	if !ok {
		return ctrl.Result{}, nil, false
	}

	log := logf.FromContext(ctx)
	parentName := labels.ParentTaskName(task.Labels, task.Annotations)
	depthValue, parseErr := strconv.ParseInt(depthStr, 10, 32)
	if parseErr != nil || depthValue < 0 || strconv.FormatInt(depthValue, 10) != depthStr {
		result, err := r.failTask(ctx, task, "coordination depth annotation is invalid")
		return result, err, true
	}
	depth := int32(depthValue)

	parentTask := &corev1alpha1.Task{}
	if err := r.Get(ctx, types.NamespacedName{Name: parentName, Namespace: task.Namespace}, parentTask); err != nil {
		log.Error(err, "failed to get parent task")
		result, err := r.failTask(ctx, task, fmt.Sprintf("failed to get parent task: %v", err))
		return result, err, true
	}
	if pinned, ok := taskParentUIDPin(task, parentName); ok {
		if err := uncachedReader(r.APIReader, r.Client).Get(ctx, types.NamespacedName{Name: parentName, Namespace: task.Namespace}, parentTask); err != nil {
			if apierrors.IsNotFound(err) {
				result, err := r.failTask(ctx, task, fmt.Sprintf("failed to get parent task: %v", err))
				return result, err, true
			}
			return ctrl.Result{}, err, true
		}
		if pinned != parentTask.UID {
			result, err := r.failTask(ctx, task, fmt.Sprintf(
				"parent Task %q was replaced: this child belongs to parent Task UID %s, not to the current Task of that name", parentName, pinned))
			return result, err, true
		}
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

	if coord.MaxDepth > 0 && depth > coord.MaxDepth {
		result, err := r.failTask(ctx, task, fmt.Sprintf("coordination depth %d exceeds max %d", depth, coord.MaxDepth))
		return result, err, true
	}

	if task.Spec.AgentRef != nil {
		allowed := false
		for _, a := range coord.AllowedAgents {
			if a.Name == task.Spec.AgentRef.Name {
				allowed = true
				break
			}
		}
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

	if coord.MaxConcurrentChildren > 0 {
		return r.acquireChildAdmission(ctx, task, r.childrenAdmissionScope(parentTask, coord.MaxConcurrentChildren), waiting)
	}

	return ctrl.Result{}, nil, false
}

func (r *TaskReconciler) acquireChildAdmission(ctx context.Context, task *corev1alpha1.Task, children taskAdmissionScope, waiting *bool) (ctrl.Result, error, bool) {
	log := logf.FromContext(ctx)
	granted, err := r.acquireTaskAdmissionSlot(ctx, task, children)
	if err != nil {
		if r.MaxTasksPerNamespace > 0 {
			if relErr := r.releaseTaskAdmissionSlot(ctx, task, r.namespaceAdmissionScope(task.Namespace, r.MaxTasksPerNamespace)); relErr != nil {
				log.Error(relErr, "failed to release namespace task slot after child admission error")
			}
		}
		log.Error(err, "failed to acquire a coordination child slot")
		return ctrl.Result{}, err, true
	}
	if granted {
		return ctrl.Result{}, nil, false
	}
	if r.MaxTasksPerNamespace > 0 {
		if err := r.releaseTaskAdmissionSlot(ctx, task, r.namespaceAdmissionScope(task.Namespace, r.MaxTasksPerNamespace)); err != nil {
			log.Error(err, "failed to release a namespace task slot")
			return ctrl.Result{}, err, true
		}
	}
	log.Info("coordination concurrency limit reached", "max", children.limit)
	*waiting = true
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil, true
}

func (r *TaskReconciler) resolveProvider(ctx context.Context, task *corev1alpha1.Task, agent *corev1alpha1.Agent) (*corev1alpha1.Provider, error) {
	if r.ProviderUseAuthorization {
		if names := corev1alpha1.ProvidersFromAgentInAnotherNamespace(task, agent); len(names) > 0 {
			return nil, fmt.Errorf("provider use authorization refuses Providers %s taken from Agent %s/%s in another namespace "+
				"than the Task's %q; set the Task's spec.ai.providerRef and use an Agent without model fallbacks, "+
				"or use an Agent from the Task's namespace",
				strings.Join(names, ", "), agent.Namespace, agent.Name, task.Namespace)
		}
	}
	if err := r.refuseKeylessFallbackProviders(ctx, task, agent); err != nil {
		return nil, err
	}
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
	if provider.UsesKeylessAuth() && (task.Spec.Type != corev1alpha1.TaskTypeAI || taskRequestsReadOnlyAgent(task)) {
		return nil, keylessProviderTaskError(provider.Name)
	}
	return provider, nil
}

func keylessProviderTaskError(name string) error {
	return fmt.Errorf("provider %s uses spec.auth, which only type: ai Tasks that are not read-only support; use a Provider with a secretRef", name)
}

func (r *TaskReconciler) refuseKeylessFallbackProviders(ctx context.Context, task *corev1alpha1.Task, agent *corev1alpha1.Agent) error {
	if task.Spec.Type == corev1alpha1.TaskTypeAgent || !agentHasFallbackProviders(agent) {
		return nil
	}
	allowKeyless := task.Spec.Type == corev1alpha1.TaskTypeAI && !taskRequestsReadOnlyAgent(task)
	for _, fallback := range agent.Spec.Model.Fallbacks {
		provider := &corev1alpha1.Provider{}
		if err := r.Get(ctx, types.NamespacedName{Name: fallback.ProviderRef, Namespace: task.Namespace}, provider); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("failed to get fallback provider: %v", err)
		}
		if !allowKeyless && provider.UsesKeylessAuth() {
			return keylessProviderTaskError(provider.Name)
		}
	}
	return nil
}

func isMemoryConsolidationTask(task *corev1alpha1.Task) bool {
	return task != nil && task.Spec.AI != nil && task.Spec.AI.MemoryConsolidation != nil
}

func taskNeedsApprovalState(task *corev1alpha1.Task, agent *corev1alpha1.Agent) bool {
	return task != nil &&
		!isMemoryConsolidationTask(task) &&
		task.Spec.Type == corev1alpha1.TaskTypeAI &&
		agent != nil &&
		agent.Spec.Coordination != nil &&
		agent.Spec.Coordination.Autonomous
}

func (r *TaskReconciler) resolvedApprovalsJSONForTask(ctx context.Context, task *corev1alpha1.Task) (string, error) {
	if r == nil || r.ExecutionEventStore == nil || task == nil {
		return "", nil
	}
	listed, err := approvals.ListEvents(ctx, r.ExecutionEventStore, task.Namespace, task.Name)
	if err != nil {
		return "", err
	}
	resolved := approvals.Resolved(approvals.Derive(
		approvals.FilterEventsForTaskUID(listed, string(task.UID)),
		time.Time{},
	))
	return resolvedApprovalsJSONForWorkerEnv(resolved)
}

func resolvedApprovalsJSONForWorkerEnv(values []approvals.ResolvedApproval) (string, error) {
	if len(values) == 0 {
		return "", nil
	}

	if resolvedApprovalsLikelyFitWorkerEnv(values, resolvedApprovalWorkerEnvFullPayload) {
		bounded := append([]approvals.ResolvedApproval(nil), values...)
		if data, ok, err := marshalResolvedApprovalsForWorkerEnv(bounded); err != nil || ok {
			return data, err
		}
	}

	if resolvedApprovalsLikelyFitWorkerEnv(values, resolvedApprovalWorkerEnvNoPreviewPayload) {
		withoutPreviews := append([]approvals.ResolvedApproval(nil), values...)
		for i := range withoutPreviews {
			withoutPreviews[i].TargetArgsPreview = nil
		}
		if data, ok, err := marshalResolvedApprovalsForWorkerEnv(withoutPreviews); err != nil || ok {
			return data, err
		}
	}

	compact := compactResolvedApprovalsForWorkerEnv(values)
	if resolvedApprovalsLikelyFitWorkerEnv(compact, resolvedApprovalWorkerEnvCompactPayload) {
		if data, ok, err := marshalResolvedApprovalsForWorkerEnv(compact); err != nil || ok {
			return data, err
		}
	}

	selected, err := selectResolvedApprovalsForWorkerEnv(compact)
	if err != nil || len(selected) == 0 {
		return "", err
	}
	data, ok, err := marshalResolvedApprovalsForWorkerEnv(selected)
	if err != nil || !ok {
		return "", err
	}
	return data, nil
}

func marshalResolvedApprovalsForWorkerEnv(values []approvals.ResolvedApproval) (string, bool, error) {
	data, err := json.Marshal(values)
	if err != nil {
		return "", false, err
	}
	if len(data) > maxResolvedApprovalsJSONForWorkerEnvBytes {
		return "", false, nil
	}
	return string(data), true, nil
}

type resolvedApprovalWorkerEnvPayload int

const (
	resolvedApprovalWorkerEnvFullPayload resolvedApprovalWorkerEnvPayload = iota
	resolvedApprovalWorkerEnvNoPreviewPayload
	resolvedApprovalWorkerEnvCompactPayload
)

func resolvedApprovalsLikelyFitWorkerEnv(
	values []approvals.ResolvedApproval,
	payload resolvedApprovalWorkerEnvPayload,
) bool {
	estimated := 2
	for _, approval := range values {
		estimated += resolvedApprovalWorkerEnvJSONOverheadEstimate
		estimated += len(approval.ID) + len(approval.TaskUID) + len(approval.TargetTool)
		estimated += len(approval.TargetArgsDigest) + len(approval.TargetSpecDigest) + len(approval.Status)
		if payload != resolvedApprovalWorkerEnvCompactPayload {
			estimated += len(approval.Actor) + len(approval.DecisionTime) + len(approval.Reason)
			estimated += len(approval.Action) + len(approval.RiskSummary) + len(approval.Severity)
		}
		if payload == resolvedApprovalWorkerEnvFullPayload {
			estimated += len(approval.TargetArgsPreview)
		}
		if estimated > maxResolvedApprovalsJSONForWorkerEnvBytes {
			return false
		}
	}
	return true
}

func resolvedApprovalWorkerEnvField(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= maxResolvedApprovalWorkerEnvFieldBytes {
		return value
	}
	return value[:maxResolvedApprovalWorkerEnvFieldBytes]
}

func compactResolvedApprovalsForWorkerEnv(values []approvals.ResolvedApproval) []approvals.ResolvedApproval {
	compact := make([]approvals.ResolvedApproval, 0, len(values))
	for _, approval := range values {
		compact = append(compact, approvals.ResolvedApproval{
			ID:               resolvedApprovalWorkerEnvField(approval.ID),
			TaskUID:          resolvedApprovalWorkerEnvField(approval.TaskUID),
			TargetTool:       resolvedApprovalWorkerEnvField(approval.TargetTool),
			TargetArgsDigest: resolvedApprovalWorkerEnvField(approval.TargetArgsDigest),
			TargetSpecDigest: resolvedApprovalWorkerEnvField(approval.TargetSpecDigest),
			Status:           resolvedApprovalWorkerEnvField(approval.Status),
		})
	}
	return compact
}

func selectResolvedApprovalsForWorkerEnv(values []approvals.ResolvedApproval) ([]approvals.ResolvedApproval, error) {
	selected := make([]approvals.ResolvedApproval, 0, min(len(values), maxRecentResolvedApprovalsForWorkerEnv))
	selectedIndexes := make(map[int]struct{}, min(len(values), maxRecentResolvedApprovalsForWorkerEnv))

	for i := len(values) - 1; i >= 0 && len(selectedIndexes) < maxRecentResolvedApprovalsForWorkerEnv; i-- {
		var added bool
		var err error
		selected, added, err = appendResolvedApprovalIfWorkerEnvFits(selected, values[i])
		if err != nil {
			return nil, err
		}
		if added {
			selectedIndexes[i] = struct{}{}
		}
	}

	omittedBlocking := false
	for i, approval := range values {
		if !resolvedApprovalBlocksExecution(approval) {
			continue
		}
		if _, ok := selectedIndexes[i]; ok {
			continue
		}
		var added bool
		var err error
		selected, added, err = appendResolvedApprovalIfWorkerEnvFits(selected, approval)
		if err != nil {
			return nil, err
		}
		if added {
			selectedIndexes[i] = struct{}{}
		} else {
			omittedBlocking = true
		}
	}
	if omittedBlocking {
		var err error
		selected, err = ensureBlockingOverflowSentinelFitsWorkerEnv(selected)
		if err != nil {
			return nil, err
		}
	}

	for i := len(values) - 1; i >= 0; i-- {
		if _, ok := selectedIndexes[i]; ok {
			continue
		}
		if resolvedApprovalBlocksExecution(values[i]) {
			continue
		}
		var added bool
		var err error
		selected, added, err = appendResolvedApprovalIfWorkerEnvFits(selected, values[i])
		if err != nil {
			return nil, err
		}
		if added {
			selectedIndexes[i] = struct{}{}
		}
	}
	return selected, nil
}

func ensureBlockingOverflowSentinelFitsWorkerEnv(
	selected []approvals.ResolvedApproval,
) ([]approvals.ResolvedApproval, error) {
	sentinel := approvals.BlockingOverflowResolvedApproval()
	if slices.ContainsFunc(selected, approvals.IsResolvedApprovalBlockingOverflow) {
		return selected, nil
	}
	for {
		candidate := append([]approvals.ResolvedApproval{sentinel}, selected...)
		if _, ok, err := marshalResolvedApprovalsForWorkerEnv(candidate); err != nil {
			return nil, err
		} else if ok {
			return candidate, nil
		}
		if len(selected) == 0 {
			return nil, fmt.Errorf("blocking approval overflow sentinel does not fit worker env budget")
		}
		drop := len(selected) - 1
		selected = append(selected[:drop], selected[drop+1:]...)
	}
}

func appendResolvedApprovalIfWorkerEnvFits(
	selected []approvals.ResolvedApproval,
	approval approvals.ResolvedApproval,
) ([]approvals.ResolvedApproval, bool, error) {
	candidate := append(append([]approvals.ResolvedApproval(nil), selected...), approval)
	if _, ok, err := marshalResolvedApprovalsForWorkerEnv(candidate); err != nil {
		return nil, false, err
	} else if ok {
		return candidate, true, nil
	}
	return selected, false, nil
}

func resolvedApprovalBlocksExecution(approval approvals.ResolvedApproval) bool {
	status := strings.TrimSpace(approval.Status)
	return status != "" && status != approvals.StatusApproved
}

const missingToolPolicyGrace = 2 * time.Minute

func (r *TaskReconciler) boundMissingToolPolicy(ctx context.Context, task *corev1alpha1.Task, err error) (ctrl.Result, bool, error) {
	if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, false, nil
	}
	if task.CreationTimestamp.IsZero() || time.Since(task.CreationTimestamp.Time) < missingToolPolicyGrace {
		logf.FromContext(ctx).Info("A Tool's outbound access policy does not exist yet; retrying dispatch", "error", err.Error())
		return ctrl.Result{RequeueAfter: 5 * time.Second}, true, nil
	}
	result, failErr := r.failTask(ctx, task, fmt.Sprintf("a Tool references an outbound access policy that does not exist: %v", err))
	return result, true, failErr
}

//nolint:gocyclo // Job creation revalidates provenance, credentials and runtime contract immediately before creation.
func (r *TaskReconciler) createTaskJob(ctx context.Context, task *corev1alpha1.Task, agent *corev1alpha1.Agent, provider *corev1alpha1.Provider) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if holdErr := r.ControllerEpochManager.StorageHold(); holdErr != nil {
		log.Info("not creating the Task Job while storage is held for recovery", "reason", holdErr.Error())
		return ctrl.Result{RequeueAfter: storageHoldRequeue}, nil
	}

	latest := &corev1alpha1.Task{}
	reader := uncachedReader(r.APIReader, r.Client)
	if err := reader.Get(ctx, types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, latest); err != nil {
		return ctrl.Result{}, err
	}
	if !canStartTaskJob(latest.Status.Phase) || executionOutcomePreventsReplay(latest.Status.ExecutionOutcome) {
		task.Status = latest.Status
		log.Info("skipping job creation because task is no longer runnable", "phase", latest.Status.Phase)
		return ctrl.Result{}, nil
	}
	if _, requested := latest.Annotations[labels.AnnotationCancelRequested]; requested {
		log.Info("skipping job creation because the task's cancellation was requested")
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if taskJobIdentityRejected(latest) {
		if err := r.retireRejectedTaskJob(ctx, latest); err != nil {
			return ctrl.Result{}, err
		}
		if latest.Spec.Type == corev1alpha1.TaskTypeAI {
			job := &batchv1.Job{}
			err := reader.Get(ctx, client.ObjectKey{Namespace: latest.Namespace, Name: latest.Status.JobName}, job)
			if err == nil {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
		return r.failTask(ctx, task, meta.FindStatusCondition(latest.Status.Conditions, ConditionTypeJobCreated).Message)
	}
	if delay := r.remainingRetryDelay(latest, time.Now()); delay > 0 {
		return ctrl.Result{RequeueAfter: delay}, nil
	}
	validationTask, err := r.repositoryMonitorValidationTask(ctx, latest)
	if err != nil {
		log.Error(err, "failed to verify repository validation provenance")
		if errors.Is(err, errRepositoryMonitorValidationConfinement) {
			return r.failTask(ctx, task, err.Error())
		}
		return ctrl.Result{}, err
	}
	gateReady, err := r.ensureRepositoryMonitorValidationNetworkGateForTask(ctx, latest, validationTask)
	if err != nil {
		log.Error(err, "failed to prepare repository validation network gate")
		if errors.Is(err, errRepositoryMonitorValidationConfinement) {
			return r.failTask(ctx, task, err.Error())
		}
		return ctrl.Result{}, err
	}
	if !gateReady {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	jobTask := task
	if validationTask || latest.Spec.Type == corev1alpha1.TaskTypeAI {
		jobTask = latest
	}

	if err := r.ensureWorkerRBAC(ctx, task.Namespace); err != nil {
		log.Error(err, "failed to ensure worker RBAC")
		if r.Recorder != nil {
			r.Recorder.Eventf(task, corev1.EventTypeWarning, workerRBACReconcileFailedReason,
				"failed to ensure worker RBAC in namespace %q: %v", task.Namespace, err)
		}
	}

	resolvedApprovalsJSON := ""
	if taskNeedsApprovalState(jobTask, agent) {
		resolvedApprovalsJSON, err = r.resolvedApprovalsJSONForTask(ctx, jobTask)
		if err != nil {
			log.Error(err, "failed to derive resolved approvals")
			return ctrl.Result{}, err
		}
	}

	gatewayReplyEligible, err := r.GatewayService.ResolveReplyEligibility(ctx, latest)
	if errors.Is(err, store.ErrNotReady) {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	existingGatewayJob := false
	if gatewayReplyEligible && jobTask.Spec.Type == corev1alpha1.TaskTypeAI {
		key := client.ObjectKey{Namespace: jobTask.Namespace, Name: buildTaskJobName(jobTask)}
		err := reader.Get(ctx, key, &batchv1.Job{})
		if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		existingGatewayJob = err == nil
		if !existingGatewayJob {
			if latest.UID != task.UID || latest.Generation != task.Generation || task.Spec.AgentRef == nil || agent == nil {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			if changed, err := taskAgentRevisionChanged(ctx, reader, task, agent); err != nil || changed {
				return ctrl.Result{RequeueAfter: time.Second}, err
			}
		}
	}

	if (task.Spec.Type == corev1alpha1.TaskTypeAI || latest.Spec.Type == corev1alpha1.TaskTypeAI) &&
		!existingGatewayJob && (task.UID != latest.UID || task.Generation != latest.Generation) {
		return ctrl.Result{}, aiSoulTaskChanged(task)
	}
	if !gatewayReplyEligible && jobTask.Spec.Type == corev1alpha1.TaskTypeAI && jobTask.Status.SoulBinding == nil &&
		task.Spec.AgentRef != nil && agent != nil {
		if changed, err := taskAgentRevisionChanged(ctx, reader, task, agent); err != nil || changed {
			return ctrl.Result{RequeueAfter: time.Second}, err
		}
	}

	var aiSoul *resolvedAISoul
	if existingGatewayJob && jobTask.Spec.Type == corev1alpha1.TaskTypeAI && jobTask.Status.SoulBinding == nil &&
		agent != nil && agent.Spec.Soul != nil {
		err = invalidAISoulConfiguration("AI Task cannot acquire a soul after execution has started; create a new Task")
	} else {
		aiSoul, err = r.prepareAISoul(ctx, jobTask, agent)
	}
	if err != nil {
		if isPermanentAISoulConfigurationError(err) {
			message := fmt.Sprintf("AI soul configuration: %v", err)
			retired, retirementErr := r.retireAISoulTaskJob(ctx, jobTask, message)
			if retirementErr != nil {
				return ctrl.Result{}, retirementErr
			}
			if retired {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			return r.failTask(ctx, task, message)
		}
		return ctrl.Result{}, err
	}
	if jobTask.Spec.Type == corev1alpha1.TaskTypeAI {
		task.Status = jobTask.Status
	}

	frozenConnections, frozenClassification, err := freezeClassifiedRequesterConnections(ctx, reader, NativeWorkerToolRegistry(jobTask, agent), jobTask, aitools.Resolve(jobTask, agent), connectorScope{})
	if err != nil {
		if result, handled, failErr := r.boundMissingToolPolicy(ctx, task, err); handled {
			return result, failErr
		}
		log.Error(err, "failed to freeze requester connections; retrying dispatch")
		return ctrl.Result{}, err
	}

	connectionBindings := taskConnectionBindings(frozenConnections)

	job, err := r.JobBuilder.BuildWithOptions(ctx, jobTask, agent, provider, JobBuildOptions{
		ResolvedApprovalsJSON:       resolvedApprovalsJSON,
		AISoul:                      aiSoul,
		RepositoryMonitorValidation: validationTask,
		ConnectionBindings:          connectionBindings,
		ConnectionBindingsFrozen:    true,
		ConnectorToolDigests:        nativeConnectorToolDigests(frozenClassification),
		Reader:                      reader,
		GatewayReplyEligible:        gatewayReplyEligible,
	})
	if err != nil {
		if errors.Is(err, ErrConnectorToolResolution) {
			if result, handled, failErr := r.boundMissingToolPolicy(ctx, task, err); handled {
				return result, failErr
			}
			log.Error(err, "failed to resolve connector-backed tools; retrying dispatch")
			return ctrl.Result{}, err
		}
		if errors.Is(err, ErrContainerTaskSecretUseUnavailable) {
			log.Error(err, "failed to confirm the container Task Secret use policy; retrying dispatch")
			return ctrl.Result{}, err
		}
		log.Error(err, "failed to build Job")
		return r.failTask(ctx, task, fmt.Sprintf("failed to build job: %v", err))
	}

	if err := controllerutil.SetControllerReference(jobTask, job, r.Scheme); err != nil {
		log.Error(err, "failed to set owner reference")
		return ctrl.Result{}, err
	}

	if !existingGatewayJob {
		err = r.ControllerEpochManager.AdmitWork(ctx, func(ctx context.Context) error {
			return r.Create(ctx, job)
		})
		if errors.Is(err, store.ErrStorageRecoveryRequired) || errors.Is(err, store.ErrNotReady) {
			log.Info("not creating the Task Job while storage is held for recovery", "reason", err.Error())
			return ctrl.Result{RequeueAfter: storageHoldRequeue}, nil
		}
	}
	if existingGatewayJob || apierrors.IsAlreadyExists(err) {
		existing, recoveryErr := r.recoverTaskJob(ctx, latest, job, validationTask)
		if recoveryErr != nil {
			if errors.Is(recoveryErr, errTaskJobIdentity) || errors.Is(recoveryErr, errRepositoryMonitorValidationConfinement) {
				return r.failTask(ctx, task, recoveryErr.Error())
			}
			return ctrl.Result{}, recoveryErr
		}
		job = existing
		recovered, _, bindingErr := FrozenConnectionBindingsFromJob(existing)
		if bindingErr != nil {
			return r.failTask(ctx, task, fmt.Sprintf("%v: %v", errTaskJobIdentity, bindingErr))
		}
		connectionBindings = recovered
	} else if err != nil {
		log.Error(err, "failed to create Job")
		return r.failTask(ctx, task, fmt.Sprintf("failed to create job: %v", err))
	}
	task.Status.JobName = job.Name
	task.Status.JobUID = string(job.UID)
	task.Status.ConnectionBindings = connectionBindings

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
	taskUID := task.UID
	jobName := task.Status.JobName
	jobUID := task.Status.JobUID
	transitionedToRunning := false
	if err := r.updateStatusWithRetry(ctx, task, func(t *corev1alpha1.Task) {
		transitionedToRunning = false
		if t.UID != taskUID || !canStartTaskJob(t.Status.Phase) {
			return
		}
		t.Status.Phase = corev1alpha1.TaskPhaseRunning
		t.Status.StartTime = &now
		t.Status.Attempts = attempts
		t.Status.JobName = jobName
		t.Status.JobUID = jobUID
		t.Status.ConnectionBindings = connectionBindings
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

func (r *TaskReconciler) handleRunning(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) { //nolint:gocyclo
	log := logf.FromContext(ctx)

	if taskManagedByACP(task) {
		if err := r.reconcileRunningACPClassWorkspaceAttachment(ctx, task); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.syncACPWaitingForApproval(ctx, task); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	if task.Spec.Timeout != nil && task.Status.StartTime != nil {
		elapsed := time.Since(task.Status.StartTime.Time)
		deadline := task.Status.StartTime.Add(task.Spec.Timeout.Duration)
		if elapsed > task.Spec.Timeout.Duration {
			succeeded, err := r.taskJobSucceededBy(ctx, task, deadline)
			if err != nil {
				return ctrl.Result{}, err
			}
			if !succeeded {
				if result, handled, err := r.handleAutonomousApprovalState(ctx, task); err != nil || handled {
					return result, err
				}
				log.Info("task timed out", "elapsed", elapsed, "timeout", task.Spec.Timeout.Duration)
				validationCommandStarted, err := r.repositoryMonitorValidationCommandStarted(ctx, task)
				if err != nil {
					log.Error(err, "failed to classify timed-out repository validation")
					if errors.Is(err, errRepositoryMonitorValidationConfinement) {
						return r.failTask(ctx, task, err.Error())
					}
					return ctrl.Result{}, err
				}
				if validationCommandStarted {
					return r.completeExecutedTask(ctx, task, corev1alpha1.TaskPhaseFailed, timedOutTaskMessage(task))
				}
				return r.failTask(ctx, task, timedOutTaskMessage(task))
			}
		}
	}

	if task.Spec.Type == corev1alpha1.TaskTypeAgent && strings.TrimSpace(task.Status.JobName) == "" {
		return r.failTask(ctx, task, "agent task is not managed by the ACP runtime dispatcher")
	}

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

	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{
		Name:      task.Status.JobName,
		Namespace: task.Namespace,
	}, job); err != nil {
		if apierrors.IsNotFound(err) {
			if r.isAutonomousTask(ctx, task) {
				oldJob := task.Status.JobName
				latest := &corev1alpha1.Task{}
				reader := uncachedReader(r.APIReader, r.Client)
				if latestErr := reader.Get(ctx, types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, latest); latestErr != nil {
					return ctrl.Result{}, latestErr
				}
				if latest.Status.JobName != oldJob || latest.Status.Phase != corev1alpha1.TaskPhaseRunning {
					task.Status = latest.Status
					log.Info("job not found for stale autonomous task state; requeueing with latest status",
						"oldJob", oldJob,
						"latestJob", latest.Status.JobName,
						"latestPhase", latest.Status.Phase)
					return ctrl.Result{RequeueAfter: time.Second}, nil
				}
				task = latest
				if result, handled, err := r.handleAutonomousApprovalState(ctx, task); err != nil || handled {
					return result, err
				}
			}
			if r.isWithinJobCreationVisibilityGracePeriod(task) {
				log.Info("job not found shortly after creation, waiting for cache visibility",
					"job", task.Status.JobName,
					"startTime", task.Status.StartTime,
				)
				return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
			}
			if r.shouldRetry(task) {
				log.Info("job not found while task still has retry budget, scheduling retry", "attempt", task.Status.Attempts)
				return r.retryTaskAfterFailure(ctx, task, "job not found")
			}
			log.Info("Job not found, task may have been cleaned up")
			return r.failTask(ctx, task, "job not found")
		}
		log.Error(err, "failed to get Job")
		return ctrl.Result{}, err
	}
	if task.Status.JobUID != "" && task.Status.JobUID != string(job.UID) {
		return r.failTask(ctx, task, "task Job identity changed")
	}
	if err := r.reconcileRepositoryMonitorValidationConfinement(ctx, task, job); err != nil {
		log.Error(err, "repository validation confinement failed")
		if errors.Is(err, errRepositoryMonitorValidationConfinement) {
			return r.failTask(ctx, task, err.Error())
		}
		return ctrl.Result{}, err
	}

	if job.Status.Succeeded > 0 {
		if r.isAutonomousTask(ctx, task) {
			return r.handleAutonomousIteration(ctx, task)
		}
		return r.completeExecutedTask(ctx, task, corev1alpha1.TaskPhaseSucceeded, "task completed successfully")
	}

	if job.Status.Failed > 0 {
		if result, handled, err := r.handleAutonomousApprovalState(ctx, task); err != nil || handled {
			return result, err
		}
		validationCommandFailed := true
		validationTask, validationErr := r.repositoryMonitorValidationTask(ctx, task)
		if validationErr != nil {
			return ctrl.Result{}, validationErr
		}
		if validationTask {
			var err error
			validationCommandFailed, err = r.repositoryMonitorValidationCommandFailed(ctx, task, job)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
		if task.Spec.Timeout != nil && jobFailedDueToActiveDeadline(job) {
			if !validationCommandFailed {
				return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, timedOutTaskMessage(task))
			}
			return r.completeExecutedTask(ctx, task, corev1alpha1.TaskPhaseFailed, timedOutTaskMessage(task))
		}
		if r.shouldRetry(task) {
			failure := r.diagnoseFailedJob(ctx, task)
			log.Info("retrying task", "attempt", task.Status.Attempts, "failure", failure)
			return r.retryTaskAfterFailure(ctx, task, failure)
		}
		msg := r.diagnoseFailedJob(ctx, task)
		if !validationCommandFailed {
			return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, msg)
		}
		return r.completeExecutedTask(ctx, task, corev1alpha1.TaskPhaseFailed, msg)
	}

	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(task.Namespace),
		client.MatchingLabels{labels.LabelTask: labels.SelectorValue(task.Name)}); err == nil {
		podWaiting := ""
		for i := range podList.Items {
			pod := &podList.Items[i]
			if pod.Status.Phase != corev1.PodPending {
				continue
			}
			if podWaiting == "" && podBelongsToJob(pod, task.Status.JobName) {
				podWaiting = podWaitingMessage(pod)
			}
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.State.Waiting != nil {
					reason := cs.State.Waiting.Reason
					if reason == podWaitingReasonCreateContainerConfig || reason == "ErrImageNeverPull" {
						msg := fmt.Sprintf("pod stuck: %s - %s", reason, cs.State.Waiting.Message)
						log.Info("failing task due to unrecoverable pod error", "reason", reason)
						return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, msg)
					}
				}
			}
			for _, cond := range pod.Status.Conditions {
				if cond.Type == corev1.PodScheduled && cond.Status == corev1.ConditionFalse && cond.Reason == "Unschedulable" {
					msg := fmt.Sprintf("pod unschedulable: %s", cond.Message)
					log.Info("failing task due to unschedulable pod", "message", cond.Message)
					return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, msg)
				}
			}
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
		if err := r.syncPodWaitingMessage(ctx, task, podWaiting); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

const podWaitingMessagePrefix = "pod waiting: "

func podWaitingMessage(pod *corev1.Pod) string {
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, cs := range statuses {
			waiting := cs.State.Waiting
			if waiting == nil {
				continue
			}
			switch waiting.Reason {
			case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", podWaitingReasonCreateContainerConfig:
				message := podWaitingMessagePrefix + waiting.Reason
				if detail := strings.TrimSpace(waiting.Message); detail != "" {
					message += " - " + detail
				}
				return sanitizeStatusMessage(message)
			}
		}
	}
	return ""
}

func (r *TaskReconciler) syncPodWaitingMessage(ctx context.Context, task *corev1alpha1.Task, message string) error {
	if message == task.Status.Message || (message == "" && !strings.HasPrefix(task.Status.Message, podWaitingMessagePrefix)) {
		return nil
	}
	jobName := task.Status.JobName
	return r.updateStatusWithRetry(ctx, task, func(t *corev1alpha1.Task) {
		if t.Status.Phase != corev1alpha1.TaskPhaseRunning || t.Status.JobName != jobName {
			return
		}
		if message == "" && !strings.HasPrefix(t.Status.Message, podWaitingMessagePrefix) {
			return
		}
		t.Status.Message = message
	})
}

func (r *TaskReconciler) taskJobSucceededBy(ctx context.Context, task *corev1alpha1.Task, deadline time.Time) (bool, error) {
	if strings.TrimSpace(task.Status.JobName) == "" {
		return false, nil
	}
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Name: task.Status.JobName, Namespace: task.Namespace}, job); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if task.Status.JobUID != "" && task.Status.JobUID != string(job.UID) {
		return false, nil
	}
	if job.Status.Succeeded == 0 || job.Status.CompletionTime == nil || job.Status.CompletionTime.After(deadline) {
		return false, nil
	}
	autonomous, err := r.taskAutonomousMode(ctx, task)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	return !autonomous && err == nil, err
}

func timedOutTaskMessage(task *corev1alpha1.Task) string {
	if strings.HasPrefix(task.Status.Message, podWaitingMessagePrefix) {
		return "task timed out; " + task.Status.Message
	}
	return "task timed out"
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
		for _, cs := range pod.Status.ContainerStatuses {
			term := cs.State.Terminated
			if term == nil {
				term = cs.LastTerminationState.Terminated
			}
			if term == nil {
				continue
			}
			if term.Reason == "OOMKilled" || term.ExitCode == 137 {
				limit := podContainerMemoryLimit(pod, cs.Name)
				if limit == "" {
					limit = repositoryMonitorIssueUnknownValue
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

//nolint:gocyclo // Task finalization keeps publication, Session settlement, and cleanup ordering visible.
func (r *TaskReconciler) handleFinalizing(
	ctx context.Context, task *corev1alpha1.Task,
) (ctrl.Result, error) {
	outcome := task.Status.ExecutionOutcome
	if outcome == nil {
		return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, "finalizing task is missing execution outcome")
	}
	genericFinalizationTimedOut := !outcome.RecordedAt.IsZero() &&
		time.Since(outcome.RecordedAt.Time) >= workspaceFinalizationTimeout
	if taskExecutionWorkspaceNeedsFinalization(task) {
		workspaceStatus := task.Status.ExecutionWorkspace
		if workspaceStatus == nil || workspaceStatus.WorkspaceRef == nil || workspaceStatus.AttachedEpoch <= 0 {
			if genericFinalizationTimedOut {
				if err := r.quarantineFinalizingWorkspace(ctx, task); err != nil {
					return ctrl.Result{}, err
				}
				return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, "workspace authority revocation timed out; workspace quarantined")
			}
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		workspaceObject := &workspacev1alpha1.ExecutionWorkspace{}
		key := types.NamespacedName{Namespace: task.Namespace, Name: workspaceStatus.WorkspaceRef.Name}
		if err := r.Get(ctx, key, workspaceObject); err != nil {
			if apierrors.IsNotFound(err) {
				return r.completeTask(ctx, task, outcome.Phase, outcome.Message)
			}
			return ctrl.Result{}, err
		}
		if workspaceStatus.WorkspaceRef.UID != "" && string(workspaceObject.UID) != workspaceStatus.WorkspaceRef.UID {
			return ctrl.Result{}, fmt.Errorf("execution workspace UID changed during finalization")
		}
		revocationEpoch := workspaceStatus.AttachedEpoch
		acpWorkspace := workspaceHasACPControllerOwnership(workspaceObject)
		if acpWorkspace {
			revocationEpoch = acpWorkspaceRevocationEpochForTask(task, workspaceObject, revocationEpoch)
			if err := r.markACPTaskAttachmentEpoch(ctx, task, revocationEpoch); err != nil {
				return ctrl.Result{}, err
			}
			if err := r.markACPWorkspaceRevocationStarted(ctx, workspaceObject, task, revocationEpoch); err != nil {
				return ctrl.Result{}, err
			}
		}
		attachmentManager := WorkspaceAttachmentManager{Client: r.Client, APIReader: r.APIReader}
		if err := attachmentManager.BeginRevocation(ctx, workspaceObject, revocationEpoch); err != nil {
			return ctrl.Result{}, err
		}
		if acpWorkspace {
			result, expired, err := r.failFinalizingTaskPastACPDetachTimeout(ctx, task, workspaceObject)
			if err != nil || expired {
				return result, err
			}
		} else if genericFinalizationTimedOut {
			if err := r.quarantineFinalizingWorkspace(ctx, task); err != nil {
				return ctrl.Result{}, err
			}
			return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, "workspace authority revocation timed out; workspace quarantined")
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	workspaceStatus := task.Status.ExecutionWorkspace
	if workspaceStatus != nil && workspaceStatus.WorkspaceRef != nil && workspaceStatus.AttachedEpoch > 0 {
		workspaceObject := &workspacev1alpha1.ExecutionWorkspace{}
		key := types.NamespacedName{Namespace: task.Namespace, Name: workspaceStatus.WorkspaceRef.Name}
		if err := r.Get(ctx, key, workspaceObject); err == nil {
			if workspaceStatus.WorkspaceRef.UID != "" && string(workspaceObject.UID) != workspaceStatus.WorkspaceRef.UID {
				return ctrl.Result{}, fmt.Errorf("execution workspace UID changed during finalization")
			}
			revocationEpoch := workspaceStatus.AttachedEpoch
			acpWorkspace := workspaceHasACPControllerOwnership(workspaceObject)
			if acpWorkspace {
				revocationEpoch = acpWorkspaceRevocationEpochForTask(task, workspaceObject, revocationEpoch)
				if err := r.markACPWorkspaceRevocationStarted(ctx, workspaceObject, task, revocationEpoch); err != nil {
					return ctrl.Result{}, err
				}
			}
			attachmentManager := WorkspaceAttachmentManager{Client: r.Client, APIReader: r.APIReader}
			if err := attachmentManager.FinalizeRevocation(ctx, workspaceObject, revocationEpoch, attachmentSecretName(workspaceObject.Name, revocationEpoch)); err != nil {
				if acpWorkspace {
					result, expired, timeoutErr := r.failFinalizingTaskPastACPDetachTimeout(ctx, task, workspaceObject)
					if timeoutErr != nil || expired {
						return result, timeoutErr
					}
				} else if genericFinalizationTimedOut {
					if quarantineErr := r.quarantineFinalizingWorkspace(ctx, task); quarantineErr != nil {
						return ctrl.Result{}, quarantineErr
					}
					return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, "workspace authority revocation timed out; workspace quarantined")
				}
				return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
			}
		} else if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	return r.completeTask(ctx, task, outcome.Phase, outcome.Message)
}

func acpWorkspaceRevocationEpochForTask(
	task *corev1alpha1.Task,
	workspaceObject *workspacev1alpha1.ExecutionWorkspace,
	projectedEpoch int64,
) int64 {
	if task == nil {
		return projectedEpoch
	}
	if workspaceObject != nil && task.UID != "" && workspaceObject.Spec.Attachment != nil &&
		workspaceObject.Spec.Attachment.TaskRef.UID == task.UID && workspaceObject.Spec.Attachment.Epoch > 0 {
		return workspaceObject.Spec.Attachment.Epoch
	}
	if recordedEpoch := acpTaskRecordedAttachmentEpoch(task); recordedEpoch > projectedEpoch {
		return recordedEpoch
	}
	return projectedEpoch
}

func (r *TaskReconciler) failFinalizingTaskPastACPDetachTimeout(
	ctx context.Context,
	task *corev1alpha1.Task,
	workspaceObject *workspacev1alpha1.ExecutionWorkspace,
) (ctrl.Result, bool, error) {
	expired, err := r.quarantineACPWorkspacePastDetachTimeout(ctx, workspaceObject)
	if err != nil || !expired {
		return ctrl.Result{}, false, err
	}
	result, err := r.completeTask(
		ctx,
		task,
		corev1alpha1.TaskPhaseFailed,
		"workspace authority revocation exceeded the class detach timeout; workspace quarantined",
	)
	return result, true, err
}

func (r *TaskReconciler) quarantineFinalizingWorkspace(ctx context.Context, task *corev1alpha1.Task) error {
	if task == nil {
		return nil
	}
	status := task.Status.ExecutionWorkspace
	workspaces := []workspacev1alpha1.ExecutionWorkspace{}
	if status != nil && status.WorkspaceRef != nil && strings.TrimSpace(status.WorkspaceRef.Name) != "" {
		workspaceObject := workspacev1alpha1.ExecutionWorkspace{}
		err := r.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: status.WorkspaceRef.Name}, &workspaceObject)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if err == nil {
			workspaces = append(workspaces, workspaceObject)
		}
	} else {
		workspaceList := workspacev1alpha1.ExecutionWorkspaceList{}
		if err := r.List(ctx, &workspaceList, client.InNamespace(task.Namespace)); err != nil {
			return err
		}
		for index := range workspaceList.Items {
			owner := metav1.GetControllerOf(&workspaceList.Items[index])
			if owner != nil && owner.Kind == "Task" && owner.UID == task.UID {
				workspaces = append(workspaces, workspaceList.Items[index])
			}
		}
	}
	for index := range workspaces {
		workspaceObject := &workspaces[index]
		if status != nil && status.WorkspaceRef != nil && status.WorkspaceRef.UID != "" && string(workspaceObject.UID) != status.WorkspaceRef.UID {
			return fmt.Errorf("execution workspace UID changed during quarantine")
		}
		epoch := workspaceObject.Spec.AttachmentEpoch
		if status != nil {
			epoch = max(epoch, status.AttachedEpoch)
		}
		before := workspaceObject.DeepCopy()
		workspaceObject.Spec.AttachmentEpoch = epoch
		workspaceObject.Spec.Attachment = nil
		workspaceObject.Spec.DesiredState = workspacev1alpha1.ExecutionWorkspaceDesiredQuarantined
		if err := r.Patch(ctx, workspaceObject, client.MergeFrom(before)); err != nil {
			return err
		}
		if epoch > 0 {
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: attachmentSecretName(workspaceObject.Name, epoch), Namespace: workspaceObject.Namespace}}
			if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
		lease := &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: attachmentLeaseName(workspaceObject.Name), Namespace: workspaceObject.Namespace}}
		if err := r.Delete(ctx, lease); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *TaskReconciler) handleCompleted(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	terminalEventRecorded := r.recordTerminalTaskLifecycleEventIfMissing(ctx, task)
	waitingForJob, err := r.cleanupTerminalTaskJob(ctx, task)
	if err != nil {
		log.Error(err, "failed to clean up terminal task Job")
		return ctrl.Result{}, err
	}
	if waitingForJob {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	if err := r.cleanupDeferredTaskFinalization(ctx, task); err != nil {
		return ctrl.Result{}, err
	}

	if !terminalEventRecorded {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	if r.ACPArtifactRetirer != nil && task.Spec.Type == corev1alpha1.TaskTypeAgent && task.Status.Execution != nil && r.DurableControlStore != nil {
		ready, readyErr := r.acpTaskDeletionReady(ctx, task)
		if readyErr != nil {
			log.Error(readyErr, "failed to verify ACP artifact retirement readiness")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		if !ready {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		retired, retireErr := r.retireACPArtifactIdentities(ctx, task)
		if retireErr != nil {
			log.Error(retireErr, "failed to retire ACP artifact identities")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		if !retired {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
	}

	settled, settleErr := r.reconcileACPClassWorkspaceSettlement(ctx, task)
	if settleErr != nil {
		log.Error(settleErr, "failed to settle terminal ACP workspace")
		return ctrl.Result{}, settleErr
	}
	if !settled {
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}

	if r.WebhookNotifier != nil {
		if outstanding, nextAttempt := terminalNoticeOutstanding(task, r.WebhookNotifier.now()); outstanding {
			if nextAttempt == 0 {
				r.enqueueWebhookDelivery(task, true)
				nextAttempt = webhookDeliveryRecheckInterval
			}
			return ctrl.Result{RequeueAfter: nextAttempt}, nil
		}
	}

	if err := r.enforceParentScheduledTaskHistory(ctx, task); err != nil {
		return ctrl.Result{}, err
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
	if taskJobIdentityRejected(task) {
		if !metav1.IsControlledBy(job, task) {
			return false, nil
		}
		return true, r.deleteRejectedTaskJob(ctx, job)
	}

	validationTask := r.repositoryMonitorValidationSafetyTask(ctx, task)
	propagationPolicy := metav1.DeletePropagationBackground
	if validationTask {
		propagationPolicy = metav1.DeletePropagationForeground
	}
	if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &propagationPolicy}); err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("deleting deleted task Job %q: %w", task.Status.JobName, err)
	}
	return validationTask, nil
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
	if taskJobIdentityRejected(task) {
		if !metav1.IsControlledBy(job, task) {
			return false, nil
		}
		return true, r.deleteRejectedTaskJob(ctx, job)
	}

	deleteJob := task.Status.Phase == corev1alpha1.TaskPhaseCancelled ||
		(task.Status.Phase == corev1alpha1.TaskPhaseFailed && job.Status.Active > 0)
	if !deleteJob {
		return false, nil
	}

	validationTask := r.repositoryMonitorValidationSafetyTask(ctx, task)
	propagationPolicy := metav1.DeletePropagationBackground
	if validationTask {
		propagationPolicy = metav1.DeletePropagationForeground
	}
	if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &propagationPolicy}); err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("deleting terminal task Job %q: %w", task.Status.JobName, err)
	}

	return validationTask, nil
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

func (r *TaskReconciler) cancelRequestedNativeTask(ctx context.Context, task *corev1alpha1.Task, message string) (ctrl.Result, error) {
	result, err := r.completeTask(ctx, task, corev1alpha1.TaskPhaseCancelled, message)
	r.stopFinishedTaskJob(ctx, task, err)
	return result, err
}

func (r *TaskReconciler) stopFinishedTaskJob(ctx context.Context, task *corev1alpha1.Task, completeErr error) {
	if completeErr != nil || !task.Status.Phase.IsTerminal() {
		return
	}
	if _, err := r.cleanupTerminalTaskJob(ctx, task); err != nil {
		logf.FromContext(ctx).Error(err, "failed to clean up the finished task's Job")
	}
}

func (r *TaskReconciler) handleCompletedThenCascade(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) {
	if err := r.releaseMemoryConsolidationRun(ctx, task, false); err != nil {
		return ctrl.Result{}, err
	}
	result, err := r.handleCompleted(ctx, task)
	cascadeErr := r.cascadeTaskCancellation(ctx, task)
	if err == nil && cascadeErr != nil && (result.RequeueAfter == 0 || result.RequeueAfter > 2*time.Second) {
		result.RequeueAfter = 2 * time.Second
	}
	return result, err
}

func (r *TaskReconciler) releaseMemoryConsolidationRun(ctx context.Context, task *corev1alpha1.Task, deleted bool) error {
	if !isMemoryConsolidationTask(task) || task.UID == "" || r.ResultStore == nil {
		return nil
	}
	runs, ok := r.ResultStore.(store.MemoryConsolidationStore)
	if !ok {
		return nil
	}
	if err := runs.ReleaseMemoryConsolidationRun(ctx, task.Namespace, string(task.UID), deleted); err != nil {
		return fmt.Errorf("release memory consolidation run for task %s/%s: %w", task.Namespace, task.Name, err)
	}
	return nil
}

func (r *TaskReconciler) cascadeTaskCancellation(ctx context.Context, task *corev1alpha1.Task) error {
	if _, requested := task.Annotations[labels.AnnotationCancelRequested]; !requested && task.Status.Phase != corev1alpha1.TaskPhaseCancelled {
		return nil
	}
	err := r.cancelChildTasks(ctx, task)
	if err != nil {
		logf.FromContext(ctx).Error(err, "failed to cancel the Task's child Tasks")
	}
	return err
}

func (r *TaskReconciler) cancelChildTasks(ctx context.Context, task *corev1alpha1.Task) error {
	index, err := r.delegatedTaskIndex(ctx, task)
	if err != nil {
		return err
	}
	visited := map[types.NamespacedName]bool{client.ObjectKeyFromObject(task): true}
	parents := []*corev1alpha1.Task{task}
	var errs []error
	for len(parents) > 0 {
		parent := parents[0]
		parents = parents[1:]
		message := "cancelled with its parent task " + parent.Name
		if parent != task {
			message = "cancelled with its ancestor task " + task.Name
		}
		children := index[labels.SelectorValue(parent.Name)]
		for _, candidate := range children {
			child := candidate.DeepCopy()
			if visited[client.ObjectKeyFromObject(child)] || !delegatedBy(child, parent) ||
				strings.TrimSpace(child.Spec.Schedule) != "" {
				continue
			}
			visited[client.ObjectKeyFromObject(child)] = true
			members := []*corev1alpha1.Task{child}
			continuations, chainErr := r.childContinuationChain(ctx, child)
			if chainErr != nil {
				errs = append(errs, chainErr)
			}
			for _, continuation := range continuations {
				if !visited[client.ObjectKeyFromObject(continuation)] {
					visited[client.ObjectKeyFromObject(continuation)] = true
					members = append(members, continuation)
				}
			}
			for _, member := range members {
				parents = append(parents, member)
				if err := r.requestDelegatedTaskCancellation(ctx, member, message); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	return errors.Join(errs...)
}

func (r *TaskReconciler) childContinuationChain(ctx context.Context, child *corev1alpha1.Task) ([]*corev1alpha1.Task, error) {
	var chain []*corev1alpha1.Task
	current := child
	for range taskwait.MaxChainLength {
		next, err := taskwait.Continuation(ctx, r.taskMetadataReader(), current)
		if apierrors.IsNotFound(err) || errors.Is(err, taskwait.ErrChainBroken) {
			return chain, nil
		}
		if err != nil || next == nil {
			return chain, err
		}
		chain = append(chain, next)
		current = next
	}
	return chain, nil
}

func (r *TaskReconciler) requestDelegatedTaskCancellation(ctx context.Context, task *corev1alpha1.Task, message string) error {
	if !task.DeletionTimestamp.IsZero() {
		return nil
	}
	base := task.DeepCopy()
	changed := false
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	if _, requested := task.Annotations[labels.AnnotationCancelRequested]; !requested {
		if phase := task.Status.Phase; phase == "" || phase == corev1alpha1.TaskPhasePending || phase == corev1alpha1.TaskPhaseRunning {
			task.Annotations[labels.AnnotationCancelRequested] = message
			changed = true
		}
	}
	if taskwait.Active(task.Status.Wait) && strings.TrimSpace(task.Annotations[labels.AnnotationCancelWait]) == "" {
		task.Annotations[labels.AnnotationCancelWait] = message
		changed = true
	}
	if !changed {
		return nil
	}
	if err := r.Patch(ctx, task, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (r *TaskReconciler) delegatedTaskIndex(ctx context.Context, root *corev1alpha1.Task) (map[string][]corev1alpha1.Task, error) {
	selector := client.HasLabels{labels.LabelParentTask}
	var local corev1alpha1.TaskList
	if err := r.taskMetadataReader().List(ctx, &local, client.InNamespace(root.Namespace), selector); err != nil {
		return nil, err
	}
	tasks := local.Items
	if !r.EnforceNamespaceIsolation {
		var all corev1alpha1.TaskList
		switch err := r.taskMetadataReader().List(ctx, &all, selector); {
		case err == nil:
			for _, task := range all.Items {
				if task.Namespace != root.Namespace {
					tasks = append(tasks, task)
				}
			}
		case apierrors.IsForbidden(err):
		default:
			return nil, err
		}
	}
	index := make(map[string][]corev1alpha1.Task, len(tasks))
	for _, task := range tasks {
		parent := task.Labels[labels.LabelParentTask]
		index[parent] = append(index[parent], task)
	}
	return index, nil
}

func (r *TaskReconciler) requestCancelForCancelledAncestor(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	if strings.TrimSpace(task.Spec.Schedule) != "" {
		return false, nil
	}
	if _, requested := task.Annotations[labels.AnnotationCancelRequested]; requested {
		return false, nil
	}
	ancestor, err := r.stoppedAncestor(ctx, task)
	if err != nil || ancestor == "" {
		return false, err
	}
	current := task.DeepCopy()
	base := current.DeepCopy()
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[labels.AnnotationCancelRequested] = "cancelled with its ancestor task " + ancestor
	if err := r.Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return true, nil
}

func (r *TaskReconciler) stoppedAncestor(ctx context.Context, task *corev1alpha1.Task) (string, error) {
	reader := r.taskMetadataReader()
	current := task
	for range maxDelegationChainDepth {
		name := labels.ParentTaskName(current.Labels, current.Annotations)
		if name == "" {
			return "", nil
		}
		namespace, pinned := current.Namespace, ""
		if owner := metav1.GetControllerOf(current); owner != nil && owner.Kind == taskResourceKind &&
			owner.APIVersion == corev1alpha1.GroupVersion.String() && owner.Name == name {
			pinned = string(owner.UID)
		} else {
			pinned = strings.TrimSpace(current.Annotations[labels.AnnotationParentTaskUID])
			if recorded := strings.TrimSpace(current.Annotations[labels.AnnotationParentTaskNamespace]); recorded != "" {
				namespace = recorded
			}
		}
		if pinned == "" {
			return "", nil
		}
		parent := &corev1alpha1.Task{}
		if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, parent); err != nil {
			if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
				return "", nil
			}
			return "", err
		}
		if string(parent.UID) != pinned {
			return "", nil
		}
		if _, requested := parent.Annotations[labels.AnnotationCancelRequested]; requested ||
			parent.Status.Phase == corev1alpha1.TaskPhaseCancelled {
			return parent.Name, nil
		}
		if strings.TrimSpace(parent.Spec.Schedule) != "" {
			return "", nil
		}
		current = parent
	}
	return "", nil
}

func delegatedBy(child, parent *corev1alpha1.Task) bool {
	if child.Namespace == parent.Namespace {
		return metav1.IsControlledBy(child, parent)
	}
	return parent.UID != "" && strings.TrimSpace(child.Annotations[labels.AnnotationParentTaskUID]) == string(parent.UID)
}

func requestedTaskCancellation(task *corev1alpha1.Task) (string, bool) {
	message, ok := task.Annotations[labels.AnnotationCancelRequested]
	if !ok || task.Spec.Type == corev1alpha1.TaskTypeAgent ||
		(task.Status.Execution != nil && task.Status.Execution.ControllerEpoch > 0) {
		return "", false
	}
	if task.Status.Phase != corev1alpha1.TaskPhasePending && task.Status.Phase != corev1alpha1.TaskPhaseRunning {
		return "", false
	}
	if strings.TrimSpace(message) == "" {
		message = "cancelled by parent task"
	}
	return boundACPStatusMessage(message), true
}

func (r *TaskReconciler) completeTask(
	ctx context.Context,
	task *corev1alpha1.Task,
	phase corev1alpha1.TaskPhase,
	message string,
) (ctrl.Result, error) {
	return r.completeTaskWithOutcome(ctx, task, phase, phase, message, false)
}

func (r *TaskReconciler) completeExecutedTask(
	ctx context.Context,
	task *corev1alpha1.Task,
	phase corev1alpha1.TaskPhase,
	message string,
) (ctrl.Result, error) {
	if taskExecutionWorkspaceNeedsFinalization(task) {
		return r.beginTaskFinalization(ctx, task, phase, message)
	}
	return r.completeTaskWithOutcome(ctx, task, phase, phase, message, true)
}

func taskExecutionWorkspaceNeedsFinalization(task *corev1alpha1.Task) bool {
	if task == nil || task.Spec.Execution == nil || task.Spec.Execution.Workspace == nil {
		return false
	}
	status := task.Status.ExecutionWorkspace
	if status == nil {
		return true
	}
	if status.AttachedEpoch <= 0 {
		return false
	}
	condition := meta.FindStatusCondition(status.Conditions, "Attached")
	return condition == nil || condition.Status != metav1.ConditionFalse
}

func (r *TaskReconciler) beginTaskFinalization(
	ctx context.Context,
	task *corev1alpha1.Task,
	outcomePhase corev1alpha1.TaskPhase,
	message string,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if err := r.collectResult(ctx, task); err != nil {
		log.Error(err, "failed to collect result before workspace finalization")
		if errors.Is(err, errRepositoryMonitorValidationClassificationUnavailable) || store.IsTemporaryStorageError(err) {
			return ctrl.Result{}, err
		}
	}
	now := metav1.Now()
	resultRef := task.Status.ResultRef
	if err := r.updateStatusWithRetry(ctx, task, func(current *corev1alpha1.Task) {
		current.Status.Phase = corev1alpha1.TaskPhaseFinalizing
		current.Status.CompletionTime = nil
		current.Status.Message = message
		current.Status.ResultRef = resultRef
		if current.Status.ExecutionOutcome == nil {
			attempt := max(current.Status.Attempts, 1)
			current.Status.ExecutionOutcome = &corev1alpha1.TaskWorkloadExecutionOutcome{
				Phase: outcomePhase, Attempt: attempt, ResultRef: resultRef, RecordedAt: now, Message: message,
			}
		}
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type: ConditionTypeWaitingForApproval, Status: metav1.ConditionFalse, LastTransitionTime: now,
			Reason: "TaskFinalizing", Message: "task execution is settled",
		})
		meta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
			Type: ConditionTypeComplete, Status: metav1.ConditionFalse, LastTransitionTime: now,
			Reason: "TaskFinalizing", Message: "workspace authority is being revoked",
		})
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
}

func (r *TaskReconciler) completeTaskWithOutcome(
	ctx context.Context,
	task *corev1alpha1.Task,
	phase corev1alpha1.TaskPhase,
	outcomePhase corev1alpha1.TaskPhase,
	message string,
	recordOutcome bool,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	now := metav1.Now()
	task.Status.Phase = phase
	task.Status.CompletionTime = &now
	task.Status.Message = message

	decidedPhase, decidedMessage := phase, message
	retryFinalization := func(err error) (ctrl.Result, error) {
		if decidedPhase == corev1alpha1.TaskPhaseCancelled || decidedPhase == corev1alpha1.TaskPhaseFailed {
			cleanupErr := r.stopJobForDeferredFinalization(ctx, task, deferredTaskFinalization{
				Phase: decidedPhase, OutcomePhase: outcomePhase, Message: decidedMessage, RecordOutcome: recordOutcome,
			})
			err = errors.Join(err, cleanupErr)
		}
		return ctrl.Result{}, err
	}

	if s := trace.SpanFromContext(ctx); s.IsRecording() {
		s.AddEvent("phase.transition", trace.WithAttributes(
			attribute.String("task.phase", string(phase)),
		))
	}

	deferredIntent, err := r.loadDeferredTaskFinalization(ctx, task)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.collectResult(ctx, task); err != nil {
		log.Error(err, "failed to collect result")
		switch {
		case errors.Is(err, store.ErrTooLarge):
			phase, message = taskResultTooLargeOutcome(phase, message, err)
			task.Status.Phase, task.Status.Message = phase, message
		case deferredIntent != nil || errors.Is(err, errRepositoryMonitorValidationClassificationUnavailable) || store.IsTemporaryStorageError(err):
			return retryFinalization(err)
		}
	}

	var inputTokens, outputTokens int64
	if task.Spec.SessionRef != nil && task.Spec.SessionRef.Append {
		if err := r.SessionManager.AppendMessages(ctx, task, r.ResultStore); err != nil {
			log.Error(err, "failed to append session messages")
			if store.IsTemporaryStorageError(err) {
				return retryFinalization(err)
			}
		}
		var err error
		if inputTokens, outputTokens, err = r.sessionTokenUsage(ctx, task); err != nil {
			log.Error(err, "failed to read session token usage")
			inputTokens, outputTokens = 0, 0
		}
	}
	if task.Spec.SessionRef != nil {
		if err := r.SessionManager.ReleaseLockWithTokenUsage(ctx, task, inputTokens, outputTokens); err != nil {
			log.Error(err, "failed to release session lock")
			if store.IsTemporaryStorageError(err) {
				return retryFinalization(err)
			}
		}
	}

	if r.PlanStore != nil && phase != corev1alpha1.TaskPhaseCancelled {
		if err := r.PlanStore.DeletePlan(ctx, task.Namespace, task.Name); err != nil {
			log.Error(err, "failed to delete plan state on completion")
		}
	}

	conditionStatus := metav1.ConditionTrue
	reason := "TaskSucceeded"
	switch phase {
	case corev1alpha1.TaskPhaseFailed:
		conditionStatus = metav1.ConditionFalse
		reason = eventReasonTaskFailed
	case corev1alpha1.TaskPhaseCancelled:
		conditionStatus = metav1.ConditionFalse
		reason = eventReasonTaskCancelled
	}

	resultRef := task.Status.ResultRef
	if err := r.updateStatusWithRetry(ctx, task, func(t *corev1alpha1.Task) {
		t.Status.Phase = phase
		t.Status.CompletionTime = &now
		t.Status.Message = message
		t.Status.ResultRef = resultRef
		if recordOutcome && t.Status.ExecutionOutcome == nil {
			attempt := max(t.Status.Attempts, 1)
			t.Status.ExecutionOutcome = &corev1alpha1.TaskWorkloadExecutionOutcome{
				Phase:      outcomePhase,
				Attempt:    attempt,
				ResultRef:  resultRef,
				RecordedAt: now,
				Message:    decidedMessage,
			}
		}
		meta.SetStatusCondition(&t.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeWaitingForApproval,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: now,
			Reason:             reason,
			Message:            taskTerminalMessage,
		})
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
		if agent.Status.LastUsed != nil && !agent.Status.LastUsed.Before(&at) {
			return nil
		}
		agent.Status.LastUsed = &at
		return r.Status().Update(ctx, agent)
	})
}

func (r *TaskReconciler) failTask(ctx context.Context, task *corev1alpha1.Task, message string) (ctrl.Result, error) {
	return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, message)
}

func executionOutcomePreventsReplay(outcome *corev1alpha1.TaskWorkloadExecutionOutcome) bool {
	return outcome != nil
}

func (r *TaskReconciler) shouldRetry(task *corev1alpha1.Task) bool {
	if task == nil || executionOutcomePreventsReplay(task.Status.ExecutionOutcome) {
		return false
	}
	if task.Spec.RetryPolicy == nil {
		return false
	}
	return task.Status.Attempts <= task.Spec.RetryPolicy.MaxRetries
}

func (r *TaskReconciler) retryTask(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	delay := r.calculateRetryDelay(task)
	oldJobName := task.Status.JobName
	now := metav1.Now()

	if err := r.updateStatusWithRetry(ctx, task, func(t *corev1alpha1.Task) {
		t.Status.Phase = corev1alpha1.TaskPhasePending
		t.Status.JobName = ""
		t.Status.JobUID = ""
		t.Status.Message = ""
		t.Status.CompletionTime = nil
		t.Status.ResultRef = nil
		meta.SetStatusCondition(&t.Status.Conditions, metav1.Condition{
			Type: ConditionTypeJobCreated, Status: metav1.ConditionFalse, LastTransitionTime: now,
			Reason: taskRetryPendingReason, Message: "waiting for the next retry attempt",
		})
	}); err != nil {
		log.Error(err, "failed to update status for retry")
		return ctrl.Result{}, err
	}

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

func (r *TaskReconciler) retryTaskAfterFailure(ctx context.Context, task *corev1alpha1.Task, failure string) (ctrl.Result, error) {
	attempt, jobName := task.Status.Attempts, task.Status.JobName
	result, err := r.retryTask(ctx, task)
	if err != nil {
		return result, err
	}
	_ = r.recordTaskLifecycleEvent(
		ctx,
		task,
		execevents.ExecutionEventTypeTaskPhaseChanged,
		execevents.ExecutionEventSeverityWarning,
		fmt.Sprintf("Attempt %d (Job %s) failed, retrying in %s: %s", attempt, jobName, result.RequeueAfter, failure),
	)
	return result, nil
}

func (r *TaskReconciler) remainingRetryDelay(task *corev1alpha1.Task, now time.Time) time.Duration {
	if task.Spec.Type == corev1alpha1.TaskTypeAgent || task.Status.Phase != corev1alpha1.TaskPhasePending {
		return 0
	}
	condition := meta.FindStatusCondition(task.Status.Conditions, ConditionTypeJobCreated)
	if condition == nil || condition.Status != metav1.ConditionFalse || condition.Reason != taskRetryPendingReason || condition.LastTransitionTime.IsZero() {
		return 0
	}
	delay := r.calculateRetryDelay(task)
	if delay <= 0 {
		return 0
	}
	retryAt := condition.LastTransitionTime.Time.Truncate(time.Second).Add(time.Second).Add(delay)
	return max(retryAt.Sub(now), 0)
}

func (r *TaskReconciler) calculateRetryDelay(task *corev1alpha1.Task) time.Duration {
	if task.Spec.RetryPolicy == nil || task.Spec.RetryPolicy.InitialDelay == nil {
		return 10 * time.Second
	}

	initialDelay := task.Spec.RetryPolicy.InitialDelay.Duration
	multiplier := task.Spec.RetryPolicy.BackoffMultiplier
	if multiplier == 0 {
		multiplier = 2
	}

	maxDelay := 5 * time.Minute
	delay := initialDelay
	for i := int32(1); i < task.Status.Attempts; i++ {
		delay = time.Duration(float64(delay) * multiplier)
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

func (r *TaskReconciler) collectResult(ctx context.Context, task *corev1alpha1.Task) error {
	if r.ResultStore == nil {
		return nil
	}
	validationTask, err := r.repositoryMonitorValidationResultTask(ctx, task)
	if err != nil {
		return err
	}
	if validationTask {
		return nil
	}

	_, err = r.ResultStore.GetResult(ctx, task.Namespace, task.Name)
	if err == nil {
		task.Status.ResultRef = &corev1alpha1.ResultReference{
			Available: true,
		}
		return nil
	}

	if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	if taskJobIdentityRejected(task) {
		return nil
	}

	intent, err := r.loadDeferredTaskFinalization(ctx, task)
	if err != nil {
		return err
	}
	var result []byte
	var available bool
	if intent != nil {
		result, available, err = r.deferredTaskResult(ctx, task, intent)
	} else {
		result, available, err = r.taskPodResult(ctx, task, false)
	}
	if err != nil {
		return err
	}
	if !available {
		return nil
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
	if base64.StdEncoding.DecodedLen(len(payload)) > store.MaxResultBytes+2 {
		return nil, true, errTaskResultTooLarge
	}
	result, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, true, fmt.Errorf("decoding stdout task result: %w", err)
	}
	if len(result) > store.MaxResultBytes {
		return nil, true, errTaskResultTooLarge
	}
	return result, true, nil
}

var errTaskResultTooLarge = fmt.Errorf("%w: task result exceeds the %d byte limit", store.ErrTooLarge, store.MaxResultBytes)

func taskResultTooLargeOutcome(phase corev1alpha1.TaskPhase, message string, err error) (corev1alpha1.TaskPhase, string) {
	if phase == corev1alpha1.TaskPhaseSucceeded {
		phase = corev1alpha1.TaskPhaseFailed
	}
	reason := fmt.Sprintf("task result exceeds the %d byte limit and was not stored", store.MaxResultBytes)
	if errors.Is(err, errStdoutResultLogTooLarge) {
		reason = fmt.Sprintf("task log exceeds the %d byte stdout result read limit, so the result was not collected", stdoutResultLogLimitBytes)
	}
	if strings.TrimSpace(message) == "" {
		return phase, reason
	}
	return phase, message + "; " + reason
}

func (r *TaskReconciler) readPodLogs(ctx context.Context, task *corev1alpha1.Task) (string, error) {
	return r.readPodLogsWithOptions(ctx, task, fullPodLogOptions(), true)
}

func (r *TaskReconciler) readStdoutResultPodLogs(ctx context.Context, task *corev1alpha1.Task) (string, error) {
	opts := stdoutResultPodLogOptions()
	limit := *opts.LimitBytes + 1
	opts.LimitBytes = &limit
	logs, err := r.readPodLogsWithOptions(ctx, task, opts, false)
	if err != nil {
		return "", err
	}
	return boundedStdoutResultLog(logs, stdoutResultLogLimitBytes)
}

func boundedStdoutResultLog(logs string, limit int64) (string, error) {
	if int64(len(logs)) > limit {
		return "", errStdoutResultLogTooLarge
	}
	return logs, nil
}

var errStdoutResultLogTooLarge = fmt.Errorf("%w: task log exceeds the %d byte stdout result read limit", store.ErrTooLarge, stdoutResultLogLimitBytes)

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

func (r *TaskReconciler) resolveProviderRef(task *corev1alpha1.Task, agent *corev1alpha1.Agent) *corev1alpha1.ProviderReference {
	if task.Spec.Type == corev1alpha1.TaskTypeAgent {
		return nil
	}

	if task.Spec.AI != nil && task.Spec.AI.ProviderRef != nil {
		return task.Spec.AI.ProviderRef
	}

	if agent != nil && agent.Spec.ProviderRef != nil {
		return agent.Spec.ProviderRef
	}

	return nil
}

func (r *TaskReconciler) validateExecutionWorkspace(task *corev1alpha1.Task) error {
	if task.Spec.Execution == nil || task.Spec.Execution.Workspace == nil {
		return nil
	}

	ws := task.Spec.Execution.Workspace
	if ws.ClassRef == nil || strings.TrimSpace(ws.ClassRef.Name) == "" {
		return fmt.Errorf("execution workspace classRef.name is required")
	}
	if !r.WorkspaceProviderAPIEnabled {
		return fmt.Errorf("execution workspace classRef requires the workspace provider API")
	}
	if task.Spec.Type != corev1alpha1.TaskTypeAgent {
		return fmt.Errorf("execution workspace is only supported for type: agent tasks")
	}
	return validateExecutionWorkspacePolicyShape(task, ws)
}

func validateExecutionWorkspacePolicyShape(task *corev1alpha1.Task, ws *corev1alpha1.ExecutionWorkspaceSpec) error {
	if !statusrules.IsOptionalReusePolicy(ws.ReusePolicy) {
		return fmt.Errorf("unsupported execution workspace reusePolicy %q", ws.ReusePolicy)
	}

	if ws.ReusePolicy == corev1alpha1.WorkspaceReusePolicySession && (task.Spec.SessionRef == nil || task.Spec.SessionRef.Name == "") {
		return fmt.Errorf("execution workspace reusePolicy %q requires spec.sessionRef.name", ws.ReusePolicy)
	}

	return nil
}

func (r *TaskReconciler) markExecutionWorkspaceValidationFailed(ctx context.Context, task *corev1alpha1.Task, validationErr error) error {
	if task == nil || task.Spec.Execution == nil || task.Spec.Execution.Workspace == nil {
		return nil
	}

	now := metav1.Now()
	message := ""
	if validationErr != nil {
		message = validationErr.Error()
	}
	ws := task.Spec.Execution.Workspace
	failure := statusrules.ValidationFailure{
		Message:    message,
		ObservedAt: &now,
	}
	if reusePolicy, ok := executionWorkspaceStatusReusePolicy(ws); ok {
		failure.ReusePolicy = reusePolicy
	}
	status := statusrules.ValidationFailedStatus(failure)

	return r.updateStatusWithRetry(ctx, task, func(t *corev1alpha1.Task) {
		t.Status.ExecutionWorkspace = status
	})
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

func validateRuntimeRefAgentTaskRestrictions(task *corev1alpha1.Task, agent *corev1alpha1.Agent) error {
	if agent != nil && agent.Spec.Runtime != nil {
		if agent.Spec.Runtime.DefaultAllowedTools != nil {
			return fmt.Errorf("runtimeRef custom runtimes require task-level allowedTools for brokered tool exposure and do not support defaultAllowedTools policy metadata")
		}
		if agent.Spec.Runtime.DefaultAllowBash != nil {
			return fmt.Errorf("runtimeRef custom runtimes do not support defaultAllowBash policy metadata")
		}
		if strings.TrimSpace(agent.Spec.Runtime.DefaultReasoningEffort) != "" {
			return fmt.Errorf("runtimeRef custom runtimes do not support defaultReasoningEffort policy metadata")
		}
	}
	if task != nil && task.Spec.AgentRuntime != nil {
		if len(task.Spec.AgentRuntime.DisallowedTools) > 0 {
			return fmt.Errorf("runtimeRef custom runtimes do not support disallowedTools policy metadata")
		}
		if task.Spec.AgentRuntime.AllowBash != nil {
			return fmt.Errorf("runtimeRef custom runtimes do not support allowBash policy metadata")
		}
	}
	if agent != nil && agent.Spec.SecretRef != nil && strings.TrimSpace(agent.Spec.SecretRef.Name) != "" {
		return fmt.Errorf("runtimeRef custom runtimes do not support agent secretRef credential delivery")
	}
	if task != nil && task.Spec.SecretRef != nil && strings.TrimSpace(task.Spec.SecretRef.Name) != "" {
		return fmt.Errorf("runtimeRef custom runtimes do not support task secretRef credential delivery")
	}
	if task != nil && task.Spec.PriorTaskRef != nil {
		return fmt.Errorf("runtimeRef custom runtimes do not support priorTaskRef workspace handoff")
	}
	return nil
}

func validatePlannedRuntimeRefAgentTaskRestrictions(
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
	plan agentExecutionPlan,
) error {
	if err := validateSoulRuntime(agent); err != nil {
		return err
	}
	if plan.path != agentExecutionPathExternal {
		return nil
	}
	return validateHarnessV2RuntimeRefAgentTaskRestrictions(task, agent)
}

func validateHarnessV2RuntimeRefAgentTaskRestrictions(task *corev1alpha1.Task, agent *corev1alpha1.Agent) error {
	if agent != nil {
		if agent.Spec.Model != nil {
			return fmt.Errorf("runtimeRef custom runtimes do not support Agent.spec.model; provider and model configuration are fixed by the registered runtime profile")
		}
		if agent.Spec.SystemPrompt != nil {
			return fmt.Errorf("runtimeRef custom runtimes do not support Agent.spec.systemPrompt because external session creation sends no AgentConfiguration")
		}
		if len(agent.Spec.Skills) > 0 {
			return fmt.Errorf("runtimeRef custom runtimes do not support Agent.spec.skills because external session creation sends no AgentConfiguration")
		}
		for _, tool := range agent.Spec.Tools {
			if tool.Enabled == nil || *tool.Enabled {
				return fmt.Errorf("runtimeRef custom runtimes do not support enabled Agent.spec.tools; use task-level allowedTools for brokered tool exposure")
			}
		}
	}
	if agent != nil && agent.Spec.Runtime != nil {
		if agent.Spec.Runtime.DefaultMaxTurns != nil && *agent.Spec.Runtime.DefaultMaxTurns != 50 {
			return fmt.Errorf("runtimeRef custom runtimes do not support defaultMaxTurns; iteration limits are fixed by the registered runtime profile")
		}
		if agent.Spec.Runtime.DefaultAllowedTools != nil {
			return fmt.Errorf("runtimeRef custom runtimes require task-level allowedTools for brokered tool exposure and do not support defaultAllowedTools policy metadata")
		}
	}
	if task != nil && task.Spec.AgentRuntime != nil && task.Spec.AgentRuntime.MaxTurns != nil {
		return fmt.Errorf("runtimeRef custom runtimes do not support maxTurns; iteration limits are fixed by the registered runtime profile")
	}
	return nil
}

func (r *TaskReconciler) validateTaskAgentCompatibility(task *corev1alpha1.Task, agent *corev1alpha1.Agent) error {
	if err := r.Identity.ValidateAgent(agent); err != nil {
		return err
	}
	switch task.Spec.Type {
	case corev1alpha1.TaskTypeAgent:
		return r.validateAgentRuntimeTaskCompatibility(task, agent)
	case corev1alpha1.TaskTypeAI:
		return validateAITaskAgentCompatibility(task, agent)
	case corev1alpha1.TaskTypeContainer:
		return nil
	default:
		return nil
	}
}

func (r *TaskReconciler) validateAgentRuntimeTaskCompatibility(task *corev1alpha1.Task, agent *corev1alpha1.Agent) error {
	if agent == nil {
		return fmt.Errorf("type: agent tasks require an agentRef")
	}
	if agent.Spec.Runtime == nil {
		return fmt.Errorf("agent %q does not have a runtime configured (required for type: agent tasks)", agent.Name)
	}
	hasRuntimeRef := agent.Spec.Runtime.RuntimeRef != nil && strings.TrimSpace(agent.Spec.Runtime.RuntimeRef.Name) != ""
	hasBuiltInRuntime := strings.TrimSpace(string(agent.Spec.Runtime.Type)) != ""
	switch {
	case hasRuntimeRef && hasBuiltInRuntime:
		return fmt.Errorf("agent %q sets both runtime.type and runtime.runtimeRef; set exactly one", agent.Name)
	case hasRuntimeRef:
		if err := validateRuntimeRefAgentTaskRestrictions(task, agent); err != nil {
			return err
		}
	case hasBuiltInRuntime:
		if err := validateBuiltInRuntimeTaskCompatibility(task, agent); err != nil {

			return err
		}
	default:
		return fmt.Errorf("agent %q runtime must set exactly one of type or runtimeRef", agent.Name)
	}
	if !hasRuntimeRef {
		if err := validateAgentRuntimeCeiling(task, agent); err != nil {
			return err
		}
	}
	if agent.Spec.Execution != nil && agent.Spec.Execution.Workspace != nil {
		return fmt.Errorf("agent %q sets spec.execution.workspace, but execution workspace requests are only supported on Task.spec.execution.workspace", agent.Name)
	}
	if agent.Spec.ProviderRef != nil {
		return fmt.Errorf("agent %q has both runtime and providerRef set (mutually exclusive)", agent.Name)
	}
	if agent.Spec.Model != nil && agent.Spec.Model.Provider != "" {
		return fmt.Errorf("agent %q has both runtime and model.provider set (mutually exclusive for agent tasks)", agent.Name)
	}
	if len(task.Spec.Env) > 0 {
		return fmt.Errorf("type: agent ACP runtime tasks do not support arbitrary task env; use the reviewed runtime profile")
	}
	if agent.Spec.Coordination != nil && len(agent.Spec.Coordination.ApprovalRequiredTools) > 0 {
		return fmt.Errorf("agent %q approvalRequiredTools is only supported for type: ai autonomous tasks", agent.Name)
	}
	if task.Spec.Prompt == "" && (task.Spec.SessionRef == nil || !task.Spec.SessionRef.PromptIncluded ||
		strings.TrimSpace(task.Spec.SessionRef.ThroughMessageID) == "") {
		return fmt.Errorf("prompt is required for type: agent tasks unless included in a bounded Session transcript")
	}
	return nil
}

func validateBuiltInRuntimeTaskCompatibility(task *corev1alpha1.Task, agent *corev1alpha1.Agent) error {
	if err := validateBuiltInAgentRuntime(agent.Spec.Runtime.Type); err != nil {
		return err
	}
	if err := validateBuiltInACPAgentCredentialSecretRef(agent); err != nil {
		return err
	}
	if isBuiltInACPProviderRuntime(agent.Spec.Runtime.Type) && task.Spec.SecretRef != nil {
		return fmt.Errorf("built-in ACP runtime %q does not support task secretRef; provider credentials are controller-managed", agent.Spec.Runtime.Type)
	}
	if err := ValidateOpenCodeAgentSpec(agent); err != nil {
		return err
	}
	return validateReadOnlyBuiltInAgentRuntime(task, agent.Spec.Runtime.Type)
}

func validateBuiltInAgentRuntime(runtimeType corev1alpha1.AgentRuntimeType) error {
	if isBuiltInACPProviderRuntime(runtimeType) {
		return nil
	}
	return fmt.Errorf("agent runtime %q does not have an ACP runtime profile configured", runtimeType)
}

func validateReadOnlyBuiltInAgentRuntime(task *corev1alpha1.Task, runtimeType corev1alpha1.AgentRuntimeType) error {
	if !taskRequestsReadOnlyAgent(task) {
		return nil
	}
	switch runtimeType {
	case corev1alpha1.AgentRuntimeCopilot:
		return fmt.Errorf("read-only agent tasks do not support copilot runtime credentials because GITHUB_TOKEN can mutate GitHub")
	default:
		return nil
	}
}

func validateAITaskAgentCompatibility(task *corev1alpha1.Task, agent *corev1alpha1.Agent) error {
	if agent != nil && agent.Spec.Runtime != nil {
		return fmt.Errorf("agent %q has runtime configured (use type: agent instead of type: ai)", agent.Name)
	}
	if aiTaskRequestsApprovalTooling(task, agent) && !agentHasAutonomousCoordination(agent) {
		return fmt.Errorf("request_approval requires enabled autonomous coordination mode")
	}
	if agent == nil || agent.Spec.Coordination == nil {
		return nil
	}
	approvalRequiredTools := agent.Spec.Coordination.ApprovalRequiredTools
	if len(approvalRequiredTools) > 0 && (!agent.Spec.Coordination.Enabled || !agent.Spec.Coordination.Autonomous) {
		return fmt.Errorf("agent %q approvalRequiredTools requires enabled autonomous coordination mode", agent.Name)
	}
	if invalidTool := invalidApprovalRequiredBuiltInTool(approvalRequiredTools); invalidTool != "" {
		return fmt.Errorf("agent %q approvalRequiredTools cannot include built-in tool %q", agent.Name, invalidTool)
	}
	return nil
}

func aiTaskRequestsApprovalTooling(task *corev1alpha1.Task, agent *corev1alpha1.Agent) bool {
	if agent != nil {
		for _, toolRef := range agent.Spec.Tools {
			if toolRef.Enabled != nil && !*toolRef.Enabled {
				continue
			}
			if strings.TrimSpace(toolRef.Name) == "request_approval" {
				return true
			}
		}
	}
	if task != nil && task.Spec.AI != nil {
		for _, toolName := range task.Spec.AI.Tools {
			if strings.TrimSpace(toolName) == "request_approval" {
				return true
			}
		}
	}
	return false
}

func agentHasAutonomousCoordination(agent *corev1alpha1.Agent) bool {
	return agent != nil && agent.Spec.Coordination != nil && agent.Spec.Coordination.Enabled && agent.Spec.Coordination.Autonomous
}

func invalidApprovalRequiredBuiltInTool(values []string) string {
	builtIns := approvalRequiredBuiltInToolSet()
	for _, value := range values {
		toolName := strings.TrimSpace(value)
		if builtIns[toolName] {
			return toolName
		}
	}
	return ""
}

func approvalRequiredBuiltInToolSet() map[string]bool {
	builtIns := map[string]bool{}
	for _, name := range tools.KnownBuiltInToolNames() {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			builtIns[trimmed] = true
		}
	}
	return builtIns
}

func (r *TaskReconciler) handleScheduled(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if task.Spec.Suspend != nil && *task.Spec.Suspend {
		log.Info("Task is suspended, skipping schedule check")
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	}

	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
	sched, err := parser.Parse(task.Spec.Schedule)
	if err != nil {
		return r.failTask(ctx, task, fmt.Sprintf("invalid cron expression: %v", err))
	}

	now := time.Now().UTC()
	loc := time.UTC
	if task.Spec.TimeZone != nil {
		if l, err := time.LoadLocation(*task.Spec.TimeZone); err == nil {
			loc = l
			now = now.In(loc)
		}
	}

	var scheduledTime time.Time
	if task.Status.LastScheduleTime != nil {
		scheduledTime = sched.Next(task.Status.LastScheduleTime.In(loc))
	} else {
		scheduledTime = sched.Next(task.CreationTimestamp.In(loc))
	}

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

	deadlineSeconds := int64(100)
	if task.Spec.StartingDeadlineSeconds != nil {
		deadlineSeconds = *task.Spec.StartingDeadlineSeconds
	}
	if now.Sub(scheduledTime) > time.Duration(deadlineSeconds)*time.Second {
		log.Info("Missed schedule beyond deadline, skipping", "scheduledTime", scheduledTime, "deadline", deadlineSeconds)
		r.Recorder.Eventf(task, "Warning", "MissedSchedule", "Missed scheduled run at %s (deadline %ds exceeded)", scheduledTime.Format(time.RFC3339), deadlineSeconds)
		next := sched.Next(now)
		nextSchedule := metav1.NewTime(next)
		nextScheduleCopy := nextSchedule
		reanchor := metav1.NewTime(now)
		if err := r.updateStatusWithRetry(ctx, task, func(t *corev1alpha1.Task) {
			t.Status.NextScheduleTime = &nextScheduleCopy
			t.Status.LastScheduleTime = &reanchor
		}); err != nil {
			return ctrl.Result{}, fmt.Errorf("re-anchoring schedule cursor: %w", err)
		}
		return ctrl.Result{RequeueAfter: time.Until(next)}, nil
	}

	if task.Spec.ConcurrencyPolicy == corev1alpha1.ForbidConcurrent || task.Spec.ConcurrencyPolicy == "" {
		var childList corev1alpha1.TaskList
		if err := r.List(ctx, &childList, client.InNamespace(task.Namespace), client.MatchingLabels{
			labels.LabelParentTask: labels.SelectorValue(task.Name),
		}); err != nil {
			return ctrl.Result{}, fmt.Errorf("listing child tasks: %w", err)
		}
		for i := range childList.Items {
			_, settled := r.settledTaskContinuationTail(ctx, &childList.Items[i])
			if taskPhaseCountsTowardConcurrency(childList.Items[i].Status.Phase) || !settled {
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

	child.Spec.Schedule = ""
	child.Spec.TimeZone = nil
	child.Spec.ConcurrencyPolicy = ""
	child.Spec.StartingDeadlineSeconds = nil
	child.Spec.SuccessfulRunsHistoryLimit = nil
	child.Spec.FailedRunsHistoryLimit = nil
	child.Spec.Suspend = nil
	if err := agentruntimepolicy.ResolveAndMaterializeTaskRuntimeRefAllowedTools(ctx, r.taskMetadataReader(), child); err != nil {
		return ctrl.Result{}, fmt.Errorf("refreshing scheduled child AgentRuntime policy: %w", err)
	}
	tracing.StampTaskTraceContext(ctx, child)

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
		seal := ACPChildTaskSealer(r.taskMetadataReader(), task.Namespace, task.Name, string(task.UID))
		if err := seal(ctx, r.Client, child); errors.Is(err, ErrChildSealRefused) {
			log.V(1).Info("Scheduled child task requester was not sealed", "child", childName, "reason", err.Error())
		} else if err != nil {
			log.Info("Scheduled child task requester could not be sealed; its connector tools fail closed", "child", childName, "error", err.Error())
		}
	}

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

	if err := r.enforceHistoryLimits(ctx, task); err != nil {
		log.Error(err, "Failed to enforce history limits")
	}

	return ctrl.Result{RequeueAfter: time.Until(next)}, nil
}

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
		if r.taskIsLinkedContinuation(ctx, child) {
			continue
		}
		tail, settled := r.settledTaskContinuationTail(ctx, child)
		if !settled {
			continue
		}
		switch tail.Status.Phase {
		case corev1alpha1.TaskPhaseSucceeded:
			succeeded = append(succeeded, child)
		case corev1alpha1.TaskPhaseFailed:
			failed = append(failed, child)
		}
	}

	sortByCreation := func(tasks []*corev1alpha1.Task) {
		slices.SortFunc(tasks, func(a, b *corev1alpha1.Task) int {
			return a.CreationTimestamp.Compare(b.CreationTimestamp.Time)
		})
	}

	sortByCreation(succeeded)
	sortByCreation(failed)

	for i := 0; i < len(succeeded)-int(successLimit); i++ {
		if r.approvalNoticesHoldRun(ctx, succeeded[i]) {
			continue
		}
		if err := r.Delete(ctx, succeeded[i]); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting old succeeded child: %w", err)
		}
	}

	for i := 0; i < len(failed)-int(failedLimit); i++ {
		if r.approvalNoticesHoldRun(ctx, failed[i]) {
			continue
		}
		if err := r.Delete(ctx, failed[i]); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting old failed child: %w", err)
		}
	}

	return nil
}

type trustedServiceReadCleanupRunnable struct {
	reconciler *TaskReconciler
}

func (r *trustedServiceReadCleanupRunnable) Start(ctx context.Context) error {
	if r == nil || r.reconciler == nil {
		return errors.New("trusted Service RBAC cleanup reconciler is required")
	}
	if err := r.reconciler.pruneTrustedServiceReadBindingsOnce(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

func (*trustedServiceReadCleanupRunnable) NeedLeaderElection() bool { return true }

func effectiveTaskMaxConcurrentReconciles(configured int) (int, error) {
	return effectiveMaxConcurrentReconciles("task", configured, DefaultTaskMaxConcurrentReconciles, MaxTaskMaxConcurrentReconciles)
}

func effectiveMaxConcurrentReconciles(controllerName string, configured, defaultValue, maximum int) (int, error) {
	if configured == 0 {
		return defaultValue, nil
	}
	if configured < 1 || configured > maximum {
		return 0, fmt.Errorf("%s controller concurrent reconciles must be between 1 and %d, got %d", controllerName, maximum, configured)
	}
	return configured, nil
}

func (r *TaskReconciler) SetupWithManager(mgr ctrl.Manager) error {
	workers, err := effectiveTaskMaxConcurrentReconciles(r.MaxConcurrentReconciles)
	if err != nil {
		return err
	}
	r.Recorder = mgr.GetEventRecorderFor("task-controller") //nolint:staticcheck
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	if strings.TrimSpace(r.ControllerNamespace) != "" {
		if err := mgr.Add(&taskAdmissionLedgerReaper{reconciler: r, interval: taskAdmissionLedgerReapInterval}); err != nil {
			return fmt.Errorf("register task admission ledger reaper: %w", err)
		}
	}
	if !r.EnforceNamespaceIsolation {
		var cleanup manager.Runnable = &trustedServiceReadCleanupRunnable{reconciler: r}
		if err := mgr.Add(cleanup); err != nil {
			return fmt.Errorf("register trusted Service RBAC startup cleanup: %w", err)
		}
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Event{}, eventInvolvedObjectNameField, eventInvolvedObjectNameIndex); err != nil {
		return err
	}
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.Event{}, eventReasonField, eventReasonIndex); err != nil {
		return err
	}
	if r.WebhookNotifier != nil && r.webhookDeliveries == nil {
		workers := r.WebhookDeliveryWorkers
		if workers < 0 || workers > MaxTaskWebhookDeliveryWorkers {
			return fmt.Errorf("webhook delivery workers must be between 1 and %d, got %d", MaxTaskWebhookDeliveryWorkers, workers)
		}
		r.webhookDeliveries = newTaskWebhookDispatcher(r.WebhookNotifier, uncachedReader(r.APIReader, r.Client), r.Status(), workers)
		if err := mgr.Add(r.webhookDeliveries); err != nil {
			return fmt.Errorf("register Task webhook delivery: %w", err)
		}
	}
	builder := ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Task{}).
		Owns(&batchv1.Job{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&corev1alpha1.Task{})
	builder, err = registerACPTaskWakeWatches(context.Background(), mgr, builder)
	if err != nil {
		return fmt.Errorf("register ACP workspace Task wakeups: %w", err)
	}
	if informer, informerErr := mgr.GetCache().GetInformer(context.Background(), &corev1alpha1.Task{}); informerErr == nil {
		if _, handlerErr := informer.AddEventHandler(toolscache.ResourceEventHandlerDetailedFuncs{
			AddFunc: acpPreDispatchTaskAdded,
		}); handlerErr != nil {
			mgr.GetLogger().Error(handlerErr, "ACP pre-dispatch informer timing unavailable")
		}
	}
	return builder.Named("task").
		WithOptions(controllerpkg.Options{MaxConcurrentReconciles: workers}).
		Complete(r)
}

const (
	DefaultAIWorkerClusterRoleName        = "orka-ai-worker-role"
	DefaultVendorWorkerClusterRoleName    = "orka-vendor-worker-role"
	DefaultContainerWorkerClusterRoleName = "orka-container-worker-role"

	maxWorkerRoleBindingNameLength = 253
	workerRoleBindingHashLength    = 10

	managedByLabelKey                         = "app.kubernetes.io/managed-by"
	managedByLabelValue                       = "orka"
	orkaManagedByLabelKey                     = "orka.ai/managed-by"
	trustedServiceReaderLabelKey              = "orka.ai/trusted-service-reader"
	trustedServiceReaderTaskNamespaceLabelKey = "orka.ai/trusted-service-reader-for"
	trustedServiceReaderLabelValue            = booleanTrueValue
)

type workerRBACSpec struct {
	serviceAccountName string
	clusterRoleName    string
	roleBindingName    string
}

func (r *TaskReconciler) aiWorkerServiceAccountName() string {
	return workerServiceAccountName(r.AIWorkerServiceAccountName, AIWorkerServiceAccount)
}

func (r *TaskReconciler) workerRBACSpecs(namespace string) []workerRBACSpec {
	return []workerRBACSpec{
		{
			serviceAccountName: r.aiWorkerServiceAccountName(),
			clusterRoleName:    workerClusterRoleName(r.AIWorkerClusterRoleName, DefaultAIWorkerClusterRoleName),
			roleBindingName:    workerRoleBindingName(r.WorkerRoleBindingNamePrefix, "ai", namespace),
		},
		{
			serviceAccountName: workerServiceAccountName(r.VendorWorkerServiceAccountName, VendorWorkerServiceAccount),
			clusterRoleName:    workerClusterRoleName(r.VendorWorkerClusterRoleName, DefaultVendorWorkerClusterRoleName),
			roleBindingName:    workerRoleBindingName(r.WorkerRoleBindingNamePrefix, "vendor", namespace),
		},
		{
			serviceAccountName: workerServiceAccountName(r.ContainerWorkerServiceAccountName, ContainerWorkerServiceAccount),
			clusterRoleName:    workerClusterRoleName(r.ContainerWorkerClusterRoleName, DefaultContainerWorkerClusterRoleName),
			roleBindingName:    workerRoleBindingName(r.WorkerRoleBindingNamePrefix, "container", namespace),
		},
	}
}

func workerClusterRoleName(configured, fallback string) string {
	if configured != "" {
		return configured
	}
	return fallback
}

func workerRoleBindingName(prefix, tier, namespace string) string {
	if prefix == "" {
		prefix = managedByLabelValue
	}
	name := fmt.Sprintf("%s-%s-worker-%s", prefix, tier, namespace)
	if len(name) <= maxWorkerRoleBindingNameLength {
		return name
	}

	sum := sha256.Sum256([]byte(name))
	suffix := hex.EncodeToString(sum[:])[:workerRoleBindingHashLength]
	prefixLength := maxWorkerRoleBindingNameLength - workerRoleBindingHashLength - 1
	return fmt.Sprintf("%s-%s", name[:prefixLength], suffix)
}

func (r *TaskReconciler) ensureWorkerRBAC(ctx context.Context, namespace string) error {
	if err := r.ensureTrustedServiceReadBindings(ctx, namespace); err != nil {
		return err
	}
	for _, spec := range r.workerRBACSpecs(namespace) {
		if err := r.ensureWorkerServiceAccount(ctx, namespace, spec.serviceAccountName); err != nil {
			return err
		}
		if err := r.ensureWorkerRoleBinding(ctx, namespace, spec); err != nil {
			return err
		}
	}

	return nil
}

func (r *TaskReconciler) trustedServiceReadReader() client.Reader {
	if r != nil && r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *TaskReconciler) ensureTrustedServiceReadBindings(ctx context.Context, taskNamespace string) error {
	r.trustedServiceCleanupMu.RLock()
	defer r.trustedServiceCleanupMu.RUnlock()
	refs := append(r.OutboundAccessTrust.Gateways.References(), r.OutboundAccessTrust.TokenEndpoints.References()...)
	desired := map[types.NamespacedName]struct{}{}
	for _, ref := range refs {
		if ref.Namespace == "" || ref.Namespace == taskNamespace {
			continue
		}
		key := types.NamespacedName{
			Namespace: ref.Namespace,
			Name: trustedServiceReadBindingName(
				taskNamespace,
				ref.Namespace,
				ref.Name,
				r.aiWorkerServiceAccountName(),
			),
		}
		if _, ok := desired[key]; ok {
			continue
		}
		desired[key] = struct{}{}
		objectLabels := trustedServiceReadBindingLabels(taskNamespace)
		role := &rbacv1.Role{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: maps.Clone(objectLabels)},
			Rules: []rbacv1.PolicyRule{{
				APIGroups: []string{""}, Resources: []string{"services"}, ResourceNames: []string{ref.Name}, Verbs: []string{"get"},
			}},
		}
		if err := r.createOrUpdateTrustedServiceRole(ctx, role); err != nil {
			return err
		}
		binding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, Labels: maps.Clone(objectLabels)},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: rbacRoleKind, Name: key.Name},
			Subjects: []rbacv1.Subject{{
				Kind: rbacv1.ServiceAccountKind, Name: r.aiWorkerServiceAccountName(), Namespace: taskNamespace,
			}},
		}
		if err := r.createOrUpdateTrustedServiceRoleBinding(ctx, binding); err != nil {
			return err
		}
	}
	if r.EnforceNamespaceIsolation {
		return nil
	}
	return r.pruneTrustedServiceReadBindings(ctx, taskNamespace, desired)
}

func trustedServiceReadBindingName(taskNamespace, serviceNamespace, serviceName string, serviceAccountNames ...string) string {
	serviceAccountName := AIWorkerServiceAccount
	if len(serviceAccountNames) > 0 && strings.TrimSpace(serviceAccountNames[0]) != "" {
		serviceAccountName = strings.TrimSpace(serviceAccountNames[0])
	}
	sum := sha256.Sum256([]byte(taskNamespace + "\x00" + serviceNamespace + "\x00" + serviceName + "\x00" + serviceAccountName))
	return "orka-outbound-service-" + hex.EncodeToString(sum[:])[:16]
}

func trustedServiceReadBindingLabels(taskNamespace string) map[string]string {
	return map[string]string{
		orkaManagedByLabelKey:                     managedByLabelValue,
		trustedServiceReaderLabelKey:              trustedServiceReaderLabelValue,
		trustedServiceReaderTaskNamespaceLabelKey: taskNamespace,
	}
}

func (r *TaskReconciler) pruneTrustedServiceReadBindings(
	ctx context.Context,
	taskNamespace string,
	desired map[types.NamespacedName]struct{},
) error {
	selector := client.MatchingLabels{
		orkaManagedByLabelKey:                     managedByLabelValue,
		trustedServiceReaderLabelKey:              trustedServiceReaderLabelValue,
		trustedServiceReaderTaskNamespaceLabelKey: taskNamespace,
	}
	bindings := &rbacv1.RoleBindingList{}
	if err := r.trustedServiceReadReader().List(ctx, bindings, selector); err != nil {
		return fmt.Errorf("listing trusted Service RoleBindings for namespace %q: %w", taskNamespace, err)
	}
	for i := range bindings.Items {
		binding := &bindings.Items[i]
		key := types.NamespacedName{Namespace: binding.Namespace, Name: binding.Name}
		if _, ok := desired[key]; ok || !strings.HasPrefix(binding.Name, "orka-outbound-service-") {
			continue
		}
		if err := r.Delete(ctx, binding); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting stale trusted Service RoleBinding %s/%s: %w", binding.Namespace, binding.Name, err)
		}
	}
	roles := &rbacv1.RoleList{}
	if err := r.trustedServiceReadReader().List(ctx, roles, selector); err != nil {
		return fmt.Errorf("listing trusted Service Roles for namespace %q: %w", taskNamespace, err)
	}
	for i := range roles.Items {
		role := &roles.Items[i]
		key := types.NamespacedName{Namespace: role.Namespace, Name: role.Name}
		if _, ok := desired[key]; ok || !strings.HasPrefix(role.Name, "orka-outbound-service-") {
			continue
		}
		if err := r.Delete(ctx, role); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting stale trusted Service Role %s/%s: %w", role.Namespace, role.Name, err)
		}
	}
	return nil
}

func (r *TaskReconciler) pruneTrustedServiceReadBindingsOnce(ctx context.Context) error {
	r.trustedServiceCleanupMu.Lock()
	defer r.trustedServiceCleanupMu.Unlock()
	if r.trustedServiceCleanupDone {
		return nil
	}
	activeTaskNamespaces, err := r.activeTaskNamespaces(ctx)
	if err != nil {
		return err
	}
	bindings := &rbacv1.RoleBindingList{}
	if err := r.trustedServiceReadReader().List(ctx, bindings); err != nil {
		return fmt.Errorf("listing trusted Service RoleBindings during startup cleanup: %w", err)
	}
	for i := range bindings.Items {
		binding := &bindings.Items[i]
		taskNamespace, ok := trustedServiceReadRoleBindingTaskNamespace(binding)
		if !ok {
			continue
		}
		key := types.NamespacedName{Namespace: binding.Namespace, Name: binding.Name}
		_, active := activeTaskNamespaces[taskNamespace]
		if active && r.trustedServiceReadBindingDesired(taskNamespace, key) {
			continue
		}
		if err := r.deleteTrustedServiceReadGrant(ctx, binding, taskNamespace); err != nil {
			return err
		}
	}
	roles := &rbacv1.RoleList{}
	if err := r.trustedServiceReadReader().List(ctx, roles, client.MatchingLabels{
		orkaManagedByLabelKey:        managedByLabelValue,
		trustedServiceReaderLabelKey: trustedServiceReaderLabelValue,
	}); err != nil {
		return fmt.Errorf("listing managed trusted Service Roles during startup cleanup: %w", err)
	}
	for i := range roles.Items {
		role := &roles.Items[i]
		taskNamespace := strings.TrimSpace(role.Labels[trustedServiceReaderTaskNamespaceLabelKey])
		if taskNamespace == "" || !strings.HasPrefix(role.Name, "orka-outbound-service-") {
			continue
		}
		key := types.NamespacedName{Namespace: role.Namespace, Name: role.Name}
		_, active := activeTaskNamespaces[taskNamespace]
		if active && r.trustedServiceReadBindingDesired(taskNamespace, key) {
			continue
		}
		if err := r.Delete(ctx, role); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting stale managed trusted Service Role %s/%s: %w", role.Namespace, role.Name, err)
		}
	}
	r.trustedServiceCleanupDone = true
	return nil
}

func (r *TaskReconciler) activeTaskNamespaces(ctx context.Context) (map[string]struct{}, error) {
	tasks := &corev1alpha1.TaskList{}
	if err := r.trustedServiceReadReader().List(ctx, tasks); err != nil {
		return nil, fmt.Errorf("listing Tasks during trusted Service RBAC startup cleanup: %w", err)
	}
	active := make(map[string]struct{}, len(tasks.Items))
	for i := range tasks.Items {
		if !tasks.Items[i].DeletionTimestamp.IsZero() {
			continue
		}
		if namespace := strings.TrimSpace(tasks.Items[i].Namespace); namespace != "" {
			active[namespace] = struct{}{}
		}
	}
	return active, nil
}

func (r *TaskReconciler) cleanupTrustedServiceReadBindingsAfterTaskRemoval(
	ctx context.Context,
	taskNamespace string,
) error {
	if r.EnforceNamespaceIsolation {
		return nil
	}
	taskNamespace = strings.TrimSpace(taskNamespace)
	if taskNamespace == "" {
		return nil
	}
	r.trustedServiceCleanupMu.Lock()
	defer r.trustedServiceCleanupMu.Unlock()
	tasks := &corev1alpha1.TaskList{}
	if err := r.trustedServiceReadReader().List(ctx, tasks, client.InNamespace(taskNamespace)); err != nil {
		return fmt.Errorf("listing Tasks before trusted Service RBAC cleanup for namespace %q: %w", taskNamespace, err)
	}
	for i := range tasks.Items {
		if tasks.Items[i].DeletionTimestamp.IsZero() {
			return nil
		}
	}
	return r.deleteTrustedServiceReadBindingsForNamespaceLocked(ctx, taskNamespace)
}

func (r *TaskReconciler) deleteTrustedServiceReadBindingsForNamespaceLocked(
	ctx context.Context,
	taskNamespace string,
) error {
	selector := client.MatchingLabels{
		orkaManagedByLabelKey:                     managedByLabelValue,
		trustedServiceReaderLabelKey:              trustedServiceReaderLabelValue,
		trustedServiceReaderTaskNamespaceLabelKey: taskNamespace,
	}
	bindings := &rbacv1.RoleBindingList{}
	if err := r.trustedServiceReadReader().List(ctx, bindings, selector); err != nil {
		return fmt.Errorf("listing trusted Service RoleBindings for inactive namespace %q: %w", taskNamespace, err)
	}
	for i := range bindings.Items {
		binding := &bindings.Items[i]
		if !strings.HasPrefix(binding.Name, "orka-outbound-service-") {
			continue
		}
		if err := r.deleteTrustedServiceReadGrant(ctx, binding, taskNamespace); err != nil {
			return err
		}
	}
	roles := &rbacv1.RoleList{}
	if err := r.trustedServiceReadReader().List(ctx, roles, selector); err != nil {
		return fmt.Errorf("listing trusted Service Roles for inactive namespace %q: %w", taskNamespace, err)
	}
	for i := range roles.Items {
		role := &roles.Items[i]
		if !strings.HasPrefix(role.Name, "orka-outbound-service-") {
			continue
		}
		if err := r.Delete(ctx, role); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting trusted Service Role %s/%s for inactive namespace %q: %w", role.Namespace, role.Name, taskNamespace, err)
		}
	}
	return nil
}

func (r *TaskReconciler) trustedServiceReadBindingDesired(
	taskNamespace string,
	key types.NamespacedName,
) bool {
	refs := append(r.OutboundAccessTrust.Gateways.References(), r.OutboundAccessTrust.TokenEndpoints.References()...)
	for _, ref := range refs {
		if ref.Namespace == "" || ref.Namespace == taskNamespace || ref.Namespace != key.Namespace {
			continue
		}
		if key.Name == trustedServiceReadBindingName(taskNamespace, ref.Namespace, ref.Name, r.aiWorkerServiceAccountName()) {
			return true
		}
	}
	return false
}

func (r *TaskReconciler) deleteTrustedServiceReadGrant(
	ctx context.Context,
	binding *rbacv1.RoleBinding,
	taskNamespace string,
) error {
	key := types.NamespacedName{Namespace: binding.Namespace, Name: binding.Name}
	role := &rbacv1.Role{}
	if err := r.trustedServiceReadReader().Get(ctx, key, role); err == nil {
		serviceAccountName := ""
		if len(binding.Subjects) == 1 {
			subject := binding.Subjects[0]
			if subject.Kind == rbacv1.ServiceAccountKind && subject.APIGroup == "" && subject.Namespace == taskNamespace {
				serviceAccountName = subject.Name
			}
		}
		if trustedServiceReadRoleOwned(role, taskNamespace, serviceAccountName) {
			if err := r.Delete(ctx, role); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("deleting trusted Service Role %s/%s: %w", role.Namespace, role.Name, err)
			}
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("getting trusted Service Role %s/%s: %w", key.Namespace, key.Name, err)
	}
	if err := r.Delete(ctx, binding); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting trusted Service RoleBinding %s/%s: %w", binding.Namespace, binding.Name, err)
	}
	return nil
}

func trustedServiceReadManaged(objectLabels map[string]string) bool {
	return objectLabels[orkaManagedByLabelKey] == managedByLabelValue &&
		objectLabels[trustedServiceReaderLabelKey] == trustedServiceReaderLabelValue
}

func trustedServiceReadRoleBindingTaskNamespace(binding *rbacv1.RoleBinding) (string, bool) {
	if binding != nil && trustedServiceReadManaged(binding.Labels) &&
		strings.HasPrefix(binding.Name, "orka-outbound-service-") {
		taskNamespace := strings.TrimSpace(binding.Labels[trustedServiceReaderTaskNamespaceLabelKey])
		if taskNamespace != "" {
			return taskNamespace, true
		}
	}
	return legacyTrustedServiceReadRoleBindingTaskNamespace(binding)
}

func legacyTrustedServiceReadRoleBindingTaskNamespace(binding *rbacv1.RoleBinding) (string, bool) {
	if binding == nil || !legacyTrustedServiceReadName(binding.Name) ||
		binding.RoleRef != (rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: rbacRoleKind, Name: binding.Name}) ||
		len(binding.Subjects) != 1 {
		return "", false
	}
	subject := binding.Subjects[0]
	if subject.Kind != rbacv1.ServiceAccountKind || subject.APIGroup != "" ||
		subject.Name != AIWorkerServiceAccount || strings.TrimSpace(subject.Namespace) == "" {
		return "", false
	}
	return subject.Namespace, true
}

func legacyTrustedServiceReadName(name string) bool {
	const prefix = "orka-outbound-service-"
	suffix := strings.TrimPrefix(name, prefix)
	if suffix == name || len(suffix) != 16 || suffix != strings.ToLower(suffix) {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}

func trustedServiceReadRoleOwned(role *rbacv1.Role, taskNamespace string, serviceAccountNames ...string) bool {
	if role != nil && trustedServiceReadManaged(role.Labels) &&
		role.Labels[trustedServiceReaderTaskNamespaceLabelKey] == taskNamespace &&
		strings.HasPrefix(role.Name, "orka-outbound-service-") {
		return true
	}
	return len(serviceAccountNames) > 0 && trustedServiceReadRoleMatches(role, taskNamespace, serviceAccountNames[0])
}

func trustedServiceReadRoleMatches(role *rbacv1.Role, taskNamespace, serviceAccountName string) bool {
	if !trustedServiceReadRoleRuleValid(role) || strings.TrimSpace(serviceAccountName) == "" {
		return false
	}
	return role.Name == trustedServiceReadBindingName(taskNamespace, role.Namespace, role.Rules[0].ResourceNames[0], serviceAccountName)
}

func trustedServiceReadRoleRuleValid(role *rbacv1.Role) bool {
	if role == nil || !legacyTrustedServiceReadName(role.Name) || len(role.Rules) != 1 {
		return false
	}
	rule := role.Rules[0]
	if !slices.Equal(rule.APIGroups, []string{""}) ||
		!slices.Equal(rule.Resources, []string{"services"}) ||
		len(rule.ResourceNames) != 1 || strings.TrimSpace(rule.ResourceNames[0]) == "" ||
		!slices.Equal(rule.Verbs, []string{"get"}) || len(rule.NonResourceURLs) != 0 {
		return false
	}
	return true
}

func (r *TaskReconciler) createOrUpdateTrustedServiceRole(ctx context.Context, desired *rbacv1.Role) error {
	current := &rbacv1.Role{}
	err := r.trustedServiceReadReader().Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, current)
	if apierrors.IsNotFound(err) {
		if err = r.Create(ctx, desired); !apierrors.IsAlreadyExists(err) {
			return err
		}
		err = r.trustedServiceReadReader().Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, current)
	}
	if err != nil {
		return err
	}
	if reflect.DeepEqual(current.Rules, desired.Rules) && reflect.DeepEqual(current.Labels, desired.Labels) {
		return nil
	}
	return fmt.Errorf("trusted Service Role %s/%s already exists with unexpected ownership or permissions", desired.Namespace, desired.Name)
}

func (r *TaskReconciler) createOrUpdateTrustedServiceRoleBinding(ctx context.Context, desired *rbacv1.RoleBinding) error {
	current := &rbacv1.RoleBinding{}
	err := r.trustedServiceReadReader().Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, current)
	if apierrors.IsNotFound(err) {
		if err = r.Create(ctx, desired); !apierrors.IsAlreadyExists(err) {
			return err
		}
		err = r.trustedServiceReadReader().Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, current)
	}
	if err != nil {
		return err
	}
	if current.RoleRef == desired.RoleRef &&
		reflect.DeepEqual(current.Subjects, desired.Subjects) &&
		reflect.DeepEqual(current.Labels, desired.Labels) {
		return nil
	}
	return fmt.Errorf("trusted Service RoleBinding %s/%s already exists with unexpected ownership or subjects", desired.Namespace, desired.Name)
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
					orkaManagedByLabelKey:        managedByLabelValue,
					labels.LabelPlatformIdentity: labels.PlatformIdentityValue,
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
	if sa.Labels[orkaManagedByLabelKey] != managedByLabelValue || sa.Labels[labels.LabelPlatformIdentity] != labels.PlatformIdentityValue {
		sa.Labels[orkaManagedByLabelKey] = managedByLabelValue
		sa.Labels[labels.LabelPlatformIdentity] = labels.PlatformIdentityValue
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
	err := r.Get(ctx, types.NamespacedName{Name: spec.roleBindingName, Namespace: namespace}, rb)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return fmt.Errorf("creating worker RoleBinding %s/%s: %w", namespace, spec.roleBindingName, err)
			}
			if err := r.Get(ctx, types.NamespacedName{Name: spec.roleBindingName, Namespace: namespace}, rb); err != nil {
				return fmt.Errorf("getting worker RoleBinding %s/%s after create conflict: %w", namespace, spec.roleBindingName, err)
			}
		} else {
			log.Info("Created worker RoleBinding", "namespace", namespace, "binding", spec.roleBindingName, "serviceAccount", spec.serviceAccountName, "clusterRole", spec.clusterRoleName)
			return nil
		}
	} else if err != nil {
		return fmt.Errorf("getting worker RoleBinding %s/%s: %w", namespace, spec.roleBindingName, err)
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
			return fmt.Errorf("updating worker RoleBinding %s/%s: %w", namespace, spec.roleBindingName, err)
		}
		log.Info("Updated worker RoleBinding", "namespace", namespace, "binding", spec.roleBindingName, "serviceAccount", spec.serviceAccountName, "clusterRole", spec.clusterRoleName)
	}

	return nil
}

func (r *TaskReconciler) deleteExactStaleWorkerRoleBinding(ctx context.Context, binding *rbacv1.RoleBinding) error {
	uid := binding.UID
	options := []client.DeleteOption{}
	if uid != "" {
		options = append(options, client.Preconditions{UID: &uid})
	}
	if err := r.Delete(ctx, binding, options...); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return err
	}
	return nil
}

func (r *TaskReconciler) recreateWorkerRoleBinding(ctx context.Context, namespace string, spec workerRBACSpec, current, desired *rbacv1.RoleBinding) (*rbacv1.RoleBinding, error) {
	log := logf.FromContext(ctx)
	log.Info("Recreating worker RoleBinding with stale RoleRef", "namespace", namespace, "binding", spec.roleBindingName, "currentKind", current.RoleRef.Kind, "currentName", current.RoleRef.Name, "desiredKind", desired.RoleRef.Kind, "desiredName", desired.RoleRef.Name)

	if err := r.deleteExactStaleWorkerRoleBinding(ctx, current); err != nil {
		return nil, fmt.Errorf("deleting worker RoleBinding %s/%s with stale RoleRef %s/%s: %w", namespace, spec.roleBindingName, current.RoleRef.Kind, current.RoleRef.Name, err)
	}

	var recreated *rbacv1.RoleBinding
	err := wait.PollUntilContextTimeout(ctx, workerRoleBindingRecreateInterval, workerRoleBindingRecreateTimeout, true, func(ctx context.Context) (bool, error) {
		latest := &rbacv1.RoleBinding{}
		err := r.Get(ctx, types.NamespacedName{Name: spec.roleBindingName, Namespace: namespace}, latest)
		if err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("getting worker RoleBinding %s/%s while waiting for stale RoleRef deletion: %w", namespace, spec.roleBindingName, err)
		}

		if err == nil {
			if latest.RoleRef == desired.RoleRef {
				recreated = latest
				return true, nil
			}

			if err := r.deleteExactStaleWorkerRoleBinding(ctx, latest); err != nil {
				return false, fmt.Errorf("deleting worker RoleBinding %s/%s with stale RoleRef %s/%s during retry: %w", namespace, spec.roleBindingName, latest.RoleRef.Kind, latest.RoleRef.Name, err)
			}
			return false, nil
		}

		create := desired.DeepCopy()
		if err := r.Create(ctx, create); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return false, nil
			}
			return false, fmt.Errorf("recreating worker RoleBinding %s/%s with RoleRef %s/%s: %w", namespace, spec.roleBindingName, desired.RoleRef.Kind, desired.RoleRef.Name, err)
		}

		recreated = create
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("recreating worker RoleBinding %s/%s after stale RoleRef %s/%s: %w", namespace, spec.roleBindingName, current.RoleRef.Kind, current.RoleRef.Name, err)
	}

	log.Info("Recreated worker RoleBinding", "namespace", namespace, "binding", spec.roleBindingName, "serviceAccount", spec.serviceAccountName, "clusterRole", spec.clusterRoleName)
	return recreated, nil
}

func workerRoleBinding(namespace string, spec workerRBACSpec) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.roleBindingName,
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

func (r *TaskReconciler) isAutonomousTask(ctx context.Context, task *corev1alpha1.Task) bool {
	autonomous, _ := r.taskAutonomousMode(ctx, task)
	return autonomous
}

func (r *TaskReconciler) taskAutonomousMode(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	if task.Spec.AgentRef == nil || isMemoryConsolidationTask(task) {
		return false, nil
	}

	agent := &corev1alpha1.Agent{}
	agentNS := task.Namespace
	if task.Spec.AgentRef.Namespace != "" {
		agentNS = task.Spec.AgentRef.Namespace
	}
	if err := r.Get(ctx, types.NamespacedName{Name: task.Spec.AgentRef.Name, Namespace: agentNS}, agent); err != nil {
		return false, err
	}

	return agent.Spec.Coordination != nil && agent.Spec.Coordination.Autonomous, nil
}

func (r *TaskReconciler) pendingApprovalsForTask(ctx context.Context, task *corev1alpha1.Task) ([]approvals.Approval, error) {
	if r == nil || r.ExecutionEventStore == nil || task == nil {
		return nil, nil
	}
	listed, err := approvals.ListEvents(ctx, r.ExecutionEventStore, task.Namespace, task.Name)
	if err != nil {
		return nil, err
	}
	return approvals.Pending(approvals.FilterEventsForTaskUID(listed, string(task.UID)), time.Time{}), nil
}

func (r *TaskReconciler) parkOnPendingApproval(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, bool, error) {
	log := logf.FromContext(ctx)
	pending, err := r.pendingApprovalsForTask(ctx, task)
	if err != nil {
		log.Error(err, "failed to derive pending approvals")
		return ctrl.Result{}, false, err
	}
	if len(pending) == 0 {
		if setWaitingForApprovalCondition(task, nil) {
			if err := r.Status().Update(ctx, task); err != nil {
				return ctrl.Result{}, false, err
			}
		}
		return ctrl.Result{}, false, nil
	}
	approval := pending[0]
	log.Info(
		"autonomous task waiting for approval",
		"approvalID", approval.ID,
		"targetTool", approval.TargetTool,
		"iteration", task.Status.Iteration,
	)
	waitingMessage := fmt.Sprintf(
		"waiting for approval %s for %s at iteration %d",
		approval.ID,
		approvalTargetName(approval),
		task.Status.Iteration,
	)
	conditionChanged := setWaitingForApprovalCondition(task, pending)
	if task.Status.Message != waitingMessage || conditionChanged {
		task.Status.Message = waitingMessage
		if err := r.Status().Update(ctx, task); err != nil {
			return ctrl.Result{}, false, err
		}
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, true, nil
}

const (
	waitingForApprovalReasonPending  = "ApprovalPending"
	waitingForApprovalReasonResolved = "ApprovalResolved"
)

func approvalTargetName(approval approvals.Approval) string {
	if approval.TargetTool != "" {
		return approval.TargetTool
	}
	if approval.Action != "" {
		return approval.Action
	}
	return "requested action"
}

func setWaitingForApprovalCondition(task *corev1alpha1.Task, pending []approvals.Approval) bool {
	existing := meta.FindStatusCondition(task.Status.Conditions, ConditionTypeWaitingForApproval)
	condition := metav1.Condition{Type: ConditionTypeWaitingForApproval, ObservedGeneration: task.Generation, LastTransitionTime: metav1.Now()}
	switch {
	case len(pending) > 0:
		condition.Status, condition.Reason = metav1.ConditionTrue, waitingForApprovalReasonPending
		condition.Message = fmt.Sprintf("waiting for approval %s for %s", pending[0].ID, approvalTargetName(pending[0]))
		if len(pending) > 1 {
			condition.Message += fmt.Sprintf(" and %d more", len(pending)-1)
		}
	case existing != nil && existing.Status == metav1.ConditionTrue:
		condition.Status, condition.Reason = metav1.ConditionFalse, waitingForApprovalReasonResolved
		condition.Message = "no approval is pending"
	default:
		return false
	}
	if existing != nil && existing.Status == condition.Status && existing.Reason == condition.Reason &&
		existing.Message == condition.Message && existing.ObservedGeneration == condition.ObservedGeneration {
		return false
	}
	meta.SetStatusCondition(&task.Status.Conditions, condition)
	return true
}

func (r *TaskReconciler) syncACPWaitingForApproval(ctx context.Context, task *corev1alpha1.Task) error {
	if r == nil || r.ExecutionEventStore == nil {
		return nil
	}
	listed, err := approvals.ListEvents(ctx, r.ExecutionEventStore, task.Namespace, task.Name)
	if err != nil {
		return err
	}
	pending := withoutParkedApprovals(approvals.Pending(approvals.FilterEventsForTaskUID(listed, string(task.UID)), time.Now().UTC()))
	if !setWaitingForApprovalCondition(task.DeepCopy(), pending) {
		return nil
	}
	reader := uncachedReader(r.APIReader, r.Client)
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &corev1alpha1.Task{}
		if err := reader.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
			return err
		}
		if current.UID != task.UID || current.Status.Phase != corev1alpha1.TaskPhaseRunning ||
			!setWaitingForApprovalCondition(current, pending) {
			return nil
		}
		if err := r.Status().Update(ctx, current); err != nil {
			return err
		}
		task.ResourceVersion, task.Status = current.ResourceVersion, current.Status
		return nil
	})
}

func (r *TaskReconciler) settledTaskContinuationTail(ctx context.Context, task *corev1alpha1.Task) (*corev1alpha1.Task, bool) {
	if task == nil || task.Status.Wait == nil {
		return task, true
	}
	if taskwait.Active(task.Status.Wait) {
		return nil, false
	}
	if task.Status.Wait.State != corev1alpha1.TaskWaitStateResumed {
		return task, true
	}
	tail, err := taskwait.ResolveTail(ctx, uncachedReader(r.APIReader, r.Client), task)
	if apierrors.IsNotFound(err) {
		return task, true
	}
	if err != nil || !taskwait.Settled(tail) {
		return nil, false
	}
	return tail, true
}

func (r *TaskReconciler) approvalNoticesHoldRun(ctx context.Context, head *corev1alpha1.Task) bool {
	if r.WebhookNotifier == nil || head == nil {
		return false
	}
	reader := uncachedReader(r.APIReader, r.Client)
	current := &corev1alpha1.Task{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(head), current); err != nil {
		return !apierrors.IsNotFound(err)
	}
	if current.UID != head.UID {
		return true
	}
	storeFailed := false
	now := r.WebhookNotifier.now()
	for range taskwait.MaxChainLength + 1 {
		if firstPendingApprovalNotice(current.Status.ApprovalWebhooks) >= 0 {
			return true
		}
		if current.Status.Phase.IsTerminal() {
			if outstanding, _ := terminalNoticeOutstanding(current, now); outstanding {
				return true
			}
		}
		unprojected, err := r.approvalEventsAwaitProjection(ctx, current)
		if err != nil {
			logf.FromContext(ctx).Error(err, "failed to read approval events for scheduled run history", "task", current.Name)
			storeFailed = true
		} else if unprojected {
			return true
		}
		next, err := taskwait.Continuation(ctx, reader, current)
		switch {
		case apierrors.IsNotFound(err):
			return storeFailed && r.approvalEventReadFailureHolds(current)
		case err != nil:
			return true
		case next == nil:
			return storeFailed && r.approvalEventReadFailureHolds(current)
		}
		current = next
	}
	return true
}

var approvalWebhookTransitionEventTypes = []string{
	execevents.ExecutionEventTypeApprovalRequested,
	execevents.ExecutionEventTypeApprovalApproved,
	execevents.ExecutionEventTypeApprovalDeclined,
	execevents.ExecutionEventTypeApprovalExpired,
	execevents.ExecutionEventTypeApprovalCancelled,
}

func (r *TaskReconciler) approvalEventsAwaitProjection(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	state := task.Status.ApprovalWebhooks
	selected := selectedApprovalWebhookEvents(task)
	if r.ExecutionEventStore == nil || task.UID == "" || !task.DeletionTimestamp.IsZero() ||
		len(selected) == 0 || state == nil || !state.Tracking || state.Overflowed {
		return false, nil
	}
	next, err := r.ExecutionEventStore.ListExecutionEvents(ctx, store.ExecutionEventFilter{
		Namespace:  task.Namespace,
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   task.Name,
		EventTypes: approvalWebhookTransitionEventTypes,
		AfterSeq:   state.ProjectedSeq,
		Limit:      1,
	})
	if err != nil || len(next) == 0 {
		return false, err
	}
	listed, err := approvals.ListEvents(ctx, r.ExecutionEventStore, task.Namespace, task.Name)
	if err != nil {
		return false, err
	}
	for _, transition := range approvalWebhookTransitions(taskApprovals(listed, string(task.UID))) {
		if transition.seq > state.ProjectedSeq && selected.allows(transition.transition) {
			return true, nil
		}
	}
	return false, nil
}

func (r *TaskReconciler) approvalEventReadFailureHolds(tail *corev1alpha1.Task) bool {
	settled := tail.CreationTimestamp.Time
	if tail.Status.CompletionTime != nil {
		settled = tail.Status.CompletionTime.Time
	}
	return r.WebhookNotifier.now().Before(settled.Add(approvalWebhookSigningWindow))
}

func (r *TaskReconciler) taskIsLinkedContinuation(ctx context.Context, task *corev1alpha1.Task) bool {
	if task == nil || strings.TrimSpace(task.Annotations[labels.AnnotationContinuationOfUID]) == "" {
		return false
	}
	_, linked, err := taskwait.Predecessor(ctx, uncachedReader(r.APIReader, r.Client), task)
	return err != nil || linked
}

func withoutParkedApprovals(pending []approvals.Approval) []approvals.Approval {
	kept := pending[:0]
	for _, approval := range pending {
		if approval.Binding == nil && approval.Action == taskwait.ParkedApprovalAction {
			continue
		}
		kept = append(kept, approval)
	}
	return kept
}

func (r *TaskReconciler) handleAutonomousApprovalState(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, bool, error) {
	if !r.isAutonomousTask(ctx, task) {
		return ctrl.Result{}, false, nil
	}
	if result, parked, err := r.parkOnPendingApproval(ctx, task); err != nil || parked {
		return result, true, err
	}
	resumingAfterApproval, err := r.resumingAfterApprovalDecision(ctx, task)
	if err != nil {
		return ctrl.Result{}, true, err
	}
	if resumingAfterApproval {
		result, err := r.handleAutonomousIteration(ctx, task)
		return result, true, err
	}
	return ctrl.Result{}, false, nil
}

func parseAnnotationInt64(value string) int64 {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || parsed < 0 {
		return 0
	}
	return parsed
}

func (r *TaskReconciler) resumingAfterApprovalDecision(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	if task == nil {
		return false, nil
	}
	waitingStatus := strings.HasPrefix(task.Status.Message, "waiting for approval ")
	decisionNudge := task.Annotations != nil && task.Annotations[labels.AnnotationApprovalDecidedAt] != ""
	if decisionNudge && task.Annotations != nil {
		decisionSeq := parseAnnotationInt64(task.Annotations[labels.AnnotationApprovalDecisionSeq])
		resumedSeq := parseAnnotationInt64(task.Annotations[labels.AnnotationApprovalResumedSeq])
		decisionNudge = decisionSeq == 0 || decisionSeq > resumedSeq
	}
	if !waitingStatus && !decisionNudge {
		return false, nil
	}
	if r == nil || r.ExecutionEventStore == nil {
		return false, nil
	}
	listed, err := approvals.ListEvents(ctx, r.ExecutionEventStore, task.Namespace, task.Name)
	if err != nil {
		return false, err
	}
	resolved := approvals.Resolved(approvals.Derive(
		approvals.FilterEventsForTaskUID(listed, string(task.UID)),
		time.Time{},
	))
	return len(resolved) > 0, nil
}

func (r *TaskReconciler) clearApprovalDecisionNudge(ctx context.Context, task *corev1alpha1.Task) error {
	if r == nil || task == nil || task.Annotations == nil || task.Annotations[labels.AnnotationApprovalDecidedAt] == "" {
		return nil
	}
	var updated *corev1alpha1.Task
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		current := &corev1alpha1.Task{}
		if err := r.Get(ctx, types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, current); err != nil {
			return err
		}
		if seq := strings.TrimSpace(current.Annotations[labels.AnnotationApprovalDecisionSeq]); seq != "" {
			current.Annotations[labels.AnnotationApprovalResumedSeq] = seq
		}
		delete(current.Annotations, labels.AnnotationApprovalDecidedAt)
		delete(current.Annotations, labels.AnnotationApprovalDecisionID)
		delete(current.Annotations, labels.AnnotationApprovalDecisionStatus)
		delete(current.Annotations, labels.AnnotationApprovalDecisionSeq)
		if err := r.Update(ctx, current); err != nil {
			return err
		}
		updated = current
		return nil
	}); err != nil {
		return err
	}
	if updated != nil {
		task.ResourceVersion = updated.ResourceVersion
		task.Annotations = updated.Annotations
	}
	return nil
}

func (r *TaskReconciler) handleAutonomousIteration(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	log.Info("handling autonomous iteration", "iteration", task.Status.Iteration)

	if err := r.collectResult(ctx, task); err != nil {
		log.Error(err, "failed to collect iteration result")
		if store.IsTemporaryStorageError(err) {
			return ctrl.Result{}, err
		}
	}

	if result, parked, err := r.parkOnPendingApproval(ctx, task); err != nil || parked {
		return result, err
	}

	resumingAfterApproval, err := r.resumingAfterApprovalDecision(ctx, task)
	if err != nil {
		return ctrl.Result{}, err
	}

	if r.PlanStore != nil {
		plan, err := r.PlanStore.GetPlan(ctx, task.Namespace, task.Name)
		if err == nil && plan.GoalComplete && !resumingAfterApproval {
			log.Info("autonomous task goal complete", "iteration", task.Status.Iteration, "summary", plan.Summary)
			return r.completeExecutedTask(ctx, task, corev1alpha1.TaskPhaseSucceeded,
				fmt.Sprintf("goal complete after %d iterations: %s", task.Status.Iteration+1, plan.Summary))
		}
	}

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
	if maxIter > 0 && task.Status.Iteration+1 >= maxIter && !resumingAfterApproval {
		log.Info("autonomous task reached max iterations", "maxIterations", maxIter)
		return r.completeExecutedTask(ctx, task, corev1alpha1.TaskPhaseSucceeded,
			fmt.Sprintf("reached max iterations (%d)", maxIter))
	}

	if task.Spec.Suspend != nil && *task.Spec.Suspend {
		log.Info("autonomous task suspended, waiting for resume")
		task.Status.Message = fmt.Sprintf("autonomous task suspended at iteration %d", task.Status.Iteration)
		if err := r.Status().Update(ctx, task); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if err := r.enforceHistoryLimits(ctx, task); err != nil {
		log.Error(err, "failed to enforce history limits for autonomous task")
	}

	if err := revokeTaskJobAuthority(ctx, r.ResultStore, store.TaskJobIdentity{
		Namespace: task.Namespace, TaskUID: string(task.UID), JobUID: task.Status.JobUID,
	}); err != nil {
		return ctrl.Result{}, err
	}
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

	task.Status.Iteration++
	task.Status.Phase = corev1alpha1.TaskPhasePending
	task.Status.JobName = ""
	task.Status.JobUID = ""
	task.Status.Message = fmt.Sprintf("autonomous iteration %d", task.Status.Iteration)

	if err := r.Status().Update(ctx, task); err != nil {
		log.Error(err, "failed to update status for autonomous iteration")
		return ctrl.Result{}, err
	}
	if resumingAfterApproval {
		if err := r.clearApprovalDecisionNudge(ctx, task); err != nil {
			return ctrl.Result{}, err
		}
	}

	log.Info("autonomous task advancing to next iteration", "nextIteration", task.Status.Iteration)
	if r.Recorder != nil {
		r.Recorder.Event(task, corev1.EventTypeNormal, "AutonomousIteration",
			fmt.Sprintf("Starting iteration %d", task.Status.Iteration))
	}

	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func taskAgentRevisionChanged(ctx context.Context, reader client.Reader, task *corev1alpha1.Task, agent *corev1alpha1.Agent) (bool, error) {
	key := client.ObjectKey{Namespace: task.Spec.AgentRef.Namespace, Name: task.Spec.AgentRef.Name}
	if key.Namespace == "" {
		key.Namespace = task.Namespace
	}
	latest := &corev1alpha1.Agent{}
	if err := reader.Get(ctx, key, latest); err != nil {
		return false, fmt.Errorf("recheck Task Agent revision: %w", err)
	}
	return latest.UID != agent.UID || latest.Generation != agent.Generation || !latest.DeletionTimestamp.IsZero(), nil
}

func (r *TaskReconciler) parkedWaitActive(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	if task == nil {
		return false, nil
	}
	latest := &corev1alpha1.Task{}
	if err := uncachedReader(r.APIReader, r.Client).Get(ctx, client.ObjectKeyFromObject(task), latest); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return latest.UID == task.UID && taskwait.Active(latest.Status.Wait), nil
}
