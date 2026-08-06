package bundle

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
)

const (
	domainManifestDocument         = "orka.security.bundle.document.manifest.v1"
	domainSemanticManifestDocument = "orka.security.bundle.document.semantic-manifest.v1"
	domainFindingsDocument         = "orka.security.bundle.document.findings.v1"
	domainCoverageDocument         = "orka.security.bundle.document.coverage.v1"
	domainThreatModelContent       = "orka.security.bundle.document.threat-model.v1"
	domainEvidence                 = "orka.security.bundle.evidence.v1"
	domainContentRoot              = "orka.security.bundle.content-root.v1"
	domainRunReceipt               = "orka.security.bundle.run-receipt.v1"
)

type manifestDocument struct {
	SchemaVersion      int                  `json:"schemaVersion"`
	Repository         RepositoryIdentity   `json:"repository"`
	Target             TargetSnapshot       `json:"target"`
	ThreatModel        threatModelDocument  `json:"threatModel"`
	Quality            QualitySummary       `json:"quality"`
	Versions           ComponentVersions    `json:"versions"`
	OccurrenceIDs      []string             `json:"occurrenceIds"`
	AssessmentIDs      []string             `json:"assessmentIds"`
	StageReceiptIDs    []string             `json:"stageReceiptIds"`
	EvidenceReceiptIDs []string             `json:"evidenceReceiptIds"`
	Metadata           map[string]string    `json:"metadata"`
	Evidence           []EvidenceDescriptor `json:"evidence"`
	Run                runEnvelopeDocument  `json:"run"`
	Digests            DigestSet            `json:"digests"`
}

type semanticManifestDocument struct {
	SchemaVersion      int                  `json:"schemaVersion"`
	Repository         RepositoryIdentity   `json:"repository"`
	Target             TargetSnapshot       `json:"target"`
	ThreatModel        threatModelDocument  `json:"threatModel"`
	Quality            QualitySummary       `json:"quality"`
	Versions           ComponentVersions    `json:"versions"`
	OccurrenceIDs      []string             `json:"occurrenceIds"`
	AssessmentIDs      []string             `json:"assessmentIds"`
	StageReceiptIDs    []string             `json:"stageReceiptIds"`
	EvidenceReceiptIDs []string             `json:"evidenceReceiptIds"`
	Metadata           map[string]string    `json:"metadata"`
	Evidence           []EvidenceDescriptor `json:"evidence"`
}

type threatModelDocument struct {
	Version       string   `json:"version"`
	Content       string   `json:"content"`
	ContentDigest string   `json:"contentDigest"`
	Scope         *string  `json:"scope"`
	Assumptions   []string `json:"assumptions"`
	Limitations   []string `json:"limitations"`
}

type runEnvelopeDocument struct {
	RunUID                   string  `json:"runUid"`
	PublicRunID              *string `json:"publicRunId"`
	Namespace                string  `json:"namespace"`
	RepositoryScanName       string  `json:"repositoryScanName"`
	RepositoryScanUID        string  `json:"repositoryScanUid"`
	RepositoryScanGeneration int64   `json:"repositoryScanGeneration"`
	StartedAt                string  `json:"startedAt"`
	CompletedAt              *string `json:"completedAt"`
	SealedAt                 string  `json:"sealedAt"`
}

type contentRootPayload struct {
	SchemaVersion    int    `json:"schemaVersion"`
	SemanticManifest string `json:"semanticManifest"`
	Findings         string `json:"findings"`
	Coverage         string `json:"coverage"`
}

type runReceiptPayload struct {
	SchemaVersion    int                 `json:"schemaVersion"`
	Run              runEnvelopeDocument `json:"run"`
	SemanticManifest string              `json:"semanticManifest"`
	Findings         string              `json:"findings"`
	Coverage         string              `json:"coverage"`
	Content          string              `json:"content"`
}

