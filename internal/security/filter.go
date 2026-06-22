package security

import (
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/sozercan/orka/internal/store"
)

const maxDroppedSampleValueBytes = 160

var (
	docsOnlyNegationPattern = regexp.MustCompile(`\bnot(?:\s+\w+){0,3}\s+(?:docs-only|docs only|documentation only|only documentation|markdown only)\b`)
	testOnlyNegationPattern = regexp.MustCompile(`\bnot(?:\s+\w+){0,3}\s+(?:test-only|test only|only test|test fixture only)\b`)
)

type FindingFilterOptions struct {
	RepositoryScan string
	ScanRunID      string
	TaskName       string
	SliceID        string
}

type FindingFilterResult struct {
	Kept    []*store.Finding
	Dropped []DroppedFindingDiagnostic
}

// FilterFindings applies deterministic hard false-positive exclusions after
// schema/evidence validation and before durable finding persistence.
func FilterFindings(findings []*store.Finding, _ FindingFilterOptions) FindingFilterResult {
	result := FindingFilterResult{Kept: make([]*store.Finding, 0, len(findings))}
	for index, finding := range findings {
		if finding == nil {
			continue
		}
		if reason := filterDropReason(finding); reason != "" {
			result.Dropped = append(result.Dropped, DroppedFindingDiagnostic{
				Index:  index,
				Reason: reason,
				Sample: sanitizedFindingFilterSample(finding),
				Layer:  "filter",
			})
			continue
		}
		result.Kept = append(result.Kept, finding)
	}
	return result
}

//nolint:gocyclo // Ordered hard-filter policy is easier to audit as one rule list.
func filterDropReason(finding *store.Finding) string {
	text := normalizedFindingText(finding)
	classificationText := normalizedFindingClassificationText(finding)
	primaryPath := normalizedFindingPath(finding)
	allPaths := findingEvidencePaths(finding)
	if len(allPaths) == 0 && primaryPath != "" {
		allPaths = []string{primaryPath}
	}

	if allPathsAreDocsOnly(allPaths) || containsDocsOnlyClassification(classificationText) {
		if likelySensitiveLeak(text) {
			return ""
		}
		if !containsAny(text, "generates runtime", "generated runtime", "runtime config", "executable example", "automation consumes", "loaded by production") {
			return "docs-only finding without runtime behavior"
		}
	}
	if allPathsAreTestOnly(allPaths) || containsTestOnlyClassification(classificationText) {
		if likelySensitiveLeak(text) {
			return ""
		}
		if !containsAny(text, "loaded by production", "shipped", "production fixture", "embedded fixture", "runtime loads") {
			return "test-only finding without production exposure"
		}
	}
	if containsAny(text, "dependency version", "outdated dependency", "vulnerable dependency", "vulnerable package", "cve-") {
		if !containsAny(text, "untrusted dependency resolution", "dependency confusion", "malicious package", "new dependency source") {
			return "dependency-version finding belongs in dependency scanner"
		}
	}
	if containsAny(text, "rate limit", "rate-limit", "rate limiting", "throttle") {
		if !containsAuthConcept(text) && !containsAny(text, "auth bypass", "brute force", "login", "password", "account takeover", "credential stuffing", "tenant", "cross-tenant", "billing", "cost", "quota", "security boundary", "credential", "token", "privileged") {
			return "generic rate-limit finding without security-boundary impact"
		}
	}
	if containsAny(text, "denial of service", "resource exhaustion", "generic dos", "cpu exhaustion", "memory exhaustion", "unbounded loop") {
		if !containsAny(text, "cluster", "control-plane", "control plane", "tenant", "cross-tenant", "billing", "cost", "quota", "privileged service", "namespace isolation") {
			return "generic dos/resource-exhaustion finding without concrete security impact"
		}
	}
	if isClientSidePath(primaryPath) && !hasServerSideEvidencePath(allPaths) && containsAuthConcept(text) {
		if likelyCredentialDisclosure(text) {
			return ""
		}
		if !containsAny(text, "server trusts", "server-side", "server side", "api exposes", "sensitive data", "backend trusts", "bypass server") {
			return "client-side auth finding without server-side trust boundary"
		}
	}
	if isReactPath(primaryPath) && containsAny(text, "xss", "cross-site scripting", "html injection", "script injection") {
		if !containsAny(text, "dangerouslysetinnerhtml", "innerhtml", "outerhtml", "domparser", "insertadjacenthtml", "javascript:", "unsafe html", "html parser") {
			return "react xss finding without unsafe html sink"
		}
	}
	if isShellPath(primaryPath) || containsAny(text, "command injection", "shell injection", "shell-script") {
		if containsAny(text, "command injection", "shell injection") && !containsAny(text, "attacker-controlled", "untrusted input", "untrusted env", "environment variable", "webhook", "argument", "args", "filename", "file content", "request", "user input") {
			return "shell command-injection finding without untrusted input path"
		}
	}
	if containsAny(text, "log spoof", "log injection", "non-sensitive logging", "logs non-sensitive", "operational data") {
		if likelySensitiveLeak(text) || containsAny(text, "audit", "admin", "integrity", "forg", "security boundary", "tenant") {
			return ""
		}
		if containsAny(text, "non-sensitive", "operational data", "metadata only", "log spoof", "log injection") {
			return "logging finding only covers non-sensitive operational data"
		}
	}
	if containsAny(text, "prompt injection", "tool injection", "user prompt inclusion", "untrusted prompt", "llm injection") {
		if !containsAny(text, "privileged tool", "tool use", "credential", "secret", "memory", "artifact", "task spec", "task status", "patch", "pull request", "pr creation", "git", "txtoken", "context token") {
			return "generic prompt-injection finding without privileged Orka effect"
		}
	}
	if containsAny(text, "best practice", "hardening", "defense in depth") {
		if !containsAny(text, "attacker-controlled", "trust boundary", "sensitive sink", "privileged", "exploit path") {
			return "generic best-practice hardening request without exploit path"
		}
	}
	return ""
}

