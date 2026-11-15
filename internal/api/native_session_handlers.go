package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/orka-agents/orka/internal/codexstate"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	"k8s.io/apimachinery/pkg/util/validation"
)

const maxNativeSessionBundleBytes = harnessv2.DefaultMaxNativeSessionBytes

// isNativeSessionImportPath identifies only the bounded native import route.
func isNativeSessionImportPath(path string) bool {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	return len(parts) == 5 && strings.EqualFold(parts[0], "api") && strings.EqualFold(parts[1], "v1") &&
		strings.EqualFold(parts[2], "sessions") && parts[3] != "" && strings.EqualFold(parts[4], "native")
}

type nativeSessionImportRequest struct {
	OperationID string `json:"operationID"`
	Data        []byte `json:"data"`
}

type nativeSessionExportResponse struct {
	Data              []byte `json:"data"`
	DataDigest        string `json:"dataDigest"`
	ProviderSessionID string `json:"providerSessionID"`
}

// ImportNativeSession explicitly creates a fresh Session. Imported provider
// state remains private and staged until its first authenticated Task admission.
func (h *Handlers) ImportNativeSession(c fiber.Ctx) error {
	namespace, err := h.resolveNamespace(c, c.Query(toolNamespaceArg, ""))
	if err != nil {
		return err
	}
	name := c.Params("id")
	if err := h.authorizeContextTokenAction(c, "importNativeSession", h.contextTokenAuthorization.SessionWriteScopes); err != nil {
		return err
	}
	if err := h.authorizeCoreResourceAction(c, "create", "sessions", namespace, ""); err != nil {
		return err
	}
	if err := h.authorizeCoreResourceAction(c, "get", "sessions", namespace, name); err != nil {
		return err
	}
	if h.nativeSessionConfigErr != nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "native session size policy is invalid")
	}
	if h.nativeSessionStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "native session migration is unavailable")
	}
	if len(validation.IsDNS1123Subdomain(name)) != 0 {
		return fiber.NewError(fiber.StatusBadRequest, "session name must be a DNS subdomain")
	}
	maxRequestBytes := harnessv2.NativeSessionJSONLimit(h.nativeSessionMaxBytes)
	if c.Request().Header.ContentLength() > maxRequestBytes {
		c.RequestCtx().SetConnectionClose()
		return fiber.NewError(fiber.StatusRequestEntityTooLarge, "native session request is too large")
	}
	var body []byte
	if stream := c.Request().BodyStream(); stream != nil {
		body, err = io.ReadAll(io.LimitReader(stream, int64(maxRequestBytes)+1))
		if err != nil {
			c.RequestCtx().SetConnectionClose()
			return fiber.NewError(fiber.StatusBadRequest, "invalid native session request")
		}
	} else {
		body = c.BodyRaw()
	}
	if len(body) > maxRequestBytes {
		c.RequestCtx().SetConnectionClose()
		return fiber.NewError(fiber.StatusRequestEntityTooLarge, "native session request is too large")
	}
	var request nativeSessionImportRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid native session request")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return fiber.NewError(fiber.StatusBadRequest, "native session request must contain one JSON object")
	}
	if err := store.ValidateControlIdentifier("native operation ID", request.OperationID); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "valid operationID is required")
	}
	if len(request.Data) == 0 || len(request.Data) > h.nativeSessionMaxBytes {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("native session bundle must contain 1 through %d bytes", h.nativeSessionMaxBytes))
	}
	summary, err := codexstate.Inspect(c.Context(), request.Data, h.nativeSessionMaxBytes)
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid native session bundle")
	}
	receipt, err := h.nativeSessionStore.StageNativeSessionImport(c.Context(), store.NativeSessionImport{
		Namespace: namespace, SessionName: name, OperationID: request.OperationID,
		RequestDigest: store.NativeSessionImportDigest(namespace, name, summary.DataDigest),
		Snapshot: harnessv2.NativeSessionSnapshot{
			Data: request.Data, DataDigest: summary.DataDigest, ProviderSessionID: summary.ThreadID,
			ProviderKind: "codex", ProviderVersion: summary.Manifest.SourceCLIVersion,
		},
	})
	if err != nil {
		return nativeSessionAPIError(err)
	}
	return c.Status(fiber.StatusCreated).JSON(receipt)
}

// ExportNativeSession exposes the portable bundle only through an explicitly
// authorized Session migration endpoint, never regular Session/event payloads.
func (h *Handlers) ExportNativeSession(c fiber.Ctx) error {
	namespace, err := h.resolveNamespace(c, c.Query(toolNamespaceArg, ""))
	if err != nil {
		return err
	}
	name := c.Params("id")
	if err := h.authorizeContextTokenAction(c, "exportNativeSession", h.contextTokenAuthorization.SessionReadScopes); err != nil {
		return err
	}
	if err := h.authorizeCoreResourceAction(c, "get", "sessions", namespace, name); err != nil {
		return err
	}
	if h.nativeSessionConfigErr != nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "native session size policy is invalid")
	}
	if h.nativeSessionStore == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "native session migration is unavailable")
	}
	identityStore, ok := h.sessionStore.(interface {
		GetSessionCleanupIdentity(context.Context, string, string) (string, error)
	})
	if !ok {
		return fiber.NewError(fiber.StatusNotImplemented, "native session ownership is unavailable")
	}
	uid, err := identityStore.GetSessionCleanupIdentity(c.Context(), namespace, name)
	if err != nil {
		return nativeSessionAPIError(err)
	}
	record, err := h.nativeSessionStore.GetNativeSession(c.Context(), namespace, name, uid)
	if err != nil {
		return nativeSessionAPIError(err)
	}
	if len(record.Snapshot.Data) > h.nativeSessionMaxBytes {
		return fiber.NewError(fiber.StatusRequestEntityTooLarge, fmt.Sprintf("native session bundle exceeds configured export limit of %d bytes", h.nativeSessionMaxBytes))
	}
	return c.JSON(nativeSessionExportResponse{
		Data: record.Snapshot.Data, DataDigest: record.Snapshot.DataDigest, ProviderSessionID: record.Snapshot.ProviderSessionID,
	})
}

func nativeSessionAPIError(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrGatewayOwnedSession):
		return fiber.NewError(fiber.StatusNotFound, "native session not found")
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrDuplicateMismatch):
		return fiber.NewError(fiber.StatusConflict, "native session operation conflicts with existing ownership or history")
	case errors.Is(err, store.ErrValidation):
		return fiber.NewError(fiber.StatusBadRequest, "invalid native session state")
	default:
		return fiber.NewError(fiber.StatusInternalServerError, "native session persistence failed")
	}
}
