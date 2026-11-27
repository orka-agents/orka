package workspacedelta

import "fmt"

const (
	ManifestSchema = "orka.workspace-delta.v1"
	DigestPrefix   = "sha256:"
)

type Intent string

const (
	IntentRead  Intent = "read"
	IntentWrite Intent = "write"
)

func (i Intent) validate() error {
	switch i {
	case IntentRead, IntentWrite:
		return nil
	default:
		return fmt.Errorf("unsupported workspace intent %q", i)
	}
}

type Classification string

const (
	ClassificationNoChange         Classification = "no_change"
	ClassificationReadOnlyModified Classification = "read_only_modified"
	ClassificationWriteDelta       Classification = "write_delta"
)

type EntryKind string

const (
	EntryDirectory EntryKind = "directory"
	EntryFile      EntryKind = "file"
	EntrySymlink   EntryKind = "symlink"
)

type ChangeOperation string

const (
	ChangeAdded    ChangeOperation = "added"
	ChangeModified ChangeOperation = "modified"
	ChangeReplaced ChangeOperation = "replaced"
)

type Change struct {
	Path      string          `json:"path"`
	Operation ChangeOperation `json:"operation"`
	Kind      EntryKind       `json:"kind"`
	Mode      int64           `json:"mode"`
	Size      int64           `json:"size,omitempty"`
	Digest    string          `json:"digest,omitempty"`
	Target    string          `json:"target,omitempty"`
}

type Deletion struct {
	Path string    `json:"path"`
	Kind EntryKind `json:"kind"`
}

type Symlink struct {
	Path   string `json:"path"`
	Target string `json:"target"`
	Mode   int64  `json:"mode"`
}

type Manifest struct {
	Schema          string   `json:"schema"`
	Entries         []Change `json:"entries"`
	DeletionsDigest string   `json:"deletionsDigest"`
	SymlinksDigest  string   `json:"symlinksDigest"`
}

type Result struct {
	Classification Classification
	Changes        []Change
	Deletions      []Deletion
	Symlinks       []Symlink
	Manifest       []byte
	ManifestDigest string
	Artifact       []byte
	ArtifactDigest string
}

type BuildLimits struct {
	MaxArtifactBytes int64
}

type Snapshot struct {
	version        uint32
	entries        map[string]entry
	options        normalizedOptions
	optionsDigest  string
	manifestDigest string
	entryCount     int
	totalBytes     int64
	ignore         *ignoreRules
}

type entry struct {
	path         string
	kind         EntryKind
	mode         int64
	size         int64
	digest       string
	target       string
	protected    bool
	sourceMode   uint32
	content      []byte
	flagged      bool
	fingerprints []string
}

func (s *Snapshot) BaselineContentFlagged(path string) bool {
	if s == nil {
		return false
	}
	e, ok := s.entries[path]
	return ok && e.flagged
}

func (s *Snapshot) BaselineContentFingerprints(path string) []string {
	if s == nil {
		return nil
	}
	e, ok := s.entries[path]
	if !ok {
		return nil
	}
	return append([]string(nil), e.fingerprints...)
}
