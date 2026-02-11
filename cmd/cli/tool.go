/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"encoding/json"
	"os"

	"github.com/sozercan/mercan/internal/cli/output"
	"github.com/spf13/cobra"
)

func newToolCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tool",
		Short: "Manage tools",
	}

	cmd.AddCommand(
		newToolListCmd(),
		newToolGetCmd(),
	)

	return cmd
}

func newToolListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List tools",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := newClientFromCmd(cmd)
			namespace, _ := cmd.Root().PersistentFlags().GetString("namespace")
			formatStr, _ := cmd.Root().PersistentFlags().GetString("output")

			data, err := c.ListTools(cmd.Context(), namespace)
			if err != nil {
				return err
			}

			format, err := output.ParseFormat(formatStr)
			if err != nil {
				return err
			}

			if format != output.FormatTable {
				return output.PrintResult(os.Stdout, format, json.RawMessage(data))
			}

			var items []map[string]any
			if err := json.Unmarshal(data, &items); err != nil {
				return output.JSON(os.Stdout, json.RawMessage(data))
			}

			tbl := output.NewTable("NAME", "NAMESPACE", "DESCRIPTION")
			for _, item := range items {
				tbl.AddRow(
					str(item["name"]),
					str(item["namespace"]),
					str(item["description"]),
				)
			}
			return tbl.Render(os.Stdout)
		},
	}
}

func newToolGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>",
		Short: "Get tool details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			namespace, _ := cmd.Root().PersistentFlags().GetString("namespace")
			formatStr, _ := cmd.Root().PersistentFlags().GetString("output")

			data, err := c.GetTool(cmd.Context(), namespace, args[0])
			if err != nil {
				return err
			}

			format, err := output.ParseFormat(formatStr)
			if err != nil {
				return err
			}

			if format == output.FormatTable {
				return output.JSON(os.Stdout, json.RawMessage(data))
			}
			return output.PrintResult(os.Stdout, format, json.RawMessage(data))
		},
	}
}
