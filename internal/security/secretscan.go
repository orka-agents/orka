package security

import (
	"regexp"
	"sort"
	"strings"
)

type secretScan struct {
	text  string
	lower string
	full  bool

	lines          []string
	lineStarts     []int
	candidateLines []int
	candidatesDone bool
	positions      map[*keywordSet][]int
}

type keywordSet struct {
	needles []string
}

const foldUnsafeRunes = "İıſK"

const (
	secretScanTokenRuns             = 4
	secretScanMaxKeywordOccurrences = 4 << 10
)

var (
	credentialKeywords = &keywordSet{needles: []string{
		"apikey", "api_key", "api-key", "privatekey", "private_key", "private-key",
		"token", "password", "secret", "credential",
	}}
	sensitivePrefixKeywords = &keywordSet{needles: []string{"github_pat_", "ghp_", "xoxb-", "sk-", "akia", "asia"}}
	jwtKeywords             = &keywordSet{needles: []string{"eyj"}}
	authorizationKeywords   = &keywordSet{needles: []string{"authorization"}}
	txnTokenKeywords        = &keywordSet{needles: []string{"tx-token", "txn-token"}}
	cookieKeywords          = &keywordSet{needles: []string{"cookie"}}
	signedURLKeywords       = &keywordSet{needles: []string{"sig=", "signature=", "sas=", "token=", "credential="}}
	pemBlockNeedle          = "-----" + "begin "
)

type textSpan struct {
	start, end int
}

func newSecretScan(text string) *secretScan {
	scan := &secretScan{text: text}
	var b strings.Builder
	b.Grow(len(text))
	nonASCII := false
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case c >= 'A' && c <= 'Z':
			c += 'a' - 'A'
		case c >= 0x80:
			nonASCII = true
		}
		b.WriteByte(c)
	}
	scan.lower = b.String()
	scan.full = nonASCII && strings.ContainsAny(text, foldUnsafeRunes)
	return scan
}

func (s *secretScan) keywordPositions(set *keywordSet) []int {
	if positions, ok := s.positions[set]; ok {
		return positions
	}
	var positions []int
	for _, needle := range set.needles {
		for from := 0; from < len(s.lower); {
			i := strings.Index(s.lower[from:], needle)
			if i < 0 {
				break
			}
			pos := from + i
			if len(positions) == 2*secretScanMaxKeywordOccurrences {
				s.full = true
				s.positions = nil
				return nil
			}
			positions = append(positions, pos, pos+len(needle))
			from = pos + 1
		}
	}
	if len(set.needles) > 1 {
		sort.Sort(keywordPositionPairs(positions))
	}
	if s.positions == nil {
		s.positions = map[*keywordSet][]int{}
	}
	s.positions[set] = positions
	return positions
}

type keywordPositionPairs []int

func (p keywordPositionPairs) Len() int           { return len(p) / 2 }
func (p keywordPositionPairs) Less(i, j int) bool { return p[2*i] < p[2*j] }
func (p keywordPositionPairs) Swap(i, j int) {
	p[2*i], p[2*j] = p[2*j], p[2*i]
	p[2*i+1], p[2*j+1] = p[2*j+1], p[2*i+1]
}

func (s *secretScan) windows(set *keywordSet, runs int) []textSpan {
	if s.full {
		return []textSpan{{start: 0, end: len(s.text)}}
	}
	positions := s.keywordPositions(set)
	if s.full {
		return []textSpan{{start: 0, end: len(s.text)}}
	}
	if len(positions) == 0 {
		return nil
	}
	merged := []textSpan{s.spanAround(positions[0], positions[1], runs)}
	for i := 2; i < len(positions); i += 2 {
		span := s.spanAround(positions[i], positions[i+1], runs)
		last := &merged[len(merged)-1]
		if span.start <= last.end+1 {
			if span.end > last.end {
				last.end = span.end
			}
			continue
		}
		merged = append(merged, span)
	}
	return merged
}

func (s *secretScan) spanAround(start, end, runs int) textSpan {
	lineStart := strings.LastIndexByte(s.text[:start], '\n') + 1
	pos := end
	for r := 0; r < runs && pos < len(s.text); r++ {
		for pos < len(s.text) && isScanSpace(s.text[pos]) {
			pos++
		}
		for pos < len(s.text) && !isScanSpace(s.text[pos]) {
			pos++
		}
	}
	lineEnd := len(s.text)
	if i := strings.IndexByte(s.text[pos:], '\n'); i >= 0 {
		lineEnd = pos + i
	}
	return textSpan{start: lineStart, end: lineEnd}
}

func isScanSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\f' || c == '\r'
}

func (s *secretScan) matchAny(pattern *regexp.Regexp, set *keywordSet, runs int) bool {
	for _, w := range s.windows(set, runs) {
		if pattern.MatchString(s.text[w.start:w.end]) {
			return true
		}
	}
	return false
}

func (s *secretScan) findAllSubmatch(pattern *regexp.Regexp, set *keywordSet, runs int) [][]string {
	windows := s.windows(set, runs)
	matches := make([][]string, 0, len(windows))
	for _, w := range windows {
		matches = append(matches, pattern.FindAllStringSubmatch(s.text[w.start:w.end], -1)...)
	}
	return matches
}

func (s *secretScan) sensitiveValueMatch(pattern *regexp.Regexp, set *keywordSet) bool {
	for _, w := range s.windows(set, secretScanTokenRuns) {
		for _, match := range pattern.FindAllStringSubmatchIndex(s.text[w.start:w.end], -1) {
			for group := 1; 2*group+1 < len(match); group++ {
				start, end := match[2*group], match[2*group+1]
				if start < 0 {
					continue
				}
				start, end = start+w.start, end+w.start
				value := s.text[start:end]
				if !secretValuePlaceholder(value) && !secretValueIsCode(s.text, value, end) {
					return true
				}
				break
			}
		}
	}
	return false
}

func (s *secretScan) containsFolded(needle string) bool {
	if s.full {
		return strings.Contains(strings.ToLower(s.text), needle)
	}
	return strings.Contains(s.lower, needle)
}

func (s *secretScan) splitLines() []string {
	if s.lines == nil {
		s.lines = strings.Split(s.text, "\n")
	}
	return s.lines
}

func (s *secretScan) credentialKeyLines() []int {
	if s.candidatesDone {
		return s.candidateLines
	}
	s.candidatesDone = true
	lines := s.splitLines()
	if s.full {
		s.candidateLines = make([]int, len(lines))
		for i := range lines {
			s.candidateLines[i] = i
		}
		return s.candidateLines
	}
	if s.lineStarts == nil {
		s.lineStarts = make([]int, 0, len(lines))
		offset := 0
		for _, line := range lines {
			s.lineStarts = append(s.lineStarts, offset)
			offset += len(line) + 1
		}
	}
	positions := s.keywordPositions(credentialKeywords)
	for i := 0; i < len(positions); i += 2 {
		line := sort.SearchInts(s.lineStarts, positions[i]+1) - 1
		if n := len(s.candidateLines); n == 0 || s.candidateLines[n-1] != line {
			s.candidateLines = append(s.candidateLines, line)
		}
	}
	return s.candidateLines
}
