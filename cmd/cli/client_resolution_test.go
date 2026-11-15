/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/orka-agents/orka/internal/cli/client"
)

const kubeconfigBearer = "kubeconfig-user-token"

// writeTokenKubeconfig writes a kubeconfig whose current context authenticates
// with a static bearer token, and returns its path.
func writeTokenKubeconfig(t *testing.T, contextName, namespace string) string {
	t.Helper()
	config := clientcmdapi.NewConfig()
	config.CurrentContext = contextName
	config.Contexts[contextName] = &clientcmdapi.Context{
		Cluster: "cluster-" + contextName, AuthInfo: "user", Namespace: namespace,
	}
	config.Clusters["cluster-"+contextName] = &clientcmdapi.Cluster{Server: "https://" + contextName + ".k8s.invalid"}
	config.AuthInfos["user"] = &clientcmdapi.AuthInfo{Token: kubeconfigBearer}
	return writeKubeconfig(t, t.TempDir(), config)
}

type observedRequest struct {
	authorization string
	namespace     string
	hasNamespace  bool
}

// agentListServer serves GET /api/v1/agents and records what each request
// carried.
type agentListServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []observedRequest
}

func newAgentListServer(t *testing.T) *agentListServer {
	t.Helper()
	s := &agentListServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/agents" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		namespace, hasNamespace := r.URL.Query()["namespace"]
		s.mu.Lock()
		s.requests = append(s.requests, observedRequest{
			authorization: r.Header.Get("Authorization"),
			namespace:     firstOrEmpty(namespace),
			hasNamespace:  hasNamespace,
		})
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
	}))
	t.Cleanup(s.Close)
	return s
}

func firstOrEmpty(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func (s *agentListServer) only(t *testing.T) observedRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	require.Len(t, s.requests, 1)
	return s.requests[0]
}

func (s *agentListServer) port(t *testing.T) int {
	t.Helper()
	u, err := url.Parse(s.URL)
	require.NoError(t, err)
	_, portText, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	return port
}

// stubKubeconfigTransport replaces Service discovery and port-forwarding, so
// tests never need a cluster or kubectl.
func stubKubeconfigTransport(
	t *testing.T,
	discover func(target kubeTarget) (string, string),
	forward func(kc kubeconfigContext, ns, svc string, isolateInterrupt bool) (int, int, func(), error),
) {
	t.Helper()
	origDiscover, origForward := discoverServiceFn, startPortForwardFn
	discoverServiceFn, startPortForwardFn = discover, forward
	t.Cleanup(func() { discoverServiceFn, startPortForwardFn = origDiscover, origForward })
}

func failDiscovery(t *testing.T) {
	t.Helper()
	stubKubeconfigTransport(t,
		func(kubeTarget) (string, string) {
			t.Error("unexpected Service discovery")
			return "", ""
		},
		func(kubeconfigContext, string, string, bool) (int, int, func(), error) {
			t.Error("unexpected port-forward")
			return 0, 0, nil, os.ErrInvalid
		})
}

// trustCachedForwards treats every cached forward's process as the kubectl
// port-forward that started it, standing in for a live kubectl.
func trustCachedForwards(t *testing.T) {
	t.Helper()
	stubPortForwardProcess(t, func(*portForwardCache) bool { return true })
}

func stubPortForwardProcess(t *testing.T, check func(cached *portForwardCache) bool) {
	t.Helper()
	orig := portForwardProcessFn
	portForwardProcessFn = check
	t.Cleanup(func() { portForwardProcessFn = orig })
}

func executeCLI(t *testing.T, args ...string) error {
	t.Helper()
	root := newRootCmd()
	root.SetOut(new(bytes.Buffer))
	root.SetErr(new(bytes.Buffer))
	root.SetArgs(args)
	return root.Execute()
}

// resolveWith runs the root command with args and returns the client that
// resolveClient picks, without sending any request.
func resolveWith(t *testing.T, args ...string) (*client.Client, error) {
	t.Helper()
	var resolved *client.Client
	root := newRootCmd()
	root.SetOut(new(bytes.Buffer))
	root.SetErr(new(bytes.Buffer))
	root.SetArgs(args)
	root.RunE = func(cmd *cobra.Command, _ []string) error {
		c, err := resolveClient(cmd, "")
		resolved = c
		return err
	}
	err := root.Execute()
	return resolved, err
}

// The kubeconfig user's bearer token never goes to a server the user named,
// whichever way they named it, and with nothing else configured the request
// carries no namespace, leaving the choice to the server.
func TestKubeconfigTokenIsNotSentToANamedServer(t *testing.T) {
	for _, source := range []string{"flag", envServer, "config"} {
		t.Run(source, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			failDiscovery(t)
			api := newAgentListServer(t)
			kubeconfig := writeTokenKubeconfig(t, "named", "")
			args := []string{"--kubeconfig", kubeconfig, "agent", "list"}
			switch source {
			case "flag":
				args = append(args, "--server", api.URL)
			case envServer:
				t.Setenv(envServer, api.URL)
			case "config":
				require.NoError(t, saveConfig(orkaConfig{Server: api.URL}))
			}

			require.NoError(t, executeCLI(t, args...))
			got := api.only(t)
			require.Empty(t, got.authorization, "the kubeconfig token reached a server named by %s", source)
			require.False(t, got.hasNamespace, "namespace = %q, want the server to choose", got.namespace)
		})
	}
}

// http://localhost:8080 is only a guess, not a server reached through the
// kubeconfig, so it gets no kubeconfig token either.
func TestKubeconfigTokenIsNotSentToTheDefaultServer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	discovered := false
	stubKubeconfigTransport(t,
		func(kubeTarget) (string, string) {
			discovered = true
			return "", ""
		},
		func(kubeconfigContext, string, string, bool) (int, int, func(), error) {
			t.Error("unexpected port-forward")
			return 0, 0, nil, os.ErrInvalid
		})

	c, err := resolveWith(t, "--kubeconfig", writeTokenKubeconfig(t, "unreachable", ""))
	require.NoError(t, err)
	require.True(t, discovered)
	require.Equal(t, defaultServer, c.BaseURL)
	require.Empty(t, c.Token)
	require.Empty(t, c.Namespace)
}

