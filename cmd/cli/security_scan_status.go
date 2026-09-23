/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/orka-agents/orka/internal/cli/client"
)

func newSecurityScanStatusCmd() *cobra.Command {
	var scanID string
	var watch bool
	var interval time.Duration
	cmd := &cobra.Command{
		Use:   "status <repo>",
		Short: "Show a security scan's progress, stage by stage",
		Long: `Show the latest scan run for a repository: its phase, how many slices have
been reviewed, and one row per pipeline stage with the count of Tasks in each
phase. Failed Tasks are listed by name under the table.

With --watch the table is reprinted whenever a count changes and the command
exits when the scan finishes: exit code 0 when it succeeded, 1 when it failed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			repo := args[0]
			resolved, err := resolveScanRunID(ctx, c, repo, scanID)
			if err != nil {
				return err
			}
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			fetch := func(ctx context.Context) (map[string]any, error) {
				path := "/api/v1/security/repositories/" + url.PathEscape(repo) +
					"/scans/" + url.PathEscape(resolved) + "/progress"
				result, err := c.DoJSON(ctx, http.MethodGet, path, nil, nil)
				if err != nil {
					return nil, err
				}
				return toGenericMap(result), nil
			}
			if !watch {
				progress, err := fetch(ctx)
				if err != nil {
					return err
				}
				if format != outputTable {
					return printStructured(cmd, progress)
				}
				fmt.Fprint(cmd.OutOrStdout(), renderScanProgress(progress)) //nolint:errcheck
				return nil
			}
			var final map[string]any
			err = watchLoop(ctx, cmd.OutOrStdout(), interval, format, func(ctx context.Context) (watchFrame, error) {
				progress, err := fetch(ctx)
				if err != nil {
					return watchFrame{}, err
				}
				complete, _ := progress["complete"].(bool)
				if complete {
					final = progress
				}
				frame := watchFrame{Key: scanProgressStateKey(progress), Done: complete}
				if format != outputTable {
					var buf bytes.Buffer
					if err := printStructuredTo(&buf, format, progress); err != nil {
						return watchFrame{}, err
					}
					frame.Text = buf.String()
					return frame, nil
				}
				frame.Text = renderScanProgress(progress)
				return frame, nil
			})
			if err != nil {
				return err
			}
			if final == nil {
				return nil
			}
			if phase := scanRunPhase(final); !scanRunSucceeded(phase) {
				return fmt.Errorf("security scan %s finished with phase %s", resolved, phase)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&scanID, "scan", "", "Scan run ID (default: the latest run)")
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "Reprint when progress changes and exit when the scan finishes")
	cmd.Flags().DurationVar(&interval, "interval", 5*time.Second, "Refresh interval for --watch")
	addOutputFlag(cmd, outputTable)
	return cmd
}

// resolveScanRunID returns the explicit --scan ID or the repository's latest
// run.
func resolveScanRunID(ctx context.Context, c *client.Client, repo, scanID string) (string, error) {
	if scanID = strings.TrimSpace(scanID); scanID != "" {
		return scanID, nil
	}
	path := "/api/v1/security/repositories/" + url.PathEscape(repo) + "/scans"
	result, err := c.DoJSON(ctx, http.MethodGet, path, map[string]string{queryLimit: "1"}, nil)
	if err != nil {
		return "", err
	}
	runs := listItems(result)
	if len(runs) == 0 || firstString(runs[0], "id") == "" {
		return "", fmt.Errorf("no scan runs found for repository %q; start one with: orka security scan run %s", repo, repo)
	}
	return firstString(runs[0], "id"), nil
}

func scanRunPhase(progress map[string]any) string {
	return strings.ToLower(strings.TrimSpace(nestedString(progress, "scan", "phase")))
}

func scanRunSucceeded(phase string) bool {
	switch phase {
	case "succeeded", "completed":
		return true
	default:
		return false
	}
}

// scanProgressStateKey describes the counts a watch reprints for: the scan
// phase, slice progress, and every stage's Task counts and failed names.
// Ages are left out so the passage of time alone never reprints.
func scanProgressStateKey(progress map[string]any) string {
	scan := nestedMap(progress, "scan")
	stages := anySliceToMaps(progress["stages"])
	parts := make([]string, 0, len(stages))
	parts = append(parts,
		firstString(scan, "phase"),
		anyString(scan["reviewedSliceCount"])+"/"+anyString(scan["sliceCount"]),
		anyString(scan["acceptedFindings"])+"/"+anyString(scan["droppedFindings"]),
		firstString(scan, "errorMessage"),
	)
	for _, stage := range stages {
		parts = append(parts, fmt.Sprintf("%s:%d/%d/%d/%d/%d/%d:%s",
			firstString(stage, "stage"), intField(stage, "tasks"), intField(stage, "pending"), intField(stage, "running"),
			intField(stage, "succeeded"), intField(stage, "failed"), intField(stage, "cancelled"),
			strings.Join(anySliceToStrings(stage["failedTasks"]), ",")))
	}
	return strings.Join(parts, "\n")
}

// renderScanProgress draws the header lines and the per-stage table.
func renderScanProgress(progress map[string]any) string {
	var buf bytes.Buffer
	scan := nestedMap(progress, "scan")
	phase := firstString(scan, "phase")
	if started := ageOrEmpty(firstString(scan, "startedAt")); started != "" {
		phase = joinNonEmpty(phase, "started "+started+" ago", ", ")
	}
	if completed := ageOrEmpty(firstString(scan, "completedAt")); completed != "" {
		phase = joinNonEmpty(phase, "finished "+completed+" ago", ", ")
	}
	header := []describeRow{
		{Label: "Scan", Value: joinNonEmpty(firstString(scan, "id"), firstString(scan, "mode"), " ")},
		{Label: labelPhase, Value: phase},
		{Label: "Slices", Value: scanSliceSummary(scan)},
		{Label: "Findings", Value: scanFindingSummary(scan)},
		{Label: "Error", Value: firstString(scan, "errorMessage")},
	}
	writeDescribeRows(&buf, compactDescribeRows(header), 0, terminalWidth())
	buf.WriteString("\n")

	stages := anySliceToMaps(progress["stages"])
	w := tabwriter.NewWriter(&buf, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "STAGE\tTASKS\tPENDING\tRUNNING\tSUCCEEDED\tFAILED") //nolint:errcheck
	failed := make([]string, 0, len(stages))
	for _, stage := range stages {
		failedCount := intField(stage, "failed") + intField(stage, "cancelled")
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%d\n", //nolint:errcheck
			dash(firstString(stage, "label", "stage")),
			intField(stage, "tasks"),
			intField(stage, "pending"),
			intField(stage, "running"),
			intField(stage, "succeeded"),
			failedCount,
		)
		failed = append(failed, anySliceToStrings(stage["failedTasks"])...)
	}
	w.Flush() //nolint:errcheck
	if len(failed) > 0 {
		buf.WriteString("\nFailed tasks:\n")
		for _, name := range failed {
			fmt.Fprintf(&buf, "  %s\n", sanitizeTerminalText(name)) //nolint:errcheck
		}
	}
	return buf.String()
}

func scanSliceSummary(scan map[string]any) string {
	total := intField(scan, "sliceCount")
	if total == 0 && intField(scan, "reviewedSliceCount") == 0 {
		return ""
	}
	out := fmt.Sprintf("%d of %d reviewed", intField(scan, "reviewedSliceCount"), total)
	if skipped := intField(scan, "skippedSliceCount"); skipped > 0 {
		out += fmt.Sprintf(", %d skipped", skipped)
	}
	return out
}

func scanFindingSummary(scan map[string]any) string {
	accepted := intField(scan, "acceptedFindings")
	dropped := intField(scan, "droppedFindings")
	if accepted == 0 && dropped == 0 {
		return ""
	}
	return fmt.Sprintf("%d accepted, %d dropped", accepted, dropped)
}

func intField(m map[string]any, key string) int {
	return int(int64Field(m, key))
}

func anySliceToStrings(value any) []string {
	if typed, ok := value.([]string); ok {
		return typed
	}
	list, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s := anyString(item); s != "" {
			out = append(out, s)
		}
	}
	return out
}
