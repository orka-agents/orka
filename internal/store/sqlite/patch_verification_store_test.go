package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	verification "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/store"
)

func patchVerificationFixture(test *testing.T) (verification.Manifest, verification.Binding, map[string][]byte, []verification.ExecutionEvidence) {
	test.Helper()
	manifest := verification.Manifest{
		Version: verification.SchemaVersion, Problem: "unsafe input is accepted", Scope: []string{"two unsafe inputs and normal input"},
		Sources: verification.Sources{
			Repository: "/local/project",
			Original:   verification.SourceIdentity{Commit: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40), ArchiveDigest: verification.Digest([]byte("original"))},
			Patched:    verification.SourceIdentity{Commit: strings.Repeat("c", 40), Tree: strings.Repeat("d", 40), ArchiveDigest: verification.Digest([]byte("patched"))},
			DiffDigest: verification.Digest([]byte("diff")),
		},
		Environment: verification.Environment{Image: "example/tool@" + verification.Digest([]byte("image")), ImageID: verification.Digest([]byte("image-config")), Platform: "linux/amd64", Profile: verification.Offline},
		Checks: []verification.Check{
			{ID: "case-one", Kind: verification.Reproduction, Command: []string{"/checks/run", "one"}, Healthy: verification.Expectation{Stdout: "rejected\n"}, Failure: verification.Expectation{Stdout: "accepted\n"}, TimeoutSeconds: 10},
			{ID: "case-two", Kind: verification.Reproduction, Command: []string{"/checks/run", "two"}, Healthy: verification.Expectation{Stdout: "rejected\n"}, Failure: verification.Expectation{Stdout: "accepted\n"}, TimeoutSeconds: 10},
			{ID: "normal", Kind: verification.Normal, Command: []string{"/checks/run", "normal"}, Healthy: verification.Expectation{Stdout: "accepted\n"}, Failure: verification.Expectation{Stdout: "rejected\n"}, TimeoutSeconds: 10},
		},
	}
	binding, err := verification.NewRunBinding(manifest, "attempt-one", "task-original-uid", "task-patched-uid")
	if err != nil {
		test.Fatal(err)
	}
	provenance := make(map[string][]byte)
	for _, content := range []string{"original", "patched", "diff"} {
		provenance[verification.Digest([]byte(content))] = []byte(content)
	}
	evidence := make([]verification.ExecutionEvidence, 0, 2*len(manifest.Checks))
	for _, side := range []string{verification.Original, verification.Patched} {
		for _, check := range manifest.Checks {
			expected := check.Healthy
			taskID, tree := binding.PatchedTaskID, manifest.Sources.Patched.Tree
			if side == verification.Original {
				taskID, tree = binding.OriginalTaskID, manifest.Sources.Original.Tree
				if check.Kind == verification.Reproduction {
					expected = check.Failure
				}
			}
			started := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
			observation := verification.Observation{
				RunID: binding.RunID, AttemptID: binding.AttemptID, TaskID: taskID, ManifestDigest: binding.ManifestDigest,
				Side: side, CheckID: check.ID, SourceTree: tree, ImageID: manifest.Environment.ImageID,
				ContainerID: "container-" + side + "-" + check.ID, Origin: "runner", StartedAt: started, FinishedAt: started.Add(time.Second),
				Executed: true, ExitCode: new(expected.ExitCode), StdoutDigest: verification.Digest([]byte(expected.Stdout)), StdoutBytes: len(expected.Stdout), StderrDigest: verification.Digest(nil),
			}
			evidence = append(evidence, verification.ExecutionEvidence{Observation: observation, Blobs: map[string][]byte{observation.StdoutDigest: []byte(expected.Stdout), observation.StderrDigest: {}}})
		}
	}
	return manifest, binding, provenance, evidence
}

func TestPatchVerificationCreateReadCleanup(test *testing.T) {
	storage := setupTestStore(test)
	ctx := context.Background()
	manifest, binding, provenance, _ := patchVerificationFixture(test)
	for range 2 {
		if err := storage.InitializePatchVerificationStore(ctx); err != nil {
			test.Fatal(err)
		}
		if err := storage.CreatePatchVerificationRun(ctx, manifest, binding, provenance); err != nil {
			test.Fatal(err)
		}
	}
	for _, taskID := range []string{binding.OriginalTaskID, binding.PatchedTaskID} {
		if err := storage.SaveResult(ctx, "default", taskID, []byte("mutable result")); err != nil {
			test.Fatal(err)
		}
		if err := storage.SaveArtifact(ctx, store.SaveArtifactRequest{Owner: store.ArtifactOwner{Namespace: "default", TaskName: taskID},
			Filename: "output", ContentType: "text/plain", Data: []byte("mutable output")}); err != nil {
			test.Fatal(err)
		}
		if err := storage.DeleteResult(ctx, "default", taskID); err != nil {
			test.Fatal(err)
		}
		if err := storage.DeleteArtifacts(ctx, store.ArtifactOwner{Namespace: "default", TaskName: taskID}); err != nil {
			test.Fatal(err)
		}
	}
	record, err := storage.GetPatchVerificationRun(ctx, binding.RunID)
	if err != nil {
		test.Fatal(err)
	}
	if !reflect.DeepEqual(record.Manifest, manifest) || record.Binding != binding || record.State != verification.RunRunning || record.Assessment.Conclusion != verification.UnableToVerify {
		test.Fatalf("unexpected record: %+v", record)
	}
	for digest, expected := range provenance {
		content, err := storage.GetPatchVerificationBlob(ctx, binding.RunID, digest)
		if err != nil || !bytes.Equal(content, expected) {
			test.Fatalf("blob mismatch: %v", err)
		}
	}
	duplicate, err := verification.NewRunBinding(manifest, binding.AttemptID, binding.OriginalTaskID, binding.PatchedTaskID)
	if err != nil || duplicate != binding {
		test.Fatalf("run identity is not deterministic: %v", err)
	}
}

func TestPatchVerificationCreateRejectsMissingBytes(test *testing.T) {
	storage := setupTestStore(test)
	ctx := context.Background()
	if err := storage.InitializePatchVerificationStore(ctx); err != nil {
		test.Fatal(err)
	}
	manifest, binding, provenance, _ := patchVerificationFixture(test)
	delete(provenance, manifest.Sources.DiffDigest)
	if err := storage.CreatePatchVerificationRun(ctx, manifest, binding, provenance); !errors.Is(err, verification.ErrIntegrity) {
		test.Fatalf("missing provenance accepted: %v", err)
	}
	if _, err := storage.GetPatchVerificationRun(ctx, binding.RunID); !errors.Is(err, verification.ErrRunNotFound) {
		test.Fatalf("failed creation left a run: %v", err)
	}
}

func createPatchVerificationFixture(test *testing.T) (*Store, verification.Manifest, verification.Binding, []verification.ExecutionEvidence) {
	test.Helper()
	storage := setupTestStore(test)
	manifest, binding, provenance, evidence := patchVerificationFixture(test)
	if err := storage.InitializePatchVerificationStore(test.Context()); err != nil {
		test.Fatal(err)
	}
	if err := storage.CreatePatchVerificationRun(test.Context(), manifest, binding, provenance); err != nil {
		test.Fatal(err)
	}
	return storage, manifest, binding, evidence
}

