package kube

import (
	"context"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

var _ store.SessionResidentRuntimeSupport = (*Store)(nil)

// SupportsSessionResidentRuntime reports that RuntimeSessionControl status
// persists ADR 0059 handoff records.
func (s *Store) SupportsSessionResidentRuntime() bool { return true }

// RetireSessionResidentRuntime moves the exact named handoff record to
// Retiring under the controller-epoch interlock. A missing, replaced, or
// already retiring record is an idempotent success: retirement only ever
// removes authority to claim.
func (s *Store) RetireSessionResidentRuntime(ctx context.Context, request store.RetireSessionResidentRuntimeRequest) (*store.SessionControl, error) {
	if err := s.requireClient(); err != nil {
		return nil, err
	}
	request.Namespace = strings.TrimSpace(request.Namespace)
	request.SessionName = strings.TrimSpace(request.SessionName)
	request.SessionUID = strings.TrimSpace(request.SessionUID)
	for field, value := range map[string]string{
		sessionControlFieldNamespace: request.Namespace,
		sessionControlFieldName:      request.SessionName,
		sessionControlFieldUID:       request.SessionUID,
	} {
		if err := store.ValidateControlIdentifier(field, value); err != nil {
			return nil, err
		}
	}
	if err := store.ValidateCanonicalDigest("resident runtime handoff ID", request.HandoffID); err != nil {
		return nil, err
	}
	ctx, fence, snapshot, err := s.requireControllerEpoch(ctx, request.Fence)
	if err != nil {
		return nil, err
	}
	defer s.releaseControllerEpochMutation(snapshot)
	object, err := s.getSessionControlObject(ctx, request.Namespace, request.SessionName)
	if err != nil {
		return nil, err
	}
	control := sessionControlFromObject(object)
	if control.SessionUID != request.SessionUID {
		return nil, store.ConflictErrorf("session %s/%s UID does not match immutable control record", request.Namespace, request.SessionName)
	}
	record := control.ResidentRuntime
	if record == nil || record.HandoffID != request.HandoffID || record.State == store.SessionResidentRuntimeRetiring {
		return &control, nil
	}
	updated := object.DeepCopy()
	retired := store.CloneSessionResidentRuntime(record)
	retired.State = store.SessionResidentRuntimeRetiring
	updated.Status.ResidentRuntime = residentRuntimeToAPI(retired)
	retiredAt := store.NormalizeControlTime(request.RetiredAt)
	setMutationStatus(&updated.Status.ControlRecordMutationStatus, fence, snapshot, control.Version+1,
		control.LastOperationID, control.LastOperationDigest, control.CreatedAt, retiredAt)
	if err := s.client.Status().Update(ctx, updated); err != nil {
		return nil, mapKubernetesError("retire Session resident runtime", err)
	}
	result := sessionControlFromObject(updated)
	return &result, nil
}

func residentRuntimeFromAPI(value *corev1alpha1.RuntimeSessionResidentRuntimeStatus) *store.SessionResidentRuntime {
	if value == nil {
		return nil
	}
	record := &store.SessionResidentRuntime{
		HandoffID:     value.HandoffID,
		SourceTaskUID: value.SourceTaskUID, SourceAttempt: value.SourceAttempt,
		SourcePromptAttemptID: value.SourcePromptAttemptID, SourceTurnID: value.SourceTurnID,
		SourceLeaseGeneration: value.SourceLeaseGeneration,
		ControllerEpochName:   value.ControllerEpochName, ControllerEpoch: value.ControllerEpoch,
		WorkspaceName: value.WorkspaceName, WorkspaceUID: value.WorkspaceUID,
		WorkspaceAttachmentEpoch: value.WorkspaceAttachmentEpoch,
		RuntimePoolName:          value.RuntimePoolName, RuntimePoolUID: value.RuntimePoolUID,
		RuntimePoolGeneration: value.RuntimePoolGeneration,
		RuntimeInstanceID:     value.RuntimeInstanceID, SupervisorBootID: value.SupervisorBootID,
		ServedRecordDigest: value.ServedRecordDigest,
		RuntimeSessionUID:  value.RuntimeSessionUID, RuntimeSessionGeneration: value.RuntimeSessionGeneration,
		ProfileDigest: value.ProfileDigest, MCPDigest: value.MCPDigest,
		WorkspaceDigest: value.WorkspaceDigest, LineageConfigDigest: value.LineageConfigDigest,
		ReuseDigest: value.ReuseDigest, TranscriptHeadDigest: value.TranscriptHeadDigest,
		RebasedTreeDigest: value.RebasedTreeDigest,
		VerifiedBaseline:  verifiedBaselineFromAPI(value.VerifiedBaseline),
		PromptDelivered:   value.PromptDelivered,
		KeptAt:            value.KeptAt.UTC(), ExpiresAt: value.ExpiresAt.UTC(),
		State: store.SessionResidentRuntimeState(value.State),
	}
	if value.Claim != nil {
		record.Claim = &store.SessionResidentRuntimeClaim{
			TaskUID: value.Claim.TaskUID, Attempt: value.Claim.Attempt,
			PromptID: value.Claim.PromptID, LeaseGeneration: value.Claim.LeaseGeneration,
		}
	}
	return record
}

func residentRuntimeToAPI(value *store.SessionResidentRuntime) *corev1alpha1.RuntimeSessionResidentRuntimeStatus {
	if value == nil {
		return nil
	}
	record := &corev1alpha1.RuntimeSessionResidentRuntimeStatus{
		HandoffID:     value.HandoffID,
		SourceTaskUID: value.SourceTaskUID, SourceAttempt: value.SourceAttempt,
		SourcePromptAttemptID: value.SourcePromptAttemptID, SourceTurnID: value.SourceTurnID,
		SourceLeaseGeneration: value.SourceLeaseGeneration,
		ControllerEpochName:   value.ControllerEpochName, ControllerEpoch: value.ControllerEpoch,
		WorkspaceName: value.WorkspaceName, WorkspaceUID: value.WorkspaceUID,
		WorkspaceAttachmentEpoch: value.WorkspaceAttachmentEpoch,
		RuntimePoolName:          value.RuntimePoolName, RuntimePoolUID: value.RuntimePoolUID,
		RuntimePoolGeneration: value.RuntimePoolGeneration,
		RuntimeInstanceID:     value.RuntimeInstanceID, SupervisorBootID: value.SupervisorBootID,
		ServedRecordDigest: value.ServedRecordDigest,
		RuntimeSessionUID:  value.RuntimeSessionUID, RuntimeSessionGeneration: value.RuntimeSessionGeneration,
		ProfileDigest: value.ProfileDigest, MCPDigest: value.MCPDigest,
		WorkspaceDigest: value.WorkspaceDigest, LineageConfigDigest: value.LineageConfigDigest,
		ReuseDigest: value.ReuseDigest, TranscriptHeadDigest: value.TranscriptHeadDigest,
		RebasedTreeDigest: value.RebasedTreeDigest,
		VerifiedBaseline:  verifiedBaselineToAPI(value.VerifiedBaseline),
		PromptDelivered:   value.PromptDelivered,
		KeptAt:            metav1.NewTime(value.KeptAt.UTC()), ExpiresAt: metav1.NewTime(value.ExpiresAt.UTC()),
		State: corev1alpha1.RuntimeSessionResidentRuntimeState(value.State),
	}
	if value.Claim != nil {
		record.Claim = &corev1alpha1.RuntimeSessionResidentRuntimeClaim{
			TaskUID: value.Claim.TaskUID, Attempt: value.Claim.Attempt,
			PromptID: value.Claim.PromptID, LeaseGeneration: value.Claim.LeaseGeneration,
		}
	}
	return record
}
