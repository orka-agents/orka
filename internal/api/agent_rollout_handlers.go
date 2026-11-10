package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v3"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/evals"
	"github.com/orka-agents/orka/internal/store"
)

// Rollout actions on a stable Agent.
const (
	AgentRolloutPromote  = "promote"
	AgentRolloutRollback = "rollback"
)

// AgentPromoter promotes approved candidates onto stable Agents and rolls
// them back, keeping a durable log of every action.
type AgentPromoter interface {
	PromoteAgent(ctx context.Context, namespace, agent, decisionID string, actor store.EvalDecisionActor) (*store.AgentPromotionRecord, error)
	RollbackAgent(ctx context.Context, namespace, agent string, actor store.EvalDecisionActor, reason string) (*store.AgentPromotionRecord, error)
	ListAgentPromotions(ctx context.Context, namespace, agent string) ([]store.AgentPromotionRecord, error)
}

// AgentRolloutRequest is the body of POST /api/v1/agents/:name/rollout.
type AgentRolloutRequest struct {
	Action     string `json:"action"`
	DecisionID string `json:"decisionID,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// RolloutAgent promotes an approved candidate onto the Agent, or rolls back
// its latest promotion. Only a human promoter with the promote verb on
// agents may ask.
func (h *Handlers) RolloutAgent(c fiber.Ctx) error {
	if err := refuseEvalTransactionTokens(c, "Agent rollouts cannot be requested with transaction tokens"); err != nil {
		return err
	}
	if h.agentPromoter == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "Agent rollouts are not configured; they need --enable-evals")
	}
	actor, err := h.evalPromoter(c)
	if err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	name := c.Params("name")
	// The route check runs RBAC only for Kubernetes identities; promote is
	// checked for every caller, including direct OIDC users.
	if err := reviewKubernetesResourceAction(c.Context(), h.clientset, GetUserInfo(c), namespace,
		"promote", corev1alpha1.GroupVersion.Group, "agents", name); err != nil {
		return fiber.NewError(fiber.StatusForbidden,
			"Agent rollouts need the promote verb on agents in this namespace, which the eval-promoter ClusterRole grants")
	}
	var req AgentRolloutRequest
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	reason := strings.TrimSpace(req.Reason)
	if len(reason) > store.MaxEvalDecisionReasonBytes {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("reason must be at most %d bytes", store.MaxEvalDecisionReasonBytes))
	}
	var record *store.AgentPromotionRecord
	switch req.Action {
	case AgentRolloutPromote:
		if strings.TrimSpace(req.DecisionID) == "" {
			return fiber.NewError(fiber.StatusBadRequest, "decisionID is required to promote")
		}
		record, err = h.agentPromoter.PromoteAgent(c.Context(), namespace, name, strings.TrimSpace(req.DecisionID), actor)
	case AgentRolloutRollback:
		if req.DecisionID != "" {
			return fiber.NewError(fiber.StatusBadRequest, "a rollback takes no decisionID; it reverts the latest promotion")
		}
		record, err = h.agentPromoter.RollbackAgent(c.Context(), namespace, name, actor, reason)
	default:
		return fiber.NewError(fiber.StatusBadRequest, "action must be promote or rollback")
	}
	if refusal, ok := errors.AsType[*evals.RolloutRefusal](err); ok {
		return fiber.NewError(fiber.StatusConflict, refusal.Reason)
	}
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("the %s did not finish: %v", req.Action, err))
	}
	return c.Status(fiber.StatusCreated).JSON(record)
}

// GetAgentPromotions lists the Agent's promotion log, oldest first.
func (h *Handlers) GetAgentPromotions(c fiber.Ctx) error {
	if err := refuseEvalTransactionTokens(c, "Agent promotions cannot be read with transaction tokens"); err != nil {
		return err
	}
	if h.agentPromoter == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "Agent rollouts are not configured; they need --enable-evals")
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	records, err := h.agentPromoter.ListAgentPromotions(c.Context(), namespace, c.Params("name"))
	if refusal, ok := errors.AsType[*evals.RolloutRefusal](err); ok {
		return fiber.NewError(fiber.StatusNotFound, refusal.Reason)
	}
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list promotions: %v", err))
	}
	if records == nil {
		records = []store.AgentPromotionRecord{}
	}
	return c.JSON(fiber.Map{apiFieldItems: records})
}
