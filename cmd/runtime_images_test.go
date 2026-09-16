package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/api"
	"github.com/orka-agents/orka/internal/controller"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestConfiguredACPRuntimeAvailabilityRequiresModelConnection(t *testing.T) {
	image := "ghcr.io/orka-agents/runtime@sha256:" + strings.Repeat("a", 64)
	images := controller.ACPRuntimeImages{Codex: image, Claude: image, Copilot: image, Opencode: image}
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte(strings.Repeat("a", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	proxy := controller.RuntimePoolProviderProxyConfig{
		BaseURL: "http://provider-auth-proxy.orka-system.svc:8080", Namespace: "orka-system",
		PodLabels:       map[string]string{"orka.ai/network-role": "provider-auth-proxy"},
		BearerTokenFile: tokenPath,
	}
	missingToken := proxy
	missingToken.BearerTokenFile = ""
	unreadableToken := proxy
	unreadableToken.BearerTokenFile = filepath.Join(t.TempDir(), "missing")
	wrongNamespace := proxy
	wrongNamespace.Namespace = "other"
	for _, test := range []struct {
		name   string
		images controller.ACPRuntimeImages
		proxy  controller.RuntimePoolProviderProxyConfig
		want   string
	}{
		{name: "no gateway", images: images, want: "none"},
		{name: "no credentials", images: images, proxy: missingToken, want: "none"},
		{name: "unreadable credentials", images: images, proxy: unreadableToken, want: "none"},
		{name: "invalid connection", images: images, proxy: wrongNamespace, want: "none"},
		{name: "configured", images: images, proxy: proxy, want: "codex, copilot, claude, opencode"},
		{name: "only configured images", images: controller.ACPRuntimeImages{Codex: image}, proxy: proxy, want: "codex"},
	} {
		t.Run(test.name, func(t *testing.T) {
			availability := configuredACPRuntimeAvailability(test.images, test.proxy)
			builder := api.NewSystemPromptBuilder(
				fake.NewClientBuilder().WithScheme(scheme).Build(), "orka-system", availability,
			)
			prompt, err := builder.BuildSystemPrompt(t.Context(), "", api.PromptModeMinimal)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(prompt, "agent_runtimes=["+test.want+"]") {
				t.Fatalf("chat did not advertise the expected runtimes: %s", test.want)
			}
			if !strings.Contains(prompt, "container=yes") {
				t.Fatal("model connection configuration disabled container tasks")
			}
		})
	}
}

func TestResolveACPRuntimeImagesPreservesIndexDigestWithAnonymousAuth(t *testing.T) {
	index := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[` +
		`{"platform":{"os":"linux","architecture":"amd64"}},` +
		`{"platform":{"os":"linux","architecture":"arm64"}}]}`
	hash := sha256.Sum256([]byte(index))
	indexDigest := "sha256:" + hex.EncodeToString(hash[:])
	token := hex.EncodeToString([]byte(t.Name()))
	var manifestRequests, tokenRequests atomic.Int32
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			tokenRequests.Add(1)
			if r.Method != http.MethodGet || r.Header.Get("Authorization") != "" {
				t.Error("anonymous token request used an unexpected method or credentials")
			}
			if r.URL.Query().Get("scope") != "repository:orka/runtime:pull" ||
				r.URL.Query().Get("service") != "test-registry" {
				t.Error("anonymous token request omitted its repository pull scope or service")
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]string{"token": token}); err != nil {
				t.Error("could not write anonymous token response")
			}
		case "/v2/orka/runtime/manifests/v0.2.0", "/v2/orka/runtime/manifests/v0.3.0":
			manifestRequests.Add(1)
			if r.Method != http.MethodHead {
				t.Error("resolver fetched a manifest instead of resolving its digest")
			}
			if !strings.Contains(r.Header.Get("Accept"), "application/vnd.oci.image.index.v1+json") ||
				!strings.Contains(r.Header.Get("Accept"), "application/vnd.docker.distribution.manifest.list.v2+json") {
				t.Error("resolver did not accept multi-platform image indexes")
			}
			if r.Header.Get("Authorization") != "Bearer "+token {
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(
					`Bearer realm="%s/token",service="test-registry"`, server.URL))
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			writeRuntimeImageDescriptor(w, indexDigest)
		default:
			t.Error("resolver requested a platform manifest, layer, or unexpected endpoint")
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	repository := strings.TrimPrefix(server.URL, "https://") + "/orka/runtime"
	digestOverride := "ghcr.io/orka-agents/claude@sha256:" + strings.Repeat("b", 64)
	configured := controller.ACPRuntimeImages{
		Codex: repository + ":v0.2.0", Claude: digestOverride, Copilot: repository + ":v0.3.0",
	}
	resolved, err := resolveACPRuntimeImages(t.Context(), configured, server.Client())
	if err != nil {
		t.Fatal("could not resolve images through anonymous registry authentication")
	}
	want := controller.ACPRuntimeImages{
		Codex: repository + "@" + indexDigest, Claude: digestOverride, Copilot: repository + "@" + indexDigest,
	}
	if resolved != want {
		t.Fatalf("resolved images = %#v, want %#v", resolved, want)
	}
	if manifestRequests.Load() != 3 || tokenRequests.Load() != 1 {
		t.Fatalf("manifest requests = %d, token requests = %d; want 3 and 1 with cached auth",
			manifestRequests.Load(), tokenRequests.Load())
	}
}

func TestResolveACPRuntimeImagesBypassesDisabledAndDigestReferences(t *testing.T) {
	digestA := "ghcr.io/orka-agents/runtime@sha256:" + strings.Repeat("a", 64)
	digestB := "ghcr.io/orka-agents/runtime@sha256:" + strings.Repeat("b", 64)
	digestZero := "ghcr.io/orka-agents/runtime@sha256:" + strings.Repeat("0", 64)
	for image, want := range map[string]string{
		"":         "",
		"  ":       "",
		digestA:    digestA,
		digestZero: digestZero,
		// A tag beside the digest is dropped so the result is admissible.
		"ghcr.io/orka-agents/runtime:v0.2.0@sha256:" + strings.Repeat("b", 64): digestB,
	} {
		t.Run(image, func(t *testing.T) {
			var requests atomic.Int32
			client := &http.Client{Transport: runtimeImageRoundTripperFunc(func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				return nil, errors.New("unexpected registry request")
			})}
			resolved, err := resolveACPRuntimeImages(t.Context(), controller.ACPRuntimeImages{Codex: image}, client)
			if err != nil || resolved.Codex != want || requests.Load() != 0 {
				t.Fatalf("digest/disabled configuration changed or contacted a registry: images=%#v err=%v requests=%d",
					resolved, err, requests.Load())
			}
		})
	}
}

func TestResolveACPRuntimeImagesCanonicalizesTaggedDigestReferences(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	var requests atomic.Int32
	client := &http.Client{Transport: runtimeImageRoundTripperFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("unexpected registry request")
	})}
	resolved, err := resolveACPRuntimeImages(t.Context(),
		controller.ACPRuntimeImages{Codex: "ghcr.io/orka-agents/runtime:v0.2.0@" + digest}, client)
	if err != nil || requests.Load() != 0 {
		t.Fatalf("tag+digest reference rejected or contacted a registry: err=%v requests=%d", err, requests.Load())
	}
	if want := "ghcr.io/orka-agents/runtime@" + digest; resolved.Codex != want {
		t.Fatalf("resolved = %q, want canonical %q", resolved.Codex, want)
	}
	if !controller.ACPRuntimeImageAvailable(resolved.Codex) {
		t.Fatalf("canonical reference %q is not admissible", resolved.Codex)
	}
}

func TestResolveACPRuntimeImagesRejectsMalformedReferencesWithoutRegistryAccess(t *testing.T) {
	for _, image := range []string{
		"runtime:v0.2.0", "orka/runtime:v0.2.0",
		"ghcr.io/orka-agents/runtime",
		"https://ghcr.io/orka-agents/runtime:v0.2.0",
		"http://ghcr.io/orka-agents/runtime:v0.2.0",
		"ghcr.io/orka-agents/Runtime:v0.2.0",
		"ghcr.io/orka-agents/runtime:v0.2.0\ninjected",
		"ghcr.io/orka-agents/runtime@sha256:abc",
		"ghcr.io/orka-agents/runtime@sha256:" + strings.Repeat("g", 64),
		"ghcr.io/orka-agents/runtime@sha512:" + strings.Repeat("a", 128),
		"ghcr.io/orka-agents/runtime:bad!tag@sha256:" + strings.Repeat("a", 64),
	} {
		t.Run(image, func(t *testing.T) {
			var requests atomic.Int32
			client := &http.Client{Transport: runtimeImageRoundTripperFunc(func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				return nil, errors.New("unexpected registry request")
			})}
			resolved, err := resolveACPRuntimeImages(t.Context(), controller.ACPRuntimeImages{Codex: image}, client)
			if err == nil || resolved != (controller.ACPRuntimeImages{}) || requests.Load() != 0 {
				t.Fatalf("invalid reference accepted or contacted a registry: images=%#v err=%v requests=%d",
					resolved, err, requests.Load())
			}
		})
	}
}

func TestResolveACPRuntimeImagesOrDisableDisablesEveryRuntimeOnFailure(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	client := &http.Client{Transport: runtimeImageRoundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("registry unreachable")
	})}
	images, err := resolveACPRuntimeImagesOrDisable(t.Context(), controller.ACPRuntimeImages{
		Codex:  "ghcr.io/orka-agents/runtime@" + digest, // already pinned, needs no lookup
		Claude: "ghcr.io/orka-agents/runtime:v0.2.0",    // needs the unreachable registry
	}, client)
	if err == nil {
		t.Fatal("an unreachable registry must be reported")
	}
	if images != (controller.ACPRuntimeImages{}) {
		t.Fatalf("a failed resolution must disable every runtime, not keep a partial set: %#v", images)
	}
	if controller.ACPRuntimeImageAvailable(images.Codex) || controller.ACPRuntimeImageAvailable(images.Claude) {
		t.Fatalf("disabled runtimes must be unavailable to pools and admission: %#v", images)
	}
}

func TestResolveACPRuntimeImagesFailsWithoutPartialOrMutableFallback(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		digest string
	}{
		{name: "not found", status: http.StatusNotFound},
		{name: "unauthorized", status: http.StatusUnauthorized},
		{name: "denied", status: http.StatusForbidden},
		{name: "rate limited", status: http.StatusTooManyRequests},
		{name: "unavailable", status: http.StatusServiceUnavailable},
		{name: "missing digest", status: http.StatusOK},
		{name: "malformed digest", status: http.StatusOK, digest: "sha256:abc"},
		{name: "placeholder digest", status: http.StatusOK, digest: "sha256:" + strings.Repeat("0", 64)},
		{name: "other algorithm", status: http.StatusOK, digest: "sha512:" + strings.Repeat("a", 128)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path == "/v2/orka/runtime/manifests/good" {
					writeRuntimeImageDescriptor(w, "sha256:"+strings.Repeat("a", 64))
					return
				}
				if tt.status != http.StatusOK {
					w.WriteHeader(tt.status)
					return
				}
				writeRuntimeImageDescriptor(w, tt.digest)
			}))
			defer server.Close()
			repository := strings.TrimPrefix(server.URL, "https://") + "/orka/runtime"
			resolved, err := resolveACPRuntimeImages(t.Context(), controller.ACPRuntimeImages{
				Codex: repository + ":good", Claude: repository + ":bad",
			}, server.Client())
			if err == nil || resolved != (controller.ACPRuntimeImages{}) || requests.Load() != 2 {
				t.Fatalf("failed lookup returned partial/mutable settings or retried: images=%#v err=%v requests=%d",
					resolved, err, requests.Load())
			}
			if !strings.Contains(err.Error(), "claude ACP runtime image") {
				t.Fatalf("error did not identify the failed provider: %v", err)
			}
		})
	}
}

func TestResolveACPRuntimeImagesHonorsCancellationAndRequestTimeout(t *testing.T) {
	for _, cancelContext := range []bool{true, false} {
		t.Run(strconv.FormatBool(cancelContext), func(t *testing.T) {
			started := make(chan struct{})
			server := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				close(started)
				<-r.Context().Done()
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			client := server.Client()
			if !cancelContext {
				client.Timeout = 100 * time.Millisecond
			}
			result := make(chan error, 1)
			go func() {
				resolved, err := resolveACPRuntimeImages(ctx, controller.ACPRuntimeImages{
					Codex: strings.TrimPrefix(server.URL, "https://") + "/orka/runtime:v0.2.0",
				}, client)
				if resolved != (controller.ACPRuntimeImages{}) {
					t.Error("interrupted resolution returned runtime image settings")
				}
				result <- err
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("registry request did not start")
			}
			want := context.DeadlineExceeded
			if cancelContext {
				cancel()
				want = context.Canceled
			}
			select {
			case err := <-result:
				if !errors.Is(err, want) {
					t.Fatalf("interrupted resolution error = %v, want %v", err, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("registry resolution ignored cancellation or request timeout")
			}
		})
	}
}

func TestResolveACPRuntimeImagesRejectsHTTPSDowngrades(t *testing.T) {
	for _, path := range []string{"manifest redirect", "token realm", "token redirect"} {
		t.Run(path, func(t *testing.T) {
			var insecureRequests atomic.Int32
			insecure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				insecureRequests.Add(1)
				writeRuntimeImageDescriptor(w, "sha256:"+strings.Repeat("a", 64))
			}))
			defer insecure.Close()
			var server *httptest.Server
			server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if path == "manifest redirect" || r.URL.Path == "/token" {
					http.Redirect(w, r, insecure.URL, http.StatusFound)
					return
				}
				realm := server.URL + "/token"
				if path == "token realm" {
					realm = insecure.URL
				}
				w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s"`, realm))
				w.WriteHeader(http.StatusUnauthorized)
			}))
			defer server.Close()
			resolved, err := resolveACPRuntimeImages(t.Context(), controller.ACPRuntimeImages{
				Codex: strings.TrimPrefix(server.URL, "https://") + "/orka/runtime:v0.2.0",
			}, server.Client())
			if err == nil || resolved != (controller.ACPRuntimeImages{}) || insecureRequests.Load() != 0 {
				t.Fatalf("HTTPS downgrade accepted: images=%#v err=%v insecure requests=%d",
					resolved, err, insecureRequests.Load())
			}
		})
	}
}

