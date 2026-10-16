/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package aimockauthority

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

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
