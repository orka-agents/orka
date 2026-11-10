package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/orka-agents/orka/internal/cli/client"
)

// agentHostAPIPath is the signed-in person's own agent host.
const agentHostAPIPath = "/api/v1/agent-hosts/me"

// agentHostPhaseFailed is the phase of a host that cannot run until its cause
// is fixed.
const agentHostPhaseFailed = "Failed"

// agentHostReasonConnectionUnavailable is a host's reason while its link is
// gone; linking again clears it.
const agentHostReasonConnectionUnavailable = "ConnectionUnavailable"

// Failures a new link can clear: the host's link was gone, gave another
// GitHub account, or gave a token that never expires.
const (
	agentHostReasonGitHubIdentityChanged   = "GitHubIdentityChanged"
	agentHostReasonGitHubTokenNeverExpires = "GitHubTokenNeverExpires"
)

// agentHostReadyPollInterval paces waiting for a host after linking.
var agentHostReadyPollInterval = 2 * time.Second

// agentHostView is the API's view of the person's own agent host.
type agentHostView struct {
	Name              string `json:"name"`
	Namespace         string `json:"namespace"`
	Environment       string `json:"environment"`
	Phase             string `json:"phase"`
	Ready             bool   `json:"ready"`
	Reason            string `json:"reason,omitempty"`
	Message           string `json:"message,omitempty"`
	GitHubLogin       string `json:"githubLogin,omitempty"`
	Suspended         bool   `json:"suspended"`
	OperatorSuspended bool   `json:"operatorSuspended,omitempty"`
	Connection        string `json:"connection"`
	Deleting          bool   `json:"deleting,omitempty"`
}

func newHostCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "host",
		Short: "See and manage your agent host for the Copilot app",
		Long: "Your agent host runs the Copilot app's environment on Orka. Orka creates it when you link " +
			"GitHub through a provider that serves agent hosts, and it appears in the Copilot app as orka-<host>.",
	}
	cmd.AddCommand(newHostStatusCmd(), newHostWakeCmd(), newHostDeleteCmd())
	return cmd
}

func newHostStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show your agent host",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			host, err := getOwnAgentHost(context.Background(), newClientFromCmd(cmd))
			if err != nil {
				return err
			}
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			if format != outputTable {
				return printStructured(cmd, host)
			}
			printAgentHost(cmd.OutOrStdout(), host)
			return nil
		},
	}
	addOutputFlag(cmd, outputTable)
	return cmd
}

func newHostWakeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "wake",
		Short: "Start your agent host after it idled out",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := newClientFromCmd(cmd)
			raw, err := c.DoJSON(context.Background(), http.MethodPost, agentHostAPIPath+"/wake", nil, nil)
			if err != nil {
				return agentHostError(err)
			}
			var host agentHostView
			if err := decodeInto(raw, &host); err != nil {
				return err
			}
			if host.Suspended {
				fmt.Fprintf(cmd.OutOrStdout(), "Waking %s. Check with 'orka host status'.\n", host.Environment) //nolint:errcheck
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "%s is not suspended.\n", host.Environment) //nolint:errcheck
			}
			return nil
		},
	}
}

func newHostDeleteCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete",
		Short: "Delete your agent host and its volume",
		Long: "Delete your agent host and its volume: its clones, worktrees and sessions are lost. While you stay " +
			"linked, Orka creates a fresh host; disconnect with 'orka connection delete' to have none.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !yes {
				return errors.New("deleting your host also deletes its volume, with its clones, worktrees and sessions; rerun with --yes")
			}
			c := newClientFromCmd(cmd)
			raw, err := c.DoJSON(context.Background(), http.MethodDelete, agentHostAPIPath, nil, nil)
			if err != nil {
				return agentHostError(err)
			}
			var host agentHostView
			if err := decodeInto(raw, &host); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleting %s and its volume. While GitHub stays linked, Orka creates a fresh host.\n", host.Environment) //nolint:errcheck
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "Confirm deleting the host and its volume")
	return cmd
}