func likelySensitiveLeak(text string) bool {
	return containsAny(text,
		"secret", "credential", "credentials", "token", "jwt", "api key", "api_key", "api-key", "apikey",
		"private key", "private_key", "private-key", "github pat", "github_pat", "github-pat", "g"+"hp",
		"access key", "access_key", "aws key", "aws credential", "a"+"kia", "a"+"sia",
		"txtoken", "tx token", "tx_token", "tx-token", "request token", "request_token", "password",
		"authorization bearer", "bearer token",
	)
}

func likelyCredentialDisclosure(text string) bool {
	if !likelySensitiveLeak(text) {
		return false
	}
	if containsAny(text,
		"leak", "leaks", "leaked", "expose", "exposes", "exposed", "disclosure", "disclose", "discloses", "disclosed",
		"reveal", "reveals", "revealed", "visible", "available", "embedded", "bundled", "included", "render", "renders", "rendered",
		"exfiltrate", "exfiltrates", "exfiltrated", "send", "sends", "sent", "transmit", "transmits", "transmitted", "upload", "uploads", "uploaded",
		"third-party", "third party", "analytics endpoint", "external endpoint", "url", "query string", "hash", "header", "frame", "iframe", "localstorage", "sessionstorage",
		"logs", "logged", "logging", "print", "prints", "printed", "store", "stores", "stored", "persist", "persists", "persisted",
		"committed", "hardcoded", "hard-coded", "plain text", "plaintext",
	) {
		return true
	}
	return !containsAny(text, "bypass", "bypasses", "bypassed", "gate", "check", "client state", "changing client state")
}

func normalizedFindingText(finding *store.Finding) string {
	parts := []string{
		finding.Title,
		finding.Category,
		finding.Summary,
		finding.Triage,
		finding.RootCause,
		finding.Reproduction,
		finding.Remediation,
		finding.SuggestedAction,
		finding.WhyTestsDoNotAlreadyCoverThis,
		finding.SuggestedRegressionTest,
		finding.MinimumFixScope,
	}
	for _, evidence := range finding.Evidence {
		parts = append(parts, evidence.Label, evidence.Path, evidence.Symbol)
	}
	return normalizeText(strings.Join(parts, " "))
}

func normalizedFindingClassificationText(finding *store.Finding) string {
	parts := []string{
		finding.Title,
		finding.Category,
		finding.Summary,
		finding.Triage,
	}
	return normalizeText(strings.Join(parts, " "))
}

func normalizeText(value string) string {
	value = strings.ToLower(value)
	value = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		switch r {
		case '-', '_', ':':
			return r
		default:
			return ' '
		}
	}, value)
	return strings.Join(strings.Fields(value), " ")
}

func containsAuthConcept(text string) bool {
	if containsAny(text, "authorization", "authentication", "access control", "authz", "authn") {
		return true
	}
	for field := range strings.FieldsSeq(text) {
		switch strings.Trim(field, "-_:") {
		case "auth":
			return true
		}
	}
	return false
}

func containsAny(text string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(text, normalizeText(needle)) {
			return true
		}
	}
	return false
}

