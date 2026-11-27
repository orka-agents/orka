/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/transport"

	"github.com/orka-agents/orka/internal/cli/client"
)

const (
	cliProgramName = "orka"
)

const (
	configDir  = ".orka"
	configFile = "config.yaml"

	defaultNamespace = "default"

	envToken     = "ORKA_TOKEN"
	envServer    = "ORKA_SERVER"
	envNamespace = "ORKA_NAMESPACE"

	tokenFlagUsage = "Bearer token for authentication (prefer --token-file or " + envToken +
		", which keep it out of process arguments)"

	orkaServiceLabel = "app.kubernetes.io/name=orka"
)

type orkaConfig struct {
	Server            string     `yaml:"server,omitempty"`
	Token             string     `yaml:"token,omitempty"`
	Namespace         string     `yaml:"namespace,omitempty"`
	TokenKind         string     `yaml:"tokenKind,omitempty"`
	TokenExpiresAt    *time.Time `yaml:"tokenExpiresAt,omitempty"`
	SessionServer     string     `yaml:"sessionServer,omitempty"`
	SessionLogin      string     `yaml:"sessionLogin,omitempty"`
	SessionSubject    string     `yaml:"sessionSubject,omitempty"`
	OIDCIssuer        string     `yaml:"oidcIssuer,omitempty"`
	OIDCAudience      string     `yaml:"oidcAudience,omitempty"`
	OIDCClientID      string     `yaml:"oidcClientID,omitempty"`
	OIDCTokenEndpoint string     `yaml:"oidcTokenEndpoint,omitempty"`
	OIDCRefreshToken  string     `yaml:"oidcRefreshToken,omitempty"`
	OIDCScope         string     `yaml:"oidcScope,omitempty"`
}

type portForwardCache struct {
	Port         int      `json:"port"`
	PID          int      `json:"pid"`
	Service      string   `json:"service"`
	Namespace    string   `json:"namespace"`
	KubeContext  string   `json:"kubeContext,omitempty"`
	Command      []string `json:"command,omitempty"`
	ProcessStart string   `json:"processStart,omitempty"`
	Timestamp    int64    `json:"timestamp"`
}

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

func clearPortForwardCache(cmd *cobra.Command) {
	if path := portForwardCachePath(commandKubeTarget(cmd, loadConfig()).cacheKey()); path != "" {
		os.Remove(path) //nolint:errcheck
	}
}

func newClientFromCmd(cmd *cobra.Command) *client.Client {
	c, err := resolveClient(cmd, "")
	cobra.CheckErr(err)
	return c
}

func sessionToken(token string, saved bool, server string) bool {
	if token == "" {
		return false
	}
	if saved {
		return true
	}
	if !strings.HasPrefix(token, sessionTokenPrefix) {
		return false
	}
	cobra.CheckErr(sessionServerAllowed(server))
	return true
}

func newClientWithTunnel(cmd *cobra.Command, server string, ownTunnel bool) (*client.Client, func()) {
	c, closeTunnel, err := resolveClientWithTunnel(cmd, server, ownTunnel)
	cobra.CheckErr(err)
	return c, closeTunnel
}

func resolveClient(cmd *cobra.Command, server string) (*client.Client, error) {
	c, _, err := resolveClientWithTunnel(cmd, server, false)
	return c, err
}

func resolveClientWithTunnel(cmd *cobra.Command, server string, ownTunnel bool) (*client.Client, func(), error) {
	closeTunnel := func() {}
	cfg := loadConfig()
	if server == "" {
		server = configuredServer(cmd, cfg)
	}
	token, saved, err := configuredTokenSource(cmd, cfg)
	if err != nil {
		return nil, closeTunnel, err
	}
	session := false
	if saved && cfg.TokenKind == tokenKindGitHubSession {
		token, session = configuredSession(cmd, server, cfg), true
	} else if saved && cfg.TokenKind == tokenKindOIDC {
		token, session = configuredOIDCToken(cmd, server, cfg), true
	}
	txnToken, err := configuredTxnToken(cmd)
	if err != nil {
		return nil, closeTunnel, err
	}
	ns := configuredNamespace(cmd, cfg)

	if server == "" {
		target := commandKubeTarget(cmd, cfg)
		var kubeToken string
		if token == "" && !session {
			kubeToken, _ = target.token()
		}
		server, closeTunnel = connectThroughKubeconfig(target, ownTunnel)
		if server != "" && token == "" && !session {
			token = kubeToken
		}
	}
	if server == "" {
		server = defaultServer
	}

	c := client.NewWithNamespace(server, token, ns)
	c.TxnToken = txnToken
	if sessionToken(token, session, server) {
		c.HTTPClient = sessionAPIClient
	}
	return c, closeTunnel, nil
}

