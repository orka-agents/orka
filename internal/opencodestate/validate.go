package opencodestate

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	roleUser      = "user"
	roleAssistant = "assistant"
	finishStop    = "stop"
	partFile      = "file"
)

func (s *snapshot) validate(sessionID, cwd, model string) error {
	if s.Format != formatVersion || s.Version != ProviderVersion {
		return incompatible("snapshot format or provider version does not match")
	}
	if s.CWD != cwd || s.Model != model || s.Session.ID != sessionID || s.Session.Directory != cwd {
		return incompatible("snapshot does not match the authorized conversation, directory, or model")
	}
	if err := s.validateSession(); err != nil {
		return err
	}
	if len(s.Messages) > MaxMessages || len(s.Parts) > MaxParts {
		return ErrSizeLimit
	}
	if len(s.Messages) < 2 || len(s.Parts) == 0 {
		return incompatible("conversation has no completed exchange")
	}
	messages, err := s.validateMessages()
	if err != nil {
		return err
	}
	return s.validateParts(messages)
}

func (s *snapshot) validateSession() error {
	project, session := s.Project, s.Session
	if !validID(project.ID, "") || project.ID != session.ProjectID || !canonicalAbsolutePath(project.Worktree) ||
		project.TimeCreated < 0 || project.TimeUpdated < project.TimeCreated {
		return incompatible("conversation project identity is invalid")
	}
	relative, err := filepath.Rel(project.Worktree, s.CWD)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return incompatible("working directory is outside the recorded project")
	}
	if project.VCS != nil && *project.VCS != "git" {
		return incompatible("project VCS is unsupported")
	}
	if session.Path != nil && *session.Path != "" && *session.Path != filepath.ToSlash(relative) {
		return incompatible("native project path does not match the stable working directory")
	}
	if session.Version != ProviderVersion || session.Agent != Mode || session.Slug == "" ||
		len(session.Slug) > 256 || len(session.Title) > MaxRowBytes ||
		session.TimeCreated < 0 || session.TimeUpdated < session.TimeCreated {
		return incompatible("native conversation version, mode, or metadata is unsupported")
	}
	return validateModel(session.Model, s.Model, "id")
}

type messageInfo struct {
	role   string
	finish string
}

func (s *snapshot) validateMessages() (map[string]messageInfo, error) {
	seen := make(map[string]messageInfo, len(s.Messages))
	var lastUser, finish string
	var previous messageRow
	for _, row := range s.Messages {
		if !validID(row.ID, "msg_") || row.SessionID != s.Session.ID ||
			row.TimeCreated < 0 || row.TimeUpdated < row.TimeCreated {
			return nil, incompatible("native message identity is invalid")
		}
		if _, exists := seen[row.ID]; exists {
			return nil, incompatible("native message IDs are duplicated")
		}
		if previous.ID != "" && (row.TimeCreated < previous.TimeCreated ||
			row.TimeCreated == previous.TimeCreated && row.ID <= previous.ID) {
			return nil, incompatible("native messages are not in recorded order")
		}
		info, parentID, err := s.validateMessage(row)
		if err != nil {
			return nil, err
		}
		if info.role == roleUser {
			if lastUser != "" && finish != finishStop {
				return nil, incompatible("a prior native exchange is unfinished")
			}
			lastUser, finish = row.ID, ""
		} else {
			if lastUser == "" || parentID != lastUser || finish == finishStop {
				return nil, incompatible("native assistant message belongs to another exchange")
			}
			finish = info.finish
		}
		seen[row.ID] = info
		previous = row
	}
	if finish != finishStop {
		return nil, incompatible("last native exchange is unfinished")
	}
	return seen, nil
}

func (s *snapshot) validateMessage(row messageRow) (messageInfo, string, error) {
	data, err := nativeObject(row.Data)
	if err != nil {
		return messageInfo{}, "", err
	}
	role, ok := stringField(data, "role")
	if !ok || role != roleUser && role != roleAssistant {
		return messageInfo{}, "", incompatible("native message role is unsupported")
	}
	if agent, _ := stringField(data, "agent"); agent != Mode {
		return messageInfo{}, "", incompatible("native message mode does not match current policy")
	}
	created, completed, err := messageTimes(data["time"])
	if err != nil || created != row.TimeCreated {
		return messageInfo{}, "", incompatible("native message timestamps are invalid")
	}
	if role == roleUser {
		err := validateUserMessage(data, s.Model)
		return messageInfo{role: role}, "", err
	}
	if completed == nil || *completed < created {
		return messageInfo{}, "", incompatible("native assistant message is unfinished")
	}
	return s.validateAssistantMessage(data)
}

