package supervisor

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// The Azure CLI reaches Azure Resource Manager through a forwarding Tool. The
// supervisor presents itself to az as an Azure cloud whose Resource Manager
// endpoint and managed identity are this loopback listener: az logs in with
// `az login --identity`, receives a session-scoped placeholder token, and
// sends every Resource Manager request here. Each request becomes one broker
// call to the forwarding Tool, so the controller checks the prompt's frozen
// tool policy, attaches the real credential, and sends the request to the
// Tool's origin. The real credential never enters the runtime.
const (
	azureCloudName          = "OrkaAzure"
	azureIdentityPath       = "/msi/token"
	azureMetadataPath       = "/metadata/endpoints"
	azureIdentityHeaderName = "X-Identity-Header"
	azureTokenLifetime      = time.Hour
	// azurePlaceholderAlgorithm marks the placeholder as unsigned: az decodes
	// it without verifying, and nothing else accepts it.
	azurePlaceholderAlgorithm = "none"
)

// azureManagementAudiences are the token resources az asks the managed
// identity for when it talks to Resource Manager.
var azureManagementAudiences = map[string]bool{
	"https://management.core.windows.net/": true, "https://management.core.windows.net": true,
	"https://management.azure.com/": true, "https://management.azure.com": true,
}

// systemCABundles are the usual locations of the image's trust store. The
// gateway's CA is appended to the first one found, so az still trusts every
// other HTTPS endpoint.
var systemCABundles = []string{
	"/etc/ssl/certs/ca-certificates.crt",
	"/etc/pki/tls/certs/ca-bundle.crt",
	"/etc/ssl/cert.pem",
}

// azureGatewayAuthority is the loopback TLS identity shared by every session
// of one supervisor boot. It is generated in memory and never persisted.
type azureGatewayAuthority struct {
	once      sync.Once
	err       error
	caPEM     []byte
	tlsConfig *tls.Config
}

func (a *azureGatewayAuthority) load() (*tls.Config, []byte, error) {
	a.once.Do(func() { a.tlsConfig, a.caPEM, a.err = newAzureGatewayTLS() })
	return a.tlsConfig, a.caPEM, a.err
}

func newAzureGatewayTLS() (*tls.Config, []byte, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now().UTC()
	caTemplate := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: "Orka runtime Azure gateway CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(397 * 24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, nil, err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(397 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	config := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey}},
	}
	return config, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), nil
}

func randomSerial() *big.Int {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
	if err != nil {
		return big.NewInt(time.Now().UnixNano())
	}
	return serial
}

// azureGateway serves az for one RuntimeSession.
type azureGateway struct {
	session  *mcpProxySession
	toolName string
	// writeToolName carries the Tool's writes, or is empty when the Tool
	// forwards reads only.
	writeToolName  string
	origin         string
	listener       net.Listener
	server         *http.Server
	base           string
	identityHeader string
	tokenSecret    string
	closeOnce      sync.Once
	// ctx ends when the gateway closes, stopping a sign-in in flight.
	ctx    context.Context
	cancel context.CancelFunc
	// profileDir is the child's ~/.azure, opened at setup, so writes to
	// az's profile cannot be redirected by a link the child plants. uid
	// and gid own the profile.
	profileDir *os.Root
	uid, gid   int
	// signInMu guards the sign-in state: the attempt in flight, if any,
	// and whether one has written the identity's subscriptions.
	signInMu      sync.Mutex
	signInAttempt *azureSignIn
	signedIn      bool
	// throttled counts, per write call ID, the throttled answers Azure gave
	// it. Azure did not process a throttled request, so the client's retry
	// is a new attempt under a new call ID rather than a replay.
	throttledMu sync.Mutex
	throttled   map[string]int
	// writePrompts records, per write call ID, the prompt that first sent
	// it. Its retries share that ID, but the broker scopes a call to its
	// prompt, so a resend during a later prompt would be a second write.
	writePromptsMu sync.Mutex
	writePrompts   map[string]harnessv2.PromptID
}