// Finding the Service is not reaching it: when the port-forward fails, the
// CLI falls back to the default server without the kubeconfig token.
func TestKubeconfigTokenIsNotSentWhenThePortForwardFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	forwarded := false
	stubKubeconfigTransport(t,
		func(kubeTarget) (string, string) { return "orka-system", "orka" },
		func(kubeconfigContext, string, string, bool) (int, int, func(), error) {
			forwarded = true
			return 0, 0, nil, os.ErrDeadlineExceeded
		})

	kubeconfig := writeTokenKubeconfig(t, "unforwarded", "")
	c, err := resolveWith(t, "--kubeconfig", kubeconfig)
	require.NoError(t, err)
	require.True(t, forwarded)
	require.Equal(t, defaultServer, c.BaseURL)
	require.Empty(t, c.Token)
	require.Empty(t, c.Namespace)
	require.Nil(t, loadPortForwardCache(kubeconfigIdentity(kubeconfig)))
}

// When the CLI reaches Orka by discovering its Service and port-forwarding
// through the kubeconfig, it authenticates as the kubeconfig user. Discovery
// looks in the selected namespace first, then in the context's. Requests name
// only the selected namespace: without one, the server uses the one it
// watches, whatever the context or the Service's namespace is. Each namespace
// selection keeps its own cached forward.
func TestDiscoveredServiceUsesKubeconfigToken(t *testing.T) {
	for _, tc := range []struct {
		name             string
		contextNamespace string
		args             []string
		env, config      string
		selected         string
		wantSearch       []string
	}{
		{name: "no namespace", wantSearch: []string{"orka-system", "orka", "default"}},
		{name: "context namespace", contextNamespace: "team-a", wantSearch: []string{"team-a", "orka-system", "orka", "default"}},
		{
			name: "--namespace", contextNamespace: "team-a", args: []string{"--namespace", "team-b"}, selected: "team-b",
			wantSearch: []string{"team-b", "team-a", "orka-system", "orka", "default"},
		},
		{name: envNamespace, env: "team-b", selected: "team-b", wantSearch: []string{"team-b", "orka-system", "orka", "default"}},
		{name: "config namespace", config: "team-b", selected: "team-b", wantSearch: []string{"team-b", "orka-system", "orka", "default"}},
		{
			name: "selected namespace is the context's", contextNamespace: "orka-system", args: []string{"-n", "orka-system"},
			selected: "orka-system", wantSearch: []string{"orka-system", "orka", "default"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			if tc.env != "" {
				t.Setenv(envNamespace, tc.env)
			}
			if tc.config != "" {
				require.NoError(t, saveConfig(orkaConfig{Namespace: tc.config}))
			}
			api := newAgentListServer(t)
			kubeconfig := writeTokenKubeconfig(t, "discovered", tc.contextNamespace)
			var searched []string
			var forwardedThrough kubeconfigContext
			stubKubeconfigTransport(t,
				func(target kubeTarget) (string, string) {
					require.Equal(t, kubeconfig, target.path)
					require.Equal(t, "discovered", target.name)
					searched = target.discoveryNamespaces()
					return "orka-system", "orka"
				},
				func(kc kubeconfigContext, ns, svc string, _ bool) (int, int, func(), error) {
					forwardedThrough = kc
					require.Equal(t, "orka-system", ns)
					require.Equal(t, "orka", svc)
					// This process stands in for kubectl, so it has a start time.
					return api.port(t), os.Getpid(), func() {}, nil
				})

			require.NoError(t, executeCLI(t, append([]string{"--kubeconfig", kubeconfig, "agent", "list"}, tc.args...)...))
			got := api.only(t)
			require.Equal(t, "Bearer "+kubeconfigBearer, got.authorization)
			require.Equal(t, tc.selected != "", got.hasNamespace)
			require.Equal(t, tc.selected, got.namespace)
			require.Equal(t, tc.wantSearch, searched)
			require.Equal(t, currentKubeconfigContext(kubeconfig), forwardedThrough)

			identity := kubeconfigIdentity(kubeconfig)
			require.NotEmpty(t, identity)
			key := kubeTarget{kubeconfigContext: forwardedThrough, selectedNamespace: tc.selected}.cacheKey()
			cached := loadPortForwardCache(key)
			require.NotNil(t, cached)
			require.Equal(t, key, cached.KubeContext)
			require.Equal(t, os.Getpid(), cached.PID)
			require.Equal(t, processStartMark(os.Getpid()), cached.ProcessStart)
			if runtime.GOOS == "linux" || runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
				require.NotEmpty(t, cached.ProcessStart, "the forward's start time was not recorded")
			}
			require.Equal(t, []string{
				"kubectl", "--kubeconfig", kubeconfig, "--context", "discovered",
				"port-forward", "-n", "orka-system", "svc/orka", fmt.Sprintf("%d:8080", api.port(t)),
			}, cached.Command)
			if tc.selected != "" {
				require.NotEqual(t, identity, key)
				require.Nil(t, loadPortForwardCache(identity), "a forward found for one namespace was cached for none")
			}
		})
	}
}

// A token the user gave still wins over the kubeconfig user's token on a
// discovered Service.
func TestDiscoveredServicePrefersConfiguredToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(envToken, "env-token")
	api := newAgentListServer(t)
	stubKubeconfigTransport(t,
		func(kubeTarget) (string, string) { return "orka-system", "orka" },
		func(kubeconfigContext, string, string, bool) (int, int, func(), error) {
			return api.port(t), 4242, func() {}, nil
		})

	require.NoError(t, executeCLI(t, "--kubeconfig", writeTokenKubeconfig(t, "discovered", ""), "agent", "list"))
	require.Equal(t, "Bearer env-token", api.only(t).authorization)
}

