package supervisor

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	executionevents "github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const minRedactedCredentialBytes = 8

const (
	maxACPToolNameBytes         = 253
	maxACPToolKindBytes         = 128
	maxPlanPriorityBytes        = 64
	maxPermissionOptionNameByte = 512
)

type credentialRedactor struct {
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

type redactionStream struct {
	redactor *credentialRedactor
	held     string
	overflow *strings.Builder
	released int
	sent     string
}

func (r *credentialRedactor) stream() *redactionStream {
	return &redactionStream{redactor: r}
}

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
	if text != joined || strings.ContainsAny(chunk, redactionWhitespace) {
		cut = patternSafeCut(text, limit)
	}
	if cut < s.released {
		cut = 0
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
	return s.unsent(text[:cut], cut > 0)
}

func (s *redactionStream) unsent(final string, done bool) string {
	if s.released == 0 {
		return final
	}
	if !done {
		return ""
	}
	redacted := executionevents.RedactExecutionEventText(final)
	common := 0
	if len(redacted) >= len(s.sent) && strings.EqualFold(redacted[:len(s.sent)], s.sent) {
		common = len(s.sent)
	}
	for common < len(redacted) && common < len(s.sent) && redacted[common] == s.sent[common] {
		common++
	}
	for common > 0 && common < len(redacted) && !utf8.RuneStart(redacted[common]) {
		common--
	}
	s.released, s.sent = 0, ""
	return redacted[common:]
}

const maxPatternHeldBytes = harnessv2.MaxProtocolStringBytes

const redactionWhitespace = " \t\n\f\r"

var redactionProbes = []string{
	"Q7f2Lk9Xw3Rt8Zp1Vn5Bc6Md4Hs0Gy", `Q7f2Lk9Xw3Rt8Zp1Vn5Bc6Md4Hs0Gy"`, `Q7f2Lk9Xw3Rt8Zp1Vn5Bc6Md4Hs0Gy'`,
}

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

func patternFinal(prefix, rest string) bool {
	redacted := executionevents.RedactExecutionEventText(prefix)
	for _, probe := range redactionProbes {
		if executionevents.RedactExecutionEventText(prefix+probe) != redacted+probe {
			return false
		}
	}
	return redacted+executionevents.RedactExecutionEventText(rest) == executionevents.RedactExecutionEventText(prefix+rest)
}

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
	return s.unsent(text, true)
}

var releasablePlainText = regexp.MustCompile(`^[A-Za-z0-9 \t\n\f\r.,;!'()*_~-]*$`)

var heldCredentialShape = regexp.MustCompile(`eyJ|[A-Za-z0-9_~-]{16,}`)

func (s *redactionStream) release() string {
	if s == nil || s.overflow != nil || len(s.held) == s.released {
		return ""
	}
	if s.redactor != nil && s.redactor.heldPrefixLength(s.held) > 0 {
		return ""
	}
	if !releasablePlainText.MatchString(s.held) || heldCredentialShape.MatchString(s.held) || !patternFinal(s.held, "") {
		return ""
	}
	redacted := executionevents.RedactExecutionEventText(s.held)
	if !strings.HasPrefix(redacted, s.sent) {
		return ""
	}
	text := redacted[len(s.sent):]
	s.released, s.sent = len(s.held), redacted
	return text
}

func (s *redactionStream) pending() bool {
	return s != nil && (len(s.held) > s.released || s.overflow != nil && s.overflow.Len() > 0)
}

func (r *credentialRedactor) redactPermission(permission *harnessv2.PermissionRequestedEvent) {
	permission.Title = boundACPToolCallTitle(r.replace(permission.Title))
	for i := range permission.Options {
		permission.Options[i].Name = truncateUTF8(r.redactText(permission.Options[i].Name), maxPermissionOptionNameByte)
	}
}

func (r *credentialRedactor) permissionRawInput(toolCall json.RawMessage) json.RawMessage {
	var call struct {
		RawInput json.RawMessage `json:"rawInput"`
	}
	if json.Unmarshal(toolCall, &call) != nil {
		return nil
	}
	raw := json.RawMessage(bytes.TrimSpace(call.RawInput))
	if len(raw) == 0 || string(raw) == acpJSONNull || len(raw) > harnessv2.MaxToolCallRawInputBytes || !json.Valid(raw) {
		return nil
	}
	if raw = r.redactRawInput(raw); len(raw) > harnessv2.MaxToolCallRawInputBytes {
		return nil
	}
	return raw
}

func (r *credentialRedactor) redactText(value string) string {
	if value == "" {
		return value
	}
	return executionevents.RedactExecutionEventText(r.replace(value))
}

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

func (s *Server) identifiedLocked(state *sessionState, prompt *promptState, at time.Time, event harnessv2.Event) harnessv2.Event {
	prompt.sequence++
	prompt.lastEventAt = at
	event.Protocol = harnessv2.ProtocolVersion
	event.Identity = eventIdentity(s.cfg.Fence, state.descriptor, prompt.request.Metadata, prompt.sequence, at)
	return event
}

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

func (s *Server) releaseHeldTextLocked(
	state *sessionState, prompt *promptState, except *redactionStream, at time.Time,
) []harnessv2.Event {
	var events []harnessv2.Event
	if prompt.assistantRedaction != except {
		events = append(events, s.textUpdateEventsLocked(
			state, prompt, harnessv2.UpdateAssistantMessageChunk, prompt.assistantRedaction.release(), at)...)
	}
	if prompt.thoughtRedaction != except {
		events = append(events, s.textUpdateEventsLocked(
			state, prompt, harnessv2.UpdateAssistantThoughtChunk, prompt.thoughtRedaction.release(), at)...)
	}
	return events
}

func (s *Server) flushTextRedaction(state *sessionState, prompt *promptState) []harnessv2.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	at := prompt.lastEventAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return s.flushHeldTextLocked(state, prompt, at)
}

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
