package eventjournal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	executionevents "github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
)

const (
	mappedUpdateIdentityKeySeparator = "\x00"
	mappedToolCallIDPrefix           = "event-tool-call-v1-sha256-"
	mappedToolCallIDDomain           = "orka.harness.v2.execution-event.tool-call-id.v1\x00"
	mappedJournalDedupeKeyPrefix     = "harness-v2-event-v1-sha256-"
	mappedJournalDedupeKeyDomain     = "orka.harness.v2.execution-event.dedupe-key.v1\x00"
	mappedAssistantTranscriptKind    = "assistant_transcript"
	mappedToolStreamClosureKind      = "tool_stream_closure"
	mappedTerminalUsageKind          = "terminal_usage"
	mappedPromptAcceptedKind         = "prompt_accepted"
	mappedPromptTerminalKind         = "prompt_terminal"
)

type mappedJournalRecordKind uint8

const (
	mappedJournalRecordUpdate mappedJournalRecordKind = iota
	mappedJournalRecordAssistantTranscript
	mappedJournalRecordToolStreamClosure
	mappedJournalRecordTerminalUsage
	mappedJournalRecordPromptAccepted
	mappedJournalRecordPromptTerminal
)

// MapContext supplies Orka-owned stream metadata for a validated harness v2
// update. Protocol events do not own namespace, task name, or session linkage.
type MapContext struct {
	Namespace   string
	TaskName    string
	SessionName string
	AgentName   string
	StreamID    string
	Provider    string
	Model       string
}

func (c MapContext) normalized() MapContext {
	c.Namespace = strings.TrimSpace(c.Namespace)
	c.TaskName = strings.TrimSpace(c.TaskName)
	c.SessionName = strings.TrimSpace(c.SessionName)
	c.AgentName = strings.TrimSpace(c.AgentName)
	c.StreamID = strings.TrimSpace(c.StreamID)
	c.Provider = strings.TrimSpace(c.Provider)
	c.Model = strings.TrimSpace(c.Model)
	if c.StreamID == "" {
		c.StreamID = c.TaskName
	}
	return c
}

func (c MapContext) validate() error {
	c = c.normalized()
	if c.Namespace == "" {
		return fmt.Errorf("namespace is required")
	}
	if c.TaskName == "" {
		return fmt.Errorf("task name is required")
	}
	if c.StreamID == "" {
		return fmt.Errorf("stream id is required")
	}
	return nil
}

// MappedUpdateIdentity is the safe protocol identity persisted with each
// execution event. RequestDigest is deliberately excluded: the validated
// stream already enforces it, and journaling must not change or expose prompt
// digest/fencing data.
type MappedUpdateIdentity struct {
	Protocol                 string                      `json:"protocol"`
	RuntimeInstanceID        harnessv2.RuntimeInstanceID `json:"runtimeInstanceID"`
	SupervisorBootID         harnessv2.SupervisorBootID  `json:"supervisorBootID"`
	RuntimeSessionUID        harnessv2.RuntimeSessionUID `json:"runtimeSessionUID"`
	RuntimeSessionGeneration uint64                      `json:"runtimeSessionGeneration"`
	TaskUID                  harnessv2.TaskUID           `json:"taskUID"`
	TaskAttempt              uint32                      `json:"taskAttempt"`
	PromptID                 harnessv2.PromptID          `json:"promptID"`
	Sequence                 uint64                      `json:"sequence"`
}

func mappedUpdateIdentity(event harnessv2.Event) MappedUpdateIdentity {
	return MappedUpdateIdentity{
		Protocol:                 event.Protocol,
		RuntimeInstanceID:        event.Identity.RuntimeInstanceID,
		SupervisorBootID:         event.Identity.SupervisorBootID,
		RuntimeSessionUID:        event.Identity.RuntimeSessionUID,
		RuntimeSessionGeneration: event.Identity.RuntimeSessionGeneration,
		TaskUID:                  event.Identity.TaskUID,
		TaskAttempt:              event.Identity.TaskAttempt,
		PromptID:                 event.Identity.PromptID,
		Sequence:                 event.Identity.Sequence,
	}
}

func (i MappedUpdateIdentity) valid() bool {
	return i.promptValid() && i.Sequence > 0
}

func (i MappedUpdateIdentity) promptValid() bool {
	return i.Protocol == harnessv2.ProtocolVersion &&
		strings.TrimSpace(string(i.RuntimeInstanceID)) != "" &&
		strings.TrimSpace(string(i.SupervisorBootID)) != "" &&
		strings.TrimSpace(string(i.RuntimeSessionUID)) != "" &&
		i.RuntimeSessionGeneration > 0 &&
		strings.TrimSpace(string(i.TaskUID)) != "" &&
		i.TaskAttempt > 0 &&
		strings.TrimSpace(string(i.PromptID)) != ""
}

