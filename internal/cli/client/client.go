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

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

type Client struct {
	BaseURL               string
	Token                 string
	TxnToken              string
	Namespace             string
	HTTPClient            *http.Client
	NativeSessionMaxBytes int
}

func NewWithNamespace(baseURL, token, namespace string) *Client {
	return &Client{
		BaseURL:    baseURL,
		Token:      token,
		Namespace:  namespace,
		HTTPClient: http.DefaultClient,
	}
}

type ListOptions struct {
	Namespace string
}

type GetOptions struct {
	Namespace string
}

type AgentSummary struct {
	Name     string `json:"name"`
	Model    string `json:"model,omitempty"`
	Runtime  string `json:"runtime,omitempty"`
	Identity string `json:"identity,omitempty"`
	Ready    bool   `json:"ready"`
	Active   int    `json:"activeTasks"`
}

type AgentDetail map[string]any

type agentListResponse struct {
	Items    []AgentDetail `json:"items"`
	Metadata struct {
		Continue           string `json:"continue,omitempty"`
		RemainingItemCount *int64 `json:"remainingItemCount,omitempty"`
	} `json:"metadata"`
}

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
	if c.TxnToken != "" {
		httpReq.Header.Set("Txn-Token", c.TxnToken)
	}

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, nil, fmt.Errorf("send request: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		_ = resp.Body.Close()
		return nil, nil, fmt.Errorf("authentication failed (HTTP 401): provide --token, save one with 'orka config set-token', or check your kubeconfig credentials")
	}

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, nil, fmt.Errorf("server error (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	return NewSSEReader(resp.Body), resp, nil
}

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
	body, _, err := c.doRaw(ctx, http.MethodGet, reqURL, nil)
	return body, err
}

func (c *Client) newRequest(ctx context.Context, method, reqURL string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.TxnToken != "" {
		req.Header.Set("Txn-Token", c.TxnToken)
	}
	return req, nil
}

func (c *Client) doRaw(ctx context.Context, method, reqURL string, body []byte) ([]byte, http.Header, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := c.newRequest(ctx, method, reqURL, reader)
	if err != nil {
		return nil, nil, err
	}
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, resp.Header, fmt.Errorf("API error (HTTP 401): %s (run 'orka auth login' to refresh OIDC credentials, or 'orka login' for GitHub sign-in)", strings.TrimSpace(string(respBody)))
	}
	if resp.StatusCode >= 400 {
		return nil, resp.Header, fmt.Errorf("API error (HTTP %d): %s", resp.StatusCode, string(respBody))
	}
	return respBody, resp.Header, nil
}

func (c *Client) resourceURL(path string, query map[string]string) (string, error) {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u, err := url.Parse(c.BaseURL + path)
	if err != nil {
		return "", fmt.Errorf("invalid base URL: %w", err)
	}
	q := u.Query()
	if c.Namespace != "" {
		if _, ok := query["namespace"]; !ok {
			q.Set("namespace", c.Namespace)
		}
	}
	for k, v := range query {
		if v != "" {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (c *Client) DoJSON(ctx context.Context, method, path string, query map[string]string, body []byte) (any, error) {
	reqURL, err := c.resourceURL(path, query)
	if err != nil {
		return nil, err
	}
	respBody, _, err := c.doRaw(ctx, method, reqURL, body)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(respBody))) == 0 {
		return map[string]any{}, nil
	}
	var out any
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	return out, nil
}

func (c *Client) Stream(ctx context.Context, path string, query map[string]string) (io.ReadCloser, error) {
	reqURL, err := c.resourceURL(path, query)
	if err != nil {
		return nil, err
	}
	req, err := c.newRequest(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("API error (HTTP %d): %s", resp.StatusCode, string(body))
	}
	return resp.Body, nil
}

func (c *Client) GetRaw(ctx context.Context, path string, query map[string]string) ([]byte, string, error) {
	reqURL, err := c.resourceURL(path, query)
	if err != nil {
		return nil, "", err
	}
	body, headers, err := c.doRaw(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, "", err
	}
	return body, headers.Get("Content-Type"), nil
}

func (c *Client) DeleteResource(ctx context.Context, path string, query map[string]string) error {
	reqURL, err := c.resourceURL(path, query)
	if err != nil {
		return err
	}
	_, _, err = c.doRaw(ctx, http.MethodDelete, reqURL, nil)
	return err
}

func (c *Client) HealthCheck(ctx context.Context) (bool, error) {
	body, err := c.doGet(ctx, c.BaseURL+"/healthz")
	if err != nil {
		return false, err
	}
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		return false, nil
	}
	status, _ := resp["status"].(string)
	return status == "ok", nil
}

