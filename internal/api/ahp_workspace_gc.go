/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orka-agents/orka/internal/ahphost"
)

const (
	ahpWorkspaceActiveAnnotation = "orka.ai/ahp-workspace-active-at"
	ahpWorkspaceActiveEvery      = 10 * time.Minute
	ahpWorkspaceCollectEvery     = 5 * time.Minute
)

func (w *ahpWorkspaces) touch(principal ahphost.Principal, pod *corev1.Pod) {
	if !w.enabled() || pod == nil {
		return
	}
	now := w.b.now().UTC()
	w.mu.Lock()
	if w.active == nil {
		w.active = map[string]time.Time{}
	}
	label := principal.UserKey[:40]
	last := w.active[label]
	w.active[label] = now
	w.mu.Unlock()
	recorded, err := time.Parse(time.RFC3339, pod.Annotations[ahpWorkspaceActiveAnnotation])
	if now.Sub(last) < ahpWorkspaceActiveEvery && err == nil && now.Sub(recorded) < ahpWorkspaceActiveEvery {
		return
	}
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[ahpWorkspaceActiveAnnotation] = now.Format(time.RFC3339)
	_ = w.b.h.client.Patch(context.Background(), pod, patch)
}

func (w *ahpWorkspaces) collectIdle(ctx context.Context) {
	if !w.enabled() || w.b.cfg.WorkspaceIdleTimeout <= 0 {
		return
	}
	ticker := time.NewTicker(ahpWorkspaceCollectEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.collectOnce(ctx)
		}
	}
}

func (w *ahpWorkspaces) collectOnce(ctx context.Context) {
	var pods corev1.PodList
	if err := w.reader().List(ctx, &pods, client.InNamespace(w.namespace),
		client.MatchingLabels{"app.kubernetes.io/component": ahpWorkspaceComponent}); err != nil {
		return
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		label := pod.Labels[ahpWorkspaceUserLabel]
		if label == "" || !pod.DeletionTimestamp.IsZero() || !w.idle(label, pod) {
			continue
		}
		w.collect(ctx, label, pod)
	}
}

func (w *ahpWorkspaces) collect(ctx context.Context, label string, listed *corev1.Pod) {
	done, ok := w.beginCollect(label)
	if !ok {
		return
	}
	defer done()
	pod := &corev1.Pod{}
	if err := w.reader().Get(ctx, client.ObjectKeyFromObject(listed), pod); err != nil || pod.UID != listed.UID ||
		!pod.DeletionTimestamp.IsZero() || !w.idle(label, pod) || w.gitBusy(label) {
		return
	}
	if pod.Status.Phase == corev1.PodRunning && w.holdsWork(ctx, pod) {
		return
	}
	_ = w.b.h.client.Delete(ctx, pod, client.Preconditions{UID: &pod.UID})
}

func (w *ahpWorkspaces) idle(label string, pod *corev1.Pod) bool {
	if w.inUse(label) {
		return false
	}
	w.mu.Lock()
	last := w.active[label]
	w.mu.Unlock()
	return ahpWorkspaceIdle(w.b.now(), last, pod, w.b.cfg.WorkspaceIdleTimeout)
}

func (w *ahpWorkspaces) beginCollect(label string) (func(), bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.collecting == nil {
		w.collecting = map[string]chan struct{}{}
	}
	if _, busy := w.collecting[label]; busy {
		return nil, false
	}
	finished := make(chan struct{})
	w.collecting[label] = finished
	return func() {
		w.mu.Lock()
		delete(w.collecting, label)
		w.mu.Unlock()
		close(finished)
	}, true
}