// maxAzureWritePrompts bounds the writes one session remembers. A session
// forgets none, since a forgotten write could be sent again by a resend in
// a later prompt; past the bound it refuses new writes instead.
const maxAzureWritePrompts = 16384

// errAzureWriteFromEarlierPrompt refuses a write first sent during an
// earlier prompt; errAzureTooManyWrites refuses a write the session cannot
// remember.
var (
	errAzureWriteFromEarlierPrompt = errors.New("the write was first sent during an earlier prompt")
	errAzureTooManyWrites          = errors.New("the session sent too many writes to remember another")
)

// admitWrite records the prompt a joinable write is sent under, and refuses
// it when an earlier prompt already sent it: a later prompt is a different
// call to the broker, which would send the write again. The session
// remembers every write for its whole life, which is as long as any az
// process that could resend one runs.
func (g *azureGateway) admitWrite(callID string, prompt harnessv2.PromptID) error {
	g.writePromptsMu.Lock()
	defer g.writePromptsMu.Unlock()
	if first, ok := g.writePrompts[callID]; ok {
		if first != prompt {
			return errAzureWriteFromEarlierPrompt
		}
		return nil
	}
	if len(g.writePrompts) >= maxAzureWritePrompts {
		return errAzureTooManyWrites
	}
	if g.writePrompts == nil {
		g.writePrompts = map[string]harnessv2.PromptID{}
	}
	g.writePrompts[callID] = prompt
	return nil
}

// maxAzureThrottledWrites bounds the throttled-write memory of one session.
const maxAzureThrottledWrites = 256

// azureJoinableWritePrefix starts the call ID of a write that retries join.
const azureJoinableWritePrefix = "az-w-"

// attemptCallID returns the call ID and number of the current attempt of a
// joinable write: a write Azure throttled is retried as a new attempt.
func (g *azureGateway) attemptCallID(write bool, callID string) (string, int) {
	if !write || !strings.HasPrefix(callID, azureJoinableWritePrefix) {
		return callID, 0
	}
	g.throttledMu.Lock()
	defer g.throttledMu.Unlock()
	if attempt := g.throttled[callID]; attempt > 0 {
		return callID + "-a" + strconv.Itoa(attempt), attempt
	}
	return callID, 0
}

// noteAnswer records a throttled answer to a joinable write: Azure did not
// process that attempt, so the client's retry is sent again as the next one.
// Only the current attempt's 429 advances it; a late 429 of an earlier
// attempt, which another request may have joined, never does, so a write
// that later succeeded is never sent again.
//
// It reports false when it cannot record the throttle because the session
// already tracks its most writes. Tracked writes keep their place, since
// forgetting one could resend a write that already succeeded, so the
// caller must answer with an error az does not retry: a retry would only
// rejoin, and replay, the throttled attempt.
func (g *azureGateway) noteAnswer(write bool, callID string, attempt, status int) bool {
	if !write || status != http.StatusTooManyRequests || !strings.HasPrefix(callID, azureJoinableWritePrefix) {
		return true
	}
	g.throttledMu.Lock()
	defer g.throttledMu.Unlock()
	current, tracked := g.throttled[callID]
	if current != attempt {
		return true
	}
	if !tracked && len(g.throttled) >= maxAzureThrottledWrites {
		return false
	}
	if g.throttled == nil {
		g.throttled = map[string]int{}
	}
	g.throttled[callID] = current + 1
	return true
}

