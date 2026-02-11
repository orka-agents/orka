/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/sozercan/mercan/internal/cli/client"
	"github.com/sozercan/mercan/internal/cli/output"
	"github.com/spf13/cobra"
)

func newTaskCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "task",
		Short: "Manage tasks",
	}

	cmd.AddCommand(
		newTaskCreateCmd(),
		newTaskListCmd(),
		newTaskGetCmd(),
		newTaskDeleteCmd(),
		newTaskLogsCmd(),
		newTaskResultCmd(),
	)

	return cmd
}

func newTaskCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new task",
		RunE: func(cmd *cobra.Command, _ []string) error {
			filePath, _ := cmd.Flags().GetString("file")
			data, err := os.ReadFile(filePath)
			if err != nil {
				return fmt.Errorf("reading file %q: %w", filePath, err)
			}

			var req client.CreateTaskRequest
			if err := json.Unmarshal(data, &req); err != nil {
				return fmt.Errorf("parsing task file: %w", err)
			}

			c := newClientFromCmd(cmd)
			result, err := c.CreateTask(cmd.Context(), req)
			if err != nil {
				return err
			}

			formatStr, _ := cmd.Root().PersistentFlags().GetString("output")
			format, err := output.ParseFormat(formatStr)
			if err != nil {
				return err
			}

			if format == output.FormatTable {
				var m map[string]any
				if err := json.Unmarshal(result, &m); err != nil {
					return output.JSON(os.Stdout, result)
				}
				name := nestedString(m, "metadata", "name")
				ns := nestedString(m, "metadata", "namespace")
				fmt.Fprintf(os.Stdout, "Task %s/%s created\n", ns, name)
				return nil
			}
			return output.JSON(os.Stdout, result)
		},
	}

	cmd.Flags().StringP("file", "f", "", "Path to task definition file (JSON)")
	_ = cmd.MarkFlagRequired("file")

	return cmd
}

func newTaskListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List tasks",
		RunE: func(cmd *cobra.Command, _ []string) error {
			namespace, _ := cmd.Root().PersistentFlags().GetString("namespace")
			limit, _ := cmd.Flags().GetInt("limit")

			c := newClientFromCmd(cmd)
			result, err := c.ListTasks(cmd.Context(), namespace, limit, "")
			if err != nil {
				return err
			}

			formatStr, _ := cmd.Root().PersistentFlags().GetString("output")
			format, err := output.ParseFormat(formatStr)
			if err != nil {
				return err
			}

			if format != output.FormatTable {
				return output.JSON(os.Stdout, result.Items)
			}

			var items []map[string]any
			if err := json.Unmarshal(result.Items, &items); err != nil {
				return fmt.Errorf("parsing task list: %w", err)
			}

			tbl := output.NewTable("NAME", "NAMESPACE", "TYPE", "STATUS", "AGE")
			for _, item := range items {
				name := nestedString(item, "metadata", "name")
				ns := nestedString(item, "metadata", "namespace")
				taskType := nestedString(item, "spec", "type")
				phase := nestedString(item, "status", "phase")
				age := formatCreationAge(item)
				tbl.AddRow(name, ns, taskType, phase, age)
			}
			return tbl.Render(os.Stdout)
		},
	}

	cmd.Flags().Int("limit", 0, "Maximum number of tasks to list (0 = no limit)")

	return cmd
}

func newTaskGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>",
		Short: "Get task details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			namespace, _ := cmd.Root().PersistentFlags().GetString("namespace")

			c := newClientFromCmd(cmd)
			result, err := c.GetTask(cmd.Context(), namespace, args[0])
			if err != nil {
				return err
			}

			formatStr, _ := cmd.Root().PersistentFlags().GetString("output")
			format, err := output.ParseFormat(formatStr)
			if err != nil {
				return err
			}

			if format != output.FormatTable {
				return output.JSON(os.Stdout, result)
			}

			var m map[string]any
			if err := json.Unmarshal(result, &m); err != nil {
				return output.JSON(os.Stdout, result)
			}

			tbl := output.NewTable("NAME", "NAMESPACE", "TYPE", "STATUS", "AGE")
			name := nestedString(m, "metadata", "name")
			ns := nestedString(m, "metadata", "namespace")
			taskType := nestedString(m, "spec", "type")
			phase := nestedString(m, "status", "phase")
			age := formatCreationAge(m)
			tbl.AddRow(name, ns, taskType, phase, age)
			return tbl.Render(os.Stdout)
		},
	}
}

func newTaskDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			namespace, _ := cmd.Root().PersistentFlags().GetString("namespace")

			c := newClientFromCmd(cmd)
			if err := c.DeleteTask(cmd.Context(), namespace, args[0]); err != nil {
				return err
			}

			fmt.Fprintf(os.Stdout, "Task %s/%s deleted\n", namespace, args[0])
			return nil
		},
	}
}

func newTaskLogsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logs <name>",
		Short: "Stream task logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			namespace, _ := cmd.Root().PersistentFlags().GetString("namespace")

			c := newClientFromCmd(cmd)
			rc, err := c.StreamTaskLogs(cmd.Context(), namespace, args[0])
			if err != nil {
				return err
			}
			defer rc.Close()

			scanner := bufio.NewScanner(rc)
			for scanner.Scan() {
				fmt.Fprintln(os.Stdout, scanner.Text())
			}
			return scanner.Err()
		},
	}
}

func newTaskResultCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "result <name>",
		Short: "Get task result",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			namespace, _ := cmd.Root().PersistentFlags().GetString("namespace")

			c := newClientFromCmd(cmd)
			result, err := c.GetTaskResult(cmd.Context(), namespace, args[0])
			if err != nil {
				return err
			}

			return output.JSON(os.Stdout, result)
		},
	}
}

// nestedString extracts a string from a nested map using the given keys.
func nestedString(m map[string]any, keys ...string) string {
	current := any(m)
	for _, key := range keys {
		cm, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = cm[key]
	}
	s, _ := current.(string)
	return s
}

// formatCreationAge extracts metadata.creationTimestamp and returns a human-readable age.
func formatCreationAge(m map[string]any) string {
	ts := nestedString(m, "metadata", "creationTimestamp")
	if ts == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	return output.FormatAge(t)
}
