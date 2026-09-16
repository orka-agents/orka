package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/opencodestate"
	"github.com/orka-agents/orka/internal/store"
)

type acpNativeSessionPolicy struct {
	WorkspaceBindingDigest string
	MCPDigest              string
}

const acpNativeProviderKind = "opencode"

type acpNativeSession struct {
	Policy              acpNativeSessionPolicy
	NamespaceUID        string
	ConfigurationDigest string
	HistoryBeforeDigest string
	HistoryBeforeCount  int
	HistoryDigest       string
	Snapshot            *store.NativeSessionSnapshot
	Capture             bool
	Reason              string
	Method              string
}

// Native continuity is opt-in for one operator-selected class. Unsupported
// providers and external runtimes retain their existing creation contract.
func (d *ACPDispatcher) nativeSessionPolicy(
	task *corev1alpha1.Task, plan ACPRuntimePlan, capabilities harnessv2.CapabilitiesResponse, mcpDigest string,
) *acpNativeSessionPolicy {
	w := plan.Workspace
	if d.NativeSessions == nil || d.Sessions == nil || !d.Sessions.RecordsLineage() ||
		d.NativeSessionWorkspaceClass == "" || task.Spec.SessionRef == nil || !task.Spec.SessionRef.Append ||
		plan.Profile.ProviderKind != acpNativeProviderKind || !capabilities.SupportsNativeSessionRestore ||
		effectiveACPWorkspaceIntent(task) != corev1alpha1.WorkspaceIntentRead || w == nil ||
		w.Provider != corev1alpha1.WorkspaceProviderAgentSandbox || w.ReusePolicy != corev1alpha1.WorkspaceReusePolicySession ||
		w.Class == nil || w.Class.Name != d.NativeSessionWorkspaceClass || w.Class.SuspendMode != "DataOnly" ||
		w.Class.SandboxVolume == nil {
		return nil
	}
	return &acpNativeSessionPolicy{WorkspaceBindingDigest: w.BindingDigest, MCPDigest: mcpDigest}
}

// Plan before taking the lease, then recheck the exact lease and transcript
// before using or staging any bytes. Reading one extra message detects a
// truncated prefix without loading an unbounded Session transcript.
func (d *ACPDispatcher) planNativeSession(
	ctx context.Context, task *corev1alpha1.Task, preparation *acpTaskSessionPreparation, lineage acpSessionLineageIdentity,
) (*acpNativeSession, error) {
	if lineage.Native == nil {
		return nil, nil
	}
	native := &acpNativeSession{Policy: *lineage.Native, NamespaceUID: lineage.NamespaceUID,
		Reason: "saved_copy_unavailable", Method: "reconstructed"}
	var err error
	native.ConfigurationDigest, err = acpDomainDigest("native-session-configuration/v1", struct {
		Lineage string `json:"lineage"`
		MCP     string `json:"mcp"`
	}{lineage.ConfigDigest, lineage.Native.MCPDigest})
	if err != nil {
		return nil, err
	}
	// Retain a canonical bootstrap even for a planned live reuse. Authenticated
	// status may prove that it is a new, empty session from an interrupted create.
	preparation.bootstrap, preparation.userPrompt, err = d.resolveTaskSessionBootstrap(ctx, task, preparation.control)
	if err != nil {
		return nil, err
	}
	limits, err := d.Sessions.bootstrapLimits.withDefaults()
	if err != nil {
		return nil, err
	}
	limit := min(acpSessionReferenceMaxMessages(task.Spec.SessionRef), limits.MaxMessages)
	messages, err := d.Sessions.transcripts.LoadTranscript(ctx, task.Namespace, preparation.control.SessionName, limit+1)
	if err != nil {
		return nil, err
	}
	if len(messages) > limit || preparation.bootstrap.Truncated {
		native.Reason = "history_limit"
		return native, nil
	}
	ref := task.Spec.SessionRef
	if cutoff := strings.TrimSpace(ref.ThroughMessageID); cutoff != "" &&
		(len(messages) == 0 || messages[len(messages)-1].ID != cutoff) {
		native.Reason = "history_cutoff"
		return native, nil
	}
	native.HistoryBeforeDigest, err = store.NativeSessionHistoryDigest(messages)
	native.HistoryBeforeCount = len(messages)
	if err != nil {
		return nil, err
	}
	if ref.PromptIncluded {
		if len(messages) == 0 || messages[len(messages)-1].Role != acpBootstrapRoleUser {
			return nil, store.ValidationErrorf("native continuation requires the canonical current user message")
		}
		messages = messages[:len(messages)-1]
	}
	native.HistoryDigest, err = store.NativeSessionHistoryDigest(messages)
	if err != nil {
		return nil, err
	}
	native.Capture = true
	saved, err := d.NativeSessions.GetNativeSessionSnapshot(ctx, task.Namespace, preparation.control.SessionName, preparation.control.SessionUID)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrConflict) {
		return native, nil
	}
	if err != nil {
		return nil, err
	}
	sourceGeneration := preparation.control.LeaseGeneration
	if lease := preparation.control.Lease; lease != nil && lease.TaskUID == string(task.UID) &&
		lease.Attempt == int64(task.Status.Execution.Attempt) && lease.PromptID == task.Status.Execution.PromptID {
		sourceGeneration--
	}
	if !native.matchesSnapshot(saved, preparation.plan.Binding, sourceGeneration, messages) {
		native.Reason = "saved_copy_incompatible"
		return native, nil
	}
	native.Snapshot = saved
	return native, nil
}

