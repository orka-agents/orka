package v1_test

import (
	"encoding/binary"
	"slices"
	"strconv"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

func TestExportSchemasAreWellFormed(t *testing.T) {
	position := map[string]int{}
	for i, schema := range storagev1.ExportSchemas() {
		require.NotContains(t, position, schema.Name, "family %s is listed twice", schema.Name)
		position[schema.Name] = i
		require.Contains(t, []string{storagev1.StateContract, storagev1.ArtifactsContract}, schema.Contract, schema.Name)
		require.Equal(t, 1, schema.Version, schema.Name)
		require.Equal(t, "orka.storage.v1/export/"+schema.Name+"/v1", schema.Encoding())
		fields := map[string]storagev1.ExportField{}
		keys := 0
		for _, field := range schema.Fields {
			require.NotContains(t, fields, field.Name, "%s field %s is listed twice", schema.Name, field.Name)
			require.NotEqual(t, "unknown", field.Kind.String(), "%s field %s", schema.Name, field.Name)
			fields[field.Name] = field
			if field.Key {
				keys++
				require.False(t, field.Nullable, "%s key field %s is never absent", schema.Name, field.Name)
			}
		}
		require.Positive(t, keys, "%s has a key", schema.Name)
		got, ok := storagev1.LookupExportSchema(schema.Name)
		require.True(t, ok)
		require.Equal(t, schema, got)
	}
	// Sessions come before the records that belong to a Session.
	for _, order := range [][2]string{
		{"sessions", "session_messages"}, {"sessions", "session_turns"}, {"sessions", "native_session_snapshots"},
		{"sessions", "native_session_operations"}, {"sessions", "native_session_finalizations"},
	} {
		require.Less(t, position[order[0]], position[order[1]], "%s is imported before %s", order[0], order[1])
	}
	_, ok := storagev1.LookupExportSchema("no-such-family")
	require.False(t, ok)
	schemas := storagev1.ExportSchemas()
	schemas[0].Fields[0].Name = "changed"
	require.NotEqual(t, "changed", storagev1.ExportSchemas()[0].Fields[0].Name, "ExportSchemas returns copies")
}

// exampleValues returns a value of each field's kind that differs for each n,
// with times in a zone other than UTC and with nanoseconds.
func exampleValues(schema storagev1.ExportFamilySchema, n int) []any {
	zone := time.FixedZone("UTC-5", -5*60*60)
	values := make([]any, len(schema.Fields))
	for i, field := range schema.Fields {
		switch field.Kind {
		case storagev1.ExportString:
			values[i] = "value-" + strconv.Itoa(n) + "-" + strconv.Itoa(i)
		case storagev1.ExportInt:
			values[i] = int64(n*1000 + i - 500)
		case storagev1.ExportBool:
			values[i] = (n+i)%2 == 0
		case storagev1.ExportTime:
			values[i] = time.Date(1969, 12, 31, 23, 59, 59, 999999999, zone).Add(time.Duration(n*1000+i) * time.Second)
		case storagev1.ExportBytes:
			values[i] = []byte{0, byte(n), byte(i), 0xff}
		}
	}
	return values
}

func TestExportRecordsRoundTripExactly(t *testing.T) {
	for _, schema := range storagev1.ExportSchemas() {
		t.Run(schema.Name, func(t *testing.T) {
			values := exampleValues(schema, 1)
			record, err := schema.Encode(values)
			require.NoError(t, err)
			require.Equal(t, schema.Name, record.Family)
			require.Equal(t, schema.Encoding(), record.Encoding)
			decoded, err := schema.Decode(record)
			require.NoError(t, err)
			require.Len(t, decoded, len(values))
			for i, field := range schema.Fields {
				if field.Kind == storagev1.ExportTime {
					decodedTime := decoded[i].(time.Time)
					require.True(t, values[i].(time.Time).Equal(decodedTime), field.Name)
					require.Equal(t, time.UTC, decodedTime.Location(), "decoded times are UTC")
					require.Equal(t, values[i].(time.Time).Nanosecond(), decodedTime.Nanosecond(), "nanoseconds are kept")
					continue
				}
				require.Equal(t, values[i], decoded[i], field.Name)
			}
			again, err := schema.Encode(decoded)
			require.NoError(t, err)
			require.Equal(t, record, again, "a record has one encoding")
			other, err := schema.Encode(exampleValues(schema, 2))
			require.NoError(t, err)
			require.NotEqual(t, record.Key, other.Key, "different key fields have different keys")
		})
	}
}

func TestExportEncodingKeepsAbsentAndZeroValuesApart(t *testing.T) {
	schema := storagev1.ExportFamilySchema{Name: "results", Contract: storagev1.StateContract, Version: 1, Fields: []storagev1.ExportField{
		{Name: "id", Kind: storagev1.ExportString, Key: true},
		{Name: "text", Kind: storagev1.ExportString, Nullable: true},
		{Name: "data", Kind: storagev1.ExportBytes, Nullable: true},
		{Name: "required", Kind: storagev1.ExportBytes},
		{Name: "count", Kind: storagev1.ExportInt},
	}}
	absent, err := schema.Encode([]any{"a", nil, nil, []byte(nil), 7})
	require.NoError(t, err)
	zero, err := schema.Encode([]any{"a", "", []byte{}, []byte{}, int32(7)})
	require.NoError(t, err)
	require.NotEqual(t, absent.Body, zero.Body)
	values, err := schema.Decode(absent)
	require.NoError(t, err)
	require.Equal(t, []any{"a", nil, nil, []byte{}, int64(7)}, values, "nil bytes for a required field are empty bytes")
	values, err = schema.Decode(zero)
	require.NoError(t, err)
	require.Equal(t, []any{"a", "", []byte{}, []byte{}, int64(7)}, values)
}

func TestExportEncodeRefusesValuesOfTheWrongShape(t *testing.T) {
	schema, ok := storagev1.LookupExportSchema("results")
	require.True(t, ok)
	valid := exampleValues(schema, 1)
	_, err := schema.Encode(valid)
	require.NoError(t, err)
	for name, values := range map[string][]any{
		"too few values":        valid[:len(valid)-1],
		"too many values":       append(slices.Clone(valid), "extra"),
		"nil for a key field":   replaced(valid, 0, nil),
		"a number for a string": replaced(valid, 0, 7),
		"a string for bytes":    replaced(valid, 2, "data"),
		"a string for a time":   replaced(valid, 3, "2026-01-01T00:00:00Z"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := schema.Encode(values)
			require.ErrorIs(t, err, storagev1.ErrValidation)
		})
	}
}

