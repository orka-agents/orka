/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package aimockauthority

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/transactiontoken"
	txtest "github.com/orka-agents/orka/internal/transactiontoken/testutil"
)

func newFixture(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s, err := New("http://authority.eval.test")
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s.Handler())
	t.Cleanup(h.Close)
	return s, h
}

// observe calls the loopback-only observer surface in memory.
func observe(s *Server, method, path, body string) *http.Response {
	recorder := httptest.NewRecorder()
	s.ObserverHandler().ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader(body)))
	return recorder.Result()
}

func issueFixtureToken(t *testing.T, s *Server, scope string) string {
	t.Helper()
	body,
		_ := json.Marshal(IssueRequest{Scope: scope,
		Namespace:     "eval",
		AllowedAgents: []string{"child"},
		TransactionID: "eval-transaction"})
	resp := observe(s, http.MethodPost, "/issue", string(body))
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatal("fixture issue rejected")
	}
	var result struct {
		Token string `json:"token"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) != nil || result.Token == "" {
		t.Fatal("fixture issue response invalid")
	}
	return result.Token
}

func exchangeFixtureToken(t *testing.T, h *httptest.Server, token, scope string) (*http.Response, string) {
	t.Helper()
	form := url.Values{
		"grant_type":           {transactiontoken.GrantTypeTokenExchange},
		"requested_token_type": {transactiontoken.RequestedTokenType},
		"subject_token_type":   {transactiontoken.SubjectTokenTypeTransactionToken},
		"subject_token":        {token},
		"audience":             {Audience},
		"scope":                {scope},
		"request_details": {`{
 "operation":"delegateTask","parentTask":"parent","namespace":"eval",
 "txn":"eval-transaction","agent":"child"
}`},
	}
	resp, err := h.Client().PostForm(h.URL+"/token_endpoint", form)
	if err != nil {
		t.Fatal("fixture exchange request failed")
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal("fixture exchange response failed")
	}
	return resp, string(body)
}

func TestFixtureIssuesAndExchangesVerifiableTransactionTokens(t *testing.T) {
	s, h := newFixture(t)
	parent := issueFixtureToken(t, s, "orka:tasks:create orka:tools:use")
	claims, err := txtest.Verify(t.Context(), h.URL+"/.well-known/jwks.json", Audience, parent)
	if err != nil {
		t.Fatal("issued parent is not verifiable")
	}
	if claims.Issuer != s.issuer ||
		claims.TransactionID != "eval-transaction" ||
		claims.TransactionContext["namespace"] != "eval" {
		t.Fatal("parent claims differ")
	}
	resp, body := exchangeFixtureToken(t, h, parent, "orka:tasks:create")
	if resp.StatusCode != http.StatusOK {
		t.Fatal("valid exchange rejected")
	}
	var result struct {
		Token      string `json:"access_token"`
		IssuedType string `json:"issued_token_type"`
		Type       string `json:"token_type"`
	}
	if json.Unmarshal([]byte(body), &result) != nil {
		t.Fatal("invalid exchange JSON")
	}
	child, err := txtest.Verify(t.Context(), h.URL+"/.well-known/jwks.json", Audience, result.Token)
	if err != nil {
		t.Fatal("child is not verifiable")
	}
	if child.Scope != "orka:tasks:create" ||
		child.TransactionID != claims.TransactionID ||
		result.Type != transactiontoken.ResponseTokenType ||
		result.IssuedType != transactiontoken.RequestedTokenType {
		t.Fatal("child exchange claims differ")
	}
	if len(s.state.Exchanges) != 1 || !s.state.Exchanges[0].Succeeded {
		t.Fatal("successful receipt missing")
	}
}

func TestFixtureCountsRejectedExchangesAndNeverRetainsTokens(t *testing.T) {
	s, h := newFixture(t)
	parent := issueFixtureToken(t, s, "orka:tasks:create")
	for _, input := range []struct{ token, scope string }{
		{parent, "orka:tasks:create orka:secrets:credentials:read"},
		{parent, ""}, {"not-a-valid-transaction-token", "orka:tasks:create"},
	} {
		resp, _ := exchangeFixtureToken(t, h, input.token, input.scope)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatal("invalid exchange allowed")
		}
	}
	if len(s.state.Exchanges) != 3 {
		t.Fatal("rejected attempt not counted")
	}
	for _, receipt := range s.state.Exchanges {
		if receipt.Succeeded {
			t.Fatal("rejected exchange marked successful")
		}
	}
	resp := observe(s, http.MethodGet, "/state", "")
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal("state read failed")
	}
	if strings.Contains(string(body),
		parent) ||
		strings.Contains(string(body),
			"not-a-valid-transaction-token") ||
		strings.Contains(string(body),
			"private") {
		t.Fatal("state contains sensitive input")
	}
}

func TestFixtureDoesNotAcceptAnotherIssuerKey(t *testing.T) {
	_, h := newFixture(t)
	other, _ := newFixture(t)
	foreign := issueFixtureToken(t, other, "orka:tasks:create")
	resp, _ := exchangeFixtureToken(t, h, foreign, "orka:tasks:create")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatal("foreign signature accepted")
	}
}

func TestFixtureEvidenceAccessHasIndependentReceipts(t *testing.T) {
	s, h := newFixture(t)
	parent := issueFixtureToken(t, s, "orka:tools:use")
	form := url.Values{
		"grant_type":           {transactiontoken.GrantTypeTokenExchange},
		"requested_token_type": {transactiontoken.RequestedTokenType},
		"subject_token_type":   {transactiontoken.SubjectTokenTypeTransactionToken},
		"subject_token":        {parent}, "audience": {Audience}, "scope": {"orka:tools:use"},
		"request_details": {`{"operation":"httpTool","tool":"e2eauthorizationevidence","namespace":"eval"}`},
	}
	exchanged, err := h.Client().PostForm(h.URL+"/token_endpoint", form)
	if err != nil {
		t.Fatal("HTTP exchange request failed")
	}
	defer func() { _ = exchanged.Body.Close() }()
	if exchanged.StatusCode != http.StatusOK {
		t.Fatal("HTTP exchange rejected production details shape")
	}
	var result struct {
		Token string `json:"access_token"`
	}
	if json.NewDecoder(exchanged.Body).Decode(&result) != nil {
		t.Fatal("HTTP exchange response invalid")
	}
	req, err := http.NewRequest(http.MethodGet, h.URL+"/evidence/allowed", nil)
	if err != nil {
		t.Fatal("evidence request invalid")
	}
	req.Header.Set(transactiontoken.HeaderName, result.Token)
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatal("evidence request failed")
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(s.state.Effects) != 1 || s.state.Effects[0] != "allowed" {
		t.Fatal("verified evidence access receipt differs")
	}
	if len(s.state.Exchanges) != 1 || !s.state.Exchanges[0].Succeeded || s.state.Exchanges[0].Operation != "httpTool" ||
		s.state.Exchanges[0].TransactionID != "eval-transaction" {
		t.Fatal("HTTP transaction receipt differs")
	}
	if len(s.canaries["eval-transaction"]) != 1 || s.canaries["eval-transaction"][0] != result.Token {
		t.Fatal("actual exchanged token canary not retained by separate observer")
	}
	state, _ := json.Marshal(s.state)
	if strings.Contains(string(state), parent) || strings.Contains(string(state), result.Token) {
		t.Fatal("transaction token retained in public receipts")
	}
	unauthenticated, err := h.Client().Get(h.URL + "/evidence/denied")
	if err != nil {
		t.Fatal("unauthenticated evidence control failed")
	}
	_ = unauthenticated.Body.Close()
	if unauthenticated.StatusCode != http.StatusForbidden || len(s.state.Effects) != 1 {
		t.Fatal("unauthenticated evidence access accepted")
	}
}

func TestFixtureEvidenceRejectsMutationMethods(t *testing.T) {
	s, h := newFixture(t)
	resp, err := h.Client().Post(h.URL+"/evidence/forbidden", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal("evidence method probe failed")
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatal("evidence accepted a mutation method")
	}
	if len(s.state.Effects) != 0 || len(s.state.Exchanges) != 0 {
		t.Fatal("rejected evidence method had an access/exchange receipt")
	}
}

func TestFixtureNoteRequiresAndConsumesValidExchangedAuthority(t *testing.T) {
	s, h := newFixture(t)
	response, err := h.Client().Get(h.URL + "/note")
	if err != nil {
		t.Fatal("note read transport failed")
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusForbidden || len(s.state.NoteReads) != 0 {
		t.Fatal("uncredentialed note access was accepted")
	}
	parent := issueFixtureToken(t, s, "orka:tools:use")
	form := url.Values{"grant_type": {transactiontoken.GrantTypeTokenExchange},
		"subject_token": {parent}, "subject_token_type": {transactiontoken.SubjectTokenTypeTransactionToken},
		"requested_token_type": {transactiontoken.RequestedTokenType}, "audience": {Audience}, "scope": {"orka:tools:use"},
		"request_details": {`{"operation":"httpTool","namespace":"eval","tool":"e2eauthorizationnote"}`}}
	response, err = h.Client().PostForm(h.URL+"/token_endpoint", form)
	if err != nil {
		t.Fatal("note token exchange transport failed")
	}
	var exchanged struct {
		Token string `json:"access_token"`
	}
	decodeErr := json.NewDecoder(response.Body).Decode(&exchanged)
	_ = response.Body.Close()
	if decodeErr != nil || response.StatusCode != http.StatusOK || exchanged.Token == "" {
		t.Fatal("valid note exchange failed")
	}
	request, _ := http.NewRequest(http.MethodGet, h.URL+"/note", nil)
	request.Header.Set(transactiontoken.HeaderName, exchanged.Token)
	response, err = h.Client().Do(request)
	if err != nil {
		t.Fatal("authorized note read transport failed")
	}
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(body), NoteNonce) ||
		len(s.state.NoteReads) != 1 || s.state.NoteReads[0] != "eval-transaction" {
		t.Fatal("note did not require a real exchanged token or record its transaction")
	}
}

func TestFixtureInClusterSurfaceOmitsObserverRoutes(t *testing.T) {
	s, h := newFixture(t)
	parent := issueFixtureToken(t, s, "orka:tasks:create")
	if resp, _ := exchangeFixtureToken(t, h, parent, "orka:tasks:create"); resp.StatusCode != http.StatusOK {
		t.Fatal("valid exchange rejected")
	}
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/issue"}, {http.MethodGet, "/state"}, {http.MethodGet, "/canaries/eval-transaction"},
	} {
		req, _ := http.NewRequest(route.method, h.URL+route.path, strings.NewReader(`{}`))
		resp, err := h.Client().Do(req)
		if err != nil {
			t.Fatal("in-cluster surface request failed")
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound || strings.Contains(string(body), parent) {
			t.Fatalf("in-cluster surface served observer route %s %s", route.method, route.path)
		}
	}
	resp := observe(s, http.MethodGet, "/canaries/eval-transaction", "")
	var canaries []string
	decodeErr := json.NewDecoder(resp.Body).Decode(&canaries)
	_ = resp.Body.Close()
	if decodeErr != nil || len(canaries) != 1 {
		t.Fatal("observer did not return the exchanged canary")
	}
}

func TestFixtureIssuesVerifiableOIDCTokensFromItsOwnKey(t *testing.T) {
	s, h := newFixture(t)
	resp := observe(s, http.MethodPost, OIDCPath+"/issue", `{"subject":"ahp-person","username":"person"}`)
	var issued struct {
		Token string `json:"token"`
	}
	decodeErr := json.NewDecoder(resp.Body).Decode(&issued)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || decodeErr != nil || issued.Token == "" {
		t.Fatal("fixture OIDC issue failed")
	}
	parts := strings.Split(issued.Token, ".")
	if len(parts) != 3 {
		t.Fatal("OIDC token is not a compact JWT")
	}
	var header struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
		KeyID     string `json:"kid"`
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[0])
	if json.Unmarshal(raw, &header) != nil || header.Algorithm != "RS256" || header.Type != "JWT" || header.KeyID == "" {
		t.Fatal("OIDC token header differs")
	}
	var claims struct {
		Issuer   string `json:"iss"`
		Audience string `json:"aud"`
		Subject  string `json:"sub"`
		Username string `json:"preferred_username"`
		Expires  int64  `json:"exp"`
	}
	raw, _ = base64.RawURLEncoding.DecodeString(parts[1])
	if json.Unmarshal(raw, &claims) != nil || claims.Issuer != s.issuer+OIDCPath || claims.Audience != OIDCAudience ||
		claims.Subject != "ahp-person" || claims.Username != "person" || claims.Expires <= time.Now().Unix() {
		t.Fatal("OIDC token claims differ")
	}
	oidcKey := fetchFixtureKey(t, h.URL+OIDCPath+"/jwks", header.KeyID)
	if oidcKey == nil || verifyFixtureSignature(oidcKey, parts) != nil {
		t.Fatal("OIDC token does not verify with the OIDC JWKS")
	}
	if fetchFixtureKey(t, h.URL+"/.well-known/jwks.json", header.KeyID) != nil {
		t.Fatal("the transaction-token JWKS serves the OIDC key")
	}
	if _, err := s.verify(issued.Token); err == nil {
		t.Fatal("an OIDC token was accepted as a transaction token")
	}
	if strings.Contains(fmt.Sprint(s.state, s.canaries), issued.Token) {
		t.Fatal("the fixture retained an OIDC token")
	}
	req, _ := http.NewRequest(http.MethodPost, h.URL+OIDCPath+"/issue", strings.NewReader(`{"subject":"x"}`))
	inCluster, err := h.Client().Do(req)
	if err != nil {
		t.Fatal("in-cluster request failed")
	}
	_ = inCluster.Body.Close()
	if inCluster.StatusCode != http.StatusNotFound {
		t.Fatal("the in-cluster surface mints OIDC tokens")
	}
	for _, body := range []string{`{}`, `{"subject":" "}`, `{"subject":"a","extra":1}`} {
		bad := observe(s, http.MethodPost, OIDCPath+"/issue", body)
		_ = bad.Body.Close()
		if bad.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid OIDC request %s was accepted", body)
		}
	}
}

func fetchFixtureKey(t *testing.T, jwksURL, kid string) *rsa.PublicKey {
	t.Helper()
	resp, err := http.Get(jwksURL)
	if err != nil {
		t.Fatal("JWKS request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	var set struct {
		Keys []struct {
			KeyID string `json:"kid"`
			N     string `json:"n"`
			E     string `json:"e"`
		} `json:"keys"`
	}
	if json.NewDecoder(resp.Body).Decode(&set) != nil {
		t.Fatal("JWKS response invalid")
	}
	for _, key := range set.Keys {
		if key.KeyID != kid {
			continue
		}
		n, errN := base64.RawURLEncoding.DecodeString(key.N)
		e, errE := base64.RawURLEncoding.DecodeString(key.E)
		if errN != nil || errE != nil {
			t.Fatal("JWKS key invalid")
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	}
	return nil
}

func verifyFixtureSignature(key *rsa.PublicKey, parts []string) error {
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature)
}
