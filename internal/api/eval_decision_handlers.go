package api

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// EvalDecisionVerifier recomputes, from live objects, what an approval of a
// candidate EvalRun would bind to now.
type EvalDecisionVerifier interface {
	CurrentEvalDecisionBinding(ctx context.Context, candidate *corev1alpha1.EvalRun) (*store.EvalDecisionBinding, error)
}

// EvalDecisionRequest approves or rejects a decided experiment.
type EvalDecisionRequest struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason"`
}

// EvalApprovalStatus reports whether an effective approval still matches.
type EvalApprovalStatus struct {
	Current bool `json:"current"`
	// Changed names what no longer matches the approved experiment.
	Changed []string `json:"changed,omitempty"`
}

// EvalExperimentResponse is one experiment and its decisions.
type EvalExperimentResponse struct {
	store.EvalExperimentEntry
	// Effective is the latest decision, the one in force.
	Effective *store.EvalDecisionRecord `json:"effective,omitempty"`
	// Approval is set when the effective decision is an approval and the
	// response rechecked it.
	Approval *EvalApprovalStatus `json:"approval,omitempty"`
}

func evalExperimentResponse(entry store.EvalExperimentEntry) EvalExperimentResponse {
	response := EvalExperimentResponse{EvalExperimentEntry: entry}
	if n := len(entry.Decisions); n > 0 {
		response.Effective = &entry.Decisions[n-1]
	}
	return response
}

// ListEvalExperiments returns a namespace's experiments with their outcomes
// and decisions, including those whose EvalRuns are gone, a page at a time
// (limit, and continue from the previous page). Reading them is authorized
// like listing EvalRuns.
func (h *Handlers) ListEvalExperiments(c fiber.Ctx) error {
	if h.evalExperimentStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "eval experiment store not configured")
	}
	if err := refuseEvalTransactionTokens(c, "eval experiments cannot be read with transaction tokens"); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	limit := 0
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		if limit, err = strconv.Atoi(raw); err != nil || limit < 1 || limit > store.MaxEvalExperimentListLimit {
			return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("limit must be 1 to %d", store.MaxEvalExperimentListLimit))
		}
	}
	page, err := h.evalExperimentStore.ListEvalExperiments(c.Context(), namespace,
		store.EvalExperimentListOptions{After: c.Query("continue"), Limit: limit})
	if errors.Is(err, store.ErrValidation) {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to list eval experiments: %v", err))
	}
	items := make([]EvalExperimentResponse, 0, len(page.Items))
	for _, entry := range page.Items {
		items = append(items, evalExperimentResponse(entry))
	}
	response := fiber.Map{apiFieldItems: items}
	if page.Next != "" {
		response["continue"] = page.Next
	}
	return c.JSON(response)
}

// GetEvalRunDecisions returns a candidate run's experiment, its decisions
// and, for an effective approval, whether it still matches the live
// configuration.
func (h *Handlers) GetEvalRunDecisions(c fiber.Ctx) error {
	if err := refuseEvalTransactionTokens(c, "eval decisions cannot be read with transaction tokens"); err != nil {
		return err
	}
	run, entry, err := h.evalRunExperiment(c)
	if err != nil {
		return err
	}
	response := evalExperimentResponse(*entry)
	if response.Effective != nil && response.Effective.Decision == store.EvalDecisionApproved {
		response.Approval = h.recheckEvalApproval(c.Context(), run, response.Effective.Binding)
	}
	return c.JSON(response)
}

