package security

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/store"
)

const (
	validationStatusValidated = "validated"
	validationStatusFailed    = "failed"
	validationStatusSkipped   = "skipped"

	ValidationArtifactSchemaVersion = 1
	MaxValidationArtifactBytes      = 256 << 10
	MaxValidationSummaryBytes       = 8 << 10
	MaxValidationTextFieldBytes     = 32 << 10
	MaxValidationArrayItems         = 128
	MaxValidationEvidenceItems      = 64
	MaxValidationEvidenceTextBytes  = 16 << 10
)

var validationArtifactKnownFields = map[string]struct{}{
	"version": {}, "finding_id": {}, "scan_run_id": {}, "occurrence_id": {}, "status": {}, "summary": {},
	"validation_steps": {}, "reproduction": {}, "attack_path_analysis": {}, "likelihood": {}, "impact": {},
	"assumptions": {}, "controls": {}, "blindspots": {}, "evidence": {}, "taskName": {},
}

var validationEvidenceKnownFields = map[string]struct{}{
	"kind": {}, "taskName": {}, "name": {}, "label": {}, "path": {}, "startLine": {}, "endLine": {}, "symbol": {}, "quote": {},
}

// ValidationArtifactParseOptions contains controller-trusted bindings and
// evidence resolvers. Callers must not populate these values from artifact JSON.
type ValidationArtifactParseOptions struct {
	ExpectedFindingID    string
	ExpectedScanRunID    string
	ExpectedOccurrenceID string
	TrustedTaskName      string
	RequireRunBinding    bool
	ValidateFileEvidence func(store.FindingEvidenceRef) error
	ResolveArtifact      func(name string) (contentSHA256 string, contentSize int64, err error)
}

// ParsedValidationArtifact is the normalized, bounded validation payload.
type ParsedValidationArtifact struct {
	Artifact       ValidationArtifact
	NormalizedJSON []byte
	ContentSHA256  string
}

