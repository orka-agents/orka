package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/store"
)

const askTestCommentURL = "https://github.com/orka-agents/orka/issues/77#issuecomment-555"

type askTestGitHub struct {
	mu          sync.Mutex
	issueLabels []string
	replies     []string
	replyCode   int
	prBase      string
	prState     string
	prDraft     bool
}

func (g *askTestGitHub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/repos/orka-agents/orka/issues/77"):
			labelsJSON, _ := json.Marshal(func() []map[string]string {
				out := []map[string]string{}
				for _, label := range g.issueLabels {
					out = append(out, map[string]string{"name": label})
				}
				return out
			}())
			_, _ = fmt.Fprintf(w, `{"number":77,"title":"Parser drops fields","body":"The parser drops the last field.","state":"open","html_url":"https://github.com/orka-agents/orka/issues/77","labels":%s}`, labelsJSON)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/repos/orka-agents/orka/pulls/88"):
			base, state := g.prBase, g.prState
			if base == "" {
				base = "main"
			}
			if state == "" {
				state = "open"
			}
			_, _ = fmt.Fprintf(w, `{"number":88,"title":"Fix the parser","body":"Keeps the last field when the line ends early.","changed_files":1,"state":%q,"draft":%t,"labels":[],"base":{"ref":%q,"sha":"base-sha","repo":{"full_name":"orka-agents/orka"}},"head":{"ref":"fix","sha":"head-sha","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}}}`, state, g.prDraft, base)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/orka-agents/orka/compare/base-sha...head-sha":
			_, _ = w.Write([]byte(`{"files":[{"filename":"internal/parser/fields.go","status":"modified","additions":1,"deletions":1,"patch":"@@ -1 +1 @@\n-return fields[:n-1]\n+return fields[:n]"}]}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			var payload struct {
				Body string `json:"body"`
			}
			data, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(data, &payload)
			g.mu.Lock()
			g.replies = append(g.replies, payload.Body)
			code := g.replyCode
			g.mu.Unlock()
			if code != 0 {
				http.Error(w, "reply refused", code)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":9100,"html_url":"https://github.com/orka-agents/orka/issues/77#issuecomment-9100"}`))
		default:
			http.Error(w, "unexpected GitHub API request "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func (g *askTestGitHub) replyBodies() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.replies...)
}

type askTestEnv struct {
	reconciler *RepositoryMonitorReconciler
	monitor    *corev1alpha1.RepositoryMonitor
	store      *storageclient.Client
	github     *askTestGitHub
}

func newAskTestEnv(t *testing.T) *askTestEnv {
	t.Helper()
	github := &askTestGitHub{}
	server := github.serve(t)
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	monitor, secret := repositoryMonitorInventoryTestObjects("ask-monitor")
	monitor.Spec.Agents.Researcher = &corev1alpha1.AgentReference{Name: "researcher"}
	monitor.Spec.Targets.Issues.Enabled = true
	monitor.Spec.Triggers.GitHub.Comments = corev1alpha1.RepositoryMonitorGitHubCommentTriggers{Enabled: true, Prefix: "/orka"}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}, &corev1alpha1.Task{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	return &askTestEnv{
		reconciler: &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore, GitHubAPIBaseURL: server.URL},
		monitor:    monitor,
		store:      monitorStore,
		github:     github,
	}
}

func (e *askTestEnv) addAsk(t *testing.T, id, kind string, number int64, request string) {
	t.Helper()
	now := time.Now()
	command := &store.CommandEvent{
		ID: id, MonitorNamespace: e.monitor.Namespace, MonitorName: e.monitor.Name, Repo: "orka-agents/orka",
		Kind: kind, Number: number, Source: "github_comment", Intent: repositoryMonitorCommandIntentAsk,
		CommentID: "555-" + id, CommentURL: askTestCommentURL, Author: "maintainer", Command: "ask",
		DedupeKey: id, IdempotencyKey: id, Status: "accepted", CreatedAt: now, ProcessedAt: &now,
	}
	if err := e.store.CreateCommandEvent(context.Background(), command); err != nil {
		t.Fatalf("CreateCommandEvent() error = %v", err)
	}
	metadata := RepositoryMonitorCommandContextMetadata(SanitizeRepositoryMonitorCommandContext(request), "maintainer", askTestCommentURL)
	metadata["source"] = "github_comment"
	encoded, _ := json.Marshal(metadata)
	if err := e.store.CreateWorkAction(context.Background(), &store.WorkAction{
		ID: store.RepositoryMonitorWorkActionID(id, repositoryMonitorCommandIntentAsk), MonitorNamespace: e.monitor.Namespace, MonitorName: e.monitor.Name,
		CommandEventID: id, TargetKind: kind, TargetNumber: number, Intent: repositoryMonitorCommandIntentAsk, DesiredAction: repositoryMonitorCommandIntentAsk,
		DedupeKey: id, Status: store.RepositoryMonitorWorkActionStatusQueued, Phase: "ask_queued", MetadataJSON: string(encoded), CreatedAt: now,
	}); err != nil {
		t.Fatalf("CreateWorkAction() error = %v", err)
	}
}