func recordPatchVerificationFixture(test *testing.T, storage *Store, binding verification.Binding, evidence []verification.ExecutionEvidence) {
	test.Helper()
	for _, entry := range evidence {
		if err := storage.RecordPatchVerificationEvidence(test.Context(), binding, entry); err != nil {
			test.Fatal(err)
		}
	}
}

func replacePatchVerificationOutput(entry *verification.ExecutionEvidence, output string) {
	entry.Observation.StdoutDigest = verification.Digest([]byte(output))
	entry.Observation.StdoutBytes = len(output)
	entry.Blobs = map[string][]byte{entry.Observation.StdoutDigest: []byte(output), entry.Observation.StderrDigest: {}}
}

func TestPatchVerificationConclusions(test *testing.T) {
	cases := []struct {
		name   string
		want   verification.Conclusion
		mutate func([]verification.ExecutionEvidence) []verification.ExecutionEvidence
	}{
		{name: "verified", want: verification.Verified},
		{name: "not fixed", want: verification.NotFixed, mutate: func(evidence []verification.ExecutionEvidence) []verification.ExecutionEvidence {
			replacePatchVerificationOutput(&evidence[3], "accepted\n")
			replacePatchVerificationOutput(&evidence[4], "accepted\n")
			return evidence
		}},
		{name: "partially fixed", want: verification.PartiallyFixed, mutate: func(evidence []verification.ExecutionEvidence) []verification.ExecutionEvidence {
			replacePatchVerificationOutput(&evidence[3], "accepted\n")
			return evidence
		}},
		{name: "regression", want: verification.Regression, mutate: func(evidence []verification.ExecutionEvidence) []verification.ExecutionEvidence {
			replacePatchVerificationOutput(&evidence[5], "rejected\n")
			return evidence
		}},
		{name: "missing check", want: verification.UnableToVerify, mutate: func(evidence []verification.ExecutionEvidence) []verification.ExecutionEvidence { return evidence[:5] }},
		{name: "no evidence", want: verification.UnableToVerify, mutate: func([]verification.ExecutionEvidence) []verification.ExecutionEvidence { return nil }},
		{name: "timeout", want: verification.UnableToVerify, mutate: func(evidence []verification.ExecutionEvidence) []verification.ExecutionEvidence {
			evidence[3].Observation.TimedOut = true
			return evidence
		}},
	}
	for _, scenario := range cases {
		test.Run(scenario.name, func(test *testing.T) {
			storage, manifest, binding, evidence := createPatchVerificationFixture(test)
			if scenario.mutate != nil {
				evidence = scenario.mutate(evidence)
			}
			recordPatchVerificationFixture(test, storage, binding, evidence)
			record, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
			if err != nil || record.Assessment.Conclusion != scenario.want || record.Seal == nil || record.Seal.Digest != verification.Digest(record.Seal.Content) {
				test.Fatalf("finalization: record=%+v error=%v", record, err)
			}
			if !reflect.DeepEqual(record.Manifest, manifest) {
				test.Fatal("finalization changed manifest")
			}
			repeated, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
			if err != nil || !reflect.DeepEqual(repeated, record) {
				test.Fatalf("finalization is not idempotent: %v", err)
			}
		})
	}
}

func TestPatchVerificationDuplicateAndConflict(test *testing.T) {
	for _, sealed := range []bool{false, true} {
		name := "before seal"
		if sealed {
			name = "after seal"
		}
		test.Run(name, func(test *testing.T) {
			storage, _, binding, evidence := createPatchVerificationFixture(test)
			recordPatchVerificationFixture(test, storage, binding, evidence)
			var seal *verification.Seal
			if sealed {
				record, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
				if err != nil {
					test.Fatal(err)
				}
				seal = record.Seal
			}
			recordPatchVerificationFixture(test, storage, binding, evidence)
			conflict := evidence[3]
			conflict.Observation.ContainerID = "another-container"
			for range 2 {
				if err := storage.RecordPatchVerificationEvidence(test.Context(), binding, conflict); !errors.Is(err, verification.ErrConflict) {
					test.Fatalf("conflict accepted: %v", err)
				}
			}
			record, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
			if err != nil || record.Assessment.Conclusion != verification.UnableToVerify || len(record.Incidents) != 1 || len(record.Evidence) != len(evidence) {
				test.Fatalf("conflict was not durable: record=%+v error=%v", record, err)
			}
			if sealed && !reflect.DeepEqual(record.Seal, seal) {
				test.Fatal("conflict changed original seal")
			}
			if record.Evidence[3].Observation.ContainerID == "another-container" {
				test.Fatal("conflict replaced accepted observation")
			}
		})
	}
}

func TestPatchVerificationForeignEvidenceDoesNotInvalidate(test *testing.T) {
	mutations := map[string]func(*verification.Binding, *verification.Observation){
		"run":  func(_ *verification.Binding, observation *verification.Observation) { observation.RunID = "foreign" },
		"task": func(_ *verification.Binding, observation *verification.Observation) { observation.TaskID = "foreign" },
		"attempt": func(_ *verification.Binding, observation *verification.Observation) {
			observation.AttemptID = "foreign"
		},
		"manifest": func(_ *verification.Binding, observation *verification.Observation) {
			observation.ManifestDigest = verification.Digest([]byte("foreign"))
		},
		"caller": func(binding *verification.Binding, observation *verification.Observation) {
			binding.PatchedTaskID = "foreign"
			observation.TaskID = "foreign"
		},
	}
	for name, mutate := range mutations {
		test.Run(name, func(test *testing.T) {
			storage, _, binding, evidence := createPatchVerificationFixture(test)
			foreignBinding, foreign := binding, evidence[3]
			mutate(&foreignBinding, &foreign.Observation)
			if err := storage.RecordPatchVerificationEvidence(test.Context(), foreignBinding, foreign); !errors.Is(err, verification.ErrBinding) {
				test.Fatalf("foreign evidence accepted: %v", err)
			}
			recordPatchVerificationFixture(test, storage, binding, evidence)
			record, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
			if err != nil || record.Assessment.Conclusion != verification.Verified || len(record.Incidents) != 0 {
				test.Fatalf("foreign evidence invalidated legitimate run: %v", err)
			}
		})
	}
}

