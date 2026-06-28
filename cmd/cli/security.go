package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"
)

func newSecurityCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "security", Short: "Manage repository security scans"}
	cmd.AddCommand(newSecurityRepoCmd())
	cmd.AddCommand(newSecurityScanCmd())
	cmd.AddCommand(newSecurityThreatModelCmd())
	cmd.AddCommand(newSecurityFindingCmd())
	cmd.AddCommand(newSecuritySliceCmd())
	cmd.AddCommand(newSecurityDroppedFindingsCmd())
	return cmd
}

func newSecurityRepoCmd() *cobra.Command {
	return newCRUDResourceCmd(crudResourceSpec{
		Use:      "repo",
		Short:    "Manage security repository scan configs",
		BasePath: "/api/v1/security/repositories",
		Name:     "repository scan",
	})
}

func newSecurityScanCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "scan", Short: "Run and list security scan runs"}
	cmd.AddCommand(newSecurityScanRunCmd())
	cmd.AddCommand(newSecurityScanListCmd())
	return cmd
}

func newSecurityScanRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run <repo>",
		Short: "Run a manual security scan",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			path := "/api/v1/security/repositories/" + url.PathEscape(args[0]) + "/scans"
			result, err := c.DoJSON(context.Background(), http.MethodPost, path, nil, []byte("{}"))
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Security scan run created: %s\n", metadataName(result)) //nolint:errcheck
			return nil
		},
	}
}

func newSecurityScanListCmd() *cobra.Command {
	var limit int
	var cursor string
	cmd := &cobra.Command{
		Use:   "list <repo>",
		Short: "List security scan runs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := map[string]string{
				"limit":    fmt.Sprintf("%d", limit),
				"cursor":   cursor,
				"continue": cursor,
			}
			c := newClientFromCmd(cmd)
			path := "/api/v1/security/repositories/" + url.PathEscape(args[0]) + "/scans"
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
	return cmd
}

func newSecurityThreatModelCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "threat-model", Short: "Manage security threat models"}
	cmd.AddCommand(newSecurityThreatModelGetCmd())
	cmd.AddCommand(newSecurityThreatModelUpdateCmd())
	return cmd
}

func newSecurityThreatModelGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <repo>",
		Short: "Get latest threat model",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			path := "/api/v1/security/repositories/" + url.PathEscape(args[0]) + "/threat-model"
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

func newSecurityThreatModelUpdateCmd() *cobra.Command {
	var file, content, source string
	cmd := &cobra.Command{
		Use:   "update <repo>",
		Short: "Update threat model",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if file != "" {
				data, err := readFileOrStdin(file)
				if err != nil {
					return err
				}
				content = string(data)
			}
			if content == "" {
				return fmt.Errorf("--content or --file is required")
			}
			body, _ := json.Marshal(map[string]string{"content": content, "source": source})
			c := newClientFromCmd(cmd)
			path := "/api/v1/security/repositories/" + url.PathEscape(args[0]) + "/threat-model"
			result, err := c.DoJSON(context.Background(), http.MethodPut, path, nil, body)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Threat model updated: %s\n", metadataName(result)) //nolint:errcheck
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "Path to threat model content")
	cmd.Flags().StringVar(&content, "content", "", "Threat model content")
	cmd.Flags().StringVar(&source, "source", "edited", "Threat model source")
	return cmd
}

func newSecurityFindingCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "finding", Short: "Manage security findings"}
	cmd.AddCommand(newSecurityFindingListCmd())
	cmd.AddCommand(newSecurityFindingGetCmd())
	for _, action := range []string{"dismiss", "reopen", "validate", "patch", "patches", "pr"} {
		cmd.AddCommand(newSecurityFindingActionCmd(action))
	}
	return cmd
}

