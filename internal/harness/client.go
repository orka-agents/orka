package harness

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/events"
)

const (
	maxFetchTurnOutputBytes        = 50 << 20
	maxHarnessControlResponseBytes = 1 << 20
)

type Client struct {
	baseURL         *url.URL
	httpClient      *http.Client
	controlTimeout  time.Duration
	authBearerValue string
}

const (
	// A 1 MiB decoded result can expand by up to 6x when JSON escapes control
	// bytes. Keep enough room for the frame envelope while bounding both each
	// SSE line and the complete multi-line event.
	maxHarnessSSELineBytes  = 8 << 20
	maxHarnessSSEEventBytes = 8 << 20
)

var errSSEDone = errors.New("harness SSE stream done")

type ClientOption func(*Client)

func WithHTTPClient(httpClient *http.Client) ClientOption {
	return func(c *Client) {
		if httpClient != nil {
			c.httpClient = httpClient
		}
	}
}

func WithControlTimeout(timeout time.Duration) ClientOption {
	return func(c *Client) {
		if timeout > 0 {
			c.controlTimeout = timeout
		}
	}
}

func WithBearerToken(token string) ClientOption {
	return func(c *Client) {
		c.authBearerValue = strings.TrimSpace(token)
	}
}

func NewClient(baseURL string, opts ...ClientOption) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("parse harness base url: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("harness base url must include scheme and host")
	}
	c := &Client{baseURL: parsed, httpClient: &http.Client{}, controlTimeout: 30 * time.Second}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c, nil
}

func (c *Client) Health(ctx context.Context) (_ *HealthResponse, err error) {
	defer func() { err = c.sanitizeClientError(err) }()
	var response HealthResponse
	if err := c.getJSON(ctx, HealthPath, &response); err != nil {
		return nil, err
	}
	if err := response.Validate(); err != nil {
		return nil, safeClientError("health", 0, err.Error(), err)
	}
	sanitized, err := c.sanitizeHealthResponse(response)
	if err != nil {
		return nil, safeClientError("health", 0, err.Error(), err)
	}
	return &sanitized, nil
}

func (c *Client) Capabilities(ctx context.Context) (_ *CapabilitiesResponse, err error) {
	defer func() { err = c.sanitizeClientError(err) }()
	var response CapabilitiesResponse
	if err := c.getJSON(ctx, CapabilitiesPath, &response); err != nil {
		return nil, err
	}
	if err := response.Validate(); err != nil {
		return nil, safeClientError("capabilities", 0, err.Error(), err)
	}
	sanitized, err := c.sanitizeCapabilitiesResponse(response)
	if err != nil {
		return nil, safeClientError("capabilities", 0, err.Error(), err)
	}
	return &sanitized, nil
}

func (c *Client) sanitizeHealthResponse(response HealthResponse) (HealthResponse, error) {
	for name, value := range map[string]string{
		"version":          response.Version,
		"status":           string(response.Status),
		"runtimeSessionID": string(response.RuntimeSessionID),
		"ready":            strconv.FormatBool(response.Ready),
		"checkedAt":        response.CheckedAt.Format(time.RFC3339Nano),
	} {
		if c.structuralValueContainsSensitiveData(value) {
			return HealthResponse{}, fmt.Errorf("harness health structural field %s contains sensitive data", name)
		}
	}
	response.Message = c.sanitizeClientMessage(response.Message)
	response.Metadata = sanitizeHarnessStringMap(response.Metadata, c.sanitizeClientMessage)
	return response, nil
}

