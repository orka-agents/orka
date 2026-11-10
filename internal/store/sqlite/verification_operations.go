package sqlite

import (
	"context"
	"fmt"

	pv "github.com/orka-agents/orka/internal/patchverification"
)

type verificationOperationResult[T any] struct {
	Value T
	Code  string
}

func verificationErrorCode(err error) (string, bool) {
	switch err {
	case nil:
		return "", true
	case pv.ErrRunNotFound:
		return "not-found", true
	case pv.ErrBinding:
		return "binding", true
	case pv.ErrConflict:
		return "conflict", true
	case pv.ErrClosed:
		return "closed", true
	case pv.ErrIntegrity:
		return "integrity", true
	case pv.ErrLimit:
		return "limit", true
	case pv.ErrEvidence:
		return "evidence", true
	default:
		return "", false
	}
}

func verificationOutcomeError(code string) error {
	switch code {
	case "":
		return nil
	case "not-found":
		return pv.ErrRunNotFound
	case "binding":
		return pv.ErrBinding
	case "conflict":
		return pv.ErrConflict
	case "closed":
		return pv.ErrClosed
	case "integrity":
		return pv.ErrIntegrity
	case "limit":
		return pv.ErrLimit
	case "evidence":
		return pv.ErrEvidence
	default:
		return fmt.Errorf("%w: invalid verification operation outcome", pv.ErrIntegrity)
	}
}

func writeVerificationOperation[T any](ctx context.Context, s *Store, method string, body func(context.Context) (T, error)) (T, error) {
	// Evidence refusals can intentionally record tampering. Keep their domain
	// outcome with the commit so envelope replay neither drops nor repeats it.
	result, err := writeOperation(ctx, s, method, nil, func(ctx context.Context) (verificationOperationResult[T], error) {
		value, err := body(ctx)
		code, known := verificationErrorCode(err)
		if !known {
			return verificationOperationResult[T]{}, err
		}
		return verificationOperationResult[T]{Value: value, Code: code}, nil
	})
	if err != nil {
		var zero T
		return zero, err
	}
	return result.Value, verificationOutcomeError(result.Code)
}

func writeVerificationOperationErr(ctx context.Context, s *Store, method string, body func(context.Context) error) error {
	_, err := writeVerificationOperation(ctx, s, method, func(ctx context.Context) (noResult, error) {
		return noResult{}, body(ctx)
	})
	return err
}

func (s *Store) CreatePatchVerificationRun(ctx context.Context, manifest pv.Manifest, binding pv.Binding, provenance map[string][]byte) error {
	return writeVerificationOperationErr(ctx, s, "CreatePatchVerificationRun", func(ctx context.Context) error {
		return s.createPatchVerificationRunWrite(ctx, manifest, binding, provenance)
	})
}

func (s *Store) GetPatchVerificationRun(ctx context.Context, runID string) (*pv.Record, error) {
	return writeVerificationOperation(ctx, s, "GetPatchVerificationRun", func(ctx context.Context) (*pv.Record, error) {
		return s.getPatchVerificationRunWrite(ctx, runID)
	})
}

func (s *Store) GetPatchVerificationBlob(ctx context.Context, runID, digest string) ([]byte, error) {
	return writeVerificationOperation(ctx, s, "GetPatchVerificationBlob", func(ctx context.Context) ([]byte, error) {
		return s.getPatchVerificationBlobWrite(ctx, runID, digest)
	})
}

func (s *Store) RecordPatchVerificationEvidence(ctx context.Context, binding pv.Binding, evidence pv.ExecutionEvidence) error {
	return writeVerificationOperationErr(ctx, s, "RecordPatchVerificationEvidence", func(ctx context.Context) error {
		return s.recordPatchVerificationEvidenceWrite(ctx, binding, evidence)
	})
}

func (s *Store) FinalizePatchVerificationRun(ctx context.Context, binding pv.Binding) (*pv.Record, error) {
	return writeVerificationOperation(ctx, s, "FinalizePatchVerificationRun", func(ctx context.Context) (*pv.Record, error) {
		return s.finalizePatchVerificationRunWrite(ctx, binding)
	})
}

func (s *Store) CancelPatchVerificationRun(ctx context.Context, binding pv.Binding) (*pv.Record, error) {
	return writeVerificationOperation(ctx, s, "CancelPatchVerificationRun", func(ctx context.Context) (*pv.Record, error) {
		return s.cancelPatchVerificationRunWrite(ctx, binding)
	})
}

