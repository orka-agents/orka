/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/adaptor"

	"github.com/orka-agents/orka/internal/ahphost"
	"github.com/orka-agents/orka/internal/store"
)

// CreateAHPTokenRequest asks for an AHP connection token.
type CreateAHPTokenRequest struct {
	// TTL is a Go duration such as "720h"; empty means the default lifetime.
	TTL         string `json:"ttl,omitempty"`
	Description string `json:"description,omitempty"`
}

// AHPTokenResponse describes an AHP connection token. Token is set only in
// the response that minted it.
type AHPTokenResponse struct {
	ID          string     `json:"id"`
	Token       string     `json:"token,omitempty"`
	Description string     `json:"description,omitempty"`
	Namespace   string     `json:"namespace,omitempty"`
	CreatedAt   *time.Time `json:"createdAt,omitempty"`
	ExpiresAt   time.Time  `json:"expiresAt"`
	RevokedAt   *time.Time `json:"revokedAt,omitempty"`
}

// ahpTokenIdentity admits only the identity that may own AHP tokens: a person
// signed in themselves with OIDC. Delegated context tokens are refused: AHP
// connections cannot yet keep to a delegated token's limits.
func (h *Handlers) ahpTokenIdentity(c fiber.Ctx) (*UserInfo, string, error) {
	if h.ahp == nil {
		return nil, "", fiber.NewError(fiber.StatusNotImplemented, "the AHP API is not enabled on this controller")
	}
	ui := GetUserInfo(c)
	if ui == nil {
		return nil, "", fiber.NewError(fiber.StatusUnauthorized, "missing authenticated identity")
	}
	if ui.AuthType != AuthTypeOIDC {
		return nil, "", fiber.NewError(fiber.StatusForbidden, "AHP tokens require the person's own OIDC sign-in; delegated context tokens are not accepted")
	}
	if strings.TrimSpace(ui.Subject) == "" || strings.TrimSpace(ui.Issuer) == "" {
		return nil, "", fiber.NewError(fiber.StatusForbidden, "AHP tokens require an identity with issuer and subject")
	}
	namespace, err := h.resolveNamespace(c, c.Query(toolNamespaceArg, ""))
	if err != nil {
		return nil, "", err
	}
	return ui, namespace, nil
}

// CreateAHPToken mints an AHP connection token for the caller.
func (h *Handlers) CreateAHPToken(c fiber.Ctx) error {
	ui, namespace, err := h.ahpTokenIdentity(c)
	if err != nil {
		return err
	}
	var req CreateAHPTokenRequest
	if err := decodeStrictJSON(c.Body(), &req); err != nil {
		return err
	}
	var ttl time.Duration
	if strings.TrimSpace(req.TTL) != "" {
		if ttl, err = time.ParseDuration(req.TTL); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "ttl must be a duration such as 720h")
		}
	}
	mint := ahphost.MintRequest{
		Issuer: ui.Issuer, Subject: ui.Subject, Username: ui.Username, Namespace: namespace,
		TTL: ttl, Description: req.Description,
	}
	minted, err := h.ahp.Tokens().Mint(c.Context(), mint)
	if errors.Is(err, ahphost.ErrInvalidMintRequest) {
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	}
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "failed to mint AHP token")
	}
	return c.Status(fiber.StatusCreated).JSON(AHPTokenResponse{
		ID: minted.ID, Token: minted.Token, Description: minted.Description, Namespace: namespace,
		ExpiresAt: minted.ExpiresAt,
	})
}

var errAHPTokenOwnerNotAdmitted = errors.New("the AHP token's owner can no longer sign in")

// AHPTokenOwnerCheck refuses the AHP tokens of owners the API would no longer
// sign in: a token needs the configured OIDC issuer, a subject the allowlist
// still admits, and the namespace a sign-in gets now. The configuration is the
// controller's current one, so narrowing the allowlist or moving the OIDC
// namespace cuts off tokens minted before.
func AHPTokenOwnerCheck(oidc OIDCConfig) func(ahphost.Principal) error {
	namespace := oidc.Namespace
	if namespace == "" {
		namespace = defaultNamespace
	}
	return func(p ahphost.Principal) error {
		if !oidc.Enabled() || p.Issuer != oidc.Issuer || p.Namespace != namespace {
			return errAHPTokenOwnerNotAdmitted
		}
		if err := authorizeOIDCClaims(oidcClaims{Issuer: p.Issuer, Subject: p.Subject}, oidc); err != nil {
			return errAHPTokenOwnerNotAdmitted
		}
		return nil
	}
}

// ListAHPTokens lists the caller's AHP tokens without their secrets.
func (h *Handlers) ListAHPTokens(c fiber.Ctx) error {
	ui, _, err := h.ahpTokenIdentity(c)
	if err != nil {
		return err
	}
	tokens, err := h.ahp.Tokens().List(c.Context(), ui.Issuer, ui.Subject)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "failed to list AHP tokens")
	}
	items := make([]AHPTokenResponse, 0, len(tokens))
	for _, token := range tokens {
		created := token.CreatedAt
		items = append(items, AHPTokenResponse{
			ID: token.ID, Description: token.Description, Namespace: token.Namespace,
			CreatedAt: &created, ExpiresAt: token.ExpiresAt, RevokedAt: token.RevokedAt,
		})
	}
	return c.JSON(fiber.Map{apiFieldItems: items})
}

// RevokeAHPToken revokes one of the caller's AHP tokens and closes every
// connection that uses it. Another person's token is reported as missing.
func (h *Handlers) RevokeAHPToken(c fiber.Ctx) error {
	ui, _, err := h.ahpTokenIdentity(c)
	if err != nil {
		return err
	}
	err = h.ahp.Tokens().Revoke(c.Context(), ui.Issuer, ui.Subject, c.Params("id"))
	if errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusNotFound, "AHP token not found")
	}
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "failed to revoke AHP token")
	}
	return c.SendStatus(fiber.StatusNoContent)
}

const ahpEndpointPath = "/ahp"

// ahpEndpoint serves /ahp through the host's net/http handler. The request
// URI can carry the connection token, so a URI the adaptor cannot parse is
// refused here rather than reaching the adaptor, which would log it.
func ahpEndpoint(host *ahphost.Host) fiber.Handler {
	serve := adaptor.HTTPHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host.ServeHTTP(&switchingProtocolsWriter{ResponseWriter: w}, r)
	}))
	return func(c fiber.Ctx) error {
		if _, err := url.ParseRequestURI(string(c.Request().RequestURI())); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid request URI")
		}
		return serve(c)
	}
}

// switchingProtocolsWriter sends the 101 response a WebSocket handshake wrote
// when the handler hijacks the connection. The fasthttp adaptor buffers the
// status and headers and drops them on hijack, so without this the client
// would wait for a handshake response that never comes.
type switchingProtocolsWriter struct {
	http.ResponseWriter
	status int
}

func (w *switchingProtocolsWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *switchingProtocolsWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil || w.status != http.StatusSwitchingProtocols {
		return conn, rw, err
	}
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	_ = w.Header().Write(rw)
	_, _ = rw.WriteString("\r\n")
	if err := rw.Flush(); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return conn, rw, nil
}
