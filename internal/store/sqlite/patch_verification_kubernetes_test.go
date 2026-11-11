package sqlite

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	verification "github.com/orka-agents/orka/internal/patchverification"
	"github.com/stretchr/testify/require"
)

func kubernetesValidationFixture(t *testing.T, namespace, requestID string, report bool) (verification.KubernetesSubmission, verification.Binding, []verification.ExecutionEvidence) {
	t.Helper()
	manifest, _, provenance, evidence := patchVerificationFixture(t)
	patchedName, patchedUID := "pv-"+requestID+"-patched", namespace+"-"+requestID+"-patched-uid"
	if report {
		manifest.Action = verification.ValidateReport
		var err error
		manifest.ReportDigest, err = verification.StableReportDigest(manifest.Problem, manifest.Scope)
		require.NoError(t, err)
		delete(provenance, manifest.Sources.Patched.ArchiveDigest)
		delete(provenance, manifest.Sources.DiffDigest)
		manifest.Sources.Patched, manifest.Sources.DiffDigest = verification.SourceIdentity{}, ""
		patchedName, patchedUID = "", ""
		evidence = evidence[:len(manifest.Checks)]
	}
	submission := verification.KubernetesSubmission{
		Namespace: namespace, RequestID: requestID, SubmittedBy: "test-submitter", AttemptID: "attempt-" + namespace + "-" + requestID,
		OriginalTaskName: "pv-" + requestID + "-original", PatchedTaskName: patchedName,
		Manifest: manifest, Provenance: provenance, State: verification.SubmissionPreparing,
	}
	binding, err := verification.NewRunBinding(manifest, submission.AttemptID, namespace+"-"+requestID+"-original-uid", patchedUID)
	require.NoError(t, err)
	for index := range evidence {
		observation := &evidence[index].Observation
		observation.RunID, observation.AttemptID = binding.RunID, binding.AttemptID
		observation.ManifestDigest, observation.TaskID = binding.ManifestDigest, binding.OriginalTaskID
		if observation.Side == verification.Patched {
			observation.TaskID = binding.PatchedTaskID
		}
	}
	return submission, binding, evidence
}

func kubernetesValidationTestStore(t *testing.T) *Store {
	t.Helper()
	storage := setupTestStore(t)
	require.NoError(t, storage.InitializeKubernetesValidationStore(t.Context()))
	return storage
}

func createBoundKubernetesValidation(t *testing.T, storage *Store, submission verification.KubernetesSubmission, binding verification.Binding) {
	t.Helper()
	require.NoError(t, storage.CreateKubernetesValidationSubmission(t.Context(), submission))
	recordKubernetesValidationTaskUIDs(t, storage, submission, binding)
	require.NoError(t, storage.CreatePatchVerificationRun(t.Context(), submission.Manifest, binding, submission.Provenance))
	require.NoError(t, storage.BindKubernetesValidationTasks(t.Context(), submission.Namespace, submission.RequestID,
		binding.OriginalTaskID, binding.PatchedTaskID, binding))
}

func recordKubernetesValidationTaskUIDs(t *testing.T, storage *Store, submission verification.KubernetesSubmission, binding verification.Binding) {
	t.Helper()
	if binding.PatchedTaskID != "" {
		require.NoError(t, storage.RecordKubernetesValidationTaskUID(t.Context(), submission.Namespace, submission.RequestID,
			verification.Patched, binding.PatchedTaskID))
	}
	require.NoError(t, storage.RecordKubernetesValidationTaskUID(t.Context(), submission.Namespace, submission.RequestID,
		verification.Original, binding.OriginalTaskID))
}

func initializeLegacyKubernetesValidationStore(t *testing.T, storage *Store) {
	t.Helper()
	require.NoError(t, storage.InitializePatchVerificationStore(t.Context()))
	for _, statement := range []string{
		`CREATE TABLE validation_submissions (
			namespace TEXT NOT NULL, request_id TEXT NOT NULL, submitted_by TEXT NOT NULL,
			attempt_id TEXT NOT NULL, original_task_name TEXT NOT NULL, patched_task_name TEXT NOT NULL,
			original_task_uid TEXT NOT NULL DEFAULT '', patched_task_uid TEXT NOT NULL DEFAULT '',
			manifest BLOB NOT NULL CHECK(length(manifest) <= 1048576),
			provenance BLOB NOT NULL CHECK(length(provenance) <= 134217728),
			binding BLOB, run_id TEXT NOT NULL DEFAULT '', state TEXT NOT NULL,
			failure TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
			PRIMARY KEY(namespace, request_id), UNIQUE(namespace, original_task_name),
			UNIQUE(namespace, patched_task_name), CHECK(namespace <> 'forbidden'))`,
		`CREATE TABLE validation_dispatches (
			run_id TEXT NOT NULL, side TEXT NOT NULL, check_id TEXT NOT NULL,
			job_name TEXT NOT NULL, spec_digest TEXT NOT NULL, job_uid TEXT NOT NULL DEFAULT '',
			pod_uid TEXT NOT NULL DEFAULT '', container_id TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL, failure TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(run_id, side, check_id), UNIQUE(job_name))`,
		`CREATE INDEX legacy_submission_state ON validation_submissions(state)`,
		`CREATE TABLE legacy_submission_updates (count INTEGER NOT NULL)`,
		`INSERT INTO legacy_submission_updates VALUES(0)`,
		`CREATE TRIGGER legacy_submission_update AFTER UPDATE OF state ON validation_submissions
			BEGIN UPDATE legacy_submission_updates SET count=count+1; END`,
	} {
		_, err := storage.db.ExecContext(t.Context(), statement)
		require.NoError(t, err)
	}
}

func TestKubernetesValidationSubmissionSchema(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			storage := setupTestStore(t)
			if legacy {
				initializeLegacyKubernetesValidationStore(t, storage)
			} else {
				require.NoError(t, storage.InitializeKubernetesValidationStore(t.Context()))
			}
			first, binding, evidence := kubernetesValidationFixture(t, "namespace-a", "first", true)
			createBoundKubernetesValidation(t, storage, first, binding)
			dispatch := kubernetesValidationDispatchFixture(binding, evidence[0])
			require.NoError(t, storage.CreateKubernetesValidationDispatch(t.Context(), dispatch))
			for _, entry := range evidence {
				require.NoError(t, storage.RecordPatchVerificationEvidence(t.Context(), binding, entry))
			}
			_, err := storage.FinalizePatchVerificationRun(t.Context(), binding)
			require.NoError(t, err)
			before, err := storage.GetKubernetesValidationSubmission(t.Context(), first.Namespace, first.RequestID)
			require.NoError(t, err)
			beforeEvidence, err := storage.GetPatchVerificationRun(t.Context(), binding.RunID)
			require.NoError(t, err)
			if legacy {
				second, _, _ := kubernetesValidationFixture(t, first.Namespace, "second", true)
				require.Error(t, storage.CreateKubernetesValidationSubmission(t.Context(), second))
			}
			for range 2 {
				require.NoError(t, storage.InitializeKubernetesValidationStore(t.Context()))
			}
			after, err := storage.GetKubernetesValidationSubmission(t.Context(), first.Namespace, first.RequestID)
			require.NoError(t, err)
			require.Equal(t, before, after)
			afterEvidence, err := storage.GetPatchVerificationRun(t.Context(), binding.RunID)
			require.NoError(t, err)
			require.Equal(t, beforeEvidence, afterEvidence)
			persistedDispatch, err := storage.GetKubernetesValidationDispatch(t.Context(), binding.RunID, dispatch.Side, dispatch.CheckID)
			require.NoError(t, err)
			require.Equal(t, dispatch, *persistedDispatch)
			for digest, content := range first.Provenance {
				persisted, err := storage.GetPatchVerificationBlob(t.Context(), binding.RunID, digest)
				require.NoError(t, err)
				require.Equal(t, content, persisted)
			}

			second, _, _ := kubernetesValidationFixture(t, first.Namespace, "second", true)
			require.NoError(t, storage.CreateKubernetesValidationSubmission(t.Context(), second))
			otherNamespace, _, _ := kubernetesValidationFixture(t, "namespace-b", first.RequestID, true)
			require.NoError(t, storage.CreateKubernetesValidationSubmission(t.Context(), otherNamespace))
			duplicateOriginal := second
			duplicateOriginal.RequestID = "duplicate-original"
			require.Error(t, storage.CreateKubernetesValidationSubmission(t.Context(), duplicateOriginal))

			patch, _, _ := kubernetesValidationFixture(t, first.Namespace, "patch-one", false)
			require.NoError(t, storage.CreateKubernetesValidationSubmission(t.Context(), patch))
			duplicatePatched, _, _ := kubernetesValidationFixture(t, first.Namespace, "patch-two", false)
			duplicatePatched.PatchedTaskName = patch.PatchedTaskName
			require.Error(t, storage.CreateKubernetesValidationSubmission(t.Context(), duplicatePatched))
			duplicatePatched.Namespace = otherNamespace.Namespace
			require.NoError(t, storage.CreateKubernetesValidationSubmission(t.Context(), duplicatePatched))
			_, err = storage.db.ExecContext(t.Context(), `UPDATE validation_submissions SET manifest=zeroblob(1048577)
				WHERE namespace=? AND request_id=?`, first.Namespace, first.RequestID)
			require.Error(t, err)

			var indexSQL string
			require.NoError(t, storage.db.QueryRowContext(t.Context(), `SELECT sql FROM sqlite_master
				WHERE type='index' AND name='validation_submissions_patched_task'`).Scan(&indexSQL))
			require.Contains(t, indexSQL, "WHERE patched_task_name <> ''")
			if legacy {
				require.NoError(t, storage.db.QueryRowContext(t.Context(), `SELECT sql FROM sqlite_master
					WHERE type='index' AND name='legacy_submission_state'`).Scan(&indexSQL))
				forbidden, _, _ := kubernetesValidationFixture(t, "forbidden", "forbidden", true)
				require.Error(t, storage.CreateKubernetesValidationSubmission(t.Context(), forbidden))
				var previousUpdates, updates int
				require.NoError(t, storage.db.QueryRowContext(t.Context(), `SELECT count FROM legacy_submission_updates`).Scan(&previousUpdates))
				require.NoError(t, storage.CompleteKubernetesValidationSubmission(t.Context(), first.Namespace, first.RequestID, ""))
				require.NoError(t, storage.db.QueryRowContext(t.Context(), `SELECT count FROM legacy_submission_updates`).Scan(&updates))
				require.Equal(t, previousUpdates+1, updates)
			}
		})
	}
}

