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
	// AuthTypeGitHubSession identifies an Orka session issued after a
	// GitHub sign-in (ADR 0037).
	AuthTypeGitHubSession = "githubSession"

	// sessionTokenPrefix marks Orka session tokens, so the API never tries
	// them as OIDC or ServiceAccount tokens.
	sessionTokenPrefix = "orka_s_"
	// SessionTTL is how long a sign-in session lasts before it renews.
	SessionTTL = 12 * time.Hour
	// sessionRenewHorizon is how fresh the GitHub token behind a renewal is.
	sessionRenewHorizon = 5 * time.Minute
	// maxGitHubAccessTokenLife bounds the expiry a sign-in claims for its
	// GitHub token, and is the expiry of one that claims none: GitHub App
	// user tokens last eight hours. A link that cannot refresh then ends, and
	// every refresh checks membership again.
	maxGitHubAccessTokenLife = 8 * time.Hour
)

// GitHubSignInConfig signs people in with GitHub (ADR 0037). The GitHub App
// is the agent-host ConnectorProvider's: its OAuth client checks tokens, and
// a sign-in links the person's account through it.
type GitHubSignInConfig struct {
	Enabled bool
	// Provider names the ConnectorProvider, in the watch namespace, whose
	// GitHub App signs people in.
	Provider string
	// Allowed lists the orgs and teams whose active members may sign in.
	Allowed githubauth.Allowed
	// CreateHosts false records every sign-in as declining an agent host.
	CreateHosts bool
	Sessions    store.AuthSessionStore
	// Credentials renews a person's GitHub token from custody.
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

// authenticateSession resolves an Orka session token to its person. Session
// identities are verified people, but GitHub sessions reach only the routes
// that act on the caller's own links, host and AHP tokens.
func (c GitHubSignInConfig) authenticateSession(ctx context.Context, token string) (*UserInfo, error) {
	if !c.Enabled || c.Sessions == nil {
		return nil, errors.New("GitHub sign-in sessions are not enabled")
	}
	session, err := c.Sessions.GetAuthSessionByHash(ctx, getTokenHash(token))
	if err != nil {
		return nil, errors.New("unknown session")
	}
	if session.RevokedAt != nil || !c.now().Before(session.ExpiresAt) {
		return nil, errors.New("session ended")
	}
	return &UserInfo{
		AuthType: AuthTypeGitHubSession, UID: session.ID,
		Issuer: session.Issuer, Subject: session.Subject, Username: session.Username, Namespace: session.Namespace,
	}, nil
}

// AuthConfigResponse tells a client how it may sign in. It holds nothing
// secret.
type AuthConfigResponse struct {
	OIDC   bool                    `json:"oidc"`
	GitHub *GitHubAuthConfigOption `json:"github,omitempty"`
}

// GitHubAuthConfigOption is the GitHub sign-in a client runs the device flow
// for.
type GitHubAuthConfigOption struct {
	ClientID string `json:"clientID"`
	// AgentHost says that signing in gives the person an agent host whose
	// code can read the GitHub token.
	AgentHost bool `json:"agentHost"`
}

// signInProvider is the ready provider sign-in links people through. It
// must serve agent hosts: sessions renew from the host link, and sign-in
// starts hosts unless the person declines one.
func (h *Handlers) signInProvider(ctx context.Context) (*corev1alpha1.ConnectorProvider, error) {
	provider, err := h.loadReadyConnectorProvider(ctx, h.watchNamespace, h.githubSignIn.Provider)
	if err != nil {
		return nil, err
	}
	if provider.Spec.AgentHost == nil {
		return nil, fiber.NewError(fiber.StatusServiceUnavailable, "the GitHub sign-in provider does not serve agent hosts")
	}
	// The controller provisions no host while two providers in a namespace
	// serve agent hosts, so sign-in would promise one that never comes.
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

// GetAuthConfig serves GET /api/v1/auth/config, unauthenticated.
func (h *Handlers) GetAuthConfig(c fiber.Ctx) error {
	response := AuthConfigResponse{OIDC: h.oidcEnabled}
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
	// ExpiresIn is the access token's lifetime in seconds, as GitHub gave it.
	ExpiresIn int  `json:"expiresIn"`
	NoHost    bool `json:"noHost"`
}

// SessionResponse is a new session token, shown once.
type SessionResponse struct {
	Token      string    `json:"token"`
	ExpiresAt  time.Time `json:"expiresAt"`
	Login      string    `json:"login"`
	Namespace  string    `json:"namespace"`
	Connection string    `json:"connection,omitempty"`
	// AgentHost says Orka provisions an agent host for this link.
	AgentHost bool `json:"agentHost"`
}

// SignInWithGitHub serves POST /api/v1/auth/github. It is unauthenticated:
// the GitHub token is the credential. Orka checks the token was issued to its
// own app and that its person is in an allowed org or team, links their
// account, and issues a session.
func (h *Handlers) SignInWithGitHub(c fiber.Ctx) error {
	cfg := h.githubSignIn
	if !cfg.Enabled || cfg.Sessions == nil || !h.connectors.Enabled {
		return fiber.NewError(fiber.StatusNotImplemented, "GitHub sign-in is not enabled on this controller")
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
	if strings.TrimSpace(req.RefreshToken) != "" {
		// The refresh token arrives unchecked beside the access token. Using
		// it once proves it is the same person's, and custody then holds only
		// what GitHub issued from it.
		refreshed, err := h.connectors.OAuth.Refresh(ctx, oauth, req.RefreshToken)
		if err != nil {
			return fiber.NewError(fiber.StatusUnauthorized, "GitHub did not accept the refresh token")
		}
		again, err := cfg.GitHub.CheckToken(ctx, oauth.ClientID, oauth.ClientSecret, refreshed.AccessToken)
		if err != nil || again.ID != user.ID {
			return fiber.NewError(fiber.StatusUnauthorized, "the GitHub access and refresh tokens belong to different accounts")
		}
		user = again
		tokens = githubauth.Tokens{AccessToken: refreshed.AccessToken, RefreshToken: refreshed.RefreshToken}
		if !refreshed.ExpiresAt.IsZero() {
			tokens.ExpiresIn = int(max(time.Until(refreshed.ExpiresAt), 0) / time.Second)
		}
	}
	// Only GitHub's verdict refuses the person; an answer it could not give
	// is retried. Either way the token this sign-in obtained goes unused.
	if err := cfg.GitHub.RequireMember(ctx, tokens.AccessToken, user.Login, cfg.Allowed); err != nil {
		h.revokeSignInToken(ctx, oauth, tokens.AccessToken)
		if errors.Is(err, githubauth.ErrNotMember) {
			return fiber.NewError(fiber.StatusForbidden, "you are not an active member of a GitHub org or team allowed to sign in")
		}
		return fiber.NewError(fiber.StatusServiceUnavailable, "could not check your GitHub membership; sign in again shortly")
	}
	ui := &UserInfo{
		AuthType: AuthTypeGitHubSession, Issuer: githubauth.Issuer, Subject: strconv.FormatInt(user.ID, 10),
		Username: user.Login, Namespace: h.watchNamespace,
	}
	declined := req.NoHost || !cfg.CreateHosts
	connection, held, err := h.linkGitHubSignIn(ctx, ui, provider, oauth, tokens, declined)
	if err != nil {
		if !held {
			h.revokeSignInToken(ctx, oauth, tokens.AccessToken)
		}
		return err
	}
	response, err := h.issueSession(ctx, ui)
	if err != nil {
		return err
	}
	response.Connection = connection.Name
	response.AgentHost = provider.Spec.AgentHost != nil && !declined
	return c.Status(fiber.StatusCreated).JSON(response)
}

// RenewSession serves POST /api/v1/auth/session/renew. It refreshes the
// person's GitHub token from custody, checks their membership again, and
// replaces the session, which then ends.
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
	held, err := cfg.Credentials.HostCredential(ctx, h.watchNamespace, name, sessionRenewHorizon)
	if errors.Is(err, credential.ErrHostLinkUnavailable) {
		return fiber.NewError(fiber.StatusUnauthorized, "your GitHub link is gone; sign in again")
	}
	if err != nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "could not refresh your GitHub token; retry")
	}
	// The link must still hold this person's own grant: the session's
	// subject, not whoever the refreshed token belongs to, renews.
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
	token, next, err := h.newSession(ui)
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
		Token: token, ExpiresAt: next.ExpiresAt, Login: next.Username, Namespace: next.Namespace, Connection: name,
	})
}

