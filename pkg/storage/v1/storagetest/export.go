package storagetest

import (
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// ExportProvider is the part of a provider the export and import cases use.
type ExportProvider interface {
	storagev1.ProviderDescriber
	storagev1.DatasetExporter
	storagev1.DatasetImporter
	storagev1.MigrationMarkerStore
}

// ExportFactory returns an ExportProvider over a fresh, empty dataset, in the
// same way as StateFactory.
type ExportFactory func(t *testing.T) ExportProvider

// ExportSeed fills an empty dataset with records to export, such as through
// the provider's own methods.
type ExportSeed func(t *testing.T, provider ExportProvider)

// exportPageLimit keeps pages small, so every export spans several pages.
const exportPageLimit = 2

// importBatchRecords keeps batches small, so every import spans several
// batches.
const importBatchRecords = 3

// RunExportImport runs the export and import cases against providers from
// newProvider, copying a dataset that seed fills. RunState and RunArtifacts
// run it for a provider that reports [storagev1.ExportCapability], and then
// check through state.v1 that the copy continues message IDs, transcript
// order and execution event sequences past values deleted before the
// export. A provider can also run it with a seed of its own, for example one
// that reaches the sequences the shared seeds cannot observe through the
// contract, such as usage accounting order.
func RunExportImport(t *testing.T, newProvider ExportFactory, seed ExportSeed) {
	t.Helper()
	t.Run("families", func(t *testing.T) {
		provider := newProvider(t)
		info := describe(t, provider)
		require.Contains(t, info.Capabilities, storagev1.ExportCapability)
		families, err := provider.ExportFamilies(t.Context())
		require.NoError(t, err)
		var want []string
		for _, schema := range storagev1.ExportSchemas() {
			if slices.Contains(info.Contracts, schema.Contract) {
				want = append(want, schema.Name)
			}
		}
		names := make([]string, 0, len(families))
		for _, family := range families {
			schema, ok := storagev1.LookupExportSchema(family.Name)
			require.True(t, ok, family.Name)
			require.Equal(t, schema.Contract, family.Contract, family.Name)
			require.Equal(t, schema.Version, family.RecordVersion, family.Name)
			require.GreaterOrEqual(t, family.Estimated, int64(0), family.Name)
			names = append(names, family.Name)
		}
		require.Equal(t, want, names, "every family of the served contract parts, in import order")
	})

	t.Run("an export reads every record once in its canonical encoding", func(t *testing.T) {
		source := seededSource(t, newProvider, seed)
		manifest, _ := exportDataset(t, source)
		require.Positive(t, totalRecords(manifest), "the seed wrote records")
		_, err := source.ExportRecords(t.Context(), storagev1.ExportRequest{Family: "no-such-family", Limit: 1})
		require.ErrorIs(t, err, storagev1.ErrValidation)
		family := manifest[0].Family
		_, err = source.ExportRecords(t.Context(), storagev1.ExportRequest{Family: family, Limit: 0})
		require.ErrorIs(t, err, storagev1.ErrValidation)
		_, err = source.ExportRecords(t.Context(), storagev1.ExportRequest{Family: family, Cursor: "not a provider cursor", Limit: 1})
		require.ErrorIs(t, err, storagev1.ErrValidation)
		_, err = source.ExportDigest(t.Context(), "no-such-family")
		require.ErrorIs(t, err, storagev1.ErrValidation)
	})

	t.Run("an import copies the dataset under a new identity", func(t *testing.T) {
		source := seededSource(t, newProvider, seed)
		sourceInfo := describe(t, source)
		manifest, records := exportDataset(t, source)
		target := newProvider(t)
		empty := describe(t, target)
		request := beginImportRequest(t, sourceInfo, manifest)
		session, err := target.BeginImport(t.Context(), request)
		require.NoError(t, err)
		require.NotEmpty(t, session.SessionID)
		require.NotEqual(t, sourceInfo.DatasetID, session.DatasetID, "an imported dataset never reuses the source's identity")
		require.NotEqual(t, empty.DatasetID, session.DatasetID, "an import mints a new dataset ID")
		importing := describe(t, target)
		require.Equal(t, session.DatasetID, importing.DatasetID)
		require.Equal(t, storagev1.DatasetImporting, importing.State)
		repeated, err := target.BeginImport(t.Context(), request)
		require.NoError(t, err, "a repeated begin returns the session")
		require.Equal(t, session, repeated)

		importDataset(t, target, session.SessionID, manifest, records, true)
		progress, err := target.GetImportProgress(t.Context(), session.SessionID)
		require.NoError(t, err)
		require.Equal(t, storagev1.DatasetImporting, progress.State)
		requireProgressMatches(t, manifest, progress)

		completed, err := target.CompleteImport(t.Context(), storagev1.CompleteImportRequest{
			SessionID: session.SessionID, ManifestDigest: request.Source.ManifestDigest,
		})
		require.NoError(t, err)
		require.Equal(t, session.DatasetID, completed.DatasetID)
		require.Equal(t, storagev1.DatasetOriginMigrated, completed.DatasetOrigin)
		require.Equal(t, storagev1.DatasetReady, completed.State)
		require.Equal(t, &request.Source, completed.Source)
		require.Nil(t, completed.Binding, "an imported dataset is unbound until a storage switch binds it")
		require.Equal(t, completed.DatasetID, describe(t, target).DatasetID)
		again, err := target.CompleteImport(t.Context(), storagev1.CompleteImportRequest{
			SessionID: session.SessionID, ManifestDigest: request.Source.ManifestDigest,
		})
		require.NoError(t, err, "a repeated completion returns the description")
		require.Equal(t, completed.DatasetID, again.DatasetID)
		progress, err = target.GetImportProgress(t.Context(), session.SessionID)
		require.NoError(t, err)
		require.Equal(t, storagev1.DatasetReady, progress.State)
		require.ErrorIs(t, target.AbortImport(t.Context(), session.SessionID), storagev1.ErrConflict,
			"a completed import cannot be aborted")

		copied, copiedRecords := exportDataset(t, target)
		require.Equal(t, manifest, copied, "the copy has the source's counts and digests")
		require.Equal(t, records, copiedRecords, "the copy holds the source's records byte for byte")
	})

	t.Run("begin import refuses a dataset that is not empty or a second import", func(t *testing.T) {
		source := seededSource(t, newProvider, seed)
		sourceInfo := describe(t, source)
		manifest, _ := exportDataset(t, source)
		request := beginImportRequest(t, sourceInfo, manifest)
		_, err := source.BeginImport(t.Context(), request)
		require.ErrorIs(t, err, storagev1.ErrConflict, "a dataset that holds records")

		invalid := request
		invalid.Source.ManifestDigest = controlDigest("another manifest")
		target := newProvider(t)
		_, err = target.BeginImport(t.Context(), invalid)
		require.ErrorIs(t, err, storagev1.ErrValidation)
		invalid = beginImportRequest(t, sourceInfo, manifest[1:])
		_, err = target.BeginImport(t.Context(), invalid)
		require.ErrorIs(t, err, storagev1.ErrValidation, "a manifest that misses a family")

		_, err = target.BeginImport(t.Context(), request)
		require.NoError(t, err)
		other := request
		other.Source.DatasetID = "another-source-dataset"
		_, err = target.BeginImport(t.Context(), other)
		require.ErrorIs(t, err, storagev1.ErrConflict, "another import while one is in progress")
	})

	t.Run("an import refuses records it cannot store unchanged", func(t *testing.T) {
		source := seededSource(t, newProvider, seed)
		manifest, records := exportDataset(t, source)
		target := newProvider(t)
		session, err := target.BeginImport(t.Context(), beginImportRequest(t, describe(t, source), manifest))
		require.NoError(t, err)
		family, record := firstRecord(t, manifest, records)
		batch := func(sequence int64, records ...storagev1.RawRecord) storagev1.ImportRecordsRequest {
			return storagev1.ImportRecordsRequest{SessionID: session.SessionID, Family: family, Sequence: sequence, Records: records}
		}
		requireRefused := func(err error, want error, msgAndArgs ...any) {
			t.Helper()
			require.ErrorIs(t, err, want, msgAndArgs...)
			progress, err := target.GetImportProgress(t.Context(), session.SessionID)
			require.NoError(t, err)
			for _, family := range progress.Families {
				require.Zero(t, family.Imported, "a refused batch commits nothing")
			}
		}
		_, err = target.ImportRecords(t.Context(), storagev1.ImportRecordsRequest{
			SessionID: "no-such-session", Family: family, Sequence: 1, Records: []storagev1.RawRecord{record},
		})
		require.ErrorIs(t, err, storagev1.ErrNotFound)

		wrongKey := record
		wrongKey.Key += "A"
		_, err = target.ImportRecords(t.Context(), batch(1, wrongKey))
		requireRefused(err, storagev1.ErrValidation, "a key that does not match the record")
		trailing := record
		trailing.Body = append(slices.Clone(record.Body), 0)
		_, err = target.ImportRecords(t.Context(), batch(1, trailing))
		requireRefused(err, storagev1.ErrValidation, "a body that is not canonical")
		wrongEncoding := record
		wrongEncoding.Encoding += "0"
		_, err = target.ImportRecords(t.Context(), batch(1, wrongEncoding))
		requireRefused(err, storagev1.ErrValidation, "an encoding other than the manifest's")
		_, err = target.ImportRecords(t.Context(), batch(2, record))
		requireRefused(err, storagev1.ErrConflict, "a batch that skips a sequence")

		progress, err := target.ImportRecords(t.Context(), batch(1, record))
		require.NoError(t, err)
		require.Equal(t, storagev1.ImportFamilyProgress{Family: family, Sequence: 1, Imported: 1}, progress)
		replay, err := target.ImportRecords(t.Context(), batch(1, record))
		require.NoError(t, err, "a repeated batch")
		require.Equal(t, progress, replay)
		_, err = target.ImportRecords(t.Context(), batch(2, changedRecord(t, record)))
		require.ErrorIs(t, err, storagev1.ErrConflict, "a record whose key is held with another body")
		_, err = target.ImportRecords(t.Context(), batch(1, changedRecord(t, record)))
		require.ErrorIs(t, err, storagev1.ErrConflict, "a repeated batch whose record changed")
		_, err = target.ImportRecords(t.Context(), batch(2, record))
		require.NoError(t, err, "a later batch may repeat a record the family holds")
		after, err := target.GetImportProgress(t.Context(), session.SessionID)
		require.NoError(t, err)
		require.Equal(t, int64(1), importedCount(after, family))
	})

	t.Run("completion compares the import with the manifest", func(t *testing.T) {
		source := seededSource(t, newProvider, seed)
		manifest, records := exportDataset(t, source)
		target := newProvider(t)
		request := beginImportRequest(t, describe(t, source), manifest)
		session, err := target.BeginImport(t.Context(), request)
		require.NoError(t, err)
		family, record := firstRecord(t, manifest, records)
		partial := map[string][]storagev1.RawRecord{}
		maps.Copy(partial, records)
		partial[family] = slices.DeleteFunc(slices.Clone(records[family]), func(r storagev1.RawRecord) bool { return r.Key == record.Key })
		next := importDataset(t, target, session.SessionID, manifest, partial, false)

		complete := storagev1.CompleteImportRequest{SessionID: session.SessionID, ManifestDigest: request.Source.ManifestDigest}
		_, err = target.CompleteImport(t.Context(), complete)
		var verification storagev1.ImportVerificationError
		require.ErrorAs(t, err, &verification)
		require.ErrorIs(t, err, storagev1.ErrConflict)
		require.Equal(t, family, verification.Expected.Family)
		require.Equal(t, verification.Expected.Count-1, verification.Actual.Count)
		require.Equal(t, storagev1.DatasetImporting, describe(t, target).State, "a refused completion leaves the import in progress")

		wrong := complete
		wrong.ManifestDigest = controlDigest("another manifest")
		_, err = target.CompleteImport(t.Context(), wrong)
		require.ErrorIs(t, err, storagev1.ErrConflict, "a manifest other than the one the import began with")
		_, err = target.CompleteImport(t.Context(), storagev1.CompleteImportRequest{
			SessionID: "no-such-session", ManifestDigest: request.Source.ManifestDigest,
		})
		require.ErrorIs(t, err, storagev1.ErrNotFound)

		_, err = target.ImportRecords(t.Context(), storagev1.ImportRecordsRequest{
			SessionID: session.SessionID, Family: family, Sequence: next[family], Records: []storagev1.RawRecord{record},
		})
		require.NoError(t, err)
		_, err = target.CompleteImport(t.Context(), complete)
		require.NoError(t, err)
	})

	t.Run("an aborted import leaves an empty dataset", func(t *testing.T) {
		source := seededSource(t, newProvider, seed)
		manifest, records := exportDataset(t, source)
		target := newProvider(t)
		empty := describe(t, target)
		request := beginImportRequest(t, describe(t, source), manifest)
		session, err := target.BeginImport(t.Context(), request)
		require.NoError(t, err)
		importDataset(t, target, session.SessionID, manifest, records, false)
		require.NoError(t, target.AbortImport(t.Context(), session.SessionID))

		aborted := describe(t, target)
		require.Equal(t, storagev1.DatasetOriginNew, aborted.DatasetOrigin)
		require.Equal(t, storagev1.DatasetReady, aborted.State)
		require.Nil(t, aborted.Source)
		require.Nil(t, aborted.Binding)
		require.NotEqual(t, session.DatasetID, aborted.DatasetID, "an aborted import's dataset ID is not reused")
		require.NotEqual(t, empty.DatasetID, aborted.DatasetID)
		copied, _ := exportDataset(t, target)
		require.Zero(t, totalRecords(copied), "the aborted import left no record")
		_, err = target.GetImportProgress(t.Context(), session.SessionID)
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		require.ErrorIs(t, target.AbortImport(t.Context(), session.SessionID), storagev1.ErrNotFound)

		restarted, err := target.BeginImport(t.Context(), request)
		require.NoError(t, err, "an import can begin again")
		require.NotEqual(t, session.SessionID, restarted.SessionID)
	})

	t.Run("a migration marker is written once per contract part", func(t *testing.T) {
		source := seededSource(t, newProvider, seed)
		info := describe(t, source)
		require.Empty(t, info.MigratedTo)
		contract := info.Contracts[0]
		request := storagev1.MigrationMarkerRequest{DatasetID: info.DatasetID, Contract: contract, TargetDatasetID: "target-dataset"}
		marker, err := source.MarkDatasetMigrated(t.Context(), request)
		require.NoError(t, err)
		require.Equal(t, contract, marker.Contract)
		require.Equal(t, "target-dataset", marker.TargetDatasetID)
		require.False(t, marker.MarkedAt.IsZero())
		repeated, err := source.MarkDatasetMigrated(t.Context(), request)
		require.NoError(t, err)
		require.Equal(t, marker, repeated, "a repeat returns the stored marker")
		require.Equal(t, []storagev1.MigrationMarker{marker}, describe(t, source).MigratedTo)

		other := request
		other.TargetDatasetID = "another-target-dataset"
		_, err = source.MarkDatasetMigrated(t.Context(), other)
		require.ErrorIs(t, err, storagev1.ErrConflict)
		require.ErrorIs(t, source.ClearMigrationMarker(t.Context(), other), storagev1.ErrConflict)
		mismatched := request
		mismatched.DatasetID = "another-dataset"
		_, err = source.MarkDatasetMigrated(t.Context(), mismatched)
		require.ErrorIs(t, err, storagev1.ErrDatasetMismatch)
		invalid := request
		invalid.TargetDatasetID = info.DatasetID
		_, err = source.MarkDatasetMigrated(t.Context(), invalid)
		require.ErrorIs(t, err, storagev1.ErrValidation)

		exported, _ := exportDataset(t, source)
		require.Positive(t, totalRecords(exported), "a marked dataset still exports")
		require.NoError(t, source.ClearMigrationMarker(t.Context(), request))
		require.Empty(t, describe(t, source).MigratedTo)
		require.NoError(t, source.ClearMigrationMarker(t.Context(), request), "clearing an unmarked contract part succeeds")
	})
}

func describe(t *testing.T, provider storagev1.ProviderDescriber) storagev1.ProviderInfo {
	t.Helper()
	info, err := provider.Describe(t.Context())
	require.NoError(t, err)
	return info
}

func seededSource(t *testing.T, newProvider ExportFactory, seed ExportSeed) ExportProvider {
	t.Helper()
	source := newProvider(t)
	seed(t, source)
	return source
}

// exportDataset exports every family of provider page by page, checks each
// record and the provider's digests, and returns the manifest and the
// records by family, sorted by key.
func exportDataset(t *testing.T, provider ExportProvider) ([]storagev1.FamilyDigest, map[string][]storagev1.RawRecord) {
	t.Helper()
	families, err := provider.ExportFamilies(t.Context())
	require.NoError(t, err)
	manifest := make([]storagev1.FamilyDigest, 0, len(families))
	records := map[string][]storagev1.RawRecord{}
	for _, family := range families {
		schema, ok := storagev1.LookupExportSchema(family.Name)
		require.True(t, ok, family.Name)
		digester, err := storagev1.NewFamilyDigester(family.Name)
		require.NoError(t, err)
		keys := map[string]bool{}
		cursor := ""
		for {
			page, err := provider.ExportRecords(t.Context(), storagev1.ExportRequest{Family: family.Name, Cursor: cursor, Limit: exportPageLimit})
			require.NoError(t, err)
			require.LessOrEqual(t, len(page.Records), exportPageLimit)
			for _, record := range page.Records {
				_, err := schema.Decode(record)
				require.NoError(t, err, "family %s exports canonical records", family.Name)
				require.False(t, keys[record.Key], "family %s exports a record twice", family.Name)
				keys[record.Key] = true
				require.NoError(t, digester.Add(record))
				records[family.Name] = append(records[family.Name], record)
			}
			if page.NextCursor == "" {
				break
			}
			require.NotEmpty(t, page.Records, "family %s returned an empty page before the end", family.Name)
			cursor = page.NextCursor
		}
		digest := digester.Sum()
		reported, err := provider.ExportDigest(t.Context(), family.Name)
		require.NoError(t, err)
		require.Equal(t, digest, reported, "family %s reports the digest of the records it exports", family.Name)
		manifest = append(manifest, digest)
		slices.SortFunc(records[family.Name], func(a, b storagev1.RawRecord) int { return strings.Compare(a.Key, b.Key) })
	}
	return manifest, records
}

func totalRecords(manifest []storagev1.FamilyDigest) int64 {
	var total int64
	for _, family := range manifest {
		total += family.Count
	}
	return total
}

func beginImportRequest(t *testing.T, source storagev1.ProviderInfo, manifest []storagev1.FamilyDigest) storagev1.BeginImportRequest {
	t.Helper()
	digest, err := storagev1.ManifestDigest(manifest)
	require.NoError(t, err)
	return storagev1.BeginImportRequest{
		Contracts: slices.Clone(source.Contracts),
		Source:    storagev1.DatasetSource{DatasetID: source.DatasetID, SchemaVersion: source.SchemaVersion, ManifestDigest: digest},
		Manifest:  manifest,
	}
}

// importDataset imports records family by family, in manifest order, in
// small batches. A batch the provider refuses with ErrTooLarge is sent again
// in halves under the same sequence, as the contract permits. With replay
// set, it repeats each family's first batch after committing it. It returns
// each family's next batch sequence.
func importDataset(
	t *testing.T, target ExportProvider, sessionID string,
	manifest []storagev1.FamilyDigest, records map[string][]storagev1.RawRecord, replay bool,
) map[string]int64 {
	t.Helper()
	next := map[string]int64{}
	for _, family := range manifest {
		sequence := int64(1)
		var imported int64
		pending := slices.Collect(slices.Chunk(records[family.Family], importBatchRecords))
		for len(pending) > 0 {
			chunk := pending[0]
			request := storagev1.ImportRecordsRequest{SessionID: sessionID, Family: family.Family, Sequence: sequence, Records: chunk}
			progress, err := target.ImportRecords(t.Context(), request)
			if errors.Is(err, storagev1.ErrTooLarge) && len(chunk) > 1 {
				half := len(chunk) / 2
				pending = append([][]storagev1.RawRecord{chunk[:half], chunk[half:]}, pending[1:]...)
				continue
			}
			require.NoError(t, err, "family %s batch %d", family.Family, sequence)
			pending = pending[1:]
			imported += int64(len(chunk))
			require.Equal(t, storagev1.ImportFamilyProgress{Family: family.Family, Sequence: sequence, Imported: imported}, progress)
			if replay && sequence == 1 {
				repeated, err := target.ImportRecords(t.Context(), request)
				require.NoError(t, err, "family %s repeated batch", family.Family)
				require.Equal(t, progress, repeated)
			}
			sequence++
		}
		next[family.Family] = sequence
	}
	return next
}

func requireProgressMatches(t *testing.T, manifest []storagev1.FamilyDigest, progress storagev1.ImportProgress) {
	t.Helper()
	require.Len(t, progress.Families, len(manifest))
	for i, family := range manifest {
		require.Equal(t, family.Family, progress.Families[i].Family)
		require.Equal(t, family.Count, progress.Families[i].Imported, family.Family)
	}
}

func importedCount(progress storagev1.ImportProgress, family string) int64 {
	for _, held := range progress.Families {
		if held.Family == family {
			return held.Imported
		}
	}
	return -1
}

// firstRecord returns the first record of the first non-empty family that has
// a field outside its key.
func firstRecord(t *testing.T, manifest []storagev1.FamilyDigest, records map[string][]storagev1.RawRecord) (string, storagev1.RawRecord) {
	t.Helper()
	for _, family := range manifest {
		schema, _ := storagev1.LookupExportSchema(family.Family)
		if len(records[family.Family]) > 0 && slices.ContainsFunc(schema.Fields, func(field storagev1.ExportField) bool { return !field.Key }) {
			return family.Family, records[family.Family][0]
		}
	}
	t.Fatal("the seed wrote no record with a field outside its key")
	return "", storagev1.RawRecord{}
}

// changedRecord returns record with its first field outside the key changed.
func changedRecord(t *testing.T, record storagev1.RawRecord) storagev1.RawRecord {
	t.Helper()
	schema, ok := storagev1.LookupExportSchema(record.Family)
	require.True(t, ok)
	values, err := schema.Decode(record)
	require.NoError(t, err)
	for i, field := range schema.Fields {
		if field.Key {
			continue
		}
		values[i] = changedExportValue(field, values[i])
		changed, err := schema.Encode(values)
		require.NoError(t, err)
		return changed
	}
	t.Fatalf("family %s has no field outside its key", record.Family)
	return storagev1.RawRecord{}
}

func changedExportValue(field storagev1.ExportField, value any) any {
	switch typed := value.(type) {
	case string:
		return typed + "-changed"
	case int64:
		return typed + 1
	case bool:
		return !typed
	case []byte:
		return append(slices.Clone(typed), 'x')
	case time.Time:
		return typed.Add(time.Nanosecond)
	default:
		return exampleExportValue(field.Kind, 1)
	}
}

// exportExampleTime is a time with nanoseconds, in a zone other than UTC.
var exportExampleTime = time.Date(2026, 9, 1, 12, 0, 0, 123456789, time.FixedZone("UTC+3", 3*60*60))

// exampleExportValue returns a value of kind that differs for each n.
func exampleExportValue(kind storagev1.ExportFieldKind, n int) any {
	switch kind {
	case storagev1.ExportString:
		return "value-" + strconv.Itoa(n)
	case storagev1.ExportInt:
		return int64(n)
	case storagev1.ExportBool:
		return n%2 == 1
	case storagev1.ExportTime:
		return exportExampleTime.Add(time.Duration(n) * time.Nanosecond)
	default:
		return []byte("bytes-" + strconv.Itoa(n))
	}
}
