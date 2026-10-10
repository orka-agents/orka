package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/usage"
)

const (
	usageUnavailableText = "Unavailable"
	usageCoverageHeader  = "COVERAGE"
	usageTokensHeader    = "RECORDED TOKENS"
)

type usageOptions struct {
	from, until, asOf, teams, repository, model, kind string
	limit, offset                                     int
	paged                                             bool
}

func newUsageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "usage",
		Short: "Inspect recorded model usage and verified PR outcomes",
		Long: "Inspect retained usage through the Orka API. Teams are Kubernetes namespaces. " +
			"Each server reports only its installation's team namespace; combined-team reports are not supported. " +
			"Issue delivery totals include failed attempts, retries, and linked follow-up work. " +
			"Missing measurements remain unavailable; model prices are not configured.",
	}
	cmd.AddCommand(newUsageSummaryCmd(), newUsageWorkCmd(), newUsageOtherCmd())
	return cmd
}

func newUsageSummaryCmd() *cobra.Command {
	var options usageOptions
	cmd := &cobra.Command{
		Use:   "summary",
		Short: "Show cohort totals, team usage, and a page of work requests",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return queryUsage(cmd, "/api/v1/usage", options, usageSummaryRows)
		},
	}
	options.bindFlags(cmd, true)
	return cmd
}

func newUsageWorkCmd() *cobra.Command {
	var options usageOptions
	cmd := &cobra.Command{
		Use:   "work <work-id>",
		Short: "Inspect a work request's Tasks, measurements, and PR outcomes",
		Long:  "Inspect a work ID returned by usage summary. Without date filters, includes all retained history for that work. Pass the summary's --as-of value to inspect the same report time.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return queryUsage(cmd, "/api/v1/usage/work/"+url.PathEscape(args[0]), options, usageWorkRows)
		},
	}
	options.bindFlags(cmd, false)
	return cmd
}

func newUsageOtherCmd() *cobra.Command {
	var options usageOptions
	cmd := &cobra.Command{
		Use:       "other <category>",
		Short:     "Inspect review_only, other_requests, or unassociated usage",
		ValidArgs: []string{"review_only", "other_requests", "unassociated"},
		Args:      cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return queryUsage(cmd, "/api/v1/usage/other/"+url.PathEscape(args[0]), options, usageOtherRows)
		},
	}
	options.bindFlags(cmd, true)
	return cmd
}

func (o *usageOptions) bindFlags(cmd *cobra.Command, paged bool) {
	addOutputFlag(cmd, outputTable)
	cmd.Flags().StringVar(&o.from, "from", "", "Request-start date or RFC3339 timestamp (UTC; default: current month, all retained history for work)")
	cmd.Flags().StringVar(&o.until, "until", "", "Exclusive request-start end date or RFC3339 timestamp (default: report time)")
	cmd.Flags().StringVar(&o.asOf, "as-of", "", "Include usage and outcomes through this UTC date or RFC3339 timestamp (default: now)")
	cmd.Flags().StringVar(&o.teams, "teams", "", "Explicit team namespace; must match this installation (overrides --namespace)")
	cmd.Flags().StringVar(&o.repository, "repository", "", "Filter by owner/repository")
	cmd.Flags().StringVar(&o.model, "model", "", "Select whole requests that used this model, including their other models")
	cmd.Flags().StringVar(&o.kind, "kind", "", "Filter by issue or pull_request")
	_ = cmd.RegisterFlagCompletionFunc("kind", func(_ *cobra.Command, _ []string, prefix string) ([]string, cobra.ShellCompDirective) {
		return completeWithPrefix(prefix, "issue", "pull_request"), cobra.ShellCompDirectiveNoFileComp
	})
	o.paged = paged
	if paged {
		cmd.Flags().IntVar(&o.limit, "limit", 25, "Page size, capped at 100 by the API; totals always cover the full selection")
		cmd.Flags().IntVar(&o.offset, "offset", 0, "Page offset; reuse --as-of and filters to keep the same report time")
	}
}

