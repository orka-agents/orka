/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/idna"

	"github.com/orka-agents/orka/internal/tokenexchange"
)

const (
	webFeedKindRSS    = "RSS"
	webFeedKindAtom   = "Atom"
	atomFeedNamespace = "http://www.w3.org/2005/Atom"
	xmlFeedNamespace  = "http://www.w3.org/XML/1998/namespace"
	maxFeedItems      = 100
)

var errInvalidWebFeed = errors.New("invalid, unsupported, or oversized RSS/Atom feed")

type webFeedFields struct {
	title, summary, published, updated string
	link                               *url.URL
	linkRank                           int
}

type webFeed struct {
	kind, namespace string
	fields          webFeedFields
	items           []webFeedFields
	omitted         int
	allowPrivate    bool
}

// extractWebFeed recognizes the document root, not arbitrary nested feed-like
// elements. The decoder stays strict, with no Entity or CharsetReader hooks:
// declarations cannot resolve external entities, read files, or fetch links.
// An empty extractor means this is not a feed. Never expose XML decoder errors,
// which can contain source-controlled text, to the caller.
func extractWebFeed(body []byte, contentType string, base *url.URL, allowPrivate bool, retrievedAt time.Time) (content, extractor string, omitted bool, err error) {
	mediaType, _, _ := mime.ParseMediaType(contentType)
	expectedFeed := mediaType == "application/rss+xml" || mediaType == "application/atom+xml"
	bodyLimitExceeded := len(body) > maxBodySize
	body = bytes.TrimPrefix(body, []byte("\xef\xbb\xbf"))
	decoder := xml.NewDecoder(bytes.NewReader(body))
	var root xml.StartElement
	for {
		offset := decoder.InputOffset()
		token, tokenErr := decoder.Token()
		if tokenErr != nil {
			if expectedFeed || looksLikeWebFeedStart(body[offset:]) || looksLikeWebFeedStart(body[decoder.InputOffset():]) {
				return "", "", false, errInvalidWebFeed
			}
			return "", "", false, nil
		}
		if start, ok := token.(xml.StartElement); ok {
			root = start
			break
		}
		if text, ok := token.(xml.CharData); ok && strings.TrimSpace(string(text)) != "" {
			if expectedFeed {
				return "", "", false, errInvalidWebFeed
			}
			return "", "", false, nil
		}
	}

	feed := webFeed{allowPrivate: allowPrivate}
	switch root.Name.Local {
	case "rss":
		if root.Name.Space != "" || webFeedAttr(root, "", "version") != "2.0" {
			return "", "", false, errInvalidWebFeed
		}
		feed.kind = webFeedKindRSS
		extractor = "rss_feed"
	case "feed":
		if root.Name.Space != atomFeedNamespace {
			return "", "", false, errInvalidWebFeed
		}
		feed.kind, feed.namespace = webFeedKindAtom, atomFeedNamespace
		extractor = "atom_feed"
	default:
		if expectedFeed {
			return "", "", false, errInvalidWebFeed
		}
		return "", "", false, nil
	}
	// The caller reads a probe byte beyond the cap. A complete prefix must not
	// disguise an oversized response as complete research material.
	if bodyLimitExceeded {
		return "", "", false, errInvalidWebFeed
	}
	base = feed.elementBase(root, base)
	if feed.kind == webFeedKindRSS {
		channels := 0
		err = readWebFeedChildren(decoder, root, func(child xml.StartElement) error {
			if child.Name != (xml.Name{Local: "channel"}) {
				return decoder.Skip()
			}
			channels++
			if channels != 1 {
				return errInvalidWebFeed
			}
			return feed.readFields(decoder, child, feed.elementBase(child, base), &feed.fields, false)
		})
		if channels != 1 {
			err = errInvalidWebFeed
		}
	} else {
		err = feed.readFields(decoder, root, base, &feed.fields, false)
	}
	if err != nil || finishWebFeed(decoder) != nil {
		return "", "", false, errInvalidWebFeed
	}
	return feed.render(retrievedAt), extractor, feed.omitted > 0, nil
}

// Also recognize a truncated/broken opening tag when Token cannot return it.
func looksLikeWebFeedStart(body []byte) bool {
	text := strings.TrimSpace(string(body))
	if !strings.HasPrefix(text, "<") {
		return false
	}
	name := text[1:]
	if end := strings.IndexAny(name, " \t\r\n/>"); end >= 0 {
		name = name[:end]
	}
	if colon := strings.IndexByte(name, ':'); colon >= 0 {
		name = name[colon+1:]
	}
	return name == "rss" || name == "feed"
}

