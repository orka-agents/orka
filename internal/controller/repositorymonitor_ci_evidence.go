package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/redact"
	"github.com/orka-agents/orka/internal/security"
)

const (
	repositoryMonitorCIEvidenceSchemaVersion = "orka.prRepair.ciEvidence.v1"

	repositoryMonitorEvidenceAvailable   = "available"
	repositoryMonitorEvidenceTruncated   = "truncated"
	repositoryMonitorEvidenceUnavailable = "unavailable"
	repositoryMonitorEvidenceRateLimited = "rate_limited"
	repositoryMonitorEvidenceUnsupported = "unsupported"
	repositoryMonitorEvidenceStale       = "stale"

	repositoryMonitorEvidenceKindCheckRun     = "check_run"
	repositoryMonitorEvidenceKindCommitStatus = "commit_status"

	repositoryMonitorEvidenceReasonIncomplete        = "ci_checks_incomplete"
	repositoryMonitorEvidenceReasonAnnotationLimit   = "annotation_fetch_limit"
	repositoryMonitorEvidenceReasonAfterRateLimit    = "skipped_after_rate_limit"
	repositoryMonitorEvidenceReasonEvidenceBudget    = "evidence_budget_exhausted"
	repositoryMonitorEvidenceReasonHeadMismatch      = "check_head_sha_mismatch"
	repositoryMonitorEvidenceReasonCredentialMissing = "credential_unavailable"

	repositoryMonitorCIEvidenceMaxChecks             = 10
	repositoryMonitorCIEvidenceMaxPendingChecks      = 10
	repositoryMonitorCIEvidenceMaxAnnotationChecks   = 5
	repositoryMonitorCIEvidenceAnnotationPages       = 2
	repositoryMonitorCIEvidenceAnnotationsPerPage    = 50
	repositoryMonitorCIEvidenceMaxAnnotations        = 10
	repositoryMonitorCIEvidenceNameBytes             = 200
	repositoryMonitorCIEvidenceTitleBytes            = 256
	repositoryMonitorCIEvidenceSummaryBytes          = 1536
	repositoryMonitorCIEvidenceSummaryLines          = 30
	repositoryMonitorCIEvidenceDescriptionBytes      = 512
	repositoryMonitorCIEvidenceAnnotationTitleBytes  = 200
	repositoryMonitorCIEvidenceAnnotationBytes       = 512
	repositoryMonitorCIEvidenceAnnotationLines       = 8
	repositoryMonitorCIEvidencePathBytes             = 300
	repositoryMonitorCIEvidenceURLBytes              = 512
	repositoryMonitorCIEvidenceEntryEncodedBytes     = 4 << 10
	repositoryMonitorCIEvidenceTotalEncodedBytes     = 16 << 10
	repositoryMonitorCIEvidenceFetchBudget           = 20 * time.Second
	repositoryMonitorCIEvidenceEntryPrefix           = "      "
	repositoryMonitorCIEvidenceEntryIndent           = "  "
	repositoryMonitorCIEvidenceEntrySeparatorBytes   = len(",\n") + len(repositoryMonitorCIEvidenceEntryPrefix)
	repositoryMonitorEvidenceMinRawTextLimit         = 2 << 10
	repositoryMonitorEvidenceRawTextFactor           = 4
	repositoryMonitorEvidenceSecretCheckBudget       = 32 << 10
	repositoryMonitorEvidenceSecretJoinBytes         = 2 << 10
	repositoryMonitorEvidenceRedactedLine            = "[REDACTED]"
	repositoryMonitorEvidenceTruncatedFieldSummary   = "summary"
	repositoryMonitorEvidenceTruncatedFieldTitle     = "title"
	repositoryMonitorEvidenceTruncatedFieldName      = "name"
	repositoryMonitorEvidenceTruncatedFieldDesc      = "description"
	repositoryMonitorEvidenceTruncatedFieldURL       = "url"
	repositoryMonitorEvidenceTruncatedFieldAnnots    = "annotations"
	repositoryMonitorEvidenceTruncatedFieldAnnotText = "annotationText"

	repositoryMonitorCIEvidenceDigestMetadataKey       = "ciEvidenceDigest"
	repositoryMonitorCIEvidenceIDsMetadataKey          = "ciEvidenceIDs"
	repositoryMonitorCIEvidenceAvailabilityMetadataKey = "ciEvidenceAvailability"
)

