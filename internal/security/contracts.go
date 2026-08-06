package security

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/orka-agents/orka/internal/store"
)

const (
	ArtifactSlices          = "security-slices.json"
	ArtifactFindingsV2      = "security-findings.v2.json"
	ArtifactDroppedFindings = "security-dropped-findings.json"

	EnvRepositoryScanName   = "ORKA_SECURITY_REPOSITORY_SCAN"
	EnvReviewSliceJSON      = "ORKA_SECURITY_REVIEW_SLICE_JSON"
	EnvStage                = "ORKA_SECURITY_STAGE"
	EnvScanID               = "ORKA_SECURITY_SCAN_ID"
	EnvSliceID              = "ORKA_SECURITY_SLICE_ID"
	EnvFindingID            = "ORKA_SECURITY_FINDING_ID"
	EnvOccurrenceID         = "ORKA_SECURITY_OCCURRENCE_ID"
	EnvPatchBranch          = "ORKA_SECURITY_PATCH_BRANCH"
	EnvScanBaseCommit       = "ORKA_SECURITY_SCAN_BASE_COMMIT"
	EnvScanHeadCommit       = "ORKA_SECURITY_SCAN_HEAD_COMMIT"
	EnvScannerPolicyVersion = "ORKA_SECURITY_SCANNER_POLICY_VERSION"
	EnvPolicyDigest         = "ORKA_SECURITY_POLICY_DIGEST"
	EnvPolicyProvenance     = "ORKA_SECURITY_POLICY_PROVENANCE"

	ScannerPolicyVersion = "2026-06-orka-fp-policy-v1"

	// SchemaVersionReviewSlices is the legacy mapper artifact schema. Keep this
	// value stable for compatibility with existing producers and stored fixtures.
	SchemaVersionReviewSlices = 1
	// SchemaVersionReviewSlicesV2 adds coverage-accountable file inventories.
	SchemaVersionReviewSlicesV2 = 2
	SchemaVersionReviewContext  = 1
	SchemaVersionFindingsV2     = 2
	SchemaVersionPatchSummary   = 1

	MapperCoverageUnknown     = "unknown"
	MapperCoverageAccountable = "accountable"
	MapperCoveragePartial     = "partial"

	MapperCoverageReasonInventoryEntryLimit = "mapper_inventory_entry_limit"

	MapperDispositionReviewable = "reviewable"
	MapperDispositionExcluded   = "excluded"
	MapperDispositionAssigned   = "assigned"
	MapperDispositionOmitted    = "omitted"

	MapperTreeDispositionRegular   = "regular"
	MapperTreeDispositionSymlink   = "symlink"
	MapperTreeDispositionSubmodule = "submodule"
	MapperTreeDispositionLFS       = "lfs"

	// MaxMapperInventoryEntries bounds the number of repository paths retained
	// in mapper inventories. Producers may count additional paths, but must
	// summarize them as truncation metadata rather than emitting one row per
	// excess path.
	MaxMapperInventoryEntries = 10000
	// A retained path may have a bounded set of distinct omission reasons
	// (unassigned, entrypoint, context, and test caps).
	MaxMapperOmittedInventoryEntries = MaxMapperInventoryEntries * 4
	MaxMapperReviewSlices            = MaxMapperInventoryEntries
	MaxMapperTreeIndexEntries        = 10000
	MaxFindingsIdentitySlugBytes     = 128

	BundleMetadataAuthorizationRepoURL        = "authorization.repoURL"
	BundleMetadataAuthorizationBranch         = "authorization.branch"
	BundleMetadataAuthorizationRef            = "authorization.ref"
	BundleMetadataAuthorizationAgentName      = "authorization.agentName"
	BundleMetadataAuthorizationAgentNamespace = "authorization.agentNamespace"
)

type ChangedLineRange = store.ChangedLineRange

// ReviewSlicesArtifact is the deterministic mapper output.
type ReviewSlicesArtifact struct {
	SchemaVersion        int                        `json:"schemaVersion"`
	CoverageStatus       string                     `json:"coverageStatus,omitempty"`
	BaseCommit           string                     `json:"baseCommit,omitempty"`
	HeadCommit           string                     `json:"headCommit,omitempty"`
	ChangedFilesComputed bool                       `json:"changedFilesComputed,omitempty"`
	ChangedFiles         []string                   `json:"changedFiles,omitempty"`
	ChangedLineRanges    []ChangedLineRange         `json:"changedLineRanges,omitempty"`
	DiffSummary          string                     `json:"diffSummary,omitempty"`
	ChangedFilesError    string                     `json:"changedFilesError,omitempty"`
	CoverageReasonCodes  []string                   `json:"coverageReasonCodes,omitempty"`
	InventorySummary     *MapperInventorySummary    `json:"inventorySummary,omitempty"`
	DiscoveredFiles      []MapperFileInventoryEntry `json:"discoveredFiles"`
	ReviewableFiles      []MapperFileInventoryEntry `json:"reviewableFiles"`
	OmittedFiles         []MapperFileInventoryEntry `json:"omittedFiles"`
	TargetReceipt        *MapperTargetReceipt       `json:"targetReceipt,omitempty"`
	Slices               []store.ReviewSlice        `json:"slices"`
}

