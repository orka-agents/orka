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
	// ConnectionSubjectDigestLabel indexes Connections by owner without
	// exposing the raw subject as a label value.
	ConnectionSubjectDigestLabel = connectors.ConnectionSubjectLabel
	// ConnectionProviderLabel indexes Connections by provider.
	ConnectionProviderLabel = connectors.ConnectionProviderLabel

	connectorSettingsPath     = "/settings/connectors"
	connectorSchemeHTTPS      = "https"
	connectorSchemeHTTP       = "http"
	maxConnectionRequestBytes = 4 << 10
)

// completionLocks serializes completion per Connection UID so a stalled
// duplicate request cannot write a superseded credential after a newer link
// committed. The API server is a single process, so a process lock suffices.
type completionLocks struct {
	mu    sync.Mutex
	locks map[string]*completionLock
}

func newCompletionLocks() *completionLocks {
	return &completionLocks{locks: map[string]*completionLock{}}
}

// completionLock is one keyed mutex with a waiter count, so the entry is
// dropped when the last holder releases it and the map stays bounded by
// the number of in-flight completions rather than by account churn.
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

// size reports the number of keyed entries currently held or awaited.
func (l *completionLocks) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.locks)
}

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
	// A bare "?" or "#" parses to an empty query or fragment but would turn
	// the appended callback path into one; the delimiters themselves are
	// refused, as is anything whose canonical form differs from the input.
	if base == "" || err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.ForceQuery || strings.ContainsAny(base, "?#") || parsed.String() != base {
		return errors.New("connector callback base URL must be an absolute http(s) URL without userinfo, query, or fragment")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		// The callback is mounted at the server root, so a prefixed origin
		// would register a redirect URI nothing serves.
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
	// Deleting marks a Connection whose disconnect is still finishing
	// (the finalizer revokes tokens first).
	Deleting bool `json:"deleting,omitempty"`
	// GrantSequence advances on every completed consent, so a client can
	// tell a new grant from the one it started with.
	GrantSequence int64 `json:"grantSequence"`
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

// revalidateConnectionView applies the checks credential resolution makes
// against the current provider to a Ready view: a provider that is gone or
// not accepted, one changed since consent, or one that now requires scopes
// the grant lacks refuses the token at once, before the Connection's own
// conditions catch up. The
// dashboard and CLI only see this view, so it must not advertise a link
// that resolution refuses.
func revalidateConnectionView(view *ConnectionResponse, connection *corev1alpha1.Connection, provider *corev1alpha1.ConnectorProvider) {
	if view == nil || !view.Ready || connection == nil {
		return
	}
	switch {
	case provider == nil:
		// The Connection keeps its tokens until disconnected, but nothing
		// resolves a link whose provider is gone.
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

// connectorIdentity returns the verified human identity behind a request.
// ServiceAccount and other TokenReview callers fail closed: a shared
// ServiceAccount must never be able to link or use a person's accounts.
func (h *Handlers) connectorIdentity(c fiber.Ctx, action connectorAction) (*UserInfo, error) {
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
	// Resolve the namespace first so the delegated transaction constraints
	// (tctx.namespace) are compared against the namespace this request will
	// actually act in, not skipped for lack of one.
	if _, err := h.resolveNamespace(c, c.Query(toolNamespaceArg, "")); err != nil {
		return nil, err
	}
	// A delegated context token acts within its scopes: a token narrowed to
	// unrelated work must not be able to link, inspect, or revoke accounts.
	scopes := h.contextTokenAuthorization.ConnectorReadScopes
	if action == connectorActionManage {
		scopes = h.contextTokenAuthorization.ConnectorManageScopes
	}
	if err := h.authorizeContextTokenAction(c, string(action), scopes); err != nil {
		return nil, err
	}
	return ui, nil
}

// connectorAction names the scope class a connector route needs.
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
	// One JSON object and nothing after it: a second value would otherwise
	// be ignored, carrying fields the first one was refused for.
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body: only one JSON object is accepted")
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

// ListConnections returns only the caller's own Connections.
func (h *Handlers) ListConnections(c fiber.Ctx) error {
	ui, err := h.connectorIdentity(c, connectorActionRead)
	if err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query(toolNamespaceArg, ""))
	if err != nil {
		return err
	}
	// Ownership is judged from spec.subject, never the index label alone:
	// a Connection created through Kubernetes, or whose label was
	// stripped, is still listed for its owner.
	list := &corev1alpha1.ConnectionList{}
	if err := h.client.List(c.Context(), list, client.InNamespace(namespace)); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "failed to list connections")
	}
	// Providers are read uncached, as credential resolution reads them, so
	// informer lag never advertises a link resolution already refuses.
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

// GetConnection returns one of the caller's Connections.
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

// markDuplicateLink marks a link unusable when the person holds more than
// one link to its provider: credential resolution refuses every one of them
// as ambiguous, so none may be advertised as ready.
func markDuplicateLink(view *ConnectionResponse, links int) {
	if view == nil || links <= 1 {
		return
	}
	view.Ready = false
	view.Message = "you hold several links to this provider; disconnect the extra ones before its tools can run"
}

// CreateConnection creates (or reuses) the caller's Connection to a provider
// and starts the consent flow. The subject is taken from the verified
// identity only; request bodies carrying any other field are rejected.
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
				Name:      name,
				Namespace: namespace,
				// Installed at creation so no token can ever be exchanged or
				// parked for a Connection the controller cannot finalize.
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
			// A concurrent request for the same identity and provider created
			// it first; this one reuses it under the same ownership checks.
			// Read uncached: the informer may not have seen the winner yet.
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
		// The provider reference is immutable; an object created outside
		// the API for another provider cannot be relabeled into this one, and
		// a consent started for it could never link.
		return fiber.NewError(fiber.StatusConflict, "a connection with this name is bound to another provider")
	default:
		// A reused object may lack the ownership labels the list route
		// selects by (created through Kubernetes, or labels stripped);
		// restore them from the authoritative spec before consent.
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
	response := ConnectionAuthorizeResponse{Connection: connectionResponse(connection), AuthorizeURL: authorizeURL}
	if !connectors.ScopesCover(connection.Status.GrantedScopes, connectors.ScopesForMode(provider, mode)) ||
		!connectors.ConsentMatchesProvider(connection, provider) {
		// A reused link whose grant does not cover the requested mode is
		// pending until this consent completes, whatever the stale
		// conditions say.
		markConsentPending(&response.Connection, mode)
	} else if ready := meta.FindStatusCondition(connection.Status.Conditions, corev1alpha1.ConnectionConditionReady); ready != nil && ready.Status == metav1.ConditionTrue {
		// A reused link narrowed to a mode its grant already covers stays
		// usable; the spec write bumped the generation the conditions
		// observe, which the controller catches up on its next pass.
		response.Connection.Ready = true
		response.Connection.State = corev1alpha1.ConnectionStateReady
	}
	return c.Status(fiber.StatusCreated).JSON(response)
}

