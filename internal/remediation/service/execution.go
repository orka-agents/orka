package service

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/orka-agents/orka/internal/store"
)

type retryableBackendError interface {
	error
	Retryable() bool
}

type noEffectsBackendError interface {
	error
	NoEffects() bool
}

func (s *Coordinator) prepareExecution(ctx context.Context, r *run, key string, request contract.ExecutionRequest) (*executionOperation, error) {
	if request.EvidenceScope != r.state.Authority.EvidenceScope {
		return nil, uncertain()
	}
	if err := contract.RequireEvidenceScope(request.EvidenceScope, s.backendEvidenceScope()); err != nil {
		return nil, fail(contract.ReasonEvidenceScopeUnsupported)
	}
	digest, err := s.config.Backend.RequestDigest(request)
	if err != nil || store.ValidateCanonicalDigest("execution", digest) != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, contract.ErrEvidenceScopeUnsupported) {
			return nil, fail(contract.ReasonEvidenceScopeUnsupported)
		}
		return nil, invalidExecutionInput(request.Action)
	}
	op := r.state.Executions[key]
	if op == nil {
		reference, err := s.putJSON(ctx, r, request)
		if err != nil {
			return nil, err
		}
		r.state.Executions[key] = &executionOperation{Request: reference, InputDigest: digest}
		return nil, s.save(ctx, r)
	}
	if op.Rejected {
		return nil, invalidExecutionInput(request.Action)
	}
	if op.Settled {
		return nil, fail("execution-unavailable")
	}
	if op.InputDigest == "" {
		if op.Receipt != nil && op.Receipt.InputDigest != digest {
			return nil, uncertain()
		}
		op.InputDigest = digest
		return nil, s.save(ctx, r)
	}
	if op.InputDigest != digest {
		return nil, uncertain()
	}
	var saved contract.ExecutionRequest
	if err := s.readJSON(ctx, r, op.Request, &saved); err != nil {
		return nil, err
	}
	if !jsonEqual(saved, request) {
		return nil, uncertain()
	}
	return op, nil
}

func (s *Coordinator) execution(ctx context.Context, r *run, key string, request contract.ExecutionRequest) (*pv.Record, bool, error) {
	op, err := s.prepareExecution(ctx, r, key, request)
	if err != nil || op == nil {
		return nil, false, err
	}
	if !op.Record.empty() {
		var record pv.Record
		if err := s.readJSON(ctx, r, op.Record, &record); err != nil {
			return nil, false, err
		}
		if err := checkRecord(request, &record); err != nil {
			return nil, false, err
		}
		return &record, true, nil
	}
	var record *pv.Record
	starting := !op.Attempted
	if starting {
		if op.AdmissionAttempt == ^uint64(0) {
			return nil, false, uncertain()
		}
		op.AdmissionAttempt++
		op.Attempted = true
		if err := s.save(ctx, r); err != nil {
			return nil, false, err
		}
		if err := s.requireCurrentRun(ctx, r); err != nil {
			return nil, false, err
		}
		record, err = s.config.Backend.Start(ctx, request, func(receipt contract.ExecutionReceipt) error {
			return s.persistExecutionReceipt(ctx, r.state.ID, key, op, receipt)
		})
	} else {
		if op.Receipt == nil {
			if err := s.recoverExecution(ctx, r, key, op); err != nil {
				return nil, false, err
			}
			if op.Settled {
				return nil, false, fail("execution-unavailable")
			}
		}
		if err := s.requireCurrentRun(ctx, r); err != nil {
			return nil, false, err
		}
		record, err = s.config.Backend.Resume(ctx, request, *op.Receipt)
	}
	if starting {
		accounted, retry, accountErr := s.accountNoEffects(ctx, r, key, op, err)
		if accountErr != nil {
			return nil, false, accountErr
		}
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		if accounted {
			if retry {
				return nil, false, contract.ErrUnavailable
			}
			if errors.Is(err, contract.ErrInvalid) {
				return nil, false, invalidExecutionInput(request.Action)
			}
			return nil, false, executionFailure(err)
		}
	}
	if ctx.Err() != nil {
		return nil, false, ctx.Err()
	}
	current, loadErr := s.load(ctx, r.state.ID)
	if loadErr != nil {
		return nil, false, loadErr
	}
	currentOp := current.state.Executions[key]
	if currentOp == nil {
		return nil, false, uncertain()
	}
	if currentOp.Receipt == nil {
		if err := s.recoverExecution(ctx, current, key, currentOp); err != nil {
			return nil, false, err
		}
		if currentOp.Settled {
			return nil, false, fail("execution-unavailable")
		}
		return nil, false, contract.ErrUnavailable
	}
	if err != nil {
		return nil, false, executionFailure(err)
	}
	if err := checkRecord(request, record); err != nil {
		return nil, false, err
	}
	reference, err := s.putJSON(ctx, current, record)
	if err != nil {
		return nil, false, err
	}
	currentOp.Record = reference
	return nil, false, s.save(ctx, current)
}

