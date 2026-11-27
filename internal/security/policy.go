package security

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const (
	DefaultPolicyConfigMapKey   = "policy"
	PolicyConfigMapAllowedLabel = "orka.ai/security-policy"
	MaxCustomPolicyBytes        = 32 * 1024
)

type PolicySource struct {
	Name   string `json:"name,omitempty"`
	Key    string `json:"key,omitempty"`
	Digest string `json:"digest,omitempty"`
}

type ScannerPolicy struct {
	CustomScanInstructions string
	FalsePositivePolicy    string
	CustomScanSource       PolicySource
	FalsePositiveSource    PolicySource
	Digest                 string
}

func PolicyRefKey(ref *corev1alpha1.PolicyConfigMapKeyRef) string {
	if ref == nil || strings.TrimSpace(ref.Key) == "" {
		return DefaultPolicyConfigMapKey
	}
	return strings.TrimSpace(ref.Key)
}

func ValidateCustomPolicyText(text string) error {
	if len([]byte(text)) > MaxCustomPolicyBytes {
		return fmt.Errorf("policy exceeds %d bytes", MaxCustomPolicyBytes)
	}
	if LooksLikeSecret(text) {
		return fmt.Errorf("policy appears to contain a secret or token")
	}
	return nil
}

var (
	policySensitivePrefixPattern          = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9])(?:(?:github` + `_pat_|` + `g` + `hp_|xo` + `xb-|s` + `k-)[A-Za-z0-9_./+=:-]{8,}|(?:A` + `KIA|A` + `SIA)[A-Z0-9]{16})`)
	policySensitiveAssignmentPattern      = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])(?:[A-Za-z0-9]+[_-]){0,3}(?:api[_-]?key|access[_-]?` + `token|refresh[_-]?` + `token|id[_-]?` + `token|auth[_-]?` + `token|to` + `ken|pass` + `word|clien` + `t[_-]?secret|secr` + `et(?:[_-]?(?:access[_-]?)?key)?|cred` + `entials?|priv` + `ate[_-]?key)\s*[:=]\s*(?:"([^"\r\n]{16,})"|'([^'\r\n]{16,})'|([A-Za-z0-9_./+=~:@#$%^&*!?|,;{}\\` + "`" + `-]{16,}))`)
	policySensitiveYAMLAssignmentPattern  = regexp.MustCompile(`(?im)^[\t ]*(?:-\s*)?["']?(?:[A-Za-z0-9]+[_-]){0,3}(?:api[_-]?key|access[_-]?` + `token|refresh[_-]?` + `token|id[_-]?` + `token|auth[_-]?` + `token|to` + `ken|pass` + `word|clien` + `t[_-]?secret|secr` + `et(?:[_-]?(?:access[_-]?)?key)?|cred` + `entials?|priv` + `ate[_-]?key)["']?\s*:[ \t]+([^\r\n]+)$`)
	policySensitiveYAMLBlockHeaderPattern = regexp.MustCompile(`(?i)^[\t ]*(?:-\s*)?["']?(?:[A-Za-z0-9]+[_-]){0,3}(?:api[_-]?key|access[_-]?` + `token|refresh[_-]?` + `token|id[_-]?` + `token|auth[_-]?` + `token|to` + `ken|pass` + `word|clien` + `t[_-]?secret|secr` + `et(?:[_-]?(?:access[_-]?)?key)?|cred` + `entials?|priv` + `ate[_-]?key)["']?\s*:\s*(?:[!&]\S*\s+)*[|>](?:[+-][1-9]?|[1-9][+-]?)?[ \t]*(?:#[^\r\n]*)?$`)
	policySensitiveYAMLEmptyValuePattern  = regexp.MustCompile(`(?i)^[\t ]*(?:-\s*)?["']?(?:[A-Za-z0-9]+[_-]){0,3}(?:api[_-]?key|access[_-]?` + `token|refresh[_-]?` + `token|id[_-]?` + `token|auth[_-]?` + `token|to` + `ken|pass` + `word|clien` + `t[_-]?secret|secr` + `et(?:[_-]?(?:access[_-]?)?key)?|cred` + `entials?|priv` + `ate[_-]?key)["']?\s*:\s*(?:#[^\r\n]*)?$`)
	policyJWTPattern                      = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_-])ey` + `J[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}([^A-Za-z0-9_-]|$)`)
	policyBearerHeaderPattern             = regexp.MustCompile(`(?i)auth` + `orization\s*:\s*be` + `arer\s+([A-Za-z0-9_./+=~:-]{16,})`)
	policyBasicHeaderPattern              = regexp.MustCompile(`(?i)auth` + `orization\s*:\s*ba` + `sic\s+([A-Za-z0-9+/=_-]{16,})`)
	policySignedURLPattern                = regexp.MustCompile(`(?i)[?&](?:sig|sign` + `ature|sas|x-amz-sign` + `ature|x-amz-sec` + `urity-token|x-amz-cred` + `ential|x-goog-sign` + `ature|x-goog-cred` + `ential)=([^&#\s"'<>,;()\[\]]{16,})`)
	policyTxnTokenPattern                 = regexp.MustCompile(`(?i)\btxn?-to` + `ken\s*:\s*([A-Za-z0-9_./+=~:-]{16,})`)
	policyCookiePattern                   = regexp.MustCompile(`(?i)\b(set-cookie|cookie)\s*:\s*([^\r\n]+)`)
	policyCookieValuePattern              = regexp.MustCompile("^[A-Za-z0-9_./+=~:@#$%^&*!?|,'\\\\`-]{16,}$")
)

var (
	directPlaceholderPattern = regexp.MustCompile(
		`^(?:` +
			`\$[A-Za-z_][A-Za-z0-9_]*` +
			`|\$\{[A-Za-z_][A-Za-z0-9_]*\}` +
			`|%\([A-Za-z_][A-Za-z0-9_]*\)[A-Za-z]` +
			`)$`)
	windowsEnvironmentPlaceholderPattern = regexp.MustCompile(`^%[A-Z_][A-Z0-9_]*%$`)
	templateFieldPlaceholderPattern      = regexp.MustCompile(`^\.[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*$`)
	placeholderMarkerPattern             = regexp.MustCompile(`(?i)^(?:redacted|masked|placeholder|example|sample|dummy|changeme|(?:your[-_ ]*)?(?:api[-_ ]?key|access[-_ ]?token|auth[-_ ]?token|id[-_ ]?token|refresh[-_ ]?token|transaction[-_ ]?token|token|password|passwd|secret|credential|private[-_ ]?key)(?:[-_ ]*(?:here|value|placeholder|redacted))?)$`)
)

func secretValuePlaceholder(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	if directPlaceholderPattern.MatchString(value) || windowsEnvironmentPlaceholderPattern.MatchString(value) {
		return true
	}
	if strings.HasPrefix(value, "{{") && strings.HasSuffix(value, "}}") {
		inner := strings.TrimSpace(value[2 : len(value)-2])
		return templateFieldPlaceholderPattern.MatchString(inner) || placeholderMarkerPattern.MatchString(inner)
	}
	if len(value) < 2 {
		return false
	}
	switch {
	case value[0] == '{' && value[len(value)-1] == '}',
		value[0] == '<' && value[len(value)-1] == '>',
		value[0] == '[' && value[len(value)-1] == ']',
		value[0] == '%' && value[len(value)-1] == '%':
		return placeholderMarkerPattern.MatchString(strings.TrimSpace(value[1 : len(value)-1]))
	default:
		return false
	}
}

var codeReferencePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)+$`)

var codeReferenceCredentialTail = regexp.MustCompile(`(?i)^(?:api[_-]?key|access[_-]?` + `token|refresh[_-]?` + `token|id[_-]?` + `token|auth[_-]?` + `token|to` + `ken|pass` + `word|passwd|clien` + `t[_-]?secret|secr` + `et(?:[_-]?(?:access[_-]?)?key)?|credentials?|priv` + `ate[_-]?key|key)$`)

var callableReferencePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*$`)
var unqualifiedCredentialReaderPattern = regexp.MustCompile(`(?i)^(?:fetch|get|load|lookup|read|resolve)[A-Za-z0-9_]*(?:api[_-]?key|token|password|passwd|secret|credential|private[_-]?key)[A-Za-z0-9_]*$`)

