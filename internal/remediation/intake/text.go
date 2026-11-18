package intake

import (
	"errors"
	"io"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/orka-agents/orka/internal/redact"
	"golang.org/x/net/html"
)

const redactionMarker = "[REDACTED]"

var (
	urlPattern        = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s<>"'\x60]+`)
	ownerPattern      = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
	emailPattern      = regexp.MustCompile("[A-Za-z0-9.!#$%&'*+/=?^_`{|}~-]+@[A-Za-z0-9.-]+")
	guidPattern       = regexp.MustCompile(`(?i)[0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12}`)
	privateKeyPattern = regexp.MustCompile(`(?s)-----BEGIN (?:[A-Z0-9]+ )*PRIVATE KEY-----.*?(?:-----END (?:[A-Z0-9]+ )*PRIVATE KEY-----|$)`)
	bearerPattern     = regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[A-Za-z0-9._~+/=-]+`)
	accessKeyPattern  = regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)
	adminLinePattern  = regexp.MustCompile(`(?i)^[ \t]*(?:admin(?:istrative)?|customer|tenant|subscription|contact|reporter|assignee|owner|credentials?)(?:[ _-]*(?:id|name|email|address|phone))?[ \t]*(?::|=|\t)`)
)

func canonicalRepository(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || !strings.HasPrefix(raw, "https://github.com/") || parsed.Scheme != "https" || parsed.Host != "github.com" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery ||
		parsed.Fragment != "" || parsed.RawFragment != "" || parsed.RawPath != "" ||
		strings.ContainsAny(raw, "?#%") || scrubText(raw) != raw {
		return false
	}
	parts := strings.Split(parsed.Path, "/")
	return len(parts) == 3 && parts[0] == "" && ownerPattern.MatchString(parts[1]) &&
		repositoryPattern.MatchString(parts[2]) && parts[2] != "." && parts[2] != ".." &&
		!strings.HasSuffix(strings.ToLower(parts[2]), ".git")
}

func trimURLPunctuation(value string) string {
	return strings.TrimRight(value, ",;!)]}")
}

type htmlFrame struct {
	tag    string
	hidden bool
	code   bool
}

func normalizeText(raw string) (string, string, []string, error) {
	var flags []string
	clean := withoutControls(raw)
	controlsRemoved := clean != raw
	text, prose, markup, err := htmlText(normalizeWhitespace(clean))
	if err != nil {
		return "", "", nil, err
	}
	cleanText, cleanProse := withoutControls(text), withoutControls(prose)
	if controlsRemoved || cleanText != text || cleanProse != prose {
		flags = append(flags, "control_characters_removed")
	}
	text, prose = normalizeWhitespace(cleanText), normalizeWhitespace(cleanProse)
	if markup {
		flags = append(flags, "html_markup_removed")
	}
	withoutAdmin := redactAdministrativeLines(text)
	if withoutAdmin != text {
		flags = append(flags, "administrative_text_redacted")
	}
	scrubbed := scrubText(withoutAdmin)
	if scrubbed != withoutAdmin {
		flags = append(flags, "sensitive_text_redacted")
	}
	return strings.TrimSpace(scrubbed), scrubText(redactAdministrativeLines(prose)), flags, nil
}

func withoutControls(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) || (unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t') {
			return -1
		}
		return r
	}, text)
}

func normalizeWhitespace(text string) string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	return strings.Map(func(r rune) rune {
		if r == '\u2028' || r == '\u2029' {
			return '\n'
		}
		if unicode.IsSpace(r) && r != '\n' && r != '\t' {
			return ' '
		}
		return r
	}, text)
}

func htmlText(raw string) (string, string, bool, error) {
	tokenizer := html.NewTokenizer(strings.NewReader(raw))
	tokenizer.SetMaxBuf(maxRawFieldBytes)
	var text, prose strings.Builder
	var stack []htmlFrame
	markup := false
	current := func() htmlFrame {
		if len(stack) == 0 {
			return htmlFrame{}
		}
		return stack[len(stack)-1]
	}
	boundary := func(tag string) {
		separator := ""
		switch tag {
		case "p", "div", "br", "hr", "li", "ul", "ol", "pre", "blockquote",
			"h1", "h2", "h3", "h4", "h5", "h6", "tr", "table", "section", "article":
			separator = "\n"
		case "td", "th":
			separator = "\t"
		}
		if !current().hidden {
			text.WriteString(separator)
			if !current().code {
				prose.WriteString(separator)
			}
		}
	}
	for tokens := 0; ; tokens++ {
		if tokens > 32768 {
			return "", "", false, errors.New("technical HTML exceeds the token count limit")
		}
		switch tokenizer.Next() {
		case html.ErrorToken:
			if !errors.Is(tokenizer.Err(), io.EOF) {
				return "", "", false, errors.New("technical HTML exceeds parser limits")
			}
			return text.String(), prose.String(), markup, nil
		case html.TextToken:
			if !current().hidden {
				value := tokenizer.Token().Data
				text.WriteString(value)
				if !current().code {
					prose.WriteString(value)
				}
			}
		case html.StartTagToken, html.SelfClosingTagToken:
			token := tokenizer.Token()
			markup = true
			boundary(token.Data)
			frame := newHTMLFrame(token, current())
			if frame.hidden || frame.code {
				prose.WriteByte('\n')
			}
			if !voidElement(token.Data) {
				if len(stack) >= 64 {
					return "", "", false, errors.New("technical HTML exceeds the nesting limit")
				}
				// In HTML, a self-closing slash does not close non-void
				// elements. Keep their content suppression fail-closed.
				stack = append(stack, frame)
			}
		case html.EndTagToken:
			token := tokenizer.Token()
			markup = true
			for i, s := range slices.Backward(stack) {
				if s.tag == token.Data {
					stack = stack[:i]
					break
				}
			}
			boundary(token.Data)
		case html.CommentToken, html.DoctypeToken:
			markup = true
			prose.WriteByte('\n')
		}
	}
}

func newHTMLFrame(token html.Token, parent htmlFrame) htmlFrame {
	frame := htmlFrame{tag: token.Data, hidden: parent.hidden, code: parent.code}
	switch token.Data {
	case "script", "style", "head", "iframe", "object", "template", "noscript", "svg", "math":
		frame.hidden = true
	case "pre", "code", "kbd", "samp":
		frame.code = true
	}
	for _, attr := range token.Attr {
		style := strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return -1
			}
			return unicode.ToLower(r)
		}, attr.Val)
		if attr.Key == "hidden" || (attr.Key == "aria-hidden" && strings.EqualFold(attr.Val, "true")) ||
			(attr.Key == "style" && (strings.Contains(style, "display:none") || strings.Contains(style, "visibility:hidden"))) {
			frame.hidden = true
		}
	}
	return frame
}

func voidElement(tag string) bool {
	switch tag {
	case "area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "param", "source", "track", "wbr":
		return true
	default:
		return false
	}
}

func redactAdministrativeLines(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if adminLinePattern.MatchString(line) {
			lines[i] = redactionMarker
		}
	}
	return strings.Join(lines, "\n")
}

func scrubText(text string) string {
	text = privateKeyPattern.ReplaceAllString(text, redactionMarker)
	text = redact.SensitiveText(text)
	text = bearerPattern.ReplaceAllString(text, redactionMarker)
	text = accessKeyPattern.ReplaceAllString(text, redactionMarker)
	text = urlPattern.ReplaceAllStringFunc(text, func(raw string) string {
		parsed, err := url.Parse(trimURLPunctuation(raw))
		if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(raw, "#") {
			return redactionMarker
		}
		return raw
	})
	text = emailPattern.ReplaceAllString(text, redactionMarker)
	return guidPattern.ReplaceAllString(text, redactionMarker)
}

// Candidate discovery deliberately discards quoted strings as well as
// Markdown code. A repository literal in a reproduction is not a source grant.
func proseWithoutCode(text string) string {
	var prose strings.Builder
	var fence byte
	fenceLength := 0
	fenceQuoteDepth := 0
	var inline inlineProseFilter
	for _, line := range strings.SplitAfter(text, "\n") {
		content := line
		quoteDepth := 0
		for {
			trimmed := strings.TrimLeft(content, " ")
			if !strings.HasPrefix(trimmed, ">") {
				break
			}
			content = strings.TrimPrefix(strings.TrimPrefix(trimmed, ">"), " ")
			quoteDepth++
		}
		indented := strings.HasPrefix(line, "    ") || strings.HasPrefix(content, "    ") || strings.HasPrefix(content, "\t")
		marker, width, rest := codeFence(content)
		if fence != 0 {
			bare := strings.TrimLeft(content, " ")
			if !indented && quoteDepth == fenceQuoteDepth && len(bare) > 0 && bare[0] == fence &&
				marker == fence && width >= fenceLength && strings.TrimSpace(rest) == "" {
				fence, fenceLength = 0, 0
			}
			prose.WriteByte('\n')
			continue
		}
		if indented {
			prose.WriteByte('\n')
			continue
		}
		if marker != 0 && inline.codeLength == 0 && inline.quote == 0 {
			fence, fenceLength = marker, width
			fenceQuoteDepth = quoteDepth
			prose.WriteByte('\n')
			continue
		}
		inline.writeProse(&prose, line)
		prose.WriteByte('\n')
	}
	return prose.String()
}

type inlineProseFilter struct {
	codeLength int
	quote      byte
}

func (filter *inlineProseFilter) writeProse(prose *strings.Builder, line string) {
	for i := 0; i < len(line); {
		char := line[i]
		if char == '`' && filter.quote == 0 {
			end := i + 1
			for end < len(line) && line[end] == char {
				end++
			}
			run := end - i
			if filter.codeLength == 0 {
				filter.codeLength = run
			} else if run == filter.codeLength {
				filter.codeLength = 0
			}
			prose.WriteByte(' ')
			i = end
			continue
		}
		if filter.codeLength == 0 {
			switch {
			case filter.quote != 0:
				if char == '\\' && i+1 < len(line) {
					i += 2
					continue
				}
				if char == filter.quote {
					filter.quote = 0
				}
			case char == '\'' && i > 0 && (unicode.IsLetter(rune(line[i-1])) || unicode.IsDigit(rune(line[i-1]))):
				prose.WriteByte(char)
			case char == '"' || char == '\'':
				filter.quote = char
				prose.WriteByte(' ')
			default:
				prose.WriteByte(char)
			}
		}
		i++
	}
}

func codeFence(line string) (byte, int, string) {
	content := strings.TrimLeft(line, " ")
	if len(content) >= 2 && strings.ContainsRune("-+*", rune(content[0])) && content[1] == ' ' {
		content = strings.TrimLeft(content[2:], " ")
	} else {
		end := 0
		for end < len(content) && content[end] >= '0' && content[end] <= '9' {
			end++
		}
		if end > 0 && end+1 < len(content) && (content[end] == '.' || content[end] == ')') && content[end+1] == ' ' {
			content = strings.TrimLeft(content[end+2:], " ")
		}
	}
	if len(content) < 3 || (content[0] != '`' && content[0] != '~') {
		return 0, 0, ""
	}
	end := 1
	for end < len(content) && content[end] == content[0] {
		end++
	}
	if end < 3 {
		return 0, 0, ""
	}
	return content[0], end, content[end:]
}
