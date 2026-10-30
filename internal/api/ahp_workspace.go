/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/orka-agents/orka/internal/ahphost"
)

// An AHP user's workspace is one Pod that stands in for the host clients
// expect: their projects are git checkouts at their project paths, each
// session has a git worktree at its working directory, and terminals are
// shells in it. It holds no credential: repositories are cloned anonymously,
// agents never run there, and each turn's Task still works in its own
// workspace and publishes to the session's branch, which the worktree then
// follows.
const (
	ahpWorkspaceContainer = "workspace"
	ahpWorkspaceRoot      = "/workspace"
	ahpWorktreeRoot       = "/workspace/worktrees"
	ahpWorkspaceComponent = "ahp-workspace"
	ahpWorkspaceUserLabel = "orka.ai/ahp-user"
	// ahpWorkspaceRestoredAnnotation marks a Pod whose projects and worktrees
	// were brought back after it started.
	ahpWorkspaceRestoredAnnotation = "orka.ai/ahp-workspace-restored"
	ahpWorkspaceStartWait          = 20 * time.Second
	ahpWorkspaceExecTimeout        = 2 * time.Minute
	// ahpWorkspaceGitTimeout bounds one git change in a workspace, a clone
	// included, however long its caller waits for it.
	ahpWorkspaceGitTimeout = 10 * time.Minute
	// ahpWorkspaceSetupWait bounds how long a request waits for a checkout.
	ahpWorkspaceSetupWait = 2 * time.Minute
	ahpWorkspaceUID       = 1000
	ahpExecOutputMax      = 64 << 10
)

var ahpGitRefName = regexp.MustCompile(`^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$`)

// ahpValidBranch accepts branch names that are safe as git refs and paths,
// and that git can create: git check-ref-format refuses, among others, a name
// that ends with a dot.
func ahpValidBranch(branch string) bool {
	if branch == "" || len(branch) > 200 || !ahpGitRefName.MatchString(branch) || strings.HasPrefix(branch, "-") ||
		strings.HasSuffix(branch, ".") {
		return false
	}
	for segment := range strings.SplitSeq(branch, "/") {
		if segment == "." || segment == ".." || strings.HasPrefix(segment, ".") || strings.HasSuffix(segment, ".lock") {
			return false
		}
	}
	return !strings.Contains(branch, "..")
}

type ahpWorkspaces struct {
	b         *ahpBackend
	clientset kubernetes.Interface
	rest      *rest.Config
	namespace string
	image     string

	// startWait bounds how long a request waits for a starting workspace.
	startWait time.Duration

	mu        sync.Mutex
	starting  map[string]chan struct{}
	restoring map[string]chan struct{}
	// active is each user's last use of their workspace, by user label.
	active map[string]time.Time
	// provided is when each user's workspace last got their git token.
	provided map[string]ahpProvidedCredential
	// refreshing are the users whose token a terminal's input is renewing.
	refreshing map[string]bool
	// tokens are the git tokens each user's workspace was given, which
	// terminal output never carries.
	tokens map[string][]string
	// providing are the users whose token is being installed; it closes when
	// the installation ends.
	providing map[string]chan struct{}
	// writeToken installs a token in a workspace; nil writes it with exec.
	writeToken func(ctx context.Context, pod *corev1.Pod, token string) error
	// removeToken removes a workspace's token; nil removes it with exec.
	removeToken func(ctx context.Context, pod *corev1.Pod) error
	// collecting are the workspaces an idle collection is deciding about.
	collecting map[string]chan struct{}
	// git serializes each user's clones and worktree changes, which share
	// the projects' checkouts.
	git map[string]chan struct{}
}

func (w *ahpWorkspaces) enabled() bool {
	return w != nil && w.image != "" && w.namespace != "" && w.rest != nil && w.clientset != nil
}

func ahpWorkspacePodName(userKey string) string { return "ahp-ws-" + userKey[:20] }