func TestKubernetesValidationMigrationRollback(t *testing.T) {
	storage := setupTestStore(t)
	initializeLegacyKubernetesValidationStore(t, storage)
	for _, namespace := range []string{"namespace-a", "namespace-b"} {
		submission, _, _ := kubernetesValidationFixture(t, namespace, "request", true)
		require.NoError(t, storage.CreateKubernetesValidationSubmission(t.Context(), submission))
	}
	_, err := storage.db.ExecContext(t.Context(), `UPDATE validation_submissions SET run_id='legacy-shared-run'`)
	require.NoError(t, err)
	require.Error(t, storage.InitializeKubernetesValidationStore(t.Context()))
	var definition string
	require.NoError(t, storage.db.QueryRowContext(t.Context(), `SELECT sql FROM sqlite_master
		WHERE type='table' AND name='validation_submissions'`).Scan(&definition))
	require.Contains(t, definition, "UNIQUE(namespace, patched_task_name)")
	var count int
	require.NoError(t, storage.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM validation_submissions`).Scan(&count))
	require.Equal(t, 2, count)
	require.NoError(t, storage.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM sqlite_master
		WHERE name='validation_submissions_migration'`).Scan(&count))
	require.Zero(t, count)
}

func TestKubernetesValidationNamespaceLookupsAndActiveOrdering(t *testing.T) {
	storage := kubernetesValidationTestStore(t)
	base := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	states := []verification.SubmissionState{
		verification.SubmissionPreparing, verification.SubmissionRunning, verification.SubmissionCancelling, verification.SubmissionTerminal,
	}
	for index, state := range states {
		submission, binding, _ := kubernetesValidationFixture(t, "namespace-a", fmt.Sprintf("request-%d", index), true)
		createBoundKubernetesValidation(t, storage, submission, binding)
		_, err := storage.db.ExecContext(t.Context(), `UPDATE validation_submissions SET state=?,created_at=?
			WHERE namespace=? AND request_id=?`, state, base.Add(time.Duration(index)*time.Nanosecond).Format(time.RFC3339Nano),
			submission.Namespace, submission.RequestID)
		require.NoError(t, err)
		found, err := storage.GetKubernetesValidationSubmissionByRun(t.Context(), submission.Namespace, binding.RunID)
		require.NoError(t, err)
		require.Equal(t, submission.RequestID, found.RequestID)
		_, err = storage.GetKubernetesValidationSubmissionByRun(t.Context(), "namespace-b", binding.RunID)
		require.ErrorIs(t, err, verification.ErrRunNotFound)
		_, err = storage.GetKubernetesValidationSubmissionByTask(t.Context(), "namespace-b", submission.OriginalTaskName)
		require.ErrorIs(t, err, verification.ErrRunNotFound)
	}
	otherNamespace, _, _ := kubernetesValidationFixture(t, "namespace-b", "request-0", true)
	require.NoError(t, storage.CreateKubernetesValidationSubmission(t.Context(), otherNamespace))
	active, err := storage.ListActiveKubernetesValidationSubmissions(t.Context(), "namespace-a")
	require.NoError(t, err)
	require.Len(t, active, 3)
	for index, submission := range active {
		require.Equal(t, verification.KubernetesSubmission{
			Namespace: "namespace-a", RequestID: fmt.Sprintf("request-%d", index), State: states[index],
			CreatedAt: base.Add(time.Duration(index) * time.Nanosecond),
		}, submission)
	}
	active, err = storage.ListActiveKubernetesValidationSubmissions(t.Context(), "namespace-b")
	require.NoError(t, err)
	require.Len(t, active, 1)
	active, err = storage.ListActiveKubernetesValidationSubmissions(t.Context(), "absent")
	require.NoError(t, err)
	require.Empty(t, active)
	for _, namespace := range []string{"", " "} {
		_, err := storage.ListActiveKubernetesValidationSubmissions(t.Context(), namespace)
		require.ErrorIs(t, err, verification.ErrBinding)
		_, err = storage.GetKubernetesValidationSubmissionByRun(t.Context(), namespace, "run")
		require.ErrorIs(t, err, verification.ErrBinding)
	}
	_, err = storage.GetKubernetesValidationSubmissionByRun(t.Context(), "namespace-a", "")
	require.ErrorIs(t, err, verification.ErrBinding)
	_, err = storage.GetKubernetesValidationSubmissionByTask(t.Context(), "namespace-a", "")
	require.ErrorIs(t, err, verification.ErrBinding)
}

func TestKubernetesValidationActiveListDoesNotLoadPayloads(t *testing.T) {
	storage := kubernetesValidationTestStore(t)
	submission, binding, _ := kubernetesValidationFixture(t, "namespace-a", "request", true)
	createBoundKubernetesValidation(t, storage, submission, binding)
	before, err := storage.GetKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID)
	require.NoError(t, err)
	_, err = storage.db.ExecContext(t.Context(), `UPDATE validation_submissions
		SET manifest=zeroblob(1048576),provenance=zeroblob(1048576),binding=x'FF'
		WHERE namespace=? AND request_id=?`, submission.Namespace, submission.RequestID)
	require.NoError(t, err)
	active, err := storage.ListActiveKubernetesValidationSubmissions(t.Context(), submission.Namespace)
	require.NoError(t, err)
	require.Equal(t, []verification.KubernetesSubmission{{
		Namespace: submission.Namespace, RequestID: submission.RequestID,
		State: verification.SubmissionRunning, CreatedAt: before.CreatedAt,
	}}, active)
	_, err = storage.GetKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID)
	require.ErrorIs(t, err, verification.ErrIntegrity)
}

func TestKubernetesValidationActiveAdmissionBound(t *testing.T) {
	storage := kubernetesValidationTestStore(t)
	submission, _, _ := kubernetesValidationFixture(t, "namespace-a", "request", true)
	for index := range maxActiveKubernetesValidationSubmissions {
		submission.RequestID = fmt.Sprintf("request-%04d", index)
		submission.OriginalTaskName = submission.RequestID + "-original"
		require.NoError(t, storage.CreateKubernetesValidationSubmission(t.Context(), submission))
	}
	submission.RequestID, submission.OriginalTaskName = "overflow", "overflow-original"
	require.ErrorIs(t, storage.CreateKubernetesValidationSubmission(t.Context(), submission), verification.ErrLimit)
	active, err := storage.ListActiveKubernetesValidationSubmissions(t.Context(), submission.Namespace)
	require.NoError(t, err)
	require.Len(t, active, maxActiveKubernetesValidationSubmissions)
	require.Equal(t, "request-0000", active[0].RequestID)
	require.Equal(t, "request-0999", active[len(active)-1].RequestID)
	submission.Namespace = "namespace-b"
	require.NoError(t, storage.CreateKubernetesValidationSubmission(t.Context(), submission))
	submission.Namespace = "namespace-a"
	require.NoError(t, storage.CompleteKubernetesValidationSubmission(t.Context(), submission.Namespace, "request-0000", ""))
	require.NoError(t, storage.CreateKubernetesValidationSubmission(t.Context(), submission))
	_, err = storage.db.ExecContext(t.Context(), `UPDATE validation_submissions SET state=?
		WHERE namespace=? AND request_id='request-0000'`, verification.SubmissionRunning, submission.Namespace)
	require.NoError(t, err)
	active, err = storage.ListActiveKubernetesValidationSubmissions(t.Context(), submission.Namespace)
	require.ErrorIs(t, err, verification.ErrLimit)
	require.Nil(t, active)
}

