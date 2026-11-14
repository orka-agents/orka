/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/transport"

	"github.com/orka-agents/orka/internal/cli/client"
)

const (
	cliProgramName = "orka"
)

const (
	configDir  = ".orka"
	configFile = "config.yaml"

	// defaultNamespace is the Kubernetes default namespace. Service discovery
	// searches it last, and orka dashboard creates its ServiceAccount token there
	// when nothing names a namespace. API requests never fall back to it.
	defaultNamespace = "default"

	// Environment variables that stand in for the global flags of the same name.
	envToken     = "ORKA_TOKEN"
	envServer    = "ORKA_SERVER"
	envNamespace = "ORKA_NAMESPACE"

	// tokenFlagUsage is the help text of --token, including where a command
	// declares its own.
	tokenFlagUsage = "Bearer token for authentication (prefer --token-file or " + envToken +
		", which keep it out of process arguments)"

	// orkaServiceLabel is used to discover the Orka service in a cluster.
	orkaServiceLabel = "app.kubernetes.io/name=orka"
)

// orkaConfig holds the persisted CLI configuration.
type orkaConfig struct {
	Server    string `yaml:"server,omitempty"`
	Token     string `yaml:"token,omitempty"`
	Namespace string `yaml:"namespace,omitempty"`
}

// portForwardCache holds cached port-forward connection info to avoid re-creating on every command.
type portForwardCache struct {
	Port      int    `json:"port"`
	PID       int    `json:"pid"`
	Service   string `json:"service"`
	Namespace string `json:"namespace"`
	// KubeContext is the kubeconfigIdentity of the context the forward goes
	// through. The CLI reuses the forward only for that same context.
	KubeContext string `json:"kubeContext,omitempty"`
	// ProcessStart is when the forward's process was created, where the CLI
	// identifies the process that way rather than by its command line.
	ProcessStart string `json:"processStart,omitempty"`
	Timestamp    int64  `json:"timestamp"`
}

// portForwardCachePath returns the cache file for the kubeconfig context with
// the given kubeconfigIdentity, so each context keeps its own forward. It
// returns "" when there is no identity or home directory.
func portForwardCachePath(identity string) string {
	if len(identity) < 16 {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, configDir, "portforward-"+identity[:16]+".json")
}

// loadPortForwardCache returns the unexpired forward cached for the context
// with the given kubeconfigIdentity, or nil.
func loadPortForwardCache(identity string) *portForwardCache {
	path := portForwardCachePath(identity)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cache portForwardCache
	if json.Unmarshal(data, &cache) != nil || cache.KubeContext != identity {
		return nil
	}
	// Expire after 30 minutes
	if time.Now().Unix()-cache.Timestamp > 1800 {
		return nil
	}
	return &cache
}

func savePortForwardCache(cache *portForwardCache) {
	path := portForwardCachePath(cache.KubeContext)
	if path == "" {
		return
	}
	os.MkdirAll(filepath.Dir(path), 0o700) //nolint:errcheck
	cache.Timestamp = time.Now().Unix()
	data, err := json.Marshal(cache)
	if err != nil {
		return
	}
	os.WriteFile(path, data, 0o600) //nolint:errcheck
}

// clearPortForwardCache forgets the forward cached for the current context of
// the kubeconfig at kubeconfigPath.
func clearPortForwardCache(kubeconfigPath string) {
	if path := portForwardCachePath(kubeconfigIdentity(kubeconfigPath)); path != "" {
		os.Remove(path) //nolint:errcheck
	}
}

// newClientFromCmd creates a client.Client using flag values resolved against
// the environment, config and kubeconfig. It exits on an unreadable token file.
func newClientFromCmd(cmd *cobra.Command) *client.Client {
	c, err := resolveClient(cmd, "")
	cobra.CheckErr(err)
	return c
}