func (c *Client) sanitizeCapabilitiesResponse(response CapabilitiesResponse) (CapabilitiesResponse, error) {
	for name, value := range map[string]string{
		"version":           response.Version,
		"protocolVersion":   response.ProtocolVersion,
		"transport":         response.Transport,
		"runtimeName":       response.RuntimeName,
		"providerKind":      string(response.ProviderKind),
		"supportsCancel":    strconv.FormatBool(response.SupportsCancel),
		"supportsSessions":  strconv.FormatBool(response.SupportsRuntimeSessions),
		"supportsContinue":  strconv.FormatBool(response.SupportsContinuation),
		"supportsArtifacts": strconv.FormatBool(response.SupportsArtifacts),
		"supportsSuspend":   strconv.FormatBool(response.SupportsSuspend),
		"supportsSnapshot":  strconv.FormatBool(response.SupportsWorkspaceSnapshot),
		"maxConcurrent":     strconv.Itoa(response.MaxConcurrentTurns),
		"maxTurnSeconds":    strconv.Itoa(response.MaxTurnSeconds),
		"maxOutputBytes":    strconv.FormatInt(response.MaxOutputBytes, 10),
	} {
		if c.structuralValueContainsSensitiveData(value) {
			return CapabilitiesResponse{}, fmt.Errorf("harness capabilities structural field %s contains sensitive data", name)
		}
	}
	for _, mode := range response.ToolExecutionModes {
		if c.structuralValueContainsSensitiveData(string(mode)) {
			return CapabilitiesResponse{}, fmt.Errorf("harness capabilities structural field toolExecutionModes contains sensitive data")
		}
	}
	for _, class := range response.BrokeredToolClasses {
		if c.structuralValueContainsSensitiveData(string(class)) {
			return CapabilitiesResponse{}, fmt.Errorf("harness capabilities structural field brokeredToolClasses contains sensitive data")
		}
	}
	response.ToolExecutionModes = append([]ToolExecutionMode(nil), response.ToolExecutionModes...)
	response.BrokeredToolClasses = append([]BrokeredToolClass(nil), response.BrokeredToolClasses...)
	response.RuntimeVersion = c.sanitizeClientMessage(response.RuntimeVersion)
	response.Metadata = sanitizeHarnessStringMap(response.Metadata, c.sanitizeClientMessage)
	return response, nil
}

type startTurnResponseWire struct {
	StartTurnResponse
	Accepted *bool `json:"accepted"`
}

func (c *Client) StartTurn(ctx context.Context, request StartTurnRequest) (_ *StartTurnResponse, err error) {
	defer func() { err = c.sanitizeClientError(err) }()
	if err := request.Validate(); err != nil {
		return nil, safeClientError("start_turn", 0, err.Error(), err)
	}
	var decoded startTurnResponseWire
	if err := c.postJSON(ctx, TurnsPath, request, &decoded); err != nil {
		return nil, err
	}
	response := decoded.StartTurnResponse
	if decoded.Accepted != nil {
		response.Accepted = *decoded.Accepted
	}
	if strings.TrimSpace(response.Version) != ProtocolVersion {
		message := fmt.Sprintf("unsupported version %q", response.Version)
		if decoded.Accepted == nil {
			return nil, unknownAcceptanceClientError("start_turn", message)
		}
		if response.Accepted {
			return nil, acceptedClientError("start_turn", message)
		}
		return nil, safeClientError("start_turn", 0, message)
	}
	if decoded.Accepted == nil {
		return nil, unknownAcceptanceClientError("start_turn", "harness response did not include accepted")
	}
	if !response.Accepted {
		return nil, safeClientError("start_turn", 0, "harness did not accept turn")
	}
	if err := validateAcceptedTurn(request, response); err != nil {
		return nil, acceptedClientError("start_turn", err.Error())
	}
	if c.structuralValueContainsSensitiveData(response.EventStreamPath) {
		return nil, acceptedClientError("start_turn", "harness start response eventStreamPath contains sensitive data")
	}
	return &response, nil
}

func (c *Client) CancelTurn(ctx context.Context, request CancelTurnRequest) (_ *CancelTurnResponse, err error) {
	defer func() { err = c.sanitizeClientError(err) }()
	if err := request.Validate(); err != nil {
		return nil, safeClientError("cancel_turn", 0, err.Error(), err)
	}
	rel, err := CancelTurnPath(request.TurnID)
	if err != nil {
		return nil, safeClientError("cancel_turn", 0, err.Error(), err)
	}
	var response CancelTurnResponse
	if err := c.postJSON(ctx, rel, request, &response); err != nil {
		return nil, err
	}
	if strings.TrimSpace(response.Version) != ProtocolVersion {
		return nil, safeClientError("cancel_turn", 0, fmt.Sprintf("unsupported version %q", response.Version))
	}
	if !response.Accepted {
		return nil, safeClientError("cancel_turn", 0, "harness did not accept cancellation")
	}
	if response.RuntimeSessionID != request.RuntimeSessionID {
		return nil, safeClientError(
			"cancel_turn",
			0,
			fmt.Sprintf("harness cancelled runtime session %q, want %q", response.RuntimeSessionID, request.RuntimeSessionID),
		)
	}
	if response.TurnID != request.TurnID {
		return nil, safeClientError("cancel_turn", 0, fmt.Sprintf("harness cancelled turn %q, want %q", response.TurnID, request.TurnID))
	}
	if response.CorrelationID != "" && response.CorrelationID != request.CorrelationID {
		return nil, safeClientError(
			"cancel_turn",
			0,
			fmt.Sprintf("harness cancelled correlation id %q, want %q", response.CorrelationID, request.CorrelationID),
		)
	}
	response.Message = c.sanitizeClientMessage(response.Message)
	return &response, nil
}

