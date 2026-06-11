/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/sozercan/orka/internal/cli/client"
)

func newSkillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Manage skills",
	}
	cmd.AddCommand(newSkillListCmd())
	cmd.AddCommand(newSkillGetCmd())
	cmd.AddCommand(newSkillContentCmd())
	cmd.AddCommand(newSkillCreateCmd())
	cmd.AddCommand(newSkillImportCmd())
	cmd.AddCommand(newSkillUpdateCmd())
	cmd.AddCommand(newSkillDeleteCmd())
	cmd.AddCommand(newSkillValidateCmd())
	cmd.AddCommand(newSkillInitCmd())
	return cmd
}

func newSkillListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List skills",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := newClientFromCmd(cmd)
			skills, err := c.ListSkills(context.Background(), client.ListOptions{
				Namespace: c.Namespace,
			})
			if err != nil {
				return err
			}

			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			if format != outputTable {
				return printStructured(cmd, skills)
			}

			if len(skills) == 0 {
				fmt.Println("No skills found.")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tDISPLAY NAME\tVERSION\tPHASE\tTAGS") //nolint:errcheck
			for _, s := range skills {
				displayName := s.DisplayName
				if displayName == "" {
					displayName = "-"
				}
				version := s.Version
				if version == "" {
					version = "-"
				}
				phase := s.Phase
				if phase == "" {
					phase = "-"
				}
				tags := "-"
				if len(s.Tags) > 0 {
					tags = strings.Join(s.Tags, ", ")
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", s.Name, displayName, version, phase, tags) //nolint:errcheck
			}
			w.Flush() //nolint:errcheck
			return nil
		},
	}
	addOutputFlag(cmd, outputTable)
	return cmd
}

func newSkillGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Get skill details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			skill, err := c.GetSkill(context.Background(), args[0], client.GetOptions{
				Namespace: c.Namespace,
			})
			if err != nil {
				return err
			}

			return printStructured(cmd, skill)
		},
	}
	addOutputFlag(cmd, outputJSON)
	return cmd
}

func newSkillContentCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "content <name>",
		Short: "Print raw skill content",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			body, _, err := c.GetRaw(context.Background(), "/api/v1/skills/"+url.PathEscape(args[0])+"/content", nil)
			if err != nil {
				return err
			}
			_, _ = cmd.OutOrStdout().Write(body)
			if len(body) == 0 || body[len(body)-1] != '\n' {
				fmt.Fprintln(cmd.OutOrStdout()) //nolint:errcheck
			}
			return nil
		},
	}
}

func newSkillDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a skill",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			if err := c.DeleteSkill(context.Background(), args[0], client.GetOptions{
				Namespace: c.Namespace,
			}); err != nil {
				return err
			}
			fmt.Printf("Skill deleted: %s\n", args[0])
			return nil
		},
	}
}

func newSkillCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create -f <file>",
		Short: "Create a skill from a YAML manifest",
		RunE: func(cmd *cobra.Command, _ []string) error {
			file, _ := cmd.Flags().GetString("file")
			if file == "" {
				return fmt.Errorf("--file (-f) is required")
			}

			c := newClientFromCmd(cmd)
			jsonBody, err := manifestWithNamespaceJSON(cmd, file, c.Namespace)
			if err != nil {
				return err
			}
			skill, err := c.CreateSkill(context.Background(), jsonBody)
			if err != nil {
				return err
			}

			name := ""
			if m, ok := (*skill)["metadata"].(map[string]any); ok {
				name, _ = m["name"].(string)
			}
			fmt.Printf("Skill created: %s\n", name)
			return nil
		},
	}
	cmd.Flags().StringP("file", "f", "", "Path to skill YAML manifest")
	return cmd
}

