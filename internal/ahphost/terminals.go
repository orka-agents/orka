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
	"unicode/utf8"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"

	"github.com/orka-agents/orka/internal/store"
)

// maxStoredTerminalContent bounds the output a terminal channel's stored
// state keeps for snapshots. Live delivery is not affected.
const maxStoredTerminalContent = 64 << 10

// TerminalProvider runs the host's terminals for a backend. A terminal is a
// root-level resource of one user: it belongs to the backend that created
// it, not to a session.
type TerminalProvider interface {
	// CreateTerminal starts a terminal on channel and returns its initial state.
	CreateTerminal(ctx context.Context, principal Principal, params ahptypes.CreateTerminalParams, emit Emitter) (ahptypes.TerminalState, error)
	// DisposeTerminal stops a terminal. A missing terminal is not an error.
	DisposeTerminal(ctx context.Context, principal Principal, channel string) error
	// Terminals lists the user's running terminals for the root state.
	Terminals(ctx context.Context, principal Principal) ([]ahptypes.TerminalInfo, error)
}

// TerminalReaper is a TerminalProvider that can stop what terminals an earlier
// process left behind still run, once their shells are no longer its own. The
// host calls it before dropping their channels, and keeps them when it fails.
type TerminalReaper interface {
	ReapTerminals(ctx context.Context, principal Principal, channels []string) error
}

// terminalProvider returns the first backend that offers terminals.
func (r *Registry) terminalProvider() (SessionBackend, TerminalProvider, bool) {
	if r == nil {
		return nil, nil, false
	}
	for _, backend := range r.list() {
		if provider, ok := backend.(TerminalProvider); ok {
			return backend, provider, true
		}
	}
	return nil, nil, false
}

func (r *Registry) terminals(ctx context.Context, principal Principal) ([]ahptypes.TerminalInfo, error) {
	var all []ahptypes.TerminalInfo
	if r == nil {
		return all, nil
	}
	for _, backend := range r.list() {
		provider, ok := backend.(TerminalProvider)
		if !ok {
			continue
		}
		terminals, err := provider.Terminals(ctx, principal)
		if err != nil {
			return nil, err
		}
		all = append(all, terminals...)
	}
	return all, nil
}

func (c *conn) createTerminal(ctx context.Context, raw json.RawMessage) error {
	var params ahptypes.CreateTerminalParams
	if err := decodeParams(raw, &params); err != nil {
		return err
	}
	if !validURI(params.Channel) {
		return rpcErr(ahptypes.ErrorCodeInvalidParams, "a valid terminal URI is required")
	}
	backend, provider, ok := c.host.cfg.Backends.terminalProvider()
	if !ok {
		return rpcErr(ahptypes.ErrorCodeMethodNotFound, "method %q is not supported", "createTerminal")
	}
	release, claimed := c.host.claimChannelURI(c.principal.UserKey, params.Channel, true)
	if !claimed {
		return rpcErr(ahptypes.ErrorCodeAlreadyExists, "the terminal already exists")
	}
	defer release()
	if _, _, err := c.host.readChannel(ctx, c.principal.UserKey, params.Channel); err == nil {
		return rpcErr(ahptypes.ErrorCodeAlreadyExists, "the terminal already exists")
	} else if !errors.Is(err, errUnknownChannel) {
		return err
	}
	state, err := provider.CreateTerminal(ctx, c.principal, params, emitter{host: c.host, principal: c.principal})
	if err != nil {
		if rejection, ok := errors.AsType[*Rejection](err); ok {
			return rpcErr(ahptypes.ErrorCodePermissionDenied, "%s", rejection.Reason)
		}
		return err
	}
	if state.Content == nil {
		state.Content = []ahptypes.TerminalContentPart{}
	}
	c.subscribeTo(params.Channel)
	if err := c.host.storeChannels(ctx, c.principal.UserKey, map[string]storedChannel{
		params.Channel: {Backend: backend.Name(), State: ahptypes.SnapshotState{Terminal: &state}},
	}); err != nil {
		c.unsubscribeFrom(params.Channel)
		_ = provider.DisposeTerminal(ctx, c.principal, params.Channel)
		return err
	}
	// The shell may already have exited, or written output. What it reported
	// that cannot be stored takes the terminal back, rather than leave a
	// channel that shows a shell no longer running.
	if err := c.host.finishCreation(ctx, c.principal, params.Channel); err != nil {
		c.unsubscribeFrom(params.Channel)
		cleanup := context.WithoutCancel(ctx)
		_ = provider.DisposeTerminal(cleanup, c.principal, params.Channel)
		_ = c.host.cfg.Store.DeleteAHPChannelStates(cleanup, c.principal.UserKey, []string{params.Channel})
		return err
	}
	c.host.refreshTerminals(ctx, c.principal)
	return nil
}

