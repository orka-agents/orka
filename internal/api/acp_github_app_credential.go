package api

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
	"github.com/orka-agents/orka/internal/security"
)

const (
	githubAppAPIBaseURL       = "https://api.github.com"
	githubAppRequestTimeout   = 15 * time.Second
	githubAppJWTLifetime      = 9 * time.Minute
	githubAppJWTClockSkew     = 60 * time.Second
	maxGitHubAppResponseBytes = 64 << 10

	githubAppContentsPermission = "contents"
)

// githubAppMintRequest is one installation-token request: the App, the
// repositories the token covers (all in one installation) and the permissions.
type githubAppMintRequest struct {
	AppID        string
	PrivateKey   *rsa.PrivateKey
	Owner        string
	Repositories []string
	Permissions  map[string]string
}

// githubAppTokenMinter mints a GitHub App installation token. It is an
// interface so broker tests can run without GitHub.
type githubAppTokenMinter interface {
	MintInstallationToken(ctx context.Context, request githubAppMintRequest) (string, error)
}

// githubAppMintError carries the broker status and a non-secret error code.
type githubAppMintError struct {
	status int
	code   string
}

func (e *githubAppMintError) Error() string { return e.code }

func githubAppRefused(code string) error {
	return &githubAppMintError{status: http.StatusForbidden, code: code}
}

func githubAppUnavailable() error {
	return &githubAppMintError{status: http.StatusServiceUnavailable, code: "github_app_unavailable"}
}

// mintGitHubAppCredential turns a GitHubApp Secret into an installation token
// for the Task's repository and the role's permission.
func (s *Server) mintGitHubAppCredential(
	ctx context.Context,
	workspace *corev1alpha1.WorkspaceConfig,
	role publisherservice.CredentialRole,
	appID, privateKey []byte,
) (string, error) {
	request, err := githubAppRequestForRole(workspace, role)
	if err != nil {
		return "", err
	}
	request.AppID = strings.TrimSpace(string(appID))
	if request.AppID == "" || len(request.AppID) > 128 || strings.ContainsAny(request.AppID, " \t\r\n\x00\"\\") {
		return "", githubAppRefused("github_app_secret_invalid")
	}
	if request.PrivateKey, err = parseGitHubAppPrivateKey(privateKey); err != nil {
		return "", githubAppRefused("github_app_secret_invalid")
	}
	minter := s.githubAppMinter
	if minter == nil {
		minter = newGitHubAppHTTPMinter(githubAppAPIBaseURL, nil)
	}
	return minter.MintInstallationToken(ctx, request)
}

// githubAppRequestForRole selects the repositories and permissions for one
// credential role. Source reads cover the source repository; target reads and
// writes cover the publication repository; the forge role covers the pull
// request base and head, because reconciliation reads the head ref and opens
// the pull request on the base.
func githubAppRequestForRole(workspace *corev1alpha1.WorkspaceConfig, role publisherservice.CredentialRole) (githubAppMintRequest, error) {
	if workspace == nil {
		return githubAppMintRequest{}, githubAppRefused("github_app_repository_invalid")
	}
	source := strings.TrimSpace(workspace.GitRepo)
	target := strings.TrimSpace(workspace.PublicationGitRepo)
	if target == "" {
		target = source
	}
	var repositoryURLs []string
	var permissions map[string]string
	switch role {
	case publisherservice.CredentialRoleSourceRead:
		repositoryURLs, permissions = []string{source}, map[string]string{githubAppContentsPermission: githubPermissionRead}
	case publisherservice.CredentialRoleTargetRead:
		repositoryURLs, permissions = []string{target}, map[string]string{githubAppContentsPermission: githubPermissionRead}
	case publisherservice.CredentialRoleTargetWrite:
		repositoryURLs, permissions = []string{target}, map[string]string{githubAppContentsPermission: githubPermissionWrite}
	case publisherservice.CredentialRoleForge:
		repositoryURLs = []string{source, target}
		permissions = map[string]string{"pull_requests": githubPermissionWrite, githubAppContentsPermission: githubPermissionRead}
	default:
		return githubAppMintRequest{}, githubAppRefused("github_app_repository_invalid")
	}
	request := githubAppMintRequest{Permissions: permissions}
	for _, repositoryURL := range repositoryURLs {
		owner, name, err := security.ParseGitHubRepositoryURL(repositoryURL)
		if err != nil {
			return githubAppMintRequest{}, githubAppRefused("github_app_repository_invalid")
		}
		if request.Owner == "" {
			request.Owner = owner
		} else if !strings.EqualFold(request.Owner, owner) {
			// One installation token covers one account's repositories.
			return githubAppMintRequest{}, githubAppRefused("github_app_repository_invalid")
		}
		duplicate := false
		for _, existing := range request.Repositories {
			duplicate = duplicate || strings.EqualFold(existing, name)
		}
		if !duplicate {
			request.Repositories = append(request.Repositories, name)
		}
	}
	return request, nil
}

func parseGitHubAppPrivateKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(bytes.TrimSpace(data))
	if block == nil {
		return nil, fmt.Errorf("private key is not PEM")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("private key is not RSA")
		}
		return rsaKey, nil
	default:
		return nil, fmt.Errorf("private key PEM type is unsupported")
	}
}

type githubAppHTTPMinter struct {
	baseURL string
	client  *http.Client
	now     func() time.Time
}

func newGitHubAppHTTPMinter(baseURL string, client *http.Client) *githubAppHTTPMinter {
	if client == nil {
		client = &http.Client{Timeout: githubAppRequestTimeout}
	} else {
		clone := *client
		client = &clone
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &githubAppHTTPMinter{baseURL: strings.TrimRight(baseURL, "/"), client: client, now: time.Now}
}

func (m *githubAppHTTPMinter) MintInstallationToken(ctx context.Context, request githubAppMintRequest) (string, error) {
	if request.PrivateKey == nil || request.AppID == "" || request.Owner == "" || len(request.Repositories) == 0 {
		return "", githubAppRefused("github_app_secret_invalid")
	}
	appJWT, err := signGitHubAppJWT(request.AppID, request.PrivateKey, m.now())
	if err != nil {
		return "", githubAppRefused("github_app_secret_invalid")
	}
	var installation struct {
		ID int64 `json:"id"`
	}
	installationPath := "/repos/" + url.PathEscape(request.Owner) + "/" + url.PathEscape(request.Repositories[0]) + "/installation"
	if err := m.do(ctx, http.MethodGet, installationPath, appJWT, nil, http.StatusOK, &installation); err != nil {
		return "", err
	}
	if installation.ID < 1 {
		return "", githubAppUnavailable()
	}
	body := map[string]any{"repositories": request.Repositories, "permissions": request.Permissions}
	var token struct {
		Token string `json:"token"`
	}
	tokenPath := "/app/installations/" + strconv.FormatInt(installation.ID, 10) + "/access_tokens"
	if err := m.do(ctx, http.MethodPost, tokenPath, appJWT, body, http.StatusCreated, &token); err != nil {
		return "", err
	}
	token.Token = strings.TrimSpace(token.Token)
	if token.Token == "" || len(token.Token) > maxBrokeredCredentialBytes || strings.ContainsAny(token.Token, " \t\r\n\x00") {
		return "", githubAppUnavailable()
	}
	return token.Token, nil
}

func (m *githubAppHTTPMinter) do(ctx context.Context, method, path, appJWT string, body any, wantStatus int, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return githubAppUnavailable()
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, m.baseURL+path, reader)
	if err != nil {
		return githubAppUnavailable()
	}
	request.Header.Set("Authorization", "Bearer "+appJWT)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := m.client.Do(request)
	if err != nil {
		return githubAppUnavailable()
	}
	defer response.Body.Close() //nolint:errcheck
	data, err := io.ReadAll(io.LimitReader(response.Body, maxGitHubAppResponseBytes+1))
	if err != nil || len(data) > maxGitHubAppResponseBytes {
		return githubAppUnavailable()
	}
	switch {
	case response.StatusCode == wantStatus:
	case response.StatusCode == http.StatusNotFound:
		return githubAppRefused("github_app_not_installed")
	case response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= http.StatusInternalServerError:
		return githubAppUnavailable()
	case response.StatusCode == http.StatusForbidden && githubAppRateLimited(response.Header, data):
		return githubAppUnavailable()
	default:
		// 401 (bad App ID or key), 403 or 422 (permission or repository not
		// granted to the installation) and anything unexpected.
		return githubAppRefused("github_app_rejected")
	}
	if err := json.Unmarshal(data, out); err != nil {
		return githubAppUnavailable()
	}
	return nil
}

// githubAppRateLimited reports a 403 that is GitHub throttling rather than a
// refusal: the primary limit reports no remaining requests, and the secondary
// limit sends Retry-After or says so in its message.
func githubAppRateLimited(header http.Header, body []byte) bool {
	return header.Get("X-RateLimit-Remaining") == "0" || header.Get("Retry-After") != "" ||
		bytes.Contains(bytes.ToLower(body), []byte("rate limit"))
}

// signGitHubAppJWT signs the short-lived RS256 App JWT GitHub requires for
// App-level endpoints. iat is backdated for clock skew; exp stays under
// GitHub's ten-minute limit.
func signGitHubAppJWT(appID string, key *rsa.PrivateKey, now time.Time) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-githubAppJWTClockSkew).Unix(),
		"exp": now.Add(githubAppJWTLifetime).Unix(),
		"iss": appID,
	})
	if err != nil {
		return "", err
	}
	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func githubAppErrorResponse(err error) (int, string) {
	if mintErr, ok := errors.AsType[*githubAppMintError](err); ok {
		return mintErr.status, mintErr.code
	}
	return http.StatusServiceUnavailable, "github_app_unavailable"
}
