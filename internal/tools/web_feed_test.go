/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// Use a public-looking request URL with a test-only dialer pinned to httptest.
// This exercises production URL/link validation without DNS or external calls.
func newWebFeedTestTool(t *testing.T, handler http.HandlerFunc) (*WebFetchTool, string) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	tool := NewWebFetchTool()
	tool.client.Transport = transport
	tool.now = webFeedTestClock
	return tool, "http://feeds.example.test"
}

// The offset deliberately crosses the UTC date boundary; production uses time.Now.
func webFeedTestClock() time.Time {
	return time.Date(2025, time.July, 1, 0, 30, 45, 123456789, time.FixedZone("test", 5*60*60+30*60))
}

func serveWebFeed(t *testing.T, body, contentType string) (*WebFetchTool, string) {
	t.Helper()
	return newWebFeedTestTool(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write([]byte(body))
	})
}

func executeWebFeed(t *testing.T, tool *WebFetchTool, args WebFetchArgs) (WebFetchResult, string) {
	t.Helper()
	input, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	output, err := tool.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var result WebFetchResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if result.URL != args.URL || result.Status != http.StatusOK {
		t.Fatalf("unexpected response metadata: URL=%q status=%d", result.URL, result.Status)
	}
	if result.Length != utf8.RuneCountInString(result.Content) || !utf8.ValidString(result.Content) {
		t.Fatal("dishonest length or invalid UTF-8 content")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(output), &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 6 {
		t.Fatalf("result contract changed: %d fields", len(fields))
	}
	return result, output
}

func assertWebFeedContains(t *testing.T, content string, values ...string) {
	t.Helper()
	for _, value := range values {
		if !strings.Contains(content, value) {
			t.Errorf("content missing %q:\n%s", value, content)
		}
	}
}

func assertWebFeedExcludes(t *testing.T, content string, values ...string) {
	t.Helper()
	for _, value := range values {
		if strings.Contains(content, value) {
			t.Errorf("content unexpectedly includes %q:\n%s", value, content)
		}
	}
}

func TestWebFetchFeedRSS(t *testing.T) {
	body := `<?xml version="1.0"?><rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/" xmlns:media="urn:media">
<channel><title>Example &amp; News</title><description>A desk feed</description>
<pubDate>Tue, 02 Jan 2024 11:00:00 GMT</pubDate><lastBuildDate>Wed, 03 Jan 2024 12:00:00 GMT</lastBuildDate>
<item><title>Science &#x1F680; &amp; cafés</title><link>https://news.example.test/story?q=1&amp;x=2</link>
<pubDate>Mon, 01 Jan 2024 09:30:00 -0500</pubDate>
<description><![CDATA[<p>A <b>brief</b> &amp; useful &#233; summary.</p><script>secretScript()</script><style>secretStyle{}</style>]]></description>
<content:encoded><![CDATA[Do not claim this is the full article.]]></content:encoded>
</item><item><title>No date</title><link>/other</link><media:pubDate>NOT A PUBLICATION DATE</media:pubDate></item>
</channel></rss>`
	tool, base := serveWebFeed(t, body, "application/rss+xml; charset=utf-8")
	result, _ := executeWebFeed(t, tool, WebFetchArgs{URL: base + "/rss/world.xml"})
	if result.Extractor != "rss_feed" || result.Truncated {
		t.Fatalf("extractor=%q truncated=%v", result.Extractor, result.Truncated)
	}
	assertWebFeedContains(t, result.Content,
		"RSS feed: Example &amp; News", "Feed summary: A desk feed",
		"Feed published: Tue, 02 Jan 2024 11:00:00 GMT", "Feed build date: Wed, 03 Jan 2024 12:00:00 GMT",
		"[Science 🚀 &amp; cafés](<https://news.example.test/story?q=1&amp;x=2>)",
		"Published: Mon, 01 Jan 2024 09:30:00 -0500", "Feed summary: A brief &amp; useful é summary.",
		"[No date](<http://feeds.example.test/other>)\nPublished: not provided")
	assertWebFeedExcludes(t, result.Content, "secretScript", "secretStyle", "<p>", "CDATA", "full article", "NOT A PUBLICATION DATE")
}

func TestWebFetchFeedAtom(t *testing.T) {
	body := `<a:feed xmlns:a="http://www.w3.org/2005/Atom" xml:base="https://news.example.test/root/">
<a:title>Journal</a:title><a:subtitle type="html">&lt;b&gt;Desk&lt;/b&gt; notes</a:subtitle><a:updated>2024-02-06T10:00:00Z</a:updated>
<a:entry xml:base="../stories/"><a:title type="html">&lt;b&gt;First&lt;/b&gt; entry</a:title>
<a:link rel="self" href="https://news.example.test/api/entry"/><a:link rel="enclosure" href="https://news.example.test/audio"/>
<a:link rel="alternate" type="application/xml" href="data.xml"/><a:link type="text/html" xml:base="editions/" href="first?q=1&amp;x=2"/>
<a:published>2024-02-01T12:34:56+03:00</a:published><a:updated>2024-02-05T13:00:00Z</a:updated>
<a:summary type="xhtml"><div xmlns="http://www.w3.org/1999/xhtml"><p>Résumé <b>text</b>.</p><script>hiddenCode()</script></div></a:summary>
<a:content type="html">NOT THE SUMMARY</a:content></a:entry>
<a:entry><a:title>Updated only</a:title><a:updated>2024-02-04T14:00:00Z</a:updated><a:link href="second"/>
<a:summary type="html">&lt;p&gt;Short &amp;amp; clear.&lt;/p&gt;</a:summary></a:entry></a:feed>`
	tool, base := serveWebFeed(t, body, "application/atom+xml")
	result, _ := executeWebFeed(t, tool, WebFetchArgs{URL: base + "/atom"})
	if result.Extractor != "atom_feed" || result.Truncated {
		t.Fatalf("extractor=%q truncated=%v", result.Extractor, result.Truncated)
	}
	assertWebFeedContains(t, result.Content,
		"Atom feed: Journal", "Feed summary: Desk notes", "Feed updated: 2024-02-06T10:00:00Z",
		"[First entry](<https://news.example.test/stories/editions/first?q=1&amp;x=2>)",
		"Published: 2024-02-01T12:34:56+03:00\nUpdated: 2024-02-05T13:00:00Z", "Feed summary: Résumé text .",
		"[Updated only](<https://news.example.test/root/second>)\nPublished: not provided\nUpdated: 2024-02-04T14:00:00Z",
		"Feed summary: Short &amp; clear.")
	assertWebFeedExcludes(t, result.Content, "hiddenCode", "NOT THE SUMMARY", "/api/", "/audio", "data.xml", "Published: 2024-02-04", "Published: 2024-02-06")
}

func TestWebFetchFeedRetrievalTimestampIsDistinctMetadata(t *testing.T) {
	for _, test := range []struct{ name, body, feedDate, itemDates string }{
		{"rss", `<rss version="2.0"><channel><lastBuildDate>Tue, 02 Jan 2024 10:00:00 GMT</lastBuildDate><item><title>Dated</title><pubDate>Mon, 01 Jan 2024 09:00:00 GMT</pubDate></item><item><title>Undated</title></item></channel></rss>`,
			"Feed build date: Tue, 02 Jan 2024 10:00:00 GMT", "Published: Mon, 01 Jan 2024 09:00:00 GMT"},
		{"atom", `<feed xmlns="http://www.w3.org/2005/Atom"><updated>2024-01-03T10:00:00Z</updated><entry><title>Dated</title><published>2024-01-01T09:00:00Z</published><updated>2024-01-02T10:00:00Z</updated></entry><entry><title>Updated only</title><updated>2024-01-02T11:00:00Z</updated></entry></feed>`,
			"Feed updated: 2024-01-03T10:00:00Z", "Published: 2024-01-01T09:00:00Z\nUpdated: 2024-01-02T10:00:00Z"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tool, base := newWebFeedTestTool(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/xml")
				w.Header().Set("Date", "Fri, 01 Jan 1999 00:00:00 GMT")
				w.Header().Set("Last-Modified", "Fri, 01 Jan 1999 00:00:00 GMT")
				_, _ = w.Write([]byte(test.body))
			})
			result, _ := executeWebFeed(t, tool, WebFetchArgs{URL: base})
			const stamp = "Retrieved at (UTC): 2025-06-30T19:00:45Z\n"
			if !strings.HasPrefix(result.Content, stamp) || result.Truncated {
				t.Fatalf("missing UTC RFC3339 retrieval metadata: %q", result.Content)
			}
			assertWebFeedContains(t, result.Content, test.feedDate, test.itemDates, "Published: not provided")
			assertWebFeedExcludes(t, result.Content, "1999", "Published: 2025-", "Updated: 2025-", "Feed build date: 2025-", ".123456789", "+05:30")

			// A later fetch gets its own clock value, not cached retrieval metadata.
			tool.now = func() time.Time { return webFeedTestClock().Add(time.Hour) }
			later, _ := executeWebFeed(t, tool, WebFetchArgs{URL: base})
			if !strings.HasPrefix(later.Content, "Retrieved at (UTC): 2025-06-30T20:00:45Z\n") {
				t.Fatalf("retrieval timestamp did not advance: %q", later.Content)
			}
		})
	}
}

