/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/connectors/credential"
	"github.com/orka-agents/orka/internal/controller"
	"github.com/orka-agents/orka/internal/githubauth"
	"github.com/orka-agents/orka/internal/store"
)

const (
	AuthTypeGitHubSession = "githubSession"

	sessionTokenPrefix       = "orka_s_"
	SessionTTL               = 12 * time.Hour
	sessionRenewHorizon      = 5 * time.Minute
	maxGitHubAccessTokenLife = 8 * time.Hour
)

type GitHubSignInConfig struct {
	Enabled     bool
	Provider    string
	Allowed     githubauth.Allowed
	CreateHosts bool
	Sessions    store.AuthSessionStore
	Credentials interface {
		HostCredential(ctx context.Context, namespace, name string, horizon time.Duration) (credential.HostCredential, error)
	}
	GitHub githubauth.Client
	Now    func() time.Time
}

func (c GitHubSignInConfig) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c GitHubSignInConfig) authenticateSession(ctx context.Context, token string) (*UserInfo, error) {
	if !c.Enabled || c.Sessions == nil {
		return nil, errors.New("GitHub sign-in sessions are not enabled")
	}
	session, err := c.Sessions.GetAuthSessionByHash(ctx, getTokenHash(token))
	if errors.Is(err, store.ErrNotFound) {
		return nil, errors.New("unknown session")
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errSessionStoreUnavailable, err)
	}
	if session.RevokedAt != nil || !c.now().Before(session.ExpiresAt) {
		return nil, errors.New("session ended")
	}
	return &UserInfo{
		AuthType: AuthTypeGitHubSession, UID: session.ID,
		Issuer: session.Issuer, Subject: session.Subject, Username: session.Username, Namespace: session.Namespace,
	}, nil
}

type AuthConfigResponse struct {
	OIDC       bool                    `json:"oidc"`
	OIDCConfig *OIDCAuthConfigOption   `json:"oidcConfig,omitempty"`
	GitHub     *GitHubAuthConfigOption `json:"github,omitempty"`
}

type OIDCAuthConfigOption struct {
	Issuer   string   `json:"issuer"`
	Audience string   `json:"audience"`
	Scopes   []string `json:"scopes,omitempty"`
}

type GitHubAuthConfigOption struct {
	ClientID  string `json:"clientID"`
	AgentHost bool   `json:"agentHost"`
}

func (h *Handlers) signInProvider(ctx context.Context) (*corev1alpha1.ConnectorProvider, error) {
	provider, err := h.loadReadyConnectorProvider(ctx, h.watchNamespace, h.githubSignIn.Provider)
	if err != nil {
		return nil, err
	}
	if provider.Spec.AgentHost == nil {
		return nil, fiber.NewError(fiber.StatusServiceUnavailable, "the GitHub sign-in provider does not serve agent hosts")
	}
	providers := &corev1alpha1.ConnectorProviderList{}
	if err := h.providerReader().List(ctx, providers, client.InNamespace(provider.Namespace)); err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, "failed to read connector providers")
	}
	serving := 0
	for i := range providers.Items {
		if providers.Items[i].Spec.AgentHost != nil && providers.Items[i].DeletionTimestamp.IsZero() {
			serving++
		}
	}
	if serving > 1 {
		return nil, fiber.NewError(fiber.StatusServiceUnavailable,
			"more than one ConnectorProvider in this namespace serves agent hosts; sign-in waits until only one does")
	}
	return provider, nil
}

func (h *Handlers) GetAuthConfig(c fiber.Ctx) error {
	response := AuthConfigResponse{OIDC: h.oidcEnabled}
	if h.oidcEnabled && h.oidcIssuer != "" && h.oidcAudience != "" {
		response.OIDCConfig = &OIDCAuthConfigOption{
			Issuer:   h.oidcIssuer,
			Audience: h.oidcAudience,
			Scopes:   h.oidcScopes,
		}
	}
	if cfg := h.githubSignIn; cfg.Enabled {
		provider, err := h.signInProvider(c.Context())
		if err != nil {
			return err
		}
		response.GitHub = &GitHubAuthConfigOption{
			ClientID:  provider.Spec.OAuth.ClientID,
			AgentHost: provider.Spec.AgentHost != nil && cfg.CreateHosts,
		}
	}
	return c.JSON(response)
}

