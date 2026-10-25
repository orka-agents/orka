/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/githubauth"
)

// Invite states. An invite ends Accepted, Refused or Expired, once.
const (
	InvitePending  = "Pending"
	InviteAccepted = "Accepted"
	InviteRefused  = "Refused"
	InviteExpired  = "Expired"
)

// maxPendingInvites bounds the invites a controller polls GitHub for at once.
const maxPendingInvites = 64

// inviteCompletionTimeout bounds checking and linking an approved invite:
// the code's expiry ends only the wait for approval.
const inviteCompletionTimeout = 2 * time.Minute

// InviteResponse is an invite and how it ended. It never carries tokens.
type InviteResponse struct {
	ID              string    `json:"id"`
	Login           string    `json:"login"`
	State           string    `json:"state"`
	Message         string    `json:"message,omitempty"`
	UserCode        string    `json:"userCode,omitempty"`
	VerificationURI string    `json:"verificationURI,omitempty"`
	ExpiresAt       time.Time `json:"expiresAt"`
	Connection      string    `json:"connection,omitempty"`
	// AgentHost says accepting starts an agent host whose code can read the
	// person's GitHub token, which the person must be told before approving.
	AgentHost bool `json:"agentHost"`
}

// githubInvites holds invites in memory: one lasts as long as its device
// code, and the API is served by one controller.
type githubInvites struct {
	mu      sync.Mutex
	invites map[string]*InviteResponse
}

func (i *githubInvites) get(id string) (InviteResponse, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	invite, ok := i.invites[id]
	if !ok {
		return InviteResponse{}, false
	}
	return *invite, true
}

func (i *githubInvites) add(invite *InviteResponse) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	pending := 0
	for id, held := range i.invites {
		switch {
		case held.State == InvitePending:
			pending++
		case time.Since(held.ExpiresAt) > time.Hour:
			delete(i.invites, id)
		}
	}
	if pending >= maxPendingInvites {
		return errors.New("too many invites are pending")
	}
	if i.invites == nil {
		i.invites = map[string]*InviteResponse{}
	}
	i.invites[invite.ID] = invite
	return nil
}

// started fills in a reserved invite once GitHub's device flow started and
// returns a copy to answer with.
func (i *githubInvites) started(id string, code githubauth.DeviceCode, expires time.Time, agentHost bool) InviteResponse {
	i.mu.Lock()
	defer i.mu.Unlock()
	invite := i.invites[id]
	invite.UserCode, invite.VerificationURI, invite.ExpiresAt, invite.AgentHost = code.UserCode, code.VerificationURI, expires, agentHost
	return *invite
}

// drop removes an invite whose device flow never started.
func (i *githubInvites) drop(id string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.invites, id)
}

func (i *githubInvites) finish(id, state, message, connection string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if invite, ok := i.invites[id]; ok && invite.State == InvitePending {
		invite.State, invite.Message, invite.Connection = state, message, connection
		invite.UserCode, invite.VerificationURI = "", ""
	}
}

type createInviteRequest struct {
	Login string `json:"login"`
}

