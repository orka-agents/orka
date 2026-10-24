/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package ahphost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"

	"github.com/orka-agents/orka/internal/store"
)

const (
	rootChannel = ahptypes.RootResourceURI

	defaultJournalEntries = 1000
	defaultJournalBytes   = 8 << 20
	defaultSendQueue      = 1024
	defaultTokenRecheck   = 30 * time.Second
	defaultPingInterval   = 30 * time.Second
)

var errUnknownChannel = errors.New("unknown channel")

// Config configures a Host.
type Config struct {
	Store    store.AHPStore
	Tokens   *TokenService
	Backends *Registry
	// Limits bound each user's replay journal.
	Limits     store.AHPJournalLimits
	ServerInfo ahptypes.Implementation
	// SendQueue bounds the messages waiting for one slow client.
	SendQueue int
	// TokenRecheck is how often connected tokens are checked again.
	TokenRecheck time.Duration
	// PingInterval is how often an open connection is pinged; a client that
	// does not answer within it is disconnected.
	PingInterval time.Duration
	Logger       logr.Logger
	Now          func() time.Time
}

// Host serves the Agent Host Protocol for every user. Each user has their
// own sequence, journal, channel states and session catalog; the host keeps
// one hub per user that serializes changes and fans them out to that user's
// connections.
type Host struct {
	cfg Config

	mu      sync.Mutex
	hubs    map[string]*userHub
	byToken map[string]map[*conn]struct{}
	stopped bool
	// swept records the users whose leftover channels this process dropped.
	swept map[string]bool
	// creating are the channel URIs, per user, whose creation as a session or
	// terminal is under way.
	creating map[string]*channelCreation
}

// channelCreation is one channel URI being created. A terminal's backend can
// emit to it before the host has stored it, as when its shell exits at once;
// such actions are held and applied once the channel is stored.
type channelCreation struct {
	hold    bool
	actions []ahptypes.StateAction
}

// claimChannelURI reserves a channel URI for one creation at a time, so two
// connections of a user cannot both create it, as a session or a terminal.
// It reports false while another creation of the URI runs.
// With hold, actions a backend emits to the URI meanwhile wait for
// finishCreation instead of failing as for an unknown channel.
func (h *Host) claimChannelURI(userKey, uri string, hold bool) (func(), bool) {
	key := userKey + "\x00" + uri
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.creating[key] != nil {
		return nil, false
	}
	if h.creating == nil {
		h.creating = map[string]*channelCreation{}
	}
	creation := &channelCreation{hold: hold}
	h.creating[key] = creation
	return func() {
		h.mu.Lock()
		if h.creating[key] == creation {
			delete(h.creating, key)
		}
		h.mu.Unlock()
	}, true
}

// holdForCreation keeps a backend's actions for a channel whose creation
// holds them, and reports whether it did.
func (h *Host) holdForCreation(userKey, channel string, actions []ahptypes.StateAction) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	creation := h.creating[userKey+"\x00"+channel]
	if creation == nil || !creation.hold {
		return false
	}
	creation.actions = append(creation.actions, actions...)
	return true
}

// finishCreation applies, in order, the actions held while a stored channel
// was being created, then lets later actions apply directly.
func (h *Host) finishCreation(ctx context.Context, principal Principal, channel string) error {
	key := principal.UserKey + "\x00" + channel
	for {
		h.mu.Lock()
		creation := h.creating[key]
		if creation == nil || len(creation.actions) == 0 {
			if creation != nil {
				creation.hold = false
			}
			h.mu.Unlock()
			return nil
		}
		actions := creation.actions
		creation.actions = nil
		h.mu.Unlock()
		if err := h.apply(ctx, principal, channel, actions, nil); err != nil {
			return err
		}
	}
}

type userHub struct {
	mu     sync.Mutex
	key    string
	seq    int64
	loaded bool
	conns  map[*conn]struct{}
}