type githubSignInRequest struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int    `json:"expiresIn"`
	NoHost       bool   `json:"noHost"`
}

type SessionResponse struct {
	Token      string    `json:"token"`
	ExpiresAt  time.Time `json:"expiresAt"`
	Login      string    `json:"login"`
	Subject    string    `json:"subject"`
	Namespace  string    `json:"namespace"`
	Connection string    `json:"connection,omitempty"`
	AgentHost  bool      `json:"agentHost"`
}

func (h *Handlers) SignInWithGitHub(c fiber.Ctx) error {
	cfg := h.githubSignIn
	if !cfg.Enabled || cfg.Sessions == nil || !h.connectors.Enabled {
		return fiber.NewError(fiber.StatusNotImplemented, "GitHub sign-in is not enabled on this controller")
	}
	if h.signInLimiter != nil && !h.signInLimiter.AllowClientFirst(c.IP(), time.Now()) {
		c.Set(fiber.HeaderRetryAfter, "10")
		return fiber.NewError(fiber.StatusTooManyRequests, "too many sign-in attempts; try again shortly")
	}
	var req githubSignInRequest
	if err := decodeStrictJSON(c.Body(), &req); err != nil {
		return err
	}
	if strings.TrimSpace(req.AccessToken) == "" {
		return fiber.NewError(fiber.StatusBadRequest, "accessToken is required")
	}
	ctx := c.Context()
	provider, err := h.signInProvider(ctx)
	if err != nil {
		return err
	}
	oauth, err := h.providerOAuthConfig(ctx, provider)
	if err != nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "the GitHub App is not configured")
	}
	user, err := cfg.GitHub.CheckToken(ctx, oauth.ClientID, oauth.ClientSecret, req.AccessToken)
	if errors.Is(err, githubauth.ErrInvalidToken) {
		return fiber.NewError(fiber.StatusUnauthorized, "the GitHub token was not issued to this Orka's GitHub App")
	}
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, "could not check the GitHub token")
	}
	tokens := githubauth.Tokens{AccessToken: req.AccessToken, ExpiresIn: req.ExpiresIn}
	minted := false
	expiryConfirmed := false
	revokeUnused := func(token string) {
		if minted {
			h.revokeSignInToken(ctx, oauth, token)
		}
	}
	if strings.TrimSpace(req.RefreshToken) != "" {
		refreshed, err := h.connectors.OAuth.Refresh(ctx, oauth, req.RefreshToken)
		if err != nil {
			return fiber.NewError(fiber.StatusUnauthorized, "GitHub did not accept the refresh token")
		}
		minted = true
		expiryConfirmed = !refreshed.ExpiresAt.IsZero() && refreshed.RefreshToken != ""
		again, err := cfg.GitHub.CheckToken(ctx, oauth.ClientID, oauth.ClientSecret, refreshed.AccessToken)
		if err != nil || again.ID != user.ID {
			revokeUnused(refreshed.AccessToken)
			if err != nil && !errors.Is(err, githubauth.ErrInvalidToken) {
				return fiber.NewError(fiber.StatusBadGateway, "could not check the GitHub token")
			}
			return fiber.NewError(fiber.StatusUnauthorized, "the GitHub access and refresh tokens belong to different accounts")
		}
		user = again
		tokens = githubauth.Tokens{AccessToken: refreshed.AccessToken, RefreshToken: refreshed.RefreshToken}
		if !refreshed.ExpiresAt.IsZero() {
			tokens.ExpiresIn = int(max(time.Until(refreshed.ExpiresAt), 0) / time.Second)
		}
	}
	if err := cfg.GitHub.RequireMember(ctx, tokens.AccessToken, user.Login, cfg.Allowed); err != nil {
		revokeUnused(tokens.AccessToken)
		if errors.Is(err, githubauth.ErrNotMember) {
			return fiber.NewError(fiber.StatusForbidden, "you are not an active member of a GitHub org or team allowed to sign in")
		}
		return fiber.NewError(fiber.StatusServiceUnavailable, "could not check your GitHub membership; sign in again shortly")
	}
	ui := &UserInfo{
		AuthType: AuthTypeGitHubSession, Issuer: githubauth.Issuer, Subject: strconv.FormatInt(user.ID, 10),
		Username: user.Login, Namespace: h.watchNamespace,
	}
	declined := req.NoHost || !cfg.CreateHosts || !expiryConfirmed
	connection, held, deadline, err := h.linkGitHubSignIn(ctx, ui, provider, oauth, tokens, declined)
	if !held {
		revokeUnused(tokens.AccessToken)
	}
	if err != nil {
		return err
	}
	response, err := h.issueSession(ctx, ui, deadline)
	if err != nil {
		return err
	}
	response.Connection = connection.Name
	response.AgentHost = provider.Spec.AgentHost != nil && !declined
	return c.Status(fiber.StatusCreated).JSON(response)
}

