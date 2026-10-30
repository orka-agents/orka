/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package ahphost

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/coder/websocket"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"

	"github.com/orka-agents/orka/internal/store"
)

const (
	closeTokenInvalid = websocket.StatusPolicyViolation
	closeGoingAway    = websocket.StatusGoingAway
	closeSlowClient   = websocket.StatusTryAgainLater

	maxFrameBytes               = 4 << 20
	maxClientIDLength           = 256
	maxURILength                = 2048
	maxWatchesPerConnection     = 64
	defaultListLimit            = 100
	maxListLimit                = 1000
	writeTimeout                = 10 * time.Second
	defaultHandledQueue         = 256
	defaultDialect              = "default"
	jsonRPCVersion              = "2.0"
	methodAction                = "action"
	methodSessionAdded          = "root/sessionAdded"
	methodSessionRemoved        = "root/sessionRemoved"
	methodSessionSummaryChanged = "root/sessionSummaryChanged"
	extensionMethodPrefix       = "extensions/"
)

type rpcError struct {
	Code    int32           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

func rpcErr(code int32, format string, args ...any) *rpcError {
	return &rpcError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// reconnectReplay and reconnectSnapshot encode the two reconnect results
// with their type discriminator, which the generated variants omit.
type reconnectReplay struct {
	Type    string                    `json:"type"`
	Actions []ahptypes.ActionEnvelope `json:"actions"`
	Missing []string                  `json:"missing"`
}

type reconnectSnapshot struct {
	Type      string              `json:"type"`
	Snapshots []ahptypes.Snapshot `json:"snapshots"`
}

// inbound is a JSON-RPC request or notification. IDs are echoed as sent.
type inbound struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type outbound struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// conn is one client connection. Commands run on the read loop in order;
// writes go through a bounded queue drained by one writer.
type conn struct {
	host      *Host
	principal Principal
	ws        *websocket.Conn
	send      chan frame
	done      chan struct{}
	closeOnce sync.Once

	// handled runs backends' side effects of client actions one at a time, in
	// the order the client sent them, off the read loop.
	handled chan func()

	mu          sync.Mutex
	initialized bool
	clientID    string
	version     string
	dialect     string
	subs        map[string]bool
	// watches are the connection's resource watches, by channel.
	watches map[string]context.CancelFunc
}

func newConn(host *Host, principal Principal, ws *websocket.Conn) *conn {
	return &conn{
		host:      host,
		principal: principal,
		ws:        ws,
		send:      make(chan frame, host.cfg.SendQueue),
		handled:   make(chan func(), defaultHandledQueue),
		done:      make(chan struct{}),
		subs:      map[string]bool{},
		watches:   map[string]context.CancelFunc{},
	}
}

// pingLoop keeps an idle connection alive through proxies that close quiet
// ones, and closes a connection whose client stopped answering.
func (c *conn) pingLoop(ctx context.Context) {
	interval := c.host.cfg.PingInterval
	if interval <= 0 {
		interval = defaultPingInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		pingCtx, cancel := context.WithTimeout(ctx, interval)
		err := c.ws.Ping(pingCtx)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				c.closeWith(closeGoingAway, "the client stopped answering pings")
			}
			return
		}
	}
}

// run serves the connection until it closes.
func (c *conn) run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.ws.SetReadLimit(maxFrameBytes)
	if !c.host.register(c) {
		_ = c.ws.Close(closeGoingAway, "the server is shutting down")
		return
	}
	defer c.host.unregister(c)
	defer c.releaseWatches()
	// A revocation that landed between the door's check and registration
	// found no connection to close; check again now that this one is findable.
	if err := c.host.cfg.Tokens.Recheck(ctx, c.principal); err != nil {
		_ = c.ws.Close(closeTokenInvalid, "the connection token is no longer valid")
		return
	}

	expires := time.Until(c.principal.ExpiresAt)
	expiry := time.AfterFunc(expires, func() { c.closeWith(closeTokenInvalid, "the connection token expired") })
	defer expiry.Stop()

	// Commands, writes and pings stop as soon as the connection closes. The
	// read does not: cancelling it would drop the connection before the close
	// frame, which tells the client why, is written. It ends with the close
	// handshake.
	work, stopWork := context.WithCancel(ctx)
	defer stopWork()
	go func() {
		<-c.done
		stopWork()
	}()
	go c.writeLoop(work)
	go c.handleLoop()
	go c.pingLoop(work)
	// Only the read loop below queues actions; closing after it stops lets
	// the worker drain them.
	defer close(c.handled)
	for {
		kind, data, err := c.ws.Read(ctx)
		if err != nil {
			c.closeWith(websocket.StatusNormalClosure, "")
			return
		}
		select {
		case <-c.done:
			// A closing connection handles nothing more.
			return
		default:
		}
		if kind != websocket.MessageText {
			c.closeWith(websocket.StatusUnsupportedData, "AHP messages are text frames")
			return
		}
		c.handle(work, data)
		select {
		case <-c.done:
			return
		default:
		}
	}
}