type repositoryMonitorCIEvidence struct {
	SchemaVersion        string                               `json:"schemaVersion"`
	Repo                 string                               `json:"repo"`
	HeadSHA              string                               `json:"headSHA"`
	BaseSHA              string                               `json:"baseSHA,omitempty"`
	Availability         string                               `json:"availability"`
	Reason               string                               `json:"reason,omitempty"`
	ChecksIncomplete     bool                                 `json:"checksIncomplete,omitempty"`
	TextOmitted          bool                                 `json:"textOmitted,omitempty"`
	Checks               []repositoryMonitorCheckEvidence     `json:"checks"`
	OmittedChecks        int                                  `json:"omittedChecks,omitempty"`
	PendingChecks        []repositoryMonitorPendingCheckEntry `json:"pendingChecks,omitempty"`
	OmittedPendingChecks int                                  `json:"omittedPendingChecks,omitempty"`
}

type repositoryMonitorCheckEvidence struct {
	ID              string                                  `json:"id"`
	Kind            string                                  `json:"kind"`
	Name            string                                  `json:"name"`
	App             string                                  `json:"app,omitempty"`
	Status          string                                  `json:"status,omitempty"`
	Conclusion      string                                  `json:"conclusion,omitempty"`
	URL             string                                  `json:"url,omitempty"`
	Title           string                                  `json:"title,omitempty"`
	Summary         string                                  `json:"summary,omitempty"`
	Description     string                                  `json:"description,omitempty"`
	AnnotationCount int                                     `json:"annotationCount,omitempty"`
	Annotations     []repositoryMonitorCheckAnnotationEntry `json:"annotations,omitempty"`
	Availability    string                                  `json:"availability"`
	Reason          string                                  `json:"reason,omitempty"`
	Truncated       []string                                `json:"truncated,omitempty"`
}

type repositoryMonitorCheckAnnotationEntry struct {
	Path      string `json:"path,omitempty"`
	StartLine int    `json:"startLine,omitempty"`
	EndLine   int    `json:"endLine,omitempty"`
	Level     string `json:"level,omitempty"`
	Title     string `json:"title,omitempty"`
	Message   string `json:"message"`
}

type repositoryMonitorPendingCheckEntry struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	App    string `json:"app,omitempty"`
	Status string `json:"status,omitempty"`
}

type repositoryMonitorRepairCIInput struct {
	BaseSHA           string                      `json:"baseSHA,omitempty"`
	FailingChecks     []string                    `json:"failingChecks,omitempty"`
	BaseFailingChecks []string                    `json:"baseFailingChecks,omitempty"`
	Evidence          repositoryMonitorCIEvidence `json:"failingCheckEvidence"`
}

func (in repositoryMonitorRepairCIInput) digest() string {
	encoded, err := json.Marshal(in)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (in repositoryMonitorRepairCIInput) evidenceIDs() []string {
	ids := make([]string, 0, len(in.Evidence.Checks)+len(in.Evidence.PendingChecks))
	for _, check := range in.Evidence.Checks {
		ids = append(ids, check.ID)
	}
	for _, check := range in.Evidence.PendingChecks {
		ids = append(ids, check.ID)
	}
	return ids
}

type repositoryMonitorCheckRunResponse struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
	App        struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
	} `json:"app"`
	Output struct {
		Title            string `json:"title"`
		Summary          string `json:"summary"`
		AnnotationsCount int    `json:"annotations_count"`
	} `json:"output"`
}

func (c repositoryMonitorCheckRunResponse) failure() repositoryMonitorCIFailure {
	return repositoryMonitorCIFailure{
		name: c.Name, source: fmt.Sprintf("check_run:%d", c.App.ID),
		id: c.ID, appSlug: c.App.Slug, headSHA: c.HeadSHA, status: c.Status, conclusion: c.Conclusion,
		url: c.HTMLURL, title: c.Output.Title, summary: c.Output.Summary, annotationCount: c.Output.AnnotationsCount,
	}
}

type repositoryMonitorCheckAnnotationResponse struct {
	Path            string `json:"path"`
	StartLine       int    `json:"start_line"`
	EndLine         int    `json:"end_line"`
	AnnotationLevel string `json:"annotation_level"`
	Title           string `json:"title"`
	Message         string `json:"message"`
}

func newRepositoryMonitorCIEvidence(repo string, pr repositoryMonitorPullRequest) repositoryMonitorCIEvidence {
	return repositoryMonitorCIEvidence{
		SchemaVersion: repositoryMonitorCIEvidenceSchemaVersion,
		Repo:          repo,
		HeadSHA:       pr.HeadSHA,
		BaseSHA:       pr.BaseSHA,
		Availability:  repositoryMonitorEvidenceAvailable,
		Checks:        []repositoryMonitorCheckEvidence{},
	}
}