func validateUserMessage(data map[string]json.RawMessage, model string) error {
	if !hasOnlyFields(data, "role time agent model format summary system tools") {
		return incompatible("native user message schema is unsupported")
	}
	if nonemptyJSON(data["tools"]) || nonemptyJSON(data["system"]) {
		return incompatible("native user message retains configuration overrides")
	}
	return validateModel(data["model"], model, "modelID")
}

func (s *snapshot) validateAssistantMessage(data map[string]json.RawMessage) (messageInfo, string, error) {
	if !hasOnlyFields(data, "role time error parentID modelID providerID mode agent path summary cost tokens structured variant finish") {
		return messageInfo{}, "", incompatible("native assistant message schema is unsupported")
	}
	provider, _ := stringField(data, "providerID")
	model, _ := stringField(data, "modelID")
	mode, _ := stringField(data, "mode")
	if provider != ProviderID || model != s.Model || mode != Mode || nonemptyJSON(data["variant"]) {
		return messageInfo{}, "", incompatible("native assistant settings do not match current policy")
	}
	if nonemptyJSON(data["error"]) {
		return messageInfo{}, "", incompatible("native assistant message contains an error")
	}
	var path struct {
		CWD  string `json:"cwd"`
		Root string `json:"root"`
	}
	if json.Unmarshal(data["path"], &path) != nil || path.CWD != s.CWD || path.Root != s.Project.Worktree {
		return messageInfo{}, "", incompatible("native assistant working directory is incompatible")
	}
	parentID, _ := stringField(data, "parentID")
	finish, _ := stringField(data, "finish")
	if !validID(parentID, "msg_") || finish != finishStop && finish != "tool-calls" {
		return messageInfo{}, "", incompatible("native assistant completion is unsupported")
	}
	if !nonnegativeNumber(data["cost"]) || !validTokenUsage(data["tokens"]) {
		return messageInfo{}, "", incompatible("native assistant usage is invalid")
	}
	return messageInfo{role: roleAssistant, finish: finish}, parentID, nil
}

func validateModel(raw json.RawMessage, model, idKey string) error {
	data, err := nativeObject(raw)
	if err != nil {
		return err
	}
	if !hasOnlyFields(data, idKey+" providerID variant") {
		return incompatible("native model schema is unsupported")
	}
	provider, _ := stringField(data, "providerID")
	id, _ := stringField(data, idKey)
	variant, _ := stringField(data, "variant")
	// OpenCode persists "default" only on session.model when no variant was
	// selected. User and assistant messages omit the variant in that case.
	defaultVariant := idKey == "id" && variant == "default"
	if provider != ProviderID || id != model || nonemptyJSON(data["variant"]) && !defaultVariant {
		return incompatible("native model does not match the governed profile")
	}
	return nil
}

func (s *snapshot) validateParts(messages map[string]messageInfo) error {
	seen := make(map[string]bool, len(s.Parts))
	messageParts := make(map[string]int, len(messages))
	for _, row := range s.Parts {
		owner, exists := messages[row.MessageID]
		if !exists || !validID(row.ID, "prt_") || row.SessionID != s.Session.ID || seen[row.ID] ||
			row.TimeCreated < 0 || row.TimeUpdated < row.TimeCreated {
			return incompatible("native part identity is invalid or belongs to another message")
		}
		data, err := nativeObject(row.Data)
		if err != nil {
			return err
		}
		if err := validatePart(data, row, owner.role); err != nil {
			return err
		}
		seen[row.ID] = true
		messageParts[row.MessageID]++
	}
	for id := range messages {
		if messageParts[id] == 0 {
			return incompatible("native message has no saved parts")
		}
	}
	return nil
}

