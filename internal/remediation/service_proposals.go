package remediation

import (
	"errors"
	"strings"

	pv "github.com/orka-agents/orka/internal/patchverification"
)

type SourceSelection struct {
	Files []string `json:"files"`
}

type ReproductionReview struct {
	Reproduced  *bool    `json:"reproduced"`
	Summary     string   `json:"summary"`
	Limitations []string `json:"limitations"`
}

type CandidateReview struct {
	Approved      *bool          `json:"approved"`
	Summary       string         `json:"summary"`
	Limitations   []string       `json:"limitations"`
	RevisedChecks *CheckProposal `json:"revisedChecks,omitempty"`
}

func DecodeSourceSelection(raw string) (SourceSelection, error) {
	var selection SourceSelection
	if err := decodeProposal(raw, &selection); err != nil {
		return selection, err
	}
	if len(selection.Files) == 0 || len(selection.Files) > 32 {
		return selection, errors.New("source selection requires one to 32 files")
	}
	seen := make(map[string]bool)
	for _, name := range selection.Files {
		if name == "" || len(name) > 255 || seen[strings.ToLower(name)] {
			return selection, errors.New("source selection contains an invalid or duplicate file")
		}
		seen[strings.ToLower(name)] = true
	}
	return selection, nil
}

func DecodeReproductionReview(raw string) (ReproductionReview, error) {
	var review ReproductionReview
	if err := decodeProposal(raw, &review); err != nil {
		return review, err
	}
	if review.Reproduced == nil || !validReviewText(review.Summary, review.Limitations) {
		return review, errors.New("reproduction review lacks an explicit decision or bounded explanation")
	}
	return review, nil
}

func DecodeCandidateReview(raw string) (CandidateReview, error) {
	var review CandidateReview
	if err := decodeProposal(raw, &review); err != nil {
		return review, err
	}
	if review.Approved == nil || !validReviewText(review.Summary, review.Limitations) ||
		(*review.Approved && review.RevisedChecks != nil) {
		return review, errors.New("candidate review lacks an explicit decision or requests unexecuted checks")
	}
	return review, nil
}

func validReviewText(summary string, limitations []string) bool {
	if strings.TrimSpace(summary) == "" || len(summary) > 8192 || len(limitations) > 32 {
		return false
	}
	for _, limitation := range limitations {
		if strings.TrimSpace(limitation) == "" || len(limitation) > 4096 {
			return false
		}
	}
	return true
}

// FreezeProposalFiles hashes the exact model-proposed bytes. These files are
// untrusted subjects; only the execution backend supplies observations.
func FreezeProposalFiles(proposal CheckProposal) ([]pv.FrozenFile, error) {
	return freezeProposalFiles(proposal, nil)
}

func FreezeApplicationProposalFiles(proposal CheckProposal, policy *pv.ApplicationPolicy) ([]pv.FrozenFile, error) {
	if proposal.Application == nil {
		return nil, errors.New("application-runtime proposals require an application plan")
	}
	return freezeProposalFiles(proposal, policy)
}

func freezeProposalFiles(proposal CheckProposal, policy *pv.ApplicationPolicy) ([]pv.FrozenFile, error) {
	if proposal.Application != nil {
		if len(proposal.Files) != 0 || len(proposal.Services) != 0 || len(proposal.Requirements) != 0 {
			return nil, errors.New("application checks cannot carry executable files or ambient services")
		}
		if err := pv.ValidateApplicationPlanWithPolicy(*proposal.Application, policy); err != nil {
			return nil, err
		}
		reproduction, normal := false, false
		for _, check := range proposal.Checks {
			if err := pv.ValidateApplicationCheckWithPolicy(check, policy); err != nil {
				return nil, err
			}
			reproduction = reproduction || check.Kind == pv.Reproduction
			normal = normal || check.Kind == pv.Normal
		}
		if !reproduction || !normal {
			return nil, errors.New("application reproduction and normal behavior checks are both required")
		}
		return nil, nil
	}
	if err := requireProtectedChecks(proposal); err != nil {
		return nil, err
	}
	if len(proposal.Services) != 0 {
		return nil, errors.New("protected HTTP checks cannot declare additional services")
	}
	reproduction, normal := false, false
	for _, check := range proposal.Checks {
		if err := pv.ValidateHTTPCheck(check); err != nil {
			return nil, err
		}
		reproduction = reproduction || check.Kind == pv.Reproduction
		normal = normal || check.Kind == pv.Normal
	}
	if !reproduction || !normal {
		return nil, errors.New("reproduction and normal behavior checks are both required")
	}
	for _, requirement := range proposal.Requirements {
		if requirement.Kind != "process" {
			return nil, errors.New("requested environment cannot be provided by the protected HTTP runner")
		}
	}
	files := make([]pv.FrozenFile, 0, len(proposal.Files))
	total := 0
	for _, file := range proposal.Files {
		total += len(file.Content)
		if total > 64<<10 || (pv.CredentialMatcher{}).MatchString(file.Content) {
			return nil, errors.New("check files exceed the private execution input bounds")
		}
		content := []byte(file.Content)
		files = append(files, pv.FrozenFile{
			Path: file.Path, Content: content, Digest: pv.Digest(content), Executable: file.Executable,
		})
	}
	return files, nil
}