func (r *RepositoryMonitorReconciler) buildRepositoryMonitorRepairCIInput(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, repo string, pr repositoryMonitorPullRequest) repositoryMonitorRepairCIInput {
	input := repositoryMonitorRepairCIInput{Evidence: newRepositoryMonitorCIEvidence(repo, pr)}
	failures, err := r.repositoryMonitorPullRequestCIFailures(ctx, monitor, pr)
	switch {
	case err != nil:
		input.Evidence.Availability, input.Evidence.Reason = repositoryMonitorEvidenceErrorAvailability(err)
		return input
	case failures.incomplete:
		input.Evidence.Availability, input.Evidence.Reason = repositoryMonitorEvidenceUnavailable, repositoryMonitorEvidenceReasonIncomplete
		return input
	case !failures.broke || len(failures.introduced) == 0:
		input.Evidence.Availability, input.Evidence.Reason = repositoryMonitorEvidenceUnavailable, repositoryMonitorRepairSkipNoIntroducedCIFailure
		return input
	}
	input.BaseSHA = pr.BaseSHA
	input.FailingChecks = boundRepositoryMonitorCheckNames(repositoryMonitorCIFailureNames(failures.introduced, failures.inherited))
	if len(failures.inherited) > 0 {
		input.BaseFailingChecks = boundRepositoryMonitorCheckNames(repositoryMonitorCIFailureNames(failures.inherited, failures.introduced))
	}
	input.Evidence = r.repositoryMonitorCIEvidenceFor(ctx, monitor, repo, pr, failures)
	return input
}

type repositoryMonitorEvidenceFetch struct {
	baseURL, owner, repo, token string
	tokenErr                    error
	rateLimited                 bool
	annotationChecks            int
	text                        bool
}

func (r *RepositoryMonitorReconciler) repositoryMonitorCIEvidenceFor(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, repo string, pr repositoryMonitorPullRequest, failures repositoryMonitorCIFailures) repositoryMonitorCIEvidence {
	evidence := newRepositoryMonitorCIEvidence(repo, pr)
	evidence.ChecksIncomplete = failures.headIncomplete
	ctx, cancel := context.WithTimeout(ctx, repositoryMonitorCIEvidenceFetchBudget)
	defer cancel()
	fetch := &repositoryMonitorEvidenceFetch{text: monitor.Spec.Repair.CIEvidence.Text}
	evidence.TextOmitted = !fetch.text
	fetch.token, fetch.tokenErr = r.repositoryMonitorGitHubToken(ctx, monitor)
	if owner, name, err := security.ParseGitHubRepositoryURL(monitor.Spec.RepoURL); err == nil {
		fetch.owner, fetch.repo = owner, name
	} else if fetch.tokenErr == nil {
		fetch.tokenErr = err
	}
	fetch.baseURL = strings.TrimRight(r.GitHubAPIBaseURL, "/")
	if fetch.baseURL == "" {
		fetch.baseURL = repositoryMonitorDefaultGitHubAPIBaseURL
	}
	used := 0
	for i, failure := range failures.introduced {
		if i == repositoryMonitorCIEvidenceMaxChecks {
			evidence.OmittedChecks += len(failures.introduced) - i
			break
		}
		entry := r.repositoryMonitorCheckEvidenceFor(ctx, fetch, failure, pr.HeadSHA)
		entry = fitRepositoryMonitorCheckEvidence(entry, repositoryMonitorCIEvidenceEntryEncodedBytes)
		size := repositoryMonitorEvidenceEncodedSize(entry)
		if size > repositoryMonitorCIEvidenceEntryEncodedBytes || used+size+repositoryMonitorCIEvidenceEntrySeparatorBytes > repositoryMonitorCIEvidenceTotalEncodedBytes {
			evidence.OmittedChecks++
			continue
		}
		used += size + repositoryMonitorCIEvidenceEntrySeparatorBytes
		evidence.Checks = append(evidence.Checks, entry)
	}
	for i, pending := range failures.pending {
		if i == repositoryMonitorCIEvidenceMaxPendingChecks {
			evidence.OmittedPendingChecks += len(failures.pending) - i
			break
		}
		name, _ := repositoryMonitorEvidenceLine(pending.name, repositoryMonitorCIEvidenceNameBytes)
		entry := repositoryMonitorPendingCheckEntry{
			ID:     repositoryMonitorEvidenceID(pending),
			Name:   name,
			App:    repositoryMonitorEvidenceToken(pending.appSlug),
			Status: repositoryMonitorEvidenceToken(pending.status),
		}
		size := repositoryMonitorEvidenceEncodedSize(entry) + repositoryMonitorCIEvidenceEntrySeparatorBytes
		if used+size > repositoryMonitorCIEvidenceTotalEncodedBytes {
			evidence.OmittedPendingChecks++
			continue
		}
		used += size
		evidence.PendingChecks = append(evidence.PendingChecks, entry)
	}
	if evidence.OmittedChecks > 0 || evidence.OmittedPendingChecks > 0 || evidence.ChecksIncomplete {
		evidence.Availability = repositoryMonitorEvidenceTruncated
		if evidence.OmittedChecks > 0 || evidence.OmittedPendingChecks > 0 {
			evidence.Reason = repositoryMonitorEvidenceReasonEvidenceBudget
		} else {
			evidence.Reason = repositoryMonitorEvidenceReasonIncomplete
		}
	}
	return evidence
}