// ParseValidationArtifact rejects ambiguous JSON and returns only normalized,
// controller-approved fields. Unknown extension fields are ignored rather than
// persisted.
//
//nolint:gocyclo // strict decoding intentionally validates every bounded field independently
func ParseValidationArtifact(data []byte, opts ValidationArtifactParseOptions) (*ParsedValidationArtifact, error) {
	if len(data) == 0 {
		return nil, errors.New("validation artifact is empty")
	}
	if len(data) > MaxValidationArtifactBytes {
		return nil, fmt.Errorf("validation artifact exceeds %d bytes", MaxValidationArtifactBytes)
	}
	if !utf8.Valid(data) {
		return nil, errors.New("validation artifact contains malformed UTF-8")
	}

	fields, err := decodeStrictJSONObject(data, validationArtifactKnownFields)
	if err != nil {
		return nil, err
	}

	var artifact ValidationArtifact
	if err := decodeRequiredField(fields, "version", &artifact.Version); err != nil {
		return nil, err
	}
	if artifact.Version != ValidationArtifactSchemaVersion {
		return nil, fmt.Errorf("version must be %d", ValidationArtifactSchemaVersion)
	}
	if err := decodeRequiredField(fields, "finding_id", &artifact.FindingID); err != nil {
		return nil, err
	}
	if raw, ok := fields["scan_run_id"]; ok {
		if err := decodeJSONValue(raw, &artifact.ScanRunID); err != nil {
			return nil, fieldError("scan_run_id", err)
		}
	}
	if raw, ok := fields["occurrence_id"]; ok {
		if err := decodeJSONValue(raw, &artifact.OccurrenceID); err != nil {
			return nil, fieldError("occurrence_id", err)
		}
	}
	if err := decodeRequiredField(fields, "status", &artifact.Status); err != nil {
		return nil, err
	}
	if err := decodeRequiredField(fields, "summary", &artifact.Summary); err != nil {
		return nil, err
	}

	artifact.FindingID = strings.TrimSpace(artifact.FindingID)
	artifact.ScanRunID = strings.TrimSpace(artifact.ScanRunID)
	artifact.OccurrenceID = strings.TrimSpace(artifact.OccurrenceID)
	artifact.Status = strings.ToLower(strings.TrimSpace(artifact.Status))
	artifact.Summary = strings.TrimSpace(artifact.Summary)

	for field, value := range map[string]string{
		"finding_id": artifact.FindingID, "scan_run_id": artifact.ScanRunID, "occurrence_id": artifact.OccurrenceID,
	} {
		if err := validateBoundedString(field, value, 256); err != nil {
			return nil, err
		}
	}
	if err := validateBoundedString("status", artifact.Status, 32); err != nil {
		return nil, err
	}

	if artifact.FindingID == "" {
		return nil, errors.New("finding_id is required")
	}
	if expected := strings.TrimSpace(opts.ExpectedFindingID); expected != "" && artifact.FindingID != expected {
		return nil, fmt.Errorf("finding_id %q does not match trusted finding %q", artifact.FindingID, expected)
	}
	if opts.RequireRunBinding {
		if artifact.ScanRunID == "" {
			return nil, errors.New("scan_run_id is required")
		}
		if expected := strings.TrimSpace(opts.ExpectedScanRunID); expected == "" || artifact.ScanRunID != expected {
			return nil, fmt.Errorf("scan_run_id %q does not match trusted scan run %q", artifact.ScanRunID, expected)
		}
		expectedOccurrenceID := strings.TrimSpace(opts.ExpectedOccurrenceID)
		if expectedOccurrenceID != "" && artifact.OccurrenceID == "" {
			return nil, errors.New("occurrence_id is required")
		}
		if artifact.OccurrenceID != expectedOccurrenceID {
			return nil, fmt.Errorf("occurrence_id %q does not match trusted occurrence %q", artifact.OccurrenceID, expectedOccurrenceID)
		}
	}
	switch artifact.Status {
	case validationStatusValidated, validationStatusFailed, validationStatusSkipped:
	default:
		return nil, fmt.Errorf("unsupported validation status %q", artifact.Status)
	}
	if artifact.Summary == "" {
		return nil, errors.New("summary is required")
	}
	if err := validateBoundedString("summary", artifact.Summary, MaxValidationSummaryBytes); err != nil {
		return nil, err
	}

	for field, target := range map[string]*string{
		"reproduction":         &artifact.Reproduction,
		"attack_path_analysis": &artifact.AttackPathAnalysis,
		"likelihood":           &artifact.Likelihood,
		"impact":               &artifact.Impact,
	} {
		if raw, ok := fields[field]; ok {
			if err := decodeJSONValue(raw, target); err != nil {
				return nil, fieldError(field, err)
			}
			*target = strings.TrimSpace(*target)
			if err := validateBoundedString(field, *target, MaxValidationTextFieldBytes); err != nil {
				return nil, err
			}
		}
	}

	for field, target := range map[string]*[]string{
		"validation_steps": &artifact.ValidationSteps,
		"assumptions":      &artifact.Assumptions,
		"controls":         &artifact.Controls,
		"blindspots":       &artifact.Blindspots,
	} {
		if raw, ok := fields[field]; ok {
			if err := decodeJSONValue(raw, target); err != nil {
				return nil, fieldError(field, err)
			}
			if err := normalizeBoundedStrings(field, target); err != nil {
				return nil, err
			}
		}
	}

	if raw, ok := fields["evidence"]; ok {
		refs, err := parseValidationEvidence(raw, opts)
		if err != nil {
			return nil, err
		}
		artifact.Evidence = ValidationArtifactEvidenceRefs(refs)
	}

	normalized, err := json.Marshal(artifact)
	if err != nil {
		return nil, fmt.Errorf("marshal normalized validation artifact: %w", err)
	}
	if len(normalized) > MaxValidationArtifactBytes {
		return nil, fmt.Errorf("normalized validation artifact exceeds %d bytes", MaxValidationArtifactBytes)
	}
	digest := sha256.Sum256(normalized)
	return &ParsedValidationArtifact{
		Artifact:       artifact,
		NormalizedJSON: normalized,
		ContentSHA256:  hex.EncodeToString(digest[:]),
	}, nil
}

func decodeStrictJSONObject(data []byte, known map[string]struct{}) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '{' {
		return nil, errors.New("validation artifact must be a JSON object")
	}

	fields := make(map[string]json.RawMessage)
	seenFolded := make(map[string]string)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("invalid JSON object key: %w", err)
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("validation artifact contains a non-string object key")
		}
		if !validStrictString(key) {
			return nil, fmt.Errorf("field name %q contains malformed Unicode", key)
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, fmt.Errorf("duplicate JSON field %q", key)
		}
		folded := strings.ToLower(key)
		if previous, duplicate := seenFolded[folded]; duplicate && previous != key {
			return nil, fmt.Errorf("ambiguous case-folded JSON fields %q and %q", previous, key)
		}
		seenFolded[folded] = key
		if canonical, alias := canonicalKnownField(key, known); alias {
			return nil, fmt.Errorf("JSON field %q must use canonical casing %q", key, canonical)
		}

		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, fmt.Errorf("invalid JSON field %q: %w", key, err)
		}
		fields[key] = raw
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("invalid JSON object: %w", err)
	}
	if token, err := decoder.Token(); err != io.EOF {
		if err != nil {
			return nil, fmt.Errorf("invalid trailing JSON: %w", err)
		}
		return nil, fmt.Errorf("trailing JSON data after validation artifact: %v", token)
	}
	return fields, nil
}

