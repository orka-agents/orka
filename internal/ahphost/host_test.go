/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package ahphost

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	"github.com/microsoft/agent-host-protocol/clients/go/ahpws"
	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

// fakeBackend is a test-only session backend.
type fakeBackend struct {
	mu       sync.Mutex
	disposed []string
	released []string
	reject   string
	seen     []Principal
	// onAgents runs once, while the root snapshot is taken under the hub lock.
	onAgents func()
}

func (f *fakeBackend) Name() string { return "fake" }

func (f *fakeBackend) Agents(context.Context, Principal) ([]ahptypes.AgentInfo, error) {
	f.mu.Lock()
	onAgents := f.onAgents
	f.onAgents = nil
	f.mu.Unlock()
	if onAgents != nil {
		onAgents()
	}
	return []ahptypes.AgentInfo{{Provider: "fake", DisplayName: "Fake", Description: "test agent", Models: []ahptypes.SessionModelInfo{}}}, nil
}

func chatURI(session string) string { return session + "/chat" }

func (f *fakeBackend) CreateSession(_ context.Context, principal Principal, req CreateSessionRequest, _ Emitter) (*CreatedSession, error) {
	f.mu.Lock()
	f.seen = append(f.seen, principal)
	f.mu.Unlock()
	chat := chatURI(req.Session)
	now := time.Now().UTC().Format(time.RFC3339)
	return &CreatedSession{
		State: ahptypes.SessionState{
			Title: "new session", Status: ahptypes.SessionStatusIdle, ActiveClients: []ahptypes.SessionActiveClient{},
			Chats:       []ahptypes.ChatSummary{{Resource: chat, Status: ahptypes.SessionStatusIdle, ModifiedAt: now}},
			DefaultChat: &chat,
		},
		Chats: map[string]ahptypes.ChatState{chat: {Resource: chat, Status: ahptypes.SessionStatusIdle, ModifiedAt: now, Turns: []ahptypes.Turn{}}},
	}, nil
}

func (f *fakeBackend) DisposeSession(_ context.Context, _ Principal, session string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disposed = append(f.disposed, session)
	return nil
}

func (f *fakeBackend) AuthorizeClientAction(context.Context, Principal, string, ahptypes.StateAction) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reject != "" {
		return &Rejection{Reason: f.reject}
	}
	return nil
}

func (f *fakeBackend) ReleaseClientAction(_ context.Context, _ Principal, channel string, _ ahptypes.StateAction) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, channel)
}

// HandleClientAction answers a title change with one chat action every
// supported version knows and one introduced after 0.9.0.
func (f *fakeBackend) HandleClientAction(ctx context.Context, _ Principal, channel string, action ahptypes.StateAction, emit Emitter) {
	if _, ok := action.Value.(*ahptypes.SessionTitleChangedAction); !ok {
		return
	}
	activity := "thinking"
	_ = emit.Emit(ctx, chatURI(channel),
		ahptypes.StateAction{Value: &ahptypes.ChatCanvasesChangedAction{Type: ahptypes.ActionTypeChatCanvasesChanged, Canvases: []ahptypes.CanvasReference{}}},
		ahptypes.StateAction{Value: &ahptypes.ChatActivityChangedAction{Type: ahptypes.ActionTypeChatActivityChanged, Activity: &activity}},
	)
}

type testEnv struct {
	t       *testing.T
	host    *Host
	tokens  *TokenService
	store   *sqlite.Store
	backend *fakeBackend
	server  *httptest.Server
}

func newTestEnv(t *testing.T, limits store.AHPJournalLimits) *testEnv {
	t.Helper()
	s, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	require.NoError(t, err)
	tokens := &TokenService{Store: s}
	backend := &fakeBackend{}
	registry, err := NewRegistry(backend)
	require.NoError(t, err)
	host, err := New(Config{Store: s, Tokens: tokens, Backends: registry, Limits: limits, Logger: logr.Discard()})
	require.NoError(t, err)
	tokens.OnRevoke = host.CloseToken
	server := httptest.NewServer(host)
	t.Cleanup(server.Close)
	return &testEnv{t: t, host: host, tokens: tokens, store: s, backend: backend, server: server}
}

// failingSessionStore refuses session catalog writes while fail is set, and
// journal appends that carry actions, such as a new session's ready action,
// while failActions is set.
type failingSessionStore struct {
	store.AHPStore
	fail        atomic.Bool
	failActions atomic.Bool
}

func (s *failingSessionStore) PutAHPSession(ctx context.Context, session store.AHPSession) error {
	if s.fail.Load() {
		return errors.New("the store refused the write")
	}
	return s.AHPStore.PutAHPSession(ctx, session)
}

func (s *failingSessionStore) AppendAHPJournal(ctx context.Context, userKey string, previousSeq int64,
	entries []store.AHPJournalEntry, states []store.AHPChannelState, limits store.AHPJournalLimits) error {
	if len(entries) > 0 && s.failActions.Load() {
		return errors.New("the store refused the write")
	}
	return s.AHPStore.AppendAHPJournal(ctx, userKey, previousSeq, entries, states, limits)
}