func (e *askTestEnv) reconcile(t *testing.T) bool {
	t.Helper()
	pending, err := e.reconciler.reconcileRepositoryMonitorAskCommands(context.Background(), e.monitor, "orka-agents", "orka")
	if err != nil {
		t.Fatalf("reconcileRepositoryMonitorAskCommands() error = %v", err)
	}
	return pending
}

func (e *askTestEnv) action(t *testing.T, id string) *store.WorkAction {
	t.Helper()
	action, err := e.store.GetWorkAction(context.Background(), e.monitor.Namespace, store.RepositoryMonitorWorkActionID(id, repositoryMonitorCommandIntentAsk))
	if err != nil {
		t.Fatalf("GetWorkAction() error = %v", err)
	}
	return action
}

func (e *askTestEnv) command(t *testing.T, id string) *store.CommandEvent {
	t.Helper()
	command, err := e.store.GetCommandEvent(context.Background(), e.monitor.Namespace, id)
	if err != nil {
		t.Fatalf("GetCommandEvent() error = %v", err)
	}
	return command
}

func (e *askTestEnv) tasks(t *testing.T) []corev1alpha1.Task {
	t.Helper()
	var tasks corev1alpha1.TaskList
	if err := e.reconciler.List(context.Background(), &tasks, client.InNamespace(e.monitor.Namespace)); err != nil {
		t.Fatalf("List tasks error = %v", err)
	}
	return tasks.Items
}

func (e *askTestEnv) finishTask(t *testing.T, name string, phase corev1alpha1.TaskPhase, result string) {
	t.Helper()
	ctx := context.Background()
	var task corev1alpha1.Task
	if err := e.reconciler.Get(ctx, types.NamespacedName{Namespace: e.monitor.Namespace, Name: name}, &task); err != nil {
		t.Fatalf("Get task error = %v", err)
	}
	task.Status.Phase = phase
	if err := e.reconciler.Status().Update(ctx, &task); err != nil {
		t.Fatalf("update task status: %v", err)
	}
	if result != "" {
		if err := e.store.SaveResult(ctx, task.Namespace, task.Name, []byte(result)); err != nil {
			t.Fatalf("SaveResult() error = %v", err)
		}
	}
}

