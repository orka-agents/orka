package publisher

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestRebaseBranchLinearSignedAndConflictRejection(t *testing.T) {
	ctx := context.Background()
	pub := newTestPublisher(t)
	keyPEM, sshPub := generateTestEd25519KeyPEM(t)

	// Create a bare remote repo with main -> base1, feature -> feat1 (off base1),
	// and advance main -> base2 (non-conflicting file).
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	runTestGit(t, "", nil, "init", "--bare", "-b", "main", remoteDir)

	work := filepath.Join(t.TempDir(), "work")
	runTestGit(t, "", nil, "clone", remoteDir, work)
	mustWriteFile(t, filepath.Join(work, "base.txt"), "v1\n", 0o644)
	runTestGit(t, work, fixedCommitEnvironment(), "add", "--all")
	runTestGit(t, work, fixedCommitEnvironment(), "commit", "-m", "base v1")
	runTestGit(t, work, nil, "push", "origin", "main")

	// Create feature branch off v1.
	runTestGit(t, work, nil, "checkout", "-b", "feat/linear")
	mustWriteFile(t, filepath.Join(work, "feat.txt"), "feature change\n", 0o644)
	runTestGit(t, work, fixedCommitEnvironment(), "add", "--all")
	runTestGit(t, work, fixedCommitEnvironment(), "commit", "-m", "feat: add feature")
	headOID := strings.TrimSpace(runTestGit(t, work, nil, "rev-parse", "HEAD"))
	runTestGit(t, work, nil, "push", "origin", "feat/linear")

	// Advance main with a non-conflicting commit.
	runTestGit(t, work, nil, "checkout", "main")
	mustWriteFile(t, filepath.Join(work, "other.txt"), "main advance\n", 0o644)
	runTestGit(t, work, fixedCommitEnvironment(), "add", "--all")
	runTestGit(t, work, fixedCommitEnvironment(), "commit", "-m", "chore: advance main")
	baseOID := strings.TrimSpace(runTestGit(t, work, nil, "rev-parse", "HEAD"))
	runTestGit(t, work, nil, "push", "origin", "main")

	repo := Repository{Provider: "github", ID: "github.com/orka-agents/test", URL: "file://" + remoteDir}
	result, err := pub.RebaseBranch(ctx, RebaseBranchRequest{
		OperationID:       "rebase-op-1",
		Target:            repo,
		TargetRef:         "refs/heads/feat/linear",
		BaseRef:           "refs/heads/main",
		BaseOID:           baseOID,
		HeadOID:           headOID,
		CommitAuthorName:  "Dynamo Bot",
		CommitAuthorEmail: "dynamo-bot@example.com",
		CommitTimestamp:   time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		SignOff:           true,
		SigningKey:        keyPEM,
	})
	if err != nil {
		t.Fatalf("RebaseBranch() error = %v", err)
	}
	if result.RebasedCount != 1 || result.NewHeadOID == headOID || result.NoChange {
		t.Fatalf("unexpected rebase result: %+v", result)
	}

	// Verify remote branch now has baseOID as parent and valid SSHSIG + DCO trailer.
	parent := strings.TrimSpace(runTestGit(t, "", nil, "--git-dir="+remoteDir, "rev-parse", result.NewHeadOID+"^"))
	if parent != baseOID {
		t.Fatalf("rebased commit parent = %s, want %s", parent, baseOID)
	}
	rawCommit := runTestGit(t, "", nil, "--git-dir="+remoteDir, "cat-file", "commit", result.NewHeadOID)
	if !strings.Contains(rawCommit, "gpgsig -----BEGIN SSH SIGNATURE-----") {
		t.Fatalf("expected SSH signature on rebased commit:\n%s", rawCommit)
	}
	if !strings.Contains(rawCommit, "Signed-off-by: Dynamo Bot <dynamo-bot@example.com>") {
		t.Fatalf("expected DCO trailer on rebased commit:\n%s", rawCommit)
	}
	if _, lookErr := exec.LookPath("ssh-keygen"); lookErr == nil {
		allowedSigners := filepath.Join(t.TempDir(), "allowed_signers")
		line := "dynamo-bot@example.com " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + "\n"
		if err := os.WriteFile(allowedSigners, []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = runTestGit(t, "", nil,
			"--git-dir="+remoteDir,
			"-c", "gpg.format=ssh",
			"-c", "gpg.ssh.allowedSignersFile="+allowedSigners,
			"verify-commit", "--raw", result.NewHeadOID,
		)
	}

	// Conflict case: advance main on feat.txt, then try rebasing a conflicting branch.
	runTestGit(t, work, nil, "checkout", "main")
	mustWriteFile(t, filepath.Join(work, "feat.txt"), "conflicting main content\n", 0o644)
	runTestGit(t, work, fixedCommitEnvironment(), "add", "--all")
	runTestGit(t, work, fixedCommitEnvironment(), "commit", "-m", "conflict on main")
	conflictBaseOID := strings.TrimSpace(runTestGit(t, work, nil, "rev-parse", "HEAD"))
	runTestGit(t, work, nil, "push", "origin", "main")

	_, conflictErr := pub.RebaseBranch(ctx, RebaseBranchRequest{
		OperationID: "rebase-op-conflict",
		Target:      repo,
		TargetRef:   "refs/heads/feat/linear",
		BaseRef:     "refs/heads/main",
		BaseOID:     conflictBaseOID,
		HeadOID:     result.NewHeadOID,
	})
	if !errors.Is(conflictErr, ErrRebaseRefused) {
		t.Fatalf("expected ErrRebaseRefused on merge conflict, got %v", conflictErr)
	}
	// Remote head must remain untouched.
	afterConflict := strings.TrimSpace(runTestGit(t, "", nil, "--git-dir="+remoteDir, "rev-parse", "refs/heads/feat/linear"))
	if afterConflict != result.NewHeadOID {
		t.Fatalf("conflicted rebase mutated remote: got %s, want %s", afterConflict, result.NewHeadOID)
	}
}
