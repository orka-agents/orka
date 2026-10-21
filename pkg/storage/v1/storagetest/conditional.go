package storagetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// The cases in this file hold providers to the conditional commands that
// replace Go callbacks: Orka resolves a target, authorizes or computes in its
// own process with no provider transaction open, and the provider applies
// only while the resolved target and version still match.

const conditionalNamespace = "tenant-conditional"

func acceptedMemoryProposal(t *testing.T, p storagev1.StateProvider, id string) *storagev1.MemoryProposal {
	t.Helper()
	proposal := &storagev1.MemoryProposal{ID: id, Namespace: conditionalNamespace, TaskName: "task-a", AgentName: "agent-a",
		Type: storagev1.MemoryProposalTypeMemory, Title: "Proposal " + id, Content: "content of " + id}
	require.NoError(t, p.CreateMemoryProposal(t.Context(), proposal))
	require.NoError(t, p.ReviewMemoryProposal(t.Context(), storagev1.MemoryProposalReview{
		Namespace: conditionalNamespace, ID: id, Status: storagev1.MemoryProposalStatusAccepted, Reviewer: "reviewer",
	}))
	return proposal
}

func proposalMemory(proposal *storagev1.MemoryProposal, at time.Time) *storagev1.Memory {
	return &storagev1.Memory{Namespace: proposal.Namespace, TaskName: proposal.TaskName, AgentName: proposal.AgentName,
		Content: proposal.Content, Tags: []string{"applied"}, CreatedAt: at, UpdatedAt: at}
}

// applyProposal resolves and commits until the target holds, as Orka does.
func applyProposal(ctx context.Context, p storagev1.StateProvider, proposal *storagev1.MemoryProposal, appliedBy string, at time.Time) (*storagev1.Memory, error) {
	for range 2 * concurrentCallers {
		target, err := p.ResolveMemoryProposalApplication(ctx, proposal.Namespace, proposal.ID)
		if err != nil {
			return nil, err
		}
		application := storagev1.MemoryProposalApplication{Target: target, AppliedBy: appliedBy, AppliedAt: at}
		if target.MemoryID == "" {
			application.Memory = proposalMemory(proposal, at)
		}
		memory, err := p.CommitMemoryProposalApplication(ctx, application)
		if !errors.Is(err, storagev1.ErrPreconditionFailed) {
			return memory, err
		}
	}
	return nil, fmt.Errorf("memory proposal target kept changing")
}

