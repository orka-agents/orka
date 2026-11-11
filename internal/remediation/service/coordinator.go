package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/orka-agents/orka/internal/remediation/intake"
	"github.com/orka-agents/orka/internal/remediation/source"
)

func (s *Coordinator) drive(operation, manager context.Context, owner runIdentity) {
	// The queued UID, not a later same-name read, defines the entire driver
	// lifetime. Derived cancellation/bookkeeping contexts retain this fence.
	operation = bindRunIdentity(operation, owner)
	manager = bindRunIdentity(manager, owner)
	id := owner.id
	r, err := s.load(operation, id)
	if err != nil {
		return
	}
	if err := s.acquireRunSlot(operation, r); err != nil {
		return
	}
	ctx, cancel := context.WithDeadline(operation, r.state.Deadline)
	defer cancel()
	watchDone := make(chan struct{})
	go s.watchRunCancellation(ctx, id, cancel, watchDone)
	defer func() { cancel(); <-watchDone }()
	for manager.Err() == nil {
		if err := s.flushNoEffects(manager, id); err != nil {
			if errors.Is(err, errNoEffectsObsolete) || errors.Is(err, errRunIdentityChanged) {
				return
			}
			s.wait(manager)
			continue
		}
		r, err = s.load(manager, id)
		if err != nil {
			return
		}
		if r.state.Final != nil && !r.state.ResultPublished && !r.state.ParentDeleted {
			if err := s.publishCompletion(manager, r); err != nil {
				if errors.Is(err, errRunIdentityChanged) {
					return
				}
				if errors.Is(err, errAggregateParentUnavailable) {
					s.stop(manager, id, reasonIdentityConflict)
					return
				}
				s.wait(manager)
			}
			continue
		}
		if r.state.CancelRequested || ctx.Err() != nil {
			reason := r.state.StopReason
			if reason == "" {
				reason = reasonDeadlineExceeded
			}
			s.stop(manager, id, reason)
			return
		}
		if contract.TerminalPhase(r.state.Status.Phase) {
			if !r.state.Settled {
				s.stop(manager, id, r.state.StopReason)
			}
			return
		}
		identity, err := s.agentAuthority(ctx)
		if err != nil {
			if errors.Is(err, contract.ErrInvalid) {
				s.stop(manager, id, "agent-configuration-changed")
				return
			}
			s.wait(ctx)
			continue
		}
		if !authorityMatches(r.state.Authority, identity) {
			s.stop(manager, id, "agent-configuration-changed")
			return
		}
		err = s.advance(ctx, r)
		if errors.Is(err, errNoEffectsObsolete) || errors.Is(err, errRunIdentityChanged) {
			return
		}
		if ctx.Err() != nil {
			continue
		}
		if failure, ok := errors.AsType[*stageFailure](err); ok {
			s.stopAfterStageFailure(manager, id, failure)
			return
		}
		if err != nil && !pending(err) {
			s.stop(manager, id, "execution-unavailable")
			return
		}
		s.wait(ctx)
	}
}

func (s *Coordinator) stopAfterStageFailure(ctx context.Context, id string, failure *stageFailure) {
	_, err := s.update(ctx, id, func(current *run) error {
		if current.state.CancelRequested {
			return nil
		}
		outcome := failure.outcome
		if current.state.Status.PatchReady {
			outcome = contract.OutcomeUnverified
		}
		setCompletion(current, failure.phase, outcome, failure.reason)
		return nil
	})
	if err == nil {
		s.stop(ctx, id, failure.reason)
	}
}

func (s *Coordinator) watchRunCancellation(ctx context.Context, id string, cancel context.CancelFunc, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(s.config.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			current, err := s.load(ctx, id)
			if errors.Is(err, errRunIdentityChanged) {
				cancel()
				return
			}
			if err == nil && current.state.CancelRequested {
				cancel()
				return
			}
		}
	}
}