func containsDocsOnlyClassification(text string) bool {
	if !containsAny(text, "docs-only", "documentation only", "only documentation", "markdown only") {
		return false
	}
	return !docsOnlyNegationPattern.MatchString(text)
}

func containsTestOnlyClassification(text string) bool {
	if !containsAny(text, "test-only", "only test", "test fixture only") {
		return false
	}
	return !testOnlyNegationPattern.MatchString(text)
}

func normalizedFindingPath(finding *store.Finding) string {
	if finding.FilePath != "" {
		return strings.TrimSpace(strings.ReplaceAll(finding.FilePath, "\\", "/"))
	}
	for _, evidence := range finding.Evidence {
		if evidence.Path != "" {
			return strings.TrimSpace(strings.ReplaceAll(evidence.Path, "\\", "/"))
		}
	}
	return ""
}

func findingEvidencePaths(finding *store.Finding) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(value string) {
		value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	add(finding.FilePath)
	for _, evidence := range finding.Evidence {
		add(evidence.Path)
	}
	sort.Strings(out)
	return out
}

func allPathsAreDocsOnly(paths []string) bool {
	if len(paths) == 0 {
		return false
	}
	for _, p := range paths {
		if !isDocsPath(p) {
			return false
		}
	}
	return true
}

func allPathsAreTestOnly(paths []string) bool {
	if len(paths) == 0 {
		return false
	}
	for _, p := range paths {
		if !isTestPath(p) {
			return false
		}
	}
	return true
}

func isDocsPath(value string) bool {
	p := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, "\\", "/")))
	if strings.HasPrefix(p, "docs/") || strings.HasPrefix(p, "website/docs/") {
		return true
	}
	ext := path.Ext(p)
	switch ext {
	case ".md", ".mdx", ".rst", ".adoc":
		return true
	case ".txt":
		return isDocsDirectoryPath(p)
	default:
		return strings.Contains(p, "/docs/") && !isRuntimeSourcePath(p)
	}
}

func isDocsDirectoryPath(p string) bool {
	return strings.HasPrefix(p, "docs/") || strings.HasPrefix(p, "website/docs/") || strings.Contains(p, "/docs/")
}

func isRuntimeSourcePath(p string) bool {
	switch path.Ext(p) {
	case ".go", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".py", ".rb", ".rs", ".java", ".kt", ".cs", ".php", ".scala", ".ex", ".exs", ".sh", ".bash", ".zsh":
		return true
	default:
		return false
	}
}

func isTestPath(value string) bool {
	p := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, "\\", "/")))
	base := path.Base(p)
	return strings.HasPrefix(p, "test/") || strings.HasPrefix(p, "tests/") || strings.HasPrefix(p, "__tests__/") ||
		strings.Contains(p, "/test/") || strings.Contains(p, "/tests/") || strings.Contains(p, "/__tests__/") ||
		strings.HasSuffix(base, "_test.go") || strings.HasSuffix(base, ".test.ts") || strings.HasSuffix(base, ".test.tsx") ||
		strings.HasSuffix(base, ".spec.ts") || strings.HasSuffix(base, ".spec.tsx") || strings.HasSuffix(base, ".test.js") ||
		strings.HasSuffix(base, ".spec.js")
}

func isClientSidePath(value string) bool {
	p := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, "\\", "/")))
	ext := path.Ext(p)
	switch ext {
	case ".tsx", ".jsx":
		return !serverSideJSPath(p)
	case ".ts", ".js", ".mjs", ".cjs":
		if serverSideJSPath(p) {
			return false
		}
		return frontendJSPath(p)
	default:
		return false
	}
}

func hasServerSideEvidencePath(paths []string) bool {
	return slices.ContainsFunc(paths, isServerSideEvidencePath)
}

func isServerSideEvidencePath(value string) bool {
	p := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, "\\", "/")))
	if p == "" || isDocsPath(p) || isTestPath(p) {
		return false
	}
	if serverSideJSPath(p) {
		return true
	}
	if frontendJSPath(p) {
		return false
	}
	switch path.Ext(p) {
	case ".go", ".py", ".rb", ".rs", ".java", ".kt", ".cs", ".php", ".scala", ".ex", ".exs":
		return true
	default:
		return false
	}
}

func frontendJSPath(p string) bool {
	return strings.HasPrefix(p, "ui/") || strings.HasPrefix(p, "web/") || strings.HasPrefix(p, "frontend/") ||
		strings.Contains(p, "/client/") || strings.Contains(p, "/browser/") || strings.Contains(p, "/components/") ||
		strings.Contains(p, "/pages/") || strings.Contains(p, "/routes/")
}

