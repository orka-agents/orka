// Package source selects public GitHub revisions and reads bounded, verified
// source over anonymous HTTP without running it or materializing a checkout.
// It does not accept credentials or private sources.
package source

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	githubAPIBase     = "https://api.github.com"
	apiTimeout        = 20 * time.Second
	apiMaxReplyBytes  = 1 << 20
	apiMaxHeaderBytes = 64 << 10
	maxRefBytes       = 1024
)

var (
	ownerPattern        = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	namePattern         = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
	errSourceReplyLimit = errors.New("GitHub reply exceeds the response limit")
)

// Repository is public repository metadata confirmed by the anonymous API.
// Archived repositories remain selectable; Archived is not an admission rule.
type Repository struct {
	URL           string `json:"url"`
	Owner         string `json:"owner"`
	Name          string `json:"name"`
	DefaultBranch string `json:"defaultBranch"`
	Archived      bool   `json:"archived"`
}

// Target freezes a selected ref to full, lowercase Git object identities.
type Target struct {
	Repository Repository `json:"repository"`
	Ref        string     `json:"ref"`
	Commit     string     `json:"commit"`
	Tree       string     `json:"tree"`
}

// Client uses anonymous HTTPS against GitHub by default. APIBaseURL may instead
// name an explicit loopback HTTP(S) test server. HTTPClient may supply a standard
// *http.Transport (for example httptest.Server.Client's TLS roots); cookies,
// proxies, client certificates, and redirects are disabled on a private clone.
// Resolution has a 20-second deadline and a 1-MiB limit per JSON reply.
type Client struct {
	HTTPClient *http.Client
	APIBaseURL string
}

// Resolve accepts only https://github.com/owner/repository[.git]. An explicitly
// empty ref selects the confirmed default branch. Other refs are never replaced
// by a default, a release guess, or a version-range interpretation.
func (c Client) Resolve(ctx context.Context, repositoryURL, ref string) (Target, error) {
	if err := ctx.Err(); err != nil {
		return Target{}, err
	}
	repository, err := parseRepositoryURL(repositoryURL)
	if err != nil {
		return Target{}, err
	}
	if ref != "" && !validRef(ref) {
		return Target{}, errors.New("source ref must be a branch, tag, or commit, not a revision expression or range")
	}
	base, err := c.apiBase()
	if err != nil {
		return Target{}, err
	}
	client, transport, err := c.anonymousClient()
	if err != nil {
		return Target{}, err
	}
	defer transport.CloseIdleConnections()
	operation, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	prefix := base + "/repos/" + repository.Owner + "/" + repository.Name
	var metadata repositoryReply
	if err := getJSON(operation, client, prefix, &metadata, "repository lookup"); err != nil {
		return Target{}, err
	}
	repository, err = metadata.repository(repository)
	if err != nil {
		return Target{}, err
	}
	if ref == "" {
		ref = repository.DefaultBranch
	}
	var commit commitReply
	if err := getJSON(operation, client, prefix+"/commits/"+url.PathEscape(ref), &commit, "ref lookup"); err != nil {
		return Target{}, err
	}
	if !validObjectID(commit.SHA) || !validObjectID(commit.Commit.Tree.SHA) ||
		len(commit.SHA) != len(commit.Commit.Tree.SHA) || (validObjectID(ref) && commit.SHA != ref) {
		return Target{}, errors.New("GitHub ref reply did not contain the requested exact commit and tree")
	}
	return Target{Repository: repository, Ref: ref, Commit: commit.SHA, Tree: commit.Commit.Tree.SHA}, nil
}

func revalidatePublic(operation context.Context, client Client, target Target) error {
	confirmed, err := client.Resolve(operation, target.Repository.URL, target.Commit)
	if err != nil {
		return fmt.Errorf("revalidate public source: %w", err)
	}
	if confirmed.Commit != target.Commit || confirmed.Tree != target.Tree {
		return errors.New("public source no longer matches the selected exact commit and tree")
	}
	return nil
}

