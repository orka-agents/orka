/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package workspacedelta

import "fmt"

// Build compares a trusted pre-prompt baseline with a frozen post-prompt tree.
// It returns no artifact for no-change and read-only-modified classifications.
func Build(baseline *Snapshot, postRoot string, intent Intent) (Result, error) {
	return BuildWithLimits(baseline, postRoot, intent, BuildLimits{})
}

// ManifestDigest returns the content digest of the normalized snapshot.
func (s *Snapshot) ManifestDigest() string {
	if s == nil {
		return ""
	}
	return s.manifestDigest
}

func (r Result) Validate() error {
	switch r.Classification {
	case ClassificationNoChange:
		if len(r.Changes) != 0 || len(r.Deletions) != 0 || len(r.Symlinks) != 0 || len(r.Manifest) != 0 ||
			r.ManifestDigest != "" || len(r.Artifact) != 0 || r.ArtifactDigest != "" {
			return fmt.Errorf("no-change result carries delta data")
		}
	case ClassificationReadOnlyModified:
		if len(r.Changes)+len(r.Deletions) == 0 || len(r.Manifest) != 0 || r.ManifestDigest != "" ||
			len(r.Artifact) != 0 || r.ArtifactDigest != "" {
			return fmt.Errorf("read-only-modified result is inconsistent")
		}
	case ClassificationWriteDelta:
		if len(r.Changes)+len(r.Deletions) == 0 || len(r.Manifest) == 0 || r.ManifestDigest == "" ||
			len(r.Artifact) == 0 || r.ArtifactDigest == "" {
			return fmt.Errorf("write-delta result is incomplete")
		}
	default:
		return fmt.Errorf("unsupported classification %q", r.Classification)
	}
	return nil
}
