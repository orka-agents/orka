package controller

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/orka-agents/orka/internal/security"
)

// CI-evidence credential heuristics. The shared redactor recognizes values
// that are unambiguously credentials; CI text also carries credentials in
// shapes that are only credential-like in context, such as a command's
// password option in a shell trace, a registry config's auth field, or a
// workflow command that masks a value. Evidence text may lose some ordinary
// CI text to these checks, so each one withholds the whole line without
// trying to decode or partly keep its value.
var (
	// A long command-line option whose value is a credential, such as
	// docker login --password, wget --http-password, or vault --token. The
	// value follows "=" or whitespace; --password-stdin and similar options
	// take no value. A quoted value is taken through its closing quote, so
	// one with spaces is checked whole. docker build --secret takes a file
	// reference, not a secret, and is not listed.
	repositoryMonitorEvidenceOptionPattern = regexp.MustCompile(`(?i)(?:^|[\s"'=])--(?:(?:http-|ftp-|proxy-|registry-|db-)?pass(?:word|wd)?|token|auth-token|access-token|api-key|apikey|client-secret)(=|\s+)("[^"]*"?|'[^']*'?|[^\s"']*)`)
	// A short -p password option of a login command or a database client,
	// as in docker login -p VALUE, mysql -pVALUE, or sshpass -p VALUE. The
	// option is case-sensitive: -P is a port.
	repositoryMonitorEvidenceShortPasswordPattern = regexp.MustCompile(`(?i:\b(?:login|mysql|mysqladmin|mysqldump|mariadb|sshpass)\b)[^\r\n]*?\s-p(=|\s+)?("[^"]*"?|'[^']*'?|[^\s"']*)`)
	// curl or wget basic credentials, as in curl -u user:VALUE.
	repositoryMonitorEvidenceUserPasswordPattern = regexp.MustCompile(`(?i)\b(?:curl|wget)\b[^\r\n]*?\s(?:-u|--user)(?:=|\s+)?(["']?)[^\s"':]*:([^\s"']*)`)
	// OpenSSL's literal pass phrase argument, as in -passin pass:VALUE.
	repositoryMonitorEvidenceOpenSSLPassPattern = regexp.MustCompile(`(?i)(?:^|\s)-pass(?:in|out)?\s+(["']?)pass:([^\s"']*)`)
	// An auth field or other credential key whose value is base64 or
	// opaque: an .npmrc _auth line, a Docker config "auth" entry, a
	// NEXUS_AUTH= environment assignment, or kubeconfig client key and
	// certificate data.
	repositoryMonitorEvidenceAuthFieldPattern = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])(?:[A-Za-z0-9]+[_.-])*(?:_?auth|client[-_]key[-_]data|client[-_]certificate[-_]data)["']?\s*[:=]\s*(["']?[^\s"',;}\]]*)`)
	// An Authorization header with a value the shared redactor does not
	// recognize, such as an unlisted scheme or a raw key.
	repositoryMonitorEvidenceAuthorizationPattern = regexp.MustCompile(`(?i)\b(?:proxy-)?authorization(?:\\?["'])?\s*[:=]\s*(?:\[\s*)?(?:\\?["'])?(?:([A-Za-z][A-Za-z0-9_-]*)[ \t]+)?([^\s"'\],}\\]*)`)
	// An Authorization header's scheme followed by an auth-param, as in
	// Token token = "VALUE", or token=\"VALUE\" in a JSON string.
	repositoryMonitorEvidenceAuthorizationParamPattern = regexp.MustCompile(`(?i)\b(?:proxy-)?authorization(?:\\?["'])?\s*[:=]\s*(?:\[\s*)?(?:\\?["'])?[A-Za-z][A-Za-z0-9_-]*[ \t]+[A-Za-z0-9._~+/-]+[ \t]*=[ \t]*((?:\\?["'])?[^\s"',\\]*)`)
	// An unexpanded shell, Actions, or Windows variable reference.
	repositoryMonitorEvidenceVariableReferencePattern = regexp.MustCompile(`^(?:\$(?:[A-Za-z_][A-Za-z0-9_]*|\{\{[^}]*(?:\}\})?|\{[^}\s]*\}?|\(.*)|%[A-Za-z_][A-Za-z0-9_]*%)$`)
	// A credential word followed by a quoted value, as in a tool's
	// diagnostic: got token "VALUE".
	repositoryMonitorEvidenceQuotedMentionPattern = regexp.MustCompile("(?i)\\b(?:token|password|passwd|secret|api[-_ ]?key|credentials?)s?\\s+([\"'`][^\"'`\\s]*)")
	// A credential key whose quoted value opens on the line; the value may
	// continue on the following lines until its closing quote.
	repositoryMonitorEvidenceQuotedCredentialPattern = regexp.MustCompile(`(?i)[A-Za-z0-9_.-]*(?:api[-_]?key|token|secret|passw(?:or)?d|credentials?|private[-_]?key|client[-_]key[-_]data)[A-Za-z0-9_.-]*["']?\s*[:=]\s*(["'])`)
)