func (w *ahpWorkspaces) reader() client.Reader {
	if w.b.h.apiReader != nil {
		return w.b.h.apiReader
	}
	return w.b.h.client
}

func (w *ahpWorkspaces) podSpec(principal ahphost.Principal) *corev1.Pod {
	no, yes := false, true
	uid := int64(ahpWorkspaceUID)
	workspaceLimit := resource.MustParse("10Gi")
	tmpLimit := resource.MustParse("1Gi")
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: ahpWorkspacePodName(principal.UserKey), Namespace: w.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name": "orka", "app.kubernetes.io/component": ahpWorkspaceComponent,
				ahpWorkspaceUserLabel: principal.UserKey[:40],
			},
		},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: &no,
			EnableServiceLinks:           &no,
			RestartPolicy:                corev1.RestartPolicyAlways,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: &yes, RunAsUser: &uid, RunAsGroup: &uid, FSGroup: &uid,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name: ahpWorkspaceContainer, Image: w.image,
				// Keep the Pod alive for exec; nothing else runs.
				Command:    []string{ahpShell, "-c", ahpWorkspaceKeepAlive},
				WorkingDir: ahpWorkspaceRoot,
				Env: []corev1.EnvVar{
					{Name: "HOME", Value: ahpWorkspaceRoot + "/.home"},
					{Name: "GIT_TERMINAL_PROMPT", Value: "0"},
					{Name: "TERM", Value: "xterm-256color"},
					// Git asks this helper for github.com only. It answers with
					// the person's own token when Orka has provided one.
					{Name: "GIT_CONFIG_COUNT", Value: "1"},
					{Name: "GIT_CONFIG_KEY_0", Value: "credential.https://github.com.helper"},
					{Name: "GIT_CONFIG_VALUE_0", Value: ahpGitCredentialHelper},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes, RunAsNonRoot: &yes,
					Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "workspace", MountPath: ahpWorkspaceRoot}, {Name: "tmp", MountPath: "/tmp"}},
			}},
			Volumes: []corev1.Volume{
				{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &workspaceLimit}}},
				{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &tmpLimit}}},
			},
		},
	}
}