// A session that cannot be catalogued, or then marked ready, is taken back:
// catalog record, channels and backend session, so the URI can be created
// again once the store works.
func TestASessionThatCannotBeCataloguedIsTakenBack(t *testing.T) {
	for name, flag := range map[string]func(*failingSessionStore) *atomic.Bool{
		"catalog write": func(s *failingSessionStore) *atomic.Bool { return &s.fail },
		"ready action":  func(s *failingSessionStore) *atomic.Bool { return &s.failActions },
	} {
		t.Run(name, func(t *testing.T) {
			s, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
			require.NoError(t, err)
			failing := &failingSessionStore{AHPStore: s}
			tokens := &TokenService{Store: s}
			backend := &fakeBackend{}
			registry, err := NewRegistry(backend)
			require.NoError(t, err)
			host, err := New(Config{Store: failing, Tokens: tokens, Backends: registry, Logger: logr.Discard()})
			require.NoError(t, err)
			server := httptest.NewServer(host)
			t.Cleanup(server.Close)
			env := &testEnv{t: t, host: host, tokens: tokens, store: s, backend: backend, server: server}
			client := env.connect(env.mint("alice").Token)
			_, err = client.Initialize(ctx5(t), "client-a", ahptypes.SupportedProtocolVersions(), nil)
			require.NoError(t, err)

			flag(failing).Store(true)
			provider := "fake"
			err = client.Request(ctx5(t), "createSession", ahptypes.CreateSessionParams{Channel: "ahp-session:/one", Provider: &provider}, nil)
			require.Error(t, err)
			backend.mu.Lock()
			require.Equal(t, []string{"ahp-session:/one"}, backend.disposed, "the backend's session is disposed of")
			backend.mu.Unlock()
			sessions, err := s.ListAHPSessions(ctx5(t), UserKey("https://issuer.example", "alice"))
			require.NoError(t, err)
			require.Empty(t, sessions, "no catalog record is left behind")

			flag(failing).Store(false)
			createSession(t, client, "ahp-session:/one")
		})
	}
}

// failingSessionListStore refuses to list sessions while armed.
type failingSessionListStore struct {
	store.AHPStore
	armed atomic.Bool
}

func (s *failingSessionListStore) ListAHPSessions(ctx context.Context, userKey string) ([]store.AHPSession, error) {
	if s.armed.Load() {
		return nil, errors.New("the store refused the read")
	}
	return s.AHPStore.ListAHPSessions(ctx, userKey)
}

// A session count that cannot be updated does not fail the creation or
// disposal it follows: both are already done, and a retry would be refused.
func TestASessionCountThatCannotBeUpdatedFailsNothing(t *testing.T) {
	s, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	require.NoError(t, err)
	failing := &failingSessionListStore{AHPStore: s}
	tokens := &TokenService{Store: s}
	backend := &fakeBackend{}
	registry, err := NewRegistry(backend)
	require.NoError(t, err)
	host, err := New(Config{Store: failing, Tokens: tokens, Backends: registry, Logger: logr.Discard()})
	require.NoError(t, err)
	server := httptest.NewServer(host)
	t.Cleanup(server.Close)
	env := &testEnv{t: t, host: host, tokens: tokens, store: s, backend: backend, server: server}
	client := env.connect(env.mint("alice").Token)
	ctx := ctx5(t)
	_, err = client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	userKey := UserKey("https://issuer.example", "alice")

	failing.armed.Store(true)
	createSession(t, client, "ahp-session:/one")
	_, err = s.GetAHPSession(ctx, userKey, "ahp-session:/one")
	require.NoError(t, err, "the session is kept")
	require.NoError(t, client.Request(ctx, "disposeSession", ahptypes.DisposeSessionParams{Channel: "ahp-session:/one"}, nil))
	_, err = s.GetAHPSession(ctx, userKey, "ahp-session:/one")
	require.ErrorIs(t, err, store.ErrNotFound, "the session is gone")
}

// An authorized action the host cannot journal is rejected, and the backend
// is told, so it can let go of what it held for the action.
func TestAnActionThatCannotBeAppliedIsReleased(t *testing.T) {
	s, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	require.NoError(t, err)
	failing := &failingSessionStore{AHPStore: s}
	tokens := &TokenService{Store: s}
	backend := &fakeBackend{}
	registry, err := NewRegistry(backend)
	require.NoError(t, err)
	host, err := New(Config{Store: failing, Tokens: tokens, Backends: registry, Logger: logr.Discard()})
	require.NoError(t, err)
	server := httptest.NewServer(host)
	t.Cleanup(server.Close)
	env := &testEnv{t: t, host: host, tokens: tokens, store: s, backend: backend, server: server}
	client := env.connect(env.mint("alice").Token)
	ctx := ctx5(t)
	_, err = client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	createSession(t, client, "ahp-session:/one")
	_, session, err := client.Subscribe(ctx, "ahp-session:/one")
	require.NoError(t, err)

	failing.failActions.Store(true)
	_, err = client.Dispatch(ctx, "ahp-session:/one", ahptypes.StateAction{Value: &ahptypes.SessionTitleChangedAction{
		Type: ahptypes.ActionTypeSessionTitleChanged, Title: "renamed"}})
	require.NoError(t, err)
	rejected := nextAction(t, session)
	require.NotNil(t, rejected.RejectionReason)
	require.Contains(t, *rejected.RejectionReason, "could not be applied")
	backend.mu.Lock()
	defer backend.mu.Unlock()
	require.Equal(t, []string{"ahp-session:/one"}, backend.released)
}

