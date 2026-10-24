package supervisor

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	executionevents "github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// minRedactedCredentialBytes ignores values too short to be a credential, as
// the workspace-delta credential check does: replacing them would mangle text.
const minRedactedCredentialBytes = 8

// Harness bounds on agent-controlled strings, applied again after redaction.
const (
	maxACPToolNameBytes         = 253
	maxACPToolKindBytes         = 128
	maxPlanPriorityBytes        = 64
	maxPermissionOptionNameByte = 512
)

// credentialRedactor replaces the exact credential values a session's agent
// can see or the supervisor holds. It runs on every event before it leaves the
// supervisor; the shared pattern redaction then covers credential shapes
// Orka did not issue.
type credentialRedactor struct {
	// values are unique and longest first, so the longest match wins.
	values   []string
	replacer *strings.Replacer
}

func newCredentialRedactor(values ...string) *credentialRedactor {
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if len(value) >= minRedactedCredentialBytes && !slices.Contains(unique, value) {
			unique = append(unique, value)
		}
	}
	slices.SortFunc(unique, func(a, b string) int { return len(b) - len(a) })
	redactor := &credentialRedactor{values: unique}
	if len(unique) > 0 {
		pairs := make([]string, 0, 2*len(unique))
		for _, value := range unique {
			pairs = append(pairs, value, executionevents.ExecutionEventRedactedValue)
		}
		redactor.replacer = strings.NewReplacer(pairs...)
	}
	return redactor
}

// sessionCredentialRedactor covers the session's proxy credentials, their
// derived forms placed in the agent's environment, and the supervisor's own
// controller, capability and upstream provider credentials.
func (s *Server) sessionCredentialRedactor(state *sessionState) *credentialRedactor {
	values := []string{s.cfg.ControllerBearerToken, string(s.cfg.CapabilitySecret), s.cfg.ProviderProxy.UpstreamBearerToken}
	if state != nil && state.providerProxy != nil && len(state.providerProxy.credential) > 0 {
		credential := string(state.providerProxy.credential)
		values = append(values, credential, openCodeServerPassword(credential))
	}
	if state != nil && state.mcpProxy != nil {
		values = append(values, string(state.mcpProxy.credential))
	}
	return newCredentialRedactor(values...)
}

func (r *credentialRedactor) replace(value string) string {
	if r == nil || r.replacer == nil || value == "" {
		return value
	}
	return r.replacer.Replace(value)
}

func (r *credentialRedactor) containedIn(data []byte) bool {
	if r == nil {
		return false
	}
	for _, value := range r.values {
		if bytes.Contains(data, []byte(value)) {
			return true
		}
	}
	return false
}

// heldPrefixLength is the length of the longest suffix of text that begins a
// credential, which must wait for the next chunk.
func (r *credentialRedactor) heldPrefixLength(text string) int {
	held := 0
	for _, value := range r.values {
		for k := min(len(value)-1, len(text)); k > held; k-- {
			if strings.HasSuffix(text, value[:k]) {
				held = k
				break
			}
		}
	}
	return held
}

// redactionStream redacts one stream of text that arrives in chunks, so a
// credential split across chunks is still replaced. It holds back a suffix
// that could begin an exact credential, and text whose pattern redaction
// could still change: the last word, and anything still waiting for a value.
type redactionStream struct {
	redactor *credentialRedactor
	held     string
	// overflow takes everything once the held text passed
	// maxPatternHeldBytes with no safe cut. The stream then releases nothing
	// until it ends, so a long unfinished construct costs one pass rather
	// than one per chunk.
	overflow *strings.Builder
}

func (r *credentialRedactor) stream() *redactionStream {
	return &redactionStream{redactor: r}
}

// write returns the text that can no longer be part of a credential, with
// exact credentials replaced. Pattern redaction of the returned text cannot
// change with what follows.
func (s *redactionStream) write(chunk string) string {
	if s == nil {
		return chunk
	}
	if s.overflow != nil {
		s.overflow.WriteString(chunk)
		return ""
	}
	joined := s.held + chunk
	text, limit := joined, len(joined)
	if s.redactor != nil && s.redactor.replacer != nil {
		text = s.redactor.replace(joined)
		limit = len(text) - s.redactor.heldPrefixLength(text)
	}
	cut := 0
	// Without new whitespace and with the held text unchanged, the only
	// candidate cuts are the ones the held text was already refused at.
	if text != joined || strings.ContainsAny(chunk, redactionWhitespace) {
		cut = patternSafeCut(text, limit)
	}
	if cut == 0 && len(text) > maxPatternHeldBytes {
		s.overflow = &strings.Builder{}
		s.overflow.WriteString(text)
		s.held = ""
		return ""
	}
	for cut > 0 && cut < len(text) && !utf8.RuneStart(text[cut]) {
		cut--
	}
	s.held = text[cut:]
	return text[:cut]
}