func callableReferenceShape(value string) bool {
	if !callableReferencePattern.MatchString(value) {
		return false
	}
	if strings.Contains(value, ".") {
		return true
	}
	return unqualifiedCredentialReaderPattern.MatchString(value)
}

func hasCompleteCallSuffix(text string, start int) bool {
	if start >= len(text) || text[start] != '(' {
		return false
	}
	depth := 0
	var quote byte
	escaped := false
	for i := start; i < len(text); i++ {
		ch := text[i]
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == quote {
				quote = 0
			}
			continue
		}
		switch ch {
		case '\'', '"', '`':
			quote = ch
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return onlyStatementTrailer(text[i+1:])
			}
		case '\r', '\n':
			return false
		}
	}
	return false
}

func onlyStatementTrailer(rest string) bool {
	if i := strings.IndexAny(rest, "\r\n"); i >= 0 {
		if nextLineContinuesExpression(rest[i:]) {
			return false
		}
		rest = rest[:i]
	}
	for {
		rest = strings.TrimLeft(rest, " \t;,)]}")
		switch {
		case rest == "":
			return true
		case strings.HasPrefix(rest, "//"), strings.HasPrefix(rest, "#"):
			return !strings.ContainsAny(rest, "\"'`")
		case strings.HasPrefix(rest, "/*"):
			end := strings.Index(rest[2:], "*/")
			if end < 0 {
				return false
			}
			rest = rest[2+end+2:]
		default:
			return false
		}
	}
}

