/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

const (
	healthPath    = "/healthz"
	readinessPath = "/readyz"
)

var (
	errRedirectDenied  = errors.New("upstream redirects are denied")
	errTunnelLimit     = errors.New("tunnel byte limit exceeded")
	errRequestTooLarge = errors.New("request body limit exceeded")
)

type scmEgressProxy struct {
	config        proxyConfig
	authenticator *proxyAuthenticator
	client        *http.Client
	requestSlots  chan struct{}
}

func newSCMEgressProxy(config proxyConfig, authenticator *proxyAuthenticator) (*scmEgressProxy, error) {
	normalized, err := normalizeProxyConfig(config)
	if err != nil {
		return nil, err
	}
	if authenticator == nil {
		return nil, fmt.Errorf("proxy authenticator is required")
	}
	proxy := &scmEgressProxy{
		config: normalized, authenticator: authenticator,
		requestSlots: make(chan struct{}, normalized.MaxConcurrent),
	}
	proxy.client = proxy.newForwardClient(nil)
	return proxy, nil
}

func (p *scmEgressProxy) newForwardClient(tlsConfig *tls.Config) *http.Client {
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            p.dialAllowedHost,
		ForceAttemptHTTP2:      false,
		DisableKeepAlives:      true,
		DisableCompression:     true,
		TLSClientConfig:        tlsConfig,
		TLSHandshakeTimeout:    p.config.ConnectTimeout,
		ResponseHeaderTimeout:  p.config.ResponseHeaderTimeout,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: p.config.MaxResponseHeader,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   p.config.ForwardTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errRedirectDenied
		},
	}
}

func (p *scmEgressProxy) hostAllowed(host string) bool {
	_, allowed := p.config.AllowedHosts[host]
	return allowed
}

func (p *scmEgressProxy) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if p.serveProbe(writer, request) {
		return
	}
	if !p.authenticator.authorized(request) {
		writer.Header().Set("Proxy-Authenticate", `Basic realm="orka-scm-egress"`)
		writeProxyError(writer, http.StatusProxyAuthRequired, "proxy authentication required")
		return
	}
	if requestHeaderBytes(request) > p.config.MaxRequestHeaderBytes {
		writeProxyError(writer, http.StatusRequestHeaderFieldsTooLarge, "request headers exceed proxy limit")
		return
	}
	if !tryAcquire(p.requestSlots) {
		writeProxyError(writer, http.StatusTooManyRequests, "proxy capacity is exhausted")
		return
	}
	defer releaseSlot(p.requestSlots)
	if request.Method == http.MethodConnect {
		p.handleConnect(writer, request)
		return
	}
	p.handleForward(writer, request)
}

