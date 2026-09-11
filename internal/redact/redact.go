package redact

import (
	"regexp"
	"slices"
	"strings"
)

const redactedValue = "[REDACTED]"

var (
	authorizationHeaderRe = regexp.MustCompile(`(?i)\b(authorization\s*:\s*)(?:bearer|basic)\s+[A-Za-z0-9._~+/=-]+`)
	txnTokenHeaderRe      = regexp.MustCompile(`(?i)\b(txn-token\s*:\s*)[A-Za-z0-9._~+/=-]+`)
	cookieHeaderRe        = regexp.MustCompile(`(?i)\b((?:set-cookie|cookie)\s*:\s*)[^\r\n]+`)
	// Unquoted assignment values consume every non-space character: a
	// redactor must never leave a recoverable credential tail behind a
	// comma or semicolon, and over-redacting a following word in prose is
	// the safe direction.
	sensitiveAssignmentRe   = regexp.MustCompile(`(?i)(["']?)([A-Z0-9_.-]*(?:api[-_]?key|token|secret|password|passwd|pwd|credential|private[-_]?key|client[-_]?secret|access[-_]?token|refresh[-_]?token)[A-Z0-9_.-]*)(["']?)(\s*[:=]\s*)("[^"\r\n]*"|'[^'\r\n]*'|[^\s"']+)`)
	escapedQuotedValueRe    = regexp.MustCompile(`^(?:"(?:[^"\\\r\n]|\\[^\r\n])*"|'(?:[^'\\\r\n]|\\[^\r\n])*')`)
	naturalLanguageSecretRe = regexp.MustCompile(`(?i)\b((?:api\s+key|token|secret|password|credential)\s+is\s+)([^\s]+)`) // e.g. "token is abc123"
	wellKnownTokenRe        = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{20,}|gh[pousr]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{30,}|xox[baprs]-[A-Za-z0-9-]{20,})\b`)
	jwtRe                   = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)
	urlCredentialRe         = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^\s/@]+@`)
	// Signed/pre-signed URL query credentials whose parameter names do not
	// contain a generic secret keyword: Azure SAS (sig), AWS SigV4
	// (X-Amz-Signature, X-Amz-Security-Token), GCS (X-Goog-Signature), and
	// generic signature/sas parameters.
	signedURLQueryRe = regexp.MustCompile(`(?i)([?&](?:sig|signature|sas|x-amz-signature|x-amz-security-token|x-amz-credential|x-goog-signature|x-goog-credential)=)[^&#\s"'<>,;()\[\]]+`)
)

// SensitiveText replaces common credential and token shapes with a stable
// placeholder suitable for persistence in durable memory and proposals. It is
// intentionally conservative about preserving surrounding prose while removing
// values that look like credentials.
func SensitiveText(s string) string {
	return SensitiveTextWithValues(s)
}

// SensitiveTextWithValues also removes supplied credentials and their canonical
// JSON-escaped forms. All matches use the original text so one replacement cannot
// hide another credential's label or leave part of a known value behind.
func SensitiveTextWithValues(s string, values ...string) string {
	// Find every literal assignment before changing text. An escaped quote can
	// make a value span another credential's label. Collect all remaining
	// patterns before replacement so their values cannot lose their labels first.
	matches := sensitiveAssignmentRe.FindAllStringSubmatchIndex(s, -1)
	spans := make([]valueSpan, 0, len(matches))
	for _, match := range matches {
		start, end := match[len(match)-2], match[len(match)-1]
		if escaped := escapedQuotedValueRe.FindStringIndex(s[start:]); escaped != nil {
			// Unmarked text can be valid under both literal and escaped quote
			// rules. Cover both interpretations; guessing that a following word
			// is shell syntax can expose the tail of an escaped credential.
			end = max(end, start+escaped[1])
		}
		spans = append(spans, valueSpan{start: start, end: end, quoted: true})
	}
	for _, pattern := range []*regexp.Regexp{
		authorizationHeaderRe, txnTokenHeaderRe, cookieHeaderRe, naturalLanguageSecretRe, signedURLQueryRe,
	} {
		for _, match := range pattern.FindAllStringSubmatchIndex(s, -1) {
			spans = append(spans, valueSpan{start: match[3], end: match[1]})
		}
	}
	for _, match := range urlCredentialRe.FindAllStringSubmatchIndex(s, -1) {
		spans = append(spans, valueSpan{start: match[3], end: match[1] - 1}) // Preserve the trailing @.
	}
	for _, pattern := range []*regexp.Regexp{wellKnownTokenRe, jwtRe} {
		for _, match := range pattern.FindAllStringIndex(s, -1) {
			spans = append(spans, valueSpan{start: match[0], end: match[1]})
		}
	}
	return redactSpans(s, appendKnownValueSpans(spans, s, values...))
}

type valueSpan struct {
	start, end int
	quoted     bool
}

func redactSpans(s string, spans []valueSpan) string {
	if len(spans) == 0 {
		return s
	}
	slices.SortStableFunc(spans, func(a, b valueSpan) int { return a.start - b.start })
	merged := spans[:0]
	for _, span := range spans {
		if n := len(merged); n > 0 && span.start < merged[n-1].end {
			merged[n-1].end = max(merged[n-1].end, span.end)
		} else {
			merged = append(merged, span)
		}
	}
	var output strings.Builder
	output.Grow(len(s))
	cursor := 0
	for _, span := range merged {
		output.WriteString(s[cursor:span.start])
		replacement := redactedValue
		// Preserve quotes so another pass cannot consume surrounding Markdown.
		if quote := s[span.start : span.start+1]; span.quoted && (quote == "\"" || quote == "'") {
			replacement = quote + replacement + quote
		}
		output.WriteString(replacement)
		cursor = span.end
	}
	output.WriteString(s[cursor:])
	return output.String()
}