// newClientWithTunnel is newClientFromCmd for a command that handles
// interrupts itself. A port-forward this call starts normally ends on the
// first interrupt. With ownTunnel the caller decides instead: no interrupt
// handler is registered for the port-forward, the terminal's own interrupt
// does not reach it, and the returned function stops it. That function does
// nothing when no port-forward was started or ownTunnel is false.
func newClientWithTunnel(cmd *cobra.Command, server string, ownTunnel bool) (*client.Client, func()) {
	c, closeTunnel, err := resolveClientWithTunnel(cmd, server, ownTunnel)
	cobra.CheckErr(err)
	return c, closeTunnel
}

// resolveClient picks the server, credentials and namespace for a command.
// A non-empty server is a transport the caller already selected.
//
// The server is --server, ORKA_SERVER or the config file. Without one, the CLI
// reaches Orka through the current kubeconfig context: a cached port-forward
// started for that same context, or a new one to the discovered Service.
// Otherwise it falls back to defaultServer.
//
// The token is --token, --token-file, ORKA_TOKEN or the config file. Only when
// none is set and the server was reached through the kubeconfig does the CLI
// send the kubeconfig user's bearer token, so that credential never goes to a
// server named some other way.
//
// The namespace is --namespace, ORKA_NAMESPACE or the config file. Otherwise it
// stays empty and the server uses the namespace it watches. The kubeconfig
// context's namespace only tells discovery where to look first: it belongs to
// the cluster, not to the Orka installation.
func resolveClient(cmd *cobra.Command, server string) (*client.Client, error) {
	c, _, err := resolveClientWithTunnel(cmd, server, false)
	return c, err
}

// resolveClientWithTunnel is resolveClient that also returns the function
// stopping a port-forward it started, with ownTunnel as in newClientWithTunnel.
func resolveClientWithTunnel(cmd *cobra.Command, server string, ownTunnel bool) (*client.Client, func(), error) {
	closeTunnel := func() {}
	cfg := loadConfig()
	if server == "" {
		server = configuredServer(cmd, cfg)
	}
	token, err := configuredToken(cmd, cfg)
	if err != nil {
		return nil, closeTunnel, err
	}
	txnToken, err := configuredTxnToken(cmd)
	if err != nil {
		return nil, closeTunnel, err
	}
	kubeconfigPath, _ := cmd.Flags().GetString("kubeconfig")
	ns := configuredNamespace(cmd, cfg)

	viaKubeconfig := false
	if server == "" {
		server, closeTunnel = connectThroughKubeconfig(kubeconfigPath, ownTunnel)
		viaKubeconfig = server != ""
	}
	if server == "" {
		server = defaultServer
	}
	if token == "" && viaKubeconfig {
		if kubeToken, err := extractToken(kubeconfigPath); err == nil {
			token = kubeToken
		}
	}

	c := client.NewWithNamespace(server, token, ns)
	c.TxnToken = txnToken
	return c, closeTunnel, nil
}

// configuredServer returns the server named by --server, ORKA_SERVER or the
// config file, or "" when none is set.
func configuredServer(cmd *cobra.Command, cfg orkaConfig) string {
	if server, _ := cmd.Flags().GetString("server"); server != "" {
		return server
	}
	if server := strings.TrimSpace(os.Getenv(envServer)); server != "" {
		return server
	}
	return cfg.Server
}

// configuredNamespace returns the namespace named by --namespace,
// ORKA_NAMESPACE or the config file, or "" when none is set.
func configuredNamespace(cmd *cobra.Command, cfg orkaConfig) string {
	if ns, _ := explicitNamespace(cmd); ns != "" {
		return ns
	}
	return cfg.Namespace
}

// explicitNamespace returns the namespace chosen for this command with
// --namespace or ORKA_NAMESPACE, and which of the two chose it, or "" when
// neither is set. A manifest naming another namespace conflicts with it.
func explicitNamespace(cmd *cobra.Command) (string, string) {
	if ns, _ := cmd.Flags().GetString(cliNamespaceQuery); strings.TrimSpace(ns) != "" {
		return strings.TrimSpace(ns), "--namespace"
	}
	if ns := strings.TrimSpace(os.Getenv(envNamespace)); ns != "" {
		return ns, envNamespace
	}
	return "", ""
}