// repositoryMonitorEvidenceAddMaskCommand is the GitHub Actions workflow
// command that masks a value. A shell trace (set -x) prints the command,
// value included, before the runner masks it.
const repositoryMonitorEvidenceAddMaskCommand = "::add-mask::"

// repositoryMonitorEvidenceQuotedScalarMaxLines bounds how many lines a
// credential's unclosed quoted value is followed through before the tracker
// stops trusting any later line to end it: past the limit, every remaining
// line is withheld.
const repositoryMonitorEvidenceQuotedScalarMaxLines = 64

// repositoryMonitorEvidenceCredentialLine reports whether a line of CI text
// carries a credential in a shape only the CI-evidence layer recognizes.
// line is the unredacted line with control and format runes stripped.
func repositoryMonitorEvidenceCredentialLine(line string) bool {
	if strings.TrimSpace(line) == "" {
		return false
	}
	if strings.Contains(strings.ToLower(line), repositoryMonitorEvidenceAddMaskCommand) {
		return true
	}
	for _, check := range []struct {
		pattern *regexp.Regexp
		// secret reports whether the value the pattern's groups capture,
		// joined, is a credential. A value keeps its opening quote.
		secret func(string) bool
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
	// The word after an Authorization header's separator is a scheme or,
	// with no scheme, the credential itself.
	for _, match := range repositoryMonitorEvidenceAuthorizationPattern.FindAllStringSubmatch(line, -1) {
		if repositoryMonitorEvidenceOpaqueValue(match[1], 16) || repositoryMonitorEvidenceEncodedValue(match[2]) {
			return true
		}
	}
	return false
}

// repositoryMonitorEvidenceOptionValue reports whether an option's value,
// with the separator before it, is a credential: a value attached to its
// option, directly or after "=", is literal even if it starts with a
// hyphen, while a value after whitespace may be the next option.
func repositoryMonitorEvidenceOptionValue(value string) bool {
	if trimmed := strings.TrimLeft(value, " \t"); trimmed != value {
		return repositoryMonitorEvidenceArgumentValue(trimmed)
	}
	return repositoryMonitorEvidenceLiteralValue(strings.TrimPrefix(value, "="))
}

// repositoryMonitorEvidenceArgumentValue reports whether a credential
// option's value, with its opening quote if it has one, is a value rather
// than the next option or a reference to one, as
// repositoryMonitorEvidenceLiteralValue reports. An unquoted value that
// starts with a hyphen is the next option.
func repositoryMonitorEvidenceArgumentValue(value string) bool {
	return !strings.HasPrefix(value, "-") && repositoryMonitorEvidenceLiteralValue(value)
}

// repositoryMonitorEvidenceLiteralValue reports whether a credential value,
// with its opening quote if it has one, is a value rather than a reference
// to one: not empty, not a mask such as ***, and not an unexpanded variable
// such as $PASSWORD, ${{ secrets.X }}, or %PASSWORD%. A single-quoted value
// is literal: '$PASSWORD' is the value itself, as a shell trace prints it.
// It is used where a value cannot be another option, such as the password
// after user: in curl -u user:password.
func repositoryMonitorEvidenceLiteralValue(value string) bool {
	value = strings.TrimPrefix(value, `\`)
	singleQuoted := strings.HasPrefix(value, "'")
	value = strings.TrimSpace(strings.TrimRight(strings.Trim(value, `"'`), `\`))
	if value == "" || strings.Trim(value, "*") == "" || value == repositoryMonitorEvidenceRedactedLine {
		return false
	}
	return singleQuoted || !repositoryMonitorEvidenceVariableReferencePattern.MatchString(value)
}

// repositoryMonitorEvidenceOpaqueValue reports whether a credential field's
// value, with its opening quote if it has one, looks like a credential
// rather than a word: a value repositoryMonitorEvidenceArgumentValue accepts
// that is at least minLen bytes once quotes and trailing punctuation are
// dropped, with a digit, a base64 or token symbol, or at least 20 bytes. An
// ordinary word such as "required" or "disabled" is kept.
func repositoryMonitorEvidenceOpaqueValue(value string, minLen int) bool {
	bare := strings.TrimRight(strings.TrimLeft(value, `"'\`+"`"), `.,;:)\`)
	if len(bare) < minLen || !repositoryMonitorEvidenceArgumentValue(value) {
		return false
	}
	return len(bare) >= 20 || strings.ContainsAny(bare, "0123456789=+/_~.$")
}

// repositoryMonitorEvidenceEncodedValue reports whether a credential
// field's value is opaque, as repositoryMonitorEvidenceOpaqueValue reports
// with a minimum of 8 bytes, or mixed-case base64 of at least 12 bytes,
// which needs no digit or padding: dXNlcjpzZWNyZXQh encodes user:secret!.
func repositoryMonitorEvidenceEncodedValue(value string) bool {
	if repositoryMonitorEvidenceOpaqueValue(value, 8) {
		return true
	}
	bare := strings.TrimRight(strings.TrimLeft(value, `"'\`+"`"), `.,;:)\`)
	return len(bare) >= 12 && repositoryMonitorEvidenceArgumentValue(value) &&
		strings.ContainsFunc(bare, unicode.IsUpper) && strings.ContainsFunc(bare, unicode.IsLower)
}

// repositoryMonitorEvidenceClosingQuote returns the index of the quote that
// closes a value quoted with quote, or -1: a backslash escapes the next byte
// of a double-quoted value, and a doubled quote stands for one inside a
// single-quoted value.
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

// repositoryMonitorEvidenceOpensQuotedCredential returns the quote that
// opens a credential key's value on line without closing it, or 0.
func repositoryMonitorEvidenceOpensQuotedCredential(line string) byte {
	for _, match := range repositoryMonitorEvidenceQuotedCredentialPattern.FindAllStringSubmatchIndex(line, -1) {
		quote := line[match[2]]
		if repositoryMonitorEvidenceClosingQuote(line[match[3]:], quote) < 0 {
			return quote
		}
	}
	return 0
}

// repositoryMonitorEvidenceDiffMarker reports whether line starts with a
// unified diff line marker: an added, removed, or context line.
func repositoryMonitorEvidenceDiffMarker(line string) bool {
	return line != "" && (line[0] == '+' || line[0] == '-' || line[0] == ' ')
}

// repositoryMonitorEvidenceMultilineTracker follows, line by line, the
// multi-line values of credential keys, whose continuation lines are each
// too short or too plain for any check to recognize:
//
//   - a YAML block scalar (password: |, with any chomping or indentation
//     indicator, tag, or anchor) or a key with no value on its line
//     (password:), through the lines indented deeper than the key;
//   - a quoted value that opens on the key's line (password: "first), through
//     the line with its closing quote. A value still open after
//     repositoryMonitorEvidenceQuotedScalarMaxLines lines leaves the tracker
//     unsure where it ends, so it withholds every remaining line;
//   - a PEM block, from the line with its BEGIN marker through the line with
//     the END marker of the same label (any END marker when the BEGIN line
//     does not show its label), blank lines included, on each diff side;
//   - after a line too long to keep whole (see openCut), the lines indented
//     deeper than it.
//
// Keys are also recognized behind a unified diff marker (+, -, or a space),
// as CI tools print diffs of configuration: such a block continues through
// diff lines indented deeper behind their marker. The tracker keeps its
// state between calls, so a stream can be scanned one line at a time; the
// scan is linear and independent of the check budget.
type repositoryMonitorEvidenceMultilineTracker struct {
	// sides holds the open value on each side of a diff: the removed side
	// (index 0) and the added side (index 1). Text that is not a diff is on
	// both sides, so a removed line that ends a value cannot expose the
	// added lines that replace it.
	sides [2]repositoryMonitorEvidenceMultilineSide
	// pems holds the PEM block open on each diff side, apart from sides so
	// that a line ending a YAML or quoted value leaves it open.
	pems [2]repositoryMonitorEvidencePEMSide
	// exhausted means a quoted value stayed open past
	// repositoryMonitorEvidenceQuotedScalarMaxLines: no later line is
	// trusted to end it, so every line is withheld.
	exhausted bool
}

// repositoryMonitorEvidencePEMSide is the PEM block open on one side of a
// diff.
type repositoryMonitorEvidencePEMSide struct {
	// open means a block is open; label is its BEGIN marker's lower-cased
	// label, or empty when the marker did not show it.
	open  bool
	label string
}

// track follows the side's PEM block through lower, the next lower-cased
// line, and reports whether the line is part of one: inside a block or
// holding a BEGIN or END marker of one. An END marker whose label differs
// from its BEGIN marker's does not end the block.
func (p *repositoryMonitorEvidencePEMSide) track(lower string) bool {
	inside := p.open
	for pos := 0; pos < len(lower); {
		if p.open {
			k := strings.Index(lower[pos:], repositoryMonitorEvidencePEMEnd)
			if k < 0 {
				break
			}
			pos += k + len(repositoryMonitorEvidencePEMEnd)
			if label := repositoryMonitorEvidencePEMLabel(lower[pos:]); p.label == "" || label == p.label {
				p.open, p.label = false, ""
			}
			continue
		}
		k := strings.Index(lower[pos:], repositoryMonitorEvidencePEMBegin)
		if k < 0 {
			break
		}
		pos += k + len(repositoryMonitorEvidencePEMBegin)
		p.open, p.label, inside = true, repositoryMonitorEvidencePEMLabel(lower[pos:]), true
	}
	return inside
}

// repositoryMonitorEvidenceMultilineSide is the multi-line value open on one
// side of a diff.
type repositoryMonitorEvidenceMultilineSide struct {
	// inBlock means a block or plain value is open; rawIndent and
	// diffIndent are the indentation of its key line without and behind its
	// diff marker, or -1 when the key was not recognized that way.
	inBlock               bool
	rawIndent, diffIndent int
	// quote is the open quoted value's quote, or 0; quoteLines counts its
	// continuation lines.
	quote      byte
	quoteLines int
}

// continues reports whether a non-blank line continues the side's open
// block: it is indented deeper than the key, or, for a key behind a diff
// marker, it is a diff line indented deeper behind its marker.
func (s *repositoryMonitorEvidenceMultilineSide) continues(line string) bool {
	return s.rawIndent >= 0 && repositoryMonitorEvidenceIndent(line) > s.rawIndent ||
		s.diffIndent >= 0 && repositoryMonitorEvidenceDiffMarker(line) &&
			(strings.TrimSpace(line[1:]) == "" || repositoryMonitorEvidenceIndent(line[1:]) > s.diffIndent)
}

func newRepositoryMonitorEvidenceMultilineTracker() *repositoryMonitorEvidenceMultilineTracker {
	return &repositoryMonitorEvidenceMultilineTracker{}
}

// repositoryMonitorEvidenceDiffSides returns the diff sides line is on: a
// removed line only the first, an added line only the second, and any other
// line both.
func repositoryMonitorEvidenceDiffSides(line string) []int {
	switch {
	case strings.HasPrefix(line, "-"):
		return []int{0}
	case strings.HasPrefix(line, "+"):
		return []int{1}
	}
	return []int{0, 1}
}

// repositoryMonitorEvidencePEMSides returns the diff sides lower, a
// lower-cased line, is on for PEM tracking: a line that starts with a PEM
// marker is the marker on both sides, not a removed line.
func repositoryMonitorEvidencePEMSides(lower string) []int {
	if strings.HasPrefix(lower, repositoryMonitorEvidencePEMBegin) || strings.HasPrefix(lower, repositoryMonitorEvidencePEMEnd) {
		return []int{0, 1}
	}
	return repositoryMonitorEvidenceDiffSides(lower)
}

// repositoryMonitorEvidenceMultilineKey reports whether line is a credential
// key whose value continues on the following lines.
func repositoryMonitorEvidenceMultilineKey(line string) bool {
	return security.SensitiveYAMLBlockHeader(line) || security.SensitiveYAMLEmptyValueKey(line)
}

// open reports that the text read so far ends inside a value the tracker
// withholds without a clear end: a quoted value, a PEM block, or any value
// once the tracker is exhausted. A reader that stops here cannot tell how
// much of the value it did not see.
func (t *repositoryMonitorEvidenceMultilineTracker) open() bool {
	for i := range t.sides {
		if t.sides[i].quote != 0 || t.pems[i].open {
			return true
		}
	}
	return t.exhausted
}

// repositoryMonitorEvidencePEMLabel returns the lower-cased label of the PEM
// marker whose label starts at rest, such as "rsa private key", or empty
// when rest does not show the marker's closing dashes.
func repositoryMonitorEvidencePEMLabel(rest string) string {
	label, _, ok := strings.Cut(rest, "-----")
	if !ok {
		return ""
	}
	return strings.TrimSpace(label)
}

// openCut follows the lines after line, which a reader keeps only the head
// of: the tail it drops may hold a credential key whose value continues on
// the next lines, so the lines indented deeper than it, or deeper behind its
// diff marker, are withheld as that value until a line that is not. A side
// already following a value keeps it. The reader passes the whole line to
// withhold first, so quoted values and PEM blocks the tail opens are
// followed as on any line.
func (t *repositoryMonitorEvidenceMultilineTracker) openCut(line string) {
	if strings.TrimSpace(line) == "" {
		return
	}
	for _, i := range repositoryMonitorEvidenceDiffSides(line) {
		side := &t.sides[i]
		if side.quote != 0 || side.inBlock {
			continue
		}
		side.inBlock, side.rawIndent, side.diffIndent = true, repositoryMonitorEvidenceIndent(line), -1
		if repositoryMonitorEvidenceDiffMarker(line) {
			side.diffIndent = repositoryMonitorEvidenceIndent(line[1:])
		}
	}
}

// withhold reports whether line, the next line of the text, belongs to a
// credential's multi-line value, or opens one. line is the unredacted line
// with control and format runes stripped. Each diff side line is on is
// updated independently: a context line can continue a value on one side
// and open another on the other. A blank line inside a block is not
// withheld, but does not end the block; a blank line inside a PEM block is
// withheld.
func (t *repositoryMonitorEvidenceMultilineTracker) withhold(line string) bool {
	if t.exhausted {
		return true
	}
	blank := strings.TrimSpace(line) == ""
	lower := strings.ToLower(line)
	// A line inside a block on either side is withheld: a base64 body line
	// can start with a diff marker's character.
	inPEM := t.pems[0].open || t.pems[1].open
	for _, i := range repositoryMonitorEvidencePEMSides(lower) {
		if t.pems[i].track(lower) {
			inPEM = true
		}
	}
	var rawKey, diffKey bool
	var quote byte
	if !blank {
		rawKey = repositoryMonitorEvidenceMultilineKey(line)
		diffKey = repositoryMonitorEvidenceDiffMarker(line) && repositoryMonitorEvidenceMultilineKey(line[1:])
		quote = repositoryMonitorEvidenceOpensQuotedCredential(line)
	}
	withheld := inPEM
	for _, i := range repositoryMonitorEvidenceDiffSides(line) {
		side := &t.sides[i]
		switch {
		case side.quote != 0:
			withheld = true
			// A line that opens another credential's quoted value, such
			// as the added line of a diff that replaces the value's first
			// line, starts over rather than closing the value.
			if quote != 0 {
				*side = repositoryMonitorEvidenceMultilineSide{quote: quote}
				continue
			}
			side.quoteLines++
			switch {
			case repositoryMonitorEvidenceClosingQuote(line, side.quote) >= 0:
				*side = repositoryMonitorEvidenceMultilineSide{}
			case side.quoteLines >= repositoryMonitorEvidenceQuotedScalarMaxLines:
				// Clearing the value here would expose its later lines
				// once its key line is out of view.
				t.exhausted = true
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