func nextLineContinuesExpression(text string) bool {
	for _, line := range splitLines(text)[1:] {
		trimmed := strings.TrimSpace(line)
		for strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "*/") {
			if strings.HasPrefix(trimmed, "*/") {
				trimmed = strings.TrimSpace(trimmed[2:])
				continue
			}
			end := strings.Index(trimmed[2:], "*/")
			if end < 0 {
				return true
			}
			trimmed = strings.TrimSpace(trimmed[2+end+2:])
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "#") {
			continue
		}
		return !startsStatement(trimmed)
	}
	return false
}

var continuationWords = map[string]bool{
	"and": true, "or": true, "not": true, "xor": true, "in": true, "is": true, "div": true, "mod": true,
	"as": true, "like": true, "between": true,
}

func startsStatement(trimmed string) bool {
	if trimmed == "" {
		return false
	}
	word := strings.ToLower(trimmed)
	if i := strings.IndexFunc(word, func(r rune) bool { return r < 'a' || r > 'z' }); i >= 0 {
		word = word[:i]
	}
	if continuationWords[word] {
		return false
	}
	r := rune(trimmed[0])
	switch {
	case r == '_', r == '$', r == '}', r == '@', r == ';':
		return true
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r >= 0x80:
		return true
	}
	return false
}

func secretValueIsCode(text string, value string, end int) bool {
	if assignmentSeparatorBefore(text, end-len(value)) != '=' {
		return false
	}
	if callableReferenceShape(value) && hasCompleteCallSuffix(text, end) {
		return true
	}
	if !codeReferencePattern.MatchString(value) {
		return false
	}
	tail := value[strings.LastIndexByte(value, '.')+1:]
	return codeReferenceCredentialTail.MatchString(tail)
}

func assignmentSeparatorBefore(text string, start int) byte {
	for i := start - 1; i >= 0; i-- {
		switch text[i] {
		case ' ', '\t', '"', '\'':
			continue
		case ':', '=':
			return text[i]
		default:
			return 0
		}
	}
	return 0
}

func trimYAMLPlainScalarComment(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || value[0] == '#' {
		return ""
	}
	for i := 1; i < len(value); i++ {
		if value[i] == '#' && (value[i-1] == ' ' || value[i-1] == '\t') {
			value = value[:i]
			break
		}
	}
	return strings.TrimSpace(value)
}

