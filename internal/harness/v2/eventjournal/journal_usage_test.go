package eventjournal

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	executionevents "github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/usage"
)

func TestJournalContinuationRetainsProfileModelForUsage(t *testing.T) {
	for _, test := range []struct {
		name          string
		runtimeModel  string
		usage         harnessv2.UsageUpdate
		completeness  string
		expectedTotal int64
	}{
		{name: "reported counts", runtimeModel: "gpt-test", usage: harnessv2.UsageUpdate{
			InputTokens: 100, OutputTokens: 25, Reported: true, Complete: true,
		}, completeness: "complete", expectedTotal: 125},
		{name: "unavailable counts", runtimeModel: "gpt-test", completeness: "unavailable"},
		{name: "untrusted runtime model", runtimeModel: strings.Repeat("b", 14), usage: harnessv2.UsageUpdate{
			InputTokens: 100, OutputTokens: 25, Reported: true, Complete: true,
		}, completeness: "complete", expectedTotal: 125},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "usage.db")
			db, err := sqlite.NewDB(path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			eventStore := sqlite.NewStore(db, "usage")
			mapCtx := testMapContext()
			now := time.Now().UTC()
			first, err := (Journal{EventStore: eventStore, MapContext: mapCtx}).Open(ctx)
			require.NoError(t, err)
			prefix := testJournalSecretPrefix + strings.Repeat("a", 10)
			plan := testUpdateEvent(2, now, harnessv2.UpdateEvent{
				Kind: harnessv2.UpdatePlan,
				Plan: &harnessv2.PlanUpdate{Entries: []harnessv2.PlanEntry{{
					Content: prefix, Status: harnessv2.PlanEntryInProgress,
				}}},
			})
			if _, added, err := first.AppendUpdateIfNew(ctx, plan); err != nil || !added {
				t.Fatalf("append first turn: added=%t err=%v", added, err)
			}

			mapCtx.TaskName, mapCtx.StreamID = testJournalSecondTaskName, testJournalSecondTaskName
			journal := Journal{EventStore: eventStore, MapContext: mapCtx}
			continued, err := journal.Open(ctx)
			require.NoError(t, err)
			require.True(t, continued.logicalFieldHistorySaturated, "continuation must enter fail-closed redaction")
			accepted := testUpdateEvent(1, now.Add(time.Second), harnessv2.UpdateEvent{})
			accepted.Type, accepted.Update = harnessv2.EventAccepted, nil
			accepted.Identity.TaskUID, accepted.Identity.PromptID = "task-uid-2", "prompt-2"
			accepted.Accepted = &harnessv2.AcceptedEvent{
				AcceptedAt: accepted.Identity.Timestamp,
				Lease:      harnessv2.PromptLease{Generation: 1, IssuedAt: now, ExpiresAt: now.Add(time.Minute)},
				ACPVersion: harnessv2.ACPProfileV1,
			}
			if err := eventStore.RegisterUsageTask(ctx, store.UsageTask{
				Namespace: mapCtx.Namespace, TaskUID: string(accepted.Identity.TaskUID), TaskName: mapCtx.TaskName,
				SessionName: mapCtx.SessionName, Runtime: "agent", Phase: "Succeeded", StartedAt: now,
			}); err != nil {
				t.Fatal(err)
			}
			if _, added, err := continued.AppendPromptLifecycleIfNew(ctx, accepted); err != nil || !added {
				t.Fatalf("append continuation acceptance: added=%t err=%v", added, err)
			}
			terminal := testTerminalEvent(3, now.Add(2*time.Second))
			terminal.Identity.TaskUID, terminal.Identity.PromptID = accepted.Identity.TaskUID, accepted.Identity.PromptID
			terminal.Completed = &harnessv2.CompletedEvent{StopReason: harnessv2.ACPStopReasonEndTurn,
				Result: harnessv2.PromptResult{Model: test.runtimeModel, Usage: test.usage,
					Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: testJournalDone}}}}
			if test.usage.Reported {
				if _, added, err := continued.AppendTerminalUsageIfNew(ctx, terminal); err != nil || !added {
					t.Fatalf("append continuation usage: added=%t err=%v", added, err)
				}
			}
			if _, added, err := continued.AppendPromptLifecycleIfNew(ctx, terminal); err != nil || !added {
				t.Fatalf("append continuation completion: added=%t err=%v", added, err)
			}
			listed, err := eventStore.ListExecutionEvents(ctx, store.ExecutionEventFilter{
				Namespace: mapCtx.Namespace, StreamType: store.ExecutionEventStreamTypeTask, StreamID: mapCtx.StreamID,
			})
			require.NoError(t, err)
			for _, event := range listed {
				var content struct {
					Model string `json:"model"`
				}
				require.NoError(t, json.Unmarshal(event.Content, &content))
				require.Equal(t, executionevents.ExecutionEventRedactedValue, content.Model, "continuation public model")
			}
			for _, taskName := range []string{testJournalTaskName, testJournalSecondTaskName} {
				require.NoError(t, eventStore.DeleteExecutionEvents(ctx, mapCtx.Namespace, store.ExecutionEventStreamTypeTask, taskName))
			}
			require.NoError(t, db.Close())
			db, err = sqlite.NewDB(path)
			require.NoError(t, err)
			eventStore = sqlite.NewStore(db, "usage")
			filter := store.UsageFilter{Namespaces: []string{mapCtx.Namespace}, Model: mapCtx.Model,
				From: now.Add(-time.Minute), Until: now.Add(time.Minute), AsOf: now.Add(time.Minute)}
			data, err := eventStore.LoadUsage(ctx, filter)
			require.NoError(t, err)
			report, err := usage.BuildPage(data, filter, usage.Page{Limit: 100}, "unassociated")
			require.NoError(t, err)
			var tasks []usage.Task
			for _, group := range report.OtherWork {
				tasks = append(tasks, group.Tasks...)
			}
			require.Len(t, tasks, 1, "model-filtered retained tasks")
			got := tasks[0]
			require.Len(t, got.Measurements, 1)
			require.Equal(t, string(accepted.Identity.TaskUID), got.TaskUID)
			require.Equal(t, mapCtx.Model, got.Measurements[0].Model)
			require.Equal(t, test.expectedTotal, got.Totals.TotalTokens)
			require.Equal(t, test.completeness, got.Totals.Completeness)
		})
	}
}