// storedChannel is how a channel's state is stored: the reduced state plus
// the session that owns the channel, so its actions reach the session's
// backend. A root-level resource such as a terminal names its backend instead.
type storedChannel struct {
	Session string                 `json:"session"`
	Backend string                 `json:"backend,omitempty"`
	State   ahptypes.SnapshotState `json:"state"`
}

// storedChannelJSON names the state's kind. Decoding a snapshot state by its
// shape alone cannot tell a terminal from a session: both have a lifecycle.
type storedChannelJSON struct {
	Session string          `json:"session"`
	Backend string          `json:"backend,omitempty"`
	Kind    string          `json:"kind,omitempty"`
	State   json.RawMessage `json:"state"`
}

func (s storedChannel) stateKind() (string, any) {
	switch state := s.State; {
	case state.Session != nil:
		return "session", state.Session
	case state.Chat != nil:
		return "chat", state.Chat
	case state.Changeset != nil:
		return "changeset", state.Changeset
	case state.Terminal != nil:
		return "terminal", state.Terminal
	case state.Annotations != nil:
		return "annotations", state.Annotations
	case state.ResourceWatch != nil:
		return "resourceWatch", state.ResourceWatch
	default:
		return "", state
	}
}

func (s storedChannel) MarshalJSON() ([]byte, error) {
	kind, value := s.stateKind()
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(storedChannelJSON{Session: s.Session, Backend: s.Backend, Kind: kind, State: raw})
}

func (s *storedChannel) UnmarshalJSON(data []byte) error {
	var stored storedChannelJSON
	if err := json.Unmarshal(data, &stored); err != nil {
		return err
	}
	*s = storedChannel{Session: stored.Session, Backend: stored.Backend}
	var target any
	switch stored.Kind {
	case "session":
		s.State.Session = &ahptypes.SessionState{}
		target = s.State.Session
	case "chat":
		s.State.Chat = &ahptypes.ChatState{}
		target = s.State.Chat
	case "changeset":
		s.State.Changeset = &ahptypes.ChangesetState{}
		target = s.State.Changeset
	case "terminal":
		s.State.Terminal = &ahptypes.TerminalState{}
		target = s.State.Terminal
	case "annotations":
		s.State.Annotations = &ahptypes.AnnotationsState{}
		target = s.State.Annotations
	case "resourceWatch":
		s.State.ResourceWatch = &ahptypes.ResourceWatchState{}
		target = s.State.ResourceWatch
	default:
		target = &s.State
	}
	return json.Unmarshal(stored.State, target)
}

// New builds a Host. Wire its token service's OnRevoke to Host.CloseToken.
func New(cfg Config) (*Host, error) {
	if cfg.Store == nil || cfg.Tokens == nil {
		return nil, fmt.Errorf("an AHP store and token service are required")
	}
	if cfg.Limits.MaxEntries <= 0 {
		cfg.Limits.MaxEntries = defaultJournalEntries
	}
	if cfg.Limits.MaxBytes <= 0 {
		cfg.Limits.MaxBytes = defaultJournalBytes
	}
	if cfg.SendQueue <= 0 {
		cfg.SendQueue = defaultSendQueue
	}
	if cfg.TokenRecheck <= 0 {
		cfg.TokenRecheck = defaultTokenRecheck
	}
	if cfg.ServerInfo.Name == "" {
		cfg.ServerInfo = ahptypes.Implementation{Name: "orka"}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	h := &Host{cfg: cfg, hubs: map[string]*userHub{}, byToken: map[string]map[*conn]struct{}{}}
	return h, nil
}

// Tokens returns the host's token service.
func (h *Host) Tokens() *TokenService { return h.cfg.Tokens }

// Store returns the host's store, for session backends' own records.
func (h *Host) Store() store.AHPStore { return h.cfg.Store }

// RegisterBackend adds a session backend before the host serves connections.
func (h *Host) RegisterBackend(backend SessionBackend) error {
	if h.cfg.Backends == nil {
		return fmt.Errorf("the AHP host has no backend registry")
	}
	return h.cfg.Backends.Register(backend)
}

// Emitter delivers a backend's actions for a user, connected or not.
func (h *Host) Emitter(principal Principal) Emitter { return emitter{host: h, principal: principal} }

// Recheck reports whether a principal's connection token is still valid:
// not revoked, not expired, and its owner may still sign in.
func (h *Host) Recheck(ctx context.Context, principal Principal) error {
	return h.cfg.Tokens.Recheck(ctx, principal)
}

// Start rechecks connected tokens until ctx ends, closing connections whose
// token was revoked or expired.
func (h *Host) Start(ctx context.Context) error {
	ticker := time.NewTicker(h.cfg.TokenRecheck)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			h.mu.Lock()
			h.stopped = true
			h.mu.Unlock()
			h.closeAll("the server is shutting down")
			return nil
		case <-ticker.C:
			for _, c := range h.connections() {
				if err := h.cfg.Tokens.Recheck(ctx, c.principal); err != nil {
					c.closeWith(closeTokenInvalid, "the connection token is no longer valid")
				}
			}
		}
	}
}

