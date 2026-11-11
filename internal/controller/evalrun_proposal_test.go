package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/evals"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

// proposalTestGate lists the experiment cases as training cases and a
// separate held-out set whose baseline monitor is the test monitor.
const proposalTestGate = `schemaVersion: orka.protectedSet.v1
paths: [".github/**"]
caseSets:
  - {namespace: team-a, name: cases, split: train}
  - {namespace: team-a, name: heldout, split: heldOut, baselineMonitors: [monitor]}
gate:
  maxInfraErrorPercent: 10
  maxCandidatesPerBaseline: 2
  maxExperimentsPerHeldOutSet: 2
  minTargetImprovementPoints: 5
  significance: 0.05
`

const proposalTestHeldOutTitle = "HELD-OUT-ONLY title"

const proposalTestAnswer = "Here is the change.\n```json\n" +
	`{"schemaVersion":"orka.improvement.candidate.v1","hypothesis":"The reviewer approves defects it should hold back.",` +
	`"agentPatch":{"systemPrompt":"Hold back any change with a defect, and cite its line."}}` + "\n```\n"

// newProposalEnv runs the training run to the end with every defect case
// passed by the reviewer, so all its trials fail.
func newProposalEnv(t *testing.T, verdict string) *experimentEnv {
	t.Helper()
	training, candidate := experimentRuns(3, 3)
	env := newExperimentEnv(t, training, candidate, proposalTestGate)
	ctx := context.Background()
	heldOut := strings.ReplaceAll(strings.ReplaceAll(experimentCases(), evalTestHeadSHA, strings.Repeat("4", 40)),
		"Change defect", proposalTestHeldOutTitle)
	for _, object := range []client.Object{
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "heldout", Namespace: evalTestNamespace},
			Data: map[string]string{"cases.jsonl": heldOut}},
		&corev1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "proposer", Namespace: evalTestNamespace, UID: "proposer-uid", Generation: 1},
			Spec: corev1alpha1.AgentSpec{
				Model:   &corev1alpha1.ModelConfig{Provider: "openai", Name: "gpt-4.1"},
				Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeOpencode, DefaultAllowedTools: []string{"read", "glob"}},
			},
		},
	} {
		if err := env.client.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	env.reconciler.ProposerAgent = "proposer"
	env.runToEnd("baseline", verdict)
	return env
}

func TestEvalProposalCreatesACandidateExperiment(t *testing.T) {
	env := newProposalEnv(t, repositoryMonitorReviewVerdictPassed)
	ctx := context.Background()
	task, err := env.reconciler.StartEvalProposal(ctx, env.getRun("baseline"), evals.ProposalRequest{RequestedBy: "alice"})
	if err != nil {
		t.Fatalf("StartEvalProposal() error = %v", err)
	}

	checkProposerTask(t, task)

	env.finishTask(task, corev1alpha1.TaskPhaseSucceeded, []byte(proposalTestAnswer), false)
	env.reconcileRun("baseline")

	proposals, err := env.reconciler.ListEvalProposals(ctx, env.getRun("baseline"))
	if err != nil || len(proposals) != 1 {
		t.Fatalf("proposals = %+v, %v", proposals, err)
	}
	proposal := proposals[0]
	if proposal.State != evalProposalStateCreated || proposal.CandidateAgent == "" || proposal.BaselineRun == "" ||
		proposal.CandidateRun == "" || proposal.HeldOut != "heldout" || proposal.RequestedBy != "alice" ||
		!strings.Contains(proposal.Hypothesis, "approves defects") {
		t.Fatalf("proposal = %+v", proposal)
	}

	agent := checkProposalCandidate(t, env, proposal)

	// A settled proposal is not processed again.
	env.reconcileRun("baseline")
	if again, _ := env.reconciler.ListEvalProposals(ctx, env.getRun("baseline")); again[0].CandidateAgent != agent.Name {
		t.Fatalf("proposal after another reconcile = %+v", again[0])
	}
}

