package bundle

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	testRepositorySubPath = "services/api"
	testCoveragePartial   = "Partial"
	testCoveragePartialLC = "partial"
)

func TestBuildByteIdenticalRetry(t *testing.T) {
	input := testInput()
	first, err := Build(input, Limits{})
	if err != nil {
		t.Fatalf("Build(first): %v", err)
	}
	second, err := Build(input, Limits{})
	if err != nil {
		t.Fatalf("Build(second): %v", err)
	}
	assertBundlesEqual(t, first, second)
	if err := Verify(first, Limits{}); err != nil {
		t.Fatalf("Verify(first): %v", err)
	}
}

func TestBuildInsertionOrderIndependent(t *testing.T) {
	leftInput := testInput()
	rightInput := testInput()

	reverseStrings(rightInput.Manifest.OccurrenceIDs)
	reverseStrings(rightInput.Manifest.AssessmentIDs)
	reverseStrings(rightInput.Manifest.StageReceiptIDs)
	reverseStrings(rightInput.Manifest.EvidenceReceiptIDs)
	reverseStrings(rightInput.Manifest.ThreatModel.Assumptions)
	reverseStrings(rightInput.Manifest.ThreatModel.Limitations)
	reverseFindings(rightInput.Findings.Findings)
	for i := range rightInput.Findings.Findings {
		reverseLocations(rightInput.Findings.Findings[i].Locations)
		reverseEvidenceReferences(rightInput.Findings.Findings[i].Evidence)
		reverseStrings(rightInput.Findings.Findings[i].AssessmentIDs)
	}
	reverseInventory(rightInput.Coverage.Inventory)
	reverseCandidates(rightInput.Coverage.Candidates)
	reverseStages(rightInput.Coverage.Stages)
	for i := range rightInput.Coverage.Inventory {
		reverseStrings(rightInput.Coverage.Inventory[i].SliceIDs)
		reverseStrings(rightInput.Coverage.Inventory[i].ReceiptIDs)
	}
	for i := range rightInput.Coverage.Candidates {
		reverseStrings(rightInput.Coverage.Candidates[i].ReceiptIDs)
	}
	reverseEvidenceInputs(rightInput.Evidence)

	left, err := Build(leftInput, Limits{})
	if err != nil {
		t.Fatalf("Build(left): %v", err)
	}
	right, err := Build(rightInput, Limits{})
	if err != nil {
		t.Fatalf("Build(right): %v", err)
	}
	assertBundlesEqual(t, left, right)
}

