package publisher

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func generateTestEd25519KeyPEM(t *testing.T) ([]byte, ssh.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "test-signing-key")
	if err != nil {
		t.Fatalf("marshal openssh private key: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("convert public key: %v", err)
	}
	return pem.EncodeToMemory(block), sshPub
}

func TestValidateCommitIdentity(t *testing.T) {
	name, email, err := ValidateCommitIdentity("", "")
	if err != nil || name != CommitAuthorName || email != CommitAuthorEmail {
		t.Fatalf("defaults = (%q, %q, %v)", name, email, err)
	}
	name, email, err = ValidateCommitIdentity("Orka Bot", "bot@example.com")
	if err != nil || name != "Orka Bot" || email != "bot@example.com" {
		t.Fatalf("custom = (%q, %q, %v)", name, email, err)
	}
	for _, tc := range []struct {
		name, email string
	}{
		{"Bad<Name>", "bot@example.com"},
		{"Bad\nName", "bot@example.com"},
		{"Orka Bot", "bad email@example.com"},
		{"Orka Bot", "bad<email>@example.com"},
	} {
		if _, _, err := ValidateCommitIdentity(tc.name, tc.email); err == nil {
			t.Fatalf("ValidateCommitIdentity(%q, %q) expected error", tc.name, tc.email)
		}
	}
}

func TestPrepareSignedCommitAndCustomIdentity(t *testing.T) {
	ctx := context.Background()
	fixture := newRepositoryFixture(t, false)
	pub := newTestPublisher(t)
	keyPEM, sshPub := generateTestEd25519KeyPEM(t)

	req := fixture.prepareRequest("pub-signed-1", "op-prepare-signed-1", RemoteRef{Absent: true})
	req.CommitAuthorName = "Dynamo Bot"
	req.CommitAuthorEmail = "dynamo-bot@example.com"
	req.CommitMessage = "fix: signed repair\n\nSigned-off-by: Dynamo Bot <dynamo-bot@example.com>\n"
	req.SigningKey = keyPEM

	one, err := pub.Prepare(ctx, req)
	if err != nil {
		t.Fatalf("Prepare signed commit: %v", err)
	}
	reqRetry := req
	reqRetry.OperationID = "op-prepare-signed-2"
	two, err := pub.Prepare(ctx, reqRetry)
	if err != nil {
		t.Fatalf("Prepare retry: %v", err)
	}
	if one.CommitOID != two.CommitOID || one.RequestDigest != two.RequestDigest {
		t.Fatalf("non-deterministic signed prepare: one=%+v two=%+v", one, two)
	}

	pub2 := newTestPublisher(t)
	three, err := pub2.Prepare(ctx, req)
	if err != nil {
		t.Fatalf("Prepare second publisher: %v", err)
	}
	if three.CommitOID != one.CommitOID {
		t.Fatalf("Ed25519 signed CommitOID not deterministic across publishers: %s vs %s", three.CommitOID, one.CommitOID)
	}

	otherKeyPEM, _ := generateTestEd25519KeyPEM(t)
	rotated := req
	rotated.SigningKey = otherKeyPEM
	if _, err := pub.Prepare(ctx, rotated); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("expected ErrIdempotencyConflict on rotated signing key, got %v", err)
	}

	verifyDir := t.TempDir()
	verifyRepo := filepath.Join(verifyDir, "verify.git")
	runTestGit(t, verifyDir, nil, "init", "--bare", verifyRepo)
	runTestGit(t, verifyDir, nil, "--git-dir="+verifyRepo, "bundle", "unbundle", one.BundlePath)
	runTestGit(t, verifyDir, nil, "--git-dir="+verifyRepo, "fsck", "--strict")

	identity := runTestGit(t, verifyDir, nil, "--git-dir="+verifyRepo, "show", "-s", "--format=%an|%ae|%cn|%ce", one.CommitOID)
	if strings.TrimSpace(identity) != "Dynamo Bot|dynamo-bot@example.com|Dynamo Bot|dynamo-bot@example.com" {
		t.Fatalf("unexpected commit identity: %q", identity)
	}
	rawCommit := runTestGit(t, verifyDir, nil, "--git-dir="+verifyRepo, "cat-file", "commit", one.CommitOID)
	if !strings.Contains(rawCommit, "gpgsig -----BEGIN SSH SIGNATURE-----") {
		t.Fatalf("expected gpgsig SSH signature header in commit:\n%s", rawCommit)
	}
	if !strings.Contains(rawCommit, "Signed-off-by: Dynamo Bot <dynamo-bot@example.com>") {
		t.Fatalf("expected DCO Signed-off-by trailer in commit:\n%s", rawCommit)
	}

	if _, lookErr := exec.LookPath("ssh-keygen"); lookErr == nil {
		allowedSigners := filepath.Join(verifyDir, "allowed_signers")
		line := "dynamo-bot@example.com " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + "\n"
		if err := os.WriteFile(allowedSigners, []byte(line), 0o600); err != nil {
			t.Fatalf("write allowed_signers: %v", err)
		}
		_ = runTestGit(t, verifyDir, nil,
			"--git-dir="+verifyRepo,
			"-c", "gpg.format=ssh",
			"-c", "gpg.ssh.allowedSignersFile="+allowedSigners,
			"verify-commit", "--raw", one.CommitOID,
		)
	}
}

func TestParseSSHSigningKeyRejectsPassphrasedKey(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "encrypted", []byte("secret-passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ParseSSHSigningKey(pem.EncodeToMemory(block)); err == nil {
		t.Fatalf("expected error for passphrase-protected key")
	}
}
