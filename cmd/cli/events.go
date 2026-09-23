package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/orka-agents/orka/internal/cli/client"
	"github.com/orka-agents/orka/internal/events"
)

const (
	cliNameKey = "name"
	queryAfter = "after"
	queryLimit = "limit"
)

func newTaskEventsCmd() *cobra.Command {
	return newExecutionEventsCmd("events <task>", "List task execution events", "/api/v1/tasks", true)
}

func newTaskFollowCmd() *cobra.Command {
	return newExecutionFollowCmd("follow <task>", "Follow task execution events", "/api/v1/tasks")
}

func newTaskTraceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trace <task>",
		Short: "Show a task trace summary",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			path := "/api/v1/tasks/" + url.PathEscape(args[0]) + "/trace"
			result, err := c.DoJSON(context.Background(), http.MethodGet, path, nil, nil)
			if err != nil {
				return err
			}
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			if format != outputTable {
				return printStructured(cmd, result)
			}
			return printTraceSummary(cmd, result)
		},
	}
	addOutputFlag(cmd, outputTable)
	return cmd
}

func newTaskApprovalsCmd() *cobra.Command {
	var wide bool
	cmd := &cobra.Command{
		Use:   "approvals <task> [id]",
		Short: "List task approvals, or show one request in full",
		Long: `List the approval requests recorded for a task: the tool each one wants to
run, its arguments, and how long a pending request has left. Pass an ID (or
a unique prefix of it) to print one request with every argument on its own
line. --wide adds the severity, risk summary, and who decided and why.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			path := "/api/v1/tasks/" + url.PathEscape(args[0]) + "/approvals"
			result, err := c.DoJSON(context.Background(), http.MethodGet, path, nil, nil)
			if err != nil {
				return err
			}
			if len(args) == 2 {
				approval, err := resolveApproval(result, args[1])
				if err != nil {
					return err
				}
				return printDescribed(cmd, approval, approvalDescribeRows)
			}
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			if format != outputTable {
				return printStructured(cmd, result)
			}
			return printApprovalsTable(cmd, result, wide, time.Now())
		},
	}
	cmd.Flags().BoolVar(&wide, "wide", false, "Show severity, risk summary, and decision details")
	addOutputFlag(cmd, outputTable)
	return cmd
}

func newTaskApprovalDecisionCmd(use, short, decision string) *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   use + " <task> <approvalID>",
		Short: short,
		Long: short + `. The approval ID may be the full ID, the short ID shown by
"orka task approvals", or any prefix of either that matches exactly one
request. The full ID is sent to the server.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			body, _ := json.Marshal(map[string]string{"decision": decision, "reason": reason})
			decide := func(approvalID string) (any, error) {
				path := "/api/v1/tasks/" + url.PathEscape(args[0]) +
					"/approvals/" + url.PathEscape(approvalID) + "/decision"
				return c.DoJSON(context.Background(), http.MethodPost, path, nil, body)
			}
			// A full ID goes straight to the decision endpoint, so a caller
			// whose permissions cover decisions but not reading the task keeps
			// working. Only an unknown ID is treated as a prefix and resolved
			// against the task's approval list.
			result, err := decide(args[1])
			if err != nil && strings.Contains(err.Error(), "HTTP 404") {
				approvalID, resolveErr := resolveApprovalID(context.Background(), c, args[0], args[1])
				if resolveErr != nil {
					return resolveErr
				}
				if approvalID != args[1] {
					result, err = decide(approvalID)
				}
			}
			if err != nil {
				return err
			}
			return printDescribed(cmd, result, approvalDescribeRows)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Decision reason")
	addOutputFlag(cmd, outputTable)
	return cmd
}

// shortApprovalID is the first 12 characters after the last ":" in an
// approval ID (digest-style IDs), or the first 12 characters otherwise.
func shortApprovalID(id string) string {
	id = strings.TrimSpace(id)
	if colon := strings.LastIndex(id, ":"); colon >= 0 {
		id = id[colon+1:]
	}
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// resolveApproval finds the one approval whose full ID, short ID, or a
// prefix of either matches the argument. An exact full-ID match wins
// outright; otherwise exactly one prefix match is required.
func resolveApproval(list any, arg string) (map[string]any, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return nil, fmt.Errorf("approval ID is required")
	}
	m, _ := list.(map[string]any)
	items := anySliceToMaps(m["approvals"])
	var matches []map[string]any
	for _, item := range items {
		id := firstString(item, "id")
		if id == arg {
			return item, nil
		}
		if strings.HasPrefix(id, arg) || strings.HasPrefix(shortApprovalID(id), arg) {
			matches = append(matches, item)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return nil, fmt.Errorf("no approval matches %q; run \"orka task approvals <task>\" to list them", arg)
	}
	lines := make([]string, 0, len(matches))
	for _, item := range matches {
		lines = append(lines, oneLine(fmt.Sprintf("  %s  %s  %s",
			shortApprovalID(firstString(item, "id")),
			firstString(item, "status"),
			firstString(item, "targetTool", "action"))))
	}
	return nil, fmt.Errorf("%q matches %d approvals; use a longer prefix:\n%s", arg, len(matches), strings.Join(lines, "\n"))
}

// resolveApprovalID lists a task's approvals and returns the full ID for a
// possibly abbreviated argument.
func resolveApprovalID(ctx context.Context, c *client.Client, task, arg string) (string, error) {
	path := "/api/v1/tasks/" + url.PathEscape(task) + "/approvals"
	result, err := c.DoJSON(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return "", err
	}
	approval, err := resolveApproval(result, arg)
	if err != nil {
		return "", err
	}
	return firstString(approval, "id"), nil
}

func newTaskForkCmd() *cobra.Command {
	var after int64 = -1
	var newName, agent, prompt string
	cmd := &cobra.Command{
		Use:   "fork <task>",
		Short: "Fork a task from an execution event checkpoint",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			bodyMap := map[string]any{}
			// Forward an explicitly-set --after (including negatives) so the
			// server can validate it. Distinguishing "flag set" from the
			// default sentinel ensures `--after -5` returns a 400 instead of
			// being silently dropped and forking from latest.
			if cmd.Flags().Changed("after") {
				bodyMap["afterSeq"] = after
			}
			if newName != "" {
				bodyMap["newTaskName"] = newName
			}
			if agent != "" {
				bodyMap["agentRef"] = map[string]string{cliNameKey: agent}
			}
			if prompt != "" {
				bodyMap["prompt"] = prompt
			}
			body, _ := json.Marshal(bodyMap)
			c := newClientFromCmd(cmd)
			path := "/api/v1/tasks/" + url.PathEscape(args[0]) + "/fork"
			result, err := c.DoJSON(context.Background(), http.MethodPost, path, nil, body)
			if err != nil {
				return err
			}
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			if format != outputTable {
				return printStructured(cmd, result)
			}
			m, _ := result.(map[string]any)
			created := anyString(m["newTaskName"])
			fmt.Fprintf(cmd.OutOrStdout(), "Forked task created: %s\n", created)                    //nolint:errcheck
			fmt.Fprintf(cmd.OutOrStdout(), "Follow with: orka task follow %s --after 0\n", created) //nolint:errcheck
			return nil
		},
	}
	cmd.Flags().Int64Var(&after, "after", -1, "Checkpoint sequence (default: latest)")
	cmd.Flags().StringVar(&newName, cliNameKey, "", "Forked task name")
	cmd.Flags().StringVar(&agent, "agent", "", "Override agent reference")
	cmd.Flags().StringVar(&prompt, "prompt", "", "Override prompt")
	addOutputFlag(cmd, outputTable)
	return cmd
}

func newSessionEventsCmd() *cobra.Command {
	return newExecutionEventsCmd("events <session>", "List session execution events", "/api/v1/sessions", false)
}

func newSessionFollowCmd() *cobra.Command {
	return newExecutionFollowCmd("follow <session>", "Follow session execution events", "/api/v1/sessions")
}

func newExecutionEventsCmd(use, short, basePath string, includeType bool) *cobra.Command {
	var after int64
	var limit, tail int
	var wide bool
	var eventTypes []string
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Long: short + `.

Table rows are cut to the terminal width; --wide prints full messages and
-o json is never truncated. --tail N keeps only the last N events that match
the filters, so "what did the agent say last?" is:

  orka task events <task> --type ModelMessage --tail 1

--type accepts these event types (case-insensitive):

  ` + strings.Join(events.ExecutionEventTypes(), "\n  "),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			canonical, err := canonicalEventTypes(eventTypes)
			if err != nil {
				return err
			}
			if tail < 0 {
				return fmt.Errorf("--tail must not be negative")
			}
			c := newClientFromCmd(cmd)
			path := basePath + "/" + url.PathEscape(args[0]) + "/events"
			var result any
			if tail > 0 {
				result, err = fetchEventsTail(context.Background(), c, path, after, canonical, tail)
			} else {
				query := map[string]string{queryAfter: strconv.FormatInt(after, 10)}
				if limit > 0 {
					query[queryLimit] = strconv.Itoa(limit)
				}
				result, err = c.DoJSON(context.Background(), http.MethodGet, appendRepeatedTypes(path, query, canonical), nil, nil)
			}
			if err != nil {
				return err
			}
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			if format != outputTable {
				return printStructured(cmd, result)
			}
			return printExecutionEventsTable(cmd, result, includeType, wide)
		},
	}
	cmd.Flags().Int64Var(&after, "after", 0, "Only return events after this sequence")
	cmd.Flags().IntVar(&limit, "limit", 100, "Maximum events to return")
	cmd.Flags().IntVar(&tail, "tail", 0, "Return only the last N matching events")
	cmd.Flags().BoolVar(&wide, "wide", false, "Print full messages instead of cutting rows to the terminal width")
	cmd.Flags().StringArrayVar(&eventTypes, "type", nil, "Filter by event type (repeatable; see --help for the names)")
	_ = cmd.RegisterFlagCompletionFunc("type", func(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeWithPrefix(toComplete, events.ExecutionEventTypes()...), cobra.ShellCompDirectiveNoFileComp
	})
	addOutputFlag(cmd, outputTable)
	return cmd
}