// MapperInventorySummary truthfully accounts for repository paths beyond the
// retained inventory bound without materializing one record per excess path.
// Truncated is the conservative aggregate across path inventory and the
// separately-accounted omission-record collection.
type MapperInventorySummary struct {
	EntryLimit       int                          `json:"entryLimit"`
	TotalEntries     int                          `json:"totalEntries"`
	RetainedEntries  int                          `json:"retainedEntries"`
	TruncatedEntries int                          `json:"truncatedEntries"`
	OmissionRecords  *MapperOmissionRecordSummary `json:"omissionRecords,omitempty"`
	Truncated        bool                         `json:"truncated"`
	Reason           string                       `json:"reason,omitempty"`
}

// MapperOmissionRecordSummary accounts for bounded omission ledger records.
// Multiple records may refer to one repository path when distinct deterministic
// reasons (for example context and test caps) apply, so these counts must never
// be folded into MapperInventorySummary's repository path totals.
type MapperOmissionRecordSummary struct {
	EntryLimit       int  `json:"entryLimit"`
	TotalRecords     int  `json:"totalRecords"`
	RetainedRecords  int  `json:"retainedRecords"`
	TruncatedRecords int  `json:"truncatedRecords"`
	Truncated        bool `json:"truncated"`
}

// MapperFileInventoryEntry records one deterministic mapper disposition. The
// same path may appear more than once in omittedFiles when distinct bounded
// references (for example context and test references) were omitted.
type MapperFileInventoryEntry struct {
	Path        string `json:"path"`
	Disposition string `json:"disposition"`
	Reason      string `json:"reason"`
}

// MapperTargetReceipt attests the exact clean Git snapshot inspected by the
// trusted mapper. TreeIndex is bounded while TreeDigest covers the complete
// canonical git ls-tree stream.
type MapperTargetReceipt struct {
	HeadOID              string                 `json:"headOID"`
	RequestedBranch      string                 `json:"requestedBranch,omitempty"`
	RequestedRef         string                 `json:"requestedRef,omitempty"`
	BaseRef              string                 `json:"baseRef,omitempty"`
	CleanTrackedWorktree bool                   `json:"cleanTrackedWorktree"`
	ObjectFormat         string                 `json:"objectFormat"`
	TreeOID              string                 `json:"treeOID"`
	TreeDigest           string                 `json:"treeDigest"`
	SnapshotDigest       string                 `json:"snapshotDigest"`
	TreeEntryCount       int                    `json:"treeEntryCount"`
	TreeIndexTruncated   bool                   `json:"treeIndexTruncated"`
	TreeIndex            []MapperTreeIndexEntry `json:"treeIndex"`
}

// MapperTreeIndexEntry records one leaf entry from git ls-tree -r.
type MapperTreeIndexEntry struct {
	Path        string `json:"path"`
	Mode        string `json:"mode"`
	Type        string `json:"type"`
	ObjectID    string `json:"objectID"`
	Disposition string `json:"disposition"`
	ContentSize int64  `json:"contentSize,omitempty"`
	LineCount   int    `json:"lineCount,omitempty"`
}

type ReviewContextManifest struct {
	SchemaVersion     int                         `json:"schemaVersion"`
	SliceID           string                      `json:"sliceId"`
	ChangedFiles      []string                    `json:"changedFiles,omitempty"`
	ChangedLineRanges []ChangedLineRange          `json:"changedLineRanges,omitempty"`
	IncludedFiles     []ReviewContextIncludedFile `json:"includedFiles"`
	OmittedFiles      []ReviewContextOmittedFile  `json:"omittedFiles,omitempty"`
	PromptBytes       int                         `json:"promptBytes"`
	ApproximateTokens int                         `json:"approximateTokens"`
}

type ReviewContextIncludedFile struct {
	Path               string                   `json:"path"`
	Role               string                   `json:"role"`
	Bytes              int                      `json:"bytes"`
	IncludedBytes      int                      `json:"includedBytes"`
	IncludedLineRanges []ReviewContextLineRange `json:"includedLineRanges"`
	Excerpt            string                   `json:"excerpt,omitempty"`
	Truncated          bool                     `json:"truncated"`
	Readable           bool                     `json:"readable"`
	SkippedReason      *string                  `json:"skippedReason"`
}

type ReviewContextLineRange struct {
	StartLine int `json:"startLine"`
	EndLine   int `json:"endLine"`
}

type ReviewContextOmittedFile struct {
	Path   string `json:"path"`
	Role   string `json:"role"`
	Reason string `json:"reason"`
}

// FindingsV2Artifact captures evidence-backed slice review output.
type FindingsV2Artifact struct {
	SchemaVersion int                  `json:"schemaVersion"`
	Repository    FindingsV2Repository `json:"repository"`
	Scan          FindingsV2Scan       `json:"scan"`
	Findings      []FindingsV2Finding  `json:"findings"`
}

type FindingsV2Repository struct {
	RepoURL string `json:"repoURL"`
	Branch  string `json:"branch"`
	SubPath string `json:"subPath"`
	BaseSHA string `json:"baseSHA"`
	HeadSHA string `json:"headSHA"`
}

type FindingsV2Scan struct {
	Mode    string `json:"mode"`
	SliceID string `json:"sliceId"`
	Summary string `json:"summary"`
}

type FindingsV2Finding struct {
	RuleID                        string                  `json:"ruleId,omitempty"`
	Identity                      *FindingsV2Identity     `json:"identity,omitempty"`
	Title                         string                  `json:"title"`
	Category                      string                  `json:"category"`
	Severity                      string                  `json:"severity"`
	Confidence                    string                  `json:"confidence"`
	Triage                        string                  `json:"triage"`
	Evidence                      []FindingsV2EvidenceRef `json:"evidence"`
	Summary                       string                  `json:"summary"`
	RootCause                     string                  `json:"rootCause"`
	Reproduction                  string                  `json:"reproduction"`
	Remediation                   string                  `json:"remediation"`
	SuggestedAction               string                  `json:"suggestedAction"`
	WhyTestsDoNotAlreadyCoverThis string                  `json:"whyTestsDoNotAlreadyCoverThis"`
	SuggestedRegressionTest       string                  `json:"suggestedRegressionTest"`
	MinimumFixScope               string                  `json:"minimumFixScope"`
}

