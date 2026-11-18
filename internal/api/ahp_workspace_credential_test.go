/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/orka-agents/orka/internal/ahphost"
)

// The helper is shell that git runs; run it here against a token file.
func TestAHPGitCredentialHelper(t *testing.T) {
	file := filepath.Join(t.TempDir(), "github-token")
	helper := strings.Replace(strings.TrimPrefix(ahpGitCredentialHelper, "!"), ahpGitTokenFile, file, 1)
	run := func(action string) string {
		// Git appends the action to a shell helper's command.
		out, err := exec.Command("/bin/sh", "-c", helper+" "+action).Output()
		require.NoError(t, err)
		return string(out)
	}
	require.Empty(t, run("get"), "no token means anonymous access")
	require.NoError(t, os.WriteFile(file, []byte("not-a-real-token"), 0o600))
	require.Equal(t, "username=x-access-token\npassword=not-a-real-token\n", run("get"))
	require.Empty(t, run("store"), "git's store and erase are ignored")
}

func TestAHPWorkspaceAsksGitHubOnlyThroughTheHelper(t *testing.T) {
	w := &ahpWorkspaces{namespace: "orka-ahp-workspaces", image: "example.com/git@sha256:" + ahpHash("x")}
	pod := w.podSpec(ahphost.Principal{UserKey: ahphost.UserKey("https://issuer.example", "alice")})
	env := map[string]string{}
	for _, v := range pod.Spec.Containers[0].Env {
		env[v.Name] = v.Value
	}
	require.Equal(t, "1", env["GIT_CONFIG_COUNT"])
	require.Equal(t, "credential.https://github.com.helper", env["GIT_CONFIG_KEY_0"])
	require.Equal(t, ahpGitCredentialHelper, env["GIT_CONFIG_VALUE_0"])
	require.Equal(t, "0", env["GIT_TERMINAL_PROMPT"])
}

// A second workspace request that arrives while the person's token is being
// installed waits for that installation, and the token is fetched once.
// Terminal output is redacted for a token before the workspace can read it,
// and for the token it replaced.
func TestAHPWorkspaceTokenIsKnownBeforeItIsWritten(t *testing.T) {
	token := "first-token-0123456789"
	b := &ahpBackend{now: time.Now, cfg: AHPBackendConfig{
		RepositoryCredential: func(context.Context, string, string, string) (string, error) { return token, nil },
	}}
	principal := ahphost.Principal{UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	var known []string
	w := &ahpWorkspaces{
		b: b, namespace: "orka-ahp-workspaces", image: "example.com/git@sha256:" + ahpHash("x"),
		rest: &rest.Config{}, clientset: k8sfake.NewClientset(),
	}
	w.writeToken = func(context.Context, *corev1.Pod, string) error {
		known = w.tokensFor(principal.UserKey)
		return nil
	}
	w.provideCredential(context.Background(), principal, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "pod-1"}})
	require.Equal(t, []string{"first-token-0123456789"}, known)

	token = "second-token-0123456789"
	w.provideCredential(context.Background(), principal, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "pod-2"}})
	require.Equal(t, []string{"first-token-0123456789", "second-token-0123456789"}, known)

	// Writes that fail never make output forget the token still installed.
	w.writeToken = func(context.Context, *corev1.Pod, string) error { return errors.New("exec failed") }
	for i, next := range []string{"third-token-0123456789", "fourth-token-0123456789"} {
		token = next
		w.provideCredential(context.Background(), principal, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: types.UID(fmt.Sprintf("pod-%d", 3+i))}})
	}
	require.Equal(t, []string{"first-token-0123456789", "second-token-0123456789", "third-token-0123456789", "fourth-token-0123456789"},
		w.tokensFor(principal.UserKey))
}

// A workspace an earlier run of this server gave a token is redacted for it
// before anything in it is shown, and is not shown while the token cannot be
// read.
func TestAHPWorkspaceRecoversTheTokenAnEarlierRunInstalled(t *testing.T) {
	b := &ahpBackend{now: time.Now, cfg: AHPBackendConfig{
		RepositoryCredential: func(context.Context, string, string, string) (string, error) { return "", nil },
	}}
	var reads atomic.Int32
	readErr := errors.New("exec failed")
	w := &ahpWorkspaces{
		b: b, namespace: "orka-ahp-workspaces", image: "example.com/git@sha256:" + ahpHash("x"),
		rest: &rest.Config{}, clientset: k8sfake.NewClientset(),
		readToken: func(context.Context, *corev1.Pod) (string, error) {
			reads.Add(1)
			return "earlier-token-0123456789\n", readErr
		},
	}
	principal := ahphost.Principal{UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "pod-1"}}

	require.Error(t, w.recoverToken(context.Background(), principal, pod), "an unreadable token stops the workspace being shown")
	require.Empty(t, w.tokensFor(principal.UserKey))
	readErr = nil
	require.NoError(t, w.recoverToken(context.Background(), principal, pod))
	require.Equal(t, []string{"earlier-token-0123456789"}, w.tokensFor(principal.UserKey))
	require.NoError(t, w.recoverToken(context.Background(), principal, pod))
	require.Equal(t, int32(2), reads.Load(), "a Pod's token is read back once")
	require.NoError(t, w.recoverToken(context.Background(), principal, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "pod-2"}}))
	require.Equal(t, int32(3), reads.Load(), "a new Pod's token is read back")
	require.Equal(t, []string{"earlier-token-0123456789"}, w.tokensFor(principal.UserKey))
	require.Equal(t, "the [REDACTED] here", string(ahpRedactWholeTokens([]byte("the earlier-token-0123456789 here"), w.tokensFor(principal.UserKey))))
}