func (c *Client) ReadyCheck(ctx context.Context) (bool, error) {
	body, err := c.doGet(ctx, c.BaseURL+"/readyz")
	if err != nil {
		return false, err
	}
	var resp map[string]any
	if err := json.Unmarshal(body, &resp); err != nil {
		return false, nil
	}
	status, _ := resp["status"].(string)
	return status == "ok", nil
}

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
		if v, ok := status["ready"].(bool); ok {
			s.Ready = v
		}
		if v, ok := status["activeTasks"].(float64); ok {
			s.Active = int(v)
		}
		if claim, ok := status["identityClaim"].(map[string]any); ok {
			issuer := stringVal(claim["issuer"])
			id := stringVal(claim["id"])
			if issuer != "" && id != "" {
				s.Identity = issuer + "/" + id
			} else if id != "" {
				s.Identity = id
			}
		}
	}
	if s.Identity == "" && spec != nil {
		if identity, ok := spec["identity"].(map[string]any); ok {
			if entra, ok := identity["entra"].(map[string]any); ok {
				if id := strings.ToLower(strings.TrimSpace(stringVal(entra["agentIdentityID"]))); id != "" {
					s.Identity = corev1alpha1.AgentIdentityIssuerEntra + "/" + id
				}
			}
		}
	}

	return s
}

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

type CreateTaskRequest struct {
	Name      string   `json:"name"`
	Namespace string   `json:"namespace"`
	Type      string   `json:"type"`
	Image     string   `json:"image,omitempty"`
	Command   []string `json:"command,omitempty"`
	Args      []string `json:"args,omitempty"`
	Env       []struct {
		Name  string `json:"name"`
		Value string `json:"value,omitempty"`
	} `json:"env,omitempty"`
	Priority *int32  `json:"priority,omitempty"`
	Prompt   string  `json:"prompt,omitempty"`
	Timeout  string  `json:"timeout,omitempty"`
	Schedule string  `json:"schedule,omitempty"`
	TimeZone *string `json:"timeZone,omitempty"`
	Suspend  *bool   `json:"suspend,omitempty"`
	AgentRef *struct {
		Name string `json:"name"`
	} `json:"agentRef,omitempty"`
	AgentRuntime *corev1alpha1.AgentRuntimeSpec `json:"agentRuntime,omitempty"`
	AI           *struct {
		ProviderRef *struct {
			Name string `json:"name"`
		} `json:"providerRef,omitempty"`
		Model  string `json:"model,omitempty"`
		Prompt string `json:"prompt,omitempty"`
	} `json:"ai,omitempty"`
}

type TaskDetail map[string]any

type TaskSummary struct {
	Name          string `json:"name" yaml:"name"`
	Namespace     string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Type          string `json:"type,omitempty" yaml:"type,omitempty"`
	Phase         string `json:"phase,omitempty" yaml:"phase,omitempty"`
	Age           string `json:"age,omitempty" yaml:"age,omitempty"`
	Agent         string `json:"agent,omitempty" yaml:"agent,omitempty"`
	Image         string `json:"-" yaml:"-"`
	Iteration     int    `json:"iteration,omitempty" yaml:"iteration,omitempty"`
	TransactionID string `json:"transactionId,omitempty" yaml:"transactionId,omitempty"`
	ParentTask    string `json:"parentTask,omitempty" yaml:"parentTask,omitempty"`
}

