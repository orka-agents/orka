/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/orka-agents/orka/internal/ahphost"
)

type ahpRecordingEmitter struct {
	mu   sync.Mutex
	data []string
}

func (e *ahpRecordingEmitter) Emit(_ context.Context, _ string, actions ...ahptypes.StateAction) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, action := range actions {
		if data, ok := action.Value.(*ahptypes.TerminalDataAction); ok {
			e.data = append(e.data, data.Data)
		}
	}
	return nil
}

func TestAHPTerminalOutputNeverSplitsACharacter(t *testing.T) {
	emit := &ahpRecordingEmitter{}
	out := newAHPTerminalOutput(context.Background(), emit, "ahp-terminal:/t", nil)
	text := strings.Repeat("héllo wörld ✓ ", 5000) // larger than one chunk
	raw := []byte(text)
	// Write in awkward pieces that cut characters in half.
	for i := 0; i < len(raw); i += 7 {
		_, err := out.Write(raw[i:min(i+7, len(raw))])
		require.NoError(t, err)
	}
	out.close()
	emit.mu.Lock()
	defer emit.mu.Unlock()
	var joined strings.Builder
	for _, chunk := range emit.data {
		require.True(t, utf8.ValidString(chunk), "every chunk is valid UTF-8")
		require.LessOrEqual(t, len(chunk), ahpTerminalChunkMax)
		joined.WriteString(chunk)
	}
	require.Equal(t, text, joined.String())
}

func TestAHPTerminalOutputFlushesQuietOutput(t *testing.T) {
	emit := &ahpRecordingEmitter{}
	out := newAHPTerminalOutput(context.Background(), emit, "ahp-terminal:/t", nil)
	_, _ = out.Write([]byte("$ "))
	require.Eventually(t, func() bool {
		emit.mu.Lock()
		defer emit.mu.Unlock()
		return len(emit.data) == 1 && emit.data[0] == "$ "
	}, time.Second, 5*time.Millisecond, "a prompt without a newline still reaches the client")
}

// Terminal data is journaled, so the workspace's git token never leaves in it,
// however its output is split, delayed, or cut short.
func TestAHPTerminalOutputRedactsTheWorkspaceToken(t *testing.T) {
	const token = "ghu_0123456789abcdefghijklmnopqrstuvwx"
	for name, tc := range map[string]struct {
		writes []string
		close  bool
		want   string
	}{
		"closed in the middle of the token": {
			writes: []string{"ghu_", token[4 : len(token)-1]},
			close:  true,
			want:   "ghu_[REDACTED]",
		},
		"split inside the secret, slowly": {
			writes: []string{"$ cat token\r\n" + token[:10], token[10:] + "\r\n$ "},
			want:   "$ cat token\r\nghu_[REDACTED]\r\n$ ",
		},
		"cut short at the end of a write": {
			writes: []string{token[:len(token)-1], "\r\n$ "},
			want:   "ghu_[REDACTED]\r\n$ ",
		},
		"cut short within a write": {
			writes: []string{"$ " + token[:20] + "\r\n$ "},
			want:   "$ [REDACTED]\r\n$ ",
		},
		"continued from emitted output": {
			writes: []string{"$ ghu_012", "3456789ab\r\n"},
			want:   "$ ghu_012[REDACTED]\r\n",
		},
		"a few characters that start the secret": {
			writes: []string{"$ 012"},
			want:   "$ 012",
		},
		"split after the public start, slowly": {
			writes: []string{"x ghu_", token[4:] + "\r\n"},
			want:   "x ghu_[REDACTED]\r\n",
		},
		"a public start that never becomes a token": {
			writes: []string{"$ gh"},
			want:   "$ gh",
		},
	} {
		t.Run(name, func(t *testing.T) {
			emit := &ahpRecordingEmitter{}
			out := newAHPTerminalOutput(context.Background(), emit, "ahp-terminal:/t", func() []string { return []string{token} })
			joined := func() string {
				emit.mu.Lock()
				defer emit.mu.Unlock()
				return strings.Join(emit.data, "")
			}
			for _, write := range tc.writes {
				_, _ = out.Write([]byte(write))
				// Longer than a flush, so each write is emitted on its own.
				time.Sleep(10 * ahpTerminalFlushEvery)
				require.NotContains(t, joined(), token[4:14], "part of the token's secret was emitted")
			}
			if tc.close {
				out.close()
			}
			require.Eventually(t, func() bool { return joined() == tc.want }, time.Second, 5*time.Millisecond,
				"got %q", joined())
		})
	}
}

