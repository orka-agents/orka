package v1

import (
	"context"
	"reflect"
	"time"
)

// Scan Task ingestion applies one Task's results to its scan run as a single
// conditional command. Orka resolves the run and the receipt with ordinary
// reads, validates the live RepositoryScan identity, and computes the
// application as an ordered list of steps: the reads it made and the writes
// it wants. The provider then commits the steps, the run update and the
// receipt in one transaction, only while every read still returns what Orka
// saw. No provider transaction is open while Orka validates or computes.
//
// When a read follows an earlier write of the same records, Orka obtains its
// result from PreviewScanTaskIngestion, which runs the steps so far and the
// read, returns their results and changes nothing. Writes use the provider's
// own semantics, so a preview returns exactly what the commit will see.

// ScanRunKey names a scan run.
type ScanRunKey struct {
	Namespace string
	ID        string
}

// ReviewSliceKey names a review slice.
type ReviewSliceKey struct {
	Namespace      string
	RepositoryScan string
	ID             string
}

// ThreatModelKey names the threat model of a repository scan.
type ThreatModelKey struct {
	Namespace      string
	RepositoryScan string
}

// FindingKey names a finding.
type FindingKey struct {
	Namespace string
	ID        string
}

// ReviewSliceStatusUpdate is the input of UpdateReviewSliceStatus.
type ReviewSliceStatusUpdate struct {
	Namespace      string
	RepositoryScan string
	ID             string
	LastScanRunID  string
	Status         string
}

// FindingDuplicate is the input of MarkFindingDuplicate.
type FindingDuplicate struct {
	Namespace   string
	ID          string
	CanonicalID string
}

// ScanIngestionStep is one read or write of a scan ingestion. Exactly one
// operation field is set; each has the meaning of the SecurityStore method
// it names.
type ScanIngestionStep struct {
	GetScanRun           *ScanRunKey
	GetReviewSlice       *ReviewSliceKey
	GetLatestThreatModel *ThreatModelKey
	GetFinding           *FindingKey
	ListFindings         *FindingFilter

	UpsertReviewSlice       *ReviewSlice
	UpdateReviewSliceStatus *ReviewSliceStatusUpdate
	SaveThreatModel         *ThreatModel
	UpsertFinding           *Finding
	UpsertObservedFinding   *Finding
	MarkFindingDuplicate    *FindingDuplicate
	CreateDroppedFinding    *DroppedFinding

	// Expect is the result Orka observed for a read step. A commit requires
	// it for every read step and compares it with what the read returns
	// when the step runs; a preview ignores it.
	Expect *ScanIngestionReadResult
}

// IsRead reports whether the step reads rather than writes.
func (s ScanIngestionStep) IsRead() bool {
	return s.GetScanRun != nil || s.GetReviewSlice != nil || s.GetLatestThreatModel != nil ||
		s.GetFinding != nil || s.ListFindings != nil
}

// ScanIngestionReadResult is the result of a read step: NotFound when the
// read returned ErrNotFound, otherwise the field of the read's type. A write
// step has an empty result.
type ScanIngestionReadResult struct {
	NotFound    bool
	ScanRun     *ScanRun
	ReviewSlice *ReviewSlice
	ThreatModel *ThreatModel
	Finding     *Finding
	Findings    []Finding
	NextCursor  string
}

// ScanIngestionPreview runs steps without committing them.
type ScanIngestionPreview struct {
	// Namespace and RepositoryScan are the ingestion's; steps must stay
	// inside them as they must in a commit.
	Namespace      string
	RepositoryScan string
	// At is the base of the times the provider assigns while running steps:
	// step i uses At plus i nanoseconds wherever the provider would read its
	// clock. A preview and the commit that follows it use the same At.
	At    time.Time
	Steps []ScanIngestionStep
}