func TestResolveACPRuntimeImagesRejectsUntrustedTLS(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writeRuntimeImageDescriptor(w, "sha256:"+strings.Repeat("a", 64))
	}))
	defer server.Close()
	resolved, err := resolveACPRuntimeImages(t.Context(), controller.ACPRuntimeImages{
		Codex: strings.TrimPrefix(server.URL, "https://") + "/orka/runtime:v0.2.0",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "TLS certificate verification failed") ||
		resolved != (controller.ACPRuntimeImages{}) || requests.Load() != 0 {
		t.Fatalf("untrusted registry certificate was accepted or misreported: images=%#v err=%v requests=%d",
			resolved, err, requests.Load())
	}
}

func TestResolveACPRuntimeImagesDoesNotExposeRegistryAuthErrors(t *testing.T) {
	sensitive := hex.EncodeToString([]byte(t.Name()))
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			if err := json.NewEncoder(w).Encode(map[string]any{
				"errors": []map[string]string{{"code": "DENIED", "message": sensitive, "detail": sensitive}},
			}); err != nil {
				t.Error("could not write registry error response")
			}
			return
		}
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token?private=%s"`, server.URL, sensitive))
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	resolved, err := resolveACPRuntimeImages(t.Context(), controller.ACPRuntimeImages{
		Codex: strings.TrimPrefix(server.URL, "https://") + "/orka/runtime:v0.2.0",
	}, server.Client())
	if err == nil || resolved != (controller.ACPRuntimeImages{}) {
		t.Fatal("failed registry authentication was accepted")
	}
	if strings.Contains(err.Error(), sensitive) || strings.Contains(err.Error(), server.URL) {
		t.Fatal("registry error exposed authentication response details")
	}
	if !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatal("registry error omitted its safe HTTP status")
	}
}

func writeRuntimeImageDescriptor(w http.ResponseWriter, digest string) {
	w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
	w.Header().Set("Content-Length", "256")
	w.Header().Set("Docker-Content-Digest", digest)
	w.WriteHeader(http.StatusOK)
}

type runtimeImageRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f runtimeImageRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
