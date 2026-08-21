package security

import (
	"bytes"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const testCanonicalRepositoryHost = "git.example"

func TestRunUIDAndAliases(t *testing.T) {
	uid, err := newRunUIDFrom(bytes.NewReader(bytes.Repeat([]byte{0xab}, RunUIDDigestBytes)))
	if err != nil {
		t.Fatalf("newRunUIDFrom() error = %v", err)
	}
	if !ValidRunUID(uid) || len(uid) != len(RunUIDPrefix)+RunUIDDigestBytes*2 {
		t.Fatalf("uid = %q", uid)
	}
	if got := PublicScanRunID(uid); got != PublicScanRunID(uid) || len(got) != len("scan_")+12 {
		t.Fatalf("public alias = %q", got)
	}
	first := ScanStageTaskNameForRun("repo", "manual", StageReview, "slice-a", uid)
	second := ScanStageTaskNameForRun("repo", "manual", StageReview, "slice-a", uid)
	if first != second || len(first) > 63 {
		t.Fatalf("task names = %q/%q", first, second)
	}
}

func TestRequestAndResolvedKeysAreStableAndBound(t *testing.T) {
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("uid-1"), Generation: 3},
		Spec: corev1alpha1.RepositoryScanSpec{
			Branch: "main", Ref: "refs/tags/v1", SubPath: "services/api",
			AnalysisIsolationPolicy: "require-hardened",
			CompletionPolicy:        "validated",
		},
	}
	first := RequestIdempotencyKey(scan, "manual", "base", "", "policy")
	if first == "" || first != RequestIdempotencyKey(scan, "manual", "base", "", "policy") {
		t.Fatalf("request key = %q", first)
	}
	scan.Generation++
	if first == RequestIdempotencyKey(scan, "manual", "base", "", "policy") {
		t.Fatal("request key did not bind generation")
	}
	headCommit := strings.Repeat("A", 40)
	withHead := RequestIdempotencyKey(scan, "manual", "base", "  "+headCommit+"  ", "policy")
	if withHead != RequestIdempotencyKey(scan, "manual", "base", strings.ToLower(headCommit), "policy") {
		t.Fatal("request key did not normalize the explicit head commit")
	}
	if withHead == RequestIdempotencyKey(scan, "manual", "base", strings.Repeat("b", 40), "policy") {
		t.Fatal("request key did not bind the explicit head commit")
	}
	resolved := ResolvedTargetKey("repo-id", "a", "b", "services/api", "policy")
	if resolved == ResolvedTargetKey("repo-id", "a", "c", "services/api", "policy") {
		t.Fatal("resolved target key did not bind head commit")
	}
}

func TestTargetKeysPreserveSubPathWhitespaceAndNormalizeSeparators(t *testing.T) {
	const (
		targetID     = "repo-id"
		baseCommit   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		headCommit   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		policyDigest = "policy"
	)

	canonical := ResolvedTargetKey(targetID, baseCommit, headCommit, "services/api", policyDigest)
	if got := ResolvedTargetKey(targetID, baseCommit, headCommit, `/services\api/`, policyDigest); got != canonical {
		t.Fatalf("separator-normalized resolved key = %q, want %q", got, canonical)
	}
	for _, distinct := range []string{" services/api", "services/api ", "services /api"} {
		if got := ResolvedTargetKey(targetID, baseCommit, headCommit, distinct, policyDigest); got == canonical {
			t.Fatalf("resolved target key collapsed significant subpath whitespace in %q", distinct)
		}
	}

	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{UID: types.UID("uid-1"), Generation: 1},
		Spec:       corev1alpha1.RepositoryScanSpec{SubPath: "services/api"},
	}
	requestKey := RequestIdempotencyKey(scan, "manual", baseCommit, "", policyDigest)
	separatorVariant := scan.DeepCopy()
	separatorVariant.Spec.SubPath = `/services\api/`
	if got := RequestIdempotencyKey(separatorVariant, "manual", baseCommit, "", policyDigest); got != requestKey {
		t.Fatalf("separator-normalized request key = %q, want %q", got, requestKey)
	}
	whitespaceVariant := scan.DeepCopy()
	whitespaceVariant.Spec.SubPath = "services/api "
	if got := RequestIdempotencyKey(whitespaceVariant, "manual", baseCommit, "", policyDigest); got == requestKey {
		t.Fatal("request key collapsed significant subpath whitespace")
	}
}