func (c *Client) ContinueTurn(ctx context.Context, request ContinueTurnRequest) (_ *ContinueTurnResponse, err error) {
	defer func() { err = c.sanitizeClientError(err) }()
	if err := request.Validate(); err != nil {
		return nil, safeClientError("continue_turn", 0, err.Error(), err)
	}
	rel, err := ContinueTurnPath(request.TurnID)
	if err != nil {
		return nil, safeClientError("continue_turn", 0, err.Error(), err)
	}
	var response ContinueTurnResponse
	if err := c.postJSON(ctx, rel, request, &response); err != nil {
		return nil, err
	}
	if err := response.ValidateFor(request); err != nil {
		return nil, safeClientError("continue_turn", 0, err.Error(), err)
	}
	response.Message = c.sanitizeClientMessage(response.Message)
	return &response, nil
}

func (c *Client) FetchTurnOutput(ctx context.Context, turnID HarnessTurnID, outputRef string) (_ []byte, err error) {
	defer func() { err = c.sanitizeClientError(err) }()
	ctx, cancel := c.controlContext(ctx)
	defer cancel()
	rel, err := OutputTurnPath(turnID)
	if err != nil {
		return nil, safeClientError("fetch_turn_output", 0, err.Error(), err)
	}
	if strings.TrimSpace(outputRef) == "" {
		return nil, safeClientError("fetch_turn_output", 0, "output ref is required")
	}
	u := c.resolve(rel)
	q := u.Query()
	q.Set("ref", outputRef)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, safeClientError("fetch_turn_output", 0, err.Error(), err)
	}
	req.Header.Set("Accept", "application/octet-stream")
	c.setAuthHeader(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, safeClientError("fetch_turn_output", 0, err.Error(), err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, c.statusError("fetch_turn_output", resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchTurnOutputBytes+1))
	if err != nil {
		return nil, safeClientError("fetch_turn_output", resp.StatusCode, err.Error(), err)
	}
	if len(data) > maxFetchTurnOutputBytes {
		return nil, safeClientError("fetch_turn_output", resp.StatusCode, "output exceeds harness fetch limit")
	}
	if c.authBearerValue != "" && bytes.Contains(data, []byte(c.authBearerValue)) {
		return nil, safeClientError("fetch_turn_output", resp.StatusCode, "output contains configured bearer")
	}
	return data, nil
}

func (c *Client) StreamFrames(ctx context.Context, turnID HarnessTurnID, afterSeq int64, emit func(HarnessEventFrame) error) error {
	rel, err := EventStreamPath(turnID)
	if err != nil {
		return c.sanitizeClientError(safeClientError("stream_frames", 0, err.Error(), err))
	}
	if emit == nil {
		return c.sanitizeClientError(safeClientError("stream_frames", 0, "emit callback is required"))
	}
	u := c.resolve(rel)
	q := u.Query()
	if afterSeq > 0 {
		q.Set("afterSeq", strconv.FormatInt(afterSeq, 10))
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return c.sanitizeClientError(safeClientError("stream_frames", 0, err.Error(), err))
	}
	req.Header.Set("Accept", "text/event-stream")
	c.setAuthHeader(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return c.sanitizeClientError(safeClientError("stream_frames", 0, err.Error(), err))
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.sanitizeClientError(c.statusError("stream_frames", resp))
	}
	return readSSEFramesWithSanitizers(resp.Body, emit, c.sanitizeClientError, c.sanitizeHarnessFrame)
}

