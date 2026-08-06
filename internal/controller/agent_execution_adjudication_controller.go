/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controlleroptions "sigs.k8s.io/controller-runtime/pkg/controller"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	storekube "github.com/orka-agents/orka/internal/store/kube"
)

const (
	agentExecutionAdjudicationOperationDomain = "agent-execution-adjudication-operation/v1"
	agentExecutionQuarantineDigestDomain      = "agent-execution-quarantine/v1"
	agentExecutionBlockedStateDigestDomain    = "agent-execution-blocked-state/v1"
	agentExecutionEvidenceClosureDomain       = "agent-execution-evidence-closure/v1"

	agentExecutionAdjudicationSubjectTask    = "Task"
	agentExecutionAdjudicationSubjectSession = "RuntimeSessionControl"
)

// RBAC intentionally separates human creation of adjudications from this
// controller's narrow read/status-application authority. Generated aggregate
// roles may be split into dedicated ServiceAccounts by deployment tooling.
// +kubebuilder:rbac:groups=core.orka.ai,resources=agentexecutionadjudications,verbs=get;list;watch
// +kubebuilder:rbac:groups=core.orka.ai,resources=agentexecutionadjudications/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=tasks,verbs=get
// +kubebuilder:rbac:groups=core.orka.ai,resources=tasks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=runtimesessioncontrols,verbs=get
// +kubebuilder:rbac:groups=core.orka.ai,resources=runtimesessioncontrols/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// AgentExecutionAdjudicationReconciler applies the safe subset of immutable
// operator adjudications. It never clears evidence and never performs route
// execution. Application consists only of a CAS-fenced, append-once resolution
// reference that route-aware cleanup must verify before consuming.
type AgentExecutionAdjudicationReconciler struct {
	client.Client
	APIReader                        client.Reader
	AgentExecutionClassificationGate *AgentExecutionClassificationGate
	Recorder                         record.EventRecorder
	Now                              func() time.Time
}

type agentExecutionAdjudicationSubject struct {
	kind                     string
	namespaceUID             types.UID
	task                     *corev1alpha1.Task
	session                  *corev1alpha1.RuntimeSessionControl
	resourceVersion          string
	domainVersion            int64
	evidenceDigest           string
	evidenceClosureWatermark string
	resolutionRef            *corev1alpha1.AgentExecutionResolutionRef
}

type agentExecutionAdjudicationOperation struct {
	SchemaVersion            int32                                         `json:"schemaVersion"`
	AdjudicationNamespace    string                                        `json:"adjudicationNamespace"`
	AdjudicationName         string                                        `json:"adjudicationName"`
	AdjudicationUID          types.UID                                     `json:"adjudicationUID"`
	RequestedBy              string                                        `json:"requestedBy"`
	Action                   corev1alpha1.AgentExecutionAdjudicationAction `json:"action"`
	TaskRef                  corev1alpha1.AgentExecutionSubjectReference   `json:"taskRef"`
	SessionRef               *corev1alpha1.AgentExecutionSubjectReference  `json:"sessionRef,omitempty"`
	SubjectKind              string                                        `json:"subjectKind"`
	SubjectResourceVersion   string                                        `json:"subjectResourceVersion"`
	SubjectDomainVersion     int64                                         `json:"subjectDomainVersion"`
	EvidenceClosureWatermark string                                        `json:"evidenceClosureWatermark"`
	EvidenceDigest           string                                        `json:"evidenceDigest"`
	EvidenceDigests          []string                                      `json:"evidenceDigests"`
}

type agentExecutionTaskRouteEvidence struct {
	Binding          *corev1alpha1.AgentExecutionBinding        `json:"binding,omitempty"`
	NoExecution      *corev1alpha1.AgentExecutionNoExecution    `json:"noExecution,omitempty"`
	QuarantineDigest string                                     `json:"quarantineDigest,omitempty"`
	HarnessRuntime   *corev1alpha1.HarnessRuntimeStatus         `json:"harnessRuntime,omitempty"`
	Execution        *corev1alpha1.TaskExecutionStatus          `json:"execution,omitempty"`
	Delivery         *corev1alpha1.TaskDeliveryStatus           `json:"delivery,omitempty"`
	ExecutionOutcome *corev1alpha1.TaskWorkloadExecutionOutcome `json:"executionOutcome,omitempty"`
}

type agentExecutionRelatedSessionEvidence struct {
	Name               string    `json:"name"`
	UID                types.UID `json:"uid"`
	ResourceVersion    string    `json:"resourceVersion"`
	SessionUID         string    `json:"sessionUID"`
	DomainVersion      int64     `json:"domainVersion"`
	BlockedStateDigest string    `json:"blockedStateDigest,omitempty"`
}

type agentExecutionTaskEvidenceClosure struct {
	SchemaVersion   int32                                 `json:"schemaVersion"`
	NamespaceUID    types.UID                             `json:"namespaceUID"`
	TaskName        string                                `json:"taskName"`
	TaskUID         types.UID                             `json:"taskUID"`
	ResourceVersion string                                `json:"resourceVersion"`
	RouteEvidence   agentExecutionTaskRouteEvidence       `json:"routeEvidence"`
	Session         *agentExecutionRelatedSessionEvidence `json:"session,omitempty"`
}

