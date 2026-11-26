package storagetest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

var oversightObservedAt = time.Date(2026, time.November, 25, 1, 0, 0, 0, time.UTC)

func testOversightDecision(id, task, digest string, offset time.Duration, facets ...storagev1.OversightFacet) storagev1.OversightDecisionRecord {
	return storagev1.OversightDecisionRecord{
		ID: id, TaskName: task, AgentName: "agent-" + task, WorkloadIdentity: "sa:" + task, RunID: "run-1",
		ToolName: "code_exec", Disposition: "allow", MaxScore: 0.2, SignatureHash: "sig-" + digest,
		ObservedAt: oversightObservedAt.Add(offset), Facets: facets,
		SearchText: "Ran Python and reached Example.com", ActionNarrative: "Task " + task + " ran code_exec.",
		Digest: digest, Body: []byte(`{"id":"` + id + `","digest":"` + digest + `"}`),
	}
}

func oversightIDs(records []storagev1.OversightDecisionRecord) []string {
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.ID)
	}
	return ids
}

func runOversight(t *testing.T, newProvider StateFactory) {
	t.Run("decisions append idempotently and refuse a changed repeat", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		results, err := p.AppendOversightDecisions(ctx, "team", []storagev1.OversightDecisionRecord{
			testOversightDecision("a@0", "a", "d1", 0, storagev1.OversightFacet{Kind: "dest", Value: "example.com:443"}),
			testOversightDecision("a@1", "a", "d2", time.Second),
		})
		require.NoError(t, err)
		require.Len(t, results, 2)
		require.False(t, results[0].Duplicate)
		require.Greater(t, results[1].Seq, results[0].Seq, "Seq increases in batch order")

		again, err := p.AppendOversightDecisions(ctx, "team", []storagev1.OversightDecisionRecord{
			testOversightDecision("a@0", "a", "d1", 0),
		})
		require.NoError(t, err)
		require.Equal(t, []storagev1.OversightAppendResult{{ID: "a@0", Seq: results[0].Seq, Duplicate: true}}, again)

		_, err = p.AppendOversightDecisions(ctx, "team", []storagev1.OversightDecisionRecord{
			testOversightDecision("b@0", "b", "d3", 0),
			testOversightDecision("a@1", "a", "changed", time.Second),
		})
		require.ErrorIs(t, err, storagev1.ErrDuplicateMismatch)
		_, err = p.GetOversightDecision(ctx, "team", "b@0")
		require.ErrorIs(t, err, storagev1.ErrNotFound, "a refused batch stores nothing")

		got, err := p.GetOversightDecision(ctx, "team", "a@0")
		require.NoError(t, err)
		require.Equal(t, results[0].Seq, got.Seq)
		require.Equal(t, "agent-a", got.AgentName)
		require.Equal(t, "sa:a", got.WorkloadIdentity)
		require.Equal(t, "Task a ran code_exec.", got.ActionNarrative)
		require.True(t, got.ObservedAt.Equal(oversightObservedAt))
		require.False(t, got.RecordedAt.IsZero())
		require.Equal(t, []storagev1.OversightFacet{{Kind: "dest", Value: "example.com:443"}}, got.Facets)
		require.JSONEq(t, `{"id":"a@0","digest":"d1"}`, string(got.Body))
		_, err = p.GetOversightDecision(ctx, "other", "a@0")
		require.ErrorIs(t, err, storagev1.ErrNotFound, "decisions are per namespace")
	})

	t.Run("invalid batches are refused", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		_, err := p.AppendOversightDecisions(ctx, "team", nil)
		require.ErrorIs(t, err, storagev1.ErrValidation)
		missing := testOversightDecision("a@0", "a", "", 0)
		_, err = p.AppendOversightDecisions(ctx, "team", []storagev1.OversightDecisionRecord{missing})
		require.ErrorIs(t, err, storagev1.ErrValidation)
		big := testOversightDecision("a@0", "a", "d", 0)
		big.Body = make([]byte, storagev1.MaxOversightBodyBytes+1)
		_, err = p.AppendOversightDecisions(ctx, "team", []storagev1.OversightDecisionRecord{big})
		require.ErrorIs(t, err, storagev1.ErrTooLarge)
		foreign := testOversightDecision("a@0", "a", "d", 0)
		foreign.Namespace = "other"
		_, err = p.AppendOversightDecisions(ctx, "team", []storagev1.OversightDecisionRecord{foreign})
		require.ErrorIs(t, err, storagev1.ErrValidation)
	})

	t.Run("filters select by facets, text, fields and cursor", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		hf := storagev1.OversightFacet{Kind: "dest", Value: "cdn.example.com:443"}
		k2 := storagev1.OversightFacet{Kind: "concern", Value: "K2"}
		flagged := testOversightDecision("b@0", "b", "d2", time.Minute, hf, k2)
		flagged.Disposition, flagged.MaxScore, flagged.WouldHavePaused = "audit_flagged", 0.9, true
		flagged.SearchText = "pushed an image to registry"
		_, err := p.AppendOversightDecisions(ctx, "team", []storagev1.OversightDecisionRecord{
			testOversightDecision("a@0", "a", "d1", 0, k2), flagged, testOversightDecision("c@0", "c", "d3", 2*time.Minute),
		})
		require.NoError(t, err)
		_, err = p.AppendOversightDecisions(ctx, "other", []storagev1.OversightDecisionRecord{
			testOversightDecision("x@0", "x", "dx", 0, hf),
		})
		require.NoError(t, err)

		list := func(filter storagev1.OversightDecisionFilter) []string {
			t.Helper()
			filter.Namespace = "team"
			records, err := p.ListOversightDecisions(ctx, filter)
			require.NoError(t, err)
			return oversightIDs(records)
		}
		half := 0.5
		paused := true
		require.Equal(t, []string{"a@0", "b@0", "c@0"}, list(storagev1.OversightDecisionFilter{}))
		require.Equal(t, []string{"c@0", "b@0", "a@0"}, list(storagev1.OversightDecisionFilter{Descending: true}))
		require.Equal(t, []string{"b@0"}, list(storagev1.OversightDecisionFilter{Facets: []storagev1.OversightFacetMatch{{Kind: "dest", Value: "cdn.example.com:443"}}}))
		require.Equal(t, []string{"b@0"}, list(storagev1.OversightDecisionFilter{Facets: []storagev1.OversightFacetMatch{{Kind: "dest", Value: "cdn.*", Glob: true}}}))
		require.Empty(t, list(storagev1.OversightDecisionFilter{Facets: []storagev1.OversightFacetMatch{{Kind: "dest", Value: "cdn.%*", Glob: true}}}), "globs match other characters literally")
		require.Equal(t, []string{"b@0"}, list(storagev1.OversightDecisionFilter{Facets: []storagev1.OversightFacetMatch{{Kind: "dest", Value: "*.example.com:*", Glob: true}}}))
		require.Equal(t, []string{"a@0", "b@0"}, list(storagev1.OversightDecisionFilter{Facets: []storagev1.OversightFacetMatch{{Kind: "concern", Value: "K2"}}}))
		require.Equal(t, []string{"a@0", "b@0"}, list(storagev1.OversightDecisionFilter{AnyFacets: []storagev1.OversightFacetMatch{{Kind: "concern", Value: "K2"}, {Kind: "dest", Value: "nope"}}}))
		require.Equal(t, []string{"b@0"}, list(storagev1.OversightDecisionFilter{Text: []string{"IMAGE", "registry"}}))
		require.Equal(t, []string{"b@0"}, list(storagev1.OversightDecisionFilter{Dispositions: []string{"audit_flagged", "paused"}}))
		require.Equal(t, []string{"b@0"}, list(storagev1.OversightDecisionFilter{MinScore: &half}))
		require.Equal(t, []string{"b@0"}, list(storagev1.OversightDecisionFilter{WouldHavePaused: &paused}))
		require.Equal(t, []string{"b@0"}, list(storagev1.OversightDecisionFilter{AgentNames: []string{"agent-b"}}))
		require.Equal(t, []string{"b@0"}, list(storagev1.OversightDecisionFilter{WorkloadIdentities: []string{"sa:b"}}))
		require.Equal(t, []string{"a@0"}, list(storagev1.OversightDecisionFilter{TaskNames: []string{"a"}}))
		require.Equal(t, []string{"c@0"}, list(storagev1.OversightDecisionFilter{SignatureHash: "sig-d3"}))
		require.Equal(t, []string{"b@0", "c@0"}, list(storagev1.OversightDecisionFilter{ObservedSince: oversightObservedAt.Add(time.Minute)}))
		require.Equal(t, []string{"a@0"}, list(storagev1.OversightDecisionFilter{ObservedBefore: oversightObservedAt.Add(time.Minute)}))
		all, err := p.ListOversightDecisions(ctx, storagev1.OversightDecisionFilter{Namespace: "team"})
		require.NoError(t, err)
		require.Equal(t, []string{"b@0", "c@0"}, list(storagev1.OversightDecisionFilter{AfterSeq: all[0].Seq}))
		require.Equal(t, []string{"a@0"}, list(storagev1.OversightDecisionFilter{BeforeSeq: all[1].Seq}))
		require.Equal(t, []string{"a@0"}, list(storagev1.OversightDecisionFilter{Limit: 1}))
		_, err = p.ListOversightDecisions(ctx, storagev1.OversightDecisionFilter{})
		require.ErrorIs(t, err, storagev1.ErrValidation, "a list names its namespace")
	})

	t.Run("feedback is a ledger whose latest label wins", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		_, err := p.AppendOversightDecisions(ctx, "team", []storagev1.OversightDecisionRecord{
			testOversightDecision("a@0", "a", "d1", 0), testOversightDecision("b@0", "b", "d2", time.Second),
		})
		require.NoError(t, err)
		at := oversightObservedAt.Add(time.Hour)
		stored, err := p.RecordOversightFeedback(ctx, "team", []storagev1.OversightFeedbackRecord{
			{DecisionID: "a@0", Label: "false_alarm", Note: "expected", Reviewer: "alice"},
		}, at)
		require.NoError(t, err)
		require.Len(t, stored, 1)
		require.True(t, stored[0].RecordedAt.Equal(at))
		_, err = p.RecordOversightFeedback(ctx, "team", []storagev1.OversightFeedbackRecord{
			{DecisionID: "a@0", Label: "real_problem", Reviewer: "bob"},
		}, at.Add(time.Minute))
		require.NoError(t, err)
		_, err = p.RecordOversightFeedback(ctx, "team", []storagev1.OversightFeedbackRecord{
			{DecisionID: "b@0", Label: "unclear"}, {DecisionID: "missing", Label: "unclear"},
		}, at)
		require.ErrorIs(t, err, storagev1.ErrNotFound)

		latest, err := p.ListLatestOversightFeedback(ctx, "team", []string{"a@0", "b@0"})
		require.NoError(t, err)
		require.Len(t, latest, 1, "a refused batch records nothing")
		require.Equal(t, "real_problem", latest["a@0"].Label)
		require.Equal(t, "bob", latest["a@0"].Reviewer)

		list := func(feedback string) []string {
			records, err := p.ListOversightDecisions(ctx, storagev1.OversightDecisionFilter{Namespace: "team", Feedback: feedback})
			require.NoError(t, err)
			return oversightIDs(records)
		}
		require.Equal(t, []string{"b@0"}, list(storagev1.OversightFeedbackUnreviewed))
		require.Equal(t, []string{"a@0"}, list(storagev1.OversightFeedbackReviewed))
		require.Equal(t, []string{"a@0"}, list("real_problem"))
		require.Empty(t, list("false_alarm"), "only the latest label counts")
		other, err := p.ListLatestOversightFeedback(ctx, "other", []string{"a@0"})
		require.NoError(t, err)
		require.Empty(t, other)
	})
}