// SignOut serves DELETE /api/v1/auth/session: it ends the caller's session.
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

// issueSession stores a new session for ui and returns its token, once.
func (h *Handlers) issueSession(ctx context.Context, ui *UserInfo) (SessionResponse, error) {
	token, session, err := h.newSession(ui)
	if err != nil {
		return SessionResponse{}, err
	}
	if err := h.githubSignIn.Sessions.CreateAuthSession(ctx, session); err != nil {
		return SessionResponse{}, fiber.NewError(fiber.StatusInternalServerError, "could not store the session")
	}
	return SessionResponse{Token: token, ExpiresAt: session.ExpiresAt, Login: session.Username, Namespace: session.Namespace}, nil
}

func (h *Handlers) newSession(ui *UserInfo) (string, store.AuthSession, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", store.AuthSession{}, fiber.NewError(fiber.StatusInternalServerError, "could not create a session")
	}
	token := sessionTokenPrefix + base64.RawURLEncoding.EncodeToString(secret)
	now := h.githubSignIn.now().UTC()
	return token, store.AuthSession{
		ID: uuid.NewString(), TokenHash: getTokenHash(token),
		Issuer: ui.Issuer, Subject: ui.Subject, Username: ui.Username, Namespace: ui.Namespace,
		CreatedAt: now, ExpiresAt: now.Add(SessionTTL),
	}, nil
}