func newSecurityFindingListCmd() *cobra.Command {
	var limit int
	var cursor, sliceID, category, severity, validationStatus, state string
	var recommended bool
	cmd := &cobra.Command{
		Use:   "list <repo>",
		Short: "List security findings",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := mergeQuery(
				map[string]string{},
				"limit", fmt.Sprintf("%d", limit),
				"cursor", cursor,
				"continue", cursor,
				"sliceID", sliceID,
				"category", category,
				"severity", severity,
				"validationStatus", validationStatus,
				"state", state,
			)
			if recommended {
				q["recommended"] = cliQueryTrue
			}
			c := newClientFromCmd(cmd)
			path := "/api/v1/security/repositories/" + url.PathEscape(args[0]) + "/findings"
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
	cmd.Flags().StringVar(&sliceID, "slice-id", "", "Filter by slice ID")
	cmd.Flags().StringVar(&category, "category", "", "Filter by category")
	cmd.Flags().StringVar(&severity, "severity", "", "Filter by severity")
	cmd.Flags().StringVar(&validationStatus, "validation-status", "", "Filter by validation status")
	cmd.Flags().StringVar(&state, "state", "", "Filter by state")
	cmd.Flags().BoolVar(&recommended, "recommended", false, "Only recommended findings")
	return cmd
}

func newSecurityFindingGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Get a security finding",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			path := "/api/v1/security/findings/" + url.PathEscape(args[0])
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

func newSecurityFindingActionCmd(action string) *cobra.Command {
	use := action + " <id>"
	short := action + " a security finding"
	method := http.MethodPost
	pathSuffix := action
	switch action {
	case "patches":
		method = http.MethodGet
		short = "List security patch proposals"
	case "pr":
		pathSuffix = "pull-request"
		short = "Create a pull request for the latest patch proposal"
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			var body []byte
			if method == http.MethodPost {
				body = []byte("{}")
			}
			path := "/api/v1/security/findings/" + url.PathEscape(args[0]) + "/" + pathSuffix
			result, err := c.DoJSON(context.Background(), method, path, nil, body)
			if err != nil {
				return err
			}
			if method == http.MethodGet || action == "patch" || action == "pr" {
				return printStructured(cmd, result)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Security finding %s: %s\n", action, args[0]) //nolint:errcheck
			return nil
		},
	}
	if action == "patches" || action == "patch" || action == "pr" {
		addOutputFlag(cmd, outputJSON)
	}
	return cmd
}

func newSecuritySliceCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "slice", Short: "Inspect security review slices"}
	cmd.AddCommand(newSecuritySliceListCmd())
	cmd.AddCommand(newSecuritySliceGetCmd())
	return cmd
}

func newSecuritySliceListCmd() *cobra.Command {
	var limit int
	var cursor, status string
	cmd := &cobra.Command{
		Use:   "list <repo>",
		Short: "List security review slices",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := mergeQuery(
				map[string]string{},
				"limit", fmt.Sprintf("%d", limit),
				"cursor", cursor,
				"continue", cursor,
				"status", status,
			)
			c := newClientFromCmd(cmd)
			path := "/api/v1/security/repositories/" + url.PathEscape(args[0]) + "/slices"
			result, err := c.DoJSON(context.Background(), http.MethodGet, path, q, nil)
			if err != nil {
				return err
			}
			return printStructured(cmd, result)
		},
	}
	addOutputFlag(cmd, outputTable)
	cmd.Flags().IntVar(&limit, "limit", 100, "Maximum number of results")
	cmd.Flags().StringVar(&cursor, "cursor", "", "Cursor token")
	cmd.Flags().StringVar(&cursor, "continue", "", "Continue token")
	cmd.Flags().StringVar(&status, "status", "", "Filter by status")
	return cmd
}

func newSecuritySliceGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <repo> <slice-id>",
		Short: "Get a security review slice",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			path := "/api/v1/security/repositories/" + url.PathEscape(args[0]) +
				"/slices/" + url.PathEscape(args[1])
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

func newSecurityDroppedFindingsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "dropped-findings", Short: "Inspect dropped findings"}
	var limit int
	var cursor, layer, reason, scanRunID, sliceID string
	list := &cobra.Command{
		Use:   "list <repo>",
		Short: "List dropped security findings",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			q := mergeQuery(
				map[string]string{},
				"limit", fmt.Sprintf("%d", limit),
				"cursor", cursor,
				"continue", cursor,
				"scanRunID", scanRunID,
				"sliceID", sliceID,
				"layer", layer,
				"reason", reason,
			)
			c := newClientFromCmd(cmd)
			path := "/api/v1/security/repositories/" + url.PathEscape(args[0]) + "/dropped-findings"
			result, err := c.DoJSON(context.Background(), http.MethodGet, path, q, nil)
			if err != nil {
				return err
			}
			return printStructured(cmd, result)
		},
	}
	addOutputFlag(list, outputTable)
	list.Flags().IntVar(&limit, "limit", 50, "Maximum number of results")
	list.Flags().StringVar(&cursor, "cursor", "", "Cursor token")
	list.Flags().StringVar(&cursor, "continue", "", "Continue token")
	list.Flags().StringVar(&scanRunID, "scan-run-id", "", "Filter by scan run ID")
	list.Flags().StringVar(&sliceID, "slice-id", "", "Filter by review slice ID")
	list.Flags().StringVar(&layer, "layer", "", "Filter by dropped-finding layer (validation, filter, cap)")
	list.Flags().StringVar(&reason, "reason", "", "Filter by exact reason or contains=<text>")
	cmd.AddCommand(list)
	return cmd
}