// azureForwardingTool returns the session's Azure forwarding Tool, if any,
// and the name of its write tool when it allows writes. A session with two
// Tools would leave the Tool, and so the identity, az acts as to chance, so
// it is refused, as is a write tool without its Tool.
func azureForwardingTool(configuration harnessv2.MCPPolicyConfiguration) (harnessv2.MCPToolDescriptor, string, bool, error) {
	var found []harnessv2.MCPToolDescriptor
	writes := map[string]bool{}
	for _, descriptor := range configuration.ToolPolicy.Tools {
		if descriptor.Forward == nil || descriptor.Forward.Protocol != harnessv2.MCPForwardProtocolAzureResourceManager {
			continue
		}
		if descriptor.Forward.Write {
			writes[descriptor.Name] = true
			continue
		}
		found = append(found, descriptor)
	}
	switch len(found) {
	case 0:
		if len(writes) > 0 {
			return harnessv2.MCPToolDescriptor{}, "", false, errors.New("the session allows an Azure write tool without its forwarding Tool")
		}
		return harnessv2.MCPToolDescriptor{}, "", false, nil
	case 1:
		writeName := harnessv2.MCPForwardWriteToolName(found[0].Name)
		if len(writes) > 1 || (len(writes) == 1 && !writes[writeName]) {
			return harnessv2.MCPToolDescriptor{}, "", false, errors.New("the session allows an Azure write tool that is not its forwarding Tool's")
		}
		if !writes[writeName] {
			writeName = ""
		}
		return found[0], writeName, true, nil
	default:
		return harnessv2.MCPToolDescriptor{}, "", false, fmt.Errorf("the session allows %d Azure forwarding tools; allow at most one", len(found))
	}
}

func newAzureGateway(authority *azureGatewayAuthority, session *mcpProxySession, descriptor harnessv2.MCPToolDescriptor, writeToolName string) (*azureGateway, error) {
	if descriptor.Forward == nil {
		return nil, errors.New("not a forwarding tool")
	}
	origin, err := harnessv2.ForwardOrigin(descriptor.Forward.Origin)
	if err != nil {
		return nil, err
	}
	tlsConfig, _, err := authority.load()
	if err != nil {
		return nil, fmt.Errorf("start the Azure gateway TLS identity: %w", err)
	}
	identityHeader, err := randomMCPSecret(32)
	if err != nil {
		return nil, err
	}
	tokenSecret, err := randomMCPSecret(32)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start the Azure gateway listener: %w", err)
	}
	gateway := &azureGateway{
		session: session, toolName: descriptor.Name, writeToolName: writeToolName, origin: origin, listener: listener,
		base: "https://" + listener.Addr().String(), identityHeader: identityHeader, tokenSecret: tokenSecret,
	}
	gateway.ctx, gateway.cancel = context.WithCancel(context.Background())
	gateway.server = &http.Server{
		Handler: http.HandlerFunc(gateway.serveHTTP), ErrorLog: log.New(io.Discard, "", 0), TLSConfig: tlsConfig,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32 << 10,
	}
	go func() { _ = gateway.server.ServeTLS(listener, "", "") }()
	return gateway, nil
}

// attachAzureGateway starts the session's Azure gateway when its tool policy
// allows an Azure forwarding Tool, and returns the environment az needs. The
// gateway closes with the session.
func (s *Server) attachAzureGateway(session *mcpProxySession, configuration harnessv2.MCPPolicyConfiguration, home string, uid, gid int) (map[string]string, error) {
	descriptor, writeToolName, ok, err := azureForwardingTool(configuration)
	if err != nil || !ok {
		return nil, err
	}
	gateway, err := newAzureGateway(&s.azureAuthority, session, descriptor, writeToolName)
	if err != nil {
		return nil, err
	}
	_, caPEM, _ := s.azureAuthority.load()
	gateway.uid, gateway.gid = uid, gid
	environment, err := gateway.prepareHome(home, caPEM)
	if err != nil {
		gateway.close()
		return nil, err
	}
	session.mu.Lock()
	if session.closed {
		session.mu.Unlock()
		gateway.close()
		return nil, errors.New("the session closed while its Azure gateway started")
	}
	session.forwarders = append(session.forwarders, gateway)
	session.mu.Unlock()
	return environment, nil
}

