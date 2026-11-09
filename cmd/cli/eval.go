/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	sigsyaml "sigs.k8s.io/yaml"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/cli/client"
	"github.com/orka-agents/orka/internal/store"
)

const evalRunsAPIPath = "/api/v1/evals/runs"

// evalRunPollInterval paces --wait. It is a variable only so tests can poll a
// local server quickly.
var evalRunPollInterval = 10 * time.Second

func newEvalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "eval",
		Short: "Run and inspect capability evals (EvalRuns)",
	}
	cmd.AddCommand(newEvalRunCmd())
	cmd.AddCommand(newEvalGetCmd())
	return cmd
}

func newEvalRunCmd() *cobra.Command {
	var file, output string
	var wait bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "run -f <evalrun.yaml>",
		Short: "Create an EvalRun, optionally wait for it, and save its results",
		Long: "Create an EvalRun from a manifest (YAML or JSON with metadata and spec). The cases ConfigMap and " +
			"RepositoryMonitor it references must already exist, and the controller must run with --enable-evals.\n\n" +
			"With --wait the command polls until the run finishes, prints its results, and fails unless the run " +
			"Succeeded; it refuses a ttlSecondsAfterFinished below 300. --output-dir <dir> implies --wait, refuses " +
			"any ttlSecondsAfterFinished, and writes evalrun.json, trials.jsonl, summary.md and the events of every " +
			"failed trial under events/.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if file == "" {
				return errors.New("-f is required")
			}
			body, err := evalRunRequestFromFile(file)
			if err != nil {
				return err
			}
			if wait || output != "" {
				if err := checkEvalRunTTLForWait(body, output != ""); err != nil {
					return err
				}
			}
			c := newClientFromCmd(cmd)
			ctx := cmd.Context()
			run, err := createEvalRun(ctx, c, body)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "evalrun/%s created in %s\n", run.Name, run.Namespace)
			if !wait && output == "" {
				return nil
			}
			return followEvalRun(ctx, c, run, timeout, output, out)
		},
	}
	cmd.Flags().StringVarP(&file, "filename", "f", "", "EvalRun manifest (YAML or JSON)")
	cmd.Flags().BoolVar(&wait, "wait", false, "Wait for the run to finish and print its results")
	cmd.Flags().StringVar(&output, "output-dir", "", "Write the run, trial records, a summary and failed-trial events here (implies --wait)")
	cmd.Flags().DurationVar(&timeout, "timeout", 2*time.Hour, "How long --wait waits")
	return cmd
}

func newEvalGetCmd() *cobra.Command {
	var output string
	var wait bool
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Show an EvalRun's phase, counts and results, or wait for it and save them",
		Long: "Show an EvalRun's phase, counts and per-grader results. With --wait or --output-dir it follows the run " +
			"like orka eval run does, so a run whose CLI exited can be picked up again.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClientFromCmd(cmd)
			run, err := getEvalRun(cmd.Context(), c, "", args[0])
			if err != nil {
				return err
			}
			if !wait && output == "" {
				printEvalRunResults(cmd.OutOrStdout(), run)
				return nil
			}
			if output != "" && run.Spec.TTLSecondsAfterFinished != nil {
				return errors.New("--output-dir needs the run kept until the export finishes, but it has ttlSecondsAfterFinished")
			}
			// A finished run is printed from this read; an unfinished one is
			// polled, and a short TTL could delete it between polls.
			if !evalRunFinished(run.Status.Phase) {
				if err := checkEvalRunTTL(run.Spec.TTLSecondsAfterFinished, false); err != nil {
					return err
				}
			}
			return followEvalRun(cmd.Context(), c, run, timeout, output, cmd.OutOrStdout())
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "Wait for the run to finish and print its results")
	cmd.Flags().StringVar(&output, "output-dir", "", "Write the run, trial records, a summary and failed-trial events here (implies --wait)")
	cmd.Flags().DurationVar(&timeout, "timeout", 2*time.Hour, "How long --wait waits")
	return cmd
}

