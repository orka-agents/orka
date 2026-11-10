package supervisor

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// subscriptionsResponder answers Subscriptions - List with two pages, the
// first subscription disabled, and anything else with a 404.
func subscriptionsResponder(t *testing.T, listed *atomic.Int32) func(harnessv2.MCPBrokerCallRequest) json.RawMessage {
	return func(request harnessv2.MCPBrokerCallRequest) json.RawMessage {
		forwarded, err := harnessv2.DecodeMCPForwardRequest(request.Call.Arguments)
		if err != nil {
			t.Errorf("forwarded arguments: %v", err)
		}
		response := harnessv2.MCPForwardResponse{Status: http.StatusOK, Headers: map[string]string{"Content-Type": "application/json"}}
		switch {
		case forwarded.Path == "/subscriptions" && !strings.Contains(forwarded.Query, "skiptoken"):
			listed.Add(1)
			response.Body = `{"value":[` +
				`{"id":"/subscriptions/11111111-1111-1111-1111-111111111111","subscriptionId":"11111111-1111-1111-1111-111111111111","tenantId":"22222222-2222-2222-2222-222222222222","displayName":"old","state":"Disabled"},` +
				`{"id":"/subscriptions/33333333-3333-3333-3333-333333333333","subscriptionId":"33333333-3333-3333-3333-333333333333","tenantId":"22222222-2222-2222-2222-222222222222","displayName":"main \"prod\"","state":"Enabled","managedByTenants":[{"tenantId":"44444444-4444-4444-4444-444444444444"}]},` +
				`{"subscriptionId":"../../etc","displayName":"not a subscription","state":"Enabled"}],` +
				`"nextLink":"https://management.azure.com/subscriptions?api-version=2022-12-01&$skiptoken=page2"}`
		case forwarded.Path == "/subscriptions":
			response.Body = `{"value":[{"subscriptionId":"55555555-5555-5555-5555-555555555555","tenantId":"22222222-2222-2222-2222-222222222222","displayName":"second page","state":"Enabled"}],` +
				`"nextLink":"https://elsewhere.example.com/subscriptions?page=3"}`
		default:
			response.Status = http.StatusNotFound
			response.Body = `{"error":{"code":"ResourceNotFound","message":"not in the fixture"}}`
		}
		raw, _ := json.Marshal(response)
		return raw
	}
}

