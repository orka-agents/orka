package bundle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"time"
)

// Verify validates canonical document bytes, immutable evidence, internal
// document/root digests, and the controller-owned external roots.
//
//nolint:gocyclo // verifier checks each canonical document and digest boundary explicitly
func Verify(bundle *Bundle, requested Limits) error {
	if bundle == nil {
		return fmt.Errorf("bundle is required")
	}
	limits, err := effectiveLimits(requested)
	if err != nil {
		return err
	}
	if err := checkDocumentSize("manifest", bundle.ManifestJSON, limits); err != nil {
		return err
	}
	if err := checkDocumentSize("findings", bundle.FindingsJSON, limits); err != nil {
		return err
	}
	if err := checkDocumentSize("coverage", bundle.CoverageJSON, limits); err != nil {
		return err
	}
	if err := preflightFindingsArrayCardinality(bundle.FindingsJSON, limits); err != nil {
		return fmt.Errorf("preflight findings: %w", err)
	}
	if err := preflightCoverageArrayCardinality(bundle.CoverageJSON, limits); err != nil {
		return fmt.Errorf("preflight coverage: %w", err)
	}

	blobs, descriptors, err := normalizeEvidenceBundle(bundle.Evidence, limits)
	if err != nil {
		return fmt.Errorf("evidence: %w", err)
	}
	if err := compareEvidenceBundle(bundle.Evidence, blobs); err != nil {
		return err
	}

	var manifest manifestDocument
	if err := decodeStrict(bundle.ManifestJSON, &manifest); err != nil {
		return fmt.Errorf("decode manifest: %w", err)
	}
	manifestInput, err := manifestInputFromDocument(manifest)
	if err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	semanticManifest, run, err := normalizeManifest(manifestInput, descriptors, limits)
	if err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	if manifest.ThreatModel.ContentDigest != semanticManifest.ThreatModel.ContentDigest {
		return fmt.Errorf("threat model content digest mismatch")
	}

	var findings FindingsInput
	if err := decodeStrict(bundle.FindingsJSON, &findings); err != nil {
		return fmt.Errorf("decode findings: %w", err)
	}
	findings, err = normalizeFindings(findings, limits)
	if err != nil {
		return fmt.Errorf("findings: %w", err)
	}
	canonicalFindings, err := marshalBoundedDocument("findings", findings, limits)
	if err != nil {
		return err
	}
	if !bytes.Equal(canonicalFindings, bundle.FindingsJSON) {
		return fmt.Errorf("findings document is not canonical")
	}
	if err := validateEvidenceReferences(findings, descriptors); err != nil {
		return err
	}

	var coverage CoverageInput
	if err := decodeStrict(bundle.CoverageJSON, &coverage); err != nil {
		return fmt.Errorf("decode coverage: %w", err)
	}
	coverage, err = normalizeCoverage(coverage, limits)
	if err != nil {
		return fmt.Errorf("coverage: %w", err)
	}
	canonicalCoverage, err := marshalBoundedDocument("coverage", coverage, limits)
	if err != nil {
		return err
	}
	if !bytes.Equal(canonicalCoverage, bundle.CoverageJSON) {
		return fmt.Errorf("coverage document is not canonical")
	}
	if err := validateCrossDocumentReferences(semanticManifest, findings, coverage); err != nil {
		return err
	}

	semanticManifestJSON, err := marshalBoundedDocument("semantic manifest", semanticManifest, limits)
	if err != nil {
		return err
	}
	expected := DigestSet{
		SemanticManifest: digestParts(domainSemanticManifestDocument, semanticManifestJSON),
		Findings:         digestParts(domainFindingsDocument, canonicalFindings),
		Coverage:         digestParts(domainCoverageDocument, canonicalCoverage),
	}
	contentRootJSON, err := canonicalJSON(contentRootPayload{
		SchemaVersion:    SchemaVersion,
		SemanticManifest: expected.SemanticManifest,
		Findings:         expected.Findings,
		Coverage:         expected.Coverage,
	})
	if err != nil {
		return fmt.Errorf("marshal content root: %w", err)
	}
	expected.Content = digestParts(domainContentRoot, contentRootJSON)
	runReceiptJSON, err := canonicalJSON(runReceiptPayload{
		SchemaVersion:    SchemaVersion,
		Run:              run,
		SemanticManifest: expected.SemanticManifest,
		Findings:         expected.Findings,
		Coverage:         expected.Coverage,
		Content:          expected.Content,
	})
	if err != nil {
		return fmt.Errorf("marshal run receipt: %w", err)
	}
	expected.RunReceipt = digestParts(domainRunReceipt, runReceiptJSON)

	manifestForDigest := manifestFromSemantic(semanticManifest, run, expected)
	expected.Manifest, err = manifestDocumentDigest(manifestForDigest, limits)
	if err != nil {
		return err
	}
	manifestForDigest.Digests = expected
	canonicalManifest, err := marshalBoundedDocument("manifest", manifestForDigest, limits)
	if err != nil {
		return err
	}
	if !bytes.Equal(canonicalManifest, bundle.ManifestJSON) {
		return fmt.Errorf("manifest document or digest mismatch")
	}
	if manifest.Digests != expected {
		return fmt.Errorf("manifest digest set mismatch")
	}
	if bundle.Roots.ContentDigest != expected.Content {
		return fmt.Errorf("content root mismatch")
	}
	if bundle.Roots.RunReceiptDigest != expected.RunReceipt {
		return fmt.Errorf("run receipt root mismatch")
	}
	return nil
}