func commandKubeTarget(cmd *cobra.Command, cfg orkaConfig) kubeTarget {
	kubeconfigPath, _ := cmd.Flags().GetString("kubeconfig")
	return kubeTarget{
		kubeconfigContext: currentKubeconfigContext(kubeconfigPath),
		selectedNamespace: configuredNamespace(cmd, cfg),
	}
}

func configuredServer(cmd *cobra.Command, cfg orkaConfig) string {
	if server, _ := cmd.Flags().GetString("server"); server != "" {
		return server
	}
	if server := strings.TrimSpace(os.Getenv(envServer)); server != "" {
		return server
	}
	return cfg.Server
}

func configuredNamespace(cmd *cobra.Command, cfg orkaConfig) string {
	if ns, _ := explicitNamespace(cmd); ns != "" {
		return ns
	}
	return cfg.Namespace
}

func explicitNamespace(cmd *cobra.Command) (string, string) {
	if ns, _ := cmd.Flags().GetString(cliNamespaceQuery); strings.TrimSpace(ns) != "" {
		return strings.TrimSpace(ns), "--namespace"
	}
	if ns := strings.TrimSpace(os.Getenv(envNamespace)); ns != "" {
		return ns, envNamespace
	}
	return "", ""
}

func configuredTokenSource(cmd *cobra.Command, cfg orkaConfig) (string, bool, error) {
	token, err := flagToken(cmd)
	if err != nil || token != "" {
		return token, false, err
	}
	if token := strings.TrimSpace(os.Getenv(envToken)); token != "" {
		return token, false, nil
	}
	return cfg.Token, cfg.Token != "", nil
}

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

var credentialReads = struct {
	sync.Mutex
	byPath map[string]credentialRead
}{byPath: map[string]credentialRead{}}

type credentialRead struct {
	stdin *os.File
	data  []byte
}

func readsStdin(path string) bool {
	switch path {
	case "-", "/dev/stdin", "/dev/fd/0", "/proc/self/fd/0":
		return true
	}
	return false
}

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

func rejectStdinCredential(cmd *cobra.Command, use string) error {
	for _, flag := range []string{"token-file", "txn-token-file"} {
		if path, _ := cmd.Flags().GetString(flag); readsStdin(path) {
			return fmt.Errorf("--%s %s reads stdin, which %s", flag, path, use)
		}
	}
	return nil
}

func connectThroughKubeconfig(target kubeTarget, ownTunnel bool) (string, func()) {
	closeTunnel := func() {}
	key := target.cacheKey()
	if cached := loadPortForwardCache(key); cached != nil && recordsCurrentForward(cached) && portForwardProcessFn(cached) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", cached.Port), 500*time.Millisecond)
		if err == nil {
			conn.Close() //nolint:errcheck
			fmt.Fprintf(os.Stderr, "Connected to %s in %s (cached port %d)\n",
				cached.Service, cached.Namespace, cached.Port)
			return portForwardURL(cached.Port), closeTunnel
		}
	}

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
	svcNS, svcName := discoverServiceFn(target)
	if svcName == "" {
		clearIndicator()
		return "", closeTunnel
	}
	localPort, pid, cleanup, err := startPortForwardFn(target.kubeconfigContext, svcNS, svcName, ownTunnel)
	if err != nil {
		clearIndicator()
		fmt.Fprintf(os.Stderr, "Could not port-forward to %s in %s: %v\n", svcName, svcNS, err)
		return "", closeTunnel
	}
	if now := readKubeconfigContext(target.path, target.name); now.cluster != target.cluster {
		cleanup()
		clearIndicator()
		fmt.Fprintln(os.Stderr, "The kubeconfig changed while connecting; not using the port-forward")
		return "", closeTunnel
	}
	if ownTunnel {
		closeTunnel = cleanup
	} else {
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
		KubeContext:  key,
		Command:      portForwardCommand(target.kubeconfigContext, svcNS, svcName, localPort),
		ProcessStart: processStartMark(pid),
	})
	clearIndicator()
	fmt.Fprintf(os.Stderr, "Connected to %s in %s (port %d)\n",
		svcName, svcNS, localPort)
	return portForwardURL(localPort), closeTunnel
}

