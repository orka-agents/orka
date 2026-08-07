package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controlleroptions "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

const (
	defaultAgentExecutionClassificationInterval       = 2 * time.Second
	defaultAgentExecutionClassificationStabilityDelay = 5 * time.Second
	defaultAgentExecutionClassificationGateInterval   = time.Second
	agentExecutionClassificationUnclassified          = "unclassified"
)

// AgentExecutionClassificationReconciler owns the bounded, source-aware
// legacy adoption sweep. It is deliberately separate from ordinary Task
// reconciliation: before the inventory is Sealed, ordinary execution is
// closed and only this controller may append migration dispositions.
type AgentExecutionClassificationReconciler struct {
	client.Client
	APIReader client.Reader

	WatchNamespace  string
	Snapshots       store.AgentExecutionSnapshotStore
	HarnessAttempts store.HarnessV1AttemptStore
	RuntimeSessions harness.RuntimeSessionStore
	TaskBinder      *TaskReconciler
	SessionLineages store.SessionLineageStore

	Interval       time.Duration
	StabilityDelay time.Duration
	Now            func() time.Time
}

type agentExecutionClassificationEvidenceItem struct {
	Kind            string `json:"kind"`
	Namespace       string `json:"namespace,omitempty"`
	Name            string `json:"name,omitempty"`
	UID             string `json:"uid,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
	Digest          string `json:"digest"`
}

type agentExecutionClassificationEvidence struct {
	V1                []agentExecutionClassificationEvidenceItem
	V2                []agentExecutionClassificationEvidenceItem
	V1RuntimeIdentity string
	V2RuntimeIdentity string
}

type agentExecutionClassificationInventoryObject struct {
	Kind            string `json:"kind"`
	Namespace       string `json:"namespace,omitempty"`
	Name            string `json:"name"`
	UID             string `json:"uid,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
	Classification  string `json:"classification,omitempty"`
	Digest          string `json:"digest,omitempty"`
}

type agentExecutionClassificationSweep struct {
	Objects  []agentExecutionClassificationInventoryObject
	Complete bool
	Mutated  bool
}

type legacyCleanupSnapshotBody struct {
	SchemaVersion      int32                                      `json:"schemaVersion"`
	MigrationInventory string                                     `json:"migrationInventory"`
	TaskNamespace      string                                     `json:"taskNamespace"`
	TaskName           string                                     `json:"taskName"`
	TaskUID            string                                     `json:"taskUID"`
	ContractVersion    corev1alpha1.AgentRuntimeContractVersion   `json:"contractVersion"`
	V1Evidence         []agentExecutionClassificationEvidenceItem `json:"v1Evidence,omitempty"`
	V2Evidence         []agentExecutionClassificationEvidenceItem `json:"v2Evidence,omitempty"`
}

// Reconcile opens a new control-generation inventory, appends immutable
// dispositions, and seals only after two complete uncached passes remain
// byte-for-byte stable for the configured delay.
func (r *AgentExecutionClassificationReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	if request.Namespace != corev1alpha1.AgentExecutionControlNamespace ||
		request.Name != corev1alpha1.AgentExecutionControlName {
		return ctrl.Result{}, nil
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if r.Client == nil || reader == nil {
		return ctrl.Result{}, errors.New("classification controller requires cached and uncached Kubernetes clients")
	}
	control := &corev1alpha1.AgentExecutionControl{}
	if err := reader.Get(ctx, request.NamespacedName, control); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if control.UID == "" || control.Generation < 1 {
		return ctrl.Result{}, errors.New("AgentExecutionControl UID and generation are required for classification")
	}
	if sealedAgentExecutionClassification(control) {
		return ctrl.Result{RequeueAfter: r.interval()}, nil
	}

	inventoryID := agentExecutionClassificationInventoryID(control.UID, control.Generation)
	classification := control.Status.Classification
	if classification == nil || classification.ControlUID != control.UID ||
		classification.ControlGeneration != control.Generation ||
		classification.State != corev1alpha1.AgentExecutionClassificationOpen {
		openingDigest, err := acpDomainDigest("agent-execution-classification-opening/v1", map[string]any{
			"controlUID": control.UID, "controlGeneration": control.Generation, "inventoryID": inventoryID,
		})
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := r.patchClassificationStatus(ctx, control, &corev1alpha1.AgentExecutionClassificationStatus{
			State:      corev1alpha1.AgentExecutionClassificationOpen,
			ControlUID: control.UID, ControlGeneration: control.Generation,
			InventoryID: inventoryID, InventoryDigest: openingDigest,
			ObservedAt: metav1.NewTime(r.now()),
		}); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: r.interval()}, nil
	}

	sweep, err := r.sweep(ctx, reader, inventoryID)
	if err != nil {
		return ctrl.Result{}, err
	}
	if sweep.Mutated {
		return ctrl.Result{RequeueAfter: r.interval()}, nil
	}
	sort.Slice(sweep.Objects, func(i, j int) bool {
		left, right := sweep.Objects[i], sweep.Objects[j]
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.Namespace != right.Namespace {
			return left.Namespace < right.Namespace
		}
		if left.Name != right.Name {
			return left.Name < right.Name
		}
		return left.UID < right.UID
	})
	digest, err := acpDomainDigest("agent-execution-classification-inventory/v1", struct {
		ControlUID        types.UID                                     `json:"controlUID"`
		ControlGeneration int64                                         `json:"controlGeneration"`
		InventoryID       string                                        `json:"inventoryID"`
		Objects           []agentExecutionClassificationInventoryObject `json:"objects"`
	}{control.UID, control.Generation, inventoryID, sweep.Objects})
	if err != nil {
		return ctrl.Result{}, err
	}

	now := r.now()
	if classification.InventoryDigest != digest || classification.InventoryID != inventoryID {
		next := &corev1alpha1.AgentExecutionClassificationStatus{
			State:      corev1alpha1.AgentExecutionClassificationOpen,
			ControlUID: control.UID, ControlGeneration: control.Generation,
			InventoryID: inventoryID, InventoryDigest: digest, ObservedAt: metav1.NewTime(now),
		}
		if err := r.patchClassificationStatus(ctx, control, next); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: r.interval()}, nil
	}
	if !sweep.Complete || now.Before(classification.ObservedAt.Add(r.stabilityDelay())) {
		return ctrl.Result{RequeueAfter: r.interval()}, nil
	}
	next := *classification
	next.State = corev1alpha1.AgentExecutionClassificationSealed
	next.ObservedAt = metav1.NewTime(now)
	if err := r.patchClassificationStatus(ctx, control, &next); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: r.interval()}, nil
}

