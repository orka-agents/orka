/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/publisher"
)

func TestSecurityPatchDecorationPreservesEditedPublisherBody(t *testing.T) {
	for _, edit := range []string{"append", "prepend", "replace description"} {
		t.Run(edit, func(t *testing.T) {
			var seenToken string
			server, pr := newPatchCommitServerWithPullRequest(t, nil, &seenToken)
			fixture := patchFixtureWithForgeSecret(t, "edited-body", server, true)
			switch edit {
			case "append":
				pr.body += "\n\nDeployment checklist: pending"
			case "prepend":
				pr.body = "Deployment checklist: pending\n\n" + pr.body
			case "replace description":
				pr.body = strings.Replace(pr.body, "Created by the Orka clean-room workspace publisher.", "Reviewed by the owner.", 1)
			}
			original := pr.body
			fixture.reconciler.decorateSecurityPatchPullRequest(context.Background(), fixture.scan, patchTaskForFixture(fixture, true), fixture.finding.ID, 42, 1, "")
			if pr.patched != nil || pr.body != original {
				t.Fatal("decoration overwrote an edited publisher body")
			}
		})
	}
}

func TestIngestPatchTaskBindsPublishedFileModes(t *testing.T) {
	modeDiff := strings.Replace(testPatchFullDiff, "\n--- ", "\nold mode 100644\nnew mode 100755\n--- ", 1)
	modified := repositoryScanCommitFileResponse{Filename: "app.py", Status: "modified", Additions: 1, Deletions: 1, Patch: "@@ -1 +1 @@\n-unsafe()\n+safe()"}
	added := repositoryScanCommitFileResponse{Filename: "app.py", Status: "added", Additions: 1, Patch: "@@ -0,0 +1 @@\n+safe()"}
	for _, tc := range []struct {
		name     string
		file     repositoryScanCommitFileResponse
		diff     string
		artifact string
		wantPass bool
	}{
		{name: "result preserves executable change", file: modified, diff: modeDiff, wantPass: true},
		{name: "artifact matches executable change", file: modified, diff: modeDiff, artifact: modeDiff, wantPass: true},
		{name: "artifact omits executable change", file: modified, diff: modeDiff, artifact: testPatchFullDiff},
		{name: "artifact misstates executable change", file: modified, diff: modeDiff, artifact: strings.Replace(modeDiff, "new mode 100755", "new mode 100644", 1)},
		{name: "new executable file", file: added, diff: "diff --git a/app.py b/app.py\nnew file mode 100755\n--- /dev/null\n+++ b/app.py\n@@ -0,0 +1 @@\n+safe()\n", wantPass: true},
		{name: "missing full diff", file: modified},
		{name: "full diff has different content", file: modified, diff: strings.Replace(modeDiff, "+safe()", "+other()", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			var seenToken string
			server, response := newPatchCommitServerWithPullRequest(t, []repositoryScanCommitFileResponse{tc.file}, &seenToken)
			response.commitDiff = tc.diff
			fixture := patchFixtureWithForgeSecret(t, "file-modes", server, true)
			if err := fixture.store.SaveResult(ctx, fixture.proposal.Namespace, fixture.proposal.TaskName, repositoryScanPatchResultEnvelope(fixture, []string{"app.py"})); err != nil {
				t.Fatal(err)
			}
			if tc.artifact != "" {
				savePatchArtifacts(t, fixture, tc.artifact, []string{"app.py"})
			}
			task := patchTaskForFixture(fixture, true)
			if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, task); err != nil {
				t.Fatal(err)
			}
			if !tc.wantPass {
				assertPatchIngestState(t, fixture, scanRunPhaseFailed, findingStateOpen)
				return
			}
			assertPatchIngestState(t, fixture, patchProposalStatusPROpened, findingStatePROpen)
			diffName, _ := patchArtifactNames(fixture.finding.ID)
			data, _, err := fixture.store.GetArtifact(ctx, task.Namespace, task.Name, diffName)
			if err != nil || string(data) != tc.diff {
				t.Fatal("verified artifact did not preserve the published file-mode transition")
			}
			if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, task); err != nil {
				t.Fatal(err)
			}
			assertPatchIngestState(t, fixture, patchProposalStatusPROpened, findingStatePROpen)
		})
	}
}