// failingChannelReadStore refuses to read a channel's state while armed.
type failingChannelReadStore struct {
	store.AHPStore
	channel string
	armed   atomic.Bool
}

func (s *failingChannelReadStore) GetAHPChannelState(ctx context.Context, userKey, channel string) (store.AHPChannelState, error) {
	if s.armed.Load() && channel == s.channel {
		return store.AHPChannelState{}, errors.New("the store refused the read")
	}
	return s.AHPStore.GetAHPChannelState(ctx, userKey, channel)
}

// A session whose state cannot be read is not disposed of: removing its
// catalog record would leave its chat and changeset channels behind for good.
func TestASessionWhoseStateCannotBeReadIsKeptForAnotherDispose(t *testing.T) {
	s, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	require.NoError(t, err)
	failing := &failingChannelReadStore{AHPStore: s, channel: "ahp-session:/one"}
	tokens := &TokenService{Store: s}
	backend := &fakeBackend{}
	registry, err := NewRegistry(backend)
	require.NoError(t, err)
	host, err := New(Config{Store: failing, Tokens: tokens, Backends: registry, Logger: logr.Discard()})
	require.NoError(t, err)
	server := httptest.NewServer(host)
	t.Cleanup(server.Close)
	env := &testEnv{t: t, host: host, tokens: tokens, store: s, backend: backend, server: server}
	client := env.connect(env.mint("alice").Token)
	ctx := ctx5(t)
	_, err = client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	createSession(t, client, "ahp-session:/one")
	userKey := UserKey("https://issuer.example", "alice")

	failing.armed.Store(true)
	require.Error(t, client.Request(ctx, "disposeSession", ahptypes.DisposeSessionParams{Channel: "ahp-session:/one"}, nil))
	_, err = s.GetAHPSession(ctx, userKey, "ahp-session:/one")
	require.NoError(t, err, "the session is kept")
	backend.mu.Lock()
	require.Empty(t, backend.disposed, "nothing was disposed of")
	backend.mu.Unlock()

	failing.armed.Store(false)
	require.NoError(t, client.Request(ctx, "disposeSession", ahptypes.DisposeSessionParams{Channel: "ahp-session:/one"}, nil))
	_, err = s.GetAHPChannelState(ctx, userKey, chatURI("ahp-session:/one"))
	require.ErrorIs(t, err, store.ErrNotFound, "the session's chat went with it")
}

// failingChannelDeleteStore refuses to delete channel states while armed.
type failingChannelDeleteStore struct {
	store.AHPStore
	armed atomic.Bool
}

func (s *failingChannelDeleteStore) DeleteAHPChannelStates(ctx context.Context, userKey string, channels []string) error {
	if s.armed.Load() {
		return errors.New("the store refused the delete")
	}
	return s.AHPStore.DeleteAHPChannelStates(ctx, userKey, channels)
}

// A disposal that fails partway leaves the session findable, so disposing of
// it again removes everything it had.
func TestADisposalThatFailsPartwayCanBeFinished(t *testing.T) {
	s, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	require.NoError(t, err)
	failing := &failingChannelDeleteStore{AHPStore: s}
	tokens := &TokenService{Store: s}
	backend := &fakeBackend{}
	registry, err := NewRegistry(backend)
	require.NoError(t, err)
	host, err := New(Config{Store: failing, Tokens: tokens, Backends: registry, Logger: logr.Discard()})
	require.NoError(t, err)
	server := httptest.NewServer(host)
	t.Cleanup(server.Close)
	env := &testEnv{t: t, host: host, tokens: tokens, store: s, backend: backend, server: server}
	client := env.connect(env.mint("alice").Token)
	ctx := ctx5(t)
	_, err = client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	createSession(t, client, "ahp-session:/one")
	userKey := UserKey("https://issuer.example", "alice")

	failing.armed.Store(true)
	require.Error(t, client.Request(ctx, "disposeSession", ahptypes.DisposeSessionParams{Channel: "ahp-session:/one"}, nil))
	_, err = s.GetAHPSession(ctx, userKey, "ahp-session:/one")
	require.NoError(t, err, "the failed disposal kept the session findable")

	failing.armed.Store(false)
	require.NoError(t, client.Request(ctx, "disposeSession", ahptypes.DisposeSessionParams{Channel: "ahp-session:/one"}, nil))
	_, err = s.GetAHPSession(ctx, userKey, "ahp-session:/one")
	require.ErrorIs(t, err, store.ErrNotFound)
	for _, channel := range []string{"ahp-session:/one", chatURI("ahp-session:/one")} {
		_, err = s.GetAHPChannelState(ctx, userKey, channel)
		require.ErrorIs(t, err, store.ErrNotFound, channel)
	}
}

// failingCleanupStore also refuses to delete channel states while armed.
type failingCleanupStore struct {
	*failingSessionStore
	failDelete atomic.Bool
}

func (s *failingCleanupStore) DeleteAHPChannelStates(ctx context.Context, userKey string, channels []string) error {
	if s.failDelete.Load() {
		return errors.New("the store refused the delete")
	}
	return s.failingSessionStore.DeleteAHPChannelStates(ctx, userKey, channels)
}

