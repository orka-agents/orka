package security

import (
	"slices"
	"strings"
)

// Review coverage dispositions recorded on a reviewed slice.
const (
	// ReviewCoverageComplete: the review corroborates every file the slice
	// owns, or, for an investigation or baseline slice, it read trusted
	// context and settled at least one question or reported a finding.
	ReviewCoverageComplete = "complete"
	// ReviewCoveragePartial: the review corroborates some trusted context,
	// but not all of what the slice owns or no settled question.
	ReviewCoveragePartial = "partial"
	// ReviewCoverageNone: the review corroborates no trusted context file.
	ReviewCoverageNone = "none"
)

// ReviewCoverage is what a review result corroborates about the slice it was
// given. A result that echoes the empty template, or names only files outside
// its trusted context, corroborates nothing.
type ReviewCoverage struct {
	Disposition string
	// TrustedFiles counts the slice's readable included files: what a review
	// could have corroborated at all.
	TrustedFiles int
	// ReviewedFiles are the slice's readable included files the review
	// listed in scan.reviewedFiles or cited in accepted evidence, sorted.
	// Only these support resolving a finding the review did not observe.
	ReviewedFiles []string
}

// AssessReviewCoverage compares what a review claims to have examined with the
// slice's trusted context. claimed is the result's scan.reviewedFiles and
// evidencePaths the files its accepted findings cite; both are reviewer
// assertions, so only paths the mapper included and could read count.
// Component slices must corroborate every owned file to be complete.
// Investigation and baseline slices reach across components, so a review of
// one is complete once it corroborates trusted context and settles a
// question or reports a finding. A question settles only with a resolution:
// listing it is not checking it.
func AssessReviewCoverage(sliceKind string, manifest ReviewContextManifest, claimed, evidencePaths []string, questions []FindingsV2ResolvedQuestion) ReviewCoverage {
	settled := 0
	for _, question := range questions {
		if strings.TrimSpace(question.Question) != "" && strings.TrimSpace(question.Resolution) != "" {
			settled++
		}
	}
	readable := map[string]string{}
	for _, file := range manifest.IncludedFiles {
		path := strings.TrimSpace(file.Path)
		if file.Readable && SafeRepoPath(path) {
			readable[path] = file.Role
		}
	}
	corroborated := map[string]struct{}{}
	for _, path := range slices.Concat(claimed, evidencePaths) {
		path = strings.TrimSpace(path)
		if _, ok := readable[path]; ok {
			corroborated[path] = struct{}{}
		}
	}
	coverage := ReviewCoverage{Disposition: ReviewCoverageNone, TrustedFiles: len(readable)}
	if len(corroborated) == 0 {
		return coverage
	}
	for path := range corroborated {
		coverage.ReviewedFiles = append(coverage.ReviewedFiles, path)
	}
	slices.Sort(coverage.ReviewedFiles)

	owned := 0
	missingOwned := false
	for path, role := range readable {
		if role != "owned" {
			continue
		}
		owned++
		if _, ok := corroborated[path]; !ok {
			missingOwned = true
		}
	}
	crossComponent := sliceKind == SliceKindInvestigation || sliceKind == SliceKindBaseline
	switch {
	case !crossComponent && owned > 0 && !missingOwned:
		coverage.Disposition = ReviewCoverageComplete
	case !crossComponent && owned > 0:
		coverage.Disposition = ReviewCoveragePartial
	case settled > 0 || len(evidencePaths) > 0:
		coverage.Disposition = ReviewCoverageComplete
	default:
		coverage.Disposition = ReviewCoveragePartial
	}
	return coverage
}
