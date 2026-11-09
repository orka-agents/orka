package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agentcontext"
	"github.com/orka-agents/orka/internal/evals"
	"github.com/orka-agents/orka/internal/labels"
)

const (
	// evalReviewPullRequestNumber is the pull request number every
	// repositoryMonitorReview trial reviews. Cases come from a fixture
	// repository branch diff, not a forge pull request.
	evalReviewPullRequestNumber = 1
	// evalTrialTaskPriority runs trials behind interactive and monitor work.
	evalTrialTaskPriority int32 = 100
	evalTrialCreatedBy          = "evalrun-controller"
)

// evalReviewFixture splits an anonymous https://github.com/<owner>/<repo>
// fixture URL. The case parser already validated its shape.
func evalReviewFixture(repoURL string) (owner, repository string, err error) {
	u, err := url.Parse(repoURL)
	if err != nil {
		return "", "", err
	}
	if u.Scheme != urlSchemeHTTPS || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("fixture repository %q must be an anonymous https://github.com URL", repoURL)
	}
	segments := strings.Split(strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git"), "/")
	if len(segments) != 2 || segments[0] == "" || segments[1] == "" {
		return "", "", fmt.Errorf("fixture repository %q must be https://github.com/<owner>/<repo>", repoURL)
	}
	return segments[0], segments[1], nil
}

// evalReviewMonitorForFixture returns a copy of the monitor whose repository
// is the fixture, so the production prompt names what the trial clones.
// Every review setting the prompt embeds comes from the monitor unchanged.
func evalReviewMonitorForFixture(monitor *corev1alpha1.RepositoryMonitor, repoURL, owner, repository string) *corev1alpha1.RepositoryMonitor {
	fixture := monitor.DeepCopy()
	fixture.Spec.RepoURL = repoURL
	fixture.Spec.Owner = owner
	fixture.Spec.Repository = repository
	return fixture
}

func evalReviewPullRequest(input *evals.ReviewCaseInput, owner, repository string) repositoryMonitorPullRequest {
	return repositoryMonitorPullRequest{
		Number:      evalReviewPullRequestNumber,
		Title:       input.Title,
		State:       "open",
		BaseSHA:     input.BaseSHA,
		HeadSHA:     input.HeadSHA,
		HeadRepo:    owner + "/" + repository,
		HeadRepoURL: input.RepoURL,
	}
}

func evalReviewFiles(input *evals.ReviewCaseInput) []repositoryMonitorPullRequestFileResponse {
	files := make([]repositoryMonitorPullRequestFileResponse, 0, len(input.Files))
	for _, file := range input.Files {
		files = append(files, repositoryMonitorPullRequestFileResponse{
			Filename:         file.Filename,
			PreviousFilename: file.PreviousFilename,
			Status:           file.Status,
			Additions:        file.Additions,
			Deletions:        file.Deletions,
			Patch:            file.Patch,
		})
	}
	return files
}

// buildEvalReviewTask renders the production review Task for a case,
// before the run's name, labels and timeout are set.
func buildEvalReviewTask(monitor *corev1alpha1.RepositoryMonitor, c evals.Case) (*corev1alpha1.Task, error) {
	input := c.Input.RepositoryMonitorReview
	if input == nil {
		return nil, fmt.Errorf("case %q has no repositoryMonitorReview input", c.ID)
	}
	owner, repository, err := evalReviewFixture(input.RepoURL)
	if err != nil {
		return nil, err
	}
	fixtureMonitor := evalReviewMonitorForFixture(monitor, input.RepoURL, owner, repository)
	pr := evalReviewPullRequest(input, owner, repository)
	files := evalReviewFiles(input)
	reviewContext := repositoryMonitorReviewContextBindChangedFileCount(
		repositoryMonitorReviewContextFromFiles(owner, repository, pr, files), len(files), len(files))
	return buildRepositoryMonitorReviewTask(repositoryMonitorReviewTaskInput{
		Monitor:       fixtureMonitor,
		Owner:         owner,
		Repository:    repository,
		PullRequest:   pr,
		Context:       reviewContext,
		Plan:          repositoryMonitorReviewPlan{Mode: repositoryMonitorReviewModeFull},
		Priority:      evalTrialTaskPriority,
		WorkspaceRepo: input.RepoURL,
	})
}

