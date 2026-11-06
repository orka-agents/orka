/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
	"github.com/valyala/fasthttp"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/orka-agents/orka/internal/redact"
)

// mcpRESTResponseLimit bounds how much of a replayed REST response the MCP
// layer reads. Handlers already bound their own collections; this is a
// backstop against unexpectedly large Task objects or artifacts.
const mcpRESTResponseLimit = 8 << 20

// mcpReplayStrippedHeaders are request headers that describe the MCP
// message rather than the caller. Credentials and every other caller header,
// including configured context-token headers, are forwarded unchanged so the
// replayed request authenticates exactly as the MCP request did.
var mcpReplayStrippedHeaders = []string{
	fiber.HeaderContentLength,
	fiber.HeaderContentType,
	fiber.HeaderAccept,
	fiber.HeaderAcceptEncoding,
	fiber.HeaderTransferEncoding,
	fiber.HeaderConnection,
	fiber.HeaderOrigin,
	"Expect",
	"Mcp-Session-Id",
	"Mcp-Protocol-Version",
	mcpMethodHeader,
	"Mcp-Name",
	"Last-Event-ID",
}

type mcpRESTResponse struct {
	status int
	body   []byte
}

// mcpRESTError is a REST refusal or failure, reported to the MCP client with
// the REST status and a redacted, bounded message.
type mcpRESTError struct {
	Status  int
	Message string
}

func (e *mcpRESTError) Error() string {
	return fmt.Sprintf("Orka API returned %d: %s", e.Status, e.Message)
}

// rest replays one public REST request in process with the MCP caller's
// credentials. The full middleware chain and route authorization run again,
// so a tool can never do more than the caller could do through REST.
func (m *mcpServer) rest(c fiber.Ctx, method, path string, query url.Values, body any) (mcpRESTResponse, error) {
	request := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(request)
	c.Request().Header.CopyTo(&request.Header)
	for _, name := range mcpReplayStrippedHeaders {
		request.Header.Del(name)
	}
	uri := path
	if encoded := query.Encode(); encoded != "" {
		uri += "?" + encoded
	}
	request.Header.SetMethod(method)
	request.SetRequestURI(uri)
	request.Header.Set(fiber.HeaderAccept, fiber.MIMEApplicationJSON)
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return mcpRESTResponse{}, fmt.Errorf("encode REST request: %w", err)
		}
		request.Header.SetContentType(fiber.MIMEApplicationJSON)
		request.SetBody(data)
	}

	var replay fasthttp.RequestCtx
	replay.Init(request, c.RequestCtx().RemoteAddr(), nil)
	m.handler(&replay)
	response := &replay.Response
	var data []byte
	if stream := response.BodyStream(); stream != nil {
		// None of the replayed routes stream today; read a stream bounded
		// rather than letting Body() buffer it whole.
		read, err := io.ReadAll(io.LimitReader(stream, mcpRESTResponseLimit+1))
		_ = response.CloseBodyStream()
		if err != nil {
			return mcpRESTResponse{}, fmt.Errorf("read REST response: %w", err)
		}
		data = read
	} else {
		data = response.Body()
	}
	if len(data) > mcpRESTResponseLimit {
		return mcpRESTResponse{}, &mcpRESTError{Status: fiber.StatusInsufficientStorage, Message: "response is too large for MCP; use the REST API"}
	}
	result := mcpRESTResponse{status: response.StatusCode(), body: append([]byte(nil), data...)}
	if result.status < 200 || result.status > 299 {
		return result, &mcpRESTError{Status: result.status, Message: mcpRESTErrorMessage(result.body)}
	}
	return result, nil
}

// restJSON replays a REST request and decodes its JSON response.
func (m *mcpServer) restJSON(c fiber.Ctx, method, path string, query url.Values, body, target any) error {
	response, err := m.rest(c, method, path, query, body)
	if err != nil {
		return err
	}
	if target == nil || len(response.body) == 0 {
		return nil
	}
	if err := json.Unmarshal(response.body, target); err != nil {
		return fmt.Errorf("decode REST response: %w", err)
	}
	return nil
}

func mcpRESTErrorMessage(body []byte) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	message := ""
	if json.Unmarshal(body, &envelope) == nil {
		message = envelope.Error.Message
	}
	if strings.TrimSpace(message) == "" {
		message = "request failed"
	}
	return mcpSafeText(message, 512)
}

// mcpSafeText redacts credential shapes and bounds a short text field.
func mcpSafeText(value string, limit int) string {
	value = redact.SensitiveText(value)
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + "…"
}

func mcpNamespaceQuery(namespace string) url.Values {
	query := url.Values{}
	if namespace != "" {
		query.Set(toolNamespaceArg, namespace)
	}
	return query
}

func mcpTaskPath(name string, suffix ...string) string {
	var path strings.Builder
	path.WriteString("/api/v1/tasks/" + name)
	for _, part := range suffix {
		path.WriteString("/" + part)
	}
	return path.String()
}

func validateMCPNamespace(namespace string) error {
	if namespace == "" {
		return nil
	}
	if errs := validation.IsDNS1123Label(namespace); len(errs) > 0 {
		return mcpArgumentError("namespace must be a valid Kubernetes namespace name")
	}
	return nil
}

func validateMCPObjectName(field, name string) error {
	if name == "" {
		return mcpArgumentError(field + " is required")
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return mcpArgumentError(field + " must be a valid Kubernetes object name")
	}
	return nil
}