func validatePart(data map[string]json.RawMessage, row partRow, role string) error {
	if nonemptyJSON(data["snapshot"]) || externalOutputReference(data["metadata"]) {
		return incompatible("native history references unsupported external snapshot or tool-output files")
	}
	kind, _ := stringField(data, "type")
	switch kind {
	case "text", "reasoning":
		if !hasOnlyFields(data, "type text synthetic ignored time metadata") {
			return incompatible("native text part schema is unsupported")
		}
		text, ok := stringField(data, "text")
		if !ok || strings.Contains(text, "Full output saved to:") {
			return incompatible("native text part references unsupported external output")
		}
	case partFile:
		return validateFilePart(data, row, false)
	case "tool":
		if role != roleAssistant {
			return incompatible("native tool part is not attached to an assistant message")
		}
		return validateToolPart(data, row)
	case "step-start":
		if role != roleAssistant || !hasOnlyFields(data, "type snapshot") {
			return incompatible("native step-start part is unsupported")
		}
	case "step-finish":
		if role != roleAssistant || !hasOnlyFields(data, "type reason snapshot cost tokens") ||
			!nonnegativeNumber(data["cost"]) || !validTokenUsage(data["tokens"]) {
			return incompatible("native step-finish part is unsupported")
		}
	default:
		return incompatible("native history contains unsupported autonomous, compaction, retry, or snapshot state")
	}
	return nil
}

func validateToolPart(data map[string]json.RawMessage, row partRow) error {
	if !hasOnlyFields(data, "type callID tool state metadata") {
		return incompatible("native tool part schema is unsupported")
	}
	callID, callOK := stringField(data, "callID")
	tool, toolOK := stringField(data, "tool")
	if !callOK || callID == "" || len(callID) > 256 || !toolOK || tool == "" || len(tool) > 256 {
		return incompatible("native tool part identity is invalid")
	}
	switch tool {
	case "task", "todowrite", "plan_enter", "plan_exit":
		return incompatible("native tool requires unsupported autonomous state")
	}
	state, err := nativeObject(data["state"])
	if err != nil {
		return err
	}
	if !hasOnlyFields(state, "status input output title metadata time attachments error") {
		return incompatible("native tool state schema is unsupported")
	}
	status, _ := stringField(state, "status")
	if status != "completed" && status != "error" {
		return incompatible("native tool call is unfinished")
	}
	if externalOutputReference(state["metadata"]) {
		return incompatible("native tool output requires unsupported external files")
	}
	timeFields, err := nativeObject(state["time"])
	if err != nil || !hasOnlyFields(timeFields, "start end") {
		return incompatible("native tool timestamps contain unsupported compaction state")
	}
	var times struct {
		Start *int64 `json:"start"`
		End   *int64 `json:"end"`
	}
	if json.Unmarshal(state["time"], &times) != nil || times.Start == nil || times.End == nil ||
		*times.Start < 0 || *times.End < *times.Start {
		return incompatible("native tool completion is invalid")
	}
	if status == "completed" {
		output, ok := stringField(state, "output")
		if !ok || strings.Contains(output, "Full output saved to:") {
			return incompatible("native tool output references unsupported external files")
		}
	} else if _, ok := stringField(state, "error"); !ok {
		return incompatible("native tool error result is invalid")
	}
	if _, err := nativeObject(state["input"]); err != nil {
		return incompatible("native tool input is invalid")
	}
	return validateAttachments(state["attachments"], row)
}

func validateAttachments(raw json.RawMessage, row partRow) error {
	if len(raw) == 0 {
		return nil
	}
	var attachments []json.RawMessage
	if json.Unmarshal(raw, &attachments) != nil || len(attachments) > 128 {
		return incompatible("native tool attachments are invalid")
	}
	for _, raw := range attachments {
		attachment, err := nativeObject(raw)
		if err != nil {
			return err
		}
		if err := validateFilePart(attachment, row, true); err != nil {
			return err
		}
	}
	return nil
}