func queryUsage(cmd *cobra.Command, path string, options usageOptions, table func([]byte) ([][]string, error)) error {
	format, err := outputFormat(cmd)
	if err != nil {
		return err
	}
	query := mergeQuery(nil, "from", options.from, "until", options.until, "asOf", options.asOf,
		"teams", options.teams, "repository", options.repository, "model", options.model, "kind", options.kind)
	if options.paged {
		if options.limit < 1 || options.offset < 0 {
			return fmt.Errorf("--limit must be positive and --offset must not be negative")
		}
		query["limit"], query["offset"] = strconv.Itoa(options.limit), strconv.Itoa(options.offset)
	}
	c := newClientFromCmd(cmd)
	body, _, err := c.GetRaw(cmd.Context(), path, query)
	if err != nil {
		return err
	}
	if format != outputTable {
		var value any
		if err := json.Unmarshal(body, &value); err != nil {
			return fmt.Errorf("decode usage response: %w", err)
		}
		return printStructured(cmd, value)
	}
	rows, err := table(body)
	if err != nil {
		return fmt.Errorf("decode usage response: %w", err)
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	for _, row := range rows {
		for i := range row {
			row[i] = sanitizeTerminalText(row[i])
		}
		if _, err := fmt.Fprintln(w, strings.Join(row, "\t")); err != nil {
			return err
		}
	}
	return w.Flush()
}

func usageSelectionRows(selection store.UsageFilter) ([][]string, error) {
	if selection.AsOf.IsZero() {
		return nil, fmt.Errorf("missing report selection")
	}
	return [][]string{
		{"Requests started: " + selection.From.UTC().Format(time.RFC3339Nano) + " through " + selection.Until.UTC().Format(time.RFC3339Nano) + " (exclusive, UTC)"},
		{"Usage and outcomes through: " + selection.AsOf.UTC().Format(time.RFC3339Nano)},
		{"Reuse this --as-of value and the same filters for later pages and work details."},
		{},
	}, nil
}

func usageSummaryRows(body []byte) ([][]string, error) {
	var report usage.Report
	if err := json.Unmarshal(body, &report); err != nil {
		return nil, err
	}
	rows, err := usageSelectionRows(report.Selection)
	if err != nil {
		return nil, err
	}
	rows = append(rows, usageSummaryHeader(), usageSummaryRow("TOTAL", report.Summary))
	for _, team := range report.Teams {
		rows = append(rows, usageSummaryRow(team.Namespace, team.Summary))
	}
	rows = append(rows, usageCoverageRows(report.Summary.Totals)...)
	rows = append(rows, []string{}, []string{"Model cost: " + report.Summary.ModelCost},
		[]string{"Partial coverage uses only recorded tokens and can understate usage."}, []string{},
		[]string{"WORK ID", "TEAM", "REPOSITORY", "REQUEST", usageTokensHeader, "OPENED", "MERGED", usageCoverageHeader})
	for _, work := range report.Works {
		rows = append(rows, []string{work.ID, work.Namespace, work.Repository, work.Kind + " #" + strconv.FormatInt(work.Number, 10),
			usageRecordedTokens(work.Summary.Totals), strconv.Itoa(work.Summary.PRsOpened), strconv.Itoa(work.Summary.PRsMerged), work.Summary.Completeness})
	}
	rows = append(rows, usagePageRows("Work requests", report.Page, len(report.Works), report.Selection.AsOf)...)
	rows = append(rows, []string{}, []string{"OTHER TEAM USAGE", "TASKS", usageTokensHeader, usageCoverageHeader})
	for _, group := range report.OtherWork {
		rows = append(rows, []string{group.Category, strconv.Itoa(group.TaskCount), usageRecordedTokens(group.Totals), group.Totals.Completeness})
	}
	if report.RetainedSince != nil {
		rows = append(rows, []string{}, []string{"Inactive cohorts may have expired before: " + report.RetainedSince.UTC().Format(time.RFC3339Nano)})
	}
	return rows, nil
}

func usageSummaryHeader() []string {
	return []string{"TEAM", "REQUESTS", usageTokensHeader, "OPENED", "READY", "MERGED", "TOKENS/OPENED", "TOKENS/MERGED", usageCoverageHeader}
}

func usageSummaryRow(team string, summary usage.Summary) []string {
	return []string{team, strconv.Itoa(summary.WorkRequests), usageRecordedTokens(summary.Totals), strconv.Itoa(summary.PRsOpened),
		strconv.Itoa(summary.PRsReady), strconv.Itoa(summary.PRsMerged), usageRatio(summary.TokensPerPROpened), usageRatio(summary.TokensPerPRMerged), summary.Completeness}
}

func usageWorkRows(body []byte) ([][]string, error) {
	var response struct {
		Selection store.UsageFilter `json:"selection"`
		Work      *usage.Work       `json:"work"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	rows, err := usageSelectionRows(response.Selection)
	if err != nil {
		return nil, err
	}
	if response.Work == nil {
		return nil, fmt.Errorf("missing work request")
	}
	work := response.Work
	rows = append(rows, []string{"Work: " + work.ID + " " + work.Repository + " #" + strconv.FormatInt(work.Number, 10)},
		usageSummaryHeader(), usageSummaryRow(work.Namespace, work.Summary))
	rows = append(rows, usageCoverageRows(work.Summary.Totals)...)
	rows = append(rows, []string{}, []string{"Model cost: " + work.Summary.ModelCost},
		[]string{}, []string{"REPOSITORY", "PR", "ORIGIN", "STATE", "READY", "HEAD", "URL"})
	for _, pr := range work.PullRequests {
		rows = append(rows, []string{pr.Repository, strconv.FormatInt(pr.Number, 10), pr.Origin, pr.State, strconv.FormatBool(pr.Ready), dash(pr.HeadSHA), pr.URL})
	}
	for _, pr := range work.PullRequests {
		if pr.ReadinessReason != "" {
			rows = append(rows, []string{"PR #" + strconv.FormatInt(pr.Number, 10) + ": " + pr.ReadinessReason})
		}
	}
	return append(rows, usageTaskRows(work.Tasks)...), nil
}

func usageOtherRows(body []byte) ([][]string, error) {
	var response struct {
		Selection store.UsageFilter `json:"selection"`
		OtherWork *usage.OtherWork  `json:"otherWork"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	rows, err := usageSelectionRows(response.Selection)
	if err != nil {
		return nil, err
	}
	if response.OtherWork == nil {
		return nil, fmt.Errorf("missing other-usage category")
	}
	group := response.OtherWork
	rows = append(rows, []string{group.Category + ": " + group.Explanation},
		[]string{"TASKS", usageTokensHeader, usageCoverageHeader}, []string{strconv.Itoa(group.TaskCount), usageRecordedTokens(group.Totals), group.Totals.Completeness})
	rows = append(rows, usageCoverageRows(group.Totals)...)
	rows = append(rows, usagePageRows("Tasks", group.Page, len(group.Tasks), response.Selection.AsOf)...)
	return append(rows, usageTaskRows(group.Tasks)...), nil
}

func usageTaskRows(tasks []usage.Task) [][]string {
	var rows [][]string
	for _, task := range tasks {
		name := task.TaskName
		if name == "" {
			name = task.TaskUID
		}
		rows = append(rows, []string{}, []string{"Task: " + task.Namespace + "/" + name},
			[]string{"ROLE", "PHASE", "SESSION", "SHARED", usageTokensHeader, usageCoverageHeader},
			[]string{dash(task.Role), task.Phase, dash(task.SessionName), strconv.FormatBool(task.Shared), usageRecordedTokens(task.Totals), task.Totals.Completeness},
			[]string{}, []string{"MEASUREMENT", "ATTEMPT", "PROVIDER", "MODEL", "SOURCE", "SCOPE", "INPUT", "OUTPUT", "CACHED READ", "CACHE WRITE", "STATUS", usageCoverageHeader})
		for _, m := range task.Measurements {
			rows = append(rows, []string{m.ID, dash(m.AttemptID), dash(m.Provider), dash(m.Model), m.Source, m.Scope,
				usageTokenCount(m.InputTokens), usageTokenCount(m.OutputTokens), usageTokenCount(m.CachedInputTokens), usageTokenCount(m.CacheWriteInputTokens), m.Status, m.Completeness})
		}
		for _, m := range task.Measurements {
			if m.Gap != "" {
				rows = append(rows, []string{"Gap " + m.ID + ": " + m.Gap})
			}
		}
	}
	return rows
}

func usagePageRows(label string, page *usage.Page, shown int, asOf time.Time) [][]string {
	if page == nil {
		return nil
	}
	rows := [][]string{{fmt.Sprintf("%s: %d shown, offset %d, total %d. Aggregate totals cover the full selection.", label, shown, page.Offset, page.Total)}}
	if shown > 0 && page.Offset < page.Total-shown {
		rows = append(rows, []string{fmt.Sprintf("Next page: --offset %d --as-of %s (reuse the same filters).", page.Offset+shown, asOf.UTC().Format(time.RFC3339Nano))})
	}
	return rows
}

func usageRecordedTokens(totals usage.Totals) string {
	if totals.Measurements == 0 {
		return "No model calls"
	}
	if totals.Completeness == "unavailable" {
		return usageUnavailableText
	}
	return strconv.FormatInt(totals.TotalTokens, 10)
}

func usageCoverageRows(totals usage.Totals) [][]string {
	cached, written := usageUnavailableText, usageUnavailableText
	if totals.CachedUsageReported {
		cached = strconv.FormatInt(totals.CachedInputTokens, 10)
	}
	if totals.CacheWriteUsageReported {
		written = strconv.FormatInt(totals.CacheWriteInputTokens, 10)
	}
	return [][]string{
		{},
		{"CACHED READ", "CACHE WRITE", "ESTIMATED TOKENS", "MODEL CALLS", "AGENT ATTEMPTS", "MEASUREMENTS", "REPORTED", "PARTIAL", "MISSING"},
		{cached, written, strconv.FormatInt(totals.EstimatedTokens, 10), strconv.Itoa(totals.Calls), strconv.Itoa(totals.Attempts),
			strconv.Itoa(totals.Measurements), strconv.Itoa(totals.ReportedMeasurements), strconv.Itoa(totals.PartialMeasurements), strconv.Itoa(totals.MissingMeasurements)},
	}
}

func usageTokenCount(value *int64) string {
	if value == nil {
		return usageUnavailableText
	}
	return strconv.FormatInt(*value, 10)
}

func usageRatio(value *float64) string {
	if value == nil {
		return usageUnavailableText
	}
	return strconv.FormatFloat(*value, 'f', -1, 64)
}
