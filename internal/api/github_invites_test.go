/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/controller"
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
	// codes counts the device flows started.
	codes atomic.Int32
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
			gh.codes.Add(1)
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
		"GitHub cannot answer the membership check": {login: "alice", approved: map[string]any{"access_token": "ghu_alice_unanswered"},
			revoked: "ghu_alice_unanswered"},
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

// A change to the provider's scopes alone, outside its authority digest,
// also refuses the invite: custody would record scopes the host path no
// longer accepts.
func TestInviteRefusesAScopeChangeWhilePending(t *testing.T) {
	h, gh := newInviteHarness(t, map[string]any{"access_token": "ghu_alice", "refresh_token": "ghr_alice", "expires_in": 28800})
	gh.onApprove = func() {
		provider := &corev1alpha1.ConnectorProvider{}
		if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: "github"}, provider); err != nil {
			t.Error(err)
			return
		}
		provider.Spec.OAuth.Scopes.Read = append(provider.Spec.OAuth.Scopes.Read, "read:org")
		provider.Generation++
		if err := h.client.Update(context.Background(), provider); err != nil {
			t.Error(err)
			return
		}
		// The provider is ready again by the time the invite checks it.
		for i := range provider.Status.Conditions {
			provider.Status.Conditions[i].ObservedGeneration = provider.Generation
		}
		if err := h.client.Status().Update(context.Background(), provider); err != nil {
			t.Error(err)
		}
	}
	_, invite := h.invite("alice")
	if outcome := h.inviteOutcome(invite.ID); outcome.State != InviteRefused {
		t.Fatalf("outcome = %+v", outcome)
	}
	if revoked := gh.revoked.list(); len(revoked) != 1 || revoked[0] != "ghu_alice" {
		t.Fatalf("revoked = %v, want the approved token", revoked)
	}
}

// An app Secret that cannot be read after approval still lets the invite
// revoke what GitHub issued, with the credentials read at creation.
func TestInviteRevokesWhenTheAppSecretIsGone(t *testing.T) {
	h, gh := newInviteHarness(t, map[string]any{"access_token": "ghu_alice", "refresh_token": "ghr_alice", "expires_in": 28800})
	gh.onApprove = func() {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-oauth", Namespace: connectorTestNamespace}}
		if err := h.client.Delete(context.Background(), secret); err != nil {
			t.Error(err)
		}
	}
	_, invite := h.invite("alice")
	if outcome := h.inviteOutcome(invite.ID); outcome.State != InviteRefused {
		t.Fatalf("outcome = %+v", outcome)
	}
	if revoked := gh.revoked.list(); len(revoked) != 1 || revoked[0] != "ghu_alice" {
		t.Fatalf("revoked = %v, want the approved token", revoked)
	}
}

// Without host creation an invite would only link an account its owner
// cannot use, so none is started.
func TestInvitesNeedHostCreation(t *testing.T) {
	h, gh := newInviteHarness(t, nil)
	h.handlers.githubSignIn.CreateHosts = false
	if status, _ := h.invite("alice"); status != http.StatusNotImplemented {
		t.Fatalf("invite without host creation = %d, want 501", status)
	}
	if got := gh.codes.Load(); got != 0 {
		t.Fatalf("%d device flows started", got)
	}
}

// A link that fails before storing anything revokes the token the invite
// would have linked, since nothing else holds it.
func TestInviteRevokesWhenLinkingStoresNothing(t *testing.T) {
	h, gh := newInviteHarness(t, map[string]any{"access_token": "ghu_alice", "refresh_token": "ghr_alice", "expires_in": 28800})
	name := connectors.ConnectionName("github", githubauth.Issuer, signInTestUserID)
	leaving := &corev1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: connectorTestNamespace, Finalizers: []string{"orka.ai/test-hold"}},
		Spec: corev1alpha1.ConnectionSpec{
			Subject:     corev1alpha1.ConnectionSubject{Issuer: githubauth.Issuer, Subject: signInTestUserID},
			ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"},
			Mode:        corev1alpha1.ConnectionModeReadOnly,
		},
	}
	if err := h.client.Create(context.Background(), leaving); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Delete(context.Background(), leaving); err != nil {
		t.Fatal(err)
	}
	_, invite := h.invite("alice")
	if outcome := h.inviteOutcome(invite.ID); outcome.State != InviteRefused ||
		!strings.Contains(outcome.Message, "could not be linked") || !strings.Contains(outcome.Message, "was revoked") {
		t.Fatalf("outcome = %+v", outcome)
	}
	if revoked := gh.revoked.list(); len(revoked) != 1 || revoked[0] != "ghu_alice_refreshed" {
		t.Fatalf("revoked = %v, want the refreshed token", revoked)
	}
}