// A cached port-forward counts as reached through the kubeconfig only for the
// context that started it, and only while the kubectl process that started it
// still holds the port. Anything else is rediscovered.
func TestCachedPortForwardIsReusedOnlyForTheSameKubeContext(t *testing.T) {
	t.Run("same context", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		failDiscovery(t)
		api := newAgentListServer(t)
		kubeconfig := writeTokenKubeconfig(t, "cached", "")
		var checked []string
		stubPortForwardProcess(t, func(cached *portForwardCache) bool {
			checked = append(checked, fmt.Sprintf("%d %d %s/%s", cached.PID, cached.Port, cached.Namespace, cached.Service))
			return true
		})
		savePortForwardCache(&portForwardCache{
			Port: api.port(t), PID: 4242, Service: "orka", Namespace: "orka-system",
			KubeContext: kubeconfigIdentity(kubeconfig),
		})

		require.NoError(t, executeCLI(t, "--kubeconfig", kubeconfig, "agent", "list"))
		got := api.only(t)
		require.Equal(t, "Bearer "+kubeconfigBearer, got.authorization)
		require.False(t, got.hasNamespace, "namespace = %q, want the server to choose", got.namespace)
		require.Equal(t, []string{fmt.Sprintf("4242 %d orka-system/orka", api.port(t))}, checked)
	})

	currentKubeconfig := func(t *testing.T) string { return writeTokenKubeconfig(t, "current", "") }
	for _, tc := range []struct {
		name       string
		kubeconfig func(t *testing.T) string
		// cache writes the stale forward for the current kubeconfig.
		cache   func(t *testing.T, kubeconfig string, port int)
		process bool
		// namespace is passed as --namespace when set.
		namespace string
	}{
		{
			name: "forward process gone", kubeconfig: currentKubeconfig, process: false,
			cache: func(t *testing.T, kubeconfig string, port int) {
				savePortForwardCache(&portForwardCache{
					Port: port, PID: 4242, Service: "orka", Namespace: "orka-system",
					KubeContext: kubeconfigIdentity(kubeconfig),
				})
			},
		},
		{
			// Discovery searched another namespace first for that forward, so
			// it can reach another installation.
			name: "other context namespace", kubeconfig: currentKubeconfig, process: true,
			cache: func(t *testing.T, kubeconfig string, port int) {
				config, err := clientcmd.LoadFromFile(kubeconfig)
				require.NoError(t, err)
				original := *config.Contexts["current"]
				config.Contexts["current"].Namespace = "team-a"
				require.NoError(t, clientcmd.WriteToFile(*config, kubeconfig))
				savePortForwardCache(&portForwardCache{
					Port: port, PID: 4242, Service: "orka", Namespace: "team-a",
					KubeContext: kubeconfigIdentity(kubeconfig),
				})
				config.Contexts["current"] = &original
				require.NoError(t, clientcmd.WriteToFile(*config, kubeconfig))
			},
		},
		{
			name: "no namespace selected for the forward", kubeconfig: currentKubeconfig, process: true, namespace: "team-b",
			cache: func(t *testing.T, kubeconfig string, port int) {
				savePortForwardCache(&portForwardCache{
					Port: port, PID: 4242, Service: "orka", Namespace: "orka-system",
					KubeContext: kubeconfigIdentity(kubeconfig),
				})
			},
		},
		{
			name: "other namespace selected for the forward", kubeconfig: currentKubeconfig, process: true, namespace: "team-b",
			cache: func(t *testing.T, kubeconfig string, port int) {
				savePortForwardCache(&portForwardCache{
					Port: port, PID: 4242, Service: "orka", Namespace: "team-a",
					KubeContext: kubeTarget{
						kubeconfigContext: currentKubeconfigContext(kubeconfig), selectedNamespace: "team-a",
					}.cacheKey(),
				})
			},
		},
		{
			name: "other context", kubeconfig: currentKubeconfig, process: true,
			cache: func(t *testing.T, _ string, port int) {
				savePortForwardCache(&portForwardCache{
					Port: port, PID: 4242, Service: "orka", Namespace: "orka-system",
					KubeContext: kubeconfigIdentity(writeTokenKubeconfig(t, "other", "")),
				})
			},
		},
		{
			name: "legacy cache", kubeconfig: currentKubeconfig, process: true,
			cache: func(t *testing.T, _ string, port int) {
				data, err := json.Marshal(portForwardCache{
					Port: port, PID: 4242, Service: "orka", Namespace: "orka-system", Timestamp: time.Now().Unix(),
				})
				require.NoError(t, err)
				dir := filepath.Join(os.Getenv("HOME"), configDir)
				require.NoError(t, os.MkdirAll(dir, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "portforward.json"), data, 0o600))
			},
		},
		{
			name:       "no current context",
			kubeconfig: func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing") },
			process:    true,
			cache: func(t *testing.T, kubeconfig string, port int) {
				savePortForwardCache(&portForwardCache{
					Port: port, PID: 4242, Service: "orka", Namespace: "orka-system",
					KubeContext: kubeconfigIdentity(kubeconfig),
				})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			stale := newAgentListServer(t)
			kubeconfig := tc.kubeconfig(t)
			tc.cache(t, kubeconfig, stale.port(t))
			stubPortForwardProcess(t, func(*portForwardCache) bool { return tc.process })
			discovered := false
			stubKubeconfigTransport(t,
				func(kubeTarget) (string, string) {
					discovered = true
					return "", ""
				},
				func(kubeconfigContext, string, string, bool) (int, int, func(), error) {
					t.Error("unexpected port-forward")
					return 0, 0, nil, os.ErrInvalid
				})

			args := []string{"--kubeconfig", kubeconfig}
			if tc.namespace != "" {
				args = append(args, "--namespace", tc.namespace)
			}
			c, err := resolveWith(t, args...)
			require.NoError(t, err)
			require.True(t, discovered, "the CLI reused a forward it cannot trust")
			require.Equal(t, defaultServer, c.BaseURL)
			require.Empty(t, c.Token)
			require.Equal(t, tc.namespace, c.Namespace)
		})
	}
}

