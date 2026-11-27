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
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/ahphost"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/controller"
	executionevents "github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/liveevents"
	"github.com/orka-agents/orka/internal/store"
)

const (
	ahpTurnPollInterval     = 2 * time.Second
	ahpTitleRunes           = 60
	ahpOptionApprove        = "approve"
	ahpOptionDeny           = "deny"
	ahpFailureTextMax       = 500
	ahpToolOutputMax        = 32 << 10
	ahpPlanToolName         = "plan"
	ahpUsageScopeSession    = "session"
	ahpExchangeMax          = 4 << 10
	ahpPartCompareMax       = 1 << 20
	ahpHistoryMax           = 20
	ahpCarryOverMax         = 24 << 10
	ahpGenericPromptFailure = "prompt failed"
)

type ahpTurn struct {
	published     bool
	b             *ahpBackend
	principal     ahphost.Principal
	session       ahpSession
	id            string
	task          string
	startedAt     time.Time
	finalReply    string
	hasFinalReply bool
	taskUID       string
	emit          ahphost.Emitter
	ctx           context.Context
	cancel        context.CancelFunc

	mu              sync.Mutex
	clientCancelled bool
	tools           map[string]*ahpToolCall
	order           []string
	textPart        string
	thoughtPart     string
	streamedText    bool
	shownPart       string
	partText        string
	partOver        bool
	journalMissed   atomic.Bool
	lagged          bool
	lastText        string
	reply           string
	failure         string
	planCall        string
	heldTools       map[string]harnessv2.ToolCallUpdate
}

