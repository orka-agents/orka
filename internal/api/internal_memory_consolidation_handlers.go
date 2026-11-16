/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v3"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/memoryconsolidation"
	"github.com/orka-agents/orka/internal/store"
)

// memoryConsolidationSource reads a run's inputs from the session, memory
// and run stores.
type memoryConsolidationSource struct {
	store.SessionStore
	store.MemoryStore
	store.MemoryConsolidationStore
}

// memoryConsolidationCaller returns the authenticated worker's Task when it is
// an active memory consolidation run. Only the run's own worker may begin or
// complete it: the run reads every user's Sessions, which admission allowed
// only for the caller that created the Task.
func (h *InternalHandlers) memoryConsolidationCaller(c fiber.Ctx) (string, *corev1alpha1.Task, error) {
	namespace := c.Params("namespace")
	if namespace == "" {
		return "", nil, fiber.NewError(fiber.StatusBadRequest, "namespace is required")
	}
	if h.memoryConsolidationStore == nil || h.sessionStore == nil || h.memoryStore == nil {
		return "", nil, fiber.NewError(fiber.StatusNotImplemented, "memory consolidation is not configured")
	}
	task, err := h.internalCallerAuthorizer().resolveActiveTaskCaller(c, namespace)
	if err != nil {
		return "", nil, err
	}
	if task.Spec.Type != corev1alpha1.TaskTypeAI || task.Spec.AI == nil || task.Spec.AI.MemoryConsolidation == nil || task.UID == "" {
		return "", nil, fiber.NewError(fiber.StatusForbidden, "caller task is not a memory consolidation run")
	}
	return namespace, task, nil
}

// memoryConsolidationNow is the clock a run's window ends by; tests move it
// past the Sessions they write.
var memoryConsolidationNow = time.Now

// errAbandonedMemoryConsolidation answers a run another run took over after
// its lease ended: it can neither resume nor complete.
var errAbandonedMemoryConsolidation = fiber.NewError(fiber.StatusConflict,
	"this memory consolidation run's lease ended and another run took over")

// BeginMemoryConsolidation starts the caller's run, or resumes it when it
// already began. Beginning reads the run's inputs once and records them with
// the run, so a resumed worker reads the same input and completion is
// checked against exactly what the model saw. A run with too few changed
// Sessions is recorded as skipped without calling the model.
func (h *InternalHandlers) BeginMemoryConsolidation(c fiber.Ctx) error {
	namespace, task, err := h.memoryConsolidationCaller(c)
	if err != nil {
		return err
	}
	ctx := c.Context()
	settings := memoryconsolidation.SettingsFor(task.Spec.AI.MemoryConsolidation)
	run, err := h.memoryConsolidationStore.GetMemoryConsolidationRun(ctx, namespace, string(task.UID))
	if errors.Is(err, store.ErrNotFound) {
		run, err = h.beginMemoryConsolidation(ctx, namespace, task, settings)
	}
	if err != nil {
		return memoryStoreError("begin memory consolidation", "memory consolidation run", err)
	}
	switch run.Status {
	case store.MemoryConsolidationRunning:
	case store.MemoryConsolidationAbandoned:
		return errAbandonedMemoryConsolidation
	default:
		return finishedMemoryConsolidation(c, settings, run)
	}
	input, err := decodeMemoryConsolidationInput(run)
	if err != nil {
		return err
	}
	if input.EligibleSessions < settings.MinNewSessions {
		report := newMemoryConsolidationReport(settings, store.MemoryConsolidationSkipped, run, input, nil, nil, nil, "", nil)
		report.SkippedReason = fmt.Sprintf("%d Sessions changed since the last completed run; the run needs %d",
			input.EligibleSessions, settings.MinNewSessions)
		completed, err := h.completeMemoryConsolidation(ctx, run, store.MemoryConsolidationSkipped, report, nil)
		if err != nil {
			return err
		}
		return finishedMemoryConsolidation(c, settings, completed)
	}
	return c.JSON(memoryconsolidation.BeginResponse{Status: run.Status, Settings: settings, Input: &input})
}

// beginMemoryConsolidation fixes a new run's window from its mode's cursor,
// reads its inputs and records both.
func (h *InternalHandlers) beginMemoryConsolidation(ctx context.Context, namespace string, task *corev1alpha1.Task,
	settings memoryconsolidation.Settings) (*store.MemoryConsolidationRun, error) {
	cursor, err := h.memoryConsolidationStore.GetMemoryConsolidationCursor(ctx, namespace, string(settings.Mode))
	if err != nil {
		return nil, err
	}
	now := memoryConsolidationNow().UTC()
	start, end := memoryconsolidation.Window(cursor, now, settings.Lookback)
	input, err := memoryconsolidation.CollectInputs(ctx, memoryConsolidationSource{h.sessionStore, h.memoryStore, h.memoryConsolidationStore},
		namespace, start, end)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encode memory consolidation input: %w", err)
	}
	run := &store.MemoryConsolidationRun{Namespace: namespace, TaskUID: string(task.UID), TaskName: task.Name,
		Mode: string(settings.Mode), Status: store.MemoryConsolidationRunning, WindowStart: start, WindowEnd: end,
		Input: encoded, CreatedAt: now}
	if err := h.memoryConsolidationStore.BeginMemoryConsolidationRun(ctx, run); err != nil {
		return nil, err
	}
	return run, nil
}

