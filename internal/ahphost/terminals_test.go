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
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

type terminalBackend struct {
	*fakeBackend
	mu          sync.Mutex
	inputs      []string
	running     map[string]bool
	disposed    []string
	reaped      []string
	reapFails   int
	exitAtOnce  *int64
	onReap      func(channels []string)
	onTerminals func()
}

func (b *terminalBackend) CreateTerminal(ctx context.Context, _ Principal, params ahptypes.CreateTerminalParams, emit Emitter) (ahptypes.TerminalState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.exitAtOnce != nil {
		_ = emit.Emit(ctx, params.Channel, ahptypes.StateAction{Value: &ahptypes.TerminalExitedAction{
			Type: ahptypes.ActionTypeTerminalExited, ExitCode: b.exitAtOnce,
		}})
	} else {
		b.running[params.Channel] = true
	}
	return ahptypes.TerminalState{Title: "shell", Claim: params.Claim,
		Lifecycle: ahptypes.TerminalLifecycleState{Value: &ahptypes.TerminalRunningLifecycleState{Status: ahptypes.TerminalLifecycleStatusRunning}}}, nil
}

func (b *terminalBackend) ReapTerminals(_ context.Context, _ Principal, channels []string) error {
	if b.onReap != nil {
		b.onReap(channels)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.reapFails > 0 {
		b.reapFails--
		return errors.New("the workspace could not be reached")
	}
	b.reaped = append(b.reaped, channels...)
	return nil
}

func (b *terminalBackend) DisposeTerminal(_ context.Context, _ Principal, channel string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.running, channel)
	b.disposed = append(b.disposed, channel)
	return nil
}

func (b *terminalBackend) Terminals(context.Context, Principal) ([]ahptypes.TerminalInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.onTerminals != nil {
		b.onTerminals()
	}
	out := make([]ahptypes.TerminalInfo, 0, len(b.running))
	for channel := range b.running {
		out = append(out, ahptypes.TerminalInfo{Resource: channel, Title: "shell",
			Claim:     ahptypes.TerminalClaim{Value: &ahptypes.TerminalClientClaim{Kind: ahptypes.TerminalClaimKindClient, ClientId: "client-a"}},
			Lifecycle: ahptypes.TerminalLifecycleState{Value: &ahptypes.TerminalRunningLifecycleState{Status: ahptypes.TerminalLifecycleStatusRunning}}})
	}
	return out, nil
}

func (b *terminalBackend) AuthorizeClientAction(context.Context, Principal, string, ahptypes.StateAction) error {
	return nil
}

func (b *terminalBackend) HandleClientAction(_ context.Context, _ Principal, _ string, action ahptypes.StateAction, _ Emitter) {
	if input, ok := action.Value.(*ahptypes.TerminalInputAction); ok {
		time.Sleep(time.Millisecond)
		b.mu.Lock()
		b.inputs = append(b.inputs, input.Data)
		b.mu.Unlock()
	}
}

func TestATerminalWhoseShellExitsAtOnceIsStoredAsExited(t *testing.T) {
	s, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	require.NoError(t, err)
	tokens := &TokenService{Store: s}
	code := int64(3)
	backend := &terminalBackend{fakeBackend: &fakeBackend{}, running: map[string]bool{}, exitAtOnce: &code}
	registry, err := NewRegistry(backend)
	require.NoError(t, err)
	host, err := New(Config{Store: s, Tokens: tokens, Backends: registry, Logger: logr.Discard()})
	require.NoError(t, err)
	server := httptest.NewServer(host)
	t.Cleanup(server.Close)
	env := &testEnv{t: t, host: host, tokens: tokens, store: s, backend: backend.fakeBackend, server: server}
	client := env.connect(env.mint("alice").Token)
	ctx := ctx5(t)
	_, err = client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)

	claim := ahptypes.TerminalClaim{Value: &ahptypes.TerminalClientClaim{Kind: ahptypes.TerminalClaimKindClient, ClientId: "client-a"}}
	require.NoError(t, client.Request(ctx, "createTerminal", ahptypes.CreateTerminalParams{Channel: "ahp-terminal:/one", Claim: claim}, nil))
	stored, _, err := host.readChannel(ctx, UserKey("https://issuer.example", "alice"), "ahp-terminal:/one")
	require.NoError(t, err)
	exited, ok := stored.State.Terminal.Lifecycle.Value.(*ahptypes.TerminalExitedLifecycleState)
	require.True(t, ok, "the stored terminal = %#v, want exited", stored.State.Terminal.Lifecycle.Value)
	require.Equal(t, code, *exited.ExitCode)
	result, _, err := client.Subscribe(ctx, rootChannel)
	require.NoError(t, err)
	require.Empty(t, result.Snapshot.State.Root.Terminals, "an exited shell is not listed as running")
}