// A creation that fails, and whose cleanup the store also refuses, does not
// keep the URI from being created again: what it left before it was cataloged
// is cleared by the next creation, and what it left after stays listed, so
// disposing of it clears it.
func TestAFailedCreationThatCannotCleanUpLeavesTheURIUsable(t *testing.T) {
	s, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	require.NoError(t, err)
	failing := &failingCleanupStore{failingSessionStore: &failingSessionStore{AHPStore: s}}
	tokens := &TokenService{Store: s}
	backend := &fakeBackend{}
	registry, err := NewRegistry(backend)
	require.NoError(t, err)
	host, err := New(Config{Store: failing, Tokens: tokens, Backends: registry, Logger: logr.Discard()})
	require.NoError(t, err)
	server := httptest.NewServer(host)
	t.Cleanup(server.Close)
	env := &testEnv{t: t, host: host, tokens: tokens, store: s, backend: backend, server: server}
	client := env.connect(env.mint("alice").Token)
	ctx := ctx5(t)
	_, err = client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	provider := "fake"
	create := func(uri string) error {
		return client.Request(ctx, "createSession", ahptypes.CreateSessionParams{Channel: uri, Provider: &provider}, nil)
	}
	userKey := UserKey("https://issuer.example", "alice")

	// The catalog record cannot be written: the channels stay, uncataloged.
	failing.fail.Store(true)
	failing.failDelete.Store(true)
	require.Error(t, create("ahp-session:/one"))
	_, err = s.GetAHPChannelState(ctx, userKey, "ahp-session:/one")
	require.NoError(t, err, "the cleanup could not delete the channels")
	failing.fail.Store(false)
	failing.failDelete.Store(false)
	require.NoError(t, create("ahp-session:/one"))

	// The session cannot be made ready: the cataloged session stays listed.
	failing.failActions.Store(true)
	failing.failDelete.Store(true)
	require.Error(t, create("ahp-session:/two"))
	failing.failActions.Store(false)
	failing.failDelete.Store(false)
	_, err = s.GetAHPSession(ctx, userKey, "ahp-session:/two")
	require.NoError(t, err, "the leftover session is listed")
	require.NoError(t, client.Request(ctx, "disposeSession", ahptypes.DisposeSessionParams{Channel: "ahp-session:/two"}, nil))
	require.NoError(t, create("ahp-session:/two"))
}

// pausingSessionStore holds the first catalog read after it is armed, once the
// read is done, until released: a summary sync slow to write its update.
type pausingSessionStore struct {
	store.AHPStore
	armed   atomic.Bool
	reached chan struct{}
	release chan struct{}
}

func (s *pausingSessionStore) GetAHPSession(ctx context.Context, userKey, session string) (store.AHPSession, error) {
	record, err := s.AHPStore.GetAHPSession(ctx, userKey, session)
	if s.armed.CompareAndSwap(true, false) {
		close(s.reached)
		<-s.release
	}
	return record, err
}

// A session disposed of while an action on it is still writing its summary
// stays gone: the summary is not written back, so the URI can be used again.
func TestADisposalWaitsForTheSummaryBeingWritten(t *testing.T) {
	s, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	require.NoError(t, err)
	pausing := &pausingSessionStore{AHPStore: s, reached: make(chan struct{}), release: make(chan struct{})}
	tokens := &TokenService{Store: s}
	backend := &fakeBackend{}
	registry, err := NewRegistry(backend)
	require.NoError(t, err)
	host, err := New(Config{Store: pausing, Tokens: tokens, Backends: registry, Logger: logr.Discard()})
	require.NoError(t, err)
	server := httptest.NewServer(host)
	t.Cleanup(server.Close)
	env := &testEnv{t: t, host: host, tokens: tokens, store: s, backend: backend, server: server}
	client := env.connect(env.mint("alice").Token)
	ctx := ctx5(t)
	_, err = client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	createSession(t, client, "ahp-session:/one")
	userKey := UserKey("https://issuer.example", "alice")

	pausing.armed.Store(true)
	applied := make(chan error, 1)
	go func() {
		applied <- emitter{host: host, principal: Principal{UserKey: userKey}}.Emit(ctx, "ahp-session:/one",
			ahptypes.StateAction{Value: &ahptypes.SessionTitleChangedAction{Type: ahptypes.ActionTypeSessionTitleChanged, Title: "late"}})
	}()
	<-pausing.reached
	disposed := make(chan error, 1)
	go func() {
		disposed <- client.Request(ctx, "disposeSession", ahptypes.DisposeSessionParams{Channel: "ahp-session:/one"}, nil)
	}()
	select {
	case err := <-disposed:
		disposed <- err
	case <-time.After(300 * time.Millisecond):
	}
	close(pausing.release)
	require.NoError(t, <-applied)
	require.NoError(t, <-disposed)

	_, err = s.GetAHPSession(ctx, userKey, "ahp-session:/one")
	require.ErrorIs(t, err, store.ErrNotFound, "the summary was not written back")
	createSession(t, client, "ahp-session:/one")
}