// CreateInvite serves POST /api/v1/agent-hosts/invites. An operator with
// create on agenthosts invites a GitHub login: Orka starts GitHub's device
// flow itself, returns the code to pass on, and once the invited person
// approves it makes the sign-in checks and links their account, which starts
// their host. The person needs only a browser and the Copilot app.
func (h *Handlers) CreateInvite(c fiber.Ctx) error {
	ui := GetUserInfo(c)
	// The route's policy checked create on agenthosts with a
	// SubjectAccessReview; only Kubernetes identities have that answer.
	if ui == nil || ui.AuthType != AuthTypeTokenReview {
		return fiber.NewError(fiber.StatusForbidden, "inviting needs Kubernetes credentials allowed to create agenthosts")
	}
	cfg := h.githubSignIn
	if !cfg.Enabled || !h.connectors.Enabled {
		return fiber.NewError(fiber.StatusNotImplemented, "GitHub sign-in is not enabled on this controller")
	}
	// An invite gives someone who cannot reach Orka a Copilot environment;
	// a link alone would leave them nothing to use.
	if !cfg.CreateHosts {
		return fiber.NewError(fiber.StatusNotImplemented, "this controller starts no agent hosts for sign-ins (--github-sign-in-create-hosts=false), so it has nothing to invite to")
	}
	var req createInviteRequest
	if err := decodeStrictJSON(c.Body(), &req); err != nil {
		return err
	}
	login := strings.TrimSpace(req.Login)
	if login == "" || len(login) > 39 || strings.ContainsAny(login, " /@") {
		return fiber.NewError(fiber.StatusBadRequest, "login must be a GitHub login")
	}
	ctx := c.Context()
	provider, err := h.signInProvider(ctx)
	if err != nil {
		return err
	}
	// The app's credentials are read before GitHub issues anything, so a
	// refusal can always revoke what it issued.
	created, err := h.providerOAuthConfig(ctx, provider)
	if err != nil {
		return fiber.NewError(fiber.StatusServiceUnavailable, "the GitHub App is not configured")
	}
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "could not create the invite")
	}
	id := hex.EncodeToString(idBytes)
	// The slot is reserved before GitHub is asked for a code, so a full
	// table never starts device flows it cannot track.
	if err := h.invites.add(&InviteResponse{ID: id, Login: login, State: InvitePending, ExpiresAt: cfg.now().Add(time.Hour).UTC()}); err != nil {
		return fiber.NewError(fiber.StatusTooManyRequests, err.Error())
	}
	code, err := cfg.GitHub.StartDeviceFlow(ctx, provider.Spec.OAuth.ClientID)
	if err != nil {
		h.invites.drop(id)
		return fiber.NewError(fiber.StatusBadGateway, "could not start GitHub's device flow")
	}
	expires := cfg.now().Add(time.Duration(max(code.ExpiresIn, 60)) * time.Second).UTC()
	response := h.invites.started(id, code, expires, provider.Spec.AgentHost != nil && cfg.CreateHosts)
	go h.completeInvite(id, login, provider.DeepCopy(), created, code, expires)
	return c.Status(fiber.StatusCreated).JSON(response)
}

// GetInvite serves GET /api/v1/agent-hosts/invites/:id.
func (h *Handlers) GetInvite(c fiber.Ctx) error {
	if ui := GetUserInfo(c); ui == nil || ui.AuthType != AuthTypeTokenReview {
		return fiber.NewError(fiber.StatusForbidden, "invites need Kubernetes credentials allowed to create agenthosts")
	}
	invite, ok := h.invites.get(c.Params("id"))
	if !ok {
		return fiber.NewError(fiber.StatusNotFound, "no such invite")
	}
	return c.JSON(invite)
}