type agentExecutionSessionEvidenceClosure struct {
	SchemaVersion          int32                           `json:"schemaVersion"`
	NamespaceUID           types.UID                       `json:"namespaceUID"`
	TaskName               string                          `json:"taskName"`
	TaskUID                types.UID                       `json:"taskUID"`
	TaskResourceVersion    string                          `json:"taskResourceVersion"`
	TaskRouteEvidence      agentExecutionTaskRouteEvidence `json:"taskRouteEvidence"`
	SessionName            string                          `json:"sessionName"`
	SessionUID             types.UID                       `json:"sessionUID"`
	SessionResourceVersion string                          `json:"sessionResourceVersion"`
	LogicalSessionUID      string                          `json:"logicalSessionUID"`
	DomainVersion          int64                           `json:"domainVersion"`
	BlockedStateDigest     string                          `json:"blockedStateDigest"`
}

type agentExecutionBlockedStateEvidence struct {
	SchemaVersion           int32                                           `json:"schemaVersion"`
	SessionUID              string                                          `json:"sessionUID"`
	Generation              int64                                           `json:"generation"`
	Lifecycle               corev1alpha1.RuntimeSessionControlLifecycle     `json:"lifecycle"`
	Availability            corev1alpha1.RuntimeSessionControlAvailability  `json:"availability"`
	MutationLeaseGeneration int64                                           `json:"mutationLeaseGeneration"`
	MutationLease           *corev1alpha1.RuntimeSessionMutationLeaseStatus `json:"mutationLease,omitempty"`
	BlockedReason           string                                          `json:"blockedReason"`
	RelatedPromptAttemptID  string                                          `json:"relatedPromptAttemptID,omitempty"`
	RelatedPublicationID    string                                          `json:"relatedPublicationID,omitempty"`
	VerifiedBaseline        *corev1alpha1.ControlVerifiedBranchBaseline     `json:"verifiedBaseline,omitempty"`
	Lineage                 *corev1alpha1.RuntimeSessionLineageStatus       `json:"lineage,omitempty"`
	ControllerEpochName     string                                          `json:"controllerEpochName,omitempty"`
	ControllerEpoch         int64                                           `json:"controllerEpoch,omitempty"`
	LastOperationID         string                                          `json:"lastOperationID,omitempty"`
	LastOperationDigest     string                                          `json:"lastOperationDigest,omitempty"`
	DomainVersion           int64                                           `json:"domainVersion"`
	CreatedAt               *metav1.Time                                    `json:"createdAt,omitempty"`
	UpdatedAt               *metav1.Time                                    `json:"updatedAt,omitempty"`
}

// Reconcile advances Pending -> Applying -> Applied. Rejected, Superseded,
// and Applied are terminal. Persisting Applying before the subject write makes
// a response-loss retry recoverable from the immutable subject-side reference.
func (r *AgentExecutionAdjudicationReconciler) Reconcile(
	ctx context.Context,
	req ctrl.Request,
) (ctrl.Result, error) {
	if r == nil || r.Client == nil {
		return ctrl.Result{}, errors.New("AgentExecutionAdjudication client is required")
	}
	if r.AgentExecutionClassificationGate != nil {
		if err := r.AgentExecutionClassificationGate.Check(ctx); err != nil {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}

	adjudication := &corev1alpha1.AgentExecutionAdjudication{}
	if err := reader.Get(ctx, req.NamespacedName, adjudication); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("read AgentExecutionAdjudication: %w", err)
	}
	if !adjudication.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	switch adjudication.Status.State {
	case corev1alpha1.AgentExecutionAdjudicationApplied,
		corev1alpha1.AgentExecutionAdjudicationRejected,
		corev1alpha1.AgentExecutionAdjudicationSuperseded:
		return ctrl.Result{}, nil
	case "", corev1alpha1.AgentExecutionAdjudicationPending:
		return r.prepare(ctx, reader, adjudication)
	case corev1alpha1.AgentExecutionAdjudicationApplying:
		return r.apply(ctx, reader, adjudication)
	default:
		return r.finish(ctx, adjudication, corev1alpha1.AgentExecutionAdjudicationRejected,
			fmt.Sprintf("unknown adjudication state %q", adjudication.Status.State), nil)
	}
}