func (h *Handlers) RenewSession(c fiber.Ctx) error {
	ui, err := h.sessionIdentity(c)
	if err != nil {
		return err
	}
	cfg := h.githubSignIn
	ctx := c.Context()
	name := connectors.ConnectionName(cfg.Provider, ui.Issuer, ui.Subject)
	if cfg.Credentials == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "session renewal is not available")
	}
	if h.signInLimiter != nil && !h.signInLimiter.AllowClientFirst("renew\x00"+ui.Issuer+"\x00"+ui.Subject, time.Now()) {
		c.Set(fiber.HeaderRetryAfter, "10")
		return fiber.NewError(fiber.StatusTooManyRequests, "too many session renewals; try again shortly")
	}
	held, err := cfg.Credentials.HostCredential(ctx, h.watchNamespace, name, sessionRenewHorizon)
	if errors.Is(err, credential.ErrHostLinkUnavailable) {
		return fiber.NewError(fiber.StatusUnauthorized, "your GitHub link is gone; sign in again")
	}
	if err != nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "could not refresh your GitHub token; retry")
	}
	if !held.Refreshable {
		return fiber.NewError(fiber.StatusUnauthorized, "your GitHub link cannot be refreshed and ends with this session; sign in again")
	}
	provider, err := h.signInProvider(ctx)
	if err != nil {
		return err
	}
	oauth, err := h.providerOAuthConfig(ctx, provider)
	if err != nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "the GitHub App is not configured")
	}
	account, err := cfg.GitHub.CheckToken(ctx, oauth.ClientID, oauth.ClientSecret, held.AccessToken)
	if err != nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "could not check your GitHub token; retry")
	}
	if strconv.FormatInt(account.ID, 10) != ui.Subject {
		return fiber.NewError(fiber.StatusUnauthorized, "your GitHub link holds another account; sign in again")
	}
	if err := cfg.GitHub.RequireMember(ctx, held.AccessToken, account.Login, cfg.Allowed); err != nil {
		if errors.Is(err, githubauth.ErrNotMember) {
			return fiber.NewError(fiber.StatusForbidden, "you are no longer an active member of a GitHub org or team allowed to sign in")
		}
		return fiber.NewError(fiber.StatusServiceUnavailable, "could not check your GitHub membership; retry")
	}
	ui.Username = account.Login
	token, next, err := h.newSession(ui, time.Time{})
	if err != nil {
		return err
	}
	if err := cfg.Sessions.RenewAuthSession(ctx, ui.UID, next, cfg.now()); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return fiber.NewError(fiber.StatusUnauthorized, "this session already ended; sign in again")
		}
		return fiber.NewError(fiber.StatusInternalServerError, "could not renew the session")
	}
	return c.Status(fiber.StatusCreated).JSON(SessionResponse{
		Token: token, ExpiresAt: next.ExpiresAt, Login: next.Username, Subject: next.Subject, Namespace: next.Namespace, Connection: name,
	})
}

