package controller

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
)

const (
	reviewSubjectTestOwner = "orka-agents"
	reviewSubjectTestRepo  = "orka"
	reviewSubjectTestHead  = "head-sha"
)

func reviewSubjectTestMonitor(validationImage string) *corev1alpha1.RepositoryMonitor {
	return &corev1alpha1.RepositoryMonitor{
		ObjectMeta: metav1.ObjectMeta{Name: "monitor", Namespace: "team-a"},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL:    "https://github.com/orka-agents/orka",
			Agents:     corev1alpha1.RepositoryMonitorAgents{Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"}},
			Validation: corev1alpha1.RepositoryMonitorValidationSpec{Image: validationImage},
		},
	}
}

func reviewSubjectTestPullRequest() repositoryMonitorPullRequest {
	return repositoryMonitorPullRequest{
		Number:     7,
		Title:      "Fix a bug",
		BaseBranch: "main",
		BaseSHA:    "base-sha",
		HeadSHA:    reviewSubjectTestHead,
		HeadRepo:   "orka-agents/orka",
	}
}

func reviewSubjectTestFiles() []repositoryMonitorPullRequestFileResponse {
	return []repositoryMonitorPullRequestFileResponse{{
		Filename:  "pkg/x.go",
		Status:    "modified",
		Additions: 1,
		Deletions: 1,
		Patch:     "@@ -1 +1 @@\n-old\n+new",
	}}
}

func reviewSubjectTestInput(monitor *corev1alpha1.RepositoryMonitor, files []repositoryMonitorPullRequestFileResponse) repositoryMonitorReviewTaskInput {
	pr := reviewSubjectTestPullRequest()
	return repositoryMonitorReviewTaskInput{
		Monitor:       monitor,
		Owner:         reviewSubjectTestOwner,
		Repository:    reviewSubjectTestRepo,
		PullRequest:   pr,
		Context:       repositoryMonitorReviewContextFromFiles(reviewSubjectTestOwner, reviewSubjectTestRepo, pr, files),
		Plan:          repositoryMonitorReviewPlan{Mode: repositoryMonitorReviewModeFull},
		Priority:      700,
		WorkspaceRepo: "https://github.com/orka-agents/orka",
	}
}

func reviewSubjectTestResult(verdict, headSHA string) []byte {
	return fmt.Appendf(nil, `{"schemaVersion":"orka.prReview.v1","repo":"orka-agents/orka","prNumber":7,"headSHA":%q,"verdict":%q,"confidence":"high","summary":"ok","findings":[],"security":{"status":"clear"},"tests":{"status":"not_run"}}`, headSHA, verdict)
}

func TestBuildRepositoryMonitorReviewTaskRendersReviewContent(t *testing.T) {
	monitor := reviewSubjectTestMonitor("ghcr.io/example/validate@sha256:abc")
	in := reviewSubjectTestInput(monitor, reviewSubjectTestFiles())
	in.ReadCredentialRef = &corev1.LocalObjectReference{Name: "read-token"}

	task, err := buildRepositoryMonitorReviewTask(in)
	if err != nil {
		t.Fatalf("buildRepositoryMonitorReviewTask() error = %v", err)
	}
	if task.Name != "" || task.Labels != nil || task.OwnerReferences != nil {
		t.Fatalf("builder set caller-owned metadata: name=%q labels=%v owners=%v", task.Name, task.Labels, task.OwnerReferences)
	}
	if task.Namespace != monitor.Namespace {
		t.Fatalf("namespace = %q, want %q", task.Namespace, monitor.Namespace)
	}
	wantAnnotations := map[string]string{
		labels.AnnotationMonitorItemKind:           repositoryMonitorPullRequestKind,
		labels.AnnotationMonitorItemNumber:         "7",
		labels.AnnotationMonitorHeadSHA:            reviewSubjectTestHead,
		labels.AnnotationGitHubRepository:          "orka-agents/orka",
		labels.AnnotationAgentReadOnly:             booleanTrueValue,
		labels.AnnotationWorkspaceInitContainer:    booleanTrueValue,
		labels.AnnotationRepositoryValidationImage: "ghcr.io/example/validate@sha256:abc",
	}
	if !reflect.DeepEqual(task.Annotations, wantAnnotations) {
		t.Fatalf("annotations = %v, want %v", task.Annotations, wantAnnotations)
	}
	spec := task.Spec
	if spec.Type != corev1alpha1.TaskTypeAgent || spec.AgentRef == nil || spec.AgentRef.Name != "reviewer" {
		t.Fatalf("task type/agent = %q/%v, want agent reviewer", spec.Type, spec.AgentRef)
	}
	if spec.AgentRef == monitor.Spec.Agents.Reviewer {
		t.Fatal("builder aliased the monitor's reviewer reference")
	}
	if want := buildRepositoryMonitorReviewPrompt(monitor, in.Owner, in.Repository, in.PullRequest, in.Context, in.Plan); spec.Prompt != want {
		t.Fatal("prompt does not match buildRepositoryMonitorReviewPrompt")
	}
	if spec.Priority == nil || *spec.Priority != 700 || spec.Timeout == nil || spec.Timeout.Duration != repositoryMonitorReviewTaskTimeout {
		t.Fatalf("priority/timeout = %v/%v", spec.Priority, spec.Timeout)
	}
	if spec.AgentRuntime == nil || !reflect.DeepEqual(spec.AgentRuntime.AllowedTools, repositoryMonitorReviewAllowedTools(monitor)) {
		t.Fatalf("allowed tools = %v", spec.AgentRuntime)
	}
	if !strings.Contains(strings.Join(spec.AgentRuntime.AllowedTools, ","), tools.RunValidationToolName) {
		t.Fatalf("allowed tools %v omit %s with a validation image", spec.AgentRuntime.AllowedTools, tools.RunValidationToolName)
	}
	ws := spec.Workspace
	if ws == nil || ws.Intent != corev1alpha1.WorkspaceIntentRead || ws.GitRepo != in.WorkspaceRepo || ws.Ref != reviewSubjectTestHead {
		t.Fatalf("workspace = %+v, want read-only checkout of the head", ws)
	}
	if ws.ReadCredentialRef == nil || ws.ReadCredentialRef.Name != "read-token" {
		t.Fatalf("read credential = %v, want read-token", ws.ReadCredentialRef)
	}
}

