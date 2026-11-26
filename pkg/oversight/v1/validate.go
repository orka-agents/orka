/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package v1

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
)

// Limits on one decision. Lists over a limit are refused, not truncated, so
// a record never silently loses evidence; text over a limit is truncated on
// ingest after redaction.
const (
	MaxBatchDecisions       = 100
	MaxIDBytes              = 253
	MaxNameBytes            = 253
	MaxShortTextBytes       = 256
	MaxRequestSummaryBytes  = 1 << 10
	MaxAdvisorSummaryBytes  = 2 << 10
	MaxEffectValueBytes     = 512
	MaxCountedValues        = 16
	MaxNotableEvents        = 32
	MaxEffectKinds          = 32
	MaxPriorActions         = 16
	MaxFlags                = 32
	MaxScorers              = 8
	MaxAdvisors             = 8
	MaxScoresPerScorer      = 64
	MaxCitedEvents          = 32
	MaxFeedbackNoteBytes    = 1 << 10
	MaxCharterSentenceBytes = 1 << 10
)

// ErrInvalid marks a decision or feedback the contract refuses.
var ErrInvalid = errors.New("invalid oversight record")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Validate checks a submitted decision's shape and bounds. It does not check
// Namespace, Seq or RecordedAt, which the server assigns.
//
//nolint:gocyclo // One flat list of independent field checks.
func (d *OversightDecision) Validate() error {
	if err := requiredText("id", d.ID, MaxIDBytes); err != nil {
		return err
	}
	if strings.ContainsAny(d.ID, "/?#") {
		return invalid("id must not contain '/', '?' or '#'")
	}
	if err := requiredText("taskName", d.TaskName, MaxNameBytes); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"taskUID": d.TaskUID, "agentName": d.AgentName, "workloadIdentity": d.WorkloadIdentity, "runID": d.RunID,
		"charterRevision": d.CharterRevision, "concernsRef": d.ConcernsRef,
		"concernsRevision": d.ConcernsRevision, "concernsDigest": d.ConcernsDigest, "approvalRef": d.ApprovalRef,
		"policyRevision": d.PolicyRevision,
	} {
		if len(value) > MaxShortTextBytes {
			return invalid("%s is longer than %d bytes", name, MaxShortTextBytes)
		}
	}
	if d.ActionSeq < 0 {
		return invalid("actionSeq must not be negative")
	}
	if d.ObservedAt.IsZero() {
		return invalid("observedAt is required")
	}
	if d.Mode != ModeAudit && d.Mode != ModeGate {
		return invalid("mode must be audit or gate")
	}
	switch d.OutageFallback {
	case "", OutageFallbackFailClosed, OutageFallbackAuditOnly:
	default:
		return invalid("outageFallback must be fail_closed or audit_only")
	}
	if !slices.Contains(Dispositions, d.Disposition) {
		return invalid("disposition %q is not known", d.Disposition)
	}
	switch d.EvidenceLevel {
	case EvidenceLevelIssued, EvidenceLevelApplied, EvidenceLevelObserved, EvidenceLevelAttested:
	default:
		return invalid("evidenceLevel %q is not known", d.EvidenceLevel)
	}
	if d.EvidenceLevel == EvidenceLevelAttested && d.Attestation == nil {
		return invalid("evidenceLevel attested needs an attestation")
	}
	if err := d.Coverage.validate(); err != nil {
		return err
	}
	if err := d.Action.validate(); err != nil {
		return err
	}
	if err := flags("escalatedBecause", d.EscalatedBecause); err != nil {
		return err
	}
	if d.Tier0 != nil {
		if err := flags("tier0.rules", d.Tier0.Rules); err != nil {
			return err
		}
	}
	if len(d.CharterFindings) > MaxFlags {
		return invalid("more than %d charter findings", MaxFlags)
	}
	for _, finding := range d.CharterFindings {
		if err := requiredText("charterFindings.producerRef", finding.ProducerRef, MaxShortTextBytes); err != nil {
			return err
		}
		if err := requiredText("charterFindings.boundID", finding.BoundID, MaxShortTextBytes); err != nil {
			return err
		}
		if err := requiredText("charterFindings.sentence", finding.Sentence, MaxCharterSentenceBytes); err != nil {
			return err
		}
		if len(finding.CitedEventIDs) > MaxCitedEvents {
			return invalid("more than %d cited events", MaxCitedEvents)
		}
	}
	if len(d.Scorers) > MaxScorers {
		return invalid("more than %d scorers", MaxScorers)
	}
	for i := range d.Scorers {
		if err := d.Scorers[i].validate(); err != nil {
			return fmt.Errorf("scorers[%d]: %w", i, err)
		}
	}
	if len(d.Advisors) > MaxAdvisors {
		return invalid("more than %d advisors", MaxAdvisors)
	}
	for i := range d.Advisors {
		if err := d.Advisors[i].validate(); err != nil {
			return fmt.Errorf("advisors[%d]: %w", i, err)
		}
	}
	return nil
}

