package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

func newMemoryCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "memory", Short: "Manage durable memory"}
	cmd.AddCommand(newMemoryListCmd())
	cmd.AddCommand(newMemoryGetCmd())
	cmd.AddCommand(newMemoryCreateCmd())
	cmd.AddCommand(newMemoryUpdateCmd())
	cmd.AddCommand(newMemoryDeleteCmd())
	cmd.AddCommand(newMemoryEnableDisableCmd("enable"))
	cmd.AddCommand(newMemoryEnableDisableCmd("disable"))
	cmd.AddCommand(newMemoryProposalCmd())
	cmd.AddCommand(newMemoryOperationCmd())
	cmd.AddCommand(newMemoryBackendCmd())
	return cmd
}

func addMemoryFilterFlags(
	cmd *cobra.Command,
	values map[string]*string,
	includeDisabled, includeDeleted *bool,
	limit *int,
) {
	for _, key := range []string{"query", "sessionName", "agentName", "taskName", "parentTask", "source", "tags", "ids"} {
		v := ""
		values[key] = &v
		cmd.Flags().StringVar(values[key], key, "", "Filter by "+key)
	}
	cmd.Flags().BoolVar(includeDisabled, "include-disabled", false, "Include disabled memories")
	cmd.Flags().BoolVar(includeDeleted, "include-deleted", false, "Include deleted memories")
	cmd.Flags().IntVar(limit, "limit", 100, "Maximum number of results")
}

func newMemoryListCmd() *cobra.Command {
	values := map[string]*string{}
	var includeDisabled, includeDeleted bool
	var cursor, continueToken string
	var limit int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List memories",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cursorValue := strings.TrimSpace(cursor)
			continueValue := strings.TrimSpace(continueToken)
			if cmd.Flags().Changed("cursor") && cmd.Flags().Changed("continue") && cursorValue != continueValue {
				return fmt.Errorf("--cursor and --continue must match when both are set")
			}
			if cursorValue == "" {
				cursorValue = continueValue
			}
			q := map[string]string{"limit": fmt.Sprintf("%d", limit)}
			for _, key := range sortedKeys(ptrStringMap(values)) {
				if v := strings.TrimSpace(*values[key]); v != "" {
					q[key] = v
				}
			}
			if includeDisabled {
				q["includeDisabled"] = cliQueryTrue
			}
			if includeDeleted {
				q["includeDeleted"] = cliQueryTrue
			}
			if cursorValue != "" {
				q["cursor"] = cursorValue
			}
			c := newClientFromCmd(cmd)
			result, err := c.DoJSON(context.Background(), http.MethodGet, "/api/v1/memories", q, nil)
			if err != nil {
				return err
			}
			return printMemoryList(cmd, result)
		},
	}
	addOutputFlag(cmd, outputTable)
	addMemoryFilterFlags(cmd, values, &includeDisabled, &includeDeleted, &limit)
	cmd.Flags().StringVar(&cursor, "cursor", "", "Cursor token for the next page")
	cmd.Flags().StringVar(&continueToken, "continue", "", "Continue/cursor token for the next page")
	return cmd
}

func printMemoryList(cmd *cobra.Command, result any) error {
	format, err := outputFormat(cmd)
	if err != nil {
		return err
	}
	if err := printStructured(cmd, result); err != nil {
		return err
	}
	if format != outputTable {
		return nil
	}
	response, ok := result.(map[string]any)
	if !ok {
		return nil
	}
	next := strings.TrimSpace(nestedString(response, "metadata", "continue"))
	if next != "" {
		fmt.Fprintf( //nolint:errcheck
			cmd.OutOrStdout(),
			"Continue with --cursor %s (reuse the same filters and --limit).\n",
			next,
		)
	}
	return nil
}

func ptrStringMap(in map[string]*string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		if v != nil {
			out[k] = *v
		}
	}
	return out
}

func newMemoryGetCmd() *cobra.Command {
	var includeDisabled bool
	cmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Get a memory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			var query map[string]string
			if includeDisabled {
				query = map[string]string{"includeDisabled": cliQueryTrue}
			}
			result, err := c.DoJSON(
				context.Background(),
				http.MethodGet,
				"/api/v1/memories/"+url.PathEscape(args[0]),
				query,
				nil,
			)
			if err != nil {
				return err
			}
			return printStructured(cmd, result)
		},
	}
	cmd.Flags().BoolVar(&includeDisabled, "include-disabled", false, "Include disabled memory")
	addOutputFlag(cmd, outputJSON)
	return cmd
}