func TestEndedTerminalsNothingFollowsGoWithTheNextTerminal(t *testing.T) {
	s, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	require.NoError(t, err)
	tokens := &TokenService{Store: s}
	code := int64(0)
	backend := &terminalBackend{fakeBackend: &fakeBackend{}, running: map[string]bool{}, exitAtOnce: &code}
	registry, err := NewRegistry(backend)
	require.NoError(t, err)
	host, err := New(Config{Store: s, Tokens: tokens, Backends: registry, Logger: logr.Discard()})
	require.NoError(t, err)
	server := httptest.NewServer(host)
	t.Cleanup(server.Close)
	env := &testEnv{t: t, host: host, tokens: tokens, store: s, backend: backend.fakeBackend, server: server}
	client := env.connect(env.mint("alice").Token)
	ctx := ctx5(t)
	_, err = client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	alice := UserKey("https://issuer.example", "alice")
	claim := ahptypes.TerminalClaim{Value: &ahptypes.TerminalClientClaim{Kind: ahptypes.TerminalClaimKindClient, ClientId: "client-a"}}
	create := func(channel string) {
		require.NoError(t, client.Request(ctx, "createTerminal", ahptypes.CreateTerminalParams{Channel: channel, Claim: claim}, nil))
	}
	stored := func() []string {
		channels, err := s.ListAHPChannels(ctx, alice, terminalScheme)
		require.NoError(t, err)
		return channels
	}

	create("ahp-terminal:/one")
	create("ahp-terminal:/two")
	require.ElementsMatch(t, []string{"ahp-terminal:/one", "ahp-terminal:/two"}, stored(), "ended terminals the client follows stay")
	require.NoError(t, client.Unsubscribe(ctx, "ahp-terminal:/one"))
	create("ahp-terminal:/three")
	require.ElementsMatch(t, []string{"ahp-terminal:/two", "ahp-terminal:/three"}, stored(), "an ended terminal nothing follows goes")
	backend.mu.Lock()
	require.Equal(t, []string{"ahp-terminal:/one"}, backend.reaped, "its shell is stopped first")
	backend.mu.Unlock()

	require.NoError(t, client.Unsubscribe(ctx, "ahp-terminal:/two"))
	other := env.connect(env.mint("alice").Token)
	_, err = other.Initialize(ctx, "client-b", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	var subscribeErr error
	backend.onReap = func([]string) {
		var raw json.RawMessage
		subscribeErr = other.Request(ctx, "subscribe", ahptypes.SubscribeParams{Channel: "ahp-terminal:/two"}, &raw)
	}
	create("ahp-terminal:/four")
	require.NoError(t, subscribeErr)
	require.Contains(t, stored(), "ahp-terminal:/two", "a terminal followed meanwhile is kept")

	backend.onReap = nil
	require.NoError(t, client.Unsubscribe(ctx, "ahp-terminal:/three"))
	require.NoError(t, other.Unsubscribe(ctx, "ahp-terminal:/two"))
	require.NoError(t, other.Request(ctx, "ping", map[string]any{}, nil))
	looks := 0
	backend.onTerminals = func() {
		looks++
		if looks == 2 {
			backend.running["ahp-terminal:/three"] = true
		}
	}
	backend.mu.Lock()
	backend.reaped = nil
	backend.mu.Unlock()
	create("ahp-terminal:/five")
	backend.mu.Lock()
	require.Equal(t, []string{"ahp-terminal:/two"}, backend.reaped, "a running shell is never reaped")
	backend.mu.Unlock()
	require.Contains(t, stored(), "ahp-terminal:/three")
}

func TestATerminalThatCannotBeFinishedIsTakenBack(t *testing.T) {
	s, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	require.NoError(t, err)
	failing := &failingSessionStore{AHPStore: s}
	tokens := &TokenService{Store: s}
	code := int64(0)
	backend := &terminalBackend{fakeBackend: &fakeBackend{}, running: map[string]bool{}, exitAtOnce: &code}
	registry, err := NewRegistry(backend)
	require.NoError(t, err)
	host, err := New(Config{Store: failing, Tokens: tokens, Backends: registry, Logger: logr.Discard()})
	require.NoError(t, err)
	server := httptest.NewServer(host)
	t.Cleanup(server.Close)
	env := &testEnv{t: t, host: host, tokens: tokens, store: s, backend: backend.fakeBackend, server: server}
	client := env.connect(env.mint("alice").Token)
	ctx := ctx5(t)
	_, err = client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)

	failing.failActions.Store(true)
	claim := ahptypes.TerminalClaim{Value: &ahptypes.TerminalClientClaim{Kind: ahptypes.TerminalClaimKindClient, ClientId: "client-a"}}
	require.Error(t, client.Request(ctx, "createTerminal", ahptypes.CreateTerminalParams{Channel: "ahp-terminal:/one", Claim: claim}, nil))
	_, err = s.GetAHPChannelState(ctx, UserKey("https://issuer.example", "alice"), "ahp-terminal:/one")
	require.ErrorIs(t, err, store.ErrNotFound, "no channel is left behind")
	backend.mu.Lock()
	defer backend.mu.Unlock()
	require.Equal(t, []string{"ahp-terminal:/one"}, backend.disposed, "the shell is disposed of")
}

