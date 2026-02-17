/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/sozercan/orka/internal/cli/client"
)

func newTaskCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "task",
		Short: "Manage tasks",
	}
	cmd.AddCommand(newTaskCreateCmd())
	cmd.AddCommand(newTaskListCmd())
	cmd.AddCommand(newTaskGetCmd())
	cmd.AddCommand(newTaskLogsCmd())
	cmd.AddCommand(newTaskDeleteCmd())
	cmd.AddCommand(newTaskArtifactsCmd())
	cmd.AddCommand(newTaskDownloadCmd())
	return cmd
}

func newTaskCreateCmd() *cobra.Command {
	var taskType, agent, provider, timeout string

	cmd := &cobra.Command{
		Use:   "create <prompt>",
		Short: "Create a new task",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			prompt := strings.Join(args, " ")

			if taskType == "" {
				taskType = "ai"
			}
			if provider == "" {
				provider = "default"
			}

			b := make([]byte, 4)
			_, _ = rand.Read(b)
			taskName := "task-" + hex.EncodeToString(b)

			req := client.CreateTaskRequest{
				Name:      taskName,
				Namespace: c.Namespace,
				Type:      taskType,
				Prompt:    prompt,
				Timeout:   timeout,
			}

			if agent != "" {
				req.AgentRef = &struct {
					Name string `json:"name"`
				}{Name: agent}
			}

			if taskType == "ai" {
				req.AI = &struct {
					ProviderRef *struct {
						Name string `json:"name"`
					} `json:"providerRef,omitempty"`
					Prompt string `json:"prompt,omitempty"`
				}{
					ProviderRef: &struct {
						Name string `json:"name"`
					}{Name: provider},
					Prompt: prompt,
				}
			}

			result, err := c.CreateTask(context.Background(), req)
			if err != nil {
				return err
			}

			name := client.StringField(*result, "metadata", "name")
			fmt.Printf("Task created: %s\n", name)
			return nil
		},
	}

	cmd.Flags().StringVar(&taskType, "type", "ai", "Task type: ai, container, agent")
	cmd.Flags().StringVar(&agent, "agent", "", "Agent reference name")
	cmd.Flags().StringVar(&provider, "provider", "default", "Provider reference name")
	cmd.Flags().StringVar(&timeout, "timeout", "", "Task timeout (e.g., \"5m\", \"1h\")")

	return cmd
}

func newTaskListCmd() *cobra.Command {
	var status string
	var limit int

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List tasks",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := newClientFromCmd(cmd)
			tasks, err := c.ListTasks(context.Background(), client.ListTasksOptions{
				Namespace: c.Namespace,
				Limit:     limit,
			})
			if err != nil {
				return err
			}

			if status != "" {
				filtered := make([]client.TaskSummary, 0)
				for _, t := range tasks {
					if strings.EqualFold(t.Phase, status) {
						filtered = append(filtered, t)
					}
				}
				tasks = filtered
			}

			if len(tasks) == 0 {
				fmt.Println("No tasks found.")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tTYPE\tSTATUS\tAGE") //nolint:errcheck
			for _, t := range tasks {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", t.Name, t.Type, t.Phase, formatAge(t.Age)) //nolint:errcheck
			}
			w.Flush() //nolint:errcheck
			return nil
		},
	}

	cmd.Flags().StringVar(&status, "status", "", "Filter by status (Pending, Running, Succeeded, Failed)")
	cmd.Flags().IntVar(&limit, "limit", 20, "Maximum number of results")

	return cmd
}

func newTaskGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <name>",
		Short: "Get task details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			detail, err := c.GetTask(context.Background(), args[0], client.GetOptions{
				Namespace: c.Namespace,
			})
			if err != nil {
				return err
			}

			out, err := json.MarshalIndent(detail, "", "  ")
			if err != nil {
				return fmt.Errorf("formatting output: %w", err)
			}
			fmt.Println(string(out))
			return nil
		},
	}
}

func newTaskLogsCmd() *cobra.Command {
	var follow bool

	cmd := &cobra.Command{
		Use:   "logs <name>",
		Short: "Get task logs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)

			if follow {
				ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
				defer cancel()

				err := c.StreamTaskLogs(ctx, args[0], client.StreamLogsOptions{
					Namespace: c.Namespace,
					Writer:    os.Stdout,
				})
				if err != nil && ctx.Err() == nil {
					return err
				}
				return nil
			}

			logsResp, err := c.GetTaskLogs(context.Background(), args[0], client.GetOptions{
				Namespace: c.Namespace,
			})
			if err != nil {
				return err
			}

			if logsResp.Logs != "" {
				fmt.Print(logsResp.Logs)
			} else if logsResp.Message != "" {
				fmt.Println(logsResp.Message)
			}
			return nil
		},
	}

	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Stream logs in real time")

	return cmd
}

func newTaskDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "delete <name>",
		Aliases: []string{"rm"},
		Short:   "Delete a task",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			if err := c.DeleteTask(context.Background(), args[0], client.GetOptions{
				Namespace: c.Namespace,
			}); err != nil {
				return err
			}
			fmt.Printf("Task deleted: %s\n", args[0])
			return nil
		},
	}
}

func formatAge(timestamp string) string {
	if timestamp == "" {
		return "<unknown>"
	}
	t, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return timestamp
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
