package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/orka-agents/orka/internal/redact"
	"github.com/orka-agents/orka/internal/security"
)

const (
	repositoryMonitorReviewContextSchemaVersion    = "orka.prReview.context.v1"
	repositoryMonitorReviewContextMaxFiles         = 100
	repositoryMonitorReviewContextMaxFileEntries   = 2000
	repositoryMonitorReviewContextMaxBytes         = 700 << 10
	repositoryMonitorReviewContextMaxPatchBytes    = 64 << 10
	repositoryMonitorReviewDeltaMaxBytes           = 192 << 10
	repositoryMonitorReviewContextMaxPathBytes     = 512
	repositoryMonitorReviewContextMaxStatusBytes   = 32
	repositoryMonitorReviewContextPatchTruncated   = "truncated"
	repositoryMonitorReviewContextStatusRemoved    = "removed"
	repositoryMonitorReviewContextStatusRenamed    = "renamed"
	repositoryMonitorReviewContextStatusModified   = "modified"
	repositoryMonitorReviewContextStatusChanged    = "changed"
	repositoryMonitorReviewContextStatusAdded      = "added"
	repositoryMonitorReviewContextStatusCopied     = "copied"
	repositoryMonitorReviewContextPatchUnavailable = "unavailable"
	repositoryMonitorReviewContextPatchCapped      = "capped"
	repositoryMonitorReviewContextBeginMarker      = "--- BEGIN orka.prReview.context.v1 ---"
	repositoryMonitorReviewContextEndMarker        = "--- END orka.prReview.context.v1 ---"
	repositoryMonitorReviewDeltaBeginMarker        = "--- BEGIN orka.prReview.followUpDelta ---"
	repositoryMonitorReviewDeltaEndMarker          = "--- END orka.prReview.followUpDelta ---"

	repositoryMonitorReviewContextErrorGitHubStatus   = "github_api_status_"
	repositoryMonitorReviewContextErrorTimeout        = "timeout"
	repositoryMonitorReviewContextErrorNetwork        = "network_error"
	repositoryMonitorReviewContextErrorInvalidPayload = "invalid_response"
	repositoryMonitorReviewContextErrorRequestFailed  = "request_failed"

	repositoryMonitorReviewContextArrayItemPrefix = "    "
	repositoryMonitorReviewContextIndent          = "  "
)

type repositoryMonitorReviewContextFile struct {
	Path         string `json:"path"`
	PreviousPath string `json:"previousPath,omitempty"`
	Status       string `json:"status"`
	Additions    int    `json:"additions"`
	Deletions    int    `json:"deletions"`
	Patch        string `json:"patch,omitempty"`
	PatchOmitted string `json:"patchOmitted,omitempty"`
}

type repositoryMonitorReviewContextTruncation struct {
	Files bool `json:"files"`
	Bytes bool `json:"bytes"`
}

type repositoryMonitorReviewContext struct {
	SchemaVersion      string                                   `json:"schemaVersion"`
	Repo               string                                   `json:"repo"`
	PRNumber           int64                                    `json:"prNumber"`
	BaseSHA            string                                   `json:"baseSHA"`
	HeadSHA            string                                   `json:"headSHA"`
	ChangedFileCount   int                                      `json:"changedFileCount"`
	Files              []repositoryMonitorReviewContextFile     `json:"files"`
	Truncated          repositoryMonitorReviewContextTruncation `json:"truncated"`
	ContextUnavailable string                                   `json:"contextUnavailable,omitempty"`
}

type repositoryMonitorReviewContextDriftError struct {
	Number int64
	Field  string
	Want   string
	Got    string
}

func (e *repositoryMonitorReviewContextDriftError) Error() string {
	return fmt.Sprintf("pull request #%d %s changed during review context assembly: want %q, got %q", e.Number, e.Field, e.Want, e.Got)
}

func newRepositoryMonitorReviewContext(owner, repository string, pr repositoryMonitorPullRequest) repositoryMonitorReviewContext {
	return repositoryMonitorReviewContext{
		SchemaVersion: repositoryMonitorReviewContextSchemaVersion,
		Repo:          owner + "/" + repository,
		PRNumber:      pr.Number,
		BaseSHA:       pr.BaseSHA,
		HeadSHA:       pr.HeadSHA,
		Files:         []repositoryMonitorReviewContextFile{},
	}
}