func TestBuildNormalizesLFAndPaths(t *testing.T) {
	crlfInput := testInput()
	lfInput := testInput()
	lfInput.Manifest.ThreatModel.Content = strings.ReplaceAll(lfInput.Manifest.ThreatModel.Content, "\r\n", "\n")
	lfInput.Manifest.ThreatModel.Assumptions[0] = strings.ReplaceAll(lfInput.Manifest.ThreatModel.Assumptions[0], "\r\n", "\n")
	lfInput.Manifest.Metadata["note"] = strings.ReplaceAll(lfInput.Manifest.Metadata["note"], "\r\n", "\n")
	*lfInput.Findings.Findings[1].Summary = strings.ReplaceAll(*lfInput.Findings.Findings[1].Summary, "\r\n", "\n")
	*lfInput.Coverage.Inventory[1].Reason = strings.ReplaceAll(*lfInput.Coverage.Inventory[1].Reason, "\r\n", "\n")
	lfInput.Evidence[1].Data = bytes.ReplaceAll(lfInput.Evidence[1].Data, []byte("\r\n"), []byte("\n"))

	crlfBundle, err := Build(crlfInput, Limits{})
	if err != nil {
		t.Fatalf("Build(CRLF): %v", err)
	}
	lfBundle, err := Build(lfInput, Limits{})
	if err != nil {
		t.Fatalf("Build(LF): %v", err)
	}
	assertBundlesEqual(t, crlfBundle, lfBundle)

	var manifest manifestDocument
	if err := json.Unmarshal(crlfBundle.ManifestJSON, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if got := deref(manifest.Repository.SubPath); got != testRepositorySubPath {
		t.Fatalf("subPath = %q, want services/api", got)
	}
	var findings FindingsInput
	if err := json.Unmarshal(crlfBundle.FindingsJSON, &findings); err != nil {
		t.Fatalf("decode findings: %v", err)
	}
	if got := findings.Findings[0].Locations[0].Path; got != "services/api/admin.go" {
		t.Fatalf("finding path changed unexpectedly: %q", got)
	}
	if bytes.Contains(crlfBundle.ManifestJSON, []byte{'\r'}) || bytes.Contains(crlfBundle.FindingsJSON, []byte{'\r'}) ||
		bytes.Contains(crlfBundle.CoverageJSON, []byte{'\r'}) || bytes.Contains(crlfBundle.Evidence[1].Data, []byte{'\r'}) {
		t.Fatal("canonical output contains CR bytes")
	}
}

func TestBuildRejectsBackslashRepositoryPaths(t *testing.T) {
	input := testInput()
	input.Findings.Findings[0].Locations[0].Path = `services\api\decode.go`
	if _, err := Build(input, DefaultLimits()); err == nil {
		t.Fatal("Build() accepted a backslash repository path")
	}
}

func TestBuildRequiresFullGitCommitObjectID(t *testing.T) {
	tests := map[string]string{
		"symbolic ref": "main",
		"abbreviated":  "deadbeef",
		"39 hex":       strings.Repeat("a", 39),
		"63 hex":       strings.Repeat("b", 63),
		"40 non-hex":   strings.Repeat("g", 40),
		"64 non-hex":   strings.Repeat("z", 64),
	}
	for name, commitSHA := range tests {
		t.Run(name, func(t *testing.T) {
			input := testInput()
			input.Manifest.Target.CommitSHA = commitSHA
			if _, err := Build(input, Limits{}); err == nil ||
				!strings.Contains(err.Error(), "full 40- or 64-hex Git object ID") {
				t.Fatalf("Build() error = %v", err)
			}
		})
	}
}

func TestBuildRejectsNonPositiveRepositoryScanGeneration(t *testing.T) {
	for name, generation := range map[string]int64{
		"zero":     0,
		"negative": -1,
	} {
		t.Run(name, func(t *testing.T) {
			input := testInput()
			input.Manifest.Run.RepositoryScanGeneration = generation
			if _, err := Build(input, Limits{}); err == nil ||
				!strings.Contains(err.Error(), "run.repositoryScanGeneration must be positive") {
				t.Fatalf("Build() error = %v", err)
			}
		})
	}
}

func TestBuildAcceptsAndCanonicalizesSHA256GitCommitObjectID(t *testing.T) {
	input := testInput()
	input.Manifest.Target.CommitSHA = strings.ToUpper(strings.Repeat("ab", 32))
	built := mustBuild(t, input)

	var manifest manifestDocument
	if err := json.Unmarshal(built.ManifestJSON, &manifest); err != nil {
		t.Fatal(err)
	}
	if want := strings.Repeat("ab", 32); manifest.Target.CommitSHA != want {
		t.Fatalf("commitSHA = %q, want %q", manifest.Target.CommitSHA, want)
	}
}

func TestBuildPreservesAbsentVersusEmpty(t *testing.T) {
	t.Run("nil versus empty array", func(t *testing.T) {
		absent := testInput()
		empty := testInput()
		absent.Manifest.ThreatModel.Assumptions = nil
		empty.Manifest.ThreatModel.Assumptions = []string{}

		absentBundle := mustBuild(t, absent)
		emptyBundle := mustBuild(t, empty)
		if absentBundle.Roots.ContentDigest == emptyBundle.Roots.ContentDigest {
			t.Fatal("nil and empty assumption arrays produced the same content digest")
		}
		if !bytes.Contains(absentBundle.ManifestJSON, []byte(`"assumptions":null`)) {
			t.Fatalf("absent assumptions not represented as null: %s", absentBundle.ManifestJSON)
		}
		if !bytes.Contains(emptyBundle.ManifestJSON, []byte(`"assumptions":[]`)) {
			t.Fatalf("empty assumptions not represented as []: %s", emptyBundle.ManifestJSON)
		}
	})

	t.Run("nil versus empty optional string", func(t *testing.T) {
		absent := testInput()
		empty := testInput()
		absent.Manifest.Repository.SubPath = nil
		empty.Manifest.Repository.SubPath = new("")

		absentBundle := mustBuild(t, absent)
		emptyBundle := mustBuild(t, empty)
		if absentBundle.Roots.ContentDigest == emptyBundle.Roots.ContentDigest {
			t.Fatal("nil and empty subPath produced the same content digest")
		}
		if !bytes.Contains(absentBundle.ManifestJSON, []byte(`"subPath":null`)) {
			t.Fatalf("absent subPath not represented as null: %s", absentBundle.ManifestJSON)
		}
		if !bytes.Contains(emptyBundle.ManifestJSON, []byte(`"subPath":""`)) {
			t.Fatalf("empty subPath not represented as empty: %s", emptyBundle.ManifestJSON)
		}
	})
}

func TestVolatileRunFieldsOnlyChangeRunReceipt(t *testing.T) {
	firstInput := testInput()
	secondInput := testInput()
	secondInput.Manifest.Run.RunUID = "run-uid-2"
	secondInput.Manifest.Run.PublicRunID = new("scan-public-2")
	secondInput.Manifest.Run.RepositoryScanGeneration = 10
	secondInput.Manifest.Run.StartedAt = secondInput.Manifest.Run.StartedAt.Add(2 * time.Hour)
	completed := secondInput.Manifest.Run.StartedAt.Add(30 * time.Minute)
	secondInput.Manifest.Run.CompletedAt = &completed
	secondInput.Manifest.Run.SealedAt = completed.Add(time.Minute)

	first := mustBuild(t, firstInput)
	second := mustBuild(t, secondInput)
	if first.Roots.ContentDigest != second.Roots.ContentDigest {
		t.Fatalf("content digest changed with volatile run fields: %s != %s", first.Roots.ContentDigest, second.Roots.ContentDigest)
	}
	if first.Roots.RunReceiptDigest == second.Roots.RunReceiptDigest {
		t.Fatal("run receipt digest did not change with volatile run fields")
	}
	if bytes.Equal(first.ManifestJSON, second.ManifestJSON) {
		t.Fatal("manifest bytes did not bind volatile run fields")
	}
	if !bytes.Equal(first.FindingsJSON, second.FindingsJSON) || !bytes.Equal(first.CoverageJSON, second.CoverageJSON) {
		t.Fatal("volatile run fields changed semantic documents")
	}
}

func TestSecurityFieldChangeChangesContentRoot(t *testing.T) {
	original := mustBuild(t, testInput())
	changedInput := testInput()
	changedInput.Findings.Findings[0].Severity = "critical"
	changed := mustBuild(t, changedInput)

	if original.Roots.ContentDigest == changed.Roots.ContentDigest {
		t.Fatal("security-relevant severity change did not change content digest")
	}
	originalManifest := decodeManifest(t, original.ManifestJSON)
	changedManifest := decodeManifest(t, changed.ManifestJSON)
	if originalManifest.Digests.Findings == changedManifest.Digests.Findings {
		t.Fatal("security-relevant severity change did not change findings digest")
	}
}

func TestVerifyDetectsDocumentBlobAndRootMismatch(t *testing.T) {
	base := mustBuild(t, testInput())

	t.Run("document", func(t *testing.T) {
		tampered := cloneBundle(base)
		var findings FindingsInput
		if err := json.Unmarshal(tampered.FindingsJSON, &findings); err != nil {
			t.Fatalf("decode findings: %v", err)
		}
		findings.Findings[0].Severity = "critical"
		data, err := canonicalJSON(findings)
		if err != nil {
			t.Fatalf("marshal tampered findings: %v", err)
		}
		tampered.FindingsJSON = data
		if err := Verify(tampered, Limits{}); err == nil {
			t.Fatal("Verify accepted a document mismatch")
		}
	})

	t.Run("blob", func(t *testing.T) {
		tampered := cloneBundle(base)
		tampered.Evidence[0].Data[len(tampered.Evidence[0].Data)-2] = 'X'
		err := Verify(tampered, Limits{})
		if err == nil || !strings.Contains(err.Error(), "evidence blob") {
			t.Fatalf("Verify(blob mismatch) error = %v", err)
		}
	})

	t.Run("content root", func(t *testing.T) {
		tampered := cloneBundle(base)
		tampered.Roots.ContentDigest = strings.Repeat("0", len(tampered.Roots.ContentDigest))
		err := Verify(tampered, Limits{})
		if err == nil || !strings.Contains(err.Error(), "content root mismatch") {
			t.Fatalf("Verify(content root mismatch) error = %v", err)
		}
	})

	t.Run("run receipt root", func(t *testing.T) {
		tampered := cloneBundle(base)
		tampered.Roots.RunReceiptDigest = strings.Repeat("f", len(tampered.Roots.RunReceiptDigest))
		err := Verify(tampered, Limits{})
		if err == nil || !strings.Contains(err.Error(), "run receipt root mismatch") {
			t.Fatalf("Verify(run receipt mismatch) error = %v", err)
		}
	})
}

func TestVerifyRejectsNonPositiveRepositoryScanGeneration(t *testing.T) {
	base := mustBuild(t, testInput())
	for name, generation := range map[string]int64{
		"zero":     0,
		"negative": -1,
	} {
		t.Run(name, func(t *testing.T) {
			tampered := cloneBundle(base)
			manifest := decodeManifest(t, tampered.ManifestJSON)
			manifest.Run.RepositoryScanGeneration = generation
			var err error
			tampered.ManifestJSON, err = canonicalJSON(manifest)
			if err != nil {
				t.Fatalf("marshal manifest: %v", err)
			}
			if err := Verify(tampered, Limits{}); err == nil ||
				!strings.Contains(err.Error(), "run.repositoryScanGeneration must be positive") {
				t.Fatalf("Verify() error = %v", err)
			}
		})
	}
}

func TestManifestDigestExcludesItself(t *testing.T) {
	built := mustBuild(t, testInput())
	manifest := decodeManifest(t, built.ManifestJSON)
	stored := manifest.Digests.Manifest

	computed, err := manifestDocumentDigest(manifest, DefaultLimits())
	if err != nil {
		t.Fatalf("manifestDocumentDigest: %v", err)
	}
	if computed != stored {
		t.Fatalf("computed manifest digest = %s, want %s", computed, stored)
	}

	manifest.Digests.Manifest = "sha256:" + strings.Repeat("a", 64)
	recomputed, err := manifestDocumentDigest(manifest, DefaultLimits())
	if err != nil {
		t.Fatalf("manifestDocumentDigest(modified self field): %v", err)
	}
	if recomputed != stored {
		t.Fatalf("self digest participated in its own input: %s != %s", recomputed, stored)
	}
}

func TestBuildEnforcesBoundsAndUTF8(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Input) Limits
	}{
		{
			name: "finding count",
			mutate: func(input *Input) Limits {
				return Limits{MaxFindings: 1}
			},
		},
		{
			name: "evidence blob bytes",
			mutate: func(input *Input) Limits {
				return Limits{MaxEvidenceBlobBytes: 4}
			},
		},
		{
			name: "document bytes",
			mutate: func(input *Input) Limits {
				return Limits{MaxDocumentBytes: 128}
			},
		},
		{
			name: "invalid UTF-8",
			mutate: func(input *Input) Limits {
				input.Manifest.ThreatModel.Content = string([]byte{0xff, 0xfe})
				return Limits{}
			},
		},
		{
			name: "escaping path",
			mutate: func(input *Input) Limits {
				input.Findings.Findings[0].Locations[0].Path = "../secret"
				return Limits{}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := testInput()
			limits := test.mutate(&input)
			if _, err := Build(input, limits); err == nil {
				t.Fatal("Build unexpectedly succeeded")
			}
		})
	}
}

