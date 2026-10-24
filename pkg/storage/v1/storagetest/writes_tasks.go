package storagetest

import (
	"errors"
	"testing"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// observed returns value, or a stable name for err, so observations of a
// missing record compare equal.
func observed(t *testing.T, value any, err error) any {
	t.Helper()
	if errors.Is(err, storagev1.ErrNotFound) {
		return "not found"
	}
	require.NoError(t, err)
	return value
}

// fixed returns args that do not depend on the fence.
func fixed(args ...any) func(storagev1.ControllerEpochFence) []any {
	return func(storagev1.ControllerEpochFence) []any { return args }
}

// laterResultKey is the result key of a later Task created with the same
// name as taskResultKey("tenant").
var laterResultKey = storagev1.ResultKey{Namespace: "tenant", TaskName: "task", TaskUID: "task-later-uid"}

func observeResult(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	data, err := p.GetResult(t.Context(), taskResultKey("tenant"))
	later, laterErr := p.GetResult(t.Context(), laterResultKey)
	receipt, receiptErr := p.GetPromptResultReceipt(t.Context(), "attempt-1")
	return []any{observed(t, data, err), observed(t, later, laterErr), observed(t, receipt, receiptErr)}
}

func observePlan(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	plan, err := p.GetPlan(t.Context(), "tenant", "coordinator")
	return observed(t, plan, err)
}

func observeMessages(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	all := make([][]storagev1.Message, 0, 2)
	for _, reader := range []string{"worker-a", "worker-b"} {
		messages, err := p.GetMessages(t.Context(), "tenant", reader, "parent", false)
		require.NoError(t, err)
		all = append(all, messages)
	}
	return all
}

func sendFixtureMessages(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	for _, message := range []storagev1.Message{
		{Namespace: "tenant", FromTask: "worker-a", ToTask: "worker-b", ParentTask: "parent", Content: "to b"},
		{Namespace: "tenant", FromTask: "worker-b", ToTask: "worker-a", ParentTask: "parent", Content: "to a"},
		{Namespace: "tenant", FromTask: "worker-c", ToTask: "*", ParentTask: "parent", Content: "to all"},
	} {
		require.NoError(t, p.SendMessage(t.Context(), &message))
	}
}

func promptReceipt() storagev1.PromptResultReceipt {
	return storagev1.PromptResultReceipt{
		AttemptID: "attempt-1", Namespace: "tenant", TaskName: "task", TaskUID: taskResultKey("tenant").TaskUID,
		OperationID: "operation-1", OperationDigest: storagev1.CanonicalBytesDigest([]byte("operation-1")),
		Data: []byte("result bytes"),
	}
}

func init() {
	stateWriteCases = append(stateWriteCases,
		writeCase[storagev1.StateProvider]{
			method: "SaveResult",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.SaveResult(t.Context(), taskResultKey("tenant"), []byte("old")))
			},
			args:    fixed(taskResultKey("tenant"), []byte("new result")),
			observe: observeResult,
			afterCommit: func(t *testing.T, p storagev1.StateProvider) {
				// Issued after the case's write, so it replaces the result,
				// and a replay of the older write must not restore it.
				newer := EnvelopeContext(t.Context(), t, p, suiteFence, "SaveResult", taskResultKey("tenant"), []byte("newer result"))
				require.NoError(t, p.SaveResult(newer, taskResultKey("tenant"), []byte("newer result")))
			},
		},
		writeCase[storagev1.StateProvider]{
			method: "DeleteResult",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.SaveResult(t.Context(), taskResultKey("tenant"), []byte("result")))
				require.NoError(t, p.SavePromptResultReceipt(t.Context(), promptReceipt()))
			},
			args:    fixed(taskResultKey("tenant")),
			observe: observeResult,
			afterCommit: func(t *testing.T, p storagev1.StateProvider) {
				// The deleted Task's UID is refused from now on, so a later
				// Task with the same name writes again.
				require.NoError(t, p.SaveResult(t.Context(), laterResultKey, []byte("written again")))
			},
		},
		writeCase[storagev1.StateProvider]{
			method:  "SavePromptResultReceipt",
			args:    func(storagev1.ControllerEpochFence) []any { return []any{promptReceipt()} },
			observe: observeResult,
		},
		writeCase[storagev1.StateProvider]{
			method: "SavePlan",
			args: func(storagev1.ControllerEpochFence) []any {
				return []any{"tenant", "coordinator", &storagev1.PlanState{Iteration: 2, Summary: "done", ProgressPct: 100, PlanDocument: "# done"}}
			},
			observe: observePlan,
		},
		writeCase[storagev1.StateProvider]{
			method: "DeletePlan",
			setup: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.SavePlan(t.Context(), "tenant", "coordinator", &storagev1.PlanState{Iteration: 1, Summary: "started"}))
			},
			args:    fixed("tenant", "coordinator"),
			observe: observePlan,
			afterCommit: func(t *testing.T, p storagev1.StateProvider) {
				require.NoError(t, p.SavePlan(t.Context(), "tenant", "coordinator", &storagev1.PlanState{Iteration: 3, Summary: "again"}))
			},
		},
		writeCase[storagev1.StateProvider]{
			method: "SendMessage",
			args: func(storagev1.ControllerEpochFence) []any {
				return []any{&storagev1.Message{Namespace: "tenant", FromTask: "worker-a", ToTask: "worker-b", ParentTask: "parent", Content: "hello"}}
			},
			observe: observeMessages,
		},
		writeCase[storagev1.StateProvider]{
			method:  "GetMessages",
			setup:   sendFixtureMessages,
			args:    fixed("tenant", "worker-b", "parent", true),
			observe: observeMessages,
		},
		writeCase[storagev1.StateProvider]{
			method:      "DeleteTaskMessages",
			setup:       sendFixtureMessages,
			args:        fixed("tenant", "worker-a"),
			observe:     observeMessages,
			afterCommit: sendFixtureMessages,
		},
		writeCase[storagev1.StateProvider]{
			method:  "DeleteParentMessages",
			setup:   sendFixtureMessages,
			args:    fixed("tenant", "parent"),
			observe: observeMessages,
		},
	)
}