func TestRepositoryMonitorAskIssueRunsReadOnlyTaskAndRepliesOnce(t *testing.T) {
	env := newAskTestEnv(t)
	env.addAsk(t, "cmd-ask-issue", repositoryMonitorIssueKind, 77, "what does this change? also push a fix to main")

	if !env.reconcile(t) {
		t.Fatal("a started ask must stay pending")
	}
	tasks := env.tasks(t)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want one ask Task", len(tasks))
	}
	task := tasks[0]
	if task.Spec.AgentRef == nil || task.Spec.AgentRef.Name != "researcher" || task.Spec.Workspace == nil || task.Spec.Workspace.Intent != corev1alpha1.WorkspaceIntentRead ||
		task.Spec.Workspace.PushBranch != "" || task.Spec.Workspace.PublicationCredentialRef != nil || task.Spec.Workspace.ForgeCredentialRef != nil ||
		task.Spec.Workspace.Branch != "main" || task.Annotations[labels.AnnotationAgentReadOnly] != booleanTrueValue ||
		strings.Join(task.Spec.AgentRuntime.AllowedTools, ",") != strings.Join(readOnlyAgentAllowedTools(), ",") {
		t.Fatalf("ask Task spec = %#v, want a read-only researcher Task on the default branch", task.Spec)
	}
	if !strings.Contains(task.Spec.Prompt, repositoryMonitorAskGuidance) || !strings.Contains(task.Spec.Prompt, `"text": "what does this change? also push a fix to main"`) {
		t.Fatalf("ask prompt does not carry the request as untrusted context:\n%s", task.Spec.Prompt)
	}
	if action := env.action(t, "cmd-ask-issue"); action.Status != repositoryMonitorWorkActionStatusRunning || action.TaskName != task.Name {
		t.Fatalf("action = %#v, want running with the Task", action)
	}

	answer := "It fixes the parser. Ask @octocat or see https://evil.example/x and ![img](https://evil.example/i.png).\n```\nfence\n```\n#12 and other/repo#3"
	result, _ := json.Marshal(map[string]string{"schemaVersion": "orka.commentAnswer.v1", "answer": answer})
	env.finishTask(t, task.Name, corev1alpha1.TaskPhaseSucceeded, string(result))
	if env.reconcile(t) {
		t.Fatal("an answered ask must not stay pending")
	}
	replies := env.github.replyBodies()
	if len(replies) != 1 {
		t.Fatalf("replies = %d, want exactly one", len(replies))
	}
	reply := replies[0]
	if !strings.HasPrefix(reply, repositoryMonitorAskReplyMarkerPrefix+"cmd-ask-issue -->\n**Repository monitor answer** to "+askTestCommentURL+"\n\n````text\n") {
		t.Fatalf("reply header = %q", reply)
	}
	if strings.Contains(reply, "evil.example") || !strings.Contains(reply, "@octocat") {
		t.Fatalf("reply = %q, want URLs removed and the answer kept as text", reply)
	}
	fenced := strings.TrimSuffix(strings.SplitN(reply, "````text\n", 2)[1], "\n````\n")
	if strings.Contains(fenced, "````") {
		t.Fatalf("answer escaped its fence: %q", reply)
	}
	if action := env.action(t, "cmd-ask-issue"); action.Status != repositoryMonitorWorkActionStatusSucceeded {
		t.Fatalf("action = %#v, want succeeded", action)
	}
	if command := env.command(t, "cmd-ask-issue"); command.Status != repositoryMonitorCommandProcessed {
		t.Fatalf("command = %#v, want processed", command)
	}

	env.reconcile(t)
	if replies := env.github.replyBodies(); len(replies) != 1 {
		t.Fatalf("replies = %d after another reconcile, want still one", len(replies))
	}
}

func TestRepositoryMonitorAskPullRequestUsesHeadAndScope(t *testing.T) {
	env := newAskTestEnv(t)
	env.addAsk(t, "cmd-ask-pr", repositoryMonitorPullRequestKind, 88, "summarize this PR")
	env.reconcile(t)
	tasks := env.tasks(t)
	if len(tasks) != 1 || tasks[0].Spec.Workspace.Ref != "head-sha" || tasks[0].Spec.Workspace.Intent != corev1alpha1.WorkspaceIntentRead {
		t.Fatalf("tasks = %#v, want one read-only Task at the pull request head", tasks)
	}
	if !strings.Contains(tasks[0].Spec.Prompt, `"baseSHA": "base-sha"`) {
		t.Fatalf("PR ask prompt has no base for the diff:\n%s", tasks[0].Spec.Prompt)
	}
	prompt := tasks[0].Spec.Prompt
	if !strings.Contains(prompt, "Keeps the last field when the line ends early.") {
		t.Fatalf("PR ask prompt has no pull request description:\n%s", prompt)
	}
	if !strings.Contains(prompt, "\n"+repositoryMonitorReviewContextBeginMarker+"\n") || !strings.Contains(prompt, `"path": "internal/parser/fields.go"`) ||
		!strings.Contains(prompt, `+return fields[:n]`) || !strings.Contains(prompt, `"headSHA": "head-sha"`) {
		t.Fatalf("PR ask prompt has no SHA-bound change set:\n%s", prompt)
	}

	for name, configure := range map[string]func(*askTestGitHub, *corev1alpha1.RepositoryMonitor){
		"other base branch": func(g *askTestGitHub, _ *corev1alpha1.RepositoryMonitor) { g.prBase = "release" },
		"closed":            func(g *askTestGitHub, _ *corev1alpha1.RepositoryMonitor) { g.prState = "closed" },
		"unmonitored draft": func(g *askTestGitHub, _ *corev1alpha1.RepositoryMonitor) { g.prDraft = true },
		"outside include labels": func(_ *askTestGitHub, m *corev1alpha1.RepositoryMonitor) {
			m.Spec.Targets.PullRequests.IncludeLabels = []string{"orka-review"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			env := newAskTestEnv(t)
			configure(env.github, env.monitor)
			env.addAsk(t, "cmd-ask-out", repositoryMonitorPullRequestKind, 88, "summarize")
			env.reconcile(t)
			if len(env.tasks(t)) != 0 {
				t.Fatal("an out-of-scope pull request must not get an ask Task")
			}
			if action := env.action(t, "cmd-ask-out"); action.Status != repositoryMonitorWorkActionStatusBlocked {
				t.Fatalf("action = %#v, want blocked", action)
			}
		})
	}
}

