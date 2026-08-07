/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/store"
)

const (
	DefaultHarnessV1DispatchInterval = time.Second
	// The shipped harness v1 wrapper advertises and enforces one concurrent
	// turn. Keep the default dispatcher capacity aligned so wrapper capacity is
	// controller backpressure rather than a terminal Task rejection.
	DefaultHarnessV1DispatchWorkers = 1
	defaultHarnessV1TurnTimeout     = 30 * time.Minute

	harnessV1ReasonRejected           = "PromptNotAccepted"
	harnessV1ReasonFailed             = "PromptFailed"
	harnessV1ReasonRetryableFailure   = "RetryablePromptFailure"
	harnessV1ReasonCancelled          = "Cancelled"
	harnessV1ReasonOutcomeUnknown     = "RuntimeLost"
	harnessV1ReasonInvalidBinding     = "InvalidBinding"
	harnessV1ReasonBackendDisabled    = "BackendDisabled"
	harnessV1ReasonCredentialChanged  = "CredentialChanged"
	harnessV1ReasonProtocolViolation  = "ProtocolViolation"
	harnessV1ReasonOutputUnavailable  = "OutputUnavailable"
	harnessV1OutcomeUnknownMessage    = "harness v1 submission or terminal outcome cannot be proven"
	harnessV1AdmissionClosedOperation = "admission-closed"
	harnessV1AdmissionClosedMessage   = "harness v1 wrapper admission is temporarily closed"
)

var (
	errHarnessV1TerminalFrame              = errors.New("harness v1 terminal frame received")
	errHarnessV1StreamEndedWithoutTerminal = errors.New("harness v1 frame stream ended without terminal evidence")
)

type harnessV1ProtocolClient interface {
	StartTurn(context.Context, harness.StartTurnRequest) (*harness.StartTurnResponse, error)
	DurableTurnStatus(context.Context, harness.HarnessTurnID) (*harness.DurableTurnStatus, error)
	StreamFrames(context.Context, harness.HarnessTurnID, int64, func(harness.HarnessEventFrame) error) error
	FetchTurnOutput(context.Context, harness.HarnessTurnID, string) ([]byte, error)
	AcknowledgeTurnOutput(context.Context, harness.TurnOutputAcknowledgementRequest) error
	CancelTurn(context.Context, harness.CancelTurnRequest) (*harness.CancelTurnResponse, error)
}

type harnessV1ClientFactory func(endpoint, bearer string, httpClient *http.Client) (harnessV1ProtocolClient, error)

type harnessV1DispatchCandidate struct {
	task    *corev1alpha1.Task
	attempt store.HarnessV1Attempt
}

// HarnessV1Dispatcher owns harness v1 network effects outside Task reconcile.
// A Prepared attempt is moved durably to Submitting before StartTurn. On
// takeover, Submitting is always treated as potentially submitted: ledger and
// frame evidence are consulted before any state change and the request is never
// replayed merely because evidence is unavailable.
type HarnessV1Dispatcher struct {
	Client              client.Client
	APIReader           client.Reader
	Attempts            store.HarnessV1AttemptStore
	Snapshots           store.AgentExecutionSnapshotStore
	BindingReservations store.AgentExecutionBindingReservationStore
	ResultStore         store.ResultStore
	EventStore          store.ExecutionEventStore
	Sessions            *ACPSessionContinuity
	Epochs              *ControllerEpochManager
	Interval            time.Duration
	MaxConcurrent       int
	HTTPClient          *http.Client
	clientFactory       harnessV1ClientFactory

	mu     sync.Mutex
	active map[types.UID]struct{}
	sem    chan struct{}
}

func (d *HarnessV1Dispatcher) NeedLeaderElection() bool { return true }

func (d *HarnessV1Dispatcher) Start(ctx context.Context) error {
	if d.Client == nil || d.Attempts == nil || d.Snapshots == nil || d.ResultStore == nil ||
		d.EventStore == nil || d.Sessions == nil || d.Epochs == nil || d.BindingReservations == nil {
		return errors.New("harness v1 dispatcher requires Kubernetes client, attempt/snapshot/reservation stores, result/event stores, Session continuity, and epoch manager")
	}
	if d.APIReader == nil {
		d.APIReader = d.Client
	}
	if d.Interval <= 0 {
		d.Interval = DefaultHarnessV1DispatchInterval
	}
	if d.MaxConcurrent <= 0 {
		d.MaxConcurrent = DefaultHarnessV1DispatchWorkers
	}
	if d.clientFactory == nil {
		d.clientFactory = func(endpoint, bearer string, httpClient *http.Client) (harnessV1ProtocolClient, error) {
			options := []harness.ClientOption{harness.WithBearerToken(bearer)}
			if httpClient != nil {
				options = append(options, harness.WithHTTPClient(httpClient))
			}
			return harness.NewClient(endpoint, options...)
		}
	}
	d.mu.Lock()
	if d.active == nil {
		d.active = make(map[types.UID]struct{})
	}
	if d.sem == nil {
		d.sem = make(chan struct{}, d.MaxConcurrent)
	}
	d.mu.Unlock()
	if _, err := d.Epochs.CurrentFence(ctx); err != nil {
		return err
	}

	ticker := time.NewTicker(d.Interval)
	defer ticker.Stop()
	for {
		if err := d.dispatchOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logf.FromContext(ctx).Error(err, "harness v1 dispatcher scan failed")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (d *HarnessV1Dispatcher) dispatchOnce(ctx context.Context) error {
	var tasks corev1alpha1.TaskList
	if err := d.Client.List(ctx, &tasks); err != nil {
		return err
	}
	candidates := make([]harnessV1DispatchCandidate, 0, len(tasks.Items))
	for i := range tasks.Items {
		task := &tasks.Items[i]
		if !taskManagedByHarnessV1(task) {
			continue
		}
		attempts, err := d.Attempts.ListHarnessV1AttemptsByTask(ctx, task.Namespace, string(task.UID))
		if err != nil {
			return fmt.Errorf("list harness v1 attempts for Task %s/%s: %w", task.Namespace, task.Name, err)
		}
		if len(attempts) == 0 {
			continue
		}
		latest := attempts[len(attempts)-1]
		if store.IsTerminalHarnessV1AttemptState(latest.State) && harnessV1AttemptProjectionMatches(task, &latest) {
			continue
		}
		candidates = append(candidates, harnessV1DispatchCandidate{task: task, attempt: latest})
	}
	sortHarnessV1DispatchCandidates(candidates, time.Now().UTC())
	for i := range candidates {
		candidate := candidates[i]
		task, latest := candidate.task, candidate.attempt
		select {
		case d.sem <- struct{}{}:
			if !d.markActive(task.UID) {
				<-d.sem
				continue
			}
			go func(task *corev1alpha1.Task, attempt store.HarnessV1Attempt) {
				defer func() {
					<-d.sem
					d.unmarkActive(task.UID)
				}()
				if err := d.reconcileAttempt(ctx, task.DeepCopy(), &attempt); err != nil && !errors.Is(err, context.Canceled) {
					logf.FromContext(ctx).Error(err, "harness v1 attempt reconciliation failed",
						"namespace", task.Namespace, "task", task.Name, "attempt", attempt.Attempt)
				}
			}(task.DeepCopy(), latest)
		default:
			return nil
		}
	}
	return nil
}

func sortHarnessV1DispatchCandidates(candidates []harnessV1DispatchCandidate, now time.Time) {
	ranks := make(map[*corev1alpha1.Task]acpTaskQueueRank, len(candidates))
	for i := range candidates {
		task := candidates[i].task
		ranks[task] = rankTaskForQueue(task, now.UTC(), harnessV1TaskQueuedAt(task))
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		leftRecovery := candidates[i].attempt.State != store.HarnessV1AttemptPrepared
		rightRecovery := candidates[j].attempt.State != store.HarnessV1AttemptPrepared
		if leftRecovery != rightRecovery {
			return leftRecovery
		}
		return taskQueueRankLess(ranks[candidates[i].task], ranks[candidates[j].task])
	})
}

func harnessV1TaskQueuedAt(task *corev1alpha1.Task) time.Time {
	if task == nil {
		return time.Unix(0, 0).UTC()
	}
	if task.Status.HarnessRuntime != nil && task.Status.HarnessRuntime.LastTransitionTime != nil &&
		!task.Status.HarnessRuntime.LastTransitionTime.IsZero() {
		return task.Status.HarnessRuntime.LastTransitionTime.UTC()
	}
	if !task.CreationTimestamp.IsZero() {
		return task.CreationTimestamp.UTC()
	}
	return time.Unix(0, 0).UTC()
}

func (d *HarnessV1Dispatcher) markActive(uid types.UID) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, exists := d.active[uid]; exists {
		return false
	}
	d.active[uid] = struct{}{}
	return true
}

func (d *HarnessV1Dispatcher) unmarkActive(uid types.UID) {
	d.mu.Lock()
	delete(d.active, uid)
	d.mu.Unlock()
}