// DecideEvalRun appends a person's decision on a decided experiment. Only a
// human promoter may decide: never a ServiceAccount or other system
// identity, never a transaction-token caller, and only a member of a
// configured promoter group, on top of the decide verb, which RBAC checks for
// every caller however they authenticated. An approval needs a
// Passed outcome whose binding still matches the live configuration; a
// rejection needs only an outcome.
func (h *Handlers) DecideEvalRun(c fiber.Ctx) error {
	if err := refuseEvalTransactionTokens(c, "eval decisions cannot be made with transaction tokens"); err != nil {
		return err
	}
	actor, err := h.evalPromoter(c)
	if err != nil {
		return err
	}
	var req EvalDecisionRequest
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	if req.Decision != store.EvalDecisionApproved && req.Decision != store.EvalDecisionRejected {
		return fiber.NewError(fiber.StatusBadRequest, "decision must be approved or rejected")
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" || len(reason) > store.MaxEvalDecisionReasonBytes {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("reason is required and must be at most %d bytes", store.MaxEvalDecisionReasonBytes))
	}
	// The route check runs RBAC only for Kubernetes identities; decide is
	// checked for every caller, including direct OIDC users.
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := reviewKubernetesResourceAction(c.Context(), h.clientset, GetUserInfo(c), namespace,
		"decide", corev1alpha1.GroupVersion.Group, "evalruns", c.Params("name")); err != nil {
		return err
	}
	run, entry, err := h.evalRunExperiment(c)
	if err != nil {
		return err
	}
	outcome := entry.Outcome
	if outcome == nil {
		return fiber.NewError(fiber.StatusConflict, "the experiment is not decided yet")
	}
	decision := &store.EvalDecisionRecord{
		SchemaVersion:   store.EvalDecisionSchemaVersion,
		ID:              uuid.NewString(),
		Namespace:       run.Namespace,
		CandidateRun:    run.Name,
		CandidateRunUID: string(run.UID),
		Decision:        req.Decision,
		Reason:          reason,
		Actor:           actor,
		Binding:         outcome.Binding,
		DecidedAt:       time.Now().UTC().Truncate(time.Second),
	}
	if req.Decision == store.EvalDecisionApproved {
		if outcome.Result != store.EvalExperimentResultPassed {
			return fiber.NewError(fiber.StatusConflict, fmt.Sprintf("only a Passed experiment can be approved; this one is %s", outcome.Result))
		}
		if status := h.recheckEvalApproval(c.Context(), run, outcome.Binding); !status.Current {
			return fiber.NewError(fiber.StatusConflict, "the experiment no longer matches the live configuration: "+strings.Join(status.Changed, "; "))
		}
	}
	if err := h.evalExperimentStore.AppendEvalDecision(c.Context(), decision); err != nil {
		switch {
		case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrNotFound):
			return fiber.NewError(fiber.StatusConflict, err.Error())
		case errors.Is(err, store.ErrValidation):
			return fiber.NewError(fiber.StatusBadRequest, err.Error())
		}
		return fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to record the decision: %v", err))
	}
	return c.Status(fiber.StatusCreated).JSON(decision)
}

// evalPromoter admits a human promoter and returns who decided.
func (h *Handlers) evalPromoter(c fiber.Ctx) (store.EvalDecisionActor, error) {
	if len(h.evalPromoterGroups) == 0 {
		return store.EvalDecisionActor{}, fiber.NewError(fiber.StatusForbidden,
			"eval decisions are off: the controller names no promoter groups (--eval-promoter-groups)")
	}
	ui := GetUserInfo(c)
	if ui == nil || strings.TrimSpace(ui.Username) == "" {
		return store.EvalDecisionActor{}, fiber.NewError(fiber.StatusForbidden, "eval decisions need an authenticated person")
	}
	if ui.AuthType == AuthTypeContextToken || ui.ContextToken != nil {
		return store.EvalDecisionActor{}, fiber.NewError(fiber.StatusForbidden, "eval decisions cannot be made with transaction tokens")
	}
	if strings.HasPrefix(ui.Username, "system:") {
		return store.EvalDecisionActor{}, fiber.NewError(fiber.StatusForbidden, "eval decisions must be made by a person, not a ServiceAccount or system identity")
	}
	var groups []string
	for _, group := range h.evalPromoterGroups {
		if slices.Contains(ui.Groups, group) {
			groups = append(groups, group)
		}
	}
	if len(groups) == 0 {
		return store.EvalDecisionActor{}, fiber.NewError(fiber.StatusForbidden, "eval decisions need membership in a promoter group")
	}
	actor := store.EvalDecisionActor{Username: ui.Username, UID: ui.UID, Groups: groups}
	if ui.AuthType == AuthTypeOIDC {
		actor.Issuer, actor.Subject = ui.Issuer, ui.Subject
	}
	return actor, nil
}

