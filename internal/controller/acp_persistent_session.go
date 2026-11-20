/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

// ADR 0059: keep a Session's ACP RuntimeSession live across warm-window turns.
//
// Turn N keeps its live RuntimeSession through a durable handoff record
// (SessionControl.ResidentRuntime) written in the same fenced finalization
// write that releases its Session lease. Turn N+1 claims the record in the
// same CAS that acquires the next lease, then gates the reuse on the full
// ADR 0057 attach handshake and an exact quiescent runtime status before any
// runtime mutation. Every refusal falls back to a fresh RuntimeSession; none
// fails the Task.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/operatoraccess"
	"github.com/orka-agents/orka/internal/store"
)

const (
	// ACPWorkspaceClassPersistentSessionAnnotation on an ExecutionWorkspaceClass
	// set to "disabled" forces a fresh RuntimeSession every turn. It is frozen
	// into the execution snapshot's class binding when the class is resolved.
	ACPWorkspaceClassPersistentSessionAnnotation = "acp.workspace.orka.ai/persistent-session"
	acpWorkspacePersistentSessionDisabled        = "disabled"

	// acpTaskPersistentSessionHandoffAnnotation names the handoff record a Task
	// queued against while it held its attachment but before the provider
	// acknowledged the epoch. Its presence obliges the dispatcher to run the
	// full attach handshake before the first runtime contact.
	acpTaskPersistentSessionHandoffAnnotation = "acp.workspace.orka.ai/persistent-session-handoff"
	// acpTaskPersistentSessionRefusedAnnotation records the bounded reason a
	// Task's reuse was refused at the gate; later attempts never claim.
	acpTaskPersistentSessionRefusedAnnotation = "acp.workspace.orka.ai/persistent-session-refused"
	// acpWorkspaceAttachedFromWarmAnnotation is written by the same optimistic
	// attach patch that installs the attachment:
	// "<taskUID> <epoch> <observed suspend-at> <last-settled-task-uid>".
	acpWorkspaceAttachedFromWarmAnnotation = "acp.workspace.orka.ai/attached-from-warm"

	acpPersistentSessionReuseLogMessage    = "ACP persistent session reuse"
	acpPersistentSessionRetainedLogMessage = "ACP persistent session retained"
	// acpPersistentSessionMissPlanReason is the RuntimeSession plan reason of
	// a fresh session that follows a reuse miss.
	acpPersistentSessionMissPlanReason = "persistent-session-miss"

	// acpPersistentSessionGateBudget bounds the pre-submission wait for the
	// provider's epoch acknowledgement on the reuse path.
	acpPersistentSessionGateBudget = 20 * time.Second
	acpPersistentSessionGatePoll   = 200 * time.Millisecond
)

// Bounded refusal reasons (ADR 0059 section 5). They are used verbatim as log
// values and metric labels.
const (
	acpPersistentReasonReused                = "reused"
	acpPersistentReasonKept                  = "kept"
	acpPersistentReasonFlagDisabled          = "flag-disabled"
	acpPersistentReasonIneligibleScope       = "ineligible-scope"
	acpPersistentReasonClassDisabled         = "class-disabled"
	acpPersistentReasonRebaseUnsupported     = "rebase-unsupported"
	acpPersistentReasonNoRecord              = "no-record"
	acpPersistentReasonRecordExpired         = "record-expired"
	acpPersistentReasonEpochChanged          = "controller-epoch-changed"
	acpPersistentReasonClaimLost             = "claim-lost"
	acpPersistentReasonSnapshotMismatch      = "snapshot-mismatch"
	acpPersistentReasonHistoryBoundary       = "history-boundary"
	acpPersistentReasonTranscriptChanged     = "transcript-changed"
	acpPersistentReasonSessionUnavailable    = "session-unavailable"
	acpPersistentReasonSessionDeleting       = "session-deleting"
	acpPersistentReasonPreviousNotSucceeded  = "previous-turn-not-succeeded"
	acpPersistentReasonPreviousParked        = "previous-turn-parked"
	acpPersistentReasonPreviousCancelled     = "previous-turn-cancelled"
	acpPersistentReasonPreviousTimedOut      = "previous-turn-timed-out"
	acpPersistentReasonRebaseFailed          = "rebase-failed"
	acpPersistentReasonPoolReplaced          = "pool-replaced"
	acpPersistentReasonPoolNotAccepting      = "pool-not-accepting"
	acpPersistentReasonRuntimeReplaced       = "runtime-replaced"
	acpPersistentReasonProviderEpochChanged  = "provider-epoch-changed"
	acpPersistentReasonWorkspaceNotWarm      = "workspace-not-warm"
	acpPersistentReasonWorkspaceIncarnation  = "workspace-incarnation-changed"
	acpPersistentReasonHandshakeTimeout      = "handshake-timeout"
	acpPersistentReasonRuntimeSessionMissing = "runtime-session-missing"
	acpPersistentReasonRuntimeSessionNotIdle = "runtime-session-not-quiescent"
	acpPersistentReasonTaskCancelled         = "task-cancelled"
	acpPersistentReasonTaskDeadline          = "task-deadline"
	acpPersistentSessionStageQueue           = "queue"
	acpPersistentSessionStageClaim           = "claim"
	acpPersistentSessionStagePlan            = "plan"
	acpPersistentSessionStageGate            = "gate"
	acpPersistentSessionDecisionKept         = "kept"
	acpPersistentSessionDecisionRetired      = "retired"
	acpPersistentSessionDecisionHit          = "hit"
	acpPersistentSessionDecisionMiss         = "miss"
	acpPersistentSessionRefusedDeleteReason  = "persistent_session_refused"
	// acpSessionTurnAssistantRole is the role of an assistant-result
	// finalization's terminal transcript message.
	acpSessionTurnAssistantRole = "assistant"
)

// acpPersistentSessionReasons is the closed metric label set.
var acpPersistentSessionReasons = map[string]struct{}{
	acpPersistentReasonReused: {}, acpPersistentReasonKept: {}, acpPersistentReasonFlagDisabled: {},
	acpPersistentReasonIneligibleScope: {}, acpPersistentReasonClassDisabled: {}, acpPersistentReasonRebaseUnsupported: {},
	acpPersistentReasonNoRecord: {}, acpPersistentReasonRecordExpired: {}, acpPersistentReasonEpochChanged: {},
	acpPersistentReasonClaimLost: {}, acpPersistentReasonSnapshotMismatch: {}, acpPersistentReasonHistoryBoundary: {},
	acpPersistentReasonTranscriptChanged: {}, acpPersistentReasonSessionUnavailable: {}, acpPersistentReasonSessionDeleting: {},
	acpPersistentReasonPreviousNotSucceeded: {}, acpPersistentReasonPreviousParked: {}, acpPersistentReasonPreviousCancelled: {},
	acpPersistentReasonPreviousTimedOut: {}, acpPersistentReasonRebaseFailed: {}, acpPersistentReasonPoolReplaced: {},
	acpPersistentReasonPoolNotAccepting: {}, acpPersistentReasonRuntimeReplaced: {}, acpPersistentReasonProviderEpochChanged: {},
	acpPersistentReasonWorkspaceNotWarm: {}, acpPersistentReasonWorkspaceIncarnation: {}, acpPersistentReasonHandshakeTimeout: {},
	acpPersistentReasonRuntimeSessionMissing: {}, acpPersistentReasonRuntimeSessionNotIdle: {}, acpPersistentReasonTaskCancelled: {},
	acpPersistentReasonTaskDeadline: {},
}

func boundedACPPersistentSessionReason(reason string) string {
	if _, ok := acpPersistentSessionReasons[reason]; ok {
		return reason
	}
	return acpPersistentReasonIneligibleScope
}

func recordACPPersistentSessionDecision(decision, reason string) {
	metrics.ACPPersistentSessionTotal.WithLabelValues(decision, boundedACPPersistentSessionReason(reason)).Inc()
}

// logACPPersistentSessionRetained is turn N's keep decision.
func logACPPersistentSessionRetained(ctx context.Context, task *corev1alpha1.Task, kept bool, reason string, generation uint64, rebase time.Duration) {
	result := acpPersistentSessionDecisionRetired
	if kept {
		result = acpPersistentSessionDecisionKept
	}
	reason = boundedACPPersistentSessionReason(reason)
	recordACPPersistentSessionDecision(result, reason)
	logf.FromContext(ctx).Info(acpPersistentSessionRetainedLogMessage,
		"namespace", task.Namespace, "task", task.Name, "result", result, "reason", reason,
		"runtimeSessionGeneration", generation, "rebaseMs", rebase.Milliseconds())
}

