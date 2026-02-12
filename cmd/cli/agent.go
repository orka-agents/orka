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

	"github.com/sozercan/mercan/internal/cli/client"
)

func agentCmd(args []string) {
	if len(args) < 1 {
		printAgentUsage()
		os.Exit(1)
	}

	switch args[0] {
	case "list":
		agentListCmd(args[1:])
	case "get":
		agentGetCmd(args[1:])
	case flagHelp, "-h":
		printAgentUsage()
	default:
		fmt.Fprintf(os.Stderr, "Unknown agent subcommand: %s\n", args[0])
		printAgentUsage()
		os.Exit(1)
	}
}

func printAgentUsage() {
	fmt.Fprint(os.Stderr, `Usage: mercan agent <command>

Commands:
  list    List agents
  get     Get agent details

Run 'mercan agent <command> --help' for more information.
`)
}

func agentListCmd(args []string) {
	var namespace, server, token string
	var help bool

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case flagNamespace, "-n":
			if i+1 < len(args) {
				i++
				namespace = args[i]
			}
		case flagServer, "-s":
			if i+1 < len(args) {
				i++
				server = args[i]
			}
		case flagToken, "-t":
			if i+1 < len(args) {
				i++
				token = args[i]
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
		fmt.Print(`Usage: mercan agent list [flags]

List all agents.

Flags:
  -n, --namespace string   Filter by namespace
  -s, --server string      Mercan server URL (default defaultServer)
  -t, --token string       Authentication token
  -h, --help               Show this help message
`)
		return
	}

	if server == "" {
		server = defaultServer
	}

	c := client.New(server, token)
	agents, err := c.ListAgents(context.Background(), client.ListOptions{
		Namespace: namespace,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	if len(agents) == 0 {
		fmt.Println("No agents found.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tMODEL\tRUNTIME\tACTIVE TASKS") //nolint:errcheck
	for _, a := range agents {
		model := a.Model
		if model == "" {
			model = "-"
		}
		runtime := a.Runtime
		if runtime == "" {
			runtime = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\n", a.Name, model, runtime, a.Active) //nolint:errcheck
	}
	w.Flush() //nolint:errcheck
}

func agentGetCmd(args []string) {
	var namespace, server, token string
	var help bool
	var name string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case flagNamespace, "-n":
			if i+1 < len(args) {
				i++
				namespace = args[i]
			}
		case flagServer, "-s":
			if i+1 < len(args) {
				i++
				server = args[i]
			}
		case flagToken, "-t":
			if i+1 < len(args) {
				i++
				token = args[i]
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
		fmt.Print(`Usage: mercan agent get <name> [flags]

Get details for a specific agent.

Flags:
  -n, --namespace string   Agent namespace (default "default")
  -s, --server string      Mercan server URL (default defaultServer)
  -t, --token string       Authentication token
  -h, --help               Show this help message
`)
		return
	}

	if name == "" {
		fmt.Fprintln(os.Stderr, "Error: agent name is required")
		fmt.Fprintln(os.Stderr, "Usage: mercan agent get <name> [flags]")
		os.Exit(1)
	}

	if server == "" {
		server = defaultServer
	}

	c := client.New(server, token)
	agent, err := c.GetAgent(context.Background(), name, client.GetOptions{
		Namespace: namespace,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	out, err := json.MarshalIndent(agent, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error formatting output: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(out))
}