// FindingsV2Identity is a producer-proposed semantic identity. Orka remains
// authoritative for reconciliation and may ignore or replace these values.
type FindingsV2Identity struct {
	Anchor           string `json:"anchor,omitempty"`
	Instance         string `json:"instance,omitempty"`
	AlgorithmVersion string `json:"algorithmVersion,omitempty"`
}

type FindingsV2EvidenceRef struct {
	Path      string  `json:"path"`
	StartLine int     `json:"startLine"`
	EndLine   int     `json:"endLine"`
	Symbol    *string `json:"symbol"`
	Quote     *string `json:"quote"`
}

type DroppedFindingArtifact struct {
	SchemaVersion int                        `json:"schemaVersion"`
	Dropped       []DroppedFindingDiagnostic `json:"dropped"`
}

type DroppedFindingDiagnostic struct {
	Index  int    `json:"index"`
	Reason string `json:"reason"`
	Sample any    `json:"sample,omitempty"`
	Layer  string `json:"layer"`
}

type PatchSummaryArtifact struct {
	SchemaVersion int            `json:"schemaVersion"`
	FindingID     string         `json:"findingId"`
	Summary       string         `json:"summary"`
	ChangedFiles  []string       `json:"changedFiles"`
	TestsRun      []PatchTestRun `json:"testsRun,omitempty"`
	Risk          string         `json:"risk"`
}

type PatchTestRun struct {
	Command  string `json:"command"`
	ExitCode int    `json:"exitCode"`
}

// ReviewContextArtifactName returns the context manifest artifact for a slice.
func ReviewContextArtifactName(sliceID string) string {
	return fmt.Sprintf("security-review-context-%s.json", sanitizeName(sliceID))
}

// ParseReviewSlicesArtifact parses and minimally validates mapper output.
// Schema v1 remains compatible but is always classified coverage-unknown.
// Schema v2 is accepted only when its inventories account for every discovered
// and reviewable path.
func ParseReviewSlicesArtifact(data []byte) (*ReviewSlicesArtifact, error) {
	if err := validateMapperJSONCollectionBounds(data); err != nil {
		return nil, err
	}
	var artifact ReviewSlicesArtifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		return nil, err
	}
	switch artifact.SchemaVersion {
	case SchemaVersionReviewSlices:
		artifact.CoverageStatus = MapperCoverageUnknown
	case SchemaVersionReviewSlicesV2:
		if artifact.CoverageStatus != MapperCoverageAccountable && artifact.CoverageStatus != MapperCoveragePartial {
			return nil, fmt.Errorf(
				"schemaVersion %d requires coverageStatus %q or %q",
				artifact.SchemaVersion,
				MapperCoverageAccountable,
				MapperCoveragePartial,
			)
		}
	default:
		return nil, fmt.Errorf("unsupported security slices schemaVersion %d", artifact.SchemaVersion)
	}
	if err := validateMapperCollectionBounds(artifact); err != nil {
		return nil, err
	}
	for _, file := range artifact.ChangedFiles {
		if !SafeRepoPath(file) {
			return nil, fmt.Errorf("changed file path %q is not repo-relative and safe", file)
		}
	}
	for _, lineRange := range artifact.ChangedLineRanges {
		if !validChangedLineRange(lineRange) {
			return nil, fmt.Errorf("changed line range for %q is invalid", lineRange.Path)
		}
	}
	for i := range artifact.Slices {
		if err := validateReviewSliceContract(artifact.Slices[i]); err != nil {
			return nil, fmt.Errorf("slice %d: %w", i, err)
		}
	}
	if artifact.SchemaVersion == SchemaVersionReviewSlicesV2 {
		if err := validateMapperTargetReceipt(artifact); err != nil {
			return nil, err
		}
		if err := validateMapperInventories(artifact); err != nil {
			return nil, err
		}
		if err := validateMapperInventorySummary(artifact); err != nil {
			return nil, err
		}
	}
	return &artifact, nil
}

func validateMapperJSONCollectionBounds(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token != json.Delim('{') {
		return fmt.Errorf("security slices artifact must be a JSON object")
	}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return fmt.Errorf("security slices artifact object key is not a string")
		}
		if name, limit, ok := mapperJSONCollectionBound(key); ok {
			if err := skipBoundedJSONArray(decoder, name, limit); err != nil {
				return err
			}
			continue
		}
		if strings.EqualFold(key, "targetReceipt") {
			if err := validateMapperTargetReceiptJSONBounds(decoder); err != nil {
				return err
			}
			continue
		}
		if err := skipJSONValue(decoder); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("security slices artifact contains trailing JSON values")
		}
		return err
	}
	return nil
}

func mapperJSONCollectionBound(key string) (string, int, bool) {
	// encoding/json uses Unicode case folding when matching object keys to
	// struct fields. Use the same matching semantics here so aliases such as
	// the long-s form of "slices" cannot bypass the streaming allocation bound.
	switch {
	case strings.EqualFold(key, "discoveredFiles"):
		return "discoveredFiles", MaxMapperInventoryEntries, true
	case strings.EqualFold(key, "reviewableFiles"):
		return "reviewableFiles", MaxMapperInventoryEntries, true
	case strings.EqualFold(key, "omittedFiles"):
		return "omittedFiles", MaxMapperOmittedInventoryEntries, true
	case strings.EqualFold(key, "slices"):
		return "slices", MaxMapperReviewSlices, true
	default:
		return "", 0, false
	}
}