func (g *azureGateway) close() {
	if g == nil {
		return
	}
	g.closeOnce.Do(func() {
		if g.cancel != nil {
			g.cancel()
		}
		_ = g.server.Close()
		if g.profileDir != nil {
			_ = g.profileDir.Close()
		}
	})
}

// prepareHome writes az's configuration into the session home, selecting the
// gateway as the active cloud and the placeholder account as az's sign-in,
// and returns the child environment az needs. The files are written before
// session ownership is finalized, so the child owns them.
func (g *azureGateway) prepareHome(home string, caPEM []byte) (map[string]string, error) {
	dir := filepath.Join(home, ".azure")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	config := "[cloud]\nname = " + azureCloudName + "\n\n[core]\ncollect_telemetry = no\nsurvey_message = no\nfirst_run = no\n"
	clouds := "[" + azureCloudName + "]\n" +
		"endpoint_active_directory = https://login.microsoftonline.com\n" +
		"endpoint_active_directory_resource_id = https://management.core.windows.net/\n" +
		"endpoint_management = https://management.core.windows.net/\n" +
		"endpoint_microsoft_graph_resource_id = https://graph.microsoft.com/\n" +
		"endpoint_portal = https://portal.azure.com\n" +
		"endpoint_resource_manager = " + g.base + "/\n" +
		"suffix_acr_login_server_endpoint = .azurecr.io\n" +
		"suffix_keyvault_dns = .vault.azure.net\n" +
		"suffix_storage_endpoint = core.windows.net\n"
	bundle := append([]byte(nil), caPEM...)
	for _, candidate := range systemCABundles {
		if roots, err := os.ReadFile(candidate); err == nil && len(roots) > 0 {
			bundle = append(append(roots, '\n'), caPEM...)
			break
		}
	}
	bundlePath := filepath.Join(dir, "orka-ca-bundle.pem")
	for name, content := range map[string][]byte{
		filepath.Join(dir, "config"): []byte(config), filepath.Join(dir, "clouds.config"): []byte(clouds), bundlePath: bundle,
	} {
		if err := os.WriteFile(name, content, 0o600); err != nil {
			return nil, err
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	g.profileDir = root
	if err := g.writeProfile(placeholderAzureProfile(azureSignInPendingName), false); err != nil {
		return nil, err
	}
	return map[string]string{
		"AZURE_CONFIG_DIR":   dir,
		"IDENTITY_ENDPOINT":  g.base + azureIdentityPath,
		"IDENTITY_HEADER":    g.identityHeader,
		"REQUESTS_CA_BUNDLE": bundlePath,
	}, nil
}

func (g *azureGateway) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackRemote(r.RemoteAddr) {
		writeAzureError(w, http.StatusForbidden, "OrkaGatewayLoopbackOnly", "The Orka Azure gateway serves only this runtime.")
		return
	}
	switch r.URL.Path {
	case azureIdentityPath:
		g.serveIdentity(w, r)
	case azureMetadataPath:
		g.serveMetadata(w)
	default:
		g.serveResourceManager(w, r)
	}
}

// serveIdentity answers az's managed-identity request with a placeholder
// bearer: a JWT-shaped string az can decode, whose signature is this
// session's secret. It authorizes nothing outside this gateway.
func (g *azureGateway) serveIdentity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || !constantTimeEqual(r.Header.Get(azureIdentityHeaderName), g.identityHeader) {
		writeAzureError(w, http.StatusUnauthorized, "OrkaIdentityUnauthorized", "The managed identity request is not from this session.")
		return
	}
	resource := r.URL.Query().Get("resource")
	if !azureManagementAudiences[resource] {
		writeAzureError(w, http.StatusBadRequest, "OrkaResourceNotForwarded",
			"Orka forwards only Azure Resource Manager requests; this az command needs another Azure service.")
		return
	}
	expires := time.Now().UTC().Add(azureTokenLifetime)
	token := g.placeholderToken(resource, expires)
	writeAzureJSON(w, http.StatusOK, map[string]string{
		"access_token": token, "expires_on": strconv.FormatInt(expires.Unix(), 10), "resource": resource,
		"token_type": "Bearer", "client_id": "00000000-0000-0000-0000-000000000000",
	})
}

