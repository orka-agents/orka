/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/workerenv"
)

const (
	internalMemoryToolBodyLimit          = 1 << 20 // 1MB
	internalMemoryToolResultLimit        = 1 << 20 // 1MB
	internalMemoryTransactionTokenHeader = "Txn-Token"
	defaultRecallMemoryToolLimit         = 100
	maxRecallMemoryToolLimit             = 200
	maxRecallMemoryToolPageLimit         = 2
)

// RecallMemoryTool retrieves durable namespace-scoped memories relevant to the current task.
type RecallMemoryTool struct{}

// NewRecallMemoryTool creates a new recall_memory tool.
func NewRecallMemoryTool() *RecallMemoryTool { return &RecallMemoryTool{} }

// Name returns the tool name.
func (t *RecallMemoryTool) Name() string { return "recall_memory" }

// Description returns the tool description.
func (t *RecallMemoryTool) Description() string {
	return "Recall durable namespace-scoped memories that may help with the current task. " +
		"Use query and/or tags to find project facts, prior decisions, reusable context, and notes saved from previous work."
}

// Parameters returns the JSON Schema for the tool parameters.
func (t *RecallMemoryTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {
				"type": "string",
				"description": "Keyword or phrase to search for in memory content"
			},
			"tags": {
				"type": "array",
				"items": {"type": "string"},
				"description": "Optional memory tags to match"
			},
			"task_name": {
				"type": "string",
				"description": "Optional task name provenance filter"
			},
			"agent_name": {
				"type": "string",
				"description": "Optional agent name provenance filter"
			},
			"source": {
				"type": "string",
				"description": "Optional source filter such as task, session, user, or system"
			},
				"limit": {
					"type": "integer",
					"minimum": 1,
					"maximum": 200,
					"description": "Maximum number of memories to return. Controller defaults apply when omitted."
			},
			"include_disabled": {
				"type": "boolean",
				"description": "Include memories disabled for normal recall"
			}
		}
	}`)
}

type recallMemoryArgs struct {
	Query                string               `json:"query,omitempty"`
	Tags                 memoryToolStringList `json:"tags,omitempty"`
	TaskName             string               `json:"task_name,omitempty"`
	TaskNameCamel        string               `json:"taskName,omitempty"`
	AgentName            string               `json:"agent_name,omitempty"`
	AgentNameCamel       string               `json:"agentName,omitempty"`
	Source               string               `json:"source,omitempty"`
	Limit                *int                 `json:"limit,omitempty"`
	IncludeDisabled      bool                 `json:"include_disabled,omitempty"`
	IncludeDisabledCamel bool                 `json:"includeDisabled,omitempty"`
}

// Execute recalls matching memories through the controller's strict internal search API.
func (t *RecallMemoryTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a recallMemoryArgs
	if err := decodeMemoryToolArgs(args, &a); err != nil {
		return "", fmt.Errorf("failed to parse arguments: %w", err)
	}
	if a.Limit != nil && (*a.Limit <= 0 || *a.Limit > maxRecallMemoryToolLimit) {
		return "", fmt.Errorf("limit must be between 1 and %d", maxRecallMemoryToolLimit)
	}
	targetLimit := defaultRecallMemoryToolLimit
	if a.Limit != nil {
		targetLimit = *a.Limit
	}
	cfg, err := loadInternalControllerConfig()
	if err != nil {
		return "", err
	}
	request := map[string]any{
		"query":           a.Query,
		"tags":            []string(a.Tags),
		"taskName":        firstNonEmpty(a.TaskName, a.TaskNameCamel),
		"agentName":       firstNonEmpty(a.AgentName, a.AgentNameCamel),
		"sources":         compactMemoryToolStrings([]string{a.Source}),
		"trust":           []string{"reviewed", "trusted"},
		"includeDisabled": a.IncludeDisabled || a.IncludeDisabledCamel,
		"mode":            "keyword",
	}
	endpoint := cfg.url("/internal/v1/memories/"+url.PathEscape(cfg.Namespace)+"/search", nil)
	memories := make([]json.RawMessage, 0, targetLimit)
	seenCursors := make(map[string]struct{})
	cursor := ""
	resultBudgetReached := false
	for len(memories) < targetLimit {
		pageLimit := min(maxRecallMemoryToolPageLimit, targetLimit-len(memories))
		request["limit"] = pageLimit
		if cursor == "" {
			delete(request, "cursor")
		} else {
			request["cursor"] = cursor
		}
		payload, err := json.Marshal(request)
		if err != nil {
			return "", fmt.Errorf("failed to encode memory search: %w", err)
		}
		transactionToken, err := loadTaskTransactionToken()
		if err != nil {
			return "", fmt.Errorf("failed to load task transaction token: %w", err)
		}
		body, err := doInternalControllerRequestWithTransactionToken(
			ctx, cfg, http.MethodPost, endpoint, payload, transactionToken,
		)
		if err != nil {
			return "", fmt.Errorf("failed to recall memory: %w", err)
		}
		var response struct {
			Items []struct {
				Memory json.RawMessage `json:"memory"`
			} `json:"items"`
			Cursor    string `json:"cursor,omitempty"`
			Exhausted bool   `json:"exhausted"`
			Complete  bool   `json:"complete"`
		}
		if err := json.Unmarshal([]byte(body), &response); err != nil {
			return "", fmt.Errorf("failed to decode memory search response: %w", err)
		}
		if !response.Complete {
			return "", fmt.Errorf("memory search was incomplete; use a narrower query")
		}
		if len(response.Items) > pageLimit {
			return "", fmt.Errorf("memory search returned %d items for page limit %d", len(response.Items), pageLimit)
		}
		for _, item := range response.Items {
			if len(item.Memory) > 0 {
				candidate := make([]json.RawMessage, len(memories)+1)
				copy(candidate, memories)
				candidate[len(memories)] = item.Memory
				encodedCandidate, err := json.Marshal(candidate)
				if err != nil {
					return "", fmt.Errorf("failed to encode recalled memories: %w", err)
				}
				if len(encodedCandidate) > internalMemoryToolResultLimit {
					resultBudgetReached = true
					break
				}
				memories = candidate
			}
		}
		if resultBudgetReached || len(memories) >= targetLimit || response.Exhausted {
			break
		}
		nextCursor := strings.TrimSpace(response.Cursor)
		if nextCursor == "" {
			return "", fmt.Errorf("memory search continuation cursor was missing")
		}
		if _, duplicate := seenCursors[nextCursor]; duplicate {
			return "", fmt.Errorf("memory search continuation cursor did not advance")
		}
		seenCursors[nextCursor] = struct{}{}
		cursor = nextCursor
	}
	encoded, err := json.Marshal(memories)
	if err != nil {
		return "", fmt.Errorf("failed to encode recalled memories: %w", err)
	}
	return string(encoded), nil
}

func compactMemoryToolStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}

// ProposeMemoryTool submits memory-adjacent governance proposals for coordinator review.
type ProposeMemoryTool struct{}

// NewProposeMemoryTool creates a new propose_memory tool.
func NewProposeMemoryTool() *ProposeMemoryTool { return &ProposeMemoryTool{} }

// Name returns the tool name.
func (t *ProposeMemoryTool) Name() string { return "propose_memory" }

// Description returns the tool description.
func (t *ProposeMemoryTool) Description() string {
	return "Propose durable memory or reusable skill changes for coordinator review. " +
		"Use this when you discover information that should persist beyond the current task but should be reviewed before becoming shared memory. " +
		"Memory proposal descriptions may include an optional `Tags: tag-a, tag-b` line to suggest durable memory tags."
}

// Parameters returns the JSON Schema for the tool parameters.
func (t *ProposeMemoryTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"type": {
				"type": "string",
				"description": "Proposal type, for example skill, memory, policy, or workflow. Defaults to skill."
			},
			"skill_name": {
				"type": "string",
				"description": "Optional reusable skill or workflow name affected by this proposal"
			},
			"title": {
				"type": "string",
				"description": "Short title describing the proposed memory or skill change"
			},
			"description": {
				"type": "string",
				"description": "Why this should be remembered or changed; memory proposals may include an optional Tags: tag-a, tag-b line"
			},
			"content": {
				"type": "string",
				"description": "Proposed memory content or full proposed skill content"
			},
			"patch": {
				"type": "string",
				"description": "Optional patch/diff when proposing a change to an existing skill or memory"
			},
			"agent_name": {
				"type": "string",
				"description": "Optional agent name override; ORKA_AGENT_NAME is used by default when present"
			}
		},
		"required": ["title"]
	}`)
}

