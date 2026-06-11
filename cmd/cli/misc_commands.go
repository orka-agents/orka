package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/sozercan/orka/internal/cli/client"
)

func newAuthCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "auth", Short: "Inspect authentication"}
	validate := &cobra.Command{
		Use:   "validate",
		Short: "Validate current credentials",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := newClientFromCmd(cmd)
			result, err := c.AuthValidate(context.Background())
			if err != nil {
				return err
			}
			return printStructured(cmd, result)
		},
	}
	addOutputFlag(validate, outputJSON)
	whoami := &cobra.Command{
		Use:   "whoami",
		Short: "Show sanitized authenticated identity",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := newClientFromCmd(cmd)
			result, err := c.AuthWhoAmI(context.Background())
			if err != nil {
				return err
			}
			return printStructured(cmd, result)
		},
	}
	addOutputFlag(whoami, outputJSON)
	cmd.AddCommand(validate, whoami)
	return cmd
}

func newModelsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "models", Short: "List compatible model IDs"}
	var compat string
	list := &cobra.Command{
		Use:   "list",
		Short: "List model IDs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if compat != "openai" && compat != "anthropic" {
				return fmt.Errorf("--compat must be openai or anthropic")
			}
			c := newClientFromCmd(cmd)
			result, err := c.ListModels(context.Background(), compat)
			if err != nil {
				return err
			}
			return printStructured(cmd, result)
		},
	}
	addOutputFlag(list, outputTable)
	list.Flags().StringVar(&compat, "compat", "openai", "Compatibility surface: openai or anthropic")
	cmd.AddCommand(list)
	return cmd
}

func newWorkspaceCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "workspace", Short: "Inspect task workspace status"}
	status := &cobra.Command{
		Use:   "status <task>",
		Short: "Show safe workspace status fields",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			detail, err := c.GetTask(context.Background(), args[0], client.GetOptions{Namespace: c.Namespace})
			if err != nil {
				return err
			}
			status := safeWorkspaceStatus(*detail)
			return printStructured(cmd, status)
		},
	}
	addOutputFlag(status, outputJSON)
	cmd.AddCommand(status)
	return cmd
}

func safeWorkspaceStatus(task client.TaskDetail) map[string]any {
	out := map[string]any{
		"task":      client.StringField(task, "metadata", "name"),
		"namespace": client.StringField(task, "metadata", "namespace"),
	}
	status, _ := task["status"].(map[string]any)
	if status == nil {
		return out
	}
	out["phase"] = status["phase"]
	if ew, ok := status["executionWorkspace"].(map[string]any); ok {
		safe := map[string]any{}
		for _, key := range []string{
			"phase",
			"provider",
			"reason",
			"message",
			"url",
			"lastTransitionTime",
			"observedGeneration",
		} {
			if v, ok := ew[key]; ok {
				safe[key] = v
			}
		}
		out["executionWorkspace"] = safe
	}
	if ws, ok := status["workspace"].(map[string]any); ok {
		safe := map[string]any{}
		for _, key := range []string{"phase", "provider", "reason", "message", "url", "lastTransitionTime"} {
			if v, ok := ws[key]; ok {
				safe[key] = v
			}
		}
		out["workspace"] = safe
	}
	return out
}
