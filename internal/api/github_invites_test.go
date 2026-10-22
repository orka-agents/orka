/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/githubauth"
)

// inviteGitHub serves the device flow, approving with approved, and records
// the tokens the app revokes; check-token and membership are fakeGitHub's.
type inviteGitHub struct {
	mu       sync.Mutex
	approved map[string]any
	revoked  *revocations
	// onApprove runs as GitHub hands out the approved tokens.
	onApprove func()
}

func newInviteHarness(t *testing.T, approved map[string]any) (*connectorTestHarness, *inviteGitHub) {
	t.Helper()
	h := newSignInHarness(t)
	gh := &inviteGitHub{approved: approved, revoked: &revocations{}}
	api := fakeGitHub(t, gh.revoked)
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/login/device/code":
			_ = json.NewEncoder(w).Encode(githubauth.DeviceCode{DeviceCode: "dev", UserCode: "ABCD-1234", VerificationURI: "https://github.com/login/device", ExpiresIn: 900})
		case "/login/oauth/access_token":
			gh.mu.Lock()
			defer gh.mu.Unlock()
			if gh.onApprove != nil {
				gh.onApprove()
			}
			_ = json.NewEncoder(w).Encode(gh.approved)
		}
	}))
	t.Cleanup(web.Close)
	h.handlers.githubSignIn.GitHub = githubauth.Client{WebURL: web.URL, APIURL: api.URL, MinPollInterval: time.Millisecond}
	h.app.Post("/api/v1/agent-hosts/invites", h.handlers.CreateInvite)
	h.app.Get("/api/v1/agent-hosts/invites/:id", h.handlers.GetInvite)
	h.identity = &UserInfo{AuthType: AuthTypeTokenReview, Username: "system:serviceaccount:orka-system:operator", Namespace: connectorTestNamespace}
	return h, gh
}

func (h *connectorTestHarness) invite(login string) (int, InviteResponse) {
	h.t.Helper()
	raw, _ := json.Marshal(map[string]string{"login": login})
	request := httptest.NewRequest(http.MethodPost, "/api/v1/agent-hosts/invites", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	resp, err := h.app.Test(request)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var invite InviteResponse
	_ = json.NewDecoder(resp.Body).Decode(&invite)
	return resp.StatusCode, invite
}

// inviteOutcome waits for the invite to end and returns it.
func (h *connectorTestHarness) inviteOutcome(id string) InviteResponse {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if invite, ok := h.handlers.invites.get(id); ok && invite.State != InvitePending {
			return invite
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatal("the invite never ended")
	return InviteResponse{}
}

func TestInviteLinksTheInvitedPerson(t *testing.T) {
	h, gh := newInviteHarness(t, map[string]any{"access_token": "ghu_alice", "refresh_token": "ghr_alice", "expires_in": 28800})
	status, invite := h.invite("Alice")
	if status != http.StatusCreated || invite.UserCode != "ABCD-1234" || invite.State != InvitePending || !invite.AgentHost {
		t.Fatalf("invite: %d %+v", status, invite)
	}
	outcome := h.inviteOutcome(invite.ID)
	wantName := connectors.ConnectionName("github", githubauth.Issuer, signInTestUserID)
	if outcome.State != InviteAccepted || outcome.Connection != wantName || outcome.UserCode != "" {
		t.Fatalf("outcome = %+v", outcome)
	}
	connection := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: wantName}, connection); err != nil {
		t.Fatal(err)
	}
	if connection.Spec.Subject.Subject != signInTestUserID || connection.Status.GrantSequence == 0 {
		t.Fatalf("connection = %+v", connection.Spec)
	}
	if len(gh.revoked.list()) != 0 {
		t.Fatalf("an accepted invite revokes nothing: %v", gh.revoked.list())
	}
	// The outcome reads back through the API.
	resp, raw := h.do(http.MethodGet, "/api/v1/agent-hosts/invites/"+invite.ID, nil)
	var read InviteResponse
	if err := json.Unmarshal(raw, &read); err != nil || resp.StatusCode != http.StatusOK || read.State != InviteAccepted {
		t.Fatalf("get: %d %s", resp.StatusCode, raw)
	}
}