type proposeMemoryArgs struct {
	Type           string `json:"type,omitempty"`
	SkillName      string `json:"skill_name,omitempty"`
	SkillNameCamel string `json:"skillName,omitempty"`
	Title          string `json:"title,omitempty"`
	Description    string `json:"description,omitempty"`
	Content        string `json:"content,omitempty"`
	Patch          string `json:"patch,omitempty"`
	AgentName      string `json:"agent_name,omitempty"`
	AgentNameCamel string `json:"agentName,omitempty"`
}

type proposeMemoryPayload struct {
	Namespace   string `json:"namespace"`
	TaskName    string `json:"taskName"`
	AgentName   string `json:"agentName,omitempty"`
	Type        string `json:"type"`
	SkillName   string `json:"skillName,omitempty"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Content     string `json:"content,omitempty"`
	Patch       string `json:"patch,omitempty"`
}

// Execute submits a memory proposal through the controller's internal API.
func (t *ProposeMemoryTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a proposeMemoryArgs
	if err := decodeMemoryToolArgs(args, &a); err != nil {
		return "", fmt.Errorf("failed to parse arguments: %w", err)
	}

	title := strings.TrimSpace(a.Title)
	if title == "" {
		return "", fmt.Errorf("title is required")
	}

	cfg, err := loadInternalControllerConfig()
	if err != nil {
		return "", err
	}

	proposalType := strings.TrimSpace(a.Type)
	if proposalType == "" {
		proposalType = "skill"
	}
	agentName := firstNonEmpty(a.AgentName, a.AgentNameCamel, cfg.AgentName)

	payload, err := json.Marshal(proposeMemoryPayload{
		Namespace:   cfg.Namespace,
		TaskName:    cfg.TaskName,
		AgentName:   agentName,
		Type:        proposalType,
		SkillName:   firstNonEmpty(a.SkillName, a.SkillNameCamel),
		Title:       title,
		Description: strings.TrimSpace(a.Description),
		Content:     strings.TrimSpace(a.Content),
		Patch:       strings.TrimSpace(a.Patch),
	})
	if err != nil {
		return "", fmt.Errorf("failed to marshal memory proposal: %w", err)
	}
	transactionToken, err := loadTaskTransactionToken()
	if err != nil {
		return "", fmt.Errorf("failed to load task transaction token: %w", err)
	}

	endpoint := cfg.url("/internal/v1/memory-proposals/"+url.PathEscape(cfg.Namespace), nil)
	body, err := doInternalControllerRequestWithTransactionToken(
		ctx, cfg, http.MethodPost, endpoint, payload, transactionToken,
	)
	if err != nil {
		return "", fmt.Errorf("failed to propose memory: %w", err)
	}
	return body, nil
}

// RememberMemoryTool submits durable memory proposals for coordinator review.
type RememberMemoryTool struct{}

// NewRememberMemoryTool creates a new remember tool.
func NewRememberMemoryTool() *RememberMemoryTool { return &RememberMemoryTool{} }

// Name returns the tool name.
func (t *RememberMemoryTool) Name() string { return "remember" }

// Description returns the tool description.
func (t *RememberMemoryTool) Description() string {
	return "Submit a durable memory proposal for coordinator review. " +
		"Use this to preserve durable project facts, conventions, or lessons learned; proposals are reviewed and are not written directly to memory."
}

// Parameters returns the JSON Schema for the tool parameters.
func (t *RememberMemoryTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"title": {
				"type": "string",
				"description": "Optional short title for the memory proposal. A title is derived from content when omitted."
			},
			"description": {
				"type": "string",
				"description": "Optional reason or context explaining why this should be remembered"
			},
			"content": {
				"type": "string",
				"description": "Durable memory content to propose for future tasks"
			},
			"tags": {
				"type": "array",
				"items": {"type": "string"},
				"description": "Optional tags for reviewer context"
			},
			"agent_name": {
				"type": "string",
				"description": "Optional agent name override; ORKA_AGENT_NAME is used by default when present"
			},
			"agentName": {
				"type": "string",
				"description": "Camel-case alias for agent_name"
			}
		},
		"required": ["content"]
	}`)
}

