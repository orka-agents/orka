//go:build windows

package main

import (
	"errors"

	"github.com/spf13/cobra"
)

func newSessionMigrateCmd() *cobra.Command {
	return &cobra.Command{
		Use: "migrate", Short: "Move Codex 0.160.0 paginated conversation state",
		RunE: func(*cobra.Command, []string) error {
			return errors.New("native session migration requires Linux or macOS")
		},
	}
}