func yamlPlainScalarAssignmentsLookLikeSecret(scan *secretScan) bool {
	for _, match := range scan.findAllSubmatch(policySensitiveYAMLAssignmentPattern, credentialKeywords, secretScanTokenRuns) {
		value := trimYAMLPlainScalarComment(match[1])
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = strings.TrimSpace(value[1 : len(value)-1])
		}
		if len(value) < 16 {
			continue
		}
		if !secretValuePlaceholder(value) {
			return true
		}
	}
	return false
}

func yamlMultilineScalarAssignmentsLookLikeSecret(scan *secretScan) bool {
	lines := scan.splitLines()
	for _, i := range scan.credentialKeyLines() {
		first, ok := yamlMultilineScalarStart(lines, i)
		if !ok {
			continue
		}
		candidate, quoted, closed, ok := yamlJoinScalarContinuation(lines, i, first)
		if !ok {
			continue
		}
		if !closed {
			return true
		}
		if !quoted && looksLikeOpenSourceExpression(candidate) {
			continue
		}
		if len(candidate) >= 16 && !secretValuePlaceholder(candidate) {
			return true
		}
	}
	return false
}

func looksLikeOpenSourceExpression(candidate string) bool {
	depth := 0
	for _, r := range candidate {
		switch r {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		}
	}
	if depth != 0 {
		return true
	}
	trimmed := strings.TrimSpace(candidate)
	return strings.HasSuffix(trimmed, ",") || strings.HasSuffix(trimmed, "+") || strings.HasSuffix(trimmed, "(")
}

func yamlMultilineScalarStart(lines []string, i int) (string, bool) {
	line := lines[i]
	first := ""
	if match := policySensitiveYAMLAssignmentPattern.FindStringSubmatch(line); match != nil {
		first = trimYAMLPlainScalarComment(match[1])
	} else if !policySensitiveYAMLEmptyValuePattern.MatchString(line) {
		return "", false
	}
	if first != "" {
		if first[0] == '|' || first[0] == '>' {
			return "", false
		}
		if (first[0] == '"' || first[0] == '\'') && yamlClosingQuoteIndex(first[1:], first[0]) >= 0 {
			return "", false
		}
		return first, true
	}
	for _, next := range lines[i+1:] {
		trimmed := strings.TrimSpace(next)
		if trimmed == "" {
			continue
		}
		if trimmed[0] == '"' || trimmed[0] == '\'' {
			if end := yamlClosingQuoteIndex(trimmed[1:], trimmed[0]); end >= 0 && strings.HasPrefix(strings.TrimSpace(trimmed[end+2:]), ":") {
				return "", false
			}
			return "", true
		}
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "#") || strings.Contains(trimmed, ": ") || strings.HasSuffix(trimmed, ":") {
			return "", false
		}
		return "", true
	}
	return "", false
}

func yamlJoinScalarContinuation(lines []string, i int, first string) (candidate string, quoted, closed, ok bool) {
	const maxContinuationLines = 64
	var quote byte
	if first != "" && (first[0] == '"' || first[0] == '\'') {
		quote = first[0]
	}
	if first == "" {
		for _, next := range lines[i+1:] {
			trimmed := strings.TrimSpace(next)
			if trimmed == "" {
				continue
			}
			if trimmed[0] == '"' || trimmed[0] == '\'' {
				quote = trimmed[0]
			}
			break
		}
	}
	baseIndent := len(lines[i]) - len(strings.TrimLeft(lines[i], " \t"))
	var value strings.Builder
	escapedBreak := quote == '"' && strings.HasSuffix(first, "\\")
	value.WriteString(strings.TrimSuffix(first, "\\"))
	joined := 0
	closed = quote == 0
	for _, next := range lines[i+1:] {
		if joined >= maxContinuationLines {
			break
		}
		trimmed := strings.TrimSpace(next)
		if trimmed == "" {
			continue
		}
		indent := len(next) - len(strings.TrimLeft(next, " \t"))
		if quote == 0 {
			if indent <= baseIndent || strings.Contains(trimmed, ": ") || strings.HasSuffix(trimmed, ":") {
				break
			}
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			if trimmed = trimYAMLPlainScalarComment(trimmed); trimmed == "" {
				continue
			}
		}
		joined++
		if !escapedBreak && value.Len() > 0 {
			value.WriteByte(' ')
		}
		if quote != 0 && value.Len() == 0 && trimmed[0] == quote {
			trimmed = trimmed[1:]
		}
		if quote != 0 {
			if end := yamlClosingQuoteIndex(trimmed, quote); end >= 0 {
				value.WriteString(trimmed[:end])
				closed = true
				break
			}
		}
		escapedBreak = quote == '"' && strings.HasSuffix(trimmed, "\\")
		value.WriteString(strings.TrimSuffix(trimmed, "\\"))
	}
	if joined == 0 {
		return "", quote != 0, closed, false
	}
	candidate = value.String()
	if quote != 0 {
		candidate = strings.TrimPrefix(candidate, string(quote))
	}
	return strings.TrimSpace(candidate), quote != 0, closed, true
}