// CloseToken closes every connection that uses a token.
func (h *Host) CloseToken(tokenID string) {
	h.mu.Lock()
	conns := make([]*conn, 0, len(h.byToken[tokenID]))
	for c := range h.byToken[tokenID] {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	for _, c := range conns {
		c.closeWith(closeTokenInvalid, "the connection token was revoked")
	}
}

func (h *Host) connections() []*conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	var conns []*conn
	for _, set := range h.byToken {
		for c := range set {
			conns = append(conns, c)
		}
	}
	return conns
}

func (h *Host) closeAll(reason string) {
	for _, c := range h.connections() {
		c.closeWith(closeGoingAway, reason)
	}
}

func (h *Host) hub(userKey string) *userHub {
	h.mu.Lock()
	defer h.mu.Unlock()
	hub := h.hubs[userKey]
	if hub == nil {
		hub = &userHub{key: userKey, conns: map[*conn]struct{}{}}
		h.hubs[userKey] = hub
	}
	return hub
}

// register adds a connection; it refuses once the host has stopped.
func (h *Host) register(c *conn) bool {
	h.mu.Lock()
	if h.stopped {
		h.mu.Unlock()
		return false
	}
	if h.byToken[c.principal.TokenID] == nil {
		h.byToken[c.principal.TokenID] = map[*conn]struct{}{}
	}
	h.byToken[c.principal.TokenID][c] = struct{}{}
	h.mu.Unlock()
	hub := h.hub(c.principal.UserKey)
	hub.mu.Lock()
	hub.conns[c] = struct{}{}
	hub.mu.Unlock()
	return true
}

func (h *Host) unregister(c *conn) {
	h.mu.Lock()
	if set := h.byToken[c.principal.TokenID]; set != nil {
		delete(set, c)
		if len(set) == 0 {
			delete(h.byToken, c.principal.TokenID)
		}
	}
	h.mu.Unlock()
	hub := h.hub(c.principal.UserKey)
	hub.mu.Lock()
	delete(hub.conns, c)
	hub.mu.Unlock()
}

// loadLocked reads the user's sequence once. hub.mu must be held.
func (h *Host) loadLocked(ctx context.Context, hub *userHub) error {
	if hub.loaded {
		return nil
	}
	seq, err := h.cfg.Store.AHPServerSeq(ctx, hub.key)
	if err != nil {
		return err
	}
	hub.seq, hub.loaded = seq, true
	return nil
}

func (h *Host) serverSeq(ctx context.Context, userKey string) (int64, error) {
	var seq int64
	err := h.locked(ctx, userKey, func(current int64) error {
		seq = current
		return nil
	})
	return seq, err
}

// locked runs fn with the user's current sequence under the hub lock. Actions
// are applied and delivered under the same lock, so a snapshot taken, a
// subscription installed and a response queued inside fn are never overtaken
// by a concurrent action, nor miss one.
func (h *Host) locked(ctx context.Context, userKey string, fn func(seq int64) error) error {
	hub := h.hub(userKey)
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if err := h.loadLocked(ctx, hub); err != nil {
		return err
	}
	return fn(hub.seq)
}

