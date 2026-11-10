package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/evals"
	"github.com/orka-agents/orka/internal/labels"
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
