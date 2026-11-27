package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const forwardResultEnvelopeBytes = 64 << 10

func isForwardingTool(tool *corev1alpha1.Tool) bool {
	return tool != nil && tool.Spec.HTTP != nil && tool.Spec.HTTP.Forward != nil
}

func (e *ToolExecutor) AllowForwarding() { e.forwarding = true }

func toolForwardProtocol(tool *corev1alpha1.Tool) corev1alpha1.HTTPForwardProtocol {
	if !isForwardingTool(tool) {
		return ""
	}
	return tool.Spec.HTTP.Forward.Protocol
}

func (e *ToolExecutor) prepareForwardRequest(ctx context.Context, tool *corev1alpha1.Tool, args json.RawMessage) (preparedToolRequest, error) {
	if !e.forwarding {
		return preparedToolRequest{}, fmt.Errorf("tool %q forwards a command-line client's requests and runs only through an ACP runtime", tool.Name)
	}
	httpConfig, err := toolHTTPConfig(tool)
	if err != nil {
		return preparedToolRequest{}, err
	}
	if tool.Spec.MCP != nil || httpConfig.AuthSecretRef != nil || len(httpConfig.Headers) > 0 ||
		httpConfig.OutboundAccessPolicyRef == nil {
		return preparedToolRequest{}, errors.New("a forwarding tool takes its credential only from its outbound access policy")
	}
	request, err := harnessv2.DecodeMCPForwardRequest(args)
	if err != nil {
		return preparedToolRequest{}, err
	}
	write := harnessv2.ForwardWriteMethod(request.Method)
	if write != (tool.Spec.BrokeredToolClass == corev1alpha1.AgentRuntimeBrokeredToolClassWrite) ||
		(write && tool.Spec.HTTP.Forward.Writes == nil) {
		return preparedToolRequest{}, fmt.Errorf("tool %q does not forward %s requests", tool.Name, request.Method)
	}
	target, err := request.Target(httpConfig.URL)
	if err != nil {
		return preparedToolRequest{}, err
	}
	var body io.Reader
	if len(request.Body) > 0 {
		body = bytes.NewReader(request.Body)
	}
	req, err := http.NewRequestWithContext(ctx, request.Method, target.String(), body)
	if err != nil {
		return preparedToolRequest{}, fmt.Errorf("failed to create forwarded request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if len(request.Body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if request.IfMatch != "" {
		req.Header.Set("If-Match", request.IfMatch)
	}
	if request.IfNoneMatch != "" {
		req.Header.Set("If-None-Match", request.IfNoneMatch)
	}
	prepared := preparedToolRequest{httpConfig: httpConfig, request: req, forward: true}
	if err := e.applyOutboundAccessPolicy(ctx, tool, args, &prepared); err != nil {
		return preparedToolRequest{}, err
	}
	if !prepared.direct {
		return preparedToolRequest{}, errors.New("a forwarding tool needs a credential-injecting outbound access policy")
	}
	return prepared, nil
}

func executeForwardRequest(httpClient *http.Client, prepared preparedToolRequest) (string, error) {
	client := *httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(prepared.request)
	if err != nil {
		return "", ToolRequestAttemptedError{Err: safeToolTransportError(prepared.request.Context(), err, prepared.redactionSecrets...)}
	}
	defer resp.Body.Close() //nolint:errcheck
	limit := int64(harnessv2.MaxMCPResultBytes - forwardResultEnvelopeBytes)
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return "", ToolRequestAttemptedError{Err: fmt.Errorf("failed to read forwarded response: %w", err)}
	}
	if int64(len(body)) > limit {
		return "", ToolExecutionError{Err: fmt.Errorf("forwarded response exceeds %d bytes", limit)}
	}
	if !utf8.Valid(body) {
		return "", ToolExecutionError{Err: errors.New("forwarded response is not UTF-8 text")}
	}
	response := harnessv2.MCPForwardResponse{
		Status: resp.StatusCode,
		Body:   redactForwardedBody(string(body), prepared.redactionSecrets...),
	}
	for _, name := range harnessv2.ForwardResponseHeaders {
		value := strings.TrimSpace(resp.Header.Get(name))
		if name == harnessv2.ForwardHeadContentLength {
			value = ""
			if prepared.request.Method == http.MethodHead && resp.ContentLength >= 0 {
				value = strconv.FormatInt(resp.ContentLength, 10)
			}
		}
		if value == "" || strings.ContainsAny(value, "\r\n\x00") {
			continue
		}
		if response.Headers == nil {
			response.Headers = map[string]string{}
		}
		response.Headers[name] = redactForwardedText(value, prepared.redactionSecrets...)
	}
	if err := response.Validate(); err != nil {
		return "", ToolExecutionError{Err: err}
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return "", err
	}
	if len(encoded) > harnessv2.MaxMCPResultBytes {
		return "", ToolExecutionError{Err: fmt.Errorf("forwarded response exceeds %d bytes once encoded", harnessv2.MaxMCPResultBytes)}
	}
	return string(encoded), nil
}

