// Package providerproxy holds the security-critical request/response
// filtering shared by Orka's provider-facing HTTP proxies: the standalone
// orka-provider-auth-proxy binary and the ACP supervisor's per-session
// provider proxy. Both proxies sit between untrusted callers and provider
// credentials, so the header allow/deny decisions, path-traversal checks,
// upstream target construction, and response byte bounds live here exactly
// once — a hardening fix in this package reaches every proxy.
package providerproxy

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	authorizationHeader      = "Authorization"
	proxyAuthorizationHeader = "Proxy-Authorization"
	apiKeyHeader             = "X-Api-Key"
	legacyAPIKeyHeader       = "Api-Key"
	contentEncodingHeader    = "Content-Encoding"
)

// ErrResponseTooLarge reports an upstream response body that exceeds the
// proxy's configured byte limit.
var ErrResponseTooLarge = errors.New("provider upstream response exceeds limit")

// ErrDestinationWrite wraps a failure to deliver upstream bytes to the
// downstream client (it closed its side of the connection). The upstream
// stream itself was healthy up to that point, so callers that account
// upstream outcomes must not treat it as an upstream failure.
var ErrDestinationWrite = errors.New("provider proxy destination write failed")

// HasUnsafePathSegment reports whether a proxied URL path must be rejected
// before it is joined onto the upstream base URL. It applies the strictest
// union of the checks the two provider proxies previously implemented
// independently — every path either proxy rejected before is still rejected:
//
//   - any "." or ".." path segment, checked on both the raw path and its
//     percent-decoded form, so percent-encoded traversal ("%2e%2e") cannot
//     survive a later decode upstream;
//   - any backslash or NUL byte, again in both raw and decoded forms, so
//     Windows-style separators and string-truncation tricks cannot smuggle
//     traversal past the segment check;
//   - malformed percent-encoding, which is rejected outright because it
//     cannot be normalized safely.
func HasUnsafePathSegment(path string) bool {
	if hasUnsafeRawPath(path) {
		return true
	}
	decoded, err := url.PathUnescape(path)
	if err != nil {
		return true
	}
	return hasUnsafeRawPath(decoded)
}

