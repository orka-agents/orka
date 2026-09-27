/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package credential

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/outboundaccess"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

const (
	testNamespace = "tenant"
	testIssuer    = "https://issuer.example.test"
	testSubject   = "alice"
)

type fakeRefresher struct {
	mu        sync.Mutex
	calls     atomic.Int32
	response  connectors.TokenResponse
	err       error
	delay     time.Duration
	lastCfg   connectors.OAuthProviderConfig
	lastTok   string
	onRefresh func()
}

func (f *fakeRefresher) Refresh(_ context.Context, cfg connectors.OAuthProviderConfig, refreshToken string) (connectors.TokenResponse, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	if f.onRefresh != nil {
		f.onRefresh()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastCfg = cfg
	f.lastTok = refreshToken
	return f.response, f.err
}

type harness struct {
	t          *testing.T
	client     client.Client
	store      *sqlite.Store
	refresher  *fakeRefresher
	source     *Source
	connection *corev1alpha1.Connection
	now        time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = corev1alpha1.AddToScheme(scheme)
	provider := &corev1alpha1.ConnectorProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: testNamespace, Generation: 1},
		Spec: corev1alpha1.ConnectorProviderSpec{
			OAuth: corev1alpha1.ConnectorOAuthConfig{
				AuthorizeURL: "https://github.com/login/oauth/authorize", TokenURL: "https://github.com/login/oauth/access_token",
				ClientID: "client", ClientSecretRef: corev1alpha1.SecretKeySelector{Name: "oauth", Key: "clientSecret"},
			},
			Tools: []corev1alpha1.ConnectorTool{{
				Name: "gh_search", Class: corev1alpha1.ConnectorToolClassRead, Source: corev1alpha1.ConnectorToolSourceHTTP, Description: "search",
				HTTP: &corev1alpha1.ConnectorHTTPTool{URL: "https://api.github.com/search/issues", Method: "GET"},
			}},
		},
		Status: corev1alpha1.ConnectorProviderStatus{ObservedGeneration: 1, Conditions: []metav1.Condition{
			{Type: corev1alpha1.ConnectorProviderConditionAccepted, Status: metav1.ConditionTrue, ObservedGeneration: 1},
			{Type: corev1alpha1.ConnectorProviderConditionResolvedRefs, Status: metav1.ConditionTrue, ObservedGeneration: 1},
		}},
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "oauth", Namespace: testNamespace}, Data: map[string][]byte{"clientSecret": []byte("s3cret")}}
	connection := &corev1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{
			Name: connectors.ConnectionName("github", testIssuer, testSubject), Namespace: testNamespace, UID: "uid-1", Generation: 2,
		},
		Spec: corev1alpha1.ConnectionSpec{
			Subject: corev1alpha1.ConnectionSubject{Issuer: testIssuer, Subject: testSubject}, ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"}, Mode: "readOnly",
		},
		Status: corev1alpha1.ConnectionStatus{State: corev1alpha1.ConnectionStateReady, Consent: connectors.ConsentFor(provider), Conditions: []metav1.Condition{
			{Type: corev1alpha1.ConnectionConditionReady, Status: metav1.ConditionTrue, Reason: corev1alpha1.ConnectionReasonLinked, ObservedGeneration: 2},
			{Type: corev1alpha1.ConnectionConditionScopesGranted, Status: metav1.ConditionTrue, Reason: corev1alpha1.ConnectionReasonScopesGranted, ObservedGeneration: 2},
			{Type: corev1alpha1.ConnectionConditionProviderResolved, Status: metav1.ConditionTrue, Reason: corev1alpha1.ConnectionReasonProviderResolved, ObservedGeneration: 2},
		}},
	}
	c := ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(provider, secret, connection).WithStatusSubresource(&corev1alpha1.Connection{}).Build()
	dbPath := filepath.Join(t.TempDir(), "custody.db")
	db, err := sqlite.NewDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := sqlite.NewStore(db, dbPath)
	cipher, err := sqlite.NewAgentExecutionSnapshotCipher(bytes.Repeat([]byte{1}, sqlite.AgentExecutionSnapshotKeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetAgentExecutionSnapshotCipher(cipher); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	refresher := &fakeRefresher{response: connectors.TokenResponse{AccessToken: "gho_new", RefreshToken: "ghr_new", ExpiresAt: now.Add(time.Hour)}}
	source := &Source{Client: c, APIReader: c, Credentials: s, OAuth: refresher, Now: func() time.Time { return now }}
	return &harness{t: t, client: c, store: s, refresher: refresher, source: source, connection: connection, now: now}
}

func (h *harness) put(credential store.ConnectorCredential) {
	h.t.Helper()
	ref, err := connectors.CredentialRef(h.connection)
	if err != nil {
		h.t.Fatal(err)
	}
	if credential.AuthorityDigest == "" {
		// Issued by the fixture provider unless a test says otherwise.
		provider := &corev1alpha1.ConnectorProvider{}
		if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "github"}, provider); err != nil {
			h.t.Fatal(err)
		}
		credential.AuthorityDigest = connectors.ProviderIssuerDigest(provider)
	}
	if err := h.store.PutConnectorCredential(context.Background(), ref, credential); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) request() outboundaccess.ConnectionCredentialRequest {
	return outboundaccess.ConnectionCredentialRequest{
		Namespace: testNamespace, Provider: "github", Issuer: testIssuer, Subject: testSubject,
		Frozen: outboundaccess.FrozenConnection{UID: "uid-1", Generation: 2},
		Tool:   outboundaccess.ToolBinding{Name: "gh_search", URL: "https://api.github.com/search/issues", Method: "GET", Class: corev1alpha1.AgentRuntimeBrokeredToolClassRead},
	}
}