func (i MappedUpdateIdentity) samePrompt(other MappedUpdateIdentity) bool {
	return i.Protocol == other.Protocol &&
		i.RuntimeInstanceID == other.RuntimeInstanceID &&
		i.SupervisorBootID == other.SupervisorBootID &&
		i.RuntimeSessionUID == other.RuntimeSessionUID &&
		i.RuntimeSessionGeneration == other.RuntimeSessionGeneration &&
		i.TaskUID == other.TaskUID &&
		i.TaskAttempt == other.TaskAttempt &&
		i.PromptID == other.PromptID
}

// Key returns the stable recovery-deduplication key for one protocol update.
func (i MappedUpdateIdentity) Key() string {
	return strings.Join([]string{
		i.Protocol,
		string(i.RuntimeInstanceID),
		string(i.SupervisorBootID),
		string(i.RuntimeSessionUID),
		strconv.FormatUint(i.RuntimeSessionGeneration, 10),
		string(i.TaskUID),
		strconv.FormatUint(uint64(i.TaskAttempt), 10),
		string(i.PromptID),
		strconv.FormatUint(i.Sequence, 10),
	}, mappedUpdateIdentityKeySeparator)
}

// MappedUpdateIdentityFromEvent extracts a persisted harness v2 update
// identity. Events from other producers and malformed/truncated payloads are
// ignored.
func MappedUpdateIdentityFromEvent(event store.ExecutionEvent) (MappedUpdateIdentity, bool) {
	if len(event.Content) == 0 {
		return MappedUpdateIdentity{}, false
	}
	var content struct {
		HarnessV2 MappedUpdateIdentity `json:"harnessV2"`
	}
	if err := json.Unmarshal(event.Content, &content); err != nil || !content.HarnessV2.valid() {
		return MappedUpdateIdentity{}, false
	}
	return content.HarnessV2, true
}

func mappedExecutionEventKey(event store.ExecutionEvent) (MappedUpdateIdentity, string, bool) {
	identity, key, _, ok := mappedExecutionEventRecord(event)
	return identity, key, ok
}

func mappedExecutionEventRecord(
	event store.ExecutionEvent,
) (MappedUpdateIdentity, string, mappedJournalRecordKind, bool) {
	identity, ok := MappedUpdateIdentityFromEvent(event)
	if !ok {
		return MappedUpdateIdentity{}, "", mappedJournalRecordUpdate, false
	}
	var content struct {
		JournalKind string `json:"journalKind"`
		UpdateKind  string `json:"updateKind"`
	}
	if err := json.Unmarshal(event.Content, &content); err != nil {
		return MappedUpdateIdentity{}, "", mappedJournalRecordUpdate, false
	}
	if content.JournalKind == mappedToolStreamClosureKind {
		return identity, mappedToolStreamClosureKey(identity), mappedJournalRecordToolStreamClosure, true
	}
	if content.JournalKind == mappedTerminalUsageKind {
		return identity, mappedTerminalUsageKey(identity), mappedJournalRecordTerminalUsage, true
	}
	if content.JournalKind == mappedPromptAcceptedKind {
		return identity, mappedPromptLifecycleKey(identity, mappedPromptAcceptedKind), mappedJournalRecordPromptAccepted, true
	}
	if content.JournalKind == mappedPromptTerminalKind {
		return identity, mappedPromptLifecycleKey(identity, mappedPromptTerminalKind), mappedJournalRecordPromptTerminal, true
	}
	if content.UpdateKind == mappedAssistantTranscriptKind {
		return identity, identity.Key(), mappedJournalRecordAssistantTranscript, true
	}
	return identity, identity.Key(), mappedJournalRecordUpdate, true
}

func mappedToolStreamClosureKey(identity MappedUpdateIdentity) string {
	return identity.Key() + mappedUpdateIdentityKeySeparator + mappedToolStreamClosureKind
}

func mappedTerminalUsageKey(identity MappedUpdateIdentity) string {
	return identity.Key() + mappedUpdateIdentityKeySeparator + mappedTerminalUsageKind
}

func mappedPromptLifecycleKey(identity MappedUpdateIdentity, kind string) string {
	return identity.Key() + mappedUpdateIdentityKeySeparator + kind
}

func mappedJournalDedupeKey(key string) string {
	digest := sha256.Sum256([]byte(mappedJournalDedupeKeyDomain + key))
	return mappedJournalDedupeKeyPrefix + hex.EncodeToString(digest[:])
}