func portForwardURL(localPort int) string {
	return fmt.Sprintf("http://127.0.0.1:%d", localPort)
}

var (
	discoverServiceFn    = discoverService
	startPortForwardFn   = startPortForward
	portForwardProcessFn = isPortForwardProcess
	freeLocalPortFn      = freeLocalPort
)

func isPortForwardProcess(cached *portForwardCache) bool {
	return cached.PID > 0 && portForwardProcessRunning(cached)
}

func recordsCurrentForward(cached *portForwardCache) bool {
	want := portForwardCommand(kubeconfigContext{}, cached.Namespace, cached.Service, cached.Port)
	at := len(cached.Command) - len(want) + 1
	return at >= 1 && cached.Command[0] == want[0] && slices.Equal(cached.Command[at:], want[1:])
}

func isOwnPortForward(uid int, args []string, cached *portForwardCache) bool {
	return uid == os.Getuid() && isPortForwardCommand(args, cached.Command)
}

func isPortForwardCommand(args, want []string) bool {
	if len(args) == 0 || len(want) == 0 || filepath.Base(args[0]) != filepath.Base(want[0]) {
		return false
	}
	return slices.Equal(strings.Fields(strings.Join(args[1:], " ")), strings.Fields(strings.Join(want[1:], " ")))
}

func portForwardCommand(kc kubeconfigContext, namespace, service string, localPort int) []string {
	command := []string{"kubectl"}
	if kc.path != "" {
		command = append(command, "--kubeconfig", kc.path)
	}
	if kc.name != "" {
		command = append(command, "--context", kc.name)
	}
	return append(command, "port-forward", "--address", "127.0.0.1", "-n", namespace, "svc/"+service,
		fmt.Sprintf("%d:8080", localPort))
}

func discoverService(target kubeTarget) (string, string) {
	restConfig, err := target.clientConfig().ClientConfig()
	if err != nil {
		return "", ""
	}
	for _, ns := range target.discoveryNamespaces() {
		if name := discoverOrkaService(restConfig, ns); name != "" {
			return ns, name
		}
	}
	return "", ""
}

var portForwardReadyTimeout = 3 * time.Second

func startPortForward(kc kubeconfigContext, namespace, service string, isolateInterrupt bool) (int, int, func(), error) {
	localPort, err := freeLocalPortFn()
	if err != nil {
		return 0, 0, nil, fmt.Errorf("find free port: %w", err)
	}

	output, progress, removeOutput, err := portForwardOutput()
	if err != nil {
		return 0, 0, nil, err
	}

	command := portForwardCommand(kc, namespace, service, localPort)
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Stderr = output
	cmd.Stdout = output
	if isolateInterrupt {
		isolateFromTerminalInterrupt(cmd)
	}

	err = cmd.Start()
	output.Close() //nolint:errcheck
	if err != nil {
		progress.Close() //nolint:errcheck
		removeOutput()
		return 0, 0, nil, fmt.Errorf("start port-forward: %w", err)
	}
	exited := make(chan struct{})
	go func() {
		cmd.Wait() //nolint:errcheck
		close(exited)
	}()

	cleanup := func() {
		cmd.Process.Kill() //nolint:errcheck
		select {
		case <-exited:
		case <-time.After(2 * time.Second):
		}
		removeOutput()
	}

	err = waitForForwarding(progress, localPort, exited, portForwardReadyTimeout)
	progress.Close() //nolint:errcheck
	if err != nil {
		cleanup()
		return 0, 0, nil, err
	}

	return localPort, cmd.Process.Pid, cleanup, nil
}

func freeLocalPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer listener.Close() //nolint:errcheck
	return listener.Addr().(*net.TCPAddr).Port, nil
}