func runMemoryProposalApplication(t *testing.T, newProvider StateFactory) {
	at := time.Date(2026, 6, 11, 10, 0, 0, 123456789, time.UTC)

	t.Run("resolve names the target without content and commit applies it once", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		proposal := acceptedMemoryProposal(t, p, "proposal-create")
		target, err := p.ResolveMemoryProposalApplication(ctx, conditionalNamespace, proposal.ID)
		require.NoError(t, err)
		require.Equal(t, storagev1.MemoryProposalApplyTarget{
			Namespace: conditionalNamespace, ProposalID: proposal.ID, ProposalStatus: storagev1.MemoryProposalStatusAccepted,
		}, target, "a proposal that creates a memory resolves to no memory")

		memory, err := p.CommitMemoryProposalApplication(ctx, storagev1.MemoryProposalApplication{
			Target: target, AppliedBy: "applier", AppliedAt: at, Memory: proposalMemory(proposal, at),
		})
		require.NoError(t, err)
		require.NotEmpty(t, memory.ID)
		require.Equal(t, storagev1.MemorySourceProposal, memory.Source, "the provider records the proposal as the source")
		require.Equal(t, proposal.ID, memory.SourceProposalID)
		require.Equal(t, conditionalNamespace, memory.Namespace)
		stored, err := p.GetMemory(ctx, conditionalNamespace, memory.ID)
		require.NoError(t, err)
		require.Equal(t, proposal.Content, stored.Content)
		applied, err := p.GetMemoryProposal(ctx, conditionalNamespace, proposal.ID)
		require.NoError(t, err)
		require.Equal(t, storagev1.MemoryProposalStatusApplied, applied.Status)
		require.Equal(t, memory.ID, applied.AppliedMemoryID)
		require.Equal(t, "applier", applied.AppliedBy)
		require.NotNil(t, applied.AppliedAt)
		requireSameInstant(t, at, *applied.AppliedAt, "application time")

		replay, err := p.ResolveMemoryProposalApplication(ctx, conditionalNamespace, proposal.ID)
		require.NoError(t, err)
		require.Equal(t, memory.ID, replay.MemoryID, "an applied proposal resolves to its memory")
		again, err := p.CommitMemoryProposalApplication(ctx, storagev1.MemoryProposalApplication{Target: replay, AppliedBy: "other", AppliedAt: at.Add(time.Hour)})
		require.NoError(t, err)
		require.Equal(t, memory.ID, again.ID)
		unchanged, err := p.GetMemoryProposal(ctx, conditionalNamespace, proposal.ID)
		require.NoError(t, err)
		require.Equal(t, applied, unchanged, "replaying an application changes nothing")
		memories, err := p.ListMemories(ctx, storagev1.MemoryFilter{Namespace: conditionalNamespace, Source: storagev1.MemorySourceProposal})
		require.NoError(t, err)
		require.Equal(t, []string{memory.ID}, memoryIDs(memories))
	})

	t.Run("a memory created from the proposal is linked rather than duplicated", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		proposal := acceptedMemoryProposal(t, p, "proposal-linked")
		linked := &storagev1.Memory{ID: "mem-linked", Namespace: conditionalNamespace, Source: storagev1.MemorySourceProposal,
			SourceProposalID: proposal.ID, Content: "already created", CreatedAt: at, UpdatedAt: at}
		require.NoError(t, p.CreateMemory(ctx, linked))
		target, err := p.ResolveMemoryProposalApplication(ctx, conditionalNamespace, proposal.ID)
		require.NoError(t, err)
		require.Equal(t, "mem-linked", target.MemoryID)
		forged := target
		forged.MemoryID = "mem-other"
		memory, err := p.CommitMemoryProposalApplication(ctx, storagev1.MemoryProposalApplication{Target: forged, AppliedBy: "applier", AppliedAt: at})
		require.ErrorIs(t, err, storagev1.ErrPreconditionFailed, "a target naming another memory is refused even with the current proposal state")
		require.Nil(t, memory)
		memory, err = p.CommitMemoryProposalApplication(ctx, storagev1.MemoryProposalApplication{Target: target, AppliedBy: "applier", AppliedAt: at})
		require.NoError(t, err)
		require.Equal(t, "mem-linked", memory.ID)
		applied, err := p.GetMemoryProposal(ctx, conditionalNamespace, proposal.ID)
		require.NoError(t, err)
		require.Equal(t, storagev1.MemoryProposalStatusApplied, applied.Status)
		require.Equal(t, "mem-linked", applied.AppliedMemoryID)
	})

	t.Run("a changed target returns no memory and changes nothing", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		proposal := acceptedMemoryProposal(t, p, "proposal-raced")
		stale, err := p.ResolveMemoryProposalApplication(ctx, conditionalNamespace, proposal.ID)
		require.NoError(t, err)
		winner, err := applyProposal(ctx, p, proposal, "winner", at)
		require.NoError(t, err)

		// The caller authorized creating a memory, not reading the winner's.
		memory, err := p.CommitMemoryProposalApplication(ctx, storagev1.MemoryProposalApplication{
			Target: stale, AppliedBy: "late", AppliedAt: at, Memory: proposalMemory(proposal, at),
		})
		require.ErrorIs(t, err, storagev1.ErrPreconditionFailed)
		require.ErrorIs(t, err, storagev1.ErrConflict)
		require.Nil(t, memory, "a precondition failure discloses no memory")
		forged := stale
		forged.MemoryID = "mem-someone-else"
		memory, err = p.CommitMemoryProposalApplication(ctx, storagev1.MemoryProposalApplication{Target: forged, AppliedBy: "late", AppliedAt: at})
		require.ErrorIs(t, err, storagev1.ErrPreconditionFailed)
		require.Nil(t, memory)
		memories, err := p.ListMemories(ctx, storagev1.MemoryFilter{Namespace: conditionalNamespace, Source: storagev1.MemorySourceProposal})
		require.NoError(t, err)
		require.Equal(t, []string{winner.ID}, memoryIDs(memories))
		applied, err := p.GetMemoryProposal(ctx, conditionalNamespace, proposal.ID)
		require.NoError(t, err)
		require.Equal(t, "winner", applied.AppliedBy)

		archived := acceptedMemoryProposal(t, p, "proposal-archived")
		target, err := p.ResolveMemoryProposalApplication(ctx, conditionalNamespace, archived.ID)
		require.NoError(t, err)
		require.NoError(t, p.ArchiveMemoryProposal(ctx, conditionalNamespace, archived.ID))
		memory, err = p.CommitMemoryProposalApplication(ctx, storagev1.MemoryProposalApplication{
			Target: target, AppliedBy: "late", AppliedAt: at, Memory: proposalMemory(archived, at),
		})
		require.ErrorIs(t, err, storagev1.ErrPreconditionFailed, "a stale version is refused with the typed error")
		require.Nil(t, memory)
		_, err = p.ResolveMemoryProposalApplication(ctx, conditionalNamespace, archived.ID)
		require.ErrorIs(t, err, storagev1.ErrValidation, "the next resolution reports why it cannot apply")
		current, err := p.GetMemoryProposal(ctx, conditionalNamespace, archived.ID)
		require.NoError(t, err)
		require.Equal(t, storagev1.MemoryProposalStatusArchived, current.Status)
		require.Empty(t, current.AppliedMemoryID)
	})

	t.Run("only accepted memory proposals resolve", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		_, err := p.ResolveMemoryProposalApplication(ctx, conditionalNamespace, "missing")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		pending := &storagev1.MemoryProposal{ID: "pending", Namespace: conditionalNamespace, Type: storagev1.MemoryProposalTypeMemory, Title: "pending", Content: "x"}
		require.NoError(t, p.CreateMemoryProposal(ctx, pending))
		_, err = p.ResolveMemoryProposalApplication(ctx, conditionalNamespace, pending.ID)
		require.ErrorIs(t, err, storagev1.ErrValidation)
		skill := &storagev1.MemoryProposal{ID: "skill", Namespace: conditionalNamespace, Type: storagev1.MemoryProposalTypeSkill, Title: "skill", Content: "x"}
		require.NoError(t, p.CreateMemoryProposal(ctx, skill))
		require.NoError(t, p.ReviewMemoryProposal(ctx, storagev1.MemoryProposalReview{Namespace: conditionalNamespace, ID: skill.ID, Status: storagev1.MemoryProposalStatusAccepted}))
		_, err = p.ResolveMemoryProposalApplication(ctx, conditionalNamespace, skill.ID)
		require.ErrorIs(t, err, storagev1.ErrValidation)
		_, err = p.ResolveMemoryProposalApplication(ctx, "other-namespace", pending.ID)
		require.ErrorIs(t, err, storagev1.ErrNotFound, "a proposal resolves only in its namespace")

		proposal := acceptedMemoryProposal(t, p, "proposal-shape")
		target, err := p.ResolveMemoryProposalApplication(ctx, conditionalNamespace, proposal.ID)
		require.NoError(t, err)
		_, err = p.CommitMemoryProposalApplication(ctx, storagev1.MemoryProposalApplication{Target: target, AppliedAt: at})
		require.ErrorIs(t, err, storagev1.ErrValidation, "creating a memory requires the memory")
		blank := proposalMemory(proposal, at)
		blank.Content = "  "
		_, err = p.CommitMemoryProposalApplication(ctx, storagev1.MemoryProposalApplication{Target: target, AppliedAt: at, Memory: blank})
		require.ErrorIs(t, err, storagev1.ErrValidation)
	})

	t.Run("competing applications create one memory", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		proposal := acceptedMemoryProposal(t, p, "proposal-contended")
		ids := make([]string, concurrentCallers)
		errs := runConcurrently(func(caller int) error {
			memory, err := applyProposal(ctx, p, proposal, fmt.Sprintf("applier-%d", caller), at)
			if err == nil {
				ids[caller] = memory.ID
			}
			return err
		})
		for caller, err := range errs {
			require.NoError(t, err, "caller %d", caller)
			require.Equal(t, ids[0], ids[caller], "every caller receives the one memory")
		}
		memories, err := p.ListMemories(ctx, storagev1.MemoryFilter{Namespace: conditionalNamespace, Source: storagev1.MemorySourceProposal})
		require.NoError(t, err)
		require.Equal(t, []string{ids[0]}, memoryIDs(memories))
		applied, err := p.GetMemoryProposal(ctx, conditionalNamespace, proposal.ID)
		require.NoError(t, err)
		require.Equal(t, ids[0], applied.AppliedMemoryID)
	})
}