//nolint:gocyclo // One ordered sweep keeps every migration classification and fail-closed branch auditable.
func (r *AgentExecutionClassificationReconciler) sweep(
	ctx context.Context,
	reader client.Reader,
	inventoryID string,
) (agentExecutionClassificationSweep, error) {
	result := agentExecutionClassificationSweep{Complete: true}
	options := []client.ListOption{}
	if namespace := strings.TrimSpace(r.WatchNamespace); namespace != "" {
		options = append(options, client.InNamespace(namespace))
	}

	agents := &corev1alpha1.AgentList{}
	if err := reader.List(ctx, agents, options...); err != nil {
		return result, fmt.Errorf("list Agents for sealed classification: %w", err)
	}
	for i := range agents.Items {
		agent := &agents.Items[i]
		classification := string(agent.BuiltInContractVersion())
		if agent.Spec.Runtime != nil && agent.Spec.Runtime.RuntimeRef != nil {
			classification = "runtimeRef:" + strings.TrimSpace(agent.Spec.Runtime.RuntimeRef.Name)
		} else if agent.Spec.Runtime != nil && agent.Spec.Runtime.Type != "" && classification == "" {
			result.Complete = false
			classification = agentExecutionClassificationUnclassified
		}
		result.Objects = append(result.Objects, classificationInventoryObject("Agent", agent, classification, ""))
	}

	runtimes := &corev1alpha1.AgentRuntimeList{}
	if err := reader.List(ctx, runtimes, options...); err != nil {
		return result, fmt.Errorf("list AgentRuntimes for sealed classification: %w", err)
	}
	for i := range runtimes.Items {
		runtime := &runtimes.Items[i]
		classification := string(runtime.RegisteredContractVersion())
		if classification == "" {
			classification = agentExecutionClassificationUnclassified
			result.Complete = false
		}
		result.Objects = append(result.Objects, classificationInventoryObject("AgentRuntime", runtime, classification, ""))
	}

	evidence, err := r.collectKubernetesEvidence(ctx, reader, options)
	if err != nil {
		return result, err
	}
	tasks := &corev1alpha1.TaskList{}
	if err := reader.List(ctx, tasks, options...); err != nil {
		return result, fmt.Errorf("list Tasks for sealed classification: %w", err)
	}
	for i := range tasks.Items {
		task := &tasks.Items[i]
		if !agentTaskNeedsMigrationClassification(task) {
			continue
		}
		taskEvidence := evidence[string(task.UID)]
		if err := r.collectStoreEvidence(ctx, task, &taskEvidence); err != nil {
			return result, err
		}
		evidence[string(task.UID)] = taskEvidence
		count := agentTaskClassificationCount(task)
		if count > 1 {
			result.Complete = false
			result.Objects = append(result.Objects, classificationInventoryObject(taskResourceKind, task, "conflicting", ""))
			continue
		}
		if count == 0 {
			mutated, classifyErr := r.classifyTask(ctx, reader, task, inventoryID, taskEvidence)
			if classifyErr != nil {
				return result, classifyErr
			}
			if mutated {
				result.Mutated = true
				continue
			}
			result.Complete = false
			result.Objects = append(result.Objects, classificationInventoryObject(
				taskResourceKind, task, agentExecutionClassificationUnclassified, evidenceSetDigest(taskEvidence),
			))
			continue
		}
		if reason := agentTaskClassificationEvidenceConflict(task, taskEvidence); reason != "" {
			result.Complete = false
			result.Objects = append(result.Objects, classificationInventoryObject(
				taskResourceKind, task, "classification-conflict:"+reason, evidenceSetDigest(taskEvidence),
			))
			continue
		}
		classification, dispositionDigest := agentTaskClassificationSummary(task)
		result.Objects = append(result.Objects, classificationInventoryObject(taskResourceKind, task, classification, dispositionDigest))
	}
	if result.Mutated {
		return result, nil
	}

	sessionsComplete, sessionsMutated, sessionObjects, err := r.classifySessions(ctx, reader, tasks.Items, inventoryID, options)
	if err != nil {
		return result, err
	}
	result.Complete = result.Complete && sessionsComplete
	result.Mutated = sessionsMutated
	result.Objects = append(result.Objects, sessionObjects...)
	return result, nil
}

func (r *AgentExecutionClassificationReconciler) classifyTask(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
	inventoryID string,
	evidence agentExecutionClassificationEvidence,
) (bool, error) {
	v1, v2 := len(evidence.V1) != 0, len(evidence.V2) != 0
	switch {
	case v1 && v2:
		return r.persistTaskQuarantine(ctx, task, corev1alpha1.AgentExecutionQuarantineMixedEvidence,
			inventoryID, evidenceItemsDigest(evidence.V1), evidenceItemsDigest(evidence.V2))
	case v1:
		return r.persistLegacyCleanupBinding(ctx, reader, task, inventoryID,
			corev1alpha1.AgentRuntimeContractHarnessV1, evidence)
	case v2:
		return r.persistLegacyCleanupBinding(ctx, reader, task, inventoryID,
			corev1alpha1.AgentRuntimeContractHarnessV2, evidence)
	case !task.DeletionTimestamp.IsZero() || task.Status.Phase == corev1alpha1.TaskPhaseFinalizing ||
		isTerminalAgentTaskPhase(task.Status.Phase):
		return r.persistTaskNoExecution(ctx, task, inventoryID, evidenceSetDigest(evidence))
	case task.Status.Phase == corev1alpha1.TaskPhaseRunning:
		return r.persistTaskQuarantine(ctx, task, corev1alpha1.AgentExecutionQuarantineAmbiguousLegacyEvidence,
			inventoryID, "", "")
	case task.Status.Phase == "" || task.Status.Phase == corev1alpha1.TaskPhasePending:
		return r.persistTaskLocalLegacyBinding(ctx, reader, task)
	default:
		return false, nil
	}
}

