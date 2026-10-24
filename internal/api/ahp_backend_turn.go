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
	"path"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/ahphost"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/liveevents"
	"github.com/orka-agents/orka/internal/store"
)

const (
	ahpTurnPollInterval = 2 * time.Second
	ahpTitleRunes       = 60
	ahpOptionApprove    = "approve"
	ahpOptionDeny       = "deny"
	// ahpFailureTextMax bounds the runtime detail a failed turn reports.
	ahpFailureTextMax = 500
	// ahpToolOutputMax bounds the output a running tool call shows.
	ahpToolOutputMax     = 32 << 10
	ahpPlanToolName      = "plan"
	ahpUsageScopeSession = "session"
	// ahpExchangeMax bounds each message and reply the history keeps, and
	// ahpHistoryMax how many exchanges; ahpCarryOverMax bounds the history a
	// new Orka Session starts with.
	ahpExchangeMax  = 4 << 10
	ahpHistoryMax   = 20
	ahpCarryOverMax = 24 << 10
	// ahpGenericPromptFailure is the Task message for a prompt that failed
	// without a more specific reason.
	ahpGenericPromptFailure = "prompt failed"
)

// ahpTurn follows one turn's Task and turns its events into chat actions.
type ahpTurn struct {
	b         *ahpBackend
	principal ahphost.Principal
	session   ahpSession
	id        string
	task      string
	startedAt time.Time
	emit      ahphost.Emitter
	ctx       context.Context
	cancel    context.CancelFunc

	mu              sync.Mutex
	clientCancelled bool
	tools           map[string]*ahpToolCall
	order           []string
	textPart        string
	thoughtPart     string
	streamedText    bool
	// lastText is the end of the newest markdown part, kept for failures.
	lastText string
	// reply is the end of the turn's text, kept as the conversation's history.
	reply string
	// failure is the runtime's own reason when the prompt failed.
	failure string
	// planCall is the synthetic tool call that shows the agent's plan.
	planCall string
}

type ahpToolCall struct {
	started, ready, completed bool
	title                     string
	// approval is the held permission request waiting for the person.
	approval string
	// output is the running call's text output so far, bounded.
	output string
}

func ahpTurnKey(userKey, session string) string { return userKey + "\x00" + session }

func (b *ahpBackend) activeTurn(userKey, session string) *ahpTurn {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.turns[ahpTurnKey(userKey, session)]
}

func ahpStatus(status ahptypes.SessionStatus) *ahptypes.SessionStatus { return new(status) }

func ahpNow(b *ahpBackend) string { return b.now().UTC().Format(time.RFC3339Nano) }

func ahpTitle(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) <= ahpTitleRunes {
		return text
	}
	runes := []rune(text)
	return string(runes[:ahpTitleRunes-1]) + "…"
}

