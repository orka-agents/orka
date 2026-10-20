/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/cli/client"

	"github.com/spf13/cobra"
)

const (
	connectionsAPIPath      = "/api/v1/connections"
	connectorsAPIPath       = "/api/v1/connectors"
	connectionModeReadOnly  = "readOnly"
	connectionModeReadWrite = "readWrite"
)

// connectionReadyPollInterval paces the wait for a consent to complete. It
// is a variable only so tests can poll a local server quickly.
var connectionReadyPollInterval = 2 * time.Second

// connectionView is the API's public view of a Connection.
type connectionView struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Provider  string `json:"provider"`
	Mode      string `json:"mode"`
	State     string `json:"state"`
	Ready     bool   `json:"ready"`
	LinkedAt  string `json:"linkedAt"`
	Message   string `json:"message"`
	// GrantSequence advances on every completed consent.
	GrantSequence int64 `json:"grantSequence"`
	// Deleting marks a disconnect that is still finishing.
	Deleting bool `json:"deleting"`
}

// connectionAuthorizeView is the API's response to a consent start.
type connectionAuthorizeView struct {
	Connection   connectionView `json:"connection"`
	AuthorizeURL string         `json:"authorizeURL"`
}

// connectionStateError is the API's state for a link that cannot be used.
const connectionStateError = "Error"

func newConnectCmd() *cobra.Command {
	var mode string
	var noOpen, noWait bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "connect <provider>",
		Short: "Link one of your accounts to a connector provider",
		Long: "Start the OAuth consent for a connector provider as the signed-in person, open the consent page in your " +
			"browser, and wait for the link to become ready. The token you use must identify you as a person " +
			"(OIDC or context token); ServiceAccount tokens cannot link accounts.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if mode != connectionModeReadOnly && mode != connectionModeReadWrite {
				return fmt.Errorf("--mode must be %s or %s", connectionModeReadOnly, connectionModeReadWrite)
			}
			c := newClientFromCmd(cmd)
			body, err := json.Marshal(map[string]string{"provider": args[0], "mode": mode})
			if err != nil {
				return err
			}
			raw, err := c.DoJSON(context.Background(), http.MethodPost, connectionsAPIPath, nil, body)
			if err != nil {
				return connectionError(err)
			}
			var started connectionAuthorizeView
			if err := decodeInto(raw, &started); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if started.AuthorizeURL == "" {
				fmt.Fprintf(out, "Connection %s to %s is already %s (%s)\n", started.Connection.Name, started.Connection.Provider, strings.ToLower(started.Connection.State), started.Connection.Mode) //nolint:errcheck
				return nil
			}
			fmt.Fprintf(out, "Authorize %s (%s) at:\n%s\n", args[0], mode, started.AuthorizeURL) //nolint:errcheck
			if noOpen {
				fmt.Fprintln(out, "Browser opening skipped. Open the URL above in your browser to finish.") //nolint:errcheck
			} else if err := openBrowser(started.AuthorizeURL); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Could not open browser: %v\nOpen the URL above manually.\n", err) //nolint:errcheck
			}
			// The provider sends the browser back to the dashboard, which
			// finishes the link only when it is signed in as this person.
			// The consent is sealed in the Connection's namespace, so the
			// fallback names it explicitly rather than trusting a later
			// invocation's default.
			fmt.Fprintf(out, "After consenting, the dashboard finishes the link if it is signed in as you. Otherwise copy the value after '#completion=' from the address bar and run:\n  %s\n", completionCommand(started.Connection)) //nolint:errcheck
			if noWait {
				return nil
			}
			fmt.Fprintf(out, "Waiting up to %s for the link to become ready...\n", timeout) //nolint:errcheck
			// The deadline bounds the requests themselves, not only the
			// checks between them, so a stalled poll cannot outlive --timeout.
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			for {
				raw, err := c.DoJSON(ctx, http.MethodGet, connectionsAPIPath+"/"+url.PathEscape(started.Connection.Name), nil, nil)
				if err != nil {
					if ctx.Err() != nil {
						return fmt.Errorf("connection %s did not become ready within %s; finish the consent and run 'orka connection get %s'", started.Connection.Name, timeout, started.Connection.Name)
					}
					// The API reads through a cache that may not hold the
					// Connection the POST just created; a 404 here is a
					// moment too early, not a verdict.
					if strings.Contains(err.Error(), "HTTP 404") {
						select {
						case <-ctx.Done():
							return fmt.Errorf("connection %s did not become ready within %s; finish the consent and run 'orka connection get %s'", started.Connection.Name, timeout, started.Connection.Name)
						case <-time.After(connectionReadyPollInterval):
						}
						continue
					}
					return connectionError(err)
				}
				var current connectionView
				if err := decodeInto(raw, &current); err != nil {
					return err
				}
				// A link that was already Ready stays Ready while the new
				// consent runs; only an advanced grant sequence proves this
				// consent finished rather than reporting the old grant.
				if current.Ready && (!started.Connection.Ready || current.GrantSequence > started.Connection.GrantSequence) {
					fmt.Fprintf(out, "Linked %s (%s)\n", current.Provider, current.Mode) //nolint:errcheck
					return nil
				}
				// A fresh consent was just started, so the state the link
				// had before it (Revoked, Expired, or a stale Error from a
				// provider that has since recovered) is what is being
				// repaired, not the outcome: only an Error that appeared
				// after the consent started is final.
				if current.State == connectionStateError && (started.Connection.State != connectionStateError || current.Message != started.Connection.Message) {
					return fmt.Errorf("connection %s is %s: %s", current.Name, current.State, current.Message)
				}
				select {
				case <-ctx.Done():
					return fmt.Errorf("connection %s is still %s after %s; finish the consent and run 'orka connection get %s'", current.Name, current.State, timeout, current.Name)
				case <-time.After(connectionReadyPollInterval):
				}
			}
		},
	}
	cmd.Flags().StringVar(&mode, "mode", connectionModeReadOnly, "Link mode: readOnly or readWrite (write tools ask for approval)")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "Print the consent URL without opening a browser")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "Return as soon as consent has started")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Minute, "How long to wait for the link to become ready")
	return cmd
}

