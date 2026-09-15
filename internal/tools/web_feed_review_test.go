package tools

import (
	"fmt"
	"strings"
	"testing"
)

func TestWebFetchFeedHTMLContentTypePreservesArticleMetadata(t *testing.T) {
	for _, test := range []struct{ name, body, extractor, published string }{
		{"rss", `<rss version="2.0"><channel><item><title>Article</title><link>https://news.example.test/article</link><pubDate>Mon, 30 Jun 2025 12:00:00 GMT</pubDate><description>Source summary.</description></item></channel></rss>`, "rss_feed", "Mon, 30 Jun 2025 12:00:00 GMT"},
		{"atom", `<feed xmlns="http://www.w3.org/2005/Atom"><entry><title>Article</title><link href="https://news.example.test/article"/><published>2025-06-30T12:00:00Z</published><summary>Source summary.</summary></entry></feed>`, "atom_feed", "2025-06-30T12:00:00Z"},
	} {
		for _, mediaType := range []string{"text/html", "text/html; charset=utf-8"} {
			t.Run(test.name+"/"+mediaType, func(t *testing.T) {
				tool, base := serveWebFeed(t, test.body, mediaType)
				result, _ := executeWebFeed(t, tool, WebFetchArgs{URL: base})
				if result.Extractor != test.extractor {
					t.Fatalf("extractor = %q, want %q", result.Extractor, test.extractor)
				}
				assertWebFeedContains(t, result.Content, "[Article](<https://news.example.test/article>)", "Published: "+test.published, "Feed summary: Source summary.")
				raw, _ := executeWebFeed(t, tool, WebFetchArgs{URL: base, Raw: true})
				if raw.Extractor != extractorRaw || raw.Content != test.body {
					t.Fatal("HTML-labelled feed raw mode changed the response bytes")
				}
			})
		}
	}
	tool, base := serveWebFeed(t, `<html><body><h1>News</h1><rss version="2.0"><channel><title>Nested</title></channel></rss></body></html>`, "text/html")
	result, _ := executeWebFeed(t, tool, WebFetchArgs{URL: base})
	if result.Extractor != "html_text" {
		t.Fatalf("nested feed-like elements changed HTML extraction: %s", result.Extractor)
	}
	assertWebFeedContains(t, result.Content, "News", "Nested")
}

func TestWebFetchFeedAtomPrefersParameterizedArticleMediaTypes(t *testing.T) {
	for _, mediaType := range []string{"text/html; charset=utf-8", "Text/HTML; charset=UTF-8", `application/xhtml+xml; charset="utf-8"`} {
		t.Run(mediaType, func(t *testing.T) {
			body := `<feed xmlns="http://www.w3.org/2005/Atom"><entry><title>Article</title><link rel="alternate" type="application/xml" href="https://news.example.test/article.xml"/><link rel="alternate" type='` + mediaType + `' href="https://news.example.test/article.html"/></entry></feed>`
			tool, base := serveWebFeed(t, body, "application/atom+xml")
			result, _ := executeWebFeed(t, tool, WebFetchArgs{URL: base})
			assertWebFeedContains(t, result.Content, "[Article](<https://news.example.test/article.html>)")
			assertWebFeedExcludes(t, result.Content, "article.xml")
		})
	}
}

func TestWebFetchFeedRejectsLocalhostLinksAndBases(t *testing.T) {
	for _, link := range []string{
		"http://localhost/article", "https://localhost:8443/article", "http://localhost./article", "http://LOCALHOST/article",
		"http://console.localhost/article", "http://a.b.LoCaLhOsT.:8080/article", "http://localhost。/article",
		"http://console。localhost/article", "http://ＬＯＣＡＬＨＯＳＴ/article", "http://１２７．０．０．１/article",
		"http://127。0。0。1/article", "http://０ｘ７ｆ０００００１/article",
	} {
		for _, format := range []string{"rss", "atom"} {
			for _, asBase := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/base=%t", format, link, asBase), func(t *testing.T) {
					baseAttribute, target := "", link
					if asBase {
						baseAttribute, target = ` xml:base="`+link+`"`, "relative"
					}
					body := `<rss version="2.0"><channel><item` + baseAttribute + `><title>Article</title><link>` + target + `</link></item></channel></rss>`
					if format == "atom" {
						body = `<feed xmlns="http://www.w3.org/2005/Atom"><entry` + baseAttribute + `><title>Article</title><link href="` + target + `"/></entry></feed>`
					}
					tool, base := serveWebFeed(t, body, "application/xml")
					result, _ := executeWebFeed(t, tool, WebFetchArgs{URL: base})
					assertWebFeedContains(t, result.Content, "Source link unavailable.")
					if strings.Contains(strings.ToLower(result.Content), "localhost") || strings.Contains(result.Content, "](<") {
						t.Fatal("a loopback name was emitted as an article link")
					}
				})
			}
		}
	}
}