func validateTarget(target Target) error {
	repository, err := parseRepositoryURL(target.Repository.URL)
	if err != nil {
		return err
	}
	if repository.URL != target.Repository.URL || repository.Owner != target.Repository.Owner ||
		repository.Name != target.Repository.Name || !validRef(target.Repository.DefaultBranch) ||
		!validRef(target.Ref) || !validObjectID(target.Commit) || !validObjectID(target.Tree) ||
		len(target.Commit) != len(target.Tree) || (validObjectID(target.Ref) && target.Ref != target.Commit) {
		return errors.New("source target must contain consistent repository metadata and exact commit/tree identities")
	}
	return nil
}

func parseRepositoryURL(raw string) (Repository, error) {
	const prefix = "https://github.com/"
	if !strings.HasPrefix(raw, prefix) {
		return Repository{}, errors.New("source must be a canonical public HTTPS GitHub repository URL")
	}
	parts := strings.Split(strings.TrimPrefix(raw, prefix), "/")
	if len(parts) != 2 || !ownerPattern.MatchString(parts[0]) {
		return Repository{}, errors.New("source must name exactly one GitHub owner and repository")
	}
	name := strings.TrimSuffix(parts[1], ".git")
	if !namePattern.MatchString(name) || name == "." || name == ".." || strings.HasSuffix(name, ".git") {
		return Repository{}, errors.New("source repository URL contains an invalid repository name")
	}
	return Repository{URL: prefix + parts[0] + "/" + name, Owner: parts[0], Name: name}, nil
}

func validRef(ref string) bool {
	if ref == "" || len(ref) > maxRefBytes || !utf8.ValidString(ref) || ref == "@" || strings.HasPrefix(ref, "-") ||
		strings.ContainsAny(ref, `~^:?*[\`) || strings.Contains(ref, "..") || strings.Contains(ref, "@{") ||
		strings.HasSuffix(ref, ".") {
		return false
	}
	for _, char := range ref {
		if unicode.IsSpace(char) || unicode.IsControl(char) || unicode.Is(unicode.Cf, char) {
			return false
		}
	}
	for part := range strings.SplitSeq(ref, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

func validObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	nonzero := false
	for _, char := range []byte(value) {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
		nonzero = nonzero || char != '0'
	}
	return nonzero
}

type repositoryReply struct {
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	HTMLURL  string `json:"html_url"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
	Private       *bool  `json:"private"`
	Visibility    string `json:"visibility"`
	DefaultBranch string `json:"default_branch"`
	Archived      *bool  `json:"archived"`
}

func (reply repositoryReply) repository(requested Repository) (Repository, error) {
	// Both signals must be present and agree. A missing boolean must not become
	// a successful public-source authorization through Go's zero value.
	if reply.Private == nil || *reply.Private || reply.Visibility != "public" {
		return Repository{}, errors.New("GitHub did not confirm a PUBLIC, nonprivate repository")
	}
	confirmed, err := parseRepositoryURL(reply.HTMLURL)
	if err != nil || reply.Archived == nil || !validRef(reply.DefaultBranch) ||
		reply.Owner.Login != confirmed.Owner || reply.Name != confirmed.Name ||
		reply.FullName != confirmed.Owner+"/"+confirmed.Name ||
		!strings.EqualFold(requested.Owner, confirmed.Owner) || !strings.EqualFold(requested.Name, confirmed.Name) {
		return Repository{}, errors.New("GitHub repository reply has missing or inconsistent identity metadata")
	}
	confirmed.DefaultBranch, confirmed.Archived = reply.DefaultBranch, *reply.Archived
	return confirmed, nil
}

type commitReply struct {
	SHA    string `json:"sha"`
	Commit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	} `json:"commit"`
}

func (c Client) apiBase() (string, error) {
	if c.APIBaseURL == "" || c.APIBaseURL == githubAPIBase {
		return githubAPIBase, nil
	}
	base, err := url.Parse(c.APIBaseURL)
	if err != nil || base.Opaque != "" || base.User != nil || base.RawQuery != "" || base.ForceQuery ||
		base.Fragment != "" || base.RawFragment != "" || base.RawPath != "" || base.Path != "" ||
		(base.Scheme != "http" && base.Scheme != "https") {
		return "", errors.New("source API override must be an explicit loopback HTTP(S) origin")
	}
	ip := net.ParseIP(base.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return "", errors.New("source API override is restricted to loopback test servers")
	}
	if base.String() != c.APIBaseURL {
		return "", errors.New("source API override must be canonical")
	}
	return base.String(), nil
}