// acpPersistentSessionReuse is turn N+1's reuse decision, logged once per
// decision of an execution attempt: pre-submission requeues of the same
// attempt that reach the same decision are not logged or counted again.
type acpPersistentSessionReuse struct {
	dispatcher *ACPDispatcher
	attempted  bool
	logged     bool
	hit        bool
	reason     string
	stage      string
	keptAt     time.Time
	gateWait   time.Duration
	generation uint64
}

func (r *acpPersistentSessionReuse) miss(stage, reason string) {
	if r == nil || r.logged {
		return
	}
	r.hit = false
	r.stage = stage
	r.reason = boundedACPPersistentSessionReason(reason)
}

func (r *acpPersistentSessionReuse) log(ctx context.Context, task *corev1alpha1.Task) {
	if r == nil || !r.attempted || r.logged {
		return
	}
	r.logged = true
	result := acpPersistentSessionDecisionMiss
	reason := r.reason
	if r.hit {
		result, reason = acpPersistentSessionDecisionHit, acpPersistentReasonReused
	}
	reason = boundedACPPersistentSessionReason(reason)
	if !r.dispatcher.notePersistentReuseDecision(task, result+"/"+reason+"/"+r.stage) {
		return
	}
	recordACPPersistentSessionDecision(result, reason)
	var handoffAge time.Duration
	if !r.keptAt.IsZero() {
		handoffAge = time.Since(r.keptAt)
	}
	logf.FromContext(ctx).Info(acpPersistentSessionReuseLogMessage,
		"namespace", task.Namespace, "task", task.Name, "result", result, "reason", reason, "stage", r.stage,
		"handoffAgeMs", handoffAge.Milliseconds(), "gateWaitMs", r.gateWait.Milliseconds(),
		"runtimeSessionGeneration", r.generation)
}

// acpPersistentReuseAttemptKey names one execution attempt of one Task.
type acpPersistentReuseAttemptKey struct {
	taskUID types.UID
	attempt int32
}

// acpPersistentReuseDecisionLimit bounds the remembered decisions of attempts
// still requeueing; past it the memory is reset, which at worst logs one
// decision again.
const acpPersistentReuseDecisionLimit = 4096

func persistentReuseAttemptKey(task *corev1alpha1.Task) (acpPersistentReuseAttemptKey, bool) {
	if task == nil || task.UID == "" || task.Status.Execution == nil {
		return acpPersistentReuseAttemptKey{}, false
	}
	return acpPersistentReuseAttemptKey{taskUID: task.UID, attempt: task.Status.Execution.Attempt}, true
}

