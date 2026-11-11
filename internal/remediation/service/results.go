package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/workers/common"
)

const MaxAggregateResultBytes = 1 << 20

var errAggregateParentUnavailable = errors.New("aggregate parent identity unavailable")

func (s *Coordinator) publishCompletion(ctx context.Context, r *run) error {
	if r.state.Final == nil {
		return contract.ErrConflict
	}
	if r.state.ParentDeleted {
		return s.completeDeletedParent(ctx, r)
	}
	parent, ready, err := s.ensureTask(ctx, r, parentRole, "")
	if err != nil {
		if errors.Is(err, contract.ErrUnavailable) {
			return err
		}
		return s.unavailableAggregateParent(ctx, r)
	}
	if !ready {
		return nil
	}
	if r.state.Result.empty() {
		result, err := s.aggregateResult(ctx, r)
		if err != nil {
			return err
		}
		reference, err := s.putBlob(ctx, r, result)
		if err != nil {
			return err
		}
		r.state.Result = reference
		return s.save(ctx, r)
	}
	result, err := s.readBlob(ctx, r, r.state.Result)
	if err != nil {
		return err
	}
	existing, err := s.config.Results.GetResult(ctx, r.state.Namespace, parent.Name)
	switch {
	case err == nil:
		if !bytes.Equal(existing, result) {
			return s.markAggregateUnavailable(ctx, r)
		}
	case errors.Is(err, store.ErrNotFound):
		if err := s.requireCurrentRun(ctx, r); err != nil {
			return err
		}
		if err := s.config.Results.SaveResult(ctx, r.state.Namespace, parent.Name, result); err != nil {
			return contract.ErrUnavailable
		}
	default:
		return contract.ErrUnavailable
	}
	after := new(corev1alpha1.Task)
	if err := s.config.Reader.Get(ctx, client.ObjectKeyFromObject(parent), after); err != nil {
		if apierrors.IsNotFound(err) {
			return s.unavailableAggregateParent(ctx, r)
		}
		return contract.ErrUnavailable
	}
	if !taskMatches(after, parent, r.state.Parent.UID) {
		return s.unavailableAggregateParent(ctx, r)
	}
	if after.Status.ResultRef == nil || !after.Status.ResultRef.Available {
		if err := s.requireCurrentRun(ctx, r); err != nil {
			return err
		}
		after.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
		after.Status.Phase = corev1alpha1.TaskPhaseFinalizing
		if err := s.config.Client.Status().Update(ctx, after); err != nil {
			return contract.ErrUnavailable
		}
	}
	// The ordinary Task result and its availability marker precede the
	// externally visible terminal run status. A lost write acknowledgement
	// retries the same bytes, never overwrites a different name-only result.
	_, err = s.update(ctx, r.state.ID, func(current *run) error {
		if current.state.Parent.UID != string(parent.UID) || current.state.ParentDeleted ||
			current.state.Result.Digest != r.state.Result.Digest || !jsonEqual(current.state.Final, r.state.Final) {
			return contract.ErrConflict
		}
		current.state.ResultPublished = true
		applyCompletion(current)
		return nil
	})
	return err
}

func applyCompletion(r *run) {
	r.state.Status.Phase, r.state.Status.Outcome, r.state.Status.Reason =
		r.state.Final.Phase, r.state.Final.Outcome, r.state.Final.Reason
	r.state.Status.Stage, r.state.Step = contract.StageComplete, "complete"
}

func (s *Coordinator) unavailableAggregateParent(ctx context.Context, r *run) error {
	if r.state.Parent.UID != "" {
		parent := new(corev1alpha1.Task)
		err := s.config.Reader.Get(ctx, client.ObjectKey{Namespace: r.state.Namespace, Name: r.state.Parent.Name}, parent)
		if apierrors.IsNotFound(err) || (err == nil && string(parent.UID) == r.state.Parent.UID && parent.DeletionTimestamp != nil) {
			r.state.ParentDeleted = true
			return s.completeDeletedParent(ctx, r)
		}
		if err != nil {
			return contract.ErrUnavailable
		}
	}
	return s.markAggregateUnavailable(ctx, r)
}