func TestWebFetchFeedRetrievalTimestampDefaultsToActualServerTime(t *testing.T) {
	tool, base := serveWebFeed(t, `<rss version="2.0"><channel><item><title>No publication date</title></item></channel></rss>`, "application/rss+xml")
	tool.now = nil // Exercise the production clock rather than the deterministic test seam.
	// Unknown public arguments must never set server time.
	args := json.RawMessage(fmt.Sprintf(`{"url":%q,"now":"1999-01-01T00:00:00Z","retrieved_at":"1999-01-01T00:00:00Z","server_time":"1999-01-01T00:00:00Z"}`, base))
	before := time.Now().UTC().Truncate(time.Second)
	output, err := tool.Execute(context.Background(), args)
	after := time.Now().UTC().Truncate(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var result WebFetchResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	line, _, _ := strings.Cut(result.Content, "\n")
	stamp := strings.TrimPrefix(line, "Retrieved at (UTC): ")
	retrievedAt, err := time.Parse(time.RFC3339, stamp)
	if err != nil || !strings.HasSuffix(stamp, "Z") || retrievedAt.Before(before) || retrievedAt.After(after) {
		t.Fatalf("timestamp %q is not actual UTC retrieval time between %s and %s: %v", stamp, before, after, err)
	}
	assertWebFeedContains(t, result.Content, "Published: not provided")
	assertWebFeedExcludes(t, result.Content, "1999-01-01")
}

func TestWebFetchFeedRetrievalMetadataAndBoundedCoverage(t *testing.T) {
	body := `<rss version="2.0"><channel><title>` + strings.Repeat("Long title ", 100) + `</title>` +
		strings.Repeat(`<item><title>Brief</title></item>`, maxFeedItems+3) + `</channel></rss>`
	tool, base := serveWebFeed(t, body, "application/rss+xml")
	// Even a long source title cannot hide retrieval metadata or the item-limit
	// notice. Character truncation can omit further entries and must stay true.
	const prefix = "Retrieved at (UTC): 2025-06-30T19:00:45Z\nItems omitted: 3 (feed item limit: 100).\n"
	result, _ := executeWebFeed(t, tool, WebFetchArgs{URL: base, MaxChars: utf8.RuneCountInString(prefix)})
	if result.Content != prefix || !result.Truncated {
		t.Fatalf("bounded feed lost retrieval/coverage metadata: %+v", result)
	}
	assertWebFeedExcludes(t, result.Content, "Published:", "Brief")
}

func TestWebFetchFeedDetectionAndRawBypass(t *testing.T) {
	const feed = `<rss version="2.0"><channel><title>Detected</title></channel></rss>`
	for _, contentType := range []string{"application/rss+xml", "application/xml", "text/xml", "text/plain", "application/octet-stream"} {
		t.Run(contentType, func(t *testing.T) {
			tool, base := serveWebFeed(t, "\xef\xbb\xbf"+feed, contentType)
			result, _ := executeWebFeed(t, tool, WebFetchArgs{URL: base})
			if result.Extractor != "rss_feed" {
				t.Fatalf("extractor = %q", result.Extractor)
			}
			result, _ = executeWebFeed(t, tool, WebFetchArgs{URL: base, Raw: true})
			if result.Extractor != extractorRaw || result.Content != "\xef\xbb\xbf"+feed || result.Truncated {
				t.Fatal("raw bypass changed feed bytes")
			}
		})
	}
	for _, body := range []string{`<rss version="2.0"><channel>&undefined;</channel>`, `<feed xmlns="http://www.w3.org/2005/Atom">`} {
		tool, base := serveWebFeed(t, body, "application/xml")
		result, _ := executeWebFeed(t, tool, WebFetchArgs{URL: base, Raw: true})
		if result.Content != body || result.Extractor != extractorRaw {
			t.Fatal("raw bypass parsed invalid XML")
		}
	}
}

func TestWebFetchFeedNonFeedRegressions(t *testing.T) {
	for _, test := range []struct {
		name, contentType, body, want, extractor string
		raw                                      bool
	}{
		{"html", "text/html", `<h1>Hello</h1><script>hidden()</script>`, "Hello", "html_text", false},
		{"raw html", "text/html", `<h1>Hello</h1>`, `<h1>Hello</h1>`, "raw", true},
		{"json", "application/json", `{"rss":"not a feed"}`, "{\n  \"rss\": \"not a feed\"\n}", "json", false},
		{"raw json still pretty", "application/json", `{"ok":true}`, "{\n  \"ok\": true\n}", "json", true},
		{"xml", "application/xml", `<document><rss version="2.0"/></document>`, `<document><rss version="2.0"/></document>`, "raw", false},
		{"broken other xml", "application/xml", `<document>&bad;`, `<document>&bad;`, "raw", false},
		{"text", "text/plain", "ordinary é text", "ordinary é text", "raw", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			tool, base := serveWebFeed(t, test.body, test.contentType)
			result, _ := executeWebFeed(t, tool, WebFetchArgs{URL: base, Raw: test.raw})
			if result.Content != test.want || result.Extractor != test.extractor || result.Truncated {
				t.Fatalf("result = %+v", result)
			}
		})
	}
}

