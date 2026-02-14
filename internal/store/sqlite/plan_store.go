/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sozercan/orka/internal/store"
)

// SavePlan upserts an autonomous plan state.
func (s *Store) SavePlan(ctx context.Context, namespace, taskName string, plan *store.PlanState) error {
	now := time.Now()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO plan_states (namespace, task_name, iteration, summary, progress_pct, goal_complete, plan_document, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (namespace, task_name) DO UPDATE SET
		   iteration = excluded.iteration,
		   summary = excluded.summary,
		   progress_pct = excluded.progress_pct,
		   goal_complete = excluded.goal_complete,
		   plan_document = excluded.plan_document,
		   updated_at = excluded.updated_at`,
		namespace, taskName, plan.Iteration, plan.Summary, plan.ProgressPct, plan.GoalComplete, plan.PlanDocument, now, now,
	)
	return err
}

// GetPlan retrieves the autonomous plan state for a task.
func (s *Store) GetPlan(ctx context.Context, namespace, taskName string) (*store.PlanState, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT namespace, task_name, iteration, summary, progress_pct, goal_complete, plan_document, created_at, updated_at
		 FROM plan_states WHERE namespace = ? AND task_name = ?`,
		namespace, taskName,
	)

	var p store.PlanState
	err := row.Scan(&p.Namespace, &p.TaskName, &p.Iteration, &p.Summary, &p.ProgressPct, &p.GoalComplete, &p.PlanDocument, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// DeletePlan removes the autonomous plan state for a task.
func (s *Store) DeletePlan(ctx context.Context, namespace, taskName string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM plan_states WHERE namespace = ? AND task_name = ?`,
		namespace, taskName,
	)
	return err
}