func (r *AgentExecutionAdjudicationReconciler) prepare(
	ctx context.Context,
	reader client.Reader,
	adjudication *corev1alpha1.AgentExecutionAdjudication,
) (ctrl.Result, error) {
	if adjudication.Status.OperationID != "" || adjudication.Status.OperationDigest != "" ||
		adjudication.Status.ResolutionRefDigest != "" || adjudication.Status.ResultingSubjectResourceVersion != "" {
		return r.finish(ctx, adjudication, corev1alpha1.AgentExecutionAdjudicationRejected,
			"pending adjudication contains controller operation state", nil)
	}
	if reason := r.staticRejection(adjudication); reason != "" {
		return r.finish(ctx, adjudication, corev1alpha1.AgentExecutionAdjudicationRejected, reason, nil)
	}
	if r.expired(adjudication) {
		return r.finish(ctx, adjudication, corev1alpha1.AgentExecutionAdjudicationSuperseded,
			"adjudication expired before application", nil)
	}

	subject, terminalState, reason, err := r.inspectSubject(ctx, reader, adjudication)
	if err != nil {
		return ctrl.Result{}, err
	}
	if terminalState != "" {
		return r.finish(ctx, adjudication, terminalState, reason, subject)
	}
	if reason := validateAgentExecutionCleanupAction(adjudication, subject); reason != "" {
		return r.finish(ctx, adjudication, corev1alpha1.AgentExecutionAdjudicationRejected, reason, subject)
	}

	operationID := agentExecutionAdjudicationOperationID(adjudication)
	operationDigest, err := canonicalAgentExecutionAdjudicationOperationDigest(adjudication, subject)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("build adjudication operation digest: %w", err)
	}
	updated := adjudication.DeepCopy()
	updated.Status = corev1alpha1.AgentExecutionAdjudicationStatus{
		State:           corev1alpha1.AgentExecutionAdjudicationApplying,
		OperationID:     operationID,
		OperationDigest: operationDigest,
		ObservedAt:      ptrToMetav1Time(r.now()),
	}
	if err := r.Status().Update(ctx, updated); err != nil {
		return ctrl.Result{}, fmt.Errorf("mark AgentExecutionAdjudication Applying: %w", err)
	}
	r.eventf(adjudication, corev1.EventTypeNormal, "AdjudicationApplying",
		"Applying %s to %s %s/%s", adjudication.Spec.Action, subject.kind,
		adjudication.Namespace, subjectName(subject))
	return ctrl.Result{RequeueAfter: time.Millisecond}, nil
}

func (r *AgentExecutionAdjudicationReconciler) apply(
	ctx context.Context,
	reader client.Reader,
	adjudication *corev1alpha1.AgentExecutionAdjudication,
) (ctrl.Result, error) {
	if reason := r.staticRejection(adjudication); reason != "" {
		return r.finish(ctx, adjudication, corev1alpha1.AgentExecutionAdjudicationRejected, reason, nil)
	}
	if adjudication.Status.OperationID != agentExecutionAdjudicationOperationID(adjudication) {
		return r.finish(ctx, adjudication, corev1alpha1.AgentExecutionAdjudicationRejected,
			"applying adjudication has an invalid operation ID", nil)
	}
	if err := store.ValidateCanonicalDigest("adjudication operation digest", adjudication.Status.OperationDigest); err != nil {
		return r.finish(ctx, adjudication, corev1alpha1.AgentExecutionAdjudicationRejected,
			"applying adjudication has an invalid operation digest", nil)
	}

	subject, terminalState, reason, err := r.inspectSubject(ctx, reader, adjudication)
	if err != nil {
		return ctrl.Result{}, err
	}
	if subject != nil && subject.resolutionRef != nil {
		if resolutionRefMatchesAdjudication(subject.resolutionRef, adjudication) {
			if err := validateAgentExecutionResolutionRefDigest(adjudication.Namespace, subject.resolutionRef); err != nil {
				return r.finish(ctx, adjudication, corev1alpha1.AgentExecutionAdjudicationRejected,
					"subject contains a malformed adjudication resolution reference", subject)
			}
			return r.finishApplied(ctx, adjudication, subject, subject.resolutionRef)
		}
		return r.finish(ctx, adjudication, corev1alpha1.AgentExecutionAdjudicationSuperseded,
			"subject was resolved by a competing adjudication", subject)
	}
	if terminalState != "" {
		return r.finish(ctx, adjudication, terminalState, reason, subject)
	}
	if r.expired(adjudication) {
		return r.finish(ctx, adjudication, corev1alpha1.AgentExecutionAdjudicationSuperseded,
			"adjudication expired before the subject write", subject)
	}
	if reason := validateAgentExecutionCleanupAction(adjudication, subject); reason != "" {
		return r.finish(ctx, adjudication, corev1alpha1.AgentExecutionAdjudicationRejected, reason, subject)
	}
	expectedOperationDigest, err := canonicalAgentExecutionAdjudicationOperationDigest(adjudication, subject)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("rebuild adjudication operation digest: %w", err)
	}
	if expectedOperationDigest != adjudication.Status.OperationDigest {
		return r.finish(ctx, adjudication, corev1alpha1.AgentExecutionAdjudicationSuperseded,
			"subject evidence no longer matches the prepared operation", subject)
	}

	appliedAt := r.now()
	resolutionRef, err := newAgentExecutionResolutionRef(adjudication, appliedAt)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("build adjudication resolution reference: %w", err)
	}
	resultingResourceVersion, err := r.appendResolutionRef(ctx, subject, resolutionRef)
	if err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{RequeueAfter: time.Millisecond}, nil
		}
		return ctrl.Result{}, err
	}
	subject.resourceVersion = resultingResourceVersion
	subject.resolutionRef = resolutionRef
	return r.finishApplied(ctx, adjudication, subject, resolutionRef)
}

