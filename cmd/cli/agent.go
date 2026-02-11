/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/sozercan/mercan/internal/cli/client"
	"github.com/sozercan/mercan/internal/cli/output"
	"github.com/spf13/cobra"
)

func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Manage agents",
	}

	cmd.AddCommand(
		newAgentListCmd(),
		newAgentGetCmd(),
		newAgentCreateCmd(),
		newAgentUpdateCmd(),
		newAgentDeleteCmd(),
	)

	return cmd
}

func newAgentListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List agents",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := newClientFromCmd(cmd)
			namespace, _ := cmd.Root().PersistentFlags().GetString("namespace")
			ctx := cmd.Context()

			result, err := c.ListAgents(ctx, namespace)
			if err != nil {
				return fmt.Errorf("listing agents: %w", err)
			}

			formatStr, _ := cmd.Root().PersistentFlags().GetString("output")
			format, err := output.ParseFormat(formatStr)
			if err != nil {
				return err
			}

			if format == output.FormatJSON {
				return output.JSON(os.Stdout, json.RawMessage(result))
			}

			var items []map[string]any
			if err := json.Unmarshal(result, &items); err != nil {
				return fmt.Errorf("parsing agent list: %w", err)
			}

			table := output.NewTable("NAME", "NAMESPACE", "MODEL", "AGE")
			for _, item := range items {
				name := nestedString(item, "metadata", "name")
				ns := nestedString(item, "metadata", "namespace")
				model := nestedString(item, "spec", "model", "name")
				age := formatCreationAge(item)
				table.AddRow(name, ns, model, age)
			}
			return table.Render(os.Stdout)
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
			namespace, _ := cmd.Root().PersistentFlags().GetString("namespace")
			ctx := cmd.Context()

			result, err := c.GetAgent(ctx, namespace, args[0])
			if err != nil {
				return fmt.Errorf("getting agent: %w", err)
			}

			formatStr, _ := cmd.Root().PersistentFlags().GetString("output")
			format, err := output.ParseFormat(formatStr)
			if err != nil {
				return err
			}

			if format == output.FormatJSON {
				return output.JSON(os.Stdout, json.RawMessage(result))
			}

			var item map[string]any
			if err := json.Unmarshal(result, &item); err != nil {
				return fmt.Errorf("parsing agent: %w", err)
			}

			table := output.NewTable("NAME", "NAMESPACE", "MODEL", "AGE")
			name := nestedString(item, "metadata", "name")
			ns := nestedString(item, "metadata", "namespace")
			model := nestedString(item, "spec", "model", "name")
			age := formatCreationAge(item)
			table.AddRow(name, ns, model, age)
			return table.Render(os.Stdout)
		},
	}
}

func newAgentCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new agent",
		RunE: func(cmd *cobra.Command, _ []string) error {
			filePath, _ := cmd.Flags().GetString("file")
			data, err := os.ReadFile(filePath)
			if err != nil {
				return fmt.Errorf("reading file %s: %w", filePath, err)
			}

			var req client.CreateAgentRequest
			if err := json.Unmarshal(data, &req); err != nil {
				return fmt.Errorf("parsing agent file: %w", err)
			}

			c := newClientFromCmd(cmd)
			ctx := cmd.Context()

			result, err := c.CreateAgent(ctx, req)
			if err != nil {
				return fmt.Errorf("creating agent: %w", err)
			}

			return output.JSON(os.Stdout, json.RawMessage(result))
		},
	}
	cmd.Flags().StringP("file", "f", "", "Path to agent definition file (JSON)")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

func newAgentUpdateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update <name>",
		Short: "Update an agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			filePath, _ := cmd.Flags().GetString("file")
			data, err := os.ReadFile(filePath)
			if err != nil {
				return fmt.Errorf("reading file %s: %w", filePath, err)
			}

			var req client.UpdateAgentRequest
			if err := json.Unmarshal(data, &req); err != nil {
				return fmt.Errorf("parsing agent file: %w", err)
			}

			c := newClientFromCmd(cmd)
			namespace, _ := cmd.Root().PersistentFlags().GetString("namespace")
			ctx := cmd.Context()

			result, err := c.UpdateAgent(ctx, namespace, args[0], req)
			if err != nil {
				return fmt.Errorf("updating agent: %w", err)
			}

			return output.JSON(os.Stdout, json.RawMessage(result))
		},
	}
	cmd.Flags().StringP("file", "f", "", "Path to agent definition file (JSON)")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

func newAgentDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete an agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			namespace, _ := cmd.Root().PersistentFlags().GetString("namespace")
			ctx := cmd.Context()

			if err := c.DeleteAgent(ctx, namespace, args[0]); err != nil {
				return fmt.Errorf("deleting agent: %w", err)
			}

			fmt.Fprintf(os.Stdout, "Agent %q deleted\n", args[0])
			return nil
		},
	}
}