func validateMapperTargetReceiptJSONBounds(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token == nil {
		return nil
	}
	if token != json.Delim('{') {
		return fmt.Errorf("targetReceipt must be an object")
	}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return fmt.Errorf("targetReceipt object key is not a string")
		}
		if strings.EqualFold(key, "treeIndex") {
			if err := skipBoundedJSONArray(decoder, "targetReceipt.treeIndex", MaxMapperTreeIndexEntries); err != nil {
				return err
			}
			continue
		}
		if err := skipJSONValue(decoder); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

func skipBoundedJSONArray(decoder *json.Decoder, name string, limit int) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token == nil {
		return nil
	}
	if token != json.Delim('[') {
		return fmt.Errorf("%s must be an array", name)
	}
	count := 0
	for decoder.More() {
		count++
		if count > limit {
			return fmt.Errorf("%s exceeds %d entries", name, limit)
		}
		if err := skipJSONValue(decoder); err != nil {
			return err
		}
	}
	_, err = decoder.Token()
	return err
}

func skipJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		for decoder.More() {
			if _, err := decoder.Token(); err != nil {
				return err
			}
			if err := skipJSONValue(decoder); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := skipJSONValue(decoder); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
	_, err = decoder.Token()
	return err
}

func validateMapperCollectionBounds(artifact ReviewSlicesArtifact) error {
	limits := []struct {
		name  string
		count int
		limit int
	}{
		{name: "discoveredFiles", count: len(artifact.DiscoveredFiles), limit: MaxMapperInventoryEntries},
		{name: "reviewableFiles", count: len(artifact.ReviewableFiles), limit: MaxMapperInventoryEntries},
		{name: "omittedFiles", count: len(artifact.OmittedFiles), limit: MaxMapperOmittedInventoryEntries},
		{name: "slices", count: len(artifact.Slices), limit: MaxMapperReviewSlices},
	}
	for _, item := range limits {
		if item.count > item.limit {
			return fmt.Errorf("%s exceeds %d entries", item.name, item.limit)
		}
	}
	return nil
}

func validateMapperTargetReceipt(artifact ReviewSlicesArtifact) error {
	receipt := artifact.TargetReceipt
	if receipt == nil {
		return fmt.Errorf("schemaVersion %d requires targetReceipt", artifact.SchemaVersion)
	}
	objectIDLength := 0
	switch receipt.ObjectFormat {
	case "sha1":
		objectIDLength = 40
	case "sha256":
		objectIDLength = 64
	default:
		return fmt.Errorf("targetReceipt objectFormat %q is unsupported", receipt.ObjectFormat)
	}
	if !fullHexObjectID(receipt.HeadOID, objectIDLength) {
		return fmt.Errorf("targetReceipt headOID must be a full %s object ID", receipt.ObjectFormat)
	}
	if artifact.HeadCommit != receipt.HeadOID {
		return fmt.Errorf("headCommit %q does not match targetReceipt headOID", artifact.HeadCommit)
	}
	if !receipt.CleanTrackedWorktree {
		return fmt.Errorf("targetReceipt tracked worktree is not clean")
	}
	if !fullHexObjectID(receipt.TreeOID, objectIDLength) {
		return fmt.Errorf("targetReceipt treeOID must be a full %s object ID", receipt.ObjectFormat)
	}
	if !sha256Digest(receipt.TreeDigest) {
		return fmt.Errorf("targetReceipt treeDigest must be a sha256 digest")
	}
	if !sha256Digest(receipt.SnapshotDigest) {
		return fmt.Errorf("targetReceipt snapshotDigest must be a sha256 digest")
	}
	if receipt.TreeIndex == nil {
		return fmt.Errorf("targetReceipt treeIndex must be an array")
	}
	if len(receipt.TreeIndex) > MaxMapperTreeIndexEntries {
		return fmt.Errorf("targetReceipt treeIndex exceeds %d entries", MaxMapperTreeIndexEntries)
	}
	if receipt.TreeEntryCount < len(receipt.TreeIndex) {
		return fmt.Errorf("targetReceipt treeEntryCount is smaller than treeIndex")
	}
	if receipt.TreeIndexTruncated {
		if receipt.TreeEntryCount <= len(receipt.TreeIndex) {
			return fmt.Errorf("targetReceipt truncated treeIndex must omit at least one entry")
		}
	} else if receipt.TreeEntryCount != len(receipt.TreeIndex) {
		return fmt.Errorf("targetReceipt complete treeIndex count does not match")
	}
	previousPath := ""
	for i, entry := range receipt.TreeIndex {
		if !SafeRepoPath(entry.Path) {
			return fmt.Errorf("targetReceipt treeIndex path %q is not repo-relative and safe", entry.Path)
		}
		if previousPath != "" && entry.Path <= previousPath {
			return fmt.Errorf("targetReceipt treeIndex must be sorted with unique paths")
		}
		previousPath = entry.Path
		if !fullHexObjectID(entry.ObjectID, objectIDLength) {
			return fmt.Errorf("targetReceipt treeIndex entry %d has abbreviated or invalid objectID", i)
		}
		if err := validateMapperTreeEntry(entry); err != nil {
			return fmt.Errorf("targetReceipt treeIndex entry %d: %w", i, err)
		}
	}
	return nil
}

