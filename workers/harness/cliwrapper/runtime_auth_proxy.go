package cliwrapper

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/orka-agents/orka/internal/workerenv"
)

const (
	defaultOpenAIEndpoint    = "https://api.openai.com/v1"
	defaultAnthropicEndpoint = "https://api.anthropic.com"
	runtimeAuthProxyHTTP     = "http"
	runtimeAuthProxyHTTPS    = "https"
)

var runtimeAuthChildBoundaryAvailable = func() bool {
	_, _, ok := childCredentialIDs()
	return ok
}

type runtimeAuthProxyMode string

const (
	runtimeAuthProxyOpenAI    runtimeAuthProxyMode = "openai"
	runtimeAuthProxyAnthropic runtimeAuthProxyMode = "anthropic"
)

func protectRuntimeAuthTurn(turn TurnContext) (TurnContext, func(), error) {
	runtimeName := strings.ToLower(strings.TrimSpace(turn.RuntimeName))
	if runtimeName == RuntimeMulti {
		runtimeName = strings.ToLower(strings.TrimSpace(turn.Metadata["runtime"]))
	}
	runtimeAuthOnly := strings.EqualFold(strings.TrimSpace(turn.Metadata["runtimeAuthOnly"]), "true")
	readOnlyCodex := runtimeName == RuntimeCodex && strings.EqualFold(strings.TrimSpace(turn.Metadata["readOnly"]), "true")
	if !runtimeAuthOnly && !readOnlyCodex {
		return turn, func() {}, nil
	}
	var mode runtimeAuthProxyMode
	var upstreamValue, endpoint string
	switch runtimeName {
	case RuntimeCodex:
		mode = runtimeAuthProxyOpenAI
		upstreamValue = firstNonEmpty(
			envEntryValue(turn.Env, workerenv.CodexAPIKey),
			envEntryValue(turn.Env, workerenv.OpenAIAPIKey),
		)
		endpoint = firstNonEmpty(envEntryValue(turn.Env, workerenv.OpenAIBaseURL), defaultOpenAIEndpoint)
	case RuntimeClaude:
		foundryValue := strings.TrimSpace(envEntryValue(turn.Env, "ANTHROPIC_FOUNDRY_API_KEY"))
		if foundryValue != "" || runtimeAuthProxyEnvFlagEnabled(turn.Env, "CLAUDE_CODE_USE_FOUNDRY") {
			return turn, nil, fmt.Errorf("runtime-auth-only Claude implementation does not support Azure AI Foundry")
		}
		mode = runtimeAuthProxyAnthropic
		upstreamValue = envEntryValue(turn.Env, workerenv.AnthropicAPIKey)
		endpoint = firstNonEmpty(envEntryValue(turn.Env, workerenv.AnthropicBaseURL), defaultAnthropicEndpoint)
	default:
		return turn, nil, fmt.Errorf("runtime-auth-only credential proxy does not support runtime %q", runtimeName)
	}
	// The wrapper retains the upstream value as root. The CLI and all tool
	// subprocesses must run under the configured non-root child identity so
	// they cannot inspect the wrapper process environment or memory.
	if !runtimeAuthChildBoundaryAvailable() {
		return turn, nil, fmt.Errorf("runtime-auth-only credential proxy requires a dedicated non-root child UID/GID")
	}
	if strings.TrimSpace(upstreamValue) == "" {
		return turn, nil, fmt.Errorf(
			"runtime-auth-only credential proxy has no upstream credential for runtime %q",
			runtimeName,
		)
	}
	upstream, err := url.Parse(endpoint)
	validScheme := upstream != nil && (upstream.Scheme == runtimeAuthProxyHTTP || upstream.Scheme == runtimeAuthProxyHTTPS)
	if err != nil || upstream == nil || !validScheme || upstream.Host == "" || upstream.User != nil {
		return turn, nil, fmt.Errorf("runtime-auth-only credential proxy endpoint is invalid for runtime %q", runtimeName)
	}
	token, err := newRuntimeAuthProxyToken()
	if err != nil {
		return turn, nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return turn, nil, fmt.Errorf("listen for runtime-auth-only credential proxy: %w", err)
	}
	proxy := newRuntimeAuthReverseProxy(upstream, mode, upstreamValue, token)
	server := &http.Server{Handler: proxy, ReadHeaderTimeout: 10 * time.Second}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = server.Serve(listener)
	}()
	var closeOnce sync.Once
	closeProxy := func() {
		closeOnce.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := server.Shutdown(ctx); err != nil {
				_ = server.Close()
			}
			_ = listener.Close()
			<-serveDone
		})
	}
	localEndpoint := (&url.URL{Scheme: runtimeAuthProxyHTTP, Host: listener.Addr().String(), Path: upstream.Path}).String()
	switch mode {
	case runtimeAuthProxyOpenAI:
		turn.Env = setEnv(turn.Env, workerenv.OpenAIBaseURL, localEndpoint)
		turn.Env = setEnv(turn.Env, workerenv.OpenAIAPIKey, token)
		turn.Env = setEnv(turn.Env, workerenv.CodexAPIKey, token)
	case runtimeAuthProxyAnthropic:
		turn.Env = setEnv(turn.Env, workerenv.AnthropicBaseURL, localEndpoint)
		turn.Env = setEnv(turn.Env, workerenv.AnthropicAPIKey, token)
	}
	turn.Env = runtimeAuthProxyAddNoProxyHosts(turn.Env, "127.0.0.1", "localhost")
	return turn, closeProxy, nil
}

