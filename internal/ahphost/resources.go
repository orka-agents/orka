/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package ahphost

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// resourceWatchScheme names the channels of resource watches, and
// terminalScheme those of terminals.
const (
	resourceWatchScheme = "ahp-resource-watch:/"
	terminalScheme      = "ahp-terminal:"
)

// ResourceLister lists the directories a backend serves to its users.
type ResourceLister interface {
	// ListResource lists a directory. ErrResourceNotFound hides anything the
	// principal may not see.
	ListResource(ctx context.Context, principal Principal, uri string) ([]ahptypes.DirectoryEntry, error)
}

// ResourceWatcher reports changes to the resources a backend serves.
type ResourceWatcher interface {
	// WatchResources reports changes under params.Uri through changed until ctx
	// ends. Once it is watching it calls changed with no changes, and it
	// returns ErrResourceNotFound for anything the principal may not see.
	// Changes changed could not record are reported again.
	WatchResources(ctx context.Context, principal Principal, params ahptypes.CreateResourceWatchParams, changed func([]ahptypes.ResourceChange) error) error
}

// SessionConfigResolver resolves the configuration a new session would take.
type SessionConfigResolver interface {
	ResolveSessionConfig(ctx context.Context, principal Principal, params ahptypes.ResolveSessionConfigParams) (ahptypes.ResolveSessionConfigResult, error)
}

// firstOffering returns the first backend that implements T.
func firstOffering[T any](r *Registry) (SessionBackend, T, bool) {
	var zero T
	if r == nil {
		return nil, zero, false
	}
	for _, backend := range r.list() {
		if offered, ok := backend.(T); ok {
			return backend, offered, true
		}
	}
	return nil, zero, false
}

func (c *conn) resourceList(ctx context.Context, raw json.RawMessage) (any, error) {
	var params ahptypes.ResourceListParams
	if err := decodeParams(raw, &params); err != nil {
		return nil, err
	}
	notFound := rpcErr(ahptypes.ErrorCodeNotFound, "not found")
	if params.Uri == "" || len(params.Uri) > maxURILength {
		return nil, notFound
	}
	_, lister, ok := firstOffering[ResourceLister](c.host.cfg.Backends)
	if !ok {
		return nil, notFound
	}
	entries, err := lister.ListResource(ctx, c.principal, params.Uri)
	if errors.Is(err, ErrResourceNotFound) {
		return nil, notFound
	}
	if err != nil {
		return nil, err
	}
	if entries == nil {
		entries = []ahptypes.DirectoryEntry{}
	}
	return ahptypes.ResourceListResult{Entries: entries}, nil
}

func (c *conn) resolveSessionConfig(ctx context.Context, raw json.RawMessage) (any, error) {
	var params ahptypes.ResolveSessionConfigParams
	if err := decodeParams(raw, &params); err != nil {
		return nil, err
	}
	provider := ""
	if params.Provider != nil {
		provider = *params.Provider
	}
	backend, _, err := c.host.cfg.Backends.ForProvider(ctx, c.principal, provider)
	if err != nil {
		return nil, rpcErr(ahptypes.ErrorCodeInvalidParams, "%s", err.Error())
	}
	resolver, ok := backend.(SessionConfigResolver)
	if !ok {
		return ahptypes.ResolveSessionConfigResult{Schema: ahptypes.SessionConfigSchema{Type: "object"}, Values: map[string]json.RawMessage{}}, nil
	}
	result, err := resolver.ResolveSessionConfig(ctx, c.principal, params)
	if rejection, ok := errors.AsType[*Rejection](err); ok {
		return nil, rpcErr(ahptypes.ErrorCodeInvalidParams, "%s", rejection.Reason)
	}
	return result, err
}