type taskListResponse struct {
	Items    []TaskDetail `json:"items"`
	Metadata struct {
		Continue           string `json:"continue,omitempty"`
		RemainingItemCount *int64 `json:"remainingItemCount,omitempty"`
	} `json:"metadata"`
}

type ListTasksOptions struct {
	Namespace     string
	Limit         int
	Continue      string
	All           bool
	LabelSelector string
}

type ListTasksResult struct {
	Items              []TaskSummary
	Continue           string
	RemainingItemCount *int64
}

type TaskLogsResponse struct {
	Logs    string `json:"logs,omitempty"`
	Message string `json:"message,omitempty"`
	JobName string `json:"jobName,omitempty"`
}

type TaskResultResponse struct {
	Result string `json:"result"`
}

func (c *Client) CreateTaskRaw(ctx context.Context, body []byte) (*TaskDetail, error) {
	respBody, _, err := c.doRaw(ctx, http.MethodPost, c.BaseURL+"/api/v1/tasks", body)
	if err != nil {
		return nil, err
	}

	var detail TaskDetail
	if err := json.Unmarshal(respBody, &detail); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	return &detail, nil
}

func (c *Client) ListTasks(ctx context.Context, opts ListTasksOptions) ([]TaskSummary, error) {
	result, err := c.ListTasksPage(ctx, opts)
	if err != nil {
		return nil, err
	}
	return result.Items, nil
}