// However output is cut into writes, the journal never carries the start of a
// token's secret, whether the token is whole or cut short.
func TestAHPTerminalOutputRedactsAtEveryCut(t *testing.T) {
	const token = "ghu_0123456789abcdefghijklmnopqrstuvwx"
	for _, text := range []string{
		"$ cat token\r\n" + token + "\r\n$ ",
		"x" + token[:len(token)-3] + "\r\n$ ",
		token[:14] + token + " " + token[4:],
	} {
		for first := 1; first < len(text); first++ {
			for second := first; second < len(text); second++ {
				emit := &ahpRecordingEmitter{}
				out := newAHPTerminalOutput(context.Background(), emit, "ahp-terminal:/t", func() []string { return []string{token} })
				for _, write := range []string{text[:first], text[first:second], text[second:]} {
					_, _ = out.Write([]byte(write))
					out.mu.Lock()
					out.flushLocked()
					out.mu.Unlock()
				}
				out.close()
				emit.mu.Lock()
				joined := strings.Join(emit.data, "")
				emit.mu.Unlock()
				require.NotContains(t, joined, token[4:4+ahpMinRedactedToken], "cut at %d and %d: %q", first, second, joined)
				require.True(t, strings.HasSuffix(joined, "$ ") == strings.HasSuffix(text, "$ "), "output after the token is kept: %q", joined)
			}
		}
	}
}

func TestAHPTerminalTitleChangesReachTheTerminalList(t *testing.T) {
	principal := ahphost.Principal{UserKey: "alice"}
	b := &ahpBackend{shells: map[string]*ahpTerminal{}}
	b.shells[ahpTerminalKey(principal.UserKey, "ahp-terminal:/t")] = &ahpTerminal{
		ctx: context.Background(), channel: "ahp-terminal:/t", title: "Terminal",
	}
	b.handleTerminalAction(principal, "ahp-terminal:/t", ahptypes.StateAction{Value: &ahptypes.TerminalTitleChangedAction{
		Type: ahptypes.ActionTypeTerminalTitleChanged, Title: "build",
	}})
	terminals, err := b.Terminals(context.Background(), principal)
	require.NoError(t, err)
	require.Len(t, terminals, 1)
	require.Equal(t, "build", terminals[0].Title)
}

func TestAHPValidBranch(t *testing.T) {
	for _, good := range []string{"main", "octocat-quiet-meadow", "orka/session-1a2b", "feature/x.y_z"} {
		require.True(t, ahpValidBranch(good), good)
	}
	for _, bad := range []string{"", "-x", "a..b", "a/../b", ".hidden", "a/.b", "x.lock", "a b", "a;b", "a$(b)", "/abs", "a//b"} {
		require.False(t, ahpValidBranch(bad), bad)
	}
}