func (h *Handlers) SignOut(c fiber.Ctx) error {
	ui, err := h.sessionIdentity(c)
	if err != nil {
		return err
	}
	if err := h.githubSignIn.Sessions.RevokeAuthSession(c.Context(), ui.UID, h.githubSignIn.now()); err != nil &&
		!errors.Is(err, store.ErrNotFound) {
		return fiber.NewError(fiber.StatusInternalServerError, "could not end the session")
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handlers) sessionIdentity(c fiber.Ctx) (*UserInfo, error) {
	if !h.githubSignIn.Enabled || h.githubSignIn.Sessions == nil {
		return nil, fiber.NewError(fiber.StatusNotImplemented, "GitHub sign-in is not enabled on this controller")
	}
	ui := GetUserInfo(c)
	if ui == nil || ui.AuthType != AuthTypeGitHubSession || ui.UID == "" {
		return nil, fiber.NewError(fiber.StatusForbidden, "this request needs an Orka session from orka login")
	}
	return ui, nil
}

func (h *Handlers) issueSession(ctx context.Context, ui *UserInfo, deadline time.Time) (SessionResponse, error) {
	token, session, err := h.newSession(ui, deadline)
	if err != nil {
		return SessionResponse{}, err
	}
	if err := h.githubSignIn.Sessions.CreateAuthSession(ctx, session); err != nil {
		return SessionResponse{}, fiber.NewError(fiber.StatusInternalServerError, "could not store the session")
	}
	return SessionResponse{Token: token, ExpiresAt: session.ExpiresAt, Login: session.Username, Subject: session.Subject,
		Namespace: session.Namespace}, nil
}

func (h *Handlers) newSession(ui *UserInfo, deadline time.Time) (string, store.AuthSession, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", store.AuthSession{}, fiber.NewError(fiber.StatusInternalServerError, "could not create a session")
	}
	token := sessionTokenPrefix + base64.RawURLEncoding.EncodeToString(secret)
	now := h.githubSignIn.now().UTC()
	expires := now.Add(SessionTTL)
	if !deadline.IsZero() && deadline.Before(expires) {
		expires = deadline.UTC()
	}
	return token, store.AuthSession{
		ID: uuid.NewString(), TokenHash: getTokenHash(token),
		Issuer: ui.Issuer, Subject: ui.Subject, Username: ui.Username, Namespace: ui.Namespace,
		CreatedAt: now, ExpiresAt: expires,
	}, nil
}

func gitHubTokenExpiry(now time.Time, expiresIn int) time.Time {
	if expiresIn > 0 && expiresIn < int(maxGitHubAccessTokenLife/time.Second) {
		return now.Add(time.Duration(expiresIn) * time.Second)
	}
	return now.Add(maxGitHubAccessTokenLife)
}

var errSignInLinkUnrecorded = fiber.NewError(fiber.StatusInternalServerError, "failed to record your GitHub link; sign in again")

