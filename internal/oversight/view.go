/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package oversight

import (
	"encoding/json"
	"fmt"
	"slices"

	oversightv1 "github.com/orka-agents/orka/pkg/oversight/v1"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// View decodes a stored record into the decision the API serves, with its
// store-assigned fields and latest label.
func View(record storagev1.OversightDecisionRecord, feedback *storagev1.OversightFeedbackRecord) (oversightv1.DecisionView, error) {
	var d oversightv1.OversightDecision
	if err := json.Unmarshal(record.Body, &d); err != nil {
		return oversightv1.DecisionView{}, fmt.Errorf("decode oversight decision %q: %w", record.ID, err)
	}
	d.Seq, d.Namespace, d.RecordedAt = record.Seq, record.Namespace, record.RecordedAt.UTC()
	view := oversightv1.DecisionView{
		OversightDecision: d, Signal: oversightv1.SignalOf(&d),
		SignatureHash: record.SignatureHash, ActionNarrative: record.ActionNarrative,
	}
	if feedback != nil {
		view.Feedback = &oversightv1.DecisionFeedback{
			Label: oversightv1.FeedbackLabel(feedback.Label), Note: feedback.Note,
			Reviewer: feedback.Reviewer, RecordedAt: feedback.RecordedAt.UTC(),
		}
	}
	return view, nil
}

// Views decodes records with their labels, in order.
func Views(records []storagev1.OversightDecisionRecord, feedback map[string]storagev1.OversightFeedbackRecord) ([]oversightv1.DecisionView, error) {
	views := make([]oversightv1.DecisionView, 0, len(records))
	for _, record := range records {
		var label *storagev1.OversightFeedbackRecord
		if found, ok := feedback[record.ID]; ok {
			label = &found
		}
		view, err := View(record, label)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

// RecordIDs returns the records' decision IDs.
func RecordIDs(records []storagev1.OversightDecisionRecord) []string {
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.ID)
	}
	return ids
}

// SimilarCandidates returns the filter that recalls decisions sharing any
// distinguishing feature of signature. The tool and generic effect kinds are
// left out: they alone would recall nearly every decision.
func SimilarCandidates(namespace string, signature []oversightv1.Feature) []storagev1.OversightFacetMatch {
	matches := make([]storagev1.OversightFacetMatch, 0, len(signature))
	for _, feature := range signature {
		kind := feature.Kind
		switch kind {
		case oversightv1.FeatureTool, oversightv1.FeatureEffect:
			continue
		case oversightv1.FeaturePath:
			kind = FacetPathPrefix
		case oversightv1.FeatureDest:
			kind = FacetDestClass
		}
		matches = append(matches, storagev1.OversightFacetMatch{Kind: kind, Value: feature.Value})
	}
	return matches
}

// RankSimilar orders candidates by their likeness to source: an identical
// signature first, then by Jaccard overlap, then newest first. The source
// itself and candidates sharing nothing are left out.
func RankSimilar(source oversightv1.DecisionView, candidates []oversightv1.DecisionView, limit int) []oversightv1.SimilarItem {
	signature := source.Signature()
	var items []oversightv1.SimilarItem
	for _, candidate := range candidates {
		if candidate.ID == source.ID {
			continue
		}
		score, shared := oversightv1.Jaccard(signature, candidate.Signature())
		if len(shared) == 0 {
			continue
		}
		items = append(items, oversightv1.SimilarItem{
			Decision: candidate, Score: score, Shared: shared,
			ExactSignature: candidate.SignatureHash != "" && candidate.SignatureHash == source.SignatureHash,
		})
	}
	slices.SortStableFunc(items, func(a, b oversightv1.SimilarItem) int {
		switch {
		case a.ExactSignature != b.ExactSignature:
			if a.ExactSignature {
				return -1
			}
			return 1
		case a.Score != b.Score:
			if a.Score > b.Score {
				return -1
			}
			return 1
		default:
			return int(b.Decision.Seq - a.Decision.Seq)
		}
	})
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items
}

// Grouping keys for Group.
const (
	GroupByAgent = "agent"
	GroupByTask  = "task"
)

// Group summarizes views by agent or Task. By agent, decisions without an
// agent share the group with an empty key, which never merges with a named
// Agent and is not a searchable agent: term.
func Group(views []oversightv1.DecisionView, by string) []oversightv1.DecisionGroup {
	index := map[string]int{}
	var groups []oversightv1.DecisionGroup
	for _, view := range views {
		key := view.TaskName
		if by == GroupByAgent {
			key = view.AgentName
		}
		i, ok := index[key]
		if !ok {
			i = len(groups)
			index[key] = i
			groups = append(groups, oversightv1.DecisionGroup{Key: key, FirstSeen: view.ObservedAt, LastSeen: view.ObservedAt})
		}
		group := &groups[i]
		group.Count++
		switch view.Signal {
		case oversightv1.SignalRisky:
			group.Risky++
		case oversightv1.SignalAgainstCharter:
			group.AgainstCharter++
		}
		if view.ObservedAt.Before(group.FirstSeen) {
			group.FirstSeen = view.ObservedAt
		}
		if view.ObservedAt.After(group.LastSeen) {
			group.LastSeen = view.ObservedAt
		}
		if by == GroupByAgent && len(group.Tasks) < 5 && !slices.Contains(group.Tasks, view.TaskName) {
			group.Tasks = append(group.Tasks, view.TaskName)
		}
	}
	slices.SortStableFunc(groups, func(a, b oversightv1.DecisionGroup) int {
		if a.Count != b.Count {
			return b.Count - a.Count
		}
		return b.LastSeen.Compare(a.LastSeen)
	})
	return groups
}