func (r *AgentExecutionClassificationReconciler) persistTaskLocalLegacyBinding(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
) (bool, error) {
	if r.TaskBinder == nil || task.Spec.AgentRef == nil || strings.TrimSpace(task.Spec.AgentRef.Name) == "" {
		return false, nil
	}
	agentNamespace := strings.TrimSpace(task.Spec.AgentRef.Namespace)
	if agentNamespace == "" {
		agentNamespace = task.Namespace
	}
	agent := &corev1alpha1.Agent{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: agentNamespace, Name: task.Spec.AgentRef.Name}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	contract, err := taskLocalAgentContract(ctx, reader, agent)
	if err != nil || contract == "" {
		return false, err
	}
	var candidate *agentExecutionCandidate
	switch contract {
	case corev1alpha1.AgentRuntimeContractHarnessV1:
		candidate, err = r.TaskBinder.resolveHarnessV1ExecutionCandidate(ctx, task, agent)
	case corev1alpha1.AgentRuntimeContractHarnessV2:
		candidate, err = r.TaskBinder.resolveAgentExecutionCandidate(ctx, task, agent)
	default:
		return false, nil
	}
	if err != nil {
		// An explicitly classified but currently unresolvable candidate remains
		// Open for operator repair; it is never silently converted to no-execution.
		return false, nil
	}
	candidate.binding.Provenance = corev1alpha1.AgentExecutionProvenanceLegacyAdopted
	candidate.binding.BindingDigest, err = canonicalAgentExecutionBindingDigest(candidate.binding)
	if err != nil {
		return false, err
	}
	if err := r.TaskBinder.persistAgentExecutionSnapshot(ctx, task, candidate); err != nil {
		return false, err
	}
	reservation, err := r.TaskBinder.createAgentExecutionBindingReservation(ctx, task, &candidate.binding)
	if err != nil {
		return false, err
	}
	binding, err := r.TaskBinder.persistAgentExecutionBinding(ctx, task, candidate)
	if err != nil {
		return false, err
	}
	if err := r.TaskBinder.settleAgentExecutionBindingReservation(
		ctx, reservation, store.AgentExecutionBindingReservationBound, "",
	); err != nil {
		return false, err
	}
	return binding != nil, nil
}

func taskLocalAgentContract(
	ctx context.Context,
	reader client.Reader,
	agent *corev1alpha1.Agent,
) (corev1alpha1.AgentRuntimeContractVersion, error) {
	if agent == nil || agent.Spec.Runtime == nil {
		return "", nil
	}
	if ref := agent.Spec.Runtime.RuntimeRef; ref != nil && strings.TrimSpace(ref.Name) != "" {
		runtime := &corev1alpha1.AgentRuntime{}
		if err := reader.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: strings.TrimSpace(ref.Name)}, runtime); err != nil {
			if apierrors.IsNotFound(err) {
				return "", nil
			}
			return "", err
		}
		return runtime.RegisteredContractVersion(), nil
	}
	return agent.BuiltInContractVersion(), nil
}

func (r *AgentExecutionClassificationReconciler) persistLegacyCleanupBinding(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
	inventoryID string,
	contract corev1alpha1.AgentRuntimeContractVersion,
	evidence agentExecutionClassificationEvidence,
) (bool, error) {
	if r.Snapshots == nil {
		return false, errors.New("encrypted snapshot store is required for legacy cleanup binding")
	}
	namespace := &corev1.Namespace{}
	if err := reader.Get(ctx, client.ObjectKey{Name: task.Namespace}, namespace); err != nil {
		return false, fmt.Errorf("read legacy Task namespace identity: %w", err)
	}
	body, err := json.Marshal(legacyCleanupSnapshotBody{
		SchemaVersion:      store.AgentExecutionSnapshotSchemaVersion,
		MigrationInventory: inventoryID, TaskNamespace: task.Namespace, TaskName: task.Name,
		TaskUID: string(task.UID), ContractVersion: contract,
		V1Evidence: evidence.V1, V2Evidence: evidence.V2,
	})
	if err != nil {
		return false, err
	}
	snapshotDigest := store.CanonicalAgentExecutionSnapshotDigest(body)
	if err := r.Snapshots.PersistAgentExecutionSnapshot(ctx, store.AgentExecutionSnapshot{
		TaskUID: string(task.UID), Digest: snapshotDigest,
		SchemaVersion: store.AgentExecutionSnapshotSchemaVersion, Body: body, CreatedAt: r.now(),
	}); err != nil {
		return false, err
	}
	binding := corev1alpha1.AgentExecutionBinding{
		SchemaVersion: 1, Mode: corev1alpha1.AgentExecutionBindingModeCleanupOnly,
		ContractVersion: contract, Provenance: corev1alpha1.AgentExecutionProvenanceLegacyCleanupOnly,
		MigrationInventoryID: inventoryID,
		Task: corev1alpha1.AgentExecutionBindingTaskRef{
			NamespaceUID: namespace.UID, UID: task.UID, BoundSpecGeneration: max(task.Generation, 1),
		},
		Snapshot: corev1alpha1.AgentExecutionSnapshotRef{
			ID: string(task.UID) + "/" + snapshotDigest, Digest: snapshotDigest,
			SchemaVersion: store.AgentExecutionSnapshotSchemaVersion,
		},
		BoundAt: metav1.NewTime(r.now()),
	}
	if contract == corev1alpha1.AgentRuntimeContractHarnessV1 {
		binding.Backend = corev1alpha1.AgentExecutionBackendHarnessWrapper
		if task.Status.HarnessRuntime != nil && strings.TrimSpace(task.Status.HarnessRuntime.RuntimeRefName) != "" {
			binding.Backend = corev1alpha1.AgentExecutionBackendExternalEndpoint
			binding.RuntimeRef = &corev1alpha1.AgentExecutionRuntimeRef{Name: task.Status.HarnessRuntime.RuntimeRefName}
		} else if identity := strings.TrimSpace(evidence.V1RuntimeIdentity); identity != "" {
			binding.RuntimeType = corev1alpha1.AgentRuntimeType(identity)
		}
	} else {
		binding.Backend = corev1alpha1.AgentExecutionBackendRuntimePool
		if identity := strings.TrimSpace(evidence.V2RuntimeIdentity); identity != "" {
			binding.RuntimeType = corev1alpha1.AgentRuntimeType(identity)
		}
	}
	binding.BindingDigest, err = canonicalAgentExecutionBindingDigest(binding)
	if err != nil {
		return false, err
	}
	return r.patchAbsentTaskClassification(ctx, task, func(current *corev1alpha1.Task) {
		current.Status.AgentExecutionBinding = binding.DeepCopy()
	})
}