//nolint:gocyclo // Subject inspection keeps every immutable evidence fence in one auditable decision path.
func (r *AgentExecutionAdjudicationReconciler) inspectSubject(
	ctx context.Context,
	reader client.Reader,
	adjudication *corev1alpha1.AgentExecutionAdjudication,
) (*agentExecutionAdjudicationSubject, corev1alpha1.AgentExecutionAdjudicationState, string, error) {
	namespace := &corev1.Namespace{}
	if err := reader.Get(ctx, client.ObjectKey{Name: adjudication.Namespace}, namespace); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, corev1alpha1.AgentExecutionAdjudicationSuperseded,
				"adjudication namespace no longer exists", nil
		}
		return nil, "", "", fmt.Errorf("read adjudication namespace: %w", err)
	}
	if namespace.UID == "" {
		return nil, corev1alpha1.AgentExecutionAdjudicationRejected,
			"adjudication namespace has no immutable UID", nil
	}

	task := &corev1alpha1.Task{}
	taskKey := client.ObjectKey{Namespace: adjudication.Namespace, Name: adjudication.Spec.TaskRef.Name}
	if err := reader.Get(ctx, taskKey, task); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, corev1alpha1.AgentExecutionAdjudicationSuperseded,
				"referenced Task no longer exists", nil
		}
		return nil, "", "", fmt.Errorf("read adjudication Task: %w", err)
	}
	if task.UID == "" || task.UID != adjudication.Spec.TaskRef.UID {
		return nil, corev1alpha1.AgentExecutionAdjudicationSuperseded,
			"referenced Task UID no longer matches", nil
	}

	var session *corev1alpha1.RuntimeSessionControl
	if adjudication.Spec.SessionRef != nil {
		if task.Spec.SessionRef == nil || strings.TrimSpace(task.Spec.SessionRef.Name) != adjudication.Spec.SessionRef.Name {
			return nil, corev1alpha1.AgentExecutionAdjudicationRejected,
				"sessionRef does not match the Task's immutable Session reference", nil
		}
		session = &corev1alpha1.RuntimeSessionControl{}
		sessionKey := client.ObjectKey{
			Namespace: adjudication.Namespace,
			Name:      storekube.RuntimeSessionControlObjectName(adjudication.Spec.SessionRef.Name),
		}
		if err := reader.Get(ctx, sessionKey, session); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, corev1alpha1.AgentExecutionAdjudicationSuperseded,
					"referenced RuntimeSessionControl no longer exists", nil
			}
			return nil, "", "", fmt.Errorf("read adjudication RuntimeSessionControl: %w", err)
		}
		if session.UID == "" || session.UID != adjudication.Spec.SessionRef.UID ||
			session.Spec.SessionName != adjudication.Spec.SessionRef.Name {
			return nil, corev1alpha1.AgentExecutionAdjudicationSuperseded,
				"referenced RuntimeSessionControl UID or Session name no longer matches", nil
		}
	}

	if adjudication.Spec.QuarantineDigest != "" {
		if adjudication.Spec.BlockedStateDigest != "" {
			return nil, corev1alpha1.AgentExecutionAdjudicationRejected,
				"combined Task and Session evidence cannot be atomically fenced by this API version", nil
		}
		if adjudication.Spec.ExpectedState.SubjectDomainVersion != 0 {
			return nil, corev1alpha1.AgentExecutionAdjudicationRejected,
				"Task adjudication must not set a subject domain version", nil
		}
		if task.Status.AgentExecutionQuarantine == nil {
			return nil, corev1alpha1.AgentExecutionAdjudicationSuperseded,
				"Task no longer has quarantine evidence", nil
		}
		quarantineDigest, err := canonicalAgentExecutionQuarantineDigest(task.Status.AgentExecutionQuarantine)
		if err != nil {
			return nil, "", "", err
		}
		closure, err := canonicalAgentExecutionTaskEvidenceClosure(namespace.UID, task, session, quarantineDigest)
		if err != nil {
			return nil, "", "", err
		}
		subject := &agentExecutionAdjudicationSubject{
			kind: agentExecutionAdjudicationSubjectTask, namespaceUID: namespace.UID,
			task: task, session: session, resourceVersion: task.ResourceVersion,
			evidenceDigest: quarantineDigest, evidenceClosureWatermark: closure,
			resolutionRef: task.Status.AgentExecutionResolutionRef,
		}
		if task.ResourceVersion != adjudication.Spec.ExpectedState.SubjectResourceVersion ||
			quarantineDigest != adjudication.Spec.QuarantineDigest ||
			closure != adjudication.Spec.ExpectedState.EvidenceClosureWatermark {
			return subject, corev1alpha1.AgentExecutionAdjudicationSuperseded,
				"Task version or quarantine evidence no longer matches", nil
		}
		return subject, "", "", nil
	}

	if session == nil {
		return nil, corev1alpha1.AgentExecutionAdjudicationRejected,
			"blocked-state adjudication requires an exact sessionRef", nil
	}
	blockedStateDigest, err := canonicalAgentExecutionBlockedStateDigest(session)
	if err != nil {
		return nil, "", "", err
	}
	taskEvidence, err := buildAgentExecutionTaskRouteEvidence(task, "")
	if err != nil {
		return nil, "", "", err
	}
	closure, err := acpDomainDigest(agentExecutionEvidenceClosureDomain, agentExecutionSessionEvidenceClosure{
		SchemaVersion: 1, NamespaceUID: namespace.UID,
		TaskName: task.Name, TaskUID: task.UID, TaskResourceVersion: task.ResourceVersion,
		TaskRouteEvidence: taskEvidence,
		SessionName:       session.Spec.SessionName, SessionUID: session.UID,
		SessionResourceVersion: session.ResourceVersion, LogicalSessionUID: session.Spec.SessionUID,
		DomainVersion: session.Status.Version, BlockedStateDigest: blockedStateDigest,
	})
	if err != nil {
		return nil, "", "", fmt.Errorf("canonicalize Session evidence closure: %w", err)
	}
	subject := &agentExecutionAdjudicationSubject{
		kind: agentExecutionAdjudicationSubjectSession, namespaceUID: namespace.UID,
		task: task, session: session, resourceVersion: session.ResourceVersion,
		domainVersion: session.Status.Version, evidenceDigest: blockedStateDigest,
		evidenceClosureWatermark: closure,
		resolutionRef:            session.Status.AgentExecutionResolutionRef,
	}
	if session.Status.Availability != corev1alpha1.RuntimeSessionControlAvailability(store.SessionReconciliationBlocked) {
		return subject, corev1alpha1.AgentExecutionAdjudicationSuperseded,
			"Session recovered automatically or is no longer reconciliation-blocked", nil
	}
	if adjudication.Spec.ExpectedState.SubjectDomainVersion < 1 {
		return subject, corev1alpha1.AgentExecutionAdjudicationRejected,
			"blocked Session adjudication requires its exact positive domain version", nil
	}
	if session.ResourceVersion != adjudication.Spec.ExpectedState.SubjectResourceVersion ||
		session.Status.Version != adjudication.Spec.ExpectedState.SubjectDomainVersion ||
		blockedStateDigest != adjudication.Spec.BlockedStateDigest ||
		closure != adjudication.Spec.ExpectedState.EvidenceClosureWatermark {
		return subject, corev1alpha1.AgentExecutionAdjudicationSuperseded,
			"Session version or blocked-state evidence no longer matches", nil
	}
	return subject, "", "", nil
}

