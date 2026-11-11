package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

const agentsRolloutAPIPath = "/api/v1/agents"

func newAgentPromoteCmd() *cobra.Command {
	var decision string
	cmd := &cobra.Command{
		Use:   "promote <agent> --decision <id>",
		Short: "Write an approved candidate's system prompt and model onto the Agent it was tested against",
		Long: "Promote the candidate an approval names onto the stable Agent. The approval must be the latest decision for " +
			"its experiment, and the experiment's candidate, baseline, cases, graders and gate must still match the live " +
			"configuration; the Agent must not have changed since its baseline run. The Agent then carries the candidate's " +
			"system prompt inline and its model. Find the decision ID with orka eval decisions. Only a person in a " +
			"promoter group with the promote permission on agents may promote. Undo it with orka agent rollback.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(decision) == "" {
				return errors.New("--decision is required")
			}
			return postAgentRollout(cmd, args[0], agentRolloutBody{Action: store.AgentPromotionPromote, DecisionID: strings.TrimSpace(decision)})
		},
	}
	cmd.Flags().StringVar(&decision, "decision", "", "ID of the approval to carry out (required)")
	addOutputFlag(cmd, outputTable)
	return cmd
}

func newAgentRollbackCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "rollback <agent> [--reason <text>]",
		Short: "Write back the system prompt and model the Agent's latest promotion replaced",
		Long: "Roll back the Agent's latest promotion. The Agent must still hold exactly what that promotion wrote: an edit " +
			"made since is never overwritten, and the rollback is refused instead. Only a person in a promoter group with " +
			"the promote permission on agents may roll back.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reason = strings.TrimSpace(reason)
			if len(reason) > store.MaxEvalDecisionReasonBytes {
				return fmt.Errorf("--reason is %d bytes; at most %d", len(reason), store.MaxEvalDecisionReasonBytes)
			}
			return postAgentRollout(cmd, args[0], agentRolloutBody{Action: store.AgentPromotionRollback, Reason: reason})
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Why, in a sentence or two")
	addOutputFlag(cmd, outputTable)
	return cmd
}

func newAgentCanaryCmd() *cobra.Command {
	var decision string
	var percent int32
	cmd := &cobra.Command{
		Use:   "canary <agent> --decision <id> --percent <1-100>",
		Short: "Run a share of the Agent's new Sessions and Tasks on an approved candidate",
		Long: "Start a canary, or change its share. The approval is checked as for a promotion. Each new Session, or Task " +
			"without a Session, is routed to the candidate when a hash of its name falls below the percent, after the " +
			"approval is checked again; a Session keeps its arm. EvalRun trials are never routed. If a check fails, " +
			"Tasks run on the stable Agent and the Agent is marked RolloutInvalid until the canary is aborted or restarted. " +
			"Finish with orka agent promote, or stop with orka agent abort.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(decision) == "" {
				return errors.New("--decision is required")
			}
			if percent < 1 || percent > 100 {
				return errors.New("--percent must be 1 to 100")
			}
			return postAgentRollout(cmd, args[0], agentRolloutBody{
				Action: store.AgentPromotionCanary, DecisionID: strings.TrimSpace(decision), Percent: percent,
			})
		},
	}
	cmd.Flags().StringVar(&decision, "decision", "", "ID of the approval to canary (required)")
	cmd.Flags().Int32Var(&percent, "percent", 0, "Share of new Sessions and Tasks to route to the candidate, 1 to 100 (required)")
	addOutputFlag(cmd, outputTable)
	return cmd
}

func newAgentAbortCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "abort <agent> [--reason <text>]",
		Short: "End the Agent's canary; new Tasks run on the stable Agent",
		Long: "Abort the running canary. New Tasks run on the stable Agent; a Session that started on the candidate can't " +
			"continue and needs a new Session. Tasks already running finish where they started.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return postAgentRollout(cmd, args[0], agentRolloutBody{Action: store.AgentPromotionAbort, Reason: strings.TrimSpace(reason)})
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Why, in a sentence or two")
	addOutputFlag(cmd, outputTable)
	return cmd
}