func TestKubernetesValidationTaskUIDDelivery(t *testing.T) {
	storage := kubernetesValidationTestStore(t)
	submission, binding, _ := kubernetesValidationFixture(t, "namespace-a", "request", false)
	require.NoError(t, storage.CreateKubernetesValidationSubmission(t.Context(), submission))
	require.ErrorIs(t, storage.RecordKubernetesValidationTaskUID(t.Context(), submission.Namespace, submission.RequestID, "invalid", "uid"), verification.ErrEvidence)
	require.ErrorIs(t, storage.RecordKubernetesValidationTaskUID(t.Context(), submission.Namespace, submission.RequestID, verification.Original, ""), verification.ErrBinding)
	require.ErrorIs(t, storage.RecordKubernetesValidationTaskUID(t.Context(), "namespace-b", submission.RequestID, verification.Original, binding.OriginalTaskID), verification.ErrRunNotFound)
	for range 2 {
		recordKubernetesValidationTaskUIDs(t, storage, submission, binding)
	}
	for _, side := range verification.ActionSides(submission.Manifest.Action) {
		require.ErrorIs(t, storage.RecordKubernetesValidationTaskUID(t.Context(), submission.Namespace, submission.RequestID, side, "changed"), verification.ErrConflict)
	}
	require.ErrorIs(t, storage.RecordKubernetesValidationTaskUID(t.Context(), submission.Namespace, submission.RequestID, verification.Patched, binding.OriginalTaskID), verification.ErrBinding)
	require.NoError(t, storage.CompleteKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID, "stopped"))
	recordKubernetesValidationTaskUIDs(t, storage, submission, binding)
	terminal, err := storage.GetKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID)
	require.NoError(t, err)
	require.Equal(t, verification.SubmissionTerminal, terminal.State)
	require.Equal(t, binding.OriginalTaskID, terminal.OriginalTaskUID)
	require.Equal(t, binding.PatchedTaskID, terminal.PatchedTaskUID)

	report, reportBinding, _ := kubernetesValidationFixture(t, "namespace-a", "report", true)
	require.NoError(t, storage.CreateKubernetesValidationSubmission(t.Context(), report))
	require.ErrorIs(t, storage.RecordKubernetesValidationTaskUID(t.Context(), report.Namespace, report.RequestID, verification.Patched, "patched"), verification.ErrBinding)
	require.NoError(t, storage.RequestKubernetesValidationCancellation(t.Context(), report.Namespace, report.RequestID))
	require.NoError(t, storage.RecordKubernetesValidationTaskUID(t.Context(), report.Namespace, report.RequestID, verification.Original, reportBinding.OriginalTaskID))
	require.ErrorIs(t, storage.RecordKubernetesValidationTaskUID(t.Context(), report.Namespace, report.RequestID, verification.Patched, "patched"), verification.ErrBinding)
}

func TestKubernetesValidationCancellationTaskUIDRecovery(t *testing.T) {
	for _, side := range []string{verification.Original, verification.Patched} {
		t.Run(side, func(t *testing.T) {
			storage := kubernetesValidationTestStore(t)
			submission, binding, _ := kubernetesValidationFixture(t, "namespace-a", "request", false)
			require.NoError(t, storage.CreateKubernetesValidationSubmission(t.Context(), submission))
			require.NoError(t, storage.RequestKubernetesValidationCancellation(t.Context(), submission.Namespace, submission.RequestID))
			uid, otherUID, otherSide := binding.OriginalTaskID, binding.PatchedTaskID, verification.Patched
			if side == verification.Patched {
				uid, otherUID, otherSide = binding.PatchedTaskID, binding.OriginalTaskID, verification.Original
			}
			record := func(recordSide, recordUID string) error {
				return storage.RecordKubernetesValidationTaskUID(t.Context(), submission.Namespace, submission.RequestID, recordSide, recordUID)
			}
			require.NoError(t, record(side, uid))
			cancelling, err := storage.GetKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID)
			require.NoError(t, err)
			require.Equal(t, verification.SubmissionCancelling, cancelling.State)
			require.Empty(t, cancelling.RunID)
			require.Nil(t, cancelling.Binding)
			if side == verification.Original {
				require.Equal(t, uid, cancelling.OriginalTaskUID)
			} else {
				require.Equal(t, uid, cancelling.PatchedTaskUID)
			}
			require.NoError(t, record(side, uid))
			require.ErrorIs(t, record(side, "changed-uid"), verification.ErrConflict)
			require.ErrorIs(t, record("unknown", uid), verification.ErrEvidence)
			require.ErrorIs(t, record(otherSide, uid), verification.ErrBinding)
			unchanged, err := storage.GetKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID)
			require.NoError(t, err)
			require.Equal(t, cancelling, unchanged)

			require.NoError(t, storage.CompleteKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID, "cancelled"))
			terminal, err := storage.GetKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID)
			require.NoError(t, err)
			require.NoError(t, record(side, uid))
			require.ErrorIs(t, record(side, "changed-uid"), verification.ErrConflict)
			require.ErrorIs(t, record(otherSide, otherUID), verification.ErrClosed)
			unchanged, err = storage.GetKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID)
			require.NoError(t, err)
			require.Equal(t, terminal, unchanged)
		})
	}
}

func TestKubernetesValidationBindingDelivery(t *testing.T) {
	storage := kubernetesValidationTestStore(t)
	submission, binding, _ := kubernetesValidationFixture(t, "namespace-a", "request", false)
	require.NoError(t, storage.CreateKubernetesValidationSubmission(t.Context(), submission))
	bind := func(original, patched string, supplied verification.Binding) error {
		return storage.BindKubernetesValidationTasks(t.Context(), submission.Namespace, submission.RequestID, original, patched, supplied)
	}
	require.ErrorIs(t, bind(binding.OriginalTaskID, binding.PatchedTaskID, binding), verification.ErrBinding)
	recordKubernetesValidationTaskUIDs(t, storage, submission, binding)
	require.ErrorIs(t, bind(binding.OriginalTaskID, binding.PatchedTaskID, binding), verification.ErrRunNotFound)
	require.NoError(t, storage.CreatePatchVerificationRun(t.Context(), submission.Manifest, binding, submission.Provenance))
	require.ErrorIs(t, bind("changed-original", binding.PatchedTaskID, binding), verification.ErrBinding)
	require.ErrorIs(t, bind(binding.OriginalTaskID, "changed-patched", binding), verification.ErrBinding)
	wrongRun := binding
	wrongRun.RunID = "another-run"
	require.ErrorIs(t, bind(binding.OriginalTaskID, binding.PatchedTaskID, wrongRun), verification.ErrBinding)
	otherAttempt, err := verification.NewRunBinding(submission.Manifest, "another-attempt", binding.OriginalTaskID, binding.PatchedTaskID)
	require.NoError(t, err)
	require.NoError(t, storage.CreatePatchVerificationRun(t.Context(), submission.Manifest, otherAttempt, submission.Provenance))
	require.ErrorIs(t, bind(binding.OriginalTaskID, binding.PatchedTaskID, otherAttempt), verification.ErrBinding)
	for range 2 {
		require.NoError(t, bind(binding.OriginalTaskID, binding.PatchedTaskID, binding))
	}
	crossNamespace := submission
	crossNamespace.Namespace = "namespace-b"
	require.NoError(t, storage.CreateKubernetesValidationSubmission(t.Context(), crossNamespace))
	recordKubernetesValidationTaskUIDs(t, storage, crossNamespace, binding)
	require.ErrorIs(t, storage.BindKubernetesValidationTasks(t.Context(), crossNamespace.Namespace, crossNamespace.RequestID,
		binding.OriginalTaskID, binding.PatchedTaskID, binding), verification.ErrBinding)
	unbound, err := storage.GetKubernetesValidationSubmission(t.Context(), crossNamespace.Namespace, crossNamespace.RequestID)
	require.NoError(t, err)
	require.Empty(t, unbound.RunID)
	require.Nil(t, unbound.Binding)

	require.NoError(t, storage.RequestKubernetesValidationCancellation(t.Context(), submission.Namespace, submission.RequestID))
	require.NoError(t, bind(binding.OriginalTaskID, binding.PatchedTaskID, binding))
	require.NoError(t, storage.CompleteKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID, "cancelled"))
	require.NoError(t, bind(binding.OriginalTaskID, binding.PatchedTaskID, binding))
	recordKubernetesValidationTaskUIDs(t, storage, submission, binding)
	terminal, err := storage.GetKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID)
	require.NoError(t, err)
	require.Equal(t, verification.SubmissionTerminal, terminal.State)
	require.Equal(t, &binding, terminal.Binding)
}