// An action stays applied once it is journaled: a catalog summary that cannot
// be written then neither rejects it nor keeps its side effects from running.
func TestAnActionIsAppliedThoughItsSummaryCannotBeWritten(t *testing.T) {
	s, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	require.NoError(t, err)
	failing := &failingSessionStore{AHPStore: s}
	tokens := &TokenService{Store: s}
	backend := &fakeBackend{}
	registry, err := NewRegistry(backend)
	require.NoError(t, err)
	host, err := New(Config{Store: failing, Tokens: tokens, Backends: registry, Logger: logr.Discard()})
	require.NoError(t, err)
	server := httptest.NewServer(host)
	t.Cleanup(server.Close)
	env := &testEnv{t: t, host: host, tokens: tokens, store: s, backend: backend, server: server}
	client := env.connect(env.mint("alice").Token)
	ctx := ctx5(t)
	_, err = client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	createSession(t, client, "ahp-session:/one")
	_, session, err := client.Subscribe(ctx, "ahp-session:/one")
	require.NoError(t, err)
	_, chat, err := client.Subscribe(ctx, chatURI("ahp-session:/one"))
	require.NoError(t, err)

	failing.fail.Store(true)
	_, err = client.Dispatch(ctx, "ahp-session:/one", ahptypes.StateAction{Value: &ahptypes.SessionTitleChangedAction{
		Type: ahptypes.ActionTypeSessionTitleChanged, Title: "renamed"}})
	require.NoError(t, err)
	accepted := nextAction(t, session)
	require.Nil(t, accepted.RejectionReason, "the journaled action is not rejected")
	nextAction(t, chat) // the backend's handler ran and answered on the chat
	backend.mu.Lock()
	defer backend.mu.Unlock()
	require.Empty(t, backend.released, "nothing is released as unapplied")
}

// A session cannot be named with a scheme the host routes to another kind of
// channel: actions on it would go to a terminal or resource watch.
func TestASessionCannotTakeAReservedScheme(t *testing.T) {
	env := newTestEnv(t, store.AHPJournalLimits{})
	client := env.connect(env.mint("alice").Token)
	ctx := ctx5(t)
	_, err := client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	provider := "fake"
	for _, uri := range []string{"ahp-terminal:/one", resourceWatchScheme + "one"} {
		err := client.Request(ctx, "createSession", ahptypes.CreateSessionParams{Channel: uri, Provider: &provider}, nil)
		require.Equal(t, ahptypes.ErrorCodeInvalidParams, rpcCode(t, err), uri)
	}
}

func (e *testEnv) mint(subject string) MintedToken {
	e.t.Helper()
	minted, err := e.tokens.Mint(context.Background(), MintRequest{
		Issuer: "https://issuer.example", Subject: subject, Username: subject + "@example.com", Namespace: "team",
	})
	require.NoError(e.t, err)
	return minted
}

func (e *testEnv) wsURL(token string) string {
	return "ws" + strings.TrimPrefix(e.server.URL, "http") + "/?tkn=" + token
}

func (e *testEnv) connect(token string) *ahp.Client {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	transport, err := ahpws.Connect(ctx, e.wsURL(token))
	require.NoError(e.t, err)
	client, err := ahp.Connect(ctx, transport, ahp.DefaultConfig())
	require.NoError(e.t, err)
	e.t.Cleanup(func() { _ = client.Shutdown(context.Background()) })
	return client
}

func ctx5(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func rpcCode(t *testing.T, err error) int32 {
	t.Helper()
	var rpc *ahp.RPCError
	require.True(t, errors.As(err, &rpc), "expected an RPC error, got %v", err)
	return rpc.Code
}

func createSession(t *testing.T, client *ahp.Client, uri string) {
	t.Helper()
	provider := "fake"
	require.NoError(t, client.Request(ctx5(t), "createSession", ahptypes.CreateSessionParams{Channel: uri, Provider: &provider}, nil))
}

func nextAction(t *testing.T, sub *ahp.Subscription) ahptypes.ActionEnvelope {
	t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case event := <-sub.Events():
			if action, ok := event.(ahp.SubscriptionEventAction); ok {
				return action.Envelope
			}
		case <-timeout:
			t.Fatal("no action arrived")
		}
	}
}

