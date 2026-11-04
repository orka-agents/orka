package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/evals"
	executionevents "github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

const (
	evalTestNamespace = "team-a"
	evalTestBaseSHA   = "1111111111111111111111111111111111111111"
	evalTestHeadSHA   = "2222222222222222222222222222222222222222"
	evalTestFixture   = "https://github.com/orka-agents/orka-eval-fixtures"
	evalTestImage     = "ghcr.io/orka-agents/claude@sha256:1234567890123456789012345678901234567890123456789012345678901234"
)

var evalTestNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

type evalRunTestEnv struct {
	t *testing.T
	// staleTaskCache makes cached Task listings miss every Task.
	staleTaskCache bool
	client         client.Client
	store          *sqlite.Store
	reconciler     *EvalRunReconciler
	now            time.Time
}

func evalTestCaseLine(id, verdict string, tags ...string) string {
	c := map[string]any{
		"schemaVersion": corev1alpha1.EvalCaseSchemaVersion,
		"id":            id,
		"tags":          tags,
		"input": map[string]any{"repositoryMonitorReview": map[string]any{
			"repoURL": evalTestFixture,
			"baseSHA": evalTestBaseSHA,
			"headSHA": evalTestHeadSHA,
			"title":   "Change " + id,
			"files": []any{map[string]any{
				"filename": "pkg/x.go", "status": "modified",
				"patch": "@@ -38,3 +38,4 @@\n-\treturn nil\n+\treturn cfg.Value\n", "additions": 1, "deletions": 1,
			}},
		}},
		"reference": map[string]any{"verdict": verdict},
	}
	if verdict == evals.ReferenceVerdictNotPassed {
		c["reference"] = map[string]any{
			"verdict":   verdict,
			"locations": []any{map[string]any{"file": "pkg/x.go", "startLine": 38, "endLine": 40}},
		}
	}
	encoded, err := json.Marshal(c)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func evalTestCases() string {
	return evalTestCaseLine("defect-1", evals.ReferenceVerdictNotPassed, "defect") + "\n" +
		evalTestCaseLine("clean-1", evals.ReferenceVerdictPassed, "clean") + "\n"
}

func evalTestRun(trials, maxConcurrent int32) *corev1alpha1.EvalRun {
	maxInfra := int32(50)
	return &corev1alpha1.EvalRun{
		ObjectMeta: metav1.ObjectMeta{Name: "pr-review", Namespace: evalTestNamespace, UID: "run-uid", Generation: 1},
		Spec: corev1alpha1.EvalRunSpec{
			Subject: corev1alpha1.EvalSubject{
				Type:                    corev1alpha1.EvalSubjectRepositoryMonitorReview,
				RepositoryMonitorReview: &corev1alpha1.EvalRepositoryMonitorReviewSubject{MonitorRef: corev1alpha1.EvalObjectReference{Name: "monitor"}},
			},
			Cases:                corev1alpha1.EvalCaseSource{ConfigMapRef: &corev1alpha1.EvalConfigMapKeyRef{Name: "cases", Key: "cases.jsonl"}},
			Trials:               trials,
			MaxConcurrentTrials:  maxConcurrent,
			TrialTimeout:         &metav1.Duration{Duration: 20 * time.Minute},
			Graders:              []corev1alpha1.EvalGraderName{corev1alpha1.EvalGraderVerdict, corev1alpha1.EvalGraderContract},
			MaxInfraErrorPercent: &maxInfra,
		},
	}
}

func evalTestObjects(run *corev1alpha1.EvalRun, cases string) []client.Object {
	return []client.Object{
		run,
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "cases", Namespace: evalTestNamespace},
			Data:       map[string]string{"cases.jsonl": cases},
		},
		&corev1alpha1.RepositoryMonitor{
			ObjectMeta: metav1.ObjectMeta{Name: "monitor", Namespace: evalTestNamespace, UID: "monitor-uid", Generation: 3},
			Spec: corev1alpha1.RepositoryMonitorSpec{
				RepoURL: "https://github.com/orka-agents/orka",
				Agents:  corev1alpha1.RepositoryMonitorAgents{Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"}},
			},
		},
		&corev1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: evalTestNamespace, UID: "agent-uid", Generation: 2},
			Spec: corev1alpha1.AgentSpec{
				Model:   &corev1alpha1.ModelConfig{Provider: "anthropic", Name: "claude-haiku"},
				Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeClaude},
			},
		},
	}
}