// errConnectionCreated marks a Connection this request created itself, so
// the reuse checks are skipped.
var errConnectionCreated = errors.New("connection created")

// markConsentPending projects a link as unusable while consent for mode is
// outstanding, regardless of controller conditions that have not observed
// the change yet.
func markConsentPending(view *ConnectionResponse, mode string) {
	view.Ready = false
	view.State = corev1alpha1.ConnectionStatePending
	view.Message = "Consent is required for the " + mode + " mode"
}

// AuthorizeConnection restarts consent for an existing Connection, for
// example after the provider revoked it.
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
	return c.JSON(ConnectionAuthorizeResponse{Connection: connectionResponse(connection), AuthorizeURL: authorizeURL})
}

// UpdateConnection changes the mode. Widening to readWrite requests the write
// scopes, so it returns a new consent URL; narrowing takes effect at once.
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
		// The empty default belongs to creation only; an update must say what
		// it wants so a client cannot narrow a link by omission.
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
	response := ConnectionAuthorizeResponse{Connection: connectionResponse(connection)}
	// Consent is needed whenever the granted scopes do not cover the mode or
	// the provider's OAuth client changed, not only on the request that
	// changed the mode, so a retry after a failed consent start still
	// returns an authorize URL.
	if !connectors.ScopesCover(connection.Status.GrantedScopes, connectors.ScopesForMode(provider, mode)) ||
		!connectors.ConsentMatchesProvider(connection, provider) {
		authorizeURL, err := h.startConnectorConsent(ctx, connection, provider, mode)
		if err != nil {
			return err
		}
		response.AuthorizeURL = authorizeURL
		// The controller has not judged the new mode yet; do not let the
		// stale conditions advertise a usable link in the meantime.
		markConsentPending(&response.Connection, mode)
	} else if ready := meta.FindStatusCondition(connection.Status.Conditions, corev1alpha1.ConnectionConditionReady); ready != nil && ready.Status == metav1.ConditionTrue {
		// Narrowing to a mode the existing grant covers is usable at once,
		// even though the controller's conditions still observe the previous
		// generation.
		response.Connection.Ready = true
		response.Connection.State = corev1alpha1.ConnectionStateReady
		response.Connection.Message = "Account linked"
	}
	return c.JSON(response)
}