func readWebFeedChildren(decoder *xml.Decoder, parent xml.StartElement, visit func(xml.StartElement) error) error {
	for {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch value := token.(type) {
		case xml.StartElement:
			if err := visit(value); err != nil {
				return err
			}
		case xml.EndElement:
			if value.Name != parent.Name {
				return errInvalidWebFeed
			}
			return nil
		case xml.CharData:
			if strings.TrimSpace(string(value)) != "" {
				return errInvalidWebFeed
			}
		case xml.Directive:
			return errInvalidWebFeed
		}
	}
}

func finishWebFeed(decoder *xml.Decoder) error {
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch value := token.(type) {
		case xml.Comment, xml.ProcInst:
		case xml.CharData:
			if strings.TrimSpace(string(value)) != "" {
				return errInvalidWebFeed
			}
		default:
			return errInvalidWebFeed
		}
	}
}

func (feed *webFeed) readFields(decoder *xml.Decoder, parent xml.StartElement, base *url.URL, fields *webFeedFields, item bool) error {
	return readWebFeedChildren(decoder, parent, func(child xml.StartElement) error {
		if child.Name.Space != feed.namespace {
			return decoder.Skip()
		}
		name := child.Name.Local
		isRSS := feed.kind == webFeedKindRSS
		childBase := feed.elementBase(child, base)
		if !item && ((isRSS && name == "item") || (!isRSS && name == "entry")) {
			if len(feed.items) == maxFeedItems {
				feed.omitted++
				return decoder.Skip() // Still validate the entire XML document.
			}
			var entry webFeedFields
			if err := feed.readFields(decoder, child, childBase, &entry, true); err != nil {
				return err
			}
			feed.items = append(feed.items, entry)
			return nil
		}
		if item && name == "link" {
			return feed.readLink(decoder, child, childBase, fields)
		}
		if item && isRSS && name == "guid" {
			return feed.readRSSGUID(decoder, child, childBase, fields)
		}
		return feed.readTextField(decoder, child, fields, item)
	})
}

func (feed *webFeed) readLink(decoder *xml.Decoder, child xml.StartElement, base *url.URL, fields *webFeedFields) error {
	if feed.kind == webFeedKindRSS {
		var link string
		if err := decoder.DecodeElement(&link, &child); err != nil {
			return err
		}
		if candidate := safeWebFeedURL(link, base, feed.allowPrivate); candidate != nil {
			// An explicit RSS link outranks the optional permalink GUID,
			// regardless of their order in the document.
			fields.link, fields.linkRank = candidate, 2
		}
		return nil
	}
	rel := webFeedAttr(child, "", "rel")
	if rel == "" || rel == "alternate" {
		rank := 1
		linkType := webFeedAttr(child, "", "type")
		mediaType, _, typeErr := mime.ParseMediaType(linkType)
		switch {
		case linkType == "":
			rank = 2
		case typeErr == nil && (mediaType == "text/html" || mediaType == "application/xhtml+xml"):
			rank = 3
		}
		link := safeWebFeedURL(webFeedAttr(child, "", "href"), base, feed.allowPrivate)
		if link != nil && rank > fields.linkRank {
			fields.link, fields.linkRank = link, rank
		}
	}
	return decoder.Skip()
}

// RSS GUIDs default to permalinks when isPermaLink is absent. Explicit
// non-true flags and opaque identifiers must not become grounding URLs.
func (feed *webFeed) readRSSGUID(decoder *xml.Decoder, child xml.StartElement, base *url.URL, fields *webFeedFields) error {
	for _, attr := range child.Attr {
		if attr.Name == (xml.Name{Local: "isPermaLink"}) && attr.Value != "true" {
			return decoder.Skip()
		}
	}
	var guid string
	if err := decoder.DecodeElement(&guid, &child); err != nil {
		return err
	}
	if fields.linkRank == 0 {
		if candidate := safeWebFeedURL(guid, base, feed.allowPrivate); candidate != nil {
			fields.link, fields.linkRank = candidate, 1
		}
	}
	return nil
}