// notePersistentReuseDecision records an attempt's decision and reports
// whether it is new for that attempt.
func (d *ACPDispatcher) notePersistentReuseDecision(task *corev1alpha1.Task, decision string) bool {
	key, ok := persistentReuseAttemptKey(task)
	if d == nil || !ok {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.persistentReuseDecisions[key] == decision {
		return false
	}
	if d.persistentReuseDecisions == nil || len(d.persistentReuseDecisions) >= acpPersistentReuseDecisionLimit {
		d.persistentReuseDecisions = make(map[acpPersistentReuseAttemptKey]string)
	}
	d.persistentReuseDecisions[key] = decision
	return true
}

func (d *ACPDispatcher) forgetPersistentReuseDecision(task *corev1alpha1.Task) {
	key, ok := persistentReuseAttemptKey(task)
	if d == nil || !ok {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.persistentReuseDecisions, key)
}

// acpPersistentSessionStaticScope is the configuration part of ADR 0059
// section 1 that needs neither runtime capabilities nor live state. It is
// shared by the queue and the dispatcher.
func acpPersistentSessionStaticScope(enabled bool, task *corev1alpha1.Task, plan ACPRuntimePlan, gatewayGrant bool) string {
	if !enabled {
		return acpPersistentReasonFlagDisabled
	}
	if task == nil || task.Spec.SessionRef == nil || !task.Spec.SessionRef.Append {
		return acpPersistentReasonIneligibleScope
	}
	workspace := plan.Workspace
	if workspace == nil || workspace.Class == nil || workspace.Class.ControllerName == "" ||
		workspace.ReusePolicy != corev1alpha1.WorkspaceReusePolicySession {
		return acpPersistentReasonIneligibleScope
	}
	if workspace.Class.PersistentSessionDisabled {
		return acpPersistentReasonClassDisabled
	}
	ref := task.Spec.SessionRef
	if gatewayGrant || strings.HasPrefix(strings.TrimSpace(ref.ThroughMessageID), "gateway:") {
		return acpPersistentReasonIneligibleScope
	}
	if task.Spec.Transaction != nil {
		return acpPersistentReasonIneligibleScope
	}
	if binding := task.Status.AgentExecutionBinding; binding != nil && binding.Agent != nil && binding.Agent.Identity != nil {
		return acpPersistentReasonIneligibleScope
	}
	if operatoraccess.Tainted(task.Annotations) {
		return acpPersistentReasonIneligibleScope
	}
	return ""
}

// acpPersistentSessionScope is the full section 1 scope at dispatch.
func (d *ACPDispatcher) acpPersistentSessionScope(
	task *corev1alpha1.Task,
	bound *verifiedAgentExecution,
	target acpDispatchTarget,
	nativeSessionsSupported, rebaseSupported bool,
) string {
	if bound == nil {
		return acpPersistentReasonIneligibleScope
	}
	if reason := acpPersistentSessionStaticScope(d.PersistentSessionEnabled, task, bound.plan, bound.body.GatewayGrant != nil); reason != "" {
		return reason
	}
	if target.external != nil || target.pool == nil || target.pool.Spec.ExecutionWorkspace == nil {
		return acpPersistentReasonIneligibleScope
	}
	if nativeSessionsSupported {
		// Codex native sessions keep their capture-and-retire path (decision 3).
		return acpPersistentReasonIneligibleScope
	}
	if support, ok := d.Store.(store.SessionResidentRuntimeSupport); !ok || !support.SupportsSessionResidentRuntime() {
		return acpPersistentReasonIneligibleScope
	}
	if !rebaseSupported {
		return acpPersistentReasonRebaseUnsupported
	}
	return ""
}

// acpPersistentSessionReuseInput is every frozen snapshot input that the
// RuntimeSession create request is built from, except per-turn metadata,
// authorizations, expiry and native restore. Any difference means a fresh
// session (ADR 0059 "the snapshot test").
type acpPersistentSessionReuseInput struct {
	ProfileDigest      string                                  `json:"profileDigest"`
	RuntimeImage       string                                  `json:"runtimeImage"`
	RuntimeProfile     harnessv2.RuntimeProfile                `json:"runtimeProfile"`
	AgentConfiguration harnessv2.AgentSessionConfiguration     `json:"agentConfiguration"`
	MCPDigest          string                                  `json:"mcpDigest"`
	WorkspaceIntent    string                                  `json:"workspaceIntent"`
	Workspace          *corev1alpha1.WorkspaceConfig           `json:"workspace,omitempty"`
	CredentialBindings []store.PromptCredentialBinding         `json:"credentialBindings,omitempty"`
	ExecutionWorkspace *agentExecutionSnapshotWorkspaceBinding `json:"executionWorkspace,omitempty"`
	Connections        []agentExecutionSnapshotConnection      `json:"connections,omitempty"`
	GatewayGrant       bool                                    `json:"gatewayGrant,omitempty"`
	AgentUID           string                                  `json:"agentUID"`
	AgentGeneration    int64                                   `json:"agentGeneration"`
	RuntimeOverride    *corev1alpha1.AgentRuntimeSpec          `json:"runtimeOverride,omitempty"`
	DefaultTools       *agentExecutionSnapshotToolPolicy       `json:"defaultTools,omitempty"`
}

func acpPersistentSessionReuseInputFor(
	bound *verifiedAgentExecution,
	task *corev1alpha1.Task,
	mcpDigest string,
	attempt *store.PromptAttempt,
) acpPersistentSessionReuseInput {
	input := acpPersistentSessionReuseInput{
		ProfileDigest: bound.body.ProfileDigest, RuntimeImage: bound.body.RuntimeImage,
		RuntimeProfile: bound.plan.Profile, AgentConfiguration: bound.configuration, MCPDigest: mcpDigest,
		WorkspaceIntent: string(effectiveACPWorkspaceIntent(task)), Workspace: bound.body.Workspace,
		ExecutionWorkspace: bound.body.ExecutionWorkspace, Connections: bound.body.Connections,
		GatewayGrant: bound.body.GatewayGrant != nil, AgentUID: bound.body.Agent.UID,
		AgentGeneration: bound.body.Agent.Generation, RuntimeOverride: bound.body.RuntimeOverride,
		DefaultTools: bound.body.DefaultTools,
	}
	if attempt != nil {
		input.CredentialBindings = append([]store.PromptCredentialBinding(nil), attempt.CredentialBindings...)
	}
	return input
}

func acpPersistentSessionReuseDigest(input acpPersistentSessionReuseInput) (string, error) {
	return acpDomainDigest("acp-resident-runtime-reuse/v1", input)
}

// acpTranscriptHeadMessage is one message a turn appended, by role and
// content.
type acpTranscriptHeadMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// acpPersistentSessionTranscriptHeadDigest identifies the end of the
// transcript one turn left: the stable ID of the message before the turn
// ("" for an empty transcript) and exactly the messages the turn appended.
// The ID is unique, so a later exchange that repeats the same reply never
// matches.
func acpPersistentSessionTranscriptHeadDigest(precedingMessageID string, appended []acpTranscriptHeadMessage) string {
	digest, err := acpDomainDigest("acp-resident-runtime-transcript-head/v2", map[string]any{
		"precedingMessageID": strings.TrimSpace(precedingMessageID), "appended": appended,
	})
	if err != nil {
		return ""
	}
	return digest
}

// acpTranscriptHeadMatches reports whether the transcript still ends exactly
// where turn N left it. Turn N appended one or two messages.
func (d *ACPDispatcher) acpTranscriptHeadMatches(ctx context.Context, namespace, name, want string) (string, bool, error) {
	for appended := 1; appended <= 2; appended++ {
		tail, err := d.Sessions.transcripts.LoadTranscript(ctx, namespace, name, appended+1)
		if err != nil {
			return "", false, fmt.Errorf("load ACP transcript head for persistent session reuse: %w", err)
		}
		if len(tail) < appended {
			continue
		}
		preceding := ""
		if len(tail) == appended+1 {
			preceding, tail = tail[0].ID, tail[1:]
		} else if len(tail) != appended {
			continue
		}
		messages := make([]acpTranscriptHeadMessage, 0, len(tail))
		for _, message := range tail {
			messages = append(messages, acpTranscriptHeadMessage{Role: message.Role, Content: message.Content})
		}
		if acpPersistentSessionTranscriptHeadDigest(preceding, messages) == want {
			return tail[len(tail)-1].ID, true, nil
		}
	}
	return "", false, nil
}

// acpRuntimePoolServedRecordDigest digests the pool's served-record
// annotation: allocation, instance, request revision, Pod and boot. Empty
// means the pool has no served record.
func acpRuntimePoolServedRecordDigest(pool *corev1alpha1.RuntimePool) string {
	if pool == nil {
		return ""
	}
	raw := strings.TrimSpace(pool.Annotations[externalRuntimeServedAnnotation])
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("orka.acp.runtime-served-record/v1\x00" + raw))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// acpRuntimeSessionBindingFromResidentRuntime rebuilds the live binding from
// a claimed record; the in-memory binding is only a cache of it.
func acpRuntimeSessionBindingFromResidentRuntime(record *store.SessionResidentRuntime) ACPRuntimeSessionBinding {
	return ACPRuntimeSessionBinding{
		SessionUID: record.RuntimeSessionUID, Generation: uint64(record.RuntimeSessionGeneration),
		ProfileDigest:     harnessv2.ProfileDigest(record.ProfileDigest),
		RuntimeInstanceID: harnessv2.RuntimeInstanceID(record.RuntimeInstanceID),
		SupervisorBootID:  harnessv2.SupervisorBootID(record.SupervisorBootID),
		WorkspaceDigest:   record.WorkspaceDigest, MCPDigest: record.MCPDigest,
		PromptDelivered: record.PromptDelivered,
	}
}

// acpPersistentSessionClaimInput is what the dispatcher knows about turn N+1
// before acquiring its lease.
type acpPersistentSessionClaimInput struct {
	// forceFresh plans a fresh RuntimeSession without consulting the record
	// or the in-memory binding: the class opted out, or this Task's reuse was
	// already refused at the gate.
	forceFresh    bool
	handoffID     string
	fence         store.ControllerEpochFence
	runtimeFence  harnessv2.Fence
	mcpDigest     string
	reuseDigest   string
	lineageDigest string
	pool          *corev1alpha1.RuntimePool
	now           time.Time
}

// acpResidentRuntimeStaticRefusal checks a record against turn N+1's frozen
// identity before the claim. "" means the claim may be attempted.
func acpResidentRuntimeStaticRefusal(record *store.SessionResidentRuntime, input acpPersistentSessionClaimInput) (string, string) {
	if record == nil || input.handoffID == "" || record.HandoffID != input.handoffID {
		return acpPersistentSessionStageClaim, acpPersistentReasonNoRecord
	}
	if record.State != store.SessionResidentRuntimeResident {
		return acpPersistentSessionStageClaim, acpPersistentReasonClaimLost
	}
	if store.ValidateSessionResidentRuntime(*record) != nil {
		return acpPersistentSessionStageClaim, acpPersistentReasonNoRecord
	}
	if record.ControllerEpochName != input.fence.Name || record.ControllerEpoch != input.fence.Epoch {
		return acpPersistentSessionStageClaim, acpPersistentReasonEpochChanged
	}
	if !input.now.Before(record.ExpiresAt) {
		return acpPersistentSessionStageClaim, acpPersistentReasonRecordExpired
	}
	if input.pool == nil || string(input.pool.UID) != record.RuntimePoolUID || input.pool.Name != record.RuntimePoolName ||
		input.pool.Generation != record.RuntimePoolGeneration {
		return acpPersistentSessionStagePlan, acpPersistentReasonPoolReplaced
	}
	if string(input.runtimeFence.RuntimeInstanceID) != record.RuntimeInstanceID ||
		string(input.runtimeFence.SupervisorBootID) != record.SupervisorBootID {
		return acpPersistentSessionStagePlan, acpPersistentReasonRuntimeReplaced
	}
	if string(input.runtimeFence.RuntimeProfileDigest) != record.ProfileDigest || input.mcpDigest != record.MCPDigest ||
		input.reuseDigest != record.ReuseDigest || input.lineageDigest != record.LineageConfigDigest {
		return acpPersistentSessionStagePlan, acpPersistentReasonSnapshotMismatch
	}
	return "", ""
}

// acpResidentRuntimeHistoryRefusal refuses reuse unless a fresh session
// would have bootstrapped the whole history through turn N.
// It also returns the last canonical message ID the reused process holds.
func (d *ACPDispatcher) acpResidentRuntimeHistoryRefusal(
	ctx context.Context, task *corev1alpha1.Task, control *store.SessionControl, record *store.SessionResidentRuntime,
) (string, string, error) {
	ref := task.Spec.SessionRef
	if ref == nil || ref.PromptIncluded {
		return acpPersistentReasonHistoryBoundary, "", nil
	}
	if d.Sessions == nil || d.Sessions.transcripts == nil {
		return acpPersistentReasonHistoryBoundary, "", nil
	}
	if control.Availability != store.SessionAvailable {
		return acpPersistentReasonSessionUnavailable, "", nil
	}
	lastID, matches, err := d.acpTranscriptHeadMatches(ctx, control.Namespace, control.SessionName, record.TranscriptHeadDigest)
	if err != nil {
		return "", "", err
	}
	if !matches || strings.TrimSpace(lastID) == "" {
		return acpPersistentReasonTranscriptChanged, "", nil
	}
	if through := strings.TrimSpace(ref.ThroughMessageID); through != "" && through != lastID {
		return acpPersistentReasonHistoryBoundary, "", nil
	}
	if ref.MaxMessages > 0 {
		bounded, err := d.Sessions.transcripts.LoadTranscript(ctx, control.Namespace, control.SessionName, int(ref.MaxMessages)+1)
		if err != nil {
			return "", "", fmt.Errorf("load bounded ACP transcript for persistent session reuse: %w", err)
		}
		if len(bounded) > int(ref.MaxMessages) {
			return acpPersistentReasonHistoryBoundary, "", nil
		}
	}
	return "", lastID, nil
}

// acpPersistentSessionObservedTranscriptHead is the last canonical message ID
// turn N's provider process received: the claimed head on a reuse, or the
// last message of the bootstrap it was created from.
func acpPersistentSessionObservedTranscriptHead(session *acpTaskSession) (string, bool) {
	if session == nil {
		return "", false
	}
	if session.Reused {
		if session.ResidentClaimed == nil || strings.TrimSpace(session.ClaimedTranscriptHeadID) == "" {
			return "", false
		}
		return session.ClaimedTranscriptHeadID, true
	}
	if session.Bootstrap == nil {
		return "", false
	}
	return session.Bootstrap.LastMessageID, true
}

// acpPersistentSessionKeepInput is everything turn N's settlement knows when
// it decides whether to keep its live RuntimeSession.
type acpPersistentSessionKeepInput struct {
	task            *corev1alpha1.Task
	bound           *verifiedAgentExecution
	target          acpDispatchTarget
	runtimeClient   *harnessv2.Client
	runtimeFence    harnessv2.Fence
	fence           store.ControllerEpochFence
	session         *acpTaskSession
	attemptID       string
	attempt         *store.PromptAttempt
	mcpDigest       string
	lineageDigest   string
	workspaceDigest string
	baseline        harnessv2.WorkspaceBaseline
	intent          harnessv2.WorkspaceIntent
	delta           harnessv2.WorkspaceDeltaDescriptor
	publicationID   string
	finalization    *runtimeSessionPublicationFinalization
	resultText      string
	scopeRefusal    string
	parked          bool
	rebaseSupported bool
	nativeCapture   bool
	transcriptSkip  bool
	// historyIncomplete reports a process that may not hold the whole
	// canonical transcript (a partial bootstrap), which a later turn asking
	// for more history must never reuse.
	historyIncomplete bool
	publicationState  store.PublicationState
}

// acpPersistentSessionProcessHoldsFullHistory reports whether the turn's
// provider process holds the whole canonical transcript: it was reused from a
// claimed record (whose keep already proved it), or it was created this turn
// from a bootstrap that omitted nothing.
func acpPersistentSessionProcessHoldsFullHistory(task *corev1alpha1.Task, session *acpTaskSession) bool {
	if session == nil {
		return false
	}
	if session.Reused {
		return session.ResidentClaimed != nil
	}
	bootstrap := session.Bootstrap
	ref := task.Spec.SessionRef
	if bootstrap == nil || ref == nil || strings.TrimSpace(ref.ThroughMessageID) != "" || ref.PromptIncluded {
		return false
	}
	// TotalMessages is how many messages the bounded load returned; reaching
	// the limit may have omitted older ones.
	return !bootstrap.Truncated && int(bootstrap.MessageCount) == bootstrap.TotalMessages &&
		bootstrap.TotalMessages < acpSessionReferenceMaxMessages(ref)
}

// prepareACPPersistentSessionKeep decides whether turn N keeps its live
// RuntimeSession and, for a verified-exact write turn, rebases it. It returns
// the record to keep in the finalization write, or the bounded reason it
// retires the session as today. It never fails the turn: every error is a
// retirement reason.
func (d *ACPDispatcher) prepareACPPersistentSessionKeep(
	ctx context.Context,
	input acpPersistentSessionKeepInput,
) (*store.SessionResidentRuntime, string, time.Duration) {
	rebaseNeeded, reason := acpPersistentSessionKeepEligibility(input)
	if reason != "" {
		return nil, reason, 0
	}
	session := input.session
	attempt := input.attempt
	reader := uncachedReader(d.APIReader, d.Client)
	task := input.task
	workspaceName := strings.TrimSpace(task.Labels[acpExecutionWorkspaceLinkLabel])
	workspaceUID := strings.TrimSpace(task.Annotations[acpExecutionWorkspaceUIDAnnotation])
	attachmentEpoch := acpTaskRecordedAttachmentEpoch(task)
	if workspaceName == "" || workspaceUID == "" || attachmentEpoch < 1 || input.target.pool == nil {
		return nil, acpPersistentReasonIneligibleScope, 0
	}
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: workspaceName}, workspace); err != nil ||
		string(workspace.UID) != workspaceUID {
		return nil, acpPersistentReasonWorkspaceIncarnation, 0
	}
	window := acpWorkspaceWarmDetachWindow(workspace, task)
	if window <= 0 {
		return nil, acpPersistentReasonWorkspaceNotWarm, 0
	}
	pool := &corev1alpha1.RuntimePool{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(input.target.pool), pool); err != nil || pool.UID != input.target.pool.UID {
		return nil, acpPersistentReasonPoolReplaced, 0
	}
	if refusal := acpRuntimePoolResidentRefusal(pool, string(input.runtimeFence.RuntimeInstanceID), string(input.runtimeFence.SupervisorBootID), input.fence); refusal != "" {
		return nil, refusal, 0
	}
	served := acpRuntimePoolServedRecordDigest(pool)
	if served == "" {
		return nil, acpPersistentReasonProviderEpochChanged, 0
	}
	reuseDigest, err := acpPersistentSessionReuseDigest(acpPersistentSessionReuseInputFor(input.bound, task, input.mcpDigest, attempt))
	if err != nil {
		return nil, acpPersistentReasonSnapshotMismatch, 0
	}

	sessionID := harnessv2.RuntimeSessionID(runtimeSessionID(input.runtimeFence))
	workspaceDigest := input.workspaceDigest
	verifiedBaseline := session.VerifiedBaseline
	rebasedTree := ""
	if claimed := session.ResidentClaimed; !rebaseNeeded && claimed != nil && claimed.RebasedTreeDigest != "" &&
		claimed.RebasedTreeDigest == input.baseline.TreeDigest && claimed.WorkspaceDigest == workspaceDigest {
		// A reused rebased session that published nothing keeps its derived
		// baseline identity, so the next turn can still reuse it.
		rebasedTree = claimed.RebasedTreeDigest
		if verifiedBaseline == nil {
			verifiedBaseline = claimed.VerifiedBaseline
		}
	}
	var rebaseDuration time.Duration
	if rebaseNeeded {
		started := time.Now()
		next, rebased, digest, err := d.rebaseACPRuntimeSession(ctx, input, sessionID)
		rebaseDuration = time.Since(started)
		if err != nil {
			logf.FromContext(ctx).Info("ACP persistent session rebase failed; retiring the RuntimeSession",
				"namespace", task.Namespace, "task", task.Name, "diagnostic", boundedRuntimeSessionServerMessage(err))
			return nil, acpPersistentReasonRebaseFailed, rebaseDuration
		}
		verifiedBaseline, rebasedTree, workspaceDigest = next, rebased, digest
	}
	_, observed, err := runtimeSessionStatusForUID(ctx, input.runtimeClient, input.runtimeFence.RuntimeSessionUID)
	if err != nil || observed == nil {
		return nil, acpPersistentReasonRuntimeSessionMissing, rebaseDuration
	}
	if observed.RuntimeSessionID != sessionID || observed.Generation != input.runtimeFence.RuntimeSessionGeneration ||
		observed.State != harnessv2.RuntimeSessionStateIdle || !observed.Quiescent || observed.ActivePromptID != "" ||
		observed.PendingPermissionCount != 0 {
		return nil, acpPersistentReasonRuntimeSessionNotIdle, rebaseDuration
	}
	head, headErr := d.acpPersistentSessionKeepTranscriptHead(ctx, input)
	if headErr != nil || head == "" {
		return nil, acpPersistentReasonHistoryBoundary, rebaseDuration
	}
	keptAt := time.Now().UTC()
	record, err := store.NewSessionResidentRuntime(store.SessionResidentRuntime{
		SourceTaskUID: string(task.UID), SourceAttempt: int64(task.Status.Execution.Attempt),
		SourcePromptAttemptID: input.attemptID, SourceTurnID: session.Turn.Turn.ID,
		SourceLeaseGeneration: session.Turn.Turn.Key.LeaseGeneration,
		ControllerEpochName:   input.fence.Name, ControllerEpoch: input.fence.Epoch,
		WorkspaceName: workspaceName, WorkspaceUID: workspaceUID, WorkspaceAttachmentEpoch: attachmentEpoch,
		RuntimePoolName: pool.Name, RuntimePoolUID: string(pool.UID), RuntimePoolGeneration: pool.Generation,
		RuntimeInstanceID: string(input.runtimeFence.RuntimeInstanceID), SupervisorBootID: string(input.runtimeFence.SupervisorBootID),
		ServedRecordDigest: served,
		RuntimeSessionUID:  string(input.runtimeFence.RuntimeSessionUID), RuntimeSessionGeneration: int64(input.runtimeFence.RuntimeSessionGeneration),
		ProfileDigest: string(input.runtimeFence.RuntimeProfileDigest), MCPDigest: input.mcpDigest,
		WorkspaceDigest: workspaceDigest, LineageConfigDigest: input.lineageDigest,
		ReuseDigest:          reuseDigest,
		TranscriptHeadDigest: head,
		RebasedTreeDigest:    rebasedTree, VerifiedBaseline: verifiedBaseline, PromptDelivered: true,
		KeptAt: keptAt, ExpiresAt: keptAt.Add(window),
	})
	if err != nil {
		return nil, acpPersistentReasonIneligibleScope, rebaseDuration
	}
	return &record, "", rebaseDuration
}

