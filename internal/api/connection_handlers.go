/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/controller"
	"github.com/orka-agents/orka/internal/store"
)

const (
	// ConnectionSubjectDigestLabel indexes Connections by owner without
	// exposing the raw subject as a label value.
	ConnectionSubjectDigestLabel = "orka.ai/connection-subject"
	// ConnectionProviderLabel indexes Connections by provider.
	ConnectionProviderLabel = "orka.ai/connector-provider"

	connectorSettingsPath        = "/settings/connectors"
	connectorSchemeHTTPS         = "https"
	connectorSchemeHTTP          = "http"
	connectionSubjectLabelLength = 32
	maxConnectionRequestBytes    = 4 << 10
)

// ConnectorConfig wires the consent flow and custody into the API server.
type ConnectorConfig struct {
	Enabled bool
	// CallbackBaseURL is the operator-facing origin the provider redirects
	// back to; the redirect URI is CallbackBaseURL + connectors.CallbackPath.
	CallbackBaseURL string
	// StateKey signs OAuth state values.
	StateKey    []byte
	Credentials store.ConnectorCredentialStore
	Consents    store.ConnectorConsentStore
	OAuth       *connectors.OAuthClient
	Now         func() time.Time
}

func (c ConnectorConfig) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c ConnectorConfig) redirectURI() string {
	return strings.TrimRight(c.CallbackBaseURL, "/") + connectors.CallbackPath
}

// ValidateConnectorConfig checks the operator settings before the server
// starts. Disabled connectors need nothing.
func ValidateConnectorConfig(cfg ConnectorConfig) error {
	if !cfg.Enabled {
		return nil
	}
	base := strings.TrimSpace(cfg.CallbackBaseURL)
	parsed, err := url.Parse(base)
	if base == "" || err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("connector callback base URL must be an absolute http(s) URL without userinfo, query, or fragment")
	}
	switch parsed.Scheme {
	case connectorSchemeHTTPS:
	case connectorSchemeHTTP:
		host := parsed.Hostname()
		if host != "localhost" && host != "127.0.0.1" && host != "::1" {
			return errors.New("connector callback base URL must use https unless it targets localhost")
		}
	default:
		return errors.New("connector callback base URL must use http or https")
	}
	if len(cfg.StateKey) < connectors.MinStateKeyBytes {
		return errors.New("connector state key is missing or too short")
	}
	if cfg.Credentials == nil || cfg.Consents == nil || cfg.OAuth == nil {
		return errors.New("connector custody, consent store, and OAuth client are required")
	}
	return nil
}

// ConnectorProviderResponse is the public catalog view of a provider.
type ConnectorProviderResponse struct {
	Name        string                       `json:"name"`
	Namespace   string                       `json:"namespace"`
	DisplayName string                       `json:"displayName"`
	Ready       bool                         `json:"ready"`
	Scopes      corev1alpha1.ConnectorScopes `json:"scopes"`
	Tools       []ConnectorToolResponse      `json:"tools"`
}

// ConnectorToolResponse names one tool and its class.
type ConnectorToolResponse struct {
	Name  string `json:"name"`
	Class string `json:"class"`
}

// ConnectionResponse is the public view of a Connection. It never carries
// token material.
type ConnectionResponse struct {
	Name            string       `json:"name"`
	Namespace       string       `json:"namespace"`
	Provider        string       `json:"provider"`
	Mode            string       `json:"mode"`
	State           string       `json:"state"`
	GrantedScopes   []string     `json:"grantedScopes,omitempty"`
	LinkedAt        *metav1.Time `json:"linkedAt,omitempty"`
	ExpiresAt       *metav1.Time `json:"expiresAt,omitempty"`
	LastRefreshTime *metav1.Time `json:"lastRefreshTime,omitempty"`
	Ready           bool         `json:"ready"`
	Message         string       `json:"message,omitempty"`
}