func TestPatchVerificationStoredBindingCorruption(test *testing.T) {
	corruptions := map[string]func(*verification.Binding){
		"task identity": func(binding *verification.Binding) { binding.PatchedTaskID = "corrupt" },
		"run identity":  func(binding *verification.Binding) { binding.RunID = "corrupt" },
		"invalid JSON":  nil,
		"unknown field": nil,
	}
	for _, sealed := range []bool{false, true} {
		for corruption, mutate := range corruptions {
			for _, operation := range []string{"read", "blob", "finalize", "cancel", "recover", "evidence", "create"} {
				test.Run(fmt.Sprintf("sealed=%t/%s/%s", sealed, corruption, operation), func(test *testing.T) {
					storage, manifest, binding, evidence := createPatchVerificationFixture(test)
					_, _, provenance, _ := patchVerificationFixture(test)
					recordPatchVerificationFixture(test, storage, binding, evidence)
					var sealContent []byte
					var sealDigest string
					if sealed {
						record, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
						if err != nil || record.Assessment.Conclusion != verification.Verified {
							test.Fatalf("finalize fixture: %v", err)
						}
						sealContent, sealDigest = record.Seal.Content, record.Seal.Digest
					}
					corruptContent := []byte("{")
					if mutate != nil {
						corruptBinding := binding
						mutate(&corruptBinding)
						var err error
						corruptContent, err = json.Marshal(corruptBinding)
						if err != nil {
							test.Fatal(err)
						}
					} else if corruption == "unknown field" {
						var err error
						corruptContent, err = json.Marshal(struct {
							verification.Binding
							Unexpected bool `json:"unexpected"`
						}{Binding: binding, Unexpected: true})
						if err != nil {
							test.Fatal(err)
						}
					}
					if _, err := storage.db.ExecContext(test.Context(), `UPDATE patch_verification_runs SET binding = ? WHERE run_id = ?`, corruptContent, binding.RunID); err != nil {
						test.Fatal(err)
					}
					wantState := verification.RunInvalid
					if operation == "cancel" {
						wantState = verification.RunCancelled
					} else if operation == "recover" && !sealed {
						wantState = verification.RunInterrupted
					}
					operations := map[string]func() error{
						"read": func() error {
							_, err := storage.GetPatchVerificationRun(test.Context(), binding.RunID)
							return err
						},
						"blob": func() error {
							_, err := storage.GetPatchVerificationBlob(test.Context(), binding.RunID, manifest.Sources.DiffDigest)
							return err
						},
						"finalize": func() error {
							_, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
							return err
						},
						"cancel": func() error {
							_, err := storage.CancelPatchVerificationRun(test.Context(), binding)
							return err
						},
						"recover": func() error {
							_, err := storage.RecoverInterruptedPatchVerificationRun(test.Context(), binding)
							return err
						},
						"evidence": func() error {
							return storage.RecordPatchVerificationEvidence(test.Context(), binding, evidence[0])
						},
						"create": func() error {
							return storage.CreatePatchVerificationRun(test.Context(), manifest, binding, provenance)
						},
					}
					for range 2 {
						if err := operations[operation](); !errors.Is(err, verification.ErrIntegrity) {
							test.Fatalf("stored binding corruption was not reported as integrity failure: %v", err)
						}
						var state verification.RunState
						var storedBinding, storedSeal []byte
						var storedSealDigest string
						if err := storage.db.QueryRowContext(test.Context(), `SELECT state, binding, seal_content, COALESCE(seal_digest, '') FROM patch_verification_runs WHERE run_id = ?`, binding.RunID).Scan(&state, &storedBinding, &storedSeal, &storedSealDigest); err != nil {
							test.Fatal(err)
						}
						var incidents int
						if err := storage.db.QueryRowContext(test.Context(), `SELECT COUNT(*) FROM patch_verification_incidents WHERE run_id = ? AND kind = 'integrity'`, binding.RunID).Scan(&incidents); err != nil {
							test.Fatal(err)
						}
						if state != wantState || incidents != 1 || !bytes.Equal(storedBinding, corruptContent) || !bytes.Equal(storedSeal, sealContent) || storedSealDigest != sealDigest {
							test.Fatalf("integrity failure was not durable or changed frozen data: state=%s incidents=%d", state, incidents)
						}
					}
					record, err := storage.GetPatchVerificationRun(test.Context(), binding.RunID)
					if !errors.Is(err, verification.ErrIntegrity) || record == nil || record.Assessment.Conclusion != verification.UnableToVerify || record.Binding.RunID != binding.RunID {
						test.Fatalf("corrupt binding did not fail closed: record=%+v error=%v", record, err)
					}
				})
			}
		}
	}
}

func TestPatchVerificationForeignBindingDoesNotInvalidate(test *testing.T) {
	for _, sealed := range []bool{false, true} {
		test.Run(fmt.Sprintf("sealed=%t", sealed), func(test *testing.T) {
			storage, manifest, binding, evidence := createPatchVerificationFixture(test)
			_, _, provenance, _ := patchVerificationFixture(test)
			recordPatchVerificationFixture(test, storage, binding, evidence)
			if sealed {
				if _, err := storage.FinalizePatchVerificationRun(test.Context(), binding); err != nil {
					test.Fatal(err)
				}
			}
			before, err := storage.GetPatchVerificationRun(test.Context(), binding.RunID)
			if err != nil {
				test.Fatal(err)
			}
			foreignBinding, foreignEvidence := binding, evidence[3]
			foreignBinding.PatchedTaskID = "foreign"
			foreignEvidence.Observation.TaskID = foreignBinding.PatchedTaskID
			operations := map[string]func() error{
				"create": func() error {
					return storage.CreatePatchVerificationRun(test.Context(), manifest, foreignBinding, provenance)
				},
				"evidence": func() error {
					return storage.RecordPatchVerificationEvidence(test.Context(), foreignBinding, foreignEvidence)
				},
				"finalize": func() error {
					_, err := storage.FinalizePatchVerificationRun(test.Context(), foreignBinding)
					return err
				},
				"cancel": func() error {
					_, err := storage.CancelPatchVerificationRun(test.Context(), foreignBinding)
					return err
				},
				"recover": func() error {
					_, err := storage.RecoverInterruptedPatchVerificationRun(test.Context(), foreignBinding)
					return err
				},
			}
			for name, operation := range operations {
				if err := operation(); !errors.Is(err, verification.ErrBinding) {
					test.Fatalf("%s accepted foreign binding: %v", name, err)
				}
				after, err := storage.GetPatchVerificationRun(test.Context(), binding.RunID)
				if err != nil || !reflect.DeepEqual(before, after) {
					test.Fatalf("%s with foreign binding changed good record: %v", name, err)
				}
			}
		})
	}
}

func TestPatchVerificationManifestCannotBeReplaced(test *testing.T) {
	for _, sealed := range []bool{false, true} {
		name := "before seal"
		if sealed {
			name = "after seal"
		}
		test.Run(name, func(test *testing.T) {
			storage, manifest, binding, evidence := createPatchVerificationFixture(test)
			_, _, provenance, _ := patchVerificationFixture(test)
			recordPatchVerificationFixture(test, storage, binding, evidence)
			var seal *verification.Seal
			if sealed {
				record, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
				if err != nil {
					test.Fatal(err)
				}
				seal = record.Seal
			}
			if err := storage.CreatePatchVerificationRun(test.Context(), manifest, binding, provenance); err != nil {
				test.Fatalf("identical creation replay failed: %v", err)
			}
			changed := manifest
			changed.Problem = "a different problem"
			for range 2 {
				if err := storage.CreatePatchVerificationRun(test.Context(), changed, binding, provenance); !errors.Is(err, verification.ErrConflict) {
					test.Fatalf("manifest replacement accepted: %v", err)
				}
			}
			record, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
			if err != nil || !reflect.DeepEqual(record.Manifest, manifest) || record.Assessment.Conclusion != verification.UnableToVerify || len(record.Incidents) != 1 {
				test.Fatalf("manifest conflict was not durable: %v", err)
			}
			if sealed && !reflect.DeepEqual(record.Seal, seal) {
				test.Fatal("manifest conflict changed original seal")
			}
		})
	}
}

