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

// CI evidence gives a CI repair the failing checks' own diagnostics: check
// output, annotations, and commit status descriptions for the exact head. It
// is untrusted CI output, sanitized and bounded before it enters the repair
// prompt, and a failed lookup degrades to the check names plus a reason.
const (
	repositoryMonitorCIEvidenceSchemaVersion = "orka.prRepair.ciEvidence.v1"

	// Availability of one evidence entry or of the evidence as a whole.
	// available: every field is complete. truncated: a field was bounded,
	// named in truncated. unavailable: a lookup failed, see reason.
	// rate_limited: GitHub rate limited the lookup. unsupported: the source
	// has no such detail (commit statuses have no output or annotations).
	// stale: GitHub reported the check for another commit.
	repositoryMonitorEvidenceAvailable   = "available"
	repositoryMonitorEvidenceTruncated   = "truncated"
	repositoryMonitorEvidenceUnavailable = "unavailable"
	repositoryMonitorEvidenceRateLimited = "rate_limited"
	repositoryMonitorEvidenceUnsupported = "unsupported"
	repositoryMonitorEvidenceStale       = "stale"

	repositoryMonitorEvidenceKindCheckRun     = "check_run"
	repositoryMonitorEvidenceKindCommitStatus = "commit_status"

	// Reasons that are not a GitHub error class.
	repositoryMonitorEvidenceReasonIncomplete         = "ci_checks_incomplete"
	repositoryMonitorEvidenceReasonAnnotationLimit    = "annotation_fetch_limit"
	repositoryMonitorEvidenceReasonAfterRateLimit     = "skipped_after_rate_limit"
	repositoryMonitorEvidenceReasonEvidenceBudget     = "evidence_budget_exhausted"
	repositoryMonitorEvidenceReasonHeadMismatch       = "check_head_sha_mismatch"
	repositoryMonitorEvidenceReasonCredentialMissing  = "credential_unavailable"
	repositoryMonitorEvidenceReasonGraphQLRateLimited = "graphql_rate_limited"

	// Evidence bounds. Text limits are UTF-8 bytes after sanitizing; encoded
	// limits are bytes of the entry's JSON encoding.
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
	repositoryMonitorEvidenceMinRawTextLimit         = 2 << 10
	repositoryMonitorEvidenceRawTextFactor           = 4
	repositoryMonitorEvidenceSecretCheckBudget       = 32 << 10
	repositoryMonitorEvidenceSecretJoinBytes         = 2 << 10
	repositoryMonitorEvidenceRedactedLine            = "[REDACTED]"
	repositoryMonitorEvidenceTruncatedFieldSummary   = "summary"
	repositoryMonitorEvidenceTruncatedFieldTitle     = "title"
	repositoryMonitorEvidenceTruncatedFieldName      = "name"
	repositoryMonitorEvidenceTruncatedFieldDesc      = "description"
	repositoryMonitorEvidenceTruncatedFieldAnnots    = "annotations"
	repositoryMonitorEvidenceTruncatedFieldAnnotText = "annotationText"

	// Work action and monitor event metadata keys recording the CI evidence
	// one repair attempt received.
	repositoryMonitorCIEvidenceDigestMetadataKey       = "ciEvidenceDigest"
	repositoryMonitorCIEvidenceIDsMetadataKey          = "ciEvidenceIDs"
	repositoryMonitorCIEvidenceAvailabilityMetadataKey = "ciEvidenceAvailability"
)