// createResourceWatch starts a watch that lives as long as this connection
// keeps it: unsubscribing from its channel or closing releases it.
func (c *conn) createResourceWatch(ctx context.Context, raw json.RawMessage) (any, error) {
	var params ahptypes.CreateResourceWatchParams
	if err := decodeParams(raw, &params); err != nil {
		return nil, err
	}
	notFound := rpcErr(ahptypes.ErrorCodeNotFound, "not found")
	if params.Uri == "" || len(params.Uri) > maxURILength {
		return nil, notFound
	}
	backend, watcher, ok := firstOffering[ResourceWatcher](c.host.cfg.Backends)
	if !ok {
		return nil, rpcErr(ahptypes.ErrorCodeMethodNotFound, "method %q is not supported", "createResourceWatch")
	}
	channel := resourceWatchScheme + randomID()
	watchCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	// The watch is live before its channel is stored, so a sweep never takes
	// a stored channel for one an earlier process left behind.
	c.mu.Lock()
	if len(c.watches) >= maxWatchesPerConnection {
		c.mu.Unlock()
		stop()
		return nil, rpcErr(ahptypes.ErrorCodeInvalidRequest, "too many resource watches")
	}
	c.watches[channel] = stop
	c.mu.Unlock()
	recursive := params.Recursive != nil && *params.Recursive
	state := ahptypes.ResourceWatchState{Root: params.Uri, Recursive: recursive, Includes: params.Includes, Excludes: params.Excludes}
	if err := c.host.storeChannels(ctx, c.principal.UserKey, map[string]storedChannel{
		channel: {Backend: backend.Name(), State: ahptypes.SnapshotState{ResourceWatch: &state}},
	}); err != nil {
		c.releaseWatch(channel)
		return nil, err
	}
	started := make(chan error, 1)
	go func() {
		err := watcher.WatchResources(watchCtx, c.principal, params, func(changes []ahptypes.ResourceChange) error {
			select {
			case started <- nil:
			default:
			}
			if len(changes) == 0 {
				return nil
			}
			batch, _ := json.Marshal(map[string]any{"items": changes})
			return c.host.apply(watchCtx, c.principal, channel, []ahptypes.StateAction{{Value: &ahptypes.ResourceWatchChangedAction{
				Type: ahptypes.ActionTypeResourceWatchChanged, Changes: batch,
			}}}, nil)
		})
		select {
		case started <- err:
		default:
		}
		c.releaseWatch(channel)
	}()
	if err := <-started; err != nil {
		if errors.Is(err, ErrResourceNotFound) {
			return nil, notFound
		}
		if rejection, ok := errors.AsType[*Rejection](err); ok {
			return nil, rpcErr(ahptypes.ErrorCodeInvalidParams, "%s", rejection.Reason)
		}
		return nil, fmt.Errorf("start the resource watch: %w", err)
	}
	return ahptypes.CreateResourceWatchResult{Channel: channel}, nil
}

// releaseWatch stops a watch and forgets its channel.
func (c *conn) releaseWatch(channel string) {
	c.mu.Lock()
	stop, ok := c.watches[channel]
	delete(c.watches, channel)
	c.mu.Unlock()
	if !ok {
		return
	}
	stop()
	if err := c.host.cfg.Store.DeleteAHPChannelStates(context.Background(), c.principal.UserKey, []string{channel}); err != nil {
		// The channel outlives its watch. The user's next initialize or
		// reconnect sweeps it, as it does a channel an earlier process left,
		// rather than find it and take a watch to be running.
		c.host.unsweep(c.principal.UserKey)
	}
}

// unsweep has the user's next initialize or reconnect sweep again.
func (h *Host) unsweep(userKey string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.swept, userKey)
	if h.sweeping[userKey] != nil {
		if h.resweep == nil {
			h.resweep = map[string]bool{}
		}
		h.resweep[userKey] = true
	}
}

// releaseWatches stops every watch of a closing connection.
func (c *conn) releaseWatches() {
	c.mu.Lock()
	channels := make([]string, 0, len(c.watches))
	for channel := range c.watches {
		channels = append(channels, channel)
	}
	c.mu.Unlock()
	for _, channel := range channels {
		c.releaseWatch(channel)
	}
}