func hasUnsafeRawPath(path string) bool {
	if strings.ContainsAny(path, "\\\x00") {
		return true
	}
	for segment := range strings.SplitSeq(path, "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

// Target joins a proxied request path and raw query onto the upstream base
// URL. Callers must reject unsafe paths with HasUnsafePathSegment first.
func Target(base *url.URL, requestPath, rawQuery string) *url.URL {
	target := *base
	target.Path = strings.TrimSuffix(base.Path, "/") + "/" + strings.TrimPrefix(requestPath, "/")
	target.RawPath = ""
	target.RawQuery = rawQuery
	return &target
}

// CopyRequestHeaders copies caller request headers onto the upstream request,
// dropping hop-by-hop headers, headers nominated by Connection, and headers
// that could leak caller credentials, identity, or routing metadata to the
// provider. Header values are never logged or printed here.
func CopyRequestHeaders(destination, source http.Header) {
	blocked := blockedHeaders(source)
	for name, values := range source {
		canonical := http.CanonicalHeaderKey(name)
		if blocked[canonical] || isSensitiveRequestHeader(canonical) {
			continue
		}
		for _, value := range values {
			destination.Add(canonical, value)
		}
	}
}

// CopyResponseHeaders copies upstream response headers back to the caller,
// dropping hop-by-hop headers, headers nominated by Connection, and headers
// that could leak provider credentials or redirect the caller elsewhere.
func CopyResponseHeaders(destination, source http.Header) {
	blocked := blockedHeaders(source)
	for name, values := range source {
		canonical := http.CanonicalHeaderKey(name)
		if blocked[canonical] || isSensitiveResponseHeader(canonical) {
			continue
		}
		for _, value := range values {
			destination.Add(canonical, value)
		}
	}
}

// HasDisallowedContentEncoding reports whether the header declares a
// Content-Encoding other than identity. The proxies refuse compressed bodies
// in both directions so byte limits apply to the true payload size.
func HasDisallowedContentEncoding(header http.Header) bool {
	encoding := strings.TrimSpace(header.Get(contentEncodingHeader))
	return encoding != "" && !strings.EqualFold(encoding, "identity")
}

func blockedHeaders(header http.Header) map[string]bool {
	blocked := map[string]bool{
		"Connection":             true,
		"Keep-Alive":             true,
		"Proxy-Authenticate":     true,
		proxyAuthorizationHeader: true,
		"Proxy-Connection":       true,
		"Te":                     true,
		"Trailer":                true,
		"Transfer-Encoding":      true,
		"Upgrade":                true,
	}
	for _, connection := range header.Values("Connection") {
		for name := range strings.SplitSeq(connection, ",") {
			name = http.CanonicalHeaderKey(strings.TrimSpace(name))
			if name != "" {
				blocked[name] = true
			}
		}
	}
	return blocked
}

func isSensitiveRequestHeader(name string) bool {
	switch name {
	case authorizationHeader, proxyAuthorizationHeader, apiKeyHeader, legacyAPIKeyHeader,
		"Cookie", "Set-Cookie", "Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto",
		"X-Real-Ip", "X-Forwarded-Prefix", "X-Original-Url", "X-Rewrite-Url", "X-Envoy-Original-Path",
		"X-Http-Method-Override", "Txn-Token", "Origin", "Referer", "Openai-Organization", "Openai-Project",
		"Anthropic-Organization-Id", "Traceparent", "Tracestate", "Baggage", contentEncodingHeader, "Expect":
		return true
	default:
		return strings.HasPrefix(name, "X-Orka-") || strings.HasPrefix(name, "X-Forwarded-") || strings.HasPrefix(name, "Sec-Fetch-")
	}
}

func isSensitiveResponseHeader(name string) bool {
	switch name {
	case authorizationHeader, proxyAuthorizationHeader, apiKeyHeader, legacyAPIKeyHeader,
		"Set-Cookie", "Set-Cookie2", "Location", "Server", "Alt-Svc", "Www-Authenticate", "Proxy-Authenticate",
		contentEncodingHeader:
		return true
	default:
		return false
	}
}

// StreamBoundedResponse copies the upstream response body to the caller,
// enforcing the byte limit as it streams. It never writes more than limit
// bytes: the read that would cross the limit returns ErrResponseTooLarge
// without forwarding the offending chunk, and reads never run more than one
// byte past the limit. When flusher is non-nil it is flushed after every
// write so streamed (for example server-sent event) responses propagate
// promptly; pass nil to keep the destination's default buffering.
func StreamBoundedResponse(destination io.Writer, source io.Reader, limit int64, flusher http.Flusher) error {
	remaining := limit
	buffer := make([]byte, 32<<10)
	for {
		readSize := len(buffer)
		if int64(readSize) > remaining+1 {
			readSize = int(remaining + 1)
		}
		n, readErr := source.Read(buffer[:readSize])
		if int64(n) > remaining {
			return ErrResponseTooLarge
		}
		if n > 0 {
			written, writeErr := destination.Write(buffer[:n])
			remaining -= int64(written)
			if writeErr != nil {
				return fmt.Errorf("%w: %w", ErrDestinationWrite, writeErr)
			}
			if written != n {
				return fmt.Errorf("%w: %w", ErrDestinationWrite, io.ErrShortWrite)
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

// TryAcquireSlot attempts to take one request slot without blocking.
func TryAcquireSlot(slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// ReleaseSlot returns a request slot taken with TryAcquireSlot.
func ReleaseSlot(slots chan struct{}) {
	<-slots
}

// WriteError writes a plain-text, non-cacheable proxy error response. The
// message must never contain credential material.
func WriteError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, message+"\n")
}