func newEvalRunTestEnv(t *testing.T, objects ...client.Object) *evalRunTestEnv {
	t.Helper()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sqliteStore := sqlite.NewStore(db, "test")
	scheme := newTestScheme()
	env := &evalRunTestEnv{t: t, store: sqliteStore, now: evalTestNow}
	kube := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.EvalRun{}, &corev1alpha1.Task{}).
		WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			// The API server sets creationTimestamp; trial deadlines count from it.
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				obj.SetCreationTimestamp(metav1.NewTime(env.now))
				return c.Create(ctx, obj, opts...)
			},
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, tasks := list.(*corev1alpha1.TaskList); tasks && env.staleTaskCache {
					return nil
				}
				return c.List(ctx, list, opts...)
			},
		}).
		Build()
	env.client = kube
	env.reconciler = &EvalRunReconciler{
		Client:        kube,
		Scheme:        scheme,
		APIReader:     kube,
		TrialStore:    sqliteStore,
		ResultStore:   sqliteStore,
		EventStore:    sqliteStore,
		RuntimeImages: ACPRuntimeImages{Claude: evalTestImage},
		Now:           func() time.Time { return env.now },
	}
	return env
}

func (e *evalRunTestEnv) reconcile() ctrl.Result {
	e.t.Helper()
	result, err := e.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: evalTestNamespace, Name: "pr-review"}})
	if err != nil {
		e.t.Fatalf("Reconcile() error = %v", err)
	}
	return result
}

func (e *evalRunTestEnv) run() *corev1alpha1.EvalRun {
	e.t.Helper()
	run := &corev1alpha1.EvalRun{}
	if err := e.client.Get(context.Background(), types.NamespacedName{Namespace: evalTestNamespace, Name: "pr-review"}, run); err != nil {
		e.t.Fatalf("get EvalRun: %v", err)
	}
	return run
}

func (e *evalRunTestEnv) trialTasks() []corev1alpha1.Task {
	e.t.Helper()
	var list corev1alpha1.TaskList
	if err := e.client.List(context.Background(), &list, client.InNamespace(evalTestNamespace),
		client.MatchingLabels{labels.LabelEvalRun: "pr-review"}); err != nil {
		e.t.Fatal(err)
	}
	return list.Items
}

// start reconciles a new run until its snapshot is frozen.
func (e *evalRunTestEnv) start() {
	e.t.Helper()
	e.reconcile() // finalizer
	e.reconcile() // snapshot
	if run := e.run(); run.Status.Phase != corev1alpha1.EvalRunPhaseRunning {
		e.t.Fatalf("phase = %q, conditions %+v; want Running", run.Status.Phase, run.Status.Conditions)
	}
}

// finishTask completes a trial Task with a review result, or fails it.
func (e *evalRunTestEnv) finishTask(task *corev1alpha1.Task, phase corev1alpha1.TaskPhase, result []byte, modelOutput bool) {
	e.t.Helper()
	ctx := context.Background()
	if modelOutput {
		e.appendEvent(task, executionevents.ExecutionEventTypeModelMessage, map[string]any{"text": "reviewing"})
		e.appendEvent(task, executionevents.ExecutionEventTypeModelUsageUpdated, map[string]any{"inputTokens": 120, "outputTokens": 30})
	}
	if result != nil {
		if err := e.store.SaveResult(ctx, task.Namespace, task.Name, result); err != nil {
			e.t.Fatal(err)
		}
		task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
	}
	completed := metav1.NewTime(e.now)
	task.Status.Phase = phase
	task.Status.CompletionTime = &completed
	if err := e.client.Status().Update(ctx, task); err != nil {
		e.t.Fatal(err)
	}
}

func (e *evalRunTestEnv) appendEvent(task *corev1alpha1.Task, eventType string, content map[string]any) {
	e.t.Helper()
	encoded, err := json.Marshal(content)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.store.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace:  task.Namespace,
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   task.Name,
		TaskName:   task.Name,
		Type:       eventType,
		Severity:   executionevents.ExecutionEventSeverityInfo,
		Content:    encoded,
	}); err != nil {
		e.t.Fatal(err)
	}
}

// evalTestReviewResult is a review result; needs_changes reports a finding.
func evalTestReviewResult(verdict string) []byte {
	findings := `[]`
	if verdict != repositoryMonitorReviewVerdictPassed {
		findings = `[{"severity":"high","file":"pkg/x.go","line":39,"title":"nil","body":"nil deref"}]`
	}
	return fmt.Appendf(nil, `{"schemaVersion":"orka.prReview.v1","repo":"orka-agents/orka-eval-fixtures","prNumber":1,"headSHA":%q,"verdict":%q,"confidence":"high","summary":"ok","findings":%s,"security":{"status":"clear"},"tests":{"status":"not_run"}}`, evalTestHeadSHA, verdict, findings)
}

func evalCompletedCondition(run *corev1alpha1.EvalRun) *metav1.Condition {
	return meta.FindStatusCondition(run.Status.Conditions, evalRunConditionCompleted)
}

