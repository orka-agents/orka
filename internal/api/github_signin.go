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
	// GitHub token: GitHub App user tokens last eight hours.
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

// GetAuthConfig serves GET /api/v1/auth/config, unauthenticated.
func (h *Handlers) GetAuthConfig(c fiber.Ctx) error {
	response := AuthConfigResponse{OIDC: h.oidcEnabled}
	if cfg := h.githubSignIn; cfg.Enabled {
		provider, err := h.loadReadyConnectorProvider(c.Context(), h.watchNamespace, cfg.Provider)
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
	provider, err := h.loadReadyConnectorProvider(ctx, h.watchNamespace, cfg.Provider)
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
	if err := cfg.GitHub.RequireMember(ctx, req.AccessToken, user.Login, cfg.Allowed); err != nil {
		return fiber.NewError(fiber.StatusForbidden, "you are not an active member of a GitHub org or team allowed to sign in")
	}
	ui := &UserInfo{
		AuthType: AuthTypeGitHubSession, Issuer: githubauth.Issuer, Subject: strconv.FormatInt(user.ID, 10),
		Username: user.Login, Namespace: h.watchNamespace,
	}
	declined := req.NoHost || !cfg.CreateHosts
	tokens := githubauth.Tokens{AccessToken: req.AccessToken, RefreshToken: req.RefreshToken, ExpiresIn: req.ExpiresIn}
	connection, err := h.linkGitHubSignIn(ctx, ui, provider, tokens, declined)
	if err != nil {
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
	if err := cfg.GitHub.RequireMember(ctx, held.AccessToken, ui.Username, cfg.Allowed); err != nil {
		return fiber.NewError(fiber.StatusForbidden, "you are no longer an active member of a GitHub org or team allowed to sign in")
	}
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
// for any link.
func (h *Handlers) linkGitHubSignIn(ctx context.Context, ui *UserInfo, provider *corev1alpha1.ConnectorProvider,
	tokens githubauth.Tokens, declined bool) (*corev1alpha1.Connection, error) {
	connection, err := h.ensureSignInConnection(ctx, ui, provider, declined)
	if err != nil {
		return nil, err
	}
	mode, err := normalizeConnectionMode(connection.Spec.Mode)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusConflict, "your link has an invalid mode")
	}
	unlock := h.completionLocks.lock(string(connection.UID))
	defer unlock()

	now := h.connectors.now()
	consentNonce, err := connectors.GenerateStateNonce()
	if err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, "failed to link GitHub")
	}
	verifier, _, err := connectors.GeneratePKCE()
	if err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, "failed to link GitHub")
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
		return nil, fiber.NewError(fiber.StatusInternalServerError, "failed to record consent")
	}
	consent, err := h.connectors.Consents.ConsumeConnectorConsent(ctx, consentNonce)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, "failed to record consent")
	}
	var expires time.Time
	if tokens.ExpiresIn > 0 {
		expires = now.Add(min(time.Duration(tokens.ExpiresIn)*time.Second, maxGitHubAccessTokenLife))
	}
	completionNonce, err := connectors.GenerateStateNonce()
	if err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, "failed to link GitHub")
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
			return nil, fiber.NewError(fiber.StatusConflict, "your GitHub link is being disconnected; sign in again in a moment")
		}
		if errors.Is(err, store.ErrConnectorConsentSuperseded) {
			return nil, fiber.NewError(fiber.StatusConflict, "another sign-in is linking GitHub at the same time; retry")
		}
		return nil, fiber.NewError(fiber.StatusInternalServerError, "failed to link GitHub")
	}
	ref, err := connectors.CredentialRef(connection)
	if err != nil {
		return nil, fiber.NewError(fiber.StatusConflict, "your link's identity is incomplete")
	}
	committed, err := h.connectors.Consents.CommitConnectorCompletion(ctx, completionNonce, ref, completion.Credential)
	if err != nil {
		return nil, h.completionCommitError(ctx, err, completion, completionNonce, connection.Name)
	}
	if err := h.markConnectionLinked(ctx, connection, provider, committed); err != nil {
		return nil, fiber.NewError(fiber.StatusInternalServerError, "failed to record your GitHub link; sign in again")
	}
	if err := h.connectors.Consents.DeleteConnectorCompletion(ctx, completionNonce); err != nil {
		log.Error(err, "consumed sign-in completion could not be removed", "connection", connection.Name)
	}
	return connection, nil
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
		if err := h.client.Create(ctx, connection); err != nil {
			return nil, fiber.NewError(fiber.StatusInternalServerError, "failed to create your GitHub link")
		}
		return connection, nil
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