// repositoryMonitorCIEvidence is the normalized evidence for the failing
// checks a pull request head introduced.
type repositoryMonitorCIEvidence struct {
	SchemaVersion string `json:"schemaVersion"`
	Repo          string `json:"repo"`
	HeadSHA       string `json:"headSHA"`
	BaseSHA       string `json:"baseSHA,omitempty"`
	Availability  string `json:"availability"`
	Reason        string `json:"reason,omitempty"`
	// ChecksIncomplete means GitHub returned fewer head checks or statuses
	// than it counted, so checks may not list every failure.
	ChecksIncomplete     bool                                 `json:"checksIncomplete,omitempty"`
	Checks               []repositoryMonitorCheckEvidence     `json:"checks"`
	OmittedChecks        int                                  `json:"omittedChecks,omitempty"`
	PendingChecks        []repositoryMonitorPendingCheckEntry `json:"pendingChecks,omitempty"`
	OmittedPendingChecks int                                  `json:"omittedPendingChecks,omitempty"`
}

// repositoryMonitorCheckEvidence is one failing check run or commit status.
type repositoryMonitorCheckEvidence struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	App         string `json:"app,omitempty"`
	Status      string `json:"status,omitempty"`
	Conclusion  string `json:"conclusion,omitempty"`
	URL         string `json:"url,omitempty"`
	Title       string `json:"title,omitempty"`
	Summary     string `json:"summary,omitempty"`
	Description string `json:"description,omitempty"`
	// AnnotationCount is the number of annotations GitHub reported.
	AnnotationCount int                                     `json:"annotationCount,omitempty"`
	Annotations     []repositoryMonitorCheckAnnotationEntry `json:"annotations,omitempty"`
	Availability    string                                  `json:"availability"`
	Reason          string                                  `json:"reason,omitempty"`
	Truncated       []string                                `json:"truncated,omitempty"`
}

// repositoryMonitorCheckAnnotationEntry is one bounded check run annotation.
type repositoryMonitorCheckAnnotationEntry struct {
	Path      string `json:"path,omitempty"`
	StartLine int    `json:"startLine,omitempty"`
	EndLine   int    `json:"endLine,omitempty"`
	Level     string `json:"level,omitempty"`
	Title     string `json:"title,omitempty"`
	Message   string `json:"message"`
}

// repositoryMonitorPendingCheckEntry is a head check run that had not
// completed when the evidence was read.
type repositoryMonitorPendingCheckEntry struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	App    string `json:"app,omitempty"`
	Status string `json:"status,omitempty"`
}

// repositoryMonitorRepairCIInput is the CI part of an orka.prRepair.input.v1
// payload. A repair Task's prompt freezes it: rebuilding the prompt for an
// existing Task reuses the input parsed back from that prompt.
type repositoryMonitorRepairCIInput struct {
	BaseSHA           string                      `json:"baseSHA,omitempty"`
	FailingChecks     []string                    `json:"failingChecks,omitempty"`
	BaseFailingChecks []string                    `json:"baseFailingChecks,omitempty"`
	Evidence          repositoryMonitorCIEvidence `json:"failingCheckEvidence"`
}

// digest identifies the exact CI input bytes a repair attempt received.
func (in repositoryMonitorRepairCIInput) digest() string {
	encoded, err := json.Marshal(in)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// evidenceIDs lists the GitHub check and status IDs the input describes.
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

// repositoryMonitorCheckRunResponse is one GitHub check run.
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

// buildRepositoryMonitorRepairCIInput reads the CI input of a fix_ci repair.
// It never fails: a failed lookup yields evidence that names the reason.
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
	// Name the checks to fix so the repairer leaves checks that also fail
	// on the base alone.
	input.BaseSHA = pr.BaseSHA
	input.FailingChecks = boundRepositoryMonitorCheckNames(repositoryMonitorCIFailureNames(failures.introduced, failures.inherited))
	if len(failures.inherited) > 0 {
		input.BaseFailingChecks = boundRepositoryMonitorCheckNames(repositoryMonitorCIFailureNames(failures.inherited, failures.introduced))
	}
	input.Evidence = r.repositoryMonitorCIEvidenceFor(ctx, monitor, repo, pr, failures)
	return input
}

// repositoryMonitorEvidenceFetch tracks one evidence build's GitHub reads.
type repositoryMonitorEvidenceFetch struct {
	baseURL, owner, repo, token string
	tokenErr                    error
	rateLimited                 bool
	annotationChecks            int
}