// followEvalRun waits for run to finish, prints its results, writes them to
// output when set, and fails unless the run Succeeded.
func followEvalRun(ctx context.Context, c *client.Client, run *corev1alpha1.EvalRun, timeout time.Duration, output string, out io.Writer) error {
	run, err := waitForEvalRun(ctx, c, run, timeout, out)
	if err != nil {
		return err
	}
	printEvalRunResults(out, run)
	if output != "" {
		if err := writeEvalRunOutput(ctx, c, run, output); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "Results written to %s\n", output)
	}
	if run.Status.Phase != corev1alpha1.EvalRunPhaseSucceeded {
		return fmt.Errorf("evalrun/%s ended %s", run.Name, run.Status.Phase)
	}
	if experiment := run.Status.Experiment; experiment != nil && experiment.Result != corev1alpha1.EvalExperimentPassed {
		return fmt.Errorf("evalrun/%s experiment is %s", run.Name, experiment.Result)
	}
	if run.Spec.Experiment != nil && run.Status.Experiment == nil {
		return fmt.Errorf("evalrun/%s declares an experiment but has no experiment decision", run.Name)
	}
	return nil
}

// evalRunRequest is the create request body: the manifest's metadata and
// spec. apiVersion and kind, when present, must name an EvalRun.
type evalRunRequest struct {
	Metadata evalRunRequestMetadata   `json:"metadata"`
	Spec     corev1alpha1.EvalRunSpec `json:"spec"`
}