// fencedOperation is one fence-checking method, called with ctx.
type fencedOperation struct {
	name string
	call func(ctx context.Context, p storagev1.StateProvider) error
}

func fencedOperations(at time.Time) []fencedOperation {
	ns := conditionalNamespace
	event := func() *storagev1.ExecutionEvent {
		return taskEvent(ns, "task-fenced", storagev1.ExecutionEventTypeTaskStarted)
	}
	return []fencedOperation{
		{"SaveResult", func(ctx context.Context, p storagev1.StateProvider) error {
			return p.SaveResult(ctx, ns, "task-fenced", []byte("result"))
		}},
		{"SavePlan", func(ctx context.Context, p storagev1.StateProvider) error {
			return p.SavePlan(ctx, ns, "task-fenced", &storagev1.PlanState{Summary: "plan"})
		}},
		{"GetPlan", func(ctx context.Context, p storagev1.StateProvider) error {
			_, err := p.GetPlan(ctx, ns, "task-fenced")
			return err
		}},
		{"SendMessage", func(ctx context.Context, p storagev1.StateProvider) error {
			return p.SendMessage(ctx, &storagev1.Message{Namespace: ns, FromTask: "task-fenced", ToTask: "peer", ParentTask: "parent", Content: "hello"})
		}},
		{"GetMessages", func(ctx context.Context, p storagev1.StateProvider) error {
			_, err := p.GetMessages(ctx, ns, "peer", "parent", true)
			return err
		}},
		{"GetSession", func(ctx context.Context, p storagev1.StateProvider) error {
			_, err := p.GetSession(ctx, ns, "fenced-session")
			return err
		}},
		{"GetSessionType", func(ctx context.Context, p storagev1.StateProvider) error {
			_, err := p.GetSessionType(ctx, ns, "fenced-session")
			return err
		}},
		{"LoadTranscript", func(ctx context.Context, p storagev1.StateProvider) error {
			_, err := p.LoadTranscript(ctx, ns, "fenced-session", 0)
			return err
		}},
		{"LoadTranscriptThrough", func(ctx context.Context, p storagev1.StateProvider) error {
			_, err := p.LoadTranscriptThrough(ctx, ns, "fenced-session", "message-1", 0)
			return err
		}},
		{"SearchTranscript", func(ctx context.Context, p storagev1.StateProvider) error {
			_, err := p.SearchTranscript(ctx, storagev1.TranscriptSearchFilter{Namespace: ns, Query: "hello"})
			return err
		}},
		{"ReadSessionSoul", func(ctx context.Context, p storagev1.StateProvider) error {
			_, err := p.ReadSessionSoul(ctx, ns, "fenced-session", "task-fenced", "task-uid")
			return err
		}},
		{"GetGatewayEventForTask", func(ctx context.Context, p storagev1.StateProvider) error {
			_, err := p.GetGatewayEventForTask(ctx, ns, "task-fenced", "task-uid")
			return err
		}},
		{"GetGatewayMessageBudget", func(ctx context.Context, p storagev1.StateProvider) error {
			_, err := p.GetGatewayMessageBudget(ctx, storagev1.GatewayMessageBudgetQuery{Namespace: ns, NamespaceUID: "namespace-uid",
				EventID: "event", TaskName: "task-fenced", TaskUID: "task-uid", RequestID: "request"})
			return err
		}},
		{"EnqueueGatewayMessage", func(ctx context.Context, p storagev1.StateProvider) error {
			_, _, err := p.EnqueueGatewayMessage(ctx, storagev1.GatewayMessageEnqueue{Namespace: ns, NamespaceUID: "namespace-uid",
				EventID: "event", TaskName: "task-fenced", TaskUID: "task-uid", RequestID: "request", Text: "hello",
				MaxMessages: 3, MaxAttempts: 3, Now: at, ExpiresAt: at.Add(time.Hour)})
			return err
		}},
		{"AppendExecutionEvent", func(ctx context.Context, p storagev1.StateProvider) error {
			_, err := p.AppendExecutionEvent(ctx, event())
			return err
		}},
		{"AppendExecutionEventIfAbsent", func(ctx context.Context, p storagev1.StateProvider) error {
			_, _, err := p.AppendExecutionEventIfAbsent(ctx, event(), "fenced-key")
			return err
		}},
		{"AppendExecutionEventWithPlanIfAbsent", func(ctx context.Context, p storagev1.StateProvider) error {
			_, _, err := p.AppendExecutionEventWithPlanIfAbsent(ctx, event(), "fenced-plan-key",
				&storagev1.PlanState{Namespace: ns, TaskName: "task-fenced", Summary: "plan"})
			return err
		}},
		{"AppendExecutionEventsIfAbsent", func(ctx context.Context, p storagev1.StateProvider) error {
			_, err := p.AppendExecutionEventsIfAbsent(ctx, []storagev1.ExecutionEventAppend{{Event: event(), DedupeKey: "fenced-group-key"}})
			return err
		}},
		{"ListExecutionEvents", func(ctx context.Context, p storagev1.StateProvider) error {
			_, err := p.ListExecutionEvents(ctx, storagev1.ExecutionEventFilter{Namespace: ns, StreamID: "task-fenced"})
			return err
		}},
		{"ListHarnessV1AttemptsByTask", func(ctx context.Context, p storagev1.StateProvider) error {
			_, err := p.ListHarnessV1AttemptsByTask(ctx, ns, "task-uid")
			return err
		}},
		{"CheckTaskJobAuthority", func(ctx context.Context, p storagev1.StateProvider) error {
			return p.CheckTaskJobAuthority(ctx, storagev1.TaskJobIdentity{Namespace: ns, TaskUID: "task-uid", JobUID: "job"})
		}},
	}
}

