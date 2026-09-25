/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/orka-agents/orka/internal/store"
)

func TestResponsesRecordsUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, coordinator := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/coordinator=%t", stream, coordinator), func(t *testing.T) {
				backend := newInternalExecutionEventStore(t)
				reader := testInternalExecutionEventClient(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: defaultNamespace, UID: "original-namespace"}})
				var calls atomic.Int32
				handler, app := setupResponsesHTTP(t, func(w http.ResponseWriter, r *http.Request) {
					if calls.Add(1) == 1 {
						// Namespace ownership must be captured before even the API-mode probe.
						require.NoError(t, reader.Delete(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: defaultNamespace}}))
						require.NoError(t, reader.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: defaultNamespace, UID: "replacement-namespace"}}))
					}
					var request struct {
						Stream bool `json:"stream"`
					}
					require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
					const text = "Done. <!-- GOAL_STATE:SATISFIED -->"
					if request.Stream {
						upstreamSSE(w, []map[string]any{{"type": "response.completed", "response": responsesFixtureResponse(text)}})
					} else {
						upstreamResponse(w, text)
					}
				}, false)
				handler.resultStore = backend
				handler.apiReader = reader
				// A stale informer identity must not override the uncached reader.
				require.NoError(t, handler.client.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: defaultNamespace, UID: "stale-namespace"}}))
				body := fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"hello"}`, stream)
				status, response := requestResponses(t, app, body, !coordinator)
				require.Equal(t, http.StatusOK, status, string(response))
				if stream {
					events := parseResponsesSSE(t, response)
					require.Equal(t, "response.completed", events[len(events)-1]["type"])
				}

				data, err := backend.LoadUsage(t.Context(), store.UsageFilter{Namespaces: []string{defaultNamespace}})
				require.NoError(t, err)
				require.NotEmpty(t, data.Observations, "Responses must persist usage independently of tracing")
				started, completed := map[string]bool{}, map[string]bool{}
				for _, observation := range data.Observations {
					require.Equal(t, defaultNamespace, observation.Namespace)
					require.Equal(t, "original-namespace", observation.NamespaceUID)
					require.Equal(t, store.UsageScopeCall, observation.Scope)
					require.Equal(t, store.UsageSourceProvider, observation.Source)
					require.Empty(t, observation.TaskUID)
					require.Empty(t, observation.SessionName)
					if observation.Status == store.UsageStatusStarted {
						started[observation.CounterID] = true
					}
					if observation.Complete {
						completed[observation.CounterID] = true
						require.Equal(t, store.UsageStatusCompleted, observation.Status)
						require.NotNil(t, observation.InputTokens)
						require.NotNil(t, observation.OutputTokens)
						require.EqualValues(t, 7, *observation.InputTokens)
						require.EqualValues(t, 3, *observation.OutputTokens)
					}
				}
				require.Len(t, started, int(calls.Load()), "each billed HTTP call, including probes, needs its own counter")
				require.Equal(t, started, completed)
				current, err := backend.LoadUsage(t.Context(), store.UsageFilter{Namespaces: []string{defaultNamespace}, NamespaceUIDs: map[string]string{defaultNamespace: "replacement-namespace"}})
				require.NoError(t, err)
				require.Empty(t, current.Observations)
			})
		}
	}
}

func TestResponsesUsageIdentityFailurePreventsProviderCall(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			var calls atomic.Int32
			handler, app := setupResponsesHTTP(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				upstreamResponse(w, "must not be called")
			}, false)
			handler.resultStore = newInternalExecutionEventStore(t)
			handler.apiReader = testInternalExecutionEventClient(t)
			// The cached Namespace still exists, but the authoritative lookup fails.
			require.NoError(t, handler.client.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: defaultNamespace, UID: "stale-namespace"}}))
			body := fmt.Sprintf(`{"model":"fixture/test-model","store":false,"stream":%t,"input":"hello"}`, stream)
			status, response := requestResponses(t, app, body, true)
			require.Zero(t, calls.Load(), "failed ownership lookup must prevent billable provider calls")
			if stream {
				require.Equal(t, http.StatusOK, status)
				events := parseResponsesSSE(t, response)
				require.Equal(t, "response.failed", events[len(events)-1]["type"])
			} else {
				require.Equal(t, http.StatusBadGateway, status, string(response))
			}
		})
	}
}
