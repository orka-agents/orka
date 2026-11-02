/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package scout defines the orka.scout.proposal.v1 proposal format and the
// orka.scout.digest.v1 wrapper a scheduled scout Task emits. Everything a scout
// returns is untrusted model output derived from untrusted web content, so the
// package parses strictly, bounds every field, and verifies evidence against an
// injected checkout and link checker before anything downstream publishes it.
package scout

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// ProposalSchemaVersion identifies one scout proposal.
	ProposalSchemaVersion = "orka.scout.proposal.v1"
	// DigestSchemaVersion identifies the digest that wraps one scout run.
	DigestSchemaVersion = "orka.scout.digest.v1"
)

// Kind classifies how far a proposal is from existing behavior.
type Kind string

const (
	KindImprovement Kind = "improvement"
	KindCapability  Kind = "capability"
	KindDirection   Kind = "direction"
)

// Size is a coarse effort estimate.
type Size string

const (
	SizeS Size = "S"
	SizeM Size = "M"
	SizeL Size = "L"
)

// ConfidenceLevel is the scout's stated confidence in a proposal.
type ConfidenceLevel string

const (
	ConfidenceLow    ConfidenceLevel = "low"
	ConfidenceMedium ConfidenceLevel = "medium"
	ConfidenceHigh   ConfidenceLevel = "high"
)

// IssueState is the tracker state of a related issue.
type IssueState string

const (
	IssueStateOpen   IssueState = "open"
	IssueStateClosed IssueState = "closed"
)

// Field limits. They bound what a hostile source can push through a scout into
// an issue tracker, and keep one digest small enough to review by hand.
const (
	MaxTitleRunes       = 100
	MaxProseRunes       = 2000
	MaxReasonRunes      = 200
	MaxQuoteRunes       = 300
	MaxURLBytes         = 2048
	MaxOrkaPathBytes    = 512
	MaxEvidenceSources  = 10
	MaxEvidencePaths    = 20
	MaxEvidenceIssues   = 20
	MaxRelatedIssues    = 20
	MaxIssueNumber      = 10_000_000
	maxRelatedNoteRunes = MaxReasonRunes
)

// Proposal is one orka.scout.proposal.v1 item.
type Proposal struct {
	SchemaVersion string     `json:"schemaVersion"`
	Title         string     `json:"title"`
	Kind          Kind       `json:"kind"`
	Problem       string     `json:"problem"`
	Evidence      Evidence   `json:"evidence"`
	Sketch        string     `json:"sketch"`
	Risks         string     `json:"risks"`
	Size          Size       `json:"size"`
	Related       []Related  `json:"related"`
	Confidence    Confidence `json:"confidence"`
}

// Evidence grounds a proposal in outside sources and in Orka itself at one
// pinned commit.
type Evidence struct {
	Sources    []Source `json:"sources"`
	OrkaPaths  []string `json:"orkaPaths"`
	OrkaIssues []int    `json:"orkaIssues"`
	Commit     string   `json:"commit"`
}

// Source is one outside link, optionally with a short verbatim quote.
type Source struct {
	URL   string `json:"url"`
	Quote string `json:"quote,omitempty"`
}

// Related is an existing Orka issue found by the duplicate search.
type Related struct {
	Number int        `json:"number"`
	State  IssueState `json:"state"`
	Note   string     `json:"note,omitempty"`
}

// Confidence is a level plus one line of reasoning.
type Confidence struct {
	Level  ConfidenceLevel `json:"level"`
	Reason string          `json:"reason"`
}

var (
	conventionalTitle = regexp.MustCompile(`^(feat|fix|docs|refactor|perf|test|chore|build|ci|design)(\([a-z0-9][a-z0-9._/-]{0,30}\))?!?: \S`)
	commitPattern     = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	orkaPathPattern   = regexp.MustCompile(`^[A-Za-z0-9._][A-Za-z0-9._/-]*$`)
)