func (p *scmEgressProxy) serveProbe(writer http.ResponseWriter, request *http.Request) bool {
	if request.Method != http.MethodGet || request.URL.IsAbs() {
		return false
	}
	if request.URL.Path != healthPath && request.URL.Path != readinessPath {
		return false
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(writer, "ok\n")
	return true
}

func (p *scmEgressProxy) handleConnect(writer http.ResponseWriter, request *http.Request) {
	if request.ContentLength > 0 || request.TransferEncoding != nil {
		writeProxyError(writer, http.StatusBadRequest, "CONNECT request body is forbidden")
		return
	}
	host, port, err := splitTarget(request.Host)
	if err != nil || !p.hostAllowed(host) {
		writeProxyError(writer, http.StatusForbidden, "target is not allowed")
		return
	}
	upstream, err := p.dialAllowedHost(request.Context(), "tcp", net.JoinHostPort(host, port))
	if err != nil {
		writeDialError(writer, err)
		return
	}
	hijacker, ok := writer.(http.Hijacker)
	if !ok {
		_ = upstream.Close()
		writeProxyError(writer, http.StatusInternalServerError, "CONNECT is unavailable")
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		_ = upstream.Close()
		return
	}
	if err := client.SetDeadline(time.Time{}); err != nil {
		_ = client.Close()
		_ = upstream.Close()
		return
	}
	if err := upstream.SetDeadline(time.Time{}); err != nil {
		_ = client.Close()
		_ = upstream.Close()
		return
	}
	p.runTunnel(client, upstream, buffered)
}

func (p *scmEgressProxy) runTunnel(client, upstream net.Conn, buffered *bufio.ReadWriter) {
	defer func() { _ = client.Close() }()
	defer func() { _ = upstream.Close() }()
	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := buffered.Flush(); err != nil {
		return
	}
	clientBudget := p.config.MaxTunnelBytes
	if buffered.Reader.Buffered() > 0 {
		count, err := copyBuffered(upstream, buffered.Reader, clientBudget, p.config.IdleTimeout)
		if err != nil {
			return
		}
		clientBudget -= count
	}
	p.copyTunnel(client, upstream, clientBudget)
}

func copyBuffered(destination net.Conn, reader *bufio.Reader, limit int64, idleTimeout time.Duration) (int64, error) {
	buffered := reader.Buffered()
	if int64(buffered) > limit {
		return 0, errTunnelLimit
	}
	data := make([]byte, buffered)
	if _, err := io.ReadFull(reader, data); err != nil {
		return 0, err
	}
	deadline := &deadlineConn{Conn: destination, idleTimeout: idleTimeout}
	written, err := io.CopyBuffer(deadline, bytes.NewReader(data), make([]byte, 32<<10))
	return written, err
}

func (p *scmEgressProxy) copyTunnel(client, upstream net.Conn, clientBudget int64) {
	results := make(chan error, 2)
	go func() {
		results <- boundedTunnelCopy(
			&deadlineConn{Conn: upstream, idleTimeout: p.config.IdleTimeout},
			&deadlineConn{Conn: client, idleTimeout: p.config.IdleTimeout},
			clientBudget,
		)
	}()
	go func() {
		results <- boundedTunnelCopy(
			&deadlineConn{Conn: client, idleTimeout: p.config.IdleTimeout},
			&deadlineConn{Conn: upstream, idleTimeout: p.config.IdleTimeout},
			p.config.MaxTunnelBytes,
		)
	}()
	timer := time.NewTimer(p.config.TunnelTimeout)
	defer timer.Stop()
	select {
	case <-results:
	case <-timer.C:
	}
}

func boundedTunnelCopy(destination io.Writer, source io.Reader, limit int64) error {
	written, err := io.CopyBuffer(destination, io.LimitReader(source, limit+1), make([]byte, 32<<10))
	if written > limit {
		return errTunnelLimit
	}
	return err
}

type deadlineConn struct {
	net.Conn
	idleTimeout time.Duration
}

func (c *deadlineConn) Read(value []byte) (int, error) {
	if err := c.SetReadDeadline(time.Now().Add(c.idleTimeout)); err != nil {
		return 0, err
	}
	return c.Conn.Read(value)
}

func (c *deadlineConn) Write(value []byte) (int, error) {
	if err := c.SetWriteDeadline(time.Now().Add(c.idleTimeout)); err != nil {
		return 0, err
	}
	return c.Conn.Write(value)
}

func (p *scmEgressProxy) handleForward(writer http.ResponseWriter, request *http.Request) {
	target, err := forwardTarget(request)
	if err != nil || !p.hostAllowed(target.Hostname()) {
		writeProxyError(writer, http.StatusForbidden, "target is not allowed")
		return
	}
	body, err := readRequestBody(request, p.config.MaxRequestBytes)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errRequestTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		writeProxyError(writer, status, "request body is invalid or exceeds proxy limit")
		return
	}
	outbound := outboundRequest(request, target, body)
	response, err := p.client.Do(outbound)
	if err != nil {
		writeForwardError(writer, err)
		return
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < http.StatusBadRequest {
		writeProxyError(writer, http.StatusBadGateway, "upstream redirect is forbidden")
		return
	}
	if responseHeaderBytes(response.Header) > p.config.MaxResponseHeader {
		writeProxyError(writer, http.StatusBadGateway, "upstream response headers exceed proxy limit")
		return
	}
	responseBody, err := readBounded(response.Body, p.config.MaxResponseBytes)
	if err != nil {
		writeProxyError(writer, http.StatusBadGateway, "upstream response exceeds proxy limit")
		return
	}
	copyResponseHeaders(writer.Header(), response.Header)
	writer.WriteHeader(response.StatusCode)
	_, _ = writer.Write(responseBody)
}

func forwardTarget(request *http.Request) (*url.URL, error) {
	if request.Method == http.MethodTrace || !request.URL.IsAbs() || request.URL.Scheme != "https" ||
		request.URL.User != nil || request.URL.Fragment != "" || request.URL.Opaque != "" {
		return nil, errHostDenied
	}
	if request.URL.Port() != "" && request.URL.Port() != "443" {
		return nil, errHostDenied
	}
	if err := validateHostname(request.URL.Hostname()); err != nil {
		return nil, errHostDenied
	}
	target := *request.URL
	target.Host = targetAddress(target.Hostname())
	return &target, nil
}

func readRequestBody(request *http.Request, limit int64) ([]byte, error) {
	if request.ContentLength > limit {
		return nil, errRequestTooLarge
	}
	if request.Body == nil {
		return nil, nil
	}
	defer func() { _ = request.Body.Close() }()
	return readBounded(request.Body, limit)
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errRequestTooLarge
	}
	return data, nil
}