// ConnectionAuthorizeResponse returns the consent URL alongside the Connection.
type ConnectionAuthorizeResponse struct {
	Connection   ConnectionResponse `json:"connection"`
	AuthorizeURL string             `json:"authorizeURL"`
}

type createConnectionRequest struct {
	Provider string `json:"provider"`
	Mode     string `json:"mode"`
}

type updateConnectionRequest struct {
	Mode string `json:"mode"`
}

type completeConnectionRequest struct {
	Completion string `json:"completion"`
}

func connectorProviderResponse(provider *corev1alpha1.ConnectorProvider) ConnectorProviderResponse {
	tools := make([]ConnectorToolResponse, 0, len(provider.Spec.Tools))
	for _, tool := range provider.Spec.Tools {
		tools = append(tools, ConnectorToolResponse{Name: tool.Name, Class: string(tool.Class)})
	}
	displayName := provider.Spec.DisplayName
	if displayName == "" {
		displayName = provider.Name
	}
	return ConnectorProviderResponse{
		Name:        provider.Name,
		Namespace:   provider.Namespace,
		DisplayName: displayName,
		Ready:       connectors.ProviderAccepted(provider),
		Scopes:      provider.Spec.OAuth.Scopes,
		Tools:       tools,
	}
}

func connectionResponse(connection *corev1alpha1.Connection) ConnectionResponse {
	mode := connection.Spec.Mode
	if mode == "" {
		mode = corev1alpha1.ConnectionModeReadOnly
	}
	state := connection.Status.State
	if state == "" {
		state = corev1alpha1.ConnectionStatePending
	}
	response := ConnectionResponse{
		Name:            connection.Name,
		Namespace:       connection.Namespace,
		Provider:        connection.Spec.ProviderRef.Name,
		Mode:            mode,
		State:           state,
		GrantedScopes:   append([]string(nil), connection.Status.GrantedScopes...),
		LinkedAt:        connection.Status.LinkedAt,
		ExpiresAt:       connection.Status.ExpiresAt,
		LastRefreshTime: connection.Status.LastRefreshTime,
	}
	if ready := meta.FindStatusCondition(connection.Status.Conditions, corev1alpha1.ConnectionConditionReady); ready != nil {
		response.Ready = ready.Status == metav1.ConditionTrue
		response.Message = ready.Message
	}
	if resolved := meta.FindStatusCondition(connection.Status.Conditions, corev1alpha1.ConnectionConditionProviderResolved); resolved != nil && resolved.Status != metav1.ConditionTrue {
		response.Ready = false
		response.Message = resolved.Message
	}
	return response
}

// connectorIdentity returns the verified human identity behind a request.
// ServiceAccount and other TokenReview callers fail closed: a shared
// ServiceAccount must never be able to link or use a person's accounts.
func (h *Handlers) connectorIdentity(c fiber.Ctx) (*UserInfo, error) {
	if !h.connectors.Enabled {
		return nil, fiber.NewError(fiber.StatusNotImplemented, "connectors are not enabled on this controller")
	}
	ui := GetUserInfo(c)
	if ui == nil {
		return nil, fiber.NewError(fiber.StatusUnauthorized, "missing authenticated identity")
	}
	if ui.AuthType != AuthTypeOIDC && ui.AuthType != AuthTypeContextToken {
		return nil, fiber.NewError(fiber.StatusForbidden, "connectors require a verified OIDC or context-token identity")
	}
	if strings.TrimSpace(ui.Subject) == "" || strings.TrimSpace(ui.Issuer) == "" {
		return nil, fiber.NewError(fiber.StatusForbidden, "connectors require an identity with issuer and subject")
	}
	return ui, nil
}

func decodeStrictJSON(body []byte, target any) error {
	if len(body) > maxConnectionRequestBytes {
		return fiber.NewError(fiber.StatusRequestEntityTooLarge, "request body is too large")
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body: only documented fields are accepted")
	}
	return nil
}