// linkGitHubSignIn records the sign-in's GitHub tokens as the person's
// Connection to the provider. It takes the steps a browser consent takes, a
// consent, a completion and its commit, so custody's fences hold as they do
// for any link. held reports whether Orka may hold the tokens: when it is
// false the link stored nothing, and a failed caller revokes them.
func (h *Handlers) linkGitHubSignIn(ctx context.Context, ui *UserInfo, provider *corev1alpha1.ConnectorProvider,
	oauth connectors.OAuthProviderConfig, tokens githubauth.Tokens, declined bool) (connection *corev1alpha1.Connection, held bool, err error) {
	connection, err = h.ensureSignInConnection(ctx, ui, provider, declined)
	if err != nil {
		return nil, false, err
	}
	// Sign-ins for one Connection take turns, and each judges the link as
	// the one before it left it: a sign-in that waited reuses what that one
	// linked rather than commit a second grant.
	unlock := h.completionLocks.lock(string(connection.UID))
	defer unlock()
	if connection, err = h.reloadSignInConnection(ctx, connection, ui); err != nil {
		return nil, false, err
	}
	// Signing in again keeps a link that still holds the person's account:
	// each commit retains the grant it replaces until disconnect, so linking
	// every sign-in would soon refuse new ones. The unused token is revoked.
	if held, ok := h.signInLinkHolds(ctx, connection, oauth, ui); ok {
		if tokens.AccessToken != held {
			if err := h.githubSignIn.GitHub.DeleteToken(ctx, oauth.ClientID, oauth.ClientSecret, tokens.AccessToken); err != nil {
				log.Error(err, "could not revoke a sign-in's unused GitHub token", "connection", connection.Name)
			}
		}
		return connection, false, nil
	}
	mode, err := normalizeConnectionMode(connection.Spec.Mode)
	if err != nil {
		return nil, false, fiber.NewError(fiber.StatusConflict, "your link has an invalid mode")
	}

	now := h.connectors.now()
	consentNonce, err := connectors.GenerateStateNonce()
	if err != nil {
		return nil, false, fiber.NewError(fiber.StatusInternalServerError, "failed to link GitHub")
	}
	verifier, _, err := connectors.GeneratePKCE()
	if err != nil {
		return nil, false, fiber.NewError(fiber.StatusInternalServerError, "failed to link GitHub")
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
		return nil, false, fiber.NewError(fiber.StatusInternalServerError, "failed to record consent")
	}
	consent, err := h.connectors.Consents.ConsumeConnectorConsent(ctx, consentNonce)
	if err != nil {
		return nil, false, fiber.NewError(fiber.StatusInternalServerError, "failed to record consent")
	}
	expires := now.Add(maxGitHubAccessTokenLife)
	if tokens.ExpiresIn > 0 {
		expires = now.Add(min(time.Duration(tokens.ExpiresIn)*time.Second, maxGitHubAccessTokenLife))
	}
	completionNonce, err := connectors.GenerateStateNonce()
	if err != nil {
		return nil, false, fiber.NewError(fiber.StatusInternalServerError, "failed to link GitHub")
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
			return nil, false, fiber.NewError(fiber.StatusConflict, "your GitHub link is being disconnected; sign in again in a moment")
		}
		if errors.Is(err, store.ErrConnectorConsentSuperseded) {
			return nil, false, fiber.NewError(fiber.StatusConflict, "another sign-in is linking GitHub at the same time; retry")
		}
		// Whether the completion was stored is unknown.
		return nil, true, fiber.NewError(fiber.StatusInternalServerError, "failed to link GitHub")
	}
	ref, err := connectors.CredentialRef(connection)
	if err != nil {
		return nil, true, fiber.NewError(fiber.StatusConflict, "your link's identity is incomplete")
	}
	committed, err := h.connectors.Consents.CommitConnectorCompletion(ctx, completionNonce, ref, completion.Credential)
	if err != nil {
		return nil, true, h.completionCommitError(ctx, err, completion, completionNonce, connection.Name)
	}
	if err := h.markConnectionLinked(ctx, connection, provider, committed); err != nil {
		return nil, true, fiber.NewError(fiber.StatusInternalServerError, "failed to record your GitHub link; sign in again")
	}
	if err := h.connectors.Consents.DeleteConnectorCompletion(ctx, completionNonce); err != nil {
		log.Error(err, "consumed sign-in completion could not be removed", "connection", connection.Name)
	}
	return connection, true, nil
}

