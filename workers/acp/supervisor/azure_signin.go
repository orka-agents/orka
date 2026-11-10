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

// The supervisor signs az in, so a stock Azure CLI works with no wrapper and
// no `az login`. az runs nothing before its profile names an account and a
// default subscription, and finding the subscriptions is a Resource Manager
// read, which the broker allows only while a prompt runs. So session setup
// writes a placeholder profile, a tenant-level account under the zero
// tenant, and the first running prompt replaces it with the identity's
// subscriptions, found through the broker like any az request. A command
// that still reaches the gateway under the placeholder triggers the same
// sign-in and is told to run again.
//
// The profile is az's own azureProfile.json, in the shape `az login
// --identity` writes. It holds no credential: az still gets the session's
// placeholder token from IDENTITY_ENDPOINT for every command.
//
// The child owns ~/.azure once the session starts, and the supervisor runs as
// root, so every write goes through an os.Root opened at setup: a link the
// child plants cannot redirect it. The supervisor never reads the child's
// files; it keeps the sign-in state in memory.
const (
	azureProfileName = "azureProfile.json"
	// azureZeroTenant is the tenant the placeholder token names, and so the
	// tenant az records for every account. Resource Manager has no
	// subscription with this ID, so a request under it is a request made
	// under the placeholder account.
	azureZeroTenant = "00000000-0000-0000-0000-000000000000"
	// azureTenantLevelAccountName is az's name for an account with no
	// subscription.
	azureTenantLevelAccountName = "N/A(tenant level account)"
	// azureSubscriptionsAPIVersion is the Subscriptions - List version az
	// itself uses.
	azureSubscriptionsAPIVersion = "2022-12-01"
	// azureSignInTimeout bounds a sign-in started at prompt start.
	azureSignInTimeout = time.Minute
	// maxAzureSubscriptionPages and maxAzureSubscriptions bound the
	// discovery. Past them the profile keeps what it found.
	maxAzureSubscriptionPages = 20
	maxAzureSubscriptions     = 2000
	// azureSignInCallPrefix starts the call ID of a sign-in read, so the
	// broker's records tell Orka's sign-in apart from the agent's requests.
	azureSignInCallPrefix = "az-signin-"
)

var azureSubscriptionIDPattern = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)

// azureProfileUser is the account az records for a system-assigned managed
// identity.
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

// placeholderAzureProfile is the profile of an identity with no
// subscription: what `az login --identity --allow-no-subscriptions` writes.
func placeholderAzureProfile() azureProfile {
	return azureProfile{Subscriptions: []azureProfileAccount{{
		ID: azureZeroTenant, Name: azureTenantLevelAccountName, State: "Enabled", User: azureManagedIdentityUser,
		IsDefault: true, TenantID: azureZeroTenant, EnvironmentName: azureCloudName,
	}}}
}

// azureSubscriptionPage is one page of Subscriptions - List.
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

// azureSignInToolError is a sign-in read the broker answered with a tool
// error, such as a missing linked account.
type azureSignInToolError struct{ result json.RawMessage }

func (e *azureSignInToolError) Error() string { return "the sign-in read was refused" }

// azureSignInUpstreamError is a sign-in read Resource Manager answered with
// an error status.
type azureSignInUpstreamError struct {
	status  int
	headers map[string]string
	body    string
}

func (e *azureSignInUpstreamError) Error() string {
	return fmt.Sprintf("Resource Manager answered the sign-in read with status %d", e.status)
}

// azureSignIn is one sign-in attempt that callers can join.
type azureSignIn struct {
	done          chan struct{}
	subscriptions int
	err           error
}

// signInAtPromptStart signs az in once per session, in the background, when
// a prompt starts running. A Tool whose reads need approval is left to the
// first az command instead, so starting a session never asks anyone to
// approve a read the agent did not make.
func (g *azureGateway) signInAtPromptStart() {
	if g.session.approvalRequired(g.toolName) {
		return
	}
	g.signInMu.Lock()
	signedIn := g.signedIn
	g.signInMu.Unlock()
	if signedIn {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(g.ctx, azureSignInTimeout)
		defer cancel()
		_, _ = g.signIn(ctx, true)
	}()
}