func TestSCPRepositoryCoordinateParsesBracketedIPv6Authority(t *testing.T) {
	const repoURL = "git@[2001:DB8::1]:Team/Repo.git"
	if got := repositoryServerOrigin(repoURL); got != "[2001:db8::1]" {
		t.Fatalf("repositoryServerOrigin() = %q, want bracketed IPv6 authority", got)
	}
	want := encodeRepositoryURLCoordinate(repositoryURLCoordinate{
		Kind: "remote", Transport: "ssh", Authority: "[2001:db8::1]",
		Rooted: false, Username: "git", Path: "Team/Repo",
	})
	if got := canonicalRepositoryURLCoordinate(repoURL); got != want {
		t.Fatalf("canonicalRepositoryURLCoordinate() = %q, want %q", got, want)
	}
}

func TestRepositoryAuthorityCanonicalizesEquivalentIPv6Spellings(t *testing.T) {
	const (
		expanded   = "2001:0DB8:0000:0000:0000:0000:0000:0001"
		compressed = "2001:db8::1"
	)

	want := "[2001:db8::1]:8443"
	if got := canonicalRepositoryAuthority(expanded, "8443"); got != want {
		t.Fatalf("canonicalRepositoryAuthority(expanded) = %q, want %q", got, want)
	}
	if got := canonicalRepositoryAuthority(compressed, "8443"); got != want {
		t.Fatalf("canonicalRepositoryAuthority(compressed) = %q, want %q", got, want)
	}
	if canonicalRepositoryAuthority(expanded, "8443") == canonicalRepositoryAuthority(compressed, "9443") {
		t.Fatal("IPv6 authority canonicalization discarded the port")
	}

	expandedSCP := canonicalRepositoryURLCoordinate("git@[2001:0DB8:0000:0000:0000:0000:0000:0001]:Team/Repo.git")
	compressedSCP := canonicalRepositoryURLCoordinate("git@[2001:db8::1]:Team/Repo.git")
	if expandedSCP != compressedSCP {
		t.Fatalf("equivalent IPv6 SCP coordinates differ: %q != %q", expandedSCP, compressedSCP)
	}

	expandedScan := &corev1alpha1.RepositoryScan{Spec: corev1alpha1.RepositoryScanSpec{
		Provider: "git", Owner: "Team", Repository: "Repo",
		RepoURL: "https://[2001:0DB8:0000:0000:0000:0000:0000:0001]:8443/Team/Repo.git",
	}}
	compressedScan := expandedScan.DeepCopy()
	compressedScan.Spec.RepoURL = "https://[2001:db8::1]:8443/Team/Repo.git"
	if RepositoryTargetID(expandedScan) != RepositoryTargetID(compressedScan) {
		t.Fatal("RepositoryTargetID split equivalent IPv6 host spellings")
	}
}

func TestRepositoryAuthorityPreservesHostnameAndIPv6ZoneIdentity(t *testing.T) {
	if got := canonicalRepositoryAuthority("Git.Example", ""); got != testCanonicalRepositoryHost {
		t.Fatalf("canonicalRepositoryAuthority(hostname) = %q, want hostname case normalization", got)
	}

	const expanded = "FE80:0000:0000:0000:0000:0000:0000:0001%en0"
	const compressed = "fe80::1%en0"
	want := "[fe80::1%en0]:22"
	if got := canonicalRepositoryAuthority(expanded, "22"); got != want {
		t.Fatalf("canonicalRepositoryAuthority(expanded zone) = %q, want %q", got, want)
	}
	if got := canonicalRepositoryAuthority(compressed, "22"); got != want {
		t.Fatalf("canonicalRepositoryAuthority(compressed zone) = %q, want %q", got, want)
	}
	if canonicalRepositoryAuthority(compressed, "22") == canonicalRepositoryAuthority("fe80::1%en1", "22") {
		t.Fatal("IPv6 authority canonicalization discarded the zone")
	}
	if canonicalRepositoryAuthority(compressed, "22") == canonicalRepositoryAuthority("fe80::1%EN0", "22") {
		t.Fatal("IPv6 authority canonicalization case-folded the zone identifier")
	}
}