var forwardContinuationRe = regexp.MustCompile(`(?i)([?&]|\\u0026)((?:\$|%24|\\u0024)skiptoken=[^&#\s"'<>\\]+)`)

func redactForwardedText(text string, secrets ...string) string {
	text = redactForwardedSecrets(text, secrets)
	var kept []string
	masked := forwardContinuationRe.ReplaceAllStringFunc(text, func(match string) string {
		parts := forwardContinuationRe.FindStringSubmatch(match)
		if name, value, ok := strings.Cut(parts[2], "="); ok && forwardValueHoldsSecret(value, secrets) {
			return parts[1] + name + "=[REDACTED]"
		}
		kept = append(kept, parts[2])
		return parts[1] + "\x00orka-forward-continuation-" + strconv.Itoa(len(kept)-1) + "\x00"
	})
	redacted := redactToolSensitiveText(masked)
	if len(kept) == 0 {
		return redacted
	}
	return forwardContinuationMarkerRe.ReplaceAllStringFunc(redacted, func(marker string) string {
		index, err := strconv.Atoi(marker[len("\x00orka-forward-continuation-") : len(marker)-1])
		if err != nil || index >= len(kept) {
			return marker
		}
		return kept[index]
	})
}

var forwardContinuationMarkerRe = regexp.MustCompile("\x00orka-forward-continuation-[0-9]+\x00")

func redactForwardedSecrets(text string, secrets []string) string {
	for _, secret := range secrets {
		if secret = strings.TrimSpace(secret); secret != "" {
			for _, form := range []string{secret, url.QueryEscape(secret), url.PathEscape(secret)} {
				text = strings.ReplaceAll(text, form, "[REDACTED]")
			}
		}
	}
	return redactPercentEncodedSecrets(text, secrets)
}

func redactPercentEncodedSecrets(text string, secrets []string) string {
	if !strings.Contains(text, "%") {
		return text
	}
	decoded := make([]byte, 0, len(text))
	source := make([]int, 0, len(text)+1)
	for i := 0; i < len(text); {
		source = append(source, i)
		if text[i] == '%' && i+2 < len(text) && isHexDigit(text[i+1]) && isHexDigit(text[i+2]) {
			value, _ := strconv.ParseUint(text[i+1:i+3], 16, 8)
			decoded = append(decoded, byte(value))
			i += 3
			continue
		}
		decoded = append(decoded, text[i])
		i++
	}
	source = append(source, len(text))
	plain := string(decoded)
	var spans [][2]int
	for _, secret := range secrets {
		if secret = strings.TrimSpace(secret); secret == "" {
			continue
		}
		for offset := 0; ; {
			index := strings.Index(plain[offset:], secret)
			if index < 0 {
				break
			}
			start := offset + index
			spans = append(spans, [2]int{source[start], source[start+len(secret)]})
			offset = start + len(secret)
		}
	}
	if len(spans) == 0 {
		return text
	}
	slices.SortFunc(spans, func(a, b [2]int) int { return a[0] - b[0] })
	var out strings.Builder
	copied := 0
	for _, span := range spans {
		if span[1] <= copied {
			continue
		}
		if span[0] > copied {
			out.WriteString(text[copied:span[0]])
		}
		out.WriteString("[REDACTED]")
		copied = span[1]
	}
	out.WriteString(text[copied:])
	return out.String()
}