func TestKubernetesValidationBindingChecksSavedManifest(t *testing.T) {
	storage := kubernetesValidationTestStore(t)
	submission, binding, _ := kubernetesValidationFixture(t, "namespace-a", "request", true)
	require.NoError(t, storage.CreateKubernetesValidationSubmission(t.Context(), submission))
	recordKubernetesValidationTaskUIDs(t, storage, submission, binding)
	require.NoError(t, storage.CreatePatchVerificationRun(t.Context(), submission.Manifest, binding, submission.Provenance))
	changed := submission.Manifest
	changed.Problem = "a different problem"
	var err error
	changed.ReportDigest, err = verification.StableReportDigest(changed.Problem, changed.Scope)
	require.NoError(t, err)
	content, err := validationJSON(changed, verification.MaxManifestBytes)
	require.NoError(t, err)
	_, err = storage.db.ExecContext(t.Context(), `UPDATE validation_submissions SET manifest=? WHERE namespace=? AND request_id=?`,
		content, submission.Namespace, submission.RequestID)
	require.NoError(t, err)
	require.ErrorIs(t, storage.BindKubernetesValidationTasks(t.Context(), submission.Namespace, submission.RequestID,
		binding.OriginalTaskID, binding.PatchedTaskID, binding), verification.ErrBinding)
}

func TestKubernetesValidationSubmissionTerminalTransitions(t *testing.T) {
	for _, initial := range []verification.SubmissionState{
		verification.SubmissionPreparing, verification.SubmissionRunning, verification.SubmissionCancelling,
	} {
		t.Run(string(initial), func(t *testing.T) {
			storage := kubernetesValidationTestStore(t)
			submission, binding, _ := kubernetesValidationFixture(t, "namespace-a", "request", true)
			if initial == verification.SubmissionRunning {
				createBoundKubernetesValidation(t, storage, submission, binding)
			} else {
				require.NoError(t, storage.CreateKubernetesValidationSubmission(t.Context(), submission))
			}
			if initial == verification.SubmissionCancelling {
				for range 2 {
					require.NoError(t, storage.RequestKubernetesValidationCancellation(t.Context(), submission.Namespace, submission.RequestID))
				}
			}
			reason := strings.Repeat("x", 255) + "é"
			require.NoError(t, storage.CompleteKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID, reason))
			terminal, err := storage.GetKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID)
			require.NoError(t, err)
			require.Equal(t, verification.SubmissionTerminal, terminal.State)
			require.Equal(t, strings.Repeat("x", 255), terminal.Failure)
			require.True(t, utf8.ValidString(terminal.Failure))
			require.NoError(t, storage.CompleteKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID, "different retry"))
			require.NoError(t, storage.RequestKubernetesValidationCancellation(t.Context(), submission.Namespace, submission.RequestID))
			unchanged, err := storage.GetKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID)
			require.NoError(t, err)
			require.Equal(t, terminal, unchanged)
			require.ErrorIs(t, storage.CompleteKubernetesValidationSubmission(t.Context(), "namespace-b", submission.RequestID, ""), verification.ErrRunNotFound)
			require.ErrorIs(t, storage.RequestKubernetesValidationCancellation(t.Context(), "namespace-b", submission.RequestID), verification.ErrRunNotFound)
		})
	}
}

func kubernetesValidationDispatchFixture(binding verification.Binding, evidence verification.ExecutionEvidence) verification.KubernetesDispatch {
	return verification.KubernetesDispatch{
		RunID: binding.RunID, Side: evidence.Observation.Side, CheckID: evidence.Observation.CheckID,
		JobName:    "job-" + binding.AttemptID + "-" + evidence.Observation.Side + "-" + evidence.Observation.CheckID,
		SpecDigest: verification.Digest([]byte("job-spec")), State: verification.DispatchPlanned,
	}
}

func TestKubernetesValidationDispatchDelivery(t *testing.T) {
	for _, report := range []bool{false, true} {
		t.Run(fmt.Sprintf("report=%v", report), func(t *testing.T) {
			storage := kubernetesValidationTestStore(t)
			submission, binding, evidence := kubernetesValidationFixture(t, "namespace-a", "request", report)
			createBoundKubernetesValidation(t, storage, submission, binding)
			entry := evidence[0]
			dispatch := kubernetesValidationDispatchFixture(binding, entry)
			ctx := t.Context()
			for range 2 {
				require.NoError(t, storage.CreateKubernetesValidationDispatch(ctx, dispatch))
			}
			changed := dispatch
			changed.JobName += "-changed"
			require.ErrorIs(t, storage.CreateKubernetesValidationDispatch(ctx, changed), verification.ErrConflict)
			changed = dispatch
			changed.SpecDigest = verification.Digest([]byte("other-spec"))
			require.ErrorIs(t, storage.CreateKubernetesValidationDispatch(ctx, changed), verification.ErrConflict)
			changed = dispatch
			changed.CheckID = "unknown"
			require.ErrorIs(t, storage.CreateKubernetesValidationDispatch(ctx, changed), verification.ErrBinding)
			changed = dispatch
			changed.Side = "unknown"
			require.ErrorIs(t, storage.CreateKubernetesValidationDispatch(ctx, changed), verification.ErrBinding)
			changed = dispatch
			changed.RunID = "absent"
			require.ErrorIs(t, storage.CreateKubernetesValidationDispatch(ctx, changed), verification.ErrRunNotFound)

			markCreated := func(uid string) error {
				return storage.MarkKubernetesValidationDispatchCreated(ctx, dispatch.RunID, dispatch.Side, dispatch.CheckID, uid)
			}
			markObserved := func(jobUID, podUID, containerID string) error {
				return storage.MarkKubernetesValidationDispatchObserved(ctx, dispatch.RunID, dispatch.Side, dispatch.CheckID, jobUID, podUID, containerID)
			}
			markRecorded := func(jobUID, podUID string) error {
				return storage.MarkKubernetesValidationDispatchRecorded(ctx, dispatch.RunID, dispatch.Side, dispatch.CheckID, jobUID, podUID)
			}
			containerID := entry.Observation.ContainerID
			require.ErrorIs(t, markCreated(""), verification.ErrBinding)
			require.ErrorIs(t, markObserved("job-uid", "pod-uid", containerID), verification.ErrConflict)
			require.ErrorIs(t, markRecorded("job-uid", "pod-uid"), verification.ErrConflict)
			for range 2 {
				require.NoError(t, markCreated("job-uid"))
			}
			require.ErrorIs(t, markCreated("other-job"), verification.ErrConflict)
			require.ErrorIs(t, markRecorded("job-uid", "pod-uid"), verification.ErrConflict)
			require.ErrorIs(t, markObserved("other-job", "pod-uid", containerID), verification.ErrConflict)
			require.ErrorIs(t, markObserved("job-uid", "", containerID), verification.ErrBinding)
			require.ErrorIs(t, markObserved("job-uid", "pod-uid", ""), verification.ErrBinding)
			for range 2 {
				require.NoError(t, markObserved("job-uid", "pod-uid", containerID))
			}
			require.NoError(t, markCreated("job-uid"))
			require.ErrorIs(t, markObserved("job-uid", "other-pod", containerID), verification.ErrConflict)
			require.ErrorIs(t, markObserved("job-uid", "pod-uid", "other-container"), verification.ErrConflict)
			require.ErrorIs(t, markRecorded("job-uid", "pod-uid"), verification.ErrEvidence)
			require.NoError(t, storage.RecordPatchVerificationEvidence(ctx, binding, entry))
			require.ErrorIs(t, markRecorded("other-job", "pod-uid"), verification.ErrConflict)
			require.ErrorIs(t, markRecorded("job-uid", "other-pod"), verification.ErrConflict)
			for range 2 {
				require.NoError(t, markRecorded("job-uid", "pod-uid"))
			}
			require.NoError(t, storage.CreateKubernetesValidationDispatch(ctx, dispatch))
			require.NoError(t, markCreated("job-uid"))
			require.NoError(t, markObserved("job-uid", "pod-uid", containerID))
			recorded, err := storage.GetKubernetesValidationDispatch(ctx, dispatch.RunID, dispatch.Side, dispatch.CheckID)
			require.NoError(t, err)
			require.Equal(t, verification.DispatchRecorded, recorded.State)
			require.Equal(t, "job-uid", recorded.JobUID)
			require.Equal(t, "pod-uid", recorded.PodUID)
			require.Equal(t, containerID, recorded.ContainerID)
			require.ErrorIs(t, storage.MarkKubernetesValidationDispatchFailed(ctx, dispatch.RunID, dispatch.Side, dispatch.CheckID, "late failure"), verification.ErrConflict)

			require.NoError(t, storage.CompleteKubernetesValidationSubmission(ctx, submission.Namespace, submission.RequestID, ""))
			require.ErrorIs(t, storage.CreateKubernetesValidationDispatch(ctx, dispatch), verification.ErrClosed)
			require.NoError(t, markCreated("job-uid"))
			require.NoError(t, markObserved("job-uid", "pod-uid", containerID))
			require.NoError(t, markRecorded("job-uid", "pod-uid"))
			require.ErrorIs(t, markCreated("other-job"), verification.ErrConflict)
			require.ErrorIs(t, markObserved("job-uid", "other-pod", containerID), verification.ErrConflict)
			require.ErrorIs(t, markRecorded("job-uid", "other-pod"), verification.ErrConflict)
			unchanged, err := storage.GetKubernetesValidationDispatch(ctx, dispatch.RunID, dispatch.Side, dispatch.CheckID)
			require.NoError(t, err)
			require.Equal(t, recorded, unchanged)
		})
	}
}