// Validate checks one proposal on its own. It does not touch the network or a
// checkout; Verify does that.
func (p *Proposal) Validate() error {
	if p == nil {
		return errors.New("proposal is required")
	}
	if p.SchemaVersion != ProposalSchemaVersion {
		return fmt.Errorf("schemaVersion must be %q", ProposalSchemaVersion)
	}
	if err := checkText("title", p.Title, MaxTitleRunes, false); err != nil {
		return err
	}
	if !conventionalTitle.MatchString(p.Title) {
		return errors.New("title must use Conventional Commit style, for example \"feat(scope): summary\"")
	}
	switch p.Kind {
	case KindImprovement, KindCapability, KindDirection:
	default:
		return fmt.Errorf("kind must be one of %q, %q or %q", KindImprovement, KindCapability, KindDirection)
	}
	for _, field := range []struct{ name, value string }{
		{"problem", p.Problem}, {"sketch", p.Sketch}, {"risks", p.Risks},
	} {
		if err := checkText(field.name, field.value, MaxProseRunes, true); err != nil {
			return err
		}
	}
	if err := p.Evidence.validate(); err != nil {
		return fmt.Errorf("evidence: %w", err)
	}
	switch p.Size {
	case SizeS, SizeM, SizeL:
	default:
		return fmt.Errorf("size must be one of %q, %q or %q", SizeS, SizeM, SizeL)
	}
	if p.Related == nil {
		return errors.New("related is required; use an empty list when the duplicate search found nothing")
	}
	if len(p.Related) > MaxRelatedIssues {
		return fmt.Errorf("related has %d entries, maximum is %d", len(p.Related), MaxRelatedIssues)
	}
	seen := make(map[int]struct{}, len(p.Related))
	for index, related := range p.Related {
		if err := checkIssueNumber(related.Number); err != nil {
			return fmt.Errorf("related[%d]: %w", index, err)
		}
		if _, duplicate := seen[related.Number]; duplicate {
			return fmt.Errorf("related[%d]: issue %d is listed twice", index, related.Number)
		}
		seen[related.Number] = struct{}{}
		if related.State != IssueStateOpen && related.State != IssueStateClosed {
			return fmt.Errorf("related[%d]: state must be %q or %q", index, IssueStateOpen, IssueStateClosed)
		}
		if related.Note != "" {
			if err := checkText(fmt.Sprintf("related[%d].note", index), related.Note, maxRelatedNoteRunes, false); err != nil {
				return err
			}
		}
	}
	switch p.Confidence.Level {
	case ConfidenceLow, ConfidenceMedium, ConfidenceHigh:
	default:
		return fmt.Errorf("confidence.level must be one of %q, %q or %q", ConfidenceLow, ConfidenceMedium, ConfidenceHigh)
	}
	return checkText("confidence.reason", p.Confidence.Reason, MaxReasonRunes, false)
}

func (e *Evidence) validate() error {
	if len(e.Sources) == 0 {
		return errors.New("sources must cite at least one outside link")
	}
	if len(e.Sources) > MaxEvidenceSources {
		return fmt.Errorf("sources has %d entries, maximum is %d", len(e.Sources), MaxEvidenceSources)
	}
	seenURLs := make(map[string]struct{}, len(e.Sources))
	for index, source := range e.Sources {
		if err := ValidateSourceURL(source.URL); err != nil {
			return fmt.Errorf("sources[%d].url: %w", index, err)
		}
		if _, duplicate := seenURLs[source.URL]; duplicate {
			return fmt.Errorf("sources[%d].url is listed twice", index)
		}
		seenURLs[source.URL] = struct{}{}
		if source.Quote != "" {
			if err := checkText(fmt.Sprintf("sources[%d].quote", index), source.Quote, MaxQuoteRunes, false); err != nil {
				return err
			}
		}
	}
	if len(e.OrkaPaths) == 0 && len(e.OrkaIssues) == 0 {
		return errors.New("cite at least one Orka path or issue")
	}
	if len(e.OrkaPaths) > MaxEvidencePaths {
		return fmt.Errorf("orkaPaths has %d entries, maximum is %d", len(e.OrkaPaths), MaxEvidencePaths)
	}
	seenPaths := make(map[string]struct{}, len(e.OrkaPaths))
	for index, orkaPath := range e.OrkaPaths {
		if err := ValidateOrkaPath(orkaPath); err != nil {
			return fmt.Errorf("orkaPaths[%d]: %w", index, err)
		}
		if _, duplicate := seenPaths[orkaPath]; duplicate {
			return fmt.Errorf("orkaPaths[%d] is listed twice", index)
		}
		seenPaths[orkaPath] = struct{}{}
	}
	if len(e.OrkaIssues) > MaxEvidenceIssues {
		return fmt.Errorf("orkaIssues has %d entries, maximum is %d", len(e.OrkaIssues), MaxEvidenceIssues)
	}
	seenIssues := make(map[int]struct{}, len(e.OrkaIssues))
	for index, number := range e.OrkaIssues {
		if err := checkIssueNumber(number); err != nil {
			return fmt.Errorf("orkaIssues[%d]: %w", index, err)
		}
		if _, duplicate := seenIssues[number]; duplicate {
			return fmt.Errorf("orkaIssues[%d]: issue %d is listed twice", index, number)
		}
		seenIssues[number] = struct{}{}
	}
	if !commitPattern.MatchString(e.Commit) {
		return errors.New("commit must be a full lowercase hexadecimal object name")
	}
	return nil
}