// PlanProjection is the durable/public read model derived from one ACP plan
// update.
type PlanProjection struct {
	Summary                    string
	ProgressPct                int
	GoalComplete               bool
	Document                   string
	EventDocument              string
	EventDocumentTruncated     bool
	EventDocumentOriginalChars int
	Total                      int
	Pending                    int
	InProgress                 int
	Completed                  int
}

// ProjectPlanUpdate converts structured ACP plan entries into the existing
// PlanStore contract and a bounded, redacted text representation for events.
func ProjectPlanUpdate(update harnessv2.PlanUpdate) PlanProjection {
	projection := PlanProjection{Total: len(update.Entries)}
	entries := redactPlanEntries(update.Entries)
	var inProgressSummary string
	var document strings.Builder
	document.WriteString("# Plan")
	for _, entry := range entries {
		switch entry.Status {
		case harnessv2.PlanEntryCompleted:
			projection.Completed++
		case harnessv2.PlanEntryInProgress:
			projection.InProgress++
			if inProgressSummary == "" {
				inProgressSummary = compactSummary(entry.Content)
			}
		default:
			projection.Pending++
		}
		document.WriteString("\n- ")
		if entry.Status == harnessv2.PlanEntryCompleted {
			document.WriteString("[x] ")
		} else {
			document.WriteString("[ ] ")
		}
		document.WriteString(strings.TrimSpace(entry.Content))
		if entry.Status == harnessv2.PlanEntryInProgress {
			document.WriteString(" _(in progress)_")
		}
		if priority := strings.TrimSpace(entry.Priority); priority != "" {
			document.WriteString(" _(priority: ")
			document.WriteString(priority)
			document.WriteString(")_")
		}
	}
	if projection.Total > 0 {
		projection.ProgressPct = projection.Completed * 100 / projection.Total
		projection.GoalComplete = projection.Completed == projection.Total
	}
	switch {
	case projection.GoalComplete:
		projection.Summary = fmt.Sprintf("Plan complete (%d/%d steps)", projection.Completed, projection.Total)
	case inProgressSummary != "":
		projection.Summary = fmt.Sprintf("Plan in progress (%d/%d complete): %s", projection.Completed, projection.Total, inProgressSummary)
	default:
		projection.Summary = fmt.Sprintf("Plan updated (%d/%d steps complete)", projection.Completed, projection.Total)
	}
	projection.Summary, _, _ = executionevents.RedactAndTruncateExecutionEventText(
		projection.Summary, executionevents.MaxExecutionEventSummaryChars,
	)
	projection.Document = executionevents.RedactExecutionEventText(document.String())
	projection.EventDocument, projection.EventDocumentTruncated, projection.EventDocumentOriginalChars =
		executionevents.RedactAndTruncateExecutionEventText(projection.Document, executionevents.MaxExecutionEventContentTextChars)
	return projection
}

func redactPlanEntries(entries []harnessv2.PlanEntry) []harnessv2.PlanEntry {
	redacted := append([]harnessv2.PlanEntry(nil), entries...)
	contents := make([]string, len(entries))
	priorities := make([]string, len(entries))
	ordered := make([]string, 0, len(entries)*2)
	for index, entry := range entries {
		redacted[index].Content = executionevents.RedactExecutionEventText(entry.Content)
		redacted[index].Priority = executionevents.RedactExecutionEventText(entry.Priority)
		contents[index] = redacted[index].Content
		priorities[index] = redacted[index].Priority
		ordered = append(ordered, redacted[index].Content, redacted[index].Priority)
	}
	if logicalSequenceSensitive(contents) || logicalSequenceSensitive(priorities) ||
		logicalSequenceSensitive(ordered) || logicalFieldPairSensitive(ordered) {
		for index := range redacted {
			if redacted[index].Content != "" {
				redacted[index].Content = executionevents.ExecutionEventRedactedValue
			}
			if redacted[index].Priority != "" {
				redacted[index].Priority = executionevents.ExecutionEventRedactedValue
			}
		}
	}
	return redacted
}

const maxLogicalFieldBoundaryRunes = 256

