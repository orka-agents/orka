package redact

import (
	"regexp"
	"strings"
)

const redactedValue = "[REDACTED]"

var (
	authorizationHeaderRe   = regexp.MustCompile(`(?i)\b(authorization\s*:\s*)(?:bearer|basic)\s+[A-Za-z0-9._~+/=-]+`)
	txnTokenHeaderRe        = regexp.MustCompile(`(?i)\b(txn-token\s*:\s*)[A-Za-z0-9._~+/=-]+`)
	sensitiveAssignmentRe   = regexp.MustCompile(`(?i)(["']?)([A-Z0-9_.-]*(?:api[-_]?key|token|secret|password|passwd|pwd|credential|private[-_]?key|client[-_]?secret|access[-_]?token|refresh[-_]?token)[A-Z0-9_.-]*)(["']?)(\s*[:=]\s*)(?:"[^"\r\n]*"|'[^'\r\n]*'|[^\s"',;]+)`)
	naturalLanguageSecretRe = regexp.MustCompile(`(?i)\b((?:api\s+key|token|secret|password|credential)\s+is\s+)([^\s,.;]+)`) // e.g. "token is abc123"
	wellKnownTokenRe        = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{20,}|gh[pousr]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{30,}|xox[baprs]-[A-Za-z0-9-]{20,})\b`)
	jwtRe                   = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)
	urlCredentialRe         = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^\s/@]+@`)
)

// SensitiveText replaces common credential and token shapes with a stable
// placeholder suitable for persistence in durable memory and proposals. It is
// intentionally conservative about preserving surrounding prose while removing
// values that look like credentials.
func SensitiveText(s string) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	s = authorizationHeaderRe.ReplaceAllString(s, `${1}`+redactedValue)
	s = txnTokenHeaderRe.ReplaceAllString(s, `${1}`+redactedValue)
	s = sensitiveAssignmentRe.ReplaceAllString(s, `${1}${2}${3}${4}`+redactedValue)
	s = naturalLanguageSecretRe.ReplaceAllString(s, `${1}`+redactedValue)
	s = urlCredentialRe.ReplaceAllString(s, `${1}`+redactedValue+`@`)
	s = wellKnownTokenRe.ReplaceAllString(s, redactedValue)
	s = jwtRe.ReplaceAllString(s, redactedValue)
	return s
}

// SensitiveStringSlice redacts each string in values in place and returns it.
func SensitiveStringSlice(values []string) []string {
	for i := range values {
		values[i] = SensitiveText(values[i])
	}
	return values
}