// repositoryMonitorCIEvidenceFor normalizes the introduced failures of one
// head. Annotation reads share one time budget and stop at the first rate
// limit; each entry records what it could not include.
func (r *RepositoryMonitorReconciler) repositoryMonitorCIEvidenceFor(ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, repo string, pr repositoryMonitorPullRequest, failures repositoryMonitorCIFailures) repositoryMonitorCIEvidence {
	evidence := newRepositoryMonitorCIEvidence(repo, pr)
	evidence.ChecksIncomplete = failures.headIncomplete
	ctx, cancel := context.WithTimeout(ctx, repositoryMonitorCIEvidenceFetchBudget)
	defer cancel()
	fetch := &repositoryMonitorEvidenceFetch{}
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
		if used+size > repositoryMonitorCIEvidenceTotalEncodedBytes {
			evidence.OmittedChecks++
			continue
		}
		used += size
		evidence.Checks = append(evidence.Checks, entry)
	}
	for i, pending := range failures.pending {
		if i == repositoryMonitorCIEvidenceMaxPendingChecks {
			evidence.OmittedPendingChecks = len(failures.pending) - i
			break
		}
		name, _ := repositoryMonitorEvidenceLine(pending.name, repositoryMonitorCIEvidenceNameBytes)
		evidence.PendingChecks = append(evidence.PendingChecks, repositoryMonitorPendingCheckEntry{
			ID:     repositoryMonitorEvidenceID(pending),
			Name:   name,
			App:    repositoryMonitorEvidenceToken(pending.appSlug),
			Status: repositoryMonitorEvidenceToken(pending.status),
		})
	}
	if evidence.OmittedChecks > 0 || evidence.ChecksIncomplete {
		evidence.Availability = repositoryMonitorEvidenceTruncated
		if evidence.OmittedChecks > 0 {
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
		entry.URL = repositoryMonitorEvidenceURL(failure.url)
		if entry.Description, cut = repositoryMonitorEvidenceLine(failure.description, repositoryMonitorCIEvidenceDescriptionBytes); cut {
			entry.Truncated = append(entry.Truncated, repositoryMonitorEvidenceTruncatedFieldDesc)
		}
		// A commit status carries only a description and a target URL.
		entry.Availability = repositoryMonitorEvidenceUnsupported
		return entry
	}
	entry.App = repositoryMonitorEvidenceToken(failure.appSlug)
	entry.Status = repositoryMonitorEvidenceToken(failure.status)
	if sha := strings.TrimSpace(failure.headSHA); sha != "" && !strings.EqualFold(sha, headSHA) {
		// Output from another commit is not evidence about this head.
		entry.Availability, entry.Reason = repositoryMonitorEvidenceStale, repositoryMonitorEvidenceReasonHeadMismatch
		return entry
	}
	entry.URL = repositoryMonitorEvidenceURL(failure.url)
	if entry.Title, cut = repositoryMonitorEvidenceLine(failure.title, repositoryMonitorCIEvidenceTitleBytes); cut {
		entry.Truncated = append(entry.Truncated, repositoryMonitorEvidenceTruncatedFieldTitle)
	}
	if entry.Summary, cut = repositoryMonitorEvidenceText(failure.summary, repositoryMonitorCIEvidenceSummaryBytes, repositoryMonitorCIEvidenceSummaryLines); cut {
		entry.Truncated = append(entry.Truncated, repositoryMonitorEvidenceTruncatedFieldSummary)
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

// repositoryMonitorAddCheckAnnotations reads a bounded number of a check
// run's annotation pages and keeps the most severe annotations.
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
			// Annotations from earlier pages stay useful; the reason says
			// the rest could not be read.
			entry.Availability, entry.Reason = availability, reason
			break
		}
		annotations = append(annotations, batch...)
		if len(batch) < repositoryMonitorCIEvidenceAnnotationsPerPage || len(annotations) >= entry.AnnotationCount {
			break
		}
	}
	// GitHub lists annotations in report order; keep failures first.
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
		item.Title, cut = repositoryMonitorEvidenceLine(annotation.Title, repositoryMonitorCIEvidenceAnnotationTitleBytes)
		textCut = textCut || cut
		item.Message, cut = repositoryMonitorEvidenceText(annotation.Message, repositoryMonitorCIEvidenceAnnotationBytes, repositoryMonitorCIEvidenceAnnotationLines)
		textCut = textCut || cut
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