func (c *ActionCoverage) validate() error {
	switch c.StreamState {
	case StreamStateOK, StreamStatePartial, StreamStateMissing:
	default:
		return invalid("coverage.streamState %q is not known", c.StreamState)
	}
	switch c.SensorState {
	case SensorStateEvents, SensorStateNone, SensorStateUnavailable, SensorStateUnattributed:
	default:
		return invalid("coverage.sensorState %q is not known", c.SensorState)
	}
	if c.UnattributedEvents < 0 {
		return invalid("coverage.unattributedEvents must not be negative")
	}
	if len(c.AttributionCounts) > MaxEffectKinds {
		return invalid("coverage.attributionCounts has more than %d entries", MaxEffectKinds)
	}
	return flags("coverage.blindSpots", c.BlindSpots)
}

func (a *ActionEnvelope) validate() error {
	if err := requiredText("action.toolName", a.ToolName, MaxShortTextBytes); err != nil {
		return err
	}
	if len(a.ToolCallID) > MaxShortTextBytes {
		return invalid("action.toolCallID is longer than %d bytes", MaxShortTextBytes)
	}
	if len(a.PriorActionIDs) > MaxPriorActions {
		return invalid("action.priorActionIDs has more than %d entries", MaxPriorActions)
	}
	for _, id := range a.PriorActionIDs {
		if len(id) > MaxIDBytes {
			return invalid("action.priorActionIDs entry is longer than %d bytes", MaxIDBytes)
		}
	}
	if err := flags("action.taintFlags", a.TaintFlags); err != nil {
		return err
	}
	if err := flags("action.inheritedTaint", a.InheritedTaint); err != nil {
		return err
	}
	e := &a.Effects
	if len(e.Counts) > MaxEffectKinds {
		return invalid("action.effects.counts has more than %d kinds", MaxEffectKinds)
	}
	for kind, count := range e.Counts {
		if len(kind) > MaxShortTextBytes || count < 0 {
			return invalid("action.effects.counts has an invalid entry")
		}
	}
	for name, values := range map[string][]CountedValue{
		"binaries": e.Binaries, "paths": e.Paths, "destinations": e.Destinations,
	} {
		if len(values) > MaxCountedValues {
			return invalid("action.effects.%s has more than %d entries", name, MaxCountedValues)
		}
		for _, value := range values {
			if value.Count < 0 {
				return invalid("action.effects.%s count must not be negative", name)
			}
		}
	}
	if len(e.NotableEvents) > MaxNotableEvents {
		return invalid("action.effects.notableEvents has more than %d entries", MaxNotableEvents)
	}
	for _, event := range e.NotableEvents {
		if event.ID == "" || len(event.ID) > MaxIDBytes || event.Kind == "" || len(event.Kind) > MaxShortTextBytes {
			return invalid("action.effects.notableEvents entries need a bounded id and kind")
		}
	}
	if e.BlockedCount < 0 || e.OmittedEvents < 0 || e.BytesSent != nil && *e.BytesSent < 0 {
		return invalid("action.effects counts must not be negative")
	}
	return nil
}