type evalRunRequestMetadata struct {
	Name         string            `json:"name,omitempty"`
	GenerateName string            `json:"generateName,omitempty"`
	Namespace    string            `json:"namespace,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
}

func evalRunRequestFromFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var manifest struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		evalRunRequest
	}
	if err := sigsyaml.UnmarshalStrict(raw, &manifest); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if manifest.Kind != "" && manifest.Kind != "EvalRun" {
		return nil, fmt.Errorf("%s is a %s, not an EvalRun", path, manifest.Kind)
	}
	if manifest.APIVersion != "" && manifest.APIVersion != corev1alpha1.GroupVersion.String() {
		return nil, fmt.Errorf("%s has apiVersion %s, want %s", path, manifest.APIVersion, corev1alpha1.GroupVersion.String())
	}
	if manifest.Metadata.Name == "" && manifest.Metadata.GenerateName == "" {
		return nil, fmt.Errorf("%s needs metadata.name or metadata.generateName", path)
	}
	return json.Marshal(manifest.evalRunRequest)
}

// evalRunMinWaitTTL is the shortest ttlSecondsAfterFinished --wait accepts:
// the run and its trial records must outlive the poll that sees it finish.
const evalRunMinWaitTTL = 300

// checkEvalRunTTLForWait refuses a TTL that could delete the run and its
// trial records before the command reads them. An export reads trial records
// and events after the run finishes, for as long as that takes, so it needs
// the run kept: no TTL at all.
func checkEvalRunTTLForWait(body []byte, export bool) error {
	var request evalRunRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return err
	}
	return checkEvalRunTTL(request.Spec.TTLSecondsAfterFinished, export)
}

// checkEvalRunTTL applies checkEvalRunTTLForWait's rule to a run's TTL.
func checkEvalRunTTL(ttl *int32, export bool) error {
	switch {
	case ttl == nil:
		return nil
	case export:
		return errors.New("--output-dir needs the run kept until the export finishes; remove ttlSecondsAfterFinished " +
			"and delete the run afterwards")
	case *ttl < evalRunMinWaitTTL:
		return fmt.Errorf("ttlSecondsAfterFinished %d could delete the run before --wait reads it; use at least %d or leave it unset",
			*ttl, evalRunMinWaitTTL)
	}
	return nil
}

func createEvalRun(ctx context.Context, c *client.Client, body []byte) (*corev1alpha1.EvalRun, error) {
	var request evalRunRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	// The manifest's namespace wins; otherwise the CLI's namespace applies.
	if request.Metadata.Namespace == "" {
		request.Metadata.Namespace = c.Namespace
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	return decodeEvalRun(c.DoJSON(ctx, http.MethodPost, evalRunsAPIPath, nil, body))
}

// getEvalRun reads a run. An empty namespace means the CLI's namespace.
func getEvalRun(ctx context.Context, c *client.Client, namespace, name string) (*corev1alpha1.EvalRun, error) {
	return decodeEvalRun(c.DoJSON(ctx, http.MethodGet, evalRunsAPIPath+"/"+url.PathEscape(name), namespaceQuery(namespace), nil))
}

// namespaceQuery pins a request to the run's namespace, which the manifest
// may have set to something other than the CLI's.
func namespaceQuery(namespace string) map[string]string {
	if namespace == "" {
		return nil
	}
	return map[string]string{"namespace": namespace}
}

func decodeEvalRun(value any, err error) (*corev1alpha1.EvalRun, error) {
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	run := &corev1alpha1.EvalRun{}
	if err := json.Unmarshal(raw, run); err != nil {
		return nil, fmt.Errorf("decode eval run: %w", err)
	}
	return run, nil
}

func evalRunFinished(phase corev1alpha1.EvalRunPhase) bool {
	switch phase {
	case corev1alpha1.EvalRunPhaseSucceeded, corev1alpha1.EvalRunPhaseInvalid, corev1alpha1.EvalRunPhaseFailed:
		return true
	}
	return false
}

// waitForEvalRun polls the run until it finishes, printing a line whenever
// its phase or counts change.
func waitForEvalRun(ctx context.Context, c *client.Client, run *corev1alpha1.EvalRun, timeout time.Duration, out io.Writer) (*corev1alpha1.EvalRun, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	last := ""
	waitingForDecision := false
	for {
		progress := fmt.Sprintf("%s: %d/%d trials done (%d passed, %d failed, %d infra errors)", run.Status.Phase,
			run.Status.Counts.Completed, run.Status.Counts.Planned, run.Status.Counts.Passed, run.Status.Counts.Failed,
			run.Status.Counts.InfraError)
		if progress != last && run.Status.Phase != "" {
			_, _ = fmt.Fprintln(out, progress)
			last = progress
		}
		if evalRunFinished(run.Status.Phase) {
			// A candidate is judged after it finishes, once its baseline
			// has finished too; wait for that decision.
			if experiment := run.Status.Experiment; experiment == nil || experiment.Result != corev1alpha1.EvalExperimentPending {
				return run, nil
			}
			if !waitingForDecision {
				_, _ = fmt.Fprintln(out, "Waiting for the experiment decision and its baseline")
				waitingForDecision = true
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("evalrun/%s did not finish within %s", run.Name, timeout)
		case <-time.After(evalRunPollInterval):
		}
		next, err := getEvalRun(ctx, c, run.Namespace, run.Name)
		if err == nil && next.UID != run.UID {
			// The name now belongs to another run: its status says nothing
			// about the run this command follows.
			return nil, fmt.Errorf("evalrun/%s was deleted and recreated while waiting", run.Name)
		}
		if err != nil {
			// A restarting API server or a dropped port-forward must not end
			// a long wait; keep polling until the timeout. A 4xx answer, such
			// as a deleted run or lost access, still fails at once.
			if transientEvalRunError(err) && ctx.Err() == nil {
				_, _ = fmt.Fprintf(out, "Retrying after: %v\n", err)
				continue
			}
			return nil, err
		}
		run = next
	}
}

// transientEvalRunError reports a failure worth retrying: no response at
// all, a response cut off mid-body, a 5xx or a 429.
func transientEvalRunError(err error) bool {
	message := err.Error()
	// No response, or a response whose body was cut off.
	if strings.Contains(message, "request failed:") || strings.Contains(message, "failed to read response:") {
		return true
	}
	return strings.Contains(message, "(HTTP 5") || strings.Contains(message, "(HTTP 429)")
}

func printEvalRunResults(out io.Writer, run *corev1alpha1.EvalRun) {
	_, _ = fmt.Fprintf(out, "EvalRun %s/%s: %s\n", run.Namespace, run.Name, run.Status.Phase)
	if condition := evalRunCompletedCondition(run); condition != nil {
		_, _ = fmt.Fprintf(out, "%s: %s\n", condition.Reason, condition.Message)
	}
	counts := run.Status.Counts
	_, _ = fmt.Fprintf(out, "Trials: %d planned, %d completed, %d passed, %d failed, %d infra errors\n",
		counts.Planned, counts.Completed, counts.Passed, counts.Failed, counts.InfraError)
	defer func() {
		for _, line := range evalExperimentLines(run) {
			_, _ = fmt.Fprintln(out, line)
		}
	}()
	if len(run.Status.Results) == 0 {
		return
	}
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "GRADER\tTAG\tPASS@1\tCASES ANY PASSED\tCASES ALL PASSED")
	for _, result := range run.Status.Results {
		tag := result.Tag
		if tag == "" {
			tag = "(all)"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%d/%d\t%d/%d\t%d/%d\n", result.Grader, tag, result.PassedTrials, result.Trials,
			result.CasesAnyPassed, result.Cases, result.CasesAllPassed, result.Cases)
	}
	_ = w.Flush()
	if len(run.Status.SuspectCases) > 0 {
		_, _ = fmt.Fprintf(out, "Suspect cases (no passing trial): %s\n", strings.Join(run.Status.SuspectCases, ", "))
	}
}

// evalExperimentLines describes a candidate run's experiment result.
func evalExperimentLines(run *corev1alpha1.EvalRun) []string {
	experiment := run.Status.Experiment
	if experiment == nil || run.Spec.Experiment == nil {
		return nil
	}
	target := run.Spec.Experiment.Target
	name := string(target.Grader)
	if target.Tag != "" {
		name += " on " + target.Tag
	}
	baseline := run.Spec.Experiment.BaselineRunRef.Name
	lines := []string{fmt.Sprintf("Experiment: %s against baseline %s (target %s)", experiment.Result, baseline, name)}
	if t := experiment.Target; t != nil {
		lines = append(lines, fmt.Sprintf("Target pass@1: %d/%d -> %d/%d (%s points); paired trials %d improved, %d regressed; p = %s",
			t.BaselinePassedTrials, t.BaselineTrials, t.CandidatePassedTrials, t.CandidateTrials, t.ImprovementPoints,
			t.ImprovedPairs, t.RegressedPairs, t.PValue))
	}
	for _, reason := range experiment.Reasons {
		lines = append(lines, "  - "+reason)
	}
	return lines
}

func evalRunCompletedCondition(run *corev1alpha1.EvalRun) *metav1.Condition {
	condition := meta.FindStatusCondition(run.Status.Conditions, "Completed")
	if condition == nil || condition.Reason == "" {
		return nil
	}
	return condition
}

// writeEvalRunOutput writes the finished run, its trial records, a Markdown
// summary and the events of every failed trial into dir. Event transcripts
// can hold prompts and source, so the export is readable only by its owner.
func writeEvalRunOutput(ctx context.Context, c *client.Client, run *corev1alpha1.EvalRun, dir string) error {
	query := namespaceQuery(run.Namespace)
	if query == nil {
		query = map[string]string{}
	}
	query["format"] = "jsonl"
	trialsJSONL, _, err := c.GetRaw(ctx, evalRunsAPIPath+"/"+url.PathEscape(run.Name)+"/trials", query)
	if err != nil {
		return fmt.Errorf("read trial records: %w", err)
	}
	trials, err := parseEvalTrials(trialsJSONL)
	if err != nil {
		return err
	}
	// The listing is by name, so a run recreated since the last poll would
	// serve its own records. Every record must belong to this run, and the
	// name must still be this run after the listing.
	for _, trial := range trials {
		if trial.EvalRunUID != string(run.UID) {
			return fmt.Errorf("evalrun/%s was deleted and recreated before its trial records were read", run.Name)
		}
	}
	current, err := getEvalRun(ctx, c, run.Namespace, run.Name)
	if err != nil {
		return fmt.Errorf("re-read evalrun/%s: %w", run.Name, err)
	}
	if current.UID != run.UID {
		return fmt.Errorf("evalrun/%s was deleted and recreated before its trial records were read", run.Name)
	}
	// An existing events directory from an earlier export keeps its mode
	// otherwise. The output directory itself is the caller's; every file in
	// it is private.
	if err := os.MkdirAll(filepath.Join(dir, "events"), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Join(dir, "events"), 0o700); err != nil {
		return err
	}
	// Events of an earlier export into this directory would sit beside this
	// run's records; only this run's failed trials are written below.
	entries, err := os.ReadDir(filepath.Join(dir, "events"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".json") {
			if err := os.Remove(filepath.Join(dir, "events", entry.Name())); err != nil {
				return err
			}
		}
	}
	encoded, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	if err := writePrivateFile(filepath.Join(dir, "evalrun.json"), append(encoded, '\n')); err != nil {
		return err
	}
	if err := writePrivateFile(filepath.Join(dir, "trials.jsonl"), trialsJSONL); err != nil {
		return err
	}
	for _, trial := range trials {
		if trial.Outcome != store.EvalTrialOutcomeFail || trial.TaskName == "" {
			continue
		}
		// The name becomes a file name, so it must be a Kubernetes object
		// name, which can never climb out of events/.
		if errs := validation.IsDNS1123Subdomain(trial.TaskName); len(errs) > 0 {
			return fmt.Errorf("trial %d of case %q names an invalid Task", trial.Trial, trial.CaseID)
		}
		events, err := allTaskEvents(ctx, c, run.Namespace, trial.TaskName)
		switch {
		case err == nil:
		case strings.Contains(err.Error(), "HTTP 404"):
			// The trial Task is gone already; the record still names it.
			events = fmt.Appendf(nil, "{\"taskName\": %q, \"missing\": \"the Task no longer exists\"}\n", trial.TaskName)
		default:
			// Any other failure leaves the export incomplete, so it fails.
			return fmt.Errorf("read events of failed trial Task %s: %w", trial.TaskName, err)
		}
		if err := writePrivateFile(filepath.Join(dir, "events", trial.TaskName+".json"), events); err != nil {
			return err
		}
	}
	return writePrivateFile(filepath.Join(dir, "summary.md"), []byte(evalRunSummaryMarkdown(run, trials)))
}

// writePrivateFile writes data to a new owner-only file and renames it over
// path. A file left by an earlier export, and any descriptor still open on
// it, keeps only its old content.
func writePrivateFile(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// evalEventPageSize and evalEventMaxPages bound one trial's event download.
const (
	evalEventPageSize = 500
	evalEventMaxPages = 1000
)

// allTaskEvents reads every page of a Task's events, following after= until
// the stream's latest sequence, and returns them as one document.
func allTaskEvents(ctx context.Context, c *client.Client, namespace, task string) ([]byte, error) {
	var collected []json.RawMessage
	var after, latest int64
	for range evalEventMaxPages {
		query := namespaceQuery(namespace)
		if query == nil {
			query = map[string]string{}
		}
		query["after"] = strconv.FormatInt(after, 10)
		query["limit"] = strconv.Itoa(evalEventPageSize)
		raw, _, err := c.GetRaw(ctx, "/api/v1/tasks/"+url.PathEscape(task)+"/events", query)
		if err != nil {
			return nil, err
		}
		var page struct {
			LatestSeq int64             `json:"latestSeq"`
			Events    []json.RawMessage `json:"events"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("decode events of Task %s: %w", task, err)
		}
		latest = page.LatestSeq
		last := after
		for _, event := range page.Events {
			var seq struct {
				Seq int64 `json:"seq"`
			}
			if err := json.Unmarshal(event, &seq); err != nil {
				return nil, fmt.Errorf("decode events of Task %s: %w", task, err)
			}
			last = max(last, seq.Seq)
		}
		collected = append(collected, page.Events...)
		// Stop at the end of the stream, or when a page makes no progress.
		if len(page.Events) == 0 || last >= latest || last == after {
			return json.MarshalIndent(map[string]any{
				"namespace": namespace, "taskName": task, "latestSeq": latest, "events": collected,
			}, "", "  ")
		}
		after = last
	}
	return nil, fmt.Errorf("events of Task %s exceed %d pages", task, evalEventMaxPages)
}

