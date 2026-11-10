package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/cli/client"
	"github.com/orka-agents/orka/internal/store"
)

const evalExperimentsAPIPath = "/api/v1/evals/experiments"

// evalExperiment is one experiment as the decisions endpoints return it: the
// reservation, the stored outcome, every decision, the one in force and, for
// an approval, whether it still matches the live configuration.
type evalExperiment struct {
	store.EvalExperimentEntry
	Effective *store.EvalDecisionRecord `json:"effective,omitempty"`
	Approval  *struct {
		Current bool     `json:"current"`
		Changed []string `json:"changed,omitempty"`
	} `json:"approval,omitempty"`
}

func newEvalExperimentsCmd() *cobra.Command {
	var limit int
	var next string
	cmd := &cobra.Command{
		Use:   "experiments",
		Short: "List experiments with their outcomes and decisions, including ones whose EvalRuns are gone",
		Long: "List the namespace's experiments, oldest first, with each one's stored outcome and the decision in " +
			"force. Experiments stay listed after their EvalRuns are deleted. Pass --continue with the token the " +
			"previous page printed to read the next page.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			query := map[string]string{}
			if limit > 0 {
				query["limit"] = strconv.Itoa(limit)
			}
			if next != "" {
				query["continue"] = next
			}
			raw, _, err := newClientFromCmd(cmd).GetRaw(cmd.Context(), evalExperimentsAPIPath, query)
			if err != nil {
				return err
			}
			var page struct {
				Items    []evalExperiment `json:"items"`
				Continue string           `json:"continue,omitempty"`
			}
			if err := json.Unmarshal(raw, &page); err != nil {
				return fmt.Errorf("decode experiments: %w", err)
			}
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			if format != outputTable {
				return printStructuredTo(cmd.OutOrStdout(), format, page)
			}
			printEvalExperiments(cmd.OutOrStdout(), page.Items)
			if page.Continue != "" {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "More experiments: orka eval experiments --continue %s\n", page.Continue)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "Experiments per page (server default 50, at most 200)")
	cmd.Flags().StringVar(&next, "continue", "", "Continue from the token a previous page printed")
	addOutputFlag(cmd, outputTable)
	return cmd
}

func printEvalExperiments(out io.Writer, items []evalExperiment) {
	if len(items) == 0 {
		_, _ = fmt.Fprintln(out, "No experiments found.")
		return
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "CANDIDATE\tBASELINE\tRESULT\tTARGET PASS@1\tDECISION\tBY\tAGE")
	for _, item := range items {
		result, target := string(corev1alpha1.EvalExperimentPending), "-"
		if outcome := item.Outcome; outcome != nil {
			result = outcome.Result
			if t := outcome.Target; t != nil {
				target = fmt.Sprintf("%d/%d -> %d/%d", t.BaselinePassedTrials, t.BaselineTrials, t.CandidatePassedTrials, t.CandidateTrials)
			}
		}
		decision, by := "-", "-"
		if effective := item.Effective; effective != nil {
			decision, by = effective.Decision, effective.Actor.Username
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", item.Experiment.CandidateRun, item.Experiment.BaselineRun,
			result, target, decision, by, formatAge(item.Experiment.ReservedAt.Format(time.RFC3339)))
	}
	_ = w.Flush()
}

func newEvalDecisionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "decisions <candidate>",
		Short: "Show a candidate's experiment outcome, its decisions and whether an approval still holds",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			experiment, err := getEvalExperiment(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			if format != outputTable {
				return printStructuredTo(cmd.OutOrStdout(), format, experiment)
			}
			printEvalDecisions(cmd.OutOrStdout(), experiment)
			return nil
		},
	}
	addOutputFlag(cmd, outputTable)
	return cmd
}

func getEvalExperiment(ctx context.Context, c *client.Client, name string) (*evalExperiment, error) {
	raw, _, err := c.GetRaw(ctx, evalRunsAPIPath+"/"+url.PathEscape(name)+"/decisions", nil)
	if err != nil {
		return nil, err
	}
	var experiment evalExperiment
	if err := json.Unmarshal(raw, &experiment); err != nil {
		return nil, fmt.Errorf("decode the experiment of evalrun/%s: %w", name, err)
	}
	return &experiment, nil
}