func TestVerifyPreflightsTopLevelArrayCardinality(t *testing.T) {
	built := mustBuild(t, testInput())
	tests := []struct {
		name       string
		limits     Limits
		findings   string
		coverage   string
		wantErrSub string
	}{
		{
			name:       "findings",
			limits:     Limits{MaxFindings: 1},
			findings:   `{"schemaVersion":1,"findings":[null,null],"metadata":{}}`,
			wantErrSub: `top-level array "findings" exceeds maximum item count of 1`,
		},
		{
			name:       "inventory",
			limits:     Limits{MaxArrayItems: 1},
			findings:   `{"schemaVersion":1,"findings":[],"metadata":{}}`,
			coverage:   `{"schemaVersion":1,"inventoryStatus":"complete","candidateStatus":"complete","coverageStatus":"complete","inventory":[null,null],"candidates":[],"stages":[],"metadata":{}}`,
			wantErrSub: `top-level array "inventory" exceeds maximum item count of 1`,
		},
		{
			name:       "candidates",
			limits:     Limits{MaxArrayItems: 1},
			findings:   `{"schemaVersion":1,"findings":[],"metadata":{}}`,
			coverage:   `{"schemaVersion":1,"inventoryStatus":"complete","candidateStatus":"complete","coverageStatus":"complete","inventory":[],"candidates":[null,null],"stages":[],"metadata":{}}`,
			wantErrSub: `top-level array "candidates" exceeds maximum item count of 1`,
		},
		{
			name:       "stages",
			limits:     Limits{MaxArrayItems: 1},
			findings:   `{"schemaVersion":1,"findings":[],"metadata":{}}`,
			coverage:   `{"schemaVersion":1,"inventoryStatus":"complete","candidateStatus":"complete","coverageStatus":"complete","inventory":[],"candidates":[],"stages":[null,null],"metadata":{}}`,
			wantErrSub: `top-level array "stages" exceeds maximum item count of 1`,
		},
		{
			name:       "coverage total",
			limits:     Limits{MaxCoverageEntries: 2},
			coverage:   `{"schemaVersion":1,"inventoryStatus":"complete","candidateStatus":"complete","coverageStatus":"complete","inventory":[null],"candidates":[null],"stages":[null],"metadata":{}}`,
			wantErrSub: "top-level arrays exceed maximum total item count of 2",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tampered := *built
			if test.findings != "" {
				tampered.FindingsJSON = []byte(test.findings)
			}
			if test.coverage != "" {
				tampered.CoverageJSON = []byte(test.coverage)
			}
			err := Verify(&tampered, test.limits)
			if err == nil || !strings.Contains(err.Error(), test.wantErrSub) {
				t.Fatalf("Verify() error = %v, want substring %q", err, test.wantErrSub)
			}
		})
	}
}