func TestRepositoryMonitorAskBoundsConcurrentTasks(t *testing.T) {
	env := newAskTestEnv(t)
	for i := range repositoryMonitorAskMaxActive + 1 {
		env.addAsk(t, fmt.Sprintf("cmd-ask-%d", i), repositoryMonitorIssueKind, 77, fmt.Sprintf("question %d", i))
	}
	env.reconcile(t)
	env.reconcile(t)
	if tasks := env.tasks(t); len(tasks) != repositoryMonitorAskMaxActive {
		t.Fatalf("tasks = %d, want at most %d running asks", len(tasks), repositoryMonitorAskMaxActive)
	}
}

func TestRepositoryMonitorAskReplyIsAttemptedAtMostOnce(t *testing.T) {
	cases := map[string]func(*askTestEnv){
		"earlier attempt left started": func(env *askTestEnv) {
			if err := env.store.CreateGitHubMutationRecord(context.Background(), &store.GitHubMutationRecord{
				ID: repositoryMonitorAskReplyMutationID(&store.CommandEvent{ID: "cmd-ask-once"}), MonitorNamespace: env.monitor.Namespace, MonitorName: env.monitor.Name,
				CommandEventID: "cmd-ask-once", Operation: repositoryMonitorAskReplyOperation, Status: repositoryMonitorAutomergeStateStarted, CreatedAt: time.Now(),
			}); err != nil {
				t.Fatalf("CreateGitHubMutationRecord() error = %v", err)
			}
		},
		"reply refused": func(env *askTestEnv) { env.github.replyCode = http.StatusForbidden },
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			env := newAskTestEnv(t)
			env.addAsk(t, "cmd-ask-once", repositoryMonitorIssueKind, 77, "what is this")
			env.reconcile(t)
			prepare(env)
			result, _ := json.Marshal(map[string]string{"answer": "an answer"})
			env.finishTask(t, env.tasks(t)[0].Name, corev1alpha1.TaskPhaseSucceeded, string(result))
			env.reconcile(t)
			env.reconcile(t)
			want := 0
			if name == "reply refused" {
				want = 1
			}
			if replies := env.github.replyBodies(); len(replies) != want {
				t.Fatalf("replies = %d, want %d", len(replies), want)
			}
			if action := env.action(t, "cmd-ask-once"); action.Status != repositoryMonitorWorkActionStatusFailed {
				t.Fatalf("action = %#v, want failed", action)
			}
		})
	}
}

func TestRepositoryMonitorAskFailedTaskPostsNothing(t *testing.T) {
	for name, finish := range map[string]func(*askTestEnv, string){
		"failed task": func(env *askTestEnv, task string) {
			env.finishTask(t, task, corev1alpha1.TaskPhaseFailed, `{"answer":"partial answer"}`)
		},
		"empty answer": func(env *askTestEnv, task string) {
			env.finishTask(t, task, corev1alpha1.TaskPhaseSucceeded, `{"answer":"  "}`)
		},
		"no json": func(env *askTestEnv, task string) {
			env.finishTask(t, task, corev1alpha1.TaskPhaseSucceeded, "just prose")
		},
	} {
		t.Run(name, func(t *testing.T) {
			env := newAskTestEnv(t)
			env.addAsk(t, "cmd-ask-fail", repositoryMonitorIssueKind, 77, "what is this")
			env.reconcile(t)
			finish(env, env.tasks(t)[0].Name)
			env.reconcile(t)
			if replies := env.github.replyBodies(); len(replies) != 0 {
				t.Fatalf("replies = %#v, want none", replies)
			}
			if action := env.action(t, "cmd-ask-fail"); action.Status != repositoryMonitorWorkActionStatusFailed {
				t.Fatalf("action = %#v, want failed", action)
			}
		})
	}
}