// ensure returns the user's running workspace Pod, creating it if needed and
// waiting at most wait for it to start. A new Pod gets the user's projects
// and session worktrees again.
func (w *ahpWorkspaces) ensure(ctx context.Context, principal ahphost.Principal, wait time.Duration) (*corev1.Pod, error) {
	if w != nil && w.startWait > 0 && wait > w.startWait {
		wait = w.startWait
	}
	if !w.enabled() {
		return nil, errors.New("workspaces are not enabled on this host")
	}
	if err := w.claimForUse(ctx, principal); err != nil {
		return nil, err
	}
	name := ahpWorkspacePodName(principal.UserKey)
	deadline := time.Now().Add(wait)
	created := false
	for {
		pod := &corev1.Pod{}
		err := w.reader().Get(ctx, types.NamespacedName{Namespace: w.namespace, Name: name}, pod)
		switch {
		case apierrors.IsNotFound(err):
			if err := w.b.h.client.Create(ctx, w.podSpec(principal)); err != nil && !apierrors.IsAlreadyExists(err) {
				return nil, fmt.Errorf("create the workspace: %w", err)
			}
			created = true
		case err != nil:
			return nil, err
		case pod.Labels[ahpWorkspaceUserLabel] != principal.UserKey[:40]:
			return nil, errors.New("the workspace Pod belongs to someone else")
		case !pod.DeletionTimestamp.IsZero():
		case pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded:
			_ = w.b.h.client.Delete(ctx, pod, client.Preconditions{UID: &pod.UID})
		case pod.Status.Phase == corev1.PodRunning && ahpContainerRunning(pod):
			w.touch(principal, pod.DeepCopy())
			// The token comes first: a restore may clone private repositories.
			w.provideCredential(ctx, principal, pod)
			if created || pod.Annotations[ahpWorkspaceRestoredAnnotation] == "" {
				w.restoreInBackground(principal, pod.DeepCopy())
			}
			return pod, nil
		}
		if time.Now().After(deadline) {
			return nil, errors.New("the workspace is starting; try again shortly")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// runningForCleanup returns the user's running workspace Pod, or none when it
// is gone, being deleted or not running: then nothing it ran still runs. An
// error means that could not be told.
func (w *ahpWorkspaces) runningForCleanup(ctx context.Context, principal ahphost.Principal) (*corev1.Pod, error) {
	if !w.enabled() {
		return nil, nil
	}
	pod := &corev1.Pod{}
	if err := w.reader().Get(ctx, types.NamespacedName{Namespace: w.namespace, Name: ahpWorkspacePodName(principal.UserKey)}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if pod.Labels[ahpWorkspaceUserLabel] != principal.UserKey[:40] || !pod.DeletionTimestamp.IsZero() ||
		pod.Status.Phase != corev1.PodRunning || !ahpContainerRunning(pod) {
		return nil, nil
	}
	return pod, nil
}

func ahpContainerRunning(pod *corev1.Pod) bool {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == ahpWorkspaceContainer && status.State.Running != nil {
			return true
		}
	}
	return false
}

// warm starts the user's workspace in the background, so the first terminal
// does not wait for it.
func (w *ahpWorkspaces) warm(principal ahphost.Principal) {
	if !w.enabled() {
		return
	}
	w.mu.Lock()
	if w.starting == nil {
		w.starting = map[string]chan struct{}{}
	}
	if _, running := w.starting[principal.UserKey]; running {
		w.mu.Unlock()
		return
	}
	done := make(chan struct{})
	w.starting[principal.UserKey] = done
	w.mu.Unlock()
	go func() {
		defer func() {
			w.mu.Lock()
			delete(w.starting, principal.UserKey)
			w.mu.Unlock()
			close(done)
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		_, _ = w.ensure(ctx, principal, 2*time.Minute)
	}()
}

// restoreInBackground restores a new workspace without holding up a request,
// once per user at a time.
func (w *ahpWorkspaces) restoreInBackground(principal ahphost.Principal, pod *corev1.Pod) {
	w.mu.Lock()
	if w.restoring == nil {
		w.restoring = map[string]chan struct{}{}
	}
	if _, running := w.restoring[principal.UserKey]; running {
		w.mu.Unlock()
		return
	}
	done := make(chan struct{})
	w.restoring[principal.UserKey] = done
	w.mu.Unlock()
	go func() {
		defer func() {
			w.mu.Lock()
			delete(w.restoring, principal.UserKey)
			w.mu.Unlock()
			close(done)
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		w.restore(ctx, principal, pod)
	}()
}

// restore clones the user's projects and adds their sessions' worktrees in a
// new workspace Pod, then marks it restored. A restore that did not finish
// leaves the Pod unmarked, so a later use restores it again; what is already
// in place stays.
func (w *ahpWorkspaces) restore(ctx context.Context, principal ahphost.Principal, pod *corev1.Pod) {
	projects, err := w.b.projects(ctx, principal.UserKey)
	if err != nil {
		return
	}
	complete := true
	for _, project := range projects {
		if w.ensureProject(ctx, principal, pod, project) != nil {
			complete = false
		}
	}
	sessions, err := w.b.store.ListAHPRecords(ctx, principal.UserKey, ahpRecordSession)
	if err != nil {
		return
	}
	for _, raw := range sessions {
		var record ahpSession
		if json.Unmarshal(raw.Value, &record) != nil || record.Branch == "" {
			continue
		}
		for _, project := range projects {
			if project.ID == record.ProjectID && w.ensureWorktree(ctx, principal, pod, project, record) != nil {
				complete = false
			}
		}
	}
	if !complete {
		return
	}
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[ahpWorkspaceRestoredAnnotation] = "true"
	_ = w.b.h.client.Patch(ctx, pod, patch)
}

// prepare sets up the checkout a terminal opens in, a project or a session's
// worktree, after any restore in flight. It waits a bounded time, and an
// error means the shell would not open where it was asked to.
func (w *ahpWorkspaces) prepare(ctx context.Context, principal ahphost.Principal, pod *corev1.Pod, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, ahpWorkspaceSetupWait)
	defer cancel()
	w.mu.Lock()
	restoring := w.restoring[principal.UserKey]
	w.mu.Unlock()
	if restoring != nil {
		select {
		case <-restoring:
		case <-ctx.Done():
			return fmt.Errorf("the workspace is still being restored: %w", ctx.Err())
		}
	}
	projects, err := w.b.projects(ctx, principal.UserKey)
	if err != nil {
		return err
	}
	for _, project := range projects {
		if dir == project.Path {
			return w.ensureProject(ctx, principal, pod, project)
		}
	}
	sessions, err := w.b.store.ListAHPRecords(ctx, principal.UserKey, ahpRecordSession)
	if err != nil {
		return err
	}
	for _, raw := range sessions {
		var record ahpSession
		if json.Unmarshal(raw.Value, &record) != nil || record.Branch == "" || record.WorkDir != dir {
			continue
		}
		for _, project := range projects {
			if project.ID == record.ProjectID {
				return w.ensureWorktree(ctx, principal, pod, project, record)
			}
		}
	}
	if dir == ahpWorkspaceRoot {
		return nil
	}
	// Any other directory must already be there: the shell would otherwise
	// start somewhere the client was not told about.
	var exit interface{ ExitStatus() int }
	if err := w.run(ctx, pod, "test", "-d", dir); errors.As(err, &exit) && exit.ExitStatus() == 1 {
		return errAHPNoSuchDirectory
	} else if err != nil {
		return err
	}
	return nil
}

// errAHPNoSuchDirectory is a terminal directory the workspace does not have.
var errAHPNoSuchDirectory = errors.New("the directory does not exist in the workspace")

// withGit runs change under the user's git lock. The change runs to the end
// with its own deadline even when ctx ends first, so no clone or worktree is
// left half made; the caller stops waiting for it.
func (w *ahpWorkspaces) withGit(ctx context.Context, principal ahphost.Principal, change func(context.Context) error) error {
	w.mu.Lock()
	if w.git == nil {
		w.git = map[string]chan struct{}{}
	}
	lock, ok := w.git[principal.UserKey]
	if !ok {
		lock = make(chan struct{}, 1)
		w.git[principal.UserKey] = lock
	}
	w.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		gitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ahpWorkspaceGitTimeout)
		defer cancel()
		select {
		case lock <- struct{}{}:
		case <-gitCtx.Done():
			done <- gitCtx.Err()
			return
		}
		defer func() { <-lock }()
		done <- change(gitCtx)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run executes a program in the workspace without a terminal. A failure
// carries the program's output, bounded.
func (w *ahpWorkspaces) run(ctx context.Context, pod *corev1.Pod, argv ...string) error {
	return w.runWithin(ctx, ahpWorkspaceExecTimeout, pod, argv...)
}

func (w *ahpWorkspaces) runWithin(ctx context.Context, timeout time.Duration, pod *corev1.Pod, argv ...string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var output ahpBoundedBuffer
	if err := w.stream(ctx, pod, argv, nil, &output, false, nil); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(output.String()))
	}
	return nil
}

// stream executes argv in the workspace container. Without a terminal its
// standard error joins its standard output.
func (w *ahpWorkspaces) stream(ctx context.Context, pod *corev1.Pod, argv []string, stdin io.Reader, stdout io.Writer, tty bool, sizes remotecommand.TerminalSizeQueue) error {
	stderr := stdout
	if tty {
		stderr = nil
	}
	return w.exec(ctx, pod, argv, remotecommand.StreamOptions{Stdin: stdin, Stdout: stdout, Stderr: stderr, Tty: tty, TerminalSizeQueue: sizes})
}

// streamOutput executes argv in the workspace container, keeping only its
// standard output.
func (w *ahpWorkspaces) streamOutput(ctx context.Context, pod *corev1.Pod, argv []string, stdout io.Writer) error {
	return w.exec(ctx, pod, argv, remotecommand.StreamOptions{Stdout: stdout, Stderr: io.Discard})
}

func (w *ahpWorkspaces) exec(ctx context.Context, pod *corev1.Pod, argv []string, options remotecommand.StreamOptions) error {
	req := w.clientset.CoreV1().RESTClient().Post().Resource("pods").Namespace(pod.Namespace).Name(pod.Name).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: ahpWorkspaceContainer, Command: argv, Stdin: options.Stdin != nil, Stdout: true, Stderr: options.Stderr != nil, TTY: options.Tty,
		}, scheme.ParameterCodec)
	executor, err := remotecommand.NewWebSocketExecutor(w.rest, "GET", req.URL().String())
	if err != nil {
		return err
	}
	return executor.StreamWithContext(ctx, options)
}

