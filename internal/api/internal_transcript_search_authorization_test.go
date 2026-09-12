/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

func TestSearchTranscriptRespectsHistoryBounds(t *testing.T) {
	for _, test := range []struct {
		name                string
		referenceTask       string
		provenanceProtected bool
		maxMessages         int32
		throughMessageID    string
		wantMessageIDs      []string
	}{
		{name: "caller message limit without provenance", referenceTask: "my-task", maxMessages: 1, wantMessageIDs: []string{"future"}},
		{name: "caller cutoff without provenance", referenceTask: "my-task", throughMessageID: "cutoff", wantMessageIDs: []string{"old", "cutoff"}},
		{name: "caller message limit with provenance", referenceTask: "my-task", provenanceProtected: true, maxMessages: 1, wantMessageIDs: []string{"future"}},
		{name: "caller cutoff with provenance", referenceTask: "my-task", provenanceProtected: true, throughMessageID: "cutoff", wantMessageIDs: []string{"old", "cutoff"}},
		{name: "related message limit", referenceTask: "prior-task", provenanceProtected: true, maxMessages: 1, wantMessageIDs: []string{"future"}},
		{name: "related cutoff", referenceTask: "prior-task", provenanceProtected: true, throughMessageID: "cutoff", wantMessageIDs: []string{"old", "cutoff"}},
		{name: "combined bounds", referenceTask: "my-task", provenanceProtected: true, maxMessages: 1, throughMessageID: "cutoff", wantMessageIDs: []string{"cutoff"}},
		{name: "missing cutoff", referenceTask: "my-task", provenanceProtected: true, throughMessageID: "missing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, app, ss := setupTestInternalHandlers()
			h.taskProvenanceProtected = test.provenanceProtected
			task := &corev1alpha1.Task{}
			require.NoError(t, h.k8sClient.Get(t.Context(), client.ObjectKey{Namespace: "default", Name: test.referenceTask}, task))
			task.Spec.SessionRef.MaxMessages = test.maxMessages
			task.Spec.SessionRef.ThroughMessageID = test.throughMessageID
			require.NoError(t, h.k8sClient.Update(t.Context(), task))
			boundedSession := task.Spec.SessionRef.Name

			// Another tree member's unbounded reference must not widen the
			// restricted session, regardless of the Task list order.
			for _, name := range []string{"a-unbounded", "z-unbounded"} {
				duplicate := internalCallerAuthTaskObject(name, name+"-uid", "", "coordinator", boundedSession)
				require.NoError(t, h.k8sClient.Create(t.Context(), duplicate))
			}
			for _, name := range []string{boundedSession, "current"} {
				require.NoError(t, ss.CreateSession(t.Context(), &store.SessionRecord{
					Namespace: "default", Name: name, SessionType: "task",
				}))
				require.NoError(t, ss.AppendMessages(t.Context(), "default", name, []store.SessionMessage{
					{ID: "old", Order: 1, Role: "assistant", Content: "needle in old history"},
					{ID: "cutoff", Order: 2, Role: "user", Content: "needle at cutoff"},
					{ID: "future", Order: 3, Role: "assistant", Content: "needle after cutoff"},
				}))
			}
			app.Get("/internal/v1/sessions/:namespace/search", h.SearchTranscript)

			for _, sessionName := range []string{boundedSession, ""} {
				t.Run("session="+sessionName, func(t *testing.T) {
					req := httptest.NewRequest(http.MethodGet, "/internal/v1/sessions/default/search?query=needle&sessionName="+sessionName, nil)
					resp, err := app.Test(req)
					require.NoError(t, err)
					defer resp.Body.Close() //nolint:errcheck
					require.Equal(t, http.StatusOK, resp.StatusCode)
					var results []store.TranscriptSearchResult
					require.NoError(t, json.NewDecoder(resp.Body).Decode(&results))
					got := make([]string, 0, len(results))
					want := make([]string, 0, len(test.wantMessageIDs)+3)
					for _, result := range results {
						got = append(got, result.SessionName+"/"+result.StableMessageID)
					}
					for _, id := range test.wantMessageIDs {
						want = append(want, boundedSession+"/"+id)
					}
					if sessionName == "" && test.provenanceProtected {
						want = append(want, "current/old", "current/cutoff", "current/future")
					}
					require.ElementsMatch(t, want, got)
				})
			}
		})
	}
}