func (g *azureGateway) placeholderToken(resource string, expires time.Time) string {
	encode := func(value any) string {
		raw, _ := json.Marshal(value)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	zero := "00000000-0000-0000-0000-000000000000"
	now := time.Now().UTC().Unix()
	return encode(map[string]string{"alg": azurePlaceholderAlgorithm, "typ": "JWT"}) + "." + encode(map[string]any{
		"aud": resource, "iss": "https://sts.windows.net/" + zero + "/", "tid": zero, "oid": zero, "appid": zero,
		"iat": now, "nbf": now, "exp": expires.Unix(),
	}) + "." + g.tokenSecret
}

func (g *azureGateway) bearerMatches(header string) bool {
	value := strings.TrimSpace(header)
	if len(value) < len("Bearer ") || !strings.EqualFold(value[:len("Bearer ")], "Bearer ") {
		return false
	}
	parts := strings.Split(strings.TrimSpace(value[len("Bearer "):]), ".")
	return len(parts) == 3 && constantTimeEqual(parts[2], g.tokenSecret)
}

func (g *azureGateway) serveMetadata(w http.ResponseWriter) {
	writeAzureJSON(w, http.StatusOK, map[string]any{
		"name": azureCloudName, "resourceManager": g.base + "/", "portal": "https://portal.azure.com",
		"authentication": map[string]any{
			"loginEndpoint": "https://login.microsoftonline.com", "tenant": "common", "identityProvider": "AAD",
			"audiences": []string{"https://management.core.windows.net/", "https://management.azure.com/"},
		},
		"microsoftGraphResourceId": "https://graph.microsoft.com/",
		"suffixes":                 map[string]string{"storage": "core.windows.net", "keyVaultDns": "vault.azure.net", "acrLoginServer": "azurecr.io"},
	})
}

func (g *azureGateway) serveResourceManager(w http.ResponseWriter, r *http.Request) {
	if !g.bearerMatches(r.Header.Get("Authorization")) {
		writeAzureError(w, http.StatusUnauthorized, "OrkaGatewayUnauthorized", "Run `az login --identity` in this session first.")
		return
	}
	if underPlaceholderAccount(r.URL.EscapedPath()) {
		g.answerUnderPlaceholderAccount(w, r)
		return
	}
	write := harnessv2.ForwardWriteMethod(r.Method)
	toolName := g.toolName
	switch {
	case write && g.writeToolName == "":
		writeAzureError(w, http.StatusMethodNotAllowed, "OrkaReadOnly",
			"Orka forwards only read requests (GET) to Azure Resource Manager through tool "+strconv.Quote(g.toolName)+
				"; this az command changes or acts on a resource, which this Task cannot do. Writes need http.forward.writes on that Tool and, for a linked account, a readWrite link.")
		return
	case write:
		toolName = g.writeToolName
	case r.Method != http.MethodGet && r.Method != http.MethodHead:
		writeAzureError(w, http.StatusMethodNotAllowed, "OrkaMethodNotForwarded", "Orka does not forward "+r.Method+" requests.")
		return
	}
	request := harnessv2.MCPForwardRequest{Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery}
	if write {
		// az --if-match and --if-none-match guard a write; dropping them
		// would run it unguarded. Reads keep ignoring them. A header sent
		// more than once is one list (RFC 9110), so every value is kept: an
		// If-None-Match cut to its first value would allow an overwrite the
		// client ruled out.
		request.IfMatch = strings.Join(r.Header.Values("If-Match"), ", ")
		request.IfNoneMatch = strings.Join(r.Header.Values("If-None-Match"), ", ")
	}
	body, status, code, message := readAzureRequestBody(r, write)
	if status != 0 {
		writeAzureError(w, status, code, message)
		return
	}
	request.Body = json.RawMessage(body)
	if err := request.Validate(); err != nil {
		writeAzureError(w, http.StatusBadRequest, "OrkaRequestRefused", "Orka refused the request: "+err.Error()+".")
		return
	}
	arguments, err := json.Marshal(request)
	if err != nil {
		writeAzureError(w, http.StatusInternalServerError, "OrkaGatewayError", "The request could not be encoded.")
		return
	}
	if len(arguments) > harnessv2.MaxMCPArgumentsBytes {
		writeAzureError(w, http.StatusRequestEntityTooLarge, "OrkaRequestTooLarge",
			fmt.Sprintf("Orka forwards request bodies up to about %d KiB.", harnessv2.MaxMCPArgumentsBytes>>10))
		return
	}
	baseCallID, err := azureCallID(write, r.Header.Get(azureClientRequestIDHeader), request)
	if err != nil {
		writeAzureError(w, http.StatusInternalServerError, "OrkaGatewayError", "The request could not be identified.")
		return
	}
	callID, attempt := g.attemptCallID(write, baseCallID)
	joinable := write && strings.HasPrefix(baseCallID, azureJoinableWritePrefix)
	// Forwarded calls share the runtime-wide MCP limit with tool calls, so
	// a larger pool keeps the same backpressure on the broker.
	if g.session.proxy != nil {
		if !tryAcquireMCPSlot(g.session.proxy.slots) {
			w.Header().Set("Retry-After", "1")
			writeAzureError(w, http.StatusTooManyRequests, "OrkaGatewayBusy", "This runtime already has its most requests in flight; retry shortly.")
			return
		}
		defer releaseMCPSlot(g.session.proxy.slots)
	}
	var admit func(harnessv2.PromptID) error
	if joinable {
		admit = func(prompt harnessv2.PromptID) error { return g.admitWrite(baseCallID, prompt) }
	}
	response, err := g.session.callBrokeredTool(r.Context(), callID, toolName, arguments, true, admit)
	if err != nil {
		writeAzureCallError(w, err, write, toolName)
		return
	}
	if response.IsError {
		writeAzureToolError(w, write, response.Result)
		return
	}
	forwarded, err := harnessv2.DecodeMCPForwardResponse(response.Result)
	if err != nil {
		writeAzureError(w, http.StatusBadGateway, "OrkaToolRefused", "Orka refused or could not complete the request: "+brokerErrorMessage(response.Result)+".")
		return
	}
	if !g.noteAnswer(write, baseCallID, attempt, forwarded.Status) {
		writeAzureError(w, http.StatusConflict, "OrkaThrottled",
			"Azure throttled the request and did not process it, and this session already tracks its most throttled writes. Run the command again later.")
		return
	}
	for name, value := range forwarded.Headers {
		if name == harnessv2.ForwardHeadContentLength {
			continue
		}
		if name == "Location" || name == "Azure-Asyncoperation" {
			value = g.rewriteLink(value)
		}
		w.Header().Set(name, value)
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		// A HEAD answer has no body; it reports the length the service gave.
		if length, err := strconv.ParseInt(forwarded.Headers[harnessv2.ForwardHeadContentLength], 10, 64); err == nil && length >= 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		}
		w.WriteHeader(forwarded.Status)
		return
	}
	answer := g.rewriteBody(forwarded.Body)
	w.Header().Set("Content-Length", strconv.Itoa(len(answer)))
	w.WriteHeader(forwarded.Status)
	_, _ = io.WriteString(w, answer)
}