func (c *Client) getJSON(ctx context.Context, rel string, out any) error {
	ctx, cancel := c.controlContext(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.resolve(rel).String(), nil)
	if err != nil {
		return safeClientError("get", 0, err.Error(), err)
	}
	req.Header.Set("Accept", "application/json")
	c.setAuthHeader(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return safeClientError("get", 0, err.Error(), err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.statusError("get", resp)
	}
	return decodeBoundedJSON("get", resp.StatusCode, resp.Body, out)
}

func (c *Client) postJSON(ctx context.Context, rel string, in, out any) error {
	ctx, cancel := c.controlContext(ctx)
	defer cancel()
	payload, err := json.Marshal(in)
	if err != nil {
		return safeClientError("post", 0, err.Error(), err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.resolve(rel).String(), bytes.NewReader(payload))
	if err != nil {
		return safeClientError("post", 0, err.Error(), err)
	}
	req.Header.Set("Accept", "application/json")
	c.setAuthHeader(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return unknownAcceptanceClientError("post", err.Error(), err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.statusError("post", resp)
	}
	return decodeBoundedJSON("post", resp.StatusCode, resp.Body, out)
}

func decodeBoundedJSON(op string, status int, body io.Reader, out any) error {
	data, err := io.ReadAll(io.LimitReader(body, maxHarnessControlResponseBytes+1))
	if err != nil {
		return safeClientError(op, status, err.Error(), err)
	}
	if len(data) > maxHarnessControlResponseBytes {
		return safeClientError(op, status, "JSON response exceeds harness control limit")
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return safeClientError(op, status, err.Error(), err)
	}
	return nil
}

func (c *Client) sanitizeClientMessage(message string) string {
	message = events.RedactExecutionEventText(message)
	if c != nil {
		message = RedactExactBearerValue(message, c.authBearerValue)
	}
	return message
}

func (c *Client) sanitizeHarnessFrame(frame HarnessEventFrame) (HarnessEventFrame, error) {
	if err := c.validateHarnessFrameStructure(frame); err != nil {
		return HarnessEventFrame{}, err
	}
	sanitize := c.sanitizeClientMessage
	frame.Severity = sanitize(frame.Severity)
	frame.Summary = sanitize(frame.Summary)
	frame.ContentText = sanitize(frame.ContentText)
	frame.Metadata = sanitizeHarnessStringMap(frame.Metadata, sanitize)
	if frame.Type == FrameToolCallRequested {
		containsBearer, err := harnessJSONContainsBearer(frame.Content, c.authBearerValue)
		if err != nil {
			return HarnessEventFrame{}, fmt.Errorf("invalid harness frame content JSON: %w", err)
		}
		if containsBearer {
			return HarnessEventFrame{}, fmt.Errorf("harness frame brokered tool content contains configured bearer")
		}
	} else {
		content, err := sanitizeHarnessJSON(frame.Content, c.authBearerValue, sanitize)
		if err != nil {
			return HarnessEventFrame{}, fmt.Errorf("invalid harness frame content JSON: %w", err)
		}
		frame.Content = content
	}
	if frame.Completed != nil {
		completed := *frame.Completed
		completed.Result = sanitize(completed.Result)
		completed.Data = sanitizeHarnessAnyMap(completed.Data, sanitize)
		completed.Artifacts = sanitizeHarnessArtifacts(completed.Artifacts, sanitize)
		frame.Completed = &completed
	}
	if frame.Failed != nil {
		failed := *frame.Failed
		failed.Message = sanitize(failed.Message)
		failed.Result = sanitize(failed.Result)
		failed.Data = sanitizeHarnessAnyMap(failed.Data, sanitize)
		failed.Artifacts = sanitizeHarnessArtifacts(failed.Artifacts, sanitize)
		frame.Failed = &failed
	}
	if frame.Error != nil {
		info := *frame.Error
		info.Message = sanitize(info.Message)
		frame.Error = &info
	}
	return frame, nil
}

func (c *Client) structuralValueContainsSensitiveData(value string) bool {
	if events.RedactExecutionEventText(value) != value {
		return true
	}
	return c != nil && StructuredValueContainsBearer(value, c.authBearerValue)
}

func (c *Client) validateHarnessFrameStructure(frame HarnessEventFrame) error {
	check := func(name, value string) error {
		if c.structuralValueContainsSensitiveData(value) {
			return fmt.Errorf("harness frame structural field %s contains sensitive data", name)
		}
		return nil
	}
	for name, value := range map[string]string{
		"version":          frame.Version,
		"type":             string(frame.Type),
		"runtimeSessionID": string(frame.RuntimeSessionID),
		"turnID":           string(frame.TurnID),
		"correlationID":    frame.CorrelationID,
		"toolName":         frame.ToolName,
		"toolCallID":       frame.ToolCallID,
		"approvalID":       frame.ApprovalID,
	} {
		if err := check(name, value); err != nil {
			return err
		}
	}
	for key := range frame.Metadata {
		if err := check("metadata key", key); err != nil {
			return err
		}
	}
	if err := check("seq", strconv.FormatInt(frame.Seq, 10)); err != nil {
		return err
	}
	if !frame.CreatedAt.IsZero() {
		if err := check("createdAt", frame.CreatedAt.Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	if frame.Completed != nil {
		if err := check("completed.outputRef", frame.Completed.OutputRef); err != nil {
			return err
		}
		if err := check("completed.finalEventSeq", strconv.FormatInt(frame.Completed.FinalEventSeq, 10)); err != nil {
			return err
		}
		if err := check("completed.retainSession", strconv.FormatBool(frame.Completed.RetainSession)); err != nil {
			return err
		}
		for _, artifact := range frame.Completed.Artifacts {
			if err := validateHarnessArtifactStructure(artifact, check); err != nil {
				return err
			}
		}
	}
	if frame.Failed != nil {
		if err := check("failed.reason", frame.Failed.Reason); err != nil {
			return err
		}
		if err := check("failed.outputRef", frame.Failed.OutputRef); err != nil {
			return err
		}
		if err := check("failed.retryable", strconv.FormatBool(frame.Failed.Retryable)); err != nil {
			return err
		}
		for _, artifact := range frame.Failed.Artifacts {
			if err := validateHarnessArtifactStructure(artifact, check); err != nil {
				return err
			}
		}
	}
	if frame.Error != nil {
		if err := check("error.code", frame.Error.Code); err != nil {
			return err
		}
		return check("error.retryable", strconv.FormatBool(frame.Error.Retryable))
	}
	return nil
}

func validateHarnessArtifactStructure(artifact ArtifactRef, check func(string, string) error) error {
	if err := check("artifact.filename", artifact.Filename); err != nil {
		return err
	}
	if err := check("artifact.contentType", artifact.ContentType); err != nil {
		return err
	}
	return check("artifact.size", strconv.FormatInt(artifact.Size, 10))
}

func sanitizeHarnessStringMap(values map[string]string, sanitize func(string) string) map[string]string {
	if values == nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		sanitizedKey := sanitize(key)
		if events.IsSensitiveExecutionEventKey(key) {
			result[sanitizedKey] = sanitize(events.ExecutionEventRedactedValue)
			continue
		}
		result[sanitizedKey] = sanitize(value)
	}
	return result
}

func sanitizeHarnessAnyMap(values map[string]any, sanitize func(string) string) map[string]any {
	if values == nil {
		return nil
	}
	result := make(map[string]any, len(values))
	for key, value := range values {
		sanitizedKey := sanitize(key)
		if events.IsSensitiveExecutionEventKey(key) {
			result[sanitizedKey] = sanitize(events.ExecutionEventRedactedValue)
			continue
		}
		result[sanitizedKey] = sanitizeHarnessAny(value, sanitize)
	}
	return result
}

func sanitizeHarnessAny(value any, sanitize func(string) string) any {
	switch typed := value.(type) {
	case string:
		return sanitize(typed)
	case map[string]any:
		return sanitizeHarnessAnyMap(typed, sanitize)
	case map[string]string:
		return sanitizeHarnessStringMap(typed, sanitize)
	case []any:
		result := make([]any, len(typed))
		for i, child := range typed {
			result[i] = sanitizeHarnessAny(child, sanitize)
		}
		return result
	case []string:
		result := make([]string, len(typed))
		for i, child := range typed {
			result[i] = sanitize(child)
		}
		return result
	case json.Number:
		return sanitizeHarnessScalar(typed, typed.String(), sanitize)
	case float64:
		encoded, _ := json.Marshal(typed)
		return sanitizeHarnessScalar(typed, string(encoded), sanitize)
	case float32:
		encoded, _ := json.Marshal(typed)
		return sanitizeHarnessScalar(typed, string(encoded), sanitize)
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return sanitizeHarnessScalar(typed, fmt.Sprint(typed), sanitize)
	case bool:
		return sanitizeHarnessScalar(typed, strconv.FormatBool(typed), sanitize)
	case nil:
		return sanitizeHarnessScalar(nil, "null", sanitize)
	default:
		return typed
	}
}

func sanitizeHarnessScalar(value any, text string, sanitize func(string) string) any {
	sanitized := sanitize(text)
	if sanitized != text {
		return sanitized
	}
	return value
}

func sanitizeHarnessArtifacts(artifacts []ArtifactRef, sanitize func(string) string) []ArtifactRef {
	if artifacts == nil {
		return nil
	}
	result := make([]ArtifactRef, len(artifacts))
	for i, artifact := range artifacts {
		artifact.Description = sanitize(artifact.Description)
		result[i] = artifact
	}
	return result
}

func harnessJSONContainsBearer(content json.RawMessage, bearer string) (bool, error) {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 {
		return false, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	containsBearer := bearer != "" && bytes.Contains(trimmed, []byte(bearer))
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return containsBearer, nil
		}
		if err != nil {
			return false, err
		}
		text, ok := harnessJSONScalarText(token)
		if ok && StructuredValueContainsBearer(text, bearer) {
			containsBearer = true
		}
	}
}

func sanitizeHarnessJSON(content json.RawMessage, bearer string, sanitize func(string) string) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("trailing data")
	}
	needsSanitization, err := harnessJSONNeedsSanitization(trimmed, sanitize)
	if err != nil {
		return nil, err
	}
	rawContainsBearer := bearer != "" && bytes.Contains(trimmed, []byte(bearer))
	if !needsSanitization && !rawContainsBearer {
		return content, nil
	}
	encoded, err := json.Marshal(sanitizeHarnessAny(value, sanitize))
	if err != nil {
		return nil, err
	}
	if bearer != "" && bytes.Contains(encoded, []byte(bearer)) {
		return nil, fmt.Errorf("sanitized harness frame content still contains configured bearer")
	}
	return json.RawMessage(encoded), nil
}

func harnessJSONNeedsSanitization(content []byte, sanitize func(string) string) (bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	needsSanitization, err := scanHarnessJSONValue(decoder, sanitize)
	if err != nil {
		return false, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return false, fmt.Errorf("trailing data")
	}
	return needsSanitization, nil
}

func scanHarnessJSONValue(decoder *json.Decoder, sanitize func(string) string) (bool, error) {
	token, err := decoder.Token()
	if err != nil {
		return false, err
	}
	if delimiter, ok := token.(json.Delim); ok {
		switch delimiter {
		case '{':
			changed := false
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return false, err
				}
				key, ok := keyToken.(string)
				if !ok {
					return false, fmt.Errorf("invalid object key")
				}
				changed = changed || sanitize(key) != key || events.IsSensitiveExecutionEventKey(key)
				childChanged, err := scanHarnessJSONValue(decoder, sanitize)
				if err != nil {
					return false, err
				}
				changed = changed || childChanged
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return false, fmt.Errorf("invalid object terminator")
			}
			return changed, nil
		case '[':
			changed := false
			for decoder.More() {
				childChanged, err := scanHarnessJSONValue(decoder, sanitize)
				if err != nil {
					return false, err
				}
				changed = changed || childChanged
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return false, fmt.Errorf("invalid array terminator")
			}
			return changed, nil
		default:
			return false, fmt.Errorf("unexpected JSON delimiter %q", delimiter)
		}
	}
	text, ok := harnessJSONScalarText(token)
	return ok && sanitize(text) != text, nil
}