// fitRepositoryMonitorCheckEvidence keeps one entry within maxBytes of
// encoded JSON, dropping annotations from the least severe end first, then
// the summary and title.
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
	return entry
}

func repositoryMonitorEvidenceEncodedSize(value any) int {
	encoded, err := json.Marshal(value)
	if err != nil {
		return repositoryMonitorCIEvidenceTotalEncodedBytes + 1
	}
	return len(encoded)
}

// repositoryMonitorEvidenceErrorAvailability classifies a failed GitHub read
// without its error text, which can carry URLs or response bodies.
func repositoryMonitorEvidenceErrorAvailability(err error) (string, string) {
	var apiErr *repositoryMonitorGitHubAPIError
	if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusTooManyRequests ||
		apiErr.StatusCode == http.StatusForbidden && repositoryMonitorGitHubErrorLooksRateLimited(apiErr.Body)) {
		return repositoryMonitorEvidenceRateLimited, repositoryMonitorReviewContextErrorClass(err)
	}
	if graphQLErr, ok := errors.AsType[*repositoryMonitorGraphQLError](err); ok && graphQLErr.rateLimited() {
		return repositoryMonitorEvidenceRateLimited, repositoryMonitorEvidenceReasonGraphQLRateLimited
	}
	if errors.Is(err, errRepositoryMonitorCIIncomplete) {
		return repositoryMonitorEvidenceUnavailable, repositoryMonitorEvidenceReasonIncomplete
	}
	return repositoryMonitorEvidenceUnavailable, repositoryMonitorReviewContextErrorClass(err)
}

// repositoryMonitorEvidenceToken keeps a short GitHub enum or slug such as
// a check status, conclusion, or app slug, and drops anything else.
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

// repositoryMonitorEvidenceURL keeps an http(s) link without credentials,
// query, or fragment, since signed URLs carry their signature in the query.
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

