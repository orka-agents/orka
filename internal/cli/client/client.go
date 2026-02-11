/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client is an HTTP client for the Mercan REST API.
type Client struct {
	BaseURL    string
	Token      string
	Namespace  string
	HTTPClient *http.Client
}

// New creates a new Mercan API client.
func New(baseURL, token, namespace string) *Client {
	return &Client{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		Token:     token,
		Namespace: namespace,
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// do executes an HTTP request with authentication and returns the response.
func (c *Client) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	reqURL := c.BaseURL + path
	req, err := http.NewRequestWithContext(ctx, method, reqURL, body)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}

	return resp, nil
}

// doJSON executes a request and decodes the JSON response into the given target.
func (c *Client) doJSON(ctx context.Context, method, path string, body io.Reader, target interface{}) error {
	resp, err := c.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	if target != nil {
		if err := json.NewDecoder(resp.Body).Decode(target); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}
	return nil
}

// encodeBody marshals v to JSON and returns a reader.
func encodeBody(v interface{}) (io.Reader, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encoding request body: %w", err)
	}
	return bytes.NewReader(data), nil
}

// --- Task methods ---

// CreateTask creates a new task.
func (c *Client) CreateTask(ctx context.Context, req CreateTaskRequest) (json.RawMessage, error) {
	body, err := encodeBody(req)
	if err != nil {
		return nil, err
	}
	var result json.RawMessage
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/tasks", body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// ListTasks lists tasks with optional filtering and pagination.
func (c *Client) ListTasks(ctx context.Context, namespace string, limit int, continueToken string) (*ListResponse, error) {
	params := url.Values{}
	if namespace != "" {
		params.Set("namespace", namespace)
	}
	if limit > 0 {
		params.Set("limit", strconv.Itoa(limit))
	}
	if continueToken != "" {
		params.Set("continue", continueToken)
	}

	path := "/api/v1/tasks"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}

	var result ListResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetTask retrieves a specific task by namespace and name.
func (c *Client) GetTask(ctx context.Context, namespace, name string) (json.RawMessage, error) {
	var result json.RawMessage
	path := fmt.Sprintf("/api/v1/tasks/%s/%s", namespace, name)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// DeleteTask deletes a task by namespace and name.
func (c *Client) DeleteTask(ctx context.Context, namespace, name string) error {
	path := fmt.Sprintf("/api/v1/tasks/%s/%s", namespace, name)
	resp, err := c.do(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// GetTaskResult retrieves the result of a completed task.
func (c *Client) GetTaskResult(ctx context.Context, namespace, name string) (json.RawMessage, error) {
	var result json.RawMessage
	path := fmt.Sprintf("/api/v1/tasks/%s/%s/result", namespace, name)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// StreamTaskLogs returns a reader for streaming task logs (SSE).
// The caller is responsible for closing the returned ReadCloser.
func (c *Client) StreamTaskLogs(ctx context.Context, namespace, name string) (io.ReadCloser, error) {
	path := fmt.Sprintf("/api/v1/tasks/%s/%s/logs", namespace, name)
	resp, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	return resp.Body, nil
}

// --- Agent methods ---

// ListAgents lists agents in the given namespace.
func (c *Client) ListAgents(ctx context.Context, namespace string) (json.RawMessage, error) {
	params := url.Values{}
	if namespace != "" {
		params.Set("namespace", namespace)
	}

	path := "/api/v1/agents"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}

	var result json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetAgent retrieves a specific agent by namespace and name.
func (c *Client) GetAgent(ctx context.Context, namespace, name string) (json.RawMessage, error) {
	var result json.RawMessage
	path := fmt.Sprintf("/api/v1/agents/%s/%s", namespace, name)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// CreateAgent creates a new agent.
func (c *Client) CreateAgent(ctx context.Context, req CreateAgentRequest) (json.RawMessage, error) {
	body, err := encodeBody(req)
	if err != nil {
		return nil, err
	}
	var result json.RawMessage
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/agents", body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// UpdateAgent updates an existing agent.
func (c *Client) UpdateAgent(ctx context.Context, namespace, name string, req UpdateAgentRequest) (json.RawMessage, error) {
	body, err := encodeBody(req)
	if err != nil {
		return nil, err
	}
	var result json.RawMessage
	path := fmt.Sprintf("/api/v1/agents/%s/%s", namespace, name)
	if err := c.doJSON(ctx, http.MethodPut, path, body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// DeleteAgent deletes an agent by namespace and name.
func (c *Client) DeleteAgent(ctx context.Context, namespace, name string) error {
	path := fmt.Sprintf("/api/v1/agents/%s/%s", namespace, name)
	resp, err := c.do(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// --- Session methods ---

// ListSessions lists sessions in the given namespace.
func (c *Client) ListSessions(ctx context.Context, namespace string) (json.RawMessage, error) {
	params := url.Values{}
	if namespace != "" {
		params.Set("namespace", namespace)
	}

	path := "/api/v1/sessions"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}

	var result json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetSession retrieves a specific session by namespace and ID.
func (c *Client) GetSession(ctx context.Context, namespace, id string) (json.RawMessage, error) {
	var result json.RawMessage
	path := fmt.Sprintf("/api/v1/sessions/%s/%s", namespace, id)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// DeleteSession deletes a session by namespace and ID.
func (c *Client) DeleteSession(ctx context.Context, namespace, id string) error {
	path := fmt.Sprintf("/api/v1/sessions/%s/%s", namespace, id)
	resp, err := c.do(ctx, http.MethodDelete, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// --- Tool methods ---

// ListTools lists tools in the given namespace.
func (c *Client) ListTools(ctx context.Context, namespace string) (json.RawMessage, error) {
	params := url.Values{}
	if namespace != "" {
		params.Set("namespace", namespace)
	}

	path := "/api/v1/tools"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}

	var result json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetTool retrieves a specific tool by namespace and name.
func (c *Client) GetTool(ctx context.Context, namespace, name string) (json.RawMessage, error) {
	var result json.RawMessage
	path := fmt.Sprintf("/api/v1/tools/%s/%s", namespace, name)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}