type rememberMemoryArgs struct {
	Title          string               `json:"title,omitempty"`
	Description    string               `json:"description,omitempty"`
	Content        string               `json:"content,omitempty"`
	Tags           memoryToolStringList `json:"tags,omitempty"`
	AgentName      string               `json:"agent_name,omitempty"`
	AgentNameCamel string               `json:"agentName,omitempty"`
}

// Execute submits a durable memory proposal through the controller's internal API.
func (t *RememberMemoryTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a rememberMemoryArgs
	if err := decodeMemoryToolArgs(args, &a); err != nil {
		return "", fmt.Errorf("failed to parse arguments: %w", err)
	}

	content := strings.TrimSpace(a.Content)
	if content == "" {
		return "", fmt.Errorf("content is required")
	}

	cfg, err := loadInternalControllerConfig()
	if err != nil {
		return "", err
	}

	title := strings.TrimSpace(a.Title)
	if title == "" {
		title = deriveRememberTitle(content)
	}
	agentName := firstNonEmpty(a.AgentName, a.AgentNameCamel, cfg.AgentName)
	description := appendRememberTags(strings.TrimSpace(a.Description), a.Tags)

	payload, err := json.Marshal(proposeMemoryPayload{
		Namespace:   cfg.Namespace,
		TaskName:    cfg.TaskName,
		AgentName:   agentName,
		Type:        "memory",
		Title:       title,
		Description: description,
		Content:     content,
	})
	if err != nil {
		return "", fmt.Errorf("failed to marshal memory proposal: %w", err)
	}
	transactionToken, err := loadTaskTransactionToken()
	if err != nil {
		return "", fmt.Errorf("failed to load task transaction token: %w", err)
	}

	endpoint := cfg.url("/internal/v1/memory-proposals/"+url.PathEscape(cfg.Namespace), nil)
	body, err := doInternalControllerRequestWithTransactionToken(
		ctx, cfg, http.MethodPost, endpoint, payload, transactionToken,
	)
	if err != nil {
		return "", fmt.Errorf("failed to remember: %w", err)
	}
	return body, nil
}