func (s *Coordinator) markAggregateUnavailable(ctx context.Context, r *run) error {
	_, err := s.update(ctx, r.state.ID, func(current *run) error {
		outcome := contract.OutcomeUnableToValidate
		if current.state.Status.PatchReady {
			outcome = contract.OutcomeUnverified
		}
		current.state.Final = &completion{Phase: contract.PhaseNeedsInput, Outcome: outcome, Reason: reasonIdentityConflict}
		current.state.CancelRequested, current.state.StopReason = true, reasonIdentityConflict
		applyCompletion(current)
		return nil
	})
	if err != nil {
		return err
	}
	return errAggregateParentUnavailable
}

func (s *Coordinator) completeDeletedParent(ctx context.Context, r *run) error {
	_, err := s.update(ctx, r.state.ID, func(current *run) error {
		current.state.ParentDeleted, current.state.CancelRequested = true, true
		if !current.state.ResultPublished {
			current.state.Final = &completion{Phase: contract.PhaseCancelled, Outcome: contract.OutcomeCancelled, Reason: reasonCancelled}
			current.state.StopReason = reasonCancelled
			applyCompletion(current)
		}
		return nil
	})
	return err
}

func (s *Coordinator) aggregateResult(ctx context.Context, r *run) ([]byte, error) {
	result := &common.StructuredResult{
		Version: 1, Summary: completionSummary(r.state.Final.Outcome), Verdict: r.state.Final.Outcome,
	}
	result.Summary += "\n" + evidenceScopeSummary(r.state.Authority.EvidenceScope)
	if r.state.Authority.BoundedExecution {
		used := 0
		for _, operation := range r.state.Agents {
			if operation.ModelCall != nil {
				used++
			}
		}
		result.Summary += fmt.Sprintf("\nOrka model-invocation permits consumed: %d/%d (one native HTTP attempt per permit). Gateway-internal upstream retries are outside this logical budget.", used, contract.MaxModelCalls)
	}
	if r.state.Source != nil {
		result.BaseSHA = r.state.Source.Commit
		result.Summary += fmt.Sprintf("\nSource: %s @ %s", r.state.Source.Repository.URL, r.state.Source.Commit)
	}
	if !r.state.Candidate.empty() {
		patch, err := s.readBlob(ctx, r, r.state.Candidate)
		if err != nil {
			return nil, err
		}
		result.Diff = string(patch)
	}
	var feedback strings.Builder
	feedback.WriteString(completionExplanation(r.state.Final.Reason))
	for _, item := range []struct {
		label string
		ref   blob
	}{{"Original", r.state.Baseline}, {"Candidate", r.state.Comparison}} {
		if item.ref.empty() {
			continue
		}
		var record pv.Record
		if err := s.readJSON(ctx, r, item.ref, &record); err != nil {
			return nil, err
		}
		fmt.Fprintf(&feedback, "\n\n%s runner: %s\n%s", item.label, record.Assessment.Conclusion, record.Assessment.Reason)
		for _, check := range record.Assessment.Checks {
			fmt.Fprintf(&feedback, "\n- %s: %s", check.CheckID, check.Outcome)
			if check.OriginalOutcome != "" || check.PatchedOutcome != "" {
				fmt.Fprintf(&feedback, " (original: %s; candidate: %s)", check.OriginalOutcome, check.PatchedOutcome)
			}
			if check.Reason != "" {
				fmt.Fprintf(&feedback, " — %s", check.Reason)
			}
		}
	}
	if err := s.appendAgentFeedback(ctx, r, &feedback); err != nil {
		return nil, err
	}
	result.Feedback = boundedNormalSummary(feedback.String(), 64<<10)
	data, err := common.FormatStructuredResult(result)
	if err != nil {
		return nil, contract.ErrUnavailable
	}
	if len(data) > MaxAggregateResultBytes {
		result.Feedback = completionExplanation(r.state.Final.Reason) +
			"\nDetailed feedback exceeds the normal result envelope; complete agent replies and execution records remain retained internally."
		data, err = common.FormatStructuredResult(result)
	}
	if err != nil || len(data) > MaxAggregateResultBytes {
		return nil, contract.ErrInvalid
	}
	return data, nil
}