func TestDoorRefusesBeforeTheUpgrade(t *testing.T) {
	env := newTestEnv(t, store.AHPJournalLimits{})
	minted := env.mint("alice")
	base := strings.Replace(env.server.URL, "http", "http", 1)

	status := func(target string, header http.Header) int {
		req, err := http.NewRequest(http.MethodGet, target, nil)
		require.NoError(t, err)
		maps.Copy(req.Header, header)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	require.Equal(t, http.StatusUnauthorized, status(base+"/", nil), "no token")
	require.Equal(t, http.StatusUnauthorized, status(base+"/?tkn=orka_ahp_wrong", nil), "unknown token")
	require.Equal(t, http.StatusUnauthorized, status(base+"/?tkn=not-an-ahp-token", nil), "malformed token")
	require.Equal(t, http.StatusForbidden, status(base+"/?tkn="+minted.Token, http.Header{"Origin": {"https://evil.example"}}), "web origin")
	require.Equal(t, http.StatusForbidden, status(base+"/?tkn="+minted.Token, http.Header{"Origin": {"HTTP://localhost:5173"}}), "local web origin")
	require.Equal(t, http.StatusMethodNotAllowed, func() int {
		resp, err := http.Post(base+"/?tkn="+minted.Token, "text/plain", nil)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}())

	// A bearer header works, and so does a native client's non-web origin.
	ctx := ctx5(t)
	for _, header := range []http.Header{
		{"Authorization": {"Bearer " + minted.Token}},
		{"Authorization": {"Bearer " + minted.Token}, "Origin": {"null"}},
		{"Authorization": {"Bearer " + minted.Token}, "Origin": {"vscode-file://vscode-app"}},
	} {
		transport, err := ahpws.Connect(ctx, "ws"+strings.TrimPrefix(env.server.URL, "http")+"/", ahpws.DialOptions{HTTPHeader: header})
		require.NoError(t, err)
		_ = transport.Close(ctx)
	}

	require.NoError(t, env.tokens.Revoke(ctx, "https://issuer.example", "alice", minted.ID))
	require.Equal(t, http.StatusUnauthorized, status(base+"/?tkn="+minted.Token, nil), "revoked token")
}

func TestInitializePingAndVersionNegotiation(t *testing.T) {
	env := newTestEnv(t, store.AHPJournalLimits{})
	minted := env.mint("alice")
	client := env.connect(minted.Token)
	ctx := ctx5(t)

	result, err := client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), []string{rootChannel})
	require.NoError(t, err)
	require.Equal(t, "1.0.0", result.ProtocolVersion)
	require.Len(t, result.Snapshots, 1)
	require.NotNil(t, result.Snapshots[0].State.Root)
	require.Equal(t, "fake", result.Snapshots[0].State.Root.Agents[0].Provider)
	require.NoError(t, client.Ping(ctx))

	_, err = client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.Equal(t, ahptypes.ErrorCodeInvalidRequest, rpcCode(t, err), "initialize runs once per connection")

	older := env.connect(minted.Token)
	result, err = older.Initialize(ctx, "client-b", []string{"0.9.0"}, nil)
	require.NoError(t, err)
	require.Equal(t, "0.9.0", result.ProtocolVersion)

	// The host answers, then closes. A client may report either first, so read
	// the host's frames in the order it sent them.
	future, err := ahpws.Connect(ctx, env.wsURL(minted.Token))
	require.NoError(t, err)
	t.Cleanup(func() { _ = future.Close(context.Background()) })
	request, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": ahptypes.InitializeParams{Channel: rootChannel, ClientId: "client-c", ProtocolVersions: []string{"2.0.0"}}})
	require.NoError(t, err)
	require.NoError(t, future.Send(ctx, ahp.NewTextMessage(string(request))))
	answer, err := future.Recv(ctx)
	require.NoError(t, err)
	raw, _, err := answer.Bytes()
	require.NoError(t, err)
	var response struct {
		Error *ahptypes.JsonRpcError `json:"error"`
	}
	require.NoError(t, json.Unmarshal(raw, &response))
	require.NotNil(t, response.Error, string(raw))
	require.Equal(t, ahptypes.ErrorCodeUnsupportedProtocolVersion, response.Error.Code)
	// The connection then closes with a close frame, which tells the client why.
	_, err = future.Recv(ctx)
	require.ErrorIs(t, err, ahp.ErrClosed, "the connection must close after an unsupported version")

	early := env.connect(minted.Token)
	_, _, err = early.Subscribe(ctx, rootChannel)
	require.Equal(t, ahptypes.ErrorCodeInvalidRequest, rpcCode(t, err), "commands need initialize first")
	require.NoError(t, early.Ping(ctx), "ping works before initialize")
}