// signIn finds the identity's subscriptions and writes them as az's profile.
// It joins an attempt already running. It reports how many subscriptions the
// profile names. With unlessSignedIn, it does nothing once an attempt has
// succeeded, so a prompt-start sign-in that started late never overwrites a
// profile the agent has since changed, for example with az account set.
func (g *azureGateway) signIn(ctx context.Context, unlessSignedIn bool) (int, error) {
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
		attempt.subscriptions, attempt.err = g.discoverAndWriteProfile(ctx)
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

func (g *azureGateway) discoverAndWriteProfile(ctx context.Context) (int, error) {
	if g.session.proxy != nil {
		if !tryAcquireMCPSlot(g.session.proxy.slots) {
			return 0, errMCPSessionAtCapacity
		}
		defer releaseMCPSlot(g.session.proxy.slots)
	}
	var accounts []azureProfileAccount
	request := harnessv2.MCPForwardRequest{Method: http.MethodGet, Path: "/subscriptions", Query: "api-version=" + azureSubscriptionsAPIVersion}
	for page := 0; page < maxAzureSubscriptionPages && len(accounts) < maxAzureSubscriptions; page++ {
		found, next, err := g.readSubscriptionPage(ctx, request)
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
	profile := placeholderAzureProfile()
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
	if err := g.writeProfile(profile); err != nil {
		return 0, err
	}
	return len(accounts), nil
}

// readSubscriptionPage reads one page of the identity's subscriptions
// through the broker, and returns the request for the next page, if any.
func (g *azureGateway) readSubscriptionPage(ctx context.Context, request harnessv2.MCPForwardRequest) ([]azureProfileAccount, *harnessv2.MCPForwardRequest, error) {
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
	response, err := g.session.callBrokeredTool(ctx, azureSignInCallPrefix+hex.EncodeToString(raw), g.toolName, arguments, true, nil)
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
			// az records the token's tenant, and the subscription's own
			// tenant as its home tenant.
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

// nextSubscriptionPage turns a nextLink on the Tool's origin into the next
// request. Any other link ends the discovery.
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

// writeProfile replaces az's profile without following any link in the
// child's directory: it creates a new file under a random name, gives it to
// the child, and renames it over the profile.
func (g *azureGateway) writeProfile(profile azureProfile) error {
	content, err := json.Marshal(profile)
	if err != nil {
		return err
	}
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	name := "." + azureProfileName + ".orka-" + hex.EncodeToString(raw)
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
	if err := file.Chown(g.uid, g.gid); err != nil {
		_ = file.Close()
		return fmt.Errorf("give az's profile to the session: %w", err)
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

// underPlaceholderAccount reports whether a request names the zero tenant as
// its subscription, as az does while the placeholder account is its
// default.
func underPlaceholderAccount(path string) bool {
	prefix := "/subscriptions/" + azureZeroTenant
	if len(path) < len(prefix) || !strings.EqualFold(path[:len(prefix)], prefix) {
		return false
	}
	return len(path) == len(prefix) || path[len(prefix)] == '/'
}

// answerUnderPlaceholderAccount signs az in for a command az sent under the
// placeholder account, then tells the agent to run it again, or why it
// cannot. The command itself is never sent: the zero tenant is no
// subscription.
func (g *azureGateway) answerUnderPlaceholderAccount(w http.ResponseWriter, r *http.Request) {
	subscriptions, err := g.signIn(r.Context(), false)
	if toolErr, ok := errors.AsType[*azureSignInToolError](err); ok {
		writeAzureToolError(w, false, toolErr.result)
		return
	}
	if upstream, ok := errors.AsType[*azureSignInUpstreamError](err); ok {
		// Resource Manager's own answer, such as an expired role
		// assignment, says more than any summary. Retry-After keeps a
		// throttled client to Azure's pace.
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