func (h *harness) reload() *corev1alpha1.Connection {
	h.t.Helper()
	connection := &corev1alpha1.Connection{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: h.connection.Name}, connection); err != nil {
		h.t.Fatal(err)
	}
	return connection
}

func TestResolveReturnsFreshCredentialWithoutRefresh(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_fresh", RefreshToken: "ghr", TokenType: "bearer", ExpiresAt: h.now.Add(2 * time.Hour)})
	got, err := h.source.ResolveConnectionCredential(context.Background(), h.request())
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "gho_fresh" || got.TokenType != "bearer" || got.ConnectionUID != "uid-1" || got.Generation != 2 || got.Mode != "readOnly" {
		t.Fatalf("credential = %+v", got)
	}
	if h.refresher.calls.Load() != 0 {
		t.Fatal("fresh credential must not be refreshed")
	}
	// No expiry reported by the provider also means no refresh.
	h.put(store.ConnectorCredential{AccessToken: "gho_forever"})
	if got, err := h.source.ResolveConnectionCredential(context.Background(), h.request()); err != nil || got.AccessToken != "gho_forever" || h.refresher.calls.Load() != 0 {
		t.Fatalf("credential without expiry = %+v err = %v calls = %d", got, err, h.refresher.calls.Load())
	}
}

func TestResolveRefreshesNearExpiryAndWritesBack(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr_old", TokenType: "bearer", ExpiresAt: h.now.Add(30 * time.Second), Scopes: []string{"repo"}})
	got, err := h.source.ResolveConnectionCredential(context.Background(), h.request())
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "gho_new" || h.refresher.calls.Load() != 1 || h.refresher.lastTok != "ghr_old" {
		t.Fatalf("credential = %+v calls = %d last = %q", got, h.refresher.calls.Load(), h.refresher.lastTok)
	}
	if h.refresher.lastCfg.ClientSecret != "s3cret" || h.refresher.lastCfg.ClientID != "client" {
		t.Fatalf("provider config = %+v", h.refresher.lastCfg)
	}
	ref, _ := connectors.CredentialRef(h.connection)
	stored, err := h.store.GetConnectorCredential(context.Background(), ref)
	if err != nil || stored.AccessToken != "gho_new" || stored.RefreshToken != "ghr_new" || stored.TokenType != "bearer" || len(stored.Scopes) != 1 {
		t.Fatalf("stored = %+v err = %v", stored, err)
	}
	updated := h.reload()
	if updated.Status.LastRefreshTime == nil || updated.Status.ExpiresAt == nil || !updated.Status.ExpiresAt.Time.Equal(h.now.Add(time.Hour)) {
		t.Fatalf("status after refresh = %+v", updated.Status)
	}
	if updated.Status.State != corev1alpha1.ConnectionStateReady {
		t.Fatalf("state = %q", updated.Status.State)
	}

	// A provider that does not rotate the refresh token keeps the old one.
	h.refresher.response = connectors.TokenResponse{AccessToken: "gho_third", ExpiresAt: h.now.Add(time.Hour)}
	h.put(store.ConnectorCredential{AccessToken: "gho_stale", RefreshToken: "ghr_keep", ExpiresAt: h.now.Add(-time.Minute)})
	if _, err := h.source.ResolveConnectionCredential(context.Background(), h.request()); err != nil {
		t.Fatal(err)
	}
	stored, _ = h.store.GetConnectorCredential(context.Background(), ref)
	if stored.AccessToken != "gho_third" || stored.RefreshToken != "ghr_keep" {
		t.Fatalf("stored after non-rotating refresh = %+v", stored)
	}
}

