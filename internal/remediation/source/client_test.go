package source

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	fixtureURL    = "https://github.com/source-fixtures/project"
	fixturePrefix = "/repos/source-fixtures/project"
	fixtureCommit = "1234567890abcdef1234567890abcdef12345678"
	fixtureTree   = "abcdef1234567890abcdef1234567890abcdef12"
)

var _ func(context.Context, string, string) (Target, error) = Client{}.Resolve

type apiReply struct {
	status int
	body   string
}

type apiRequest struct {
	method string
	path   string
	query  string
	header http.Header
}

type apiFixture struct {
	server   *httptest.Server
	client   Client
	mutex    sync.Mutex
	requests []apiRequest
}

func repositoryJSON() string {
	return `{"name":"project","full_name":"source-fixtures/project","html_url":"` + fixtureURL +
		`","owner":{"login":"source-fixtures"},"private":false,"visibility":"public","default_branch":"main","archived":false}`
}

func commitJSON(commit, tree string) string {
	return `{"sha":"` + commit + `","commit":{"tree":{"sha":"` + tree + `"}}}`
}

func newAPIFixture(t *testing.T, replies map[string]apiReply) *apiFixture {
	t.Helper()
	fixture := &apiFixture{}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fixture.mutex.Lock()
		fixture.requests = append(fixture.requests, apiRequest{
			method: request.Method,
			path:   request.URL.EscapedPath(),
			query:  request.URL.RawQuery,
			header: request.Header.Clone(),
		})
		fixture.mutex.Unlock()
		reply, found := replies[request.URL.EscapedPath()+"?"+request.URL.RawQuery]
		if !found {
			reply, found = replies[request.URL.EscapedPath()]
		}
		if !found {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(reply.status)
		_, _ = writer.Write([]byte(reply.body))
	}))
	t.Cleanup(fixture.server.Close)
	fixture.client = Client{HTTPClient: fixture.server.Client(), APIBaseURL: fixture.server.URL}
	return fixture
}

func (fixture *apiFixture) observed() []apiRequest {
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	return append([]apiRequest(nil), fixture.requests...)
}

func TestResolveExactPublicRef(t *testing.T) {
	for _, ref := range []string{"", "main", "v1.2.3", "feature/source", "feature/caf\u00e9", "feature/\u4fee\u590d", "refs/tags/v1.2.3", fixtureCommit} {
		t.Run(ref, func(t *testing.T) {
			expectedRef := ref
			if expectedRef == "" {
				expectedRef = "main"
			}
			fixture := newAPIFixture(t, map[string]apiReply{
				fixturePrefix: {http.StatusOK, repositoryJSON()},
				fixturePrefix + "/commits/" + url.PathEscape(expectedRef): {http.StatusOK, commitJSON(fixtureCommit, fixtureTree)},
			})
			target, err := fixture.client.Resolve(t.Context(), fixtureURL+".git", ref)
			require.NoError(t, err)
			require.Equal(t, Target{
				Repository: Repository{URL: fixtureURL, Owner: "source-fixtures", Name: "project", DefaultBranch: "main"},
				Ref:        expectedRef, Commit: fixtureCommit, Tree: fixtureTree,
			}, target)
			requests := fixture.observed()
			require.Len(t, requests, 2)
			require.Equal(t, fixturePrefix, requests[0].path)
			require.Equal(t, fixturePrefix+"/commits/"+url.PathEscape(expectedRef), requests[1].path)
			for _, request := range requests {
				require.Equal(t, http.MethodGet, request.method)
				require.Empty(t, request.header.Get("Authorization"))
				require.Empty(t, request.header.Get("Cookie"))
			}
		})
	}
}

func TestResolveRefFailuresNeverSelectFallback(t *testing.T) {
	for _, ref := range []string{"missing", "v1.x", fixtureCommit} {
		t.Run(ref, func(t *testing.T) {
			fixture := newAPIFixture(t, map[string]apiReply{fixturePrefix: {http.StatusOK, repositoryJSON()}})
			target, err := fixture.client.Resolve(t.Context(), fixtureURL, ref)
			require.ErrorContains(t, err, "ref lookup returned HTTP 404")
			require.Empty(t, target)
			requests := fixture.observed()
			require.Len(t, requests, 2)
			require.Equal(t, fixturePrefix+"/commits/"+ref, requests[1].path)
		})
	}
}