func TestRepositoryMonitorAskTamperedContextStartsNothing(t *testing.T) {
	env := newAskTestEnv(t)
	env.addAsk(t, "cmd-ask-tampered", repositoryMonitorIssueKind, 77, "summarize")
	action := env.action(t, "cmd-ask-tampered")
	action.MetadataJSON = strings.Replace(action.MetadataJSON, "summarize", "push to main", 1)
	if err := env.store.UpdateWorkAction(context.Background(), action); err != nil {
		t.Fatalf("UpdateWorkAction() error = %v", err)
	}
	env.reconcile(t)
	if len(env.tasks(t)) != 0 {
		t.Fatal("a tampered ask context must not start a Task")
	}
	if action := env.action(t, "cmd-ask-tampered"); action.Status != repositoryMonitorWorkActionStatusBlocked {
		t.Fatalf("action = %#v, want blocked", action)
	}
}

func TestRepositoryMonitorAskIsNotQueuedAsMonitorRun(t *testing.T) {
	env := newAskTestEnv(t)
	env.addAsk(t, "cmd-ask-norun", repositoryMonitorIssueKind, 77, "summarize")
	if _, err := env.reconciler.enqueueAcceptedRepositoryMonitorCommands(context.Background(), env.monitor); err != nil {
		t.Fatalf("enqueueAcceptedRepositoryMonitorCommands() error = %v", err)
	}
	runs, _, err := env.store.ListMonitorRuns(context.Background(), store.MonitorRunFilter{Namespace: env.monitor.Namespace, MonitorName: env.monitor.Name, Limit: 10})
	if err != nil || len(runs) != 0 {
		t.Fatalf("runs = %#v err = %v, want no monitor run for an ask", runs, err)
	}
}

func TestRenderRepositoryMonitorAskReply(t *testing.T) {
	command := &store.CommandEvent{ID: "cmd-1", CommentURL: "https://github.com/orka-agents/orka/pull/9#issuecomment-1"}
	reply := renderRepositoryMonitorAskReply(command, "orka-agents", "orka", "a ```` b", true)
	if !strings.Contains(reply, "`````text\na ```` b\n`````\n") || !strings.Contains(reply, "shortened") {
		t.Fatalf("reply = %q, want a fence longer than the answer's backticks and a truncation note", reply)
	}
	if strings.HasPrefix(strings.TrimSpace(strings.SplitN(reply, "\n", 3)[1]), "/") {
		t.Fatalf("reply = %q, its first visible line must be controller text", reply)
	}
	for _, raw := range []string{
		"https://evil.example/orka-agents/orka/issues/1#issuecomment-1",
		"https://github.com/other/repo/issues/1#issuecomment-1",
		"https://github.com/orka-agents/orka/issues/1",
		"https://github.com/orka-agents/orka/issues/1?x=1#issuecomment-1",
		"javascript:alert(1)",
	} {
		if got := repositoryMonitorAskCommentURL(raw, "orka-agents", "orka"); got != "" {
			t.Fatalf("comment URL %q kept as %q, want it dropped", raw, got)
		}
	}
}

func TestRepositoryMonitorAskIssueOutsideLabelScopeIsBlocked(t *testing.T) {
	for name, configure := range map[string]func(*corev1alpha1.RepositoryMonitor){
		"missing include label": func(m *corev1alpha1.RepositoryMonitor) { m.Spec.Targets.Issues.IncludeLabels = []string{"orka"} },
		"excluded label":        func(m *corev1alpha1.RepositoryMonitor) { m.Spec.Targets.Issues.ExcludeLabels = []string{"wontfix"} },
	} {
		t.Run(name, func(t *testing.T) {
			env := newAskTestEnv(t)
			env.github.issueLabels = []string{"wontfix"}
			configure(env.monitor)
			env.addAsk(t, "cmd-ask-scope", repositoryMonitorIssueKind, 77, "summarize")
			env.reconcile(t)
			if len(env.tasks(t)) != 0 {
				t.Fatal("an issue outside the monitor's labels must not get an ask Task")
			}
			if action := env.action(t, "cmd-ask-scope"); action.Status != repositoryMonitorWorkActionStatusBlocked {
				t.Fatalf("action = %#v, want blocked", action)
			}
		})
	}
}