func TestActionsAreFilteredByNegotiatedVersion(t *testing.T) {
	env := newTestEnv(t, store.AHPJournalLimits{})
	minted := env.mint("alice")
	ctx := ctx5(t)
	current := env.connect(minted.Token)
	_, err := current.Initialize(ctx, "current", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	createSession(t, current, "ahp-session:/one")

	older := env.connect(minted.Token)
	_, err = older.Initialize(ctx, "older", []string{"0.9.0"}, nil)
	require.NoError(t, err)

	_, currentChat, err := current.Subscribe(ctx, chatURI("ahp-session:/one"))
	require.NoError(t, err)
	_, olderChat, err := older.Subscribe(ctx, chatURI("ahp-session:/one"))
	require.NoError(t, err)

	_, err = current.Dispatch(ctx, "ahp-session:/one", ahptypes.StateAction{Value: &ahptypes.SessionTitleChangedAction{
		Type: ahptypes.ActionTypeSessionTitleChanged, Title: "renamed"}})
	require.NoError(t, err)

	first := nextAction(t, currentChat)
	second := nextAction(t, currentChat)
	require.Equal(t, []string{"chat/canvasesChanged", "chat/activityChanged"}, []string{typeOf(t, first), typeOf(t, second)})
	only := nextAction(t, olderChat)
	require.Equal(t, "chat/activityChanged", typeOf(t, only), "a 0.9.0 client never sees an action introduced later")
	require.Equal(t, second.ServerSeq, only.ServerSeq)
}

func typeOf(t *testing.T, envelope ahptypes.ActionEnvelope) string {
	t.Helper()
	kind, err := actionType(envelope.Action)
	require.NoError(t, err)
	return string(kind)
}

func TestClientActionsAreValidatedAndEchoed(t *testing.T) {
	env := newTestEnv(t, store.AHPJournalLimits{})
	minted := env.mint("alice")
	ctx := ctx5(t)
	client := env.connect(minted.Token)
	_, err := client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	createSession(t, client, "ahp-session:/one")
	_, session, err := client.Subscribe(ctx, "ahp-session:/one")
	require.NoError(t, err)

	handle, err := client.Dispatch(ctx, "ahp-session:/one", ahptypes.StateAction{Value: &ahptypes.SessionTitleChangedAction{
		Type: ahptypes.ActionTypeSessionTitleChanged, Title: "renamed"}})
	require.NoError(t, err)
	accepted := nextAction(t, session)
	require.Nil(t, accepted.RejectionReason)
	require.Equal(t, "client-a", accepted.Origin.ClientId)
	require.Equal(t, handle.ClientSeq, accepted.Origin.ClientSeq)

	_, err = client.Dispatch(ctx, "ahp-session:/one", ahptypes.StateAction{Value: &ahptypes.SessionReadyAction{Type: ahptypes.ActionTypeSessionReady}})
	require.NoError(t, err)
	rejected := nextAction(t, session)
	require.NotNil(t, rejected.RejectionReason)
	require.Contains(t, *rejected.RejectionReason, "may not dispatch")

	env.backend.mu.Lock()
	env.backend.reject = "not allowed by policy"
	env.backend.mu.Unlock()
	_, err = client.Dispatch(ctx, "ahp-session:/one", ahptypes.StateAction{Value: &ahptypes.SessionTitleChangedAction{
		Type: ahptypes.ActionTypeSessionTitleChanged, Title: "again"}})
	require.NoError(t, err)
	rejected = nextAction(t, session)
	require.Equal(t, "not allowed by policy", *rejected.RejectionReason)

	snapshot, err := env.host.snapshot(ctx, mustPrincipal(t, env, minted.Token), "ahp-session:/one")
	require.NoError(t, err)
	require.Equal(t, "renamed", snapshot.State.Session.Title, "only the accepted action changed the state")
}

func mustPrincipal(t *testing.T, env *testEnv, token string) Principal {
	t.Helper()
	principal, err := env.tokens.Verify(context.Background(), token)
	require.NoError(t, err)
	return principal
}

func TestReconnectRules(t *testing.T) {
	env := newTestEnv(t, store.AHPJournalLimits{MaxEntries: 3})
	alice := env.mint("alice")
	bob := env.mint("bob")
	ctx := ctx5(t)

	first := env.connect(alice.Token)
	init, err := first.Initialize(ctx, "laptop", []string{"0.9.0"}, []string{rootChannel})
	require.NoError(t, err)
	createSession(t, first, "ahp-session:/one")
	seq := init.ServerSeq

	// A known client resumes with its negotiated version and gets what it missed.
	again := env.connect(alice.Token)
	result, err := again.Reconnect(ctx, "laptop", seq, []string{"ahp-session:/one", "ahp-session:/gone"})
	require.NoError(t, err)
	replay, ok := result.Value.(*ahptypes.ReconnectReplayResult)
	require.True(t, ok, "a covered gap is replayed: %T", result.Value)
	require.NotEmpty(t, replay.Actions)
	require.Equal(t, []string{"ahp-session:/gone"}, replay.Missing)
	// The session's addition went out as a root notification, which no
	// replay carries: a client following the root gets snapshots instead.
	rooted := env.connect(alice.Token)
	result, err = rooted.Reconnect(ctx, "laptop", seq, []string{rootChannel, "ahp-session:/one"})
	require.NoError(t, err)
	missed, ok := result.Value.(*ahptypes.ReconnectSnapshotResult)
	require.True(t, ok, "a gap with a root notification in it falls back to snapshots: %T", result.Value)
	require.Len(t, missed.Snapshots, 2)

	// An unknown client, or another user's client, must initialize again.
	unknown := env.connect(alice.Token)
	_, err = unknown.Reconnect(ctx, "never-seen", 0, []string{rootChannel})
	require.Equal(t, ahptypes.ErrorCodeNotFound, rpcCode(t, err))
	stolen := env.connect(bob.Token)
	_, err = stolen.Reconnect(ctx, "laptop", seq, []string{rootChannel})
	require.Equal(t, ahptypes.ErrorCodeNotFound, rpcCode(t, err))
	thief := env.connect(bob.Token)
	_, err = thief.Initialize(ctx, "laptop", ahptypes.SupportedProtocolVersions(), nil)
	require.Equal(t, ahptypes.ErrorCodePermissionDenied, rpcCode(t, err), "a clientId stays bound to its first user")

	// A gap the journal no longer covers gets snapshots instead.
	for i := range 5 {
		_, err := first.Dispatch(ctx, "ahp-session:/one", ahptypes.StateAction{Value: &ahptypes.SessionTitleChangedAction{
			Type: ahptypes.ActionTypeSessionTitleChanged, Title: strings.Repeat("x", i+1)}})
		require.NoError(t, err)
	}
	require.Eventually(t, func() bool {
		current, _ := env.store.AHPServerSeq(ctx, UserKey("https://issuer.example", "alice"))
		return current >= seq+7
	}, 5*time.Second, 20*time.Millisecond)
	late := env.connect(alice.Token)
	result, err = late.Reconnect(ctx, "laptop", seq, []string{rootChannel, "ahp-session:/one"})
	require.NoError(t, err)
	snapshots, ok := result.Value.(*ahptypes.ReconnectSnapshotResult)
	require.True(t, ok, "a pruned gap falls back to snapshots: %T", result.Value)
	require.Len(t, snapshots.Snapshots, 2)
}

func TestRevocationClosesPassiveSubscribers(t *testing.T) {
	env := newTestEnv(t, store.AHPJournalLimits{})
	minted := env.mint("alice")
	ctx := ctx5(t)
	client := env.connect(minted.Token)
	_, err := client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), []string{rootChannel})
	require.NoError(t, err)

	require.NoError(t, env.tokens.Revoke(ctx, "https://issuer.example", "alice", minted.ID))
	select {
	case <-client.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a revoked token must close its connections even when they are idle")
	}
}