func (n *acpNativeSession) matchesSnapshot(
	saved *store.NativeSessionSnapshot, binding ACPRuntimeSessionBinding, sourceGeneration int64, messages []store.SessionMessage,
) bool {
	return saved.Key.LeaseGeneration == sourceGeneration && saved.NamespaceUID == n.NamespaceUID &&
		saved.ProfileDigest == string(binding.ProfileDigest) && saved.ConfigurationDigest == n.ConfigurationDigest &&
		saved.WorkspaceBindingDigest == n.Policy.WorkspaceBindingDigest && saved.ProviderVersion == opencodestate.ProviderVersion &&
		saved.HistoryDigest == n.HistoryDigest && saved.HistoryMessageCount == len(messages) &&
		len(messages) > 0 && saved.ThroughMessageID == messages[len(messages)-1].ID
}

// A live conversation can be reused only if its last recorded turn is exactly
// the saved source, or if this Task itself created the still-unprompted session.
// Thus append=false taints warm reuse until an eligible Task recreates it.
func (s *acpTaskSession) adoptNativeSession(task *corev1alpha1.Task, observed harnessv2.RuntimeSessionStatus) bool {
	if s.Native == nil {
		return true
	}
	native := s.Native
	if observed.CreationTaskUID == harnessv2.TaskUID(task.UID) &&
		observed.CreationTaskAttempt == uint32(task.Status.Execution.Attempt) {
		result := observed.NativeRestoration
		if result == nil || result.Method == "reconstructed" {
			if result != nil {
				native.Reason = result.Reason
			}
			return true
		}
		if native.Snapshot == nil || result.SnapshotID != native.Snapshot.ID ||
			observed.ProviderSessionID != native.Snapshot.ProviderSessionID {
			return false
		}
		native.Method, native.Reason = result.Method, result.Reason
		s.Bootstrap = nil
		return true
	}
	if native.Snapshot == nil || observed.ProviderSessionID != native.Snapshot.ProviderSessionID ||
		s.Binding.WorkspaceDigest != native.Snapshot.WorkspaceDigest {
		return false
	}
	native.Method, native.Reason = "reused", "complete_live_conversation"
	s.Bootstrap = nil
	return true
}

func (s *acpTaskSession) nativeRestore(workspaceDigest string) *harnessv2.NativeSessionRestore {
	if s == nil || s.Native == nil || s.Native.Snapshot == nil || s.Reused {
		return nil
	}
	saved := s.Native.Snapshot
	if saved.WorkspaceDigest != workspaceDigest || saved.Key.LeaseGeneration+1 != s.LeaseGeneration {
		s.Native.Snapshot, s.Native.Reason = nil, "workspace_binding_mismatch"
		return nil
	}
	return &harnessv2.NativeSessionRestore{SnapshotID: saved.ID, Snapshot: harnessv2.NativeSessionSnapshot{
		SessionUID: harnessv2.RuntimeSessionUID(saved.SessionUID), ProviderKind: acpNativeProviderKind,
		ProviderVersion: saved.ProviderVersion, ProviderSessionID: saved.ProviderSessionID,
		ProfileDigest: harnessv2.ProfileDigest(saved.ProfileDigest), WorkingDirectory: saved.WorkingDirectory,
		WorkspaceStateDigest: saved.WorkspaceStateDigest, DataDigest: saved.DataDigest, Data: saved.Data,
	}}
}