func TestPatchVerificationInvalidAttemptsAreDurable(test *testing.T) {
	cases := map[string]func(*verification.ExecutionEvidence){
		"missing stdout bytes": func(evidence *verification.ExecutionEvidence) {
			delete(evidence.Blobs, evidence.Observation.StdoutDigest)
		},
		"missing stderr bytes": func(evidence *verification.ExecutionEvidence) {
			delete(evidence.Blobs, evidence.Observation.StderrDigest)
		},
		"wrong blob hash": func(evidence *verification.ExecutionEvidence) {
			evidence.Blobs[evidence.Observation.StdoutDigest] = []byte("corrupt\n")
		},
		"wrong byte count":      func(evidence *verification.ExecutionEvidence) { evidence.Observation.StdoutBytes++ },
		"missing stderr digest": func(evidence *verification.ExecutionEvidence) { evidence.Observation.StderrDigest = "" },
		"unknown check":         func(evidence *verification.ExecutionEvidence) { evidence.Observation.CheckID = "not-frozen" },
		"wrong source": func(evidence *verification.ExecutionEvidence) {
			evidence.Observation.SourceTree = strings.Repeat("e", 40)
		},
		"wrong image": func(evidence *verification.ExecutionEvidence) {
			evidence.Observation.ImageID = verification.Digest([]byte("foreign"))
		},
		"wrong origin": func(evidence *verification.ExecutionEvidence) { evidence.Observation.Origin = "project" },
		"oversized output": func(evidence *verification.ExecutionEvidence) {
			replacePatchVerificationOutput(evidence, strings.Repeat("x", verification.MaxOutputBytes+1))
		},
		"oversized metadata": func(evidence *verification.ExecutionEvidence) {
			evidence.Observation.SetupError = strings.Repeat("x", verification.MaxObservationBytes+1)
		},
		"credential output": func(evidence *verification.ExecutionEvidence) {
			replacePatchVerificationOutput(evidence, "Authorization: Bearer test-placeholder")
		},
		"credential metadata": func(evidence *verification.ExecutionEvidence) {
			evidence.Observation.SetupError = "Authorization: Bearer test-placeholder"
		},
	}
	for name, mutate := range cases {
		test.Run(name, func(test *testing.T) {
			storage, _, binding, evidence := createPatchVerificationFixture(test)
			invalid := evidence[0]
			mutate(&invalid)
			if err := storage.RecordPatchVerificationEvidence(test.Context(), binding, invalid); !errors.Is(err, verification.ErrEvidence) {
				test.Fatalf("invalid attempt accepted: %v", err)
			}
			record, err := storage.GetPatchVerificationRun(test.Context(), binding.RunID)
			if err != nil || record.State != verification.RunInvalid || len(record.Incidents) == 0 || record.Assessment.Conclusion != verification.UnableToVerify {
				test.Fatalf("invalid attempt was lost: record=%+v error=%v", record, err)
			}
			if err := storage.RecordPatchVerificationEvidence(test.Context(), binding, evidence[1]); !errors.Is(err, verification.ErrClosed) {
				test.Fatalf("invalid run resumed: %v", err)
			}
			record, err = storage.FinalizePatchVerificationRun(test.Context(), binding)
			if err != nil || record.Assessment.Conclusion != verification.UnableToVerify || record.Seal == nil {
				test.Fatalf("invalid run finalized successfully: %v", err)
			}
		})
	}
}

func TestPatchVerificationCorruptBlobsFailClosed(test *testing.T) {
	for _, sealed := range []bool{false, true} {
		for _, missing := range []bool{false, true} {
			name := "corrupt"
			if missing {
				name = "missing"
			}
			if sealed {
				name += " after seal"
			}
			test.Run(name, func(test *testing.T) {
				storage, _, binding, evidence := createPatchVerificationFixture(test)
				recordPatchVerificationFixture(test, storage, binding, evidence)
				var originalSeal *verification.Seal
				if sealed {
					record, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
					if err != nil {
						test.Fatal(err)
					}
					originalSeal = record.Seal
				}
				digest := evidence[0].Observation.StdoutDigest
				statement := `UPDATE patch_verification_blobs SET data = 'corrupt' WHERE run_id = ? AND digest = ?`
				if missing {
					statement = `DELETE FROM patch_verification_blobs WHERE run_id = ? AND digest = ?`
				}
				if _, err := storage.db.ExecContext(test.Context(), statement, binding.RunID, digest); err != nil {
					test.Fatal(err)
				}
				if _, err := storage.GetPatchVerificationBlob(test.Context(), binding.RunID, digest); !errors.Is(err, verification.ErrIntegrity) {
					test.Fatalf("invalid blob returned: %v", err)
				}
				for _, load := range []func() (*verification.Record, error){
					func() (*verification.Record, error) {
						return storage.GetPatchVerificationRun(test.Context(), binding.RunID)
					},
					func() (*verification.Record, error) {
						return storage.FinalizePatchVerificationRun(test.Context(), binding)
					},
				} {
					record, err := load()
					if !errors.Is(err, verification.ErrIntegrity) || record == nil || record.Assessment.Conclusion != verification.UnableToVerify {
						test.Fatalf("corruption did not fail closed: record=%+v error=%v", record, err)
					}
				}
				if _, err := storage.db.ExecContext(test.Context(), `INSERT INTO patch_verification_blobs (run_id, digest, data) VALUES (?, ?, ?) ON CONFLICT(run_id, digest) DO UPDATE SET data = excluded.data`, binding.RunID, digest, evidence[0].Blobs[digest]); err != nil {
					test.Fatal(err)
				}
				record, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
				if err != nil || record.Assessment.Conclusion != verification.UnableToVerify {
					test.Fatalf("repair resurrected a run: %v", err)
				}
				if sealed && !reflect.DeepEqual(record.Seal, originalSeal) {
					test.Fatal("corruption handling rewrote original seal")
				}
			})
		}
	}
}

func openPatchVerificationTestStore(test *testing.T, path string) *Store {
	test.Helper()
	database, err := NewDB(path)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = database.Close() })
	storage := NewStore(database, path)
	if err := storage.InitializePatchVerificationStore(test.Context()); err != nil {
		test.Fatal(err)
	}
	return storage
}

func TestPatchVerificationReopenAndRecovery(test *testing.T) {
	path := filepath.Join(test.TempDir(), "evidence.sqlite")
	storage := openPatchVerificationTestStore(test, path)
	manifest, binding, provenance, evidence := patchVerificationFixture(test)
	if err := storage.CreatePatchVerificationRun(test.Context(), manifest, binding, provenance); err != nil {
		test.Fatal(err)
	}
	recordPatchVerificationFixture(test, storage, binding, evidence)
	if err := storage.db.Close(); err != nil {
		test.Fatal(err)
	}
	reopened := openPatchVerificationTestStore(test, path)
	record, err := reopened.GetPatchVerificationRun(test.Context(), binding.RunID)
	if err != nil || record.State != verification.RunRunning || record.Assessment.Conclusion != verification.UnableToVerify || len(record.Evidence) != len(evidence) {
		test.Fatalf("unsealed evidence was lost or trusted: %v", err)
	}
	for range 2 {
		record, err = reopened.RecoverInterruptedPatchVerificationRun(test.Context(), binding)
		if err != nil || record.State != verification.RunInterrupted || record.Assessment.Conclusion != verification.UnableToVerify || len(record.Incidents) != 1 {
			test.Fatalf("recovery was not fail-closed or idempotent: %v", err)
		}
	}
	record, err = reopened.FinalizePatchVerificationRun(test.Context(), binding)
	if err != nil || record.Assessment.Conclusion != verification.UnableToVerify {
		test.Fatalf("interrupted run became successful: %v", err)
	}
	seal := record.Seal
	if err := reopened.db.Close(); err != nil {
		test.Fatal(err)
	}
	reopened = openPatchVerificationTestStore(test, path)
	record, err = reopened.FinalizePatchVerificationRun(test.Context(), binding)
	if err != nil || !reflect.DeepEqual(record.Seal, seal) || record.Assessment.Conclusion != verification.UnableToVerify {
		test.Fatalf("reopen changed interrupted seal: %v", err)
	}
}

