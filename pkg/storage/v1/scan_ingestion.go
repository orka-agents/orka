package v1

import (
	"context"
	"reflect"
	"time"
)

type ScanRunKey struct {
	Namespace string
	ID        string
}

type ReviewSliceKey struct {
	Namespace      string
	RepositoryScan string
	ID             string
}

type ThreatModelKey struct {
	Namespace      string
	RepositoryScan string
}

type FindingKey struct {
	Namespace string
	ID        string
}

type ReviewSliceStatusUpdate struct {
	Namespace      string
	RepositoryScan string
	ID             string
	LastScanRunID  string
	Status         string
}

type FindingDuplicate struct {
	Namespace   string
	ID          string
	CanonicalID string
}

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

	Expect *ScanIngestionReadResult
}

func (s ScanIngestionStep) IsRead() bool {
	return s.GetScanRun != nil || s.GetReviewSlice != nil || s.GetLatestThreatModel != nil ||
		s.GetFinding != nil || s.ListFindings != nil
}

type ScanIngestionReadResult struct {
	NotFound    bool
	ScanRun     *ScanRun
	ReviewSlice *ReviewSlice
	ThreatModel *ThreatModel
	Finding     *Finding
	Findings    []Finding
	NextCursor  string
}

type ScanIngestionPreview struct {
	Namespace      string
	RepositoryScan string
	At             time.Time
	Steps          []ScanIngestionStep
}

type ScanTaskIngestionCommit struct {
	Ingestion   ScanTaskIngestion
	ExpectedRun ScanRun
	Run         ScanRun
	At          time.Time
	Steps       []ScanIngestionStep
}

func ScanIngestionReadsEqual(a, b ScanIngestionReadResult) bool {
	return comparableRecordsEqual(a, b)
}

func ScanRunsEqual(a, b ScanRun) bool {
	return comparableRecordsEqual(a, b)
}

func comparableRecordsEqual(a, b any) bool {
	left, right := reflect.ValueOf(&a).Elem(), reflect.ValueOf(&b).Elem()
	return reflect.DeepEqual(normalizedRecord(left).Interface(), normalizedRecord(right).Interface())
}

var timeType = reflect.TypeFor[time.Time]()

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

type ScanIngestionStore interface {
	PreviewScanTaskIngestion(ctx context.Context, preview ScanIngestionPreview) ([]ScanIngestionReadResult, error)
	CommitScanTaskIngestion(ctx context.Context, commit ScanTaskIngestionCommit) (bool, error)
}
