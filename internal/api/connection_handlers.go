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
	"io"
	"net/url"
	"strings"
	"sync"
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
	ConnectionSubjectDigestLabel = connectors.ConnectionSubjectLabel
	ConnectionProviderLabel      = connectors.ConnectionProviderLabel

	connectorSettingsPath     = "/settings/connectors"
	connectorSchemeHTTPS      = "https"
	connectorSchemeHTTP       = "http"
	maxConnectionRequestBytes = 4 << 10
)

type completionLocks struct {
	mu    sync.Mutex
	locks map[string]*completionLock
}

func newCompletionLocks() *completionLocks {
	return &completionLocks{locks: map[string]*completionLock{}}
}

type completionLock struct {
	mu      sync.Mutex
	waiters int
}

func (l *completionLocks) lock(key string) func() {
	l.mu.Lock()
	entry, ok := l.locks[key]
	if !ok {
		entry = &completionLock{}
		l.locks[key] = entry
	}
	entry.waiters++
	l.mu.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		l.mu.Lock()
		entry.waiters--
		if entry.waiters == 0 {
			delete(l.locks, key)
		}
		l.mu.Unlock()
	}
}

func (l *completionLocks) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.locks)
}

type ConnectorConfig struct {
	Enabled         bool
	CallbackBaseURL string
	StateKey        []byte
	Credentials     store.ConnectorCredentialStore
	Consents        store.ConnectorConsentStore
	OAuth           *connectors.OAuthClient
	Now             func() time.Time
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

func ValidateConnectorConfig(cfg ConnectorConfig) error {
	if !cfg.Enabled {
		return nil
	}
	base := strings.TrimSpace(cfg.CallbackBaseURL)
	parsed, err := url.Parse(base)
	if base == "" || err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.ForceQuery || strings.ContainsAny(base, "?#") || parsed.String() != base {
		return errors.New("connector callback base URL must be an absolute http(s) URL without userinfo, query, or fragment")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return errors.New("connector callback base URL must be an origin without a path")
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

type ConnectorProviderResponse struct {
	Name        string                       `json:"name"`
	Namespace   string                       `json:"namespace"`
	DisplayName string                       `json:"displayName"`
	Ready       bool                         `json:"ready"`
	Scopes      corev1alpha1.ConnectorScopes `json:"scopes"`
	Tools       []ConnectorToolResponse      `json:"tools"`
	AgentHost   bool                         `json:"agentHost,omitempty"`
}

type ConnectorToolResponse struct {
	Name  string `json:"name"`
	Class string `json:"class"`
}

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
	Deleting        bool         `json:"deleting,omitempty"`
	GrantSequence   int64        `json:"grantSequence"`
}

func connectionGetsAgentHost(provider *corev1alpha1.ConnectorProvider, connection *corev1alpha1.Connection) bool {
	return provider.Spec.AgentHost != nil &&
		connection.Annotations[corev1alpha1.ConnectionAnnotationAgentHost] != corev1alpha1.ConnectionAgentHostDeclined
}

type ConnectionAuthorizeResponse struct {
	Connection   ConnectionResponse `json:"connection"`
	AuthorizeURL string             `json:"authorizeURL"`
	AgentHost    bool               `json:"agentHost,omitempty"`
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
		AgentHost:   provider.Spec.AgentHost != nil,
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
		Deleting:        !connection.DeletionTimestamp.IsZero(),
		GrantSequence:   connection.Status.GrantSequence,
	}
	if ready := meta.FindStatusCondition(connection.Status.Conditions, corev1alpha1.ConnectionConditionReady); ready != nil {
		response.Ready = connectors.ConnectionLinked(connection)
		response.Message = ready.Message
	}
	if granted := meta.FindStatusCondition(connection.Status.Conditions, corev1alpha1.ConnectionConditionScopesGranted); granted != nil && granted.Status != metav1.ConditionTrue {
		response.Message = granted.Message
	}
	if resolved := meta.FindStatusCondition(connection.Status.Conditions, corev1alpha1.ConnectionConditionProviderResolved); resolved != nil && resolved.Status != metav1.ConditionTrue {
		response.Ready = false
		response.Message = resolved.Message
	}
	return response
}

func revalidateConnectionView(view *ConnectionResponse, connection *corev1alpha1.Connection, provider *corev1alpha1.ConnectorProvider) {
	if view == nil || !view.Ready || connection == nil {
		return
	}
	switch {
	case provider == nil:
		view.Ready = false
		view.Message = "the provider is no longer configured; this link cannot be used and should be disconnected"
	case !connectors.ProviderAccepted(provider):
		view.Ready = false
		view.Message = "the provider is not accepted right now, so this link cannot be used"
	case !connectors.ConsentMatchesProvider(connection, provider):
		view.Ready = false
		view.Message = "the provider changed since you consented; reconnect this link before its tools can run"
	case !connectors.ScopesCover(connection.Status.GrantedScopes, connectors.ScopesForMode(provider, view.Mode)):
		view.Ready = false
		view.Message = "the provider now requires scopes this link was not granted; reconnect it before its tools can run"
	}
}

func (h *Handlers) connectorIdentity(c fiber.Ctx, action connectorAction) (*UserInfo, error) {
	if !h.connectors.Enabled {
		return nil, fiber.NewError(fiber.StatusNotImplemented, "connectors are not enabled on this controller")
	}
	ui := GetUserInfo(c)
	if ui == nil {
		return nil, fiber.NewError(fiber.StatusUnauthorized, "missing authenticated identity")
	}
	if ui.AuthType != AuthTypeOIDC && ui.AuthType != AuthTypeContextToken && ui.AuthType != AuthTypeGitHubSession {
		return nil, fiber.NewError(fiber.StatusForbidden, "connectors require a verified OIDC, context-token or GitHub sign-in identity")
	}
	if strings.TrimSpace(ui.Subject) == "" || strings.TrimSpace(ui.Issuer) == "" {
		return nil, fiber.NewError(fiber.StatusForbidden, "connectors require an identity with issuer and subject")
	}
	if _, err := h.resolveNamespace(c, c.Query(toolNamespaceArg, "")); err != nil {
		return nil, err
	}
	scopes := h.contextTokenAuthorization.ConnectorReadScopes
	if action == connectorActionManage {
		scopes = h.contextTokenAuthorization.ConnectorManageScopes
	}
	if err := h.authorizeContextTokenAction(c, string(action), scopes); err != nil {
		return nil, err
	}
	return ui, nil
}

type connectorAction string

const (
	connectorActionRead   connectorAction = "connectorsRead"
	connectorActionManage connectorAction = "connectorsManage"
)

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
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body: only one JSON object is accepted")
	}
	return nil
}

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