func validateMapperTreeEntry(entry MapperTreeIndexEntry) error {
	if entry.ContentSize < 0 || entry.LineCount < 0 {
		return fmt.Errorf("contentSize and lineCount must be non-negative")
	}
	switch {
	case entry.Mode == "120000" && entry.Type == "blob" && entry.Disposition == MapperTreeDispositionSymlink:
		return nil
	case entry.Mode == "160000" && entry.Type == "commit" && entry.Disposition == MapperTreeDispositionSubmodule:
		return nil
	case (entry.Mode == "100644" || entry.Mode == "100755") && entry.Type == "blob" &&
		(entry.Disposition == MapperTreeDispositionRegular || entry.Disposition == MapperTreeDispositionLFS):
		return nil
	default:
		return fmt.Errorf("unsupported mode/type/disposition combination %s/%s/%s", entry.Mode, entry.Type, entry.Disposition)
	}
}

func fullHexObjectID(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, ch := range value {
		if (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') {
			continue
		}
		return false
	}
	return true
}

func sha256Digest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && fullHexObjectID(strings.TrimPrefix(value, "sha256:"), 64)
}

//nolint:gocyclo // cross-inventory accounting intentionally validates each disposition relationship
func validateMapperInventories(artifact ReviewSlicesArtifact) error {
	if artifact.DiscoveredFiles == nil || artifact.ReviewableFiles == nil || artifact.OmittedFiles == nil {
		return fmt.Errorf("schemaVersion %d requires discoveredFiles, reviewableFiles, and omittedFiles arrays", artifact.SchemaVersion)
	}
	if err := validateMapperInventoryList("discoveredFiles", artifact.DiscoveredFiles, map[string]struct{}{
		MapperDispositionReviewable: {},
		MapperDispositionExcluded:   {},
	}, true); err != nil {
		return err
	}
	if err := validateMapperInventoryList("reviewableFiles", artifact.ReviewableFiles, map[string]struct{}{
		MapperDispositionAssigned: {},
		MapperDispositionOmitted:  {},
	}, true); err != nil {
		return err
	}
	if err := validateMapperInventoryList("omittedFiles", artifact.OmittedFiles, map[string]struct{}{
		MapperDispositionExcluded: {},
		MapperDispositionOmitted:  {},
	}, false); err != nil {
		return err
	}

	discovered := make(map[string]MapperFileInventoryEntry, len(artifact.DiscoveredFiles))
	for _, entry := range artifact.DiscoveredFiles {
		discovered[entry.Path] = entry
	}
	reviewable := make(map[string]MapperFileInventoryEntry, len(artifact.ReviewableFiles))
	for _, entry := range artifact.ReviewableFiles {
		reviewable[entry.Path] = entry
	}
	omitted := make(map[string]struct{}, len(artifact.OmittedFiles))
	for _, entry := range artifact.OmittedFiles {
		discoveredEntry, ok := discovered[entry.Path]
		if !ok {
			return fmt.Errorf("omittedFiles path %q was not discovered", entry.Path)
		}
		switch entry.Disposition {
		case MapperDispositionExcluded:
			if discoveredEntry.Disposition != MapperDispositionExcluded || discoveredEntry.Reason != entry.Reason {
				return fmt.Errorf("omittedFiles excluded path %q does not match its discoveredFiles classification", entry.Path)
			}
		case MapperDispositionOmitted:
			if discoveredEntry.Disposition != MapperDispositionReviewable {
				return fmt.Errorf("omittedFiles omitted path %q is not reviewable in discoveredFiles", entry.Path)
			}
		}
		omitted[mapperInventoryKey(entry)] = struct{}{}
	}

	assignedPaths := reviewSliceReferencedPaths(artifact.Slices)
	for _, entry := range artifact.DiscoveredFiles {
		switch entry.Disposition {
		case MapperDispositionReviewable:
			if _, ok := reviewable[entry.Path]; !ok {
				return fmt.Errorf("discovered reviewable path %q is missing from reviewableFiles", entry.Path)
			}
		case MapperDispositionExcluded:
			key := mapperInventoryKey(MapperFileInventoryEntry{
				Path: entry.Path, Disposition: MapperDispositionExcluded, Reason: entry.Reason,
			})
			if _, ok := omitted[key]; !ok {
				return fmt.Errorf("discovered excluded path %q is missing its omittedFiles disposition", entry.Path)
			}
		}
	}
	for _, entry := range artifact.ReviewableFiles {
		discoveredEntry, ok := discovered[entry.Path]
		if !ok || discoveredEntry.Disposition != MapperDispositionReviewable {
			return fmt.Errorf("reviewableFiles path %q is not classified reviewable in discoveredFiles", entry.Path)
		}
		switch entry.Disposition {
		case MapperDispositionAssigned:
			if _, ok := assignedPaths[entry.Path]; !ok {
				return fmt.Errorf("reviewable assigned path %q is not referenced by a slice", entry.Path)
			}
		case MapperDispositionOmitted:
			if _, ok := omitted[mapperInventoryKey(entry)]; !ok {
				return fmt.Errorf("reviewable omitted path %q is missing its omittedFiles disposition", entry.Path)
			}
		}
	}
	for filePath := range assignedPaths {
		entry, ok := reviewable[filePath]
		if !ok || entry.Disposition != MapperDispositionAssigned {
			return fmt.Errorf("slice-referenced path %q is not assigned in reviewableFiles", filePath)
		}
	}
	return nil
}

