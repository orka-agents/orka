package eventjournal

import (
	"fmt"
	"strings"
	"testing"

	executionevents "github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestProjectPlanUpdateRedactsSplitMalformedRelativeURL(t *testing.T) {
	// A malformed relative URL needs no credential marker or query string to
	// trigger the public redactor's fail-closed URL parsing behavior.
	for _, parts := range [][]string{
		{"/reports", "%zz"},
		{"/", "reports%zz"},
		{"See (/", "reports%zz)"},
	} {
		joined := strings.Join(parts, "")
		if executionevents.RedactExecutionEventText(joined) == joined {
			t.Fatal("fixture does not reconstruct a protected URL")
		}
		entries := make([]harnessv2.PlanEntry, len(parts))
		for index, part := range parts {
			if executionevents.RedactExecutionEventText(part) != part {
				t.Fatal("fixture parts must be harmless in isolation")
			}
			entries[index] = harnessv2.PlanEntry{Content: part, Status: harnessv2.PlanEntryPending}
		}
		projection := ProjectPlanUpdate(harnessv2.PlanUpdate{Entries: entries})
		if strings.Contains(projection.Document, "%zz") ||
			!strings.Contains(projection.Document, executionevents.ExecutionEventRedactedValue) {
			t.Fatal("plan exposed a malformed URL split across public fields")
		}
	}
}

func TestLogicalFieldsRedactsClippedAssignmentPrefixes(t *testing.T) {
	for _, marker := range []string{"token", "pwd", "secret", "password", "api_key"} {
		t.Run(marker, func(t *testing.T) {
			key := marker + strings.Repeat("a", maxLogicalFieldBoundaryRunes+44)
			values, _ := redactLogicalFieldsWithHistory(nil, false, key+"=head", "tail")
			for _, value := range values {
				if value != executionevents.ExecutionEventRedactedValue {
					t.Fatal("clipped assignment prefix left a public continuation unredacted")
				}
			}
		})
	}
}

func BenchmarkBenignLogicalFieldHistory(b *testing.B) {
	var fields []logicalFieldBoundaries
	for index := 1; index <= 128; index++ {
		fields = appendLogicalFieldBoundary(fields, fmt.Sprintf("Completed step %03d.", index))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if permutedLogicalFieldSubsetsSensitive(fields) {
			b.Fatal("benign history was classified as sensitive")
		}
	}
}