func (c *conn) disposeTerminal(ctx context.Context, raw json.RawMessage) error {
	var params ahptypes.DisposeTerminalParams
	if err := decodeParams(raw, &params); err != nil {
		return err
	}
	stored, _, err := c.host.readChannel(ctx, c.principal.UserKey, params.Channel)
	if errors.Is(err, errUnknownChannel) || (err == nil && stored.State.Terminal == nil) {
		return rpcErr(ahptypes.ErrorCodeNotFound, "unknown terminal")
	}
	if err != nil {
		return err
	}
	if backend, ok := c.host.cfg.Backends.Named(stored.Backend); ok {
		if provider, ok := backend.(TerminalProvider); ok {
			if err := provider.DisposeTerminal(ctx, c.principal, params.Channel); err != nil {
				if rejection, ok := errors.AsType[*Rejection](err); ok {
					return rpcErr(ahptypes.ErrorCodePermissionDenied, "%s", rejection.Reason)
				}
				return err
			}
		}
	}
	if err := c.host.cfg.Store.DeleteAHPChannelStates(ctx, c.principal.UserKey, []string{params.Channel}); err != nil {
		return err
	}
	c.host.refreshTerminals(ctx, c.principal)
	return nil
}

// refreshTerminals updates the user's terminal list after a terminal was
// created or disposed of. That change is done, so a failed update does not
// report it failed, which would leave the client unable to retry it: the list
// is built whole again on its next change.
func (h *Host) refreshTerminals(ctx context.Context, principal Principal) {
	if err := h.updateTerminals(ctx, principal); err != nil {
		h.cfg.Logger.Error(err, "could not update the terminal list")
	}
}

// updateTerminals journals the user's terminal list on the root channel.
func (h *Host) updateTerminals(ctx context.Context, principal Principal) error {
	terminals, err := h.cfg.Backends.terminals(ctx, principal)
	if err != nil {
		return err
	}
	if terminals == nil {
		terminals = []ahptypes.TerminalInfo{}
	}
	action := ahptypes.StateAction{Value: &ahptypes.RootTerminalsChangedAction{
		Type: ahptypes.ActionTypeRootTerminalsChanged, Terminals: terminals,
	}}
	return h.apply(ctx, principal, rootChannel, []ahptypes.StateAction{action}, nil)
}

// UpdateTerminals lets a backend publish a change to the user's terminals,
// such as one that exited.
func (h *Host) UpdateTerminals(ctx context.Context, principal Principal) error {
	return h.updateTerminals(ctx, principal)
}

// trimTerminalContent keeps the end of a terminal's stored output.
func trimTerminalContent(state *ahptypes.TerminalState) {
	total := 0
	for _, part := range state.Content {
		total += terminalPartSize(part)
	}
	for total > maxStoredTerminalContent && len(state.Content) > 0 {
		excess := total - maxStoredTerminalContent
		size := terminalPartSize(state.Content[0])
		if size <= excess || len(state.Content) > 1 && size == 0 {
			state.Content = state.Content[1:]
			total -= size
			continue
		}
		if part, ok := state.Content[0].Value.(*ahptypes.TerminalUnclassifiedPart); ok {
			cut := excess
			for cut < len(part.Value) && !utf8.RuneStart(part.Value[cut]) {
				cut++
			}
			total -= cut
			part.Value = part.Value[cut:]
			continue
		}
		state.Content = state.Content[1:]
		total -= size
	}
}

func terminalPartSize(part ahptypes.TerminalContentPart) int {
	switch value := part.Value.(type) {
	case *ahptypes.TerminalUnclassifiedPart:
		return len(value.Value)
	case *ahptypes.TerminalCommandPart:
		return len(value.CommandLine) + len(value.Output)
	default:
		return 0
	}
}

// backendForChannel finds the backend that owns a session or terminal channel.
func (c *conn) backendForChannel(ctx context.Context, stored storedChannel) (SessionBackend, error) {
	name := stored.Backend
	if stored.Session != "" {
		record, err := c.host.cfg.Store.GetAHPSession(ctx, c.principal.UserKey, stored.Session)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, fmt.Errorf("unknown session")
			}
			return nil, err
		}
		name = record.Backend
	}
	backend, ok := c.host.cfg.Backends.Named(name)
	if !ok {
		return nil, fmt.Errorf("the channel's backend is not available")
	}
	return backend, nil
}
