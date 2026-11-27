/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/orka-agents/orka/internal/ahphost"
)

const (
	ahpGitTokenDir         = "/tmp/orka-git"
	ahpGitTokenFile        = ahpGitTokenDir + "/github-token"
	ahpGitTokenEvery       = 20 * time.Minute
	ahpGitCredentialHelper = `!f() { test "$1" = get || exit 0; t=$(cat ` + ahpGitTokenFile +
		` 2>/dev/null) || exit 0; test -n "$t" || exit 0; echo username=x-access-token; echo "password=$t"; }; f`
)

const AHPWorkspaceTokenHorizon = 45 * time.Minute

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
	w.rememberToken(principal.UserKey, token)
	if err := write(writeCtx, pod, token); err != nil {
		return time.Time{}, false
	}
	return now, true
}

func (w *ahpWorkspaces) recoverToken(ctx context.Context, principal ahphost.Principal, pod *corev1.Pod) error {
	if w.b == nil {
		return nil
	}
	w.mu.Lock()
	done := w.recovered[principal.UserKey] == pod.UID
	w.mu.Unlock()
	if done {
		return nil
	}
	read := w.readToken
	if read == nil {
		read = w.execReadToken
	}
	token, err := read(ctx, pod)
	if err != nil {
		return fmt.Errorf("read the workspace's git token: %w", err)
	}
	if token = strings.TrimSpace(token); token != "" && !strings.ContainsAny(token, "\n\r") {
		w.rememberToken(principal.UserKey, token)
	}
	w.mu.Lock()
	if w.recovered == nil {
		w.recovered = map[string]types.UID{}
	}
	w.recovered[principal.UserKey] = pod.UID
	w.mu.Unlock()
	return nil
}

func (w *ahpWorkspaces) execReadToken(ctx context.Context, pod *corev1.Pod) (string, error) {
	out, err := w.output(ctx, pod, 4<<10, ahpShell, "-c", `[ -f "$1" ] || exit 0; exec cat -- "$1"`, "sh", ahpGitTokenFile)
	return string(out), err
}

type ahpGivenToken struct {
	value string
	at    time.Time
}

const (
	ahpTokenRedactionKeep = 24 * time.Hour
	ahpTokenRedactionMax  = 128
)

func (w *ahpWorkspaces) rememberToken(userKey, token string) {
	now := time.Now()
	if w.b != nil && w.b.now != nil {
		now = w.b.now()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.tokens == nil {
		w.tokens = map[string][]ahpGivenToken{}
	}
	given := slices.DeleteFunc(w.tokens[userKey], func(t ahpGivenToken) bool {
		return t.value == token || now.Sub(t.at) > ahpTokenRedactionKeep
	})
	given = append(given, ahpGivenToken{value: token, at: now})
	if extra := len(given) - ahpTokenRedactionMax; extra > 0 {
		given = given[extra:]
	}
	w.tokens[userKey] = given
}

func (w *ahpWorkspaces) tokensFor(userKey string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	tokens := make([]string, 0, len(w.tokens[userKey]))
	for _, given := range w.tokens[userKey] {
		tokens = append(tokens, given.value)
	}
	return tokens
}

func (w *ahpWorkspaces) execWriteToken(ctx context.Context, pod *corev1.Pod, token string) error {
	return w.exec(ctx, pod, []string{ahpShell, "-c",
		`umask 077; mkdir -p "$1"; cat > "$2.next" && mv "$2.next" "$2"`, "sh", ahpGitTokenDir, ahpGitTokenFile},
		remotecommand.StreamOptions{Stdin: strings.NewReader(token), Stdout: &ahpLimitedBuffer{limit: 4 << 10}, Stderr: &ahpLimitedBuffer{limit: 4 << 10}})
}

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
		_, _ = w.ensure(ctx, principal, 0)
	}()
}

type ahpProvidedCredential struct {
	pod string
	at  time.Time
}