func TestEvalRunRunsGradesAndAggregatesTrials(t *testing.T) {
	run := evalTestRun(2, 4)
	env := newEvalRunTestEnv(t, evalTestObjects(run, evalTestCases())...)
	env.start()

	frozen := env.run().Status.Snapshot
	assertEvalTestSnapshot(t, frozen)
	if got := env.run().Status.Counts.Planned; got != 4 {
		t.Fatalf("planned = %d, want 4", got)
	}

	env.reconcile()
	tasks := env.trialTasks()
	if len(tasks) != 4 {
		t.Fatalf("created %d trial Tasks, want 4", len(tasks))
	}
	byKey := map[string]*corev1alpha1.Task{}
	for i := range tasks {
		task := &tasks[i]
		assertEvalTrialTaskSafe(t, task)
		byKey[task.Labels[labels.LabelEvalCase]+"/"+task.Labels[labels.LabelEvalTrial]] = task
	}

	// defect-1: one trial holds the PR back, one passes it. clean-1: one
	// trial passes, the other loses its provider before any output.
	env.finishTask(byKey["defect-1/1"], corev1alpha1.TaskPhaseSucceeded, evalTestReviewResult("needs_changes"), true)
	env.finishTask(byKey["defect-1/2"], corev1alpha1.TaskPhaseSucceeded, evalTestReviewResult("passed"), true)
	env.finishTask(byKey["clean-1/1"], corev1alpha1.TaskPhaseSucceeded, evalTestReviewResult("passed"), true)
	env.appendEvent(byKey["clean-1/2"], executionevents.ExecutionEventTypeModelRequestFailed,
		map[string]any{"code": "provider_upstream_error", "message": "provider upstream returned HTTP 502 for the final inference request"})
	env.finishTask(byKey["clean-1/2"], corev1alpha1.TaskPhaseFailed, nil, false)

	env.reconcile()
	got := env.run()
	if got.Status.Phase != corev1alpha1.EvalRunPhaseSucceeded {
		t.Fatalf("phase = %q, condition %+v", got.Status.Phase, evalCompletedCondition(got))
	}
	wantCounts := corev1alpha1.EvalRunCounts{Planned: 4, Completed: 4, Passed: 2, Failed: 1, InfraError: 1}
	if got.Status.Counts != wantCounts {
		t.Fatalf("counts = %+v, want %+v", got.Status.Counts, wantCounts)
	}
	assertEvalTestResults(t, got.Status.Results)
	if got.Status.CompletionTime == nil || evalCompletedCondition(got).Reason != evalRunReasonSucceeded {
		t.Fatalf("completion = %v condition = %+v", got.Status.CompletionTime, evalCompletedCondition(got))
	}

	records, err := env.store.ListEvalTrials(context.Background(), evalTestNamespace, "pr-review", "run-uid")
	if err != nil || len(records) != 4 {
		t.Fatalf("records = %d, err = %v", len(records), err)
	}
	infra := records[1] // clean-1/2
	if infra.CaseID != "clean-1" || infra.Trial != 2 || infra.Outcome != store.EvalTrialOutcomeInfraError ||
		infra.InfraClass != string(evals.InfraProviderProxyError) || len(infra.Grades) != 0 {
		t.Fatalf("infra record = %+v", infra)
	}
	failed := records[3] // defect-1/2
	if failed.Outcome != store.EvalTrialOutcomeFail || failed.Verdict != repositoryMonitorReviewVerdictPassed ||
		failed.ReviewerVerdict != repositoryMonitorReviewVerdictPassed || failed.InputTokens == nil || *failed.InputTokens != 120 ||
		failed.Snapshot.CaseSetDigest != frozen.CaseSetDigest || failed.Snapshot.TrialPlanDigest != frozen.TrialPlanDigest {
		t.Fatalf("failed record = %+v", failed)
	}
}

func assertEvalTestSnapshot(t *testing.T, frozen *corev1alpha1.EvalRunSnapshot) {
	t.Helper()
	if frozen.MonitorUID != "monitor-uid" || frozen.MonitorGeneration != 3 ||
		frozen.ReviewerAgent == nil || frozen.ReviewerAgent.UID != "agent-uid" || frozen.ReviewerAgent.Generation != 2 ||
		frozen.RuntimeImageDigest != evalTestImage[strings.LastIndex(evalTestImage, "@")+1:] ||
		frozen.Model != "anthropic/claude-haiku" {
		t.Fatalf("snapshot = %+v", frozen)
	}
	for name, digest := range map[string]string{
		"prompt": frozen.PromptTemplateDigest, "system prompt": frozen.ReviewerSystemPromptDigest, "cases": frozen.CaseSetDigest,
		"graders": frozen.GraderSetDigest, "plan": frozen.TrialPlanDigest,
	} {
		if !strings.HasPrefix(digest, evals.DigestPrefix) {
			t.Fatalf("%s digest = %q", name, digest)
		}
	}
}

