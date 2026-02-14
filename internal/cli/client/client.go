/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Client is an HTTP client for the Orka API.
type Client struct {
	BaseURL    string
	Token      string
	Namespace  string
	HTTPClient *http.Client
}

// New creates a new Orka API client.
func New(baseURL, token string) *Client {
	return &Client{
		BaseURL:    baseURL,
		Token:      token,
		HTTPClient: http.DefaultClient,
	}
}

// NewWithNamespace creates a new Orka API client with a default namespace.
func NewWithNamespace(baseURL, token, namespace string) *Client {
	return &Client{
		BaseURL:    baseURL,
		Token:      token,
		Namespace:  namespace,
		HTTPClient: http.DefaultClient,
	}
}

// ListOptions contains options for list operations.
type ListOptions struct {
	Namespace string
}

// GetOptions contains options for get operations.
type GetOptions struct {
	Namespace string
}

// AgentSummary is a lightweight representation of an agent for list display.
type AgentSummary struct {
	Name    string `json:"name"`
	Model   string `json:"model,omitempty"`
	Runtime string `json:"runtime,omitempty"`
	Active  int    `json:"activeTasks"`
}

// AgentDetail is the full agent object returned by the API.
type AgentDetail map[string]any

// agentListResponse matches the API ListResponse shape.
type agentListResponse struct {
	Items    []AgentDetail `json:"items"`
	Metadata struct {
		Continue           string `json:"continue,omitempty"`
		RemainingItemCount *int64 `json:"remainingItemCount,omitempty"`
	} `json:"metadata"`
}

// ListAgents returns all agents from the API.
func (c *Client) ListAgents(ctx context.Context, opts ListOptions) ([]AgentSummary, error) {
	u, err := url.Parse(c.BaseURL + "/api/v1/agents")
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}
	q := u.Query()
	if opts.Namespace != "" {
		q.Set("namespace", opts.Namespace)
	}
	u.RawQuery = q.Encode()

	body, err := c.doGet(ctx, u.String())
	if err != nil {
		return nil, err
	}

	var resp agentListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	summaries := make([]AgentSummary, 0, len(resp.Items))
	for _, item := range resp.Items {
		summaries = append(summaries, extractAgentSummary(item))
	}
	return summaries, nil
}

// GetAgent returns full details for a single agent.
func (c *Client) GetAgent(ctx context.Context, name string, opts GetOptions) (*AgentDetail, error) {
	u, err := url.Parse(c.BaseURL + "/api/v1/agents/" + url.PathEscape(name))
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}
	q := u.Query()
	if opts.Namespace != "" {
		q.Set("namespace", opts.Namespace)
	}
	u.RawQuery = q.Encode()

	body, err := c.doGet(ctx, u.String())
	if err != nil {
		return nil, err
	}

	var detail AgentDetail
	if err := json.Unmarshal(body, &detail); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	return &detail, nil
}

// StreamChat sends a chat message and returns an SSE reader for streaming the response.
func (c *Client) StreamChat(ctx context.Context, req ChatRequest) (*SSEReader, *http.Response, error) {
	if req.Namespace == "" && c.Namespace != "" {
		req.Namespace = c.Namespace
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/v1/chat", bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.Token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, nil, fmt.Errorf("send request: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		_ = resp.Body.Close()
		return nil, nil, fmt.Errorf("authentication failed (HTTP 401): try 'orka login' or provide --token")
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, nil, fmt.Errorf("server error (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	return NewSSEReader(resp.Body), resp, nil
}

// GetChatConfig fetches the chat configuration from the server.
func (c *Client) GetChatConfig(ctx context.Context) (*ChatConfigResponse, error) {
	body, err := c.doGet(ctx, c.BaseURL+"/api/v1/chat/config")
	if err != nil {
		return nil, err
	}
	var cfg ChatConfigResponse
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("failed to decode chat config: %w", err)
	}
	return &cfg, nil
}

func (c *Client) doGet(ctx context.Context, reqURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error (HTTP %d): %s", resp.StatusCode, string(body))
	}

	return body, nil
}

// HealthCheck calls GET /healthz and returns true if healthy.
func (c *Client) HealthCheck(ctx context.Context) (bool, error) {
	body, err := c.doGet(ctx, c.BaseURL+"/healthz")
	if err != nil {
		return false, err
	}
	var resp map[string]string
	if err := json.Unmarshal(body, &resp); err != nil {
		return false, nil
	}
	return resp["status"] == "ok", nil
}

