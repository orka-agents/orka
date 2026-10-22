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

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
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
	var req createInviteRequest
	if err := decodeStrictJSON(c.Body(), &req); err != nil {
		return err
	}
	login := strings.TrimSpace(req.Login)
	if login == "" || len(login) > 39 || strings.ContainsAny(login, " /@") {
		return fiber.NewError(fiber.StatusBadRequest, "login must be a GitHub login")
	}
	ctx := c.Context()
	provider, err := h.loadReadyConnectorProvider(ctx, h.watchNamespace, cfg.Provider)
	if err != nil {
		return err
	}
	code, err := cfg.GitHub.StartDeviceFlow(ctx, provider.Spec.OAuth.ClientID)
	if err != nil {
		return fiber.NewError(fiber.StatusBadGateway, "could not start GitHub's device flow")
	}
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "could not create the invite")
	}
	invite := &InviteResponse{
		ID: hex.EncodeToString(idBytes), Login: login, State: InvitePending,
		UserCode: code.UserCode, VerificationURI: code.VerificationURI,
		ExpiresAt: cfg.now().Add(time.Duration(max(code.ExpiresIn, 60)) * time.Second).UTC(),
		AgentHost: provider.Spec.AgentHost != nil && cfg.CreateHosts,
	}
	if err := h.invites.add(invite); err != nil {
		return fiber.NewError(fiber.StatusTooManyRequests, err.Error())
	}
	response := *invite
	go h.completeInvite(invite.ID, login, provider.Name, code, invite.ExpiresAt)
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
// it expires, then makes the sign-in checks and links their account.
func (h *Handlers) completeInvite(id, login, providerName string, code githubauth.DeviceCode, expires time.Time) {
	waiting, stopWaiting := context.WithDeadline(context.Background(), expires)
	defer stopWaiting()
	cfg := h.githubSignIn
	provider, err := h.loadReadyConnectorProvider(waiting, h.watchNamespace, providerName)
	if err != nil {
		h.invites.finish(id, InviteRefused, "the GitHub provider is no longer ready", "")
		return
	}
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
	oauth, err := h.providerOAuthConfig(ctx, provider)
	if err != nil {
		h.invites.finish(id, InviteRefused, "the GitHub App is not configured", "")
		return
	}
	// Tokens obtained for someone other than the invited login, or from a
	// person who may not sign in, are revoked with the app's credentials.
	refuse := func(message string, revoke ...string) {
		for _, token := range revoke {
			if token != "" {
				if err := cfg.GitHub.DeleteToken(context.Background(), oauth.ClientID, oauth.ClientSecret, token); err != nil {
					log.Error(err, "could not revoke an invite's GitHub token", "invite", id)
				}
			}
		}
		h.invites.finish(id, InviteRefused, message, "")
	}
	user, err := cfg.GitHub.CheckToken(ctx, oauth.ClientID, oauth.ClientSecret, tokens.AccessToken)
	if err != nil {
		refuse("the approved GitHub token could not be checked; it was revoked", tokens.AccessToken)
		return
	}
	if !strings.EqualFold(user.Login, login) {
		refuse("the code was approved by another GitHub account; it was refused and its token revoked", tokens.AccessToken)
		return
	}
	if strings.TrimSpace(tokens.RefreshToken) != "" {
		refreshed, err := h.connectors.OAuth.Refresh(ctx, oauth, tokens.RefreshToken)
		if err != nil {
			refuse("GitHub did not accept the refresh token", tokens.AccessToken)
			return
		}
		again, err := cfg.GitHub.CheckToken(ctx, oauth.ClientID, oauth.ClientSecret, refreshed.AccessToken)
		if err != nil || again.ID != user.ID {
			refuse("GitHub issued tokens for another account", refreshed.AccessToken)
			return
		}
		tokens = githubauth.Tokens{AccessToken: refreshed.AccessToken, RefreshToken: refreshed.RefreshToken}
		if !refreshed.ExpiresAt.IsZero() {
			tokens.ExpiresIn = int(max(time.Until(refreshed.ExpiresAt), 0) / time.Second)
		}
	}
	if err := cfg.GitHub.RequireMember(ctx, tokens.AccessToken, user.Login, cfg.Allowed); err != nil {
		refuse("the invited account is not an active member of a GitHub org or team allowed to sign in", tokens.AccessToken)
		return
	}
	ui := &UserInfo{
		AuthType: AuthTypeGitHubSession, Issuer: githubauth.Issuer, Subject: strconv.FormatInt(user.ID, 10),
		Username: user.Login, Namespace: h.watchNamespace,
	}
	connection, err := h.linkGitHubSignIn(ctx, ui, provider, oauth, tokens, !cfg.CreateHosts)
	if err != nil {
		h.invites.finish(id, InviteRefused, "the GitHub account could not be linked; invite again", "")
		return
	}
	h.invites.finish(id, InviteAccepted, inviteAcceptedMessage(provider, cfg.CreateHosts), connection.Name)
}

func inviteAcceptedMessage(provider *corev1alpha1.ConnectorProvider, createHosts bool) string {
	if provider.Spec.AgentHost == nil || !createHosts {
		return "linked; this Orka starts no agent host for it"
	}
	return "linked; Orka is starting their Copilot environment"
}