// ensureProject clones a project at its path if it is not there yet.
func (w *ahpWorkspaces) ensureProject(ctx context.Context, principal ahphost.Principal, pod *corev1.Pod, project ahpProject) error {
	return w.withGit(ctx, principal, func(ctx context.Context) error { return w.clone(ctx, pod, project) })
}

// clone puts a project's checkout at its path only once it is complete, so
// an interrupted clone never looks like a checkout. Callers hold the git lock.
func (w *ahpWorkspaces) clone(ctx context.Context, pod *corev1.Pod, project ahpProject) error {
	return w.runWithin(ctx, ahpWorkspaceGitTimeout, pod, ahpShell, "-c", ahpCloneScript,
		"sh", project.Path, project.URL, ahpWorkspaceRoot+"/.clones")
}

// ahpCloneScript clones the repository $2 at the path $1 through a staging
// directory under $3. A checkout already at the path is used only when it is
// of that repository: a removed project leaves its checkout, and a repository
// of the same owner and name on another host has the same path. The origin is
// compared as stored, before any url.*.insteadOf rewriting.
const ahpCloneScript = `set -e
if [ -d "$1/.git" ]; then
  [ "$(git -C "$1" config --get remote.origin.url)" = "$2" ] && exit 0
  echo "$1 holds a checkout of another repository; remove it to use $2 there" >&2
  exit 1
fi
if [ -e "$1" ]; then echo "$1 is not a git checkout" >&2; exit 1; fi
mkdir -p "$(dirname "$1")" "$3"
staging=$(mktemp -d "$3/clone.XXXXXX")
trap 'rm -rf "$staging"' EXIT
git clone --quiet --filter=blob:none -- "$2" "$staging/checkout"
mv "$staging/checkout" "$1"`

