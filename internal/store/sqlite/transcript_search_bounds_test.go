package sqlite

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/store"
)

func TestSearchTranscriptHistoryBounds(t *testing.T) {
	s := setupTestStore(t)
	require.NoError(t, s.CreateSession(t.Context(), &store.SessionRecord{
		Namespace: "default", Name: "session", SessionType: "task",
	}))
	now := time.Now().UTC()
	// Insertion and timestamp order deliberately differ from logical order.
	require.NoError(t, s.AppendMessages(t.Context(), "default", "session", []store.SessionMessage{
		{ID: "five", Order: 5, Role: "user", Content: "later nonmatching message", Timestamp: now.Add(-5 * time.Minute)},
		{ID: "three", Order: 3, Role: "user", Content: "needle at cutoff", Timestamp: now.Add(-3 * time.Minute)},
		{ID: "one", Order: 1, Role: "assistant", Content: "needle in old history", Timestamp: now.Add(-time.Minute)},
		{ID: "four", Order: 4, Role: "assistant", Content: "needle after cutoff", Timestamp: now.Add(-4 * time.Minute)},
		{ID: "two", Order: 2, Role: "assistant", Content: "needle before cutoff", Timestamp: now.Add(-2 * time.Minute)},
	}))
	for _, decoy := range []struct{ namespace, session string }{{"other", "session"}, {"default", "other"}} {
		require.NoError(t, s.CreateSession(t.Context(), &store.SessionRecord{
			Namespace: decoy.namespace, Name: decoy.session, SessionType: "task",
		}))
		require.NoError(t, s.AppendMessages(t.Context(), decoy.namespace, decoy.session, []store.SessionMessage{
			{ID: "foreign-cutoff", Order: 100, Role: "user", Content: "needle in another session"},
		}))
	}

	for _, test := range []struct {
		name   string
		bounds []store.TranscriptSearchHistoryBound
		roles  []string
		limit  int
		want   []string
	}{
		{name: "latest logical messages", bounds: []store.TranscriptSearchHistoryBound{{MaxMessages: 2}}, want: []string{"four"}},
		{name: "cutoff", bounds: []store.TranscriptSearchHistoryBound{{ThroughMessageID: "three"}}, want: []string{"one", "two", "three"}},
		{name: "combined bounds", bounds: []store.TranscriptSearchHistoryBound{{MaxMessages: 2, ThroughMessageID: "three"}}, want: []string{"two", "three"}},
		{name: "bounds before query", bounds: []store.TranscriptSearchHistoryBound{{MaxMessages: 1}}},
		{name: "bounds before roles", bounds: []store.TranscriptSearchHistoryBound{{MaxMessages: 3}}, roles: []string{"assistant"}, want: []string{"four"}},
		{name: "bounds before result limit", bounds: []store.TranscriptSearchHistoryBound{{MaxMessages: 2, ThroughMessageID: "three"}}, limit: 1, want: []string{"two"}},
		{name: "missing cutoff", bounds: []store.TranscriptSearchHistoryBound{{ThroughMessageID: "missing"}}},
		{name: "cutoff in another namespace or session", bounds: []store.TranscriptSearchHistoryBound{{ThroughMessageID: "foreign-cutoff"}}},
		{
			name: "disjoint references",
			bounds: []store.TranscriptSearchHistoryBound{
				{MaxMessages: 1, ThroughMessageID: "three"}, {MaxMessages: 1, ThroughMessageID: "four"},
			},
		},
		{
			name: "overlapping references",
			bounds: []store.TranscriptSearchHistoryBound{
				{MaxMessages: 3, ThroughMessageID: "four"}, {MaxMessages: 4},
			},
			want: []string{"two", "three", "four"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			for i := range test.bounds {
				test.bounds[i].SessionName = "session"
			}
			results, err := s.SearchTranscript(t.Context(), store.TranscriptSearchFilter{
				Namespace: "default", SessionName: "session", Query: "needle",
				HistoryBounds: test.bounds, Roles: test.roles, Limit: test.limit,
			})
			require.NoError(t, err)
			got := make([]string, 0, len(results))
			for _, result := range results {
				got = append(got, result.StableMessageID)
			}
			require.ElementsMatch(t, test.want, got)
		})
	}
}
