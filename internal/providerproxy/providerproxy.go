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

var ErrResponseTooLarge = errors.New("provider upstream response exceeds limit")

var ErrDestinationWrite = errors.New("provider proxy destination write failed")

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

func Target(base *url.URL, requestPath, rawQuery string) *url.URL {
	target := *base
	target.Path = strings.TrimSuffix(base.Path, "/") + "/" + strings.TrimPrefix(requestPath, "/")
	target.RawPath = ""
	target.RawQuery = rawQuery
	return &target
}

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
	case Codex429RouteHeader, Codex429RetryHeader:
		return true
	case authorizationHeader, proxyAuthorizationHeader, apiKeyHeader, legacyAPIKeyHeader,
		"Set-Cookie", "Set-Cookie2", "Location", "Server", "Alt-Svc", "Www-Authenticate", "Proxy-Authenticate",
		contentEncodingHeader:
		return true
	default:
		return false
	}
}

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

func TryAcquireSlot(slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func ReleaseSlot(slots chan struct{}) {
	<-slots
}

func WriteError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, message+"\n")
}
