package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	securitybundle "github.com/orka-agents/orka/internal/security/bundle"
	"github.com/orka-agents/orka/internal/store"
)

func validSecurityScanBundle(t *testing.T, scanRunID string) *store.SecurityScanBundle {
	t.Helper()
	return validSecurityScanBundleWithEvidence(t, scanRunID, nil)
}

func validSecurityScanBundleWithEvidence(
	t *testing.T,
	scanRunID string,
	evidence []securitybundle.EvidenceBlobInput,
) *store.SecurityScanBundle {
	t.Helper()
	if evidence == nil {
		evidence = []securitybundle.EvidenceBlobInput{}
	}
	started := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	completed := started.Add(time.Minute)
	sealedAt := completed.Add(time.Second)
	publicRunID := scanRunID
	built, err := securitybundle.Build(securitybundle.Input{
		Manifest: securitybundle.ManifestInput{
			SchemaVersion: securitybundle.SchemaVersion,
			Repository: securitybundle.RepositoryIdentity{
				Provider: "github", RepositoryID: "target-v1", RepoURL: "https://github.com/example/repo",
			},
			Target: securitybundle.TargetSnapshot{
				CommitSHA: strings.Repeat("a", 40), TreeDigest: integrityTestDigest("tree"), TargetID: "target-v1",
				ReceiptID: "target-receipt", ReceiptDigest: integrityTestDigest("target-receipt"),
			},
			ThreatModel: securitybundle.ThreatModelInput{Version: "1", Content: "# Threat model\n"},
			Quality: securitybundle.QualitySummary{
				InventoryCoverage: "complete", CandidateCoverage: "complete", Coverage: "complete",
				ValidationScope: "all", ValidationExecution: "complete", AttackPathExecution: "complete",
				AnalysisAttestation: "tool-observed", TargetVerification: "verified",
				Authorization: "verified", Isolation: "hardened",
			},
			Versions: securitybundle.ComponentVersions{
				Schema: "security-bundle-v1", Controller: "controller-v1", Additional: map[string]string{},
			},
			OccurrenceIDs: []string{}, AssessmentIDs: []string{}, StageReceiptIDs: []string{},
			EvidenceReceiptIDs: []string{}, Metadata: map[string]string{},
			Run: securitybundle.RunEnvelope{
				RunUID:      "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				PublicRunID: &publicRunID, Namespace: "ns", RepositoryScanName: "repo",
				RepositoryScanUID: "scan-uid", RepositoryScanGeneration: 2,
				StartedAt: started, CompletedAt: &completed, SealedAt: sealedAt,
			},
		},
		Findings: securitybundle.FindingsInput{SchemaVersion: securitybundle.SchemaVersion, Findings: []securitybundle.Finding{}, Metadata: map[string]string{}},
		Coverage: securitybundle.CoverageInput{
			SchemaVersion: securitybundle.SchemaVersion, InventoryStatus: "complete", CandidateStatus: "complete",
			CoverageStatus: "complete", Inventory: []securitybundle.InventoryCoverageEntry{},
			Candidates: []securitybundle.CandidateCoverageEntry{}, Stages: []securitybundle.StageCoverageEntry{},
			Metadata: map[string]string{},
		},
		Evidence: evidence,
	}, securitybundle.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	canonicalEvidence := built.Evidence
	if canonicalEvidence == nil {
		canonicalEvidence = []securitybundle.EvidenceBlob{}
	}
	evidenceJSON, err := json.Marshal(canonicalEvidence)
	if err != nil {
		t.Fatal(err)
	}
	return &store.SecurityScanBundle{
		ID: "bundle_" + scanRunID, Namespace: "ns", RepositoryScan: "repo", RepositoryScanUID: "scan-uid",
		RepositoryScanGeneration: 2, ScanRunID: scanRunID,
		RunUID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Version: securitybundle.SchemaVersion,
		ManifestJSON: built.ManifestJSON, FindingsJSON: built.FindingsJSON, CoverageJSON: built.CoverageJSON,
		EvidenceJSON: evidenceJSON, ContentDigest: built.Roots.ContentDigest, RunReceiptDigest: built.Roots.RunReceiptDigest,
		SealedAt: sealedAt,
	}
}

func TestSecurityScanBundleAtomicImmutableReplay(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	bundle := validSecurityScanBundle(t, "scan")
	created, err := s.SealSecurityScanBundle(ctx, bundle)
	if err != nil || !created {
		t.Fatalf("SealSecurityScanBundle(first) = %v, %v", created, err)
	}
	created, err = s.SealSecurityScanBundle(ctx, bundle)
	if err != nil || created {
		t.Fatalf("SealSecurityScanBundle(replay) = %v, %v", created, err)
	}
	conflict := *bundle
	conflict.ContentDigest = integrityTestDigest("different-content")
	if _, err := s.SealSecurityScanBundle(ctx, &conflict); !errors.Is(err, store.ErrValidation) && !errors.Is(err, store.ErrDuplicateMismatch) {
		t.Fatalf("conflicting seal error = %v", err)
	}
	got, err := s.GetSecurityScanBundle(ctx, "ns", "scan")
	if err != nil || string(got.ManifestJSON) != string(bundle.ManifestJSON) || got.RepositoryScanGeneration != 2 {
		t.Fatalf("GetSecurityScanBundle() = %#v, %v", got, err)
	}
	if _, err := s.db.Exec(`DELETE FROM security_scan_bundles WHERE id = ?`, bundle.ID); err == nil {
		t.Fatal("sealed bundle deletion error = nil")
	}
}

func TestSecurityScanBundleEvidenceRequiresCanonicalStrictJSON(t *testing.T) {
	tests := map[string]func(string) string{
		"duplicate field": func(payload string) string {
			return strings.Replace(payload, `"Name":`, `"Name":"evidence/trace.txt","Name":`, 1)
		},
		"case-folded field": func(payload string) string {
			return strings.Replace(payload, `"Name":`, `"name":`, 1)
		},
		"unknown field": func(payload string) string {
			return strings.Replace(payload, `{`, `{"Unknown":true,`, 1)
		},
		"noncanonical serialization": func(payload string) string {
			return payload + "\n"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			s := setupTestStore(t)
			bundle := validSecurityScanBundleWithEvidence(t, "scan-evidence-"+strings.ReplaceAll(name, " ", "-"), []securitybundle.EvidenceBlobInput{{
				Name: "evidence/trace.txt", MediaType: "text/plain", Data: []byte("trace\n"),
			}})
			bundle.EvidenceJSON = []byte(mutate(string(bundle.EvidenceJSON)))
			if _, err := s.SealSecurityScanBundle(context.Background(), bundle); !errors.Is(err, store.ErrValidation) {
				t.Fatalf("SealSecurityScanBundle(%s) error = %v, want ErrValidation", name, err)
			}
		})
	}
}