// revokeSignInToken revokes a token a sign-in obtained and did not store.
func (h *Handlers) revokeSignInToken(ctx context.Context, oauth connectors.OAuthProviderConfig, token string) {
	if err := h.githubSignIn.GitHub.DeleteToken(ctx, oauth.ClientID, oauth.ClientSecret, token); err != nil {
		log.Error(err, "could not revoke a sign-in's unused GitHub token")
	}
}

// signInLinkHolds returns the token custody holds for connection when it is
// linked and the token is still ui's GitHub account's. A token that never
// expires is not kept: membership is checked again only at refreshes, so the
// sign-in replaces it with one that ends.
func (h *Handlers) signInLinkHolds(ctx context.Context, connection *corev1alpha1.Connection,
	oauth connectors.OAuthProviderConfig, ui *UserInfo) (string, bool) {
	cfg := h.githubSignIn
	if cfg.Credentials == nil || !connectors.ConnectionLinked(connection) {
		return "", false
	}
	held, err := cfg.Credentials.HostCredential(ctx, connection.Namespace, connection.Name, sessionRenewHorizon)
	if err != nil || held.ExpiresAt.IsZero() {
		return "", false
	}
	account, err := cfg.GitHub.CheckToken(ctx, oauth.ClientID, oauth.ClientSecret, held.AccessToken)
	if err != nil || strconv.FormatInt(account.ID, 10) != ui.Subject {
		return "", false
	}
	return held.AccessToken, true
}

// reloadSignInConnection reads connection again, uncached, and requires it
// to be the same live, protected link of ui's.
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

// ensureSignInConnection returns the person's canonical Connection to
// provider, creating it as CreateConnection does, and records whether they
// declined an agent host.
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
		// A concurrent sign-in created it first; this one goes on under the
		// same checks. Read uncached: the informer may not have seen it yet.
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
	// Custody is written only under the finalizer that tombstones and
	// revokes it on delete; a Connection made elsewhere gets it when first
	// reconciled.
	if !connectionCustodyProtected(connection) {
		return nil, fiber.NewError(fiber.StatusConflict, "your GitHub link is still being set up; sign in again in a moment")
	}
	base := connection.DeepCopy()
	setAgentHostDeclined(connection, declined)
	if err := h.client.Patch(ctx, connection, client.MergeFrom(base)); err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, "failed to update your GitHub link")
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
