/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"github.com/sozercan/mercan/internal/cli/client"
	"github.com/spf13/cobra"
)

func newClientFromCmd(cmd *cobra.Command) *client.Client {
	server, _ := cmd.Root().PersistentFlags().GetString("server")
	token, _ := cmd.Root().PersistentFlags().GetString("token")
	namespace, _ := cmd.Root().PersistentFlags().GetString("namespace")
	return client.New(server, token, namespace)
}