// acpPersistentSessionKeepEligibility is the part of turn N's keep decision
// that needs no live state: scope, a settled succeeded prompt, and a
// delivery state the session can cross a turn boundary from. It reports
// whether a write turn must rebase first.
func acpPersistentSessionKeepEligibility(input acpPersistentSessionKeepInput) (bool, string) {
	if input.scopeRefusal != "" {
		return false, input.scopeRefusal
	}
	if input.session == nil || input.session.Turn == nil || input.nativeCapture {
		return false, acpPersistentReasonIneligibleScope
	}
	if input.parked {
		return false, acpPersistentReasonPreviousParked
	}
	if input.transcriptSkip || input.historyIncomplete {
		return false, acpPersistentReasonHistoryBoundary
	}
	attempt := input.attempt
	if attempt == nil || attempt.ExecutionState != store.PromptExecutionSucceeded {
		return false, acpPersistentReasonPreviousNotSucceeded
	}
	switch input.intent {
	case harnessv2.WorkspaceIntentRead:
		if input.delta.State != harnessv2.WorkspaceDeltaNoChange || attempt.DeliveryState != store.PromptDeliveryReadValidated {
			return false, acpPersistentReasonPreviousNotSucceeded
		}
		return false, ""
	case harnessv2.WorkspaceIntentWrite:
		switch {
		case input.delta.State == harnessv2.WorkspaceDeltaNoChange && attempt.DeliveryState == store.PromptDeliveryNoChange:
			return false, ""
		case input.delta.State == harnessv2.WorkspaceDeltaPrepared && attempt.DeliveryState == store.PromptDeliveryVerifiedExact &&
			input.publicationState == store.PublicationVerifiedExact && input.finalization != nil && input.publicationID != "":
			return true, ""
		default:
			return false, acpPersistentReasonPreviousNotSucceeded
		}
	default:
		return false, acpPersistentReasonIneligibleScope
	}
}