// connectionCustodyProtected reports whether the custody finalizer is
// installed, which every token-bearing step requires so a disconnect can
// always reach finalize and its tombstone.
func connectionCustodyProtected(connection *corev1alpha1.Connection) bool {
	return connection != nil && controllerutil.ContainsFinalizer(connection, controller.ConnectionCustodyFinalizer)
}

func connectionOwnedBy(connection *corev1alpha1.Connection, ui *UserInfo) bool {
	return connection != nil && ui != nil &&
		connection.Spec.Subject.Issuer == ui.Issuer && connection.Spec.Subject.Subject == ui.Subject
}

func normalizeConnectionMode(mode string) (string, error) {
	switch mode {
	case "":
		return corev1alpha1.ConnectionModeReadOnly, nil
	case corev1alpha1.ConnectionModeReadOnly, corev1alpha1.ConnectionModeReadWrite:
		return mode, nil
	default:
		return "", fiber.NewError(fiber.StatusBadRequest, "mode must be readOnly or readWrite")
	}
}

// ListConnectors returns the provider catalog for the caller's namespace.
func (h *Handlers) ListConnectors(c fiber.Ctx) error {
	if _, err := h.connectorIdentity(c); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query(toolNamespaceArg, ""))
	if err != nil {
		return err
	}
	list := &corev1alpha1.ConnectorProviderList{}
	if err := h.client.List(c.Context(), list, client.InNamespace(namespace)); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "failed to list connector providers")
	}
	items := make([]ConnectorProviderResponse, 0, len(list.Items))
	for i := range list.Items {
		items = append(items, connectorProviderResponse(&list.Items[i]))
	}
	return c.JSON(fiber.Map{"items": items})
}

// ListConnections returns only the caller's own Connections.
func (h *Handlers) ListConnections(c fiber.Ctx) error {
	ui, err := h.connectorIdentity(c)
	if err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query(toolNamespaceArg, ""))
	if err != nil {
		return err
	}
	list := &corev1alpha1.ConnectionList{}
	if err := h.client.List(c.Context(), list, client.InNamespace(namespace),
		client.MatchingLabels{ConnectionSubjectDigestLabel: connectionSubjectLabel(ui)}); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "failed to list connections")
	}
	items := make([]ConnectionResponse, 0, len(list.Items))
	for i := range list.Items {
		if connectionOwnedBy(&list.Items[i], ui) {
			items = append(items, connectionResponse(&list.Items[i]))
		}
	}
	return c.JSON(fiber.Map{"items": items})
}

// GetConnection returns one of the caller's Connections.
func (h *Handlers) GetConnection(c fiber.Ctx) error {
	ui, err := h.connectorIdentity(c)
	if err != nil {
		return err
	}
	connection, err := h.loadOwnedConnection(c, ui)
	if err != nil {
		return err
	}
	return c.JSON(connectionResponse(connection))
}