//nolint:gocyclo // The attempt state machine is kept together so crash boundaries remain auditable.
func (d *HarnessV1Dispatcher) reconcileAttempt(ctx context.Context, task *corev1alpha1.Task, observed *store.HarnessV1Attempt) error {
	if task == nil || observed == nil || task.Status.AgentExecutionBinding == nil {
		return errors.New("harness v1 Task, binding, and attempt are required")
	}
	fence, err := d.Epochs.CurrentFence(ctx)
	if err != nil {
		return err
	}
	attempt, err := d.Attempts.GetHarnessV1Attempt(ctx, store.HarnessV1AttemptKey{
		Namespace: observed.Namespace, TaskUID: observed.TaskUID, Attempt: observed.Attempt,
	})
	if err != nil {
		return err
	}
	if err := d.validateAttemptIdentity(task, attempt); err != nil {
		return d.failUnsubmittedAttempt(ctx, task, attempt, fence, harnessV1ReasonInvalidBinding)
	}

	if store.IsTerminalHarnessV1AttemptState(attempt.State) {
		return d.reconcileTerminalAttemptAfterOutputAcknowledgement(ctx, task, attempt, fence)
	}

	verifier := TaskReconciler{
		Client: d.Client, APIReader: d.APIReader, AgentExecutionSnapshots: d.Snapshots,
		AgentExecutionBindingReservations: d.BindingReservations,
	}
	var (
		verified  *verifiedHarnessV1Execution
		verifyErr error
	)
	if attempt.State == store.HarnessV1AttemptPrepared {
		// Prepared is the one recovery state that may still cross StartTurn.
		// Verify its immutable reservation, snapshot, and Secret identities here,
		// then authorize the current enabled/closing/drain revision immediately
		// before submission below. The generic recovery verifier intentionally
		// permits disabled cleanup and is therefore not a submission authority.
		verified, verifyErr = verifier.loadHarnessV1ExecutionWithOptions(
			ctx, task, task.Status.AgentExecutionBinding, true, true, false, true,
		)
	} else {
		verified, verifyErr = verifier.loadVerifiedHarnessV1ExecutionForRecovery(
			ctx, task, task.Status.AgentExecutionBinding, true,
		)
	}
	if verifyErr != nil {
		if attempt.State == store.HarnessV1AttemptPrepared {
			return d.failUnsubmittedAttempt(ctx, task, attempt, fence, harnessV1ReasonCredentialChanged)
		}
		if attempt.State == store.HarnessV1AttemptSettling {
			return fmt.Errorf("verify settling harness v1 execution: %w", verifyErr)
		}
		return d.markOutcomeUnknown(ctx, task, attempt, fence, harnessV1ReasonCredentialChanged)
	}
	if attempt.State == store.HarnessV1AttemptSettling {
		protocolClient, request, err := d.protocolClientAndRequest(ctx, task, verified, attempt)
		if err != nil {
			return fmt.Errorf("build settling harness v1 recovery request: %w", err)
		}
		return d.recoverSettlingAttempt(ctx, task, protocolClient, request, attempt, fence)
	}

	if !task.DeletionTimestamp.IsZero() || task.Status.Phase == corev1alpha1.TaskPhaseCancelled {
		return d.cancelAttempt(ctx, task, verified, attempt, fence)
	}

	switch attempt.State {
	case store.HarnessV1AttemptPrepared:
		authorized, err := d.v1PreparedExecutionAuthorized(ctx, task, verified.binding)
		if err != nil {
			return err
		}
		if !authorized {
			return d.failUnsubmittedAttempt(ctx, task, attempt, fence, harnessV1ReasonBackendDisabled)
		}
		protocolClient, request, err := d.protocolClientAndRequest(ctx, task, verified, attempt)
		if err != nil {
			return d.failUnsubmittedAttempt(ctx, task, attempt, fence, harnessV1ReasonCredentialChanged)
		}
		// Establish or verify the protocol lineage, acquire the exact Kubernetes
		// Session lease, and persist the open SQLite SessionTurn before crossing
		// the StartTurn request-write boundary.
		if err := d.prepareHarnessV1TaskSession(ctx, task, verified, attempt, fence); err != nil {
			return err
		}
		attempt, err = d.transitionAttempt(ctx, attempt, fence, store.HarnessV1AttemptSubmitting, "begin-submit", store.HarnessV1AttemptUpdates{})
		if err != nil {
			return err
		}
		if err := d.projectAttemptState(ctx, task, attempt, "submitting harness v1 turn"); err != nil {
			return err
		}
		accepted, startErr := protocolClient.StartTurn(ctx, request)
		if startErr != nil {
			return d.handleStartTurnError(ctx, task, verified, protocolClient, request, attempt, fence, startErr)
		}
		runtimeSessionID := string(accepted.RuntimeSessionID)
		correlationID := accepted.CorrelationID
		if correlationID == "" {
			correlationID = attempt.CorrelationID
		}
		attempt, err = d.transitionAttempt(ctx, attempt, fence, store.HarnessV1AttemptAccepted, "accepted", store.HarnessV1AttemptUpdates{
			RuntimeSessionID: &runtimeSessionID, CorrelationID: &correlationID,
		})
		if err != nil {
			return err
		}
		return d.streamAcceptedAttempt(ctx, task, verified, protocolClient, request, attempt, fence)

	case store.HarnessV1AttemptSubmitting, store.HarnessV1AttemptSubmittedUnknown:
		protocolClient, request, err := d.protocolClientAndRequest(ctx, task, verified, attempt)
		if err != nil {
			return d.markOutcomeUnknown(ctx, task, attempt, fence, harnessV1ReasonCredentialChanged)
		}
		return d.recoverAmbiguousSubmission(ctx, task, verified, protocolClient, request, attempt, fence)

	case store.HarnessV1AttemptAccepted, store.HarnessV1AttemptRunning,
		store.HarnessV1AttemptCancelRequested:
		protocolClient, request, err := d.protocolClientAndRequest(ctx, task, verified, attempt)
		if err != nil {
			return d.markOutcomeUnknown(ctx, task, attempt, fence, harnessV1ReasonCredentialChanged)
		}
		return d.recoverActiveAttempt(ctx, task, verified, protocolClient, request, attempt, fence)
	default:
		return fmt.Errorf("unsupported harness v1 attempt state %q", attempt.State)
	}
}

func (d *HarnessV1Dispatcher) validateAttemptIdentity(task *corev1alpha1.Task, attempt *store.HarnessV1Attempt) error {
	binding := task.Status.AgentExecutionBinding
	if task.UID == "" || attempt.TaskUID != string(task.UID) || attempt.TaskName != task.Name ||
		binding == nil || binding.ContractVersion != corev1alpha1.AgentRuntimeContractHarnessV1 ||
		attempt.BindingDigest != binding.BindingDigest || attempt.SnapshotDigest != binding.Snapshot.Digest ||
		attempt.TurnID == "" || attempt.RequestDigest == "" {
		return errors.New("attempt identity does not match immutable harness v1 Task binding")
	}
	return nil
}

// v1PreparedExecutionAuthorized proves that a Prepared attempt belongs to a
// durably settled enabled-revision binding. Closing may finish reservations
// from its exact source revision, and drain-only may finish only reservations
// proven settled before the recorded cutoff. Disabled and stale enabled
// revisions never cross StartTurn.
func (d *HarnessV1Dispatcher) v1PreparedExecutionAuthorized(
	ctx context.Context,
	task *corev1alpha1.Task,
	binding *corev1alpha1.AgentExecutionBinding,
) (bool, error) {
	if task == nil || binding == nil || binding.BackendControl == nil ||
		binding.BackendControl.AdmittedMode != corev1alpha1.AgentExecutionEffectiveModeEnabled {
		return false, nil
	}
	reader := d.APIReader
	if reader == nil {
		reader = d.Client
	}
	if reader == nil || d.BindingReservations == nil {
		return false, errors.New("API reader and binding reservation store are required for Prepared dispatch")
	}
	verifier := TaskReconciler{AgentExecutionBindingReservations: d.BindingReservations}
	reservation, err := verifier.loadBoundAgentExecutionReservation(ctx, task, binding)
	if err != nil {
		return false, err
	}
	if reservation.SettledAt == nil {
		return false, nil
	}

	control := &corev1alpha1.AgentExecutionControl{}
	if err := reader.Get(ctx, types.NamespacedName{
		Namespace: corev1alpha1.AgentExecutionControlNamespace, Name: corev1alpha1.AgentExecutionControlName,
	}, control); err != nil {
		return false, err
	}
	ref := binding.BackendControl
	if control.Name != ref.Name || control.UID == "" || control.UID != ref.UID || control.Status.Backends == nil {
		return false, nil
	}
	observed := control.Status.Backends.V1
	switch observed.EffectiveMode {
	case corev1alpha1.AgentExecutionEffectiveModeEnabled:
		return control.Status.ObservedGeneration == control.Generation &&
			ref.Generation == control.Generation && ref.ModeRevision == observed.ModeRevision, nil
	case corev1alpha1.AgentExecutionEffectiveModeClosing:
		return control.Status.ObservedGeneration == ref.Generation &&
			ref.Generation <= control.Generation && ref.ModeRevision+1 == observed.ModeRevision, nil
	case corev1alpha1.AgentExecutionEffectiveModeDrainOnly:
		if control.Status.ObservedGeneration != control.Generation || observed.AdmissionClosedAt == nil ||
			observed.CutoffInventoryDigest == "" || ref.Generation > control.Generation ||
			ref.ModeRevision >= observed.ModeRevision {
			return false, nil
		}
		cutoff := observed.AdmissionClosedAt.Time
		return !reservation.ReservedAt.After(cutoff) && !reservation.SettledAt.After(cutoff), nil
	case corev1alpha1.AgentExecutionEffectiveModeDisabled:
		return false, nil
	default:
		return false, fmt.Errorf("unsupported harness v1 effective mode %q", observed.EffectiveMode)
	}
}

func (d *HarnessV1Dispatcher) protocolClientAndRequest(
	ctx context.Context,
	task *corev1alpha1.Task,
	verified *verifiedHarnessV1Execution,
	attempt *store.HarnessV1Attempt,
) (harnessV1ProtocolClient, harness.StartTurnRequest, error) {
	if verified == nil || verified.body.HarnessV1 == nil {
		return nil, harness.StartTurnRequest{}, errors.New("verified harness v1 snapshot is required")
	}
	target := verified.body.HarnessV1
	auth := &corev1.Secret{}
	if err := d.APIReader.Get(ctx, types.NamespacedName{Namespace: target.AuthSecretNamespace, Name: target.AuthSecretName}, auth); err != nil {
		return nil, harness.StartTurnRequest{}, err
	}
	if string(auth.UID) != target.AuthSecretUID || auth.ResourceVersion != target.AuthSecretResourceVersion {
		return nil, harness.StartTurnRequest{}, errors.New("frozen harness auth Secret identity changed")
	}
	bearer := string(auth.Data[target.AuthSecretKey])
	if strings.TrimSpace(bearer) == "" {
		return nil, harness.StartTurnRequest{}, errors.New("frozen harness auth Secret key is empty")
	}
	env, err := resolveFrozenHarnessV1Env(ctx, d.APIReader, target.CredentialRefs)
	if err != nil {
		return nil, harness.StartTurnRequest{}, err
	}
	request, err := buildHarnessV1StartTurnRequest(task, verified, attempt, env)
	if err != nil {
		return nil, harness.StartTurnRequest{}, err
	}
	protocolClient, err := d.clientFactory(target.Endpoint, bearer, d.HTTPClient)
	return protocolClient, request, err
}

