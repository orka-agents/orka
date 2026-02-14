/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/sozercan/orka/internal/cli/client"
)

func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Manage agents",
	}
	cmd.AddCommand(newAgentListCmd())
	cmd.AddCommand(newAgentGetCmd())
	cmd.AddCommand(newAgentDeleteCmd())
	return cmd
}

func newAgentListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List agents",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := newClientFromCmd(cmd)
			agents, err := c.ListAgents(context.Background(), client.ListOptions{
				Namespace: c.Namespace,
			})
			if err != nil {
				return err
			}

			if len(agents) == 0 {
				fmt.Println("No agents found.")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tMODEL\tRUNTIME\tACTIVE TASKS") //nolint:errcheck
			for _, a := range agents {
				model := a.Model
				if model == "" {
					model = "-"
				}
				rt := a.Runtime
				if rt == "" {
					rt = "-"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\n", a.Name, model, rt, a.Active) //nolint:errcheck
			}
			w.Flush() //nolint:errcheck
			return nil
		},
	}
}

func newAgentGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>",
		Short: "Get agent details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			agent, err := c.GetAgent(context.Background(), args[0], client.GetOptions{
				Namespace: c.Namespace,
			})
			if err != nil {
				return err
			}

			out, err := json.MarshalIndent(agent, "", "  ")
			if err != nil {
				return fmt.Errorf("formatting output: %w", err)
			}
			fmt.Println(string(out))
			return nil
		},
	}
}

func newAgentDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete an agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			if err := c.DeleteAgent(context.Background(), args[0], client.GetOptions{
				Namespace: c.Namespace,
			}); err != nil {
				return err
			}
			fmt.Printf("Agent deleted: %s\n", args[0])
			return nil
		},
	}
}
