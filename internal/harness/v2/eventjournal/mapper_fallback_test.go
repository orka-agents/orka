package eventjournal

import (
	"net/url"
	"strings"
	"testing"

	executionevents "github.com/orka-agents/orka/internal/events"
)

func TestLogicalFieldFallbackConsumesEachActualCopyOnce(t *testing.T) {
	for _, parts := range [][]string{
		{"ken=X\tto"},
		{"=fixture token"},
		{"=fixture pwd"},
		{"pa", "s", "word=fixture"},
	} {
		for _, repeat := range []bool{false, true} {
			var fields []logicalFieldBoundaries
			for _, part := range parts {
				fields = appendLogicalFieldCopies(fields, part)
				if repeat {
					fields = appendLogicalFieldCopies(fields, part)
				}
			}
			for len(fields) <= maxLogicalFieldPermutationFields {
				fields = appendLogicalFieldCopies(fields, "filler")
			}
			if got := logicalFieldsMayReconstructSensitiveMarker(fields); got != repeat {
				t.Errorf("parts=%q repeated=%v: fallback sensitivity=%v", parts, repeat, got)
			}
		}
	}
}

func TestLogicalFieldCopyAccountingPreservesCredentialSplits(t *testing.T) {
	for _, text := range []string{
		"pwd=fixture", "token=fixture", "token_name\" :fixture", "pwd'\t=fixture",
		"token is fixture", "token\tis\tfixture", "api key is fixture", "password is fixture",
		"secret=fixture", "paſſword=fixture", "APIKEY=fixture", "Authorization: Bearer fixture",
		"Txn-Token: fixture", "Cookie: fixture=value",
		(&url.URL{Scheme: "https", User: url.UserPassword("user", "fixture"), Host: "example.test"}).String(),
		"?sig=fixture", "&x-goog-signature=fixture",
	} {
		if executionevents.RedactExecutionEventText(text) == text {
			t.Fatalf("invalid protected fixture %q", text)
		}
		runes := []rune(text)
		for first := 1; first < len(runes); first++ {
			for second := first; second < len(runes); second++ {
				parts := []string{string(runes[:first]), string(runes[first:second]), string(runes[second:])}
				var fields []logicalFieldBoundaries
				individuallySafe := true
				for _, part := range parts {
					if executionevents.RedactExecutionEventText(part) != part {
						individuallySafe = false
						break
					}
					fields = appendLogicalFieldCopies(fields, part)
				}
				if !individuallySafe {
					continue
				}
				if !logicalFieldsMayReconstructSensitiveMarker(fields) {
					t.Fatalf("protected text survives fallback: %q joined=%q", parts, strings.Join(parts, ""))
				}
			}
		}
	}
}

func TestLogicalFieldCopyAccountingKeepsClippedAssignmentFinishes(t *testing.T) {
	for _, parts := range [][]string{
		{"to", "ken" + strings.Repeat("a", 800) + ":", "fixture"},
		{"to", "ken" + strings.Repeat("a", 800) + "'\t:", "fixture"},
		{"p", "w", "d" + strings.Repeat("a", 800) + "=", "fixture"},
		{"to", "ken" + strings.Repeat("a", 800), "=fixture"},
		{"to", "ken\t", "\t", " ", "\n", "is", "\t", "fixture"},
		{"token", "\t", "\t", "is", "\t", "\t", "fixture"},
		{"token ", "\t", " ", "is ", "\t", "fixture"},
		{"to", "ken" + strings.Repeat(" ", 800), "is fixture"},
		{"to", "ken" + strings.Repeat(" \r\n\t", 800) + "is fixture"},
	} {
		if executionevents.RedactExecutionEventText(strings.Join(parts, "")) == strings.Join(parts, "") {
			t.Fatal("fixture does not reconstruct a protected value")
		}
		var fields []logicalFieldBoundaries
		for _, part := range parts {
			if executionevents.RedactExecutionEventText(part) != part {
				t.Fatal("fixture part was not independently safe")
			}
			fields = appendLogicalFieldCopies(fields, part)
		}
		for len(fields) <= maxLogicalFieldPermutationFields {
			fields = appendLogicalFieldCopies(fields, "filler")
		}
		if !logicalFieldsMayReconstructSensitiveMarker(fields) {
			t.Fatalf("protected assignment finish or whitespace chain was discarded (part lengths=%v)", func() []int {
				lengths := make([]int, len(parts))
				for i, part := range parts {
					lengths[i] = len(part)
				}
				return lengths
			}())
		}
	}
}

func TestLogicalFieldFallbackRejectsFixedAssignmentTail(t *testing.T) {
	for _, parts := range [][]string{
		{"p", "wd && ls", "=fixture"},
		{"to", "ken && ls", "=fixture"},
		{"to", "ken && ls", "unrelated: fixture"},
		{"to", "ken && ls", "Model usage updated: 1 input, 0 output, 0 cached input tokens"},
		{"to", "ken" + strings.Repeat("a", 800) + " && ls", "=fixture"},
		{"p", "wd" + strings.Repeat("a", 800) + " && ls", "=fixture"},
	} {
		var fields []logicalFieldBoundaries
		for _, part := range parts {
			fields = appendLogicalFieldBoundary(fields, part)
		}
		if logicalFieldsMayReconstructSensitiveMarker(fields) {
			t.Fatal("fixed command suffix was ignored while completing an assignment")
		}
	}
}