func (r *RepositoryMonitorReconciler) buildRepositoryMonitorReviewContext(ctx context.Context, owner, repository, token string, pr repositoryMonitorPullRequest) (repositoryMonitorReviewContext, error) {
	logger := log.FromContext(ctx).WithName("repositorymonitor")
	files, filesErr := r.listRepositoryMonitorCompareFiles(ctx, owner, repository, token, pr.BaseSHA, pr.HeadSHA)
	current, err := r.fetchRepositoryMonitorPullRequest(ctx, owner, repository, token, pr.Number)
	if err != nil {
		reviewContext := newRepositoryMonitorReviewContext(owner, repository, pr)
		reviewContext.ContextUnavailable = repositoryMonitorReviewContextErrorClass(err)
		logger.Info("pull request review context unavailable", "pr", pr.Number, "operation", "refetch_pull_request", "errorClass", reviewContext.ContextUnavailable)
		return reviewContext, nil
	}
	if driftErr := repositoryMonitorReviewContextDrift(pr, *current); driftErr != nil {
		return repositoryMonitorReviewContext{}, driftErr
	}
	if filesErr != nil {
		reviewContext := newRepositoryMonitorReviewContext(owner, repository, pr)
		reviewContext.ContextUnavailable = repositoryMonitorReviewContextErrorClass(filesErr)
		logger.Info("pull request review context unavailable", "pr", pr.Number, "operation", "list_files", "errorClass", reviewContext.ContextUnavailable)
		return reviewContext, nil
	}
	return repositoryMonitorReviewContextBindChangedFileCount(repositoryMonitorReviewContextFromFiles(owner, repository, pr, files), len(files), current.ChangedFiles), nil
}

func repositoryMonitorReviewContextBindChangedFileCount(reviewContext repositoryMonitorReviewContext, listed, total int) repositoryMonitorReviewContext {
	switch {
	case total > listed:
		reviewContext.ChangedFileCount = total
		reviewContext.Truncated.Files = true
	case total <= 0 && listed >= repositoryMonitorGitHubCompareMaxFiles:
		reviewContext.Truncated.Files = true
	}
	return reviewContext
}

func repositoryMonitorReviewContextDrift(expected, current repositoryMonitorPullRequest) error {
	checks := []struct {
		field     string
		want, got string
	}{
		{field: "head SHA", want: expected.HeadSHA, got: current.HeadSHA},
		{field: "base SHA", want: expected.BaseSHA, got: current.BaseSHA},
		{field: "head repository", want: expected.HeadRepo, got: current.HeadRepo},
	}
	for _, check := range checks {
		if strings.TrimSpace(check.want) != strings.TrimSpace(check.got) {
			return &repositoryMonitorReviewContextDriftError{Number: expected.Number, Field: check.field, Want: check.want, Got: check.got}
		}
	}
	return nil
}

func repositoryMonitorReviewContextErrorClass(err error) string {
	var apiErr *repositoryMonitorGitHubAPIError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &apiErr):
		return fmt.Sprintf("%s%d", repositoryMonitorReviewContextErrorGitHubStatus, apiErr.StatusCode)
	case errors.Is(err, context.DeadlineExceeded):
		return repositoryMonitorReviewContextErrorTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return repositoryMonitorReviewContextErrorTimeout
	}
	if _, ok := errors.AsType[*url.Error](err); ok {
		return repositoryMonitorReviewContextErrorNetwork
	}
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &syntaxErr) || errors.As(err, &typeErr) {
		return repositoryMonitorReviewContextErrorInvalidPayload
	}
	return repositoryMonitorReviewContextErrorRequestFailed
}

func repositoryMonitorReviewContextFromFiles(owner, repository string, pr repositoryMonitorPullRequest, files []repositoryMonitorPullRequestFileResponse) repositoryMonitorReviewContext {
	return repositoryMonitorReviewContextFromFilesWithin(owner, repository, pr, files, repositoryMonitorReviewContextMaxBytes)
}