// CreateConnection creates (or reuses) the caller's Connection to a provider
// and starts the consent flow. The subject is taken from the verified
// identity only; request bodies carrying any other field are rejected.
func (h *Handlers) CreateConnection(c fiber.Ctx) error {
	ui, err := h.connectorIdentity(c)
	if err != nil {
		return err
	}
	var req createConnectionRequest
	if err := decodeStrictJSON(c.Body(), &req); err != nil {
		return err
	}
	if strings.TrimSpace(req.Provider) == "" {
		return fiber.NewError(fiber.StatusBadRequest, "provider is required")
	}
	mode, err := normalizeConnectionMode(req.Mode)
	if err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query(toolNamespaceArg, ""))
	if err != nil {
		return err
	}
	ctx := c.Context()
	provider, err := h.loadReadyConnectorProvider(ctx, namespace, req.Provider)
	if err != nil {
		return err
	}
	name := connectors.ConnectionName(provider.Name, ui.Issuer, ui.Subject)
	connection := &corev1alpha1.Connection{}
	err = h.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, connection)
	switch {
	case apierrors.IsNotFound(err):
		connection = &corev1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				// Installed at creation so no token can ever be exchanged or
				// parked for a Connection the controller cannot finalize.
				Finalizers: []string{controller.ConnectionCustodyFinalizer},
				Labels: map[string]string{
					ConnectionSubjectDigestLabel: connectionSubjectLabel(ui),
					ConnectionProviderLabel:      provider.Name,
				},
			},
			Spec: corev1alpha1.ConnectionSpec{
				Subject:     corev1alpha1.ConnectionSubject{Issuer: ui.Issuer, Subject: ui.Subject},
				ProviderRef: corev1alpha1.LocalObjectReference{Name: provider.Name},
				Mode:        mode,
			},
		}
		if err := h.client.Create(ctx, connection); err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "failed to create connection")
		}
	case err != nil:
		return fiber.NewError(fiber.StatusInternalServerError, "failed to read connection")
	case !connectionOwnedBy(connection, ui):
		return fiber.NewError(fiber.StatusConflict, "a connection with this name belongs to another identity")
	case !connection.DeletionTimestamp.IsZero():
		return fiber.NewError(fiber.StatusConflict, "the previous connection is still being removed; retry shortly")
	default:
		if connection.Spec.Mode != mode {
			connection.Spec.Mode = mode
			if err := h.client.Update(ctx, connection); err != nil {
				return fiber.NewError(fiber.StatusInternalServerError, "failed to update connection mode")
			}
		}
	}
	authorizeURL, err := h.startConnectorConsent(ctx, connection, provider, mode)
	if err != nil {
		return err
	}
	return c.Status(fiber.StatusCreated).JSON(ConnectionAuthorizeResponse{
		Connection:   connectionResponse(connection),
		AuthorizeURL: authorizeURL,
	})
}

// AuthorizeConnection restarts consent for an existing Connection, for
// example after the provider revoked it.
func (h *Handlers) AuthorizeConnection(c fiber.Ctx) error {
	ui, err := h.connectorIdentity(c)
	if err != nil {
		return err
	}
	connection, err := h.loadOwnedConnection(c, ui)
	if err != nil {
		return err
	}
	provider, err := h.loadReadyConnectorProvider(c.Context(), connection.Namespace, connection.Spec.ProviderRef.Name)
	if err != nil {
		return err
	}
	mode, _ := normalizeConnectionMode(connection.Spec.Mode)
	authorizeURL, err := h.startConnectorConsent(c.Context(), connection, provider, mode)
	if err != nil {
		return err
	}
	return c.JSON(ConnectionAuthorizeResponse{Connection: connectionResponse(connection), AuthorizeURL: authorizeURL})
}

// UpdateConnection changes the mode. Widening to readWrite requests the write
// scopes, so it returns a new consent URL; narrowing takes effect at once.
func (h *Handlers) UpdateConnection(c fiber.Ctx) error {
	ui, err := h.connectorIdentity(c)
	if err != nil {
		return err
	}
	var req updateConnectionRequest
	if err := decodeStrictJSON(c.Body(), &req); err != nil {
		return err
	}
	mode, err := normalizeConnectionMode(req.Mode)
	if err != nil {
		return err
	}
	connection, err := h.loadOwnedConnection(c, ui)
	if err != nil {
		return err
	}
	ctx := c.Context()
	previous, _ := normalizeConnectionMode(connection.Spec.Mode)
	if previous != mode {
		connection.Spec.Mode = mode
		if err := h.client.Update(ctx, connection); err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "failed to update connection mode")
		}
	}
	response := ConnectionAuthorizeResponse{Connection: connectionResponse(connection)}
	if mode == corev1alpha1.ConnectionModeReadWrite && previous != mode {
		provider, err := h.loadReadyConnectorProvider(ctx, connection.Namespace, connection.Spec.ProviderRef.Name)
		if err != nil {
			return err
		}
		authorizeURL, err := h.startConnectorConsent(ctx, connection, provider, mode)
		if err != nil {
			return err
		}
		response.AuthorizeURL = authorizeURL
	}
	return c.JSON(response)
}