func readAzureProfile(t *testing.T, fixture *azureGatewayFixture) azureProfile {
	t.Helper()
	path := filepath.Join(fixture.home, ".azure", azureProfileName)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("az's profile = %v, %v", info, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var profile azureProfile
	if err := json.Unmarshal(raw, &profile); err != nil {
		t.Fatalf("az's profile %q: %v", raw, err)
	}
	return profile
}

func TestAzureGatewaySignsAzInWhenThePromptStarts(t *testing.T) {
	var listed atomic.Int32
	fixture := newAzureGatewayFixtureSigningIn(t, false, subscriptionsResponder(t, &listed))
	fixture.waitForSignIn(t)
	profile := readAzureProfile(t, fixture)
	if len(profile.Subscriptions) != 3 {
		t.Fatalf("subscriptions = %+v", profile.Subscriptions)
	}
	for index, account := range profile.Subscriptions {
		if account.User != azureManagedIdentityUser || account.TenantID != azureZeroTenant || account.EnvironmentName != azureCloudName ||
			account.HomeTenantID == nil || *account.HomeTenantID != "22222222-2222-2222-2222-222222222222" {
			t.Fatalf("account %d = %+v", index, account)
		}
		// The first enabled subscription is the default, as az picks it.
		if account.IsDefault != (account.ID == "33333333-3333-3333-3333-333333333333") {
			t.Fatalf("account %d default = %v", index, account.IsDefault)
		}
	}
	if main := profile.Subscriptions[1]; main.Name != `main "prod"` || main.ManagedByTenants == nil ||
		len(*main.ManagedByTenants) != 1 || (*main.ManagedByTenants)[0].TenantID != "44444444-4444-4444-4444-444444444444" {
		t.Fatalf("main subscription = %+v", main)
	}
	calls := fixture.brokerCalls()
	if len(calls) != 2 {
		t.Fatalf("broker calls = %d, want two pages and no link off the origin", len(calls))
	}
	for _, call := range calls {
		if !strings.HasPrefix(call.Call.CallID, azureSignInCallPrefix) || call.Call.ToolName != testForwardTool {
			t.Fatalf("sign-in call = %s on %s", call.Call.CallID, call.Call.ToolName)
		}
	}
	// A later prompt start does not sign in again, so it keeps the agent's
	// own choices, such as az account set.
	fixture.gateway.signInAtPromptStart()
	fixture.waitForSignIn(t)
	if listed.Load() != 1 {
		t.Fatalf("subscription lists = %d", listed.Load())
	}
}

func TestAzureGatewayStartsWithThePlaceholderAccount(t *testing.T) {
	fixture := newAzureGatewayFixtureSigningIn(t, false, func(harnessv2.MCPBrokerCallRequest) json.RawMessage {
		return json.RawMessage(`{"isError":true,"code":"connection_required","error":"Azure is not linked for the person who asked. They need to run ` +
			"`orka connect azure -n team-a`" + `, then run this Task again."}`)
	})
	waitForAttempt(t, fixture)
	profile := readAzureProfile(t, fixture)
	if len(profile.Subscriptions) != 1 || profile.Subscriptions[0].ID != azureZeroTenant ||
		profile.Subscriptions[0].Name != azureTenantLevelAccountName || !profile.Subscriptions[0].IsDefault {
		t.Fatalf("placeholder profile = %+v", profile.Subscriptions)
	}
	// A command az sends under the placeholder tries again, and says why it
	// cannot sign in, instead of az's "Please run 'az login'".
	token := fixture.token(t)
	response, body := fixture.do(t, http.MethodGet, "/subscriptions/"+azureZeroTenant+"/resourcegroups?api-version=2024-11-01",
		http.Header{"Authorization": {"Bearer " + token}})
	if response.StatusCode != http.StatusForbidden || !strings.Contains(body, "OrkaConnectionRequired") || !strings.Contains(body, "orka connect azure -n team-a") {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	for _, call := range fixture.brokerCalls() {
		if strings.Contains(string(call.Call.Arguments), "resourcegroups") {
			t.Fatal("a request under the placeholder account reached the broker")
		}
	}
}

// waitForAttempt waits for the prompt start's sign-in attempt, which may
// fail, to end.
func waitForAttempt(t *testing.T, fixture *azureGatewayFixture) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		calls := len(fixture.brokerCalls())
		fixture.gateway.signInMu.Lock()
		attempt := fixture.gateway.signInAttempt
		fixture.gateway.signInMu.Unlock()
		switch {
		case attempt != nil:
			<-attempt.done
		case calls > 0:
			return
		case time.Now().After(deadline):
			t.Fatal("the prompt start did not try to sign az in")
		default:
			time.Sleep(time.Millisecond)
		}
	}
}