// buildEvalReviewTrialTask renders one trial through the production review
// Task builder. The trial clones the fixture anonymously and read-only: it
// gets no read, publication or forge credential.
func buildEvalReviewTrialTask(run *corev1alpha1.EvalRun, monitor *corev1alpha1.RepositoryMonitor, c evals.Case, trial int32, timeout metav1.Duration) (*corev1alpha1.Task, error) {
	task, err := buildEvalReviewTask(monitor, c)
	if err != nil {
		return nil, err
	}
	task.Name = evalTrialTaskName(run, c.ID, trial)
	task.Namespace = run.Namespace
	task.Labels = map[string]string{
		labels.LabelManaged:   booleanTrueValue,
		labels.LabelCreatedBy: evalTrialCreatedBy,
		labels.LabelEvalRun:   labels.SelectorValue(run.Name),
		labels.LabelEvalCase:  labels.SelectorValue(c.ID),
		labels.LabelEvalTrial: strconv.Itoa(int(trial)),
	}
	task.Spec.Timeout = &timeout
	if err := validateEvalTrialTaskCredentialFree(task); err != nil {
		return nil, err
	}
	return task, nil
}

// validateEvalTrialTaskCredentialFree fails closed when a trial Task would
// carry any credential or publication capability.
func validateEvalTrialTaskCredentialFree(task *corev1alpha1.Task) error {
	spec := task.Spec
	switch {
	case spec.SecretRef != nil, len(spec.Env) > 0, spec.Transaction != nil, spec.SessionRef != nil, spec.PriorTaskRef != nil:
		return fmt.Errorf("eval trial Task must not carry secrets, environment, transactions or prior sessions")
	case spec.Workspace == nil:
		return fmt.Errorf("eval trial Task requires a read-only workspace")
	}
	workspace := spec.Workspace
	switch {
	case workspace.Intent != corev1alpha1.WorkspaceIntentRead:
		return fmt.Errorf("eval trial workspace must be read-only")
	case workspace.ReadCredentialRef != nil, workspace.PublicationReadCredentialRef != nil,
		workspace.PublicationCredentialRef != nil, workspace.ForgeCredentialRef != nil:
		return fmt.Errorf("eval trial workspace must not reference a credential")
	case strings.TrimSpace(workspace.PublicationGitRepo) != "", strings.TrimSpace(workspace.PushBranch) != "", workspace.CreatePR:
		return fmt.Errorf("eval trial workspace must not publish")
	}
	if _, _, err := evalReviewFixture(workspace.GitRepo); err != nil {
		return err
	}
	return nil
}

// evalTrialTaskName is unique per EvalRun incarnation, case and trial. The
// readable prefix is lossy, so a digest of the exact identity follows it.
func evalTrialTaskName(run *corev1alpha1.EvalRun, caseID string, trial int32) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d", run.Name, run.UID, caseID, trial)))
	prefix := repositoryMonitorBoundedDNSName("eval-"+run.Name+"-"+caseID, 40)
	return fmt.Sprintf("%s-%d-%s", prefix, trial, hex.EncodeToString(sum[:])[:12])
}