func outboundRequest(request *http.Request, target *url.URL, body []byte) *http.Request {
	outbound := request.Clone(request.Context())
	outbound.URL = target
	outbound.RequestURI = ""
	outbound.Host = target.Hostname()
	outbound.Header = request.Header.Clone()
	stripHopByHopHeaders(outbound.Header)
	outbound.Header.Del("Proxy-Authorization")
	outbound.Header.Del("Content-Length")
	outbound.Body = io.NopCloser(bytes.NewReader(body))
	outbound.ContentLength = int64(len(body))
	outbound.TransferEncoding = nil
	outbound.Trailer = nil
	return outbound
}

func stripHopByHopHeaders(header http.Header) {
	for _, value := range header.Values("Connection") {
		for token := range strings.SplitSeq(value, ",") {
			header.Del(textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(token)))
		}
	}
	for _, name := range []string{
		"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Proxy-Connection", "Te", "Trailer",
		"Transfer-Encoding", "Upgrade",
	} {
		header.Del(name)
	}
}

func copyResponseHeaders(destination, source http.Header) {
	cloned := source.Clone()
	stripHopByHopHeaders(cloned)
	for name, values := range cloned {
		for _, value := range values {
			destination.Add(name, value)
		}
	}
}

func requestHeaderBytes(request *http.Request) int64 {
	return int64(len(request.Method) + len(request.Host) + len(request.URL.String()) + 4 + headerBytes(request.Header))
}

func responseHeaderBytes(header http.Header) int64 { return int64(headerBytes(header)) }

func headerBytes(header http.Header) int {
	total := 0
	for name, values := range header {
		for _, value := range values {
			total += len(name) + len(value) + 4
		}
	}
	return total
}

func writeDialError(writer http.ResponseWriter, err error) {
	if errors.Is(err, errHostDenied) || errors.Is(err, errAddressDenied) {
		writeProxyError(writer, http.StatusForbidden, "target is not allowed")
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeProxyError(writer, http.StatusGatewayTimeout, "target connection timed out")
		return
	}
	writeProxyError(writer, http.StatusBadGateway, "target connection failed")
}

func writeForwardError(writer http.ResponseWriter, err error) {
	if errors.Is(err, errRedirectDenied) {
		writeProxyError(writer, http.StatusBadGateway, "upstream redirect is forbidden")
		return
	}
	writeDialError(writer, err)
}

func writeProxyError(writer http.ResponseWriter, status int, message string) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)
	_, _ = io.WriteString(writer, message+"\n")
}

func tryAcquire(slots chan struct{}) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func releaseSlot(slots chan struct{}) { <-slots }