func TestBuildRejectsMissingEvidenceReference(t *testing.T) {
	input := testInput()
	input.Findings.Findings[0].Evidence[0].BlobName = "evidence/missing.txt"
	if _, err := Build(input, Limits{}); err == nil || !strings.Contains(err.Error(), "missing evidence blob") {
		t.Fatalf("Build error = %v", err)
	}
}

func testInput() Input {
	started := time.Date(2026, time.July, 30, 10, 0, 0, 123456789, time.FixedZone("PDT", -7*60*60))
	completed := started.Add(20 * time.Minute)
	sealed := completed.Add(time.Minute)
	summary := "Authorization bypass reaches admin action.\r\nRequires tenant input."
	reason := "Full source delivered.\r\nDigest verified."
	legacy := "v2:legacy-fingerprint"
	instance := "tenant-boundary-a"
	symbol := "HandleAdmin"
	receiptID := "evidence-receipt-1"
	label := "Trace"
	occurrenceID := "occurrence-1"
	scope := testRepositorySubPath
	subPath := testRepositorySubPath
	originalRef := "refs/heads/main"
	producer := "codex"
	mapper := "mapper-v2"
	policy := "policy-v3"
	runtime := "wrapper-v4"
	publicRunID := "scan-public-1"

	return Input{
		Manifest: ManifestInput{
			SchemaVersion: SchemaVersion,
			Repository: RepositoryIdentity{
				Provider:     "GitHub",
				RepositoryID: "github:12345",
				RepoURL:      "https://github.com/example/repo",
				SubPath:      &subPath,
			},
			Target: TargetSnapshot{
				CommitSHA:     "ABCDEF0123456789ABCDEF0123456789ABCDEF01",
				TreeDigest:    "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
				TargetID:      "target-v1:github:12345",
				OriginalRef:   &originalRef,
				ReceiptID:     "target-receipt-1",
				ReceiptDigest: "SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
			},
			ThreatModel: ThreatModelInput{
				Version:     "tm-v1",
				Content:     "# Threat model\r\n\r\nUntrusted request reaches a privileged action.\r\n",
				Scope:       &scope,
				Assumptions: []string{"Tenant input is attacker-controlled.\r\nAuthentication already succeeded.", "Repository target is verified."},
				Limitations: []string{"No production credentials.", "Static analysis only."},
			},
			Quality: QualitySummary{
				InventoryCoverage:   "Complete",
				CandidateCoverage:   "Complete",
				Coverage:            "Complete",
				ValidationScope:     "All",
				ValidationExecution: "Complete",
				AttackPathExecution: "Complete",
				AnalysisAttestation: "Tool-Observed",
				TargetVerification:  "Verified",
				Authorization:       "Verified",
				Isolation:           "Hardened",
			},
			Versions: ComponentVersions{
				Producer:       &producer,
				Schema:         "security-bundle-v1",
				Controller:     "controller-v1",
				Mapper:         &mapper,
				Policy:         &policy,
				RuntimeAdapter: &runtime,
				Additional:     map[string]string{"worker": "worker-v5", "normalizer": "normalizer-v2"},
			},
			OccurrenceIDs: []string{"occurrence-2", "occurrence-1"},
			AssessmentIDs: []string{
				"assessment-2", "assessment-attack-1", "assessment-validation-1",
			},
			StageReceiptIDs: []string{"stage-review", "stage-mapper"},
			EvidenceReceiptIDs: []string{
				"evidence-receipt-2", "evidence-receipt-1",
				"candidate-receipt-2", "candidate-receipt-1", "candidate-receipt-0",
				"receipt-router", "receipt-admin-2", "receipt-admin-1",
			},
			Metadata: map[string]string{"zeta": "last", "note": "line one\r\nline two", "alpha": "first"},
			Run: RunEnvelope{
				RunUID:                   "run-uid-1",
				PublicRunID:              &publicRunID,
				Namespace:                "security",
				RepositoryScanName:       "example-repo",
				RepositoryScanUID:        "repository-scan-uid-1",
				RepositoryScanGeneration: 9,
				StartedAt:                started,
				CompletedAt:              &completed,
				SealedAt:                 sealed,
			},
		},
		Findings: FindingsInput{
			SchemaVersion: SchemaVersion,
			Findings: []Finding{
				{
					SemanticFingerprint: "semantic:2",
					OccurrenceID:        "occurrence-2",
					RuleID:              "unsafe-deserialization",
					IdentityAnchor:      "request-decoder",
					Title:               "Unsafe deserialization",
					Severity:            "medium",
					Confidence:          "high",
					Locations: []FindingLocation{
						{Path: `services/api/decode.go`, StartLine: 40, EndLine: 44},
					},
					Evidence:      []EvidenceReference{{BlobName: `evidence/payload.json`}},
					AssessmentIDs: []string{"assessment-2"},
				},
				{
					SemanticFingerprint: "semantic:1",
					OccurrenceID:        "occurrence-1",
					LegacyFingerprint:   &legacy,
					RuleID:              "authorization-bypass",
					IdentityAnchor:      "admin-handler",
					IdentityInstance:    &instance,
					Title:               "Authorization bypass",
					Summary:             &summary,
					Severity:            "HIGH",
					Confidence:          "HIGH",
					Locations: []FindingLocation{
						{Path: `services/api/admin.go`, StartLine: 22, EndLine: 24, Symbol: &symbol},
						{Path: `services/api/router.go`, StartLine: 10, EndLine: 12},
					},
					Evidence: []EvidenceReference{
						{BlobName: `evidence/trace.txt`, ReceiptID: &receiptID, Label: &label},
						{BlobName: `evidence/payload.json`},
					},
					AssessmentIDs: []string{"assessment-attack-1", "assessment-validation-1"},
				},
			},
			Metadata: map[string]string{"source": "controller", "note": "line one\r\nline two"},
		},
		Coverage: CoverageInput{
			SchemaVersion:   SchemaVersion,
			InventoryStatus: "Complete",
			CandidateStatus: "Complete",
			CoverageStatus:  "Complete",
			Inventory: []InventoryCoverageEntry{
				{Path: `services/api/router.go`, Classification: "eligible", SliceIDs: []string{"slice-router"}, ReceiptIDs: []string{"receipt-router"}},
				{Path: `services/api/admin.go`, Classification: "eligible", Reason: &reason, SliceIDs: []string{"slice-admin-2", "slice-admin-1"}, ReceiptIDs: []string{"receipt-admin-2", "receipt-admin-1"}},
			},
			Candidates: []CandidateCoverageEntry{
				{CandidateID: "candidate-2", Disposition: "accepted", OccurrenceID: &occurrenceID, ReceiptIDs: []string{"candidate-receipt-2", "candidate-receipt-1"}},
				{CandidateID: "candidate-1", Disposition: "rejected", Reason: new("Not a security boundary."), ReceiptIDs: []string{"candidate-receipt-0"}},
			},
			Stages: []StageCoverageEntry{
				{Stage: "review", ScopeID: new("slice-admin-1"), Disposition: "accepted", ReceiptID: "stage-review"},
				{Stage: "mapper", Disposition: "accepted", ReceiptID: "stage-mapper"},
			},
			Metadata: map[string]string{"strategy": "repository", "schema": "coverage-v1"},
		},
		Evidence: []EvidenceBlobInput{
			{Name: `evidence/payload.json`, MediaType: "application/json", Data: []byte(`{"tenant":"a","role":"admin"}`)},
			{Name: `evidence/trace.txt`, MediaType: "text/plain; charset=utf-8", Data: []byte("request\r\nauthorization check\r\nadmin action\r\n")},
		},
	}
}

