package sqlite

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	pv "github.com/orka-agents/orka/internal/patchverification"
)

func reportValidationFixture(test *testing.T) (*Store, pv.Manifest, pv.Binding, []pv.ExecutionEvidence) {
	test.Helper()
	storage := setupTestStore(test)
	manifest, _, provenance, evidence := patchVerificationFixture(test)
	delete(provenance, manifest.Sources.Patched.ArchiveDigest)
	delete(provenance, manifest.Sources.DiffDigest)
	manifest.Action = pv.ValidateReport
	manifest.ReportDigest, _ = pv.StableReportDigest(manifest.Problem, manifest.Scope)
	manifest.Sources.Patched, manifest.Sources.DiffDigest = pv.SourceIdentity{}, ""
	binding, err := pv.NewRunBinding(manifest, "report-attempt", "report-original", "")
	if err != nil {
		test.Fatal(err)
	}
	evidence = evidence[:len(manifest.Checks)]
	for index := range evidence {
		observation := &evidence[index].Observation
		observation.RunID, observation.AttemptID = binding.RunID, binding.AttemptID
		observation.TaskID, observation.ManifestDigest = binding.OriginalTaskID, binding.ManifestDigest
	}
	if err := storage.InitializePatchVerificationStore(test.Context()); err != nil {
		test.Fatal(err)
	}
	if err := storage.CreatePatchVerificationRun(test.Context(), manifest, binding, provenance); err != nil {
		test.Fatal(err)
	}
	return storage, manifest, binding, evidence
}

func TestPatchVerificationLegacyByteContract(test *testing.T) {
	storage, manifest, binding, evidence := createPatchVerificationFixture(test)
	recordPatchVerificationFixture(test, storage, binding, evidence)
	record, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
	if err != nil {
		test.Fatal(err)
	}
	legacyManifest := struct {
		Version     int             `json:"version"`
		Problem     string          `json:"problem"`
		Scope       []string        `json:"scope"`
		Gaps        []string        `json:"gaps"`
		Sources     pv.Sources      `json:"sources"`
		Environment pv.Environment  `json:"environment"`
		Files       []pv.FrozenFile `json:"files"`
		Checks      []pv.Check      `json:"checks"`
	}{manifest.Version, manifest.Problem, manifest.Scope, manifest.Gaps, manifest.Sources,
		manifest.Environment, manifest.Files, manifest.Checks}
	oldManifestJSON, err := json.Marshal(legacyManifest)
	if err != nil || pv.Digest(oldManifestJSON) != binding.ManifestDigest {
		test.Fatalf("legacy manifest bytes changed: %v", err)
	}
	creation, err := json.Marshal(struct {
		Manifest   any                `json:"manifest"`
		Binding    pv.Binding         `json:"binding"`
		Provenance []pv.BlobReference `json:"provenance"`
	}{legacyManifest, binding, record.Provenance})
	if err != nil {
		test.Fatal(err)
	}
	type legacyCheck struct {
		CheckID string `json:"checkID"`
		Outcome string `json:"outcome"`
		Reason  string `json:"reason,omitempty"`
	}
	type legacyAssessment struct {
		Conclusion pv.Conclusion `json:"conclusion"`
		Reason     string        `json:"reason"`
		Checks     []legacyCheck `json:"checks"`
	}
	checks := make([]legacyCheck, 0, len(manifest.Checks))
	for _, check := range manifest.Checks {
		checks = append(checks, legacyCheck{CheckID: check.ID, Outcome: "passed"})
	}
	observations, err := json.Marshal(record.Evidence)
	if err != nil {
		test.Fatal(err)
	}
	seal, err := json.Marshal(struct {
		CreationDigest string           `json:"creationDigest"`
		EvidenceDigest string           `json:"evidenceDigest"`
		State          pv.RunState      `json:"state"`
		Assessment     legacyAssessment `json:"assessment"`
	}{pv.Digest(creation), pv.Digest(observations), pv.RunFinalized,
		legacyAssessment{pv.Verified,
			"all stated problems reproduced on the original; patched checks and required normal behavior passed", checks}})
	if err != nil || !bytes.Equal(seal, record.Seal.Content) || pv.Digest(seal) != record.Seal.Digest {
		test.Fatalf("legacy seal bytes changed: %v", err)
	}
}