// completionCommand is the CLI fallback that finishes a consent when the
// dashboard cannot, in the namespace the consent was sealed in.
func completionCommand(connection connectionView) string {
	command := "orka connection complete " + connection.Name
	if strings.TrimSpace(connection.Namespace) != "" {
		command += " --namespace " + connection.Namespace
	}
	return command + " --completion <value>"
}

// providerReadiness returns which connector providers are accepted right
// now. A link's own conditions can lag a provider change, and credential
// resolution refuses a link whose provider is not accepted, so the CLI
// joins the two before calling a link ready.
func providerReadiness(ctx context.Context, c *client.Client) (map[string]bool, error) {
	raw, err := c.DoJSON(ctx, http.MethodGet, connectorsAPIPath, nil, nil)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Name  string `json:"name"`
			Ready bool   `json:"ready"`
		} `json:"items"`
	}
	if err := decodeInto(raw, &list); err != nil {
		return nil, err
	}
	ready := make(map[string]bool, len(list.Items))
	for _, item := range list.Items {
		ready[item.Name] = item.Ready
	}
	return ready, nil
}

// duplicateProviders names the providers a person holds more than one
// link to; credential resolution refuses every link to such a provider.
func duplicateProviders(items []connectionView) map[string]bool {
	counts := map[string]int{}
	for _, item := range items {
		counts[item.Provider]++
	}
	duplicated := map[string]bool{}
	for provider, count := range counts {
		if count > 1 {
			duplicated[provider] = true
		}
	}
	return duplicated
}

// duplicateProvidersFor lists the person's links to find duplicated providers.
func duplicateProvidersFor(ctx context.Context, c *client.Client) (map[string]bool, error) {
	raw, err := c.DoJSON(ctx, http.MethodGet, connectionsAPIPath, nil, nil)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []connectionView `json:"items"`
	}
	if err := decodeInto(raw, &list); err != nil {
		return nil, err
	}
	return duplicateProviders(list.Items), nil
}

// joinProviderReadiness folds the provider's acceptance and the person's
// duplicate links into the view.
// stateNotUsableSuffix marks a stored Ready state the API judged unusable.
const stateNotUsableSuffix = " (not usable)"

func joinProviderReadiness(item connectionView, providersReady map[string]bool, duplicated map[string]bool) connectionView {
	// A disconnect in progress (tokens being revoked, possibly retrying)
	// is neither ready nor something to relink.
	if item.Deleting {
		item.Ready = false
		item.State = "Disconnecting"
		item.Message = "this link is being disconnected; it will disappear once its tokens are revoked where the provider supports it and deleted"
		return item
	}
	if duplicated[item.Provider] {
		item.Ready = false
		item.State += " (duplicate link)"
		item.Message = "you hold several links to this provider; disconnect the extra ones before its tools can run"
		return item
	}
	if !item.Ready {
		// The API reports a link it judged unusable (its provider changed or
		// is gone) with the stored state still Ready; the state says so, and
		// 'orka connection get' shows why.
		if item.State == corev1alpha1.ConnectionStateReady {
			item.State += stateNotUsableSuffix
		}
		return item
	}
	if providerReady, configured := providersReady[item.Provider]; !configured || !providerReady {
		item.Ready = false
		item.State += " (provider unavailable)"
		if item.Message == "" {
			item.Message = "the provider is not accepted right now, so this link cannot be used"
		}
	}
	return item
}