func TestPatchVerificationCancelRecordFinalizeOrders(test *testing.T) {
	orders := [][]string{
		{"record", "cancel", "finalize"}, {"record", "finalize", "cancel"},
		{"cancel", "record", "finalize"}, {"cancel", "finalize", "record"},
		{"finalize", "record", "cancel"}, {"finalize", "cancel", "record"},
	}
	for _, order := range orders {
		test.Run(strings.Join(order, "/"), func(test *testing.T) {
			storage, _, binding, evidence := createPatchVerificationFixture(test)
			for _, operation := range order {
				switch operation {
				case "record":
					for _, entry := range evidence {
						err := storage.RecordPatchVerificationEvidence(test.Context(), binding, entry)
						if err != nil && !errors.Is(err, verification.ErrClosed) {
							test.Fatal(err)
						}
					}
				case "cancel":
					if _, err := storage.CancelPatchVerificationRun(test.Context(), binding); err != nil {
						test.Fatal(err)
					}
				case "finalize":
					if _, err := storage.FinalizePatchVerificationRun(test.Context(), binding); err != nil {
						test.Fatal(err)
					}
				}
			}
			record, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
			wantState, wantConclusion := verification.RunCancelled, verification.UnableToVerify
			if order[0] == "finalize" || (order[0] == "record" && order[1] == "finalize") {
				wantState = verification.RunFinalized
			}
			if order[0] == "record" && order[1] == "finalize" {
				wantConclusion = verification.Verified
			}
			if err != nil || record.State != wantState || record.Assessment.Conclusion != wantConclusion {
				test.Fatalf("first terminal operation was not preserved: record=%+v error=%v", record, err)
			}
			repeated, err := storage.CancelPatchVerificationRun(test.Context(), binding)
			if err != nil || !reflect.DeepEqual(record, repeated) {
				test.Fatalf("repeated cancellation changed record: %v", err)
			}
		})
	}
}

func TestPatchVerificationIndependentConnectionsRace(test *testing.T) {
	for range 12 {
		path := filepath.Join(test.TempDir(), "race.sqlite")
		first := openPatchVerificationTestStore(test, path)
		second := openPatchVerificationTestStore(test, path)
		third := openPatchVerificationTestStore(test, path)
		manifest, binding, provenance, evidence := patchVerificationFixture(test)
		if err := first.CreatePatchVerificationRun(test.Context(), manifest, binding, provenance); err != nil {
			test.Fatal(err)
		}
		recordPatchVerificationFixture(test, first, binding, evidence[:5])
		start := make(chan struct{})
		results := make(chan error, 3)
		var workers sync.WaitGroup
		for _, operation := range []func() error{
			func() error { return first.RecordPatchVerificationEvidence(test.Context(), binding, evidence[5]) },
			func() error { _, err := second.FinalizePatchVerificationRun(test.Context(), binding); return err },
			func() error { _, err := third.CancelPatchVerificationRun(test.Context(), binding); return err },
		} {
			workers.Go(func() { <-start; results <- operation() })
		}
		close(start)
		workers.Wait()
		close(results)
		for err := range results {
			if err != nil && !errors.Is(err, verification.ErrClosed) {
				test.Fatal(err)
			}
		}
		record, err := first.GetPatchVerificationRun(test.Context(), binding.RunID)
		if err != nil || (record.State != verification.RunCancelled && record.State != verification.RunFinalized) {
			test.Fatalf("race did not preserve a terminal result: record=%+v error=%v", record, err)
		}
		if record.State == verification.RunCancelled && record.Assessment.Conclusion != verification.UnableToVerify {
			test.Fatal("cancellation winner produced a favorable conclusion")
		}
		if record.Assessment.Conclusion == verification.Verified && (len(record.Evidence) != len(evidence) || len(record.Incidents) != 0) {
			test.Fatal("finalization winner accepted incomplete or invalidated evidence")
		}
	}
}

func TestPatchVerificationBlobReadPersistsCorruption(test *testing.T) {
	storage, _, binding, evidence := createPatchVerificationFixture(test)
	recordPatchVerificationFixture(test, storage, binding, evidence)
	digest := evidence[0].Observation.StdoutDigest
	if _, err := storage.db.ExecContext(test.Context(), `UPDATE patch_verification_blobs SET data = 'corrupt' WHERE run_id = ? AND digest = ?`, binding.RunID, digest); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.GetPatchVerificationBlob(test.Context(), binding.RunID, digest); !errors.Is(err, verification.ErrIntegrity) {
		test.Fatalf("corrupt blob returned: %v", err)
	}
	if _, err := storage.db.ExecContext(test.Context(), `UPDATE patch_verification_blobs SET data = ? WHERE run_id = ? AND digest = ?`, evidence[0].Blobs[digest], binding.RunID, digest); err != nil {
		test.Fatal(err)
	}
	record, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
	if err != nil || record.Assessment.Conclusion != verification.UnableToVerify || len(record.Incidents) != 1 {
		test.Fatalf("direct blob read did not persist corruption: %v", err)
	}
}

func TestPatchVerificationConflictReceiptsAreBounded(test *testing.T) {
	storage, _, binding, evidence := createPatchVerificationFixture(test)
	recordPatchVerificationFixture(test, storage, binding, evidence)
	for iteration := range verification.MaxIncidents + 5 {
		conflict := evidence[0]
		conflict.Observation.ContainerID = fmt.Sprintf("conflicting-container-%d", iteration)
		if err := storage.RecordPatchVerificationEvidence(test.Context(), binding, conflict); !errors.Is(err, verification.ErrConflict) {
			test.Fatalf("conflicting delivery accepted: %v", err)
		}
	}
	record, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
	if err != nil || record.Assessment.Conclusion != verification.UnableToVerify || len(record.Incidents) != verification.MaxIncidents+1 {
		test.Fatalf("unbounded or missing conflict receipts: %v", err)
	}
	foundOverflow := false
	for _, incident := range record.Incidents {
		foundOverflow = foundOverflow || incident.Kind == "overflow"
	}
	if !foundOverflow {
		test.Fatal("receipt overflow was not recorded")
	}
}

