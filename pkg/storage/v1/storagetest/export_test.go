package storagetest

import (
	"bytes"
	"context"
	"encoding/base64"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// TestRunExportImportAgainstAReferenceProvider runs the export and import
// cases against memoryExport, an in-memory provider of only the export and
// import surface, so the cases are exercised before a full provider
// implements them.
func TestRunExportImportAgainstAReferenceProvider(t *testing.T) {
	RunExportImport(t, func(t *testing.T) ExportProvider { return newMemoryExport() }, seedMemoryExport)
}

// seedMemoryExport stores four records of every family, each field holding
// a value of its kind, with every third nullable field absent.
func seedMemoryExport(t *testing.T, provider ExportProvider) {
	t.Helper()
	memory := provider.(*memoryExport)
	for _, schema := range storagev1.ExportSchemas() {
		for i := range 4 {
			values := make([]any, len(schema.Fields))
			for f, field := range schema.Fields {
				if field.Nullable && (i+f)%3 == 0 {
					continue
				}
				values[f] = exampleExportValue(field.Kind, i*1000+f)
			}
			record, err := schema.Encode(values)
			require.NoError(t, err)
			memory.records[schema.Name][record.Key] = record
		}
	}
}

// memoryExport keeps one dataset of canonical records in memory and
// implements the export and import surface as the contract states it.
type memoryExport struct {
	mu        sync.Mutex
	ids       int
	datasetID string
	origin    storagev1.DatasetOrigin
	state     storagev1.DatasetState
	source    *storagev1.DatasetSource
	markers   map[string]storagev1.MigrationMarker
	records   map[string]map[string]storagev1.RawRecord
	session   *memoryImport
}

type memoryImport struct {
	id        string
	request   storagev1.BeginImportRequest
	manifest  map[string]storagev1.FamilyDigest
	sequences map[string]int64
	completed bool
}

func newMemoryExport() *memoryExport {
	m := &memoryExport{origin: storagev1.DatasetOriginNew, state: storagev1.DatasetReady, markers: map[string]storagev1.MigrationMarker{}}
	m.datasetID = m.newID("dataset")
	m.clear()
	return m
}

var memoryExportContracts = []string{storagev1.StateContract, storagev1.ArtifactsContract}

func (m *memoryExport) newID(prefix string) string {
	m.ids++
	return prefix + "-" + strconv.Itoa(m.ids)
}

func (m *memoryExport) clear() {
	m.records = map[string]map[string]storagev1.RawRecord{}
	for _, schema := range storagev1.ExportSchemas() {
		m.records[schema.Name] = map[string]storagev1.RawRecord{}
	}
}

func (m *memoryExport) Describe(context.Context) (storagev1.ProviderInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.describeLocked(), nil
}

func (m *memoryExport) describeLocked() storagev1.ProviderInfo {
	info := storagev1.ProviderInfo{
		ContractVersion: storagev1.ContractVersion, Contracts: slices.Clone(memoryExportContracts),
		DatasetID: m.datasetID, DatasetOrigin: m.origin, SchemaVersion: "memory-1",
		Capabilities: []string{storagev1.ExportCapability}, Durability: storagev1.DurabilityLocalBuffered,
		State: m.state, ObservedAt: time.Now().UTC(),
	}
	if m.source != nil {
		source := *m.source
		info.Source = &source
	}
	for _, contract := range slices.Sorted(maps.Keys(m.markers)) {
		info.MigratedTo = append(info.MigratedTo, m.markers[contract])
	}
	return info
}

func (m *memoryExport) ExportFamilies(context.Context) ([]storagev1.ExportFamily, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	schemas := storagev1.ExportSchemas()
	families := make([]storagev1.ExportFamily, 0, len(schemas))
	for _, schema := range schemas {
		families = append(families, storagev1.ExportFamily{
			Name: schema.Name, Contract: schema.Contract, RecordVersion: schema.Version, Estimated: int64(len(m.records[schema.Name])),
		})
	}
	return families, nil
}

// The cursor is the base64url form of the last key returned.
const memoryCursorPrefix = "after:"

func (m *memoryExport) ExportRecords(_ context.Context, request storagev1.ExportRequest) (storagev1.ExportPage, error) {
	if err := request.Validate(); err != nil {
		return storagev1.ExportPage{}, err
	}
	after := ""
	if request.Cursor != "" {
		encoded, ok := strings.CutPrefix(request.Cursor, memoryCursorPrefix)
		decoded, err := base64.RawURLEncoding.DecodeString(encoded)
		if !ok || err != nil {
			return storagev1.ExportPage{}, storagev1.ValidationErrorf("export cursor %q is not valid", request.Cursor)
		}
		after = string(decoded)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var page storagev1.ExportPage
	keys := slices.Sorted(maps.Keys(m.records[request.Family]))
	for _, key := range keys {
		if request.Cursor != "" && key <= after {
			continue
		}
		if len(page.Records) == request.Limit {
			page.NextCursor = memoryCursorPrefix + base64.RawURLEncoding.EncodeToString([]byte(page.Records[len(page.Records)-1].Key))
			break
		}
		page.Records = append(page.Records, m.records[request.Family][key])
	}
	return page, nil
}

func (m *memoryExport) ExportDigest(_ context.Context, family string) (storagev1.FamilyDigest, error) {
	digester, err := storagev1.NewFamilyDigester(family)
	if err != nil {
		return storagev1.FamilyDigest{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.digestLocked(digester, family), nil
}

func (m *memoryExport) digestLocked(digester *storagev1.FamilyDigester, family string) storagev1.FamilyDigest {
	for _, record := range m.records[family] {
		if err := digester.Add(record); err != nil {
			panic(err)
		}
	}
	return digester.Sum()
}

func (m *memoryExport) BeginImport(_ context.Context, request storagev1.BeginImportRequest) (storagev1.ImportSession, error) {
	if err := request.Validate(); err != nil {
		return storagev1.ImportSession{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.session != nil && !m.session.completed {
		if !sameBeginImport(m.session.request, request) {
			return storagev1.ImportSession{}, storagev1.ConflictErrorf("another import is in progress")
		}
		return storagev1.ImportSession{SessionID: m.session.id, DatasetID: m.datasetID}, nil
	}
	if m.origin != storagev1.DatasetOriginNew || m.state != storagev1.DatasetReady || len(m.markers) > 0 || m.holdsRecordsLocked() {
		return storagev1.ImportSession{}, storagev1.ConflictErrorf("the dataset is not empty")
	}
	m.session = &memoryImport{
		id: m.newID("import"), request: cloneBeginImport(request),
		manifest: map[string]storagev1.FamilyDigest{}, sequences: map[string]int64{},
	}
	for _, family := range request.Manifest {
		m.session.manifest[family.Family] = family
	}
	m.datasetID = m.newID("dataset")
	m.state = storagev1.DatasetImporting
	return storagev1.ImportSession{SessionID: m.session.id, DatasetID: m.datasetID}, nil
}

func sameBeginImport(a, b storagev1.BeginImportRequest) bool {
	return slices.Equal(slices.Sorted(slices.Values(a.Contracts)), slices.Sorted(slices.Values(b.Contracts))) &&
		a.Source == b.Source
}

func cloneBeginImport(request storagev1.BeginImportRequest) storagev1.BeginImportRequest {
	request.Contracts = slices.Clone(request.Contracts)
	request.Manifest = slices.Clone(request.Manifest)
	return request
}

func (m *memoryExport) holdsRecordsLocked() bool {
	for _, records := range m.records {
		if len(records) > 0 {
			return true
		}
	}
	return false
}

func (m *memoryExport) sessionLocked(id string) (*memoryImport, error) {
	if m.session == nil || m.session.id != id {
		return nil, storagev1.ErrNotFound
	}
	return m.session, nil
}

func (m *memoryExport) ImportRecords(_ context.Context, request storagev1.ImportRecordsRequest) (storagev1.ImportFamilyProgress, error) {
	if err := request.Validate(); err != nil {
		return storagev1.ImportFamilyProgress{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session, err := m.sessionLocked(request.SessionID)
	if err != nil {
		return storagev1.ImportFamilyProgress{}, err
	}
	if session.completed {
		return storagev1.ImportFamilyProgress{}, storagev1.ConflictErrorf("the import completed")
	}
	expected, ok := session.manifest[request.Family]
	if !ok {
		return storagev1.ImportFamilyProgress{}, storagev1.ValidationErrorf("family %q is not part of the import", request.Family)
	}
	schema, _ := storagev1.LookupExportSchema(request.Family)
	held := m.records[request.Family]
	var added []storagev1.RawRecord
	for _, record := range request.Records {
		if record.Encoding != expected.Encoding {
			return storagev1.ImportFamilyProgress{}, storagev1.ValidationErrorf("record encoding %q is not the manifest's", record.Encoding)
		}
		if _, err := schema.Decode(record); err != nil {
			return storagev1.ImportFamilyProgress{}, err
		}
		stored, ok := held[record.Key]
		switch {
		case ok && !bytes.Equal(stored.Body, record.Body):
			return storagev1.ImportFamilyProgress{}, storagev1.ConflictErrorf("family %q holds the record's key with another body", request.Family)
		case !ok:
			added = append(added, record)
		}
	}
	last := session.sequences[request.Family]
	switch {
	case request.Sequence > last+1:
		return storagev1.ImportFamilyProgress{}, storagev1.ConflictErrorf("batch %d follows batch %d", request.Sequence, last)
	case request.Sequence <= last:
		if len(added) > 0 {
			return storagev1.ImportFamilyProgress{}, storagev1.ConflictErrorf("repeated batch %d holds records the family does not", request.Sequence)
		}
	default:
		for _, record := range added {
			held[record.Key] = record
		}
		session.sequences[request.Family] = request.Sequence
	}
	return storagev1.ImportFamilyProgress{
		Family: request.Family, Sequence: session.sequences[request.Family], Imported: int64(len(held)),
	}, nil
}

func (m *memoryExport) GetImportProgress(_ context.Context, sessionID string) (storagev1.ImportProgress, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, err := m.sessionLocked(sessionID)
	if err != nil {
		return storagev1.ImportProgress{}, err
	}
	progress := storagev1.ImportProgress{SessionID: session.id, DatasetID: m.datasetID, State: m.state}
	for _, schema := range storagev1.ExportSchemas() {
		if _, ok := session.manifest[schema.Name]; ok {
			progress.Families = append(progress.Families, storagev1.ImportFamilyProgress{
				Family: schema.Name, Sequence: session.sequences[schema.Name], Imported: int64(len(m.records[schema.Name])),
			})
		}
	}
	return progress, nil
}

func (m *memoryExport) CompleteImport(_ context.Context, request storagev1.CompleteImportRequest) (storagev1.ProviderInfo, error) {
	if err := request.Validate(); err != nil {
		return storagev1.ProviderInfo{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session, err := m.sessionLocked(request.SessionID)
	if err != nil {
		return storagev1.ProviderInfo{}, err
	}
	if request.ManifestDigest != session.request.Source.ManifestDigest {
		return storagev1.ProviderInfo{}, storagev1.ConflictErrorf("the import began with another manifest")
	}
	if session.completed {
		return m.describeLocked(), nil
	}
	for _, schema := range storagev1.ExportSchemas() {
		expected, ok := session.manifest[schema.Name]
		if !ok {
			continue
		}
		digester, err := storagev1.NewFamilyDigester(schema.Name)
		if err != nil {
			return storagev1.ProviderInfo{}, err
		}
		if actual := m.digestLocked(digester, schema.Name); actual != expected {
			return storagev1.ProviderInfo{}, storagev1.ImportVerificationError{Expected: expected, Actual: actual}
		}
	}
	session.completed = true
	source := session.request.Source
	m.source = &source
	m.origin = storagev1.DatasetOriginMigrated
	m.state = storagev1.DatasetReady
	return m.describeLocked(), nil
}

func (m *memoryExport) AbortImport(_ context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, err := m.sessionLocked(sessionID)
	if err != nil {
		return err
	}
	if session.completed {
		return storagev1.ConflictErrorf("the import completed")
	}
	m.session = nil
	m.clear()
	m.datasetID = m.newID("dataset")
	m.state = storagev1.DatasetReady
	return nil
}

func (m *memoryExport) MarkDatasetMigrated(_ context.Context, request storagev1.MigrationMarkerRequest) (storagev1.MigrationMarker, error) {
	if err := request.Validate(); err != nil {
		return storagev1.MigrationMarker{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if request.DatasetID != m.datasetID {
		return storagev1.MigrationMarker{}, storagev1.ErrDatasetMismatch
	}
	if marker, ok := m.markers[request.Contract]; ok {
		if marker.TargetDatasetID != request.TargetDatasetID {
			return storagev1.MigrationMarker{}, storagev1.ConflictErrorf("the contract part was migrated to another dataset")
		}
		return marker, nil
	}
	marker := storagev1.MigrationMarker{Contract: request.Contract, TargetDatasetID: request.TargetDatasetID, MarkedAt: time.Now().UTC()}
	m.markers[request.Contract] = marker
	return marker, nil
}

func (m *memoryExport) ClearMigrationMarker(_ context.Context, request storagev1.MigrationMarkerRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if request.DatasetID != m.datasetID {
		return storagev1.ErrDatasetMismatch
	}
	if marker, ok := m.markers[request.Contract]; ok {
		if marker.TargetDatasetID != request.TargetDatasetID {
			return storagev1.ConflictErrorf("the contract part was migrated to another dataset")
		}
		delete(m.markers, request.Contract)
	}
	return nil
}