// maxPatternHeldBytes bounds the text a stream re-examines on each chunk.
// Text with no safe cut in that much is held to the end of the stream, never
// released early.
const maxPatternHeldBytes = harnessv2.MaxProtocolStringBytes

// redactionWhitespace is what `\s` matches in the redaction patterns.
const redactionWhitespace = " \t\n\f\r"

// redactionProbes stand in for a value that has not arrived yet. If one
// changes when appended, the text before it ends in a construct that redacts
// what follows, such as "token is " or `password: "`.
var redactionProbes = []string{
	"Q7f2Lk9Xw3Rt8Zp1Vn5Bc6Md4Hs0Gy", `Q7f2Lk9Xw3Rt8Zp1Vn5Bc6Md4Hs0Gy"`, `Q7f2Lk9Xw3Rt8Zp1Vn5Bc6Md4Hs0Gy'`,
}

// patternSafeCut returns where text can be cut, at or before limit, so that
// pattern redaction of the part before the cut cannot change with what
// follows, or 0 when there is no such cut yet. A cut follows whitespace, so
// no word is split; nothing before it waits for a value; and no match crosses
// it. Only the last whitespace and the last line break are tried, so the work
// per chunk stays bounded by the held text.
func patternSafeCut(text string, limit int) int {
	space := strings.LastIndexAny(text[:limit], redactionWhitespace) + 1
	line := strings.LastIndexByte(text[:limit], '\n') + 1
	for _, cut := range []int{space, line} {
		if cut > 0 && patternFinal(text[:cut], text[cut:]) {
			return cut
		}
	}
	return 0
}

// patternFinal reports whether prefix redacts the same whatever follows it.
func patternFinal(prefix, rest string) bool {
	redacted := executionevents.RedactExecutionEventText(prefix)
	for _, probe := range redactionProbes {
		if executionevents.RedactExecutionEventText(prefix+probe) != redacted+probe {
			return false
		}
	}
	return redacted+executionevents.RedactExecutionEventText(rest) == executionevents.RedactExecutionEventText(prefix+rest)
}

// flush returns the held text once its stream ended.
func (s *redactionStream) flush() string {
	if s == nil {
		return ""
	}
	text := s.held
	if s.overflow != nil {
		text = s.overflow.String()
	}
	s.held, s.overflow = "", nil
	if s.redactor != nil {
		text = s.redactor.replace(text)
	}
	return text
}

func (s *redactionStream) pending() bool {
	return s != nil && (s.held != "" || s.overflow != nil && s.overflow.Len() > 0)
}

// redactPermission redacts the agent-controlled text of a permission request.
func (r *credentialRedactor) redactPermission(permission *harnessv2.PermissionRequestedEvent) {
	permission.Title = boundACPToolCallTitle(r.replace(permission.Title))
	for i := range permission.Options {
		permission.Options[i].Name = truncateUTF8(r.redactText(permission.Options[i].Name), maxPermissionOptionNameByte)
	}
}

// redactText applies exact then pattern redaction to a complete value.
func (r *credentialRedactor) redactText(value string) string {
	if value == "" {
		return value
	}
	return executionevents.RedactExecutionEventText(r.replace(value))
}