func repositoryMonitorReviewContextFromFilesWithin(owner, repository string, pr repositoryMonitorPullRequest, files []repositoryMonitorPullRequestFileResponse, maxBytes int) repositoryMonitorReviewContext {
	reviewContext := newRepositoryMonitorReviewContext(owner, repository, pr)
	reviewContext.ChangedFileCount = len(files)
	if len(files) > repositoryMonitorReviewContextMaxFileEntries {
		files = files[:repositoryMonitorReviewContextMaxFileEntries]
		reviewContext.Truncated.Files = true
	}

	entries := make([]repositoryMonitorReviewContextFile, 0, len(files))
	used := repositoryMonitorReviewContextEncodedSize(reviewContext)
	for i, file := range files {
		if repositoryMonitorReviewContextPathAltered(file) || repositoryMonitorReviewContextDeletedLinesAltered(file.Patch) {
			reviewContext.Truncated.Files = true
		}
		entry := repositoryMonitorReviewContextIdentityEntry(file, i >= repositoryMonitorReviewContextMaxFiles)
		entrySize := repositoryMonitorReviewContextEntrySize(entry, len(entries))
		if used+entrySize > maxBytes {
			reviewContext.Truncated.Files = true
			reviewContext.Truncated.Bytes = true
			break
		}
		entries = append(entries, entry)
		used += entrySize
	}
	for i := range min(len(entries), repositoryMonitorReviewContextMaxFiles) {
		withPatch := repositoryMonitorReviewContextFileEntry(files[i])
		if withPatch.Patch == "" {
			continue
		}
		delta := repositoryMonitorReviewContextEntrySize(withPatch, i) - repositoryMonitorReviewContextEntrySize(entries[i], i)
		if used+delta > maxBytes {
			reviewContext.Truncated.Bytes = true
			break
		}
		entries[i] = withPatch
		used += delta
	}
	reviewContext.Files = entries
	return repositoryMonitorReviewContextWithin(reviewContext, maxBytes)
}

func repositoryMonitorReviewContextWithin(reviewContext repositoryMonitorReviewContext, maxBytes int) repositoryMonitorReviewContext {
	reviewContext = repositoryMonitorReviewContextTrimToBudget(reviewContext, maxBytes)
	return repositoryMonitorReviewContextMarkUnreviewableOmissions(reviewContext)
}

func repositoryMonitorReviewContextMarkUnreviewableOmissions(reviewContext repositoryMonitorReviewContext) repositoryMonitorReviewContext {
	for _, file := range reviewContext.Files {
		if file.PatchOmitted == "" {
			if file.Patch != "" && !patchMatchesLineCounts(file.Patch, file.Additions, file.Deletions) {
				reviewContext.Truncated.Files = true
				return reviewContext
			}
			continue
		}
		if (file.Status == repositoryMonitorReviewContextStatusAdded || file.Status == repositoryMonitorReviewContextStatusCopied) && file.Deletions == 0 {
			continue
		}
		reviewContext.Truncated.Files = true
		return reviewContext
	}
	return reviewContext
}

func repositoryMonitorReviewContextTrimToBudget(reviewContext repositoryMonitorReviewContext, maxBytes int) repositoryMonitorReviewContext {
	for repositoryMonitorReviewContextEncodedSize(reviewContext) > maxBytes && len(reviewContext.Files) > 0 {
		reviewContext.Truncated.Bytes = true
		if i := repositoryMonitorReviewContextLastPatchIndex(reviewContext.Files); i >= 0 {
			reviewContext.Files[i] = repositoryMonitorReviewContextDropPatch(reviewContext.Files[i])
			continue
		}
		reviewContext.Files = reviewContext.Files[:len(reviewContext.Files)-1]
		reviewContext.Truncated.Files = true
	}
	return reviewContext
}

func repositoryMonitorReviewContextLastPatchIndex(files []repositoryMonitorReviewContextFile) int {
	for i, file := range slices.Backward(files) {
		if file.Patch != "" {
			return i
		}
	}
	return -1
}

func repositoryMonitorReviewContextIdentityEntry(file repositoryMonitorPullRequestFileResponse, capped bool) repositoryMonitorReviewContextFile {
	if capped {
		entry := repositoryMonitorReviewContextFileIdentity(file)
		entry.PatchOmitted = repositoryMonitorReviewContextPatchCapped
		return entry
	}
	return repositoryMonitorReviewContextDropPatch(repositoryMonitorReviewContextFileEntry(file))
}

func repositoryMonitorReviewContextFileIdentity(file repositoryMonitorPullRequestFileResponse) repositoryMonitorReviewContextFile {
	return repositoryMonitorReviewContextFile{
		Path:         repositoryMonitorReviewContextBoundedField(file.Filename, repositoryMonitorReviewContextMaxPathBytes),
		PreviousPath: repositoryMonitorReviewContextBoundedField(file.PreviousFilename, repositoryMonitorReviewContextMaxPathBytes),
		Status:       repositoryMonitorReviewContextBoundedField(file.Status, repositoryMonitorReviewContextMaxStatusBytes),
		Additions:    max(file.Additions, 0),
		Deletions:    max(file.Deletions, 0),
	}
}