func TestExpiredTokensCloseTheirConnections(t *testing.T) {
	env := newTestEnv(t, store.AHPJournalLimits{})
	ctx := ctx5(t)
	soon := time.Now().Add(1500 * time.Millisecond)
	// Minted a minute before soon with the shortest lifetime, it expires at soon.
	env.tokens.Now = func() time.Time { return soon.Add(-time.Minute) }
	minted, err := env.tokens.Mint(ctx, MintRequest{Issuer: "https://issuer.example", Subject: "alice", TTL: time.Minute})
	require.NoError(t, err)
	env.tokens.Now = nil
	client := env.connect(minted.Token)
	select {
	case <-client.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("an expired token must close its connections")
	}
}

func TestUsersAreIsolated(t *testing.T) {
	env := newTestEnv(t, store.AHPJournalLimits{})
	alice := env.connect(env.mint("alice").Token)
	bob := env.connect(env.mint("bob").Token)
	ctx := ctx5(t)
	_, err := alice.Initialize(ctx, "alice-laptop", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	init, err := bob.Initialize(ctx, "bob-laptop", ahptypes.SupportedProtocolVersions(), []string{rootChannel})
	require.NoError(t, err)
	createSession(t, alice, "ahp-session:/private")

	var list ahptypes.ListSessionsResult
	require.NoError(t, bob.Request(ctx, "listSessions", ahptypes.ListSessionsParams{Channel: rootChannel}, &list))
	require.Empty(t, list.Items)
	require.NoError(t, alice.Request(ctx, "listSessions", ahptypes.ListSessionsParams{Channel: rootChannel}, &list))
	require.Len(t, list.Items, 1)

	_, _, err = bob.Subscribe(ctx, "ahp-session:/private")
	require.Equal(t, ahptypes.ErrorCodeNotFound, rpcCode(t, err))
	require.Zero(t, *init.Snapshots[0].State.Root.ActiveSessions)

	// Bob may create a session with the same URI; it is his own.
	createSession(t, bob, "ahp-session:/private")
	require.NoError(t, bob.Request(ctx, "disposeSession", ahptypes.DisposeSessionParams{Channel: "ahp-session:/private"}, nil))
	require.NoError(t, alice.Request(ctx, "listSessions", ahptypes.ListSessionsParams{Channel: rootChannel}, &list))
	require.Len(t, list.Items, 1, "disposing bob's session leaves alice's")
}

func TestAuthenticateIsRefusedWithoutAdvertisedResources(t *testing.T) {
	env := newTestEnv(t, store.AHPJournalLimits{})
	client := env.connect(env.mint("alice").Token)
	ctx := ctx5(t)
	_, err := client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	raw := json.RawMessage(`{"channel":"ahp-root://","resource":"https://api.github.com","token":"secret"}`)
	err = client.Request(ctx, "authenticate", raw, nil)
	require.Equal(t, ahptypes.ErrorCodeInvalidParams, rpcCode(t, err))
}

// An action committed while a subscription's snapshot is being taken must
// reach the subscriber after the snapshot, never fall between the two.
func TestSubscribeNeverMissesAConcurrentAction(t *testing.T) {
	env := newTestEnv(t, store.AHPJournalLimits{})
	minted := env.mint("alice")
	ctx := ctx5(t)
	client := env.connect(minted.Token)
	_, err := client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)

	principal := mustPrincipal(t, env, minted.Token)
	applied := make(chan error, 1)
	env.backend.mu.Lock()
	env.backend.onAgents = func() {
		go func() {
			action := ahptypes.StateAction{Value: &ahptypes.RootActiveSessionsChangedAction{
				Type: ahptypes.ActionTypeRootActiveSessionsChanged, ActiveSessions: 7}}
			applied <- env.host.apply(context.Background(), principal, rootChannel, []ahptypes.StateAction{action}, nil)
		}()
		// Give the concurrent action time to wait on the hub lock.
		time.Sleep(100 * time.Millisecond)
	}
	env.backend.mu.Unlock()

	result, sub, err := client.Subscribe(ctx, rootChannel)
	require.NoError(t, err)
	require.NoError(t, <-applied)
	envelope := nextAction(t, sub)
	require.Equal(t, result.Snapshot.FromSeq+1, envelope.ServerSeq, "the action follows the snapshot it is missing from")
}

func TestSessionURIsAreClaimedOneCreationAtATime(t *testing.T) {
	h := &Host{}
	release, ok := h.claimChannelURI("alice", "ahp-session:/one", false)
	require.True(t, ok)
	_, ok = h.claimChannelURI("alice", "ahp-session:/one", false)
	require.False(t, ok, "a second creation of the same URI waits for none: it is refused")
	other, ok := h.claimChannelURI("bob", "ahp-session:/one", false)
	require.True(t, ok, "another user's URI is their own")
	other()
	release()
	again, ok := h.claimChannelURI("alice", "ahp-session:/one", false)
	require.True(t, ok, "a finished creation frees the URI")
	again()
}