// configuredToken returns the bearer token from --token, --token-file,
// ORKA_TOKEN or the config file, in that order.
func configuredToken(cmd *cobra.Command, cfg orkaConfig) (string, error) {
	token, err := flagToken(cmd)
	if err != nil || token != "" {
		return token, err
	}
	if token := strings.TrimSpace(os.Getenv(envToken)); token != "" {
		return token, nil
	}
	return cfg.Token, nil
}

// flagToken returns the bearer token given by --token or --token-file, or ""
// when neither is set.
func flagToken(cmd *cobra.Command) (string, error) {
	token, _ := cmd.Flags().GetString("token")
	tokenFile, _ := cmd.Flags().GetString("token-file")
	if token != "" && tokenFile != "" {
		return "", errors.New("use either --token or --token-file, not both")
	}
	if token != "" || tokenFile == "" {
		return token, nil
	}
	if txnTokenFile, _ := cmd.Flags().GetString("txn-token-file"); readsStdin(tokenFile) && readsStdin(txnTokenFile) {
		return "", errors.New("--token-file and --txn-token-file cannot both read stdin")
	}
	data, err := readCredentialFile(cmd, "--token-file", tokenFile)
	if err != nil {
		return "", err
	}
	token = strings.TrimSpace(string(data))
	if token == "" {
		return "", errors.New("--token-file is empty")
	}
	return token, nil
}

// configuredTxnToken returns the transaction token from --txn-token or
// --txn-token-file.
func configuredTxnToken(cmd *cobra.Command) (string, error) {
	txnToken, _ := cmd.Flags().GetString("txn-token")
	txnTokenFile, _ := cmd.Flags().GetString("txn-token-file")
	if txnToken == "" && txnTokenFile != "" {
		data, err := readCredentialFile(cmd, "--txn-token-file", txnTokenFile)
		if err != nil {
			return "", err
		}
		txnToken = string(data)
	}
	return strings.TrimSpace(txnToken), nil
}

// credentialReads remembers each credential file read, so resolving the client
// a second time (orka run retries once) does not find stdin, a pipe or a
// process substitution already drained. An entry is kept only while os.Stdin
// is the file it was read with.
var credentialReads = struct {
	sync.Mutex
	byPath map[string]credentialRead
}{byPath: map[string]credentialRead{}}

type credentialRead struct {
	stdin *os.File
	data  []byte
}

// readsStdin reports whether path names stdin.
func readsStdin(path string) bool {
	switch path {
	case "-", "/dev/stdin", "/dev/fd/0", "/proc/self/fd/0":
		return true
	}
	return false
}

// readCredentialFile reads the credential file named by flag, where "-" is
// stdin. Stdin can feed only one reader, so a command whose --file also reads
// it is rejected.
func readCredentialFile(cmd *cobra.Command, flag, path string) ([]byte, error) {
	if readsStdin(path) {
		if file := cmd.Flags().Lookup("file"); file != nil && readsStdin(file.Value.String()) {
			return nil, fmt.Errorf("%s and --file cannot both read stdin", flag)
		}
	}
	credentialReads.Lock()
	defer credentialReads.Unlock()
	if read, ok := credentialReads.byPath[path]; ok && read.stdin == os.Stdin {
		return read.data, nil
	}
	data, err := readFileOrStdin(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", flag, err)
	}
	credentialReads.byPath[path] = credentialRead{stdin: os.Stdin, data: data}
	return data, nil
}

// rejectStdinCredential returns an error when --token-file or --txn-token-file
// reads stdin, which the command itself uses as use describes.
func rejectStdinCredential(cmd *cobra.Command, use string) error {
	for _, flag := range []string{"token-file", "txn-token-file"} {
		if path, _ := cmd.Flags().GetString(flag); readsStdin(path) {
			return fmt.Errorf("--%s %s reads stdin, which %s", flag, path, use)
		}
	}
	return nil
}