func canonicalKnownField(key string, known map[string]struct{}) (string, bool) {
	if _, ok := known[key]; ok {
		return "", false
	}
	for candidate := range known {
		if strings.EqualFold(candidate, key) {
			return candidate, true
		}
	}
	return "", false
}

func decodeRequiredField(fields map[string]json.RawMessage, name string, target any) error {
	raw, ok := fields[name]
	if !ok {
		return fmt.Errorf("%s is required", name)
	}
	if err := decodeJSONValue(raw, target); err != nil {
		return fieldError(name, err)
	}
	return nil
}

func decodeJSONValue(raw json.RawMessage, target any) error {
	if !utf8.Valid(raw) {
		return errors.New("malformed UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if token, err := decoder.Token(); err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("trailing JSON value %v", token)
	}
	return nil
}

func fieldError(name string, err error) error {
	return fmt.Errorf("invalid %s: %w", name, err)
}

func validStrictString(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsRune(value, utf8.RuneError)
}

func validateBoundedString(field, value string, maximum int) error {
	if !validStrictString(value) {
		return fmt.Errorf("%s contains malformed Unicode", field)
	}
	if len(value) > maximum {
		return fmt.Errorf("%s exceeds %d bytes", field, maximum)
	}
	return nil
}

func normalizeBoundedStrings(field string, values *[]string) error {
	if len(*values) > MaxValidationArrayItems {
		return fmt.Errorf("%s exceeds %d items", field, MaxValidationArrayItems)
	}
	out := make([]string, 0, len(*values))
	for i, value := range *values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if err := validateBoundedString(fmt.Sprintf("%s[%d]", field, i), value, MaxValidationEvidenceTextBytes); err != nil {
			return err
		}
		out = append(out, value)
	}
	*values = out
	return nil
}

func parseValidationEvidence(raw json.RawMessage, opts ValidationArtifactParseOptions) ([]store.FindingEvidenceRef, error) {
	var items []json.RawMessage
	trimmed := bytes.TrimSpace(raw)
	switch {
	case bytes.Equal(trimmed, []byte("null")), len(trimmed) == 0:
		return nil, nil
	case len(trimmed) > 0 && trimmed[0] == '[':
		if err := decodeJSONValue(trimmed, &items); err != nil {
			return nil, fieldError("evidence", err)
		}
	default:
		items = []json.RawMessage{append(json.RawMessage(nil), trimmed...)}
	}
	if len(items) > MaxValidationEvidenceItems {
		return nil, fmt.Errorf("evidence exceeds %d items", MaxValidationEvidenceItems)
	}

	refs := make([]store.FindingEvidenceRef, 0, len(items))
	for i, item := range items {
		ref, ok, err := parseValidationEvidenceItem(item)
		if err != nil {
			return nil, fmt.Errorf("invalid evidence[%d]: %w", i, err)
		}
		if !ok {
			continue
		}
		if err := normalizeValidationEvidence(&ref, opts); err != nil {
			return nil, fmt.Errorf("invalid evidence[%d]: %w", i, err)
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

func parseValidationEvidenceItem(data []byte) (store.FindingEvidenceRef, bool, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return store.FindingEvidenceRef{}, false, nil
	}
	if trimmed[0] == '"' {
		var text string
		if err := decodeJSONValue(trimmed, &text); err != nil {
			return store.FindingEvidenceRef{}, false, err
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return store.FindingEvidenceRef{}, false, nil
		}
		return store.FindingEvidenceRef{Kind: "note", Label: text}, true, nil
	}
	fields, err := decodeStrictJSONObject(trimmed, validationEvidenceKnownFields)
	if err != nil {
		return store.FindingEvidenceRef{}, false, err
	}
	var ref store.FindingEvidenceRef
	for name, target := range map[string]*string{
		"kind": &ref.Kind, "taskName": &ref.TaskName, "name": &ref.Name, "label": &ref.Label,
		"path": &ref.Path, "symbol": &ref.Symbol, "quote": &ref.Quote,
	} {
		if raw, ok := fields[name]; ok {
			if err := decodeJSONValue(raw, target); err != nil {
				return store.FindingEvidenceRef{}, false, fieldError(name, err)
			}
		}
	}
	for name, target := range map[string]*int{"startLine": &ref.StartLine, "endLine": &ref.EndLine} {
		if raw, ok := fields[name]; ok {
			if err := decodeJSONValue(raw, target); err != nil {
				return store.FindingEvidenceRef{}, false, fieldError(name, err)
			}
		}
	}
	return ref, true, nil
}

func windowsVolumeQualifiedValidationPath(value string) bool {
	if strings.HasPrefix(value, "//") {
		return true
	}
	return len(value) >= 2 && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) && value[1] == ':'
}

func cleanValidationFilePath(value string) (string, bool) {
	if windowsVolumeQualifiedValidationPath(value) {
		return "", false
	}
	cleaned := path.Clean(value)
	if windowsVolumeQualifiedValidationPath(cleaned) || cleaned == "." || cleaned == ".." ||
		strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
		return "", false
	}
	return cleaned, true
}