func TestWebFetchFeedPreservesLiteralScalarText(t *testing.T) {
	for _, format := range []string{"rss", "atom"} {
		t.Run(format, func(t *testing.T) {
			body := `<rss version="2.0"><channel><title>1 &lt; 2 &gt; 0</title><item><title>Literal &lt;b&gt;text&lt;/b&gt; &amp;lt;word&amp;gt;</title><description><![CDATA[<p>HTML <b>summary</b>.</p><script>hiddenMarkup()</script>]]></description></item></channel></rss>`
			if format == "atom" {
				body = `<feed xmlns="http://www.w3.org/2005/Atom"><title>1 &lt; 2 &gt; 0</title><entry><title type="text">Literal &lt;b&gt;text&lt;/b&gt; &amp;lt;word&amp;gt;</title><summary>1 &lt; 2 &gt; 0 &lt;script&gt;literalText()&lt;/script&gt;</summary></entry><entry><title>Markup</title><summary type="html">&lt;p&gt;HTML &lt;b&gt;summary&lt;/b&gt;.&lt;/p&gt;&lt;script&gt;hiddenMarkup()&lt;/script&gt;</summary></entry></feed>`
			}
			tool, base := serveWebFeed(t, body, "application/xml")
			result, _ := executeWebFeed(t, tool, WebFetchArgs{URL: base})
			assertWebFeedContains(t, result.Content, "feed: 1 &lt; 2 &gt; 0", "Literal &lt;b&gt;text&lt;/b&gt; &amp;lt;word&amp;gt;", "Feed summary: HTML summary .")
			assertWebFeedExcludes(t, result.Content, "hiddenMarkup", "<b>", "<script>")
			if format == "atom" {
				assertWebFeedContains(t, result.Content, "Feed summary: 1 &lt; 2 &gt; 0 &lt;script&gt;literalText\\(\\)&lt;/script&gt;")
			}
		})
	}
}

func TestWebFetchRSSPermalinkGUIDFallback(t *testing.T) {
	for _, test := range []struct{ name, guid, attributes, before, after, want string }{
		{"implicit", "https://news.example.test/article", "", "", "", "https://news.example.test/article"},
		{"explicit", "https://news.example.test/article", ` isPermaLink="true"`, "", "", "https://news.example.test/article"},
		{"non-permalink", "https://news.example.test/article", ` isPermaLink="false"`, "", "", ""},
		{"empty flag", "https://news.example.test/article", ` isPermaLink=""`, "", "", ""},
		{"opaque identifier", "urn:uuid:1234", ` isPermaLink="false"`, "", "", ""},
		{"relative", "article", ` xml:base="https://news.example.test/edition/"`, "", "", "https://news.example.test/edition/article"},
		{"explicit link first", "https://news.example.test/guid", "", `<link>https://news.example.test/link</link>`, "", "https://news.example.test/link"},
		{"explicit link last", "https://news.example.test/guid", "", "", `<link>https://news.example.test/link</link>`, "https://news.example.test/link"},
		{"unsafe link fallback", "https://news.example.test/guid", "", `<link>javascript:bad</link>`, "", "https://news.example.test/guid"},
		{"private address", "http://127.0.0.1/article", "", "", "", ""},
		{"local hostname", "http://console.localhost/article", "", "", "", ""},
		{"unsafe scheme", "javascript:bad", "", "", "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := `<rss version="2.0"><channel><item><title>Article</title>` + test.before + `<guid` + test.attributes + `>` + test.guid + `</guid>` + test.after + `</item></channel></rss>`
			tool, base := serveWebFeed(t, body, "application/rss+xml")
			result, _ := executeWebFeed(t, tool, WebFetchArgs{URL: base})
			if test.want == "" {
				assertWebFeedContains(t, result.Content, "Source link unavailable.")
				assertWebFeedExcludes(t, result.Content, "](<")
			} else {
				assertWebFeedContains(t, result.Content, "[Article](<"+test.want+">)")
				if strings.Count(result.Content, "](<") != 1 {
					t.Fatal("expected one grounding link")
				}
			}
		})
	}
}

func TestWebFetchFeedCanonicalizesPublicIDNLinksAndBases(t *testing.T) {
	for _, test := range []struct{ name, body, want string }{
		{"rss", `<rss version="2.0"><channel><item><title>Article</title><link>https://bücher.example.test:8443/article?q=1</link></item></channel></rss>`, "https://xn--bcher-kva.example.test:8443/article?q=1"},
		{"rss guid", `<rss version="2.0"><channel><item><title>Article</title><guid>https://bücher.example.test./article</guid></item></channel></rss>`, "https://xn--bcher-kva.example.test./article"},
		{"atom base", `<feed xmlns="http://www.w3.org/2005/Atom"><entry xml:base="https://bücher.example.test:8443/edition/"><title>Article</title><link href="article"/></entry></feed>`, "https://xn--bcher-kva.example.test:8443/edition/article"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tool, base := serveWebFeed(t, test.body, "application/xml")
			result, _ := executeWebFeed(t, tool, WebFetchArgs{URL: base})
			assertWebFeedContains(t, result.Content, "[Article](<"+test.want+">)")
		})
	}
}

func TestWebFetchAtomXHTMLPreservesEscapedText(t *testing.T) {
	body := `<feed xmlns="http://www.w3.org/2005/Atom"><title type="xhtml"><div xmlns="http://www.w3.org/1999/xhtml"><span>1 &lt; 2 &gt; 0</span></div></title><entry><title type="xhtml"><div xmlns="http://www.w3.org/1999/xhtml"><span>Literal &lt;b&gt;tag&lt;/b&gt; &amp;lt;word&amp;gt;</span></div></title><summary type="xhtml"><div xmlns="http://www.w3.org/1999/xhtml"><p><![CDATA[1 < 2 > 0]]></p><p>Literal &lt;script&gt;text()&lt;/script&gt;</p><script>hiddenExecutable()</script><style>hiddenStyle{}</style></div></summary></entry></feed>`
	tool, base := serveWebFeed(t, body, "application/atom+xml")
	result, _ := executeWebFeed(t, tool, WebFetchArgs{URL: base})
	assertWebFeedContains(t, result.Content, "Atom feed: 1 &lt; 2 &gt; 0", "Literal &lt;b&gt;tag&lt;/b&gt; &amp;lt;word&amp;gt;", "Feed summary: 1 &lt; 2 &gt; 0 Literal &lt;script&gt;text\\(\\)&lt;/script&gt;")
	assertWebFeedExcludes(t, result.Content, "hiddenExecutable", "hiddenStyle", "<div", "<script>", "<![CDATA[")
}