// repositoryMonitorANSIEscapePattern matches ANSI CSI, OSC, and other escape
// sequences, which CI tools emit for color and cursor control.
var repositoryMonitorANSIEscapePattern = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)?|[PX^_][^\x1b]*(?:\x1b\\)?|[ -/]*[0-~])`)

// repositoryMonitorEvidenceSanitizedLines cleans untrusted CI text into
// lines: ANSI escapes are stripped first, then control and format runes;
// credentials are redacted per line, and lines that only look like a
// credential once joined are withheld. Raw text past rawLimit bytes is
// dropped at a line or word boundary first, so no partial credential is kept
// from the cut; the second result reports that cut.
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
	// Redaction can replace part of a wrapped credential, which would hide
	// the rest from a joined check; join the stripped, unredacted lines.
	original := strings.Split(repositoryMonitorEvidenceStripRunes(value), "\n")
	lines := strings.Split(repositoryMonitorReviewContextSanitize(value), "\n")
	if len(lines) != len(original) {
		// Redaction never joins or splits lines; if it did, keep nothing.
		return []string{repositoryMonitorEvidenceRedactedLine}, true
	}
	repositoryMonitorEvidenceWithholdJoinedSecrets(original, lines)
	return lines, cut
}

// repositoryMonitorEvidenceStripRunes drops the control and format runes
// repositoryMonitorReviewContextSanitize drops, keeping newlines and tabs.
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

// repositoryMonitorEvidenceCutRaw keeps at most limit bytes of raw text,
// ending at the last line break, or the last space of a single long line.
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

// repositoryMonitorEvidenceWithholdJoinedSecrets withholds each sanitized
// line that still looks like a credential, and the lines a credential
// wrapped across line breaks spans, such as a JWT or token split over lines.
// A wrapped credential continues from the last word of one line into the
// first word of the next, through any number of lines that are a single
// word: such a run of lines is a continuation chain. A trailing backslash
// always continues a chain. From each line, up to
// repositoryMonitorEvidenceSecretJoinBytes of the chain's joined words are
// checked, within repositoryMonitorEvidenceSecretCheckBudget bytes in all,
// and a match withholds that line through the end of its chain, so
// no fragment of the credential stays behind. The original, unredacted lines
// are joined: redaction can replace the head of a wrapped credential and
// hide its tail from the check. A credential directly followed by a word on
// the next line cannot be told from a longer one, so that line is withheld
// too.
func repositoryMonitorEvidenceWithholdJoinedSecrets(original, lines []string) {
	for i, line := range lines {
		if repositoryMonitorEvidenceSecretShaped(line) {
			lines[i] = repositoryMonitorEvidenceRedactedLine
		}
	}
	words, escaped := repositoryMonitorEvidenceLineWords(original)
	// chainEnd[i] is the last line the chain from line i's last word reaches.
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
	spent := 0
	for i := range original {
		if chainEnd[i] == i {
			continue
		}
		var joined strings.Builder
		joined.WriteString(words[i][len(words[i])-1])
		for j := i + 1; j <= chainEnd[i] && joined.Len() < repositoryMonitorEvidenceSecretJoinBytes; j++ {
			joined.WriteString(words[j][0])
		}
		spent += joined.Len()
		// Text built to defeat the check by splitting into many tiny lines
		// would make it quadratic; past the budget, chains are withheld
		// unchecked.
		if spent > repositoryMonitorEvidenceSecretCheckBudget || repositoryMonitorEvidenceSecretShaped(joined.String()) {
			for k := i; k <= chainEnd[i]; k++ {
				lines[k] = repositoryMonitorEvidenceRedactedLine
			}
		}
	}
}

// repositoryMonitorEvidenceLineWords splits lines into words. A line ending
// in an escaped line break (a trailing backslash) continues into the next
// line whatever the characters around the break: the backslash is dropped
// from its words and escaped reports the continuation.
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

// repositoryMonitorEvidenceTokenEdge reports whether value starts (first) or
// ends with a character a credential can continue through.
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

// repositoryMonitorEvidenceRawLimit is how much raw text a field kept to
// maxBytes is sanitized from: enough that redaction rarely shortens it below
// maxBytes, while bounding the sanitizer's work.
func repositoryMonitorEvidenceRawLimit(maxBytes int) int {
	return max(repositoryMonitorEvidenceRawTextFactor*maxBytes, repositoryMonitorEvidenceMinRawTextLimit)
}

// repositoryMonitorEvidenceText sanitizes untrusted multi-line CI text, then
// keeps whole leading lines within maxBytes and maxLines. A single first line
// longer than maxBytes is cut at a rune boundary. The second result reports
// that text was dropped.
func repositoryMonitorEvidenceText(value string, maxBytes, maxLines int) (string, bool) {
	lines, cut := repositoryMonitorEvidenceSanitizedLines(value, repositoryMonitorEvidenceRawLimit(maxBytes))
	text, bounded := repositoryMonitorEvidenceBoundHead(lines, maxBytes, maxLines)
	return text, cut || bounded
}

// repositoryMonitorEvidenceLine sanitizes untrusted CI text into one line of
// at most maxBytes.
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

// repositoryMonitorEvidenceCutRunes cuts value to at most maxBytes without
// splitting a rune, marking the cut with an ellipsis.
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

// repositoryMonitorRepairCIInputFromPrompt recovers the frozen CI input from
// an existing repair Task's prompt. It reports false when the prompt has no
// CI input for headSHA.
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
