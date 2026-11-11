package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
	kubevalidation "k8s.io/apimachinery/pkg/util/validation"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/remediation/contract"
)

const (
	maxRemediationReportBytes  = 64 << 10
	maxRemediationPatchBytes   = 256 << 10
	maxRemediationRequestBytes = 2 << 20
	remediationResource        = "remediations"
	remediationCreateVerb      = "create"
	remediationRepositoryField = "repository"
	remediationRefField        = "ref"
)

var remediationReasonCode = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)

func isRemediationPath(path string) bool {
	path = strings.ToLower(path)
	return strings.TrimRight(path, "/") == "/api/v1/remediations" || strings.HasPrefix(path, "/api/v1/remediations/")
}

func setRemediationPrivate(c fiber.Ctx) {
	c.Set(fiber.HeaderCacheControl, "private, no-store")
	c.Set(fiber.HeaderPragma, "no-cache")
	c.Set("X-Content-Type-Options", "nosniff")
}

func remediationPrivateResponse(c fiber.Ctx) error {
	setRemediationPrivate(c)
	return c.Next()
}

func remediationExplicitNamespace(c fiber.Ctx) (string, error) {
	query := c.Request().URI().QueryArgs()
	unknown := false
	for key := range query.All() {
		if string(key) != "namespace" {
			unknown = true
			break
		}
	}
	values := query.PeekMulti("namespace")
	if unknown || len(values) > 1 || (len(values) == 1 && len(values[0]) == 0) {
		return "", fiber.NewError(fiber.StatusForbidden, "namespace not allowed")
	}
	explicit := c.Query("namespace")
	if explicit != "" && len(kubevalidation.IsDNS1123Label(explicit)) != 0 {
		return "", fiber.NewError(fiber.StatusForbidden, "namespace not allowed")
	}
	return explicit, nil
}

func (h *Handlers) remediationRequest(c fiber.Ctx, verb, resource string) (string, string, error) {
	setRemediationPrivate(c)
	if h.remediationService == nil ||
		(reflect.ValueOf(h.remediationService).Kind() == reflect.Pointer && reflect.ValueOf(h.remediationService).IsNil()) {
		return "", "", remediationHTTPError(contract.ErrDisabled)
	}
	user := GetUserInfo(c)
	if user == nil {
		return "", "", fiber.NewError(fiber.StatusUnauthorized, "authentication required")
	}
	if user.AuthType != AuthTypeTokenReview || strings.TrimSpace(user.Username) == "" {
		return "", "", remediationHTTPError(contract.ErrForbidden)
	}
	explicit, err := remediationExplicitNamespace(c)
	if err != nil {
		return "", "", err
	}
	namespace, err := h.resolveNamespace(c, explicit)
	if err != nil || len(kubevalidation.IsDNS1123Label(namespace)) != 0 {
		return "", "", fiber.NewError(fiber.StatusForbidden, "namespace not allowed")
	}
	id := c.Params("id")
	if verb != remediationCreateVerb && !validRemediationRunID(id) {
		return "", "", remediationHTTPError(contract.ErrInvalid)
	}
	if err := authorizeKubernetesResourceAction(c.Context(), h.clientset, user,
		namespace, verb, corev1alpha1.GroupVersion.Group, resource, id); err != nil {
		return "", "", err
	}
	return namespace, user.Username, nil
}

func (h *Handlers) CreateRemediation(c fiber.Ctx) error {
	namespace, subject, err := h.remediationRequest(c, remediationCreateVerb, remediationResource)
	if err != nil {
		return err
	}
	contentType, _, err := mime.ParseMediaType(c.Get(fiber.HeaderContentType))
	if err != nil || contentType != fiber.MIMEApplicationJSON {
		return fiber.NewError(fiber.StatusUnsupportedMediaType, "JSON content type is required")
	}
	body, err := readRemediationBody(c)
	if err != nil {
		return err
	}
	request, err := decodeRemediationRequest(body)
	if err != nil {
		return remediationHTTPError(err)
	}
	status, err := h.remediationService.Submit(c.Context(), namespace, subject, request)
	if err != nil {
		return remediationHTTPError(err)
	}
	if request.RequiredEvidenceScope != "" && (status == nil ||
		contract.RequireEvidenceScope(request.RequiredEvidenceScope, status.EvidenceScope) != nil) {
		return remediationHTTPError(contract.ErrUnavailable)
	}
	return sendRemediationStatus(c, status, namespace, request.RequestID, fiber.StatusAccepted)
}

