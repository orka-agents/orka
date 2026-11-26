package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/publisher"
	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
	"github.com/orka-agents/orka/internal/store"
)

type stubBranchRebaser struct {
	got publisherservice.BranchRebaseRequest
	res publisherservice.BranchRebaseResponse
	err error
}

func (s *stubBranchRebaser) RebaseBranch(_ context.Context, req publisherservice.BranchRebaseRequest) (publisherservice.BranchRebaseResponse, error) {
	s.got = req
	return s.res, s.err
}

func TestRepositoryMonitorUpdateBranchRebaseStrategy(t *testing.T) {
	ctx := context.Background()
	db := setupControllerSQLiteStore(t)

	githubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/compare/"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "behind"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer githubServer.Close()

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = corev1alpha1.AddToScheme(scheme)
	monitor, secret := repositoryMonitorInventoryTestObjects("rebase-strategy")
	monitor.Spec.ReadCredentialRef = &corev1.LocalObjectReference{Name: "read-cred"}
	monitor.Spec.PublicationReadCredentialRef = &corev1.LocalObjectReference{Name: "pub-read-cred"}
	monitor.Spec.PublicationCredentialRef = &corev1.LocalObjectReference{Name: "pub-write-cred"}
	monitor.Spec.ForgeCredentialRef = &corev1.LocalObjectReference{Name: secret.Name}
	monitor.Spec.Repair.Enabled = true
	monitor.Spec.Repair.UpdateBranchStrategy = repositoryMonitorUpdateBranchStrategyRebase
	monitor.Spec.Commit = corev1alpha1.RepositoryMonitorCommitSpec{
		AuthorName:           "Dynamo Bot",
		AuthorEmail:          "dynamo-bot@example.com",
		SignOff:              true,
		SigningCredentialRef: &corev1.LocalObjectReference{Name: "sign-cred"},
	}

	rebaser := &stubBranchRebaser{
		res: publisherservice.BranchRebaseResponse{
			OperationID: "op",
			Result: publisher.RebaseBranchResult{
				OperationID:  "op",
				PreviousOID:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				BaseOID:      "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				NewHeadOID:   "cccccccccccccccccccccccccccccccccccccccc",
				RebasedCount: 2,
			},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(monitor, secret).Build()
	r := &RepositoryMonitorReconciler{
		Client:           cl,
		Scheme:           scheme,
		Store:            db,
		GitHubAPIBaseURL: githubServer.URL,
		BranchRebaser:    rebaser,
	}

	pr := repositoryMonitorPullRequest{
		Number:         51,
		HeadSHA:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseSHA:        "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		HeadBranch:     "feat/rebase-me",
		BaseBranch:     "main",
		HeadRepo:       "orka-agents/orka",
		MergeableState: repositoryMonitorMergeableStateBehind,
	}
	cmd := &store.CommandEvent{
		ID:      "cmd-rebase-51",
		Repo:    "orka-agents/orka",
		Intent:  repositoryMonitorCommandIntentUpdateBranch,
		HeadSHA: pr.HeadSHA,
	}
	item := &store.MonitorItem{
		MonitorNamespace: monitor.Namespace,
		MonitorName:      monitor.Name,
		Kind:             repositoryMonitorPullRequestKind,
		Number:           pr.Number,
		HeadSHA:          pr.HeadSHA,
	}

	handled, created, err := r.tryProcessPullRequestUpdateBranchCommand(ctx, monitor, &store.MonitorRun{ID: "run-1"}, cmd, "orka-agents", "orka", pr, item)
	if err != nil || !handled || created != 0 {
		t.Fatalf("tryProcessPullRequestUpdateBranchCommand() = (%v, %d, %v)", handled, created, err)
	}
	if rebaser.got.Request.BaseOID != pr.BaseSHA || rebaser.got.Request.HeadOID != pr.HeadSHA || !rebaser.got.Request.SignOff || rebaser.got.Request.CommitAuthorEmail != "dynamo-bot@example.com" {
		t.Fatalf("unexpected rebase request: %+v", rebaser.got)
	}
	if rebaser.got.CredentialRef == nil || rebaser.got.CredentialRef.Name != "pub-write-cred" || rebaser.got.CredentialRef.Role != publisherservice.CredentialRoleTargetWrite {
		t.Fatalf("unexpected write credential ref: %+v", rebaser.got.CredentialRef)
	}
	if rebaser.got.SigningCredentialRef == nil || rebaser.got.SigningCredentialRef.Name != "sign-cred" || rebaser.got.SigningCredentialRef.Role != publisherservice.CredentialRoleCommitSign {
		t.Fatalf("unexpected signing credential ref: %+v", rebaser.got.SigningCredentialRef)
	}

	rebaser.err = publisher.ErrCASRejected
	cmdConflict := &store.CommandEvent{
		ID:      "cmd-rebase-52",
		Repo:    "orka-agents/orka",
		Intent:  repositoryMonitorCommandIntentUpdateBranch,
		HeadSHA: pr.HeadSHA,
	}
	handled, _, err = r.tryProcessPullRequestUpdateBranchCommand(ctx, monitor, &store.MonitorRun{ID: "run-2"}, cmdConflict, "orka-agents", "orka", pr, item)
	if err != nil || !handled {
		t.Fatalf("conflict rebase = (%v, %v)", handled, err)
	}
	if item.SkipReason != repositoryMonitorUpdateBranchFailure {
		t.Fatalf("item.SkipReason = %q, want %q", item.SkipReason, repositoryMonitorUpdateBranchFailure)
	}
}