// DeleteConnection disconnects. Custody removal and best-effort provider
// revocation happen in the controller's finalizer so kubectl deletes behave
// identically.
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
	// A foreign Connection is reported as missing so names cannot be enumerated.
	if !connectionOwnedBy(connection, ui) {
		return nil, fiber.NewError(fiber.StatusNotFound, "connection not found")
	}
	return connection, nil
}

// providerReader reads ConnectorProviders through the uncached reader when
// one is configured: the reads that precede a code exchange or a custody
// commit must see the provider as it is, not as the informer last saw it.
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
		// Retryable: the completion row is kept while the provider's
		// conditions catch up. Every retryable conflict says "retry" so a
		// client can tell it from a consent that can never be finished.
		return nil, fiber.NewError(fiber.StatusConflict, "connector provider is not ready; retry shortly")
	}
	return provider, nil
}

// providerOAuthConfig reads the client secret through the uncached reader so
// it never enters the informer cache, and returns the resolved configuration.
func (h *Handlers) providerOAuthConfig(ctx context.Context, provider *corev1alpha1.ConnectorProvider) (connectors.OAuthProviderConfig, error) {
	reader := h.providerReader()
	ref := provider.Spec.OAuth.ClientSecretRef
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: provider.Namespace, Name: ref.Name}, secret); err != nil {
		return connectors.OAuthProviderConfig{}, errors.New("connector provider client secret is unavailable")
	}
	// The secret is opaque bytes; only emptiness is judged, never trimmed.
	value := string(secret.Data[ref.Key])
	if strings.TrimSpace(value) == "" {
		return connectors.OAuthProviderConfig{}, errors.New("connector provider client secret is empty")
	}
	return connectors.ProviderOAuthConfig(provider, value), nil
}

func connectionSubjectLabel(ui *UserInfo) string {
	return connectors.ConnectionSubjectLabelValue(ui.Issuer, ui.Subject)
}