func (s *Coordinator) persistExecutionReceipt(ctx context.Context, id, key string, expected *executionOperation, receipt contract.ExecutionReceipt) error {
	if receipt.Namespace != s.config.Namespace || receipt.TaskName == "" || receipt.TaskUID == "" ||
		receipt.InputDigest != expected.InputDigest {
		return contract.ErrConflict
	}
	_, err := s.update(ctx, id, func(current *run) error {
		op := current.state.Executions[key]
		if !sameExecutionAttempt(op, expected) ||
			(op.Receipt != nil && *op.Receipt != receipt) {
			return contract.ErrConflict
		}
		copy := receipt
		op.Receipt = &copy
		return nil
	})
	return err
}

func sameExecutionAttempt(op, expected *executionOperation) bool {
	return op != nil && op.Attempted && !op.Rejected && !op.Settled &&
		op.Request.Digest == expected.Request.Digest && op.InputDigest == expected.InputDigest &&
		op.AdmissionAttempt == expected.AdmissionAttempt
}

func (s *Coordinator) recoverExecution(ctx context.Context, r *run, key string, op *executionOperation) error {
	var request contract.ExecutionRequest
	if err := s.readJSON(ctx, r, op.Request, &request); err != nil {
		return err
	}
	digest, err := s.config.Backend.RequestDigest(request)
	if err != nil || digest != op.InputDigest || request.EvidenceScope != r.state.Authority.EvidenceScope {
		return uncertain()
	}
	if err := s.requireCurrentRun(ctx, r); err != nil {
		return err
	}
	recovered, err := s.config.Backend.Recover(ctx, request)
	if err != nil {
		if errors.Is(err, contract.ErrUnavailable) || ctx.Err() != nil {
			return contract.ErrUnavailable
		}
		return uncertain()
	}
	if recovered.Receipt != nil && !recovered.Settled {
		if err := s.persistExecutionReceipt(ctx, r.state.ID, key, op, *recovered.Receipt); err != nil {
			return err
		}
		copy := *recovered.Receipt
		op.Receipt = &copy
		return nil
	}
	if !recovered.Settled || recovered.Receipt != nil {
		return uncertain()
	}
	_, err = s.update(ctx, r.state.ID, func(current *run) error {
		actual := current.state.Executions[key]
		if !sameExecutionAttempt(actual, op) || actual.Receipt != nil || !actual.Record.empty() {
			return contract.ErrConflict
		}
		actual.Settled = true
		return nil
	})
	if err == nil {
		op.Settled = true
	}
	return err
}

func invalidExecutionInput(action pv.Action) error {
	if action == pv.VerifyPatch {
		return fail("candidate-invalid")
	}
	return fail("checks-invalid")
}

func executionFailure(err error) error {
	if errors.Is(err, contract.ErrEvidenceScopeUnsupported) {
		return fail(contract.ReasonEvidenceScopeUnsupported)
	}
	if retryable, ok := errors.AsType[retryableBackendError](err); ok && retryable.Retryable() {
		return contract.ErrUnavailable
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &stageFailure{
			phase: contract.PhaseTimedOut, outcome: contract.OutcomeTimedOut, reason: reasonDeadlineExceeded,
		}
	}
	return fail("execution-unavailable")
}

func jsonEqual(first, second any) bool {
	a, aErr := json.Marshal(first)
	b, bErr := json.Marshal(second)
	return aErr == nil && bErr == nil && string(a) == string(b)
}

