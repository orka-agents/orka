package supervisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const (
	azureProfileName             = "azureProfile.json"
	azureZeroTenant              = "00000000-0000-0000-0000-000000000000"
	azureTenantLevelAccountName  = "N/A(tenant level account)"
	azureSignInPendingName       = "Orka is still signing az in; run the command again"
	azureSubscriptionsAPIVersion = "2022-12-01"
	azureSignInTimeout           = time.Minute
	maxAzureSubscriptionPages    = 20
	maxAzureSubscriptions        = 2000
	azureSignInCallPrefix        = "az-signin-"
)

var azureSubscriptionIDPattern = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)

type azureProfileUser struct {
	Name             string `json:"name"`
	Type             string `json:"type"`
	AssignedIdentity string `json:"assignedIdentityInfo"`
}

type azureProfileTenant struct {
	TenantID string `json:"tenantId"`
}

type azureProfileAccount struct {
	ID               string                `json:"id"`
	Name             string                `json:"name"`
	State            string                `json:"state"`
	User             azureProfileUser      `json:"user"`
	IsDefault        bool                  `json:"isDefault"`
	TenantID         string                `json:"tenantId"`
	EnvironmentName  string                `json:"environmentName"`
	HomeTenantID     *string               `json:"homeTenantId,omitempty"`
	ManagedByTenants *[]azureProfileTenant `json:"managedByTenants,omitempty"`
}

type azureProfile struct {
	Subscriptions []azureProfileAccount `json:"subscriptions"`
}

var azureManagedIdentityUser = azureProfileUser{Name: "systemAssignedIdentity", Type: "servicePrincipal", AssignedIdentity: "MSI"}

func placeholderAzureProfile(name string) azureProfile {
	return azureProfile{Subscriptions: []azureProfileAccount{{
		ID: azureZeroTenant, Name: name, State: "Enabled", User: azureManagedIdentityUser,
		IsDefault: true, TenantID: azureZeroTenant, EnvironmentName: azureCloudName,
	}}}
}

type azureSubscriptionPage struct {
	Value []struct {
		ID               string  `json:"id"`
		SubscriptionID   string  `json:"subscriptionId"`
		TenantID         *string `json:"tenantId"`
		DisplayName      string  `json:"displayName"`
		State            string  `json:"state"`
		ManagedByTenants *[]struct {
			TenantID string `json:"tenantId"`
		} `json:"managedByTenants"`
	} `json:"value"`
	NextLink string `json:"nextLink"`
}

var errAzureSignInPromptEnded = fmt.Errorf("%w: the prompt the sign-in started under ended", errMCPCallNotAuthorized)

type azureSignInToolError struct{ result json.RawMessage }

func (e *azureSignInToolError) Error() string { return "the sign-in read was refused" }

type azureSignInUpstreamError struct {
	status  int
	headers map[string]string
	body    string
}

func (e *azureSignInUpstreamError) Error() string {
	return fmt.Sprintf("Resource Manager answered the sign-in read with status %d", e.status)
}

type azureSignIn struct {
	done          chan struct{}
	subscriptions int
	err           error
}

func (g *azureGateway) signInAtPromptStart() {
	if g.session.approvalRequired(g.toolName) {
		return
	}
	g.signInMu.Lock()
	skip := g.signedIn || g.autoSignInTried
	g.autoSignInTried = true
	g.signInMu.Unlock()
	if skip {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(g.ctx, azureSignInTimeout)
		defer cancel()
		_, _ = g.signIn(ctx, true, false)
	}()
}