// requireNoFencedEffects checks that refused fenced writes left nothing.
func requireNoFencedEffects(t *testing.T, p storagev1.StateProvider) {
	t.Helper()
	ctx := t.Context()
	_, err := p.GetResult(ctx, conditionalNamespace, "task-fenced")
	require.ErrorIs(t, err, storagev1.ErrNotFound, "a refused result write")
	_, err = p.GetPlan(ctx, conditionalNamespace, "task-fenced")
	require.ErrorIs(t, err, storagev1.ErrNotFound, "a refused plan write")
	messages, err := p.GetMessages(ctx, conditionalNamespace, "peer", "parent", false)
	require.NoError(t, err)
	require.Empty(t, messages, "a refused message")
	require.Empty(t, listEvents(t, p, storagev1.ExecutionEventFilter{Namespace: conditionalNamespace, StreamID: "task-fenced"}), "refused appends")
}

func runTaskDataFences(t *testing.T, newProvider StateFactory) {
	at := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)

	t.Run("registration isolates a Task from other Tasks' cleanup", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		_, _, err := p.ResolveTaskDataFence(ctx, "", "")
		require.ErrorIs(t, err, storagev1.ErrValidation)
		_, _, err = p.RegisterTaskDataFence(ctx, conditionalNamespace, "")
		require.ErrorIs(t, err, storagev1.ErrValidation, "only a Task is registered")

		unregistered, registered, err := p.ResolveTaskDataFence(ctx, conditionalNamespace, "task-a")
		require.NoError(t, err)
		require.False(t, registered)
		require.NoError(t, p.DeleteResult(ctx, conditionalNamespace, "task-b"))
		require.ErrorIs(t, p.CheckTaskDataFence(ctx, unregistered), storagev1.ErrTaskDataCleanupChanged,
			"an unregistered Task follows the namespace generation")

		fence, created, err := p.RegisterTaskDataFence(ctx, conditionalNamespace, "task-a")
		require.NoError(t, err)
		require.True(t, created)
		again, created, err := p.RegisterTaskDataFence(ctx, conditionalNamespace, "task-a")
		require.NoError(t, err)
		require.False(t, created, "registration is idempotent")
		require.Equal(t, fence, again)
		resolved, registered, err := p.ResolveTaskDataFence(ctx, conditionalNamespace, "task-a")
		require.NoError(t, err)
		require.True(t, registered)
		require.Equal(t, fence, resolved)

		require.NoError(t, p.DeleteResult(ctx, conditionalNamespace, "task-b"))
		require.NoError(t, p.CheckTaskDataFence(ctx, fence), "cleanup of another Task leaves a registered fence current")
		namespace, _, err := p.ResolveTaskDataFence(ctx, conditionalNamespace, "")
		require.NoError(t, err)
		require.NoError(t, p.DeletePlan(ctx, conditionalNamespace, "task-a"))
		require.ErrorIs(t, p.CheckTaskDataFence(ctx, fence), storagev1.ErrTaskDataCleanupChanged, "the Task's own cleanup")
		require.ErrorIs(t, p.CheckTaskDataFence(ctx, namespace), storagev1.ErrTaskDataCleanupChanged, "any cleanup moves the namespace fence")

		current, _, err := p.ResolveTaskDataFence(ctx, conditionalNamespace, "task-a")
		require.NoError(t, err)
		require.NoError(t, p.CheckTaskDataFence(ctx, current))
		require.NoError(t, p.DeleteTaskJobRevocations(ctx, conditionalNamespace, "task-a", "task-a-uid"))
		require.ErrorIs(t, p.CheckTaskDataFence(ctx, current), storagev1.ErrTaskDataCleanupChanged,
			"finalizer reclamation invalidates every earlier fence")
		_, registered, err = p.ResolveTaskDataFence(ctx, conditionalNamespace, "task-a")
		require.NoError(t, err)
		require.False(t, registered, "finalizer reclamation removes the registration")
	})

	t.Run("release removes only an unchanged registration", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		fence, created, err := p.RegisterTaskDataFence(ctx, conditionalNamespace, "task-a")
		require.NoError(t, err)
		require.True(t, created)
		require.NoError(t, p.DeleteResult(ctx, conditionalNamespace, "task-a"))
		require.NoError(t, p.ReleaseTaskDataFence(ctx, fence), "releasing a moved registration is a no-op")
		_, registered, err := p.ResolveTaskDataFence(ctx, conditionalNamespace, "task-a")
		require.NoError(t, err)
		require.True(t, registered, "a registration cleanup moved is kept")

		other, created, err := p.RegisterTaskDataFence(ctx, conditionalNamespace, "task-b")
		require.NoError(t, err)
		require.True(t, created)
		require.NoError(t, p.ReleaseTaskDataFence(ctx, other))
		_, registered, err = p.ResolveTaskDataFence(ctx, conditionalNamespace, "task-b")
		require.NoError(t, err)
		require.False(t, registered)
	})

	t.Run("every fence-checking method refuses a stale fence", func(t *testing.T) {
		for _, operation := range fencedOperations(at) {
			t.Run(operation.name, func(t *testing.T) {
				p, ctx := newProvider(t), t.Context()
				createSessionFixture(t, p, conditionalNamespace, "fenced-session", sessionsTypeTask, at)
				current, _, err := p.ResolveTaskDataFence(ctx, conditionalNamespace, "")
				require.NoError(t, err)
				err = operation.call(storagev1.WithTaskDataFence(ctx, current), p)
				require.NotErrorIs(t, err, storagev1.ErrTaskDataCleanupChanged, "a current fence does not refuse the operation")
				require.NotErrorIs(t, err, storagev1.ErrTaskJobRevoked)

				p = newProvider(t)
				createSessionFixture(t, p, conditionalNamespace, "fenced-session", sessionsTypeTask, at)
				stale, _, err := p.ResolveTaskDataFence(ctx, conditionalNamespace, "")
				require.NoError(t, err)
				require.NoError(t, p.DeleteResult(ctx, conditionalNamespace, "unrelated-task"))
				err = operation.call(storagev1.WithTaskDataFence(ctx, stale), p)
				require.ErrorIs(t, err, storagev1.ErrTaskDataCleanupChanged, "a stale fence is refused before the operation runs")
				requireNoFencedEffects(t, p)
			})
		}
	})

	t.Run("a revoked fence Job refuses fenced operations", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		job := storagev1.TaskJobIdentity{Namespace: conditionalNamespace, TaskUID: "task-uid", JobUID: "job-old"}
		fence, _, err := p.RegisterTaskDataFence(ctx, conditionalNamespace, "task-fenced")
		require.NoError(t, err)
		fence.Jobs = []storagev1.TaskJobIdentity{job}
		fenced := storagev1.WithTaskDataFence(ctx, fence)
		require.NoError(t, p.SaveResult(fenced, conditionalNamespace, "task-fenced", []byte("first")))
		require.NoError(t, p.RevokeTaskJob(ctx, job))
		require.ErrorIs(t, p.SaveResult(fenced, conditionalNamespace, "task-fenced", []byte("late")), storagev1.ErrTaskJobRevoked)
		require.ErrorIs(t, p.CheckTaskDataFence(ctx, fence), storagev1.ErrTaskJobRevoked)
		result, err := p.GetResult(ctx, conditionalNamespace, "task-fenced")
		require.NoError(t, err)
		require.Equal(t, "first", string(result), "a revoked Job's write has no effect")
	})

	t.Run("a fence bound to a harness-v1 attempt refuses once it ends or is superseded", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		epoch := syncEpoch(t, p, 1, "owner-a")
		first := harnessAttempt("task-uid", 1)
		require.NoError(t, p.CreateHarnessV1Attempt(ctx, first, epoch))
		advance := func(attempt *storagev1.HarnessV1Attempt, version int64, from, to storagev1.HarnessV1AttemptState) {
			t.Helper()
			transition := storagev1.HarnessV1AttemptTransition{
				Key: harnessKey(attempt), ExpectedVersion: version, ExpectedState: from, TargetState: to,
				OperationID: fmt.Sprintf("%d-%s", attempt.Attempt, to), OperationDigest: controlDigest(string(to)), Fence: epoch,
			}
			if storagev1.IsTerminalHarnessV1AttemptState(to) {
				receipt := controlDigest("terminal")
				transition.Updates.TerminalReceiptDigest = &receipt
			}
			_, err := p.TransitionHarnessV1Attempt(ctx, transition)
			require.NoError(t, err)
		}
		fence, _, err := p.ResolveTaskDataFence(ctx, "tenant", "")
		require.NoError(t, err)
		key := harnessKey(first)
		fence.HarnessV1Attempt = &key
		fenced := storagev1.WithTaskDataFence(ctx, fence)
		require.ErrorIs(t, p.SaveResult(fenced, "tenant", "task-a", []byte("prepared")), storagev1.ErrTaskJobRevoked,
			"a prepared attempt is not active yet")
		advance(first, 1, storagev1.HarnessV1AttemptPrepared, storagev1.HarnessV1AttemptSubmitting)
		require.NoError(t, p.SaveResult(fenced, "tenant", "task-a", []byte("active")))

		second := harnessAttempt("task-uid", 2)
		require.NoError(t, p.CreateHarnessV1Attempt(ctx, second, epoch))
		require.ErrorIs(t, p.SaveResult(fenced, "tenant", "task-a", []byte("superseded")), storagev1.ErrTaskJobRevoked,
			"a newer attempt supersedes the fenced one")

		secondKey := harnessKey(second)
		fence.HarnessV1Attempt = &secondKey
		fencedSecond := storagev1.WithTaskDataFence(ctx, fence)
		advance(second, 1, storagev1.HarnessV1AttemptPrepared, storagev1.HarnessV1AttemptSubmitting)
		require.NoError(t, p.SaveResult(fencedSecond, "tenant", "task-a", []byte("second")))
		advance(second, 2, storagev1.HarnessV1AttemptSubmitting, storagev1.HarnessV1AttemptRejected)
		require.ErrorIs(t, p.SaveResult(fencedSecond, "tenant", "task-a", []byte("ended")), storagev1.ErrTaskJobRevoked,
			"an attempt that ended no longer authorizes writes")
		result, err := p.GetResult(ctx, "tenant", "task-a")
		require.NoError(t, err)
		require.Equal(t, "second", string(result))
	})

	t.Run("fenced writes racing cleanup leave nothing behind", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		fence, _, err := p.RegisterTaskDataFence(ctx, conditionalNamespace, "task-fenced")
		require.NoError(t, err)
		fenced := storagev1.WithTaskDataFence(ctx, fence)
		errs := runConcurrently(func(caller int) error {
			if caller == 0 {
				return p.DeleteResult(ctx, conditionalNamespace, "task-fenced")
			}
			return p.SaveResult(fenced, conditionalNamespace, "task-fenced", fmt.Appendf(nil, "write %d", caller))
		})
		require.NoError(t, errs[0])
		for caller, err := range errs[1:] {
			if err != nil {
				require.ErrorIs(t, err, storagev1.ErrTaskDataCleanupChanged, "caller %d", caller+1)
			}
		}
		_, err = p.GetResult(ctx, conditionalNamespace, "task-fenced")
		require.ErrorIs(t, err, storagev1.ErrNotFound,
			"each write either committed before cleanup, which removed it, or was refused")
	})
}

