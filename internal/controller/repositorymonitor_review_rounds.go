package controller

import (
	"context"
	"fmt"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/store"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	repositoryMonitorReviewEscalatedEvent   = "review_verdict_escalated"
	repositoryMonitorReviewFirstPassedEvent = "review_first_passed"
	repositoryMonitorReviewMaxRoundsReason  = "max_rounds"
	repositoryMonitorFindingOutcomeReported = "reported"
	repositoryMonitorFindingOutcomeFixed    = "fixed"
)

type repositoryMonitorReviewRounds struct {
	SincePass     int
	PassedEarlier bool
}

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
		for i := range records {
			verdict := strings.TrimSpace(records[i].Verdict)
			if !repositoryMonitorReviewVerdictMarksHeadFresh(verdict) {
				continue
			}
			head := strings.TrimSpace(records[i].HeadSHA)
			_, superseded := heads[head]
			if verdict == repositoryMonitorReviewVerdictPassed && !superseded {
				unfinished, err := r.repositoryMonitorExhaustivePassPending(ctx, monitor, &records[i])
				if err != nil {
					return repositoryMonitorReviewRounds{}, err
				}
				if !unfinished {
					return repositoryMonitorReviewRounds{SincePass: len(heads), PassedEarlier: true}, nil
				}
			}
			heads[head] = struct{}{}
		}
		if next == "" {
			return repositoryMonitorReviewRounds{SincePass: len(heads)}, nil
		}
		cursor = next
	}
}

func repositoryMonitorMaxRoundsReached(spec corev1alpha1.RepositoryMonitorSpec, rounds repositoryMonitorReviewRounds) bool {
	limit := spec.Review.MaxRounds
	return limit != nil && *limit > 0 && rounds.SincePass >= int(*limit)
}

func repositoryMonitorMaxRoundsNote(spec corev1alpha1.RepositoryMonitorSpec, rounds repositoryMonitorReviewRounds) string {
	return fmt.Sprintf("Verdict raised from needs_changes to needs_human: %d heads of this pull request in a row were reviewed without passing, reaching spec.review.maxRounds (%d). Before fixing more findings, consider whether the approach should change.", rounds.SincePass, *spec.Review.MaxRounds)
}

func recordRepositoryMonitorReviewMetrics(plan repositoryMonitorReviewPlan, ledger []repositoryMonitorReviewFinding, record *store.ReviewRecord) {
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
}

func (r *RepositoryMonitorReconciler) recordRepositoryMonitorRoundsToPassBestEffort(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, item *store.MonitorItem, record *store.ReviewRecord, rounds repositoryMonitorReviewRounds) {
	if record.Verdict != repositoryMonitorReviewVerdictPassed || rounds.SincePass <= 0 || rounds.PassedEarlier {
		return
	}
	logger := log.FromContext(ctx).WithName("repositorymonitor")
	pending, err := r.repositoryMonitorExhaustivePassPending(ctx, monitor, record)
	if err != nil || pending {
		if err != nil {
			logger.Error(err, "skipping review rounds-to-pass metric", "pullRequest", item.Number)
		}
		return
	}
	events, _, err := r.Store.ListMonitorEvents(ctx, store.MonitorEventFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, ItemKind: repositoryMonitorPullRequestKind, ItemNumber: item.Number, EventType: repositoryMonitorReviewFirstPassedEvent, Limit: 1})
	if err != nil {
		logger.Error(err, "skipping review rounds-to-pass metric", "pullRequest", item.Number)
		return
	}
	if len(events) > 0 {
		return
	}
	if err := r.createMonitorEvent(ctx, monitor, "", repositoryMonitorPullRequestKind, item.Number, record.HeadSHA, repositoryMonitorReviewFirstPassedEvent, fmt.Sprintf("Pull request #%d first passed review after %d reviewed heads", item.Number, rounds.SincePass), map[string]any{
		eventReviewIDField: record.ID,
		"rounds":           rounds.SincePass,
	}); err != nil {
		logger.Error(err, "skipping review rounds-to-pass metric", "pullRequest", item.Number)
		return
	}
	metrics.RecordRepositoryMonitorReviewRoundsToPass(rounds.SincePass)
}

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

func repositoryMonitorFindingMetricPriority(priority string) string {
	if _, ok := repositoryMonitorPriorityRank(priority); ok {
		return strings.ToUpper(strings.TrimSpace(priority))
	}
	return ""
}
