package security

import (
	"testing"

	"github.com/orka-agents/orka/internal/store"
)

func TestSemanticIdentityIgnoresLineMovementButKeepsLegacyInstancesSeparate(t *testing.T) {
	first := &store.Finding{Fingerprint: "legacy-a", Category: "Path Traversal", Evidence: []store.FindingEvidenceRef{{Path: "internal/archive/extract.go", StartLine: 10, EndLine: 12, Symbol: "Extract"}}}
	second := &store.Finding{Fingerprint: "legacy-a", Category: "Path Traversal", Evidence: []store.FindingEvidenceRef{{Path: "internal/archive/extract.go", StartLine: 30, EndLine: 32, Symbol: "Extract"}}}
	left := DeriveSemanticIdentity("https://github.com/example/repo", first)
	right := DeriveSemanticIdentity("https://github.com/example/repo", second)
	if left.SemanticFingerprint != right.SemanticFingerprint {
		t.Fatalf("line movement changed semantic fingerprint: %q != %q", left.SemanticFingerprint, right.SemanticFingerprint)
	}
	third := *second
	third.Fingerprint = "legacy-b"
	if left.SemanticFingerprint == DeriveSemanticIdentity("https://github.com/example/repo", &third).SemanticFingerprint {
		t.Fatal("independent legacy instances merged")
	}
	if len(left.SemanticFingerprint) != len("sha256:")+64 || len(left.SemanticFindingID) != len("sf_")+64 {
		t.Fatalf("identity widths = %q / %q", left.SemanticFingerprint, left.SemanticFindingID)
	}
}

func TestOccurrenceAndObservationIDsAreFullWidthAndStable(t *testing.T) {
	occ := OccurrenceID("run_abc", "sha256:def")
	if occ != OccurrenceID("run_abc", "sha256:def") || len(occ) != len("occ_")+64 {
		t.Fatalf("occurrence ID = %q", occ)
	}
	obs := ObservationID("run_abc", "task-1", 2, "sha256:def", 3)
	if obs != ObservationID("run_abc", "task-1", 2, "sha256:def", 3) || len(obs) != len("obs_")+64 {
		t.Fatalf("observation ID = %q", obs)
	}
}

func TestProvisionalFindingIdentityIsRunAndPayloadScoped(t *testing.T) {
	runA := "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	runB := "run_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	groupA := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\x00sha256:1111111111111111111111111111111111111111111111111111111111111111"
	groupB := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\x00sha256:2222222222222222222222222222222222222222222222222222222222222222"
	if ProvisionalFindingID(runA, groupA) == ProvisionalFindingID(runA, groupB) {
		t.Fatal("different noncanonical payload groups reused one public finding ID")
	}
	if ProvisionalFindingID(runA, groupA) == ProvisionalFindingID(runB, groupA) {
		t.Fatal("different runs reused one provisional public finding ID")
	}
	if ProvisionalFindingFingerprint(runA, groupA) == ProvisionalFindingFingerprint(runB, groupA) {
		t.Fatal("different runs reused one provisional compatibility fingerprint")
	}
}

func TestSemanticIdentityCanonicalizesBracketedIPv6SCPRepository(t *testing.T) {
	finding := &store.Finding{Fingerprint: "legacy", Category: "Path Traversal", Evidence: []store.FindingEvidenceRef{{Path: "internal/archive.go", Symbol: "Extract"}}}
	scp := DeriveSemanticIdentity("git@[2001:DB8::1]:/Team/Repo.git", finding)
	sshURL := DeriveSemanticIdentity("ssh://git@[2001:db8::1]/Team/Repo.git", finding)
	if scp.SemanticFingerprint != sshURL.SemanticFingerprint {
		t.Fatal("bracketed IPv6 absolute SCP and URL repositories produced different semantic identities")
	}
	relative := DeriveSemanticIdentity("git@[2001:DB8::1]:Team/Repo.git", finding)
	if scp.SemanticFingerprint == relative.SemanticFingerprint {
		t.Fatal("relative and absolute bracketed IPv6 SCP repositories collapsed")
	}
	differentHost := DeriveSemanticIdentity("git@[2001:db8::2]:/Team/Repo.git", finding)
	if scp.SemanticFingerprint == differentHost.SemanticFingerprint {
		t.Fatal("distinct bracketed IPv6 SCP repositories collapsed to one semantic identity")
	}
}