// runArtifactFence covers a fenced artifact write on a provider that serves
// both contracts; providers that serve only state.v1 skip it.
func runArtifactFence(t *testing.T, newProvider StateFactory) {
	p, ctx := newProvider(t), t.Context()
	artifacts, ok := p.(storagev1.ArtifactStore)
	if !ok {
		t.Skip("the provider does not also serve artifacts.v1")
	}
	owner := storagev1.ArtifactOwner{Namespace: conditionalNamespace, TaskName: "task-fenced"}
	fence, _, err := p.RegisterTaskDataFence(ctx, conditionalNamespace, "task-fenced")
	require.NoError(t, err)
	request := storagev1.SaveArtifactRequest{Owner: owner, Filename: "out.txt", ContentType: "text/plain", Data: []byte("artifact")}
	require.NoError(t, artifacts.SaveArtifact(storagev1.WithTaskDataFence(ctx, fence), request))
	require.NoError(t, artifacts.DeleteArtifacts(ctx, owner))
	require.ErrorIs(t, artifacts.SaveArtifact(storagev1.WithTaskDataFence(ctx, fence), request), storagev1.ErrTaskDataCleanupChanged)
	listed, err := artifacts.ListArtifacts(ctx, owner)
	require.NoError(t, err)
	require.Empty(t, listed, "a refused artifact write leaves nothing")
}