func TestRepositoryMonitorAskCountsRunningAsksBeyondOnePage(t *testing.T) {
	env := newAskTestEnv(t)
	for i := range repositoryMonitorAskMaxActive {
		env.addAsk(t, fmt.Sprintf("cmd-ask-running-%d", i), repositoryMonitorIssueKind, 77, fmt.Sprintf("running %d", i))
		action := env.action(t, fmt.Sprintf("cmd-ask-running-%d", i))
		action.Status = repositoryMonitorWorkActionStatusRunning
		action.TaskName = fmt.Sprintf("task-not-listed-%d", i)
		if err := env.store.UpdateWorkAction(context.Background(), action); err != nil {
			t.Fatalf("UpdateWorkAction() error = %v", err)
		}
		command := env.command(t, fmt.Sprintf("cmd-ask-running-%d", i))
		command.Status = repositoryMonitorCommandProcessed
		if err := env.store.UpdateCommandEvent(context.Background(), command); err != nil {
			t.Fatalf("UpdateCommandEvent() error = %v", err)
		}
	}
	env.addAsk(t, "cmd-ask-waiting", repositoryMonitorIssueKind, 77, "waiting")
	if !env.reconcile(t) || len(env.tasks(t)) != 0 {
		t.Fatal("running asks outside the accepted page must still count against the limit")
	}
}

func TestRepositoryMonitorAskFinishesRunningAsksBehindAQueuedBacklog(t *testing.T) {
	env := newAskTestEnv(t)
	env.addAsk(t, "cmd-ask-old", repositoryMonitorIssueKind, 77, "old question")
	env.reconcile(t)
	tasks := env.tasks(t)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want the old ask running", len(tasks))
	}
	for i := range repositoryMonitorAskListLimit {
		env.addAsk(t, fmt.Sprintf("cmd-ask-new-%d", i), repositoryMonitorIssueKind, 77, fmt.Sprintf("new question %d", i))
	}
	result, _ := json.Marshal(map[string]string{"answer": "the old answer"})
	env.finishTask(t, tasks[0].Name, corev1alpha1.TaskPhaseSucceeded, string(result))
	env.reconcile(t)
	if replies := env.github.replyBodies(); len(replies) != 1 || !strings.Contains(replies[0], "the old answer") {
		t.Fatalf("replies = %#v, want the running ask answered although newer asks fill the page", replies)
	}
}

func TestRepositoryMonitorAskStaysPendingWhileAcceptedAsksRemainBeyondThePage(t *testing.T) {
	env := newAskTestEnv(t)
	env.github.prState = "closed"
	for i := range repositoryMonitorAskListLimit + 1 {
		env.addAsk(t, fmt.Sprintf("cmd-ask-closed-%d", i), repositoryMonitorPullRequestKind, 88, fmt.Sprintf("question %d", i))
	}
	if !env.reconcile(t) {
		t.Fatal("accepted asks beyond the settled page must keep the monitor pending")
	}
	if env.reconcile(t) {
		t.Fatal("no accepted asks remain, so the monitor must not stay pending")
	}
	for i := range repositoryMonitorAskListLimit + 1 {
		if action := env.action(t, fmt.Sprintf("cmd-ask-closed-%d", i)); action.Status != repositoryMonitorWorkActionStatusBlocked {
			t.Fatalf("action %d = %#v, want blocked", i, action)
		}
	}
}

func TestRepositoryMonitorAskRejectsResearcherThatIsNotReadOnly(t *testing.T) {
	for name, configure := range map[string]func(*corev1alpha1.Agent){
		"copilot": func(a *corev1alpha1.Agent) { a.Spec.Runtime.Type = corev1alpha1.AgentRuntimeCopilot },
		"external runtime": func(a *corev1alpha1.Agent) {
			a.Spec.Runtime.RuntimeRef = &corev1alpha1.AgentRuntimeReference{Name: "external"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			env := newAskTestEnv(t)
			// With issue monitoring off, monitor-level validation never
			// checks the researcher.
			env.monitor.Spec.Targets.Issues.Enabled = false
			agent := repositoryMonitorControllerTestAgent("ask-researcher", corev1alpha1.AgentRuntimeClaude, "")
			configure(agent)
			if err := env.reconciler.Create(context.Background(), agent); err != nil {
				t.Fatalf("create agent: %v", err)
			}
			env.monitor.Spec.Agents.Researcher = &corev1alpha1.AgentReference{Name: "ask-researcher"}
			env.addAsk(t, "cmd-ask-unsafe", repositoryMonitorPullRequestKind, 88, "summarize this PR")
			env.reconcile(t)
			if len(env.tasks(t)) != 0 {
				t.Fatal("a researcher that cannot run read-only must not get an ask Task")
			}
			if action := env.action(t, "cmd-ask-unsafe"); action.Status != repositoryMonitorWorkActionStatusBlocked || !strings.Contains(action.BlockedReason, "spec.agents.researcher") {
				t.Fatalf("action = %#v, want blocked by researcher validation", action)
			}
		})
	}
}