func repositoryMonitorEvidenceID(failure repositoryMonitorCIFailure) string {
	if failure.source == repositoryMonitorCIFailureSourceStatus {
		return repositoryMonitorEvidenceKindCommitStatus + ":" + strconv.FormatInt(failure.id, 10)
	}
	return repositoryMonitorEvidenceKindCheckRun + ":" + strconv.FormatInt(failure.id, 10)
}

func (r *RepositoryMonitorReconciler) repositoryMonitorCheckEvidenceFor(ctx context.Context, fetch *repositoryMonitorEvidenceFetch, failure repositoryMonitorCIFailure, headSHA string) repositoryMonitorCheckEvidence {
	entry := repositoryMonitorCheckEvidence{
		ID:           repositoryMonitorEvidenceID(failure),
		Kind:         repositoryMonitorEvidenceKindCheckRun,
		Conclusion:   repositoryMonitorEvidenceToken(failure.conclusion),
		Availability: repositoryMonitorEvidenceAvailable,
	}
	var cut bool
	if entry.Name, cut = repositoryMonitorEvidenceLine(failure.name, repositoryMonitorCIEvidenceNameBytes); cut {
		entry.Truncated = append(entry.Truncated, repositoryMonitorEvidenceTruncatedFieldName)
	}
	if failure.source == repositoryMonitorCIFailureSourceStatus {
		entry.Kind = repositoryMonitorEvidenceKindCommitStatus
		entry.URL = repositoryMonitorEvidenceGitHubCheckURL(failure.url, fetch.owner, fetch.repo)
		if fetch.text {
			if entry.Description, cut = repositoryMonitorEvidenceLine(failure.description, repositoryMonitorCIEvidenceDescriptionBytes); cut {
				entry.Truncated = append(entry.Truncated, repositoryMonitorEvidenceTruncatedFieldDesc)
			}
		}
		entry.Availability = repositoryMonitorEvidenceUnsupported
		return entry
	}
	entry.App = repositoryMonitorEvidenceToken(failure.appSlug)
	entry.Status = repositoryMonitorEvidenceToken(failure.status)
	if sha := strings.TrimSpace(failure.headSHA); sha != "" && !strings.EqualFold(sha, headSHA) {
		entry.Availability, entry.Reason = repositoryMonitorEvidenceStale, repositoryMonitorEvidenceReasonHeadMismatch
		return entry
	}
	entry.URL = repositoryMonitorEvidenceGitHubCheckURL(failure.url, fetch.owner, fetch.repo)
	if fetch.text {
		if entry.Title, cut = repositoryMonitorEvidenceLine(failure.title, repositoryMonitorCIEvidenceTitleBytes); cut {
			entry.Truncated = append(entry.Truncated, repositoryMonitorEvidenceTruncatedFieldTitle)
		}
		if entry.Summary, cut = repositoryMonitorEvidenceText(failure.summary, repositoryMonitorCIEvidenceSummaryBytes, repositoryMonitorCIEvidenceSummaryLines); cut {
			entry.Truncated = append(entry.Truncated, repositoryMonitorEvidenceTruncatedFieldSummary)
		}
	}
	entry.AnnotationCount = max(failure.annotationCount, 0)
	if entry.AnnotationCount > 0 {
		r.repositoryMonitorAddCheckAnnotations(ctx, fetch, failure.id, &entry)
	}
	if entry.Availability == repositoryMonitorEvidenceAvailable && len(entry.Truncated) > 0 {
		entry.Availability = repositoryMonitorEvidenceTruncated
	}
	return entry
}