// canonicalEventTypes maps case-insensitive --type values to their exact
// event type names and rejects unknown ones with the list of valid names.
func canonicalEventTypes(values []string) ([]string, error) {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		canonical := events.CanonicalExecutionEventType(value)
		if canonical == "" {
			return nil, fmt.Errorf("unknown event type %q; valid types: %s", value, strings.Join(events.ExecutionEventTypes(), ", "))
		}
		out = append(out, canonical)
	}
	return out, nil
}

const eventsTailPageSize = 500

// fetchEventsTail reads the stream to its latest sequence in pages and keeps
// the last n matching events. The response keeps the list shape so json
// output is the same as an untailed page.
func fetchEventsTail(ctx context.Context, c *client.Client, path string, after int64, eventTypes []string, n int) (any, error) {
	var last map[string]any
	var kept []any
	cursor := after
	for {
		query := map[string]string{queryAfter: strconv.FormatInt(cursor, 10), queryLimit: strconv.Itoa(eventsTailPageSize)}
		result, err := c.DoJSON(ctx, http.MethodGet, appendRepeatedTypes(path, query, eventTypes), nil, nil)
		if err != nil {
			return nil, err
		}
		page, _ := result.(map[string]any)
		if page == nil {
			page = map[string]any{}
		}
		last = page
		pageEvents, _ := page["events"].([]any)
		kept = append(kept, pageEvents...)
		if len(kept) > n {
			kept = kept[len(kept)-n:]
		}
		if len(pageEvents) == 0 {
			break
		}
		lastSeq := int64Field(pageEvents[len(pageEvents)-1].(map[string]any), "seq")
		if lastSeq <= cursor || lastSeq >= int64Field(page, "latestSeq") || len(pageEvents) < eventsTailPageSize {
			break
		}
		cursor = lastSeq
	}
	if kept == nil {
		kept = []any{}
	}
	// The envelope describes the caller's request, not the last page read.
	last["afterSeq"] = after
	last["events"] = kept
	return last, nil
}