func logicalFieldPairSensitive(values []string) bool {
	type boundaries struct {
		prefix string
		suffix string
	}
	fields := make([]boundaries, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		runes := []rune(value)
		if len(runes) <= maxLogicalFieldBoundaryRunes {
			fields = append(fields, boundaries{prefix: value, suffix: value})
			continue
		}
		fields = append(fields, boundaries{
			prefix: string(runes[:maxLogicalFieldBoundaryRunes]),
			suffix: string(runes[len(runes)-maxLogicalFieldBoundaryRunes:]),
		})
	}
	for left := 0; left < len(fields); left++ {
		for right := left + 1; right < len(fields); right++ {
			forward := fields[left].suffix + fields[right].prefix
			reverse := fields[right].suffix + fields[left].prefix
			if executionevents.RedactExecutionEventText(forward) != forward ||
				executionevents.RedactExecutionEventText(reverse) != reverse {
				return true
			}
		}
	}
	return false
}

func logicalSequenceSensitive(values []string) bool {
	var forward, reverse strings.Builder
	for index, value := range values {
		forward.WriteString(value)
		reverse.WriteString(values[len(values)-1-index])
	}
	forwardText := forward.String()
	reverseText := reverse.String()
	return executionevents.RedactExecutionEventText(forwardText) != forwardText ||
		executionevents.RedactExecutionEventText(reverseText) != reverseText
}

type mapUpdateOptions struct {
	toolContentText                  *string
	toolContentTruncated             bool
	toolContentMultipleBlocksOmitted bool
	omitToolMetadata                 bool
	journalKind                      string
}

const (
	toolContentMultipleBlocksOmittedReason = "streamed_text_multiple_blocks_omitted"
	streamedTextTruncatedOrOmittedReason   = "streamed_text_truncated_or_omitted"
	assistantResponseOmittedSummary        = "Assistant response omitted"
)

// MapUpdate maps one validated harness v2 update to the public execution-event
// taxonomy. Streamed assistant and tool text is deliberately omitted here:
// independently redacting chunks can leak credentials split across update
// boundaries. Journal state supplies tool text only after the logical stream is
// complete, and assistant text is persisted from the terminal transcript.
func MapUpdate(event harnessv2.Event, mapCtx MapContext) (*store.ExecutionEvent, error) {
	return mapUpdate(event, mapCtx, mapUpdateOptions{})
}