// checkProposerTask asserts the proposer is read-only and workspace-less,
// learns only from training data, and passes the dispatch-time rules.
func checkProposerTask(t *testing.T, task *corev1alpha1.Task) {
	t.Helper()
	if task.Annotations[labels.AnnotationImprovementRole] != labels.ImprovementRoleProposer || task.Spec.Workspace != nil ||
		task.Spec.AgentRuntime == nil || strings.Join(task.Spec.AgentRuntime.AllowedTools, ",") != "read,glob" ||
		task.Spec.AgentRef == nil || task.Spec.AgentRef.Name != "proposer" {
		t.Fatalf("proposer Task = %+v", task)
	}
	for _, want := range []string{"Change defect-1", "Grader verdict failed", "Expected verdict: notPassed", "BEGIN DATA",
		evals.CandidateSchemaVersion, "only prompt changes are allowed"} {
		if !strings.Contains(task.Spec.Prompt, want) {
			t.Fatalf("proposer prompt lacks %q:\n%s", want, task.Spec.Prompt)
		}
	}
	if strings.Contains(task.Spec.Prompt, proposalTestHeldOutTitle) {
		t.Fatal("the proposer prompt shows a held-out case")
	}
	// The dispatcher admits it under the protected set's proposer rules.
	if violation := proposerConstraintViolation(task, task.Spec.AgentRuntime.AllowedTools, false, sampleProtectedSet(t)); violation != "" {
		t.Fatalf("the proposer Task breaks a proposer constraint: %s", violation)
	}

}

// checkProposalCandidate asserts the candidate Agent and the paired held-out
// runs a created proposal points at.
func checkProposalCandidate(t *testing.T, env *experimentEnv, proposal evals.Proposal) *corev1alpha1.Agent {
	t.Helper()
	ctx := context.Background()
	agent := &corev1alpha1.Agent{}
	if err := env.client.Get(ctx, types.NamespacedName{Namespace: evalTestNamespace, Name: proposal.CandidateAgent}, agent); err != nil {
		t.Fatal(err)
	}
	if agent.Labels[labels.LabelCandidateOf] != "reviewer" || agent.Spec.SystemPrompt == nil ||
		agent.Spec.SystemPrompt.Inline != "Hold back any change with a defect, and cite its line." ||
		len(agent.OwnerReferences) != 1 || agent.OwnerReferences[0].UID != "agent-uid" ||
		agent.Spec.Model == nil || agent.Spec.Model.Name != "claude-haiku" {
		t.Fatalf("candidate Agent = %+v", agent)
	}

	baseline := env.getRun(proposal.BaselineRun)
	candidate := env.getRun(proposal.CandidateRun)
	if baseline.Spec.Cases.ConfigMapRef.Name != "heldout" || baseline.Spec.Subject.RepositoryMonitorReview.AgentRef != nil ||
		baseline.Spec.Experiment != nil || baseline.Spec.Trials != 3 {
		t.Fatalf("baseline = %+v", baseline.Spec)
	}
	experiment := candidate.Spec.Experiment
	if candidate.Spec.Cases.ConfigMapRef.Name != "heldout" || candidate.Spec.Subject.RepositoryMonitorReview.AgentRef == nil ||
		candidate.Spec.Subject.RepositoryMonitorReview.AgentRef.Name != agent.Name || experiment == nil ||
		experiment.BaselineRunRef.Name != baseline.Name || experiment.Target.Grader != corev1alpha1.EvalGraderVerdict ||
		experiment.Hypothesis != proposal.Hypothesis {
		t.Fatalf("candidate = %+v", candidate.Spec)
	}

	return agent
}

