package v1

import (
	"context"
	"fmt"
	"time"
)

// ExportCapability is the capability a provider reports in
// [ProviderInfo.Capabilities] when it implements [DatasetExporter],
// [DatasetImporter] and [MigrationMarkerStore] for every contract part it
// serves. Orka migrates a dataset to another provider only between providers
// that report it.
const ExportCapability = "export.v1"

const (
	// MaxExportPageRecords is the most records one [ExportRequest] may ask
	// for.
	MaxExportPageRecords = 1000
	// MaxImportBatchRecords is the most records one [ImportRecordsRequest] may
	// carry.
	MaxImportBatchRecords = 1000
)

// ErrDatasetMigrated is returned for a write to a contract part whose records
// were migrated to another dataset (see [MigrationMarkerStore]). It matches
// [ErrNotReady]. Nothing was committed, and the dataset accepts writes for
// that contract part again only after Orka clears the marker to roll the
// migration back.
var ErrDatasetMigrated = fmt.Errorf("%w: dataset migrated to another dataset", ErrNotReady)

// DatasetSource records the dataset an imported dataset was copied from. It
// is reported in [ProviderInfo.Source] for a dataset whose origin is
// [DatasetOriginMigrated].
type DatasetSource struct {
	// DatasetID is the source dataset's ID, as its Describe reported it.
	DatasetID string
	// SchemaVersion is the source provider's schema version for the source
	// dataset. It is opaque beyond equality.
	SchemaVersion string
	// ManifestDigest is [ManifestDigest] over the families the import copied.
	ManifestDigest string
}

// Validate checks that every field is present and well formed.
func (s DatasetSource) Validate() error {
	if err := ValidateControlIdentifier("dataset source dataset ID", s.DatasetID); err != nil {
		return err
	}
	if err := ValidateControlIdentifier("dataset source schema version", s.SchemaVersion); err != nil {
		return err
	}
	return ValidateCanonicalDigest("dataset source manifest digest", s.ManifestDigest)
}

// ExportFamily describes one record family a provider exports.
type ExportFamily struct {
	// Name is the family name, as [ExportSchemas] lists it.
	Name string
	// Contract is the contract part the family belongs to, StateContract or
	// ArtifactsContract.
	Contract string
	// RecordVersion is the version of the family's canonical record encoding.
	RecordVersion int
	// Estimated is the provider's estimate of how many records the family
	// holds, for progress reporting only. It may be zero.
	Estimated int64
}

// RawRecord is one exported record in its canonical encoding.
type RawRecord struct {
	// Family is the record's family name.
	Family string
	// Key is the canonical encoding of the record's key fields (see
	// [ExportField.Key]). It identifies the record within its family,
	// so an import recognizes a record it already holds.
	Key string
	// Encoding names the family and record version of Body, as
	// [ExportFamilySchema.Encoding] returns it.
	Encoding string
	// Body is the canonical encoding of the record's fields (see
	// [ExportFamilySchema.Encode]). Sealed fields are carried as the opaque
	// bytes the provider stored.
	Body []byte
}

// ExportRequest asks for one page of a family's records.
type ExportRequest struct {
	// Family is the family to export.
	Family string
	// Cursor is the NextCursor of the previous page, or empty for the first
	// page. It is opaque and issued by the provider.
	Cursor string
	// Limit is the most records the page may hold, from 1 to
	// [MaxExportPageRecords].
	Limit int
}

// Validate checks that every field is present and well formed.
func (r ExportRequest) Validate() error {
	if _, ok := LookupExportSchema(r.Family); !ok {
		return ValidationErrorf("export family %q is not defined", r.Family)
	}
	if r.Limit < 1 || r.Limit > MaxExportPageRecords {
		return ValidationErrorf("export limit must be between 1 and %d", MaxExportPageRecords)
	}
	return nil
}

// ExportPage is one page of a family's records.
type ExportPage struct {
	// Records holds at most the requested number of records, and at least
	// one unless the family has no more. A provider may return fewer than
	// requested, for example to bound a page's size.
	Records []RawRecord
	// NextCursor continues the export, or is empty when the family has no
	// more records.
	NextCursor string
}

// FamilyDigest summarizes a family's records: how many there are and an
// order-independent digest of them (see [FamilyDigester]).
type FamilyDigest struct {
	// Family is the family name.
	Family string
	// Encoding is the family's record encoding, as
	// [ExportFamilySchema.Encoding] returns it.
	Encoding string
	// Count is the number of records.
	Count int64
	// Digest is the "sha256:<hex>" digest of the records.
	Digest string
}

