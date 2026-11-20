/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package utils

import (
	"fmt"
	"regexp"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

var fullCommitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// PseudoVersionNamesRevision reports whether a pinned module version is a
// pseudo-version and, if so, verifies that it encodes the exact pinned commit.
// No tag exists for a pseudo-version: its commit prefix is the only revision
// claim the version makes. A release version returns false so the caller can
// verify its tag. Malformed versions or revisions, and a pseudo-version whose
// commit prefix differs from the revision, fail closed.
func PseudoVersionNamesRevision(version, revision string) (bool, error) {
	if !fullCommitSHA.MatchString(revision) {
		return false, fmt.Errorf("pinned module revision %q is not a full lowercase commit SHA", revision)
	}
	if !semver.IsValid(version) {
		return false, fmt.Errorf("pinned module version %q is not a valid semantic version", version)
	}
	if !module.IsPseudoVersion(version) {
		return false, nil
	}
	prefix, err := module.PseudoVersionRev(version)
	if err != nil {
		return false, fmt.Errorf("parse pinned module pseudo-version %q: %w", version, err)
	}
	if prefix != revision[:12] {
		return false, fmt.Errorf("pinned module pseudo-version %q does not name revision %s", version, revision)
	}
	return true, nil
}