// evalReviewPromptTemplateDigest digests the production prompt each case
// renders, in case order. It covers every rendering branch the run's trials
// use, so a controller change to any of them changes the digest, as does a
// monitor setting rendered into the prompt.
func evalReviewPromptTemplateDigest(monitor *corev1alpha1.RepositoryMonitor, cases []evals.Case) (string, error) {
	hash := sha256.New()
	for _, c := range cases {
		task, err := buildEvalReviewTask(monitor, c)
		if err != nil {
			return "", evalRunConfigErrorf(evalRunReasonSubjectInvalid, "render the prompt of case %q: %v", c.ID, err)
		}
		_, _ = fmt.Fprintf(hash, "%d:%s%d:%s", len(c.ID), c.ID, len(task.Spec.Prompt), task.Spec.Prompt)
	}
	return evals.DigestPrefix + hex.EncodeToString(hash.Sum(nil)), nil
}

// evalReviewOutcomeFromResult runs a completed trial's raw output through the
// production parse-and-gate step and the verdict derivation a full review
// gets, and returns what production would have recorded.
func evalReviewOutcomeFromResult(raw []byte, monitor *corev1alpha1.RepositoryMonitor, c evals.Case, task *corev1alpha1.Task) *evals.ReviewTrialOutcome {
	input := c.Input.RepositoryMonitorReview
	owner, repository, _ := evalReviewFixture(input.RepoURL)
	parsed := repositoryMonitorReviewOutcomeFromResult(raw, repositoryMonitorReviewExpectation{
		Repository: owner + "/" + repository,
		PRNumber:   evalReviewPullRequestNumber,
		HeadSHA:    input.HeadSHA,
	}, task.Spec.Prompt)
	if parsed.Rejection != "" {
		return &evals.ReviewTrialOutcome{
			Verdict:    repositoryMonitorReviewVerdictFailed,
			SkipReason: repositoryMonitorReviewSkipReasonMalformed,
		}
	}
	review := parsed.Review
	plan := repositoryMonitorReviewPlan{Mode: repositoryMonitorReviewModeFull}
	reviewerVerdict := strings.TrimSpace(review.Verdict)
	ledger := repositoryMonitorMergeFindingLedger(plan, review, strings.TrimSpace(review.HeadSHA), task.Name, nil)
	verdict := repositoryMonitorDerivedReviewVerdict(monitor.Spec, reviewerVerdict, ledger)
	gateReason := repositoryMonitorReviewVerdictGateReason(task.Spec.Prompt, verdict)
	if gateReason != "" {
		verdict = repositoryMonitorReviewVerdictNeedsHuman
	}
	result := &evals.ReviewResult{
		SchemaVersion: strings.TrimSpace(review.SchemaVersion),
		HeadSHA:       strings.TrimSpace(review.HeadSHA),
		Verdict:       reviewerVerdict,
		TestsStatus:   strings.TrimSpace(review.Tests.Status),
	}
	for _, finding := range review.Findings {
		// The path stays as the review wrote it: localization matches paths
		// exactly, as production's changed-lines check does.
		result.Findings = append(result.Findings, evals.ReviewFinding{File: finding.File, Line: finding.Line})
	}
	outcome := &evals.ReviewTrialOutcome{Result: result, Verdict: verdict, GateReason: gateReason}
	if verdict == repositoryMonitorVerdictSkipped {
		// Production ingestion records a skipped review with the skip reason
		// skipped, so the review counts as not done.
		outcome.SkipReason = repositoryMonitorVerdictSkipped
	}
	return outcome
}

// evalReviewSubject is the resolved repositoryMonitorReview subject.
type evalReviewSubject struct {
	Monitor            *corev1alpha1.RepositoryMonitor
	Reviewer           *corev1alpha1.Agent
	RuntimeImageDigest string
	// SystemPromptDigest is the digest of the system prompt a trial Task
	// resolves for the reviewer.
	SystemPromptDigest string
}

