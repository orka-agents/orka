/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/orka-agents/orka/internal/ahphost"
)

const (
	// ahpGitTokenFile holds the person's github.com token in their workspace,
	// readable by them only, on the Pod's private /tmp.
	ahpGitTokenDir  = "/tmp/orka-git"
	ahpGitTokenFile = ahpGitTokenDir + "/github-token"
	// ahpGitTokenEvery is how often a workspace's token is replaced; a token
	// stays valid for at least AHPWorkspaceTokenHorizon.
	ahpGitTokenEvery = 20 * time.Minute
	// ahpGitCredentialHelper answers git's "get" with the token file, and
	// nothing when there is none, so git falls back to anonymous access.
	ahpGitCredentialHelper = `!f() { test "$1" = get || exit 0; t=$(cat ` + ahpGitTokenFile +
		` 2>/dev/null) || exit 0; test -n "$t" || exit 0; echo username=x-access-token; echo "password=$t"; }; f`
)

// AHPWorkspaceTokenHorizon is how long a token provided to a workspace stays
// valid at least: past the next replacement, with room for a long fetch.
const AHPWorkspaceTokenHorizon = 45 * time.Minute

// provideCredential puts the person's own github.com token in their
// workspace, from their linked account, at most every ahpGitTokenEvery. A
// person without a usable link gets none, and a token they no longer hold
// is removed. One installation runs per person at a time: a request that
// finds one under way waits for it, so it never clones private repositories
// before the token is there, and only a finished installation counts.
func (w *ahpWorkspaces) provideCredential(ctx context.Context, principal ahphost.Principal, pod *corev1.Pod) {
	fetch := w.b.cfg.RepositoryCredential
	if fetch == nil || !w.enabled() {
		return
	}
	for {
		now := w.b.now()
		w.mu.Lock()
		last, provided := w.provided[principal.UserKey]
		if provided && last.pod == string(pod.UID) && now.Sub(last.at) < ahpGitTokenEvery {
			w.mu.Unlock()
			return
		}
		if installing := w.providing[principal.UserKey]; installing != nil {
			w.mu.Unlock()
			select {
			case <-installing:
				continue
			case <-ctx.Done():
				return
			}
		}
		done := make(chan struct{})
		if w.providing == nil {
			w.providing = map[string]chan struct{}{}
		}
		w.providing[principal.UserKey] = done
		w.mu.Unlock()

		at, record := w.installCredential(ctx, principal, pod, fetch, now)
		w.mu.Lock()
		if record {
			if w.provided == nil {
				w.provided = map[string]ahpProvidedCredential{}
			}
			w.provided[principal.UserKey] = ahpProvidedCredential{pod: string(pod.UID), at: at}
		}
		delete(w.providing, principal.UserKey)
		close(done)
		w.mu.Unlock()
		return
	}
}

// installCredential fetches the person's token and writes it to their
// workspace. It reports when the attempt counts as made, and whether to
// record it: a token that did not arrive, or one that could not be removed,
// is tried again on the next request.
func (w *ahpWorkspaces) installCredential(ctx context.Context, principal ahphost.Principal, pod *corev1.Pod,
	fetch func(ctx context.Context, namespace, issuer, subject string) (string, error), now time.Time) (time.Time, bool) {
	token, err := fetch(ctx, principal.Namespace, principal.Issuer, principal.Subject)
	if err != nil || strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\n\r") {
		remove := w.removeToken
		if remove == nil {
			remove = func(ctx context.Context, pod *corev1.Pod) error { return w.run(ctx, pod, "rm", "-f", ahpGitTokenFile) }
		}
		if remove(ctx, pod) != nil {
			return time.Time{}, false
		}
		if err != nil {
			// Ask again in half the usual time: the person may link their
			// account at any moment.
			return now.Add(-ahpGitTokenEvery / 2), true
		}
		return now, true
	}
	writeCtx, cancel := context.WithTimeout(ctx, ahpWorkspaceExecTimeout)
	defer cancel()
	write := w.writeToken
	if write == nil {
		write = w.execWriteToken
	}
	// Terminal output is redacted for the token before it can be read in
	// the workspace.
	w.rememberToken(principal.UserKey, token)
	if err := write(writeCtx, pod, token); err != nil {
		return time.Time{}, false
	}
	return now, true
}

// rememberToken records a token given to a user's workspace. Terminal output
// stays redacted for every token the workspace was given while this server
// runs: a token may never expire, and a shell can hold one long after it was
// replaced. The list grows only with distinct tokens, and a person's token
// changes only when it is refreshed near its expiry.
func (w *ahpWorkspaces) rememberToken(userKey, token string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if slices.Contains(w.tokens[userKey], token) {
		return
	}
	if w.tokens == nil {
		w.tokens = map[string][]string{}
	}
	w.tokens[userKey] = append(w.tokens[userKey], token)
}

// tokensFor returns the git tokens a user's workspace was given, oldest first.
func (w *ahpWorkspaces) tokensFor(userKey string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.tokens[userKey])
}

// execWriteToken writes a token to the workspace. It travels on standard
// input, never in a command line.
func (w *ahpWorkspaces) execWriteToken(ctx context.Context, pod *corev1.Pod, token string) error {
	return w.exec(ctx, pod, []string{ahpShell, "-c",
		`umask 077; mkdir -p "$1"; cat > "$2.next" && mv "$2.next" "$2"`, "sh", ahpGitTokenDir, ahpGitTokenFile},
		remotecommand.StreamOptions{Stdin: strings.NewReader(token), Stdout: &ahpLimitedBuffer{limit: 4 << 10}, Stderr: &ahpLimitedBuffer{limit: 4 << 10}})
}

// refreshCredentialForTerminal renews the person's token while they use a
// terminal: input goes straight to the shell, which would otherwise keep a
// token past its expiry. It never blocks the input, and it starts at most one
// renewal per person once the last token is due for replacement.
func (w *ahpWorkspaces) refreshCredentialForTerminal(principal ahphost.Principal) {
	if !w.enabled() || w.b.cfg.RepositoryCredential == nil {
		return
	}
	w.mu.Lock()
	last, ok := w.provided[principal.UserKey]
	if (ok && w.b.now().Sub(last.at) < ahpGitTokenEvery) || w.refreshing[principal.UserKey] {
		w.mu.Unlock()
		return
	}
	if w.refreshing == nil {
		w.refreshing = map[string]bool{}
	}
	w.refreshing[principal.UserKey] = true
	w.mu.Unlock()
	go func() {
		defer func() {
			w.mu.Lock()
			delete(w.refreshing, principal.UserKey)
			w.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 2*ahpWorkspaceExecTimeout)
		defer cancel()
		// ensure finds the running workspace and provides the token, checking
		// again that this connection may use the workspace.
		_, _ = w.ensure(ctx, principal, 0)
	}()
}

// ahpProvidedCredential records when a workspace last got its token.
type ahpProvidedCredential struct {
	pod string
	at  time.Time
}