func (h *Handlers) linkGitHubSignIn(ctx context.Context, ui *UserInfo, provider *corev1alpha1.ConnectorProvider,
	oauth connectors.OAuthProviderConfig, tokens githubauth.Tokens, declined bool) (connection *corev1alpha1.Connection, held bool, deadline time.Time, err error) {
	connection, err = h.ensureSignInConnection(ctx, ui, provider, declined)
	if err != nil {
		return nil, false, time.Time{}, err
	}
	unlock := h.completionLocks.lock(string(connection.UID))
	defer unlock()
	if connection, err = h.reloadSignInConnection(ctx, connection, ui); err != nil {
		return nil, false, time.Time{}, err
	}
	if declined {
		if err := h.recordAgentHostDecline(ctx, connection, ui, true); err != nil {
			return nil, false, time.Time{}, err
		}
	}
	kept, holds, err := h.signInLinkHolds(ctx, connection, oauth, ui, !declined)
	custody := kept.AccessToken
	if err != nil {
		return nil, custody != "" && tokens.AccessToken == custody, time.Time{},
			fiber.NewError(fiber.StatusServiceUnavailable, "could not check your GitHub link; retry")
	}
	if holds {
		if !declined {
			if err := h.recordAgentHostDecline(ctx, connection, ui, false); err != nil {
				return nil, tokens.AccessToken == custody, time.Time{}, err
			}
		}
		if !kept.Refreshable {
			deadline = kept.ExpiresAt
		}
		return connection, tokens.AccessToken == custody, deadline, nil
	}
	mode, err := normalizeConnectionMode(connection.Spec.Mode)
	if err != nil {
		return nil, false, time.Time{}, fiber.NewError(fiber.StatusConflict, "your link has an invalid mode")
	}

	now := h.connectors.now()
	consentNonce, err := connectors.GenerateStateNonce()
	if err != nil {
		return nil, false, time.Time{}, fiber.NewError(fiber.StatusInternalServerError, "failed to link GitHub")
	}
	verifier, _, err := connectors.GeneratePKCE()
	if err != nil {
		return nil, false, time.Time{}, fiber.NewError(fiber.StatusInternalServerError, "failed to link GitHub")
	}
	subject := connectors.SubjectDigest(ui.Issuer, ui.Subject)
	scopes := connectors.ScopesForMode(provider, mode)
	if err := h.connectors.Consents.CreateConnectorConsent(ctx, store.ConnectorConsent{
		Nonce: consentNonce, ConnectionUID: string(connection.UID), Namespace: connection.Namespace, Name: connection.Name,
		SubjectDigest: subject, Provider: provider.Name, Mode: mode, CodeVerifier: verifier,
		RevocationDigest: connectors.ProviderRevocationDigest(provider),
		AuthorityDigest:  connectors.ProviderAuthorityDigest(provider),
		Scopes:           scopes, ExpiresAt: now.Add(connectors.ConsentTTL),
	}); err != nil {
		return nil, false, time.Time{}, fiber.NewError(fiber.StatusInternalServerError, "failed to record consent")
	}
	consent, err := h.connectors.Consents.ConsumeConnectorConsent(ctx, consentNonce)
	if err != nil {
		return nil, false, time.Time{}, fiber.NewError(fiber.StatusInternalServerError, "failed to record consent")
	}
	expires := gitHubTokenExpiry(now, tokens.ExpiresIn)
	var linked time.Time
	if tokens.RefreshToken == "" {
		linked = expires
	}
	completionNonce, err := connectors.GenerateStateNonce()
	if err != nil {
		return nil, false, time.Time{}, fiber.NewError(fiber.StatusInternalServerError, "failed to link GitHub")
	}
	completion := store.ConnectorCompletion{
		Nonce: completionNonce, ConnectionUID: consent.ConnectionUID, Namespace: consent.Namespace, Name: consent.Name,
		SubjectDigest: consent.SubjectDigest, Provider: consent.Provider, Mode: consent.Mode,
		Credential: store.ConnectorCredential{
			AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, TokenType: "bearer", ExpiresAt: expires,
			Scopes: append([]string(nil), consent.Scopes...), AuthorityDigest: connectors.ProviderIssuerDigest(provider),
			RevocationDigest: consent.RevocationDigest,
		},
		ConsentAuthorityDigest: consent.AuthorityDigest, ConsentSequence: consent.Sequence,
		ExpiresAt: now.Add(connectors.ConsentTTL),
	}
	if err := h.connectors.Consents.CreateConnectorCompletion(ctx, completion); err != nil {
		if errors.Is(err, store.ErrConnectorCustodyTombstoned) {
			return nil, false, time.Time{}, fiber.NewError(fiber.StatusConflict, "your GitHub link is being disconnected; sign in again in a moment")
		}
		if errors.Is(err, store.ErrConnectorConsentSuperseded) {
			return nil, false, time.Time{}, fiber.NewError(fiber.StatusConflict, "another sign-in is linking GitHub at the same time; retry")
		}
		return nil, true, time.Time{}, fiber.NewError(fiber.StatusInternalServerError, "failed to link GitHub")
	}
	ref, err := connectors.CredentialRef(connection)
	if err != nil {
		return nil, true, time.Time{}, fiber.NewError(fiber.StatusConflict, "your link's identity is incomplete")
	}
	committed, err := h.connectors.Consents.CommitConnectorCompletion(ctx, completionNonce, ref, completion.Credential)
	if err != nil {
		return nil, true, time.Time{}, h.completionCommitError(ctx, err, completion, completionNonce, connection.Name)
	}
	if !declined {
		if err := h.recordAgentHostDecline(ctx, connection, ui, false); err != nil {
			return nil, true, time.Time{}, err
		}
	}
	if err := h.markConnectionLinked(ctx, connection, provider, committed); err != nil {
		return connection, true, linked, errSignInLinkUnrecorded
	}
	if err := h.connectors.Consents.DeleteConnectorCompletion(ctx, completionNonce); err != nil {
		log.Error(err, "consumed sign-in completion could not be removed", "connection", connection.Name)
	}
	return connection, true, linked, nil
}