func yamlClosingQuoteIndex(line string, quote byte) int {
	for i := 0; i < len(line); i++ {
		switch {
		case quote == '"' && line[i] == '\\':
			i++
		case line[i] == quote:
			if quote == '\'' && i+1 < len(line) && line[i+1] == '\'' {
				i++
				continue
			}
			return i
		}
	}
	return -1
}

func yamlBlockScalarAssignmentsLookLikeSecret(scan *secretScan) bool {
	lines := scan.splitLines()
	for _, i := range scan.credentialKeyLines() {
		line := lines[i]
		if !policySensitiveYAMLBlockHeaderPattern.MatchString(line) {
			continue
		}
		baseIndent := len(line) - len(strings.TrimLeft(line, " \t"))
		var value strings.Builder
		for _, contentLine := range lines[i+1:] {
			if strings.TrimSpace(contentLine) == "" {
				continue
			}
			indent := len(contentLine) - len(strings.TrimLeft(contentLine, " \t"))
			if indent <= baseIndent {
				break
			}
			value.WriteString(strings.TrimLeft(contentLine, " \t"))
		}
		candidate := strings.TrimSpace(value.String())
		if len(candidate) < 16 || secretValuePlaceholder(candidate) {
			continue
		}
		return true
	}
	return false
}

func cookieHeadersLookLikeSecret(scan *secretScan) bool {
	for _, match := range scan.findAllSubmatch(policyCookiePattern, cookieKeywords, secretScanTokenRuns) {
		parts := strings.Split(match[2], ";")
		if strings.EqualFold(match[1], "set-cookie") && len(parts) > 1 {
			parts = parts[:1]
		}
		for _, part := range parts {
			_, value, ok := strings.Cut(part, "=")
			if !ok {
				continue
			}
			value = strings.TrimSpace(value)
			if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
				value = value[1 : len(value)-1]
			}
			if !secretValuePlaceholder(value) && policyCookieValuePattern.MatchString(value) {
				return true
			}
		}
	}
	return false
}

func LooksLikeSecret(text string) bool {
	return looksLikeSecret(newSecretScan(stripUnsafeTextRunes(text)))
}

func SensitiveYAMLBlockHeader(line string) bool {
	return policySensitiveYAMLBlockHeaderPattern.MatchString(line)
}

func SensitiveYAMLEmptyValueKey(line string) bool {
	return policySensitiveYAMLEmptyValuePattern.MatchString(line)
}

func looksLikeSecret(scan *secretScan) bool {
	if scan.matchAny(policySensitivePrefixPattern, sensitivePrefixKeywords, 0) {
		return true
	}
	if scan.sensitiveValueMatch(policySensitiveAssignmentPattern, credentialKeywords) {
		return true
	}
	if yamlPlainScalarAssignmentsLookLikeSecret(scan) {
		return true
	}
	if scan.matchAny(policyJWTPattern, jwtKeywords, 0) {
		return true
	}
	if scan.sensitiveValueMatch(policyBearerHeaderPattern, authorizationKeywords) ||
		scan.sensitiveValueMatch(policyBasicHeaderPattern, authorizationKeywords) ||
		scan.sensitiveValueMatch(policyTxnTokenPattern, txnTokenKeywords) {
		return true
	}
	if cookieHeadersLookLikeSecret(scan) {
		return true
	}
	if scan.sensitiveValueMatch(policySignedURLPattern, signedURLKeywords) {
		return true
	}
	if yamlBlockScalarAssignmentsLookLikeSecret(scan) {
		return true
	}
	if yamlMultilineScalarAssignmentsLookLikeSecret(scan) {
		return true
	}
	return scan.containsFolded(pemBlockNeedle)
}