// startConnectorConsent records a pending consent and returns the provider
// authorize URL. The PKCE verifier stays sealed server-side.
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
		Nonce:         nonce,
		ConnectionUID: string(connection.UID),
		Namespace:     connection.Namespace,
		Name:          connection.Name,
		SubjectDigest: connectors.SubjectDigest(connection.Spec.Subject.Issuer, connection.Spec.Subject.Subject),
		Provider:      provider.Name,
		Mode:          mode,
		CodeVerifier:  verifier,
		// The revocation identity is bound when consent starts and carried
		// through the callback, so an endpoint moved during the window is
		// never handed the tokens at disconnect.
		RevocationDigest: connectors.ProviderRevocationDigest(provider),
		// The code may only ever be exchanged with this OAuth client and
		// for these scopes; a token that reports no scope grants exactly them.
		AuthorityDigest: connectors.ProviderAuthorityDigest(provider),
		Scopes:          connectors.ScopesForMode(provider, mode),
		ExpiresAt:       h.connectors.now().Add(connectors.ConsentTTL),
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
		return h.connectorCallbackRedirectTo(c, consent.Namespace, consent.Name, reason, "")
	}
	code := c.Query("code")
	if strings.TrimSpace(code) == "" {
		return h.connectorCallbackRedirectTo(c, consent.Namespace, consent.Name, "missing_code", "")
	}
	// Read uncached: the fences below decide whether a code is exchanged
	// at all, so they judge the Connection as it is now.
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
	// The mode changed while the person was at the provider: completion
	// would refuse the result, so no code is exchanged and no token issued.
	if currentMode, err := normalizeConnectionMode(connection.Spec.Mode); err != nil || currentMode != consent.Mode {
		return h.connectorCallbackRedirectTo(c, consent.Namespace, consent.Name, "mode_changed", "")
	}
	provider := &corev1alpha1.ConnectorProvider{}
	if err := h.providerReader().Get(ctx, types.NamespacedName{Namespace: consent.Namespace, Name: consent.Provider}, provider); err != nil || !connectors.ProviderAccepted(provider) {
		return h.connectorCallbackRedirectTo(c, consent.Namespace, consent.Name, "provider_unavailable", "")
	}
	// The provider was replaced or its OAuth client changed while the person
	// was at the provider: the code belongs to the old client and must not be
	// exchanged with the new endpoints.
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
	// A provider may grant fewer scopes than requested (the person declined
	// the write permission, say). A partial grant would let a readWrite link
	// advertise tools its token cannot use, so refuse it and revoke the
	// token rather than parking it. A provider that reports no scopes is
	// taken at its word for the requested set.
	// The requested set is the one sealed with the consent, never the
	// provider's current configuration: a provider expanded after consent
	// started must not be credited with scopes the token never received,
	// and a consent that legitimately requested no scopes stays empty.
	// Only an omitted scope field means "as requested"; an explicitly
	// empty one is a grant of nothing.
	required := consent.Scopes
	if !token.ScopePresent {
		token.Scopes = append([]string(nil), required...)
	} else if !connectors.ScopesCover(token.Scopes, required) {
		// The issued material is dropped, never revoked: Orka cannot prove
		// whose grant a token nobody committed belongs to, and a shared or
		// re-issued token could be another person's live credential.
		return h.connectorCallbackRedirectTo(c, consent.Namespace, consent.Name, "scopes_denied", "")
	}
	// Park the material until the verified owner commits it. This is what
	// stops a forwarded consent link from binding a victim's account to the
	// attacker's Connection: the browser that finished consent receives a
	// one-time completion token, and only the Connection's owner may spend it.
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
			AccessToken:  token.AccessToken,
			RefreshToken: token.RefreshToken,
			TokenType:    token.TokenType,
			ExpiresAt:    token.ExpiresAt,
			Scopes:       token.Scopes,
			// Sealed with the tokens: whichever path later refreshes or
			// revokes them knows the client that issued them, independent
			// of tool-destination changes.
			AuthorityDigest:  connectors.ProviderIssuerDigest(provider),
			RevocationDigest: consent.RevocationDigest,
		},
		// The full authority the person consented to (client identity plus
		// tool destinations), verified again at completion.
		ConsentAuthorityDigest: consent.AuthorityDigest,
		ConsentSequence:        consent.Sequence,
		ExpiresAt:              h.connectors.now().Add(connectors.ConsentTTL),
	}); err != nil {
		if errors.Is(err, store.ErrConnectorCustodyTombstoned) {
			// The link was disconnected while the code was being exchanged.
			// The material is dropped and left to expire, never revoked.
			return h.connectorCallbackRedirectTo(c, consent.Namespace, consent.Name, "disconnected", "")
		}
		if errors.Is(err, store.ErrConnectorConsentSuperseded) {
			// A newer consent for this Connection already parked or
			// committed its tokens; this older one is dropped.
			return h.connectorCallbackRedirectTo(c, consent.Namespace, consent.Name, "consent_superseded", "")
		}
		log.Error(err, "connector completion could not be sealed", "connection", consent.Name)
		return h.connectorCallbackRedirectTo(c, consent.Namespace, consent.Name, "storage_failed", "")
	}
	return h.connectorCallbackRedirectTo(c, consent.Namespace, consent.Name, "", completionToken)
}