// ahpWorkspaceCommands are the commands Orka's scripts run in a workspace,
// besides sh and git.
const ahpWorkspaceCommands = "sleep readlink cat dirname find grep head ls mkdir mktemp mv rm sed stat"

// ahpWorkspaceKeepAlive keeps a workspace container running for exec. Besides
// sh and git, it needs the commands Orka's scripts in the workspace run: a
// missing one could make files look missing, leave credentials uninstalled,
// or have the check for unpublished work find none. It stops with an error
// when one is missing, rather than run with the Pod looking ready.
const ahpWorkspaceKeepAlive = `for c in ` + ahpWorkspaceCommands + `; do
  command -v "$c" >/dev/null 2>&1 || { echo "the workspace image has no $c command; it needs sh, git and ` + ahpWorkspaceCommands + `" >&2; exit 1; }
done
trap 'exit 0' TERM
while :; do sleep 3600 & wait $!; done`

// ahpWorktreePath is a session's worktree: unique per project and branch, and
// a project's branch belongs to one session.
func ahpWorktreePath(project ahpProject, branch string) string {
	return path.Join(ahpWorktreeRoot, project.Owner, project.Name, branch)
}

// ensureWorktree adds a session's worktree on its branch: the published
// branch when it exists, then a local one an earlier session left, otherwise
// a new branch from the base.
func (w *ahpWorkspaces) ensureWorktree(ctx context.Context, principal ahphost.Principal, pod *corev1.Pod, project ahpProject, record ahpSession) error {
	return w.withGit(ctx, principal, func(ctx context.Context) error { return w.worktree(ctx, pod, project, record) })
}

