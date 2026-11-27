package redact

import (
	"regexp"
	"strings"
)

const (
	redactedValue         = "[REDACTED]"
	authorizationPrefix   = `(?:proxy-)?authorization(?:\\?["'])?\s*[:=]\s*(?:\[\s*)?`
	authorizationScheme   = `(?:bearer|basic|token|digest|negotiate|ntlm|apikey|api-key|key|bot|sso-key)\s+`
	authParam             = `[A-Za-z0-9._~+/-]+[ \t]*=[ \t]*(?:` + authParamEscapedValue + `|"(?:[^"\\\r\n]|\\[^\r\n])*"|'[^'\r\n]*'|[A-Za-z0-9._~+/=-]*)`
	authParamOpen         = `[A-Za-z0-9._~+/-]+[ \t]*=[ \t]*(?:` + authParamEscapedValue + `|"(?:[^"\\\r\n]|\\[^\r\n])*"|'[^'\r\n]*'|["'][^\r\n]*|[A-Za-z0-9._~+/=-]*)`
	authParamEscapedValue = `\\["'](?:[^\\\r\n]|\\[^"'\r\n])*(?:\\["']|$)`
	token68               = `[A-Za-z0-9._~+/=-]+`
)

var (
	authorizationQuotedHeaderRe = regexp.MustCompile(`(?i)\b(` + authorizationPrefix + `\\?["'])` + authorizationScheme + `(?:` + authParam + `(?:[ \t]*,[ \t]*` + authParam + `)*|` + token68 + `)`)
	authorizationHeaderRe       = regexp.MustCompile(`(?i)\b(` + authorizationPrefix + `)` + authorizationScheme + `(?:` + authParamOpen + `(?:[ \t]*,[ \t]*` + authParamOpen + `)*|` + token68 + `)`)
	txnTokenHeaderRe            = regexp.MustCompile(`(?i)\b(txn-token\s*:\s*)[A-Za-z0-9._~+/=-]+`)
	cookieHeaderRe              = regexp.MustCompile(`(?i)\b((?:set-cookie|cookie)\s*:\s*)[^\r\n]+`)
	sensitiveAssignmentRe       = regexp.MustCompile(`(?i)(["']?)([A-Z0-9_.-]*(?:api[-_]?key|token|secret|password|passwd|pwd|credential|private[-_]?key|client[-_]?secret|access[-_]?token|refresh[-_]?token)[A-Z0-9_.-]*)(["']?)(\s*[:=]\s*)("[^"\r\n]*"|'[^'\r\n]*'|[^\s"']+)`)
	naturalLanguageSecretRe     = regexp.MustCompile(`(?i)\b((?:api\s+key|token|secret|password|credential)\s+is\s+)([^\s]+)`)
	wellKnownTokenRe            = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{20,}|gh[pousr]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{30,}|xox[abposr]-[A-Za-z0-9-]{10,}|xapp-[A-Za-z0-9-]{10,}|` +
		`ya29\.[A-Za-z0-9_-]{20,}|AIza[A-Za-z0-9_-]{30,}|(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,}|gl(?:pat|ptt|dt|rt)-[A-Za-z0-9_-]{20,}|npm_[A-Za-z0-9]{30,}|hv[sbr]\.[A-Za-z0-9_-]{20,}|dckr_pat_[A-Za-z0-9_-]{20,})\b`)
	webhookURLPathRe     = regexp.MustCompile(`(/(?:services|workflows|triggers)/)T[A-Z0-9]{6,}/[A-Za-z0-9_/-]+|(/api/webhooks/)[0-9]{6,}/[A-Za-z0-9_-]+|(/webhookb2/)[A-Za-z0-9_./@-]+`)
	azureConnectionKeyRe = regexp.MustCompile(`(?i)\b((?:AccountKey|SharedAccessKey|SharedAccessSignature)\s*=\s*)[^;\s"']+`)
	jwtRe                = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)
	urlCredentialRe      = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^\s/@]+@`)
	signedURLQueryRe     = regexp.MustCompile(`(?i)([?&](?:sig|signature|sas|x-amz-signature|x-amz-security-token|x-amz-credential|x-goog-signature|x-goog-credential)=)[^&#\s"'<>,;()\[\]]+`)
)

func SensitiveText(s string) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	s = authorizationQuotedHeaderRe.ReplaceAllString(s, `${1}`+redactedValue)
	s = authorizationHeaderRe.ReplaceAllString(s, `${1}`+redactedValue)
	s = txnTokenHeaderRe.ReplaceAllString(s, `${1}`+redactedValue)
	s = cookieHeaderRe.ReplaceAllString(s, `${1}`+redactedValue)
	s = sensitiveAssignmentRe.ReplaceAllStringFunc(s, func(match string) string {
		parts := sensitiveAssignmentRe.FindStringSubmatch(match)
		replacement := redactedValue
		if quote := parts[5][:1]; quote == "\"" || quote == "'" {
			replacement = quote + replacement + quote
		}
		return parts[1] + parts[2] + parts[3] + parts[4] + replacement
	})
	s = naturalLanguageSecretRe.ReplaceAllString(s, `${1}`+redactedValue)
	s = urlCredentialRe.ReplaceAllString(s, `${1}`+redactedValue+`@`)
	s = signedURLQueryRe.ReplaceAllString(s, `${1}`+redactedValue)
	s = webhookURLPathRe.ReplaceAllString(s, `${1}${2}${3}`+redactedValue)
	s = azureConnectionKeyRe.ReplaceAllString(s, `${1}`+redactedValue)
	s = wellKnownTokenRe.ReplaceAllString(s, redactedValue)
	s = jwtRe.ReplaceAllString(s, redactedValue)
	return s
}
