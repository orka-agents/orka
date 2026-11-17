package redact

import (
	"regexp"
	"strings"
)

const (
	redactedValue = "[REDACTED]"
	// authorizationPrefix is an Authorization header's name and separator,
	// bare or serialized: a quoted name ({"Authorization":...}) or a Go
	// http.Header dump (map[Authorization:[...]]).
	// A backslash may escape the quotes inside an enclosing serialized
	// string ({\"Authorization\":\"Basic x\"}).
	authorizationPrefix = `(?:proxy-)?authorization(?:\\?["'])?\s*[:=]\s*(?:\[\s*)?`
	authorizationScheme = `(?:bearer|basic|token|digest|negotiate|ntlm|apikey|api-key|key|bot|sso-key)\s+`
	// authParam is one name=value Authorization header parameter; a value
	// may be quoted. A parameter stays on its header's line. authParamOpen
	// also takes a quoted value that runs to
	// the end of the line unclosed, so no part of it is left behind.
	authParam     = `[A-Za-z0-9._~+/-]+[ \t]*=[ \t]*(?:` + authParamEscapedValue + `|"[^"\r\n]*"|'[^'\r\n]*'|[A-Za-z0-9._~+/=-]*)`
	authParamOpen = `[A-Za-z0-9._~+/-]+[ \t]*=[ \t]*(?:` + authParamEscapedValue + `|"[^"\r\n]*"|'[^'\r\n]*'|["'][^\r\n]*|[A-Za-z0-9._~+/=-]*)`
	// authParamEscapedValue is a quoted value inside a JSON or other
	// backslash-escaped string, as in "Token token=\"x\"". An unclosed one
	// runs to the end of the line.
	authParamEscapedValue = `\\["'](?:[^\\\r\n]|\\[^"'\r\n])*(?:\\["']|$)`
	token68               = `[A-Za-z0-9._~+/=-]+`
)

var (
	// Authorization headers with any common scheme, including GitHub's
	// "token" scheme, and a token68 value or a list of auth-params, as in
	// Token token="x" or Digest username="u", response="r". The whole value
	// is consumed: a partial match would drop an auth-param's name and leave
	// its quoted value unrecognized. A quoted header value ('Bearer x') is
	// matched first: its closing quote may directly follow base64 padding,
	// so it takes only closed auth-param values.
	authorizationQuotedHeaderRe = regexp.MustCompile(`(?i)\b(` + authorizationPrefix + `\\?["'])` + authorizationScheme + `(?:` + authParam + `(?:[ \t]*,[ \t]*` + authParam + `)*|` + token68 + `)`)
	authorizationHeaderRe       = regexp.MustCompile(`(?i)\b(` + authorizationPrefix + `)` + authorizationScheme + `(?:` + authParamOpen + `(?:[ \t]*,[ \t]*` + authParamOpen + `)*|` + token68 + `)`)
	txnTokenHeaderRe            = regexp.MustCompile(`(?i)\b(txn-token\s*:\s*)[A-Za-z0-9._~+/=-]+`)
	cookieHeaderRe              = regexp.MustCompile(`(?i)\b((?:set-cookie|cookie)\s*:\s*)[^\r\n]+`)
	// Unquoted assignment values consume every non-space character: a
	// redactor must never leave a recoverable credential tail behind a
	// comma or semicolon, and over-redacting a following word in prose is
	// the safe direction.
	sensitiveAssignmentRe   = regexp.MustCompile(`(?i)(["']?)([A-Z0-9_.-]*(?:api[-_]?key|token|secret|password|passwd|pwd|credential|private[-_]?key|client[-_]?secret|access[-_]?token|refresh[-_]?token)[A-Z0-9_.-]*)(["']?)(\s*[:=]\s*)("[^"\r\n]*"|'[^'\r\n]*'|[^\s"']+)`)
	naturalLanguageSecretRe = regexp.MustCompile(`(?i)\b((?:api\s+key|token|secret|password|credential)\s+is\s+)([^\s]+)`) // e.g. "token is abc123"
	wellKnownTokenRe        = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{20,}|gh[pousr]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{30,}|xox[abposr]-[A-Za-z0-9-]{10,}|xapp-[A-Za-z0-9-]{10,}|` +
		// Google OAuth access tokens and API keys, Stripe secret and
		// restricted keys, GitLab, npm, Vault, and Docker Hub tokens.
		`ya29\.[A-Za-z0-9_-]{20,}|AIza[A-Za-z0-9_-]{30,}|(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,}|gl(?:pat|ptt|dt|rt)-[A-Za-z0-9_-]{20,}|npm_[A-Za-z0-9]{30,}|hv[sbr]\.[A-Za-z0-9_-]{20,}|dckr_pat_[A-Za-z0-9_-]{20,})\b`)
	// Webhook URLs whose path is the credential: Slack, Discord, and
	// Microsoft Teams incoming webhooks.
	webhookURLPathRe = regexp.MustCompile(`(?i)(\bhooks\.slack\.com/(?:services|workflows|triggers)/|\bdiscord(?:app)?\.com/api/webhooks/|\.webhook\.office\.com/webhookb2/)[A-Za-z0-9_./@-]+`)
	// Azure Storage and Service Bus connection string keys.
	azureConnectionKeyRe = regexp.MustCompile(`(?i)\b((?:AccountKey|SharedAccessKey|SharedAccessSignature)\s*=\s*)[^;\s"']+`)
	jwtRe                = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)
	urlCredentialRe      = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^\s/@]+@`)
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
		// Keep quoted values quoted so another pass cannot consume Markdown
		// delimiters or punctuation outside the original value.
		if quote := parts[5][:1]; quote == "\"" || quote == "'" {
			replacement = quote + replacement + quote
		}
		return parts[1] + parts[2] + parts[3] + parts[4] + replacement
	})
	s = naturalLanguageSecretRe.ReplaceAllString(s, `${1}`+redactedValue)
	s = urlCredentialRe.ReplaceAllString(s, `${1}`+redactedValue+`@`)
	s = signedURLQueryRe.ReplaceAllString(s, `${1}`+redactedValue)
	s = webhookURLPathRe.ReplaceAllString(s, `${1}`+redactedValue)
	s = azureConnectionKeyRe.ReplaceAllString(s, `${1}`+redactedValue)
	s = wellKnownTokenRe.ReplaceAllString(s, redactedValue)
	s = jwtRe.ReplaceAllString(s, redactedValue)
	return s
}
