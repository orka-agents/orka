package sqlite

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	pv "github.com/orka-agents/orka/internal/patchverification"
)

func TestReportStoreDisputedCaseResults(test *testing.T) {
	for _, scenario := range []string{"conflict", "conflict-after-seal", "missing-output"} {
		test.Run(scenario, func(test *testing.T) {
			storage, manifest, binding, evidence := reportValidationFixture(test)
			recordPatchVerificationFixture(test, storage, binding, evidence[1:])
			var historical *pv.Seal
			if scenario != "missing-output" {
				recordPatchVerificationFixture(test, storage, binding, evidence[:1])
				if scenario == "conflict-after-seal" {
					completed, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
					if err != nil {
						test.Fatal(err)
					}
					historical = completed.Seal
				}
			}
			disputed := evidence[0]
			wantError := pv.ErrConflict
			if scenario == "missing-output" {
				disputed.Observation.StderrDigest = pv.Digest([]byte("missing output"))
				disputed.Observation.StderrBytes = len("missing output")
				wantError = pv.ErrEvidence
			} else {
				disputed.Observation.ContainerID = "conflicting-execution"
			}
			if err := storage.RecordPatchVerificationEvidence(test.Context(), binding, disputed); !errors.Is(err, wantError) {
				test.Fatalf("disputed evidence result: %v", err)
			}
			final, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
			if err != nil || final.Assessment.Conclusion != pv.UnableToValidate {
				test.Fatalf("invalid report finalization: %v", err)
			}
			for _, result := range final.Assessment.Checks {
				want := pv.OutcomeReproduced
				switch result.CheckID {
				case manifest.Checks[0].ID:
					want = pv.OutcomeUntested
				case manifest.Checks[2].ID:
					want = pv.OutcomePassed
				}
				if result.Outcome != want {
					test.Errorf("case %s = %s, want %s", result.CheckID, result.Outcome, want)
				}
			}
			if historical != nil && !reflect.DeepEqual(historical, final.Seal) {
				test.Fatal("current invalidation rewrote the historical seal")
			}
			reloaded, err := storage.GetPatchVerificationRun(test.Context(), binding.RunID)
			if err != nil || !reflect.DeepEqual(final, reloaded) {
				test.Fatalf("disputed case assessment changed after reload: %v", err)
			}
			later := evidence[1]
			later.Observation.ContainerID = "later-conflicting-execution"
			if err := storage.RecordPatchVerificationEvidence(test.Context(), binding, later); !errors.Is(err, pv.ErrConflict) {
				test.Fatal(err)
			}
			after, err := storage.GetPatchVerificationRun(test.Context(), binding.RunID)
			if err != nil || !reflect.DeepEqual(final.Seal, after.Seal) {
				test.Fatalf("later incident changed sealed snapshot: %v", err)
			}
			for _, result := range after.Assessment.Checks {
				if result.CheckID != manifest.Checks[2].ID && result.Outcome != pv.OutcomeUntested {
					test.Fatal("later conflict retained a conclusive disputed case")
				}
			}
		})
	}
}

func TestReportStorePreviousRequirementReasonsRemainReadable(test *testing.T) {
	for _, state := range []pv.RunState{pv.RunFinalized, pv.RunCancelled, pv.RunInterrupted, pv.RunInvalid} {
		test.Run(string(state), func(test *testing.T) {
			storage, previous, seal := previousRequirementReport(test, state)
			restored, err := storage.GetPatchVerificationRun(test.Context(), previous.Binding.RunID)
			if err != nil {
				test.Fatalf("previous report seal became unreadable: %v", err)
			}
			if !reflect.DeepEqual(restored.Seal, seal) || restored.State != state ||
				!reflect.DeepEqual(restored.Incidents, previous.Incidents) ||
				restored.Assessment.Conclusion != pv.UnableToValidate {
				test.Fatal("reading a previous report changed its seal, state, or incidents")
			}
			if state == pv.RunFinalized && !reflect.DeepEqual(restored.Assessment, previous.Assessment) {
				test.Fatal("reading a previous finalized report changed its result")
			}
		})
	}
}