func TestAzureGatewaySignsInAgainForACommandUnderThePlaceholder(t *testing.T) {
	fixture := newAzureGatewayFixture(t)
	token := fixture.token(t)
	// The identity had no subscription when the prompt started.
	response, body := fixture.do(t, http.MethodGet, "/subscriptions/"+azureZeroTenant+"/resourcegroups", http.Header{"Authorization": {"Bearer " + token}})
	if response.StatusCode != http.StatusForbidden || !strings.Contains(body, "OrkaNoSubscription") {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	// Now it has one: the command signs az in again and asks to be rerun.
	var listed atomic.Int32
	fixture.mu.Lock()
	fixture.respond = subscriptionsResponder(t, &listed)
	fixture.mu.Unlock()
	response, body = fixture.do(t, http.MethodDelete, "/subscriptions/"+strings.ToUpper(azureZeroTenant[:8])+azureZeroTenant[8:]+"/resourcegroups/rg",
		http.Header{"Authorization": {"Bearer " + token}})
	if response.StatusCode != http.StatusConflict || !strings.Contains(body, "OrkaSignedIn") {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	if profile := readAzureProfile(t, fixture); len(profile.Subscriptions) != 3 {
		t.Fatalf("profile = %+v", profile.Subscriptions)
	}
	if listed.Load() != 1 {
		t.Fatalf("subscription lists = %d", listed.Load())
	}
	// A path that only starts like the placeholder is an ordinary request.
	response, _ = fixture.do(t, http.MethodGet, "/subscriptions/"+azureZeroTenant+"0/resourcegroups", http.Header{"Authorization": {"Bearer " + token}})
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d", response.StatusCode)
	}
}

func TestAzureGatewayPassesASignInRefusalThrough(t *testing.T) {
	fixture := newAzureGatewayFixture(t)
	fixture.mu.Lock()
	fixture.respond = func(harnessv2.MCPBrokerCallRequest) json.RawMessage {
		raw, _ := json.Marshal(harnessv2.MCPForwardResponse{Status: http.StatusForbidden, Headers: map[string]string{"Content-Type": "application/json"},
			Body: `{"error":{"code":"AuthorizationFailed","message":"no access"}}`})
		return raw
	}
	fixture.mu.Unlock()
	token := fixture.token(t)
	response, body := fixture.do(t, http.MethodGet, "/subscriptions/"+azureZeroTenant, http.Header{"Authorization": {"Bearer " + token}})
	if response.StatusCode != http.StatusForbidden || !strings.Contains(body, "AuthorizationFailed") {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	// A throttled discovery keeps Azure's pace for az's retry.
	fixture.mu.Lock()
	fixture.respond = func(harnessv2.MCPBrokerCallRequest) json.RawMessage {
		raw, _ := json.Marshal(harnessv2.MCPForwardResponse{Status: http.StatusTooManyRequests,
			Headers: map[string]string{"Content-Type": "application/json", "Retry-After": "17"},
			Body:    `{"error":{"code":"TooManyRequests","message":"slow down"}}`})
		return raw
	}
	fixture.mu.Unlock()
	response, body = fixture.do(t, http.MethodGet, "/subscriptions/"+azureZeroTenant, http.Header{"Authorization": {"Bearer " + token}})
	if response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") != "17" {
		t.Fatalf("status = %d, Retry-After = %q: %s", response.StatusCode, response.Header.Get("Retry-After"), body)
	}
}

// A prompt-start sign-in that runs after another sign-in succeeded does
// nothing, so it cannot overwrite a profile the agent changed since.
func TestAzureGatewayLatePromptStartSignInKeepsTheProfile(t *testing.T) {
	fixture := newAzureGatewayFixture(t)
	if _, err := fixture.gateway.signIn(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if calls := fixture.brokerCalls(); len(calls) != 0 {
		t.Fatalf("a late prompt-start sign-in made %d broker calls", len(calls))
	}
	if _, err := fixture.gateway.signIn(t.Context(), false); err != nil || len(fixture.brokerCalls()) != 1 {
		t.Fatalf("a sign-in for a command under the placeholder must run: %v", err)
	}
}

func TestAzureGatewaySignInOutsideAPromptIsRefused(t *testing.T) {
	fixture := newAzureGatewayFixture(t)
	token := fixture.token(t)
	fixture.session.deactivate(fixture.authorize.PromptID, harnessv2.RuntimeSessionStateIdle)
	response, body := fixture.do(t, http.MethodGet, "/subscriptions/"+azureZeroTenant+"/resourcegroups", http.Header{"Authorization": {"Bearer " + token}})
	if response.StatusCode != http.StatusForbidden || !strings.Contains(body, "OrkaNotAuthorized") {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	if calls := fixture.brokerCalls(); len(calls) != 0 {
		t.Fatal("a sign-in outside the prompt reached the broker")
	}
}

// The child owns ~/.azure while the session runs, and the supervisor is
// root, so a link the child plants must not redirect the profile write.
func TestAzureGatewayProfileWritesDoNotFollowLinks(t *testing.T) {
	fixture := newAzureGatewayFixture(t)
	dir := filepath.Join(fixture.home, ".azure")
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(dir, azureProfileName)
	if err := os.Remove(profile); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, profile); err != nil {
		t.Fatal(err)
	}
	token := fixture.token(t)
	if response, body := fixture.do(t, http.MethodGet, "/subscriptions/"+azureZeroTenant, http.Header{"Authorization": {"Bearer " + token}}); response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	if raw, err := os.ReadFile(victim); err != nil || string(raw) != "keep" {
		t.Fatalf("the link's target = %q, %v", raw, err)
	}
	readAzureProfile(t, fixture)

	// Swapping the whole directory for a link leaves the write in the
	// directory the session started with.
	if err := os.Rename(dir, filepath.Join(fixture.home, ".azure-moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, dir); err != nil {
		t.Fatal(err)
	}
	if response, body := fixture.do(t, http.MethodGet, "/subscriptions/"+azureZeroTenant, http.Header{"Authorization": {"Bearer " + token}}); response.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d: %s", response.StatusCode, body)
	}
	if _, err := os.Lstat(filepath.Join(outside, azureProfileName)); !os.IsNotExist(err) {
		t.Fatalf("the profile was written through the link: %v", err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 1 {
		t.Fatalf("the link's target holds %v, %v", entries, err)
	}
}
