/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package cli

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

	"github.com/sozercan/mercan/internal/cli/client"
)

const (
	flagServer    = "--server"
	flagToken     = "--token"
	flagHelp      = "--help"
	flagNamespace = "--namespace"
	defaultServer = "http://localhost:8080"
	defaultNS     = "default"
)

// TaskOptions holds shared configuration for task commands.
type TaskOptions struct {
	Server    string
	Token     string
	Namespace string
}

// RunTaskCmd dispatches the task subcommand.
func RunTaskCmd(args []string) {
	if len(args) == 0 {
		printTaskUsage()
		os.Exit(1)
	}

	switch args[0] {
	case "create":
		taskCreateCmd(args[1:])
	case "list", "ls":
		taskListCmd(args[1:])
	case "get":
		taskGetCmd(args[1:])
	case "logs":
		taskLogsCmd(args[1:])
	case "delete", "rm":
		taskDeleteCmd(args[1:])
	case flagHelp, "-h":
		printTaskUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown task command: %s\n", args[0])
		printTaskUsage()
		os.Exit(1)
	}
}

func printTaskUsage() {
	fmt.Fprintf(os.Stderr, `Usage: mercan task <command> [flags]

Commands:
  create     Create a new task
  list       List tasks
  get        Get task details
  logs       Get task logs
  delete     Delete a task

Run 'mercan task <command> --help' for more information.
`)
}

func taskCreateCmd(args []string) {
	var opts TaskOptions
	var taskType, agent, provider, timeout string
	var help bool
	var positional []string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case flagServer, "-s":
			if i+1 < len(args) {
				i++
				opts.Server = args[i]
			}
		case flagToken, "-t":
			if i+1 < len(args) {
				i++
				opts.Token = args[i]
			}
		case flagNamespace, "-n":
			if i+1 < len(args) {
				i++
				opts.Namespace = args[i]
			}
		case "--type":
			if i+1 < len(args) {
				i++
				taskType = args[i]
			}
		case "--agent":
			if i+1 < len(args) {
				i++
				agent = args[i]
			}
		case "--provider":
			if i+1 < len(args) {
				i++
				provider = args[i]
			}
		case "--timeout":
			if i+1 < len(args) {
				i++
				timeout = args[i]
			}
		case flagHelp, "-h":
			help = true
		default:
			if len(args[i]) > 0 && args[i][0] == '-' {
				fmt.Fprintf(os.Stderr, "Unknown flag: %s\n", args[i])
				os.Exit(1)
			}
			positional = append(positional, args[i])
		}
	}

	if help {
		fmt.Print(`Usage: mercan task create [flags] <prompt>

Create a new task from a prompt.

Flags:
  -s, --server string       Mercan server URL (default defaultServer)
  -t, --token string        Bearer token for authentication
  -n, --namespace string    Kubernetes namespace (default "default")
      --type string         Task type: ai, container, agent (default "ai")
      --agent string        Agent reference name
      --provider string     Provider reference name (default "default")
      --timeout string      Task timeout (e.g., "5m", "1h")
  -h, --help                Show this help message
`)
		return
	}

	if len(positional) == 0 {
		fmt.Fprintln(os.Stderr, "Error: prompt is required")
		fmt.Fprintln(os.Stderr, "Usage: mercan task create [flags] <prompt>")
		os.Exit(1)
	}

	applyDefaults(&opts)
	if taskType == "" {
		taskType = "ai"
	}
	if provider == "" {
		provider = "default"
	}

	prompt := strings.Join(positional, " ")
	taskName := generateTaskName()

	req := client.CreateTaskRequest{
		Name:      taskName,
		Namespace: opts.Namespace,
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
			Provider struct {
				Name string `json:"name"`
			} `json:"provider"`
		}{
			Provider: struct {
				Name string `json:"name"`
			}{Name: provider},
		}
	}

	c := client.New(opts.Server, opts.Token)
	result, err := c.CreateTask(context.Background(), req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	name := client.StringField(*result, "metadata", "name")
	fmt.Printf("Task created: %s\n", name)
}