// acpPersistentSessionKeepTranscriptHead reads the message before turn N's
// appends, just before the finalization that appends them, and digests the
// head that finalization will leave. A concurrent out-of-band append lands
// between them, so the next turn then refuses with transcript-changed.
func (d *ACPDispatcher) acpPersistentSessionKeepTranscriptHead(ctx context.Context, input acpPersistentSessionKeepInput) (string, error) {
	ref := input.task.Spec.SessionRef
	if d.Sessions == nil || d.Sessions.transcripts == nil || ref == nil || input.session == nil || input.session.Turn == nil {
		return "", errors.New("transcript store is unavailable")
	}
	last, err := d.Sessions.transcripts.LoadTranscript(ctx, input.task.Namespace, strings.TrimSpace(ref.Name), 1)
	if err != nil {
		return "", err
	}
	preceding := ""
	if len(last) == 1 {
		preceding = last[0].ID
		if strings.TrimSpace(preceding) == "" {
			return "", errors.New("the transcript head has no stable message ID")
		}
	}
	// Anything appended since the process received its history (out of band
	// during this turn) is a message the process never saw.
	if observed, ok := acpPersistentSessionObservedTranscriptHead(input.session); !ok || observed != preceding {
		return "", errors.New("the transcript changed after the process received its history")
	}
	appended := make([]acpTranscriptHeadMessage, 0, 2)
	if !input.session.Turn.SkipUserPromptAppend {
		appended = append(appended, acpTranscriptHeadMessage{Role: acpBootstrapRoleUser, Content: input.session.Turn.Turn.UserPrompt})
	}
	appended = append(appended, acpTranscriptHeadMessage{Role: acpSessionTurnAssistantRole, Content: input.resultText})
	return acpPersistentSessionTranscriptHeadDigest(preceding, appended), nil
}

// acpRuntimePoolResidentRefusal checks the pool state that a resident runtime
// depends on: Serving and admitting its own resident session
// (acpRuntimePoolServesItsResident), the exact active instance and boot, the
// current controller epoch, and no requested retirement or eviction.
func acpRuntimePoolResidentRefusal(pool *corev1alpha1.RuntimePool, instanceID, bootID string, fence store.ControllerEpochFence) string {
	if pool == nil || !pool.DeletionTimestamp.IsZero() {
		return acpPersistentReasonPoolReplaced
	}
	if !acpRuntimePoolServesItsResident(pool) {
		return acpPersistentReasonPoolNotAccepting
	}
	active := pool.Status.ActiveInstance
	if active == nil || active.RuntimeInstanceID != instanceID || active.BootID != bootID {
		return acpPersistentReasonRuntimeReplaced
	}
	if active.ControllerEpoch != fence.Epoch {
		return acpPersistentReasonEpochChanged
	}
	if pool.Annotations[externalRuntimeRetirementRequestedAnnotation] == booleanTrueValue ||
		strings.TrimSpace(pool.Annotations[externalRuntimeEvictionAnnotation]) != "" ||
		runtimePoolWorkspaceInstanceRetiring(pool) {
		return acpPersistentReasonWorkspaceNotWarm
	}
	return ""
}

// acpRuntimePoolServesItsResident reports whether a Serving pool may keep, or
// let the next turn reuse, the RuntimeSession it already holds. Accepting
// always qualifies. Closed qualifies only when the pool's one reason for
// closing is its configured capacity, on the dedicated single-session pool of
// a class-backed workspace: there the Session's own resident session is what
// fills the only resident slot, from turn N's creation until turn N+1 reuses
// it. That closure stops new RuntimeSessions, which reuse never creates.
// Draining, Degraded, Starting, Stopping, Stopped, a closure for any other
// reason, a status the pool controller has not observed for the current
// generation, or more than one resident session all refuse.
func acpRuntimePoolServesItsResident(pool *corev1alpha1.RuntimePool) bool {
	if pool == nil || pool.Status.Lifecycle != corev1alpha1.RuntimePoolLifecycleServing {
		return false
	}
	switch pool.Status.AdmissionState {
	case corev1alpha1.RuntimePoolAdmissionAccepting:
		return true
	case corev1alpha1.RuntimePoolAdmissionClosed:
	default:
		return false
	}
	if pool.Spec.ExecutionWorkspace == nil || pool.Status.ObservedGeneration != pool.Generation {
		return false
	}
	admission := meta.FindStatusCondition(pool.Status.Conditions, corev1alpha1.RuntimePoolConditionAdmissionReady)
	if admission == nil || admission.Status != metav1.ConditionFalse ||
		admission.Reason != corev1alpha1.RuntimePoolReasonAtCapacity || admission.ObservedGeneration != pool.Generation {
		return false
	}
	capacity := pool.Status.Capacity
	return capacity.MaxResidentSessions == 1 && capacity.ResidentSessions <= 1 && runtimePoolAtCapacity(capacity)
}

