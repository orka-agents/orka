/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gofiber/fiber/v3"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/controller"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/taskwait"
)

// The attention read model answers "what needs a person now?" across the
// Tasks of one namespace. It is computed on every read from Task objects,
// their approval event streams, and blocked RepositoryMonitor work actions;
// nothing is persisted. Every item is a short, safe summary that links to the
// per-Task API, which stays the only place approval detail is shown and the
// only place a decision is enforced.

// Attention item kinds, in rank order.
const (
	AttentionKindApproval       = "approval"
	AttentionKindOutcomeUnknown = "outcomeUnknown"
	AttentionKindFailed         = "failed"
	AttentionKindPodWaiting     = "podWaiting"
	AttentionKindMonitorBlocked = "monitorBlocked"
)

// Approval sources reported on approval items.
const (
	AttentionApprovalSourceParked   = "parked"
	AttentionApprovalSourceBrokered = "brokered"
	AttentionApprovalSourceWorker   = "worker"
)

// Monitor source states reported in AttentionSources.
const (
	attentionSourceIncluded    = "included"
	attentionSourceForbidden   = "forbidden"
	attentionSourceUnavailable = "unavailable"
	attentionSourceNotRequired = "notRequested"
)

// Reasons a response is incomplete. A bound that was hit means the ranking
// covers only part of the namespace and must not be read as complete.
const (
	attentionIncompleteTaskScanLimit          = "taskScanLimit"
	attentionIncompleteApprovalCandidateLimit = "approvalCandidateLimit"
	attentionIncompleteApprovalEventLimit     = "approvalEventLimit"
	attentionIncompleteMonitorActionLimit     = "monitorActionLimit"
)

const (
	attentionDefaultSince        = 24 * time.Hour
	attentionMaxSince            = 7 * 24 * time.Hour
	attentionDefaultLimit        = 50
	attentionMaxLimit            = 200
	attentionTaskPageSize        = 500
	attentionMonitorPageSize     = 100
	attentionCursorMaxBytes      = 4096
	attentionCursorVersion       = 1
	attentionSafeTextMaxRunes    = 120
	attentionPodWaitingPrefix    = "pod waiting: "
	attentionPodWaitingUnknown   = "Unknown"
	attentionScopeAttention      = "attention"
	attentionScopeApprovals      = "approvals"
	attentionApprovalStatusQuery = "pending"
)

// attentionBounds limits the work one attention request does. A zero field
// uses its default.
type attentionBounds struct {
	// TaskScan bounds the Tasks listed and filtered.
	TaskScan int
	// ApprovalCandidates bounds how many Tasks' event streams are read, and
	// ApprovalEvents bounds each stream.
	ApprovalCandidates int
	ApprovalEvents     int
	// MonitorActions bounds the blocked work actions scanned.
	MonitorActions int
	// CapabilityTasks bounds the Tasks per page whose decision hints need
	// SubjectAccessReviews or Gateway lookups; later approvals report null.
	CapabilityTasks int
}

var defaultAttentionBounds = attentionBounds{
	TaskScan:           5000,
	ApprovalCandidates: 200,
	ApprovalEvents:     2 * store.MaxExecutionEventLimit,
	MonitorActions:     500,
	CapabilityTasks:    20,
}

func (h *Handlers) attentionLimits() attentionBounds {
	limits := h.attentionBounds
	limits.TaskScan = cmp.Or(limits.TaskScan, defaultAttentionBounds.TaskScan)
	limits.ApprovalCandidates = cmp.Or(limits.ApprovalCandidates, defaultAttentionBounds.ApprovalCandidates)
	limits.ApprovalEvents = cmp.Or(limits.ApprovalEvents, defaultAttentionBounds.ApprovalEvents)
	limits.MonitorActions = cmp.Or(limits.MonitorActions, defaultAttentionBounds.MonitorActions)
	limits.CapabilityTasks = cmp.Or(limits.CapabilityTasks, defaultAttentionBounds.CapabilityTasks)
	return limits
}

// attentionPodWaitingReasons are the container waiting reasons the controller
// reports with the "pod waiting: " message prefix. Any other text is reported
// as Unknown so the response never echoes runtime-supplied detail.
var attentionPodWaitingReasons = map[string]bool{
	"ErrImagePull": true, "ImagePullBackOff": true, "InvalidImageName": true, "CreateContainerConfigError": true,
}

// AttentionResponse is one computed attention ranking page.
type AttentionResponse struct {
	Namespace   string    `json:"namespace"`
	GeneratedAt time.Time `json:"generatedAt"`
	// Since is the start of the window for Failed, OutcomeUnknown and
	// monitor items. The approvals inbox has no window and omits it.
	Since *time.Time      `json:"since,omitempty"`
	Items []AttentionItem `json:"items"`
	// Total and Counts cover every ranked item, not just this page. When
	// Incomplete is true they are lower bounds.
	Total  int            `json:"total"`
	Counts map[string]int `json:"counts"`
	// Incomplete is true when a scan bound was hit. The items are then a
	// partial ranking; IncompleteReasons names each bound.
	Incomplete        bool              `json:"incomplete"`
	IncompleteReasons []string          `json:"incompleteReasons,omitempty"`
	Sources           AttentionSources  `json:"sources"`
	Metadata          AttentionListMeta `json:"metadata"`
}

