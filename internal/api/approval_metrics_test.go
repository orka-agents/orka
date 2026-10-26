package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
	"github.com/orka-agents/orka/workers/common"
)

// Gather the registered collector, including its actual label names. These tests
// are serial because the production collector is process-global.
func approvalDecisionSamples(t *testing.T) map[string]*dto.Histogram {
	t.Helper()
	families, err := ctrlmetrics.Registry.Gather()
	require.NoError(t, err)
	samples := map[string]*dto.Histogram{}
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
			require.Contains(t, []string{"approved", "denied", "cancelled", "expired"}, labels["outcome"])
			require.Contains(t, []string{"acp", "worker"}, labels["path"])
			samples[labels["outcome"]+"/"+labels["path"]] = metric.Histogram
		}
	}
	return samples
}

type approvalMetricTransport func(*http.Request) (*http.Response, error)

func (f approvalMetricTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func requestWorkerMetricApproval(t *testing.T, f *v2ApprovalAPIFixture) approvals.Approval {
	t.Helper()
	// Exercise request_approval, the worker HTTP recorder, authenticated worker
	// ownership checks and real SQLite ingestion. Only the small emitter adapter
	// and HTTP transport are fixtures; Kubernetes is the existing fake client.
	app := setupOwnedInternalExecutionEventApp(t, f.store, f.task.Name, "worker-pod", "worker-pod-uid")
	tokenPath := filepath.Join(t.TempDir(), "worker-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("synthetic-worker-token"), 0600))
	recorder := common.NewHTTPEventRecorder(common.HTTPEventRecorderConfig{
		ControllerURL: "http://controller.example", Namespace: f.task.Namespace, TaskName: f.task.Name,
		BearerPath: tokenPath,
		Client: &http.Client{Transport: approvalMetricTransport(func(req *http.Request) (*http.Response, error) {
			return app.Test(req)
		})},
	})
	ctx := tools.WithToolContext(t.Context(), &tools.ToolContext{
		Namespace: f.task.Namespace, TaskID: f.task.Name, TaskUID: string(f.task.UID),
		ApprovalEmitter: func(ctx context.Context, target approvals.ApprovalTarget) error {
			content, err := json.Marshal(target)
			if err != nil {
				return err
			}
			return common.RecordEventStrict(ctx, recorder, events.ExecutionEventTypeApprovalRequested,
				common.WithEventToolCallID(target.ApprovalID), common.WithEventContent(json.RawMessage(content)))
		},
	})
	_, err := tools.NewRequestApprovalTool().Execute(ctx, json.RawMessage(`{"action":"Create fixture order","targetTool":"fixture_order","targetArguments":{"order":"synthetic"}}`))
	require.NoError(t, err)
	pending := f.pending(t)
	require.Nil(t, pending.Binding)
	require.False(t, pending.CreatedAt.IsZero())
	return pending
}

func TestApprovalDecisionMetricsHumanPaths(t *testing.T) {
	for _, path := range []string{"acp", "worker"} {
		for _, decision := range []string{"approve", "decline"} {
			t.Run(path+"/"+decision, func(t *testing.T) {
				metrics.ApprovalDecisionDuration.Reset()
				t.Cleanup(metrics.ApprovalDecisionDuration.Reset)
				f := newV2ApprovalAPIFixture(t)
				var pending approvals.Approval
				var call v2ApprovalAPIBrokerCall
				if path == "acp" {
					call = f.start(t)
					pending = f.pending(t)
				} else {
					pending = requestWorkerMetricApproval(t, f)
				}
				require.Empty(t, approvalDecisionSamples(t), "requests and polling must not count")
				response := f.decide("reviewer-a", pending.ID, decision)
				require.NoError(t, response.err)
				require.Equal(t, http.StatusOK, response.status, "%s", response.body)
				if path == "acp" {
					call.result(t)
					f.start(t).result(t) // receipt replay must not count again
				}
				response = f.decide("reviewer-b", pending.ID, decision)
				require.NoError(t, response.err)
				require.Equal(t, http.StatusOK, response.status)
				other := "approve"
				outcome := "denied"
				if decision == "approve" {
					other, outcome = "decline", "approved"
				}
				response = f.decide("reviewer-b", pending.ID, other)
				require.NoError(t, response.err)
				require.Equal(t, http.StatusConflict, response.status)
				decided := f.list(t)[0]
				require.NotNil(t, decided.DecisionTime)
				samples := approvalDecisionSamples(t)
				require.Len(t, samples, 1)
				histogram := samples[outcome+"/"+path]
				require.NotNil(t, histogram)
				require.EqualValues(t, 1, histogram.GetSampleCount())
				require.InDelta(t, decided.DecisionTime.Sub(pending.CreatedAt).Seconds(), histogram.GetSampleSum(), 1e-9)
			})
		}
	}
}

type approvalMetricFailingStore struct{ store.ExecutionEventStore }

func (s approvalMetricFailingStore) AppendExecutionEvent(context.Context, *store.ExecutionEvent) (*store.ExecutionEvent, error) {
	return nil, errors.New("synthetic decision append failure")
}

type approvalMetricFailingNudge struct{ client.Client }

func (c approvalMetricFailingNudge) Patch(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
	return errors.New("synthetic task nudge failure")
}

func TestApprovalDecisionMetricsFailedAndRejectedDecisions(t *testing.T) {
	metrics.ApprovalDecisionDuration.Reset()
	t.Cleanup(metrics.ApprovalDecisionDuration.Reset)
	f := newV2ApprovalAPIFixture(t)
	pending := requestWorkerMetricApproval(t, f)
	for _, test := range []struct {
		role, id, decision string
		status             int
	}{
		{"outsider", pending.ID, "approve", http.StatusForbidden},
		{"reviewer-a", "missing-approval", "approve", http.StatusNotFound},
		{"reviewer-a", pending.ID, "invalid", http.StatusBadRequest},
	} {
		response := f.decide(test.role, test.id, test.decision)
		require.NoError(t, response.err)
		require.Equal(t, test.status, response.status)
	}
	f.server.handlers.executionEventStore = approvalMetricFailingStore{f.store}
	response := f.decide("reviewer-a", pending.ID, "approve")
	require.NoError(t, response.err)
	require.Equal(t, http.StatusInternalServerError, response.status)
	require.Empty(t, approvalDecisionSamples(t))
	f.server.handlers.executionEventStore = f.store
	response = f.decide("reviewer-a", pending.ID, "approve")
	require.NoError(t, response.err)
	require.Equal(t, http.StatusOK, response.status)
	require.EqualValues(t, 1, approvalDecisionSamples(t)["approved/worker"].GetSampleCount())
}

func TestApprovalDecisionMetricsSurviveTaskNudgeFailure(t *testing.T) {
	metrics.ApprovalDecisionDuration.Reset()
	t.Cleanup(metrics.ApprovalDecisionDuration.Reset)
	f := newV2ApprovalAPIFixture(t)
	pending := requestWorkerMetricApproval(t, f)
	original := f.server.handlers.client
	f.server.handlers.client = approvalMetricFailingNudge{original}
	response := f.decide("reviewer-a", pending.ID, "approve")
	require.NoError(t, response.err)
	require.Equal(t, http.StatusInternalServerError, response.status)
	require.EqualValues(t, 1, approvalDecisionSamples(t)["approved/worker"].GetSampleCount(), "the decision committed before the task nudge failed")
	f.server.handlers.client = original
	response = f.decide("reviewer-a", pending.ID, "approve")
	require.NoError(t, response.err)
	require.Equal(t, http.StatusOK, response.status)
	require.EqualValues(t, 1, approvalDecisionSamples(t)["approved/worker"].GetSampleCount(), "retrying the nudge must not recount the decision")
}