func (r *RepositoryMonitorReconciler) repositoryMonitorAddCheckAnnotations(ctx context.Context, fetch *repositoryMonitorEvidenceFetch, checkRunID int64, entry *repositoryMonitorCheckEvidence) {
	switch {
	case fetch.rateLimited:
		entry.Availability, entry.Reason = repositoryMonitorEvidenceUnavailable, repositoryMonitorEvidenceReasonAfterRateLimit
		return
	case fetch.tokenErr != nil:
		entry.Availability, entry.Reason = repositoryMonitorEvidenceUnavailable, repositoryMonitorEvidenceReasonCredentialMissing
		return
	case fetch.annotationChecks >= repositoryMonitorCIEvidenceMaxAnnotationChecks:
		entry.Truncated = append(entry.Truncated, repositoryMonitorEvidenceTruncatedFieldAnnots)
		entry.Reason = repositoryMonitorEvidenceReasonAnnotationLimit
		return
	}
	fetch.annotationChecks++
	var annotations []repositoryMonitorCheckAnnotationResponse
	for page := 1; page <= repositoryMonitorCIEvidenceAnnotationPages; page++ {
		endpoint := fmt.Sprintf("%s/repos/%s/%s/check-runs/%d/annotations?per_page=%d&page=%d", fetch.baseURL, url.PathEscape(fetch.owner), url.PathEscape(fetch.repo), checkRunID, repositoryMonitorCIEvidenceAnnotationsPerPage, page)
		var batch []repositoryMonitorCheckAnnotationResponse
		if err := r.fetchRepositoryMonitorAuthorizedJSON(ctx, endpoint, fetch.token, &batch); err != nil {
			availability, reason := repositoryMonitorEvidenceErrorAvailability(err)
			if availability == repositoryMonitorEvidenceRateLimited {
				fetch.rateLimited = true
			}
			entry.Availability, entry.Reason = availability, reason
			break
		}
		annotations = append(annotations, batch...)
		if len(batch) < repositoryMonitorCIEvidenceAnnotationsPerPage || len(annotations) >= entry.AnnotationCount {
			break
		}
	}
	slices.SortStableFunc(annotations, func(a, b repositoryMonitorCheckAnnotationResponse) int {
		return repositoryMonitorAnnotationLevelRank(a.AnnotationLevel) - repositoryMonitorAnnotationLevelRank(b.AnnotationLevel)
	})
	textCut := false
	for _, annotation := range annotations {
		if len(entry.Annotations) == repositoryMonitorCIEvidenceMaxAnnotations {
			break
		}
		item := repositoryMonitorCheckAnnotationEntry{StartLine: max(annotation.StartLine, 0), EndLine: max(annotation.EndLine, 0)}
		var cut bool
		item.Path, cut = repositoryMonitorEvidenceLine(annotation.Path, repositoryMonitorCIEvidencePathBytes)
		textCut = textCut || cut
		if fetch.text {
			item.Title, cut = repositoryMonitorEvidenceLine(annotation.Title, repositoryMonitorCIEvidenceAnnotationTitleBytes)
			textCut = textCut || cut
			item.Message, cut = repositoryMonitorEvidenceText(annotation.Message, repositoryMonitorCIEvidenceAnnotationBytes, repositoryMonitorCIEvidenceAnnotationLines)
			textCut = textCut || cut
		}
		if level := strings.ToLower(strings.TrimSpace(annotation.AnnotationLevel)); repositoryMonitorAnnotationLevelRank(level) < 3 {
			item.Level = level
		}
		entry.Annotations = append(entry.Annotations, item)
	}
	if textCut {
		entry.Truncated = append(entry.Truncated, repositoryMonitorEvidenceTruncatedFieldAnnotText)
	}
	if len(entry.Annotations) < entry.AnnotationCount && entry.Availability == repositoryMonitorEvidenceAvailable {
		entry.Truncated = append(entry.Truncated, repositoryMonitorEvidenceTruncatedFieldAnnots)
	}
}

func repositoryMonitorAnnotationLevelRank(level string) int {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "failure":
		return 0
	case "warning":
		return 1
	case "notice":
		return 2
	default:
		return 3
	}
}