func (h *Handlers) ListConnectors(c fiber.Ctx) error {
	if _, err := h.connectorIdentity(c, connectorActionRead); err != nil {
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

func (h *Handlers) ListConnections(c fiber.Ctx) error {
	ui, err := h.connectorIdentity(c, connectorActionRead)
	if err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query(toolNamespaceArg, ""))
	if err != nil {
		return err
	}
	list := &corev1alpha1.ConnectionList{}
	if err := h.client.List(c.Context(), list, client.InNamespace(namespace)); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "failed to list connections")
	}
	providers := &corev1alpha1.ConnectorProviderList{}
	if err := h.providerReader().List(c.Context(), providers, client.InNamespace(namespace)); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "failed to list connector providers")
	}
	byName := make(map[string]*corev1alpha1.ConnectorProvider, len(providers.Items))
	for i := range providers.Items {
		byName[providers.Items[i].Name] = &providers.Items[i]
	}
	links := map[string]int{}
	for i := range list.Items {
		if connectionOwnedBy(&list.Items[i], ui) {
			links[list.Items[i].Spec.ProviderRef.Name]++
		}
	}
	items := make([]ConnectionResponse, 0, len(list.Items))
	for i := range list.Items {
		if connectionOwnedBy(&list.Items[i], ui) {
			view := connectionResponse(&list.Items[i])
			revalidateConnectionView(&view, &list.Items[i], byName[list.Items[i].Spec.ProviderRef.Name])
			markDuplicateLink(&view, links[list.Items[i].Spec.ProviderRef.Name])
			items = append(items, view)
		}
	}
	return c.JSON(fiber.Map{"items": items})
}

