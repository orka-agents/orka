package controller

import (
	"errors"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/metrics"
)

func gatherMCPApprovalDecisionMetrics(t *testing.T) map[string]*dto.Histogram {
	t.Helper()
	families, err := ctrlmetrics.Registry.Gather()
	require.NoError(t, err)
	out := map[string]*dto.Histogram{}
	for _, family := range families {
		if family.GetName() != "orka_approval_decision_duration_seconds" {
			continue
		}
		for _, metric := range family.Metric {
			require.Len(t, metric.Label, 2)
			labels := map[string]string{}
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			require.Equal(t, "acp", labels["path"])
			require.Contains(t, []string{"approved", "denied", "cancelled", "expired"}, labels["outcome"])
			out[labels["outcome"]] = metric.Histogram
		}
	}
	return out
}

func requireMCPApprovalDecisionMetric(t *testing.T, f *mcpApprovalRecoveryFixture, outcome string) {
	t.Helper()
	approval, _ := f.approval(t)
	require.NotNil(t, approval.DecisionTime)
	samples := gatherMCPApprovalDecisionMetrics(t)
	require.Len(t, samples, 1)
	histogram := samples[outcome]
	require.NotNil(t, histogram)
	require.EqualValues(t, 1, histogram.GetSampleCount())
	require.InDelta(t, approval.DecisionTime.Sub(approval.CreatedAt).Seconds(), histogram.GetSampleSum(), 1e-9)
}

func TestMCPApprovalDecisionMetricsRevocation(t *testing.T) {
	for _, tc := range []struct {
		name, outcome string
		cause         error
	}{
		{"cancelled task", "cancelled", errACPMCPTaskCancelled},
		{"expired task", "expired", errACPMCPTaskExpired},
		{"revoked authority", "cancelled", errors.New("synthetic revoked policy")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metrics.ApprovalDecisionDuration.Reset()
			t.Cleanup(metrics.ApprovalDecisionDuration.Reset)
			f := newMCPApprovalRecoveryFixture(t)
			call, _ := seedMCPApprovalPending(t, f, true)
			credentials, err := f.broker.Credentials.ResolveACPMCPBrokerCredentials(f.ctx, f.request)
			require.NoError(t, err)
			_, _, err = f.broker.revokeApproval(f.ctx, call, credentials, tc.cause)
			require.NoError(t, err)
			requireMCPApprovalDecisionMetric(t, f, tc.outcome)
			// Repeated settlement, polling and execution projection are no-ops
			// for decision metrics, including after reconstructing the handler.
			restarted := *f.broker
			_, _, err = restarted.revokeApproval(f.ctx, call, credentials, tc.cause)
			require.NoError(t, err)
			_, err = restarted.loadApprovalDecision(f.ctx, call)
			require.NoError(t, err)
			requireMCPApprovalDecisionMetric(t, f, tc.outcome)
		})
	}
}

func TestMCPApprovalDecisionMetricsRevocationPreservesHumanDecision(t *testing.T) {
	metrics.ApprovalDecisionDuration.Reset()
	t.Cleanup(metrics.ApprovalDecisionDuration.Reset)
	f := newMCPApprovalRecoveryFixture(t)
	call, _ := seedMCPApprovalPending(t, f, true)
	require.NoError(t, f.broker.approvalDecision(f.ctx, call, events.ExecutionEventTypeApprovalApproved, "synthetic review"))
	credentials, err := f.broker.Credentials.ResolveACPMCPBrokerCredentials(f.ctx, f.request)
	require.NoError(t, err)
	_, _, err = f.broker.revokeApproval(f.ctx, call, credentials, errACPMCPTaskCancelled)
	require.NoError(t, err)
	requireMCPApprovalDecisionMetric(t, f, "approved")
}

func TestMCPApprovalDecisionMetricsRecoveryCommitsAtomically(t *testing.T) {
	for _, code := range []string{acpApprovalCodeDeclined, acpApprovalCodeExpired, acpApprovalCodeCancelled, acpApprovalCodeStale} {
		t.Run(code, func(t *testing.T) {
			metrics.ApprovalDecisionDuration.Reset()
			t.Cleanup(metrics.ApprovalDecisionDuration.Reset)
			f := newMCPApprovalRecoveryFixture(t)
			call, effect := seedMCPApprovalPending(t, f, true)
			expected, _ := f.approval(t)
			credentials, err := f.broker.Credentials.ResolveACPMCPBrokerCredentials(f.ctx, f.request)
			require.NoError(t, err)
			_, _, err = f.broker.blockApproval(f.ctx, call, credentials, code)
			require.NoError(t, err)
			require.Empty(t, gatherMCPApprovalDecisionMetrics(t), "an execution receipt alone is not a decision")
			outage := &approvalTerminalProjectionOutageStore{DeduplicatingExecutionEventStore: f.events, TaskDataTransactionStore: f.events}
			outage.unavailable.Store(true)
			f.dispatcher.EventStore = outage
			err = f.dispatcher.projectMCPApprovalExecution(f.ctx, f.task, expected, effect.Identity)
			require.Error(t, err)
			actual, _ := f.approval(t)
			require.Equal(t, approvals.StatusPending, actual.Status, "the decision/outcome transaction rolled back")
			require.Empty(t, gatherMCPApprovalDecisionMetrics(t), "rolled-back decisions must not count")
			outage.unavailable.Store(false)
			require.NoError(t, f.dispatcher.projectMCPApprovalExecution(f.ctx, f.task, expected, effect.Identity))
			outcome := "cancelled"
			switch code {
			case acpApprovalCodeDeclined:
				outcome = "denied"
			case acpApprovalCodeExpired:
				outcome = "expired"
			}
			requireMCPApprovalDecisionMetric(t, f, outcome)
			require.NoError(t, f.dispatcher.projectMCPApprovalExecution(f.ctx, f.task, expected, effect.Identity))
			requireMCPApprovalDecisionMetric(t, f, outcome)
		})
	}
}

func TestMCPApprovalDecisionMetricsWaitExpiry(t *testing.T) {
	metrics.ApprovalDecisionDuration.Reset()
	t.Cleanup(metrics.ApprovalDecisionDuration.Reset)
	f := newMCPApprovalFixture(t)
	f.broker.ApprovalWaitTimeout = 60 * time.Millisecond
	done := f.start(f.request)
	f.pending()
	result := awaitMCPApprovalResult(t, done)
	require.True(t, result.IsError)
	samples := gatherMCPApprovalDecisionMetrics(t)
	require.Len(t, samples, 1)
	require.EqualValues(t, 1, samples["expired"].GetSampleCount())
	listed, err := approvals.ListEvents(t.Context(), f.events, f.request.Namespace, "approval-task")
	require.NoError(t, err)
	approval := approvals.Derive(listed, time.Time{})[0]
	require.NotNil(t, approval.DecisionTime)
	require.InDelta(t, approval.DecisionTime.Sub(approval.CreatedAt).Seconds(), samples["expired"].GetSampleSum(), 1e-9)
}
