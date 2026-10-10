package controller

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

func TestRepositoryMonitorControlCommandPublishesReadinessBeforeCompletion(t *testing.T) {
	for _, tc := range []struct {
		name         string
		intent       string
		staleCommand bool
		observedHead string
		failStatus   bool
		rateLimited  bool
		wantState    string
	}{
		{name: "stop", intent: repositoryMonitorCommandIntentStop, wantState: repositoryMonitorStatusFailure},
		{name: "stale stop", intent: repositoryMonitorCommandIntentStop, staleCommand: true, wantState: repositoryMonitorStatusFailure},
		{name: "stop follows current head", intent: repositoryMonitorCommandIntentStop, observedHead: "sha2", wantState: repositoryMonitorStatusFailure},
		{name: "resume reevaluates", intent: repositoryMonitorCommandIntentResume, wantState: repositoryMonitorStatusPending},
		{name: "suspended stop status retry", intent: repositoryMonitorCommandIntentStop, failStatus: true, wantState: repositoryMonitorStatusFailure},
		{name: "suspended resume status retry", intent: repositoryMonitorCommandIntentResume, failStatus: true, wantState: repositoryMonitorStatusPending},
		{name: "suspended stop rate limit retry", intent: repositoryMonitorCommandIntentStop, failStatus: true, rateLimited: true, wantState: repositoryMonitorStatusFailure},
		{name: "suspended resume rate limit retry", intent: repositoryMonitorCommandIntentResume, failStatus: true, rateLimited: true, wantState: repositoryMonitorStatusPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRepositoryMonitorCommandReadinessFixture(t)
			f.command.Intent = tc.intent
			if tc.staleCommand {
				f.command.HeadSHA = "old-sha"
			}
			require.NoError(t, f.reconciler.Store.UpdateCommandEvent(t.Context(), f.command))
			item, err := f.reconciler.Store.GetMonitorItem(t.Context(), f.monitor.Namespace, f.monitor.Name, repositoryMonitorPullRequestKind, "1")
			require.NoError(t, err)
			if tc.intent == repositoryMonitorCommandIntentResume {
				item.SkipReason = repositoryMonitorIssueSkipStoppedByCommand
				item.RepairState = repositoryMonitorRepairPhaseFailed
				require.NoError(t, f.reconciler.Store.UpsertMonitorItem(t.Context(), item))
			}
			// Represent the published status from a previous workflow poll.
			pr := repositoryMonitorPullRequest{Number: 1, HeadSHA: item.HeadSHA, State: repositoryMonitorItemStateOpen, BaseBranch: repositoryMonitorTestDefaultBranch}
			require.NoError(t, f.reconciler.reconcileRepositoryMonitorReadiness(t.Context(), f.monitor, &pr, item))
			before := len(f.statuses)
			beforeActionStatuses := len(f.actionStatuses)
			currentHead := item.HeadSHA
			if tc.observedHead != "" {
				currentHead = tc.observedHead
			}
			var postedHeads []string
			failStatus := tc.failStatus
			if tc.failStatus {
				monitor := f.monitor.DeepCopy()
				require.NoError(t, f.reconciler.Get(t.Context(), types.NamespacedName{Namespace: monitor.Namespace, Name: monitor.Name}, monitor))
				suspended := true
				monitor.Spec.Suspend = &suspended
				require.NoError(t, f.reconciler.Update(t.Context(), monitor))
				f.monitor = monitor
			}
			f.reconciler.HTTPClient = &http.Client{Transport: controlReadinessHTTPTransport(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodPost && strings.Contains(req.URL.Path, "/statuses/") {
					postedHeads = append(postedHeads, req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:])
					if failStatus {
						if tc.rateLimited {
							return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"message":"You have exceeded a secondary rate limit."}`))}, nil
						}
						return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("unavailable"))}, nil
					}
				}
				response, transportErr := http.DefaultTransport.RoundTrip(req)
				if transportErr != nil || tc.observedHead == "" || req.Method != http.MethodGet || !strings.HasSuffix(req.URL.Path, "/pulls/1") {
					return response, transportErr
				}
				var body map[string]any
				decodeErr := json.NewDecoder(response.Body).Decode(&body)
				_ = response.Body.Close()
				if decodeErr != nil {
					return nil, decodeErr
				}
				body["head"].(map[string]any)["sha"] = tc.observedHead
				data, marshalErr := json.Marshal(body)
				if marshalErr != nil {
					return nil, marshalErr
				}
				response.Body = io.NopCloser(strings.NewReader(string(data)))
				response.ContentLength = int64(len(data))
				return response, nil
			})}
			f.createRun(t, time.Now().Add(-time.Minute))
			var run *store.MonitorRun
			for range 3 {
				f.reconcile(t)
				run, err = f.reconciler.Store.GetMonitorRun(t.Context(), f.monitor.Namespace, f.runID)
				require.NoError(t, err)
				if run.Phase == repositoryMonitorRunPhaseSucceeded || run.Phase == repositoryMonitorRunPhaseFailed {
					break
				}
			}
			action, err := f.reconciler.Store.GetWorkAction(t.Context(), f.monitor.Namespace, store.RepositoryMonitorWorkActionID(f.command.ID, tc.intent))
			require.NoError(t, err)
			require.NotEmpty(t, postedHeads, "control transition must publish before completing its run")
			if tc.failStatus {
				require.Equal(t, repositoryMonitorRunPhaseFailed, run.Phase)
				require.Contains(t, []string{repositoryMonitorWorkActionStatusQueued, repositoryMonitorWorkActionStatusRunning}, action.Status)
				if tc.rateLimited {
					require.Contains(t, run.Error, "github_rate_limited")
				} else {
					require.Contains(t, run.Error, "503")
				}
				command, commandErr := f.reconciler.Store.GetCommandEvent(t.Context(), f.monitor.Namespace, f.command.ID)
				require.NoError(t, commandErr)
				require.Equal(t, repositoryMonitorCommandAccepted, command.Status)
				failStatus = false
				f.reconcile(t)
				run, err = f.reconciler.Store.GetMonitorRun(t.Context(), f.monitor.Namespace, f.runID)
				require.NoError(t, err)
				require.Equal(t, repositoryMonitorRunPhaseQueued, run.Phase, "transient status failure must requeue even while schedules are suspended")
				run.StartedAt = time.Now().Add(-time.Second)
				require.NoError(t, f.reconciler.Store.UpdateMonitorRun(t.Context(), run))
				f.reconcile(t)
				run, err = f.reconciler.Store.GetMonitorRun(t.Context(), f.monitor.Namespace, f.runID)
				require.NoError(t, err)
				action, err = f.reconciler.Store.GetWorkAction(t.Context(), f.monitor.Namespace, action.ID)
				require.NoError(t, err)
			}
			require.Equal(t, repositoryMonitorRunPhaseSucceeded, run.Phase)
			require.Equal(t, repositoryMonitorWorkActionStatusSucceeded, action.Status)
			require.Greater(t, len(f.statuses), before)
			require.Equal(t, tc.wantState, f.statuses[len(f.statuses)-1].State)
			require.Equal(t, currentHead, postedHeads[len(postedHeads)-1])
			for _, status := range f.actionStatuses[beforeActionStatuses:] {
				require.NotEqual(t, repositoryMonitorWorkActionStatusSucceeded, status, "readiness must precede successful control completion")
			}
		})
	}
}

type controlReadinessHTTPTransport func(*http.Request) (*http.Response, error)

func (f controlReadinessHTTPTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