func fitRepositoryMonitorCheckEvidence(entry repositoryMonitorCheckEvidence, maxBytes int) repositoryMonitorCheckEvidence {
	markTruncated := func(field string) {
		if !slices.Contains(entry.Truncated, field) {
			entry.Truncated = append(entry.Truncated, field)
		}
		if entry.Availability == repositoryMonitorEvidenceAvailable {
			entry.Availability = repositoryMonitorEvidenceTruncated
		}
	}
	for len(entry.Annotations) > 0 && repositoryMonitorEvidenceEncodedSize(entry) > maxBytes {
		entry.Annotations = entry.Annotations[:len(entry.Annotations)-1]
		markTruncated(repositoryMonitorEvidenceTruncatedFieldAnnots)
	}
	if entry.Summary != "" && repositoryMonitorEvidenceEncodedSize(entry) > maxBytes {
		entry.Summary = ""
		markTruncated(repositoryMonitorEvidenceTruncatedFieldSummary)
	}
	if entry.Title != "" && repositoryMonitorEvidenceEncodedSize(entry) > maxBytes {
		entry.Title = ""
		markTruncated(repositoryMonitorEvidenceTruncatedFieldTitle)
	}
	if entry.URL != "" && repositoryMonitorEvidenceEncodedSize(entry) > maxBytes {
		entry.URL = ""
		markTruncated(repositoryMonitorEvidenceTruncatedFieldURL)
	}
	for _, field := range []struct {
		value *string
		name  string
	}{
		{&entry.Description, repositoryMonitorEvidenceTruncatedFieldDesc},
		{&entry.Name, repositoryMonitorEvidenceTruncatedFieldName},
	} {
		if *field.value == "" || repositoryMonitorEvidenceEncodedSize(entry) <= maxBytes {
			continue
		}
		markTruncated(field.name)
		full := *field.value
		lo, hi := -1, len(full)-1
		for lo < hi {
			mid := lo + (hi-lo+1)/2
			*field.value = repositoryMonitorEvidenceCutRunes(full, mid)
			if repositoryMonitorEvidenceEncodedSize(entry) <= maxBytes {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		*field.value = ""
		if lo >= 0 {
			*field.value = repositoryMonitorEvidenceCutRunes(full, lo)
		}
	}
	return entry
}

func repositoryMonitorEvidenceEncodedSize(value any) int {
	encoded, err := json.MarshalIndent(value, repositoryMonitorCIEvidenceEntryPrefix, repositoryMonitorCIEvidenceEntryIndent)
	if err != nil {
		return repositoryMonitorCIEvidenceTotalEncodedBytes + 1
	}
	return len(encoded)
}

func repositoryMonitorEvidenceErrorAvailability(err error) (string, string) {
	var apiErr *repositoryMonitorGitHubAPIError
	if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusTooManyRequests ||
		apiErr.StatusCode == http.StatusForbidden && repositoryMonitorGitHubErrorLooksRateLimited(apiErr.Body)) {
		return repositoryMonitorEvidenceRateLimited, repositoryMonitorReviewContextErrorClass(err)
	}
	if errors.Is(err, errRepositoryMonitorCIIncomplete) {
		return repositoryMonitorEvidenceUnavailable, repositoryMonitorEvidenceReasonIncomplete
	}
	return repositoryMonitorEvidenceUnavailable, repositoryMonitorReviewContextErrorClass(err)
}

func repositoryMonitorEvidenceToken(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) > 64 {
		return ""
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_' && r != '.' {
			return ""
		}
	}
	return value
}

func repositoryMonitorEvidenceURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 4*repositoryMonitorCIEvidenceURLBytes {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != urlSchemeHTTPS && parsed.Scheme != urlSchemeHTTP) || parsed.Host == "" || parsed.User != nil || parsed.Opaque != "" {
		return ""
	}
	parsed.RawQuery, parsed.ForceQuery, parsed.Fragment, parsed.RawFragment = "", false, "", ""
	out := parsed.String()
	if len(out) > repositoryMonitorCIEvidenceURLBytes || strings.ContainsFunc(out, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.IsSpace(r) }) ||
		redact.SensitiveText(out) != out || security.LooksLikeSecret(out) {
		return ""
	}
	return out
}

var repositoryMonitorEvidenceGitHubCheckPathPattern = regexp.MustCompile(`^/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/(?:runs/[0-9]{1,20}|actions/runs/[0-9]{1,20}(?:/attempts/[0-9]{1,6})?(?:/job/[0-9]{1,20})?)$`)

func repositoryMonitorEvidenceGitHubCheckURL(raw, owner, repo string) string {
	out := repositoryMonitorEvidenceURL(raw)
	if out == "" || owner == "" || repo == "" {
		return ""
	}
	parsed, err := url.Parse(out)
	if err != nil || parsed.Scheme != urlSchemeHTTPS || !strings.EqualFold(parsed.Host, "github.com") || parsed.RawPath != "" {
		return ""
	}
	match := repositoryMonitorEvidenceGitHubCheckPathPattern.FindStringSubmatch(parsed.Path)
	if match == nil || !strings.EqualFold(match[1], owner) || !strings.EqualFold(match[2], repo) {
		return ""
	}
	return "https://github.com" + parsed.Path
}