func (h *Host) readChannel(ctx context.Context, userKey, channel string) (storedChannel, int64, error) {
	record, err := h.cfg.Store.GetAHPChannelState(ctx, userKey, channel)
	if errors.Is(err, store.ErrNotFound) {
		return storedChannel{}, 0, errUnknownChannel
	}
	if err != nil {
		return storedChannel{}, 0, err
	}
	var stored storedChannel
	if err := json.Unmarshal(record.State, &stored); err != nil {
		return storedChannel{}, 0, fmt.Errorf("decode channel state: %w", err)
	}
	return stored, record.FromSeq, nil
}

// apply reduces actions into a channel's state, journals them under the next
// sequence numbers, and fans them out to the user's subscribed connections.
func (h *Host) apply(
	ctx context.Context,
	principal Principal,
	channel string,
	actions []ahptypes.StateAction,
	origin *ahptypes.ActionOrigin,
) error {
	hub := h.hub(principal.UserKey)
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if err := h.loadLocked(ctx, hub); err != nil {
		return err
	}
	var states []store.AHPChannelState
	var stored storedChannel
	if channel != rootChannel {
		var err error
		if stored, _, err = h.readChannel(ctx, principal.UserKey, channel); err != nil {
			return err
		}
		for _, action := range actions {
			if reduce(&stored.State, action) == ahp.ReduceOutcomeOutOfScope {
				return fmt.Errorf("action does not apply to channel %s", channel)
			}
		}
		if stored.State.Terminal != nil {
			trimTerminalContent(stored.State.Terminal)
		}
	}
	envelopes := make([]ahptypes.ActionEnvelope, len(actions))
	entries := make([]store.AHPJournalEntry, len(actions))
	for i, action := range actions {
		envelopes[i] = ahptypes.ActionEnvelope{
			Channel: channel, Action: action, ServerSeq: hub.seq + int64(i) + 1, Origin: origin,
		}
		raw, err := json.Marshal(envelopes[i])
		if err != nil {
			return err
		}
		entries[i] = store.AHPJournalEntry{Seq: envelopes[i].ServerSeq, Channel: channel, Envelope: raw}
	}
	last := hub.seq + int64(len(actions))
	if channel != rootChannel {
		raw, err := json.Marshal(stored)
		if err != nil {
			return err
		}
		states = append(states, store.AHPChannelState{Channel: channel, State: raw, FromSeq: last})
	}
	if err := h.cfg.Store.AppendAHPJournal(ctx, principal.UserKey, hub.seq, entries, states, h.cfg.Limits); err != nil {
		hub.loaded = false
		return err
	}
	hub.seq = last
	for c := range hub.conns {
		for _, envelope := range envelopes {
			c.deliverAction(envelope)
		}
	}
	if stored.State.Session != nil && stored.Session == channel {
		return h.syncSessionSummaryLocked(ctx, hub, principal, channel, stored.State.Session)
	}
	return nil
}

// syncSessionSummaryLocked keeps a session's catalog summary in step with its
// state and tells the user's root subscribers what changed. hub.mu must be held.
func (h *Host) syncSessionSummaryLocked(
	ctx context.Context, hub *userHub, principal Principal, session string, state *ahptypes.SessionState,
) error {
	record, err := h.cfg.Store.GetAHPSession(ctx, principal.UserKey, session)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var summary ahptypes.SessionSummary
	if err := json.Unmarshal(record.Summary, &summary); err != nil {
		return fmt.Errorf("decode session summary: %w", err)
	}
	next := summary
	next.Title, next.Status, next.Changes = state.Title, summaryStatus(state), summaryChanges(state.Chats)
	next.Project, next.WorkingDirectories = state.Project, state.WorkingDirectories
	var changes ahptypes.PartialSessionSummary
	changed := false
	if next.Title != summary.Title {
		changes.Title, changed = &next.Title, true
	}
	if next.Status != summary.Status {
		changes.Status, changed = &next.Status, true
	}
	if !jsonEqual(next.Changes, summary.Changes) {
		changes.Changes, changed = next.Changes, true
	}
	if !jsonEqual(next.WorkingDirectories, summary.WorkingDirectories) {
		changes.WorkingDirectories, changed = next.WorkingDirectories, true
	}
	if !changed {
		return nil
	}
	now := h.cfg.Now().UTC()
	next.ModifiedAt = now.Format(time.RFC3339Nano)
	changes.ModifiedAt = &next.ModifiedAt
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	record.Summary, record.ModifiedAt = raw, now
	if err := h.cfg.Store.PutAHPSession(ctx, record); err != nil {
		return err
	}
	params, err := json.Marshal(ahptypes.SessionSummaryChangedParams{Channel: rootChannel, Session: session, Changes: changes})
	if err != nil {
		return err
	}
	for c := range hub.conns {
		c.deliverNotification(rootChannel, methodSessionSummaryChanged, params)
	}
	return nil
}

