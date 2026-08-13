package providerproxy

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestHasUnsafePathSegment(t *testing.T) {
	for _, test := range []struct {
		name   string
		path   string
		unsafe bool
	}{
		{name: "empty", path: "", unsafe: false},
		{name: "root", path: "/", unsafe: false},
		{name: "benign api path", path: "/v1/messages", unsafe: false},
		{name: "benign nested path", path: "/base/v1/chat/completions", unsafe: false},
		{name: "dots inside a segment", path: "/v1.2/models.json", unsafe: false},
		{name: "segment starting with dots", path: "/..hidden/x", unsafe: false},
		{name: "benign query-free asterisk", path: "*", unsafe: false},
		{name: "raw parent traversal", path: "/../secrets", unsafe: true},
		{name: "raw parent traversal mid-path", path: "/v1/../admin", unsafe: true},
		{name: "raw parent traversal at end", path: "/v1/..", unsafe: true},
		{name: "bare parent segment", path: "..", unsafe: true},
		{name: "raw current-directory segment", path: "/./v1", unsafe: true},
		{name: "bare current-directory segment", path: ".", unsafe: true},
		{name: "percent-encoded parent traversal", path: "/%2e%2e/secrets", unsafe: true},
		{name: "percent-encoded parent traversal uppercase", path: "/v1/%2E%2E", unsafe: true},
		{name: "percent-encoded current-directory segment", path: "/%2e/v1", unsafe: true},
		{name: "percent-encoded slash traversal", path: "/a%2f..%2fb", unsafe: true},
		{name: "mixed encoded parent segment", path: "/.%2e/x", unsafe: true},
		{name: "raw backslash", path: "/a\\b", unsafe: true},
		{name: "raw windows traversal", path: "/a\\..\\b", unsafe: true},
		{name: "percent-encoded backslash", path: "/a%5Cb", unsafe: true},
		{name: "percent-encoded backslash lowercase", path: "/a%5cb", unsafe: true},
		{name: "raw NUL byte", path: "/a\x00b", unsafe: true},
		{name: "percent-encoded NUL byte", path: "/a%00b", unsafe: true},
		{name: "malformed percent encoding", path: "/a%zzb", unsafe: true},
		{name: "truncated percent encoding", path: "/a%2", unsafe: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := HasUnsafePathSegment(test.path); got != test.unsafe {
				t.Fatalf("HasUnsafePathSegment(%q) = %t, want %t", test.path, got, test.unsafe)
			}
		})
	}
}

func TestCopyRequestHeadersDropsSensitiveAndHopByHopHeaders(t *testing.T) {
	source := http.Header{}
	for name, value := range map[string]string{
		"Authorization":             "Bearer caller-credential",
		"Proxy-Authorization":       "Bearer proxy-credential",
		"X-Api-Key":                 "caller-key",
		"Api-Key":                   "caller-legacy-key",
		"Cookie":                    "caller=cookie",
		"Set-Cookie":                "caller=cookie",
		"Forwarded":                 "for=203.0.113.1",
		"X-Forwarded-For":           "203.0.113.1",
		"X-Forwarded-Anything":      "anything",
		"X-Real-Ip":                 "203.0.113.1",
		"X-Original-Url":            "/original",
		"X-Rewrite-Url":             "/rewrite",
		"X-Envoy-Original-Path":     "/envoy",
		"X-Http-Method-Override":    "DELETE",
		"Txn-Token":                 "transaction",
		"Origin":                    "http://caller.example",
		"Referer":                   "http://caller.example/page",
		"Openai-Organization":       "org",
		"Openai-Project":            "project",
		"Anthropic-Organization-Id": "org",
		"Traceparent":               "00-trace",
		"Tracestate":                "vendor=1",
		"Baggage":                   "key=value",
		"Content-Encoding":          "gzip",
		"Expect":                    "100-continue",
		"X-Orka-Internal":           "internal",
		"Sec-Fetch-Site":            "cross-site",
		"Keep-Alive":                "timeout=5",
		"Te":                        "trailers",
		"Trailer":                   "Expires",
		"Transfer-Encoding":         "chunked",
		"Upgrade":                   "websocket",
		"Proxy-Connection":          "keep-alive",
		"Proxy-Authenticate":        "Basic",
		"X-Connection-Named":        "remove-me",
	} {
		source.Set(name, value)
	}
	source.Set("Connection", "X-Connection-Named")
	source.Add("Content-Type", "application/json")
	source.Add("Accept", "application/json")
	source.Add("Accept", "text/event-stream")
	source.Set("Anthropic-Version", "2023-06-01")
	source.Set("Openai-Beta", "assistants=v2")
	source["x-lowercase-safe"] = []string{"kept"}

	destination := http.Header{}
	CopyRequestHeaders(destination, source)

	for _, name := range []string{
		"Authorization", "Proxy-Authorization", "X-Api-Key", "Api-Key", "Cookie", "Set-Cookie",
		"Forwarded", "X-Forwarded-For", "X-Forwarded-Anything", "X-Real-Ip", "X-Original-Url",
		"X-Rewrite-Url", "X-Envoy-Original-Path", "X-Http-Method-Override", "Txn-Token", "Origin",
		"Referer", "Openai-Organization", "Openai-Project", "Anthropic-Organization-Id",
		"Traceparent", "Tracestate", "Baggage", "Content-Encoding", "Expect", "X-Orka-Internal",
		"Sec-Fetch-Site", "Connection", "Keep-Alive", "Te", "Trailer", "Transfer-Encoding",
		"Upgrade", "Proxy-Connection", "Proxy-Authenticate", "X-Connection-Named",
	} {
		if values := destination.Values(name); len(values) != 0 {
			t.Errorf("sensitive or hop-by-hop request header %s was forwarded: %q", name, values)
		}
	}
	if got := destination.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := destination.Values("Accept"); len(got) != 2 || got[0] != "application/json" || got[1] != "text/event-stream" {
		t.Errorf("Accept values = %q, want both preserved in order", got)
	}
	if got := destination.Get("Anthropic-Version"); got != "2023-06-01" {
		t.Errorf("Anthropic-Version = %q, want preserved", got)
	}
	if got := destination.Get("Openai-Beta"); got != "assistants=v2" {
		t.Errorf("Openai-Beta = %q, want preserved", got)
	}
	if got := destination.Get("X-Lowercase-Safe"); got != "kept" {
		t.Errorf("non-canonical source header was not canonicalized and preserved: %q", got)
	}
}

