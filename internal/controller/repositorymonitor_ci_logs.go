package controller

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/security"

	"github.com/orka-agents/orka/internal/taskwebhook"
)

// GitHub Actions log excerpts are an opt-in part of CI evidence
// (spec.repair.ciEvidence.logs). The job log is a redirect to a signed blob
// URL: the API request carries the GitHub token and follows no redirect, and
// the blob request carries no credentials, follows no redirect, and dials
// only public addresses. The signed URL is never logged or returned.
const (
	repositoryMonitorCILogAppSlug = "github-actions"

	repositoryMonitorCILogMaxChecks      = 3
	repositoryMonitorCILogReadLimit      = 8 << 20
	repositoryMonitorCILogRawLineLimit   = 4 << 10
	repositoryMonitorCILogLineBytes      = 320
	repositoryMonitorCILogExcerptLines   = 40
	repositoryMonitorCILogExcerptBytes   = 3 << 10
	repositoryMonitorCILogTotalBytes     = 8 << 10
	repositoryMonitorCILogLinesAfter     = 6
	repositoryMonitorCILogSecretMargin   = 8
	repositoryMonitorCILogFetchTimeout   = 10 * time.Second
	repositoryMonitorCILogBudget         = 20 * time.Second
	repositoryMonitorCILogStepNameBytes  = 200
	repositoryMonitorCILogErrorMarker    = "##[error]"
	repositoryMonitorCILogGroupMarker    = "##[group]"
	repositoryMonitorCILogEndGroupMarker = "##[endgroup]"

	// Excerpt kinds. A truncated read never claims its last error is the
	// job's last error.
	repositoryMonitorCILogExcerptLastError        = "last_error"
	repositoryMonitorCILogExcerptErrorInReadPart  = "error_in_read_portion"
	repositoryMonitorCILogExcerptTail             = "tail"
	repositoryMonitorCILogExcerptTailOfReadPart   = "tail_of_read_portion"
	repositoryMonitorCILogReasonCheckLimit        = "log_check_limit"
	repositoryMonitorCILogReasonTotalBudget       = "log_budget_exhausted"
	repositoryMonitorCILogReasonNoRedirect        = "log_redirect_missing"
	repositoryMonitorCILogReasonBadRedirect       = "log_redirect_invalid"
	repositoryMonitorCILogReasonRedirectRefused   = "log_redirect_refused"
	repositoryMonitorCILogReasonAddressNotAllowed = "log_destination_not_allowed"
	repositoryMonitorCILogReasonUnresolved        = "log_destination_unresolved"
	repositoryMonitorCILogReasonEmpty             = "log_empty"
	repositoryMonitorCILogReasonHeadMismatch      = "job_head_sha_mismatch"
)

// repositoryMonitorCheckLogEvidence is a bounded excerpt of one failed
// GitHub Actions job's log.
type repositoryMonitorCheckLogEvidence struct {
	Availability string `json:"availability"`
	Reason       string `json:"reason,omitempty"`
	// FailedStep names the first job step GitHub reports as failed.
	FailedStep string `json:"failedStep,omitempty"`
	// ExcerptKind says where Excerpt comes from: the window around the last
	// ##[error] line, or the log's last lines when it has none. A read that
	// stopped at the size or time limit reports the *_read_portion kinds.
	ExcerptKind string `json:"excerptKind,omitempty"`
	Excerpt     string `json:"excerpt,omitempty"`
	// ReadTruncated means only the first part of the log was read.
	ReadTruncated bool `json:"readTruncated,omitempty"`
}

var repositoryMonitorCILogTimestampPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2}) ?`)

// repositoryMonitorCILogCleanLine strips a log line's ANSI escapes and
// timestamp, and reports false for a line that only closes or opens a group
// marker without text. A line that progress output rewrote with carriage
// returns keeps only the text after its last one, as a terminal renders it,
// so no carriage return reaches the sanitizer to split the line.
func repositoryMonitorCILogCleanLine(line string) (string, bool) {
	line = repositoryMonitorANSIEscapePattern.ReplaceAllString(line, "")
	line = strings.TrimRight(line, "\r")
	if i := strings.LastIndexByte(line, '\r'); i >= 0 {
		line = line[i+1:]
	}
	line = strings.TrimPrefix(line, "\ufeff")
	line = repositoryMonitorCILogTimestampPattern.ReplaceAllString(line, "")
	if strings.HasPrefix(line, repositoryMonitorCILogEndGroupMarker) {
		return "", false
	}
	if rest, ok := strings.CutPrefix(line, repositoryMonitorCILogGroupMarker); ok {
		if strings.TrimSpace(rest) == "" {
			return "", false
		}
		line = rest
	}
	return line, true
}

func repositoryMonitorCILogCleanupLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	return strings.HasPrefix(trimmed, "Post job cleanup.") || strings.HasPrefix(trimmed, "Cleaning up orphan processes") ||
		strings.HasPrefix(trimmed, "Post ") && strings.Contains(trimmed, "Run ")
}

// repositoryMonitorCILogExcerpt is the raw excerpt selected from a log read.
type repositoryMonitorCILogExcerpt struct {
	lines []string
	// errorAt is the index of the ##[error] line in lines, or -1.
	errorAt   int
	truncated bool
	// openStart and openEnd mean log lines that were not kept precede or
	// follow lines.
	openStart, openEnd bool
}

// repositoryMonitorCILogScanner reads a job log line by line, keeping only a
// rolling window: the lines before and after the last ##[error] line before
// post-job cleanup, and the last lines before cleanup started.
type repositoryMonitorCILogScanner struct {
	seen   int
	recent []string
	// errorSeen counts the lines seen when errorWindow was taken, tailSeen
	// when cleanup started.
	errorSeen, tailSeen int
	// errorClosed means post-job cleanup, not an unread line, follows the
	// error window.
	errorClosed bool
	errorWindow []string
	errorAt     int
	errorAfter  int
	cleanup     bool
	tail        []string
	// yamlIndent is the indentation of the credential YAML block scalar
	// header whose body the scan is in, or -1 outside one.
	yamlIndent int
}

func newRepositoryMonitorCILogScanner() *repositoryMonitorCILogScanner {
	return &repositoryMonitorCILogScanner{errorAt: -1, yamlIndent: -1}
}

// withholdYAMLBlock withholds the body of a credential YAML block scalar as
// it streams by, so pruning the rolling window cannot drop the header that
// identifies it while its body lines survive.
func (s *repositoryMonitorCILogScanner) withholdYAMLBlock(line string) string {
	normalized := repositoryMonitorEvidenceStripRunes(strings.ToValidUTF8(line, ""))
	if s.yamlIndent >= 0 {
		if strings.TrimSpace(normalized) == "" {
			return line
		}
		if repositoryMonitorEvidenceIndent(normalized) > s.yamlIndent {
			return repositoryMonitorEvidenceRedactedLine
		}
		s.yamlIndent = -1
	}
	if security.SensitiveYAMLBlockHeader(normalized) {
		s.yamlIndent = repositoryMonitorEvidenceIndent(normalized)
		return repositoryMonitorEvidenceRedactedLine
	}
	return line
}

func (s *repositoryMonitorCILogScanner) windowBefore() int {
	return repositoryMonitorCILogExcerptLines + repositoryMonitorCILogSecretMargin
}

func (s *repositoryMonitorCILogScanner) add(line string) {
	line, keep := repositoryMonitorCILogCleanLine(line)
	if !keep {
		return
	}
	line = s.withholdYAMLBlock(line)
	if !s.cleanup && repositoryMonitorCILogCleanupLine(line) {
		s.cleanup = true
		s.tail, s.tailSeen = append([]string(nil), s.recent...), s.seen
		// The error window ends at the fixed cleanup marker.
		if s.errorAfter > 0 {
			s.errorAfter, s.errorClosed = 0, true
		}
	}
	s.seen++
	if s.errorAfter > 0 {
		s.errorWindow = append(s.errorWindow, line)
		s.errorAfter--
	}
	s.recent = append(s.recent, line)
	if len(s.recent) > s.windowBefore() {
		s.recent = s.recent[len(s.recent)-s.windowBefore():]
	}
	if strings.Contains(line, repositoryMonitorCILogErrorMarker) {
		if s.cleanup {
			// An error in post-job cleanup, such as a failed cache save,
			// is not why the job failed.
			return
		}
		s.errorWindow, s.errorAt, s.errorSeen = append([]string(nil), s.recent...), len(s.recent)-1, s.seen
		s.errorAfter = repositoryMonitorCILogLinesAfter + repositoryMonitorCILogSecretMargin
	}
}

func (s *repositoryMonitorCILogScanner) excerpt(truncated bool) (repositoryMonitorCILogExcerpt, string) {
	errorKind := repositoryMonitorCILogExcerptLastError
	tailKind := repositoryMonitorCILogExcerptTail
	if truncated {
		errorKind, tailKind = repositoryMonitorCILogExcerptErrorInReadPart, repositoryMonitorCILogExcerptTailOfReadPart
	}
	if s.errorAt >= 0 {
		// The window ends where its trailing lines stopped being collected.
		end := s.errorSeen + len(s.errorWindow) - s.errorAt - 1
		return repositoryMonitorCILogExcerpt{
			lines: s.errorWindow, errorAt: s.errorAt, truncated: truncated,
			openStart: s.errorSeen > s.errorAt+1, openEnd: truncated || !s.errorClosed && end < s.seen,
		}, errorKind
	}
	tail, seen := s.recent, s.seen
	if s.cleanup {
		// Lines after cleanup started are post-job boilerplate; the line
		// that follows the tail is the fixed cleanup marker.
		tail, seen = s.tail, s.tailSeen
	}
	return repositoryMonitorCILogExcerpt{
		lines: tail, errorAt: -1, truncated: truncated,
		openStart: seen > len(tail), openEnd: truncated && !s.cleanup,
	}, tailKind
}

// readRepositoryMonitorCILog scans at most limit bytes of a log body. A line
// longer than repositoryMonitorCILogRawLineLimit keeps only its head, cut at
// the last space so no fragment of a credential survives the cut, and a
// final line the limit or a read error cut short is dropped for the same
// reason. The second result reports that the read stopped before the end of
// the log.
func readRepositoryMonitorCILog(body io.Reader, limit int64) (*repositoryMonitorCILogScanner, bool, error) {
	limited := &io.LimitedReader{R: body, N: limit}
	reader := bufio.NewReaderSize(limited, 64<<10)
	scanner := newRepositoryMonitorCILogScanner()
	line := make([]byte, 0, 1024)
	overlong := false
	for {
		chunk, err := reader.ReadSlice('\n')
		if !overlong {
			room := repositoryMonitorCILogRawLineLimit - len(line)
			if len(chunk) > room {
				line, overlong = append(line, chunk[:max(room, 0)]...), true
			} else {
				line = append(line, chunk...)
			}
		}
		switch {
		case err == nil:
			text := strings.TrimSuffix(string(line), "\n")
			if overlong {
				text = repositoryMonitorEvidenceCutRaw(text, len(text)) + repositoryMonitorCILogCutMarker
			}
			scanner.add(text)
			line, overlong = line[:0], false
		case errors.Is(err, bufio.ErrBufferFull):
		case errors.Is(err, io.EOF):
			// A body that ends without a final newline is complete only
			// when the limit did not stop it.
			if limited.N > 0 {
				if len(line) > 0 {
					text := string(line)
					if overlong {
						text = repositoryMonitorEvidenceCutRaw(text, len(text)) + repositoryMonitorCILogCutMarker
					}
					scanner.add(text)
				}
				return scanner, false, nil
			}
			return scanner, !repositoryMonitorCILogAtEOF(body), nil
		default:
			return scanner, true, err
		}
	}
}

// repositoryMonitorCILogAtEOF reports whether body has nothing left after
// the read limit was reached. Only an EOF says so: a probe that fails
// otherwise, such as on a timeout or a reset connection, leaves the read
// truncated.
func repositoryMonitorCILogAtEOF(body io.Reader) bool {
	var probe [1]byte
	n, err := io.ReadFull(body, probe[:])
	return n == 0 && errors.Is(err, io.EOF)
}

// repositoryMonitorCILogExcerptText sanitizes the selected lines together
// with their margins, so a credential wrapped across the excerpt's edge is
// still recognized, then keeps at most repositoryMonitorCILogExcerptLines
// lines and repositoryMonitorCILogExcerptBytes bytes ending just after the
// error line.
func repositoryMonitorCILogExcerptText(excerpt repositoryMonitorCILogExcerpt) string {
	if len(excerpt.lines) == 0 {
		// Splitting the empty join would yield one line, not none.
		return ""
	}
	raw := strings.Join(excerpt.lines, "\n")
	lines, _ := repositoryMonitorEvidenceSanitizedLines(raw, len(raw))
	if len(lines) != len(excerpt.lines) {
		return repositoryMonitorEvidenceRedactedLine
	}
	repositoryMonitorCILogWithholdOpenEdges(excerpt.lines, lines, excerpt.openStart, excerpt.openEnd)
	end := len(lines)
	if excerpt.errorAt >= 0 {
		end = min(len(lines), excerpt.errorAt+1+repositoryMonitorCILogLinesAfter)
	}
	lines = lines[:end]
	for i, line := range lines {
		lines[i] = repositoryMonitorEvidenceCutRunes(line, repositoryMonitorCILogLineBytes)
	}
	return repositoryMonitorEvidenceBoundTail(lines, repositoryMonitorCILogExcerptBytes, repositoryMonitorCILogExcerptLines)
}

// repositoryMonitorCILogContinuesCut reports whether a line, given its words
// and whether it ends in an escaped line break, could continue a credential
// whose start a raw-line cut dropped: it begins with a credential character,
// or it is only an escaped line break.
func repositoryMonitorCILogContinuesCut(words []string, escaped bool) bool {
	if len(words) == 0 {
		return escaped
	}
	return repositoryMonitorEvidenceTokenEdge(words[0], true)
}

// repositoryMonitorCILogCutMarker ends a line that was longer than
// repositoryMonitorCILogRawLineLimit and kept only its head.
const repositoryMonitorCILogCutMarker = " …"

// repositoryMonitorCILogWithholdOpenEdges withholds the lines at either edge
// of the retained log that could belong to a credential wrapped from or into
// a line that was not retained: the first line, which an escaped line break
// on the unseen line before it could continue a credential into whatever its
// first character, and the lines it continues into; a last line ending with
// a credential character or in an escaped line break, and the lines leading
// into it. Continuation follows the sanitizer's chains: a credential
// continues past a line only when the line is a single word, or only a
// backslash, and that line ends in an escaped line break or continues
// through credential characters into the next. Only an edge with unkept
// lines beyond it is withheld.
func repositoryMonitorCILogWithholdOpenEdges(original, lines []string, openStart, openEnd bool) {
	// Analyze the text the sanitizer keeps: a zero-width or other format
	// rune it strips must not hide a credential character or an escaped
	// line break at a boundary.
	normalized := make([]string, len(original))
	for i, line := range original {
		normalized[i] = repositoryMonitorEvidenceStripRunes(strings.ToValidUTF8(line, ""))
	}
	original = normalized
	words, escaped := repositoryMonitorEvidenceLineWords(original)
	// passes reports that a credential can run through all of line i.
	passes := func(i int) bool { return len(words[i]) == 1 || len(words[i]) == 0 && escaped[i] }
	// continues reports that a credential ending line i continues into line
	// i+1.
	continues := func(i int) bool {
		return escaped[i] || len(words[i]) > 0 && len(words[i+1]) > 0 &&
			repositoryMonitorEvidenceTokenEdge(words[i][len(words[i])-1], false) && repositoryMonitorEvidenceTokenEdge(words[i+1][0], true)
	}
	n := len(words)
	// withholdFrom withholds line start, which a credential from an unseen
	// line could continue into, and the lines it continues into.
	withholdFrom := func(start int) {
		for i := start; i < n; i++ {
			lines[i] = repositoryMonitorEvidenceRedactedLine
			if i+1 == n || !passes(i) || !continues(i) {
				break
			}
		}
	}
	if openStart && n > 0 && (len(words[0]) > 0 || escaped[0]) {
		withholdFrom(0)
	}
	// A line cut at the raw line limit lost its tail, which may have held a
	// credential's recognizable start, so a wrapped body on the next lines
	// can no longer be matched. The line after the cut is withheld when it
	// begins with a credential character, with the lines it continues into;
	// a line such as an ##[error] marker cannot continue one and is kept.
	for i := 0; i+1 < n; i++ {
		next := i + 1
		if strings.HasSuffix(original[i], repositoryMonitorCILogCutMarker) && repositoryMonitorCILogContinuesCut(words[next], escaped[next]) {
			withholdFrom(next)
		}
	}
	if openEnd && n > 0 && (escaped[n-1] || len(words[n-1]) > 0 && repositoryMonitorEvidenceTokenEdge(words[n-1][len(words[n-1])-1], false)) {
		for i := n - 1; i >= 0; i-- {
			lines[i] = repositoryMonitorEvidenceRedactedLine
			if i == 0 || !passes(i) || !continues(i-1) {
				break
			}
		}
	}
}

// repositoryMonitorEvidenceBoundTail keeps whole trailing lines within
// maxBytes and maxLines.
func repositoryMonitorEvidenceBoundTail(lines []string, maxBytes, maxLines int) string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	start, size := len(lines), 0
	for start > 0 {
		next := size + len(lines[start-1])
		if start < len(lines) {
			next++
		}
		if len(lines)-start == maxLines || next > maxBytes {
			break
		}
		start, size = start-1, next
	}
	for start < len(lines) && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	return strings.Join(lines[start:], "\n")
}

type repositoryMonitorActionsJobResponse struct {
	ID      int64  `json:"id"`
	HeadSHA string `json:"head_sha"`
	Steps   []struct {
		Name       string `json:"name"`
		Number     int    `json:"number"`
		Conclusion string `json:"conclusion"`
	} `json:"steps"`
}

// repositoryMonitorAddCILogs attaches log excerpts to the first eligible
// GitHub Actions check runs in evidence, within the log limits.
func (r *RepositoryMonitorReconciler) repositoryMonitorAddCILogs(ctx context.Context, fetch *repositoryMonitorEvidenceFetch, evidence *repositoryMonitorCIEvidence, headSHA string) {
	ctx, cancel := context.WithTimeout(ctx, repositoryMonitorCILogBudget)
	defer cancel()
	attempted, used := 0, 0
	for i := range evidence.Checks {
		entry := &evidence.Checks[i]
		if entry.Kind != repositoryMonitorEvidenceKindCheckRun || entry.App != repositoryMonitorCILogAppSlug {
			continue
		}
		if entry.Availability == repositoryMonitorEvidenceStale {
			continue
		}
		jobID, err := strconv.ParseInt(strings.TrimPrefix(entry.ID, repositoryMonitorEvidenceKindCheckRun+":"), 10, 64)
		if err != nil || jobID <= 0 {
			continue
		}
		switch {
		case attempted == repositoryMonitorCILogMaxChecks:
			entry.Log = &repositoryMonitorCheckLogEvidence{Availability: repositoryMonitorEvidenceUnavailable, Reason: repositoryMonitorCILogReasonCheckLimit}
			continue
		case used >= repositoryMonitorCILogTotalBytes:
			entry.Log = &repositoryMonitorCheckLogEvidence{Availability: repositoryMonitorEvidenceUnavailable, Reason: repositoryMonitorCILogReasonTotalBudget}
			continue
		case fetch.rateLimited:
			entry.Log = &repositoryMonitorCheckLogEvidence{Availability: repositoryMonitorEvidenceUnavailable, Reason: repositoryMonitorEvidenceReasonAfterRateLimit}
			continue
		case fetch.tokenErr != nil:
			entry.Log = &repositoryMonitorCheckLogEvidence{Availability: repositoryMonitorEvidenceUnavailable, Reason: repositoryMonitorEvidenceReasonCredentialMissing}
			continue
		}
		attempted++
		logEvidence := r.repositoryMonitorCILogFor(ctx, fetch, jobID, headSHA)
		if size := len(logEvidence.Excerpt); size > 0 {
			if used+size > repositoryMonitorCILogTotalBytes {
				logEvidence.Excerpt = repositoryMonitorEvidenceBoundTail(strings.Split(logEvidence.Excerpt, "\n"), repositoryMonitorCILogTotalBytes-used, repositoryMonitorCILogExcerptLines)
				if logEvidence.Availability == repositoryMonitorEvidenceAvailable {
					logEvidence.Availability = repositoryMonitorEvidenceTruncated
				}
				if logEvidence.Reason == "" {
					logEvidence.Reason = repositoryMonitorCILogReasonTotalBudget
				}
			}
			used += len(logEvidence.Excerpt)
		}
		entry.Log = &logEvidence
	}
}

// repositoryMonitorCILogFor reads one job's metadata and log excerpt.
func (r *RepositoryMonitorReconciler) repositoryMonitorCILogFor(ctx context.Context, fetch *repositoryMonitorEvidenceFetch, jobID int64, headSHA string) repositoryMonitorCheckLogEvidence {
	failed := func(err error) repositoryMonitorCheckLogEvidence {
		availability, reason := repositoryMonitorEvidenceErrorAvailability(err)
		if availability == repositoryMonitorEvidenceRateLimited {
			fetch.rateLimited = true
		}
		return repositoryMonitorCheckLogEvidence{Availability: availability, Reason: reason}
	}
	jobCtx, cancel := context.WithTimeout(ctx, repositoryMonitorCILogFetchTimeout)
	defer cancel()
	jobEndpoint := fmt.Sprintf("%s/repos/%s/%s/actions/jobs/%d", fetch.baseURL, url.PathEscape(fetch.owner), url.PathEscape(fetch.repo), jobID)
	var job repositoryMonitorActionsJobResponse
	if err := r.fetchRepositoryMonitorAuthorizedJSON(jobCtx, jobEndpoint, fetch.token, &job); err != nil {
		return failed(err)
	}
	if !strings.EqualFold(strings.TrimSpace(job.HeadSHA), headSHA) {
		return repositoryMonitorCheckLogEvidence{Availability: repositoryMonitorEvidenceStale, Reason: repositoryMonitorCILogReasonHeadMismatch}
	}
	evidence := repositoryMonitorCheckLogEvidence{Availability: repositoryMonitorEvidenceAvailable}
	for _, step := range job.Steps {
		if strings.EqualFold(step.Conclusion, "failure") {
			evidence.FailedStep, _ = repositoryMonitorEvidenceLine(step.Name, repositoryMonitorCILogStepNameBytes)
			break
		}
	}
	scanner, truncated, reason, err := r.readRepositoryMonitorCILogBlob(ctx, fetch, jobID)
	if err != nil {
		failure := failed(err)
		failure.FailedStep = evidence.FailedStep
		return failure
	}
	if reason != "" {
		evidence.Availability, evidence.Reason = repositoryMonitorEvidenceUnavailable, reason
		return evidence
	}
	excerpt, kind := scanner.excerpt(truncated)
	evidence.ExcerptKind, evidence.ReadTruncated = kind, truncated
	evidence.Excerpt = repositoryMonitorCILogExcerptText(excerpt)
	if evidence.Excerpt == "" {
		evidence.Availability, evidence.Reason, evidence.ExcerptKind = repositoryMonitorEvidenceUnavailable, repositoryMonitorCILogReasonEmpty, ""
	} else if truncated {
		evidence.Availability = repositoryMonitorEvidenceTruncated
	}
	return evidence
}

// readRepositoryMonitorCILogBlob follows the log redirect once by hand. It
// returns a reason, not an error, for a refused redirect or destination, and
// never includes the signed blob URL in what it returns.
func (r *RepositoryMonitorReconciler) readRepositoryMonitorCILogBlob(ctx context.Context, fetch *repositoryMonitorEvidenceFetch, jobID int64) (*repositoryMonitorCILogScanner, bool, string, error) {
	apiCtx, cancel := context.WithTimeout(ctx, repositoryMonitorCILogFetchTimeout)
	defer cancel()
	endpoint := fmt.Sprintf("%s/repos/%s/%s/actions/jobs/%d/logs", fetch.baseURL, url.PathEscape(fetch.owner), url.PathEscape(fetch.repo), jobID)
	req, err := http.NewRequestWithContext(apiCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, false, repositoryMonitorReviewContextErrorRequestFailed, nil
	}
	req.Header.Set("Authorization", strings.Join([]string{bearerAuthScheme, fetch.token}, " "))
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	apiClient := &http.Client{Timeout: repositoryMonitorCILogFetchTimeout}
	if r.HTTPClient != nil {
		copied := *r.HTTPClient
		apiClient = &copied
	}
	apiClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := apiClient.Do(req)
	if err != nil {
		return nil, false, repositoryMonitorCILogTransportReason(err), nil
	}
	location := resp.Header.Get("Location")
	status := resp.StatusCode
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if status != http.StatusFound && status != http.StatusMovedPermanently && status != http.StatusSeeOther && status != http.StatusTemporaryRedirect {
		if status >= 200 && status < 300 {
			return nil, false, repositoryMonitorCILogReasonNoRedirect, nil
		}
		return nil, false, "", &repositoryMonitorGitHubAPIError{Operation: "actions job logs", StatusCode: status, Body: string(body)}
	}
	target, err := url.Parse(location)
	if err != nil || target.Scheme != urlSchemeHTTPS || target.Host == "" || target.User != nil || target.Opaque != "" {
		return nil, false, repositoryMonitorCILogReasonBadRedirect, nil
	}
	blobCtx, cancelBlob := context.WithTimeout(ctx, repositoryMonitorCILogFetchTimeout)
	defer cancelBlob()
	blobReq, err := http.NewRequestWithContext(blobCtx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, false, repositoryMonitorCILogReasonBadRedirect, nil
	}
	blobResp, err := r.repositoryMonitorCILogBlobClient().Do(blobReq)
	if err != nil {
		return nil, false, repositoryMonitorCILogTransportReason(err), nil
	}
	defer blobResp.Body.Close() //nolint:errcheck
	if blobResp.StatusCode < 200 || blobResp.StatusCode >= 300 {
		if blobResp.StatusCode >= 300 && blobResp.StatusCode < 400 {
			return nil, false, repositoryMonitorCILogReasonRedirectRefused, nil
		}
		return nil, false, fmt.Sprintf("log_blob_status_%d", blobResp.StatusCode), nil
	}
	scanner, truncated, err := readRepositoryMonitorCILog(blobResp.Body, repositoryMonitorCILogReadLimit)
	if err != nil {
		// A read that failed midway keeps what it read, marked truncated.
		return scanner, true, "", nil
	}
	return scanner, truncated, "", nil
}

// repositoryMonitorCILogBlobClient returns the client for signed log blob
// URLs.
func (r *RepositoryMonitorReconciler) repositoryMonitorCILogBlobClient() *http.Client {
	if r.ciLogBlobClient != nil {
		return r.ciLogBlobClient
	}
	return newRepositoryMonitorCILogBlobClient(taskwebhook.PublicAddressPolicy, nil)
}

// newRepositoryMonitorCILogBlobClient builds a client with no proxy, no
// redirects, and no connection reuse, whose dialer connects only to
// addresses policy permits, checked again on the connecting socket.
func newRepositoryMonitorCILogBlobClient(policy taskwebhook.AddressPolicy, tlsConfig *tls.Config) *http.Client {
	client := taskwebhook.NewHTTPClient(&taskwebhook.Dialer{Policy: policy, Timeout: repositoryMonitorCILogFetchTimeout}, repositoryMonitorCILogFetchTimeout)
	if transport, ok := client.Transport.(*http.Transport); ok && tlsConfig != nil {
		transport.TLSClientConfig = tlsConfig
	}
	return client
}

// repositoryMonitorCILogTransportReason classifies a transport failure
// without its text: url.Error carries the request URL, which for the blob
// request is the signed URL.
func repositoryMonitorCILogTransportReason(err error) string {
	var netErr net.Error
	switch {
	case errors.Is(err, taskwebhook.ErrRedirectRefused):
		return repositoryMonitorCILogReasonRedirectRefused
	case errors.Is(err, taskwebhook.ErrAddressNotAllowed):
		return repositoryMonitorCILogReasonAddressNotAllowed
	case errors.Is(err, taskwebhook.ErrResolution):
		return repositoryMonitorCILogReasonUnresolved
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return repositoryMonitorReviewContextErrorTimeout
	default:
		return repositoryMonitorReviewContextErrorNetwork
	}
}