func (r *AgentExecutionClassificationReconciler) persistTaskNoExecution(
	ctx context.Context,
	task *corev1alpha1.Task,
	inventoryID, evidenceDigest string,
) (bool, error) {
	if !canonicalSHA256Digest(evidenceDigest) {
		var err error
		evidenceDigest, err = acpDomainDigest("agent-execution-no-route-evidence/v1", map[string]any{
			"namespace": task.Namespace, "name": task.Name, "uid": task.UID,
			"resourceVersion": task.ResourceVersion, "phase": task.Status.Phase,
			"deleting": !task.DeletionTimestamp.IsZero(), "finalizers": task.Finalizers,
		})
		if err != nil {
			return false, err
		}
	}
	disposition := &corev1alpha1.AgentExecutionNoExecution{
		SchemaVersion: 1, State: corev1alpha1.AgentExecutionNoExecutionUnbound,
		MigrationInventoryID: inventoryID, EvidenceDigest: evidenceDigest,
		RecordedAt: metav1.NewTime(r.now()),
	}
	return r.patchAbsentTaskClassification(ctx, task, func(current *corev1alpha1.Task) {
		current.Status.AgentExecutionNoExecution = disposition.DeepCopy()
	})
}

func (r *AgentExecutionClassificationReconciler) persistTaskQuarantine(
	ctx context.Context,
	task *corev1alpha1.Task,
	reason corev1alpha1.AgentExecutionQuarantineReason,
	inventoryID, v1Digest, v2Digest string,
) (bool, error) {
	quarantine := &corev1alpha1.AgentExecutionQuarantine{
		SchemaVersion: 1, Reason: reason, MigrationInventoryID: inventoryID,
		V1EvidenceDigest: v1Digest, V2EvidenceDigest: v2Digest,
		RecordedAt: metav1.NewTime(r.now()),
	}
	return r.patchAbsentTaskClassification(ctx, task, func(current *corev1alpha1.Task) {
		current.Status.AgentExecutionQuarantine = quarantine.DeepCopy()
	})
}

func (r *AgentExecutionClassificationReconciler) patchAbsentTaskClassification(
	ctx context.Context,
	task *corev1alpha1.Task,
	mutate func(*corev1alpha1.Task),
) (bool, error) {
	current := &corev1alpha1.Task{}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	key := client.ObjectKey{Namespace: task.Namespace, Name: task.Name}
	if err := reader.Get(ctx, key, current); err != nil {
		return false, err
	}
	if current.UID != task.UID {
		return false, fmt.Errorf("task %s/%s UID changed during sealed classification", task.Namespace, task.Name)
	}
	if agentTaskClassificationCount(current) != 0 {
		return false, nil
	}
	base := current.DeepCopy()
	mutate(current)
	if err := r.Status().Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, err
	}
	return true, nil
}

func (r *AgentExecutionClassificationReconciler) collectStoreEvidence(
	ctx context.Context,
	task *corev1alpha1.Task,
	evidence *agentExecutionClassificationEvidence,
) error {
	if r.HarnessAttempts != nil {
		attempts, err := r.HarnessAttempts.ListHarnessV1AttemptsByTask(ctx, task.Namespace, string(task.UID))
		if err != nil {
			return fmt.Errorf("list v1 attempts for sealed Task %s/%s: %w", task.Namespace, task.Name, err)
		}
		for i := range attempts {
			attempt := attempts[i]
			evidence.V1 = append(evidence.V1, classificationEvidenceItem("HarnessV1Attempt", task.Namespace,
				(store.HarnessV1AttemptKey{Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: attempt.Attempt}).CanonicalID(),
				"", "", attempt))
		}
	}
	if r.RuntimeSessions != nil {
		sessions, _, err := r.RuntimeSessions.ListRuntimeSessions(ctx, harness.RuntimeSessionFilter{
			Namespace: task.Namespace, ActiveTask: task.Name, IncludeDeleted: true,
		})
		if err != nil {
			return fmt.Errorf("list legacy runtime Sessions for Task %s/%s: %w", task.Namespace, task.Name, err)
		}
		for i := range sessions {
			session := sessions[i]
			evidence.V1 = append(evidence.V1, classificationEvidenceItem(
				"LegacyRuntimeSession", task.Namespace, string(session.ID), "", "", session,
			))
		}
	}
	sortClassificationEvidence(evidence.V1)
	sortClassificationEvidence(evidence.V2)
	return nil
}

func appendAgentExecutionClassificationEvidence(
	byTask map[string]agentExecutionClassificationEvidence,
	taskUID string,
	v2 bool,
	item agentExecutionClassificationEvidenceItem,
	runtimeIdentity string,
) {
	taskUID = strings.TrimSpace(taskUID)
	if taskUID == "" {
		return
	}
	evidence := byTask[taskUID]
	if v2 {
		evidence.V2 = append(evidence.V2, item)
		if evidence.V2RuntimeIdentity == "" {
			evidence.V2RuntimeIdentity = runtimeIdentity
		} else if runtimeIdentity != "" && evidence.V2RuntimeIdentity != runtimeIdentity {
			evidence.V2RuntimeIdentity = ""
		}
	} else {
		evidence.V1 = append(evidence.V1, item)
		if evidence.V1RuntimeIdentity == "" {
			evidence.V1RuntimeIdentity = runtimeIdentity
		} else if runtimeIdentity != "" && evidence.V1RuntimeIdentity != runtimeIdentity {
			evidence.V1RuntimeIdentity = ""
		}
	}
	byTask[taskUID] = evidence
}