// writeAzureCallError answers a request whose broker call failed before it
// returned an answer.
func writeAzureCallError(w http.ResponseWriter, err error, write bool, toolName string) {
	switch {
	case errors.Is(err, errAzureWriteFromEarlierPrompt):
		writeAzureError(w, http.StatusConflict, "OrkaResendRefused",
			"Orka received this request during an earlier turn and did not send it again. Check the resource before running the command again.")
	case errors.Is(err, errAzureTooManyWrites):
		writeAzureError(w, http.StatusConflict, "OrkaResendRefused",
			"This session sent too many writes for Orka to tell a retry from a new request; it did not send this one. Start a new session to write again.")
	case errors.Is(err, errMCPSessionAtCapacity):
		w.Header().Set("Retry-After", "1")
		writeAzureError(w, http.StatusTooManyRequests, "OrkaGatewayBusy", "This session already has requests in flight; retry shortly.")
	case errors.Is(err, errMCPCallNotAuthorized):
		writeAzureError(w, http.StatusForbidden, "OrkaNotAuthorized",
			"Orka allows az only while this session's prompt is running and the Task allows tool "+strconv.Quote(toolName)+".")
	case write:
		// The broker records a failed write as an unknown outcome, so a
		// retry could only replay that; a 5xx would make az retry it.
		writeAzureError(w, http.StatusConflict, "OrkaOutcomeUnknown",
			"The request may have reached Azure. Check the resource before repeating it.")
	default:
		writeAzureError(w, http.StatusBadGateway, "OrkaBrokerError", "Orka could not complete the request.")
	}
}