func runGroupedExecutionEvents(t *testing.T, newProvider StateFactory) {
	approval := func(eventType string) *storagev1.ExecutionEvent {
		event := taskEvent(eventsTenant, "task-grouped", eventType)
		event.Content = json.RawMessage(`{"approvalID":"approval-1"}`)
		return event
	}

	t.Run("a group appends together and replays by key", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		group := []storagev1.ExecutionEventAppend{
			{Event: approval(storagev1.ExecutionEventTypeApprovalCancelled), DedupeKey: "decision"},
			{Event: approval(storagev1.ExecutionEventTypeApprovalExecutionUpdated), DedupeKey: "outcome"},
		}
		first, err := p.AppendExecutionEventsIfAbsent(ctx, group)
		require.NoError(t, err)
		require.Len(t, first, 2)
		require.True(t, first[0].Appended)
		require.True(t, first[1].Appended)
		require.Less(t, first[0].Event.Seq, first[1].Event.Seq, "entries append in order")
		replay, err := p.AppendExecutionEventsIfAbsent(ctx, group)
		require.NoError(t, err)
		require.False(t, replay[0].Appended)
		require.False(t, replay[1].Appended)
		require.Equal(t, first[0].Event.ID, replay[0].Event.ID)
		require.Equal(t, first[1].Event.ID, replay[1].Event.ID)
		require.Len(t, listEvents(t, p, storagev1.ExecutionEventFilter{Namespace: eventsTenant, StreamID: "task-grouped"}), 2)
	})

	t.Run("a failing entry appends none of the group", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		appendEvent(t, p, approval(storagev1.ExecutionEventTypeApprovalApproved))
		_, err := p.AppendExecutionEventsIfAbsent(ctx, []storagev1.ExecutionEventAppend{
			{Event: approval(storagev1.ExecutionEventTypeApprovalExecutionUpdated), DedupeKey: "outcome"},
			{Event: approval(storagev1.ExecutionEventTypeApprovalDeclined), DedupeKey: "decision"},
		})
		require.ErrorIs(t, err, storagev1.ErrConflict, "a second terminal decision refuses the group")
		_, err = p.AppendExecutionEventsIfAbsent(ctx, []storagev1.ExecutionEventAppend{
			{Event: approval(storagev1.ExecutionEventTypeApprovalExecutionUpdated), DedupeKey: "outcome"},
			{Event: approval(storagev1.ExecutionEventTypeApprovalExecutionUpdated), DedupeKey: " "},
		})
		require.ErrorIs(t, err, storagev1.ErrValidation)
		require.Len(t, listEvents(t, p, storagev1.ExecutionEventFilter{Namespace: eventsTenant, StreamID: "task-grouped"}), 1,
			"no entry of a refused group is appended")
	})
}

func ingestionIdentity(task string) storagev1.ScanTaskIdentity {
	return storagev1.ScanTaskIdentity{Namespace: workflowNamespace, RepositoryScan: workflowScan, ScanRunID: "run-1",
		TaskName: task, TaskUID: task + "-uid", Stage: "review", SliceID: "slice-1"}
}

