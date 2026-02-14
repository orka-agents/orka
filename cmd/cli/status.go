/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/sozercan/mercan/internal/cli/client"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show system overview (health, tasks, agents)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := newClientFromCmd(cmd)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			fmt.Println("Mercan Status")
			fmt.Println("─────────────")

			healthy, healthErr := c.HealthCheck(ctx)
			if healthErr != nil {
				fmt.Printf("  Health:  ✗ Unreachable (%v)\n", healthErr)
			} else if healthy {
				fmt.Println("  Health:  ✓ Healthy")
			} else {
				fmt.Println("  Health:  ✗ Unhealthy")
			}

			ready, readyErr := c.ReadyCheck(ctx)
			if readyErr != nil {
				fmt.Printf("  Ready:   ✗ Unreachable (%v)\n", readyErr)
			} else if ready {
				fmt.Println("  Ready:   ✓ Ready")
			} else {
				fmt.Println("  Ready:   ✗ Not Ready")
			}

			if healthErr != nil {
				fmt.Fprintln(os.Stderr, "\nCannot reach the server. Skipping task and agent queries.")
				return nil
			}

			fmt.Println()

			tasks, err := c.ListTasks(ctx, client.ListTasksOptions{Limit: 100})
			if err != nil {
				fmt.Fprintf(os.Stderr, "  Tasks:   ✗ Error (%v)\n", err)
			} else {
				counts := map[string]int{
					"Pending": 0, "Running": 0, "Succeeded": 0, "Failed": 0, "Scheduled": 0,
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

				// Show autonomous tasks
				var autonomousTasks []client.TaskSummary
				for _, t := range tasks {
					if t.Iteration > 0 {
						autonomousTasks = append(autonomousTasks, t)
					}
				}
				if len(autonomousTasks) > 0 {
					fmt.Println()
					fmt.Println("  Autonomous Tasks:")
					for _, t := range autonomousTasks {
						fmt.Printf("    %s: iteration %d (%s)\n", t.Name, t.Iteration, t.Phase)
					}
				}
			}

			fmt.Println()

			agents, err := c.ListAgents(ctx, client.ListOptions{})
			if err != nil {
				fmt.Fprintf(os.Stderr, "  Agents:  ✗ Error (%v)\n", err)
			} else {
				fmt.Printf("  Agents:    %d\n", len(agents))
			}

			return nil
		},
	}
}