func replaced(values []any, index int, value any) []any {
	changed := slices.Clone(values)
	changed[index] = value
	return changed
}

func TestExportDecodeRefusesAnythingButTheCanonicalEncoding(t *testing.T) {
	schema := storagev1.ExportFamilySchema{Name: "results", Contract: storagev1.StateContract, Version: 1, Fields: []storagev1.ExportField{
		{Name: "id", Kind: storagev1.ExportInt, Key: true},
		{Name: "flag", Kind: storagev1.ExportBool},
		{Name: "at", Kind: storagev1.ExportTime},
	}}
	record, err := schema.Encode([]any{int64(5), true, time.Unix(10, 20)})
	require.NoError(t, err)
	body := func(parts ...[]byte) []byte { return slices.Concat(parts...) }
	count := binary.AppendUvarint(nil, 3)
	id := binary.AppendVarint([]byte{'i'}, 5)
	flag := []byte{'b', 1}
	at := binary.AppendUvarint(binary.AppendVarint([]byte{'t'}, 10), 20)
	require.Equal(t, body(count, id, flag, at), record.Body, "the documented encoding")

	for name, changed := range map[string]storagev1.RawRecord{
		"another family":     {Family: "plans", Key: record.Key, Encoding: record.Encoding, Body: record.Body},
		"another encoding":   {Family: record.Family, Key: record.Key, Encoding: record.Encoding + "0", Body: record.Body},
		"another key":        {Family: record.Family, Key: record.Key + "A", Encoding: record.Encoding, Body: record.Body},
		"a missing field":    {Family: record.Family, Key: record.Key, Encoding: record.Encoding, Body: body(binary.AppendUvarint(nil, 2), id, flag)},
		"trailing bytes":     {Family: record.Family, Key: record.Key, Encoding: record.Encoding, Body: body(count, id, flag, at, []byte{0})},
		"a truncated value":  {Family: record.Family, Key: record.Key, Encoding: record.Encoding, Body: body(count, id, flag, at[:2])},
		"an overlong varint": {Family: record.Family, Key: record.Key, Encoding: record.Encoding, Body: body(count, []byte{'i', 0x8a, 0x00}, flag, at)},
		"a boolean of 2":     {Family: record.Family, Key: record.Key, Encoding: record.Encoding, Body: body(count, id, []byte{'b', 2}, at)},
		"a second of nanoseconds": {Family: record.Family, Key: record.Key, Encoding: record.Encoding,
			Body: body(count, id, flag, binary.AppendUvarint(binary.AppendVarint([]byte{'t'}, 10), uint64(time.Second)))},
		"another kind's tag":       {Family: record.Family, Key: record.Key, Encoding: record.Encoding, Body: body(count, id, []byte{'i', 2}, at)},
		"an absent required field": {Family: record.Family, Key: record.Key, Encoding: record.Encoding, Body: body(count, id, []byte{'n'}, at)},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := schema.Decode(changed)
			require.ErrorIs(t, err, storagev1.ErrValidation)
		})
	}
}