func TestSecurityScanBundleEvidenceRejectsTopLevelNull(t *testing.T) {
	s := setupTestStore(t)
	bundle := validSecurityScanBundle(t, "scan-null-evidence")
	bundle.EvidenceJSON = []byte("null")
	if _, err := s.SealSecurityScanBundle(context.Background(), bundle); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("SealSecurityScanBundle(null evidence) error = %v, want ErrValidation", err)
	}
}

func TestSecurityScanBundleRejectsOversizedEvidenceArrayCardinality(t *testing.T) {
	maxItems := securitybundle.DefaultLimits().MaxEvidenceBlobs
	var payload strings.Builder
	payload.WriteByte('[')
	for i := 0; i <= maxItems; i++ {
		if i > 0 {
			payload.WriteByte(',')
		}
		payload.WriteString("null")
	}
	payload.WriteByte(']')

	if _, err := decodeCanonicalSecurityBundleEvidence([]byte(payload.String()), securitybundle.DefaultLimits()); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("decodeCanonicalSecurityBundleEvidence() error = %v, want ErrValidation", err)
	} else if !strings.Contains(err.Error(), fmt.Sprintf("exceeds %d items", maxItems)) {
		t.Fatalf("decodeCanonicalSecurityBundleEvidence() error = %v, want cardinality limit", err)
	}
}