// DeleteConnection disconnects. Custody removal and best-effort provider
// revocation happen in the controller's finalizer so kubectl deletes behave
// identically.
func (h *Handlers) DeleteConnection(c fiber.Ctx) error {
	ui, err := h.connectorIdentity(c)
	if err != nil {
		return err
	}
	connection, err := h.loadOwnedConnection(c, ui)
	if err != nil {
		return err
	}
	if err := h.client.Delete(c.Context(), connection); err != nil && !apierrors.IsNotFound(err) {
		return fiber.NewError(fiber.StatusInternalServerError, "failed to delete connection")
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handlers) loadOwnedConnection(c fiber.Ctx, ui *UserInfo) (*corev1alpha1.Connection, error) {
	namespace, err := h.resolveNamespace(c, c.Query(toolNamespaceArg, ""))
	if err != nil {
		return nil, err
	}
	connection := &corev1alpha1.Connection{}
	if err := h.client.Get(c.Context(), types.NamespacedName{Namespace: namespace, Name: c.Params("name")}, connection); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fiber.NewError(fiber.StatusNotFound, "connection not found")
		}
		return nil, fiber.NewError(fiber.StatusInternalServerError, "failed to read connection")
	}
	// A foreign Connection is reported as missing so names cannot be enumerated.
	if !connectionOwnedBy(connection, ui) {
		return nil, fiber.NewError(fiber.StatusNotFound, "connection not found")
	}
	return connection, nil
}

func (h *Handlers) loadReadyConnectorProvider(ctx context.Context, namespace, name string) (*corev1alpha1.ConnectorProvider, error) {
	provider := &corev1alpha1.ConnectorProvider{}
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, provider); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fiber.NewError(fiber.StatusNotFound, "connector provider not found")
		}
		return nil, fiber.NewError(fiber.StatusInternalServerError, "failed to read connector provider")
	}
	if !connectors.ProviderAccepted(provider) {
		return nil, fiber.NewError(fiber.StatusConflict, "connector provider is not ready")
	}
	return provider, nil
}

// providerOAuthConfig reads the client secret through the uncached reader so
// it never enters the informer cache, and returns the resolved configuration.
func (h *Handlers) providerOAuthConfig(ctx context.Context, provider *corev1alpha1.ConnectorProvider) (connectors.OAuthProviderConfig, error) {
	reader := client.Reader(h.client)
	if h.apiReader != nil {
		reader = h.apiReader
	}
	ref := provider.Spec.OAuth.ClientSecretRef
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: provider.Namespace, Name: ref.Name}, secret); err != nil {
		return connectors.OAuthProviderConfig{}, errors.New("connector provider client secret is unavailable")
	}
	value := strings.TrimSpace(string(secret.Data[ref.Key]))
	if value == "" {
		return connectors.OAuthProviderConfig{}, errors.New("connector provider client secret is empty")
	}
	return connectors.ProviderOAuthConfig(provider, value), nil
}

func connectionSubjectLabel(ui *UserInfo) string {
	return connectors.SubjectDigest(ui.Issuer, ui.Subject)[:connectionSubjectLabelLength]
}