func assertEvalTestResults(t *testing.T, got []corev1alpha1.EvalGraderResult) {
	t.Helper()
	results := map[string]corev1alpha1.EvalGraderResult{}
	for _, result := range got {
		results[string(result.Grader)+"/"+result.Tag] = result
	}
	// Verdict over every case: three graded trials, the infra error excluded.
	if r := results["verdict/"]; r.Trials != 3 || r.PassedTrials != 2 || r.Cases != 2 || r.CasesAnyPassed != 2 || r.CasesAllPassed != 1 {
		t.Fatalf("verdict over all cases = %+v", r)
	}
	if r := results["verdict/defect"]; r.Trials != 2 || r.PassedTrials != 1 || r.CasesAnyPassed != 1 || r.CasesAllPassed != 0 {
		t.Fatalf("verdict over defect cases = %+v", r)
	}
	if r := results["verdict/clean"]; r.Trials != 1 || r.PassedTrials != 1 || r.CasesAllPassed != 1 {
		t.Fatalf("verdict over clean cases = %+v", r)
	}
	if r := results["contract/"]; r.Trials != 3 || r.PassedTrials != 3 {
		t.Fatalf("contract = %+v", r)
	}
}

func assertEvalTrialTaskSafe(t *testing.T, task *corev1alpha1.Task) {
	t.Helper()
	if err := validateEvalTrialTaskCredentialFree(task); err != nil {
		t.Fatalf("trial Task %s is not credential free: %v", task.Name, err)
	}
	owner := metav1.GetControllerOf(task)
	if owner == nil || owner.UID != "run-uid" || owner.Kind != "EvalRun" {
		t.Fatalf("trial Task owner = %+v", owner)
	}
	if task.Spec.Priority == nil || *task.Spec.Priority != evalTrialTaskPriority {
		t.Fatalf("priority = %v", task.Spec.Priority)
	}
	if task.Spec.Timeout == nil || task.Spec.Timeout.Duration != 20*time.Minute {
		t.Fatalf("timeout = %v", task.Spec.Timeout)
	}
	if task.Spec.Workspace.GitRepo != evalTestFixture || task.Spec.Workspace.Ref != evalTestHeadSHA {
		t.Fatalf("workspace = %+v", task.Spec.Workspace)
	}
	if task.Labels[labels.LabelRepositoryMonitor] != "" || task.Annotations[labels.AnnotationMonitorRunID] != "" {
		t.Fatalf("trial Task carries monitor ownership metadata: labels %v annotations %v", task.Labels, task.Annotations)
	}
	if !strings.Contains(task.Spec.Prompt, "orka-agents/orka-eval-fixtures") {
		t.Fatalf("prompt does not name the fixture repository")
	}
}

func TestEvalRunStartsTrialsInPlanOrderUpToConcurrency(t *testing.T) {
	run := evalTestRun(2, 1)
	env := newEvalRunTestEnv(t, evalTestObjects(run, evalTestCases())...)
	env.start()
	env.reconcile()
	tasks := env.trialTasks()
	if len(tasks) != 1 || tasks[0].Labels[labels.LabelEvalCase] != "defect-1" || tasks[0].Labels[labels.LabelEvalTrial] != "1" {
		t.Fatalf("first trial Tasks = %d %v", len(tasks), tasks)
	}
	env.reconcile()
	if len(env.trialTasks()) != 1 {
		t.Fatal("a second trial started while the first was running")
	}
	env.finishTask(&tasks[0], corev1alpha1.TaskPhaseSucceeded, evalTestReviewResult("needs_changes"), true)
	env.reconcile()
	var next *corev1alpha1.Task
	for i, task := range env.trialTasks() {
		if task.Labels[labels.LabelEvalCase] == "clean-1" {
			next = &env.trialTasks()[i]
		}
	}
	if next == nil || next.Labels[labels.LabelEvalTrial] != "1" {
		t.Fatalf("second planned trial is clean-1/1, got %v", next)
	}
}

func TestEvalRunInvalidatesOnSnapshotChange(t *testing.T) {
	for name, mutate := range map[string]func(*evalRunTestEnv){
		"case set": func(e *evalRunTestEnv) {
			cm := &corev1.ConfigMap{}
			_ = e.client.Get(context.Background(), types.NamespacedName{Namespace: evalTestNamespace, Name: "cases"}, cm)
			cm.Data["cases.jsonl"] = evalTestCaseLine("defect-1", evals.ReferenceVerdictNotPassed, "defect") + "\n" +
				evalTestCaseLine("clean-1", evals.ReferenceVerdictPassed, "clean", "edited") + "\n"
			if err := e.client.Update(context.Background(), cm); err != nil {
				e.t.Fatal(err)
			}
		},
		"reviewer agent model": func(e *evalRunTestEnv) {
			agent := &corev1alpha1.Agent{}
			_ = e.client.Get(context.Background(), types.NamespacedName{Namespace: evalTestNamespace, Name: "reviewer"}, agent)
			agent.Spec.Model.Name = "claude-opus"
			if err := e.client.Update(context.Background(), agent); err != nil {
				e.t.Fatal(err)
			}
		},
		"runtime image": func(e *evalRunTestEnv) {
			e.reconciler.RuntimeImages.Claude = strings.Replace(evalTestImage, "1234", "9999", 1)
		},
		"monitor deleted": func(e *evalRunTestEnv) {
			monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Namespace: evalTestNamespace, Name: "monitor"}}
			if err := e.client.Delete(context.Background(), monitor); err != nil {
				e.t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			env := newEvalRunTestEnv(t, evalTestObjects(evalTestRun(1, 2), evalTestCases())...)
			env.start()
			env.reconcile()
			if len(env.trialTasks()) != 2 {
				t.Fatal("trials did not start")
			}
			mutate(env)
			env.reconcile()
			run := env.run()
			if run.Status.Phase != corev1alpha1.EvalRunPhaseInvalid || evalCompletedCondition(run).Reason != evalRunReasonSnapshotChanged {
				t.Fatalf("phase = %q condition = %+v", run.Status.Phase, evalCompletedCondition(run))
			}
			// The next reconcile stops the trials still running.
			env.reconcile()
			if tasks := env.trialTasks(); len(tasks) != 0 {
				t.Fatalf("%d trial Tasks still exist after the run became Invalid", len(tasks))
			}
		})
	}
}

