package worker

import (
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

// forwardResultEnvelopeBytes leaves room in the broker's result bound for the
// status, headers, and JSON escaping around a forwarded body.
const forwardResultEnvelopeBytes = 64 << 10

// isForwardingTool reports whether a Tool forwards a client's requests.
func isForwardingTool(tool *corev1alpha1.Tool) bool {
	return tool != nil && tool.Spec.HTTP != nil && tool.Spec.HTTP.Forward != nil
}

// AllowForwarding lets this executor run forwarding Tools. Only the ACP broker
// sets it: there the runtime built the call from a client request and the
// broker checked it against the prompt's frozen tool policy. A native worker,
// or the internal connector endpoint, refuses forwarding Tools.
func (e *ToolExecutor) AllowForwarding() { e.forwarding = true }

func toolForwardProtocol(tool *corev1alpha1.Tool) corev1alpha1.HTTPForwardProtocol {
	if !isForwardingTool(tool) {
		return ""
	}
	return tool.Spec.HTTP.Forward.Protocol
}

// prepareForwardRequest builds the request a forwarding Tool call names: a
// read on the Tool's own origin, with no body and no headers from the call.
// The outbound access policy then adds the credential as for any Tool.
func (e *ToolExecutor) prepareForwardRequest(ctx context.Context, tool *corev1alpha1.Tool, args json.RawMessage) (preparedToolRequest, error) {
	if !e.forwarding {
		return preparedToolRequest{}, fmt.Errorf("tool %q forwards a command-line client's requests and runs only through an ACP runtime", tool.Name)
	}
	httpConfig, routeHost, err := toolHTTPConfig(tool)
	if err != nil {
		return preparedToolRequest{}, err
	}
	if routeHost != "" || tool.Spec.MCP != nil || httpConfig.AuthSecretRef != nil || len(httpConfig.Headers) > 0 ||
		httpConfig.OutboundAccessPolicyRef == nil {
		return preparedToolRequest{}, errors.New("a forwarding tool takes its credential only from its outbound access policy")
	}
	request, err := harnessv2.DecodeMCPForwardRequest(args)
	if err != nil {
		return preparedToolRequest{}, err
	}
	target, err := request.Target(httpConfig.URL)
	if err != nil {
		return preparedToolRequest{}, err
	}
	req, err := http.NewRequestWithContext(ctx, request.Method, target.String(), nil)
	if err != nil {
		return preparedToolRequest{}, fmt.Errorf("failed to create forwarded request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	prepared := preparedToolRequest{httpConfig: httpConfig, request: req, forward: true}
	if err := e.applyOutboundAccessPolicy(ctx, tool, args, &prepared); err != nil {
		return preparedToolRequest{}, err
	}
	if !prepared.direct {
		return preparedToolRequest{}, errors.New("a forwarding tool needs a credential-injecting outbound access policy")
	}
	return prepared, nil
}

// executeForwardRequest returns the upstream answer as it is, whatever its
// status, so the client sees the service's own errors. Redirects are never
// followed: the credential stays on the Tool's origin, and the client gets the
// redirect to handle.
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
	// JSON escaping can grow the body past the broker's result bound even
	// when the raw body fits; that is a controlled tool failure, not a
	// broker error.
	if len(encoded) > harnessv2.MaxMCPResultBytes {
		return "", ToolExecutionError{Err: fmt.Errorf("forwarded response exceeds %d bytes once encoded", harnessv2.MaxMCPResultBytes)}
	}
	return string(encoded), nil
}

// forwardContinuationRe matches an Azure Resource Manager continuation
// parameter, `$skiptoken` (or its escaped `%24skiptoken`), in a nextLink or a
// URL-valued header. A JSON body may escape the separator as `\u0026` and the
// dollar as `\u0024`; the value stops at a backslash, so a following escaped
// separator ends it.
var forwardContinuationRe = regexp.MustCompile(`(?i)([?&]|\\u0026)((?:\$|%24|\\u0024)skiptoken=[^&#\s"'<>\\]+)`)

// redactForwardedText redacts a forwarded response as other tool results are,
// but keeps continuation parameters: the generic `token=` rule would replace a
// `$skiptoken` value, and the gateway would then refuse the next page's
// request. The credentials Orka injected are still removed everywhere first,
// continuation parameters included.
func redactForwardedText(text string, secrets ...string) string {
	text = redactForwardedSecrets(text, secrets)
	var kept []string
	masked := forwardContinuationRe.ReplaceAllStringFunc(text, func(match string) string {
		parts := forwardContinuationRe.FindStringSubmatch(match)
		// A continuation token is kept as is only when it carries no
		// injected credential in any encoding the client would undo.
		if name, value, ok := strings.Cut(parts[2], "="); ok && forwardValueHoldsSecret(value, secrets) {
			return parts[1] + name + "=[REDACTED]"
		}
		kept = append(kept, parts[2])
		return parts[1] + "\x00orka-forward-continuation-" + strconv.Itoa(len(kept)-1) + "\x00"
	})
	redacted := redactToolSensitiveText(masked)
	for index, value := range kept {
		redacted = strings.Replace(redacted, "\x00orka-forward-continuation-"+strconv.Itoa(index)+"\x00", value, 1)
	}
	return redacted
}

// redactForwardedSecrets removes the credentials Orka injected from text,
// as they are and in any percent encoding a client would undo.
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

// redactPercentEncodedSecrets finds a credential the upstream percent-encoded
// in a non-standard way, such as `%2D` for "-", anywhere in text. The text is
// decoded with each decoded byte mapped back to its source bytes, and only
// the source of each match is replaced, so the rest of a URL is unchanged.
func redactPercentEncodedSecrets(text string, secrets []string) string {
	if !strings.Contains(text, "%") {
		return text
	}
	decoded := make([]byte, 0, len(text))
	// source[i] is where decoded byte i starts in text; a final entry marks
	// the end.
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
	var spans [][2]int
	for _, secret := range secrets {
		if secret = strings.TrimSpace(secret); secret == "" {
			continue
		}
		for offset := 0; ; {
			index := strings.Index(string(decoded[offset:]), secret)
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

// forwardValueHoldsSecret reports whether a query value, once URL decoded
// (any percent escape, not only the standard ones), contains an injected
// credential.
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

// forwardSensitiveKeyRe matches a JSON key whose string value is a
// credential, as the text redactor's assignment rule does for prose.
var forwardSensitiveKeyRe = regexp.MustCompile(`(?i)^[a-z0-9_.$-]*(?:api[-_]?key|token|secret|password|passwd|pwd|credential|private[-_]?key|client[-_]?secret)[a-z0-9_.-]*$`)

// forwardContinuationKeyRe matches the JSON keys that carry paging state,
// whose values are kept so the next page can be requested.
var forwardContinuationKeyRe = regexp.MustCompile(`(?i)^(?:\$?skiptoken|continuationtoken|nextlink)$`)

// redactForwardedBody redacts a forwarded response body. The credentials Orka
// injected are removed from the raw bytes first. A JSON body is then redacted
// value by value: a string under a credential-named key becomes
// "[REDACTED]" and every other string goes through the text rules, while
// arrays, objects, numbers, and nulls are never rewritten, so a valid
// response stays valid JSON. Other bodies are redacted as text.
func redactForwardedBody(body string, secrets ...string) string {
	// A JSON body is judged token by token, so redaction can never leave it
	// malformed; a raw replacement could split a number or a literal.
	if json.Valid([]byte(body)) {
		if redacted, ok := redactForwardedJSON(body, secrets); ok {
			return redacted
		}
	}
	return redactForwardedText(body, secrets...)
}

// redactForwardedJSON rewrites only the string literals whose redacted
// value differs, copying every other byte of body unchanged. Strings are
// judged decoded, so a credential the upstream escaped (`\u002d` for "-") is
// still found, in keys as in values.
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
	// rewrite replaces the string literal the decoder just read.
	// rewrite replaces the token the decoder just read with a JSON string.
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
			// A number holding an injected credential becomes a string; the
			// literals true, false, and null cannot hold one.
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