func portForwardOutput() (output, progress *os.File, remove func(), err error) {
	output, err = os.CreateTemp("", "orka-port-forward-*.out")
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create port-forward output: %w", err)
	}
	name := output.Name()
	progress, err = os.Open(name)
	if err == nil {
		err = sameFile(output, progress)
		if err != nil {
			progress.Close() //nolint:errcheck
		}
	}
	if err != nil {
		output.Close()  //nolint:errcheck
		os.Remove(name) //nolint:errcheck
		return nil, nil, nil, fmt.Errorf("open port-forward output: %w", err)
	}
	removed := os.Remove(name) == nil
	return output, progress, func() {
		if !removed {
			removed = os.Remove(name) == nil
		}
	}, nil
}

func sameFile(a, b *os.File) error {
	aInfo, err := a.Stat()
	if err != nil {
		return err
	}
	bInfo, err := b.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(aInfo, bInfo) {
		return fmt.Errorf("%s was replaced", a.Name())
	}
	return nil
}

func waitForForwarding(progress io.Reader, localPort int, exited <-chan struct{}, timeout time.Duration) error {
	var seen []byte
	failed := func(reason string) error {
		if last := lastLine(seen); last != "" {
			return fmt.Errorf("%s: %s", reason, last)
		}
		return errors.New(reason)
	}
	deadline := time.After(timeout)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-exited:
		case <-deadline:
			more, _ := io.ReadAll(progress)
			seen = append(seen, more...)
			return failed(fmt.Sprintf("kubectl port-forward did not report forwarding within %s", timeout))
		case <-tick.C:
		}
		more, _ := io.ReadAll(progress)
		seen = append(seen, more...)
		select {
		case <-exited:
			more, _ := io.ReadAll(progress)
			seen = append(seen, more...)
			return failed("kubectl port-forward exited before it was forwarding")
		default:
		}
		if forwardingReported(seen, localPort) {
			return nil
		}
	}
}

func lastLine(output []byte) string {
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if len(last) > 300 {
		last = last[:300] + "…"
	}
	return last
}

func forwardingReported(output []byte, localPort int) bool {
	want := fmt.Sprintf("Forwarding from 127.0.0.1:%d -> ", localPort)
	for line := range strings.Lines(string(output)) {
		if strings.HasPrefix(line, want) {
			return true
		}
	}
	return false
}

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

	for _, candidate := range []string{"orka-api", cliProgramName, "orka-controller-manager"} {
		if checkServiceExists(httpClient, restConfig.Host, namespace, candidate) {
			return candidate
		}
	}

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

func kubeconfigLoadingRules(kubeconfigPath string) *clientcmd.ClientConfigLoadingRules {
	if kubeconfigPath != "" {
		return &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfigPath}
	}
	return clientcmd.NewDefaultClientConfigLoadingRules()
}

func kubeconfigLoader(kubeconfigPath, kubeContext string) clientcmd.ClientConfig {
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		kubeconfigLoadingRules(kubeconfigPath), &clientcmd.ConfigOverrides{CurrentContext: kubeContext})
}

type kubeconfigContext struct {
	path      string
	name      string
	namespace string
	identity  string
	cluster   string
	config    *clientcmdapi.Config
}

func currentKubeconfigContext(kubeconfigPath string) kubeconfigContext {
	return readKubeconfigContext(kubeconfigPath, "")
}

func readKubeconfigContext(kubeconfigPath, name string) kubeconfigContext {
	kc := kubeconfigContext{path: kubeconfigPath}
	raw, err := kubeconfigLoader(kubeconfigPath, "").RawConfig()
	if name == "" {
		name = raw.CurrentContext
	}
	if err != nil || name == "" {
		return kc
	}
	kc.name = name
	kc.config = &raw
	kubeCtx, ok := raw.Contexts[kc.name]
	if !ok || kubeCtx == nil {
		return kc
	}
	kc.namespace = kubeCtx.Namespace
	cluster, ok := raw.Clusters[kubeCtx.Cluster]
	if !ok || cluster == nil || cluster.Server == "" {
		return kc
	}
	ca, err := clusterCA(cluster)
	if err != nil {
		return kc
	}
	clusterFields := []string{
		resolvedPath(cluster.LocationOfOrigin), cluster.Server, cluster.ProxyURL, cluster.TLSServerName,
		strconv.FormatBool(cluster.InsecureSkipTLSVerify),
		cluster.CertificateAuthority, string(ca),
	}
	clusterSum := sha256.Sum256([]byte(strings.Join(clusterFields, "\x00")))
	kc.cluster = hex.EncodeToString(clusterSum[:])
	sum := sha256.Sum256([]byte(strings.Join(append([]string{
		kc.name, kubeCtx.Namespace, kubeCtx.Cluster, kubeCtx.AuthInfo,
	}, clusterFields...), "\x00")))
	kc.identity = hex.EncodeToString(sum[:])
	return kc
}