func isHexDigit(c byte) bool {
	return ('0' <= c && c <= '9') || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')
}

func forwardValueHoldsSecret(value string, secrets []string) bool {
	decoded, err := url.QueryUnescape(value)
	if err != nil {
		decoded = value
	}
	for _, secret := range secrets {
		if secret = strings.TrimSpace(secret); secret != "" && strings.Contains(decoded, secret) {
			return true
		}
	}
	return false
}

var forwardSensitiveKeyRe = regexp.MustCompile(`(?i)^[a-z0-9_.$-]*(?:api[-_]?key|token|secret|password|passwd|pwd|credential|private[-_]?key|client[-_]?secret)[a-z0-9_.-]*$`)

var forwardContinuationKeyRe = regexp.MustCompile(`(?i)^(?:\$?skiptoken|continuationtoken|nextlink)$`)

func redactForwardedBody(body string, secrets ...string) string {
	if json.Valid([]byte(body)) {
		if redacted, ok := redactForwardedJSON(body, secrets); ok {
			return redacted
		}
	}
	return redactForwardedText(body, secrets...)
}

func redactForwardedJSON(body string, secrets []string) (string, bool) {
	type container struct {
		object    bool
		expectKey bool
		key       string
	}
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.UseNumber()
	var out strings.Builder
	var stack []*container
	copied := 0
	valueDone := func() {
		if len(stack) > 0 && stack[len(stack)-1].object {
			stack[len(stack)-1].expectKey = true
		}
	}
	var previous, end int
	rewrite := func(replacement string) bool {
		between := body[previous:end]
		start := len(between) - len(strings.TrimLeft(between, " \t\r\n:,"))
		encoded, err := json.Marshal(replacement)
		if err != nil || start >= len(between) {
			return false
		}
		out.WriteString(body[copied : previous+start])
		out.Write(encoded)
		copied = end
		return true
	}
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", false
		}
		end = int(decoder.InputOffset())
		var top *container
		if len(stack) > 0 {
			top = stack[len(stack)-1]
		}
		switch value := token.(type) {
		case json.Delim:
			switch value {
			case '{':
				stack = append(stack, &container{object: true, expectKey: true})
			case '[':
				stack = append(stack, &container{})
			default:
				stack = stack[:len(stack)-1]
				valueDone()
			}
		case string:
			if top != nil && top.object && top.expectKey {
				top.key, top.expectKey = value, false
				if redactedKey := redactForwardedSecrets(value, secrets); redactedKey != value {
					if !rewrite(redactedKey) {
						return "", false
					}
				}
				break
			}
			redacted := redactForwardedText(value, secrets...)
			if top != nil && top.object && !forwardContinuationKeyRe.MatchString(top.key) && forwardSensitiveKeyRe.MatchString(top.key) {
				redacted = "[REDACTED]"
			}
			if redacted != value && !rewrite(redacted) {
				return "", false
			}
			valueDone()
		default:
			if number, ok := value.(json.Number); ok && redactForwardedSecrets(number.String(), secrets) != number.String() {
				if !rewrite("[REDACTED]") {
					return "", false
				}
			}
			valueDone()
		}
		previous = end
	}
	out.WriteString(body[copied:])
	return out.String(), true
}
