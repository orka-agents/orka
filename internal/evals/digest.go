package evals

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// DigestPrefix prefixes every digest this package returns.
const DigestPrefix = "sha256:"

// CaseSetDigest returns the digest of the selected cases in their order. Any
// change to a case's input, reference, tags or id changes it.
func CaseSetDigest(cases []Case) (string, error) {
	return digestJSON(struct {
		Schema string `json:"schema"`
		Cases  []Case `json:"cases"`
	}{Schema: corev1alpha1.EvalCaseSchemaVersion, Cases: cases})
}

// HeldOutCaseKeys returns the sorted, distinct keys of the evidence the
// cases hold out: for a review case, the reviewed repository and commits.
// Case IDs, order, tags, references and descriptive text do not change a
// key, so rearranging or relabeling held-out cases never makes them fresh.
func HeldOutCaseKeys(cases []Case) ([]string, error) {
	seen := map[string]struct{}{}
	keys := make([]string, 0, len(cases))
	for i := range cases {
		input := cases[i].Input.RepositoryMonitorReview
		if input == nil {
			return nil, fmt.Errorf("case %q has no repositoryMonitorReview input", cases[i].ID)
		}
		key, err := digestJSON(struct {
			RepoURL string `json:"repoURL"`
			BaseSHA string `json:"baseSHA"`
			HeadSHA string `json:"headSHA"`
		}{input.RepoURL, input.BaseSHA, input.HeadSHA})
		if err != nil {
			return nil, err
		}
		if _, dup := seen[key]; !dup {
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// GraderSetDigest returns the digest of the named graders' names and
// versions. Order and repetition of names do not matter. An unknown name is
// an error.
func (r *Registry) GraderSetDigest(names []corev1alpha1.EvalGraderName) (string, error) {
	type graderIdentity struct {
		Name    corev1alpha1.EvalGraderName `json:"name"`
		Version string                      `json:"version"`
	}
	seen := map[corev1alpha1.EvalGraderName]struct{}{}
	identities := make([]graderIdentity, 0, len(names))
	for _, name := range names {
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		grader, err := r.Lookup(name)
		if err != nil {
			return "", err
		}
		identities = append(identities, graderIdentity{Name: name, Version: grader.Version()})
	}
	sort.Slice(identities, func(i, j int) bool { return identities[i].Name < identities[j].Name })
	return digestJSON(identities)
}

func digestJSON(value any) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode digest input: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return DigestPrefix + hex.EncodeToString(sum[:]), nil
}

// PlannedTrial is one trial of one case in a run's trial plan. Trial is one
// based.
type PlannedTrial struct {
	CaseID string `json:"caseID"`
	Trial  int32  `json:"trial"`
}

// TrialPlan orders every trial of every case: the first trial of each case in
// case order, then the second, and so on. Trials of one case are spread over
// the run instead of running back to back under the same conditions. The
// plan depends only on the case order and the trial count, so two runs over
// the same cases and trials always assign the same trials.
func TrialPlan(cases []Case, trials int32) []PlannedTrial {
	if trials < 1 {
		return nil
	}
	plan := make([]PlannedTrial, 0, len(cases)*int(trials))
	for trial := int32(1); trial <= trials; trial++ {
		for _, c := range cases {
			plan = append(plan, PlannedTrial{CaseID: c.ID, Trial: trial})
		}
	}
	return plan
}

// TrialPlanDigest returns the digest of a trial plan in its order.
func TrialPlanDigest(plan []PlannedTrial) (string, error) {
	return digestJSON(struct {
		Schema string         `json:"schema"`
		Plan   []PlannedTrial `json:"plan"`
	}{Schema: "orka.eval.trialPlan.v1", Plan: plan})
}

// DigestJSON digests value's JSON encoding, for snapshot digests built
// outside this package.
func DigestJSON(value any) (string, error) {
	return digestJSON(value)
}