func (s *Coordinator) appendAgentFeedback(ctx context.Context, r *run, output *strings.Builder) error {
	for _, role := range []string{contract.StageReproduce, contract.StageGenerate, "verify-0", "verify-1"} {
		op := r.state.Agents[role]
		if op == nil || op.Output.empty() {
			continue
		}
		raw, err := s.readBlob(ctx, r, op.Output)
		if err != nil {
			return err
		}
		var summary string
		var limitations []string
		switch role {
		case contract.StageReproduce:
			review, err := remediation.DecodeReproductionReview(string(raw))
			if err != nil {
				continue
			}
			summary, limitations = review.Summary, review.Limitations
		case contract.StageGenerate:
			proposal, err := remediation.DecodePatchProposal(string(raw))
			if err != nil {
				continue
			}
			summary, limitations = proposal.Summary, proposal.Limitations
		default:
			review, err := remediation.DecodeCandidateReview(string(raw))
			if err != nil {
				continue
			}
			summary, limitations = review.Summary, review.Limitations
		}
		fmt.Fprintf(output, "\n\nAgent %s (advisory; does not override runner evidence): %s", role, summary)
		for _, limitation := range limitations {
			fmt.Fprintf(output, "\n- Limitation: %s", limitation)
		}
	}
	return nil
}

func boundedNormalSummary(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for !utf8.ValidString(text[:limit]) {
		limit--
	}
	return common.TruncateSummary(text, limit)
}

func completionSummary(outcome string) string {
	switch outcome {
	case contract.OutcomeVerified:
		return "One candidate was verified for the frozen reproduction and normal-behavior checks."
	case contract.OutcomeReproduced:
		return "The report was reproduced on the exact original revision. No candidate was generated."
	case contract.OutcomeNotReproduced:
		return "The report was not reproduced under the tested conditions. No fix was generated."
	case contract.OutcomeUnverified:
		return "One candidate is available but remains unverified. No automatic fix retry was performed."
	case contract.OutcomeCancelled:
		return "The workflow was cancelled. No verified fix is claimed."
	case contract.OutcomeTimedOut:
		return "The execution deadline was exceeded. No verified fix is claimed."
	default:
		return "The required execution evidence could not be established. No verified fix is claimed."
	}
}

func completionExplanation(reason string) string {
	switch reason {
	case contract.ReasonEvidenceScopeUnsupported:
		return "The configured backend cannot provide the required evidence scope. No weaker scope was substituted."
	case "checks-invalid", "environment-unsupported":
		return "The proposed checks or their required environment were rejected before runner admission."
	case "candidate-invalid":
		return "The proposed candidate, declared changes, or execution input was invalid. The candidate is not verified."
	case "source-too-large":
		return "The complete encoded model context exceeds the supported prompt budget. No comparison was admitted after this preflight rejection."
	case "source-unavailable":
		return "The explicit public source revision could not be resolved or read."
	case "original-evidence-invalid":
		return "Execution evidence failed its frozen identity or integrity checks and cannot authorize a fix."
	case reasonIdentityConflict:
		return "An operation identity or acknowledgement is unresolved. No replacement execution or automatic fix retry was authorized."
	case "agent-output-invalid", "validation-invalid":
		return "An agent reply did not match the required response contract; it was not accepted as validation."
	case "agent-configuration-changed":
		return "The frozen agent, provider, or worker-image authority changed. The run cannot use replacement authority."
	case "agent-task-failed", "execution-unavailable":
		return "An agent or runner did not complete successfully. Any returned candidate remains unverified."
	default:
		return "Conclusion is limited to the recorded checks and observed conditions. Agent assessments cannot override missing or failing execution evidence."
	}
}

func evidenceScopeSummary(scope contract.EvidenceScope) string {
	switch scope {
	case contract.SourceHarness:
		return "Evidence scope: source-harness. Only source-harness checks are covered; application-runtime behavior is not qualified."
	case contract.ApplicationRuntime:
		return "Evidence scope: application-runtime. Conclusions remain limited to the recorded runtime checks and observed conditions."
	default:
		return "Evidence scope: unrecorded (legacy). Application-runtime behavior is not qualified."
	}
}