func TestATerminalChangeOutlivesAFailedTerminalListUpdate(t *testing.T) {
	s, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	require.NoError(t, err)
	failing := &failingSessionStore{AHPStore: s}
	tokens := &TokenService{Store: s}
	backend := &terminalBackend{fakeBackend: &fakeBackend{}, running: map[string]bool{}}
	registry, err := NewRegistry(backend)
	require.NoError(t, err)
	host, err := New(Config{Store: failing, Tokens: tokens, Backends: registry, Logger: logr.Discard()})
	require.NoError(t, err)
	server := httptest.NewServer(host)
	t.Cleanup(server.Close)
	env := &testEnv{t: t, host: host, tokens: tokens, store: s, backend: backend.fakeBackend, server: server}
	client := env.connect(env.mint("alice").Token)
	ctx := ctx5(t)
	_, err = client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	alice := UserKey("https://issuer.example", "alice")
	claim := ahptypes.TerminalClaim{Value: &ahptypes.TerminalClientClaim{Kind: ahptypes.TerminalClaimKindClient, ClientId: "client-a"}}

	failing.failActions.Store(true)
	require.NoError(t, client.Request(ctx, "createTerminal", ahptypes.CreateTerminalParams{Channel: "ahp-terminal:/one", Claim: claim}, nil))
	_, err = s.GetAHPChannelState(ctx, alice, "ahp-terminal:/one")
	require.NoError(t, err, "the terminal stays")
	require.NoError(t, client.Request(ctx, "disposeTerminal", ahptypes.DisposeTerminalParams{Channel: "ahp-terminal:/one"}, nil))
	_, err = s.GetAHPChannelState(ctx, alice, "ahp-terminal:/one")
	require.ErrorIs(t, err, store.ErrNotFound, "the terminal is gone")

	failing.failActions.Store(false)
	require.NoError(t, client.Request(ctx, "createTerminal", ahptypes.CreateTerminalParams{Channel: "ahp-terminal:/two", Claim: claim}, nil))
	result, _, err := client.Subscribe(ctx, rootChannel)
	require.NoError(t, err)
	listed := make([]string, 0, len(result.Snapshot.State.Root.Terminals))
	for _, terminal := range result.Snapshot.State.Root.Terminals {
		listed = append(listed, terminal.Resource)
	}
	require.Equal(t, []string{"ahp-terminal:/two"}, listed)
}