// A finished proposer whose answer is not a valid candidate is refused with
// a reason, and creates nothing.
func TestEvalProposalRefusesBadAnswers(t *testing.T) {
	for name, test := range map[string]struct {
		phase  corev1alpha1.TaskPhase
		answer string
		want   string
	}{
		"proposer failed":   {phase: corev1alpha1.TaskPhaseFailed, want: "ended Failed"},
		"no result":         {phase: corev1alpha1.TaskPhaseSucceeded, want: "no result"},
		"not a candidate":   {phase: corev1alpha1.TaskPhaseSucceeded, answer: "I would tighten the prompt.", want: "no JSON object"},
		"touches tools":     {phase: corev1alpha1.TaskPhaseSucceeded, answer: strings.Replace(proposalTestAnswer, `"agentPatch":{`, `"agentPatch":{"tools":["Bash"],`, 1), want: "unknown field"},
		"changes the model": {phase: corev1alpha1.TaskPhaseSucceeded, answer: strings.Replace(proposalTestAnswer, `"systemPrompt":`, `"model":"openai/o3","systemPrompt":`, 1), want: "prompt changes only"},
	} {
		t.Run(name, func(t *testing.T) {
			env := newProposalEnv(t, repositoryMonitorReviewVerdictPassed)
			ctx := context.Background()
			task, err := env.reconciler.StartEvalProposal(ctx, env.getRun("baseline"), evals.ProposalRequest{})
			if err != nil {
				t.Fatal(err)
			}
			var result []byte
			if test.answer != "" {
				result = []byte(test.answer)
			}
			env.finishTask(task, test.phase, result, false)
			env.reconcileRun("baseline")
			proposals, err := env.reconciler.ListEvalProposals(ctx, env.getRun("baseline"))
			if err != nil || len(proposals) != 1 || proposals[0].State != evalProposalStateRefused ||
				!strings.Contains(proposals[0].Reason, test.want) || proposals[0].CandidateAgent != "" {
				t.Fatalf("proposals = %+v, %v; want refused with %q", proposals, err, test.want)
			}
			var candidates corev1alpha1.AgentList
			if err := env.client.List(ctx, &candidates, client.MatchingLabels{labels.LabelCandidateOf: "reviewer"}); err != nil || len(candidates.Items) != 0 {
				t.Fatalf("candidate Agents = %d, %v", len(candidates.Items), err)
			}
		})
	}
}