// connectThroughKubeconfig reaches the Orka API through the current kubeconfig
// context, reusing a cached port-forward started for that context or starting
// a new one to the Service discovered from it. It returns the local server URL,
// or "" when no Service could be reached, and the function that stops a new
// port-forward when ownTunnel is set (see newClientWithTunnel).
func connectThroughKubeconfig(kubeconfigPath string, ownTunnel bool) (string, func()) {
	closeTunnel := func() {}
	identity := kubeconfigIdentity(kubeconfigPath)
	if cached := loadPortForwardCache(identity); cached != nil && portForwardProcessFn(cached) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", cached.Port), 500*time.Millisecond)
		if err == nil {
			conn.Close() //nolint:errcheck
			fmt.Fprintf(os.Stderr, "Connected to %s in %s (cached port %d)\n",
				cached.Service, cached.Namespace, cached.Port)
			return portForwardURL(cached.Port), closeTunnel
		}
	}

	// Show connecting indicator if stderr is a terminal
	stderrIsTTY := false
	if fi, err := os.Stderr.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		stderrIsTTY = true
	}
	if stderrIsTTY {
		fmt.Fprint(os.Stderr, "⠋ Connecting to cluster…")
	}
	clearIndicator := func() {
		if stderrIsTTY {
			fmt.Fprint(os.Stderr, "\r\033[2K")
		}
	}
	svcNS, svcName := discoverServiceFn(kubeconfigPath, kubeconfigNamespace(kubeconfigPath))
	if svcName == "" {
		clearIndicator()
		return "", closeTunnel
	}
	localPort, pid, cleanup, err := startPortForwardFn(kubeconfigPath, svcNS, svcName, ownTunnel)
	if err != nil {
		clearIndicator()
		return "", closeTunnel
	}
	if ownTunnel {
		closeTunnel = cleanup
	} else {
		// Stop the port-forward on an interrupt. Register before returning, so
		// an interrupt that arrives as soon as the command starts still reaches
		// the handler.
		interrupts := make(chan os.Signal, 1)
		signal.Notify(interrupts, os.Interrupt)
		go func() {
			<-interrupts
			cleanup()
		}()
	}
	savePortForwardCache(&portForwardCache{
		Port:         localPort,
		PID:          pid,
		Service:      svcName,
		Namespace:    svcNS,
		KubeContext:  identity,
		ProcessStart: processStartMark(pid),
	})
	clearIndicator()
	fmt.Fprintf(os.Stderr, "Connected to %s in %s (port %d)\n",
		svcName, svcNS, localPort)
	return portForwardURL(localPort), closeTunnel
}

// portForwardURL is the API URL of a port-forward on localPort. It names
// 127.0.0.1, the address the CLI checks the forward on, rather than localhost,
// which can resolve to an IPv6 listener the forward does not hold.
func portForwardURL(localPort int) string {
	return fmt.Sprintf("http://127.0.0.1:%d", localPort)
}

// discoverServiceFn, startPortForwardFn and portForwardProcessFn reach the
// cluster or inspect local processes; tests replace them to avoid needing a
// cluster and kubectl.
var (
	discoverServiceFn    = discoverService
	startPortForwardFn   = startPortForward
	portForwardProcessFn = isPortForwardProcess
)

// isPortForwardProcess reports whether the cached forward's process is still
// the kubectl port-forward the CLI started. A cached port is trusted only while
// that process holds it: once the forward exits, any local program could
// listen on the port. Where the process cannot be identified, nothing is
// reused.
func isPortForwardProcess(cached *portForwardCache) bool {
	return cached.PID > 0 && portForwardProcessRunning(cached)
}

// isOwnPortForward reports whether a process owned by uid and running args is
// the forward described by cached, started by this user.
func isOwnPortForward(uid int, args []string, cached *portForwardCache) bool {
	return uid == os.Getuid() && isPortForwardCommand(args, cached.Port, cached.Namespace, cached.Service)
}