func TestALeftoverTerminalIsReapedBeforeItsChannelGoes(t *testing.T) {
	s, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	require.NoError(t, err)
	tokens := &TokenService{Store: s}
	backend := &terminalBackend{fakeBackend: &fakeBackend{}, running: map[string]bool{"ahp-terminal:/live": true}}
	registry, err := NewRegistry(backend)
	require.NoError(t, err)
	host, err := New(Config{Store: s, Tokens: tokens, Backends: registry, Logger: logr.Discard()})
	require.NoError(t, err)
	server := httptest.NewServer(host)
	t.Cleanup(server.Close)
	env := &testEnv{t: t, host: host, tokens: tokens, store: s, backend: backend.fakeBackend, server: server}
	minted := env.mint("alice")
	userKey := UserKey("https://issuer.example", "alice")
	ctx := ctx5(t)
	state := ahptypes.TerminalState{Content: []ahptypes.TerminalContentPart{}}
	require.NoError(t, host.storeChannels(ctx, userKey, map[string]storedChannel{
		"ahp-terminal:/gone": {Backend: "fake", State: ahptypes.SnapshotState{Terminal: &state}},
		"ahp-terminal:/live": {Backend: "fake", State: ahptypes.SnapshotState{Terminal: &state}},
	}))
	client := env.connect(minted.Token)
	_, err = client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	backend.mu.Lock()
	require.Equal(t, []string{"ahp-terminal:/gone"}, backend.reaped)
	backend.mu.Unlock()
	_, err = s.GetAHPChannelState(ctx, userKey, "ahp-terminal:/gone")
	require.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetAHPChannelState(ctx, userKey, "ahp-terminal:/live")
	require.NoError(t, err)
}

