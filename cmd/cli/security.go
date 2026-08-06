package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/spf13/cobra"

	securitybundle "github.com/orka-agents/orka/internal/security/bundle"
)

const securityFindingAssessmentsAction = "assessments"

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
	cmd.AddCommand(newSecurityScanDocumentCmd("bundle"))
	cmd.AddCommand(newSecurityScanDocumentCmd("coverage"))
	cmd.AddCommand(newSecurityScanQualityCheckCmd())
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

func newSecurityScanDocumentCmd(document string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   document + " <repo> <run-id>",
		Short: "Get a sealed security scan " + document,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			path := "/api/v1/security/repositories/" + url.PathEscape(args[0]) +
				"/scans/" + url.PathEscape(args[1]) + "/" + document
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

func newSecurityScanQualityCheckCmd() *cobra.Command {
	var validated bool
	cmd := &cobra.Command{
		Use:   "check <repo> <run-id>",
		Short: "Fail when a sealed scan bundle does not satisfy requested quality",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			path := "/api/v1/security/repositories/" + url.PathEscape(args[0]) +
				"/scans/" + url.PathEscape(args[1]) + "/bundle"
			raw, _, err := c.GetRaw(context.Background(), path, nil)
			if err != nil {
				return err
			}
			var response struct {
				ID               string                        `json:"id"`
				ScanRunID        string                        `json:"scanRunID"`
				RunUID           string                        `json:"runUID"`
				Version          int                           `json:"version"`
				Manifest         json.RawMessage               `json:"manifest"`
				Findings         json.RawMessage               `json:"findings"`
				Coverage         json.RawMessage               `json:"coverage"`
				Evidence         []securitybundle.EvidenceBlob `json:"evidence"`
				ContentDigest    string                        `json:"contentDigest"`
				RunReceiptDigest string                        `json:"runReceiptDigest"`
				SealedAt         time.Time                     `json:"sealedAt"`
			}
			if err := json.Unmarshal(raw, &response); err != nil {
				return fmt.Errorf("decode bundle response: %w", err)
			}
			if err := securitybundle.Verify(&securitybundle.Bundle{
				ManifestJSON: response.Manifest, FindingsJSON: response.Findings, CoverageJSON: response.Coverage,
				Evidence: response.Evidence, Roots: securitybundle.RootDigests{
					ContentDigest: response.ContentDigest, RunReceiptDigest: response.RunReceiptDigest,
				},
			}, securitybundle.DefaultLimits()); err != nil {
				return fmt.Errorf("security scan bundle verification failed: %w", err)
			}
			var envelope struct {
				SchemaVersion int `json:"schemaVersion"`
				Run           struct {
					RunUID             string  `json:"runUid"`
					PublicRunID        *string `json:"publicRunId"`
					RepositoryScanName string  `json:"repositoryScanName"`
					SealedAt           string  `json:"sealedAt"`
				} `json:"run"`
			}
			if err := json.Unmarshal(response.Manifest, &envelope); err != nil {
				return fmt.Errorf("decode bundle manifest envelope: %w", err)
			}
			publicRunID := ""
			if envelope.Run.PublicRunID != nil {
				publicRunID = *envelope.Run.PublicRunID
			}
			manifestSealedAt, err := time.Parse(time.RFC3339Nano, envelope.Run.SealedAt)
			if err != nil {
				return fmt.Errorf("decode bundle sealedAt: %w", err)
			}
			if response.Version != securitybundle.SchemaVersion || envelope.SchemaVersion != securitybundle.SchemaVersion ||
				response.ScanRunID != args[1] || publicRunID != args[1] || envelope.Run.RepositoryScanName != args[0] ||
				response.RunUID != envelope.Run.RunUID || !response.SealedAt.Equal(manifestSealedAt) {
				return fmt.Errorf("security scan bundle does not match requested repository/run envelope")
			}
			if !validated {
				fmt.Fprintln(cmd.OutOrStdout(), "Security scan bundle is sealed") //nolint:errcheck
				return nil
			}
			var manifest map[string]any
			if err := json.Unmarshal(response.Manifest, &manifest); err != nil {
				return fmt.Errorf("decode bundle manifest: %w", err)
			}
			quality, ok := manifest["quality"].(map[string]any)
			if !ok {
				return fmt.Errorf("bundle quality summary is missing")
			}
			required := map[string][]string{
				"inventoryCoverage": {"complete"}, "candidateCoverage": {"complete"}, "coverage": {"complete"},
				"validationScope": {"all"}, "validationExecution": {"complete"},
				"attackPathExecution": {"complete"}, "analysisAttestation": {"tool-observed", "brokered"},
				"targetVerification": {"verified"},
				"authorization":      {"verified", "admitted"}, "isolation": {"hardened"},
			}
			for field, allowed := range required {
				value, _ := quality[field].(string)
				matched := false
				for _, candidate := range allowed {
					matched = matched || value == candidate
				}
				if !matched {
					return fmt.Errorf("validated quality check failed: %s=%q", field, value)
				}
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Security scan satisfies validated quality") //nolint:errcheck
			return nil
		},
	}
	cmd.Flags().BoolVar(&validated, "validated", false, "Require full verified validated assurance")
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
	actions := []string{
		"dismiss", "reopen", "validate", "patch", "patches", "pr",
		"occurrences", "decisions", securityFindingAssessmentsAction,
	}
	for _, action := range actions {
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
	historyAction := false
	switch action {
	case "patches":
		method = http.MethodGet
		short = "List security patch proposals"
	case "occurrences", "decisions", securityFindingAssessmentsAction:
		method = http.MethodGet
		historyAction = true
		short = "List immutable security finding " + action
	case "pr":
		pathSuffix = "pull-request"
		short = "Create a pull request for the latest patch proposal"
	}
	var limit int
	var cursor, kind string
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
			var query map[string]string
			if historyAction {
				query = mergeQuery(
					map[string]string{},
					"limit", fmt.Sprintf("%d", limit),
					"cursor", cursor,
				)
				if action == securityFindingAssessmentsAction && kind != "" {
					query["kind"] = kind
				}
			}
			path := "/api/v1/security/findings/" + url.PathEscape(args[0]) + "/" + pathSuffix
			result, err := c.DoJSON(context.Background(), method, path, query, body)
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
	structuredOutput := action == "patches" || action == "patch" || action == "pr" || historyAction
	if structuredOutput {
		addOutputFlag(cmd, outputJSON)
	}
	if historyAction {
		cmd.Flags().IntVar(&limit, "limit", 50, "Maximum number of results")
		cmd.Flags().StringVar(&cursor, "cursor", "", "Cursor token")
		cmd.Flags().StringVar(&cursor, "continue", "", "Continue token")
	}
	if action == securityFindingAssessmentsAction {
		cmd.Flags().StringVar(&kind, "kind", "", "Filter by assessment kind")
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