func (h *Handlers) GetConnection(c fiber.Ctx) error {
	ui, err := h.connectorIdentity(c, connectorActionRead)
	if err != nil {
		return err
	}
	connection, err := h.loadOwnedConnection(c, ui)
	if err != nil {
		return err
	}
	view := connectionResponse(connection)
	provider := &corev1alpha1.ConnectorProvider{}
	switch err := h.providerReader().Get(c.Context(), types.NamespacedName{Namespace: connection.Namespace, Name: connection.Spec.ProviderRef.Name}, provider); {
	case err == nil:
		revalidateConnectionView(&view, connection, provider)
	case apierrors.IsNotFound(err):
		revalidateConnectionView(&view, connection, nil)
	default:
		return fiber.NewError(fiber.StatusInternalServerError, "failed to read connector provider")
	}
	owned := &corev1alpha1.ConnectionList{}
	if err := h.client.List(c.Context(), owned, client.InNamespace(connection.Namespace)); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "failed to list connections")
	}
	links := 0
	for i := range owned.Items {
		if connectionOwnedBy(&owned.Items[i], ui) && owned.Items[i].Spec.ProviderRef.Name == connection.Spec.ProviderRef.Name {
			links++
		}
	}
	markDuplicateLink(&view, links)
	return c.JSON(view)
}

func markDuplicateLink(view *ConnectionResponse, links int) {
	if view == nil || links <= 1 {
		return
	}
	view.Ready = false
	view.Message = "you hold several links to this provider; disconnect the extra ones before its tools can run"
}

func (h *Handlers) CreateConnection(c fiber.Ctx) error {
	ui, err := h.connectorIdentity(c, connectorActionManage)
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
	if apierrors.IsNotFound(err) {
		connection = &corev1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{
				Name:       name,
				Namespace:  namespace,
				Finalizers: []string{controller.ConnectionCustodyFinalizer},
				Labels: map[string]string{
					ConnectionSubjectDigestLabel: connectionSubjectLabel(ui),
					ConnectionProviderLabel:      connectors.ConnectionProviderLabelValue(provider.Name),
				},
			},
			Spec: corev1alpha1.ConnectionSpec{
				Subject:     corev1alpha1.ConnectionSubject{Issuer: ui.Issuer, Subject: ui.Subject},
				ProviderRef: corev1alpha1.LocalObjectReference{Name: provider.Name},
				Mode:        mode,
			},
		}
		err = h.client.Create(ctx, connection)
		if apierrors.IsAlreadyExists(err) {
			connection = &corev1alpha1.Connection{}
			err = h.providerReader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, connection)
		} else if err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "failed to create connection")
		} else {
			err = errConnectionCreated
		}
	}
	switch {
	case errors.Is(err, errConnectionCreated):
	case err != nil:
		return fiber.NewError(fiber.StatusInternalServerError, "failed to read connection")
	case !connectionOwnedBy(connection, ui):
		return fiber.NewError(fiber.StatusConflict, "a connection with this name belongs to another identity")
	case !connection.DeletionTimestamp.IsZero():
		return fiber.NewError(fiber.StatusConflict, "the previous connection is still being removed; retry shortly")
	case connection.Spec.ProviderRef.Name != provider.Name:
		return fiber.NewError(fiber.StatusConflict, "a connection with this name is bound to another provider")
	default:
		wantLabels := map[string]string{ConnectionSubjectDigestLabel: connectionSubjectLabel(ui), ConnectionProviderLabel: connectors.ConnectionProviderLabelValue(provider.Name)}
		changed := connection.Spec.Mode != mode
		for key, value := range wantLabels {
			if connection.Labels[key] != value {
				if connection.Labels == nil {
					connection.Labels = map[string]string{}
				}
				connection.Labels[key] = value
				changed = true
			}
		}
		if changed {
			connection.Spec.Mode = mode
			if err := h.client.Update(ctx, connection); err != nil {
				return fiber.NewError(fiber.StatusInternalServerError, "failed to update connection")
			}
		}
	}
	authorizeURL, err := h.startConnectorConsent(ctx, connection, provider, mode)
	if err != nil {
		return err
	}
	response := ConnectionAuthorizeResponse{Connection: connectionResponse(connection), AuthorizeURL: authorizeURL,
		AgentHost: connectionGetsAgentHost(provider, connection)}
	if !connectors.ScopesCover(connection.Status.GrantedScopes, connectors.ScopesForMode(provider, mode)) ||
		!connectors.ConsentMatchesProvider(connection, provider) {
		markConsentPending(&response.Connection, mode)
	} else if ready := meta.FindStatusCondition(connection.Status.Conditions, corev1alpha1.ConnectionConditionReady); ready != nil && ready.Status == metav1.ConditionTrue {
		response.Connection.Ready = true
		response.Connection.State = corev1alpha1.ConnectionStateReady
	}
	return c.Status(fiber.StatusCreated).JSON(response)
}