func TestBuildRepositoryMonitorReviewTaskWithoutCredentialHasNoForgeCredentials(t *testing.T) {
	task, err := buildRepositoryMonitorReviewTask(reviewSubjectTestInput(reviewSubjectTestMonitor(""), reviewSubjectTestFiles()))
	if err != nil {
		t.Fatalf("buildRepositoryMonitorReviewTask() error = %v", err)
	}
	ws := task.Spec.Workspace
	if ws.ReadCredentialRef != nil || ws.PublicationReadCredentialRef != nil || ws.PublicationCredentialRef != nil || ws.ForgeCredentialRef != nil {
		t.Fatalf("workspace carries credential refs: %+v", ws)
	}
	if _, ok := task.Annotations[labels.AnnotationRepositoryValidationImage]; ok {
		t.Fatal("validation image annotation set without a configured image")
	}
	if strings.Contains(strings.Join(task.Spec.AgentRuntime.AllowedTools, ","), tools.RunValidationToolName) {
		t.Fatalf("allowed tools %v include %s without a validation image", task.Spec.AgentRuntime.AllowedTools, tools.RunValidationToolName)
	}
}

func TestBuildRepositoryMonitorReviewTaskRecordsIncrementalPlan(t *testing.T) {
	in := reviewSubjectTestInput(reviewSubjectTestMonitor(""), reviewSubjectTestFiles())
	in.Plan = repositoryMonitorReviewPlan{Mode: repositoryMonitorReviewModeIncremental, Base: &store.ReviewRecord{ID: "review-1", HeadSHA: "old-head"}}
	task, err := buildRepositoryMonitorReviewTask(in)
	if err != nil {
		t.Fatalf("buildRepositoryMonitorReviewTask() error = %v", err)
	}
	if got := task.Annotations[repositoryMonitorAnnotationReviewMode]; got != repositoryMonitorReviewModeIncremental {
		t.Fatalf("review mode annotation = %q", got)
	}
	if got := task.Annotations[repositoryMonitorAnnotationReviewBase]; got != in.Plan.baseID() {
		t.Fatalf("review base annotation = %q, want %q", got, in.Plan.baseID())
	}
}

func TestBuildRepositoryMonitorReviewTaskRequiresReviewer(t *testing.T) {
	if _, err := buildRepositoryMonitorReviewTask(repositoryMonitorReviewTaskInput{}); err == nil {
		t.Fatal("accepted a nil monitor")
	}
	monitor := reviewSubjectTestMonitor("")
	monitor.Spec.Agents.Reviewer = nil
	if _, err := buildRepositoryMonitorReviewTask(reviewSubjectTestInput(monitor, nil)); err == nil {
		t.Fatal("accepted a monitor without a reviewer")
	}
}