func TestResolveRevokedRefreshShredsCustody(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr_old", ExpiresAt: h.now.Add(-time.Minute)})
	h.refresher.err = &connectors.OAuthError{StatusCode: 400, Code: "invalid_grant"}
	_, err := h.source.ResolveConnectionCredential(context.Background(), h.request())
	if err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("err = %v", err)
	}
	ref, _ := connectors.CredentialRef(h.connection)
	if _, err := h.store.GetConnectorCredential(context.Background(), ref); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("custody after revocation err = %v, want ErrNotFound", err)
	}
	// The shred leaves no tombstone: the person can consent again.
	h.put(store.ConnectorCredential{AccessToken: "gho_reconsented"})
	if err := h.store.DeleteConnectorCredential(context.Background(), ref.ConnectionUID); err != nil {
		t.Fatal(err)
	}
	updated := h.reload()
	ready := meta.FindStatusCondition(updated.Status.Conditions, corev1alpha1.ConnectionConditionReady)
	if updated.Status.State != corev1alpha1.ConnectionStateRevoked || ready == nil || ready.Status != metav1.ConditionTrue && ready.Reason != corev1alpha1.ConnectionReasonRevoked {
		t.Fatalf("status after revocation = %+v", updated.Status)
	}
	// The link is now not Ready, so the next call fails before any refresh.
	calls := h.refresher.calls.Load()
	if _, err := h.source.ResolveConnectionCredential(context.Background(), h.request()); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("err after revocation = %v", err)
	}
	if h.refresher.calls.Load() != calls {
		t.Fatal("a revoked connection must not be refreshed again")
	}
}

func TestResolveExpiredWithoutRefreshTokenMarksExpired(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_old", ExpiresAt: h.now.Add(-time.Minute)})
	_, err := h.source.ResolveConnectionCredential(context.Background(), h.request())
	if err == nil || !strings.Contains(err.Error(), "cannot be refreshed") {
		t.Fatalf("err = %v", err)
	}
	if h.refresher.calls.Load() != 0 {
		t.Fatal("no refresh token means no refresh call")
	}
	updated := h.reload()
	ready := meta.FindStatusCondition(updated.Status.Conditions, corev1alpha1.ConnectionConditionReady)
	if updated.Status.State != corev1alpha1.ConnectionStateExpired || ready == nil || ready.Reason != corev1alpha1.ConnectionReasonExpired {
		t.Fatalf("status = %+v", updated.Status)
	}
}

func TestResolveTransientRefreshFailureKeepsState(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr_old", ExpiresAt: h.now.Add(-time.Minute)})
	h.refresher.err = &connectors.OAuthError{StatusCode: 503, Code: "temporarily_unavailable"}
	_, err := h.source.ResolveConnectionCredential(context.Background(), h.request())
	if err == nil || strings.Contains(err.Error(), "ghr_old") || strings.Contains(err.Error(), "gho_old") {
		t.Fatalf("err = %v", err)
	}
	if h.reload().Status.State != corev1alpha1.ConnectionStateReady {
		t.Fatal("a transient failure must not change the link state")
	}
	ref, _ := connectors.CredentialRef(h.connection)
	if stored, err := h.store.GetConnectorCredential(context.Background(), ref); err != nil || stored.RefreshToken != "ghr_old" {
		t.Fatalf("custody must be untouched after a transient failure: %+v %v", stored, err)
	}
}