func newMemoryCreateCmd() *cobra.Command {
	var file, content, source, tags string
	var mutation memoryMutationOptions
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a memory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var body []byte
			var err error
			c := newClientFromCmd(cmd)
			if file != "" {
				body, err = manifestWithNamespaceJSON(cmd, file, c.Namespace)
			} else {
				if strings.TrimSpace(content) == "" {
					return fmt.Errorf("--content or --file is required")
				}
				if source == "" {
					source = "cli"
				}
				body, err = json.Marshal(map[string]any{
					"namespace": c.Namespace,
					"content":   content,
					"source":    source,
					"tags":      splitComma(tags),
				})
			}
			if err != nil {
				return err
			}
			response, err := executeMemoryMutation(cmd, c, http.MethodPost, "/api/v1/memories", nil, body, mutation)
			if err != nil {
				return err
			}
			return printMemoryMutationOutcome(cmd, "Memory created", "", response)
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "Path to memory JSON/YAML body")
	cmd.Flags().StringVar(&content, "content", "", "Memory content")
	cmd.Flags().StringVar(&source, "source", "cli", "Memory source")
	cmd.Flags().StringVar(&tags, "tags", "", "Comma-separated tags")
	addMemoryMutationFlags(cmd, &mutation)
	return cmd
}

func newMemoryUpdateCmd() *cobra.Command {
	var file, content, source, tags string
	var mutation memoryMutationOptions
	cmd := &cobra.Command{
		Use:   "update <id>",
		Short: "Update a memory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			var body []byte
			var query map[string]string
			var err error
			if file != "" {
				var manifest map[string]any
				var manifestBody []byte
				manifest, manifestBody, err = manifestMap(file)
				manifestNS := ""
				if err == nil {
					manifestNS = strings.TrimSpace(manifestNamespace(manifest))
					query, err = namespaceQueryForManifest(cmd, c.Namespace, manifest)
				}
				if err == nil && manifestNS != "" {
					query = map[string]string{"namespace": manifestNS}
				}
				body = manifestBody
			} else {
				patch := map[string]any{}
				if content != "" {
					patch["content"] = content
				}
				if source != "" {
					patch["source"] = source
				}
				if tags != "" {
					patch["tags"] = splitComma(tags)
				}
				if len(patch) == 0 {
					return fmt.Errorf("--file or at least one mutable field flag is required")
				}
				body, err = json.Marshal(patch)
			}
			if err != nil {
				return err
			}
			response, err := executeMemoryMutation(
				cmd,
				c,
				http.MethodPut,
				"/api/v1/memories/"+url.PathEscape(args[0]),
				query,
				body,
				mutation,
			)
			if err != nil {
				return err
			}
			return printMemoryMutationOutcome(cmd, "Memory updated", args[0], response)
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "Path to memory JSON/YAML body")
	cmd.Flags().StringVar(&content, "content", "", "Memory content")
	cmd.Flags().StringVar(&source, "source", "", "Memory source")
	cmd.Flags().StringVar(&tags, "tags", "", "Comma-separated tags")
	addMemoryMutationFlags(cmd, &mutation)
	return cmd
}

func newMemoryDeleteCmd() *cobra.Command {
	var mutation memoryMutationOptions
	cmd := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a memory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			response, err := executeMemoryMutation(
				cmd,
				c,
				http.MethodDelete,
				"/api/v1/memories/"+url.PathEscape(args[0]),
				nil,
				nil,
				mutation,
			)
			if err != nil {
				return err
			}
			return printMemoryMutationOutcome(cmd, "Memory deleted", args[0], response)
		},
	}
	addMemoryMutationFlags(cmd, &mutation)
	return cmd
}

func newMemoryEnableDisableCmd(action string) *cobra.Command {
	return &cobra.Command{
		Use:   action + " <id>",
		Short: titleName(action) + " a memory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			path := "/api/v1/memories/" + url.PathEscape(args[0]) + "/" + action
			_, err := c.DoJSON(context.Background(), http.MethodPost, path, nil, []byte("{}"))
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Memory %sd: %s\n", action, args[0]) //nolint:errcheck
			return nil
		},
	}
}

func newMemoryProposalCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "proposal", Short: "Manage memory proposals"}
	cmd.AddCommand(newMemoryProposalListCmd())
	cmd.AddCommand(newMemoryProposalGetCmd())
	cmd.AddCommand(newMemoryProposalReviewCmd())
	cmd.AddCommand(newMemoryProposalApplyCmd())
	cmd.AddCommand(newMemoryProposalArchiveCmd())
	return cmd
}

func newMemoryProposalListCmd() *cobra.Command {
	var status, typ, taskName, agentName, query string
	var limit int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List memory proposals",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := mergeQuery(
				map[string]string{},
				"status", status,
				"type", typ,
				"taskName", taskName,
				"agentName", agentName,
				"query", query,
				"limit", fmt.Sprintf("%d", limit),
			)
			c := newClientFromCmd(cmd)
			result, err := c.DoJSON(context.Background(), http.MethodGet, "/api/v1/memory-proposals", q, nil)
			if err != nil {
				return err
			}
			return printStructured(cmd, result)
		},
	}
	addOutputFlag(cmd, outputTable)
	cmd.Flags().StringVar(&status, "status", "", "Filter by proposal status")
	cmd.Flags().StringVar(&typ, "type", "", "Filter by proposal type")
	cmd.Flags().StringVar(&taskName, "task-name", "", "Filter by task name")
	cmd.Flags().StringVar(&agentName, "agent-name", "", "Filter by agent name")
	cmd.Flags().StringVar(&query, "query", "", "Search query")
	cmd.Flags().IntVar(&limit, "limit", 100, "Maximum number of results")
	return cmd
}

func newMemoryProposalGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Get a memory proposal",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			path := "/api/v1/memory-proposals/" + url.PathEscape(args[0])
			result, err := c.DoJSON(context.Background(), http.MethodGet, path, nil, nil)
			if err != nil {
				return err
			}
			return printStructured(cmd, result)
		},
	}
	addOutputFlag(cmd, outputJSON)
	return cmd
}

func newMemoryProposalReviewCmd() *cobra.Command {
	var status, reviewer, note string
	cmd := &cobra.Command{
		Use:   "review <id>",
		Short: "Review a memory proposal",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if status == "" {
				return fmt.Errorf("--status is required")
			}
			body, _ := json.Marshal(map[string]string{
				"status":     status,
				"reviewer":   reviewer,
				"reviewNote": note,
			})
			c := newClientFromCmd(cmd)
			path := "/api/v1/memory-proposals/" + url.PathEscape(args[0]) + "/review"
			_, err := c.DoJSON(context.Background(), http.MethodPost, path, nil, body)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Memory proposal reviewed: %s\n", args[0]) //nolint:errcheck
			return nil
		},
	}
	cmd.Flags().StringVar(&status, "status", "", "Review status (accepted or rejected)")
	cmd.Flags().StringVar(&reviewer, "reviewer", "", "Reviewer name")
	cmd.Flags().StringVar(&note, "note", "", "Review note")
	return cmd
}

func newMemoryProposalApplyCmd() *cobra.Command {
	var appliedBy string
	var mutation memoryMutationOptions
	cmd := &cobra.Command{
		Use:   "apply <id>",
		Short: "Apply an accepted memory proposal",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, _ := json.Marshal(map[string]string{"appliedBy": appliedBy})
			c := newClientFromCmd(cmd)
			path := "/api/v1/memory-proposals/" + url.PathEscape(args[0]) + "/apply"
			response, err := executeMemoryMutation(cmd, c, http.MethodPost, path, nil, body, mutation)
			if err != nil {
				return err
			}
			return printMemoryMutationOutcome(cmd, "Memory proposal applied", args[0], response)
		},
	}
	cmd.Flags().StringVar(&appliedBy, "applied-by", "", "Reviewer applying the proposal")
	addMemoryMutationFlags(cmd, &mutation)
	return cmd
}

func newMemoryProposalArchiveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "archive <id>",
		Short: "Archive a memory proposal",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			path := "/api/v1/memory-proposals/" + url.PathEscape(args[0]) + "/archive"
			_, err := c.DoJSON(context.Background(), http.MethodPost, path, nil, []byte("{}"))
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Memory proposal archived: %s\n", args[0]) //nolint:errcheck
			return nil
		},
	}
}

func splitComma(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