func repositoryMonitorReviewContextFileEntry(file repositoryMonitorPullRequestFileResponse) repositoryMonitorReviewContextFile {
	entry := repositoryMonitorReviewContextFileIdentity(file)
	patch := repositoryMonitorReviewContextSanitize(file.Patch)
	if patch == "" {
		entry.PatchOmitted = repositoryMonitorReviewContextPatchUnavailable
		return entry
	}
	entry.Patch, entry.PatchOmitted = repositoryMonitorReviewContextTruncatePatch(patch, repositoryMonitorReviewContextMaxPatchBytes)
	return entry
}

func repositoryMonitorReviewContextDropPatch(entry repositoryMonitorReviewContextFile) repositoryMonitorReviewContextFile {
	if entry.Patch == "" {
		return entry
	}
	entry.Patch = ""
	entry.PatchOmitted = repositoryMonitorReviewContextPatchTruncated
	return entry
}

func repositoryMonitorReviewContextTruncatePatch(patch string, maxEncodedBytes int) (string, string) {
	if repositoryMonitorReviewContextEncodedStringSize(patch) <= maxEncodedBytes {
		return patch, ""
	}
	candidate := patch
	if len(candidate) > maxEncodedBytes {
		candidate = candidate[:maxEncodedBytes]
	}
	for {
		cut := strings.LastIndexByte(candidate, '\n')
		if cut < 0 {
			return "", repositoryMonitorReviewContextPatchTruncated
		}
		candidate = candidate[:cut]
		if repositoryMonitorReviewContextEncodedStringSize(candidate) <= maxEncodedBytes {
			return candidate, repositoryMonitorReviewContextPatchTruncated
		}
	}
}

func repositoryMonitorReviewContextPathAltered(file repositoryMonitorPullRequestFileResponse) bool {
	for _, value := range []string{file.Filename, file.PreviousFilename} {
		if repositoryMonitorReviewContextBoundedField(value, repositoryMonitorReviewContextMaxPathBytes) != value {
			return true
		}
	}
	return false
}

