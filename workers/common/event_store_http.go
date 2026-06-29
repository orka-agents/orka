package common

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

	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/workerenv"
)

// HTTPExecutionEventStore is a worker-side event store adapter backed by the
// controller event APIs. It gives tools that run inside a worker task enough
// durable event access to request and inspect approvals without direct DB access.
type HTTPExecutionEventStore struct {
	controllerURL string
	bearerPath    string
	client        *http.Client
	timeout       time.Duration
}

var _ store.ExecutionEventStore = (*HTTPExecutionEventStore)(nil)

const maxHTTPExecutionEventStoreResponseBytes = 64 << 20

// NewHTTPExecutionEventStoreFromEnv creates a worker-side event store client
// from the same environment contract used by the HTTP event recorder. Missing
// controller configuration returns nil so callers can fail closed for gated tools.
func NewHTTPExecutionEventStoreFromEnv() store.ExecutionEventStore {
	controllerURL := strings.TrimSpace(firstNonEmpty(
		os.Getenv(workerenv.ControllerURL),
	))
	if controllerURL == "" {
		return nil
	}
	return &HTTPExecutionEventStore{
		controllerURL: controllerURL,
		bearerPath:    firstNonEmpty(os.Getenv(workerenv.ServiceAccountTokenPath), DefaultServiceAccountBearerPath),
		client:        &http.Client{},
		timeout:       defaultEventRecorderTimeout,
	}
}

func (s *HTTPExecutionEventStore) AppendExecutionEvent(
	ctx context.Context,
	event *store.ExecutionEvent,
) (*store.ExecutionEvent, error) {
	if s == nil {
		return nil, store.ErrNotFound
	}
	if event == nil {
		return nil, store.ValidationErrorf("execution event is required")
	}
	if store.IsTerminalApprovalExecutionEventType(event.Type) {
		return nil, store.ValidationErrorf("approval decision events must use the approval decision API")
	}
	streamType := firstNonEmpty(event.StreamType, events.ExecutionEventStreamTypeTask)
	if streamType != events.ExecutionEventStreamTypeTask {
		return nil, store.ValidationErrorf("worker HTTP event store supports task streams only")
	}
	endpoint, err := executionEventEndpoint(s.controllerURL, event.Namespace, event.StreamID)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(submitRecordedEventRequest{
		Type:        event.Type,
		Severity:    event.Severity,
		TaskName:    event.TaskName,
		SessionName: event.SessionName,
		AgentName:   event.AgentName,
		ToolName:    event.ToolName,
		ToolCallID:  event.ToolCallID,
		Summary:     event.Summary,
		Content:     event.Content,
		ContentText: event.ContentText,
		Truncation:  event.Truncation,
	})
	if err != nil {
		return nil, err
	}
	var response struct {
		ID        string    `json:"id"`
		Seq       int64     `json:"seq"`
		CreatedAt time.Time `json:"createdAt"`
	}
	if err := s.doJSON(ctx, http.MethodPost, endpoint, body, &response); err != nil {
		return nil, err
	}
	copy := *event
	copy.ID = response.ID
	copy.Seq = response.Seq
	copy.CreatedAt = response.CreatedAt
	return &copy, nil
}

func (s *HTTPExecutionEventStore) ListExecutionEvents(
	ctx context.Context,
	filter store.ExecutionEventFilter,
) ([]store.ExecutionEvent, error) {
	if s == nil {
		return nil, store.ErrNotFound
	}
	filter = filter.Normalized()
	if filter.StreamType != events.ExecutionEventStreamTypeTask {
		return nil, store.ValidationErrorf("worker HTTP event store supports task streams only")
	}
	endpoint, err := taskEventsEndpoint(s.controllerURL, filter)
	if err != nil {
		return nil, err
	}
	var response struct {
		Events []store.ExecutionEvent `json:"events"`
	}
	if err := s.doJSON(ctx, http.MethodGet, endpoint, nil, &response); err != nil {
		return nil, err
	}
	return response.Events, nil
}

func (s *HTTPExecutionEventStore) ListSessionExecutionEvents(
	context.Context,
	store.SessionExecutionEventFilter,
) ([]store.SessionExecutionEvent, int64, error) {
	return nil, 0, store.ValidationErrorf("worker HTTP event store does not support session event reads")
}

func (s *HTTPExecutionEventStore) GetLatestExecutionEventSeq(
	ctx context.Context,
	namespace, streamType, streamID string,
) (int64, error) {
	if s == nil {
		return 0, store.ErrNotFound
	}
	filter := store.ExecutionEventFilter{
		Namespace:  namespace,
		StreamType: streamType,
		StreamID:   streamID,
		Limit:      1,
	}.Normalized()
	endpoint, err := taskEventsEndpoint(s.controllerURL, filter)
	if err != nil {
		return 0, err
	}
	var response struct {
		LatestSeq int64 `json:"latestSeq"`
	}
	if err := s.doJSON(ctx, http.MethodGet, endpoint, nil, &response); err != nil {
		return 0, err
	}
	return response.LatestSeq, nil
}

func (s *HTTPExecutionEventStore) DeleteExecutionEvents(context.Context, string, string, string) error {
	return store.ValidationErrorf("worker HTTP event store does not support deleting events")
}

func (s *HTTPExecutionEventStore) doJSON(ctx context.Context, method, endpoint string, body []byte, out any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := s.timeout
	if timeout <= 0 {
		timeout = defaultEventRecorderTimeout
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, endpoint, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token := readServiceAccountToken(s.bearerPath); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	var data []byte
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		data, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return fmt.Errorf(
			"execution event API %s %s returned %d: %s",
			method,
			endpoint,
			resp.StatusCode,
			strings.TrimSpace(string(data)),
		)
	}
	if out == nil {
		return nil
	}
	data, _ = io.ReadAll(io.LimitReader(resp.Body, maxHTTPExecutionEventStoreResponseBytes+1))
	if len(data) > maxHTTPExecutionEventStoreResponseBytes {
		return fmt.Errorf(
			"execution event API %s %s response exceeds %d bytes",
			method,
			endpoint,
			maxHTTPExecutionEventStoreResponseBytes,
		)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode execution event API response: %w", err)
	}
	return nil
}

func taskEventsEndpoint(controllerURL string, filter store.ExecutionEventFilter) (string, error) {
	base, err := url.Parse(controllerURL)
	if err != nil {
		return "", err
	}
	if base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("controller URL must be absolute")
	}
	endpoint, err := url.JoinPath(base.String(), "api", "v1", "tasks", filter.StreamID, "events")
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	query.Set("namespace", filter.Namespace)
	query.Set("after", fmt.Sprintf("%d", filter.AfterSeq))
	query.Set("limit", fmt.Sprintf("%d", filter.Limit))
	if filter.ToolCallID != "" {
		query.Set("toolCallID", filter.ToolCallID)
	}
	for _, typ := range filter.EventTypes {
		query.Add("type", typ)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}