func PolicyTextDigest(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(text))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func ScannerPolicyDigest(policy ScannerPolicy) string {
	parts := []string{ScannerPolicyVersion}
	if policy.CustomScanSource.Name != "" || policy.CustomScanInstructions != "" {
		parts = append(parts,
			"custom-scan", policy.CustomScanSource.Name, policy.CustomScanSource.Key,
			PolicyTextDigest(policy.CustomScanInstructions),
		)
	}
	if policy.FalsePositiveSource.Name != "" || policy.FalsePositivePolicy != "" {
		parts = append(parts,
			"false-positive", policy.FalsePositiveSource.Name, policy.FalsePositiveSource.Key,
			PolicyTextDigest(policy.FalsePositivePolicy),
		)
	}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (p ScannerPolicy) PromptPolicy() PromptPolicy {
	return PromptPolicy{
		CustomScanInstructions: p.CustomScanInstructions,
		FalsePositivePolicy:    p.FalsePositivePolicy,
		PolicyDigest:           p.Digest,
		CustomScanSource:       p.CustomScanSource.String(),
		FalsePositiveSource:    p.FalsePositiveSource.String(),
	}
}

func (s PolicySource) String() string {
	if s.Name == "" {
		return ""
	}
	key := s.Key
	if key == "" {
		key = DefaultPolicyConfigMapKey
	}
	if s.Digest == "" {
		return s.Name + "/" + key
	}
	return s.Name + "/" + key + " (" + s.Digest + ")"
}

func PolicyProvenanceEnv(policy ScannerPolicy) string {
	items := []string{}
	if value := policy.CustomScanSource.String(); value != "" {
		items = append(items, "customScan="+value)
	}
	if value := policy.FalsePositiveSource.String(); value != "" {
		items = append(items, "falsePositive="+value)
	}
	sort.Strings(items)
	return strings.Join(items, ";")
}

func LoadScannerPolicy(ctx context.Context, reader client.Reader, namespace string, spec corev1alpha1.RepositoryScanSpec) (ScannerPolicy, error) {
	policy := ScannerPolicy{}
	if reader == nil {
		policy.Digest = ScannerPolicyDigest(policy)
		return policy, nil
	}
	if spec.CustomScanInstructionsRef != nil {
		text, source, err := loadPolicyConfigMapKey(ctx, reader, namespace, spec.CustomScanInstructionsRef)
		if err != nil {
			return ScannerPolicy{}, fmt.Errorf("customScanInstructionsRef: %w", err)
		}
		policy.CustomScanInstructions = text
		policy.CustomScanSource = source
	}
	if spec.FalsePositivePolicyRef != nil {
		text, source, err := loadPolicyConfigMapKey(ctx, reader, namespace, spec.FalsePositivePolicyRef)
		if err != nil {
			return ScannerPolicy{}, fmt.Errorf("falsePositivePolicyRef: %w", err)
		}
		policy.FalsePositivePolicy = text
		policy.FalsePositiveSource = source
	}
	policy.Digest = ScannerPolicyDigest(policy)
	return policy, nil
}

func policyConfigMapAllowed(cm corev1.ConfigMap) bool {
	return strings.EqualFold(strings.TrimSpace(cm.Labels[PolicyConfigMapAllowedLabel]), "true") ||
		strings.EqualFold(strings.TrimSpace(cm.Annotations[PolicyConfigMapAllowedLabel]), "true")
}

func loadPolicyConfigMapKey(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	ref *corev1alpha1.PolicyConfigMapKeyRef,
) (string, PolicySource, error) {
	name := strings.TrimSpace(ref.Name)
	if name == "" {
		return "", PolicySource{}, fmt.Errorf("name is required")
	}
	key := PolicyRefKey(ref)
	var cm corev1.ConfigMap
	if err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &cm); err != nil {
		return "", PolicySource{}, err
	}
	if !policyConfigMapAllowed(cm) {
		return "", PolicySource{}, fmt.Errorf("ConfigMap %q must be labeled or annotated %s=true to be used as scanner policy", name, PolicyConfigMapAllowedLabel)
	}
	value, ok := cm.Data[key]
	if !ok {
		return "", PolicySource{}, fmt.Errorf("key %q is missing in ConfigMap %q", key, name)
	}
	if err := ValidateCustomPolicyText(value); err != nil {
		return "", PolicySource{}, err
	}
	source := PolicySource{Name: name, Key: key, Digest: PolicyTextDigest(value)}
	return strings.TrimSpace(value), source, nil
}