func repositoryMonitorReviewContextBoundedField(value string, maxBytes int) string {
	shadow := strings.NewReplacer("\n", "", "\t", "").Replace(value)
	if sanitizedShadow := repositoryMonitorReviewContextSanitize(shadow); sanitizedShadow != shadow {
		value = sanitizedShadow
	} else {
		value = repositoryMonitorReviewContextSanitize(value)
	}
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(strings.ReplaceAll(value, "\n", " "), "\t", " ")
	if len(value) <= maxBytes {
		return value
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

func repositoryMonitorReviewContextDeletedLinesAltered(patch string) bool {
	if !strings.HasPrefix(patch, "-") && !strings.Contains(patch, "\n-") {
		return false
	}
	original := strings.Split(patch, "\n")
	sanitized := strings.Split(repositoryMonitorReviewContextSanitize(patch), "\n")
	if len(original) != len(sanitized) {
		return true
	}
	for i, line := range original {
		if strings.HasPrefix(line, "-") && line != sanitized[i] {
			return true
		}
	}
	return false
}

func patchMatchesLineCounts(patch string, additions, deletions int) bool {
	adds, dels := 0, 0
	inHunk := false
	for line := range strings.SplitSeq(patch, "\n") {
		if strings.HasPrefix(line, "@@") {
			inHunk = true
			continue
		}
		if !inHunk || len(line) == 0 {
			continue
		}
		switch line[0] {
		case '+':
			adds++
		case '-':
			dels++
		}
	}
	return adds == additions && dels == deletions
}

const repositoryMonitorReviewContextSecretWindowLines = 8

func repositoryMonitorReviewContextSanitize(value string) string {
	stripped := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(value, ""))
	lines := strings.Split(stripped, "\n")
	prefixes := make([]string, len(lines))
	shadows := make([]string, len(lines))
	withheld := make([]bool, len(lines))
	for i, line := range lines {
		if len(line) > 0 && (line[0] == '+' || line[0] == '-' || line[0] == ' ') {
			prefixes[i] = line[:1]
		}
		shadows[i] = strings.ReplaceAll(line[len(prefixes[i]):], "\t", "")
	}
	for i, line := range lines {
		shadow := shadows[i]
		if redacted := redact.SensitiveText(shadow); redacted != shadow {
			if strings.ContainsRune(line, '\t') {
				lines[i] = prefixes[i] + redacted
			} else {
				lines[i] = redact.SensitiveText(line)
			}
			continue
		}
		if security.LooksLikeSecret(shadow) {
			lines[i] = prefixes[i] + "[REDACTED]"
			withheld[i] = true
			continue
		}
		lines[i] = redact.SensitiveText(line)
	}
	for i := range lines {
		if withheld[i] || !repositoryMonitorReviewContextLineMayContinue(shadows[i]) {
			continue
		}
		var window strings.Builder
		window.WriteString(shadows[i])
		for j := i + 1; j < len(lines) && j < i+repositoryMonitorReviewContextSecretWindowLines; j++ {
			if withheld[j] {
				break
			}
			window.WriteString("\n" + shadows[j])
			if !security.LooksLikeSecret(window.String()) {
				continue
			}
			for k := i; k <= j; k++ {
				lines[k] = prefixes[k] + "[REDACTED]"
				withheld[k] = true
			}
			break
		}
	}
	return redact.SensitiveText(strings.Join(lines, "\n"))
}

func repositoryMonitorReviewContextLineMayContinue(shadow string) bool {
	trimmed := strings.TrimRight(shadow, " ")
	if trimmed == "" || !strings.ContainsAny(trimmed, ":=") {
		return false
	}
	if strings.HasSuffix(trimmed, "\\") || strings.HasSuffix(trimmed, ":") || strings.HasSuffix(trimmed, "=") {
		return true
	}
	for _, indicator := range []string{"|", ">", "|-", ">-", "|+", ">+"} {
		if strings.HasSuffix(trimmed, ": "+indicator) || strings.HasSuffix(trimmed, ":"+indicator) {
			return true
		}
	}
	return strings.Count(trimmed, "\"")%2 == 1 || strings.Count(trimmed, "'")%2 == 1
}

func repositoryMonitorReviewContextEncodedSize(reviewContext repositoryMonitorReviewContext) int {
	encoded, err := json.MarshalIndent(reviewContext, "", repositoryMonitorReviewContextIndent)
	if err != nil {
		return repositoryMonitorReviewContextMaxBytes + 1
	}
	return len(encoded)
}

func repositoryMonitorReviewContextEncodedStringSize(value string) int {
	encoded, err := json.Marshal(value)
	if err != nil {
		return len(value) + 2
	}
	return len(encoded)
}

func repositoryMonitorReviewContextEntrySize(entry repositoryMonitorReviewContextFile, existingEntries int) int {
	encoded, err := json.MarshalIndent(entry, repositoryMonitorReviewContextArrayItemPrefix, repositoryMonitorReviewContextIndent)
	if err != nil {
		return repositoryMonitorReviewContextMaxBytes + 1
	}
	overhead := len(",\n") + len(repositoryMonitorReviewContextArrayItemPrefix)
	if existingEntries == 0 {
		overhead = len("\n") + len(repositoryMonitorReviewContextArrayItemPrefix) + len("\n") + len(repositoryMonitorReviewContextIndent)
	}
	return len(encoded) + overhead
}

func renderRepositoryMonitorReviewContext(reviewContext repositoryMonitorReviewContext) string {
	if reviewContext.Files == nil {
		reviewContext.Files = []repositoryMonitorReviewContextFile{}
	}
	encoded, err := json.MarshalIndent(reviewContext, "", repositoryMonitorReviewContextIndent)
	if err != nil {
		fallback := newRepositoryMonitorReviewContext("", "", repositoryMonitorPullRequest{})
		fallback.Repo = reviewContext.Repo
		fallback.PRNumber = reviewContext.PRNumber
		fallback.BaseSHA = reviewContext.BaseSHA
		fallback.HeadSHA = reviewContext.HeadSHA
		fallback.ContextUnavailable = repositoryMonitorReviewContextErrorInvalidPayload
		encoded, _ = json.MarshalIndent(fallback, "", repositoryMonitorReviewContextIndent)
	}
	return repositoryMonitorReviewContextBeginMarker + "\n" + string(encoded) + "\n" + repositoryMonitorReviewContextEndMarker
}

func renderRepositoryMonitorReviewDelta(delta repositoryMonitorReviewContext) string {
	rendered := renderRepositoryMonitorReviewContext(delta)
	rendered = strings.TrimPrefix(rendered, repositoryMonitorReviewContextBeginMarker)
	rendered = strings.TrimSuffix(rendered, repositoryMonitorReviewContextEndMarker)
	return repositoryMonitorReviewDeltaBeginMarker + rendered + repositoryMonitorReviewDeltaEndMarker
}

func repositoryMonitorReviewPromptDelta(prompt string) (repositoryMonitorReviewContext, bool) {
	beginLine := "\n" + repositoryMonitorReviewDeltaBeginMarker + "\n"
	endLine := "\n" + repositoryMonitorReviewDeltaEndMarker
	_, block, found := strings.Cut(prompt, beginLine)
	if !found {
		return repositoryMonitorReviewContext{}, false
	}
	block, _, found = strings.Cut(block, endLine)
	if !found {
		return repositoryMonitorReviewContext{}, false
	}
	var parsed repositoryMonitorReviewContext
	if err := json.Unmarshal([]byte(block), &parsed); err != nil {
		return repositoryMonitorReviewContext{}, false
	}
	return parsed, true
}

func repositoryMonitorReviewPromptContextBounds(prompt string) (int, int, bool) {
	beginLine := "\n" + repositoryMonitorReviewContextBeginMarker + "\n"
	endLine := "\n" + repositoryMonitorReviewContextEndMarker
	start := strings.Index(prompt, beginLine)
	if start < 0 {
		return 0, 0, false
	}
	start++
	end := strings.LastIndex(prompt, endLine)
	if end < start {
		return 0, 0, false
	}
	return start, end + len(endLine), true
}

func repositoryMonitorReviewPromptContext(prompt string) (repositoryMonitorReviewContext, bool) {
	start, end, found := repositoryMonitorReviewPromptContextBounds(prompt)
	if !found {
		return repositoryMonitorReviewContext{}, false
	}
	block := prompt[start:end]
	block = strings.TrimPrefix(block, repositoryMonitorReviewContextBeginMarker+"\n")
	block = strings.TrimSuffix(block, "\n"+repositoryMonitorReviewContextEndMarker)
	var parsed repositoryMonitorReviewContext
	if err := json.Unmarshal([]byte(block), &parsed); err != nil {
		return repositoryMonitorReviewContext{}, false
	}
	return parsed, true
}

func repositoryMonitorReviewContextIsUnavailableEnvelope(candidate, expected repositoryMonitorReviewContext) bool {
	if candidate.SchemaVersion != expected.SchemaVersion || candidate.Repo != expected.Repo || candidate.PRNumber != expected.PRNumber ||
		candidate.BaseSHA != expected.BaseSHA || candidate.HeadSHA != expected.HeadSHA {
		return false
	}
	if candidate.ChangedFileCount != 0 || len(candidate.Files) != 0 || candidate.Truncated.Files || candidate.Truncated.Bytes {
		return false
	}
	return repositoryMonitorReviewContextErrorClassWellFormed(candidate.ContextUnavailable)
}

func repositoryMonitorReviewContextErrorClassWellFormed(class string) bool {
	switch class {
	case repositoryMonitorReviewContextErrorTimeout, repositoryMonitorReviewContextErrorNetwork, repositoryMonitorReviewContextErrorInvalidPayload, repositoryMonitorReviewContextErrorRequestFailed:
		return true
	}
	status, ok := strings.CutPrefix(class, repositoryMonitorReviewContextErrorGitHubStatus)
	if !ok || len(status) != 3 {
		return false
	}
	for _, r := range status {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func repositoryMonitorReviewVerdictGateReason(prompt, verdict string) string {
	if strings.TrimSpace(verdict) != repositoryMonitorReviewVerdictPassed {
		return ""
	}
	return repositoryMonitorReviewContextGap(prompt)
}

func repositoryMonitorReviewContextGap(prompt string) string {
	reviewContext, ok := repositoryMonitorReviewPromptContext(prompt)
	switch {
	case !ok:
		return "the review task carried no embedded diff context"
	case reviewContext.ContextUnavailable != "":
		return "the diff context was unavailable (" + reviewContext.ContextUnavailable + ")"
	case reviewContext.Truncated.Files:
		return "the change set was not completely represented in the diff context"
	}
	if delta, ok := repositoryMonitorReviewPromptDelta(prompt); ok && (delta.ContextUnavailable != "" || delta.Truncated.Files) {
		return "the changes since the last review were not completely represented"
	}
	return ""
}
