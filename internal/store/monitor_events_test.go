package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"testing"
)

func TestRepositoryMonitorEventDecisionCoversDecisionTypesOnly(t *testing.T) {
	tests := map[string]string{
		"repair_task_created":    RepositoryMonitorDecisionAccepted,
		"command_rejected":       RepositoryMonitorDecisionRefused,
		"item_skipped":           RepositoryMonitorDecisionHeld,
		"issue_action_blocked":   RepositoryMonitorDecisionBlocked,
		"repair_succeeded":       RepositoryMonitorDecisionActed,
		" run_failed ":           RepositoryMonitorDecisionFailed,
		"run_started":            "",
		"item_selected":          "",
		"review_result_ingested": "",
	}
	for eventType, want := range tests {
		if got := RepositoryMonitorEventDecision(eventType); got != want {
			t.Errorf("RepositoryMonitorEventDecision(%q) = %q, want %q", eventType, got, want)
		}
	}
}

func TestNewRepositoryMonitorEventStampsDecisionAndPolicy(t *testing.T) {
	event, err := NewRepositoryMonitorEvent(RepositoryMonitorEventInput{
		MonitorNamespace: "default", MonitorName: "m", RunID: "run-1",
		ItemKind: "pull_request", ItemNumber: 7, ItemSHA: "head",
		EventType: "repair_refused", Summary: "refused",
		Metadata: map[string]any{RepositoryMonitorEventReasonField: "repair_pr_budget_exhausted"},
		Policy:   "spec.repair.maxRepairsPerPR=3",
	})
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := json.Unmarshal([]byte(event.MetadataJSON), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata[RepositoryMonitorEventDecisionField] != RepositoryMonitorDecisionRefused || metadata[RepositoryMonitorEventPolicyField] != "spec.repair.maxRepairsPerPR=3" {
		t.Fatalf("metadata = %v, want refused decision and policy", metadata)
	}

	lifecycle, err := NewRepositoryMonitorEvent(RepositoryMonitorEventInput{MonitorNamespace: "default", MonitorName: "m", RunID: "run-1", EventType: "run_started"})
	if err != nil {
		t.Fatal(err)
	}
	if lifecycle.MetadataJSON != "{}" {
		t.Fatalf("lifecycle metadata = %s, want no decision", lifecycle.MetadataJSON)
	}
	explicit, err := NewRepositoryMonitorEvent(RepositoryMonitorEventInput{ID: "mevt-fixed", MonitorNamespace: "default", MonitorName: "m", EventType: "update_branch_succeeded"})
	if err != nil || explicit.ID != "mevt-fixed" {
		t.Fatalf("explicit ID = %q err=%v, want it kept", explicit.ID, err)
	}
}

// Holds and refusals are identified without their run, so repeated polls
// collapse into one event per head, reason and policy; other decisions keep
// one event per run step.
func TestNewRepositoryMonitorEventIDs(t *testing.T) {
	id := func(eventType, runID, head, reason, policy string, extra map[string]any) string {
		t.Helper()
		metadata := map[string]any{RepositoryMonitorEventReasonField: reason}
		maps.Copy(metadata, extra)
		event, err := NewRepositoryMonitorEvent(RepositoryMonitorEventInput{
			MonitorNamespace: "default", MonitorName: "m", RunID: runID,
			ItemKind: "pull_request", ItemNumber: 7, ItemSHA: head,
			EventType: eventType, Summary: "Pull request #7: " + reason, Metadata: metadata, Policy: policy,
		})
		if err != nil {
			t.Fatal(err)
		}
		return event.ID
	}
	held := id("item_skipped", "run-1", "head-a", "draft", "spec.targets.pullRequests.includeDrafts=false", nil)
	if held != id("item_skipped", "run-2", "head-a", "draft", "spec.targets.pullRequests.includeDrafts=false", map[string]any{"labels": []string{"x"}}) {
		t.Fatal("a repeated hold on one head got a new event ID")
	}
	for name, other := range map[string]string{
		"new head":   id("item_skipped", "run-2", "head-b", "draft", "spec.targets.pullRequests.includeDrafts=false", nil),
		"new reason": id("item_skipped", "run-2", "head-a", "ci_not_green", "spec.targets.pullRequests.includeDrafts=false", nil),
		"new policy": id("item_skipped", "run-2", "head-a", "draft", "", nil),
		"command":    id("item_skipped", "run-2", "head-a", "draft", "spec.targets.pullRequests.includeDrafts=false", map[string]any{"commandEventID": "cmd-1"}),
	} {
		if other == held {
			t.Errorf("%s reused the hold's event ID", name)
		}
	}
	accepted := id("review_task_created", "run-1", "head-a", "", "", map[string]any{"taskName": "t"})
	if accepted != id("review_task_created", "run-1", "head-a", "", "", map[string]any{"taskName": "t"}) {
		t.Fatal("a replayed step got a new event ID")
	}
	if accepted == id("review_task_created", "run-2", "head-a", "", "", map[string]any{"taskName": "t"}) {
		t.Fatal("a new run reused an accepted step's event ID")
	}
	if accepted != id("review_task_created", "run-1", "head-a", "", "", map[string]any{"taskName": "t", "created": false}) {
		t.Fatal("adopting a created Task got a new event ID")
	}
	if accepted == id("review_task_created", "run-1", "head-a", "", "", map[string]any{"taskName": "u"}) {
		t.Fatal("a new Task reused an accepted step's event ID")
	}
	if RepositoryMonitorEventID("a", "b|c", "", 0, "", "t") == RepositoryMonitorEventID("a|b", "c", "", 0, "", "t") {
		t.Fatal("event ID parts are ambiguous")
	}
}

type monitorEventWriterFunc func(context.Context, *MonitorEvent) error

func (f monitorEventWriterFunc) CreateMonitorEvent(ctx context.Context, event *MonitorEvent) error {
	return f(ctx, event)
}

func TestRecordRepositoryMonitorEventTreatsConflictAsRecorded(t *testing.T) {
	conflict := monitorEventWriterFunc(func(context.Context, *MonitorEvent) error {
		return fmt.Errorf("insert: %w", ErrConflict)
	})
	if err := RecordRepositoryMonitorEvent(t.Context(), conflict, &MonitorEvent{}); err != nil {
		t.Fatalf("conflict error = %v, want nil", err)
	}
	failure := errors.New("disk full")
	failing := monitorEventWriterFunc(func(context.Context, *MonitorEvent) error { return failure })
	if err := RecordRepositoryMonitorEvent(t.Context(), failing, &MonitorEvent{}); !errors.Is(err, failure) {
		t.Fatalf("write error = %v, want %v", err, failure)
	}
}