// ReadyCheck calls GET /readyz and returns true if ready.
func (c *Client) ReadyCheck(ctx context.Context) (bool, error) {
	body, err := c.doGet(ctx, c.BaseURL+"/readyz")
	if err != nil {
		return false, err
	}
	var resp map[string]string
	if err := json.Unmarshal(body, &resp); err != nil {
		return false, nil
	}
	return resp["status"] == "ok", nil
}

// extractAgentSummary pulls summary fields from the raw agent JSON.
func extractAgentSummary(item AgentDetail) AgentSummary {
	s := AgentSummary{
		Name: StringField(item, "metadata", "name"),
	}

	spec, _ := item["spec"].(map[string]any)
	if spec != nil {
		if model, ok := spec["model"].(map[string]any); ok {
			s.Model = stringVal(model["name"])
		}
		if rt, ok := spec["runtime"].(map[string]any); ok {
			s.Runtime = stringVal(rt["type"])
		}
	}

	status, _ := item["status"].(map[string]any)
	if status != nil {
		if v, ok := status["activeTasks"].(float64); ok {
			s.Active = int(v)
		}
	}

	return s
}

// StringField extracts a nested string value from a map.
func StringField(m map[string]any, keys ...string) string {
	current := m
	for i, k := range keys {
		if i == len(keys)-1 {
			return stringVal(current[k])
		}
		next, ok := current[k].(map[string]any)
		if !ok {
			return ""
		}
		current = next
	}
	return ""
}

func stringVal(v any) string {
	s, _ := v.(string)
	return s
}

// CreateTaskRequest is the request body for creating a task.
type CreateTaskRequest struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Type      string `json:"type"`
	Prompt    string `json:"prompt,omitempty"`
	Timeout   string `json:"timeout,omitempty"`
	AgentRef  *struct {
		Name string `json:"name"`
	} `json:"agentRef,omitempty"`
	AI *struct {
		ProviderRef *struct {
			Name string `json:"name"`
		} `json:"providerRef,omitempty"`
		Prompt string `json:"prompt,omitempty"`
	} `json:"ai,omitempty"`
}

// TaskDetail is the full task object returned by the API.
type TaskDetail map[string]any

// TaskSummary is a lightweight representation of a task for list display.
type TaskSummary struct {
	Name      string
	Namespace string
	Type      string
	Phase     string
	Age       string
	Iteration int
}

// taskListResponse matches the API ListResponse shape.
type taskListResponse struct {
	Items    []TaskDetail `json:"items"`
	Metadata struct {
		Continue           string `json:"continue,omitempty"`
		RemainingItemCount *int64 `json:"remainingItemCount,omitempty"`
	} `json:"metadata"`
}

// ListTasksOptions contains options for listing tasks.
type ListTasksOptions struct {
	Namespace string
	Limit     int
	Continue  string
}

// TaskLogsResponse is the response for getting task logs.
type TaskLogsResponse struct {
	Logs    string `json:"logs,omitempty"`
	Message string `json:"message,omitempty"`
	JobName string `json:"jobName,omitempty"`
}

// TaskResultResponse is the response for getting task results.
type TaskResultResponse struct {
	Result string `json:"result"`
}

// CreateTask creates a new task.
func (c *Client) CreateTask(ctx context.Context, req CreateTaskRequest) (*TaskDetail, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/v1/tasks", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("API error (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	var detail TaskDetail
	if err := json.Unmarshal(respBody, &detail); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	return &detail, nil
}

// ListTasks returns tasks from the API.
func (c *Client) ListTasks(ctx context.Context, opts ListTasksOptions) ([]TaskSummary, error) {
	u, err := url.Parse(c.BaseURL + "/api/v1/tasks")
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}
	q := u.Query()
	if opts.Namespace != "" {
		q.Set("namespace", opts.Namespace)
	}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Continue != "" {
		q.Set("continue", opts.Continue)
	}
	u.RawQuery = q.Encode()

	body, err := c.doGet(ctx, u.String())
	if err != nil {
		return nil, err
	}

	var resp taskListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	summaries := make([]TaskSummary, 0, len(resp.Items))
	for _, item := range resp.Items {
		summaries = append(summaries, extractTaskSummary(item))
	}
	return summaries, nil
}