func TestResolveFailsClosedOnIdentityAndBindingMismatch(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho", ExpiresAt: h.now.Add(time.Hour)})
	for name, mutate := range map[string]func(*outboundaccess.ConnectionCredentialRequest){
		"other subject":     func(r *outboundaccess.ConnectionCredentialRequest) { r.Subject = "bob" },
		"other issuer":      func(r *outboundaccess.ConnectionCredentialRequest) { r.Issuer = "https://other.example.test" },
		"other provider":    func(r *outboundaccess.ConnectionCredentialRequest) { r.Provider = "gmail" },
		"frozen uid":        func(r *outboundaccess.ConnectionCredentialRequest) { r.Frozen.UID = "uid-9" },
		"frozen generation": func(r *outboundaccess.ConnectionCredentialRequest) { r.Frozen.Generation = 1 },
		"no frozen":         func(r *outboundaccess.ConnectionCredentialRequest) { r.Frozen = outboundaccess.FrozenConnection{} },
		"no subject":        func(r *outboundaccess.ConnectionCredentialRequest) { r.Subject = "" },
	} {
		req := h.request()
		mutate(&req)
		if _, err := h.source.ResolveConnectionCredential(context.Background(), req); err == nil {
			t.Fatalf("%s must fail closed", name)
		}
	}
	// A Connection whose scopes no longer cover its mode is not usable.
	stale := h.reload()
	meta.SetStatusCondition(&stale.Status.Conditions, metav1.Condition{Type: corev1alpha1.ConnectionConditionScopesGranted, Status: metav1.ConditionFalse, Reason: corev1alpha1.ConnectionReasonConsentRequired})
	if err := h.client.Status().Update(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	if _, err := h.source.ResolveConnectionCredential(context.Background(), h.request()); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("scopes not granted err = %v", err)
	}
	if _, err := (&Source{}).ResolveConnectionCredential(context.Background(), h.request()); err == nil {
		t.Fatal("unconfigured source must fail")
	}
}

func TestResolveRefreshIsSingleFlight(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr_old", ExpiresAt: h.now.Add(-time.Minute)})
	h.refresher.delay = 50 * time.Millisecond
	var wg sync.WaitGroup
	results := make([]string, 8)
	errs := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := h.source.ResolveConnectionCredential(context.Background(), h.request())
			results[i], errs[i] = got.AccessToken, err
		}(i)
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil || results[i] != "gho_new" {
			t.Fatalf("caller %d = %q err = %v", i, results[i], errs[i])
		}
	}
	if calls := h.refresher.calls.Load(); calls != 1 {
		t.Fatalf("refresh calls = %d, want 1", calls)
	}
}