func validateFilePart(data map[string]json.RawMessage, row partRow, attachment bool) error {
	fields := "type mime filename url source"
	if attachment {
		fields += " id sessionID messageID"
		id, _ := stringField(data, "id")
		sessionID, _ := stringField(data, "sessionID")
		messageID, _ := stringField(data, "messageID")
		if !validID(id, "prt_") || sessionID != row.SessionID || messageID != row.MessageID {
			return incompatible("native attachment belongs to another conversation")
		}
	}
	kind, _ := stringField(data, "type")
	mime, _ := stringField(data, "mime")
	location, _ := stringField(data, "url")
	if !hasOnlyFields(data, fields) || kind != partFile || mime == "" ||
		!strings.HasPrefix(location, "data:") || !strings.Contains(location, ",") {
		return incompatible("native attachment requires unsupported external files")
	}
	return nil
}

func externalOutputReference(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var metadata map[string]json.RawMessage
	if json.Unmarshal(raw, &metadata) != nil {
		return true
	}
	return nonemptyJSON(metadata["outputPath"]) || nonemptyJSON(metadata["snapshot"])
}

func messageTimes(raw json.RawMessage) (int64, *int64, error) {
	var times struct {
		Created   *int64 `json:"created"`
		Completed *int64 `json:"completed"`
	}
	if json.Unmarshal(raw, &times) != nil || times.Created == nil || *times.Created < 0 {
		return 0, nil, ErrInvalidSnapshot
	}
	return *times.Created, times.Completed, nil
}

func validTokenUsage(raw json.RawMessage) bool {
	var tokens map[string]json.RawMessage
	if json.Unmarshal(raw, &tokens) != nil {
		return false
	}
	for _, name := range []string{"input", "output", "reasoning"} {
		if !nonnegativeNumber(tokens[name]) {
			return false
		}
	}
	var cache map[string]json.RawMessage
	return json.Unmarshal(tokens["cache"], &cache) == nil &&
		nonnegativeNumber(cache["read"]) && nonnegativeNumber(cache["write"])
}

func nonnegativeNumber(raw json.RawMessage) bool {
	var value float64
	return len(raw) != 0 && !bytes.Equal(raw, []byte("null")) && json.Unmarshal(raw, &value) == nil &&
		value >= 0 && !math.IsInf(value, 0)
}

func stringField(data map[string]json.RawMessage, name string) (string, bool) {
	var value string
	raw, exists := data[name]
	if !exists || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}

func nonemptyJSON(raw json.RawMessage) bool {
	text := string(bytes.TrimSpace(raw))
	return text != "" && text != "null" && text != `""` && text != "[]" && text != "{}"
}

func hasOnlyFields(data map[string]json.RawMessage, fields string) bool {
	allowed := strings.Fields(fields)
	for key := range data {
		if !slices.Contains(allowed, key) {
			return false
		}
	}
	return true
}

func nativeObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(raw) > MaxRowBytes {
		return nil, ErrSizeLimit
	}
	if err := validateJSON(raw); err != nil {
		return nil, err
	}
	var value map[string]json.RawMessage
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return nil, ErrInvalidSnapshot
	}
	return value, nil
}

// Reject duplicate keys and excessive nesting before preserving native JSON.
// This avoids parser-dependent model, session, and tool-state interpretations.
func validateJSON(raw []byte) error {
	if !utf8.Valid(raw) {
		return ErrInvalidSnapshot
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := readJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ErrInvalidSnapshot
	}
	return nil
}

func readJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return ErrInvalidSnapshot
	}
	token, err := decoder.Token()
	if err != nil {
		return ErrInvalidSnapshot
	}
	delimiter, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	if delimiter != '{' && delimiter != '[' {
		return ErrInvalidSnapshot
	}
	keys := make(map[string]bool)
	for decoder.More() {
		if delimiter == '{' {
			key, err := decoder.Token()
			name, ok := key.(string)
			if err != nil || !ok || keys[name] {
				return ErrInvalidSnapshot
			}
			keys[name] = true
		}
		if err := readJSONValue(decoder, depth+1); err != nil {
			return err
		}
	}
	closing, err := decoder.Token()
	if err != nil || delimiter == '{' && closing != json.Delim('}') || delimiter == '[' && closing != json.Delim(']') {
		return ErrInvalidSnapshot
	}
	return nil
}