func validateMapperInventorySummary(artifact ReviewSlicesArtifact) error {
	summary := artifact.InventorySummary
	if summary == nil {
		if artifact.CoverageStatus != MapperCoverageAccountable {
			return fmt.Errorf("coverageStatus %q requires inventorySummary", artifact.CoverageStatus)
		}
		if len(artifact.CoverageReasonCodes) != 0 {
			return fmt.Errorf("accountable mapper coverage cannot include coverageReasonCodes")
		}
		return nil
	}
	if summary.EntryLimit <= 0 || summary.EntryLimit > MaxMapperInventoryEntries {
		return fmt.Errorf("inventorySummary entryLimit must be between 1 and %d", MaxMapperInventoryEntries)
	}
	if summary.RetainedEntries != len(artifact.DiscoveredFiles) {
		return fmt.Errorf(
			"inventorySummary retainedEntries %d does not match discoveredFiles count %d",
			summary.RetainedEntries,
			len(artifact.DiscoveredFiles),
		)
	}
	if summary.RetainedEntries < 0 || summary.RetainedEntries > summary.EntryLimit {
		return fmt.Errorf("inventorySummary retainedEntries must be between 0 and entryLimit")
	}
	if summary.TotalEntries < summary.RetainedEntries {
		return fmt.Errorf("inventorySummary totalEntries is smaller than retainedEntries")
	}
	if summary.TruncatedEntries != summary.TotalEntries-summary.RetainedEntries {
		return fmt.Errorf("inventorySummary truncatedEntries does not match totalEntries minus retainedEntries")
	}

	pathInventoryTruncated := summary.TruncatedEntries > 0
	if pathInventoryTruncated && summary.RetainedEntries != summary.EntryLimit {
		return fmt.Errorf("truncated inventorySummary must retain exactly entryLimit entries")
	}
	omissionRecordsTruncated, err := validateMapperOmissionRecordSummary(artifact, summary.OmissionRecords)
	if err != nil {
		return err
	}
	anyTruncated := pathInventoryTruncated || omissionRecordsTruncated
	if summary.Truncated != anyTruncated {
		return fmt.Errorf("inventorySummary truncated flag does not match bounded collection metadata")
	}

	if anyTruncated {
		if summary.Reason != MapperCoverageReasonInventoryEntryLimit {
			return fmt.Errorf(
				"truncated inventorySummary requires reason %q",
				MapperCoverageReasonInventoryEntryLimit,
			)
		}
		if artifact.CoverageStatus != MapperCoveragePartial {
			return fmt.Errorf("truncated inventorySummary requires coverageStatus %q", MapperCoveragePartial)
		}
		if len(artifact.CoverageReasonCodes) != 1 ||
			artifact.CoverageReasonCodes[0] != MapperCoverageReasonInventoryEntryLimit {
			return fmt.Errorf(
				"truncated inventorySummary requires coverageReasonCodes [%q]",
				MapperCoverageReasonInventoryEntryLimit,
			)
		}
		return nil
	}

	if summary.Reason != "" {
		return fmt.Errorf("complete inventorySummary cannot include a truncation reason")
	}
	if summary.TruncatedEntries != 0 || summary.TotalEntries != summary.RetainedEntries {
		return fmt.Errorf("complete inventorySummary has inconsistent entry counts")
	}
	if artifact.CoverageStatus != MapperCoverageAccountable {
		return fmt.Errorf("complete inventorySummary requires coverageStatus %q", MapperCoverageAccountable)
	}
	if len(artifact.CoverageReasonCodes) != 0 {
		return fmt.Errorf("accountable mapper coverage cannot include coverageReasonCodes")
	}
	return nil
}

func validateMapperOmissionRecordSummary(
	artifact ReviewSlicesArtifact,
	summary *MapperOmissionRecordSummary,
) (bool, error) {
	if summary == nil {
		return false, nil
	}
	if summary.EntryLimit <= 0 || summary.EntryLimit > MaxMapperOmittedInventoryEntries {
		return false, fmt.Errorf(
			"inventorySummary omissionRecords entryLimit must be between 1 and %d",
			MaxMapperOmittedInventoryEntries,
		)
	}
	if summary.RetainedRecords != len(artifact.OmittedFiles) {
		return false, fmt.Errorf(
			"inventorySummary omissionRecords retainedRecords %d does not match omittedFiles count %d",
			summary.RetainedRecords,
			len(artifact.OmittedFiles),
		)
	}
	if summary.RetainedRecords < 0 || summary.RetainedRecords > summary.EntryLimit {
		return false, fmt.Errorf(
			"inventorySummary omissionRecords retainedRecords must be between 0 and entryLimit",
		)
	}
	if summary.TotalRecords < summary.RetainedRecords {
		return false, fmt.Errorf(
			"inventorySummary omissionRecords totalRecords is smaller than retainedRecords",
		)
	}
	if summary.TruncatedRecords != summary.TotalRecords-summary.RetainedRecords {
		return false, fmt.Errorf(
			"inventorySummary omissionRecords truncatedRecords does not match totalRecords minus retainedRecords",
		)
	}
	truncated := summary.TruncatedRecords > 0
	if summary.Truncated != truncated {
		return false, fmt.Errorf(
			"inventorySummary omissionRecords truncated flag does not match truncatedRecords",
		)
	}
	if truncated && summary.RetainedRecords != summary.EntryLimit {
		return false, fmt.Errorf(
			"truncated inventorySummary omissionRecords must retain exactly entryLimit records",
		)
	}
	return truncated, nil
}