// TestRefreshLosesToConcurrentReconsent models a tool call refreshing while
// the owner completes a new consent: the refresh result must not overwrite
// the newer material, and an invalid_grant for the old token must not shred it.
func TestRefreshLosesToConcurrentReconsent(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr_old", ExpiresAt: h.now.Add(-time.Minute)})
	ref, _ := connectors.CredentialRef(h.connection)
	reconsent := func() {
		h.put(store.ConnectorCredential{AccessToken: "gho_reconsented", RefreshToken: "ghr_reconsented", ExpiresAt: h.now.Add(2 * time.Hour)})
	}
	h.refresher.onRefresh = reconsent
	got, err := h.source.ResolveConnectionCredential(context.Background(), h.request())
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "gho_reconsented" {
		t.Fatalf("refresh must yield to the newer consent, got %q", got.AccessToken)
	}
	stored, _ := h.store.GetConnectorCredential(context.Background(), ref)
	if stored.AccessToken != "gho_reconsented" || stored.RefreshToken != "ghr_reconsented" {
		t.Fatalf("custody after lost race = %+v", stored)
	}

	// A mode change and re-consent that land during the refresh are seen:
	// the generation no longer matches the frozen binding, so nothing is
	// released even though custody now holds fresh material.
	h.put(store.ConnectorCredential{AccessToken: "gho_old3", RefreshToken: "ghr_old3", ExpiresAt: h.now.Add(-time.Minute)})
	h.refresher.onRefresh = func() {
		reconsent()
		live := h.reload()
		live.Generation = 3
		if err := h.client.Update(context.Background(), live); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.source.ResolveConnectionCredential(context.Background(), h.request()); err == nil || !strings.Contains(err.Error(), "changed since the task was dispatched") {
		t.Fatalf("generation change during refresh err = %v", err)
	}
	h.refresher.onRefresh = reconsent
	live := h.reload()
	live.Generation = 2
	if err := h.client.Update(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	h.put(store.ConnectorCredential{AccessToken: "gho_old4", RefreshToken: "ghr_old4", ExpiresAt: h.now.Add(-time.Minute)})

	// The same for a late invalid_grant: the newer consent survives.
	h.put(store.ConnectorCredential{AccessToken: "gho_old2", RefreshToken: "ghr_old2", ExpiresAt: h.now.Add(-time.Minute)})
	h.refresher.err = &connectors.OAuthError{StatusCode: 400, Code: "invalid_grant"}
	got, err = h.source.ResolveConnectionCredential(context.Background(), h.request())
	if err != nil || got.AccessToken != "gho_reconsented" {
		t.Fatalf("late invalid_grant must not shred the newer consent: got %+v err = %v", got, err)
	}
	if h.reload().Status.State != corev1alpha1.ConnectionStateReady {
		t.Fatal("the link must stay Ready when the revoked token was already superseded")
	}
}

// TestRefreshRefusesChangedProviderAuthority covers a provider whose OAuth
// client was rotated after consent: the held refresh token belongs to the old
// client and is never sent to the new token endpoint.
func TestRefreshRefusesChangedProviderAuthority(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr_old", ExpiresAt: h.now.Add(-time.Minute)})
	provider := &corev1alpha1.ConnectorProvider{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: h.connection.Namespace, Name: h.connection.Spec.ProviderRef.Name}, provider); err != nil {
		t.Fatal(err)
	}
	provider.Spec.OAuth.TokenURL = "https://provider.example.test/rotated-token"
	if err := h.client.Update(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	_, err := h.source.ResolveConnectionCredential(context.Background(), h.request())
	if err == nil || !strings.Contains(err.Error(), "OAuth client changed") {
		t.Fatalf("refresh against a rotated client err = %v", err)
	}
	if h.refresher.calls.Load() != 0 {
		t.Fatal("the refresh token must not be sent to a different authority")
	}
}

// TestRefreshWithNarrowedScopesFailsClosed covers a provider that narrows the
// grant on refresh: the narrowed scopes are recorded, ScopesGranted is
// withdrawn, and nothing is released for the mode.
func TestRefreshWithNarrowedScopesFailsClosed(t *testing.T) {
	h := newHarness(t)
	provider := &corev1alpha1.ConnectorProvider{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "github"}, provider); err != nil {
		t.Fatal(err)
	}
	provider.Spec.OAuth.Scopes.Read = []string{"read:user"}
	if err := h.client.Update(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	h.put(store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr_old", ExpiresAt: h.now.Add(-time.Minute), Scopes: []string{"read:user"}})
	h.refresher.response = connectors.TokenResponse{AccessToken: "gho_narrow", TokenType: "bearer", ExpiresAt: h.now.Add(time.Hour), Scopes: []string{"public_repo"}}
	_, err := h.source.ResolveConnectionCredential(context.Background(), h.request())
	if err == nil || !strings.Contains(err.Error(), "no longer covers") {
		t.Fatalf("narrowed refresh err = %v", err)
	}
	live := h.reload()
	granted := meta.FindStatusCondition(live.Status.Conditions, corev1alpha1.ConnectionConditionScopesGranted)
	if granted == nil || granted.Status != metav1.ConditionFalse || granted.Reason != corev1alpha1.ConnectionReasonConsentRequired ||
		strings.Join(live.Status.GrantedScopes, ",") != "public_repo" || live.Status.State != corev1alpha1.ConnectionStatePending {
		t.Fatalf("status after narrowed refresh = %+v", live.Status)
	}
	if _, err := h.source.ResolveConnectionCredential(context.Background(), h.request()); err == nil {
		t.Fatal("the link must stay unusable until the person consents again")
	}
}

// TestRevocationVerdictYieldsToConcurrentReconsent covers a consent that
// completes between the custody shred and the Revoked status write: the
// fenced patch conflicts and the fresh link keeps its Ready status.
func TestRevocationVerdictYieldsToConcurrentReconsent(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr_old", ExpiresAt: h.now.Add(-time.Minute)})
	h.refresher.err = &connectors.OAuthError{StatusCode: 400, Code: "invalid_grant"}
	h.refresher.onRefresh = func() {
		// The owner re-consents while the refresh is in flight: new custody
		// and a new status write land before the stale verdict.
		h.put(store.ConnectorCredential{AccessToken: "gho_new", RefreshToken: "ghr_new"})
		live := h.reload()
		linked := metav1.NewTime(h.now)
		live.Status.LinkedAt = &linked
		if err := h.client.Status().Update(context.Background(), live); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := h.source.ResolveConnectionCredential(context.Background(), h.request()); err != nil || got.AccessToken != "gho_new" {
		t.Fatalf("resolve during re-consent = %+v err = %v", got, err)
	}
	if live := h.reload(); live.Status.State != corev1alpha1.ConnectionStateReady {
		t.Fatalf("a stale revocation verdict must not overwrite the fresh link: %+v", live.Status)
	}
}

// TestResolveJudgesProviderWithoutRefresh covers a fresh (non-expiring)
// credential: the provider is still validated on every resolution, so a
// retargeted tool set or a rotated client refuses the token even before
// the Connection's status catches up.
func TestResolveJudgesProviderWithoutRefresh(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_fresh", RefreshToken: "ghr_fresh", ExpiresAt: h.now.Add(time.Hour), Scopes: []string{"read:user"}})
	if _, err := h.source.ResolveConnectionCredential(context.Background(), h.request()); err != nil {
		t.Fatalf("baseline resolve: %v", err)
	}
	provider := &corev1alpha1.ConnectorProvider{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "github"}, provider); err != nil {
		t.Fatal(err)
	}
	// A new credential-receiving destination the person never consented to.
	retargeted := provider.DeepCopy()
	retargeted.Spec.Tools = append(retargeted.Spec.Tools, corev1alpha1.ConnectorTool{
		Name: "gh_extra", Class: corev1alpha1.ConnectorToolClassRead, Source: corev1alpha1.ConnectorToolSourceHTTP, Description: "x",
		HTTP: &corev1alpha1.ConnectorHTTPTool{URL: "https://api.github.com/extra"},
	})
	if err := h.client.Update(context.Background(), retargeted); err != nil {
		t.Fatal(err)
	}
	if _, err := h.source.ResolveConnectionCredential(context.Background(), h.request()); err == nil || !strings.Contains(err.Error(), "changed since consent") {
		t.Fatalf("retargeted provider err = %v, want consent refusal", err)
	}
	if h.refresher.calls.Load() != 0 {
		t.Fatal("no refresh may run against an unconsented provider")
	}
	// A rotated OAuth client refuses the token issued by the old one.
	rotated := retargeted.DeepCopy()
	rotated.Spec.Tools = provider.Spec.Tools
	rotated.Spec.OAuth.ClientID = "rotated-client"
	if err := h.client.Update(context.Background(), rotated); err != nil {
		t.Fatal(err)
	}
	if _, err := h.source.ResolveConnectionCredential(context.Background(), h.request()); err == nil || !strings.Contains(err.Error(), "OAuth client changed") {
		t.Fatalf("rotated client err = %v, want issuer refusal", err)
	}
}

// TestRefreshLosesToReconsentOnRetargetedProvider covers a provider whose
// client and tool destination both change while a refresh is in flight and
// the person re-consents to the new provider: the newer credential is
// returned only if the executing tool is still what the new provider
// declares, judged against the same provider state as the credential.
func TestRefreshLosesToReconsentOnRetargetedProvider(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr_old", ExpiresAt: h.now.Add(-time.Minute), Scopes: []string{"read:user"}})
	h.refresher.onRefresh = func() {
		provider := &corev1alpha1.ConnectorProvider{}
		if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "github"}, provider); err != nil {
			t.Fatal(err)
		}
		provider.Spec.OAuth.ClientID = "client-b"
		provider.Spec.Tools[0].HTTP.URL = "https://api.github.com/elsewhere"
		if err := h.client.Update(context.Background(), provider); err != nil {
			t.Fatal(err)
		}
		// Re-consent to provider B lands newer material and a matching consent record.
		h.put(store.ConnectorCredential{AccessToken: "gho_b", RefreshToken: "ghr_b", ExpiresAt: h.now.Add(time.Hour), Scopes: []string{"read:user"}})
		live := h.reload()
		live.Status.Consent = connectors.ConsentFor(provider)
		if err := h.client.Status().Update(context.Background(), live); err != nil {
			t.Fatal(err)
		}
	}
	_, err := h.source.ResolveConnectionCredential(context.Background(), h.request())
	if err == nil || !strings.Contains(err.Error(), "does not match the endpoint declared") {
		t.Fatalf("credential from a retargeted re-consent must not be paired with the old destination: err = %v", err)
	}
}