func newExecutionFollowCmd(use, short, basePath string) *cobra.Command {
	var after int64
	var eventTypes []string
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			c := newClientFromCmd(cmd)
			query := map[string]string{queryAfter: strconv.FormatInt(after, 10)}
			path := basePath + "/" + url.PathEscape(args[0]) + "/stream"
			body, err := c.Stream(ctx, appendRepeatedTypes(path, query, eventTypes), nil)
			if err != nil {
				return err
			}
			defer body.Close() //nolint:errcheck
			reader := client.NewSSEReader(body)
			lastSeq := after
			for {
				evt, ok := reader.Next()
				if !ok {
					break
				}
				if evt.Event == "execution_event" {
					var data map[string]any
					if err := json.Unmarshal([]byte(evt.Data), &data); err != nil {
						continue
					}
					if seq := int64Field(data, "seq"); seq > lastSeq {
						lastSeq = seq
					}
					_, _ = fmt.Fprintf(
						cmd.OutOrStdout(),
						"%d\t%s\t%s\t%s\n",
						lastSeq,
						anyString(data["type"]),
						anyString(data["severity"]),
						anyString(data["summary"]),
					)
				}
				if evt.Event == "stream_complete" {
					fmt.Fprintln(cmd.OutOrStdout(), "stream complete") //nolint:errcheck
					break
				}
			}
			if ctx.Err() != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Resume with --after %d\n", lastSeq) //nolint:errcheck
				return nil
			}
			return reader.Err()
		},
	}
	cmd.Flags().Int64Var(&after, "after", 0, "Resume after this sequence")
	cmd.Flags().StringArrayVar(&eventTypes, "type", nil, "Filter by event type (repeatable)")
	return cmd
}