// DatasetExporter reads a dataset's records in their canonical encoding, so
// they can be copied to another provider. Each method is a read
// ([MethodRead]). Behavioral methods cannot copy a dataset: they assign
// sequences, IDs and times, apply quotas and deduplication, and some records
// have no public writer.
//
// The export is a copy of what the dataset holds, field for field:
// provider-assigned values (IDs, sequences, versions and generations), every
// time to the nanosecond, domain tombstones and natural-key receipts, and
// sealed records as the opaque bytes the provider stored. It does not read a
// snapshot across pages. Orka exports while writes are stopped, and compares
// the counts and digests of both datasets before it uses the copy.
type DatasetExporter interface {
	// ExportFamilies lists every family of every contract part the provider
	// serves, in [ExportSchemas] order.
	ExportFamilies(ctx context.Context) ([]ExportFamily, error)
	// ExportRecords returns one page of a family's records, in an order the
	// provider chooses and keeps from page to page, so the pages of one
	// export hold every record once. An unknown family, a family of a
	// contract part the provider does not serve, or an invalid cursor
	// returns ErrValidation.
	ExportRecords(ctx context.Context, request ExportRequest) (ExportPage, error)
	// ExportDigest returns the count and digest of every record of a family,
	// equal to a [FamilyDigester] over the records ExportRecords returns.
	ExportDigest(ctx context.Context, family string) (FamilyDigest, error)
}

// BeginImportRequest starts copying records into an empty dataset.
type BeginImportRequest struct {
	// Contracts lists the contract parts to import, each served by the
	// provider. Every family of those parts is imported; no other family is.
	Contracts []string
	// Source identifies the dataset the records come from. Its
	// ManifestDigest must equal [ManifestDigest] over Manifest.
	Source DatasetSource
	// Manifest holds the expected [FamilyDigest] of every family of
	// Contracts, including empty families, and of no other family.
	Manifest []FamilyDigest
}

// Validate checks that every field is present and well formed and that the
// manifest covers exactly the families of Contracts.
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

// ImportSession identifies an import in progress.
type ImportSession struct {
	// SessionID identifies the import. The provider assigns it.
	SessionID string
	// DatasetID is the new dataset ID the provider minted for the imported
	// dataset.
	DatasetID string
}

// ImportRecordsRequest carries one batch of a family's records.
type ImportRecordsRequest struct {
	// SessionID identifies the import.
	SessionID string
	// Family is the family every record belongs to.
	Family string
	// Sequence is the batch's position among the family's batches, starting
	// at 1. Each batch follows the last one the family committed.
	Sequence int64
	// Records holds at most [MaxImportBatchRecords] records, each in the
	// encoding the manifest names for Family. A provider may refuse a batch
	// larger than it accepts at once with ErrTooLarge, committing nothing;
	// Orka then sends the records in smaller batches. A batch of one record
	// within the provider's [ProviderLimits] is always accepted.
	Records []RawRecord
}

// Validate checks the request's shape. It does not decode the records.
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

// ImportFamilyProgress reports how far one family's import has come.
type ImportFamilyProgress struct {
	// Family is the family name.
	Family string
	// Sequence is the last batch sequence the family committed, or 0.
	Sequence int64
	// Imported is the number of records the family holds.
	Imported int64
}

// ImportProgress reports an import's state.
type ImportProgress struct {
	// SessionID identifies the import.
	SessionID string
	// DatasetID is the imported dataset's ID.
	DatasetID string
	// State is [DatasetImporting] until the import completes, then
	// [DatasetReady].
	State DatasetState
	// Families reports every family of the imported contract parts, in
	// [ExportSchemas] order.
	Families []ImportFamilyProgress
}

// CompleteImportRequest finishes an import.
type CompleteImportRequest struct {
	// SessionID identifies the import.
	SessionID string
	// ManifestDigest repeats the manifest digest the import began with.
	ManifestDigest string
}

// Validate checks that every field is present and well formed.
func (r CompleteImportRequest) Validate() error {
	if err := ValidateControlIdentifier("complete import session ID", r.SessionID); err != nil {
		return err
	}
	return ValidateCanonicalDigest("complete import manifest digest", r.ManifestDigest)
}

// ImportVerificationError reports a CompleteImport whose stored records do
// not match the manifest. It matches [ErrConflict]. Nothing was committed;
// the dataset stays importing, so the import can continue or be aborted.
type ImportVerificationError struct {
	// Expected is the manifest entry.
	Expected FamilyDigest
	// Actual is what the dataset holds for the family.
	Actual FamilyDigest
}

func (e ImportVerificationError) Error() string {
	return fmt.Sprintf("import of family %q holds %d records with digest %s, but the manifest expects %d records with digest %s",
		e.Expected.Family, e.Actual.Count, e.Actual.Digest, e.Expected.Count, e.Expected.Digest)
}

// Is reports whether target is ErrConflict.
func (e ImportVerificationError) Is(target error) bool {
	return target == ErrConflict
}