var errConnectionCreated = errors.New("connection created")

func markConsentPending(view *ConnectionResponse, mode string) {
	view.Ready = false
	view.State = corev1alpha1.ConnectionStatePending
	view.Message = "Consent is required for the " + mode + " mode"
}

func (h *Handlers) AuthorizeConnection(c fiber.Ctx) error {
	ui, err := h.connectorIdentity(c, connectorActionManage)
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
	return c.JSON(ConnectionAuthorizeResponse{Connection: connectionResponse(connection), AuthorizeURL: authorizeURL,
		AgentHost: connectionGetsAgentHost(provider, connection)})
}

func (h *Handlers) UpdateConnection(c fiber.Ctx) error {
	ui, err := h.connectorIdentity(c, connectorActionManage)
	if err != nil {
		return err
	}
	var req updateConnectionRequest
	if err := decodeStrictJSON(c.Body(), &req); err != nil {
		return err
	}
	if strings.TrimSpace(req.Mode) == "" {
		return fiber.NewError(fiber.StatusBadRequest, "mode is required")
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
	provider, err := h.loadReadyConnectorProvider(ctx, connection.Namespace, connection.Spec.ProviderRef.Name)
	if err != nil {
		return err
	}
	previous, _ := normalizeConnectionMode(connection.Spec.Mode)
	if previous != mode {
		connection.Spec.Mode = mode
		if err := h.client.Update(ctx, connection); err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "failed to update connection mode")
		}
	}
	response := ConnectionAuthorizeResponse{Connection: connectionResponse(connection), AgentHost: connectionGetsAgentHost(provider, connection)}
	if !connectors.ScopesCover(connection.Status.GrantedScopes, connectors.ScopesForMode(provider, mode)) ||
		!connectors.ConsentMatchesProvider(connection, provider) {
		authorizeURL, err := h.startConnectorConsent(ctx, connection, provider, mode)
		if err != nil {
			return err
		}
		response.AuthorizeURL = authorizeURL
		markConsentPending(&response.Connection, mode)
	} else if ready := meta.FindStatusCondition(connection.Status.Conditions, corev1alpha1.ConnectionConditionReady); ready != nil && ready.Status == metav1.ConditionTrue {
		response.Connection.Ready = true
		response.Connection.State = corev1alpha1.ConnectionStateReady
		response.Connection.Message = "Account linked"
	}
	return c.JSON(response)
}

func (h *Handlers) DeleteConnection(c fiber.Ctx) error {
	ui, err := h.connectorIdentity(c, connectorActionManage)
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
	if !connectionOwnedBy(connection, ui) {
		return nil, fiber.NewError(fiber.StatusNotFound, "connection not found")
	}
	return connection, nil
}

func (h *Handlers) providerReader() client.Reader {
	if h.apiReader != nil {
		return h.apiReader
	}
	return h.client
}