// ScanTaskIngestionCommit applies one Task's ingestion.
type ScanTaskIngestionCommit struct {
	// Ingestion is the receipt to record. IngestedAt is the receipt time.
	Ingestion ScanTaskIngestion
	// ExpectedRun is the run as Orka read it before computing the steps.
	ExpectedRun ScanRun
	// Run is the run after the application. Its identity must equal
	// ExpectedRun's.
	Run ScanRun
	// At has the meaning it has in ScanIngestionPreview.
	At    time.Time
	Steps []ScanIngestionStep
}

// ScanIngestionReadsEqual reports whether two read results are equal in the
// sense a commit compares a read with its Expect: field by field, with an
// empty list equal to an absent one and times equal when they are the same
// instant. Every persisted field counts, including fields the JSON form
// omits, such as Finding.TargetKey and Finding.DecisionAt, so a wire format
// must carry them (see "Records and wire format" in the package
// documentation). A provider that crosses a process boundary compares decoded
// results with the same rule.
func ScanIngestionReadsEqual(a, b ScanIngestionReadResult) bool {
	return comparableRecordsEqual(a, b)
}

// ScanRunsEqual reports whether two scan runs are equal in the sense a commit
// compares the stored run with ExpectedRun, including the cancellation fields
// that are not part of the JSON form.
func ScanRunsEqual(a, b ScanRun) bool {
	return comparableRecordsEqual(a, b)
}

func comparableRecordsEqual(a, b any) bool {
	left, right := reflect.ValueOf(&a).Elem(), reflect.ValueOf(&b).Elem()
	return reflect.DeepEqual(normalizedRecord(left).Interface(), normalizedRecord(right).Interface())
}

var timeType = reflect.TypeFor[time.Time]()

// normalizedRecord returns a deep copy of value with empty slices made nil and
// times reduced to their UTC instant.
func normalizedRecord(value reflect.Value) reflect.Value {
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return value
		}
		return normalizedRecord(value.Elem())
	case reflect.Pointer:
		if value.IsNil() {
			return value
		}
		copied := reflect.New(value.Type().Elem())
		copied.Elem().Set(normalizedRecord(value.Elem()))
		return copied
	case reflect.Slice:
		if value.Len() == 0 {
			return reflect.Zero(value.Type())
		}
		copied := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for i := range value.Len() {
			copied.Index(i).Set(normalizedRecord(value.Index(i)))
		}
		return copied
	case reflect.Struct:
		if value.Type() == timeType {
			instant := value.Interface().(time.Time)
			return reflect.ValueOf(instant.UTC().Round(0))
		}
		copied := reflect.New(value.Type()).Elem()
		for i := range value.NumField() {
			if copied.Field(i).CanSet() {
				copied.Field(i).Set(normalizedRecord(value.Field(i)))
			}
		}
		return copied
	default:
		return value
	}
}

// ScanIngestionStore commits scan Task ingestions.
//
//   - PreviewScanTaskIngestion runs the steps in order, as one transaction
//     that it then discards, and returns one result per step. A failing write
//     returns its error.
//   - CommitScanTaskIngestion returns false and changes nothing when the
//     Task's receipt already exists, or when the run is terminal (succeeded
//     or failed) or has a cancellation requested. ExpectedRun and Run must
//     be the ingestion's run, with the same identity, or the commit returns
//     ErrValidation; a stored run of another RepositoryScan returns
//     ErrConflict. Otherwise it returns ErrPreconditionFailed, with nothing
//     changed, unless the stored run equals ExpectedRun (ScanRunsEqual) and
//     each read step returns its Expect (ScanIngestionReadsEqual). It then
//     applies the write steps, updates the run to Run and records the
//     receipt, all in one transaction, and returns true. A failing write
//     returns its error and changes nothing. Two commits computed against the
//     same run therefore cannot both apply.
//   - Every step must name the ingestion's namespace, and steps that name a
//     repository scan must name the ingestion's RepositoryScan; otherwise
//     both methods return ErrValidation.
type ScanIngestionStore interface {
	PreviewScanTaskIngestion(ctx context.Context, preview ScanIngestionPreview) ([]ScanIngestionReadResult, error)
	CommitScanTaskIngestion(ctx context.Context, commit ScanTaskIngestionCommit) (bool, error)
}