// startConnectorConsent records a pending consent and returns the provider
// authorize URL. The PKCE verifier stays sealed server-side.
func (h *Handlers) startConnectorConsent(ctx context.Context, connection *corev1alpha1.Connection, provider *corev1alpha1.ConnectorProvider, mode string) (string, error) {
	if !connectionCustodyProtected(connection) {
		return "", fiber.NewError(fiber.StatusConflict, "connection is not yet protected by the controller; retry shortly")
	}
	cfg, err := h.providerOAuthConfig(ctx, provider)
	if err != nil {
		return "", fiber.NewError(fiber.StatusConflict, err.Error())
	}
	nonce, err := connectors.GenerateStateNonce()
	if err != nil {
		return "", fiber.NewError(fiber.StatusInternalServerError, "failed to start consent")
	}
	verifier, challenge, err := connectors.GeneratePKCE()
	if err != nil {
		return "", fiber.NewError(fiber.StatusInternalServerError, "failed to start consent")
	}
	state, err := connectors.SignState(h.connectors.StateKey, nonce)
	if err != nil {
		return "", fiber.NewError(fiber.StatusInternalServerError, "failed to start consent")
	}
	if err := h.connectors.Consents.CreateConnectorConsent(ctx, store.ConnectorConsent{
		Nonce:         nonce,
		ConnectionUID: string(connection.UID),
		Namespace:     connection.Namespace,
		Name:          connection.Name,
		SubjectDigest: connectors.SubjectDigest(connection.Spec.Subject.Issuer, connection.Spec.Subject.Subject),
		Provider:      provider.Name,
		Mode:          mode,
		CodeVerifier:  verifier,
		ExpiresAt:     h.connectors.now().Add(connectors.ConsentTTL),
	}); err != nil {
		return "", fiber.NewError(fiber.StatusInternalServerError, "failed to record consent")
	}
	authorizeURL, err := connectors.AuthorizeURL(cfg, h.connectors.redirectURI(), state, connectors.ScopesForMode(provider, mode), challenge)
	if err != nil {
		return "", fiber.NewError(fiber.StatusConflict, "connector provider authorize URL is invalid")
	}
	return authorizeURL, nil
}