func mapUpdate(event harnessv2.Event, mapCtx MapContext, options mapUpdateOptions) (*store.ExecutionEvent, error) {
	if err := mapCtx.validate(); err != nil {
		return nil, err
	}
	mapCtx = mapCtx.normalized()
	if event.Protocol != harnessv2.ProtocolVersion {
		return nil, fmt.Errorf("unsupported protocol %q", event.Protocol)
	}
	if event.Type != harnessv2.EventUpdate || event.Update == nil {
		return nil, fmt.Errorf("harness v2 update event is required")
	}
	if err := event.Identity.Validate(); err != nil {
		return nil, fmt.Errorf("invalid update identity: %w", err)
	}
	if err := event.Update.Validate(); err != nil {
		return nil, fmt.Errorf("invalid update payload: %w", err)
	}

	content := map[string]any{
		"harnessV2":  mappedUpdateIdentity(event),
		"updateKind": event.Update.Kind,
	}
	if options.journalKind != "" {
		content["journalKind"] = options.journalKind
	}
	mapped := &store.ExecutionEvent{
		Namespace:   mapCtx.Namespace,
		StreamType:  store.ExecutionEventStreamTypeTask,
		StreamID:    mapCtx.StreamID,
		Severity:    executionevents.ExecutionEventSeverityInfo,
		TaskName:    mapCtx.TaskName,
		SessionName: mapCtx.SessionName,
		AgentName:   mapCtx.AgentName,
		CreatedAt:   event.Identity.Timestamp.UTC(),
	}

	switch event.Update.Kind {
	case harnessv2.UpdateAssistantMessageChunk:
		mapped.Type = executionevents.ExecutionEventTypeModelMessage
		mapped.Summary = "Assistant message streamed"
		content["contentOmitted"] = "streamed_text_pending_terminal_redaction"
	case harnessv2.UpdateToolCall, harnessv2.UpdateToolCallUpdate:
		tool := event.Update.ToolCall
		mapped.ToolCallID = safeMappedToolCallID(tool.ToolCallID)
		mapped.Type, mapped.Severity = toolCallEventType(tool.Status)
		if options.omitToolMetadata {
			metadataFree := *tool
			metadataFree.Title = ""
			metadataFree.Kind = ""
			mapped.Summary = toolCallSummary(metadataFree)
			content["metadataOmitted"] = "streamed_metadata_pending_completion_redaction"
		} else {
			toolContentText := ""
			if options.toolContentText != nil {
				toolContentText = *options.toolContentText
			}
			fields := redactSmallLogicalFieldSet(tool.Title, tool.Kind, toolContentText)
			title, kind, toolContentText := fields[0], fields[1], fields[2]
			redactedTool := *tool
			redactedTool.Title = title
			redactedTool.Kind = kind
			mapped.ToolName = strings.TrimSpace(kind)
			mapped.ToolName, _, _ = executionevents.RedactAndTruncateExecutionEventText(mapped.ToolName, 128)
			mapped.Summary = toolCallSummary(redactedTool)
			if options.toolContentText != nil && strings.TrimSpace(title) == "" {
				if summary := compactSummary(toolContentText); summary != "" {
					mapped.Summary = summary
				}
			}
			content["title"] = title
			content["toolKind"] = kind
		}
		content["toolCallID"] = mapped.ToolCallID
		content["status"] = tool.Status
		content["contentBlockCount"] = len(tool.Content)
		if options.toolContentText != nil {
			fields := redactSmallLogicalFieldSet(tool.Title, tool.Kind, *options.toolContentText)
			mapped.ContentText = fields[2]
		} else if len(tool.Content) > 0 {
			content["contentOmitted"] = "streamed_text_pending_completion_redaction"
		}
		if options.toolContentTruncated || tool.ContentOmitted {
			mapped.ContentText = ""
			mapped.Truncation = &executionevents.ExecutionEventTruncation{ContentTextTruncated: true}
			content["contentOmitted"] = streamedTextTruncatedOrOmittedReason
		} else if options.toolContentMultipleBlocksOmitted {
			content["contentOmitted"] = toolContentMultipleBlocksOmittedReason
		}
	case harnessv2.UpdatePlan:
		projection := ProjectPlanUpdate(*event.Update.Plan)
		mapped.Type = executionevents.ExecutionEventTypePlanUpdated
		mapped.Summary = projection.Summary
		mapped.ContentText = projection.EventDocument
		content["totalEntries"] = projection.Total
		content["pendingEntries"] = projection.Pending
		content["inProgressEntries"] = projection.InProgress
		content["completedEntries"] = projection.Completed
		content["progressPct"] = projection.ProgressPct
		content["goalComplete"] = projection.GoalComplete
		if projection.EventDocumentTruncated {
			mapped.Truncation = &executionevents.ExecutionEventTruncation{
				ContentTextTruncated:     true,
				ContentTextOriginalChars: projection.EventDocumentOriginalChars,
			}
		}
	case harnessv2.UpdateUsage:
		mapUsageUpdate(event.Update.Usage, mapCtx, mapped, content)
	case harnessv2.UpdateDiagnostic:
		diagnostic := event.Update.Diagnostic
		fields := redactSmallLogicalFieldSet(diagnostic.Code, diagnostic.Message)
		code, message := fields[0], fields[1]
		mapped.Type = executionevents.ExecutionEventTypeAgentRuntimeCommandStarted
		mapped.Severity = executionevents.ExecutionEventSeverityError
		if diagnostic.Retryable {
			mapped.Severity = executionevents.ExecutionEventSeverityWarning
		}
		mapped.Summary = compactSummary(code + ": " + message)
		mapped.ContentText = message
		content["code"] = code
		content["retryable"] = diagnostic.Retryable
	default:
		return nil, fmt.Errorf("unsupported harness v2 update kind %q", event.Update.Kind)
	}

	encoded, err := json.Marshal(content)
	if err != nil {
		return nil, fmt.Errorf("marshal mapped harness v2 update: %w", err)
	}
	mapped.Content = encoded
	if err := store.SanitizeExecutionEventPayloadFields(mapped); err != nil {
		return nil, fmt.Errorf("sanitize mapped harness v2 update: %w", err)
	}
	return mapped, nil
}

func redactSmallLogicalFieldSet(values ...string) []string {
	redacted := make([]string, len(values))
	for index, value := range values {
		redacted[index] = executionevents.RedactExecutionEventText(value)
	}
	if !smallLogicalFieldSetSensitive(redacted) {
		return redacted
	}
	for index, value := range values {
		if value != "" {
			redacted[index] = executionevents.ExecutionEventRedactedValue
		}
	}
	return redacted
}

func smallLogicalFieldSetSensitive(values []string) bool {
	if len(values) < 2 || len(values) > 4 {
		return false
	}
	used := make([]bool, len(values))
	var visit func(string, int) bool
	visit = func(prefix string, depth int) bool {
		if depth >= 2 && executionevents.RedactExecutionEventText(prefix) != prefix {
			return true
		}
		if depth == len(values) {
			return false
		}
		for index, value := range values {
			if used[index] {
				continue
			}
			used[index] = true
			if visit(prefix+value, depth+1) {
				return true
			}
			used[index] = false
		}
		return false
	}
	return visit("", 0)
}