func taskListCmd(args []string) {
	var opts TaskOptions
	var status string
	var limit int
	var help bool

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case flagServer, "-s":
			if i+1 < len(args) {
				i++
				opts.Server = args[i]
			}
		case flagToken, "-t":
			if i+1 < len(args) {
				i++
				opts.Token = args[i]
			}
		case flagNamespace, "-n":
			if i+1 < len(args) {
				i++
				opts.Namespace = args[i]
			}
		case "--status":
			if i+1 < len(args) {
				i++
				status = args[i]
			}
		case "--limit":
			if i+1 < len(args) {
				i++
				fmt.Sscanf(args[i], "%d", &limit) //nolint:errcheck
			}
		case flagHelp, "-h":
			help = true
		default:
			if len(args[i]) > 0 && args[i][0] == '-' {
				fmt.Fprintf(os.Stderr, "Unknown flag: %s\n", args[i])
				os.Exit(1)
			}
		}
	}

	if help {
		fmt.Print(`Usage: mercan task list [flags]

List tasks.

Flags:
  -s, --server string       Mercan server URL (default defaultServer)
  -t, --token string        Bearer token for authentication
  -n, --namespace string    Kubernetes namespace
      --status string       Filter by status (Pending, Running, Succeeded, Failed)
      --limit int           Maximum number of results (default 20)
  -h, --help                Show this help message
`)
		return
	}

	applyDefaults(&opts)
	if limit == 0 {
		limit = 20
	}

	c := client.New(opts.Server, opts.Token)
	tasks, err := c.ListTasks(context.Background(), client.ListTasksOptions{
		Namespace: opts.Namespace,
		Limit:     limit,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Filter by status client-side if specified
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
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tTYPE\tSTATUS\tAGE") //nolint:errcheck
	for _, t := range tasks {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", t.Name, t.Type, t.Phase, formatAge(t.Age)) //nolint:errcheck
	}
	w.Flush() //nolint:errcheck
}

func taskGetCmd(args []string) {
	var opts TaskOptions
	var help bool
	var name string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case flagServer, "-s":
			if i+1 < len(args) {
				i++
				opts.Server = args[i]
			}
		case flagToken, "-t":
			if i+1 < len(args) {
				i++
				opts.Token = args[i]
			}
		case flagNamespace, "-n":
			if i+1 < len(args) {
				i++
				opts.Namespace = args[i]
			}
		case flagHelp, "-h":
			help = true
		default:
			if len(args[i]) > 0 && args[i][0] == '-' {
				fmt.Fprintf(os.Stderr, "Unknown flag: %s\n", args[i])
				os.Exit(1)
			}
			if name == "" {
				name = args[i]
			}
		}
	}

	if help {
		fmt.Print(`Usage: mercan task get <name> [flags]

Get detailed information about a task.

Flags:
  -s, --server string       Mercan server URL (default defaultServer)
  -t, --token string        Bearer token for authentication
  -n, --namespace string    Kubernetes namespace (default "default")
  -h, --help                Show this help message
`)
		return
	}

	if name == "" {
		fmt.Fprintln(os.Stderr, "Error: task name is required")
		fmt.Fprintln(os.Stderr, "Usage: mercan task get <name>")
		os.Exit(1)
	}

	applyDefaults(&opts)

	c := client.New(opts.Server, opts.Token)
	detail, err := c.GetTask(context.Background(), name, client.GetOptions{
		Namespace: opts.Namespace,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	out, err := json.MarshalIndent(detail, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error formatting output: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(out))
}

func taskLogsCmd(args []string) {
	var opts TaskOptions
	var help bool
	var follow bool
	var name string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case flagServer, "-s":
			if i+1 < len(args) {
				i++
				opts.Server = args[i]
			}
		case flagToken, "-t":
			if i+1 < len(args) {
				i++
				opts.Token = args[i]
			}
		case flagNamespace, "-n":
			if i+1 < len(args) {
				i++
				opts.Namespace = args[i]
			}
		case "--follow", "-f":
			follow = true
		case flagHelp, "-h":
			help = true
		default:
			if len(args[i]) > 0 && args[i][0] == '-' {
				fmt.Fprintf(os.Stderr, "Unknown flag: %s\n", args[i])
				os.Exit(1)
			}
			if name == "" {
				name = args[i]
			}
		}
	}

	if help {
		fmt.Print(`Usage: mercan task logs <name> [flags]

Get logs for a task.

Flags:
  -f, --follow              Stream logs in real time (for running tasks)
  -s, --server string       Mercan server URL (default defaultServer)
  -t, --token string        Bearer token for authentication
  -n, --namespace string    Kubernetes namespace (default "default")
  -h, --help                Show this help message
`)
		return
	}

	if name == "" {
		fmt.Fprintln(os.Stderr, "Error: task name is required")
		fmt.Fprintln(os.Stderr, "Usage: mercan task logs <name>")
		os.Exit(1)
	}

	applyDefaults(&opts)

	c := client.New(opts.Server, opts.Token)

	if follow {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()

		err := c.StreamTaskLogs(ctx, name, client.StreamLogsOptions{
			Namespace: opts.Namespace,
			Writer:    os.Stdout,
		})
		if err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	logsResp, err := c.GetTaskLogs(context.Background(), name, client.GetOptions{
		Namespace: opts.Namespace,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if logsResp.Logs != "" {
		fmt.Print(logsResp.Logs)
	} else if logsResp.Message != "" {
		fmt.Println(logsResp.Message)
	}
}

func taskDeleteCmd(args []string) {
	var opts TaskOptions
	var help bool
	var name string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case flagServer, "-s":
			if i+1 < len(args) {
				i++
				opts.Server = args[i]
			}
		case flagToken, "-t":
			if i+1 < len(args) {
				i++
				opts.Token = args[i]
			}
		case flagNamespace, "-n":
			if i+1 < len(args) {
				i++
				opts.Namespace = args[i]
			}
		case flagHelp, "-h":
			help = true
		default:
			if len(args[i]) > 0 && args[i][0] == '-' {
				fmt.Fprintf(os.Stderr, "Unknown flag: %s\n", args[i])
				os.Exit(1)
			}
			if name == "" {
				name = args[i]
			}
		}
	}

	if help {
		fmt.Print(`Usage: mercan task delete <name> [flags]

Delete a task.

Flags:
  -s, --server string       Mercan server URL (default defaultServer)
  -t, --token string        Bearer token for authentication
  -n, --namespace string    Kubernetes namespace (default "default")
  -h, --help                Show this help message
`)
		return
	}

	if name == "" {
		fmt.Fprintln(os.Stderr, "Error: task name is required")
		fmt.Fprintln(os.Stderr, "Usage: mercan task delete <name>")
		os.Exit(1)
	}

	applyDefaults(&opts)

	c := client.New(opts.Server, opts.Token)
	err := c.DeleteTask(context.Background(), name, client.GetOptions{
		Namespace: opts.Namespace,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Task deleted: %s\n", name)
}

func applyDefaults(opts *TaskOptions) {
	if opts.Server == "" {
		opts.Server = defaultServer
	}
	if opts.Namespace == "" {
		opts.Namespace = defaultNS
	}
}

func generateTaskName() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return "task-" + hex.EncodeToString(b)
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