// summaryStatus is the session's status as a list shows it: what its busiest
// chat is doing, plus the session's read and archived flags.
func summaryStatus(state *ahptypes.SessionState) ahptypes.SessionStatus {
	flags := state.Status & (ahptypes.SessionStatusIsRead | ahptypes.SessionStatusIsArchived)
	activity := ahptypes.SessionStatusIdle
	rank := func(status ahptypes.SessionStatus) int {
		switch status &^ (ahptypes.SessionStatusIsRead | ahptypes.SessionStatusIsArchived) {
		case ahptypes.SessionStatusInputNeeded:
			return 3
		case ahptypes.SessionStatusInProgress:
			return 2
		case ahptypes.SessionStatusError:
			return 1
		default:
			return 0
		}
	}
	for _, chat := range state.Chats {
		if rank(chat.Status) > rank(activity) {
			activity = chat.Status &^ (ahptypes.SessionStatusIsRead | ahptypes.SessionStatusIsArchived)
		}
	}
	if rank(state.Status) > rank(activity) {
		activity = state.Status &^ (ahptypes.SessionStatusIsRead | ahptypes.SessionStatusIsArchived)
	}
	return activity | flags
}

func summaryChanges(chats []ahptypes.ChatSummary) *ahptypes.ChangesSummary {
	var additions, deletions, files int64
	seen := false
	for _, chat := range chats {
		if chat.Changes == nil {
			continue
		}
		seen = true
		for target, value := range map[*int64]*int64{&additions: chat.Changes.Additions, &deletions: chat.Changes.Deletions, &files: chat.Changes.Files} {
			if value != nil {
				*target += *value
			}
		}
	}
	if !seen {
		return nil
	}
	return &ahptypes.ChangesSummary{Additions: &additions, Deletions: &deletions, Files: &files}
}

func jsonEqual(a, b any) bool {
	left, errA := json.Marshal(a)
	right, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(left) == string(right)
}

// storeChannels records new channel states without journaling actions.
func (h *Host) storeChannels(ctx context.Context, userKey string, channels map[string]storedChannel) error {
	hub := h.hub(userKey)
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if err := h.loadLocked(ctx, hub); err != nil {
		return err
	}
	states := make([]store.AHPChannelState, 0, len(channels))
	for channel, state := range channels {
		raw, err := json.Marshal(state)
		if err != nil {
			return err
		}
		states = append(states, store.AHPChannelState{Channel: channel, State: raw, FromSeq: hub.seq})
	}
	if err := h.cfg.Store.AppendAHPJournal(ctx, userKey, hub.seq, nil, states, h.cfg.Limits); err != nil {
		hub.loaded = false
		return err
	}
	return nil
}

// notifyRoot sends a protocol notification to the user's connections
// subscribed to the root channel. Notifications are not journaled.
func (h *Host) notifyRoot(userKey, method string, params any) {
	raw, err := json.Marshal(params)
	if err != nil {
		return
	}
	hub := h.hub(userKey)
	hub.mu.Lock()
	defer hub.mu.Unlock()
	for c := range hub.conns {
		c.deliverNotification(rootChannel, method, raw)
	}
}