func (r *AgentExecutionClassificationReconciler) collectKubernetesEvidence(
	ctx context.Context,
	reader client.Reader,
	options []client.ListOption,
) (map[string]agentExecutionClassificationEvidence, error) {
	byTask := make(map[string]agentExecutionClassificationEvidence)

	prompts := &corev1alpha1.PromptAttemptList{}
	if err := reader.List(ctx, prompts, options...); err != nil {
		return nil, fmt.Errorf("list PromptAttempts for sealed classification: %w", err)
	}
	for i := range prompts.Items {
		item := &prompts.Items[i]
		appendAgentExecutionClassificationEvidence(
			byTask, item.Spec.TaskUID, true, classificationObjectEvidenceItem("PromptAttempt", item, item.Spec), "",
		)
	}
	publications := &corev1alpha1.PublicationList{}
	if err := reader.List(ctx, publications, options...); err != nil {
		return nil, fmt.Errorf("list Publications for sealed classification: %w", err)
	}
	for i := range publications.Items {
		item := &publications.Items[i]
		appendAgentExecutionClassificationEvidence(
			byTask, item.Spec.TaskUID, true, classificationObjectEvidenceItem("Publication", item, item.Spec), "",
		)
	}
	effects := &corev1alpha1.ExternalEffectList{}
	if err := reader.List(ctx, effects, options...); err != nil {
		return nil, fmt.Errorf("list ExternalEffects for sealed classification: %w", err)
	}
	for i := range effects.Items {
		item := &effects.Items[i]
		taskUID := item.Labels[corev1alpha1.ControlRecordTaskUIDLabel]
		if strings.TrimSpace(taskUID) == "" {
			taskUID = item.Spec.AggregateID
		}
		appendAgentExecutionClassificationEvidence(
			byTask, taskUID, true, classificationObjectEvidenceItem("ExternalEffect", item, item.Spec), "",
		)
	}
	claims := &corev1alpha1.BranchClaimList{}
	if err := reader.List(ctx, claims); err != nil {
		return nil, fmt.Errorf("list BranchClaims for sealed classification: %w", err)
	}
	for i := range claims.Items {
		item := &claims.Items[i]
		if item.Spec.OwnerKind == corev1alpha1.BranchClaimOwnerKind(taskResourceKind) {
			appendAgentExecutionClassificationEvidence(
				byTask, item.Spec.OwnerUID, true, classificationObjectEvidenceItem("BranchClaim", item, item.Spec), "",
			)
		}
	}
	pools := &corev1alpha1.RuntimePoolList{}
	if err := reader.List(ctx, pools); err != nil {
		return nil, fmt.Errorf("list RuntimePools for sealed classification: %w", err)
	}
	for i := range pools.Items {
		pool := &pools.Items[i]
		for j := range pool.Status.Capacity.Reservations {
			reservation := pool.Status.Capacity.Reservations[j]
			appendAgentExecutionClassificationEvidence(byTask, reservation.TaskUID, true,
				classificationEvidenceItem("RuntimePoolReservation", pool.Namespace, pool.Name,
					string(pool.UID), pool.ResourceVersion, reservation),
				pool.Spec.Runtime.Profile.ProviderKind)
		}
	}
	controls := &corev1alpha1.RuntimeSessionControlList{}
	if err := reader.List(ctx, controls, options...); err != nil {
		return nil, fmt.Errorf("list RuntimeSessionControls for sealed classification: %w", err)
	}
	for i := range controls.Items {
		control := &controls.Items[i]
		if control.Spec.Owner.Kind == taskResourceKind {
			appendAgentExecutionClassificationEvidence(byTask, control.Spec.Owner.UID, true,
				classificationObjectEvidenceItem("RuntimeSessionControl", control, control.Spec), "")
		}
		if control.Status.MutationLease != nil {
			appendAgentExecutionClassificationEvidence(byTask, control.Status.MutationLease.TaskUID, true,
				classificationObjectEvidenceItem("RuntimeSessionMutationLease", control, control.Status.MutationLease), "")
		}
	}

	// Task status is authoritative route evidence and is added last so its
	// exact resourceVersion participates in the evidence digest.
	tasks := &corev1alpha1.TaskList{}
	if err := reader.List(ctx, tasks, options...); err != nil {
		return nil, fmt.Errorf("list Task status evidence for sealed classification: %w", err)
	}
	for i := range tasks.Items {
		task := &tasks.Items[i]
		if task.Status.HarnessRuntime != nil {
			identity := ""
			if task.Status.HarnessRuntime.RuntimeRefName == "" {
				identity = strings.TrimSpace(task.Status.HarnessRuntime.RuntimeName)
			}
			appendAgentExecutionClassificationEvidence(byTask, string(task.UID), false,
				classificationObjectEvidenceItem("TaskHarnessRuntime", task, task.Status.HarnessRuntime), identity)
		}
		if task.Status.Execution != nil {
			appendAgentExecutionClassificationEvidence(byTask, string(task.UID), true,
				classificationObjectEvidenceItem("TaskExecution", task, task.Status.Execution), "")
		}
		if task.Status.Delivery != nil {
			appendAgentExecutionClassificationEvidence(byTask, string(task.UID), true,
				classificationObjectEvidenceItem("TaskDelivery", task, task.Status.Delivery), "")
		}
	}
	for taskUID, evidence := range byTask {
		sortClassificationEvidence(evidence.V1)
		sortClassificationEvidence(evidence.V2)
		byTask[taskUID] = evidence
	}
	return byTask, nil
}

type sessionClassificationClaim struct {
	Contract        corev1alpha1.AgentRuntimeContractVersion
	RuntimeIdentity string
	ConfigDigest    string
	Ambiguous       bool
}

type sessionLineagePatchOutcome uint8

const (
	sessionLineagePatchUnchanged sessionLineagePatchOutcome = iota
	sessionLineagePatchAdopted
	sessionLineagePatchBlocked
)

