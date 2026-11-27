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
	"slices"
	"strings"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

const (
	resourceWatchScheme = "ahp-resource-watch:/"
	terminalScheme      = "ahp-terminal:"
)

type ResourceLister interface {
	ListResource(ctx context.Context, principal Principal, uri string) ([]ahptypes.DirectoryEntry, error)
}

type ResourceWatcher interface {
	WatchResources(ctx context.Context, principal Principal, params ahptypes.CreateResourceWatchParams, changed func([]ahptypes.ResourceChange) error) error
}

type SessionConfigResolver interface {
	ResolveSessionConfig(ctx context.Context, principal Principal, params ahptypes.ResolveSessionConfigParams) (ahptypes.ResolveSessionConfigResult, error)
}

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
		c.host.unsweep(c.principal.UserKey)
	}
}

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
	if err == nil && !h.resweep[principal.UserKey] {
		h.swept[principal.UserKey] = true
	}
	delete(h.resweep, principal.UserKey)
	delete(h.sweeping, principal.UserKey)
	h.mu.Unlock()
	close(done)
}

func (h *Host) sweep(ctx context.Context, principal Principal) error {
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
	if len(staleTerminals) > 0 {
		if _, provider, ok := h.cfg.Backends.terminalProvider(); ok {
			if reaper, ok := provider.(TerminalReaper); ok {
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

func (h *Host) dropOrphanedTerminals(ctx context.Context, principal Principal) error {
	stored, err := h.cfg.Store.ListAHPChannels(ctx, principal.UserKey, terminalScheme)
	if err != nil || len(stored) == 0 {
		return err
	}
	live, err := h.cfg.Backends.terminals(ctx, principal)
	if err != nil {
		return err
	}
	keep := h.followed(principal.UserKey)
	for _, terminal := range live {
		keep[terminal.Resource] = true
	}
	var held []string
	for _, channel := range stored {
		if keep[channel] {
			continue
		}
		release, claimed := h.claimChannelURI(principal.UserKey, channel, false)
		if !claimed {
			continue
		}
		defer release()
		held = append(held, channel)
	}
	if len(held) == 0 {
		return nil
	}
	live, err = h.cfg.Backends.terminals(ctx, principal)
	if err != nil {
		return err
	}
	running := map[string]bool{}
	for _, terminal := range live {
		running[terminal.Resource] = true
	}
	held = slices.DeleteFunc(held, func(channel string) bool { return running[channel] })
	if len(held) == 0 {
		return nil
	}
	if _, provider, ok := h.cfg.Backends.terminalProvider(); ok {
		if reaper, ok := provider.(TerminalReaper); ok {
			if err := reaper.ReapTerminals(ctx, principal, held); err != nil {
				return err
			}
		}
	}
	return h.locked(ctx, principal.UserKey, func(int64) error {
		followed := h.followedLocked(h.hub(principal.UserKey))
		orphaned := slices.DeleteFunc(held, func(channel string) bool { return followed[channel] })
		if len(orphaned) == 0 {
			return nil
		}
		return h.cfg.Store.DeleteAHPChannelStates(ctx, principal.UserKey, orphaned)
	})
}

func (h *Host) followed(userKey string) map[string]bool {
	hub := h.hub(userKey)
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return h.followedLocked(hub)
}

func (h *Host) followedLocked(hub *userHub) map[string]bool {
	followed := map[string]bool{}
	for c := range hub.conns {
		c.mu.Lock()
		for channel := range c.subs {
			followed[channel] = true
		}
		c.mu.Unlock()
	}
	return followed
}

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
