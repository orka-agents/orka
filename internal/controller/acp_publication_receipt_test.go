package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/publisher"
	"github.com/orka-agents/orka/internal/store"
)

func TestPullRequestNumberFromForgeID(t *testing.T) {
	number, ok := pullRequestNumberFromForgeID("github:123456:42")
	if !ok || number != 42 {
		t.Fatalf("number, ok = %d, %t", number, ok)
	}
	for _, value := range []string{"42", "gitlab:123:42", "github:123:not-a-number", "github:123:0", "github:123:2147483648"} {
		if _, ok := pullRequestNumberFromForgeID(value); ok {
			t.Fatalf("unexpectedly parsed %q", value)
		}
	}
}

func TestExpectedPublicationRemoteState(t *testing.T) {
	got, err := expectedPublicationRemoteState(nil, nil, publisher.Repository{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Absent || got.SHA != "" {
		t.Fatalf("nil workspace state = %#v, want absent", got)
	}
	workspace := &corev1alpha1.WorkspaceConfig{ExpectedRemoteSHA: strings.Repeat("a", 40)}
	got, err = expectedPublicationRemoteState(workspace, nil, publisher.Repository{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Absent || got.SHA != workspace.ExpectedRemoteSHA {
		t.Fatalf("exact workspace state = %#v", got)
	}

	baseline := &store.VerifiedBranchBaseline{
		RepositoryID: "github.com/orka-agents/orka",
		Ref:          "refs/heads/orka/session-session-uid",
		SHA:          strings.Repeat("b", 40),
	}
	target := publisher.Repository{ID: baseline.RepositoryID}
	session := &acpTaskSession{VerifiedBaseline: baseline}
	workspace.ExpectedRemoteSHA = baseline.SHA
	got, err = expectedPublicationRemoteState(workspace, session, target, baseline.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if got.Absent || got.SHA != baseline.SHA {
		t.Fatalf("session workspace state = %#v", got)
	}
	workspace.ExpectedRemoteSHA = strings.Repeat("c", 40)
	if _, err := expectedPublicationRemoteState(workspace, session, target, baseline.Ref); err == nil {
		t.Fatal("conflicting expectedRemoteSHA unexpectedly accepted")
	}
}

func TestWorkspaceSourceRefPreservesHistoricalCommitOIDs(t *testing.T) {
	for _, oid := range []string{strings.Repeat("a", 40), strings.Repeat("b", 64)} {
		workspace := &corev1alpha1.WorkspaceConfig{Branch: "main", Ref: oid}
		got, err := workspaceSourceRef(workspace)
		if err != nil {
			t.Fatal(err)
		}
		if got != oid {
			t.Fatalf("workspaceSourceRef() = %q, want exact historical commit %q", got, oid)
		}
	}
}

func TestRuntimeSessionDeltaAbandonmentFinalization(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "task", Namespace: "default", UID: types.UID("task-uid")},
		Status:     corev1alpha1.TaskStatus{Execution: &corev1alpha1.TaskExecutionStatus{Attempt: 2}},
	}
	finalization, err := runtimeSessionDeltaAbandonmentFinalization(task, "delta-1", corev1alpha1.TaskDeliveryStatus{State: corev1alpha1.TaskDeliveryStateDeliveryConflict})
	if err != nil {
		t.Fatal(err)
	}
	if finalization.PublicationID == "" || finalization.PublicationGeneration != 1 || finalization.PublicationVersion != 1 || finalization.TerminalState != harnessv2.PublicationTerminalDeliveryConflict || finalization.TerminalReceiptDigest == "" {
		t.Fatalf("finalization = %#v", finalization)
	}
}

func TestWorkspaceSourceRefPreservesExplicitAndBareSelectors(t *testing.T) {
	const (
		bareTag         = "v1.2.3"
		canonicalTag    = "refs/tags/" + bareTag
		canonicalBranch = "refs/heads/" + defaultACPSourceBranch
	)
	for _, test := range []struct {
		name string
		ref  string
		want string
	}{
		{name: "canonical-tag", ref: canonicalTag, want: canonicalTag},
		{name: "bare-tag", ref: bareTag, want: bareTag},
		{name: "canonical-branch", ref: canonicalBranch, want: canonicalBranch},
		{name: "bare-branch", ref: defaultACPSourceBranch, want: defaultACPSourceBranch},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := workspaceSourceRef(&corev1alpha1.WorkspaceConfig{Ref: test.ref})
			if err != nil || got != test.want {
				t.Fatalf("workspaceSourceRef(%q) = %q, %v, want %q, nil", test.ref, got, err, test.want)
			}
		})
	}
}
