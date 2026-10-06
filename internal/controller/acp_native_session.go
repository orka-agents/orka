package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const nativeCaptureIntentAnnotation = "orka.ai/native-session-capture"
const nativeInstallUnresolvedAnnotation = "orka.ai/native-session-install-unresolved"

var errNativeSessionInstallUnresolved = fmt.Errorf("%w: native Session installation outcome is unresolved; original runtime home and frozen plan require receipt reconciliation", store.ErrNotReady)
var errNativeSessionRuntimeUnsupported = fmt.Errorf("%w: staged native Session requires a Codex runtime advertising native sessions", store.ErrConflict)

func (d *ACPDispatcher) retainNativeSessionInstall(ctx context.Context, task *corev1alpha1.Task, metadata harnessv2.MutationMetadata) error {
	body, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &corev1alpha1.Task{}
		if err := uncachedReader(d.APIReader, d.Client).Get(ctx, client.ObjectKeyFromObject(task), latest); err != nil {
			return err
		}
		if latest.UID != task.UID || latest.Status.Execution == nil || task.Status.Execution == nil ||
			sessionRuntimeCleanupIdentityForExecution(latest.Status.Execution) != sessionRuntimeCleanupIdentityForExecution(task.Status.Execution) {
			return fmt.Errorf("%w: unresolved native install Task identity changed", store.ErrConflict)
		}
		base := latest.DeepCopy()
		if latest.Annotations == nil {
			latest.Annotations = map[string]string{}
		}
		latest.Annotations[nativeInstallUnresolvedAnnotation] = string(body)
		if err := d.Client.Patch(ctx, latest, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		task.Annotations = latest.Annotations
		return nil
	})
}

// Only immutable, non-secret request identity is retained on the Task. The
// client signs each send with the current controller credentials separately.
type nativeCaptureIntent struct {
	Request        harnessv2.CaptureNativeSessionRequest  `json:"request"`
	Reconciliation *harnessv2.CaptureNativeSessionRequest `json:"reconciliation,omitempty"`
	Unsupported    bool                                   `json:"unsupported,omitempty"`
}

func (d *ACPDispatcher) nativeSessionStore() store.NativeSessionStore {
	if d.Sessions == nil {
		return nil
	}
	s, _ := d.Sessions.transcripts.(store.NativeSessionStore)
	return s
}

func (d *ACPDispatcher) loadTaskNativeSession(ctx context.Context, task *corev1alpha1.Task, control *store.SessionControl) (*store.NativeSessionRecord, error) {
	s := d.nativeSessionStore()
	if s == nil {
		return nil, nil
	}
	record, err := s.GetNativeSession(ctx, task.Namespace, control.SessionName, control.SessionUID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load private native Session checkpoint: %w", err)
	}

	return record, nil
}

// Reject an incompatible Task without binding a staged import to a new Session
// control or acquiring its mutation lease. The preparation path checks again
// before opening a turn in case an import was staged after this read.
func (d *ACPDispatcher) validateTaskNativeSessionRuntime(ctx context.Context, task *corev1alpha1.Task, supported bool) error {
	if supported || task.Spec.SessionRef == nil || d.nativeSessionStore() == nil {
		return nil
	}
	control, err := d.Store.GetSessionControl(ctx, task.Namespace, task.Spec.SessionRef.Name)
	if errors.Is(err, store.ErrNotFound) {
		control = &store.SessionControl{SessionName: task.Spec.SessionRef.Name}
	} else if err != nil {
		return err
	}
	native, err := d.loadTaskNativeSession(ctx, task, control)
	if err != nil {
		return err
	}
	if native != nil {
		return errNativeSessionRuntimeUnsupported
	}
	return nil
}

func (d *ACPDispatcher) rejectNativeSessionRuntime(ctx context.Context, task *corev1alpha1.Task, attemptID string, fence store.ControllerEpochFence) error {
	reason := corev1alpha1.TaskExecutionReason("NativeSessionRuntimeUnsupported")
	message := errNativeSessionRuntimeUnsupported.Error()
	if err := d.transitionAttemptToFailed(ctx, attemptID, fence, "native-session-runtime-unsupported", reason, message); err != nil {
		return err
	}
	return d.failTaskBeforeSessionBinding(ctx, task, corev1alpha1.TaskExecutionStateFailed, corev1alpha1.TaskExecutionOutcomeFailed, reason, message)
}

func nativeRestoreForTask(session *acpTaskSession, request harnessv2.CreateRuntimeSessionRequest, supported bool) (*harnessv2.NativeSessionRestore, error) {
	if session == nil || session.NativeSession == nil {
		return nil, nil
	}
	if !supported || request.Profile.ProviderKind != runtimePoolProviderCodex {
		return nil, errNativeSessionRuntimeUnsupported
	}
	snapshot := session.NativeSession.Snapshot
	// Imported/captured runtime and policy identities never become destination
	// authority. The supervisor installs and loads with its actual fresh cwd.
	snapshot.RuntimeSessionUID = request.Metadata.Fence.RuntimeSessionUID
	snapshot.RuntimeProfileDigest = request.Metadata.Fence.RuntimeProfileDigest
	snapshot.WorkingDirectory = "/workspace"
	return &harnessv2.NativeSessionRestore{Snapshot: snapshot}, nil
}

