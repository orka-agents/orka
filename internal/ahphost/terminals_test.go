/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package ahphost

import (
	"context"
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

// terminalBackend is a fake backend that runs terminals and records input.
type terminalBackend struct {
	*fakeBackend
	mu       sync.Mutex
	inputs   []string
	running  map[string]bool
	disposed []string
	reaped   []string
	// reapFails fails that many reaps first.
	reapFails int
	// exitAtOnce makes a terminal's shell exit with this code before
	// CreateTerminal returns.
	exitAtOnce *int64
}

func (b *terminalBackend) CreateTerminal(ctx context.Context, _ Principal, params ahptypes.CreateTerminalParams, emit Emitter) (ahptypes.TerminalState, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.exitAtOnce != nil {
		// Like a real backend, it does not wait to learn whether the exit landed.
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
		time.Sleep(time.Millisecond) // a slow handler must not reorder input
		b.mu.Lock()
		b.inputs = append(b.inputs, input.Data)
		b.mu.Unlock()
	}
}

// A shell that exits before the host has stored its terminal still ends it:
// the exit waits for the channel and applies once it is stored.
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

// A terminal whose early output or exit cannot be stored is taken back: its
// shell is disposed of and no channel is left showing it running.
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

	// The shell's exit is journaled after the channel is stored; that fails.
	failing.failActions.Store(true)
	claim := ahptypes.TerminalClaim{Value: &ahptypes.TerminalClientClaim{Kind: ahptypes.TerminalClaimKindClient, ClientId: "client-a"}}
	require.Error(t, client.Request(ctx, "createTerminal", ahptypes.CreateTerminalParams{Channel: "ahp-terminal:/one", Claim: claim}, nil))
	_, err = s.GetAHPChannelState(ctx, UserKey("https://issuer.example", "alice"), "ahp-terminal:/one")
	require.ErrorIs(t, err, store.ErrNotFound, "no channel is left behind")
	backend.mu.Lock()
	defer backend.mu.Unlock()
	require.Equal(t, []string{"ahp-terminal:/one"}, backend.disposed, "the shell is disposed of")
}

// Creating or disposing of a terminal succeeds once done, even when the
// terminal list cannot be updated after it: the client could not repeat it.
// The list is built whole again on its next change.
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

	// Only the terminal list's update journals an action; it fails.
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

// A terminal an earlier process left behind has what it still runs stopped
// before its channel is swept; a running terminal is left alone.
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

// A leftover terminal whose shell could not be stopped keeps its channel, and
// the next sweep tries again.
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
	// A session cannot take over the terminal's channel: its scheme is the
	// terminals' own.
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

	// What a person types, such as a password at a prompt that does not echo
	// it, reaches the shell but is never kept in the durable journal.
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

	// Another user cannot use the terminal.
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