func (r *AgentExecutionAdjudicationReconciler) appendResolutionRef(
	ctx context.Context,
	subject *agentExecutionAdjudicationSubject,
	resolutionRef *corev1alpha1.AgentExecutionResolutionRef,
) (string, error) {
	switch subject.kind {
	case agentExecutionAdjudicationSubjectTask:
		updated := subject.task.DeepCopy()
		updated.Status.AgentExecutionResolutionRef = resolutionRef.DeepCopy()
		if err := r.Status().Update(ctx, updated); err != nil {
			return "", fmt.Errorf("append Task adjudication resolution reference: %w", err)
		}
		return updated.ResourceVersion, nil
	case agentExecutionAdjudicationSubjectSession:
		updated := subject.session.DeepCopy()
		updated.Status.AgentExecutionResolutionRef = resolutionRef.DeepCopy()
		if err := r.Status().Update(ctx, updated); err != nil {
			return "", fmt.Errorf("append RuntimeSessionControl adjudication resolution reference: %w", err)
		}
		return updated.ResourceVersion, nil
	default:
		return "", fmt.Errorf("unsupported adjudication subject kind %q", subject.kind)
	}
}

func (r *AgentExecutionAdjudicationReconciler) finishApplied(
	ctx context.Context,
	adjudication *corev1alpha1.AgentExecutionAdjudication,
	subject *agentExecutionAdjudicationSubject,
	resolutionRef *corev1alpha1.AgentExecutionResolutionRef,
) (ctrl.Result, error) {
	updated := adjudication.DeepCopy()
	updated.Status.State = corev1alpha1.AgentExecutionAdjudicationApplied
	updated.Status.ResultingSubjectResourceVersion = subject.resourceVersion
	updated.Status.ResolutionRefDigest = resolutionRef.ResolutionDigest
	updated.Status.ObservedAt = resolutionRef.AppliedAt.DeepCopy()
	updated.Status.Message = ""
	if err := r.Status().Update(ctx, updated); err != nil {
		return ctrl.Result{}, fmt.Errorf("mark AgentExecutionAdjudication Applied: %w", err)
	}
	r.eventf(adjudication, corev1.EventTypeNormal, "AdjudicationApplied",
		"Applied %s to %s %s/%s at resourceVersion %s", adjudication.Spec.Action,
		subject.kind, adjudication.Namespace, subjectName(subject), subject.resourceVersion)
	if subject.kind == agentExecutionAdjudicationSubjectTask {
		r.eventf(subject.task, corev1.EventTypeNormal, "ExecutionAdjudicationApplied",
			"Adjudication %s applied cleanup action %s", adjudication.Name, adjudication.Spec.Action)
	} else {
		r.eventf(subject.session, corev1.EventTypeNormal, "ExecutionAdjudicationApplied",
			"Adjudication %s applied cleanup action %s", adjudication.Name, adjudication.Spec.Action)
	}
	return ctrl.Result{}, nil
}