func (h *Handlers) GetRemediation(c fiber.Ctx) error {
	namespace, _, err := h.remediationRequest(c, "get", remediationResource)
	if err != nil {
		return err
	}
	status, err := h.remediationService.Get(c.Context(), namespace, c.Params("id"))
	if err != nil {
		return remediationHTTPError(err)
	}
	return sendRemediationStatus(c, status, namespace, c.Params("id"), fiber.StatusOK)
}

func (h *Handlers) CancelRemediation(c fiber.Ctx) error {
	namespace, _, err := h.remediationRequest(c, "update", remediationResource+"/cancel")
	if err != nil {
		return err
	}
	if c.Request().Header.ContentLength() != 0 || len(c.BodyRaw()) != 0 {
		return remediationHTTPError(contract.ErrInvalid)
	}
	status, err := h.remediationService.Cancel(c.Context(), namespace, c.Params("id"))
	if err != nil {
		return remediationHTTPError(err)
	}
	return sendRemediationStatus(c, status, namespace, c.Params("id"), fiber.StatusAccepted)
}

func (h *Handlers) GetRemediationPatch(c fiber.Ctx) error {
	namespace, _, err := h.remediationRequest(c, "get", remediationResource+"/patch")
	if err != nil {
		return err
	}
	content, err := h.remediationService.Patch(c.Context(), namespace, c.Params("id"))
	if err != nil {
		return remediationHTTPError(err)
	}
	if len(content) == 0 || len(content) > maxRemediationPatchBytes || !utf8.Valid(content) {
		return remediationHTTPError(contract.ErrUnavailable)
	}
	c.Set(fiber.HeaderContentType, fiber.MIMEOctetStream)
	c.Set(fiber.HeaderContentDisposition, `attachment; filename="candidate.patch"`)
	return c.Send(content)
}

func readRemediationBody(c fiber.Ctx) ([]byte, error) {
	tooLarge := fiber.NewError(fiber.StatusRequestEntityTooLarge, "remediation request exceeds its size limit")
	if c.Request().Header.ContentLength() > maxRemediationRequestBytes {
		c.RequestCtx().SetConnectionClose()
		return nil, tooLarge
	}
	var body []byte
	if stream := c.Request().BodyStream(); stream != nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(stream, maxRemediationRequestBytes+1))
		if err != nil {
			c.RequestCtx().SetConnectionClose()
			return nil, remediationHTTPError(contract.ErrInvalid)
		}
	} else {
		body = c.BodyRaw()
	}
	if len(body) > maxRemediationRequestBytes {
		c.RequestCtx().SetConnectionClose()
		return nil, tooLarge
	}
	return body, nil
}

func decodeRemediationRequest(body []byte) (contract.Request, error) {
	var request contract.Request
	fields, err := remediationObject(body)
	if err != nil {
		return request, contract.ErrInvalid
	}
	for name := range fields {
		switch name {
		case "requestID", "mode", "report", remediationRepositoryField, remediationRefField, "requiredEvidenceScope":
		default:
			return request, contract.ErrInvalid
		}
	}
	if json.Unmarshal(body, &request) != nil || !validRemediationRunID(request.RequestID) ||
		len(request.Report) == 0 || len(request.Report) > maxRemediationReportBytes {
		return request, contract.ErrInvalid
	}
	switch request.Mode {
	case contract.Generate, contract.Validate:
	default:
		return request, contract.ErrInvalid
	}
	if !validRemediationTarget(request.Repository, request.Ref) || !validRemediationReport(request) {
		return request, contract.ErrInvalid
	}
	return request, nil
}

func validRemediationTarget(repository, ref string) bool {
	target, err := url.Parse(repository)
	if err != nil || len(repository) > 2048 || target.Scheme != connectorSchemeHTTPS || target.Hostname() == "" ||
		target.User != nil || target.RawQuery != "" || target.Fragment != "" {
		return false
	}
	return ref != "" && len(ref) <= 256 && !strings.ContainsRune(ref, utf8.RuneError) &&
		strings.IndexFunc(ref, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) == -1
}