// startTurn creates the turn's Task as the person and follows it.
func (b *ahpBackend) startTurn(ctx context.Context, principal ahphost.Principal, record ahpSession, action *ahptypes.ChatTurnStartedAction, emit ahphost.Emitter) {
	started := b.now().UTC()
	// The slot reserved when the message was admitted passes to the running
	// turn, or is freed if the turn never starts; then a waiting message may
	// start instead. It is freed once: a turn that fails frees it before the
	// person sees the error, so a message sent straight back is admitted, and
	// a later release can never free that message's slot.
	failed, released := false, false
	release := func() {
		if !released {
			released = true
			b.releaseTurn(principal.UserKey, record.Session)
		}
	}
	defer func() {
		release()
		if failed {
			b.runPending(context.WithoutCancel(ctx), principal.UserKey, record.Session, emit)
		}
	}()
	fail := func(message string) {
		failed = true
		release()
		b.emitTurnError(ctx, emit, record, action.TurnId, started, "TurnNotStarted", message)
	}
	// Start from the session as stored now: a change accepted since the
	// message was admitted, such as the approval mode, applies to this turn.
	var current ahpSession
	if err := b.getRecord(ctx, principal.UserKey, ahpRecordSession, record.Session, &current); errors.Is(err, store.ErrNotFound) {
		// The session was disposed of while the message waited: run nothing.
		return
	} else if err == nil {
		record = current
	}
	choice, switching, err := b.resolveModel(ctx, principal, record, action.Message.Model)
	if err != nil {
		if rejection, ok := errors.AsType[*ahphost.Rejection](err); ok {
			fail(rejection.Reason)
		} else {
			fail("the model could not be resolved")
		}
		return
	}
	note := ""
	if switching {
		if record, note, err = b.nextGeneration(ctx, principal, record, choice, emit); err != nil {
			if rejection, ok := errors.AsType[*ahphost.Rejection](err); ok {
				fail(rejection.Reason)
			} else {
				fail("the session could not switch models")
				log.Error(err, "switch AHP session model", "session", record.Session)
			}
			return
		}
	}
	number := record.Turns + 1
	name := fmt.Sprintf("%s-t%d", record.OrkaSession, number)
	first := record.Turns == 0 || (record.Generation > 0 && record.SessionTurns == 0)
	prompt := action.Message.Text
	if first && len(record.History) > 0 {
		prompt = ahpCarryOver(record.History) + prompt
	}
	task := b.turnTask(ctx, principal, record, choice, name, prompt, first)
	ui := ahpPrincipalUserInfo(principal)

	// Subscribe before the Task exists so no event of the turn is missed.
	sub := b.live.Subscribe(record.Namespace, name)
	if err := authorizeAndStampToolTaskCreate(ctx, b.h.client, b.h.clientset, ui.ContextToken, b.h.contextTokenAuthorization, "ahpTurn", ui, task); err != nil {
		sub.Close()
		fail(ahpErrorMessage(err))
		return
	}
	turnCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	turn := &ahpTurn{
		b: b, principal: principal, session: record, id: action.TurnId, task: name, startedAt: started, emit: emit,
		ctx: turnCtx, cancel: cancel, tools: map[string]*ahpToolCall{},
	}
	// The turn is recorded, and registered for this process, before its Task
	// exists. A controller that stops after creating the Task leaves a turn
	// that recovery follows to its end, and one that stops before leaves a
	// turn recovery settles as deleted; neither leaves a Task nothing finds.
	// Disposal holds the user's lifecycle lock while it cancels the session's
	// active Task and deletes the session. Holding it through the Task's
	// creation means disposal either finds and cancels the created Task, or
	// has already deleted the session, and then nothing is created.
	unlock := b.lockUser(principal.UserKey)
	err = b.updateSession(ctx, principal.UserKey, record.Session, func(s *ahpSession) {
		s.Agent, s.Model, s.Turns = choice.Agent, choice.Model, number
		s.SessionTurns++
		s.ActiveTurn, s.ActiveTask, s.ActiveStartedAt = action.TurnId, name, started.Format(time.RFC3339Nano)
		s.History = ahpKeepHistory(append(s.History, ahpExchange{User: ahpBounded(action.Message.Text, ahpExchangeMax)}))
		turn.session = *s
	})
	if err != nil {
		unlock()
		sub.Close()
		cancel()
		if !errors.Is(err, store.ErrNotFound) {
			log.Error(err, "record AHP turn", "session", record.Session, "task", name)
			fail("the turn could not be recorded")
		}
		return
	}
	turnKey := ahpTurnKey(principal.UserKey, record.Session)
	b.mu.Lock()
	b.turns[turnKey] = turn
	b.mu.Unlock()
	if err := b.h.client.Create(ctx, task); err != nil {
		// The Task may exist even so; stop it, then take the turn back.
		b.cancelTask(context.WithoutCancel(ctx), principal, task.Namespace, name)
		logSessionUpdate(b.updateSession(ctx, principal.UserKey, record.Session, func(s *ahpSession) {
			if s.ActiveTurn != action.TurnId {
				return
			}
			s.ActiveTurn, s.ActiveTask, s.ActiveStartedAt = "", "", ""
			s.Agent, s.Model, s.Turns, s.SessionTurns, s.History = record.Agent, record.Model, record.Turns, record.SessionTurns, record.History
		}), record.Session, "take back a turn whose Task was not created")
		b.mu.Lock()
		if b.turns[turnKey] == turn {
			delete(b.turns, turnKey)
		}
		b.mu.Unlock()
		unlock()
		sub.Close()
		cancel()
		if invalid := invalidObjectError("task", err); invalid != nil {
			fail(ahpErrorMessage(invalid))
		} else {
			fail("the turn's Task could not be created")
			log.Error(err, "create AHP turn Task", "namespace", task.Namespace, "task", task.Name)
		}
		return
	}
	unlock()
	sealRequesterStamp(ctx, b.h.client, task)

	now := ahpNow(b)
	_ = emit.Emit(ctx, record.Session, ahptypes.StateAction{Value: &ahptypes.SessionChatUpdatedAction{
		Type: ahptypes.ActionTypeSessionChatUpdated, Chat: record.Chat,
		Changes: ahptypes.PartialChatSummary{Status: ahpStatus(ahptypes.SessionStatusInProgress), ModifiedAt: &now},
	}})
	if number == 1 {
		_ = emit.Emit(ctx, record.Session, ahptypes.StateAction{Value: &ahptypes.SessionTitleChangedAction{
			Type: ahptypes.ActionTypeSessionTitleChanged, Title: ahpTitle(action.Message.Text),
		}})
	}
	if note != "" {
		turn.mu.Lock()
		turn.text(note)
		turn.textPart, turn.lastText, turn.reply, turn.streamedText = "", "", "", false
		turn.mu.Unlock()
	}
	go turn.run(sub)
}

// turnTask builds one turn's Task. The Session carries the conversation; the
// repository comes from the project, cloned by Orka with Orka's credentials.
func (b *ahpBackend) turnTask(ctx context.Context, principal ahphost.Principal, record ahpSession, choice ahpAgentChoice, name, prompt string, first bool) *corev1alpha1.Task {
	source := record.BaseBranch
	if record.SourceBranch != "" {
		source = record.SourceBranch
	}
	workspace := &corev1alpha1.WorkspaceConfig{GitRepo: record.RepoURL, Branch: source, Intent: corev1alpha1.WorkspaceIntentRead}
	// The namespace's credential goes to github.com only: a project on another
	// host, which the user may name freely, is cloned anonymously.
	if ahpOnGitHub(record.RepoURL) {
		if credential := b.gitCredential(ctx, principal.Namespace); credential != nil {
			workspace.Intent = corev1alpha1.WorkspaceIntentWrite
			workspace.ReadCredentialRef = credential
			publication := *credential
			workspace.PublicationCredentialRef = &publication
			workspace.PushBranch = record.Branch
		}
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: principal.Namespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, AgentRef: &corev1alpha1.AgentReference{Name: choice.Agent},
			Prompt:     prompt,
			SessionRef: &corev1alpha1.SessionReference{Name: record.OrkaSession, Create: first, Append: true},
			Workspace:  workspace,
			Timeout:    &metav1.Duration{Duration: b.cfg.TurnTimeout},
		},
	}
	if b.cfg.WorkspaceClass != "" {
		task.Spec.Execution = &corev1alpha1.ExecutionSpec{Workspace: &corev1alpha1.ExecutionWorkspaceSpec{
			ClassRef:    &corev1alpha1.WorkspaceClassReference{Name: b.cfg.WorkspaceClass},
			ReusePolicy: corev1alpha1.WorkspaceReusePolicySession,
		}}
	}
	b.applyApprovalMode(task, record.ApprovalMode != ahpApprovalAllowAll)
	return task
}

func ahpErrorMessage(err error) string {
	if rejection, ok := errors.AsType[*ahphost.Rejection](err); ok {
		return rejection.Reason
	}
	if fiberErr, ok := errors.AsType[*fiber.Error](err); ok {
		return fiberErr.Message
	}
	return "the turn could not start"
}