// An account renamed between the approval's check and the refreshed token's
// is no longer the invited login, and its membership would be read for the
// old one: the invite is refused and the refreshed token revoked.
func TestInviteRefusesAnAccountRenamedDuringItsChecks(t *testing.T) {
	h, gh := newInviteHarness(t, map[string]any{"access_token": "ghu_alice", "refresh_token": "ghr_alice_renamed", "expires_in": 28800})
	_, invite := h.invite("alice")
	if outcome := h.inviteOutcome(invite.ID); outcome.State != InviteRefused || outcome.Connection != "" {
		t.Fatalf("outcome = %+v", outcome)
	}
	if revoked := gh.revoked.list(); len(revoked) != 1 || revoked[0] != "ghu_alice_renamed_refreshed" {
		t.Fatalf("revoked = %v, want the refreshed token", revoked)
	}
}

// An invite gives a host, and a host takes only a token that expires: a token
// GitHub issued without an expiry is refused and revoked.
func TestInviteRefusesATokenWithoutAnExpiry(t *testing.T) {
	for name, response := range map[string]map[string]any{
		"no expiry": {"access_token": "ghu_alice"},
		// An expiry with no refresh token beside it is one Orka cannot
		// refresh, which a host does not take.
		"no refresh token": {"access_token": "ghu_alice", "expires_in": 28800},
	} {
		t.Run(name, func(t *testing.T) {
			h, gh := newInviteHarness(t, response)
			_, invite := h.invite("alice")
			if outcome := h.inviteOutcome(invite.ID); outcome.State != InviteRefused || !strings.Contains(outcome.Message, "no expiry or refresh token") {
				t.Fatalf("outcome = %+v", outcome)
			}
			if revoked := gh.revoked.list(); len(revoked) != 1 || revoked[0] != "ghu_alice" {
				t.Fatalf("revoked = %v, want the approved token", revoked)
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

// A revocation GitHub did not confirm is reported as such, never as done.
func TestInviteReportsARevocationThatFailed(t *testing.T) {
	h, gh := newInviteHarness(t, map[string]any{"access_token": "ghu_alice"})
	gh.revoked.mu.Lock()
	gh.revoked.fail = true
	gh.revoked.mu.Unlock()
	_, invite := h.invite("carol")
	outcome := h.inviteOutcome(invite.ID)
	if outcome.State != InviteRefused || !strings.Contains(outcome.Message, "may still be active") || strings.Contains(outcome.Message, "was revoked") {
		t.Fatalf("outcome = %+v", outcome)
	}
}

// A provider moved to another GitHub app while the code was pending: the
// approved token is revoked with the app that issued it, not with the new
// app's secret.
func TestInviteRevokesWithTheIssuingAppAfterAnAppChange(t *testing.T) {
	h, gh := newInviteHarness(t, map[string]any{"access_token": "ghu_alice"})
	gh.onApprove = func() {
		secret := &corev1.Secret{}
		if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: "github-oauth"}, secret); err != nil {
			t.Error(err)
			return
		}
		secret.Data["clientSecret"] = []byte("another-app-secret")
		if err := h.client.Update(context.Background(), secret); err != nil {
			t.Error(err)
			return
		}
		provider := &corev1alpha1.ConnectorProvider{}
		if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: "github"}, provider); err != nil {
			t.Error(err)
			return
		}
		provider.Spec.OAuth.ClientID = "another-app"
		provider.Generation++
		if err := h.client.Update(context.Background(), provider); err != nil {
			t.Error(err)
		}
	}
	_, invite := h.invite("alice")
	if outcome := h.inviteOutcome(invite.ID); outcome.State != InviteRefused || !strings.Contains(outcome.Message, "was revoked") {
		t.Fatalf("outcome = %+v", outcome)
	}
	if revoked := gh.revoked.list(); len(revoked) != 1 || revoked[0] != "ghu_alice" {
		t.Fatalf("revoked = %v, want the approved token revoked by the issuing app", revoked)
	}
}

// inviteRevocationAPI accepts only the expected issuing-app credentials.
func inviteRevocationAPI(t *testing.T, wantSecret string, attempts *atomic.Int32, revoked *atomic.Bool) string {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/applications/client-id/token" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		attempts.Add(1)
		id, secret, ok := r.BasicAuth()
		if !ok || id != "client-id" || secret != wantSecret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body struct {
			AccessToken string `json:"access_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.AccessToken != "ghu_alice" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		revoked.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(api.Close)
	return api.URL
}

// A valid rotated secret must remain available for revocation even when the
// provider's disclosed settings changed and the creation secret was revoked.
func TestInviteRevokesWithRotatedSecretAfterProviderChange(t *testing.T) {
	for _, change := range []string{
		"scopes", "host template", "secret reference", "secret reference with missing original",
		"secret reference before provider readiness", "another app's secret reference",
	} {
		t.Run(change, func(t *testing.T) {
			h, gh := newInviteHarness(t, map[string]any{"access_token": "ghu_alice"})
			wantSecret := "rotated-test-secret"
			if change == "another app's secret reference" {
				wantSecret = "client-secret-value"
			}
			var attempts atomic.Int32
			var revoked atomic.Bool
			h.handlers.githubSignIn.GitHub.APIURL = inviteRevocationAPI(t, wantSecret, &attempts, &revoked)
			gh.onApprove = func() {
				ctx := context.Background()
				secret := &corev1.Secret{}
				if err := h.client.Get(ctx, types.NamespacedName{Namespace: connectorTestNamespace, Name: "github-oauth"}, secret); err != nil {
					t.Error(err)
					return
				}
				changedReference := strings.Contains(change, "secret reference")
				if changedReference {
					replacement := &corev1.Secret{
						ObjectMeta: metav1.ObjectMeta{Name: "replacement-github-oauth", Namespace: connectorTestNamespace},
						Data:       map[string][]byte{"clientSecret": []byte("rotated-test-secret")},
					}
					if err := h.client.Create(ctx, replacement); err != nil {
						t.Error(err)
						return
					}
					if change == "secret reference with missing original" {
						if err := h.client.Delete(ctx, secret); err != nil {
							t.Error(err)
							return
						}
					}
				} else {
					secret.Data["clientSecret"] = []byte("rotated-test-secret")
					if err := h.client.Update(ctx, secret); err != nil {
						t.Error(err)
						return
					}
				}
				provider := &corev1alpha1.ConnectorProvider{}
				if err := h.client.Get(ctx, types.NamespacedName{Namespace: connectorTestNamespace, Name: "github"}, provider); err != nil {
					t.Error(err)
					return
				}
				if changedReference {
					provider.Spec.OAuth.ClientSecretRef.Name = "replacement-github-oauth"
					if change == "another app's secret reference" {
						provider.Spec.OAuth.ClientID = "another-app"
					}
				} else if change == "scopes" {
					provider.Spec.OAuth.Scopes.Read = append(provider.Spec.OAuth.Scopes.Read, "read:org")
				} else {
					provider.Spec.AgentHost.TemplateRef.Name = "another-template"
				}
				provider.Generation++
				if err := h.client.Update(ctx, provider); err != nil {
					t.Error(err)
					return
				}
				if change == "secret reference before provider readiness" {
					return
				}
				for i := range provider.Status.Conditions {
					provider.Status.Conditions[i].ObservedGeneration = provider.Generation
				}
				if err := h.client.Status().Update(ctx, provider); err != nil {
					t.Error(err)
				}
			}
			status, invite := h.invite("alice")
			if status != http.StatusCreated {
				t.Fatalf("create invite: status = %d", status)
			}
			outcome := h.inviteOutcome(invite.ID)
			wantRefusal := "provider changed"
			if change == "secret reference with missing original" {
				wantRefusal = "App is not configured"
			}
			if outcome.State != InviteRefused || outcome.Connection != "" ||
				!strings.Contains(outcome.Message, wantRefusal) || !strings.Contains(outcome.Message, "was revoked") {
				t.Fatalf("outcome = %+v", outcome)
			}
			if !revoked.Load() || attempts.Load() != 1 {
				t.Fatalf("revoked = %v, attempts = %d; want revocation with the issuing app on the first attempt", revoked.Load(), attempts.Load())
			}
		})
	}
}

// An app secret changed to a wrong value while the code was pending fails
// the invite's checks, and the approved token is revoked with the secret
// read when the invite was created.
func TestInviteRevokesWithTheCreationSecretAfterASecretChange(t *testing.T) {
	h, gh := newInviteHarness(t, map[string]any{"access_token": "ghu_alice", "refresh_token": "ghr_alice", "expires_in": 28800})
	gh.onApprove = func() {
		secret := &corev1.Secret{}
		if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: "github-oauth"}, secret); err != nil {
			t.Error(err)
			return
		}
		secret.Data["clientSecret"] = []byte("not-the-app-secret")
		if err := h.client.Update(context.Background(), secret); err != nil {
			t.Error(err)
		}
	}
	_, invite := h.invite("alice")
	if outcome := h.inviteOutcome(invite.ID); outcome.State != InviteRefused || !strings.Contains(outcome.Message, "was revoked") {
		t.Fatalf("outcome = %+v", outcome)
	}
	if revoked := gh.revoked.list(); len(revoked) != 1 || revoked[0] != "ghu_alice" {
		t.Fatalf("revoked = %v, want the approved token revoked", revoked)
	}
}

// Inviting someone whose link already works keeps it and revokes the
// approved token; a revocation GitHub refused is reported on the outcome.
func TestInviteForAWorkingLinkRevokesTheUnusedToken(t *testing.T) {
	h, gh := newInviteHarness(t, map[string]any{"access_token": "ghu_alice", "refresh_token": "ghr_alice", "expires_in": 28800})
	_, first := h.invite("alice")
	if outcome := h.inviteOutcome(first.ID); outcome.State != InviteAccepted {
		t.Fatalf("first invite = %+v", outcome)
	}
	_, second := h.invite("alice")
	if outcome := h.inviteOutcome(second.ID); outcome.State != InviteAccepted || strings.Contains(outcome.Message, "may still be active") {
		t.Fatalf("second invite = %+v", outcome)
	}
	if revoked := gh.revoked.list(); len(revoked) != 1 || revoked[0] != "ghu_alice_refreshed" {
		t.Fatalf("revoked = %v, want the unused token", revoked)
	}
	gh.revoked.mu.Lock()
	gh.revoked.fail = true
	gh.revoked.mu.Unlock()
	_, third := h.invite("alice")
	if outcome := h.inviteOutcome(third.ID); outcome.State != InviteAccepted || !strings.Contains(outcome.Message, "may still be active") {
		t.Fatalf("invite whose unused token GitHub would not revoke = %+v", outcome)
	}
}

// A full invite table refuses before asking GitHub for a code.
func TestInvitesReserveCapacityBeforeStartingTheDeviceFlow(t *testing.T) {
	h, gh := newInviteHarness(t, nil)
	for i := range maxPendingInvites {
		if err := h.handlers.invites.add(&InviteResponse{ID: fmt.Sprint(i), State: InvitePending, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	if status, _ := h.invite("alice"); status != http.StatusTooManyRequests {
		t.Fatalf("invite with a full table = %d, want 429", status)
	}
	if got := gh.codes.Load(); got != 0 {
		t.Fatalf("%d device flows started for invites that could not be held", got)
	}
}

// Custody committed with a status write that failed is an accepted invite:
// the Connection reconciler records the link from the committed completion.
// The host it promises starts too: a decline the person's link carried is
// lifted with the commit, since nothing repairs it later.
func TestInviteAcceptsALinkCommittedButNotYetRecorded(t *testing.T) {
	h, gh := newInviteHarness(t, map[string]any{"access_token": "ghu_alice", "refresh_token": "ghr_alice", "expires_in": 28800})
	wantName := connectors.ConnectionName("github", githubauth.Issuer, signInTestUserID)
	declined := &corev1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{
			Name: wantName, Namespace: connectorTestNamespace, Finalizers: []string{controller.ConnectionCustodyFinalizer},
			Annotations: map[string]string{corev1alpha1.ConnectionAnnotationAgentHost: corev1alpha1.ConnectionAgentHostDeclined},
		},
		Spec: corev1alpha1.ConnectionSpec{
			Subject:     corev1alpha1.ConnectionSubject{Issuer: githubauth.Issuer, Subject: signInTestUserID},
			ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"},
			Mode:        corev1alpha1.ConnectionModeReadOnly,
		},
	}
	if err := h.client.Create(context.Background(), declined); err != nil {
		t.Fatal(err)
	}
	h.statusFailure = &atomic.Bool{}
	h.statusFailure.Store(true)
	_, invite := h.invite("alice")
	outcome := h.inviteOutcome(invite.ID)
	if outcome.State != InviteAccepted || outcome.Connection != wantName || !strings.Contains(outcome.Message, "finishes recording") || !outcome.AgentHost {
		t.Fatalf("outcome = %+v", outcome)
	}
	if revoked := gh.revoked.list(); len(revoked) != 0 {
		t.Fatalf("a committed link revokes nothing: %v", revoked)
	}
	connection := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: connectorTestNamespace, Name: wantName}, connection); err != nil {
		t.Fatal(err)
	}
	if got := connection.Annotations[corev1alpha1.ConnectionAnnotationAgentHost]; got != "" {
		t.Fatalf("annotation = %q, want the decline lifted with the commit", got)
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