func TestResolveRejectsNoncanonicalRepositoryURLsBeforeHTTP(t *testing.T) {
	fixture := newAPIFixture(t, nil)
	for _, input := range []string{
		"", " ", "https://github.com", fixtureURL + "/", fixtureURL + "/tree/main", fixtureURL + "?x=y",
		fixtureURL + "?", fixtureURL + "#fragment", fixtureURL + "#", fixtureURL + "%00",
		"http://github.com/source-fixtures/project", "ssh://github.com/source-fixtures/project",
		"git@github.com:source-fixtures/project.git", (&url.URL{
			Scheme: "https",
			Host:   "github.com",
			Path:   "/source-fixtures/project",
			User:   url.User("SYNTHETIC_USER_" + rand.Text()),
		}).String(),
		"https://github.com:443/source-fixtures/project", "https://github.com./source-fixtures/project",
		"https://GITHUB.COM/source-fixtures/project", "https://github.com.evil.invalid/source-fixtures/project",
		"https://github.com/source-fixtures/..", "https://github.com/source-fixtures/.",
		"https://github.com/source-fixtures/.git", "https://github.com/source-fixtures/%2e%2e",
		"https://github.com/source-fixtures/project.git.git",
		"https://github.com/source-fixtures%2fother/project", "https://github.com/source-fixtures/project.git/../other",
		"https://github.com/-owner/project", "https://github.com/owner-/project",
		"https://github.com/" + strings.Repeat("a", 40) + "/project",
		"https://github.com/owner/" + strings.Repeat("a", 101),
		fixtureURL + "\n", " " + fixtureURL, "https://github.com/source-fixtures\\project",
	} {
		_, err := fixture.client.Resolve(t.Context(), input, "")
		require.Error(t, err, "URL should be rejected")
	}
	require.Empty(t, fixture.observed())
	for _, input := range []string{fixtureURL, fixtureURL + ".git", "https://github.com/Owner/.github", "https://github.com/a/r..r"} {
		_, err := parseRepositoryURL(input)
		require.NoError(t, err)
	}
}

func TestResolveRejectsRevisionExpressions(t *testing.T) {
	fixture := newAPIFixture(t, nil)
	for _, ref := range []string{
		" ", "\n", "main\n", " main", "main ", "--help", "HEAD~1", "HEAD^", "main:README.md", "v1..v2",
		"v1...v2", "^v1", "~1.2", ">=1.2 <2", "v*", "main@{1}", "@", "bad\\ref", "a//b",
		"/main", "main/", ".hidden", "a/.hidden", "a.lock", "main.", strings.Repeat("a", maxRefBytes+1),
		"bad\x00ref", "branch\x7f", "branch\u200b", "branch\u00a0name", string([]byte{0xff}),
	} {
		_, err := fixture.client.Resolve(t.Context(), fixtureURL, ref)
		require.Error(t, err)
	}
	require.Empty(t, fixture.observed())
}

func TestResolveRequiresExplicitConsistentPublicMetadata(t *testing.T) {
	for _, metadata := range []string{
		strings.Replace(repositoryJSON(), `"private":false`, `"private":true`, 1),
		strings.Replace(repositoryJSON(), `"visibility":"public"`, `"visibility":"private"`, 1),
		strings.Replace(repositoryJSON(), `"visibility":"public"`, `"visibility":"internal"`, 1),
		strings.Replace(repositoryJSON(), `"private":false,`, "", 1),
		strings.Replace(repositoryJSON(), `"private":false`, `"private":null`, 1),
		strings.Replace(repositoryJSON(), `"private":false`, `"private":"false"`, 1),
		strings.Replace(repositoryJSON(), `"visibility":"public",`, "", 1),
		strings.Replace(repositoryJSON(), `"default_branch":"main"`, `"default_branch":""`, 1),
		strings.Replace(repositoryJSON(), `"archived":false`, `"archived":null`, 1),
		strings.Replace(repositoryJSON(), `"full_name":"source-fixtures/project"`, `"full_name":"other/project"`, 1),
		strings.Replace(repositoryJSON(), `"login":"source-fixtures"`, `"login":"other"`, 1),
		strings.Replace(repositoryJSON(), `"name":"project"`, `"name":"different"`, 1),
		strings.Replace(repositoryJSON(), fixtureURL, "https://github.com/other/project", 1),
		strings.Replace(repositoryJSON(), fixtureURL, "https://elsewhere.invalid/project", 1),
		`null`, `{}`, `{"private":false,"visibility":"public"}`,
	} {
		fixture := newAPIFixture(t, map[string]apiReply{fixturePrefix: {http.StatusOK, metadata}})
		_, err := fixture.client.Resolve(t.Context(), fixtureURL, "")
		require.Error(t, err)
		require.Len(t, fixture.observed(), 1, "source must not be queried before public identity confirmation")
	}
}

