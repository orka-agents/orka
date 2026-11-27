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

type memoryConsolidationSource struct {
	store.SessionStore
	store.MemoryStore
	store.MemoryConsolidationStore
	store.MemoryProposalStore
}

func (h *InternalHandlers) memoryConsolidationCaller(c fiber.Ctx) (string, *corev1alpha1.Task, error) {
	namespace := c.Params("namespace")
	if namespace == "" {
		return "", nil, fiber.NewError(fiber.StatusBadRequest, "namespace is required")
	}
	if h.memoryConsolidationStore == nil || h.sessionStore == nil || h.memoryStore == nil || h.memoryProposalStore == nil {
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

const memoryConsolidationBeginAttempts = 3

var memoryConsolidationNow = time.Now

var errAbandonedMemoryConsolidation = fiber.NewError(fiber.StatusConflict,
	"this memory consolidation run's lease ended and another run took over")

func (h *InternalHandlers) BeginMemoryConsolidation(c fiber.Ctx) error {
	var response memoryconsolidation.BeginResponse
	if err := h.withMemoryConsolidationRun(c, func(ctx context.Context, namespace string, task *corev1alpha1.Task) error {
		var err error
		response, err = h.beginOrResumeMemoryConsolidation(ctx, namespace, task)
		return err
	}); err != nil {
		return err
	}
	return c.JSON(response)
}

func (h *InternalHandlers) withMemoryConsolidationRun(c fiber.Ctx,
	access func(ctx context.Context, namespace string, task *corev1alpha1.Task) error) error {
	if h.memoryConsolidationStore == nil || h.sessionStore == nil || h.memoryStore == nil || h.memoryProposalStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "memory consolidation is not configured")
	}
	var namespace string
	var task *corev1alpha1.Task
	return withInternalTaskData(c, h.sessionStore, "", func(context.Context) error {
		var err error
		namespace, task, err = h.memoryConsolidationCaller(c)
		return err
	}, func(ctx context.Context) error {
		return access(ctx, namespace, task)
	})
}

func (h *InternalHandlers) beginOrResumeMemoryConsolidation(ctx context.Context, namespace string,
	task *corev1alpha1.Task) (memoryconsolidation.BeginResponse, error) {
	settings := memoryconsolidation.SettingsFor(task.Spec.AI.MemoryConsolidation)
	run, err := h.memoryConsolidationStore.GetMemoryConsolidationRun(ctx, namespace, string(task.UID))
	if errors.Is(err, store.ErrNotFound) {
		for attempt := 1; ; attempt++ {
			run, err = h.beginMemoryConsolidation(ctx, namespace, task, settings)
			if !errors.Is(err, store.ErrPreconditionFailed) || attempt == memoryConsolidationBeginAttempts {
				break
			}
		}
	}
	if err != nil {
		return memoryconsolidation.BeginResponse{}, consolidationStoreError("begin memory consolidation", err)
	}
	switch run.Status {
	case store.MemoryConsolidationRunning:
	case store.MemoryConsolidationAbandoned:
		return memoryconsolidation.BeginResponse{}, errAbandonedMemoryConsolidation
	default:
		return finishedMemoryConsolidation(settings, run)
	}
	input, err := decodeMemoryConsolidationInput(run)
	if err != nil {
		return memoryconsolidation.BeginResponse{}, err
	}
	if input.EligibleSessions < settings.MinNewSessions {
		report := newMemoryConsolidationReport(settings, store.MemoryConsolidationSkipped, run, input, nil, nil, nil, "", nil)
		report.SkippedReason = fmt.Sprintf("%d Sessions changed since the last completed run; the run needs %d",
			input.EligibleSessions, settings.MinNewSessions)
		completed, err := h.completeMemoryConsolidation(ctx, run, store.MemoryConsolidationSkipped, report, nil)
		if err != nil {
			return memoryconsolidation.BeginResponse{}, err
		}
		return finishedMemoryConsolidation(settings, completed)
	}
	return memoryconsolidation.BeginResponse{Status: run.Status, Settings: settings, Input: &input}, nil
}

