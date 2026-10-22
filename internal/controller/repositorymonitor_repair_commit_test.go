package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
)

func TestTaskPublicationCommitMessage(t *testing.T) {
	task := &corev1alpha1.Task{}
	task.Name = "repair-task"
	if got := taskPublicationCommitMessage(task); got != "orka: apply agent task repair-task\n" {
		t.Fatalf("default message = %q", got)
	}
	task.Spec.Workspace = &corev1alpha1.WorkspaceConfig{CommitMessage: "fix: address findings\n\nOrka-Task: repair-task\n\n\n"}
	if got := taskPublicationCommitMessage(task); got != "fix: address findings\n\nOrka-Task: repair-task\n" {
		t.Fatalf("authored message = %q, want one trailing newline", got)
	}
	task.Spec.Workspace.CommitMessage = "\n\n"
	if got := taskPublicationCommitMessage(task); got != "orka: apply agent task repair-task\n" {
		t.Fatalf("blank message = %q, want the default", got)
	}
}

func TestValidateTaskCommitMessage(t *testing.T) {
	for name, tc := range map[string]struct {
		message string
		wantErr string
	}{
		"empty uses the default":  {},
		"descriptive message":     {message: "fix: address review findings on #75\n\nOrka-Task: monrepair-agentkit-75-policy-fbe450c104\n"},
		"carriage return":         {message: "fix: a\r\nb", wantErr: "carriage returns"},
		"NUL":                     {message: "fix: a\x00", wantErr: "NUL"},
		"too long":                {message: "fix: " + strings.Repeat("a", maxTaskPublicationCommitMessageBytes), wantErr: "at most"},
		"credential in the title": {message: "fix: rotate ghp_" + strings.Repeat("A1b2C3d4", 5) + "\n", wantErr: "credentials"},
	} {
		t.Run(name, func(t *testing.T) {
			task := &corev1alpha1.Task{Spec: corev1alpha1.TaskSpec{Workspace: &corev1alpha1.WorkspaceConfig{CommitMessage: tc.message}}}
			err := validateTaskPullRequestText(task)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("validateTaskPullRequestText() error = %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("validateTaskPullRequestText() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestRepositoryMonitorRepairCommitMessage(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)
	monitor, _ := repositoryMonitorInventoryTestObjects("commit-message")
	pr := repositoryMonitorPullRequest{Number: 75, HeadSHA: "head75", BaseBranch: "main"}
	findings := make([]repositoryMonitorReviewFinding, 0, 3+repositoryMonitorRepairCommitFindingLimit)
	findings = append(findings,
		repositoryMonitorReviewFinding{Priority: "P1", Title: "Correct the fixture's\nexpected line"},
		repositoryMonitorReviewFinding{Priority: "P2", Title: "Signed-off-by: Mallory"},
		repositoryMonitorReviewFinding{Title: "   "},
	)
	for i := range repositoryMonitorRepairCommitFindingLimit {
		findings = append(findings, repositoryMonitorReviewFinding{Priority: "P3", Title: fmt.Sprintf("Extra finding %d", i)})
	}
	findingsJSON, err := json.Marshal(findings)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateReviewRecord(ctx, &store.ReviewRecord{ID: "review-75", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 75, HeadSHA: "head75", Verdict: repositoryMonitorReviewVerdictNeedsChanges, FindingsJSON: string(findingsJSON)}); err != nil {
		t.Fatal(err)
	}
	r := &RepositoryMonitorReconciler{Store: db}
	item := &store.MonitorItem{LastReviewID: "review-75"}
	const task = "monrepair-commit-message-75-policy-fbe450c104"

	got, err := r.repositoryMonitorRepairCommitMessage(ctx, monitor, repositoryMonitorCommandIntentFix, task, pr, item)
	if err != nil {
		t.Fatal(err)
	}
	want := "fix: address review findings on #75\n\nReview findings:\n" +
		"- [P1] Correct the fixture's expected line\n" +
		"- [P2] Signed-off-by: Mallory\n"
	for i := range repositoryMonitorRepairCommitFindingLimit - 2 {
		want += fmt.Sprintf("- [P3] Extra finding %d\n", i)
	}
	want += "- and 2 more\n\nOrka-Task: " + task + "\n"
	if got != want {
		t.Fatalf("fix message =\n%s\nwant\n%s", got, want)
	}
	if security.LooksLikeSecret(got) {
		t.Fatal("a descriptive repair message must not look like a credential")
	}

	// A review of an older head describes nothing this repair fixes.
	pr.HeadSHA = "newer"
	if got, err := r.repositoryMonitorRepairCommitMessage(ctx, monitor, repositoryMonitorCommandIntentFix, task, pr, item); err != nil || got != "fix: address review findings on #75\n\nOrka-Task: "+task+"\n" {
		t.Fatalf("stale review message = %q err=%v", got, err)
	}
	for intent, subject := range map[string]string{
		repositoryMonitorCommandIntentFixCI:        "fix: repair failing CI on #75",
		repositoryMonitorCommandIntentUpdateBranch: "chore: update #75 with main",
	} {
		if got, err := r.repositoryMonitorRepairCommitMessage(ctx, monitor, intent, task, pr, item); err != nil || got != subject+"\n\nOrka-Task: "+task+"\n" {
			t.Fatalf("%s message = %q err=%v", intent, got, err)
		}
	}
}