func buildHarnessV1StartTurnRequest(
	task *corev1alpha1.Task,
	verified *verifiedHarnessV1Execution,
	attempt *store.HarnessV1Attempt,
	env []harness.TurnEnvVar,
) (harness.StartTurnRequest, error) {
	if task == nil || verified == nil || verified.binding == nil || verified.body.HarnessV1 == nil || attempt == nil {
		return harness.StartTurnRequest{}, errors.New("task, verified harness v1 execution, and attempt are required")
	}
	if verified.binding.BoundAt.IsZero() {
		return harness.StartTurnRequest{}, errors.New("harness v1 binding time is required")
	}
	timeout := defaultHarnessV1TurnTimeout
	if verified.body.Timeout != "" {
		parsed, err := time.ParseDuration(verified.body.Timeout)
		if err != nil || parsed <= 0 {
			return harness.StartTurnRequest{}, errors.New("frozen harness v1 timeout must be a positive duration")
		}
		timeout = parsed
	}
	request := harness.StartTurnRequest{
		Version: harness.ProtocolVersion, Namespace: task.Namespace, TaskName: task.Name,
		SessionName:       verified.body.HarnessV1.SessionName,
		RuntimeSessionID:  harness.RuntimeSessionID(attempt.RuntimeSessionID),
		TurnID:            harness.HarnessTurnID(attempt.TurnID),
		CorrelationID:     attempt.CorrelationID,
		Deadline:          verified.binding.BoundAt.Time.UTC().Add(timeout),
		AuthIdentity:      harness.AuthIdentity{Subject: "orka-task:" + attempt.TaskUID},
		Input:             harness.TurnInput{Prompt: verified.body.Prompt, Env: env},
		ToolExecutionMode: harness.ToolExecutionModeObserved,
		Metadata: map[string]string{
			harness.MetadataTaskUID:             attempt.TaskUID,
			harness.MetadataAttempt:             strconv.FormatInt(int64(attempt.Attempt), 10),
			harness.MetadataBindingDigest:       attempt.BindingDigest,
			harness.MetadataSnapshotDigest:      attempt.SnapshotDigest,
			"runtime":                           verified.body.HarnessV1.RuntimeName,
			"orka.runtimeName":                  verified.body.HarnessV1.RuntimeName,
			harness.MetadataRuntimePolicyFrozen: booleanTrueValue,
			"model":                             verified.body.Configuration.Model,
			"systemPrompt":                      verified.body.Configuration.SystemPrompt,
			"maxTurns":                          strconv.FormatInt(int64(verified.body.Configuration.MaxTurns), 10),
			"reasoningEffort":                   verified.body.Configuration.ReasoningEffort,
		},
	}
	if verified.body.Configuration.MaxTurns < 1 {
		return harness.StartTurnRequest{}, errors.New("frozen harness v1 max turns must be positive")
	}
	allowedTools, allowedToolsSet, disallowedTools, allowBash := frozenHarnessV1ToolPolicy(verified.body)
	if !allowedToolsSet || allowBash == nil {
		return harness.StartTurnRequest{}, errors.New("frozen harness v1 tool policy must explicitly set allowedTools and allowBash")
	}
	request.Metadata[harness.MetadataAllowedToolsSet] = booleanTrueValue
	request.Metadata["allowedTools"] = strings.Join(allowedTools, ",")
	request.Metadata["disallowedTools"] = strings.Join(disallowedTools, ",")
	request.Metadata["allowBash"] = strconv.FormatBool(*allowBash)
	if strings.EqualFold(strings.TrimSpace(verified.body.HarnessV1.RuntimeName), string(corev1alpha1.AgentRuntimeCodex)) &&
		len(allowedTools) == 0 && !*allowBash {
		request.Metadata["readOnly"] = booleanTrueValue
	}
	if attempt.RequestDigest != "" {
		request.Metadata[harness.MetadataRequestDigest] = attempt.RequestDigest
	}
	if verified.body.HarnessV1.RuntimeAuthOnly {
		request.Metadata["runtimeAuthOnly"] = booleanTrueValue
	}
	if err := request.Validate(); err != nil {
		return harness.StartTurnRequest{}, err
	}
	digest, err := harness.CanonicalStartTurnRequestDigest(request)
	if err != nil {
		return harness.StartTurnRequest{}, err
	}
	if attempt.RequestDigest != "" && digest != attempt.RequestDigest {
		return harness.StartTurnRequest{}, errors.New("reconstructed harness v1 request digest does not match the durable attempt")
	}
	return request, nil
}

func frozenHarnessV1ToolPolicy(
	body agentExecutionSnapshotBody,
) (allowedTools []string, allowedToolsSet bool, disallowedTools []string, allowBash *bool) {
	if body.DefaultTools != nil {
		if !body.DefaultTools.AllowedToolsOmitted {
			allowedTools = append([]string(nil), body.DefaultTools.AllowedTools...)
			allowedToolsSet = true
		}
		if body.DefaultTools.AllowBash != nil {
			value := *body.DefaultTools.AllowBash
			allowBash = &value
		}
	}
	if body.RuntimeOverride != nil {
		if body.RuntimeOverride.AllowedTools != nil {
			allowedTools = append([]string(nil), body.RuntimeOverride.AllowedTools...)
			allowedToolsSet = true
		}
		disallowedTools = append([]string(nil), body.RuntimeOverride.DisallowedTools...)
		if body.RuntimeOverride.AllowBash != nil {
			value := *body.RuntimeOverride.AllowBash
			allowBash = &value
		}
	}
	return allowedTools, allowedToolsSet, disallowedTools, allowBash
}

func resolveFrozenHarnessV1Env(
	ctx context.Context,
	reader client.Reader,
	refs []agentExecutionSnapshotSecretRef,
) ([]harness.TurnEnvVar, error) {
	if reader == nil {
		return nil, errors.New("API reader is required to resolve frozen harness v1 environment")
	}
	var env []harness.TurnEnvVar
	seen := make(map[string]struct{})
	for _, ref := range refs {
		secret := &corev1.Secret{}
		if err := reader.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, secret); err != nil {
			return nil, err
		}
		if string(secret.UID) != ref.UID || secret.ResourceVersion != ref.ResourceVersion {
			return nil, fmt.Errorf("frozen %s Secret identity changed", ref.Role)
		}
		keys := append([]string(nil), ref.Keys...)
		slices.Sort(keys)
		for _, key := range keys {
			if _, duplicate := seen[key]; duplicate {
				return nil, fmt.Errorf("duplicate frozen harness environment key %q", key)
			}
			value := secret.Data[key]
			if len(value) == 0 {
				return nil, fmt.Errorf("frozen %s Secret key %q is empty", ref.Role, key)
			}
			seen[key] = struct{}{}
			env = append(env, harness.TurnEnvVar{Name: key, Value: string(value)})
		}
	}
	return env, nil
}

func (d *HarnessV1Dispatcher) handleStartTurnError(
	ctx context.Context,
	task *corev1alpha1.Task,
	verified *verifiedHarnessV1Execution,
	protocolClient harnessV1ProtocolClient,
	request harness.StartTurnRequest,
	attempt *store.HarnessV1Attempt,
	fence store.ControllerEpochFence,
	startErr error,
) error {
	var clientErr harness.ClientError
	if errors.As(startErr, &clientErr) {
		switch {
		case clientErr.RemoteAccepted:
			accepted, err := d.transitionAttempt(ctx, attempt, fence, store.HarnessV1AttemptAccepted, "accepted-response-invalid", store.HarnessV1AttemptUpdates{})
			if err != nil {
				return err
			}
			return d.streamAcceptedAttempt(ctx, task, verified, protocolClient, request, accepted, fence)
		case clientErr.IsDuplicateTurn(), clientErr.RemoteAcceptanceUnknown:
			unknown, err := d.transitionAttempt(ctx, attempt, fence, store.HarnessV1AttemptSubmittedUnknown, "submission-unknown", store.HarnessV1AttemptUpdates{})
			if err != nil {
				return err
			}
			return d.recoverAmbiguousSubmission(ctx, task, verified, protocolClient, request, unknown, fence)
		case isHarnessV1AdmissionClosedError(clientErr):
			recoverable, err := d.transitionAttempt(
				ctx,
				attempt,
				fence,
				store.HarnessV1AttemptSubmittedUnknown,
				harnessV1AdmissionClosedOperation,
				store.HarnessV1AttemptUpdates{},
			)
			if err != nil {
				return err
			}
			return d.projectAttemptState(ctx, task, recoverable, harnessV1AdmissionClosedMessage)
		default:
			return d.failUnsubmittedAttempt(ctx, task, attempt, fence, harnessV1ReasonRejected)
		}
	}
	unknown, err := d.transitionAttempt(ctx, attempt, fence, store.HarnessV1AttemptSubmittedUnknown, "submission-unknown", store.HarnessV1AttemptUpdates{})
	if err != nil {
		return err
	}
	return d.recoverAmbiguousSubmission(ctx, task, verified, protocolClient, request, unknown, fence)
}

func isHarnessV1AdmissionClosedError(err harness.ClientError) bool {
	return err.StatusCode == http.StatusConflict && err.Message == "wrapper admission is closed"
}

func isHarnessV1DurableTurnNotFoundError(err error) bool {
	var clientErr harness.ClientError
	return errors.As(err, &clientErr) && clientErr.StatusCode == http.StatusNotFound && clientErr.Message == "turn not found"
}

func harnessV1AttemptHasAdmissionClosedProof(attempt *store.HarnessV1Attempt) bool {
	return attempt != nil && attempt.State == store.HarnessV1AttemptSubmittedUnknown &&
		strings.HasPrefix(attempt.LastOperationID, harnessV1AdmissionClosedOperation+"-")
}

func (d *HarnessV1Dispatcher) retryAdmissionClosedStartTurn(
	ctx context.Context,
	task *corev1alpha1.Task,
	verified *verifiedHarnessV1Execution,
	protocolClient harnessV1ProtocolClient,
	request harness.StartTurnRequest,
	attempt *store.HarnessV1Attempt,
	fence store.ControllerEpochFence,
) error {
	accepted, startErr := protocolClient.StartTurn(ctx, request)
	if startErr != nil {
		var clientErr harness.ClientError
		if !errors.As(startErr, &clientErr) {
			// The request-write outcome is ambiguous. Preserve the durable
			// admission-closed proof and require another exact ledger lookup
			// before any later resend.
			return startErr
		}
		switch {
		case isHarnessV1AdmissionClosedError(clientErr):
			return d.projectAttemptState(ctx, task, attempt, harnessV1AdmissionClosedMessage)
		case clientErr.RemoteAccepted:
			acceptedAttempt, err := d.transitionAttempt(
				ctx, attempt, fence, store.HarnessV1AttemptAccepted,
				"accepted-response-invalid-after-admission-reopen", store.HarnessV1AttemptUpdates{},
			)
			if err != nil {
				return err
			}
			return d.streamAcceptedAttempt(ctx, task, verified, protocolClient, request, acceptedAttempt, fence)
		case clientErr.IsDuplicateTurn(), clientErr.RemoteAcceptanceUnknown:
			// Do not recursively resend after an ambiguous response. A future
			// reconciliation must consult the durable ledger again.
			return startErr
		default:
			return d.failUnsubmittedAttempt(ctx, task, attempt, fence, harnessV1ReasonRejected)
		}
	}

	runtimeSessionID := string(accepted.RuntimeSessionID)
	correlationID := accepted.CorrelationID
	if correlationID == "" {
		correlationID = attempt.CorrelationID
	}
	acceptedAttempt, err := d.transitionAttempt(
		ctx,
		attempt,
		fence,
		store.HarnessV1AttemptAccepted,
		"accepted-after-admission-reopen",
		store.HarnessV1AttemptUpdates{
			RuntimeSessionID: &runtimeSessionID,
			CorrelationID:    &correlationID,
		},
	)
	if err != nil {
		return err
	}
	return d.streamAcceptedAttempt(ctx, task, verified, protocolClient, request, acceptedAttempt, fence)
}

