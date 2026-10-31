package storageclient

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// ApplyScanTaskIngestion implements store.SecurityStore's ingestion over the
// contract's conditional command. It resolves the receipt and the run with
// ordinary reads, runs validate (a live Kubernetes check) and apply with no
// provider transaction open, and commits the reads and writes apply recorded
// together with the run update and the receipt. The commit applies only while
// the run and every record apply read are unchanged; otherwise it returns
// ErrPreconditionFailed and changes nothing, and the caller repeats the
// ingestion from fresh state.
func (c *Client) ApplyScanTaskIngestion(
	ctx context.Context,
	ingestion *store.ScanTaskIngestion,
	validate func(*store.ScanRun) error,
	apply func(store.SecurityStore, *store.ScanRun) error,
) (bool, error) {
	if ingestion == nil || ingestion.Namespace == "" || ingestion.RepositoryScan == "" ||
		ingestion.ScanRunID == "" || ingestion.TaskName == "" || ingestion.Stage == "" || apply == nil {
		return false, store.ValidationErrorf("scan Task ingestion requires a run, Task identity, and apply callback")
	}
	if _, err := c.GetScanTaskIngestion(ctx, ingestion.ScanTaskIdentity); err == nil {
		return false, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	run, err := c.GetScanRun(ctx, ingestion.Namespace, ingestion.ScanRunID)
	if err != nil {
		return false, err
	}
	if run.RepositoryScan != ingestion.RepositoryScan {
		return false, fmt.Errorf("%w: scan Task belongs to a different RepositoryScan", store.ErrConflict)
	}
	if run.CancellationVersion != 0 || run.Phase == "succeeded" || run.Phase == "failed" {
		return false, nil
	}
	expected := cloneScanRun(*run)
	if validate != nil {
		if err := validate(run); err != nil {
			return false, err
		}
	}
	at := time.Now().UTC()
	recorder := &scanIngestionRecorder{
		provider: c.StateProvider, namespace: ingestion.Namespace, repositoryScan: ingestion.RepositoryScan, at: at,
		dirtySlices: map[store.ReviewSliceKey]bool{},
	}
	if err := apply(recorder, run); err != nil {
		return false, err
	}
	if recorder.err != nil {
		return false, recorder.err
	}
	if validate != nil {
		if err := validate(run); err != nil {
			return false, err
		}
	}
	receipt := *ingestion
	receipt.FindingIDs = slices.Clone(ingestion.FindingIDs)
	receipt.IngestedAt = at
	applied, err := c.CommitScanTaskIngestion(ctx, store.ScanTaskIngestionCommit{
		Ingestion: receipt, ExpectedRun: expected, Run: cloneScanRun(*run), At: at, Steps: recorder.steps,
	})
	if applied {
		ingestion.IngestedAt = at
	}
	return applied, err
}

// errScanIngestionUnsupported rejects a store method an ingestion callback
// cannot use: its effect would escape the ingestion's single commit.
var errScanIngestionUnsupported = store.ValidationErrorf("security store method is not available during scan Task ingestion")

// scanIngestionRecorder is the SecurityStore an ingestion callback receives.
// It records each read with the result the callback saw, and each write, as
// steps of the ingestion's commit. Writes take effect only at the commit, so
// their errors surface at a later read or at the commit. A read of records
// that an earlier recorded write may have changed is answered by a provider
// preview of the steps so far, which applies the provider's own write
// semantics; other reads go to the provider directly. The callback receives
// copies, so mutating a returned record cannot change what the commit
// checks. A write records the finding ID default that UpsertFinding applies
// (the fingerprint) and the threat model sanitization the client applies; it
// does not write other provider-assigned fields back to the caller.
type scanIngestionRecorder struct {
	provider       storagev1.StateProvider
	namespace      string
	repositoryScan string
	at             time.Time
	steps          []store.ScanIngestionStep
	err            error

	// Which recorded writes later reads must see. Any finding write can
	// change any finding: an upsert matches by fingerprint, and duplicate
	// marking re-points descendants.
	dirtyFindings    bool
	dirtyThreatModel bool
	dirtySlices      map[store.ReviewSliceKey]bool
}

var _ store.SecurityStore = (*scanIngestionRecorder)(nil)

func (r *scanIngestionRecorder) read(
	ctx context.Context,
	step store.ScanIngestionStep,
	dirty bool,
	direct func() (store.ScanIngestionReadResult, error),
) (store.ScanIngestionReadResult, error) {
	if r.err != nil {
		return store.ScanIngestionReadResult{}, r.err
	}
	var result store.ScanIngestionReadResult
	if dirty {
		steps := append(slices.Clone(r.steps), step)
		results, err := r.provider.PreviewScanTaskIngestion(ctx, store.ScanIngestionPreview{
			Namespace: r.namespace, RepositoryScan: r.repositoryScan, At: r.at, Steps: steps,
		})
		if err == nil && len(results) != len(steps) {
			err = fmt.Errorf("scan ingestion preview returned %d results for %d steps", len(results), len(steps))
		}
		if err != nil {
			r.err = err
			return store.ScanIngestionReadResult{}, err
		}
		result = results[len(results)-1]
	} else {
		var err error
		result, err = direct()
		if errors.Is(err, store.ErrNotFound) {
			result, err = store.ScanIngestionReadResult{NotFound: true}, nil
		}
		if err != nil {
			// A read the commit cannot check must not be committed.
			r.err = err
			return store.ScanIngestionReadResult{}, err
		}
	}
	expect := cloneReadResult(result)
	step.Expect = &expect
	r.steps = append(r.steps, step)
	return cloneReadResult(result), nil
}

func (r *scanIngestionRecorder) write(step store.ScanIngestionStep) error {
	if r.err != nil {
		return r.err
	}
	r.steps = append(r.steps, step)
	return nil
}

func (r *scanIngestionRecorder) GetScanRun(ctx context.Context, namespace, id string) (*store.ScanRun, error) {
	key := store.ScanRunKey{Namespace: namespace, ID: id}
	result, err := r.read(ctx, store.ScanIngestionStep{GetScanRun: &key}, false, func() (store.ScanIngestionReadResult, error) {
		run, err := r.provider.GetScanRun(ctx, namespace, id)
		return store.ScanIngestionReadResult{ScanRun: run}, err
	})
	if err != nil {
		return nil, err
	}
	if result.NotFound || result.ScanRun == nil {
		return nil, store.ErrNotFound
	}
	return result.ScanRun, nil
}

func (r *scanIngestionRecorder) GetReviewSlice(ctx context.Context, namespace, repositoryScan, id string) (*store.ReviewSlice, error) {
	key := store.ReviewSliceKey{Namespace: namespace, RepositoryScan: repositoryScan, ID: id}
	result, err := r.read(ctx, store.ScanIngestionStep{GetReviewSlice: &key}, r.dirtySlices[key], func() (store.ScanIngestionReadResult, error) {
		slice, err := r.provider.GetReviewSlice(ctx, namespace, repositoryScan, id)
		return store.ScanIngestionReadResult{ReviewSlice: slice}, err
	})
	if err != nil {
		return nil, err
	}
	if result.NotFound || result.ReviewSlice == nil {
		return nil, store.ErrNotFound
	}
	return result.ReviewSlice, nil
}

func (r *scanIngestionRecorder) GetLatestThreatModel(ctx context.Context, namespace, repositoryScan string) (*store.ThreatModel, error) {
	key := store.ThreatModelKey{Namespace: namespace, RepositoryScan: repositoryScan}
	result, err := r.read(ctx, store.ScanIngestionStep{GetLatestThreatModel: &key}, r.dirtyThreatModel, func() (store.ScanIngestionReadResult, error) {
		model, err := r.provider.GetLatestThreatModel(ctx, namespace, repositoryScan)
		return store.ScanIngestionReadResult{ThreatModel: model}, err
	})
	if err != nil {
		return nil, err
	}
	if result.NotFound || result.ThreatModel == nil {
		return nil, store.ErrNotFound
	}
	return result.ThreatModel, nil
}

func (r *scanIngestionRecorder) GetFinding(ctx context.Context, namespace, id string) (*store.Finding, error) {
	key := store.FindingKey{Namespace: namespace, ID: id}
	result, err := r.read(ctx, store.ScanIngestionStep{GetFinding: &key}, r.dirtyFindings, func() (store.ScanIngestionReadResult, error) {
		finding, err := r.provider.GetFinding(ctx, namespace, id)
		return store.ScanIngestionReadResult{Finding: finding}, err
	})
	if err != nil {
		return nil, err
	}
	if result.NotFound || result.Finding == nil {
		return nil, store.ErrNotFound
	}
	return result.Finding, nil
}

func (r *scanIngestionRecorder) ListFindings(ctx context.Context, filter store.FindingFilter) ([]store.Finding, string, error) {
	recorded := filter
	result, err := r.read(ctx, store.ScanIngestionStep{ListFindings: &recorded}, r.dirtyFindings, func() (store.ScanIngestionReadResult, error) {
		findings, next, err := r.provider.ListFindings(ctx, filter)
		return store.ScanIngestionReadResult{Findings: findings, NextCursor: next}, err
	})
	if err != nil {
		return nil, "", err
	}
	if result.NotFound {
		return nil, "", store.ErrNotFound
	}
	return result.Findings, result.NextCursor, nil
}

func (r *scanIngestionRecorder) UpsertReviewSlice(_ context.Context, slice *store.ReviewSlice) error {
	if slice == nil {
		return store.ValidationErrorf("review slice is required")
	}
	recorded := cloneReviewSlice(*slice)
	if err := r.write(store.ScanIngestionStep{UpsertReviewSlice: &recorded}); err != nil {
		return err
	}
	r.dirtySlices[store.ReviewSliceKey{Namespace: slice.Namespace, RepositoryScan: slice.RepositoryScan, ID: slice.ID}] = true
	return nil
}

func (r *scanIngestionRecorder) UpdateReviewSliceStatus(_ context.Context, namespace, repositoryScan, id, lastScanRunID, status string) error {
	update := store.ReviewSliceStatusUpdate{
		Namespace: namespace, RepositoryScan: repositoryScan, ID: id, LastScanRunID: lastScanRunID, Status: status,
	}
	if err := r.write(store.ScanIngestionStep{UpdateReviewSliceStatus: &update}); err != nil {
		return err
	}
	r.dirtySlices[store.ReviewSliceKey{Namespace: namespace, RepositoryScan: repositoryScan, ID: id}] = true
	return nil
}

// SaveThreatModel sanitizes the threat model content, as the client does for
// any threat model write, before recording it.
func (r *scanIngestionRecorder) SaveThreatModel(_ context.Context, model *store.ThreatModel) error {
	if model == nil {
		return store.ValidationErrorf("threat model is required")
	}
	model.Content = security.SanitizeThreatModel(model.Content)
	recorded := *model
	if err := r.write(store.ScanIngestionStep{SaveThreatModel: &recorded}); err != nil {
		return err
	}
	r.dirtyThreatModel = true
	return nil
}

func (r *scanIngestionRecorder) UpsertFinding(_ context.Context, finding *store.Finding) error {
	return r.recordFinding(finding, false)
}

func (r *scanIngestionRecorder) UpsertObservedFinding(_ context.Context, finding *store.Finding) error {
	return r.recordFinding(finding, true)
}

func (r *scanIngestionRecorder) recordFinding(finding *store.Finding, observed bool) error {
	if finding == nil {
		return store.ValidationErrorf("finding is required")
	}
	if finding.ID == "" {
		finding.ID = finding.Fingerprint
	}
	recorded := cloneFinding(*finding)
	step := store.ScanIngestionStep{UpsertFinding: &recorded}
	if observed {
		step = store.ScanIngestionStep{UpsertObservedFinding: &recorded}
	}
	if err := r.write(step); err != nil {
		return err
	}
	r.dirtyFindings = true
	return nil
}

func (r *scanIngestionRecorder) MarkFindingDuplicate(_ context.Context, namespace, id, canonicalID string) error {
	if err := r.write(store.ScanIngestionStep{MarkFindingDuplicate: &store.FindingDuplicate{
		Namespace: namespace, ID: id, CanonicalID: canonicalID,
	}}); err != nil {
		return err
	}
	r.dirtyFindings = true
	return nil
}

func (r *scanIngestionRecorder) CreateDroppedFinding(_ context.Context, dropped *store.DroppedFinding) error {
	if dropped == nil {
		return store.ValidationErrorf("dropped finding is required")
	}
	recorded := *dropped
	return r.write(store.ScanIngestionStep{CreateDroppedFinding: &recorded})
}

// The remaining SecurityStore methods are not available to an ingestion
// callback. Each refuses and fails the ingestion.

func (r *scanIngestionRecorder) unsupported() error {
	if r.err == nil {
		r.err = errScanIngestionUnsupported
	}
	return errScanIngestionUnsupported
}

func (r *scanIngestionRecorder) GetScanTaskIngestion(context.Context, store.ScanTaskIdentity) (*store.ScanTaskIngestion, error) {
	return nil, r.unsupported()
}

func (r *scanIngestionRecorder) CompleteScanTaskIngestion(context.Context, store.ScanTaskIdentity) error {
	return r.unsupported()
}

func (r *scanIngestionRecorder) CreateScanRun(context.Context, *store.ScanRun) error {
	return r.unsupported()
}

func (r *scanIngestionRecorder) UpdateScanRun(context.Context, *store.ScanRun) error {
	return r.unsupported()
}

func (r *scanIngestionRecorder) ListScanRuns(context.Context, string, string, int, string) ([]store.ScanRun, string, error) {
	return nil, "", r.unsupported()
}

func (r *scanIngestionRecorder) ListActiveScanRuns(context.Context, string, string) ([]store.ScanRun, error) {
	return nil, r.unsupported()
}

func (r *scanIngestionRecorder) ListScanRunsPendingCancellation(context.Context, string, string) ([]store.ScanRun, error) {
	return nil, r.unsupported()
}

func (r *scanIngestionRecorder) RequestScanRunCancellation(context.Context, *store.ScanRun, string) error {
	return r.unsupported()
}

func (r *scanIngestionRecorder) CompleteScanRunCancellation(context.Context, *store.ScanRun) error {
	return r.unsupported()
}

func (r *scanIngestionRecorder) ListReviewSlices(context.Context, store.ReviewSliceFilter) ([]store.ReviewSlice, string, error) {
	return nil, "", r.unsupported()
}

func (r *scanIngestionRecorder) SaveThreatModelIfVersion(context.Context, *store.ThreatModel, int64) error {
	return r.unsupported()
}

func (r *scanIngestionRecorder) GetFindingCounts(context.Context, string, string) (store.FindingCounts, error) {
	return store.FindingCounts{}, r.unsupported()
}

func (r *scanIngestionRecorder) UpdateFindingState(context.Context, string, string, string) error {
	return r.unsupported()
}

func (r *scanIngestionRecorder) ResolveFindingIfCurrent(context.Context, string, string, string, int) (bool, error) {
	return false, r.unsupported()
}

func (r *scanIngestionRecorder) CreatePatchProposal(context.Context, *store.PatchProposal) error {
	return r.unsupported()
}

func (r *scanIngestionRecorder) UpdatePatchProposal(context.Context, *store.PatchProposal) error {
	return r.unsupported()
}

func (r *scanIngestionRecorder) BindPatchProposalPublicationEvidence(context.Context, *store.PatchProposal) error {
	return r.unsupported()
}

func (r *scanIngestionRecorder) ListPatchProposals(context.Context, string, string) ([]store.PatchProposal, error) {
	return nil, r.unsupported()
}

func (r *scanIngestionRecorder) ListDroppedFindings(context.Context, store.DroppedFindingFilter) ([]store.DroppedFinding, string, error) {
	return nil, "", r.unsupported()
}

func (r *scanIngestionRecorder) PreviewScanTaskIngestion(context.Context, store.ScanIngestionPreview) ([]store.ScanIngestionReadResult, error) {
	return nil, r.unsupported()
}

func (r *scanIngestionRecorder) CommitScanTaskIngestion(context.Context, store.ScanTaskIngestionCommit) (bool, error) {
	return false, r.unsupported()
}

func (r *scanIngestionRecorder) ApplyScanTaskIngestion(
	context.Context, *store.ScanTaskIngestion, func(*store.ScanRun) error, func(store.SecurityStore, *store.ScanRun) error,
) (bool, error) {
	return false, r.unsupported()
}

func cloneReadResult(result store.ScanIngestionReadResult) store.ScanIngestionReadResult {
	if result.ScanRun != nil {
		run := cloneScanRun(*result.ScanRun)
		result.ScanRun = &run
	}
	if result.ReviewSlice != nil {
		slice := cloneReviewSlice(*result.ReviewSlice)
		result.ReviewSlice = &slice
	}
	if result.ThreatModel != nil {
		model := *result.ThreatModel
		result.ThreatModel = &model
	}
	if result.Finding != nil {
		finding := cloneFinding(*result.Finding)
		result.Finding = &finding
	}
	if result.Findings != nil {
		findings := make([]store.Finding, len(result.Findings))
		for i := range result.Findings {
			findings[i] = cloneFinding(result.Findings[i])
		}
		result.Findings = findings
	}
	return result
}

func cloneScanRun(run store.ScanRun) store.ScanRun {
	if run.CompletedAt != nil {
		completed := *run.CompletedAt
		run.CompletedAt = &completed
	}
	return run
}

func cloneReviewSlice(slice store.ReviewSlice) store.ReviewSlice {
	slice.Entrypoints = slices.Clone(slice.Entrypoints)
	slice.OwnedFiles = slices.Clone(slice.OwnedFiles)
	slice.ContextFiles = slices.Clone(slice.ContextFiles)
	slice.Tests = slices.Clone(slice.Tests)
	slice.ChangedFiles = slices.Clone(slice.ChangedFiles)
	slice.ChangedLineRanges = slices.Clone(slice.ChangedLineRanges)
	slice.Tags = slices.Clone(slice.Tags)
	slice.TrustBoundaries = slices.Clone(slice.TrustBoundaries)
	if slice.LastReviewedAt != nil {
		reviewed := *slice.LastReviewedAt
		slice.LastReviewedAt = &reviewed
	}
	return slice
}

func cloneFinding(finding store.Finding) store.Finding {
	finding.Evidence = slices.Clone(finding.Evidence)
	if finding.PRNumber != nil {
		number := *finding.PRNumber
		finding.PRNumber = &number
	}
	return finding
}