func TestSCPRepositoryCoordinateParsesUsernameLessOriginBeforeTransportSuffix(t *testing.T) {
	const relative = "git.example:Team/Repo.git"
	const decorated = relative + "?mirror=https://mirror.example/Team/Repo.git#fragment"
	if got := repositoryServerOrigin(decorated); got != testCanonicalRepositoryHost {
		t.Fatalf("repositoryServerOrigin() = %q, want username-less SCP origin", got)
	}
	want := encodeRepositoryURLCoordinate(repositoryURLCoordinate{
		Kind: "remote", Transport: "ssh", Authority: testCanonicalRepositoryHost,
		Rooted: false, Path: "Team/Repo",
	})
	if got := canonicalRepositoryURLCoordinate(decorated); got != want {
		t.Fatalf("canonicalRepositoryURLCoordinate() = %q, want %q", got, want)
	}

	absolute := canonicalRepositoryURLCoordinate("git.example:/Team/Repo.git")
	sshURL := canonicalRepositoryURLCoordinate("ssh://git.example/Team/Repo.git")
	if absolute != sshURL {
		t.Fatalf("absolute SCP coordinate %q differs from rooted SSH URL %q", absolute, sshURL)
	}
	if relativeCoordinate := canonicalRepositoryURLCoordinate(relative); relativeCoordinate == absolute {
		t.Fatal("relative and absolute SCP repository coordinates collapsed")
	}
}

func TestSCPRepositoryCoordinateKeepsAtSignInUsernameLessPath(t *testing.T) {
	const repoURL = "git.example:Team/Repo@archive.git"

	username, host, repoPath, ok := splitSCPRepository(repoURL)
	if !ok || username != "" || host != testCanonicalRepositoryHost || repoPath != "Team/Repo@archive.git" {
		t.Fatalf("splitSCPRepository() = (%q, %q, %q, %v), want username-less host-scoped repository", username, host, repoPath, ok)
	}
	if got := repositoryServerOrigin(repoURL); got != testCanonicalRepositoryHost {
		t.Fatalf("repositoryServerOrigin() = %q, want username-less SCP origin", got)
	}
	want := encodeRepositoryURLCoordinate(repositoryURLCoordinate{
		Kind: "remote", Transport: "ssh", Authority: testCanonicalRepositoryHost,
		Rooted: false, Path: "Team/Repo@archive",
	})
	if got := canonicalRepositoryURLCoordinate(repoURL); got != want {
		t.Fatalf("canonicalRepositoryURLCoordinate() = %q, want %q", got, want)
	}
}

func TestRawRepositoryCoordinatePreservesPathRootedness(t *testing.T) {
	relative := canonicalRepositoryURLCoordinate("workspace/Repo.git")
	rooted := canonicalRepositoryURLCoordinate("/workspace/Repo.git")
	if relative == rooted {
		t.Fatal("raw relative and rooted repository paths collapsed")
	}

	wantRelative := encodeRepositoryURLCoordinate(repositoryURLCoordinate{
		Kind: "raw", Path: "workspace/Repo",
	})
	if relative != wantRelative {
		t.Fatalf("relative raw coordinate = %q, want %q", relative, wantRelative)
	}
	wantRooted := encodeRepositoryURLCoordinate(repositoryURLCoordinate{
		Kind: "raw", Rooted: true, Path: "workspace/Repo",
	})
	if rooted != wantRooted {
		t.Fatalf("rooted raw coordinate = %q, want %q", rooted, wantRooted)
	}

	relativeScan := &corev1alpha1.RepositoryScan{Spec: corev1alpha1.RepositoryScanSpec{
		Provider: "git", RepoURL: "workspace/Repo.git",
	}}
	rootedScan := relativeScan.DeepCopy()
	rootedScan.Spec.RepoURL = "/workspace/Repo.git"
	if RepositoryTargetID(relativeScan) == RepositoryTargetID(rootedScan) {
		t.Fatal("RepositoryTargetID collapsed raw relative and rooted repository paths")
	}
}