func ScanRunIdempotencyKey(namespace, repositoryScan, mode, baseSHA, headSHA, subPath, policyDigest string) string {
	parts := []string{
		strings.TrimSpace(namespace),
		strings.TrimSpace(repositoryScan),
		strings.TrimSpace(mode),
		strings.TrimSpace(baseSHA),
		strings.TrimSpace(headSHA),
		strings.Trim(strings.TrimSpace(subPath), "/"),
		strings.TrimSpace(policyDigest),
		ScannerPolicyVersion,
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "scanidem:" + hex.EncodeToString(sum[:])
}

func SecretLikeLineDigests(text string) []string {
	lines := splitLines(text)
	var windows *lineWindows
	var digests []string
	for i, line := range lines {
		if strings.TrimSpace(line) == "" || !LooksLikeSecret(line) {
			continue
		}
		if windows == nil {
			windows = newLineWindows(lines)
		}
		digests = append(digests, windows.digest(i))
	}
	return digests
}

func StripLinesByDigest(text string, known map[string]struct{}) string {
	if len(known) == 0 {
		return text
	}
	lines := splitLines(text)
	var windows *lineWindows
	kept := make([]string, 0, len(lines))
	for i, line := range lines {
		if strings.TrimSpace(line) != "" && LooksLikeSecret(line) {
			if windows == nil {
				windows = newLineWindows(lines)
			}
			if _, ok := known[windows.digest(i)]; ok {
				continue
			}
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func splitLines(text string) []string {
	return strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
}

type lineWindows struct {
	lines  []string
	before []int
	after  []int
	cache  map[[2]int]string
}

func newLineWindows(lines []string) *lineWindows {
	w := &lineWindows{lines: lines, before: make([]int, len(lines)), after: make([]int, len(lines)), cache: map[[2]int]string{}}
	blank := func(i int) bool { return !isCodeLine(lines[i]) }
	start := 0
	for i := range lines {
		if i > 0 && blank(i-1) && !blank(i) {
			start = i
		}
		w.before[i] = start
	}
	end := len(lines) - 1
	for i := len(lines) - 1; i >= 0; i-- {
		if i+1 < len(lines) && blank(i+1) && !blank(i) {
			end = i
		}
		w.after[i] = end
	}
	prevStart := make([]int, len(lines))
	nextEnd := make([]int, len(lines))
	for i := range lines {
		j := w.before[i] - 1
		for j >= 0 && blank(j) {
			j--
		}
		if j >= 0 {
			prevStart[i] = w.before[j]
		} else {
			prevStart[i] = w.before[i]
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		j := w.after[i] + 1
		for j < len(lines) && blank(j) {
			j++
		}
		if j < len(lines) {
			nextEnd[i] = w.after[j]
		} else {
			nextEnd[i] = w.after[i]
		}
	}
	w.before, w.after = prevStart, nextEnd
	return w
}

func isCodeLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	for _, prefix := range []string{"//", "#", "/*", "*"} {
		if strings.HasPrefix(trimmed, prefix) {
			return false
		}
	}
	return true
}

func (w *lineWindows) digest(i int) string {
	bounds := [2]int{w.before[i], w.after[i]}
	if cached, ok := w.cache[bounds]; ok {
		return cached
	}
	digest := sha256.Sum256([]byte(strings.Join(w.lines[bounds[0]:bounds[1]+1], "\n")))
	encoded := hex.EncodeToString(digest[:])
	w.cache[bounds] = encoded
	return encoded
}
