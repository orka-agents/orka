package v1

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"time"
)

// ExportFieldKind is the type of one field of an exported record.
type ExportFieldKind uint8

const (
	// ExportString is a string, carried byte for byte. Its Go value is a
	// string.
	ExportString ExportFieldKind = iota + 1
	// ExportInt is a signed 64-bit integer. Its Go value is an int64.
	ExportInt
	// ExportBool is a boolean. Its Go value is a bool.
	ExportBool
	// ExportTime is an instant, exact to the nanosecond. Its Go value is a
	// time.Time; the encoding keeps the instant, not the location, and
	// decoding returns it in UTC.
	ExportTime
	// ExportBytes is a byte string, such as sealed ciphertext or a nonce,
	// carried byte for byte. Its Go value is a []byte.
	ExportBytes
)

// String returns the kind's name.
func (k ExportFieldKind) String() string {
	switch k {
	case ExportString:
		return "string"
	case ExportInt:
		return "int"
	case ExportBool:
		return "bool"
	case ExportTime:
		return "time"
	case ExportBytes:
		return "bytes"
	default:
		return "unknown"
	}
}

// ExportField is one field of a family's canonical record.
type ExportField struct {
	// Name is the field name, unique within the family.
	Name string
	// Kind is the field's type.
	Kind ExportFieldKind
	// Nullable says whether the field may be absent, which is distinct from
	// its zero value. A provider that keeps an absent value as SQL NULL
	// exports nil.
	Nullable bool
	// Key marks one of the fields that together identify a record within
	// its family. No two records of a family have equal key fields, and a
	// key field is never nullable.
	Key bool
}

// ExportFamilySchema defines a family's canonical record: the fields of one
// record, in order, some of them marked as the key that identifies it. It
// describes what a provider holds for the family, not how it stores it.
type ExportFamilySchema struct {
	// Name is the family name.
	Name string
	// Contract is the contract part the family belongs to.
	Contract string
	// Version is the version of the record encoding. It changes whenever the
	// fields change.
	Version int
	// Fields lists every field of a record, in encoding order.
	Fields []ExportField
}

// Encoding returns the name of the family's record encoding,
// "orka.storage.v1/export/<family>/v<version>".
func (s ExportFamilySchema) Encoding() string {
	return ContractVersion + "/export/" + s.Name + "/v" + strconv.Itoa(s.Version)
}

// The canonical encoding of a record is the field count as an unsigned
// varint, then each field in schema order as one tag byte and its value:
// strings and bytes as an unsigned varint length and the bytes, integers as
// a signed varint, booleans as one byte 0 or 1, and times as the signed
// varint of their Unix seconds and the unsigned varint of the nanoseconds
// within that second. An absent nullable field is the tag alone. A record has
// exactly one encoding, so equal records have equal bytes. A key is the same
// encoding of the key fields alone, in field order, in base64url without
// padding.
const (
	exportTagNull   = 'n'
	exportTagString = 's'
	exportTagInt    = 'i'
	exportTagBool   = 'b'
	exportTagTime   = 't'
	exportTagBytes  = 'y'
)

// Encode encodes a record whose values are given in field order, as the Go
// types [ExportFieldKind] names. An int or int32 is accepted for an
// [ExportInt], and nil for an absent nullable field. A value of another type,
// a missing or extra value, or nil for a field that is not nullable returns
// ErrValidation.
func (s ExportFamilySchema) Encode(values []any) (RawRecord, error) {
	if len(values) != len(s.Fields) {
		return RawRecord{}, ValidationErrorf("export family %q record has %d values, want %d", s.Name, len(values), len(s.Fields))
	}
	body := binary.AppendUvarint(nil, uint64(len(values)))
	var err error
	for i, field := range s.Fields {
		if body, err = appendExportValue(body, field, values[i]); err != nil {
			return RawRecord{}, ValidationErrorf("export family %q field %q: %v", s.Name, field.Name, err)
		}
	}
	key, err := s.encodeKey(values)
	if err != nil {
		return RawRecord{}, err
	}
	return RawRecord{Family: s.Name, Key: key, Encoding: s.Encoding(), Body: body}, nil
}