// rebaseACPRuntimeSession folds turn N's exactly published delta into its
// RuntimeSession's trusted baseline (ADR 0059 section 4). It returns the
// Session's next verified baseline, the rebased tree digest, and the
// workspace binding digest turn N+1 must compute for reuse.
func (d *ACPDispatcher) rebaseACPRuntimeSession(
	ctx context.Context,
	input acpPersistentSessionKeepInput,
	sessionID harnessv2.RuntimeSessionID,
) (*store.VerifiedBranchBaseline, string, string, error) {
	if d.Store == nil {
		return nil, "", "", errors.New("publication store is required for rebase")
	}
	publication, err := d.Store.GetPublication(ctx, input.publicationID)
	if err != nil {
		return nil, "", "", err
	}
	if publication.State != store.PublicationVerifiedExact || publication.PreparedReceipt == nil ||
		publication.VerificationReceipt == nil || publication.VerificationReceipt.ObservedRemote.SHA != publication.PreparedReceipt.CommitSHA {
		return nil, "", "", errors.New("publication is not verified exact")
	}
	next := &store.VerifiedBranchBaseline{
		RepositoryID: publication.TargetRepositoryID, Ref: publication.TargetRef, SHA: publication.PreparedReceipt.CommitSHA,
	}
	if err := store.ValidateVerifiedBaseline(*next); err != nil {
		return nil, "", "", err
	}
	manifest := strings.TrimSpace(input.delta.ManifestDigest)
	tree, err := harnessv2.RebasedWorkspaceTreeDigest(input.baseline.TreeDigest, manifest, next.SHA)
	if err != nil {
		return nil, "", "", err
	}
	nextBaseline := harnessv2.WorkspaceBaseline{RepositoryIdentity: next.RepositoryID, Revision: next.SHA, TreeDigest: tree}
	sourceRef, err := runtimeWorkspaceSourceRef(&corev1alpha1.WorkspaceConfig{Ref: next.Ref})
	if err != nil {
		return nil, "", "", err
	}
	bindingDigest, err := acpRuntimeWorkspaceBindingDigest(sourceRef, harnessv2.WorkspaceSpec{
		Intent: input.intent, Baseline: nextBaseline, RelativeRoot: acpWorkspaceRelativeRoot(input.task),
	})
	if err != nil {
		return nil, "", "", err
	}
	request := harnessv2.RebaseRuntimeSessionRequest{
		Protocol: harnessv2.ProtocolVersion,
		Metadata: mutationMetadata(input.runtimeFence, input.task, "rebase-session-g"+
			strconv.FormatUint(input.runtimeFence.RuntimeSessionGeneration, 10), true, time.Now().UTC().Add(30*time.Second)),
		WorkspaceDeltaID: input.delta.DeltaID, PublicationID: input.publicationID,
		TerminalReceiptDigest: input.finalization.TerminalReceiptDigest, PublishedManifestDigest: manifest,
		NextBaseline: nextBaseline,
	}
	if err := sealMutation(&request.Metadata.RequestDigest, request); err != nil {
		return nil, "", "", err
	}
	rebaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if _, err := input.runtimeClient.RebaseRuntimeSession(rebaseCtx, sessionID, request); err != nil {
		return nil, "", "", err
	}
	return next, tree, bindingDigest, nil
}

func acpWorkspaceRelativeRoot(task *corev1alpha1.Task) string {
	if task == nil || task.Spec.Workspace == nil {
		return ""
	}
	return strings.TrimSpace(task.Spec.Workspace.SubPath)
}

// acpRebasedRuntimeWorkspace replaces turn N+1's freshly prepared baseline
// with the claimed record's rebased identity when the clean-room publisher
// resolved exactly the commit the record was rebased to. The returned digest
// is the one the reused binding must match; any difference recreates.
func acpRebasedRuntimeWorkspace(
	prepared preparedACPRuntimeWorkspace,
	record *store.SessionResidentRuntime,
) (preparedACPRuntimeWorkspace, bool, error) {
	if record == nil || record.RebasedTreeDigest == "" || record.VerifiedBaseline == nil {
		return prepared, false, nil
	}
	if prepared.baseline.Revision != record.VerifiedBaseline.SHA ||
		!sameWorkspaceRepositoryIdentity(prepared.baseline.RepositoryIdentity, record.VerifiedBaseline.RepositoryID) {
		return prepared, false, nil
	}
	baseline := harnessv2.WorkspaceBaseline{
		RepositoryIdentity: record.VerifiedBaseline.RepositoryID, Revision: record.VerifiedBaseline.SHA,
		TreeDigest: record.RebasedTreeDigest,
	}
	sourceRef, err := runtimeWorkspaceSourceRef(&corev1alpha1.WorkspaceConfig{Ref: record.VerifiedBaseline.Ref})
	if err != nil {
		return prepared, false, err
	}
	spec := prepared.spec
	spec.Baseline = baseline
	digest, err := acpRuntimeWorkspaceBindingDigest(sourceRef, spec)
	if err != nil {
		return prepared, false, err
	}
	prepared.baseline = baseline
	prepared.spec = spec
	prepared.bindingDigest = digest
	prepared.authorization = nil
	return prepared, true, nil
}

// acpPersistentSessionHandshakeGate waits, within the pre-submission budget,
// for the full ADR 0057 post-attach handshake that the queue skipped for a
// Task queued against a held attachment. It is read-only.
func (d *ACPDispatcher) acpPersistentSessionHandshakeGate(
	ctx context.Context,
	task *corev1alpha1.Task,
	pool *corev1alpha1.RuntimePool,
) (time.Duration, error) {
	started := time.Now()
	workspaceName := strings.TrimSpace(task.Labels[acpExecutionWorkspaceLinkLabel])
	workspaceUID := strings.TrimSpace(task.Annotations[acpExecutionWorkspaceUIDAnnotation])
	if workspaceName == "" || workspaceUID == "" || pool == nil {
		return 0, errors.New("the held workspace identity is incomplete")
	}
	reader := uncachedReader(d.APIReader, d.Client)
	budget := acpPersistentSessionGateBudget
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < budget {
		budget = time.Until(deadline)
	}
	waitCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	ticker := time.NewTicker(acpPersistentSessionGatePoll)
	defer ticker.Stop()
	var last error
	for {
		workspace := &workspacev1alpha1.ExecutionWorkspace{}
		currentPool := &corev1alpha1.RuntimePool{}
		err := reader.Get(waitCtx, types.NamespacedName{Namespace: task.Namespace, Name: workspaceName}, workspace)
		if err == nil {
			err = reader.Get(waitCtx, client.ObjectKeyFromObject(pool), currentPool)
		}
		switch {
		case err != nil && apierrors.IsNotFound(err):
			return time.Since(started), errors.New("the held workspace or its RuntimePool no longer exists")
		case err == nil && currentPool.UID != pool.UID:
			return time.Since(started), errors.New("the RuntimePool was replaced")
		case err == nil:
			if readiness := acpWorkspacePoolReadinessFailure(workspace, currentPool, workspaceUID, string(task.UID), time.Now()); readiness == nil {
				return time.Since(started), nil
			} else {
				last = readiness
			}
		default:
			last = err
		}
		select {
		case <-waitCtx.Done():
			return time.Since(started), fmt.Errorf("%w: %v", errACPPersistentSessionHandshakeTimeout, last)
		case <-ticker.C:
		}
	}
}

var errACPPersistentSessionHandshakeTimeout = errors.New("the attach handshake did not complete within the pre-submission budget")

// acpPersistentSessionRuntimeGate is the gate before first runtime contact for
// a claimed record: pool, runtime, workspace evidence and the exact quiescent
// RuntimeSession status. "" means reuse may proceed.
func (d *ACPDispatcher) acpPersistentSessionRuntimeGate(
	ctx context.Context,
	task *corev1alpha1.Task,
	record *store.SessionResidentRuntime,
	runtimeClient *harnessv2.Client,
	runtimeFence harnessv2.Fence,
	fence store.ControllerEpochFence,
	pool *corev1alpha1.RuntimePool,
) (string, error) {
	reader := uncachedReader(d.APIReader, d.Client)
	currentPool := &corev1alpha1.RuntimePool{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(pool), currentPool); err != nil {
		if apierrors.IsNotFound(err) {
			return acpPersistentReasonPoolReplaced, nil
		}
		return "", err
	}
	if string(currentPool.UID) != record.RuntimePoolUID || currentPool.Generation != record.RuntimePoolGeneration {
		return acpPersistentReasonPoolReplaced, nil
	}
	if refusal := acpRuntimePoolResidentRefusal(currentPool, record.RuntimeInstanceID, record.SupervisorBootID, fence); refusal != "" {
		return refusal, nil
	}
	if acpRuntimePoolServedRecordDigest(currentPool) != record.ServedRecordDigest {
		return acpPersistentReasonProviderEpochChanged, nil
	}
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: record.WorkspaceName}, workspace); err != nil {
		if apierrors.IsNotFound(err) {
			return acpPersistentReasonWorkspaceIncarnation, nil
		}
		return "", err
	}
	if refusal := acpPersistentSessionWorkspaceRefusal(workspace, task, record); refusal != "" {
		return refusal, nil
	}
	expected, ok := acpPersistentSessionClaimedFence(record, runtimeFence)
	if !ok {
		return acpPersistentReasonRuntimeReplaced, nil
	}
	status, observed, err := runtimeSessionStatusForUID(ctx, runtimeClient, expected.RuntimeSessionUID)
	if err != nil {
		return acpPersistentReasonRuntimeSessionMissing, nil
	}
	if harnessv2.CompareFence(runtimePoolFenceWithSession(expected, "", 0), status.Fence, false) != harnessv2.FenceMatch {
		return acpPersistentReasonRuntimeReplaced, nil
	}
	if observed == nil || observed.RuntimeSessionUID != expected.RuntimeSessionUID ||
		observed.Generation != expected.RuntimeSessionGeneration ||
		observed.RuntimeSessionID != harnessv2.RuntimeSessionID(runtimeSessionID(expected)) {
		return acpPersistentReasonRuntimeSessionMissing, nil
	}
	if observed.State != harnessv2.RuntimeSessionStateIdle || !observed.Quiescent || observed.ActivePromptID != "" ||
		observed.PendingPermissionCount != 0 || observed.ReservedForFinalization {
		return acpPersistentReasonRuntimeSessionNotIdle, nil
	}
	return "", nil
}