// checkRecord independently recomputes conclusions from exact bound
// observations. Neither a model verdict nor a record's summary is authority.
func checkRecord(request contract.ExecutionRequest, record *pv.Record) error {
	if (request.EvidenceScope != "" && !request.EvidenceScope.Valid()) ||
		record == nil || record.State != pv.RunFinalized || record.Seal == nil || len(record.Incidents) != 0 ||
		len(record.Seal.Content) == 0 || record.Seal.Digest != pv.Digest(record.Seal.Content) ||
		pv.ValidateRunBinding(record.Manifest, record.Binding) != nil {
		return fail("original-evidence-invalid")
	}
	manifest := &record.Manifest
	if !validRecordScope(request, manifest) {
		return fail("original-evidence-invalid")
	}
	if !manifestMatchesRequest(manifest, request) {
		return fail("original-evidence-invalid")
	}
	if request.Action == pv.VerifyPatch && manifest.Sources.PatchDigest != pv.Digest([]byte(request.Patch)) {
		return fail("original-evidence-invalid")
	}
	if request.Action == pv.ValidateReport && (request.Patch != "" || manifest.Sources.PatchDigest != "") {
		return fail("original-evidence-invalid")
	}
	observations := make([]pv.Observation, 0, len(record.Evidence))
	seen := make(map[string]bool)
	for _, evidence := range record.Evidence {
		encoded, err := json.Marshal(evidence.Observation)
		key := evidence.Observation.Side + ":" + evidence.Observation.CheckID
		if err != nil || evidence.Digest != pv.Digest(encoded) || evidence.Rejection != "" || seen[key] ||
			pv.ValidateObservation(record.Manifest, record.Binding, evidence.Observation) != nil {
			return fail("original-evidence-invalid")
		}
		seen[key] = true
		observations = append(observations, evidence.Observation)
	}
	expected := pv.Evaluate(record.Manifest, record.Binding, observations)
	if !reflect.DeepEqual(record.Assessment, expected) || !reflect.DeepEqual(record.Seal.Assessment, expected) ||
		!modelEvidenceWithinBudget(record) {
		return fail("original-evidence-invalid")
	}
	return nil
}

func validRecordScope(request contract.ExecutionRequest, manifest *pv.Manifest) bool {
	if request.EvidenceScope == contract.ApplicationRuntime {
		return request.Deadline != nil && !request.Deadline.IsZero() &&
			request.Checks.Application != nil && manifest.Application != nil &&
			manifest.Environment.Profile == pv.ApplicationRuntimeProfile
	}
	return request.Checks.Application == nil && manifest.Application == nil &&
		request.ApplicationPolicy == nil && manifest.ApplicationPolicy == nil
}

func manifestMatchesRequest(manifest *pv.Manifest, request contract.ExecutionRequest) bool {
	return manifest.Action == request.Action && manifest.Problem == request.Problem &&
		manifest.Sources.Repository == request.Source.Repository && manifest.Sources.Original.Commit == request.Source.Commit &&
		manifest.Sources.Original.Tree == request.Source.Tree &&
		slices.Equal(manifest.Sources.Paths, request.Source.Paths) &&
		jsonEqual(manifest.Scope, request.Checks.Scope) && jsonEqual(manifest.Gaps, request.Checks.Gaps) &&
		jsonEqual(manifest.Checks, request.Checks.Checks) && jsonEqual(manifest.Files, request.Checks.Files) &&
		jsonEqual(manifest.Application, request.Checks.Application) &&
		jsonEqual(manifest.ApplicationPolicy, request.ApplicationPolicy) &&
		jsonEqual(manifest.Environment.Requirements, request.Checks.Requirements) &&
		jsonEqual(manifest.Environment.Services, request.Checks.Services) &&
		manifest.Environment.Dependencies[contract.EvidenceScopeKey] == string(request.EvidenceScope) &&
		jsonEqual(manifest.DeclaredChanges, request.DeclaredChanges)
}

func originalReady(record *pv.Record) bool {
	if record == nil {
		return false
	}
	observations := make([]pv.Observation, 0, len(record.Evidence))
	for _, evidence := range record.Evidence {
		observations = append(observations, evidence.Observation)
	}
	return pv.OriginalReady(record.Manifest, record.Binding, observations)
}
