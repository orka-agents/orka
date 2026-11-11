package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/evals"
)

func newEvalProposeCmd() *cobra.Command {
	var heldOut, target string
	cmd := &cobra.Command{
		Use:   "propose <training-run>",
		Short: "Ask a proposer to learn from a training run's failed trials and propose a candidate reviewer",
		Long: "Start a read-only proposer Task that reads the failed trials of a finished training run (never held-out " +
			"cases) and proposes one change to the reviewer's system prompt or model. When it finishes, the controller " +
			"creates the candidate Agent and a paired baseline and candidate EvalRun on a held-out case set. Follow it " +
			"with orka eval proposals. Only a person in a promoter group with the propose verb on evalruns may ask, and " +
			"each training run allows the gate's maxCandidatesPerBaseline proposals.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			request := map[string]any{}
			if heldOut != "" {
				request["heldOut"] = heldOut
			}
			if target != "" {
				grader, tag, _ := strings.Cut(target, ":")
				request["target"] = corev1alpha1.EvalExperimentTarget{Grader: corev1alpha1.EvalGraderName(grader), Tag: tag}
			}
			body, err := json.Marshal(request)
			if err != nil {
				return err
			}
			value, err := newClientFromCmd(cmd).DoJSON(cmd.Context(), http.MethodPost,
				evalRunsAPIPath+"/"+url.PathEscape(args[0])+"/proposals", nil, body)
			if err != nil {
				return err
			}
			created, _ := value.(map[string]any)
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Started proposal %v from evalrun/%s; follow it with: orka eval proposals %s\n",
				created["name"], args[0], args[0])
			return nil
		},
	}
	cmd.Flags().StringVar(&heldOut, "held-out", "", "Held-out case ConfigMap for the experiment (default: the one that lists the run's monitor)")
	cmd.Flags().StringVar(&target, "target", "", "Experiment target as grader or grader:tag (default: verdict)")
	return cmd
}

func newEvalProposalsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "proposals <training-run>",
		Short: "List a training run's proposals: hypothesis, candidate Agent and paired experiment",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			raw, _, err := newClientFromCmd(cmd).GetRaw(cmd.Context(), evalRunsAPIPath+"/"+url.PathEscape(args[0])+"/proposals", nil)
			if err != nil {
				return err
			}
			var page struct {
				Items []evals.Proposal `json:"items"`
			}
			if err := json.Unmarshal(raw, &page); err != nil {
				return fmt.Errorf("decode proposals: %w", err)
			}
			format, err := outputFormat(cmd)
			if err != nil {
				return err
			}
			if format != outputTable {
				return printStructuredTo(cmd.OutOrStdout(), format, page)
			}
			printEvalProposals(cmd.OutOrStdout(), page.Items)
			return nil
		},
	}
	addOutputFlag(cmd, outputTable)
	return cmd
}

func printEvalProposals(out io.Writer, items []evals.Proposal) {
	if len(items) == 0 {
		_, _ = fmt.Fprintln(out, "No proposals found.")
		return
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "PROPOSAL\tSTATE\tCANDIDATE AGENT\tCANDIDATE RUN\tAGE")
	for _, item := range items {
		state := item.State
		if state == "" {
			state = "Running"
			if item.Phase != "" {
				state = item.Phase
			}
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", item.Name, state, dashIfEmpty(item.CandidateAgent), dashIfEmpty(item.CandidateRun),
			formatAge(item.CreatedAt.Format(time.RFC3339)))
	}
	_ = w.Flush()
	for _, item := range items {
		switch {
		case item.Hypothesis != "":
			_, _ = fmt.Fprintf(out, "%s: %s\n", item.Name, item.Hypothesis)
		case item.Reason != "":
			_, _ = fmt.Fprintf(out, "%s refused: %s\n", item.Name, item.Reason)
		}
	}
}

func dashIfEmpty(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