type persistedHarnessV1Evidence struct {
	found     bool
	lastSeq   int64
	terminal  harness.FrameType
	completed *harness.TurnCompleted
	failed    *harness.TurnFailed
}

//nolint:gocyclo // Submission recovery keeps every durable evidence branch in one auditable state machine.
func (d *HarnessV1Dispatcher) recoverAmbiguousSubmission(
	ctx context.Context,
	task *corev1alpha1.Task,
	verified *verifiedHarnessV1Execution,
	protocolClient harnessV1ProtocolClient,
	request harness.StartTurnRequest,
	attempt *store.HarnessV1Attempt,
	fence store.ControllerEpochFence,
) error {
	var durableOutcomeUnknownReceiptDigest string
	status, statusErr := protocolClient.DurableTurnStatus(ctx, request.TurnID)
	if statusErr == nil {
		if status.TaskUID != attempt.TaskUID || status.Attempt != attempt.Attempt || status.RequestDigest != attempt.RequestDigest {
			return d.markOutcomeUnknown(ctx, task, attempt, fence, harnessV1ReasonProtocolViolation)
		}
		switch status.State {
		case harness.DurableTurnRejected:
			if attempt.State == store.HarnessV1AttemptSubmitting || attempt.State == store.HarnessV1AttemptSubmittedUnknown {
				return d.failUnsubmittedAttempt(ctx, task, attempt, fence, harnessV1ReasonRejected)
			}
			return d.markOutcomeUnknown(ctx, task, attempt, fence, harnessV1ReasonProtocolViolation)
		case harness.DurableTurnOutcomeUnknown:
			if err := validateHarnessV1DurableTerminalReceipt(status, request, true); err != nil {
				return d.markOutcomeUnknown(ctx, task, attempt, fence, harnessV1ReasonProtocolViolation)
			}
			durableOutcomeUnknownReceiptDigest = status.TerminalReceiptDigest
		case harness.DurableTurnTerminal:
			if err := validateHarnessV1DurableTerminalReceipt(status, request, false); err != nil {
				return d.markOutcomeUnknown(ctx, task, attempt, fence, harnessV1ReasonProtocolViolation)
			}
			accepted, err := d.ensureAttemptAccepted(ctx, attempt, fence, status.TerminalReceiptDigest)
			if err != nil {
				return err
			}
			frame, ok := status.TerminalReceipt.HarnessFrame()
			if !ok {
				return d.markOutcomeUnknown(ctx, task, accepted, fence, harnessV1ReasonProtocolViolation)
			}
			return d.settleTerminalFrameWithReceiptDigest(
				ctx, task, protocolClient, request, accepted, fence, frame, status.TerminalReceiptDigest,
			)
		case harness.DurableTurnAccepted, harness.DurableTurnAdmitted:
			accepted, err := d.ensureAttemptAccepted(ctx, attempt, fence, "")
			if err != nil {
				return err
			}
			attempt = accepted
		}
	}

	evidence, err := d.persistedEvidence(ctx, task, attempt)
	if err != nil {
		return err
	}
	if evidence.terminal != "" {
		accepted, err := d.ensureAttemptAccepted(ctx, attempt, fence, "")
		if err != nil {
			return err
		}
		return d.settlePersistedTerminal(ctx, task, protocolClient, request, accepted, fence, evidence)
	}
	if durableOutcomeUnknownReceiptDigest != "" {
		return d.markOutcomeUnknownWithReceipt(
			ctx, task, attempt, fence, harnessV1ReasonOutcomeUnknown, durableOutcomeUnknownReceiptDigest,
		)
	}
	if evidence.found {
		accepted, err := d.ensureAttemptAccepted(ctx, attempt, fence, "")
		if err != nil {
			return err
		}
		return d.streamAcceptedAttempt(ctx, task, verified, protocolClient, request, accepted, fence)
	}
	if harnessV1AttemptHasAdmissionClosedProof(attempt) && statusErr != nil {
		if isHarnessV1DurableTurnNotFoundError(statusErr) {
			return d.retryAdmissionClosedStartTurn(
				ctx, task, verified, protocolClient, request, attempt, fence,
			)
		}
		return fmt.Errorf("recover admission-closed harness v1 turn status: %w", statusErr)
	}
	if statusErr == nil && status != nil && status.State != harness.DurableTurnRejected {
		accepted, err := d.ensureAttemptAccepted(ctx, attempt, fence, status.TerminalReceiptDigest)
		if err != nil {
			return err
		}
		return d.streamAcceptedAttempt(ctx, task, verified, protocolClient, request, accepted, fence)
	}
	if attempt.State == store.HarnessV1AttemptSubmitting {
		var transitionErr error
		attempt, transitionErr = d.transitionAttempt(ctx, attempt, fence, store.HarnessV1AttemptSubmittedUnknown, "submission-unknown-recovery", store.HarnessV1AttemptUpdates{})
		if transitionErr != nil {
			return transitionErr
		}
	}
	return d.markOutcomeUnknown(ctx, task, attempt, fence, harnessV1ReasonOutcomeUnknown)
}

// recoverActiveAttempt consults the durable wrapper ledger before reopening a
// frame stream. A wrapper can commit its terminal receipt immediately before
// either side restarts, so streaming first could turn authoritative terminal
// evidence into OutcomeUnknown when the process-local turn is gone.
func (d *HarnessV1Dispatcher) recoverActiveAttempt(
	ctx context.Context,
	task *corev1alpha1.Task,
	verified *verifiedHarnessV1Execution,
	protocolClient harnessV1ProtocolClient,
	request harness.StartTurnRequest,
	attempt *store.HarnessV1Attempt,
	fence store.ControllerEpochFence,
) error {
	var durableOutcomeUnknownReceiptDigest string
	status, statusErr := protocolClient.DurableTurnStatus(ctx, request.TurnID)
	if statusErr == nil {
		if status == nil || status.TurnID != attempt.TurnID || status.TaskUID != attempt.TaskUID ||
			status.Attempt != attempt.Attempt || status.RequestDigest != attempt.RequestDigest {
			return d.markOutcomeUnknown(ctx, task, attempt, fence, harnessV1ReasonProtocolViolation)
		}
		switch status.State {
		case harness.DurableTurnTerminal:
			if err := validateHarnessV1DurableTerminalReceipt(status, request, false); err != nil {
				return d.markOutcomeUnknown(ctx, task, attempt, fence, harnessV1ReasonProtocolViolation)
			}
			frame, ok := status.TerminalReceipt.HarnessFrame()
			if !ok {
				return d.markOutcomeUnknown(ctx, task, attempt, fence, harnessV1ReasonProtocolViolation)
			}
			return d.settleTerminalFrameWithReceiptDigest(
				ctx, task, protocolClient, request, attempt, fence, frame, status.TerminalReceiptDigest,
			)
		case harness.DurableTurnOutcomeUnknown:
			if err := validateHarnessV1DurableTerminalReceipt(status, request, true); err != nil {
				return d.markOutcomeUnknown(ctx, task, attempt, fence, harnessV1ReasonProtocolViolation)
			}
			durableOutcomeUnknownReceiptDigest = status.TerminalReceiptDigest
		case harness.DurableTurnRejected:
			// An active durable attempt cannot also have definitive
			// non-acceptance evidence.
			return d.markOutcomeUnknown(ctx, task, attempt, fence, harnessV1ReasonProtocolViolation)
		case harness.DurableTurnAdmitted, harness.DurableTurnAccepted:
			// Nonterminal ledger evidence is compatible with streaming below.
		default:
			return d.markOutcomeUnknown(ctx, task, attempt, fence, harnessV1ReasonProtocolViolation)
		}
	}
	if durableOutcomeUnknownReceiptDigest != "" {
		evidence, err := d.persistedEvidence(ctx, task, attempt)
		if err != nil {
			return err
		}
		if evidence.terminal != "" {
			return d.settlePersistedTerminal(ctx, task, protocolClient, request, attempt, fence, evidence)
		}
		return d.markOutcomeUnknownWithReceipt(
			ctx, task, attempt, fence, harnessV1ReasonOutcomeUnknown, durableOutcomeUnknownReceiptDigest,
		)
	}
	return d.streamAcceptedAttempt(ctx, task, verified, protocolClient, request, attempt, fence)
}

func validateHarnessV1DurableTerminalReceipt(
	status *harness.DurableTurnStatus,
	request harness.StartTurnRequest,
	wantOutcomeUnknown bool,
) error {
	if status == nil || status.TerminalReceipt == nil {
		return errors.New("durable terminal receipt is required")
	}
	receipt := status.TerminalReceipt
	if err := receipt.Validate(); err != nil {
		return err
	}
	if receipt.RuntimeSessionID != request.RuntimeSessionID || receipt.TurnID != request.TurnID ||
		receipt.CorrelationID != request.CorrelationID {
		return errors.New("durable terminal receipt identity does not match the immutable request")
	}
	digest, err := harness.DurableTurnTerminalReceiptDigest(*receipt)
	if err != nil {
		return err
	}
	if digest != status.TerminalReceiptDigest {
		return errors.New("durable terminal receipt digest mismatch")
	}
	isOutcomeUnknown := receipt.Kind == harness.DurableTurnTerminalOutcomeUnknown
	if isOutcomeUnknown != wantOutcomeUnknown {
		return errors.New("durable terminal receipt kind does not match ledger state")
	}
	return nil
}