func TestWebFetchFeedUnsafeLinksAndEscaping(t *testing.T) {
	for _, link := range []string{
		"javascript:alert(1)", "data:text/html,bad", "file:///etc/passwd", "ftp://news.example.test/a",
		"https://fixture-user@news.example.test/a", "http://127.0.0.1/a", "http://10.0.0.1/a", "http://169.254.169.254/a",
		"http://127.0.0.1./a", "http://127.1/a", "http://2130706433/a", "http://0177.0.0.1/a", "http://0x7f000001/a",
		"http://[::1]/a", "http://[::ffff:127.0.0.1]/a", "http://[fe80::1%25eth0]/a", "https:///missing-host", "https://bad%host/a",
	} {
		t.Run(link, func(t *testing.T) {
			for _, atom := range []bool{false, true} {
				body := `<rss version="2.0"><channel><item><title>Safe title</title><link>` + link + `</link></item></channel></rss>`
				if atom {
					body = `<feed xmlns="http://www.w3.org/2005/Atom"><entry><title>Safe title</title><link href="` + link + `"/></entry></feed>`
				}
				tool, base := serveWebFeed(t, body, "application/xml")
				result, _ := executeWebFeed(t, tool, WebFetchArgs{URL: base})
				assertWebFeedContains(t, result.Content, "Source link unavailable.", "Published: not provided")
				assertWebFeedExcludes(t, result.Content, link, "FAKE_PASSWORD", "](<")
			}
		})
	}
	body := `<rss version="2.0"><channel><item><title><![CDATA[Evil](https://evil.test) *bold* ` + "`code`" + `]]></title>
<link><![CDATA[https://news.example.test/a(b)?x=)>[bad](javascript:alert(1))&y="hello world"&copy;]]></link>
<description><![CDATA[<script>hidden()</script> [click](javascript:bad) &lt;script&gt;encodedHidden()&lt;/script&gt;]]></description></item></channel></rss>`
	tool, base := serveWebFeed(t, body, "application/rss+xml")
	result, _ := executeWebFeed(t, tool, WebFetchArgs{URL: base})
	assertWebFeedContains(t, result.Content, `[Evil\]\(https://evil.test\) \*bold\* `+"\\`code\\`"+`](<https://news.example.test/a%28b%29?x=%29%3E[bad]%28javascript:alert%281%29%29&amp;y=%22hello%20world%22&amp;copy;>)`,
		`Feed summary: \[click\]\(javascript:bad\)`)
	assertWebFeedExcludes(t, result.Content, "hidden()", "encodedHidden()", "](javascript:", "](https://evil.test)")
}