func serverSideJSPath(p string) bool {
	if strings.Contains(p, "/server/") || strings.Contains(p, "/backend/") || strings.Contains(p, "/pages/api/") || strings.Contains(p, "/app/api/") ||
		strings.HasPrefix(p, "pages/api/") || strings.HasPrefix(p, "app/api/") ||
		strings.HasPrefix(p, "server/") || strings.HasPrefix(p, "api/") || strings.HasPrefix(p, "backend/") ||
		strings.Contains(path.Base(p), "server") {
		return true
	}
	if strings.Contains(p, "/api/") && !frontendAPIPath(p) {
		return true
	}
	return strings.Contains(p, "/routes/api/") && !frontendRouteAPIPath(p)
}

func frontendAPIPath(p string) bool {
	return strings.HasPrefix(p, "ui/") || strings.HasPrefix(p, "frontend/") || strings.HasPrefix(p, "web/src/") ||
		strings.Contains(p, "/client/") || strings.Contains(p, "/browser/") || strings.Contains(p, "/components/")
}

func frontendRouteAPIPath(p string) bool {
	return frontendAPIPath(p)
}

func isReactPath(value string) bool {
	p := strings.ToLower(value)
	return strings.HasSuffix(p, ".tsx") || strings.HasSuffix(p, ".jsx")
}

func isShellPath(value string) bool {
	p := strings.ToLower(value)
	switch path.Ext(p) {
	case ".sh", ".bash", ".zsh":
		return true
	default:
		return false
	}
}

func sanitizedFindingFilterSample(finding *store.Finding) map[string]string {
	sample := map[string]string{}
	add := func(key, value string) {
		value = sanitizeDroppedSampleValue(value)
		if value != "" {
			sample[key] = value
		}
	}
	add("title", finding.Title)
	add("category", finding.Category)
	add("filePath", finding.FilePath)
	add("severity", finding.Severity)
	add("confidence", finding.Confidence)
	return sample
}

func sanitizeDroppedSampleValue(value string) string {
	value = strings.TrimSpace(strings.Join(strings.Fields(value), " "))
	if value == "" {
		return ""
	}
	value = redactSensitiveSampleText(value)
	if len(value) > maxDroppedSampleValueBytes {
		value = strings.TrimSpace(value[:maxDroppedSampleValueBytes]) + "…"
	}
	return value
}

var (
	droppedSampleSensitivePrefixPattern = regexp.MustCompile(`(?i)(?:` + "g" + `hp_|github` + `_pat_|xo` + `xb-|s` + `k-)[A-Za-z0-9_./+=:-]{8,}|(?:` + "A" + `KIA|` + "A" + `SIA)[A-Z0-9]{16}`)
	droppedSampleAssignmentPattern      = regexp.MustCompile(`(?i)((?:to` + `ken|se` + `cret|api[_-]?key|pass` + `word|credential)[^\s,;:]{0,16}[=:])[^\s,;]+`)
)

func redactSensitiveSampleText(value string) string {
	value = droppedSampleSensitivePrefixPattern.ReplaceAllString(value, "[REDACTED]")
	value = droppedSampleAssignmentPattern.ReplaceAllString(value, "$1[REDACTED]")
	redacted := strings.Fields(value)
	for i, field := range redacted {
		trimmed := strings.Trim(field, "'\"`.,;:()[]{}<>")
		lower := strings.ToLower(trimmed)
		looksSecret := len(trimmed) >= 24 && hasMixedSecretAlphabet(trimmed)
		if strings.HasPrefix(lower, "g"+"hp_") || strings.HasPrefix(lower, "github"+"_pat_") || strings.HasPrefix(lower, "xo"+"xb-") ||
			strings.HasPrefix(lower, "s"+"k-") || strings.HasPrefix(lower, "ey"+"j") || looksSecret {
			redacted[i] = strings.Replace(field, trimmed, "[REDACTED]", 1)
		}
	}
	return strings.Join(redacted, " ")
}

func hasMixedSecretAlphabet(value string) bool {
	classes := 0
	var hasLower, hasUpper, hasDigit bool
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			hasLower = true
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= '0' && r <= '9':
			hasDigit = true
		case r == '_' || r == '-' || r == '.' || r == '/':
			// common token alphabet
		default:
			return false
		}
	}
	if hasLower {
		classes++
	}
	if hasUpper {
		classes++
	}
	if hasDigit {
		classes++
	}
	return classes >= 2
}