func TestKubernetesValidationDispatchSavedBinding(t *testing.T) {
	storage := kubernetesValidationTestStore(t)
	submission, binding, evidence := kubernetesValidationFixture(t, "namespace-a", "request", true)
	createBoundKubernetesValidation(t, storage, submission, binding)
	dispatch := kubernetesValidationDispatchFixture(binding, evidence[0])
	require.NoError(t, storage.CreateKubernetesValidationDispatch(t.Context(), dispatch))
	for _, key := range []struct{ side, check string }{
		{verification.Patched, dispatch.CheckID}, {"unknown", dispatch.CheckID}, {verification.Original, "unknown"},
	} {
		require.ErrorIs(t, storage.MarkKubernetesValidationDispatchCreated(t.Context(), binding.RunID, key.side, key.check, "job-uid"), verification.ErrBinding)
		require.ErrorIs(t, storage.MarkKubernetesValidationDispatchObserved(t.Context(), binding.RunID, key.side, key.check, "job-uid", "pod-uid", "container"), verification.ErrBinding)
		require.ErrorIs(t, storage.MarkKubernetesValidationDispatchRecorded(t.Context(), binding.RunID, key.side, key.check, "job-uid", "pod-uid"), verification.ErrBinding)
	}
	require.ErrorIs(t, storage.MarkKubernetesValidationDispatchCreated(t.Context(), binding.RunID, verification.Original, submission.Manifest.Checks[1].ID, "job-uid"), verification.ErrRunNotFound)
	_, err := storage.db.ExecContext(t.Context(), `UPDATE validation_submissions SET original_task_uid='changed'
					WHERE namespace=? AND request_id=?`, submission.Namespace, submission.RequestID)
	require.NoError(t, err)
	require.ErrorIs(t, storage.CreateKubernetesValidationDispatch(t.Context(), dispatch), verification.ErrBinding)
	require.ErrorIs(t, storage.MarkKubernetesValidationDispatchCreated(t.Context(), dispatch.RunID, dispatch.Side, dispatch.CheckID, "job-uid"), verification.ErrBinding)
	require.ErrorIs(t, storage.MarkKubernetesValidationDispatchObserved(t.Context(), dispatch.RunID, dispatch.Side, dispatch.CheckID, "job-uid", "pod-uid", "container"), verification.ErrBinding)
	require.ErrorIs(t, storage.MarkKubernetesValidationDispatchRecorded(t.Context(), dispatch.RunID, dispatch.Side, dispatch.CheckID, "job-uid", "pod-uid"), verification.ErrBinding)
	unchanged, err := storage.GetKubernetesValidationDispatch(t.Context(), dispatch.RunID, dispatch.Side, dispatch.CheckID)
	require.NoError(t, err)
	require.Equal(t, dispatch, *unchanged)
}

func TestKubernetesValidationDispatchFailure(t *testing.T) {
	for _, stage := range []verification.DispatchState{
		verification.DispatchPlanned, verification.DispatchCreated, verification.DispatchObserved,
	} {
		t.Run(string(stage), func(t *testing.T) {
			storage := kubernetesValidationTestStore(t)
			submission, binding, evidence := kubernetesValidationFixture(t, "namespace-a", "request", true)
			createBoundKubernetesValidation(t, storage, submission, binding)
			dispatch := kubernetesValidationDispatchFixture(binding, evidence[0])
			require.NoError(t, storage.CreateKubernetesValidationDispatch(t.Context(), dispatch))
			if stage != verification.DispatchPlanned {
				require.NoError(t, storage.MarkKubernetesValidationDispatchCreated(t.Context(), dispatch.RunID, dispatch.Side, dispatch.CheckID, "job-uid"))
			}
			if stage == verification.DispatchObserved {
				require.NoError(t, storage.MarkKubernetesValidationDispatchObserved(t.Context(), dispatch.RunID, dispatch.Side, dispatch.CheckID, "job-uid", "pod-uid", evidence[0].Observation.ContainerID))
			}
			require.NoError(t, storage.RequestKubernetesValidationCancellation(t.Context(), submission.Namespace, submission.RequestID))
			for range 2 {
				require.NoError(t, storage.MarkKubernetesValidationDispatchFailed(t.Context(), dispatch.RunID, dispatch.Side, dispatch.CheckID, strings.Repeat("x", 257)))
			}
			require.ErrorIs(t, storage.MarkKubernetesValidationDispatchFailed(t.Context(), dispatch.RunID, dispatch.Side, dispatch.CheckID, "different failure"), verification.ErrConflict)
			failed, err := storage.GetKubernetesValidationDispatch(t.Context(), dispatch.RunID, dispatch.Side, dispatch.CheckID)
			require.NoError(t, err)
			require.Equal(t, verification.DispatchFailed, failed.State)
			require.Equal(t, strings.Repeat("x", 256), failed.Failure)
			require.ErrorIs(t, storage.MarkKubernetesValidationDispatchRecorded(t.Context(), dispatch.RunID, dispatch.Side, dispatch.CheckID, "job-uid", "pod-uid"), verification.ErrConflict)
			if stage != verification.DispatchPlanned {
				require.NoError(t, storage.MarkKubernetesValidationDispatchCreated(t.Context(), dispatch.RunID, dispatch.Side, dispatch.CheckID, "job-uid"))
			}
			if stage == verification.DispatchObserved {
				require.NoError(t, storage.MarkKubernetesValidationDispatchObserved(t.Context(), dispatch.RunID, dispatch.Side, dispatch.CheckID, "job-uid", "pod-uid", evidence[0].Observation.ContainerID))
			}
			require.NoError(t, storage.CompleteKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID, "cancelled"))
			require.NoError(t, storage.MarkKubernetesValidationDispatchFailed(t.Context(), dispatch.RunID, dispatch.Side, dispatch.CheckID, strings.Repeat("x", 257)))
			unchanged, err := storage.GetKubernetesValidationDispatch(t.Context(), dispatch.RunID, dispatch.Side, dispatch.CheckID)
			require.NoError(t, err)
			require.Equal(t, failed, unchanged)
		})
	}
}

func TestKubernetesValidationClosedSubmissionRejectsNewDispatchTransitions(t *testing.T) {
	for _, state := range []verification.SubmissionState{verification.SubmissionCancelling, verification.SubmissionTerminal} {
		t.Run(string(state), func(t *testing.T) {
			storage := kubernetesValidationTestStore(t)
			submission, binding, evidence := kubernetesValidationFixture(t, "namespace-a", "request", true)
			createBoundKubernetesValidation(t, storage, submission, binding)
			for index, entry := range evidence {
				dispatch := kubernetesValidationDispatchFixture(binding, entry)
				require.NoError(t, storage.CreateKubernetesValidationDispatch(t.Context(), dispatch))
				if index > 0 {
					require.NoError(t, storage.MarkKubernetesValidationDispatchCreated(t.Context(), dispatch.RunID, dispatch.Side, dispatch.CheckID, "job-uid"))
				}
				if index > 1 {
					require.NoError(t, storage.MarkKubernetesValidationDispatchObserved(t.Context(), dispatch.RunID, dispatch.Side, dispatch.CheckID, "job-uid", "pod-uid", entry.Observation.ContainerID))
					require.NoError(t, storage.RecordPatchVerificationEvidence(t.Context(), binding, entry))
				}
			}
			require.NoError(t, storage.RequestKubernetesValidationCancellation(t.Context(), submission.Namespace, submission.RequestID))
			if state == verification.SubmissionTerminal {
				require.NoError(t, storage.CompleteKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID, "cancelled"))
			}
			require.ErrorIs(t, storage.MarkKubernetesValidationDispatchCreated(t.Context(), binding.RunID, verification.Original, evidence[0].Observation.CheckID, "job-uid"), verification.ErrClosed)
			require.ErrorIs(t, storage.MarkKubernetesValidationDispatchObserved(t.Context(), binding.RunID, verification.Original, evidence[1].Observation.CheckID, "job-uid", "pod-uid", evidence[1].Observation.ContainerID), verification.ErrClosed)
			require.ErrorIs(t, storage.MarkKubernetesValidationDispatchRecorded(t.Context(), binding.RunID, verification.Original, evidence[2].Observation.CheckID, "job-uid", "pod-uid"), verification.ErrClosed)
			if state == verification.SubmissionTerminal {
				require.ErrorIs(t, storage.MarkKubernetesValidationDispatchFailed(t.Context(), binding.RunID, verification.Original, evidence[0].Observation.CheckID, "late failure"), verification.ErrClosed)
			}
		})
	}
}