func TestWebFetchFeedRelativeLinksAndRedirectBase(t *testing.T) {
	var requests atomic.Int32
	tool, base := newWebFeedTestTool(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/moved/feed.xml", http.StatusFound)
			return
		}
		if r.URL.Path != "/moved/feed.xml" {
			t.Errorf("unexpected source-link request: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(`<rss version="2.0"><channel><item><title>Relative</title><link>story</link></item>
<item xml:base="../editions/"><title>Base</title><link>one</link></item>
<item><title>Network path</title><link>//news.example.test/two</link></item>
<item xml:base="http://127.0.0.1/"><title>Bad base</title><link>three</link></item></channel></rss>`))
	})
	result, _ := executeWebFeed(t, tool, WebFetchArgs{URL: base + "/start"})
	assertWebFeedContains(t, result.Content,
		"[Relative](<http://feeds.example.test/moved/story>)", "[Base](<http://feeds.example.test/editions/one>)",
		"[Network path](<http://news.example.test/two>)", "Bad base\nSource link unavailable.")
	if requests.Load() != 2 {
		t.Fatalf("made %d requests, want fetch + redirect only", requests.Load())
	}
}

func TestWebFetchFeedMalformedFailsWithoutBody(t *testing.T) {
	for _, test := range []struct{ name, body, contentType string }{
		{"truncated", `<rss version="2.0"><channel><title>FAKE_CREDENTIAL</title>`, "application/xml"},
		{"broken root", `<rss version="2.0`, "text/xml"},
		{"broken atom root", `<a:feed xmlns:a=`, "application/xml"},
		{"entity", `<rss version="2.0"><channel><title>&FAKE_CREDENTIAL;</title></channel></rss>`, "application/rss+xml"},
		{"mismatched tags", `<feed xmlns="http://www.w3.org/2005/Atom"><title>FAKE_CREDENTIAL</bad></feed>`, "application/atom+xml"},
		{"two roots", `<rss version="2.0"><channel/></rss><extra>FAKE_CREDENTIAL</extra>`, "application/xml"},
		{"trailing text", `<rss version="2.0"><channel/></rss>FAKE_CREDENTIAL`, "application/xml"},
		{"no channel", `<rss version="2.0"/>`, "application/rss+xml"},
		{"duplicate channels", `<rss version="2.0"><channel/><channel/></rss>`, "application/rss+xml"},
		{"wrong atom namespace", `<feed xmlns="urn:wrong"/>`, "application/atom+xml"},
		{"wrong rss version", `<rss version="1.0"><channel/></rss>`, "application/xml"},
		{"invalid utf8", "<rss version=\"2.0\"><channel><title>\xff</title></channel></rss>", "application/xml"},
		{"feed mime wrong root", `<error>FAKE_CREDENTIAL</error>`, "application/atom+xml"},
		{"feed mime no XML", `FAKE_CREDENTIAL`, "application/rss+xml"},
		{"generic XML unsupported encoding", `<?xml version="1.0" encoding="made-up-FAKE_CREDENTIAL"?><rss version="2.0"><channel/></rss>`, "application/xml"},
		{"unsupported encoding", `<?xml version="1.0" encoding="made-up-FAKE_CREDENTIAL"?><rss version="2.0"><channel/></rss>`, "application/rss+xml"},
	} {
		t.Run(test.name, func(t *testing.T) {
			tool, base := serveWebFeed(t, test.body, test.contentType)
			args, _ := json.Marshal(WebFetchArgs{URL: base})
			result, err := tool.Execute(context.Background(), args)
			if err == nil || err.Error() != "invalid, unsupported, or oversized RSS/Atom feed" || result != "" {
				t.Fatalf("expected sanitized feed failure, got result=%q err=%v", result, err)
			}
		})
	}
}

func TestWebFetchFeedNoExternalEntitiesOrRequests(t *testing.T) {
	var requests atomic.Int32
	tool, base := newWebFeedTestTool(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/feed" {
			t.Errorf("unexpected external request: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(`<!DOCTYPE rss SYSTEM "http://entities.example.test/dtd" [<!ENTITY external SYSTEM "http://entities.example.test/secret">]>
<rss version="2.0"><channel><item><title>&external;</title></item></channel></rss>`))
	})
	args, _ := json.Marshal(WebFetchArgs{URL: base + "/feed"})
	if output, err := tool.Execute(context.Background(), args); err == nil || output != "" {
		t.Fatal("external entity was not safely rejected")
	}
	if requests.Load() != 1 {
		t.Fatalf("made %d requests, want only feed", requests.Load())
	}
	// External declarations and links are inert even when the feed is valid.
	tool, base = newWebFeedTestTool(t, func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write([]byte(`<!DOCTYPE rss SYSTEM "http://entities.example.test/dtd"><rss version="2.0"><channel><item>
<title>Only fetch feed</title><link>https://article.example.test/story</link><description>&lt;img src="https://image.example.test/a"/&gt;Summary</description>
</item></channel></rss>`))
	})
	result, _ := executeWebFeed(t, tool, WebFetchArgs{URL: base})
	assertWebFeedContains(t, result.Content, "[Only fetch feed](<https://article.example.test/story>)", "Feed summary: Summary")
	if requests.Load() != 2 {
		t.Fatalf("made %d total requests, want two feeds only", requests.Load())
	}
}

func TestWebFetchFeedEmptyAndSummaryOnly(t *testing.T) {
	for _, body := range []string{
		`<rss version="2.0"><channel><title>Empty</title></channel></rss>`,
		`<feed xmlns="http://www.w3.org/2005/Atom"><title>Empty</title></feed>`,
	} {
		tool, base := serveWebFeed(t, body, "application/xml")
		result, _ := executeWebFeed(t, tool, WebFetchArgs{URL: base})
		assertWebFeedContains(t, result.Content, "No items in feed.")
		assertWebFeedExcludes(t, result.Content, "Published:", "Updated:", "1.")
		if result.Truncated {
			t.Fatal("empty feed marked truncated")
		}
	}
	tool, base := serveWebFeed(t, `<rss version="2.0"><channel><item><description>Just a feed summary</description></item></channel></rss>`, "application/xml")
	result, _ := executeWebFeed(t, tool, WebFetchArgs{URL: base})
	assertWebFeedContains(t, result.Content, "1. Untitled item", "Source link unavailable.", "Published: not provided", "Feed summary: Just a feed summary")
}

func TestWebFetchFeedUnicodeTruncation(t *testing.T) {
	tool, base := serveWebFeed(t, `<rss version="2.0"><channel><title>é😀界</title><item><title>é😀界 repeated</title><description>More é😀界</description></item></channel></rss>`, "application/xml")
	full, _ := executeWebFeed(t, tool, WebFetchArgs{URL: base})
	unicodeOffset := utf8.RuneCountInString(strings.SplitN(full.Content, "é😀界", 2)[0])
	for _, limit := range []int{1, unicodeOffset + 1, unicodeOffset + 2, unicodeOffset + 3, full.Length, full.Length + 1} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			result, _ := executeWebFeed(t, tool, WebFetchArgs{URL: base, MaxChars: limit})
			wantLength := min(full.Length, limit)
			if result.Content != string([]rune(full.Content)[:wantLength]) || result.Length != wantLength || result.Truncated != (limit < full.Length) {
				t.Fatalf("incorrect truncation at %d: %+v", limit, result)
			}
		})
	}
}

func TestWebFetchFeedItemLimitReportsOmissionAndValidatesTail(t *testing.T) {
	for _, atom := range []bool{false, true} {
		prefix, item, suffix := `<rss version="2.0"><channel>`, `<item><title>Brief</title></item>`, `</channel></rss>`
		if atom {
			prefix, item, suffix = `<feed xmlns="http://www.w3.org/2005/Atom">`, `<entry><title>Brief</title></entry>`, `</feed>`
		}
		for _, count := range []int{maxFeedItems, maxFeedItems + 3} {
			tool, base := serveWebFeed(t, prefix+strings.Repeat(item, count)+suffix, "application/xml")
			result, _ := executeWebFeed(t, tool, WebFetchArgs{URL: base})
			if result.Truncated != (count > maxFeedItems) || strings.Count(result.Content, "Published: not provided") != maxFeedItems {
				t.Fatal("item bound not honestly reported")
			}
			if count > maxFeedItems {
				assertWebFeedContains(t, result.Content, "Items omitted: 3 (feed item limit: 100).")
			} else {
				assertWebFeedExcludes(t, result.Content, "Items omitted")
			}
		}
		tool, base := serveWebFeed(t, prefix+strings.Repeat(item, maxFeedItems)+strings.Replace(item, "Brief", "&FAKE_CREDENTIAL;", 1)+suffix, "application/xml")
		args, _ := json.Marshal(WebFetchArgs{URL: base})
		if output, err := tool.Execute(context.Background(), args); output != "" || err == nil {
			t.Fatal("malformed tail beyond item limit was accepted")
		}
	}
}

func TestWebFetchFeedBodyLimitAndBrokerSerializationBound(t *testing.T) {
	prefix, suffix := `<rss version="2.0"><channel><description>`, `</description></channel></rss>`
	for _, extra := range []int{1, 100} {
		body := prefix + strings.Repeat("x", maxBodySize-len(prefix)-len(suffix)+extra) + suffix
		tool, base := serveWebFeed(t, body, "application/rss+xml")
		args, _ := json.Marshal(WebFetchArgs{URL: base})
		if output, err := tool.Execute(context.Background(), args); output != "" || err == nil {
			t.Fatalf("body size %d accepted as a complete feed", len(body))
		}
	}
	body := prefix + strings.Repeat("😀\u2028&amp;", brokeredWebFetchMaxChars) + suffix
	tool, base := serveWebFeed(t, body, "application/rss+xml")
	tool.maxChars, tool.maxURLBytes = brokeredWebFetchMaxChars, brokeredWebFetchMaxURLBytes
	requestURL := base + "/?" + strings.Repeat("&", brokeredWebFetchMaxURLBytes-len(base)-2)
	result, output := executeWebFeed(t, tool, WebFetchArgs{URL: requestURL, MaxChars: brokeredWebFetchMaxChars})
	if len(output) > harnessv2.MaxMCPResultBytes || result.Length != brokeredWebFetchMaxChars || !result.Truncated {
		t.Fatalf("brokered bound violated: bytes=%d length=%d truncated=%v", len(output), result.Length, result.Truncated)
	}
}

func TestWebFetchFeedAcceptsExactByteLimitOnlyAtEOF(t *testing.T) {
	for _, format := range []struct{ name, prefix, suffix, extractor string }{
		{"rss", `<rss version="2.0"><channel><title>Exact</title>`, `</channel></rss>`, "rss_feed"},
		{"atom", `<feed xmlns="http://www.w3.org/2005/Atom"><title>Exact</title>`, `</feed>`, "atom_feed"},
	} {
		for _, bom := range []string{"", "\xef\xbb\xbf"} {
			t.Run(fmt.Sprintf("%s/bom=%t", format.name, bom != ""), func(t *testing.T) {
				body := bom + format.prefix + strings.Repeat(" ", maxBodySize-len(bom)-len(format.prefix)-len(format.suffix)) + format.suffix
				tool, base := serveWebFeed(t, body, "application/xml")
				result, _ := executeWebFeed(t, tool, WebFetchArgs{URL: base})
				if result.Extractor != format.extractor || result.Truncated {
					t.Fatalf("complete exact-limit feed was not preserved: extractor=%s truncated=%t", result.Extractor, result.Truncated)
				}
				assertWebFeedContains(t, result.Content, "feed: Exact", "No items in feed.")
				// The same complete prefix must not hide additional response data.
				tooLarge, largeURL := serveWebFeed(t, body+" ", "application/xml")
				input, err := json.Marshal(WebFetchArgs{URL: largeURL})
				if err != nil {
					t.Fatal(err)
				}
				if output, err := tooLarge.Execute(t.Context(), input); err == nil || output != "" {
					t.Fatal("response past the byte cap was accepted as a complete feed")
				}
			})
		}
	}
}

func TestWebFetchBodyOverflowReportsOmissionAfterHTMLExtraction(t *testing.T) {
	body := "<p>Visible</p>" + strings.Repeat(" ", maxBodySize)
	tool, base := serveWebFeed(t, body, "text/html")
	result, _ := executeWebFeed(t, tool, WebFetchArgs{URL: base})
	if result.Content != "Visible" || result.Extractor != "html_text" || !result.Truncated {
		t.Fatalf("physical response truncation was not reported: %+v", result)
	}
}
