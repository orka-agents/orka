package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"
)

func newMonitorCmd() *cobra.Command {
	cmd := newCRUDResourceCmd(crudResourceSpec{
		Use:      "monitor",
		Short:    "Manage repository monitors",
		BasePath: "/api/v1/monitors/repositories",
		Name:     "repository monitor",
	})
	cmd.AddCommand(newMonitorRunCmd())
	cmd.AddCommand(newMonitorRunsCmd())
	cmd.AddCommand(newMonitorItemsCmd())
	cmd.AddCommand(newMonitorEventsCmd())
	return cmd
}

func newMonitorRunCmd() *cobra.Command {
	var targetKind, targetSHA string
	var targetNumber int64
	cmd := &cobra.Command{
		Use:   "run <name>",
		Short: "Trigger a manual repository monitor run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, _ := json.Marshal(map[string]any{
				"targetKind":   targetKind,
				"targetNumber": targetNumber,
				"targetSHA":    targetSHA,
			})
			c := newClientFromCmd(cmd)
			path := "/api/v1/monitors/repositories/" + url.PathEscape(args[0]) + "/runs"
			result, err := c.DoJSON(context.Background(), http.MethodPost, path, nil, body)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Repository monitor run created: %s\n", metadataName(result)) //nolint:errcheck
			return nil
		},
	}
	cmd.Flags().StringVar(&targetKind, "target-kind", "", "Target kind (e.g. pull_request)")
	cmd.Flags().Int64Var(&targetNumber, "target-number", 0, "Target number")
	cmd.Flags().StringVar(&targetSHA, "target-sha", "", "Target commit SHA")
	return cmd
}

func newMonitorRunsCmd() *cobra.Command {
	var limit int
	var cursor, trigger, targetKind string
	cmd := &cobra.Command{
		Use:   "runs <name>",
		Short: "List repository monitor runs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := mergeQuery(
				map[string]string{},
				"limit", fmt.Sprintf("%d", limit),
				"cursor", cursor,
				"continue", cursor,
				"trigger", trigger,
				"targetKind", targetKind,
			)
			c := newClientFromCmd(cmd)
			path := "/api/v1/monitors/repositories/" + url.PathEscape(args[0]) + "/runs"
			result, err := c.DoJSON(context.Background(), http.MethodGet, path, q, nil)
			if err != nil {
				return err
			}
			return printStructured(cmd, result)
		},
	}
	addOutputFlag(cmd, outputTable)
	cmd.Flags().IntVar(&limit, "limit", 20, "Maximum number of results")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Cursor token")
	cmd.Flags().StringVar(&cursor, "continue", "", "Continue token")
	cmd.Flags().StringVar(&trigger, "trigger", "", "Filter by trigger")
	cmd.Flags().StringVar(&targetKind, "target-kind", "", "Filter by target kind")
	return cmd
}

func newMonitorItemsCmd() *cobra.Command {
	var limit int
	var cursor, kind, state, verdict, repairState, automergeState string
	cmd := &cobra.Command{
		Use:   "items <name>",
		Short: "List repository monitor items",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := mergeQuery(
				map[string]string{},
				"limit", fmt.Sprintf("%d", limit),
				"cursor", cursor,
				"continue", cursor,
				"kind", kind,
				"state", state,
				"verdict", verdict,
				"repairState", repairState,
				"automergeState", automergeState,
			)
			c := newClientFromCmd(cmd)
			path := "/api/v1/monitors/repositories/" + url.PathEscape(args[0]) + "/items"
			result, err := c.DoJSON(context.Background(), http.MethodGet, path, q, nil)
			if err != nil {
				return err
			}
			return printStructured(cmd, result)
		},
	}
	addOutputFlag(cmd, outputTable)
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum number of results")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Cursor token")
	cmd.Flags().StringVar(&cursor, "continue", "", "Continue token")
	cmd.Flags().StringVar(&kind, "kind", "", "Filter by item kind")
	cmd.Flags().StringVar(&state, "state", "", "Filter by state")
	cmd.Flags().StringVar(&verdict, "verdict", "", "Filter by review verdict")
	cmd.Flags().StringVar(&repairState, "repair-state", "", "Filter by repair state")
	cmd.Flags().StringVar(&automergeState, "automerge-state", "", "Filter by automerge state")
	return cmd
}

func newMonitorEventsCmd() *cobra.Command {
	var monitorName, runID, itemKind, eventType, cursor string
	var itemNumber int64
	var limit int
	cmd := &cobra.Command{
		Use:   "events [name]",
		Short: "List repository monitor events",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				monitorName = args[0]
			}
			if monitorName == "" {
				return fmt.Errorf("monitor name is required")
			}
			q := mergeQuery(
				map[string]string{},
				"name", monitorName,
				"limit", fmt.Sprintf("%d", limit),
				"cursor", cursor,
				"continue", cursor,
				"runID", runID,
				"itemKind", itemKind,
				"eventType", eventType,
			)
			if itemNumber > 0 {
				q["itemNumber"] = fmt.Sprintf("%d", itemNumber)
			}
			c := newClientFromCmd(cmd)
			result, err := c.DoJSON(context.Background(), http.MethodGet, "/api/v1/monitors/events", q, nil)
			if err != nil {
				return err
			}
			return printStructured(cmd, result)
		},
	}
	addOutputFlag(cmd, outputTable)
	cmd.Flags().StringVar(&monitorName, "name", "", "Repository monitor name")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum number of results")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Cursor token")
	cmd.Flags().StringVar(&cursor, "continue", "", "Continue token")
	cmd.Flags().StringVar(&runID, "run-id", "", "Filter by run ID")
	cmd.Flags().StringVar(&itemKind, "item-kind", "", "Filter by item kind")
	cmd.Flags().Int64Var(&itemNumber, "item-number", 0, "Filter by item number")
	cmd.Flags().StringVar(&eventType, "event-type", "", "Filter by event type")
	return cmd
}