func TestPatchVerificationServiceOutputIntegrity(test *testing.T) {
	for _, invalid := range []string{"", "missing", "corrupt", "wrong count", "unknown", "truncated"} {
		name := invalid
		if name == "" {
			name = "valid"
		}
		test.Run(name, func(test *testing.T) {
			storage := setupTestStore(test)
			manifest, _, provenance, evidence := patchVerificationFixture(test)
			manifest.Environment.Profile = verification.LocalServices
			manifest.Environment.Services = []verification.Service{{ID: "fixture", Command: []string{"/service"}, Port: 8080, ReadyOutput: "ready"}}
			for index := range manifest.Checks {
				manifest.Checks[index].Healthy.Services = map[string]string{"fixture": "observed"}
				manifest.Checks[index].Failure.Services = map[string]string{"fixture": "observed"}
			}
			binding, err := verification.NewRunBinding(manifest, "attempt-one", "task-original-uid", "task-patched-uid")
			if err != nil {
				test.Fatal(err)
			}
			if err := storage.InitializePatchVerificationStore(test.Context()); err != nil {
				test.Fatal(err)
			}
			if err := storage.CreatePatchVerificationRun(test.Context(), manifest, binding, provenance); err != nil {
				test.Fatal(err)
			}
			serviceDigest := verification.Digest([]byte("observed"))
			for index := range evidence {
				evidence[index].Observation.RunID = binding.RunID
				evidence[index].Observation.ManifestDigest = binding.ManifestDigest
				evidence[index].Observation.ServiceOutputs = map[string]verification.CapturedOutput{"fixture": {Digest: serviceDigest, Bytes: len("observed")}}
				evidence[index].Blobs[serviceDigest] = []byte("observed")
			}
			switch invalid {
			case "missing":
				delete(evidence[0].Blobs, serviceDigest)
			case "corrupt":
				evidence[0].Blobs[serviceDigest] = []byte("corrupt!")
			case "wrong count":
				evidence[0].Observation.ServiceOutputs["fixture"] = verification.CapturedOutput{Digest: serviceDigest, Bytes: 1}
			case "unknown":
				evidence[0].Observation.ServiceOutputs = map[string]verification.CapturedOutput{"unknown": {Digest: serviceDigest, Bytes: len("observed")}}
			case "truncated":
				evidence[0].Observation.ServiceOutputs["fixture"] = verification.CapturedOutput{Digest: serviceDigest, Bytes: len("observed"), Truncated: true}
			}
			if invalid == "" || invalid == "truncated" {
				recordPatchVerificationFixture(test, storage, binding, evidence)
			} else if err := storage.RecordPatchVerificationEvidence(test.Context(), binding, evidence[0]); !errors.Is(err, verification.ErrEvidence) {
				test.Fatalf("invalid service evidence accepted: %v", err)
			}
			record, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
			if err != nil {
				test.Fatal(err)
			}
			want := verification.UnableToVerify
			if invalid == "" {
				want = verification.Verified
			}
			if record.Assessment.Conclusion != want {
				test.Fatalf("conclusion=%s want=%s", record.Assessment.Conclusion, want)
			}
		})
	}
}

func TestPatchVerificationSealedReopenCleanup(test *testing.T) {
	path := filepath.Join(test.TempDir(), "sealed.sqlite")
	storage := openPatchVerificationTestStore(test, path)
	manifest, binding, provenance, evidence := patchVerificationFixture(test)
	if err := storage.CreatePatchVerificationRun(test.Context(), manifest, binding, provenance); err != nil {
		test.Fatal(err)
	}
	recordPatchVerificationFixture(test, storage, binding, evidence)
	original, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
	if err != nil {
		test.Fatal(err)
	}
	if err := storage.db.Close(); err != nil {
		test.Fatal(err)
	}
	reopened := openPatchVerificationTestStore(test, path)
	for _, taskID := range []string{binding.OriginalTaskID, binding.PatchedTaskID} {
		if err := reopened.SaveResult(test.Context(), "default", taskID, []byte("result")); err != nil {
			test.Fatal(err)
		}
		if err := reopened.DeleteResult(test.Context(), "default", taskID); err != nil {
			test.Fatal(err)
		}
		if err := reopened.DeleteArtifacts(test.Context(), store.ArtifactOwner{Namespace: "default", TaskName: taskID}); err != nil {
			test.Fatal(err)
		}
	}
	recovered, err := reopened.RecoverInterruptedPatchVerificationRun(test.Context(), binding)
	if err != nil || !reflect.DeepEqual(recovered, original) {
		test.Fatalf("startup recovery changed completed run: %v", err)
	}
	if err := reopened.CreatePatchVerificationRun(test.Context(), manifest, binding, provenance); err != nil {
		test.Fatal(err)
	}
	record, err := reopened.FinalizePatchVerificationRun(test.Context(), binding)
	if err != nil || !reflect.DeepEqual(record, original) {
		test.Fatalf("sealed record changed after cleanup and reopen: %v", err)
	}
}

func TestPatchVerificationProcessWriter(test *testing.T) {
	path := os.Getenv("ORKA_PATCH_VERIFICATION_TEST_DB")
	if path == "" {
		test.Skip("subprocess helper")
	}
	storage := openPatchVerificationTestStore(test, path)
	manifest, binding, provenance, evidence := patchVerificationFixture(test)
	if err := storage.CreatePatchVerificationRun(test.Context(), manifest, binding, provenance); err != nil {
		test.Fatal(err)
	}
	recordPatchVerificationFixture(test, storage, binding, evidence)
	record, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
	if err != nil || record.Assessment.Conclusion != verification.Verified {
		test.Fatalf("subprocess could not finalize: %v", err)
	}
}

func TestPatchVerificationMultipleProcesses(test *testing.T) {
	path := filepath.Join(test.TempDir(), "process.sqlite")
	storage := openPatchVerificationTestStore(test, path)
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			<-start
			command := exec.CommandContext(test.Context(), os.Args[0], "-test.run=^TestPatchVerificationProcessWriter$", "-test.count=1")
			command.Env = append(os.Environ(), "ORKA_PATCH_VERIFICATION_TEST_DB="+path)
			output, err := command.CombinedOutput()
			if err != nil {
				results <- fmt.Errorf("subprocess failed: %w: %s", err, output)
				return
			}
			results <- nil
		})
	}
	close(start)
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			test.Fatal(err)
		}
	}
	_, binding, _, evidence := patchVerificationFixture(test)
	record, err := storage.GetPatchVerificationRun(test.Context(), binding.RunID)
	if err != nil || record.Assessment.Conclusion != verification.Verified || len(record.Evidence) != len(evidence) || len(record.Incidents) != 0 {
		test.Fatalf("processes lost or conflicted identical evidence: %v", err)
	}
}

func TestPatchVerificationControlBindings(test *testing.T) {
	mutations := map[string]func(*verification.Binding){
		"run":           func(binding *verification.Binding) { binding.RunID = "foreign" },
		"attempt":       func(binding *verification.Binding) { binding.AttemptID = "foreign" },
		"original task": func(binding *verification.Binding) { binding.OriginalTaskID = "foreign" },
		"patched task":  func(binding *verification.Binding) { binding.PatchedTaskID = "foreign" },
		"manifest":      func(binding *verification.Binding) { binding.ManifestDigest = verification.Digest([]byte("foreign")) },
	}
	for name, mutate := range mutations {
		test.Run(name, func(test *testing.T) {
			storage, manifest, binding, evidence := createPatchVerificationFixture(test)
			_, _, provenance, _ := patchVerificationFixture(test)
			foreign := binding
			mutate(&foreign)
			operations := []func() error{
				func() error { return storage.CreatePatchVerificationRun(test.Context(), manifest, foreign, provenance) },
				func() error { _, err := storage.FinalizePatchVerificationRun(test.Context(), foreign); return err },
				func() error { _, err := storage.CancelPatchVerificationRun(test.Context(), foreign); return err },
				func() error {
					_, err := storage.RecoverInterruptedPatchVerificationRun(test.Context(), foreign)
					return err
				},
			}
			for _, operation := range operations {
				err := operation()
				if !errors.Is(err, verification.ErrBinding) && !errors.Is(err, verification.ErrRunNotFound) {
					test.Fatalf("foreign control operation accepted: %v", err)
				}
			}
			recordPatchVerificationFixture(test, storage, binding, evidence)
			record, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
			if err != nil || record.Assessment.Conclusion != verification.Verified || len(record.Incidents) != 0 {
				test.Fatalf("foreign control invalidated legitimate run: %v", err)
			}
		})
	}
}