func deriveRememberTitle(content string) string {
	for line := range strings.SplitSeq(content, "\n") {
		title := strings.Join(strings.Fields(line), " ")
		if title == "" {
			continue
		}
		const maxRunes = 80
		runes := []rune(title)
		if len(runes) > maxRunes {
			return string(runes[:maxRunes])
		}
		return title
	}
	return "Memory"
}

func appendRememberTags(description string, tags memoryToolStringList) string {
	if len(tags) == 0 {
		return description
	}
	tagLine := "Tags: " + strings.Join(tags, ", ")
	if description == "" {
		return tagLine
	}
	return description + "\n\n" + tagLine
}

// SearchTranscriptTool searches prior session transcripts in the current namespace.
type SearchTranscriptTool struct{}

// NewSearchTranscriptTool creates a new search_transcript tool.
func NewSearchTranscriptTool() *SearchTranscriptTool { return &SearchTranscriptTool{} }

// Name returns the tool name.
func (t *SearchTranscriptTool) Name() string { return "search_transcript" }

// Description returns the tool description.
func (t *SearchTranscriptTool) Description() string {
	return "Search prior session transcripts in this namespace and return compact snippets. " +
		"Use this to recover context from previous tasks without reading the current task transcript."
}

// Parameters returns the JSON Schema for the tool parameters.
func (t *SearchTranscriptTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {
				"type": "string",
				"description": "Keyword or phrase to search for in transcript content"
			},
			"session_name": {
				"type": "string",
				"description": "Optional session name to search within"
			},
			"exclude_session_name": {
				"type": "string",
				"description": "Optional session name to exclude. Defaults to the current ORKA_TASK_NAME."
			},
			"roles": {
				"type": "array",
				"items": {"type": "string"},
				"description": "Optional message roles to search, such as user, assistant, or tool"
			},
			"limit": {
				"type": "integer",
				"minimum": 0,
				"description": "Maximum number of matches to return. Controller defaults apply when omitted or 0."
			},
			"max_snippet_length": {
				"type": "integer",
				"minimum": 0,
				"description": "Maximum snippet length. Controller defaults apply when omitted or 0."
			}
		},
		"required": ["query"]
	}`)
}

type searchTranscriptArgs struct {
	Query                   string               `json:"query,omitempty"`
	SessionName             string               `json:"session_name,omitempty"`
	SessionNameCamel        string               `json:"sessionName,omitempty"`
	ExcludeSessionName      string               `json:"exclude_session_name,omitempty"`
	ExcludeSessionNameCamel string               `json:"excludeSessionName,omitempty"`
	Roles                   memoryToolStringList `json:"roles,omitempty"`
	Limit                   int                  `json:"limit,omitempty"`
	MaxSnippetLength        int                  `json:"max_snippet_length,omitempty"`
	MaxSnippetLengthCamel   int                  `json:"maxSnippetLength,omitempty"`
}

// Execute searches transcript snippets through the controller's internal API.
func (t *SearchTranscriptTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var a searchTranscriptArgs
	if err := decodeMemoryToolArgs(args, &a); err != nil {
		return "", fmt.Errorf("failed to parse arguments: %w", err)
	}

	query := strings.TrimSpace(a.Query)
	if query == "" {
		return "", fmt.Errorf("query is required")
	}
	if a.Limit < 0 {
		return "", fmt.Errorf("limit must be non-negative")
	}
	maxSnippetLength := a.MaxSnippetLength
	if maxSnippetLength == 0 {
		maxSnippetLength = a.MaxSnippetLengthCamel
	}
	if maxSnippetLength < 0 {
		return "", fmt.Errorf("max_snippet_length must be non-negative")
	}

	cfg, err := loadInternalControllerConfig()
	if err != nil {
		return "", err
	}

	excludeSessionName := firstNonEmpty(a.ExcludeSessionName, a.ExcludeSessionNameCamel)
	if excludeSessionName == "" {
		excludeSessionName = cfg.TaskName
	}

	values := url.Values{}
	values.Set("query", query)
	addQueryValue(values, "sessionName", firstNonEmpty(a.SessionName, a.SessionNameCamel))
	addQueryValue(values, "excludeSessionName", excludeSessionName)
	if len(a.Roles) > 0 {
		values.Set("roles", strings.Join(a.Roles, ","))
	}
	if a.Limit > 0 {
		values.Set("limit", fmt.Sprintf("%d", a.Limit))
	}
	if maxSnippetLength > 0 {
		values.Set("maxSnippetLength", fmt.Sprintf("%d", maxSnippetLength))
	}

	endpoint := cfg.url("/internal/v1/sessions/"+url.PathEscape(cfg.Namespace)+"/search", values)
	body, err := doInternalControllerRequest(ctx, cfg, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("failed to search transcript: %w", err)
	}
	return body, nil
}

type memoryToolStringList []string

func (l *memoryToolStringList) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*l = nil
		return nil
	}
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var raw string
		if err := json.Unmarshal(trimmed, &raw); err != nil {
			return err
		}
		parts := strings.Split(raw, ",")
		out := make([]string, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part != "" {
				out = append(out, part)
			}
		}
		*l = out
		return nil
	}
	var values []string
	if err := json.Unmarshal(trimmed, &values); err != nil {
		return err
	}
	out := values[:0]
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	*l = out
	return nil
}

type internalControllerConfig struct {
	ControllerURL string
	Namespace     string
	TaskName      string
	AgentName     string
	Token         string
}

func loadInternalControllerConfig() (internalControllerConfig, error) {
	cfg := internalControllerConfig{
		ControllerURL: strings.TrimRight(strings.TrimSpace(os.Getenv(workerenv.ControllerURL)), "/"),
		Namespace:     strings.TrimSpace(os.Getenv(workerenv.TaskNamespace)),
		TaskName:      strings.TrimSpace(os.Getenv(workerenv.TaskName)),
		AgentName:     strings.TrimSpace(os.Getenv(workerenv.AgentName)),
		Token:         strings.TrimSpace(os.Getenv(workerenv.ServiceAccountToken)),
	}
	if cfg.ControllerURL == "" || cfg.Namespace == "" || cfg.TaskName == "" {
		return cfg, fmt.Errorf("%s, %s, and %s are required", workerenv.ControllerURL, workerenv.TaskName, workerenv.TaskNamespace)
	}
	if cfg.Token == "" {
		if data, err := os.ReadFile(saTokenPath); err == nil {
			cfg.Token = strings.TrimSpace(string(data))
		}
	}
	return cfg, nil
}

func (c internalControllerConfig) url(path string, values url.Values) string {
	endpoint := c.ControllerURL + path
	if len(values) > 0 {
		endpoint += "?" + values.Encode()
	}
	return endpoint
}

func loadTaskTransactionToken() (string, error) {
	token, ok, err := workerenv.ReadTokenFileEnv(workerenv.TransactionTokenFile, "task transaction token")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	return token, nil
}

func doInternalControllerRequest(ctx context.Context, cfg internalControllerConfig, method, endpoint string, payload []byte) (string, error) {
	return doInternalControllerRequestWithTransactionToken(ctx, cfg, method, endpoint, payload, "")
}

func doInternalControllerRequestWithTransactionToken(
	ctx context.Context,
	cfg internalControllerConfig,
	method, endpoint string,
	payload []byte,
	transactionToken string,
) (string, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	if transactionToken = strings.TrimSpace(transactionToken); transactionToken != "" {
		req.Header.Set(internalMemoryTransactionTokenHeader, transactionToken)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, internalMemoryToolBodyLimit+1))
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}
	if len(respBody) > internalMemoryToolBodyLimit {
		return "", fmt.Errorf("controller response exceeded %d bytes", internalMemoryToolBodyLimit)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return string(respBody), nil
}

func decodeMemoryToolArgs(args json.RawMessage, dst any) error {
	trimmed := bytes.TrimSpace(args)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	return json.Unmarshal(trimmed, dst)
}

func addQueryValue(values url.Values, key, value string) {
	if value = strings.TrimSpace(value); value != "" {
		values.Set(key, value)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

var _ Tool = (*RecallMemoryTool)(nil)
var _ Tool = (*RememberMemoryTool)(nil)
var _ Tool = (*ProposeMemoryTool)(nil)
var _ Tool = (*SearchTranscriptTool)(nil)