// Proposals start only from a finished plain run on training cases, with a
// proposer configured and budget left.
func TestEvalProposalRefusals(t *testing.T) {
	ctx := context.Background()
	refused := func(t *testing.T, err error, want string) {
		t.Helper()
		refusal, ok := errors.AsType[*evals.ProposalRefusal](err)
		if !ok || !strings.Contains(refusal.Reason, want) {
			t.Fatalf("StartEvalProposal() error = %v, want a refusal containing %q", err, want)
		}
	}
	t.Run("proposals off", func(t *testing.T) {
		env := newProposalEnv(t, repositoryMonitorReviewVerdictPassed)
		env.reconciler.ProposerAgent = ""
		_, err := env.reconciler.StartEvalProposal(ctx, env.getRun("baseline"), evals.ProposalRequest{})
		refused(t, err, "--eval-proposer-agent")
	})
	t.Run("proposer Agent with a write tool", func(t *testing.T) {
		env := newProposalEnv(t, repositoryMonitorReviewVerdictPassed)
		proposer := &corev1alpha1.Agent{}
		if err := env.client.Get(ctx, types.NamespacedName{Namespace: evalTestNamespace, Name: "proposer"}, proposer); err != nil {
			t.Fatal(err)
		}
		proposer.Spec.Runtime.DefaultAllowedTools = []string{"read", "web_fetch"}
		if err := env.client.Update(ctx, proposer); err != nil {
			t.Fatal(err)
		}
		_, err := env.reconciler.StartEvalProposal(ctx, env.getRun("baseline"), evals.ProposalRequest{})
		refused(t, err, "a proposer may not use")
	})
	t.Run("candidate run", func(t *testing.T) {
		env := newProposalEnv(t, repositoryMonitorReviewVerdictPassed)
		_, err := env.reconciler.StartEvalProposal(ctx, env.getRun("candidate"), evals.ProposalRequest{})
		refused(t, err, "is a candidate")
	})
	t.Run("no failed trials", func(t *testing.T) {
		env := newProposalEnv(t, "needs_changes")
		_, err := env.reconciler.StartEvalProposal(ctx, env.getRun("baseline"), evals.ProposalRequest{})
		refused(t, err, "no failed trials")
	})
	t.Run("held-out cases are not training cases", func(t *testing.T) {
		env := newProposalEnv(t, repositoryMonitorReviewVerdictPassed)
		run := env.getRun("baseline")
		run.Spec.Cases.ConfigMapRef.Name = "heldout"
		_, err := env.reconciler.StartEvalProposal(ctx, run, evals.ProposalRequest{})
		refused(t, err, "split: train")
	})
	t.Run("unknown held-out set", func(t *testing.T) {
		env := newProposalEnv(t, repositoryMonitorReviewVerdictPassed)
		_, err := env.reconciler.StartEvalProposal(ctx, env.getRun("baseline"), evals.ProposalRequest{HeldOut: "cases"})
		refused(t, err, "not a held-out case set")
	})
	t.Run("target the run does not grade", func(t *testing.T) {
		env := newProposalEnv(t, repositoryMonitorReviewVerdictPassed)
		_, err := env.reconciler.StartEvalProposal(ctx, env.getRun("baseline"),
			evals.ProposalRequest{Target: &corev1alpha1.EvalExperimentTarget{Grader: "localization"}})
		refused(t, err, "does not grade")
	})
	t.Run("budget spent", func(t *testing.T) {
		env := newProposalEnv(t, repositoryMonitorReviewVerdictPassed)
		for range 2 {
			if _, err := env.reconciler.StartEvalProposal(ctx, env.getRun("baseline"), evals.ProposalRequest{}); err != nil {
				t.Fatal(err)
			}
			env.now = env.now.Add(1)
		}
		_, err := env.reconciler.StartEvalProposal(ctx, env.getRun("baseline"), evals.ProposalRequest{})
		refused(t, err, "maxCandidatesPerBaseline")
	})
	t.Run("a racing request took the slot", func(t *testing.T) {
		env := newProposalEnv(t, repositoryMonitorReviewVerdictPassed)
		run := env.getRun("baseline")
		// Another replica's create landed between this request's list and
		// its create.
		racer := &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Name: evalProposalTaskName(run, 0), Namespace: evalTestNamespace},
			Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Prompt: "x"},
		}
		if err := env.client.Create(ctx, racer); err != nil {
			t.Fatal(err)
		}
		_, err := env.reconciler.StartEvalProposal(ctx, run, evals.ProposalRequest{})
		refused(t, err, "took the same slot")
	})
	t.Run("training cases edited since the run", func(t *testing.T) {
		env := newProposalEnv(t, repositoryMonitorReviewVerdictPassed)
		cases := &corev1.ConfigMap{}
		if err := env.client.Get(ctx, types.NamespacedName{Namespace: evalTestNamespace, Name: "cases"}, cases); err != nil {
			t.Fatal(err)
		}
		cases.Data["cases.jsonl"] = strings.ReplaceAll(cases.Data["cases.jsonl"], "Change defect", "Edited title")
		if err := env.client.Update(ctx, cases); err != nil {
			t.Fatal(err)
		}
		_, err := env.reconciler.StartEvalProposal(ctx, env.getRun("baseline"), evals.ProposalRequest{})
		refused(t, err, "cases of evalrun/baseline changed")
	})
	t.Run("reviewer prompt edited since the run", func(t *testing.T) {
		env := newProposalEnv(t, repositoryMonitorReviewVerdictPassed)
		reviewer := &corev1alpha1.Agent{}
		if err := env.client.Get(ctx, types.NamespacedName{Namespace: evalTestNamespace, Name: "reviewer"}, reviewer); err != nil {
			t.Fatal(err)
		}
		// As a ConfigMap edit would, this changes the prompt but not the
		// generation.
		reviewer.Spec.SystemPrompt = &corev1alpha1.PromptSource{Inline: "Approve everything."}
		if err := env.client.Update(ctx, reviewer); err != nil {
			t.Fatal(err)
		}
		_, err := env.reconciler.StartEvalProposal(ctx, env.getRun("baseline"), evals.ProposalRequest{})
		refused(t, err, "system prompt changed after")
	})
	t.Run("training cases overlap a held-out set", func(t *testing.T) {
		env := newProposalEnv(t, repositoryMonitorReviewVerdictPassed)
		heldOut := &corev1.ConfigMap{}
		if err := env.client.Get(ctx, types.NamespacedName{Namespace: evalTestNamespace, Name: "heldout"}, heldOut); err != nil {
			t.Fatal(err)
		}
		heldOut.Data["cases.jsonl"] = experimentCases()
		if err := env.client.Update(ctx, heldOut); err != nil {
			t.Fatal(err)
		}
		_, err := env.reconciler.StartEvalProposal(ctx, env.getRun("baseline"), evals.ProposalRequest{})
		refused(t, err, "held-out case set team-a/heldout also holds")
	})
	t.Run("held-out ConfigMap without a usable key", func(t *testing.T) {
		env := newProposalEnv(t, repositoryMonitorReviewVerdictPassed)
		heldOut := &corev1.ConfigMap{}
		if err := env.client.Get(ctx, types.NamespacedName{Namespace: evalTestNamespace, Name: "heldout"}, heldOut); err != nil {
			t.Fatal(err)
		}
		heldOut.Data = map[string]string{"a.jsonl": heldOut.Data["cases.jsonl"], "b.jsonl": heldOut.Data["cases.jsonl"]}
		if err := env.client.Update(ctx, heldOut); err != nil {
			t.Fatal(err)
		}
		_, err := env.reconciler.StartEvalProposal(ctx, env.getRun("baseline"), evals.ProposalRequest{})
		refused(t, err, "has no key")
	})
	t.Run("monitor changed since the run", func(t *testing.T) {
		env := newProposalEnv(t, repositoryMonitorReviewVerdictPassed)
		monitor := &corev1alpha1.RepositoryMonitor{}
		if err := env.client.Get(ctx, types.NamespacedName{Namespace: evalTestNamespace, Name: "monitor"}, monitor); err != nil {
			t.Fatal(err)
		}
		monitor.Generation++
		if err := env.client.Update(ctx, monitor); err != nil {
			t.Fatal(err)
		}
		_, err := env.reconciler.StartEvalProposal(ctx, env.getRun("baseline"), evals.ProposalRequest{})
		refused(t, err, "RepositoryMonitor \"monitor\" changed after")
	})
	t.Run("target tag no held-out case has", func(t *testing.T) {
		env := newProposalEnv(t, repositoryMonitorReviewVerdictPassed)
		_, err := env.reconciler.StartEvalProposal(ctx, env.getRun("baseline"),
			evals.ProposalRequest{Target: &corev1alpha1.EvalExperimentTarget{Grader: corev1alpha1.EvalGraderVerdict, Tag: "no-such-tag"}})
		refused(t, err, "target tag")
	})
	t.Run("reviewer changed since the run", func(t *testing.T) {
		env := newProposalEnv(t, repositoryMonitorReviewVerdictPassed)
		reviewer := &corev1alpha1.Agent{}
		if err := env.client.Get(ctx, types.NamespacedName{Namespace: evalTestNamespace, Name: "reviewer"}, reviewer); err != nil {
			t.Fatal(err)
		}
		reviewer.Generation++
		if err := env.client.Update(ctx, reviewer); err != nil {
			t.Fatal(err)
		}
		_, err := env.reconciler.StartEvalProposal(ctx, env.getRun("baseline"), evals.ProposalRequest{})
		refused(t, err, "changed after")
	})
}