func appendRepeatedTypes(path string, query map[string]string, eventTypes []string) string {
	values := url.Values{}
	for k, v := range query {
		if v != "" {
			values.Set(k, v)
		}
	}
	for _, typ := range eventTypes {
		if strings.TrimSpace(typ) != "" {
			values.Add("type", strings.TrimSpace(typ))
		}
	}
	if encoded := values.Encode(); encoded != "" {
		return path + "?" + encoded
	}
	return path
}

const (
	eventSummaryMinWidth    = 12
	columnSeq               = "SEQ"
	columnType              = "TYPE"
	eventTaskColumnMinWidth = 8
	eventTaskColumnMaxWidth = 32
)

func printExecutionEventsTable(cmd *cobra.Command, value any, includeTask, wide bool) error {
	m, _ := value.(map[string]any)
	items, _ := m["events"].([]any)
	if len(items) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No events found.") //nolint:errcheck
		return nil
	}
	// Reserve the width of the other columns so the summary column is the
	// only one cut and every event stays on one line.
	headers := []string{columnSeq, columnType, columnSeverity}
	if !includeTask {
		headers = []string{columnSeq, "TASK", "TASKSEQ", columnType, columnSeverity}
	}
	// The task column is capped from the terminal budget too, so one long
	// Task name cannot push a row past the terminal on its own: whatever the
	// other fixed columns leave, minus the summary minimum, goes to the task
	// column, within [8, 32] runes.
	taskCap := eventTaskColumnMaxWidth
	if !includeTask && !wide {
		otherRows := make([][]string, 0, len(items))
		for _, raw := range items {
			event, _ := raw.(map[string]any)
			otherRows = append(otherRows, []string{numberString(event["seq"]), numberString(event["taskSeq"]), anyString(event["type"]), anyString(event["severity"])})
		}
		others := fixedColumnsWidth([]string{columnSeq, "TASKSEQ", columnType, columnSeverity}, otherRows)
		taskCap = min(eventTaskColumnMaxWidth, max(eventTaskColumnMinWidth, terminalWidth()-others-eventSummaryMinWidth-2))
	}
	taskName := func(event map[string]any) string {
		name := anyString(event["taskName"])
		if wide {
			return sanitizeTerminalText(name)
		}
		return truncateToWidth(name, taskCap)
	}
	fixedRows := make([][]string, 0, len(items))
	for _, raw := range items {
		event, _ := raw.(map[string]any)
		if includeTask {
			fixedRows = append(fixedRows, []string{numberString(event["seq"]), anyString(event["type"]), anyString(event["severity"])})
		} else {
			fixedRows = append(fixedRows, []string{numberString(event["seq"]), taskName(event), numberString(event["taskSeq"]), anyString(event["type"]), anyString(event["severity"])})
		}
	}
	summaryWidth := max(terminalWidth()-fixedColumnsWidth(headers, fixedRows), eventSummaryMinWidth)
	// A model message carries what the agent said in contentText; its
	// summary is often just "model returned message". Show the content when
	// there is any, cut to the terminal unless --wide.
	summary := func(event map[string]any) string {
		text := firstNonEmpty(anyString(event["contentText"]), anyString(event["summary"]))
		if wide {
			return oneLine(text)
		}
		return truncateToWidth(text, summaryWidth)
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	if includeTask {
		fmt.Fprintln(w, "SEQ\tTYPE\tSEVERITY\tSUMMARY") //nolint:errcheck
	} else {
		fmt.Fprintln(w, "SEQ\tTASK\tTASKSEQ\tTYPE\tSEVERITY\tSUMMARY") //nolint:errcheck
	}
	for _, raw := range items {
		event, _ := raw.(map[string]any)
		if includeTask {
			_, _ = fmt.Fprintf(
				w,
				"%s\t%s\t%s\t%s\n",
				numberString(event["seq"]),
				anyString(event["type"]),
				anyString(event["severity"]),
				summary(event),
			)
		} else {
			_, _ = fmt.Fprintf(
				w,
				"%s\t%s\t%s\t%s\t%s\t%s\n",
				numberString(event["seq"]),
				taskName(event),
				numberString(event["taskSeq"]),
				anyString(event["type"]),
				anyString(event["severity"]),
				summary(event),
			)
		}
	}
	return w.Flush()
}