// resolveEvalReviewSubject reads the monitor and its reviewer Agent and
// checks that trials can run without any credential or forge effect.
func (r *EvalRunReconciler) resolveEvalReviewSubject(ctx context.Context, run *corev1alpha1.EvalRun) (*evalReviewSubject, error) {
	ref := run.Spec.Subject.RepositoryMonitorReview.MonitorRef
	monitor := &corev1alpha1.RepositoryMonitor{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: ref.Name}, monitor); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, evalRunConfigErrorf(evalRunReasonSubjectInvalid, "RepositoryMonitor %q was not found", ref.Name)
		}
		return nil, err
	}
	reviewerRef := monitor.Spec.Agents.Reviewer
	if reviewerRef == nil || strings.TrimSpace(reviewerRef.Name) == "" {
		return nil, evalRunConfigErrorf(evalRunReasonSubjectInvalid, "RepositoryMonitor %q has no reviewer agent", monitor.Name)
	}
	if ns := strings.TrimSpace(reviewerRef.Namespace); ns != "" && ns != run.Namespace {
		return nil, evalRunConfigErrorf(evalRunReasonSubjectInvalid, "reviewer agent %s/%s is not in the EvalRun namespace", ns, reviewerRef.Name)
	}
	if image := strings.TrimSpace(monitor.Spec.Validation.Image); image != "" {
		// A validation run is bound to a monitor run in the monitor's event
		// log; trials have neither, so they would review under different
		// tools than production.
		return nil, evalRunConfigErrorf(evalRunReasonSubjectUnsupported,
			"RepositoryMonitor %q runs a validation image; evals do not support validation yet", monitor.Name)
	}
	reviewer := &corev1alpha1.Agent{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: reviewerRef.Name}, reviewer); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, evalRunConfigErrorf(evalRunReasonSubjectInvalid, "reviewer agent %q was not found", reviewerRef.Name)
		}
		return nil, err
	}
	runtimeSpec := reviewer.Spec.Runtime
	if runtimeSpec == nil || runtimeSpec.RuntimeRef != nil || runtimeSpec.Type == "" {
		return nil, evalRunConfigErrorf(evalRunReasonSubjectUnsupported,
			"reviewer agent %q must use a built-in runtime; external runtimes are not supported by evals", reviewer.Name)
	}
	if runtimeSpec.Type == corev1alpha1.AgentRuntimeCopilot {
		// Review Tasks are read-only, and Task admission rejects Copilot
		// there because its runtime credential can mutate GitHub.
		return nil, evalRunConfigErrorf(evalRunReasonSubjectUnsupported,
			"reviewer agent %q cannot use copilot for read-only reviews; use claude, codex, or opencode", reviewer.Name)
	}
	_, image, err := acpRuntimeArtifacts(runtimeSpec.Type, r.RuntimeImages)
	if err != nil {
		return nil, evalRunConfigErrorf(evalRunReasonSubjectUnsupported, "reviewer agent %q: %v", reviewer.Name, err)
	}
	if !ACPRuntimeImageAvailable(image) {
		return nil, evalRunConfigErrorf(evalRunReasonRuntimeImageUnavailable,
			"the %s runtime image is not a digest-pinned image on this controller", runtimeSpec.Type)
	}
	systemPromptDigest, err := r.evalReviewerSystemPromptDigest(ctx, reviewer)
	if err != nil {
		return nil, err
	}
	return &evalReviewSubject{
		Monitor:            monitor,
		Reviewer:           reviewer,
		RuntimeImageDigest: image[strings.LastIndex(image, "@")+1:],
		SystemPromptDigest: systemPromptDigest,
	}, nil
}