func (d *HarnessV1Dispatcher) recoverSettlingAttempt(
	ctx context.Context,
	task *corev1alpha1.Task,
	protocolClient harnessV1ProtocolClient,
	request harness.StartTurnRequest,
	attempt *store.HarnessV1Attempt,
	fence store.ControllerEpochFence,
) error {
	if attempt == nil || attempt.State != store.HarnessV1AttemptSettling {
		return errors.New("a settling harness v1 attempt is required")
	}
	if err := store.ValidateCanonicalDigest("harness v1 settling terminal receipt digest", attempt.TerminalReceiptDigest); err != nil {
		return err
	}
	status, statusErr := protocolClient.DurableTurnStatus(ctx, request.TurnID)
	if statusErr == nil {
		if status == nil || status.TurnID != attempt.TurnID || status.TaskUID != attempt.TaskUID ||
			status.Attempt != attempt.Attempt || status.RequestDigest != attempt.RequestDigest {
			return errors.New("settling durable turn status does not match the immutable attempt")
		}
	}
	if statusErr == nil && status.State == harness.DurableTurnTerminal {
		if err := validateHarnessV1DurableTerminalReceipt(status, request, false); err != nil {
			return fmt.Errorf("validate settling durable terminal receipt: %w", err)
		}
		if status.TerminalReceiptDigest != attempt.TerminalReceiptDigest {
			return errors.New("settling durable terminal receipt does not match the persisted receipt digest")
		}
		frame, ok := status.TerminalReceipt.HarnessFrame()
		if !ok {
			return errors.New("settling durable terminal receipt has no authoritative terminal frame")
		}
		return d.settleTerminalFrameWithReceiptDigest(
			ctx, task, protocolClient, request, attempt, fence, frame, status.TerminalReceiptDigest,
		)
	}
	if statusErr == nil {
		switch status.State {
		case harness.DurableTurnAdmitted, harness.DurableTurnAccepted:
			// The terminal frame can become visible immediately before the wrapper
			// commits its ledger receipt. Persisted mapped evidence may already be
			// sufficient to finish controller-side settlement.
		case harness.DurableTurnOutcomeUnknown:
			if err := validateHarnessV1DurableTerminalReceipt(status, request, true); err != nil {
				return fmt.Errorf("validate settling durable outcome-unknown receipt: %w", err)
			}
		default:
			return fmt.Errorf("settling durable turn has incompatible ledger state %q", status.State)
		}
	}

	evidence, err := d.persistedEvidence(ctx, task, attempt)
	if err != nil {
		return fmt.Errorf("recover settling persisted terminal evidence: %w", err)
	}
	if evidence.terminal != "" {
		// settlePersistedTerminal reconstructs the canonical receipt and
		// settleTerminalFrameWithReceiptDigest requires its digest to match the
		// immutable digest already stored on the Settling attempt.
		return d.settlePersistedTerminal(ctx, task, protocolClient, request, attempt, fence, evidence)
	}
	if statusErr != nil {
		return fmt.Errorf("recover settling harness v1 terminal receipt: %w", statusErr)
	}
	return fmt.Errorf("durable terminal receipt is not yet available for settling turn %s", request.TurnID)
}

func (d *HarnessV1Dispatcher) ensureAttemptAccepted(
	ctx context.Context,
	attempt *store.HarnessV1Attempt,
	fence store.ControllerEpochFence,
	receiptDigest string,
) (*store.HarnessV1Attempt, error) {
	if attempt.State == store.HarnessV1AttemptAccepted || attempt.State == store.HarnessV1AttemptRunning ||
		attempt.State == store.HarnessV1AttemptCancelRequested || attempt.State == store.HarnessV1AttemptSettling {
		return attempt, nil
	}
	updates := store.HarnessV1AttemptUpdates{}
	if receiptDigest != "" {
		updates.TerminalReceiptDigest = &receiptDigest
	}
	return d.transitionAttempt(ctx, attempt, fence, store.HarnessV1AttemptAccepted, "accepted-recovery", updates)
}

//nolint:gocyclo // Streaming and terminal persistence are one crash-sensitive boundary.
func (d *HarnessV1Dispatcher) streamAcceptedAttempt(
	ctx context.Context,
	task *corev1alpha1.Task,
	_ *verifiedHarnessV1Execution,
	protocolClient harnessV1ProtocolClient,
	request harness.StartTurnRequest,
	attempt *store.HarnessV1Attempt,
	fence store.ControllerEpochFence,
) error {
	evidence, err := d.persistedEvidence(ctx, task, attempt)
	if err != nil {
		return err
	}
	if evidence.terminal != "" {
		return d.settlePersistedTerminal(ctx, task, protocolClient, request, attempt, fence, evidence)
	}
	if evidence.lastSeq > attempt.LastEventSeq {
		last := evidence.lastSeq
		attempt, err = d.transitionAttemptProgress(ctx, attempt, fence, last)
		if err != nil {
			return err
		}
	}
	journalState, err := (harness.TurnJournal{EventStore: d.EventStore, MapContext: harness.EventMapContext{
		Namespace: task.Namespace, TaskName: task.Name, SessionName: request.SessionName,
		AgentName: task.Status.AgentExecutionBinding.Agent.Name, StreamID: task.Name,
	}}).Open(ctx)
	if err != nil {
		return err
	}
	current := attempt
	var terminal *harness.HarnessEventFrame
	streamErr := protocolClient.StreamFrames(ctx, request.TurnID, current.LastEventSeq, func(frame harness.HarnessEventFrame) error {
		if frame.RuntimeSessionID != request.RuntimeSessionID || frame.TurnID != request.TurnID ||
			frame.CorrelationID != request.CorrelationID || frame.Seq <= current.LastEventSeq {
			return fmt.Errorf("harness frame identity or sequence does not match the durable attempt")
		}
		if frame.Type == harness.FrameToolCallRequested || frame.Type == harness.FrameApprovalRequested {
			return fmt.Errorf("harness v1 pure-prompt binding received an unauthorized continuation request")
		}
		if _, _, err := journalState.AppendFrameIfNew(ctx, frame); err != nil {
			return err
		}
		targetState := current.State
		if targetState == store.HarnessV1AttemptAccepted {
			targetState = store.HarnessV1AttemptRunning
		}
		seq := frame.Seq
		if targetState == current.State {
			current, err = d.transitionAttemptProgress(ctx, current, fence, seq)
		} else {
			current, err = d.transitionAttempt(ctx, current, fence, targetState, "frame-"+strconv.FormatInt(seq, 10), store.HarnessV1AttemptUpdates{LastEventSeq: &seq})
		}
		if err != nil {
			return err
		}
		if frame.Type == harness.FrameTurnCompleted || frame.Type == harness.FrameTurnFailed || frame.Type == harness.FrameTurnCancelled {
			copy := frame
			terminal = &copy
			return errHarnessV1TerminalFrame
		}
		return nil
	})
	if errors.Is(streamErr, errHarnessV1TerminalFrame) {
		streamErr = nil
	}
	if terminal != nil {
		return d.settleTerminalFrame(ctx, task, protocolClient, request, current, fence, *terminal)
	}
	if streamErr != nil {
		var clientErr harness.ClientError
		if errors.As(streamErr, &clientErr) && clientErr.IsProtocolViolation() {
			return d.markOutcomeUnknown(ctx, task, current, fence, harnessV1ReasonProtocolViolation)
		}
		return fmt.Errorf("%w: %v", errHarnessV1StreamEndedWithoutTerminal, streamErr)
	}
	return errHarnessV1StreamEndedWithoutTerminal
}

func (d *HarnessV1Dispatcher) persistedEvidence(
	ctx context.Context,
	task *corev1alpha1.Task,
	attempt *store.HarnessV1Attempt,
) (persistedHarnessV1Evidence, error) {
	var evidence persistedHarnessV1Evidence
	var after int64
	for {
		events, err := d.EventStore.ListExecutionEvents(ctx, store.ExecutionEventFilter{
			Namespace: task.Namespace, StreamType: store.ExecutionEventStreamTypeTask, StreamID: task.Name,
			AfterSeq: after, Limit: store.MaxExecutionEventLimit,
		})
		if err != nil {
			return evidence, err
		}
		if len(events) == 0 {
			return evidence, nil
		}
		for _, event := range events {
			if event.Seq > after {
				after = event.Seq
			}
			identity, ok := harness.MappedFrameIdentityFromEvent(event)
			if !ok || !identity.HasTurnID(harness.HarnessTurnID(attempt.TurnID)) {
				continue
			}
			evidence.found = true
			if identity.Seq > evidence.lastSeq {
				evidence.lastSeq = identity.Seq
			}
			if identity.FrameType == harness.FrameTurnCompleted || identity.FrameType == harness.FrameTurnFailed || identity.FrameType == harness.FrameTurnCancelled {
				evidence.terminal = identity.FrameType
				var content struct {
					Completed *harness.TurnCompleted `json:"completed"`
					Failed    *harness.TurnFailed    `json:"failed"`
				}
				if err := json.Unmarshal(event.Content, &content); err == nil {
					evidence.completed = content.Completed
					evidence.failed = content.Failed
				}
			}
		}
		if len(events) < store.MaxExecutionEventLimit {
			return evidence, nil
		}
	}
}

func (d *HarnessV1Dispatcher) settlePersistedTerminal(
	ctx context.Context,
	task *corev1alpha1.Task,
	protocolClient harnessV1ProtocolClient,
	request harness.StartTurnRequest,
	attempt *store.HarnessV1Attempt,
	fence store.ControllerEpochFence,
	evidence persistedHarnessV1Evidence,
) error {
	frame := harness.HarnessEventFrame{
		Version: harness.ProtocolVersion, Type: evidence.terminal, RuntimeSessionID: request.RuntimeSessionID,
		TurnID: request.TurnID, CorrelationID: request.CorrelationID, Seq: evidence.lastSeq,
		Completed: evidence.completed, Failed: evidence.failed,
	}
	return d.settleTerminalFrame(ctx, task, protocolClient, request, attempt, fence, frame)
}

func (d *HarnessV1Dispatcher) settleTerminalFrame(
	ctx context.Context,
	task *corev1alpha1.Task,
	protocolClient harnessV1ProtocolClient,
	request harness.StartTurnRequest,
	attempt *store.HarnessV1Attempt,
	fence store.ControllerEpochFence,
	frame harness.HarnessEventFrame,
) error {
	receipt, err := harness.DurableTurnTerminalReceiptFromFrame(frame)
	if err != nil {
		return err
	}
	receiptDigest, err := harness.DurableTurnTerminalReceiptDigest(receipt)
	if err != nil {
		return err
	}
	return d.settleTerminalFrameWithReceiptDigest(
		ctx, task, protocolClient, request, attempt, fence, frame, receiptDigest,
	)
}