//nolint:gocyclo // Session migration classification intentionally keeps its fail-closed cases in one auditable path.
func (r *AgentExecutionClassificationReconciler) classifySessions(
	ctx context.Context,
	reader client.Reader,
	tasks []corev1alpha1.Task,
	inventoryID string,
	options []client.ListOption,
) (bool, bool, []agentExecutionClassificationInventoryObject, error) {
	claims := make(map[client.ObjectKey][]sessionClassificationClaim)
	for i := range tasks {
		task := &tasks[i]
		if !agentTaskNeedsMigrationClassification(task) || task.Spec.SessionRef == nil ||
			strings.TrimSpace(task.Spec.SessionRef.Name) == "" || task.Status.AgentExecutionNoExecution != nil {
			continue
		}
		key := client.ObjectKey{Namespace: task.Namespace, Name: strings.TrimSpace(task.Spec.SessionRef.Name)}
		if task.Status.AgentExecutionQuarantine != nil || task.Status.AgentExecutionBinding == nil {
			claims[key] = append(claims[key], sessionClassificationClaim{Ambiguous: true})
			continue
		}
		binding := task.Status.AgentExecutionBinding
		identity := string(binding.RuntimeType)
		if identity == "" && binding.RuntimeRef != nil && binding.RuntimeRef.UID != "" {
			identity = string(binding.RuntimeRef.UID)
		}
		configDigest, err := agentExecutionLineageConfigDigest(binding)
		if err != nil {
			return false, false, nil, err
		}
		claims[key] = append(claims[key], sessionClassificationClaim{
			Contract: binding.ContractVersion, RuntimeIdentity: identity,
			ConfigDigest: configDigest, Ambiguous: identity == "" || !canonicalSHA256Digest(configDigest),
		})
	}
	if len(claims) == 0 {
		return true, false, nil, nil
	}
	controls := &corev1alpha1.RuntimeSessionControlList{}
	if err := reader.List(ctx, controls, options...); err != nil {
		return false, false, nil, err
	}
	bySession := make(map[client.ObjectKey]*corev1alpha1.RuntimeSessionControl, len(controls.Items))
	for i := range controls.Items {
		control := &controls.Items[i]
		bySession[client.ObjectKey{Namespace: control.Namespace, Name: control.Spec.SessionName}] = control
	}
	complete := true
	objects := make([]agentExecutionClassificationInventoryObject, 0, len(claims))
	for key, values := range claims {
		control := bySession[key]
		if control == nil {
			complete = false
			objects = append(objects, agentExecutionClassificationInventoryObject{
				Kind: "Session", Namespace: key.Namespace, Name: key.Name, Classification: "missing-control",
			})
			continue
		}
		if control.Status.Lineage != nil {
			valid := sessionLineageMatchesClaims(control.Status.Lineage, values)
			if !valid {
				complete = false
			}
			objects = append(objects, classificationInventoryObject("Session", control,
				map[bool]string{true: "lineage", false: "lineage-conflict"}[valid], control.Status.Lineage.ConfigDigest))
			continue
		}
		if control.Status.Availability == corev1alpha1.RuntimeSessionControlAvailability("ReconciliationBlocked") &&
			strings.TrimSpace(control.Status.BlockedReason) != "" {
			objects = append(objects, classificationInventoryObject("Session", control, "blocked", ""))
			continue
		}
		claim, exact := exactSessionClassificationClaim(values)
		if !exact {
			mutated, err := r.blockSessionClassification(ctx, control, inventoryID)
			if err != nil {
				return false, false, nil, err
			}
			if mutated {
				return false, true, objects, nil
			}
			complete = false
			continue
		}
		namespace := &corev1.Namespace{}
		if err := reader.Get(ctx, client.ObjectKey{Name: control.Namespace}, namespace); err != nil {
			return false, false, nil, err
		}
		lineage := &corev1alpha1.RuntimeSessionLineageStatus{
			NamespaceUID: namespace.UID, SessionUID: control.Spec.SessionUID,
			ContractVersion: claim.Contract, Generation: 1, RuntimeIdentity: claim.RuntimeIdentity,
			ConfigDigest: claim.ConfigDigest, Provenance: corev1alpha1.RuntimeSessionLineageLegacyAdopted,
			EstablishedAt: metav1.NewTime(r.now()),
		}
		outcome, err := r.patchSessionLineage(ctx, control, lineage, inventoryID)
		if err != nil {
			return false, false, nil, err
		}
		if outcome != sessionLineagePatchUnchanged {
			if outcome == sessionLineagePatchAdopted && r.SessionLineages != nil {
				_, err = r.SessionLineages.ProjectSessionLineage(ctx, store.SessionLineage{
					Namespace: control.Namespace, SessionName: control.Spec.SessionName,
					NamespaceUID: string(namespace.UID), SessionUID: control.Spec.SessionUID,
					ContractVersion: string(claim.Contract), LineageGeneration: 1,
					RuntimeIdentity: claim.RuntimeIdentity, ConfigDigest: claim.ConfigDigest,
					Provenance: store.SessionLineageLegacyAdopted, Version: 1,
					CreatedAt: lineage.EstablishedAt.Time, UpdatedAt: lineage.EstablishedAt.Time,
				})
				if err != nil {
					return false, false, nil, err
				}
			}
			return false, true, objects, nil
		}
	}
	return complete, false, objects, nil
}

func (r *AgentExecutionClassificationReconciler) patchSessionLineage(
	ctx context.Context,
	control *corev1alpha1.RuntimeSessionControl,
	lineage *corev1alpha1.RuntimeSessionLineageStatus,
	inventoryID string,
) (sessionLineagePatchOutcome, error) {
	current := &corev1alpha1.RuntimeSessionControl{}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: control.Namespace, Name: control.Name}, current); err != nil {
		return sessionLineagePatchUnchanged, err
	}
	if current.UID != control.UID || current.Status.Lineage != nil {
		return sessionLineagePatchUnchanged, nil
	}
	base := current.DeepCopy()
	outcome := sessionLineagePatchAdopted
	if current.Status.MutationLease != nil {
		// Legacy mutation Leases do not carry the lineage digest required by the
		// v2 Lease protocol. Appending lineage to status alone would permanently
		// split the status and authoritative Lease, so require adjudication.
		current.Status.Availability = corev1alpha1.RuntimeSessionControlAvailability("ReconciliationBlocked")
		current.Status.BlockedReason = "sealed inventory " + inventoryID + " found ambiguous runtime lineage"
		outcome = sessionLineagePatchBlocked
	} else {
		current.Status.Lineage = lineage.DeepCopy()
	}
	if err := r.Status().Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return sessionLineagePatchUnchanged, err
	}
	return outcome, nil
}

