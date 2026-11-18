package kube

import (
	"context"
	"errors"

	pv "github.com/orka-agents/orka/internal/patchverification"
)

func (s *Service) GetSubmission(ctx context.Context, namespace, requestID string) (*pv.KubernetesSubmission, error) {
	if s == nil || !s.config.Enabled || namespace != s.config.Namespace {
		return nil, pv.ErrBinding
	}
	submission, err := s.store.GetKubernetesValidationSubmission(ctx, namespace, requestID)
	if err != nil {
		return nil, err
	}
	submission.RequiredChecks = len(pv.ActionSides(submission.Manifest.Action)) * len(submission.Manifest.Checks)
	if submission.RunID != "" {
		record, err := s.store.GetPatchVerificationRun(ctx, submission.RunID)
		if err != nil && !errors.Is(err, pv.ErrIntegrity) {
			return nil, err
		}
		if record != nil {
			submission.Assessment = &record.Assessment
			submission.RecordedChecks = len(record.Evidence)
		}
	} else if submission.State == pv.SubmissionTerminal {
		submission.Assessment = &pv.Assessment{Conclusion: pv.UnavailableAction(submission.Manifest.Action), Reason: submission.Failure}
		for _, check := range submission.Manifest.Checks {
			submission.Assessment.Checks = append(submission.Assessment.Checks,
				pv.CheckResult{CheckID: check.ID, Outcome: pv.OutcomeUntested, Reason: "execution did not start"})
		}
	}
	return submission, nil
}

func (s *Service) Cancel(ctx context.Context, namespace, requestID string) error {
	if s == nil || !s.config.Enabled || namespace != s.config.Namespace {
		return pv.ErrBinding
	}
	return s.store.RequestKubernetesValidationCancellation(ctx, namespace, requestID)
}

func (s *Service) Evidence(ctx context.Context, namespace, requestID string) (*pv.Record, error) {
	if s == nil || !s.config.Enabled || namespace != s.config.Namespace {
		return nil, pv.ErrBinding
	}
	submission, err := s.store.GetKubernetesValidationSubmission(ctx, namespace, requestID)
	if err != nil {
		return nil, err
	}
	if submission.RunID == "" {
		return nil, pv.ErrRunNotFound
	}
	return s.store.GetPatchVerificationRun(ctx, submission.RunID)
}