// acpPersistentSessionClaimedFence is the exact identity a claimed record
// names: its RuntimeSession UID and generation on the runtime instance and
// supervisor boot this turn reached. The dispatcher's runtime fence carries no
// RuntimeSession until after the gate, so the gate must not derive the
// expected session from it. It reports false when the record names another
// instance or boot, or no valid session.
func acpPersistentSessionClaimedFence(record *store.SessionResidentRuntime, runtimeFence harnessv2.Fence) (harnessv2.Fence, bool) {
	if record == nil || strings.TrimSpace(record.RuntimeSessionUID) == "" || record.RuntimeSessionGeneration < 1 ||
		strings.TrimSpace(record.RuntimeInstanceID) == "" || strings.TrimSpace(record.SupervisorBootID) == "" ||
		record.RuntimeInstanceID != string(runtimeFence.RuntimeInstanceID) ||
		record.SupervisorBootID != string(runtimeFence.SupervisorBootID) {
		return harnessv2.Fence{}, false
	}
	return runtimePoolFenceWithSession(
		runtimeFence, harnessv2.RuntimeSessionUID(record.RuntimeSessionUID), uint64(record.RuntimeSessionGeneration),
	), true
}

// acpPersistentSessionWorkspaceRefusal checks the warm-window evidence the
// attach patch recorded and the workspace's warm state.
func acpPersistentSessionWorkspaceRefusal(
	workspace *workspacev1alpha1.ExecutionWorkspace,
	task *corev1alpha1.Task,
	record *store.SessionResidentRuntime,
) string {
	if string(workspace.UID) != record.WorkspaceUID || !workspace.DeletionTimestamp.IsZero() {
		return acpPersistentReasonWorkspaceIncarnation
	}
	if workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady ||
		workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateFailed ||
		acpWorkspaceQuarantined(workspace) || operatoraccess.Tainted(workspace.Annotations) ||
		strings.TrimSpace(workspace.Annotations[acpWorkspaceWarmWindowDrainedAnnotation]) != "" ||
		strings.TrimSpace(workspace.Annotations[acpWorkspaceSuspendPendingAnnotation]) != "" {
		return acpPersistentReasonWorkspaceNotWarm
	}
	attachment := workspace.Spec.Attachment
	epoch := acpTaskRecordedAttachmentEpoch(task)
	if attachment == nil || attachment.TaskRef.UID != task.UID || attachment.Epoch != epoch || epoch <= record.WorkspaceAttachmentEpoch {
		return acpPersistentReasonWorkspaceNotWarm
	}
	evidence, ok := parseACPWorkspaceAttachedFromWarm(workspace.Annotations[acpWorkspaceAttachedFromWarmAnnotation])
	if !ok || evidence.taskUID != string(task.UID) || evidence.epoch != epoch ||
		evidence.lastSettledTaskUID != record.SourceTaskUID || !evidence.suspendAt.After(record.KeptAt) {
		return acpPersistentReasonWorkspaceNotWarm
	}
	return ""
}

type acpWorkspaceAttachedFromWarm struct {
	taskUID            string
	epoch              int64
	suspendAt          time.Time
	lastSettledTaskUID string
}

func formatACPWorkspaceAttachedFromWarm(taskUID types.UID, epoch int64, suspendAt, lastSettled string) string {
	return strings.Join([]string{string(taskUID), strconv.FormatInt(epoch, 10), suspendAt, lastSettled}, " ")
}

func parseACPWorkspaceAttachedFromWarm(raw string) (acpWorkspaceAttachedFromWarm, bool) {
	fields := strings.Fields(strings.TrimSpace(raw))
	if len(fields) != 4 {
		return acpWorkspaceAttachedFromWarm{}, false
	}
	epoch, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || epoch < 1 {
		return acpWorkspaceAttachedFromWarm{}, false
	}
	suspendAt, err := time.Parse(time.RFC3339Nano, fields[2])
	if err != nil {
		return acpWorkspaceAttachedFromWarm{}, false
	}
	lastSettled := fields[3]
	if receiptTask, _, ok := parseACPWorkspaceSettlementReceipt(lastSettled); ok {
		lastSettled = receiptTask
	}
	return acpWorkspaceAttachedFromWarm{taskUID: fields[0], epoch: epoch, suspendAt: suspendAt, lastSettledTaskUID: lastSettled}, true
}

// acpWorkspaceAttachedFromWarmEvidence renders the attach-patch evidence for
// the exact object version being patched, or nil when that version is not in
// a warm window.
func acpWorkspaceAttachedFromWarmEvidence(current *workspacev1alpha1.ExecutionWorkspace, task *corev1alpha1.Task, epoch int64) map[string]string {
	if current == nil || task == nil || epoch < 1 {
		return nil
	}
	suspendAt := strings.TrimSpace(current.Annotations[acpWorkspaceSuspendAtAnnotation])
	lastSettled := strings.Fields(strings.TrimSpace(current.Annotations[acpWorkspaceLastSettledTaskAnnotation]))
	if suspendAt == "" || len(lastSettled) == 0 {
		return nil
	}
	if _, err := time.Parse(time.RFC3339Nano, suspendAt); err != nil {
		return nil
	}
	return map[string]string{
		acpWorkspaceAttachedFromWarmAnnotation: formatACPWorkspaceAttachedFromWarm(task.UID, epoch, suspendAt, lastSettled[0]),
	}
}

// acpPersistentSessionHeldWorkspace reports whether a Task may queue on the
// reuse path (ADR 0059 section 3 step 2): it holds its attachment at its own
// recorded epoch with a fenced Lease, the workspace is Ready, warm, untainted
// and not being revoked, the attach evidence names it, and a Resident record
// of the current epoch names this workspace and pool. It returns the
// record's handoff ID.
//
//nolint:gocyclo // Every queue-path condition of ADR 0059 section 3 step 2 is checked in one auditable place.
func (r *TaskReconciler) acpPersistentSessionHeldWorkspace(
	ctx context.Context,
	task *corev1alpha1.Task,
	plan ACPRuntimePlan,
	gatewayGrant bool,
) (string, string, error) {
	if acpPersistentSessionStaticScope(r.ACPPersistentSessionEnabled, task, plan, gatewayGrant) != "" ||
		r.DurableControlStore == nil || r.ControllerEpochManager == nil {
		return "", "", nil
	}
	if support, ok := r.DurableControlStore.(store.SessionResidentRuntimeSupport); !ok || !support.SupportsSessionResidentRuntime() {
		return "", "", nil
	}
	reader := uncachedReader(r.APIReader, r.Client)
	name := acpClassWorkspaceName(task, plan.Workspace)
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: name}, workspace); err != nil {
		return "", "", client.IgnoreNotFound(err)
	}
	epoch := acpTaskRecordedAttachmentEpoch(task)
	attachment := workspace.Spec.Attachment
	now := time.Now()
	if attachment == nil || attachment.TaskRef.UID != task.UID || epoch < 1 || attachment.Epoch != epoch ||
		workspace.Spec.AttachmentEpoch != epoch || !attachment.ExpiresAt.After(now) ||
		string(workspace.UID) != strings.TrimSpace(task.Annotations[acpExecutionWorkspaceUIDAnnotation]) ||
		workspace.Spec.DesiredState != workspacev1alpha1.ExecutionWorkspaceDesiredReady ||
		acpWorkspaceRevocationStampMatchesCurrentEpoch(workspace) ||
		workspace.Status.State == workspacev1alpha1.ExecutionWorkspaceStateFailed ||
		acpWorkspaceQuarantined(workspace) || operatoraccess.Tainted(workspace.Annotations) ||
		!workspace.DeletionTimestamp.IsZero() {
		return "", "", nil
	}
	evidence, ok := parseACPWorkspaceAttachedFromWarm(workspace.Annotations[acpWorkspaceAttachedFromWarmAnnotation])
	if !ok || evidence.taskUID != string(task.UID) || evidence.epoch != epoch {
		return "", "", nil
	}
	lease := &coordinationv1.Lease{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: workspace.Namespace, Name: attachmentLeaseName(workspace.Name)}, lease); err != nil {
		return "", "", client.IgnoreNotFound(err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != string(task.UID) || leaseExpired(lease, now) ||
		lease.Annotations[workspaceAttachmentLeaseEpochAnnotation] != strconv.FormatInt(epoch, 10) {
		return "", "", nil
	}
	control, err := r.DurableControlStore.GetSessionControl(ctx, task.Namespace, strings.TrimSpace(task.Spec.SessionRef.Name))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", "", nil
		}
		return "", "", err
	}
	record := control.ResidentRuntime
	if record == nil || record.State != store.SessionResidentRuntimeResident || record.WorkspaceUID != string(workspace.UID) ||
		record.RuntimePoolName != plan.PoolName || !now.Before(record.ExpiresAt) ||
		evidence.lastSettledTaskUID != record.SourceTaskUID {
		return "", "", nil
	}
	fence, err := r.ControllerEpochManager.CurrentFence(ctx)
	if err != nil {
		return "", "", err
	}
	if record.ControllerEpochName != fence.Name || record.ControllerEpoch != fence.Epoch {
		return "", "", nil
	}
	return name, record.HandoffID, nil
}

