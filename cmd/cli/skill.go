/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/sozercan/orka/internal/cli/client"
)

func newSkillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Manage skills",
	}
	cmd.AddCommand(newSkillListCmd())
	cmd.AddCommand(newSkillGetCmd())
	cmd.AddCommand(newSkillDeleteCmd())
	cmd.AddCommand(newSkillCreateCmd())
	cmd.AddCommand(newSkillImportCmd())
	return cmd
}

func newSkillListCmd() *cobra.Command {
	return &cobra.Command{
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
}

func newSkillGetCmd() *cobra.Command {
	return &cobra.Command{
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

			out, err := json.MarshalIndent(skill, "", "  ")
			if err != nil {
				return fmt.Errorf("formatting output: %w", err)
			}
			fmt.Println(string(out))
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

			data, err := os.ReadFile(file)
			if err != nil {
				return fmt.Errorf("reading file: %w", err)
			}

			jsonBody, err := yaml.YAMLToJSON(data)
			if err != nil {
				return fmt.Errorf("parsing manifest: %w", err)
			}

			c := newClientFromCmd(cmd)
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

			body := map[string]any{
				"name": name,
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

			c := newClientFromCmd(cmd)
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