func TestPatchVerificationBindingLimits(test *testing.T) {
	storage, _, binding, evidence := createPatchVerificationFixture(test)
	binding.RunID = strings.Repeat("x", 4097)
	evidence[0].Observation.RunID = binding.RunID
	operations := map[string]func() error{
		"finalize": func() error {
			_, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
			return err
		},
		"cancel": func() error {
			_, err := storage.CancelPatchVerificationRun(test.Context(), binding)
			return err
		},
		"recover": func() error {
			_, err := storage.RecoverInterruptedPatchVerificationRun(test.Context(), binding)
			return err
		},
		"evidence": func() error {
			return storage.RecordPatchVerificationEvidence(test.Context(), binding, evidence[0])
		},
	}
	for name, operation := range operations {
		if err := operation(); !errors.Is(err, verification.ErrBinding) {
			test.Errorf("%s did not reject oversized binding before lookup: %v", name, err)
		}
	}
}

func TestPatchVerificationFailedExecutionCannotBeReplaced(test *testing.T) {
	mutations := map[string]func(*verification.ExecutionEvidence){
		"setup failure": func(evidence *verification.ExecutionEvidence) {
			evidence.Observation.Executed = false
			evidence.Observation.SetupError = "fixture unavailable"
			evidence.Observation.StdoutDigest, evidence.Observation.StderrDigest = "", ""
			evidence.Observation.StdoutBytes, evidence.Observation.StderrBytes = 0, 0
			evidence.Blobs = nil
		},
		"timeout":            func(evidence *verification.ExecutionEvidence) { evidence.Observation.TimedOut = true },
		"skipped":            func(evidence *verification.ExecutionEvidence) { evidence.Observation.Skipped = true },
		"truncated":          func(evidence *verification.ExecutionEvidence) { evidence.Observation.OutputTruncated = true },
		"missing executable": func(evidence *verification.ExecutionEvidence) { evidence.Observation.ExitCode = new(127) },
	}
	for name, mutate := range mutations {
		test.Run(name, func(test *testing.T) {
			storage, _, binding, evidence := createPatchVerificationFixture(test)
			failed := evidence[3]
			mutate(&failed)
			recordPatchVerificationFixture(test, storage, binding, []verification.ExecutionEvidence{failed})
			recordPatchVerificationFixture(test, storage, binding, evidence[:3])
			recordPatchVerificationFixture(test, storage, binding, evidence[4:])
			record, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
			if err != nil || record.Assessment.Conclusion != verification.UnableToVerify || len(record.Evidence) != len(evidence) {
				test.Fatalf("failed execution was discarded: %v", err)
			}
			if err := storage.RecordPatchVerificationEvidence(test.Context(), binding, evidence[3]); !errors.Is(err, verification.ErrConflict) {
				test.Fatalf("successful retry replaced failure: %v", err)
			}
			retried, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
			if err != nil || retried.Assessment.Conclusion != verification.UnableToVerify || !reflect.DeepEqual(retried.Seal, record.Seal) {
				test.Fatalf("successful retry changed the failed seal: %v", err)
			}
		})
	}
}

func TestPatchVerificationMetadataCorruption(test *testing.T) {
	mutations := map[string]string{
		"manifest bytes":      `UPDATE patch_verification_runs SET manifest = manifest || ' ' WHERE run_id = ?`,
		"observation bytes":   `UPDATE patch_verification_evidence SET observation = observation || ' ' WHERE run_id = ?`,
		"missing observation": `DELETE FROM patch_verification_evidence WHERE run_id = ? AND slot = 'patched:normal'`,
		"seal bytes":          `UPDATE patch_verification_runs SET seal_content = seal_content || ' ' WHERE run_id = ?`,
		"seal digest":         `UPDATE patch_verification_runs SET seal_digest = 'wrong' WHERE run_id = ?`,
		"missing seal":        `UPDATE patch_verification_runs SET seal_content = NULL, seal_digest = NULL WHERE run_id = ?`,
		"running after seal":  `UPDATE patch_verification_runs SET state = 'running' WHERE run_id = ?`,
	}
	for name, statement := range mutations {
		test.Run(name, func(test *testing.T) {
			storage, _, binding, evidence := createPatchVerificationFixture(test)
			recordPatchVerificationFixture(test, storage, binding, evidence)
			if _, err := storage.FinalizePatchVerificationRun(test.Context(), binding); err != nil {
				test.Fatal(err)
			}
			if _, err := storage.db.ExecContext(test.Context(), statement, binding.RunID); err != nil {
				test.Fatal(err)
			}
			record, err := storage.GetPatchVerificationRun(test.Context(), binding.RunID)
			if !errors.Is(err, verification.ErrIntegrity) || record == nil || record.Assessment.Conclusion != verification.UnableToVerify {
				test.Fatalf("corrupt metadata produced success: record=%+v error=%v", record, err)
			}
			record, err = storage.FinalizePatchVerificationRun(test.Context(), binding)
			if name == "missing seal" && !errors.Is(err, verification.ErrIntegrity) {
				test.Fatalf("deleted seal allowed a second finalization: %v", err)
			}
			if err != nil && !errors.Is(err, verification.ErrIntegrity) {
				test.Fatal(err)
			}
			if record == nil || record.Assessment.Conclusion != verification.UnableToVerify {
				test.Fatal("finalization resurrected corrupted metadata")
			}
		})
	}
}

func TestPatchVerificationCreationLimitsAndCredentials(test *testing.T) {
	mutations := map[string]func(*verification.Manifest){
		"manifest size": func(manifest *verification.Manifest) {
			manifest.Problem = strings.Repeat("x", verification.MaxManifestBytes+1)
		},
		"credential environment": func(manifest *verification.Manifest) {
			manifest.Environment.Variables = map[string]string{"SERVICE_TOKEN": "placeholder"}
		},
		"repository user info": func(manifest *verification.Manifest) {
			manifest.Sources.Repository = "https://fixture-user@example.test/repo"
		},
		"frozen file count": func(manifest *verification.Manifest) {
			for iteration := 0; iteration <= verification.MaxFrozenFiles; iteration++ {
				manifest.Files = append(manifest.Files, verification.FrozenFile{Path: fmt.Sprintf("check-%d", iteration), Content: []byte("check"), Digest: verification.Digest([]byte("check"))})
			}
		},
	}
	for name, mutate := range mutations {
		test.Run(name, func(test *testing.T) {
			storage := setupTestStore(test)
			manifest, _, provenance, _ := patchVerificationFixture(test)
			mutate(&manifest)
			binding, err := verification.NewRunBinding(manifest, "attempt-one", "original-task", "patched-task")
			if err != nil {
				test.Fatal(err)
			}
			if err := storage.InitializePatchVerificationStore(test.Context()); err != nil {
				test.Fatal(err)
			}
			err = storage.CreatePatchVerificationRun(test.Context(), manifest, binding, provenance)
			if !errors.Is(err, verification.ErrLimit) && !errors.Is(err, verification.ErrEvidence) {
				test.Fatalf("unsafe creation accepted: %v", err)
			}
			if _, err := storage.GetPatchVerificationRun(test.Context(), binding.RunID); !errors.Is(err, verification.ErrRunNotFound) {
				test.Fatalf("unsafe creation left a record: %v", err)
			}
		})
	}
}