func mustBuild(t *testing.T, input Input) *Bundle {
	t.Helper()
	built, err := Build(input, Limits{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return built
}

func decodeManifest(t *testing.T, data []byte) manifestDocument {
	t.Helper()
	var manifest manifestDocument
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	return manifest
}

func cloneBundle(input *Bundle) *Bundle {
	return &Bundle{
		ManifestJSON: cloneBytes(input.ManifestJSON),
		FindingsJSON: cloneBytes(input.FindingsJSON),
		CoverageJSON: cloneBytes(input.CoverageJSON),
		Evidence:     cloneEvidenceBlobs(input.Evidence),
		Roots:        input.Roots,
	}
}

func assertBundlesEqual(t *testing.T, left, right *Bundle) {
	t.Helper()
	if !bytes.Equal(left.ManifestJSON, right.ManifestJSON) {
		t.Fatalf("manifest bytes differ:\nleft:  %s\nright: %s", left.ManifestJSON, right.ManifestJSON)
	}
	if !bytes.Equal(left.FindingsJSON, right.FindingsJSON) {
		t.Fatalf("findings bytes differ:\nleft:  %s\nright: %s", left.FindingsJSON, right.FindingsJSON)
	}
	if !bytes.Equal(left.CoverageJSON, right.CoverageJSON) {
		t.Fatalf("coverage bytes differ:\nleft:  %s\nright: %s", left.CoverageJSON, right.CoverageJSON)
	}
	if left.Roots != right.Roots {
		t.Fatalf("roots differ: left=%+v right=%+v", left.Roots, right.Roots)
	}
	if !evidenceBlobsEqual(left.Evidence, right.Evidence) {
		t.Fatalf("evidence differs: left=%+v right=%+v", left.Evidence, right.Evidence)
	}
}

//go:fix inline

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func reverseStrings(values []string) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func reverseFindings(values []Finding) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func reverseLocations(values []FindingLocation) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func reverseEvidenceReferences(values []EvidenceReference) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func reverseInventory(values []InventoryCoverageEntry) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func reverseCandidates(values []CandidateCoverageEntry) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func reverseStages(values []StageCoverageEntry) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func reverseEvidenceInputs(values []EvidenceBlobInput) {
	for left, right := 0, len(values)-1; left < right; left, right = left+1, right-1 {
		values[left], values[right] = values[right], values[left]
	}
}

func TestRepositoryPathWhitespaceDoesNotCollide(t *testing.T) {
	plain := testInput()
	spaced := testInput()
	plain.Findings.Findings[0].Locations[0].Path = "services/api/decode.go"
	spaced.Findings.Findings[0].Locations[0].Path = "services/api/decode.go "

	plainBundle := mustBuild(t, plain)
	spacedBundle := mustBuild(t, spaced)
	if plainBundle.Roots.ContentDigest == spacedBundle.Roots.ContentDigest {
		t.Fatal("distinct Git paths differing by trailing whitespace collided")
	}
	var findings FindingsInput
	if err := json.Unmarshal(spacedBundle.FindingsJSON, &findings); err != nil {
		t.Fatalf("decode findings: %v", err)
	}
	found := false
	for _, finding := range findings.Findings {
		for _, location := range finding.Locations {
			if location.Path == "services/api/decode.go " {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("canonical findings did not preserve significant path whitespace")
	}
}

func TestBuildRejectsDuplicateNormalizedCoverageIdentities(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*Input)
		wantErrSub string
	}{
		{
			name: "inventory path",
			mutate: func(input *Input) {
				input.Coverage.Inventory = append(input.Coverage.Inventory, InventoryCoverageEntry{
					Path:           "services/api/./router.go",
					Classification: "excluded",
				})
			},
			wantErrSub: `duplicate normalized path "services/api/router.go"`,
		},
		{
			name: "candidate ID",
			mutate: func(input *Input) {
				input.Coverage.Candidates = append(input.Coverage.Candidates, CandidateCoverageEntry{
					CandidateID: " candidate-2 ",
					Disposition: "rejected",
				})
			},
			wantErrSub: `duplicate normalized candidateId "candidate-2"`,
		},
		{
			name: "stage and scope",
			mutate: func(input *Input) {
				input.Coverage.Stages = append(input.Coverage.Stages, StageCoverageEntry{
					Stage:       " REVIEW ",
					ScopeID:     new(" slice-admin-1 "),
					Disposition: "deferred",
					ReceiptID:   "stage-review-duplicate",
				})
			},
			wantErrSub: `duplicate normalized stage and scope identity "review:slice-admin-1"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := testInput()
			test.mutate(&input)
			_, err := Build(input, Limits{})
			if err == nil || !strings.Contains(err.Error(), test.wantErrSub) {
				t.Fatalf("Build() error = %v, want substring %q", err, test.wantErrSub)
			}
		})
	}
}

func TestBundleRejectsDuplicateImmutableReferences(t *testing.T) {
	input := testInput()
	input.Manifest.OccurrenceIDs = append(input.Manifest.OccurrenceIDs, input.Manifest.OccurrenceIDs[0])
	if _, err := Build(input, DefaultLimits()); err == nil {
		t.Fatal("Build() accepted duplicate immutable occurrence references")
	}
}

func TestBuildRejectsCoverageStatusDisagreement(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*Input)
		wantErrSub string
	}{
		{
			name: "inventory coverage",
			mutate: func(input *Input) {
				input.Manifest.Quality.InventoryCoverage = testCoveragePartial
			},
			wantErrSub: "inventoryCoverage",
		},
		{
			name: "candidate coverage",
			mutate: func(input *Input) {
				input.Manifest.Quality.CandidateCoverage = testCoveragePartial
			},
			wantErrSub: "candidateCoverage",
		},
		{
			name: "aggregate coverage",
			mutate: func(input *Input) {
				input.Manifest.Quality.Coverage = testCoveragePartial
			},
			wantErrSub: "coverageStatus",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := testInput()
			test.mutate(&input)
			_, err := Build(input, Limits{})
			if err == nil || !strings.Contains(err.Error(), test.wantErrSub) {
				t.Fatalf("Build() error = %v, want substring %q", err, test.wantErrSub)
			}
		})
	}
}

func TestVerifyRejectsCoverageStatusDisagreement(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*CoverageInput)
		wantErrSub string
	}{
		{
			name: "inventory coverage",
			mutate: func(coverage *CoverageInput) {
				coverage.InventoryStatus = testCoveragePartialLC
			},
			wantErrSub: "inventoryCoverage",
		},
		{
			name: "candidate coverage",
			mutate: func(coverage *CoverageInput) {
				coverage.CandidateStatus = testCoveragePartialLC
			},
			wantErrSub: "candidateCoverage",
		},
		{
			name: "aggregate coverage",
			mutate: func(coverage *CoverageInput) {
				coverage.CoverageStatus = testCoveragePartialLC
			},
			wantErrSub: "coverageStatus",
		},
	}

	base := mustBuild(t, testInput())
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tampered := cloneBundle(base)
			var coverage CoverageInput
			if err := json.Unmarshal(tampered.CoverageJSON, &coverage); err != nil {
				t.Fatalf("decode coverage: %v", err)
			}
			test.mutate(&coverage)
			data, err := canonicalJSON(coverage)
			if err != nil {
				t.Fatalf("marshal coverage: %v", err)
			}
			tampered.CoverageJSON = data

			err = Verify(tampered, Limits{})
			if err == nil || !strings.Contains(err.Error(), test.wantErrSub) {
				t.Fatalf("Verify() error = %v, want substring %q", err, test.wantErrSub)
			}
		})
	}
}

func TestEvidenceAbsentVersusEmptyIsPreserved(t *testing.T) {
	absent := testInput()
	empty := testInput()
	for i := range absent.Findings.Findings {
		absent.Findings.Findings[i].Evidence = nil
		empty.Findings.Findings[i].Evidence = nil
	}
	absent.Evidence = nil
	empty.Evidence = []EvidenceBlobInput{}

	absentBundle := mustBuild(t, absent)
	emptyBundle := mustBuild(t, empty)
	if absentBundle.Roots.ContentDigest == emptyBundle.Roots.ContentDigest {
		t.Fatal("nil and empty evidence sets produced the same content digest")
	}
	if !bytes.Contains(absentBundle.ManifestJSON, []byte(`"evidence":null`)) {
		t.Fatalf("absent evidence not represented as null: %s", absentBundle.ManifestJSON)
	}
	if !bytes.Contains(emptyBundle.ManifestJSON, []byte(`"evidence":[]`)) {
		t.Fatalf("empty evidence not represented as []: %s", emptyBundle.ManifestJSON)
	}
	if err := Verify(absentBundle, Limits{}); err != nil {
		t.Fatalf("Verify(absent): %v", err)
	}
	if err := Verify(emptyBundle, Limits{}); err != nil {
		t.Fatalf("Verify(empty): %v", err)
	}

	tampered := cloneBundle(absentBundle)
	tampered.Evidence = []EvidenceBlob{}
	if err := Verify(tampered, Limits{}); err == nil {
		t.Fatal("Verify accepted changing absent evidence to explicitly empty")
	}
}

func TestTextEvidenceCharsetCanonicalization(t *testing.T) {
	explicit := testInput()
	implicit := testInput()
	explicit.Evidence[1].MediaType = "TEXT/PLAIN; CHARSET=UTF8"
	implicit.Evidence[1].MediaType = "text/plain"
	assertBundlesEqual(t, mustBuild(t, explicit), mustBuild(t, implicit))

	nonUTF8 := testInput()
	nonUTF8.Evidence[1].MediaType = "text/plain; charset=utf-16le"
	if _, err := Build(nonUTF8, Limits{}); err == nil || !strings.Contains(err.Error(), "charset must be UTF-8") {
		t.Fatalf("Build(non-UTF-8 charset) error = %v", err)
	}

	nulText := testInput()
	nulText.Evidence[1].Data = []byte{'a', 0, 'b'}
	if _, err := Build(nulText, Limits{}); err == nil || !strings.Contains(err.Error(), "contains NUL") {
		t.Fatalf("Build(NUL text) error = %v", err)
	}
}

func TestBuildRejectsTimestampOutsideRFC3339Range(t *testing.T) {
	input := testInput()
	input.Manifest.Run.SealedAt = time.Date(10_000, time.January, 1, 0, 0, 0, 0, time.UTC)
	if _, err := Build(input, Limits{}); err == nil || !strings.Contains(err.Error(), "canonical RFC3339 range") {
		t.Fatalf("Build(out-of-range timestamp) error = %v", err)
	}
}

func TestBuildRejectsCrossDocumentReferenceMismatch(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Input)
	}{
		{
			name: "inventory receipt missing from manifest",
			mutate: func(input *Input) {
				input.Coverage.Inventory[0].ReceiptIDs = append(input.Coverage.Inventory[0].ReceiptIDs, "unknown-receipt")
			},
		},
		{
			name: "finding evidence receipt missing from manifest",
			mutate: func(input *Input) {
				unknown := "unknown-receipt"
				input.Findings.Findings[0].Evidence[0].ReceiptID = &unknown
			},
		},
		{
			name: "assessment attached to multiple findings",
			mutate: func(input *Input) {
				input.Findings.Findings[1].AssessmentIDs = append(input.Findings.Findings[1].AssessmentIDs, "assessment-2")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := testInput()
			test.mutate(&input)
			if _, err := Build(input, Limits{}); err == nil {
				t.Fatal("Build() error = nil")
			}
		})
	}
}

func TestNormalizeRepositoryURLRejectsMalformedURLForm(t *testing.T) {
	values := []string{
		"https://" + "alice" + ":" + "placeholder" + "@[2001:db8::1/repo.git",
		"1https://" + "alice" + ":" + "placeholder" + "@git.example/repo.git",
	}
	for _, value := range values {
		_, err := normalizeRepositoryURL(value, DefaultLimits())
		if err == nil {
			t.Fatalf("normalizeRepositoryURL(%q) accepted a malformed URL-form repository input", value)
		}
		if strings.Contains(err.Error(), "alice") || strings.Contains(err.Error(), "placeholder") {
			t.Fatalf("normalizeRepositoryURL(%q) exposed repository userinfo in its error", value)
		}
	}
}

func TestNormalizeRepositoryURLParseFailureRequiresValidatedSCP(t *testing.T) {
	valid := []struct {
		input string
		want  string
	}{
		{
			input: "git@git.example:Team/Repo%zz.git",
			want:  "git@git.example:Team/Repo%zz.git",
		},
		{
			input: "git@[2001:db8::1]:Team/Repo%zz.git?mirror=https://mirror.example#fragment",
			want:  "git@[2001:db8::1]:Team/Repo%zz.git",
		},
	}
	for _, test := range valid {
		t.Run(test.input, func(t *testing.T) {
			if _, err := url.Parse(strings.TrimSpace(test.input)); err == nil {
				t.Fatal("test input does not exercise the URL parse-failure path")
			}
			got, err := normalizeRepositoryURL(test.input, DefaultLimits())
			if err != nil {
				t.Fatalf("normalizeRepositoryURL() rejected validated SCP input: %v", err)
			}
			if got != test.want {
				t.Fatalf("normalizeRepositoryURL() = %q, want %q", got, test.want)
			}
		})
	}

	invalid := []string{
		"not-a-repository%zz",
		"alice-placeholder@git.example/secret-placeholder%zz",
		"git@@git.example:Team/Repo%zz",
		"git@[not-an-ipv6]:Team/Repo%zz",
		"git@...:Team/Repo%zz",
		`git@git.example:Team\Repo%zz`,
		"git@git.example:   ",
	}
	for _, input := range invalid {
		t.Run(input, func(t *testing.T) {
			if _, err := url.Parse(strings.TrimSpace(input)); err == nil {
				t.Fatal("test input does not exercise the URL parse-failure path")
			}
			_, err := normalizeRepositoryURL(input, DefaultLimits())
			if err == nil {
				t.Fatal("normalizeRepositoryURL() accepted a malformed non-SCP fallback")
			}
			for _, sensitive := range []string{"alice-placeholder", "secret-placeholder"} {
				if strings.Contains(err.Error(), sensitive) {
					t.Fatalf("normalizeRepositoryURL() exposed sensitive input in error: %v", err)
				}
			}
		})
	}
}

func TestNormalizeRepositoryURLPreservesSCPPathAt(t *testing.T) {
	tests := []string{
		"git.example:Team/@scope/Repo%zz.git",
		"git.example:Team/Repo@revision%zz.git",
		"[2001:db8::1]:Team/@scope/Repo%zz.git",
		"git@[2001:db8::1]:Team/@scope/Repo%zz.git",
	}
	for _, input := range tests {
		t.Run(input, func(t *testing.T) {
			got, err := normalizeRepositoryURL(input, DefaultLimits())
			if err != nil {
				t.Fatalf("normalizeRepositoryURL() rejected SCP path containing @: %v", err)
			}
			if got != input {
				t.Fatalf("normalizeRepositoryURL() = %q, want exact SCP spelling %q", got, input)
			}
		})
	}
}

func TestNormalizeRepositoryURLRejectsHostlessURLFormUserinfo(t *testing.T) {
	tests := []string{
		"https://" + "alice" + ":" + "placeholder" + "@/Team/Repo.git",
		"ssh://" + "git" + "@/Team/Repo.git",
		"https:///" + "alice" + ":" + "placeholder" + "@/Team/Repo.git",
		"git+ssh:/" + "alice" + ":" + "placeholder" + "@/Team/Repo.git",
		"git+ssh:" + "alice" + ":" + "placeholder" + "@/Team/Repo.git",
		"custom:/" + "alice" + ":" + "placeholder%zz" + "@/Team/Repo.git",
		"//" + "alice" + ":" + "placeholder" + "@/Team/Repo.git",
	}
	for _, value := range tests {
		_, err := normalizeRepositoryURL(value, DefaultLimits())
		if err == nil {
			t.Fatalf("normalizeRepositoryURL(%q) accepted hostless URL-form userinfo", value)
		}
		if strings.Contains(err.Error(), "alice") || strings.Contains(err.Error(), "placeholder") {
			t.Fatalf("normalizeRepositoryURL(%q) exposed repository userinfo in its error", value)
		}
	}
}

func TestNormalizeRepositoryURLRejectsHostlessNetworkForms(t *testing.T) {
	for _, value := range []string{
		"https:///git.example/Team/Repo.git",
		"https:/git.example/Team/Repo.git",
		"https:git.example/Team/Repo.git",
		"ssh:///git.example/Team/Repo.git",
		"git+ssh:/git.example/Team/Repo.git",
	} {
		if _, err := normalizeRepositoryURL(value, DefaultLimits()); err == nil {
			t.Fatalf("normalizeRepositoryURL(%q) accepted a hostless network URL", value)
		}
	}
}

func TestNormalizeRepositoryURLPreservesSCPFallback(t *testing.T) {
	got, err := normalizeRepositoryURL("git@git.example:Team/Repo.git?mirror=https://mirror.example#fragment", DefaultLimits())
	if err != nil {
		t.Fatalf("normalizeRepositoryURL() rejected SCP-style input: %v", err)
	}
	if got != "git@git.example:Team/Repo.git" {
		t.Fatalf("normalizeRepositoryURL() = %q", got)
	}
}

func TestNormalizeRepositoryURLPreservesSCPRepositoryPathWhitespace(t *testing.T) {
	for _, input := range []string{
		"git@git.example: Team/Repo.git ",
		"git.example:Team/Repo.git ",
		"git@[2001:db8::1]: Team/Repo.git ",
	} {
		t.Run(input, func(t *testing.T) {
			got, err := normalizeRepositoryURL(input, DefaultLimits())
			if err != nil {
				t.Fatalf("normalizeRepositoryURL() rejected SCP path whitespace: %v", err)
			}
			if got != input {
				t.Fatalf("normalizeRepositoryURL() = %q, want exact SCP spelling %q", got, input)
			}
		})
	}
}

func TestNormalizeRepositoryURLTrimsOnlyLeadingSCPFormattingWhitespace(t *testing.T) {
	const input = "  git@git.example: Team/Repo.git "
	const want = "git@git.example: Team/Repo.git "
	got, err := normalizeRepositoryURL(input, DefaultLimits())
	if err != nil {
		t.Fatalf("normalizeRepositoryURL() rejected SCP input: %v", err)
	}
	if got != want {
		t.Fatalf("normalizeRepositoryURL() = %q, want %q", got, want)
	}
}

func TestNormalizeRepositoryURLPreservesHostlessFileURL(t *testing.T) {
	const value = "file:///workspace/repo.git"
	got, err := normalizeRepositoryURL(value, DefaultLimits())
	if err != nil {
		t.Fatalf("normalizeRepositoryURL() rejected a hostless file URL: %v", err)
	}
	if got != value {
		t.Fatalf("normalizeRepositoryURL() = %q, want %q", got, value)
	}
}

func TestNormalizeRepositoryURLRechecksCanonicalLength(t *testing.T) {
	value := "https://git.example/Team Repo.git"
	canonical := "https://git.example/Team%20Repo.git"
	limits := DefaultLimits()
	limits.MaxStringBytes = len(value)
	if _, err := normalizeRepositoryURL(value, limits); err == nil || !strings.Contains(err.Error(), "exceeds maximum size") {
		t.Fatal("normalizeRepositoryURL() accepted a canonical URL beyond MaxStringBytes")
	}

	limits.MaxStringBytes = len(canonical)
	got, err := normalizeRepositoryURL(value, limits)
	if err != nil {
		t.Fatalf("normalizeRepositoryURL() rejected a URL at the canonical size limit: %v", err)
	}
	if got != canonical {
		t.Fatalf("normalizeRepositoryURL() = %q, want %q", got, canonical)
	}
}

func TestNormalizePathRejectsLeadingWindowsDrivePrefix(t *testing.T) {
	for _, value := range []string{"C:", "C:repo/file.go", "d:/repo/file.go"} {
		if _, err := normalizePath("path", value, false, DefaultLimits()); err == nil {
			t.Fatalf("normalizePath(%q) accepted a Windows drive-qualified path", value)
		}
	}
}

func TestNormalizePathAllowsColonAfterLeadingSegment(t *testing.T) {
	const value = "docs/C:notes/file.go"
	got, err := normalizePath("path", value, false, DefaultLimits())
	if err != nil {
		t.Fatalf("normalizePath(%q) error = %v", value, err)
	}
	if got != value {
		t.Fatalf("normalizePath(%q) = %q", value, got)
	}
}

func TestBuildRejectsRepositoryURLWithEmptySanitizedFallback(t *testing.T) {
	for _, repoURL := range []string{"?mode=read", "#fragment", "?mode=read#fragment"} {
		t.Run(repoURL, func(t *testing.T) {
			input := testInput()
			input.Manifest.Repository.RepoURL = repoURL
			_, err := Build(input, Limits{})
			if err == nil || !strings.Contains(err.Error(), "repository.repoURL is required after sanitization") {
				t.Fatalf("Build() error = %v", err)
			}
		})
	}
}

func TestNormalizeMediaTypeEnforcesLimitAfterFormatting(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxStringBytes = len("text/plain")
	_, err := normalizeMediaType("text/plain", limits)
	if err == nil || !strings.Contains(err.Error(), "mediaType exceeds maximum size") {
		t.Fatalf("normalizeMediaType() error = %v", err)
	}
}

func TestBuildStripsRepositoryURLCredentialsAndQuery(t *testing.T) {
	input := testInput()
	input.Manifest.Repository.RepoURL = "https://" + "alice" + ":" + "example" + "@git.example/Team/Repo.git?mode=read#fragment"
	built := mustBuild(t, input)
	var manifest manifestDocument
	if err := json.Unmarshal(built.ManifestJSON, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Repository.RepoURL != "https://git.example/Team/Repo.git" {
		t.Fatalf("sanitized repoURL = %q", manifest.Repository.RepoURL)
	}
}

func TestNormalizePathRejectsDriveQualifiedPathAfterCleaning(t *testing.T) {
	for _, value := range []string{
		"safe/../C:/repo/file.go",
		"safe/../d:repo/file.go",
	} {
		if _, err := normalizePath("path", value, false, DefaultLimits()); err == nil {
			t.Fatalf("normalizePath(%q) accepted a drive-qualified path exposed by cleaning", value)
		}
	}
}