func TestCopyRequestHeadersDropsNonCanonicalSensitiveNames(t *testing.T) {
	source := http.Header{
		"authorization": []string{"Bearer caller-credential"},
		"x-api-key":     []string{"caller-key"},
		"cookie":        []string{"caller=cookie"},
	}
	destination := http.Header{}
	CopyRequestHeaders(destination, source)
	if len(destination) != 0 {
		t.Fatalf("non-canonical sensitive request headers were forwarded: %v", destination)
	}
}

func TestCopyResponseHeadersDropsSensitiveAndHopByHopHeaders(t *testing.T) {
	source := http.Header{}
	for name, value := range map[string]string{
		"Authorization":       "Bearer provider-credential",
		"Proxy-Authorization": "Bearer provider-proxy-credential",
		"X-Api-Key":           "provider-key",
		"Api-Key":             "provider-legacy-key",
		"Set-Cookie":          "provider=secret",
		"Set-Cookie2":         "provider=secret",
		"Location":            "http://elsewhere.example",
		"Server":              "provider-server",
		"Alt-Svc":             `h3=":443"`,
		"Www-Authenticate":    "Bearer",
		"Proxy-Authenticate":  "Basic",
		"Content-Encoding":    "gzip",
		"Transfer-Encoding":   "chunked",
		"Keep-Alive":          "timeout=5",
		"Upgrade":             "websocket",
		"X-Connection-Named":  "remove-me",
	} {
		source.Set(name, value)
	}
	source.Set("Connection", "X-Connection-Named")
	source.Set("Content-Type", "application/json")
	source.Set("X-Request-Id", "request-1")
	source.Set("Openai-Version", "2020-10-01")

	destination := http.Header{}
	CopyResponseHeaders(destination, source)

	for _, name := range []string{
		"Authorization", "Proxy-Authorization", "X-Api-Key", "Api-Key", "Set-Cookie", "Set-Cookie2",
		"Location", "Server", "Alt-Svc", "Www-Authenticate", "Proxy-Authenticate", "Content-Encoding",
		"Transfer-Encoding", "Keep-Alive", "Upgrade", "Connection", "X-Connection-Named",
	} {
		if values := destination.Values(name); len(values) != 0 {
			t.Errorf("sensitive or hop-by-hop response header %s was forwarded: %q", name, values)
		}
	}
	for name, want := range map[string]string{
		"Content-Type":   "application/json",
		"X-Request-Id":   "request-1",
		"Openai-Version": "2020-10-01",
	} {
		if got := destination.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestHasDisallowedContentEncoding(t *testing.T) {
	for _, test := range []struct {
		name       string
		encoding   string
		disallowed bool
	}{
		{name: "absent", encoding: "", disallowed: false},
		{name: "identity", encoding: "identity", disallowed: false},
		{name: "identity uppercase", encoding: "IDENTITY", disallowed: false},
		{name: "identity padded", encoding: "  identity  ", disallowed: false},
		{name: "gzip", encoding: "gzip", disallowed: true},
		{name: "brotli", encoding: "br", disallowed: true},
		{name: "gzip padded", encoding: " gzip ", disallowed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			header := http.Header{}
			if test.encoding != "" {
				header.Set("Content-Encoding", test.encoding)
			}
			if got := HasDisallowedContentEncoding(header); got != test.disallowed {
				t.Fatalf("HasDisallowedContentEncoding(%q) = %t, want %t", test.encoding, got, test.disallowed)
			}
		})
	}
}

func TestTarget(t *testing.T) {
	for _, test := range []struct {
		name        string
		base        string
		requestPath string
		rawQuery    string
		want        string
	}{
		{name: "root base", base: "http://upstream.example/", requestPath: "/v1/messages", want: "http://upstream.example/v1/messages"},
		{name: "prefixed base", base: "http://upstream.example/base", requestPath: "/v1/responses", rawQuery: "stream=true", want: "http://upstream.example/base/v1/responses?stream=true"},
		{name: "prefixed base with trailing slash", base: "http://upstream.example/base/", requestPath: "/v1/models", want: "http://upstream.example/base/v1/models"},
		{name: "root suffix", base: "http://upstream.example/base", requestPath: "/", want: "http://upstream.example/base/"},
	} {
		t.Run(test.name, func(t *testing.T) {
			base, err := url.Parse(test.base)
			if err != nil {
				t.Fatal(err)
			}
			target := Target(base, test.requestPath, test.rawQuery)
			if target.String() != test.want {
				t.Fatalf("Target(%q, %q, %q) = %q, want %q", test.base, test.requestPath, test.rawQuery, target.String(), test.want)
			}
			if target.RawPath != "" {
				t.Fatalf("Target left RawPath = %q, want empty", target.RawPath)
			}
			if base.Path != mustParse(t, test.base).Path {
				t.Fatal("Target mutated the shared base URL")
			}
		})
	}
}

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

type chunkReader struct {
	data  []byte
	chunk int
}

func (r *chunkReader) Read(buffer []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := min(min(len(buffer), r.chunk), len(r.data))
	copy(buffer, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

type countingFlusher struct{ flushes int }

func (f *countingFlusher) Flush() { f.flushes++ }

func TestStreamBoundedResponse(t *testing.T) {
	t.Run("under limit", func(t *testing.T) {
		var out bytes.Buffer
		if err := StreamBoundedResponse(&out, strings.NewReader("hello"), 10, nil); err != nil {
			t.Fatalf("StreamBoundedResponse() = %v, want nil", err)
		}
		if out.String() != "hello" {
			t.Fatalf("streamed %q, want %q", out.String(), "hello")
		}
	})

	t.Run("exactly at limit", func(t *testing.T) {
		var out bytes.Buffer
		if err := StreamBoundedResponse(&out, strings.NewReader("12345"), 5, nil); err != nil {
			t.Fatalf("StreamBoundedResponse() = %v, want nil", err)
		}
		if out.String() != "12345" {
			t.Fatalf("streamed %q, want %q", out.String(), "12345")
		}
	})

	t.Run("over limit in one read", func(t *testing.T) {
		var out bytes.Buffer
		err := StreamBoundedResponse(&out, strings.NewReader("123456"), 5, nil)
		if !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("StreamBoundedResponse() = %v, want ErrResponseTooLarge", err)
		}
		if int64(out.Len()) > 5 {
			t.Fatalf("streamed %d bytes, want at most the 5-byte limit", out.Len())
		}
	})

	t.Run("over limit across chunks", func(t *testing.T) {
		var out bytes.Buffer
		err := StreamBoundedResponse(&out, &chunkReader{data: []byte("1234567890"), chunk: 3}, 5, nil)
		if !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("StreamBoundedResponse() = %v, want ErrResponseTooLarge", err)
		}
		if out.String() != "123" {
			t.Fatalf("streamed %q, want the chunks that fit under the limit", out.String())
		}
	})

	t.Run("zero limit rejects any body", func(t *testing.T) {
		var out bytes.Buffer
		err := StreamBoundedResponse(&out, strings.NewReader("x"), 0, nil)
		if !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("StreamBoundedResponse() = %v, want ErrResponseTooLarge", err)
		}
		if out.Len() != 0 {
			t.Fatalf("streamed %q, want nothing", out.String())
		}
	})

	t.Run("flushes after every chunk", func(t *testing.T) {
		var out bytes.Buffer
		flusher := &countingFlusher{}
		if err := StreamBoundedResponse(&out, &chunkReader{data: []byte("abcdef"), chunk: 2}, 10, flusher); err != nil {
			t.Fatalf("StreamBoundedResponse() = %v, want nil", err)
		}
		if out.String() != "abcdef" || flusher.flushes != 3 {
			t.Fatalf("streamed %q with %d flushes, want %q with 3 flushes", out.String(), flusher.flushes, "abcdef")
		}
	})
}

func TestTryAcquireAndReleaseSlot(t *testing.T) {
	slots := make(chan struct{}, 1)
	if !TryAcquireSlot(slots) {
		t.Fatal("first acquire failed on an empty slot channel")
	}
	if TryAcquireSlot(slots) {
		t.Fatal("second acquire succeeded past capacity")
	}
	ReleaseSlot(slots)
	if !TryAcquireSlot(slots) {
		t.Fatal("acquire failed after release")
	}
}

func TestWriteError(t *testing.T) {
	recorder := httptest.NewRecorder()
	WriteError(recorder, http.StatusBadGateway, "provider upstream request failed")
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadGateway)
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if recorder.Body.String() != "provider upstream request failed\n" {
		t.Fatalf("body = %q", recorder.Body.String())
	}
}