func (d *HarnessV1Dispatcher) settleTerminalFrameWithReceiptDigest(
	ctx context.Context,
	task *corev1alpha1.Task,
	protocolClient harnessV1ProtocolClient,
	request harness.StartTurnRequest,
	attempt *store.HarnessV1Attempt,
	fence store.ControllerEpochFence,
	frame harness.HarnessEventFrame,
	receiptDigest string,
) error {
	if err := store.ValidateCanonicalDigest("harness v1 terminal receipt digest", receiptDigest); err != nil {
		return err
	}
	if attempt.State == store.HarnessV1AttemptSettling && attempt.TerminalReceiptDigest != receiptDigest {
		return errors.New("terminal receipt does not match the persisted settling digest")
	}
	var err error
	if attempt.State != store.HarnessV1AttemptSettling {
		if store.IsTerminalHarnessV1AttemptState(attempt.State) {
			return d.reconcileTerminalAttempt(ctx, task, attempt, fence)
		}
		attempt, err = d.transitionAttempt(ctx, attempt, fence, store.HarnessV1AttemptSettling, "settling", store.HarnessV1AttemptUpdates{
			TerminalReceiptDigest: &receiptDigest,
		})
		if err != nil {
			return err
		}
	}
	switch frame.Type {
	case harness.FrameTurnCompleted:
		if frame.Completed == nil {
			return d.markOutcomeUnknown(ctx, task, attempt, fence, harnessV1ReasonProtocolViolation)
		}
		result := []byte(frame.Completed.Result)
		if frame.Completed.OutputRef != "" {
			result, err = protocolClient.FetchTurnOutput(ctx, request.TurnID, frame.Completed.OutputRef)
			if err != nil {
				return fmt.Errorf("fetch harness v1 terminal output: %w", err)
			}
		}
		if err := d.ResultStore.SaveResult(ctx, task.Namespace, task.Name, result); err != nil {
			return err
		}
		if frame.Completed.OutputRef != "" {
			return d.finishAttemptWithOutputAcknowledgement(
				ctx, task, protocolClient, request, attempt, fence,
				store.HarnessV1AttemptSucceeded, "Succeeded", receiptDigest, frame.Completed.OutputRef,
			)
		}
		return d.finishAttempt(ctx, task, attempt, fence, store.HarnessV1AttemptSucceeded, "Succeeded", receiptDigest)
	case harness.FrameTurnFailed:
		reason := harnessV1ReasonFailed
		if frame.Failed != nil && frame.Failed.Retryable {
			reason = harnessV1ReasonRetryableFailure
		}
		if frame.Failed != nil && frame.Failed.OutputRef != "" {
			result, err := protocolClient.FetchTurnOutput(ctx, request.TurnID, frame.Failed.OutputRef)
			if err != nil {
				return fmt.Errorf("fetch harness v1 failed-turn output: %w", err)
			}
			if err := d.ResultStore.SaveResult(ctx, task.Namespace, task.Name, result); err != nil {
				return err
			}
			return d.finishAttemptWithOutputAcknowledgement(
				ctx, task, protocolClient, request, attempt, fence,
				store.HarnessV1AttemptFailed, reason, receiptDigest, frame.Failed.OutputRef,
			)
		}
		return d.finishAttempt(ctx, task, attempt, fence, store.HarnessV1AttemptFailed, reason, receiptDigest)
	case harness.FrameTurnCancelled:
		return d.finishAttempt(ctx, task, attempt, fence, store.HarnessV1AttemptCancelled, harnessV1ReasonCancelled, receiptDigest)
	default:
		return d.markOutcomeUnknown(ctx, task, attempt, fence, harnessV1ReasonProtocolViolation)
	}
}

func (d *HarnessV1Dispatcher) cancelAttempt(
	ctx context.Context,
	task *corev1alpha1.Task,
	verified *verifiedHarnessV1Execution,
	attempt *store.HarnessV1Attempt,
	fence store.ControllerEpochFence,
) error {
	if attempt.State == store.HarnessV1AttemptPrepared {
		return d.finishAttempt(ctx, task, attempt, fence, store.HarnessV1AttemptCancelled, harnessV1ReasonCancelled, "")
	}
	protocolClient, request, err := d.protocolClientAndRequest(ctx, task, verified, attempt)
	if err != nil {
		return d.markOutcomeUnknown(ctx, task, attempt, fence, harnessV1ReasonCredentialChanged)
	}
	if attempt.State == store.HarnessV1AttemptSubmitting || attempt.State == store.HarnessV1AttemptSubmittedUnknown {
		return d.recoverAmbiguousSubmission(ctx, task, verified, protocolClient, request, attempt, fence)
	}
	if attempt.State != store.HarnessV1AttemptCancelRequested {
		now := time.Now().UTC()
		attempt, err = d.transitionAttempt(ctx, attempt, fence, store.HarnessV1AttemptCancelRequested, "cancel-requested", store.HarnessV1AttemptUpdates{CancelRequestedAt: &now})
		if err != nil {
			return err
		}
	}
	_, cancelErr := protocolClient.CancelTurn(ctx, harness.CancelTurnRequest{
		Version: harness.ProtocolVersion, Namespace: task.Namespace, TaskName: task.Name,
		SessionName: request.SessionName, RuntimeSessionID: request.RuntimeSessionID,
		TurnID: request.TurnID, CorrelationID: request.CorrelationID, Reason: "Task cancellation requested",
	})
	if cancelErr != nil {
		return fmt.Errorf("request harness v1 cancellation: %w", cancelErr)
	}
	// CancelAccepted is explicitly nonterminal; only a terminal frame or durable
	// receipt may settle the attempt.
	return d.streamAcceptedAttempt(ctx, task, verified, protocolClient, request, attempt, fence)
}

func (d *HarnessV1Dispatcher) failUnsubmittedAttempt(
	ctx context.Context,
	task *corev1alpha1.Task,
	attempt *store.HarnessV1Attempt,
	fence store.ControllerEpochFence,
	reason string,
) error {
	if attempt.State != store.HarnessV1AttemptPrepared && attempt.State != store.HarnessV1AttemptSubmitting && attempt.State != store.HarnessV1AttemptSubmittedUnknown {
		return d.markOutcomeUnknown(ctx, task, attempt, fence, reason)
	}
	updated, err := d.transitionAttempt(ctx, attempt, fence, store.HarnessV1AttemptRejected, "rejected-"+reason, store.HarnessV1AttemptUpdates{TerminalReason: &reason})
	if err != nil {
		return err
	}
	return d.reconcileTerminalAttempt(ctx, task, updated, fence)
}

func (d *HarnessV1Dispatcher) markOutcomeUnknown(
	ctx context.Context,
	task *corev1alpha1.Task,
	attempt *store.HarnessV1Attempt,
	fence store.ControllerEpochFence,
	reason string,
) error {
	return d.markOutcomeUnknownWithReceipt(ctx, task, attempt, fence, reason, "")
}

func (d *HarnessV1Dispatcher) markOutcomeUnknownWithReceipt(
	ctx context.Context,
	task *corev1alpha1.Task,
	attempt *store.HarnessV1Attempt,
	fence store.ControllerEpochFence,
	reason, receiptDigest string,
) error {
	if attempt.State == store.HarnessV1AttemptSubmitting {
		var err error
		attempt, err = d.transitionAttempt(ctx, attempt, fence, store.HarnessV1AttemptSubmittedUnknown, "submission-unknown-before-outcome", store.HarnessV1AttemptUpdates{})
		if err != nil {
			return err
		}
	}
	if attempt.State == store.HarnessV1AttemptPrepared || attempt.State == store.HarnessV1AttemptRejected || store.IsTerminalHarnessV1AttemptState(attempt.State) {
		if attempt.State == store.HarnessV1AttemptOutcomeUnknown {
			return d.reconcileTerminalAttempt(ctx, task, attempt, fence)
		}
		return fmt.Errorf("attempt state %s cannot become OutcomeUnknown", attempt.State)
	}
	updates := store.HarnessV1AttemptUpdates{TerminalReason: &reason}
	if receiptDigest != "" {
		updates.TerminalReceiptDigest = &receiptDigest
	}
	updated, err := d.transitionAttempt(ctx, attempt, fence, store.HarnessV1AttemptOutcomeUnknown, "outcome-unknown-"+reason, updates)
	if err != nil {
		return err
	}
	return d.reconcileTerminalAttempt(ctx, task, updated, fence)
}

func (d *HarnessV1Dispatcher) finishAttempt(
	ctx context.Context,
	task *corev1alpha1.Task,
	attempt *store.HarnessV1Attempt,
	fence store.ControllerEpochFence,
	target store.HarnessV1AttemptState,
	reason, receiptDigest string,
) error {
	updated, err := d.finishAttemptDurably(ctx, attempt, fence, target, reason, receiptDigest)
	if err != nil {
		return err
	}
	return d.reconcileTerminalAttempt(ctx, task, updated, fence)
}

func (d *HarnessV1Dispatcher) finishAttemptWithOutputAcknowledgement(
	ctx context.Context,
	task *corev1alpha1.Task,
	protocolClient harnessV1ProtocolClient,
	request harness.StartTurnRequest,
	attempt *store.HarnessV1Attempt,
	fence store.ControllerEpochFence,
	target store.HarnessV1AttemptState,
	reason, receiptDigest, outputRef string,
) error {
	updated, err := d.finishAttemptDurably(ctx, attempt, fence, target, reason, receiptDigest)
	if err != nil {
		return err
	}
	if err := d.acknowledgeTerminalOutput(ctx, task, protocolClient, request, updated, outputRef); err != nil {
		return err
	}
	return d.reconcileTerminalAttempt(ctx, task, updated, fence)
}

func (d *HarnessV1Dispatcher) finishAttemptDurably(
	ctx context.Context,
	attempt *store.HarnessV1Attempt,
	fence store.ControllerEpochFence,
	target store.HarnessV1AttemptState,
	reason, receiptDigest string,
) (*store.HarnessV1Attempt, error) {
	if attempt.State == target {
		return attempt, nil
	}
	updates := store.HarnessV1AttemptUpdates{TerminalReason: &reason}
	if receiptDigest != "" {
		updates.TerminalReceiptDigest = &receiptDigest
	}
	return d.transitionAttempt(ctx, attempt, fence, target, "terminal-"+string(target), updates)
}