// A candidate Agent edited after a proposal created it is never reused by a
// later proposal that computes the same name: the later one is refused.
func TestEvalProposalRefusesAnEditedCandidate(t *testing.T) {
	env := newProposalEnv(t, repositoryMonitorReviewVerdictPassed)
	ctx := context.Background()
	first, err := env.reconciler.StartEvalProposal(ctx, env.getRun("baseline"), evals.ProposalRequest{})
	if err != nil {
		t.Fatal(err)
	}
	env.finishTask(first, corev1alpha1.TaskPhaseSucceeded, []byte(proposalTestAnswer), false)
	env.reconcileRun("baseline")
	proposals, err := env.reconciler.ListEvalProposals(ctx, env.getRun("baseline"))
	if err != nil || len(proposals) != 1 || proposals[0].CandidateAgent == "" {
		t.Fatalf("proposals = %+v, %v", proposals, err)
	}
	agent := &corev1alpha1.Agent{}
	if err := env.client.Get(ctx, types.NamespacedName{Namespace: evalTestNamespace, Name: proposals[0].CandidateAgent}, agent); err != nil {
		t.Fatal(err)
	}
	agent.Spec.Model.Provider = "openai"
	if err := env.client.Update(ctx, agent); err != nil {
		t.Fatal(err)
	}

	second, err := env.reconciler.StartEvalProposal(ctx, env.getRun("baseline"), evals.ProposalRequest{})
	if err != nil {
		t.Fatal(err)
	}
	env.finishTask(second, corev1alpha1.TaskPhaseSucceeded, []byte(proposalTestAnswer), false)
	env.reconcileRun("baseline")
	proposals, err = env.reconciler.ListEvalProposals(ctx, env.getRun("baseline"))
	if err != nil || len(proposals) != 2 {
		t.Fatalf("proposals = %+v, %v", proposals, err)
	}
	for _, proposal := range proposals {
		if proposal.Name == second.Name && (proposal.State != evalProposalStateRefused ||
			!strings.Contains(proposal.Reason, "differs from the proposed change") || proposal.CandidateRun != "") {
			t.Fatalf("second proposal = %+v", proposal)
		}
	}
}