// The CLI reads the current context once. Switching contexts while it
// discovers the Service does not move the port-forward, its cache entry or the
// bearer token to the new context.
func TestContextSwitchWhileConnectingKeepsTheFirstContext(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	config := clientcmdapi.NewConfig()
	for _, name := range []string{"a", "b"} {
		config.Contexts[name] = &clientcmdapi.Context{Cluster: "cluster-" + name, AuthInfo: "user-" + name}
		config.Clusters["cluster-"+name] = &clientcmdapi.Cluster{Server: "https://" + name + ".k8s.invalid"}
		config.AuthInfos["user-"+name] = &clientcmdapi.AuthInfo{Token: "token-" + name}
	}
	config.CurrentContext = "a"
	kubeconfig := writeKubeconfig(t, dir, config)
	contextA := currentKubeconfigContext(kubeconfig)
	require.NotEmpty(t, contextA.identity)

	api := newAgentListServer(t)
	stubKubeconfigTransport(t,
		func(target kubeTarget) (string, string) {
			require.Equal(t, "a", target.name)
			// Switch contexts and change context a's credential: the CLI
			// keeps the kubeconfig it read.
			config.CurrentContext = "b"
			config.AuthInfos["user-a"].Token = "token-a-changed"
			writeKubeconfig(t, dir, config)
			return "orka-system", "orka"
		},
		func(kc kubeconfigContext, _, _ string, _ bool) (int, int, func(), error) {
			require.Equal(t, contextA, kc)
			return api.port(t), 4242, func() {}, nil
		})

	require.NoError(t, executeCLI(t, "--kubeconfig", kubeconfig, "agent", "list"))
	require.Equal(t, "Bearer token-a", api.only(t).authorization)
	cached := loadPortForwardCache(contextA.identity)
	require.NotNil(t, cached, "the forward through context a was not cached for it")
	require.Contains(t, strings.Join(cached.Command, " "), "--context a ")

	contextB := currentKubeconfigContext(kubeconfig)
	require.Equal(t, "b", contextB.name)
	require.NotEqual(t, contextA.identity, contextB.identity)
	require.Nil(t, loadPortForwardCache(contextB.identity), "the forward through context a was cached for context b")
}

// kubectl reads the kubeconfig again when it starts. If the context the CLI
// read no longer reaches the same cluster, or cannot be read, the CLI stops
// that forward and sends the context's token nowhere. A change that cannot
// move kubectl to another cluster keeps the forward.
func TestContextRedefinedWhileConnectingDropsTheForward(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*clientcmdapi.Config)
		drop   bool
	}{
		{name: "another server", drop: true, change: func(c *clientcmdapi.Config) {
			c.Clusters["cluster-a"].Server = "https://other.k8s.invalid"
		}},
		{name: "another CA", drop: true, change: func(c *clientcmdapi.Config) {
			c.Clusters["cluster-a"].CertificateAuthorityData = []byte("other-ca")
		}},
		{name: "another CA file", drop: true, change: func(c *clientcmdapi.Config) {
			c.Clusters["cluster-a"].CertificateAuthority = "/tmp/other-ca.crt"
		}},
		{name: "another proxy", drop: true, change: func(c *clientcmdapi.Config) {
			c.Clusters["cluster-a"].ProxyURL = "socks5://127.0.0.1:1080"
		}},
		{name: "another TLS server name", drop: true, change: func(c *clientcmdapi.Config) {
			c.Clusters["cluster-a"].TLSServerName = "api.internal"
		}},
		{name: "TLS verification off", drop: true, change: func(c *clientcmdapi.Config) {
			c.Clusters["cluster-a"].InsecureSkipTLSVerify = true
		}},
		{name: "context removed", drop: true, change: func(c *clientcmdapi.Config) { delete(c.Contexts, "a") }},
		{name: "another namespace", change: func(c *clientcmdapi.Config) { c.Contexts["a"].Namespace = "team-b" }},
		{name: "another token", change: func(c *clientcmdapi.Config) { c.AuthInfos["user-a"].Token = "token-a-changed" }},
		{name: "another user", change: func(c *clientcmdapi.Config) {
			c.AuthInfos["user-b"] = &clientcmdapi.AuthInfo{Token: "token-b"}
			c.Contexts["a"].AuthInfo = "user-b"
		}},
		{name: "cluster entry renamed", change: func(c *clientcmdapi.Config) {
			c.Clusters["renamed"] = c.Clusters["cluster-a"]
			delete(c.Clusters, "cluster-a")
			c.Contexts["a"].Cluster = "renamed"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			dir := t.TempDir()
			config := clientcmdapi.NewConfig()
			config.CurrentContext = "a"
			config.Contexts["a"] = &clientcmdapi.Context{Cluster: "cluster-a", AuthInfo: "user-a", Namespace: "team-a"}
			config.Clusters["cluster-a"] = &clientcmdapi.Cluster{Server: "https://a.k8s.invalid"}
			config.AuthInfos["user-a"] = &clientcmdapi.AuthInfo{Token: "token-a"}
			kubeconfig := writeKubeconfig(t, dir, config)
			contextA := currentKubeconfigContext(kubeconfig)

			stopped := false
			stubKubeconfigTransport(t,
				func(kubeTarget) (string, string) {
					tc.change(config)
					writeKubeconfig(t, dir, config)
					return "orka-system", "orka"
				},
				func(kubeconfigContext, string, string, bool) (int, int, func(), error) {
					return 4321, os.Getpid(), func() { stopped = true }, nil
				})

			c, err := resolveWith(t, "--kubeconfig", kubeconfig)
			require.NoError(t, err)
			if tc.drop {
				require.True(t, stopped, "the forward was not stopped")
				require.Equal(t, defaultServer, c.BaseURL)
				require.Empty(t, c.Token)
				require.Nil(t, loadPortForwardCache(contextA.identity), "the forward was cached")
				return
			}
			require.False(t, stopped, "the forward was stopped")
			require.Equal(t, portForwardURL(4321), c.BaseURL)
			require.Equal(t, "token-a", c.Token)
			require.NotNil(t, loadPortForwardCache(contextA.identity), "the forward was not cached")
		})
	}
}

// kubectl names the kubeconfig the user gave and the context the CLI read, so
// it forwards through that context whatever the current one is when it starts.
func TestPortForwardCommand(t *testing.T) {
	forward := []string{"port-forward", "-n", "orka-system", "svc/orka", "4321:8080"}
	for _, tc := range []struct {
		name string
		kc   kubeconfigContext
		want []string
	}{
		{
			name: "kubeconfig and context", kc: kubeconfigContext{path: "/tmp/kc", name: "admin@prod"},
			want: append([]string{"kubectl", "--kubeconfig", "/tmp/kc", "--context", "admin@prod"}, forward...),
		},
		{
			name: "default kubeconfig", kc: kubeconfigContext{name: "admin@prod"},
			want: append([]string{"kubectl", "--context", "admin@prod"}, forward...),
		},
		{name: "no context", kc: kubeconfigContext{}, want: append([]string{"kubectl"}, forward...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, portForwardCommand(tc.kc, "orka-system", "orka", 4321))
		})
	}
}

