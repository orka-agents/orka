/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"fmt"
	"net/url"
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
	cmd.AddCommand(newAgentCreateCmd())
	cmd.AddCommand(newAgentUpdateCmd())
	cmd.AddCommand(newAgentDeleteCmd())
	return cmd
}

func newAgentListCmd() *cobra.Command {
	cmd := &cobra.Command{
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

			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			if format != outputTable {
				return printStructured(cmd, agents)
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
	addOutputFlag(cmd, outputTable)
	return cmd
}

func newAgentGetCmd() *cobra.Command {
	cmd := &cobra.Command{
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

			return printStructured(cmd, agent)
		},
	}
	addOutputFlag(cmd, outputJSON)
	return cmd
}

func newAgentCreateCmd() *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "create -f <file>",
		Short: "Create an agent from a manifest",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file == "" {
				return fmt.Errorf("--file (-f) is required")
			}
			c := newClientFromCmd(cmd)
			body, err := manifestWithNamespaceJSON(cmd, file, c.Namespace)
			if err != nil {
				return err
			}
			result, err := c.DoJSON(context.Background(), "POST", "/api/v1/agents", nil, body)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Agent created: %s\n", metadataName(result)) //nolint:errcheck
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "Path to agent YAML/JSON manifest")
	return cmd
}

func newAgentUpdateCmd() *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "update <name> -f <file>",
		Short: "Update an agent from a manifest",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if file == "" {
				return fmt.Errorf("--file (-f) is required")
			}
			manifest, body, err := manifestMap(file)
			if err != nil {
				return err
			}
			c := newClientFromCmd(cmd)
			query, err := namespaceQueryForManifest(cmd, c.Namespace, manifest)
			if err != nil {
				return err
			}
			result, err := c.DoJSON(context.Background(), "PUT", "/api/v1/agents/"+url.PathEscape(args[0]), query, body)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Agent updated: %s\n", metadataName(result)) //nolint:errcheck
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "Path to agent YAML/JSON manifest")
	return cmd
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