func runtimeAuthProxyAddNoProxyHosts(env []string, hosts ...string) []string {
	values := make([]string, 0, len(hosts)+2)
	seen := make(map[string]struct{}, len(hosts)+2)
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		raw := runtimeAuthProxyEffectiveEnvValue(env, name)
		for value := range strings.SplitSeq(raw, ",") {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			key := strings.ToLower(value)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			values = append(values, value)
		}
	}
	for _, host := range hosts {
		host = strings.TrimSpace(host)
		if host == "" {
			continue
		}
		key := strings.ToLower(host)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		values = append(values, host)
	}
	combined := strings.Join(values, ",")
	env = removeTurnEnv(env, "NO_PROXY", "no_proxy")
	for _, name := range []string{"NO_PROXY", "no_proxy"} {
		env = setEnv(env, name, combined)
	}
	return env
}

func runtimeAuthProxyEffectiveEnvValue(env []string, name string) string {
	prefix := name + "="
	for i := len(env) - 1; i >= 0; i-- {
		if after, ok := strings.CutPrefix(env[i], prefix); ok {
			return after
		}
	}
	return os.Getenv(name)
}

func runtimeAuthProxyEnvFlagEnabled(env []string, name string) bool {
	switch strings.ToLower(strings.TrimSpace(envEntryValue(env, name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func newRuntimeAuthProxyToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate runtime-auth-only proxy token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func newRuntimeAuthReverseProxy(
	upstream *url.URL,
	mode runtimeAuthProxyMode,
	upstreamValue string,
	token string,
) http.Handler {
	proxy := &httputil.ReverseProxy{
		Transport: runtimeAuthProxyTransport(),
		Director: func(request *http.Request) {
			request.URL.Scheme = upstream.Scheme
			request.URL.Host = upstream.Host
			request.Host = upstream.Host
			if upstream.RawQuery != "" {
				request.URL.RawQuery = upstream.RawQuery
			}
			request.Header.Del("Authorization")
			request.Header.Del("api-key")
			request.Header.Del("x-api-key")
			switch mode {
			case runtimeAuthProxyOpenAI:
				request.Header.Set("Authorization", "Bearer "+upstreamValue)
			case runtimeAuthProxyAnthropic:
				request.Header.Set("x-api-key", upstreamValue)
			}
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "upstream request failed", http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if !runtimeAuthProxyPathAllowed(upstream.Path, mode, request.URL.Path) {
			http.NotFound(w, request)
			return
		}
		provided := runtimeAuthProxyRequestToken(request)
		if len(provided) != len(token) || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		websocketPreflight := mode == runtimeAuthProxyOpenAI && request.Method == http.MethodGet &&
			strings.EqualFold(strings.TrimSpace(request.Header.Get("Upgrade")), "websocket")
		if websocketPreflight {
			http.Error(w, "websocket transport is unavailable; use HTTP", http.StatusUpgradeRequired)
			return
		}
		if request.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if request.URL.RawQuery != "" {
			http.Error(w, "query parameters are not allowed", http.StatusBadRequest)
			return
		}
		proxy.ServeHTTP(w, request)
	})
}

func runtimeAuthProxyTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Upstream credentials must never traverse operator-configured HTTP(S)
	// proxies, where they could be observed or logged outside the pod.
	transport.Proxy = nil
	return transport
}

func runtimeAuthProxyPathAllowed(basePath string, mode runtimeAuthProxyMode, requestPath string) bool {
	requestPath = "/" + strings.TrimPrefix(requestPath, "/")
	if requestPath != path.Clean(requestPath) {
		return false
	}
	basePath = path.Clean("/" + strings.TrimPrefix(basePath, "/"))
	join := func(parts ...string) string {
		values := append([]string{basePath}, parts...)
		return path.Join(values...)
	}
	switch mode {
	case runtimeAuthProxyOpenAI:
		responses := join("responses")
		return requestPath == responses || requestPath == path.Join(responses, "compact")
	case runtimeAuthProxyAnthropic:
		apiBase := basePath
		if path.Base(apiBase) != "v1" {
			apiBase = path.Join(apiBase, "v1")
		}
		messages := path.Join(apiBase, "messages")
		return requestPath == messages || requestPath == path.Join(messages, "count_tokens")
	default:
		return false
	}
}

func runtimeAuthProxyRequestToken(request *http.Request) string {
	if request == nil {
		return ""
	}
	if value := strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")); value != "" {
		return value
	}
	if value := strings.TrimSpace(request.Header.Get("x-api-key")); value != "" {
		return value
	}
	return strings.TrimSpace(request.Header.Get("api-key"))
}