func ingestionFinding(fingerprint string) *storagev1.Finding {
	finding := workflowFinding(fingerprint, "high")
	finding.ID = fingerprint
	return finding
}

// ingestionCommit applies one review: it reads a finding, writes it as
// observed and counts it on the run.
func ingestionCommit(t *testing.T, p storagev1.StateProvider, task, fingerprint string, at time.Time) storagev1.ScanTaskIngestionCommit {
	t.Helper()
	run, err := p.GetScanRun(t.Context(), workflowNamespace, "run-1")
	require.NoError(t, err)
	updated := *run
	updated.AcceptedFindings++
	updated.ReviewedSliceCount++
	notFound := storagev1.ScanIngestionReadResult{NotFound: true}
	return storagev1.ScanTaskIngestionCommit{
		Ingestion:   storagev1.ScanTaskIngestion{ScanTaskIdentity: ingestionIdentity(task), FindingIDs: []string{fingerprint}, IngestedAt: at},
		ExpectedRun: *run, Run: updated, At: at,
		Steps: []storagev1.ScanIngestionStep{
			{GetFinding: &storagev1.FindingKey{Namespace: workflowNamespace, ID: fingerprint}, Expect: &notFound},
			{UpsertObservedFinding: ingestionFinding(fingerprint)},
		},
	}
}