func TestResolveArchivedRepositoryAndCanonicalMetadata(t *testing.T) {
	metadata := strings.Replace(repositoryJSON(), `"archived":false`, `"archived":true`, 1)
	metadata = strings.ReplaceAll(metadata, "source-fixtures", "Source-Fixtures")
	fixture := newAPIFixture(t, map[string]apiReply{
		fixturePrefix:                   {http.StatusOK, metadata},
		fixturePrefix + "/commits/main": {http.StatusOK, commitJSON(fixtureCommit, fixtureTree)},
	})
	target, err := fixture.client.Resolve(t.Context(), fixtureURL, "")
	require.NoError(t, err)
	require.True(t, target.Repository.Archived)
	require.Equal(t, "Source-Fixtures", target.Repository.Owner)
	require.Equal(t, "https://github.com/Source-Fixtures/project", target.Repository.URL)
}

func TestResolveRejectsInexactIdentities(t *testing.T) {
	for _, response := range []string{
		`null`, `{}`, `{"sha":"` + fixtureCommit + `"}`, commitJSON("abc123", fixtureTree),
		commitJSON(strings.ToUpper(fixtureCommit), fixtureTree), commitJSON(fixtureCommit, "main"),
		commitJSON(strings.Repeat("0", 40), fixtureTree), commitJSON(fixtureCommit, strings.Repeat("f", 64)),
		commitJSON(strings.Repeat("e", 40), fixtureTree),
	} {
		fixture := newAPIFixture(t, map[string]apiReply{
			fixturePrefix: {http.StatusOK, repositoryJSON()},
			fixturePrefix + "/commits/" + fixtureCommit: {http.StatusOK, response},
		})
		_, err := fixture.client.Resolve(t.Context(), fixtureURL, fixtureCommit)
		require.Error(t, err)
	}
}

func TestResolveRejectsHostileAndOversizedReplies(t *testing.T) {
	for _, body := range []string{
		repositoryJSON() + `{}`, `<html>not an API response</html>`,
		strings.Replace(repositoryJSON(), `"private":false`, `"private":true,"private":false`, 1),
		strings.Replace(repositoryJSON(), `"private":false`, `"private":true,"PRIVATE":false`, 1),
		strings.Replace(repositoryJSON(), `"private":false`, `"private":true,"\u0070rivate":false`, 1),
		strings.Replace(repositoryJSON(), `"login":"source-fixtures"`, `"login":"other","login":"source-fixtures"`, 1),
		strings.Repeat("[", 40) + "0" + strings.Repeat("]", 40),
		strings.Repeat(" ", apiMaxReplyBytes+1) + repositoryJSON(),
		strings.Replace(repositoryJSON(), `"name":"project"`, "\"name\":\"pro\xffject\"", 1),
	} {
		fixture := newAPIFixture(t, map[string]apiReply{fixturePrefix: {http.StatusOK, body}})
		_, err := fixture.client.Resolve(t.Context(), fixtureURL, "")
		require.Error(t, err)
		require.Len(t, fixture.observed(), 1)
	}
	for _, code := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError} {
		fixture := newAPIFixture(t, map[string]apiReply{fixturePrefix: {code, "synthetic-response-must-not-be-returned"}})
		_, err := fixture.client.Resolve(t.Context(), fixtureURL, "")
		require.ErrorContains(t, err, fmt.Sprintf("HTTP %d", code))
		require.NotContains(t, err.Error(), "synthetic-response")
	}
}