func validRemediationReport(request contract.Request) bool {
	fields, err := remediationObject(request.Report)
	if err != nil {
		return false
	}
	text := func(name string) string {
		var value string
		_ = json.Unmarshal(fields[name], &value)
		return value
	}
	if strings.TrimSpace(text("title")) == "" ||
		(strings.TrimSpace(text("problem")) == "" && strings.TrimSpace(text("description")) == "") {
		return false
	}
	for name, expected := range map[string]string{remediationRepositoryField: request.Repository, remediationRefField: request.Ref} {
		if _, ok := fields[name]; ok && text(name) != expected {
			return false
		}
	}
	return true
}

func remediationObject(raw []byte) (map[string]json.RawMessage, error) {
	if !utf8.Valid(raw) {
		return nil, contract.ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil, contract.ErrInvalid
	}
	fields := map[string]json.RawMessage{}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		name, ok := token.(string)
		key := strings.ToLower(name)
		if err != nil || !ok || seen[key] {
			return nil, contract.ErrInvalid
		}
		seen[key] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, contract.ErrInvalid
		}
		fields[name] = value
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return nil, contract.ErrInvalid
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, contract.ErrInvalid
	}
	return fields, nil
}

func validRemediationRunID(id string) bool {
	return len(id) <= 63 && len(kubevalidation.IsDNS1123Subdomain(id)) == 0
}

func sendRemediationStatus(c fiber.Ctx, status *contract.Status, namespace, id string, code int) error {
	if status == nil || status.ID != id || status.Namespace != namespace || !validRemediationStatus(status) {
		return remediationHTTPError(contract.ErrUnavailable)
	}
	return c.Status(code).JSON(status)
}

func validRemediationStatus(status *contract.Status) bool {
	if status.EvidenceScope != "" && !status.EvidenceScope.Valid() {
		return false
	}
	if (status.TaskName != "" || status.TaskUID != "") && !validRemediationTaskReference(status.TaskName, status.TaskUID) {
		return false
	}
	if status.Phase != contract.PhasePending && status.Phase != contract.PhaseRunning && !contract.TerminalPhase(status.Phase) {
		return false
	}
	switch status.Stage {
	case contract.StageReproduce, contract.StageGenerate, contract.StageVerify, contract.StageComplete:
	default:
		return false
	}
	switch status.Outcome {
	case "", contract.OutcomeReproduced, contract.OutcomeNotReproduced, contract.OutcomeUnableToValidate,
		contract.OutcomeUnableToVerify, contract.OutcomeUnverified, contract.OutcomeCancelled, contract.OutcomeTimedOut:
	case contract.OutcomeVerified:
		if status.Phase != contract.PhaseSucceeded {
			return false
		}
	default:
		return false
	}
	return status.Reason == "" || remediationReasonCode.MatchString(status.Reason)
}

func validRemediationTaskReference(name, uid string) bool {
	if len(kubevalidation.IsDNS1123Subdomain(name)) != 0 || uid == "" || len(uid) > 128 {
		return false
	}
	for _, character := range []byte(uid) {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '-' && character != '_' && character != '.' {
			return false
		}
	}
	return true
}

func remediationHTTPError(err error) error {
	for _, mapping := range []struct {
		domain error
		status int
	}{
		{contract.ErrDisabled, fiber.StatusNotFound},
		{contract.ErrInvalid, fiber.StatusBadRequest},
		{contract.ErrEvidenceScopeUnsupported, fiber.StatusUnprocessableEntity},
		{contract.ErrNotFound, fiber.StatusNotFound},
		{contract.ErrConflict, fiber.StatusConflict},
		{contract.ErrUnavailable, fiber.StatusServiceUnavailable},
		{contract.ErrForbidden, fiber.StatusForbidden},
	} {
		if errors.Is(err, mapping.domain) {
			return fiber.NewError(mapping.status, mapping.domain.Error())
		}
	}
	return fiber.NewError(fiber.StatusInternalServerError, "remediation operation failed")
}
