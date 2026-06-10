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

const (
	cliTaskTypeAI    = "ai"
	cliTaskTypeAgent = "agent"
	cliTaskTypeCont  = "container"
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
	cmd.AddCommand(newTaskResultCmd())
	cmd.AddCommand(newTaskPlanCmd())
	cmd.AddCommand(newTaskChildrenCmd())
	cmd.AddCommand(newTaskWaitCmd())
	cmd.AddCommand(newTaskDeleteCmd())
	cmd.AddCommand(newTaskArtifactsCmd())
	cmd.AddCommand(newTaskDownloadCmd())
	return cmd
}

func newTaskCreateCmd() *cobra.Command {
	var taskType, taskName, agent, provider, model, timeout, image, schedule, timezone, file string
	var commandVals, argVals, envVals []string
	var priority int32
	var suspend bool

	cmd := &cobra.Command{
		Use:   "create <prompt>",
		Short: "Create a new task",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)

			if file != "" {
				body, err := manifestWithNamespaceJSON(cmd, file, c.Namespace)
				if err != nil {
					return err
				}
				result, err := c.CreateTaskRaw(context.Background(), body)
				if err != nil {
					return err
				}
				name := client.StringField(*result, "metadata", "name")
				fmt.Printf("Task created: %s\n", name)
				return nil
			}

			prompt := strings.Join(args, " ")
			if taskType == "" {
				taskType = cliTaskTypeAI
			}
			if (taskType == cliTaskTypeAI || taskType == cliTaskTypeAgent) && strings.TrimSpace(prompt) == "" {
				return fmt.Errorf("prompt is required for ai and agent tasks unless --file is used")
			}
			if taskName == "" {
				b := make([]byte, 4)
				_, _ = rand.Read(b)
				taskName = "task-" + hex.EncodeToString(b)
			}

			req := client.CreateTaskRequest{
				Name:      taskName,
				Namespace: c.Namespace,
				Type:      taskType,
				Image:     image,
				Command:   commandVals,
				Args:      argVals,
				Prompt:    prompt,
				Timeout:   timeout,
				Schedule:  schedule,
			}
			if timezone != "" {
				req.TimeZone = &timezone
			}
			if cmd.Flags().Changed("suspend") {
				req.Suspend = &suspend
			}
			if cmd.Flags().Changed("priority") {
				req.Priority = &priority
			}
			if len(envVals) > 0 {
				for _, env := range envVals {
					key, value, ok := strings.Cut(env, "=")
					if !ok || key == "" {
						return fmt.Errorf("invalid --env %q, expected KEY=VALUE", env)
					}
					req.Env = append(req.Env, struct {
						Name  string `json:"name"`
						Value string `json:"value,omitempty"`
					}{Name: key, Value: value})
				}
			}

			if agent != "" {
				req.AgentRef = &struct {
					Name string `json:"name"`
				}{Name: agent}
			}

			if taskType == cliTaskTypeAI {
				if provider == "" {
					provider = defaultNamespace
				}
				req.AI = &struct {
					ProviderRef *struct {
						Name string `json:"name"`
					} `json:"providerRef,omitempty"`
					Model  string `json:"model,omitempty"`
					Prompt string `json:"prompt,omitempty"`
				}{
					ProviderRef: &struct {
						Name string `json:"name"`
					}{Name: provider},
					Model:  model,
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

	cmd.Flags().StringVarP(&file, "file", "f", "", "Path to task YAML/JSON manifest")
	cmd.Flags().StringVar(&taskName, "name", "", "Task name (default: generated)")
	cmd.Flags().StringVar(
		&taskType,
		"type",
		cliTaskTypeAI,
		"Task type: "+cliTaskTypeAI+", "+cliTaskTypeCont+", "+cliTaskTypeAgent,
	)
	cmd.Flags().StringVar(&image, "image", "", "Container image")
	cmd.Flags().StringArrayVar(&commandVals, "command", nil, "Command entry to run (repeat for multiple entries)")
	cmd.Flags().StringArrayVar(&argVals, "arg", nil, "Command argument (repeat for multiple arguments)")
	cmd.Flags().StringArrayVar(&envVals, "env", nil, "Environment variable KEY=VALUE (repeatable)")
	cmd.Flags().Int32Var(&priority, "priority", 0, "Task priority (0-1000)")
	cmd.Flags().StringVar(&agent, "agent", "", "Agent reference name")
	cmd.Flags().StringVar(&provider, "provider", defaultNamespace, "Provider reference name")
	cmd.Flags().StringVar(&model, "model", "", "Model name for AI tasks")
	cmd.Flags().StringVar(&timeout, "timeout", "", "Task timeout (e.g., \"5m\", \"1h\")")
	cmd.Flags().StringVar(&schedule, "schedule", "", "Cron schedule for recurring tasks")
	cmd.Flags().StringVar(&timezone, "timezone", "", "IANA time zone for scheduled tasks")
	cmd.Flags().BoolVar(&suspend, "suspend", false, "Suspend scheduled task runs")

	return cmd
}

func newTaskListCmd() *cobra.Command {
	var status string
	var transactionID string
	var limit int
	var continueToken string

	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List tasks",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := newClientFromCmd(cmd)
			var tasks []client.TaskSummary
			if status != "" || transactionID != "" {
				var truncated bool
				var err error
				tasks, truncated, err = listFilteredTasks(
					context.Background(),
					c,
					c.Namespace,
					limit,
					func(t client.TaskSummary) bool {
						if status != "" && !strings.EqualFold(t.Phase, status) {
							return false
						}
						if transactionID != "" && t.TransactionID != transactionID {
							return false
						}
						return true
					},
				)
				if err != nil {
					return err
				}
				if truncated {
					warnFilteredTaskOutputLimited(limit)
				}
			} else {
				var err error
				tasks, err = c.ListTasks(context.Background(), client.ListTasksOptions{
					Namespace: c.Namespace,
					Limit:     limit,
					Continue:  continueToken,
				})
				if err != nil {
					return err
				}
			}

			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			if format != outputTable {
				return printStructured(cmd, tasks)
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

	cmd.Flags().StringVar(&status, "status", "", "Filter by status (client-side scan; may page through many tasks)")
	cmd.Flags().StringVar(&transactionID, "transaction", "", "Filter by kontxt transaction ID (client-side scan)")
	cmd.Flags().IntVar(&limit, "limit", 20, "Maximum number of results")
	cmd.Flags().StringVar(&continueToken, "continue", "", "Continue token for the next page")
	cmd.Flags().StringVar(&continueToken, "cursor", "", "Cursor token for the next page")
	addOutputFlag(cmd, outputTable)

	return cmd
}

func newTaskGetCmd() *cobra.Command {
	var showTransaction bool

	cmd := &cobra.Command{
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

			if showTransaction {
				transaction, ok := taskTransaction(*detail)
				if !ok {
					fmt.Println("No transaction metadata found.")
					return nil
				}
				out, err := json.MarshalIndent(transaction, "", "  ")
				if err != nil {
					return fmt.Errorf("formatting transaction output: %w", err)
				}
				fmt.Println(string(out))
				return nil
			}

			return printStructured(cmd, detail)
		},
	}

	cmd.Flags().BoolVar(&showTransaction, "show-transaction", false, "Show only transaction metadata")
	addOutputFlag(cmd, outputJSON)
	return cmd
}

func taskTransaction(detail client.TaskDetail) (map[string]any, bool) {
	spec, ok := detail["spec"].(map[string]any)
	if !ok {
		return nil, false
	}
	transaction, ok := spec["transaction"].(map[string]any)
	if !ok || len(transaction) == 0 {
		return nil, false
	}
	return transaction, true
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

func newTaskResultCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "result <name>",
		Short: "Get task result",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			result, err := c.GetTaskResult(context.Background(), args[0], client.GetOptions{Namespace: c.Namespace})
			if err != nil {
				return err
			}
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			if format == outputTable {
				fmt.Fprint(cmd.OutOrStdout(), result.Result) //nolint:errcheck
				if !strings.HasSuffix(result.Result, "\n") {
					fmt.Fprintln(cmd.OutOrStdout()) //nolint:errcheck
				}
				return nil
			}
			return printStructured(cmd, result)
		},
	}
	addOutputFlag(cmd, outputTable)
	return cmd
}

func newTaskPlanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plan <name>",
		Short: "Get task autonomous plan state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			result, err := c.GetTaskPlan(context.Background(), args[0], client.GetOptions{Namespace: c.Namespace})
			if err != nil {
				return err
			}
			return printStructured(cmd, result)
		},
	}
	addOutputFlag(cmd, outputJSON)
	return cmd
}

func newTaskChildrenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "children <name>",
		Short: "List child tasks",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			result, err := c.GetTaskChildren(context.Background(), args[0], client.GetOptions{Namespace: c.Namespace})
			if err != nil {
				return err
			}
			return printStructured(cmd, result)
		},
	}
	addOutputFlag(cmd, outputTable)
	return cmd
}

func newTaskWaitCmd() *cobra.Command {
	var timeout string
	cmd := &cobra.Command{
		Use:   "wait <name>",
		Short: "Wait for a task to complete",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var deadline <-chan time.Time
			if timeout != "" {
				d, err := time.ParseDuration(timeout)
				if err != nil {
					return fmt.Errorf("invalid timeout: %w", err)
				}
				deadline = time.After(d)
			}
			c := newClientFromCmd(cmd)
			ticker := time.NewTicker(2 * time.Second)
			defer ticker.Stop()
			for {
				detail, err := c.GetTask(context.Background(), args[0], client.GetOptions{Namespace: c.Namespace})
				if err != nil {
					return err
				}
				phase := client.StringField(*detail, "status", "phase")
				switch strings.ToLower(phase) {
				case "succeeded":
					fmt.Fprintf(cmd.OutOrStdout(), "Task %s succeeded.\n", args[0]) //nolint:errcheck
					return nil
				case "failed", "cancelled":
					return fmt.Errorf("task %s finished with phase %s", args[0], phase)
				}
				select {
				case <-deadline:
					return fmt.Errorf("timed out waiting for task %s", args[0])
				case <-ticker.C:
				}
			}
		},
	}
	cmd.Flags().StringVar(&timeout, "timeout", "", "Maximum time to wait (e.g. 5m)")
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