func (r *AgentExecutionClassificationReconciler) blockSessionClassification(
	ctx context.Context,
	control *corev1alpha1.RuntimeSessionControl,
	inventoryID string,
) (bool, error) {
	current := &corev1alpha1.RuntimeSessionControl{}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: control.Namespace, Name: control.Name}, current); err != nil {
		return false, err
	}
	if current.Status.Lineage != nil ||
		(current.Status.Availability == corev1alpha1.RuntimeSessionControlAvailability("ReconciliationBlocked") &&
			strings.TrimSpace(current.Status.BlockedReason) != "") {
		return false, nil
	}
	base := current.DeepCopy()
	current.Status.Availability = corev1alpha1.RuntimeSessionControlAvailability("ReconciliationBlocked")
	current.Status.BlockedReason = "sealed inventory " + inventoryID + " found ambiguous runtime lineage"
	if err := r.Status().Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, err
	}
	return true, nil
}

func (r *AgentExecutionClassificationReconciler) patchClassificationStatus(
	ctx context.Context,
	control *corev1alpha1.AgentExecutionControl,
	classification *corev1alpha1.AgentExecutionClassificationStatus,
) error {
	current := &corev1alpha1.AgentExecutionControl{}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	key := client.ObjectKey{Namespace: control.Namespace, Name: control.Name}
	if err := reader.Get(ctx, key, current); err != nil {
		return err
	}
	if current.UID != control.UID || current.Generation != control.Generation {
		return fmt.Errorf("AgentExecutionControl changed during classification status CAS")
	}
	base := current.DeepCopy()
	copyClassification := *classification
	current.Status.Classification = &copyClassification
	return r.Status().Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func (r *AgentExecutionClassificationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	needLeaderElection := true
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.AgentExecutionControl{}).
		WithOptions(controlleroptions.Options{MaxConcurrentReconciles: 1, NeedLeaderElection: &needLeaderElection}).
		Named("agentexecutionclassification").
		Complete(r)
}

func (r *AgentExecutionClassificationReconciler) interval() time.Duration {
	if r.Interval > 0 {
		return r.Interval
	}
	return defaultAgentExecutionClassificationInterval
}

func (r *AgentExecutionClassificationReconciler) stabilityDelay() time.Duration {
	if r.StabilityDelay > 0 {
		return r.StabilityDelay
	}
	return defaultAgentExecutionClassificationStabilityDelay
}

