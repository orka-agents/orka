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
			"system prompt and model: an inline prompt is copied inline, and a prompt read from a ConfigMap stays a reference " +
			"to that ConfigMap, so keep the ConfigMap as it is. Find the decision ID with orka eval decisions. Only a person in a " +
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

// agentRolloutBody is the request POST /api/v1/agents/:name/rollout takes.
type agentRolloutBody struct {
	Action     string `json:"action"`
	DecisionID string `json:"decisionID,omitempty"`
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
	// Structured output is the API's response as it came: its prompt views
	// carry digests and sizes a decoded record would drop.
	if format != outputTable {
		return printStructuredTo(cmd.OutOrStdout(), format, value)
	}
	switch record.Action {
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
			return printItemsPage(cmd, agentsRolloutAPIPath+"/"+url.PathEscape(args[0])+"/promotions", "promotions", printAgentPromotions)
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
		var response any
		if err := json.Unmarshal(raw, &response); err != nil {
			return fmt.Errorf("decode %s: %w", what, err)
		}
		return printStructuredTo(cmd.OutOrStdout(), format, response)
	}
	table(cmd.OutOrStdout(), page.Items)
	return nil
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
		if item.Action == store.AgentPromotionPromote && item.CandidateAgent != nil {
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
		if item.StateReason != "" {
			_, _ = fmt.Fprintf(out, "%s %s: %s\n", item.ID, item.State, item.StateReason)
		}
	}
}
