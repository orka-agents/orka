/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package credential

import (
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
	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/storageclient/storageclienttest"
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
	revoked   []string
	lastTok   string
	onRefresh func()
}

func (f *fakeRefresher) Revoke(_ context.Context, _ connectors.OAuthProviderConfig, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, token)
	return nil
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
	store      *storageclient.Client
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
		Status: corev1alpha1.ConnectionStatus{State: corev1alpha1.ConnectionStateReady, GrantSequence: 1, Consent: connectors.ConsentFor(provider), Conditions: []metav1.Condition{
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
	s := storageclienttest.Client(t, sqlite.NewStore(db, dbPath))
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
	if credential.AuthorityDigest == "" || credential.RevocationDigest == "" {
		provider := &corev1alpha1.ConnectorProvider{}
		if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "github"}, provider); err != nil {
			h.t.Fatal(err)
		}
		if credential.AuthorityDigest == "" {
			credential.AuthorityDigest = connectors.ProviderIssuerDigest(provider)
		}
		if credential.RevocationDigest == "" {
			credential.RevocationDigest = connectors.ProviderRevocationDigest(provider)
		}
	}
	if err := h.store.PutConnectorCredential(context.Background(), ref, credential); err != nil {
		h.t.Fatal(err)
	}
	held, err := h.store.GetConnectorCredential(context.Background(), ref)
	if err != nil {
		h.t.Fatal(err)
	}
	live := h.reload()
	live.Status.GrantSequence = held.GrantSequence
	if err := h.client.Status().Update(context.Background(), live); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) request() outboundaccess.ConnectionCredentialRequest {
	grant := int64(1)
	if ref, err := connectors.CredentialRef(h.connection); err == nil {
		if held, err := h.store.GetConnectorCredential(context.Background(), ref); err == nil && held.GrantSequence > 0 {
			grant = held.GrantSequence
		}
	}
	return outboundaccess.ConnectionCredentialRequest{
		Namespace: testNamespace, Provider: "github", Issuer: testIssuer, Subject: testSubject,
		Frozen: outboundaccess.FrozenConnection{UID: "uid-1", Generation: 2, GrantSequence: grant},
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
	h.put(store.ConnectorCredential{AccessToken: "gho_forever"})
	if got, err := h.source.ResolveConnectionCredential(context.Background(), h.request()); err != nil || got.AccessToken != "gho_forever" || h.refresher.calls.Load() != 0 {
		t.Fatalf("credential without expiry = %+v err = %v calls = %d", got, err, h.refresher.calls.Load())
	}
}

func TestResolveRefreshesNearExpiryAndWritesBack(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr_old", TokenType: "bearer", ExpiresAt: h.now.Add(30 * time.Second), Scopes: []string{"repo"}, RevocationDigest: "revocation-1"})
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
	if stored.RevocationDigest != "revocation-1" || stored.GrantSequence != 1 {
		t.Fatalf("stored revocation digest = %q grant = %d, want carried over", stored.RevocationDigest, stored.GrantSequence)
	}
	updated := h.reload()
	if updated.Status.LastRefreshTime == nil || updated.Status.ExpiresAt == nil || !updated.Status.ExpiresAt.Time.Equal(h.now.Add(time.Hour)) {
		t.Fatalf("status after refresh = %+v", updated.Status)
	}
	if updated.Status.State != corev1alpha1.ConnectionStateReady {
		t.Fatalf("state = %q", updated.Status.State)
	}

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
	h.put(store.ConnectorCredential{AccessToken: "gho_reconsented"})
	if err := h.store.DeleteConnectorCredential(context.Background(), ref.ConnectionUID); err != nil {
		t.Fatal(err)
	}
	updated := h.reload()
	ready := meta.FindStatusCondition(updated.Status.Conditions, corev1alpha1.ConnectionConditionReady)
	if updated.Status.State != corev1alpha1.ConnectionStateRevoked || ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != corev1alpha1.ConnectionReasonRevoked {
		t.Fatalf("status after revocation = %+v", updated.Status)
	}
	calls := h.refresher.calls.Load()
	_, err = h.source.ResolveConnectionCredential(context.Background(), h.request())
	if err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("err after revocation = %v", err)
	}
	if _, ok := errors.AsType[*outboundaccess.ConnectionRequiredError](err); !ok {
		t.Fatalf("err = %#v, want a ConnectionRequiredError the broker can show", err)
	}
	if h.refresher.calls.Load() != calls {
		t.Fatal("a revoked connection must not be refreshed again")
	}
}

