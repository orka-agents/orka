/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package ahphost

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
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

func (b *resourceBackend) WatchResources(ctx context.Context, _ Principal, params ahptypes.CreateResourceWatchParams, changed func([]ahptypes.ResourceChange) error) error {
	if params.Uri == "file:///refused" {
		return &Rejection{Reason: "too many patterns"}
	}
	if params.Uri != "file:///work" {
		return ErrResourceNotFound
	}
	_ = changed(nil)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			close(b.released)
			return nil
		case <-ticker.C:
			_ = changed([]ahptypes.ResourceChange{{Uri: "file:///work/a.go", Type: ahptypes.ResourceChangeTypeUpdated}})
		}
	}
}

// retryingWatchBackend reports one change once told to, again on every tick
// until it is recorded, as a poller that keeps its baseline does.
type retryingWatchBackend struct {
	*extendedBackend
	report   chan struct{}
	failures atomic.Int32
}

func (b *retryingWatchBackend) WatchResources(ctx context.Context, _ Principal, _ ahptypes.CreateResourceWatchParams, changed func([]ahptypes.ResourceChange) error) error {
	_ = changed(nil)
	select {
	case <-b.report:
	case <-ctx.Done():
		return nil
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for changed([]ahptypes.ResourceChange{{Uri: "file:///work/a.go", Type: ahptypes.ResourceChangeTypeUpdated}}) != nil {
		b.failures.Add(1)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
	<-ctx.Done()
	return nil
}

// A change a watch cannot record is reported as not recorded, so the watcher
// reports it again instead of losing it.
func TestAWatchChangeThatCannotBeRecordedIsReportedAgain(t *testing.T) {
	env, extended := newExtendedEnv(t)
	backend := &retryingWatchBackend{extendedBackend: extended, report: make(chan struct{})}
	env.host.cfg.Backends.mu.Lock()
	env.host.cfg.Backends.backends = []SessionBackend{backend}
	env.host.cfg.Backends.mu.Unlock()
	failing := &failingSessionStore{AHPStore: env.host.cfg.Store}
	env.host.cfg.Store = failing
	client := env.connect(env.mint("alice").Token)
	ctx := ctx5(t)
	_, err := client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	var watch ahptypes.CreateResourceWatchResult
	require.NoError(t, client.Request(ctx, "createResourceWatch", ahptypes.CreateResourceWatchParams{Channel: rootChannel, Uri: "file:///work"}, &watch))
	_, sub, err := client.Subscribe(ctx, watch.Channel)
	require.NoError(t, err)

	failing.failActions.Store(true)
	close(backend.report)
	require.Eventually(t, func() bool { return backend.failures.Load() > 0 }, 5*time.Second, 10*time.Millisecond,
		"the watcher was never told the change was not recorded")
	failing.failActions.Store(false)
	changed, ok := nextAction(t, sub).Action.Value.(*ahptypes.ResourceWatchChangedAction)
	require.True(t, ok)
	require.JSONEq(t, `{"items":[{"uri":"file:///work/a.go","type":"updated"}]}`, string(changed.Changes))
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

// failingChannelListStore refuses to list channels while armed.
type failingChannelListStore struct {
	store.AHPStore
	armed atomic.Bool
}

func (s *failingChannelListStore) ListAHPChannels(ctx context.Context, userKey, prefix string) ([]string, error) {
	if s.armed.Load() {
		return nil, errors.New("the store refused the read")
	}
	return s.AHPStore.ListAHPChannels(ctx, userKey, prefix)
}

// A sweep that fails runs again on the user's next initialize, and spares the
// resource watches this process opened meanwhile.
func TestAFailedSweepRunsAgainAndSparesLiveWatches(t *testing.T) {
	env, extended := newExtendedEnv(t)
	backend := &resourceBackend{extendedBackend: extended, released: make(chan struct{})}
	env.host.cfg.Backends.mu.Lock()
	env.host.cfg.Backends.backends = []SessionBackend{backend}
	env.host.cfg.Backends.mu.Unlock()
	failing := &failingChannelListStore{AHPStore: env.host.cfg.Store}
	env.host.cfg.Store = failing
	minted := env.mint("alice")
	userKey := ahpUserKey(t, env, minted.Token)
	ctx := ctx5(t)
	state := ahptypes.TerminalState{Content: []ahptypes.TerminalContentPart{}}
	watch := ahptypes.ResourceWatchState{Root: "file:///work"}
	require.NoError(t, env.host.storeChannels(ctx, userKey, map[string]storedChannel{
		"ahp-terminal:/gone":             {Backend: "fake", State: ahptypes.SnapshotState{Terminal: &state}},
		resourceWatchScheme + "leftover": {Backend: "fake", State: ahptypes.SnapshotState{ResourceWatch: &watch}},
	}))

	failing.armed.Store(true)
	first := env.connect(minted.Token)
	_, err := first.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	_, err = env.store.GetAHPChannelState(ctx, userKey, "ahp-terminal:/gone")
	require.NoError(t, err, "the failed sweep removed nothing")

	failing.armed.Store(false)
	var opened ahptypes.CreateResourceWatchResult
	require.NoError(t, first.Request(ctx, "createResourceWatch", ahptypes.CreateResourceWatchParams{Channel: rootChannel, Uri: "file:///work"}, &opened))
	second := env.connect(minted.Token)
	_, err = second.Initialize(ctx, "client-b", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	for _, channel := range []string{"ahp-terminal:/gone", resourceWatchScheme + "leftover"} {
		_, err := env.store.GetAHPChannelState(ctx, userKey, channel)
		require.ErrorIs(t, err, store.ErrNotFound, channel)
	}
	_, err = env.store.GetAHPChannelState(ctx, userKey, opened.Channel)
	require.NoError(t, err, "a watch open in this process is not swept")
}

// A watch whose channel cannot be deleted when its connection closes leaves
// the channel to the user's next initialize, which sweeps it: a reconnect
// must not find it and take the watch to be running.
func TestAWatchChannelLeftBehindIsSweptLater(t *testing.T) {
	env, extended := newExtendedEnv(t)
	backend := &resourceBackend{extendedBackend: extended, released: make(chan struct{})}
	env.host.cfg.Backends.mu.Lock()
	env.host.cfg.Backends.backends = []SessionBackend{backend}
	env.host.cfg.Backends.mu.Unlock()
	failing := &failingCleanupStore{failingSessionStore: &failingSessionStore{AHPStore: env.host.cfg.Store}}
	env.host.cfg.Store = failing
	minted := env.mint("alice")
	userKey := ahpUserKey(t, env, minted.Token)
	ctx := ctx5(t)

	first := env.connect(minted.Token)
	_, err := first.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	var opened ahptypes.CreateResourceWatchResult
	require.NoError(t, first.Request(ctx, "createResourceWatch", ahptypes.CreateResourceWatchParams{Channel: rootChannel, Uri: "file:///work"}, &opened))
	failing.failDelete.Store(true)
	require.NoError(t, first.Shutdown(ctx))
	<-backend.released
	require.Eventually(t, func() bool {
		env.host.mu.Lock()
		defer env.host.mu.Unlock()
		return !env.host.swept[userKey]
	}, 5*time.Second, 10*time.Millisecond, "the failed deletion leaves the channel to a sweep")
	_, err = env.store.GetAHPChannelState(ctx, userKey, opened.Channel)
	require.NoError(t, err, "the channel outlived its watch")

	failing.failDelete.Store(false)
	second := env.connect(minted.Token)
	_, err = second.Initialize(ctx, "client-b", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	_, err = env.store.GetAHPChannelState(ctx, userKey, opened.Channel)
	require.ErrorIs(t, err, store.ErrNotFound)
}

// A watch its backend refuses is refused with the backend's reason, as
// invalid parameters, and leaves no channel.
func TestARefusedWatchReportsItsReason(t *testing.T) {
	env, extended := newExtendedEnv(t)
	backend := &resourceBackend{extendedBackend: extended, released: make(chan struct{})}
	env.host.cfg.Backends.mu.Lock()
	env.host.cfg.Backends.backends = []SessionBackend{backend}
	env.host.cfg.Backends.mu.Unlock()
	minted := env.mint("alice")
	userKey := ahpUserKey(t, env, minted.Token)
	ctx := ctx5(t)
	client := env.connect(minted.Token)
	_, err := client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	err = client.Request(ctx, "createResourceWatch", ahptypes.CreateResourceWatchParams{Channel: rootChannel, Uri: "file:///refused"}, nil)
	require.ErrorContains(t, err, "too many patterns")
	channels, err := env.store.ListAHPChannels(ctx, userKey, resourceWatchScheme)
	require.NoError(t, err)
	require.Empty(t, channels)
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

// gatedChannelListStore holds the first listing of terminal channels until
// released, so a sweep can be caught while it runs.
type gatedChannelListStore struct {
	store.AHPStore
	entered, release chan struct{}
	once             sync.Once
}

func (s *gatedChannelListStore) ListAHPChannels(ctx context.Context, userKey, prefix string) ([]string, error) {
	if prefix == terminalScheme {
		first := false
		s.once.Do(func() { first = true })
		if first {
			close(s.entered)
			<-s.release
		}
	}
	return s.AHPStore.ListAHPChannels(ctx, userKey, prefix)
}

// A connection that arrives while another's sweep runs waits for it, so it
// never sees a channel the sweep is about to remove.
func TestAConnectionWaitsForTheSweepInProgress(t *testing.T) {
	env, _ := newExtendedEnv(t)
	gated := &gatedChannelListStore{AHPStore: env.host.cfg.Store, entered: make(chan struct{}), release: make(chan struct{})}
	env.host.cfg.Store = gated
	minted := env.mint("alice")
	userKey := ahpUserKey(t, env, minted.Token)
	ctx := ctx5(t)
	state := ahptypes.TerminalState{Content: []ahptypes.TerminalContentPart{}}
	require.NoError(t, env.host.storeChannels(ctx, userKey, map[string]storedChannel{
		"ahp-terminal:/gone": {Backend: "fake", State: ahptypes.SnapshotState{Terminal: &state}},
	}))

	first, second := env.connect(minted.Token), env.connect(minted.Token)
	firstDone := make(chan error, 1)
	go func() {
		_, err := first.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
		firstDone <- err
	}()
	<-gated.entered
	secondDone := make(chan error, 1)
	go func() {
		_, err := second.Initialize(ctx, "client-b", ahptypes.SupportedProtocolVersions(), nil)
		secondDone <- err
	}()
	select {
	case err := <-secondDone:
		t.Fatalf("the second connection initialized while the sweep ran: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(gated.release)
	require.NoError(t, <-firstDone)
	require.NoError(t, <-secondDone)
	_, err := env.store.GetAHPChannelState(ctx, userKey, "ahp-terminal:/gone")
	require.ErrorIs(t, err, store.ErrNotFound)
}