func previousRequirementReport(test *testing.T, state pv.RunState) (*Store, *pv.Record, *pv.Seal) {
	test.Helper()
	storage, _, _, _ := reportValidationFixture(test)
	manifest, _, provenance, _ := patchVerificationFixture(test)
	manifest.Action = pv.ValidateReport
	manifest.ReportDigest, _ = pv.StableReportDigest(manifest.Problem, manifest.Scope)
	delete(provenance, manifest.Sources.Patched.ArchiveDigest)
	delete(provenance, manifest.Sources.DiffDigest)
	manifest.Sources.Patched, manifest.Sources.DiffDigest = pv.SourceIdentity{}, ""
	manifest.Environment.Requirements = []pv.EnvironmentRequirement{{Kind: "cluster", Name: "prior-cluster"}}
	binding, err := pv.NewRunBinding(manifest, "prior-report", "prior-original", "")
	if err != nil {
		test.Fatal(err)
	}
	if err := storage.CreatePatchVerificationRun(test.Context(), manifest, binding, provenance); err != nil {
		test.Fatal(err)
	}
	switch state {
	case pv.RunCancelled:
		_, err = storage.CancelPatchVerificationRun(test.Context(), binding)
	case pv.RunInterrupted:
		_, err = storage.RecoverInterruptedPatchVerificationRun(test.Context(), binding)
	case pv.RunInvalid:
		conflicting := manifest
		conflicting.Problem = "conflicting report delivery"
		err = storage.CreatePatchVerificationRun(test.Context(), conflicting, binding, provenance)
		if !errors.Is(err, pv.ErrConflict) {
			test.Fatalf("fixture did not produce a conflicting delivery: %v", err)
		}
		err = nil
	}
	if err != nil {
		test.Fatal(err)
	}
	previous, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
	if err != nil {
		test.Fatal(err)
	}
	for index := range previous.Assessment.Checks {
		previous.Assessment.Checks[index].Reason = strings.Join(pv.MissingRequirements(manifest.Environment), "; ")
	}
	seal, err := makePatchVerificationSealSnapshot(previous, nil)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := storage.db.ExecContext(test.Context(),
		`UPDATE patch_verification_runs SET seal_content = ?, seal_digest = ? WHERE run_id = ?`,
		seal.Content, seal.Digest, binding.RunID); err != nil {
		test.Fatal(err)
	}
	return storage, previous, seal
}

func TestReportStoreMaximumUnsupportedRequirements(test *testing.T) {
	for _, action := range []pv.Action{pv.ValidateReport, pv.VerifyPatch} {
		test.Run(string(action), func(test *testing.T) {
			storage := setupTestStore(test)
			manifest, _, provenance, _ := patchVerificationFixture(test)
			manifest.Action = action
			manifest.ReportDigest, _ = pv.StableReportDigest(manifest.Problem, manifest.Scope)
			patchedTask := "patched-task"
			if action == pv.ValidateReport {
				delete(provenance, manifest.Sources.Patched.ArchiveDigest)
				delete(provenance, manifest.Sources.DiffDigest)
				manifest.Sources.Patched, manifest.Sources.DiffDigest = pv.SourceIdentity{}, ""
				patchedTask = ""
			} else {
				manifest.DeclaredChanges = []pv.DeclaredChange{{Kind: "source", Description: "supplied change"}}
			}
			prototype := manifest.Checks[0]
			manifest.Checks = make([]pv.Check, 100)
			for index := range manifest.Checks {
				manifest.Checks[index] = prototype
				manifest.Checks[index].ID = fmt.Sprintf("check-%03d-%s", index, strings.Repeat("a", 117))
			}
			manifest.Checks[0].Kind = pv.Normal
			for index := range 32 {
				manifest.Environment.Requirements = append(manifest.Environment.Requirements,
					pv.EnvironmentRequirement{Kind: "cluster", Name: fmt.Sprintf("cluster-%02d-%s", index, strings.Repeat("b", 117))})
			}
			binding, err := pv.NewRunBinding(manifest, "unsupported-attempt", "original-task", patchedTask)
			if err != nil {
				test.Fatal(err)
			}
			if err := storage.InitializePatchVerificationStore(test.Context()); err != nil {
				test.Fatal(err)
			}
			if err := storage.CreatePatchVerificationRun(test.Context(), manifest, binding, provenance); err != nil {
				test.Fatal(err)
			}
			final, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
			if err != nil {
				test.Fatalf("accepted unsupported request could not finalize: %v", err)
			}
			if final.State != pv.RunFinalized || final.Seal == nil || final.Assessment.Conclusion != pv.UnavailableAction(action) ||
				len(final.Assessment.Checks) != 100 || len(final.Evidence) != 0 {
				test.Fatal("unsupported request did not produce a complete unavailable record")
			}
			for _, requirement := range manifest.Environment.Requirements {
				if !strings.Contains(final.Assessment.Reason, requirement.Name) {
					test.Fatal("missing requirement was not named in the overall result")
				}
			}
			if _, err := storage.GetPatchVerificationRun(test.Context(), binding.RunID); err != nil {
				test.Fatal(err)
			}
		})
	}
}