func TestKubernetesValidationSubmissionWriteFailure(t *testing.T) {
	for _, operation := range []string{"task-uid", "bind", "cancel", "complete", "attach-cancelled"} {
		t.Run(operation, func(t *testing.T) {
			storage := kubernetesValidationTestStore(t)
			submission, binding, _ := kubernetesValidationFixture(t, "namespace-a", "request", true)
			require.NoError(t, storage.CreateKubernetesValidationSubmission(t.Context(), submission))
			if operation != "task-uid" {
				recordKubernetesValidationTaskUIDs(t, storage, submission, binding)
				require.NoError(t, storage.CreatePatchVerificationRun(t.Context(), submission.Manifest, binding, submission.Provenance))
			}
			if operation == "attach-cancelled" {
				require.NoError(t, storage.RequestKubernetesValidationCancellation(t.Context(), submission.Namespace, submission.RequestID))
				_, err := storage.CancelPatchVerificationRun(t.Context(), binding)
				require.NoError(t, err)
			}
			before, err := storage.GetKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID)
			require.NoError(t, err)
			_, err = storage.db.ExecContext(t.Context(), `CREATE TRIGGER fail_submission_update BEFORE UPDATE ON validation_submissions
							BEGIN SELECT RAISE(ABORT, 'forced submission update failure'); END`)
			require.NoError(t, err)
			switch operation {
			case "task-uid":
				err = storage.RecordKubernetesValidationTaskUID(t.Context(), submission.Namespace, submission.RequestID, verification.Original, binding.OriginalTaskID)
			case "bind":
				err = storage.BindKubernetesValidationTasks(t.Context(), submission.Namespace, submission.RequestID, binding.OriginalTaskID, binding.PatchedTaskID, binding)
			case "cancel":
				err = storage.RequestKubernetesValidationCancellation(t.Context(), submission.Namespace, submission.RequestID)
			case "complete":
				err = storage.CompleteKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID, "failed")
			case "attach-cancelled":
				err = storage.AttachCancelledKubernetesValidationRun(t.Context(), submission.Namespace, submission.RequestID, binding)
			}
			require.ErrorContains(t, err, "forced submission update failure")
			after, err := storage.GetKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID)
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}

func TestKubernetesValidationDispatchRecordedWriteFailure(t *testing.T) {
	storage := kubernetesValidationTestStore(t)
	submission, binding, evidence := kubernetesValidationFixture(t, "namespace-a", "request", true)
	createBoundKubernetesValidation(t, storage, submission, binding)
	dispatch := kubernetesValidationDispatchFixture(binding, evidence[0])
	require.NoError(t, storage.CreateKubernetesValidationDispatch(t.Context(), dispatch))
	require.NoError(t, storage.MarkKubernetesValidationDispatchCreated(t.Context(), dispatch.RunID, dispatch.Side, dispatch.CheckID, "job-uid"))
	require.NoError(t, storage.MarkKubernetesValidationDispatchObserved(t.Context(), dispatch.RunID, dispatch.Side, dispatch.CheckID, "job-uid", "pod-uid", evidence[0].Observation.ContainerID))
	require.NoError(t, storage.RecordPatchVerificationEvidence(t.Context(), binding, evidence[0]))
	_, err := storage.db.ExecContext(t.Context(), `CREATE TRIGGER fail_dispatch_update BEFORE UPDATE ON validation_dispatches
					BEGIN SELECT RAISE(ABORT, 'forced dispatch update failure'); END`)
	require.NoError(t, err)
	require.ErrorContains(t, storage.MarkKubernetesValidationDispatchRecorded(t.Context(), dispatch.RunID, dispatch.Side, dispatch.CheckID, "job-uid", "pod-uid"), "forced dispatch update failure")
	observed, err := storage.GetKubernetesValidationDispatch(t.Context(), dispatch.RunID, dispatch.Side, dispatch.CheckID)
	require.NoError(t, err)
	require.Equal(t, verification.DispatchObserved, observed.State)
	_, err = storage.db.ExecContext(t.Context(), `DROP TRIGGER fail_dispatch_update`)
	require.NoError(t, err)
	require.NoError(t, storage.MarkKubernetesValidationDispatchRecorded(t.Context(), dispatch.RunID, dispatch.Side, dispatch.CheckID, "job-uid", "pod-uid"))
}

func TestKubernetesValidationIgnoredInsertsFailClosed(t *testing.T) {
	for _, table := range []string{"validation_submissions", "validation_dispatches"} {
		t.Run(table, func(t *testing.T) {
			storage := kubernetesValidationTestStore(t)
			submission, binding, evidence := kubernetesValidationFixture(t, "namespace-a", "request", true)
			createBoundKubernetesValidation(t, storage, submission, binding)
			_, err := storage.db.ExecContext(t.Context(), `CREATE TRIGGER ignore_insert BEFORE INSERT ON `+table+`
				BEGIN SELECT RAISE(IGNORE); END`)
			require.NoError(t, err)
			if table == "validation_submissions" {
				another, _, _ := kubernetesValidationFixture(t, submission.Namespace, "another", true)
				require.Error(t, storage.CreateKubernetesValidationSubmission(t.Context(), another))
				_, err = storage.GetKubernetesValidationSubmission(t.Context(), another.Namespace, another.RequestID)
				require.ErrorIs(t, err, verification.ErrRunNotFound)
			} else {
				dispatch := kubernetesValidationDispatchFixture(binding, evidence[0])
				require.Error(t, storage.CreateKubernetesValidationDispatch(t.Context(), dispatch))
				_, err = storage.GetKubernetesValidationDispatch(t.Context(), dispatch.RunID, dispatch.Side, dispatch.CheckID)
				require.ErrorIs(t, err, verification.ErrRunNotFound)
			}
		})
	}
}

func TestKubernetesValidationCancellationFencesFinalization(t *testing.T) {
	for _, report := range []bool{false, true} {
		for _, cancelFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("report=%v/cancel-first=%v", report, cancelFirst), func(t *testing.T) {
				storage := kubernetesValidationTestStore(t)
				submission, binding, evidence := kubernetesValidationFixture(t, "namespace-a", "request", report)
				createBoundKubernetesValidation(t, storage, submission, binding)
				for _, entry := range evidence {
					require.NoError(t, storage.RecordPatchVerificationEvidence(t.Context(), binding, entry))
				}
				var finalized *verification.Record
				if !cancelFirst {
					var err error
					finalized, err = storage.FinalizePatchVerificationRun(t.Context(), binding)
					require.NoError(t, err)
					want := verification.Verified
					if report {
						want = verification.Reproduced
					}
					require.Equal(t, want, finalized.Assessment.Conclusion)
				}
				require.NoError(t, storage.RequestKubernetesValidationCancellation(t.Context(), submission.Namespace, submission.RequestID))
				cancelledSubmission, err := storage.GetKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID)
				require.NoError(t, err)
				require.Equal(t, verification.SubmissionCancelling, cancelledSubmission.State)
				afterCancellation, err := storage.GetPatchVerificationRun(t.Context(), binding.RunID)
				require.NoError(t, err)
				if cancelFirst {
					require.Equal(t, verification.RunCancelled, afterCancellation.State)
					require.Equal(t, verification.UnavailableAction(submission.Manifest.Action), afterCancellation.Assessment.Conclusion)
					require.Len(t, afterCancellation.Incidents, 1)
					require.Equal(t, "cancelled", afterCancellation.Incidents[0].Kind)
					require.Nil(t, afterCancellation.Seal)
				} else {
					require.Equal(t, finalized, afterCancellation)
				}
				settled, err := storage.FinalizePatchVerificationRun(t.Context(), binding)
				require.NoError(t, err)
				require.Equal(t, afterCancellation.State, settled.State)
				require.Equal(t, afterCancellation.Assessment, settled.Assessment)
				require.NotNil(t, settled.Seal)
				require.NoError(t, storage.RequestKubernetesValidationCancellation(t.Context(), submission.Namespace, submission.RequestID))
				replayed, err := storage.GetPatchVerificationRun(t.Context(), binding.RunID)
				require.NoError(t, err)
				require.Equal(t, settled, replayed)
				require.NoError(t, storage.CompleteKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID, ""))
				require.NoError(t, storage.RequestKubernetesValidationCancellation(t.Context(), submission.Namespace, submission.RequestID))
				replayed, err = storage.GetPatchVerificationRun(t.Context(), binding.RunID)
				require.NoError(t, err)
				require.Equal(t, settled, replayed)
			})
		}
	}
}

func TestKubernetesValidationCancellationAtomicRollback(t *testing.T) {
	for _, target := range []struct{ table, operation string }{
		{"patch_verification_incidents", "INSERT"},
		{"patch_verification_runs", "UPDATE"},
		{"validation_submissions", "UPDATE"},
	} {
		t.Run(target.table, func(t *testing.T) {
			storage := kubernetesValidationTestStore(t)
			submission, binding, _ := kubernetesValidationFixture(t, "namespace-a", "request", true)
			createBoundKubernetesValidation(t, storage, submission, binding)
			beforeSubmission, err := storage.GetKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID)
			require.NoError(t, err)
			beforeRun, err := storage.GetPatchVerificationRun(t.Context(), binding.RunID)
			require.NoError(t, err)
			_, err = storage.db.ExecContext(t.Context(), `CREATE TRIGGER fail_cancellation BEFORE `+target.operation+` ON `+target.table+`
				BEGIN SELECT RAISE(ABORT, 'forced atomic cancellation failure'); END`)
			require.NoError(t, err)
			require.ErrorContains(t, storage.RequestKubernetesValidationCancellation(t.Context(), submission.Namespace, submission.RequestID), "forced atomic cancellation failure")
			afterSubmission, err := storage.GetKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID)
			require.NoError(t, err)
			require.Equal(t, beforeSubmission, afterSubmission)
			afterRun, err := storage.GetPatchVerificationRun(t.Context(), binding.RunID)
			require.NoError(t, err)
			require.Equal(t, beforeRun, afterRun)
		})
	}
}

