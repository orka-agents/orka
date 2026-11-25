/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/orka-agents/orka/internal/cli/client"
	oversightv1 "github.com/orka-agents/orka/pkg/oversight/v1"
)

// maxReplayBatchBytes keeps a replay batch under the server's 1 MiB limit.
const maxReplayBatchBytes = 900 << 10

func newOversightCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "oversight",
		Short: "Inspect oversight decisions and replay recorded ones",
		Long: "Oversight records what a monitoring pipeline decided about each agent action. " +
			"These commands are read-only apart from replay, which submits recorded decisions.",
	}
	cmd.AddCommand(newOversightReplayCmd(), newOversightPostureCmd(), newOversightDecisionsCmd())
	return cmd
}

type replayOptions struct {
	files      []string
	namespace  string
	runID      string
	batchSize  int
	pace       time.Duration
	shiftToNow bool
	dryRun     bool
	// preserveTiming keeps the recorded gaps between observations (divided
	// by speed, capped at maxGap), sends whatever is due together, and stamps
	// each decision with the time it is sent.
	preserveTiming bool
	speed          float64
	maxGap         time.Duration
	// idSuffix is appended to every ID and Task name, so one recorded take can
	// be played again as new decisions.
	idSuffix string
}

func newOversightReplayCmd() *cobra.Command {
	var opts replayOptions
	cmd := &cobra.Command{
		Use:   "replay --file decisions.jsonl",
		Short: "Submit recorded oversight decisions from JSON Lines files",
		Long: "Replay reads newline-delimited OversightDecision records (the pkg/oversight/v1 schema), " +
			"checks every line first, and submits them in bounded batches to the namespace's ingest endpoint. " +
			"It submits nothing when any line is invalid. Replaying the same records again is a no-op. " +
			"The caller needs a ServiceAccount token allowed to create oversightdecisions in the namespace.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := newClientFromCmd(cmd)
			if opts.namespace == "" {
				opts.namespace = c.Namespace
			}
			return runOversightReplay(cmd.Context(), c, opts, cmd.OutOrStdout(), time.Now)
		},
	}
	cmd.Flags().StringArrayVar(&opts.files, "file", nil, "JSON Lines file of decisions (repeatable; - reads stdin)")
	cmd.Flags().StringVar(&opts.namespace, "into", "", "Namespace to submit into (default: the selected namespace)")
	cmd.Flags().StringVar(&opts.runID, "run-id", "", "Set every decision's runID")
	cmd.Flags().IntVar(&opts.batchSize, "batch-size", 50, "Decisions per request (1-100)")
	cmd.Flags().DurationVar(&opts.pace, "pace", 0, "Wait this long between batches, to replay as a live stream")
	cmd.Flags().BoolVar(&opts.shiftToNow, "shift-to-now", false,
		"Shift every observedAt by the same amount so the newest record is now, keeping their spacing")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "Check the files and report what would be sent")
	cmd.Flags().BoolVar(&opts.preserveTiming, "preserve-timing", false,
		"Keep the recorded gaps between decisions, sending those due together, stamped with the time they are sent")
	cmd.Flags().Float64Var(&opts.speed, "speed", 1, "With --preserve-timing, play this many times faster")
	cmd.Flags().DurationVar(&opts.maxGap, "max-gap", 10*time.Second, "With --preserve-timing, wait at most this long between decisions")
	cmd.Flags().StringVar(&opts.idSuffix, "id-suffix", "", "Append this to every decision ID and Task name (for a repeat take)")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

// readReplayDecisions reads and validates every line, reporting each bad line
// by file and number.
func readReplayDecisions(files []string, stdin io.Reader) ([]oversightv1.OversightDecision, error) {
	var decisions []oversightv1.OversightDecision
	var problems []string
	for _, name := range files {
		var reader io.Reader
		if name == "-" {
			reader = stdin
		} else {
			f, err := os.Open(name) //nolint:gosec // The operator names the file to replay.
			if err != nil {
				return nil, err
			}
			defer f.Close() //nolint:errcheck
			reader = f
		}
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
		line := 0
		for scanner.Scan() {
			line++
			text := strings.TrimSpace(scanner.Text())
			if text == "" || strings.HasPrefix(text, "#") {
				continue
			}
			var d oversightv1.OversightDecision
			dec := json.NewDecoder(strings.NewReader(text))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&d); err != nil {
				problems = append(problems, fmt.Sprintf("%s:%d: %v", name, line, err))
				continue
			}
			if err := d.Validate(); err != nil {
				problems = append(problems, fmt.Sprintf("%s:%d: %v", name, line, err))
				continue
			}
			decisions = append(decisions, d)
		}
		if err := scanner.Err(); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	if len(problems) > 0 {
		if len(problems) > 20 {
			problems = append(problems[:20], fmt.Sprintf("... and %d more", len(problems)-20))
		}
		return nil, fmt.Errorf("invalid decisions, nothing submitted:\n  %s", strings.Join(problems, "\n  "))
	}
	return decisions, nil
}