// ConnectionCallback completes consent. It is unauthenticated because the
// browser arrives from the provider without an Orka bearer token; the signed,
// single-use state plus the sealed verifier are the credential. It never
// echoes provider parameters and never places token material anywhere but
// the sealed custody row.
func (h *Handlers) ConnectionCallback(c fiber.Ctx) error {
	if !h.connectors.Enabled {
		return fiber.NewError(fiber.StatusNotFound, "connectors are not enabled on this controller")
	}
	ctx := c.Context()
	nonce, err := connectors.VerifyState(h.connectors.StateKey, c.Query("state"))
	if err != nil {
		return h.connectorCallbackRedirect(c, "", "invalid_state", "")
	}
	consent, err := h.connectors.Consents.ConsumeConnectorConsent(ctx, nonce)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return h.connectorCallbackRedirect(c, "", "consent_expired", "")
		}
		return h.connectorCallbackRedirect(c, "", "storage_failed", "")
	}
	if h.watchNamespace != "" && consent.Namespace != h.watchNamespace {
		return h.connectorCallbackRedirect(c, "", "invalid_state", "")
	}
	if providerError := strings.TrimSpace(c.Query("error")); providerError != "" {
		reason := "access_denied"
		if providerError != "access_denied" {
			reason = "provider_rejected"
		}
		return h.connectorCallbackRedirect(c, consent.Name, reason, "")
	}
	code := c.Query("code")
	if strings.TrimSpace(code) == "" {
		return h.connectorCallbackRedirect(c, consent.Name, "missing_code", "")
	}
	connection := &corev1alpha1.Connection{}
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: consent.Namespace, Name: consent.Name}, connection); err != nil {
		return h.connectorCallbackRedirect(c, consent.Name, "connection_missing", "")
	}
	if !connectionCustodyProtected(connection) {
		return h.connectorCallbackRedirect(c, consent.Name, "connection_unprotected", "")
	}
	if string(connection.UID) != consent.ConnectionUID || !connection.DeletionTimestamp.IsZero() ||
		connectors.SubjectDigest(connection.Spec.Subject.Issuer, connection.Spec.Subject.Subject) != consent.SubjectDigest ||
		connection.Spec.ProviderRef.Name != consent.Provider {
		return h.connectorCallbackRedirect(c, consent.Name, "connection_mismatch", "")
	}
	provider := &corev1alpha1.ConnectorProvider{}
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: consent.Namespace, Name: consent.Provider}, provider); err != nil || !connectors.ProviderAccepted(provider) {
		return h.connectorCallbackRedirect(c, consent.Name, "provider_unavailable", "")
	}
	cfg, err := h.providerOAuthConfig(ctx, provider)
	if err != nil {
		return h.connectorCallbackRedirect(c, consent.Name, "provider_unavailable", "")
	}
	token, err := h.connectors.OAuth.ExchangeCode(ctx, cfg, code, consent.CodeVerifier, h.connectors.redirectURI())
	if err != nil {
		log.Info("connector code exchange failed", "connection", consent.Name, "provider", consent.Provider, "reason", oauthFailureReason(err))
		return h.connectorCallbackRedirect(c, consent.Name, "exchange_failed", "")
	}
	// Park the material until the verified owner commits it. This is what
	// stops a forwarded consent link from binding a victim's account to the
	// attacker's Connection: the browser that finished consent receives a
	// one-time completion token, and only the Connection's owner may spend it.
	completionNonce, err := connectors.GenerateStateNonce()
	if err != nil {
		return h.connectorCallbackRedirect(c, consent.Name, "storage_failed", "")
	}
	completionToken, err := connectors.SignState(h.connectors.StateKey, completionNonce)
	if err != nil {
		return h.connectorCallbackRedirect(c, consent.Name, "storage_failed", "")
	}
	if err := h.connectors.Consents.CreateConnectorCompletion(ctx, store.ConnectorCompletion{
		Nonce:         completionNonce,
		ConnectionUID: consent.ConnectionUID,
		Namespace:     consent.Namespace,
		Name:          consent.Name,
		SubjectDigest: consent.SubjectDigest,
		Provider:      consent.Provider,
		Mode:          consent.Mode,
		Credential: store.ConnectorCredential{
			AccessToken:  token.AccessToken,
			RefreshToken: token.RefreshToken,
			TokenType:    token.TokenType,
			ExpiresAt:    token.ExpiresAt,
			Scopes:       token.Scopes,
		},
		ExpiresAt: h.connectors.now().Add(connectors.ConsentTTL),
	}); err != nil {
		if errors.Is(err, store.ErrConnectorCustodyTombstoned) {
			return h.connectorCallbackRedirect(c, consent.Name, "disconnected", "")
		}
		log.Error(err, "connector completion could not be sealed", "connection", consent.Name)
		return h.connectorCallbackRedirect(c, consent.Name, "storage_failed", "")
	}
	return h.connectorCallbackRedirect(c, consent.Name, "", completionToken)
}

