/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

// severityRank orders findings critical-first; unknown severities sort last.
func severityRank(value string) int {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "critical":
		return 5
	case "high":
		return 4
	case "medium":
		return 3
	case "low":
		return 2
	case "info", "informational":
		return 1
	default:
		return 0
	}
}

// validatedLabel collapses the validation status to what a reader needs:
// whether Orka reproduced the finding. Unknown statuses print as-is.
func validatedLabel(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "validated":
		return validatedYes
	case "pending", "running":
		return "pending"
	case "", "unvalidated", "failed":
		return "no"
	default:
		return strings.ToLower(strings.TrimSpace(status))
	}
}

// sortFindings orders rows critical-first, validated before unvalidated,
// then by id, matching what --recommended implies.
func sortFindings(items []map[string]any) {
	sort.SliceStable(items, func(i, j int) bool {
		si, sj := severityRank(firstString(items[i], "severity")), severityRank(firstString(items[j], "severity"))
		if si != sj {
			return si > sj
		}
		vi, vj := validatedLabel(firstString(items[i], "validationStatus")) == validatedYes, validatedLabel(firstString(items[j], "validationStatus")) == validatedYes
		if vi != vj {
			return vi
		}
		return firstString(items[i], "id") < firstString(items[j], "id")
	})
}

const (
	findingTitleMinWidth = 20
	validatedYes         = "yes"
	columnSeverity       = "SEVERITY"
	columnStatus         = "STATUS"
)

func printFindingsTable(cmd *cobra.Command, value any) error {
	items := listItems(value)
	if len(items) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No findings found.") //nolint:errcheck
		return nil
	}
	sortFindings(items)
	// Reserve room for the fixed columns so a long title never wraps the
	// row: severity and validation labels are short, ids are 16 characters,
	// and file locations vary.
	fixedRows := make([][]string, 0, len(items))
	for _, item := range items {
		fixedRows = append(fixedRows, []string{firstString(item, "severity"), validatedLabel(firstString(item, "validationStatus")), firstString(item, "id"), findingLocation(item)})
	}
	titleWidth := max(terminalWidth()-fixedColumnsWidth([]string{columnSeverity, "VALIDATED", "ID", "FILE"}, fixedRows), findingTitleMinWidth)
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "SEVERITY\tVALIDATED\tID\tTITLE\tFILE") //nolint:errcheck
	for _, item := range items {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", //nolint:errcheck
			dash(oneLine(firstString(item, "severity"))),
			validatedLabel(firstString(item, "validationStatus")),
			dash(oneLine(firstString(item, "id"))),
			dash(truncateToWidth(firstString(item, "title"), titleWidth)),
			dash(oneLine(findingLocation(item))),
		)
	}
	return w.Flush()
}

func printScanRunsTable(cmd *cobra.Command, value any) error {
	items := listItems(value)
	if len(items) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No scan runs found.") //nolint:errcheck
		return nil
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tPHASE\tMODE\tSLICES\tFINDINGS\tDROPPED\tSTARTED\tDURATION") //nolint:errcheck
	for _, item := range items {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", //nolint:errcheck
			dash(oneLine(firstString(item, "id"))),
			dash(oneLine(firstString(item, "phase"))),
			dash(oneLine(firstString(item, "mode"))),
			scanSliceProgress(item),
			dash(anyString(item["acceptedFindings"])),
			dash(anyString(item["droppedFindings"])),
			dash(ageOrEmpty(firstString(item, "startedAt"))+scanStartedSuffix(item)),
			dash(scanDuration(item, time.Now())),
		)
	}
	return w.Flush()
}

func scanStartedSuffix(item map[string]any) string {
	if ageOrEmpty(firstString(item, "startedAt")) == "" {
		return ""
	}
	return " ago"
}

// scanSliceProgress renders "reviewed/total" slices, noting skipped ones.
func scanSliceProgress(item map[string]any) string {
	total := anyString(item["sliceCount"])
	reviewed := anyString(item["reviewedSliceCount"])
	if total == "" && reviewed == "" {
		return "-"
	}
	out := dash(reviewed) + "/" + dash(total)
	if skipped := anyString(item["skippedSliceCount"]); skipped != "" && skipped != "0" {
		out += " (" + skipped + " skipped)"
	}
	return out
}

func scanDuration(item map[string]any, now time.Time) string {
	started, err := time.Parse(time.RFC3339, firstString(item, "startedAt"))
	if err != nil {
		return ""
	}
	end := now
	if completed, err := time.Parse(time.RFC3339, firstString(item, "completedAt")); err == nil {
		end = completed
	}
	return formatDurationShort(max(end.Sub(started), 0))
}

func printDroppedFindingsTable(cmd *cobra.Command, value any) error {
	items := listItems(value)
	if len(items) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No dropped findings found.") //nolint:errcheck
		return nil
	}
	fixedRows := make([][]string, 0, len(items))
	for _, item := range items {
		fixedRows = append(fixedRows, []string{firstString(item, "id"), firstString(item, "layer"), firstString(item, "sliceID"), ageOrEmpty(firstString(item, "createdAt"))})
	}
	reasonWidth := max(terminalWidth()-fixedColumnsWidth([]string{"ID", "LAYER", "SLICE", "AGE"}, fixedRows), findingTitleMinWidth)
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tLAYER\tREASON\tSLICE\tAGE") //nolint:errcheck
	for _, item := range items {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", //nolint:errcheck
			dash(oneLine(firstString(item, "id"))),
			dash(oneLine(firstString(item, "layer"))),
			dash(truncateToWidth(firstString(item, "reason"), reasonWidth)),
			dash(oneLine(firstString(item, "sliceID"))),
			dash(ageOrEmpty(firstString(item, "createdAt"))),
		)
	}
	return w.Flush()
}

func printPatchProposalsTable(cmd *cobra.Command, value any) error {
	items := listItems(value)
	if len(items) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No patch proposals found.") //nolint:errcheck
		return nil
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "STATUS\tBRANCH\tPULL REQUEST\tCREATED") //nolint:errcheck
	for _, item := range items {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", //nolint:errcheck
			dash(oneLine(firstString(item, "status"))),
			dash(oneLine(firstString(item, "branch"))),
			dash(oneLine(firstString(item, "prURL"))),
			dash(formatTimestamp(firstString(item, "createdAt"))),
		)
	}
	return w.Flush()
}

// printThreatModel prints the Markdown threat model as text under a short
// header, so a person can read it without pulling `.content` out of JSON.
func printThreatModel(cmd *cobra.Command, value any) error {
	model := toGenericMap(value)
	header := []describeRow{
		{Label: labelRepository, Value: firstString(model, "repositoryScan")},
		{Label: labelVersion, Value: anyString(model["version"])},
		{Label: labelSource, Value: firstString(model, "source")},
		{Label: "Generated by scan", Value: firstString(model, "generatedByScan")},
		{Label: labelUpdated, Value: formatTimestamp(firstString(model, "updatedAt"))},
	}
	if err := printDescribe(cmd, header); err != nil {
		return err
	}
	content := sanitizeTerminalTextKeepNewlines(firstString(model, "content"))
	if strings.TrimSpace(content) == "" {
		return nil
	}
	fmt.Fprintln(cmd.OutOrStdout())        //nolint:errcheck
	fmt.Fprint(cmd.OutOrStdout(), content) //nolint:errcheck
	if !strings.HasSuffix(content, "\n") {
		fmt.Fprintln(cmd.OutOrStdout()) //nolint:errcheck
	}
	return nil
}