func (h *Handlers) loadReadyConnectorProvider(ctx context.Context, namespace, name string) (*corev1alpha1.ConnectorProvider, error) {
	provider := &corev1alpha1.ConnectorProvider{}
	if err := h.providerReader().Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, provider); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fiber.NewError(fiber.StatusNotFound, "connector provider not found")
		}
		return nil, fiber.NewError(fiber.StatusInternalServerError, "failed to read connector provider")
	}
	if !connectors.ProviderAccepted(provider) {
		return nil, fiber.NewError(fiber.StatusConflict, "connector provider is not ready; retry shortly")
	}
	return provider, nil
}

func (h *Handlers) providerOAuthConfig(ctx context.Context, provider *corev1alpha1.ConnectorProvider) (connectors.OAuthProviderConfig, error) {
	reader := h.providerReader()
	ref := provider.Spec.OAuth.ClientSecretRef
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: provider.Namespace, Name: ref.Name}, secret); err != nil {
		return connectors.OAuthProviderConfig{}, errors.New("connector provider client secret is unavailable")
	}
	value := string(secret.Data[ref.Key])
	if strings.TrimSpace(value) == "" {
		return connectors.OAuthProviderConfig{}, errors.New("connector provider client secret is empty")
	}
	return connectors.ProviderOAuthConfig(provider, value), nil
}

func connectionSubjectLabel(ui *UserInfo) string {
	return connectors.ConnectionSubjectLabelValue(ui.Issuer, ui.Subject)
}

func (h *Handlers) startConnectorConsent(ctx context.Context, connection *corev1alpha1.Connection, provider *corev1alpha1.ConnectorProvider, mode string) (string, error) {
	if !connection.DeletionTimestamp.IsZero() {
		return "", fiber.NewError(fiber.StatusConflict, "connection is being deleted")
	}
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
		Nonce:            nonce,
		ConnectionUID:    string(connection.UID),
		Namespace:        connection.Namespace,
		Name:             connection.Name,
		SubjectDigest:    connectors.SubjectDigest(connection.Spec.Subject.Issuer, connection.Spec.Subject.Subject),
		Provider:         provider.Name,
		Mode:             mode,
		CodeVerifier:     verifier,
		RevocationDigest: connectors.ProviderRevocationDigest(provider),
		AuthorityDigest:  connectors.ProviderAuthorityDigest(provider),
		Scopes:           connectors.ScopesForMode(provider, mode),
		ExpiresAt:        h.connectors.now().Add(connectors.ConsentTTL),
	}); err != nil {
		return "", fiber.NewError(fiber.StatusInternalServerError, "failed to record consent")
	}
	authorizeURL, err := connectors.AuthorizeURL(cfg, h.connectors.redirectURI(), state, connectors.ScopesForMode(provider, mode), challenge)
	if err != nil {
		return "", fiber.NewError(fiber.StatusConflict, "connector provider authorize URL is invalid")
	}
	return authorizeURL, nil
}