// CompleteConnection commits parked token material. The caller must own the
// Connection and present the one-time completion token the callback handed
// to the completing browser, so a link can only be finished by the person who
// both started it and approved it.
func (h *Handlers) CompleteConnection(c fiber.Ctx) error {
	ui, err := h.connectorIdentity(c)
	if err != nil {
		return err
	}
	var req completeConnectionRequest
	if err := decodeStrictJSON(c.Body(), &req); err != nil {
		return err
	}
	// Ownership is checked before the token so a non-owner learns nothing
	// about the token, not even whether it is well-formed.
	connection, err := h.loadOwnedConnection(c, ui)
	if err != nil {
		return err
	}
	if !connection.DeletionTimestamp.IsZero() {
		return fiber.NewError(fiber.StatusConflict, "connection is being deleted")
	}
	if !connectionCustodyProtected(connection) {
		return fiber.NewError(fiber.StatusConflict, "connection is not yet protected by the controller; retry shortly")
	}
	nonce, err := connectors.VerifyState(h.connectors.StateKey, strings.TrimSpace(req.Completion))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "completion token is invalid")
	}
	ctx := c.Context()
	completion, err := h.connectors.Consents.ConsumeConnectorCompletion(ctx, nonce)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusConflict, "completion token was already used or has expired")
		}
		return fiber.NewError(fiber.StatusInternalServerError, "failed to read completion")
	}
	if completion.ConnectionUID != string(connection.UID) || completion.Namespace != connection.Namespace ||
		completion.Name != connection.Name || completion.Provider != connection.Spec.ProviderRef.Name ||
		completion.SubjectDigest != connectors.SubjectDigest(connection.Spec.Subject.Issuer, connection.Spec.Subject.Subject) {
		return fiber.NewError(fiber.StatusConflict, "completion token does not belong to this connection")
	}
	ref, err := connectors.CredentialRef(connection)
	if err != nil {
		return fiber.NewError(fiber.StatusConflict, "connection identity is incomplete")
	}
	if err := h.connectors.Credentials.PutConnectorCredential(ctx, ref, completion.Credential); err != nil {
		if errors.Is(err, store.ErrConnectorCustodyTombstoned) {
			return fiber.NewError(fiber.StatusConflict, "connection was disconnected; create it again")
		}
		log.Error(err, "connector credential could not be sealed", "connection", connection.Name)
		return fiber.NewError(fiber.StatusInternalServerError, "failed to store credential")
	}
	if err := h.markConnectionLinked(ctx, connection, completion.Credential); err != nil {
		log.Error(err, "connection status could not be updated after completion", "connection", connection.Name)
		return fiber.NewError(fiber.StatusInternalServerError, "failed to update connection status")
	}
	return c.JSON(connectionResponse(connection))
}

func oauthFailureReason(err error) string {
	if oauthErr, ok := errors.AsType[*connectors.OAuthError](err); ok {
		return fmt.Sprintf("status=%d code=%s", oauthErr.StatusCode, oauthErr.Code)
	}
	return "transport"
}

// markConnectionLinked records the successful consent on the Connection.
func (h *Handlers) markConnectionLinked(ctx context.Context, connection *corev1alpha1.Connection, credential store.ConnectorCredential) error {
	now := metav1.NewTime(h.connectors.now().UTC())
	connection.Status.State = corev1alpha1.ConnectionStateReady
	connection.Status.GrantedScopes = append([]string(nil), credential.Scopes...)
	connection.Status.LinkedAt = &now
	connection.Status.LastRefreshTime = nil
	connection.Status.ExpiresAt = nil
	if !credential.ExpiresAt.IsZero() {
		expires := metav1.NewTime(credential.ExpiresAt.UTC())
		connection.Status.ExpiresAt = &expires
	}
	meta.SetStatusCondition(&connection.Status.Conditions, metav1.Condition{
		Type:               corev1alpha1.ConnectionConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             corev1alpha1.ConnectionReasonLinked,
		Message:            "Account linked",
		ObservedGeneration: connection.Generation,
		LastTransitionTime: now,
	})
	return h.client.Status().Update(ctx, connection)
}

// connectorCallbackRedirect sends the browser back to the dashboard. Only a
// fixed reason code and the Connection name travel in the query string. The
// one-time completion token travels in the URL fragment, which browsers keep
// out of requests, referrers, and server logs.
func (h *Handlers) connectorCallbackRedirect(c fiber.Ctx, connectionName, reason, completionToken string) error {
	target, err := url.Parse(strings.TrimRight(h.connectors.CallbackBaseURL, "/") + connectorSettingsPath)
	if err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "connector callback base URL is invalid")
	}
	query := url.Values{}
	if reason == "" {
		query.Set("status", "pending")
	} else {
		query.Set("status", "error")
		query.Set("reason", reason)
	}
	if connectionName != "" {
		query.Set("connection", connectionName)
	}
	target.RawQuery = query.Encode()
	if completionToken != "" {
		target.Fragment = url.Values{"completion": {completionToken}}.Encode()
	}
	return c.Redirect().Status(fiber.StatusSeeOther).To(target.String())
}
