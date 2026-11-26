/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/orka-agents/orka/internal/cli/client"
)

func newAuditCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Inspect audit and transaction traces",
	}
	cmd.AddCommand(newAuditTraceCmd())
	cmd.AddCommand(newAuditIdentityCmd())
	return cmd
}

func newAuditTraceCmd() *cobra.Command {
	var limit int

	cmd := &cobra.Command{
		Use:   "trace <transaction-id>",
		Short: "Show tasks correlated by transaction ID",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			transactionID := args[0]
			c := newClientFromCmd(cmd)
			filtered, truncated, err := listFilteredTasks(
				context.Background(),
				c,
				c.Namespace,
				limit,
				func(task client.TaskSummary) bool {
					return task.TransactionID == transactionID
				},
			)
			if err != nil {
				return err
			}
			if truncated {
				warnFilteredTaskOutputLimited(limit)
			}
			if len(filtered) == 0 {
				fmt.Printf("No tasks found for transaction %s.\n", transactionID)
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tNAMESPACE\tTYPE\tSTATUS\tPARENT\tAGE") //nolint:errcheck
			for _, task := range filtered {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", //nolint:errcheck
					task.Name, task.Namespace, task.Type, task.Phase, displayParent(task.ParentTask), formatAge(task.Age))
			}
			w.Flush() //nolint:errcheck
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 100, "Maximum number of matching tasks to show")
	return cmd
}

func displayParent(parent string) string {
	if parent == "" {
		return "-"
	}
	return parent
}

type identityAuditRecordView struct {
	Timestamp       string `json:"timestamp"`
	Outcome         string `json:"outcome"`
	Reason          string `json:"reason,omitempty"`
	Mode            string `json:"mode"`
	Tool            string `json:"tool"`
	ToolMethod      string `json:"toolMethod,omitempty"`
	AgentName       string `json:"agentName,omitempty"`
	IdentityIssuer  string `json:"identityIssuer,omitempty"`
	IdentityID      string `json:"identityID,omitempty"`
	Requester       string `json:"requester,omitempty"`
	TaskUID         string `json:"taskUID,omitempty"`
	PolicyName      string `json:"policyName,omitempty"`
	PolicyNamespace string `json:"policyNamespace,omitempty"`
}

func newAuditIdentityCmd() *cobra.Command {
	var taskName, taskUID, agentName, outcome string
	var limit int

	cmd := &cobra.Command{
		Use:   "identity",
		Short: "Query redacted agent-identity-audit records",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			outcome = strings.TrimSpace(outcome)
			if outcome != "" && outcome != "issued" && outcome != "refused" {
				return fmt.Errorf("--outcome must be issued or refused")
			}
			c := newClientFromCmd(cmd)
			query := map[string]string{}
			if strings.TrimSpace(taskName) != "" {
				query["task"] = strings.TrimSpace(taskName)
			}
			if strings.TrimSpace(taskUID) != "" {
				query["taskUID"] = strings.TrimSpace(taskUID)
			}
			if strings.TrimSpace(agentName) != "" {
				query["agent"] = strings.TrimSpace(agentName)
			}
			if outcome != "" {
				query["outcome"] = outcome
			}
			if limit > 0 {
				query["limit"] = strconv.Itoa(limit)
			}
			raw, err := c.DoJSON(context.Background(), http.MethodGet, "/api/v1/audit/identity", query, nil)
			if err != nil {
				return err
			}
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			if format != outputTable {
				return printStructuredTo(cmd.OutOrStdout(), format, raw)
			}
			var payload struct {
				Items []identityAuditRecordView `json:"items"`
			}
			if err := decodeInto(raw, &payload); err != nil {
				return err
			}
			if len(payload.Items) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No identity audit records found.") //nolint:errcheck
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "TIMESTAMP\tOUTCOME\tREASON\tMODE\tTOOL\tAGENT\tIDENTITY\tTASK UID") //nolint:errcheck
			for _, rec := range payload.Items {
				reason := rec.Reason
				if reason == "" {
					reason = "-"
				}
				agent := rec.AgentName
				if agent == "" {
					agent = "-"
				}
				identity := "-"
				if rec.IdentityIssuer != "" && rec.IdentityID != "" {
					identity = rec.IdentityIssuer + "/" + rec.IdentityID
				} else if rec.IdentityID != "" {
					identity = rec.IdentityID
				}
				uid := rec.TaskUID
				if uid == "" {
					uid = "-"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", //nolint:errcheck
					rec.Timestamp, rec.Outcome, reason, rec.Mode, rec.Tool, agent, identity, uid)
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&taskName, "task", "", "Filter by Task name in the active namespace")
	cmd.Flags().StringVar(&taskUID, "task-uid", "", "Filter by Task UID")
	cmd.Flags().StringVar(&agentName, "agent", "", "Filter by Agent name")
	cmd.Flags().StringVar(&outcome, "outcome", "", "Filter by outcome: issued or refused")
	cmd.Flags().IntVar(&limit, "limit", 100, "Maximum number of audit records to return")
	addOutputFlag(cmd, outputTable)
	return cmd
}