func printTraceSummary(cmd *cobra.Command, value any) error {
	m, _ := value.(map[string]any)
	task, _ := m["task"].(map[string]any)
	_, _ = fmt.Fprintf(
		cmd.OutOrStdout(),
		"Task: %s/%s phase=%s latestSeq=%s\n",
		anyString(task["namespace"]),
		anyString(task[cliNameKey]),
		anyString(task["phase"]),
		numberString(m["latestSeq"]),
	) //nolint:errcheck
	for _, section := range []string{"modelRequests", "toolCalls", "childTasks", "errors"} {
		items, _ := m[section].([]any)
		fmt.Fprintf(cmd.OutOrStdout(), "%s: %d\n", section, len(items)) //nolint:errcheck
	}
	return nil
}

const approvalArgsMinWidth = 24

// printApprovalsTable shows what each request asks for: the tool, its
// arguments cut to the terminal width, and the time left on a pending
// request. --wide adds severity, the risk summary, and the decision.
func printApprovalsTable(cmd *cobra.Command, value any, wide bool, now time.Time) error {
	m, _ := value.(map[string]any)
	items := anySliceToMaps(m["approvals"])
	if len(items) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No approvals found.") //nolint:errcheck
		return nil
	}
	fixedRows := make([][]string, 0, len(items))
	for _, item := range items {
		fixedRows = append(fixedRows, []string{shortApprovalID(firstString(item, "id")), firstString(item, "status"), approvalTool(item), approvalExpiry(item, now)})
	}
	fixed := fixedColumnsWidth([]string{"ID", columnStatus, "TOOL", "EXPIRES"}, fixedRows)
	argsWidth := max(terminalWidth()-fixed, approvalArgsMinWidth)
	hasExecution := false
	for _, item := range items {
		if _, ok := item["executionOutcome"]; ok {
			hasExecution = true
		}
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	header := "ID\tSTATUS\tTOOL\tARGUMENTS\tEXPIRES"
	if wide {
		header += "\tSEVERITY\tRISK\tDECIDED BY\tREASON"
		if hasExecution {
			header += "\tEXECUTION"
		}
	}
	fmt.Fprintln(w, header) //nolint:errcheck
	for _, item := range items {
		args := keyValuePairs(item["targetArgsPreview"])
		if !wide {
			args = truncateToWidth(args, argsWidth)
		}
		cells := []string{
			shortApprovalID(firstString(item, "id")),
			firstString(item, "status"),
			approvalTool(item),
			dash(args),
			dash(approvalExpiry(item, now)),
		}
		if wide {
			cells = append(cells,
				dash(firstString(item, "severity")),
				dash(firstString(item, "riskSummary")),
				dash(firstString(item, "decisionActor")),
				dash(firstString(item, "decisionReason")),
			)
			if hasExecution {
				cells = append(cells, dash(scalarOrJSON(item["executionOutcome"])))
			}
		}
		for i := range cells {
			cells[i] = oneLine(cells[i])
		}
		fmt.Fprintln(w, strings.Join(cells, "\t")) //nolint:errcheck
	}
	return w.Flush()
}

// approvalTool is the tool a request wants to run, or its action when the
// request is not for a tool.
func approvalTool(item map[string]any) string {
	return dash(firstString(item, "targetTool", "action"))
}

func numberString(v any) string {
	switch n := v.(type) {
	case float64:
		return strconv.FormatInt(int64(n), 10)
	case int64:
		return strconv.FormatInt(n, 10)
	case int:
		return strconv.Itoa(n)
	case json.Number:
		return n.String()
	case string:
		return n
	default:
		return ""
	}
}

func int64Field(m map[string]any, key string) int64 {
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	default:
		return 0
	}
}