func validateMapperInventoryList(
	name string,
	entries []MapperFileInventoryEntry,
	allowedDispositions map[string]struct{},
	requireUniquePath bool,
) error {
	seen := map[string]struct{}{}
	previous := ""
	for i, entry := range entries {
		if !SafeRepoPath(entry.Path) {
			return fmt.Errorf("%s path %q is not repo-relative and safe", name, entry.Path)
		}
		if _, ok := allowedDispositions[entry.Disposition]; !ok {
			return fmt.Errorf("%s path %q has unsupported disposition %q", name, entry.Path, entry.Disposition)
		}
		if strings.TrimSpace(entry.Reason) == "" {
			return fmt.Errorf("%s path %q requires a reason", name, entry.Path)
		}
		key := mapperInventoryKey(entry)
		if previous != "" && key < previous {
			return fmt.Errorf("%s must be sorted deterministically", name)
		}
		previous = key
		uniqueKey := key
		if requireUniquePath {
			uniqueKey = entry.Path
		}
		if _, ok := seen[uniqueKey]; ok {
			return fmt.Errorf("%s contains duplicate entry at index %d for path %q", name, i, entry.Path)
		}
		seen[uniqueKey] = struct{}{}
	}
	return nil
}

func mapperInventoryKey(entry MapperFileInventoryEntry) string {
	return strings.Join([]string{entry.Path, entry.Disposition, entry.Reason}, "\x00")
}

func reviewSliceReferencedPaths(slices []store.ReviewSlice) map[string]struct{} {
	paths := map[string]struct{}{}
	for _, slice := range slices {
		for _, file := range slice.Entrypoints {
			paths[file.Path] = struct{}{}
		}
		for _, file := range slice.OwnedFiles {
			paths[file.Path] = struct{}{}
		}
		for _, file := range slice.ContextFiles {
			paths[file.Path] = struct{}{}
		}
		for _, test := range slice.Tests {
			if test.Path != "" {
				paths[test.Path] = struct{}{}
			}
		}
	}
	return paths
}

// ParseReviewContextManifest parses and minimally validates a review context manifest.
func ParseReviewContextManifest(data []byte) (*ReviewContextManifest, error) {
	var manifest ReviewContextManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}
	if manifest.SchemaVersion != SchemaVersionReviewContext {
		return nil, fmt.Errorf("unsupported review context schemaVersion %d", manifest.SchemaVersion)
	}
	if strings.TrimSpace(manifest.SliceID) == "" {
		return nil, fmt.Errorf("sliceId is required")
	}
	for _, file := range manifest.ChangedFiles {
		if !SafeRepoPath(file) {
			return nil, fmt.Errorf("changed file path %q is not repo-relative and safe", file)
		}
	}
	for _, lineRange := range manifest.ChangedLineRanges {
		if !validChangedLineRange(lineRange) {
			return nil, fmt.Errorf("changed line range for %q is invalid", lineRange.Path)
		}
	}
	for _, file := range manifest.IncludedFiles {
		if !SafeRepoPath(file.Path) {
			return nil, fmt.Errorf("included file path %q is not repo-relative and safe", file.Path)
		}
		for _, lineRange := range file.IncludedLineRanges {
			if lineRange.StartLine <= 0 || lineRange.EndLine < lineRange.StartLine {
				return nil, fmt.Errorf("included file %q has invalid line range", file.Path)
			}
		}
	}
	return &manifest, nil
}

// ParseFindingsV2Artifact parses and validates only the top-level contract.
func ParseFindingsV2Artifact(data []byte) (*FindingsV2Artifact, error) {
	var artifact FindingsV2Artifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		return nil, err
	}
	if artifact.SchemaVersion != SchemaVersionFindingsV2 {
		return nil, fmt.Errorf("unsupported findings schemaVersion %d", artifact.SchemaVersion)
	}
	if artifact.Findings == nil {
		return nil, fmt.Errorf("findings must be an array")
	}
	for i, finding := range artifact.Findings {
		if err := validateFindingsV2IdentityProposal(finding); err != nil {
			return nil, fmt.Errorf("finding %d identity proposal: %w", i, err)
		}
	}
	return &artifact, nil
}

func validateFindingsV2IdentityProposal(finding FindingsV2Finding) error {
	type identitySlugField struct {
		name  string
		value string
	}
	fields := []identitySlugField{
		{name: "ruleId", value: finding.RuleID},
	}
	if finding.Identity != nil {
		fields = append(fields,
			identitySlugField{name: "identity.anchor", value: finding.Identity.Anchor},
			identitySlugField{name: "identity.instance", value: finding.Identity.Instance},
			identitySlugField{name: "identity.algorithmVersion", value: finding.Identity.AlgorithmVersion},
		)
	}
	for _, field := range fields {
		if field.value == "" {
			continue
		}
		if !validLowerSemanticSlug(field.value) {
			return fmt.Errorf("%s must be a lowercase semantic slug of at most %d bytes", field.name, MaxFindingsIdentitySlugBytes)
		}
	}
	return nil
}

func validLowerSemanticSlug(value string) bool {
	if value == "" || len(value) > MaxFindingsIdentitySlugBytes || strings.TrimSpace(value) != value {
		return false
	}
	previousSeparator := true
	for _, ch := range value {
		switch {
		case ch >= 'a' && ch <= 'z', ch >= '0' && ch <= '9':
			previousSeparator = false
		case ch == '-', ch == '_', ch == '.', ch == '/', ch == ':':
			if previousSeparator {
				return false
			}
			previousSeparator = true
		default:
			return false
		}
	}
	return !previousSeparator
}