var repositoryMonitorANSIEscapePattern = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)?|[PX^_][^\x1b]*(?:\x1b\\)?|[ -/]*[0-~])`)

func repositoryMonitorEvidenceSanitizedLines(value string, rawLimit int) ([]string, bool) {
	cut := false
	if len(value) > rawLimit {
		value = repositoryMonitorEvidenceCutRaw(value, rawLimit)
		cut = true
	}
	value = strings.ToValidUTF8(value, "")
	value = repositoryMonitorANSIEscapePattern.ReplaceAllString(value, "")
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	original := strings.Split(repositoryMonitorEvidenceStripRunes(value), "\n")
	lines := strings.Split(repositoryMonitorReviewContextSanitize(value), "\n")
	if len(lines) != len(original) {
		return []string{repositoryMonitorEvidenceRedactedLine}, true
	}
	repositoryMonitorEvidenceWithholdJoinedSecrets(original, lines)
	return lines, cut
}

func repositoryMonitorEvidenceStripRunes(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, value)
}

func repositoryMonitorEvidenceCutRaw(value string, limit int) string {
	value = value[:limit]
	if i := strings.LastIndexAny(value, "\r\n"); i >= 0 {
		return value[:i]
	}
	if i := strings.LastIndexFunc(value, unicode.IsSpace); i >= 0 {
		return value[:i]
	}
	return ""
}

func repositoryMonitorEvidenceWithholdJoinedSecrets(original, lines []string) {
	for i, line := range lines {
		if repositoryMonitorEvidenceSecretShaped(line) {
			lines[i] = repositoryMonitorEvidenceRedactedLine
		}
	}
	repositoryMonitorEvidenceWithholdPEMBlocks(original, lines)
	repositoryMonitorEvidenceWithholdMultilineValues(original, lines)
	words, escaped := repositoryMonitorEvidenceLineWords(original)
	chainEnd := make([]int, len(original))
	for i := len(original) - 1; i >= 0; i-- {
		chainEnd[i] = i
		if i+1 == len(original) || len(words[i]) == 0 || len(words[i+1]) == 0 {
			continue
		}
		if !escaped[i] && (!repositoryMonitorEvidenceTokenEdge(words[i][len(words[i])-1], false) || !repositoryMonitorEvidenceTokenEdge(words[i+1][0], true)) {
			continue
		}
		chainEnd[i] = i + 1
		if len(words[i+1]) == 1 {
			chainEnd[i] = chainEnd[i+1]
		}
	}
	nextKept := make([]int, len(lines)+1)
	nextKept[len(lines)] = len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		nextKept[i] = i
		if lines[i] == repositoryMonitorEvidenceRedactedLine {
			nextKept[i] = nextKept[i+1]
		}
	}
	spent, withheldThrough := 0, -1
	for i := range original {
		if chainEnd[i] == i || nextKept[i] > chainEnd[i] || withheldThrough >= chainEnd[i] {
			continue
		}
		withhold := spent > repositoryMonitorEvidenceSecretCheckBudget || repositoryMonitorEvidenceOpensHeaderValue(original[i])
		if !withhold {
			var continued strings.Builder
			for j := i + 1; j <= chainEnd[i] && continued.Len() < repositoryMonitorEvidenceSecretJoinBytes; j++ {
				continued.WriteString(words[j][0])
			}
			last := words[i][len(words[i])-1]
			spent += len(last) + continued.Len()
			withhold = spent > repositoryMonitorEvidenceSecretCheckBudget ||
				repositoryMonitorEvidenceContinuesSecret(last, strings.Join(words[i], " "), continued.String())
		}
		if withhold {
			for k := max(i, withheldThrough+1); k <= chainEnd[i]; k++ {
				lines[k] = repositoryMonitorEvidenceRedactedLine
			}
			withheldThrough = chainEnd[i]
		}
	}
}

func repositoryMonitorEvidenceContinuesSecret(last, line, continued string) bool {
	if repositoryMonitorEvidenceSecretShaped(last + continued) {
		return true
	}
	joined := line + continued
	if redacted := redact.SensitiveText(joined); redacted != joined && redacted != redact.SensitiveText(line)+redact.SensitiveText(continued) {
		return true
	}
	return security.LooksLikeSecret(joined) && !security.LooksLikeSecret(line) && !security.LooksLikeSecret(continued)
}

const (
	repositoryMonitorEvidencePEMBegin = "-----" + "begin "
	repositoryMonitorEvidencePEMEnd   = "-----" + "end "
)

func repositoryMonitorEvidenceWithholdPEMBlocks(original, lines []string) {
	inBlock, begun := false, false
	for i, line := range original {
		lower := strings.ToLower(line)
		withhold := inBlock
		for pos := 0; ; {
			if inBlock {
				k := strings.Index(lower[pos:], repositoryMonitorEvidencePEMEnd)
				if k < 0 {
					break
				}
				inBlock, pos = false, pos+k+len(repositoryMonitorEvidencePEMEnd)
				continue
			}
			begin := strings.Index(lower[pos:], repositoryMonitorEvidencePEMBegin)
			if !begun {
				if end := strings.Index(lower[pos:], repositoryMonitorEvidencePEMEnd); end >= 0 && (begin < 0 || end < begin) {
					for k := range i {
						lines[k] = repositoryMonitorEvidenceRedactedLine
					}
					withhold, begun = true, true
				}
			}
			if begin < 0 {
				break
			}
			inBlock, begun, withhold, pos = true, true, true, pos+begin+len(repositoryMonitorEvidencePEMBegin)
		}
		if withhold {
			lines[i] = repositoryMonitorEvidenceRedactedLine
		}
	}
}

func repositoryMonitorEvidenceWithholdMultilineValues(original, lines []string) {
	tracker := newRepositoryMonitorEvidenceMultilineTracker()
	for i, line := range original {
		if tracker.withhold(line) || repositoryMonitorEvidenceCredentialLine(line) {
			lines[i] = repositoryMonitorEvidenceRedactedLine
		}
	}
}

func repositoryMonitorEvidenceIndent(line string) int {
	return len(line) - len(strings.TrimLeft(line, " \t"))
}

var repositoryMonitorEvidenceCredentialHeaderPattern = regexp.MustCompile(`(?i)\b(?:authorization|txn-token|cookie)\s*:`)

func repositoryMonitorEvidenceOpensHeaderValue(line string) bool {
	matches := repositoryMonitorEvidenceCredentialHeaderPattern.FindAllStringIndex(line, -1)
	if len(matches) == 0 {
		return false
	}
	rest := strings.Trim(line[matches[len(matches)-1][1]:], " \t\\")
	rest = strings.TrimLeft(rest, `"' `+"\t")
	return !strings.ContainsAny(rest, `"'`) && len(strings.Fields(rest)) <= 1
}