type ahpToolCall struct {
	started, ready, completed bool
	title                     string
	approval, ended           string
	output                    string
	input                     json.RawMessage
	denyOnly                  bool
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

func (b *ahpBackend) startTurn(ctx context.Context, principal ahphost.Principal, record ahpSession, action *ahptypes.ChatTurnStartedAction, emit ahphost.Emitter) {
	started := b.now().UTC()
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
	var current ahpSession
	if err := b.getRecord(ctx, principal.UserKey, ahpRecordSession, record.Session, &current); errors.Is(err, store.ErrNotFound) {
		return
	} else if err != nil {
		log.Error(err, "read AHP session", "session", record.Session)
		fail("the session could not be read")
		return
	}
	record = current
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
	first := record.SessionTurns == 0
	if !first && b.h.sessionStore != nil {
		if _, err := b.h.sessionStore.GetSession(ctx, record.Namespace, record.OrkaSession); errors.Is(err, store.ErrNotFound) {
			first = true
		}
	}
	prompt := action.Message.Text
	if first && len(record.History) > 0 {
		prompt = ahpCarryOver(record.History) + prompt
	}
	task := b.turnTask(ctx, principal, record, choice, name, prompt, first)
	ui := ahpPrincipalUserInfo(principal)

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
	unlock := b.lockUser(principal.UserKey)
	busy, modeChanged := false, false
	err = b.updateSession(ctx, principal.UserKey, record.Session, func(s *ahpSession) {
		if s.ActiveTurn != "" || b.turns[ahpTurnKey(principal.UserKey, record.Session)] != nil {
			busy = true
			return
		}
		if s.ApprovalMode != record.ApprovalMode {
			modeChanged = true
			return
		}
		s.Agent, s.Model, s.Turns = choice.Agent, choice.Model, number
		s.SessionTurns++
		s.ActiveTurn, s.ActiveTask, s.ActiveTaskUID, s.ActiveStartedAt = action.TurnId, name, "", started.Format(time.RFC3339Nano)
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
	if busy || modeChanged {
		unlock()
		sub.Close()
		cancel()
		if busy {
			fail("another turn is already running in this session")
		} else {
			fail("the session's approval mode changed while the message waited; send it again")
		}
		return
	}
	turnKey := ahpTurnKey(principal.UserKey, record.Session)
	b.mu.Lock()
	b.turns[turnKey] = turn
	b.mu.Unlock()
	if err := b.h.client.Create(ctx, task); err != nil {
		if b.cancelTask(context.WithoutCancel(ctx), principal, task.Namespace, name, "") == nil {
			b.takeBackUncreatedTurn(ctx, principal, record, action.TurnId, emit)
		} else {
			go b.takeBackOnceStopped(context.WithoutCancel(ctx), principal, record, action.TurnId, task.Namespace, name, emit)
		}
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
	turn.adoptCreatedTask(ctx, task)
	b.cancelIfClientCancelled(context.WithoutCancel(ctx), principal, turn, task)
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

func (b *ahpBackend) turnTask(ctx context.Context, principal ahphost.Principal, record ahpSession, choice ahpAgentChoice, name, prompt string, first bool) *corev1alpha1.Task {
	source := record.BaseBranch
	if record.SourceBranch != "" {
		source = record.SourceBranch
	}
	workspace := &corev1alpha1.WorkspaceConfig{GitRepo: record.RepoURL, Branch: source, Intent: corev1alpha1.WorkspaceIntentRead}
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
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: principal.Namespace, Labels: map[string]string{labels.LabelAHPTurn: labels.LabelAHPTurnValue}},
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

func (b *ahpBackend) emitTurnError(ctx context.Context, emit ahphost.Emitter, record ahpSession, turnID string, started time.Time, kind, message string) {
	_ = emit.Emit(ctx, record.Chat, ahptypes.StateAction{Value: &ahptypes.ChatErrorAction{
		Type: ahptypes.ActionTypeChatError, TurnId: turnID, Duration: b.now().Sub(started).Milliseconds(),
		Part: ahptypes.ErrorResponsePart{Kind: ahptypes.ResponsePartKindError, Error: ahptypes.ErrorInfo{ErrorType: kind, Message: message}},
	}})
	now := ahpNow(b)
	_ = emit.Emit(ctx, record.Session, ahpSessionIdle(record.Chat, now)...)
}

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

func (b *ahpBackend) cancelIfClientCancelled(ctx context.Context, principal ahphost.Principal, turn *ahpTurn, task *corev1alpha1.Task) {
	turn.mu.Lock()
	cancelled := turn.clientCancelled
	turn.mu.Unlock()
	if cancelled {
		b.keepCancelling(ctx, principal, turn.session.Session, task.Namespace, task.Name, string(task.UID))
	}
}

func (t *ahpTurn) markCancelledByClient() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.clientCancelled = true
}

func (t *ahpTurn) chat(actions ...ahptypes.StateAction) {
	if err := t.emit.Emit(t.ctx, t.session.Chat, actions...); err != nil {
		t.journalMissed.Store(true)
	}
}

func (t *ahpTurn) run(sub *liveevents.Subscription) {
	defer func() { sub.Close() }()
	defer t.b.forgetTurn(t)
	events := sub.C()
	ticker := time.NewTicker(ahpTurnPollInterval)
	defer ticker.Stop()
	if !t.knowTaskUID(ticker.C) {
		return
	}
	for {
		select {
		case event, ok := <-events:
			if !ok {
				t.mu.Lock()
				t.lagged = true
				t.mu.Unlock()
				sub.Close()
				sub = t.b.live.Subscribe(t.session.Namespace, t.task)
				events = sub.C()
				t.recoverApprovals()
				continue
			}
			t.handle(event)
		case <-ticker.C:
			task := &corev1alpha1.Task{}
			err := t.b.h.client.Get(t.ctx, types.NamespacedName{Namespace: t.session.Namespace, Name: t.task}, task)
			if apierrors.IsNotFound(err) || (err == nil && !t.ownsTask(task)) {
				t.settleWithError("TaskDeleted", "the turn's Task was deleted")
				return
			}
			if err == nil && ahpTaskTerminal(task) {
				t.drain(events)
				t.settle(task)
				return
			}
		case <-t.ctx.Done():
			return
		}
	}
}

func (t *ahpTurn) knowTaskUID(ticks <-chan time.Time) bool {
	for {
		t.mu.Lock()
		known := t.taskUID != ""
		t.mu.Unlock()
		if known {
			return true
		}
		task := &corev1alpha1.Task{}
		err := t.b.h.uncachedReader().Get(t.ctx, types.NamespacedName{Namespace: t.session.Namespace, Name: t.task}, task)
		uid := ""
		switch {
		case err == nil && task.UID != "":
			uid = string(task.UID)
		case apierrors.IsNotFound(err):
			uid = ahpTaskGoneUID
		}
		if uid != "" {
			t.mu.Lock()
			t.taskUID = uid
			t.mu.Unlock()
			t.recordTaskUID(t.ctx, uid)
			return true
		}
		select {
		case <-ticks:
		case <-t.ctx.Done():
			return false
		}
	}
}

func (t *ahpTurn) ownsTask(task *corev1alpha1.Task) bool {
	t.mu.Lock()
	if t.taskUID != "" {
		owns := string(task.UID) == t.taskUID
		t.mu.Unlock()
		return owns
	}
	t.taskUID = string(task.UID)
	t.mu.Unlock()
	t.recordTaskUID(t.ctx, string(task.UID))
	return true
}

const ahpTaskGoneUID = "\x00gone"

func (b *ahpBackend) takeBackOnceStopped(ctx context.Context, principal ahphost.Principal, record ahpSession, turnID, namespace, name string, emit ahphost.Emitter) {
	err := retry.OnError(b.cancelBackoff, func(error) bool { return true }, func() error {
		return b.cancelTask(ctx, principal, namespace, name, "")
	})
	if err != nil {
		log.Error(err, "stop an AHP turn Task whose creation reported a failure; its session stays busy", "session", record.Session, "task", name)
		return
	}
	b.takeBackUncreatedTurn(ctx, principal, record, turnID, emit)
}

func (b *ahpBackend) takeBackUncreatedTurn(ctx context.Context, principal ahphost.Principal, record ahpSession, turnID string, emit ahphost.Emitter) {
	takenBack := b.updateSessionUntilDone(ctx, principal.UserKey, record.Session, "take back a turn whose Task was not created", func(s *ahpSession) {
		if s.ActiveTurn != turnID {
			return
		}
		s.ActiveTurn, s.ActiveTask, s.ActiveTaskUID, s.ActiveStartedAt = "", "", "", ""
		s.Agent, s.Model, s.SessionTurns, s.History = record.Agent, record.Model, record.SessionTurns, record.History
	})
	select {
	case <-takenBack:
	default:
		go func() {
			<-takenBack
			b.runPending(context.WithoutCancel(ctx), principal.UserKey, record.Session, emit)
		}()
	}
}

func (t *ahpTurn) adoptCreatedTask(ctx context.Context, task *corev1alpha1.Task) {
	uid := string(task.UID)
	if uid == "" {
		return
	}
	t.mu.Lock()
	t.taskUID = uid
	t.mu.Unlock()
	t.recordTaskUID(ctx, uid)
}

func (t *ahpTurn) recordTaskUID(ctx context.Context, uid string) {
	if uid == "" {
		return
	}
	t.b.updateSessionUntilDone(ctx, t.principal.UserKey, t.session.Session, "record the turn's Task UID", func(s *ahpSession) {
		if s.ActiveTurn == t.id && s.ActiveTask == t.task && s.ActiveTaskUID == "" {
			s.ActiveTaskUID = uid
		}
	})
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
	if event.TaskUID != "" && t.taskUID != "" && event.TaskUID != t.taskUID {
		return
	}
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

func (t *ahpTurn) text(content string) {
	if content == "" {
		return
	}
	t.thoughtPart = ""
	if t.textPart == "" {
		t.textPart = ahpRandomHex(8)
		t.lastText, t.partText, t.partOver = "", "", false
		t.chat(ahptypes.StateAction{Value: &ahptypes.ChatResponsePartAction{
			Type: ahptypes.ActionTypeChatResponsePart, TurnId: t.id,
			Part: ahptypes.ResponsePart{Value: &ahptypes.MarkdownResponsePart{Kind: ahptypes.ResponsePartKindMarkdown, Id: t.textPart}},
		}})
	}
	t.lastText = ahpTail(t.lastText+content, ahpFailureTextMax)
	if !t.partOver && len(t.partText)+len(content) <= ahpPartCompareMax {
		t.partText += content
	} else {
		t.partText, t.partOver = "", true
	}
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

var ahpTypedToolFields = func() map[string][]string {
	fields := map[string][]string{}
	for _, group := range []struct{ names, fields string }{
		{"read read_file view show_file", "file_path path"},
		{"search grep glob rg", "pattern query"},
		{"edit create write write_file str_replace_editor", "file_path path"},
	} {
		for name := range strings.FieldsSeq(group.names) {
			fields[name] = strings.Fields(group.fields)
		}
	}
	return fields
}()

func ahpToolName(update *harnessv2.ToolCallUpdate, input json.RawMessage) string {
	name := update.Name
	if strings.TrimSpace(name) == "" {
		name = update.Kind
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return ahpToolFallbackName
	}
	key := strings.ToLower(name)
	fields, typed := ahpTypedToolFields[key]
	if !typed || ahpInputHasAny(input, fields) {
		return name
	}
	return key + "_call"
}

func ahpInputHasAny(input json.RawMessage, fields []string) bool {
	var values map[string]any
	if len(input) == 0 || json.Unmarshal(input, &values) != nil {
		return false
	}
	for _, field := range fields {
		if value, ok := values[field].(string); ok && strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
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

func (t *ahpTurn) tool(update *harnessv2.ToolCallUpdate) {
	if len(update.Diffs) > 0 {
		t.recordDiffs(update.Diffs)
	}
	held := t.heldTools[update.ToolCallID]
	if update.Name != "" {
		held.Name = update.Name
	}
	if update.Kind != "" {
		held.Kind = update.Kind
	}
	if len(update.RawInput) > 0 {
		held.RawInput = ahpRelativeInput(update.RawInput)
	}
	input := held.RawInput
	title := ahpRelativeTitle(update.Title)
	if existing := t.tools[update.ToolCallID]; (existing == nil || !existing.started) && title == "" &&
		update.Status == harnessv2.ToolCallStatusPending {
		if t.heldTools == nil {
			t.heldTools = map[string]harnessv2.ToolCallUpdate{}
		}
		t.heldTools[update.ToolCallID] = held
		return
	}
	call := t.startTool(update.ToolCallID, ahpToolName(&held, input), title)
	delete(t.heldTools, update.ToolCallID)
	if len(input) > 0 {
		call.input = input
	} else {
		input = call.input
	}
	if title == "" {
		title = call.title
	}
	finished := update.Status == harnessv2.ToolCallStatusCompleted || update.Status == harnessv2.ToolCallStatusFailed
	if call.completed || (call.approval != "" && !finished) {
		return
	}
	if call.approval != "" {
		call.approval = ""
		reason := ahptypes.ToolCallConfirmationReasonUserAction
		t.chat(ahptypes.StateAction{Value: &ahptypes.ChatToolCallConfirmedAction{
			Type: ahptypes.ActionTypeChatToolCallConfirmed, TurnId: t.id, ToolCallId: update.ToolCallID,
			Approved: update.Status == harnessv2.ToolCallStatusCompleted, Confirmed: &reason,
		}})
	}
	switch update.Status {
	case harnessv2.ToolCallStatusInProgress:
		t.readyTool(update.ToolCallID, call, title, input)
		t.toolOutput(update.ToolCallID, call, update)
	case harnessv2.ToolCallStatusCompleted, harnessv2.ToolCallStatusFailed:
		t.readyTool(update.ToolCallID, call, title, input)
		content := update.Content
		if !ahpHasText(content) && call.output != "" {
			content = []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: call.output}}
		}
		t.completeTool(update.ToolCallID, call, update.Status == harnessv2.ToolCallStatusCompleted, title, content)
	}
}

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

func (t *ahpTurn) finishPlan() {
	if t.planCall == "" {
		return
	}
	if call := t.tools[t.planCall]; call != nil && !call.completed {
		t.completeTool(t.planCall, call, true, "Plan",
			[]harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: call.output}})
	}
}

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

var ahpRuntimeWorkspacePath = regexp.MustCompile(`(^|\s)(?:\.\.\.|…|/)\S*?/workspace/`)

func ahpRelativeTitle(title string) string {
	return ahpRuntimeWorkspacePath.ReplaceAllString(title, "$1")
}

func ahpRelativeInput(input json.RawMessage) json.RawMessage {
	var values map[string]json.RawMessage
	if json.Unmarshal(input, &values) != nil {
		return input
	}
	changed := false
	for _, field := range []string{"file_path", "path"} {
		var value string
		if json.Unmarshal(values[field], &value) != nil {
			continue
		}
		if relative := ahpRelativeTitle(value); relative != value && relative != "" {
			values[field], _ = json.Marshal(relative)
			changed = true
		}
	}
	if !changed {
		return input
	}
	out, err := json.Marshal(values)
	if err != nil {
		return input
	}
	return out
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

func (t *ahpTurn) onApproval(approval *liveevents.Approval) {
	t.onApprovalFrom(approval, false)
}

func (t *ahpTurn) onApprovalFrom(approval *liveevents.Approval, recovered bool) {
	id := approval.ToolCallID
	if id == "" {
		id = "approval-" + approval.ID
	}
	switch approval.State {
	case liveevents.ApprovalPending:
		held, wasHeld := t.heldTools[id]
		delete(t.heldTools, id)
		name := approval.ToolName
		if name == "" && wasHeld {
			name = ahpToolName(&held, held.RawInput)
		}
		if name == "" {
			name = ahpToolFallbackName
		}
		call := t.startTool(id, name, approval.Title)
		if len(call.input) == 0 && len(held.RawInput) > 0 {
			call.input = held.RawInput
		}
		if call.completed || call.approval == approval.ID {
			return
		}
		call.ready, call.approval = true, approval.ID
		if len(approval.Input) > 0 {
			call.input = approval.Input
		}
		call.denyOnly = recovered && len(call.input) == 0
		title := approval.Title
		if title == "" {
			title = call.title
		}
		t.askForConfirmation(id, title, call.input, call.denyOnly)
	default:
		call := t.tools[id]
		if call == nil {
			return
		}
		call.ended = approval.ID
		if call.approval != approval.ID {
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

func (t *ahpTurn) recoverApprovals() {
	eventStore := t.b.h.executionEventStore
	if eventStore == nil {
		return
	}
	listed, err := approvals.ListEvents(t.ctx, eventStore, t.session.Namespace, t.task)
	if err != nil {
		log.Info("read an AHP turn's held permission requests", "namespace", t.session.Namespace, "task", t.task, "reason", err.Error())
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	listed = approvals.FilterEventsForTaskUID(listed, t.taskUID)
	for _, held := range approvals.Derive(listed, t.b.now()) {
		if held.Kind != approvals.KindPermission || held.Status != approvals.StatusPending {
			continue
		}
		approval := &liveevents.Approval{
			ID: held.ID, State: liveevents.ApprovalPending, ToolCallID: held.ToolCallID, ToolName: held.TargetTool, Title: held.RiskSummary,
		}
		if held.ExpiresAt != nil {
			approval.ExpiresAt = *held.ExpiresAt
		}
		t.onApprovalFrom(approval, true)
	}
}

func (t *ahpTurn) askForConfirmation(id, title string, input json.RawMessage, denyOnly bool) {
	options := []ahptypes.ConfirmationOption{
		{Id: ahpOptionApprove, Label: "Allow", Kind: ahptypes.ConfirmationOptionKindApprove},
		{Id: ahpOptionDeny, Label: "Deny", Kind: ahptypes.ConfirmationOptionKindDeny},
	}
	confirmation := title
	if denyOnly {
		options = options[1:]
		confirmation = strings.TrimSpace(title + " " + ahpDenyOnlyNote)
	}
	ready := &ahptypes.ChatToolCallReadyAction{
		Type: ahptypes.ActionTypeChatToolCallReady, TurnId: t.id, ToolCallId: id,
		InvocationMessage: ahptypes.StringOrMarkdown{Plain: &title}, ConfirmationTitle: &ahptypes.StringOrMarkdown{Plain: &confirmation},
		Options: options,
	}
	if len(input) > 0 {
		inline := string(input)
		ready.ToolInput = &ahptypes.ToolInput{Inline: &inline}
	}
	t.chat(ahptypes.StateAction{Value: ready})
}

const ahpDenyOnlyNote = "(Orka cannot show this call's input after missing its live update, so it can only be denied; the agent can ask again.)"

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

func (t *ahpTurn) confirm(ctx context.Context, principal ahphost.Principal, toolCallID string, approved bool) {
	t.mu.Lock()
	call := t.tools[toolCallID]
	approval := ""
	if call != nil {
		if approved && call.denyOnly && call.approval != "" {
			log.Info("an AHP approval of a request offered only for denial was refused", "namespace", t.session.Namespace, "task", t.task)
			t.askForConfirmation(toolCallID, call.title, call.input, true)
			t.mu.Unlock()
			return
		}
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
	if call.completed || call.approval != "" || call.ended == approval {
		return
	}
	call.approval = approval
	t.askForConfirmation(toolCallID, call.title, call.input, call.denyOnly)
}

func (t *ahpTurn) recordDiffs(diffs []harnessv2.ToolCallDiff) {
	changes := t.changes()
	project := t.session.WorkDir
	if project == "" {
		project, _ = ahpURIPath(t.sessionProjectDir())
	}
	changes.change(func() {
		for _, diff := range diffs {
			after := diff.NewText
			changes.setFile(project+path.Clean("/"+diff.Path), diff.OldText, &after, true)
		}
	})
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

func (t *ahpTurn) showStoredAnswer(task *corev1alpha1.Task) {
	missed := t.journalMissed.Load()
	if (t.streamedText && !t.lagged && !missed) || task.Status.ResultRef == nil || !task.Status.ResultRef.Available || t.b.h.resultStore == nil {
		return
	}
	data, err := t.readResult(task)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		t.text(fmt.Sprintf("\n\nOrka could not read this turn's full answer; it is the result of Task %s.", task.Name))
	}
	if err == nil {
		answer := executionevents.RedactExecutionEventText(string(data))
		newest, known := t.shownPart, !missed
		if t.streamedText {
			newest, known = t.partText, !t.partOver && !missed
		}
		if whole := strings.TrimSpace(answer); whole != "" && known && strings.TrimSpace(newest) == whole {
			t.reply = ahpTail(whole, ahpExchangeMax)
		} else {
			if t.lagged || missed {
				t.textPart, t.reply = "", ""
			}
			t.text(answer)
		}
	}
}

func (t *ahpTurn) settle(task *corev1alpha1.Task) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.showStoredAnswer(task)
	t.finishPlan()
	t.heldTools = nil
	for _, id := range t.order {
		if call := t.tools[id]; call != nil && !call.completed {
			title := "interrupted"
			t.readyTool(id, call, title, nil)
			t.completeTool(id, call, false, title, nil)
		}
	}
	duration := t.b.now().Sub(t.startedAt).Milliseconds()
	published := task.Status.Phase == corev1alpha1.TaskPhaseSucceeded && ahpDeliveryNote(task) != ""
	t.published = published
	reply := strings.TrimSpace(t.reply)
	logSessionUpdate(t.b.updateSession(t.ctx, t.principal.UserKey, t.session.Session, func(s *ahpSession) {
		if n := len(s.History); n > 0 && s.History[n-1].Reply == "" {
			s.History[n-1].Reply = reply
		}
		s.Published = s.Published || published
	}), t.session.Session, "record the reply")
	t.finalReply, t.hasFinalReply = reply, true
	switch task.Status.Phase {
	case corev1alpha1.TaskPhaseSucceeded:
		if note := ahpDeliveryNote(task); note != "" {
			t.textPart = ""
			t.text(note)
			go t.b.workspaces.follow(context.WithoutCancel(t.ctx), t.principal, t.session)
		}
		t.chat(ahptypes.StateAction{Value: &ahptypes.ChatTurnCompleteAction{Type: ahptypes.ActionTypeChatTurnComplete, TurnId: t.id, Duration: duration}})
	case corev1alpha1.TaskPhaseCancelled:
		if t.clientCancelled {
			break
		}
		if task.Status.Execution != nil && task.Status.Execution.Reason == controller.ACPTaskTimeoutReason {
			message := "the turn ran past its time limit and was stopped"
			if task.Spec.Timeout != nil && task.Spec.Timeout.Duration > 0 {
				message = fmt.Sprintf("the turn ran past its %s time limit and was stopped", task.Spec.Timeout.Duration)
			}
			t.chat(ahptypes.StateAction{Value: &ahptypes.ChatErrorAction{
				Type: ahptypes.ActionTypeChatError, TurnId: t.id, Duration: duration,
				Part: ahptypes.ErrorResponsePart{Kind: ahptypes.ResponsePartKindError, Error: ahptypes.ErrorInfo{ErrorType: controller.ACPTaskTimeoutReason, Message: message}},
			}})
			break
		}
		t.chat(ahptypes.StateAction{Value: &ahptypes.ChatTurnCancelledAction{Type: ahptypes.ActionTypeChatTurnCancelled, TurnId: t.id, Duration: duration}})
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

var ahpResultReadDelays = []time.Duration{250 * time.Millisecond, 750 * time.Millisecond}

func (t *ahpTurn) readResult(task *corev1alpha1.Task) ([]byte, error) {
	data, err := t.b.h.resultStore.GetResult(t.ctx, task.Namespace, task.Name)
	for _, delay := range ahpResultReadDelays {
		if err == nil || errors.Is(err, store.ErrNotFound) {
			break
		}
		select {
		case <-t.ctx.Done():
			return nil, t.ctx.Err()
		case <-time.After(delay):
		}
		data, err = t.b.h.resultStore.GetResult(t.ctx, task.Namespace, task.Name)
	}
	if err != nil {
		log.Info("read an AHP turn's result", "namespace", task.Namespace, "task", task.Name, "reason", err.Error())
		return data, err
	}
	if !t.nameStillOwned(task.Namespace, task.Name) {
		return nil, store.ErrNotFound
	}
	return data, nil
}

func (t *ahpTurn) nameStillOwned(namespace, name string) bool {
	uid := t.taskUID
	if uid == "" {
		return true
	}
	current := &corev1alpha1.Task{}
	err := t.b.h.uncachedReader().Get(t.ctx, types.NamespacedName{Namespace: namespace, Name: name}, current)
	if apierrors.IsNotFound(err) {
		return true
	}
	if err != nil {
		log.Info("check an AHP turn's Task before showing its result", "namespace", namespace, "task", name, "reason", err.Error())
		return false
	}
	return string(current.UID) == uid
}

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

func (t *ahpTurn) finishSession() {
	now := ahpNow(t.b)
	_ = t.emit.Emit(t.ctx, t.session.Session, ahpSessionIdle(t.session.Chat, now)...)
	published, reply, hasReply := t.published, t.finalReply, t.hasFinalReply
	ended := t.b.updateSessionUntilDone(t.ctx, t.principal.UserKey, t.session.Session, "end the turn", func(s *ahpSession) {
		if s.ActiveTurn != t.id {
			return
		}
		s.ActiveTurn, s.ActiveTask, s.ActiveTaskUID, s.ActiveStartedAt = "", "", "", ""
		s.Published = s.Published || published
		if n := len(s.History); hasReply && n > 0 && s.History[n-1].Reply == "" {
			s.History[n-1].Reply = reply
		}
	})
	go func() {
		<-ended
		<-t.ctx.Done()
		t.b.runPending(context.WithoutCancel(t.ctx), t.principal.UserKey, t.session.Session, t.emit)
	}()
}

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

func (b *ahpBackend) recoverTurns(ctx context.Context, principal ahphost.Principal) {
	unlock := b.lockUser(principal.UserKey)
	defer unlock()
	b.mu.Lock()
	if b.recovered[principal.UserKey] || b.emitterFor == nil {
		b.mu.Unlock()
		return
	}
	b.recovered[principal.UserKey] = true
	b.mu.Unlock()
	records, err := b.store.ListAHPRecords(ctx, principal.UserKey, ahpRecordSession)
	if err != nil {
		b.mu.Lock()
		delete(b.recovered, principal.UserKey)
		b.mu.Unlock()
		return
	}
	for _, raw := range records {
		var record ahpSession
		if json.Unmarshal(raw.Value, &record) != nil || b.activeTurn(principal.UserKey, record.Session) != nil {
			continue
		}
		if record.ActiveTurn == "" {
			go func() {
				ctx := context.WithoutCancel(ctx)
				if !b.endUnstartedTurn(ctx, principal, record) {
					b.mu.Lock()
					delete(b.recovered, principal.UserKey)
					b.mu.Unlock()
				}
				if len(record.Pending) > 0 {
					b.runPending(ctx, principal.UserKey, record.Session, b.emitterFor(principal))
				}
			}()
			continue
		}
		started, _ := time.Parse(time.RFC3339Nano, record.ActiveStartedAt)
		turnCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		turn := &ahpTurn{
			b: b, principal: principal, session: record, id: record.ActiveTurn,
			task: record.ActiveTask, taskUID: record.ActiveTaskUID, startedAt: started, emit: b.emitterFor(principal), ctx: turnCtx, cancel: cancel,
			tools: map[string]*ahpToolCall{},
		}
		if turn.taskUID == "" {
			task := &corev1alpha1.Task{}
			err := b.h.uncachedReader().Get(ctx, types.NamespacedName{Namespace: record.Namespace, Name: record.ActiveTask}, task)
			switch {
			case err == nil && task.UID != "":
				turn.taskUID = string(task.UID)
				turn.recordTaskUID(ctx, turn.taskUID)
			case apierrors.IsNotFound(err):
				turn.taskUID = ahpTaskGoneUID
				turn.recordTaskUID(ctx, turn.taskUID)
			}
		}
		if state, err := ahphost.ChannelState(ctx, b.store, principal.UserKey, record.Chat); err == nil &&
			state.Chat != nil && state.Chat.ActiveTurn != nil && state.Chat.ActiveTurn.Id == record.ActiveTurn {
			turn.shownPart = ahpLastMarkdown(state.Chat.ActiveTurn.ResponseParts)
		}
		b.mu.Lock()
		b.turns[ahpTurnKey(principal.UserKey, record.Session)] = turn
		b.mu.Unlock()
		go turn.run(b.live.Subscribe(record.Namespace, record.ActiveTask))
	}
}

func ahpLastMarkdown(parts []ahptypes.ResponsePart) string {
	for _, part := range slices.Backward(parts) {
		if markdown, ok := part.Value.(*ahptypes.MarkdownResponsePart); ok {
			return markdown.Content
		}
	}
	return ""
}

func (b *ahpBackend) endUnstartedTurn(ctx context.Context, principal ahphost.Principal, record ahpSession) bool {
	key := ahpTurnKey(principal.UserKey, record.Session)
	b.mu.Lock()
	_, reserved := b.reserved[key]
	b.mu.Unlock()
	if reserved || b.activeTurn(principal.UserKey, record.Session) != nil {
		return true
	}
	state, err := ahphost.ChannelState(ctx, b.store, principal.UserKey, record.Chat)
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if errors.Is(err, store.ErrNotFound) || errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
		return true
	}
	if err != nil {
		log.Info("read an AHP chat to end a turn that did not start", "chat", record.Chat, "reason", err.Error())
		return false
	}
	if state.Chat == nil || state.Chat.ActiveTurn == nil {
		return true
	}
	turn := state.Chat.ActiveTurn
	started, err := time.Parse(time.RFC3339Nano, turn.StartedAt)
	if err != nil {
		started = b.now()
	}
	b.emitTurnError(ctx, b.emitterFor(principal), record, turn.Id, started, "TurnNotStarted",
		"the turn did not start before Orka restarted; send it again")
	return true
}

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
		s.OrkaSession = ahpOrkaSessionName(principal.UserKey, *s, generation)
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
			b.dropOldWorktree(ctx, principal, project, record.Branch)
		}
		_ = emit.Emit(ctx, record.Session, ahptypes.StateAction{Value: &ahptypes.SessionWorkingDirectoryReplacedAction{
			Type: ahptypes.ActionTypeSessionWorkingDirectoryReplaced, Directory: ahpDirectoryURI(record.WorkDir), Replacement: ahpDirectoryURI(next.WorkDir),
		}})
	}
	return next, note + "\n\n", nil
}

func (b *ahpBackend) dropOldWorktree(ctx context.Context, principal ahphost.Principal, project ahpProject, branch string) bool {
	if b.hasTerminals(principal.UserKey) {
		return false
	}
	go b.workspaces.dropWorktree(context.WithoutCancel(ctx), principal, project, branch)
	return true
}

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

func ahpKeepHistory(history []ahpExchange) []ahpExchange {
	if len(history) > ahpHistoryMax {
		history = append([]ahpExchange(nil), history[len(history)-ahpHistoryMax:]...)
	}
	return history
}

func (b *ahpBackend) runPending(ctx context.Context, userKey, session string, emit ahphost.Emitter) {
	if !b.drainPending(ctx, userKey, session, emit) {
		return
	}
	go func() {
		ctx := context.WithoutCancel(ctx)
		for delay := b.retryWait(); ; delay = min(2*delay, 32*b.retryWait()) {
			time.Sleep(delay)
			if !b.drainPending(ctx, userKey, session, emit) {
				return
			}
		}
	}()
}

func (b *ahpBackend) drainPending(ctx context.Context, userKey, session string, emit ahphost.Emitter) bool {
	for {
		var record ahpSession
		if err := b.getRecord(ctx, userKey, ahpRecordSession, session, &record); err != nil ||
			record.ActiveTurn != "" || len(record.Pending) == 0 || b.activeTurn(userKey, session) != nil {
			return false
		}
		if !b.reserveTurn(userKey, session) {
			return false
		}
		next := record.Pending[0]
		record.Pending = record.Pending[1:]
		if err := b.updateSession(ctx, userKey, session, func(s *ahpSession) {
			s.Pending = slices.DeleteFunc(s.Pending, func(p ahpPending) bool { return p.ID == next.ID && p.Version == next.Version })
		}); err != nil {
			logSessionUpdate(err, session, "take a message off the queue")
			b.releaseTurn(userKey, session)
			return !errors.Is(err, store.ErrNotFound)
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
			b.mu.Lock()
			if known {
				b.pendingAuth[key] = stored
			}
			b.mu.Unlock()
			restored := b.updateSessionUntilDone(ctx, userKey, session, "put a message back on the queue", func(s *ahpSession) {
				if !slices.ContainsFunc(s.Pending, func(p ahpPending) bool { return p.ID == next.ID }) {
					s.Pending = slices.Insert(s.Pending, 0, next)
				}
			})
			b.releaseTurn(userKey, session)
			select {
			case <-restored:
				return true
			default:
				go func() {
					<-restored
					b.runPending(context.WithoutCancel(ctx), userKey, session, emit)
				}()
				return false
			}
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
			return false
		}
		b.emitTurnError(ctx, emit, record, action.TurnId, b.now().UTC(), "TurnNotStarted", reason)
		b.releaseTurn(userKey, session)
	}
}

func ahpPendingKey(userKey, session, id string) string {
	return userKey + "\x00" + session + "\x00" + id
}

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