// Build validates, normalizes, canonicalizes, and digests one bundle.
func Build(input Input, requested Limits) (*Bundle, error) {
	limits, err := effectiveLimits(requested)
	if err != nil {
		return nil, err
	}

	blobs, descriptors, err := normalizeEvidenceInputs(input.Evidence, limits)
	if err != nil {
		return nil, fmt.Errorf("evidence: %w", err)
	}
	semanticManifest, run, err := normalizeManifest(input.Manifest, descriptors, limits)
	if err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	findings, err := normalizeFindings(input.Findings, limits)
	if err != nil {
		return nil, fmt.Errorf("findings: %w", err)
	}
	coverage, err := normalizeCoverage(input.Coverage, limits)
	if err != nil {
		return nil, fmt.Errorf("coverage: %w", err)
	}
	if err := validateEvidenceReferences(findings, descriptors); err != nil {
		return nil, err
	}
	if err := validateCrossDocumentReferences(semanticManifest, findings, coverage); err != nil {
		return nil, err
	}

	findingsJSON, err := marshalBoundedDocument("findings", findings, limits)
	if err != nil {
		return nil, err
	}
	coverageJSON, err := marshalBoundedDocument("coverage", coverage, limits)
	if err != nil {
		return nil, err
	}
	semanticManifestJSON, err := marshalBoundedDocument("semantic manifest", semanticManifest, limits)
	if err != nil {
		return nil, err
	}

	digests := DigestSet{
		SemanticManifest: digestParts(domainSemanticManifestDocument, semanticManifestJSON),
		Findings:         digestParts(domainFindingsDocument, findingsJSON),
		Coverage:         digestParts(domainCoverageDocument, coverageJSON),
	}
	contentRootJSON, err := canonicalJSON(contentRootPayload{
		SchemaVersion:    SchemaVersion,
		SemanticManifest: digests.SemanticManifest,
		Findings:         digests.Findings,
		Coverage:         digests.Coverage,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal content root: %w", err)
	}
	digests.Content = digestParts(domainContentRoot, contentRootJSON)

	runReceiptJSON, err := canonicalJSON(runReceiptPayload{
		SchemaVersion:    SchemaVersion,
		Run:              run,
		SemanticManifest: digests.SemanticManifest,
		Findings:         digests.Findings,
		Coverage:         digests.Coverage,
		Content:          digests.Content,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal run receipt: %w", err)
	}
	digests.RunReceipt = digestParts(domainRunReceipt, runReceiptJSON)

	manifest := manifestFromSemantic(semanticManifest, run, digests)
	digests.Manifest, err = manifestDocumentDigest(manifest, limits)
	if err != nil {
		return nil, err
	}
	manifest.Digests = digests
	manifestJSON, err := marshalBoundedDocument("manifest", manifest, limits)
	if err != nil {
		return nil, err
	}

	return &Bundle{
		ManifestJSON: cloneBytes(manifestJSON),
		FindingsJSON: cloneBytes(findingsJSON),
		CoverageJSON: cloneBytes(coverageJSON),
		Evidence:     cloneEvidenceBlobs(blobs),
		Roots: RootDigests{
			ContentDigest:    digests.Content,
			RunReceiptDigest: digests.RunReceipt,
		},
	}, nil
}

func manifestFromSemantic(semantic semanticManifestDocument, run runEnvelopeDocument, digests DigestSet) manifestDocument {
	return manifestDocument{
		SchemaVersion:      semantic.SchemaVersion,
		Repository:         semantic.Repository,
		Target:             semantic.Target,
		ThreatModel:        semantic.ThreatModel,
		Quality:            semantic.Quality,
		Versions:           semantic.Versions,
		OccurrenceIDs:      semantic.OccurrenceIDs,
		AssessmentIDs:      semantic.AssessmentIDs,
		StageReceiptIDs:    semantic.StageReceiptIDs,
		EvidenceReceiptIDs: semantic.EvidenceReceiptIDs,
		Metadata:           semantic.Metadata,
		Evidence:           semantic.Evidence,
		Run:                run,
		Digests:            digests,
	}
}

// manifestDocumentDigest excludes only the manifest's own digest field. All
// other document and root digests remain bound into the published manifest.
func manifestDocumentDigest(manifest manifestDocument, limits Limits) (string, error) {
	manifest.Digests.Manifest = ""
	data, err := marshalBoundedDocument("manifest digest input", manifest, limits)
	if err != nil {
		return "", err
	}
	return digestParts(domainManifestDocument, data), nil
}

func marshalBoundedDocument(name string, value any, limits Limits) ([]byte, error) {
	data, err := canonicalJSON(value)
	if err != nil {
		return nil, fmt.Errorf("marshal %s: %w", name, err)
	}
	if len(data) > limits.MaxDocumentBytes {
		return nil, fmt.Errorf("%s exceeds maximum size of %d bytes", name, limits.MaxDocumentBytes)
	}
	return data, nil
}

func canonicalJSON(value any) ([]byte, error) {
	// encoding/json emits struct fields in declaration order and sorts string map
	// keys. Inputs are normalized first, so this is the package's canonical JSON
	// encoding: UTF-8, no insignificant whitespace, and no trailing newline.
	return json.Marshal(value)
}

func digestParts(domain string, parts ...[]byte) string {
	h := sha256.New()
	writeDigestPart(h, []byte(domain))
	for _, part := range parts {
		writeDigestPart(h, part)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

func writeDigestPart(h hash.Hash, part []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(part)))
	_, _ = h.Write(length[:])
	_, _ = h.Write(part)
}

func cloneBytes(data []byte) []byte {
	if data == nil {
		return nil
	}
	return append([]byte(nil), data...)
}

func cloneEvidenceBlobs(blobs []EvidenceBlob) []EvidenceBlob {
	if blobs == nil {
		return nil
	}
	out := make([]EvidenceBlob, len(blobs))
	for i := range blobs {
		out[i] = blobs[i]
		out[i].Data = cloneBytes(blobs[i].Data)
	}
	return out
}