func TestRawRepositoryCoordinatePreservesPathNormalization(t *testing.T) {
	for _, test := range []struct {
		name      string
		canonical string
		variant   string
	}{
		{name: "relative", canonical: "workspace/Repo", variant: "workspace/Repo.git///"},
		{name: "rooted", canonical: "/workspace/Repo", variant: "///workspace/Repo.git///"},
	} {
		t.Run(test.name, func(t *testing.T) {
			want := canonicalRepositoryURLCoordinate(test.canonical)
			if got := canonicalRepositoryURLCoordinate(test.variant); got != want {
				t.Fatalf("normalized raw coordinate = %q, want %q", got, want)
			}
		})
	}
}

func TestRepositoryTargetIDProviderCoordinatesIgnoreRawPathRootedness(t *testing.T) {
	relative := &corev1alpha1.RepositoryScan{Spec: corev1alpha1.RepositoryScanSpec{
		Provider: "github", Owner: "Team", Repository: "Repo", RepoURL: "workspace/Repo.git",
	}}
	rooted := relative.DeepCopy()
	rooted.Spec.RepoURL = "/workspace/Repo.git"
	if RepositoryTargetID(relative) != RepositoryTargetID(rooted) {
		t.Fatal("provider-authoritative repository identity changed with raw path rootedness")
	}
}

func TestRepositoryURLCoordinateSeparatesTransportAndResourceSignificantEscapes(t *testing.T) {
	httpsCoordinate := canonicalRepositoryURLCoordinate("https://git.example/Team/Repo.git")
	sshCoordinate := canonicalRepositoryURLCoordinate("ssh://git.example/Team/Repo.git")
	if httpsCoordinate == sshCoordinate {
		t.Fatal("HTTPS and SSH repository coordinates collapsed")
	}

	escapedSlash := canonicalRepositoryURLCoordinate("https://git.example/Team%2fRepo.git")
	literalSlash := canonicalRepositoryURLCoordinate("https://git.example/Team/Repo.git")
	if escapedSlash == literalSlash {
		t.Fatal("escaped and literal repository path separators collapsed")
	}
	if uppercaseEscape := canonicalRepositoryURLCoordinate("https://git.example/Team%2FRepo.git"); escapedSlash != uppercaseEscape {
		t.Fatalf("percent-escape case changed coordinate: %q != %q", escapedSlash, uppercaseEscape)
	}
}

func TestRepositoryURLCoordinatePreservesValidTransportAndPathNormalization(t *testing.T) {
	httpsCanonical := canonicalRepositoryURLCoordinate("https://git.example/Team/Repo")
	httpsVariant := canonicalRepositoryURLCoordinate("HTTPS://Git.Example:0443/Team/%52epo.git/")
	if httpsCanonical != httpsVariant {
		t.Fatalf("equivalent HTTPS coordinates differ: %q != %q", httpsCanonical, httpsVariant)
	}

	sshCanonical := canonicalRepositoryURLCoordinate("ssh://alice@git.example/~/Team/Repo")
	sshVariant := canonicalRepositoryURLCoordinate("git+ssh://alice@GIT.EXAMPLE:022/~/Team/Repo.git/")
	if sshCanonical != sshVariant {
		t.Fatalf("equivalent SSH coordinates differ: %q != %q", sshCanonical, sshVariant)
	}
}