func normalizeValidationEvidence(ref *store.FindingEvidenceRef, opts ValidationArtifactParseOptions) error {
	ref.Kind = strings.ToLower(strings.TrimSpace(ref.Kind))
	artifactNameHasSurroundingWhitespace := ref.Name != strings.TrimSpace(ref.Name)
	ref.Name = strings.TrimSpace(ref.Name)
	ref.Label = strings.TrimSpace(ref.Label)
	// Preserve significant leading/trailing whitespace in Git paths and reject
	// ambiguous platform separators instead of rebinding them to a different path.
	if strings.Contains(ref.Path, "\\") {
		return errors.New("evidence path must not contain backslashes")
	}
	ref.Symbol = strings.TrimSpace(ref.Symbol)
	ref.Quote = strings.TrimSpace(ref.Quote)
	ref.TaskName = ""
	for field, value := range map[string]string{
		"name": ref.Name, "label": ref.Label, "path": ref.Path, "symbol": ref.Symbol, "quote": ref.Quote,
	} {
		if err := validateBoundedString(field, value, MaxValidationEvidenceTextBytes); err != nil {
			return err
		}
	}

	switch ref.Kind {
	case "note":
		if ref.Label == "" {
			return errors.New("note evidence requires label")
		}
		ref.Path, ref.Name, ref.Symbol, ref.Quote = "", "", "", ""
		ref.StartLine, ref.EndLine = 0, 0
	case "file":
		if ref.Path == "" {
			return errors.New("file evidence requires path")
		}
		cleaned, ok := cleanValidationFilePath(ref.Path)
		if !ok {
			return errors.New("file evidence path is outside repository scope")
		}
		ref.Path = cleaned
		if ref.StartLine <= 0 || ref.EndLine < ref.StartLine {
			return errors.New("file evidence requires a valid positive line range")
		}
		if opts.ValidateFileEvidence != nil {
			if err := opts.ValidateFileEvidence(*ref); err != nil {
				return err
			}
		}
	case "artifact":
		if artifactNameHasSurroundingWhitespace {
			return errors.New("artifact evidence name cannot contain surrounding whitespace")
		}
		if ref.Name == "" {
			return errors.New("artifact evidence requires name")
		}
		if ref.Path != "" || ref.StartLine != 0 || ref.EndLine != 0 || ref.Symbol != "" || ref.Quote != "" {
			return errors.New("artifact evidence cannot include file-only fields")
		}
		ref.TaskName = strings.TrimSpace(opts.TrustedTaskName)
		if ref.TaskName == "" {
			return errors.New("artifact evidence requires trusted task provenance")
		}
		if opts.ResolveArtifact == nil {
			return errors.New("artifact evidence cannot be resolved")
		}
		digest, size, err := opts.ResolveArtifact(ref.Name)
		if err != nil {
			return err
		}
		digest = strings.ToLower(strings.TrimSpace(digest))
		digest = strings.TrimPrefix(digest, "sha256:")
		if len(digest) != sha256.Size*2 || size < 0 {
			return errors.New("artifact evidence resolver returned invalid metadata")
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return errors.New("artifact evidence resolver returned invalid SHA-256 metadata")
		}
		ref.ContentSHA256 = digest
		ref.ContentSize = size
	default:
		return fmt.Errorf("unsupported evidence kind %q", ref.Kind)
	}
	return nil
}