// worktree adds a session's worktree. Callers hold the git lock.
func (w *ahpWorkspaces) worktree(ctx context.Context, pod *corev1.Pod, project ahpProject, record ahpSession) error {
	if err := w.clone(ctx, pod, project); err != nil {
		return err
	}
	base := record.BaseBranch
	if base == "" {
		base = project.DefaultBranch
	}
	return w.run(ctx, pod, ahpShell, "-c", ahpWorktreeScript, "sh", project.Path, ahpWorktreePath(project, record.Branch), record.Branch, base)
}

// ahpWorktreeScript adds, in the checkout $1, a worktree at $2 on the branch
// $3: the published branch, a local one a disposed session left unpublished,
// or a new one from the base branch $4.
const ahpWorktreeScript = `set -e
cd "$1"
[ -e "$2/.git" ] && exit 0
mkdir -p "$(dirname "$2")"
if git ls-remote --exit-code --heads origin "$3" >/dev/null 2>&1; then
  git fetch --quiet origin "+refs/heads/$3:refs/remotes/origin/$3"
  git worktree add --quiet -B "$3" "$2" "origin/$3"
  git -C "$2" branch --quiet --set-upstream-to="origin/$3" "$3"
elif git show-ref --verify --quiet "refs/heads/$3"; then
  git worktree add --quiet "$2" "$3"
elif [ -n "$4" ]; then
  git fetch --quiet origin "+refs/heads/$4:refs/remotes/origin/$4"
  git worktree add --quiet -b "$3" "$2" "origin/$4"
else
  git worktree add --quiet -b "$3" "$2"
fi`

// follow fast-forwards a session's worktree to what the last turn published.
// A branch with local commits or conflicting local changes is left alone.
func (w *ahpWorkspaces) follow(ctx context.Context, principal ahphost.Principal, record ahpSession) {
	if !w.enabled() || record.Branch == "" {
		return
	}
	var project ahpProject
	if err := w.b.getRecord(ctx, principal.UserKey, ahpRecordProject, record.ProjectID, &project); err != nil {
		return
	}
	pod, err := w.ensure(ctx, principal, time.Minute)
	if err != nil {
		return
	}
	var changed map[string]ahpGitChange
	err = w.withGit(ctx, principal, func(ctx context.Context) error {
		if err := w.worktree(ctx, pod, project, record); err != nil {
			return err
		}
		if err := w.run(ctx, pod, ahpShell, "-c", `set -e
cd "$1"
git fetch --quiet origin "+refs/heads/$2:refs/remotes/origin/$2"
git branch --quiet --set-upstream-to="origin/$2" "$2"
git merge --quiet --ff-only "origin/$2"`, "sh", ahpWorktreePath(project, record.Branch), record.Branch); err != nil {
			return err
		}
		base := record.BaseBranch
		if base == "" {
			base = project.DefaultBranch
		}
		var err error
		changed, err = w.branchChanges(ctx, pod, ahpWorktreePath(project, record.Branch), base)
		return err
	})
	if err != nil || changed == nil || w.b.emitterFor == nil {
		return
	}
	// What the branch now holds replaces what the turns reported.
	changes := &ahpChanges{b: w.b, ctx: ctx, principal: principal, session: record, emit: w.b.emitterFor(principal)}
	changes.sync(changed)
}