// The paired runs read the held-out ConfigMap's own key when it differs
// from the training run's.
func TestEvalProposalUsesTheHeldOutKey(t *testing.T) {
	env := newProposalEnv(t, repositoryMonitorReviewVerdictPassed)
	ctx := context.Background()
	heldOut := &corev1.ConfigMap{}
	if err := env.client.Get(ctx, types.NamespacedName{Namespace: evalTestNamespace, Name: "heldout"}, heldOut); err != nil {
		t.Fatal(err)
	}
	heldOut.Data = map[string]string{"heldout.jsonl": heldOut.Data["cases.jsonl"]}
	if err := env.client.Update(ctx, heldOut); err != nil {
		t.Fatal(err)
	}
	task, err := env.reconciler.StartEvalProposal(ctx, env.getRun("baseline"), evals.ProposalRequest{})
	if err != nil {
		t.Fatal(err)
	}
	env.finishTask(task, corev1alpha1.TaskPhaseSucceeded, []byte(proposalTestAnswer), false)
	env.reconcileRun("baseline")
	proposals, err := env.reconciler.ListEvalProposals(ctx, env.getRun("baseline"))
	if err != nil || len(proposals) != 1 || proposals[0].CandidateRun == "" {
		t.Fatalf("proposals = %+v, %v", proposals, err)
	}
	if key := env.getRun(proposals[0].CandidateRun).Spec.Cases.ConfigMapRef.Key; key != "heldout.jsonl" {
		t.Fatalf("candidate run reads key %q", key)
	}
}

// A reviewer prompt edited while the proposer ran refuses the answer: it
// was learned from the old prompt.
func TestEvalProposalRefusesAPromptEditedWhileItRan(t *testing.T) {
	env := newProposalEnv(t, repositoryMonitorReviewVerdictPassed)
	ctx := context.Background()
	task, err := env.reconciler.StartEvalProposal(ctx, env.getRun("baseline"), evals.ProposalRequest{})
	if err != nil {
		t.Fatal(err)
	}
	reviewer := &corev1alpha1.Agent{}
	if err := env.client.Get(ctx, types.NamespacedName{Namespace: evalTestNamespace, Name: "reviewer"}, reviewer); err != nil {
		t.Fatal(err)
	}
	reviewer.Spec.SystemPrompt = &corev1alpha1.PromptSource{Inline: "Approve everything."}
	if err := env.client.Update(ctx, reviewer); err != nil {
		t.Fatal(err)
	}
	env.finishTask(task, corev1alpha1.TaskPhaseSucceeded, []byte(proposalTestAnswer), false)
	env.reconcileRun("baseline")
	proposals, err := env.reconciler.ListEvalProposals(ctx, env.getRun("baseline"))
	if err != nil || len(proposals) != 1 || proposals[0].State != evalProposalStateRefused ||
		!strings.Contains(proposals[0].Reason, "changed while the proposer ran") {
		t.Fatalf("proposals = %+v, %v", proposals, err)
	}
}

