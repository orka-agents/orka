package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

type securityDB interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) securityDB() securityDB {
	if s.securityTx != nil {
		return s.securityTx
	}
	return s.db
}

func (s *Store) withSecurityTransaction(ctx context.Context, apply func(*Store) error) error {
	if s.securityTx != nil {
		return apply(s)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := apply(&Store{db: s.db, securityTx: tx}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetScanTaskIngestion(ctx context.Context, task store.ScanTaskIdentity) (*store.ScanTaskIngestion, error) {
	ingestion := &store.ScanTaskIngestion{ScanTaskIdentity: task}
	var findingIDs string
	err := s.securityDB().QueryRowContext(ctx, `SELECT repository_scan, stage, slice_id,
		finding_ids_json, dropped_findings_json, completed, ingested_at
		FROM security_scan_task_ingestions
		WHERE namespace = ? AND scan_run_id = ? AND task_name = ? AND task_uid = ?`,
		task.Namespace, task.ScanRunID, task.TaskName, task.TaskUID,
	).Scan(&ingestion.RepositoryScan, &ingestion.Stage, &ingestion.SliceID,
		&findingIDs, &ingestion.DroppedFindingsJSON, &ingestion.Completed, &ingestion.IngestedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if ingestion.ScanTaskIdentity != task {
		return nil, fmt.Errorf("%w: scan Task ingestion identity changed", store.ErrConflict)
	}
	if err := unmarshalSecurityJSON(findingIDs, &ingestion.FindingIDs); err != nil {
		return nil, err
	}
	return ingestion, nil
}

// securityNow is the clock of the security writes a scan ingestion step makes:
// the step's time inside a preview or commit, and the wall clock otherwise.
func (s *Store) securityNow() time.Time {
	if s.securityClock != nil {
		return s.securityClock().UTC()
	}
	return time.Now().UTC()
}

// PreviewScanTaskIngestion runs steps in a transaction it always rolls back.
func (s *Store) PreviewScanTaskIngestion(ctx context.Context, preview store.ScanIngestionPreview) ([]store.ScanIngestionReadResult, error) {
	if strings.TrimSpace(preview.Namespace) == "" || strings.TrimSpace(preview.RepositoryScan) == "" {
		return nil, store.ValidationErrorf("scan ingestion preview requires a namespace and repository scan")
	}
	if err := validateScanIngestionSteps(preview.Namespace, preview.RepositoryScan, preview.Steps, false); err != nil {
		return nil, err
	}
	if s.securityTx != nil {
		return nil, fmt.Errorf("nested scan ingestion transaction")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	return (&Store{db: s.db, securityTx: tx}).runScanIngestionSteps(ctx, preview.At, preview.Steps, false)
}

// CommitScanTaskIngestion applies an ingestion computed against ExpectedRun
// while the run and every recorded read are unchanged.
func (s *Store) CommitScanTaskIngestion(ctx context.Context, commit store.ScanTaskIngestionCommit) (bool, error) {
	ingestion := commit.Ingestion
	if ingestion.Namespace == "" || ingestion.RepositoryScan == "" || ingestion.ScanRunID == "" ||
		ingestion.TaskName == "" || ingestion.Stage == "" {
		return false, store.ValidationErrorf("scan Task ingestion requires a run and Task identity")
	}
	if !sameScanRunIdentity(commit.ExpectedRun, commit.Run) || commit.ExpectedRun.Namespace != ingestion.Namespace ||
		commit.ExpectedRun.ID != ingestion.ScanRunID || commit.ExpectedRun.RepositoryScan != ingestion.RepositoryScan {
		return false, store.ValidationErrorf("scan Task ingestion run identity does not match its Task")
	}
	if err := validateScanIngestionSteps(ingestion.Namespace, ingestion.RepositoryScan, commit.Steps, true); err != nil {
		return false, err
	}
	findingIDs, err := marshalSecurityJSON(ingestion.FindingIDs)
	if err != nil {
		return false, err
	}
	ingestedAt := ingestion.IngestedAt.UTC()
	if ingestedAt.IsZero() {
		ingestedAt = time.Now().UTC()
	}
	applied := false
	err = s.withSecurityTransaction(ctx, func(tx *Store) error {
		if _, err := tx.GetScanTaskIngestion(ctx, ingestion.ScanTaskIdentity); err == nil {
			return nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		run, err := tx.GetScanRun(ctx, ingestion.Namespace, ingestion.ScanRunID)
		if err != nil {
			return err
		}
		if run.RepositoryScan != ingestion.RepositoryScan {
			return fmt.Errorf("%w: scan Task belongs to a different RepositoryScan", store.ErrConflict)
		}
		if run.CancellationVersion != 0 || run.Phase == "succeeded" || run.Phase == "failed" {
			return nil
		}
		if !reflect.DeepEqual(*run, commit.ExpectedRun) {
			return store.PreconditionFailedErrorf("scan run %q changed after the ingestion read it", run.ID)
		}
		if _, err := tx.runScanIngestionSteps(ctx, commit.At, commit.Steps, true); err != nil {
			return err
		}
		updated := commit.Run
		if err := tx.UpdateScanRun(ctx, &updated); err != nil {
			return err
		}
		if _, err := tx.securityDB().ExecContext(ctx, `INSERT INTO security_scan_task_ingestions
			(namespace, repository_scan, scan_run_id, task_name, task_uid, stage, slice_id,
			 finding_ids_json, dropped_findings_json, completed, ingested_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			ingestion.Namespace, ingestion.RepositoryScan, ingestion.ScanRunID, ingestion.TaskName,
			ingestion.TaskUID, ingestion.Stage, ingestion.SliceID, findingIDs,
			ingestion.DroppedFindingsJSON, ingestion.Completed, ingestedAt); err != nil {
			return err
		}
		applied = true
		return nil
	})
	return applied && err == nil, err
}

func sameScanRunIdentity(a, b store.ScanRun) bool {
	return a.ID == b.ID && a.Namespace == b.Namespace && a.RepositoryScan == b.RepositoryScan &&
		a.RepositoryScanUID == b.RepositoryScanUID && a.RepositoryScanGeneration == b.RepositoryScanGeneration
}

// validateScanIngestionSteps requires one operation per step, inside the
// ingestion's namespace and, where a step names one, its repository scan. A
// commit also requires the observed result of every read.
func validateScanIngestionSteps(namespace, repositoryScan string, steps []store.ScanIngestionStep, commit bool) error {
	for i, step := range steps {
		namespaces, scans, operations := scanIngestionStepScope(step)
		if operations != 1 {
			return store.ValidationErrorf("scan ingestion step %d must name exactly one operation", i)
		}
		for _, stepNamespace := range namespaces {
			if stepNamespace != namespace {
				return store.ValidationErrorf("scan ingestion step %d is outside namespace %q", i, namespace)
			}
		}
		for _, scan := range scans {
			if scan != repositoryScan {
				return store.ValidationErrorf("scan ingestion step %d is outside repository scan %q", i, repositoryScan)
			}
		}
		if step.IsRead() && commit && step.Expect == nil {
			return store.ValidationErrorf("scan ingestion read step %d has no observed result", i)
		}
		if !step.IsRead() && step.Expect != nil {
			return store.ValidationErrorf("scan ingestion write step %d has an observed result", i)
		}
	}
	return nil
}

func scanIngestionStepScope(step store.ScanIngestionStep) (namespaces, scans []string, operations int) {
	add := func(present bool, namespace string, scan ...string) {
		if !present {
			return
		}
		operations++
		namespaces = append(namespaces, namespace)
		scans = append(scans, scan...)
	}
	add(step.GetScanRun != nil, ptrField(step.GetScanRun, func(k *store.ScanRunKey) string { return k.Namespace }))
	add(step.GetReviewSlice != nil, ptrField(step.GetReviewSlice, func(k *store.ReviewSliceKey) string { return k.Namespace }),
		ptrField(step.GetReviewSlice, func(k *store.ReviewSliceKey) string { return k.RepositoryScan }))
	add(step.GetLatestThreatModel != nil, ptrField(step.GetLatestThreatModel, func(k *store.ThreatModelKey) string { return k.Namespace }),
		ptrField(step.GetLatestThreatModel, func(k *store.ThreatModelKey) string { return k.RepositoryScan }))
	add(step.GetFinding != nil, ptrField(step.GetFinding, func(k *store.FindingKey) string { return k.Namespace }))
	add(step.ListFindings != nil, ptrField(step.ListFindings, func(f *store.FindingFilter) string { return f.Namespace }),
		ptrField(step.ListFindings, func(f *store.FindingFilter) string { return f.RepositoryScan }))
	add(step.UpsertReviewSlice != nil, ptrField(step.UpsertReviewSlice, func(r *store.ReviewSlice) string { return r.Namespace }),
		ptrField(step.UpsertReviewSlice, func(r *store.ReviewSlice) string { return r.RepositoryScan }))
	add(step.UpdateReviewSliceStatus != nil, ptrField(step.UpdateReviewSliceStatus, func(u *store.ReviewSliceStatusUpdate) string { return u.Namespace }),
		ptrField(step.UpdateReviewSliceStatus, func(u *store.ReviewSliceStatusUpdate) string { return u.RepositoryScan }))
	add(step.SaveThreatModel != nil, ptrField(step.SaveThreatModel, func(m *store.ThreatModel) string { return m.Namespace }),
		ptrField(step.SaveThreatModel, func(m *store.ThreatModel) string { return m.RepositoryScan }))
	add(step.UpsertFinding != nil, ptrField(step.UpsertFinding, func(f *store.Finding) string { return f.Namespace }),
		ptrField(step.UpsertFinding, func(f *store.Finding) string { return f.RepositoryScan }))
	add(step.UpsertObservedFinding != nil, ptrField(step.UpsertObservedFinding, func(f *store.Finding) string { return f.Namespace }),
		ptrField(step.UpsertObservedFinding, func(f *store.Finding) string { return f.RepositoryScan }))
	add(step.MarkFindingDuplicate != nil, ptrField(step.MarkFindingDuplicate, func(d *store.FindingDuplicate) string { return d.Namespace }))
	add(step.CreateDroppedFinding != nil, ptrField(step.CreateDroppedFinding, func(d *store.DroppedFinding) string { return d.Namespace }),
		ptrField(step.CreateDroppedFinding, func(d *store.DroppedFinding) string { return d.RepositoryScan }))
	return namespaces, scans, operations
}

func ptrField[T any](value *T, field func(*T) string) string {
	if value == nil {
		return ""
	}
	return field(value)
}

// runScanIngestionSteps runs steps on a transaction-scoped store, giving step
// i the time at plus i nanoseconds. With compare set, each read must return
// its Expect.
func (s *Store) runScanIngestionSteps(ctx context.Context, at time.Time, steps []store.ScanIngestionStep, compare bool) ([]store.ScanIngestionReadResult, error) {
	if s.securityTx == nil {
		return nil, fmt.Errorf("scan ingestion steps require a transaction")
	}
	defer func() { s.securityClock = nil }()
	results := make([]store.ScanIngestionReadResult, len(steps))
	for i, step := range steps {
		stepTime := at.UTC().Add(time.Duration(i))
		s.securityClock = func() time.Time { return stepTime }
		result, err := s.runScanIngestionStep(ctx, step)
		if err != nil {
			return nil, fmt.Errorf("scan ingestion step %d: %w", i, err)
		}
		if compare && step.IsRead() && !reflect.DeepEqual(result, *step.Expect) {
			return nil, store.PreconditionFailedErrorf("scan ingestion step %d read a record that changed after the ingestion read it", i)
		}
		results[i] = result
	}
	return results, nil
}

//nolint:gocyclo // One case per ingestion operation.
func (s *Store) runScanIngestionStep(ctx context.Context, step store.ScanIngestionStep) (store.ScanIngestionReadResult, error) {
	var result store.ScanIngestionReadResult
	read := func(err error) (store.ScanIngestionReadResult, error) {
		if errors.Is(err, store.ErrNotFound) {
			return store.ScanIngestionReadResult{NotFound: true}, nil
		}
		return result, err
	}
	var err error
	switch {
	case step.GetScanRun != nil:
		result.ScanRun, err = s.GetScanRun(ctx, step.GetScanRun.Namespace, step.GetScanRun.ID)
		return read(err)
	case step.GetReviewSlice != nil:
		key := step.GetReviewSlice
		result.ReviewSlice, err = s.GetReviewSlice(ctx, key.Namespace, key.RepositoryScan, key.ID)
		return read(err)
	case step.GetLatestThreatModel != nil:
		key := step.GetLatestThreatModel
		result.ThreatModel, err = s.GetLatestThreatModel(ctx, key.Namespace, key.RepositoryScan)
		return read(err)
	case step.GetFinding != nil:
		result.Finding, err = s.GetFinding(ctx, step.GetFinding.Namespace, step.GetFinding.ID)
		return read(err)
	case step.ListFindings != nil:
		result.Findings, result.NextCursor, err = s.ListFindings(ctx, *step.ListFindings)
		return read(err)
	case step.UpsertReviewSlice != nil:
		slice := *step.UpsertReviewSlice
		return result, s.UpsertReviewSlice(ctx, &slice)
	case step.UpdateReviewSliceStatus != nil:
		update := step.UpdateReviewSliceStatus
		return result, s.UpdateReviewSliceStatus(ctx, update.Namespace, update.RepositoryScan, update.ID, update.LastScanRunID, update.Status)
	case step.SaveThreatModel != nil:
		model := *step.SaveThreatModel
		return result, s.SaveThreatModel(ctx, &model)
	case step.UpsertFinding != nil:
		finding := *step.UpsertFinding
		return result, s.UpsertFinding(ctx, &finding)
	case step.UpsertObservedFinding != nil:
		finding := *step.UpsertObservedFinding
		return result, s.UpsertObservedFinding(ctx, &finding)
	case step.MarkFindingDuplicate != nil:
		duplicate := step.MarkFindingDuplicate
		return result, s.MarkFindingDuplicate(ctx, duplicate.Namespace, duplicate.ID, duplicate.CanonicalID)
	case step.CreateDroppedFinding != nil:
		dropped := *step.CreateDroppedFinding
		return result, s.CreateDroppedFinding(ctx, &dropped)
	default:
		return result, store.ValidationErrorf("scan ingestion step names no operation")
	}
}

func (s *Store) CompleteScanTaskIngestion(ctx context.Context, task store.ScanTaskIdentity) error {
	result, err := s.securityDB().ExecContext(ctx, `UPDATE security_scan_task_ingestions SET completed = TRUE
		WHERE namespace = ? AND scan_run_id = ? AND task_name = ? AND task_uid = ? AND repository_scan = ? AND stage = ? AND slice_id = ?`,
		task.Namespace, task.ScanRunID, task.TaskName, task.TaskUID, task.RepositoryScan, task.Stage, task.SliceID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return store.ErrNotFound
	}
	return nil
}
