/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package utils

import (
	"strings"
	"testing"
)

func TestPseudoVersionNamesRevision(t *testing.T) {
	const (
		revision = "3f54963ed92b87752c27379affd932aaf653ce2a"
		pinned   = "v0.1.0-alpha.5.0.20261106183142-3f54963ed92b"
		other    = "v0.1.0-alpha.5.0.20261106150457-dd4970df4295"
		release  = "v0.1.0-alpha.5"
	)
	for _, test := range []struct {
		name, version, revision string
		pseudo, wantErr         bool
	}{
		{name: "pseudo-version naming the revision", version: pinned, revision: revision, pseudo: true},
		{name: "release version needs its tag", version: release, revision: revision},
		{name: "pseudo-version naming another commit", version: other, revision: revision, wantErr: true},
		{name: "unparseable version", version: "main", revision: revision, wantErr: true},
		{name: "empty version", version: "", revision: revision, wantErr: true},
		{name: "abbreviated revision", version: pinned, revision: revision[:12], wantErr: true},
		{name: "uppercase revision", version: release, revision: strings.ToUpper(revision), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			pseudo, err := PseudoVersionNamesRevision(test.version, test.revision)
			if (err != nil) != test.wantErr || pseudo != test.pseudo {
				t.Fatalf("PseudoVersionNamesRevision(%q, %q) = %v, %v; want pseudo=%v err=%v",
					test.version, test.revision, pseudo, err, test.pseudo, test.wantErr)
			}
		})
	}
}