// redactToolCall redacts every text field of a tool update. Content, raw
// input and diffs are complete snapshots in ACP, so exact replacement needs no
// hold-back across updates.
//
// Redaction can lengthen text, so each field is bounded again afterwards:
// content or edits that no longer fit are dropped and marked omitted.
func (r *credentialRedactor) redactToolCall(tool *harnessv2.ToolCallUpdate) {
	if tool == nil {
		return
	}
	tool.Title = boundACPToolCallTitle(r.replace(tool.Title))
	tool.Name = truncateUTF8(r.redactText(tool.Name), maxACPToolNameBytes)
	tool.Kind = truncateUTF8(r.redactText(tool.Kind), maxACPToolKindBytes)
	for i := range tool.Content {
		if tool.Content[i].Type == harnessv2.ContentBlockText {
			tool.Content[i].Text = r.redactText(tool.Content[i].Text)
		}
		tool.Content[i].URI = r.redactText(tool.Content[i].URI)
		tool.Content[i].Name = r.redactText(tool.Content[i].Name)
		tool.Content[i].MimeType = r.redactText(tool.Content[i].MimeType)
		if acpToolContentExceedsHarnessLimits(tool.Content[i]) {
			tool.Content, tool.ContentReplace, tool.ContentOmitted = nil, false, true
			break
		}
	}
	for i := range tool.Diffs {
		tool.Diffs[i].Path = r.redactText(tool.Diffs[i].Path)
		if tool.Diffs[i].OldText != nil {
			old := r.redactText(*tool.Diffs[i].OldText)
			tool.Diffs[i].OldText = &old
		}
		tool.Diffs[i].NewText = r.redactText(tool.Diffs[i].NewText)
	}
	if len(tool.Diffs) > 0 {
		if err := (harnessv2.ToolCallUpdate{ToolCallID: "bound", Status: harnessv2.ToolCallStatusPending, Diffs: tool.Diffs}).Validate(); err != nil {
			tool.Diffs, tool.DiffsOmitted = nil, true
		}
	}
}

// truncateUTF8 bounds text to limit bytes on a rune boundary.
func truncateUTF8(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut]
}

// redactRawInput redacts every string in a JSON value. It returns nil when
// the value cannot be decoded, so undecodable input never leaves unredacted.
func (r *credentialRedactor) redactRawInput(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil
	}
	encoded, err := json.Marshal(r.redactJSONValue(value))
	if err != nil {
		return nil
	}
	return encoded
}

func (r *credentialRedactor) redactJSONValue(value any) any {
	switch typed := value.(type) {
	case string:
		return r.redactText(typed)
	case json.Number:
		// A credential made only of digits decodes as a number.
		if replaced := r.replace(string(typed)); replaced != string(typed) {
			return replaced
		}
		return typed
	case []any:
		for i := range typed {
			typed[i] = r.redactJSONValue(typed[i])
		}
		return typed
	case map[string]any:
		redacted := make(map[string]any, len(typed))
		for key, item := range typed {
			redacted[r.redactText(key)] = r.redactJSONValue(item)
		}
		return redacted
	default:
		return value
	}
}

// scrubEvent is the last check before an event is encoded: any exact
// credential still present anywhere in the event is replaced in every string.
// The per-field redaction above makes this a no-op in practice.
func (r *credentialRedactor) scrubEvent(event *harnessv2.Event) error {
	if r == nil || r.replacer == nil || event == nil {
		return nil
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if !r.containedIn(encoded) {
		return nil
	}
	var generic any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&generic); err != nil {
		return err
	}
	scrubbed, err := json.Marshal(r.replaceJSONValue(generic))
	if err != nil {
		return err
	}
	var replaced harnessv2.Event
	if err := json.Unmarshal(scrubbed, &replaced); err != nil {
		return err
	}
	if encoded, err = json.Marshal(replaced); err != nil {
		return err
	}
	if r.containedIn(encoded) {
		// Fail closed: the event is not sent.
		return errEventCarriesCredential
	}
	*event = replaced
	return nil
}

var errEventCarriesCredential = errors.New("event still carries a session credential after redaction")

func (r *credentialRedactor) replaceJSONValue(value any) any {
	switch typed := value.(type) {
	case string:
		return r.replace(typed)
	case json.Number:
		if replaced := r.replace(string(typed)); replaced != string(typed) {
			return replaced
		}
		return typed
	case []any:
		for i := range typed {
			typed[i] = r.replaceJSONValue(typed[i])
		}
		return typed
	case map[string]any:
		replaced := make(map[string]any, len(typed))
		for key, item := range typed {
			replaced[r.replace(key)] = r.replaceJSONValue(item)
		}
		return replaced
	default:
		return value
	}
}

// textRedaction returns the redaction stream of an assistant or thought
// update.
func (p *promptState) textRedaction(update *harnessv2.UpdateEvent) (*redactionStream, bool) {
	switch {
	case update.AssistantMessage != nil:
		return p.assistantRedaction, true
	case update.AssistantThought != nil:
		return p.thoughtRedaction, true
	default:
		return nil, false
	}
}