// joinProviderReadinessInto is joinProviderReadiness for the raw API
// object, so structured output keeps every field the API returned.
func joinProviderReadinessInto(item map[string]any, providersReady map[string]bool, duplicated map[string]bool) {
	if deleting, _ := item["deleting"].(bool); deleting {
		item["ready"] = false
		item["state"] = "Disconnecting"
		item["message"] = "this link is being disconnected; it will disappear once its tokens are revoked where the provider supports it and deleted"
		return
	}
	if provider, _ := item["provider"].(string); duplicated[provider] {
		item["ready"] = false
		state, _ := item["state"].(string)
		item["state"] = state + " (duplicate link)"
		item["message"] = "you hold several links to this provider; disconnect the extra ones before its tools can run"
		return
	}
	ready, _ := item["ready"].(bool)
	if !ready {
		if state, _ := item["state"].(string); state == corev1alpha1.ConnectionStateReady {
			item["state"] = state + stateNotUsableSuffix
		}
		return
	}
	provider, _ := item["provider"].(string)
	if providerReady, configured := providersReady[provider]; configured && providerReady {
		return
	}
	item["ready"] = false
	state, _ := item["state"].(string)
	item["state"] = state + " (provider unavailable)"
	if message, _ := item["message"].(string); message == "" {
		item["message"] = "the provider is not accepted right now, so this link cannot be used"
	}
}

func newConnectionCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "connection", Short: "Manage your linked accounts"}
	cmd.AddCommand(newConnectionListCmd(), newConnectionGetCmd(), newConnectionCompleteCmd(), newConnectionDeleteCmd(), newConnectionProvidersCmd())
	return cmd
}

func newConnectionListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   cliListUse,
		Short: "List your linked accounts",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := newClientFromCmd(cmd)
			raw, err := c.DoJSON(context.Background(), http.MethodGet, connectionsAPIPath, nil, nil)
			if err != nil {
				return connectionError(err)
			}
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			providersReady, err := providerReadiness(cmd.Context(), c)
			if err != nil {
				return connectionError(err)
			}
			var list struct {
				Items []connectionView `json:"items"`
			}
			if err := decodeInto(raw, &list); err != nil {
				return err
			}
			duplicated := duplicateProviders(list.Items)
			if format != outputTable {
				// Structured output carries the same joined readiness as
				// the table, so scripts never see a link the resolver refuses.
				var payload struct {
					Items []map[string]any `json:"items"`
				}
				if err := decodeInto(raw, &payload); err != nil {
					return err
				}
				for i := range payload.Items {
					joinProviderReadinessInto(payload.Items[i], providersReady, duplicated)
				}
				return printStructuredTo(cmd.OutOrStdout(), format, payload)
			}
			if len(list.Items) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No linked accounts. Link one with 'orka connect <provider>'.") //nolint:errcheck
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tPROVIDER\tMODE\tSTATE\tREADY\tLINKED") //nolint:errcheck
			for _, item := range list.Items {
				item = joinProviderReadiness(item, providersReady, duplicated)
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%t\t%s\n", item.Name, item.Provider, item.Mode, item.State, item.Ready, item.LinkedAt) //nolint:errcheck
			}
			return w.Flush()
		},
	}
	addOutputFlag(cmd, outputTable)
	return cmd
}

func newConnectionGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Show one of your linked accounts",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			raw, err := c.DoJSON(context.Background(), http.MethodGet, connectionsAPIPath+"/"+url.PathEscape(args[0]), nil, nil)
			if err != nil {
				return connectionError(err)
			}
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			providersReady, err := providerReadiness(cmd.Context(), c)
			if err != nil {
				return connectionError(err)
			}
			// A duplicate link to the same provider makes every link to it
			// unusable, so one link is judged against the whole list.
			duplicated, err := duplicateProvidersFor(cmd.Context(), c)
			if err != nil {
				return connectionError(err)
			}
			if format != outputTable {
				var payload map[string]any
				if err := decodeInto(raw, &payload); err != nil {
					return err
				}
				joinProviderReadinessInto(payload, providersReady, duplicated)
				return printStructuredTo(cmd.OutOrStdout(), format, payload)
			}
			var item connectionView
			if err := decodeInto(raw, &item); err != nil {
				return err
			}
			item = joinProviderReadiness(item, providersReady, duplicated)
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			for _, row := range [][2]string{
				{"Name", item.Name}, {"Provider", item.Provider}, {"Mode", item.Mode}, {"State", item.State},
				{labelReady, fmt.Sprint(item.Ready)}, {"Linked", item.LinkedAt}, {"Message", item.Message},
			} {
				if row[1] != "" {
					fmt.Fprintf(w, "%s:\t%s\n", row[0], row[1]) //nolint:errcheck
				}
			}
			return w.Flush()
		},
	}
	addOutputFlag(cmd, outputTable)
	return cmd
}