// emitTurnError ends a turn that never got a Task.
func (b *ahpBackend) emitTurnError(ctx context.Context, emit ahphost.Emitter, record ahpSession, turnID string, started time.Time, kind, message string) {
	_ = emit.Emit(ctx, record.Chat, ahptypes.StateAction{Value: &ahptypes.ChatErrorAction{
		Type: ahptypes.ActionTypeChatError, TurnId: turnID, Duration: b.now().Sub(started).Milliseconds(),
		Part: ahptypes.ErrorResponsePart{Kind: ahptypes.ResponsePartKindError, Error: ahptypes.ErrorInfo{ErrorType: kind, Message: message}},
	}})
	now := ahpNow(b)
	_ = emit.Emit(ctx, record.Session, ahpSessionIdle(record.Chat, now)...)
}

// ahpSessionIdle marks a session's chat idle after a turn and clears the
// session's activity, which clients read as the end of its work.
func ahpSessionIdle(chat, now string) []ahptypes.StateAction {
	return []ahptypes.StateAction{
		{Value: &ahptypes.SessionChatUpdatedAction{
			Type: ahptypes.ActionTypeSessionChatUpdated, Chat: chat,
			Changes: ahptypes.PartialChatSummary{Status: ahpStatus(ahptypes.SessionStatusIdle), ModifiedAt: &now},
		}},
		{Value: &ahptypes.SessionActivityChangedAction{Type: ahptypes.ActionTypeSessionActivityChanged}},
	}
}

func (t *ahpTurn) stop() { t.cancel() }

func (t *ahpTurn) markCancelledByClient() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.clientCancelled = true
}

func (t *ahpTurn) chat(actions ...ahptypes.StateAction) {
	_ = t.emit.Emit(t.ctx, t.session.Chat, actions...)
}

// run follows the turn's live events and its Task until the Task settles.
func (t *ahpTurn) run(sub *liveevents.Subscription) {
	defer sub.Close()
	defer t.b.forgetTurn(t)
	events := sub.C()
	ticker := time.NewTicker(ahpTurnPollInterval)
	defer ticker.Stop()
	for {
		select {
		case event, ok := <-events:
			if !ok {
				// The subscription fell behind: the turn ends from the Task's
				// stored result instead of live updates.
				events = nil
				continue
			}
			t.handle(event)
		case <-ticker.C:
			task := &corev1alpha1.Task{}
			err := t.b.h.client.Get(t.ctx, types.NamespacedName{Namespace: t.session.Namespace, Name: t.task}, task)
			if apierrors.IsNotFound(err) {
				t.settleWithError("TaskDeleted", "the turn's Task was deleted")
				return
			}
			if err == nil && ahpTaskTerminal(task) {
				// Deliver anything still queued for this turn first.
				t.drain(events)
				t.settle(task)
				return
			}
		case <-t.ctx.Done():
			return
		}
	}
}

func (t *ahpTurn) drain(events <-chan liveevents.Event) {
	for events != nil {
		select {
		case event, ok := <-events:
			if !ok {
				return
			}
			t.handle(event)
		default:
			return
		}
	}
}

func ahpTaskTerminal(task *corev1alpha1.Task) bool {
	switch task.Status.Phase {
	case corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskPhaseCancelled:
		return true
	default:
		return false
	}
}

func (t *ahpTurn) handle(event liveevents.Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if event.Approval != nil {
		t.onApproval(event.Approval)
		return
	}
	runtime := event.Runtime
	if runtime == nil {
		return
	}
	switch runtime.Type {
	case harnessv2.EventUpdate:
		update := runtime.Update
		if update == nil {
			return
		}
		switch {
		case update.AssistantMessage != nil:
			t.text(update.AssistantMessage.Text)
		case update.AssistantThought != nil:
			t.thought(update.AssistantThought.Text)
		case update.ToolCall != nil:
			t.tool(update.ToolCall)
		case update.Plan != nil:
			t.showPlan(update.Plan)
		case update.Usage != nil:
			t.usage(update.Usage)
		}
	case harnessv2.EventFailed:
		if runtime.Failed != nil {
			t.failure = ahpBounded(strings.TrimSpace(runtime.Failed.Message), ahpFailureTextMax)
		}
	case harnessv2.EventCompleted:
		if runtime.Completed == nil {
			return
		}
		if !t.streamedText {
			for _, block := range runtime.Completed.Result.Content {
				if block.Type == harnessv2.ContentBlockText {
					t.text(block.Text)
				}
			}
		}
		usage := runtime.Completed.Result.Usage
		if usage.InputTokens > 0 || usage.OutputTokens > 0 {
			info := ahptypes.UsageInfo{}
			input, output := int64(usage.InputTokens), int64(usage.OutputTokens) //nolint:gosec // token counts fit in int64
			info.InputTokens, info.OutputTokens = &input, &output
			if model := runtime.Completed.Result.Model; model != "" {
				info.Model = &model
			}
			t.chat(ahptypes.StateAction{Value: &ahptypes.ChatUsageAction{Type: ahptypes.ActionTypeChatUsage, TurnId: t.id, Usage: info}})
		}
	}
}

// text appends assistant text, opening a markdown part when needed.
func (t *ahpTurn) text(content string) {
	if content == "" {
		return
	}
	t.thoughtPart = ""
	if t.textPart == "" {
		t.textPart = ahpRandomHex(8)
		t.lastText = ""
		t.chat(ahptypes.StateAction{Value: &ahptypes.ChatResponsePartAction{
			Type: ahptypes.ActionTypeChatResponsePart, TurnId: t.id,
			Part: ahptypes.ResponsePart{Value: &ahptypes.MarkdownResponsePart{Kind: ahptypes.ResponsePartKindMarkdown, Id: t.textPart}},
		}})
	}
	t.lastText = ahpTail(t.lastText+content, ahpFailureTextMax)
	t.reply = ahpTail(t.reply+content, ahpExchangeMax)
	t.streamedText = true
	t.chat(ahptypes.StateAction{Value: &ahptypes.ChatDeltaAction{Type: ahptypes.ActionTypeChatDelta, TurnId: t.id, PartId: t.textPart, Content: content}})
}