func (c Client) anonymousClient() (*http.Client, *http.Transport, error) {
	transport := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConns:          2,
		MaxIdleConnsPerHost:   2,
	}
	timeout := apiTimeout
	if c.HTTPClient != nil {
		if c.HTTPClient.Timeout > 0 && c.HTTPClient.Timeout < timeout {
			timeout = c.HTTPClient.Timeout
		}
		if c.HTTPClient.Transport != nil {
			configured, ok := c.HTTPClient.Transport.(*http.Transport)
			if !ok || configured == nil {
				return nil, nil, errors.New("anonymous source lookups require a standard HTTP transport")
			}
			transport = configured.Clone()
		}
	}
	transport.Proxy = nil
	transport.DisableCompression = true
	transport.MaxResponseHeaderBytes = apiMaxHeaderBytes
	transport.ResponseHeaderTimeout = minPositiveDuration(transport.ResponseHeaderTimeout, 10*time.Second)
	transport.TLSHandshakeTimeout = minPositiveDuration(transport.TLSHandshakeTimeout, 5*time.Second)
	if transport.TLSClientConfig != nil {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
		transport.TLSClientConfig.Certificates = nil
		transport.TLSClientConfig.GetClientCertificate = nil
		transport.TLSClientConfig.ClientSessionCache = nil
		transport.TLSClientConfig.InsecureSkipVerify = false
		transport.TLSClientConfig.ServerName = ""
		transport.TLSClientConfig.MinVersion = max(transport.TLSClientConfig.MinVersion, tls.VersionTLS12)
	} else {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return client, transport, nil
}

func minPositiveDuration(value, maximum time.Duration) time.Duration {
	if value <= 0 || value > maximum {
		return maximum
	}
	return value
}

func getJSON(ctx context.Context, client *http.Client, endpoint string, result any, operation string) error {
	return getJSONWithinLimit(ctx, client, endpoint, result, operation, apiMaxReplyBytes)
}

func getJSONWithinLimit(ctx context.Context, client *http.Client, endpoint string, result any, operation string, limit int64) (resultErr error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return errors.New("could not construct anonymous GitHub request")
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "orka-public-source")
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return context.DeadlineExceeded
		}
		return fmt.Errorf("anonymous GitHub %s failed", operation)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			resultErr = errors.Join(resultErr, errors.New("could not close GitHub response"))
		}
	}()
	if response.StatusCode != http.StatusOK {
		if rateLimit := sourceRateLimit(response, time.Now().UTC()); rateLimit != nil {
			return rateLimit
		}
		return fmt.Errorf("GitHub %s returned HTTP %d (no fallback or redirects)", operation, response.StatusCode)
	}
	if response.Header.Get("Content-Encoding") != "" {
		return errors.New("GitHub reply exceeds the uncompressed response limit")
	}
	if response.ContentLength > limit {
		return errSourceReplyLimit
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("could not read bounded GitHub reply")
	}
	if int64(len(body)) > limit {
		return errSourceReplyLimit
	}
	if !unambiguousJSON(body) || json.Unmarshal(body, result) != nil {
		return errors.New("GitHub returned malformed source metadata")
	}
	return nil
}

func unambiguousJSON(body []byte) bool {
	if !utf8.Valid(body) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if !jsonValue(decoder, 0) {
		return false
	}
	_, err := decoder.Token()
	return errors.Is(err, io.EOF)
}

func jsonValue(decoder *json.Decoder, depth int) bool {
	if depth > 32 {
		return false
	}
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return true
	}
	seen := make(map[string]bool)
	for decoder.More() {
		if delim == '{' {
			token, err := decoder.Token()
			if err != nil {
				return false
			}
			name, ok := token.(string)
			if !ok || strings.ContainsFunc(name, func(char rune) bool { return char > 127 }) {
				return false
			}
			// encoding/json also accepts case-insensitive struct field matches.
			name = strings.ToLower(name)
			if seen[name] {
				return false
			}
			seen[name] = true
		}
		if !jsonValue(decoder, depth+1) {
			return false
		}
	}
	end, err := decoder.Token()
	return err == nil && ((delim == '{' && end == json.Delim('}')) || (delim == '[' && end == json.Delim(']')))
}