// Output stays redacted for a replaced token for a day after it was last
// given, never for the token given last, and for at most
// ahpTokenRedactionMax tokens.
func TestAHPWorkspaceTokenHistoryIsBounded(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	w := &ahpWorkspaces{b: &ahpBackend{now: func() time.Time { return now }}}
	alice := ahphost.UserKey("https://issuer.example", "alice")
	w.rememberToken(alice, "old-token-0123456789")
	now = now.Add(time.Hour)
	w.rememberToken(alice, "kept-token-0123456789")
	now = now.Add(ahpTokenRedactionKeep)
	w.rememberToken(alice, "kept-token-0123456789")
	w.rememberToken(alice, "new-token-0123456789")
	require.Equal(t, []string{"kept-token-0123456789", "new-token-0123456789"}, w.tokensFor(alice),
		"a token not given for a day goes; one given again stays")

	for i := range ahpTokenRedactionMax + 5 {
		w.rememberToken(alice, fmt.Sprintf("rotated-token-%04d", i))
	}
	tokens := w.tokensFor(alice)
	require.Len(t, tokens, ahpTokenRedactionMax)
	require.Equal(t, fmt.Sprintf("rotated-token-%04d", ahpTokenRedactionMax+4), tokens[len(tokens)-1], "the token given last stays")
}

// A token the person no longer holds that cannot be removed from their
// workspace is tried again on the next request, not counted as removed.
func TestAHPWorkspaceRetriesATokenItCouldNotRemove(t *testing.T) {
	b := &ahpBackend{now: time.Now, cfg: AHPBackendConfig{
		RepositoryCredential: func(context.Context, string, string, string) (string, error) {
			return "", errors.New("the link is gone")
		},
	}}
	var removals atomic.Int32
	removeErr := errors.New("exec failed")
	w := &ahpWorkspaces{
		b: b, namespace: "orka-ahp-workspaces", image: "example.com/git@sha256:" + ahpHash("x"),
		rest: &rest.Config{}, clientset: k8sfake.NewClientset(),
		removeToken: func(context.Context, *corev1.Pod) error { removals.Add(1); return removeErr },
	}
	principal := ahphost.Principal{UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "pod-1"}}

	w.provideCredential(context.Background(), principal, pod)
	w.provideCredential(context.Background(), principal, pod)
	require.Equal(t, int32(2), removals.Load(), "a failed removal is tried again")

	removeErr = nil
	w.provideCredential(context.Background(), principal, pod)
	w.provideCredential(context.Background(), principal, pod)
	require.Equal(t, int32(3), removals.Load(), "a removal that worked counts")
}

func TestAHPWorkspaceCredentialInstallsOnceWhileRequestsOverlap(t *testing.T) {
	var fetches atomic.Int32
	b := &ahpBackend{now: time.Now, cfg: AHPBackendConfig{
		RepositoryCredential: func(context.Context, string, string, string) (string, error) {
			fetches.Add(1)
			return "not-a-real-token", nil
		},
	}}
	started, release := make(chan struct{}), make(chan struct{})
	w := &ahpWorkspaces{
		b: b, namespace: "orka-ahp-workspaces", image: "example.com/git@sha256:" + ahpHash("x"),
		rest: &rest.Config{}, clientset: k8sfake.NewClientset(),
		writeToken: func(context.Context, *corev1.Pod, string) error {
			close(started)
			<-release
			return nil
		},
	}
	principal := ahphost.Principal{UserKey: ahphost.UserKey("https://issuer.example", "alice")}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: "pod-1"}}

	first := make(chan struct{})
	go func() { w.provideCredential(context.Background(), principal, pod); close(first) }()
	<-started
	second := make(chan struct{})
	go func() { w.provideCredential(context.Background(), principal, pod); close(second) }()
	select {
	case <-second:
		t.Fatal("a request returned while the token was still being installed")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	<-first
	<-second
	require.Equal(t, int32(1), fetches.Load(), "the waiting request used the finished installation")

	// A request whose own context ends stops waiting.
	w.writeToken = func(ctx context.Context, _ *corev1.Pod, _ string) error { <-ctx.Done(); return ctx.Err() }
	w.provided = nil
	// The test's context ends the blocked installation when the test does.
	go w.provideCredential(t.Context(), principal, pod)
	require.Eventually(t, func() bool { w.mu.Lock(); defer w.mu.Unlock(); return w.providing[principal.UserKey] != nil },
		time.Second, 10*time.Millisecond)
	waiter, cancelWaiter := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelWaiter()
	w.provideCredential(waiter, principal, pod)
	require.ErrorIs(t, waiter.Err(), context.DeadlineExceeded)
}