func verifyTaskNativeRestoration(session *acpTaskSession, proof *harnessv2.NativeSessionRestoration) error {
	if session == nil || session.NativeSession == nil {
		return nil
	}
	want := session.NativeSession.Snapshot
	if proof == nil || !proof.Loaded || proof.DataDigest != want.DataDigest || proof.ProviderSessionID != want.ProviderSessionID {
		return fmt.Errorf("%w: runtime did not prove loading the exact private native Session", store.ErrConflict)
	}
	// Canonical bootstrap is suppressed only after the exact ACP load proof.
	session.Bootstrap = nil
	return nil
}

func nativeCaptureEligible(session *acpTaskSession, supported bool) bool {
	return supported && session != nil && session.Turn != nil && !session.Turn.SkipTranscriptAppend
}

func (d *ACPDispatcher) persistNativeCaptureIntent(ctx context.Context, task *corev1alpha1.Task, intent nativeCaptureIntent) error {
	body, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &corev1alpha1.Task{}
		if err := uncachedReader(d.APIReader, d.Client).Get(ctx, client.ObjectKeyFromObject(task), latest); err != nil {
			return err
		}
		if latest.UID != task.UID || latest.Status.Execution == nil || task.Status.Execution == nil ||
			sessionRuntimeCleanupIdentityForExecution(latest.Status.Execution) != sessionRuntimeCleanupIdentityForExecution(task.Status.Execution) ||
			latest.Status.Execution.Attempt != task.Status.Execution.Attempt || latest.Status.Execution.PromptID != task.Status.Execution.PromptID {
			return fmt.Errorf("%w: Task native capture identity changed", store.ErrConflict)
		}
		if previous := latest.Annotations[nativeCaptureIntentAnnotation]; previous != "" {
			var old nativeCaptureIntent
			if err := json.Unmarshal([]byte(previous), &old); err != nil || old.Request.Metadata.RequestDigest != intent.Request.Metadata.RequestDigest {
				return fmt.Errorf("%w: Task native capture intent changed", store.ErrConflict)
			}
		}
		base := latest.DeepCopy()
		if latest.Annotations == nil {
			latest.Annotations = map[string]string{}
		}
		latest.Annotations[nativeCaptureIntentAnnotation] = string(body)
		if err := d.Client.Patch(ctx, latest, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		task.Annotations = latest.Annotations
		return nil
	})
}