func TestFamilyDigestIgnoresOrderOnly(t *testing.T) {
	schema, ok := storagev1.LookupExportSchema("results")
	require.True(t, ok)
	records := make([]storagev1.RawRecord, 0, 3)
	for n := range 3 {
		record, err := schema.Encode(exampleValues(schema, n))
		require.NoError(t, err)
		records = append(records, record)
	}
	digest := func(records ...storagev1.RawRecord) storagev1.FamilyDigest {
		digester, err := storagev1.NewFamilyDigester("results")
		require.NoError(t, err)
		for _, record := range records {
			require.NoError(t, digester.Add(record))
		}
		return digester.Sum()
	}
	forward := digest(records...)
	require.Equal(t, "results", forward.Family)
	require.Equal(t, schema.Encoding(), forward.Encoding)
	require.Equal(t, int64(3), forward.Count)
	require.NoError(t, storagev1.ValidateCanonicalDigest("digest", forward.Digest))
	require.Equal(t, forward, digest(records[2], records[0], records[1]), "order does not matter")
	require.NotEqual(t, forward.Digest, digest(records[:2]...).Digest)
	require.NotEqual(t, forward.Digest, digest(records[0], records[1], records[1]).Digest, "a repeated record counts")
	changed := records[2]
	changed.Body = append(slices.Clone(changed.Body[:len(changed.Body)-1]), changed.Body[len(changed.Body)-1]^1)
	require.NotEqual(t, forward.Digest, digest(records[0], records[1], changed).Digest)
	require.NotEqual(t, digest().Digest, digest(records[0]).Digest)

	digester, err := storagev1.NewFamilyDigester("results")
	require.NoError(t, err)
	plans, ok := storagev1.LookupExportSchema("plan_states")
	require.True(t, ok)
	plan, err := plans.Encode(exampleValues(plans, 1))
	require.NoError(t, err)
	require.ErrorIs(t, digester.Add(plan), storagev1.ErrValidation)
	_, err = storagev1.NewFamilyDigester("no-such-family")
	require.ErrorIs(t, err, storagev1.ErrValidation)
}

