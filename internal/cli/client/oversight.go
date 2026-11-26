/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	oversightv1 "github.com/orka-agents/orka/pkg/oversight/v1"
)

// IngestOversightDecisions submits a batch of oversight decisions to the
// namespace's internal ingest endpoint.
func (c *Client) IngestOversightDecisions(ctx context.Context, namespace string, decisions []oversightv1.OversightDecision) (*oversightv1.IngestResponse, error) {
	body, err := json.Marshal(oversightv1.IngestRequest{Decisions: decisions})
	if err != nil {
		return nil, fmt.Errorf("encode decisions: %w", err)
	}
	respBody, _, err := c.doRaw(ctx, http.MethodPost, c.BaseURL+"/internal/v1/oversight/"+url.PathEscape(namespace)+"/decisions", body)
	if err != nil {
		return nil, err
	}
	var response oversightv1.IngestResponse
	if err := json.Unmarshal(respBody, &response); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}
	return &response, nil
}

// GetOversightPosture reads the namespace's oversight posture.
func (c *Client) GetOversightPosture(ctx context.Context, window, runID string) (*oversightv1.PostureResponse, error) {
	var posture oversightv1.PostureResponse
	if err := c.getOversight(ctx, "/api/v1/oversight/posture", map[string]string{"window": window, "runID": runID}, &posture); err != nil {
		return nil, err
	}
	return &posture, nil
}

// ListOversightDecisions searches the namespace's oversight decisions.
func (c *Client) ListOversightDecisions(ctx context.Context, query, window string, limit int, cursor string) (*oversightv1.DecisionListResponse, error) {
	params := map[string]string{"q": query, "window": window, "continue": cursor}
	if limit > 0 {
		params["limit"] = fmt.Sprint(limit)
	}
	var list oversightv1.DecisionListResponse
	if err := c.getOversight(ctx, "/api/v1/oversight/decisions", params, &list); err != nil {
		return nil, err
	}
	return &list, nil
}

func (c *Client) getOversight(ctx context.Context, path string, query map[string]string, out any) error {
	reqURL, err := c.resourceURL(path, query)
	if err != nil {
		return err
	}
	body, err := c.doGet(ctx, reqURL)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("failed to decode response: %w", err)
	}
	return nil
}