func (d *HarnessV1Dispatcher) reconcileTerminalAttemptAfterOutputAcknowledgement(
	ctx context.Context,
	task *corev1alpha1.Task,
	attempt *store.HarnessV1Attempt,
	fence store.ControllerEpochFence,
) error {
	if (attempt.State != store.HarnessV1AttemptSucceeded && attempt.State != store.HarnessV1AttemptFailed) ||
		attempt.TerminalReceiptDigest == "" {
		return d.reconcileTerminalAttempt(ctx, task, attempt, fence)
	}

	outputRef, found, err := d.persistedTerminalOutputRef(ctx, task, attempt)
	if err != nil {
		return err
	}
	var (
		protocolClient harnessV1ProtocolClient
		request        harness.StartTurnRequest
	)
	if !found {
		verified, loadErr := d.loadHarnessV1ExecutionForSettlement(ctx, task, task.Status.AgentExecutionBinding)
		if loadErr != nil {
			return loadErr
		}
		protocolClient, request, err = d.protocolClientAndRequest(ctx, task, verified, attempt)
		if err != nil {
			return err
		}
		status, statusErr := protocolClient.DurableTurnStatus(ctx, request.TurnID)
		if statusErr != nil {
			return fmt.Errorf("recover harness v1 terminal output receipt: %w", statusErr)
		}
		if status == nil || status.TurnID != attempt.TurnID || status.TaskUID != attempt.TaskUID ||
			status.Attempt != attempt.Attempt || status.RequestDigest != attempt.RequestDigest ||
			status.State != harness.DurableTurnTerminal || status.TerminalReceiptDigest != attempt.TerminalReceiptDigest {
			return errors.New("durable terminal output receipt does not match the settled attempt")
		}
		if err := validateHarnessV1DurableTerminalReceipt(status, request, false); err != nil {
			return fmt.Errorf("validate harness v1 terminal output receipt: %w", err)
		}
		frame, ok := status.TerminalReceipt.HarnessFrame()
		if !ok {
			return errors.New("durable terminal output receipt has no terminal frame")
		}
		outputRef = harnessV1TerminalOutputRef(frame)
	}
	if outputRef == "" {
		return d.reconcileTerminalAttempt(ctx, task, attempt, fence)
	}
	if protocolClient == nil {
		verified, loadErr := d.loadHarnessV1ExecutionForSettlement(ctx, task, task.Status.AgentExecutionBinding)
		if loadErr != nil {
			return loadErr
		}
		protocolClient, request, err = d.protocolClientAndRequest(ctx, task, verified, attempt)
		if err != nil {
			return err
		}
	}
	if err := d.acknowledgeTerminalOutput(ctx, task, protocolClient, request, attempt, outputRef); err != nil {
		return err
	}
	return d.reconcileTerminalAttempt(ctx, task, attempt, fence)
}

func (d *HarnessV1Dispatcher) persistedTerminalOutputRef(
	ctx context.Context,
	task *corev1alpha1.Task,
	attempt *store.HarnessV1Attempt,
) (string, bool, error) {
	evidence, err := d.persistedEvidence(ctx, task, attempt)
	if err != nil {
		return "", false, err
	}
	if evidence.terminal == "" {
		return "", false, nil
	}
	frame := harness.HarnessEventFrame{
		Version: harness.ProtocolVersion, Type: evidence.terminal,
		RuntimeSessionID: harness.RuntimeSessionID(attempt.RuntimeSessionID),
		TurnID:           harness.HarnessTurnID(attempt.TurnID), CorrelationID: attempt.CorrelationID,
		Seq: evidence.lastSeq, Completed: evidence.completed, Failed: evidence.failed,
	}
	receipt, err := harness.DurableTurnTerminalReceiptFromFrame(frame)
	if err != nil {
		return "", false, err
	}
	digest, err := harness.DurableTurnTerminalReceiptDigest(receipt)
	if err != nil {
		return "", false, err
	}
	if digest != attempt.TerminalReceiptDigest {
		return "", false, errors.New("persisted terminal output receipt does not match the settled attempt")
	}
	return harnessV1TerminalOutputRef(frame), true, nil
}

func harnessV1TerminalOutputRef(frame harness.HarnessEventFrame) string {
	switch frame.Type {
	case harness.FrameTurnCompleted:
		if frame.Completed != nil {
			return frame.Completed.OutputRef
		}
	case harness.FrameTurnFailed:
		if frame.Failed != nil {
			return frame.Failed.OutputRef
		}
	}
	return ""
}

func (d *HarnessV1Dispatcher) acknowledgeTerminalOutput(
	ctx context.Context,
	task *corev1alpha1.Task,
	protocolClient harnessV1ProtocolClient,
	request harness.StartTurnRequest,
	attempt *store.HarnessV1Attempt,
	outputRef string,
) error {
	if outputRef == "" {
		return nil
	}
	if !store.IsTerminalHarnessV1AttemptState(attempt.State) {
		return errors.New("harness v1 output cannot be acknowledged before terminal attempt settlement")
	}
	if _, err := d.ResultStore.GetResult(ctx, task.Namespace, task.Name); err != nil {
		return fmt.Errorf("verify durable harness v1 result before output acknowledgement: %w", err)
	}
	if err := protocolClient.AcknowledgeTurnOutput(ctx, harness.TurnOutputAcknowledgementRequest{
		Version: harness.ProtocolVersion, TurnID: request.TurnID,
		OutputRef: outputRef, TerminalReceiptDigest: attempt.TerminalReceiptDigest,
	}); err != nil {
		return fmt.Errorf("acknowledge durable harness v1 terminal output: %w", err)
	}
	return nil
}

func (d *HarnessV1Dispatcher) reconcileTerminalAttempt(
	ctx context.Context,
	task *corev1alpha1.Task,
	attempt *store.HarnessV1Attempt,
	fence store.ControllerEpochFence,
) error {
	retryPolicy, err := d.frozenRetryPolicy(ctx, attempt)
	if err != nil {
		return err
	}
	retrying := ((attempt.State == store.HarnessV1AttemptRejected && attempt.TerminalReason == harnessV1ReasonRejected) ||
		(attempt.State == store.HarnessV1AttemptFailed && attempt.TerminalReason == harnessV1ReasonRetryableFailure)) &&
		d.retryAllowed(task, attempt, retryPolicy)
	verified, err := d.loadHarnessV1ExecutionForSettlement(ctx, task, task.Status.AgentExecutionBinding)
	if err != nil {
		return err
	}
	sessionSettled, err := d.finalizeHarnessV1TaskSession(ctx, task, verified, attempt, fence, retrying)
	if err != nil {
		return err
	}
	if retrying {
		return d.createRetryAttempt(ctx, task, attempt, fence, retryPolicy)
	}
	if err := d.settleHarnessV1RuntimeSessionRecord(ctx, task, attempt); err != nil {
		return err
	}
	if sessionSettled {
		// The deferred outbox projection is the only terminal Task visibility
		// point for Session-backed execution.
		return nil
	}
	return d.projectAttemptState(ctx, task, attempt, d.terminalMessage(attempt))
}

func (d *HarnessV1Dispatcher) frozenRetryPolicy(
	ctx context.Context,
	attempt *store.HarnessV1Attempt,
) (*corev1alpha1.RetryPolicy, error) {
	snapshot, err := d.Snapshots.GetAgentExecutionSnapshot(ctx, store.AgentExecutionSnapshotKey{
		TaskUID: attempt.TaskUID, Digest: attempt.SnapshotDigest,
	})
	if err != nil {
		return nil, err
	}
	if store.CanonicalAgentExecutionSnapshotDigest(snapshot.Body) != attempt.SnapshotDigest {
		return nil, errors.New("harness v1 retry snapshot digest mismatch")
	}
	body, err := decodeAgentExecutionSnapshot(snapshot.Body)
	if err != nil {
		return nil, err
	}
	if body.ContractVersion != string(corev1alpha1.AgentRuntimeContractHarnessV1) || body.HarnessV1 == nil {
		return nil, errors.New("harness v1 retry snapshot route mismatch")
	}
	return body.RetryPolicy.DeepCopy(), nil
}

func (d *HarnessV1Dispatcher) retryAllowed(
	task *corev1alpha1.Task,
	attempt *store.HarnessV1Attempt,
	policy *corev1alpha1.RetryPolicy,
) bool {
	return task != nil && attempt != nil && task.DeletionTimestamp.IsZero() &&
		attempt.DuplicateSafe && attempt.RetryClass == store.HarnessV1RetryClassDuplicateSafe &&
		policy != nil && attempt.Attempt <= policy.MaxRetries &&
		attempt.State != store.HarnessV1AttemptOutcomeUnknown
}

func (d *HarnessV1Dispatcher) createRetryAttempt(
	ctx context.Context,
	task *corev1alpha1.Task,
	previous *store.HarnessV1Attempt,
	fence store.ControllerEpochFence,
	retryPolicy *corev1alpha1.RetryPolicy,
) error {
	if delay := harnessV1RetryDelay(retryPolicy, previous.Attempt); delay > 0 && time.Since(previous.UpdatedAt) < delay {
		return nil
	}
	number := previous.Attempt + 1
	turnID := harnessV1TurnID(task, number)
	next := &store.HarnessV1Attempt{
		Namespace: previous.Namespace, TaskName: previous.TaskName, TaskUID: previous.TaskUID, Attempt: number,
		BindingDigest: previous.BindingDigest, SnapshotDigest: previous.SnapshotDigest,
		TurnID: string(turnID), RuntimeSessionID: previous.RuntimeSessionID, CorrelationID: previous.CorrelationID,
		Backend: previous.Backend, BackendEndpoint: previous.BackendEndpoint,
		AuthSecretNamespace: previous.AuthSecretNamespace, AuthSecretName: previous.AuthSecretName,
		AuthSecretKey: previous.AuthSecretKey, AuthSecretUID: previous.AuthSecretUID,
		AuthSecretResourceVersion: previous.AuthSecretResourceVersion,
		State:                     store.HarnessV1AttemptPrepared, DuplicateSafe: previous.DuplicateSafe, RetryClass: previous.RetryClass,
		ControllerEpochName: fence.Name, ControllerEpoch: fence.Epoch,
	}
	verifier := TaskReconciler{
		Client: d.Client, APIReader: d.APIReader, AgentExecutionSnapshots: d.Snapshots,
		AgentExecutionBindingReservations: d.BindingReservations,
	}
	verified, err := verifier.loadVerifiedHarnessV1ExecutionForRecovery(
		ctx, task, task.Status.AgentExecutionBinding, false,
	)
	if err != nil {
		return err
	}
	env, err := resolveFrozenHarnessV1Env(ctx, d.APIReader, verified.body.HarnessV1.CredentialRefs)
	if err != nil {
		return err
	}
	request, err := buildHarnessV1StartTurnRequest(task, verified, next, env)
	if err != nil {
		return err
	}
	next.RequestDigest, err = harness.CanonicalStartTurnRequestDigest(request)
	if err != nil {
		return err
	}
	if err := d.Attempts.CreateHarnessV1Attempt(ctx, next, fence); err != nil {
		return err
	}
	return d.projectAttemptState(ctx, task, next, "queued safe harness v1 retry")
}