func parseEvalTrials(raw []byte) ([]store.EvalTrialRecord, error) {
	var trials []store.EvalTrialRecord
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	for {
		var trial store.EvalTrialRecord
		if err := decoder.Decode(&trial); errors.Is(err, io.EOF) {
			return trials, nil
		} else if err != nil {
			return nil, fmt.Errorf("decode trial records: %w", err)
		}
		trials = append(trials, trial)
	}
}

func evalRunSummaryMarkdown(run *corev1alpha1.EvalRun, trials []store.EvalTrialRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# EvalRun %s/%s\n\n", run.Namespace, run.Name)
	fmt.Fprintf(&b, "**Phase:** %s", run.Status.Phase)
	if condition := evalRunCompletedCondition(run); condition != nil {
		fmt.Fprintf(&b, " (%s: %s)", condition.Reason, condition.Message)
	}
	counts := run.Status.Counts
	fmt.Fprintf(&b, "\n\n**Trials:** %d planned, %d completed, %d passed, %d failed, %d infra errors\n\n",
		counts.Planned, counts.Completed, counts.Passed, counts.Failed, counts.InfraError)
	// A run that failed before it froze its snapshot has none.
	if snapshot := run.Status.Snapshot; snapshot != nil {
		fmt.Fprintf(&b, "**Model:** %s  \n**Runtime image:** %s  \n**Case set:** %s  \n**Grader set:** %s\n\n",
			snapshot.Model, snapshot.RuntimeImageDigest, snapshot.CaseSetDigest, snapshot.GraderSetDigest)
	}
	if len(run.Status.Results) > 0 {
		b.WriteString("| Grader | Tag | pass@1 | Cases any passed | Cases all passed |\n|---|---|---|---|---|\n")
		for _, result := range run.Status.Results {
			tag := result.Tag
			if tag == "" {
				tag = "(all)"
			}
			fmt.Fprintf(&b, "| %s | %s | %d/%d | %d/%d | %d/%d |\n", result.Grader, tag, result.PassedTrials, result.Trials,
				result.CasesAnyPassed, result.Cases, result.CasesAllPassed, result.Cases)
		}
		b.WriteString("\n")
	}
	if lines := evalExperimentLines(run); len(lines) > 0 {
		b.WriteString("## Experiment\n\n")
		for _, line := range lines {
			b.WriteString(line + "\n")
		}
		b.WriteString("\n")
	}
	var failed []store.EvalTrialRecord
	for _, trial := range trials {
		if trial.Outcome == store.EvalTrialOutcomeFail {
			failed = append(failed, trial)
		}
	}
	if len(failed) > 0 {
		b.WriteString("## Failed trials\n\nRead the transcripts in `events/` before trusting a score.\n\n")
		b.WriteString("| Case | Trial | Verdict | Failed graders | Task |\n|---|---|---|---|---|\n")
		for _, trial := range failed {
			var graders []string
			for _, grade := range trial.Grades {
				if grade.Status == "fail" {
					graders = append(graders, grade.Grader)
				}
			}
			fmt.Fprintf(&b, "| %s | %d | %s | %s | %s |\n", trial.CaseID, trial.Trial, trial.Verdict,
				strings.Join(graders, ", "), trial.TaskName)
		}
	}
	return b.String()
}
