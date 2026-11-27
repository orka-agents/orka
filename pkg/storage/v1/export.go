package v1

import (
	"context"
	"fmt"
	"time"
)

const ExportCapability = "export.v1"

const (
	MaxExportPageRecords  = 1000
	MaxImportBatchRecords = 1000
)

var ErrDatasetMigrated = fmt.Errorf("%w: dataset migrated to another dataset", ErrNotReady)

type DatasetSource struct {
	DatasetID      string
	SchemaVersion  string
	ManifestDigest string
}

func (s DatasetSource) Validate() error {
	if err := ValidateControlIdentifier("dataset source dataset ID", s.DatasetID); err != nil {
		return err
	}
	if err := ValidateControlIdentifier("dataset source schema version", s.SchemaVersion); err != nil {
		return err
	}
	return ValidateCanonicalDigest("dataset source manifest digest", s.ManifestDigest)
}

type ExportFamily struct {
	Name          string
	Contract      string
	RecordVersion int
	Estimated     int64
}

type RawRecord struct {
	Family   string
	Key      string
	Encoding string
	Body     []byte
}

type ExportRequest struct {
	Family string
	Cursor string
	Limit  int
}

func (r ExportRequest) Validate() error {
	if _, ok := LookupExportSchema(r.Family); !ok {
		return ValidationErrorf("export family %q is not defined", r.Family)
	}
	if r.Limit < 1 || r.Limit > MaxExportPageRecords {
		return ValidationErrorf("export limit must be between 1 and %d", MaxExportPageRecords)
	}
	return nil
}

type ExportPage struct {
	Records    []RawRecord
	NextCursor string
}

type FamilyDigest struct {
	Family   string
	Encoding string
	Count    int64
	Digest   string
}

type DatasetExporter interface {
	ExportFamilies(ctx context.Context) ([]ExportFamily, error)
	ExportRecords(ctx context.Context, request ExportRequest) (ExportPage, error)
	ExportDigest(ctx context.Context, family string) (FamilyDigest, error)
}

type BeginImportRequest struct {
	Contracts []string
	Source    DatasetSource
	Manifest  []FamilyDigest
}

func (r BeginImportRequest) Validate() error {
	if len(r.Contracts) == 0 {
		return ValidationErrorf("begin import requires at least one contract part")
	}
	contracts := map[string]bool{}
	for _, contract := range r.Contracts {
		if contract != StateContract && contract != ArtifactsContract {
			return ValidationErrorf("begin import contract part %q is not defined", contract)
		}
		if contracts[contract] {
			return ValidationErrorf("begin import names contract part %q twice", contract)
		}
		contracts[contract] = true
	}
	if err := r.Source.Validate(); err != nil {
		return err
	}
	digest, err := ManifestDigest(r.Manifest)
	if err != nil {
		return err
	}
	if digest != r.Source.ManifestDigest {
		return ValidationErrorf("begin import source manifest digest does not match the manifest")
	}
	listed := map[string]bool{}
	for _, family := range r.Manifest {
		schema, _ := LookupExportSchema(family.Family)
		if !contracts[schema.Contract] {
			return ValidationErrorf("begin import manifest family %q is not part of the imported contract parts", family.Family)
		}
		listed[family.Family] = true
	}
	for _, schema := range exportSchemas {
		if contracts[schema.Contract] && !listed[schema.Name] {
			return ValidationErrorf("begin import manifest is missing family %q", schema.Name)
		}
	}
	return nil
}

type ImportSession struct {
	SessionID string
	DatasetID string
}

type ImportRecordsRequest struct {
	SessionID string
	Family    string
	Sequence  int64
	Records   []RawRecord
}

func (r ImportRecordsRequest) Validate() error {
	if err := ValidateControlIdentifier("import records session ID", r.SessionID); err != nil {
		return err
	}
	if _, ok := LookupExportSchema(r.Family); !ok {
		return ValidationErrorf("import records family %q is not defined", r.Family)
	}
	if r.Sequence < 1 {
		return ValidationErrorf("import records batch sequence must be at least 1")
	}
	if len(r.Records) > MaxImportBatchRecords {
		return ValidationErrorf("import records batch holds more than %d records", MaxImportBatchRecords)
	}
	for _, record := range r.Records {
		if record.Family != r.Family {
			return ValidationErrorf("import records batch for family %q holds a record of family %q", r.Family, record.Family)
		}
	}
	return nil
}

type ImportFamilyProgress struct {
	Family   string
	Sequence int64
	Imported int64
}

type ImportProgress struct {
	SessionID string
	DatasetID string
	State     DatasetState
	Families  []ImportFamilyProgress
}

type CompleteImportRequest struct {
	SessionID      string
	ManifestDigest string
}

func (r CompleteImportRequest) Validate() error {
	if err := ValidateControlIdentifier("complete import session ID", r.SessionID); err != nil {
		return err
	}
	return ValidateCanonicalDigest("complete import manifest digest", r.ManifestDigest)
}

type ImportVerificationError struct {
	Expected FamilyDigest
	Actual   FamilyDigest
}

func (e ImportVerificationError) Error() string {
	return fmt.Sprintf("import of family %q holds %d records with digest %s, but the manifest expects %d records with digest %s",
		e.Expected.Family, e.Actual.Count, e.Actual.Digest, e.Expected.Count, e.Expected.Digest)
}

func (e ImportVerificationError) Is(target error) bool {
	return target == ErrConflict
}

type DatasetImporter interface {
	BeginImport(ctx context.Context, request BeginImportRequest) (ImportSession, error)
	ImportRecords(ctx context.Context, request ImportRecordsRequest) (ImportFamilyProgress, error)
	GetImportProgress(ctx context.Context, sessionID string) (ImportProgress, error)
	CompleteImport(ctx context.Context, request CompleteImportRequest) (ProviderInfo, error)
	AbortImport(ctx context.Context, sessionID string) error
}

type MigrationMarker struct {
	Contract        string
	TargetDatasetID string
	MarkedAt        time.Time
}

type MigrationMarkerRequest struct {
	DatasetID       string
	Contract        string
	TargetDatasetID string
}

func (r MigrationMarkerRequest) Validate() error {
	if err := ValidateControlIdentifier("migration marker dataset ID", r.DatasetID); err != nil {
		return err
	}
	if r.Contract != StateContract && r.Contract != ArtifactsContract {
		return ValidationErrorf("migration marker contract part %q is not defined", r.Contract)
	}
	if err := ValidateControlIdentifier("migration marker target dataset ID", r.TargetDatasetID); err != nil {
		return err
	}
	if r.TargetDatasetID == r.DatasetID {
		return ValidationErrorf("migration marker target dataset must differ from the source dataset")
	}
	return nil
}

type MigrationMarkerStore interface {
	MarkDatasetMigrated(ctx context.Context, request MigrationMarkerRequest) (MigrationMarker, error)
	ClearMigrationMarker(ctx context.Context, request MigrationMarkerRequest) error
}
