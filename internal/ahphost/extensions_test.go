/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package ahphost

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

// extendedBackend adds root extensions, an extension command, a resource and
// a changeset channel to the fake backend.
type extendedBackend struct {
	*fakeBackend
	read []string
}

func changesetURI(session string) string { return session + "/changes" }

func (b *extendedBackend) CreateSession(ctx context.Context, principal Principal, req CreateSessionRequest, emit Emitter) (*CreatedSession, error) {
	created, err := b.fakeBackend.CreateSession(ctx, principal, req, emit)
	if err != nil {
		return nil, err
	}
	created.Changesets = map[string]ahptypes.ChangesetState{changesetURI(req.Session): {Status: ahptypes.ChangesetStatusReady}}
	created.State.Changesets = []ahptypes.Changeset{{Label: "Changes", ChangeKind: "session", UriTemplate: changesetURI(req.Session)}}
	return created, nil
}

// HandleClientAction marks the chat busy when the title changes, so the
// session's summary changes too.
func (b *extendedBackend) HandleClientAction(ctx context.Context, _ Principal, channel string, action ahptypes.StateAction, emit Emitter) {
	if _, ok := action.Value.(*ahptypes.SessionTitleChangedAction); !ok {
		return
	}
	busy := ahptypes.SessionStatusInProgress
	_ = emit.Emit(ctx, channel, ahptypes.StateAction{Value: &ahptypes.SessionChatUpdatedAction{
		Type: ahptypes.ActionTypeSessionChatUpdated, Chat: chatURI(channel), Changes: ahptypes.PartialChatSummary{Status: &busy},
	}})
}

func (b *extendedBackend) RootExtensions(context.Context, Principal) (map[string]json.RawMessage, map[string]json.RawMessage, error) {
	return map[string]json.RawMessage{"example.feature": json.RawMessage(`{"available":true}`)},
		map[string]json.RawMessage{"example": json.RawMessage(`{"items":[]}`)}, nil
}

func (b *extendedBackend) HandleExtension(_ context.Context, _ Principal, method string, params json.RawMessage, emit Emitter) (any, error) {
	if method != "extensions/echo" {
		return nil, ErrUnknownExtension
	}
	if err := emit.Emit(context.Background(), rootChannel, ahptypes.StateAction{Value: &ahptypes.RootConfigChangedAction{
		Type: ahptypes.ActionTypeRootConfigChanged, Config: map[string]json.RawMessage{"example": json.RawMessage(`{"items":[1]}`)},
	}}); err != nil {
		return nil, err
	}
	return map[string]json.RawMessage{"echo": params}, nil
}

func (b *extendedBackend) ReadResource(_ context.Context, _ Principal, session, channel, uri string) (ahptypes.ResourceReadResult, error) {
	b.read = append(b.read, session+" "+channel+" "+uri)
	if uri != "file:///work/a.go" {
		return ahptypes.ResourceReadResult{}, ErrResourceNotFound
	}
	return ahptypes.ResourceReadResult{Data: "package a\n", Encoding: ahptypes.ContentEncodingUtf8}, nil
}

func newExtendedEnv(t *testing.T) (*testEnv, *extendedBackend) {
	t.Helper()
	s, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	require.NoError(t, err)
	tokens := &TokenService{Store: s}
	backend := &extendedBackend{fakeBackend: &fakeBackend{}}
	registry, err := NewRegistry(backend)
	require.NoError(t, err)
	host, err := New(Config{Store: s, Tokens: tokens, Backends: registry, Logger: logr.Discard()})
	require.NoError(t, err)
	server := httptest.NewServer(host)
	t.Cleanup(server.Close)
	return &testEnv{t: t, host: host, tokens: tokens, store: s, backend: backend.fakeBackend, server: server}, backend
}

func TestRootCarriesBackendExtensions(t *testing.T) {
	env, _ := newExtendedEnv(t)
	client := env.connect(env.mint("alice").Token)
	ctx := ctx5(t)
	result, err := client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), []string{rootChannel})
	require.NoError(t, err)
	root := result.Snapshots[0].State.Root
	require.JSONEq(t, `{"available":true}`, string(root.Meta["example.feature"]))
	require.NotNil(t, root.Config)
	require.JSONEq(t, `{"items":[]}`, string(root.Config.Values["example"]))
}

func TestExtensionCommandsReachTheirBackend(t *testing.T) {
	env, _ := newExtendedEnv(t)
	client := env.connect(env.mint("alice").Token)
	ctx := ctx5(t)
	_, err := client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	_, root, err := client.Subscribe(ctx, rootChannel)
	require.NoError(t, err)

	var result map[string]json.RawMessage
	require.NoError(t, client.Request(ctx, "extensions/echo", map[string]string{"x": "y"}, &result))
	require.JSONEq(t, `{"x":"y"}`, string(result["echo"]))
	changed := nextAction(t, root)
	require.Equal(t, "root/configChanged", typeOf(t, changed), "a backend may change the root configuration")

	err = client.Request(ctx, "extensions/unknown", map[string]string{}, nil)
	require.Equal(t, ahptypes.ErrorCodeMethodNotFound, rpcCode(t, err))

	completions, err := client.Completions(ctx, ahptypes.CompletionsParams{Channel: rootChannel, Kind: ahptypes.CompletionItemKindUserMessage, Text: "/"})
	require.NoError(t, err)
	require.Empty(t, completions.Items)
}

