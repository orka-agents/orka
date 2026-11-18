package remediation

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestExplicitEditOccurrencesRemainExactAndBounded(t *testing.T) {
	original := map[string][]byte{"source.go": []byte("call(old)\ncall(old)\n")}
	proposal := PatchProposal{Edits: []SourceEdit{{Path: "source.go", Old: "call(old)", New: "call(new)"}}}
	if _, err := CompilePatch(proposal, original); err == nil {
		t.Fatal("omitted count must retain unique-match behavior")
	}
	proposal.Edits[0].Occurrences = new(2)
	patch, err := CompilePatch(proposal, original)
	if err != nil || strings.Count(patch, "+call(new)") != 2 || strings.Count(patch, "-call(old)") != 2 {
		t.Fatalf("explicit two-site edit did not compile: %v", err)
	}
	for _, count := range []int{-1, 0, 1, 3, MaxEditOccurrences + 1} {
		proposal.Edits[0].Occurrences = new(count)
		if _, err := CompilePatch(proposal, original); err == nil {
			t.Fatalf("count %d accepted for two matches", count)
		}
	}
	proposal.Edits[0].Occurrences = new(3)
	_, err = CompilePatch(proposal, original)
	var mismatch *EditMatchError
	if !errors.As(err, &mismatch) || mismatch.Expected != 3 || mismatch.Matches != 2 {
		t.Fatal("mismatch did not preserve required and observed counts")
	}
	proposal.Edits[0].Occurrences = new(MaxEditOccurrences)
	_, err = CompilePatch(proposal, map[string][]byte{"source.go": []byte(strings.Repeat("call(old)\n", MaxEditOccurrences))})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDecodeEditOccurrencesRejectsInvalidCounts(t *testing.T) {
	for _, value := range []string{"-1", "0", "17", "1.5", `"2"`, "true"} {
		raw := fmt.Sprintf(`{"summary":"fixture","edits":[{"path":"source.go","old":"old","new":"new","occurrences":%s}],"declaredChanges":[{"kind":"source","paths":["source.go"]}],"limitations":[]}`, value)
		if _, err := DecodePatchProposal(raw); err == nil {
			t.Fatalf("invalid occurrence count %s accepted", value)
		}
	}
}