func TestEvalRunFailsConfigurationsItCannotRun(t *testing.T) {
	many := make([]string, 0, 26)
	for i := range 26 {
		many = append(many, evalTestCaseLine(fmt.Sprintf("case-%d", i), evals.ReferenceVerdictPassed))
	}
	for name, tc := range map[string]struct {
		trials int32
		cases  string
		mutate func([]client.Object)
		reason string
	}{
		"case trial cap": {trials: 20, cases: strings.Join(many, "\n"), reason: evalRunReasonCaseTrialCapExceeded},
		"invalid cases":  {trials: 1, cases: `{"schemaVersion":"orka.eval.case.v1"}`, reason: evalRunReasonCasesInvalid},
		"validation image": {trials: 1, cases: evalTestCases(), reason: evalRunReasonSubjectUnsupported, mutate: func(objects []client.Object) {
			objects[2].(*corev1alpha1.RepositoryMonitor).Spec.Validation.Image = "ghcr.io/example/validate@sha256:abc"
		}},
		"external runtime": {trials: 1, cases: evalTestCases(), reason: evalRunReasonSubjectUnsupported, mutate: func(objects []client.Object) {
			objects[3].(*corev1alpha1.Agent).Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "byo"}}
		}},
		"cross-namespace reviewer": {trials: 1, cases: evalTestCases(), reason: evalRunReasonSubjectInvalid, mutate: func(objects []client.Object) {
			objects[2].(*corev1alpha1.RepositoryMonitor).Spec.Agents.Reviewer.Namespace = "other"
		}},
	} {
		t.Run(name, func(t *testing.T) {
			objects := evalTestObjects(evalTestRun(tc.trials, 1), tc.cases)
			if tc.mutate != nil {
				tc.mutate(objects)
			}
			env := newEvalRunTestEnv(t, objects...)
			env.reconcile()
			env.reconcile()
			run := env.run()
			if run.Status.Phase != corev1alpha1.EvalRunPhaseFailed || evalCompletedCondition(run).Reason != tc.reason {
				t.Fatalf("phase = %q condition = %+v", run.Status.Phase, evalCompletedCondition(run))
			}
			if run.Status.Snapshot != nil || len(env.trialTasks()) != 0 {
				t.Fatal("a failed configuration froze a snapshot or started trials")
			}
		})
	}
}

func TestEvalRunInfraErrorBudgetInvalidatesRun(t *testing.T) {
	run := evalTestRun(2, 1)
	zero := int32(0)
	run.Spec.MaxInfraErrorPercent = &zero
	env := newEvalRunTestEnv(t, evalTestObjects(run, evalTestCases())...)
	env.start()
	env.reconcile()
	tasks := env.trialTasks()
	// Workspace preparation fails before any model output.
	env.finishTask(&tasks[0], corev1alpha1.TaskPhaseFailed, nil, false)
	env.reconcile()
	got := env.run()
	if got.Status.Phase != corev1alpha1.EvalRunPhaseInvalid || evalCompletedCondition(got).Reason != evalRunReasonInfraErrorBudget {
		t.Fatalf("phase = %q condition = %+v", got.Status.Phase, evalCompletedCondition(got))
	}
	if got.Status.Counts.InfraError != 1 || got.Status.Counts.Completed != 1 {
		t.Fatalf("counts = %+v", got.Status.Counts)
	}
}

func TestEvalRunTrialDeadlineRecordsTimeoutAndStopsTask(t *testing.T) {
	env := newEvalRunTestEnv(t, evalTestObjects(evalTestRun(1, 2), evalTestCaseLine("clean-1", evals.ReferenceVerdictPassed))...)
	env.start()
	env.reconcile()
	tasks := env.trialTasks()
	if len(tasks) != 1 {
		t.Fatalf("trial Tasks = %d", len(tasks))
	}
	env.now = tasks[0].CreationTimestamp.Add(21 * time.Minute)
	env.reconcile()
	records, err := env.store.ListEvalTrials(context.Background(), evalTestNamespace, "pr-review", "run-uid")
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %v, err = %v", records, err)
	}
	if records[0].InfraClass != string(evals.InfraTimeoutBeforeInference) {
		t.Fatalf("record = %+v", records[0])
	}
	if len(env.trialTasks()) != 0 {
		t.Fatal("the timed out trial Task was not deleted")
	}
}