// frame is a queued message, or a close that runs after the messages before it.
type frame struct {
	data   []byte
	close  bool
	code   websocket.StatusCode
	reason string
}

// handleLoop runs admitted client actions' side effects in order, including
// the ones still queued when the connection closes. The read loop is the only
// producer and closes the queue when it stops.
func (c *conn) handleLoop() {
	for handle := range c.handled {
		handle()
	}
}

func (c *conn) writeLoop(ctx context.Context) {
	for {
		select {
		case <-c.done:
			return
		case next := <-c.send:
			if next.close {
				c.closeWith(next.code, next.reason)
				return
			}
			writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.ws.Write(writeCtx, websocket.MessageText, next.data)
			cancel()
			if err != nil {
				c.closeWith(websocket.StatusInternalError, "")
				return
			}
		}
	}
}

// closeAfterWrites closes the connection once the messages already queued,
// such as the error that explains the close, have been written.
func (c *conn) closeAfterWrites(code websocket.StatusCode, reason string) {
	select {
	case <-c.done:
	case c.send <- frame{close: true, code: code, reason: reason}:
	default:
		c.closeWith(code, reason)
	}
}

func (c *conn) closeWith(code websocket.StatusCode, reason string) {
	c.closeOnce.Do(func() {
		close(c.done)
		go func() { _ = c.ws.Close(code, reason) }()
	})
}

func (c *conn) enqueue(message []byte) {
	select {
	case <-c.done:
	case c.send <- frame{data: message}:
	default:
		c.closeWith(closeSlowClient, "the client is not reading fast enough")
	}
}

func (c *conn) write(message outbound) {
	message.JSONRPC = jsonRPCVersion
	raw, err := json.Marshal(message)
	if err != nil {
		return
	}
	c.enqueue(raw)
}

func (c *conn) state() (bool, string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.initialized, c.clientID, c.version
}

// deliverAction sends an envelope if the connection subscribes to its
// channel and knows its action type. It must not block.
func (c *conn) deliverAction(envelope ahptypes.ActionEnvelope) {
	c.mu.Lock()
	ok := c.initialized && c.subs[envelope.Channel]
	version := c.version
	c.mu.Unlock()
	if !ok {
		return
	}
	if kind, err := actionType(envelope.Action); err != nil || !actionKnownTo(kind, version) {
		return
	}
	c.write(outbound{Method: methodAction, Params: envelope})
}

func (c *conn) deliverNotification(channel, method string, params json.RawMessage) {
	c.mu.Lock()
	ok := c.initialized && c.subs[channel] && notificationKnownTo(method, c.version)
	c.mu.Unlock()
	if ok {
		c.write(outbound{Method: method, Params: params})
	}
}

func (c *conn) handle(ctx context.Context, data []byte) {
	var msg inbound
	if err := json.Unmarshal(data, &msg); err != nil || msg.JSONRPC != jsonRPCVersion || msg.Method == "" {
		c.write(outbound{ID: json.RawMessage("null"), Error: rpcErr(ahptypes.ErrorCodeParseError, "invalid JSON-RPC message")})
		return
	}
	// Revocation takes effect on the next command, not only on the next sweep.
	if err := c.host.cfg.Tokens.Recheck(ctx, c.principal); err != nil {
		c.closeWith(closeTokenInvalid, "the connection token is no longer valid")
		return
	}
	isRequest := len(msg.ID) > 0 && !bytes.Equal(msg.ID, []byte("null"))
	if !isRequest {
		c.handleNotification(ctx, msg)
		return
	}
	result, err := c.dispatch(ctx, msg)
	if err != nil {
		var rpc *rpcError
		if !errors.As(err, &rpc) {
			c.host.cfg.Logger.Error(err, "AHP command failed", "method", msg.Method, "user", c.principal.UserKey)
			rpc = rpcErr(ahptypes.ErrorCodeInternalError, "the command failed")
		}
		c.write(outbound{ID: msg.ID, Error: rpc})
		if rpc.Code == ahptypes.ErrorCodeUnsupportedProtocolVersion {
			c.closeAfterWrites(websocket.StatusPolicyViolation, "unsupported protocol version")
		}
		return
	}
	if _, ok := result.(responded); ok {
		return
	}
	c.respond(msg.ID, result)
}

// responded is returned by a command that queued its own response, in order
// with the actions around it.
type responded struct{}