// isPortForwardCommand reports whether args is the command line startPortForward
// runs for namespace/service on localPort.
func isPortForwardCommand(args []string, localPort int, namespace, service string) bool {
	want := portForwardArgs(namespace, service, localPort)
	for i := 0; i+len(want) <= len(args); i++ {
		if slices.Equal(args[i:i+len(want)], want) {
			return true
		}
	}
	return false
}

// portForwardArgs are the kubectl arguments that forward localPort to the
// Orka API port of namespace/service.
func portForwardArgs(namespace, service string, localPort int) []string {
	return []string{"port-forward", "-n", namespace, "svc/" + service, fmt.Sprintf("%d:8080", localPort)}
}

// discoverService finds the Orka service in the cluster.
// Returns the namespace and service name, or empty strings if not found.
func discoverService(kubeconfigPath, defaultNS string) (string, string) {
	restConfig, err := buildRESTConfig(kubeconfigPath)
	if err != nil {
		return "", ""
	}

	// Try the user's namespace first, then well-known orka namespaces
	var namespacesToTry []string
	if defaultNS != "" {
		namespacesToTry = append(namespacesToTry, defaultNS)
	}
	for _, ns := range []string{"orka-system", cliProgramName, defaultNamespace} {
		if ns != defaultNS {
			namespacesToTry = append(namespacesToTry, ns)
		}
	}

	for _, ns := range namespacesToTry {
		if name := discoverOrkaService(restConfig, ns); name != "" {
			return ns, name
		}
	}
	return "", ""
}

// startPortForward starts a kubectl port-forward to the Orka service.
// Returns the local port, the process PID, a cleanup function, and any error.
// With isolateInterrupt the port-forward does not receive the interrupt a
// terminal sends to the CLI's foreground process group on Ctrl-C; only the
// cleanup function stops it.
func startPortForward(kubeconfigPath, namespace, service string, isolateInterrupt bool) (int, int, func(), error) {
	// Find a free port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, 0, nil, fmt.Errorf("find free port: %w", err)
	}
	localPort := listener.Addr().(*net.TCPAddr).Port
	listener.Close() //nolint:errcheck

	args := portForwardArgs(namespace, service, localPort)
	if kubeconfigPath != "" {
		args = append([]string{"--kubeconfig", kubeconfigPath}, args...)
	}

	cmd := exec.Command("kubectl", args...)
	cmd.Stderr = nil
	cmd.Stdout = nil
	if isolateInterrupt {
		isolateFromTerminalInterrupt(cmd)
	}

	if err := cmd.Start(); err != nil {
		return 0, 0, nil, fmt.Errorf("start port-forward: %w", err)
	}

	cleanup := func() {
		if cmd.Process != nil {
			cmd.Process.Kill() //nolint:errcheck
		}
	}

	// Wait for port-forward to be ready
	ready := false
	for range 30 {
		time.Sleep(100 * time.Millisecond)
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", localPort), 200*time.Millisecond)
		if err == nil {
			conn.Close() //nolint:errcheck
			ready = true
			break
		}
	}

	if !ready {
		cleanup()
		return 0, 0, nil, fmt.Errorf("port-forward not ready after 3s")
	}

	return localPort, cmd.Process.Pid, cleanup, nil
}

// discoverOrkaService finds the Orka API service in the given namespace.
// Tries well-known service names first, then falls back to label selector.
func discoverOrkaService(restConfig *rest.Config, namespace string) string {
	transportConfig, err := restConfig.TransportConfig()
	if err != nil {
		return ""
	}
	rt, err := transport.New(transportConfig)
	if err != nil {
		return ""
	}

	httpClient := &http.Client{Transport: rt, Timeout: 5 * time.Second}

	// Strategy 1: check well-known service names
	for _, candidate := range []string{"orka-api", cliProgramName, "orka-controller-manager"} {
		if checkServiceExists(httpClient, restConfig.Host, namespace, candidate) {
			return candidate
		}
	}

	// Strategy 2: find by label (look for a service with an "api" port)
	if name := findServiceByLabel(httpClient, restConfig.Host, namespace); name != "" {
		return name
	}

	return ""
}