// CompleteConnection commits parked token material. The caller must own the
// Connection and present the one-time completion token the callback handed
// to the completing browser, so a link can only be finished by the person who
// both started it and approved it.
func (h *Handlers) CompleteConnection(c fiber.Ctx) error {
	ui, err := h.connectorIdentity(c, connectorActionManage)
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
	// One completion at a time per Connection: a duplicate that waited here
	// re-reads the row and finds it consumed.
	unlock := h.completionLocks.lock(string(connection.UID))
	defer unlock()
	// Peek rather than consume: the row is removed only after the credential
	// and the linked status are both committed, so a transient failure in
	// between can be retried with the same token.
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
	// The completion must still belong to this Connection and to the
	// provider authority (OAuth client and consented tool destinations) the
	// person consented under; otherwise it is discarded so it cannot be
	// tried again or across Connections. A committed completion is exempt
	// only from the mode check: custody already holds its material, and a
	// mode change since must not discard the only record that repairs a
	// lost status write, while recording it against the current mode
	// projects Pending on its own when the scopes fall short.
	if err := completionFenceError(connection, provider, completion, !completion.Committed); err != nil {
		h.discardCompletion(ctx, completion, nonce)
		return err
	}
	ref, err := connectors.CredentialRef(connection)
	if err != nil {
		return fiber.NewError(fiber.StatusConflict, "connection identity is incomplete")
	}
	// A short-lived token without a refresh token that expired while the
	// browser held the completion can never authenticate; committing it
	// would advertise a Ready link nothing can use.
	if !completion.Committed && completion.Credential.RefreshToken == "" && !completion.Credential.ExpiresAt.IsZero() &&
		!completion.Credential.ExpiresAt.After(h.connectors.now()) {
		h.discardCompletion(ctx, completion, nonce)
		return fiber.NewError(fiber.StatusConflict, "the granted token expired before completion; start consent again")
	}
	if completion.Committed {
		// A retry after the status write failed: custody was written by
		// this completion. It resumes only if that material is still what
		// custody holds; a newer completion that replaced it wins.
		current, err := h.connectors.Credentials.GetConnectorCredential(ctx, ref)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return fiber.NewError(fiber.StatusInternalServerError, "failed to read custody; retry")
		}
		// The committed completion carries the grant custody assigned it;
		// custody holding any other grant (even one that re-issued the same
		// token strings with other scopes or expiry) means a newer
		// completion replaced this one.
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

// completionCommitError maps a failed custody commit to the response, and
// discards the parked completion when it can never be committed.
func (h *Handlers) completionCommitError(ctx context.Context, err error, completion store.ConnectorCompletion, nonce, connectionName string) error {
	switch {
	case errors.Is(err, store.ErrConnectorCustodyTombstoned):
		h.discardCompletion(ctx, completion, nonce)
		return fiber.NewError(fiber.StatusConflict, "connection was disconnected; create it again")
	case errors.Is(err, store.ErrConnectorCompletionCommitted):
		// Another API replica committed it meanwhile; a retry resumes it.
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

// completionFenceError judges a parked completion against the Connection
// that presents it: it must belong to this Connection, to the mode in force
// now (a narrower or wider mode would mismatch the granted scopes), and to
// the provider's current OAuth client and consented destinations.
// completionFenceError judges a parked completion against its Connection and
// the provider's current authority; requireMode additionally demands the mode
// consent started under (a committed completion is recorded against the
// current mode instead).
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

// discardCompletion deletes a parked completion that will never be
// committed. Its tokens are left to expire, never revoked: Orka cannot prove
// whose grant a token nobody committed belongs to (a forwarded consent link
// completed by an already-linked person parks that person's token under
// another Connection, and providers re-issue long-lived tokens), so a
// revocation could sever a live link that is not this one.
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

// markConnectionLinked records the successful consent on the Connection,
// including the provider OAuth client it was granted against. Custody was
// already replaced, so a Connection that changed meanwhile (a PUT that moved
// the mode, a reconciler status pass) is re-read and judged again against
// the committed material rather than left describing a grant custody no
// longer holds.
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
		// A later consent already recorded its grant; this older one must
		// not roll the status back behind custody.
		return nil
	}
	connectors.ApplyLinkedStatus(connection, provider, credential, metav1.NewTime(h.connectors.now().UTC()))
	return h.client.Status().Update(ctx, connection)
}

// connectorCallbackRedirect sends the browser back to the dashboard. Only a
// fixed reason code and the Connection name travel in the query string. The
// one-time completion token travels in the URL fragment, which browsers keep
// out of requests, referrers, and server logs.
func (h *Handlers) connectorCallbackRedirect(c fiber.Ctx, connectionName, reason, completionToken string) error {
	return h.connectorCallbackRedirectTo(c, "", connectionName, reason, completionToken)
}

// connectorCallbackRedirectTo is connectorCallbackRedirect with the
// namespace the consent was sealed in, so the page completes the link where
// it was started rather than in whatever namespace it currently shows.
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
