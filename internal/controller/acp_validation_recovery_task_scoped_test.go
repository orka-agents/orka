package controller

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A task-scoped Task has no Session binding, so its dispatch must record the
// workspace digest itself or a later abandoned-validation recovery has nothing
// to compare against.
func TestACPDispatcherRecordsTaskScopedRuntimeWorkspaceDigest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture := newTaskScopedCreateConflictFixture(t, ctx, "task-scoped-workspace-digest", types.UID("99999999-9999-9999-9999-999999999999"),
		func(profile harnessv2.RuntimeProfile, digest harnessv2.ProfileDigest, _ *client.Client) *httptest.Server {
			return newDispatcherRuntimeServerWithOptions(t, profile, digest, dispatcherRuntimeServerOptions{})
		})
	defer fixture.stop()

	dispatchQueuedTask(ctx, t, fixture.dispatcher, fixture.task.DeepCopy())

	current := fixture.currentTask(t, ctx)
	if current.Spec.SessionRef != nil || current.Status.Execution == nil {
		t.Fatalf("fixture is not a dispatched task-scoped Task: %#v", current.Status.Execution)
	}
	_, workspace, err := emptyRuntimeWorkspace(current, string(current.UID))
	if err != nil {
		t.Fatal(err)
	}
	want, err := acpRuntimeWorkspaceBindingDigest("", workspace)
	if err != nil {
		t.Fatal(err)
	}
	if got := current.Status.Execution.RuntimeSessionWorkspaceDigest; got != want {
		t.Fatalf("task-scoped workspace digest = %q, want %q", got, want)
	}
}

func TestRecoveredValidationWorkspaceTaskScoped(t *testing.T) {
	for _, git := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "git"}[git], func(t *testing.T) {
			t.Run("recorded digest", func(t *testing.T) {
				d, task, _, _ := newValidationRecoveryFixture(t, git, false)
				baseline, _, err := d.recoveredValidationWorkspace(t.Context(), task, task.UID)
				if err != nil {
					t.Fatal(err)
				}
				if !git && baseline.RepositoryIdentity != acpNoWorkspaceRevision+":"+string(task.UID) {
					t.Fatalf("task-scoped empty baseline = %q", baseline.RepositoryIdentity)
				}
			})
			t.Run("changed digest", func(t *testing.T) {
				d, task, _, _ := newValidationRecoveryFixture(t, git, false)
				task.Status.Execution.RuntimeSessionWorkspaceDigest = testControlDigestForDispatcher("other")
				if _, _, err := d.recoveredValidationWorkspace(t.Context(), task, task.UID); err == nil {
					t.Fatal("accepted a task-scoped baseline that differs from its recorded workspace")
				}
			})
			t.Run("legacy empty digest", func(t *testing.T) {
				// Controllers before the fix never recorded the digest for
				// task-scoped Tasks; recovery must not loop on them forever.
				d, task, _, _ := newValidationRecoveryFixture(t, git, false)
				task.Status.Execution.RuntimeSessionWorkspaceDigest = ""
				if _, _, err := d.recoveredValidationWorkspace(t.Context(), task, task.UID); err != nil {
					t.Fatalf("legacy task-scoped recovery: %v", err)
				}
			})
			t.Run("legacy empty digest still needs committed evidence", func(t *testing.T) {
				if !git {
					t.Skip("only git workspaces have preparation evidence")
				}
				d, task, control, _ := newValidationRecoveryFixture(t, git, false)
				task.Status.Execution.RuntimeSessionWorkspaceDigest = ""
				control.preparation = nil
				if _, _, err := d.recoveredValidationWorkspace(t.Context(), task, task.UID); err == nil {
					t.Fatal("legacy task-scoped recovery accepted missing preparation evidence")
				}
			})
		})
	}
}

// Only task-scoped Tasks may omit the digest. A Session-backed Task always
// recorded it, so an empty value there is still a mismatch.
func TestRecoveredValidationWorkspaceSessionRequiresDigest(t *testing.T) {
	for _, git := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "git"}[git], func(t *testing.T) {
			d, task, _, _ := newValidationRecoveryFixture(t, git, true)
			task.Status.Execution.RuntimeSessionWorkspaceDigest = ""
			if _, _, err := d.recoveredValidationWorkspace(t.Context(), task, task.UID); err == nil {
				t.Fatal("Session-backed recovery accepted an empty workspace digest")
			}
		})
	}
}