func (d *ACPDispatcher) validateNativeSessionAuthority(ctx context.Context, session *acpTaskSession, fence store.ControllerEpochFence) error {
	if session == nil || session.Turn == nil || session.Native == nil {
		return store.ValidationErrorf("native continuation requires an open Session turn")
	}
	current, err := d.Sessions.controls.GetSessionControl(ctx, session.Turn.Lease.Session.Namespace, session.Turn.Lease.Session.SessionName)
	if err != nil {
		return err
	}
	key := session.Turn.Turn.Key
	if current.SessionUID != key.SessionUID || current.Availability != store.SessionAvailable ||
		current.LeaseGeneration != key.LeaseGeneration || current.Lease == nil ||
		current.Lease.TaskUID != key.TaskUID || current.Lease.Attempt != key.Attempt || current.Lease.PromptID != key.PromptID ||
		current.ControllerEpochName != fence.Name || current.ControllerEpoch != fence.Epoch ||
		current.Lineage == nil || current.Lineage.NamespaceUID != session.Native.NamespaceUID {
		return fmt.Errorf("%w: native continuation Session authority changed", store.ErrConflict)
	}
	if reader, ok := d.Store.(interface {
		GetControllerEpochFence(context.Context, string) (store.ControllerEpochFence, error)
	}); ok {
		authoritative, err := reader.GetControllerEpochFence(ctx, fence.Name)
		if err != nil {
			return err
		}
		if authoritative != fence {
			return fmt.Errorf("%w: native continuation controller authority changed", store.ErrConflict)
		}
	} else {
		return errors.New("native continuation requires authoritative epoch reads")
	}
	return nil
}

func (d *ACPDispatcher) validateNativeSessionHistory(ctx context.Context, session *acpTaskSession, fence store.ControllerEpochFence) error {
	if session == nil || session.Native == nil {
		return nil
	}
	if err := d.validateNativeSessionAuthority(ctx, session, fence); err != nil {
		return err
	}
	native := session.Native
	if native.HistoryBeforeDigest == "" {
		return nil
	}
	messages, err := d.Sessions.transcripts.LoadTranscript(ctx, session.Turn.Lease.Session.Namespace,
		session.Turn.Lease.Session.SessionName, native.HistoryBeforeCount+1)
	if err != nil {
		return err
	}
	digest, err := store.NativeSessionHistoryDigest(messages)
	if err != nil {
		return err
	}
	if len(messages) != native.HistoryBeforeCount || digest != native.HistoryBeforeDigest {
		return store.ConflictErrorf("native continuation allowed transcript changed")
	}
	return nil
}

func (d *ACPDispatcher) stageNativeSession(
	ctx context.Context, session *acpTaskSession, fence store.ControllerEpochFence, captured *harnessv2.NativeSessionSnapshot,
) error {
	if session == nil || session.Native == nil || !session.Native.Capture || captured == nil {
		return nil
	}
	if err := d.validateNativeSessionAuthority(ctx, session, fence); err != nil {
		return err
	}
	if captured.SessionUID != harnessv2.RuntimeSessionUID(session.Binding.SessionUID) ||
		captured.ProfileDigest != session.Binding.ProfileDigest || captured.ProviderKind != acpNativeProviderKind ||
		captured.ProviderVersion != opencodestate.ProviderVersion {
		return store.ConflictErrorf("native capture runtime identity changed")
	}
	if err := captured.Validate(); err != nil {
		return err
	}
	native := session.Native
	return d.NativeSessions.StageNativeSessionSnapshot(ctx, store.NativeSessionSnapshot{
		Namespace: session.Turn.Lease.Session.Namespace, SessionName: session.Turn.Lease.Session.SessionName,
		SessionUID: session.Binding.SessionUID, NamespaceUID: native.NamespaceUID, Key: session.Turn.Turn.Key,
		ProviderSessionID: captured.ProviderSessionID, ProviderVersion: captured.ProviderVersion,
		ProfileDigest: string(captured.ProfileDigest), ConfigurationDigest: native.ConfigurationDigest,
		WorkspaceBindingDigest: native.Policy.WorkspaceBindingDigest, WorkspaceDigest: session.Binding.WorkspaceDigest,
		WorkspaceStateDigest: captured.WorkspaceStateDigest, WorkingDirectory: captured.WorkingDirectory,
		DataDigest: captured.DataDigest, Data: captured.Data,
	}, native.HistoryBeforeDigest)
}

func (d *ACPDispatcher) reportNativeSession(ctx context.Context, task *corev1alpha1.Task, session *acpTaskSession) error {
	if session == nil || session.Native == nil {
		return nil
	}
	native := session.Native
	reason, status := "Reconstructed", metav1.ConditionFalse
	switch native.Method {
	case "session/resume", "session/load":
		reason, status = "Restored", metav1.ConditionTrue
	case "reused":
		reason, status = "Reused", metav1.ConditionTrue
	}
	current := &corev1alpha1.Task{}
	if err := d.Client.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
		return err
	}
	if current.UID != task.UID {
		return store.ConflictErrorf("native continuation Task was replaced")
	}
	before := current.DeepCopy()
	apimeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{
		Type: "NativeSessionContinuity", Status: status, Reason: reason,
		Message: native.Reason, ObservedGeneration: current.Generation, LastTransitionTime: metav1.NewTime(time.Now().UTC()),
	})
	if err := d.Client.Status().Patch(ctx, current, client.MergeFrom(before)); err != nil {
		return err
	}
	logf.FromContext(ctx).Info("ACP Session continuity", "namespace", task.Namespace, "task", task.Name,
		"method", native.Method, "reason", native.Reason)
	return nil
}