func harnessJSONScalarText(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case json.Number:
		return typed.String(), true
	case bool:
		return strconv.FormatBool(typed), true
	case nil:
		return "null", true
	default:
		return "", false
	}
}

func (c *Client) sanitizeClientError(err error) error {
	if err == nil {
		return nil
	}
	sanitize := func(clientErr ClientError) ClientError {
		clientErr.sanitizedDisplay = ""
		clientErr.sanitizedDisplaySet = false
		clientErr.Message = c.sanitizeClientMessage(clientErr.Message)
		clientErr.sanitizedDisplay = c.sanitizeClientMessage(clientErr.render())
		clientErr.sanitizedDisplaySet = true
		return clientErr
	}
	switch clientErr := err.(type) {
	case ClientError:
		return sanitize(clientErr)
	case *ClientError:
		sanitized := sanitize(*clientErr)
		return &sanitized
	default:
		// Stream callbacks are caller-owned and must retain their dynamic error
		// types and trees. Client-generated remote, transport, and decode errors
		// are always ClientError values and are scrubbed above.
		return err
	}
}

func StructuredValueContainsBearer(value, bearer string) bool {
	if bearer == "" {
		return false
	}
	// Bearer confidentiality is fail-closed: any occurrence is treated as a
	// reflection, even when a short credential could also match coincidentally.
	return strings.Contains(value, bearer)
}