// AttentionSources reports whether optional sources were included.
type AttentionSources struct {
	// Approvals is included, or unavailable when execution event storage is
	// not enabled and no approval can be read.
	Approvals string `json:"approvals"`
	// Monitors is included, forbidden (the caller may not list
	// repositorymonitors and monitorworkactions), unavailable (no monitor
	// store), or notRequested (the approvals inbox).
	Monitors string `json:"monitors"`
}

// AttentionListMeta carries the opaque cursor for the next page.
type AttentionListMeta struct {
	Continue string `json:"continue,omitempty"`
}

// AttentionItem is one thing that needs a person. It carries only identifiers,
// fixed labels and bounded tool/action names; detail stays on the per-Task API.
type AttentionItem struct {
	ID       string             `json:"id"`
	Kind     string             `json:"kind"`
	Rank     int                `json:"rank"`
	Since    time.Time          `json:"since"`
	Deadline *time.Time         `json:"deadline,omitempty"`
	Summary  string             `json:"summary"`
	Reason   string             `json:"reason,omitempty"`
	Task     *AttentionTaskRef  `json:"task,omitempty"`
	Approval *AttentionApproval `json:"approval,omitempty"`
	Monitor  *AttentionMonitor  `json:"monitor,omitempty"`
}

// AttentionTaskRef identifies the Task an item belongs to.
type AttentionTaskRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
	Phase     string `json:"phase,omitempty"`
}