func runOversightReplay(ctx context.Context, c *client.Client, opts replayOptions, out io.Writer, now func() time.Time) error {
	if opts.namespace == "" {
		return errors.New("a namespace is required: pass --into or --namespace")
	}
	if opts.batchSize < 1 || opts.batchSize > oversightv1.MaxBatchDecisions {
		return fmt.Errorf("--batch-size must be from 1 to %d", oversightv1.MaxBatchDecisions)
	}
	decisions, err := readReplayDecisions(opts.files, os.Stdin)
	if err != nil {
		return err
	}
	if len(decisions) == 0 {
		return errors.New("no decisions to replay")
	}
	sort.SliceStable(decisions, func(i, j int) bool { return decisions[i].ObservedAt.Before(decisions[j].ObservedAt) })
	if opts.shiftToNow {
		shift := now().UTC().Sub(decisions[len(decisions)-1].ObservedAt).Truncate(time.Second)
		for i := range decisions {
			decisions[i].ObservedAt = decisions[i].ObservedAt.Add(shift)
		}
	}
	if opts.runID != "" {
		for i := range decisions {
			decisions[i].RunID = opts.runID
		}
	}
	if opts.idSuffix != "" {
		applyIDSuffix(decisions, opts.idSuffix)
	}
	if opts.preserveTiming && !opts.dryRun {
		return playWithTiming(ctx, c, opts, decisions, out, now)
	}
	if opts.dryRun {
		_, _ = fmt.Fprintf(out, "%d decisions are valid; nothing submitted (dry run).\n", len(decisions))
		return nil
	}
	ingested, duplicates := 0, 0
	for start := 0; start < len(decisions); {
		end := min(start+opts.batchSize, len(decisions))
		for end > start+1 {
			raw, _ := json.Marshal(oversightv1.IngestRequest{Decisions: decisions[start:end]})
			if len(raw) <= maxReplayBatchBytes {
				break
			}
			end = start + (end-start)/2
		}
		response, err := c.IngestOversightDecisions(ctx, opts.namespace, decisions[start:end])
		if err != nil {
			return fmt.Errorf("submit decisions %d-%d (ingested %d, duplicates %d so far): %w", start+1, end, ingested, duplicates, err)
		}
		ingested += response.Ingested
		duplicates += response.Duplicates
		start = end
		if opts.pace > 0 && start < len(decisions) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(opts.pace):
			}
		}
	}
	_, _ = fmt.Fprintf(out, "Replayed %d decisions into %s: %d ingested, %d already stored.\n",
		len(decisions), opts.namespace, ingested, duplicates)
	return nil
}

func applyIDSuffix(decisions []oversightv1.OversightDecision, suffix string) {
	rename := func(id string) string {
		task, seq, found := strings.Cut(id, "@")
		if !found {
			return id + suffix
		}
		return task + suffix + "@" + seq
	}
	for i := range decisions {
		d := &decisions[i]
		d.ID, d.TaskName = rename(d.ID), d.TaskName+suffix
		for j := range d.Action.PriorActionIDs {
			d.Action.PriorActionIDs[j] = rename(d.Action.PriorActionIDs[j])
		}
	}
}

// playWithTiming replays a recorded take as a live stream.
func playWithTiming(ctx context.Context, c *client.Client, opts replayOptions, decisions []oversightv1.OversightDecision,
	out io.Writer, now func() time.Time) error {
	if opts.speed <= 0 {
		return errors.New("--speed must be positive")
	}
	// Each decision is due at its recorded offset from the first, divided by
	// speed and with every gap capped at maxGap. Whatever is due is sent in one
	// request (up to batchSize), so a dense take keeps to its schedule instead
	// of falling behind one request per decision.
	due := make([]time.Duration, len(decisions))
	for i := 1; i < len(decisions); i++ {
		gap := time.Duration(float64(decisions[i].ObservedAt.Sub(decisions[i-1].ObservedAt)) / opts.speed)
		due[i] = due[i-1] + min(max(gap, 0), opts.maxGap)
	}
	began := time.Now()
	ingested, duplicates := 0, 0
	for next := 0; next < len(decisions); {
		if wait := due[next] - time.Since(began); wait > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		elapsed := time.Since(began)
		batch := make([]oversightv1.OversightDecision, 0, opts.batchSize)
		stamp := now().UTC().Truncate(time.Millisecond)
		for next < len(decisions) && len(batch) < opts.batchSize && due[next] <= elapsed {
			d := decisions[next]
			d.ObservedAt = stamp
			batch = append(batch, d)
			next++
		}
		response, err := c.IngestOversightDecisions(ctx, opts.namespace, batch)
		if err != nil {
			return fmt.Errorf("submit %s (ingested %d so far): %w", batch[0].ID, ingested, err)
		}
		ingested += response.Ingested
		duplicates += response.Duplicates
	}
	_, _ = fmt.Fprintf(out, "Played %d decisions into %s: %d ingested, %d already stored.\n",
		len(decisions), opts.namespace, ingested, duplicates)
	return nil
}