func RedactStructuredBearerValue(value, bearer string) string {
	if !StructuredValueContainsBearer(value, bearer) {
		return value
	}
	return RedactExactBearerValue(value, bearer)
}

func RedactExactBearerValue(message, token string) string {
	if message == "" || token == "" {
		return message
	}
	replacement := events.ExecutionEventRedactedValue
	if strings.Contains(replacement, token) {
		replacement = ""
	}
	message = strings.ReplaceAll(message, token, replacement)
	for range 16 {
		if !strings.Contains(message, token) {
			return message
		}
		message = strings.ReplaceAll(message, token, "")
	}
	// A crafted overlapping value must not escape even if the bounded cleanup
	// above cannot converge quickly enough.
	return ""
}

func (c *Client) setAuthHeader(req *http.Request) {
	if c == nil || req == nil || c.authBearerValue == "" {
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.authBearerValue)
}

func (c *Client) controlContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.controlTimeout <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.controlTimeout)
}

func (c *Client) statusError(op string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	message := strings.TrimSpace(string(body))
	if message == "" {
		message = resp.Status
	}
	clientErr := ClientError{Op: op, StatusCode: resp.StatusCode, Message: events.RedactExecutionEventText(message)}
	return classifyClientError(clientErr, message)
}

func (c *Client) resolve(rel string) *url.URL {
	copy := *c.baseURL
	escapedPath := path.Join(copy.EscapedPath(), rel)
	if strings.HasSuffix(rel, "/") && !strings.HasSuffix(escapedPath, "/") {
		escapedPath += "/"
	}
	decodedPath, err := url.PathUnescape(escapedPath)
	if err != nil {
		decodedPath = escapedPath
		escapedPath = ""
	}
	copy.Path = decodedPath
	copy.RawPath = escapedPath
	return &copy
}

