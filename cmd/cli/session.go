/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/sozercan/mercan/internal/cli/output"
	"github.com/spf13/cobra"
)

func newSessionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Manage sessions",
	}

	cmd.AddCommand(
		newSessionListCmd(),
		newSessionGetCmd(),
		newSessionDeleteCmd(),
	)

	return cmd
}

func newSessionListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List sessions",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := newClientFromCmd(cmd)
			namespace, _ := cmd.Root().PersistentFlags().GetString("namespace")
			formatStr, _ := cmd.Root().PersistentFlags().GetString("output")

			data, err := c.ListSessions(cmd.Context(), namespace)
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

			tbl := output.NewTable("NAME", "NAMESPACE", "TYPE", "MESSAGES", "ACTIVE TASK", "CREATED")
			for _, item := range items {
				tbl.AddRow(
					str(item["name"]),
					str(item["namespace"]),
					str(item["sessionType"]),
					str(item["messageCount"]),
					str(item["activeTask"]),
					str(item["createdAt"]),
				)
			}
			return tbl.Render(os.Stdout)
		},
	}
}

func newSessionGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <id>",
		Short: "Get session details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			namespace, _ := cmd.Root().PersistentFlags().GetString("namespace")
			formatStr, _ := cmd.Root().PersistentFlags().GetString("output")

			data, err := c.GetSession(cmd.Context(), namespace, args[0])
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

func newSessionDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			namespace, _ := cmd.Root().PersistentFlags().GetString("namespace")

			if err := c.DeleteSession(cmd.Context(), namespace, args[0]); err != nil {
				return err
			}

			fmt.Fprintf(os.Stdout, "Session %q deleted\n", args[0])
			return nil
		},
	}
}

// str converts an interface value to a string for table display.
func str(v any) string {
	if v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case float64:
		if val == float64(int64(val)) {
			return fmt.Sprintf("%d", int64(val))
		}
		return fmt.Sprintf("%g", val)
	default:
		return fmt.Sprintf("%v", val)
	}
}