// Only the kubectl command line the CLI started counts as its forward.
func TestIsPortForwardCommand(t *testing.T) {
	want := portForwardCommand(kubeconfigContext{path: "/tmp/kc", name: "prod"}, "orka-system", "orka", 4321)
	with := func(index int, value string) []string {
		args := slices.Clone(want)
		args[index] = value
		return args
	}
	last := len(want) - 1
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{name: "forward", args: want, want: true},
		{name: "kubectl by its full path", args: with(0, "/usr/local/bin/kubectl"), want: true},
		{name: "other program", args: with(0, "python3")},
		{name: "kubectl arguments to a shell", args: append([]string{"sh", "-c", "sleep 30; :"}, want...)},
		{name: "other kubeconfig", args: with(2, "/tmp/other")},
		{name: "other context", args: with(4, "staging")},
		{name: "current context", args: append([]string{"kubectl", "--kubeconfig", "/tmp/kc"}, want[5:]...)},
		{name: "other namespace", args: with(last-2, "team-a")},
		{name: "other service", args: with(last-1, "svc/other")},
		{name: "other port", args: with(last, "4322:8080")},
		{name: "extra argument", args: append(slices.Clone(want), "--address=0.0.0.0")},
		{name: "missing argument", args: want[:last]},
		{name: "empty", args: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isPortForwardCommand(tc.args, want))
		})
	}
	require.False(t, isPortForwardCommand(want, nil), "a cache entry without a command line matched")
}

// isPortForwardProcess reads the real command line and start time of a live
// process. Only the process the cache entry recorded counts: the same command
// line, started at the recorded time. Once it exits, nothing is reused.
func TestIsPortForwardProcess(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("reading process command lines is not supported on %s", runtime.GOOS)
	}
	command := portForwardCommand(kubeconfigContext{path: "/tmp/kc", name: "prod"}, "orka-system", "orka", 4321)
	cmd := startFakePortForward(t, command)
	pid := cmd.Process.Pid
	mark := processStartMark(pid)
	require.NotEmpty(t, mark)
	forward := func(pid int, command []string, mark string) *portForwardCache {
		return &portForwardCache{
			PID: pid, Port: 4321, Namespace: "orka-system", Service: "orka", Command: command, ProcessStart: mark,
		}
	}

	// Linux can show an empty command line for a moment after the process
	// starts, so wait for the real one before checking what does not match.
	require.Eventually(t, func() bool {
		return isPortForwardProcess(forward(pid, command, mark))
	}, 5*time.Second, 20*time.Millisecond)
	require.False(t, isPortForwardProcess(forward(pid, command, mark+"0")), "another start time matched")
	require.False(t, isPortForwardProcess(forward(pid, command, "")), "a missing start time matched")
	otherContext := slices.Clone(command)
	otherContext[4] = "staging"
	require.False(t, isPortForwardProcess(forward(pid, otherContext, mark)))
	require.False(t, isPortForwardProcess(forward(os.Getpid(), command, processStartMark(os.Getpid()))))
	require.False(t, isPortForwardProcess(forward(0, command, mark)))

	require.NoError(t, cmd.Process.Kill())
	_ = cmd.Wait()
	require.False(t, isPortForwardProcess(forward(pid, command, mark)))
}

// fakeKubectlEnv makes this test binary sleep instead of running tests; see
// startFakePortForward.
const fakeKubectlEnv = "ORKA_CLI_TEST_FAKE_KUBECTL"

