/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/orka-agents/orka/internal/cli/client"
)

const (
	filteredTaskListPageSize = 500
	maxFilteredTaskListPages = 1000
)

func listFilteredTasks(
	ctx context.Context,
	c *client.Client,
	namespace string,
	limit int,
	match func(client.TaskSummary) bool,
) ([]client.TaskSummary, bool, error) {
	return listFilteredTasksWithPagination(ctx, c, namespace, limit, match, true)
}

func listFilteredTasksWithPagination(
	ctx context.Context,
	c *client.Client,
	namespace string,
	limit int,
	match func(client.TaskSummary) bool,
	usePagination bool,
) ([]client.TaskSummary, bool, error) {
	if match == nil {
		match = func(client.TaskSummary) bool { return true }
	}

	var tasks []client.TaskSummary
	cursor := ""
	seenContinuations := make(map[string]struct{})
	for pageNumber := 1; pageNumber <= maxFilteredTaskListPages; pageNumber++ {
		if err := ctx.Err(); err != nil {
			return tasks, false, fmt.Errorf(
				"listing tasks stopped after %d matching items before page %d: %w",
				len(tasks),
				pageNumber,
				err,
			)
		}

		opts := client.ListTasksOptions{Namespace: namespace}
		if usePagination {
			opts.Limit = filteredTaskListPageSize
			opts.Continue = cursor
		} else {
			opts.All = true
		}
		page, err := c.ListTasksPage(ctx, opts)
		if err != nil {
			if usePagination && ctx.Err() == nil && isCachePaginationUnsupported(err) {
				return listFilteredTasksWithPagination(ctx, c, namespace, limit, match, false)
			}
			return tasks, false, fmt.Errorf(
				"listing tasks stopped after %d matching items on page %d: %w",
				len(tasks),
				pageNumber,
				err,
			)
		}

		for _, task := range page.Items {
			if !match(task) {
				continue
			}
			if limit > 0 && len(tasks) >= limit {
				return tasks, true, nil
			}
			tasks = append(tasks, task)
		}

		if !usePagination || page.Continue == "" {
			if page.RemainingItemCount != nil && *page.RemainingItemCount > 0 {
				return tasks, false, fmt.Errorf(
					"listing tasks stopped after %d matching items: pagination ended with %d remaining items but no continuation",
					len(tasks),
					*page.RemainingItemCount,
				)
			}
			return tasks, false, nil
		}
		if page.Continue == cursor {
			return tasks, false, fmt.Errorf(
				"listing tasks stopped after %d matching items: continuation did not advance",
				len(tasks),
			)
		}
		if _, seen := seenContinuations[page.Continue]; seen {
			return tasks, false, fmt.Errorf(
				"listing tasks stopped after %d matching items: continuation cycle detected",
				len(tasks),
			)
		}
		if limit > 0 && len(tasks) >= limit {
			return tasks, true, nil
		}
		if pageNumber == maxFilteredTaskListPages {
			return tasks, false, fmt.Errorf(
				"listing tasks stopped after %d matching items: pagination page limit (%d) reached",
				len(tasks),
				maxFilteredTaskListPages,
			)
		}
		seenContinuations[page.Continue] = struct{}{}
		cursor = page.Continue
	}

	return tasks, false, fmt.Errorf("listing tasks stopped: pagination terminated unexpectedly")
}

func isCachePaginationUnsupported(err error) bool {
	return err != nil && strings.Contains(err.Error(), "list option is not supported by the cache")
}

func warnFilteredTaskOutputLimited(limit int) {
	if limit <= 0 {
		return
	}
	_, _ = fmt.Fprintf(
		os.Stderr,
		"Warning: output limited to %d matching tasks; additional matches may exist. "+
			"Increase --limit to inspect more.\n",
		limit,
	)
}