func (feed *webFeed) readTextField(decoder *xml.Decoder, child xml.StartElement, fields *webFeedFields, item bool) error {
	name := child.Name.Local
	isRSS := feed.kind == webFeedKindRSS
	var target *string
	switch {
	case name == "title":
		target = &fields.title
	case isRSS && name == "description", !isRSS && ((!item && name == "subtitle") || (item && name == "summary")):
		target = &fields.summary
	case isRSS && name == "pubDate", !isRSS && item && name == "published":
		target = &fields.published
	case isRSS && !item && name == "lastBuildDate", !isRSS && name == "updated":
		target = &fields.updated
	default:
		// In particular, do not present Atom content or RSS content:encoded
		// as a summary, or substitute extension/build dates for publication.
		return decoder.Skip()
	}
	var text struct {
		Value string `xml:",chardata"`
		Inner string `xml:",innerxml"`
	}
	if err := decoder.DecodeElement(&text, &child); err != nil {
		return err
	}
	// RSS descriptions may contain HTML. Atom text constructs declare their
	// markup type explicitly; scalar dates and default/text constructs remain
	// literal after XML entity decoding, including escaped angle brackets.
	markup := isRSS && name == "description"
	if !isRSS && (name == "title" || name == "subtitle" || name == "summary") {
		switch webFeedAttr(child, "", "type") {
		case "html":
			markup = true
		case "xhtml":
			value, err := webFeedXHTMLText(text.Inner)
			if err != nil {
				return err
			}
			*target = value
			return nil
		}
	}
	if markup {
		*target = webFeedText(text.Value)
	} else {
		*target = webFeedLiteralText(text.Value)
	}
	return nil
}

func webFeedAttr(element xml.StartElement, namespace, name string) string {
	for _, attr := range element.Attr {
		if attr.Name == (xml.Name{Space: namespace, Local: name}) {
			return attr.Value
		}
	}
	return ""
}

func (feed *webFeed) elementBase(element xml.StartElement, base *url.URL) *url.URL {
	if ref := webFeedAttr(element, xmlFeedNamespace, "base"); ref != "" {
		// An unsafe base must not silently fall back to a different source URL.
		return safeWebFeedURL(ref, base, feed.allowPrivate)
	}
	return base
}

// Link validation is purely local: extraction does not resolve DNS or fetch any
// feed-supplied URL. Actual fetches retain the existing public-DNS/dial guards.
func safeWebFeedURL(reference string, base *url.URL, allowPrivate bool) *url.URL {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return nil
	}
	parsed, err := url.Parse(reference)
	if err != nil {
		return nil
	}
	if base != nil {
		parsed = base.ResolveReference(parsed)
	}
	if validateWebFetchURL(parsed, allowPrivate) != nil || strings.Contains(parsed.Hostname(), "%") {
		return nil // Reject scoped IP literals too; ParseIP does not accept zones.
	}
	hostname := parsed.Hostname()
	if net.ParseIP(hostname) == nil {
		// Browsers normalize IDNs, including Unicode dots and fullwidth digits,
		// before interpreting local/numeric hosts. Check and emit the same ASCII
		// authority so a rendered link cannot bypass the local-address guard.
		asciiHost, err := idna.Lookup.ToASCII(hostname)
		if err != nil {
			return nil
		}
		if asciiHost != hostname {
			if port := parsed.Port(); port != "" {
				parsed.Host = net.JoinHostPort(asciiHost, port)
			} else {
				parsed.Host = asciiHost
			}
		}
	}
	if !allowPrivate {
		host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
		if host == "localhost" || strings.HasSuffix(host, ".localhost") {
			return nil // These names are loopback destinations without a DNS lookup.
		}
		if address := net.ParseIP(host); address != nil {
			if !tokenexchange.IsPublicAddress(address) {
				return nil
			}
		} else if webFeedNumericHost(host) {
			// Browsers can interpret short, decimal, octal, and hex IPv4
			// spellings differently from Go. Do not emit ambiguous literals.
			return nil
		}
	}
	return parsed
}

func webFeedNumericHost(host string) bool {
	for part := range strings.SplitSeq(strings.ToLower(host), ".") {
		digits := "0123456789"
		if strings.HasPrefix(part, "0x") {
			part, digits = part[2:], "0123456789abcdef"
		}
		if part == "" || strings.Trim(part, digits) != "" {
			return false
		}
	}
	return true
}