func checkDocumentSize(name string, data []byte, limits Limits) error {
	if len(data) == 0 {
		return fmt.Errorf("%s document is empty", name)
	}
	if len(data) > limits.MaxDocumentBytes {
		return fmt.Errorf("%s exceeds maximum size of %d bytes", name, limits.MaxDocumentBytes)
	}
	return nil
}

func preflightFindingsArrayCardinality(data []byte, limits Limits) error {
	return preflightTopLevelArrayCardinalities(data, map[string]int{
		"findings": min(limits.MaxFindings, limits.MaxArrayItems),
	}, 0)
}

func preflightCoverageArrayCardinality(data []byte, limits Limits) error {
	return preflightTopLevelArrayCardinalities(data, map[string]int{
		"inventory":  limits.MaxArrayItems,
		"candidates": limits.MaxArrayItems,
		"stages":     limits.MaxArrayItems,
	}, limits.MaxCoverageEntries)
}

func preflightTopLevelArrayCardinalities(data []byte, arrayLimits map[string]int, totalLimit int) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := opening.(json.Delim); !ok || delimiter != '{' {
		return fmt.Errorf("document must be a JSON object")
	}

	total := 0
	for decoder.More() {
		fieldToken, err := decoder.Token()
		if err != nil {
			return err
		}
		field, ok := fieldToken.(string)
		if !ok {
			return fmt.Errorf("top-level object field name is not a string")
		}
		valueToken, err := decoder.Token()
		if err != nil {
			return err
		}
		maxItems, tracked := arrayLimits[field]
		if !tracked {
			if err := skipJSONValue(decoder, valueToken); err != nil {
				return err
			}
			continue
		}

		count, err := countJSONArrayItems(decoder, valueToken, field, maxItems, totalLimit-total, totalLimit)
		if err != nil {
			return err
		}
		total += count
	}
	closing, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != '}' {
		return fmt.Errorf("document has an invalid closing delimiter")
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func countJSONArrayItems(
	decoder *json.Decoder,
	opening json.Token,
	field string,
	maxItems, remainingTotal, totalLimit int,
) (int, error) {
	if opening == nil {
		return 0, nil
	}
	delimiter, ok := opening.(json.Delim)
	if !ok || delimiter != '[' {
		return 0, fmt.Errorf("top-level field %q must be an array or null", field)
	}

	count := 0
	for decoder.More() {
		count++
		if count > maxItems {
			return 0, fmt.Errorf("top-level array %q exceeds maximum item count of %d", field, maxItems)
		}
		if totalLimit > 0 && count > remainingTotal {
			return 0, fmt.Errorf("top-level arrays exceed maximum total item count of %d", totalLimit)
		}
		itemToken, err := decoder.Token()
		if err != nil {
			return 0, err
		}
		if err := skipJSONValue(decoder, itemToken); err != nil {
			return 0, err
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return 0, err
	}
	if delimiter, ok := closing.(json.Delim); !ok || delimiter != ']' {
		return 0, fmt.Errorf("top-level array %q has an invalid closing delimiter", field)
	}
	return count, nil
}

func skipJSONValue(decoder *json.Decoder, first json.Token) error {
	delimiter, ok := first.(json.Delim)
	if !ok {
		return nil
	}
	if delimiter != '{' && delimiter != '[' {
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	depth := 1
	for depth > 0 {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			continue
		}
		switch delimiter {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		default:
			return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
		}
	}
	return nil
}

func normalizeEvidenceBundle(input []EvidenceBlob, limits Limits) ([]EvidenceBlob, []EvidenceDescriptor, error) {
	if input == nil {
		return normalizeEvidenceInputs(nil, limits)
	}
	converted := make([]EvidenceBlobInput, len(input))
	for i := range input {
		converted[i] = EvidenceBlobInput{Name: input[i].Name, MediaType: input[i].MediaType, Data: input[i].Data}
	}
	return normalizeEvidenceInputs(converted, limits)
}

func compareEvidenceBundle(actual, expected []EvidenceBlob) error {
	if (actual == nil) != (expected == nil) || len(actual) != len(expected) {
		return fmt.Errorf("evidence blob set mismatch")
	}
	for i := range expected {
		if actual[i].Name != expected[i].Name || actual[i].MediaType != expected[i].MediaType ||
			actual[i].Size != expected[i].Size || actual[i].Digest != expected[i].Digest ||
			!bytes.Equal(actual[i].Data, expected[i].Data) {
			return fmt.Errorf("evidence blob %q digest or metadata mismatch", expected[i].Name)
		}
	}
	return nil
}

func manifestInputFromDocument(manifest manifestDocument) (ManifestInput, error) {
	startedAt, err := parseCanonicalTime("run.startedAt", manifest.Run.StartedAt)
	if err != nil {
		return ManifestInput{}, err
	}
	completedAt, err := parseCanonicalOptionalTime("run.completedAt", manifest.Run.CompletedAt)
	if err != nil {
		return ManifestInput{}, err
	}
	sealedAt, err := parseCanonicalTime("run.sealedAt", manifest.Run.SealedAt)
	if err != nil {
		return ManifestInput{}, err
	}
	return ManifestInput{
		SchemaVersion: manifest.SchemaVersion,
		Repository:    manifest.Repository,
		Target:        manifest.Target,
		ThreatModel: ThreatModelInput{
			Version:     manifest.ThreatModel.Version,
			Content:     manifest.ThreatModel.Content,
			Scope:       manifest.ThreatModel.Scope,
			Assumptions: manifest.ThreatModel.Assumptions,
			Limitations: manifest.ThreatModel.Limitations,
		},
		Quality:            manifest.Quality,
		Versions:           manifest.Versions,
		OccurrenceIDs:      manifest.OccurrenceIDs,
		AssessmentIDs:      manifest.AssessmentIDs,
		StageReceiptIDs:    manifest.StageReceiptIDs,
		EvidenceReceiptIDs: manifest.EvidenceReceiptIDs,
		Metadata:           manifest.Metadata,
		Run: RunEnvelope{
			RunUID:                   manifest.Run.RunUID,
			PublicRunID:              manifest.Run.PublicRunID,
			Namespace:                manifest.Run.Namespace,
			RepositoryScanName:       manifest.Run.RepositoryScanName,
			RepositoryScanUID:        manifest.Run.RepositoryScanUID,
			RepositoryScanGeneration: manifest.Run.RepositoryScanGeneration,
			StartedAt:                startedAt,
			CompletedAt:              completedAt,
			SealedAt:                 sealedAt,
		},
	}, nil
}

func parseCanonicalTime(name, value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s is invalid: %w", name, err)
	}
	if parsed.UTC().Format(time.RFC3339Nano) != value {
		return time.Time{}, fmt.Errorf("%s is not canonical UTC", name)
	}
	return parsed, nil
}

func parseCanonicalOptionalTime(name string, value *string) (*time.Time, error) {
	if value == nil {
		return nil, nil
	}
	parsed, err := parseCanonicalTime(name, *value)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func evidenceBlobsEqual(left, right []EvidenceBlob) bool {
	if (left == nil) != (right == nil) || len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].Name != right[i].Name || left[i].MediaType != right[i].MediaType ||
			left[i].Size != right[i].Size || left[i].Digest != right[i].Digest ||
			!slices.Equal(left[i].Data, right[i].Data) {
			return false
		}
	}
	return true
}
