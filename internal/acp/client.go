package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
)

const DefaultMaxMessageBytes = 8 << 20

// DefaultMaxConcurrentRequests bounds concurrently handled adapter-initiated
// requests per connection. Legitimate adapters issue at most a handful of
// concurrent permission or filesystem requests; the bound exists to stop an
// untrusted child from pinning unbounded blocked handler goroutines.
const DefaultMaxConcurrentRequests = 32

var ErrClosed = errors.New("ACP connection closed")

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("ACP JSON-RPC error %d: %s", e.Code, e.Message)
}

type IncomingRequest struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
}

type IncomingNotification struct {
	Method string
	Params json.RawMessage
}

type RequestHandler func(context.Context, IncomingRequest) (any, *RPCError)
type NotificationHandler func(context.Context, IncomingNotification)

type Options struct {
	MaxMessageBytes int
	// MaxConcurrentRequests bounds concurrently handled adapter-initiated
	// requests. Handlers can block on permission resolution, so an unbounded
	// flood from an untrusted child would pin one goroutine per message and
	// exhaust supervisor memory. Defaults to DefaultMaxConcurrentRequests.
	MaxConcurrentRequests int
	RequestHandler        RequestHandler
	NotificationHandler   NotificationHandler
}

type Client struct {
	reader *bufio.Reader
	writer io.Writer
	opts   Options

	requestGate chan struct{}
	rejectGate  chan struct{}

	writeMu sync.Mutex
	nextID  atomic.Int64

	pendingMu sync.Mutex
	pending   map[string]chan rpcResponse

	closeOnce sync.Once
	closed    chan struct{}
	done      chan struct{}
	errMu     sync.Mutex
	err       error
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type rpcResponse struct {
	result json.RawMessage
	err    error
}

func NewClient(reader io.Reader, writer io.Writer, opts Options) *Client {
	if opts.MaxMessageBytes <= 0 {
		opts.MaxMessageBytes = DefaultMaxMessageBytes
	}
	if opts.MaxConcurrentRequests <= 0 {
		opts.MaxConcurrentRequests = DefaultMaxConcurrentRequests
	}
	client := &Client{
		reader:      bufio.NewReaderSize(reader, min(opts.MaxMessageBytes, 64<<10)),
		writer:      writer,
		opts:        opts,
		requestGate: make(chan struct{}, opts.MaxConcurrentRequests),
		rejectGate:  make(chan struct{}, opts.MaxConcurrentRequests),
		pending:     make(map[string]chan rpcResponse),
		closed:      make(chan struct{}),
		done:        make(chan struct{}),
	}
	client.nextID.Store(0)
	go client.readLoop()
	return client
}

func (c *Client) Done() <-chan struct{} { return c.done }

func (c *Client) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.err
}

func (c *Client) Close() error {
	c.fail(ErrClosed)
	return nil
}

func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	return c.call(ctx, method, params, result, nil)
}

// CallWithWritten is Call with a non-blocking callback invoked after the complete
// JSON-RPC request has been written to the adapter transport. The callback is the
// caller's durable ambiguity boundary: after it fires, loss of the response must
// not be treated as proof that the operation was never accepted.
func (c *Client) CallWithWritten(ctx context.Context, method string, params, result any, onWritten func()) error {
	return c.call(ctx, method, params, result, onWritten)
}

func (c *Client) call(ctx context.Context, method string, params, result any, onWritten func()) error {
	if method == "" {
		return fmt.Errorf("ACP method is required")
	}
	id := c.nextID.Add(1)
	idRaw := json.RawMessage(strconv.FormatInt(id, 10))
	paramsRaw, err := marshalOptional(params)
	if err != nil {
		return fmt.Errorf("marshal ACP %s request: %w", method, err)
	}
	responseCh := make(chan rpcResponse, 1)
	key := string(idRaw)
	c.pendingMu.Lock()
	select {
	case <-c.closed:
		c.pendingMu.Unlock()
		return c.closedError()
	default:
	}
	c.pending[key] = responseCh
	c.pendingMu.Unlock()

	request := rpcMessage{JSONRPC: "2.0", ID: idRaw, Method: method, Params: paramsRaw}
	// The request write blocks indefinitely when the adapter stops reading
	// stdin, so it must honor cancellation like the response wait below: run
	// it in a goroutine and abandon it on ctx expiry. If the abandoned write
	// completes later, a courtesy cancel follows it on the same ordered pipe
	// so the adapter does not keep executing a request whose caller gave up.
	writeDone := make(chan error, 1)
	go func() { writeDone <- c.writeMessage(request) }()
	select {
	case err := <-writeDone:
		if err != nil {
			c.removePending(key)
			return err
		}
	case <-ctx.Done():
		c.removePending(key)
		go func() {
			if err := <-writeDone; err == nil {
				_ = c.Notify(context.Background(), MethodCancelRequest, CancelRequestNotification{RequestID: idRaw})
			}
		}()
		return ctx.Err()
	case <-c.closed:
		c.removePending(key)
		return c.closedError()
	}
	if onWritten != nil {
		onWritten()
	}

	select {
	case response := <-responseCh:
		if response.err != nil {
			return response.err
		}
		if result == nil || len(response.result) == 0 || bytes.Equal(response.result, []byte("null")) {
			return nil
		}
		if err := json.Unmarshal(response.result, result); err != nil {
			return fmt.Errorf("decode ACP %s response: %w", method, err)
		}
		return nil
	case <-ctx.Done():
		c.removePending(key)
		// Best-effort courtesy cancel: it must not block the caller past its
		// already-expired deadline when the adapter stopped reading stdin, so
		// it is dispatched asynchronously; adapter exit unblocks the write.
		go func() {
			_ = c.Notify(context.Background(), MethodCancelRequest, CancelRequestNotification{RequestID: idRaw})
		}()
		return ctx.Err()
	case <-c.closed:
		c.removePending(key)
		return c.closedError()
	}
}

