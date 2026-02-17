/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/sozercan/orka/internal/cli/client"
)

func newTaskArtifactsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "artifacts <task-name>",
		Short: "List artifacts for a task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			artifacts, err := c.ListArtifacts(context.Background(), args[0], client.GetOptions{
				Namespace: c.Namespace,
			})
			if err != nil {
				return err
			}

			if len(artifacts) == 0 {
				fmt.Println("No artifacts found.")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "FILENAME\tTYPE\tSIZE") //nolint:errcheck
			for _, a := range artifacts {
				fmt.Fprintf(w, "%s\t%s\t%s\n", a.Filename, a.ContentType, formatSize(a.Size)) //nolint:errcheck
			}
			w.Flush() //nolint:errcheck
			return nil
		},
	}
	return cmd
}

func newTaskDownloadCmd() *cobra.Command {
	var outputPath string

	cmd := &cobra.Command{
		Use:   "download <task-name> [filename]",
		Short: "Download task artifacts",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			ctx := context.Background()
			taskName := args[0]

			var filename string
			if len(args) > 1 {
				filename = args[1]
			} else {
				artifacts, err := c.ListArtifacts(ctx, taskName, client.GetOptions{
					Namespace: c.Namespace,
				})
				if err != nil {
					return err
				}
				switch len(artifacts) {
				case 0:
					return fmt.Errorf("no artifacts found for task %q", taskName)
				case 1:
					filename = artifacts[0].Filename
				default:
					fmt.Println("Multiple artifacts found, specify filename:")
					for _, a := range artifacts {
						fmt.Printf("  %s\n", a.Filename)
					}
					return fmt.Errorf("multiple artifacts found, specify filename")
				}
			}

			data, _, err := c.DownloadArtifact(ctx, taskName, filename, client.GetOptions{
				Namespace: c.Namespace,
			})
			if err != nil {
				return err
			}

			// Use only the base filename to prevent path traversal
			safeFilename := filepath.Base(filename)
			dest := outputPath
			if dest == "" {
				dest = safeFilename
			}
			if info, err := os.Stat(dest); err == nil && info.IsDir() {
				dest = filepath.Join(dest, safeFilename)
			}

			if err := os.WriteFile(dest, data, 0644); err != nil { //nolint:gosec
				return fmt.Errorf("writing file: %w", err)
			}

			fmt.Printf("Saved to %s (%s)\n", dest, formatSize(int64(len(data))))
			return nil
		},
	}

	cmd.Flags().StringVarP(&outputPath, "output", "o", "", "output file path")

	return cmd
}

func formatSize(b int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
	)
	switch {
	case b >= mb:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