// evalReviewerSystemPromptDigest resolves the reviewer's system prompt the
// way each trial Task does, ConfigMap source and soul included, and digests
// it. A ConfigMap edit changes neither the Agent generation nor the prompt
// template, so without this digest trials could run with different
// instructions under one snapshot. The ConfigMap is read uncached so a stale
// cache cannot hide an edit the trial Task already resolved.
func (r *EvalRunReconciler) evalReviewerSystemPromptDigest(ctx context.Context, reviewer *corev1alpha1.Agent) (string, error) {
	reader := r.uncachedReader()
	systemPrompt, err := resolveACPSystemPrompt(ctx, reader, reviewer)
	if err != nil {
		if isPermanentACPAgentConfigurationError(err) {
			return "", evalRunConfigErrorf(evalRunReasonSubjectInvalid, "reviewer agent %q: %v", reviewer.Name, err)
		}
		return "", err
	}
	soul, err := agentcontext.ResolveSoul(ctx, reader, reviewer)
	if err != nil {
		if agentcontext.IsInvalidSource(err) {
			return "", evalRunConfigErrorf(evalRunReasonSubjectInvalid, "reviewer agent %q: %v", reviewer.Name, err)
		}
		return "", err
	}
	sum := sha256.Sum256([]byte(agentcontext.Compose(systemPrompt, soul)))
	return evals.DigestPrefix + hex.EncodeToString(sum[:]), nil
}

// snapshot returns the subject's part of the run snapshot for the run's
// cases.
func (s *evalReviewSubject) snapshot(cases []evals.Case) (corev1alpha1.EvalRunSnapshot, error) {
	promptDigest, err := evalReviewPromptTemplateDigest(s.Monitor, cases)
	if err != nil {
		return corev1alpha1.EvalRunSnapshot{}, err
	}
	return corev1alpha1.EvalRunSnapshot{
		MonitorUID:        s.Monitor.UID,
		MonitorGeneration: s.Monitor.Generation,
		ReviewerAgent: &corev1alpha1.EvalObjectIdentity{
			Name:       s.Reviewer.Name,
			UID:        s.Reviewer.UID,
			Generation: s.Reviewer.Generation,
		},
		RuntimeImageDigest:         s.RuntimeImageDigest,
		Model:                      evalReviewerModel(s.Reviewer),
		PromptTemplateDigest:       promptDigest,
		ReviewerSystemPromptDigest: s.SystemPromptDigest,
	}, nil
}

// evalSnapshotMaxModelLength matches the snapshot model field limit.
const evalSnapshotMaxModelLength = 256

// evalReviewerModel names the reviewer's model route as provider/model.
func evalReviewerModel(agent *corev1alpha1.Agent) string {
	if agent.Spec.Model == nil {
		return ""
	}
	model := strings.TrimSpace(agent.Spec.Model.Name)
	if provider := strings.TrimSpace(agent.Spec.Model.Provider); provider != "" && model != "" {
		model = provider + "/" + model
	}
	if len(model) > evalSnapshotMaxModelLength {
		sum := sha256.Sum256([]byte(model))
		return evals.DigestPrefix + hex.EncodeToString(sum[:])
	}
	return model
}

// evalRunSnapshotChanges names the snapshot fields that differ.
func evalRunSnapshotChanges(frozen, current corev1alpha1.EvalRunSnapshot) []string {
	var changed []string
	check := func(name string, equal bool) {
		if !equal {
			changed = append(changed, name)
		}
	}
	check("monitorUID", frozen.MonitorUID == current.MonitorUID)
	check("monitorGeneration", frozen.MonitorGeneration == current.MonitorGeneration)
	check("reviewerAgent", reflect.DeepEqual(frozen.ReviewerAgent, current.ReviewerAgent))
	check("runtimeImageDigest", frozen.RuntimeImageDigest == current.RuntimeImageDigest)
	check("model", frozen.Model == current.Model)
	check("promptTemplateDigest", frozen.PromptTemplateDigest == current.PromptTemplateDigest)
	check("reviewerSystemPromptDigest", frozen.ReviewerSystemPromptDigest == current.ReviewerSystemPromptDigest)
	check("caseSetDigest", frozen.CaseSetDigest == current.CaseSetDigest)
	check("graderSetDigest", frozen.GraderSetDigest == current.GraderSetDigest)
	check("trialPlanDigest", frozen.TrialPlanDigest == current.TrialPlanDigest)
	if len(changed) == 0 && !reflect.DeepEqual(frozen, current) {
		changed = append(changed, "snapshot")
	}
	return changed
}
