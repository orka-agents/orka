package controller

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/orka-agents/orka/internal/security"
)

var (
	repositoryMonitorEvidenceOptionPattern             = regexp.MustCompile(`(?i)(?:^|[\s"'=])--(?:(?:http-|ftp-|proxy-|registry-|db-)?pass(?:word|wd)?|token|auth-token|access-token|api-key|apikey|client-secret)(=|\s+)("[^"]*"?|'[^']*'?|[^\s"']*)`)
	repositoryMonitorEvidenceShortPasswordPattern      = regexp.MustCompile(`(?i:\b(?:login|mysql|mysqladmin|mysqldump|mariadb|sshpass)\b)[^\r\n]*?\s-p(=|\s+)?("[^"]*"?|'[^']*'?|[^\s"']*)`)
	repositoryMonitorEvidenceUserPasswordPattern       = regexp.MustCompile(`(?i)\b(?:curl|wget)\b[^\r\n]*?\s(?:-u|--user)(?:=|\s+)?(["']?)[^\s"':]*:([^\s"']*)`)
	repositoryMonitorEvidenceOpenSSLPassPattern        = regexp.MustCompile(`(?i)(?:^|\s)-pass(?:in|out)?\s+(["']?)pass:([^\s"']*)`)
	repositoryMonitorEvidenceAuthFieldPattern          = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])(?:[A-Za-z0-9]+[_.-])*(?:_?auth|client[-_]key[-_]data|client[-_]certificate[-_]data)["']?\s*[:=]\s*(["']?[^\s"',;}\]]*)`)
	repositoryMonitorEvidenceAuthorizationPattern      = regexp.MustCompile(`(?i)\b(?:proxy-)?authorization(?:\\?["'])?\s*[:=]\s*(?:\[\s*)?(?:\\?["'])?(?:([A-Za-z][A-Za-z0-9_-]*)[ \t]+)?([^\s"'\],}\\]*)`)
	repositoryMonitorEvidenceAuthorizationParamPattern = regexp.MustCompile(`(?i)\b(?:proxy-)?authorization(?:\\?["'])?\s*[:=]\s*(?:\[\s*)?(?:\\?["'])?[A-Za-z][A-Za-z0-9_-]*[ \t]+[A-Za-z0-9._~+/-]+[ \t]*=[ \t]*((?:\\?["'])?[^\s"',\\]*)`)
	repositoryMonitorEvidenceVariableReferencePattern  = regexp.MustCompile(`^(?:\$(?:[A-Za-z_][A-Za-z0-9_]*|\{\{[^}]*(?:\}\})?|\{[^}\s]*\}?|\(.*)|%[A-Za-z_][A-Za-z0-9_]*%)$`)
	repositoryMonitorEvidenceQuotedMentionPattern      = regexp.MustCompile("(?i)\\b(?:token|password|passwd|secret|api[-_ ]?key|credentials?)s?\\s+([\"'`][^\"'`\\s]*)")
	repositoryMonitorEvidenceQuotedCredentialPattern   = regexp.MustCompile(`(?i)[A-Za-z0-9_.-]*(?:api[-_]?key|token|secret|passw(?:or)?d|credentials?|private[-_]?key|client[-_]key[-_]data)[A-Za-z0-9_.-]*["']?\s*[:=]\s*(["'])`)
)

const repositoryMonitorEvidenceAddMaskCommand = "::add-mask::"

const repositoryMonitorEvidenceQuotedScalarMaxLines = 64