func (r *AgentExecutionAdjudicationReconciler) finish(
	ctx context.Context,
	adjudication *corev1alpha1.AgentExecutionAdjudication,
	state corev1alpha1.AgentExecutionAdjudicationState,
	message string,
	subject *agentExecutionAdjudicationSubject,
) (ctrl.Result, error) {
	updated := adjudication.DeepCopy()
	updated.Status.State = state
	updated.Status.Message = truncateAdjudicationMessage(message)
	updated.Status.ObservedAt = ptrToMetav1Time(r.now())
	if err := r.Status().Update(ctx, updated); err != nil {
		return ctrl.Result{}, fmt.Errorf("mark AgentExecutionAdjudication %s: %w", state, err)
	}
	eventType := corev1.EventTypeWarning
	reason := "Adjudication" + string(state)
	r.eventf(adjudication, eventType, reason, "%s", updated.Status.Message)
	if subject != nil {
		if subject.kind == agentExecutionAdjudicationSubjectTask {
			r.eventf(subject.task, eventType, reason, "Adjudication %s: %s", adjudication.Name, updated.Status.Message)
		} else if subject.session != nil {
			r.eventf(subject.session, eventType, reason, "Adjudication %s: %s", adjudication.Name, updated.Status.Message)
		}
	}
	return ctrl.Result{}, nil
}

func (r *AgentExecutionAdjudicationReconciler) staticRejection(
	adjudication *corev1alpha1.AgentExecutionAdjudication,
) string {
	if adjudication.UID == "" {
		return "adjudication has no immutable UID"
	}
	if strings.TrimSpace(adjudication.Spec.TaskRef.Name) == "" || adjudication.Spec.TaskRef.UID == "" {
		return "adjudication requires an exact Task name and UID"
	}
	if strings.TrimSpace(adjudication.Spec.RequestedBy) == "" || strings.TrimSpace(adjudication.Spec.Justification) == "" {
		return "adjudication requester and justification are required"
	}
	if adjudication.Spec.SessionRef != nil &&
		(strings.TrimSpace(adjudication.Spec.SessionRef.Name) == "" || adjudication.Spec.SessionRef.UID == "") {
		return "sessionRef requires an exact Session name and RuntimeSessionControl UID"
	}
	if adjudication.Spec.QuarantineDigest == "" && adjudication.Spec.BlockedStateDigest == "" {
		return "adjudication requires exactly one quarantine or blocked-state digest"
	}
	if adjudication.Spec.QuarantineDigest != "" && adjudication.Spec.BlockedStateDigest != "" {
		return "combined Task and Session evidence cannot be atomically fenced by this API version"
	}
	for _, candidate := range append([]string{
		adjudication.Spec.ExpectedState.EvidenceClosureWatermark,
		adjudication.Spec.QuarantineDigest,
		adjudication.Spec.BlockedStateDigest,
	}, adjudication.Spec.EvidenceDigests...) {
		if candidate == "" {
			continue
		}
		if err := store.ValidateCanonicalDigest("adjudication evidence digest", candidate); err != nil {
			return "adjudication contains a non-canonical evidence digest"
		}
	}
	if len(adjudication.Spec.EvidenceDigests) == 0 {
		return "adjudication requires independent evidence digests"
	}
	if adjudication.Spec.ExpiresAt != nil && !adjudication.CreationTimestamp.IsZero() &&
		!adjudication.Spec.ExpiresAt.After(adjudication.CreationTimestamp.Time) {
		return "adjudication expiry must be after creation"
	}
	switch adjudication.Spec.Action {
	case corev1alpha1.AgentExecutionAdjudicationCleanupV1,
		corev1alpha1.AgentExecutionAdjudicationCleanupV2,
		corev1alpha1.AgentExecutionAdjudicationCleanupBoth:
		return ""
	case corev1alpha1.AgentExecutionAdjudicationConfirmV1Outcome,
		corev1alpha1.AgentExecutionAdjudicationConfirmV2Outcome:
		return "confirming an outcome is unsupported until the API carries an exact terminal receipt and outcome"
	case corev1alpha1.AgentExecutionAdjudicationMarkNoExecution:
		return "MarkNoExecution is unsupported until sealed inventory proof can be fenced without replacing quarantine evidence"
	case corev1alpha1.AgentExecutionAdjudicationAbandonOutcomeUnknown:
		return "AbandonOutcomeUnknown is unsupported until exact effect identity and break-glass authorization are represented"
	case corev1alpha1.AgentExecutionAdjudicationBootstrapNewLineage:
		return "BootstrapNewLineage is unsupported until destination Session identity and canonical transcript digest are represented"
	default:
		return fmt.Sprintf("unsupported adjudication action %q", adjudication.Spec.Action)
	}
}

func validateAgentExecutionCleanupAction(
	adjudication *corev1alpha1.AgentExecutionAdjudication,
	subject *agentExecutionAdjudicationSubject,
) string {
	if subject == nil {
		return "adjudication subject is unavailable"
	}
	switch subject.kind {
	case agentExecutionAdjudicationSubjectTask:
		quarantine := subject.task.Status.AgentExecutionQuarantine
		if quarantine == nil {
			return "Task quarantine evidence is unavailable"
		}
		hasV1 := quarantine.V1EvidenceDigest != ""
		hasV2 := quarantine.V2EvidenceDigest != ""
		switch adjudication.Spec.Action {
		case corev1alpha1.AgentExecutionAdjudicationCleanupV1:
			if !hasV1 || hasV2 {
				return "CleanupV1 requires exclusively v1 quarantine evidence"
			}
		case corev1alpha1.AgentExecutionAdjudicationCleanupV2:
			if !hasV2 || hasV1 {
				return "CleanupV2 requires exclusively v2 quarantine evidence"
			}
		case corev1alpha1.AgentExecutionAdjudicationCleanupBoth:
			if !hasV1 || !hasV2 {
				return "CleanupBoth requires both v1 and v2 quarantine evidence"
			}
		}
	case agentExecutionAdjudicationSubjectSession:
		lineage := subject.session.Status.Lineage
		if lineage == nil {
			if adjudication.Spec.Action != corev1alpha1.AgentExecutionAdjudicationCleanupBoth {
				return "a blocked Session without proven lineage requires CleanupBoth"
			}
			return ""
		}
		switch lineage.ContractVersion {
		case corev1alpha1.AgentRuntimeContractHarnessV1:
			if adjudication.Spec.Action != corev1alpha1.AgentExecutionAdjudicationCleanupV1 {
				return "v1 Session lineage requires CleanupV1"
			}
		case corev1alpha1.AgentRuntimeContractHarnessV2:
			if adjudication.Spec.Action != corev1alpha1.AgentExecutionAdjudicationCleanupV2 {
				return "v2 Session lineage requires CleanupV2"
			}
		default:
			return "blocked Session has an unsupported contract version"
		}
	}
	return ""
}

