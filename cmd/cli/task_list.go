/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/orka-agents/orka/internal/cli/client"
)

// taskListOptions holds the flags of `orka task list`.
type taskListOptions struct {
	status        string
	transactionID string
	selector      string
	since         string
	limit         int
	continueToken string
	watch         bool
	interval      time.Duration
}

func newTaskListCmd() *cobra.Command {
	var opts taskListOptions
	cmd := &cobra.Command{
		Use:     cliListUse,
		Aliases: []string{"ls"},
		Short:   "List tasks",
		Long: `List tasks in the namespace, oldest first.

Filter with -l/--selector (kubectl label selector syntax, applied by the
server) and --since (a duration such as 10m or 2h, or an RFC 3339 timestamp,
applied by the client). Orka labels the Tasks it creates for you:

  orka.ai/source=anthropic-proxy        Tasks from the Anthropic-compatible API
  orka.ai/security-target=<repository>  Tasks from a security scan
  gateway.orka.ai/gateway=<gateway>     Tasks from a gateway message

With --watch the table is reprinted whenever a Task appears or changes phase.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			since, err := parseSince(opts.since, time.Now())
			if err != nil {
				return err
			}
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			c := newClientFromCmd(cmd)
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if !opts.watch {
				tasks, err := fetchTaskList(ctx, c, opts, since)
				if err != nil {
					return err
				}
				if format != outputTable {
					return printStructured(cmd, tasks)
				}
				fmt.Fprint(cmd.OutOrStdout(), renderTaskListTable(tasks)) //nolint:errcheck
				return nil
			}
			return watchLoop(ctx, cmd.OutOrStdout(), opts.interval, format, func(ctx context.Context) (watchFrame, error) {
				tasks, err := fetchTaskList(ctx, c, opts, since)
				if err != nil {
					return watchFrame{}, err
				}
				frame := watchFrame{Key: taskListStateKey(tasks)}
				if format != outputTable {
					var buf bytes.Buffer
					if err := printStructuredTo(&buf, format, tasks); err != nil {
						return watchFrame{}, err
					}
					frame.Text = buf.String()
					return frame, nil
				}
				frame.Text = renderTaskListTable(tasks)
				return frame, nil
			})
		},
	}

	cmd.Flags().StringVar(&opts.status, "status", "", "Filter by status (client-side scan; may page through many tasks)")
	_ = cmd.RegisterFlagCompletionFunc("status", completeTaskStatus)
	cmd.Flags().StringVar(&opts.transactionID, "transaction", "", "Filter by transaction ID (client-side scan)")
	cmd.Flags().StringVarP(&opts.selector, "selector", "l", "", "Label selector, like kubectl -l (for example orka.ai/source=anthropic-proxy)")
	cmd.Flags().StringVar(&opts.since, "since", "", "Only tasks created after this duration ago (10m, 2h) or timestamp (2026-09-23T08:00:00Z)")
	cmd.Flags().IntVar(&opts.limit, "limit", 20, "Maximum number of results")
	cmd.Flags().StringVar(&opts.continueToken, "continue", "", "Continue token for the next page")
	cmd.Flags().StringVar(&opts.continueToken, "cursor", "", "Cursor token for the next page")
	cmd.Flags().BoolVarP(&opts.watch, "watch", "w", false, "Reprint the table when a task appears or changes phase (Ctrl-C to stop)")
	cmd.Flags().DurationVar(&opts.interval, "interval", 5*time.Second, "Refresh interval for --watch")
	addOutputFlag(cmd, outputTable)

	return cmd
}

// parseSince accepts a duration ("10m") or an RFC 3339 timestamp and returns
// the earliest creation time to keep. An empty value keeps everything.
func parseSince(value string, now time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(value); err == nil {
		if d < 0 {
			return time.Time{}, fmt.Errorf("--since duration must not be negative")
		}
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("invalid --since %q: expected a duration such as 10m or an RFC 3339 timestamp", value)
}

// fetchTaskList lists tasks with the server-side selector and the
// client-side status, transaction, and since filters. Any client-side
// filter switches to the paged scan so a match on a later page is not
// missed.
func fetchTaskList(ctx context.Context, c *client.Client, opts taskListOptions, since time.Time) ([]client.TaskSummary, error) {
	filtered := opts.status != "" || opts.transactionID != "" || !since.IsZero()
	if !filtered {
		return c.ListTasks(ctx, client.ListTasksOptions{
			Namespace:     c.Namespace,
			Limit:         opts.limit,
			Continue:      opts.continueToken,
			LabelSelector: opts.selector,
		})
	}
	tasks, truncated, err := listFilteredTasksWithSelector(ctx, c, c.Namespace, opts.limit, opts.selector, func(t client.TaskSummary) bool {
		if opts.status != "" && !strings.EqualFold(t.Phase, opts.status) {
			return false
		}
		if opts.transactionID != "" && t.TransactionID != opts.transactionID {
			return false
		}
		if !since.IsZero() {
			created, err := time.Parse(time.RFC3339, t.Age)
			if err != nil || !created.After(since) {
				return false
			}
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	if truncated {
		warnFilteredTaskOutputLimited(opts.limit)
	}
	return tasks, nil
}

// taskListStateKey describes the set of Tasks and their phases, so a watch
// reprints when a Task appears, disappears, or changes phase, and not when
// its age ticks over.
func taskListStateKey(tasks []client.TaskSummary) string {
	sorted := append([]client.TaskSummary(nil), tasks...)
	sortTasksByCreation(sorted)
	parts := make([]string, 0, len(sorted))
	for _, t := range sorted {
		parts = append(parts, t.Name+"="+t.Phase+"/"+taskAgentLabel(t.Agent, t.Image))
	}
	return strings.Join(parts, "\n")
}

// sortTasksByCreation orders rows oldest first so new Tasks appear at the
// bottom. Rows without a parseable timestamp keep their relative order at
// the end.
func sortTasksByCreation(tasks []client.TaskSummary) {
	sort.SliceStable(tasks, func(i, j int) bool {
		ti, errI := time.Parse(time.RFC3339, tasks[i].Age)
		tj, errJ := time.Parse(time.RFC3339, tasks[j].Age)
		if errI != nil || errJ != nil {
			return errI == nil && errJ != nil
		}
		return ti.Before(tj)
	})
}

func renderTaskListTable(tasks []client.TaskSummary) string {
	if len(tasks) == 0 {
		return "No tasks found.\n"
	}
	sorted := append([]client.TaskSummary(nil), tasks...)
	sortTasksByCreation(sorted)
	var buf bytes.Buffer
	w := tabwriter.NewWriter(&buf, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tTYPE\tAGENT\tSTATUS\tAGE") //nolint:errcheck
	for _, t := range sorted {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", t.Name, t.Type, dash(oneLine(taskAgentLabel(t.Agent, t.Image))), t.Phase, formatAge(t.Age)) //nolint:errcheck
	}
	w.Flush() //nolint:errcheck
	return buf.String()
}