func (c *Client) Notify(ctx context.Context, method string, params any) error {
	if method == "" {
		return fmt.Errorf("ACP method is required")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	paramsRaw, err := marshalOptional(params)
	if err != nil {
		return fmt.Errorf("marshal ACP %s notification: %w", method, err)
	}
	// The transport write blocks indefinitely when the adapter stops reading
	// stdin, so it must honor cancellation: run it in a goroutine and abandon
	// it when ctx expires. An abandoned write holds writeMu until adapter
	// exit closes the pipe, which unblocks the write and ends the goroutine.
	written := make(chan error, 1)
	go func() {
		written <- c.writeMessage(rpcMessage{JSONRPC: "2.0", Method: method, Params: paramsRaw})
	}()
	select {
	case err := <-written:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return c.closedError()
	}
}

func (c *Client) Initialize(ctx context.Context, request InitializeRequest) (InitializeResponse, error) {
	if request.ProtocolVersion == 0 {
		request.ProtocolVersion = ProtocolVersion
	}
	var response InitializeResponse
	if err := c.Call(ctx, MethodInitialize, request, &response); err != nil {
		return InitializeResponse{}, err
	}
	if response.ProtocolVersion != ProtocolVersion {
		return InitializeResponse{}, fmt.Errorf("ACP protocol version %d is unsupported; require %d", response.ProtocolVersion, ProtocolVersion)
	}
	return response, nil
}

func (c *Client) Authenticate(ctx context.Context, methodID string) error {
	if methodID == "" {
		return fmt.Errorf("ACP authentication method ID is required")
	}
	var response AuthenticateResponse
	return c.Call(ctx, MethodAuthenticate, AuthenticateRequest{MethodID: methodID}, &response)
}

func (c *Client) NewSession(ctx context.Context, request NewSessionRequest) (NewSessionResponse, error) {
	var response NewSessionResponse
	if err := c.Call(ctx, MethodSessionNew, request, &response); err != nil {
		return NewSessionResponse{}, err
	}
	if response.SessionID == "" {
		return NewSessionResponse{}, fmt.Errorf("ACP session/new response omitted sessionId")
	}
	return response, nil
}

func (c *Client) Prompt(ctx context.Context, request PromptRequest) (PromptResponse, error) {
	return c.PromptWithWritten(ctx, request, nil)
}

func (c *Client) PromptWithWritten(ctx context.Context, request PromptRequest, onWritten func()) (PromptResponse, error) {
	var response PromptResponse
	if err := c.CallWithWritten(ctx, MethodSessionPrompt, request, &response, onWritten); err != nil {
		return PromptResponse{}, err
	}
	if err := response.StopReason.Validate(); err != nil {
		return PromptResponse{}, err
	}
	return response, nil
}

func (c *Client) Cancel(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("ACP session ID is required")
	}
	return c.Notify(ctx, MethodSessionCancel, CancelNotification{SessionID: sessionID})
}

func (c *Client) readLoop() {
	defer close(c.done)
	for {
		line, err := readLineLimited(c.reader, c.opts.MaxMessageBytes)
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) == 0 {
				c.fail(io.EOF)
			} else {
				c.fail(fmt.Errorf("read ACP message: %w", err))
			}
			return
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var message rpcMessage
		if err := json.Unmarshal(line, &message); err != nil {
			c.fail(fmt.Errorf("decode ACP JSON-RPC message: %w", err))
			return
		}
		if message.JSONRPC != "2.0" {
			c.fail(fmt.Errorf("unsupported ACP JSON-RPC version %q", message.JSONRPC))
			return
		}
		switch {
		case len(message.ID) > 0 && message.Method == "":
			c.deliverResponse(message)
		case len(message.ID) > 0 && message.Method != "":
			c.dispatchRequest(message)
		case message.Method != "":
			if handler := c.opts.NotificationHandler; handler != nil {
				handler(context.Background(), IncomingNotification{Method: message.Method, Params: cloneRaw(message.Params)})
			}
		default:
			c.fail(fmt.Errorf("invalid ACP JSON-RPC envelope"))
			return
		}
	}
}