// Decode checks that record belongs to the family, in its encoding, with a
// canonical body and a Key that matches its key fields, and returns its
// values in field order, as Encode accepts them, with times in UTC. Anything
// else returns ErrValidation.
func (s ExportFamilySchema) Decode(record RawRecord) ([]any, error) {
	if record.Family != s.Name {
		return nil, ValidationErrorf("record of family %q decoded as family %q", record.Family, s.Name)
	}
	if record.Encoding != s.Encoding() {
		return nil, ValidationErrorf("export family %q record encoding %q is not %q", s.Name, record.Encoding, s.Encoding())
	}
	reader := exportReader{data: record.Body}
	count, err := reader.uvarint()
	if err != nil {
		return nil, ValidationErrorf("export family %q record: %v", s.Name, err)
	}
	if count != uint64(len(s.Fields)) {
		return nil, ValidationErrorf("export family %q record has %d fields, want %d", s.Name, count, len(s.Fields))
	}
	values := make([]any, len(s.Fields))
	for i, field := range s.Fields {
		if values[i], err = reader.value(field); err != nil {
			return nil, ValidationErrorf("export family %q field %q: %v", s.Name, field.Name, err)
		}
	}
	if len(reader.data) != 0 {
		return nil, ValidationErrorf("export family %q record has trailing bytes", s.Name)
	}
	encoded, err := s.Encode(values)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(encoded.Body, record.Body) {
		return nil, ValidationErrorf("export family %q record is not canonically encoded", s.Name)
	}
	if encoded.Key != record.Key {
		return nil, ValidationErrorf("export family %q record key does not match its key fields", s.Name)
	}
	return values, nil
}

func (s ExportFamilySchema) encodeKey(values []any) (string, error) {
	var fields []int
	for i, field := range s.Fields {
		if field.Key {
			fields = append(fields, i)
		}
	}
	if len(fields) == 0 {
		return "", ValidationErrorf("export family %q has no key field", s.Name)
	}
	key := binary.AppendUvarint(nil, uint64(len(fields)))
	for _, i := range fields {
		if values[i] == nil {
			return "", ValidationErrorf("export family %q key field %q is absent", s.Name, s.Fields[i].Name)
		}
		var err error
		if key, err = appendExportValue(key, s.Fields[i], values[i]); err != nil {
			return "", ValidationErrorf("export family %q key field %q: %v", s.Name, s.Fields[i].Name, err)
		}
	}
	return base64.RawURLEncoding.EncodeToString(key), nil
}

func appendExportValue(dst []byte, field ExportField, value any) ([]byte, error) {
	if value == nil {
		if !field.Nullable {
			return nil, fmt.Errorf("is not nullable")
		}
		return append(dst, exportTagNull), nil
	}
	switch field.Kind {
	case ExportString:
		if v, ok := value.(string); ok {
			dst = binary.AppendUvarint(append(dst, exportTagString), uint64(len(v)))
			return append(dst, v...), nil
		}
	case ExportInt:
		var v int64
		switch typed := value.(type) {
		case int64:
			v = typed
		case int:
			v = int64(typed)
		case int32:
			v = int64(typed)
		default:
			return nil, fmt.Errorf("holds %T, want an integer", value)
		}
		return binary.AppendVarint(append(dst, exportTagInt), v), nil
	case ExportBool:
		if v, ok := value.(bool); ok {
			if v {
				return append(dst, exportTagBool, 1), nil
			}
			return append(dst, exportTagBool, 0), nil
		}
	case ExportTime:
		if v, ok := value.(time.Time); ok {
			dst = binary.AppendVarint(append(dst, exportTagTime), v.Unix())
			return binary.AppendUvarint(dst, uint64(v.Nanosecond())), nil
		}
	case ExportBytes:
		if v, ok := value.([]byte); ok {
			if v == nil && !field.Nullable {
				v = []byte{}
			}
			if v == nil {
				return append(dst, exportTagNull), nil
			}
			dst = binary.AppendUvarint(append(dst, exportTagBytes), uint64(len(v)))
			return append(dst, v...), nil
		}
	default:
		return nil, fmt.Errorf("has undefined kind %d", field.Kind)
	}
	return nil, fmt.Errorf("holds %T, want %s", value, field.Kind)
}