func (t *ahpTurn) thought(content string) {
	if content == "" {
		return
	}
	t.textPart = ""
	if t.thoughtPart == "" {
		t.thoughtPart = ahpRandomHex(8)
		t.chat(ahptypes.StateAction{Value: &ahptypes.ChatResponsePartAction{
			Type: ahptypes.ActionTypeChatResponsePart, TurnId: t.id,
			Part: ahptypes.ResponsePart{Value: &ahptypes.ReasoningResponsePart{Kind: ahptypes.ResponsePartKindReasoning, Id: t.thoughtPart}},
		}})
	}
	t.chat(ahptypes.StateAction{Value: &ahptypes.ChatReasoningAction{Type: ahptypes.ActionTypeChatReasoning, TurnId: t.id, PartId: t.thoughtPart, Content: content}})
}

func ahpToolName(update *harnessv2.ToolCallUpdate) string {
	for _, name := range []string{update.Name, update.Kind} {
		if strings.TrimSpace(name) != "" {
			return name
		}
	}
	return ahpToolFallbackName
}

func (t *ahpTurn) startTool(id, name, title string) *ahpToolCall {
	call := t.tools[id]
	if call == nil {
		call = &ahpToolCall{}
		t.tools[id] = call
		t.order = append(t.order, id)
	}
	if call.started {
		return call
	}
	if title == "" {
		title = name
	}
	t.textPart, t.thoughtPart = "", ""
	call.started, call.title = true, title
	t.chat(ahptypes.StateAction{Value: &ahptypes.ChatToolCallStartAction{
		Type: ahptypes.ActionTypeChatToolCallStart, TurnId: t.id, ToolCallId: id, ToolName: name, DisplayName: title,
	}})
	return call
}

func (t *ahpTurn) readyTool(id string, call *ahpToolCall, title string, input json.RawMessage) {
	if call.ready {
		return
	}
	call.ready = true
	notNeeded := ahptypes.ToolCallConfirmationReasonNotNeeded
	ready := &ahptypes.ChatToolCallReadyAction{
		Type: ahptypes.ActionTypeChatToolCallReady, TurnId: t.id, ToolCallId: id,
		InvocationMessage: ahptypes.StringOrMarkdown{Plain: &title}, Confirmed: &notNeeded,
	}
	if len(input) > 0 {
		inline := string(input)
		ready.ToolInput = &ahptypes.ToolInput{Inline: &inline}
	}
	t.chat(ahptypes.StateAction{Value: ready})
}

// tool advances one tool call through start, ready and complete.
func (t *ahpTurn) tool(update *harnessv2.ToolCallUpdate) {
	call := t.startTool(update.ToolCallID, ahpToolName(update), ahpRelativeTitle(update.Title))
	// Later updates usually carry no title; keep the one the call started with.
	title := ahpRelativeTitle(update.Title)
	if title == "" {
		title = call.title
	}
	if len(update.Diffs) > 0 {
		t.recordDiffs(update.Diffs)
	}
	finished := update.Status == harnessv2.ToolCallStatusCompleted || update.Status == harnessv2.ToolCallStatusFailed
	if call.completed || (call.approval != "" && !finished) {
		// A held call moves on only after its decision.
		return
	}
	if call.approval != "" {
		// The call ended while its confirmation was open, for example when the
		// request expired: close the confirmation before the result.
		call.approval = ""
		reason := ahptypes.ToolCallConfirmationReasonUserAction
		t.chat(ahptypes.StateAction{Value: &ahptypes.ChatToolCallConfirmedAction{
			Type: ahptypes.ActionTypeChatToolCallConfirmed, TurnId: t.id, ToolCallId: update.ToolCallID,
			Approved: update.Status == harnessv2.ToolCallStatusCompleted, Confirmed: &reason,
		}})
	}
	switch update.Status {
	case harnessv2.ToolCallStatusInProgress:
		t.readyTool(update.ToolCallID, call, title, update.RawInput)
		t.toolOutput(update.ToolCallID, call, update)
	case harnessv2.ToolCallStatusCompleted, harnessv2.ToolCallStatusFailed:
		t.readyTool(update.ToolCallID, call, title, update.RawInput)
		content := update.Content
		if !ahpHasText(content) && call.output != "" {
			// Keep the output the call streamed when its result carries none.
			content = []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: call.output}}
		}
		t.completeTool(update.ToolCallID, call, update.Status == harnessv2.ToolCallStatusCompleted, title, content)
	}
}

// toolOutput shows a running call's output as it arrives.
func (t *ahpTurn) toolOutput(id string, call *ahpToolCall, update *harnessv2.ToolCallUpdate) {
	if !ahpHasText(update.Content) {
		return
	}
	var text strings.Builder
	for _, block := range update.Content {
		if block.Type == harnessv2.ContentBlockText {
			text.WriteString(block.Text)
		}
	}
	if update.ContentReplace {
		call.output = ""
	}
	call.output = ahpTail(call.output+text.String(), ahpToolOutputMax)
	t.chat(ahptypes.StateAction{Value: &ahptypes.ChatToolCallContentChangedAction{
		Type: ahptypes.ActionTypeChatToolCallContentChanged, TurnId: t.id, ToolCallId: id,
		Content: []ahptypes.ToolResultContent{{Value: &ahptypes.ToolResultTextContent{Type: ahptypes.ToolResultContentTypeText, Text: call.output}}},
	}})
}

// showPlan keeps the agent's current plan in one tool call of the turn and
// on the session, for clients that ask for it.
func (t *ahpTurn) showPlan(plan *harnessv2.PlanUpdate) {
	markdown := ahpPlanMarkdown(plan)
	if t.planCall == "" {
		t.planCall = "plan-" + ahpRandomHex(8)
		call := t.startTool(t.planCall, ahpPlanToolName, "Plan")
		t.readyTool(t.planCall, call, "Plan", nil)
	}
	t.chat(ahptypes.StateAction{Value: &ahptypes.ChatToolCallContentChangedAction{
		Type: ahptypes.ActionTypeChatToolCallContentChanged, TurnId: t.id, ToolCallId: t.planCall,
		Content: []ahptypes.ToolResultContent{{Value: &ahptypes.ToolResultTextContent{Type: ahptypes.ToolResultContentTypeText, Text: markdown}}},
	}})
	if call := t.tools[t.planCall]; call != nil {
		call.output = markdown
	}
	logSessionUpdate(t.b.updateSession(t.ctx, t.principal.UserKey, t.session.Session, func(s *ahpSession) {
		s.Plan = markdown
	}), t.session.Session, "record the plan")
}

