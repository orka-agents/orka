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

	"github.com/spf13/cobra"

	"github.com/orka-agents/orka/internal/cli/client"
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
	cmd := &cobra.Command{
		Use:   "approvals <task>",
		Short: "List task approvals",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			path := "/api/v1/tasks/" + url.PathEscape(args[0]) + "/approvals"
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
			return printApprovalsTable(cmd, result)
		},
	}
	addOutputFlag(cmd, outputTable)
	return cmd
}

func newTaskApprovalDecisionCmd(use, short, decision string) *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   use + " <task> <approvalID>",
		Short: short,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			body, _ := json.Marshal(map[string]string{"decision": decision, "reason": reason})
			path := "/api/v1/tasks/" + url.PathEscape(args[0]) +
				"/approvals/" + url.PathEscape(args[1]) + "/decision"
			result, err := c.DoJSON(context.Background(), http.MethodPost, path, nil, body)
			if err != nil {
				return err
			}
			return printStructured(cmd, result)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Decision reason")
	addOutputFlag(cmd, outputJSON)
	return cmd
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
				bodyMap["agentRef"] = map[string]string{"name": agent}
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
	cmd.Flags().StringVar(&newName, "name", "", "Forked task name")
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
	var limit int
	var eventTypes []string
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			query := map[string]string{"after": strconv.FormatInt(after, 10)}
			if limit > 0 {
				query["limit"] = strconv.Itoa(limit)
			}
			path := basePath + "/" + url.PathEscape(args[0]) + "/events"
			path = appendRepeatedTypes(path, query, eventTypes)
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
			return printExecutionEventsTable(cmd, result, includeType)
		},
	}
	cmd.Flags().Int64Var(&after, "after", 0, "Only return events after this sequence")
	cmd.Flags().IntVar(&limit, "limit", 100, "Maximum events to return")
	cmd.Flags().StringArrayVar(&eventTypes, "type", nil, "Filter by event type (repeatable; streaming supports repeats)")
	addOutputFlag(cmd, outputTable)
	return cmd
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
			query := map[string]string{"after": strconv.FormatInt(after, 10)}
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

func printExecutionEventsTable(cmd *cobra.Command, value any, includeTask bool) error {
	m, _ := value.(map[string]any)
	events, _ := m["events"].([]any)
	if len(events) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No events found.") //nolint:errcheck
		return nil
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	if includeTask {
		fmt.Fprintln(w, "SEQ\tTYPE\tSEVERITY\tSUMMARY") //nolint:errcheck
	} else {
		fmt.Fprintln(w, "SEQ\tTASK\tTASKSEQ\tTYPE\tSEVERITY\tSUMMARY") //nolint:errcheck
	}
	for _, raw := range events {
		event, _ := raw.(map[string]any)
		if includeTask {
			_, _ = fmt.Fprintf(
				w,
				"%s\t%s\t%s\t%s\n",
				numberString(event["seq"]),
				anyString(event["type"]),
				anyString(event["severity"]),
				anyString(event["summary"]),
			)
		} else {
			_, _ = fmt.Fprintf(
				w,
				"%s\t%s\t%s\t%s\t%s\t%s\n",
				numberString(event["seq"]),
				anyString(event["taskName"]),
				numberString(event["taskSeq"]),
				anyString(event["type"]),
				anyString(event["severity"]),
				anyString(event["summary"]),
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
		anyString(task["name"]),
		anyString(task["phase"]),
		numberString(m["latestSeq"]),
	) //nolint:errcheck
	for _, section := range []string{"modelRequests", "toolCalls", "childTasks", "errors"} {
		items, _ := m[section].([]any)
		fmt.Fprintf(cmd.OutOrStdout(), "%s: %d\n", section, len(items)) //nolint:errcheck
	}
	return nil
}

func printApprovalsTable(cmd *cobra.Command, value any) error {
	m, _ := value.(map[string]any)
	items, _ := m["approvals"].([]any)
	if len(items) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No approvals found.") //nolint:errcheck
		return nil
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tSTATUS\tACTION\tRISK") //nolint:errcheck
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		_, _ = fmt.Fprintf(
			w,
			"%s\t%s\t%s\t%s\n",
			anyString(item["id"]),
			anyString(item["status"]),
			anyString(item["action"]),
			anyString(item["riskSummary"]),
		)
	}
	return w.Flush()
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
	case json.Number:
		n, _ := v.Int64()
		return n
	default:
		return 0
	}
}