func TestSSHHomeRelativeDetectionPreservesEscapedSlash(t *testing.T) {
	const escapedSlash = "ssh://alice@git.example/%2F~/Team/Repo.git"
	const literalHome = "ssh://alice@git.example/~/Team/Repo.git"

	if got := repositoryTargetOrigin(escapedSlash); got != testCanonicalRepositoryHost {
		t.Fatalf("repositoryTargetOrigin(escaped slash) = %q, want rooted host origin", got)
	}
	if got := repositoryTargetOrigin(literalHome); got != "git.example/~user/alice" {
		t.Fatalf("repositoryTargetOrigin(literal home) = %q, want user-bound home origin", got)
	}
	if canonicalRepositoryURLCoordinate(escapedSlash) == canonicalRepositoryURLCoordinate(literalHome) {
		t.Fatal("escaped path separator collapsed onto literal SSH home-relative path")
	}

	alice := &corev1alpha1.RepositoryScan{Spec: corev1alpha1.RepositoryScanSpec{
		Provider: "git", Owner: "Team", Repository: "Repo", RepoURL: escapedSlash,
	}}
	bob := alice.DeepCopy()
	bob.Spec.RepoURL = "ssh://bob@git.example/%2F~/Team/Repo.git"
	if RepositoryTargetID(alice) != RepositoryTargetID(bob) {
		t.Fatal("escaped path separator was treated as a user-bound SSH home-relative path")
	}
}

func TestRepositoryTargetIDSeparatesTransportAndResourceSignificantEscapesWithoutProviderCoordinates(t *testing.T) {
	httpsScan := &corev1alpha1.RepositoryScan{Spec: corev1alpha1.RepositoryScanSpec{
		Provider: "git", RepoURL: "https://git.example/Team/Repo.git",
	}}
	sshScan := httpsScan.DeepCopy()
	sshScan.Spec.RepoURL = "ssh://git.example/Team/Repo.git"
	if RepositoryTargetID(httpsScan) == RepositoryTargetID(sshScan) {
		t.Fatal("RepositoryTargetID collapsed HTTPS and SSH repositories")
	}

	escapedSlash := httpsScan.DeepCopy()
	escapedSlash.Spec.RepoURL = "https://git.example/Team%2FRepo.git"
	if RepositoryTargetID(httpsScan) == RepositoryTargetID(escapedSlash) {
		t.Fatal("RepositoryTargetID collapsed escaped and literal repository path separators")
	}
}

func TestRepositoryTargetIDProviderCoordinatesRetainTransportAndPathEquivalence(t *testing.T) {
	httpsScan := &corev1alpha1.RepositoryScan{Spec: corev1alpha1.RepositoryScanSpec{
		Provider: "github", Owner: "Team", Repository: "Repo",
		RepoURL: "https://git.example:443/unrelated%2Fpath.git",
	}}
	sshScan := httpsScan.DeepCopy()
	sshScan.Spec.RepoURL = "ssh://git.example:22/another/path.git"
	if RepositoryTargetID(httpsScan) != RepositoryTargetID(sshScan) {
		t.Fatal("provider-authoritative coordinates changed across equivalent server transports and URL paths")
	}
}

func TestSCPRepositoryCoordinateTagsRootednessAndUsernameWithoutPathSentinelCollisions(t *testing.T) {
	relative := canonicalRepositoryURLCoordinate("git.example:Team/Repo.git")
	rootedHomeMarker := canonicalRepositoryURLCoordinate("git.example:/~/Team/Repo.git")
	if relative == rootedHomeMarker {
		t.Fatal("relative SCP path collided with rooted /~/ path")
	}

	userRelative := canonicalRepositoryURLCoordinate("alice@git.example:Team/Repo.git")
	rootedUserMarker := canonicalRepositoryURLCoordinate("git.example:/~user/alice/Team/Repo.git")
	if userRelative == rootedUserMarker {
		t.Fatal("user-relative SCP path collided with rooted /~user/... path")
	}

	relativeScan := &corev1alpha1.RepositoryScan{Spec: corev1alpha1.RepositoryScanSpec{Provider: "git", RepoURL: "git.example:Team/Repo.git"}}
	rootedScan := relativeScan.DeepCopy()
	rootedScan.Spec.RepoURL = "git.example:/~/Team/Repo.git"
	if RepositoryTargetID(relativeScan) == RepositoryTargetID(rootedScan) {
		t.Fatal("RepositoryTargetID collapsed relative SCP and rooted /~/ repositories")
	}
}

