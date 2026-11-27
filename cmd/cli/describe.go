/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

const (
	labelName              = "Name"
	labelNamespace         = "Namespace"
	labelPhase             = "Phase"
	labelWaitingOn         = "Waiting on"
	labelStatus            = "Status"
	labelType              = "Type"
	labelAgent             = "Agent"
	labelProvider          = "Provider"
	labelSession           = "Session"
	labelCreated           = "Created"
	labelUpdated           = "Updated"
	labelExecution         = "Execution"
	labelDelivery          = "Delivery"
	labelPublicationBranch = "Publication branch"
	labelPullRequest       = "Pull request"
	labelMessage           = "Message"
	labelReady             = "Ready"
	labelTitle             = "Title"
	labelState             = "State"
	labelSummary           = "Summary"
	labelRepository        = "Repository"
	labelBranch            = "Branch"
	labelTask              = "Task"
	labelReason            = "Reason"
	labelSource            = "Source"
	labelTags              = "Tags"
	labelVersion           = "Version"
	labelAccepted          = "Accepted"
	labelGateway           = "Gateway"
)

type describeRow struct {
	Label    string
	Value    string
	Children []describeRow
}

const (
	defaultTerminalWidth = 80
	minTerminalWidth     = 40
	describeIndent       = 2
)

func terminalWidth() int {
	if raw := strings.TrimSpace(os.Getenv("COLUMNS")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return max(n, minTerminalWidth)
		}
	}
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		return max(w, minTerminalWidth)
	}
	return defaultTerminalWidth
}

func printDescribe(cmd *cobra.Command, rows []describeRow) error {
	rows = compactDescribeRows(rows)
	if len(rows) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No fields to show.") //nolint:errcheck
		return nil
	}
	writeDescribeRows(cmd.OutOrStdout(), rows, 0, terminalWidth())
	return nil
}

func compactDescribeRows(rows []describeRow) []describeRow {
	out := make([]describeRow, 0, len(rows))
	for _, row := range rows {
		row.Children = compactDescribeRows(row.Children)
		if strings.TrimSpace(row.Value) == "" && len(row.Children) == 0 {
			continue
		}
		out = append(out, row)
	}
	return out
}

func writeDescribeRows(w io.Writer, rows []describeRow, indent, width int) {
	labels := make([]string, len(rows))
	labelWidth := 0
	for i, row := range rows {
		labels[i] = oneLine(row.Label)
		labelWidth = max(labelWidth, utf8.RuneCountInString(labels[i])+1)
	}
	pad := strings.Repeat(" ", indent)
	for i, row := range rows {
		if len(row.Children) > 0 {
			fmt.Fprintf(w, "%s%s:\n", pad, labels[i]) //nolint:errcheck
			writeDescribeRows(w, row.Children, indent+describeIndent, width)
			continue
		}
		label := labels[i] + ":"
		valueIndent := indent + labelWidth + 1
		lines := wrapText(sanitizeTerminalTextKeepNewlines(row.Value), max(width-valueIndent, 20))
		for i, line := range lines {
			switch {
			case i == 0:
				fmt.Fprintf(w, "%s%-*s %s\n", pad, labelWidth, label, line) //nolint:errcheck
			case line == "":
				fmt.Fprintln(w) //nolint:errcheck
			default:
				fmt.Fprintf(w, "%s%s\n", strings.Repeat(" ", valueIndent), line) //nolint:errcheck
			}
		}
	}
}