func TestReportStoreLifecycle(test *testing.T) {
	storage, manifest, binding, evidence := reportValidationFixture(test)
	initial, err := storage.GetPatchVerificationRun(test.Context(), binding.RunID)
	if err != nil || initial.Assessment.Conclusion != pv.UnableToValidate || len(initial.Provenance) != 1 {
		test.Fatalf("report creation: %+v, %v", initial, err)
	}
	for range 2 {
		recordPatchVerificationFixture(test, storage, binding, evidence)
	}
	record, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
	if err != nil || record.Assessment.Conclusion != pv.Reproduced || len(record.Assessment.Checks) != len(manifest.Checks) {
		test.Fatalf("report finalization: %+v, %v", record, err)
	}
	closed, err := storage.CancelPatchVerificationRun(test.Context(), binding)
	if err != nil || !reflect.DeepEqual(record, closed) {
		test.Fatalf("cancelling a completed report changed it: %v", err)
	}
	for _, side := range []string{pv.Original, pv.Patched} {
		foreign := evidence[0]
		foreign.Observation.Side = side
		foreign.Observation.AttemptID = "foreign-attempt"
		if err := storage.RecordPatchVerificationEvidence(test.Context(), binding, foreign); !errors.Is(err, pv.ErrBinding) {
			test.Fatalf("foreign report evidence accepted: %v", err)
		}
	}
	wrongSide := evidence[0]
	wrongSide.Observation.Side, wrongSide.Observation.TaskID = pv.Patched, ""
	if err := storage.RecordPatchVerificationEvidence(test.Context(), binding, wrongSide); !errors.Is(err, pv.ErrBinding) {
		test.Fatalf("report accepted a patched slot: %v", err)
	}
	conflict := evidence[0]
	conflict.Observation.ContainerID = "conflicting-container"
	if err := storage.RecordPatchVerificationEvidence(test.Context(), binding, conflict); !errors.Is(err, pv.ErrConflict) {
		test.Fatalf("conflicting report delivery accepted: %v", err)
	}
	after, err := storage.GetPatchVerificationRun(test.Context(), binding.RunID)
	if err != nil || after.Assessment.Conclusion != pv.UnableToValidate ||
		!reflect.DeepEqual(after.Seal, record.Seal) || len(after.Evidence) != len(evidence) {
		test.Fatalf("report conflict did not preserve seal and evidence: %+v, %v", after, err)
	}
}

func TestReportStoreCancellationFirstTerminalWins(test *testing.T) {
	storage, manifest, binding, evidence := reportValidationFixture(test)
	recordPatchVerificationFixture(test, storage, binding, evidence[:1])
	stopped, err := storage.CancelPatchVerificationRun(test.Context(), binding)
	if err != nil || stopped.State != pv.RunCancelled || stopped.Assessment.Conclusion != pv.UnableToValidate {
		test.Fatalf("cancel: %+v, %v", stopped, err)
	}
	recovered, err := storage.RecoverInterruptedPatchVerificationRun(test.Context(), binding)
	if err != nil || !reflect.DeepEqual(stopped, recovered) {
		test.Fatalf("later interruption changed cancellation: %v", err)
	}
	final, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
	if err != nil || final.Assessment.Conclusion != pv.UnableToValidate ||
		len(final.Assessment.Checks) != len(manifest.Checks) || final.Assessment.Checks[0].Outcome != "reproduced" {
		test.Fatalf("cancelled report lost partial cases: %+v, %v", final, err)
	}
	if err := storage.RecordPatchVerificationEvidence(test.Context(), binding, evidence[1]); !errors.Is(err, pv.ErrClosed) {
		test.Fatalf("cancelled run accepted a new slot: %v", err)
	}
}

