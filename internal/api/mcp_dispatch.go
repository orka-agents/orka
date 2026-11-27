/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
	"github.com/valyala/fasthttp"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/orka-agents/orka/internal/redact"
)

const mcpRESTResponseLimit = 8 << 20

func mcpReplayHeaders(contextTokens ContextTokenConfig) []string {
	headers := []string{
		AuthHeader, XAPIKeyHeader, fiber.HeaderHost, fiber.HeaderUserAgent, "Traceparent", "Tracestate", "Baggage",
	}
	for _, profile := range contextTokens.Profiles {
		for _, header := range profile.Headers {
			if header.Name != "" && !slices.ContainsFunc(headers, func(name string) bool { return strings.EqualFold(name, header.Name) }) {
				headers = append(headers, header.Name)
			}
		}
	}
	return headers
}

type mcpRESTResponse struct {
	status int
	body   []byte
}

type mcpRESTError struct {
	Status  int
	Message string
}

func (e *mcpRESTError) Error() string {
	return fmt.Sprintf("Orka API returned %d: %s", e.Status, e.Message)
}

var errMCPResponseTooLarge = &mcpRESTError{Status: fiber.StatusInsufficientStorage, Message: "response is too large for MCP; use the REST API"}

type mcpReplayDeadlineKey struct{}

func adoptMCPReplayDeadline(c fiber.Ctx) error {
	deadline, ok := c.RequestCtx().UserValue(mcpReplayDeadlineKey{}).(time.Time)
	if !ok {
		return c.Next()
	}
	ctx, cancel := context.WithDeadline(c.Context(), deadline)
	defer cancel()
	c.SetContext(ctx)
	return c.Next()
}

func mcpReplayDeadlinePassed(c fiber.Ctx) bool {
	deadline, ok := c.Locals(mcpReplayDeadlineKey{}).(time.Time)
	return ok && !time.Now().Before(deadline)
}

func (m *mcpServer) rest(c fiber.Ctx, method, path string, query url.Values, body any) (mcpRESTResponse, error) {
	request := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(request)
	for _, name := range m.replayHeaders {
		for _, value := range c.Request().Header.PeekAll(name) {
			if strings.EqualFold(name, fiber.HeaderHost) {
				request.Header.SetHostBytes(value)
				continue
			}
			request.Header.AddBytesV(name, value)
		}
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
	if deadline, ok := c.Locals(mcpReplayDeadlineKey{}).(time.Time); ok {
		replay.SetUserValue(mcpReplayDeadlineKey{}, deadline)
	}
	m.handler(&replay)
	response := &replay.Response
	var data []byte
	if stream := response.BodyStream(); stream != nil {
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
		return mcpRESTResponse{}, errMCPResponseTooLarge
	}
	result := mcpRESTResponse{status: response.StatusCode(), body: append([]byte(nil), data...)}
	if result.status < 200 || result.status > 299 {
		return result, &mcpRESTError{Status: result.status, Message: mcpRESTErrorMessage(result.body)}
	}
	return result, nil
}

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

func mcpTaskUIDQuery(namespace, uid string) url.Values {
	query := mcpNamespaceQuery(namespace)
	query.Set(taskUIDQueryParam, uid)
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
