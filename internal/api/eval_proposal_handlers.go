package api

import (
	"context"
	"errors"
	"fmt"

	"github.com/gofiber/fiber/v3"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/evals"
)

// EvalProposer starts and lists proposals: proposer Tasks that learn from a
// training run's failed trials and become candidate experiments.
type EvalProposer interface {
	StartEvalProposal(ctx context.Context, run *corev1alpha1.EvalRun, request evals.ProposalRequest) (*corev1alpha1.Task, error)
	ListEvalProposals(ctx context.Context, run *corev1alpha1.EvalRun) ([]evals.Proposal, error)
}

// EvalProposalRequestBody is the body of POST .../proposals.
type EvalProposalRequestBody struct {
	HeldOut string                             `json:"heldOut,omitempty"`
	Target  *corev1alpha1.EvalExperimentTarget `json:"target,omitempty"`
}

// ProposeEvalRun starts one proposal from a finished training run. Only a
// promoter may ask: a proposal spends from the baseline's candidate budget
// and creates an Agent and two EvalRuns.
func (h *Handlers) ProposeEvalRun(c fiber.Ctx) error {
	if err := refuseEvalTransactionTokens(c, "eval proposals cannot be requested with transaction tokens"); err != nil {
		return err
	}
	if h.evalProposer == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "eval proposals are not configured")
	}
	actor, err := h.evalPromoter(c)
	if err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := reviewKubernetesResourceAction(c.Context(), h.clientset, GetUserInfo(c), namespace,
		"propose", corev1alpha1.GroupVersion.Group, "evalruns", c.Params("name")); err != nil {
		return fiber.NewError(fiber.StatusForbidden,
			"eval proposals need the propose verb on evalruns in this namespace, which the eval-promoter ClusterRole grants")
	}
	var body EvalProposalRequestBody
	if len(c.Body()) > 0 {
		if err := c.Bind().JSON(&body); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
		}
	}
	if body.Target != nil && body.Target.Grader == "" {
		return fiber.NewError(fiber.StatusBadRequest, "target.grader is required when target is set")
	}
	run, err := h.getEvalRunUncached(c)
	if err != nil {
		return err
	}
	task, err := h.evalProposer.StartEvalProposal(c.Context(), run, evals.ProposalRequest{
		HeldOut: body.HeldOut, Target: body.Target, RequestedBy: actor.Username,
	})
	if refusal, ok := errors.AsType[*evals.ProposalRefusal](err); ok {
		return fiber.NewError(fiber.StatusConflict, refusal.Reason)
	}
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to start the proposal: %v", err))
	}
	return c.Status(fiber.StatusCreated).JSON(fiber.Map{apiFieldName: task.Name, "namespace": task.Namespace, "evalRun": run.Name})
}

// GetEvalRunProposals lists a training run's proposals and what became of
// each: the hypothesis, the candidate Agent and the paired experiment.
func (h *Handlers) GetEvalRunProposals(c fiber.Ctx) error {
	if err := refuseEvalTransactionTokens(c, "eval proposals cannot be read with transaction tokens"); err != nil {
		return err
	}
	if h.evalProposer == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "eval proposals are not configured")
	}
	run, err := h.getEvalRunUncached(c)
	if err != nil {
		return err
	}
	proposals, err := h.evalProposer.ListEvalProposals(c.Context(), run)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list proposals: %v", err))
	}
	return c.JSON(fiber.Map{apiFieldItems: proposals})
}