func runScanIngestion(t *testing.T, newProvider StateFactory) {
	at := workflowAt(30)
	setup := func(t *testing.T) storagev1.StateProvider {
		p := newProvider(t)
		require.NoError(t, p.CreateScanRun(t.Context(), workflowScanRun("run-1", "running")))
		return p
	}

	t.Run("a preview runs the steps and keeps nothing", func(t *testing.T) {
		p, ctx := setup(t), t.Context()
		steps := []storagev1.ScanIngestionStep{
			{UpsertObservedFinding: ingestionFinding("finding-1")},
			{GetFinding: &storagev1.FindingKey{Namespace: workflowNamespace, ID: "finding-1"}},
		}
		preview := storagev1.ScanIngestionPreview{Namespace: workflowNamespace, RepositoryScan: workflowScan, At: at, Steps: steps}
		results, err := p.PreviewScanTaskIngestion(ctx, preview)
		require.NoError(t, err)
		require.Len(t, results, 2)
		require.NotNil(t, results[1].Finding, "a read sees the earlier write")
		require.Equal(t, "finding-1", results[1].Finding.ID)
		again, err := p.PreviewScanTaskIngestion(ctx, preview)
		require.NoError(t, err)
		require.Equal(t, results, again, "a preview with the same time is deterministic")
		_, err = p.GetFinding(ctx, workflowNamespace, "finding-1")
		require.ErrorIs(t, err, storagev1.ErrNotFound, "a preview changes nothing")

		// A commit that replays the preview's read sees the same result.
		run, err := p.GetScanRun(ctx, workflowNamespace, "run-1")
		require.NoError(t, err)
		steps[1].Expect = &results[1]
		applied, err := p.CommitScanTaskIngestion(ctx, storagev1.ScanTaskIngestionCommit{
			Ingestion:   storagev1.ScanTaskIngestion{ScanTaskIdentity: ingestionIdentity("review-a"), IngestedAt: at},
			ExpectedRun: *run, Run: *run, At: at, Steps: steps,
		})
		require.NoError(t, err)
		require.True(t, applied)
	})

	t.Run("a commit applies writes, run and receipt together once", func(t *testing.T) {
		p, ctx := setup(t), t.Context()
		commit := ingestionCommit(t, p, "review-a", "finding-1", at)
		commit.Steps = append(commit.Steps, storagev1.ScanIngestionStep{CreateDroppedFinding: &storagev1.DroppedFinding{
			ID: "drop-1", Namespace: workflowNamespace, RepositoryScan: workflowScan, ScanRunID: "run-1", TaskName: "review-a", Reason: "duplicate",
		}})
		applied, err := p.CommitScanTaskIngestion(ctx, commit)
		require.NoError(t, err)
		require.True(t, applied)
		finding, err := p.GetFinding(ctx, workflowNamespace, "finding-1")
		require.NoError(t, err)
		require.Equal(t, "Finding finding-1", finding.Title)
		run, err := p.GetScanRun(ctx, workflowNamespace, "run-1")
		require.NoError(t, err)
		require.Equal(t, 1, run.AcceptedFindings)
		receipt, err := p.GetScanTaskIngestion(ctx, ingestionIdentity("review-a"))
		require.NoError(t, err)
		require.Equal(t, []string{"finding-1"}, receipt.FindingIDs)
		requireSameInstant(t, at, receipt.IngestedAt, "ingestion time")
		dropped, _, err := p.ListDroppedFindings(ctx, storagev1.DroppedFindingFilter{Namespace: workflowNamespace, ScanRunID: "run-1"})
		require.NoError(t, err)
		require.Len(t, dropped, 1)

		applied, err = p.CommitScanTaskIngestion(ctx, commit)
		require.NoError(t, err)
		require.False(t, applied, "an ingested Task is not applied again")
		again, err := p.GetScanRun(ctx, workflowNamespace, "run-1")
		require.NoError(t, err)
		require.Equal(t, run, again)
	})

	t.Run("competing ingestions of one run apply one", func(t *testing.T) {
		p, ctx := setup(t), t.Context()
		commits := make([]storagev1.ScanTaskIngestionCommit, concurrentCallers)
		for caller := range commits {
			commits[caller] = ingestionCommit(t, p, fmt.Sprintf("review-%d", caller), fmt.Sprintf("finding-%d", caller), at)
		}
		errs := runConcurrently(func(caller int) error {
			applied, err := p.CommitScanTaskIngestion(ctx, commits[caller])
			if err == nil && !applied {
				return errors.New("ingestion was skipped")
			}
			return err
		})
		winner := requireOneWinner(t, errs, storagev1.ErrPreconditionFailed)
		run, err := p.GetScanRun(ctx, workflowNamespace, "run-1")
		require.NoError(t, err)
		require.Equal(t, 1, run.AcceptedFindings, "no counter update is lost or doubled")
		findings, _, err := p.ListFindings(ctx, storagev1.FindingFilter{Namespace: workflowNamespace, RepositoryScan: workflowScan, Limit: 50})
		require.NoError(t, err)
		require.Equal(t, []string{fmt.Sprintf("finding-%d", winner)}, workflowFindingIDs(findings))
		for caller := range commits {
			_, err := p.GetScanTaskIngestion(ctx, ingestionIdentity(fmt.Sprintf("review-%d", caller)))
			if caller == winner {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, storagev1.ErrNotFound, "a refused ingestion records no receipt")
			}
		}
	})

	t.Run("a changed read refuses the commit and changes nothing", func(t *testing.T) {
		p, ctx := setup(t), t.Context()
		commit := ingestionCommit(t, p, "review-a", "finding-1", at)
		concurrent := ingestionFinding("finding-1")
		concurrent.Title = "written by someone else"
		require.NoError(t, p.UpsertFinding(ctx, concurrent))
		applied, err := p.CommitScanTaskIngestion(ctx, commit)
		require.ErrorIs(t, err, storagev1.ErrPreconditionFailed)
		require.False(t, applied)
		finding, err := p.GetFinding(ctx, workflowNamespace, "finding-1")
		require.NoError(t, err)
		require.Equal(t, "written by someone else", finding.Title)
		run, err := p.GetScanRun(ctx, workflowNamespace, "run-1")
		require.NoError(t, err)
		require.Zero(t, run.AcceptedFindings)
		_, err = p.GetScanTaskIngestion(ctx, ingestionIdentity("review-a"))
		require.ErrorIs(t, err, storagev1.ErrNotFound)
	})

	t.Run("a failing write rolls the ingestion back", func(t *testing.T) {
		p, ctx := setup(t), t.Context()
		commit := ingestionCommit(t, p, "review-a", "finding-1", at)
		commit.Steps = append(commit.Steps, storagev1.ScanIngestionStep{UpdateReviewSliceStatus: &storagev1.ReviewSliceStatusUpdate{
			Namespace: workflowNamespace, RepositoryScan: workflowScan, ID: "missing-slice", LastScanRunID: "run-1", Status: "reviewed",
		}})
		applied, err := p.CommitScanTaskIngestion(ctx, commit)
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		require.False(t, applied)
		_, err = p.GetFinding(ctx, workflowNamespace, "finding-1")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		_, err = p.GetScanTaskIngestion(ctx, ingestionIdentity("review-a"))
		require.ErrorIs(t, err, storagev1.ErrNotFound)
	})

	t.Run("terminal and cancelled runs skip ingestion", func(t *testing.T) {
		p, ctx := setup(t), t.Context()
		commit := ingestionCommit(t, p, "review-a", "finding-1", at)
		run, err := p.GetScanRun(ctx, workflowNamespace, "run-1")
		require.NoError(t, err)
		require.NoError(t, p.RequestScanRunCancellation(ctx, run, "operator cancelled"))
		applied, err := p.CommitScanTaskIngestion(ctx, commit)
		require.NoError(t, err)
		require.False(t, applied)
		_, err = p.GetFinding(ctx, workflowNamespace, "finding-1")
		require.ErrorIs(t, err, storagev1.ErrNotFound)

		other := commit
		other.Ingestion.RepositoryScan = "another-scan"
		_, err = p.CommitScanTaskIngestion(ctx, other)
		require.ErrorIs(t, err, storagev1.ErrValidation, "a commit whose run is not the ingestion's is refused")
	})

	t.Run("steps stay inside the ingestion's scope", func(t *testing.T) {
		p, ctx := setup(t), t.Context()
		outside := storagev1.ScanIngestionReadResult{NotFound: true}
		for name, step := range map[string]storagev1.ScanIngestionStep{
			"namespace":       {GetFinding: &storagev1.FindingKey{Namespace: workflowOtherNamespace, ID: "x"}, Expect: &outside},
			"repository scan": {ListFindings: &storagev1.FindingFilter{Namespace: workflowNamespace, RepositoryScan: "another-scan"}, Expect: &outside},
			"no operation":    {},
			"two operations": {GetFinding: &storagev1.FindingKey{Namespace: workflowNamespace, ID: "x"},
				GetScanRun: &storagev1.ScanRunKey{Namespace: workflowNamespace, ID: "run-1"}, Expect: &outside},
			"unobserved read": {GetFinding: &storagev1.FindingKey{Namespace: workflowNamespace, ID: "x"}},
		} {
			commit := ingestionCommit(t, p, "review-a", "finding-1", at)
			commit.Steps = append(commit.Steps, step)
			_, err := p.CommitScanTaskIngestion(ctx, commit)
			require.ErrorIs(t, err, storagev1.ErrValidation, name)
		}
		_, err := p.PreviewScanTaskIngestion(ctx, storagev1.ScanIngestionPreview{Namespace: workflowNamespace, RepositoryScan: workflowScan, At: at,
			Steps: []storagev1.ScanIngestionStep{{GetFinding: &storagev1.FindingKey{Namespace: workflowOtherNamespace, ID: "x"}}}})
		require.ErrorIs(t, err, storagev1.ErrValidation)
		_, err = p.GetScanTaskIngestion(ctx, ingestionIdentity("review-a"))
		require.ErrorIs(t, err, storagev1.ErrNotFound)
	})
}