func canonicalAgentExecutionQuarantineDigest(
	quarantine *corev1alpha1.AgentExecutionQuarantine,
) (string, error) {
	if quarantine == nil {
		return "", errors.New("AgentExecutionQuarantine is required")
	}
	digest, err := acpDomainDigest(agentExecutionQuarantineDigestDomain, quarantine)
	if err != nil {
		return "", fmt.Errorf("canonicalize Task quarantine evidence: %w", err)
	}
	return digest, nil
}

func canonicalAgentExecutionBlockedStateDigest(
	session *corev1alpha1.RuntimeSessionControl,
) (string, error) {
	if session == nil {
		return "", errors.New("RuntimeSessionControl is required")
	}
	status := session.Status
	digest, err := acpDomainDigest(agentExecutionBlockedStateDigestDomain, agentExecutionBlockedStateEvidence{
		SchemaVersion: 1, SessionUID: session.Spec.SessionUID,
		Generation: status.Generation, Lifecycle: status.Lifecycle, Availability: status.Availability,
		MutationLeaseGeneration: status.MutationLeaseGeneration, MutationLease: status.MutationLease,
		BlockedReason: status.BlockedReason, RelatedPromptAttemptID: status.RelatedPromptAttemptID,
		RelatedPublicationID: status.RelatedPublicationID, VerifiedBaseline: status.VerifiedBaseline,
		Lineage: status.Lineage, ControllerEpochName: status.ControllerEpochName,
		ControllerEpoch: status.ControllerEpoch, LastOperationID: status.LastOperationID,
		LastOperationDigest: status.LastOperationDigest, DomainVersion: status.Version,
		CreatedAt: status.CreatedAt, UpdatedAt: status.UpdatedAt,
	})
	if err != nil {
		return "", fmt.Errorf("canonicalize Session blocked-state evidence: %w", err)
	}
	return digest, nil
}

func canonicalAgentExecutionTaskEvidenceClosure(
	namespaceUID types.UID,
	task *corev1alpha1.Task,
	session *corev1alpha1.RuntimeSessionControl,
	quarantineDigest string,
) (string, error) {
	routeEvidence, err := buildAgentExecutionTaskRouteEvidence(task, quarantineDigest)
	if err != nil {
		return "", err
	}
	var related *agentExecutionRelatedSessionEvidence
	if session != nil {
		blockedDigest := ""
		if session.Status.Availability == corev1alpha1.RuntimeSessionControlAvailability(store.SessionReconciliationBlocked) {
			blockedDigest, err = canonicalAgentExecutionBlockedStateDigest(session)
			if err != nil {
				return "", err
			}
		}
		related = &agentExecutionRelatedSessionEvidence{
			Name: session.Spec.SessionName, UID: session.UID, ResourceVersion: session.ResourceVersion,
			SessionUID: session.Spec.SessionUID, DomainVersion: session.Status.Version,
			BlockedStateDigest: blockedDigest,
		}
	}
	digest, err := acpDomainDigest(agentExecutionEvidenceClosureDomain, agentExecutionTaskEvidenceClosure{
		SchemaVersion: 1, NamespaceUID: namespaceUID, TaskName: task.Name, TaskUID: task.UID,
		ResourceVersion: task.ResourceVersion, RouteEvidence: routeEvidence, Session: related,
	})
	if err != nil {
		return "", fmt.Errorf("canonicalize Task evidence closure: %w", err)
	}
	return digest, nil
}

func buildAgentExecutionTaskRouteEvidence(
	task *corev1alpha1.Task,
	quarantineDigest string,
) (agentExecutionTaskRouteEvidence, error) {
	if task == nil {
		return agentExecutionTaskRouteEvidence{}, errors.New("task is required")
	}
	if quarantineDigest == "" && task.Status.AgentExecutionQuarantine != nil {
		var err error
		quarantineDigest, err = canonicalAgentExecutionQuarantineDigest(task.Status.AgentExecutionQuarantine)
		if err != nil {
			return agentExecutionTaskRouteEvidence{}, err
		}
	}
	return agentExecutionTaskRouteEvidence{
		Binding: task.Status.AgentExecutionBinding, NoExecution: task.Status.AgentExecutionNoExecution,
		QuarantineDigest: quarantineDigest, HarnessRuntime: task.Status.HarnessRuntime,
		Execution: task.Status.Execution, Delivery: task.Status.Delivery,
		ExecutionOutcome: task.Status.ExecutionOutcome,
	}, nil
}

