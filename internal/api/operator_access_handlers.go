package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/gofiber/fiber/v3"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/operatoraccess"
)

// HandleOperatorExec upgrades GET /api/v1/tasks/:id/exec to a WebSocket and
// relays one operator command to the Task's RuntimeSession supervisor. All
// identity, policy, and class opt-in checks complete before the upgrade, so
// refusals are ordinary HTTP errors. The command arrives as the first stream
// message, keeping argv out of URLs and request headers (and access logs).
func (s *Server) HandleOperatorExec(c fiber.Ctx) error {
	envelope := operatorRequestAudit(c, operatoraccess.OperationExec)
	err := s.handleOperatorExec(c, envelope)
	s.auditOperatorRefusal(c, envelope, err)
	return err
}

func (s *Server) handleOperatorExec(c fiber.Ctx, envelope *operatorAuditEnvelope) error {
	if !websocketUpgradeRequested(c) {
		return fiber.NewError(fiber.StatusUpgradeRequired, "operator exec requires a WebSocket upgrade")
	}
	target, err := s.resolveOperatorTarget(c, operatoraccess.OperationExec)
	if err != nil {
		return err
	}
	// The relay owns every later record of this request.
	envelope.recorded = true
	return hijackWebSocket(c, func(conn *websocket.Conn) {
		s.relayOperatorExec(conn, target)
	})
}

func websocketUpgradeRequested(c fiber.Ctx) bool {
	return c.Method() == fiber.MethodGet &&
		headerHasToken(c.Get(fiber.HeaderConnection), "upgrade") &&
		headerHasToken(c.Get(fiber.HeaderUpgrade), "websocket")
}

func headerHasToken(value, token string) bool {
	for part := range strings.SplitSeq(value, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// hijackWebSocket completes the WebSocket handshake on the raw connection
// after fasthttp hands it over. Authentication and authorization already ran
// in the route middleware and the handler.
func hijackWebSocket(c fiber.Ctx, run func(*websocket.Conn)) error {
	header := http.Header{}
	for key, value := range c.Request().Header.All() {
		header.Add(string(key), string(value))
	}
	request := &http.Request{
		Method: http.MethodGet, URL: &url.URL{Path: c.Path()}, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: header, Host: string(c.Request().Host()),
	}
	ctx := c.RequestCtx()
	ctx.HijackSetNoResponse(true)
	ctx.Hijack(func(conn net.Conn) {
		_ = conn.SetDeadline(time.Time{})
		writer := newHijackResponseWriter(conn)
		ws, err := websocket.Accept(writer, request.WithContext(context.Background()), &websocket.AcceptOptions{
			Subprotocols: []string{operatoraccess.WebSocketSubprotocol},
			// Operator streams authenticate with bearer credentials, never
			// ambient browser cookies, so cross-origin checks add nothing.
			InsecureSkipVerify: true,
		})
		if err != nil {
			writer.flushError()
			return
		}
		ws.SetReadLimit(operatoraccess.MaxFrameBytes + 1)
		run(ws)
	})
	return nil
}

// hijackResponseWriter adapts a hijacked fasthttp connection to the
// net/http handshake API used by the WebSocket library.
type hijackResponseWriter struct {
	conn        net.Conn
	rw          *bufio.ReadWriter
	header      http.Header
	wroteHeader bool
}

func newHijackResponseWriter(conn net.Conn) *hijackResponseWriter {
	return &hijackResponseWriter{
		conn:   conn,
		rw:     bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn)),
		header: http.Header{},
	}
}

func (w *hijackResponseWriter) Header() http.Header { return w.header }

func (w *hijackResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	if status != http.StatusSwitchingProtocols {
		w.header.Set("Connection", "close")
	}
	_, _ = fmt.Fprintf(w.rw, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status))
	_ = w.header.Write(w.rw)
	_, _ = w.rw.WriteString("\r\n")
}

func (w *hijackResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.rw.Write(p)
}

func (w *hijackResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if err := w.rw.Flush(); err != nil {
		return nil, nil, err
	}
	return w.conn, w.rw, nil
}

func (w *hijackResponseWriter) flushError() { _ = w.rw.Flush() }

