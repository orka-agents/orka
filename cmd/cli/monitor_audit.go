package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func newMonitorAuditCmd() *cobra.Command {
	var since, until, decision, cursor string
	var limit int
	cmd := &cobra.Command{
		Use:   "audit <name>",
		Short: "List a repository monitor's decisions in a time window",
		Long: `List what a repository monitor decided, newest first: each accepted,
refused, held, blocked, acted or failed decision with its reason, the spec
clause that decided it, and the command, Task and GitHub mutation it names.
--since and --until take a duration before now (24h) or an RFC 3339 time.`,
		Example: `  orka monitor audit example-app --since 24h
  orka monitor audit example-app --since 2026-11-25T00:00:00Z --decision refused,failed -o json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			q := mergeQuery(
				map[string]string{},
				"since", since,
				"until", until,
				"decision", decision,
				"limit", fmt.Sprintf("%d", limit),
				"continue", cursor,
			)
			c := newClientFromCmd(cmd)
			path := "/api/v1/monitors/repositories/" + url.PathEscape(args[0]) + "/audit"
			result, err := c.DoJSON(context.Background(), http.MethodGet, path, q, nil)
			if err != nil {
				return err
			}
			if format == outputTable {
				return printMonitorAuditTable(cmd.OutOrStdout(), result)
			}
			return printStructuredTo(cmd.OutOrStdout(), format, result)
		},
	}
	addOutputFlag(cmd, outputTable)
	cmd.Flags().StringVar(&since, "since", "24h", "Start of the window: a duration before now or an RFC 3339 time")
	cmd.Flags().StringVar(&until, "until", "", "End of the window, exclusive: a duration before now or an RFC 3339 time")
	cmd.Flags().StringVar(&decision, "decision", "", "Comma-separated decisions to keep: accepted, refused, held, blocked, acted, failed")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum number of decisions")
	cmd.Flags().StringVar(&cursor, "continue", "", "Continue token from a previous page")
	return cmd
}

// printMonitorAuditTable prints one row per decision. Every cell is
// sanitized: summaries and refs can carry forge-supplied text.
func printMonitorAuditTable(w io.Writer, result any) error {
	items := listItems(result)
	if len(items) == 0 {
		_, err := fmt.Fprintln(w, "No decisions found.")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tDECISION\tTYPE\tITEM\tREASON\tPOLICY\tACTOR\tTASK\tMUTATION") //nolint:errcheck
	for _, item := range items {
		cells := []string{
			firstString(item, "createdAt"),
			firstString(item, "decision"),
			firstString(item, "eventType"),
			monitorAuditItem(item),
			firstString(item, "reason"),
			firstString(item, "policy"),
			monitorAuditActor(item),
			nestedString(item, "task", "name"),
			monitorAuditMutations(item),
		}
		for i := range cells {
			cells[i] = dash(strings.TrimSpace(sanitizeTerminalText(cells[i])))
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t")) //nolint:errcheck
	}
	if next := nestedString(asMap(result), "metadata", "continue"); next != "" {
		fmt.Fprintf(tw, "\nMore decisions: --continue %s\n", sanitizeTerminalText(next)) //nolint:errcheck
	}
	return tw.Flush()
}

func monitorAuditItem(item map[string]any) string {
	number := firstString(item, "itemNumber")
	if number == "" {
		return ""
	}
	kind := "#"
	if firstString(item, "itemKind") == "pull_request" {
		kind = "PR #"
	}
	return kind + number
}

// monitorAuditActor shows the command author when a person triggered the
// decision, and the recording component otherwise.
func monitorAuditActor(item map[string]any) string {
	if author := nestedString(item, "command", "author"); author != "" {
		return author
	}
	return firstString(item, "actor")
}

func monitorAuditMutations(item map[string]any) string {
	mutations, _ := item["mutations"].([]any)
	parts := make([]string, 0, len(mutations))
	for _, raw := range mutations {
		mutation := asMap(raw)
		label := firstString(mutation, "operation")
		if label == "" {
			label = firstString(mutation, "id")
		}
		if status := firstString(mutation, "status"); status != "" {
			label += " " + status
		}
		parts = append(parts, label)
	}
	return strings.Join(parts, ", ")
}

func asMap(value any) map[string]any {
	m, _ := value.(map[string]any)
	return m
}