// finishPlan completes the plan's tool call with the last plan.
func (t *ahpTurn) finishPlan() {
	if t.planCall == "" {
		return
	}
	if call := t.tools[t.planCall]; call != nil && !call.completed {
		t.completeTool(t.planCall, call, true, "Plan",
			[]harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: call.output}})
	}
}

// usage reports the prompt's token counts so far. Session-wide totals are
// left to the completed result.
func (t *ahpTurn) usage(update *harnessv2.UsageUpdate) {
	if update.Scope == ahpUsageScopeSession {
		return
	}
	if update.InputTokens == 0 && update.OutputTokens == 0 {
		return
	}
	input, output := int64(update.InputTokens), int64(update.OutputTokens) //nolint:gosec // token counts fit in int64
	info := ahptypes.UsageInfo{InputTokens: &input, OutputTokens: &output}
	if update.CachedInputTokens != nil {
		cached := int64(*update.CachedInputTokens) //nolint:gosec // token counts fit in int64
		info.CacheReadTokens = &cached
	}
	if t.session.Model != "" {
		model := t.session.Model
		info.Model = &model
	}
	t.chat(ahptypes.StateAction{Value: &ahptypes.ChatUsageAction{Type: ahptypes.ActionTypeChatUsage, TurnId: t.id, Usage: info}})
}

func ahpPlanMarkdown(plan *harnessv2.PlanUpdate) string {
	var md strings.Builder
	for _, entry := range plan.Entries {
		mark := " "
		switch entry.Status {
		case harnessv2.PlanEntryCompleted:
			mark = "x"
		case harnessv2.PlanEntryInProgress:
			mark = "~"
		}
		fmt.Fprintf(&md, "- [%s] %s\n", mark, strings.Join(strings.Fields(entry.Content), " "))
	}
	return md.String()
}

func ahpHasText(content []harnessv2.ContentBlock) bool {
	for _, block := range content {
		if block.Type == harnessv2.ContentBlockText && block.Text != "" {
			return true
		}
	}
	return false
}

// ahpTail keeps the last limit bytes of text without splitting a character.
func ahpTail(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := len(text) - limit
	for cut < len(text) && !utf8.RuneStart(text[cut]) {
		cut++
	}
	return text[cut:]
}