// CompleteMemoryConsolidation validates the caller's submitted result against
// the input its run recorded, and against the targets' current generations,
// and records the report. In Propose mode it also creates a pending proposal
// for every kept operation, in the same write. A run that already finished
// returns its report unchanged.
func (h *InternalHandlers) CompleteMemoryConsolidation(c fiber.Ctx) error {
	namespace, task, err := h.memoryConsolidationCaller(c)
	if err != nil {
		return err
	}
	var submission memoryconsolidation.Submission
	if err := c.Bind().JSON(&submission); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	ctx := c.Context()
	settings := memoryconsolidation.SettingsFor(task.Spec.AI.MemoryConsolidation)
	run, err := h.memoryConsolidationStore.GetMemoryConsolidationRun(ctx, namespace, string(task.UID))
	if err != nil {
		return memoryStoreError("read memory consolidation run", "memory consolidation run", err)
	}
	switch run.Status {
	case store.MemoryConsolidationRunning:
	case store.MemoryConsolidationAbandoned:
		return errAbandonedMemoryConsolidation
	default:
		report, err := decodeMemoryConsolidationReport(run)
		if err != nil {
			return err
		}
		return c.JSON(report)
	}
	input, err := decodeMemoryConsolidationInput(run)
	if err != nil {
		return err
	}
	current, err := h.currentTargetGenerations(ctx, namespace, submission)
	if err != nil {
		return memoryStoreError("read memory consolidation targets", "memory consolidation run", err)
	}
	kept, rejected, conflicts := memoryconsolidation.Validate(submission, input, current, settings.MaxOperations)
	var proposals []store.MemoryProposal
	if settings.Mode == corev1alpha1.MemoryConsolidationPropose {
		proposals = memoryconsolidation.Proposals(kept, namespace, task.Name, string(task.UID), agentNameOf(task))
	}
	report := newMemoryConsolidationReport(settings, store.MemoryConsolidationCompleted, run, input, kept, rejected, conflicts, submission.Summary, proposals)
	completed, err := h.completeMemoryConsolidation(ctx, run, store.MemoryConsolidationCompleted, report, proposals)
	if err != nil {
		return err
	}
	if completed.Status == store.MemoryConsolidationAbandoned {
		return errAbandonedMemoryConsolidation
	}
	stored, err := decodeMemoryConsolidationReport(completed)
	if err != nil {
		return err
	}
	return c.JSON(stored)
}

// currentTargetGenerations returns the current generation of every active
// memory a submission targets.
func (h *InternalHandlers) currentTargetGenerations(ctx context.Context, namespace string, submission memoryconsolidation.Submission) (map[string]uint64, error) {
	var ids []string
	for _, op := range submission.Operations {
		for _, target := range op.Targets {
			if len(ids) < store.MaxMemoryRecallIDs {
				ids = append(ids, target.MemoryID)
			}
		}
	}
	current := map[string]uint64{}
	if len(ids) == 0 {
		return current, nil
	}
	memories, err := h.memoryStore.ListMemories(ctx, store.MemoryFilter{Namespace: namespace, IDs: ids, Limit: store.MaxMemoryRecallIDs})
	if err != nil {
		return nil, err
	}
	for _, memory := range memories {
		current[memory.ID] = memory.Generation
	}
	return current, nil
}

func decodeMemoryConsolidationInput(run *store.MemoryConsolidationRun) (memoryconsolidation.Input, error) {
	var input memoryconsolidation.Input
	if err := json.Unmarshal(run.Input, &input); err != nil {
		return memoryconsolidation.Input{}, fmt.Errorf("decode memory consolidation input: %w", err)
	}
	return input, nil
}

func (h *InternalHandlers) completeMemoryConsolidation(ctx context.Context, run *store.MemoryConsolidationRun, status string,
	report memoryconsolidation.Report, proposals []store.MemoryProposal) (*store.MemoryConsolidationRun, error) {
	report.Redact()
	encoded, err := json.Marshal(report)
	if err != nil {
		return nil, fmt.Errorf("encode memory consolidation report: %w", err)
	}
	completed, err := h.memoryConsolidationStore.CompleteMemoryConsolidationRun(ctx, store.MemoryConsolidationCompletion{
		Namespace: run.Namespace, TaskUID: run.TaskUID, Status: status, Report: encoded,
		CompletedAt: time.Now().UTC(), Proposals: proposals,
	})
	if err != nil {
		return nil, memoryStoreError("complete memory consolidation", "memory consolidation run", err)
	}
	return completed, nil
}

func newMemoryConsolidationReport(settings memoryconsolidation.Settings, status string, run *store.MemoryConsolidationRun,
	input memoryconsolidation.Input, kept []memoryconsolidation.Operation, rejected []memoryconsolidation.Rejection,
	conflicts []memoryconsolidation.Conflict, summary string, proposals []store.MemoryProposal) memoryconsolidation.Report {
	report := memoryconsolidation.NewReport(settings, status, input, kept, rejected, conflicts, summary, proposals)
	report.WindowStart, report.WindowEnd = run.WindowStart, run.WindowEnd
	return report
}

func finishedMemoryConsolidation(c fiber.Ctx, settings memoryconsolidation.Settings, run *store.MemoryConsolidationRun) error {
	report, err := decodeMemoryConsolidationReport(run)
	if err != nil {
		return err
	}
	return c.JSON(memoryconsolidation.BeginResponse{Status: run.Status, Settings: settings, Report: report})
}

func decodeMemoryConsolidationReport(run *store.MemoryConsolidationRun) (*memoryconsolidation.Report, error) {
	var report memoryconsolidation.Report
	if err := json.Unmarshal(run.Report, &report); err != nil {
		return nil, fmt.Errorf("decode memory consolidation report: %w", err)
	}
	return &report, nil
}

func agentNameOf(task *corev1alpha1.Task) string {
	if task.Spec.AgentRef != nil {
		return task.Spec.AgentRef.Name
	}
	return ""
}