func (c *conn) respond(id json.RawMessage, result any) {
	if result == nil {
		result = json.RawMessage("null")
	}
	c.write(outbound{ID: id, Result: result})
}

func (c *conn) dispatch(ctx context.Context, msg inbound) (any, error) {
	initialized, _, _ := c.state()
	switch msg.Method {
	case "ping":
		return nil, nil
	case "initialize":
		return c.initialize(ctx, msg.ID, msg.Params)
	case "reconnect":
		return c.reconnect(ctx, msg.ID, msg.Params)
	}
	if !initialized {
		return nil, rpcErr(ahptypes.ErrorCodeInvalidRequest, "initialize or reconnect first")
	}
	switch msg.Method {
	case "subscribe":
		return c.subscribe(ctx, msg.ID, msg.Params)
	case "listSessions":
		return c.listSessions(ctx, msg.Params)
	case "createSession":
		return nil, c.createSession(ctx, msg.Params)
	case "disposeSession":
		return nil, c.disposeSession(ctx, msg.Params)
	case "authenticate":
		// No agent advertises a protected resource yet. The params are never
		// logged: they carry the client's credential.
		return nil, rpcErr(ahptypes.ErrorCodeInvalidParams, "the resource is not advertised by any agent")
	case "createTerminal":
		return nil, c.createTerminal(ctx, msg.Params)
	case "disposeTerminal":
		return nil, c.disposeTerminal(ctx, msg.Params)
	case "completions":
		// No agent offers completions; an empty list keeps clients' input working.
		return ahptypes.CompletionsResult{Items: []ahptypes.CompletionItem{}}, nil
	case "resourceRead":
		return c.resourceRead(ctx, msg.Params)
	case "resourceList":
		return c.resourceList(ctx, msg.Params)
	case "createResourceWatch":
		return c.createResourceWatch(ctx, msg.Params)
	case "resolveSessionConfig":
		return c.resolveSessionConfig(ctx, msg.Params)
	default:
		if strings.HasPrefix(msg.Method, extensionMethodPrefix) {
			return c.extension(ctx, msg.Method, msg.Params)
		}
		return nil, rpcErr(ahptypes.ErrorCodeMethodNotFound, "method %q is not supported", msg.Method)
	}
}

// extension serves a host-specific extensions/ command through the backend
// that offers it.
func (c *conn) extension(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	result, err := c.host.cfg.Backends.extension(ctx, c.principal, method, raw, emitter{host: c.host, principal: c.principal})
	if errors.Is(err, ErrUnknownExtension) {
		return nil, rpcErr(ahptypes.ErrorCodeMethodNotFound, "method %q is not supported", method)
	}
	if rejection, ok := errors.AsType[*Rejection](err); ok {
		return nil, rpcErr(ahptypes.ErrorCodeInvalidParams, "%s", rejection.Reason)
	}
	return result, err
}

// resourceRead serves content a session's channel advertises, such as the
// before and after files of a changeset. The host reads nothing else: no
// path on the server is reachable through it.
func (c *conn) resourceRead(ctx context.Context, raw json.RawMessage) (any, error) {
	var params ahptypes.ResourceReadParams
	if err := decodeParams(raw, &params); err != nil {
		return nil, err
	}
	notFound := rpcErr(ahptypes.ErrorCodeNotFound, "not found")
	if params.Uri == "" || len(params.Uri) > maxURILength {
		return nil, notFound
	}
	if params.Channel == rootChannel {
		// The protocol scopes resourceRead to the root channel; backends find
		// the session that owns the resource, within this user's state only.
		result, err := c.host.cfg.Backends.readRootResource(ctx, c.principal, params.Uri)
		if errors.Is(err, ErrResourceNotFound) {
			return nil, notFound
		}
		if err != nil {
			return nil, err
		}
		return result, nil
	}
	if !validURI(params.Channel) {
		return nil, notFound
	}
	stored, _, err := c.host.readChannel(ctx, c.principal.UserKey, params.Channel)
	if errors.Is(err, errUnknownChannel) {
		return nil, notFound
	}
	if err != nil {
		return nil, err
	}
	record, err := c.host.cfg.Store.GetAHPSession(ctx, c.principal.UserKey, stored.Session)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound
	}
	if err != nil {
		return nil, err
	}
	backend, ok := c.host.cfg.Backends.Named(record.Backend)
	if !ok {
		return nil, notFound
	}
	reader, ok := backend.(ResourceReader)
	if !ok {
		return nil, notFound
	}
	result, err := reader.ReadResource(ctx, c.principal, stored.Session, params.Channel, params.Uri)
	if errors.Is(err, ErrResourceNotFound) {
		return nil, notFound
	}
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (c *conn) handleNotification(ctx context.Context, msg inbound) {
	initialized, _, _ := c.state()
	if !initialized {
		return
	}
	switch msg.Method {
	case "unsubscribe":
		var params ahptypes.UnsubscribeParams
		if json.Unmarshal(msg.Params, &params) == nil {
			c.mu.Lock()
			delete(c.subs, params.Channel)
			c.mu.Unlock()
			if isResourceWatchChannel(params.Channel) {
				c.releaseWatch(params.Channel)
			}
		}
	case "dispatchAction":
		c.dispatchAction(ctx, msg.Params)
	}
}