func (h *Handlers) recordAgentHostDecline(ctx context.Context, connection *corev1alpha1.Connection, ui *UserInfo, declined bool) error {
	for attempt := 1; ; attempt++ {
		if (connection.Annotations[corev1alpha1.ConnectionAnnotationAgentHost] == corev1alpha1.ConnectionAgentHostDeclined) == declined {
			return nil
		}
		base := connection.DeepCopy()
		setAgentHostDeclined(connection, declined)
		err := h.client.Patch(ctx, connection, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
		if err == nil {
			return nil
		}
		if (!apierrors.IsConflict(err) && !apierrors.IsNotFound(err)) || attempt == 3 {
			return fiber.NewError(fiber.StatusInternalServerError, "failed to update your GitHub link; sign in again")
		}
		fresh, err := h.reloadSignInConnection(ctx, base, ui)
		if err != nil {
			return err
		}
		*connection = *fresh
	}
}

func (h *Handlers) revokeSignInToken(ctx context.Context, oauth connectors.OAuthProviderConfig, token string) {
	if err := h.githubSignIn.GitHub.DeleteToken(ctx, oauth.ClientID, oauth.ClientSecret, token); err != nil {
		log.Error(err, "could not revoke a sign-in's unused GitHub token")
	}
}

func (h *Handlers) signInLinkHolds(ctx context.Context, connection *corev1alpha1.Connection,
	oauth connectors.OAuthProviderConfig, ui *UserInfo, startsHost bool) (credential.HostCredential, bool, error) {
	cfg := h.githubSignIn
	none := credential.HostCredential{}
	if cfg.Credentials == nil || !connectors.ConnectionLinked(connection) {
		return none, false, nil
	}
	held, err := cfg.Credentials.HostCredential(ctx, connection.Namespace, connection.Name, sessionRenewHorizon)
	if errors.Is(err, credential.ErrHostLinkUnavailable) {
		return none, false, nil
	}
	if err != nil {
		return none, false, err
	}
	if held.ExpiresAt.IsZero() || (startsHost && !held.Refreshable) {
		return none, false, nil
	}
	account, err := cfg.GitHub.CheckToken(ctx, oauth.ClientID, oauth.ClientSecret, held.AccessToken)
	if errors.Is(err, githubauth.ErrInvalidToken) {
		return none, false, nil
	}
	if err != nil {
		return credential.HostCredential{AccessToken: held.AccessToken}, false, err
	}
	if strconv.FormatInt(account.ID, 10) != ui.Subject {
		return none, false, nil
	}
	return held, true, nil
}

func (h *Handlers) reloadSignInConnection(ctx context.Context, connection *corev1alpha1.Connection, ui *UserInfo) (*corev1alpha1.Connection, error) {
	fresh := &corev1alpha1.Connection{}
	if err := h.providerReader().Get(ctx, client.ObjectKeyFromObject(connection), fresh); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fiber.NewError(fiber.StatusConflict, "your GitHub link was removed; sign in again")
		}
		return nil, fiber.NewError(fiber.StatusInternalServerError, "failed to read your GitHub link")
	}
	if fresh.UID != connection.UID || !fresh.DeletionTimestamp.IsZero() {
		return nil, fiber.NewError(fiber.StatusConflict, "your GitHub link is being disconnected; sign in again in a moment")
	}
	if !connectionOwnedBy(fresh, ui) || !connectionCustodyProtected(fresh) {
		return nil, fiber.NewError(fiber.StatusConflict, "your GitHub link changed; sign in again")
	}
	return fresh, nil
}