func (d *ACPDispatcher) captureTaskNativeSession(ctx context.Context, runtimeClient *harnessv2.Client, task *corev1alpha1.Task, runtimeFence harnessv2.Fence, session *acpTaskSession) error {
	if session.NativeCapture != nil {
		return nil
	}
	var intent nativeCaptureIntent
	if saved := task.Annotations[nativeCaptureIntentAnnotation]; saved != "" {
		if err := json.Unmarshal([]byte(saved), &intent); err != nil {
			return fmt.Errorf("%w: durable native capture intent is malformed", store.ErrConflict)
		}
		original := intent.Request.Metadata
		if original.TaskUID != harnessv2.TaskUID(acpTaskControlUID(task)) || original.TaskAttempt != uint32(task.Status.Execution.Attempt) ||
			original.Fence.RuntimeSessionUID != runtimeFence.RuntimeSessionUID || original.Fence.RuntimeSessionGeneration != runtimeFence.RuntimeSessionGeneration ||
			original.Fence.RuntimeInstanceID != runtimeFence.RuntimeInstanceID || original.Fence.SupervisorBootID != runtimeFence.SupervisorBootID ||
			original.Fence.RuntimeProfileDigest != runtimeFence.RuntimeProfileDigest {
			return fmt.Errorf("%w: durable native capture intent belongs to another runtime", store.ErrConflict)
		}
	} else {
		intent.Request = harnessv2.CaptureNativeSessionRequest{
			Protocol: harnessv2.ProtocolVersion,
			Metadata: mutationMetadataForTaskUID(runtimeFence, task, acpTaskControlUID(task), "capture-native-g"+strconv.FormatUint(runtimeFence.RuntimeSessionGeneration, 10), false, time.Now().UTC().Add(2*time.Minute)),
		}
		if err := sealMutation(&intent.Request.Metadata.RequestDigest, intent.Request); err != nil {
			return err
		}
		if err := d.persistNativeCaptureIntent(ctx, task, intent); err != nil {
			return err
		}
	}
	if intent.Unsupported {
		if session.NativeSession != nil {
			return fmt.Errorf("%w: native continuity cannot omit a checkpoint", store.ErrConflict)
		}
		return nil
	}
	request := intent.Request
	if harnessv2.CompareFence(runtimeFence, request.Metadata.Fence, true) != harnessv2.FenceMatch || !request.Metadata.ExpiresAt.After(time.Now().UTC()) {
		// Receipt reconciliation can only retrieve the immutable result of the
		// original capture. It cannot stop a child or begin another capture.
		if intent.Reconciliation == nil || harnessv2.CompareFence(runtimeFence, intent.Reconciliation.Metadata.Fence, true) != harnessv2.FenceMatch ||
			!intent.Reconciliation.Metadata.ExpiresAt.After(time.Now().UTC()) {
			reconciliation := harnessv2.CaptureNativeSessionRequest{
				Protocol:            harnessv2.ProtocolVersion,
				Metadata:            mutationMetadataForTaskUID(runtimeFence, task, acpTaskControlUID(task), "reconcile-native-"+strconv.FormatInt(time.Now().UTC().UnixNano(), 10), false, time.Now().UTC().Add(2*time.Minute)),
				OriginalOperationID: request.Metadata.OperationID, OriginalRequestDigest: request.Metadata.RequestDigest,
			}
			if err := sealMutation(&reconciliation.Metadata.RequestDigest, reconciliation); err != nil {
				return err
			}
			intent.Reconciliation = &reconciliation
			if err := d.persistNativeCaptureIntent(ctx, task, intent); err != nil {
				return err
			}
		}
		request = *intent.Reconciliation
	}
	response, err := runtimeClient.CaptureNativeSession(ctx, harnessv2.RuntimeSessionID(runtimeSessionID(runtimeFence)), request)
	if err != nil {
		if clientErr, ok := errors.AsType[*harnessv2.ClientError](err); ok && clientErr.Code == harnessv2.ErrorCodeNativeCaptureUnsupported && session.NativeSession == nil {
			intent.Unsupported = true
			if persistErr := d.persistNativeCaptureIntent(ctx, task, intent); persistErr != nil {
				return persistErr
			}
			return nil
		}
		return fmt.Errorf("capture private native Session; exact runtime evidence retained: %w", err)
	}
	session.NativeCapture = &store.NativeSessionRecord{
		Namespace: session.Turn.Lease.Session.Namespace, SessionName: session.Turn.Lease.Session.SessionName, SessionUID: session.Turn.Lease.Session.SessionUID,
		Snapshot: response.Snapshot, RuntimeSessionGeneration: int64(runtimeFence.RuntimeSessionGeneration),
		SourceOperationID: string(intent.Request.Metadata.OperationID),
	}
	session.Binding.RecreationRequired = true
	d.setRuntimeSessionBinding(session.Binding)
	return d.patchExecution(ctx, task, func(execution *corev1alpha1.TaskExecutionStatus) {
		execution.RuntimeSessionRecreationPending = true
	})
}

// Recovery uses the exact surviving built-in RuntimePool incarnation. Losing
// that incarnation cannot be interpreted as a successful native checkpoint.
func (d *ACPDispatcher) recoverTaskNativeCapture(ctx context.Context, task *corev1alpha1.Task, session *acpTaskSession) error {
	if session == nil || session.Turn == nil || session.Turn.SkipTranscriptAppend {
		return nil
	}
	native, nativeErr := d.loadTaskNativeSession(ctx, task, &session.Turn.Lease.Session)
	if nativeErr != nil {
		return nativeErr
	}
	session.NativeSession = native
	pending := task.Annotations[nativeCaptureIntentAnnotation] != ""
	if !pending && native == nil {
		return nil
	}
	execution := task.Status.Execution
	pool := &corev1alpha1.RuntimePool{}
	if execution == nil || execution.RuntimePoolName == "" {
		return fmt.Errorf("%w: native capture recovery requires its exact RuntimePool", store.ErrConflict)
	}
	if err := d.APIReader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: execution.RuntimePoolName}, pool); err != nil {
		return err
	}
	active := pool.Status.ActiveInstance
	if string(pool.UID) != execution.RuntimePoolUID || active == nil || active.RuntimeInstanceID != execution.RuntimeInstanceID ||
		active.BootID != execution.RuntimeSessionSupervisorBootID || pool.Spec.Runtime.Profile.Digest != execution.RuntimeSessionProfileDigest {
		return fmt.Errorf("%w: native capture runtime was replaced before its durable receipt", store.ErrConflict)
	}
	runtimeClient, runtimeFence, _, _, err := d.runtimePoolClient(ctx, pool)
	if err != nil {
		return err
	}
	runtimeFence.RuntimeSessionUID = harnessv2.RuntimeSessionUID(execution.RuntimeSessionUID)
	runtimeFence.RuntimeSessionGeneration = uint64(execution.RuntimeSessionGeneration)
	return d.captureTaskNativeSession(ctx, runtimeClient, task, runtimeFence, session)
}