func (c *Client) ListTasksPage(ctx context.Context, opts ListTasksOptions) (*ListTasksResult, error) {
	u, err := url.Parse(c.BaseURL + "/api/v1/tasks")
	if err != nil {
		return nil, fmt.Errorf("invalid base URL: %w", err)
	}
	q := u.Query()
	if opts.Namespace != "" {
		q.Set("namespace", opts.Namespace)
	}
	if opts.All {
		q.Set("limit", "0")
	} else if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Continue != "" {
		q.Set("continue", opts.Continue)
	}
	if opts.LabelSelector != "" {
		q.Set("labelSelector", opts.LabelSelector)
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
	return &ListTasksResult{
		Items:              summaries,
		Continue:           resp.Metadata.Continue,
		RemainingItemCount: resp.Metadata.RemainingItemCount,
	}, nil
}

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
	if c.TxnToken != "" {
		req.Header.Set("Txn-Token", c.TxnToken)
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
	if c.TxnToken != "" {
		req.Header.Set("Txn-Token", c.TxnToken)
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

type SkillSummary struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"displayName,omitempty"`
	Description string   `json:"description"`
	Version     string   `json:"version,omitempty"`
	Author      string   `json:"author,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Phase       string   `json:"phase,omitempty"`
}

type SkillDetail map[string]any

type skillListResponse struct {
	Items    []SkillSummary `json:"items"`
	Metadata struct {
		Continue           string `json:"continue,omitempty"`
		RemainingItemCount *int64 `json:"remainingItemCount,omitempty"`
	} `json:"metadata"`
}

func (c *Client) ListSkills(ctx context.Context, opts ListOptions) ([]SkillSummary, error) {
	u, err := url.Parse(c.BaseURL + "/api/v1/skills")
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

	var resp skillListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return resp.Items, nil
}

func (c *Client) GetSkill(ctx context.Context, name string, opts GetOptions) (*SkillDetail, error) {
	u, err := url.Parse(c.BaseURL + "/api/v1/skills/" + url.PathEscape(name))
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

	var detail SkillDetail
	if err := json.Unmarshal(body, &detail); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	return &detail, nil
}

func (c *Client) DeleteSkill(ctx context.Context, name string, opts GetOptions) error {
	u, err := url.Parse(c.BaseURL + "/api/v1/skills/" + url.PathEscape(name))
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
	if c.TxnToken != "" {
		req.Header.Set("Txn-Token", c.TxnToken)
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

func (c *Client) CreateSkill(ctx context.Context, body []byte) (*SkillDetail, error) {
	u := c.BaseURL + "/api/v1/skills"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.TxnToken != "" {
		req.Header.Set("Txn-Token", c.TxnToken)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("API error (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	var detail SkillDetail
	if err := json.Unmarshal(respBody, &detail); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	return &detail, nil
}

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

type StreamLogsOptions struct {
	Namespace string
	Writer    io.Writer
}

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
	if c.TxnToken != "" {
		req.Header.Set("Txn-Token", c.TxnToken)
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

type ArtifactMetadata struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
	CreatedAt   string `json:"createdAt"`
}

func (c *Client) ListArtifacts(ctx context.Context, taskName string, opts GetOptions) ([]ArtifactMetadata, error) {
	u, err := url.Parse(c.BaseURL + "/api/v1/tasks/" + url.PathEscape(taskName) + "/artifacts")
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

	var resp struct {
		Artifacts []ArtifactMetadata `json:"artifacts"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	if resp.Artifacts == nil {
		return []ArtifactMetadata{}, nil
	}
	return resp.Artifacts, nil
}

func (c *Client) DownloadArtifact(
	ctx context.Context,
	taskName, filename string,
	opts GetOptions,
) ([]byte, string, error) {
	u, err := url.Parse(c.BaseURL + "/api/v1/tasks/" + url.PathEscape(taskName) + "/artifacts/" + url.PathEscape(filename))
	if err != nil {
		return nil, "", fmt.Errorf("invalid base URL: %w", err)
	}
	q := u.Query()
	if opts.Namespace != "" {
		q.Set("namespace", opts.Namespace)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create request: %w", err)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.TxnToken != "" {
		req.Header.Set("Txn-Token", c.TxnToken)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode == http.StatusNotFound {
		return nil, "", fmt.Errorf("artifact %q not found for task %q", filename, taskName)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, "", fmt.Errorf("API error (HTTP %d): %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read response: %w", err)
	}
	contentType := resp.Header.Get("Content-Type")
	return body, contentType, nil
}

func extractTaskSummary(item TaskDetail) TaskSummary {
	s := TaskSummary{
		Name:          StringField(item, "metadata", "name"),
		Namespace:     StringField(item, "metadata", "namespace"),
		Type:          StringField(item, "spec", "type"),
		Phase:         StringField(item, "status", "phase"),
		Age:           StringField(item, "metadata", "creationTimestamp"),
		Agent:         StringField(item, "spec", "agentRef", "name"),
		Image:         StringField(item, "spec", "image"),
		TransactionID: StringField(item, "spec", "transaction", "id"),
		ParentTask:    StringField(item, "metadata", "annotations", labels.AnnotationParentTaskName),
	}
	if s.ParentTask == "" {
		s.ParentTask = StringField(item, "metadata", "labels", labels.LabelParentTask)
	}
	if status, ok := item["status"].(map[string]any); ok {
		if v, ok := status["iteration"].(float64); ok {
			s.Iteration = int(v)
		}
	}
	return s
}

func (c *Client) GetTaskPlan(ctx context.Context, name string, opts GetOptions) (any, error) {
	query := map[string]string{}
	if opts.Namespace != "" {
		query["namespace"] = opts.Namespace
	}
	return c.DoJSON(ctx, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(name)+"/plan", query, nil)
}

func (c *Client) GetTaskChildren(ctx context.Context, name string, opts GetOptions) (any, error) {
	query := map[string]string{}
	if opts.Namespace != "" {
		query["namespace"] = opts.Namespace
	}
	return c.DoJSON(ctx, http.MethodGet, "/api/v1/tasks/"+url.PathEscape(name)+"/children", query, nil)
}

func (c *Client) AuthValidate(ctx context.Context) (any, error) {
	return c.DoJSON(ctx, http.MethodGet, "/api/v1/auth/validate", nil, nil)
}

func (c *Client) AuthWhoAmI(ctx context.Context) (any, error) {
	return c.DoJSON(ctx, http.MethodGet, "/api/v1/auth/whoami", nil, nil)
}

func (c *Client) ListModels(ctx context.Context, compat string) (any, error) {
	path := "/openai/v1/models"
	if compat == "anthropic" {
		path = "/anthropic/v1/models"
	}
	return c.DoJSON(ctx, http.MethodGet, path, nil, nil)
}