func (h *Handlers) ensureSignInConnection(ctx context.Context, ui *UserInfo, provider *corev1alpha1.ConnectorProvider,
	declined bool) (*corev1alpha1.Connection, error) {
	name := connectors.ConnectionName(provider.Name, ui.Issuer, ui.Subject)
	connection := &corev1alpha1.Connection{}
	err := h.client.Get(ctx, types.NamespacedName{Namespace: provider.Namespace, Name: name}, connection)
	if apierrors.IsNotFound(err) {
		connection = &corev1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: provider.Namespace,
				Finalizers: []string{controller.ConnectionCustodyFinalizer},
				Labels: map[string]string{
					ConnectionSubjectDigestLabel: connectionSubjectLabel(ui),
					ConnectionProviderLabel:      connectors.ConnectionProviderLabelValue(provider.Name),
				},
			},
			Spec: corev1alpha1.ConnectionSpec{
				Subject:     corev1alpha1.ConnectionSubject{Issuer: ui.Issuer, Subject: ui.Subject},
				ProviderRef: corev1alpha1.LocalObjectReference{Name: provider.Name},
				Mode:        corev1alpha1.ConnectionModeReadOnly,
			},
		}
		setAgentHostDeclined(connection, declined)
		err = h.client.Create(ctx, connection)
		if err == nil {
			return connection, nil
		}
		if !apierrors.IsAlreadyExists(err) {
			return nil, fiber.NewError(fiber.StatusInternalServerError, "failed to create your GitHub link")
		}
		connection = &corev1alpha1.Connection{}
		err = h.providerReader().Get(ctx, types.NamespacedName{Namespace: provider.Namespace, Name: name}, connection)
	}
	if err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, "failed to read your GitHub link")
	}
	if !connection.DeletionTimestamp.IsZero() {
		return nil, fiber.NewError(fiber.StatusConflict, "your GitHub link is being disconnected; sign in again in a moment")
	}
	if !connectionOwnedBy(connection, ui) {
		return nil, fiber.NewError(fiber.StatusConflict, "a link with your name belongs to someone else")
	}
	if connection.Spec.ProviderRef.Name != provider.Name {
		return nil, fiber.NewError(fiber.StatusConflict, "a link with your name is bound to another provider")
	}
	if !connectionCustodyProtected(connection) {
		return nil, fiber.NewError(fiber.StatusConflict, "your GitHub link is still being set up; sign in again in a moment")
	}
	return connection, nil
}

func setAgentHostDeclined(connection *corev1alpha1.Connection, declined bool) {
	if declined {
		if connection.Annotations == nil {
			connection.Annotations = map[string]string{}
		}
		connection.Annotations[corev1alpha1.ConnectionAnnotationAgentHost] = corev1alpha1.ConnectionAgentHostDeclined
		return
	}
	delete(connection.Annotations, corev1alpha1.ConnectionAnnotationAgentHost)
}
