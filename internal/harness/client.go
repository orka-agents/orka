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
		return nil, safeClientError("health", 0, err.Error())
	}
	return &response, nil
}

func (c *Client) Capabilities(ctx context.Context) (_ *CapabilitiesResponse, err error) {
	defer func() { err = c.sanitizeClientError(err) }()
	var response CapabilitiesResponse
	if err := c.getJSON(ctx, CapabilitiesPath, &response); err != nil {
		return nil, err
	}
	if err := response.Validate(); err != nil {
		return nil, safeClientError("capabilities", 0, err.Error())
	}
	return &response, nil
}

type startTurnResponseWire struct {
	StartTurnResponse
	Accepted *bool `json:"accepted"`
}

func (c *Client) StartTurn(ctx context.Context, request StartTurnRequest) (_ *StartTurnResponse, err error) {
	defer func() { err = c.sanitizeClientError(err) }()
	if err := request.Validate(); err != nil {
		return nil, safeClientError("start_turn", 0, err.Error())
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
	return &response, nil
}

func (c *Client) CancelTurn(ctx context.Context, request CancelTurnRequest) (_ *CancelTurnResponse, err error) {
	defer func() { err = c.sanitizeClientError(err) }()
	if err := request.Validate(); err != nil {
		return nil, safeClientError("cancel_turn", 0, err.Error())
	}
	rel, err := CancelTurnPath(request.TurnID)
	if err != nil {
		return nil, safeClientError("cancel_turn", 0, err.Error())
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
	return &response, nil
}

func (c *Client) ContinueTurn(ctx context.Context, request ContinueTurnRequest) (_ *ContinueTurnResponse, err error) {
	defer func() { err = c.sanitizeClientError(err) }()
	if err := request.Validate(); err != nil {
		return nil, safeClientError("continue_turn", 0, err.Error())
	}
	rel, err := ContinueTurnPath(request.TurnID)
	if err != nil {
		return nil, safeClientError("continue_turn", 0, err.Error())
	}
	var response ContinueTurnResponse
	if err := c.postJSON(ctx, rel, request, &response); err != nil {
		return nil, err
	}
	if err := response.ValidateFor(request); err != nil {
		return nil, safeClientError("continue_turn", 0, err.Error())
	}
	return &response, nil
}

func (c *Client) FetchTurnOutput(ctx context.Context, turnID HarnessTurnID, outputRef string) (_ []byte, err error) {
	defer func() { err = c.sanitizeClientError(err) }()
	ctx, cancel := c.controlContext(ctx)
	defer cancel()
	rel, err := OutputTurnPath(turnID)
	if err != nil {
		return nil, safeClientError("fetch_turn_output", 0, err.Error())
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
		return nil, safeClientError("fetch_turn_output", 0, err.Error())
	}
	req.Header.Set("Accept", "application/octet-stream")
	c.setAuthHeader(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, safeClientError("fetch_turn_output", 0, err.Error())
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, c.statusError("fetch_turn_output", resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchTurnOutputBytes+1))
	if err != nil {
		return nil, safeClientError("fetch_turn_output", resp.StatusCode, err.Error())
	}
	if len(data) > maxFetchTurnOutputBytes {
		return nil, safeClientError("fetch_turn_output", resp.StatusCode, "output exceeds harness fetch limit")
	}
	return data, nil
}

func (c *Client) StreamFrames(ctx context.Context, turnID HarnessTurnID, afterSeq int64, emit func(HarnessEventFrame) error) error {
	rel, err := EventStreamPath(turnID)
	if err != nil {
		return c.sanitizeClientError(safeClientError("stream_frames", 0, err.Error()))
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
		return c.sanitizeClientError(safeClientError("stream_frames", 0, err.Error()))
	}
	req.Header.Set("Accept", "text/event-stream")
	c.setAuthHeader(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return c.sanitizeClientError(safeClientError("stream_frames", 0, err.Error()))
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.sanitizeClientError(c.statusError("stream_frames", resp))
	}
	return readSSEFramesWithSanitizer(resp.Body, emit, c.sanitizeClientError)
}

func (c *Client) getJSON(ctx context.Context, rel string, out any) error {
	ctx, cancel := c.controlContext(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.resolve(rel).String(), nil)
	if err != nil {
		return safeClientError("get", 0, err.Error())
	}
	req.Header.Set("Accept", "application/json")
	c.setAuthHeader(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return safeClientError("get", 0, err.Error())
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
		return safeClientError("post", 0, err.Error())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.resolve(rel).String(), bytes.NewReader(payload))
	if err != nil {
		return safeClientError("post", 0, err.Error())
	}
	req.Header.Set("Accept", "application/json")
	c.setAuthHeader(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return unknownAcceptanceClientError("post", err.Error())
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
		return safeClientError(op, status, err.Error())
	}
	if len(data) > maxHarnessControlResponseBytes {
		return safeClientError(op, status, "JSON response exceeds harness control limit")
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return safeClientError(op, status, err.Error())
	}
	return nil
}

func (c *Client) sanitizeClientMessage(message string) string {
	message = events.RedactExecutionEventText(message)
	if c != nil {
		message = redactExactBearerValue(message, c.authBearerValue)
	}
	return message
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

func redactExactBearerValue(message, token string) string {
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
	return safeClientError(op, resp.StatusCode, message)
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
	return readSSEFramesWithSanitizer(r, emit, nil)
}

func readSSEFramesWithSanitizer(r io.Reader, emit func(HarnessEventFrame) error, sanitize func(error) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxHarnessSSELineBytes)
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := emitSSEData(data.String(), emit, sanitize); err != nil {
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
				return sanitizeSSEClientError(sanitize, err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return sanitizeSSEClientError(sanitize, safeClientError("stream_frames", 0, err.Error()))
	}
	if data.Len() > 0 {
		if err := emitSSEData(data.String(), emit, sanitize); err != nil {
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

func emitSSEData(raw string, emit func(HarnessEventFrame) error, sanitize func(error) error) error {
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
			sanitize,
			safeClientError("stream_frames", 0, fmt.Sprintf("decode harness frame: %v", err)),
		)
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
}

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
	return ClientError{
		Op:             op,
		Message:        events.RedactExecutionEventText(message),
		RemoteAccepted: true,
	}
}

func unknownAcceptanceClientError(op, message string) error {
	return ClientError{
		Op:                      op,
		Message:                 events.RedactExecutionEventText(message),
		RemoteAcceptanceUnknown: true,
	}
}

func safeClientError(op string, status int, message string) error {
	return ClientError{Op: op, StatusCode: status, Message: events.RedactExecutionEventText(message)}
}
