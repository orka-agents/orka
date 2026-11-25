/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package oversight

import (
	"fmt"
	"slices"
	"strings"
	"time"

	oversightv1 "github.com/orka-agents/orka/pkg/oversight/v1"
)

// Posture reasons. They are machine codes a client may translate.
const (
	ReasonNoDecisions         = "no_decisions_in_window"
	ReasonStreamMissing       = "event_stream_missing"
	ReasonStreamPartial       = "event_stream_partial"
	ReasonSensorUnavailable   = "sensor_unavailable"
	ReasonSensorNone          = "sensor_saw_nothing"
	ReasonUnattributedEvents  = "unattributed_events_present"
	ReasonBlindSpots          = "known_blind_spots_active"
	ReasonBelowObserved       = "effects_not_observed"
	ReasonProducerFailures    = "monitor_failures"
	ReasonUntrustedMonitor    = "untrusted_monitor"
	ReasonAuditMode           = "audit_mode_only"
	ReasonCanariesNotReported = "canaries_not_reported"
	ReasonAgainstCharter      = "against_charter"
	ReasonHeldOrFailClosed    = "held_or_fail_closed"
	ReasonRiskyUnreviewed     = "risky_unreviewed"
	ReasonVisibilityNotFull   = "visibility_not_warranted"
	ReasonIncomplete          = "window_not_fully_read"
)