func TestRepositoryMonitorReviewOutcomeFromResult(t *testing.T) {
	complete, err := buildRepositoryMonitorReviewTask(reviewSubjectTestInput(reviewSubjectTestMonitor(""), reviewSubjectTestFiles()))
	if err != nil {
		t.Fatalf("buildRepositoryMonitorReviewTask() error = %v", err)
	}
	unavailableInput := reviewSubjectTestInput(reviewSubjectTestMonitor(""), nil)
	unavailableInput.Context = newRepositoryMonitorReviewContext(reviewSubjectTestOwner, reviewSubjectTestRepo, unavailableInput.PullRequest)
	unavailableInput.Context.ContextUnavailable = "github_unavailable"
	unavailable, err := buildRepositoryMonitorReviewTask(unavailableInput)
	if err != nil {
		t.Fatalf("buildRepositoryMonitorReviewTask() error = %v", err)
	}
	expected := repositoryMonitorReviewExpectation{Repository: "orka-agents/orka", PRNumber: 7, HeadSHA: reviewSubjectTestHead}

	tests := []struct {
		name          string
		raw           []byte
		expected      repositoryMonitorReviewExpectation
		prompt        string
		wantRejection string
		wantGate      bool
	}{
		{name: "passed with complete context", raw: reviewSubjectTestResult("passed", reviewSubjectTestHead), expected: expected, prompt: complete.Spec.Prompt},
		{name: "passed with unavailable context is gated", raw: reviewSubjectTestResult("passed", reviewSubjectTestHead), expected: expected, prompt: unavailable.Spec.Prompt, wantGate: true},
		{name: "passed without embedded context is gated", raw: reviewSubjectTestResult("passed", reviewSubjectTestHead), expected: expected, prompt: "no context", wantGate: true},
		{name: "needs_changes is never gated", raw: reviewSubjectTestResult("needs_changes", reviewSubjectTestHead), expected: expected, prompt: "no context"},
		{name: "empty expected repository skips the repository check", raw: reviewSubjectTestResult("passed", reviewSubjectTestHead), expected: repositoryMonitorReviewExpectation{PRNumber: 7, HeadSHA: reviewSubjectTestHead}, prompt: complete.Spec.Prompt},
		{name: "head mismatch", raw: reviewSubjectTestResult("passed", "other-head"), expected: expected, prompt: complete.Spec.Prompt, wantRejection: "headSHA"},
		{name: "head prefix is not an exact match", raw: reviewSubjectTestResult("passed", "head"), expected: expected, prompt: complete.Spec.Prompt, wantRejection: "headSHA"},
		{name: "repository mismatch", raw: reviewSubjectTestResult("passed", reviewSubjectTestHead), expected: repositoryMonitorReviewExpectation{Repository: "other/repo", PRNumber: 7, HeadSHA: reviewSubjectTestHead}, prompt: complete.Spec.Prompt, wantRejection: "repo"},
		{name: "pull request mismatch", raw: reviewSubjectTestResult("passed", reviewSubjectTestHead), expected: repositoryMonitorReviewExpectation{Repository: "orka-agents/orka", PRNumber: 8, HeadSHA: reviewSubjectTestHead}, prompt: complete.Spec.Prompt, wantRejection: "prNumber"},
		{name: "unknown verdict", raw: reviewSubjectTestResult("great", reviewSubjectTestHead), expected: expected, prompt: complete.Spec.Prompt, wantRejection: "verdict"},
		{name: "not a review", raw: []byte("I looked at the code."), expected: expected, prompt: complete.Spec.Prompt, wantRejection: "JSON review object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := repositoryMonitorReviewOutcomeFromResult(tt.raw, tt.expected, tt.prompt)
			if tt.wantRejection != "" {
				if got.Review != nil || !strings.Contains(got.Rejection, tt.wantRejection) || got.GateReason != "" {
					t.Fatalf("outcome = %+v, want rejection containing %q", got, tt.wantRejection)
				}
				return
			}
			if got.Rejection != "" || got.Review == nil {
				t.Fatalf("outcome = %+v, want an accepted review", got)
			}
			if (got.GateReason != "") != tt.wantGate {
				t.Fatalf("gate reason = %q, want gated=%v", got.GateReason, tt.wantGate)
			}
			if want := repositoryMonitorReviewVerdictGateReason(tt.prompt, got.Review.Verdict); got.GateReason != want {
				t.Fatalf("gate reason = %q, want repositoryMonitorReviewVerdictGateReason %q", got.GateReason, want)
			}
		})
	}
}

func TestRepositoryMonitorReviewExpectationForTaskPrefersTaskHead(t *testing.T) {
	item := &store.MonitorItem{Number: 7, HeadSHA: "item-head"}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		labels.AnnotationGitHubRepository: " orka-agents/orka ",
		labels.AnnotationMonitorHeadSHA:   "task-head",
	}}}
	want := repositoryMonitorReviewExpectation{Repository: "orka-agents/orka", PRNumber: 7, HeadSHA: "task-head"}
	if got := repositoryMonitorReviewExpectationForTask(item, task); got != want {
		t.Fatalf("expectation = %+v, want %+v", got, want)
	}
	delete(task.Annotations, labels.AnnotationMonitorHeadSHA)
	want.HeadSHA = "item-head"
	if got := repositoryMonitorReviewExpectationForTask(item, task); got != want {
		t.Fatalf("expectation without task head = %+v, want %+v", got, want)
	}
}
