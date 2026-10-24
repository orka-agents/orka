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
	WatchResources(ctx context.Context, principal Principal, params ahptypes.CreateResourceWatchParams, changed func([]ahptypes.ResourceChange)) error
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
	c.mu.Lock()
	if len(c.watches) >= maxWatchesPerConnection {
		c.mu.Unlock()
		return nil, rpcErr(ahptypes.ErrorCodeInvalidRequest, "too many resource watches")
	}
	c.mu.Unlock()
	channel := resourceWatchScheme + randomID()
	recursive := params.Recursive != nil && *params.Recursive
	state := ahptypes.ResourceWatchState{Root: params.Uri, Recursive: recursive, Includes: params.Includes, Excludes: params.Excludes}
	if err := c.host.storeChannels(ctx, c.principal.UserKey, map[string]storedChannel{
		channel: {Backend: backend.Name(), State: ahptypes.SnapshotState{ResourceWatch: &state}},
	}); err != nil {
		return nil, err
	}
	watchCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	c.mu.Lock()
	c.watches[channel] = stop
	c.mu.Unlock()
	started := make(chan error, 1)
	go func() {
		err := watcher.WatchResources(watchCtx, c.principal, params, func(changes []ahptypes.ResourceChange) {
			select {
			case started <- nil:
			default:
			}
			if len(changes) == 0 {
				return
			}
			batch, _ := json.Marshal(map[string]any{"items": changes})
			_ = c.host.apply(watchCtx, c.principal, channel, []ahptypes.StateAction{{Value: &ahptypes.ResourceWatchChangedAction{
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
	_ = c.host.cfg.Store.DeleteAHPChannelStates(context.Background(), c.principal.UserKey, []string{channel})
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
// connection, and terminals whose shells are gone.
func (h *Host) sweepOnce(ctx context.Context, principal Principal) {
	h.mu.Lock()
	if h.swept == nil {
		h.swept = map[string]bool{}
	}
	if h.swept[principal.UserKey] {
		h.mu.Unlock()
		return
	}
	h.swept[principal.UserKey] = true
	h.mu.Unlock()
	stale, err := h.cfg.Store.ListAHPChannels(ctx, principal.UserKey, resourceWatchScheme)
	if err != nil {
		return
	}
	// List the stored terminals before the live ones: a terminal's shell
	// starts before its channel is stored, so a newer channel is never swept.
	terminals, err := h.cfg.Store.ListAHPChannels(ctx, principal.UserKey, terminalScheme)
	if err != nil {
		return
	}
	live, err := h.cfg.Backends.terminals(ctx, principal)
	if err != nil {
		return
	}
	running := map[string]bool{}
	for _, terminal := range live {
		running[terminal.Resource] = true
	}
	for _, channel := range terminals {
		if !running[channel] {
			stale = append(stale, channel)
		}
	}
	if len(stale) > 0 {
		_ = h.cfg.Store.DeleteAHPChannelStates(ctx, principal.UserKey, stale)
	}
}