// writeAzureToolError answers a request the broker answered with a tool
// error. A decided or possibly delivered write gets a status az does not
// retry, since a retry could only replay the recorded outcome.
func writeAzureToolError(w http.ResponseWriter, write bool, result json.RawMessage) {
	code, message := brokerErrorCode(result), brokerErrorMessage(result)
	switch {
	case code == harnessv2.MCPToolErrorCodeConnectionRequired:
		// The broker's message already says who must do what; the agent
		// should not retry or try to sign az in itself.
		writeAzureError(w, http.StatusForbidden, "OrkaConnectionRequired", message+". Retrying or running az login will not help.")
	case code == "approval_declined" || code == "approval_expired" || code == "approval_cancelled" || code == "approval_stale":
		writeAzureError(w, http.StatusForbidden, "OrkaApprovalNotGranted", "Orka did not send the request: "+message)
	case code == "tool_failed_without_effect":
		writeAzureError(w, http.StatusForbidden, "OrkaToolRefused", "Orka refused the request without sending it: "+message)
	case code == "tool_outcome_unknown" || (write && code == "tool_execution_failed"):
		// A failed execution was delivered, so a write may have reached Azure.
		writeAzureError(w, http.StatusConflict, "OrkaOutcomeUnknown",
			"The request may have reached Azure. Check the resource before repeating it.")
	default:
		writeAzureError(w, http.StatusBadGateway, "OrkaToolRefused", "Orka refused or could not complete the request: "+message+".")
	}
}

// azureNextLinkFields are the paging fields Resource Manager responses use.
var azureNextLinkFields = map[string]bool{"nextLink": true, "@odata.nextLink": true}

// rewriteLink points one URL on the Tool's origin, such as a nextLink or a
// long-running operation URL, at the gateway, so az keeps following it
// through Orka instead of calling the service directly. Any other value,
// including a URL on another host, is returned unchanged.
func (g *azureGateway) rewriteLink(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.User != nil || !strings.EqualFold(parsed.Scheme, providerProxyTLSScheme) ||
		"https://"+harnessv2.ForwardHost(parsed.Host) != g.origin {
		return value
	}
	rewritten := *parsed
	rewritten.Scheme, rewritten.Host = providerProxyTLSScheme, strings.TrimPrefix(g.base, "https://")
	return rewritten.String()
}

