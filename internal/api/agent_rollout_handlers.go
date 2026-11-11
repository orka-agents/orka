package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	AgentRolloutCanary   = "canary"
	AgentRolloutAbort    = "abort"
)

// AgentPromoter promotes approved candidates onto stable Agents and rolls
// them back, keeping a durable log of every action.
type AgentPromoter interface {
	PromoteAgent(ctx context.Context, namespace, agent, decisionID string, actor store.EvalDecisionActor) (*store.AgentPromotionRecord, error)
	RollbackAgent(ctx context.Context, namespace, agent string, actor store.EvalDecisionActor, reason string) (*store.AgentPromotionRecord, error)
	CanaryAgent(ctx context.Context, namespace, agent, decisionID string, percent int32, actor store.EvalDecisionActor) (*store.AgentPromotionRecord, error)
	AbortAgentRollout(ctx context.Context, namespace, agent string, actor store.EvalDecisionActor, reason string) (*store.AgentPromotionRecord, error)
	ListAgentPromotions(ctx context.Context, namespace, agent string) ([]store.AgentPromotionRecord, error)
	AgentRollout(ctx context.Context, namespace, agent string) (*corev1alpha1.AgentRolloutStatus, string, error)
}

// AgentRolloutRequest is the body of POST /api/v1/agents/:name/rollout.
type AgentRolloutRequest struct {
	Action     string `json:"action"`
	DecisionID string `json:"decisionID,omitempty"`
	Percent    int32  `json:"percent,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// AgentPromotionValuesView shows promotable values without the prompt text.
// A previous prompt may have been resolved from a ConfigMap the reader
// cannot read, so responses carry its digest and size only.
type AgentPromotionValuesView struct {
	SystemPromptDigest string `json:"systemPromptDigest,omitempty"`
	SystemPromptBytes  int    `json:"systemPromptBytes"`
	// SystemPromptConfigMap names the ConfigMap the prompt is read from.
	SystemPromptConfigMap *store.AgentPromotionConfigMapRef `json:"systemPromptConfigMap,omitempty"`
	Model                 string                            `json:"model,omitempty"`
}

// AgentPromotionResponse is a promotion record as the API returns it.
type AgentPromotionResponse struct {
	store.AgentPromotionRecord
	Previous AgentPromotionValuesView `json:"previous"`
	Target   AgentPromotionValuesView `json:"target"`
}

func agentPromotionValuesView(values store.AgentPromotionValues) AgentPromotionValuesView {
	view := AgentPromotionValuesView{SystemPromptBytes: len(values.SystemPrompt), SystemPromptConfigMap: values.SystemPromptConfigMap,
		Model: values.Model}
	if values.SystemPrompt != "" {
		sum := sha256.Sum256([]byte(values.SystemPrompt))
		view.SystemPromptDigest = "sha256:" + hex.EncodeToString(sum[:])
	}
	return view
}

func agentPromotionResponse(record store.AgentPromotionRecord) AgentPromotionResponse {
	response := AgentPromotionResponse{
		AgentPromotionRecord: record,
		Previous:             agentPromotionValuesView(record.Previous),
		Target:               agentPromotionValuesView(record.Target),
	}
	response.AgentPromotionRecord.Previous, response.AgentPromotionRecord.Target = store.AgentPromotionValues{}, store.AgentPromotionValues{}
	return response
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
	case AgentRolloutCanary:
		if strings.TrimSpace(req.DecisionID) == "" || req.Percent < 1 || req.Percent > 100 {
			return fiber.NewError(fiber.StatusBadRequest, "a canary needs a decisionID and a percent from 1 to 100")
		}
		record, err = h.agentPromoter.CanaryAgent(c.Context(), namespace, name, strings.TrimSpace(req.DecisionID), req.Percent, actor)
	case AgentRolloutAbort:
		if req.DecisionID != "" {
			return fiber.NewError(fiber.StatusBadRequest, "an abort takes no decisionID; it ends the running canary")
		}
		record, err = h.agentPromoter.AbortAgentRollout(c.Context(), namespace, name, actor, reason)
	default:
		return fiber.NewError(fiber.StatusBadRequest, "action must be promote, rollback, canary or abort")
	}
	if refusal, ok := errors.AsType[*evals.RolloutRefusal](err); ok {
		return fiber.NewError(fiber.StatusConflict, refusal.Reason)
	}
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("the %s did not finish: %v", req.Action, err))
	}
	return c.Status(fiber.StatusCreated).JSON(agentPromotionResponse(*record))
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
	items := make([]AgentPromotionResponse, 0, len(records))
	for _, record := range records {
		items = append(items, agentPromotionResponse(record))
	}
	rollout, invalid, err := h.agentPromoter.AgentRollout(c.Context(), namespace, c.Params("name"))
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to read the Agent's canary: %v", err))
	}
	response := fiber.Map{apiFieldItems: items}
	if rollout != nil && rollout.Candidate != nil {
		response["rollout"] = rollout
	}
	if invalid != "" {
		response["rolloutInvalid"] = invalid
	}
	return c.JSON(response)
}
