package controller

import (
	"context"
	"fmt"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/store"
)

const (
	repositoryMonitorReviewEscalatedEvent   = "review_verdict_escalated"
	repositoryMonitorReviewMaxRoundsReason  = "max_rounds"
	repositoryMonitorFindingOutcomeReported = "reported"
	repositoryMonitorFindingOutcomeFixed    = "fixed"
)

// repositoryMonitorReviewRounds describes a pull request's reviews as of a
// new review of one head.
type repositoryMonitorReviewRounds struct {
	// SincePass counts the distinct heads reviewed since the last passed
	// review, the new review's head included.
	SincePass int
	// PassedBefore reports whether an earlier review of the pull request
	// passed.
	PassedBefore bool
}

// repositoryMonitorReviewRoundsFor counts the pull request's reviewed heads
// before a new review of headSHA is recorded. Only accepted reviews count;
// failed, stale, and skipped ones do not, and several reviews of one head
// count once. A passed review ends the count only while it is its head's
// latest review: one that a later review of the same head (an exhaustive
// pass or a review command) superseded did not leave the head passing.
func (r *RepositoryMonitorReconciler) repositoryMonitorReviewRoundsFor(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, number int64, headSHA string) (repositoryMonitorReviewRounds, error) {
	heads := map[string]struct{}{headSHA: {}}
	cursor := ""
	for {
		records, next, err := r.Store.ListReviewRecords(ctx, store.ReviewRecordFilter{
			Namespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: number, Limit: 50, Cursor: cursor,
		})
		if err != nil {
			return repositoryMonitorReviewRounds{}, err
		}
		// Records are newest first.
		for i := range records {
			verdict := strings.TrimSpace(records[i].Verdict)
			if !repositoryMonitorReviewVerdictMarksHeadFresh(verdict) {
				continue
			}
			head := strings.TrimSpace(records[i].HeadSHA)
			_, superseded := heads[head]
			if verdict == repositoryMonitorReviewVerdictPassed && !superseded {
				return repositoryMonitorReviewRounds{SincePass: len(heads), PassedBefore: true}, nil
			}
			heads[head] = struct{}{}
		}
		if next == "" {
			return repositoryMonitorReviewRounds{SincePass: len(heads)}, nil
		}
		cursor = next
	}
}

// repositoryMonitorMaxRoundsReached reports whether a needs_changes review
// with these rounds reaches spec.review.maxRounds.
func repositoryMonitorMaxRoundsReached(spec corev1alpha1.RepositoryMonitorSpec, rounds repositoryMonitorReviewRounds) bool {
	limit := spec.Review.MaxRounds
	return limit != nil && *limit > 0 && rounds.SincePass >= int(*limit)
}

// repositoryMonitorMaxRoundsNote explains a verdict raised by
// spec.review.maxRounds in the review summary.
func repositoryMonitorMaxRoundsNote(spec corev1alpha1.RepositoryMonitorSpec, rounds repositoryMonitorReviewRounds) string {
	return fmt.Sprintf("Verdict raised from needs_changes to needs_human: %d heads of this pull request in a row were reviewed without passing, reaching spec.review.maxRounds (%d). Before fixing more findings, consider whether the approach should change.", rounds.SincePass, *spec.Review.MaxRounds)
}

// recordRepositoryMonitorReviewMetrics records convergence metrics for a
// newly stored review record. Callers invoke it once, right after the record
// is created, so a retried ingestion does not count the review again. A
// passed exhaustive pass that another pass will extend is not yet a pass of
// the pull request, so it records no rounds to pass.
func recordRepositoryMonitorReviewMetrics(plan repositoryMonitorReviewPlan, ledger []repositoryMonitorReviewFinding, record *store.ReviewRecord, rounds repositoryMonitorReviewRounds, passPending bool) {
	metrics.RecordRepositoryMonitorReview(repositoryMonitorReviewMetricMode(plan.Mode), record.Verdict)
	openBefore := map[string]struct{}{}
	for _, finding := range plan.Ledger {
		if repositoryMonitorFindingOpen(finding) {
			openBefore[finding.ID] = struct{}{}
		}
	}
	type key struct{ priority, outcome string }
	counts := map[key]int{}
	for _, finding := range ledger {
		priority := repositoryMonitorFindingMetricPriority(finding.Priority)
		if finding.FirstReview == record.ID {
			counts[key{priority, repositoryMonitorFindingOutcomeReported}]++
			continue
		}
		if _, wasOpen := openBefore[finding.ID]; wasOpen && !repositoryMonitorFindingOpen(finding) {
			counts[key{priority, repositoryMonitorFindingOutcomeFixed}]++
		}
	}
	for k, count := range counts {
		metrics.RecordRepositoryMonitorReviewFindings(k.priority, k.outcome, count)
	}
	if record.Verdict == repositoryMonitorReviewVerdictPassed && !passPending && !rounds.PassedBefore && rounds.SincePass > 0 {
		metrics.RecordRepositoryMonitorReviewRoundsToPass(rounds.SincePass)
	}
}

// repositoryMonitorReviewMetricMode bounds a review mode, which a rejected
// review reads from Task metadata, to the review modes Orka creates so it is
// safe as a metric label.
func repositoryMonitorReviewMetricMode(mode string) string {
	switch mode = strings.TrimSpace(mode); mode {
	case repositoryMonitorReviewModeFull, repositoryMonitorReviewModeIncremental, repositoryMonitorReviewModeSameHead:
		return mode
	case "":
		return repositoryMonitorReviewModeFull
	default:
		return ""
	}
}

// repositoryMonitorFindingMetricPriority bounds a reviewer-supplied priority
// to the P0-P3 vocabulary so it is safe as a metric label.
func repositoryMonitorFindingMetricPriority(priority string) string {
	if _, ok := repositoryMonitorPriorityRank(priority); ok {
		return strings.ToUpper(strings.TrimSpace(priority))
	}
	return ""
}