// agentRolloutBody is the request POST /api/v1/agents/:name/rollout takes.
type agentRolloutBody struct {
	Action     string `json:"action"`
	DecisionID string `json:"decisionID,omitempty"`
	Percent    int32  `json:"percent,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

func postAgentRollout(cmd *cobra.Command, agent string, request agentRolloutBody) error {
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	value, err := newClientFromCmd(cmd).DoJSON(cmd.Context(), http.MethodPost, agentsRolloutAPIPath+"/"+url.PathEscape(agent)+"/rollout", nil, body)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var record store.AgentPromotionRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return fmt.Errorf("decode the rollout record: %w", err)
	}
	format, err := outputFormat(cmd)
	if err != nil {
		return err
	}
	if format != outputTable {
		return printStructuredTo(cmd.OutOrStdout(), format, record)
	}
	switch record.Action {
	case store.AgentPromotionCanary:
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Routing %d%% of new Sessions and Tasks for agent/%s to %s (record %s)\n",
			record.Percent, record.Agent, record.CandidateAgent.Name, record.ID)
	case store.AgentPromotionAbort:
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Ended the canary on agent/%s; new Tasks run on it (record %s)\n", record.Agent, record.ID)
	case store.AgentPromotionPromote:
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Promoted %s onto agent/%s (record %s, generation %d); undo it with: orka agent rollback %s\n",
			record.CandidateAgent.Name, record.Agent, record.ID, record.AppliedGeneration, record.Agent)
	default:
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Rolled back agent/%s to its values before promotion %s (record %s, generation %d)\n",
			record.Agent, record.PromotionID, record.ID, record.AppliedGeneration)
	}
	return nil
}

func newAgentPromotionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "promotions <agent>",
		Short: "List the Agent's promotions and rollbacks, oldest first",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, _, err := newClientFromCmd(cmd).GetRaw(cmd.Context(), agentsRolloutAPIPath+"/"+url.PathEscape(args[0])+"/promotions", nil)
			if err != nil {
				return err
			}
			var page agentPromotionsPage
			if err := json.Unmarshal(raw, &page); err != nil {
				return fmt.Errorf("decode promotions: %w", err)
			}
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			if format != outputTable {
				return printStructuredTo(cmd.OutOrStdout(), format, page)
			}
			if page.Rollout != nil && page.Rollout.Candidate != nil {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Canary: %d%% of new Sessions and Tasks run on %s (decision %s)\n",
					page.Rollout.Percent, page.Rollout.Candidate.Name, page.Rollout.DecisionID)
			}
			if page.RolloutInvalid != "" {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "RolloutInvalid: %s; Tasks run on the stable Agent\n", page.RolloutInvalid)
			}
			printAgentPromotions(cmd.OutOrStdout(), page.Items)
			return nil
		},
	}
	addOutputFlag(cmd, outputTable)
	return cmd
}

// printItemsPage reads an {"items": [...]} page and prints it as a table or
// in the requested structured format.
func printItemsPage[T any](cmd *cobra.Command, path, what string, table func(io.Writer, []T)) error {
	raw, _, err := newClientFromCmd(cmd).GetRaw(cmd.Context(), path, nil)
	if err != nil {
		return err
	}
	var page struct {
		Items []T `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return fmt.Errorf("decode %s: %w", what, err)
	}
	format, err := outputFormat(cmd)
	if err != nil {
		return err
	}
	if format != outputTable {
		return printStructuredTo(cmd.OutOrStdout(), format, page)
	}
	table(cmd.OutOrStdout(), page.Items)
	return nil
}

// agentPromotionsPage is GET /api/v1/agents/:name/promotions.
type agentPromotionsPage struct {
	Items          []store.AgentPromotionRecord     `json:"items"`
	Rollout        *corev1alpha1.AgentRolloutStatus `json:"rollout,omitempty"`
	RolloutInvalid string                           `json:"rolloutInvalid,omitempty"`
}

func printAgentPromotions(out io.Writer, items []store.AgentPromotionRecord) {
	if len(items) == 0 {
		_, _ = fmt.Fprintln(out, "No promotions found.")
		return
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "RECORD\tACTION\tSTATE\tFROM\tBY\tGENERATION\tAGE")
	for _, item := range items {
		from := item.PromotionID
		if (item.Action == store.AgentPromotionPromote || item.Action == store.AgentPromotionCanary) && item.CandidateAgent != nil {
			from = item.CandidateAgent.Name
		}
		generation := "-"
		if item.AppliedGeneration != 0 {
			generation = fmt.Sprint(item.AppliedGeneration)
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", item.ID, item.Action, item.State, dashIfEmpty(from),
			dashIfEmpty(item.Actor.Username), generation, formatAge(item.CreatedAt.Format(time.RFC3339)))
	}
	_ = w.Flush()
	for _, item := range items {
		if item.Reason != "" {
			_, _ = fmt.Fprintf(out, "%s: %s\n", item.ID, item.Reason)
		}
	}
}