func (r *AgentExecutionClassificationReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func agentTaskNeedsMigrationClassification(task *corev1alpha1.Task) bool {
	if task == nil || task.Spec.Type != corev1alpha1.TaskTypeAgent || task.Spec.Schedule != "" {
		return false
	}
	if !isTerminalAgentTaskPhase(task.Status.Phase) {
		return true
	}
	return !task.DeletionTimestamp.IsZero() || slices.Contains(task.Finalizers, labels.TaskFinalizer) ||
		task.Status.HarnessRuntime != nil || task.Status.Execution != nil || task.Status.Delivery != nil ||
		agentTaskClassificationCount(task) != 0
}

func isTerminalAgentTaskPhase(phase corev1alpha1.TaskPhase) bool {
	switch phase {
	case corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskPhaseCancelled:
		return true
	default:
		return false
	}
}

func agentTaskClassificationCount(task *corev1alpha1.Task) int {
	if task == nil {
		return 0
	}
	count := 0
	if task.Status.AgentExecutionBinding != nil {
		count++
	}
	if task.Status.AgentExecutionNoExecution != nil {
		count++
	}
	if task.Status.AgentExecutionQuarantine != nil {
		count++
	}
	return count
}

func agentTaskClassificationSummary(task *corev1alpha1.Task) (string, string) {
	switch {
	case task.Status.AgentExecutionBinding != nil:
		return "binding:" + string(task.Status.AgentExecutionBinding.ContractVersion) + ":" +
			string(task.Status.AgentExecutionBinding.Mode), task.Status.AgentExecutionBinding.BindingDigest
	case task.Status.AgentExecutionNoExecution != nil:
		return "no-execution", task.Status.AgentExecutionNoExecution.EvidenceDigest
	case task.Status.AgentExecutionQuarantine != nil:
		digest, _ := canonicalAgentExecutionQuarantineDigest(task.Status.AgentExecutionQuarantine)
		return "quarantine:" + string(task.Status.AgentExecutionQuarantine.Reason), digest
	default:
		return agentExecutionClassificationUnclassified, ""
	}
}

func agentTaskClassificationEvidenceConflict(
	task *corev1alpha1.Task,
	evidence agentExecutionClassificationEvidence,
) string {
	if task == nil {
		return "missing-task"
	}
	if task.Status.AgentExecutionNoExecution != nil && (len(evidence.V1) != 0 || len(evidence.V2) != 0) {
		return "no-execution-has-route-evidence"
	}
	binding := task.Status.AgentExecutionBinding
	if binding == nil {
		return ""
	}
	switch binding.ContractVersion {
	case corev1alpha1.AgentRuntimeContractHarnessV1:
		if len(evidence.V2) != 0 {
			return "v1-binding-has-v2-evidence"
		}
	case corev1alpha1.AgentRuntimeContractHarnessV2:
		if len(evidence.V1) != 0 {
			return "v2-binding-has-v1-evidence"
		}
	default:
		return "unsupported-binding-contract"
	}
	return ""
}

func classificationInventoryObject(kind string, object client.Object, classification, digest string) agentExecutionClassificationInventoryObject {
	return agentExecutionClassificationInventoryObject{
		Kind: kind, Namespace: object.GetNamespace(), Name: object.GetName(), UID: string(object.GetUID()),
		ResourceVersion: object.GetResourceVersion(), Classification: classification, Digest: digest,
	}
}

func classificationObjectEvidenceItem(kind string, object client.Object, value any) agentExecutionClassificationEvidenceItem {
	return classificationEvidenceItem(kind, object.GetNamespace(), object.GetName(), string(object.GetUID()), object.GetResourceVersion(), value)
}

func classificationEvidenceItem(kind, namespace, name, uid, resourceVersion string, value any) agentExecutionClassificationEvidenceItem {
	digest, err := acpDomainDigest("agent-execution-classification-evidence/v1", value)
	if err != nil {
		panic(err)
	}
	return agentExecutionClassificationEvidenceItem{
		Kind: kind, Namespace: namespace, Name: name, UID: uid, ResourceVersion: resourceVersion, Digest: digest,
	}
}

func sortClassificationEvidence(items []agentExecutionClassificationEvidenceItem) {
	sort.Slice(items, func(i, j int) bool {
		if items[i].Kind != items[j].Kind {
			return items[i].Kind < items[j].Kind
		}
		if items[i].Namespace != items[j].Namespace {
			return items[i].Namespace < items[j].Namespace
		}
		if items[i].Name != items[j].Name {
			return items[i].Name < items[j].Name
		}
		return items[i].Digest < items[j].Digest
	})
}

func evidenceItemsDigest(items []agentExecutionClassificationEvidenceItem) string {
	digest, err := acpDomainDigest("agent-execution-classification-evidence-set/v1", items)
	if err != nil {
		panic(err)
	}
	return digest
}

func evidenceSetDigest(evidence agentExecutionClassificationEvidence) string {
	digest, err := acpDomainDigest("agent-execution-classification-route-evidence/v1", struct {
		V1 []agentExecutionClassificationEvidenceItem `json:"v1"`
		V2 []agentExecutionClassificationEvidenceItem `json:"v2"`
	}{evidence.V1, evidence.V2})
	if err != nil {
		panic(err)
	}
	return digest
}

func exactSessionClassificationClaim(values []sessionClassificationClaim) (sessionClassificationClaim, bool) {
	if len(values) == 0 || values[0].Ambiguous {
		return sessionClassificationClaim{}, false
	}
	want := values[0]
	for i := 1; i < len(values); i++ {
		if values[i].Ambiguous || values[i].Contract != want.Contract ||
			values[i].RuntimeIdentity != want.RuntimeIdentity || values[i].ConfigDigest != want.ConfigDigest {
			return sessionClassificationClaim{}, false
		}
	}
	return want, true
}

func sessionLineageMatchesClaims(lineage *corev1alpha1.RuntimeSessionLineageStatus, values []sessionClassificationClaim) bool {
	claim, ok := exactSessionClassificationClaim(values)
	return ok && lineage != nil && lineage.ContractVersion == claim.Contract &&
		lineage.RuntimeIdentity == claim.RuntimeIdentity && lineage.ConfigDigest == claim.ConfigDigest
}

func agentExecutionLineageConfigDigest(binding *corev1alpha1.AgentExecutionBinding) (string, error) {
	if binding == nil {
		return "", errors.New("execution binding is required for Session lineage")
	}
	if canonicalSHA256Digest(binding.RuntimeProfileDigest) {
		return binding.RuntimeProfileDigest, nil
	}
	// V1 has no managed RuntimeProfile. Commit to the immutable runtime, Agent,
	// policy, and backend identities while deliberately excluding Task prompt
	// and other turn-local snapshot fields so later turns can share one lineage.
	return acpDomainDigest("agent-execution-session-lineage-config/v1", struct {
		Contract   corev1alpha1.AgentRuntimeContractVersion `json:"contract"`
		Backend    corev1alpha1.AgentExecutionBackend       `json:"backend"`
		Runtime    corev1alpha1.AgentRuntimeType            `json:"runtime,omitempty"`
		RuntimeRef *corev1alpha1.AgentExecutionRuntimeRef   `json:"runtimeRef,omitempty"`
		Agent      *corev1alpha1.AgentExecutionAgentRef     `json:"agent,omitempty"`
		Policy     *corev1alpha1.AgentExecutionPolicyRef    `json:"policy,omitempty"`
	}{binding.ContractVersion, binding.Backend, binding.RuntimeType, binding.RuntimeRef, binding.Agent, binding.Policy})
}

func agentExecutionClassificationInventoryID(uid types.UID, generation int64) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s/%d", uid, generation))
	return "coexistence-" + hex.EncodeToString(sum[:12])
}

func sealedAgentExecutionClassification(control *corev1alpha1.AgentExecutionControl) bool {
	classification := control.Status.Classification
	return classification != nil && classification.State == corev1alpha1.AgentExecutionClassificationSealed &&
		classification.ControlUID == control.UID && classification.ControlGeneration == control.Generation &&
		strings.TrimSpace(classification.InventoryID) != "" && canonicalSHA256Digest(classification.InventoryDigest) &&
		!classification.ObservedAt.IsZero()
}

// AgentExecutionClassificationGatedRunnable starts and continuously monitors a
// mutating runnable under the persisted classification gate. If a control edit
// or recreation invalidates the seal, the child context is cancelled before
// any later scan and the runnable remains stopped until a new exact seal exists.
type AgentExecutionClassificationGatedRunnable struct {
	Gate     *AgentExecutionClassificationGate
	Runnable manager.Runnable
	Interval time.Duration
}

func (r *AgentExecutionClassificationGatedRunnable) NeedLeaderElection() bool { return true }

func (r *AgentExecutionClassificationGatedRunnable) Start(ctx context.Context) error {
	if r == nil || r.Gate == nil || r.Runnable == nil {
		return errors.New("classification-gated runnable requires gate and child runnable")
	}
	interval := r.Interval
	if interval <= 0 {
		interval = defaultAgentExecutionClassificationGateInterval
	}
	for {
		for {
			if err := r.Gate.Check(ctx); err == nil {
				break
			}
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(interval):
			}
		}

		restart, err := r.runWhileClassificationSealed(ctx, interval)
		if err != nil {
			return err
		}
		if !restart {
			return nil
		}
	}
}

func (r *AgentExecutionClassificationGatedRunnable) runWhileClassificationSealed(
	ctx context.Context,
	interval time.Duration,
) (bool, error) {
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Runnable.Start(childCtx) }()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			cancel()
			<-done
			return false, nil
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				return false, err
			}
			if ctx.Err() == nil {
				return false, errors.New("classification-gated runnable stopped while its parent context remained active")
			}
			return false, nil
		case <-ticker.C:
			if err := r.Gate.Check(ctx); err != nil {
				cancel()
				<-done
				return true, nil
			}
		}
	}
}

var _ manager.LeaderElectionRunnable = (*AgentExecutionClassificationGatedRunnable)(nil)