func TestResolveRejectsEveryRedirect(t *testing.T) {
	var destinationRequests atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		destinationRequests.Add(1)
	}))
	t.Cleanup(destination.Close)
	password := "SYNTHETIC_PASSWORD_" + rand.Text()
	credentials := url.UserPassword("SYNTHETIC_USER_"+rand.Text(), password)
	credentialURL := (&url.URL{
		Scheme: "http",
		Host:   "127.0.0.1",
		Path:   "/",
		User:   credentials,
	}).String()
	for _, redirect := range []struct{ name, location string }{
		{"absolute", destination.URL},
		{"relative", "/different"},
		{"foreign-host", "https://api.github.com/other"},
		{"credentials", credentialURL},
	} {
		t.Run(redirect.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				require.Equal(t, fixturePrefix, request.URL.Path)
				http.Redirect(writer, request, redirect.location, http.StatusFound)
			}))
			t.Cleanup(server.Close)
			client := Client{APIBaseURL: server.URL, HTTPClient: server.Client()}
			client.HTTPClient.CheckRedirect = func(*http.Request, []*http.Request) error {
				t.Error("caller redirect policy must not run")
				return nil
			}
			_, err := client.Resolve(t.Context(), fixtureURL, "")
			require.ErrorContains(t, err, "HTTP 302")
			require.NotContains(t, err.Error(), credentials.Username())
			require.NotContains(t, err.Error(), password)
		})
	}
	require.Zero(t, destinationRequests.Load())
}

func TestResolveIsAnonymousDespiteAmbientCredentials(t *testing.T) {
	t.Setenv("GH_TOKEN", "synthetic-token-only")
	t.Setenv("GITHUB_TOKEN", "synthetic-token-only")
	t.Setenv("HTTPS_PROXY", (&url.URL{
		Scheme: "http",
		Host:   "127.0.0.1:1",
		User:   url.UserPassword("SYNTHETIC_USER_"+rand.Text(), "SYNTHETIC_PASSWORD_"+rand.Text()),
	}).String())
	fixture := newAPIFixture(t, map[string]apiReply{
		fixturePrefix:                   {http.StatusOK, repositoryJSON()},
		fixturePrefix + "/commits/main": {http.StatusOK, commitJSON(fixtureCommit, fixtureTree)},
	})
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	endpoint, err := url.Parse(fixture.server.URL)
	require.NoError(t, err)
	jar.SetCookies(endpoint, []*http.Cookie{{Name: "private-session", Value: "synthetic-cookie-only"}})
	fixture.client.HTTPClient.Jar = jar
	transport := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) {
		t.Error("caller proxy must not run")
		return nil, nil
	}}
	fixture.client.HTTPClient.Transport = transport
	_, err = fixture.client.Resolve(t.Context(), fixtureURL, "")
	require.NoError(t, err)
	require.Same(t, jar, fixture.client.HTTPClient.Jar)
	require.NotNil(t, transport.Proxy, "caller-owned transport must not be mutated")
	for _, request := range fixture.observed() {
		require.Empty(t, request.header.Get("Authorization"))
		require.Empty(t, request.header.Get("Proxy-Authorization"))
		require.Empty(t, request.header.Get("Cookie"))
	}
}