func TestRepositoryTargetIDPreservesSCPPathRootednessWithoutExplicitCoordinates(t *testing.T) {
	relative := &corev1alpha1.RepositoryScan{Spec: corev1alpha1.RepositoryScanSpec{
		Provider: "git", RepoURL: "git.example:Team/Repo.git",
	}}
	absolute := relative.DeepCopy()
	absolute.Spec.RepoURL = "git.example:/Team/Repo.git"
	if RepositoryTargetID(relative) == RepositoryTargetID(absolute) {
		t.Fatal("RepositoryTargetID collapsed relative and absolute SCP repository paths")
	}
}

func TestSCPRepositoryCoordinatePreservesSignificantPathWhitespace(t *testing.T) {
	canonical := canonicalRepositoryURLCoordinate("git.example:Team/Repo.git")
	for _, repoURL := range []string{
		"git.example: Team/Repo.git",
		"git.example:Team/Repo.git ",
		"git.example:\u00a0Team/Repo.git",
	} {
		if got := canonicalRepositoryURLCoordinate(repoURL); got == canonical {
			t.Fatalf("canonicalRepositoryURLCoordinate(%q) collapsed significant SCP path whitespace", repoURL)
		}

		scan := &corev1alpha1.RepositoryScan{Spec: corev1alpha1.RepositoryScanSpec{Provider: "git", RepoURL: repoURL}}
		withoutWhitespace := scan.DeepCopy()
		withoutWhitespace.Spec.RepoURL = "git.example:Team/Repo.git"
		if RepositoryTargetID(scan) == RepositoryTargetID(withoutWhitespace) {
			t.Fatalf("RepositoryTargetID collapsed significant SCP path whitespace in %q", repoURL)
		}
	}
}

func TestSCPRepositoryCoordinateBindsUserForHomeRelativePaths(t *testing.T) {
	aliceSCP := canonicalRepositoryURLCoordinate("alice@git.example:Team/Repo.git")
	aliceSSH := canonicalRepositoryURLCoordinate("ssh://alice@git.example/~/Team/Repo.git")
	if aliceSCP != aliceSSH {
		t.Fatalf("home-relative SCP coordinate %q differs from equivalent SSH coordinate %q", aliceSCP, aliceSSH)
	}
	if bob := canonicalRepositoryURLCoordinate("bob@git.example:Team/Repo.git"); bob == aliceSCP {
		t.Fatal("home-relative SCP coordinates for distinct SSH users collapsed")
	}
	if bob := canonicalRepositoryURLCoordinate("ssh://bob@git.example/~/Team/Repo.git"); bob == aliceSSH {
		t.Fatal("home-relative SSH URL coordinates for distinct users collapsed")
	}

	withoutUserSCP := canonicalRepositoryURLCoordinate("git.example:Team/Repo.git")
	withoutUserSSH := canonicalRepositoryURLCoordinate("ssh://git.example/~/Team/Repo.git")
	if withoutUserSCP != withoutUserSSH {
		t.Fatalf("username-less home-relative SCP coordinate %q differs from SSH coordinate %q", withoutUserSCP, withoutUserSSH)
	}

	absoluteAlice := canonicalRepositoryURLCoordinate("alice@git.example:/Team/Repo.git")
	absoluteBob := canonicalRepositoryURLCoordinate("ssh://bob@git.example/Team/Repo.git")
	if absoluteAlice != absoluteBob {
		t.Fatalf("absolute repository coordinate unexpectedly depends on SSH user: %q != %q", absoluteAlice, absoluteBob)
	}
}

