package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

type crudResourceSpec struct {
	Use       string
	Short     string
	BasePath  string
	Name      string
	ReadOnly  bool
	NoGet     bool
	NoCreate  bool
	NoUpdate  bool
	NoDelete  bool
	ListFlags func(*cobra.Command)
	ListQuery func(*cobra.Command) map[string]string
}

func newCRUDResourceCmd(spec crudResourceSpec) *cobra.Command {
	cmd := &cobra.Command{Use: spec.Use, Short: spec.Short}
	cmd.AddCommand(newCRUDListCmd(spec))
	if !spec.NoGet {
		cmd.AddCommand(newCRUDGetCmd(spec))
	}
	if !spec.ReadOnly && !spec.NoCreate {
		cmd.AddCommand(newCRUDCreateCmd(spec))
	}
	if !spec.ReadOnly && !spec.NoUpdate {
		cmd.AddCommand(newCRUDUpdateCmd(spec))
	}
	if !spec.ReadOnly && !spec.NoDelete {
		cmd.AddCommand(newCRUDDeleteCmd(spec))
	}
	return cmd
}

func newCRUDListCmd(spec crudResourceSpec) *cobra.Command {
	var limit int
	var cont string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List " + spec.Name + " resources",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := newClientFromCmd(cmd)
			query := map[string]string{}
			if limit > 0 {
				query["limit"] = fmt.Sprintf("%d", limit)
			}
			if cont != "" {
				query["continue"] = cont
				query["cursor"] = cont
			}
			if spec.ListQuery != nil {
				for k, v := range spec.ListQuery(cmd) {
					if v != "" {
						query[k] = v
					}
				}
			}
			result, err := c.DoJSON(context.Background(), http.MethodGet, spec.BasePath, query, nil)
			if err != nil {
				return err
			}
			return printStructured(cmd, result)
		},
	}
	addOutputFlag(cmd, outputTable)
	cmd.Flags().IntVar(&limit, "limit", 100, "Maximum number of results")
	cmd.Flags().StringVar(&cont, "continue", "", "Continue/cursor token for the next page")
	cmd.Flags().StringVar(&cont, "cursor", "", "Cursor token for the next page")
	if spec.ListFlags != nil {
		spec.ListFlags(cmd)
	}
	return cmd
}

func newCRUDGetCmd(spec crudResourceSpec) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Get a " + spec.Name + " resource",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			result, err := c.DoJSON(context.Background(), http.MethodGet, spec.BasePath+"/"+url.PathEscape(args[0]), nil, nil)
			if err != nil {
				return err
			}
			return printStructured(cmd, result)
		},
	}
	addOutputFlag(cmd, outputJSON)
	return cmd
}

func newCRUDCreateCmd(spec crudResourceSpec) *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "create -f <file>",
		Short: "Create a " + spec.Name + " resource from a manifest",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file == "" {
				return fmt.Errorf("--file (-f) is required")
			}
			c := newClientFromCmd(cmd)
			body, err := manifestWithNamespaceJSON(cmd, file, c.Namespace)
			if err != nil {
				return err
			}
			result, err := c.DoJSON(context.Background(), http.MethodPost, spec.BasePath, nil, body)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s created: %s\n", titleName(spec.Name), metadataName(result)) //nolint:errcheck
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "Path to YAML/JSON manifest (use - for stdin)")
	return cmd
}

func newCRUDUpdateCmd(spec crudResourceSpec) *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "update <name> -f <file>",
		Short: "Update a " + spec.Name + " resource from a manifest",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if file == "" {
				return fmt.Errorf("--file (-f) is required")
			}
			manifest, body, err := manifestMap(file)
			if err != nil {
				return err
			}
			c := newClientFromCmd(cmd)
			query, err := namespaceQueryForManifest(cmd, c.Namespace, manifest)
			if err != nil {
				return err
			}
			result, err := c.DoJSON(context.Background(), http.MethodPut, spec.BasePath+"/"+url.PathEscape(args[0]), query, body)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s updated: %s\n", titleName(spec.Name), metadataName(result)) //nolint:errcheck
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "Path to YAML/JSON manifest (use - for stdin)")
	return cmd
}

func newCRUDDeleteCmd(spec crudResourceSpec) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a " + spec.Name + " resource",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			if err := c.DeleteResource(context.Background(), spec.BasePath+"/"+url.PathEscape(args[0]), nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s deleted: %s\n", titleName(spec.Name), args[0]) //nolint:errcheck
			return nil
		},
	}
}

func titleName(s string) string {
	if s == "" {
		return "Resource"
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func newProviderCmd() *cobra.Command {
	return newCRUDResourceCmd(crudResourceSpec{
		Use:      "provider",
		Short:    "Manage providers",
		BasePath: "/api/v1/providers",
		Name:     "provider",
	})
}

func newToolCmd() *cobra.Command {
	return newCRUDResourceCmd(crudResourceSpec{
		Use:      "tool",
		Short:    "Manage tools",
		BasePath: "/api/v1/tools",
		Name:     "tool",
	})
}

func newSessionCmd() *cobra.Command {
	cmd := newCRUDResourceCmd(crudResourceSpec{
		Use:      "session",
		Short:    "Manage sessions",
		BasePath: "/api/v1/sessions",
		Name:     "session",
		NoCreate: true,
		NoUpdate: true,
	})
	cmd.AddCommand(newSessionEventsCmd())
	cmd.AddCommand(newSessionFollowCmd())
	return cmd
}

func newSecretCmd() *cobra.Command {
	return newCRUDResourceCmd(crudResourceSpec{
		Use:      "secret",
		Short:    "Inspect secret metadata",
		BasePath: "/api/v1/secrets",
		Name:     "secret",
		NoGet:    true,
		NoCreate: true,
		NoUpdate: true,
		NoDelete: true,
	})
}

func newSubstrateCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "substrate", Short: "Inspect and manage substrate resources"}
	cmd.AddCommand(newCRUDResourceCmd(crudResourceSpec{
		Use:      "pool",
		Short:    "Manage substrate actor pools",
		BasePath: "/api/v1/substrate-actor-pools",
		Name:     "substrate pool",
	}))
	return cmd
}