// ahpBounded keeps the first limit bytes of text without splitting a character.
func ahpBounded(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// ahpRuntimeWorkspacePath matches a path inside a runtime's own session
// workspace, absolute or shortened with a leading ellipsis. Clients see the
// session's files relative to its working directory instead.
var ahpRuntimeWorkspacePath = regexp.MustCompile(`(^|\s)(?:\.\.\.|…|/)\S*?/workspace/`)

func ahpRelativeTitle(title string) string {
	return ahpRuntimeWorkspacePath.ReplaceAllString(title, "$1")
}

func (t *ahpTurn) completeTool(id string, call *ahpToolCall, success bool, title string, content []harnessv2.ContentBlock) {
	if call.completed {
		return
	}
	call.completed = true
	result := ahptypes.ToolCallResult{Success: success, PastTenseMessage: ahptypes.StringOrMarkdown{Plain: &title}}
	for _, block := range content {
		if block.Type == harnessv2.ContentBlockText && block.Text != "" {
			result.Content = append(result.Content, ahptypes.ToolResultContent{Value: &ahptypes.ToolResultTextContent{
				Type: ahptypes.ToolResultContentTypeText, Text: block.Text,
			}})
		}
	}
	t.chat(ahptypes.StateAction{Value: &ahptypes.ChatToolCallCompleteAction{
		Type: ahptypes.ActionTypeChatToolCallComplete, TurnId: t.id, ToolCallId: id, Result: result,
	}})
}

// onApproval shows a held permission request as a confirmation, and settles
// the confirmation when the request ends without the client's answer.
func (t *ahpTurn) onApproval(approval *liveevents.Approval) {
	id := approval.ToolCallID
	if id == "" {
		id = "approval-" + approval.ID
	}
	switch approval.State {
	case liveevents.ApprovalPending:
		name := approval.ToolName
		if name == "" {
			name = ahpToolFallbackName
		}
		call := t.startTool(id, name, approval.Title)
		// A running call may ask again; each hold reopens its confirmation.
		if call.completed || call.approval == approval.ID {
			return
		}
		call.ready, call.approval = true, approval.ID
		title := approval.Title
		if title == "" {
			title = call.title
		}
		t.askForConfirmation(id, title)
	default:
		call := t.tools[id]
		if call == nil || call.approval == "" {
			return
		}
		call.approval = ""
		approved := approval.State == liveevents.ApprovalResolved && ahpOptionAllows(approval)
		reason := ahptypes.ToolCallConfirmationReasonUserAction
		t.chat(ahptypes.StateAction{Value: &ahptypes.ChatToolCallConfirmedAction{
			Type: ahptypes.ActionTypeChatToolCallConfirmed, TurnId: t.id, ToolCallId: id, Approved: approved, Confirmed: &reason,
		}})
	}
}

// askForConfirmation shows a tool call as waiting for the person's decision.
func (t *ahpTurn) askForConfirmation(id, title string) {
	t.chat(ahptypes.StateAction{Value: &ahptypes.ChatToolCallReadyAction{
		Type: ahptypes.ActionTypeChatToolCallReady, TurnId: t.id, ToolCallId: id,
		InvocationMessage: ahptypes.StringOrMarkdown{Plain: &title}, ConfirmationTitle: &ahptypes.StringOrMarkdown{Plain: &title},
		Options: []ahptypes.ConfirmationOption{
			{Id: ahpOptionApprove, Label: "Allow", Kind: ahptypes.ConfirmationOptionKindApprove},
			{Id: ahpOptionDeny, Label: "Deny", Kind: ahptypes.ConfirmationOptionKindDeny},
		},
	}})
}

func ahpOptionAllows(approval *liveevents.Approval) bool {
	for _, option := range approval.Options {
		if option.OptionID == approval.OptionID {
			return option.Kind == harnessv2.PermissionOptionAllowOnce || option.Kind == harnessv2.PermissionOptionAllowAlways
		}
	}
	return false
}

func (t *ahpTurn) awaitingConfirmation(toolCallID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	call := t.tools[toolCallID]
	return call != nil && call.approval != ""
}

// confirm answers a held permission request with the decision of the
// connection that confirmed it, as that connection's own identity. A refused
// approval reopens the confirmation. A refused denial needs nothing: the
// request expires and is rejected, so the tool does not run either way.
func (t *ahpTurn) confirm(ctx context.Context, principal ahphost.Principal, toolCallID string, approved bool) {
	t.mu.Lock()
	call := t.tools[toolCallID]
	approval := ""
	if call != nil {
		approval, call.approval = call.approval, ""
	}
	t.mu.Unlock()
	if approval == "" || t.b.decideApproval == nil {
		return
	}
	err := t.b.decideApproval(ctx, ahpPrincipalUserInfo(principal), t.session.Namespace, t.task, approval, approved)
	if err == nil {
		return
	}
	log.Info("an AHP tool decision was refused", "namespace", t.session.Namespace, "task", t.task, "reason", ahpErrorMessage(err))
	if !approved {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if call.completed || call.approval != "" {
		return
	}
	call.approval = approval
	t.askForConfirmation(toolCallID, call.title)
}

// recordDiffs keeps the reported file edits and lists them in the session's
// changeset as they happen. Git replaces them once the turn's changes land.
func (t *ahpTurn) recordDiffs(diffs []harnessv2.ToolCallDiff) {
	changes := t.changes()
	project := t.session.WorkDir
	if project == "" {
		project, _ = ahpURIPath(t.sessionProjectDir())
	}
	for _, diff := range diffs {
		after := diff.NewText
		changes.setFile(project+path.Clean("/"+diff.Path), diff.OldText, &after, true)
	}
	changes.emitCounts()
}

func (t *ahpTurn) changes() *ahpChanges {
	return &ahpChanges{b: t.b, ctx: t.ctx, principal: t.principal, session: t.session, emit: t.emit}
}

func (t *ahpTurn) sessionProjectDir() string {
	var project ahpProject
	if err := t.b.getRecord(t.ctx, t.principal.UserKey, ahpRecordProject, t.session.ProjectID, &project); err != nil {
		return ahpDirectoryURI(ahpProjectRoot)
	}
	return ahpDirectoryURI(project.Path)
}

// ahpLineChanges estimates added and removed lines by matching lines as a
// multiset; it is a summary count, not a diff.
func ahpLineChanges(before, after string) (int64, int64) {
	counts := map[string]int{}
	oldLines := ahpLines(before)
	for _, line := range oldLines {
		counts[line]++
	}
	common := 0
	newLines := ahpLines(after)
	for _, line := range newLines {
		if counts[line] > 0 {
			counts[line]--
			common++
		}
	}
	return int64(len(newLines) - common), int64(len(oldLines) - common)
}

func ahpLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

// settle ends the turn from its Task's terminal phase.
func (t *ahpTurn) settle(task *corev1alpha1.Task) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.streamedText && task.Status.ResultRef != nil && task.Status.ResultRef.Available && t.b.h.resultStore != nil {
		if data, err := t.b.h.resultStore.GetResult(t.ctx, task.Namespace, task.Name); err == nil {
			t.text(string(data))
		}
	}
	t.finishPlan()
	for _, id := range t.order {
		if call := t.tools[id]; call != nil && !call.completed {
			title := "interrupted"
			t.readyTool(id, call, title, nil)
			t.completeTool(id, call, false, title, nil)
		}
	}
	duration := t.b.now().Sub(t.startedAt).Milliseconds()
	published := task.Status.Phase == corev1alpha1.TaskPhaseSucceeded && ahpDeliveryNote(task) != ""
	reply := strings.TrimSpace(t.reply)
	logSessionUpdate(t.b.updateSession(t.ctx, t.principal.UserKey, t.session.Session, func(s *ahpSession) {
		if n := len(s.History); n > 0 && s.History[n-1].Reply == "" {
			s.History[n-1].Reply = reply
		}
		s.Published = s.Published || published
	}), t.session.Session, "record the reply")
	switch task.Status.Phase {
	case corev1alpha1.TaskPhaseSucceeded:
		if note := ahpDeliveryNote(task); note != "" {
			t.textPart = ""
			t.text(note)
			// The session's worktree on the host follows what the turn published.
			go t.b.workspaces.follow(context.WithoutCancel(t.ctx), t.principal, t.session)
		}
		t.chat(ahptypes.StateAction{Value: &ahptypes.ChatTurnCompleteAction{Type: ahptypes.ActionTypeChatTurnComplete, TurnId: t.id, Duration: duration}})
	case corev1alpha1.TaskPhaseCancelled:
		if !t.clientCancelled {
			t.chat(ahptypes.StateAction{Value: &ahptypes.ChatTurnCancelledAction{Type: ahptypes.ActionTypeChatTurnCancelled, TurnId: t.id, Duration: duration}})
		}
	default:
		reason, message := "TaskFailed", strings.TrimSpace(task.Status.Message)
		if task.Status.Execution != nil && task.Status.Execution.Reason != "" {
			reason = string(task.Status.Execution.Reason)
		}
		if task.Status.Delivery != nil && task.Status.Delivery.Message != "" {
			message = task.Status.Delivery.Message
		}
		message = t.failureMessage(message)
		t.chat(ahptypes.StateAction{Value: &ahptypes.ChatErrorAction{
			Type: ahptypes.ActionTypeChatError, TurnId: t.id, Duration: duration,
			Part: ahptypes.ErrorResponsePart{Kind: ahptypes.ResponsePartKindError, Error: ahptypes.ErrorInfo{ErrorType: reason, Message: message}},
		}})
	}
	t.finishSession()
}

// failureMessage adds the runtime's own reason to a failed turn's message.
// A runtime that reports a failure as reply text leaves the Task only the
// generic prompt failure, so that text is the reason the person needs.
func (t *ahpTurn) failureMessage(message string) string {
	if message == "" {
		message = "the turn failed"
	}
	detail := t.failure
	if detail == "" && message == ahpGenericPromptFailure {
		detail = strings.TrimSpace(t.lastText)
	}
	if detail == "" || strings.Contains(message, detail) {
		return message
	}
	return message + ": " + detail
}

func (t *ahpTurn) settleWithError(kind, message string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.chat(ahptypes.StateAction{Value: &ahptypes.ChatErrorAction{
		Type: ahptypes.ActionTypeChatError, TurnId: t.id, Duration: t.b.now().Sub(t.startedAt).Milliseconds(),
		Part: ahptypes.ErrorResponsePart{Kind: ahptypes.ResponsePartKindError, Error: ahptypes.ErrorInfo{ErrorType: kind, Message: message}},
	}})
	t.finishSession()
}

// finishSession marks the chat idle and the session free for the next turn.
func (t *ahpTurn) finishSession() {
	now := ahpNow(t.b)
	_ = t.emit.Emit(t.ctx, t.session.Session, ahpSessionIdle(t.session.Chat, now)...)
	logSessionUpdate(t.b.updateSession(t.ctx, t.principal.UserKey, t.session.Session, func(s *ahpSession) {
		if s.ActiveTurn == t.id {
			s.ActiveTurn, s.ActiveTask, s.ActiveStartedAt = "", "", ""
		}
	}), t.session.Session, "end the turn")
	go func() {
		// The turn's slot frees when its run loop returns.
		<-t.ctx.Done()
		t.b.runPending(context.WithoutCancel(t.ctx), t.principal.UserKey, t.session.Session, t.emit)
	}()
}

// ahpDeliveryNote tells the person where a write turn's changes went.
func ahpDeliveryNote(task *corev1alpha1.Task) string {
	delivery := task.Status.Delivery
	if delivery == nil || delivery.Branch == "" {
		return ""
	}
	switch delivery.State {
	case corev1alpha1.TaskDeliveryStateVerifiedExact, corev1alpha1.TaskDeliveryStateDeliveredSuperseded:
		return fmt.Sprintf("\n\nOrka pushed this turn's changes to the branch `%s`.", delivery.Branch)
	default:
		return ""
	}
}

func (b *ahpBackend) forgetTurn(turn *ahpTurn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := ahpTurnKey(turn.principal.UserKey, turn.session.Session)
	if b.turns[key] == turn {
		delete(b.turns, key)
	}
	turn.cancel()
}

// recoverTurns settles turns a previous leader left running, once per user,
// from their Tasks.
func (b *ahpBackend) recoverTurns(ctx context.Context, principal ahphost.Principal) {
	b.mu.Lock()
	if b.recovered[principal.UserKey] || b.emitterFor == nil {
		b.mu.Unlock()
		return
	}
	b.recovered[principal.UserKey] = true
	b.mu.Unlock()
	records, err := b.store.ListAHPRecords(ctx, principal.UserKey, ahpRecordSession)
	if err != nil {
		return
	}
	for _, raw := range records {
		var record ahpSession
		if json.Unmarshal(raw.Value, &record) != nil || record.ActiveTurn == "" || b.activeTurn(principal.UserKey, record.Session) != nil {
			continue
		}
		started, _ := time.Parse(time.RFC3339Nano, record.ActiveStartedAt)
		turnCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		turn := &ahpTurn{
			b: b, principal: principal, session: record, id: record.ActiveTurn,
			task: record.ActiveTask, startedAt: started, emit: b.emitterFor(principal), ctx: turnCtx, cancel: cancel,
			tools: map[string]*ahpToolCall{},
		}
		b.mu.Lock()
		b.turns[ahpTurnKey(principal.UserKey, record.Session)] = turn
		b.mu.Unlock()
		go turn.run(b.live.Subscribe(record.Namespace, record.ActiveTask))
	}
}

// nextGeneration moves a session to a new Orka Session for another model. A
// branch a turn already published belongs to the old Orka Session, so the
// new one continues on a new branch based on it, with its own worktree. It
// changes only the generation's fields of the session as stored now, so a
// change made meanwhile, such as the approval mode, stays.
func (b *ahpBackend) nextGeneration(ctx context.Context, principal ahphost.Principal, stale ahpSession, choice ahpAgentChoice, emit ahphost.Emitter) (ahpSession, string, error) {
	unlock := b.lockUser(principal.UserKey)
	defer unlock()
	var record ahpSession
	if err := b.getRecord(ctx, principal.UserKey, ahpRecordSession, stale.Session, &record); err != nil {
		return stale, "", err
	}
	var project ahpProject
	if err := b.getRecord(ctx, principal.UserKey, ahpRecordProject, record.ProjectID, &project); err != nil {
		return record, "", err
	}
	generation := max(record.Generation, 1) + 1
	note := fmt.Sprintf("Switched to %s. Orka continues in a new session with the conversation so far as context.", choice.Model)
	branch, workDir := record.Branch, record.WorkDir
	if record.Published {
		branch = fmt.Sprintf("%s-%d", record.Branch, generation)
		if !ahpValidBranch(branch) {
			return record, "", &ahphost.Rejection{Reason: "the session's branch cannot continue under another model"}
		}
		sessions, err := b.store.ListAHPRecords(ctx, principal.UserKey, ahpRecordSession)
		if err != nil {
			return record, "", err
		}
		for _, raw := range sessions {
			var other ahpSession
			if json.Unmarshal(raw.Value, &other) == nil && other.Session != record.Session && other.ProjectID == project.ID && other.Branch == branch {
				return record, "", &ahphost.Rejection{Reason: fmt.Sprintf("another session already uses the branch %s", branch)}
			}
		}
		if b.workspaces.enabled() {
			workDir = ahpWorktreePath(project, branch)
		}
		note += fmt.Sprintf(" Its changes continue on the branch `%s`, based on `%s`.", branch, record.Branch)
	}
	next := record
	err := b.updateSession(ctx, principal.UserKey, record.Session, func(s *ahpSession) {
		s.Generation = generation
		s.OrkaSession = fmt.Sprintf("ahp-%s-g%d", ahpHash(principal.UserKey, record.Session)[:24], generation)
		s.Agent, s.Model, s.SessionTurns = "", "", 0
		if record.Published {
			s.SourceBranch, s.Branch, s.Published, s.WorkDir = record.Branch, branch, false, workDir
		}
		next = *s
	})
	if err != nil {
		return record, "", fmt.Errorf("record the session's next generation: %w", err)
	}
	if next.WorkDir != record.WorkDir {
		if b.workspaces.enabled() {
			if pod, err := b.workspaces.ensure(ctx, principal, ahpWorkspaceStartWait); err == nil {
				setupCtx, cancel := context.WithTimeout(ctx, ahpWorkspaceSetupWait)
				_ = b.workspaces.ensureWorktree(setupCtx, principal, pod, project, next)
				cancel()
			}
			go b.workspaces.dropWorktree(context.WithoutCancel(ctx), principal, project, record.Branch)
		}
		_ = emit.Emit(ctx, record.Session, ahptypes.StateAction{Value: &ahptypes.SessionWorkingDirectoryReplacedAction{
			Type: ahptypes.ActionTypeSessionWorkingDirectoryReplaced, Directory: ahpDirectoryURI(record.WorkDir), Replacement: ahpDirectoryURI(next.WorkDir),
		}})
	}
	return next, note + "\n\n", nil
}

// ahpCarryOver gives a new Orka Session the conversation so far.
func ahpCarryOver(history []ahpExchange) string {
	var kept []string
	size := 0
	for _, exchange := range slices.Backward(history) {
		entry := "User: " + exchange.User
		if exchange.Reply != "" {
			entry += "\nAssistant: " + exchange.Reply
		}
		if size+len(entry) > ahpCarryOverMax {
			break
		}
		size += len(entry)
		kept = append([]string{entry}, kept...)
	}
	if len(kept) == 0 {
		return ""
	}
	return "The conversation so far, with another model, for context:\n\n" + strings.Join(kept, "\n\n") + "\n\n---\n\n"
}

// ahpKeepHistory keeps the newest exchanges.
func ahpKeepHistory(history []ahpExchange) []ahpExchange {
	if len(history) > ahpHistoryMax {
		history = append([]ahpExchange(nil), history[len(history)-ahpHistoryMax:]...)
	}
	return history
}

// runPending starts the next waiting message as a turn when none is running,
// with the authority of the connection that sent it. A message whose sender
// is gone, or whose token no longer holds, does not run: the person sees why.
func (b *ahpBackend) runPending(ctx context.Context, userKey, session string, emit ahphost.Emitter) {
	for {
		var record ahpSession
		if err := b.getRecord(ctx, userKey, ahpRecordSession, session, &record); err != nil ||
			record.ActiveTurn != "" || len(record.Pending) == 0 || b.activeTurn(userKey, session) != nil {
			return
		}
		if !b.reserveTurn(userKey, session) {
			return
		}
		next := record.Pending[0]
		record.Pending = record.Pending[1:]
		// Only this version of the message leaves the queue, and only its
		// own sender may run it; a replacement stays for the next round. A
		// message still stored in the queue must not start, or it would run
		// again after this turn.
		if err := b.updateSession(ctx, userKey, session, func(s *ahpSession) {
			s.Pending = slices.DeleteFunc(s.Pending, func(p ahpPending) bool { return p.ID == next.ID && p.Version == next.Version })
		}); err != nil {
			logSessionUpdate(err, session, "take a message off the queue")
			b.releaseTurn(userKey, session)
			return
		}
		key := ahpPendingKey(userKey, session, next.ID)
		b.mu.Lock()
		stored, found := b.pendingAuth[key]
		known := found && next.Version != "" && stored.version == next.Version
		if known {
			delete(b.pendingAuth, key)
		}
		b.mu.Unlock()
		sender := stored.principal
		action := &ahptypes.ChatTurnStartedAction{
			Type: ahptypes.ActionTypeChatTurnStarted, TurnId: "turn-" + ahpRandomHex(16),
			StartedAt: b.now().UTC().Format(time.RFC3339Nano), Message: next.Message,
		}
		if err := emit.Emit(ctx, record.Chat,
			ahptypes.StateAction{Value: &ahptypes.ChatPendingMessageRemovedAction{
				Type: ahptypes.ActionTypeChatPendingMessageRemoved, Kind: ahptypes.PendingMessageKind(next.Kind), Id: next.ID,
			}},
			ahptypes.StateAction{Value: action},
		); err != nil {
			b.releaseTurn(userKey, session)
			return
		}
		reason := ""
		switch {
		case !known:
			reason = "the connection that sent this message is gone; send it again"
		case b.recheckToken != nil && b.recheckToken(ctx, sender) != nil:
			reason = "the token that sent this message is no longer valid; send it again"
		}
		if reason == "" {
			b.startTurn(ctx, sender, record, action, emit)
			return
		}
		b.emitTurnError(ctx, emit, record, action.TurnId, b.now().UTC(), "TurnNotStarted", reason)
		b.releaseTurn(userKey, session)
	}
}

func ahpPendingKey(userKey, session, id string) string {
	return userKey + "\x00" + session + "\x00" + id
}

// ahpReorderPending puts queued messages in the client's order; steering
// messages stay first and messages the order omits keep their place after it.
func ahpReorderPending(pending []ahpPending, order []string) []ahpPending {
	position := map[string]int{}
	for i, id := range order {
		position[id] = i
	}
	reordered := slices.Clone(pending)
	slices.SortStableFunc(reordered, func(a, b ahpPending) int {
		aSteer, bSteer := a.Kind == string(ahptypes.PendingMessageKindSteering), b.Kind == string(ahptypes.PendingMessageKindSteering)
		if aSteer != bSteer {
			if aSteer {
				return -1
			}
			return 1
		}
		ai, aok := position[a.ID]
		bi, bok := position[b.ID]
		switch {
		case aok && bok:
			return ai - bi
		case aok:
			return -1
		case bok:
			return 1
		default:
			return 0
		}
	})
	return reordered
}