func repositoryMonitorEvidenceCredentialLine(line string) bool {
	if strings.TrimSpace(line) == "" {
		return false
	}
	if strings.Contains(strings.ToLower(line), repositoryMonitorEvidenceAddMaskCommand) {
		return true
	}
	for _, check := range []struct {
		pattern *regexp.Regexp
		secret  func(string) bool
	}{
		{repositoryMonitorEvidenceOptionPattern, repositoryMonitorEvidenceOptionValue},
		{repositoryMonitorEvidenceShortPasswordPattern, repositoryMonitorEvidenceOptionValue},
		{repositoryMonitorEvidenceUserPasswordPattern, repositoryMonitorEvidenceLiteralValue},
		{repositoryMonitorEvidenceOpenSSLPassPattern, repositoryMonitorEvidenceLiteralValue},
		{repositoryMonitorEvidenceAuthFieldPattern, repositoryMonitorEvidenceEncodedValue},
		{repositoryMonitorEvidenceAuthorizationParamPattern, func(v string) bool { return repositoryMonitorEvidenceOpaqueValue(v, 8) }},
		{repositoryMonitorEvidenceQuotedMentionPattern, func(v string) bool {
			return repositoryMonitorEvidenceOpaqueValue(v, 12) && strings.ContainsAny(v, "0123456789") &&
				strings.ContainsFunc(v, func(r rune) bool { return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' })
		}},
	} {
		for _, match := range check.pattern.FindAllStringSubmatch(line, -1) {
			if check.secret(strings.Join(match[1:], "")) {
				return true
			}
		}
	}
	for _, match := range repositoryMonitorEvidenceAuthorizationPattern.FindAllStringSubmatch(line, -1) {
		if repositoryMonitorEvidenceOpaqueValue(match[1], 16) || repositoryMonitorEvidenceEncodedValue(match[2]) {
			return true
		}
	}
	return false
}

func repositoryMonitorEvidenceOptionValue(value string) bool {
	if trimmed := strings.TrimLeft(value, " \t"); trimmed != value {
		return repositoryMonitorEvidenceArgumentValue(trimmed)
	}
	return repositoryMonitorEvidenceLiteralValue(strings.TrimPrefix(value, "="))
}

func repositoryMonitorEvidenceArgumentValue(value string) bool {
	return !strings.HasPrefix(value, "-") && repositoryMonitorEvidenceLiteralValue(value)
}

func repositoryMonitorEvidenceLiteralValue(value string) bool {
	value = strings.TrimPrefix(value, `\`)
	singleQuoted := strings.HasPrefix(value, "'")
	value = strings.TrimSpace(strings.TrimRight(strings.Trim(value, `"'`), `\`))
	if value == "" || strings.Trim(value, "*") == "" || value == repositoryMonitorEvidenceRedactedLine {
		return false
	}
	return singleQuoted || !repositoryMonitorEvidenceVariableReferencePattern.MatchString(value)
}

func repositoryMonitorEvidenceOpaqueValue(value string, minLen int) bool {
	bare := strings.TrimRight(strings.TrimLeft(value, `"'\`+"`"), `.,;:)\`)
	if len(bare) < minLen || !repositoryMonitorEvidenceArgumentValue(value) {
		return false
	}
	return len(bare) >= 20 || strings.ContainsAny(bare, "0123456789=+/_~.$")
}

func repositoryMonitorEvidenceEncodedValue(value string) bool {
	if repositoryMonitorEvidenceOpaqueValue(value, 8) {
		return true
	}
	bare := strings.TrimRight(strings.TrimLeft(value, `"'\`+"`"), `.,;:)\`)
	return len(bare) >= 12 && repositoryMonitorEvidenceArgumentValue(value) &&
		strings.ContainsFunc(bare, unicode.IsUpper) && strings.ContainsFunc(bare, unicode.IsLower)
}

func repositoryMonitorEvidenceClosingQuote(value string, quote byte) int {
	for i := 0; i < len(value); i++ {
		switch {
		case quote == '"' && value[i] == '\\':
			i++
		case value[i] == quote && quote == '\'' && i+1 < len(value) && value[i+1] == '\'':
			i++
		case value[i] == quote:
			return i
		}
	}
	return -1
}

func repositoryMonitorEvidenceOpensQuotedCredential(line string) byte {
	for _, match := range repositoryMonitorEvidenceQuotedCredentialPattern.FindAllStringSubmatchIndex(line, -1) {
		quote := line[match[2]]
		if repositoryMonitorEvidenceClosingQuote(line[match[3]:], quote) < 0 {
			return quote
		}
	}
	return 0
}

func repositoryMonitorEvidenceDiffMarker(line string) bool {
	return line != "" && (line[0] == '+' || line[0] == '-' || line[0] == ' ')
}

type repositoryMonitorEvidenceMultilineTracker struct {
	sides [2]repositoryMonitorEvidenceMultilineSide
}

type repositoryMonitorEvidenceMultilineSide struct {
	inBlock               bool
	rawIndent, diffIndent int
	quote                 byte
	quoteLines            int
}

func (s *repositoryMonitorEvidenceMultilineSide) continues(line string) bool {
	return s.rawIndent >= 0 && repositoryMonitorEvidenceIndent(line) > s.rawIndent ||
		s.diffIndent >= 0 && repositoryMonitorEvidenceDiffMarker(line) &&
			(strings.TrimSpace(line[1:]) == "" || repositoryMonitorEvidenceIndent(line[1:]) > s.diffIndent)
}

func newRepositoryMonitorEvidenceMultilineTracker() *repositoryMonitorEvidenceMultilineTracker {
	return &repositoryMonitorEvidenceMultilineTracker{}
}

func repositoryMonitorEvidenceDiffSides(line string) []int {
	switch {
	case strings.HasPrefix(line, "-"):
		return []int{0}
	case strings.HasPrefix(line, "+"):
		return []int{1}
	}
	return []int{0, 1}
}

func repositoryMonitorEvidenceMultilineKey(line string) bool {
	return security.SensitiveYAMLBlockHeader(line) || security.SensitiveYAMLEmptyValueKey(line)
}

func (t *repositoryMonitorEvidenceMultilineTracker) withhold(line string) bool {
	blank := strings.TrimSpace(line) == ""
	var rawKey, diffKey bool
	var quote byte
	if !blank {
		rawKey = repositoryMonitorEvidenceMultilineKey(line)
		diffKey = repositoryMonitorEvidenceDiffMarker(line) && repositoryMonitorEvidenceMultilineKey(line[1:])
		quote = repositoryMonitorEvidenceOpensQuotedCredential(line)
	}
	withheld := false
	for _, i := range repositoryMonitorEvidenceDiffSides(line) {
		side := &t.sides[i]
		switch {
		case side.quote != 0:
			withheld = true
			if quote != 0 {
				*side = repositoryMonitorEvidenceMultilineSide{quote: quote}
				continue
			}
			side.quoteLines++
			if repositoryMonitorEvidenceClosingQuote(line, side.quote) >= 0 || side.quoteLines >= repositoryMonitorEvidenceQuotedScalarMaxLines {
				*side = repositoryMonitorEvidenceMultilineSide{}
			}
			continue
		case blank:
			continue
		case side.inBlock && side.continues(line):
			withheld = true
			continue
		}
		*side = repositoryMonitorEvidenceMultilineSide{}
		switch {
		case rawKey || diffKey:
			side.inBlock, side.rawIndent, side.diffIndent = true, -1, -1
			if rawKey {
				side.rawIndent = repositoryMonitorEvidenceIndent(line)
			}
			if diffKey {
				side.diffIndent = repositoryMonitorEvidenceIndent(line[1:])
			}
			withheld = true
		case quote != 0:
			side.quote = quote
			withheld = true
		}
	}
	return withheld
}