// startFakePortForward starts a process whose command line is command, a
// kubectl port-forward's, and stops it when the test ends. The process is this
// test binary, which only sleeps when fakeKubectlEnv is set.
func startFakePortForward(t *testing.T, command []string) *exec.Cmd {
	t.Helper()
	self, err := os.Executable()
	require.NoError(t, err)
	cmd := &exec.Cmd{Path: self, Args: command, Env: append(os.Environ(), fakeKubectlEnv+"=1")}
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

// A forward run by another user is not trusted, even with the same arguments.
func TestIsOwnPortForward(t *testing.T) {
	command := portForwardCommand(kubeconfigContext{name: "prod"}, "orka-system", "orka", 4321)
	cached := &portForwardCache{PID: 4242, Port: 4321, Namespace: "orka-system", Service: "orka", Command: command}
	require.True(t, isOwnPortForward(os.Getuid(), command, cached))
	require.False(t, isOwnPortForward(os.Getuid()+1, command, cached))
	require.False(t, isOwnPortForward(os.Getuid(), command[:len(command)-1], cached))
}

func TestPortForwardURLMatchesTheCheckedAddress(t *testing.T) {
	require.Equal(t, "http://127.0.0.1:4321", portForwardURL(4321))
}

// The identity changes with anything that changes which cluster a port-forward
// reaches or where discovery looks first, the context's namespace, and not with
// the user's credential.
func TestKubeconfigIdentityDistinguishesContexts(t *testing.T) {
	base := func() *clientcmdapi.Config {
		config := clientcmdapi.NewConfig()
		config.CurrentContext = "admin@kubernetes"
		config.Contexts["admin@kubernetes"] = &clientcmdapi.Context{Cluster: "kubernetes", AuthInfo: "admin"}
		config.Clusters["kubernetes"] = &clientcmdapi.Cluster{
			Server: "https://10.0.0.1:6443", CertificateAuthorityData: []byte("ca-a"),
		}
		config.AuthInfos["admin"] = &clientcmdapi.AuthInfo{Token: "token-a"}
		return config
	}
	dir := t.TempDir()
	reference := kubeconfigIdentity(writeKubeconfig(t, dir, base()))
	require.NotEmpty(t, reference)

	// Each case changes one setting, so each part of the identity is checked
	// on its own.
	for _, tc := range []struct {
		name   string
		change func(*clientcmdapi.Config)
		same   bool
	}{
		{name: "context namespace", change: func(c *clientcmdapi.Config) {
			c.Contexts["admin@kubernetes"].Namespace = "team-a"
		}},
		{name: "user token", same: true, change: func(c *clientcmdapi.Config) { c.AuthInfos["admin"].Token = "token-b" }},
		{name: "context name", change: func(c *clientcmdapi.Config) {
			c.Contexts["other"] = c.Contexts["admin@kubernetes"]
			delete(c.Contexts, "admin@kubernetes")
			c.CurrentContext = "other"
		}},
		{name: "user name", change: func(c *clientcmdapi.Config) {
			c.AuthInfos["other"] = c.AuthInfos["admin"]
			c.Contexts["admin@kubernetes"].AuthInfo = "other"
		}},
		{name: "server", change: func(c *clientcmdapi.Config) { c.Clusters["kubernetes"].Server = "https://10.0.0.2:6443" }},
		{name: "proxy", change: func(c *clientcmdapi.Config) { c.Clusters["kubernetes"].ProxyURL = "socks5://127.0.0.1:1080" }},
		{name: "TLS server name", change: func(c *clientcmdapi.Config) { c.Clusters["kubernetes"].TLSServerName = "api.internal" }},
		{name: "TLS verification", change: func(c *clientcmdapi.Config) {
			c.Clusters["kubernetes"].InsecureSkipTLSVerify = true
		}},
		{name: "CA data", change: func(c *clientcmdapi.Config) {
			c.Clusters["kubernetes"].CertificateAuthorityData = []byte("ca-b")
		}},
		{name: "CA file", change: func(c *clientcmdapi.Config) {
			c.Clusters["kubernetes"].CertificateAuthority = filepath.Join(dir, "ca.crt")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := base()
			tc.change(config)
			got := kubeconfigIdentity(writeKubeconfig(t, dir, config))
			require.NotEmpty(t, got)
			if tc.same {
				require.Equal(t, reference, got)
			} else {
				require.NotEqual(t, reference, got)
			}
		})
	}

	// The same names and settings in another file can describe another
	// cluster behind the same private address.
	otherFile := kubeconfigIdentity(writeKubeconfig(t, t.TempDir(), base()))
	require.NotEmpty(t, otherFile)
	require.NotEqual(t, reference, otherFile)
	require.Empty(t, kubeconfigIdentity(filepath.Join(t.TempDir(), "missing")))

	// One file has one identity however its path is written, and two files
	// given by the same relative name do not share one.
	t.Run("path spelling", func(t *testing.T) {
		path := writeKubeconfig(t, dir, base())
		require.Equal(t, reference, kubeconfigIdentity(path))
		if runtime.GOOS != "windows" {
			link := filepath.Join(t.TempDir(), "link")
			require.NoError(t, os.Symlink(path, link))
			require.Equal(t, reference, kubeconfigIdentity(link))
		}
		t.Chdir(dir)
		require.Equal(t, reference, kubeconfigIdentity("kubeconfig"))
		other := t.TempDir()
		writeKubeconfig(t, other, base())
		t.Chdir(other)
		sameName := kubeconfigIdentity("kubeconfig")
		require.NotEmpty(t, sameName)
		require.NotEqual(t, reference, sameName)
	})
}

func TestTokenSourcePrecedence(t *testing.T) {
	writeToken := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "token")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}
	for _, tc := range []struct {
		name      string
		flag      string
		file      string
		env       string
		config    string
		wantToken string
	}{
		{name: "flag wins", flag: "flag-token", env: "env-token", config: "config-token", wantToken: "flag-token"},
		{name: "file beats env", file: "file-token\n", env: "env-token", config: "config-token", wantToken: "file-token"},
		{name: "env beats config", env: "  env-token\n", config: "config-token", wantToken: "env-token"},
		{name: "blank env is unset", env: " \n", config: "config-token", wantToken: "config-token"},
		{name: "config", config: "config-token", wantToken: "config-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			require.NoError(t, saveConfig(orkaConfig{Token: tc.config}))
			t.Setenv(envToken, tc.env)
			args := []string{"--server", "http://orka.invalid"}
			if tc.flag != "" {
				args = append(args, "--token", tc.flag)
			}
			if tc.file != "" {
				args = append(args, "--token-file", writeToken(t, tc.file))
			}
			c, err := resolveWith(t, args...)
			require.NoError(t, err)
			require.Equal(t, tc.wantToken, c.Token)
		})
	}
}

func TestTokenFileFromStdin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	pipeStdin(t, " stdin-token\n")

	c, err := resolveWith(t, "--server", "http://orka.invalid", "--token-file", "-")
	require.NoError(t, err)
	require.Equal(t, "stdin-token", c.Token)

	// Resolving again, as orka run does when it retries, finds the same token
	// rather than drained stdin.
	again, err := resolveWith(t, "--server", "http://orka.invalid", "--token-file", "-")
	require.NoError(t, err)
	require.Equal(t, "stdin-token", again.Token)

	// A new stdin is read afresh.
	pipeStdin(t, "next-token\n")
	next, err := resolveWith(t, "--server", "http://orka.invalid", "--token-file", "-")
	require.NoError(t, err)
	require.Equal(t, "next-token", next.Token)
}

// A token file that is a stream, such as a shell's process substitution
// (--token-file <(...)), can be read only once; resolving the client again
// still finds the token.
func TestTokenFileStreamIsReadOnce(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("/dev/fd is not available on %s", runtime.GOOS)
	}
	t.Setenv("HOME", t.TempDir())
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = reader.Close() })
	_, err = writer.WriteString("stream-token\n")
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	stream := fmt.Sprintf("/dev/fd/%d", reader.Fd())

	for range 2 {
		c, err := resolveWith(t, "--server", "http://orka.invalid", "--token-file", stream)
		require.NoError(t, err)
		require.Equal(t, "stream-token", c.Token)
	}
}

// pipeStdin replaces os.Stdin with a pipe holding content.
func pipeStdin(t *testing.T, content string) {
	t.Helper()
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	_, err = writer.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	origStdin := os.Stdin
	os.Stdin = reader
	t.Cleanup(func() {
		os.Stdin = origStdin
		_ = reader.Close()
	})
}

