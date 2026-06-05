/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/sozercan/orka/internal/cli/client"
)

const filteredTaskListPageSize = 500

func listFilteredTasks(
	ctx context.Context,
	c *client.Client,
	namespace string,
	limit int,
	match func(client.TaskSummary) bool,
) ([]client.TaskSummary, bool, error) {
	if match == nil {
		match = func(client.TaskSummary) bool { return true }
	}

	var tasks []client.TaskSummary
	continueToken := ""
	for {
		page, err := c.ListTasksPage(ctx, client.ListTasksOptions{
			Namespace: namespace,
			Limit:     filteredTaskListPageSize,
			Continue:  continueToken,
		})
		if err != nil {
			return nil, false, err
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

		if page.Continue == "" {
			return tasks, false, nil
		}
		if limit > 0 && len(tasks) >= limit {
			return tasks, true, nil
		}
		continueToken = page.Continue
	}
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