// dropWorktree removes a session's worktree from a running workspace when
// nothing in it exists only there: without --force, git keeps a worktree with
// changes or untracked files, and its branch stays in the checkout either way.
func (w *ahpWorkspaces) dropWorktree(ctx context.Context, principal ahphost.Principal, project ahpProject, branch string) {
	if !w.enabled() || branch == "" {
		return
	}
	pod, ok := w.running(ctx, principal)
	if !ok {
		return
	}
	_ = w.withGit(ctx, principal, func(ctx context.Context) error {
		return w.run(ctx, pod, ahpShell, "-c", ahpWorktreeRemoveScript, "sh", project.Path, ahpWorktreePath(project, branch))
	})
}

// ahpWorktreeRemoveScript removes, from the checkout $1, the worktree $2 when
// nothing in it exists only there: its HEAD reaches no commit that a branch,
// tag or remote-tracking branch does not, as a commit made on a detached
// HEAD; git finds no change or untracked file, whatever the repository's
// status settings; and no file is one git was told not to check. Any check
// that fails keeps the worktree.
const ahpWorktreeRemoveScript = `n=$(git -C "$2" rev-list --count HEAD --not --branches --tags --remotes) || exit 0
[ "$n" = 0 ] || exit 0
c=$(git -C "$2" status --porcelain --untracked-files=all --ignore-submodules=none) || exit 0
[ -z "$c" ] || exit 0
f=$(git -C "$2" ls-files -v) || exit 0
printf '%s\n' "$f" | grep -q -E '^([a-z]|S) ' && exit 0
git -C "$1" worktree remove "$2"`

// removeUnlessHoldsWork deletes the user's workspace Pod unless something in
// it may exist nowhere else, and reports whether it is gone. A Pod that
// cannot be checked, as while its container restarts or when it cannot be
// read, still has its files: it stays, for the idle collector to decide
// about. The Pod deleted is the one checked.
func (w *ahpWorkspaces) removeUnlessHoldsWork(ctx context.Context, principal ahphost.Principal) bool {
	if !w.enabled() {
		return true
	}
	pod := &corev1.Pod{}
	err := w.reader().Get(ctx, types.NamespacedName{Namespace: w.namespace, Name: ahpWorkspacePodName(principal.UserKey)}, pod)
	switch {
	case apierrors.IsNotFound(err):
		return true
	case err != nil:
		return false
	case pod.Labels[ahpWorkspaceUserLabel] != principal.UserKey[:40] || !pod.DeletionTimestamp.IsZero():
		// Another's, or going already: nothing of this user's to delete.
		return true
	}
	if pod.Status.Phase != corev1.PodRunning || !ahpContainerRunning(pod) || w.holdsWork(ctx, pod) {
		return false
	}
	return w.b.h.client.Delete(ctx, pod, client.Preconditions{UID: &pod.UID}) == nil
}

// running returns the user's workspace Pod if it is running; it never creates one.
func (w *ahpWorkspaces) running(ctx context.Context, principal ahphost.Principal) (*corev1.Pod, bool) {
	if !w.enabled() {
		return nil, false
	}
	pod := &corev1.Pod{}
	if err := w.reader().Get(ctx, types.NamespacedName{Namespace: w.namespace, Name: ahpWorkspacePodName(principal.UserKey)}, pod); err != nil {
		return nil, false
	}
	return pod, pod.Labels[ahpWorkspaceUserLabel] == principal.UserKey[:40] && pod.DeletionTimestamp.IsZero() &&
		pod.Status.Phase == corev1.PodRunning && ahpContainerRunning(pod)
}

// ahpBoundedBuffer keeps at most ahpExecOutputMax bytes of output.
type ahpBoundedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *ahpBoundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := ahpExecOutputMax - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *ahpBoundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
