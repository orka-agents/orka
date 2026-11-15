package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/coder/websocket"

	"github.com/orka-agents/orka/internal/operatoraccess"
)

// OperatorExecStreams are the local ends of an operator exec.
type OperatorExecStreams struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Controls carries resize and signal requests (Ctrl-C forwarding).
	Controls <-chan operatoraccess.Control
}

func (c *Client) operatorURL(taskName, suffix, scheme string, query url.Values) (string, error) {
	u, err := url.Parse(strings.TrimRight(c.BaseURL, "/") + "/api/v1/tasks/" + url.PathEscape(taskName) + "/" + suffix)
	if err != nil {
		return "", fmt.Errorf("invalid base URL: %w", err)
	}
	if scheme != "" {
		switch u.Scheme {
		case "https":
			u.Scheme = "wss"
		case "http":
			u.Scheme = "ws"
		default:
			return "", fmt.Errorf("unsupported server URL scheme %q", u.Scheme)
		}
	}
	if query == nil {
		query = url.Values{}
	}
	if c.Namespace != "" && query.Get("namespace") == "" {
		query.Set("namespace", c.Namespace)
	}
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func (c *Client) authHeader() http.Header {
	header := http.Header{}
	if c.Token != "" {
		header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.TxnToken != "" {
		header.Set("Txn-Token", c.TxnToken)
	}
	return header
}

func operatorAPIError(response *http.Response, fallback error) error {
	if response == nil {
		return fallback
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
	message := strings.TrimSpace(string(body))
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil && payload.Error.Message != "" {
		message = payload.Error.Message
	}
	if message == "" {
		message = http.StatusText(response.StatusCode)
	}
	return fmt.Errorf("API error (HTTP %d): %s", response.StatusCode, message)
}

// OperatorExec runs one command in the Task's RuntimeSession workspace and
// returns its final status. The command is sent as the first stream message
// so argv never appears in URLs or headers.
func (c *Client) OperatorExec(ctx context.Context, taskName string, options operatoraccess.ExecOptions, streams OperatorExecStreams) (operatoraccess.Status, error) {
	if err := options.Normalize(); err != nil {
		return operatoraccess.Status{}, err
	}
	endpoint, err := c.operatorURL(taskName, "exec", "ws", nil)
	if err != nil {
		return operatoraccess.Status{}, err
	}
	conn, response, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPClient: c.HTTPClient, HTTPHeader: c.authHeader(),
		Subprotocols: []string{operatoraccess.WebSocketSubprotocol},
	})
	if err != nil {
		return operatoraccess.Status{}, operatorAPIError(response, fmt.Errorf("connect operator exec stream: %w", err))
	}
	defer conn.CloseNow() //nolint:errcheck
	conn.SetReadLimit(operatoraccess.MaxFrameBytes + 1)
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	request, err := json.Marshal(map[string]any{"type": "exec", "exec": options})
	if err != nil {
		return operatoraccess.Status{}, err
	}
	var writeMu sync.Mutex
	write := func(frame []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.Write(streamCtx, websocket.MessageBinary, frame)
	}
	if err := write(operatoraccess.Frame(operatoraccess.ChannelControl, request)); err != nil {
		return operatoraccess.Status{}, fmt.Errorf("send exec request: %w", err)
	}
	// Input that cannot be read is not the end of the input. Telling the
	// remote command it was would let it finish on truncated input and report
	// success, so a failed read ends the exec instead.
	stdinFailure := make(chan error, 1)
	if options.Stdin && streams.Stdin != nil {
		go func() {
			if err := pumpOperatorStdin(streams.Stdin, options.TTY, write); err != nil {
				stdinFailure <- err
				cancel()
			}
		}()
	}
	if streams.Controls != nil {
		go pumpOperatorControls(streamCtx, streams.Controls, write)
	}
	for {
		kind, payload, err := conn.Read(streamCtx)
		if err != nil {
			if ctx.Err() != nil {
				return operatoraccess.Status{}, ctx.Err()
			}
			select {
			case stdinErr := <-stdinFailure:
				return operatoraccess.Status{}, fmt.Errorf("read command stdin: %w", stdinErr)
			default:
			}
			var closeErr websocket.CloseError
			if errors.As(err, &closeErr) && closeErr.Reason != "" {
				return operatoraccess.Status{}, fmt.Errorf("operator exec stream closed: %s", closeErr.Reason)
			}
			return operatoraccess.Status{}, fmt.Errorf("operator exec stream closed before the command finished")
		}
		if kind != websocket.MessageBinary || len(payload) == 0 {
			continue
		}
		// Output that cannot be written locally is lost, so the exec is not a
		// success whatever the remote command goes on to return. Returning
		// closes the stream, which stops the command.
		switch payload[0] {
		case operatoraccess.ChannelStdout:
			if err := writeOperatorOutput(streams.Stdout, payload[1:]); err != nil {
				return operatoraccess.Status{}, fmt.Errorf("write command stdout: %w", err)
			}
		case operatoraccess.ChannelStderr:
			if err := writeOperatorOutput(streams.Stderr, payload[1:]); err != nil {
				return operatoraccess.Status{}, fmt.Errorf("write command stderr: %w", err)
			}
		case operatoraccess.ChannelStatus:
			var status operatoraccess.Status
			if err := json.Unmarshal(payload[1:], &status); err != nil {
				return operatoraccess.Status{}, fmt.Errorf("decode exec status: %w", err)
			}
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return status, nil
		}
	}
}

func writeOperatorOutput(w io.Writer, data []byte) error {
	if w == nil {
		return nil
	}
	_, err := w.Write(data)
	return err
}

// pumpOperatorStdin forwards stdin until it ends. Only a real end of input is
// announced to the remote command; any other read error is returned.
func pumpOperatorStdin(stdin io.Reader, tty bool, write func([]byte) error) error {
	buffer := make([]byte, 16<<10)
	for {
		n, readErr := stdin.Read(buffer)
		if n > 0 && write(operatoraccess.Frame(operatoraccess.ChannelStdin, buffer[:n])) != nil {
			return nil
		}
		switch {
		case readErr == nil:
		case errors.Is(readErr, io.EOF):
			if !tty {
				_ = write(operatoraccess.ControlFrame(operatoraccess.Control{Type: operatoraccess.ControlEOF}))
			}
			return nil
		default:
			return readErr
		}
	}
}

func pumpOperatorControls(ctx context.Context, controls <-chan operatoraccess.Control, write func([]byte) error) {
	for {
		select {
		case <-ctx.Done():
			return
		case control, ok := <-controls:
			if !ok || write(operatoraccess.ControlFrame(control)) != nil {
				return
			}
		}
	}
}

// OperatorCopyFrom copies one regular workspace file into w.
func (c *Client) OperatorCopyFrom(ctx context.Context, taskName, path string, maxBytes int64, w io.Writer) (int64, error) {
	query := url.Values{"path": {path}}
	if maxBytes > 0 {
		query.Set("maxBytes", strconv.FormatInt(maxBytes, 10))
	}
	endpoint, err := c.operatorURL(taskName, "files", "", query)
	if err != nil {
		return 0, err
	}
	request, err := c.newRequest(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	response, err := c.HTTPClient.Do(request)
	if err != nil {
		return 0, fmt.Errorf("request failed: %w", err)
	}
	defer response.Body.Close() //nolint:errcheck
	if response.StatusCode != http.StatusOK {
		return 0, operatorAPIError(response, nil)
	}
	copied, err := io.Copy(w, response.Body)
	if err != nil {
		return copied, fmt.Errorf("copy failed after %d bytes: %w", copied, err)
	}
	if response.ContentLength >= 0 && copied != response.ContentLength {
		return copied, fmt.Errorf("copy ended after %d of %d bytes", copied, response.ContentLength)
	}
	return copied, nil
}

// OperatorCopyTo writes exactly size bytes from r into one workspace file.
func (c *Client) OperatorCopyTo(ctx context.Context, taskName, path string, r io.Reader, size int64) (int64, error) {
	endpoint, err := c.operatorURL(taskName, "files", "", url.Values{"path": {path}, "size": {strconv.FormatInt(size, 10)}})
	if err != nil {
		return 0, err
	}
	request, err := c.newRequest(ctx, http.MethodPut, endpoint, io.LimitReader(r, size))
	if err != nil {
		return 0, err
	}
	request.ContentLength = size
	if size == 0 {
		request.Body = http.NoBody
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err := c.HTTPClient.Do(request)
	if err != nil {
		return 0, fmt.Errorf("request failed: %w", err)
	}
	defer response.Body.Close() //nolint:errcheck
	if response.StatusCode != http.StatusOK {
		return 0, operatorAPIError(response, nil)
	}
	var result struct {
		Bytes int64 `json:"bytes"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<10)).Decode(&result); err != nil {
		return 0, fmt.Errorf("decode upload result: %w", err)
	}
	return result.Bytes, nil
}