func mapUsageUpdate(
	usage *harnessv2.UsageUpdate,
	mapCtx MapContext,
	mapped *store.ExecutionEvent,
	content map[string]any,
) {
	hasTokenUsage := usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.CachedInputTokens > 0
	hasContextWindow := usage.ContextWindowUsed != nil
	if hasTokenUsage || !hasContextWindow {
		mapped.Type = executionevents.ExecutionEventTypeModelUsageUpdated
		mapped.Summary = fmt.Sprintf(
			"Model usage updated: %d input, %d output, %d cached input tokens",
			usage.InputTokens, usage.OutputTokens, usage.CachedInputTokens,
		)
		content["inputTokens"] = usage.InputTokens
		content["outputTokens"] = usage.OutputTokens
		content["cachedInputTokens"] = usage.CachedInputTokens
	} else {
		mapped.Type = executionevents.ExecutionEventTypeModelContextUpdated
		mapped.Summary = fmt.Sprintf(
			"Model context updated: %d of %d tokens used",
			*usage.ContextWindowUsed, *usage.ContextWindowSize,
		)
	}
	if usage.ContextWindowUsed != nil {
		content["contextWindowUsed"] = *usage.ContextWindowUsed
		content["contextWindowSize"] = *usage.ContextWindowSize
	}
	if mapCtx.Provider != "" {
		content["provider"] = mapCtx.Provider
	}
	if mapCtx.Model != "" {
		content["model"] = mapCtx.Model
	}
}

func hasUsageTelemetry(usage harnessv2.UsageUpdate) bool {
	return usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.CachedInputTokens > 0 ||
		usage.ContextWindowUsed != nil || usage.ContextWindowSize != nil
}

func mapToolUpdateWithContent(
	event harnessv2.Event,
	mapCtx MapContext,
	contentText string,
	contentTruncated bool,
	contentMultipleBlocksOmitted bool,
) (*store.ExecutionEvent, error) {
	return mapUpdate(event, mapCtx, mapUpdateOptions{
		toolContentText:                  &contentText,
		toolContentTruncated:             contentTruncated,
		toolContentMultipleBlocksOmitted: contentMultipleBlocksOmitted,
	})
}

func mapToolStreamClosure(
	event harnessv2.Event,
	mapCtx MapContext,
	contentText string,
	contentTruncated bool,
	contentMultipleBlocksOmitted bool,
) (*store.ExecutionEvent, error) {
	return mapUpdate(event, mapCtx, mapUpdateOptions{
		toolContentText:                  &contentText,
		toolContentTruncated:             contentTruncated,
		toolContentMultipleBlocksOmitted: contentMultipleBlocksOmitted,
		journalKind:                      mappedToolStreamClosureKind,
	})
}

func mapToolUpdateWithoutMetadata(event harnessv2.Event, mapCtx MapContext) (*store.ExecutionEvent, error) {
	return mapUpdate(event, mapCtx, mapUpdateOptions{omitToolMetadata: true})
}

// MapTerminalUsage projects usage reported only by a completed result while
// retaining the terminal event's durable protocol identity.
func MapTerminalUsage(event harnessv2.Event, mapCtx MapContext) (*store.ExecutionEvent, error) {
	if event.Type != harnessv2.EventCompleted || event.Completed == nil {
		return nil, fmt.Errorf("completed harness v2 event is required")
	}
	usage := event.Completed.Result.Usage
	if !hasUsageTelemetry(usage) {
		return nil, fmt.Errorf("completed harness v2 usage is required")
	}
	if err := usage.Validate(); err != nil {
		return nil, fmt.Errorf("invalid completed usage: %w", err)
	}
	if event.Completed.Result.Model != "" {
		mapCtx.Model = event.Completed.Result.Model
	}
	update := event
	update.Type = harnessv2.EventUpdate
	update.Completed = nil
	update.Update = &harnessv2.UpdateEvent{Kind: harnessv2.UpdateUsage, Usage: &usage}
	return mapUpdate(update, mapCtx, mapUpdateOptions{journalKind: mappedTerminalUsageKind})
}