func repositoryMonitorEvidenceLineWords(lines []string) ([][]string, []bool) {
	words := make([][]string, len(lines))
	escaped := make([]bool, len(lines))
	for i, line := range lines {
		fields := strings.Fields(strings.ReplaceAll(line, "\t", ""))
		if n := len(fields); n > 0 && strings.HasSuffix(fields[n-1], "\\") {
			escaped[i] = true
			if last := strings.TrimRight(fields[n-1], "\\"); last != "" {
				fields[n-1] = last
			} else {
				fields = fields[:n-1]
			}
		}
		words[i] = fields
	}
	return words, escaped
}

func repositoryMonitorEvidenceTokenEdge(value string, first bool) bool {
	if value == "" {
		return false
	}
	var r rune
	if first {
		r, _ = utf8.DecodeRuneInString(value)
	} else {
		r, _ = utf8.DecodeLastRuneInString(value)
	}
	return r < utf8.RuneSelf && (r == '_' || r == '-' || r == '.' || r == '=' || r == '/' || r == '+' || r == '~' || r == ':' ||
		r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
}

func repositoryMonitorEvidenceSecretShaped(value string) bool {
	if strings.TrimSpace(value) == "" || value == repositoryMonitorEvidenceRedactedLine {
		return false
	}
	return security.LooksLikeSecret(value) || redact.SensitiveText(value) != value
}

func repositoryMonitorEvidenceRawLimit(maxBytes int) int {
	return max(repositoryMonitorEvidenceRawTextFactor*maxBytes, repositoryMonitorEvidenceMinRawTextLimit)
}

func repositoryMonitorEvidenceText(value string, maxBytes, maxLines int) (string, bool) {
	lines, cut := repositoryMonitorEvidenceSanitizedLines(value, repositoryMonitorEvidenceRawLimit(maxBytes))
	text, bounded := repositoryMonitorEvidenceBoundHead(lines, maxBytes, maxLines)
	return text, cut || bounded
}

func repositoryMonitorEvidenceLine(value string, maxBytes int) (string, bool) {
	lines, cut := repositoryMonitorEvidenceSanitizedLines(value, repositoryMonitorEvidenceRawLimit(maxBytes))
	line := strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
	if len(line) <= maxBytes {
		return line, cut
	}
	return repositoryMonitorEvidenceCutRunes(line, maxBytes), true
}

func repositoryMonitorEvidenceBoundHead(lines []string, maxBytes, maxLines int) (string, bool) {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	kept, size := 0, 0
	for _, line := range lines {
		next := size + len(line)
		if kept > 0 {
			next++
		}
		if kept == maxLines || next > maxBytes {
			break
		}
		kept, size = kept+1, next
	}
	if kept == 0 && len(lines) > 0 {
		return repositoryMonitorEvidenceCutRunes(lines[0], maxBytes), true
	}
	return strings.Join(lines[:kept], "\n"), kept < len(lines)
}

func repositoryMonitorEvidenceCutRunes(value string, maxBytes int) string {
	const ellipsis = "…"
	if len(value) <= maxBytes {
		return value
	}
	limit := max(maxBytes-len(ellipsis), 0)
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit] + ellipsis
}

func repositoryMonitorRepairCIInputFromPrompt(prompt, headSHA string) (repositoryMonitorRepairCIInput, bool) {
	_, payload, ok := strings.Cut(prompt, "\n\nInput:\n")
	if !ok {
		return repositoryMonitorRepairCIInput{}, false
	}
	var decoded struct {
		repositoryMonitorRepairCIInput
		HeadSHA string `json:"headSHA"`
	}
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil || decoded.HeadSHA != headSHA ||
		decoded.Evidence.SchemaVersion != repositoryMonitorCIEvidenceSchemaVersion || decoded.Evidence.HeadSHA != headSHA {
		return repositoryMonitorRepairCIInput{}, false
	}
	return decoded.repositoryMonitorRepairCIInput, true
}