func (g *azureGateway) signIn(ctx context.Context, unlessSignedIn, holdsSlot bool) (int, error) {
	g.signInMu.Lock()
	if unlessSignedIn && g.signedIn {
		g.signInMu.Unlock()
		return 0, nil
	}
	attempt := g.signInAttempt
	if attempt == nil {
		attempt = &azureSignIn{done: make(chan struct{})}
		g.signInAttempt = attempt
		g.signInMu.Unlock()
		attempt.subscriptions, attempt.err = g.discoverAndWriteProfile(ctx, holdsSlot)
		g.signInMu.Lock()
		g.signInAttempt = nil
		if attempt.err == nil {
			g.signedIn = true
		}
		g.signInMu.Unlock()
		close(attempt.done)
		return attempt.subscriptions, attempt.err
	}
	g.signInMu.Unlock()
	select {
	case <-attempt.done:
		return attempt.subscriptions, attempt.err
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (g *azureGateway) discoverAndWriteProfile(ctx context.Context, holdsSlot bool) (int, error) {
	if g.session.proxy != nil && !holdsSlot {
		if !tryAcquireMCPSlot(g.session.proxy.slots) {
			return 0, errMCPSessionAtCapacity
		}
		defer releaseMCPSlot(g.session.proxy.slots)
	}
	var prompt harnessv2.PromptID
	admit := func(current harnessv2.PromptID) error {
		if prompt == "" {
			prompt = current
			return nil
		}
		if current != prompt {
			return errAzureSignInPromptEnded
		}
		return nil
	}
	var accounts []azureProfileAccount
	request := harnessv2.MCPForwardRequest{Method: http.MethodGet, Path: "/subscriptions", Query: "api-version=" + azureSubscriptionsAPIVersion}
	for page := 0; page < maxAzureSubscriptionPages && len(accounts) < maxAzureSubscriptions; page++ {
		found, next, err := g.readSubscriptionPage(ctx, request, admit)
		if err != nil {
			return 0, err
		}
		accounts = append(accounts, found...)
		if next == nil {
			break
		}
		request = *next
	}
	if len(accounts) > maxAzureSubscriptions {
		accounts = accounts[:maxAzureSubscriptions]
	}
	profile := placeholderAzureProfile(azureTenantLevelAccountName)
	if len(accounts) > 0 {
		chosen := 0
		for index := range accounts {
			if accounts[index].State == "Enabled" {
				chosen = index
				break
			}
		}
		accounts[chosen].IsDefault = true
		profile.Subscriptions = accounts
	}
	if !g.session.promptRunning(prompt) {
		return 0, errAzureSignInPromptEnded
	}
	if err := g.writeProfile(profile, true); err != nil {
		return 0, err
	}
	return len(accounts), nil
}

func (g *azureGateway) readSubscriptionPage(
	ctx context.Context, request harnessv2.MCPForwardRequest, admit func(harnessv2.PromptID) error,
) ([]azureProfileAccount, *harnessv2.MCPForwardRequest, error) {
	if err := request.Validate(); err != nil {
		return nil, nil, err
	}
	arguments, err := json.Marshal(request)
	if err != nil {
		return nil, nil, err
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, nil, err
	}
	response, err := g.session.callBrokeredTool(ctx, azureSignInCallPrefix+hex.EncodeToString(raw), g.toolName, arguments, true, admit)
	if err != nil {
		return nil, nil, err
	}
	if response.IsError {
		return nil, nil, &azureSignInToolError{result: response.Result}
	}
	forwarded, err := harnessv2.DecodeMCPForwardResponse(response.Result)
	if err != nil {
		return nil, nil, &azureSignInToolError{result: response.Result}
	}
	if forwarded.Status != http.StatusOK {
		return nil, nil, &azureSignInUpstreamError{status: forwarded.Status, headers: forwarded.Headers, body: forwarded.Body}
	}
	var page azureSubscriptionPage
	if err := json.Unmarshal([]byte(forwarded.Body), &page); err != nil {
		return nil, nil, fmt.Errorf("decode the subscription list: %w", err)
	}
	accounts := make([]azureProfileAccount, 0, len(page.Value))
	for _, subscription := range page.Value {
		id := subscription.SubscriptionID
		if id == "" {
			id = subscription.ID[strings.LastIndex(subscription.ID, "/")+1:]
		}
		if !azureSubscriptionIDPattern.MatchString(id) {
			continue
		}
		account := azureProfileAccount{
			ID: id, Name: subscription.DisplayName, State: subscription.State, User: azureManagedIdentityUser,
			TenantID: azureZeroTenant, EnvironmentName: azureCloudName, HomeTenantID: subscription.TenantID,
		}
		if subscription.ManagedByTenants != nil {
			tenants := make([]azureProfileTenant, 0, len(*subscription.ManagedByTenants))
			for _, tenant := range *subscription.ManagedByTenants {
				tenants = append(tenants, azureProfileTenant(tenant))
			}
			account.ManagedByTenants = &tenants
		}
		accounts = append(accounts, account)
	}
	return accounts, g.nextSubscriptionPage(page.NextLink), nil
}

func (g *azureGateway) nextSubscriptionPage(link string) *harnessv2.MCPForwardRequest {
	if link == "" {
		return nil
	}
	parsed, err := url.Parse(link)
	if err != nil || parsed.User != nil || !strings.EqualFold(parsed.Scheme, providerProxyTLSScheme) ||
		"https://"+harnessv2.ForwardHost(parsed.Host) != g.origin || !strings.EqualFold(parsed.EscapedPath(), "/subscriptions") {
		return nil
	}
	return &harnessv2.MCPForwardRequest{Method: http.MethodGet, Path: parsed.EscapedPath(), Query: parsed.RawQuery}
}

func (g *azureGateway) writeProfile(profile azureProfile, asChild bool) error {
	content, err := json.Marshal(profile)
	if err != nil {
		return err
	}
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	name := "." + azureProfileName + ".orka-" + hex.EncodeToString(raw)
	write := func() error {
		file, err := g.profileDir.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("write az's profile: %w", err)
		}
		written := false
		defer func() {
			if !written {
				_ = g.profileDir.Remove(name)
			}
		}()
		if !asChild {
			if err := file.Chown(g.uid, g.gid); err != nil {
				_ = file.Close()
				return fmt.Errorf("give az's profile to the session: %w", err)
			}
		}
		if _, err := file.Write(content); err != nil {
			_ = file.Close()
			return fmt.Errorf("write az's profile: %w", err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("write az's profile: %w", err)
		}
		if err := g.profileDir.Rename(name, azureProfileName); err != nil {
			return fmt.Errorf("write az's profile: %w", err)
		}
		written = true
		return nil
	}
	if asChild {
		return asSessionChild(g.uid, g.gid, write)
	}
	return write()
}

func underPlaceholderAccount(path string) bool {
	prefix := "/subscriptions/" + azureZeroTenant
	if len(path) < len(prefix) || !strings.EqualFold(path[:len(prefix)], prefix) {
		return false
	}
	return len(path) == len(prefix) || path[len(prefix)] == '/'
}

func (g *azureGateway) answerUnderPlaceholderAccount(w http.ResponseWriter, r *http.Request) {
	if g.session.proxy != nil {
		if !tryAcquireMCPSlot(g.session.proxy.slots) {
			w.Header().Set("Retry-After", "1")
			writeAzureError(w, http.StatusTooManyRequests, "OrkaGatewayBusy", "This runtime already has its most requests in flight; retry shortly.")
			return
		}
		defer releaseMCPSlot(g.session.proxy.slots)
	}
	subscriptions, err := g.signIn(r.Context(), false, true)
	if toolErr, ok := errors.AsType[*azureSignInToolError](err); ok {
		writeAzureToolError(w, false, toolErr.result)
		return
	}
	if upstream, ok := errors.AsType[*azureSignInUpstreamError](err); ok {
		for _, name := range []string{"Content-Type", "Retry-After", "X-Ms-Request-Id", "X-Ms-Correlation-Request-Id"} {
			if value := upstream.headers[name]; value != "" {
				w.Header().Set(name, value)
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(upstream.status)
		_, _ = w.Write([]byte(upstream.body))
		return
	}
	switch {
	case err != nil:
		writeAzureCallError(w, err, false, g.toolName)
	case subscriptions == 0:
		writeAzureError(w, http.StatusForbidden, "OrkaNoSubscription",
			"The identity az acts as through tool "+fmt.Sprintf("%q", g.toolName)+" has no role on any Azure subscription, so az has none to use. "+
				"Ask an administrator to grant it a role on a subscription, then run the command again.")
	default:
		writeAzureError(w, http.StatusConflict, "OrkaSignedIn",
			"Orka signed az in to the identity's subscriptions while this command started. Run the command again.")
	}
}
