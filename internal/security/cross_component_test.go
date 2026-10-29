package security

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

func TestThreatModelInvestigationsRoundTripThroughMarkdown(t *testing.T) {
	items := []SecurityInvestigation{
		{Title: "Caller identity", Question: "Can a cached value replace the caller identity a backend receives?", Paths: []string{"pkg/cache/", "../escape", "pkg/auth/token.go"}},
		{Question: "Do generated routes apply the handler checks?"},
		{Title: "duplicate", Question: "do generated routes apply the handler checks?"},
		{Title: "empty"},
	}
	content := RenderThreatModelInvestigations("# Threat Model\n\nBody.\n", items)
	if !strings.HasPrefix(content, "# Threat Model\n\nBody.") || !strings.Contains(content, threatModelInvestigationsHeading) {
		t.Fatalf("rendered threat model = %q", content)
	}

	got := ThreatModelInvestigations(content)
	if len(got) != 2 {
		t.Fatalf("investigations = %#v, want the two valid, distinct questions", got)
	}
	if strings.Join(got[0].Paths, ",") != "pkg/cache,pkg/auth/token.go" {
		t.Fatalf("paths = %v, want unsafe path dropped and directory trimmed", got[0].Paths)
	}
	if got[1].Title != "Do generated routes apply the handler checks?" {
		t.Fatalf("title = %q, want question as fallback title", got[1].Title)
	}

	// Re-rendering replaces the section instead of appending a second one.
	again := RenderThreatModelInvestigations(content, got[:1])
	if strings.Count(again, threatModelInvestigationsMarker) != 1 || len(ThreatModelInvestigations(again)) != 1 {
		t.Fatalf("re-rendered threat model kept stale investigations:\n%s", again)
	}
	if cleared := RenderThreatModelInvestigations(content, nil); strings.Contains(cleared, threatModelInvestigationsMarker) || !strings.Contains(cleared, "Body.") {
		t.Fatalf("cleared threat model = %q", cleared)
	}
	if ThreatModelInvestigations("# Threat Model\n"+threatModelInvestigationsMarker+"\n```json\nnot json\n```\n") != nil {
		t.Fatal("malformed investigations block was accepted")
	}
}

func TestParseThreatModelResultRendersInvestigations(t *testing.T) {
	binding := testResultBinding()
	result := ThreatModelResultEnvelope{
		SchemaVersion:  AgentResultSchemaVersion,
		Kind:           AgentResultKindThreatModel,
		RepositoryScan: binding.RepositoryScan,
		ScanID:         binding.ScanID,
		PolicyDigest:   binding.PolicyDigest,
		ThreatModel:    "# Threat Model\n\nBody.",
		Investigations: []SecurityInvestigation{{Title: "Flow", Question: "Does input reach the sink?", Paths: []string{"cmd"}}},
	}
	content, err := ParseThreatModelResult(mustMarshalSecurityResult(t, result), AgentResultBinding{
		RepositoryScan: binding.RepositoryScan, ScanID: binding.ScanID, PolicyDigest: binding.PolicyDigest,
	})
	if err != nil {
		t.Fatalf("ParseThreatModelResult() error = %v", err)
	}
	got := ThreatModelInvestigations(content)
	if len(got) != 1 || got[0].Question != "Does input reach the sink?" {
		t.Fatalf("investigations = %#v", got)
	}

	// An incremental result that omits investigations keeps the saved ones.
	result.ThreatModel, result.Investigations = content, nil
	updated, err := ParseThreatModelResult(mustMarshalSecurityResult(t, result), AgentResultBinding{
		RepositoryScan: binding.RepositoryScan, ScanID: binding.ScanID, PolicyDigest: binding.PolicyDigest,
	})
	if err != nil {
		t.Fatalf("ParseThreatModelResult(without investigations) error = %v", err)
	}
	if got := ThreatModelInvestigations(updated); len(got) != 1 {
		t.Fatalf("investigations after an incremental result = %#v, want the saved one kept", got)
	}
}