// MapPromptLifecycle maps prompt acceptance and settlement into the existing
// model-request lifecycle taxonomy used by task traces and UI execution graphs.
func MapPromptLifecycle(event harnessv2.Event, mapCtx MapContext) (*store.ExecutionEvent, error) {
	if err := mapCtx.validate(); err != nil {
		return nil, err
	}
	mapCtx = mapCtx.normalized()
	if event.Protocol != harnessv2.ProtocolVersion {
		return nil, fmt.Errorf("unsupported protocol %q", event.Protocol)
	}
	if err := event.Identity.Validate(); err != nil {
		return nil, fmt.Errorf("invalid lifecycle identity: %w", err)
	}
	content := map[string]any{
		"harnessV2":      mappedUpdateIdentity(event),
		"modelRequestID": string(event.Identity.PromptID),
	}
	mapped := &store.ExecutionEvent{
		Namespace:   mapCtx.Namespace,
		StreamType:  store.ExecutionEventStreamTypeTask,
		StreamID:    mapCtx.StreamID,
		Severity:    executionevents.ExecutionEventSeverityInfo,
		TaskName:    mapCtx.TaskName,
		SessionName: mapCtx.SessionName,
		AgentName:   mapCtx.AgentName,
		CreatedAt:   event.Identity.Timestamp.UTC(),
	}
	model := mapCtx.Model
	switch event.Type {
	case harnessv2.EventAccepted:
		if event.Accepted == nil {
			return nil, fmt.Errorf("accepted harness v2 event is required")
		}
		if err := event.Accepted.Validate(); err != nil {
			return nil, fmt.Errorf("invalid accepted payload: %w", err)
		}
		content["journalKind"] = mappedPromptAcceptedKind
		content["acceptedAt"] = event.Accepted.AcceptedAt.UTC()
		content["acpVersion"] = event.Accepted.ACPVersion
		mapped.Type = executionevents.ExecutionEventTypeModelRequestStarted
		mapped.Summary = "Model request started"
	case harnessv2.EventCompleted:
		if event.Completed == nil {
			return nil, fmt.Errorf("completed harness v2 event is required")
		}
		if err := event.Completed.Validate(); err != nil {
			return nil, fmt.Errorf("invalid completed payload: %w", err)
		}
		if event.Completed.Result.Model != "" {
			model = event.Completed.Result.Model
		}
		content["journalKind"] = mappedPromptTerminalKind
		content["stopReason"] = event.Completed.StopReason
		mapped.Type = executionevents.ExecutionEventTypeModelRequestCompleted
		mapped.Summary = "Model request completed"
	case harnessv2.EventCancelled:
		if event.Cancelled == nil {
			return nil, fmt.Errorf("cancelled harness v2 event is required")
		}
		if err := event.Cancelled.Validate(); err != nil {
			return nil, fmt.Errorf("invalid cancelled payload: %w", err)
		}
		content["journalKind"] = mappedPromptTerminalKind
		content["stopReason"] = event.Cancelled.StopReason
		content["reason"] = executionevents.RedactExecutionEventText(event.Cancelled.Reason)
		mapped.Type = executionevents.ExecutionEventTypeModelRequestFailed
		mapped.Severity = executionevents.ExecutionEventSeverityWarning
		mapped.Summary = "Model request cancelled"
	case harnessv2.EventFailed:
		if event.Failed == nil {
			return nil, fmt.Errorf("failed harness v2 event is required")
		}
		if err := event.Failed.Validate(); err != nil {
			return nil, fmt.Errorf("invalid failed payload: %w", err)
		}
		fields := redactSmallLogicalFieldSet(event.Failed.Code, event.Failed.Message)
		content["journalKind"] = mappedPromptTerminalKind
		content["stopReason"] = event.Failed.StopReason
		content["code"] = fields[0]
		content["message"] = fields[1]
		mapped.Type = executionevents.ExecutionEventTypeModelRequestFailed
		mapped.Severity = executionevents.ExecutionEventSeverityError
		mapped.Summary = compactSummary(fields[0] + ": " + fields[1])
	case harnessv2.EventOutcomeUnknown:
		if event.OutcomeUnknown == nil {
			return nil, fmt.Errorf("outcome_unknown harness v2 event is required")
		}
		if err := event.OutcomeUnknown.Validate(); err != nil {
			return nil, fmt.Errorf("invalid outcome_unknown payload: %w", err)
		}
		fields := redactSmallLogicalFieldSet(event.OutcomeUnknown.Code, event.OutcomeUnknown.Message)
		content["journalKind"] = mappedPromptTerminalKind
		content["stopReason"] = event.Type
		content["code"] = fields[0]
		content["message"] = fields[1]
		content["forcedTermination"] = event.OutcomeUnknown.ForcedTermination
		mapped.Type = executionevents.ExecutionEventTypeModelRequestFailed
		mapped.Severity = executionevents.ExecutionEventSeverityError
		mapped.Summary = compactSummary(fields[0] + ": " + fields[1])
	default:
		return nil, fmt.Errorf("accepted or terminal harness v2 event is required")
	}
	if mapCtx.Provider != "" {
		content["provider"] = mapCtx.Provider
	}
	if model != "" {
		content["model"] = model
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return nil, fmt.Errorf("marshal mapped harness v2 lifecycle: %w", err)
	}
	mapped.Content = encoded
	if err := store.SanitizeExecutionEventPayloadFields(mapped); err != nil {
		return nil, fmt.Errorf("sanitize mapped harness v2 lifecycle: %w", err)
	}
	return mapped, nil
}