// Stdin feeds one reader, so a credential cannot come from it when the
// command's manifest, prompt, remote command or copied file does.
func TestCredentialFileAndOtherStdinReaders(t *testing.T) {
	for _, flag := range []string{"--token-file", "--txn-token-file"} {
		for _, stdin := range []string{"-", "/dev/stdin"} {
			t.Run(flag+" "+stdin+" with --file", func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				pipeStdin(t, "kind: Agent\n")
				root := newRootCmd()
				root.SetOut(new(bytes.Buffer))
				root.SetErr(new(bytes.Buffer))
				root.Flags().StringP("file", "f", "", "manifest")
				root.SetArgs([]string{"--server", "http://orka.invalid", flag, stdin, "--file", "-"})
				root.RunE = func(cmd *cobra.Command, _ []string) error {
					_, err := resolveClient(cmd, "")
					return err
				}
				require.ErrorContains(t, root.Execute(), flag+" and --file cannot both read stdin")
			})
			for name, tc := range map[string]struct {
				args []string
				want string
			}{
				"a piped prompt":      {args: []string{"run"}, want: "orka run uses for the prompt"},
				"an empty prompt":     {args: []string{"run", ""}, want: "orka run uses for the prompt"},
				"orka task exec -i":   {args: []string{"task", "exec", "my-task", "-i", "--", "cat"}, want: "orka task exec -i or -t passes to the command"},
				"orka task exec -t":   {args: []string{"task", "exec", "my-task", "-t", "--", "cat"}, want: "orka task exec -i or -t passes to the command"},
				"orka task cp from -": {args: []string{"task", "cp", "-", "my-task:notes.txt"}, want: "orka task cp uses for the file to copy in"},
				"orka task cp from /dev/stdin": {
					args: []string{"task", "cp", "/dev/stdin", "my-task:notes.txt"},
					want: "orka task cp uses for the file to copy in",
				},
			} {
				t.Run(flag+" "+stdin+" with "+name, func(t *testing.T) {
					t.Setenv("HOME", t.TempDir())
					pipeStdin(t, "fix bugs\n")
					err := executeCLI(t, append([]string{"--server", "http://orka.invalid", flag, stdin}, tc.args...)...)
					require.ErrorContains(t, err, fmt.Sprintf("%s %s reads stdin, which %s", flag, stdin, tc.want))
				})
			}
		}
	}
}

// Commands that leave stdin alone still take a credential from it.
func TestCredentialFromStdinWithCommandsThatLeaveStdinFree(t *testing.T) {
	localFile := filepath.Join(t.TempDir(), "notes.txt")
	require.NoError(t, os.WriteFile(localFile, []byte("notes\n"), 0o600))
	for _, tc := range []struct {
		flag, header, want string
	}{
		{flag: "--token-file", header: "Authorization", want: "Bearer stdin-credential"},
		{flag: "--txn-token-file", header: "Txn-Token", want: "stdin-credential"},
	} {
		for name, args := range map[string][]string{
			"a prompt argument":               {"run", "fix bugs"},
			"orka task exec without -i or -t": {"task", "exec", "my-task", "--quiet", "--", "true"},
			"orka task cp from the task to -": {"task", "cp", "my-task:notes.txt", "-"},
			"orka task cp from a local file":  {"task", "cp", "--quiet", localFile, "my-task:notes.txt"},
		} {
			t.Run(tc.flag+" - with "+name, func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				pipeStdin(t, "stdin-credential\n")
				var mu sync.Mutex
				var seen []string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					seen = append(seen, r.Header.Get(tc.header))
					mu.Unlock()
					if r.URL.Path == "/api/v1/chat/config" {
						// Disabled chat ends orka run after one request.
						_ = json.NewEncoder(w).Encode(map[string]any{"enabled": false})
						return
					}
					http.NotFound(w, r)
				}))
				t.Cleanup(server.Close)

				err := executeCLI(t, append([]string{"--server", server.URL, tc.flag, "-"}, args...)...)
				if err != nil {
					require.NotContains(t, err.Error(), "reads stdin")
				}
				mu.Lock()
				defer mu.Unlock()
				require.NotEmpty(t, seen, "no request reached the server")
				for _, got := range seen {
					require.Equal(t, tc.want, got)
				}
			})
		}
	}
}

func TestTokenFileErrors(t *testing.T) {
	emptyFile := filepath.Join(t.TempDir(), "empty")
	require.NoError(t, os.WriteFile(emptyFile, []byte(" \n"), 0o600))
	for _, tc := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "both token flags", args: []string{"--token", "a", "--token-file", emptyFile}, wantErr: "either --token or --token-file"},
		{name: "empty file", args: []string{"--token-file", emptyFile}, wantErr: "--token-file is empty"},
		{name: "missing file", args: []string{"--token-file", filepath.Join(t.TempDir(), "missing")}, wantErr: "reading --token-file"},
		{name: "two stdin readers", args: []string{"--token-file", "-", "--txn-token-file", "-"}, wantErr: "cannot both read stdin"},
		{name: "two stdin readers by name", args: []string{"--token-file", "/dev/stdin", "--txn-token-file", "-"}, wantErr: "cannot both read stdin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv(envToken, "env-token")
			_, err := resolveWith(t, append([]string{"--server", "http://orka.invalid"}, tc.args...)...)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// The kubeconfig context's namespace never names the request's namespace: the
// context belongs to a cluster, and the Orka server it reaches watches one
// namespace of its own.
func TestNamespacePrecedence(t *testing.T) {
	for _, tc := range []struct {
		name, flag, env, config, context, want string
	}{
		{name: "flag wins", flag: "flag-ns", env: "env-ns", config: "config-ns", context: "context-ns", want: "flag-ns"},
		{name: "env beats config", env: "env-ns", config: "config-ns", context: "context-ns", want: "env-ns"},
		{name: "blank env is unset", env: " ", config: "config-ns", want: "config-ns"},
		{name: "config", config: "config-ns", context: "context-ns", want: "config-ns"},
		{name: "context is not used", context: "context-ns", want: ""},
		{name: "none", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			require.NoError(t, saveConfig(orkaConfig{Namespace: tc.config}))
			t.Setenv(envNamespace, tc.env)
			args := []string{"--server", "http://orka.invalid", "--kubeconfig", writeTokenKubeconfig(t, "ns", tc.context)}
			if tc.flag != "" {
				args = append(args, "--namespace", tc.flag)
			}
			c, err := resolveWith(t, args...)
			require.NoError(t, err)
			require.Equal(t, tc.want, c.Namespace)
		})
	}
}