// snapshot returns a channel's current state for the principal, or
// errUnknownChannel if the user has no such channel.
func (h *Host) snapshot(ctx context.Context, principal Principal, channel string) (ahptypes.Snapshot, error) {
	var snapshot ahptypes.Snapshot
	err := h.locked(ctx, principal.UserKey, func(seq int64) error {
		var err error
		snapshot, err = h.snapshotLocked(ctx, principal, seq, channel)
		return err
	})
	return snapshot, err
}

// snapshotLocked is snapshot for a caller inside locked, at sequence seq.
func (h *Host) snapshotLocked(ctx context.Context, principal Principal, seq int64, channel string) (ahptypes.Snapshot, error) {
	if channel == rootChannel {
		root, err := h.rootState(ctx, principal)
		if err != nil {
			return ahptypes.Snapshot{}, err
		}
		return ahptypes.Snapshot{Resource: rootChannel, State: ahptypes.SnapshotState{Root: root}, FromSeq: seq}, nil
	}
	stored, fromSeq, err := h.readChannel(ctx, principal.UserKey, channel)
	if err != nil {
		return ahptypes.Snapshot{}, err
	}
	return ahptypes.Snapshot{Resource: channel, State: stored.State, FromSeq: fromSeq}, nil
}

func (h *Host) rootState(ctx context.Context, principal Principal) (*ahptypes.RootState, error) {
	agents, err := h.cfg.Backends.Agents(ctx, principal)
	if err != nil {
		return nil, err
	}
	sessions, err := h.cfg.Store.ListAHPSessions(ctx, principal.UserKey)
	if err != nil {
		return nil, err
	}
	active := int64(len(sessions))
	meta, config, err := h.cfg.Backends.rootExtensions(ctx, principal)
	if err != nil {
		return nil, err
	}
	terminals, err := h.cfg.Backends.terminals(ctx, principal)
	if err != nil {
		return nil, err
	}
	root := &ahptypes.RootState{Agents: agents, ActiveSessions: &active, Meta: meta, Terminals: terminals}
	if config != nil {
		root.Config = &ahptypes.RootConfigState{
			Schema: ahptypes.ConfigSchema{Type: "object", Properties: map[string]ahptypes.ConfigPropertySchema{}},
			Values: config,
		}
	}
	return root, nil
}

// emitter delivers a backend's actions for one principal.
type emitter struct {
	host      *Host
	principal Principal
}

func (e emitter) Emit(ctx context.Context, channel string, actions ...ahptypes.StateAction) error {
	if channel == rootChannel {
		for _, action := range actions {
			switch action.Value.(type) {
			case *ahptypes.RootConfigChangedAction, *ahptypes.RootAgentsChangedAction:
			default:
				return fmt.Errorf("session backends may change only the root configuration and agents")
			}
		}
	} else if e.host.holdForCreation(e.principal.UserKey, channel, actions) {
		return nil
	}
	return e.host.apply(ctx, e.principal, channel, actions, nil)
}

func reduce(state *ahptypes.SnapshotState, action ahptypes.StateAction) ahp.ReduceOutcome {
	switch {
	case state.Session != nil:
		return ahp.ApplyActionToSession(state.Session, action)
	case state.Chat != nil:
		return ahp.ApplyActionToChat(state.Chat, action)
	case state.Changeset != nil:
		return ahp.ApplyActionToChangeset(state.Changeset, action)
	case state.Terminal != nil:
		return ahp.ApplyActionToTerminal(state.Terminal, action)
	case state.Annotations != nil:
		return ahp.ApplyActionToAnnotations(state.Annotations, action)
	case state.Canvas != nil:
		return ahp.ApplyActionToCanvas(state.Canvas, action)
	case state.ResourceWatch != nil:
		return ahp.ApplyActionToResourceWatch(state.ResourceWatch, action)
	case state.Automations != nil:
		return ahp.ApplyActionToAutomation(state.Automations, action)
	case state.AutomationRun != nil:
		return ahp.ApplyActionToAutomationRun(state.AutomationRun, action)
	default:
		return ahp.ReduceOutcomeOutOfScope
	}
}