// MapAssistantTranscript maps the complete terminal assistant transcript as a
// single event so redaction sees credential shapes spanning protocol chunks.
func MapAssistantTranscript(
	event harnessv2.Event,
	mapCtx MapContext,
	transcript string,
	contentOmitted bool,
) (*store.ExecutionEvent, error) {
	if err := mapCtx.validate(); err != nil {
		return nil, err
	}
	mapCtx = mapCtx.normalized()
	if event.Protocol != harnessv2.ProtocolVersion {
		return nil, fmt.Errorf("unsupported protocol %q", event.Protocol)
	}
	if !event.Type.IsTerminal() &&
		(event.Type != harnessv2.EventUpdate || event.Update == nil || event.Update.AssistantMessage == nil) {
		return nil, fmt.Errorf("terminal or assistant-update harness v2 event is required")
	}
	if err := event.Identity.Validate(); err != nil {
		return nil, fmt.Errorf("invalid terminal identity: %w", err)
	}
	if transcript == "" && !contentOmitted {
		return nil, fmt.Errorf("assistant transcript is required")
	}
	if contentOmitted {
		// Never retain a prefix once the complete logical stream exceeded its
		// bound; a credential may span the discarded cutoff.
		transcript = ""
	}

	contentBody := map[string]any{
		"harnessV2":  mappedUpdateIdentity(event),
		"updateKind": mappedAssistantTranscriptKind,
	}
	if contentOmitted {
		contentBody["contentOmitted"] = streamedTextTruncatedOrOmittedReason
	}
	content, err := json.Marshal(contentBody)
	if err != nil {
		return nil, fmt.Errorf("marshal mapped harness v2 assistant transcript: %w", err)
	}
	mapped := &store.ExecutionEvent{
		Namespace:   mapCtx.Namespace,
		StreamType:  store.ExecutionEventStreamTypeTask,
		StreamID:    mapCtx.StreamID,
		Type:        executionevents.ExecutionEventTypeModelMessage,
		Severity:    executionevents.ExecutionEventSeverityInfo,
		TaskName:    mapCtx.TaskName,
		SessionName: mapCtx.SessionName,
		AgentName:   mapCtx.AgentName,
		Summary:     compactSummary(transcript),
		Content:     content,
		ContentText: transcript,
		CreatedAt:   event.Identity.Timestamp.UTC(),
	}
	if contentOmitted {
		mapped.Summary = assistantResponseOmittedSummary
		mapped.Truncation = &executionevents.ExecutionEventTruncation{ContentTextTruncated: true}
	}
	if err := store.SanitizeExecutionEventPayloadFields(mapped); err != nil {
		return nil, fmt.Errorf("sanitize mapped harness v2 assistant transcript: %w", err)
	}
	return mapped, nil
}

func safeMappedToolCallID(value string) string {
	value = strings.TrimSpace(value)
	digest := sha256.Sum256([]byte(mappedToolCallIDDomain + value))
	return mappedToolCallIDPrefix + hex.EncodeToString(digest[:])
}

func toolCallEventType(status harnessv2.ToolCallStatus) (string, string) {
	switch status {
	case harnessv2.ToolCallStatusCompleted:
		return executionevents.ExecutionEventTypeToolCallCompleted, executionevents.ExecutionEventSeverityInfo
	case harnessv2.ToolCallStatusFailed:
		return executionevents.ExecutionEventTypeToolCallFailed, executionevents.ExecutionEventSeverityError
	default:
		return executionevents.ExecutionEventTypeToolCallStarted, executionevents.ExecutionEventSeverityInfo
	}
}

func toolCallSummary(tool harnessv2.ToolCallUpdate) string {
	if title := compactSummary(tool.Title); title != "" {
		return title
	}
	return fmt.Sprintf("Tool call %s", strings.ReplaceAll(string(tool.Status), "_", " "))
}

func compactSummary(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	value, _, _ = executionevents.RedactAndTruncateExecutionEventText(value, executionevents.MaxExecutionEventSummaryChars)
	return value
}