func TestRepositoryTargetIDBindsUserForHomeRelativeSSHRepositories(t *testing.T) {
	const (
		owner      = "Team"
		repository = "Repo"
	)
	alice := &corev1alpha1.RepositoryScan{Spec: corev1alpha1.RepositoryScanSpec{
		Provider: "git", RepoURL: "alice@git.example:Team/Repo.git",
	}}
	bob := alice.DeepCopy()
	bob.Spec.RepoURL = "ssh://bob@git.example/~/Team/Repo.git"
	if RepositoryTargetID(alice) == RepositoryTargetID(bob) {
		t.Fatal("RepositoryTargetID collapsed home-relative repositories for distinct SSH users")
	}
	aliceURL := alice.DeepCopy()
	aliceURL.Spec.RepoURL = "ssh://alice@git.example/~/Team/Repo.git"
	if RepositoryTargetID(alice) != RepositoryTargetID(aliceURL) {
		t.Fatal("RepositoryTargetID split equivalent SCP and SSH home-relative repositories for one user")
	}

	alice.Spec.Owner = owner
	alice.Spec.Repository = repository
	bob.Spec.Owner = owner
	bob.Spec.Repository = repository
	aliceURL.Spec.Owner = owner
	aliceURL.Spec.Repository = repository
	if RepositoryTargetID(alice) == RepositoryTargetID(bob) {
		t.Fatal("RepositoryTargetID explicit coordinates collapsed home-relative repositories for distinct SSH users")
	}
	if RepositoryTargetID(alice) != RepositoryTargetID(aliceURL) {
		t.Fatal("RepositoryTargetID explicit coordinates split equivalent SCP and SSH home-relative repositories for one user")
	}
}

func TestRepositoryTargetIDCanonicalizesUsernameLessSCPOrigin(t *testing.T) {
	scp := &corev1alpha1.RepositoryScan{Spec: corev1alpha1.RepositoryScanSpec{
		Provider: "git", Owner: "Team", Repository: "Repo", RepoURL: "git.example:Team/Repo.git?mirror=https://mirror.example#fragment",
	}}
	sshURL := scp.DeepCopy()
	sshURL.Spec.RepoURL = "ssh://git.example/Team/Repo.git"
	if RepositoryTargetID(scp) != RepositoryTargetID(sshURL) {
		t.Fatal("username-less SCP and SSH URL repository target IDs differ")
	}
}

func TestRepositoryTargetIDIncludesServerOrigin(t *testing.T) {
	first := &corev1alpha1.RepositoryScan{Spec: corev1alpha1.RepositoryScanSpec{
		Provider: "github", Owner: "example", Repository: "repo", RepoURL: "https://github.com/example/repo",
	}}
	second := first.DeepCopy()
	second.Spec.RepoURL = "https://github.enterprise.example/example/repo"
	if RepositoryTargetID(first) == RepositoryTargetID(second) {
		t.Fatal("RepositoryTargetID ignored repository server origin")
	}
}

func TestRepositoryTargetIDSeparatesIPv6HostAndPortAuthority(t *testing.T) {
	withPort := &corev1alpha1.RepositoryScan{Spec: corev1alpha1.RepositoryScanSpec{
		Provider: "git", Owner: "Team", Repository: "Repo", RepoURL: "https://[2001:db8::1]:443/Team/Repo.git",
	}}
	addressEndingInPort := withPort.DeepCopy()
	addressEndingInPort.Spec.RepoURL = "https://[2001:db8::1:443]/Team/Repo.git"
	if RepositoryTargetID(withPort) == RepositoryTargetID(addressEndingInPort) {
		t.Fatal("RepositoryTargetID collapsed an IPv6 host/port authority with a distinct IPv6 host")
	}
}

func TestRepositoryTargetIDStripsCredentialsAndPreservesRepositoryCase(t *testing.T) {
	plain := &corev1alpha1.RepositoryScan{Spec: corev1alpha1.RepositoryScanSpec{
		Provider: "gitlab", Owner: "Team", Repository: "Repo", RepoURL: "https://git.example/Team/Repo.git",
	}}
	credentialed := plain.DeepCopy()
	credentialed.Spec.RepoURL = "https://" + "alice" + ":" + "example" + "@git.example/Team/Repo.git?mode=read#fragment"
	if RepositoryTargetID(plain) != RepositoryTargetID(credentialed) {
		t.Fatal("RepositoryTargetID changed when URL credentials/query changed")
	}
	differentCase := plain.DeepCopy()
	differentCase.Spec.Owner = "team"
	if RepositoryTargetID(plain) == RepositoryTargetID(differentCase) {
		t.Fatal("RepositoryTargetID collapsed case-sensitive repository coordinates")
	}
}