func (c *Client) deliverResponse(message rpcMessage) {
	key := canonicalID(message.ID)
	c.pendingMu.Lock()
	responseCh, ok := c.pending[key]
	if ok {
		delete(c.pending, key)
	}
	c.pendingMu.Unlock()
	if !ok {
		return
	}
	if message.Error != nil {
		responseCh <- rpcResponse{err: message.Error}
		return
	}
	responseCh <- rpcResponse{result: cloneRaw(message.Result)}
}

// dispatchRequest bounds concurrently handled adapter-initiated requests.
// Handlers can block until permission resolution or prompt settlement, so
// excess requests are rejected with a JSON-RPC error through a second bounded
// lane instead of spawning more blocked handler goroutines; when even the
// rejection lane is saturated the request is dropped, because a peer flooding
// both lanes is not reading responses anyway.
func (c *Client) dispatchRequest(message rpcMessage) {
	select {
	case c.requestGate <- struct{}{}:
		go func() {
			defer func() { <-c.requestGate }()
			c.handleRequest(message)
		}()
		return
	default:
	}
	select {
	case c.rejectGate <- struct{}{}:
		id := cloneRaw(message.ID)
		go func() {
			defer func() { <-c.rejectGate }()
			response := rpcMessage{JSONRPC: "2.0", ID: id, Error: &RPCError{Code: -32000, Message: "too many concurrent requests"}}
			if err := c.writeMessage(response); err != nil {
				c.fail(err)
			}
		}()
	default:
	}
}

func (c *Client) handleRequest(message rpcMessage) {
	request := IncomingRequest{ID: cloneRaw(message.ID), Method: message.Method, Params: cloneRaw(message.Params)}
	var result any
	var rpcErr *RPCError
	if handler := c.opts.RequestHandler; handler != nil {
		result, rpcErr = handler(context.Background(), request)
	} else {
		rpcErr = &RPCError{Code: -32601, Message: "method not supported"}
	}
	response := rpcMessage{JSONRPC: "2.0", ID: cloneRaw(message.ID), Error: rpcErr}
	if rpcErr == nil {
		raw, err := marshalOptional(result)
		if err != nil {
			response.Error = &RPCError{Code: -32603, Message: "failed to encode client response"}
		} else {
			response.Result = raw
		}
	}
	if err := c.writeMessage(response); err != nil {
		c.fail(err)
	}
}

func (c *Client) writeMessage(message rpcMessage) error {
	select {
	case <-c.closed:
		return c.closedError()
	default:
	}
	data, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode ACP JSON-RPC message: %w", err)
	}
	if len(data) > c.opts.MaxMessageBytes {
		return fmt.Errorf("ACP message exceeds %d-byte limit", c.opts.MaxMessageBytes)
	}
	data = append(data, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.writer.Write(data); err != nil {
		c.fail(fmt.Errorf("write ACP message: %w", err))
		return c.closedError()
	}
	return nil
}

func (c *Client) fail(err error) {
	if err == nil {
		err = ErrClosed
	}
	c.closeOnce.Do(func() {
		c.errMu.Lock()
		c.err = err
		c.errMu.Unlock()
		close(c.closed)

		c.pendingMu.Lock()
		pending := c.pending
		c.pending = make(map[string]chan rpcResponse)
		c.pendingMu.Unlock()
		for _, responseCh := range pending {
			responseCh <- rpcResponse{err: err}
		}
	})
}

func (c *Client) closedError() error {
	if err := c.Err(); err != nil {
		return err
	}
	return ErrClosed
}

func (c *Client) removePending(key string) {
	c.pendingMu.Lock()
	delete(c.pending, key)
	c.pendingMu.Unlock()
}

func marshalOptional(value any) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func cloneRaw(raw json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), raw...)
}

func canonicalID(raw json.RawMessage) string {
	return string(bytes.TrimSpace(raw))
}

func readLineLimited(reader *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		fragment, more, err := reader.ReadLine()
		if len(line)+len(fragment) > max {
			return nil, fmt.Errorf("ACP message exceeds %d-byte limit", max)
		}
		line = append(line, fragment...)
		if err != nil {
			return line, err
		}
		if !more {
			return line, nil
		}
	}
}
