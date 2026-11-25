/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package oversight turns submitted oversight decisions into stored records
// and stored records into the read models the API serves. Everything here is
// deterministic: no model is called and no stored text is interpreted.
package oversight

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/events"
	oversightv1 "github.com/orka-agents/orka/pkg/oversight/v1"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// Facet kinds stored beside the signature features. Raw values keep what the
// sensor reported; the signature kinds keep the normalized forms.
const (
	FacetPathPrefix       = "path_prefix"
	FacetDestClass        = "dest_class"
	FacetDisposition      = "disposition"
	FacetSignal           = "signal"
	FacetBound            = "bound"
	FacetWorkloadIdentity = "workload_identity"
	FacetProducer         = "producer"
)

// Prepare validates a submitted decision, redacts and bounds its text, and
// returns the decision as stored together with its record. Seq and
// RecordedAt are cleared: the store assigns them.
func Prepare(namespace string, d oversightv1.OversightDecision) (oversightv1.OversightDecision, storagev1.OversightDecisionRecord, error) {
	d.Namespace, d.Seq, d.RecordedAt = namespace, 0, time.Time{}
	d.ObservedAt = d.ObservedAt.UTC()
	if err := d.Validate(); err != nil {
		return d, storagev1.OversightDecisionRecord{}, err
	}
	redactDecision(&d)
	body, err := json.Marshal(d)
	if err != nil {
		return d, storagev1.OversightDecisionRecord{}, fmt.Errorf("encode decision: %w", err)
	}
	if len(body) > storagev1.MaxOversightBodyBytes {
		return d, storagev1.OversightDecisionRecord{}, fmt.Errorf("%w: decision %q encodes to %d bytes, over %d",
			storagev1.ErrTooLarge, d.ID, len(body), storagev1.MaxOversightBodyBytes)
	}
	sum := sha256.Sum256(body)
	signature := d.Signature()
	narrative := Narrative(&d)
	record := storagev1.OversightDecisionRecord{
		Namespace: namespace, ID: d.ID, TaskName: d.TaskName, AgentName: d.AgentName,
		WorkloadIdentity: d.WorkloadIdentity, RunID: d.RunID, ToolName: d.Action.ToolName,
		Disposition: string(d.Disposition), WouldHavePaused: d.WouldHavePaused, MaxScore: d.MaxProbability(),
		SignatureHash: oversightv1.SignatureHash(signature), ObservedAt: d.ObservedAt,
		Facets: facets(&d, signature), ActionNarrative: narrative, SearchText: searchText(&d, narrative),
		Digest: "sha256:" + hex.EncodeToString(sum[:]), Body: body,
	}
	return d, record, nil
}

func redactText(value string, limit int) string {
	redacted, _, _ := events.RedactAndTruncateExecutionEventText(value, limit)
	// The shared helper bounds runes; the contract's limits are bytes.
	return truncateBytes(redacted, limit)
}

func redactAll(values []string, limit int) {
	for i := range values {
		values[i] = redactText(values[i], limit)
	}
}

func redactCounted(values []oversightv1.CountedValue) {
	for i := range values {
		values[i].Value = redactText(values[i].Value, oversightv1.MaxEffectValueBytes)
	}
}

// redactDecision runs every free-text field through Orka's shared redaction
// and bounds it. Identifiers and machine codes are already bounded by
// validation; they are redacted too, because they are just as untrusted.
func redactDecision(d *oversightv1.OversightDecision) {
	short := oversightv1.MaxShortTextBytes
	a := &d.Action
	a.RequestSummary = redactText(a.RequestSummary, oversightv1.MaxRequestSummaryBytes)
	a.ToolCallID = redactText(a.ToolCallID, short)
	redactAll(a.PriorActionIDs, oversightv1.MaxIDBytes)
	redactAll(a.TaintFlags, short)
	redactAll(a.InheritedTaint, short)
	redactCounted(a.Effects.Binaries)
	redactCounted(a.Effects.Paths)
	redactCounted(a.Effects.Destinations)
	for i := range a.Effects.NotableEvents {
		e := &a.Effects.NotableEvents[i]
		e.Binary = redactText(e.Binary, oversightv1.MaxEffectValueBytes)
		e.Path = redactText(e.Path, oversightv1.MaxEffectValueBytes)
		e.Dest = redactText(e.Dest, oversightv1.MaxEffectValueBytes)
		e.Args = redactText(e.Args, oversightv1.MaxEffectValueBytes)
		e.Outcome = redactText(e.Outcome, short)
	}
	redactAll(d.EscalatedBecause, short)
	redactAll(d.Coverage.BlindSpots, short)
	for i := range d.CharterFindings {
		d.CharterFindings[i].Sentence = redactText(d.CharterFindings[i].Sentence, oversightv1.MaxCharterSentenceBytes)
	}
	for i := range d.Advisors {
		d.Advisors[i].Summary = redactText(d.Advisors[i].Summary, oversightv1.MaxAdvisorSummaryBytes)
		d.Advisors[i].ReleaseDeniedReason = redactText(d.Advisors[i].ReleaseDeniedReason, short)
	}
}