// DatasetImporter copies exported records into an empty dataset.
// GetImportProgress is a read; every other method is administrative
// ([MethodAdmin]): it carries no write envelope and is not checked against a
// fence, because the dataset has no controller yet. Each method is atomic
// and durable when it returns, and a provider that cannot tell whether a
// change committed returns [ErrOutcomeUnknown]; repeating the identical
// request is safe.
//
// An import moves the dataset from empty, through [DatasetImporting], to
// [DatasetReady] with origin [DatasetOriginMigrated], or back to empty when
// it is aborted. While the dataset is importing, every write, the controller
// epoch advance and BindDataset return [ErrNotReady], and reads may observe a
// partial import.
type DatasetImporter interface {
	// BeginImport requires a dataset that serves every requested contract
	// part, or returns ErrValidation, and that [BindFresh] would accept
	// (origin new, no record of any kind, bootstrap not completed,
	// unclaimed) and is also ready and unmarked, or returns ErrConflict. It
	// mints a new dataset ID, records the source and manifest, and moves the
	// dataset to importing. A repeat of the request that began the current
	// import returns its session; any other request while an import is in
	// progress returns ErrConflict.
	BeginImport(ctx context.Context, request BeginImportRequest) (ImportSession, error)
	// ImportRecords commits one batch atomically and returns the family's
	// progress. An unknown session returns ErrNotFound, a completed import
	// ErrConflict, and a family the import does not cover ErrValidation.
	// Each record must decode in its family's encoding, and its Key must
	// match its key fields, or the batch returns ErrValidation. A batch after
	// the family's last committed batch is committed; a batch at or before it
	// is a repeat, and returns the progress without change if the family
	// holds every record in it with the same body. A record whose key the
	// family already holds with a different body, or a batch that skips a
	// sequence, returns ErrConflict.
	//
	// Orka imports families in [ExportSchemas] order, but references between
	// families can be circular and a family can refer to itself, so a
	// provider never refuses a record because a record it refers to has not
	// arrived yet. A provider that checks references checks them in
	// CompleteImport.
	ImportRecords(ctx context.Context, request ImportRecordsRequest) (ImportFamilyProgress, error)
	// GetImportProgress reports the import, also after it completed. An
	// unknown session returns ErrNotFound.
	GetImportProgress(ctx context.Context, sessionID string) (ImportProgress, error)
	// CompleteImport finishes the import. An unknown session returns
	// ErrNotFound, and a ManifestDigest other than the one the import began
	// with returns ErrConflict. It compares the count and digest of every
	// family with the manifest, returning an [ImportVerificationError] for
	// the first that differs. It then moves every provider-assigned sequence
	// past both its imported head and the largest imported value, records the
	// source, moves the dataset to ready with origin [DatasetOriginMigrated],
	// still unbound, and returns its description. Repeating it after it
	// succeeded returns the description again. An explicit storage switch
	// later binds the dataset with [BindMigrated].
	CompleteImport(ctx context.Context, request CompleteImportRequest) (ProviderInfo, error)
	// AbortImport discards every imported record and the session, and
	// returns the dataset to empty, unbound and ready, with origin
	// [DatasetOriginNew] and a dataset ID it has not reported before. A
	// completed import returns ErrConflict, and an unknown session, including
	// one already aborted, returns ErrNotFound.
	AbortImport(ctx context.Context, sessionID string) error
}

// MigrationMarker records that a contract part of a dataset was migrated to
// another dataset.
type MigrationMarker struct {
	// Contract is the contract part, StateContract or ArtifactsContract.
	Contract string
	// TargetDatasetID is the dataset the contract part's records were copied
	// to.
	TargetDatasetID string
	// MarkedAt is the provider clock when the marker was written.
	MarkedAt time.Time
}

// MigrationMarkerRequest marks or clears a contract part of a dataset.
type MigrationMarkerRequest struct {
	// DatasetID is the source dataset. A different dataset returns
	// ErrDatasetMismatch.
	DatasetID string
	// Contract is the contract part, served by the provider.
	Contract string
	// TargetDatasetID is the dataset the contract part was migrated to.
	TargetDatasetID string
}

// Validate checks that every field is present and well formed.
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

// MigrationMarkerStore marks the contract parts of a source dataset that were
// migrated, so the source cannot become the active writer for them again
// unnoticed. Each method is administrative ([MethodAdmin]), atomic, and
// durable when it returns; a provider that cannot tell whether a change
// committed returns [ErrOutcomeUnknown].
//
// A contract part holds at most one marker. While it is marked, every write
// method of that contract part ([MethodWrite], with or without a
// [WriteEnvelope]) returns [ErrDatasetMigrated]; reads, exports, the replay
// of a committed operation and the other contract part are unaffected.
// Describe reports the markers in [ProviderInfo.MigratedTo].
type MigrationMarkerStore interface {
	// MarkDatasetMigrated writes the marker and returns it. A repeat for the
	// same target returns the stored marker; a contract part marked for
	// another target returns ErrConflict.
	MarkDatasetMigrated(ctx context.Context, request MigrationMarkerRequest) (MigrationMarker, error)
	// ClearMigrationMarker removes the marker, which only an explicit
	// rollback of the migration does. A contract part without a marker
	// succeeds; one marked for another target returns ErrConflict.
	ClearMigrationMarker(ctx context.Context, request MigrationMarkerRequest) error
}
