package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/orka-agents/orka/internal/cli/client"
)

// agentHostAPIPath is the signed-in person's own agent host.
const agentHostAPIPath = "/api/v1/agent-hosts/me"

// agentHostInvitesAPIPath starts and reads invites.
const agentHostInvitesAPIPath = "/api/v1/agent-hosts/invites"

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

// A sign-in that just succeeded also supersedes this reason on a host kept
// from before: its account is a member again. The host recovers at its next
// reconcile.
const agentHostReasonGitHubMembershipEnded = "GitHubMembershipEnded"

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
	cmd.AddCommand(newHostStatusCmd(), newHostWakeCmd(), newHostDeleteCmd(), newHostInviteCmd())
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
// app lists it. A Failed host is waited on only for a reason the new link
// clears or one of the stale reasons.
func waitForAgentHost(ctx context.Context, cmd *cobra.Command, c *client.Client, connection string, stale ...string) error {
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "Waiting for your agent host...") //nolint:errcheck
	woken := false
	// The last failure seen that the new link or sign-in may clear: it is
	// waited out, and reported if the wait ends on it.
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
		// An operator's suspension is theirs to lift, and is checked first: a
		// host just suspended can still report the Ready of its last run. A
		// host that idled out is woken once, as 'orka host wake' would.
		case err == nil && host.OperatorSuspended:
			return fmt.Errorf("an operator suspended your agent host %s; ask them to resume it", host.Name)
		case err == nil && host.Ready:
			fmt.Fprintf(out, "Host ready: %s. Pick it in the Copilot app's Environments.\n", host.Environment) //nolint:errcheck
			return nil
		case err == nil && host.Suspended && !woken:
			if _, err := c.DoJSON(ctx, http.MethodPost, agentHostAPIPath+"/wake", nil, nil); err != nil {
				return agentHostError(err)
			}
			woken = true
			fmt.Fprintln(out, "Your agent host idled out; waking it.") //nolint:errcheck
		// A host kept from an earlier link still shows the failure that link
		// left until the controller sees the new one.
		case err == nil && host.Phase == agentHostPhaseFailed && (linkMayClear(host.Reason) || slices.Contains(stale, host.Reason)):
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

// inviteView is an invite and how it ended.
type inviteView struct {
	ID              string    `json:"id"`
	Login           string    `json:"login"`
	State           string    `json:"state"`
	Message         string    `json:"message"`
	UserCode        string    `json:"userCode"`
	VerificationURI string    `json:"verificationURI"`
	ExpiresAt       time.Time `json:"expiresAt"`
	AgentHost       bool      `json:"agentHost"`
}

// controllerWide sends no namespace: invites act in the namespace the
// controller watches, which the CLI's default namespace would contradict.
var controllerWide = map[string]string{"namespace": ""}

// invitePollInterval paces waiting for an invite's outcome.
var invitePollInterval = 3 * time.Second

func newHostInviteCmd() *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "invite <github-login>",
		Short: "Invite someone who cannot reach Orka to a Copilot environment",
		Long: "Start GitHub's device flow for a GitHub login. Pass the code on; once that person approves it with that " +
			"GitHub account, and is in an allowed org or team, Orka links their account and starts their Copilot " +
			"environment. They need only a browser and the Copilot app. Needs Kubernetes credentials allowed to " +
			"create agenthosts.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			body, err := json.Marshal(map[string]string{"login": args[0]})
			if err != nil {
				return err
			}
			raw, err := c.DoJSON(context.Background(), http.MethodPost, agentHostInvitesAPIPath, controllerWide, body)
			if err != nil {
				return err
			}
			var invite inviteView
			if err := decodeInto(raw, &invite); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Ask %s to open %s and enter the code %s before %s.\n", //nolint:errcheck
				invite.Login, invite.VerificationURI, invite.UserCode, invite.ExpiresAt.Local().Format(time.Kitchen))
			if invite.AgentHost {
				fmt.Fprintln(out, "Tell them first: approving starts their Copilot environment on Orka, and code running there can read their GitHub token.") //nolint:errcheck
			}
			outcome := agentHostInvitesAPIPath + "/" + url.PathEscape(invite.ID)
			stopped := fmt.Errorf("stopped waiting; the invite stays open until %s, and GET %s reports its outcome. "+
				"Running this command again creates another invite with a new code",
				invite.ExpiresAt.Local().Format(time.Kitchen), outcome)
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			for {
				select {
				case <-ctx.Done():
					return stopped
				case <-time.After(invitePollInterval):
				}
				raw, err := c.DoJSON(ctx, http.MethodGet, outcome, controllerWide, nil)
				if err != nil {
					if ctx.Err() != nil {
						return stopped
					}
					return err
				}
				var current inviteView
				if err := decodeInto(raw, &current); err != nil {
					return err
				}
				switch current.State {
				case "Accepted":
					fmt.Fprintf(out, "%s accepted: %s.\n", current.Login, current.Message) //nolint:errcheck
					return nil
				case "Refused", "Expired":
					return fmt.Errorf("the invite for %s was %s: %s", current.Login, strings.ToLower(current.State), current.Message)
				}
			}
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 16*time.Minute, "How long to wait for the invited person")
	return cmd
}