// completeInvite polls GitHub until the invited person approves the code or
// it expires, then makes the sign-in checks and links their account. The
// provider is the one the invite was created and disclosed for, and created
// its app's credentials then.
func (h *Handlers) completeInvite(id, login string, provider *corev1alpha1.ConnectorProvider, created connectors.OAuthProviderConfig,
	code githubauth.DeviceCode, expires time.Time) {
	waiting, stopWaiting := context.WithDeadline(context.Background(), expires)
	defer stopWaiting()
	cfg := h.githubSignIn
	tokens, err := cfg.GitHub.PollDeviceFlow(waiting, provider.Spec.OAuth.ClientID, code)
	if errors.Is(err, githubauth.ErrDeviceFlowDenied) || errors.Is(err, context.DeadlineExceeded) {
		h.invites.finish(id, InviteExpired, "the code was not approved in time, or was declined", "")
		return
	}
	if err != nil {
		h.invites.finish(id, InviteRefused, "GitHub's device flow failed; invite again", "")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), inviteCompletionTimeout)
	defer cancel()
	// The app's credentials are read again, as a rotated secret would be;
	// when they cannot be, the ones read at creation revoke the token.
	oauth, oauthErr := h.providerOAuthConfig(ctx, provider)
	if oauthErr != nil {
		oauth = created
	}
	// Tokens obtained for someone other than the invited login, from a
	// person who may not sign in, or left unused, are revoked with the app's
	// credentials. The outcome says whether GitHub confirmed it.
	revoke := func(tokens ...string) (note string, failed bool) {
		revokeCtx, cancelRevocation := context.WithTimeout(context.Background(), inviteCompletionTimeout)
		defer cancelRevocation()
		revocation := h.inviteRevocationConfig(revokeCtx, provider, created.ClientID, oauth)
		attempted := false
		for _, token := range tokens {
			if token == "" {
				continue
			}
			attempted = true
			err := cfg.GitHub.DeleteToken(revokeCtx, revocation.ClientID, revocation.ClientSecret, token)
			// A secret changed while the invite was pending need not be the
			// one that issued the token; the one read at creation is tried
			// too.
			if err != nil && (created.ClientID != revocation.ClientID || created.ClientSecret != revocation.ClientSecret) {
				err = cfg.GitHub.DeleteToken(revokeCtx, created.ClientID, created.ClientSecret, token)
			}
			if err != nil {
				log.Error(err, "could not revoke an invite's GitHub token", "invite", id)
				failed = true
			}
		}
		switch {
		case failed:
			return "; revoking the approved GitHub token failed, so it may still be active", true
		case attempted:
			return "; the approved GitHub token was revoked", false
		}
		return "", false
	}
	refuse := func(message string, tokens ...string) {
		note, _ := revoke(tokens...)
		h.invites.finish(id, InviteRefused, message+note, "")
	}
	if oauthErr != nil {
		refuse("the GitHub App is not configured", tokens.AccessToken)
		return
	}
	// The person approved what the invite disclosed: a provider replaced or
	// changed meanwhile, its agent host or scopes included, needs a new
	// invite. The token is revoked with the app that issued it.
	current, err := h.signInProvider(ctx)
	if err != nil || current.UID != provider.UID || current.Generation != provider.Generation ||
		connectors.ProviderAuthorityDigest(current) != connectors.ProviderAuthorityDigest(provider) {
		// Keep the freshly read secret for the issuing app; revoke falls
		// back to the creation credentials if the current ones do not work.
		refuse("the GitHub provider changed while the invite was pending", tokens.AccessToken)
		return
	}
	user, tokens, refusal, unused := h.inviteIdentity(ctx, oauth, login, tokens)
	if refusal != "" {
		refuse(refusal, unused)
		return
	}
	if err := cfg.GitHub.RequireMember(ctx, tokens.AccessToken, user.Login, cfg.Allowed); err != nil {
		if errors.Is(err, githubauth.ErrNotMember) {
			refuse("the invited account is not an active member of a GitHub org or team allowed to sign in", tokens.AccessToken)
		} else {
			refuse("GitHub could not confirm the invited account's membership; invite again shortly", tokens.AccessToken)
		}
		return
	}
	// An invite gives a host, and a host takes only a token that expires:
	// one GitHub issued a refresh token beside, which it does only for a
	// token that expires.
	if tokens.ExpiresIn <= 0 || strings.TrimSpace(tokens.RefreshToken) == "" {
		refuse("GitHub gave the approved token no expiry or refresh token; turn on user-token expiration in the GitHub App, then invite again", tokens.AccessToken)
		return
	}
	ui := &UserInfo{
		AuthType: AuthTypeGitHubSession, Issuer: githubauth.Issuer, Subject: strconv.FormatInt(user.ID, 10),
		Username: user.Login, Namespace: h.watchNamespace,
	}
	connection, held, err := h.linkGitHubSignIn(ctx, ui, provider, oauth, tokens, !cfg.CreateHosts)
	if errors.Is(err, errSignInLinkUnrecorded) {
		// Custody holds the grant; the Connection reconciler records the link.
		h.invites.finish(id, InviteAccepted, inviteAcceptedMessage(provider, cfg.CreateHosts)+" once Orka finishes recording the link", connection.Name)
		return
	}
	if err != nil && !held {
		// Nothing stored the tokens, so nothing else will revoke them.
		refuse("the GitHub account could not be linked", tokens.AccessToken)
		return
	}
	if err != nil {
		h.invites.finish(id, InviteRefused, "the GitHub account could not be linked; invite again", "")
		return
	}
	message := inviteAcceptedMessage(provider, cfg.CreateHosts)
	// A link that already held the person's account was kept, and the
	// approved token went unused.
	if !held {
		if note, failed := revoke(tokens.AccessToken); failed {
			message += "; the approved GitHub token was not needed" + note
		}
	}
	h.invites.finish(id, InviteAccepted, message, connection.Name)
}