func TestResourceReadServesOnlyASessionsChannels(t *testing.T) {
	env, backend := newExtendedEnv(t)
	client := env.connect(env.mint("alice").Token)
	ctx := ctx5(t)
	_, err := client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	createSession(t, client, "ahp-session:/one")

	// The client helper always targets the root channel; clients reading a
	// changeset name its channel.
	read := func(client *ahp.Client, params ahptypes.ResourceReadParams) (*ahptypes.ResourceReadResult, error) {
		var out ahptypes.ResourceReadResult
		err := client.Request(ctx, "resourceRead", params, &out)
		return &out, err
	}
	got, err := read(client, ahptypes.ResourceReadParams{Channel: changesetURI("ahp-session:/one"), Uri: "file:///work/a.go"})
	require.NoError(t, err)
	require.Equal(t, "package a\n", got.Data)
	require.Equal(t, []string{"ahp-session:/one ahp-session:/one/changes file:///work/a.go"}, backend.read)

	// The protocol's root-channel form reaches the backend without a session.
	root, err := client.ResourceRead(ctx, ahptypes.ResourceReadParams{Uri: "file:///work/a.go"})
	require.NoError(t, err)
	require.Equal(t, "package a\n", root.Data)
	require.Equal(t, " ahp-root:// file:///work/a.go", backend.read[1])

	for _, params := range []ahptypes.ResourceReadParams{
		{Channel: changesetURI("ahp-session:/one"), Uri: "file:///etc/passwd"},
		{Channel: rootChannel, Uri: "file:///etc/passwd"},
		{Channel: "ahp-session:/missing", Uri: "file:///work/a.go"},
	} {
		_, err := read(client, params)
		require.Equal(t, ahptypes.ErrorCodeNotFound, rpcCode(t, err), "%+v", params)
	}

	// Another user cannot read the session's resources.
	other := env.connect(env.mint("bob").Token)
	_, err = other.Initialize(ctx, "client-b", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	_, err = read(other, ahptypes.ResourceReadParams{Channel: changesetURI("ahp-session:/one"), Uri: "file:///work/a.go"})
	require.Equal(t, ahptypes.ErrorCodeNotFound, rpcCode(t, err))
}

func TestSessionSummaryFollowsItsChats(t *testing.T) {
	env, _ := newExtendedEnv(t)
	minted := env.mint("alice")
	client := env.connect(minted.Token)
	ctx := ctx5(t)
	_, err := client.Initialize(ctx, "client-a", ahptypes.SupportedProtocolVersions(), nil)
	require.NoError(t, err)
	createSession(t, client, "ahp-session:/one")
	_, root, err := client.Subscribe(ctx, rootChannel)
	require.NoError(t, err)

	_, err = client.Dispatch(ctx, "ahp-session:/one", ahptypes.StateAction{Value: &ahptypes.SessionTitleChangedAction{
		Type: ahptypes.ActionTypeSessionTitleChanged, Title: "renamed"}})
	require.NoError(t, err)

	var title, status bool
	timeout := time.After(5 * time.Second)
	for !title || !status {
		select {
		case event := <-root.Events():
			changed, ok := event.(ahp.SubscriptionEventSessionSummaryChanged)
			if !ok {
				continue
			}
			require.Equal(t, "ahp-session:/one", changed.Params.Session)
			if changed.Params.Changes.Title != nil {
				require.Equal(t, "renamed", *changed.Params.Changes.Title)
				title = true
			}
			if changed.Params.Changes.Status != nil {
				require.Equal(t, ahptypes.SessionStatusInProgress, *changed.Params.Changes.Status)
				status = true
			}
		case <-timeout:
			t.Fatalf("summary changes: title=%v status=%v", title, status)
		}
	}
	// A moved working directory reaches the summary too.
	_, err = client.Dispatch(ctx, "ahp-session:/one", ahptypes.StateAction{Value: &ahptypes.SessionWorkingDirectorySetAction{
		Type: ahptypes.ActionTypeSessionWorkingDirectorySet, Directory: "file:///work/next"}})
	require.NoError(t, err)
	for moved := false; !moved; {
		select {
		case event := <-root.Events():
			if changed, ok := event.(ahp.SubscriptionEventSessionSummaryChanged); ok && changed.Params.Changes.WorkingDirectories != nil {
				require.Contains(t, changed.Params.Changes.WorkingDirectories, ahptypes.URI("file:///work/next"))
				moved = true
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the summary never got the working directory")
		}
	}
	var listed ahptypes.ListSessionsResult
	require.NoError(t, client.Request(ctx, "listSessions", ahptypes.ListSessionsParams{}, &listed))
	require.Len(t, listed.Items, 1)
	require.Equal(t, "renamed", listed.Items[0].Title)
	require.Equal(t, ahptypes.SessionStatusInProgress, listed.Items[0].Status)

	// Disposing removes the changeset channel with the session.
	require.NoError(t, client.Request(ctx, "disposeSession", ahptypes.DisposeSessionParams{Channel: "ahp-session:/one"}, nil))
	_, err = env.store.GetAHPChannelState(ctx, ahpUserKey(t, env, minted.Token), changesetURI("ahp-session:/one"))
	require.ErrorIs(t, err, store.ErrNotFound)
}

func ahpUserKey(t *testing.T, env *testEnv, token string) string {
	t.Helper()
	return mustPrincipal(t, env, token).UserKey
}