func decodeParams(raw json.RawMessage, target any) error {
	if len(raw) == 0 {
		return rpcErr(ahptypes.ErrorCodeInvalidParams, "params are required")
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return rpcErr(ahptypes.ErrorCodeInvalidParams, "invalid params")
	}
	return nil
}

func validClientID(id string) bool {
	if id == "" || len(id) > maxClientIDLength {
		return false
	}
	for _, r := range id {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func validURI(uri string) bool {
	if uri == "" || len(uri) > maxURILength || uri == rootChannel {
		return false
	}
	for _, r := range uri {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func (c *conn) initialize(ctx context.Context, id, raw json.RawMessage) (any, error) {
	var params ahptypes.InitializeParams
	if err := decodeParams(raw, &params); err != nil {
		return nil, err
	}
	if initialized, _, _ := c.state(); initialized {
		return nil, rpcErr(ahptypes.ErrorCodeInvalidRequest, "the connection is already initialized")
	}
	c.host.sweepOnce(ctx, c.principal)
	if !validClientID(params.ClientId) {
		return nil, rpcErr(ahptypes.ErrorCodeInvalidParams, "a valid clientId is required")
	}
	version, err := ahptypes.NegotiateProtocolVersion(params.ProtocolVersions)
	if err != nil || version == "" {
		data, _ := json.Marshal(map[string]any{"supportedVersions": ahptypes.SupportedProtocolVersions()})
		return nil, &rpcError{Code: ahptypes.ErrorCodeUnsupportedProtocolVersion,
			Message: "none of the offered protocol versions is supported", Data: data}
	}
	dialect := detectDialect(params.ClientInfo)
	now := c.host.cfg.Now().UTC()
	if err := c.host.cfg.Store.PutAHPClient(ctx, store.AHPClient{
		ClientID: params.ClientId, UserKey: c.principal.UserKey, ProtocolVersion: version, Dialect: dialect, UpdatedAt: now,
	}); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, rpcErr(ahptypes.ErrorCodePermissionDenied, "the clientId belongs to another user")
		}
		return nil, err
	}
	err = c.host.locked(ctx, c.principal.UserKey, func(seq int64) error {
		snapshots := []ahptypes.Snapshot{}
		var channels []string
		for _, channel := range params.InitialSubscriptions {
			snapshot, err := c.host.snapshotLocked(ctx, c.principal, seq, channel)
			if errors.Is(err, errUnknownChannel) {
				continue
			}
			if err != nil {
				return err
			}
			channels = append(channels, channel)
			snapshots = append(snapshots, snapshot)
		}
		c.activate(params.ClientId, version, dialect, channels)
		info := c.host.cfg.ServerInfo
		c.respond(id, ahptypes.InitializeResult{
			ProtocolVersion: version, ServerSeq: seq, ServerInfo: &info, Snapshots: snapshots,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return responded{}, nil
}

// activate marks the connection initialized with its subscriptions. Callers
// hold the hub lock, so delivery starts exactly after the baseline they send.
func (c *conn) activate(clientID, version, dialect string, channels []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initialized, c.clientID, c.version, c.dialect = true, clientID, version, dialect
	for _, channel := range channels {
		c.subs[channel] = true
	}
}

func (c *conn) subscribeTo(channel string) {
	c.mu.Lock()
	c.subs[channel] = true
	c.mu.Unlock()
}

func (c *conn) unsubscribeFrom(channel string) {
	c.mu.Lock()
	delete(c.subs, channel)
	c.mu.Unlock()
}

// reconnect restores a client's negotiated state and replays what it missed,
// or sends fresh snapshots when the journal no longer covers the gap. A
// client the server does not know, or one that belongs to another user, must
// initialize again; it never receives an empty replay.
func (c *conn) reconnect(ctx context.Context, id, raw json.RawMessage) (any, error) {
	var params ahptypes.ReconnectParams
	if err := decodeParams(raw, &params); err != nil {
		return nil, err
	}
	if initialized, _, _ := c.state(); initialized {
		return nil, rpcErr(ahptypes.ErrorCodeInvalidRequest, "the connection is already initialized")
	}
	c.host.sweepOnce(ctx, c.principal)
	record, err := c.host.cfg.Store.GetAHPClient(ctx, params.ClientId)
	if errors.Is(err, store.ErrNotFound) || (err == nil && record.UserKey != c.principal.UserKey) {
		return nil, rpcErr(ahptypes.ErrorCodeNotFound, "unknown client; initialize again")
	}
	if err != nil {
		return nil, err
	}
	err = c.host.locked(ctx, c.principal.UserKey, func(seq int64) error {
		result, allowed, err := c.reconnectResult(ctx, record.ProtocolVersion, seq, params)
		if err != nil {
			return err
		}
		c.activate(record.ClientID, record.ProtocolVersion, record.Dialect, allowed)
		c.respond(id, result)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return responded{}, nil
}

// reconnectResult builds the replay, or the snapshots when the journal no
// longer covers the gap, for the channels the user may still see. Callers
// hold the hub lock at sequence seq.
func (c *conn) reconnectResult(
	ctx context.Context,
	version string,
	seq int64,
	params ahptypes.ReconnectParams,
) (any, []string, error) {
	var allowed []string
	missing := []string{}
	for _, channel := range params.Subscriptions {
		if channel == rootChannel {
			allowed = append(allowed, channel)
			continue
		}
		if _, _, err := c.host.readChannel(ctx, c.principal.UserKey, channel); err != nil {
			if errors.Is(err, errUnknownChannel) {
				missing = append(missing, channel)
				continue
			}
			return nil, nil, err
		}
		allowed = append(allowed, channel)
	}
	entries, complete, err := c.host.cfg.Store.ReadAHPJournal(ctx, c.principal.UserKey, params.LastSeenServerSeq)
	if err != nil {
		return nil, nil, err
	}
	// Root notifications, such as a session added, are not journaled, so a
	// replay cannot carry one the client may have missed: it gets snapshots,
	// the protocol's durable recovery path, and lists its sessions again.
	if slices.Contains(allowed, rootChannel) && c.host.hub(c.principal.UserKey).rootNotified >= params.LastSeenServerSeq {
		complete = false
	}
	if complete {
		wanted := map[string]bool{}
		for _, channel := range allowed {
			wanted[channel] = true
		}
		actions := []ahptypes.ActionEnvelope{}
		for _, entry := range entries {
			if !wanted[entry.Channel] {
				continue
			}
			var envelope ahptypes.ActionEnvelope
			if err := json.Unmarshal(entry.Envelope, &envelope); err != nil {
				return nil, nil, err
			}
			if kind, err := actionType(envelope.Action); err != nil || !actionKnownTo(kind, version) {
				continue
			}
			actions = append(actions, envelope)
		}
		return reconnectReplay{Type: string(ahptypes.ReconnectResultTypeReplay), Actions: actions, Missing: missing}, allowed, nil
	}
	snapshots := []ahptypes.Snapshot{}
	for _, channel := range allowed {
		snapshot, err := c.host.snapshotLocked(ctx, c.principal, seq, channel)
		if errors.Is(err, errUnknownChannel) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	return reconnectSnapshot{Type: string(ahptypes.ReconnectResultTypeSnapshot), Snapshots: snapshots}, allowed, nil
}

func (c *conn) subscribe(ctx context.Context, id, raw json.RawMessage) (any, error) {
	var params ahptypes.SubscribeParams
	if err := decodeParams(raw, &params); err != nil {
		return nil, err
	}
	err := c.host.locked(ctx, c.principal.UserKey, func(seq int64) error {
		snapshot, err := c.host.snapshotLocked(ctx, c.principal, seq, params.Channel)
		if err != nil {
			return err
		}
		c.subscribeTo(params.Channel)
		c.respond(id, ahptypes.SubscribeResult{Snapshot: &snapshot})
		return nil
	})
	if errors.Is(err, errUnknownChannel) {
		return nil, rpcErr(ahptypes.ErrorCodeNotFound, "unknown channel")
	}
	if err != nil {
		return nil, err
	}
	return responded{}, nil
}

func (c *conn) listSessions(ctx context.Context, raw json.RawMessage) (any, error) {
	var params ahptypes.ListSessionsParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, rpcErr(ahptypes.ErrorCodeInvalidParams, "invalid params")
		}
	}
	limit := defaultListLimit
	if params.Limit != nil {
		if *params.Limit < 1 {
			return nil, rpcErr(ahptypes.ErrorCodeInvalidParams, "limit must be positive")
		}
		limit = int(min(*params.Limit, maxListLimit))
	}
	offset := 0
	if params.Cursor != nil {
		decoded, err := base64.RawURLEncoding.DecodeString(*params.Cursor)
		if err != nil {
			return nil, rpcErr(ahptypes.ErrorCodeInvalidParams, "invalid cursor")
		}
		if offset, err = strconv.Atoi(string(decoded)); err != nil || offset < 0 {
			return nil, rpcErr(ahptypes.ErrorCodeInvalidParams, "invalid cursor")
		}
	}
	sessions, err := c.host.cfg.Store.ListAHPSessions(ctx, c.principal.UserKey)
	if err != nil {
		return nil, err
	}
	result := ahptypes.ListSessionsResult{Items: []ahptypes.SessionSummary{}}
	for i := offset; i < len(sessions) && len(result.Items) < limit; i++ {
		var summary ahptypes.SessionSummary
		if err := json.Unmarshal(sessions[i].Summary, &summary); err != nil {
			return nil, err
		}
		result.Items = append(result.Items, summary)
	}
	if next := offset + len(result.Items); next < len(sessions) {
		cursor := base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(next)))
		result.NextCursor = &cursor
	}
	return result, nil
}

func (c *conn) createSession(ctx context.Context, raw json.RawMessage) (err error) {
	var params ahptypes.CreateSessionParams
	if err := decodeParams(raw, &params); err != nil {
		return err
	}
	session := params.Channel
	if !validURI(session) {
		return rpcErr(ahptypes.ErrorCodeInvalidParams, "a valid session URI is required")
	}
	if hostChannelScheme(session) {
		return rpcErr(ahptypes.ErrorCodeInvalidParams, "the URI scheme is reserved for another kind of channel")
	}
	release, claimed := c.host.claimChannelURI(c.principal.UserKey, session, false)
	if !claimed {
		return rpcErr(ahptypes.ErrorCodeSessionAlreadyExists, "the session already exists")
	}
	defer release()
	if err := c.freeSessionURI(ctx, session); err != nil {
		return err
	}
	provider := ""
	if params.Provider != nil {
		provider = *params.Provider
	}
	backend, provider, err := c.host.cfg.Backends.ForProvider(ctx, c.principal, provider)
	if err != nil {
		return rpcErr(ahptypes.ErrorCodeProviderNotFound, "%s", err.Error())
	}
	created, err := backend.CreateSession(ctx, c.principal, CreateSessionRequest{
		Session: session, Provider: provider, WorkingDirectories: params.WorkingDirectories, Config: params.Config,
	}, emitter{host: c.host, principal: c.principal})
	if err != nil {
		if rejection, ok := errors.AsType[*Rejection](err); ok {
			return rpcErr(ahptypes.ErrorCodePermissionDenied, "%s", rejection.Reason)
		}
		return err
	}
	// Until the session is ready, a failure takes back what was created, so
	// the URI can be created again: the catalog record, the stored channels
	// and the backend's session.
	var stored []string
	cataloged, committed := false, false
	defer func() {
		if err != nil && !committed {
			c.takeBackSession(context.WithoutCancel(ctx), backend, session, cataloged, stored)
		}
	}()
	state := created.State
	state.Provider = provider
	state.Lifecycle = ahptypes.SessionLifecycleCreating
	if state.ActiveClients == nil {
		state.ActiveClients = []ahptypes.SessionActiveClient{}
	}
	if state.Chats == nil {
		state.Chats = []ahptypes.ChatSummary{}
	}
	channels := map[string]storedChannel{session: {Session: session, State: ahptypes.SnapshotState{Session: &state}}}
	for chat, chatState := range created.Chats {
		if !validURI(chat) || chat == session {
			return fmt.Errorf("backend returned an invalid chat URI")
		}
		if chatState.Turns == nil {
			chatState.Turns = []ahptypes.Turn{}
		}
		channels[chat] = storedChannel{Session: session, State: ahptypes.SnapshotState{Chat: &chatState}}
	}
	for changeset, changesetState := range created.Changesets {
		if _, taken := channels[changeset]; taken || !validURI(changeset) {
			return fmt.Errorf("backend returned an invalid changeset URI")
		}
		if changesetState.Files == nil {
			changesetState.Files = []ahptypes.ChangesetFile{}
		}
		channels[changeset] = storedChannel{Session: session, State: ahptypes.SnapshotState{Changeset: &changesetState}}
	}
	// Subscribe before the channels exist so no action on them is missed.
	c.subscribeTo(session)
	defer func() {
		if err != nil {
			c.unsubscribeFrom(session)
		}
	}()
	if err := c.host.storeChannels(ctx, c.principal.UserKey, channels); err != nil {
		return err
	}
	stored = slices.Sorted(maps.Keys(channels))
	now := c.host.cfg.Now().UTC()
	summary := ahptypes.SessionSummary{
		Resource: session, Provider: provider, Title: state.Title, Status: state.Status,
		Project: state.Project, WorkingDirectories: state.WorkingDirectories,
		CreatedAt: now.Format(time.RFC3339Nano), ModifiedAt: now.Format(time.RFC3339Nano),
	}
	rawSummary, err := json.Marshal(summary)
	if err != nil {
		return err
	}
	if err := c.host.cfg.Store.PutAHPSession(ctx, store.AHPSession{
		UserKey: c.principal.UserKey, Session: session, Backend: backend.Name(), Summary: rawSummary,
		CreatedAt: now, ModifiedAt: now,
	}); err != nil {
		return err
	}
	cataloged = true
	ready := ahptypes.StateAction{Value: &ahptypes.SessionReadyAction{Type: ahptypes.ActionTypeSessionReady}}
	if err := c.host.apply(ctx, c.principal, session, []ahptypes.StateAction{ready}, nil); err != nil {
		return err
	}
	committed = true
	c.host.notifyRoot(c.principal.UserKey, methodSessionAdded, ahptypes.SessionAddedParams{Channel: rootChannel, Summary: summary})
	c.host.updateActiveSessions(ctx, c.principal)
	return nil
}

// freeSessionURI makes sure no session or other channel uses a URI claimed
// for a new session. A session's channels without its catalog record are what
// a failed creation could not take back, since a session is cataloged before
// it is ready and loses the record last: they are cleared for the new one.
func (c *conn) freeSessionURI(ctx context.Context, session string) error {
	if _, err := c.host.cfg.Store.GetAHPSession(ctx, c.principal.UserKey, session); err == nil {
		return rpcErr(ahptypes.ErrorCodeSessionAlreadyExists, "the session already exists")
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	stored, _, err := c.host.readChannel(ctx, c.principal.UserKey, session)
	if errors.Is(err, errUnknownChannel) {
		return nil
	}
	if err != nil {
		return err
	}
	// Another channel, such as a terminal, may already use the URI.
	if stored.State.Session == nil {
		return rpcErr(ahptypes.ErrorCodeAlreadyExists, "the URI is already used by another channel")
	}
	return c.host.forgetSession(ctx, c.principal.UserKey, session, sessionChannels(session, stored))
}

// takeBackSession removes what a failed createSession made: the stored
// channels, the catalog record when it was written and the backend's session.
// What a store failure leaves is found again: channels with the catalog
// record keep the session listed for disposal, and channels without it are
// cleared by the next creation of the URI.
func (c *conn) takeBackSession(ctx context.Context, backend SessionBackend, session string, cataloged bool, stored []string) {
	if cataloged || len(stored) > 0 {
		_ = c.host.forgetSession(ctx, c.principal.UserKey, session, stored)
	}
	_ = backend.DisposeSession(ctx, c.principal, session)
}

// sessionChannels returns a session's channels as its stored state lists them:
// the session's own, its chats and its changesets.
func sessionChannels(session string, stored storedChannel) []string {
	channels := []string{session}
	if stored.State.Session == nil {
		return channels
	}
	for _, chat := range stored.State.Session.Chats {
		channels = append(channels, chat.Resource)
	}
	for _, changeset := range stored.State.Session.Changesets {
		if !strings.Contains(changeset.UriTemplate, "{") {
			channels = append(channels, changeset.UriTemplate)
		}
	}
	return channels
}

// hostChannelScheme reports a URI on a scheme the host routes to terminals or
// resource watches, which therefore cannot name a session.
func hostChannelScheme(uri string) bool {
	return strings.HasPrefix(uri, terminalScheme) || strings.HasPrefix(uri, resourceWatchScheme)
}

func (c *conn) disposeSession(ctx context.Context, raw json.RawMessage) error {
	var params ahptypes.DisposeSessionParams
	if err := decodeParams(raw, &params); err != nil {
		return err
	}
	session := params.Channel
	record, err := c.host.cfg.Store.GetAHPSession(ctx, c.principal.UserKey, session)
	if errors.Is(err, store.ErrNotFound) {
		return rpcErr(ahptypes.ErrorCodeSessionNotFound, "unknown session")
	}
	if err != nil {
		return err
	}
	// The session's chats and changesets are found from its state, read before
	// anything is disposed of: a read that fails leaves the session to be
	// disposed of again, rather than its channels behind.
	channels := []string{session}
	stored, _, err := c.host.readChannel(ctx, c.principal.UserKey, session)
	if err != nil && !errors.Is(err, errUnknownChannel) {
		return err
	}
	if err == nil {
		channels = sessionChannels(session, stored)
	}
	if backend, ok := c.host.cfg.Backends.Named(record.Backend); ok {
		if err := backend.DisposeSession(ctx, c.principal, session); err != nil {
			return err
		}
	}
	if err := c.host.forgetSession(ctx, c.principal.UserKey, session, channels); err != nil {
		return err
	}
	c.host.notifyRoot(c.principal.UserKey, methodSessionRemoved, ahptypes.SessionRemovedParams{Channel: rootChannel, Session: session})
	c.host.updateActiveSessions(ctx, c.principal)
	return nil
}

// dispatchAction validates a client action, lets its session's backend
// authorize it as the operation it causes, then applies and journals it with
// its origin. A rejected action is echoed to this client with a reason.
func (c *conn) dispatchAction(ctx context.Context, raw json.RawMessage) {
	var params struct {
		Channel   string          `json:"channel"`
		ClientSeq int64           `json:"clientSeq"`
		Action    json.RawMessage `json:"action"`
	}
	if json.Unmarshal(raw, &params) != nil || len(params.Action) == 0 {
		return
	}
	_, clientID, version := c.state()
	origin := &ahptypes.ActionOrigin{ClientId: clientID, ClientSeq: params.ClientSeq}
	var action ahptypes.StateAction
	actionErr := json.Unmarshal(params.Action, &action)
	reject := func(reason string) {
		seq, _ := c.host.serverSeq(ctx, c.principal.UserKey)
		envelope := ahptypes.ActionEnvelope{Channel: params.Channel, ServerSeq: seq, Origin: origin, RejectionReason: &reason}
		if actionErr == nil {
			envelope.Action = action
			c.write(outbound{Method: methodAction, Params: envelope})
			return
		}
		// An undecodable action is echoed as the client sent it.
		c.write(outbound{Method: methodAction, Params: struct {
			Channel         string                 `json:"channel"`
			Action          json.RawMessage        `json:"action"`
			ServerSeq       int64                  `json:"serverSeq"`
			Origin          *ahptypes.ActionOrigin `json:"origin"`
			RejectionReason string                 `json:"rejectionReason"`
		}{params.Channel, params.Action, seq, origin, reason}})
	}
	kind, err := rawActionType(params.Action)
	switch {
	case err != nil || actionErr != nil:
		reject("the action is not valid")
		return
	case actionIntroducedIn[kind] == "":
		reject("unknown action type")
		return
	case !isClientDispatchable(kind):
		reject("clients may not dispatch this action")
		return
	case !actionKnownTo(kind, version):
		reject("the action is newer than the negotiated protocol version")
		return
	case params.Channel == rootChannel:
		reject("the host configuration is read-only")
		return
	}
	stored, _, err := c.host.readChannel(ctx, c.principal.UserKey, params.Channel)
	if err != nil {
		reject("unknown channel")
		return
	}
	backend, err := c.backendForChannel(ctx, stored)
	if err != nil {
		reject(err.Error())
		return
	}
	if err := backend.AuthorizeClientAction(ctx, c.principal, params.Channel, action); err != nil {
		if rejection, ok := errors.AsType[*Rejection](err); ok {
			reject(rejection.Reason)
		} else {
			reject("the action is not allowed")
		}
		return
	}
	if err := c.host.apply(ctx, c.principal, params.Channel, []ahptypes.StateAction{action}, origin); err != nil {
		if releaser, ok := backend.(ActionReleaser); ok {
			releaser.ReleaseClientAction(context.WithoutCancel(ctx), c.principal, params.Channel, action)
		}
		reject("the action could not be applied")
		return
	}
	principal := c.principal
	handleCtx := context.WithoutCancel(ctx)
	handle := func() {
		backend.HandleClientAction(handleCtx, principal, params.Channel, action, emitter{host: c.host, principal: principal})
	}
	// The action was admitted and applied: its side effect runs even if the
	// connection closes meanwhile.
	c.handled <- handle
}

// updateActiveSessions journals the user's session count on the root channel.
// It runs once a session is created or disposed of, which a failure here must
// not report as failed: the count is logged instead, and the next session
// created or disposed of counts again.
func (h *Host) updateActiveSessions(ctx context.Context, principal Principal) {
	sessions, err := h.cfg.Store.ListAHPSessions(ctx, principal.UserKey)
	if err == nil {
		action := ahptypes.StateAction{Value: &ahptypes.RootActiveSessionsChangedAction{
			Type: ahptypes.ActionTypeRootActiveSessionsChanged, ActiveSessions: int64(len(sessions)),
		}}
		err = h.apply(ctx, principal, rootChannel, []ahptypes.StateAction{action}, nil)
	}
	if err != nil {
		h.cfg.Logger.Error(err, "update the AHP active session count")
	}
}

// detectDialect picks the per-client translation layer. Only the default
// dialect exists so far; the value is persisted so reconnects keep it.
func detectDialect(info *ahptypes.Implementation) string {
	_ = info
	return defaultDialect
}