func findServiceByLabel(httpClient *http.Client, host, namespace string) string {
	listURL := fmt.Sprintf("%s/api/v1/namespaces/%s/services?labelSelector=%s",
		host, namespace, orkaServiceLabel)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return ""
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return ""
	}

	var result struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Ports []struct {
					Name string `json:"name"`
				} `json:"ports"`
			} `json:"spec"`
		} `json:"items"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ""
	}

	// Prefer a service with an "api" port
	for _, svc := range result.Items {
		for _, port := range svc.Spec.Ports {
			if port.Name == "api" {
				return svc.Metadata.Name
			}
		}
	}

	if len(result.Items) > 0 {
		return result.Items[0].Metadata.Name
	}

	return ""
}

func checkServiceExists(httpClient *http.Client, host, namespace, name string) bool {
	svcURL := fmt.Sprintf("%s/api/v1/namespaces/%s/services/%s", host, namespace, name)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, svcURL, nil)
	if err != nil {
		return false
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close() //nolint:errcheck

	return resp.StatusCode == http.StatusOK
}

// buildRESTConfig builds a Kubernetes REST config from kubeconfig.
func buildRESTConfig(kubeconfigPath string) (*rest.Config, error) {
	return kubeconfigLoader(kubeconfigPath).ClientConfig()
}

// kubeconfigLoader loads the kubeconfig at kubeconfigPath, or the default
// kubeconfig ($KUBECONFIG, then ~/.kube/config) when the path is empty.
func kubeconfigLoader(kubeconfigPath string) clientcmd.ClientConfig {
	loadingRules := &clientcmd.ClientConfigLoadingRules{}
	if kubeconfigPath != "" {
		loadingRules.ExplicitPath = kubeconfigPath
	} else {
		loadingRules = clientcmd.NewDefaultClientConfigLoadingRules()
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, nil)
}

// kubeconfigIdentity identifies the current kubeconfig context by a digest of
// its name, its cluster and user names, the file the cluster comes from and
// how the cluster is reached: server, proxy and TLS settings. Two contexts that
// differ in any of these get separate port-forwards. It returns "" when the
// kubeconfig has no usable current context.
func kubeconfigIdentity(kubeconfigPath string) string {
	raw, err := kubeconfigLoader(kubeconfigPath).RawConfig()
	if err != nil || raw.CurrentContext == "" {
		return ""
	}
	kubeCtx, ok := raw.Contexts[raw.CurrentContext]
	if !ok || kubeCtx == nil {
		return ""
	}
	cluster, ok := raw.Clusters[kubeCtx.Cluster]
	if !ok || cluster == nil || cluster.Server == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		raw.CurrentContext, kubeCtx.Cluster, kubeCtx.AuthInfo,
		resolvedPath(cluster.LocationOfOrigin), cluster.Server, cluster.ProxyURL, cluster.TLSServerName,
		strconv.FormatBool(cluster.InsecureSkipTLSVerify),
		cluster.CertificateAuthority, string(cluster.CertificateAuthorityData),
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}

// resolvedPath returns path as an absolute path with symlinks resolved, so
// one file has one name however it was given; it returns path unchanged when
// it cannot be resolved.
func resolvedPath(path string) string {
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	return path
}

// configPath returns the full path to the config file.
func configPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, configDir, configFile)
}

// loadConfig reads the config file. Returns empty config on error.
func loadConfig() orkaConfig {
	var cfg orkaConfig
	path := configPath()
	if path == "" {
		return cfg
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	_ = yaml.Unmarshal(data, &cfg)
	return cfg
}

// saveConfig writes the config file with 0600 permissions.
func saveConfig(cfg orkaConfig) error {
	path := configPath()
	if path == "" {
		return fmt.Errorf("cannot determine home directory")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	return os.WriteFile(path, data, 0o600)
}

// maskToken shows only the first 4 chars + *** for security.
func maskToken(token string) string {
	if len(token) <= 4 {
		return "***"
	}
	return token[:4] + "...***"
}