func isResourceWatchChannel(channel string) bool {
	return strings.HasPrefix(channel, resourceWatchScheme)
}

func randomID() string {
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	return hex.EncodeToString(id)
}

// sweepOnce drops, once per user in this process, the channels an earlier
// process left behind: resource watches, which never outlive their
// connection, and terminals whose shells are gone. A connection arriving while
// the user's sweep runs waits for it, so it never reads a channel about to go.
// A sweep that fails is tried again by the user's next initialize or
// reconnect.
func (h *Host) sweepOnce(ctx context.Context, principal Principal) {
	h.mu.Lock()
	if h.swept == nil {
		h.swept = map[string]bool{}
		h.sweeping = map[string]chan struct{}{}
	}
	if h.swept[principal.UserKey] {
		h.mu.Unlock()
		return
	}
	if running := h.sweeping[principal.UserKey]; running != nil {
		h.mu.Unlock()
		select {
		case <-running:
		case <-ctx.Done():
		}
		return
	}
	done := make(chan struct{})
	h.sweeping[principal.UserKey] = done
	h.mu.Unlock()
	err := h.sweep(ctx, principal)
	h.mu.Lock()
	// A channel left behind while the sweep ran may have been listed live.
	if err == nil && !h.resweep[principal.UserKey] {
		h.swept[principal.UserKey] = true
	}
	delete(h.resweep, principal.UserKey)
	delete(h.sweeping, principal.UserKey)
	h.mu.Unlock()
	close(done)
}

func (h *Host) sweep(ctx context.Context, principal Principal) error {
	// List the stored channels before the live ones: a watch is live and a
	// terminal's shell starts before its channel is stored, so a channel in
	// use is never swept, also when a retried sweep runs after this process
	// created some.
	watches, err := h.cfg.Store.ListAHPChannels(ctx, principal.UserKey, resourceWatchScheme)
	if err != nil {
		return err
	}
	terminals, err := h.cfg.Store.ListAHPChannels(ctx, principal.UserKey, terminalScheme)
	if err != nil {
		return err
	}
	live, err := h.cfg.Backends.terminals(ctx, principal)
	if err != nil {
		return err
	}
	inUse := h.liveWatches(principal.UserKey)
	for _, terminal := range live {
		inUse[terminal.Resource] = true
	}
	var stale, staleTerminals []string
	for _, channel := range watches {
		if !inUse[channel] {
			stale = append(stale, channel)
		}
	}
	for _, channel := range terminals {
		if !inUse[channel] {
			staleTerminals = append(staleTerminals, channel)
		}
	}
	// A terminal an earlier process left can still run its shell in the
	// user's workspace, which outlives the process: stop it before its
	// channel, the last trace of it, goes.
	if len(staleTerminals) > 0 {
		if _, provider, ok := h.cfg.Backends.terminalProvider(); ok {
			if reaper, ok := provider.(TerminalReaper); ok {
				// Their channels stay until their shells are known stopped:
				// a failed sweep runs again.
				if err := reaper.ReapTerminals(ctx, principal, staleTerminals); err != nil {
					return err
				}
			}
		}
	}
	stale = append(stale, staleTerminals...)
	if len(stale) == 0 {
		return nil
	}
	return h.cfg.Store.DeleteAHPChannelStates(ctx, principal.UserKey, stale)
}

// liveWatches returns the channels of the user's resource watches open in
// this process.
func (h *Host) liveWatches(userKey string) map[string]bool {
	hub := h.hub(userKey)
	hub.mu.Lock()
	defer hub.mu.Unlock()
	live := map[string]bool{}
	for c := range hub.conns {
		c.mu.Lock()
		for channel := range c.watches {
			live[channel] = true
		}
		c.mu.Unlock()
	}
	return live
}