func (h *Handlers) ConnectionCallback(c fiber.Ctx) error {
	if !h.connectors.Enabled {
		return fiber.NewError(fiber.StatusNotFound, "connectors are not enabled on this controller")
	}
	ctx := c.Context()
	nonce, err := connectors.VerifyState(h.connectors.StateKey, c.Query("state"))
	if err != nil {
		return h.connectorCallbackRedirect(c, "invalid_state")
	}
	consent, err := h.connectors.Consents.ConsumeConnectorConsent(ctx, nonce)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return h.connectorCallbackRedirect(c, "consent_expired")
		}
		return h.connectorCallbackRedirect(c, "storage_failed")
	}
	if h.watchNamespace != "" && consent.Namespace != h.watchNamespace {
		return h.connectorCallbackRedirect(c, "invalid_state")
	}
	if providerError := strings.TrimSpace(c.Query("error")); providerError != "" {
		const accessDenied = "access_denied"
		reason := accessDenied
		if providerError != accessDenied {
			reason = "provider_rejected"
		}
		return h.connectorCallbackRedirectTo(c, consent.Namespace, consent.Name, reason, "")
	}
	code := c.Query("code")
	if strings.TrimSpace(code) == "" {
		return h.connectorCallbackRedirectTo(c, consent.Namespace, consent.Name, "missing_code", "")
	}
	connection := &corev1alpha1.Connection{}
	if err := h.providerReader().Get(ctx, types.NamespacedName{Namespace: consent.Namespace, Name: consent.Name}, connection); err != nil {
		return h.connectorCallbackRedirectTo(c, consent.Namespace, consent.Name, "connection_missing", "")
	}
	if !connectionCustodyProtected(connection) {
		return h.connectorCallbackRedirectTo(c, consent.Namespace, consent.Name, "connection_unprotected", "")
	}
	if string(connection.UID) != consent.ConnectionUID || !connection.DeletionTimestamp.IsZero() ||
		connectors.SubjectDigest(connection.Spec.Subject.Issuer, connection.Spec.Subject.Subject) != consent.SubjectDigest ||
		connection.Spec.ProviderRef.Name != consent.Provider {
		return h.connectorCallbackRedirectTo(c, consent.Namespace, consent.Name, "connection_mismatch", "")
	}
	if currentMode, err := normalizeConnectionMode(connection.Spec.Mode); err != nil || currentMode != consent.Mode {
		return h.connectorCallbackRedirectTo(c, consent.Namespace, consent.Name, "mode_changed", "")
	}
	provider := &corev1alpha1.ConnectorProvider{}
	if err := h.providerReader().Get(ctx, types.NamespacedName{Namespace: consent.Namespace, Name: consent.Provider}, provider); err != nil || !connectors.ProviderAccepted(provider) {
		return h.connectorCallbackRedirectTo(c, consent.Namespace, consent.Name, "provider_unavailable", "")
	}
	if consent.AuthorityDigest != connectors.ProviderAuthorityDigest(provider) {
		return h.connectorCallbackRedirectTo(c, consent.Namespace, consent.Name, "provider_changed", "")
	}
	cfg, err := h.providerOAuthConfig(ctx, provider)
	if err != nil {
		return h.connectorCallbackRedirectTo(c, consent.Namespace, consent.Name, "provider_unavailable", "")
	}
	token, err := h.connectors.OAuth.ExchangeCode(ctx, cfg, code, consent.CodeVerifier, h.connectors.redirectURI())
	if err != nil {
		log.Info("connector code exchange failed", "connection", consent.Name, "provider", consent.Provider, "reason", oauthFailureReason(err))
		return h.connectorCallbackRedirectTo(c, consent.Namespace, consent.Name, "exchange_failed", "")
	}
	required := consent.Scopes
	if !token.ScopePresent {
		token.Scopes = append([]string(nil), required...)
	} else if !connectors.ScopesCover(token.Scopes, required) {
		return h.connectorCallbackRedirectTo(c, consent.Namespace, consent.Name, "scopes_denied", "")
	}
	completionNonce, err := connectors.GenerateStateNonce()
	if err != nil {
		return h.connectorCallbackRedirectTo(c, consent.Namespace, consent.Name, "storage_failed", "")
	}
	completionToken, err := connectors.SignState(h.connectors.StateKey, completionNonce)
	if err != nil {
		return h.connectorCallbackRedirectTo(c, consent.Namespace, consent.Name, "storage_failed", "")
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
			AccessToken:      token.AccessToken,
			RefreshToken:     token.RefreshToken,
			TokenType:        token.TokenType,
			ExpiresAt:        token.ExpiresAt,
			Scopes:           token.Scopes,
			AuthorityDigest:  connectors.ProviderIssuerDigest(provider),
			RevocationDigest: consent.RevocationDigest,
		},
		ConsentAuthorityDigest: consent.AuthorityDigest,
		ConsentSequence:        consent.Sequence,
		ExpiresAt:              h.connectors.now().Add(connectors.ConsentTTL),
	}); err != nil {
		if errors.Is(err, store.ErrConnectorCustodyTombstoned) {
			return h.connectorCallbackRedirectTo(c, consent.Namespace, consent.Name, "disconnected", "")
		}
		if errors.Is(err, store.ErrConnectorConsentSuperseded) {
			return h.connectorCallbackRedirectTo(c, consent.Namespace, consent.Name, "consent_superseded", "")
		}
		log.Error(err, "connector completion could not be sealed", "connection", consent.Name)
		return h.connectorCallbackRedirectTo(c, consent.Namespace, consent.Name, "storage_failed", "")
	}
	return h.connectorCallbackRedirectTo(c, consent.Namespace, consent.Name, "", completionToken)
}