func TestInviteRefusesSomeoneElseOrAnOutsider(t *testing.T) {
	for name, test := range map[string]struct {
		login    string
		approved map[string]any
		revoked  string
	}{
		"another account approved the code": {login: "carol", approved: map[string]any{"access_token": "ghu_alice"}, revoked: "ghu_alice"},
		"the invited person is outside the org": {login: "bob", approved: map[string]any{"access_token": "ghu_bob", "refresh_token": "ghr_bob"},
			revoked: "ghu_bob_refreshed"},
		"GitHub cannot check the approved token": {login: "alice", approved: map[string]any{"access_token": "ghu_unknown"}, revoked: "ghu_unknown"},
	} {
		t.Run(name, func(t *testing.T) {
			h, gh := newInviteHarness(t, test.approved)
			_, invite := h.invite(test.login)
			if outcome := h.inviteOutcome(invite.ID); outcome.State != InviteRefused || outcome.Connection != "" {
				t.Fatalf("outcome = %+v", outcome)
			}
			gh.mu.Lock()
			defer gh.mu.Unlock()
			if revoked := gh.revoked.list(); len(revoked) != 1 || revoked[0] != test.revoked {
				t.Fatalf("revoked = %v, want [%s]", revoked, test.revoked)
			}
			connections := &corev1alpha1.ConnectionList{}
			if err := h.client.List(context.Background(), connections); err != nil {
				t.Fatal(err)
			}
			if len(connections.Items) != 0 {
				t.Fatal("a refused invite links no one")
			}
		})
	}
}

// A provider changed while the code was pending, its agent host included,
// is not what the person was told about: the invite is refused and the
// token revoked with the app that issued it.
func TestInviteRefusesAProviderChangedWhilePending(t *testing.T) {
	h, gh := newInviteHarness(t, map[string]any{"access_token": "ghu_alice", "refresh_token": "ghr_alice", "expires_in": 28800})
	gh.onApprove = func() {
		provider := &corev1alpha1.ConnectorProvider{}
		if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: "github"}, provider); err != nil {
			t.Error(err)
			return
		}
		provider.Spec.AgentHost.TemplateRef.Name = "another-template"
		if err := h.client.Update(context.Background(), provider); err != nil {
			t.Error(err)
		}
	}
	_, invite := h.invite("alice")
	if outcome := h.inviteOutcome(invite.ID); outcome.State != InviteRefused || outcome.Connection != "" {
		t.Fatalf("outcome = %+v", outcome)
	}
	if revoked := gh.revoked.list(); len(revoked) != 1 || revoked[0] != "ghu_alice" {
		t.Fatalf("revoked = %v, want the approved token", revoked)
	}
}

// Custody committed with a status write that failed is an accepted invite:
// the Connection reconciler records the link from the committed completion.
func TestInviteAcceptsALinkCommittedButNotYetRecorded(t *testing.T) {
	h, gh := newInviteHarness(t, map[string]any{"access_token": "ghu_alice", "refresh_token": "ghr_alice", "expires_in": 28800})
	h.statusFailure = &atomic.Bool{}
	h.statusFailure.Store(true)
	_, invite := h.invite("alice")
	outcome := h.inviteOutcome(invite.ID)
	wantName := connectors.ConnectionName("github", githubauth.Issuer, signInTestUserID)
	if outcome.State != InviteAccepted || outcome.Connection != wantName || !strings.Contains(outcome.Message, "finishes recording") {
		t.Fatalf("outcome = %+v", outcome)
	}
	if revoked := gh.revoked.list(); len(revoked) != 0 {
		t.Fatalf("a committed link revokes nothing: %v", revoked)
	}
}

// Only Kubernetes identities, whose create on agenthosts the route's policy
// checked, invite; a person's own session or OIDC sign-in does not.
func TestInvitesNeedKubernetesCredentials(t *testing.T) {
	h, _ := newInviteHarness(t, nil)
	for _, identity := range []*UserInfo{
		{AuthType: AuthTypeOIDC, Issuer: connectorTestIssuer, Subject: "alice", Namespace: connectorTestNamespace},
		{AuthType: AuthTypeGitHubSession, Issuer: githubauth.Issuer, Subject: signInTestUserID, Namespace: connectorTestNamespace},
	} {
		h.identity = identity
		if status, _ := h.invite("alice"); status != http.StatusForbidden {
			t.Fatalf("%s: %d, want 403", identity.AuthType, status)
		}
	}
}