// rewriteBody rewrites only paging links in a JSON response. Resource data,
// such as a tag or deployment output that happens to name the Resource
// Manager origin, is returned exactly as the service sent it.
func (g *azureGateway) rewriteBody(body string) string {
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil || decoder.More() {
		return body
	}
	object, ok := document.(map[string]any)
	if !ok {
		return body
	}
	changed := false
	for field := range azureNextLinkFields {
		link, ok := object[field].(string)
		if !ok {
			continue
		}
		if rewritten := g.rewriteLink(link); rewritten != link {
			object[field] = rewritten
			changed = true
		}
	}
	if !changed {
		return body
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return body
	}
	return string(encoded)
}

func brokerErrorMessage(result json.RawMessage) string {
	var failure struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(result, &failure) == nil && strings.TrimSpace(failure.Error) != "" {
		return strings.TrimSuffix(strings.TrimSpace(failure.Error), ".")
	}
	return "the tool returned an unexpected result"
}

func brokerErrorCode(result json.RawMessage) string {
	var failure struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(result, &failure) != nil {
		return ""
	}
	return failure.Code
}

// azureClientRequestIDHeader names the request ID the Azure SDK sets once per
// operation and keeps across its retries.
const azureClientRequestIDHeader = "X-Ms-Client-Request-Id"

// maxAzureRequestBodyBytes bounds a write's body so its arguments fit the
// broker's argument limit.
const maxAzureRequestBodyBytes = harnessv2.MaxMCPArgumentsBytes - 8<<10

// readAzureRequestBody reads a write's JSON body. A read carries none: a
// chunked request reports an unknown length (-1) and may still carry one
// that forwarding would drop, so only a read known to be empty passes. It
// returns a nonzero status, with an error code and message, to refuse.
func readAzureRequestBody(r *http.Request, write bool) (string, int, string, string) {
	if !write {
		if r.ContentLength != 0 {
			return "", http.StatusBadRequest, "OrkaReadOnly", "Read requests carry no body."
		}
		return "", 0, "", ""
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxAzureRequestBodyBytes+1))
	if err != nil {
		return "", http.StatusBadRequest, "OrkaRequestRefused", "The request body could not be read."
	}
	if len(raw) > maxAzureRequestBodyBytes {
		return "", http.StatusRequestEntityTooLarge, "OrkaRequestTooLarge",
			fmt.Sprintf("Orka forwards request bodies up to %d KiB.", maxAzureRequestBodyBytes>>10)
	}
	if len(raw) == 0 {
		return "", 0, "", ""
	}
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		return "", http.StatusUnsupportedMediaType, "OrkaRequestRefused", "Orka forwards only JSON request bodies."
	}
	return string(raw), 0, "", ""
}

// azureCallID names one forwarded request. A read gets a fresh ID. A write
// with the SDK's request ID gets an ID derived from it and the request, so a
// retry of the same write joins its pending approval or recorded result
// instead of writing twice; a write without one is never joined.
func azureCallID(write bool, clientRequestID string, request harnessv2.MCPForwardRequest) (string, error) {
	clientRequestID = strings.TrimSpace(clientRequestID)
	if !write || clientRequestID == "" {
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			return "", err
		}
		return "az-" + hex.EncodeToString(raw), nil
	}
	// The digest below makes the call ID joinable: retries of the same
	// write share it.
	body := sha256.Sum256([]byte(request.Body))
	digest := sha256.Sum256([]byte(strings.Join([]string{
		"orka.azure-gateway.write.v1", clientRequestID, request.Method, request.Path, request.Query, hex.EncodeToString(body[:]),
		request.IfMatch, request.IfNoneMatch,
	}, "\x00")))
	return azureJoinableWritePrefix + hex.EncodeToString(digest[:16]), nil
}

func constantTimeEqual(got, want string) bool {
	got = strings.TrimSpace(got)
	return want != "" && len(got) == len(want) && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func writeAzureError(w http.ResponseWriter, status int, code, message string) {
	writeAzureJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func writeAzureJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