func sendOperatorStatus(ctx context.Context, conn *websocket.Conn, status operatoraccess.Status) {
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = conn.Write(writeCtx, websocket.MessageBinary, operatoraccess.StatusFrame(status))
	_ = conn.Close(websocket.StatusNormalClosure, status.Reason)
}

func readOperatorExecOptions(ctx context.Context, conn *websocket.Conn) (operatoraccess.ExecOptions, error) {
	readCtx, cancel := context.WithTimeout(ctx, operatorExecOptionsReadTimeout)
	defer cancel()
	kind, payload, err := conn.Read(readCtx)
	if err != nil {
		return operatoraccess.ExecOptions{}, fmt.Errorf("the exec request was not received")
	}
	if kind != websocket.MessageBinary || len(payload) < 2 || payload[0] != operatoraccess.ChannelControl {
		return operatoraccess.ExecOptions{}, fmt.Errorf("the first stream message must be the exec request")
	}
	var envelope struct {
		Type string                     `json:"type"`
		Exec operatoraccess.ExecOptions `json:"exec"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload[1:])))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil || envelope.Type != "exec" {
		return operatoraccess.ExecOptions{}, fmt.Errorf("the exec request is malformed")
	}
	if err := envelope.Exec.Normalize(); err != nil {
		return operatoraccess.ExecOptions{}, err
	}
	return envelope.Exec, nil
}

func (s *Server) relayOperatorExec(client *websocket.Conn, target *operatorTarget) {
	ctx := context.Background()
	record := target.record
	options, err := readOperatorExecOptions(ctx, client)
	if err != nil {
		record.Phase = OperatorAuditDenied
		record.Outcome = err.Error()
		s.operatorAudit(ctx, record)
		sendOperatorStatus(ctx, client, operatoraccess.Status{ExitCode: -1, Reason: operatoraccess.ExitReasonError, Message: err.Error()})
		return
	}
	record.Command = operatoraccess.RedactedCommand(options.Argv)
	record.ArgvDigest = operatoraccess.ArgvDigest(options.Argv)
	record.Argc = len(options.Argv)
	record.TTY = options.TTY
	target.record = record
	fail := func(message string) {
		failed := operatorFinishRecord(target.record, message, nil, 0)
		s.operatorAudit(ctx, failed)
		sendOperatorStatus(ctx, client, operatoraccess.Status{ExitCode: -1, Reason: operatoraccess.ExitReasonError, Message: message})
	}
	refuse := func(err error) {
		denied := target.record
		denied.Phase = OperatorAuditDenied
		denied.Outcome = operatorFailureOutcome(err)
		s.operatorAudit(ctx, denied)
		sendOperatorStatus(ctx, client, operatoraccess.Status{ExitCode: -1, Reason: operatoraccess.ExitReasonError, Message: denied.Outcome})
	}
	// The client chose when to send argv; the authorization it was resolved
	// under must still hold now.
	if err := s.revalidateOperatorTarget(ctx, target, operatoraccess.OperationExec); err != nil {
		refuse(err)
		return
	}
	// Sign before tainting: a valid argv can still encode past the request
	// header bound, and that refusal must not forfeit the workspace.
	request := harnessv2.OperatorExecRequest{
		Protocol: harnessv2.ProtocolVersion, Metadata: target.metadata(operatoraccess.OperationExec),
		AuditID: record.AuditID, Exec: options,
	}
	header, capability, err := harnessv2.SealOperatorRequest(target.secret, &request, &request.Metadata)
	if err != nil {
		refuse(fiber.NewError(fiber.StatusBadRequest, "operator exec request could not be signed: "+err.Error()))
		return
	}
	if err := s.taintOperatorTarget(ctx, target); err != nil {
		if _, refused := errors.AsType[*fiber.Error](err); refused {
			refuse(err)
			return
		}
		log.Error(err, "operator exec refused: taint could not be persisted", "auditID", record.AuditID)
		fail("operator exec refused: the workspace taint could not be persisted")
		return
	}
	path, err := harnessv2.OperatorExecPath(target.sessionID)
	if err != nil {
		fail("runtime session identity is invalid")
		return
	}
	httpHeader := http.Header{}
	httpHeader.Set("Authorization", "Bearer "+target.bearer)
	httpHeader.Set(harnessv2.OperationCapabilityHeader, capability)
	httpHeader.Set(harnessv2.OperatorRequestHeader, header)
	dialCtx, dialCancel := context.WithTimeout(ctx, operatorSupervisorResponseTimeout)
	upstream, response, err := websocket.Dial(dialCtx, "ws"+strings.TrimPrefix(target.endpoint, "http")+path, &websocket.DialOptions{
		HTTPClient: s.operatorHTTPClient(), HTTPHeader: httpHeader,
		Subprotocols: []string{operatoraccess.WebSocketSubprotocol},
	})
	dialCancel()
	if err != nil {
		_, message := supervisorErrorMessage(response)
		fail(message)
		return
	}
	upstream.SetReadLimit(operatoraccess.MaxFrameBytes + 1)
	target.record.StartedAt = time.Now().UTC()
	started := target.record
	started.Phase = OperatorAuditStarted
	s.operatorAudit(ctx, started)

	final, ok := relayOperatorFrames(ctx, client, upstream, time.Duration(options.TimeoutSeconds)*time.Second+time.Minute)
	outcome := final.Reason
	var exitCode *int
	if ok {
		code := final.ExitCode
		exitCode = &code
	} else if outcome == "" {
		outcome = operatoraccess.ExitReasonDisconnect
	}
	s.operatorAudit(ctx, operatorFinishRecord(target.record, outcome, exitCode, 0))
}

// relayOperatorFrames copies validated frames in both directions until the
// supervisor sends the final status or either side disconnects.
func relayOperatorFrames(parent context.Context, client, upstream *websocket.Conn, bound time.Duration) (operatoraccess.Status, bool) {
	ctx, cancel := context.WithTimeout(parent, bound)
	defer cancel()
	var final operatoraccess.Status
	var gotFinal atomic.Bool
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer cancel()
		for {
			kind, payload, err := client.Read(ctx)
			if err != nil {
				return
			}
			if kind != websocket.MessageBinary || len(payload) == 0 || len(payload) > operatoraccess.MaxFrameBytes+1 {
				continue
			}
			if payload[0] != operatoraccess.ChannelStdin && payload[0] != operatoraccess.ChannelControl {
				continue
			}
			if upstream.Write(ctx, websocket.MessageBinary, payload) != nil {
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		defer cancel()
		for {
			kind, payload, err := upstream.Read(ctx)
			if err != nil {
				return
			}
			if kind != websocket.MessageBinary || len(payload) == 0 {
				continue
			}
			switch payload[0] {
			case operatoraccess.ChannelStdout, operatoraccess.ChannelStderr:
			case operatoraccess.ChannelStatus:
				if json.Unmarshal(payload[1:], &final) == nil {
					gotFinal.Store(true)
				}
			default:
				continue
			}
			if client.Write(ctx, websocket.MessageBinary, payload) != nil {
				return
			}
			if gotFinal.Load() {
				return
			}
		}
	}()
	wg.Wait()
	reason := "relay closed"
	if gotFinal.Load() {
		reason = final.Reason
	}
	_ = upstream.Close(websocket.StatusNormalClosure, reason)
	_ = client.Close(websocket.StatusNormalClosure, reason)
	return final, gotFinal.Load()
}

// HandleOperatorFileDownload streams one regular workspace file out of the
// Task's RuntimeSession. Read-only copies never taint the session.
func (s *Server) HandleOperatorFileDownload(c fiber.Ctx) error {
	envelope := operatorRequestAudit(c, operatoraccess.OperationCopyOut)
	err := s.handleOperatorFileDownload(c, envelope)
	s.auditOperatorRefusal(c, envelope, err)
	return err
}

func (s *Server) handleOperatorFileDownload(c fiber.Ctx, envelope *operatorAuditEnvelope) error {
	target, err := s.resolveOperatorTarget(c, operatoraccess.OperationCopyOut)
	if err != nil {
		return err
	}
	cleaned, err := operatoraccess.CleanWorkspacePath(c.Query("path"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	maxBytes, err := parseOperatorBytes(c.Query("maxBytes"))
	if err != nil {
		return err
	}
	target.record.Path = cleaned
	envelope.record = target.record
	if err := s.revalidateOperatorTarget(c.Context(), target, operatoraccess.OperationCopyOut); err != nil {
		return err
	}
	request := harnessv2.OperatorFileRequest{
		Protocol: harnessv2.ProtocolVersion, Metadata: target.metadata(operatoraccess.OperationCopyOut),
		AuditID: target.record.AuditID, Path: cleaned, MaxBytes: operatoraccess.ClampFileBytes(maxBytes),
	}
	started := target.record
	started.Phase = OperatorAuditStarted
	envelope.recorded = true
	s.operatorAudit(c.Context(), started)
	sealed, err := sealOperatorFileRequest(target, &request)
	if err != nil {
		s.operatorAudit(c.Context(), operatorFinishRecord(target.record, operatorFailureOutcome(err), nil, 0))
		return err
	}
	response, err := s.sendOperatorFileRequest(c.Context(), target, &request, sealed, http.MethodGet, nil)
	if err != nil {
		s.operatorAudit(c.Context(), operatorFinishRecord(target.record, operatorFailureOutcome(err), nil, 0))
		return err
	}
	size := response.ContentLength
	if size < 0 || size > request.MaxBytes {
		_ = response.Body.Close()
		s.operatorAudit(c.Context(), operatorFinishRecord(target.record, "invalid supervisor response", nil, 0))
		return fiber.NewError(fiber.StatusBadGateway, "runtime supervisor returned an invalid file response")
	}
	c.Set(fiber.HeaderContentType, "application/octet-stream")
	c.Set("X-Orka-Audit-ID", target.record.AuditID)
	c.Response().SetBodyStream(&auditedOperatorBody{
		body: response.Body, expected: size, onClose: func(copied int64, complete bool) {
			outcome := "completed"
			if !complete {
				outcome = "incomplete"
			}
			s.operatorAudit(context.Background(), operatorFinishRecord(target.record, outcome, nil, copied))
		},
	}, int(size))
	return nil
}

// HandleOperatorFileUpload writes one regular file into the Task's
// RuntimeSession workspace. The session is tainted before the upload starts.
func (s *Server) HandleOperatorFileUpload(c fiber.Ctx) error {
	envelope := operatorRequestAudit(c, operatoraccess.OperationCopyIn)
	err := s.handleOperatorFileUpload(c, envelope)
	s.auditOperatorRefusal(c, envelope, err)
	return err
}

func (s *Server) handleOperatorFileUpload(c fiber.Ctx, envelope *operatorAuditEnvelope) error {
	target, err := s.resolveOperatorTarget(c, operatoraccess.OperationCopyIn)
	if err != nil {
		return err
	}
	cleaned, err := operatoraccess.CleanWorkspacePath(c.Query("path"))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	target.record.Path = cleaned
	envelope.record = target.record
	size, err := strconv.ParseInt(strings.TrimSpace(c.Query("size")), 10, 64)
	if err != nil || size < 0 || size > operatoraccess.MaxFileBytes {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("size must be between 0 and %d bytes", operatoraccess.MaxFileBytes))
	}
	// The body is forwarded through a reader bounded to size, which would hide
	// any excess of a chunked or unframed upload from the helper's exact-size
	// check. Only an upload whose framing proves exactly size bytes is accepted.
	if length := c.Request().Header.ContentLength(); length < 0 || int64(length) != size {
		return fiber.NewError(fiber.StatusLengthRequired, "a Content-Length equal to the declared size is required")
	}
	if err := s.revalidateOperatorTarget(c.Context(), target, operatoraccess.OperationCopyIn); err != nil {
		return err
	}
	// Sign before tainting: a valid path can still encode past the request
	// header bound, and that refusal must not forfeit the workspace.
	request := harnessv2.OperatorFileRequest{
		Protocol: harnessv2.ProtocolVersion, Metadata: target.metadata(operatoraccess.OperationCopyIn),
		AuditID: target.record.AuditID, Path: cleaned, Write: true, MaxBytes: max(size, 1), Size: size,
	}
	sealed, err := sealOperatorFileRequest(target, &request)
	if err != nil {
		return err
	}
	if err := s.taintOperatorTarget(c.Context(), target); err != nil {
		if _, refused := errors.AsType[*fiber.Error](err); refused {
			return err
		}
		log.Error(err, "operator copy refused: taint could not be persisted", "auditID", target.record.AuditID)
		return fiber.NewError(fiber.StatusServiceUnavailable, "operator copy refused: the workspace taint could not be persisted")
	}
	envelope.record = target.record
	var body io.Reader
	if c.Request().IsBodyStream() {
		body = c.Request().BodyStream()
	} else {
		body = strings.NewReader(string(c.Body()))
	}
	started := target.record
	started.Phase = OperatorAuditStarted
	envelope.recorded = true
	s.operatorAudit(c.Context(), started)
	response, err := s.sendOperatorFileRequest(c.Context(), target, &request, sealed, http.MethodPut, io.LimitReader(body, size))
	if err != nil {
		s.operatorAudit(c.Context(), operatorFinishRecord(target.record, operatorFailureOutcome(err), nil, 0))
		return err
	}
	defer response.Body.Close() //nolint:errcheck
	var result harnessv2.OperatorFileResult
	if json.NewDecoder(io.LimitReader(response.Body, 4<<10)).Decode(&result) != nil || result.Bytes != size {
		s.operatorAudit(c.Context(), operatorFinishRecord(target.record, "invalid supervisor response", nil, 0))
		return fiber.NewError(fiber.StatusBadGateway, "runtime supervisor returned an invalid upload result")
	}
	s.operatorAudit(c.Context(), operatorFinishRecord(target.record, "completed", nil, result.Bytes))
	return c.JSON(fiber.Map{"bytes": result.Bytes, "auditID": target.record.AuditID, "tainted": true})
}

func operatorFailureOutcome(err error) string {
	if fiberErr, ok := errors.AsType[*fiber.Error](err); ok {
		return fiberErr.Message
	}
	return operatorOutcomeFailed
}

func parseOperatorBytes(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, fiber.NewError(fiber.StatusBadRequest, "maxBytes must be a non-negative integer")
	}
	return value, nil
}

// sealedOperatorRequest is a signed operator request header and its
// single-use capability.
type sealedOperatorRequest struct {
	header     string
	capability string
}

func sealOperatorFileRequest(target *operatorTarget, request *harnessv2.OperatorFileRequest) (sealedOperatorRequest, error) {
	header, capability, err := harnessv2.SealOperatorRequest(target.secret, request, &request.Metadata)
	if err != nil {
		return sealedOperatorRequest{}, fiber.NewError(fiber.StatusBadRequest, "operator file request could not be signed: "+err.Error())
	}
	return sealedOperatorRequest{header: header, capability: capability}, nil
}

func (s *Server) sendOperatorFileRequest(
	ctx context.Context, target *operatorTarget, request *harnessv2.OperatorFileRequest, sealed sealedOperatorRequest,
	method string, body io.Reader,
) (*http.Response, error) {
	path, err := harnessv2.OperatorFilesPath(target.sessionID)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusConflict, "runtime session identity is invalid")
	}
	httpRequest, err := http.NewRequestWithContext(context.WithoutCancel(ctx), method, target.endpoint+path, body)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, "operator file request could not be built")
	}
	if body != nil {
		httpRequest.ContentLength = request.Size
		if request.Size == 0 {
			httpRequest.Body = http.NoBody
		}
	}
	httpRequest.Header.Set("Authorization", "Bearer "+target.bearer)
	httpRequest.Header.Set(harnessv2.OperationCapabilityHeader, sealed.capability)
	httpRequest.Header.Set(harnessv2.OperatorRequestHeader, sealed.header)
	response, err := s.operatorHTTPClient().Do(httpRequest)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusBadGateway, "runtime supervisor is unreachable")
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close() //nolint:errcheck
		status, message := supervisorErrorMessage(response)
		return nil, fiber.NewError(status, message)
	}
	return response, nil
}

type auditedOperatorBody struct {
	body     io.ReadCloser
	expected int64
	copied   int64
	once     sync.Once
	onClose  func(copied int64, complete bool)
}

func (b *auditedOperatorBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	b.copied += int64(n)
	return n, err
}

func (b *auditedOperatorBody) Close() error {
	err := b.body.Close()
	b.once.Do(func() { b.onClose(b.copied, b.copied == b.expected) })
	return err
}