func TestActionStoreDisputedPatchResults(test *testing.T) {
	for _, side := range []string{pv.Original, pv.Patched} {
		test.Run(side, func(test *testing.T) {
			storage := setupTestStore(test)
			manifest, _, provenance, evidence := patchVerificationFixture(test)
			manifest.Action = pv.VerifyPatch
			manifest.ReportDigest, _ = pv.StableReportDigest(manifest.Problem, manifest.Scope)
			manifest.DeclaredChanges = []pv.DeclaredChange{{Kind: "source", Description: "supplied fix"}}
			binding, err := pv.NewRunBinding(manifest, "comparison-attempt", "original-task", "patched-task")
			if err != nil {
				test.Fatal(err)
			}
			for index := range evidence {
				observation := &evidence[index].Observation
				observation.RunID, observation.AttemptID = binding.RunID, binding.AttemptID
				observation.ManifestDigest, observation.TaskID = binding.ManifestDigest, binding.OriginalTaskID
				if observation.Side == pv.Patched {
					observation.TaskID = binding.PatchedTaskID
				}
			}
			if err := storage.InitializePatchVerificationStore(test.Context()); err != nil {
				test.Fatal(err)
			}
			if err := storage.CreatePatchVerificationRun(test.Context(), manifest, binding, provenance); err != nil {
				test.Fatal(err)
			}
			recordPatchVerificationFixture(test, storage, binding, evidence)
			completed, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
			if err != nil || completed.Assessment.Conclusion != pv.Verified {
				test.Fatalf("fixture verification failed: %v", err)
			}
			position := 0
			if side == pv.Patched {
				position = len(manifest.Checks)
			}
			conflict := evidence[position]
			conflict.Observation.ContainerID = "other-comparison-execution"
			if err := storage.RecordPatchVerificationEvidence(test.Context(), binding, conflict); !errors.Is(err, pv.ErrConflict) {
				test.Fatal(err)
			}
			current, err := storage.GetPatchVerificationRun(test.Context(), binding.RunID)
			if err != nil || current.Assessment.Conclusion != pv.UnableToVerify ||
				!reflect.DeepEqual(current.Seal, completed.Seal) {
				test.Fatalf("comparison invalidation changed its seal: %v", err)
			}
			for _, result := range current.Assessment.Checks {
				want := pv.OutcomePassed
				if result.CheckID == conflict.Observation.CheckID {
					want = pv.OutcomeUntested
				}
				if result.Outcome != want {
					test.Errorf("comparison case %s = %s, want %s", result.CheckID, result.Outcome, want)
				}
			}
		})
	}
}