func harnessV1RetryDelay(policy *corev1alpha1.RetryPolicy, completedAttempt int32) time.Duration {
	if policy == nil || policy.InitialDelay == nil || policy.InitialDelay.Duration <= 0 {
		return 0
	}
	delay := float64(policy.InitialDelay.Duration)
	multiplier := policy.BackoffMultiplier
	if multiplier <= 0 {
		multiplier = 1
	}
	for i := int32(1); i < completedAttempt; i++ {
		delay *= multiplier
	}
	return time.Duration(delay)
}

func (d *HarnessV1Dispatcher) transitionAttempt(
	ctx context.Context,
	attempt *store.HarnessV1Attempt,
	fence store.ControllerEpochFence,
	target store.HarnessV1AttemptState,
	operation string,
	updates store.HarnessV1AttemptUpdates,
) (*store.HarnessV1Attempt, error) {
	digest, err := acpDomainDigest("harness-v1-attempt-transition", struct {
		ID        string                        `json:"id"`
		Version   int64                         `json:"version"`
		From      store.HarnessV1AttemptState   `json:"from"`
		To        store.HarnessV1AttemptState   `json:"to"`
		Operation string                        `json:"operation"`
		Updates   store.HarnessV1AttemptUpdates `json:"updates"`
	}{harnessV1AttemptKey(attempt).CanonicalID(), attempt.Version, attempt.State, target, operation, updates})
	if err != nil {
		return nil, err
	}
	return d.Attempts.TransitionHarnessV1Attempt(ctx, store.HarnessV1AttemptTransition{
		Key: harnessV1AttemptKey(attempt), ExpectedVersion: attempt.Version, ExpectedState: attempt.State, TargetState: target,
		OperationID: operation + "-" + strconv.FormatInt(attempt.Version, 10), OperationDigest: digest,
		Fence: fence, Updates: updates,
	})
}

func (d *HarnessV1Dispatcher) transitionAttemptProgress(
	ctx context.Context,
	attempt *store.HarnessV1Attempt,
	fence store.ControllerEpochFence,
	seq int64,
) (*store.HarnessV1Attempt, error) {
	if seq <= attempt.LastEventSeq {
		return attempt, nil
	}
	// Progress uses the legal Running -> Running self-transition only through an
	// explicit store operation. The aggregate transition validator intentionally
	// excludes self-transitions, so persist progress while advancing Accepted to
	// Running; subsequent sequence durability is represented by mapped frames and
	// folded into the next state transition.
	if attempt.State == store.HarnessV1AttemptAccepted {
		return d.transitionAttempt(ctx, attempt, fence, store.HarnessV1AttemptRunning, "running-"+strconv.FormatInt(seq, 10), store.HarnessV1AttemptUpdates{LastEventSeq: &seq})
	}
	return attempt, nil
}

func (d *HarnessV1Dispatcher) projectAttemptState(
	ctx context.Context,
	task *corev1alpha1.Task,
	attempt *store.HarnessV1Attempt,
	message string,
) error {
	key := types.NamespacedName{Namespace: task.Namespace, Name: task.Name}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &corev1alpha1.Task{}
		if err := d.Client.Get(ctx, key, latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		if latest.UID != task.UID || latest.Status.AgentExecutionBinding == nil ||
			latest.Status.AgentExecutionBinding.BindingDigest != attempt.BindingDigest {
			return errors.New("task binding changed before harness v1 status projection")
		}
		if latest.Status.HarnessRuntime != nil && latest.Status.HarnessRuntime.Attempt > attempt.Attempt {
			return nil
		}
		if harnessV1AttemptProjectionMatches(latest, attempt) {
			return nil
		}
		base := latest.DeepCopy()
		now := metav1.Now()
		state, outcome, phase := harnessV1TaskProjection(attempt.State)
		if latest.Status.HarnessRuntime == nil {
			latest.Status.HarnessRuntime = &corev1alpha1.HarnessRuntimeStatus{ContractVersion: harness.ProtocolVersion}
		}
		latest.Status.HarnessRuntime.Attempt = attempt.Attempt
		latest.Status.HarnessRuntime.TurnID = attempt.TurnID
		latest.Status.HarnessRuntime.RuntimeSessionID = attempt.RuntimeSessionID
		latest.Status.HarnessRuntime.State = state
		latest.Status.HarnessRuntime.Outcome = outcome
		latest.Status.HarnessRuntime.Reason = attempt.TerminalReason
		latest.Status.HarnessRuntime.TerminalReceiptDigest = attempt.TerminalReceiptDigest
		latest.Status.HarnessRuntime.RequestDigest = attempt.RequestDigest
		latest.Status.HarnessRuntime.ControllerEpoch = attempt.ControllerEpoch
		latest.Status.HarnessRuntime.LastEventSeq = attempt.LastEventSeq
		latest.Status.HarnessRuntime.Message = message
		latest.Status.HarnessRuntime.LastTransitionTime = &now
		if attempt.CancelRequestedAt != nil {
			requestedAt := metav1.NewTime(attempt.CancelRequestedAt.UTC())
			latest.Status.HarnessRuntime.CancelRequestedAt = &requestedAt
		} else {
			latest.Status.HarnessRuntime.CancelRequestedAt = nil
		}
		latest.Status.Attempts = attempt.Attempt
		latest.Status.Phase = phase
		latest.Status.Message = message
		if latest.Status.StartTime == nil && phase == corev1alpha1.TaskPhaseRunning {
			latest.Status.StartTime = &now
		}
		if store.IsTerminalHarnessV1AttemptState(attempt.State) {
			latest.Status.CompletionTime = &now
		}
		return d.Client.Status().Patch(ctx, latest, client.MergeFrom(base))
	})
}

func harnessV1AttemptProjectionMatches(task *corev1alpha1.Task, attempt *store.HarnessV1Attempt) bool {
	if task == nil || attempt == nil || task.Status.HarnessRuntime == nil {
		return false
	}
	state, outcome, phase := harnessV1TaskProjection(attempt.State)
	runtime := task.Status.HarnessRuntime
	if runtime.Attempt != attempt.Attempt || runtime.TurnID != attempt.TurnID ||
		runtime.RuntimeSessionID != attempt.RuntimeSessionID || runtime.State != state ||
		runtime.Outcome != outcome || runtime.Reason != attempt.TerminalReason ||
		runtime.TerminalReceiptDigest != attempt.TerminalReceiptDigest ||
		runtime.RequestDigest != attempt.RequestDigest || runtime.ControllerEpoch != attempt.ControllerEpoch ||
		runtime.LastEventSeq != attempt.LastEventSeq || runtime.LastTransitionTime == nil ||
		task.Status.Attempts != attempt.Attempt || task.Status.Phase != phase {
		return false
	}
	if attempt.CancelRequestedAt == nil {
		if runtime.CancelRequestedAt != nil {
			return false
		}
	} else if runtime.CancelRequestedAt == nil || !runtime.CancelRequestedAt.Time.Equal(attempt.CancelRequestedAt.UTC()) {
		return false
	}
	return !store.IsTerminalHarnessV1AttemptState(attempt.State) || task.Status.CompletionTime != nil
}

func harnessV1TaskProjection(state store.HarnessV1AttemptState) (
	corev1alpha1.TaskExecutionState,
	corev1alpha1.TaskExecutionOutcome,
	corev1alpha1.TaskPhase,
) {
	switch state {
	case store.HarnessV1AttemptPrepared:
		return corev1alpha1.TaskExecutionStateQueued, "", corev1alpha1.TaskPhaseRunning
	case store.HarnessV1AttemptSubmitting:
		return corev1alpha1.TaskExecutionStateSubmitting, "", corev1alpha1.TaskPhaseRunning
	case store.HarnessV1AttemptSubmittedUnknown:
		return corev1alpha1.TaskExecutionStateSubmittedUnknown, "", corev1alpha1.TaskPhaseRunning
	case store.HarnessV1AttemptAccepted:
		return corev1alpha1.TaskExecutionStateAccepted, "", corev1alpha1.TaskPhaseRunning
	case store.HarnessV1AttemptRunning:
		return corev1alpha1.TaskExecutionStateRunning, "", corev1alpha1.TaskPhaseRunning
	case store.HarnessV1AttemptCancelRequested, store.HarnessV1AttemptSettling:
		return corev1alpha1.TaskExecutionStateSettling, "", corev1alpha1.TaskPhaseRunning
	case store.HarnessV1AttemptSucceeded:
		return corev1alpha1.TaskExecutionStateSucceeded, corev1alpha1.TaskExecutionOutcomeSucceeded, corev1alpha1.TaskPhaseSucceeded
	case store.HarnessV1AttemptCancelled:
		return corev1alpha1.TaskExecutionStateCancelled, corev1alpha1.TaskExecutionOutcomeCancelled, corev1alpha1.TaskPhaseCancelled
	case store.HarnessV1AttemptOutcomeUnknown:
		return corev1alpha1.TaskExecutionStateOutcomeUnknown, corev1alpha1.TaskExecutionOutcomeOutcomeUnknown, corev1alpha1.TaskPhaseFailed
	case store.HarnessV1AttemptRejected, store.HarnessV1AttemptFailed:
		return corev1alpha1.TaskExecutionStateFailed, corev1alpha1.TaskExecutionOutcomeFailed, corev1alpha1.TaskPhaseFailed
	default:
		return corev1alpha1.TaskExecutionStateOutcomeUnknown, corev1alpha1.TaskExecutionOutcomeOutcomeUnknown, corev1alpha1.TaskPhaseFailed
	}
}

func (d *HarnessV1Dispatcher) terminalMessage(attempt *store.HarnessV1Attempt) string {
	switch attempt.State {
	case store.HarnessV1AttemptSucceeded:
		return "harness v1 turn succeeded"
	case store.HarnessV1AttemptCancelled:
		return "harness v1 turn cancelled"
	case store.HarnessV1AttemptOutcomeUnknown:
		return harnessV1OutcomeUnknownMessage
	case store.HarnessV1AttemptRejected:
		return "harness v1 turn was definitively not accepted"
	default:
		return "harness v1 turn failed"
	}
}

func harnessV1AttemptKey(a *store.HarnessV1Attempt) store.HarnessV1AttemptKey {
	return store.HarnessV1AttemptKey{Namespace: a.Namespace, TaskUID: a.TaskUID, Attempt: a.Attempt}
}

var _ interface{ NeedLeaderElection() bool } = (*HarnessV1Dispatcher)(nil)