// ComputePosture derives the namespace's posture from the decisions observed
// in the window. The rules are deterministic and only ever lower a state:
//
//   - Visibility is full only when every action's stream was complete,
//     a sensor attributed its effects, nothing was left unattributed, no
//     blind spot was declared and every monitor answered.
//   - Control is enforced only when every decision was made in gate mode.
//   - Confidence needs attention when any action went against the charter,
//     was held or failed closed and nobody has labelled it a false alarm.
//     Otherwise it is blind when visibility is, and degraded while control
//     is not enforced, visibility is not warranted, a risky action is
//     unreviewed or no canary evidence exists. Without canaries Phase 0
//     never reports warranted confidence.
//
//nolint:gocyclo // The rules read best as one pass over the decisions.
func ComputePosture(namespace, window string, now time.Time, views []oversightv1.DecisionView, incomplete bool) oversightv1.PostureResponse {
	basis := oversightv1.PostureBasis{Dispositions: map[string]int{}, Signals: map[string]int{}}
	basis.Coverage.BlindSpots = map[string]int{}
	tasks, agents := map[string]bool{}, map[string]bool{}
	charters, concerns := map[string]bool{}, map[string]bool{}
	producers := map[string]*oversightv1.PostureProducer{}
	visibility := map[string]bool{}
	attention := map[string]bool{}
	allGate := len(views) > 0
	riskyUnreviewed := 0
	evidenceRank := map[oversightv1.EvidenceLevel]int{
		oversightv1.EvidenceLevelIssued: 1, oversightv1.EvidenceLevelApplied: 2,
		oversightv1.EvidenceLevelObserved: 3, oversightv1.EvidenceLevelAttested: 4,
	}
	for i := range views {
		v := &views[i]
		basis.Actions++
		tasks[v.TaskName] = true
		if v.AgentName != "" {
			agents[v.AgentName] = true
		}
		if v.CharterRevision != "" {
			charters[v.CharterRevision] = true
		}
		if v.ConcernsRevision != "" {
			concerns[v.ConcernsRevision] = true
		}
		basis.Dispositions[string(v.Disposition)]++
		basis.Signals[string(v.Signal)]++
		if v.WouldHavePaused {
			basis.WouldHavePaused++
		}
		basis.CharterFindings += len(v.CharterFindings)
		if basis.LastObservedAt == nil || v.ObservedAt.After(*basis.LastObservedAt) {
			observed := v.ObservedAt
			basis.LastObservedAt = &observed
		}
		if v.Mode != oversightv1.ModeGate {
			allGate = false
		}
		label := oversightv1.FeedbackLabel("")
		if v.Feedback != nil {
			label = v.Feedback.Label
			switch label {
			case oversightv1.FeedbackFalseAlarm:
				basis.FalseAlarms++
			case oversightv1.FeedbackRealProblem:
				basis.RealProblems++
			}
		}
		if v.Signal != oversightv1.SignalConsistent && v.Feedback == nil {
			basis.Unreviewed++
		}
		if label != oversightv1.FeedbackFalseAlarm {
			switch {
			case len(v.CharterFindings) > 0:
				attention[ReasonAgainstCharter] = true
			case v.Signal == oversightv1.SignalAgainstCharter, v.Disposition == oversightv1.DispositionFailClosed:
				attention[ReasonHeldOrFailClosed] = true
			case v.Signal == oversightv1.SignalMonitorUnavailable && v.Feedback == nil:
				riskyUnreviewed++
			case v.Signal == oversightv1.SignalRisky && v.Feedback == nil:
				riskyUnreviewed++
			}
		}

		c := &v.Coverage
		switch c.StreamState {
		case oversightv1.StreamStateOK:
			basis.Coverage.StreamOK++
		case oversightv1.StreamStateMissing:
			visibility[ReasonStreamMissing] = true
		default:
			visibility[ReasonStreamPartial] = true
		}
		switch c.SensorState {
		case oversightv1.SensorStateEvents:
			basis.Coverage.SensorAttributed++
		case oversightv1.SensorStateUnavailable:
			visibility[ReasonSensorUnavailable] = true
		case oversightv1.SensorStateNone:
			visibility[ReasonSensorNone] = true
		default:
			visibility[ReasonUnattributedEvents] = true
		}
		basis.Coverage.UnattributedEvents += c.UnattributedEvents
		for _, n := range v.Action.Effects.Counts {
			basis.Coverage.ObservedEvents += max(n, 0)
		}
		if c.UnattributedEvents > 0 {
			visibility[ReasonUnattributedEvents] = true
		}
		for _, spot := range c.BlindSpots {
			basis.Coverage.BlindSpots[spot]++
			visibility[ReasonBlindSpots] = true
		}
		if evidenceRank[v.EvidenceLevel] > evidenceRank[basis.Coverage.MaxEvidenceLevel] {
			basis.Coverage.MaxEvidenceLevel = v.EvidenceLevel
		}
		if evidenceRank[v.EvidenceLevel] < evidenceRank[oversightv1.EvidenceLevelObserved] {
			visibility[ReasonBelowObserved] = true
		}
		record := func(role, ref, model, trust, status string) {
			key := role + "\x00" + ref + "\x00" + model
			p, ok := producers[key]
			if !ok {
				p = &oversightv1.PostureProducer{Role: role, ProviderRef: ref, ModelID: model, TrustState: trust}
				producers[key] = p
			}
			p.Evaluations++
			if status != oversightv1.EvaluationStatusOK {
				p.Failures++
				visibility[ReasonProducerFailures] = true
			}
			if trust == oversightv1.TrustStateUntrustedMonitor {
				visibility[ReasonUntrustedMonitor] = true
			}
		}
		for _, s := range v.Scorers {
			record("scorer", s.ProviderRef, s.ModelID, s.TrustState, s.Status)
		}
		for _, a := range v.Advisors {
			record("advisor", a.ProviderRef, a.ModelID, a.TrustState, a.Status)
		}
	}
	basis.Tasks, basis.Agents = len(tasks), len(agents)
	if len(basis.Coverage.BlindSpots) == 0 {
		basis.Coverage.BlindSpots = nil
	}

	posture := oversightv1.PostureResponse{
		Namespace: namespace, Window: window, GeneratedAt: now.UTC(), Basis: basis, Incomplete: incomplete,
		CharterRevisions: sortedSet(charters), ConcernsRevision: sortedSet(concerns),
	}
	if len(views) > 0 {
		posture.Mode = oversightv1.ModeAudit
		if allGate {
			posture.Mode = oversightv1.ModeGate
		}
	}
	for _, p := range producers {
		posture.Producers = append(posture.Producers, *p)
	}
	slices.SortFunc(posture.Producers, func(a, b oversightv1.PostureProducer) int {
		return strings.Compare(a.Role+a.ProviderRef+a.ModelID, b.Role+b.ProviderRef+b.ModelID)
	})

	// Visibility.
	switch {
	case len(views) == 0:
		posture.Visibility = state(oversightv1.VisibilityBlind, "No decisions were recorded in this window, so nothing was seen.", ReasonNoDecisions)
	case visibility[ReasonStreamMissing] || visibility[ReasonSensorUnavailable]:
		posture.Visibility = state(oversightv1.VisibilityBlind, "The monitor could not see some actions at all.", sortedSet(visibility)...)
	case len(visibility) > 0 || incomplete:
		reasons := sortedSet(visibility)
		if incomplete {
			reasons = append(reasons, ReasonIncomplete)
		}
		posture.Visibility = state(oversightv1.VisibilityPartial, visibilitySummary(basis), reasons...)
	default:
		posture.Visibility = state(oversightv1.VisibilityFull,
			fmt.Sprintf("All %d actions were seen and attributed.", basis.Actions))
	}

	// Control.
	if allGate {
		posture.Control = state(oversightv1.ControlEnforced, "Decisions can hold actions.")
	} else {
		posture.Control = state(oversightv1.ControlNotEnforced,
			"Audit mode: findings are recorded, but no action is held.", ReasonAuditMode)
	}

	// Confidence.
	var reasons []string
	switch {
	case len(attention) > 0:
		reasons = sortedSet(attention)
		posture.Confidence = state(oversightv1.ConfidenceAttentionRequired, attentionSummary(basis), reasons...)
	case posture.Visibility.State == oversightv1.VisibilityBlind:
		posture.Confidence = state(oversightv1.ConfidenceBlind, posture.Visibility.Summary, posture.Visibility.Reasons...)
	default:
		if !allGate {
			reasons = append(reasons, ReasonAuditMode)
		}
		if posture.Visibility.State != oversightv1.VisibilityFull {
			reasons = append(reasons, ReasonVisibilityNotFull)
		}
		if riskyUnreviewed > 0 {
			reasons = append(reasons, ReasonRiskyUnreviewed)
		}
		// No canary or random-audit records exist yet, so no measured miss
		// rate backs the monitors: confidence cannot be warranted.
		reasons = append(reasons, ReasonCanariesNotReported)
		summary := fmt.Sprintf("Nothing went against the charter in %d actions", basis.Actions)
		if riskyUnreviewed > 0 {
			summary += fmt.Sprintf("; %d risky actions await review", riskyUnreviewed)
		}
		posture.Confidence = state(oversightv1.ConfidenceDegraded, summary+".", reasons...)
	}
	return posture
}