func updateText(update *harnessv2.UpdateEvent) string {
	switch {
	case update.AssistantMessage != nil:
		return update.AssistantMessage.Text
	case update.AssistantThought != nil:
		return update.AssistantThought.Text
	default:
		return ""
	}
}

// redactUpdate redacts a non-text update in place.
func (p *promptState) redactUpdate(update *harnessv2.UpdateEvent) {
	switch {
	case update.ToolCall != nil:
		p.redactor.redactToolCall(update.ToolCall)
		if len(update.ToolCall.RawInput) > 0 {
			raw := p.redactor.redactRawInput(update.ToolCall.RawInput)
			if len(raw) > harnessv2.MaxToolCallRawInputBytes {
				raw = nil
			}
			update.ToolCall.RawInput = raw
		}
	case update.Plan != nil:
		for i := range update.Plan.Entries {
			entry := &update.Plan.Entries[i]
			entry.Content = truncateUTF8(p.redactor.redactText(entry.Content), harnessv2.MaxProtocolStringBytes)
			entry.Priority = truncateUTF8(p.redactor.redactText(entry.Priority), maxPlanPriorityBytes)
		}
	case update.Diagnostic != nil:
		update.Diagnostic.Message = truncateUTF8(p.redactor.redactText(update.Diagnostic.Message), harnessv2.MaxDiagnosticBytes)
	}
}

// textUpdateEventsLocked builds the events for redacted text of one kind,
// pattern-redacting it and splitting it to the per-event bound. Callers hold
// s.mu.
func (s *Server) textUpdateEventsLocked(
	state *sessionState, prompt *promptState, kind harnessv2.UpdateKind, text string, at time.Time,
) []harnessv2.Event {
	if text == "" {
		return nil
	}
	text = executionevents.RedactExecutionEventText(text)
	var events []harnessv2.Event
	for _, piece := range splitUTF8(text, harnessv2.MaxProtocolStringBytes) {
		update := &harnessv2.UpdateEvent{Kind: kind}
		chunk := &harnessv2.AssistantMessageChunk{Text: piece}
		if kind == harnessv2.UpdateAssistantThoughtChunk {
			update.AssistantThought = chunk
		} else {
			update.AssistantMessage = chunk
		}
		events = append(events, s.identifiedLocked(state, prompt, at, harnessv2.Event{Type: harnessv2.EventUpdate, Update: update}))
	}
	return events
}

// identifiedLocked gives an event the prompt's next sequence. Callers hold s.mu.
func (s *Server) identifiedLocked(state *sessionState, prompt *promptState, at time.Time, event harnessv2.Event) harnessv2.Event {
	prompt.sequence++
	prompt.lastEventAt = at
	event.Protocol = harnessv2.ProtocolVersion
	event.Identity = eventIdentity(s.cfg.Fence, state.descriptor, prompt.request.Metadata, prompt.sequence, at)
	return event
}

// flushHeldTextLocked ends both text streams and returns their held text as
// events. Callers hold s.mu.
func (s *Server) flushHeldTextLocked(state *sessionState, prompt *promptState, at time.Time) []harnessv2.Event {
	var events []harnessv2.Event
	if prompt.assistantRedaction.pending() {
		events = append(events, s.textUpdateEventsLocked(
			state, prompt, harnessv2.UpdateAssistantMessageChunk, prompt.assistantRedaction.flush(), at)...)
	}
	if prompt.thoughtRedaction.pending() {
		events = append(events, s.textUpdateEventsLocked(
			state, prompt, harnessv2.UpdateAssistantThoughtChunk, prompt.thoughtRedaction.flush(), at)...)
	}
	return events
}

// flushTextRedaction ends both text streams when the prompt's event stream
// ends. Held text is released only by its own stream's next chunk or here,
// never by an interleaved event: a tool update or another stream can come
// between two chunks of one message, so releasing it early could emit a
// credential's prefix apart from its rest. The held text keeps the last
// event's time, so it never follows the terminal event's settlement time.
func (s *Server) flushTextRedaction(state *sessionState, prompt *promptState) []harnessv2.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	at := prompt.lastEventAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return s.flushHeldTextLocked(state, prompt, at)
}

// splitUTF8 splits text into pieces of at most limit bytes on rune boundaries.
func splitUTF8(text string, limit int) []string {
	var pieces []string
	for len(text) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		pieces = append(pieces, text[:cut])
		text = text[cut:]
	}
	return append(pieces, text)
}