func exampleManifest(t *testing.T) []storagev1.FamilyDigest {
	t.Helper()
	schemas := storagev1.ExportSchemas()
	manifest := make([]storagev1.FamilyDigest, 0, len(schemas))
	for _, schema := range schemas {
		digester, err := storagev1.NewFamilyDigester(schema.Name)
		require.NoError(t, err)
		manifest = append(manifest, digester.Sum())
	}
	return manifest
}

func TestManifestDigest(t *testing.T) {
	manifest := exampleManifest(t)
	digest, err := storagev1.ManifestDigest(manifest)
	require.NoError(t, err)
	reversed := slices.Clone(manifest)
	slices.Reverse(reversed)
	again, err := storagev1.ManifestDigest(reversed)
	require.NoError(t, err)
	require.Equal(t, digest, again, "the manifest's order does not matter")
	changed := slices.Clone(manifest)
	changed[0].Count = 1
	other, err := storagev1.ManifestDigest(changed)
	require.NoError(t, err)
	require.NotEqual(t, digest, other)

	for name, change := range map[string]func([]storagev1.FamilyDigest) []storagev1.FamilyDigest{
		"no family":        func([]storagev1.FamilyDigest) []storagev1.FamilyDigest { return nil },
		"a family twice":   func(m []storagev1.FamilyDigest) []storagev1.FamilyDigest { return append(m, m[0]) },
		"unknown family":   func(m []storagev1.FamilyDigest) []storagev1.FamilyDigest { m[0].Family = "no-such-family"; return m },
		"another encoding": func(m []storagev1.FamilyDigest) []storagev1.FamilyDigest { m[0].Encoding += "0"; return m },
		"negative count":   func(m []storagev1.FamilyDigest) []storagev1.FamilyDigest { m[0].Count = -1; return m },
		"invalid digest":   func(m []storagev1.FamilyDigest) []storagev1.FamilyDigest { m[0].Digest = "sha256:00"; return m },
	} {
		t.Run(name, func(t *testing.T) {
			_, err := storagev1.ManifestDigest(change(slices.Clone(manifest)))
			require.ErrorIs(t, err, storagev1.ErrValidation)
		})
	}
}

