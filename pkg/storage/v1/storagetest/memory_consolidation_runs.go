package storagetest

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// The cases in this file hold providers to memory consolidation runs: one run
// per Task, a cursor only completed runs move, and a completion that creates
// its proposals atomically.

func consolidationRun(taskUID string, start, end time.Time) *storagev1.MemoryConsolidationRun {
	return &storagev1.MemoryConsolidationRun{Namespace: conditionalNamespace, TaskUID: taskUID, TaskName: "run-" + taskUID,
		Mode: "Propose", Status: storagev1.MemoryConsolidationRunning, WindowStart: start, WindowEnd: end, CreatedAt: end}
}

func runProposal(id string) storagev1.MemoryProposal {
	return storagev1.MemoryProposal{ID: id, Namespace: conditionalNamespace, Type: storagev1.MemoryProposalTypeMemory,
		Title: "Learned " + id, Content: "content of " + id, TaskName: "run",
		Sources:   []storagev1.MemoryProposalSource{{SessionName: "session-a"}},
		CreatedAt: time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)}
}

func runMemoryConsolidationRuns(t *testing.T, newProvider StateFactory) {
	at := time.Date(2026, 6, 11, 10, 0, 0, 0, time.UTC)

	t.Run("a Task begins one run and keeps its first window", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		for name, invalid := range map[string]*storagev1.MemoryConsolidationRun{
			"no namespace":        {TaskUID: "uid", TaskName: "run", Status: storagev1.MemoryConsolidationRunning, WindowStart: at, WindowEnd: at},
			"no Task UID":         {Namespace: conditionalNamespace, TaskName: "run", Status: storagev1.MemoryConsolidationRunning, WindowStart: at, WindowEnd: at},
			"no mode":             {Namespace: conditionalNamespace, TaskUID: "uid", TaskName: "run", Status: storagev1.MemoryConsolidationRunning, WindowStart: at, WindowEnd: at, CreatedAt: at},
			"no creation time":    {Namespace: conditionalNamespace, TaskUID: "uid", TaskName: "run", Mode: "DryRun", Status: storagev1.MemoryConsolidationRunning, WindowStart: at, WindowEnd: at},
			"not running":         {Namespace: conditionalNamespace, TaskUID: "uid", TaskName: "run", Status: storagev1.MemoryConsolidationCompleted, WindowStart: at, WindowEnd: at},
			"a backwards window":  {Namespace: conditionalNamespace, TaskUID: "uid", TaskName: "run", Status: storagev1.MemoryConsolidationRunning, WindowStart: at, WindowEnd: at.Add(-time.Hour)},
			"no window start":     {Namespace: conditionalNamespace, TaskUID: "uid", TaskName: "run", Status: storagev1.MemoryConsolidationRunning, WindowEnd: at},
			"no run at all (nil)": nil,
		} {
			require.ErrorIs(t, p.BeginMemoryConsolidationRun(ctx, invalid), storagev1.ErrValidation, name)
		}
		first := consolidationRun("uid-1", at.Add(-time.Hour), at)
		require.NoError(t, p.BeginMemoryConsolidationRun(ctx, first))
		requireSameInstant(t, at, first.WindowEnd, "the stored window is written back")

		retry := consolidationRun("uid-1", at, at.Add(time.Hour))
		require.NoError(t, p.BeginMemoryConsolidationRun(ctx, retry))
		requireSameInstant(t, at.Add(-time.Hour), retry.WindowStart, "a second begin keeps the first window")
		requireSameInstant(t, at, retry.WindowEnd, "a second begin keeps the first window")
		stored, err := p.GetMemoryConsolidationRun(ctx, conditionalNamespace, "uid-1")
		require.NoError(t, err)
		require.Equal(t, storagev1.MemoryConsolidationRunning, stored.Status)
		require.Equal(t, "run-uid-1", stored.TaskName)
		require.Nil(t, stored.CompletedAt)
		_, err = p.GetMemoryConsolidationRun(ctx, "other-namespace", "uid-1")
		require.ErrorIs(t, err, storagev1.ErrNotFound, "a run is read only in its namespace")
	})

	t.Run("each mode's cursor moves only with its completed runs", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		cursor, err := p.GetMemoryConsolidationCursor(ctx, conditionalNamespace, "Propose")
		require.NoError(t, err)
		require.Nil(t, cursor, "a namespace starts with no cursor")
		_, err = p.GetMemoryConsolidationCursor(ctx, "", "Propose")
		require.ErrorIs(t, err, storagev1.ErrValidation)
		_, err = p.GetMemoryConsolidationCursor(ctx, conditionalNamespace, "")
		require.ErrorIs(t, err, storagev1.ErrValidation)

		finish := func(run *storagev1.MemoryConsolidationRun, status string) {
			t.Helper()
			require.NoError(t, p.BeginMemoryConsolidationRun(ctx, run))
			_, err := p.CompleteMemoryConsolidationRun(ctx, storagev1.MemoryConsolidationCompletion{Namespace: conditionalNamespace,
				TaskUID: run.TaskUID, Status: status, Report: []byte(`{"status":"` + status + `"}`), CompletedAt: run.WindowEnd})
			require.NoError(t, err)
		}
		skipped := consolidationRun("uid-skipped", at, at.Add(time.Hour))
		finish(skipped, storagev1.MemoryConsolidationSkipped)
		cursor, err = p.GetMemoryConsolidationCursor(ctx, conditionalNamespace, "Propose")
		require.NoError(t, err)
		require.NotNil(t, cursor)
		requireSameInstant(t, at, *cursor, "before any completed run, the first run's start holds")
		later := consolidationRun("uid-later-skip", at.Add(2*time.Hour), at.Add(3*time.Hour))
		finish(later, storagev1.MemoryConsolidationSkipped)
		cursor, err = p.GetMemoryConsolidationCursor(ctx, conditionalNamespace, "Propose")
		require.NoError(t, err)
		requireSameInstant(t, at, *cursor, "a later skipped run keeps the first start")

		completed := consolidationRun("uid-completed", at, at.Add(4*time.Hour))
		finish(completed, storagev1.MemoryConsolidationCompleted)
		dryRun := consolidationRun("uid-dry", at, at.Add(9*time.Hour))
		dryRun.Mode = "DryRun"
		finish(dryRun, storagev1.MemoryConsolidationCompleted)
		cursor, err = p.GetMemoryConsolidationCursor(ctx, conditionalNamespace, "Propose")
		require.NoError(t, err)
		requireSameInstant(t, at.Add(4*time.Hour), *cursor, "a completed run moves its own mode's cursor only")
		dry, err := p.GetMemoryConsolidationCursor(ctx, conditionalNamespace, "DryRun")
		require.NoError(t, err)
		requireSameInstant(t, at.Add(9*time.Hour), *dry, "a dry run's cursor")
		other, err := p.GetMemoryConsolidationCursor(ctx, "other-namespace", "Propose")
		require.NoError(t, err)
		require.Nil(t, other, "the cursor is per namespace")
	})

	t.Run("a run cannot begin from a cursor another run moved", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		first := consolidationRun("uid-first", at, at.Add(time.Hour))
		require.NoError(t, p.BeginMemoryConsolidationRun(ctx, first))
		_, err := p.CompleteMemoryConsolidationRun(ctx, storagev1.MemoryConsolidationCompletion{Namespace: conditionalNamespace,
			TaskUID: "uid-first", Status: storagev1.MemoryConsolidationCompleted, Report: []byte(`{}`), CompletedAt: at.Add(time.Hour)})
		require.NoError(t, err)

		stale := consolidationRun("uid-stale", at.Add(30*time.Minute), at.Add(2*time.Hour))
		err = p.BeginMemoryConsolidationRun(ctx, stale)
		require.ErrorIs(t, err, storagev1.ErrPreconditionFailed, "a window chosen before the first run completed")
		require.ErrorIs(t, err, storagev1.ErrConflict)
		_, err = p.GetMemoryConsolidationRun(ctx, conditionalNamespace, "uid-stale")
		require.ErrorIs(t, err, storagev1.ErrNotFound, "a refused begin records nothing")

		dryRun := consolidationRun("uid-dry", at.Add(30*time.Minute), at.Add(2*time.Hour))
		dryRun.Mode = "DryRun"
		require.NoError(t, p.BeginMemoryConsolidationRun(ctx, dryRun), "another mode's cursor is its own")
		_, err = p.CompleteMemoryConsolidationRun(ctx, storagev1.MemoryConsolidationCompletion{Namespace: conditionalNamespace,
			TaskUID: "uid-dry", Status: storagev1.MemoryConsolidationSkipped, Report: []byte(`{}`), CompletedAt: at.Add(2 * time.Hour)})
		require.NoError(t, err)
		require.NoError(t, p.BeginMemoryConsolidationRun(ctx, consolidationRun("uid-next", at.Add(time.Hour), at.Add(3*time.Hour))),
			"a window that starts at the cursor begins")
	})

	t.Run("releasing a run clears it and keeps every cursor", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		require.ErrorIs(t, p.ReleaseMemoryConsolidationRun(ctx, "", "uid"), storagev1.ErrValidation)
		require.ErrorIs(t, p.ReleaseMemoryConsolidationRun(ctx, conditionalNamespace, " "), storagev1.ErrValidation)
		require.NoError(t, p.ReleaseMemoryConsolidationRun(ctx, conditionalNamespace, "missing"), "a missing run is left alone")
		cursor := func() time.Time {
			t.Helper()
			at, err := p.GetMemoryConsolidationCursor(ctx, conditionalNamespace, "Propose")
			require.NoError(t, err)
			require.NotNil(t, at)
			return *at
		}
		complete := func(uid string, start, end time.Time) {
			t.Helper()
			require.NoError(t, p.BeginMemoryConsolidationRun(ctx, consolidationRun(uid, start, end)))
			_, err := p.CompleteMemoryConsolidationRun(ctx, storagev1.MemoryConsolidationCompletion{Namespace: conditionalNamespace,
				TaskUID: uid, Status: storagev1.MemoryConsolidationCompleted, Report: []byte(`{"run":"` + uid + `"}`), CompletedAt: end})
			require.NoError(t, err)
		}
		gone := func(uid string) bool {
			t.Helper()
			_, err := p.GetMemoryConsolidationRun(ctx, conditionalNamespace, uid)
			if errors.Is(err, storagev1.ErrNotFound) {
				return true
			}
			require.NoError(t, err)
			return false
		}

		dead := consolidationRun("uid-dead", at, at.Add(time.Hour))
		dead.Input = []byte(`{"transcript":"excerpt"}`)
		require.NoError(t, p.BeginMemoryConsolidationRun(ctx, dead))
		require.NoError(t, p.ReleaseMemoryConsolidationRun(ctx, conditionalNamespace, "uid-dead"))
		released, err := p.GetMemoryConsolidationRun(ctx, conditionalNamespace, "uid-dead")
		require.NoError(t, err, "the mode's only run holds its cursor, so it is kept")
		require.Equal(t, storagev1.MemoryConsolidationAbandoned, released.Status)
		require.Empty(t, released.Input, "a released run keeps no transcripts")
		requireSameInstant(t, at, cursor(), "the released run's start still holds the cursor")

		complete("uid-first", at, at.Add(2*time.Hour))
		complete("uid-kept", at.Add(2*time.Hour), at.Add(3*time.Hour))
		require.NoError(t, p.ReleaseMemoryConsolidationRun(ctx, conditionalNamespace, "uid-first"))
		require.True(t, gone("uid-first"), "a released completed run a later one supersedes is removed")
		require.NoError(t, p.ReleaseMemoryConsolidationRun(ctx, conditionalNamespace, "uid-kept"))
		latest, err := p.GetMemoryConsolidationRun(ctx, conditionalNamespace, "uid-kept")
		require.NoError(t, err, "the latest completed run holds the cursor, so it is kept")
		require.Empty(t, latest.Report, "a released run keeps no report")
		requireSameInstant(t, at.Add(3*time.Hour), cursor(), "the latest completed run's end")

		complete("uid-unreleased", at.Add(3*time.Hour), at.Add(4*time.Hour))
		complete("uid-last", at.Add(4*time.Hour), at.Add(5*time.Hour))
		require.NoError(t, p.ReleaseMemoryConsolidationRun(ctx, conditionalNamespace, "uid-last"))
		require.True(t, gone("uid-kept"), "the run kept as the cursor goes once a later run supersedes it")
		unreleased, err := p.GetMemoryConsolidationRun(ctx, conditionalNamespace, "uid-unreleased")
		require.NoError(t, err, "a run whose Task has not released it is never removed")
		require.JSONEq(t, `{"run":"uid-unreleased"}`, string(unreleased.Report))
		requireSameInstant(t, at.Add(5*time.Hour), cursor(), "releasing never moves the cursor")

		other, err := p.GetMemoryConsolidationCursor(ctx, conditionalNamespace, "DryRun")
		require.NoError(t, err)
		require.Nil(t, other, "another mode is untouched")
	})

	t.Run("a running run keeps another from beginning until its lease ends", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		first := consolidationRun("uid-first", at, at.Add(time.Hour))
		first.Input = []byte(`{"sessions":[{"name":"s"}]}`)
		require.NoError(t, p.BeginMemoryConsolidationRun(ctx, first))
		second := consolidationRun("uid-second", at, at.Add(2*time.Hour))
		second.Mode = "DryRun"
		require.ErrorIs(t, p.BeginMemoryConsolidationRun(ctx, second), storagev1.ErrConflict,
			"two runs, of any mode, never propose over the same memories at once")
		_, err := p.GetMemoryConsolidationRun(ctx, conditionalNamespace, "uid-second")
		require.ErrorIs(t, err, storagev1.ErrNotFound)
		retry := consolidationRun("uid-first", at, at.Add(3*time.Hour))
		require.NoError(t, p.BeginMemoryConsolidationRun(ctx, retry), "the running run's own Task may begin again")
		elsewhere := consolidationRun("uid-elsewhere", at, at.Add(time.Hour))
		elsewhere.Namespace = "other-namespace"
		require.NoError(t, p.BeginMemoryConsolidationRun(ctx, elsewhere), "the lease is per namespace")

		late := consolidationRun("uid-late", at, at.Add(time.Hour).Add(storagev1.MemoryConsolidationRunLease))
		late.CreatedAt = first.CreatedAt.Add(storagev1.MemoryConsolidationRunLease + time.Minute)
		require.NoError(t, p.BeginMemoryConsolidationRun(ctx, late), "a run whose Task died stops blocking after its lease")
		superseded, err := p.GetMemoryConsolidationRun(ctx, conditionalNamespace, "uid-first")
		require.NoError(t, err)
		require.Equal(t, storagev1.MemoryConsolidationAbandoned, superseded.Status, "taking over abandons the expired run")
		require.Empty(t, superseded.Input, "an abandoned run keeps no transcript excerpts")
		resumed := consolidationRun("uid-first", at, at.Add(time.Hour))
		require.NoError(t, p.BeginMemoryConsolidationRun(ctx, resumed))
		require.Equal(t, storagev1.MemoryConsolidationAbandoned, resumed.Status, "an abandoned run cannot begin again")
		completed, err := p.CompleteMemoryConsolidationRun(ctx, storagev1.MemoryConsolidationCompletion{Namespace: conditionalNamespace,
			TaskUID: "uid-first", Status: storagev1.MemoryConsolidationCompleted, Report: []byte(`{}`), CompletedAt: at,
			Proposals: []storagev1.MemoryProposal{runProposal("mprop-from-abandoned")}})
		require.NoError(t, err)
		require.Equal(t, storagev1.MemoryConsolidationAbandoned, completed.Status, "an abandoned run cannot complete")
		_, err = p.GetMemoryProposal(ctx, conditionalNamespace, "mprop-from-abandoned")
		require.ErrorIs(t, err, storagev1.ErrNotFound, "nor create proposals")
	})

	t.Run("a run keeps the input it began with", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		run := consolidationRun("uid-input", at, at.Add(time.Hour))
		run.Input = []byte(`{"memories":[{"id":"mem-a","generation":2}]}`)
		require.NoError(t, p.BeginMemoryConsolidationRun(ctx, run))
		retry := consolidationRun("uid-input", at, at.Add(time.Hour))
		retry.Input = []byte(`{"memories":[]}`)
		require.NoError(t, p.BeginMemoryConsolidationRun(ctx, retry))
		require.JSONEq(t, string(run.Input), string(retry.Input), "a retried worker reads the first input")
		completed, err := p.CompleteMemoryConsolidationRun(ctx, storagev1.MemoryConsolidationCompletion{Namespace: conditionalNamespace,
			TaskUID: "uid-input", Status: storagev1.MemoryConsolidationCompleted, Report: []byte(`{}`), CompletedAt: at.Add(time.Hour)})
		require.NoError(t, err)
		require.Empty(t, completed.Input, "a finished run keeps no transcript excerpts")
		oversized := consolidationRun("uid-oversized", at, at.Add(time.Hour))
		oversized.Input = make([]byte, storagev1.MaxMemoryConsolidationInputBytes+1)
		require.ErrorIs(t, p.BeginMemoryConsolidationRun(ctx, oversized), storagev1.ErrValidation)
	})

	t.Run("changed Sessions are listed by when they changed", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		for _, session := range []struct {
			name, kind string
			created    time.Duration
			updated    time.Duration
		}{
			{"old-chat-active-now", storagev1.SessionTypeChat, -48 * time.Hour, 3 * time.Hour},
			{"task-recent", "task", 2 * time.Hour, 2 * time.Hour},
			{"task-also", "task", time.Hour, 2 * time.Hour},
			{"gateway-chat", storagev1.SessionTypeGateway, time.Hour, time.Hour},
			{"before-window", "task", -2 * time.Hour, -time.Hour},
			{"after-window", "task", time.Hour, 6 * time.Hour},
		} {
			require.NoError(t, p.CreateSession(ctx, &storagev1.SessionRecord{Namespace: conditionalNamespace, Name: session.name,
				SessionType: session.kind, CreatedAt: at.Add(session.created), UpdatedAt: at.Add(session.updated)}))
		}
		require.NoError(t, p.CreateSession(ctx, &storagev1.SessionRecord{Namespace: "other-namespace", Name: "foreign",
			SessionType: "task", CreatedAt: at, UpdatedAt: at.Add(time.Hour)}))

		sessions, total, err := p.ListMemoryConsolidationSessions(ctx, conditionalNamespace, at, at.Add(5*time.Hour), 2)
		require.NoError(t, err)
		require.Equal(t, 3, total, "chat and task Sessions changed in the window, never gateway ones or other namespaces")
		names := make([]string, 0, len(sessions))
		for _, session := range sessions {
			names = append(names, session.Name)
		}
		require.Equal(t, []string{"old-chat-active-now", "task-also"}, names,
			"most recently changed first, then by name, however long ago a Session was created")
		_, _, err = p.ListMemoryConsolidationSessions(ctx, conditionalNamespace, at, at.Add(time.Hour), 0)
		require.ErrorIs(t, err, storagev1.ErrValidation)
		_, _, err = p.ListMemoryConsolidationSessions(ctx, "", at, at.Add(time.Hour), 1)
		require.ErrorIs(t, err, storagev1.ErrValidation)
	})

	t.Run("completion records the report and creates its proposals once", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		require.NoError(t, p.BeginMemoryConsolidationRun(ctx, consolidationRun("uid-1", at, at.Add(time.Hour))))
		completion := storagev1.MemoryConsolidationCompletion{Namespace: conditionalNamespace, TaskUID: "uid-1",
			Status: storagev1.MemoryConsolidationCompleted, Report: []byte(`{"status":"completed"}`), CompletedAt: at.Add(2 * time.Hour),
			Proposals: []storagev1.MemoryProposal{runProposal("mprop-run-1"), runProposal("mprop-run-2")}}
		completed, err := p.CompleteMemoryConsolidationRun(ctx, completion)
		require.NoError(t, err)
		require.Equal(t, storagev1.MemoryConsolidationCompleted, completed.Status)
		require.JSONEq(t, `{"status":"completed"}`, string(completed.Report))
		require.NotNil(t, completed.CompletedAt)
		requireSameInstant(t, at.Add(2*time.Hour), *completed.CompletedAt, "completion time")
		for _, id := range []string{"mprop-run-1", "mprop-run-2"} {
			proposal, err := p.GetMemoryProposal(ctx, conditionalNamespace, id)
			require.NoError(t, err)
			require.Equal(t, storagev1.MemoryProposalStatusPending, proposal.Status)
			require.Equal(t, []storagev1.MemoryProposalSource{{SessionName: "session-a"}}, proposal.Sources)
		}

		again := completion
		again.Report = []byte(`{"status":"different"}`)
		again.Proposals = []storagev1.MemoryProposal{runProposal("mprop-run-3")}
		replayed, err := p.CompleteMemoryConsolidationRun(ctx, again)
		require.NoError(t, err)
		require.JSONEq(t, `{"status":"completed"}`, string(replayed.Report), "a finished run is returned unchanged")
		_, err = p.GetMemoryProposal(ctx, conditionalNamespace, "mprop-run-3")
		require.ErrorIs(t, err, storagev1.ErrNotFound, "a finished run creates nothing more")
		_, err = p.CompleteMemoryConsolidationRun(ctx, storagev1.MemoryConsolidationCompletion{Namespace: conditionalNamespace,
			TaskUID: "missing", Status: storagev1.MemoryConsolidationCompleted, Report: []byte(`{}`), CompletedAt: at})
		require.ErrorIs(t, err, storagev1.ErrNotFound)
	})

	t.Run("an invalid completion changes nothing", func(t *testing.T) {
		p, ctx := newProvider(t), t.Context()
		require.NoError(t, p.CreateMemoryProposal(ctx, &storagev1.MemoryProposal{ID: "mprop-taken", Namespace: conditionalNamespace,
			Title: "Taken", CreatedAt: at, UpdatedAt: at}))
		require.NoError(t, p.BeginMemoryConsolidationRun(ctx, consolidationRun("uid-1", at, at.Add(time.Hour))))
		valid := storagev1.MemoryConsolidationCompletion{Namespace: conditionalNamespace, TaskUID: "uid-1",
			Status: storagev1.MemoryConsolidationCompleted, Report: []byte(`{}`), CompletedAt: at}
		invalidProposal := runProposal("mprop-invalid")
		invalidProposal.Type = storagev1.MemoryProposalTypeMerge
		foreign := runProposal("mprop-foreign")
		foreign.Namespace = "other-namespace"
		tooMany := make([]storagev1.MemoryProposal, storagev1.MaxMemoryConsolidationProposals+1)
		for i := range tooMany {
			tooMany[i] = runProposal("mprop-many-" + string(rune('a'+i)))
		}
		for name, change := range map[string]func(*storagev1.MemoryConsolidationCompletion){
			"a running status": func(c *storagev1.MemoryConsolidationCompletion) { c.Status = storagev1.MemoryConsolidationRunning },
			"no report":        func(c *storagev1.MemoryConsolidationCompletion) { c.Report = nil },
			"an oversized report": func(c *storagev1.MemoryConsolidationCompletion) {
				c.Report = make([]byte, storagev1.MaxMemoryConsolidationReportBytes+1)
			},
			"no completion time": func(c *storagev1.MemoryConsolidationCompletion) { c.CompletedAt = time.Time{} },
			"too many proposals": func(c *storagev1.MemoryConsolidationCompletion) { c.Proposals = tooMany },
			"a skip with proposals": func(c *storagev1.MemoryConsolidationCompletion) {
				c.Status = storagev1.MemoryConsolidationSkipped
				c.Proposals = []storagev1.MemoryProposal{runProposal("mprop-x")}
			},
			"an invalid proposal": func(c *storagev1.MemoryConsolidationCompletion) {
				c.Proposals = []storagev1.MemoryProposal{runProposal("mprop-ok"), invalidProposal}
			},
			"a foreign proposal": func(c *storagev1.MemoryConsolidationCompletion) { c.Proposals = []storagev1.MemoryProposal{foreign} },
			"a reused proposal ID": func(c *storagev1.MemoryConsolidationCompletion) {
				c.Proposals = []storagev1.MemoryProposal{runProposal("mprop-ok"), runProposal("mprop-taken")}
			},
		} {
			completion := valid
			change(&completion)
			_, err := p.CompleteMemoryConsolidationRun(ctx, completion)
			require.Error(t, err, name)
			run, err := p.GetMemoryConsolidationRun(ctx, conditionalNamespace, "uid-1")
			require.NoError(t, err)
			require.Equal(t, storagev1.MemoryConsolidationRunning, run.Status, "%s: the run stays running", name)
			_, err = p.GetMemoryProposal(ctx, conditionalNamespace, "mprop-ok")
			require.ErrorIs(t, err, storagev1.ErrNotFound, "%s: no proposal is created", name)
		}
	})
}