func (h *Handlers) CompleteConnection(c fiber.Ctx) error {
	ui, err := h.connectorIdentity(c, connectorActionManage)
	if err != nil {
		return err
	}
	var req completeConnectionRequest
	if err := decodeStrictJSON(c.Body(), &req); err != nil {
		return err
	}
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
	unlock := h.completionLocks.lock(string(connection.UID))
	defer unlock()
	completion, err := h.connectors.Consents.PeekConnectorCompletion(ctx, nonce)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusConflict, "completion token was already used or has expired")
		}
		return fiber.NewError(fiber.StatusInternalServerError, "failed to read completion")
	}
	provider, err := h.loadReadyConnectorProvider(ctx, connection.Namespace, connection.Spec.ProviderRef.Name)
	if err != nil {
		return err
	}
	if err := completionFenceError(connection, provider, completion, !completion.Committed); err != nil {
		h.discardCompletion(ctx, completion, nonce)
		return err
	}
	ref, err := connectors.CredentialRef(connection)
	if err != nil {
		return fiber.NewError(fiber.StatusConflict, "connection identity is incomplete")
	}
	if !completion.Committed && completion.Credential.RefreshToken == "" && !completion.Credential.ExpiresAt.IsZero() &&
		!completion.Credential.ExpiresAt.After(h.connectors.now()) {
		h.discardCompletion(ctx, completion, nonce)
		return fiber.NewError(fiber.StatusConflict, "the granted token expired before completion; start consent again")
	}
	if completion.Committed {
		current, err := h.connectors.Credentials.GetConnectorCredential(ctx, ref)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusInternalServerError, "failed to read custody; retry")
		}
		if err != nil || current.GrantSequence != completion.Credential.GrantSequence ||
			current.AccessToken != completion.Credential.AccessToken ||
			current.RefreshToken != completion.Credential.RefreshToken || current.AuthorityDigest != completion.Credential.AuthorityDigest {
			h.discardCompletion(ctx, completion, nonce)
			return fiber.NewError(fiber.StatusConflict, "a newer completion replaced this one; nothing to resume")
		}
		completion.Credential = current
	} else if committed, err := h.connectors.Consents.CommitConnectorCompletion(ctx, nonce, ref, completion.Credential); err == nil {
		completion.Credential = committed
	} else {
		return h.completionCommitError(ctx, err, completion, nonce, connection.Name)
	}
	if err := h.markConnectionLinked(ctx, connection, provider, completion.Credential); err != nil {
		log.Error(err, "connection status could not be updated after completion; the completion token remains valid for retry", "connection", connection.Name)
		return fiber.NewError(fiber.StatusInternalServerError, "failed to update connection status; retry")
	}
	if err := h.connectors.Consents.DeleteConnectorCompletion(ctx, nonce); err != nil {
		log.Error(err, "consumed completion could not be removed", "connection", connection.Name)
	}
	return c.JSON(connectionResponse(connection))
}

func (h *Handlers) completionCommitError(ctx context.Context, err error, completion store.ConnectorCompletion, nonce, connectionName string) error {
	switch {
	case errors.Is(err, store.ErrConnectorCustodyTombstoned):
		h.discardCompletion(ctx, completion, nonce)
		return fiber.NewError(fiber.StatusConflict, "connection was disconnected; create it again")
	case errors.Is(err, store.ErrConnectorCompletionCommitted):
		return fiber.NewError(fiber.StatusConflict, "completion was already committed; retry to finish it")
	case errors.Is(err, store.ErrNotFound):
		return fiber.NewError(fiber.StatusConflict, "completion token was already used or has expired")
	case errors.Is(err, store.ErrConnectorConsentSuperseded):
		return fiber.NewError(fiber.StatusConflict, "a newer consent for this connection superseded this one; finish that consent or start again")
	case errors.Is(err, store.ErrConnectorRetiredLimit):
		h.discardCompletion(ctx, completion, nonce)
		return fiber.NewError(fiber.StatusConflict, "this connection retains too many superseded grants; disconnect it and link again")
	}
	log.Error(err, "connector credential could not be sealed", "connection", connectionName)
	return fiber.NewError(fiber.StatusInternalServerError, "failed to store credential")
}

