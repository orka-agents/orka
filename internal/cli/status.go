/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/sozercan/mercan/internal/cli/client"
)

// StatusOptions holds configuration for the status command.
type StatusOptions struct {
	Server string
	Token  string
}

// RunStatus displays a system overview.
func RunStatus(opts StatusOptions) {
	if opts.Server == "" {
		opts.Server = defaultServer
	}

	c := client.New(opts.Server, opts.Token)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	fmt.Println("Mercan Status")
	fmt.Println("─────────────")

	// Health check
	healthy, healthErr := c.HealthCheck(ctx)
	if healthErr != nil {
		fmt.Printf("  Health:  ✗ Unreachable (%v)\n", healthErr)
	} else if healthy {
		fmt.Println("  Health:  ✓ Healthy")
	} else {
		fmt.Println("  Health:  ✗ Unhealthy")
	}

	// Ready check
	ready, readyErr := c.ReadyCheck(ctx)
	if readyErr != nil {
		fmt.Printf("  Ready:   ✗ Unreachable (%v)\n", readyErr)
	} else if ready {
		fmt.Println("  Ready:   ✓ Ready")
	} else {
		fmt.Println("  Ready:   ✗ Not Ready")
	}

	// If health checks failed due to connection error, skip API calls
	if healthErr != nil {
		fmt.Fprintln(os.Stderr, "\nCannot reach the server. Skipping task and agent queries.")
		os.Exit(1)
	}

	fmt.Println()

	// Tasks
	tasks, err := c.ListTasks(ctx, client.ListTasksOptions{Limit: 100})
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Tasks:   ✗ Error (%v)\n", err)
	} else {
		counts := map[string]int{
			"Pending":   0,
			"Running":   0,
			"Succeeded": 0,
			"Failed":    0,
			"Scheduled": 0,
		}
		for _, t := range tasks {
			counts[t.Phase]++
		}
		fmt.Println("  Tasks:")
		fmt.Printf("    Pending:    %d\n", counts["Pending"])
		fmt.Printf("    Running:    %d\n", counts["Running"])
		fmt.Printf("    Succeeded:  %d\n", counts["Succeeded"])
		fmt.Printf("    Failed:     %d\n", counts["Failed"])
		if counts["Scheduled"] > 0 {
			fmt.Printf("    Scheduled:  %d\n", counts["Scheduled"])
		}
	}

	fmt.Println()

	// Agents
	agents, err := c.ListAgents(ctx, client.ListOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "  Agents:  ✗ Error (%v)\n", err)
	} else {
		fmt.Printf("  Agents:    %d\n", len(agents))
	}
}