func (s *ScorerEvaluation) validate() error {
	if err := requiredText("providerRef", s.ProviderRef, MaxShortTextBytes); err != nil {
		return err
	}
	if err := modelIdentity(s.ModelID, s.ModelFamily, s.ModelDigest, s.PromptRevision, s.TrustState); err != nil {
		return err
	}
	if err := evaluationStatus(s.Status, false); err != nil {
		return err
	}
	if len(s.CitedEventIDs) > MaxCitedEvents {
		return invalid("more than %d cited events", MaxCitedEvents)
	}
	if len(s.Scores) > MaxScoresPerScorer {
		return invalid("more than %d scores", MaxScoresPerScorer)
	}
	for _, score := range s.Scores {
		if err := requiredText("questionID", score.QuestionID, MaxShortTextBytes); err != nil {
			return err
		}
		if len(score.Label) > MaxShortTextBytes {
			return invalid("score label is longer than %d bytes", MaxShortTextBytes)
		}
		if !probability(score.Probability) || score.Threshold != nil && !probability(*score.Threshold) {
			return invalid("probabilities and thresholds must be within [0, 1]")
		}
	}
	return nil
}

func (a *AdvisorEvaluation) validate() error {
	if err := requiredText("providerRef", a.ProviderRef, MaxShortTextBytes); err != nil {
		return err
	}
	if err := modelIdentity(a.ModelID, a.ModelFamily, a.ModelDigest, a.PromptRevision, a.TrustState); err != nil {
		return err
	}
	if err := evaluationStatus(a.Status, true); err != nil {
		return err
	}
	if len(a.Verdict) > MaxShortTextBytes || len(a.ReleaseDeniedReason) > MaxShortTextBytes {
		return invalid("advisor verdict and releaseDeniedReason are limited to %d bytes", MaxShortTextBytes)
	}
	if len(a.CitedEventIDs) > MaxCitedEvents {
		return invalid("more than %d cited events", MaxCitedEvents)
	}
	return flags("concerns", a.Concerns)
}

func modelIdentity(id, family, digest, prompt, trust string) error {
	if err := requiredText("modelID", id, MaxShortTextBytes); err != nil {
		return err
	}
	for _, value := range []string{family, digest, prompt} {
		if len(value) > MaxShortTextBytes {
			return invalid("model identity fields are limited to %d bytes", MaxShortTextBytes)
		}
	}
	switch trust {
	case "", TrustStateQualified, TrustStateShadow, TrustStateUntrustedMonitor:
		return nil
	default:
		return invalid("trustState %q is not known", trust)
	}
}

func evaluationStatus(status string, advisor bool) error {
	switch status {
	case EvaluationStatusOK, EvaluationStatusUnavailable, EvaluationStatusTimeout, EvaluationStatusError:
		return nil
	case EvaluationStatusContentFiltered:
		if advisor {
			return nil
		}
	}
	return invalid("status %q is not known", status)
}

func requiredText(name, value string, limit int) error {
	if strings.TrimSpace(value) == "" {
		return invalid("%s is required", name)
	}
	if len(value) > limit {
		return invalid("%s is longer than %d bytes", name, limit)
	}
	return nil
}

func flags(name string, values []string) error {
	if len(values) > MaxFlags {
		return invalid("%s has more than %d entries", name, MaxFlags)
	}
	for _, value := range values {
		if strings.TrimSpace(value) == "" || len(value) > MaxShortTextBytes {
			return invalid("%s entries must be non-empty and at most %d bytes", name, MaxShortTextBytes)
		}
	}
	return nil
}

func probability(p float64) bool {
	return !math.IsNaN(p) && p >= 0 && p <= 1
}

// Validate checks a feedback label and note.
func (f *DecisionFeedback) Validate() error {
	switch f.Label {
	case FeedbackRealProblem, FeedbackFalseAlarm, FeedbackUnclear:
	default:
		return invalid("label must be real_problem, false_alarm or unclear")
	}
	if len(f.Note) > MaxFeedbackNoteBytes {
		return invalid("note is longer than %d bytes", MaxFeedbackNoteBytes)
	}
	return nil
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