func TestValidateFindingsV2AcceptsReferenceFileEvidence(t *testing.T) {
	manifest := ReviewContextManifest{
		SchemaVersion: SchemaVersionReviewContext,
		SliceID:       "slice_app",
		IncludedFiles: []ReviewContextIncludedFile{{
			Path:               "app.go",
			IncludedLineRanges: []ReviewContextLineRange{{StartLine: 1, EndLine: 2}},
			Readable:           true,
		}},
		ReferenceFiles: []ReviewContextReferenceFile{
			{Path: "app.go", Lines: 40, Relation: ReferenceRelationSlice},
			{Path: "pkg/consumer/consumer.go", Lines: 10, Relation: ReferenceRelationImportedBy},
		},
	}
	validate := func(ref FindingsV2EvidenceRef) FindingValidationResult {
		finding := validFinding()
		finding.Evidence = append(finding.Evidence, ref)
		return ValidateFindingsV2(FindingsV2Artifact{
			SchemaVersion: SchemaVersionFindingsV2,
			Scan:          FindingsV2Scan{Mode: "initial", SliceID: "slice_app"},
			Findings:      []FindingsV2Finding{finding},
		}, manifest, FindingValidationOptions{Namespace: "default", RepositoryScan: "repo", ScanRunID: "scan1", TaskName: "task1"})
	}

	accepted := validate(FindingsV2EvidenceRef{Path: "pkg/consumer/consumer.go", StartLine: 3, EndLine: 9, Role: " Sink "})
	if len(accepted.Accepted) != 1 {
		t.Fatalf("reference evidence dropped: %#v", accepted.Dropped)
	}
	var role string
	for _, ref := range accepted.Accepted[0].Evidence {
		if ref.Path == "pkg/consumer/consumer.go" {
			role = ref.Role
		}
	}
	if role != "sink" {
		t.Fatalf("evidence role = %q, want normalized sink", role)
	}
	if got := validate(FindingsV2EvidenceRef{Path: "app.go", StartLine: 30, EndLine: 35, Role: "made-up"}); len(got.Accepted) != 1 || got.Accepted[0].Evidence[1].Role != "" {
		t.Fatalf("slice file past its excerpt = %#v, want accepted with unknown role dropped", got)
	}
	for name, ref := range map[string]FindingsV2EvidenceRef{
		"past end of file":  {Path: "pkg/consumer/consumer.go", StartLine: 9, EndLine: 11},
		"unlisted file":     {Path: "pkg/other/other.go", StartLine: 1, EndLine: 2},
		"inverted in range": {Path: "pkg/consumer/consumer.go", StartLine: 5, EndLine: 4},
	} {
		// The unverifiable citation is removed; the verified one keeps the finding.
		got := validate(ref)
		if len(got.Accepted) != 1 || len(got.Accepted[0].Evidence) != 1 || got.Accepted[0].Evidence[0].Path != "app.go" {
			t.Fatalf("%s: result = %#v, want the finding with only its verified evidence", name, got)
		}
		// A finding whose only citation is unverifiable is dropped.
		only := validFinding()
		only.Evidence = []FindingsV2EvidenceRef{ref}
		dropped := ValidateFindingsV2(FindingsV2Artifact{
			SchemaVersion: SchemaVersionFindingsV2,
			Scan:          FindingsV2Scan{Mode: "initial", SliceID: "slice_app"},
			Findings:      []FindingsV2Finding{only},
		}, manifest, FindingValidationOptions{Namespace: "default", RepositoryScan: "repo", ScanRunID: "scan1", TaskName: "task1"})
		if len(dropped.Accepted) != 0 || len(dropped.Dropped) != 1 {
			t.Fatalf("%s: unverifiable-only finding = %#v, want dropped", name, dropped)
		}
	}
}

func TestParseFindingsResultBoundsCoverageNotes(t *testing.T) {
	scan := FindingsV2Scan{
		ReviewedFiles: []string{"a.go", "a.go", "../secret", "b.go"},
		ResolvedQuestions: []FindingsV2ResolvedQuestion{
			{Question: " Is input authenticated? ", Resolution: "Yes, by middleware."},
			{Resolution: "no question"},
		},
	}
	for range maxReviewedFiles {
		scan.ReviewedFiles = append(scan.ReviewedFiles, "c.go")
	}
	normalizeFindingsCoverage(&scan)
	if strings.Join(scan.ReviewedFiles, ",") != "a.go,b.go,c.go" {
		t.Fatalf("reviewedFiles = %v", scan.ReviewedFiles)
	}
	if len(scan.ResolvedQuestions) != 1 || scan.ResolvedQuestions[0].Question != "Is input authenticated?" {
		t.Fatalf("resolvedQuestions = %#v", scan.ResolvedQuestions)
	}
}