func completionFenceError(connection *corev1alpha1.Connection, provider *corev1alpha1.ConnectorProvider, completion store.ConnectorCompletion, requireMode bool) error {
	if completion.ConnectionUID != string(connection.UID) || completion.Namespace != connection.Namespace ||
		completion.Name != connection.Name || completion.Provider != connection.Spec.ProviderRef.Name ||
		completion.SubjectDigest != connectors.SubjectDigest(connection.Spec.Subject.Issuer, connection.Spec.Subject.Subject) {
		return fiber.NewError(fiber.StatusConflict, "completion token does not belong to this connection")
	}
	currentMode, _ := normalizeConnectionMode(connection.Spec.Mode)
	if requireMode && completion.Mode != currentMode {
		return fiber.NewError(fiber.StatusConflict, "the connection mode changed after consent started; start consent again")
	}
	if completion.Credential.AuthorityDigest != connectors.ProviderIssuerDigest(provider) ||
		completion.ConsentAuthorityDigest != connectors.ProviderAuthorityDigest(provider) {
		return fiber.NewError(fiber.StatusConflict, "the connector provider changed after consent started; start consent again")
	}
	return nil
}

func (h *Handlers) discardCompletion(ctx context.Context, completion store.ConnectorCompletion, nonce string) {
	if err := h.connectors.Consents.DeleteConnectorCompletion(ctx, nonce); err != nil {
		log.Error(err, "discarded completion could not be removed", "connection", completion.Name)
	}
}

func oauthFailureReason(err error) string {
	if oauthErr, ok := errors.AsType[*connectors.OAuthError](err); ok {
		return fmt.Sprintf("status=%d code=%s", oauthErr.StatusCode, oauthErr.Code)
	}
	return "transport"
}

func (h *Handlers) markConnectionLinked(ctx context.Context, connection *corev1alpha1.Connection, provider *corev1alpha1.ConnectorProvider, credential store.ConnectorCredential) error {
	const maxAttempts = 4
	for attempt := 1; ; attempt++ {
		err := h.applyConnectionLinked(ctx, connection, provider, credential)
		if err == nil || !apierrors.IsConflict(err) || attempt >= maxAttempts {
			return err
		}
		fresh := &corev1alpha1.Connection{}
		if getErr := h.client.Get(ctx, client.ObjectKeyFromObject(connection), fresh); getErr != nil {
			return getErr
		}
		if fresh.UID != connection.UID || !fresh.DeletionTimestamp.IsZero() {
			return err
		}
		*connection = *fresh
	}
}

func (h *Handlers) applyConnectionLinked(ctx context.Context, connection *corev1alpha1.Connection, provider *corev1alpha1.ConnectorProvider, credential store.ConnectorCredential) error {
	if connection.Status.GrantSequence > credential.GrantSequence {
		return nil
	}
	connectors.ApplyLinkedStatus(connection, provider, credential, metav1.NewTime(h.connectors.now().UTC()))
	return h.client.Status().Update(ctx, connection)
}

func (h *Handlers) connectorCallbackRedirect(c fiber.Ctx, reason string) error {
	return h.connectorCallbackRedirectTo(c, "", "", reason, "")
}

func (h *Handlers) connectorCallbackRedirectTo(c fiber.Ctx, namespace, connectionName, reason, completionToken string) error {
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
	if strings.TrimSpace(namespace) != "" {
		query.Set("namespace", namespace)
	}
	target.RawQuery = query.Encode()
	if completionToken != "" {
		target.Fragment = url.Values{"completion": {completionToken}}.Encode()
	}
	return c.Redirect().Status(fiber.StatusSeeOther).To(target.String())
}
