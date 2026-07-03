/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"context"
	"fmt"
	"os"
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
	return cmd
}

func newAuditTraceCmd() *cobra.Command {
	var limit int

	cmd := &cobra.Command{
		Use:   "trace <transaction-id>",
		Short: "Show tasks correlated by kontxt transaction ID",
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