func state(name, summary string, reasons ...string) oversightv1.PostureState {
	return oversightv1.PostureState{State: name, Summary: summary, Reasons: reasons}
}

func visibilitySummary(basis oversightv1.PostureBasis) string {
	parts := []string{fmt.Sprintf("%d of %d actions attributed", basis.Coverage.SensorAttributed, basis.Actions)}
	if basis.Coverage.UnattributedEvents > 0 {
		parts = append(parts, fmt.Sprintf("%d unattributed events", basis.Coverage.UnattributedEvents))
	}
	if n := len(basis.Coverage.BlindSpots); n > 0 {
		parts = append(parts, fmt.Sprintf("%d known blind spots", n))
	}
	return strings.Join(parts, "; ") + "."
}

func attentionSummary(basis oversightv1.PostureBasis) string {
	var parts []string
	if basis.CharterFindings > 0 {
		parts = append(parts, fmt.Sprintf("%d charter findings", basis.CharterFindings))
	}
	if n := basis.Signals[string(oversightv1.SignalAgainstCharter)]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d actions against the charter or held", n))
	}
	if basis.WouldHavePaused > 0 {
		parts = append(parts, fmt.Sprintf("%d would have paused", basis.WouldHavePaused))
	}
	return strings.Join(parts, "; ") + "."
}

func sortedSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	slices.Sort(out)
	return out
}