func clusterCA(cluster *clientcmdapi.Cluster) ([]byte, error) {
	if len(cluster.CertificateAuthorityData) > 0 || cluster.CertificateAuthority == "" {
		return cluster.CertificateAuthorityData, nil
	}
	return os.ReadFile(cluster.CertificateAuthority)
}

func (kc kubeconfigContext) clientConfig() clientcmd.ClientConfig {
	if kc.config == nil {
		return kubeconfigLoader(kc.path, kc.name)
	}
	return clientcmd.NewNonInteractiveClientConfig(*kc.config, kc.name, &clientcmd.ConfigOverrides{}, kubeconfigLoadingRules(kc.path))
}

func (kc kubeconfigContext) token() (string, error) {
	if kc.name == "" {
		return "", fmt.Errorf("no current context set in kubeconfig")
	}
	config := kc.clientConfig()
	rawConfig, err := config.RawConfig()
	if err != nil {
		return "", fmt.Errorf("failed to load kubeconfig: %w", err)
	}

	ctx, ok := rawConfig.Contexts[kc.name]
	if !ok || ctx == nil {
		return "", fmt.Errorf("context %q not found in kubeconfig", kc.name)
	}

	authInfo, ok := rawConfig.AuthInfos[ctx.AuthInfo]
	if !ok || authInfo == nil {
		return "", fmt.Errorf("user %q not found in kubeconfig", ctx.AuthInfo)
	}

	if authInfo.Token != "" {
		return authInfo.Token, nil
	}

	if authInfo.TokenFile != "" {
		data, err := os.ReadFile(authInfo.TokenFile)
		if err != nil {
			return "", fmt.Errorf("failed to read token file %q: %w", authInfo.TokenFile, err)
		}
		return strings.TrimSpace(string(data)), nil
	}

	if authInfo.Exec != nil {
		restConfig, err := config.ClientConfig()
		if err != nil {
			return "", fmt.Errorf("failed to get REST config for exec-based auth: %w", err)
		}
		if restConfig.BearerToken != "" {
			return restConfig.BearerToken, nil
		}
	}

	if authInfo.AuthProvider != nil {
		restConfig, err := config.ClientConfig()
		if err != nil {
			return "", fmt.Errorf("failed to get REST config for auth provider: %w", err)
		}
		if restConfig.BearerToken != "" {
			return restConfig.BearerToken, nil
		}
	}

	return "", fmt.Errorf("no token, token file, exec, or auth provider found for user %q", ctx.AuthInfo)
}

type kubeTarget struct {
	kubeconfigContext
	selectedNamespace string
}

func (t kubeTarget) discoveryNamespaces() []string {
	var namespaces []string
	for _, ns := range []string{t.selectedNamespace, t.namespace, "orka-system", cliProgramName, defaultNamespace} {
		if ns != "" && !slices.Contains(namespaces, ns) {
			namespaces = append(namespaces, ns)
		}
	}
	return namespaces
}

func (t kubeTarget) cacheKey() string {
	if t.identity == "" || t.selectedNamespace == "" {
		return t.identity
	}
	sum := sha256.Sum256([]byte(t.identity + "\x00" + t.selectedNamespace))
	return hex.EncodeToString(sum[:])
}

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

func configPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, configDir, configFile)
}

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
	file, err := os.CreateTemp(filepath.Dir(path), ".config-*.yaml")
	if err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return fmt.Errorf("save config: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	return os.Rename(file.Name(), path)
}

func maskToken(token string) string {
	if len(token) <= 4 {
		return "***"
	}
	return token[:4] + "...***"
}