func wrapText(text string, width int) []string {
	if width <= 0 {
		width = defaultTerminalWidth
	}
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var out []string
	for paragraph := range strings.SplitSeq(strings.TrimRight(text, "\n"), "\n") {
		paragraph = strings.TrimRight(paragraph, " \t")
		if utf8.RuneCountInString(paragraph) <= width {
			out = append(out, paragraph)
			continue
		}
		line := ""
		for word := range strings.FieldsSeq(paragraph) {
			switch {
			case line == "":
				line = word
			case utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) > width:
				out = append(out, line)
				line = word
			default:
				line += " " + word
			}
		}
		if line != "" {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}

func fixedColumnsWidth(headers []string, rows [][]string) int {
	widths := make([]int, len(headers))
	for i, header := range headers {
		widths[i] = utf8.RuneCountInString(header)
	}
	for _, row := range rows {
		for i := range widths {
			if i < len(row) {
				widths[i] = max(widths[i], utf8.RuneCountInString(row[i]))
			}
		}
	}
	total := 0
	for _, width := range widths {
		total += width + 2
	}
	return total
}

func sanitizeTerminalTextKeepNewlines(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r == '\n' || r == '\t' {
			b.WriteRune(r)
			continue
		}
		b.WriteString(sanitizeTerminalText(string(r)))
	}
	return b.String()
}

func truncateToWidth(value string, width int) string {
	value = oneLine(value)
	if width <= 0 || utf8.RuneCountInString(value) <= width {
		return value
	}
	if width == 1 {
		return "…"
	}
	runes := []rune(value)
	return strings.TrimRight(string(runes[:width-1]), " ") + "…"
}

func oneLine(value string) string {
	value = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ").Replace(value)
	value = sanitizeTerminalText(value)
	value = strings.Join(strings.Fields(value), " ")
	return value
}

func firstLines(text string, n int) string {
	lines := make([]string, 0, n)
	total := 0
	for line := range strings.SplitSeq(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		total++
		if len(lines) < n {
			lines = append(lines, line)
		}
	}
	if total > n {
		lines = append(lines, "…")
	}
	return strings.Join(lines, "\n")
}

func formatTimestamp(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return value
	}
	return fmt.Sprintf("%s (%s ago)", t.Local().Format("2006-01-02 15:04:05"), formatAge(value))
}

func formatUntil(value string, now time.Time) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return value
	}
	d := t.Sub(now)
	if d <= 0 {
		return "expired"
	}
	return "in " + formatDurationShort(d)
}

func formatDurationShort(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func joinStrings(value any) string {
	switch list := value.(type) {
	case []string:
		return strings.Join(list, ", ")
	case []any:
		parts := make([]string, 0, len(list))
		for _, item := range list {
			if s := anyString(item); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ", ")
	default:
		return ""
	}
}

func nameList(value any) string {
	list, ok := value.([]any)
	if !ok {
		return ""
	}
	names := make([]string, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			if name := firstString(m, "name"); name != "" {
				names = append(names, name)
			}
		}
	}
	return strings.Join(names, ", ")
}

func keyValuePairs(value any) string {
	m, ok := value.(map[string]any)
	if !ok || len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		value := m[key]
		switch value.(type) {
		case map[string]any, []any:
			parts = append(parts, key+"="+scalarOrJSON(value))
		default:
			parts = append(parts, key+"="+quoteIfNeeded(scalarOrJSON(value)))
		}
	}
	return strings.Join(parts, " ")
}

func quoteIfNeeded(value string) string {
	if value == "" || strings.ContainsAny(value, " \t\"") {
		return strconv.Quote(value)
	}
	return value
}

func genericDescribeRows(item map[string]any) []describeRow {
	return []describeRow{
		{Label: labelName, Value: genericRowName(item)},
		{Label: labelNamespace, Value: genericRowNamespace(item)},
		{Label: labelStatus, Value: genericRowStatus(item)},
		{Label: "Age", Value: ageOrEmpty(genericRowTimestamp(item))},
	}
}

func toGenericMap(value any) map[string]any {
	if m, ok := value.(map[string]any); ok {
		return m
	}
	return jsonRoundTrip(value)
}

func scalarOrJSON(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool, float64, int, int64, json.Number:
		return anyString(v)
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(encoded)
	}
}

func jsonRoundTrip(value any) map[string]any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return map[string]any{}
	}
	var generic map[string]any
	if err := json.Unmarshal(encoded, &generic); err != nil {
		return map[string]any{}
	}
	return generic
}

func ageOrEmpty(timestamp string) string {
	if strings.TrimSpace(timestamp) == "" {
		return ""
	}
	return formatAge(timestamp)
}

func sortStrings(values []string) {
	sort.Strings(values)
}