func (h *InternalHandlers) beginMemoryConsolidation(ctx context.Context, namespace string, task *corev1alpha1.Task,
	settings memoryconsolidation.Settings) (*store.MemoryConsolidationRun, error) {
	cursor, err := h.memoryConsolidationStore.GetMemoryConsolidationCursor(ctx, namespace, string(settings.Mode))
	if err != nil {
		return nil, err
	}
	now := memoryConsolidationNow().UTC()
	start, end := memoryconsolidation.Window(cursor, now, settings.Lookback)
	input, err := memoryconsolidation.CollectInputs(ctx, memoryConsolidationSource{h.sessionStore, h.memoryStore, h.memoryConsolidationStore, h.memoryProposalStore},
		namespace, start, end)
	if err != nil {
		return nil, err
	}
	if cursor != nil && start.After(*cursor) {
		cut := cursor.UTC()
		input.CutFrom = &cut
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

func (h *InternalHandlers) CompleteMemoryConsolidation(c fiber.Ctx) error {
	var submission memoryconsolidation.Submission
	if err := c.Bind().JSON(&submission); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	var report *memoryconsolidation.Report
	if err := h.withMemoryConsolidationRun(c, func(ctx context.Context, namespace string, task *corev1alpha1.Task) error {
		var err error
		report, err = h.completeSubmittedMemoryConsolidation(ctx, namespace, task, submission)
		return err
	}); err != nil {
		return err
	}
	return c.JSON(report.Structure())
}

func (h *InternalHandlers) completeSubmittedMemoryConsolidation(ctx context.Context, namespace string, task *corev1alpha1.Task,
	submission memoryconsolidation.Submission) (*memoryconsolidation.Report, error) {
	settings := memoryconsolidation.SettingsFor(task.Spec.AI.MemoryConsolidation)
	run, err := h.memoryConsolidationStore.GetMemoryConsolidationRun(ctx, namespace, string(task.UID))
	if err != nil {
		return nil, consolidationStoreError("read memory consolidation run", err)
	}
	switch run.Status {
	case store.MemoryConsolidationRunning:
	case store.MemoryConsolidationAbandoned:
		return nil, errAbandonedMemoryConsolidation
	default:
		return decodeMemoryConsolidationReport(run)
	}
	if submission.Operations == nil {
		return nil, fiber.NewError(fiber.StatusBadRequest, "a memory consolidation result needs its operations list, empty when nothing changes")
	}
	input, err := decodeMemoryConsolidationInput(run)
	if err != nil {
		return nil, err
	}
	current, err := h.currentTargetGenerations(ctx, namespace, input)
	if err != nil {
		return nil, consolidationStoreError("read memory consolidation targets", err)
	}
	var laterTruncated bool
	input.LaterProposals, laterTruncated, err = memoryconsolidation.OpenProposals(ctx, h.memoryProposalStore, namespace)
	if err != nil {
		return nil, consolidationStoreError("read open memory proposals", err)
	}
	input.Truncated = input.Truncated || laterTruncated
	input.ClosedProposals, err = memoryconsolidation.ClosedProposals(ctx, h.memoryProposalStore, namespace, input.Proposals)
	if err != nil {
		return nil, consolidationStoreError("read memory proposals the run read", err)
	}
	kept, rejected, conflicts := memoryconsolidation.Validate(submission, input, current, settings.MaxOperations)
	var proposals []store.MemoryProposal
	if settings.Mode == corev1alpha1.MemoryConsolidationPropose {
		proposals = memoryconsolidation.Proposals(kept, namespace, task.Name, string(task.UID), agentNameOf(task))
	}
	report := newMemoryConsolidationReport(settings, store.MemoryConsolidationCompleted, run, input, kept, rejected, conflicts, submission.Summary, proposals)
	completed, err := h.completeMemoryConsolidation(ctx, run, store.MemoryConsolidationCompleted, report, proposals)
	if err != nil {
		return nil, err
	}
	if completed.Status == store.MemoryConsolidationAbandoned {
		return nil, errAbandonedMemoryConsolidation
	}
	return decodeMemoryConsolidationReport(completed)
}

func (h *InternalHandlers) currentTargetGenerations(ctx context.Context, namespace string, input memoryconsolidation.Input) (map[string]uint64, error) {
	ids := make([]string, 0, len(input.Memories))
	for _, memory := range input.Memories {
		ids = append(ids, memory.ID)
	}
	current := map[string]uint64{}
	if len(ids) == 0 {
		return current, nil
	}
	memories, err := h.memoryStore.ListMemories(ctx, store.MemoryFilter{Namespace: namespace, IDs: ids, Limit: len(ids)})
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
	encoded, err := report.Encode(store.MaxMemoryConsolidationReportBytes)
	if err != nil {
		return nil, fmt.Errorf("encode memory consolidation report: %w", err)
	}
	completed, err := h.memoryConsolidationStore.CompleteMemoryConsolidationRun(ctx, store.MemoryConsolidationCompletion{
		Namespace: run.Namespace, TaskUID: run.TaskUID, Status: status, Report: encoded,
		CompletedAt: time.Now().UTC(), Proposals: proposals,
	})
	if err != nil {
		return nil, consolidationStoreError("complete memory consolidation", err)
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

func finishedMemoryConsolidation(settings memoryconsolidation.Settings, run *store.MemoryConsolidationRun) (memoryconsolidation.BeginResponse, error) {
	report, err := decodeMemoryConsolidationReport(run)
	if err != nil {
		return memoryconsolidation.BeginResponse{}, err
	}
	structure := report.Structure()
	return memoryconsolidation.BeginResponse{Status: run.Status, Settings: settings, Report: &structure}, nil
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

func consolidationStoreError(action string, err error) error {
	if errors.Is(err, store.ErrTaskDataCleanupChanged) || errors.Is(err, store.ErrTaskJobRevoked) {
		return err
	}
	return memoryStoreError(action, "memory consolidation run", err)
}