func TestPatchVerificationStopPersistsCorruption(test *testing.T) {
	for _, operation := range []string{"cancel", "recover"} {
		test.Run(operation, func(test *testing.T) {
			storage, _, binding, evidence := createPatchVerificationFixture(test)
			recordPatchVerificationFixture(test, storage, binding, evidence)
			var originalSeal *verification.Seal
			if operation == "cancel" {
				record, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
				if err != nil {
					test.Fatal(err)
				}
				originalSeal = record.Seal
			}
			digest := evidence[0].Observation.StdoutDigest
			if _, err := storage.db.ExecContext(test.Context(), `UPDATE patch_verification_blobs SET data = 'corrupt' WHERE run_id = ? AND digest = ?`, binding.RunID, digest); err != nil {
				test.Fatal(err)
			}
			stop := storage.CancelPatchVerificationRun
			wantState := verification.RunCancelled
			if operation == "recover" {
				stop = storage.RecoverInterruptedPatchVerificationRun
				wantState = verification.RunInterrupted
			}
			record, err := stop(test.Context(), binding)
			if !errors.Is(err, verification.ErrIntegrity) || record == nil || record.Assessment.Conclusion != verification.UnableToVerify {
				test.Fatalf("corrupt stop did not fail closed: record=%+v error=%v", record, err)
			}
			if _, err := storage.db.ExecContext(test.Context(), `UPDATE patch_verification_blobs SET data = ? WHERE run_id = ? AND digest = ?`, evidence[0].Blobs[digest], binding.RunID, digest); err != nil {
				test.Fatal(err)
			}
			record, err = storage.FinalizePatchVerificationRun(test.Context(), binding)
			if err != nil || record.State != wantState || record.Assessment.Conclusion != verification.UnableToVerify || len(record.Incidents) == 0 {
				test.Fatalf("stop discarded detected corruption: record=%+v error=%v", record, err)
			}
			if originalSeal != nil && !reflect.DeepEqual(record.Seal, originalSeal) {
				test.Fatal("corrupt cancellation rewrote original seal")
			}
		})
	}
}

func TestPatchVerificationMaximumEvidenceCanBeSealed(test *testing.T) {
	storage := setupTestStore(test)
	manifest, _, provenance, fixtures := patchVerificationFixture(test)
	for len(manifest.Checks) < 100 {
		check := manifest.Checks[0]
		check.ID = fmt.Sprintf("case-%d", len(manifest.Checks))
		manifest.Checks = append(manifest.Checks, check)
	}
	binding, err := verification.NewRunBinding(manifest, "attempt-one", "task-original-uid", "task-patched-uid")
	if err != nil {
		test.Fatal(err)
	}
	if err := storage.InitializePatchVerificationStore(test.Context()); err != nil {
		test.Fatal(err)
	}
	if err := storage.CreatePatchVerificationRun(test.Context(), manifest, binding, provenance); err != nil {
		test.Fatal(err)
	}
	for _, side := range []string{verification.Original, verification.Patched} {
		for _, check := range manifest.Checks {
			observation := fixtures[0].Observation
			if side == verification.Patched {
				observation = fixtures[3].Observation
			}
			observation.RunID, observation.ManifestDigest, observation.CheckID = binding.RunID, binding.ManifestDigest, check.ID
			observation.Executed = false
			observation.StdoutDigest, observation.StderrDigest = "", ""
			observation.StdoutBytes, observation.StderrBytes = 0, 0
			observation.SetupError = "failure"
			content, err := json.Marshal(observation)
			if err != nil {
				test.Fatal(err)
			}
			observation.SetupError += strings.Repeat("x", verification.MaxObservationBytes-len(content))
			if err := storage.RecordPatchVerificationEvidence(test.Context(), binding, verification.ExecutionEvidence{Observation: observation}); err != nil {
				test.Fatalf("near-limit observation was not accepted: %v", err)
			}
		}
	}
	record, err := storage.FinalizePatchVerificationRun(test.Context(), binding)
	if err != nil || record.Seal == nil || record.Assessment.Conclusion != verification.UnableToVerify || len(record.Evidence) != 200 {
		test.Fatalf("accepted maximum evidence is not sealable: %v", err)
	}
	if _, err := storage.FinalizePatchVerificationRun(test.Context(), binding); err != nil {
		test.Fatalf("near-limit seal is not replayable: %v", err)
	}
}

func TestPatchVerificationIncidentCapSurvivesEarlyCorruption(test *testing.T) {
	storage, _, binding, evidence := createPatchVerificationFixture(test)
	recordPatchVerificationFixture(test, storage, binding, evidence)
	for iteration := range verification.MaxIncidents + 1 {
		conflict := evidence[0]
		conflict.Observation.ContainerID = fmt.Sprintf("conflicting-container-%d", iteration)
		if err := storage.RecordPatchVerificationEvidence(test.Context(), binding, conflict); !errors.Is(err, verification.ErrConflict) {
			test.Fatal(err)
		}
	}
	for _, field := range []string{"manifest", "observation"} {
		var original []byte
		read := `SELECT manifest FROM patch_verification_runs WHERE run_id = ?`
		write := `UPDATE patch_verification_runs SET manifest = ? WHERE run_id = ?`
		if field == "observation" {
			read = `SELECT observation FROM patch_verification_evidence WHERE run_id = ? AND slot = 'original:case-one'`
			write = `UPDATE patch_verification_evidence SET observation = ? WHERE run_id = ? AND slot = 'original:case-one'`
		}
		if err := storage.db.QueryRowContext(test.Context(), read, binding.RunID).Scan(&original); err != nil {
			test.Fatal(err)
		}
		if _, err := storage.db.ExecContext(test.Context(), write, []byte("{}"), binding.RunID); err != nil {
			test.Fatal(err)
		}
		if _, err := storage.GetPatchVerificationRun(test.Context(), binding.RunID); !errors.Is(err, verification.ErrIntegrity) {
			test.Fatalf("corrupt %s was accepted: %v", field, err)
		}
		if _, err := storage.db.ExecContext(test.Context(), write, original, binding.RunID); err != nil {
			test.Fatal(err)
		}
		record, err := storage.GetPatchVerificationRun(test.Context(), binding.RunID)
		if err != nil {
			test.Fatalf("repaired %s is unreadable after saturated incidents: %v", field, err)
		}
		if record.Assessment.Conclusion != verification.UnableToVerify || len(record.Incidents) != verification.MaxIncidents+1 {
			test.Fatalf("incident cap changed after repairing %s", field)
		}
	}
	if _, err := storage.FinalizePatchVerificationRun(test.Context(), binding); err != nil {
		test.Fatalf("incident saturation blocked finalization: %v", err)
	}
	if _, err := storage.GetPatchVerificationBlob(test.Context(), binding.RunID, evidence[0].Observation.StdoutDigest); err != nil {
		test.Fatalf("incident saturation blocked intact blob retrieval: %v", err)
	}
}