func TestLinkedStoreReferenceAndFreshEvidence(test *testing.T) {
	storage, _, reportBinding, evidence := reportValidationFixture(test)
	recordPatchVerificationFixture(test, storage, reportBinding, evidence)
	earlier, err := storage.FinalizePatchVerificationRun(test.Context(), reportBinding)
	if err != nil {
		test.Fatal(err)
	}
	manifest, _, provenance, _ := patchVerificationFixture(test)
	manifest.Action, manifest.ReportDigest = pv.VerifyPatch, earlier.Manifest.ReportDigest
	manifest.DeclaredChanges = []pv.DeclaredChange{{Kind: "source", Description: "reject unsafe inputs"}}
	manifest.EarlierValidation = &pv.EarlierValidationReference{
		RunID: reportBinding.RunID, ReportDigest: earlier.Manifest.ReportDigest,
		ManifestDigest: reportBinding.ManifestDigest, SealDigest: earlier.Seal.Digest,
	}
	for _, scenario := range []string{"seal", "manifest", "base", "environment", "checks", "missing", "valid"} {
		test.Run(scenario, func(test *testing.T) {
			changed := manifest
			reference := *manifest.EarlierValidation
			changed.EarlierValidation = &reference
			switch scenario {
			case "seal":
				reference.SealDigest = pv.Digest([]byte("foreign seal"))
			case "manifest":
				reference.ManifestDigest = pv.Digest([]byte("foreign manifest"))
			case "base":
				changed.Sources.Original.Commit = changed.Sources.Patched.Commit
			case "environment":
				changed.Environment.ImageID = pv.Digest([]byte("different image"))
			case "checks":
				changed.Checks = changed.Checks[1:]
			case "missing":
				reference.RunID = "unresolvable-private-record"
			}
			binding, err := pv.NewRunBinding(changed, "linked-"+scenario, "new-original", "new-patched")
			if err != nil {
				test.Fatal(err)
			}
			err = storage.CreatePatchVerificationRun(test.Context(), changed, binding, provenance)
			if scenario != "valid" {
				if err == nil {
					test.Fatal("mismatched earlier validation was accepted")
				}
				return
			}
			if err != nil {
				test.Fatal(err)
			}
			fresh, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
			if err != nil || fresh.Assessment.Conclusion != pv.UnableToVerify || len(fresh.Evidence) != 0 ||
				len(fresh.Assessment.Checks) != len(manifest.Checks) {
				test.Fatalf("earlier observations were reused: %+v, %v", fresh, err)
			}
		})
	}
	after, err := storage.GetCompletedReportValidation(test.Context(), reportBinding.RunID)
	if err != nil || !reflect.DeepEqual(earlier, after) {
		test.Fatalf("link changed earlier report: %v", err)
	}
	if _, err := storage.db.ExecContext(test.Context(),
		`UPDATE patch_verification_blobs SET data = ? WHERE run_id = ? AND digest = ?`,
		[]byte("tampered"), reportBinding.RunID, earlier.Manifest.Sources.Original.ArchiveDigest); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.GetCompletedReportValidation(test.Context(), reportBinding.RunID); !errors.Is(err, pv.ErrIntegrity) {
		test.Fatalf("corrupt earlier evidence accepted: %v", err)
	}
	var state string
	var incidents int
	var seal []byte
	if err := storage.db.QueryRowContext(test.Context(),
		`SELECT state, seal_content, (SELECT COUNT(*) FROM patch_verification_incidents WHERE run_id = ?) FROM patch_verification_runs WHERE run_id = ?`,
		reportBinding.RunID, reportBinding.RunID).Scan(&state, &seal, &incidents); err != nil {
		test.Fatal(err)
	}
	if state != string(pv.RunFinalized) || incidents != 0 || !bytes.Equal(seal, earlier.Seal.Content) {
		test.Fatal("read-only link lookup modified earlier evidence")
	}
}