func newOversightPostureCmd() *cobra.Command {
	var window, runID, output string
	cmd := &cobra.Command{
		Use:   "posture",
		Short: "Answer \"is anything untoward happening?\" for the namespace",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := newClientFromCmd(cmd)
			posture, err := c.GetOversightPosture(cmd.Context(), window, runID)
			if err != nil {
				return err
			}
			if output == "json" {
				return printJSONTo(cmd.OutOrStdout(), posture)
			}
			printPosture(cmd.OutOrStdout(), posture)
			return nil
		},
	}
	cmd.Flags().StringVar(&window, "window", "24h", "How far back to look, such as 1h, 24h or 7d")
	cmd.Flags().StringVar(&runID, "run-id", "", "Only decisions of this run")
	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format: json")
	return cmd
}

func printJSONTo(out io.Writer, value any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func printPosture(out io.Writer, p *oversightv1.PostureResponse) {
	line := func(name string, s oversightv1.PostureState) {
		_, _ = fmt.Fprintf(out, "%-11s %s: %s\n", name, strings.ToUpper(s.State), s.Summary)
		if len(s.Reasons) > 0 {
			_, _ = fmt.Fprintf(out, "%-11s   reasons: %s\n", "", strings.Join(s.Reasons, ", "))
		}
	}
	_, _ = fmt.Fprintf(out, "Namespace %s, last %s: %d actions by %d agents in %d tasks\n",
		p.Namespace, p.Window, p.Basis.Actions, p.Basis.Agents, p.Basis.Tasks)
	line("Confidence", p.Confidence)
	line("Visibility", p.Visibility)
	line("Control", p.Control)
	_, _ = fmt.Fprintf(out, "Signals     %d consistent, %d risky, %d against the charter, %d monitor unavailable; %d would pause\n",
		p.Basis.Signals[string(oversightv1.SignalConsistent)], p.Basis.Signals[string(oversightv1.SignalRisky)],
		p.Basis.Signals[string(oversightv1.SignalAgainstCharter)], p.Basis.Signals[string(oversightv1.SignalMonitorUnavailable)],
		p.Basis.WouldHavePaused)
	if len(p.Basis.Coverage.BlindSpots) > 0 {
		spots := make([]string, 0, len(p.Basis.Coverage.BlindSpots))
		for spot, n := range p.Basis.Coverage.BlindSpots {
			spots = append(spots, fmt.Sprintf("%s (%d)", spot, n))
		}
		sort.Strings(spots)
		_, _ = fmt.Fprintf(out, "Blind spots %s\n", strings.Join(spots, ", "))
	}
	if p.Incomplete {
		_, _ = fmt.Fprintln(out, "Note: the window held more decisions than were read; counts are partial.")
	}
}

func newOversightDecisionsCmd() *cobra.Command {
	var query, window, output string
	var limit int
	cmd := &cobra.Command{
		Use:   "decisions [query]",
		Short: "Search oversight decisions, newest first",
		Long: "The query uses the oversight search grammar: key:value terms such as dest:example.com:443, " +
			"domain:example.com, bin:curl, concern:K2, agent:name, task:prefix*, disposition:audit_flagged|fail_closed, " +
			"feedback:unreviewed, plus free-text words or \"quoted phrases\".",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				query = strings.TrimSpace(query + " " + args[0])
			}
			c := newClientFromCmd(cmd)
			list, err := c.ListOversightDecisions(cmd.Context(), query, window, limit, "")
			if err != nil {
				return err
			}
			if output == "json" {
				return printJSONTo(cmd.OutOrStdout(), list)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "SEQ\tOBSERVED\tSIGNAL\tDISPOSITION\tAGENT\tTASK\tTOOL\tACTION")
			for _, d := range list.Items {
				_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", d.Seq, d.ObservedAt.Format(time.RFC3339), d.Signal,
					d.Disposition, dash(d.AgentName), d.TaskName, d.Action.ToolName, d.ID)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if list.Continue != "" {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "More results: raise --limit (next page starts before seq %s).\n", list.Continue)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&query, "query", "q", "", "Search query")
	cmd.Flags().StringVar(&window, "window", "", "How far back to look, such as 24h or 7d (default: all)")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum decisions to show (up to 200)")
	cmd.Flags().StringVarP(&output, "output", "o", "", "Output format: json")
	return cmd
}