func facets(d *oversightv1.OversightDecision, signature []oversightv1.Feature) []storagev1.OversightFacet {
	var out []storagev1.OversightFacet
	add := func(kind, value string) {
		if value != "" {
			out = append(out, storagev1.OversightFacet{Kind: kind, Value: value})
		}
	}
	for _, feature := range signature {
		switch feature.Kind {
		case oversightv1.FeaturePath:
			add(FacetPathPrefix, feature.Value)
		case oversightv1.FeatureDest:
			add(FacetDestClass, feature.Value)
		default:
			add(feature.Kind, feature.Value)
		}
	}
	for _, p := range d.Action.Effects.Paths {
		add(oversightv1.FeaturePath, p.Value)
	}
	for _, dest := range d.Action.Effects.Destinations {
		add(oversightv1.FeatureDest, dest.Value)
		add(oversightv1.FeatureDestDomain, oversightv1.DestinationDomain(dest.Value))
	}
	for _, event := range d.Action.Effects.NotableEvents {
		add(oversightv1.FeatureBinary, oversightv1.BinaryName(event.Binary))
		add(oversightv1.FeaturePath, event.Path)
		if event.Dest != "" {
			add(oversightv1.FeatureDest, event.Dest)
			add(oversightv1.FeatureDestDomain, oversightv1.DestinationDomain(event.Dest))
		}
	}
	add(FacetDisposition, string(d.Disposition))
	add(FacetSignal, string(oversightv1.SignalOf(d)))
	add(FacetWorkloadIdentity, d.WorkloadIdentity)
	for _, finding := range d.CharterFindings {
		add(FacetBound, finding.BoundID)
		add(FacetProducer, finding.ProducerRef)
	}
	for _, scorer := range d.Scorers {
		add(FacetProducer, scorer.ProviderRef)
	}
	for _, advisor := range d.Advisors {
		add(FacetProducer, advisor.ProviderRef)
	}
	seen := map[storagev1.OversightFacet]bool{}
	kept := out[:0]
	for _, facet := range out {
		if !seen[facet] && len(kept) < storagev1.MaxOversightFacets {
			seen[facet] = true
			kept = append(kept, facet)
		}
	}
	return kept
}

func values(counted []oversightv1.CountedValue) []string {
	out := make([]string, 0, len(counted))
	for _, value := range counted {
		out = append(out, value.Value)
	}
	return out
}

// Narrative renders a fixed plain-language account of an action from its
// redacted envelope. It is a template, not a summary: it says only what the
// record says, and changing the template rebuilds it for every record.
func Narrative(d *oversightv1.OversightDecision) string {
	var b strings.Builder
	subject := "Task " + d.TaskName
	if d.AgentName != "" {
		subject += " (agent " + d.AgentName + ")"
	}
	fmt.Fprintf(&b, "%s ran %s", subject, d.Action.ToolName)
	if summary := strings.TrimSpace(d.Action.RequestSummary); summary != "" {
		if short := truncateBytes(summary, 160); short != summary {
			summary = short + "…"
		}
		fmt.Fprintf(&b, " (%q)", summary)
	}
	effects := d.Action.Effects
	if binaries := values(effects.Binaries); len(binaries) > 0 {
		fmt.Fprintf(&b, "; started %s", strings.Join(binaries, ", "))
	}
	if paths := values(effects.Paths); len(paths) > 0 {
		fmt.Fprintf(&b, "; touched %s", strings.Join(paths, ", "))
	}
	if dests := values(effects.Destinations); len(dests) > 0 {
		fmt.Fprintf(&b, "; connected to %s", strings.Join(dests, ", "))
	}
	if effects.BlockedCount > 0 {
		fmt.Fprintf(&b, "; %d attempt(s) blocked", effects.BlockedCount)
	}
	taint := append(append([]string(nil), d.Action.TaintFlags...), d.Action.InheritedTaint...)
	if len(taint) == 0 {
		b.WriteString("; taint: none.")
	} else {
		fmt.Fprintf(&b, "; taint: %s.", strings.Join(taint, ", "))
	}
	return truncateBytes(b.String(), storagev1.MaxOversightNarrativeBytes)
}

func searchText(d *oversightv1.OversightDecision, narrative string) string {
	effects := d.Action.Effects
	parts := make([]string, 0, 5+len(effects.Binaries)+len(effects.Paths)+len(effects.Destinations)+4*len(effects.NotableEvents)+
		len(d.Advisors)+len(d.CharterFindings))
	parts = append(parts, d.TaskName, d.AgentName, d.Action.ToolName, d.Action.RequestSummary, narrative)
	parts = append(parts, values(effects.Binaries)...)
	parts = append(parts, values(effects.Paths)...)
	parts = append(parts, values(effects.Destinations)...)
	for _, event := range effects.NotableEvents {
		parts = append(parts, event.Args, event.Path, event.Dest, event.Outcome)
	}
	for _, advisor := range d.Advisors {
		parts = append(parts, advisor.Summary)
	}
	for _, finding := range d.CharterFindings {
		parts = append(parts, finding.Sentence)
	}
	return truncateBytes(strings.ToLower(strings.Join(parts, "\n")), storagev1.MaxOversightSearchTextBytes)
}

// truncateBytes bounds s to limit bytes without splitting a UTF-8 sequence.
func truncateBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	s = s[:limit]
	for len(s) > 0 && !utf8.ValidString(s[max(0, len(s)-utf8.UTFMax):]) {
		s = s[:len(s)-1]
	}
	return s
}

// RedactNote redacts and bounds a person's feedback note.
func RedactNote(note string) string {
	return redactText(note, oversightv1.MaxFeedbackNoteBytes)
}