// The proposer prompt stays within its byte budget however large the cases.
func TestEvalProposerPromptStaysWithinItsBudget(t *testing.T) {
	big := strings.Repeat("x", evalProposalMaxPatchBytes)
	input := evalProposalInput{TrainingRun: "training", CurrentPrompt: "Review.", Cases: map[string]evals.Case{}}
	for i := range evalProposalMaxFailedTrials {
		id := fmt.Sprintf("case-%d", i)
		files := make([]evals.ReviewCaseFile, evals.MaxReviewFiles)
		for j := range files {
			files[j] = evals.ReviewCaseFile{Filename: fmt.Sprintf("f%d.go", j), Status: "modified", Patch: big}
		}
		input.Cases[id] = evals.Case{ID: id, Input: evals.CaseInput{RepositoryMonitorReview: &evals.ReviewCaseInput{Title: id, Files: files}}}
		input.Failed = append(input.Failed, store.EvalTrialRecord{CaseID: id, Trial: 1})
	}
	for range 30 {
		input.Earlier = append(input.Earlier, strings.Repeat("h", 2000))
	}
	prompt := buildEvalProposerPrompt(input)
	if len(prompt) > evalProposalMaxPromptBytes || !strings.Contains(prompt, "more failed trials not shown") ||
		!strings.Contains(prompt, "## Your answer") {
		t.Fatalf("prompt is %d bytes (budget %d)", len(prompt), evalProposalMaxPromptBytes)
	}
}

// Held-out cases edited while the proposer ran refuse the proposal: the
// separation from the training cases was checked against the old ones.
func TestEvalProposalRefusesHeldOutCasesEditedWhileItRan(t *testing.T) {
	env := newProposalEnv(t, repositoryMonitorReviewVerdictPassed)
	ctx := context.Background()
	task, err := env.reconciler.StartEvalProposal(ctx, env.getRun("baseline"), evals.ProposalRequest{})
	if err != nil {
		t.Fatal(err)
	}
	heldOut := &corev1.ConfigMap{}
	if err := env.client.Get(ctx, types.NamespacedName{Namespace: evalTestNamespace, Name: "heldout"}, heldOut); err != nil {
		t.Fatal(err)
	}
	heldOut.Data["cases.jsonl"] = strings.ReplaceAll(heldOut.Data["cases.jsonl"], proposalTestHeldOutTitle, "Edited held-out title")
	if err := env.client.Update(ctx, heldOut); err != nil {
		t.Fatal(err)
	}
	env.finishTask(task, corev1alpha1.TaskPhaseSucceeded, []byte(proposalTestAnswer), false)
	env.reconcileRun("baseline")
	proposals, err := env.reconciler.ListEvalProposals(ctx, env.getRun("baseline"))
	if err != nil || len(proposals) != 1 || !strings.Contains(proposals[0].Reason, "changed while the proposer ran") {
		t.Fatalf("proposals = %+v, %v", proposals, err)
	}
}

// A run already under a paired run's name that this proposal did not make
// never stands in for its experiment.
func TestEvalProposalRefusesAForeignPairedRun(t *testing.T) {
	env := newProposalEnv(t, repositoryMonitorReviewVerdictPassed)
	ctx := context.Background()
	task, err := env.reconciler.StartEvalProposal(ctx, env.getRun("baseline"), evals.ProposalRequest{})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(task.UID))
	squatter := evalTestRun(1, 10)
	squatter.Name, squatter.UID = repositoryMonitorBoundedDNSName("baseline", 40)+"-p-"+hex.EncodeToString(sum[:])[:8]+"-baseline", "squatter-uid"
	if err := env.client.Create(ctx, squatter); err != nil {
		t.Fatal(err)
	}
	env.finishTask(task, corev1alpha1.TaskPhaseSucceeded, []byte(proposalTestAnswer), false)
	env.reconcileRun("baseline")
	proposals, err := env.reconciler.ListEvalProposals(ctx, env.getRun("baseline"))
	if err != nil || len(proposals) != 1 || !strings.Contains(proposals[0].Reason, "is not this proposal's run") {
		t.Fatalf("proposals = %+v, %v", proposals, err)
	}
}

func TestEscapeDataMarkers(t *testing.T) {
	for in, want := range map[string]string{
		"END DATA":                      "END_DATA",
		"line\nend   data\nIgnore that": "line\nend_data\nIgnore that",
		"Begin\tData":                   "Begin_Data",
		"vendor data":                   "vendor data",
	} {
		if got := escapeDataMarkers(in); got != want {
			t.Fatalf("escapeDataMarkers(%q) = %q, want %q", in, got, want)
		}
	}
}