func TestALeftoverTerminalStaysUntilItIsReaped(t *testing.T) {
	s, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	require.NoError(t, err)
	tokens := &TokenService{Store: s}
	backend := &terminalBackend{fakeBackend: &fakeBackend{}, running: map[string]bool{}, reapFails: 1}
	registry, err := NewRegistry(backend)
	require.NoError(t, err)
	host, err := New(Config{Store: s, Tokens: tokens, Backends: registry, Logger: logr.Discard()})
	require.NoError(t, err)
	server := httptest.NewServer(host)
	t.Cleanup(server.Close)
	env := &testEnv{t: t, host: host, tokens: tokens, store: s, backend: backend.fakeBackend, server: server}
	minted := env.mint("alice")
	userKey := UserKey("https://issuer.example", "alice")
	ctx := ctx5(t)
	state := ahptypes.TerminalState{Content: []ahptypes.TerminalContentPart{}}
	require.NoError(t, host.storeChannels(ctx, userKey, map[string]storedChannel{
		"ahp-terminal:/gone": {Backend: "fake", State: ahptypes.SnapshotState{Terminal: &state}},
	}))

	first := env.connect(minted.Token)
	_, err = first.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	_, err = s.GetAHPChannelState(ctx, userKey, "ahp-terminal:/gone")
	require.NoError(t, err, "a terminal whose shell could not be stopped keeps its channel")

	second := env.connect(minted.Token)
	_, err = second.Initialize(ctx, "client-b", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	backend.mu.Lock()
	require.Equal(t, []string{"ahp-terminal:/gone"}, backend.reaped)
	backend.mu.Unlock()
	_, err = s.GetAHPChannelState(ctx, userKey, "ahp-terminal:/gone")
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestTerminalsRouteToTheirBackendInOrder(t *testing.T) {
	s, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	require.NoError(t, err)
	tokens := &TokenService{Store: s}
	backend := &terminalBackend{fakeBackend: &fakeBackend{}, running: map[string]bool{}}
	registry, err := NewRegistry(backend)
	require.NoError(t, err)
	host, err := New(Config{Store: s, Tokens: tokens, Backends: registry, Logger: logr.Discard()})
	require.NoError(t, err)
	server := httptest.NewServer(host)
	t.Cleanup(server.Close)
	env := &testEnv{t: t, host: host, tokens: tokens, store: s, backend: backend.fakeBackend, server: server}
	client := env.connect(env.mint("alice").Token)
	ctx := ctx5(t)
	_, err = client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)

	claim := ahptypes.TerminalClaim{Value: &ahptypes.TerminalClientClaim{Kind: ahptypes.TerminalClaimKindClient, ClientId: "client-a"}}
	require.NoError(t, client.Request(ctx, "createTerminal", ahptypes.CreateTerminalParams{Channel: "ahp-terminal:/one", Claim: claim}, nil))
	err = client.Request(ctx, "createTerminal", ahptypes.CreateTerminalParams{Channel: "ahp-terminal:/one", Claim: claim}, nil)
	require.Equal(t, ahptypes.ErrorCodeAlreadyExists, rpcCode(t, err))
	err = client.Request(ctx, "createSession", ahptypes.CreateSessionParams{Channel: "ahp-terminal:/one"}, nil)
	require.Equal(t, ahptypes.ErrorCodeInvalidParams, rpcCode(t, err))
	stored, _, err := host.readChannel(ctx, UserKey("https://issuer.example", "alice"), "ahp-terminal:/one")
	require.NoError(t, err)
	require.NotNil(t, stored.State.Terminal, "the terminal's channel state is intact")

	result, _, err := client.Subscribe(ctx, rootChannel)
	require.NoError(t, err)
	require.Len(t, result.Snapshot.State.Root.Terminals, 1)

	for i := range 50 {
		_, err := client.Dispatch(ctx, "ahp-terminal:/one", ahptypes.StateAction{Value: &ahptypes.TerminalInputAction{
			Type: ahptypes.ActionTypeTerminalInput, Data: fmt.Sprintf("%d;", i)}})
		require.NoError(t, err)
	}
	require.Eventually(t, func() bool {
		backend.mu.Lock()
		defer backend.mu.Unlock()
		return len(backend.inputs) == 50
	}, 5*time.Second, 10*time.Millisecond)
	backend.mu.Lock()
	var want strings.Builder
	for i := range 50 {
		fmt.Fprintf(&want, "%d;", i)
	}
	require.Equal(t, want.String(), strings.Join(backend.inputs, ""), "input reaches the backend in order")
	backend.mu.Unlock()

	secret := "not-a-real-password"
	_, err = client.Dispatch(ctx, "ahp-terminal:/one", ahptypes.StateAction{Value: &ahptypes.TerminalInputAction{
		Type: ahptypes.ActionTypeTerminalInput, Data: secret + "\n"}})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		backend.mu.Lock()
		defer backend.mu.Unlock()
		return len(backend.inputs) == 51 && backend.inputs[50] == secret+"\n"
	}, 5*time.Second, 10*time.Millisecond)
	journal, _, err := s.ReadAHPJournal(ctx, UserKey("https://issuer.example", "alice"), 0)
	require.NoError(t, err)
	inputs := 0
	for _, entry := range journal {
		require.NotContains(t, string(entry.Envelope), secret, "the journal keeps no terminal input")
		require.NotContains(t, string(entry.Envelope), "49;", "the journal keeps no terminal input")
		if strings.Contains(string(entry.Envelope), `"terminal/input"`) {
			inputs++
		}
	}
	require.Equal(t, 51, inputs, "each input keeps its place in the journal")

	other := env.connect(env.mint("bob").Token)
	_, err = other.Initialize(ctx, "client-b", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	_, _, err = other.Subscribe(ctx, "ahp-terminal:/one")
	require.Error(t, err)

	require.NoError(t, client.Request(ctx, "disposeTerminal", ahptypes.DisposeTerminalParams{Channel: "ahp-terminal:/one"}, nil))
	require.Equal(t, []string{"ahp-terminal:/one"}, backend.disposed)
	_, _, err = client.Subscribe(ctx, "ahp-terminal:/one")
	require.Error(t, err)
}

func TestTrimTerminalContentKeepsTheEnd(t *testing.T) {
	state := &ahptypes.TerminalState{Content: []ahptypes.TerminalContentPart{
		{Value: &ahptypes.TerminalUnclassifiedPart{Type: "unclassified", Value: strings.Repeat("a", maxStoredTerminalContent)}},
		{Value: &ahptypes.TerminalUnclassifiedPart{Type: "unclassified", Value: strings.Repeat("é", 100)}},
	}}
	trimTerminalContent(state)
	total := 0
	for _, part := range state.Content {
		total += terminalPartSize(part)
	}
	require.LessOrEqual(t, total, maxStoredTerminalContent)
	last := state.Content[len(state.Content)-1].Value.(*ahptypes.TerminalUnclassifiedPart)
	require.Equal(t, strings.Repeat("é", 100), last.Value, "the newest output is kept")
}