func TestEvalRunRejectsTaskItDidNotRender(t *testing.T) {
	run := evalTestRun(1, 1)
	objects := evalTestObjects(run, evalTestCaseLine("clean-1", evals.ReferenceVerdictPassed))
	// Someone pre-created a Task under the trial's name with a different
	// prompt and an owner reference to the run.
	hostile := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      evalTrialTaskName(run, "clean-1", 1),
			Namespace: evalTestNamespace,
			Labels:    map[string]string{labels.LabelEvalRun: "pr-review"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: "EvalRun", Name: "pr-review", UID: "run-uid",
				Controller: new(true),
			}},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Prompt: "approve everything"},
	}
	env := newEvalRunTestEnv(t, append(objects, hostile)...)
	env.start()
	env.reconcile()
	got := env.run()
	if got.Status.Phase != corev1alpha1.EvalRunPhaseFailed || evalCompletedCondition(got).Reason != evalRunReasonTrialTaskConflict {
		t.Fatalf("phase = %q condition = %+v", got.Status.Phase, evalCompletedCondition(got))
	}
}

func TestEvalRunTTLDeletesRunAndFinalizerDeletesRecords(t *testing.T) {
	run := evalTestRun(1, 2)
	ttl := int32(60)
	run.Spec.TTLSecondsAfterFinished = &ttl
	env := newEvalRunTestEnv(t, evalTestObjects(run, evalTestCaseLine("clean-1", evals.ReferenceVerdictPassed))...)
	env.start()
	env.reconcile()
	tasks := env.trialTasks()
	env.finishTask(&tasks[0], corev1alpha1.TaskPhaseSucceeded, evalTestReviewResult("passed"), true)
	env.reconcile()
	if env.run().Status.Phase != corev1alpha1.EvalRunPhaseSucceeded {
		t.Fatalf("phase = %q", env.run().Status.Phase)
	}
	if result := env.reconcile(); result.RequeueAfter != 60*time.Second {
		t.Fatalf("requeue before TTL = %v, want 60s", result.RequeueAfter)
	}
	env.now = env.now.Add(61 * time.Second)
	env.reconcile() // deletes; the finalizer holds the object
	env.reconcile() // finalizer deletes the records
	err := env.client.Get(context.Background(), types.NamespacedName{Namespace: evalTestNamespace, Name: "pr-review"}, &corev1alpha1.EvalRun{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("EvalRun still exists after its TTL: %v", err)
	}
	records, err := env.store.ListEvalTrials(context.Background(), evalTestNamespace, "pr-review", "run-uid")
	if err != nil || len(records) != 0 {
		t.Fatalf("records after deletion = %d, err = %v", len(records), err)
	}
}

func TestEvalRunAdoptsOwnTrialMissingFromStaleCache(t *testing.T) {
	env := newEvalRunTestEnv(t, evalTestObjects(evalTestRun(1, 1), evalTestCaseLine("clean-1", evals.ReferenceVerdictPassed))...)
	env.start()
	env.reconcile()
	if len(env.trialTasks()) != 1 {
		t.Fatal("trial did not start")
	}
	env.staleTaskCache = true
	env.reconcile()
	env.staleTaskCache = false
	if run := env.run(); run.Status.Phase != corev1alpha1.EvalRunPhaseRunning {
		t.Fatalf("phase = %q condition = %+v; a stale cache must not fail the run", run.Status.Phase, evalCompletedCondition(run))
	}
	if len(env.trialTasks()) != 1 {
		t.Fatal("the trial was duplicated")
	}
}

func TestEvalRunRetriesCleanupOfRecordedTimedOutTrial(t *testing.T) {
	run := evalTestRun(1, 1)
	env := newEvalRunTestEnv(t, evalTestObjects(run, evalTestCases())...)
	env.start()
	env.reconcile()
	tasks := env.trialTasks()
	if len(tasks) != 1 {
		t.Fatalf("trial Tasks = %d", len(tasks))
	}
	// The controller recorded the timeout, then lost the deletion.
	record := &store.EvalTrialRecord{
		SchemaVersion: store.EvalTrialSchemaVersion, Namespace: evalTestNamespace, EvalRun: "pr-review", EvalRunUID: "run-uid",
		CaseID: "defect-1", Trial: 1, TaskName: tasks[0].Name, Outcome: store.EvalTrialOutcomeInfraError,
		InfraClass: string(evals.InfraTimeoutBeforeInference), CompletedAt: env.now,
	}
	if err := env.store.CreateEvalTrial(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	env.reconcile()
	// The deletion only requested cancellation, so the slot frees on the
	// next reconcile, once the Task is gone.
	if remaining := env.trialTasks(); len(remaining) != 0 {
		t.Fatalf("trial Tasks = %v; want the stale trial stopped and none started in its slot", remaining)
	}
	env.reconcile()
	remaining := env.trialTasks()
	if len(remaining) != 1 || remaining[0].Labels[labels.LabelEvalCase] != "clean-1" {
		t.Fatalf("trial Tasks = %v; want only clean-1 running", remaining)
	}
}

// evalTestHoldTask adds a finalizer, as the Task controller does, so a
// deletion leaves the Task in place until the test releases it.
func (e *evalRunTestEnv) holdTask(task *corev1alpha1.Task) {
	e.t.Helper()
	task.Finalizers = append(task.Finalizers, "test.orka.ai/hold")
	if err := e.client.Update(context.Background(), task); err != nil {
		e.t.Fatal(err)
	}
}

func (e *evalRunTestEnv) releaseTask(name string) {
	e.t.Helper()
	task := &corev1alpha1.Task{}
	if err := e.client.Get(context.Background(), types.NamespacedName{Namespace: evalTestNamespace, Name: name}, task); err != nil {
		e.t.Fatal(err)
	}
	task.Finalizers = nil
	if err := e.client.Update(context.Background(), task); err != nil {
		e.t.Fatal(err)
	}
}

func TestEvalRunTimedOutTrialHoldsItsSlotUntilTheTaskIsGone(t *testing.T) {
	env := newEvalRunTestEnv(t, evalTestObjects(evalTestRun(1, 1), evalTestCases())...)
	env.start()
	env.reconcile()
	tasks := env.trialTasks()
	if len(tasks) != 1 {
		t.Fatalf("trial Tasks = %d", len(tasks))
	}
	timedOut := tasks[0].Name
	env.holdTask(&tasks[0])
	env.now = tasks[0].CreationTimestamp.Add(21 * time.Minute)

	// The timeout is recorded and the Task is asked to stop, but it is still
	// running behind its finalizer, so no trial may take its slot.
	for range 2 {
		env.reconcile()
		remaining := env.trialTasks()
		if len(remaining) != 1 || remaining[0].Name != timedOut || remaining[0].DeletionTimestamp.IsZero() {
			t.Fatalf("trial Tasks = %v; want only the stopping trial, holding its slot", remaining)
		}
	}
	records, err := env.store.ListEvalTrials(context.Background(), evalTestNamespace, "pr-review", "run-uid")
	if err != nil || len(records) != 1 || records[0].InfraClass != string(evals.InfraTimeoutBeforeInference) {
		t.Fatalf("records = %+v, err = %v", records, err)
	}

	env.releaseTask(timedOut)
	env.reconcile()
	remaining := env.trialTasks()
	if len(remaining) != 1 || remaining[0].Labels[labels.LabelEvalCase] != "clean-1" {
		t.Fatalf("trial Tasks = %v; want clean-1 started once the timed out Task was gone", remaining)
	}
}

func TestEvalRunWaitsForTerminalTaskPhaseBeforeGrading(t *testing.T) {
	env := newEvalRunTestEnv(t, evalTestObjects(evalTestRun(1, 1), evalTestCaseLine("defect-1", evals.ReferenceVerdictNotPassed, "defect"))...)
	env.start()
	env.reconcile()
	tasks := env.trialTasks()
	if len(tasks) != 1 {
		t.Fatalf("trial Tasks = %d", len(tasks))
	}
	task := &tasks[0]
	// The dispatcher records a succeeded execution before delivery and
	// cleanup finish; the Task is still Running.
	env.appendEvent(task, executionevents.ExecutionEventTypeModelUsageUpdated, map[string]any{"inputTokens": 120, "outputTokens": 30})
	if err := env.store.SaveResult(context.Background(), task.Namespace, task.Name, evalTestReviewResult("needs_changes")); err != nil {
		t.Fatal(err)
	}
	task.Status.Phase = corev1alpha1.TaskPhaseRunning
	task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
	task.Status.Execution = &corev1alpha1.TaskExecutionStatus{
		State: corev1alpha1.TaskExecutionStateSucceeded, Outcome: corev1alpha1.TaskExecutionOutcomeSucceeded,
	}
	if err := env.client.Status().Update(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	env.reconcile()
	records, err := env.store.ListEvalTrials(context.Background(), evalTestNamespace, "pr-review", "run-uid")
	if err != nil || len(records) != 0 {
		t.Fatalf("records = %+v, err = %v; a Running Task must not be graded", records, err)
	}

	env.finishTask(task, corev1alpha1.TaskPhaseSucceeded, nil, false)
	env.reconcile()
	records, err = env.store.ListEvalTrials(context.Background(), evalTestNamespace, "pr-review", "run-uid")
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %+v, err = %v", records, err)
	}
	if records[0].Outcome != store.EvalTrialOutcomePass || records[0].Verdict != repositoryMonitorReviewVerdictNeedsChanges {
		t.Fatalf("record = %+v; want the succeeded review graded", records[0])
	}
}

func TestEvalRunExcludesCompletionWithoutModelText(t *testing.T) {
	env := newEvalRunTestEnv(t, evalTestObjects(evalTestRun(1, 1), evalTestCaseLine("clean-1", evals.ReferenceVerdictPassed))...)
	env.start()
	env.reconcile()
	tasks := env.trialTasks()
	if len(tasks) != 1 {
		t.Fatalf("trial Tasks = %d", len(tasks))
	}
	task := &tasks[0]
	// With nothing streamed, the dispatcher journals the runtime's
	// placeholder result as the transcript and stores it as the result.
	if _, err := env.store.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace: task.Namespace, StreamType: store.ExecutionEventStreamTypeTask, StreamID: task.Name, TaskName: task.Name,
		Type: executionevents.ExecutionEventTypeModelMessage, Severity: executionevents.ExecutionEventSeverityInfo,
		ContentText: harnessv2.CompletedWithoutTextResult,
	}); err != nil {
		t.Fatal(err)
	}
	env.finishTask(task, corev1alpha1.TaskPhaseSucceeded, []byte(harnessv2.CompletedWithoutTextResult), false)
	env.reconcile()
	records, err := env.store.ListEvalTrials(context.Background(), evalTestNamespace, "pr-review", "run-uid")
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %+v, err = %v", records, err)
	}
	if records[0].Outcome != store.EvalTrialOutcomeInfraError || records[0].InfraClass != string(evals.InfraNoModelOutput) {
		t.Fatalf("record = %+v; want a NoModelOutput infra error", records[0])
	}
}

