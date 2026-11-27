package v2

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"
)

const MCPForwardProtocolAzureResourceManager = "AzureResourceManager"

const (
	maxForwardPathBytes  = 4 << 10
	maxForwardQueryBytes = 4 << 10
)

var MCPForwardInputSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["method","path"],"properties":{"method":{"type":"string","enum":["GET","HEAD"]},"path":{"type":"string"},"query":{"type":"string"}}}`)

var MCPForwardWriteInputSchema = json.RawMessage(`{"type":"object","additionalProperties":false,"required":["method","path"],"properties":{"method":{"type":"string","enum":["PUT","PATCH","DELETE","POST"]},"path":{"type":"string"},"query":{"type":"string"},"body":{"type":["object","array"]},"ifMatch":{"type":"string"},"ifNoneMatch":{"type":"string"}}}`)

const MCPForwardWriteSuffix = ":write"

func MCPForwardWriteToolName(name string) string {
	return name + MCPForwardWriteSuffix
}

func MCPForwardToolResource(name string) string {
	return strings.TrimSuffix(name, MCPForwardWriteSuffix)
}

type MCPToolForward struct {
	Protocol string `json:"protocol"`
	Origin   string `json:"origin"`
	Write    bool   `json:"write,omitempty"`
}

func (f MCPToolForward) InputSchema() json.RawMessage {
	if f.Write {
		return MCPForwardWriteInputSchema
	}
	return MCPForwardInputSchema
}

func (f MCPToolForward) Validate() error {
	if f.Protocol != MCPForwardProtocolAzureResourceManager {
		return fmt.Errorf("unsupported forwarding protocol %q", f.Protocol)
	}
	if _, err := ForwardOrigin(f.Origin); err != nil {
		return err
	}
	return nil
}

func ForwardOrigin(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" ||
		strings.ContainsAny(parsed.Host, "{}") {
		return "", fmt.Errorf("forwarding origin must be an https origin with no path")
	}
	return "https://" + ForwardHost(parsed.Host), nil
}

func ForwardHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(host), ":443")
}

type MCPForwardRequest struct {
	Method      string          `json:"method"`
	Path        string          `json:"path"`
	Query       string          `json:"query,omitempty"`
	Body        json.RawMessage `json:"body,omitempty"`
	IfMatch     string          `json:"ifMatch,omitempty"`
	IfNoneMatch string          `json:"ifNoneMatch,omitempty"`
}

const maxForwardPreconditionBytes = 1024

func validForwardPrecondition(value string) bool {
	if len(value) > maxForwardPreconditionBytes {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return false
		}
	}
	return true
}

func forwardBodyIsStructured(body json.RawMessage) bool {
	trimmed := bytes.TrimSpace(body)
	return json.Valid(trimmed) && len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[')
}

func ForwardWriteMethod(method string) bool {
	switch method {
	case http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodPost:
		return true
	}
	return false
}

func DecodeMCPForwardRequest(arguments json.RawMessage) (MCPForwardRequest, error) {
	decoder := json.NewDecoder(bytes.NewReader(arguments))
	decoder.DisallowUnknownFields()
	var request MCPForwardRequest
	if err := decoder.Decode(&request); err != nil {
		return MCPForwardRequest{}, fmt.Errorf("forwarding arguments must be an object with method, path, query, and body")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return MCPForwardRequest{}, fmt.Errorf("forwarding arguments have trailing data")
	}
	return request, request.Validate()
}

func (r MCPForwardRequest) Validate() error {
	write := ForwardWriteMethod(r.Method)
	if !write && r.Method != http.MethodGet && r.Method != http.MethodHead {
		return fmt.Errorf("forwarding Tools carry GET, HEAD, PUT, PATCH, DELETE, and POST requests only")
	}
	if len(r.Body) > 0 && (!write || !forwardBodyIsStructured(r.Body)) {
		return fmt.Errorf("only a forwarded write carries a body, and it must be a JSON object or array")
	}
	if (r.IfMatch != "" || r.IfNoneMatch != "") && (!write || !validForwardPrecondition(r.IfMatch) || !validForwardPrecondition(r.IfNoneMatch)) {
		return fmt.Errorf("only a forwarded write carries If-Match or If-None-Match, as printable values of at most %d bytes", maxForwardPreconditionBytes)
	}
	if err := validateForwardPath(r.Path); err != nil {
		return err
	}
	return validateForwardQuery(r.Query)
}

func (r MCPForwardRequest) ValidateFor(forward MCPToolForward) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if ForwardWriteMethod(r.Method) != forward.Write {
		if forward.Write {
			return fmt.Errorf("a forwarding write tool carries only PUT, PATCH, DELETE, and POST requests")
		}
		return fmt.Errorf("forwarding Tools carry only GET and HEAD requests; writes go to the write tool")
	}
	return nil
}

func (r MCPForwardRequest) Target(origin string) (*url.URL, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	base, err := ForwardOrigin(origin)
	if err != nil {
		return nil, err
	}
	target, err := url.Parse(base + r.Path)
	if err != nil {
		return nil, fmt.Errorf("forwarding path is not a valid URL path")
	}
	target.RawQuery = r.Query
	if "https://"+ForwardHost(target.Host) != base {
		return nil, fmt.Errorf("forwarding request left the Tool's origin")
	}
	return target, nil
}

func validateForwardPath(path string) error {
	if path == "" || path[0] != '/' || strings.HasPrefix(path, "//") || len(path) > maxForwardPathBytes {
		return fmt.Errorf("forwarding path must be absolute and at most %d bytes", maxForwardPathBytes)
	}
	for i := 0; i < len(path); i++ {
		if !forwardPathByte(path[i]) {
			return fmt.Errorf("forwarding path contains a character that is not allowed")
		}
		if path[i] == '%' {
			if i+2 >= len(path) || !isHexDigit(path[i+1]) || !isHexDigit(path[i+2]) {
				return fmt.Errorf("forwarding path contains an invalid escape")
			}
			switch strings.ToUpper(path[i+1 : i+3]) {
			case "2F", "5C", "2E", "00":
				return fmt.Errorf("forwarding path must not encode separators or dots")
			}
		}
	}
	decoded, err := url.PathUnescape(path)
	if err != nil || !utf8.ValidString(decoded) {
		return fmt.Errorf("forwarding path contains an invalid escape")
	}
	for _, segment := range strings.Split(decoded, "/")[1:] {
		if segment == "." || segment == ".." {
			return fmt.Errorf("forwarding path must not contain dot segments")
		}
	}
	if strings.Contains(decoded, "//") {
		return fmt.Errorf("forwarding path must not contain empty segments")
	}
	for _, r := range decoded {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("forwarding path must not contain control characters")
		}
	}
	return nil
}

func validateForwardQuery(query string) error {
	if len(query) > maxForwardQueryBytes {
		return fmt.Errorf("forwarding query must be at most %d bytes", maxForwardQueryBytes)
	}
	for i := 0; i < len(query); i++ {
		if !forwardQueryByte(query[i]) {
			return fmt.Errorf("forwarding query contains a character that is not allowed")
		}
	}
	if _, err := url.ParseQuery(query); err != nil {
		return fmt.Errorf("forwarding query is not well formed")
	}
	return nil
}

func forwardPathByte(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	return strings.IndexByte("-._~!$&'()*+,;=:@/%", c) >= 0
}

func forwardQueryByte(c byte) bool {
	return forwardPathByte(c) || c == '?'
}

func isHexDigit(c byte) bool {
	return ('0' <= c && c <= '9') || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')
}

type MCPForwardResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

var ForwardResponseHeaders = []string{
	"Content-Type", "Etag", "Location", "Azure-Asyncoperation", "Retry-After",
	"X-Ms-Request-Id", "X-Ms-Correlation-Request-Id", ForwardHeadContentLength,
}

const ForwardHeadContentLength = "Content-Length"

func (r MCPForwardResponse) Validate() error {
	if r.Status < 100 || r.Status > 599 {
		return fmt.Errorf("forwarding response status is out of range")
	}
	for name, value := range r.Headers {
		allowed := slices.Contains(ForwardResponseHeaders, name)
		if !allowed || strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("forwarding response carries a header that is not allowed")
		}
	}
	if !utf8.ValidString(r.Body) {
		return fmt.Errorf("forwarding response body must be UTF-8 text")
	}
	return nil
}

func DecodeMCPForwardResponse(result json.RawMessage) (MCPForwardResponse, error) {
	decoder := json.NewDecoder(bytes.NewReader(result))
	decoder.DisallowUnknownFields()
	var response MCPForwardResponse
	if err := decoder.Decode(&response); err != nil {
		return MCPForwardResponse{}, fmt.Errorf("forwarding result is not a forwarding response")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return MCPForwardResponse{}, fmt.Errorf("forwarding result has trailing data")
	}
	return response, response.Validate()
}