func validateReviewSliceContract(slice store.ReviewSlice) error {
	if slice.SchemaVersion != 0 && slice.SchemaVersion != SchemaVersionReviewSlices {
		return fmt.Errorf("unsupported schemaVersion %d", slice.SchemaVersion)
	}
	if strings.TrimSpace(slice.ID) == "" {
		return fmt.Errorf("id is required")
	}
	if strings.TrimSpace(slice.RepositoryScan) == "" {
		return fmt.Errorf("repositoryScan is required")
	}
	if strings.TrimSpace(slice.Source) == "" {
		return fmt.Errorf("source is required")
	}
	if strings.TrimSpace(slice.Title) == "" {
		return fmt.Errorf("title is required")
	}
	files := append([]store.ReviewSliceFile{}, slice.Entrypoints...)
	files = append(files, slice.OwnedFiles...)
	files = append(files, slice.ContextFiles...)
	for _, file := range files {
		if !SafeRepoPath(file.Path) {
			return fmt.Errorf("path %q is not repo-relative and safe", file.Path)
		}
	}
	for _, test := range slice.Tests {
		if test.Path != "" && !SafeRepoPath(test.Path) {
			return fmt.Errorf("test path %q is not repo-relative and safe", test.Path)
		}
	}
	for _, file := range slice.ChangedFiles {
		if !SafeRepoPath(file) {
			return fmt.Errorf("changed file path %q is not repo-relative and safe", file)
		}
	}
	for _, lineRange := range slice.ChangedLineRanges {
		if !validChangedLineRange(lineRange) {
			return fmt.Errorf("changed line range for %q is invalid", lineRange.Path)
		}
	}
	return nil
}

func validChangedLineRange(lineRange ChangedLineRange) bool {
	return SafeRepoPath(lineRange.Path) && lineRange.StartLine > 0 && lineRange.EndLine >= lineRange.StartLine
}

// SafeRepoPath returns true when p is a clean relative repository path.
func SafeRepoPath(p string) bool {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" || strings.HasPrefix(p, "/") {
		return false
	}
	cleaned := path.Clean(p)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.Contains(cleaned, "/../") {
		return false
	}
	return cleaned == p
}

// FindingV2Fingerprint computes an Orka-owned stable fingerprint for a v2 finding.
func FindingV2Fingerprint(namespace, repositoryScan, repoURL, branch, subPath, sliceID string, finding FindingsV2Finding) string {
	refs := make([]findingV2FingerprintEvidenceRef, 0, len(finding.Evidence))
	seenRefs := map[string]struct{}{}
	for _, ref := range finding.Evidence {
		canonicalRef := findingV2FingerprintEvidenceRef{
			Path:      strings.TrimSpace(strings.ReplaceAll(ref.Path, "\\", "/")),
			StartLine: ref.StartLine,
			EndLine:   ref.EndLine,
			Symbol:    canonicalFingerprintSymbol(ref.Symbol),
		}
		key := canonicalEvidenceKey(canonicalRef)
		if _, ok := seenRefs[key]; ok {
			continue
		}
		seenRefs[key] = struct{}{}
		refs = append(refs, canonicalRef)
	}
	sort.Slice(refs, func(i, j int) bool {
		left := canonicalEvidenceKey(refs[i])
		right := canonicalEvidenceKey(refs[j])
		return left < right
	})

	payload := struct {
		Version        int                               `json:"version"`
		Namespace      string                            `json:"namespace"`
		RepositoryScan string                            `json:"repositoryScan"`
		RepoURL        string                            `json:"repoURL"`
		Branch         string                            `json:"branch"`
		SubPath        string                            `json:"subPath"`
		SliceID        string                            `json:"sliceID"`
		Category       string                            `json:"category"`
		Title          string                            `json:"title"`
		Evidence       []findingV2FingerprintEvidenceRef `json:"evidence"`
	}{
		Version:        2,
		Namespace:      strings.TrimSpace(namespace),
		RepositoryScan: strings.TrimSpace(repositoryScan),
		RepoURL:        strings.TrimSpace(repoURL),
		Branch:         strings.TrimSpace(branch),
		SubPath:        strings.Trim(strings.TrimSpace(subPath), "/"),
		SliceID:        strings.TrimSpace(sliceID),
		Category:       strings.ToLower(strings.TrimSpace(finding.Category)),
		Title:          normalizeFingerprintText(finding.Title),
		Evidence:       refs,
	}
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return "v2:" + hex.EncodeToString(sum[:])
}

type findingV2FingerprintEvidenceRef struct {
	Path      string  `json:"path"`
	StartLine int     `json:"startLine"`
	EndLine   int     `json:"endLine"`
	Symbol    *string `json:"symbol,omitempty"`
}

func canonicalEvidenceKey(ref findingV2FingerprintEvidenceRef) string {
	symbol := ""
	if ref.Symbol != nil {
		symbol = strings.TrimSpace(*ref.Symbol)
	}
	return fmt.Sprintf("%s:%d:%d:%s", ref.Path, ref.StartLine, ref.EndLine, symbol)
}

func canonicalFingerprintSymbol(value *string) *string {
	if value == nil {
		return nil
	}
	symbol := strings.TrimSpace(*value)
	if symbol == "" {
		return nil
	}
	return &symbol
}

func normalizeFingerprintText(value string) string {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(value)))
	return strings.Join(fields, " ")
}
