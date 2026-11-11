package remediation

import (
	"errors"
	"testing"
)

func TestCompilePatchReportsMatchMetadataWithoutSourceText(t *testing.T) {
	proposal := PatchProposal{Edits: []SourceEdit{{Path: "source.go", Old: "duplicate", New: "replacement"}}}
	patch, err := CompilePatch(proposal, map[string][]byte{"source.go": []byte("duplicate\nduplicate\n")})
	var mismatch *EditMatchError
	if patch != "" || !errors.As(err, &mismatch) || mismatch.Index != 0 || mismatch.Matches != 2 {
		t.Fatalf("ambiguous edit did not return safe exact-match metadata: %v", err)
	}
	if err.Error() != "candidate edit 0 old text matches 2 times; exactly one match is required" {
		t.Fatal("match metadata changed or source text leaked")
	}
	proposal.Edits[0].Old = "missing"
	_, err = CompilePatch(proposal, map[string][]byte{"source.go": []byte("unchanged\n")})
	if !errors.As(err, &mismatch) || mismatch.Matches != 0 {
		t.Fatal("missing edit was not reported precisely")
	}
}
