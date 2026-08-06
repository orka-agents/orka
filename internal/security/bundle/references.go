package bundle

import (
	"fmt"
	"sort"
)

func validateCrossDocumentReferences(manifest semanticManifestDocument, findings FindingsInput, coverage CoverageInput) error {
	coverageStatuses := []struct {
		manifestName  string
		manifestValue string
		coverageName  string
		coverageValue string
	}{
		{"inventoryCoverage", manifest.Quality.InventoryCoverage, "inventoryStatus", coverage.InventoryStatus},
		{"candidateCoverage", manifest.Quality.CandidateCoverage, "candidateStatus", coverage.CandidateStatus},
		{"coverage", manifest.Quality.Coverage, "coverageStatus", coverage.CoverageStatus},
	}
	for _, status := range coverageStatuses {
		if status.manifestValue != status.coverageValue {
			return fmt.Errorf(
				"manifest quality %s %q does not match coverage document %s %q",
				status.manifestName,
				status.manifestValue,
				status.coverageName,
				status.coverageValue,
			)
		}
	}

	findingOccurrences := make([]string, 0, len(findings.Findings))
	findingAssessments := make([]string, 0)
	seenAssessments := map[string]string{}
	evidenceReceiptSet := stringSet(manifest.EvidenceReceiptIDs)
	for _, finding := range findings.Findings {
		findingOccurrences = append(findingOccurrences, finding.OccurrenceID)
		for _, assessmentID := range finding.AssessmentIDs {
			if prior, exists := seenAssessments[assessmentID]; exists {
				return fmt.Errorf("assessment %q is attached to multiple findings %q and %q", assessmentID, prior, finding.OccurrenceID)
			}
			seenAssessments[assessmentID] = finding.OccurrenceID
			findingAssessments = append(findingAssessments, assessmentID)
		}
		for _, evidence := range finding.Evidence {
			if evidence.ReceiptID != nil {
				if _, ok := evidenceReceiptSet[*evidence.ReceiptID]; !ok {
					return fmt.Errorf("finding %q references unknown evidence receipt %q", finding.OccurrenceID, *evidence.ReceiptID)
				}
			}
		}
	}
	if err := requireExactIDSet("manifest occurrenceIds", manifest.OccurrenceIDs, findingOccurrences); err != nil {
		return err
	}
	if err := requireExactIDSet("manifest assessmentIds", manifest.AssessmentIDs, findingAssessments); err != nil {
		return err
	}
	stageReceipts := make([]string, 0, len(coverage.Stages))
	for _, stage := range coverage.Stages {
		stageReceipts = append(stageReceipts, stage.ReceiptID)
	}
	if err := requireExactIDSet("manifest stageReceiptIds", manifest.StageReceiptIDs, stageReceipts); err != nil {
		return err
	}
	occurrenceSet := stringSet(manifest.OccurrenceIDs)
	receiptSet := stringSet(append(append([]string{}, manifest.StageReceiptIDs...), manifest.EvidenceReceiptIDs...))
	for _, inventory := range coverage.Inventory {
		for _, receiptID := range inventory.ReceiptIDs {
			if _, ok := receiptSet[receiptID]; !ok {
				return fmt.Errorf("inventory path %q references unknown receipt %q", inventory.Path, receiptID)
			}
		}
	}
	for _, candidate := range coverage.Candidates {
		if candidate.OccurrenceID != nil {
			if _, ok := occurrenceSet[*candidate.OccurrenceID]; !ok {
				return fmt.Errorf("coverage candidate %q references unknown occurrence %q", candidate.CandidateID, *candidate.OccurrenceID)
			}
		}
		for _, receiptID := range candidate.ReceiptIDs {
			if _, ok := receiptSet[receiptID]; !ok {
				return fmt.Errorf("coverage candidate %q references unknown receipt %q", candidate.CandidateID, receiptID)
			}
		}
	}
	return nil
}

func requireExactIDSet(name string, expected, actual []string) error {
	leftSet := stringSet(expected)
	rightSet := stringSet(actual)
	if len(leftSet) != len(expected) {
		return fmt.Errorf("%s contains duplicate manifest references", name)
	}
	if len(rightSet) != len(actual) {
		return fmt.Errorf("%s contains duplicate canonical document references", name)
	}
	if len(leftSet) != len(rightSet) {
		return fmt.Errorf("%s does not match canonical document references", name)
	}
	left := make([]string, 0, len(leftSet))
	right := make([]string, 0, len(rightSet))
	for value := range leftSet {
		left = append(left, value)
	}
	for value := range rightSet {
		right = append(right, value)
	}
	sort.Strings(left)
	sort.Strings(right)
	for i := range left {
		if left[i] != right[i] {
			return fmt.Errorf("%s does not match canonical document references", name)
		}
	}
	return nil
}

func stringSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}
