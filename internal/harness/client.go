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

	"github.com/sozercan/orka/internal/events"
)

const maxFetchTurnOutputBytes = 50 << 20

type Client struct {
	baseURL         *url.URL
	httpClient      *http.Client
	controlTimeout  time.Duration
	authBearerValue string
}

const maxHarnessSSEFrameBytes = 1 << 20

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

func (c *Client) Health(ctx context.Context) (*HealthResponse, error) {
	var response HealthResponse
	if err := c.getJSON(ctx, HealthPath, &response); err != nil {
		return nil, err
	}
	if err := response.Validate(); err != nil {
		return nil, safeClientError("health", 0, err.Error())
	}
	return &response, nil
}

func (c *Client) Capabilities(ctx context.Context) (*CapabilitiesResponse, error) {
	var response CapabilitiesResponse
	if err := c.getJSON(ctx, CapabilitiesPath, &response); err != nil {
		return nil, err
	}
	if err := response.Validate(); err != nil {
		return nil, safeClientError("capabilities", 0, err.Error())
	}
	return &response, nil
}

func (c *Client) StartTurn(ctx context.Context, request StartTurnRequest) (*StartTurnResponse, error) {
	if err := request.Validate(); err != nil {
		return nil, safeClientError("start_turn", 0, err.Error())
	}
	var response StartTurnResponse
	if err := c.postJSON(ctx, TurnsPath, request, &response); err != nil {
		return nil, err
	}
	if strings.TrimSpace(response.Version) != ProtocolVersion {
		return nil, safeClientError("start_turn", 0, fmt.Sprintf("unsupported version %q", response.Version))
	}
	if !response.Accepted {
		return nil, safeClientError("start_turn", 0, "harness did not accept turn")
	}
	if err := validateAcceptedTurn(request, response); err != nil {
		return nil, safeClientError("start_turn", 0, err.Error())
	}
	return &response, nil
}

func (c *Client) CancelTurn(ctx context.Context, request CancelTurnRequest) (*CancelTurnResponse, error) {
	if err := request.Validate(); err != nil {
		return nil, safeClientError("cancel_turn", 0, err.Error())
	}
	if err := validateHarnessTurnPathID(request.TurnID); err != nil {
		return nil, safeClientError("cancel_turn", 0, err.Error())
	}
	var response CancelTurnResponse
	if err := c.postJSON(ctx, turnPath(request.TurnID, "cancel"), request, &response); err != nil {
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

func (c *Client) FetchTurnOutput(ctx context.Context, turnID HarnessTurnID, outputRef string) ([]byte, error) {
	ctx, cancel := c.controlContext(ctx)
	defer cancel()
	if strings.TrimSpace(string(turnID)) == "" {
		return nil, safeClientError("fetch_turn_output", 0, "turn id is required")
	}
	if err := validateHarnessTurnPathID(turnID); err != nil {
		return nil, safeClientError("fetch_turn_output", 0, err.Error())
	}
	if strings.TrimSpace(outputRef) == "" {
		return nil, safeClientError("fetch_turn_output", 0, "output ref is required")
	}
	rel := turnPath(turnID, "output")
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
	if strings.TrimSpace(string(turnID)) == "" {
		return safeClientError("stream_frames", 0, "turn id is required")
	}
	if err := validateHarnessTurnPathID(turnID); err != nil {
		return safeClientError("stream_frames", 0, err.Error())
	}
	if emit == nil {
		return safeClientError("stream_frames", 0, "emit callback is required")
	}
	rel := turnPath(turnID, "events")
	u := c.resolve(rel)
	q := u.Query()
	if afterSeq > 0 {
		q.Set("afterSeq", strconv.FormatInt(afterSeq, 10))
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return safeClientError("stream_frames", 0, err.Error())
	}
	req.Header.Set("Accept", "text/event-stream")
	c.setAuthHeader(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return safeClientError("stream_frames", 0, err.Error())
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.statusError("stream_frames", resp)
	}
	return readSSEFrames(resp.Body, emit)
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
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return safeClientError("get", resp.StatusCode, err.Error())
	}
	return nil
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
		return safeClientError("post", 0, err.Error())
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.statusError("post", resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return safeClientError("post", resp.StatusCode, err.Error())
	}
	return nil
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
	if _, ok := ctx.Deadline(); ok {
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
	copy.Path = path.Join(copy.Path, rel)
	if strings.HasSuffix(rel, "/") && !strings.HasSuffix(copy.Path, "/") {
		copy.Path += "/"
	}
	return &copy
}

func turnPath(turnID HarnessTurnID, suffix string) string {
	base := strings.TrimRight(TurnsPath, "/") + "/" + url.PathEscape(strings.TrimSpace(string(turnID)))
	if strings.TrimSpace(suffix) == "" {
		return base
	}
	return base + "/" + strings.Trim(strings.TrimSpace(suffix), "/")
}

func validateHarnessTurnPathID(turnID HarnessTurnID) error {
	value := strings.TrimSpace(string(turnID))
	if value == "." || value == ".." {
		return fmt.Errorf("turn id must not be a dot path segment")
	}
	return nil
}

func readSSEFrames(r io.Reader, emit func(HarnessEventFrame) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxHarnessSSEFrameBytes)
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := emitSSEData(data.String(), emit); err != nil {
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
		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if data.Len() > 0 {
		if err := emitSSEData(data.String(), emit); err != nil {
			if errors.Is(err, errSSEDone) {
				return nil
			}
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return safeClientError("stream_frames", 0, err.Error())
	}
	return nil
}

func emitSSEData(raw string, emit func(HarnessEventFrame) error) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if raw == sseDone {
		return errSSEDone
	}
	var frame HarnessEventFrame
	if err := json.Unmarshal([]byte(raw), &frame); err != nil {
		return safeClientError("stream_frames", 0, fmt.Sprintf("decode harness frame: %v", err))
	}
	if err := emit(frame); err != nil {
		return err
	}
	return nil
}

type ClientError struct {
	Op         string
	StatusCode int
	Message    string
}

func (e ClientError) Error() string {
	if e.StatusCode > 0 {
		return fmt.Sprintf("harness %s failed (%d): %s", e.Op, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("harness %s failed: %s", e.Op, e.Message)
}

func safeClientError(op string, status int, message string) error {
	return ClientError{Op: op, StatusCode: status, Message: events.RedactExecutionEventText(message)}
}