func printEvalDecisions(out io.Writer, experiment *evalExperiment) {
	record := experiment.Experiment
	_, _ = fmt.Fprintf(out, "Experiment %s/%s against baseline %s\n", record.Namespace, record.CandidateRun, record.BaselineRun)
	if record.Hypothesis != "" {
		_, _ = fmt.Fprintf(out, "Hypothesis: %s\n", record.Hypothesis)
	}
	outcome := experiment.Outcome
	if outcome == nil {
		_, _ = fmt.Fprintln(out, "Outcome: not decided yet")
	} else {
		_, _ = fmt.Fprintf(out, "Outcome: %s at %s\n", outcome.Result, outcome.DecidedAt.Format(time.RFC3339))
		if t := outcome.Target; t != nil {
			name := t.Grader
			if t.Tag != "" {
				name += " on " + t.Tag
			}
			_, _ = fmt.Fprintf(out, "Target %s pass@1: %d/%d -> %d/%d (%s points); paired trials %d improved, %d regressed; p = %s\n",
				name, t.BaselinePassedTrials, t.BaselineTrials, t.CandidatePassedTrials, t.CandidateTrials, t.ImprovementPoints,
				t.ImprovedPairs, t.RegressedPairs, t.PValue)
		}
		for _, reason := range outcome.Reasons {
			_, _ = fmt.Fprintf(out, "  - %s\n", reason)
		}
		if binding := outcome.Binding; binding != nil {
			_, _ = fmt.Fprintf(out, "Candidate Agent: %s (generation %d); rollback target: %s (generation %d)\n",
				binding.CandidateAgent.Name, binding.CandidateAgent.Generation, binding.BaselineAgent.Name, binding.BaselineAgent.Generation)
		}
	}
	if len(experiment.Decisions) == 0 {
		_, _ = fmt.Fprintln(out, "Decisions: none")
		return
	}
	_, _ = fmt.Fprintln(out, "Decisions (latest is in force):")
	for _, decision := range experiment.Decisions {
		_, _ = fmt.Fprintf(out, "  %s  %s by %s: %s\n", decision.DecidedAt.Format(time.RFC3339), decision.Decision,
			decision.Actor.Username, decision.Reason)
	}
	if approval := experiment.Approval; approval != nil {
		if approval.Current {
			_, _ = fmt.Fprintln(out, "Approval: current; the live configuration still matches what was approved")
		} else {
			_, _ = fmt.Fprintf(out, "Approval: stale; changed since approval: %s\n", strings.Join(approval.Changed, ", "))
		}
	}
}

func newEvalDecideCmd(decision string) *cobra.Command {
	var reason string
	verb := map[string]string{store.EvalDecisionApproved: "approve", store.EvalDecisionRejected: "reject"}[decision]
	cmd := &cobra.Command{
		Use:   verb + " <candidate> --reason <text>",
		Short: fmt.Sprintf("Record that you %s a decided experiment", verb),
		Long: "Append a decision to a candidate's experiment. Decisions are never edited; the latest one is in force. " +
			"Only a person in a promoter group with the decide permission on evalruns may decide, and an approval needs " +
			"a Passed experiment whose candidate, baseline, cases, graders and gate still match the live configuration. " +
			"Recording a decision changes no Agent; roll the change out separately.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reason = strings.TrimSpace(reason)
			if reason == "" {
				return errors.New("--reason is required")
			}
			if len(reason) > store.MaxEvalDecisionReasonBytes {
				return fmt.Errorf("--reason is %d bytes; at most %d", len(reason), store.MaxEvalDecisionReasonBytes)
			}
			body, err := json.Marshal(struct {
				Decision string `json:"decision"`
				Reason   string `json:"reason"`
			}{decision, reason})
			if err != nil {
				return err
			}
			c := newClientFromCmd(cmd)
			value, err := c.DoJSON(cmd.Context(), http.MethodPost, evalRunsAPIPath+"/"+url.PathEscape(args[0])+"/decisions", nil, body)
			if err != nil {
				return err
			}
			raw, err := json.Marshal(value)
			if err != nil {
				return err
			}
			var recorded store.EvalDecisionRecord
			if err := json.Unmarshal(raw, &recorded); err != nil {
				return fmt.Errorf("decode the recorded decision: %w", err)
			}
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			if format != outputTable {
				return printStructuredTo(cmd.OutOrStdout(), format, recorded)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Recorded %s for evalrun/%s by %s (decision %s)\n", recorded.Decision,
				recorded.CandidateRun, recorded.Actor.Username, recorded.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "Why, in a sentence or two (required)")
	addOutputFlag(cmd, outputTable)
	return cmd
}