func (s *Coordinator) wait(ctx context.Context) {
	timer := time.NewTimer(s.config.PollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func (s *Coordinator) advance(ctx context.Context, r *run) error {
	if err := contract.RequireEvidenceScope(r.state.Authority.EvidenceScope, s.backendEvidenceScope()); err != nil {
		return fail(contract.ReasonEvidenceScopeUnsupported)
	}
	parent, ready, err := s.ensureTask(ctx, r, parentRole, "")
	if err != nil || !ready {
		return err
	}
	if _, cancelling := parent.Annotations[labels.AnnotationCancelRequested]; cancelling {
		_, err := s.Cancel(ctx, r.state.Namespace, r.state.ID)
		return err
	}
	if r.state.Status.Phase == contract.PhasePending {
		r.state.Status.Phase = contract.PhaseRunning
		return s.save(ctx, r)
	}
	if parent.Status.Phase != corev1alpha1.TaskPhaseRunning {
		return s.projectParent(ctx, r, parent)
	}
	report, err := intake.Parse(r.input.Report)
	if err != nil {
		return fail("agent-output-invalid")
	}
	switch r.state.Step {
	case stepSource:
		return s.resolveSource(ctx, r)
	case "select":
		return s.selectSource(ctx, r, report)
	case "packet":
		return s.readPacket(ctx, r)
	case "checks":
		return s.proposeChecks(ctx, r, report)
	case "baseline":
		return s.runChecks(ctx, r, report, false)
	case contract.StageReproduce:
		return s.assessReproduction(ctx, r, report)
	case contract.StageGenerate:
		return s.generateCandidate(ctx, r, report)
	case stepCompare:
		return s.runChecks(ctx, r, report, true)
	case "verify":
		return s.assessCandidate(ctx, r, report)
	default:
		return fail("state-conflict")
	}
}

func (s *Coordinator) resolveSource(ctx context.Context, r *run) error {
	if r.state.Source == nil {
		target, err := s.config.Source.Resolve(ctx, r.input.Repository, r.input.Ref)
		if err != nil {
			return fail("source-unavailable")
		}
		r.state.Source = &target
		// Ref resolution is committed before any inventory or agent operation.
		return s.save(ctx, r)
	}
	inventory, err := s.config.Source.Inventory(ctx, *r.state.Source)
	if err != nil {
		return fail("source-unavailable")
	}
	encoded, err := json.Marshal(inventory)
	if err != nil || len(encoded) > 512<<10 {
		return fail("source-too-large")
	}
	reference, err := s.putBlob(ctx, r, encoded)
	if err != nil {
		return err
	}
	r.state.Inventory, r.state.Step = reference, "select"
	return s.save(ctx, r)
}

func (s *Coordinator) selectSource(ctx context.Context, r *run, report intake.Report) error {
	var inventory []source.Entry
	if err := s.readJSON(ctx, r, r.state.Inventory, &inventory); err != nil {
		return err
	}
	output, ready, err := s.agentOutput(ctx, r, "select", selectionPrompt(report, *r.state.Source, inventory))
	if err != nil || !ready {
		return err
	}
	selection, err := remediation.DecodeSourceSelection(output)
	if err != nil {
		return fail("agent-output-invalid")
	}
	kept, invalid, tooLarge := boundedSelectedFiles(inventory, selection.Files)
	if invalid {
		return fail("agent-output-invalid")
	}
	if tooLarge || len(kept) == 0 {
		return fail("source-too-large")
	}
	r.state.Step = "packet"
	return s.save(ctx, r)
}

func (s *Coordinator) readPacket(ctx context.Context, r *run) error {
	selectionTask := r.state.Agents["select"]
	if selectionTask == nil {
		return fail("state-conflict")
	}
	var inventory []source.Entry
	if err := s.readJSON(ctx, r, r.state.Inventory, &inventory); err != nil {
		return err
	}
	data, err := s.readBlob(ctx, r, selectionTask.Output)
	if err != nil {
		return err
	}
	selection, err := remediation.DecodeSourceSelection(string(data))
	if err != nil {
		return fail("agent-output-invalid")
	}
	selectedFiles, invalid, tooLarge := boundedSelectedFiles(inventory, selection.Files)
	if invalid {
		return fail("agent-output-invalid")
	}
	if tooLarge || len(selectedFiles) == 0 {
		return fail("source-too-large")
	}
	packet, err := s.config.Source.Packet(ctx, *r.state.Source, selectedFiles)
	if err != nil {
		return fail("source-unavailable")
	}
	reference, err := s.putJSON(ctx, r, packet)
	if err != nil {
		return err
	}
	r.state.Packet, r.state.Step = reference, "checks"
	return s.save(ctx, r)
}

func (s *Coordinator) proposeChecks(ctx context.Context, r *run, report intake.Report) error {
	var packet source.Packet
	if err := s.readJSON(ctx, r, r.state.Packet, &packet); err != nil {
		return err
	}
	instructions := checksPrompt(report, packet)
	var policy *pv.ApplicationPolicy
	if r.state.Authority.EvidenceScope == contract.ApplicationRuntime {
		backend, ok := s.config.Backend.(contract.ApplicationPlanningBackend)
		if !ok {
			return fail("environment-unsupported")
		}
		policy = backend.ApplicationPolicy()
		instructions = applicationChecksPrompt(report, packet, backend.ApplicationPlanDefaults(), policy)
	}
	output, ready, err := s.agentOutput(ctx, r, "checks", instructions)
	if err != nil || !ready {
		return err
	}
	var proposal remediation.CheckProposal
	if r.state.Authority.EvidenceScope == contract.ApplicationRuntime {
		proposal, err = remediation.DecodeApplicationCheckProposal(output, policy)
	} else {
		proposal, err = remediation.DecodeCheckProposal(output)
	}
	if err != nil {
		return fail("checks-invalid")
	}
	if err := s.freezeChecks(ctx, r, proposal); err != nil {
		return err
	}
	r.state.Step = "baseline"
	return s.save(ctx, r)
}

func (s *Coordinator) freezeChecks(ctx context.Context, r *run, proposal remediation.CheckProposal) error {
	// Revised proposals share the same strict schema, policy and bounds checks.
	raw, err := json.Marshal(proposal)
	if err != nil {
		return fail("checks-invalid")
	}
	if (proposal.Application != nil) != (r.state.Authority.EvidenceScope == contract.ApplicationRuntime) {
		return fail(contract.ReasonEvidenceScopeUnsupported)
	}
	var policy *pv.ApplicationPolicy
	if r.state.Authority.EvidenceScope == contract.ApplicationRuntime {
		backend, ok := s.config.Backend.(contract.ApplicationPlanningBackend)
		if !ok {
			return fail("environment-unsupported")
		}
		policy = backend.ApplicationPolicy()
		proposal, err = remediation.DecodeApplicationCheckProposal(string(raw), policy)
	} else {
		proposal, err = remediation.DecodeCheckProposal(string(raw))
	}
	if err != nil {
		return fail("checks-invalid")
	}
	if proposal.Application != nil && len(proposal.Gaps) != 0 {
		return fail("environment-unsupported")
	}
	if !r.state.Checks.empty() {
		var previous contract.Checks
		if err := s.readJSON(ctx, r, r.state.Checks, &previous); err != nil {
			return err
		}
		if !preservesCheckCoverage(previous, proposal) {
			return fail("validation-invalid")
		}
	}
	var files []pv.FrozenFile
	if proposal.Application != nil {
		files, err = remediation.FreezeApplicationProposalFiles(proposal, policy)
	} else {
		files, err = remediation.FreezeProposalFiles(proposal)
	}
	if err != nil {
		return fail("environment-unsupported")
	}
	checks := contract.Checks{
		Scope: proposal.Scope, Gaps: proposal.Gaps, Requirements: proposal.Requirements,
		Files: files, Checks: proposal.Checks, Services: proposal.Services, Application: proposal.Application,
	}
	reference, err := s.putJSON(ctx, r, checks)
	if err != nil {
		return err
	}
	r.state.Checks = reference
	return nil
}

func (s *Coordinator) runChecks(ctx context.Context, r *run, report intake.Report, candidate bool) error {
	var checks contract.Checks
	if err := s.readJSON(ctx, r, r.state.Checks, &checks); err != nil {
		return err
	}
	problem, err := json.Marshal(report)
	if err != nil {
		return fail("checks-invalid")
	}
	key, action := "original", pv.ValidateReport
	if candidate {
		key, action = fmt.Sprintf("compare-%d", r.state.ReviewRound), pv.VerifyPatch
	}
	request := contract.ExecutionRequest{
		ID: r.object.Name + "-" + key, Namespace: s.config.Namespace,
		ParentTaskName: r.state.Parent.Name, ParentTaskUID: r.state.Parent.UID,
		Action: action, Problem: string(problem),
		Source: contract.Source{Repository: r.state.Source.Repository.URL, Commit: r.state.Source.Commit, Tree: r.state.Source.Tree},
		Checks: checks, EvidenceScope: r.state.Authority.EvidenceScope,
	}
	if request.EvidenceScope == contract.ApplicationRuntime {
		deadline := r.state.Deadline
		request.Deadline = &deadline
		backend, ok := s.config.Backend.(contract.ApplicationPlanningBackend)
		if !ok {
			return fail("environment-unsupported")
		}
		request.ApplicationPolicy = backend.ApplicationPolicy()
	}
	if candidate {
		patch, err := s.readBlob(ctx, r, r.state.Candidate)
		if err != nil {
			return err
		}
		request.Patch, request.DeclaredChanges = string(patch), r.state.Changes
	}
	var packet source.Packet
	if err := s.readJSON(ctx, r, r.state.Packet, &packet); err != nil {
		return err
	}
	var inventory []source.Entry
	if err := s.readJSON(ctx, r, r.state.Inventory, &inventory); err != nil {
		return err
	}
	if request.EvidenceScope != contract.ApplicationRuntime {
		request.Source.Paths = scopedPreparationPaths(inventory, packet, request.Patch)
	}
	if err := preflightPromptEnvelopes(report, packet, request, r.input.Mode, r.state.ReviewRound != 0); err != nil {
		return err
	}
	record, ready, err := s.execution(ctx, r, key, request)
	if err != nil || !ready {
		return err
	}
	if candidate {
		r.state.Comparison, r.state.Step = r.state.Executions[key].Record, "verify"
	} else {
		r.state.Baseline, r.state.Step = r.state.Executions[key].Record, contract.StageReproduce
	}
	if record == nil {
		return fail("original-evidence-invalid")
	}
	return s.save(ctx, r)
}

func preservesCheckCoverage(previous contract.Checks, revised remediation.CheckProposal) bool {
	for _, scope := range previous.Scope {
		if !slices.Contains(revised.Scope, scope) {
			return false
		}
	}
	for _, gap := range previous.Gaps {
		if !slices.Contains(revised.Gaps, gap) {
			return false
		}
	}
	for _, check := range previous.Checks {
		if !slices.ContainsFunc(revised.Checks, func(next pv.Check) bool {
			if next.ID != check.ID || next.Kind != check.Kind {
				return false
			}
			return previous.Application == nil || (check.Application != nil && next.Application != nil &&
				jsonEqual(check.Application.Resources, next.Application.Resources) &&
				jsonEqual(check.Application.Actions, next.Application.Actions))
		}) {
			return false
		}
	}
	if previous.Application != nil {
		for _, next := range revised.Checks {
			if !slices.ContainsFunc(previous.Checks, func(check pv.Check) bool { return check.ID == next.ID }) &&
				(next.Application == nil || len(next.Application.Resources) != 0 || len(next.Application.Actions) != 0) {
				return false
			}
		}
	}
	// Review may repair probes, not replace the application, its deployment,
	// fixtures, or authority to make a previously failing report disappear.
	return jsonEqual(previous.Application, revised.Application)
}

func (s *Coordinator) assessReproduction(ctx context.Context, r *run, report intake.Report) error {
	var packet source.Packet
	var record pv.Record
	if err := s.readJSON(ctx, r, r.state.Packet, &packet); err != nil {
		return err
	}
	if err := s.readJSON(ctx, r, r.state.Baseline, &record); err != nil {
		return err
	}
	output, ready, err := s.agentOutput(ctx, r, contract.StageReproduce, reproductionPrompt(report, packet, &record))
	if err != nil || !ready {
		return err
	}
	review, err := remediation.DecodeReproductionReview(output)
	if err != nil {
		return fail("agent-output-invalid")
	}
	if !originalReady(&record) {
		if record.Assessment.Conclusion == pv.NotReproduced {
			return s.finish(ctx, r, contract.PhaseSucceeded, contract.OutcomeNotReproduced, "original-not-reproduced")
		}
		return s.finish(ctx, r, contract.PhaseFailed, contract.OutcomeUnableToValidate, "original-setup-failed")
	}
	if !*review.Reproduced {
		return s.finish(ctx, r, contract.PhaseFailed, contract.OutcomeUnableToValidate, "validation-rejected")
	}
	switch r.input.Mode {
	case contract.Validate:
		return s.finish(ctx, r, contract.PhaseSucceeded, contract.OutcomeReproduced, "report-reproduced")
	case contract.Generate:
		r.state.Step, r.state.Status.Stage = contract.StageGenerate, contract.StageGenerate
	default:
		return fail("state-conflict")
	}
	return s.save(ctx, r)
}

func (s *Coordinator) generateCandidate(ctx context.Context, r *run, report intake.Report) error {
	var packet source.Packet
	var record pv.Record
	if err := s.readJSON(ctx, r, r.state.Packet, &packet); err != nil {
		return err
	}
	if err := s.readJSON(ctx, r, r.state.Baseline, &record); err != nil {
		return err
	}
	if !originalReady(&record) {
		return fail("original-evidence-invalid")
	}
	output, ready, err := s.agentOutput(ctx, r, contract.StageGenerate, generationPrompt(report, packet, &record))
	if err != nil || !ready {
		return err
	}
	proposal, err := remediation.DecodePatchProposal(output)
	if err != nil {
		return fail("candidate-invalid")
	}
	files := make(map[string][]byte, len(packet.Files))
	for _, file := range packet.Files {
		files[file.Path] = []byte(file.Content)
	}
	patch, err := remediation.CompilePatch(proposal, files)
	if err != nil || len(patch) > MaxPatchBytes || (pv.CredentialMatcher{}).MatchString(patch) {
		return fail("candidate-invalid")
	}
	reference, err := s.putBlob(ctx, r, []byte(patch))
	if err != nil {
		return err
	}
	r.state.Candidate, r.state.Changes, r.state.Status.PatchReady = reference, proposal.DeclaredChanges, true
	r.state.Step, r.state.Status.Stage = stepCompare, contract.StageVerify
	return s.save(ctx, r)
}

func (s *Coordinator) assessCandidate(ctx context.Context, r *run, report intake.Report) error {
	var packet source.Packet
	var record pv.Record
	if err := s.readJSON(ctx, r, r.state.Packet, &packet); err != nil {
		return err
	}
	if err := s.readJSON(ctx, r, r.state.Comparison, &record); err != nil {
		return err
	}
	patch, err := s.readBlob(ctx, r, r.state.Candidate)
	if err != nil {
		return err
	}
	role := fmt.Sprintf("verify-%d", r.state.ReviewRound)
	output, ready, err := s.agentOutput(ctx, r, role, verificationPrompt(report, packet, string(patch), &record, r.state.ReviewRound != 0))
	if err != nil || !ready {
		return err
	}
	review, err := remediation.DecodeCandidateReview(output)
	if err != nil {
		return fail("validation-invalid")
	}
	if review.RevisedChecks != nil {
		if r.state.ReviewRound != 0 {
			return fail("validation-invalid")
		}
		previous := r.state.Checks.Digest
		if err := s.freezeChecks(ctx, r, *review.RevisedChecks); err != nil {
			return err
		}
		if r.state.Checks.Digest == previous {
			return fail("validation-invalid")
		}
		r.state.ReviewRound, r.state.Step = 1, stepCompare
		return s.save(ctx, r)
	}
	if *review.Approved && originalReady(&record) && record.Assessment.Conclusion == pv.Verified {
		return s.finish(ctx, r, contract.PhaseSucceeded, contract.OutcomeVerified, "verified")
	}
	reason := "candidate-unverified"
	switch record.Assessment.Conclusion {
	case pv.NotFixed, pv.PartiallyFixed:
		reason = "candidate-not-fixed"
	case pv.Regression:
		reason = "candidate-regression"
	case pv.Verified:
		reason = "validation-rejected"
	}
	return s.finish(ctx, r, contract.PhaseFailed, contract.OutcomeUnverified, reason)
}

func (s *Coordinator) finish(ctx context.Context, r *run, phase, outcome, reason string) error {
	setCompletion(r, phase, outcome, reason)
	return s.save(ctx, r)
}

func setCompletion(r *run, phase, outcome, reason string) {
	if r.state.Final == nil {
		r.state.Final = &completion{Phase: phase, Outcome: outcome, Reason: reason}
		r.state.Status.Stage = contract.StageComplete
		r.state.Settled = false
		r.state.StopReason = reason
	}
}

func (s *Coordinator) stop(ctx context.Context, id, reason string) {
	r, err := s.update(ctx, id, func(r *run) error {
		if r.state.StopReason == "" {
			r.state.StopReason = reason
		}
		r.state.CancelRequested = true
		return nil
	})
	if err != nil {
		return
	}
	settled := true
	for _, op := range r.state.Agents {
		done, err := s.cancelAgent(ctx, r, op)
		settled = settled && done && err == nil
	}
	// Backend cleanup must still run while agent cleanup is pending.
	settled = s.settleExecutions(ctx, r) && settled
	if !settled || ctx.Err() != nil {
		return
	}
	current, err := s.update(ctx, id, func(current *run) error {
		if !contract.TerminalPhase(current.state.Status.Phase) && current.state.Final == nil {
			phase, outcome := contract.PhaseFailed, contract.OutcomeUnableToValidate
			switch current.state.StopReason {
			case reasonCancelled:
				phase, outcome = contract.PhaseCancelled, contract.OutcomeCancelled
			case reasonDeadlineExceeded:
				phase, outcome = contract.PhaseTimedOut, contract.OutcomeTimedOut
			default:
				if current.state.Status.PatchReady {
					outcome = contract.OutcomeUnverified
				}
			}
			setCompletion(current, phase, outcome, current.state.StopReason)
		}
		return nil
	})
	if err != nil {
		return
	}
	if !contract.TerminalPhase(current.state.Status.Phase) {
		if err := s.publishCompletion(ctx, current); err != nil {
			return
		}
		current, err = s.load(ctx, id)
		if err != nil || !contract.TerminalPhase(current.state.Status.Phase) {
			return
		}
	}
	if settled, err := s.settleParent(ctx, current); err != nil || !settled {
		return
	}
	current, err = s.update(ctx, id, func(current *run) error {
		current.state.Settled = true
		return nil
	})
	if err == nil {
		_ = s.releaseRunSlot(ctx, current)
	}
}

func (s *Coordinator) settleExecutions(ctx context.Context, r *run) bool {
	settled := true
	for key, op := range r.state.Executions {
		if !op.Attempted || op.Rejected || op.Settled || !op.Record.empty() {
			continue
		}
		if op.Receipt == nil {
			if err := s.recoverExecution(ctx, r, key, op); err != nil {
				settled = false
				continue
			}
			if op.Settled {
				continue
			}
		}
		if err := s.requireCurrentRun(ctx, r); err != nil {
			return false
		}
		if err := s.config.Backend.Cancel(ctx, *op.Receipt); err != nil {
			settled = false
		}
	}
	return settled
}

func boundedSelectedFiles(inventory []source.Entry, selected []string) ([]string, bool, bool) {
	sizes := make([]int64, len(selected))
	for i, name := range selected {
		index := slices.IndexFunc(inventory, func(entry source.Entry) bool {
			return entry.Path == name && (entry.Mode == "100644" || entry.Mode == "100755")
		})
		if index < 0 || inventory[index].Size < 1 {
			return nil, true, false
		}
		sizes[i] = inventory[index].Size
	}
	if len(selected) == 1 {
		if sizes[0] > 64<<10 {
			return nil, false, true
		}
		return slices.Clone(selected), false, false
	}
	const multiFileBudget = 44 << 10
	kept := make([]string, 0, len(selected))
	total := int64(0)
	for i, name := range selected {
		limit := int64(multiFileBudget)
		if len(kept) == 0 {
			limit = 64 << 10
		}
		if total+sizes[i] > limit {
			continue
		}
		kept = append(kept, name)
		total += sizes[i]
	}
	if len(kept) == 0 {
		return nil, false, true
	}
	return kept, false, false
}

func scopedPreparationPaths(inventory []source.Entry, packet source.Packet, patch string) []string {
	total := int64(0)
	scoped := false
	regular := make(map[string]bool, len(inventory))
	for _, entry := range inventory {
		if entry.Mode == "040000" {
			scoped = true
			continue
		}
		if entry.Mode == "100644" || entry.Mode == "100755" {
			regular[entry.Path] = true
			total += entry.Size
			if total > pv.MaxPublicSourceArchiveBytes/2 {
				scoped = true
			}
		}
	}
	if !scoped {
		return nil
	}
	seen := make(map[string]bool, len(packet.Files))
	paths := make([]string, 0, len(packet.Files))
	for _, file := range packet.Files {
		if regular[file.Path] && !seen[file.Path] {
			seen[file.Path] = true
			paths = append(paths, file.Path)
		}
	}
	for _, name := range patchReferencedPaths(patch) {
		if regular[name] && !seen[name] && len(paths) < 32 {
			seen[name] = true
			paths = append(paths, name)
		}
	}
	slices.Sort(paths)
	return paths
}

func patchReferencedPaths(patch string) []string {
	var paths []string
	for line := range strings.SplitSeq(patch, "\n") {
		line = strings.TrimSuffix(line, "\r")
		for _, prefix := range []string{"--- a/", "+++ b/"} {
			if candidate, ok := strings.CutPrefix(line, prefix); ok && candidate != "/dev/null" {
				paths = append(paths, candidate)
			}
		}
	}
	return paths
}