func newConnectionCompleteCmd() *cobra.Command {
	var completion string
	cmd := &cobra.Command{
		Use:   "complete <name>",
		Short: "Finish a consent with the completion value the provider callback returned",
		Long: "After consent, the controller sends the browser to the dashboard with a one-time completion value in the " +
			"URL fragment (#completion=...). When the dashboard is not signed in as you, pass that value here to " +
			"finish the link as yourself; it is accepted exactly once.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			completion = strings.TrimSpace(completion)
			if completion == "" {
				return errors.New("--completion is required")
			}
			c := newClientFromCmd(cmd)
			body, err := json.Marshal(map[string]string{"completion": completion})
			if err != nil {
				return err
			}
			raw, err := c.DoJSON(context.Background(), http.MethodPost, connectionsAPIPath+"/"+url.PathEscape(args[0])+"/complete", nil, body)
			if err != nil {
				return connectionError(err)
			}
			var item connectionView
			if err := decodeInto(raw, &item); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Linked %s (%s): %s\n", item.Provider, item.Mode, strings.ToLower(item.State)) //nolint:errcheck
			return nil
		},
	}
	cmd.Flags().StringVar(&completion, "completion", "", "The value after '#completion=' in the dashboard URL the provider callback opened")
	return cmd
}

func newConnectionDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <name>",
		Short: "Disconnect a linked account and delete its tokens",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			if err := c.DeleteResource(context.Background(), connectionsAPIPath+"/"+url.PathEscape(args[0]), nil); err != nil {
				return connectionError(err)
			}
			// DELETE only starts the removal: the finalizer revokes the
			// tokens first and may retry, so nothing is claimed finished.
			fmt.Fprintf(cmd.OutOrStdout(), "Disconnect requested for %s: its tokens are being revoked where the provider supports it and deleted, and the link removed. Check with 'orka connection get %s'.\n", args[0], args[0]) //nolint:errcheck
			return nil
		},
	}
}

func newConnectionProvidersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "providers",
		Short: "List the connector providers you can link",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := newClientFromCmd(cmd)
			raw, err := c.DoJSON(context.Background(), http.MethodGet, connectorsAPIPath, nil, nil)
			if err != nil {
				return connectionError(err)
			}
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			if format != outputTable {
				return printStructuredTo(cmd.OutOrStdout(), format, raw)
			}
			var list struct {
				Items []struct {
					Name        string `json:"name"`
					DisplayName string `json:"displayName"`
					Ready       bool   `json:"ready"`
					Tools       []struct {
						Name  string `json:"name"`
						Class string `json:"class"`
					} `json:"tools"`
				} `json:"items"`
			}
			if err := decodeInto(raw, &list); err != nil {
				return err
			}
			if len(list.Items) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No connector providers are configured.") //nolint:errcheck
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tDISPLAY NAME\tREADY\tTOOLS") //nolint:errcheck
			for _, item := range list.Items {
				names := make([]string, 0, len(item.Tools))
				for _, tool := range item.Tools {
					names = append(names, tool.Name+" ("+tool.Class+")")
				}
				fmt.Fprintf(w, "%s\t%s\t%t\t%s\n", item.Name, item.DisplayName, item.Ready, strings.Join(names, ", ")) //nolint:errcheck
			}
			return w.Flush()
		},
	}
	addOutputFlag(cmd, outputTable)
	return cmd
}

// decodeInto re-encodes a generic JSON value into a typed view.
func decodeInto(raw any, target any) error {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, target)
}

// connectionError explains the one failure people hit first: a token that
// is not a person's.
func connectionError(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "HTTP 403") {
		if strings.Contains(err.Error(), "not authorized") {
			return errors.Join(err, errors.New("this context token is not delegated the scope your controller requires for this command (the connector-read scope to list, the connector-manage scope to link or disconnect)"))
		}
		return errors.Join(err, errors.New("linked accounts belong to a signed-in person: pass your OIDC token with --token, or a context token with --txn-token (ServiceAccount tokens cannot link accounts)"))
	}
	return err
}