func TestExportRequestsValidate(t *testing.T) {
	manifest := exampleManifest(t)
	digest, err := storagev1.ManifestDigest(manifest)
	require.NoError(t, err)
	begin := storagev1.BeginImportRequest{
		Contracts: []string{storagev1.ArtifactsContract, storagev1.StateContract},
		Source:    storagev1.DatasetSource{DatasetID: "source-dataset", SchemaVersion: "3", ManifestDigest: digest},
		Manifest:  manifest,
	}
	require.NoError(t, begin.Validate())
	var stateOnly []storagev1.FamilyDigest
	for _, family := range manifest {
		if schema, _ := storagev1.LookupExportSchema(family.Family); schema.Contract == storagev1.StateContract {
			stateOnly = append(stateOnly, family)
		}
	}
	stateDigest, err := storagev1.ManifestDigest(stateOnly)
	require.NoError(t, err)
	stateImport := storagev1.BeginImportRequest{
		Contracts: []string{storagev1.StateContract},
		Source:    storagev1.DatasetSource{DatasetID: "source-dataset", SchemaVersion: "3", ManifestDigest: stateDigest},
		Manifest:  stateOnly,
	}
	require.NoError(t, stateImport.Validate(), "a contract part can move on its own")

	for name, change := range map[string]func(*storagev1.BeginImportRequest){
		"no contract part":         func(r *storagev1.BeginImportRequest) { r.Contracts = nil },
		"unknown contract part":    func(r *storagev1.BeginImportRequest) { r.Contracts = []string{"other.v1"} },
		"a contract part twice":    func(r *storagev1.BeginImportRequest) { r.Contracts = append(r.Contracts, storagev1.StateContract) },
		"no source dataset":        func(r *storagev1.BeginImportRequest) { r.Source.DatasetID = "" },
		"no source schema":         func(r *storagev1.BeginImportRequest) { r.Source.SchemaVersion = "" },
		"another manifest digest":  func(r *storagev1.BeginImportRequest) { r.Source.ManifestDigest = stateDigest },
		"a family of another part": func(r *storagev1.BeginImportRequest) { r.Contracts = []string{storagev1.StateContract} },
	} {
		t.Run(name, func(t *testing.T) {
			request := begin
			request.Contracts = slices.Clone(begin.Contracts)
			change(&request)
			require.ErrorIs(t, request.Validate(), storagev1.ErrValidation)
		})
	}
	missing := stateImport
	missing.Manifest = stateOnly[1:]
	missing.Source.ManifestDigest, err = storagev1.ManifestDigest(missing.Manifest)
	require.NoError(t, err)
	require.ErrorIs(t, missing.Validate(), storagev1.ErrValidation, "a manifest that misses a family")

	schema, _ := storagev1.LookupExportSchema("results")
	record, err := schema.Encode(exampleValues(schema, 1))
	require.NoError(t, err)
	batch := storagev1.ImportRecordsRequest{SessionID: "session", Family: "results", Sequence: 1, Records: []storagev1.RawRecord{record}}
	require.NoError(t, batch.Validate())
	for name, change := range map[string]func(*storagev1.ImportRecordsRequest){
		"no session":                 func(r *storagev1.ImportRecordsRequest) { r.SessionID = "" },
		"unknown family":             func(r *storagev1.ImportRecordsRequest) { r.Family = "no-such-family" },
		"sequence zero":              func(r *storagev1.ImportRecordsRequest) { r.Sequence = 0 },
		"a record of another family": func(r *storagev1.ImportRecordsRequest) { r.Family = "plan_states" },
		"too many records": func(r *storagev1.ImportRecordsRequest) {
			r.Records = slices.Repeat(r.Records, storagev1.MaxImportBatchRecords+1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			request := batch
			change(&request)
			require.ErrorIs(t, request.Validate(), storagev1.ErrValidation)
		})
	}

	require.NoError(t, storagev1.ExportRequest{Family: "results", Limit: storagev1.MaxExportPageRecords}.Validate())
	for _, request := range []storagev1.ExportRequest{
		{Family: "no-such-family", Limit: 1}, {Family: "results"}, {Family: "results", Limit: storagev1.MaxExportPageRecords + 1},
	} {
		require.ErrorIs(t, request.Validate(), storagev1.ErrValidation)
	}

	require.NoError(t, storagev1.CompleteImportRequest{SessionID: "session", ManifestDigest: digest}.Validate())
	require.ErrorIs(t, storagev1.CompleteImportRequest{ManifestDigest: digest}.Validate(), storagev1.ErrValidation)
	require.ErrorIs(t, storagev1.CompleteImportRequest{SessionID: "session"}.Validate(), storagev1.ErrValidation)

	marker := storagev1.MigrationMarkerRequest{DatasetID: "source", Contract: storagev1.StateContract, TargetDatasetID: "target"}
	require.NoError(t, marker.Validate())
	for _, request := range []storagev1.MigrationMarkerRequest{
		{Contract: storagev1.StateContract, TargetDatasetID: "target"},
		{DatasetID: "source", Contract: "other.v1", TargetDatasetID: "target"},
		{DatasetID: "source", Contract: storagev1.StateContract},
		{DatasetID: "source", Contract: storagev1.StateContract, TargetDatasetID: "source"},
	} {
		require.ErrorIs(t, request.Validate(), storagev1.ErrValidation)
	}
}

func TestExportErrorsMatchTheirSentinels(t *testing.T) {
	require.ErrorIs(t, storagev1.ErrDatasetMigrated, storagev1.ErrNotReady)
	err := storagev1.ImportVerificationError{
		Expected: storagev1.FamilyDigest{Family: "results", Count: 2},
		Actual:   storagev1.FamilyDigest{Family: "results", Count: 1},
	}
	require.ErrorIs(t, err, storagev1.ErrConflict)
	require.Contains(t, err.Error(), "results")
}
