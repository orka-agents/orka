package api

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	sqlitestore "github.com/orka-agents/orka/internal/store/sqlite"
)

func TestSecurityRunBoundToRepositoryScanRequiresCurrentGeneration(t *testing.T) {
	db, err := sqlitestore.NewDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := sqlitestore.NewStore(db, ":memory:")
	run := &store.ScanRun{
		ID: "scan-1", RunUID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Namespace: "ns", RepositoryScan: "repo", RepositoryScanUID: "scan-uid", RepositoryScanGeneration: 1,
		TaskName: "task", Mode: "manual", Phase: "succeeded", StartedAt: time.Now().UTC(),
		Quality: store.LegacyScanQuality(),
	}
	if err := s.CreateScanRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	handlers := &Handlers{securityStore: s}
	scan := &corev1alpha1.RepositoryScan{ObjectMeta: metav1.ObjectMeta{
		Name: "repo", Namespace: "ns", UID: types.UID("scan-uid"), Generation: 2,
	}}
	bound, err := handlers.securityRunBoundToRepositoryScan(context.Background(), scan, run.ID)
	if err != nil || bound {
		t.Fatalf("securityRunBoundToRepositoryScan(stale generation) = %v, %v, want false", bound, err)
	}
	scan.Generation = run.RepositoryScanGeneration
	bound, err = handlers.securityRunBoundToRepositoryScan(context.Background(), scan, run.ID)
	if err != nil || !bound {
		t.Fatalf("securityRunBoundToRepositoryScan(current generation) = %v, %v, want true", bound, err)
	}
	scan.UID = types.UID("recreated-uid")
	bound, err = handlers.securityRunBoundToRepositoryScan(context.Background(), scan, run.ID)
	if err != nil || bound {
		t.Fatalf("securityRunBoundToRepositoryScan(recreated) = %v, %v, want false", bound, err)
	}
}

func TestPatchProposalMatchesAuthorizedFindingSupportsLegacyProjection(t *testing.T) {
	head := strings.Repeat("a", 40)
	finding := &store.Finding{
		ID: "finding-1", Namespace: "ns", RepositoryScan: "repo", ScanRunID: "scan-1",
	}
	authorized := &securityFindingAuthorization{
		finding: finding,
		scan:    &corev1alpha1.RepositoryScan{ObjectMeta: metav1.ObjectMeta{Name: "repo", Namespace: "ns"}},
		run:     &store.ScanRun{ID: "scan-1", HeadCommit: head},
	}
	proposal := &store.PatchProposal{
		ID: "proposal-1", Namespace: "ns", RepositoryScan: "repo", FindingID: "finding-1",
		SourceScanRunID: "scan-1", SourceHeadSHA: head,
	}
	if !patchProposalMatchesAuthorizedFinding(proposal, authorized) {
		t.Fatal("legacy current-run proposal was rejected")
	}
	proposal.OccurrenceID = "unbound-occurrence"
	if patchProposalMatchesAuthorizedFinding(proposal, authorized) {
		t.Fatal("legacy finding accepted proposal with an unrelated occurrence binding")
	}
}

func TestThreatModelBoundToRepositoryScanRequiresCurrentGeneration(t *testing.T) {
	scan := &corev1alpha1.RepositoryScan{ObjectMeta: metav1.ObjectMeta{
		Name: "repo", UID: types.UID("scan-uid"), Generation: 2,
	}}
	model := &store.ThreatModel{
		RepositoryScan: "repo", RepositoryScanUID: "scan-uid", RepositoryScanGeneration: 1,
	}
	if threatModelBoundToRepositoryScan(model, scan) {
		t.Fatal("stale threat model generation was accepted")
	}
	model.RepositoryScanGeneration = 2
	if !threatModelBoundToRepositoryScan(model, scan) {
		t.Fatal("current threat model binding was rejected")
	}
}

func TestValidationAssessmentBlocksManualRequest(t *testing.T) {
	if validationAssessmentBlocksManualRequest(store.FindingAssessment{Method: "policy", Outcome: "deferred"}) {
		t.Fatal("policy deferral blocked explicit validation")
	}
	if !validationAssessmentBlocksManualRequest(store.FindingAssessment{Method: "validator-agent", Outcome: "validated"}) {
		t.Fatal("validator assessment did not block duplicate validation")
	}
}

func TestCreateSecurityScanRunRevalidatesIntegrityPolicy(t *testing.T) {
	scan := securityAuthzTestRepositoryScan("scan-policy", securityTestRepoURL)
	scan.Spec.CompletionPolicy = "validated"
	_, handlers := setupSecurityHandlersWithAuthzFixture(
		t, ContextTokenConfig{}, ContextTokenAuthorizationModeOff, scan,
	)
	_, err := handlers.createSecurityScanRun(context.Background(), nil, scan, "")
	if err == nil || !strings.Contains(err.Error(), "strict completion") {
		t.Fatalf("createSecurityScanRun() error = %v, want integrity policy rejection", err)
	}
}