func TestAHPTerminalsAreTheirOwnersOnly(t *testing.T) {
	env := newAHPTestEnv(t)
	alice := ahphost.Principal{UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	bob := ahphost.Principal{UserKey: ahphost.UserKey("https://issuer.example", "bob")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	term := &ahpTerminal{ctx: ctx, channel: "ahp-terminal:/a", input: make(chan []byte, ahpTerminalInputQueue), cancel: cancel}
	env.backend.mu.Lock()
	env.backend.shells[ahpTerminalKey(alice.UserKey, term.channel)] = term
	env.backend.mu.Unlock()
	input := ahptypes.StateAction{Value: &ahptypes.TerminalInputAction{Type: ahptypes.ActionTypeTerminalInput, Data: "ls\n"}}
	require.NoError(t, env.backend.AuthorizeClientAction(context.Background(), alice, term.channel, input))
	require.Error(t, env.backend.AuthorizeClientAction(context.Background(), bob, term.channel, input), "another person's terminal")
	env.backend.HandleClientAction(context.Background(), alice, term.channel, input, nil)
	require.Equal(t, "ls\n", string(<-term.input))
	listed, err := env.backend.Terminals(context.Background(), bob)
	require.NoError(t, err)
	require.Empty(t, listed)

	// Without a workspace image there are no terminals.
	_, err = env.backend.CreateTerminal(context.Background(), alice, ahptypes.CreateTerminalParams{Channel: "ahp-terminal:/b"}, nil)
	require.Error(t, err)
}

func TestAHPTerminalOutputPassesMalformedBytesOn(t *testing.T) {
	emit := &ahpRecordingEmitter{}
	out := newAHPTerminalOutput(context.Background(), emit, "ahp-terminal:/t", nil)
	garbage := make([]byte, 2*ahpTerminalChunkMax+5)
	for i := range garbage {
		garbage[i] = 0x80 // continuation bytes with no start byte
	}
	_, err := out.Write(garbage)
	require.NoError(t, err)
	out.mu.Lock()
	pending := len(out.pending)
	out.mu.Unlock()
	require.Less(t, pending, ahpTerminalChunkMax, "malformed output does not pile up")
	out.close()
	emit.mu.Lock()
	defer emit.mu.Unlock()
	for _, chunk := range emit.data {
		require.True(t, utf8.ValidString(chunk))
	}
}

func TestAHPTerminalSizeQueueEndsWithTheTerminal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	term := &ahpTerminal{ctx: ctx, sizes: make(chan remotecommand.TerminalSize)}
	done := make(chan *remotecommand.TerminalSize)
	go func() { done <- term.Next() }()
	cancel()
	select {
	case size := <-done:
		require.Nil(t, size)
	case <-time.After(time.Second):
		t.Fatal("the resize worker stayed blocked after the terminal ended")
	}
}

func TestAHPTerminalRefusesInputItCannotQueue(t *testing.T) {
	env := newAHPTestEnv(t)
	alice := ahphost.Principal{UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	term := &ahpTerminal{ctx: ctx, channel: "ahp-terminal:/a", input: make(chan []byte, ahpTerminalInputQueue), cancel: cancel}
	env.backend.mu.Lock()
	env.backend.shells[ahpTerminalKey(alice.UserKey, term.channel)] = term
	env.backend.mu.Unlock()
	input := ahptypes.StateAction{Value: &ahptypes.TerminalInputAction{Type: ahptypes.ActionTypeTerminalInput, Data: "x"}}
	for range ahpTerminalInputQueue {
		require.NoError(t, env.backend.AuthorizeClientAction(context.Background(), alice, term.channel, input))
		env.backend.HandleClientAction(context.Background(), alice, term.channel, input, nil)
	}
	// The shell reads nothing, so the next input is refused, not dropped.
	require.Error(t, env.backend.AuthorizeClientAction(context.Background(), alice, term.channel, input))
	require.Len(t, term.input, ahpTerminalInputQueue)
}

func TestAHPTerminalURIsAreNotReused(t *testing.T) {
	env := newAHPTestEnv(t)
	alice := ahphost.Principal{UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	term := &ahpTerminal{ctx: ctx, channel: "ahp-terminal:/a", input: make(chan []byte, ahpTerminalInputQueue), cancel: cancel}
	env.backend.mu.Lock()
	env.backend.shells[ahpTerminalKey(alice.UserKey, term.channel)] = term
	env.backend.mu.Unlock()
	input := ahptypes.StateAction{Value: &ahptypes.TerminalInputAction{Type: ahptypes.ActionTypeTerminalInput, Data: "rm -rf old\n"}}
	require.NoError(t, env.backend.AuthorizeClientAction(context.Background(), alice, term.channel, input))
	require.NoError(t, env.backend.DisposeTerminal(context.Background(), alice, term.channel))
	// Queued input for the ended terminal finds nothing to reach.
	env.backend.HandleClientAction(context.Background(), alice, term.channel, input, nil)
	require.Empty(t, term.input)
	env.backend.mu.Lock()
	_, retired := env.backend.retired[ahpTerminalKey(alice.UserKey, term.channel)]
	env.backend.mu.Unlock()
	require.True(t, retired, "the URI cannot name a new terminal")
}

func TestAHPWorkspaceGitChangesFinishAndTakeTurns(t *testing.T) {
	w := &ahpWorkspaces{}
	alice := ahphost.Principal{UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	release := make(chan struct{})
	finished := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	holding := make(chan struct{})
	go func() {
		first <- w.withGit(ctx, alice, func(changeCtx context.Context) error {
			close(holding)
			<-release
			finished <- changeCtx.Err()
			return nil
		})
	}()
	<-holding
	ran := make(chan struct{})
	go func() {
		_ = w.withGit(context.Background(), alice, func(context.Context) error {
			close(ran)
			return nil
		})
	}()
	cancel()
	require.ErrorIs(t, <-first, context.Canceled, "the caller stops waiting")
	select {
	case <-ran:
		t.Fatal("a second change ran while the first held the lock")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-finished, "the change kept running after its caller left")
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("the second change never ran")
	}
}
