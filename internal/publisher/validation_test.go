package publisher

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateSourceRefAcceptsCanonicalObjectBranchAndTags(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		ref  string
	}{
		{name: "sha1 object ID", ref: strings.Repeat("a", 40)},
		{name: "sha256 object ID", ref: strings.Repeat("b", 64)},
		{name: "branch", ref: "refs/heads/main"},
		// Lightweight and annotated tags use the same canonical ref syntax. The
		// service's Git resolver is responsible for peeling annotated tags.
		{name: "lightweight tag", ref: "refs/tags/v1.2.3"},
		{name: "annotated tag", ref: "refs/tags/releases/v2.0.0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := validateSourceRef(test.ref); err != nil {
				t.Fatalf("validateSourceRef(%q) error = %v", test.ref, err)
			}
		})
	}
}

func TestValidateSourceRefRejectsMalformedCanonicalTags(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		ref  string
	}{
		{name: "empty", ref: "refs/tags/"},
		{name: "leading dash", ref: "refs/tags/-release"},
		{name: "trailing slash", ref: "refs/tags/release/"},
		{name: "trailing dot", ref: "refs/tags/release."},
		{name: "double dot", ref: "refs/tags/release..candidate"},
		{name: "reflog selector", ref: "refs/tags/release@{candidate"},
		{name: "double slash", ref: "refs/tags/release//candidate"},
		{name: "forbidden character", ref: "refs/tags/release[candidate"},
		{name: "hidden component", ref: "refs/tags/releases/.candidate"},
		{name: "lock component", ref: "refs/tags/releases/candidate.lock"},
		{name: "backslash", ref: `refs/tags/releases\candidate`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateSourceRef(test.ref)
			if !errors.Is(err, ErrInvalidRef) {
				t.Fatalf("validateSourceRef(%q) error = %v, want ErrInvalidRef", test.ref, err)
			}
		})
	}
}
