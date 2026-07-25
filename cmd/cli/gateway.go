/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"net/http"
	"net/url"

	"github.com/spf13/cobra"
)

func newGatewayCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gateway",
		Short: "Inspect generic gateway resources and durable event delivery",
	}

	gatewaySpec := crudResourceSpec{
		Use:      "gateway",
		Short:    "Inspect Gateway adapter instances",
		BasePath: "/api/v1/gateways",
		Name:     "gateway",
		ReadOnly: true,
	}
	cmd.AddCommand(newCRUDListCmd(gatewaySpec), newCRUDGetCmd(gatewaySpec))
	cmd.AddCommand(newCRUDResourceCmd(crudResourceSpec{
		Use:      "class",
		Short:    "Inspect cluster-scoped GatewayClass profiles",
		BasePath: "/api/v1/gatewayclasses",
		Name:     "gateway class",
		ReadOnly: true,
	}))
	cmd.AddCommand(newCRUDResourceCmd(crudResourceSpec{
		Use:      "binding",
		Short:    "Inspect GatewayBinding routes",
		BasePath: "/api/v1/gatewaybindings",
		Name:     "gateway binding",
		ReadOnly: true,
	}))
	cmd.AddCommand(newGatewayEventsCmd())
	cmd.AddCommand(newGatewayDeliveriesCmd())
	return cmd
}

func newGatewayEventsCmd() *cobra.Command {
	var state, gatewayName, binding, session, task string
	spec := crudResourceSpec{
		Use:      "events",
		Short:    "Inspect durable normalized gateway ingress events",
		BasePath: "/api/v1/gateway-events",
		Name:     "gateway event",
		ReadOnly: true,
		ListFlags: func(cmd *cobra.Command) {
			cmd.Flags().StringVar(&state, "state", "", "Filter by comma-separated event state")
			cmd.Flags().StringVar(&gatewayName, "gateway", "", "Filter by Gateway name")
			cmd.Flags().StringVar(&binding, "binding", "", "Filter by GatewayBinding name")
			cmd.Flags().StringVar(&session, "session", "", "Filter by Session name")
			cmd.Flags().StringVar(&task, "task", "", "Filter by Task name")
		},
		ListQuery: func(*cobra.Command) map[string]string {
			return map[string]string{
				"state": state, "gateway": gatewayName, "binding": binding,
				"session": session, "task": task,
			}
		},
	}
	return newCRUDResourceCmd(spec)
}

func newGatewayDeliveriesCmd() *cobra.Command {
	var state, gatewayName, binding, event, session, task string
	spec := crudResourceSpec{
		Use:      "deliveries",
		Short:    "Inspect and retry durable gateway deliveries",
		BasePath: "/api/v1/gateway-deliveries",
		Name:     "gateway delivery",
		ReadOnly: true,
		ListFlags: func(cmd *cobra.Command) {
			cmd.Flags().StringVar(&state, "state", "", "Filter by comma-separated delivery state")
			cmd.Flags().StringVar(&gatewayName, "gateway", "", "Filter by Gateway name")
			cmd.Flags().StringVar(&binding, "binding", "", "Filter by GatewayBinding name")
			cmd.Flags().StringVar(&event, "event", "", "Filter by gateway event ID")
			cmd.Flags().StringVar(&session, "session", "", "Filter by Session name")
			cmd.Flags().StringVar(&task, "task", "", "Filter by Task name")
		},
		ListQuery: func(*cobra.Command) map[string]string {
			return map[string]string{
				"state": state, "gateway": gatewayName, "binding": binding, "event": event,
				"session": session, "task": task,
			}
		},
	}
	cmd := newCRUDResourceCmd(spec)
	retryCmd := &cobra.Command{
		Use:   "retry <delivery-id>",
		Short: "Retry a dead-lettered gateway delivery",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client := newClientFromCmd(cmd)
			result, err := client.DoJSON(
				context.Background(), http.MethodPost,
				"/api/v1/gateway-deliveries/"+url.PathEscape(args[0])+"/retry",
				map[string]string{"namespace": client.Namespace}, nil,
			)
			if err != nil {
				return err
			}
			return printStructured(cmd, result)
		},
	}
	addOutputFlag(retryCmd, outputJSON)
	cmd.AddCommand(retryCmd)
	return cmd
}