func TestSecurityScanBundleEvidenceEnforcesDecodedByteLimitsDuringStreamingDecode(t *testing.T) {
	tests := []struct {
		name       string
		blobs      []securitybundle.EvidenceBlob
		limits     securitybundle.Limits
		wantErrSub string
	}{
		{
			name: "per blob",
			blobs: []securitybundle.EvidenceBlob{{
				Name: "evidence/one.bin", MediaType: "application/octet-stream", Size: 5,
				Digest: integrityTestDigest("one"), Data: []byte("12345"),
			}},
			limits: func() securitybundle.Limits {
				limits := securitybundle.DefaultLimits()
				limits.MaxEvidenceBlobBytes = 4
				return limits
			}(),
			wantErrSub: `blob "evidence/one.bin" exceeds maximum decoded size of 4 bytes`,
		},
		{
			name: "cumulative",
			blobs: []securitybundle.EvidenceBlob{
				{Name: "evidence/one.bin", MediaType: "application/octet-stream", Size: 3, Digest: integrityTestDigest("one"), Data: []byte("123")},
				{Name: "evidence/two.bin", MediaType: "application/octet-stream", Size: 3, Digest: integrityTestDigest("two"), Data: []byte("456")},
			},
			limits: func() securitybundle.Limits {
				limits := securitybundle.DefaultLimits()
				limits.MaxEvidenceBlobBytes = 3
				limits.MaxTotalEvidenceBytes = 5
				return limits
			}(),
			wantErrSub: "exceeds maximum total decoded size of 5 bytes",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload, err := json.Marshal(test.blobs)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeCanonicalSecurityBundleEvidence(payload, test.limits); !errors.Is(err, store.ErrValidation) {
				t.Fatalf("decodeCanonicalSecurityBundleEvidence() error = %v, want ErrValidation", err)
			} else if !strings.Contains(err.Error(), test.wantErrSub) {
				t.Fatalf("decodeCanonicalSecurityBundleEvidence() error = %v, want substring %q", err, test.wantErrSub)
			}
		})
	}
}

func TestSecurityScanBundleEvidenceAcceptsDecodedByteLimitBoundaries(t *testing.T) {
	blobs := []securitybundle.EvidenceBlob{
		{Name: "evidence/one.bin", MediaType: "application/octet-stream", Size: 3, Digest: integrityTestDigest("one"), Data: []byte("123")},
		{Name: "evidence/two.bin", MediaType: "application/octet-stream", Size: 3, Digest: integrityTestDigest("two"), Data: []byte("456")},
	}
	payload, err := json.Marshal(blobs)
	if err != nil {
		t.Fatal(err)
	}
	limits := securitybundle.DefaultLimits()
	limits.MaxEvidenceBlobBytes = 3
	limits.MaxTotalEvidenceBytes = 6
	got, err := decodeCanonicalSecurityBundleEvidence(payload, limits)
	if err != nil {
		t.Fatalf("decodeCanonicalSecurityBundleEvidence() error = %v", err)
	}
	if len(got) != len(blobs) {
		t.Fatalf("decoded evidence count = %d, want %d", len(got), len(blobs))
	}
}

func TestSecurityScanBundleAcceptsCanonicalEvidenceJSON(t *testing.T) {
	s := setupTestStore(t)
	bundle := validSecurityScanBundleWithEvidence(t, "scan-canonical-evidence", []securitybundle.EvidenceBlobInput{{
		Name: "evidence/trace.txt", MediaType: "text/plain", Data: []byte("trace\n"),
	}})
	created, err := s.SealSecurityScanBundle(context.Background(), bundle)
	if err != nil || !created {
		t.Fatalf("SealSecurityScanBundle(canonical evidence) = %v, %v", created, err)
	}
}

func TestSecurityScanBundleRequiresStableTimestamp(t *testing.T) {
	s := setupTestStore(t)
	bundle := validSecurityScanBundle(t, "scan-timestamp")
	bundle.SealedAt = time.Time{}
	if _, err := s.SealSecurityScanBundle(context.Background(), bundle); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("SealSecurityScanBundle(missing timestamp) error = %v, want ErrValidation", err)
	}
}

func TestSecurityScanBundleRejectsMalformedJSON(t *testing.T) {
	s := setupTestStore(t)
	bundle := validSecurityScanBundle(t, "scan-invalid")
	bundle.ManifestJSON = []byte(`not-json`)
	if _, err := s.SealSecurityScanBundle(context.Background(), bundle); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("SealSecurityScanBundle(malformed) error = %v, want ErrValidation", err)
	}
}