// tag returns the tag byte of a present value of kind k.
func (k ExportFieldKind) tag() byte {
	switch k {
	case ExportString:
		return exportTagString
	case ExportInt:
		return exportTagInt
	case ExportBool:
		return exportTagBool
	case ExportTime:
		return exportTagTime
	case ExportBytes:
		return exportTagBytes
	default:
		return 0
	}
}

type exportReader struct {
	data []byte
}

func (r *exportReader) uvarint() (uint64, error) {
	value, n := binary.Uvarint(r.data)
	if n <= 0 {
		return 0, fmt.Errorf("truncated or overlong varint")
	}
	r.data = r.data[n:]
	return value, nil
}

func (r *exportReader) varint() (int64, error) {
	value, n := binary.Varint(r.data)
	if n <= 0 {
		return 0, fmt.Errorf("truncated or overlong varint")
	}
	r.data = r.data[n:]
	return value, nil
}

func (r *exportReader) bytes() ([]byte, error) {
	length, err := r.uvarint()
	if err != nil {
		return nil, err
	}
	if length > uint64(len(r.data)) {
		return nil, fmt.Errorf("truncated value")
	}
	value := bytes.Clone(r.data[:length])
	r.data = r.data[length:]
	return value, nil
}

func (r *exportReader) value(field ExportField) (any, error) {
	if len(r.data) == 0 {
		return nil, fmt.Errorf("truncated record")
	}
	tag := r.data[0]
	r.data = r.data[1:]
	if tag == exportTagNull {
		if !field.Nullable {
			return nil, fmt.Errorf("is not nullable")
		}
		return nil, nil
	}
	if want := field.Kind.tag(); tag != want {
		return nil, fmt.Errorf("has tag %q, want %q", tag, want)
	}
	switch field.Kind {
	case ExportString:
		value, err := r.bytes()
		return string(value), err
	case ExportInt:
		return r.varint()
	case ExportBool:
		if len(r.data) == 0 || r.data[0] > 1 {
			return nil, fmt.Errorf("is not a canonical boolean")
		}
		value := r.data[0] == 1
		r.data = r.data[1:]
		return value, nil
	case ExportTime:
		seconds, err := r.varint()
		if err != nil {
			return nil, err
		}
		nanos, err := r.uvarint()
		if err != nil {
			return nil, err
		}
		if nanos >= uint64(time.Second) {
			return nil, fmt.Errorf("has %d nanoseconds", nanos)
		}
		return time.Unix(seconds, int64(nanos)).UTC(), nil
	default:
		return r.bytes()
	}
}

// exportRecordDomain and exportFamilyDomain separate export digests from every
// other digest Orka computes. Changing an encoding changes them.
const (
	exportRecordDomain   = "orka.storage.v1 export record 1\x00"
	exportFamilyDomain   = "orka.storage.v1 export family 1\x00"
	exportManifestDomain = "orka.storage.v1 export manifest 1\x00"
)

// FamilyDigester computes a family's [FamilyDigest] from its records in any
// order: the SHA-256 of the family's encoding name, the record count and the
// sorted SHA-256 digests of the records, each over its encoding and body. Both sides
// of an import compute it the same way.
type FamilyDigester struct {
	schema  ExportFamilySchema
	records [][sha256.Size]byte
}