func (s *Store) RecoverInterruptedPatchVerificationRun(ctx context.Context, binding pv.Binding) (*pv.Record, error) {
	return writeVerificationOperation(ctx, s, "RecoverInterruptedPatchVerificationRun", func(ctx context.Context) (*pv.Record, error) {
		return s.recoverInterruptedPatchVerificationRunWrite(ctx, binding)
	})
}

func (s *Store) CreateKubernetesValidationSubmission(ctx context.Context, submission pv.KubernetesSubmission) error {
	return writeVerificationOperationErr(ctx, s, "CreateKubernetesValidationSubmission", func(ctx context.Context) error {
		return s.createKubernetesValidationSubmissionWrite(ctx, submission)
	})
}

func (s *Store) CompleteKubernetesValidationSubmission(ctx context.Context, namespace, id, reason string) error {
	return writeVerificationOperationErr(ctx, s, "CompleteKubernetesValidationSubmission", func(ctx context.Context) error {
		return s.completeKubernetesValidationSubmissionWrite(ctx, namespace, id, reason)
	})
}

func (s *Store) RecordKubernetesValidationTaskUID(ctx context.Context, namespace, id, side, uid string) error {
	return writeVerificationOperationErr(ctx, s, "RecordKubernetesValidationTaskUID", func(ctx context.Context) error {
		return s.recordKubernetesValidationTaskUIDWrite(ctx, namespace, id, side, uid)
	})
}

func (s *Store) BindKubernetesValidationTasks(ctx context.Context, namespace, id, original, patched string, binding pv.Binding) error {
	return writeVerificationOperationErr(ctx, s, "BindKubernetesValidationTasks", func(ctx context.Context) error {
		return s.bindKubernetesValidationTasksWrite(ctx, namespace, id, original, patched, binding)
	})
}

func (s *Store) AttachCancelledKubernetesValidationRun(ctx context.Context, namespace, id string, binding pv.Binding) error {
	return writeVerificationOperationErr(ctx, s, "AttachCancelledKubernetesValidationRun", func(ctx context.Context) error {
		return s.attachCancelledKubernetesValidationRunWrite(ctx, namespace, id, binding)
	})
}

func (s *Store) RequestKubernetesValidationCancellation(ctx context.Context, namespace, id string) error {
	return writeVerificationOperationErr(ctx, s, "RequestKubernetesValidationCancellation", func(ctx context.Context) error {
		return s.requestKubernetesValidationCancellationWrite(ctx, namespace, id)
	})
}

func (s *Store) CreateKubernetesValidationDispatch(ctx context.Context, dispatch pv.KubernetesDispatch) error {
	return writeVerificationOperationErr(ctx, s, "CreateKubernetesValidationDispatch", func(ctx context.Context) error {
		return s.createKubernetesValidationDispatchWrite(ctx, dispatch)
	})
}

func (s *Store) MarkKubernetesValidationDispatchCreated(ctx context.Context, runID, side, checkID, jobUID string) error {
	return writeVerificationOperationErr(ctx, s, "MarkKubernetesValidationDispatchCreated", func(ctx context.Context) error {
		return s.markKubernetesValidationDispatchCreatedWrite(ctx, runID, side, checkID, jobUID)
	})
}

func (s *Store) MarkKubernetesValidationDispatchObserved(ctx context.Context, runID, side, checkID, jobUID, podUID, containerID string) error {
	return writeVerificationOperationErr(ctx, s, "MarkKubernetesValidationDispatchObserved", func(ctx context.Context) error {
		return s.markKubernetesValidationDispatchObservedWrite(ctx, runID, side, checkID, jobUID, podUID, containerID)
	})
}

func (s *Store) MarkKubernetesValidationDispatchRecorded(ctx context.Context, runID, side, checkID, jobUID, podUID string) error {
	return writeVerificationOperationErr(ctx, s, "MarkKubernetesValidationDispatchRecorded", func(ctx context.Context) error {
		return s.markKubernetesValidationDispatchRecordedWrite(ctx, runID, side, checkID, jobUID, podUID)
	})
}

func (s *Store) MarkKubernetesValidationDispatchFailed(ctx context.Context, runID, side, checkID, reason string) error {
	return writeVerificationOperationErr(ctx, s, "MarkKubernetesValidationDispatchFailed", func(ctx context.Context) error {
		return s.markKubernetesValidationDispatchFailedWrite(ctx, runID, side, checkID, reason)
	})
}