var webFeedMarkdownEscaper = strings.NewReplacer(
	"\\", "\\\\", "`", "\\`", "*", "\\*", "_", "\\_", "{", "\\{", "}", "\\}",
	"[", "\\[", "]", "\\]", "(", "\\(", ")", "\\)", "#", "\\#", "!", "\\!", "|", "\\|", "~", "\\~",
	"&", "&amp;", "<", "&lt;", ">", "&gt;",
)

func webFeedText(value string) string {
	// XML has already decoded XML entities/CDATA. Decode HTML entities before
	// the existing script/style/tag stripping so encoded markup stays inert.
	text := extractText([]byte(html.UnescapeString(value)))
	return webFeedLiteralText(text)
}

// Read XHTML nodes before decoding/escaping their character data. An escaped
// angle bracket or CDATA payload is text, never markup to strip a second time.
func webFeedXHTMLText(value string) (string, error) {
	decoder := xml.NewDecoder(strings.NewReader(value))
	var text strings.Builder
	ignoredDepth := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return webFeedLiteralText(text.String()), nil
		}
		if err != nil {
			return "", errInvalidWebFeed
		}
		switch part := token.(type) {
		case xml.StartElement:
			if ignoredDepth > 0 {
				ignoredDepth++
			} else if name := strings.ToLower(part.Name.Local); name == "script" || name == "style" {
				ignoredDepth = 1
			} else {
				text.WriteByte(' ')
			}
		case xml.EndElement:
			if ignoredDepth > 0 {
				ignoredDepth--
			} else {
				text.WriteByte(' ')
			}
		case xml.CharData:
			if ignoredDepth == 0 {
				text.Write(part)
			}
		case xml.Directive:
			return "", errInvalidWebFeed
		}
	}
}

func webFeedLiteralText(value string) string {
	return webFeedMarkdownEscaper.Replace(strings.Join(strings.Fields(value), " "))
}

// Angle-delimited Markdown destinations still need escaping: URL.String
// leaves query punctuation intact. Preserve query separators, not %26.
var webFeedDestinationEscaper = strings.NewReplacer(
	"&", "&amp;", "<", "%3C", ">", "%3E", "\\", "%5C", "\"", "%22", "'", "%27",
	"(", "%28", ")", "%29", "`", "%60", " ", "%20",
)

func (feed *webFeed) render(retrievedAt time.Time) string {
	var out strings.Builder
	// Keep retrieval metadata and item-limit coverage ahead of source-controlled
	// text. max_chars can still shorten this output; the outer flag records it.
	fmt.Fprintf(&out, "Retrieved at (UTC): %s\n", retrievedAt.UTC().Format(time.RFC3339))
	if feed.omitted > 0 {
		fmt.Fprintf(&out, "Items omitted: %d (feed item limit: %d).\n", feed.omitted, maxFeedItems)
	}
	fmt.Fprintf(&out, "%s feed", feed.kind)
	if feed.fields.title != "" {
		fmt.Fprintf(&out, ": %s", feed.fields.title)
	}
	out.WriteByte('\n')
	if feed.fields.published != "" {
		fmt.Fprintf(&out, "Feed published: %s\n", feed.fields.published)
	}
	if feed.fields.updated != "" {
		label := "Feed updated"
		if feed.kind == webFeedKindRSS {
			label = "Feed build date"
		}
		fmt.Fprintf(&out, "%s: %s\n", label, feed.fields.updated)
	}
	if feed.fields.summary != "" {
		fmt.Fprintf(&out, "Feed summary: %s\n", feed.fields.summary)
	}
	if len(feed.items) == 0 {
		out.WriteString("No items in feed.\n")
	}
	for i, item := range feed.items {
		title := item.title
		if title == "" {
			title = "Untitled item"
		}
		if item.link == nil {
			fmt.Fprintf(&out, "\n%d. %s\nSource link unavailable.\n", i+1, title)
		} else {
			fmt.Fprintf(&out, "\n%d. [%s](<%s>)\n", i+1, title, webFeedDestinationEscaper.Replace(item.link.String()))
		}
		published := item.published
		if published == "" {
			published = "not provided"
		}
		fmt.Fprintf(&out, "Published: %s\n", published)
		if item.updated != "" {
			fmt.Fprintf(&out, "Updated: %s\n", item.updated)
		}
		if item.summary != "" {
			fmt.Fprintf(&out, "Feed summary: %s\n", item.summary)
		}
	}
	return strings.TrimSpace(out.String())
}