// runACPPersistentSessionGates runs ADR 0059 section 3 step 6 before the
// first runtime mutation. A Task queued against a held attachment first waits
// for the full post-attach handshake the queue skipped; that wait never
// times out into a prompt, only into a pre-submission requeue. A claimed
// record is then reused only if the pool, runtime, workspace evidence and the
// exact RuntimeSession status still match; otherwise the record is retired
// and the turn recreates. It reports whether the Task was requeued.
func (d *ACPDispatcher) runACPPersistentSessionGates(
	ctx, runtimeCtx context.Context,
	task *corev1alpha1.Task,
	attemptID string,
	fence store.ControllerEpochFence,
	target acpDispatchTarget,
	runtimeClient *harnessv2.Client,
	runtimeFence harnessv2.Fence,
	session *acpTaskSession,
	handshakeRequired bool,
	reuse *acpPersistentSessionReuse,
	timings *acpTurnTimings,
) (bool, error) {
	if !handshakeRequired && session.ResidentClaimed == nil {
		return false, nil
	}
	defer timings.mark(acpTurnStepResidentSessionGate)
	requeue := func(reason string, cause error) (bool, error) {
		reuse.miss(acpPersistentSessionStageGate, reason)
		d.stampACPPersistentSessionRefused(ctx, task, reason)
		if session.ResidentClaimed != nil {
			d.retireACPResidentRuntime(ctx, task, fence, session.ResidentClaimed)
		}
		if err := d.requeuePreSubmissionTaskWithRuntimeBinding(ctx, task, attemptID, fence, cause, &session.Binding); err != nil {
			return false, errors.Join(cause, err)
		}
		session.requeued = true
		return true, nil
	}
	if handshakeRequired {
		wait, err := d.acpPersistentSessionHandshakeGate(runtimeCtx, task, target.pool)
		reuse.gateWait = wait
		if err != nil {
			if runtimeContextError(runtimeCtx) != nil {
				// Cancellation and deadline settle through the ordinary
				// pre-submission handling; nothing was sent.
				if errors.Is(runtimeContextError(runtimeCtx), context.DeadlineExceeded) {
					reuse.miss(acpPersistentSessionStageGate, acpPersistentReasonTaskDeadline)
				} else {
					reuse.miss(acpPersistentSessionStageGate, acpPersistentReasonTaskCancelled)
				}
				if handled, deadlineErr := d.handlePreSubmissionContextDone(ctx, runtimeCtx, task, attemptID, fence); handled {
					return true, deadlineErr
				}
				session.requeued = false
				return true, d.settlePreSubmissionCancellation(
					ctx, task, attemptID, fence, "cancelled-before-submission", "Cancelled", "task cancelled before prompt submission",
				)
			}
			return requeue(acpPersistentReasonHandshakeTimeout, err)
		}
	}
	record := session.ResidentClaimed
	if record == nil {
		return false, nil
	}
	reason, err := d.acpPersistentSessionRuntimeGate(ctx, task, record, runtimeClient, runtimeFence, fence, target.pool)
	if err != nil {
		return requeue(acpPersistentReasonRuntimeSessionMissing, err)
	}
	if reason == "" && (session.Binding.SessionUID != record.RuntimeSessionUID ||
		session.Binding.Generation != uint64(record.RuntimeSessionGeneration) ||
		string(session.Binding.RuntimeInstanceID) != record.RuntimeInstanceID ||
		string(session.Binding.SupervisorBootID) != record.SupervisorBootID) {
		// The turn would address a session other than the one the gate proved.
		reason = acpPersistentReasonRuntimeSessionMissing
	}
	if reason != "" {
		reuse.miss(acpPersistentSessionStageGate, reason)
		d.stampACPPersistentSessionRefused(ctx, task, reason)
		if retired := d.retireACPResidentRuntime(ctx, task, fence, record); retired != nil && session.Turn != nil &&
			validateACPSessionLease(retired, session.Turn.Lease.Key) == nil {
			// Retirement advanced the SessionControl version under this
			// turn's exact lease; the fresh session's generation commit in
			// this same pass must expect that version, not requeue on it.
			session.Turn.Lease.Session = *retired
		}
		session.ResidentClaimed = nil
		plan := acpPersistentSessionRecreatePlan(
			ACPRuntimeSessionPlan{Binding: session.Binding}, record, session.LeaseGeneration,
		)
		session.Binding = plan.Binding
		session.Reused = false
		session.Bootstrap = session.FallbackBootstrap
		return false, nil
	}
	reuse.hit = true
	reuse.stage = acpPersistentSessionStageGate
	d.setRuntimeSessionBinding(session.Binding)
	return false, nil
}

// stampACPPersistentSessionRefused records a gate refusal on the Task so no
// later execution of this Task claims; it is best effort and bounded.
func (d *ACPDispatcher) stampACPPersistentSessionRefused(ctx context.Context, task *corev1alpha1.Task, reason string) {
	reason = boundedACPPersistentSessionReason(reason)
	if task == nil || task.Annotations[acpTaskPersistentSessionRefusedAnnotation] == reason || d.Client == nil {
		return
	}
	latest := &corev1alpha1.Task{}
	if err := d.Client.Get(ctx, client.ObjectKeyFromObject(task), latest); err != nil {
		return
	}
	base := latest.DeepCopy()
	if latest.Annotations == nil {
		latest.Annotations = map[string]string{}
	}
	latest.Annotations[acpTaskPersistentSessionRefusedAnnotation] = reason
	if err := d.Client.Patch(ctx, latest, client.MergeFrom(base)); err == nil {
		if task.Annotations == nil {
			task.Annotations = map[string]string{}
		}
		task.Annotations[acpTaskPersistentSessionRefusedAnnotation] = reason
	}
}

// retireACPResidentRuntime moves an exact record to Retiring and returns the
// resulting SessionControl, or nil when it did not. A failure is harmless:
// the record was claimed by this turn's lease and can never be claimed again,
// and the turn's own finalization clears it.
func (d *ACPDispatcher) retireACPResidentRuntime(
	ctx context.Context, task *corev1alpha1.Task, fence store.ControllerEpochFence, record *store.SessionResidentRuntime,
) *store.SessionControl {
	support, ok := d.Store.(store.SessionResidentRuntimeSupport)
	if !ok || record == nil || task == nil || task.Spec.SessionRef == nil {
		return nil
	}
	control, err := support.RetireSessionResidentRuntime(context.WithoutCancel(ctx), store.RetireSessionResidentRuntimeRequest{
		Namespace: task.Namespace, SessionName: strings.TrimSpace(task.Spec.SessionRef.Name),
		SessionUID: record.RuntimeSessionUID, HandoffID: record.HandoffID, Fence: fence, RetiredAt: time.Now().UTC(),
	})
	if err != nil {
		logf.FromContext(ctx).Info("retire ACP persistent session record failed; it stays unclaimable",
			"namespace", task.Namespace, "task", task.Name)
		return nil
	}
	return control
}