func canonicalAgentExecutionAdjudicationOperationDigest(
	adjudication *corev1alpha1.AgentExecutionAdjudication,
	subject *agentExecutionAdjudicationSubject,
) (string, error) {
	evidenceDigests := append([]string(nil), adjudication.Spec.EvidenceDigests...)
	slices.Sort(evidenceDigests)
	return acpDomainDigest(agentExecutionAdjudicationOperationDomain, agentExecutionAdjudicationOperation{
		SchemaVersion: 1, AdjudicationNamespace: adjudication.Namespace,
		AdjudicationName: adjudication.Name, AdjudicationUID: adjudication.UID,
		RequestedBy: adjudication.Spec.RequestedBy, Action: adjudication.Spec.Action,
		TaskRef: adjudication.Spec.TaskRef, SessionRef: adjudication.Spec.SessionRef,
		SubjectKind: subject.kind, SubjectResourceVersion: subject.resourceVersion,
		SubjectDomainVersion:     subject.domainVersion,
		EvidenceClosureWatermark: subject.evidenceClosureWatermark,
		EvidenceDigest:           subject.evidenceDigest, EvidenceDigests: evidenceDigests,
	})
}

func newAgentExecutionResolutionRef(
	adjudication *corev1alpha1.AgentExecutionAdjudication,
	appliedAt time.Time,
) (*corev1alpha1.AgentExecutionResolutionRef, error) {
	ref := &corev1alpha1.AgentExecutionResolutionRef{
		AdjudicationName: adjudication.Name, AdjudicationUID: adjudication.UID,
		Action: adjudication.Spec.Action, OperationDigest: adjudication.Status.OperationDigest,
		AppliedAt: metav1.NewTime(appliedAt.UTC()),
	}
	digest, err := canonicalAgentExecutionResolutionRefDigest(adjudication.Namespace, ref)
	if err != nil {
		return nil, err
	}
	ref.ResolutionDigest = digest
	return ref, nil
}

func canonicalAgentExecutionResolutionRefDigest(
	namespace string,
	ref *corev1alpha1.AgentExecutionResolutionRef,
) (string, error) {
	return store.CanonicalAgentExecutionResolutionRefDigest(namespace, ref)
}

func validateAgentExecutionResolutionRefDigest(
	namespace string,
	ref *corev1alpha1.AgentExecutionResolutionRef,
) error {
	expected, err := canonicalAgentExecutionResolutionRefDigest(namespace, ref)
	if err != nil {
		return err
	}
	if expected != ref.ResolutionDigest {
		return errors.New("AgentExecutionResolutionRef digest does not match canonical content")
	}
	return nil
}

func resolutionRefMatchesAdjudication(
	ref *corev1alpha1.AgentExecutionResolutionRef,
	adjudication *corev1alpha1.AgentExecutionAdjudication,
) bool {
	return ref != nil && ref.AdjudicationName == adjudication.Name &&
		ref.AdjudicationUID == adjudication.UID && ref.Action == adjudication.Spec.Action &&
		ref.OperationDigest == adjudication.Status.OperationDigest
}

func agentExecutionAdjudicationOperationID(adjudication *corev1alpha1.AgentExecutionAdjudication) string {
	return "agent-execution-adjudication-" + string(adjudication.UID)
}

func subjectName(subject *agentExecutionAdjudicationSubject) string {
	if subject == nil {
		return ""
	}
	if subject.kind == agentExecutionAdjudicationSubjectSession && subject.session != nil {
		return subject.session.Spec.SessionName
	}
	if subject.task != nil {
		return subject.task.Name
	}
	return ""
}

func truncateAdjudicationMessage(message string) string {
	message = strings.TrimSpace(message)
	if len(message) <= 1024 {
		return message
	}
	return message[:1024]
}

func ptrToMetav1Time(value time.Time) *metav1.Time {
	result := metav1.NewTime(value.UTC())
	return &result
}

func (r *AgentExecutionAdjudicationReconciler) expired(
	adjudication *corev1alpha1.AgentExecutionAdjudication,
) bool {
	return adjudication.Spec.ExpiresAt != nil && !r.now().Before(adjudication.Spec.ExpiresAt.UTC())
}

func (r *AgentExecutionAdjudicationReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *AgentExecutionAdjudicationReconciler) eventf(
	object client.Object,
	eventType, reason, message string,
	args ...any,
) {
	if r.Recorder != nil && object != nil {
		r.Recorder.Eventf(object, eventType, reason, message, args...)
	}
}

// SetupWithManager registers a leader-only, single-concurrency reconciler so
// competing adjudications serialize locally in addition to Kubernetes CAS.
func (r *AgentExecutionAdjudicationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorderFor("agent-execution-adjudication-controller") //nolint:staticcheck
	}
	needLeaderElection := true
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.AgentExecutionAdjudication{}).
		WithOptions(controlleroptions.Options{
			MaxConcurrentReconciles: 1,
			NeedLeaderElection:      &needLeaderElection,
		}).
		Named("agentexecutionadjudication").
		Complete(r)
}