func TestBuildReviewResultPromptExplainsCrossComponentScope(t *testing.T) {
	scan := &corev1alpha1.RepositoryScan{Spec: corev1alpha1.RepositoryScanSpec{RepoURL: "https://github.com/example/project", Branch: "main"}}
	manifest := testReviewContextManifest("slice_api")
	manifest.ReferenceFiles = []ReviewContextReferenceFile{
		{Path: "api/handler.go", Lines: 10, Relation: ReferenceRelationSlice},
		{Path: "store/db.go", Lines: 10, Relation: ReferenceRelationImports},
		{Path: "cmd/server/main.go", Lines: 10, Relation: ReferenceRelationImportedBy},
	}
	slice := store.ReviewSlice{ID: "slice_api", Title: "API", Kind: "package"}
	got := BuildReviewResultPrompt(scan, "initial", "", "", "", slice, testResultBinding(), manifest, FindingsV2Repository{RepoURL: scan.Spec.RepoURL, Branch: "main"})
	for _, want := range []string{"REFERENCE FILES", "files in packages this slice imports: store/db.go", "files in packages that import this slice: cmd/server/main.go", "scan.resolvedQuestions", "root_control"} {
		if !strings.Contains(got, want) {
			t.Fatalf("review prompt missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "INVESTIGATION SLICE") {
		t.Fatal("package slice prompt contains investigation instructions")
	}

	investigation := store.ReviewSlice{ID: "slice_inv", Title: "Investigation: flow", Kind: SliceKindInvestigation, Summary: "Does input reach the sink?"}
	manifest.ReferenceFiles = []ReviewContextReferenceFile{{Path: "a.go", Lines: 1, Relation: ReferenceRelationRepository}}
	got = BuildReviewResultPrompt(scan, "initial", "", "", "", investigation, testResultBinding(), manifest, FindingsV2Repository{RepoURL: scan.Spec.RepoURL, Branch: "main"})
	for _, want := range []string{"INVESTIGATION SLICE", "Does input reach the sink?", "any of the 1 indexed repository"} {
		if !strings.Contains(got, want) {
			t.Fatalf("investigation prompt missing %q:\n%s", want, got)
		}
	}
}

func TestBuildThreatModelResultPromptRequestsInvestigationsForFullScans(t *testing.T) {
	scan := &corev1alpha1.RepositoryScan{Spec: corev1alpha1.RepositoryScanSpec{RepoURL: "https://github.com/example/project", Branch: "main"}}
	if got := BuildThreatModelResultPrompt(scan, "initial", "", "", "", testResultBinding()); !strings.Contains(got, "investigations array") {
		t.Fatalf("full-scan threat model prompt does not request investigations:\n%s", got)
	}
	if got := BuildThreatModelResultPrompt(scan, "incremental", "base", "head", "", testResultBinding()); strings.Contains(got, "investigations array") {
		t.Fatalf("incremental threat model prompt requests investigations:\n%s", got)
	}
}

// Shapes taken from live reviews: resolved questions written as
// {question, answer, evidence}, an extra scan.limitations key, and narrative
// fields returned as structured JSON.
func TestParseFindingsResultToleratesDescriptiveShapeDrift(t *testing.T) {
	binding := testResultBinding()
	repository := FindingsV2Repository{RepoURL: "https://github.com/example/project", Branch: "main"}
	data := []byte(`{"schemaVersion":1,"kind":"orka.security.findings.v1","repositoryScan":"project","scanId":"scan_123","sliceId":"slice_inv","policyDigest":"sha256:policy","contextDigest":"sha256:context",` +
		`"findings":{"schemaVersion":2,"repository":{"repoURL":"https://github.com/example/project","branch":"main","subPath":"","baseSHA":"","headSHA":""},` +
		`"scan":{"mode":"initial","sliceId":"slice_inv","summary":"Traced the flow.","reviewedFiles":["a.go"],` +
		`"resolvedQuestions":[{"question":"Is the key bound?","answer":"Yes.","evidence":[{"path":"a.go","startLine":1}]},"Is it cached?"],"limitations":["No runtime checks."],"confidenceNotes":"extra"},` +
		`"findings":[{"title":"Key replaced","category":"authz","severity":"medium","confidence":"high","triage":"needs-review","evidence":[{"path":"a.go","startLine":1,"endLine":2,"role":"sink"}],` +
		`"summary":"s","rootCause":"r","reproduction":{"steps":["a"]},"remediation":"fix","suggestedAction":"a","whyTestsDoNotAlreadyCoverThis":"w","suggestedRegressionTest":"t","minimumFixScope":"m","exploitability":"high"}]}}`)
	artifact, err := ParseFindingsResult(data, FindingsResultExpectation{Binding: binding, SliceID: "slice_inv", Mode: "initial", Repository: repository})
	if err != nil {
		t.Fatalf("ParseFindingsResult() error = %v", err)
	}
	questions := artifact.Scan.ResolvedQuestions
	if len(questions) != 2 || questions[0].Resolution != "Yes.\nEvidence: [{\"path\":\"a.go\",\"startLine\":1}]" || questions[1].Question != "Is it cached?" {
		t.Fatalf("resolvedQuestions = %#v", questions)
	}
	if len(artifact.Scan.Limitations) != 1 || len(artifact.Findings) != 1 || artifact.Findings[0].Reproduction != `{"steps":["a"]}` {
		t.Fatalf("artifact = %#v", artifact)
	}

	// Identity stays strict.
	mismatched := strings.Replace(string(data), `"mode":"initial"`, `"mode":"incremental"`, 1)
	if _, err := ParseFindingsResult([]byte(mismatched), FindingsResultExpectation{Binding: binding, SliceID: "slice_inv", Mode: "initial", Repository: repository}); err == nil {
		t.Fatal("ParseFindingsResult() accepted a mismatched scan mode")
	}
	unknownEnvelopeKey := strings.Replace(string(data), `"kind":`, `"note":"x","kind":`, 1)
	if _, err := ParseFindingsResult([]byte(unknownEnvelopeKey), FindingsResultExpectation{Binding: binding, SliceID: "slice_inv", Mode: "initial", Repository: repository}); err == nil {
		t.Fatal("ParseFindingsResult() accepted an unknown envelope key")
	}
}

// Calibrated ratings and a single-string list.
func TestParseValidationResultAcceptsCalibratedRatings(t *testing.T) {
	finding := &store.Finding{ID: "fnd_1", ScanRunID: "scan_123"}
	expected := ValidationResultExpectation{
		Binding: AgentResultBinding{RepositoryScan: "vekil", ScanID: "scan_123", PolicyDigest: "sha256:policy"},
		Finding: finding,
	}
	data := []byte(`{"schemaVersion":1,"kind":"orka.security.validation.v1","repositoryScan":"vekil","scanId":"scan_123","findingId":"fnd_1","policyDigest":"sha256:policy",` +
		`"validation":{"version":1,"finding_id":"fnd_1","status":"validated","summary":"Confirmed.","severity":"Medium","confidence":"high","assumptions":"Feature flag enabled.","exploit_notes":"extra"}}`)
	artifact, err := ParseValidationResult(data, expected)
	if err != nil {
		t.Fatalf("ParseValidationResult() error = %v", err)
	}
	if artifact.Severity != "medium" || artifact.Confidence != "high" || len(artifact.Assumptions) != 1 {
		t.Fatalf("artifact = %#v", artifact)
	}
	bad := strings.Replace(string(data), `"status":"validated"`, `"status":"maybe"`, 1)
	if _, err := ParseValidationResult([]byte(bad), expected); err == nil {
		t.Fatal("ParseValidationResult() accepted an unknown status")
	}
}

func TestMarshalReviewContextKeepsManifestWithinLimit(t *testing.T) {
	manifest := ReviewContextManifest{SchemaVersion: SchemaVersionReviewContext, SliceID: "slice_inv"}
	long := strings.Repeat("d/", 120)
	for i := range MaxReviewContextReferenceFiles {
		manifest.ReferenceFiles = append(manifest.ReferenceFiles, ReviewContextReferenceFile{
			Path: fmt.Sprintf("%sfile%04d.go", long, i), Lines: 100, Relation: ReferenceRelationRepository,
		})
	}
	data, err := MarshalReviewContext(manifest)
	if err != nil {
		t.Fatalf("MarshalReviewContext() error = %v", err)
	}
	if len(data) > maxReviewContextBytes {
		t.Fatalf("manifest = %d bytes, want at most %d", len(data), maxReviewContextBytes)
	}
	var got ReviewContextManifest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if len(got.ReferenceFiles) == 0 || len(got.ReferenceFiles) == len(manifest.ReferenceFiles) || got.ReferenceFiles[0] != manifest.ReferenceFiles[0] {
		t.Fatalf("kept %d of %d references, want a trimmed prefix", len(got.ReferenceFiles), len(manifest.ReferenceFiles))
	}
}

func TestFindingsV2ScanAcceptsASingleResolvedQuestion(t *testing.T) {
	for name, data := range map[string]string{
		"object": `{"resolvedQuestions":{"question":"Is the key bound?","answer":"Yes."}}`,
		"string": `{"resolvedQuestions":"Is the key bound?"}`,
	} {
		var scan FindingsV2Scan
		if err := json.Unmarshal([]byte(data), &scan); err != nil {
			t.Fatalf("%s: Unmarshal() error = %v", name, err)
		}
		if len(scan.ResolvedQuestions) != 1 || scan.ResolvedQuestions[0].Question != "Is the key bound?" {
			t.Fatalf("%s: resolvedQuestions = %#v", name, scan.ResolvedQuestions)
		}
	}
}

func TestMarshalReviewContextsKeepsTheRunWithinTheUploadBound(t *testing.T) {
	long := strings.Repeat("d/", 100)
	manifests := make([]ReviewContextManifest, 50)
	for i := range manifests {
		manifests[i] = ReviewContextManifest{SchemaVersion: SchemaVersionReviewContext, SliceID: fmt.Sprintf("slice_%02d", i)}
		for j := range 4000 {
			manifests[i].ReferenceFiles = append(manifests[i].ReferenceFiles, ReviewContextReferenceFile{
				Path: fmt.Sprintf("%sfile%04d.go", long, j), Lines: 10, Relation: ReferenceRelationRepository,
			})
		}
	}
	encoded, err := MarshalReviewContexts(manifests, MaxReviewContextsBytes)
	if err != nil {
		t.Fatalf("MarshalReviewContexts() error = %v", err)
	}
	total := 0
	for i, data := range encoded {
		total += len(data)
		var got ReviewContextManifest
		if err := json.Unmarshal(data, &got); err != nil || got.SliceID != manifests[i].SliceID || len(got.ReferenceFiles) == 0 {
			t.Fatalf("context %d = %d references (%v), want its own trimmed listing", i, len(got.ReferenceFiles), err)
		}
	}
	if total > MaxReviewContextsBytes {
		t.Fatalf("contexts total %d bytes, want at most %d", total, MaxReviewContextsBytes)
	}
}

func TestMarshalReviewContextsFailsWhenExcerptsAloneExceedTheBound(t *testing.T) {
	prompt := strings.Repeat("x", 1<<20)
	manifests := make([]ReviewContextManifest, MaxReviewContextsBytes/(1<<20)+2)
	for i := range manifests {
		manifests[i] = ReviewContextManifest{SchemaVersion: SchemaVersionReviewContext, SliceID: fmt.Sprintf("slice_%02d", i), Prompt: prompt}
	}
	if _, err := MarshalReviewContexts(manifests, MaxReviewContextsBytes); err == nil {
		t.Fatal("MarshalReviewContexts() = nil error, want the oversized batch rejected")
	}
}

func TestReviewPromptNamesTheLastIndexedRepositoryFile(t *testing.T) {
	scan := &corev1alpha1.RepositoryScan{Spec: corev1alpha1.RepositoryScanSpec{RepoURL: "https://github.com/example/project", Branch: "main"}}
	manifest := testReviewContextManifest("slice_inv")
	manifest.ReferenceFiles = []ReviewContextReferenceFile{
		{Path: "api/a.go", Lines: 10, Relation: ReferenceRelationRepository},
		{Path: "store/b.go", Lines: 10, Relation: ReferenceRelationRepository},
		{Path: "deploy/config.yaml", Lines: 10, Relation: ReferenceRelationRepository},
	}
	slice := store.ReviewSlice{ID: "slice_inv", Title: "Investigation", Kind: SliceKindInvestigation}
	got := BuildReviewResultPrompt(scan, "initial", "", "", "", slice, testResultBinding(), manifest, FindingsV2Repository{RepoURL: scan.Spec.RepoURL, Branch: "main"})
	if !strings.Contains(got, "any of the 3 indexed repository files") || !strings.Contains(got, "through deploy/config.yaml;") {
		t.Fatalf("review prompt does not name the index boundary:\n%s", got)
	}
}