// GetTask returns full details for a single task.
func (c *Client) GetTask(ctx context.Context, name string, opts GetOptions) (*TaskDetail, error) {
	u, err := url.Parse(c.BaseURL + "/api/v1/tasks/" + url.PathEscape(name))
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}
	q := u.Query()
	if opts.Namespace != "" {
		q.Set("namespace", opts.Namespace)
	}
	u.RawQuery = q.Encode()

	body, err := c.doGet(ctx, u.String())
	if err != nil {
		return nil, err
	}

	var detail TaskDetail
	if err := json.Unmarshal(body, &detail); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	return &detail, nil
}

// DeleteTask deletes a task by name.
func (c *Client) DeleteTask(ctx context.Context, name string, opts GetOptions) error {
	u, err := url.Parse(c.BaseURL + "/api/v1/tasks/" + url.PathEscape(name))
	if err != nil {
		return fmt.Errorf("invalid base URL: %w", err)
	}
	q := u.Query()
	if opts.Namespace != "" {
		q.Set("namespace", opts.Namespace)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u.String(), nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error (HTTP %d): %s", resp.StatusCode, string(body))
	}
	return nil
}

// DeleteAgent deletes an agent by name.
func (c *Client) DeleteAgent(ctx context.Context, name string, opts GetOptions) error {
	u, err := url.Parse(c.BaseURL + "/api/v1/agents/" + url.PathEscape(name))
	if err != nil {
		return fmt.Errorf("invalid base URL: %w", err)
	}
	q := u.Query()
	if opts.Namespace != "" {
		q.Set("namespace", opts.Namespace)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u.String(), nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error (HTTP %d): %s", resp.StatusCode, string(body))
	}
	return nil
}

// GetTaskLogs gets logs for a task.
func (c *Client) GetTaskLogs(ctx context.Context, name string, opts GetOptions) (*TaskLogsResponse, error) {
	u, err := url.Parse(c.BaseURL + "/api/v1/tasks/" + url.PathEscape(name) + "/logs")
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}
	q := u.Query()
	if opts.Namespace != "" {
		q.Set("namespace", opts.Namespace)
	}
	u.RawQuery = q.Encode()

	body, err := c.doGet(ctx, u.String())
	if err != nil {
		return nil, err
	}

	var result TaskLogsResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	return &result, nil
}

// StreamLogsOptions contains options for streaming task logs.
type StreamLogsOptions struct {
	Namespace string
	Writer    io.Writer
}

// StreamTaskLogs streams live logs for a running task via SSE.
func (c *Client) StreamTaskLogs(ctx context.Context, name string, opts StreamLogsOptions) error {
	u, err := url.Parse(c.BaseURL + "/api/v1/tasks/" + url.PathEscape(name) + "/logs")
	if err != nil {
		return fmt.Errorf("invalid base URL: %w", err)
	}
	q := u.Query()
	q.Set("follow", "true")
	if opts.Namespace != "" {
		q.Set("namespace", opts.Namespace)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error (HTTP %d): %s", resp.StatusCode, string(body))
	}

	w := opts.Writer
	if w == nil {
		w = io.Discard
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if after, ok := strings.CutPrefix(line, "data: "); ok {
			fmt.Fprintln(w, after) //nolint:errcheck
		}
	}
	return scanner.Err()
}

// GetTaskResult gets the result of a task.
func (c *Client) GetTaskResult(ctx context.Context, name string, opts GetOptions) (*TaskResultResponse, error) {
	u, err := url.Parse(c.BaseURL + "/api/v1/tasks/" + url.PathEscape(name) + "/result")
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}
	q := u.Query()
	if opts.Namespace != "" {
		q.Set("namespace", opts.Namespace)
	}
	u.RawQuery = q.Encode()

	body, err := c.doGet(ctx, u.String())
	if err != nil {
		return nil, err
	}

	var result TaskResultResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	return &result, nil
}

// extractTaskSummary pulls summary fields from the raw task JSON.
func extractTaskSummary(item TaskDetail) TaskSummary {
	s := TaskSummary{
		Name:      StringField(item, "metadata", "name"),
		Namespace: StringField(item, "metadata", "namespace"),
		Type:      StringField(item, "spec", "type"),
		Phase:     StringField(item, "status", "phase"),
		Age:       StringField(item, "metadata", "creationTimestamp"),
	}
	if status, ok := item["status"].(map[string]any); ok {
		if v, ok := status["iteration"].(float64); ok {
			s.Iteration = int(v)
		}
	}
	return s
}