// ValidateSourceURL accepts only absolute https URLs with a public DNS host.
// IP literals, single-label hosts, userinfo and non-default schemes are
// rejected so a hostile page cannot route a later link check or a reader into
// a private network or a non-web handler.
func ValidateSourceURL(raw string) error {
	if raw == "" {
		return errors.New("is required")
	}
	if len(raw) > MaxURLBytes {
		return fmt.Errorf("exceeds %d bytes", MaxURLBytes)
	}
	if !utf8.ValidString(raw) {
		return errors.New("is not valid UTF-8")
	}
	for _, r := range raw {
		if r <= ' ' || r == 0x7f || r > unicode.MaxASCII {
			return errors.New("must be ASCII without spaces or control characters")
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("is not a valid URL: %w", err)
	}
	if parsed.Scheme != "https" {
		return errors.New("must use https")
	}
	if parsed.Opaque != "" || parsed.Host == "" {
		return errors.New("must be an absolute URL with a host")
	}
	if parsed.User != nil {
		return errors.New("must not contain userinfo")
	}
	if port := parsed.Port(); port != "" && port != "443" {
		return errors.New("must use the default https port")
	}
	host := strings.ToLower(parsed.Hostname())
	if net.ParseIP(host) != nil || strings.HasPrefix(host, "[") {
		return errors.New("must use a DNS host name, not an IP address")
	}
	if !strings.Contains(host, ".") || strings.HasSuffix(host, ".") {
		return errors.New("must use a fully qualified public host name")
	}
	for _, suffix := range []string{".localhost", ".local", ".internal", ".svc", ".cluster.local"} {
		if host == strings.TrimPrefix(suffix, ".") || strings.HasSuffix(host, suffix) {
			return errors.New("must not target a local or cluster-internal host")
		}
	}
	return nil
}

// ValidateOrkaPath accepts a clean, relative, repository path.
func ValidateOrkaPath(value string) error {
	if value == "" {
		return errors.New("path is required")
	}
	if len(value) > MaxOrkaPathBytes {
		return fmt.Errorf("path exceeds %d bytes", MaxOrkaPathBytes)
	}
	if !orkaPathPattern.MatchString(value) {
		return errors.New("path must be relative and use only letters, digits, '.', '_', '-' and '/'")
	}
	if path.Clean(value) != value || value == "." {
		return errors.New("path must be clean, without empty, '.' or trailing segments")
	}
	for segment := range strings.SplitSeq(value, "/") {
		if segment == ".." {
			return errors.New("path must not leave the repository")
		}
		if strings.EqualFold(segment, ".git") {
			return errors.New("path must not name repository metadata")
		}
	}
	return nil
}

func checkIssueNumber(number int) error {
	if number < 1 || number > MaxIssueNumber {
		return fmt.Errorf("issue number %d is out of range", number)
	}
	return nil
}

// checkText bounds a free-text field and rejects characters that hide or
// reorder content (control and bidirectional formatting characters) as well
// as HTML comment delimiters, which downstream filing uses for machine
// markers that a proposal must never be able to forge.
func checkText(name, value string, maxRunes int, multiline bool) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", name)
	}
	if count := utf8.RuneCountInString(value); count > maxRunes {
		return fmt.Errorf("%s has %d characters, maximum is %d", name, count, maxRunes)
	}
	for _, r := range value {
		switch {
		case r == '\n' && multiline:
		case r == '\n':
			return fmt.Errorf("%s must be a single line", name)
		case r == utf8.RuneError:
			return fmt.Errorf("%s contains a replacement character", name)
		case unicode.IsControl(r), isFormatRune(r):
			return fmt.Errorf("%s contains a control or formatting character U+%04X", name, r)
		}
	}
	if strings.Contains(value, "<!--") || strings.Contains(value, "-->") {
		return fmt.Errorf("%s must not contain HTML comment delimiters", name)
	}
	return nil
}

// isFormatRune reports Unicode format characters (category Cf), which include
// bidirectional overrides and zero-width joiners used to disguise text.
func isFormatRune(r rune) bool {
	return unicode.Is(unicode.Cf, r)
}