func readSSEFrames(r io.Reader, emit func(HarnessEventFrame) error) error {
	return readSSEFramesWithSanitizers(r, emit, nil, nil)
}

func readSSEFramesWithSanitizer(r io.Reader, emit func(HarnessEventFrame) error, sanitize func(error) error) error {
	return readSSEFramesWithSanitizers(r, emit, sanitize, nil)
}

func readSSEFramesWithSanitizers(
	r io.Reader,
	emit func(HarnessEventFrame) error,
	sanitizeError func(error) error,
	sanitizeFrame func(HarnessEventFrame) (HarnessEventFrame, error),
) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxHarnessSSELineBytes)
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := emitSSEData(data.String(), emit, sanitizeError, sanitizeFrame); err != nil {
				if errors.Is(err, errSSEDone) {
					return nil
				}
				return err
			}
			data.Reset()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if after, ok := strings.CutPrefix(line, "data:"); ok {
			if err := appendSSEData(&data, strings.TrimSpace(after)); err != nil {
				return sanitizeSSEClientError(sanitizeError, err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return sanitizeSSEClientError(sanitizeError, safeClientError("stream_frames", 0, err.Error(), err))
	}
	if data.Len() > 0 {
		if err := emitSSEData(data.String(), emit, sanitizeError, sanitizeFrame); err != nil {
			if errors.Is(err, errSSEDone) {
				return nil
			}
			return err
		}
	}
	return nil
}

func appendSSEData(data *strings.Builder, value string) error {
	additional := len(value)
	if data.Len() > 0 {
		additional++
	}
	if data.Len()+additional > maxHarnessSSEEventBytes {
		return safeClientError("stream_frames", 0, "SSE event exceeds harness frame limit")
	}
	if data.Len() > 0 {
		data.WriteByte('\n')
	}
	data.WriteString(value)
	return nil
}

func sanitizeSSEClientError(sanitize func(error) error, err error) error {
	if sanitize != nil {
		return sanitize(err)
	}
	return err
}

func emitSSEData(
	raw string,
	emit func(HarnessEventFrame) error,
	sanitizeError func(error) error,
	sanitizeFrame func(HarnessEventFrame) (HarnessEventFrame, error),
) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if raw == sseDone {
		return errSSEDone
	}
	var frame HarnessEventFrame
	if err := json.Unmarshal([]byte(raw), &frame); err != nil {
		return sanitizeSSEClientError(
			sanitizeError,
			safeClientError("stream_frames", 0, fmt.Sprintf("decode harness frame: %v", err)),
		)
	}
	if sanitizeFrame != nil {
		sanitized, err := sanitizeFrame(frame)
		if err != nil {
			return sanitizeSSEClientError(sanitizeError, safeClientError("stream_frames", 0, err.Error(), err))
		}
		frame = sanitized
	}
	if err := emit(frame); err != nil {
		return err
	}
	return nil
}