// NewFamilyDigester returns a digester for a family, or ErrValidation for an
// unknown family.
func NewFamilyDigester(family string) (*FamilyDigester, error) {
	schema, ok := LookupExportSchema(family)
	if !ok {
		return nil, ValidationErrorf("export family %q is not defined", family)
	}
	return &FamilyDigester{schema: schema}, nil
}

// Add adds one record. A record of another family or encoding returns
// ErrValidation; Add does not decode the body.
func (d *FamilyDigester) Add(record RawRecord) error {
	if record.Family != d.schema.Name || record.Encoding != d.schema.Encoding() {
		return ValidationErrorf("record of family %q in encoding %q added to the digest of family %q", record.Family, record.Encoding, d.schema.Name)
	}
	hash := sha256.New()
	hash.Write([]byte(exportRecordDomain))
	writeExportDigestText(hash, record.Encoding)
	hash.Write(record.Body)
	var sum [sha256.Size]byte
	hash.Sum(sum[:0])
	d.records = append(d.records, sum)
	return nil
}

// Sum returns the digest of the records added so far.
func (d *FamilyDigester) Sum() FamilyDigest {
	sorted := slices.Clone(d.records)
	slices.SortFunc(sorted, func(a, b [sha256.Size]byte) int { return bytes.Compare(a[:], b[:]) })
	hash := sha256.New()
	hash.Write([]byte(exportFamilyDomain))
	writeExportDigestText(hash, d.schema.Encoding())
	hash.Write(binary.AppendUvarint(nil, uint64(len(sorted))))
	for _, sum := range sorted {
		hash.Write(sum[:])
	}
	return FamilyDigest{
		Family:   d.schema.Name,
		Encoding: d.schema.Encoding(),
		Count:    int64(len(sorted)),
		Digest:   "sha256:" + hex.EncodeToString(hash.Sum(nil)),
	}
}

// ManifestDigest returns the "sha256:<hex>" digest of a manifest: the
// [FamilyDigest] of each family, in [ExportSchemas] order whatever order the
// manifest lists them in. Each entry must name a defined family once, in its
// current encoding, with a non-negative count and a canonical digest, or
// ManifestDigest returns ErrValidation.
func ManifestDigest(manifest []FamilyDigest) (string, error) {
	byFamily := make(map[string]FamilyDigest, len(manifest))
	for _, family := range manifest {
		schema, ok := LookupExportSchema(family.Family)
		if !ok {
			return "", ValidationErrorf("manifest family %q is not defined", family.Family)
		}
		if _, ok := byFamily[family.Family]; ok {
			return "", ValidationErrorf("manifest lists family %q twice", family.Family)
		}
		if family.Encoding != schema.Encoding() {
			return "", ValidationErrorf("manifest family %q encoding %q is not %q", family.Family, family.Encoding, schema.Encoding())
		}
		if family.Count < 0 {
			return "", ValidationErrorf("manifest family %q count is negative", family.Family)
		}
		if err := ValidateCanonicalDigest("manifest family "+family.Family+" digest", family.Digest); err != nil {
			return "", err
		}
		byFamily[family.Family] = family
	}
	if len(byFamily) == 0 {
		return "", ValidationErrorf("manifest lists no family")
	}
	hash := sha256.New()
	hash.Write([]byte(exportManifestDomain))
	hash.Write(binary.AppendUvarint(nil, uint64(len(byFamily))))
	for _, schema := range exportSchemas {
		family, ok := byFamily[schema.Name]
		if !ok {
			continue
		}
		writeExportDigestText(hash, family.Encoding)
		hash.Write(binary.AppendUvarint(nil, uint64(family.Count)))
		writeExportDigestText(hash, family.Digest)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func writeExportDigestText(hash interface{ Write([]byte) (int, error) }, text string) {
	_, _ = hash.Write(binary.AppendUvarint(nil, uint64(len(text))))
	_, _ = hash.Write([]byte(text))
}