func TestKubernetesValidationCancellationRecoversLateBinding(t *testing.T) {
	storage := kubernetesValidationTestStore(t)
	submission, binding, evidence := kubernetesValidationFixture(t, "namespace-a", "request", true)
	require.NoError(t, storage.CreateKubernetesValidationSubmission(t.Context(), submission))
	recordKubernetesValidationTaskUIDs(t, storage, submission, binding)
	require.NoError(t, storage.CreatePatchVerificationRun(t.Context(), submission.Manifest, binding, submission.Provenance))
	require.NoError(t, storage.RequestKubernetesValidationCancellation(t.Context(), submission.Namespace, submission.RequestID))
	require.ErrorIs(t, storage.BindKubernetesValidationTasks(t.Context(), submission.Namespace, submission.RequestID,
		binding.OriginalTaskID, binding.PatchedTaskID, binding), verification.ErrClosed)
	_, err := storage.GetKubernetesValidationSubmissionByRun(t.Context(), submission.Namespace, binding.RunID)
	require.ErrorIs(t, err, verification.ErrRunNotFound)
	require.ErrorIs(t, storage.CreateKubernetesValidationDispatch(t.Context(), kubernetesValidationDispatchFixture(binding, evidence[0])), verification.ErrRunNotFound)
	persisted, err := storage.GetKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID)
	require.NoError(t, err)
	require.Equal(t, verification.SubmissionCancelling, persisted.State)
	require.Nil(t, persisted.Binding)
	require.Empty(t, persisted.RunID)
	require.ErrorIs(t, storage.AttachCancelledKubernetesValidationRun(t.Context(), submission.Namespace, submission.RequestID, binding), verification.ErrClosed)
	cancelled, err := storage.CancelPatchVerificationRun(t.Context(), binding)
	require.NoError(t, err)
	require.NoError(t, storage.AttachCancelledKubernetesValidationRun(t.Context(), submission.Namespace, submission.RequestID, binding))
	attached, err := storage.GetKubernetesValidationSubmissionByRun(t.Context(), submission.Namespace, binding.RunID)
	require.NoError(t, err)
	require.Equal(t, verification.SubmissionCancelling, attached.State)
	require.Equal(t, &binding, attached.Binding)
	require.NoError(t, storage.AttachCancelledKubernetesValidationRun(t.Context(), submission.Namespace, submission.RequestID, binding))
	replayed, err := storage.GetKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID)
	require.NoError(t, err)
	require.Equal(t, attached, replayed)
	record, err := storage.GetPatchVerificationRun(t.Context(), binding.RunID)
	require.NoError(t, err)
	require.Equal(t, cancelled, record)
	require.ErrorIs(t, storage.CreateKubernetesValidationDispatch(t.Context(), kubernetesValidationDispatchFixture(binding, evidence[0])), verification.ErrClosed)
	finalized, err := storage.FinalizePatchVerificationRun(t.Context(), binding)
	require.NoError(t, err)
	require.Equal(t, verification.RunCancelled, finalized.State)
	require.Equal(t, verification.UnableToValidate, finalized.Assessment.Conclusion)
	require.NoError(t, storage.AttachCancelledKubernetesValidationRun(t.Context(), submission.Namespace, submission.RequestID, binding))

	otherNamespace := submission
	otherNamespace.Namespace = "namespace-b"
	require.NoError(t, storage.CreateKubernetesValidationSubmission(t.Context(), otherNamespace))
	recordKubernetesValidationTaskUIDs(t, storage, otherNamespace, binding)
	require.NoError(t, storage.RequestKubernetesValidationCancellation(t.Context(), otherNamespace.Namespace, otherNamespace.RequestID))
	require.ErrorIs(t, storage.AttachCancelledKubernetesValidationRun(t.Context(), otherNamespace.Namespace, otherNamespace.RequestID, binding), verification.ErrBinding)
	_, err = storage.GetKubernetesValidationSubmissionByRun(t.Context(), otherNamespace.Namespace, binding.RunID)
	require.ErrorIs(t, err, verification.ErrRunNotFound)
	require.NoError(t, storage.CompleteKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID, "cancelled"))
	require.ErrorIs(t, storage.AttachCancelledKubernetesValidationRun(t.Context(), submission.Namespace, submission.RequestID, binding), verification.ErrClosed)
}

func TestKubernetesValidationAttachCancelledRunCoreStates(t *testing.T) {
	for _, state := range []verification.RunState{
		verification.RunRunning, verification.RunFinalized, verification.RunInvalid, verification.RunCancelled, verification.RunInterrupted,
	} {
		t.Run(string(state), func(t *testing.T) {
			storage := kubernetesValidationTestStore(t)
			submission, binding, evidence := kubernetesValidationFixture(t, "namespace-a", "request", true)
			require.NoError(t, storage.CreateKubernetesValidationSubmission(t.Context(), submission))
			recordKubernetesValidationTaskUIDs(t, storage, submission, binding)
			require.NoError(t, storage.CreatePatchVerificationRun(t.Context(), submission.Manifest, binding, submission.Provenance))
			switch state {
			case verification.RunFinalized:
				for _, entry := range evidence {
					require.NoError(t, storage.RecordPatchVerificationEvidence(t.Context(), binding, entry))
				}
				_, err := storage.FinalizePatchVerificationRun(t.Context(), binding)
				require.NoError(t, err)
			case verification.RunInvalid:
				evidence[0].Observation.CheckID = "unknown"
				require.ErrorIs(t, storage.RecordPatchVerificationEvidence(t.Context(), binding, evidence[0]), verification.ErrEvidence)
			case verification.RunCancelled:
				_, err := storage.CancelPatchVerificationRun(t.Context(), binding)
				require.NoError(t, err)
			case verification.RunInterrupted:
				_, err := storage.RecoverInterruptedPatchVerificationRun(t.Context(), binding)
				require.NoError(t, err)
			}
			require.NoError(t, storage.RequestKubernetesValidationCancellation(t.Context(), submission.Namespace, submission.RequestID))
			before, err := storage.GetKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID)
			require.NoError(t, err)
			beforeRun, err := storage.GetPatchVerificationRun(t.Context(), binding.RunID)
			require.NoError(t, err)
			require.Equal(t, state, beforeRun.State)
			err = storage.AttachCancelledKubernetesValidationRun(t.Context(), submission.Namespace, submission.RequestID, binding)
			if state == verification.RunCancelled || state == verification.RunInterrupted {
				require.NoError(t, err)
				attached, err := storage.GetKubernetesValidationSubmissionByRun(t.Context(), submission.Namespace, binding.RunID)
				require.NoError(t, err)
				require.Equal(t, verification.SubmissionCancelling, attached.State)
				require.Equal(t, &binding, attached.Binding)
			} else {
				require.ErrorIs(t, err, verification.ErrClosed)
				after, err := storage.GetKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID)
				require.NoError(t, err)
				require.Equal(t, before, after)
			}
			afterRun, err := storage.GetPatchVerificationRun(t.Context(), binding.RunID)
			require.NoError(t, err)
			require.Equal(t, beforeRun, afterRun)
		})
	}
}

func TestKubernetesValidationAttachCancelledRunSubmissionStates(t *testing.T) {
	for _, state := range []verification.SubmissionState{
		verification.SubmissionPreparing, verification.SubmissionRunning, verification.SubmissionTerminal,
	} {
		t.Run(string(state), func(t *testing.T) {
			storage := kubernetesValidationTestStore(t)
			submission, binding, _ := kubernetesValidationFixture(t, "namespace-a", "request", true)
			require.NoError(t, storage.CreateKubernetesValidationSubmission(t.Context(), submission))
			recordKubernetesValidationTaskUIDs(t, storage, submission, binding)
			require.NoError(t, storage.CreatePatchVerificationRun(t.Context(), submission.Manifest, binding, submission.Provenance))
			if state == verification.SubmissionRunning {
				require.NoError(t, storage.BindKubernetesValidationTasks(t.Context(), submission.Namespace, submission.RequestID,
					binding.OriginalTaskID, binding.PatchedTaskID, binding))
			}
			if state == verification.SubmissionTerminal {
				require.NoError(t, storage.CompleteKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID, "cancelled"))
			}
			_, err := storage.CancelPatchVerificationRun(t.Context(), binding)
			require.NoError(t, err)
			before, err := storage.GetKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID)
			require.NoError(t, err)
			require.Equal(t, state, before.State)
			require.ErrorIs(t, storage.AttachCancelledKubernetesValidationRun(t.Context(), submission.Namespace, submission.RequestID, binding), verification.ErrClosed)
			after, err := storage.GetKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID)
			require.NoError(t, err)
			require.Equal(t, before, after)
		})
	}
}