func TestResolveExpiredWithoutRefreshTokenMarksExpired(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_old", ExpiresAt: h.now.Add(-time.Minute)})
	_, err := h.source.ResolveConnectionCredential(context.Background(), h.request())
	if refusal, ok := errors.AsType[*outboundaccess.ConnectionRequiredError](err); !ok || refusal.Kind != outboundaccess.ConnectionExpired {
		t.Fatalf("err = %v, want an expired-link refusal", err)
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
		"frozen grant":      func(r *outboundaccess.ConnectionCredentialRequest) { r.Frozen.GrantSequence = 2 },
		"no frozen grant":   func(r *outboundaccess.ConnectionCredentialRequest) { r.Frozen.GrantSequence = 0 },
		"no frozen":         func(r *outboundaccess.ConnectionCredentialRequest) { r.Frozen = outboundaccess.FrozenConnection{} },
		"no subject":        func(r *outboundaccess.ConnectionCredentialRequest) { r.Subject = "" },
	} {
		req := h.request()
		mutate(&req)
		if _, err := h.source.ResolveConnectionCredential(context.Background(), req); err == nil {
			t.Fatalf("%s must fail closed", name)
		}
	}
	relinked := h.reload()
	relinked.Status.GrantSequence++
	if err := h.client.Status().Update(context.Background(), relinked); err != nil {
		t.Fatal(err)
	}
	if _, err := h.source.ResolveConnectionCredential(context.Background(), h.request()); err == nil || !strings.Contains(err.Error(), "re-linked") {
		t.Fatalf("re-linked connection err = %v", err)
	}
	relinked = h.reload()
	relinked.Status.GrantSequence--
	if err := h.client.Status().Update(context.Background(), relinked); err != nil {
		t.Fatal(err)
	}
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

func TestRefreshFlightLeavesAnotherGrantAlone(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr_old", ExpiresAt: h.now.Add(-time.Minute)})
	req := h.request()
	h.refresher.err = &connectors.OAuthError{StatusCode: 400, Code: "invalid_grant"}
	h.source.Credentials = &disconnectingCustody{Client: h.store, onRead: func() {
		h.put(store.ConnectorCredential{AccessToken: "gho_reconsented", RefreshToken: "ghr_reconsented", ExpiresAt: h.now.Add(-time.Second)})
	}}
	if _, err := h.source.ResolveConnectionCredential(context.Background(), req); err == nil || !strings.Contains(err.Error(), "re-linked") {
		t.Fatalf("err = %v, want the re-linked refusal", err)
	}
	if calls := h.refresher.calls.Load(); calls != 0 {
		t.Fatalf("refresh calls = %d, want the new grant left alone", calls)
	}
	ref, _ := connectors.CredentialRef(h.connection)
	if stored, err := h.store.GetConnectorCredential(context.Background(), ref); err != nil || stored.AccessToken != "gho_reconsented" {
		t.Fatalf("custody = %+v err = %v, want the re-consented credential kept", stored, err)
	}
	if live := h.reload(); live.Status.State == corev1alpha1.ConnectionStateRevoked {
		t.Fatal("the re-consented link must not be marked revoked")
	}
}

func TestRefreshLosesToConcurrentReconsent(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr_old", ExpiresAt: h.now.Add(-time.Minute)})
	ref, _ := connectors.CredentialRef(h.connection)
	reconsent := func() {
		h.put(store.ConnectorCredential{AccessToken: "gho_reconsented", RefreshToken: "ghr_reconsented", ExpiresAt: h.now.Add(2 * time.Hour)})
	}
	h.refresher.onRefresh = reconsent
	if _, err := h.source.ResolveConnectionCredential(context.Background(), h.request()); err == nil || !strings.Contains(err.Error(), "re-linked") {
		t.Fatalf("refresh losing to a re-consent err = %v, want refusal", err)
	}
	stored, _ := h.store.GetConnectorCredential(context.Background(), ref)
	if stored.AccessToken != "gho_reconsented" || stored.RefreshToken != "ghr_reconsented" {
		t.Fatalf("custody after lost race = %+v", stored)
	}
	h.refresher.mu.Lock()
	revoked := strings.Join(h.refresher.revoked, ",")
	h.refresher.mu.Unlock()
	if revoked != "" {
		t.Fatalf("revoked = %q, want nothing revoked while the winning consent is live", revoked)
	}
	retired, err := h.store.ListRetiredConnectorCredentials(context.Background(), ref)
	if err != nil || len(retired) == 0 || retired[len(retired)-1].AccessToken != "gho_new" {
		t.Fatalf("retired = %+v err = %v, want the losing refresh's material kept for disconnect", retired, err)
	}

	h.put(store.ConnectorCredential{AccessToken: "gho_old3", RefreshToken: "ghr_old3", ExpiresAt: h.now.Add(-time.Minute)})
	h.refresher.onRefresh = func() {
		reconsent()
		live := h.reload()
		live.Generation = 3
		if err := h.client.Update(context.Background(), live); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.source.ResolveConnectionCredential(context.Background(), h.request()); err == nil ||
		!strings.Contains(err.Error(), "changed after this Task started") && !strings.Contains(err.Error(), "re-linked") {
		t.Fatalf("generation change during refresh err = %v", err)
	}
	h.refresher.onRefresh = reconsent
	live := h.reload()
	live.Generation = 2
	if err := h.client.Update(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	h.put(store.ConnectorCredential{AccessToken: "gho_old4", RefreshToken: "ghr_old4", ExpiresAt: h.now.Add(-time.Minute)})

	h.put(store.ConnectorCredential{AccessToken: "gho_old2", RefreshToken: "ghr_old2", ExpiresAt: h.now.Add(-time.Minute)})
	h.refresher.err = &connectors.OAuthError{StatusCode: 400, Code: "invalid_grant"}
	if _, err := h.source.ResolveConnectionCredential(context.Background(), h.request()); err == nil || !strings.Contains(err.Error(), "re-linked") {
		t.Fatalf("late invalid_grant err = %v, want refusal", err)
	}
	if held, _ := h.store.GetConnectorCredential(context.Background(), ref); held.AccessToken != "gho_reconsented" {
		t.Fatalf("custody after late invalid_grant = %+v, want the newer consent kept", held)
	}
	if h.reload().Status.State != corev1alpha1.ConnectionStateReady {
		t.Fatal("the link must stay Ready when the revoked token was already superseded")
	}
}

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
	h.refresher.response = connectors.TokenResponse{AccessToken: "gho_narrow", TokenType: "bearer", ExpiresAt: h.now.Add(time.Hour), Scopes: []string{"public_repo"}, ScopePresent: true}
	_, err := h.source.ResolveConnectionCredential(context.Background(), h.request())
	if err == nil || !strings.Contains(err.Error(), "no longer covers") {
		t.Fatalf("narrowed refresh err = %v", err)
	}
	live := h.reload()
	ref, _ := connectors.CredentialRef(live)
	if held, err := h.store.GetConnectorCredential(context.Background(), ref); err != nil || held.AccessToken != "gho_narrow" {
		t.Fatalf("custody after narrowed refresh = %+v err = %v, want the rotated material sealed", held, err)
	}
	granted := meta.FindStatusCondition(live.Status.Conditions, corev1alpha1.ConnectionConditionScopesGranted)
	if granted == nil || granted.Status != metav1.ConditionFalse || granted.Reason != corev1alpha1.ConnectionReasonConsentRequired ||
		strings.Join(live.Status.GrantedScopes, ",") != "public_repo" || live.Status.State != corev1alpha1.ConnectionStatePending {
		t.Fatalf("status after narrowed refresh = %+v", live.Status)
	}
	if _, err := h.source.ResolveConnectionCredential(context.Background(), h.request()); err == nil {
		t.Fatal("the link must stay unusable until the person consents again")
	}
}

func TestRevocationVerdictYieldsToConcurrentReconsent(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr_old", ExpiresAt: h.now.Add(-time.Minute)})
	h.refresher.err = &connectors.OAuthError{StatusCode: 400, Code: "invalid_grant"}
	h.refresher.onRefresh = func() {
		h.put(store.ConnectorCredential{AccessToken: "gho_new", RefreshToken: "ghr_new"})
		live := h.reload()
		linked := metav1.NewTime(h.now)
		live.Status.LinkedAt = &linked
		if err := h.client.Status().Update(context.Background(), live); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.source.ResolveConnectionCredential(context.Background(), h.request()); err == nil || !strings.Contains(err.Error(), "re-linked") {
		t.Fatalf("resolve during re-consent err = %v, want refusal", err)
	}
	if live := h.reload(); live.Status.State != corev1alpha1.ConnectionStateReady {
		t.Fatalf("a stale revocation verdict must not overwrite the fresh link: %+v", live.Status)
	}
	ref, _ := connectors.CredentialRef(h.connection)
	if held, err := h.store.GetConnectorCredential(context.Background(), ref); err != nil || held.AccessToken != "gho_new" {
		t.Fatalf("custody after a yielded verdict = %+v err = %v, want the fresh consent kept", held, err)
	}
}

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
		h.put(store.ConnectorCredential{AccessToken: "gho_b", RefreshToken: "ghr_b", ExpiresAt: h.now.Add(time.Hour), Scopes: []string{"read:user"}})
		live := h.reload()
		live.Status.Consent = connectors.ConsentFor(provider)
		if err := h.client.Status().Update(context.Background(), live); err != nil {
			t.Fatal(err)
		}
	}
	_, err := h.source.ResolveConnectionCredential(context.Background(), h.request())
	if err == nil || (!strings.Contains(err.Error(), "re-linked") && !strings.Contains(err.Error(), "does not match the endpoint declared")) {
		t.Fatalf("credential from a retargeted re-consent must not be paired with the old destination: err = %v", err)
	}
}

func TestRefreshScopeFieldPresenceIsHonored(t *testing.T) {
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
	h.refresher.response = connectors.TokenResponse{AccessToken: "gho_omitted", TokenType: "bearer", ExpiresAt: h.now.Add(time.Hour)}
	got, err := h.source.ResolveConnectionCredential(context.Background(), h.request())
	if err != nil || got.AccessToken != "gho_omitted" {
		t.Fatalf("omitted scope refresh = %+v err = %v", got, err)
	}
	if live := h.reload(); strings.Join(live.Status.GrantedScopes, ",") != "read:user" {
		t.Fatalf("granted scopes after omitted-scope refresh = %v, want the previous grant inherited", live.Status.GrantedScopes)
	}

	h.put(store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr_old", ExpiresAt: h.now.Add(-time.Minute), Scopes: []string{"read:user"}})
	h.refresher.response = connectors.TokenResponse{AccessToken: "gho_empty", TokenType: "bearer", ExpiresAt: h.now.Add(time.Hour), ScopePresent: true}
	if _, err := h.source.ResolveConnectionCredential(context.Background(), h.request()); err == nil || !strings.Contains(err.Error(), "no longer covers") {
		t.Fatalf("explicit empty scope refresh err = %v, want fail closed", err)
	}
	if live := h.reload(); strings.Join(live.Status.GrantedScopes, ",") != "" || live.Status.State != corev1alpha1.ConnectionStatePending {
		t.Fatalf("status after empty grant = %+v", live.Status)
	}
}

func TestRevocationVerdictSurvivesUnrelatedStatusWrite(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr_old", ExpiresAt: h.now.Add(-time.Minute)})
	h.refresher.err = &connectors.OAuthError{StatusCode: 400, Code: "invalid_grant"}
	h.refresher.onRefresh = func() {
		live := h.reload()
		touched := metav1.NewTime(h.now)
		live.Status.LastRefreshTime = &touched
		if err := h.client.Status().Update(context.Background(), live); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.source.ResolveConnectionCredential(context.Background(), h.request()); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("resolve after invalid_grant err = %v", err)
	}
	live := h.reload()
	ready := meta.FindStatusCondition(live.Status.Conditions, corev1alpha1.ConnectionConditionReady)
	if live.Status.State != corev1alpha1.ConnectionStateRevoked || ready == nil || ready.Reason != corev1alpha1.ConnectionReasonRevoked {
		t.Fatalf("a verdict that lost only to an unrelated status write must still land: %+v", live.Status)
	}
}

func TestRefreshWaiterReturnsWhenItsContextEnds(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr_old", ExpiresAt: h.now.Add(-time.Minute)})
	release := make(chan struct{})
	started := make(chan struct{})
	h.refresher.response = connectors.TokenResponse{AccessToken: "gho_new", RefreshToken: "ghr_new", TokenType: "bearer", ExpiresAt: h.now.Add(time.Hour)}
	h.refresher.onRefresh = func() {
		close(started)
		<-release
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := h.source.ResolveConnectionCredential(ctx, h.request())
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "abandoned") {
			t.Fatalf("cancelled waiter err = %v", err)
		}
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("a cancelled caller must not wait for the detached refresh")
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := h.source.ResolveConnectionCredential(context.Background(), h.request())
		if err == nil && got.AccessToken == "gho_new" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("refresh after cancelled waiter = %+v err = %v", got, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRefreshLosingToDisconnectRevokesRotatedTokens(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr_old", ExpiresAt: h.now.Add(-time.Minute)})
	h.refresher.response = connectors.TokenResponse{AccessToken: "gho_rotated", RefreshToken: "ghr_rotated", TokenType: "bearer", ExpiresAt: h.now.Add(time.Hour)}
	live := h.reload()
	h.refresher.onRefresh = func() {
		if err := h.store.TombstoneConnectorCustody(context.Background(), string(live.UID)); err != nil {
			t.Fatal(err)
		}
	}
	requireDisconnectedDuringRefresh(t, h)
	h.refresher.mu.Lock()
	revoked := strings.Join(h.refresher.revoked, ",")
	h.refresher.mu.Unlock()
	if revoked != "ghr_rotated,gho_rotated" {
		t.Fatalf("revoked = %q, want the rotated pair, refresh token first", revoked)
	}
	ref, _ := connectors.CredentialRef(live)
	if held, err := h.store.GetConnectorCredential(context.Background(), ref); err != nil || held.AccessToken != "gho_old" {
		t.Fatalf("custody = %+v err = %v, want the fenced material untouched for disconnect to revoke", held, err)
	}
}

func TestRefreshWithinSkewIsSealedButNotReleased(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr_old", ExpiresAt: h.now.Add(-time.Minute)})
	h.refresher.response = connectors.TokenResponse{AccessToken: "gho_short", RefreshToken: "ghr_short", TokenType: "bearer", ExpiresAt: h.now.Add(30 * time.Second)}
	if _, err := h.source.ResolveConnectionCredential(context.Background(), h.request()); err == nil || !strings.Contains(err.Error(), "refresh window") {
		t.Fatalf("in-skew refresh err = %v", err)
	}
	live := h.reload()
	ref, _ := connectors.CredentialRef(live)
	if held, err := h.store.GetConnectorCredential(context.Background(), ref); err != nil || held.AccessToken != "gho_short" || held.RefreshToken != "ghr_short" {
		t.Fatalf("custody after in-skew refresh = %+v err = %v, want the rotated pair sealed", held, err)
	}
}

func TestRevocationVerdictYieldsToReconsentAfterShred(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr_old", ExpiresAt: h.now.Add(-time.Minute)})
	h.refresher.err = &connectors.OAuthError{StatusCode: 400, Code: "invalid_grant"}
	live := h.reload()
	ref, _ := connectors.CredentialRef(live)
	h.refresher.onRefresh = func() {
		if err := h.store.ShredConnectorCredential(context.Background(), string(live.UID), 1); err != nil {
			t.Fatal(err)
		}
		h.put(store.ConnectorCredential{AccessToken: "gho_new", RefreshToken: "ghr_new"})
		fresh := h.reload()
		linked := metav1.NewTime(h.now)
		fresh.Status.LinkedAt = &linked
		if err := h.client.Status().Update(context.Background(), fresh); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.source.ResolveConnectionCredential(context.Background(), h.request()); err == nil || !strings.Contains(err.Error(), "re-linked") {
		t.Fatalf("resolve during re-consent err = %v, want refusal", err)
	}
	if held, err := h.store.GetConnectorCredential(context.Background(), ref); err != nil || held.Version != 2 || held.AccessToken != "gho_new" {
		t.Fatalf("custody = %+v err = %v, want the re-consented row at a fresh version", held, err)
	}
	if fresh := h.reload(); fresh.Status.State != corev1alpha1.ConnectionStateReady {
		t.Fatalf("a verdict judged against the shredded row must not overwrite the fresh link: %+v", fresh.Status)
	}
}

func TestRefreshLosingToDisconnectSkipsMovedRevocationEndpoint(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr_old", ExpiresAt: h.now.Add(-time.Minute), RevocationDigest: "revocation-at-consent"})
	h.refresher.response = connectors.TokenResponse{AccessToken: "gho_rotated", RefreshToken: "ghr_rotated", TokenType: "bearer", ExpiresAt: h.now.Add(time.Hour)}
	live := h.reload()
	h.refresher.onRefresh = func() {
		if err := h.store.TombstoneConnectorCustody(context.Background(), string(live.UID)); err != nil {
			t.Fatal(err)
		}
	}
	requireDisconnectedDuringRefresh(t, h)
	h.refresher.mu.Lock()
	revoked := strings.Join(h.refresher.revoked, ",")
	h.refresher.mu.Unlock()
	if revoked != "" {
		t.Fatalf("revoked = %q, want nothing sent to a revocation authority the material was not sealed under", revoked)
	}
}

func TestRefreshHorizonCoversTheToolTimeout(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_short", RefreshToken: "ghr", TokenType: "bearer", ExpiresAt: h.now.Add(3 * time.Minute)})
	req := h.request()
	if got, err := h.source.ResolveConnectionCredential(context.Background(), req); err != nil || got.AccessToken != "gho_short" || h.refresher.calls.Load() != 0 {
		t.Fatalf("default 30s timeout: got %+v err = %v calls = %d, want the token released unrefreshed", got, err, h.refresher.calls.Load())
	}
	provider := &corev1alpha1.ConnectorProvider{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "github"}, provider); err != nil {
		t.Fatal(err)
	}
	for i := range provider.Spec.Tools {
		if provider.Spec.Tools[i].Name == "gh_search" && provider.Spec.Tools[i].HTTP != nil {
			provider.Spec.Tools[i].HTTP.Timeout = &metav1.Duration{Duration: 5 * time.Minute}
		}
	}
	if err := h.client.Update(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	req.Tool.Timeout, req.Tool.TimeoutSet = 5*time.Minute, true
	if got, err := h.source.ResolveConnectionCredential(context.Background(), req); err != nil || got.AccessToken != "gho_new" || h.refresher.calls.Load() != 1 {
		t.Fatalf("5m timeout: got %+v err = %v calls = %d, want a refresh before release", got, err, h.refresher.calls.Load())
	}
}

func (h *harness) curateSlowTool() outboundaccess.ConnectionCredentialRequest {
	h.t.Helper()
	provider := &corev1alpha1.ConnectorProvider{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "github"}, provider); err != nil {
		h.t.Fatal(err)
	}
	provider.Spec.Tools = append(provider.Spec.Tools, corev1alpha1.ConnectorTool{
		Name: "gh_slow", Class: corev1alpha1.ConnectorToolClassRead, Source: corev1alpha1.ConnectorToolSourceHTTP, Description: "slow",
		HTTP: &corev1alpha1.ConnectorHTTPTool{URL: "https://api.github.com/slow", Method: "GET", Timeout: &metav1.Duration{Duration: 10 * time.Minute}},
	})
	if err := h.client.Update(context.Background(), provider); err != nil {
		h.t.Fatal(err)
	}
	live := h.reload()
	live.Status.Consent = connectors.ConsentFor(provider)
	if err := h.client.Status().Update(context.Background(), live); err != nil {
		h.t.Fatal(err)
	}
	req := h.request()
	req.Tool = outboundaccess.ToolBinding{Name: "gh_slow", URL: "https://api.github.com/slow", Method: "GET", Class: corev1alpha1.AgentRuntimeBrokeredToolClassRead, Timeout: 10 * time.Minute, TimeoutSet: true}
	return req
}

func TestSharedRefreshIsJudgedAgainstEachCallersHorizon(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr_old", ExpiresAt: h.now.Add(30 * time.Second)})
	h.refresher.response = connectors.TokenResponse{AccessToken: "gho_5m", RefreshToken: "ghr_5m", TokenType: "bearer", ExpiresAt: h.now.Add(5 * time.Minute)}
	slow := h.curateSlowTool()
	joined := make(chan error, 1)
	entered := make(chan struct{}, 2)
	h.source.flightEntered = func() { entered <- struct{}{} }
	h.refresher.onRefresh = func() {
		go func() {
			_, err := h.source.ResolveConnectionCredential(context.Background(), slow)
			joined <- err
		}()
		for range 2 {
			<-entered
		}
	}
	if got, err := h.source.ResolveConnectionCredential(context.Background(), h.request()); err != nil || got.AccessToken != "gho_5m" {
		t.Fatalf("short request = %+v err = %v, want the five-minute token", got, err)
	}
	select {
	case err := <-joined:
		if err == nil || !strings.Contains(err.Error(), "request timeout") {
			t.Fatalf("long request joining the flight err = %v, want refusal against its own horizon", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the long request never returned")
	}
	if calls := h.refresher.calls.Load(); calls != 1 {
		t.Fatalf("refresh calls = %d, want the one shared flight", calls)
	}
}

func TestNonrefreshableTokenWithinHorizonKeepsTheLinkReady(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_5m", TokenType: "bearer", ExpiresAt: h.now.Add(5 * time.Minute)})
	slow := h.curateSlowTool()
	if _, err := h.source.ResolveConnectionCredential(context.Background(), slow); err == nil || !strings.Contains(err.Error(), "shorten the tool timeout") {
		t.Fatalf("long request err = %v, want a request-specific refusal", err)
	}
	if live := h.reload(); live.Status.State != corev1alpha1.ConnectionStateReady {
		t.Fatalf("state = %q, want the link left Ready", live.Status.State)
	}
	if got, err := h.source.ResolveConnectionCredential(context.Background(), h.request()); err != nil || got.AccessToken != "gho_5m" {
		t.Fatalf("short request = %+v err = %v, want the still-valid token", got, err)
	}
	if h.refresher.calls.Load() != 0 {
		t.Fatal("a token without a refresh token must not be refreshed")
	}
}

type flakyCustody struct {
	*storageclient.Client
	failReplace bool
	failRetire  int
	blockRetire bool
	retires     int
}

func (f *flakyCustody) ReplaceConnectorCredential(ctx context.Context, ref store.ConnectorCredentialRef, credential store.ConnectorCredential, expectedVersion int64) error {
	if f.failReplace {
		f.failReplace = false
		return errors.New("database is locked")
	}
	return f.Client.ReplaceConnectorCredential(ctx, ref, credential, expectedVersion)
}

func (f *flakyCustody) RetireConnectorCredential(ctx context.Context, ref store.ConnectorCredentialRef, credential store.ConnectorCredential) error {
	f.retires++
	if f.blockRetire {
		<-ctx.Done()
		return ctx.Err()
	}
	if f.failRetire > 0 {
		f.failRetire--
		return errors.New("database is locked")
	}
	return f.Client.RetireConnectorCredential(ctx, ref, credential)
}

func TestRefreshKeepsIssuedMaterialWhenCustodyWriteFails(t *testing.T) {
	previous := refreshRetireBackoff
	refreshRetireBackoff = time.Millisecond
	t.Cleanup(func() { refreshRetireBackoff = previous })
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr_old", TokenType: "bearer", ExpiresAt: h.now.Add(30 * time.Second), Scopes: []string{"repo"}})
	flaky := &flakyCustody{Client: h.store, failReplace: true, failRetire: 1}
	h.source.Credentials = flaky
	if _, err := h.source.ResolveConnectionCredential(context.Background(), h.request()); err == nil {
		t.Fatal("a refresh custody could not store must fail the call")
	}
	ref, _ := connectors.CredentialRef(h.connection)
	retired, err := h.store.ListRetiredConnectorCredentials(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	kept := false
	for _, credential := range retired {
		kept = kept || (credential.AccessToken == "gho_new" && credential.RefreshToken == "ghr_new")
	}
	if !kept || flaky.retires != 2 {
		t.Fatalf("retired = %+v retires = %d, want the issued pair retired after one retry", retired, flaky.retires)
	}
}

func TestRefreshRetirementIsBounded(t *testing.T) {
	previousTimeout, previousBackoff := refreshRetireTimeout, refreshRetireBackoff
	refreshRetireTimeout, refreshRetireBackoff = 50*time.Millisecond, time.Millisecond
	t.Cleanup(func() { refreshRetireTimeout, refreshRetireBackoff = previousTimeout, previousBackoff })
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr_old", TokenType: "bearer", ExpiresAt: h.now.Add(30 * time.Second), Scopes: []string{"repo"}})
	flaky := &flakyCustody{Client: h.store, failReplace: true, blockRetire: true}
	h.source.Credentials = flaky
	done := make(chan error, 1)
	go func() {
		_, err := h.source.ResolveConnectionCredential(context.Background(), h.request())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a refresh custody could not store must fail the call")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a blocked retirement held the refresh flight")
	}
	if flaky.retires == 0 {
		t.Fatal("the issued pair was never offered to retirement custody")
	}
}

type disconnectingCustody struct {
	*storageclient.Client
	onRead func()
}

func (d *disconnectingCustody) GetConnectorCredential(ctx context.Context, ref store.ConnectorCredentialRef) (store.ConnectorCredential, error) {
	credential, err := d.Client.GetConnectorCredential(ctx, ref)
	if d.onRead != nil {
		d.onRead()
		d.onRead = nil
	}
	return credential, err
}

func TestResolveRechecksDisconnectAfterReadingCustody(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_fresh", RefreshToken: "ghr", TokenType: "bearer", ExpiresAt: h.now.Add(2 * time.Hour)})
	h.source.Credentials = &disconnectingCustody{Client: h.store, onRead: func() {
		live := h.reload()
		live.Finalizers = append(live.Finalizers, "orka.ai/test-hold")
		if err := h.client.Update(context.Background(), live); err != nil {
			t.Fatal(err)
		}
		if err := h.client.Delete(context.Background(), live); err != nil {
			t.Fatal(err)
		}
	}}
	if got, err := h.source.ResolveConnectionCredential(context.Background(), h.request()); err == nil {
		t.Fatalf("credential = %+v, want refusal once the disconnect began", got)
	}
}

func TestRequireLiveConnectionRechecksTheFrozenLink(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_fresh", RefreshToken: "ghr", TokenType: "bearer", ExpiresAt: h.now.Add(2 * time.Hour)})
	custody := &countingCustody{Client: h.store}
	h.source.Credentials = custody
	request := h.request()
	if err := h.source.RequireLiveConnection(context.Background(), request); err != nil {
		t.Fatalf("live link: %v", err)
	}
	relinked := request
	relinked.Frozen.GrantSequence++
	if err := h.source.RequireLiveConnection(context.Background(), relinked); err == nil {
		t.Fatal("a link re-consented since dispatch must be refused")
	}
	if custody.reads == 0 {
		t.Fatal("a live link is checked against the grant custody holds")
	}
	live := h.reload()
	live.Finalizers = append(live.Finalizers, "orka.ai/test-hold")
	if err := h.client.Update(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Delete(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	before := custody.reads
	if err := h.source.RequireLiveConnection(context.Background(), request); err == nil || !strings.Contains(err.Error(), "being deleted") {
		t.Fatalf("unlinked: err = %v, want refusal", err)
	}
	if custody.reads != before {
		t.Fatalf("custody reads for a link being deleted = %d, want none", custody.reads-before)
	}
}

func TestRequireLiveConnectionTrustsCustodyOverLaggingStatus(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr", TokenType: "bearer", ExpiresAt: h.now.Add(2 * time.Hour)})
	request := h.request()
	used, err := h.source.ResolveConnectionCredential(context.Background(), request)
	if err != nil || used.AccessToken != "gho_old" {
		t.Fatalf("credential = %+v, %v", used, err)
	}
	ref, err := connectors.CredentialRef(h.reload())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.PutConnectorCredential(context.Background(), ref, store.ConnectorCredential{AccessToken: "gho_new", TokenType: "bearer"}); err != nil {
		t.Fatal(err)
	}
	if h.reload().Status.GrantSequence != request.Frozen.GrantSequence {
		t.Fatal("the status must still show the frozen grant")
	}
	err = h.source.RequireLiveConnection(context.Background(), request)
	if refusal, ok := errors.AsType[*outboundaccess.ConnectionRequiredError](err); !ok ||
		refusal.Kind != outboundaccess.ConnectionRelinked || !strings.Contains(err.Error(), "re-linked") {
		t.Fatalf("err = %v, want the stale grant refused as a re-linked Connection", err)
	}

	h.source.Credentials = grantlessCustody{h.store}
	if err := h.source.RequireLiveConnection(context.Background(), h.request()); err == nil {
		t.Fatal("custody without a grant read must fail closed")
	}
}

type grantlessCustody struct{ store.ConnectorCredentialStore }

type countingCustody struct {
	*storageclient.Client
	reads int
}

func (c *countingCustody) GetConnectorCredential(ctx context.Context, ref store.ConnectorCredentialRef) (store.ConnectorCredential, error) {
	c.reads++
	return c.Client.GetConnectorCredential(ctx, ref)
}

func (c *countingCustody) GetConnectorCredentialGrant(ctx context.Context, ref store.ConnectorCredentialRef) (store.ConnectorGrant, error) {
	c.reads++
	return c.Client.GetConnectorCredentialGrant(ctx, ref)
}

func TestResolveRefusesWritesOnReadOnlyLinksBeforeCustody(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr_old", TokenType: "bearer", ExpiresAt: h.now.Add(30 * time.Second), Scopes: []string{"repo"}})
	counting := &countingCustody{Client: h.store}
	h.source.Credentials = counting
	req := h.request()
	req.Tool.Class = corev1alpha1.AgentRuntimeBrokeredToolClassWrite
	if _, err := h.source.ResolveConnectionCredential(context.Background(), req); err == nil || !strings.Contains(err.Error(), "readOnly") {
		t.Fatalf("err = %v, want the readOnly refusal", err)
	}
	if counting.reads != 0 || h.refresher.calls.Load() != 0 {
		t.Fatalf("custody reads = %d refreshes = %d, want none", counting.reads, h.refresher.calls.Load())
	}
}

func TestResolveGitHubPersonCredential(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if _, err := h.source.ResolveGitHubPersonCredential(ctx, testNamespace, "github", testIssuer, "someone-else", time.Minute); !errors.Is(err, ErrNoPersonConnection) {
		t.Fatalf("another person's link err = %v", err)
	}
	h.put(store.ConnectorCredential{AccessToken: "gho_fresh", RefreshToken: "ghr", TokenType: "bearer", ExpiresAt: h.now.Add(2 * time.Hour)})
	got, err := h.source.ResolveGitHubPersonCredential(ctx, testNamespace, "github", testIssuer, testSubject, 10*time.Minute)
	if err != nil || got.AccessToken != "gho_fresh" || h.refresher.calls.Load() != 0 {
		t.Fatalf("fresh credential = %+v err = %v calls = %d", got, err, h.refresher.calls.Load())
	}
	h.put(store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr_old", TokenType: "bearer", ExpiresAt: h.now.Add(5 * time.Minute)})
	got, err = h.source.ResolveGitHubPersonCredential(ctx, testNamespace, "github", testIssuer, testSubject, 10*time.Minute)
	if err != nil || got.AccessToken != "gho_new" || h.refresher.calls.Load() != 1 {
		t.Fatalf("refreshed credential = %+v err = %v calls = %d", got, err, h.refresher.calls.Load())
	}
	live := h.reload()
	live.Status.Conditions[0].Status = metav1.ConditionFalse
	if err := h.client.Status().Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	if _, err := h.source.ResolveGitHubPersonCredential(ctx, testNamespace, "github", testIssuer, testSubject, time.Minute); err == nil {
		t.Fatal("a link that is not ready must not serve a credential")
	}
}

func TestResolveGitHubPersonCredentialRefusesOtherIssuers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.put(store.ConnectorCredential{AccessToken: "gho_fresh", RefreshToken: "ghr", TokenType: "bearer", ExpiresAt: h.now.Add(2 * time.Hour)})
	provider := &corev1alpha1.ConnectorProvider{}
	if err := h.client.Get(ctx, types.NamespacedName{Namespace: testNamespace, Name: "github"}, provider); err != nil {
		t.Fatal(err)
	}
	provider.Spec.OAuth.AuthorizeURL = "https://ghe.example.com/login/oauth/authorize"
	provider.Spec.OAuth.TokenURL = "https://ghe.example.com/login/oauth/access_token"
	if err := h.client.Update(ctx, provider); err != nil {
		t.Fatal(err)
	}
	if _, err := h.source.ResolveGitHubPersonCredential(ctx, testNamespace, "github", testIssuer, testSubject, time.Minute); err == nil {
		t.Fatal("a provider that does not issue github.com tokens must not serve them")
	}
}

func TestResolveGitHubPersonCredentialRefusesAReplacedConnection(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.put(store.ConnectorCredential{AccessToken: "gho_old", RefreshToken: "ghr_old", TokenType: "bearer", ExpiresAt: h.now.Add(5 * time.Minute)})
	h.refresher.onRefresh = func() {
		h.refresher.onRefresh = nil
		live := h.reload()
		replacement := live.DeepCopy()
		replacement.ResourceVersion = ""
		replacement.UID = "uid-2"
		if err := h.client.Delete(ctx, live); err != nil {
			t.Fatal(err)
		}
		if err := h.client.Create(ctx, replacement); err != nil {
			t.Fatal(err)
		}
		replacement.Status = live.Status
		if err := h.client.Status().Update(ctx, replacement); err != nil {
			t.Fatal(err)
		}
	}
	_, err := h.source.ResolveGitHubPersonCredential(ctx, testNamespace, "github", testIssuer, testSubject, 10*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("a replaced Connection served a credential: err = %v", err)
	}
}

func TestRequireLiveConnectionRechecksTheProvider(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_fresh", RefreshToken: "ghr", TokenType: "bearer", ExpiresAt: h.now.Add(2 * time.Hour)})
	request := h.request()
	if err := h.source.RequireLiveConnection(context.Background(), request); err != nil {
		t.Fatalf("unchanged provider: %v", err)
	}
	provider := &corev1alpha1.ConnectorProvider{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "github"}, provider); err != nil {
		t.Fatal(err)
	}
	provider.Spec.Tools = nil
	if err := h.client.Update(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	if err := h.source.RequireLiveConnection(context.Background(), request); err == nil {
		t.Fatal("a provider that no longer declares the Tool must refuse the exchanged credential")
	}
	if err := h.client.Delete(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	if err := h.source.RequireLiveConnection(context.Background(), request); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("deleted provider: err = %v", err)
	}
}

func TestRequireLiveConnectionRefusesScopesThePersonDidNotGrant(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_fresh", RefreshToken: "ghr", TokenType: "bearer", ExpiresAt: h.now.Add(2 * time.Hour)})
	request := h.request()
	provider := &corev1alpha1.ConnectorProvider{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "github"}, provider); err != nil {
		t.Fatal(err)
	}
	provider.Spec.OAuth.Scopes.Read = append(provider.Spec.OAuth.Scopes.Read, "read:org")
	if err := h.client.Update(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	if err := h.source.RequireLiveConnection(context.Background(), request); err == nil || !strings.Contains(err.Error(), "did not grant") {
		t.Fatalf("a newly required scope: err = %v, want a refusal", err)
	}
	h.put(store.ConnectorCredential{AccessToken: "gho_regranted", RefreshToken: "ghr", TokenType: "bearer", ExpiresAt: h.now.Add(2 * time.Hour), Scopes: []string{"read:org"}})
	live := h.reload()
	live.Status.GrantedScopes = append(live.Status.GrantedScopes, "read:org")
	if err := h.client.Status().Update(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	if err := h.source.RequireLiveConnection(context.Background(), h.request()); err != nil {
		t.Fatalf("a granted scope: %v", err)
	}
}

func TestRequireLiveConnectionRefusesARefreshThatNarrowedScopes(t *testing.T) {
	h := newHarness(t)
	provider := &corev1alpha1.ConnectorProvider{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "github"}, provider); err != nil {
		t.Fatal(err)
	}
	provider.Spec.OAuth.Scopes.Read = []string{"repo"}
	if err := h.client.Update(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	h.put(store.ConnectorCredential{AccessToken: "gho_wide", RefreshToken: "ghr", TokenType: "bearer", ExpiresAt: h.now.Add(2 * time.Hour), Scopes: []string{"repo"}})
	live := h.reload()
	live.Status.GrantedScopes = []string{"repo"}
	if err := h.client.Status().Update(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	request := h.request()
	if err := h.source.RequireLiveConnection(context.Background(), request); err != nil {
		t.Fatalf("covering scopes: %v", err)
	}
	ref, err := connectors.CredentialRef(h.reload())
	if err != nil {
		t.Fatal(err)
	}
	held, err := h.store.GetConnectorCredential(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	narrowed := held
	narrowed.AccessToken, narrowed.Scopes = "gho_narrow", []string{"public_repo"}
	if err := h.store.ReplaceConnectorCredential(context.Background(), ref, narrowed, held.Version); err != nil {
		t.Fatal(err)
	}
	if current := h.reload(); current.Status.GrantSequence != request.Frozen.GrantSequence || len(current.Status.GrantedScopes) != 1 {
		t.Fatal("the status must still show the frozen grant and its old scopes")
	}
	if err := h.source.RequireLiveConnection(context.Background(), request); err == nil || !strings.Contains(err.Error(), "did not grant") {
		t.Fatalf("err = %v, want the narrowed credential refused", err)
	}
}

type grantReadHook struct {
	*storageclient.Client
	during func()
}

func (h grantReadHook) GetConnectorCredentialGrant(ctx context.Context, ref store.ConnectorCredentialRef) (store.ConnectorGrant, error) {
	grant, err := h.Client.GetConnectorCredentialGrant(ctx, ref)
	if h.during != nil {
		h.during()
	}
	return grant, err
}

func TestRequireLiveConnectionRefusesALinkThatChangedDuringTheGrantRead(t *testing.T) {
	h := newHarness(t)
	h.put(store.ConnectorCredential{AccessToken: "gho_fresh", RefreshToken: "ghr", TokenType: "bearer", ExpiresAt: h.now.Add(2 * time.Hour)})
	request := h.request()
	h.source.Credentials = grantReadHook{Client: h.store, during: func() {
		live := h.reload()
		live.Status.State = corev1alpha1.ConnectionStateRevoked
		for i := range live.Status.Conditions {
			if live.Status.Conditions[i].Type == corev1alpha1.ConnectionConditionReady {
				live.Status.Conditions[i].Status = metav1.ConditionFalse
			}
		}
		if err := h.client.Status().Update(context.Background(), live); err != nil {
			t.Fatal(err)
		}
	}}
	if err := h.source.RequireLiveConnection(context.Background(), request); err == nil {
		t.Fatal("a link disconnected during the grant read must refuse the exchanged credential")
	}
}

func requireDisconnectedDuringRefresh(t *testing.T, h *harness) {
	t.Helper()
	_, err := h.source.ResolveConnectionCredential(context.Background(), h.request())
	var required *outboundaccess.ConnectionRequiredError
	if !errors.As(err, &required) || required.Kind != outboundaccess.ConnectionNoCredential {
		t.Fatalf("refresh after disconnect err = %v, want a ConnectionRequiredError for a missing credential", err)
	}
}