// With no namespace configured, discovery searches the well-known namespaces
// and never asks the Kubernetes API for an empty one.
func TestDiscoverServiceWithoutNamespace(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	kube := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/api/v1/namespaces/orka-system/services/orka-api" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer kube.Close()
	config := clientcmdapi.NewConfig()
	config.CurrentContext = testCtxName
	config.Contexts[testCtxName] = &clientcmdapi.Context{Cluster: "c", AuthInfo: "u"}
	config.Clusters["c"] = &clientcmdapi.Cluster{Server: kube.URL, InsecureSkipTLSVerify: true}
	config.AuthInfos["u"] = &clientcmdapi.AuthInfo{Token: kubeconfigBearer}

	ns, name := discoverService(kubeTarget{kubeconfigContext: currentKubeconfigContext(writeKubeconfig(t, t.TempDir(), config))})
	require.Equal(t, "orka-system", ns)
	require.Equal(t, "orka-api", name)
	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, paths)
	for _, path := range paths {
		require.NotContains(t, path, "/namespaces//", "discovery queried an empty namespace")
	}
}

// Discovery asks the cluster of the context the CLI read, even when the
// kubeconfig's current context has changed since.
func TestDiscoverServiceUsesTheContextItRead(t *testing.T) {
	var asked sync.Map
	cluster := func(name string) *httptest.Server {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			asked.Store(name, true)
			if r.URL.Path == "/api/v1/namespaces/team-"+name+"/services/orka-api" {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	config := clientcmdapi.NewConfig()
	servers := map[string]string{}
	for _, name := range []string{"a", "b"} {
		servers[name] = cluster(name).URL
		config.Contexts[name] = &clientcmdapi.Context{Cluster: name, AuthInfo: "u", Namespace: "team-" + name}
		config.Clusters[name] = &clientcmdapi.Cluster{Server: servers[name], InsecureSkipTLSVerify: true}
	}
	config.AuthInfos["u"] = &clientcmdapi.AuthInfo{Token: kubeconfigBearer}
	dir := t.TempDir()
	config.CurrentContext = "a"
	contextA := currentKubeconfigContext(writeKubeconfig(t, dir, config))
	// Switch contexts and point context a's cluster at b's server: the CLI
	// keeps the kubeconfig it read.
	config.CurrentContext = "b"
	config.Clusters["a"].Server = servers["b"]
	writeKubeconfig(t, dir, config)

	ns, name := discoverService(kubeTarget{kubeconfigContext: contextA})
	require.Equal(t, "team-a", ns)
	require.Equal(t, "orka-api", name)
	_, askedB := asked.Load("b")
	require.False(t, askedB, "discovery asked a server the kubeconfig named after the CLI read it")
}

// ORKA_NAMESPACE chooses the namespace as --namespace does, so a manifest
// naming another namespace conflicts with it in the same way.
func TestManifestNamespaceConflictsWithExplicitNamespace(t *testing.T) {
	manifest := filepath.Join(t.TempDir(), "agent.yaml")
	require.NoError(t, os.WriteFile(manifest, []byte("kind: Agent\nmetadata:\n  name: a\n  namespace: team-b\n"), 0o600))
	for _, tc := range []struct {
		name, flag, env, wantErr string
	}{
		{name: "flag", flag: "team-a", wantErr: `manifest namespace "team-b" does not match --namespace "team-a"`},
		{name: "env", env: "team-a", wantErr: `manifest namespace "team-b" does not match ORKA_NAMESPACE "team-a"`},
		{name: "matching env", env: "team-b"},
		{name: "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envNamespace, tc.env)
			root := newRootCmd()
			root.SetOut(new(bytes.Buffer))
			root.SetErr(new(bytes.Buffer))
			args := []string{}
			if tc.flag != "" {
				args = append(args, "--namespace", tc.flag)
			}
			root.SetArgs(args)
			var bodyErr, queryErr error
			root.RunE = func(cmd *cobra.Command, _ []string) error {
				_, bodyErr = manifestWithNamespaceJSON(cmd, manifest, "")
				_, queryErr = namespaceQueryForManifest(cmd, "", map[string]any{
					"metadata": map[string]any{"namespace": "team-b"},
				})
				return nil
			}
			require.NoError(t, root.Execute())
			if tc.wantErr == "" {
				require.NoError(t, bodyErr)
				require.NoError(t, queryErr)
				return
			}
			require.EqualError(t, bodyErr, tc.wantErr)
			require.EqualError(t, queryErr, tc.wantErr)
		})
	}
}

// orka dashboard signs the browser in with a --token-file token instead of
// minting a ServiceAccount token.
func TestDashboardUsesTokenFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubDashboardProbe(t)
	origCreate := serviceAccountTokenFunc
	serviceAccountTokenFunc = func(string, string) (string, error) {
		t.Error("orka dashboard minted a token although --token-file gave one")
		return "", os.ErrInvalid
	}
	t.Cleanup(func() { serviceAccountTokenFunc = origCreate })
	var opened string
	origOpen := openBrowserFunc
	openBrowserFunc = func(u string) error {
		opened = u
		return nil
	}
	t.Cleanup(func() { openBrowserFunc = origOpen })
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("file-login-token\n"), 0o600))

	require.NoError(t, executeCLI(t, "dashboard", "--server", "http://dashboard.invalid", "--token-file", tokenFile))
	require.Equal(t, "http://dashboard.invalid/login#token=file-login-token", opened)
}

// The dashboard signs the browser in only with a token given for that command:
// an exported ORKA_TOKEN or the config file's token is the CLI's own
// credential, so it mints a ServiceAccount token instead.
func TestDashboardIgnoresEnvAndConfigToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(envToken, "env-token")
	require.NoError(t, saveConfig(orkaConfig{Token: "config-token"}))
	stubDashboardProbe(t)
	origCreate := serviceAccountTokenFunc
	serviceAccountTokenFunc = func(string, string) (string, error) { return "minted-token", nil }
	t.Cleanup(func() { serviceAccountTokenFunc = origCreate })
	var opened string
	origOpen := openBrowserFunc
	openBrowserFunc = func(u string) error {
		opened = u
		return nil
	}
	t.Cleanup(func() { openBrowserFunc = origOpen })

	require.NoError(t, executeCLI(t, "dashboard", "--server", "http://dashboard.invalid"))
	require.Equal(t, "http://dashboard.invalid/login#token=minted-token", opened)
}