// inviteRevocationConfig follows a rotated Secret reference only for cleanup.
// The provider need not be ready, but must still identify the issuing app.
// The candidate cannot authorize the changed invite.
func (h *Handlers) inviteRevocationConfig(ctx context.Context, provider *corev1alpha1.ConnectorProvider,
	issuingClientID string, fallback connectors.OAuthProviderConfig) connectors.OAuthProviderConfig {
	revocation := fallback
	current := &corev1alpha1.ConnectorProvider{}
	if err := h.providerReader().Get(ctx, client.ObjectKeyFromObject(provider), current); err == nil &&
		current.Spec.OAuth.ClientID == issuingClientID {
		if latest, err := h.providerOAuthConfig(ctx, current); err == nil {
			revocation = latest
		}
	}
	return revocation
}

// inviteIdentity checks that the approved tokens are the invited login's and,
// given a refresh token, redeems it once, so custody holds what GitHub issued
// from it. It returns the account and tokens to link, or why it refuses and
// the token to revoke.
func (h *Handlers) inviteIdentity(ctx context.Context, oauth connectors.OAuthProviderConfig, login string,
	tokens githubauth.Tokens) (user githubauth.User, linked githubauth.Tokens, refusal, unused string) {
	github := h.githubSignIn.GitHub
	user, err := github.CheckToken(ctx, oauth.ClientID, oauth.ClientSecret, tokens.AccessToken)
	if err != nil {
		return user, tokens, "the approved GitHub token could not be checked", tokens.AccessToken
	}
	if !strings.EqualFold(user.Login, login) {
		return user, tokens, "the code was approved by another GitHub account", tokens.AccessToken
	}
	if strings.TrimSpace(tokens.RefreshToken) == "" {
		return user, tokens, "", ""
	}
	refreshed, err := h.connectors.OAuth.Refresh(ctx, oauth, tokens.RefreshToken)
	if err != nil {
		return user, tokens, "GitHub did not accept the refresh token", tokens.AccessToken
	}
	again, err := github.CheckToken(ctx, oauth.ClientID, oauth.ClientSecret, refreshed.AccessToken)
	if err != nil || again.ID != user.ID {
		return user, tokens, "GitHub issued tokens for another account", refreshed.AccessToken
	}
	// An account renamed between the checks is no longer the invited login,
	// and membership is read for the login GitHub gives now.
	if !strings.EqualFold(again.Login, login) {
		return user, tokens, "the code was approved by another GitHub account", refreshed.AccessToken
	}
	linked = githubauth.Tokens{AccessToken: refreshed.AccessToken, RefreshToken: refreshed.RefreshToken}
	if !refreshed.ExpiresAt.IsZero() {
		linked.ExpiresIn = int(max(time.Until(refreshed.ExpiresAt), 0) / time.Second)
	}
	return again, linked, "", ""
}

func inviteAcceptedMessage(provider *corev1alpha1.ConnectorProvider, createHosts bool) string {
	if provider.Spec.AgentHost == nil || !createHosts {
		return "linked; this Orka starts no agent host for it"
	}
	return "linked; Orka is starting their Copilot environment"
}