func TestResolveAPIOverrideAndDeadlines(t *testing.T) {
	for _, base := range []string{
		"https://elsewhere.invalid", "http://api.github.com", "https://api.github.com/",
		"http://127.0.0.1/path", "http://127.0.0.1?x=y", "http://127.0.0.1?", "http://127.0.0.1#",
		(&url.URL{
			Scheme: "http",
			Host:   "127.0.0.1",
			User:   url.UserPassword("SYNTHETIC_USER_"+rand.Text(), "SYNTHETIC_PASSWORD_"+rand.Text()),
		}).String(), "file:///tmp", "http://0.0.0.0", "http://127.0.0.1.evil.invalid",
	} {
		_, err := (Client{APIBaseURL: base}).apiBase()
		require.Error(t, err)
	}
	for _, base := range []string{"", githubAPIBase, "http://127.0.0.1:1234", "https://[::1]:1234"} {
		_, err := (Client{APIBaseURL: base}).apiBase()
		require.NoError(t, err)
	}
	fixture := newAPIFixture(t, nil)
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := fixture.client.Resolve(cancelled, fixtureURL, "")
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, fixture.observed())

	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
	}))
	t.Cleanup(server.Close)
	deadline, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	_, err = (Client{APIBaseURL: server.URL, HTTPClient: server.Client()}).Resolve(deadline, fixtureURL, "")
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestAnonymousClientClonesTLSAndBoundsConfiguration(t *testing.T) {
	original := &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS10, Certificates: []tls.Certificate{{}}, InsecureSkipVerify: true,
		ServerName: "not-github.invalid", ClientSessionCache: tls.NewLRUClientSessionCache(1),
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			t.Error("client identity must not be requested")
			return nil, nil
		},
	}}
	configured := Client{HTTPClient: &http.Client{Transport: original, Timeout: time.Hour}}
	client, transport, err := configured.anonymousClient()
	require.NoError(t, err)
	t.Cleanup(transport.CloseIdleConnections)
	require.Equal(t, apiTimeout, client.Timeout)
	require.NotSame(t, original, transport)
	require.Empty(t, transport.TLSClientConfig.Certificates)
	require.Nil(t, transport.TLSClientConfig.GetClientCertificate)
	require.Nil(t, transport.TLSClientConfig.ClientSessionCache)
	require.False(t, transport.TLSClientConfig.InsecureSkipVerify)
	require.Empty(t, transport.TLSClientConfig.ServerName)
	require.EqualValues(t, tls.VersionTLS12, transport.TLSClientConfig.MinVersion)
	require.Len(t, original.TLSClientConfig.Certificates, 1)
	require.NotNil(t, original.TLSClientConfig.GetClientCertificate)
	require.EqualValues(t, apiMaxHeaderBytes, transport.MaxResponseHeaderBytes)
	require.Equal(t, 10*time.Second, transport.ResponseHeaderTimeout)
	require.True(t, transport.DisableCompression)
}

func TestResolveResponseByteBoundariesAndCompression(t *testing.T) {
	for _, size := range []int{apiMaxReplyBytes - 1, apiMaxReplyBytes, apiMaxReplyBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			body := repositoryJSON() + strings.Repeat(" ", size-len(repositoryJSON()))
			fixture := newAPIFixture(t, map[string]apiReply{
				fixturePrefix:                   {http.StatusOK, body},
				fixturePrefix + "/commits/main": {http.StatusOK, commitJSON(fixtureCommit, fixtureTree)},
			})
			_, err := fixture.client.Resolve(t.Context(), fixtureURL, "")
			if size <= apiMaxReplyBytes {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "limit")
				require.Len(t, fixture.observed(), 1)
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Encoding", "gzip")
		_, _ = writer.Write([]byte(repositoryJSON()))
	}))
	t.Cleanup(server.Close)
	_, err := (Client{APIBaseURL: server.URL, HTTPClient: server.Client()}).Resolve(t.Context(), fixtureURL, "")
	require.ErrorContains(t, err, "uncompressed response limit")
}

func TestResolveLoopbackTLSWithoutClientIdentity(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Empty(t, request.TLS.PeerCertificates)
		if request.URL.Path == fixturePrefix {
			_, _ = writer.Write([]byte(repositoryJSON()))
			return
		}
		_, _ = writer.Write([]byte(commitJSON(fixtureCommit, fixtureTree)))
	}))
	t.Cleanup(server.Close)
	target, err := (Client{APIBaseURL: server.URL, HTTPClient: server.Client()}).Resolve(t.Context(), fixtureURL, "main")
	require.NoError(t, err)
	require.Equal(t, fixtureCommit, target.Commit)
}

func TestTargetJSONShape(t *testing.T) {
	target := Target{Repository: Repository{
		URL: fixtureURL, Owner: "source-fixtures", Name: "project", DefaultBranch: "main",
	}, Ref: "main", Commit: fixtureCommit, Tree: fixtureTree}
	encoded, err := json.Marshal(target)
	require.NoError(t, err)
	require.JSONEq(t, `{"repository":{"url":"`+fixtureURL+`","owner":"source-fixtures","name":"project","defaultBranch":"main","archived":false},"ref":"main","commit":"`+fixtureCommit+`","tree":"`+fixtureTree+`"}`, string(encoded))
	var decoded Target
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.Equal(t, target, decoded)
}