func TestKubernetesValidationAttachCancelledRunBinding(t *testing.T) {
	storage := kubernetesValidationTestStore(t)
	submission, binding, _ := kubernetesValidationFixture(t, "namespace-a", "request", false)
	require.NoError(t, storage.CreateKubernetesValidationSubmission(t.Context(), submission))
	require.NoError(t, storage.RequestKubernetesValidationCancellation(t.Context(), submission.Namespace, submission.RequestID))
	require.ErrorIs(t, storage.AttachCancelledKubernetesValidationRun(t.Context(), submission.Namespace, submission.RequestID, binding), verification.ErrBinding)
	recordKubernetesValidationTaskUIDs(t, storage, submission, binding)
	require.ErrorIs(t, storage.AttachCancelledKubernetesValidationRun(t.Context(), submission.Namespace, submission.RequestID, binding), verification.ErrRunNotFound)
	require.NoError(t, storage.CreatePatchVerificationRun(t.Context(), submission.Manifest, binding, submission.Provenance))
	_, err := storage.CancelPatchVerificationRun(t.Context(), binding)
	require.NoError(t, err)
	before, err := storage.GetKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID)
	require.NoError(t, err)
	changedManifest := submission.Manifest
	changedManifest.Problem = "different frozen problem"
	for _, candidate := range []struct {
		manifest                   verification.Manifest
		attempt, original, patched string
	}{
		{submission.Manifest, "other-attempt", binding.OriginalTaskID, binding.PatchedTaskID},
		{submission.Manifest, binding.AttemptID, "other-original", binding.PatchedTaskID},
		{submission.Manifest, binding.AttemptID, binding.OriginalTaskID, "other-patched"},
		{changedManifest, binding.AttemptID, binding.OriginalTaskID, binding.PatchedTaskID},
	} {
		wrong, err := verification.NewRunBinding(candidate.manifest, candidate.attempt, candidate.original, candidate.patched)
		require.NoError(t, err)
		require.ErrorIs(t, storage.AttachCancelledKubernetesValidationRun(t.Context(), submission.Namespace, submission.RequestID, wrong), verification.ErrBinding)
	}
	require.ErrorIs(t, storage.AttachCancelledKubernetesValidationRun(t.Context(), "namespace-b", submission.RequestID, binding), verification.ErrRunNotFound)
	require.ErrorIs(t, storage.AttachCancelledKubernetesValidationRun(t.Context(), "", submission.RequestID, binding), verification.ErrBinding)
	after, err := storage.GetKubernetesValidationSubmission(t.Context(), submission.Namespace, submission.RequestID)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.NoError(t, storage.AttachCancelledKubernetesValidationRun(t.Context(), submission.Namespace, submission.RequestID, binding))
}

func TestKubernetesValidationCancellationRepairsLegacyMarker(t *testing.T) {
	storage := kubernetesValidationTestStore(t)
	submission, binding, _ := kubernetesValidationFixture(t, "namespace-a", "request", true)
	createBoundKubernetesValidation(t, storage, submission, binding)
	_, err := storage.db.ExecContext(t.Context(), `UPDATE validation_submissions SET state=?
		WHERE namespace=? AND request_id=?`, verification.SubmissionCancelling, submission.Namespace, submission.RequestID)
	require.NoError(t, err)
	require.NoError(t, storage.RequestKubernetesValidationCancellation(t.Context(), submission.Namespace, submission.RequestID))
	record, err := storage.GetPatchVerificationRun(t.Context(), binding.RunID)
	require.NoError(t, err)
	require.Equal(t, verification.RunCancelled, record.State)
	require.Len(t, record.Incidents, 1)
	require.NoError(t, storage.RequestKubernetesValidationCancellation(t.Context(), submission.Namespace, submission.RequestID))
	replayed, err := storage.GetPatchVerificationRun(t.Context(), binding.RunID)
	require.NoError(t, err)
	require.Equal(t, record, replayed)
}

func TestKubernetesValidationClosedSubmissionBlocksDispatchPlanning(t *testing.T) {
	for _, state := range []verification.SubmissionState{verification.SubmissionCancelling, verification.SubmissionTerminal} {
		t.Run(string(state), func(t *testing.T) {
			storage := kubernetesValidationTestStore(t)
			submission, binding, evidence := kubernetesValidationFixture(t, "namespace-a", "request", true)
			createBoundKubernetesValidation(t, storage, submission, binding)
			dispatch := kubernetesValidationDispatchFixture(binding, evidence[0])
			require.NoError(t, storage.CreateKubernetesValidationDispatch(t.Context(), dispatch))
			_, err := storage.db.ExecContext(t.Context(), `UPDATE validation_submissions SET state=?
				WHERE namespace=? AND request_id=?`, state, submission.Namespace, submission.RequestID)
			require.NoError(t, err)
			record, err := storage.GetPatchVerificationRun(t.Context(), binding.RunID)
			require.NoError(t, err)
			require.Equal(t, verification.RunRunning, record.State)
			require.ErrorIs(t, storage.CreateKubernetesValidationDispatch(t.Context(), dispatch), verification.ErrClosed)
			next := kubernetesValidationDispatchFixture(binding, evidence[1])
			require.ErrorIs(t, storage.CreateKubernetesValidationDispatch(t.Context(), next), verification.ErrClosed)
			_, err = storage.GetKubernetesValidationDispatch(t.Context(), next.RunID, next.Side, next.CheckID)
			require.ErrorIs(t, err, verification.ErrRunNotFound)
		})
	}
}

func TestKubernetesValidationDatabaseFailures(t *testing.T) {
	for _, failure := range []string{"closed-database", "cancelled-context"} {
		t.Run(failure, func(t *testing.T) {
			storage := kubernetesValidationTestStore(t)
			submission, binding, evidence := kubernetesValidationFixture(t, "namespace-a", "request", true)
			createBoundKubernetesValidation(t, storage, submission, binding)
			dispatch := kubernetesValidationDispatchFixture(binding, evidence[0])
			require.NoError(t, storage.CreateKubernetesValidationDispatch(t.Context(), dispatch))
			ctx := t.Context()
			if failure == "closed-database" {
				require.NoError(t, storage.db.Close())
			} else {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			for name, operation := range map[string]func() error{
				"initialize": func() error { return storage.InitializeKubernetesValidationStore(ctx) },
				"list": func() error {
					_, err := storage.ListActiveKubernetesValidationSubmissions(ctx, submission.Namespace)
					return err
				},
				"get-by-run": func() error {
					_, err := storage.GetKubernetesValidationSubmissionByRun(ctx, submission.Namespace, binding.RunID)
					return err
				},
				"get": func() error {
					_, err := storage.GetKubernetesValidationSubmission(ctx, submission.Namespace, submission.RequestID)
					return err
				},
				"create": func() error { return storage.CreateKubernetesValidationSubmission(ctx, submission) },
				"bind-replay": func() error {
					return storage.BindKubernetesValidationTasks(ctx, submission.Namespace, submission.RequestID, binding.OriginalTaskID, binding.PatchedTaskID, binding)
				},
				"attach-cancelled": func() error {
					return storage.AttachCancelledKubernetesValidationRun(ctx, submission.Namespace, submission.RequestID, binding)
				},
				"task-uid-replay": func() error {
					return storage.RecordKubernetesValidationTaskUID(ctx, submission.Namespace, submission.RequestID, verification.Original, binding.OriginalTaskID)
				},
				"cancel": func() error {
					return storage.RequestKubernetesValidationCancellation(ctx, submission.Namespace, submission.RequestID)
				},
				"complete": func() error {
					return storage.CompleteKubernetesValidationSubmission(ctx, submission.Namespace, submission.RequestID, "")
				},
				"dispatch-create-replay": func() error {
					return storage.CreateKubernetesValidationDispatch(ctx, dispatch)
				},
				"dispatch-created": func() error {
					return storage.MarkKubernetesValidationDispatchCreated(ctx, dispatch.RunID, dispatch.Side, dispatch.CheckID, "job-uid")
				},
				"dispatch-observed": func() error {
					return storage.MarkKubernetesValidationDispatchObserved(ctx, dispatch.RunID, dispatch.Side, dispatch.CheckID, "job-uid", "pod-uid", "container")
				},
				"dispatch-recorded": func() error {
					return storage.MarkKubernetesValidationDispatchRecorded(ctx, dispatch.RunID, dispatch.Side, dispatch.CheckID, "job-uid", "pod-uid")
				},
				"dispatch-failed": func() error {
					return storage.MarkKubernetesValidationDispatchFailed(ctx, dispatch.RunID, dispatch.Side, dispatch.CheckID, "failed")
				},
			} {
				t.Run(name, func(t *testing.T) {
					err := operation()
					require.Error(t, err)
					require.NotErrorIs(t, err, verification.ErrRunNotFound)
				})
			}
		})
	}
}