func (w *ahpWorkspaces) claimForUse(ctx context.Context, principal ahphost.Principal) error {
	label := principal.UserKey[:40]
	w.mu.Lock()
	if w.active == nil {
		w.active = map[string]time.Time{}
	}
	w.active[label] = w.b.now().UTC()
	finished := w.collecting[label]
	w.mu.Unlock()
	if finished == nil {
		return nil
	}
	select {
	case <-finished:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *ahpWorkspaces) gitBusy(label string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for key, lock := range w.git {
		if strings.HasPrefix(key, label) && len(lock) > 0 {
			return true
		}
	}
	return false
}

func (w *ahpWorkspaces) inUse(label string) bool {
	b := w.b
	b.mu.Lock()
	defer b.mu.Unlock()
	for key := range b.shells {
		if strings.HasPrefix(key, label) {
			return true
		}
	}
	for key := range b.turns {
		if strings.HasPrefix(key, label) {
			return true
		}
	}
	for key := range b.reserved {
		if strings.HasPrefix(key, label) {
			return true
		}
	}
	return false
}

func ahpWorkspaceIdle(now, last time.Time, pod *corev1.Pod, idle time.Duration) bool {
	if recorded, err := time.Parse(time.RFC3339, pod.Annotations[ahpWorkspaceActiveAnnotation]); err == nil && recorded.After(last) {
		last = recorded
	}
	if created := pod.CreationTimestamp.Time; created.After(last) {
		last = created
	}
	return now.Sub(last) >= idle
}

const ahpHoldsWorkScript = `for p in "$1"/src/*/*; do
  [ -d "$p/.git" ] || continue
  # Every ref and every worktree's HEAD: branches, tags, notes and the stash.
  n=$(git -C "$p" rev-list --count --all --not --remotes 2>/dev/null) || { echo "error $p"; continue; }
  [ "$n" = 0 ] || echo "unpushed $p"
  # A submodule keeps its own branches and stashes: keep any checkout with one.
  [ ! -e "$p/.git/modules" ] || echo "submodules $p"
  wts=$(git -C "$p" worktree list --porcelain 2>/dev/null) || { echo "error $p"; continue; }
  printf '%s\n' "$wts" | sed -n 's/^worktree //p' | while IFS= read -r wt; do
    warn=$(mktemp) || { echo "error $wt"; continue; }
    c=$(git -C "$wt" status --porcelain --untracked-files=all 2>"$warn") || echo "error $wt"
    # A warning, such as an unreadable directory, means git could not look.
    [ ! -s "$warn" ] || echo "error $wt"
    rm -f "$warn"
    [ -z "$c" ] || echo "changed $wt"
    # Files git was told not to check could hold changes it does not report.
    f=$(git -C "$wt" ls-files -v 2>/dev/null | grep -E '^([a-z]|S) ' | head -n 1)
    [ -z "$f" ] || echo "flagged $wt"
    [ ! -e "$wt/.gitmodules" ] || echo "submodules $wt"
  done
done
# Any file outside the checkouts, anywhere under the root, except shell
# history in HOME. A traversal that fails keeps the workspace.
root=$1
roots=$(mktemp) || { echo "error mktemp"; exit 0; }
found=$(mktemp) || { echo "error mktemp"; exit 0; }
for p in "$root"/src/*/*; do
  [ -d "$p/.git" ] || continue
  git -C "$p" worktree list --porcelain 2>/dev/null | sed -n 's/^worktree //p' >> "$roots" || echo "error $p"
done
set -- -path "$root/.clones" -prune -o
for h in .ash_history .bash_history .sh_history .lesshst; do set -- "$@" -path "$root/.home/$h" -prune -o; done
while IFS= read -r r; do set -- "$@" -path "$r" -prune -o; done < "$roots"
find "$root" "$@" ! -type d -print > "$found" 2>/dev/null || echo "error find"
head -n 1 "$found" | sed 's/^/other /'
rm -f "$roots" "$found"
echo done`

func (w *ahpWorkspaces) holdsWork(ctx context.Context, pod *corev1.Pod) bool {
	out, err := w.output(ctx, pod, 64<<10, ahpShell, "-c", ahpHoldsWorkScript, "sh", ahpWorkspaceRoot)
	if err != nil {
		return true
	}
	return strings.TrimSpace(string(out)) != "done"
}
