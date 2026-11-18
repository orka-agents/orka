/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/orka-agents/orka/internal/connectors"
)

func TestRegisterBrokeredGitHubToolsMatchesTheConnectorCatalog(t *testing.T) {
	registry := NewRegistry()
	k8sClient := fake.NewClientBuilder().Build()
	if err := RegisterBrokeredGitHubTools(registry, k8sClient); err != nil {
		t.Fatal(err)
	}
	if err := RegisterBrokeredGitHubTools(registry, k8sClient); err != nil {
		t.Fatalf("second registration: %v", err)
	}
	names := registry.Names()
	slices.Sort(names)
	if want := connectors.BuiltinConnectorToolNames(); !slices.Equal(names, want) {
		t.Fatalf("registered = %v, want the catalog %v", names, want)
	}
	review, _ := registry.Get("review_pull_request")
	if tool, ok := review.(*ReviewPullRequestTool); !ok || tool.maxResultBytes != brokeredResultBudget {
		t.Fatalf("brokered review_pull_request must be bounded: %#v", review)
	}
	issue, _ := registry.Get("get_issue")
	if tool, ok := issue.(*GetIssueTool); !ok || tool.maxResultBytes != brokeredResultBudget {
		t.Fatalf("brokered get_issue must be bounded: %#v", issue)
	}
	if err := RegisterBrokeredGitHubTools(nil, k8sClient); err == nil {
		t.Fatal("nil registry must be refused")
	}
	if err := RegisterBrokeredGitHubTools(NewRegistry(), nil); err == nil {
		t.Fatal("nil client must be refused")
	}
}

func TestBoundReviewPullRequestResult(t *testing.T) {
	big := strings.Repeat("+line of diff content\n", 20000)
	result := ReviewPullRequestResult{
		PRTitle: "t", Diff: big, Status: "fetched",
		Files: []FileChange{{Filename: "a.go", Patch: strings.Repeat("@@ hunk\n", 5000)}, {Filename: "b.go", Patch: strings.Repeat("@@ hunk\n", 5000)}},
	}
	if got := boundReviewPullRequestResult(result, 0); got.Truncated || got.Diff != big {
		t.Fatal("no limit must leave the result alone")
	}
	encoded, _ := json.Marshal(result)
	if got := boundReviewPullRequestResult(result, len(encoded)); got.Truncated {
		t.Fatal("a result within the limit must not be truncated")
	}
	// Patches go first, then the diff, and the result always fits.
	for _, limit := range []int{len(encoded) - 1, 200_000, 4096, 512} {
		got := boundReviewPullRequestResult(result, limit)
		out, _ := json.Marshal(got)
		if len(out) > limit || !got.Truncated || got.TruncationNote == "" || len(got.Files) != 2 || got.Files[0].Patch != "" || got.Files[0].Filename != "a.go" {
			t.Fatalf("limit %d: len=%d truncated=%t files=%+v", limit, len(out), got.Truncated, got.Files)
		}
		if limit == len(encoded)-1 && got.Diff != big {
			t.Fatal("dropping patches alone must have been enough")
		}
		if !utf8.ValidString(got.Diff) {
			t.Fatalf("limit %d: diff is not valid UTF-8", limit)
		}
	}
	if result.Files[0].Patch == "" {
		t.Fatal("the caller's result must not be mutated")
	}
}

func TestBoundGetIssueResult(t *testing.T) {
	comment := func(i int) IssueComment {
		return IssueComment{Author: "a", Body: strings.Repeat("c", 10_000), CreatedAt: strings.Repeat("t", i+1)}
	}
	result := GetIssueResult{Number: 7, Title: "t", Body: strings.Repeat("b", 50_000), CommentCount: 5, Comments: []IssueComment{comment(0), comment(1), comment(2), comment(3), comment(4)}}
	if got := boundGetIssueResult(result, 0); got.Truncated || len(got.Comments) != 5 {
		t.Fatal("no limit must leave the result alone")
	}
	encoded, _ := json.Marshal(result)
	if got := boundGetIssueResult(result, len(encoded)); got.Truncated {
		t.Fatal("a result within the limit must not be truncated")
	}
	// The oldest comments go first and the newest stay; then the body.
	got := boundGetIssueResult(result, len(encoded)-1)
	if out, _ := json.Marshal(got); len(out) > len(encoded)-1 || !got.Truncated || len(got.Comments) != 4 || got.Comments[0].CreatedAt != "tt" || got.Body != result.Body || got.CommentCount != 5 {
		t.Fatalf("one comment: len=%d %+v", len(out), got)
	}
	for _, limit := range []int{60_000, 4096, 512} {
		got := boundGetIssueResult(result, limit)
		out, _ := json.Marshal(got)
		if len(out) > limit || !got.Truncated || got.TruncationNote == "" || !utf8.ValidString(got.Body) {
			t.Fatalf("limit %d: len=%d truncated=%t note=%q", limit, len(out), got.Truncated, got.TruncationNote)
		}
	}
	if len(result.Comments) != 5 || result.Body == "" {
		t.Fatal("the caller's result must not be mutated")
	}
}
