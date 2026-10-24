/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package ahphost

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/store"
)

// resourceBackend serves a listing, a session config and watches that report
// one change and then wait to be released.
type resourceBackend struct {
	*extendedBackend
	released chan struct{}
}

func (b *resourceBackend) ListResource(_ context.Context, _ Principal, uri string) ([]ahptypes.DirectoryEntry, error) {
	if uri != "file:///work" {
		return nil, ErrResourceNotFound
	}
	return []ahptypes.DirectoryEntry{{Name: "a.go", Type: "file"}, {Name: "pkg", Type: "directory"}}, nil
}

func (b *resourceBackend) WatchResources(ctx context.Context, _ Principal, params ahptypes.CreateResourceWatchParams, changed func([]ahptypes.ResourceChange)) error {
	if params.Uri != "file:///work" {
		return ErrResourceNotFound
	}
	changed(nil)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			close(b.released)
			return nil
		case <-ticker.C:
			changed([]ahptypes.ResourceChange{{Uri: "file:///work/a.go", Type: ahptypes.ResourceChangeTypeUpdated}})
		}
	}
}

func (b *resourceBackend) ResolveSessionConfig(_ context.Context, _ Principal, params ahptypes.ResolveSessionConfigParams) (ahptypes.ResolveSessionConfigResult, error) {
	return ahptypes.ResolveSessionConfigResult{
		Schema: ahptypes.SessionConfigSchema{Type: "object"},
		Values: map[string]json.RawMessage{"echo": json.RawMessage(`"` + *params.WorkingDirectory + `"`)},
	}, nil
}

func TestResourceListsWatchesAndSessionConfig(t *testing.T) {
	env, extended := newExtendedEnv(t)
	backend := &resourceBackend{extendedBackend: extended, released: make(chan struct{})}
	env.host.cfg.Backends.mu.Lock()
	env.host.cfg.Backends.backends = []SessionBackend{backend}
	env.host.cfg.Backends.mu.Unlock()
	minted := env.mint("alice")
	client := env.connect(minted.Token)
	ctx := ctx5(t)
	_, err := client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)

	var listed ahptypes.ResourceListResult
	require.NoError(t, client.Request(ctx, "resourceList", ahptypes.ResourceListParams{Channel: rootChannel, Uri: "file:///work"}, &listed))
	require.Equal(t, []ahptypes.DirectoryEntry{{Name: "a.go", Type: "file"}, {Name: "pkg", Type: "directory"}}, listed.Entries)
	err = client.Request(ctx, "resourceList", ahptypes.ResourceListParams{Channel: rootChannel, Uri: "file:///etc"}, &listed)
	require.Equal(t, ahptypes.ErrorCodeNotFound, rpcCode(t, err))

	dir := ahptypes.URI("file:///work")
	var resolved ahptypes.ResolveSessionConfigResult
	require.NoError(t, client.Request(ctx, "resolveSessionConfig", ahptypes.ResolveSessionConfigParams{Channel: rootChannel, WorkingDirectory: &dir}, &resolved))
	require.JSONEq(t, `"file:///work"`, string(resolved.Values["echo"]))

	var watch ahptypes.CreateResourceWatchResult
	require.NoError(t, client.Request(ctx, "createResourceWatch", ahptypes.CreateResourceWatchParams{Channel: rootChannel, Uri: "file:///work"}, &watch))
	require.True(t, isResourceWatchChannel(watch.Channel))
	snapshot, sub, err := client.Subscribe(ctx, watch.Channel)
	require.NoError(t, err)
	require.Equal(t, ahptypes.URI("file:///work"), snapshot.Snapshot.State.ResourceWatch.Root)
	userKey := ahpUserKey(t, env, minted.Token)
	for seen := false; !seen; {
		select {
		case event := <-sub.Events():
			if action, ok := event.(ahp.SubscriptionEventAction); ok {
				changed, ok := action.Envelope.Action.Value.(*ahptypes.ResourceWatchChangedAction)
				require.True(t, ok)
				require.JSONEq(t, `{"items":[{"uri":"file:///work/a.go","type":"updated"}]}`, string(changed.Changes))
				seen = true
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no change arrived")
		}
	}

	// Unsubscribing releases the watch and its channel.
	require.NoError(t, client.Notify(ctx, "unsubscribe", ahptypes.UnsubscribeParams(watch)))
	select {
	case <-backend.released:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch was never released")
	}
	require.Eventually(t, func() bool {
		_, err := env.store.GetAHPChannelState(context.Background(), userKey, watch.Channel)
		return err != nil
	}, 5*time.Second, 20*time.Millisecond)
	_, err = env.store.GetAHPChannelState(context.Background(), userKey, watch.Channel)
	require.ErrorIs(t, err, store.ErrNotFound)

	err = client.Request(ctx, "createResourceWatch", ahptypes.CreateResourceWatchParams{Channel: rootChannel, Uri: "file:///etc"}, &watch)
	require.Equal(t, ahptypes.ErrorCodeNotFound, rpcCode(t, err))
}

func TestLeftoverChannelsAreSweptOnce(t *testing.T) {
	env, _ := newExtendedEnv(t)
	minted := env.mint("alice")
	userKey := ahpUserKey(t, env, minted.Token)
	ctx := ctx5(t)
	state := ahptypes.TerminalState{Content: []ahptypes.TerminalContentPart{}}
	watch := ahptypes.ResourceWatchState{Root: "file:///work"}
	require.NoError(t, env.host.storeChannels(ctx, userKey, map[string]storedChannel{
		"ahp-terminal:/gone":             {Backend: "fake", State: ahptypes.SnapshotState{Terminal: &state}},
		resourceWatchScheme + "leftover": {Backend: "fake", State: ahptypes.SnapshotState{ResourceWatch: &watch}},
	}))
	_, err := env.store.GetAHPChannelState(ctx, userKey, "ahp-terminal:/gone")
	require.NoError(t, err)
	client := env.connect(minted.Token)
	_, err = client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	for _, channel := range []string{"ahp-terminal:/gone", resourceWatchScheme + "leftover"} {
		_, err := env.store.GetAHPChannelState(ctx, userKey, channel)
		require.ErrorIs(t, err, store.ErrNotFound, channel)
	}
}

func TestIdleConnectionsArePinged(t *testing.T) {
	env, _ := newExtendedEnv(t)
	env.host.cfg.PingInterval = 50 * time.Millisecond
	client := env.connect(env.mint("alice").Token)
	ctx := ctx5(t)
	_, err := client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	// A client that answers pings stays connected well past several of them.
	time.Sleep(300 * time.Millisecond)
	require.NoError(t, client.Request(ctx, "ping", map[string]any{}, nil))
}