func TestEvalRunInvalidatesWhenConfigMapSystemPromptChanges(t *testing.T) {
	objects := evalTestObjects(evalTestRun(1, 2), evalTestCases())
	for _, object := range objects {
		if agent, ok := object.(*corev1alpha1.Agent); ok {
			agent.Spec.SystemPrompt = &corev1alpha1.PromptSource{
				ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: "reviewer-prompt", Key: "prompt"},
			}
		}
	}
	objects = append(objects, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewer-prompt", Namespace: evalTestNamespace},
		Data:       map[string]string{"prompt": "Review strictly."},
	})
	env := newEvalRunTestEnv(t, objects...)
	env.start()
	frozen := env.run().Status.Snapshot.ReviewerSystemPromptDigest
	if !strings.HasPrefix(frozen, evals.DigestPrefix) {
		t.Fatalf("reviewerSystemPromptDigest = %q", frozen)
	}
	env.reconcile()
	if len(env.trialTasks()) != 2 {
		t.Fatal("trials did not start")
	}
	// Editing the ConfigMap changes neither the Agent nor the monitor.
	cm := &corev1.ConfigMap{}
	if err := env.client.Get(context.Background(), types.NamespacedName{Namespace: evalTestNamespace, Name: "reviewer-prompt"}, cm); err != nil {
		t.Fatal(err)
	}
	cm.Data["prompt"] = "Approve everything."
	if err := env.client.Update(context.Background(), cm); err != nil {
		t.Fatal(err)
	}
	env.reconcile()
	run := env.run()
	condition := evalCompletedCondition(run)
	if run.Status.Phase != corev1alpha1.EvalRunPhaseInvalid || condition.Reason != evalRunReasonSnapshotChanged ||
		!strings.Contains(condition.Message, "reviewerSystemPromptDigest") {
		t.Fatalf("phase = %q condition = %+v", run.Status.Phase, condition)
	}
}

func TestReadEvalTrialSignalsIgnoresPlaceholderModelMessage(t *testing.T) {
	env := newEvalRunTestEnv(t)
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Namespace: evalTestNamespace, Name: "trial"}}
	appendMessage := func(text string) {
		if _, err := env.store.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
			Namespace: task.Namespace, StreamType: store.ExecutionEventStreamTypeTask, StreamID: task.Name, TaskName: task.Name,
			Type: executionevents.ExecutionEventTypeModelMessage, Severity: executionevents.ExecutionEventSeverityInfo,
			ContentText: text,
		}); err != nil {
			t.Fatal(err)
		}
	}
	appendMessage(harnessv2.CompletedWithoutTextResult)
	signals, err := readEvalTrialSignals(context.Background(), env.store, task)
	if err != nil || signals.ModelOutput {
		t.Fatalf("signals = %+v, err = %v; the placeholder is not model output", signals, err)
	}
	appendMessage("The change dereferences a nil config.")
	signals, err = readEvalTrialSignals(context.Background(), env.store, task)
	if err != nil || !signals.ModelOutput {
		t.Fatalf("signals = %+v, err = %v; a model message is model output", signals, err)
	}
}