type ClientError struct {
	Op                      string
	StatusCode              int
	Message                 string
	RemoteAccepted          bool
	RemoteAcceptanceUnknown bool
	sanitizedDisplay        string
	sanitizedDisplaySet     bool
	duplicateTurn           bool
	capacityExceeded        bool
	unsupportedVersion      bool
	remoteRejected          bool
	turnNotFound            bool
	protocolViolation       bool
	contextCanceled         bool
	deadlineExceeded        bool
}

// Is reports typed context termination causes without exposing raw error text.
func (e ClientError) Is(target error) bool {
	return target == context.Canceled && e.contextCanceled ||
		target == context.DeadlineExceeded && e.deadlineExceeded
}

// IsDuplicateTurn reports whether the remote deterministically rejected an
// already-started or already-completed turn identity.
func (e ClientError) IsDuplicateTurn() bool { return e.duplicateTurn }

func (e ClientError) IsCapacityExceeded() bool   { return e.capacityExceeded }
func (e ClientError) IsUnsupportedVersion() bool { return e.unsupportedVersion }
func (e ClientError) IsRemoteRejected() bool     { return e.remoteRejected }
func (e ClientError) IsTurnNotFound() bool       { return e.turnNotFound }
func (e ClientError) IsProtocolViolation() bool  { return e.protocolViolation }

func (e ClientError) Error() string {
	if e.sanitizedDisplaySet {
		return e.sanitizedDisplay
	}
	return e.render()
}

func (e ClientError) render() string {
	if e.StatusCode > 0 {
		return fmt.Sprintf("harness %s failed (%d): %s", e.Op, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("harness %s failed: %s", e.Op, e.Message)
}

func acceptedClientError(op, message string) error {
	clientErr := ClientError{
		Op:             op,
		Message:        events.RedactExecutionEventText(message),
		RemoteAccepted: true,
	}
	return classifyClientError(clientErr, message)
}

func unknownAcceptanceClientError(op, message string, causes ...error) error {
	clientErr := ClientError{
		Op:                      op,
		Message:                 events.RedactExecutionEventText(message),
		RemoteAcceptanceUnknown: true,
	}
	return withClientErrorCause(classifyClientError(clientErr, message), causes)
}

func safeClientError(op string, status int, message string, causes ...error) error {
	clientErr := ClientError{Op: op, StatusCode: status, Message: events.RedactExecutionEventText(message)}
	return withClientErrorCause(classifyClientError(clientErr, message), causes)
}

func classifyClientError(clientErr ClientError, message string) ClientError {
	lower := strings.ToLower(clientErr.Op + " " + message)
	clientErr.duplicateTurn = clientErr.StatusCode == http.StatusConflict &&
		(strings.Contains(lower, "turn already exists") || strings.Contains(lower, "turn already completed"))
	clientErr.capacityExceeded = strings.Contains(lower, "maximum concurrent turns")
	clientErr.unsupportedVersion = strings.Contains(lower, "unsupported version") || strings.Contains(lower, "unsupported protocol version")
	clientErr.remoteRejected = strings.Contains(lower, "harness did not accept")
	clientErr.turnNotFound = strings.Contains(lower, "turn not found")
	clientErr.protocolViolation = strings.Contains(lower, "harness frame identity does not match") ||
		strings.Contains(lower, "harness frame structural field") || strings.Contains(lower, "harness frame brokered tool content") ||
		strings.Contains(lower, "harness health structural field") || strings.Contains(lower, "harness capabilities structural field") ||
		strings.Contains(lower, "invalid harness frame") ||
		strings.Contains(lower, "invalid harness frame content json") || strings.Contains(lower, "decode harness frame")
	return clientErr
}

func withClientErrorCause(clientErr ClientError, causes []error) ClientError {
	if len(causes) == 0 || causes[0] == nil {
		return clientErr
	}
	clientErr.contextCanceled = errors.Is(causes[0], context.Canceled)
	clientErr.deadlineExceeded = errors.Is(causes[0], context.DeadlineExceeded)
	return clientErr
}