// evalRunExperiment loads a live candidate run and its stored experiment.
func (h *Handlers) evalRunExperiment(c fiber.Ctx) (*corev1alpha1.EvalRun, *store.EvalExperimentEntry, error) {
	if h.evalExperimentStore == nil {
		return nil, nil, fiber.NewError(fiber.StatusNotImplemented, "eval experiment store not configured")
	}
	// Uncached, so a decision never attaches to a deleted incarnation's
	// outcome when the run was recreated under the same name.
	run, err := h.getEvalRunUncached(c)
	if err != nil {
		return nil, nil, err
	}
	if run.Spec.Experiment == nil {
		return nil, nil, fiber.NewError(fiber.StatusConflict, "eval run is not an experiment candidate")
	}
	page, err := h.evalExperimentStore.ListEvalExperiments(c.Context(), run.Namespace,
		store.EvalExperimentListOptions{CandidateRunUID: string(run.UID), Limit: 1})
	if err != nil {
		return nil, nil, fiber.NewError(fiber.StatusInternalServerError, fmt.Sprintf("failed to read the experiment: %v", err))
	}
	if len(page.Items) == 0 {
		return nil, nil, fiber.NewError(fiber.StatusConflict, "the experiment holds no attempt; it never started")
	}
	return run, &page.Items[0], nil
}

// recheckEvalApproval compares an approval's binding with the live one.
// Anything that cannot be rechecked counts as changed.
func (h *Handlers) recheckEvalApproval(ctx context.Context, run *corev1alpha1.EvalRun, approved *store.EvalDecisionBinding) *EvalApprovalStatus {
	if approved == nil {
		return &EvalApprovalStatus{Changed: []string{"the approval records no binding"}}
	}
	if h.evalDecisionVerifier == nil {
		return &EvalApprovalStatus{Changed: []string{"the controller cannot recheck approvals"}}
	}
	current, err := h.evalDecisionVerifier.CurrentEvalDecisionBinding(ctx, run)
	if err != nil {
		return &EvalApprovalStatus{Changed: []string{"the live configuration cannot be checked: " + err.Error()}}
	}
	changed := evalBindingChanges(*approved, *current)
	return &EvalApprovalStatus{Current: len(changed) == 0, Changed: changed}
}

// evalBindingChanges names each element of the binding that differs.
func evalBindingChanges(approved, current store.EvalDecisionBinding) []string {
	var changed []string
	check := func(differs bool, what string) {
		if differs {
			changed = append(changed, what)
		}
	}
	check(approved.CandidateAgent != current.CandidateAgent, fmt.Sprintf("candidate Agent %q was edited or recreated", approved.CandidateAgent.Name))
	check(approved.CandidateDigest != current.CandidateDigest, "the candidate's prompt, model or runtime image changed")
	check(approved.BaselineAgent != current.BaselineAgent, fmt.Sprintf("stable Agent %q was edited, recreated or replaced", approved.BaselineAgent.Name))
	check(approved.BaselineDigest != current.BaselineDigest, "the stable reviewer's prompt, model or runtime image changed")
	check(approved.MonitorUID != current.MonitorUID || approved.MonitorGeneration != current.MonitorGeneration, "the RepositoryMonitor changed")
	check(approved.PromptTemplateDigest != current.PromptTemplateDigest, "the review prompt template changed")
	check(approved.CaseSetDigest != current.CaseSetDigest, "the case set changed")
	check(approved.GraderSetDigest != current.GraderSetDigest, "the grader set changed")
	check(approved.GateDigest != current.GateDigest, "the experiment gate changed")
	return changed
}