func TestSemanticIdentityCanonicalizesUsernameLessSCPBeforeTransportSuffix(t *testing.T) {
	finding := &store.Finding{Fingerprint: "legacy", Category: "Path Traversal", Evidence: []store.FindingEvidenceRef{{Path: "internal/archive.go", Symbol: "Extract"}}}
	scp := DeriveSemanticIdentity("git.example:/Team/Repo.git?mirror=https://mirror.example/Team/Repo.git#fragment", finding)
	sshURL := DeriveSemanticIdentity("ssh://git.example/Team/Repo.git", finding)
	if scp.SemanticFingerprint != sshURL.SemanticFingerprint {
		t.Fatal("username-less absolute SCP and SSH URL repositories produced different semantic identities")
	}
	relative := DeriveSemanticIdentity("git.example:Team/Repo.git", finding)
	if relative.SemanticFingerprint == scp.SemanticFingerprint {
		t.Fatal("relative and absolute username-less SCP repositories collapsed")
	}
}

func TestSemanticIdentityBindsSSHUserForHomeRelativeRepositories(t *testing.T) {
	finding := &store.Finding{Fingerprint: "legacy", Category: "Path Traversal", Evidence: []store.FindingEvidenceRef{{Path: "internal/archive.go", Symbol: "Extract"}}}
	aliceSCP := DeriveSemanticIdentity("alice@git.example:Team/Repo.git", finding)
	aliceSSH := DeriveSemanticIdentity("ssh://alice@git.example/~/Team/Repo.git", finding)
	if aliceSCP.SemanticFingerprint != aliceSSH.SemanticFingerprint {
		t.Fatal("equivalent SCP and SSH home-relative repositories for one user produced different semantic identities")
	}
	bob := DeriveSemanticIdentity("ssh://bob@git.example/~/Team/Repo.git", finding)
	if aliceSCP.SemanticFingerprint == bob.SemanticFingerprint {
		t.Fatal("home-relative repositories for distinct SSH users collapsed to one semantic identity")
	}
}

func TestSemanticIdentityPreservesSCPPathWhitespace(t *testing.T) {
	finding := &store.Finding{Fingerprint: "legacy", Category: "Path Traversal", Evidence: []store.FindingEvidenceRef{{Path: "internal/archive.go", Symbol: "Extract"}}}
	canonical := DeriveSemanticIdentity("git.example:Team/Repo.git", finding)
	withWhitespace := DeriveSemanticIdentity("git.example:Team/Repo.git ", finding)
	if canonical.SemanticFingerprint == withWhitespace.SemanticFingerprint {
		t.Fatal("significant SCP path whitespace collapsed to one semantic identity")
	}
}

func TestSemanticIdentitySeparatesIPv6HostAndPortAuthority(t *testing.T) {
	finding := &store.Finding{Fingerprint: "legacy", Category: "Path Traversal", Evidence: []store.FindingEvidenceRef{{Path: "internal/archive.go", Symbol: "Extract"}}}
	withPort := DeriveSemanticIdentity("https://[2001:db8::1]:443/Team/Repo.git", finding)
	addressEndingInPort := DeriveSemanticIdentity("https://[2001:db8::1:443]/Team/Repo.git", finding)
	if withPort.SemanticFingerprint == addressEndingInPort.SemanticFingerprint {
		t.Fatal("semantic identity collapsed an IPv6 host/port authority with a distinct IPv6 host")
	}
}

func TestSemanticIdentityCanonicalizesCredentialedRepositoryURL(t *testing.T) {
	finding := &store.Finding{Fingerprint: "legacy", Category: "Path Traversal", Evidence: []store.FindingEvidenceRef{{Path: "internal/archive.go", Symbol: "Extract"}}}
	plain := DeriveSemanticIdentity("https://git.example/Team/Repo.git", finding)
	credentialed := DeriveSemanticIdentity("https://"+"alice"+":"+"example"+"@git.example/Team/Repo.git?mode=read", finding)
	if plain.SemanticFingerprint != credentialed.SemanticFingerprint {
		t.Fatal("credential/query changes split semantic identity")
	}
	caseDistinct := DeriveSemanticIdentity("https://git.example/team/Repo.git", finding)
	if plain.SemanticFingerprint == caseDistinct.SemanticFingerprint {
		t.Fatal("case-distinct repository coordinates collapsed")
	}
}
