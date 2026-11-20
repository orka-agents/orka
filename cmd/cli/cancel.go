// Copyright (c) 2026. MIT License - see LICENSE file for details.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"
)

// cancelReasonKey is both the --reason flag and the request body field.
const cancelReasonKey = "reason"

// newTaskCancelCmd stops a Pending or Running Task without deleting it.
func newTaskCancelCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "cancel <name>",
		Short: "Stop a running task and keep its history",
		Long: `Stop a Pending or Running task. Unlike delete, cancel keeps the task's
result, artifacts, plan and events. Cancelling an already cancelled task
changes nothing.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			body, _ := json.Marshal(map[string]string{cancelReasonKey: reason})
			result, err := c.DoJSON(context.Background(), http.MethodPost,
				"/api/v1/tasks/"+url.PathEscape(args[0])+"/cancel", nil, body)
			if err != nil {
				return err
			}
			printTaskCancellation(cmd.OutOrStdout(), result)
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, cancelReasonKey, "", "Why the task is being cancelled")
	return cmd
}

// newSessionCancelCmd stops a Session's current turn. The Session stays
// usable for the next message.
func newSessionCancelCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "cancel <name>",
		Short: "Stop a session's current turn",
		Long: `Stop the session's in-flight chat request, if any, and cancel its
Pending and Running tasks. The session keeps its history and accepts the next
message.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			body, _ := json.Marshal(map[string]string{cancelReasonKey: reason})
			result, err := c.DoJSON(context.Background(), http.MethodPost,
				"/api/v1/sessions/"+url.PathEscape(args[0])+"/cancel", nil, body)
			if err != nil {
				return err
			}
			fields, _ := result.(map[string]any)
			if stopped, _ := fields["chatTurnStopped"].(bool); stopped {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Chat turn stopped: %s\n", args[0])
			}
			tasks, _ := fields["tasks"].([]any)
			for _, task := range tasks {
				printTaskCancellation(cmd.OutOrStdout(), task)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, cancelReasonKey, "", "Why the turn is being stopped")
	return cmd
}

func printTaskCancellation(out io.Writer, result any) {
	fields, _ := result.(map[string]any)
	name, _ := fields["name"].(string)
	if requested, _ := fields["requested"].(bool); requested {
		_, _ = fmt.Fprintf(out, "Task cancellation requested: %s\n", name)
		return
	}
	_, _ = fmt.Fprintf(out, "Task already cancelling or cancelled: %s\n", name)
}