// getOwnAgentHost reads the person's host with c. Callers keep one client
// for a whole command: credentials read from stdin are read once.
func getOwnAgentHost(ctx context.Context, c *client.Client) (agentHostView, error) {
	raw, err := c.DoJSON(ctx, http.MethodGet, agentHostAPIPath, nil, nil)
	if err != nil {
		return agentHostView{}, agentHostError(err)
	}
	var host agentHostView
	if err := decodeInto(raw, &host); err != nil {
		return agentHostView{}, err
	}
	return host, nil
}

func printAgentHost(out io.Writer, host agentHostView) {
	state := host.Phase
	if state == "" {
		state = "Pending"
	}
	fmt.Fprintf(out, "%s: %s\n", host.Environment, state) //nolint:errcheck
	fmt.Fprintf(out, "  host: %s\n", host.Name)           //nolint:errcheck
	if host.GitHubLogin != "" {
		fmt.Fprintf(out, "  GitHub account: %s\n", host.GitHubLogin) //nolint:errcheck
	}
	if !host.Ready && host.Reason != "" {
		fmt.Fprintf(out, "  reason: %s: %s\n", host.Reason, host.Message) //nolint:errcheck
	}
	switch {
	case host.Deleting:
		fmt.Fprintln(out, "  being deleted") //nolint:errcheck
	case host.OperatorSuspended:
		fmt.Fprintln(out, "  suspended by an operator") //nolint:errcheck
	case host.Suspended:
		fmt.Fprintln(out, "  suspended while idle; run 'orka host wake'") //nolint:errcheck
	}
}

// waitForAgentHost waits until the host fed by connection, the link just made
// to a provider that serves agent hosts, is ready, and says how the Copilot
// app lists it.
func waitForAgentHost(ctx context.Context, cmd *cobra.Command, c *client.Client, connection string) error {
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "Waiting for your agent host...") //nolint:errcheck
	// The last failure seen that the new link may clear: it is waited out,
	// and reported if the wait ends on it.
	var failed *agentHostView
	for {
		host, err := getOwnAgentHost(ctx, c)
		if err == nil {
			failed = nil
		}
		switch {
		// Until the new link's host exists, the API can answer with a host
		// kept from another link, ready or failed; it is not the one to use.
		case err == nil && host.Connection != connection:
		// A host being deleted is not the one to use; its replacement follows.
		case err == nil && host.Deleting:
		case err == nil && host.Ready:
			fmt.Fprintf(out, "Host ready: %s. Pick it in the Copilot app's Environments.\n", host.Environment) //nolint:errcheck
			return nil
		// A host kept from an earlier link still shows the failure that link
		// left until the controller sees the new one.
		case err == nil && host.Phase == agentHostPhaseFailed && linkMayClear(host.Reason):
			failed = &host
		case err == nil && host.Phase == agentHostPhaseFailed:
			return fmt.Errorf("your agent host %s failed: %s: %s", host.Name, host.Reason, host.Message)
		// The host is created moments after the link; until then the API
		// answers 404. A request cut short by the deadline ends the wait
		// below.
		case err != nil && ctx.Err() == nil && !strings.Contains(err.Error(), "HTTP 404"):
			return err
		}
		select {
		case <-ctx.Done():
			if failed != nil {
				return fmt.Errorf("your agent host %s is not ready: %s: %s; check with 'orka host status'", failed.Name, failed.Reason, failed.Message)
			}
			return errors.New("your agent host is not ready yet; check with 'orka host status'")
		case <-time.After(agentHostReadyPollInterval):
		}
	}
}

func linkMayClear(reason string) bool {
	switch reason {
	case agentHostReasonConnectionUnavailable, agentHostReasonGitHubIdentityChanged, agentHostReasonGitHubTokenNeverExpires:
		return true
	}
	return false
}

func agentHostError(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "HTTP 403") {
		return errors.Join(err, errors.New("your agent host belongs to a signed-in person: pass your OIDC token with --token, or a context token with --txn-token"))
	}
	return err
}
