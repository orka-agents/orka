package api

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/orka-agents/orka/internal/artifactcap"
)

const (
	defaultAPIRequestBodyLimit = 15 << 20
	defaultACPArtifactMaxBytes = 512 << 20
	defaultACPArtifactRoot     = "/data/acp-artifacts"

	envACPArtifactRoot       = "ORKA_ACP_ARTIFACT_ROOT"
	envACPArtifactSecretFile = "ORKA_ACP_ARTIFACT_CAPABILITY_SECRET_FILE"
	envACPArtifactMaxBytes   = "ORKA_ACP_ARTIFACT_MAX_BYTES"

	acpArtifactRoutePrefix = "/internal/v2/acp/artifacts"
)

type ACPArtifactHandlers struct {
	service *artifactcap.Service
}

func NewACPArtifactHandlers(service *artifactcap.Service) (*ACPArtifactHandlers, error) {
	if service == nil {
		return nil, fmt.Errorf("artifact service is required")
	}
	return &ACPArtifactHandlers{service: service}, nil
}

func (h *ACPArtifactHandlers) Upload(c fiber.Ctx) error {
	presented, token, err := artifactPresentedRequest(c, artifactcap.OperationUpload)
	if err != nil {
		return writeACPArtifactError(c, err)
	}
	actualLength := int64(c.Request().Header.ContentLength())
	if actualLength < 0 {
		return writeACPArtifactError(c, fmt.Errorf("%w: content-length is required", artifactcap.ErrInvalidRequest))
	}
	if actualLength != presented.ContentLength {
		return writeACPArtifactError(c, fmt.Errorf("%w: content length does not match request binding", artifactcap.ErrInvalidRequest))
	}
	if contentType := string(c.Request().Header.ContentType()); contentType != presented.MediaType {
		return writeACPArtifactError(c, fmt.Errorf("%w: content type does not match request binding", artifactcap.ErrInvalidRequest))
	}
	artifact, err := h.service.Upload(c.Context(), token, presented, c.Request().BodyStream())
	if err != nil {
		return writeACPArtifactError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(artifact)
}

func (h *ACPArtifactHandlers) Download(c fiber.Ctx) error {
	presented, token, err := artifactPresentedRequest(c, artifactcap.OperationDownload)
	if err != nil {
		return writeACPArtifactError(c, err)
	}
	download, err := h.service.OpenDownload(c.Context(), token, presented)
	if err != nil {
		return writeACPArtifactError(c, err)
	}
	c.Set(fiber.HeaderContentType, download.Artifact.MediaType)
	c.Set(fiber.HeaderContentLength, strconv.FormatInt(download.Artifact.SizeBytes, 10))
	c.Set(artifactcap.ObjectDigestHeader, download.Artifact.Digest)
	c.Set(fiber.HeaderETag, `"`+download.Artifact.Digest+`"`)
	c.Status(fiber.StatusOK)
	return c.SendStream(download, int(download.Artifact.SizeBytes))
}

func artifactPresentedRequest(c fiber.Ctx, operation artifactcap.Operation) (artifactcap.PresentedRequest, string, error) {
	hexDigest := c.Params("digest")
	if len(hexDigest) != 64 || strings.ToLower(hexDigest) != hexDigest {
		return artifactcap.PresentedRequest{}, "", artifactcap.ErrInvalidRequest
	}
	objectDigest := "sha256:" + hexDigest
	canonicalPath, err := artifactcap.ObjectPath(objectDigest)
	if err != nil || c.Path() != canonicalPath {
		return artifactcap.PresentedRequest{}, "", artifactcap.ErrInvalidRequest
	}
	lengthValue := string(c.Request().Header.Peek(artifactcap.ContentLengthHeader))
	contentLength, err := strconv.ParseInt(lengthValue, 10, 64)
	if err != nil || strconv.FormatInt(contentLength, 10) != lengthValue || contentLength < 0 {
		return artifactcap.PresentedRequest{}, "", artifactcap.ErrInvalidRequest
	}
	mediaType := string(c.Request().Header.Peek(artifactcap.MediaTypeHeader))
	presented := artifactcap.PresentedRequest{
		Method:        operation.Method(),
		Path:          canonicalPath,
		ObjectDigest:  objectDigest,
		ContentLength: contentLength,
		MediaType:     mediaType,
		RequestDigest: string(c.Request().Header.Peek(artifactcap.RequestDigestHeader)),
	}
	if err := presented.Validate(); err != nil {
		return artifactcap.PresentedRequest{}, "", err
	}
	token := string(c.Request().Header.Peek(artifactcap.CapabilityHeader))
	if token == "" {
		return artifactcap.PresentedRequest{}, "", artifactcap.ErrUnauthorized
	}
	return presented, token, nil
}

func writeACPArtifactError(c fiber.Ctx, err error) error {
	status := fiber.StatusInternalServerError
	code := "artifact_io_failure"
	switch {
	case errors.Is(err, artifactcap.ErrExpired), errors.Is(err, artifactcap.ErrUnauthorized):
		status, code = fiber.StatusForbidden, "artifact_authorization_failed"
	case errors.Is(err, artifactcap.ErrReplay):
		status, code = fiber.StatusConflict, "artifact_operation_replayed"
	case errors.Is(err, artifactcap.ErrOperationConflict):
		status, code = fiber.StatusConflict, "artifact_operation_conflict"
	case errors.Is(err, artifactcap.ErrTooLarge):
		status, code = fiber.StatusRequestEntityTooLarge, "artifact_too_large"
	case errors.Is(err, artifactcap.ErrNotFound):
		status, code = fiber.StatusNotFound, "artifact_not_found"
	case errors.Is(err, artifactcap.ErrPartialUpload):
		status, code = fiber.StatusBadRequest, "artifact_upload_incomplete"
	case errors.Is(err, artifactcap.ErrDigestMismatch):
		status, code = fiber.StatusUnprocessableEntity, "artifact_digest_mismatch"
	case errors.Is(err, artifactcap.ErrInvalidRequest):
		status, code = fiber.StatusBadRequest, "artifact_request_invalid"
	case errors.Is(err, artifactcap.ErrUnsafePath), errors.Is(err, artifactcap.ErrCorrupt):
		status, code = fiber.StatusInternalServerError, "artifact_storage_unavailable"
	}
	return c.Status(status).JSON(fiber.Map{"error": code})
}

func (s *Server) installACPArtifactTransport() {
	s.app.Use(acpArtifactStreamingGuard)
	handlers, err := loadACPArtifactHandlersFromEnvironment()
	group := s.app.Group(acpArtifactRoutePrefix)
	if err != nil {
		unavailable := func(c fiber.Ctx) error {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": "artifact_transport_unavailable"})
		}
		group.Put("/sha256/:digest", unavailable)
		group.Get("/sha256/:digest", unavailable)
		return
	}
	group.Put("/sha256/:digest", handlers.Upload)
	group.Get("/sha256/:digest", handlers.Download)
}

func loadACPArtifactHandlersFromEnvironment() (*ACPArtifactHandlers, error) {
	secretFile := strings.TrimSpace(os.Getenv(envACPArtifactSecretFile))
	if secretFile == "" || !filepath.IsAbs(secretFile) {
		return nil, fmt.Errorf("artifact capability secret file is not configured")
	}
	secret, err := readACPArtifactCapabilitySecretFile(secretFile)
	if err != nil {
		return nil, err
	}
	root := strings.TrimSpace(os.Getenv(envACPArtifactRoot))
	if root == "" {
		root = defaultACPArtifactRoot
	}
	maxBytes := int64(defaultACPArtifactMaxBytes)
	if raw := strings.TrimSpace(os.Getenv(envACPArtifactMaxBytes)); raw != "" {
		parsed, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || parsed <= 0 {
			return nil, fmt.Errorf("artifact maximum size is invalid")
		}
		maxBytes = parsed
	}
	service, err := artifactcap.NewService(artifactcap.ServiceConfig{Root: root, Secret: secret, MaxObjectBytes: maxBytes})
	if err != nil {
		return nil, err
	}
	return NewACPArtifactHandlers(service)
}

func acpArtifactStreamingGuard(c fiber.Ctx) error {
	if strings.HasPrefix(c.Path(), acpArtifactRoutePrefix+"/") {
		return c.Next()
	}
	// Gateway adapter ingress legitimately arrives chunked (or as HTTP/2
	// without a declared length); it carries its own per-route body-size
	// configuration and reads a bounded body stream, so the Content-Length
	// requirement must not reject it.
	if isGatewayIngressPath(c.Path()) {
		return c.Next()
	}
	method := c.Method()
	if method != fiber.MethodPost && method != fiber.MethodPut && method != fiber.MethodPatch {
		return c.Next()
	}
	contentLength := c.Request().Header.ContentLength()
	if contentLength == -1 {
		return c.Status(fiber.StatusLengthRequired).JSON(fiber.Map{"error": "content_length_required"})
	}
	if contentLength > defaultAPIRequestBodyLimit {
		return c.Status(fiber.StatusRequestEntityTooLarge).JSON(fiber.Map{"error": "request_body_too_large"})
	}
	return c.Next()
}