func TestIngestPatchTaskRetriesTransientPublishedDiffFailure(t *testing.T) {
	ctx := context.Background()
	var seenToken string
	server, response := newPatchCommitServerWithPullRequest(t, []repositoryScanCommitFileResponse{
		{Filename: "app.py", Status: "modified", Additions: 1, Deletions: 1, Patch: "@@ -1 +1 @@\n-unsafe()\n+safe()"},
	}, &seenToken)
	response.diffStatus = http.StatusServiceUnavailable
	fixture := patchFixtureWithForgeSecret(t, "diff-outage", server, true)
	savePatchArtifacts(t, fixture, testPatchFullDiff, []string{"app.py"})
	task := patchTaskForFixture(fixture, true)
	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, task); !errors.Is(err, errRepositoryScanPublishedCommitTransient) {
		t.Fatalf("ingestPatchTask() error = %v, want retryable diff failure", err)
	}
	assertPatchIngestState(t, fixture, scanRunPhasePending, findingStatePatchPending)
	response.diffStatus = http.StatusOK
	if err := fixture.reconciler.ingestPatchTask(ctx, fixture.scan, task); err != nil {
		t.Fatal(err)
	}
	assertPatchIngestState(t, fixture, patchProposalStatusPROpened, findingStatePROpen)
}

func TestSecurityPatchDecorationWithTaskPresentation(t *testing.T) {
	for _, test := range []struct {
		name                                                                                string
		legacy, missingTitle, session, editedTitle, editedBody, explicitTitle, explicitBody bool
	}{
		{name: "prompt default"},
		{name: "legacy default", legacy: true},
		{name: "missing finding title", missingTitle: true},
		{name: "session footer", session: true},
		{name: "human title", editedTitle: true},
		{name: "human body", editedBody: true},
		{name: "explicit Task title", explicitTitle: true},
		{name: "explicit Task body", explicitBody: true},
		{name: "explicit body without finding title", explicitBody: true, missingTitle: true},
		{name: "explicit body with edited title", explicitBody: true, editedTitle: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			var seenToken string
			server, pr := newPatchCommitServerWithPullRequest(t, nil, &seenToken)
			fixture := patchFixtureWithForgeSecret(t, "task-presentation", server, true)
			task := patchTaskForFixture(fixture, true)
			task.Spec.Prompt = "  Repair the redirect validation  \nDetails"
			if test.missingTitle {
				fixture.finding.Title = ""
				if err := fixture.store.UpsertFinding(ctx, fixture.finding); err != nil {
					t.Fatal(err)
				}
			}
			if test.explicitTitle {
				task.Spec.Workspace.PRTitle = "fix: authored security title"
			}
			if test.explicitBody {
				task.Spec.Workspace.PRBody = "Authored security summary"
			}
			marker := "<!-- orka.publisher.pr-intent.v1 key=sha256:" + strings.Repeat("c", 64) + " -->"
			if test.session {
				marker += "\n\n<!-- orka.publisher.pr-session.v1 key=sha256:" + strings.Repeat("d", 64) + " -->"
			}
			if !test.legacy {
				intent := withTaskPullRequestMetadata(publisher.PullRequestIntent{PublicationGeneration: 1}, task)
				pr.title, pr.body = intent.Title, intent.Description()+"\n\n"+marker
			}
			if test.editedTitle {
				pr.title = "Reviewed title"
			}
			if test.editedBody {
				pr.body += "\n\nReviewed summary"
			}
			originalTitle, originalBody := pr.title, pr.body
			fixture.reconciler.decorateSecurityPatchPullRequest(ctx, fixture.scan, task, fixture.finding.ID, 42, 1, "")
			if test.editedTitle || test.editedBody || test.explicitTitle || (test.explicitBody && test.missingTitle) {
				if pr.patched != nil || pr.title != originalTitle || pr.body != originalBody {
					t.Fatal("decoration overwrote authored PR presentation")
				}
				return
			}
			wantTitle := "fix(security): Patch target"
			if test.missingTitle {
				wantTitle = "Repair the redirect validation"
			}
			if pr.patched == nil || pr.title != wantTitle || !strings.Contains(pr.body, "Publication generation: 1") || !strings.HasSuffix(pr.body, "\n\n"+marker) {
				t.Fatal("decoration lost the finding title, generation, or reconciliation footer")
			}
			if test.explicitBody {
				if _, sentBody := pr.patched[repositoryScanPullRequestBodyField]; sentBody || pr.body != originalBody {
					t.Fatal("title-only decoration rewrote the authored body")
				}
			} else if !strings.Contains(pr.body, "Task: `"+task.Namespace+"/"+task.Name+"`") {
				t.Fatal("default decorated body lost the Task identity")
			}
			pr.patched = nil
			fixture.reconciler.decorateSecurityPatchPullRequest(ctx, fixture.scan, task, fixture.finding.ID, 42, 1, "")
			if pr.patched != nil {
				t.Fatal("decorated an already decorated PR again")
			}
		})
	}
}