// AttentionApproval is the safe summary of one decidable approval.
//
// CanApprove and CanDecline are hints computed from the same checks the
// decision endpoint runs: true or false when they settle the answer, null when
// a check could not be run cheaply or gave no answer. The decision endpoint
// stays the enforcement point.
type AttentionApproval struct {
	ID         string     `json:"id"`
	Source     string     `json:"source"`
	Action     string     `json:"action,omitempty"`
	TargetTool string     `json:"targetTool,omitempty"`
	Severity   string     `json:"severity,omitempty"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
	CanApprove *bool      `json:"canApprove"`
	CanDecline *bool      `json:"canDecline"`
}

// AttentionMonitor identifies one blocked RepositoryMonitor work action.
type AttentionMonitor struct {
	Name          string `json:"name"`
	ActionID      string `json:"actionID"`
	TargetKind    string `json:"targetKind,omitempty"`
	TargetNumber  int64  `json:"targetNumber,omitempty"`
	DesiredAction string `json:"desiredAction,omitempty"`
	BlockedReason string `json:"blockedReason,omitempty"`
}

type attentionSortKey struct {
	Rank      int    `json:"r"`
	Deadline  int64  `json:"d"`
	Since     int64  `json:"s"`
	Namespace string `json:"ns"`
	Name      string `json:"n"`
	UID       string `json:"u"`
	ID        string `json:"i"`
}

func compareAttentionSortKeys(a, b attentionSortKey) int {
	return cmp.Or(
		cmp.Compare(a.Rank, b.Rank),
		cmp.Compare(a.Deadline, b.Deadline),
		cmp.Compare(a.Since, b.Since),
		cmp.Compare(a.Namespace, b.Namespace),
		cmp.Compare(a.Name, b.Name),
		cmp.Compare(a.UID, b.UID),
		cmp.Compare(a.ID, b.ID),
	)
}

type attentionCursor struct {
	Version   int              `json:"v"`
	Scope     string           `json:"sc"`
	Namespace string           `json:"ns"`
	Since     int64            `json:"t,omitempty"`
	After     attentionSortKey `json:"k"`
}

func encodeAttentionCursor(cursor attentionCursor) string {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeAttentionCursor(value, scope, namespace string) (*attentionCursor, error) {
	invalid := fiber.NewError(fiber.StatusBadRequest, "invalid continue token")
	if len(value) > attentionCursorMaxBytes {
		return nil, invalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, invalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	cursor := &attentionCursor{}
	if err := decoder.Decode(cursor); err != nil || decoder.More() {
		return nil, invalid
	}
	if cursor.Version != attentionCursorVersion || cursor.Scope != scope || cursor.Namespace != namespace {
		return nil, invalid
	}
	if scope == attentionScopeAttention && cursor.Since <= 0 {
		return nil, invalid
	}
	return cursor, nil
}

// attentionEntry is a ranked item plus the Task it came from, kept only on
// the server to compute decision capabilities for the returned page.
type attentionEntry struct {
	item AttentionItem
	key  attentionSortKey
	task *corev1alpha1.Task
}

type attentionScan struct {
	entries    []attentionEntry
	incomplete map[string]bool
	monitors   string
	approvals  string
}

func (s *attentionScan) markIncomplete(reason string) {
	if s.incomplete == nil {
		s.incomplete = map[string]bool{}
	}
	s.incomplete[reason] = true
}

type attentionRequest struct {
	scope     string
	namespace string
	since     time.Time
	limit     int
	after     *attentionSortKey
	now       time.Time
}

// ListAttention handles GET /api/v1/attention.
func (h *Handlers) ListAttention(c fiber.Ctx) error {
	return h.serveAttention(c, attentionScopeAttention)
}

// ListPendingApprovals handles GET /api/v1/approvals?status=pending.
func (h *Handlers) ListPendingApprovals(c fiber.Ctx) error {
	if status := strings.TrimSpace(c.Query("status", attentionApprovalStatusQuery)); status != attentionApprovalStatusQuery {
		return fiber.NewError(fiber.StatusBadRequest, "status must be pending")
	}
	if h.executionEventStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "execution event storage not enabled")
	}
	return h.serveAttention(c, attentionScopeApprovals)
}

func (h *Handlers) serveAttention(c fiber.Ctx, scope string) error {
	action := "listAttention"
	if scope == attentionScopeApprovals {
		action = "listPendingApprovals"
	}
	namespace, err := h.resolveNamespace(c, c.Query(toolNamespaceArg, ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, action, h.contextTokenAuthorization.TaskListScopes); err != nil {
		return err
	}
	req, err := parseAttentionRequest(c, scope, namespace, time.Now().UTC())
	if err != nil {
		return err
	}

	scan, err := h.scanAttention(c, action, req)
	if err != nil {
		return err
	}
	slices.SortFunc(scan.entries, func(a, b attentionEntry) int { return compareAttentionSortKeys(a.key, b.key) })

	counts := map[string]int{}
	for _, entry := range scan.entries {
		counts[entry.item.Kind]++
	}
	start := 0
	if req.after != nil {
		start, _ = slices.BinarySearchFunc(scan.entries, *req.after, func(entry attentionEntry, key attentionSortKey) int {
			return compareAttentionSortKeys(entry.key, key)
		})
		for start < len(scan.entries) && compareAttentionSortKeys(scan.entries[start].key, *req.after) <= 0 {
			start++
		}
	}
	end := min(start+req.limit, len(scan.entries))
	page := scan.entries[start:end]

	checker := newAttentionDecisionChecker(h, c)
	items := make([]AttentionItem, 0, len(page))
	for i := range page {
		entry := page[i]
		if entry.item.Approval != nil {
			entry.item.Approval.CanApprove, entry.item.Approval.CanDecline = checker.capabilities(entry.task)
		}
		items = append(items, entry.item)
	}

	response := AttentionResponse{
		Namespace:   namespace,
		GeneratedAt: req.now,
		Items:       items,
		Total:       len(scan.entries),
		Counts:      counts,
		Sources:     AttentionSources{Approvals: scan.approvals, Monitors: scan.monitors},
	}
	if scope == attentionScopeAttention {
		since := req.since
		response.Since = &since
	}
	for reason := range scan.incomplete {
		response.IncompleteReasons = append(response.IncompleteReasons, reason)
	}
	slices.Sort(response.IncompleteReasons)
	response.Incomplete = len(response.IncompleteReasons) > 0
	if end < len(scan.entries) && end > 0 {
		cursor := attentionCursor{Version: attentionCursorVersion, Scope: scope, Namespace: namespace, After: page[len(page)-1].key}
		if scope == attentionScopeAttention {
			cursor.Since = req.since.UnixNano()
		}
		response.Metadata.Continue = encodeAttentionCursor(cursor)
	}
	return c.JSON(response)
}

func parseAttentionRequest(c fiber.Ctx, scope, namespace string, now time.Time) (attentionRequest, error) {
	req := attentionRequest{scope: scope, namespace: namespace, limit: attentionDefaultLimit, now: now}
	if raw := strings.TrimSpace(c.Query("limit", "")); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > attentionMaxLimit {
			return req, fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("limit must be between 1 and %d", attentionMaxLimit))
		}
		req.limit = limit
	}
	rawSince := strings.TrimSpace(c.Query("since", ""))
	if scope == attentionScopeApprovals && rawSince != "" {
		return req, fiber.NewError(fiber.StatusBadRequest, "since is not supported for approvals")
	}
	if token := strings.TrimSpace(c.Query("continue", "")); token != "" {
		if rawSince != "" {
			return req, fiber.NewError(fiber.StatusBadRequest, "since cannot be combined with continue; the window is carried by the token")
		}
		cursor, err := decodeAttentionCursor(token, scope, namespace)
		if err != nil {
			return req, err
		}
		req.after = &cursor.After
		if scope == attentionScopeAttention {
			req.since = time.Unix(0, cursor.Since).UTC()
		}
		return req, nil
	}
	if scope != attentionScopeAttention {
		return req, nil
	}
	req.since = now.Add(-attentionDefaultSince)
	if rawSince != "" {
		since, err := parseAttentionSince(rawSince, now)
		if err != nil {
			return req, err
		}
		req.since = since
	}
	return req, nil
}

// parseAttentionSince accepts a positive Go duration (such as 24h) or an
// RFC 3339 timestamp, at most attentionMaxSince in the past.
func parseAttentionSince(raw string, now time.Time) (time.Time, error) {
	invalid := fiber.NewError(fiber.StatusBadRequest,
		fmt.Sprintf("since must be a positive duration or RFC 3339 time within %s", attentionMaxSince))
	var since time.Time
	if duration, err := time.ParseDuration(raw); err == nil {
		if duration <= 0 || duration > attentionMaxSince {
			return time.Time{}, invalid
		}
		since = now.Add(-duration)
	} else if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		since = parsed.UTC()
		if since.After(now) || now.Sub(since) > attentionMaxSince {
			return time.Time{}, invalid
		}
	} else {
		return time.Time{}, invalid
	}
	return since, nil
}

// scanAttention authorizes before it reads: the route policy and the
// TaskListScopes check already passed, each listed Task then passes the same
// per-item context-token and Gateway read filters as ListTasks, and only then
// is a candidate Task's approval event stream read.
func (h *Handlers) scanAttention(c fiber.Ctx, action string, req attentionRequest) (*attentionScan, error) {
	ctx := c.Context()
	limits := h.attentionLimits()
	scan := &attentionScan{monitors: attentionSourceNotRequired, approvals: attentionSourceIncluded}
	if h.executionEventStore == nil {
		scan.approvals = attentionSourceUnavailable
	}
	gatewayAuthorizations := map[gatewayTaskAuthorizationKey]bool{}
	approvalCandidates := 0
	scanned := 0
	continueToken := ""
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		taskList := &corev1alpha1.TaskList{}
		opts := &client.ListOptions{Namespace: req.namespace, Limit: attentionTaskPageSize, Continue: continueToken}
		if err := h.listPage(ctx, taskList, opts, "tasks"); err != nil {
			return nil, err
		}
		for i := range taskList.Items {
			if scanned >= limits.TaskScan {
				scan.markIncomplete(attentionIncompleteTaskScanLimit)
				break
			}
			scanned++
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			task := &taskList.Items[i]
			allowed, err := h.attentionTaskVisible(c, action, task, gatewayAuthorizations)
			if err != nil {
				return nil, err
			}
			if !allowed {
				continue
			}
			if req.scope == attentionScopeAttention {
				if entry, ok := attentionTaskStateEntry(task, req.since); ok {
					scan.entries = append(scan.entries, entry)
				}
			}
			if !attentionApprovalCandidate(task) {
				continue
			}
			readable, err := h.contextTokenAllowsTaskRead(c, action, task)
			if err != nil {
				return nil, err
			}
			if !readable {
				continue
			}
			if approvalCandidates >= limits.ApprovalCandidates {
				scan.markIncomplete(attentionIncompleteApprovalCandidateLimit)
				continue
			}
			approvalCandidates++
			entries, complete, err := h.attentionApprovalEntries(ctx, task, req.now, limits.ApprovalEvents)
			if err != nil {
				return nil, err
			}
			if !complete {
				scan.markIncomplete(attentionIncompleteApprovalEventLimit)
				continue
			}
			if len(entries) > 0 {
				// Keep one trimmed copy for the decision hints, not the page.
				kept := task.DeepCopy()
				kept.ManagedFields = nil
				for i := range entries {
					entries[i].task = kept
				}
			}
			scan.entries = append(scan.entries, entries...)
		}
		continueToken = taskList.Continue
		if continueToken == "" || scan.incomplete[attentionIncompleteTaskScanLimit] {
			break
		}
	}
	if req.scope == attentionScopeAttention {
		if err := h.scanAttentionMonitors(c, req, scan); err != nil {
			return nil, err
		}
	}
	return scan, nil
}

func (h *Handlers) attentionTaskVisible(c fiber.Ctx, action string, task *corev1alpha1.Task, cache map[gatewayTaskAuthorizationKey]bool) (bool, error) {
	allowed := true
	if h.contextTokenAuthorization.Enabled() {
		var err error
		allowed, err = h.contextTokenAllowsLoadedTask(c, action, task)
		if err != nil {
			return false, err
		}
	}
	if !allowed {
		return false, nil
	}
	return h.taskAccess().gatewayTaskReadableCached(c, action, task, cache)
}

// contextTokenAllowsTaskRead applies the per-Task read check the per-Task
// approvals route runs (Task read scopes and the token's Task identity), so
// the attention view never shows approval data that route would refuse.
func (h *Handlers) contextTokenAllowsTaskRead(c fiber.Ctx, action string, task *corev1alpha1.Task) (bool, error) {
	cfg := h.contextTokenAuthorization
	if !cfg.Enabled() {
		return true, nil
	}
	ui := GetUserInfo(c)
	if ui == nil || ui.AuthType != AuthTypeContextToken || ui.ContextToken == nil {
		return true, nil
	}
	failures := contextTokenTaskReadFailures(ui.ContextToken, cfg, task.Namespace, task.Name)
	if len(failures) == 0 {
		return true, nil
	}
	if cfg.enforcing() {
		return false, nil
	}
	return true, h.handleContextTokenAuthorizationFailures(ui.ContextToken, action, failures)
}

// attentionApprovalCandidate reports, from the Task object alone, whether its
// approval stream may hold a decidable approval: a live Task the controller
// marks as waiting for approval, or a Task parked on an approval that still
// takes a decision.
func attentionApprovalCandidate(task *corev1alpha1.Task) bool {
	if task == nil || !task.DeletionTimestamp.IsZero() {
		return false
	}
	if wait := task.Status.Wait; wait != nil && wait.Kind == corev1alpha1.TaskWaitKindApproval && len(wait.References) == 1 &&
		taskwait.AcceptsApprovalDecision(task, wait.References[0]) {
		return true
	}
	if task.Status.Phase.IsTerminal() {
		return false
	}
	if condition := meta.FindStatusCondition(task.Status.Conditions, controller.ConditionTypeWaitingForApproval); condition != nil &&
		condition.Status == metav1.ConditionTrue {
		return true
	}
	return strings.HasPrefix(task.Status.Message, "waiting for approval ")
}

// attentionApprovalEntries reads one candidate Task's approval stream and
// returns its decidable approvals. complete is false when the stream is longer
// than the per-Task bound; the Task's approvals are then left out rather than
// derived from a prefix that could miss a later decision.
func (h *Handlers) attentionApprovalEntries(ctx context.Context, task *corev1alpha1.Task, now time.Time, maxEvents int) ([]attentionEntry, bool, error) {
	if h.executionEventStore == nil {
		return nil, true, nil
	}
	listed, complete, err := h.listBoundedTaskApprovalEvents(ctx, task.Namespace, task.Name, maxEvents)
	if err != nil {
		return nil, false, fiber.NewError(fiber.StatusInternalServerError, "failed to list approval events")
	}
	if !complete {
		return nil, false, nil
	}
	var entries []attentionEntry
	for _, approval := range deriveTaskApprovalState(filterTaskApprovalEvents(listed, task)) {
		source, decidable := attentionApprovalDecidable(task, approval, now)
		if !decidable {
			continue
		}
		entries = append(entries, attentionApprovalEntry(task, approval, source))
	}
	return entries, true, nil
}

func (h *Handlers) listBoundedTaskApprovalEvents(ctx context.Context, namespace, taskName string, maxEvents int) ([]store.ExecutionEvent, bool, error) {
	reader := newTaskTimelineReader(h.executionEventStore, namespace, taskName)
	out := []store.ExecutionEvent{}
	var after int64
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		batch, err := reader.list(ctx, after, store.MaxExecutionEventLimit, approvals.EventTypes())
		if err != nil {
			return nil, false, err
		}
		if len(batch) == 0 {
			return out, true, nil
		}
		if len(out)+len(batch) > maxEvents {
			return nil, false, nil
		}
		out = append(out, batch...)
		after = batch[len(batch)-1].Seq
		if len(batch) < store.MaxExecutionEventLimit {
			return out, true, nil
		}
	}
}

// attentionApprovalDecidable mirrors the state checks of DecideTaskApproval
// and drops approvals whose deadline has passed by the clock, even before the
// controller records their expiry.
func attentionApprovalDecidable(task *corev1alpha1.Task, approval approvals.Approval, now time.Time) (string, bool) {
	if approval.Status != approvals.StatusPending || !task.DeletionTimestamp.IsZero() {
		return "", false
	}
	if approval.ExpiresAt != nil && !now.Before(*approval.ExpiresAt) {
		return "", false
	}
	wait := task.Status.Wait
	parkedWait := wait != nil && wait.Kind == corev1alpha1.TaskWaitKindApproval && len(wait.References) == 1 && wait.References[0] == approval.ID
	switch {
	case approval.Binding != nil:
		if validateV2ApprovalTask(approval, task) != nil || task.Status.Phase.IsTerminal() {
			return "", false
		}
		return AttentionApprovalSourceBrokered, true
	case parkedWait || approval.Action == taskwait.ParkedApprovalAction:
		// A parked sign-off is decidable only while its wait is armed.
		if !taskwait.AcceptsApprovalDecision(task, approval.ID) {
			return "", false
		}
		if !wait.Deadline.IsZero() && !now.Before(wait.Deadline.Time) {
			return "", false
		}
		return AttentionApprovalSourceParked, true
	default:
		if task.Status.Phase.IsTerminal() {
			return "", false
		}
		return AttentionApprovalSourceWorker, true
	}
}

func attentionApprovalEntry(task *corev1alpha1.Task, approval approvals.Approval, source string) attentionEntry {
	action := attentionSafeText(approval.Action)
	tool := attentionSafeText(approval.TargetTool)
	summary := "Approval requested"
	switch {
	case source == AttentionApprovalSourceParked:
		summary = "Agent sign-off requested"
	case tool != "":
		summary = "Approval requested for " + tool
	case action != "":
		summary = "Approval requested: " + action
	}
	var expiresAt *time.Time
	if approval.ExpiresAt != nil {
		value := approval.ExpiresAt.UTC()
		expiresAt = &value
	} else if wait := task.Status.Wait; source == AttentionApprovalSourceParked && wait != nil && !wait.Deadline.IsZero() {
		value := wait.Deadline.UTC()
		expiresAt = &value
	}
	item := AttentionItem{
		ID:       "approval:" + string(task.UID) + ":" + approval.ID,
		Kind:     AttentionKindApproval,
		Rank:     attentionRank(AttentionKindApproval),
		Since:    approval.CreatedAt.UTC(),
		Deadline: expiresAt,
		Summary:  summary,
		Task:     attentionTaskRef(task),
		Approval: &AttentionApproval{
			ID:         approval.ID,
			Source:     source,
			Action:     action,
			TargetTool: tool,
			Severity:   attentionSafeSeverity(approval.Severity),
			ExpiresAt:  expiresAt,
		},
	}
	return attentionEntry{item: item, key: attentionItemSortKey(item, task.Namespace, task.Name, string(task.UID)), task: task}
}

// attentionTaskStateEntry classifies a Task's own state: an unprovable
// outcome or a failure inside the window, or a running Pod that cannot start.
func attentionTaskStateEntry(task *corev1alpha1.Task, since time.Time) (attentionEntry, bool) {
	if task == nil || !task.DeletionTimestamp.IsZero() {
		return attentionEntry{}, false
	}
	var kind, summary, reason string
	var at time.Time
	execution := task.Status.Execution
	switch {
	case execution != nil && (execution.State == corev1alpha1.TaskExecutionStateOutcomeUnknown ||
		execution.Outcome == corev1alpha1.TaskExecutionOutcomeOutcomeUnknown):
		kind, summary = AttentionKindOutcomeUnknown, "Task outcome is unknown"
		at = attentionTaskEndedAt(task)
	case task.Status.Phase == corev1alpha1.TaskPhaseFailed:
		kind, summary = AttentionKindFailed, "Task failed"
		at = attentionTaskEndedAt(task)
	case task.Status.Phase == corev1alpha1.TaskPhaseRunning && strings.HasPrefix(task.Status.Message, attentionPodWaitingPrefix):
		kind = AttentionKindPodWaiting
		reason = attentionPodWaitingReason(task.Status.Message)
		summary = "Pod cannot start: " + reason
		at = task.CreationTimestamp.Time
		if task.Status.StartTime != nil {
			at = task.Status.StartTime.Time
		}
	default:
		return attentionEntry{}, false
	}
	if kind != AttentionKindPodWaiting && at.Before(since) {
		return attentionEntry{}, false
	}
	item := AttentionItem{
		ID:      kind + ":" + string(task.UID),
		Kind:    kind,
		Rank:    attentionRank(kind),
		Since:   at.UTC(),
		Summary: summary,
		Reason:  reason,
		Task:    attentionTaskRef(task),
	}
	return attentionEntry{item: item, key: attentionItemSortKey(item, task.Namespace, task.Name, string(task.UID))}, true
}

func attentionTaskEndedAt(task *corev1alpha1.Task) time.Time {
	if task.Status.CompletionTime != nil {
		return task.Status.CompletionTime.Time
	}
	latest := task.CreationTimestamp.Time
	for _, condition := range task.Status.Conditions {
		if condition.LastTransitionTime.After(latest) {
			latest = condition.LastTransitionTime.Time
		}
	}
	return latest
}

func attentionPodWaitingReason(message string) string {
	rest := strings.TrimPrefix(message, attentionPodWaitingPrefix)
	reason, _, _ := strings.Cut(rest, " ")
	if attentionPodWaitingReasons[reason] {
		return reason
	}
	return attentionPodWaitingUnknown
}

func attentionTaskRef(task *corev1alpha1.Task) *AttentionTaskRef {
	return &AttentionTaskRef{Namespace: task.Namespace, Name: task.Name, UID: string(task.UID), Phase: string(task.Status.Phase)}
}

func attentionRank(kind string) int {
	switch kind {
	case AttentionKindApproval:
		return 1
	case AttentionKindOutcomeUnknown:
		return 2
	case AttentionKindFailed:
		return 3
	case AttentionKindPodWaiting:
		return 4
	default:
		return 5
	}
}

func attentionItemSortKey(item AttentionItem, namespace, name, uid string) attentionSortKey {
	deadline := int64(math.MaxInt64)
	if item.Deadline != nil {
		deadline = item.Deadline.UnixNano()
	}
	return attentionSortKey{
		Rank: item.Rank, Deadline: deadline, Since: item.Since.UnixNano(),
		Namespace: namespace, Name: name, UID: uid, ID: item.ID,
	}
}

// attentionSafeText bounds a tool or action name: control and format
// characters are dropped, whitespace is collapsed and the result is cut to a
// short label.
func attentionSafeText(value string) string {
	var b strings.Builder
	space := false
	runes := 0
	for _, r := range strings.TrimSpace(value) {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			continue
		}
		if space && b.Len() > 0 {
			b.WriteRune(' ')
			runes++
		}
		space = false
		if runes >= attentionSafeTextMaxRunes {
			b.WriteRune('…')
			break
		}
		b.WriteRune(r)
		runes++
	}
	return b.String()
}

func attentionSafeSeverity(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "info", "warning", "critical":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return ""
	}
}

// attentionSafeCode keeps short machine codes such as a work action's blocked
// reason or desired action, and drops anything that looks like free text.
func attentionSafeCode(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 {
		return ""
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' && r != '.' {
			return ""
		}
	}
	return value
}

// scanAttentionMonitors adds blocked RepositoryMonitor work actions inside the
// window. Monitors are authorized separately from Tasks: a TokenReview caller
// needs list on repositorymonitors and monitorworkactions, a context-token
// caller needs the monitor read scopes, and each monitor passes the same
// context-token filter as the monitor list. A caller without that access
// still gets the Task items, with sources.monitors set to forbidden.
func (h *Handlers) scanAttentionMonitors(c fiber.Ctx, req attentionRequest, scan *attentionScan) error {
	if h.repositoryMonitorStore == nil {
		scan.monitors = attentionSourceUnavailable
		return nil
	}
	allowed, err := h.attentionMonitorsAllowed(c, req.namespace)
	if err != nil {
		return err
	}
	if !allowed {
		scan.monitors = attentionSourceForbidden
		return nil
	}
	scan.monitors = attentionSourceIncluded
	ctx := c.Context()
	maxActions := h.attentionLimits().MonitorActions
	monitorList := &corev1alpha1.RepositoryMonitorList{}
	if err := h.client.List(ctx, monitorList, client.InNamespace(req.namespace)); err != nil {
		return listPageError("repository monitors", err)
	}
	monitors := map[string]bool{}
	for i := range monitorList.Items {
		monitor := &monitorList.Items[i]
		if h.contextTokenRepositoryMonitorAllowed(c, monitor) {
			monitors[monitor.Name] = true
		}
	}
	if len(monitors) == 0 {
		return nil
	}
	type target struct {
		monitor, kind string
		number        int64
	}
	seen := map[target]bool{}
	scanned := 0
	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		actions, next, err := h.repositoryMonitorStore.ListWorkActions(ctx, store.WorkActionFilter{
			Namespace: req.namespace,
			Status:    store.RepositoryMonitorWorkActionStatusBlocked,
			Limit:     attentionMonitorPageSize,
			Cursor:    cursor,
		})
		if err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "failed to list monitor workflow actions")
		}
		for i := range actions {
			action := &actions[i]
			// Actions arrive newest first; the rest of the scan is older
			// than the window.
			if action.UpdatedAt.Before(req.since) {
				return nil
			}
			if scanned >= maxActions {
				scan.markIncomplete(attentionIncompleteMonitorActionLimit)
				return nil
			}
			scanned++
			if action.MonitorNamespace != req.namespace || !monitors[action.MonitorName] {
				continue
			}
			key := target{monitor: action.MonitorName, kind: action.TargetKind, number: action.TargetNumber}
			if seen[key] {
				continue
			}
			seen[key] = true
			scan.entries = append(scan.entries, attentionMonitorEntry(action))
		}
		if next == "" || len(actions) == 0 {
			return nil
		}
		cursor = next
	}
}

func (h *Handlers) attentionMonitorsAllowed(c fiber.Ctx, namespace string) (bool, error) {
	ui := GetUserInfo(c)
	if ui != nil && ui.AuthType == AuthTypeTokenReview {
		for _, resource := range []string{usageMonitorResource, "monitorworkactions"} {
			allowed, err := kubernetesResourceActionAllowed(c.Context(), h.clientset, ui, namespace, "list",
				corev1alpha1.GroupVersion.Group, resource, "")
			if err != nil || !allowed {
				return false, nil
			}
		}
	}
	if err := h.authorizeContextTokenAction(c, "listAttentionMonitors", h.contextTokenAuthorization.MonitorReadScopes); err != nil {
		var fiberErr *fiber.Error
		if errors.As(err, &fiberErr) && fiberErr.Code == fiber.StatusForbidden {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func attentionMonitorEntry(action *store.WorkAction) attentionEntry {
	desired := attentionSafeCode(action.DesiredAction)
	summary := "Monitor action blocked"
	if desired != "" {
		summary = "Monitor " + desired + " blocked"
	}
	kind := attentionSafeCode(action.TargetKind)
	if kind != "" && action.TargetNumber > 0 {
		summary += fmt.Sprintf(" on %s #%d", kind, action.TargetNumber)
	}
	item := AttentionItem{
		ID:      "monitor:" + action.MonitorName + ":" + action.ID,
		Kind:    AttentionKindMonitorBlocked,
		Rank:    attentionRank(AttentionKindMonitorBlocked),
		Since:   action.UpdatedAt.UTC(),
		Summary: summary,
		Reason:  attentionSafeCode(action.BlockedReason),
		Monitor: &AttentionMonitor{
			Name:          action.MonitorName,
			ActionID:      action.ID,
			TargetKind:    kind,
			TargetNumber:  action.TargetNumber,
			DesiredAction: desired,
			BlockedReason: attentionSafeCode(action.BlockedReason),
		},
	}
	return attentionEntry{item: item, key: attentionItemSortKey(item, action.MonitorNamespace, action.MonitorName, "")}
}

// attentionDecisionChecker computes approve/decline hints for the approvals
// on one page by running the checks DecideTaskApproval runs, without side
// effects: the context-token Task update scopes (once per request), the
// route's named SubjectAccessReviews for TokenReview callers (two per Task,
// for at most attentionBounds.CapabilityTasks Tasks), the Gateway operate check
// (once per Gateway), and the requester-only approve rule (no I/O).
type attentionDecisionChecker struct {
	h            *Handlers
	c            fiber.Ctx
	ui           *UserInfo
	scopeChecked bool
	scopeAllowed bool
	byTask       map[string]*bool
	gateways     map[gatewayTaskAuthorizationKey]*bool
	reviewed     int
	maxReviewed  int
}

func newAttentionDecisionChecker(h *Handlers, c fiber.Ctx) *attentionDecisionChecker {
	return &attentionDecisionChecker{
		h: h, c: c, ui: GetUserInfo(c), maxReviewed: h.attentionLimits().CapabilityTasks,
		byTask: map[string]*bool{}, gateways: map[gatewayTaskAuthorizationKey]*bool{},
	}
}

//go:fix inline

func (d *attentionDecisionChecker) capabilities(task *corev1alpha1.Task) (canApprove, canDecline *bool) {
	base := d.decide(task)
	if requireRequesterApprovesDelegatedWrite(task, d.ui) != nil {
		// Only the requester may approve; this caller is not them.
		return new(false), base
	}
	return base, base
}

func (d *attentionDecisionChecker) decide(task *corev1alpha1.Task) *bool {
	if task == nil || !task.DeletionTimestamp.IsZero() {
		return new(false)
	}
	if !d.scopeChecked {
		d.scopeChecked = true
		d.scopeAllowed = d.h.authorizeContextTokenAction(d.c, "attentionDecideTaskApproval",
			d.h.contextTokenAuthorization.TaskUpdateScopes) == nil
	}
	if !d.scopeAllowed {
		return new(false)
	}
	key := string(task.UID) + "/" + task.Name
	if result, ok := d.byTask[key]; ok {
		return result
	}
	result := d.decideTask(task)
	d.byTask[key] = result
	return result
}

func (d *attentionDecisionChecker) decideTask(task *corev1alpha1.Task) *bool {
	access := d.h.taskAccess()
	// The same ownership lookup the read filter already ran for this Task.
	identity, gatewayOwned, err := access.gatewayTaskIdentity(d.c, task)
	if err != nil {
		return nil
	}
	gatewayKey := gatewayTaskAuthorizationKey{
		GatewayNamespace: identity.GatewayNamespace, NamespaceUID: identity.NamespaceUID,
		GatewayName: identity.GatewayName, GatewayUID: identity.GatewayUID,
	}
	_, gatewayCached := d.gateways[gatewayKey]
	tokenReview := d.ui != nil && d.ui.AuthType == AuthTypeTokenReview
	if tokenReview || (gatewayOwned && !gatewayCached) {
		// Every Task past the budget would cost new reviews or lookups.
		if d.reviewed >= d.maxReviewed {
			return nil
		}
		d.reviewed++
	}
	if tokenReview {
		policy := externalAPIPolicies["POST /api/v1/tasks/:id/approvals/:approvalID/decision"]
		for _, permission := range policy.permissions {
			name := ""
			if permission.nameParam != "" {
				name = task.Name
			}
			allowed, err := kubernetesResourceActionAllowed(d.c.Context(), d.h.clientset, d.ui, task.Namespace,
				permission.verb, permission.group, permission.resource, name)
			if err != nil {
				return nil
			}
			if !allowed {
				return new(false)
			}
		}
	}
	if !gatewayOwned {
		return new(true)
	}
	if result, ok := d.gateways[gatewayKey]; ok {
		return result
	}
	var result *bool
	err = access.authorizeGatewayTaskIdentity(d.c, "attentionDecideTaskApproval", task, identity, true)
	var fiberErr *fiber.Error
	switch {
	case err == nil:
		result = new(true)
	case errors.As(err, &fiberErr) && (fiberErr.Code == fiber.StatusForbidden || fiberErr.Code == fiber.StatusNotFound):
		result = new(false)
	}
	d.gateways[gatewayKey] = result
	return result
}