func newSkillImportCmd() *cobra.Command {
	var name string
	cmd := &cobra.Command{
		Use:   "import <path/to/SKILL.md>",
		Short: "Create a skill from a local SKILL.md file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			filePath := args[0]
			data, err := os.ReadFile(filePath)
			if err != nil {
				return fmt.Errorf("reading file: %w", err)
			}

			if name == "" {
				// Derive name from filename
				base := strings.TrimSuffix(filePath, ".md")
				base = strings.TrimSuffix(base, ".MD")
				parts := strings.Split(base, "/")
				name = strings.ToLower(parts[len(parts)-1])
				name = strings.ReplaceAll(name, " ", "-")
				name = strings.ReplaceAll(name, "_", "-")
			}

			c := newClientFromCmd(cmd)
			body := map[string]any{
				"name":      name,
				"namespace": c.Namespace,
				"spec": map[string]any{
					"description": fmt.Sprintf("Imported from %s", filePath),
					"content": map[string]any{
						"inline": string(data),
					},
				},
			}

			bodyJSON, err := json.Marshal(body)
			if err != nil {
				return fmt.Errorf("marshaling request: %w", err)
			}

			skill, err := c.CreateSkill(context.Background(), bodyJSON)
			if err != nil {
				return err
			}

			createdName := name
			if m, ok := (*skill)["metadata"].(map[string]any); ok {
				if n, ok := m["name"].(string); ok {
					createdName = n
				}
			}
			fmt.Printf("Skill imported: %s (from %s)\n", createdName, filePath)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Override skill name (default: derived from filename)")
	return cmd
}

func newSkillUpdateCmd() *cobra.Command {
	return newCRUDUpdateCmd(crudResourceSpec{
		BasePath: "/api/v1/skills",
		Name:     "skill",
	})
}

func newSkillValidateCmd() *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "validate [-f manifest.yaml] [SKILL.md]",
		Short: "Validate a local skill manifest or SKILL.md file",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if file != "" {
				m, _, err := manifestMap(file)
				if err != nil {
					return err
				}
				if metadataName(m) == "" {
					return fmt.Errorf("skill manifest must include metadata.name or name")
				}
				spec, _ := m["spec"].(map[string]any)
				if spec == nil {
					return fmt.Errorf("skill manifest must include spec")
				}
				if firstString(spec, "description") == "" {
					return fmt.Errorf("skill manifest spec.description is required")
				}
				content, _ := spec["content"].(map[string]any)
				if content == nil || strings.TrimSpace(anyString(content["inline"])) == "" {
					return fmt.Errorf("skill manifest spec.content.inline is required")
				}
				fmt.Fprintln(cmd.OutOrStdout(), "Skill manifest is valid.") //nolint:errcheck
				return nil
			}
			path := "SKILL.md"
			if len(args) == 1 {
				path = args[0]
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("reading skill file: %w", err)
			}
			if strings.TrimSpace(string(data)) == "" {
				return fmt.Errorf("skill file is empty")
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Skill file is valid.") //nolint:errcheck
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "Path to skill YAML/JSON manifest")
	return cmd
}

func newSkillInitCmd() *cobra.Command {
	var name, description string
	var force bool
	cmd := &cobra.Command{
		Use:   "init [dir]",
		Short: "Initialize a local SKILL.md template",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			if name == "" {
				name = "new-skill"
			}
			if description == "" {
				description = "Describe when and how to use this skill."
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("creating directory: %w", err)
			}
			path := dir + string(os.PathSeparator) + "SKILL.md"
			content := fmt.Sprintf(
				"# %s\n\n## Description\n\n%s\n\n## Instructions\n\n- Add step-by-step guidance here.\n",
				name,
				description,
			)
			flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
			if force {
				flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
			}
			f, err := os.OpenFile(path, flags, 0o644)
			if err != nil {
				if os.IsExist(err) {
					return fmt.Errorf("%s already exists (use --force to overwrite)", path)
				}
				return fmt.Errorf("opening %s: %w", path, err)
			}
			defer f.Close() //nolint:errcheck
			if _, err := f.WriteString(content); err != nil {
				return fmt.Errorf("writing %s: %w", path, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Skill template created: %s\n", path) //nolint:errcheck
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "Skill name for the template")
	cmd.Flags().StringVar(&description, "description", "", "Skill description for the template")
	cmd.Flags().BoolVar(&force, "force", false, "Overwrite an existing SKILL.md")
	return cmd
}
